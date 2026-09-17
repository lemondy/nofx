package breakout

import "testing"

// A symbol in BOTH the gainer ranking and the slow-top screen keeps its
// gainer entry but must carry NearHighAlso — the pool cut exempts it from
// the gainer-side OI floor (the whole point of the near_high exemption).
// Before the fix the collision silently dropped the flag and the floor ate
// the coin (user audit 2026-09-17).
func TestMergeShortScansCollisionKeepsExemption(t *testing.T) {
	gainers := []ShortSignal{
		{Symbol: "PUMPUSDT", Universe: "gainer", Score: 80},
		{Symbol: "GRINDUSDT", Universe: "gainer", Score: 60, OIValueMillions: 9}, // also a grinding top
		{Symbol: "OTHERUSDT", Universe: "gainer", Score: 40},
	}
	slowTops := []ShortSignal{
		{Symbol: "GRINDUSDT", Universe: "near_high", Score: 60, OIValueMillions: 9},
		{Symbol: "PUREGRINDUSDT", Universe: "near_high", Score: 58},
	}
	out := mergeShortScans(gainers, slowTops)
	if len(out) != 4 {
		t.Fatalf("merged len = %d, want 4", len(out))
	}
	bySym := map[string]ShortSignal{}
	for _, s := range out {
		bySym[s.Symbol] = s
	}
	g := bySym["GRINDUSDT"]
	if g.Universe != "gainer" || !g.NearHighAlso {
		t.Fatalf("collision: universe=%q nearHighAlso=%v — want gainer label kept + flag set", g.Universe, g.NearHighAlso)
	}
	if bySym["PUREGRINDUSDT"].Universe != "near_high" || bySym["PUREGRINDUSDT"].NearHighAlso {
		t.Fatalf("pure slow-top entry mangled: universe=%q nearHighAlso=%v (own near_high needs no flag)", bySym["PUREGRINDUSDT"].Universe, bySym["PUREGRINDUSDT"].NearHighAlso)
	}
	if bySym["PUMPUSDT"].NearHighAlso {
		t.Fatal("non-colliding gainer must not carry the flag")
	}
}

// AnalyzeShort must record the latest OI notional so callers can apply the
// strategy's min-OI liquidity threshold before taking the top-N candidates.
func TestAnalyzeShortRecordsOIValue(t *testing.T) {
	k := make([]Kline, 0, 60)
	for i := 0; i < 60; i++ {
		k = append(k, mkCandle(100+float64(i), 1000, 50))
	}
	m := &mockDS{
		series:  map[string][]Kline{"1h": k, "4h": k},
		oi:      []OIPoint{{OI: 250, Value: 25_000_000, TS: 1}, {OI: 260, Value: 26_000_000, TS: 2}},
		funding: []FundingPoint{{Rate: 0.0001, TS: 1}},
	}
	sig, err := AnalyzeShort("TESTUSDT", 5, nil, m)
	if err != nil {
		t.Fatalf("AnalyzeShort failed: %v", err)
	}
	if sig.OIValueMillions != 26 {
		t.Fatalf("OIValueMillions = %v, want 26", sig.OIValueMillions)
	}
	// mockDS pads the series to the requested 84 bars ⇒ 14 days.
	if sig.ListingDays != 14 {
		t.Fatalf("ListingDays = %v, want 14", sig.ListingDays)
	}
}

// A pump whose price makes a higher high while RSI prints a lower high must
// be flagged as a 4h bearish divergence.
func TestAnalyzeShortDetectsBearishDivergence(t *testing.T) {
	k4h := make([]Kline, 0, 84)
	price := 100.0
	// 54 slow-drift bars
	for i := 0; i < 54; i++ {
		price *= 1.001
		k4h = append(k4h, mkCandle(price, 800, 50))
	}
	// 14 rally bars (+1% each): RSI hits 100 at the local top (~134.9)
	for i := 0; i < 14; i++ {
		price *= 1.01
		k4h = append(k4h, mkCandle(price, 1000, 55))
	}
	// 10-bar pullback (-1.5% each)
	for i := 0; i < 10; i++ {
		price *= 0.985
		k4h = append(k4h, mkCandle(price, 600, 30))
	}
	// 6 grind bars (+4% each): price reclaims the prior high (higher high)
	// but the RSI recovery is weak — bearish divergence.
	for i := 0; i < 6; i++ {
		price *= 1.04
		k4h = append(k4h, mkCandle(price, 900, 60))
	}

	k1h := make([]Kline, 0, 60)
	p1 := 100.0
	for i := 0; i < 60; i++ {
		p1 *= 1.005
		k1h = append(k1h, mkCandle(p1, 1000, 50))
	}

	m := &mockDS{
		series:  map[string][]Kline{"1h": k1h, "4h": k4h},
		funding: []FundingPoint{{Rate: 0.0002, TS: 1}},
	}
	sig, err := AnalyzeShort("TESTUSDT", 15, nil, m)
	if err != nil {
		t.Fatalf("AnalyzeShort failed: %v", err)
	}
	if !sig.BearishDiv4h {
		t.Fatalf("expected 4h bearish divergence, got %+v", sig.Components)
	}
	if !sig.Confirmed {
		t.Fatalf("divergence should mark the signal confirmed")
	}
}
