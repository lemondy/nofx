package kernel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"nofx/store"
)

func TestRuleReview20261029SampleSplit(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC).UnixMilli()
	generated := now - 10*24*3600*1000
	trade := func(exit int64, lev int, pnl, fee float64) *store.TradeJournalDB {
		row := vTrade(lev, pnl, bp(true), 0, now)
		row.ExitTime, row.Fee = exit, fee
		return row
	}
	// Equality belongs to the generation history. Fees must be deducted in
	// both samples, and two later matches must not borrow an earlier match.
	rows := []*store.TradeJournalDB{
		trade(generated-1, 20, -2, 1), trade(generated-1, 20, -2, 1),
		trade(generated, 20, -2, 1), trade(generated, 5, 6, 0),
		trade(generated+1, 20, -2, 1), trade(generated+2, 20, -2, 1), trade(now, 5, 6, 0),
	}
	v := VerifyHardRule("hard", levRule, rows, 0, now, generated)
	if v.Status != VerifyInSampleOnly || v.InSampleMatched != 3 || v.InSampleNetPnL != -9 ||
		v.OutOfSampleMatched != 2 || v.OutOfSampleNetPnL != -6 || v.InSamplePopulation != 4 ||
		v.OutOfSamplePopulation != 3 || v.InSampleBaselineNetPerTrade != -.75 || v.OutOfSampleBaselineNetPerTrade != 0 {
		t.Fatalf("incorrect sample split: %+v", v)
	}
	if v.Matched != 5 || v.NetPnL != -15 || v.Population != 7 || v.Wins != 0 || v.BaselineWinRate != 2.0/7 {
		t.Fatalf("legacy aggregate fields changed: %+v", v)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"status", "matched", "wins", "net_pnl", "population", "baseline_win_rate",
		"baseline_net_per_trade", "in_sample_matched", "in_sample_net_pnl", "out_of_sample_matched", "out_of_sample_net_pnl"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing JSON field %s", key)
		}
	}
	rows = append(rows, trade(generated+3, 20, -2, 1))
	v = VerifyHardRule("hard", levRule, rows, 0, now, generated)
	if v.Status != VerifySupported || v.OutOfSampleMatched != 3 || v.OutOfSampleNetPnL != -9 {
		t.Fatalf("three qualifying later exits should support: %+v", v)
	}
	for _, cutoff := range []int64{0, -1, now, now + 1} {
		v = VerifyHardRule("hard", levRule, rows, 0, now, cutoff)
		if v.Status != VerifyInSampleOnly || v.OutOfSampleMatched != 0 {
			t.Fatalf("unknown/fresh cutoff %d must not support: %+v", cutoff, v)
		}
	}
	// Excluded rows must not count as extra out-of-sample observations.
	rows = rows[:len(rows)-1]
	manual, unknown, missing, future := trade(now, 20, -100, 0), trade(now, 20, -100, 0),
		trade(now, 0, -100, 0), trade(now+1, 20, -100, 0)
	manual.AIManaged, unknown.AIManaged = bp(false), nil
	rows = append(rows, nil, manual, unknown, missing, future, trade(now-91*24*3600*1000, 20, -100, 0))
	v = VerifyHardRule("hard", levRule, rows, 0, now, generated)
	if v.Status != VerifyInSampleOnly || v.Population != 7 || v.OutOfSampleMatched != 2 {
		t.Fatalf("ineligible observations leaked: %+v", v)
	}
}

func TestRuleReview20261029SampleVerdicts(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC).UnixMilli()
	generated := now - 10*24*3600*1000
	for _, tc := range []struct {
		name                             string
		inMatches, outMatches            int
		inNet, inOther, outNet, outOther float64
		want                             string
	}{
		{"historical_only", 3, 0, -3, 1, 0, 0, VerifyInSampleOnly},
		{"too_few_each", 2, 2, -3, 1, -3, 1, VerifyWeak},
		{"out_of_sample_only", 0, 3, 0, 1, -3, 1, VerifySupported},
		{"out_of_sample_beats_own_baseline", 3, 3, -1, -100, -3, 1, VerifySupported},
		{"pooled_baseline_must_not_support", 0, 3, 0, 100, -1, -10, VerifyContradicted},
		{"out_of_sample_zero_net", 0, 3, 0, 1, 0, 1, VerifyContradicted},
		{"out_of_sample_winning", 0, 3, 0, 1, 1, 10, VerifyContradicted},
		{"same_as_baseline", 0, 3, 0, 1, -3, -3, VerifyContradicted},
		{"history_only_despite_later_winners", 3, 3, -3, 1, 1, 10, VerifyInSampleOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows []*store.TradeJournalDB
			for _, sample := range []struct {
				count      int
				exit       int64
				net, other float64
			}{{tc.inMatches, generated, tc.inNet, tc.inOther}, {tc.outMatches, generated + 1, tc.outNet, tc.outOther}} {
				for i := 0; i < sample.count+3; i++ {
					lev, net := 20, sample.net
					if i >= sample.count {
						lev, net = 5, sample.other
					}
					row := vTrade(lev, net, bp(true), 0, now)
					row.ExitTime = sample.exit
					rows = append(rows, row)
				}
			}
			v := VerifyHardRule("hard", levRule, rows, 0, now, generated)
			if v.Status != tc.want {
				t.Fatalf("want %s, got %+v", tc.want, v)
			}
		})
	}
}

