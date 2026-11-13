package trader

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/market/bnweight"
	"nofx/store"
)

// entryBandTheta uses only closed bars in the prior day. Neither returns nor
// lag-one pairs may bridge a missing minute. Input order is immaterial.
func entryBandTheta(bars []market.Kline, entryMinuteStart time.Time, phi float64) (theta, sg float64, reason string) {
	end := entryMinuteStart.Truncate(time.Minute).UnixMilli()
	start := end - 1440*60000
	closes := make(map[int64]float64)
	for _, b := range bars {
		if b.OpenTime >= start && b.OpenTime < end && b.Close > 0 && !math.IsNaN(b.Close) && !math.IsInf(b.Close, 0) {
			closes[b.OpenTime] = b.Close
		}
	}
	times := make([]int64, 0, len(closes))
	for ts := range closes {
		times = append(times, ts)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	returns := make(map[int64]float64)
	mean := 0.0
	for _, ts := range times {
		if prev, ok := closes[ts-60000]; ok {
			r := math.Log(closes[ts] / prev)
			returns[ts] = r
			mean += r
		}
	}
	if len(returns) == 0 {
		return phi, 0, "theta_fallback_phi"
	}
	mean /= float64(len(returns))
	cov, pairs := 0.0, 0
	for _, ts := range times {
		r, ok := returns[ts]
		if !ok {
			continue
		}
		if prev, ok := returns[ts-60000]; ok {
			cov += (prev - mean) * (r - mean)
			pairs++
		}
	}
	if pairs == 0 {
		return phi, 0, "theta_fallback_phi"
	}
	cov /= float64(pairs)
	sg = math.Sqrt(math.Max(0, -cov))
	return .5 * (phi + math.Sqrt(phi*phi+4*sg*sg)), sg, ""
}

func (at *AutoTrader) entryBandEnabled() bool {
	cfg := at.config.StrategyConfig
	return cfg != nil && cfg.RiskControl.EntryBandEnabled && !cfg.RiskControl.LimitEntryEnabled && (cfg.StrategyType == "" || cfg.StrategyType == "ai_trading")
}

func (at *AutoTrader) bandTheta(symbol string, placed time.Time) (float64, float64, string) {
	rc := at.config.StrategyConfig.RiskControl
	if rc.EntryBandMode == "fixed" {
		return rc.EntryBandFixedBps / 10000, 0, ""
	}
	phi := rc.EntryBandPhiBps / 10000
	if phi <= 0 {
		phi = .0008
	}
	end := placed.Truncate(time.Minute)
	start := end.Add(-1440 * time.Minute)
	var bars []market.Kline
	var err error
	if at.entryBandKlinesFn != nil {
		bars, err = at.entryBandKlinesFn(symbol, start, end)
	} else {
		bars, err = market.GetKlinesRangePriority(symbol, "1m", start, end.Add(-time.Millisecond), bnweight.Normal)
	}
	if err != nil {
		logger.Warnf("[%s] %s theta_fallback_phi: %v", at.name, symbol, err)
		return phi, 0, "theta_fallback_phi"
	}
	return entryBandTheta(bars, end, phi)
}

// Use the adapter's real tick (including non-decimal ticks). Decimal precision
// remains available for adapters that expose only that existing contract.
func (at *AutoTrader) bandLimitPrice(symbol, side string, raw float64) (float64, error) {
	tick := 0.0
	if rules, ok := at.trader.(interface {
		EntryBandPriceTick(string, float64) (float64, error)
	}); ok {
		v, err := rules.EntryBandPriceTick(symbol, raw)
		if err != nil {
			return 0, err
		}
		tick = v
	} else if rules, ok := at.trader.(interface{ GetSymbolTickSize(string) (float64, error) }); ok {
		v, err := rules.GetSymbolTickSize(symbol)
		if err != nil {
			return 0, err
		}
		tick = v
	} else if rules, ok := at.trader.(interface{ GetSymbolPricePrecision(string) (int, error) }); ok {
		precision, err := rules.GetSymbolPricePrecision(symbol)
		if err != nil {
			return 0, err
		}
		tick = math.Pow10(-precision)
	}
	if tick <= 0 || math.IsNaN(tick) || math.IsInf(tick, 0) {
		return 0, fmt.Errorf("entry band: %s price tick unavailable", symbol)
	}
	steps := math.Floor(raw / tick)
	if side == "short" {
		steps = math.Ceil(raw / tick)
	}
	price := steps * tick
	// Strictly preserve the offset even under floating-point multiplication.
	if side == "long" && price > raw {
		price -= tick
	}
	if side == "short" && price < raw {
		price += tick
	}
	if math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, fmt.Errorf("entry band: invalid limit price")
	}
	return price, nil
}

