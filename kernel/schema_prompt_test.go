package kernel

import (
	"strings"
	"testing"
)

func TestSchemaPromptIsDeterministic(t *testing.T) {
	want := GetSchemaPrompt(LangChinese)
	for i := 0; i < 100; i++ {
		if got := GetSchemaPrompt(LangChinese); got != want {
			t.Fatalf("schema prompt changed across identical renders at iteration %d", i)
		}
	}
	ordered := []string{"**Equity**", "**Balance**", "**PnL**", "**MarginUsage**"}
	last := -1
	for _, marker := range ordered {
		i := strings.Index(want, marker)
		if i <= last {
			t.Fatalf("schema field %s is absent or out of canonical order", marker)
		}
		last = i
	}
}

func TestSchemaPromptUsesSideAwareRiskDefinitions(t *testing.T) {
	zh := GetSchemaPrompt(LangChinese)
	for _, want := range []string{
		"方向因子 × (出场价 - 进场价)",
		"总权益 - 已用保证金",
		"0 表示未知或不适用",
		"不能单独证明多空方向",
	} {
		if !strings.Contains(zh, want) {
			t.Errorf("corrected schema definition missing %q", want)
		}
	}
	for _, gone := range []string{
		"0.0000表示无爆仓风险",
		"持仓量增加=资金流入",
	} {
		if strings.Contains(zh, gone) {
			t.Errorf("unsafe schema definition remains: %q", gone)
		}
	}
}
