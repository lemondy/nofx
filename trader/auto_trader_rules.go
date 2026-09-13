package trader

import (
	"fmt"

	"nofx/kernel"
	"nofx/logger"
	"nofx/store"
	notify "nofx/telegram/notify"
)

// preTradeRuleCheck filters open decisions through the trader's rule system
// (extracted from past trade reviews). Hard rules with action=block reject the
// decision; warn rules log a warning. Soft lessons are logged for awareness.
// Returns the filtered decision list safe to execute.
func (at *AutoTrader) preTradeRuleCheck(decisions []kernel.Decision, equity float64) []kernel.Decision {
	if at.store == nil || len(decisions) == 0 {
		return decisions
	}

	rules, err := at.store.Rule().GetEnabledRules(at.id)
	if err != nil {
		logger.Warnf("⚠️ [%s] Failed to load trading rules, skipping pre-trade check: %v", at.name, err)
		return decisions
	}
	if len(rules) == 0 {
		return decisions
	}

	filtered := make([]kernel.Decision, 0, len(decisions))
	for _, d := range decisions {
		if d.Action != "open_long" && d.Action != "open_short" {
			filtered = append(filtered, d)
			continue
		}

		violations, warnings := kernel.CheckDecisionAgainstRules(rules, d, equity, d.Price)

		blocked := false
		for _, v := range violations {
			blocked = true
			logger.Warnf("🚫 [%s] RULE BLOCKED %s %s: %s (%s) — %s",
				at.name, d.Action, d.Symbol, v.RuleName, v.Detail, v.Message)
			at.logRuleCheck(v, d, true)
			notify.Notify("ALERT", at.name, fmt.Sprintf("<b>🚫 规则拦截 %s %s</b>\n%s（%s）\n本次开仓已阻止",
				d.Action, notify.Escape(d.Symbol), notify.Escape(v.RuleName), notify.Escape(v.Detail)))
		}
		for _, w := range warnings {
			logger.Warnf("⚠️ [%s] RULE WARN %s %s: %s (%s) — %s",
				at.name, d.Action, d.Symbol, w.RuleName, w.Detail, w.Message)
			at.logRuleCheck(w, d, false)
		}

		// Soft lessons relevant to this decision (awareness log only)
		for _, s := range kernel.MatchSoftRules(rules, d) {
			logger.Infof("📚 [%s] LESSON %s %s: %s", at.name, d.Action, d.Symbol, s.Message)
		}

		if blocked {
			continue
		}
		filtered = append(filtered, d)
	}
	return filtered
}

// logRuleCheck persists a fired rule and increments its hit counter
func (at *AutoTrader) logRuleCheck(v kernel.RuleViolation, d kernel.Decision, blocked bool) {
	if at.store == nil {
		return
	}
	log := &store.RuleCheckLogDB{
		TraderID: at.id,
		RuleID:   v.RuleID,
		RuleName: v.RuleName,
		Action:   d.Action,
		Symbol:   d.Symbol,
		Violated: true,
		Blocked:  blocked,
		Message:  fmt.Sprintf("%s | %s", v.Detail, v.Message),
	}
	if err := at.store.Rule().LogCheck(log); err != nil {
		logger.Warnf("⚠️ [%s] Failed to log rule check: %v", at.name, err)
	}
	if err := at.store.Rule().IncrementHitCount(v.RuleID, blocked); err != nil {
		logger.Warnf("⚠️ [%s] Failed to update rule hit count: %v", at.name, err)
	}
}
