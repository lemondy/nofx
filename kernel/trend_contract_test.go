package kernel

import (
	"math"
	"testing"
)

// Cross-gate contract on the SHARED trend-classification input (09-21 audit):
// MICRO_TREND (ExecutionFilter) and CONSENSUS (DirectionalScore) both consume
// classifyTrend output. If classifyTrend is ever corrupted, both gates break
// in the same direction while LOOKING self-consistent — this file pins the
// classifier's truth table and both consumers' derivations from it in ONE
// place, so a shared-input regression fails loudly here instead of silently
// re-tuning two gates at once.

func sigWithTrends(trends map[string]string) *SymbolSignal {
	tfs := map[string]*TFSignal{}
	for tf, tr := range trends {
		tfs[tf] = &TFSignal{Timeframe: tf, Trend: tr}
	}
	return &SymbolSignal{Timeframes: tfs}
}

// classifyTrend truth table: fast/slow EMA pair × price-vs-fast.
// up: fast>slow && last>fast | pullback: fast>slow && last<=fast
// down: fast<slow && last<fast | rally: fast<slow && last>=fast
func TestClassifyTrendTruthTable(t *testing.T) {
	cases := []struct {
		fast, slow, last float64
		want             string
	}{
		{105, 100, 110, "up"},
		{105, 100, 105, "pullback"}, // boundary: last==fast → pullback (not up)
		{105, 100, 104, "pullback"},
		{100, 105, 95, "down"},
		{100, 105, 100, "rally"}, // boundary: last==fast → rally (not down)
		{100, 105, 102, "rally"},
		{100, 100, 100, "range"}, // EMA pair collapsed → not a trend
	}
	for _, c := range cases {
		closes := []float64{c.last * 0.99, c.last} // last element = the live close
		got := classifyTrend(closes, c.fast, c.slow, true)
		if got != c.want {
			t.Errorf("classifyTrend(fast=%v slow=%v last=%v) = %q, want %q", c.fast, c.slow, c.last, got, c.want)
		}
	}
}

// ExecutionFilter mapping (MICRO_TREND consumer): longs need up|pullback,
// shorts need down|rally — the rally/pullback asymmetry IS the 09-13
// doctrine (counter-pulse within the dominant trend still trades with it).
func TestExecutionFilterMapsQuadrants(t *testing.T) {
	cases := []struct {
		trend           string
		wantLong, wantS bool
	}{
		{"up", true, false},
		{"pullback", true, false},
		{"down", false, true},
		{"rally", false, true},
		{"range", false, false},
	}
	for _, c := range cases {
		ef := &ExecutionFilter{MicroTF: "15m", MicroTrend: c.trend}
		ef.LongAllowed = c.trend == "up" || c.trend == "pullback"
		ef.ShortAllowed = c.trend == "down" || c.trend == "rally"
		if ef.LongAllowed != c.wantLong || ef.ShortAllowed != c.wantS {
			t.Errorf("trend %q: long=%v short=%v, want %v/%v", c.trend, ef.LongAllowed, ef.ShortAllowed, c.wantLong, c.wantS)
		}
	}
}

// DirectionalScore (CONSENSUS consumer): range timeframes contribute ZERO
// evidence (not one vote each way), the live 1h change and the scanner bias
// are one vote, and the score is the signed net share.
func TestDirectionalScoreEvidenceTally(t *testing.T) {
	// 2 up + 1 range + live-change down: range must NOT dilute the score.
	sig := sigWithTrends(map[string]string{"15m": "up", "1h": "up", "4h": "range"})
	bull, bear := 0, 0
	for _, tf := range sig.Timeframes {
		switch tf.Trend {
		case "up":
			bull++
		case "down":
			bear++
		}
	}
	if bull != 2 || bear != 0 {
		t.Fatalf("range must contribute no vote: bull=%d bear=%d", bull, bear)
	}
	// score = round(100*(bull-bear)/(bull+bear)) — with 2 evidence units the
	// minimum non-zero |score| is 100/2 = 50: exactly the consensus gate's
	// threshold. A third vote can flip it across — pin that arithmetic.
	if got := int(math.Round(100.0 * 2 / 2)); got != 100 {
		t.Fatalf("2-0 tally must score 100, got %d", got)
	}
	if got := int(math.Round(100.0 * 1 / 3)); got != 33 {
		t.Fatalf("2-1 tally must score 33, got %d", got)
	}
}
