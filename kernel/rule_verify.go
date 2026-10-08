package kernel

import (
	"math"

	"nofx/store"
)

// review 2026-10-09 K: server-side verification of AI-proposed hard rules.
// The ">=3 supporting trades" guideline used to live only in the LLM prompt
// (source_stats is the model's free-text claim). This replays the rule's
// condition through the SAME fact extraction the live pre-trade check uses
// (buildDecisionContext/evalCondition) against the trader's closed AI trades.

const (
	VerifySupported    = "supported"
	VerifyWeak         = "weak"
	VerifyContradicted = "contradicted"
	VerifyUnverifiable = "unverifiable"
	VerifySoft         = "soft"

	// RuleVerifyWindowDays population window (all closed AI trades, not only
	// the reviewed ones — the reviewed subset is biased toward losers).
	RuleVerifyWindowDays = 90
	// RuleVerifyMinMatched minimum matching trades for a verdict.
	RuleVerifyMinMatched = 3
)

// RuleVerification result of replaying one proposed rule over history.
type RuleVerification struct {
	Status              string  `json:"status"`
	Matched             int     `json:"matched"`
	Wins                int     `json:"wins"`
	NetPnL              float64 `json:"net_pnl"`
	BaselineWinRate     float64 `json:"baseline_win_rate"` // 0..1 over the population
	BaselineNetPerTrade float64 `json:"baseline_net_per_trade"`
	Population          int     `json:"population"`
	Reason              string  `json:"reason,omitempty"`
}

// VerificationPopulation keeps closed AI-managed trades exited within the
// window; manual and unclassified (NULL ai_managed) rows are excluded.
func VerificationPopulation(trades []*store.TradeJournalDB, nowMs int64) []*store.TradeJournalDB {
	cutoff := nowMs - int64(RuleVerifyWindowDays)*24*3600*1000
	out := make([]*store.TradeJournalDB, 0, len(trades))
	for _, t := range trades {
		if t == nil || t.AIManaged == nil || !*t.AIManaged {
			continue
		}
		if t.ExitTime <= 0 || t.ExitTime < cutoff {
			continue
		}
		out = append(out, t)
	}
	return out
}

// journalFieldsAvailable reports whether the journal row carries the inputs
// for a condition field; false means it cannot be reconstructed for this row.
func journalFieldsAvailable(field string, t *store.TradeJournalDB, equity float64) bool {
	switch field {
	case "symbol":
		return t.Symbol != ""
	case "leverage":
		return t.Leverage > 0
	case "position_size_usd":
		return t.Quantity > 0 && t.EntryPrice > 0
	case "position_value_pct":
		return equity > 0 && t.Quantity > 0 && t.EntryPrice > 0
	case "stop_loss_pct":
		return t.EntryPrice > 0 && t.PlannedStopLoss > 0
	case "take_profit_pct":
		return t.EntryPrice > 0 && t.PlannedTakeProfit > 0
	case "risk_reward":
		return t.EntryPrice > 0 && t.PlannedStopLoss > 0 && t.PlannedTakeProfit > 0
	}
	// confidence (not reliably journaled), has_stop_loss / has_take_profit
	// (a zero planned SL/TP cannot be told apart from a missing enrichment).
	return false
}

// journalDecision rebuilds the open Decision the live check would have seen.
func journalDecision(t *store.TradeJournalDB) Decision {
	action := "open_long"
	if t.Side == "SHORT" {
		action = "open_short"
	}
	return Decision{
		Symbol:          t.Symbol,
		Action:          action,
		Leverage:        t.Leverage,
		PositionSizeUSD: t.Quantity * t.EntryPrice,
		StopLoss:        t.PlannedStopLoss,
		TakeProfit:      t.PlannedTakeProfit,
	}
}

// VerifyHardRule replays a rule's condition over the trader's closed AI
// trades. trades may be unfiltered (filtered here). equity is the trader's
// initial balance (0 = unknown, position_value_pct becomes unverifiable).
//
// Claim of a block/warn rule: trades matching the condition do worse.
//   - supported:    matched >= 3, matched net < 0 and net/trade < baseline net/trade
//   - contradicted: matched >= 3 but not supported
//   - weak:         matched < 3
//   - unverifiable: bad condition, or the field cannot be rebuilt from the journal
//   - soft:         soft rule, no machine check
//
// Net PnL = RealizedPnL - Fee (same caliber as the journal stats).
func VerifyHardRule(ruleType, conditionJSON string, trades []*store.TradeJournalDB, equity float64, nowMs int64) RuleVerification {
	if ruleType == "soft" {
		return RuleVerification{Status: VerifySoft, Reason: "soft rule: no machine check"}
	}
	cond, err := ParseRuleCondition(conditionJSON)
	if err != nil {
		return RuleVerification{Status: VerifyUnverifiable, Reason: "invalid condition: " + err.Error()}
	}
	pop := VerificationPopulation(trades, nowMs)
	eval := make([]*store.TradeJournalDB, 0, len(pop))
	for _, t := range pop {
		if journalFieldsAvailable(cond.Field, t, equity) {
			eval = append(eval, t)
		}
	}
	if len(eval) == 0 {
		return RuleVerification{Status: VerifyUnverifiable,
			Reason: "field " + cond.Field + " cannot be reconstructed from the trade journal"}
	}

	res := RuleVerification{Population: len(eval)}
	var totalNet float64
	var totalWins int
	for _, t := range eval {
		net := t.RealizedPnL - t.Fee
		totalNet += net
		if net > 0 {
			totalWins++
		}
		c := buildDecisionContext(journalDecision(t), equity, t.EntryPrice)
		if hit, _ := evalCondition(cond, c); hit {
			res.Matched++
			res.NetPnL += net
			if net > 0 {
				res.Wins++
			}
		}
	}
	res.BaselineWinRate = float64(totalWins) / float64(len(eval))
	res.BaselineNetPerTrade = totalNet / float64(len(eval))

	if res.Matched < RuleVerifyMinMatched {
		res.Status = VerifyWeak
		return res
	}
	perTrade := res.NetPnL / float64(res.Matched)
	if res.NetPnL < 0 && perTrade < res.BaselineNetPerTrade && !math.IsNaN(perTrade) {
		res.Status = VerifySupported
	} else {
		res.Status = VerifyContradicted
	}
	return res
}
