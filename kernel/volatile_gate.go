package kernel

import (
	"fmt"
	"math"
	"nofx/logger"
	"nofx/market"
	"sort"
	"sync"
	"time"
)

func validPositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

// rollingRangeSpike includes the forming tail. Each historical window must
// end BEFORE the current window starts: none of its bars can contaminate its
// own baseline. For N baseline samples and w bars/window we need N+2w-1 bars.
// The market fetch floor is 200 (not 300); on 5m use up to 288 samples, but
// never fewer than 144 (12h). Thus 200 bars give 177 baseline samples for 60m;
// the full 288-sample 60m baseline needs 311 bars, not merely 288+12.
func rollingRangeSpike(bars []market.KlineBar, period time.Duration, window, baseline, minimum int, now time.Time) *float64 {
	count := min(baseline, len(bars)-2*window+1)
	if count < minimum {
		return nil
	}
	bars = bars[len(bars)-(count+2*window-1):]
	age := now.Sub(time.UnixMilli(bars[len(bars)-1].Time))
	if age < 0 || age > 2*period {
		return nil
	}
	for i, b := range bars {
		if b.Time <= 0 || !validPositive(b.Open) || !validPositive(b.High) || !validPositive(b.Low) || b.High < b.Low {
			return nil
		}
		if i > 0 && b.Time-bars[i-1].Time != period.Milliseconds() {
			return nil
		}
	}
	rangeAt := func(end int) float64 {
		start := end - window + 1
		high, low := bars[start].High, bars[start].Low
		for i := start + 1; i <= end; i++ {
			high = math.Max(high, bars[i].High)
			low = math.Min(low, bars[i].Low)
		}
		return (high - low) / bars[start].Open * 100
	}
	ranges := make([]float64, count)
	for i := range ranges {
		ranges[i] = rangeAt(window - 1 + i)
		if ranges[i] < 0 || math.IsNaN(ranges[i]) || math.IsInf(ranges[i], 0) {
			return nil
		}
	}
	sort.Float64s(ranges)
	median := ranges[count/2]
	if count%2 == 0 {
		median = ranges[count/2-1]/2 + median/2
	}
	if !validPositive(median) {
		return nil
	}
	ratio := rangeAt(len(bars)-1) / median
	if ratio < 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return nil
	}
	return &ratio
}

// Missing/too-short sources fall back 5m -> 15m -> legacy 1h. A present
// source with gaps/stale/invalid bars fails open; do not mask corruption by
// silently substituting a coarser series. On 15m use 96 (minimum 48) samples
// to preserve the same 24h (minimum 12h) baseline horizon; 5m is unavailable.
func volatilityRangeSpikes(data *market.Data, now time.Time) (five, fifteen, hour *float64) {
	if data == nil {
		return
	}
	if tf := data.TimeframeData["5m"]; tf != nil && len(tf.Klines) >= 145 {
		return rollingRangeSpike(tf.Klines, 5*time.Minute, 1, 288, 144, now),
			rollingRangeSpike(tf.Klines, 5*time.Minute, 3, 288, 144, now),
			rollingRangeSpike(tf.Klines, 5*time.Minute, 12, 288, 144, now)
	}
	logger.Infof("[RISK CONTROL] %s 5m range history missing/short — trying 15m fallback", data.Symbol)
	if tf := data.TimeframeData["15m"]; tf != nil && len(tf.Klines) >= 49 {
		return nil, rollingRangeSpike(tf.Klines, 15*time.Minute, 1, 96, 48, now),
			rollingRangeSpike(tf.Klines, 15*time.Minute, 4, 96, 48, now)
	}
	logger.Infof("[RISK CONTROL] %s 15m range history missing/short — trying legacy 1h fallback", data.Symbol)
	return nil, nil, hourlyRangeSpike(data, now)
}

// Last-resort compatibility path: a forming hour vs the preceding 24 hours.
// It retains the old hour-boundary limitation when short history is absent.
func hourlyRangeSpike(data *market.Data, now time.Time) *float64 {
	if data == nil || data.TimeframeData["1h"] == nil {
		return nil
	}
	return rollingRangeSpike(data.TimeframeData["1h"].Klines, time.Hour, 1, 24, 24, now)
}

