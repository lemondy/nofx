package kernel

import (
	"testing"

	"nofx/store"
)

func ruleForTest(id int64, name, condJSON, onViolation string) *store.TradingRuleDB {
	return &store.TradingRuleDB{
		ID:            id,
		RuleType:      "hard",
		Name:          name,
		ConditionJSON: condJSON,
		OnViolation:   onViolation,
		Description:   name,
		Enabled:       true,
	}
}

func TestParseRuleCondition(t *testing.T) {
	if _, err := ParseRuleCondition(`{"field":"leverage","op":"<=","value":10}`); err != nil {
		t.Fatalf("valid condition rejected: %v", err)
	}
	if _, err := ParseRuleCondition(""); err == nil {
		t.Fatal("empty condition should fail")
	}
	if _, err := ParseRuleCondition(`{"field":"leverage","op":"~","value":10}`); err == nil {
		t.Fatal("unsupported operator should fail")
	}
	if _, err := ParseRuleCondition(`{"field":"","op":"<=","value":10}`); err == nil {
		t.Fatal("missing field should fail")
	}
}

func TestCheckDecisionAgainstRules_Block(t *testing.T) {
	rules := []*store.TradingRuleDB{
		ruleForTest(1, "max leverage", `{"field":"leverage","op":">","value":10}`, "block"),
		ruleForTest(2, "min confidence", `{"field":"confidence","op":"<","value":60}`, "warn"),
		ruleForTest(3, "no stop loss", `{"field":"has_stop_loss","op":"==","value":false}`, "block"),
	}
	d := Decision{
		Symbol:          "BTCUSDT",
		Action:          "open_long",
		Leverage:        20,
		PositionSizeUSD: 1000,
		Confidence:      50,
		StopLoss:        0,
	}

	violations, warnings := CheckDecisionAgainstRules(rules, d, 10000, 50000)
	if len(violations) != 2 {
		t.Fatalf("expected 2 violations (leverage>10 block, no stop-loss block), got %d: %+v", len(violations), violations)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning (confidence<60), got %d", len(warnings))
	}
	for _, v := range violations {
		if v.Action != "block" || v.Symbol != "BTCUSDT" {
			t.Fatalf("unexpected violation: %+v", v)
		}
	}
}

func TestCheckDecisionAgainstRules_Pass(t *testing.T) {
	rules := []*store.TradingRuleDB{
		ruleForTest(1, "max leverage", `{"field":"leverage","op":">","value":10}`, "block"),
		ruleForTest(2, "risk reward", `{"field":"risk_reward","op":"<","value":1.5}`, "warn"),
	}
	d := Decision{
		Symbol:          "ETHUSDT",
		Action:          "open_short",
		Leverage:        5,
		PositionSizeUSD: 500,
		Confidence:      80,
		StopLoss:        2100,
		TakeProfit:      1900,
	}
	// price 2000: SL distance 5%, TP distance 5% → risk_reward = 1
	violations, warnings := CheckDecisionAgainstRules(rules, d, 10000, 2000)
	if len(violations) != 0 {
		t.Fatalf("expected no violations, got %+v", violations)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning for risk_reward=1 < 1.5, got %d", len(warnings))
	}
}

func TestCheckDecisionAgainstRules_CloseAndHoldSkipped(t *testing.T) {
	rules := []*store.TradingRuleDB{
		ruleForTest(1, "block everything", `{"field":"leverage","op":">=","value":0}`, "block"),
	}
	for _, action := range []string{"close_long", "close_short", "hold", "wait"} {
		d := Decision{Symbol: "BTCUSDT", Action: action, Leverage: 10}
		violations, _ := CheckDecisionAgainstRules(rules, d, 10000, 0)
		if len(violations) != 0 {
			t.Fatalf("action %s should not be checked, got %+v", action, violations)
		}
	}
}

func TestCheckDecisionAgainstRules_MalformedRuleSkipped(t *testing.T) {
	rules := []*store.TradingRuleDB{
		{ID: 1, RuleType: "hard", Name: "broken", ConditionJSON: `{invalid`, OnViolation: "block", Enabled: true},
	}
	d := Decision{Symbol: "BTCUSDT", Action: "open_long", Leverage: 100}
	violations, _ := CheckDecisionAgainstRules(rules, d, 10000, 0)
	if len(violations) != 0 {
		t.Fatalf("malformed rule should be skipped, got %+v", violations)
	}
}

func TestPositionValuePct(t *testing.T) {
	rules := []*store.TradingRuleDB{
		ruleForTest(1, "max position pct", `{"field":"position_value_pct","op":">","value":20}`, "block"),
	}
	d := Decision{Symbol: "BTCUSDT", Action: "open_long", Leverage: 5, PositionSizeUSD: 3000}
	violations, _ := CheckDecisionAgainstRules(rules, d, 10000, 0)
	if len(violations) != 1 {
		t.Fatalf("3000/10000 = 30%% should violate >20%%, got %+v", violations)
	}
}

func TestMatchSoftRules(t *testing.T) {
	rules := []*store.TradingRuleDB{
		{ID: 1, RuleType: "soft", Name: "fomo", Tags: "fomo,chase", LessonText: "high leverage + chasing historically wins 15%", Enabled: true},
		{ID: 2, RuleType: "soft", Name: "btc only", Tags: "BTCUSDT", LessonText: "BTC mean reversion works better", Enabled: true},
		{ID: 3, RuleType: "soft", Name: "disabled", Tags: "", LessonText: "should not appear", Enabled: false},
	}
	d := Decision{Symbol: "BTCUSDT", Action: "open_long"}
	matched := MatchSoftRules(rules, d)
	if len(matched) != 2 {
		t.Fatalf("expected generic + BTC rule to match (disabled excluded), got %d: %+v", len(matched), matched)
	}
	d2 := Decision{Symbol: "ETHUSDT", Action: "open_long"}
	matched2 := MatchSoftRules(rules, d2)
	if len(matched2) != 1 {
		t.Fatalf("ETH should only match generic rule, got %d", len(matched2))
	}
}

func TestBuildRulesPromptText(t *testing.T) {
	rules := []*store.TradingRuleDB{
		ruleForTest(1, "max leverage", `{"field":"leverage","op":">","value":10}`, "block"),
		{ID: 2, RuleType: "soft", Name: "fomo", LessonText: "avoid FOMO", SourceStats: "win_rate=15%", Enabled: true},
		{ID: 3, RuleType: "hard", Name: "off", ConditionJSON: `{"field":"leverage","op":">","value":1}`, OnViolation: "warn", Enabled: false},
	}
	text := BuildRulesPromptText(rules)
	if text == "" {
		t.Fatal("expected non-empty rules prompt")
	}
	if !contains(text, "max leverage") || !contains(text, "avoid FOMO") {
		t.Fatalf("prompt missing rules: %s", text)
	}
	if ruleTestContains(text, "off") {
		t.Fatalf("disabled rule should be excluded: %s", text)
	}
	// empty rules → empty prompt
	if BuildRulesPromptText(nil) != "" {
		t.Fatal("expected empty prompt for no rules")
	}
}

func ruleTestContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
