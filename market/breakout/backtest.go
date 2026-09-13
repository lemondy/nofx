package breakout

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"nofx/logger"
)

// The backtest harness replays the scorer over historical klines and measures
// forward returns of every signal. The tuner uses those outcomes to micro-
// adjust tunable parameters within hard bounds — a small, evidence-based step
// every 3 days instead of theory-driven constants.

const (
	btForwardBars1h  = 4   // 15m bars ≈ 1h
	btForwardBars4h  = 16  // 15m bars ≈ 4h
	btForwardBars24h = 96  // 15m bars ≈ 24h
	btWarmupBars     = 260 // bars before the first scored bar (windows + levels)
	btMinSample      = 40  // minimum signals for a cutoff to be tunable
)

// BTSignal is one historical signal with forward outcomes.
type BTSignal struct {
	Time        time.Time `json:"time"`
	Symbol      string    `json:"symbol"`
	Direction   string    `json:"direction"`
	Grade       string    `json:"grade"`
	Score       float64   `json:"score"`
	Entry       float64   `json:"entry"`
	Pattern     string    `json:"pattern"`
	ATRStrength float64   `json:"atr_strength"`
	VolMultiple float64   `json:"vol_multiple"`
	Ret1h       float64   `json:"ret_1h"` // signed by direction, %
	Ret4h       float64   `json:"ret_4h"`
	Ret24h      float64   `json:"ret_24h"`
}

// GradeStats aggregates outcomes for one grade bucket.
type GradeStats struct {
	Count    int     `json:"count"`
	WinRate  float64 `json:"win_rate"`  // % of signals with positive 24h forward return
	AvgRet24 float64 `json:"avg_ret24"` // mean signed 24h forward return, %
}

// BacktestSummary is the persisted result of one tuning session.
type BacktestSummary struct {
	GeneratedAt time.Time              `json:"generated_at"`
	Symbols     int                    `json:"symbols"`
	Signals     int                    `json:"signals"`
	ByGrade     map[string]*GradeStats `json:"by_grade"`
	Changes     []string               `json:"changes"`
	Params      TunableParams          `json:"params"`
}

const backtestPath = "data/breakout_backtest.json"

// RunBacktest replays the scorer over historical 15m klines for the given
// symbols and returns the signal outcomes.
func RunBacktest(symbols []string, bars int) ([]BTSignal, int, error) {
	total := 0
	var all []BTSignal
	for _, sym := range symbols {
		ds := NewBinanceDS(sym)
		k15, err := ds.Klines("15m", bars)
		if err != nil {
			logger.Warnf("⚠️ Backtest %s: 15m klines: %v", sym, err)
			continue
		}
		k1h, err := ds.Klines("1h", 400)
		if err != nil {
			logger.Warnf("⚠️ Backtest %s: 1h klines: %v", sym, err)
			continue
		}
		d1, err := ds.Klines("1d", 90)
		if err != nil {
			logger.Warnf("⚠️ Backtest %s: daily klines: %v", sym, err)
			continue
		}

		// Levels use the full fetched history as a static approximation
		// (rebuilding per-bar daily levels would multiply cost with little gain).
		levels := buildLevels(d1, k1h)

		end := len(k15) - btForwardBars24h
		for i := btWarmupBars; i < end; i += 3 { // stride 3 keeps cost sane
			series := k15[:i+1]
			levelsHere := levels // static
			sh := &shared{}      // point-in-time OI/funding/depth are unavailable historically — context dims score neutral

			tfs := map[string]map[string]*TFReport{
				"15m": {
					DirUp:   computeTF("15m", DirUp, series, levelsHere, sh),
					DirDown: computeTF("15m", DirDown, series, levelsHere, sh),
				},
				"1h": {
					DirUp:   computeTF("1h", DirUp, resample1h(series), levelsHere, sh),
					DirDown: computeTF("1h", DirDown, resample1h(series), levelsHere, sh),
				},
			}
			up := combine(tfs, DirUp)
			down := combine(tfs, DirDown)
			dir, score := DirUp, up.Score
			if down.Score > up.Score {
				dir, score = DirDown, down.Score
			}
			if score < GetParams().MediumThreshold {
				continue
			}
			tf := tfs["15m"][dir]

			entry := series[len(series)-1].Close
			sig := BTSignal{
				Time:        time.UnixMilli(series[len(series)-1].OpenTime).UTC(),
				Symbol:      sym,
				Direction:   dir,
				Score:       score,
				Entry:       entry,
				Pattern:     tf.Pattern,
				ATRStrength: tf.ATRStrength,
				VolMultiple: volSlotMultiple(series, 288),
			}
			sig.Ret1h = forwardReturn(k15, i, btForwardBars1h, dir)
			sig.Ret4h = forwardReturn(k15, i, btForwardBars4h, dir)
			sig.Ret24h = forwardReturn(k15, i, btForwardBars24h, dir)
			all = append(all, sig)
		}
		total++
	}
	return all, total, nil
}

