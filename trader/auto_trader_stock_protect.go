package trader

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nofx/logger"

	notify "nofx/telegram/notify"
)

// us_stock (design 2026-10-09 §5/§6): the 60s protection tick and resting
// limit-entry reconciliation.

// stockProtectionTick re-places missing protection, sells at market when the
// price sits below the stop and the stop order has not filled for
// stockStopFallbackTicks ticks (program fallback), and in paper mode simulates
// stop / take-profit fills.
func (at *AutoTrader) stockProtectionTick(ctx context.Context) {
	mu := at.stockMutex()
	if !mu.TryLock() { // a decision cycle is running
		return
	}
	defer mu.Unlock()
	if !at.stockRunning() || at.stock.cfg == nil || ctx.Err() != nil {
		return
	}
	now := at.stockClock()
	at.stock.tickCount++
	logf := func(format string, args ...interface{}) {
		logger.Infof("🇺🇸 [%s] %s%s", at.name, stockPrefix(at.stockPaper()), fmt.Sprintf(format, args...))
	}
	if at.stockPaper() {
		at.stockTickPaper(ctx, now, logf)
	} else {
		at.stockTickLive(ctx, now, logf)
	}
}

func (at *AutoTrader) stockTickPaper(ctx context.Context, now time.Time, logf func(string, ...interface{})) {
	open, err := at.store.StockPaper().ListOpen(at.id)
	if err != nil {
		logf("paper tick: %v", err)
		return
	}
	for _, p := range open {
		q, err := stockGetQuote(ctx, p.Symbol)
		if err != nil || q == nil || q.BStockPrice <= 0 {
			logf("paper tick price %s unavailable: %v", p.Symbol, err)
			continue
		}
		price := q.BStockPrice
		at.stock.setPrice(p.Symbol, price)
		fill, reason := 0.0, ""
		switch {
		case p.Stop > 0 && price <= p.Stop:
			fill, reason = p.Stop, "stop_loss"
		case p.TakeProfit > 0 && price >= p.TakeProfit:
			fill, reason = p.TakeProfit, "take_profit"
		}
		if reason == "" {
			continue
		}
		out, err := at.stockSellPaper(p.Symbol, 0, fill, reason, now)
		if err != nil {
			logf("paper %s sell failed: %v", reason, err)
			continue
		}
		out.Qty = p.Quantity
		logf("%s %s: sold %.6f at %.4f (pnl %.2f)", reason, p.Symbol, p.Quantity, fill, out.PnL)
		at.stockNotifyTrade(true, map[string]string{"stop_loss": "止损触发", "take_profit": "止盈触发"}[reason], p.Symbol, out, "")
	}
	if at.stock.tickCount%stockSnapshotEvery == 0 {
		if st, err := at.stockGatherPaper(nil); err == nil {
			at.saveStockEquity(now, st)
		}
	}
}

func (at *AutoTrader) stockTickLive(ctx context.Context, now time.Time, logf func(string, ...interface{})) {
	at.reconcileStockPendings(ctx, now, logf)
	st, err := at.stockGatherLive(ctx, now, true, true)
	if err != nil {
		logf("tick: %v", err)
		return
	}
	for _, n := range st.Notes {
		logf("%s", n)
	}
	for _, h := range st.sortedHoldings() {
		price := h.Mark
		if p, e := at.stockTrader.GetMarketPrice(h.Symbol); e == nil && p > 0 {
			price = p
		}
		at.stock.setPrice(h.Symbol, price)
		h.Mark = price
		switch {
		case h.ProtMissing:
			stop, tp := at.persistedStop(h.Symbol, h)
			if stop <= 0 {
				stockNotify("ALERT", at.name, fmt.Sprintf("<b>🚨 美股持仓无保护单且无已知止损 %s</b>\n<i>请人工处理</i>", notify.Escape(h.Symbol)))
				continue
			}
			if price > 0 && price <= stop {
				at.stockFallbackSell(ctx, h, now, logf, "no protection and price below stop")
				continue
			}
			if err := at.stockPlaceProtection(ctx, h.Symbol, h.Owned, stop, tp, price); err != nil {
				logf("re-placing protection for %s failed: %v", h.Symbol, err)
				stockNotify("ALERT", at.name, fmt.Sprintf("<b>🚨 美股保护单缺失且补挂失败 %s</b>\n<i>%s</i>", notify.Escape(h.Symbol), notify.Escape(err.Error())))
				continue
			}
			logf("protection for %s was missing — re-placed (stop %.4f)", h.Symbol, stop)
			stockNotify("ALERT", at.name, fmt.Sprintf("<b>⚠️ 美股保护单缺失，已按止损 %.4g 补挂 %s</b>", stop, notify.Escape(h.Symbol)))
		case h.Stop > 0 && price > 0 && price <= h.Stop:
			at.stock.stopTicks[h.Symbol]++
			if at.stock.stopTicks[h.Symbol] >= stockStopFallbackTicks {
				at.stockFallbackSell(ctx, h, now, logf, "price below stop with the stop order still open")
			}
		default:
			at.stock.stopTicks[h.Symbol] = 0
		}
	}
	if at.stock.tickCount%stockSnapshotEvery == 0 {
		at.saveStockEquity(now, st)
	}
}

