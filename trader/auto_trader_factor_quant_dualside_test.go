package trader

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"nofx/logger"
)

func TestPlannerTwoSidedHoldings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		weights map[string]float64
		short   float64
		kind    string
		qty     float64
	}{
		{"keep short", map[string]float64{"XUSDT": -.5}, 5, "", 0},
		{"increase short", map[string]float64{"XUSDT": -.5}, 3, fqKindIncrease, 2},
		{"reduce short", map[string]float64{"XUSDT": -.5}, 7, fqKindReduce, 2},
		{"zero target", map[string]float64{"XUSDT": 0}, 5, fqKindClose, 5},
		{"absent target", map[string]float64{}, 5, fqKindClose, 5},
	} {
		for _, reverse := range []bool{false, true} {
			order := "long first"
			if reverse {
				order = "short first"
			}
			t.Run(tc.name+"/"+order, func(t *testing.T) {
				positions := []fqPosition{
					{Symbol: "XUSDT", Side: fqSideLong, Qty: 2, Mark: 100},
					{Symbol: "XUSDT", Side: fqSideShort, Qty: tc.short, Mark: 100},
				}
				if reverse {
					positions[0], positions[1] = positions[1], positions[0]
				}
				plan := fqPlanOrders(fqPlanInput{Positions: positions, Equity: 1000, CapitalFraction: 1,
					Prices: map[string]float64{"XUSDT": 100}, Weights: tc.weights, Band: .1, MinOrder: 10})
				wantCount := 1
				if tc.kind != "" {
					wantCount++
				}
				if len(plan.Orders) != wantCount {
					t.Fatalf("plan=%+v", plan)
				}
				close := plan.Orders[0]
				if close.Kind != fqKindClose || close.Side != fqSideLong || !close.CloseAll || close.Qty != 2 {
					t.Fatalf("long was not fully closed: %+v", close)
				}
				if tc.kind != "" {
					o := plan.Orders[1]
					if o.Kind != tc.kind || o.Side != fqSideShort || o.Qty != tc.qty || o.CloseAll != (tc.kind == fqKindClose) {
						t.Fatalf("short adjustment=%+v", o)
					}
				}
			})
		}
	}
}

