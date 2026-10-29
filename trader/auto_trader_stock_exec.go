package trader

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"nofx/kernel/stockengine"
	"nofx/logger"
	"nofx/market/usstock"
	"nofx/trader/types"

	notify "nofx/telegram/notify"
)

// us_stock (design 2026-10-09 §5/§6): order execution against the live spot
// executor or the paper ledger. Live sells always pass an EXPLICIT quantity
// (min(program-owned, exchange balance), step-rounded) and protection covers
// the owned quantity only — the user's manual holdings are never touched.

var errStopNotBelowPrice = errors.New("stop is not below the current price")

// stockFirstProtectionMaxAttempts is how many failed protection ticks a freshly
// bought position may accumulate before it is exited at market.
const stockFirstProtectionMaxAttempts = 5

// stockOutcome is the result of one executed decision.
type stockOutcome struct {
	Qty, Price, Fee, PnL float64
	OrderID              string
	Pending              bool // limit entry resting on the exchange
	Closed               bool
	Note                 string
}

func minNotionalOf(info usstock.SymbolInfo) float64 {
	return math.Max(5, info.MinNotional)
}

// stockStopLimit derives the stop-limit price: stop × (1 − 0.3%), tick-safe.
func stockStopLimit(stop float64, info usstock.SymbolInfo) float64 {
	return floorStep(stop*(1-stockStopLimitBuffer), info.TickSize)
}

func orderIDInt(id string) int64 {
	n, _ := strconv.ParseInt(id, 10, 64)
	return n
}

// ------------------------------------------------------------ live helpers

// stockFormatQty rounds a sell quantity down to the lot step via the
// executor's FormatQuantity (falls back to the registry step).
func (at *AutoTrader) stockFormatQty(sym string, qty float64, info usstock.SymbolInfo) float64 {
	if s, err := at.stockTrader.FormatQuantity(sym, qty); err == nil {
		if v, e := strconv.ParseFloat(s, 64); e == nil {
			return v
		}
	}
	return floorStep(qty, info.StepSize)
}

// stockPlaceProtection (re)places the protective orders for the OWNED
// quantity. tp is dropped when it is not above the price.
func (at *AutoTrader) stockPlaceProtection(ctx context.Context, sym string, owned, stop, tp, price float64) error {
	info := stockSymbolInfo(ctx, sym)
	qty := at.stockFormatQty(sym, owned, info)
	stop = floorStep(stop, info.TickSize)
	if stop <= 0 || qty <= 0 {
		return fmt.Errorf("no protectable quantity/stop for %s (qty=%v stop=%v)", sym, qty, stop)
	}
	if price > 0 && stop >= price {
		return errStopNotBelowPrice
	}
	if tp > 0 {
		tp = nearestStep(tp, info.TickSize)
		if price > 0 && tp <= price {
			tp = 0
		}
	}
	if band, ok, err := at.stockSellBand(sym); err != nil {
		logger.Warnf("⚠️ [%s] cannot read sell price band for %s: %v", at.name, sym, err)
	} else if ok {
		if tp > 0 && band.Ceiling > 0 && tp > band.Ceiling {
			logger.Warnf("⚠️ [%s] dropping %s take-profit %.4f above sell ceiling %.4f", at.name, sym, tp, band.Ceiling)
			tp = 0
		}
		if limit := stockStopLimit(stop, info); band.Floor > 0 && limit < band.Floor {
			return fmt.Errorf("%w: stop %.4f (limit %.4f) below sell floor %.4f", types.ErrStopBelowSellBand, stop, limit, band.Floor)
		}
	}
	if _, err := at.stockPrepareProtectionChange(sym, at.stockClock()); err != nil {
		return err
	}
	freshOwned, balance, err := at.stockLiveQuantities(sym)
	if err != nil {
		return err
	}
	qty = at.stockFormatQty(sym, math.Min(qty, math.Min(freshOwned, balance)), info)
	if qty <= 0 {
		return fmt.Errorf("no program-owned quantity left to protect for %s", sym)
	}
	p, placeErr := at.stockTrader.SetProtection(sym, qty, stop, stockStopLimit(stop, info), tp)
	// An accepted response can contain IDs alongside an error. Preserve every
	// returned leg; never erase the old IDs on an ambiguous replacement.
	if p != nil {
		if err := at.stockSaveProtection(p); err != nil {
			at.stockProtectionAlert(sym, "protection_change", err)
			return fmt.Errorf("protection placed but IDs could not be saved: %w", err)
		}
	}
	if placeErr != nil {
		return placeErr
	}
	if p == nil || p.StopOrderID == "" || (tp > 0 && p.TakeProfitID == "") {
		return fmt.Errorf("protection response lacks required leg IDs")
	}
	at.stock.lastProtect[sym] = stockProtect{Stop: stop, TakeProfit: tp, Qty: qty}
	at.stock.stopTicks[sym] = 0
	delete(at.stock.bandHeld, sym)
	if err := at.store.StockPaper().ClearProtectionIntent(at.id, sym); err != nil {
		logger.Warnf("⚠️ [%s] cannot clear the protection intent of %s: %v", at.name, sym, err)
	}
	return nil
}

