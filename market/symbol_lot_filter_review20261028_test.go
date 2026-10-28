package market

import (
	"math"
	"testing"
)

// Review 2026-10-28 A5: the minimum tradable notional must round the
// MIN_NOTIONAL-implied quantity UP to the step grid. Previously only MinQty
// was grid-aligned, so (step 0.1, notional 5, price 26) quoted 5U — but
// 5/26 = 0.19 rounds DOWN to 0.1 when ordering (2.6U notional) and the
// exchange rejects the order. The placeable floor is 0.2 → 5.2U.
func TestReviewA5MinTradableNotional(t *testing.T) {
	tests := []struct {
		name  string
		f     SymbolLotFilter
		price float64
		want  float64
	}{
		{
			name:  "report example: notional-implied qty rounds up to grid",
			f:     SymbolLotFilter{StepSize: 0.1, MinQty: 0.1, MinNotional: 5},
			price: 26,
			want:  5.2,
		},
		{
			name:  "notional-implied qty rounds up even with minQty=0",
			f:     SymbolLotFilter{StepSize: 0.1, MinNotional: 5},
			price: 26,
			want:  5.2,
		},
		{
			name:  "notional already on grid unchanged",
			f:     SymbolLotFilter{StepSize: 0.5, MinQty: 0.5, MinNotional: 5},
			price: 10,
			want:  5,
		},
		{
			name:  "minQty dominates",
			f:     SymbolLotFilter{StepSize: 1, MinQty: 1, MinNotional: 5},
			price: 26,
			want:  26,
		},
		{
			name:  "QNTUSDT floor stays 26",
			f:     SymbolLotFilter{StepSize: 0.1, MinQty: 0.1, MinNotional: 5},
			price: 260,
			want:  26,
		},
		{
			name:  "no stepSize: legacy max(notional, minQty*price)",
			f:     SymbolLotFilter{MinQty: 0.1, MinNotional: 5},
			price: 26,
			want:  5,
		},
		{
			name:  "non-positive price: legacy passthrough",
			f:     SymbolLotFilter{StepSize: 0.1, MinQty: 0.1, MinNotional: 5},
			price: 0,
			want:  5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.MinTradableNotional(tc.price)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("MinTradableNotional(%v) = %v, want %v", tc.price, got, tc.want)
			}
		})
	}
}

// Fine-grained grids must never quote below the exchange floor, and the
// quoted floor must sit on the step grid.
func TestReviewA5MinTradableNotionalFloatEdge(t *testing.T) {
	f := SymbolLotFilter{StepSize: 0.001, MinQty: 0.001, MinNotional: 5}
	got := f.MinTradableNotional(3)
	if got < 5 {
		t.Fatalf("MinTradableNotional(3) = %v, must be >= MIN_NOTIONAL 5", got)
	}
	steps := got / f.StepSize
	if math.Abs(steps-math.Round(steps)) > 1e-6 {
		t.Fatalf("MinTradableNotional(3) = %v is not on the %v grid (steps=%v)", got, f.StepSize, steps)
	}
	if got != f.MinTradableNotional(3) {
		t.Fatalf("MinTradableNotional(3) is not deterministic: %v vs %v", got, f.MinTradableNotional(3))
	}
}
