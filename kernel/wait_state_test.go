package kernel

import (
	"strings"
	"testing"
)

// Trade state machine (review 2026-09-15 point 11): wait_state is dataset
// annotation — invalid/contradicting tags are STRIPPED, never batch-fatal —
// and an explicit state overrides the derived stage while back-filling the
// direction into wait_bias (one verdict, two views).

func TestWaitStateHygiene(t *testing.T) {
	// Valid directional state back-fills wait_bias and maps onto the stage.
	d := Decision{Symbol: "CHIPUSDT", Action: "wait", WaitState: "WATCH_SHORT", NextTrigger: "0.03854 破位确认 + RECHECK_ALL_HARD_GATES"}
	if err := validateDecision(&d, 52, 10, 5, 10, 1.5, 12, false, nil); err != nil {
		t.Fatalf("valid wait_state rejected: %v", err)
	}
	if d.WaitBias != "short" {
		t.Errorf("wait_bias = %q, want back-filled short", d.WaitBias)
	}
	if d.Stage != "READY" {
		// 09-16: the declared state no longer wins — with no gate snapshot the
		// stage falls back to the bias/blockers rule (short bias, no
		// substantive blocker → READY).
		t.Errorf("stage = %q, want READY (declared state is ignored)", d.Stage)
	}

	// Contradicting state (WATCH_LONG under a short bias) is stripped; the
	// bias survives, the stage falls back to derivation.
	d2 := Decision{Symbol: "XUSDT", Action: "wait", WaitBias: "short", WaitState: "WATCH_LONG", BlockingFactors: []string{"TIMING_GATE"}}
	if err := validateDecision(&d2, 52, 10, 5, 10, 1.5, 12, false, nil); err != nil {
		t.Fatalf("conflicting wait_state must not error: %v", err)
	}
	if d2.WaitState != "" {
		t.Errorf("conflicting state not stripped: %q", d2.WaitState)
	}
	if d2.WaitBias != "short" {
		t.Errorf("wait_bias damaged by the strip: %q", d2.WaitBias)
	}
	if d2.Stage != "WATCH" {
		t.Errorf("fallback stage = %q, want WATCH (DeriveWaitStage)", d2.Stage)
	}

	// Unknown enum value is stripped silently.
	d3 := Decision{Symbol: "XUSDT", Action: "wait", WaitState: "READY_WE"}
	if err := validateDecision(&d3, 52, 10, 5, 10, 1.5, 12, false, nil); err != nil {
		t.Fatalf("unknown wait_state must not error: %v", err)
	}
	if d3.WaitState != "" {
		t.Errorf("unknown state survived: %q", d3.WaitState)
	}

	// BLOCKED keeps its direction as a WATCH under the stage mapping
	// (ZEC case: short bias stands, the block is entry mechanics) — the
	// derived BLOCKED comes from the gate snapshot now, not the declaration.
	gatesBlocked := map[string]*GateState{"zecusdt": {LongAllowed: false, ShortAllowed: false}}
	d4 := Decision{Symbol: "ZECUSDT", Action: "wait", WaitBias: "short", WaitState: "BLOCKED"}
	if err := validateDecision(&d4, 52, 10, 5, 10, 1.5, 12, false, gatesBlocked["zecusdt"]); err != nil {
		t.Fatalf("blocked+state rejected: %v", err)
	}
	if d4.WaitState != "BLOCKED" {
		t.Errorf("derived wait_state = %q, want BLOCKED (both gates failed)", d4.WaitState)
	}
	if d4.Stage != "WATCH" {
		t.Errorf("BLOCKED with a surviving bias → stage %q, want WATCH", d4.Stage)
	}

	// Non-wait actions never carry the annotation.
	d5 := Decision{Symbol: "XUSDT", Action: "hold", WaitState: "READY_LONG", NextTrigger: "junk"}
	if err := validateDecision(&d5, 52, 10, 5, 10, 1.5, 12, false, nil); err != nil {
		t.Fatalf("hold rejected: %v", err)
	}
	if d5.WaitState != "" || d5.NextTrigger != "" {
		t.Errorf("hold carried wait annotation: %q %q", d5.WaitState, d5.NextTrigger)
	}

	// next_trigger is clamped to the dataset column width.
	long := "事" + strings.Repeat("x", 400)
	gatesOpen := map[string]*GateState{"xusdt": {LongAllowed: true, ShortAllowed: false}}
	d6 := Decision{Symbol: "XUSDT", Action: "wait", WaitBias: "long", WaitState: "WATCH_LONG", NextTrigger: long}
	if err := validateDecision(&d6, 52, 10, 5, 10, 1.5, 12, false, gatesOpen["xusdt"]); err != nil {
		t.Fatalf("wait rejected: %v", err)
	}
	if d6.WaitState != "READY_LONG" {
		t.Errorf("derived wait_state = %q, want READY_LONG (long gate allowed)", d6.WaitState)
	}
	if r := []rune(d6.NextTrigger); len(r) > 180 {
		t.Errorf("next_trigger not clamped: %d runes", len(r))
	}
}

func TestWaitStateToStageMapping(t *testing.T) {
	cases := []struct {
		state, bias, want string
	}{
		{"READY_LONG", "long", "READY"},
		{"READY_SHORT", "short", "READY"},
		{"WATCH_LONG", "long", "WATCH"},
		{"BLOCKED", "short", "WATCH"},
		{"BLOCKED", "", "NO_SETUP"},
		{"", "short", ""},
	}
	for _, c := range cases {
		if got := WaitStateToStage(c.state, c.bias); got != c.want {
			t.Errorf("WaitStateToStage(%q,%q) = %q, want %q", c.state, c.bias, got, c.want)
		}
	}
}
