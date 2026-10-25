package kernel

import (
	"fmt"
	"math"
	"nofx/logger"
	"nofx/market"
	"sort"
	"time"
)

func validPositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

// Compare the latest hourly high-low range / open with the MEDIAN of the
// preceding 24 hourly ranges. Include a current forming hour: waiting for
// its close would admit trades during the manipulation. Never include it
// in its own baseline. Require contiguous, fresh, timestamped hourly bars.
func hourlyRangeSpike(data *market.Data) *float64 {
	if data == nil || data.TimeframeData["1h"] == nil {
		return nil
	}
	bars := data.TimeframeData["1h"].Klines
	if len(bars) < 25 {
		return nil
	}
	bars = bars[len(bars)-25:]
	now := data.SampledAt
	if now.IsZero() {
		now = time.Now()
	}
	age := now.Sub(time.UnixMilli(bars[24].Time))
	if age < 0 || age > 2*time.Hour {
		return nil
	}
	ranges := make([]float64, 0, 24)
	for i, b := range bars {
		if !validPositive(b.Open) || !validPositive(b.High) || !validPositive(b.Low) || b.High < b.Low {
			return nil
		}
		if i > 0 && b.Time-bars[i-1].Time != time.Hour.Milliseconds() {
			return nil
		}
		r := (b.High - b.Low) / b.Open * 100
		if math.IsNaN(r) || math.IsInf(r, 0) {
			return nil
		}
		if i == 24 {
			sort.Float64s(ranges)
			median := (ranges[11] + ranges[12]) / 2
			if !validPositive(median) {
				return nil
			}
			ratio := r / median
			return &ratio
		}
		ranges = append(ranges, r)
	}
	return nil
}

// Absolute entry bans: a resting anchor cannot cure thin turnover or a
// manipulation event, so these apply to BOTH directions and order styles.
func volatileEntryCodes(symbol string, opt SignalOptions) []string {
	if market.IsBStockSymbol(symbol) {
		return nil
	}
	var codes []string
	if opt.MinQuoteVolume24hMillions > 0 && symbol != "BTCUSDT" && symbol != "ETHUSDT" {
		if !validPositive(opt.QuoteVolume24hUsd) {
			logger.Infof("[RISK CONTROL] %s 24h quote volume unknown — turnover gate fail-open", symbol)
		} else if v := opt.QuoteVolume24hUsd / 1e6; v < opt.MinQuoteVolume24hMillions {
			codes = append(codes, fmt.Sprintf("THIN_VOLUME_%.2f_LT_%.2f", v, opt.MinQuoteVolume24hMillions))
		}
	}
	// BTC/ETH exempt: 1h range spikes of 3x the median fire ~4% of hours on
	// BTC with only ~1% follow-through (2026-10 replay), so the gate would
	// block majors for no gain. It targets thin, manipulable altcoins.
	if opt.VolSpikeMult > 0 && symbol != "BTCUSDT" && symbol != "ETHUSDT" {
		if opt.VolSpike1hRatio == nil {
			logger.Infof("[RISK CONTROL] %s hourly range history unknown — volatility spike gate fail-open", symbol)
		} else if v := *opt.VolSpike1hRatio; v > opt.VolSpikeMult {
			codes = append(codes, fmt.Sprintf("VOL_SPIKE_1H_%.2f_GT_%.2f", v, opt.VolSpikeMult))
		}
	}
	if opt.OISurge1hPct > 0 {
		if opt.OI1hPct == nil || math.IsNaN(*opt.OI1hPct) || math.IsInf(*opt.OI1hPct, 0) {
			logger.Infof("[RISK CONTROL] %s 1h OI history unknown — OI surge gate fail-open", symbol)
		} else if v := math.Abs(*opt.OI1hPct); v > opt.OISurge1hPct {
			codes = append(codes, fmt.Sprintf("OI_SURGE_%.2f_GT_%.2f", v, opt.OISurge1hPct))
		}
	}
	return codes
}
