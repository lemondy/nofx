package trader

import (
	"testing"

	"nofx/store"
)

// review 2026-10-08 C: no_data rows (exit 0) polluted sum_r/avg_r, so the
// resolved-shadow log must not print an R for them.
func TestShadowRLabelNoDataIsNA(t *testing.T) {
	long := &store.GateShadowBlock{Direction: "long", EntryPrice: 100, StopPrice: 95}
	short := &store.GateShadowBlock{Direction: "short", EntryPrice: 100, StopPrice: 105}
	cases := []struct {
		name    string
		row     *store.GateShadowBlock
		outcome string
		exit    float64
		want    string
	}{
		{"long no_data", long, "no_data", 0, "n/a"},
		{"short no_data", short, "no_data", 0, "n/a"},
		{"long timeout exit 0", long, "timeout", 0, "n/a"},
		{"long tp_first", long, "tp_first", 110, "2.00R"},
		{"short sl_first", short, "sl_first", 105, "-1.00R"},
		{"short timeout at entry", short, "timeout", 100, "0.00R"},
		{"long risk <= 0", &store.GateShadowBlock{Direction: "long", EntryPrice: 100, StopPrice: 105}, "tp_first", 110, "n/a"},
	}
	for _, tc := range cases {
		if got := shadowRLabel(tc.row, tc.outcome, tc.exit); got != tc.want {
			t.Errorf("%s: shadowRLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
}