// Process-local, per normalized symbol, shared by prompt and pending rechecks.
// Restart loses the hold-off history, which is acceptable; no persisted bans.
// Callers inject the evaluation clock through SignalOptions.Now; tests may
// replace this state with a fresh instance before starting concurrent calls.
type volatileHoldoffState struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var volatileHoldoffs = volatileHoldoffState{last: make(map[string]time.Time)}

func (s *volatileHoldoffState) active(symbol string, now time.Time, minutes float64, triggered bool) bool {
	if !validPositive(minutes) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = make(map[string]time.Time)
	}
	if triggered && (s.last[symbol].IsZero() || now.After(s.last[symbol])) {
		s.last[symbol] = now
	}
	at, ok := s.last[symbol]
	if !ok {
		return false
	}
	age := now.Sub(at)
	if age < 0 { // a historical/replayed evaluation must not inherit a future ban
		return false
	}
	if age.Minutes() >= minutes {
		delete(s.last, symbol)
		return false
	}
	return true
}

func volatileExempt(symbol string) bool {
	symbol = market.Normalize(symbol)
	return market.IsBStockSymbol(symbol) || symbol == "BTCUSDT" || symbol == "ETHUSDT"
}

// Absolute entry bans apply to BOTH directions and order styles. A resting
// anchor cannot cure thin turnover, a manipulation event or its hold-off.
func volatileEntryCodes(symbol string, opt SignalOptions) []string {
	symbol = market.Normalize(symbol)
	if volatileExempt(symbol) {
		return nil
	}
	var codes []string
	if opt.MinQuoteVolume24hMillions > 0 {
		if !validPositive(opt.QuoteVolume24hUsd) {
			logger.Infof("[RISK CONTROL] %s 24h quote volume unknown — turnover gate fail-open", symbol)
		} else if v := opt.QuoteVolume24hUsd / 1e6; v < opt.MinQuoteVolume24hMillions {
			codes = append(codes, fmt.Sprintf("THIN_VOLUME_%.2f_LT_%.2f", v, opt.MinQuoteVolume24hMillions))
		}
	}
	triggered := false
	for _, w := range []struct {
		name  string
		ratio *float64
		mult  float64
	}{
		{"5M", opt.VolSpike5mRatio, opt.VolSpike5mMult},
		{"15M", opt.VolSpike15mRatio, opt.VolSpike15mMult},
		{"1H", opt.VolSpike1hRatio, opt.VolSpikeMult},
	} {
		if w.mult <= 0 {
			continue
		}
		if w.ratio == nil || *w.ratio < 0 || math.IsNaN(*w.ratio) || math.IsInf(*w.ratio, 0) {
			logger.Infof("[RISK CONTROL] %s %s range history unknown — volatility spike gate fail-open", symbol, w.name)
		} else if v := *w.ratio; v >= w.mult {
			codes = append(codes, fmt.Sprintf("VOL_SPIKE_%s_%.2f_GT_%.2f", w.name, v, w.mult))
			triggered = true
		}
	}
	for _, w := range []struct {
		name string
		pct  *float64
		max  float64
	}{
		{"15M", opt.OI15mPct, opt.OISurge15mPct},
		{"1H", opt.OI1hPct, opt.OISurge1hPct},
	} {
		if w.max <= 0 {
			continue
		}
		if w.pct == nil || math.IsNaN(*w.pct) || math.IsInf(*w.pct, 0) {
			logger.Infof("[RISK CONTROL] %s %s OI history unknown — OI surge gate fail-open", symbol, w.name)
		} else if v := math.Abs(*w.pct); v > w.max {
			codes = append(codes, fmt.Sprintf("OI_SURGE_%s_%.2f_GT_%.2f", w.name, v, w.max))
			triggered = true
		}
	}
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	if volatileHoldoffs.active(symbol, now, opt.VolSpikeHoldoffMinutes, triggered) && !triggered {
		codes = append(codes, fmt.Sprintf("VOL_HOLDOFF_%gm", opt.VolSpikeHoldoffMinutes))
	}
	return codes
}