func (at *AutoTrader) tryEntryBand(d *kernel.Decision, record *store.DecisionAction, side string, ref, qty float64, fallback bool) (bool, error) {
	if fallback || !at.entryBandEnabled() {
		return false, nil
	}
	if _, ok := at.trader.(limitEntryExchange); !ok {
		return false, nil
	}
	placed := time.Now()
	theta, sg, reason := at.bandTheta(d.Symbol, placed)
	raw := ref * (1 - theta)
	if side == "short" {
		raw = ref * (1 + theta)
	}
	timeout := at.config.StrategyConfig.RiskControl.EntryBandTimeoutMin
	if timeout <= 0 {
		timeout = 30
	}
	price, err := at.bandLimitPrice(d.Symbol, side, raw)
	if err != nil {
		// Keep the trade: an unknown tick only disables the band for this
		// entry. The S2 study keeps every entry (no "fewer trades" effect).
		skipped := &pendingEntry{Mode: "band", RefPrice: ref, ThetaBps: theta * 10000, SG: sg, ThetaReason: reason,
			Symbol: d.Symbol, Side: side, Price: raw, Quantity: qty, PlacedAt: placed}
		at.recordBandOutcome(skipped, "tick_unavailable", nil, "", err)
		return false, nil
	}
	plan := &pendingEntry{Mode: "band", State: "pending", RefPrice: ref, ThetaBps: theta * 10000, SG: sg, ThetaReason: reason,
		Symbol: d.Symbol, Side: side, Price: price, Quantity: qty, StopLoss: d.StopLoss, TakeProfit: d.TakeProfit, Leverage: d.Leverage,
		ExitMode: d.ExitMode, OriginalR: math.Abs(price - d.StopLoss), PlacedAt: placed, Deadline: placed.Add(time.Duration(timeout) * time.Minute)}
	if side == "long" && price <= d.StopLoss || side == "short" && price >= d.StopLoss {
		at.recordBandOutcome(plan, "stop_guard", nil, "", nil)
		return false, nil
	}
	limitDecision := *d
	limitDecision.Price = price
	record.Price, record.Quantity, record.EntryMode = price, qty, "band"
	err = at.placePendingLimit(&limitDecision, record, side, qty, plan)
	if err == nil {
		// The durable pending order now accounts for this risk. Retaining
		// the market path's cycle reservation would count the entry twice.
		at.setCycleReservation(d, 0)
	}
	if id, e := strconv.ParseInt(record.EntryOrderID, 10, 64); e == nil {
		record.OrderID = id
	}
	return true, err
}

// Best effort attribution: a failed event write cannot fail or delay an order
// through retries. No additional market-data request is made for extremes.
func (at *AutoTrader) recordBandOutcome(pe *pendingEntry, outcome string, status map[string]interface{}, fallbackID string, cause error) {
	if pe.Mode != "band" {
		return
	}
	if status == nil {
		status = pe.bandReceipt
	}
	event := &store.EntryBandEvent{TraderID: at.id, Symbol: pe.Symbol, Side: pe.Side, OrderID: pe.OrderID,
		PlacedAt: pe.PlacedAt, RefPrice: pe.RefPrice, LimitPrice: pe.Price, ThetaBps: pe.ThetaBps, SG: pe.SG,
		Outcome: outcome, FallbackOrderID: fallbackID, ThetaReason: pe.ThetaReason}
	if cause != nil {
		event.Error = cause.Error()
	}
	if statusFloat(status, "executedQty") > 0 {
		now := time.Now()
		ms := statusFloat(status, "updateTime")
		if v, ok := status["updateTime"].(int64); ok {
			ms = float64(v)
		}
		if ms > 0 {
			now = time.UnixMilli(int64(ms))
		}
		event.FillPrice, event.FillTime = statusFloat(status, "avgPrice"), &now
	}
	logger.Infof("[%s] entry_band %s %s %s order=%s theta_reason=%s: %s", at.name, pe.Symbol, pe.Side, outcome, pe.OrderID, event.ThetaReason, event.Error)
	if at.store != nil {
		if err := at.store.PendingEntry().RecordBandEvent(event); err != nil {
			logger.Warnf("entry_band event write failed: %v", err)
		}
	}
}

