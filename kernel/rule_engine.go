package kernel

import (
	"encoding/json"
	"fmt"
	"strings"

	"nofx/store"
)

// RuleCondition one machine-evaluable rule condition (stored as JSON in trading_rules.condition_json)
// Supported fields:
//   - leverage            (float) decision leverage
//   - position_size_usd   (float) position value in USDT
//   - position_value_pct  (float) position value as % of account equity
//   - stop_loss_pct       (float) distance from entry to stop-loss in %
//   - take_profit_pct     (float) distance from entry to take-profit in %
//   - risk_reward         (float) take_profit_pct / stop_loss_pct
//   - confidence          (float) AI confidence 0-100
//   - symbol              (string) trading symbol
//   - has_stop_loss       (bool)
//   - has_take_profit     (bool)
//
// Supported operators: >, >=, <, <=, ==, !=, in (comma list for strings)
type RuleCondition struct {
	Field string      `json:"field"`
	Op    string      `json:"op"`
	Value interface{} `json:"value"`
}

// RuleViolation one fired rule
type RuleViolation struct {
	RuleID   int64  `json:"rule_id"`
	RuleName string `json:"rule_name"`
	RuleType string `json:"rule_type"` // hard|soft
	Action   string `json:"action"`    // block|warn
	Message  string `json:"message"`
	Symbol   string `json:"symbol"`
	Decision string `json:"decision"` // open_long|open_short|...
	Detail   string `json:"detail"`   // matched condition description
}

// ParseRuleCondition validates and parses a condition JSON string
func ParseRuleCondition(conditionJSON string) (*RuleCondition, error) {
	conditionJSON = strings.TrimSpace(conditionJSON)
	if conditionJSON == "" {
		return nil, fmt.Errorf("empty condition")
	}
	var cond RuleCondition
	if err := json.Unmarshal([]byte(conditionJSON), &cond); err != nil {
		return nil, fmt.Errorf("invalid condition JSON: %w", err)
	}
	if cond.Field == "" {
		return nil, fmt.Errorf("condition field is required")
	}
	if cond.Op == "" {
		return nil, fmt.Errorf("condition operator is required")
	}
	switch cond.Op {
	case ">", ">=", "<", "<=", "==", "!=", "in":
	default:
		return nil, fmt.Errorf("unsupported operator: %s", cond.Op)
	}
	return &cond, nil
}

// decisionContext numeric/string facts extracted from a decision for rule evaluation
type decisionContext struct {
	symbol          string
	action          string
	leverage        float64
	positionSizeUSD float64
	positionValuePc float64
	stopLossPc      float64
	takeProfitPc    float64
	riskReward      float64
	confidence      float64
	hasStopLoss     bool
	hasTakeProfit   bool
}

// buildDecisionContext extracts evaluable facts from a decision.
// price: current/reference price (0 = unknown, percentage conditions skipped)
func buildDecisionContext(d Decision, equity float64, price float64) decisionContext {
	c := decisionContext{
		symbol:          d.Symbol,
		action:          d.Action,
		leverage:        float64(d.Leverage),
		positionSizeUSD: d.PositionSizeUSD,
		confidence:      float64(d.Confidence),
		hasStopLoss:     d.StopLoss > 0,
		hasTakeProfit:   d.TakeProfit > 0,
	}
	if equity > 0 {
		c.positionValuePc = d.PositionSizeUSD / equity * 100
	}
	if price > 0 {
		if d.StopLoss > 0 {
			c.stopLossPc = absPct(price, d.StopLoss)
		}
		if d.TakeProfit > 0 {
			c.takeProfitPc = absPct(price, d.TakeProfit)
		}
		if c.stopLossPc > 0 {
			c.riskReward = c.takeProfitPc / c.stopLossPc
		}
	}
	return c
}

func absPct(base, v float64) float64 {
	if base <= 0 {
		return 0
	}
	diff := v - base
	if diff < 0 {
		diff = -diff
	}
	return diff / base * 100
}