// stockPrepareProtectionChange retires old legs only after reading their final
// executions. Used by replace AND sell, including decisions made from a stale
// cycle snapshot while exchange protection continued trading.
func (at *AutoTrader) stockPrepareProtectionChange(sym string, now time.Time) (stockProtectionCheck, error) {
	p, err := at.stockTrader.GetProtection(sym)
	if err == nil && p != nil {
		err = at.stockSaveProtection(p)
	}
	var before, after stockProtectionCheck
	if err == nil {
		before, err = at.stockCheckProtection(sym, now)
	}
	if err == nil {
		after, err = at.stockCancelProtection(sym, now)
	}
	if err != nil {
		at.stockProtectionAlert(sym, "protection_change", err)
		return before, err
	}
	before.Filled = before.Filled || after.Filled
	before.Known = before.Known || after.Known
	before.Open = false
	return before, nil
}

func (at *AutoTrader) stockLiveQuantities(sym string) (owned, balance float64, err error) {
	rows, err := at.store.Position().StockOwnedRows(at.id, sym)
	if err != nil {
		return 0, 0, &stockProtectionReadError{err}
	}
	owned, _, _, _ = sumRows(rows)
	balance, err = at.stockRawBalance(sym)
	if err != nil {
		return 0, 0, &stockProtectionReadError{err}
	}
	return owned, balance, nil
}

// stockProtectOrEmergency places protection; when the stop already sits at or
// above the price the position is sold immediately instead.
func (at *AutoTrader) stockProtectOrEmergency(ctx context.Context, h *stockHolding, owned, stop, tp, price float64, now time.Time, logf func(string, ...interface{})) {
	err := at.stockPlaceProtection(ctx, h.Symbol, owned, stop, tp, price)
	switch {
	case err == nil:
	case errors.Is(err, errStopNotBelowPrice):
		logf("stop %.4f is not below price %.4f for %s — selling the owned position now", stop, price, h.Symbol)
		hh := *h
		hh.Owned, hh.Balance = owned, math.Max(h.Balance, owned)
		if _, e := at.stockSellLive(ctx, &hh, owned, true, "stop_fallback", now); e != nil {
			logf("emergency sell of %s FAILED: %v", h.Symbol, e)
			stockNotify("ALERT", at.name, fmt.Sprintf("<b>🚨 美股止损兜底卖出失败 %s</b>\n<i>%s</i>", notify.Escape(h.Symbol), notify.Escape(err.Error())))
		}
	default:
		logf("PROTECTION FAILED for %s: %v (the protection tick will retry)", h.Symbol, err)
		if stockStopBelowBand(err) {
			at.stockNotifyBandHeld(h.Symbol, stop, err)
		} else {
			stockNotify("ALERT", at.name, fmt.Sprintf("<b>⚠️ 美股保护单挂单失败 %s</b>\n<i>%s；每分钟保护检查会重试</i>", notify.Escape(h.Symbol), notify.Escape(err.Error())))
		}
	}
}

