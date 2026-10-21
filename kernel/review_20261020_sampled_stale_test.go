package kernel

import (
	"strings"
	"testing"
	"time"

	"nofx/market"
)

func sampledTestData(lastBarOpen time.Time, sampledAt time.Time) *market.Data {
	d := &market.Data{
		Symbol: "CAPUSDT", CurrentPrice: 0.0632,
		FundingRate: 0.0001, FundingRateOK: true,
		TimeframeData: map[string]*market.TimeframeSeriesData{},
		SampledAt:     sampledAt,
	}
	d.TimeframeData["1h"] = buildTF("1h", lastBarOpen, 60, 0.0632, false)
	return d
}

// Review 2026-10-20 #4: a bar that was still forming when the klines were
// SAMPLED (14:59:30) must stay unclosed when the signal is recomputed after the
// boundary (15:00:05); only snapshots without a sampling time fall back to the
// caller's clock.
func TestClosedCandleJudgedAtSamplingTime(t *testing.T) {
	sampled := time.Date(2026, 10, 20, 14, 59, 30, 0, time.UTC)
	later := time.Date(2026, 10, 20, 15, 0, 5, 0, time.UTC)
	barOpen := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC) // forming 14:00-15:00

	withSample := sampledTestData(barOpen, sampled)
	sig, err := ComputeSymbolSignals("CAPUSDT", withSample, SignalOptions{Now: later, PrimaryTF: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	tf := sig.Timeframes["1h"]
	if !tf.UnclosedDropped || tf.BarsUsed != 59 {
		t.Fatalf("sampled 14:59:30: forming bar must be dropped even when recomputed at 15:00:05; dropped=%v bars=%d", tf.UnclosedDropped, tf.BarsUsed)
	}

	// Compatibility: no SampledAt -> caller's clock, the legacy behaviour.
	legacy := sampledTestData(barOpen, time.Time{})
	sig, err = ComputeSymbolSignals("CAPUSDT", legacy, SignalOptions{Now: later, PrimaryTF: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if tf := sig.Timeframes["1h"]; tf.UnclosedDropped || tf.BarsUsed != 60 {
		t.Fatalf("zero SampledAt must fall back to opt.Now; dropped=%v bars=%d", tf.UnclosedDropped, tf.BarsUsed)
	}

	// TimeframeTrend (trader hard gates call it with the wall clock) also judges
	// at the sampling time: the helper picks SampledAt over the fallback.
	if got := snapshotNow(withSample, later); !got.Equal(sampled) {
		t.Errorf("snapshotNow = %v, want sampling time", got)
	}
	if got := snapshotNow(legacy, later); !got.Equal(later) {
		t.Errorf("snapshotNow fallback = %v, want caller clock", got)
	}
}

func staleGateCode(g *DirectionGate) string {
	for _, c := range g.Failed {
		if strings.HasPrefix(c, "DATA_STALE_") {
			return c
		}
	}
	return ""
}

// Review 2026-10-20 #8: a snapshot whose newest bar is hours behind the
// sampling time blocks new entries in BOTH directions; a fresh one does not.
func TestStaleBarTimeBlocksEntries(t *testing.T) {
	sampled := time.Date(2026, 10, 20, 12, 0, 30, 0, time.UTC)
	opt := SignalOptions{Now: sampled, PrimaryTF: "1h"}

	stale := sampledTestData(sampled.Add(-6*time.Hour), sampled)
	sig, err := ComputeSymbolSignals("CAPUSDT", stale, opt)
	if err != nil {
		t.Fatal(err)
	}
	if sig.DataFreshness == nil || len(sig.DataFreshness.StaleTimeframes) != 1 || sig.DataFreshness.StaleTimeframes[0] != "1h" {
		t.Fatalf("stale_timeframes = %+v", sig.DataFreshness)
	}
	if staleGateCode(sig.HardGate.Long) == "" || staleGateCode(sig.HardGate.Short) == "" {
		t.Fatalf("stale feed must fail the gate both ways: long=%v short=%v", sig.HardGate.Long.Failed, sig.HardGate.Short.Failed)
	}
	if sig.HardGate.Long.Allowed || sig.HardGate.Short.Allowed {
		t.Error("stale feed must not leave any direction allowed")
	}
	found := false
	for _, w := range sig.Warnings {
		if strings.Contains(w, "DATA STALE") {
			found = true
		}
	}
	if !found {
		t.Errorf("no DATA STALE warning: %v", sig.Warnings)
	}

	fresh := sampledTestData(sampled.Add(-30*time.Minute), sampled) // forming 11:30-12:30 bar
	sig, err = ComputeSymbolSignals("CAPUSDT", fresh, opt)
	if err != nil {
		t.Fatal(err)
	}
	if sig.DataFreshness != nil && len(sig.DataFreshness.StaleTimeframes) != 0 {
		t.Fatalf("fresh feed flagged stale: %v", sig.DataFreshness.StaleTimeframes)
	}
	if c := staleGateCode(sig.HardGate.Long); c != "" {
		t.Errorf("fresh feed carries %s", c)
	}

	// Hand-built snapshots without a sampling time are never judged on wall
	// clock age (fixtures / backtests stay valid).
	legacy := sampledTestData(sampled.Add(-6*time.Hour), time.Time{})
	sig, err = ComputeSymbolSignals("CAPUSDT", legacy, opt)
	if err != nil {
		t.Fatal(err)
	}
	if c := staleGateCode(sig.HardGate.Long); c != "" {
		t.Errorf("zero SampledAt must disable the staleness gate, got %s", c)
	}
}
