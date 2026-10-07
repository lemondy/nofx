package kernel

import (
	"reflect"
	"testing"
)

// 2026-10-07 review: mixed candidates render in a deterministic order.
func TestSortMixedCandidatesDeterministic(t *testing.T) {
	in := []CandidateCoin{
		{Symbol: "ZUSDT", Sources: []string{"ai500"}},
		{Symbol: "BUSDT", Sources: []string{"piggy_dash", "short_scan"}},
		{Symbol: "AUSDT", Sources: []string{"short_scan"}},
	}
	sortMixedCandidates(in)
	got := []string{in[0].Symbol, in[1].Symbol, in[2].Symbol}
	if want := []string{"BUSDT", "AUSDT", "ZUSDT"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v want %v", got, want)
	}
}

// S2: the nearest structural level is reported even when it fails min_rr,
// while first_rr_ge_target still skips to the qualifying one.
func TestRRScanReportsFirstObstacle(t *testing.T) {
	tfs := map[string]*TFSignal{
		"1h": {StructuralResistance: []float64{101, 106}},
	}
	out := scanRRWindowWithCost(100, "live_price", 2, 98, tfs, true, 1.5, 0, 1<<62, 0)
	if out.FirstObstacle != 101 || out.FirstObstacleRR != 0.5 {
		t.Fatalf("first obstacle=%v rr=%v, want 101 / 0.5", out.FirstObstacle, out.FirstObstacleRR)
	}
	if out.FirstRRGeTarget != 106 {
		t.Fatalf("first_rr_ge_target=%v, want 106", out.FirstRRGeTarget)
	}
}
