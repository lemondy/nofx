package kernel

import (
	"strings"
	"testing"

	"nofx/market"
	"nofx/store"
)

func TestReview20261029BTCHeaderClosedIndicators(t *testing.T) {
	stubLiveMarket(t, neutralBTC4h())
	btc := &market.Data{Symbol: "BTCUSDT", CurrentPrice: 65000,
		CurrentMACD: 9999, CurrentRSI7: 99, ClosedMACD: -12.3456, ClosedRSI7: 42.25, ClosedTimeframe: "15m"}
	ctx := &Context{MarketDataMap: map[string]*market.Data{"BTCUSDT": btc}}
	engine := NewStrategyEngine(&store.StrategyConfig{})
	header := func() string {
		for _, line := range strings.Split(engine.BuildUserPrompt(ctx), "\n") {
			if strings.HasPrefix(line, "BTC:") {
				return line
			}
		}
		t.Fatal("BTC header missing")
		return ""
	}
	line := header()
	for _, want := range []string{"BTC(15m 收盘)", "=-12.3456", "RSI7=42.25"} {
		if !strings.Contains(line, want) {
			t.Fatalf("header missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "9999") || strings.Contains(line, "99.00") {
		t.Fatalf("header used Current indicators: %s", line)
	}
	btc.ClosedTimeframe = ""
	line = header()
	if strings.Contains(line, "MACD") || strings.Contains(line, "RSI") {
		t.Fatalf("unavailable closed indicators fell back to live values: %s", line)
	}
	btc.ClosedTimeframe, btc.ClosedMACD, btc.ClosedRSI7 = "1h", 0, 0
	if line = header(); !strings.Contains(line, "BTC(1h 收盘)") || !strings.Contains(line, "RSI7=0.00") {
		t.Fatalf("valid zero indicators were suppressed: %s", line)
	}
}

func TestReview20261029VolTargetAndMarginWording(t *testing.T) {
	cfg := &store.StrategyConfig{Language: "zh"}
	cfg.RiskControl.MaxMarginUsage = 0.9
	cfg.RiskControl.VolTargetEnabled = true
	prompt := NewStrategyEngine(cfg).BuildSystemPrompt(100, "")
	for _, want := range []string{"<80% 时程序不会自动加仓,同向加仓会被拒绝,因此不要提出加仓", ">120% 程序自动减仓", "不要因波动率变化去动 SL/TP", "90%"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, "评估加仓") {
		t.Fatal("VolTarget prompt still invites adding to a position")
	}
	if len(prompt) > 26000 {
		t.Fatalf("VolTarget system prompt exceeds budget: %d bytes", len(prompt))
	}
	cfg.RiskControl.VolTargetEnabled = false
	if strings.Contains(NewStrategyEngine(cfg).BuildSystemPrompt(100, ""), "<80% 时") {
		t.Fatal("disabled VolTarget still emits adjustment instructions")
	}
	for _, tc := range []struct {
		lang Language
		want string
	}{
		{LangChinese, ">70%为风险提示（非硬上限，硬上限见策略参数）"},
		{LangEnglish, ">70% warning only; cap: strategy config"},
	} {
		if !strings.Contains(GetSchemaPrompt(tc.lang), tc.want) {
			t.Fatalf("schema %s missing %q", tc.lang, tc.want)
		}
	}
}
