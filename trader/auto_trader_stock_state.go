package trader

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"nofx/kernel/stockengine"
	"nofx/logger"
	"nofx/store"

	notify "nofx/telegram/notify"
)

// us_stock (design 2026-10-09): account / holdings state of one cycle or tick.

// stockHolding is a PROGRAM-OWNED position (the quantity recorded in the
// program's own ai_managed trader_positions rows, or the paper ledger row).
type stockHolding struct {
	Symbol      string
	Owned       float64 // program-owned quantity (never above the exchange balance)
	Balance     float64 // exchange base-asset balance (live); = Owned in paper
	Avg         float64 // program's own entry average (NOT the account-wide FIFO cost)
	Mark        float64
	Stop        float64 // live protective stop (0 = none)
	TakeProfit  float64
	InitialStop float64
	OpenedAt    time.Time
	Rows        []*store.TraderPosition // live
	ProtMissing bool                    // live: protection queried and found nil
}

// stockState is the account view the cycle decides on.
type stockState struct {
	Holdings  map[string]*stockHolding // program-owned positions
	Balances  map[string]float64       // exchange balances by pair (live; includes manual holdings)
	Marks     map[string]float64
	Available float64 // free USDT (paper: cash)
	Equity    float64
	Exposure  float64 // value of ALL holdings, manual ones included (risk is account-wide)
	Notes     []string
}

