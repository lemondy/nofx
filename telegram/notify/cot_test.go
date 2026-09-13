package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The full render must stay within Telegram's 4096-char cap and escape raw
// HTML in the reasoning.
func TestFormatCoTSizeAndEscape(t *testing.T) {
	cot := strings.Repeat("分析第 N 个候选币种: <b>污染</b> & *强调* 文本。\n", 200) // ~10k runes
	decisions := []DecisionSummary{
		{Symbol: "WLDUSDT", Action: "open_long_limit", OK: true, Detail: "@0.4599 50U SL0.44 TP0.49 置信85"},
		{Symbol: "ICPUSDT", Action: "wait", ErrText: "被闸门过滤，未执行"},
	}
	out := FormatCoT("mac-nofx", 961, "deepseek", cot, decisions)
	if utf8.RuneCountInString(out) > telegramMsgLimit {
		t.Fatalf("rendered message %d runes exceeds %d", utf8.RuneCountInString(out), telegramMsgLimit)
	}
	if strings.Contains(out, "<b>污染</b> &") {
		t.Error("raw HTML in reasoning was not escaped")
	}
	// Tail-keep: the LAST line of the input must survive truncation.
	if !strings.Contains(out, "（前文已截断）") {
		t.Error("expected truncation marker")
	}
	if !strings.Contains(out, "本周期决策") || !strings.Contains(out, "WLDUSDT") {
		t.Error("decisions section missing")
	}
	if !strings.Contains(out, "周期 #961") {
		t.Error("cycle number missing from headline")
	}
}

// Short reasoning passes through without the truncation marker.
func TestFormatCoTShortBody(t *testing.T) {
	out := FormatCoT("猪猪侠", 5, "glm", "账户无持仓，所有候选 RR 不足，等待。", nil)
	if strings.Contains(out, "截断") {
		t.Errorf("short body must not be truncated: %s", out)
	}
	if !strings.Contains(out, "\n账户无持仓，所有候选 RR 不足，等待。\n") {
		t.Errorf("escaped body line missing: %s", out)
	}
	if !strings.Contains(out, "周期 #5") {
		t.Error("cycle number missing from headline")
	}
}

// Empty reasoning renders a placeholder instead of an empty block.
func TestFormatCoTEmpty(t *testing.T) {
	out := FormatCoT("t", 1, "", "  ", nil)
	if !strings.Contains(out, "无思维链输出") {
		t.Errorf("empty CoT placeholder missing: %s", out)
	}
}

// markdownToHTML: bold/italic/code conversion, heading strip, and unbalanced
// markers must be closed (Telegram would reject otherwise).
func TestMarkdownToHTML(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"**LTCUSDT** 不做", "<b>LTCUSDT</b> 不做"},
		{"### 结论", "结论"},
		{"`RR` < 2", "<code>RR</code> &lt; 2"},
		{"*未闭合加粗", "<i>未闭合加粗</i>"},
		{"**a *b", "<b>a <i>b</i></b>"},
		{"a_b_c", "a_b_c"}, // intraword underscores stay literal (snake_case)
		{"limit_buy_price 字段", "limit_buy_price 字段"},
		{"_开头_ 斜体", "<i>开头</i> 斜体"},
	}
	for _, tc := range cases {
		if got := markdownToHTML(tc.in); got != tc.want {
			t.Errorf("markdownToHTML(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Decision outcome markers: success ✓, failure ✗ + error code, passive •.
func TestFormatDecisionLine(t *testing.T) {
	ok := FormatDecisionLine(DecisionSummary{Symbol: "SOL", Action: "close_long", OK: true})
	if !strings.HasPrefix(ok, "✓") {
		t.Errorf("success line marker wrong: %s", ok)
	}
	bad := FormatDecisionLine(DecisionSummary{Symbol: "SOL", Action: "open_long", ErrText: "rejected: RR"})
	if !strings.HasPrefix(bad, "✗") || !strings.Contains(bad, "<code>rejected: RR</code>") {
		t.Errorf("failure line wrong: %s", bad)
	}
	passive := FormatDecisionLine(DecisionSummary{Symbol: "BTC", Action: "hold"})
	if !strings.HasPrefix(passive, "•") {
		t.Errorf("passive line marker wrong: %s", passive)
	}
}
