package kernel

import (
	"math"
	"strings"
	"testing"
	"time"

	"nofx/market"
)

// ============================================================================
// Tests for the program-truth gate fields added after the 2026-09-15 cycle
// review: rr_scan / hard_entry_gate / bias / funding_rollover. The review
// cases (SAGA "最近1h阻力 RR≈0.4" phrasing, ZEC anchor-suppression meaning,
// PONS hidden triple block, AKE "空头证据强" wording, AINU unverifiable
// funding_rollover) are pinned here as regressions.
// ============================================================================

func tfWith(levels []float64, atrPct float64) *TFSignal {
	return &TFSignal{ATRPct: atrPct, Resistance: levels}
}

func tfWithSupport(levels []float64, atrPct float64) *TFSignal {
	return &TFSignal{ATRPct: atrPct, Support: levels}
}

// SAGA case (review point 1): the model declared failure off the nearest 1h
// resistance only; the program must scan ALL arrays and report the MAXIMUM
// structural RR (at the best-case floor stop) as the verdict.
func TestScanRRSagaMaxStructuralRR(t *testing.T) {
	tfs := map[string]*TFSignal{
		"5m":  {Resistance: []float64{0.021352, 0.02139}},
		"15m": {Resistance: []float64{0.021, 0.02139}},
		"1h":  {Resistance: []float64{0.021034}},
	}
	// entry = the shown limit anchor; floor = SLMinATRMult 1.5 × ATR(1h) 4.593 ≈ 6.89
	scan := scanRR(0.020501, "limit_anchor", 6.89, 0.020501*(1-6.89/100), tfs, true, 1.5)
	// 0.02139 appears twice across blocks; the 0.05%-tolerance dedup collapses
	// them. The 5m block's targets are EXCLUDED by the 15m-4h scan window
	// (09-19 audit 五-5): 15m(2) + 1h(1) = 3.
	if scan.TargetsScanned != 3 {
		t.Errorf("targets_scanned = %d, want 3 (dedup + 5m excluded)", scan.TargetsScanned)
	}
	if scan.BestTarget != 0.02139 {
		t.Errorf("best_target = %.6f, want 0.02139", scan.BestTarget)
	}
	wantBest := (0.02139 - 0.020501) / 0.020501 * 100 / 6.89
	if math.Abs(scan.BestRR-round2(wantBest)) > 0.011 {
		t.Errorf("best_rr = %.2f, want ≈%.2f (SAGA max structural RR ≈ 0.63)", scan.BestRR, wantBest)
	}
	if scan.Usable || scan.FirstRRGeTarget != 0 {
		t.Errorf("usable=%v first=%g, want false/0 — every level sits below RR 1.5 at the floor stop", scan.Usable, scan.FirstRRGeTarget)
	}
}

// The scan must also find the FIRST qualifying level (TP adoption rule) and
// work mirrored for shorts (CHIP support cluster).
func TestScanRRShortAndFirstQualifying(t *testing.T) {
	tfs := map[string]*TFSignal{
		"15m": {Support: []float64{0.03854, 0.038148}},
		"4h":  {Support: []float64{0.037931}},
	}
	scan := scanRR(0.0390875, "limit_anchor", 3.32, 0.0390875*(1+3.32/100), tfs, false, 1.5)
	if scan.Direction != "short" {
		t.Errorf("direction = %s", scan.Direction)
	}
	// CHIP-style squeeze: everything < 1.5 at the floor stop.
	if scan.Usable {
		t.Errorf("CHIP case must come out unusable, got first=%g best=%.2f", scan.FirstRRGeTarget, scan.BestRR)
	}
	if scan.StopPrice <= 0.0390875 {
		t.Errorf("short stop_price %.8f must sit above entry", scan.StopPrice)
	}

	// A qualifying near level wins even when a farther one exists: the pick is
	// the FIRST level clearing min RR, not the best RR (that's best_target's job).
	tfs2 := map[string]*TFSignal{
		"15m": {Support: []float64{0.0350, 0.0340}},
		"4h":  {Support: []float64{0.0300}},
	}
	scan2 := scanRR(0.0390875, "limit_anchor", 3.32, 0.0390875*(1+3.32/100), tfs2, false, 1.5)
	firstRR := (0.0390875 - 0.035) / 0.0390875 * 100 / 3.32
	if firstRR < 1.5 {
		t.Fatalf("fixture math wrong: first level RR %.2f should clear 1.5", firstRR)
	}
	if !scan2.Usable || scan2.FirstRRGeTarget != 0.035 {
		t.Errorf("first_rr_ge_target = %g usable=%v, want 0.035/true (nearest qualifying level)", scan2.FirstRRGeTarget, scan2.Usable)
	}
	if scan2.BestTarget != 0.030 {
		t.Errorf("best_target = %g, want 0.030 (farthest = max RR)", scan2.BestTarget)
	}
}