// forwardReturn returns the signed % change of close[i+bars] vs close[i].
func forwardReturn(k []Kline, i, bars int, dir string) float64 {
	j := i + bars
	if j >= len(k) {
		j = len(k) - 1
	}
	if j <= i || k[i].Close <= 0 {
		return 0
	}
	r := (k[j].Close/k[i].Close - 1) * 100
	if dir == DirDown {
		r = -r
	}
	return r
}

// resample1h aggregates 15m candles into 1h candles.
func resample1h(k []Kline) []Kline {
	var out []Kline
	for i := 0; i+3 < len(k); i += 4 {
		b := k[i : i+4]
		c := Kline{
			OpenTime: b[0].OpenTime,
			Open:     b[0].Open,
			High:     b[0].High,
			Low:      b[0].Low,
			Close:    b[3].Close,
		}
		for _, x := range b {
			if x.High > c.High {
				c.High = x.High
			}
			if x.Low < c.Low {
				c.Low = x.Low
			}
			c.Volume += x.Volume
			c.QuoteVolume += x.QuoteVolume
			c.TakerBuyBase += x.TakerBuyBase
			c.TakerBuyQty += x.TakerBuyQty
		}
		out = append(out, c)
	}
	return out
}

// ── Tuner ──

// TuneFromBacktest runs the backtest over the given symbols, computes outcome
// stats, and micro-adjusts tunable parameters within hard bounds. Returns the
// summary including a human-readable change list.
func TuneFromBacktest(symbols []string) (*BacktestSummary, error) {
	signals, symbolCount, err := RunBacktest(symbols, 1400)
	if err != nil {
		return nil, err
	}

	summary := &BacktestSummary{
		GeneratedAt: time.Now().UTC(),
		Symbols:     symbolCount,
		Signals:     len(signals),
		ByGrade:     map[string]*GradeStats{},
	}
	for _, g := range []string{"strong", "medium", "weak", "noise"} {
		summary.ByGrade[g] = &GradeStats{}
	}
	for _, s := range signals {
		g, ok := summary.ByGrade[s.Grade]
		if !ok || g == nil {
			continue // unexpected grade — skip rather than crash the tuner
		}
		g.Count++
		if s.Ret24h > 0 {
			g.WinRate++
		}
		g.AvgRet24 += s.Ret24h
	}
	for _, g := range summary.ByGrade {
		if g.Count > 0 {
			g.WinRate = round2(g.WinRate / float64(g.Count) * 100)
			g.AvgRet24 = round2(g.AvgRet24 / float64(g.Count))
		}
	}

	changes := tune(signals, summary)
	summary.Changes = changes

	params := GetParams()
	summary.Params = params

	if data, err := json.MarshalIndent(summary, "", "  "); err == nil {
		_ = os.MkdirAll("data", 0o755)
		_ = os.WriteFile(backtestPath, data, 0o644)
	}

	if len(changes) == 0 {
		logger.Infof("🐷 Backtest tuning: %d signals analyzed, parameters already optimal", len(signals))
	} else {
		for _, ch := range changes {
			logger.Infof("🐷 Backtest tuning: %s", ch)
		}
	}
	return summary, nil
}

