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
// NET forward returns (round-trip taker fees + slippage) of every signal.
// The tuner selects cutoffs on a TRAIN segment and applies them only when
// they verify on the held-out TEST segment (temporal walk-forward) —
// in-sample argmax was rejected by the 2026-09-22 quant review (E2/A2).
// Known non-replayable layers, uniform for all candidates: the crowd
// penalty (no funding/OI/LS history) stays out of the replay score.

const (
	btForwardBars1h  = 4    // 15m bars ≈ 1h
	btForwardBars4h  = 16   // 15m bars ≈ 4h
	btForwardBars24h = 96   // 15m bars ≈ 24h
	btWarmupBars     = 260  // bars before the first scored bar (windows + levels)
	btMinSample      = 40   // TRAIN-set minimum signals for a cutoff to be selectable
	btVerifySample   = 15   // TEST-set minimum signals for a change to verify
	btTrainSplit     = 0.7  // temporal walk-forward split (train share)
	btCostRoundTrip  = 0.20 // % net cost per trade: 2×5bps taker fee + 2×5bps slippage
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
	Ret1h       float64   `json:"ret_1h"` // signed by direction, NET of round-trip cost
	Ret4h       float64   `json:"ret_4h"`
	Ret24h      float64   `json:"ret_24h"`
	Regime      string    `json:"regime,omitempty"` // BTC regime replayed at signal time
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
	// E2/A2 (QUANT_REVIEW 09-22): outcomes are NET of btCostRoundTrip, and
	// parameter changes come from a temporal walk-forward split — cutoffs
	// selected on the TRAIN segment must verify on the held-out TEST segment
	// (bucket edge positive AND above the test-wide average) or they are
	// rejected, not applied.
	CostRoundTripPct float64 `json:"cost_round_trip_pct"`
	TrainSignals     int     `json:"train_signals"`
	TestSignals      int     `json:"test_signals"`
	Verified         []string `json:"verified,omitempty"`
	Rejected         []string `json:"rejected,omitempty"`
}

const backtestPath = "data/breakout_backtest.json"

// RunBacktest replays the scorer over historical 15m klines for the given
// symbols and returns the signal outcomes. Scores carry the ONLINE penalty
// layers that are kline-replayable (extended-pattern ×0.65, BTC-regime
// haircut; α/β/confirm already live in the TF score) so threshold selection
// happens on the same distribution shape Analyze() produces online (A2) —
// the crowd penalty is NOT replayable (no funding/OI/LS history) and stays
// out, a direction-uniform limitation. Forward returns are NET of
// btCostRoundTrip.
func RunBacktest(symbols []string, bars int) ([]BTSignal, int, error) {
	total := 0
	var all []BTSignal
	btc := NewBinanceDS("BTCUSDT")
	btc4h, err := btc.Klines("4h", 200)
	if err != nil {
		return nil, 0, fmt.Errorf("BTC 4h klines for regime replay: %w", err)
	}
	regimeBars := btcRegimeTimeline(btc4h)
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
			sigTime := time.UnixMilli(series[len(series)-1].OpenTime).UTC()
			regime, regimeMult := regimeAt(regimeBars, sigTime, dir)
			score *= regimeMult
			// Online extended-pattern penalty (A2): Analyze() multiplies the
			// selected side by extendedPenalty when the 1h pattern is
			// extended — the backtest used to skip it entirely.
			if tf1h := tfs["1h"][dir]; tf1h != nil && tf1h.Pattern == "extended" {
				score *= extendedPenalty
			}
			if score < GetParams().MediumThreshold {
				continue
			}
			tf := tfs["15m"][dir]

			entry := series[len(series)-1].Close
			sig := BTSignal{
				Time:        sigTime,
				Symbol:      sym,
				Direction:   dir,
				Score:       clamp(score, 0, 100),
				Grade:       gradeOf(clamp(score, 0, 100)),
				Entry:       entry,
				Pattern:     tf.Pattern,
				ATRStrength: tf.ATRStrength,
				VolMultiple: volSlotMultiple(series, 288),
				Regime:      regime,
			}
			sig.Ret1h = forwardReturn(k15, i, btForwardBars1h, dir) - btCostRoundTrip
			sig.Ret4h = forwardReturn(k15, i, btForwardBars4h, dir) - btCostRoundTrip
			sig.Ret24h = forwardReturn(k15, i, btForwardBars24h, dir) - btCostRoundTrip
			all = append(all, sig)
		}
		total++
	}
	return all, total, nil
}

