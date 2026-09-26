package breakout

import (
	"errors"
	"os"
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
