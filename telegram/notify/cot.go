package notify

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Telegram hard-caps one message at 4096 UTF-8 characters.
const telegramMsgLimit = 4096

// isWordRune reports whether r is a letter/digit (ASCII check is enough —
// the snake_case identifiers the reasoning quotes are ASCII).
func isWordRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// FormatCoT renders the AI's chain of thought as a Telegram HTML message:
// a 💭 headline (trader, cycle, model), the reasoning body with light
// markdown → HTML conversion, and the cycle's executed decisions. Oversized
// reasoning is tail-kept (the conclusion sits at the END of the chain —
// keep that, drop the front preamble).
func FormatCoT(traderName string, cycle int, model, cot string, decisions []DecisionSummary) string {
	modelTag := model
	if modelTag == "" {
		modelTag = "AI"
	}
	headline := fmt.Sprintf("<b>💭 AI 思维链 · %s</b> · <i>周期 #%d · %s</i>\n",
		Escape(traderName), cycle, Escape(modelTag))
	headline += "━━━━━━━━━━━━━━━━━━\n"

	var suffix strings.Builder
	if len(decisions) > 0 {
		suffix.WriteString("━━━━━━━━━━━━━━━━━━\n")
		suffix.WriteString("<b>本周期决策</b>\n")
		for _, d := range decisions {
			suffix.WriteString(FormatDecisionLine(d))
			suffix.WriteString("\n")
		}
	}

	// The budget is enforced on CONVERTED text: escaping and markdown→HTML
	// both expand the raw chain, so a raw-text cap would silently overflow.
	bodyBudget := telegramMsgLimit - utf8.RuneCountInString(headline) - utf8.RuneCountInString(suffix.String()) - 16
	body := FormatCoTBody(cot, bodyBudget)
	return headline + body + suffix.String()
}

// FormatCoTBody converts one chain-of-thought text into Telegram HTML:
// HTML-escaped, markdown bold/italic translated, line structure preserved.
// Capped at maxRunes by dropping whole converted lines from the FRONT (whole
// lines keep tags balanced; the tail holds the model's conclusion).
func FormatCoTBody(cot string, maxRunes int) string {
	cot = strings.TrimSpace(cot)
	if cot == "" {
		return "<i>（本轮无思维链输出）</i>\n"
	}
	if maxRunes < 200 {
		maxRunes = 200
	}
	lines := strings.Split(cot, "\n")
	rendered := make([]string, 0, len(lines))
	total := 0
	for _, line := range lines {
		r := markdownToHTML(strings.TrimRight(line, " \t\r"))
		rendered = append(rendered, r)
		total += utf8.RuneCountInString(r) + 1
	}
	start := 0
	for start < len(rendered) && total > maxRunes {
		total -= utf8.RuneCountInString(rendered[start]) + 1
		start++
	}
	var sb strings.Builder
	if start > 0 {
		sb.WriteString("<i>…（前文已截断）</i>\n")
	}
	for _, r := range rendered[start:] {
		sb.WriteString(r)
		sb.WriteString("\n")
	}
	return sb.String()
}

// DecisionSummary is one executed decision line shown under the reasoning.
type DecisionSummary struct {
	Symbol  string
	Action  string
	OK      bool
	Detail  string // short human summary (price/size/error), already plain text
	ErrText string // non-empty → execution failed
}

// FormatDecisionLine renders one decision line with an outcome marker:
// ✓ executed / ✗ failed / • not executed (hold/wait or filtered).
func FormatDecisionLine(d DecisionSummary) string {
	marker := "•"
	switch {
	case d.ErrText != "":
		marker = "✗"
	case d.OK:
		marker = "✓"
	}
	line := fmt.Sprintf("%s <b>%s</b> %s", marker, Escape(d.Symbol), Escape(d.Action))
	if d.Detail != "" {
		line += " — " + Escape(d.Detail)
	}
	if d.ErrText != "" {
		line += "\n   <code>" + Escape(d.ErrText) + "</code>"
	}
	return line
}

// markdownToHTML escapes HTML specials then translates the light markdown the
// reasoning uses (**bold**, *italic*, `code`, ### headings) into Telegram tags.
// Tag toggles nest: opening pushes, closing matches the innermost open tag —
// the final sweep closes anything left so Telegram never rejects the message.
func markdownToHTML(line string) string {
	s := Escape(line)
	s = strings.TrimPrefix(s, "### ")
	s = strings.TrimPrefix(s, "## ")
	s = strings.TrimPrefix(s, "# ")

	var sb strings.Builder
	var open []string // stack of open tag closers: "</b>", "</i>", "</code>"
	closeTop := func() {
		if n := len(open); n > 0 {
			sb.WriteString(open[n-1])
			open = open[:n-1]
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '`':
			if len(open) > 0 && open[len(open)-1] == "</code>" {
				closeTop()
			} else {
				sb.WriteString("<code>")
				open = append(open, "</code>")
			}
		case c == '*' && i+1 < len(runes) && runes[i+1] == '*':
			if len(open) > 0 && open[len(open)-1] == "</b>" {
				closeTop()
			} else {
				sb.WriteString("<b>")
				open = append(open, "</b>")
			}
			i++
		case c == '*' || c == '_':
			// Intraword underscores stay literal: the reasoning quotes
			// snake_case field names (limit_buy_price) constantly.
			if c == '_' && i > 0 && i+1 < len(runes) &&
				isWordRune(runes[i-1]) && isWordRune(runes[i+1]) {
				sb.WriteRune(c)
				continue
			}
			if len(open) > 0 && open[len(open)-1] == "</i>" {
				closeTop()
			} else {
				sb.WriteString("<i>")
				open = append(open, "</i>")
			}
		default:
			sb.WriteRune(c)
		}
	}
	for len(open) > 0 {
		closeTop()
	}
	return sb.String()
}

// SendCoT enqueues a pre-rendered CoT message bypassing the kind-badge
// formatter (the text is already full HTML with its own headline).
func SendCoT(html string) {
	mu.RLock()
	configured := st != nil
	mu.RUnlock()
	if !configured {
		return
	}
	select {
	case queue <- html:
	default:
		droppedMu.Lock()
		dropped++
		droppedMu.Unlock()
	}
}
