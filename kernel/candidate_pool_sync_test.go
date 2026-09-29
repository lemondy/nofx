package kernel

import (
	"testing"
)

// 09-29 user report: IOTA/AZTEC appeared in the UI's candidate pool (16) but
// never in the prompt's render (14) — piggy-dash selects without an OI floor
// while the fetch pass filters with one, and the dropped coins stayed in
// ctx.CandidateCoins. The pool must now shrink in lockstep with the render.
func TestLowOISkipRemovesCandidateFromPool(t *testing.T) {
	ctx := &Context{CandidateCoins: []CandidateCoin{
		{Symbol: "AAAUSDT"},
		{Symbol: "IOTAUSDT"},
		{Symbol: "BBBUSDT"},
	}}
	removeCandidate(ctx, "IOTAUSDT")
	if len(ctx.CandidateCoins) != 2 {
		t.Fatalf("pool = %d coins, want 2", len(ctx.CandidateCoins))
	}
	for _, c := range ctx.CandidateCoins {
		if c.Symbol == "IOTAUSDT" {
			t.Fatal("skipped coin still in the pool")
		}
	}
	// Unknown symbol is a no-op (idempotent).
	removeCandidate(ctx, "NOTUSDT")
	if len(ctx.CandidateCoins) != 2 {
		t.Fatalf("no-op removal changed the pool: %d", len(ctx.CandidateCoins))
	}
	// Case normalization: lower-case raw symbols match the normalized entry.
	ctx2 := &Context{CandidateCoins: []CandidateCoin{{Symbol: "IOTAUSDT"}}}
	removeCandidate(ctx2, "iotausdt")
	if len(ctx2.CandidateCoins) != 0 {
		t.Fatal("normalized match failed")
	}
}