// stockSellLive sells qty of the program-owned position at market with an
// explicit quantity. full closes the position record. Protection is cancelled
// first (the stop locks the inventory) and re-placed for any remainder.
func (at *AutoTrader) stockSellLive(ctx context.Context, h *stockHolding, qty float64, full bool, reason string, now time.Time) (*stockOutcome, error) {
	sym := h.Symbol
	info := stockSymbolInfo(ctx, sym)
	stop, tp := at.persistedStop(sym, h)
	if h.Stop > 0 {
		stop, tp = h.Stop, h.TakeProfit
	}
	// A full exit must also retire a resting limit entry: otherwise its unfilled
	// remainder would rebuy the position the moment price touches the limit.
	// Fills that happened before the cancel are booked first and sold with it.
	pendingBooked := false
	if full {
		var cerr error
		if pendingBooked, cerr = at.stockCancelPendingEntry(ctx, sym, now); cerr != nil {
			return nil, fmt.Errorf("cannot cancel the resting entry before closing %s: %w", sym, cerr)
		}
	}
	check, err := at.stockPrepareProtectionChange(sym, now)
	if err != nil {
		return nil, fmt.Errorf("cannot cancel protection before selling %s: %w", sym, err)
	}
	owned, balance, err := at.stockLiveQuantities(sym)
	if err != nil {
		return nil, err
	}
	if full && pendingBooked {
		qty = owned // the entry's fills were added after the caller sized the exit
	}
	if owned <= 1e-9 {
		return &stockOutcome{Closed: true, Note: "position exited through exchange protection"}, nil
	}
	if reason == "stop_fallback" && check.Filled {
		return nil, fmt.Errorf("protection filled during reconciliation; fallback deferred for %s", sym)
	}
	qty = at.stockFormatQty(sym, math.Min(qty, math.Min(owned, balance)), info)
	if qty <= 0 {
		return nil, fmt.Errorf("nothing sellable for %s (owned %.6f, balance %.6f)", sym, owned, balance)
	}
	res, err := at.stockTrader.SellMarket(sym, qty)
	if err != nil || res == nil || res.ExecutedQty <= 0 {
		if err == nil {
			err = fmt.Errorf("sell not filled (status %s)", func() string {
				if res != nil {
					return res.Status
				}
				return "?"
			}())
		}
		if stop > 0 {
			_ = at.stockPlaceProtection(ctx, sym, owned, stop, tp, 0)
		}
		return nil, err
	}
	price := fillPrice(res, h.Mark)
	fee, _ := stockFee(res, price, info.BaseAsset)
	remaining := owned - res.ExecutedQty
	fullNow := remaining <= 1e-9
	pnl := at.stockRecordSell(sym, res.ExecutedQty, price, fee, res.OrderID, reason, fullNow, now)
	out := &stockOutcome{Qty: res.ExecutedQty, Price: price, Fee: fee, PnL: pnl, OrderID: res.OrderID, Closed: fullNow}
	if !fullNow && stop > 0 {
		if err := at.stockPlaceProtection(ctx, sym, remaining, stop, tp, price); err != nil {
			out.Note = fmt.Sprintf("remainder protection failed: %v", err)
			stockNotify("ALERT", at.name, fmt.Sprintf("<b>⚠️ 减仓后保护单挂单失败 %s</b>\n<i>%s</i>", notify.Escape(sym), notify.Escape(err.Error())))
		}
	}
	return out, nil
}

// stockBuyFilled books a (possibly incremental) buy fill and protects the
// total owned quantity. h is the holding before the fill (nil for a new one).
//
// pend, when non-nil, is the resting entry's state AFTER this fill (watermark
// advanced). It is committed in the same transaction as the position increment,
// and the caller adopts it in memory only once this returns without error.
func (at *AutoTrader) stockBuyFilled(ctx context.Context, sym string, executed, avg, feeUSDT, baseFee float64, orderID string,
	stop, tp float64, h *stockHolding, now time.Time, logf func(string, ...interface{}), pend *stockPending) (*stockOutcome, error) {
	net := executed - baseFee
	owned, booked, err := at.stockRecordBuyAtomic(sym, net, avg, feeUSDT, orderID, stop, now, pend, true)
	if err != nil {
		return nil, fmt.Errorf("fill recorded on the exchange but the position row failed: %w", err)
	}
	if !booked {
		return &stockOutcome{Qty: 0, Price: avg, OrderID: orderID, Note: "fill already booked"}, nil
	}
	newStop, newTP := stop, tp
	if h != nil {
		if h.Stop > newStop {
			newStop = h.Stop // never loosen an existing stop on an add
		}
		if newTP <= 0 {
			newTP = h.TakeProfit
		}
	}
	price := avg
	if p, e := at.stockTrader.GetMarketPrice(sym); e == nil && p > 0 {
		price = p
	}
	hh := &stockHolding{Symbol: sym, Owned: owned, Balance: owned, Mark: price, Stop: newStop, TakeProfit: newTP}
	at.stockProtectOrEmergency(ctx, hh, owned, newStop, newTP, price, now, logf)
	return &stockOutcome{Qty: net, Price: avg, Fee: feeUSDT, OrderID: orderID}, nil
}

