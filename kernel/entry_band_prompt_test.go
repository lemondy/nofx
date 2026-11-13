package kernel

import (
	"nofx/store"
	"reflect"
	"testing"
)

func TestEntryBandPromptIdentity(t *testing.T) {
	stubLiveMarket(t, neutralBTC4h())
	cfg := store.StrategyConfig{}
	off := NewStrategyEngine(&cfg)
	ctx := okCandidateCtx()
	system, user := off.BuildSystemPrompt(1000, ""), off.BuildUserPrompt(ctx)
	gates := ctx.GateStates
	cfg.RiskControl.EntryBandEnabled = true
	cfg.RiskControl.EntryBandMode = "fixed"
	cfg.RiskControl.EntryBandPhiBps = 12
	cfg.RiskControl.EntryBandFixedBps = 9
	cfg.RiskControl.EntryBandTimeoutMin = 35
	on := NewStrategyEngine(&cfg)
	if got := on.BuildSystemPrompt(1000, ""); got != system {
		t.Fatal("entry band altered system prompt")
	}
	if got := on.BuildUserPrompt(ctx); got != user {
		t.Fatal("entry band altered user prompt")
	}
	if !reflect.DeepEqual(gates, ctx.GateStates) {
		t.Fatal("entry band altered signal gates")
	}
}
