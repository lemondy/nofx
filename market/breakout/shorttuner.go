package breakout

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"nofx/logger"
)

// Short-scan online tuner. The nine component weights start at the designed
// values; a forward evaluator keeps nudging them toward the components that
// actually predicted 24h short PnL:
//
//   - every hour the scheduler samples the current top-10 (score, components,
//     price) into a JSONL journal;
//   - samples older than 24h are marked evaluated with
//     outcome = (scanPrice - priceNow) / scanPrice × 100 (unlevered short PnL);
//   - once ≥30 evaluated samples exist (evaluated since the last tune), each
//     component's weight is multiplied by exp(η × Pearson(component, outcome))
//     — components that correlated with profitable shorts gain share, the
//     rest lose — then clamped to [0.03, 0.30] and renormalized to sum 1.
//
// Historical backtesting is NOT possible for these components (Binance only
// serves ~24h of OI history and the LS ratio is spot-only-recent), so the
// tuner learns forward from live samples instead. Everything persists in a
// JSONL journal under the data dir; restart-safe.

const (
	shortTunerSampleEvery = time.Hour
	shortTunerEvalAfter   = 24 * time.Hour
	shortTunerMinSamples  = 30
	shortTunerEta         = 0.25
	shortTunerPruneAfter  = 14 * 24 * time.Hour
)

var shortTunerMu sync.Mutex

// Tunable short-weight keys, in AnalyzeShort's component order.
var shortWeightKeys = []string{
	"stretch", "overbought", "rejection", "volume_fade", "extension",
	"crowding", "parabolic", "divergence", "structure",
}

// DefaultShortWeights mirrors the designed composite (sums to 1.0).
func DefaultShortWeights() map[string]float64 {
	return map[string]float64{
		"stretch": 0.10,
		// overbought down-weighted (audit 2026-09-12 #2): RSI pinned high is a
		// STRONG-TREND feature, not a reversal signal — 0.15 was the largest
		// weight and fired hardest exactly when shorting is worst. The freed
		// weight goes to structure (a real topping confirmation).
		"overbought":  0.10,
		"rejection":   0.10,
		"volume_fade": 0.10,
		"extension":   0.10,
		"crowding":    0.15,
		"parabolic":   0.10,
		"divergence":  0.10,
		"structure":   0.15,
	}
}

// shortWeights returns the effective weights (tuned values when present and
// sane, designed defaults otherwise).
func shortWeights() map[string]float64 {
	p := GetParams()
	if len(p.ShortWeights) == 0 {
		return DefaultShortWeights()
	}
	w := make(map[string]float64, len(shortWeightKeys))
	total := 0.0
	for _, k := range shortWeightKeys {
		v := p.ShortWeights[k]
		if v <= 0 {
			v = DefaultShortWeights()[k]
		}
		w[k] = v
		total += v
	}
	if total <= 0 {
		return DefaultShortWeights()
	}
	for _, k := range shortWeightKeys {
		w[k] /= total
	}
	return w
}

// shortTuningPath is where the signal journal lives (override in tests).
var shortTuningPath = "data/shortscan_signals.jsonl"

// SetShortTuningPath overrides the journal location (tests / custom data dir).
func SetShortTuningPath(p string) {
	shortTunerMu.Lock()
	shortTuningPath = p
	shortTunerMu.Unlock()
}

type shortSample struct {
	TS         int64              `json:"ts"`
	Symbol     string             `json:"symbol"`
	Score      float64            `json:"score"`
	Price      float64            `json:"price"`
	Components map[string]float64 `json:"components"`
	Evaluated  bool               `json:"evaluated"`
	Outcome    float64            `json:"outcome,omitempty"` // % short PnL at +24h
}

// SampleShortSignals journals the current top-10 for future evaluation.
// Hourly cadence is enforced internally; a no-op before the next sample.
func SampleShortSignals(signals []ShortSignal, now time.Time) {
	shortTunerMu.Lock()
	defer shortTunerMu.Unlock()
	if len(signals) == 0 {
		return
	}
	samples := readSamples()
	for _, s := range samples {
		if now.UnixMilli()-s.TS < shortTunerSampleEvery.Milliseconds() {
			return // a fresh sample already exists this hour
		}
	}
	n := len(signals)
	if n > 10 {
		n = 10
	}
	var buf []byte
	for _, sig := range signals[:n] {
		rec := shortSample{
			TS:     now.UnixMilli(),
			Symbol: sig.Symbol,
			Score:  sig.Score,
			Price:  sig.Price,
			Components: map[string]float64{
				"stretch":     sig.Components.Stretch,
				"overbought":  sig.Components.Overbought,
				"rejection":   sig.Components.Rejection,
				"volume_fade": sig.Components.VolumeFade,
				"extension":   sig.Components.Extension,
				"crowding":    sig.Components.Crowding,
				"parabolic":   sig.Components.Parabolic,
				"divergence":  sig.Components.Divergence,
				"structure":   sig.Components.Structure,
			},
		}
		b, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		buf = append(buf, append(b, '\n')...)
	}
	f, err := os.OpenFile(shortTuningPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warnf("⚠️ Short tuner: cannot open journal: %v", err)
		return
	}
	defer f.Close()
	f.Write(buf)
}

