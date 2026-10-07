package kernel

import (
	"strings"
	"testing"
)

// review 2026-10-07 B1-6: "~" / thousand separators are only illegal in
// numeric positions, never inside free-text strings.
func TestValidateJSONFormatIgnoresStringLiterals(t *testing.T) {
	ok := `[{"symbol":"BTCUSDT","action":"wait","reasoning":"~80% odds, 1.5×ATR1h~上限, size 1,234 \"~\" x"}]`
	if err := validateJSONFormat(ok); err != nil {
		t.Fatalf("~ / 1,234 inside a string must pass: %v", err)
	}
	bad := `[{"symbol":"BTCUSDT","action":"open_long","stop_loss":~100}]`
	if err := validateJSONFormat(bad); err == nil {
		t.Fatalf("~ in numeric position must fail")
	}
	bad2 := `[{"symbol":"BTCUSDT","action":"open_long","position_size_usd":1,234}]`
	if err := validateJSONFormat(bad2); err == nil {
		t.Fatalf("thousand separator in numeric position must fail")
	}
}

func TestParseBatchKeepsValidDecisionsAndShowsRejection(t *testing.T) {
	resp := "```json\n[" +
		`{"symbol":"ETHUSDT","action":"close_long","reasoning":"exit ~80%"},` +
		`{"symbol":"SOLUSDT","action":"open_long","leverage":3,"position_size_usd":1,"stop_loss":90,"take_profit":120}` +
		"]\n```"
	fd, err := parseFullDecisionResponse(resp, 1000, 10, 5, 0.5, 0.5, 12,
		map[string]bool{"ETHUSDT": true}, nil, nil)
	if err != nil {
		t.Fatalf("batch with one valid decision must not error: %v", err)
	}
	if len(fd.Decisions) != 2 {
		t.Fatalf("want 2 decisions, got %d", len(fd.Decisions))
	}
	if fd.Decisions[0].Action != "close_long" {
		t.Fatalf("valid close must survive, got %s", fd.Decisions[0].Action)
	}
	r := fd.Decisions[1]
	if r.Action != "wait" || r.Symbol != "SOLUSDT" {
		t.Fatalf("rejected open must become wait, got %+v", r)
	}
	if len(r.NoTradeReasons) == 0 || !strings.HasPrefix(r.NoTradeReasons[0], "VALIDATION_REJECTED") ||
		!strings.Contains(r.NoTradeReasons[0], "too small") {
		t.Fatalf("rejection not visible: %v", r.NoTradeReasons)
	}
}

func TestParseBatchRejectedOnHeldSymbolBecomesHold(t *testing.T) {
	resp := `[{"symbol":"ETHUSDT","action":"no_action"},{"symbol":"BTCUSDT","action":"wait"}]`
	fd, err := parseFullDecisionResponse(resp, 1000, 10, 5, 0.5, 0.5, 12,
		map[string]bool{"ETHUSDT": true}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fd.Decisions[0].Action != "hold" || !strings.HasPrefix(fd.Decisions[0].NoTradeReasons[0], "VALIDATION_REJECTED") {
		t.Fatalf("held symbol rejection must become hold: %+v", fd.Decisions[0])
	}
}

func TestParseBatchPlaceholderStopCorrectedNotRejected(t *testing.T) {
	resp := `[{"symbol":"SOLUSDT","action":"open_long","leverage":3,"position_size_usd":100,"stop_loss":0,"take_profit":0}]`
	gates := map[string]*GateState{"SOLUSDT": {LongStopPlanPrice: 95, LongTakeProfit: 120}}
	fd, err := parseFullDecisionResponse(resp, 1000, 10, 5, 0.5, 0.5, 12, nil, gates, nil)
	if err != nil {
		t.Fatalf("placeholder SL/TP must be corrected, not rejected: %v", err)
	}
	d := fd.Decisions[0]
	if d.Action != "open_long" || d.StopLoss != 95 || d.TakeProfit != 120 {
		t.Fatalf("expected open_long SL 95 TP 120, got %+v", d)
	}
}

func TestParseBatchAllInvalidStillErrors(t *testing.T) {
	resp := `[{"symbol":"SOLUSDT","action":"open_long","leverage":3,"position_size_usd":1,"stop_loss":90,"take_profit":120},{"symbol":"X","action":"no_action"}]`
	if _, err := parseFullDecisionResponse(resp, 1000, 10, 5, 0.5, 0.5, 12, nil, nil, nil); err == nil {
		t.Fatalf("all-invalid batch must error")
	}
}