func (at *AutoTrader) dispatchBandPending(d *kernel.Decision, record *store.DecisionAction) (bool, error) {
	var side string
	switch d.Action {
	case "open_long", "close_long", "partial_close_long":
		side = "long"
	case "open_short", "close_short", "partial_close_short":
		side = "short"
	default:
		return false, nil
	}
	pe := at.getPendingEntry(d.Symbol, side)
	if pe == nil || pe.Mode != "band" {
		return false, nil
	}
	record.EntryMode = "band"
	if strings.HasPrefix(d.Action, "open_") {
		at.setCycleReservation(d, 0) // this decision placed no new order
		at.recordBandOutcome(pe, "duplicate_ignored", nil, "", nil)
		return true, nil
	}
	positions, err := at.trader.GetPositions()
	if err != nil {
		return true, fmt.Errorf("band close position check: %w", err)
	}
	for _, p := range positions {
		if p["symbol"] == d.Symbol && strings.EqualFold(fmt.Sprint(p["side"]), side) {
			return false, nil
		}
	}
	_, err = at.cancelPendingWithExecuted(pe)
	if err != nil {
		return true, err
	}
	// A racing fill is protected by cancelPendingWithExecuted; never add exposure.
	at.recordBandOutcome(pe, "ai_cancelled", nil, "", nil)
	return true, nil
}

// expireBandPending runs only for acknowledged NEW/PARTIALLY_FILLED orders.
// The final cancel receipt, rather than the earlier poll, decides if a market
// replacement is allowed. Even a tiny racing fill suppresses replacement.
func (at *AutoTrader) expireBandPending(pe *pendingEntry, partial bool) {
	executed, err := at.cancelPendingWithExecuted(pe)
	if err != nil {
		logger.Warnf("entry_band deadline cancel unconfirmed: %v", err)
		return
	}
	if partial || executed > 0 {
		outcome := "partial"
		if executed >= pe.Quantity {
			outcome = "filled"
		}
		at.recordBandOutcome(pe, outcome, nil, "", nil)
		return
	}
	md, err := at.getMarketData(pe.Symbol)
	record := &store.DecisionAction{EntryMode: "band_fallback"}
	if err == nil && (md == nil || md.CurrentPrice <= 0) {
		err = fmt.Errorf("entry band fallback price unavailable")
	}
	if err == nil {
		d := &kernel.Decision{Symbol: pe.Symbol, Action: "open_" + pe.Side, StopLoss: pe.StopLoss, TakeProfit: pe.TakeProfit,
			Leverage: pe.Leverage, ExitMode: pe.ExitMode, PositionSizeUSD: pe.Quantity * md.CurrentPrice, Reasoning: "entry_band_fallback"}
		if pe.Side == "long" {
			err = at.executeOpenLongWithRecord(d, record, true)
		} else {
			err = at.executeOpenShortWithRecord(d, record, true)
		}
		// This lifecycle call does not pass through the decision loop's
		// reservation finalizer. Mirror its failure accounting here.
		if err != nil && !at.holdUnknownSubmitReservation(err, d) {
			at.setCycleReservation(d, 0)
		}
	}
	outcome := "fallback"
	if err != nil {
		outcome = "invalidated"
	}
	var receipt map[string]interface{}
	if record.EntryOrderID != "" {
		receipt = map[string]interface{}{"executedQty": record.Quantity, "avgPrice": record.Price}
	}
	at.recordBandOutcome(pe, outcome, receipt, record.EntryOrderID, err)
}
