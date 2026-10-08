package breakout

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	oldProposal := shortWeightProposalPath
	shortWeightProposalPath = filepath.Join(dir, "proposal.json")
	SetShortTuningPath(journal)
	SetParamsPath(filepath.Join(dir, "params.json"))
	t.Cleanup(func() {
		SetShortTuningPath("data/shortscan_signals.jsonl")
		shortWeightProposalPath = oldProposal
		SetParamsPath("data/breakout_params.json")
	})

	// Stub historical HTTP in memory, preserving labelling coverage without
	// requiring a listening socket.
	originalClient := binanceHTTP
	binanceHTTP = &http.Client{Transport: fix06Transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/fapi/v1/fundingRate" {
			return fix06Response("[]")
		}
		open, _ := strconv.ParseInt(r.URL.Query().Get("startTime"), 10, 64)
		body, err := json.Marshal([][]interface{}{{open, "100", "110", "99", "110", "100", open + 59999}})
		if err != nil {
			return nil, err
		}
		return fix06Response(string(body))
	})}
	t.Cleanup(func() { binanceHTTP = originalClient })
	t.Setenv("BINANCE_FAPI_BASE", "https://short-label.test")

	before := shortWeights()
	now := time.Now().UTC()
	// Keep the disabled guard meaningful under rank IC: this already-labelled
	// cohort would produce a proposal if research were enabled.
	cohort := reviewRankICSamples(now.Add(-82*24*time.Hour), 40, 10)
	if _, ok := updateShortWeights(cohort, before, shortTunerEta); !ok {
		t.Fatal("disabled-switch fixture must be statistically significant")
	}

	// Thirty additional matured samples still need historical labelling,
	// independent of whether proposal generation is enabled.
	base := time.Now().Add(-48 * time.Hour).UnixMilli()
	var lines []byte
	for _, sample := range cohort {
		b, _ := json.Marshal(sample)
		lines = append(lines, b...)
		lines = append(lines, '\n')
	}
	for i := 0; i < 30; i++ {
		s := shortSample{
			TS:     base + int64(i)*1000,
			Symbol: "TESTUSDT",
			Score:  60,
			Price:  100,
			Components: map[string]float64{
				"structure": float64(i),
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
		if s.Symbol == "TESTUSDT" && s.Evaluated {
			evaluated++
		}
	}
	if evaluated != 30 {
		t.Fatalf("expected 30 evaluated samples with update disabled, got %d", evaluated)
	}
	if _, err := os.Stat(shortWeightProposalPath); !os.IsNotExist(err) {
		t.Fatalf("disabled research must not write a proposal: %v", err)
	}
}