func (s *stockState) sortedHoldings() []*stockHolding {
	out := make([]*stockHolding, 0, len(s.Holdings))
	for _, h := range s.Holdings {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// manualNotes lists balances above the program-owned quantity. Manual
// holdings are NOT shown to the model (the program never manages them); they
// only count in Exposure/Equity.
func (s *stockState) manualNotes() []string {
	var notes []string
	for sym, bal := range s.Balances {
		owned := 0.0
		if h := s.Holdings[sym]; h != nil {
			owned = h.Owned
		}
		if extra := bal - owned; extra > 1e-9 {
			notes = append(notes, fmt.Sprintf("manual/external holding %s qty=%.6f (not managed by the program)", sym, extra))
		}
	}
	sort.Strings(notes)
	return notes
}

func numberOf(m map[string]interface{}, keys ...string) float64 {
	for _, k := range keys {
		if v, err := SafeFloat64(m, k); err == nil {
			return v
		}
	}
	return 0
}

// stockOwnedRows loads the program's OPEN ai_managed LONG rows grouped by pair.
func (at *AutoTrader) stockOwnedRows() (map[string][]*store.TraderPosition, error) {
	rows, err := at.store.Position().GetOpenPositions(at.id)
	if err != nil {
		return nil, err
	}
	out := map[string][]*store.TraderPosition{}
	for _, r := range rows {
		if r.AIManaged && strings.EqualFold(r.Side, "LONG") && strings.HasSuffix(r.Symbol, "BUSDT") && r.Quantity > 0 {
			out[r.Symbol] = append(out[r.Symbol], r)
		}
	}
	return out, nil
}

func sumRows(rows []*store.TraderPosition) (qty, avg float64, initial float64, opened time.Time) {
	cost := 0.0
	for _, r := range rows {
		qty += r.Quantity
		cost += r.Quantity * r.EntryPrice
		if initial == 0 {
			initial = r.InitialStopLoss
		}
		if t := time.UnixMilli(r.EntryTime); opened.IsZero() || t.Before(opened) {
			opened = t
		}
	}
	if qty > 0 {
		avg = cost / qty
	}
	return
}

// stockGatherLive reads the spot account. With reconcile, a balance below the
// owned quantity shrinks the program position (external reduction: no PnL
// guess). withProtection also queries each holding's protective orders.
func (at *AutoTrader) stockGatherLive(ctx context.Context, now time.Time, reconcile, withProtection bool) (*stockState, error) {
	bal, err := at.stockTrader.GetBalance()
	if err != nil {
		return nil, fmt.Errorf("get balance: %w", err)
	}
	poss, err := at.stockTrader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("get positions: %w", err)
	}
	st := &stockState{Holdings: map[string]*stockHolding{}, Balances: map[string]float64{}, Marks: map[string]float64{}}
	st.Available = numberOf(bal, "availableBalance", "available_balance")
	for _, p := range poss {
		sym, _ := p["symbol"].(string)
		qty := numberOf(p, "positionAmt", "position_amt", "quantity")
		if qty < 0 {
			qty = -qty
		}
		if sym == "" || qty <= 0 {
			continue
		}
		mark := numberOf(p, "markPrice", "mark_price", "price")
		st.Balances[sym] += qty
		if mark > 0 {
			st.Marks[sym] = mark
			at.stock.setPrice(sym, mark)
		}
		st.Exposure += qty * mark
	}
	if eq := firstPositive(bal, "totalEquity", "total_equity"); eq > 0 {
		st.Equity = eq
	} else {
		st.Equity = st.Available + st.Exposure
	}

	owned, err := at.stockOwnedRows()
	if err != nil {
		return nil, fmt.Errorf("load owned positions: %w", err)
	}
	for sym, rows := range owned {
		qty, avg, initial, opened := sumRows(rows)
		balance := st.Balances[sym]
		info := stockSymbolInfo(ctx, sym)
		if reconcile && qty-balance > maxFloat(info.StepSize, 1e-9)/2 {
			newQty := at.stockShrinkOwned(sym, rows, qty, balance, st.Marks[sym], now, st)
			if newQty <= 0 {
				continue
			}
			if owned2, e := at.stockOwnedRows(); e == nil {
				rows = owned2[sym]
				qty, avg, initial, opened = sumRows(rows)
			}
		}
		if qty > balance {
			qty = balance // read-only callers: never act on more than the balance
		}
		if qty <= 0 {
			continue
		}
		h := &stockHolding{Symbol: sym, Owned: qty, Balance: balance, Avg: avg, Mark: st.Marks[sym],
			InitialStop: initial, OpenedAt: opened, Rows: rows}
		if withProtection {
			prot, perr := at.stockTrader.GetProtection(sym)
			switch {
			case perr != nil:
				logger.Warnf("⚠️ [%s] GetProtection %s: %v", at.name, sym, perr)
			case prot == nil:
				h.ProtMissing = true
			default:
				h.Stop, h.TakeProfit = prot.StopPrice, prot.TakeProfit
				if reconcile { // loop callers only: API readers must not write run state
					at.stock.lastProtect[sym] = stockProtect{Stop: prot.StopPrice, TakeProfit: prot.TakeProfit, Qty: prot.Quantity}
				}
			}
		}
		st.Holdings[sym] = h
	}
	return st, nil
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// stockShrinkOwned reduces the program-owned rows of sym from owned to the
// exchange balance after an external (manual / exchange-side) reduction. No
// exit is booked and no PnL attributed. Returns the new owned quantity.
func (at *AutoTrader) stockShrinkOwned(sym string, rows []*store.TraderPosition, owned, balance, mark float64, now time.Time, st *stockState) float64 {
	info := stockSymbolInfo(context.Background(), sym)
	target := floorStep(balance, info.StepSize)
	if target < 0 {
		target = 0
	}
	diff := owned - target
	if target <= 0 {
		// Whole holding gone. If the program's own protection was live and the
		// price is at that level, the exchange-side stop / take-profit filled:
		// book it as the program's exit. Otherwise it is an external sale.
		px := mark
		if px <= 0 && at.stockTrader != nil {
			if p, e := at.stockTrader.GetMarketPrice(sym); e == nil {
				px = p
			}
		}
		if px <= 0 {
			px = at.stock.price(sym)
		}
		if lp, ok := at.stock.lastProtect[sym]; ok && px > 0 {
			exit, reason := 0.0, ""
			switch {
			case lp.Stop > 0 && px <= lp.Stop*1.02:
				exit, reason = lp.Stop, "stop_loss"
			case lp.TakeProfit > 0 && px >= lp.TakeProfit*0.98:
				exit, reason = lp.TakeProfit, "take_profit"
			}
			if reason != "" {
				at.stockRecordSell(sym, owned, exit, 0, "", reason, true, now)
				note := fmt.Sprintf("%s%s on %s filled on the exchange (booked at %.4f, qty %.6f)", stockPrefix(at.stockPaper()), reason, sym, exit, owned)
				logger.Infof("🛑 [%s] %s", at.name, note)
				if st != nil {
					st.Notes = append(st.Notes, note)
				}
				stockNotify("ORDER", at.name, fmt.Sprintf("<b>🛑 美股交易所端 %s 成交 %s</b>\n<i>%.6g @ %.4g（按保护价记账）</i>", reason, notify.Escape(sym), owned, exit))
				return 0
			}
		}
	}
	for i := len(rows) - 1; i >= 0 && diff > 1e-12; i-- {
		take := diff
		if rows[i].Quantity < take {
			take = rows[i].Quantity
		}
		if err := at.store.Position().ApplyExternalReduction(rows[i].ID, rows[i].Quantity-take, mark, now); err != nil {
			logger.Errorf("❌ [%s] external-reduction update failed for %s: %v", at.name, sym, err)
			return owned
		}
		diff -= take
	}
	if target <= 0 {
		_ = at.store.AIManaged().Unmark(at.id, sym, "long")
		delete(at.stock.lastProtect, sym)
	}
	pfx := stockPrefix(at.stockPaper())
	note := fmt.Sprintf("%sexternal reduction on %s: program-owned %.6f -> exchange balance %.6f; position shrunk, no PnL attributed", pfx, sym, owned, target)
	logger.Warnf("⚠️ [%s] %s", at.name, note)
	if st != nil {
		st.Notes = append(st.Notes, note)
	}
	stockNotify("ALERT", at.name, fmt.Sprintf("<b>⚠️ 美股持仓被外部减少 %s</b>\n<i>程序持仓 %.6f → 交易所余额 %.6f，已按余额收缩（外部减仓，不计盈亏）</i>",
		notify.Escape(sym), owned, target))
	return target
}

// stockGatherPaper reads the paper ledger. prices (latest known) value the
// holdings; a pair without a price is valued at its average cost.
func (at *AutoTrader) stockGatherPaper(prices map[string]float64) (*stockState, error) {
	acct, err := at.store.StockPaper().EnsureAccount(at.id, at.initialBalance)
	if err != nil {
		return nil, err
	}
	open, err := at.store.StockPaper().ListOpen(at.id)
	if err != nil {
		return nil, err
	}
	st := &stockState{Holdings: map[string]*stockHolding{}, Balances: map[string]float64{}, Marks: map[string]float64{}, Available: acct.Cash}
	for _, p := range open {
		mark := prices[p.Symbol]
		if mark <= 0 {
			mark = at.stock.price(p.Symbol)
		}
		if mark <= 0 {
			mark = p.AvgPrice
		}
		st.Holdings[p.Symbol] = &stockHolding{Symbol: p.Symbol, Owned: p.Quantity, Balance: p.Quantity, Avg: p.AvgPrice, Mark: mark,
			Stop: p.Stop, TakeProfit: p.TakeProfit, InitialStop: p.InitialStop, OpenedAt: time.UnixMilli(p.OpenedAt)}
		st.Balances[p.Symbol] = p.Quantity
		st.Marks[p.Symbol] = mark
		st.Exposure += p.Quantity * mark
	}
	st.Equity = st.Available + st.Exposure
	return st, nil
}

// reprice revalues a paper state with fresh prices (live equity comes from
// the exchange and is left alone).
func (s *stockState) repricePaper(prices map[string]float64) {
	exposure := 0.0
	for sym, h := range s.Holdings {
		if p := prices[sym]; p > 0 {
			h.Mark = p
			s.Marks[sym] = p
		}
		exposure += h.Owned * h.Mark
	}
	s.Exposure = exposure
	s.Equity = s.Available + exposure
}

func (s *stockState) enginePositions(prices map[string]float64) []stockengine.Position {
	var out []stockengine.Position
	for _, h := range s.sortedHoldings() {
		price := h.Mark
		if p := prices[h.Symbol]; p > 0 {
			price = p
		}
		out = append(out, stockengine.Position{Symbol: h.Symbol, Quantity: h.Owned, AvgPrice: h.Avg, Price: price,
			StopPrice: h.Stop, TakeProfit: h.TakeProfit, InitialStop: h.InitialStop, OpenedAt: h.OpenedAt})
	}
	return out
}

func stockPrefix(paper bool) string {
	if paper {
		return "[PAPER] "
	}
	return ""
}
