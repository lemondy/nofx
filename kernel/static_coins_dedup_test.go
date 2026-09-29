package kernel

import (
	"testing"

	"nofx/store"
)

// 09-28 review P3: a repeated entry in the configured static coin list
// rendered twice in the prompt's candidate section — duplicated market
// data, duplicated decision budget. Case-difference normalizes to the same
// symbol too.
func TestStaticCoinCandidatesDedup(t *testing.T) {
	e := NewStrategyEngine(&store.StrategyConfig{})
	cfg := store.CoinSourceConfig{
		StaticCoins: []string{"BTCUSDT", "btcusdt", "ETHUSDT", "BTCUSDT", ""},
	}
	got := e.staticCoinCandidates(cfg)
	if len(got) != 2 {
		t.Fatalf("static candidates = %v, want exactly BTCUSDT+ETHUSDT", got)
	}
	if got[0].Symbol != "BTCUSDT" || got[1].Symbol != "ETHUSDT" {
		t.Fatalf("unexpected symbols: %s, %s", got[0].Symbol, got[1].Symbol)
	}
	for _, c := range got {
		if len(c.Sources) != 1 || c.Sources[0] != "static" {
			t.Fatalf("source tags wrong: %+v", c)
		}
	}
}
