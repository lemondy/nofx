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
	// Forty alternate-day blocks, each with ten distinct symbols. Structure
	// predicts better shorts within each day; overbought predicts worse shorts.
	samples := reviewRankICSamples(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 40, 10)
	out, ok := updateShortWeights(samples, base, shortTunerEta)
	if !ok {
		t.Fatal("strong correlations must pass the significance gate")
	}
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
