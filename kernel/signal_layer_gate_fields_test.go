package kernel

import (
	"math"
	"testing"
	"time"

	"nofx/market"
)

// Regression tests for the two program-truth gate fields added 2026-09-15:
// min_size (minimum-notional feasibility, mirrors the executor's
// CheckMinNotional rejection) and loss_streak (circuit-breaker verdict the
// model must read instead of self-judging).

func gateTestMarket(now time.Time, price float64) *market.Data {
	data := &market.Data{
		Symbol:       "CAPUSDT",
		CurrentPrice: price,
		FundingRate:  0.0001, FundingRateOK: true,
		TimeframeData: map[string]*market.TimeframeSeriesData{},
	}
	data.TimeframeData["1h"] = buildTF("1h", now, 60, price, false)
	return data
}

func TestMinSizeCheckFeasibleAndDeadZone(t *testing.T) {
	now := time.Date(2026, 9, 15, 4, 0, 0, 0, time.UTC)
	price := 0.0632
	opts := SignalOptions{
		Now:             now,
		PrimaryTF:       "1h",
		EquityUSDT:      52.9,
		RiskPct:         1.5,
		MinPositionSizeUSDT: 10.0, // strategy min_position_size as configured
		// No noise floor → the tightest allowed stop is a structure stop, so
		// the minimum is always reachable (feasible; the max-stop ceiling is
		// still surfaced for the model).
		SLMinATRMult: 0,
	}
	sig, err := ComputeSymbolSignals("CAPUSDT", gateTestMarket(now, price), opts)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if sig.MinSize == nil {
		t.Fatal("min_size block missing — equity/risk/minPositionSize inputs were provided")
	}
	wantMax := 52.9 * 1.5 / 100 / 10.0 * 100 // equity×risk% ÷ minPositionSize → d% ceiling
	if math.Abs(sig.MinSize.MaxStopPct-wantMax) > 0.01 {
		t.Errorf("max_stop_pct_for_min_size = %.2f, want %.2f", sig.MinSize.MaxStopPct, wantMax)
	}
	if !sig.MinSize.Feasible {
		t.Errorf("feasible = false without a stop floor (floor %.2f%% ≤ max %.2f%%)", sig.MinSize.StopFloorPct, sig.MinSize.MaxStopPct)
	}
	if sig.MinSize.Reason != "" {
		t.Errorf("feasible coin must not carry a reason, got %q", sig.MinSize.Reason)
	}
	if sig.MinSize.MinPositionSizeUsd != 10.0 {
		t.Errorf("min_position_size_usd = %.1f, want 10.0 (strategy config echoed)", sig.MinSize.MinPositionSizeUsd)
	}

	// Structural dead zone: a floor mult whose ×ATR(1h)% overshoots the
	// ceiling means NO allowed stop can meet the minimum (09-15 CAPUSDT case:
	// 10.4-11.4% floor vs 7.9% ceiling → 7-7.6U notional < 10U).
	opts.SLMinATRMult = 100
	sigDead, err := ComputeSymbolSignals("CAPUSDT", gateTestMarket(now, price), opts)
	if err != nil {
		t.Fatalf("compute dead zone: %v", err)
	}
	if sigDead.MinSize == nil || sigDead.MinSize.Feasible {
		t.Fatalf("dead zone not flagged: %+v", sigDead.MinSize)
	}
	atr1h := sigDead.Timeframes["1h"].ATRPct
	if math.Abs(sigDead.MinSize.StopFloorPct-100*atr1h) > 0.01 {
		t.Errorf("stop_floor_pct = %.2f, want SLMinATRMult×ATR(1h)%% = %.2f", sigDead.MinSize.StopFloorPct, 100*atr1h)
	}
	if sigDead.MinSize.Reason == "" {
		t.Error("infeasible coin must explain why (reason empty)")
	}
}

func TestMinSizeCheckOmittedWithoutInputs(t *testing.T) {
	now := time.Date(2026, 9, 15, 4, 0, 0, 0, time.UTC)
	sig, err := ComputeSymbolSignals("CAPUSDT", gateTestMarket(now, 0.0632), SignalOptions{Now: now, PrimaryTF: "1h"})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if sig.MinSize != nil {
		t.Errorf("min_size must be omitted without equity/risk/minPositionSize inputs, got %+v", sig.MinSize)
	}
}

func TestLossStreakSignalMirror(t *testing.T) {
	now := time.Date(2026, 9, 15, 4, 0, 0, 0, time.UTC)
	until := now.Add(17 * time.Hour)

	sig, err := ComputeSymbolSignals("CAPUSDT", gateTestMarket(now, 0.0632), SignalOptions{
		Now: now, PrimaryTF: "1h", LossStreakBannedUntil: until,
	})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if sig.LossStreak == nil || !sig.LossStreak.Banned {
		t.Fatalf("loss_streak verdict missing for banned symbol: %+v", sig.LossStreak)
	}
	if sig.LossStreak.UntilUTC == "" {
		t.Error("until_utc empty — the model needs the expiry to judge WAIT vs NO_SETUP")
	}
	if sig.LossStreak.HoursLeft < 16.9 || sig.LossStreak.HoursLeft > 17.1 {
		t.Errorf("hours_left = %.1f, want ~17.0", sig.LossStreak.HoursLeft)
	}

	sigClean, err := ComputeSymbolSignals("CAPUSDT", gateTestMarket(now, 0.0632), SignalOptions{Now: now, PrimaryTF: "1h"})
	if err != nil {
		t.Fatalf("compute clean: %v", err)
	}
	if sigClean.LossStreak != nil {
		t.Errorf("loss_streak must be OMITTED for non-banned symbols (its absence = 'not banned' contract), got %+v", sigClean.LossStreak)
	}
}
