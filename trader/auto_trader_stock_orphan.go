package trader

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"nofx/logger"
	"nofx/store"
	"nofx/trader/types"

	notify "nofx/telegram/notify"
)

// us_stock: live resting entries left behind when the strategy switches to
// paper mode (review 2026-10-22). The paper instance has no live executor and
// must never place an order; it may only (a) cancel / read the order through a
// temporary executor (cancelOrphanStockPendings), (b) book the shares that
// really filled into the LIVE ledger together with the "first protection still
// to be placed" intent, and (c) keep telling the user until that is resolved.
// The protection itself is placed by the live protection tick after a switch
// back to live (stockGatherLive reads the intent + the opening stop of the row).

// stockUnprotectedRemindEvery bounds the repeated alert while real shares sit
// without exchange protection (one ALERT per interval, not per tick).
const stockUnprotectedRemindEvery = time.Hour

// stockFillDelta is the not-yet-booked part of a resting entry's cumulative
// executions, priced from the quote/fee DELTAS against the persisted watermark.
type stockFillDelta struct {
	Executed float64 // cumulative executed quantity (the new watermark)
	Inc      float64 // increment over the watermark
	Price    float64 // price of the increment
	IncFee   float64 // fee of the increment (USDT)
	Quote    float64 // cumulative quote (the new watermark)
	Fee      float64 // cumulative fee (the new watermark)
}

// stockPendingDelta computes the increment over (filledQty, filledQuote,
// filledFee) from an exchange order status. ok=false when nothing new executed
// or no price can be derived (nothing may be booked then).
func stockPendingDelta(filledQty, filledQuote, filledFee float64, status map[string]interface{}) (d stockFillDelta, ok bool) {
	executed := numberOf(status, "executedQty")
	inc := executed - filledQty
	if inc <= 1e-12 {
		return d, false
	}
	avg := numberOf(status, "avgPrice")
	quote := numberOf(status, "cummulativeQuoteQty")
	fee := numberOf(status, "commission")
	if avg <= 0 && quote > 0 {
		avg = quote / executed
	}
	if avg <= 0 {
		return d, false
	}
	// Rows persisted before the quote/fee watermarks existed carry FilledQty
	// without FilledQuote: fall back to the old cumulative-average estimate.
	legacy := filledQty > 1e-12 && filledQuote <= 0
	price, incFee := avg, fee*inc/executed
	if !legacy && quote > filledQuote+1e-12 {
		price = (quote - filledQuote) / inc
		incFee = math.Max(fee-filledFee, 0)
	}
	if quote <= 0 {
		quote = avg * executed
	}
	return stockFillDelta{Executed: executed, Inc: inc, Price: price, IncFee: incFee, Quote: quote, Fee: fee}, true
}

// stockBookOrphanFill books the executions of a kept live entry that filled
// while the instance is paper (or otherwise without a live executor). It is
// idempotent: the position increment and the entry's fill watermark commit in
// one transaction (BookEntryFill), so a replay or crash cannot book a fill
// twice, and it places NO order. exec is only read (balance) through. The first
// protection intent is recorded so the live protection tick places the stop.
// Returns a one-line description of what was booked ("" when nothing new).
func (at *AutoTrader) stockBookOrphanFill(exec types.SpotStockTrader, r *store.StockPendingEntry, status map[string]interface{}, now time.Time) (string, error) {
	d, ok := stockPendingDelta(r.FilledQty, r.FilledQuote, r.FilledFee, status)
	if !ok {
		if executed := numberOf(status, "executedQty"); executed > r.FilledQty+1e-9 {
			return "", fmt.Errorf("%.6f shares executed beyond the booked %.6f but no fill price is available", executed, r.FilledQty)
		}
		return "", nil
	}
	net := at.stockNetReceivedVia(exec, r.Symbol, d.Inc)
	next := &stockPending{Symbol: r.Symbol, OrderID: r.OrderID, Action: r.Action, Qty: r.Quantity,
		FilledQty: d.Executed, FilledQuote: d.Quote, FilledFee: d.Fee, Limit: r.LimitPrice, Stop: r.Stop, TakeProfit: r.TakeProfit,
		PlacedAt: time.UnixMilli(r.PlacedAt)}
	_, booked, err := at.stockRecordBuyAtomic(r.Symbol, net, d.Price, d.IncFee, r.OrderID, r.Stop, now, next, true)
	if err != nil {
		return "", fmt.Errorf("book orphan fill: %w", err)
	}
	if !booked {
		return "", nil // watermark already covered it
	}
	return fmt.Sprintf("%s: %.6f shares @ %.4f (order %s, stop %.4g)", r.Symbol, net, d.Price, r.OrderID, r.Stop), nil
}

