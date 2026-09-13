package kernel

import (
	"testing"
)

// The anchor offset / breathing yardstick is ATR(1h) (user directive
// 2026-09-10): a wild execution-TF ATR must not inflate the offset when a 1h
// block exists; 1h missing → execution-TF fallback; both missing → 0 (fixed
// mode path).
func TestAnchorATRPct(t *testing.T) {
	sig := &SymbolSignal{
		RoleTFs: RoleTimeframes{ExecutionTF: "15m", TrendTF: "1h", RegimeTF: "4h"},
		Timeframes: map[string]*TFSignal{
			"15m": {Timeframe: "15m", ATRPct: 0.40},
			"1h":  {Timeframe: "1h", ATRPct: 1.10},
			"4h":  {Timeframe: "4h", ATRPct: 2.80},
		},
	}
	if got := sig.AnchorATRPct(); got != 1.10 {
		t.Fatalf("AnchorATRPct must read the 1h block, got %.3f", got)
	}

	// No 1h → execution TF fallback.
	no1h := &SymbolSignal{
		RoleTFs: RoleTimeframes{ExecutionTF: "15m"},
		Timeframes: map[string]*TFSignal{
			"15m": {Timeframe: "15m", ATRPct: 0.40},
		},
	}
	if got := no1h.AnchorATRPct(); got != 0.40 {
		t.Fatalf("1h missing must fall back to execution TF, got %.3f", got)
	}

	// Nothing → 0 (AnchorOffsetPct then rides the fixed fallback).
	if got := (&SymbolSignal{}).AnchorATRPct(); got != 0 {
		t.Fatalf("empty signal must yield 0, got %.3f", got)
	}
	if got := (*SymbolSignal)(nil).AnchorATRPct(); got != 0 {
		t.Fatalf("nil signal must yield 0, got %.3f", got)
	}
}

// Offset on the 1h yardstick: mult × ATR(1h) clamped — a 1h ATR of 1.1% with
// mult 0.5 gives 0.55% (not the 0.2% the 15m ATR would have produced).
func TestAnchorOffsetUsesOneHourATR(t *testing.T) {
	cfg := AnchorOffsetConfig{Mode: AnchorOffsetModeATR, ATRMult: 0.5, MinPct: 0.15, MaxPct: 1.2}
	if got := AnchorOffsetPct(1.10, cfg); got != 0.55 {
		t.Fatalf("offset = 0.5×ATR(1h)=0.55, got %.3f", got)
	}
	// Low-vol 1h still clamps to the floor.
	if got := AnchorOffsetPct(0.20, cfg); got != 0.15 {
		t.Fatalf("clamp floor, got %.3f", got)
	}
	// ATR unavailable → fixed fallback.
	if got := AnchorOffsetPct(0, cfg); got != 0.5 {
		t.Fatalf("fixed fallback 0.5, got %.3f", got)
	}
}
