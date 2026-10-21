package market

import (
	"fmt"
	"sort"
	"time"
)

// Time-based staleness of a kline series (review 2026-10-20 #8).
//
// isStaleData only catches a frozen price + zero volume; a vendor stuck 30
// minutes in the past with a perfectly "normal" tail (real volume, wiggling
// prices) sailed through, and the vendor-divergence gate compares prices only
// — a live ticker that happens to sit near the old close passed too. These
// checks look at the bar TIMESTAMPS against the sampling clock instead.
const (
	// MaxStaleBars: the newest bar (forming or just closed) may end at most
	// this many bar-periods before the sampling time. A healthy feed always has
	// a forming bar (age ≈ 0) or, with vendor lag, a just-closed one (≤ 1
	// period); 3 periods leaves room for a lagging cache while still catching
	// "feed stopped a few bars ago" quickly on every timeframe.
	MaxStaleBars = 3
	// MaxSeriesGapBars: a hole between two consecutive bars wider than this many
	// periods inside the recent tail means missing history (ATR/swing structure
	// would silently span it). Kept wider than MaxStaleBars because quiet
	// listings legitimately skip a few empty intervals.
	MaxSeriesGapBars = 6
	// gapScanBars: only the recent tail matters for the live signal; ancient
	// maintenance holes further back are irrelevant.
	gapScanBars = 30
)

// SeriesStaleReason checks one timeframe series against the sampling time and
// returns "" when it is fresh, else a short human-readable reason. Unknown
// timeframe labels and empty series are not judged here (missing data is
// handled by the callers' own minimum-bars checks).
func SeriesStaleReason(tf string, klines []KlineBar, sampledAt time.Time, checkGaps bool) string {
	dur := TimeframeDuration(tf)
	if dur <= 0 || len(klines) == 0 || sampledAt.IsZero() {
		return ""
	}
	last := time.UnixMilli(klines[len(klines)-1].Time)
	age := sampledAt.Sub(last.Add(dur)) // time since the newest bar's END
	if age > time.Duration(MaxStaleBars)*dur {
		return fmt.Sprintf("newest %s bar opened %s, %s behind sampling time (> %d bars)",
			tf, last.UTC().Format("01-02 15:04"), age.Round(time.Second), MaxStaleBars)
	}
	if checkGaps {
		start := len(klines) - gapScanBars
		if start < 1 {
			start = 1
		}
		limit := time.Duration(MaxSeriesGapBars) * dur
		for i := start; i < len(klines); i++ {
			gap := time.Duration(klines[i].Time-klines[i-1].Time) * time.Millisecond
			if gap > limit {
				return fmt.Sprintf("%s series has a %s gap before %s (> %d bars)",
					tf, gap.Round(time.Second), time.UnixMilli(klines[i].Time).UTC().Format("01-02 15:04"), MaxSeriesGapBars)
			}
		}
	}
	return ""
}

// StaleTimeframes evaluates every series of the snapshot at sampledAt and
// returns tf → reason for the stale ones (nil when all fresh).
//
// Non-24h markets are judged by their trading session: tokenized stocks are
// only checked while the US regular session has been open for the whole
// look-back window (and never for the gap check — overnight/holiday holes are
// normal), other tokenized equities (KR/HK/CN) and xyz-dex assets (equity/FX/
// commodity perps with their own calendars) are never judged by wall-clock
// age. A closed market therefore never reads as stale.
func StaleTimeframes(symbol string, tfs map[string]*TimeframeSeriesData, sampledAt time.Time) map[string]string {
	if sampledAt.IsZero() || len(tfs) == 0 {
		return nil
	}
	mode := freshnessAlways
	switch {
	case IsXyzDexAsset(symbol):
		mode = freshnessNever
	case IsBStockSymbol(symbol):
		if IsUSEquitySymbol(symbol) {
			mode = freshnessUSSession
		} else {
			mode = freshnessNever
		}
	}
	return staleTimeframesMode(tfs, sampledAt, mode)
}

type freshnessMode int

const (
	freshnessAlways    freshnessMode = iota // 24/7 market
	freshnessUSSession                      // only while the US regular session covers the window
	freshnessNever                          // calendar unknown: don't judge
)

func staleTimeframesMode(tfs map[string]*TimeframeSeriesData, sampledAt time.Time, mode freshnessMode) map[string]string {
	if mode == freshnessNever {
		return nil
	}
	var out map[string]string
	tfKeys := make([]string, 0, len(tfs))
	for tf := range tfs {
		tfKeys = append(tfKeys, tf)
	}
	sort.Strings(tfKeys)
	for _, tf := range tfKeys {
		sd := tfs[tf]
		dur := TimeframeDuration(tf)
		if sd == nil || dur <= 0 {
			continue
		}
		if mode == freshnessUSSession {
			// The market must have been open for the entire window the check
			// looks back over, otherwise the "missing" bars are just the
			// overnight/weekend break (this also covers every ≥4h frame).
			// Holidays are not modelled: on one the gate may flag stale, which
			// only withholds new entries on a day without a live market.
			window := time.Duration(MaxStaleBars+1) * dur
			if !IsUSMarketOpen(sampledAt) || !IsUSMarketOpen(sampledAt.Add(-window)) {
				continue
			}
		}
		if reason := SeriesStaleReason(tf, sd.Klines, sampledAt, mode == freshnessAlways); reason != "" {
			if out == nil {
				out = make(map[string]string)
			}
			out[tf] = reason
		}
	}
	return out
}