// tune adjusts parameters toward the empirically best values, bounded and
// conservative (half-step toward the target).
func tune(signals []BTSignal, summary *BacktestSummary) []string {
	if len(signals) < btMinSample {
		logger.Infof("🐷 Backtest tuning skipped: %d signals < %d minimum", len(signals), btMinSample)
		return nil
	}

	prev := GetParams()
	next := prev
	var changes []string

	// 1. Grade thresholds: pick the score cutoffs with the best 24h edge
	//    (mean signed forward return) given enough samples.
	strongCutoff := bestCutoff(signals, 72, 90, 5, btMinSample)
	mediumCutoff := bestCutoff(signals, 50, int(strongCutoff)-10, 5, btMinSample+20)
	if strongCutoff > 0 && strongCutoff != prev.StrongThreshold {
		next.StrongThreshold = strongCutoff
	}
	if mediumCutoff > 0 && mediumCutoff != prev.MediumThreshold {
		next.MediumThreshold = mediumCutoff
	}

	// 2. Price sigmoid center: median ATR strength of 24h winners.
	var winATR []float64
	var winVol []float64
	for _, s := range signals {
		if s.Ret24h > 0 {
			winATR = append(winATR, s.ATRStrength)
			winVol = append(winVol, s.VolMultiple)
		}
	}
	if len(winATR) >= btMinSample {
		next.PriceATRCenter = median(winATR)
		next.VolCenter = median(winVol)
	}

	// threshold step uses full jump (they are re-anchored cutoffs, not drift)
	if next.StrongThreshold != prev.StrongThreshold {
		changes = append(changes, fmt.Sprintf("strong_threshold: %.1f → %.1f", prev.StrongThreshold, next.StrongThreshold))
	}
	if next.MediumThreshold != prev.MediumThreshold {
		changes = append(changes, fmt.Sprintf("medium_threshold: %.1f → %.1f", prev.MediumThreshold, next.MediumThreshold))
	}

	half := func(key string, target, current *float64) {
		t := clampParam(key, *target)
		if math.Abs(t-*current) < 0.01 {
			return
		}
		*current = clampParam(key, *current+0.5*(t-*current))
		changes = append(changes, fmt.Sprintf("%s: %.3f → %.3f", key, prevValue(key, prev), *current))
	}
	half("price_atr_center", &next.PriceATRCenter, &next.PriceATRCenter)
	half("vol_center", &next.VolCenter, &next.VolCenter)

	if len(changes) == 0 {
		return nil
	}
	next.UpdatedAt = time.Now()
	next.BacktestAt = time.Now()
	next.Samples = len(signals)
	ApplyParams(next)
	return changes
}

// prevValue reads the previous value for change-log formatting.
func prevValue(key string, prev TunableParams) float64 {
	switch key {
	case "price_atr_center":
		return prev.PriceATRCenter
	case "vol_center":
		return prev.VolCenter
	}
	return 0
}

// bestCutoff sweeps candidate cutoffs and returns the one with the best mean
// 24h forward return among signals at or above the cutoff (n ≥ minSamples).
// Returns 0 when no cutoff qualifies.
func bestCutoff(signals []BTSignal, lo, hi, step, minSamples int) float64 {
	type cut struct {
		cutoff float64
		edge   float64
		n      int
	}
	var best *cut
	for c := float64(lo); c <= float64(hi); c += float64(step) {
		var sum float64
		n := 0
		for _, s := range signals {
			if s.Score >= c {
				sum += s.Ret24h
				n++
			}
		}
		if n < minSamples {
			continue
		}
		edge := sum / float64(n)
		if best == nil || edge > best.edge {
			b, _ := c, 0
			best = &cut{cutoff: b, edge: edge, n: n}
		}
	}
	if best == nil {
		return 0
	}
	return best.cutoff
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// LoadBacktestSummary reads the last persisted tuning summary.
func LoadBacktestSummary() (*BacktestSummary, error) {
	data, err := os.ReadFile(backtestPath)
	if err != nil {
		return nil, err
	}
	var s BacktestSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
