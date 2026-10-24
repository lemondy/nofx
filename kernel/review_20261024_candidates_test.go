package kernel

import (
	"testing"
	"time"

	"nofx/market/breakout"
	"nofx/provider/vergex"
	"nofx/store"
)

func TestReviewC1PiggyFiltersWholeBoard(t *testing.T) {
	s := breakout.DefaultScheduler()
	t.Cleanup(func() { s.SetSnapshotForTest(nil) })
	s.SetSnapshotForTest([]breakout.ScanResult{
		{Symbol: "AUSDT", Direction: breakout.DirUp, Grade: "strong", Pattern: breakout.PatternApproach},
		{Symbol: "BUSDT", Direction: breakout.DirUp, Grade: "weak", Pattern: breakout.PatternBreakout},
		{Symbol: "CUSDT", Direction: breakout.DirUp, Grade: "noise", Pattern: breakout.PatternBreakout},
		{Symbol: "OTHERUSDT", Direction: breakout.DirDown, Grade: "strong", Pattern: breakout.PatternBreakout},
		{Symbol: "GOODUSDT", Direction: breakout.DirUp, Grade: "medium", Pattern: breakout.PatternBreakout},
	})
	got, err := (&StrategyEngine{}).getPiggyDashCoins(1, breakout.DirUp, "medium")
	if err != nil || len(got) != 1 || got[0].Symbol != "GOODUSDT" {
		t.Fatalf("qualified row beyond 3x limit lost: %v, %v", got, err)
	}
}

func TestReviewC1AI500SmallLimitDoesNotShrinkFallback(t *testing.T) {
	setupAI500Fallback(t)
	all := []string{"BTCUSDT", "ETHUSDT", "SOLUSDT", "XRPUSDT"}
	ai500Fetch = func(_ *StrategyEngine) (vergex.AI500Snapshot, error) {
		return vergex.AI500Snapshot{Symbols: all, FetchedAt: ai500Now(), ExpiresAt: ai500Now().Add(ai500FallbackTTL)}, nil
	}
	e := &StrategyEngine{}
	if _, err := e.getAI500Coins(4); err != nil {
		t.Fatal(err)
	}
	got, err := e.getAI500Coins(1)
	if err != nil {
		t.Fatal(err)
	}
	assertAI500Candidates(t, got, all[:1])
	scriptAI500Fetch(nil, errAI500Vergex403)
	got, err = e.getAI500Coins(4)
	if err != nil {
		t.Fatal(err)
	}
	assertAI500Candidates(t, got, all)
}

func TestReviewC1AI500RelayHitDoesNotRenewAge(t *testing.T) {
	clock := setupAI500Fallback(t)
	*clock = time.Now().Add(29 * time.Minute)
	const path = "/trending-category?key=ai500&lang=en"
	vergex.FeedRelay(path, []byte(`{"category":{"assets":[{"pair":"BTCUSDT"}]}}`))
	t.Cleanup(func() { vergex.FeedRelay(path, []byte(`{"category":{"assets":[]}}`)) })
	e := &StrategyEngine{vergexClient: vergex.NewClient()}
	if _, err := e.getAI500Coins(10); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(2 * time.Minute)
	scriptAI500Fetch(nil, errAI500Vergex403)
	if got, err := e.getAI500Coins(10); err == nil {
		t.Fatalf("31-minute-old relay data renewed in kernel: %v", got)
	}
}

func TestReviewC1AI500FallbackHonorsEarlierSourceDeadline(t *testing.T) {
	clock := setupAI500Fallback(t)
	start := *clock
	ai500Fetch = func(_ *StrategyEngine) (vergex.AI500Snapshot, error) {
		return vergex.AI500Snapshot{Symbols: []string{"BTCUSDT"}, FetchedAt: start, ExpiresAt: start.Add(5 * time.Minute)}, nil
	}
	e := &StrategyEngine{}
	if _, err := e.getAI500Coins(1); err != nil {
		t.Fatal(err)
	}
	*clock = start.Add(4 * time.Minute)
	if _, err := e.getAI500Coins(1); err != nil {
		t.Fatal(err)
	}
	scriptAI500Fetch(nil, errAI500Vergex403)
	if _, err := e.getAI500Coins(1); err != nil {
		t.Fatal(err)
	}
	*clock = start.Add(5 * time.Minute)
	if got, err := e.getAI500Coins(1); err == nil {
		t.Fatalf("source deadline renewed: %v", got)
	}
}

func TestReviewC1CandidateWhitespaceExclusionAndDedupe(t *testing.T) {
	e := NewStrategyEngine(&store.StrategyConfig{CoinSource: store.CoinSourceConfig{
		SourceType: "static", StaticCoins: []string{" BTC ", "btcusdt", " ETHUSDT ", "eth"},
		ExcludedCoins: []string{" BTCUSDT \t"},
	}})
	got, err := e.GetCandidateCoins()
	if err != nil || len(got) != 1 || got[0].Symbol != "ETHUSDT" {
		t.Fatalf("exclusion/dedupe mismatch: %v, %v", got, err)
	}
}