// stockUnprotectedPosition is a program-owned live holding whose first
// protection has not been confirmed.
type stockUnprotectedPosition struct {
	Symbol   string  `json:"symbol"`
	Quantity float64 `json:"quantity"`
	Stop     float64 `json:"stop"`
}

// stockUnprotectedLive lists the real holdings the program booked but never
// protected (open owned rows with a protection intent) and the kept live entry
// orders whose fills are not settled yet. Read-only; safe in every mode.
func (at *AutoTrader) stockUnprotectedLive() (positions []stockUnprotectedPosition, pendings []string, err error) {
	if at.store == nil {
		return nil, nil, nil
	}
	intents, err := at.store.StockPaper().ListProtectionIntents(at.id)
	if err != nil {
		return nil, nil, err
	}
	for sym := range intents {
		rows, e := at.store.Position().StockOwnedRows(at.id, sym)
		if e != nil {
			return nil, nil, e
		}
		qty, _, _, _ := sumRows(rows)
		if qty <= 1e-12 {
			continue
		}
		stop := 0.0
		for _, row := range rows {
			if row.InitialStopLoss > 0 {
				stop = row.InitialStopLoss
			}
		}
		positions = append(positions, stockUnprotectedPosition{Symbol: sym, Quantity: qty, Stop: stop})
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })
	rows, err := at.store.StockPaper().ListPending(at.id)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		pendings = append(pendings, fmt.Sprintf("%s(order %s)", r.Symbol, r.OrderID))
	}
	sort.Strings(pendings)
	return positions, pendings, nil
}

// stockRemindUnprotectedLive runs on the paper protection tick. While live
// entries or unprotected live shares remain it retries the orphan handling and
// repeats the ALERT at most once per stockUnprotectedRemindEvery (deduplicated;
// the first window starts at the first tick, the switch itself already alerted).
func (at *AutoTrader) stockRemindUnprotectedLive(now time.Time) {
	if at.store == nil {
		return
	}
	if at.stock.unprotectedRemindAt.IsZero() {
		at.stock.unprotectedRemindAt = now
		return
	}
	if now.Sub(at.stock.unprotectedRemindAt) < stockUnprotectedRemindEvery {
		return
	}
	at.stock.unprotectedRemindAt = now
	if _, pendings, err := at.stockUnprotectedLive(); err == nil && len(pendings) > 0 {
		at.cancelOrphanStockPendings("hourly retry in paper mode")
	}
	positions, pendings, err := at.stockUnprotectedLive()
	if err != nil {
		logger.Warnf("⚠️ [%s] cannot read unprotected live holdings: %v", at.name, err)
		return
	}
	if len(positions) == 0 && len(pendings) == 0 {
		return
	}
	var parts []string
	for _, p := range positions {
		parts = append(parts, fmt.Sprintf("%s %.6f 股(止损 %.4g)", p.Symbol, p.Quantity, p.Stop))
	}
	if len(pendings) > 0 {
		parts = append(parts, "未结算的限价入场单: "+strings.Join(pendings, ", "))
	}
	msg := strings.Join(parts, "；")
	logger.Warnf("⚠️ [%s] real us_stock shares without exchange protection while in paper mode: %s", at.name, msg)
	stockNotify("ALERT", at.name, fmt.Sprintf("<b>🚨 美股实盘持仓无保护单（当前为模拟盘）</b>\n<i>%s — 模拟盘不会下真实订单；请切回实盘（保护检查会按止损补挂）或手动挂止损/卖出</i>", notify.Escape(msg)))
}