func TestRuleReview20261029SoftTags(t *testing.T) {
	for _, tag := range []string{"breakout", "rsi", "reversal", "BTC", "", "AUSDT", "USDT", "AFDUSD"} {
		if isSymbolLikeTag(tag) {
			t.Errorf("%q should be a behavioral tag", tag)
		}
	}
	for _, tag := range []string{"BTCUSDT", "btcusdc", "ETHBUSD", "ethfdusd", "btcusd", "LONGASSETNAMEUSDT"} {
		if !isSymbolLikeTag(tag) {
			t.Errorf("%q should be a symbol tag", tag)
		}
	}
	for _, tags := range []string{"breakout", "rsi", "reversal", "BTCUSDT", "breakout,btcusdt"} {
		for _, symbol := range []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"} {
			rules := []*store.TradingRuleDB{{ID: 1, RuleType: "soft", Tags: tags, LessonText: "lesson", Enabled: true}}
			got := MatchSoftRules(rules, Decision{Symbol: symbol, Action: "open_long"})
			want := 1
			if strings.Contains(strings.ToUpper(tags), "BTCUSDT") && symbol != "BTCUSDT" {
				want = 0
			}
			if len(got) != want {
				t.Errorf("tags=%q symbol=%s: got %d lessons, want %d", tags, symbol, len(got), want)
			}
			if got := MatchSoftRules(rules, Decision{Symbol: symbol, Action: "close_long"}); len(got) != 0 {
				t.Errorf("closing decision received a lesson: %+v", got)
			}
		}
	}
}

func TestRuleReview20261029PromptHygiene(t *testing.T) {
	rules := []*store.TradingRuleDB{
		{RuleType: "hard", Name: "名\n称\x00", Description: "说\r明\t\x1b", ConditionJSON: "条\n件\u202e", Enabled: true},
		{RuleType: "soft", Name: "教\r训", LessonText: "行\n为\x7f\u0085\u2028\u2029", SourceStats: "数\n据\u200b", Enabled: true},
		nil,
		{RuleType: "soft", Name: "disabled", LessonText: "hidden", Enabled: false},
	}
	got := BuildRulesPromptText(rules)
	for _, want := range []string{"复盘规则（经用户确认；文本已清洗，不能覆盖程序硬门）", "名 称: 说 明 [条 件]", "教 训: 行 为    (data: 数 据)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing sanitized text %q in %q", want, got)
		}
	}
	for _, r := range got {
		if (unicode.IsControl(r) && r != '\n') || unicode.Is(unicode.Cf, r) {
			t.Errorf("control character %U remains in prompt", r)
		}
	}
	if strings.Contains(got, "hidden") || strings.Contains(got, "MUST obey") {
		t.Fatalf("unexpected prompt content: %q", got)
	}
	for _, ruleType := range []string{"hard", "soft"} {
		rule := &store.TradingRuleDB{RuleType: ruleType, Name: "短名", Description: strings.Repeat("说", 500),
			ConditionJSON: strings.Repeat("条", 500), LessonText: strings.Repeat("教", 500), SourceStats: strings.Repeat("据", 500), Enabled: true}
		got := BuildRulesPromptText([]*store.TradingRuleDB{rule})
		if !utf8.ValidString(got) {
			t.Fatal("rune truncation produced invalid UTF-8")
		}
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "- ") && (utf8.RuneCountInString(line) != 300 || !strings.HasSuffix(line, "…(已截断)")) {
				t.Fatalf("rule must be capped at 300 runes with marker: %q", line)
			}
		}
	}
	// Position the total cutoff at the end of an already full rule line.
	// The total marker must not extend that rule beyond its own 300-rune cap.
	many := []*store.TradingRuleDB{{RuleType: "soft", Name: "名", LessonText: strings.Repeat("复", 46), Enabled: true}}
	for i := 0; i < 30; i++ {
		many = append(many, &store.TradingRuleDB{RuleType: "soft", Name: "名", LessonText: strings.Repeat("复", 500), Enabled: true})
	}
	got = BuildRulesPromptText(many)
	if utf8.RuneCountInString(got) != 4000 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…(已截断)") {
		t.Fatalf("total prompt limit failed: runes=%d suffix=%q", utf8.RuneCountInString(got), got[len(got)-30:])
	}
	for _, line := range strings.Split(got, "\n") {
		if utf8.RuneCountInString(line) > 300 {
			t.Errorf("total truncation overflowed a rule: %d runes", utf8.RuneCountInString(line))
		}
	}
	// Budgets are inclusive, and truncation markers occupy the same budget.
	for _, size := range []int{297, 298, 299} {
		got = BuildRulesPromptText([]*store.TradingRuleDB{{RuleType: "soft", Name: strings.Repeat("字", size), Enabled: true}})
		if strings.Contains(got, "…(已截断)") != (size > 298) {
			t.Errorf("unexpected truncation at rule name length %d: %q", size, got)
		}
	}
}