func sagaSignal() *SymbolSignal {
	return &SymbolSignal{
		Price:          0.02075,
		LimitBuyPrice:  0.020501,
		LimitSellPrice: 0.020999,
		Timeframes: map[string]*TFSignal{
			"1h":  {Trend: "up", ATRPct: 4.593, Resistance: []float64{0.021034}, Support: []float64{0.01814, 0.01781}},
			"15m": {Trend: "up", ATRPct: 3.579, Resistance: []float64{0.021, 0.02139}, Support: []float64{0.02006, 0.01989}},
			"5m":  {Trend: "up", ATRPct: 1.951, Resistance: []float64{0.021352, 0.02139}, Support: []float64{0.02074, 0.02023}},
		},
		ExecutionFilter: &ExecutionFilter{MicroTF: "15m", MicroTrend: "up", LongAllowed: true, ShortAllowed: false},
		DataQuality:     &DataQuality{Complete: true, Sufficient: true},
	}
}

func sagaOpt() SignalOptions {
	return SignalOptions{
		SLMinATRMult: 1.5, MinRR: 1.5,
		LimitEntryEnabled: true, EntryTimingGate: true,
	}
}

func has(list []string, prefix string) bool {
	for _, v := range list {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// SAGA verdict: long fails ONLY on the structural RR ceiling; short additionally
// on the micro-trend gate. The model must see complete machine lists.
func TestHardEntryGateSAGA(t *testing.T) {
	g := computeHardEntryGate(sagaSignal(), sagaOpt())
	l := g.Long
	if l.EntryBasis != "limit_anchor" || l.EntryPrice != 0.020501 {
		t.Errorf("long entry = %.6g (%s), want 0.020501 limit_anchor", l.EntryPrice, l.EntryBasis)
	}
	if !l.LimitAllowed {
		t.Error("long anchor present must keep limit_allowed=true")
	}
	if has(l.Failed, "MICRO_TREND") || has(l.Failed, "LIMIT_ANCHOR") {
		t.Errorf("long failed list carries wrong blockers: %v", l.Failed)
	}
	// 09-19 step-out: the nearest supports (0.02006/0.01989) sit 4-5% below
	// the anchor — under the 6.89% noise floor — and the next one out
	// (0.01814, 13.4%) overshoots the 8% cap. No structural stop lands in
	// the band → OUT_OF_BAND, not an RR verdict at a clamped fantasy stop.
	if !has(l.Failed, "STOP_PLAN_OUT_OF_BAND") || len(l.Failed) != 1 {
		t.Errorf("long failed = %v, want exactly STOP_PLAN_OUT_OF_BAND (no in-band structural stop)", l.Failed)
	}
	if l.RR != nil {
		t.Errorf("rr_scan must be absent without a stop plan, got %+v", l.RR)
	}
	if l.Allowed {
		t.Error("long must be blocked (no structural stop inside the band)")
	}
	s := g.Short
	if !has(s.Failed, "MICRO_TREND_NOT_SHORT") {
		t.Errorf("short failed = %v, want MICRO_TREND_NOT_SHORT", s.Failed)
	}
	if s.Allowed {
		t.Error("short must be blocked")
	}
}

// ZEC case (review point 7): anchor suppression is a LIMIT-path blocker —
// it must be spelled out separately from a direction ban, and the entry
// basis falls back to the live price so rr_scan still answers "could I chase
// a market exception at all".
func TestHardEntryGateAnchorSuppression(t *testing.T) {
	sig := &SymbolSignal{
		Price:         1128.83,
		LimitBuyPrice: 1119.6, LimitSellPrice: 0, // short anchor suppressed
		Timeframes: map[string]*TFSignal{
			"1h":  {Trend: "pullback", ATRPct: 1.634, Support: []float64{1126.25, 1114.96}, Resistance: []float64{1148.17, 1159.06}},
			"15m": {Trend: "down", ATRPct: 0.812, Support: []float64{1126.25, 1125.23}, Resistance: []float64{1141.81, 1145.45}},
		},
		ExecutionFilter: &ExecutionFilter{MicroTF: "15m", MicroTrend: "down", LongAllowed: false, ShortAllowed: true},
		DataQuality:     &DataQuality{Complete: true, Sufficient: true},
	}
	g := computeHardEntryGate(sig, sagaOpt())
	s := g.Short
	if s.LimitAllowed {
		t.Error("suppressed anchor must give limit_allowed=false")
	}
	if !has(s.Failed, "LIMIT_ANCHOR_SUPPRESSED") {
		t.Errorf("short failed = %v, want LIMIT_ANCHOR_SUPPRESSED", s.Failed)
	}
	if has(s.Failed, "MICRO_TREND") {
		t.Errorf("15m down permits shorts — no micro-trend blocker expected: %v", s.Failed)
	}
	if s.EntryBasis != "live_price" || s.EntryPrice != 1128.83 {
		t.Errorf("entry = %.6g (%s), want live_price basis after suppression", s.EntryPrice, s.EntryBasis)
	}
	// ZEC RR is also structurally short (support cluster 0.2% away) — the
	// point of the field is the model sees BOTH facts, not the anchor alone.
	if !has(s.Failed, "RR_MAX_") {
		t.Errorf("short failed = %v, want the RR ceiling too", s.Failed)
	}
	// Long side: micro-trend blocks it outright.
	if !has(g.Long.Failed, "MICRO_TREND_NOT_LONG") {
		t.Errorf("long failed = %v, want MICRO_TREND_NOT_LONG", g.Long.Failed)
	}
}

// PONS case (review point 5): hidden hard blocks must aggregate —
// data shortfall + min-size dead zone + loss streak, not just the direction
// reads the model happened to emphasize.
func TestHardEntryGateHiddenBlocksAggregate(t *testing.T) {
	sig := sagaSignal()
	sig.DataQuality.Sufficient = false
	sig.MinSize = &MinSizeCheck{Feasible: false, Reason: "dead"}
	sig.LossStreak = &LossStreakState{Banned: true, UntilUTC: "2026-09-16T00:00:00Z"}
	opt := sagaOpt()
	opt.StockWeekendBlock = true
	g := computeHardEntryGate(sig, opt)
	for dir, d := range map[string]*DirectionGate{"long": g.Long, "short": g.Short} {
		for _, want := range []string{"DATA_INSUFFICIENT", "MIN_SIZE_DEAD_ZONE", "LOSS_STREAK_BANNED", "STOCK_WEEKEND"} {
			if !has(d.Failed, want) {
				t.Errorf("%s failed = %v, missing %s", dir, d.Failed, want)
			}
		}
		if d.Allowed {
			t.Errorf("%s must not be allowed with four hard blockers", dir)
		}
	}
}

// With the timing gate disabled the micro-trend verdict is advisory (executor
// parity): it must NOT appear as a hard blocker.
func TestHardEntryGateTimingGateDisabledIsAdvisory(t *testing.T) {
	opt := sagaOpt()
	opt.EntryTimingGate = false
	g := computeHardEntryGate(sagaSignal(), opt)
	if has(g.Short.Failed, "MICRO_TREND_NOT_SHORT") {
		t.Errorf("gate disabled but MICRO_TREND blocker rendered: %v", g.Short.Failed)
	}
}

func TestBiasBlockSources(t *testing.T) {
	sig := sagaSignal()
	sig.SignalConflict = &SignalConflict{DirectionalScore: 33}
	b := computeBiasBlock(sig, SignalOptions{ScannerBias: "short"})
	if b.Scanner != "short" || b.Structure != "long" || b.Execution != "long_only" {
		t.Errorf("bias = %+v, want short/long/long_only (AKE case: scanner short vs bullish structure)", b)
	}
	b2 := computeBiasBlock(sig, SignalOptions{})
	if b2.Scanner != "none" {
		t.Errorf("scanner bias without a hint = %q, want none", b2.Scanner)
	}
	sig.ExecutionFilter = nil
	b3 := computeBiasBlock(sig, SignalOptions{})
	if b3.Execution != "unknown" {
		t.Errorf("execution without filter = %q, want unknown", b3.Execution)
	}
}

// Integration through ComputeSymbolSignals: the new blocks must render and the
// funding_rollover verdict must follow the settled history vs the live rate.
func TestComputeSignalsEmitsGateAndRollover(t *testing.T) {
	now := time.Date(2026, 9, 15, 14, 20, 0, 0, time.UTC)
	price := 0.0208
	data := &market.Data{
		Symbol:       "SAGAUSDT",
		CurrentPrice: price,
		FundingRate:  0.00005, FundingRateOK: true, FundingSettleHours: 4,
		// settled history (oldest→newest): high for four settlements, then two
		// low ones — a classic rollover.
		FundingHistory: []float64{0.0004, 0.0004, 0.0004, 0.0004, 0.00005, 0.00005},
		TimeframeData:  map[string]*market.TimeframeSeriesData{"1h": buildTF("1h", now, 60, price, false)},
	}
	opt := SignalOptions{
		Now: now, PrimaryTF: "1h",
		LimitEntryEnabled: true, EntryTimingGate: true, MinRR: 1.5,
		SLMinATRMult: 1.5, ShortFundingCrowdPctP: 0.03,
	}
	sig, err := ComputeSymbolSignals("SAGAUSDT", data, opt)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if sig.HardGate == nil || sig.HardGate.Long == nil || sig.HardGate.Short == nil {
		t.Fatal("hard_entry_gate missing from the computed signal")
	}
	if sig.Bias == nil {
		t.Fatal("bias block missing")
	}
	fr := sig.Derivatives.FundingRollover
	if fr == nil {
		t.Fatal("funding_rollover missing despite settled history")
	}
	if !fr.Detected {
		t.Errorf("detected=false, want true (prev 0.0004 > threshold 0.00015, live 0.00005 < prev)")
	}
	if fr.FromAnnualizedPct == nil || fr.ToAnnualizedPct == nil || *fr.FromAnnualizedPct <= *fr.ToAnnualizedPct {
		t.Errorf("from/to annualized not ordered: %+v", fr)
	}
	if fr.SettlesBack != 3 {
		t.Errorf("settles_back = %d, want 3", fr.SettlesBack)
	}

	// No history → field absent (UNKNOWN, not "not rolled over").
	data2 := &market.Data{
		Symbol: "SAGAUSDT", CurrentPrice: price,
		FundingRate: 0.00005, FundingRateOK: true, FundingSettleHours: 4,
		TimeframeData: map[string]*market.TimeframeSeriesData{"1h": buildTF("1h", now, 60, price, false)},
	}
	sig2, err := ComputeSymbolSignals("SAGAUSDT", data2, opt)
	if err != nil {
		t.Fatalf("compute2: %v", err)
	}
	if sig2.Derivatives.FundingRollover != nil {
		t.Errorf("funding_rollover must be OMITTED without history, got %+v", sig2.Derivatives.FundingRollover)
	}

	// Flat history: never above the threshold → detected=false, field still
	// present (verifiable negative ≠ absence).
	data3 := &market.Data{
		Symbol: "SAGAUSDT", CurrentPrice: price,
		FundingRate: 0.00005, FundingRateOK: true, FundingSettleHours: 4,
		FundingHistory: []float64{0.00002, 0.00002, 0.00002, 0.00002, 0.00002, 0.00002},
		TimeframeData:  map[string]*market.TimeframeSeriesData{"1h": buildTF("1h", now, 60, price, false)},
	}
	sig3, err := ComputeSymbolSignals("SAGAUSDT", data3, opt)
	if err != nil {
		t.Fatalf("compute3: %v", err)
	}
	if sig3.Derivatives.FundingRollover == nil || sig3.Derivatives.FundingRollover.Detected {
		t.Errorf("quiet history must render detected=false, got %+v", sig3.Derivatives.FundingRollover)
	}
}

// Without a configured noise floor the best-case RR is undefined — rr_scan
// must be absent rather than fabricated (align-strategy-prompts-with-executor:
// the prompt may not present defaults the executor does not enforce).
func TestHardEntryGateNoFloorSkipsRRScan(t *testing.T) {
	opt := sagaOpt()
	opt.SLMinATRMult = 0
	g := computeHardEntryGate(sagaSignal(), opt)
	if g.Long.RR != nil || g.Short.RR != nil {
		t.Errorf("rr_scan rendered without a noise floor: %+v / %+v", g.Long.RR, g.Short.RR)
	}
	if has(g.Long.Failed, "RR_MAX") {
		t.Errorf("RR blocker without a floor definition: %v", g.Long.Failed)
	}
}