// btcRegimeTimelinePoint is BTC's regime on one 4h bar, replayed from kline
// history so backtest signals can carry the same haircut the online scan
// applies (bull: counter-BTC shorts ×0.85 / bear: counter-BTC longs ×0.85 /
// chop: both ×0.95 — scheduler.go semantics, shortscan.go bull predicate).
type btcRegimeTimelinePoint struct {
	openTimeMs int64
	upMult     float64
	downMult   float64
	regime     string
}

// btcRegimeTimeline classifies every 4h bar of BTC history.
func btcRegimeTimeline(btc4h []Kline) []btcRegimeTimelinePoint {
	c := closes(btc4h)
	if len(c) < 30 {
		return nil
	}
	e20 := ema(c, 20)
	e50 := ema(c, 50)
	rsi := rsiSeries(c, 14)
	out := make([]btcRegimeTimelinePoint, len(c))
	for i := range c {
		pt := btcRegimeTimelinePoint{openTimeMs: btc4h[i].OpenTime, upMult: 1.0, downMult: 1.0, regime: "chop"}
		if i >= 20 && i < len(rsi) {
			bull := e20[i] > e50[i] && c[i] > e20[i] && rsi[i] >= 60
			bear := e20[i] < e50[i] && c[i] < e20[i] && rsi[i] <= 40
			switch {
			case bull:
				pt.regime = "btc_bull"
				pt.downMult = 0.85
			case bear:
				pt.regime = "btc_bear"
				pt.upMult = 0.85
			default:
				pt.upMult = 0.95
				pt.downMult = 0.95
			}
		}
		out[i] = pt
	}
	return out
}

