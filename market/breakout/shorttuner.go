package breakout

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"nofx/logger"
)

// Short scanner forward research: hourly observations are labelled at a
// fixed +24h horizon using historical prices, funding and disclosed costs.
// Optional studies aggregate separated 24h time blocks and save bounded
// weight proposals. They never publish live weights or advance a live cursor.
// Live custom weights additionally require explicit validation/promotion.

const (
	shortTunerSampleEvery = time.Hour
	shortTunerEvalAfter   = 24 * time.Hour
	shortTunerMinSamples  = 30
	shortTunerEta         = 0.25
	shortTunerPruneAfter  = 180 * 24 * time.Hour
)

var shortTunerMu sync.Mutex

// The existing enable switch now permits research proposals only.
func shortTunerUpdateEnabled() bool {
	p := GetParams()
	return p.ShortTunerEnabled != nil && *p.ShortTunerEnabled
}

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

// shortWeights returns the effective weights. P0 fix (2026-09-26 review):
// persisted ShortWeights are ONLY honored while the tuner update is ENABLED —
// the railed file (structure 0.0299 etc., written by the pre-disable binary
// on 09-23) kept feeding live scoring after the update switch went off.
// Disabled ⇒ designed defaults, full stop; the polluted file becomes inert.
func shortWeights() map[string]float64 {
	p := GetParams()
	if !shortTunerUpdateEnabled() || !p.ShortWeightsValidated || len(p.ShortWeights) == 0 {
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
	return boundedShortWeights(w)
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
	LabelAttempts int                `json:"label_attempts,omitempty"`
	NextRetryAt   int64              `json:"next_retry_at,omitempty"`
	LabelAt       int64              `json:"label_at,omitempty"`
	LabelVersion  int                `json:"label_version,omitempty"`
	EvaluatedAt   int64              `json:"evaluated_at,omitempty"`
	FundingPct    float64            `json:"funding_pct,omitempty"`
	CostPct       float64            `json:"cost_pct,omitempty"`
	MissingReason string             `json:"missing_reason,omitempty"`
	TS            int64              `json:"ts"`
	Symbol        string             `json:"symbol"`
	Score         float64            `json:"score"`
	Price         float64            `json:"price"`
	Components    map[string]float64 `json:"components"`
	Evaluated     bool               `json:"evaluated"`
	Outcome       float64            `json:"outcome,omitempty"`     // % short PnL at +24h
	Unpriceable   bool               `json:"unpriceable,omitempty"` // delisted/no quote — excluded from correlations
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
	if _, err := f.Write(buf); err != nil {
		logger.Errorf("short sample append: %v", err)
	}
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
	// P3-3 (review 2026-10-06): the lock used to span up to 50 samples × 2
	// sequential HTTP calls (~25 min worst case) — journaling and param-path
	// callers blocked behind it. Three phases now: collect DUE samples under
	// the lock, run the HTTP labelling with NO lock held, then re-lock and
	// MERGE results by (TS, Symbol) into a FRESH read (concurrent appends
	// during labelling must survive — never write back a stale snapshot).
	shortTunerMu.Lock()
	samples := readSamples()
	type dueItem struct {
		sample shortSample
	}
	var due []dueItem
	legacyInvalidated := false
	evaluatedThisRun := 0
	for i := range samples {
		s := &samples[i]
		if s.Evaluated && s.LabelVersion != 2 {
			s.Evaluated = false
			s.Unpriceable = false
			legacyInvalidated = true
		}
		if s.Evaluated || s.NextRetryAt > now.UnixMilli() || now.UnixMilli()-s.TS < shortTunerEvalAfter.Milliseconds() {
			continue
		}
		if evaluatedThisRun >= 50 {
			break
		}
		evaluatedThisRun++
		due = append(due, dueItem{sample: *s})
	}
	shortTunerMu.Unlock()

	// Lock-free labelling: historical +24h prices, never the scheduler's
	// current quote. Each result is keyed by (TS, Symbol) for the merge.
	type labelResult struct {
		price, funding    float64
		labelAt           int64
		errMsg                  string
		failed                  bool
	}
	results := make(map[[2]interface{}]labelResult, len(due))
	for _, d := range due {
		s := d.sample
		res := labelResult{}
		if s.Price <= 0 || math.IsNaN(s.Price) || math.IsInf(s.Price, 0) || s.TS <= 0 {
			res.failed, res.errMsg = true, "invalid sample price/time"
		} else {
			price, labelAt, funding, err := historicalShortLabel(s)
			if err != nil {
				res.failed, res.errMsg = true, err.Error()
			} else {
				res.price, res.labelAt, res.funding = price, labelAt, funding
			}
		}
		results[[2]interface{}{s.TS, s.Symbol}] = res
	}

	shortTunerMu.Lock()
	defer shortTunerMu.Unlock()
	// Fresh read: samples appended while we were labelling stay in the slice.
	samples = readSamples()
	changed := legacyInvalidated
	for i := range samples {
		s := &samples[i]
		// Re-apply the legacy invalidation on the FRESH read (recheck R4):
		// phase 1 invalidated v1 labels on the PRE-labelling snapshot only —
		// a v1 sample re-read from disk still carries Evaluated=true and the
		// skip below would discard its freshly fetched label forever, looping
		// the relabel budget every run without ever migrating the sample.
		if s.Evaluated && s.LabelVersion != 2 {
			s.Evaluated = false
			s.Unpriceable = false
			changed = true
		}
		if s.Evaluated || s.NextRetryAt > now.UnixMilli() || now.UnixMilli()-s.TS < shortTunerEvalAfter.Milliseconds() {
			continue
		}
		res, ok := results[[2]interface{}{s.TS, s.Symbol}]
		if !ok {
			continue // not due in this run (cap) or appended mid-labelling
		}
		if res.failed {
			if res.errMsg == "invalid sample price/time" {
				s.Unpriceable = true
				s.MissingReason = res.errMsg
				s.Evaluated = true
				s.LabelVersion = 2
				changed = true
				continue
			}
			s.Unpriceable = true
			s.MissingReason = res.errMsg
			s.LabelAttempts++
			retry := time.Hour * time.Duration(1<<min(s.LabelAttempts, 4))
			s.NextRetryAt = now.Add(retry).UnixMilli()
			changed = true
			continue // transient/delisting gaps are observable and retryable
		}
		s.Outcome = (s.Price-res.price)/s.Price*100 + res.funding - btCostRoundTrip
		s.LabelAt = res.labelAt
		s.LabelVersion = 2
		s.EvaluatedAt = now.UnixMilli()
		s.FundingPct = res.funding
		s.CostPct = btCostRoundTrip
		s.Evaluated = true
		s.Unpriceable = false
		s.MissingReason = ""
		s.NextRetryAt = 0
		changed = true
	}

	// Persist valid labels before using them to produce a research proposal.
	cut := now.Add(-shortTunerPruneAfter).UnixMilli()
	kept := samples[:0]
	for _, s := range samples {
		if s.TS >= cut {
			kept = append(kept, s)
		}
	}
	if changed || len(kept) != len(samples) {
		if err := writeSamples(kept); err != nil {
			logger.Errorf("short journal persist failed: %v", err)
			return
		}
	}
	samples = kept

	// Optional bounded research proposal; publication remains separate.
	if shortTunerUpdateEnabled() {
		// Evaluate labels since the last explicit promotion. Re-running a
		// proposal never compounds the candidate into live weights.
		var eval []shortSample
		lastTuned := GetParams().ShortTunerLastTunedMs
		for _, s := range samples {
			// Delisted/unpriceable samples carry Outcome=0 — including them
			// poisoned the correlations with fake zeros. Excluded.
			if s.Evaluated && s.LabelVersion == 2 && !s.Unpriceable && s.TS > lastTuned && len(s.Components) > 0 {
				eval = append(eval, s)
			}
		}
		if len(eval) >= shortTunerMinSamples {
			w := shortWeights()
			newW, ok := updateShortWeights(eval, w, shortTunerEta)
			// Retain thin cohorts until enough non-overlapping time blocks accrue.
			// Proposal generation does not consume the live promotion cursor.
			if ok {
				proposal := ShortWeightProposal{GeneratedAt: now.UTC(), Incumbent: w, Candidate: newW, Samples: len(eval), Applied: false, EvaluationScope: "fixed_24h_forward_labels_research_only"}
				data, err := json.MarshalIndent(proposal, "", "  ")
				if err == nil {
					err = atomicWriteJSON(shortWeightProposalPath, data)
				}
				if err != nil {
					logger.Errorf("short research proposal persist failed: %v", err)
				}
			}

		}
	} else {
		logger.Infof("🩸 Short tuner: weight research disabled (short_tuner_enabled=false) — outcome journal keeps accumulating, weights untouched")
	}

}

const shortTunerMinComponentN = 30

// One observation per non-overlapping 24h block limits repeated-symbol and
// overlapping-label pseudo replication. Fisher z > 3 is a conservative
// normal-approximation gate after testing nine components (two-sided p<0.003).
func updateShortWeights(samples []shortSample, base map[string]float64, eta float64) (map[string]float64, bool) {
	type block struct {
		x, y float64
		n    int
	}
	out := make(map[string]float64, len(shortWeightKeys))
	significant := false
	for _, key := range shortWeightKeys {
		groups := map[int64]*block{}
		for _, s := range samples {
			x, ok := s.Components[key]
			if !ok || s.TS <= 0 || math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(s.Outcome) || math.IsInf(s.Outcome, 0) {
				continue
			}
			// Entry windows from adjacent UTC days still overlap. Retain alternate
			// daily blocks so every included block has a full 24h separation.
			day := s.TS / (24 * time.Hour).Milliseconds()
			if day%2 != 0 {
				continue
			}
			g := groups[day]
			if g == nil {
				g = &block{}
				groups[day] = g
			}
			g.x += x
			g.y += s.Outcome
			g.n++
		}
		days := make([]int64, 0, len(groups))
		for day := range groups {
			days = append(days, day)
		}
		sort.Slice(days, func(i, j int) bool { return days[i] < days[j] })
		var xs, ys []float64
		for _, day := range days {
			g := groups[day]
			xs = append(xs, g.x/float64(g.n))
			ys = append(ys, g.y/float64(g.n))
		}
		corr := pearson(xs, ys)
		r := math.Min(math.Abs(corr), 1-1e-12)
		z := math.Atanh(r) * math.Sqrt(math.Max(0, float64(len(xs)-3)))
		if len(xs) >= shortTunerMinComponentN && z > 3 {
			significant = true
			out[key] = base[key] * math.Exp(eta*corr)
		} else {
			out[key] = base[key]
		}
	}
	if !significant {
		return nil, false
	}
	return boundedShortWeights(out), true
}

// Project onto a bounded simplex; normalization cannot undo either bound.
func boundedShortWeights(weights map[string]float64) map[string]float64 {
	lo, hi := 0.0, 100.0
	for i := 0; i < 100; i++ {
		scale := (lo + hi) / 2
		sum := 0.0
		for _, key := range shortWeightKeys {
			sum += clamp(weights[key]*scale, 0.03, 0.30)
		}
		if sum > 1 {
			hi = scale
		} else {
			lo = scale
		}
	}
	out := map[string]float64{}
	for _, key := range shortWeightKeys {
		out[key] = clamp(weights[key]*(lo+hi)/2, 0.03, 0.30)
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

func writeSamples(samples []shortSample) error {
	var buf bytes.Buffer
	for _, s := range samples {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return atomicWriteJSON(shortTuningPath, buf.Bytes())
}

// The last fully closed one-minute candle at +24h gives a fixed horizon
// with <=60s resolution. Funding is signed for a short and fees/slippage
// use the same disclosed estimate as the research replay.
func historicalShortLabel(s shortSample) (float64, int64, float64, error) {
	target := time.UnixMilli(s.TS).Add(shortTunerEvalAfter)
	open := target.Truncate(time.Minute).Add(-time.Minute).UnixMilli()
	u := fmt.Sprintf("%s/fapi/v1/klines?symbol=%s&interval=1m&startTime=%d&endTime=%d&limit=1", fapiBase(), url.QueryEscape(s.Symbol), open, open+time.Minute.Milliseconds()-1)
	var raw [][]interface{}
	if err := fetchJSON(u, &raw); err != nil {
		return 0, 0, 0, err
	}
	if len(raw) != 1 || len(raw[0]) < 7 {
		return 0, 0, 0, fmt.Errorf("24h historical candle unavailable")
	}
	if int64(toF(raw[0][0])) != open {
		return 0, 0, 0, fmt.Errorf("24h historical candle misaligned")
	}
	price := toF(raw[0][4])
	labelAt := open + time.Minute.Milliseconds()
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, 0, 0, fmt.Errorf("invalid historical price")
	}
	var funding []struct {
		FundingRate string `json:"fundingRate"`
		FundingTime int64  `json:"fundingTime"`
	}
	u = fmt.Sprintf("%s/fapi/v1/fundingRate?symbol=%s&startTime=%d&endTime=%d&limit=1000", fapiBase(), url.QueryEscape(s.Symbol), s.TS+1, labelAt)
	if err := fetchJSON(u, &funding); err != nil {
		return 0, 0, 0, fmt.Errorf("historical funding: %w", err)
	}
	total := 0.0
	for _, f := range funding {
		if f.FundingTime <= s.TS || f.FundingTime > labelAt {
			continue
		}
		rate, err := strconv.ParseFloat(f.FundingRate, 64)
		if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return 0, 0, 0, fmt.Errorf("invalid historical funding")
		}
		total += rate * 100
	}
	return price, labelAt, total, nil
}

var shortWeightProposalPath = "data/shortscan_weight_proposal.json"

type ShortWeightProposal struct {
	GeneratedAt     time.Time          `json:"generated_at"`
	Incumbent       map[string]float64 `json:"incumbent"`
	Candidate       map[string]float64 `json:"candidate"`
	Samples         int                `json:"samples"`
	Applied         bool               `json:"applied"`
	EvaluationScope string             `json:"evaluation_scope"`
}
