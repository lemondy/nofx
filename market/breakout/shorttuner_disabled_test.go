package breakout

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Pin for the 2026-09-22 tuner disable (QUANT_REVIEW A3): the weight update
// must be OFF unless short_tuner_enabled is explicitly true, and when disabled
// RunShortTuner must evaluate outcomes but never write weights.
func TestShortTunerUpdateDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	SetParamsPath(filepath.Join(dir, "params.json"))
	t.Cleanup(func() { SetParamsPath("data/breakout_params.json") })

	// No short_tuner_enabled key at all → disabled.
	if shortTunerUpdateEnabled() {
		t.Fatal("tuner update must be disabled when short_tuner_enabled is unset")
	}

	on := true
	p := GetParams()
	p.ShortTunerEnabled = &on
	ApplyParams(p)
	if !shortTunerUpdateEnabled() {
		t.Fatal("tuner update must be enabled when short_tuner_enabled=true")
	}
	off := false
	p.ShortTunerEnabled = &off
	ApplyParams(p)
	if shortTunerUpdateEnabled() {
		t.Fatal("tuner update must be disabled when short_tuner_enabled=false")
	}
}

// With the update disabled, RunShortTuner must not touch ShortWeights even
// when a matured, perfectly-correlated cohort exists — but outcome evaluation
// must still run (the journal is the future re-tuner's dataset).
func TestRunShortTunerNoWeightWriteWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "signals.jsonl")
	SetShortTuningPath(journal)
	SetParamsPath(filepath.Join(dir, "params.json"))
	t.Cleanup(func() {
		SetShortTuningPath("data/shortscan_signals.jsonl")
		SetParamsPath("data/breakout_params.json")
	})

	// Stub the ticker endpoint RunShortTuner's evaluation uses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"symbol": "TESTUSDT", "lastPrice": "110", "priceChangePercent": "10", "quoteVolume": "1000000"},
		})
	}))
	defer srv.Close()
	t.Setenv("BINANCE_FAPI_BASE", srv.URL)

	before := shortWeights()

	// 30 matured samples (older than the 24h eval window) with a perfect
	// positive correlation between one component and the outcome — the old
	// rule would have moved weights immediately.
	base := time.Now().Add(-48 * time.Hour).UnixMilli()
	var lines []byte
	for i := 0; i < 30; i++ {
		s := shortSample{
			TS:     base + int64(i)*1000,
			Symbol: "TESTUSDT",
			Score:  60,
			Price:  100,
			Components: map[string]float64{
				"structure": float64(i), // monotonic → |corr| = 1
			},
		}
		b, _ := json.Marshal(s)
		lines = append(lines, b...)
		lines = append(lines, '\n')
	}
	if err := os.WriteFile(journal, lines, 0o644); err != nil {
		t.Fatal(err)
	}

	RunShortTuner(time.Now())

	after := shortWeights()
	for _, k := range shortWeightKeys {
		if after[k] != before[k] {
			t.Fatalf("weight %s moved while tuner disabled: %v → %v", k, before[k], after[k])
		}
	}

	// Evaluation must still have happened (journal keeps accumulating value).
	samples := readSamples()
	evaluated := 0
	for _, s := range samples {
		if s.Evaluated {
			evaluated++
		}
	}
	if evaluated != 30 {
		t.Fatalf("expected 30 evaluated samples with update disabled, got %d", evaluated)
	}
}