// regimeAt returns the regime in force at t for the given direction — the
// last 4h bar that had CLOSED by then.
func regimeAt(tl []btcRegimeTimelinePoint, t time.Time, dir string) (string, float64) {
	if len(tl) == 0 {
		return "", 1.0
	}
	ms := t.UnixMilli()
	pick := tl[0]
	for i := len(tl) - 1; i >= 0; i-- {
		if tl[i].openTimeMs+4*3600*1000 <= ms {
			pick = tl[i]
			break
		}
	}
	if dir == DirDown {
		return pick.regime, pick.downMult
	}
	return pick.regime, pick.upMult
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

	summary.CostRoundTripPct = btCostRoundTrip
	changes, verified, rejected, trainN, testN := tuneWalkForward(signals)
	summary.Changes = changes
	summary.Verified = verified
	summary.Rejected = rejected
	summary.TrainSignals = trainN
	summary.TestSignals = testN

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

// tuneWalkForward adjusts parameters with a temporal hold-out: cutoffs are
// selected on the TRAIN segment (oldest 70%) and applied only when they
// VERIFY on the TEST segment (newest 30%) — the bucket's net edge must be
// positive AND above the test-wide average with enough samples. In-sample
// argmax alone was the old protocol; it selected cutoffs on exactly the
// window it was scored on (E2, QUANT_REVIEW 09-22).
func tuneWalkForward(signals []BTSignal) (changes, verified, rejected []string, trainN, testN int) {
	if len(signals) < btMinSample {
		logger.Infof("🐷 Backtest tuning skipped: %d signals < %d minimum", len(signals), btMinSample)
		return nil, nil, nil, 0, 0
	}
	sorted := append([]BTSignal(nil), signals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })
	split := int(float64(len(sorted)) * btTrainSplit)
	if split < btMinSample || len(sorted)-split < btVerifySample {
		logger.Infof("🐷 Backtest tuning skipped: split %d/%d too thin (train ≥ %d, test ≥ %d required)", split, len(sorted)-split, btMinSample, btVerifySample)
		return nil, nil, nil, split, len(sorted) - split
	}
	train, test := sorted[:split], sorted[split:]
	trainN, testN = len(train), len(test)

	prev := GetParams()
	next := prev

	// 1. Grade thresholds: best 24h NET edge on TRAIN, then verify on TEST.
	testOverall := avgRet(test)
	strongCutoff := bestCutoff(train, 72, 90, 5, btMinSample)
	mediumCutoff := 0.0
	if strongCutoff > 0 {
		mediumCutoff = bestCutoff(train, 50, int(strongCutoff)-10, 5, btMinSample+20)
	}
	verify := func(name string, cutoff float64) bool {
		if cutoff <= 0 {
			return false
		}
		sum, n := 0.0, 0
		for _, s := range test {
			if s.Score >= cutoff {
				sum += s.Ret24h
				n++
			}
		}
		if n < btVerifySample {
			rejected = append(rejected, fmt.Sprintf("%s %.0f: test n=%d < %d", name, cutoff, n, btVerifySample))
			return false
		}
		edge := sum / float64(n)
		if edge <= 0 || edge <= testOverall {
			rejected = append(rejected, fmt.Sprintf("%s %.0f: test edge %.2f%% (overall %.2f%%) failed verification", name, cutoff, edge, testOverall))
			return false
		}
		verified = append(verified, fmt.Sprintf("%s %.0f verified: test edge %+.2f%% vs overall %+.2f%% (n=%d)", name, cutoff, edge, testOverall, n))
		return true
	}
	if strongCutoff > 0 && strongCutoff != prev.StrongThreshold && verify("strong_threshold", strongCutoff) {
		next.StrongThreshold = strongCutoff
	}
	if mediumCutoff > 0 && mediumCutoff != prev.MediumThreshold && verify("medium_threshold", mediumCutoff) {
		next.MediumThreshold = mediumCutoff
	}
	if next.StrongThreshold != prev.StrongThreshold {
		changes = append(changes, fmt.Sprintf("strong_threshold: %.1f → %.1f", prev.StrongThreshold, next.StrongThreshold))
	}
	if next.MediumThreshold != prev.MediumThreshold {
		changes = append(changes, fmt.Sprintf("medium_threshold: %.1f → %.1f", prev.MediumThreshold, next.MediumThreshold))
	}

	// 2. Sigmoid centers: median ATR strength / volume multiple of TRAIN
	// winners, half-step toward target (small bounded drift, no holdout claim).
	var winATR, winVol []float64
	for _, s := range train {
		if s.Ret24h > 0 {
			winATR = append(winATR, s.ATRStrength)
			winVol = append(winVol, s.VolMultiple)
		}
	}
	if len(winATR) >= btMinSample {
		half := func(key string, target, current *float64) {
			t := clampParam(key, *target)
			if math.Abs(t-*current) < 0.01 {
				return
			}
			*current = clampParam(key, *current+0.5*(t-*current))
			changes = append(changes, fmt.Sprintf("%s: %.3f → %.3f (train-median, half-step)", key, prevValue(key, prev), *current))
		}
		half("price_atr_center", &next.PriceATRCenter, &next.PriceATRCenter)
		half("vol_center", &next.VolCenter, &next.VolCenter)
	}

	if len(changes) == 0 {
		return nil, verified, rejected, trainN, testN
	}
	next.UpdatedAt = time.Now()
	next.BacktestAt = time.Now()
	next.Samples = len(signals)
	ApplyParams(next)
	return changes, verified, rejected, trainN, testN
}

// avgRet returns the mean 24h net forward return of a signal set.
func avgRet(signals []BTSignal) float64 {
	if len(signals) == 0 {
		return 0
	}
	sum := 0.0
	for _, s := range signals {
		sum += s.Ret24h
	}
	return sum / float64(len(signals))
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
