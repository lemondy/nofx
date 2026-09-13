package trader

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"nofx/kernel"
	"nofx/store"
	notify "nofx/telegram/notify"
)

// isActionableAction reports whether the action would touch the exchange
// (opens and closes) — passive hold/wait cycles don't justify a CoT push.
func isActionableAction(action string) bool {
	return strings.HasPrefix(action, "open_") || strings.HasPrefix(action, "close_")
}

// cotHasActionable reports whether any proposed decision touches the exchange.
func cotHasActionable(decisions []kernel.Decision) bool {
	for _, d := range decisions {
		if isActionableAction(d.Action) {
			return true
		}
	}
	return false
}

// buildCoTSummaries maps the AI's proposed decisions onto their execution
// outcomes: executed entries get ✓ (or ✗ with the error), hold/wait stays
// passive (•), and an actionable decision that never reached execution was
// filtered by a gate — flagged as such instead of silently vanishing.
func buildCoTSummaries(proposed []kernel.Decision, executed []store.DecisionAction) []notify.DecisionSummary {
	byKey := make(map[string]*store.DecisionAction, len(executed))
	for i := range executed {
		a := &executed[i]
		byKey[a.Symbol+"|"+a.Action] = a
	}

	summaries := make([]notify.DecisionSummary, 0, len(proposed))
	for _, d := range proposed {
		s := notify.DecisionSummary{
			Symbol: d.Symbol,
			Action: d.Action,
			Detail: cotDecisionDetail(d),
		}
		if a, ok := byKey[d.Symbol+"|"+d.Action]; ok {
			switch {
			case a.Success:
				s.OK = true
			case a.Error != "":
				s.ErrText = a.Error
			default:
				s.ErrText = "未执行"
			}
		} else if isActionableAction(d.Action) {
			s.ErrText = "被闸门过滤，未执行"
		}
		summaries = append(summaries, s)
	}
	return summaries
}

// cotDecisionDetail is the compact parameter line: trigger price for limit
// entries, planned size, SL/TP.
func cotDecisionDetail(d kernel.Decision) string {
	var parts []string
	if d.Price > 0 {
		parts = append(parts, fmt.Sprintf("@%g", d.Price))
	}
	if d.PositionSizeUSD > 0 {
		parts = append(parts, fmt.Sprintf("%.0fU", d.PositionSizeUSD))
	}
	if d.StopLoss > 0 {
		parts = append(parts, fmt.Sprintf("SL%g", d.StopLoss))
	}
	if d.TakeProfit > 0 {
		parts = append(parts, fmt.Sprintf("TP%g", d.TakeProfit))
	}
	if d.Confidence > 0 {
		parts = append(parts, fmt.Sprintf("置信%d", d.Confidence))
	}
	return strings.Join(parts, " ")
}

// cotClamp bounds a dynamic string (error text, detail) so one pathological
// decision can't crowd the 4096-char message budget.
func cotClamp(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes-1]) + "…"
}
