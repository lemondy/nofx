package breakout

import (
	"testing"
	"time"
)

func TestNear90dHigh(t *testing.T) {
	mk := func(high, last float64, n int) []Kline {
		k := make([]Kline, n)
		for i := range k {
			k[i] = Kline{High: high, Close: last}
		}
		return k
	}
	if !near90dHigh(mk(100, 96.5, 90), 5.0) {
		t.Fatal("3.5% below the 90d high must qualify")
	}
	if near90dHigh(mk(100, 94.0, 90), 5.0) {
		t.Fatal("6% below the high must not qualify")
	}
	if near90dHigh(mk(100, 96.5, 5), 5.0) {
		t.Fatal("too-short series must not qualify (fail-closed on data)")
	}
	if near90dHigh(nil, 5.0) {
		t.Fatal("nil series must not qualify")
	}
}

func TestShortWeightsDefaultsAndNormalization(t *testing.T) {
	w := shortWeights()
	if len(w) != 9 {
		t.Fatalf("want 9 component weights, got %d", len(w))
	}
	total := 0.0
	for _, k := range shortWeightKeys {
		total += w[k]
	}
	if total < 0.9999 || total > 1.0001 {
		t.Fatalf("weights must sum to 1, got %.4f", total)
	}
	// Audit 2026-09-12: overbought yields to structure (RSI-pinned-high is a
	// trend feature, not a reversal signal) — structure is now the 0.15 pole.
	if w["overbought"] != 0.10 || w["structure"] != 0.15 || w["crowding"] != 0.15 {
		t.Fatalf("designed defaults drifted: %v", w)
	}
}

func TestUpdateShortWeights(t *testing.T) {
	base := DefaultShortWeights()
	now := time.Now()

	// Cohort: high "structure" scores predicted profitable shorts (positive
	// outcome), high "overbought" predicted pain (negative outcome).
	var samples []shortSample
	for i := 0; i < 20; i++ {
		structure := 80.0
		overbought := 20.0
		outcome := 3.0 // profitable short
		if i%2 == 1 {  // half the cohort with opposite readings/outcomes
			structure, overbought = 20.0, 80.0
			outcome = -2.0
		}
		_ = now
		samples = append(samples, shortSample{
			Evaluated: true,
			Outcome:   outcome,
			Components: map[string]float64{
				"structure":  structure,
				"overbought": overbought,
				"stretch":    50,
			},
		})
	}
	out := updateShortWeights(samples, base, shortTunerEta)
	if out["structure"] <= base["structure"] {
		t.Fatalf("predictive component must gain weight: %.3f → %.3f", base["structure"], out["structure"])
	}
	if out["overbought"] >= base["overbought"] {
		t.Fatalf("counter-predictive component must lose weight: %.3f → %.3f", base["overbought"], out["overbought"])
	}
	// Unseen component keeps its base share (corr 0 → exp(0) = 1 before norm).
	if out["stretch"] < 0.02 {
		t.Fatalf("zero-correlation weight collapsed: %.3f", out["stretch"])
	}
	// Renormalized.
	total := 0.0
	for _, k := range shortWeightKeys {
		total += out[k]
		if out[k] < 0.03-1e-9 || out[k] > 0.30+1e-9 {
			t.Fatalf("weight %s = %.3f outside clamp [0.03, 0.30]", k, out[k])
		}
	}
	if total < 0.999 || total > 1.001 {
		t.Fatalf("weights must renormalize to 1, got %.4f", total)
	}
}
