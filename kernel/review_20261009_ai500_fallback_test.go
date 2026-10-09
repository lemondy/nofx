package kernel

import (
	"errors"
	"testing"
	"time"
)

// 2026-10-09: transient vergex 403 dropped the AI500 source for a whole cycle.
// getAI500Coins serves the last-good list for up to ai500FallbackTTL after a
// vergex failure; these tests pin the TTL window, the error passthrough, the
// limit truncation and the copy-on-store.

var errAI500Vergex403 = errors.New("vergex returned status 403 for /trending-category?key=ai500")

// setupAI500Fallback clears the package-level last-good cache, installs a
// controllable clock and returns a pointer to it. Overrides are restored on
// cleanup.
func setupAI500Fallback(t *testing.T) *time.Time {
	t.Helper()

	origFetch, origNow := ai500Fetch, ai500Now
	ai500LastGood.mu.Lock()
	origSymbols, origAt := ai500LastGood.symbols, ai500LastGood.at
	ai500LastGood.symbols, ai500LastGood.at = nil, time.Time{}
	ai500LastGood.mu.Unlock()

	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	ai500Now = func() time.Time { return now }
	t.Cleanup(func() {
		ai500Fetch, ai500Now = origFetch, origNow
		ai500LastGood.mu.Lock()
		ai500LastGood.symbols, ai500LastGood.at = origSymbols, origAt
		ai500LastGood.mu.Unlock()
	})
	return &now
}

// scriptAI500Fetch makes the next vergex fetch return symbols and err.
func scriptAI500Fetch(symbols []string, err error) {
	ai500Fetch = func(_ *StrategyEngine, _ int) ([]string, error) {
		return symbols, err
	}
}

// assertAI500Candidates checks the candidate symbols in order and that each
// one carries Sources [ai500].
func assertAI500Candidates(t *testing.T, got []CandidateCoin, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d: %+v", len(got), len(want), got)
	}
	for i, c := range got {
		if c.Symbol != want[i] {
			t.Fatalf("candidate %d = %s, want %s", i, c.Symbol, want[i])
		}
		if len(c.Sources) != 1 || c.Sources[0] != "ai500" {
			t.Fatalf("%s Sources = %v, want [ai500]", c.Symbol, c.Sources)
		}
	}
}

func TestAI500FallbackServesLastGoodWithinTTL(t *testing.T) {
	clock := setupAI500Fallback(t)
	e := &StrategyEngine{}

	scriptAI500Fetch([]string{"BTCUSDT", "ETHUSDT", "SOLUSDT"}, nil)
	got, err := e.getAI500Coins(10)
	if err != nil {
		t.Fatalf("healthy fetch: %v", err)
	}
	assertAI500Candidates(t, got, []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"})

	*clock = clock.Add(14*time.Minute + 59*time.Second)
	scriptAI500Fetch(nil, errAI500Vergex403)
	got, err = e.getAI500Coins(10)
	if err != nil {
		t.Fatalf("vergex failure within TTL: err = %v, want nil (last-good fallback)", err)
	}
	assertAI500Candidates(t, got, []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"})
}

func TestAI500FallbackExpiresAfterTTL(t *testing.T) {
	clock := setupAI500Fallback(t)
	e := &StrategyEngine{}

	scriptAI500Fetch([]string{"BTCUSDT"}, nil)
	if _, err := e.getAI500Coins(10); err != nil {
		t.Fatalf("healthy fetch: %v", err)
	}

	*clock = clock.Add(16 * time.Minute)
	scriptAI500Fetch(nil, errAI500Vergex403)
	got, err := e.getAI500Coins(10)
	if err != errAI500Vergex403 {
		t.Fatalf("vergex failure after TTL: err = %v, want the original vergex error", err)
	}
	if got != nil {
		t.Fatalf("vergex failure after TTL returned %d candidates, want none", len(got))
	}
}

func TestAI500FallbackWithoutPriorSuccessReturnsError(t *testing.T) {
	setupAI500Fallback(t)
	scriptAI500Fetch(nil, errAI500Vergex403)

	got, err := (&StrategyEngine{}).getAI500Coins(10)
	if err != errAI500Vergex403 {
		t.Fatalf("no cached list: err = %v, want the original vergex error", err)
	}
	if got != nil {
		t.Fatalf("no cached list returned %d candidates, want none", len(got))
	}
}

func TestAI500FallbackTruncatesToLimit(t *testing.T) {
	clock := setupAI500Fallback(t)
	e := &StrategyEngine{}

	scriptAI500Fetch([]string{"BTCUSDT", "ETHUSDT", "SOLUSDT", "XRPUSDT", "DOGEUSDT"}, nil)
	if _, err := e.getAI500Coins(10); err != nil {
		t.Fatalf("healthy fetch: %v", err)
	}

	*clock = clock.Add(time.Minute)
	scriptAI500Fetch(nil, errAI500Vergex403)
	got, err := e.getAI500Coins(3)
	if err != nil {
		t.Fatalf("vergex failure within TTL: err = %v, want nil", err)
	}
	assertAI500Candidates(t, got, []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"})
}

func TestAI500FallbackCachesCopyOfSuccess(t *testing.T) {
	setupAI500Fallback(t)
	e := &StrategyEngine{}

	fetched := []string{"BTCUSDT", "ETHUSDT"}
	scriptAI500Fetch(fetched, nil)
	if _, err := e.getAI500Coins(10); err != nil {
		t.Fatalf("healthy fetch: %v", err)
	}
	fetched[0] = "MUTATEDUSDT" // the caller reuses its slice; the cache keeps its own copy

	scriptAI500Fetch(nil, errAI500Vergex403)
	got, err := e.getAI500Coins(10)
	if err != nil {
		t.Fatalf("vergex failure within TTL: err = %v, want nil", err)
	}
	assertAI500Candidates(t, got, []string{"BTCUSDT", "ETHUSDT"})
}

// An empty 200 response must not wipe a usable last-good list.
func TestAI500FallbackEmptySuccessKeepsLastGood(t *testing.T) {
	clock := setupAI500Fallback(t)
	e := &StrategyEngine{}

	scriptAI500Fetch([]string{"BTCUSDT", "ETHUSDT"}, nil)
	if _, err := e.getAI500Coins(10); err != nil {
		t.Fatalf("healthy fetch: %v", err)
	}
	*clock = clock.Add(5 * time.Minute)
	scriptAI500Fetch([]string{}, nil)
	if _, err := e.getAI500Coins(10); err != nil {
		t.Fatalf("empty fetch: %v", err)
	}
	*clock = clock.Add(5 * time.Minute)
	scriptAI500Fetch(nil, errAI500Vergex403)
	got, err := e.getAI500Coins(10)
	if err != nil {
		t.Fatalf("failure after empty success: err = %v, want last-good fallback", err)
	}
	assertAI500Candidates(t, got, []string{"BTCUSDT", "ETHUSDT"})
}
