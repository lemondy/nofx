package trader

import (
	"context"
	"fmt"
	"time"

	"nofx/store"
)

// us_stock (design 2026-10-09): API-facing counterparts of the futures account
// readers. The shared GetAccountInfo / GetPositions / RiskStatus /
// GetOpenOrders assume futures position maps and a non-nil exchange trader, so
// each delegates here for a us_stock trader.

// stockReadState gathers account state for read-only callers (no run-state
// writes, no reconciliation).
func (at *AutoTrader) stockReadState(withProtection bool) (*stockState, error) {
	if at.store == nil {
		return nil, fmt.Errorf("no store")
	}
	if at.stockPaper() {
		return at.stockGatherPaper(nil)
	}
	if at.stockTrader == nil {
		return nil, fmt.Errorf("us_stock executor not linked")
	}
	return at.stockGatherLive(context.Background(), at.stockClock(), false, withProtection)
}

func (at *AutoTrader) stockAccountInfo() (map[string]interface{}, error) {
	st, err := at.stockReadState(false)
	if err != nil {
		return nil, fmt.Errorf("failed to get stock account: %w", err)
	}
	unrealized := 0.0
	for _, h := range st.Holdings {
		unrealized += (h.Mark - h.Avg) * h.Owned
	}
	totalPnL := st.Equity - at.initialBalance
	pnlPct, usedPct := 0.0, 0.0
	if at.initialBalance > 0 {
		pnlPct = totalPnL / at.initialBalance * 100
	}
	if st.Equity > 0 {
		usedPct = st.Exposure / st.Equity * 100
	}
	return map[string]interface{}{
		"total_equity":      st.Equity,
		"wallet_balance":    st.Equity - unrealized,
		"unrealized_profit": unrealized,
		"available_balance": st.Available,
		"total_pnl":         totalPnL,
		"total_pnl_pct":     pnlPct,
		"initial_balance":   at.initialBalance,
		"daily_pnl":         at.dailyPnL,
		"position_count":    len(st.Holdings),
		"margin_used":       st.Exposure,
		"margin_used_pct":   usedPct,
		"paper":             at.stockPaper(),
	}, nil
}

func (at *AutoTrader) stockPositionsAPI() ([]map[string]interface{}, error) {
	st, err := at.stockReadState(true)
	if err != nil {
		return nil, fmt.Errorf("failed to get stock positions: %w", err)
	}
	out := []map[string]interface{}{}
	for _, h := range st.sortedHoldings() {
		pnl := (h.Mark - h.Avg) * h.Owned
		pct := 0.0
		if h.Avg > 0 {
			pct = (h.Mark/h.Avg - 1) * 100
		}
		out = append(out, map[string]interface{}{
			"symbol": h.Symbol, "side": "long", "entry_price": h.Avg, "mark_price": h.Mark, "quantity": h.Owned,
			"leverage": 1, "unrealized_pnl": pnl, "unrealized_pnl_pct": pct, "liquidation_price": 0.0,
			"margin_used": h.Owned * h.Mark, "paper": at.stockPaper(),
			"protection": map[string]interface{}{"sl_price": h.Stop, "tp_price": h.TakeProfit},
		})
	}
	return out, nil
}

func (at *AutoTrader) stockRiskStatus() RiskStatus {
	out := RiskStatus{GeneratedAt: time.Now().UTC(), InitialBalance: at.initialBalance,
		LossStreakBans: []LossStreakBan{}, Pending: []PendingStatus{}, Positions: []PositionRStatus{}}
	st, err := at.stockReadState(true)
	if err != nil {
		return out
	}
	out.Equity = st.Equity
	if out.InitialBalance > 0 && out.Equity > 0 {
		out.AccountDrawdownPct = (out.InitialBalance - out.Equity) / out.InitialBalance * 100
	}
	for _, h := range st.sortedHoldings() {
		r := 0.0
		if h.InitialStop > 0 && h.Avg > h.InitialStop {
			r = (h.Mark - h.Avg) / (h.Avg - h.InitialStop)
		}
		out.Positions = append(out.Positions, PositionRStatus{Symbol: h.Symbol, Side: "long", EntryPrice: h.Avg, MarkPrice: h.Mark,
			InitialStop: h.InitialStop, CurrentStop: h.Stop, CurrentR: r, AIManaged: true, HasInitialStop: h.InitialStop > 0})
	}
	return out
}

// stockOpenOrders: paper has no exchange orders; live delegates to the executor.
func (at *AutoTrader) stockOpenOrders(symbol string) ([]OpenOrder, error) {
	if at.stockPaper() || at.stockTrader == nil {
		return []OpenOrder{}, nil
	}
	return at.stockTrader.GetOpenOrders(symbol)
}

// ForceCloseStockPosition closes the program-owned quantity of a pair at
// market (the close-position API for us_stock traders). Manual holdings of the
// same pair are not touched.
func (at *AutoTrader) ForceCloseStockPosition(symbol string) (map[string]interface{}, error) {
	if !at.IsStockStrategy() {
		return nil, fmt.Errorf("not a us_stock trader")
	}
	mu := at.stockMutex()
	mu.Lock()
	defer mu.Unlock()
	ctx := context.Background()
	now := at.stockClock()
	st, err := at.stockReadState(true)
	if err != nil {
		return nil, err
	}
	h := st.Holdings[symbol]
	if h == nil || h.Owned <= 0 {
		return nil, fmt.Errorf("no program-owned position in %s", symbol)
	}
	var out *stockOutcome
	if at.stockPaper() {
		price := at.stock.price(symbol)
		if price <= 0 {
			price = h.Mark
		}
		out, err = at.stockSellPaper(symbol, 0, price, "manual_close", now)
		if out != nil {
			out.Qty = h.Owned
		}
	} else {
		out, err = at.stockSellLive(ctx, h, h.Owned, true, "manual_close", now)
		if err == nil {
			at.syncStockJournal()
		}
	}
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"symbol": symbol, "status": "FILLED", "quantity": out.Qty, "price": out.Price, "realized_pnl": out.PnL,
		"orderId": out.OrderID, "paper": at.stockPaper()}, nil
}

// StockPaperView returns the paper ledger with equity valued at the latest
// known prices (average cost where no price has been seen yet).
func (at *AutoTrader) StockPaperView() (*StockPaperSnapshot, error) {
	if at.store == nil {
		return nil, fmt.Errorf("no store")
	}
	st, err := at.stockGatherPaper(nil)
	if err != nil {
		return nil, err
	}
	acct, err := at.store.StockPaper().GetAccount(at.id)
	if err != nil {
		return nil, err
	}
	open, err := at.store.StockPaper().ListOpen(at.id)
	if err != nil {
		return nil, err
	}
	closed, err := at.store.StockPaper().ListClosed(at.id, 50)
	if err != nil {
		return nil, err
	}
	return &StockPaperSnapshot{Account: acct, Equity: st.Equity, Open: open, Closed: closed}, nil
}

// StockPaperSnapshot is the payload of GET /api/usstock/paper/:trader_id.
type StockPaperSnapshot struct {
	Account *store.StockPaperAccount    `json:"account"`
	Equity  float64                     `json:"equity"`
	Open    []*store.StockPaperPosition `json:"open_positions"`
	Closed  []*store.StockPaperPosition `json:"closed_positions"`
}
