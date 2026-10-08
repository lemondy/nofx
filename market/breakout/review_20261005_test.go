package breakout

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAudit05ResampleMustAlignToExchangeHours(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 15, 0, 0, time.UTC)
	var bars []Kline
	for i := 0; i < 8; i++ {
		bars = append(bars, Kline{OpenTime: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), Open: 100, High: 101, Low: 99, Close: 100})
	}
	out := resample1h(bars)
	for _, b := range out {
		if b.OpenTime%time.Hour.Milliseconds() != 0 {
			t.Errorf("synthetic 1h bar starts %s instead of UTC-hour boundary", time.UnixMilli(b.OpenTime).UTC())
		}
	}
}

func TestAudit05PersistFailureMustKeepLiveParams(t *testing.T) {
	paramsMu.Lock()
	oldPath, oldLoaded, oldParams := paramsPath, paramsLoaded, currentParams
	paramsMu.Unlock()
	t.Cleanup(func() {
		paramsMu.Lock()
		paramsPath, paramsLoaded, currentParams = oldPath, oldLoaded, oldParams
		paramsMu.Unlock()
	})
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	paramsMu.Lock()
	paramsPath = filepath.Join(blocker, "params.json")
	paramsLoaded = true
	currentParams = defaultParams()
	paramsMu.Unlock()
	before := GetParams()
	next := before
	next.StrongThreshold = 85
	if err := ApplyParamsChecked(next); err == nil {
		t.Fatal("fixture persist unexpectedly succeeded")
	}
	if GetParams().StrongThreshold != before.StrongThreshold {
		t.Error("failed persistence still changes production in-memory parameters")
	}
}

func TestAudit05WeakCorrelationMustNotBeCalledSignificant(t *testing.T) {
	samples := reviewRankICSamples(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 30, 10)
	// Seventeen positive and thirteen negative block ICs: mean IC is positive,
	// but its t is below 1, so sufficient block count alone must not pass.
	for i := range samples {
		block, symbol := i/10, i%10
		samples[i].Components = map[string]float64{"stretch": float64(symbol)}
		if block < 17 {
			samples[i].Outcome = float64(symbol)
		} else {
			samples[i].Outcome = -float64(symbol)
		}
	}
	_, stats, ok := updateShortWeightsWithDiagnostics(samples, DefaultShortWeights(), shortTunerEta)
	stat := stats["stretch"]
	if ok || stat.N != 30 || stat.T == nil || math.Abs(*stat.T) >= 1 {
		t.Errorf("weak mean rank IC incorrectly admitted: %+v", stat)
	}
}