// evalNumeric compares a numeric fact against the condition value
func evalNumeric(op string, actual, target float64) bool {
	switch op {
	case ">":
		return actual > target
	case ">=":
		return actual >= target
	case "<":
		return actual < target
	case "<=":
		return actual <= target
	case "==":
		return actual == target
	case "!=":
		return actual != target
	}
	return false
}

// evalCondition evaluates one condition against the decision context
func evalCondition(cond *RuleCondition, c decisionContext) (bool, string) {
	switch cond.Field {
	case "symbol":
		target := fmt.Sprintf("%v", cond.Value)
		switch cond.Op {
		case "==":
			return c.symbol == target, fmt.Sprintf("symbol=%s", c.symbol)
		case "!=":
			return c.symbol != target, fmt.Sprintf("symbol=%s", c.symbol)
		case "in":
			for _, s := range strings.Split(target, ",") {
				if strings.TrimSpace(s) == c.symbol {
					return true, fmt.Sprintf("symbol=%s in [%s]", c.symbol, target)
				}
			}
			return false, fmt.Sprintf("symbol=%s not in [%s]", c.symbol, target)
		}
		return false, ""
	case "leverage":
		return evalNumeric(cond.Op, c.leverage, toFloat(cond.Value)),
			fmt.Sprintf("leverage=%dx", int(c.leverage))
	case "position_size_usd":
		return evalNumeric(cond.Op, c.positionSizeUSD, toFloat(cond.Value)),
			fmt.Sprintf("position=%.2f USDT", c.positionSizeUSD)
	case "position_value_pct":
		return evalNumeric(cond.Op, c.positionValuePc, toFloat(cond.Value)),
			fmt.Sprintf("position=%.1f%% of equity", c.positionValuePc)
	case "stop_loss_pct":
		return evalNumeric(cond.Op, c.stopLossPc, toFloat(cond.Value)),
			fmt.Sprintf("stop-loss distance=%.2f%%", c.stopLossPc)
	case "take_profit_pct":
		return evalNumeric(cond.Op, c.takeProfitPc, toFloat(cond.Value)),
			fmt.Sprintf("take-profit distance=%.2f%%", c.takeProfitPc)
	case "risk_reward":
		return evalNumeric(cond.Op, c.riskReward, toFloat(cond.Value)),
			fmt.Sprintf("risk/reward=%.2f", c.riskReward)
	case "confidence":
		return evalNumeric(cond.Op, c.confidence, toFloat(cond.Value)),
			fmt.Sprintf("confidence=%d", int(c.confidence))
	case "has_stop_loss":
		target, ok := cond.Value.(bool)
		if !ok {
			target = toFloat(cond.Value) != 0
		}
		return c.hasStopLoss == target, fmt.Sprintf("has_stop_loss=%v", c.hasStopLoss)
	case "has_take_profit":
		target, ok := cond.Value.(bool)
		if !ok {
			target = toFloat(cond.Value) != 0
		}
		return c.hasTakeProfit == target, fmt.Sprintf("has_take_profit=%v", c.hasTakeProfit)
	}
	return false, ""
}

func toFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		var f float64
		fmt.Sscanf(n, "%g", &f)
		return f
	}
	return 0
}

// CheckDecisionAgainstRules evaluates all enabled rules against one open decision.
// Returns (violations, warnings). Violations come from hard rules with action=block.
func CheckDecisionAgainstRules(rules []*store.TradingRuleDB, d Decision, equity float64, price float64) (violations []RuleViolation, warnings []RuleViolation) {
	violations = []RuleViolation{}
	warnings = []RuleViolation{}

	// Only check open decisions
	if d.Action != "open_long" && d.Action != "open_short" {
		return violations, warnings
	}

	c := buildDecisionContext(d, equity, price)

	for _, rule := range rules {
		switch rule.RuleType {
		case "hard":
			cond, err := ParseRuleCondition(rule.ConditionJSON)
			if err != nil {
				continue // malformed rule: skip, never block trading on bad config
			}
			matched, detail := evalCondition(cond, c)
			if !matched {
				continue
			}
			v := RuleViolation{
				RuleID:   rule.ID,
				RuleName: rule.Name,
				RuleType: "hard",
				Action:   rule.OnViolation,
				Message:  rule.Description,
				Symbol:   d.Symbol,
				Decision: d.Action,
				Detail:   detail,
			}
			if v.Message == "" {
				v.Message = fmt.Sprintf("%s %s %v", cond.Field, cond.Op, cond.Value)
			}
			if rule.OnViolation == "block" {
				violations = append(violations, v)
			} else {
				warnings = append(warnings, v)
			}
		case "soft":
			// Soft rules are prompt-level guidance; they fire only when the
			// decision's symbol matches the rule's tags (tag=symbol or generic).
			continue
		}
	}
	return violations, warnings
}