// stockOpenLive places the entry order for a sized open/add.
func (at *AutoTrader) stockOpenLive(ctx context.Context, d stockengine.Decision, qty, entry float64, h *stockHolding, info usstock.SymbolInfo,
	now time.Time, logf func(string, ...interface{})) (*stockOutcome, error) {
	stop := floorStep(d.StopLoss, info.TickSize)

	var res *types.SpotOrderResult
	var err error
	if d.EntryType == stockengine.EntryLimit {
		res, err = at.stockTrader.BuyLimit(d.Symbol, qty, entry)
	} else {
		res, err = at.stockTrader.BuyMarketNotional(d.Symbol, math.Floor(qty*entry*100)/100)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.OrderID == "" {
		return nil, fmt.Errorf("order response without order id")
	}
	if res.ExecutedQty <= 0 {
		if d.EntryType != stockengine.EntryLimit {
			return nil, fmt.Errorf("market buy not filled (status %s)", res.Status)
		}
		p := &stockPending{Symbol: d.Symbol, OrderID: res.OrderID, Action: d.Action, Qty: qty, Limit: entry, Stop: stop,
			TakeProfit: d.TakeProfit, PlacedAt: now}
		at.stock.pendings[d.Symbol] = p
		at.savePending(p)
		return &stockOutcome{Pending: true, OrderID: res.OrderID, Price: entry, Note: "limit entry resting; filled quantity will be booked and protected when it fills"}, nil
	}
	price := fillPrice(res, entry)
	fee, baseFee := stockFee(res, price, info.BaseAsset)
	// A partially filled limit keeps resting: its row (with the fill watermark)
	// is committed together with the position increment.
	var p *stockPending
	if d.EntryType == stockengine.EntryLimit && res.ExecutedQty < qty-math.Max(info.StepSize, 1e-9) {
		quote := res.QuoteQty
		if quote <= 0 {
			quote = price * res.ExecutedQty
		}
		p = &stockPending{Symbol: d.Symbol, OrderID: res.OrderID, Action: d.Action, Qty: qty, FilledQty: res.ExecutedQty,
			FilledQuote: quote, FilledFee: fee, Limit: entry, Stop: stop, TakeProfit: d.TakeProfit, PlacedAt: now}
	}
	out, err := at.stockBuyFilled(ctx, d.Symbol, res.ExecutedQty, price, fee, baseFee, res.OrderID, stop, d.TakeProfit, h, now, logf, p)
	if err != nil {
		return nil, err
	}
	if p != nil {
		at.stock.pendings[d.Symbol] = p
		out.Note = "limit entry partially filled; the remainder keeps resting"
	}
	return out, nil
}

// ------------------------------------------------------------ paper helpers

func (at *AutoTrader) stockOpenPaper(d stockengine.Decision, qty, price float64, h *stockHolding, info usstock.SymbolInfo, now time.Time) (*stockOutcome, error) {
	if d.EntryType == stockengine.EntryLimit && price > d.LimitPrice {
		return nil, fmt.Errorf("limit not filled: price %.4f above limit %.4f", price, d.LimitPrice)
	}
	stop := floorStep(d.StopLoss, info.TickSize)
	tp := d.TakeProfit
	if h != nil {
		if h.Stop > stop {
			stop = h.Stop
		}
		if tp <= 0 {
			tp = h.TakeProfit
		}
	}
	if _, err := at.store.StockPaper().OpenOrAdd(at.id, d.Symbol, qty, price, stop, tp, now); err != nil {
		return nil, err
	}
	return &stockOutcome{Qty: qty, Price: price}, nil
}

// stockSellPaper books a paper sell of qty (0 = everything) at price.
func (at *AutoTrader) stockSellPaper(sym string, qty, price float64, reason string, now time.Time) (*stockOutcome, error) {
	pnl, closed, err := at.store.StockPaper().Reduce(at.id, sym, qty, price, now, reason)
	if err != nil {
		return nil, err
	}
	return &stockOutcome{Qty: qty, Price: price, PnL: pnl, Closed: closed}, nil
}

// ------------------------------------------------------------- notifications

func (at *AutoTrader) stockNotifyTrade(paper bool, title, sym string, out *stockOutcome, extra string) {
	label := ""
	if paper {
		label = "[PAPER 模拟] "
	}
	body := fmt.Sprintf("%.6g @ %.4g", out.Qty, out.Price)
	if out.PnL != 0 {
		body += fmt.Sprintf("，盈亏 %.2f USDT", out.PnL)
	}
	if extra != "" {
		body += "，" + extra
	}
	stockNotify("ORDER", at.name, fmt.Sprintf("<b>%s🇺🇸 %s %s</b>\n<i>%s</i>", label, title, notify.Escape(sym), notify.Escape(body)))
}

func joinCodes(c []string) string { return strings.Join(c, ",") }

// The optional executor capability keeps the shared trader interface unchanged.
// Missing raw inventory is an error: a filtered display is not safe evidence
// for shrinking ownership or replacing a position's protection.
func (at *AutoTrader) stockRawBalance(sym string) (float64, error) {
	reader, ok := at.stockTrader.(interface{ RawStockBalance(string) (float64, error) })
	if !ok {
		return 0, fmt.Errorf("raw inventory unavailable for %s", sym)
	}
	balance, err := reader.RawStockBalance(sym)
	if err == nil && (math.IsNaN(balance) || math.IsInf(balance, 0) || balance < 0) {
		err = fmt.Errorf("invalid raw inventory for %s", sym)
	}
	return balance, err
}

func (at *AutoTrader) stockSellBand(sym string) (types.SellBand, bool, error) {
	reader, ok := at.stockTrader.(interface {
		SellPriceBand(string) (types.SellBand, error)
	})
	if !ok {
		return types.SellBand{}, false, nil
	}
	band, err := reader.SellPriceBand(sym)
	return band, true, err
}

func stockStopBelowBand(err error) bool {
	return err != nil && (errors.Is(err, types.ErrStopBelowSellBand) || strings.Contains(err.Error(), "PERCENT_PRICE"))
}

func (at *AutoTrader) stockCheckEntryManager(sym string) error {
	// Refuse before any order is sent when another trader of this account
	// already manages the symbol: their protection and ours would contend for
	// the same shared inventory.
	others, oerr := at.stockOtherManagers(sym)
	if oerr != nil {
		return fmt.Errorf("cannot verify that no other trader manages %s: %w", sym, oerr)
	}
	if len(others) > 0 {
		msg := fmt.Sprintf("%s 已由同账户的其他美股交易员(%s)管理，拒绝开仓以免互相接管保护单", sym, strings.Join(others, ","))
		logger.Warnf("⚠️ [%s] %s", at.name, msg)
		stockNotify("ALERT", at.name, fmt.Sprintf("<b>⚠️ 美股开仓被拒绝</b>\n<i>%s</i>", notify.Escape(msg)))
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// stockEntryAlertEvery bounds repeated Telegram alerts for the same symbol and
// the same pre-order rejection reason.
const stockEntryAlertEvery = 30 * time.Minute

// stockAlertEntryRejected tells the user (Telegram ALERT, live only) that a
// live entry was abandoned by the final pre-order check — a stale bStock quote
// (older than 30 s), an unavailable quote, or a failed re-validation with the
// execution clock. Deduplicated per symbol+reason; a cancelled context is not
// an alert-worthy rejection.
func (at *AutoTrader) stockAlertEntryRejected(paper bool, d stockengine.Decision, cause error, now time.Time) {
	if paper || cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return
	}
	msg := cause.Error()
	code := msg
	if i := strings.IndexAny(msg, ": "); i > 0 {
		code = msg[:i]
	}
	key := d.Symbol + "|" + code
	if code == "ENTRY_RECHECK" {
		if f := strings.Fields(msg); len(f) > 1 {
			key += "|" + f[1] // the validation codes, so a new reason is not suppressed
		}
	}
	if last, ok := at.stock.entryAlertAt[key]; ok && now.Sub(last) < stockEntryAlertEvery {
		return
	}
	if at.stock.entryAlertAt == nil {
		at.stock.entryAlertAt = map[string]time.Time{}
	}
	at.stock.entryAlertAt[key] = now
	reason := "下单前复核未通过（时段、报价新鲜度、背离或止损带）"
	switch code {
	case "BSTOCK_PRICE_STALE":
		reason = "bStock 报价超过 30 秒未更新，不能据此下单"
	case "ENTRY_QUOTE_UNAVAILABLE":
		reason = "下单前无法刷新 bStock 报价"
	case "DATA_INSUFFICIENT":
		reason = "缺少下单所需的行情数据"
	case "STOP_BELOW_SELL_BAND":
		reason = "止损距入场价超过交易所卖出价格带（PERCENT_PRICE），交易所止损单无法挂出"
	}
	logger.Warnf("⚠️ [%s] 美股入场被下单前复核放弃 %s %s: %s", at.name, d.Action, d.Symbol, msg)
	stockNotify("ALERT", at.name, fmt.Sprintf("<b>⚠️ 美股入场已放弃（下单前复核未通过）</b>\n%s %s\n<i>%s</i>\n<code>%s</code>",
		notify.Escape(d.Action), notify.Escape(d.Symbol), notify.Escape(reason), notify.Escape(msg)))
}

// stockEntryContext is the last network read before an entry is submitted.
// It bypasses both quote caches, then validates with the execution clock and
// sizes from that same final bStock price. Existing position exits do not use it.
func (at *AutoTrader) stockEntryContext(ctx context.Context, sizing *stockengine.Context, d stockengine.Decision,
	snap *stockengine.SymbolSnapshot) (*stockengine.SymbolSnapshot, *stockengine.Context, error) {
	if sizing == nil || snap == nil {
		return nil, nil, fmt.Errorf("DATA_INSUFFICIENT: no entry context")
	}
	quote, err := stockGetQuote(usstock.WithForceRefresh(ctx), d.Symbol)
	if err != nil {
		return nil, nil, fmt.Errorf("ENTRY_QUOTE_UNAVAILABLE: %w", err)
	}
	now := at.stockClock()
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if quote == nil {
		return nil, nil, fmt.Errorf("ENTRY_QUOTE_UNAVAILABLE: no quote")
	}
	q := usstock.RecheckQuote(quote, now)
	age := now.Sub(q.BStockTime)
	if q.BStockTime.IsZero() || age < 0 || age > 30*time.Second || q.BStockPrice <= 0 || math.IsNaN(q.BStockPrice) || math.IsInf(q.BStockPrice, 0) {
		return nil, nil, fmt.Errorf("BSTOCK_PRICE_STALE: execution quote age %s", age)
	}
	current := *sizing
	current.Now, current.Session = now, usstock.SessionAt(now)
	fresh := *snap
	fresh.Price, fresh.Quote = q.BStockPrice, &q
	current.Snapshots = append([]*stockengine.SymbolSnapshot(nil), sizing.Snapshots...)
	replaced := false
	for i, s := range current.Snapshots {
		if s != nil && s.Symbol == d.Symbol {
			current.Snapshots[i] = &fresh
			replaced = true
		}
	}
	if !replaced {
		current.Snapshots = append(current.Snapshots, &fresh)
	}
	v := stockengine.Validate(&current, []stockengine.Decision{d})[0]
	if !v.Accepted {
		return nil, nil, fmt.Errorf("ENTRY_RECHECK: %s %s", joinCodes(v.Codes), v.Note)
	}
	return &fresh, &current, nil
}
