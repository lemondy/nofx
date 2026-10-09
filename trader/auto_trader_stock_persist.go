package trader

import (
	"fmt"
	"strings"
	"time"

	"nofx/logger"
	"nofx/store"
	"nofx/trader/types"
)

// us_stock (design 2026-10-09 §6): live persistence. A program buy creates or
// merges one OPEN ai_managed LONG trader_positions row per pair (the program's
// owned quantity); sells reduce / close it; closed rows feed trade_journal.

// stockRecordBuy books a filled program buy. qty is the NET quantity received
// (commission in the base asset already deducted). Returns the owned quantity
// after the buy.
func (at *AutoTrader) stockRecordBuy(symbol string, qty, price, fee float64, orderID string, stop float64, now time.Time) (float64, error) {
	pos := at.store.Position()
	rows, err := at.store.Position().StockOwnedRows(at.id, symbol)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		nowMs := now.UTC().UnixMilli()
		row := &store.TraderPosition{
			TraderID: at.id, ExchangeID: at.exchangeID, ExchangeType: "binance", Symbol: symbol, Side: "LONG",
			Quantity: qty, EntryQuantity: qty, EntryPrice: price, EntryOrderID: orderID, EntryTime: nowMs,
			Leverage: 1, Status: "OPEN", Source: "system", AIManaged: true, InitialStopLoss: stop, Fee: fee,
			CreatedAt: nowMs, UpdatedAt: nowMs,
		}
		if err := pos.CreateOpenPosition(row); err != nil {
			return 0, err
		}
	} else if err := pos.UpdatePositionQuantityAndPrice(rows[0].ID, qty, price, fee); err != nil {
		return 0, err
	}
	if err := at.store.AIManaged().MarkEntry(at.id, symbol, "long", orderID); err != nil {
		logger.Errorf("🤖 [%s] AI-managed MARK FAILED %s: %v", at.name, symbol, err)
	}
	rows, err = pos.StockOwnedRows(at.id, symbol)
	if err != nil {
		return 0, err
	}
	owned, _, _, _ := sumRows(rows)
	return owned, nil
}

// stockRecordSell books a filled program sell of qty. full closes the row(s);
// otherwise the quantity is reduced. Returns the leg's gross realized PnL
// (vs the program's own entry price).
func (at *AutoTrader) stockRecordSell(symbol string, qty, price, fee float64, orderID, reason string, full bool, now time.Time) float64 {
	rows, err := at.store.Position().StockOwnedRows(at.id, symbol)
	if err != nil || len(rows) == 0 {
		logger.Warnf("⚠️ [%s] sell of %s has no program-owned position row (%v) — nothing recorded", at.name, symbol, err)
		return 0
	}
	nowMs := now.UTC().UnixMilli()
	pnlTotal := 0.0
	remaining := qty
	for i, r := range rows {
		take := remaining
		if take > r.Quantity || (full && i == len(rows)-1) {
			take = r.Quantity
		}
		if take <= 0 {
			continue
		}
		pnl := (price - r.EntryPrice) * take
		pnlTotal += pnl
		share := 0.0
		if qty > 0 {
			share = fee * take / qty
		}
		if full || take >= r.Quantity-1e-9 {
			exit := price
			if closed := r.EntryQuantity - r.Quantity; closed > 0 && r.ExitPrice > 0 {
				exit = (r.ExitPrice*closed + price*take) / (closed + take)
			}
			err = at.store.Position().ClosePositionFully(r.ID, exit, orderID, nowMs, pnl, share, reason)
		} else {
			err = at.store.Position().ReducePositionQuantity(r.ID, take, price, share, pnl)
		}
		if err != nil {
			logger.Errorf("❌ [%s] recording sell of %s failed: %v", at.name, symbol, err)
		}
		remaining -= take
	}
	if full {
		_ = at.store.AIManaged().Unmark(at.id, symbol, "long")
		delete(at.stock.lastProtect, symbol)
		delete(at.stock.stopTicks, symbol)
		at.syncStockJournal()
	}
	return pnlTotal
}

func (at *AutoTrader) syncStockJournal() {
	if created, err := at.store.TradeJournal().SyncFromPositions(at.id); err != nil {
		logger.Infof("⚠ [%s] Failed to sync trade journal: %v", at.name, err)
	} else if created > 0 {
		logger.Infof("📓 [%s] Trade journal synced: %d new entries pending review", at.name, created)
	}
}

// stockFee converts an order's commission to USDT where that is knowable.
func stockFee(res *types.SpotOrderResult, price float64, baseAsset string) (feeUSDT, baseQty float64) {
	if res == nil || res.Commission <= 0 {
		return 0, 0
	}
	switch strings.ToUpper(res.CommissionAsset) {
	case "USDT", "":
		return res.Commission, 0
	case strings.ToUpper(baseAsset):
		return res.Commission * price, res.Commission
	}
	return 0, 0 // BNB etc.: not convertible here
}

func fillPrice(res *types.SpotOrderResult, fallback float64) float64 {
	switch {
	case res.AvgPrice > 0:
		return res.AvgPrice
	case res.ExecutedQty > 0 && res.QuoteQty > 0:
		return res.QuoteQty / res.ExecutedQty
	}
	return fallback
}

// persistedStop returns the stop the program last placed for a pair, falling
// back to the opening stop of the owned rows.
func (at *AutoTrader) persistedStop(sym string, h *stockHolding) (stop, tp float64) {
	if lp, ok := at.stock.lastProtect[sym]; ok && lp.Stop > 0 {
		return lp.Stop, lp.TakeProfit
	}
	if h != nil && h.InitialStop > 0 {
		return h.InitialStop, 0
	}
	return 0, 0
}

// restoreStockPendings reloads resting limit entries after a restart.
func (at *AutoTrader) restoreStockPendings() {
	rows, err := at.store.StockPaper().ListPending(at.id)
	if err != nil {
		logger.Warnf("⚠️ [%s] cannot load resting us_stock entries: %v", at.name, err)
		return
	}
	for _, r := range rows {
		at.stock.pendings[r.Symbol] = &stockPending{Symbol: r.Symbol, OrderID: r.OrderID, Action: r.Action, Qty: r.Quantity,
			FilledQty: r.FilledQty, Limit: r.LimitPrice, Stop: r.Stop, TakeProfit: r.TakeProfit, PlacedAt: time.UnixMilli(r.PlacedAt)}
		logger.Infof("📌 [%s] resting limit entry restored: %s order %s", at.name, r.Symbol, r.OrderID)
	}
}

func (at *AutoTrader) savePending(p *stockPending) {
	err := at.store.StockPaper().UpsertPending(&store.StockPendingEntry{TraderID: at.id, Symbol: p.Symbol, OrderID: p.OrderID,
		Action: p.Action, Quantity: p.Qty, FilledQty: p.FilledQty, LimitPrice: p.Limit, Stop: p.Stop, TakeProfit: p.TakeProfit, PlacedAt: p.PlacedAt.UnixMilli()})
	if err != nil {
		logger.Errorf("❌ [%s] cannot persist resting entry %s: %v", at.name, p.Symbol, err)
	}
}

func (at *AutoTrader) dropPending(symbol string) {
	delete(at.stock.pendings, symbol)
	if err := at.store.StockPaper().DeletePending(at.id, symbol); err != nil {
		logger.Warnf("⚠️ [%s] cannot delete resting entry row %s: %v", at.name, symbol, err)
	}
}

func fmtQty(v float64) string { return fmt.Sprintf("%.6f", v) }
