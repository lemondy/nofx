package trader

import (
	"testing"

	"nofx/store"
)

// Trigger price band: buy limits below market (pullback), sell limits above
// (rally), 0.1%-5% distance; anything else rejected.
func TestValidateLimitEntryPrice(t *testing.T) {
	live := 100.0
	cases := []struct {
		name    string
		action  string
		limit   float64
		wantErr bool
	}{
		{"long pullback 2%", "open_long_limit", 98, false},
		{"long shallow 0.5%", "open_long_limit", 99.5, false},
		{"long at/above market", "open_long_limit", 100.05, true},
		{"long too far 6%", "open_long_limit", 93.9, true},
		{"short rally 2%", "open_short_limit", 102, false},
		{"short at/below market", "open_short_limit", 99.9, true},
		{"short too far 6%", "open_short_limit", 106.1, true},
		{"zero price", "open_long_limit", 0, true},
	}
	for _, tc := range cases {
		err := validateLimitEntryPrice(tc.action, tc.limit, live)
		if tc.wantErr && err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
	}
}

// Far-away half of the band: an uncrossed anchor more than 5% from market is
// stale and must be rejected — the old fall-through placed it anyway.
func TestLimitEntryTooFar(t *testing.T) {
	live := 100.0
	cases := []struct {
		name   string
		action string
		limit  float64
		want   bool
	}{
		{"long 6% below", "open_long_limit", 93.9, true},
		{"long exactly 5% below", "open_long_limit", 95.0, false},
		{"long 0.05% below (near market)", "open_long_limit", 99.95, false},
		{"short 6% above", "open_short_limit", 106.1, true},
		{"short exactly 5% above", "open_short_limit", 105.0, false},
		{"short 0.05% above (near market)", "open_short_limit", 100.05, false},
		{"zero price", "open_long_limit", 0, true},
	}
	for _, tc := range cases {
		if got := limitEntryTooFar(tc.action, tc.limit, live); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Anchor crossing: the AI latency window can move the market through the
// pre-computed anchor. A crossed anchor means the planned pullback/rally
// arrived (convert to market); a crossed stop means the setup is dead.
func TestAnchorCrossedByMarket(t *testing.T) {
	cases := []struct {
		name          string
		side          string
		anchor, live  float64
		sl            float64
		wantCrossed   bool
		wantSLCrossed bool
	}{
		{"long pullback arrived", "long", 100.5, 100.2, 99, true, false},
		{"long deeper than stop", "long", 100.5, 98.8, 99, true, true},
		{"long at anchor boundary", "long", 100.5, 100.5, 99, true, false},
		{"long not crossed", "long", 100.5, 101.0, 99, false, false},
		{"short rally arrived", "short", 99.5, 99.8, 101, true, false},
		{"short beyond stop", "short", 99.5, 101.2, 101, true, true},
		{"short at anchor boundary", "short", 99.5, 99.5, 101, true, false},
		{"short not crossed", "short", 99.5, 99.0, 101, false, false},
		{"zero SL never crossed", "short", 99.5, 102.0, 0, false, false},
	}
	for _, tc := range cases {
		crossed, slCrossed := anchorCrossedByMarket(tc.side, tc.anchor, tc.live, tc.sl)
		if crossed != tc.wantCrossed || slCrossed != tc.wantSLCrossed {
			t.Errorf("%s: got crossed=%v slCrossed=%v, want %v/%v", tc.name, crossed, slCrossed, tc.wantCrossed, tc.wantSLCrossed)
		}
	}
}

// Fallback switch: nil (unset) defaults to enabled; explicit false disables.
func TestLimitEntryMarketFallbackDefault(t *testing.T) {
	at := &AutoTrader{}
	if !at.limitEntryMarketFallbackEnabled() {
		t.Error("nil StrategyConfig must default to enabled")
	}
	on := true
	off := false
	at.config = AutoTraderConfig{StrategyConfig: &store.StrategyConfig{}}
	at.config.StrategyConfig.RiskControl.LimitEntryMarketFallback = &on
	if !at.limitEntryMarketFallbackEnabled() {
		t.Error("explicit true must stay enabled")
	}
	at.config.StrategyConfig.RiskControl.LimitEntryMarketFallback = &off
	if at.limitEntryMarketFallbackEnabled() {
		t.Error("explicit false must disable the fallback")
	}
}

// Pending state lifecycle via the map helpers.
func TestPendingEntryLifecycle(t *testing.T) {
	at := &AutoTrader{}
	if at.getPendingEntry("SOLUSDT") != nil {
		t.Fatal("empty map must return nil")
	}
	at.setPendingEntry(&pendingEntry{Symbol: "SOLUSDT", Side: "long", Price: 103, OrderID: "123"})
	if pe := at.getPendingEntry("SOLUSDT"); pe == nil || pe.OrderID != "123" {
		t.Fatalf("set/get broken: %+v", pe)
	}
	// Replace semantics: same symbol, new order.
	at.setPendingEntry(&pendingEntry{Symbol: "SOLUSDT", Side: "long", Price: 102, OrderID: "456"})
	if pe := at.getPendingEntry("SOLUSDT"); pe == nil || pe.OrderID != "456" {
		t.Fatalf("replace broken: %+v", pe)
	}
	at.dropPendingEntry("SOLUSDT")
	if at.getPendingEntry("SOLUSDT") != nil {
		t.Fatal("drop broken")
	}
}