func TestFactorQuantOrdinaryTickReconcilesTwoSidedHoldings(t *testing.T) {
	for _, tc := range []struct {
		name, weights string
		short         float64
	}{
		{"short", `{"XUSDT":-0.5}`, 5},
		{"zero", `{"XUSDT":0}`, 0},
		{"absent", `{}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := fqPaperTrader(t, fqTestStore(t), "fq-dualside", "crypto", &paperTestPrices{mark: 100})
			fqSetOwned(t, at, "2026-10-28")
			fqCheckRetry(t, at, "", 0)
			at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, tc.weights))
			f := newFakeFutures(1000, map[string]float64{"XUSDT": 100})
			f.pos["XUSDT:long"] = &fakePos{"XUSDT", fqSideLong, 2, 100}
			f.pos["XUSDT:short"] = &fakePos{"XUSDT", fqSideShort, 3, 100}
			var logs bytes.Buffer
			previousOutput := logger.Log.Out
			logger.Log.SetOutput(&logs)
			defer logger.Log.SetOutput(previousOutput)
			b := newFQFuturesBroker(f, 3, .2, .2)
			now := fqDay("2026-11-01")
			at.factorQuantTick(b, now)
			if f.pos["XUSDT:long"] != nil || fqCountCalls(f, "close_long XUSDT 0") != 1 {
				t.Fatalf("long leaked: positions=%+v calls=%v", f.pos, f.calls)
			}
			if tc.short == 0 {
				if len(f.pos) != 0 || fqCountCalls(f, "close_short XUSDT 0") != 1 {
					t.Fatalf("short leaked: positions=%+v calls=%v", f.pos, f.calls)
				}
			} else {
				if f.pos["XUSDT:short"] == nil {
					t.Fatal("target short was closed")
				}
				fqNear(t, f.pos["XUSDT:short"].qty, tc.short, "target short")
			}
			warning := "检测到双边持仓 XUSDT"
			note := at.factorQuantRiskStatus().FactorQuant.LastTick
			recs := fqRecords(t, at)
			if note.Code != "rebalanced" || !strings.Contains(note.Message, warning) || !strings.Contains(logs.String(), warning) || len(recs) != 1 || !strings.Contains(recs[0].CoTTrace, warning) {
				t.Fatalf("missing warning: tick=%+v records=%+v logs=%s", note, recs, logs.String())
			}
			fqCheckRetry(t, at, "", 0)
			calls := len(f.calls)
			at.factorQuantTick(b, now.Add(5*time.Minute))
			if len(f.calls) != calls || len(fqRecords(t, at)) != 1 {
				t.Fatalf("ordinary dedupe changed: %v", f.calls)
			}
		})
	}
}

func TestRebalanceNormalBookPlanUnchanged(t *testing.T) {
	f := newFakeFutures(1000, map[string]float64{"LONGUSDT": 100, "SHORTUSDT": 100, "GONEUSDT": 100})
	f.pos["LONGUSDT:long"] = &fakePos{"LONGUSDT", fqSideLong, 2, 100}
	f.pos["SHORTUSDT:short"] = &fakePos{"SHORTUSDT", fqSideShort, 7, 100}
	f.pos["GONEUSDT:long"] = &fakePos{"GONEUSDT", fqSideLong, 1, 100}
	b := newFQFuturesBroker(f, 3, .2, .2)
	before, err := b.Account()
	if err != nil {
		t.Fatal(err)
	}
	targets := fqTestTargets("2026-10-31", map[string]float64{"LONGUSDT": .5, "SHORTUSDT": -.5})
	want := fqPlanOrders(fqPlanInput{Positions: before.Positions, Equity: 1000, Prices: f.prices,
		Weights: targets.Weights, CapitalFraction: 1, Band: .1, MinOrder: 10, Format: b.FormatQty})
	rep := fqRunRebalance(b, targets, fqPlanConfig{CapitalFraction: 1, Band: .1, MinOrder: 10})
	if rep.Err != nil || len(rep.Warnings) != 0 || !reflect.DeepEqual(rep.Plan, want) || len(want.Orders) != 3 {
		t.Fatalf("normal plan changed: got=%+v want=%+v warnings=%v err=%v", rep.Plan, want, rep.Warnings, rep.Err)
	}
}

func TestFactorQuantNextAsOfReconcilesAfterRetryExhaustion(t *testing.T) {
	at := fqPaperTrader(t, fqTestStore(t), "fq-exhausted-flip", "crypto", &paperTestPrices{mark: 100})
	fqSetOwned(t, at, "2026-10-28")
	dir := t.TempDir()
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "ok", "2026-10-31", 3, `{"XUSDT":-0.5}`))
	f := newFakeFutures(1000, map[string]float64{"XUSDT": 100})
	f.pos["XUSDT:long"] = &fakePos{"XUSDT", fqSideLong, 2, 100}
	f.failClose["XUSDT"] = true
	b := newFQFuturesBroker(f, 3, .2, .2)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	for attempt := 1; attempt <= fqMaxRetryAttempts; attempt++ {
		at.factorQuantTick(b, now.Add(time.Duration(attempt)*factorQuantTickInterval))
	}
	fqCheckRetry(t, at, "", 0)
	if len(f.pos) != 2 {
		t.Fatalf("expected both sides after exhausted flip: %+v", f.pos)
	}
	f.failClose["XUSDT"] = false
	cfg.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "ok", "2026-11-03", 3, `{"XUSDT":-0.5}`))
	at.factorQuantTick(b, fqDay("2026-11-04"))
	state := fqCheckRetry(t, at, "", 0)
	if state.LastAsOf != "2026-11-03" || len(f.pos) != 1 || f.pos["XUSDT:short"] == nil || fqCountCalls(f, "close_long XUSDT 0") != fqMaxRetryAttempts+2 || fqCountCalls(f, "open_short XUSDT") != 1 {
		t.Fatalf("next ordinary tick failed to reconcile: state=%+v positions=%+v calls=%v", state, f.pos, f.calls)
	}
	fqNear(t, f.pos["XUSDT:short"].qty, 5, "retained short")
}

func TestTwoSidedSymbolsIgnoreNonpositiveQuantity(t *testing.T) {
	positions := []fqPosition{
		{Symbol: "Z", Side: fqSideLong, Qty: 1}, {Symbol: "Z", Side: fqSideShort, Qty: 1},
		{Symbol: "A", Side: fqSideShort, Qty: 1}, {Symbol: "A", Side: fqSideLong, Qty: 1},
		{Symbol: "ZERO", Side: fqSideLong, Qty: 1}, {Symbol: "ZERO", Side: fqSideShort},
		{Symbol: "NEG", Side: fqSideLong, Qty: -1}, {Symbol: "NEG", Side: fqSideShort, Qty: 1},
	}
	if got := fqTwoSidedSymbols(positions); !reflect.DeepEqual(got, []string{"A", "Z"}) {
		t.Fatalf("two-sided symbols=%v", got)
	}
}