// MatchSoftRules returns soft lessons relevant to a decision (tag/symbol match)
func MatchSoftRules(rules []*store.TradingRuleDB, d Decision) []RuleViolation {
	matched := []RuleViolation{}
	if d.Action != "open_long" && d.Action != "open_short" {
		return matched
	}
	for _, rule := range rules {
		if rule.RuleType != "soft" || rule.LessonText == "" || !rule.Enabled {
			continue
		}
		// Soft rules match by tag: if a tag equals the symbol (case-insensitive),
		// it only applies to that symbol; otherwise it applies to all decisions.
		applies := false
		tags := strings.Split(strings.ToLower(rule.Tags), ",")
		hasSymbolTag := false
		for _, tag := range tags {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			if strings.EqualFold(tag, d.Symbol) {
				applies = true
				break
			}
			hasSymbolTag = hasSymbolTag || isSymbolLikeTag(tag)
		}
		if !hasSymbolTag {
			applies = true // generic lesson
		}
		if applies {
			matched = append(matched, RuleViolation{
				RuleID:   rule.ID,
				RuleName: rule.Name,
				RuleType: "soft",
				Action:   "warn",
				Message:  rule.LessonText,
				Symbol:   d.Symbol,
				Decision: d.Action,
			})
		}
	}
	return matched
}

// isSymbolLikeTag checks whether a tag looks like a trading symbol (e.g. BTCUSDT, ETH)
func isSymbolLikeTag(tag string) bool {
	if len(tag) < 2 || len(tag) > 12 {
		return false
	}
	for _, r := range tag {
		if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	// Exclude common non-symbol keyword tags
	exclude := map[string]bool{
		"fomo": true, "revenge": true, "chase": true, "pumping": true, "chasepumping": true,
		"highleverage": true, "high_leverage": true, "nostop": true, "no_stop": true,
		"overtrade": true, "revenge_trade": true, "revengeTrading": true,
	}
	return !exclude[strings.ToLower(tag)]
}

// BuildRulesPromptText renders rules as prompt text injected into the AI user prompt
func BuildRulesPromptText(rules []*store.TradingRuleDB) string {
	var hard, soft []*store.TradingRuleDB
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if r.RuleType == "hard" {
			hard = append(hard, r)
		} else {
			soft = append(soft, r)
		}
	}
	if len(hard) == 0 && len(soft) == 0 {
		return ""
	}

	var sb strings.Builder
	if len(hard) > 0 {
		sb.WriteString("## Mandatory Trading Rules (learned from your past trade reviews)\n")
		sb.WriteString("The following rules were extracted from review of past trades. You MUST obey them when making decisions:\n")
		for _, r := range hard {
			line := fmt.Sprintf("- %s", r.Name)
			if r.Description != "" {
				line += fmt.Sprintf(": %s", r.Description)
			}
			if r.ConditionJSON != "" {
				line += fmt.Sprintf(" [%s]", r.ConditionJSON)
			}
			if r.OnViolation == "block" {
				line += " (violating orders are automatically rejected)"
			}
			sb.WriteString(line + "\n")
		}
		sb.WriteString("\n")
	}
	if len(soft) > 0 {
		sb.WriteString("## Lessons from Past Trade Reviews\n")
		for _, r := range soft {
			line := fmt.Sprintf("- %s", r.Name)
			if r.LessonText != "" {
				line += fmt.Sprintf(": %s", r.LessonText)
			}
			if r.SourceStats != "" {
				line += fmt.Sprintf(" (data: %s)", r.SourceStats)
			}
			sb.WriteString(line + "\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
