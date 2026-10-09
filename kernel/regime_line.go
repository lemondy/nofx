package kernel

import (
	"fmt"
	"math"
	"time"

	"nofx/market"
	"nofx/store"
)

// ResolveRoleTimeframes mirrors the strategy's role mapping. Only execution
// is configurable today; trend and regime remain 1h and 4h.
// 2026-10-09 regime line → trend TF (user option B).
func ResolveRoleTimeframes(config *store.StrategyConfig) RoleTimeframes {
	roles := RoleTimeframes{TrendTF: "1h", RegimeTF: "4h"}
	if config != nil {
		roles.ExecutionTF = config.Indicators.Klines.PrimaryTimeframe
	}
	return roles
}

// RegimeLine reads the converged EMA50 at the last closed trend-TF bar.
// Values are tail-aligned: early raw bars have no EMA50 value.
func RegimeLine(data *market.Data, tf string, now time.Time) float64 {
	if data == nil || market.TimeframeDuration(tf) <= 0 {
		return 0
	}
	td := data.TimeframeData[tf]
	if td == nil || len(td.Klines) == 0 || len(td.EMA50Values) == 0 {
		return 0
	}
	j := len(ClosedKlines(td, now, market.TimeframeDuration(tf))) - 1
	i := j - (len(td.Klines) - len(td.EMA50Values))
	if j < 0 || i < 0 || i >= len(td.EMA50Values) {
		return 0
	}
	return td.EMA50Values[i]
}

// RegimeLineBreached compares the entry site with the trend-TF EMA50.
func RegimeLineBreached(isLong bool, px, line float64) bool {
	if px <= 0 || line <= 0 {
		return false
	}
	return (isLong && px < line) || (!isLong && px > line)
}

// RegimeLineApplies is the shared dip/bounce trigger.
func RegimeLineApplies(isLong bool, trend string) bool {
	return (isLong && trend == "pullback") || (!isLong && trend == "rally")
}

// ExecutionMicroTrend uses the kernel's definition: valid 15m, else 30m,
// including the EMA-gap noise rule. Finer bars and 1h are not micro timing.
// 2026-10-09 regime line → trend TF (user option B).
func ExecutionMicroTrend(data *market.Data, now time.Time) (string, string) {
	if data == nil || data.CurrentPrice <= 0 {
		return "", ""
	}
	tfs := make(map[string]*TFSignal)
	for _, tf := range []string{"15m", "30m"} {
		if td := data.TimeframeData[tf]; td != nil && len(td.Klines) >= 10 {
			tfs[tf], _ = computeTFSignal(tf, td, now, data.CurrentPrice)
		}
	}
	if ef := executionFilterFromTimeframes(tfs, data.CurrentPrice); ef != nil {
		return ef.MicroTF, ef.MicroTrend
	}
	return "", ""
}

func executionFilterFromTimeframes(tfs map[string]*TFSignal, price float64) *ExecutionFilter {
	var tf string
	if t := tfs["15m"]; t != nil && t.Trend != "" && t.Trend != "unknown" {
		tf = "15m"
	} else if tfs["30m"] != nil {
		tf = "30m"
	} else {
		return nil
	}
	t := tfs[tf]
	ef := &ExecutionFilter{MicroTF: tf, MicroTrend: t.Trend}
	ef.LongAllowed = t.Trend == "up" || t.Trend == "pullback"
	ef.ShortAllowed = t.Trend == "down" || t.Trend == "rally"
	// 09-19 audit 六-②: a gap inside 0.25×ATR is directionless noise.
	if t.EMAFast != nil && t.EMASlow != nil && t.ATRPct > 0 && price > 0 {
		gapPct := math.Abs(*t.EMAFast-*t.EMASlow) / price * 100
		if gapPct < 0.25*t.ATRPct {
			ef.LongAllowed, ef.ShortAllowed = false, false
			ef.MicroTrend = "range"
			ef.Reason = fmt.Sprintf("EMA gap %.2f%% < 0.25×ATR %.2f%% — micro direction is noise", gapPct, t.ATRPct)
		}
	}
	if ef.LongAllowed && !ef.ShortAllowed {
		ef.Reason = "micro trend aligned for longs only"
	} else if ef.ShortAllowed && !ef.LongAllowed {
		ef.Reason = "micro trend aligned for shorts only"
	} else if ef.Reason == "" {
		ef.Reason = "micro trend is " + ef.MicroTrend + " — no directional alignment"
	}
	return ef
}

type RegimeLineSignal struct {
	TF    string  `json:"tf"`
	EMA50 float64 `json:"ema50"`
	// Precomputed comparisons (2026-10-09 ONUSDT: the model read a close
	// 0.07% BELOW the line — 0.1107 vs 0.110776 — as "升破" and closed a
	// short on it). CloseAbove compares the last CLOSED trend-TF bar's
	// close; LiveAbove compares the live ticker price. Cite these instead
	// of re-comparing the decimals yourself.
	LiveAbove  bool `json:"live_price_above"`
	CloseAbove bool `json:"last_close_above"`
}

func suppressAnchorsAgainstRegimeLine(sig *SymbolSignal, enabled bool) {
	if !enabled || sig.RegimeLine == nil || sig.ExecutionFilter == nil {
		return
	}
	for _, isLong := range []bool{true, false} {
		if !RegimeLineApplies(isLong, sig.ExecutionFilter.MicroTrend) ||
			RegimeLineBreached(isLong, sig.Price, sig.RegimeLine.EMA50) {
			continue
		}
		anchor, field := &sig.LimitSellPrice, "limit_sell_price"
		if isLong {
			anchor, field = &sig.LimitBuyPrice, "limit_buy_price"
		}
		if RegimeLineBreached(isLong, *anchor, sig.RegimeLine.EMA50) {
			*anchor = 0
			sig.Warnings = append(sig.Warnings, fmt.Sprintf("%s suppressed: anchor crossed %s regime line (EMA50) %.6g — possible reversal, limit would be rejected", field, sig.RegimeLine.TF, sig.RegimeLine.EMA50))
		}
	}
}