// stockFallbackSell is the program-side stop: sell the OWNED quantity at market.
func (at *AutoTrader) stockFallbackSell(ctx context.Context, h *stockHolding, now time.Time, logf func(string, ...interface{}), why string) {
	out, err := at.stockSellLive(ctx, h, h.Owned, true, "stop_fallback", now)
	if err != nil {
		logf("fallback sell of %s FAILED: %v", h.Symbol, err)
		stockNotify("ALERT", at.name, fmt.Sprintf("<b>🚨 美股止损兜底卖出失败 %s</b>\n<i>%s</i>", notify.Escape(h.Symbol), notify.Escape(err.Error())))
		return
	}
	logf("fallback sell %s: %s — sold %.6f at %.4f (pnl %.2f)", h.Symbol, why, out.Qty, out.Price, out.PnL)
	stockNotify("ALERT", at.name, fmt.Sprintf("<b>🛑 美股止损兜底市价卖出 %s</b>\n<i>%s；%.6g @ %.4g，盈亏 %.2f</i>",
		notify.Escape(h.Symbol), notify.Escape(why), out.Qty, out.Price, out.PnL))
}

// reconcileStockPendings books fills of resting limit entries (protecting the
// filled quantity) and forgets terminal orders. The program never cancels the
// resting order itself (only its protective orders).
func (at *AutoTrader) reconcileStockPendings(ctx context.Context, now time.Time, logf func(string, ...interface{})) {
	for sym, p := range at.stock.pendings {
		status, err := at.stockTrader.GetOrderStatus(sym, p.OrderID)
		if err != nil {
			logf("resting entry %s order %s status: %v", sym, p.OrderID, err)
			continue
		}
		executed := numberOf(status, "executedQty")
		avg := numberOf(status, "avgPrice")
		fee := numberOf(status, "commission")
		if inc := executed - p.FilledQty; inc > 1e-12 && avg > 0 {
			net := at.stockNetReceived(sym, inc)
			lp := at.stock.lastProtect[sym]
			h := &stockHolding{Symbol: sym, Stop: lp.Stop, TakeProfit: lp.TakeProfit}
			incFee := fee * inc / executed
			if _, err := at.stockBuyFilled(ctx, sym, inc, avg, incFee, inc-net, p.OrderID, p.Stop, p.TakeProfit, h, now, logf); err != nil {
				logf("resting entry %s fill could not be booked: %v", sym, err)
				continue
			}
			p.FilledQty = executed
			at.savePending(p)
			logf("resting limit entry %s filled %.6f @ %.4f — booked and protected", sym, inc, avg)
			stockNotify("ORDER", at.name, fmt.Sprintf("<b>📌 美股限价入场成交 %s</b>\n<i>%.6g @ %.4g，保护单已挂</i>", notify.Escape(sym), inc, avg))
		}
		switch strings.ToUpper(fmt.Sprint(status["status"])) {
		case "FILLED", "CANCELED", "CANCELLED", "EXPIRED", "REJECTED", "EXPIRED_IN_MATCH":
			at.dropPending(sym)
		}
	}
}

// stockNetReceived caps a fill increment by the quantity that actually arrived
// in the account (base-asset commission lowers the received units).
func (at *AutoTrader) stockNetReceived(sym string, inc float64) float64 {
	poss, err := at.stockTrader.GetPositions()
	if err != nil {
		return inc
	}
	balance := 0.0
	for _, p := range poss {
		if s, _ := p["symbol"].(string); s == sym {
			balance = numberOf(p, "positionAmt", "position_amt", "quantity")
		}
	}
	rows, _ := at.store.Position().StockOwnedRows(at.id, sym)
	owned, _, _, _ := sumRows(rows)
	if room := balance - owned; room > 0 && room < inc {
		return room
	}
	return inc
}