// RunShortTuner evaluates matured samples and nudges the component weights.
// Called on the scheduler's 30-min slow tick (and once at goroutine start) —
// the old blind 24h ticker reset on every deploy and the tuner never fired
// again after 09-07 (user request 2026-09-17). Idempotent: Evaluated flags
// keep re-runs free; every step is best-effort and panic-recovered.
func RunShortTuner(now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("🩸 Short tuner panicked (recovered): %v", r)
		}
	}()
	shortTunerMu.Lock()
	defer shortTunerMu.Unlock()
	samples := readSamples()
	if len(samples) == 0 {
		return
	}

	// 1. Evaluate matured samples with one batched ticker call.
	prices, err := allPerpTickers()
	if err != nil {
		return
	}
	priceOf := make(map[string]float64, len(prices))
	for _, t := range prices {
		if p, perr := parseFloatStr(t.LastPrice); perr == nil && p > 0 {
			priceOf[t.Symbol] = p
		}
	}
	changed := false
	for i := range samples {
		s := &samples[i]
		if s.Evaluated || s.Price <= 0 {
			continue
		}
		if now.UnixMilli()-s.TS < shortTunerEvalAfter.Milliseconds() {
			continue
		}
		nowPrice, ok := priceOf[s.Symbol]
		if !ok || nowPrice <= 0 {
			s.Evaluated = true // delisted/no data — drop from future evaluation
			s.Outcome = 0
			changed = true
			continue
		}
		s.Outcome = (s.Price - nowPrice) / s.Price * 100
		s.Evaluated = true
		changed = true
	}

	// 2. Weight update from the evaluated cohort (bounded, clamped).
	var eval []shortSample
	for _, s := range samples {
		if s.Evaluated && len(s.Components) > 0 {
			eval = append(eval, s)
		}
	}
	if len(eval) >= shortTunerMinSamples {
		w := shortWeights()
		newW := updateShortWeights(eval, w, shortTunerEta)
		if !weightsClose(newW, w) {
			p := GetParams()
			p.ShortWeights = newW
			ApplyParams(p)
			logger.Infof("🩸 Short tuner: weights updated from %d evaluated samples: %v", len(eval), formatWeights(newW))
			changed = true
		}
	}

	// 3. Prune old journal entries and rewrite when anything changed.
	cut := now.Add(-shortTunerPruneAfter).UnixMilli()
	kept := samples[:0]
	for _, s := range samples {
		if s.TS >= cut {
			kept = append(kept, s)
		}
	}
	if changed || len(kept) != len(samples) {
		writeSamples(kept)
	}
}

// updateShortWeights nudges each component weight by exp(η × corr(component,
// outcome)) over the evaluated cohort, then clamps and renormalizes. Pure.
func updateShortWeights(samples []shortSample, base map[string]float64, eta float64) map[string]float64 {
	out := make(map[string]float64, len(shortWeightKeys))
	for _, k := range shortWeightKeys {
		var xs, ys []float64
		for _, s := range samples {
			v, ok := s.Components[k]
			if !ok {
				continue
			}
			xs = append(xs, v)
			ys = append(ys, s.Outcome)
		}
		corr := pearson(xs, ys)
		out[k] = base[k] * math.Exp(eta*corr)
	}
	// Clamp + renormalize.
	const lo, hi = 0.03, 0.30
	total := 0.0
	for _, k := range shortWeightKeys {
		if out[k] < lo {
			out[k] = lo
		}
		if out[k] > hi {
			out[k] = hi
		}
		total += out[k]
	}
	if total <= 0 {
		return DefaultShortWeights()
	}
	for _, k := range shortWeightKeys {
		out[k] = math.Round(out[k]/total*10000) / 10000
	}
	return out
}

func pearson(xs, ys []float64) float64 {
	n := len(xs)
	if n < 2 || len(ys) != n {
		return 0
	}
	var sx, sy float64
	for i := 0; i < n; i++ {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/float64(n), sy/float64(n)
	var cov, vx, vy float64
	for i := 0; i < n; i++ {
		dx, dy := xs[i]-mx, ys[i]-my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	if vx == 0 || vy == 0 {
		return 0
	}
	return cov / math.Sqrt(vx*vy)
}

func weightsClose(a, b map[string]float64) bool {
	for _, k := range shortWeightKeys {
		if math.Abs(a[k]-b[k]) > 0.0005 {
			return false
		}
	}
	return true
}

func formatWeights(w map[string]float64) string {
	out := "{"
	first := true
	for _, k := range shortWeightKeys {
		if !first {
			out += ", "
		}
		out += fmt.Sprintf("%s:%.3f", k, w[k])
		first = false
	}
	return out + "}"
}

func readSamples() []shortSample {
	b, err := os.ReadFile(shortTuningPath)
	if err != nil {
		return nil
	}
	var samples []shortSample
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var s shortSample
		if json.Unmarshal(line, &s) == nil && s.Symbol != "" {
			samples = append(samples, s)
		}
	}
	return samples
}

func writeSamples(samples []shortSample) {
	f, err := os.OpenFile(shortTuningPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	for _, s := range samples {
		if b, err := json.Marshal(s); err == nil {
			f.Write(append(b, '\n'))
		}
	}
}

func parseFloatStr(s string) (float64, error) {
	var v float64
	_, err := fmt.Sscanf(s, "%g", &v)
	return v, err
}
