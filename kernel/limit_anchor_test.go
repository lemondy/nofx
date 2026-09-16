package kernel

import "strings"

import "testing"

func TestCorrectLimitAnchors(t *testing.T) {
	anchors := map[string]*LimitAnchor{
		"SOLUSDT":  {LimitBuy: 106.545, LimitSell: 107.615},
		"HYPEUSDT": {LimitBuy: 87.07, LimitSell: 87.95},
	}

	t.Run("drifting long anchor snapped back (SOL prototype)", func(t *testing.T) {
		// Model computed its own 106.75876 instead of copying 106.545 (0.20% off).
		ds := []Decision{{Symbol: "SOLUSDT", Action: "open_long_limit", Price: 106.75876}}
		correctLimitAnchors(ds, anchors, LimitAnchorTolerancePct)
		if ds[0].Price != 106.545 {
			t.Fatalf("price = %.6g, want snapped to 106.545", ds[0].Price)
		}
	})

	t.Run("drifting short anchor snapped to limit_sell", func(t *testing.T) {
		ds := []Decision{{Symbol: "HYPEUSDT", Action: "open_short_limit", Price: 88.5}}
		correctLimitAnchors(ds, anchors, LimitAnchorTolerancePct)
		if ds[0].Price != 87.95 {
			t.Fatalf("price = %.6g, want snapped to 87.95", ds[0].Price)
		}
	})

	t.Run("within tolerance keeps the model price", func(t *testing.T) {
		p := 106.545 * 1.0003 // +0.03% — inside the 0.05% tolerance
		ds := []Decision{{Symbol: "SOLUSDT", Action: "open_long_limit", Price: p}}
		correctLimitAnchors(ds, anchors, LimitAnchorTolerancePct)
		if ds[0].Price != p {
			t.Fatalf("price %.6g was modified inside tolerance", ds[0].Price)
		}
	})

	t.Run("non-limit actions and unknown symbols untouched", func(t *testing.T) {
		ds := []Decision{
			{Symbol: "SOLUSDT", Action: "open_long", Price: 999},
			{Symbol: "ZZZUSDT", Action: "open_long_limit", Price: 1.23},
			{Symbol: "SOLUSDT", Action: "hold"},
		}
		correctLimitAnchors(ds, anchors, LimitAnchorTolerancePct)
		if ds[0].Price != 999 || ds[1].Price != 1.23 {
			t.Fatal("non-limit or anchor-less decisions must pass through")
		}
	})

	t.Run("zero price fails safe without panicking", func(t *testing.T) {
		// Placeholder zero from the "unknown → 0" output rule: with a valid
		// anchor the model intent is unambiguous — snap to the shown anchor
		// instead of failing the whole batch in validateDecision.
		ds := []Decision{{Symbol: "SOLUSDT", Action: "open_long_limit", Price: 0}}
		correctLimitAnchors(ds, anchors, LimitAnchorTolerancePct)
		if ds[0].Price != 106.545 {
			t.Fatalf("price = %.6g, want snapped to anchor 106.545", ds[0].Price)
		}
	})

	t.Run("suppressed anchor downgrades the limit open to wait", func(t *testing.T) {
		// Review 2026-09-07: entry_rule_triggered stays true while the anchor
		// is 0 — a model-invented price must not reach the exchange.
		suppressed := map[string]*LimitAnchor{
			"CLUSDT": {LimitBuy: 0, LimitSell: 93.1},
		}
		ds := []Decision{{Symbol: "CLUSDT", Action: "open_long_limit", Price: 90.5, Stage: "TRIGGERED"}}
		correctLimitAnchors(ds, suppressed, LimitAnchorTolerancePct)
		if ds[0].Action != "wait" {
			t.Fatalf("action = %q, want downgraded to wait", ds[0].Action)
		}
		if ds[0].Price != 0 {
			t.Fatalf("price = %.6g, want 0 after downgrade", ds[0].Price)
		}
		// Stage is no longer set by the downgrade — it derives at validation
		// from wait_bias + blocking_factors (schema-redundancy audit 09-13).
		if len(ds[0].BlockingFactors) == 0 || ds[0].BlockingFactors[0] != "ANCHOR_SUPPRESSED" {
			t.Fatalf("downgrade must carry the ANCHOR_SUPPRESSED tag: %v", ds[0].BlockingFactors)
		}
		if st := DeriveWaitStage(ds[0].WaitBias, ds[0].BlockingFactors); st != "WATCH" {
			t.Fatalf("derived stage = %q, want WATCH after downgrade", st)
		}
		if len(ds[0].NoTradeReasons) == 0 {
			t.Fatal("downgraded decision must carry a no_trade_reason")
		}
	})

	t.Run("suppressed short anchor downgrades short limit too", func(t *testing.T) {
		suppressed := map[string]*LimitAnchor{
			"CLUSDT": {LimitBuy: 91.8, LimitSell: 0},
		}
		ds := []Decision{{Symbol: "CLUSDT", Action: "open_short_limit", Price: 93.4}}
		correctLimitAnchors(ds, suppressed, LimitAnchorTolerancePct)
		if ds[0].Action != "wait" || ds[0].Price != 0 {
			t.Fatalf("got action=%q price=%.6g, want wait/0 after downgrade", ds[0].Action, ds[0].Price)
		}
	})

	t.Run("other side of a suppressed anchor stays tradable", func(t *testing.T) {
		suppressed := map[string]*LimitAnchor{
			"CLUSDT": {LimitBuy: 0, LimitSell: 93.1},
		}
		// 0.32% drift on the valid side → normal snap still applies; the
		// point is the action survives (only the suppressed side downgrades).
		ds := []Decision{{Symbol: "CLUSDT", Action: "open_short_limit", Price: 93.4}}
		correctLimitAnchors(ds, suppressed, LimitAnchorTolerancePct)
		if ds[0].Action != "open_short_limit" {
			t.Fatalf("short side with a valid anchor must not be downgraded, got %q", ds[0].Action)
		}
		if ds[0].Price != 93.1 {
			t.Fatalf("price = %.6g, want snapped to valid anchor 93.1", ds[0].Price)
		}
	})
}

