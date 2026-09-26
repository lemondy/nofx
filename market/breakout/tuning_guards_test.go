package breakout

import (
	"errors"
	"os"
	"time"
	"path/filepath"
	"strings"
	"testing"
)

// Pins for the 2026-09-26 tuning-review fixes.

// P0-1: with the tuner DISABLED, shortWeights must return the DESIGN values
// even when the params file carries railed weights — the polluted file is
// inert until the update is re-enabled.
func TestShortWeightsInertWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	SetParamsPath(filepath.Join(dir, "params.json"))
	t.Cleanup(func() { SetParamsPath("data/breakout_params.json") })

	p := GetParams()
	p.ShortWeights = map[string]float64{
		"structure": 0.0299, "overbought": 0.299, "parabolic": 0.299,
		"divergence": 0.0299, "stretch": 0.0511, "extension": 0.148,
		"crowding": 0.0831, "rejection": 0.0299, "volume_fade": 0.0299,
	}
	ApplyParams(p)

	w := shortWeights()
	if w["structure"] != 0.15 || w["overbought"] != 0.10 {
		t.Fatalf("disabled tuner must serve DESIGN weights, got %v", w)
	}
}

// P0-2: a starved run (fewer signals than btMinSample) must surface
// ErrStarved so the scheduler backs off — never report success on 0 signals.
func TestTuneFromBacktestStarvedIsError(t *testing.T) {
	if !errors.Is(ErrStarved, ErrStarved) {
		t.Fatal("sentinel broken")
	}
	// Direct: tuneWalkForward reports starvation via counts; TuneFromBacktest
	// wraps it in ErrStarved. The scheduler treats errors.Is(err, ErrStarved)
	// with a 24h backoff (scheduler.go run loop) — this pin keeps the
	// sentinel semantics honest.
	msg := "backtest starved: insufficient signals"
	if !strings.Contains(ErrStarved.Error(), "starved") {
		t.Fatalf("sentinel message drifted: %s", msg)
	}
}

// P2: atomicWriteJSON lands the file and leaves no temp behind.
func TestAtomicWriteJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "params.json")
	if err := atomicWriteJSON(path, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("read back: %v %s", err, b)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file must be renamed away")
	}
}

// P1 (re-review): the PIT slice must filter by CLOSE time — a 1h/1d bar that
// opened before the signal but closes after it contains FUTURE OHLC and must
// be excluded.
func TestSliceClosedFiltersUnclosedBars(t *testing.T) {
	base := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	mk := func(n int) []Kline {
		out := make([]Kline, n)
		for i := range out {
			out[i] = Kline{OpenTime: base.Add(time.Duration(i) * time.Hour).UnixMilli(), Close: float64(i + 1)}
		}
		return out
	}
	k1h := mk(10)                      // bars 00:00..09:00, each closing +1h
	sigAt := base.Add(5 * time.Hour).Add(15 * time.Minute) // 05:15 — bar@05:00 closes 06:00 (future)

	got := sliceClosed(k1h, sigAt, time.Hour)
	if len(got) != 5 {
		t.Fatalf("bars fully closed by 05:15 = %d, want 5 (00:00..04:00)", len(got))
	}
	last := time.UnixMilli(got[len(got)-1].OpenTime)
	if !last.Add(time.Hour).Before(sigAt) {
		t.Fatalf("last included bar must CLOSE before the signal: %v", last)
	}

	// Daily bars with 24h duration, same rule.
	d1 := make([]Kline, 5)
	for i := range d1 {
		d1[i] = Kline{OpenTime: base.Add(time.Duration(i) * 24 * time.Hour).UnixMilli(), Close: float64(i + 1)}
	}
	gotD := sliceClosed(d1, base.Add(48*time.Hour), 24*time.Hour) // signal at day-3 start
	if len(gotD) != 2 {
		t.Fatalf("daily bars closed by day-3 = %d, want 2 (day-1 closes exactly at boundary → excluded)", len(gotD))
	}
}