// Suppression downgrade must preserve the intended direction as wait_bias
// (NO VALID ENTRY semantics — the model wanted long, the anchor was blocked;
// the direction verdict must not be re-phrased as "not bullish").
func TestCorrectLimitAnchorsSetsWaitBias(t *testing.T) {
	anchors := map[string]*LimitAnchor{"NEARUSDT": {LimitBuy: 0, LimitSell: 0}}
	decs := []Decision{
		{Symbol: "NEARUSDT", Action: "open_long_limit", Price: 3.1},
		{Symbol: "NEARUSDT", Action: "open_short_limit", Price: 3.1},
	}
	correctLimitAnchors(decs, anchors, LimitAnchorTolerancePct)
	if decs[0].Action != "wait" || decs[0].WaitBias != "long" {
		t.Fatalf("long downgrade: action=%s wait_bias=%q, want wait/long", decs[0].Action, decs[0].WaitBias)
	}
	if decs[1].Action != "wait" || decs[1].WaitBias != "short" {
		t.Fatalf("short downgrade: action=%s wait_bias=%q, want wait/short", decs[1].Action, decs[1].WaitBias)
	}
}

// Cross-scanner conflict detection (audit 2026-09-12 #11): short_scan(short)
// vs piggy_dash(up) on the same symbol must flag; aligned or single-scanner
// cases must not.
func TestHasScannerConflict(t *testing.T) {
	both := []string{"short_scan", "piggy_dash"}
	if !hasScannerConflict(both, "up") {
		t.Fatal("short_scan + piggy up = conflict")
	}
	if hasScannerConflict(both, "down") {
		t.Fatal("piggy down agrees with short_scan — no conflict")
	}
	if hasScannerConflict([]string{"piggy_dash"}, "up") {
		t.Fatal("piggy alone cannot conflict")
	}
	if hasScannerConflict([]string{"short_scan", "ai500"}, "up") {
		t.Fatal("short_scan without piggy cannot conflict")
	}
}


// New position-management actions: enum acceptance + required-field guards.
func TestValidateManagementActions(t *testing.T) {
	d := Decision{Symbol: "T", Action: "adjust_stop_loss", StopLoss: 0}
	if err := validateDecision(&d, 100, 3, 3, 1, 1, 12, false, nil); err == nil || !strings.Contains(err.Error(), "requires stop_loss") {
		t.Fatalf("adjust_stop_loss without stop_loss must fail: %v", err)
	}
	d.StopLoss = 101
	if err := validateDecision(&d, 100, 3, 3, 1, 1, 12, false, nil); err != nil {
		t.Fatalf("adjust_stop_loss with price must pass: %v", err)
	}
	p := Decision{Symbol: "T", Action: "partial_close_long", CloseFraction: 0.6}
	if err := validateDecision(&p, 100, 3, 3, 1, 1, 12, false, nil); err == nil || !strings.Contains(err.Error(), "close_fraction") {
		t.Fatalf("fraction > 0.5 must fail: %v", err)
	}
	p.CloseFraction = 0.5
	if err := validateDecision(&p, 100, 3, 3, 1, 1, 12, false, nil); err != nil {
		t.Fatalf("fraction 0.5 must pass: %v", err)
	}
	p.Action = "partial_close_short"
	p.CloseFraction = 0
	if err := validateDecision(&p, 100, 3, 3, 1, 1, 12, false, nil); err == nil {
		t.Fatal("fraction 0 must fail")
	}
}

// Wait stage derivation + the downgrade path carries the machine tag so the
// derived stage lands on WATCH (not NO_SETUP).
func TestDeriveWaitStage(t *testing.T) {
	if got := DeriveWaitStage("", nil); got != "NO_SETUP" {
		t.Fatalf("no bias → NO_SETUP, got %s", got)
	}
	if got := DeriveWaitStage("long", []string{"ANCHOR_SUPPRESSED"}); got != "WATCH" {
		t.Fatalf("bias + substantive blocker → WATCH, got %s", got)
	}
	if got := DeriveWaitStage("short", []string{"WAIT_PULLBACK"}); got != "READY" {
		t.Fatalf("bias + pullback-only → READY, got %s", got)
	}
	if got := DeriveWaitStage("long", nil); got != "READY" {
		t.Fatalf("bias + no blockers → READY, got %s", got)
	}

	anchors := map[string]*LimitAnchor{"NEARUSDT": {LimitBuy: 0, LimitSell: 0}}
	decs := []Decision{{Symbol: "NEARUSDT", Action: "open_long_limit", Price: 3.1}}
	correctLimitAnchors(decs, anchors, LimitAnchorTolerancePct)
	if decs[0].Action != "wait" || decs[0].WaitBias != "long" {
		t.Fatalf("downgrade lost bias: %+v", decs[0])
	}
	if got := DeriveWaitStage(decs[0].WaitBias, decs[0].BlockingFactors); got != "WATCH" {
		t.Fatalf("downgraded decision must derive WATCH, got %s (blockers %v)", got, decs[0].BlockingFactors)
	}
}
