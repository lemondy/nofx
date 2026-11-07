package trader

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"nofx/logger"
	"nofx/store"
)

func fqEnableRisk(at *AutoTrader) *store.FactorQuantConfig {
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.MaxDrawdownPct, cfg.DrawdownWarnPct, cfg.DailyLossPct = nil, nil, nil
	return cfg
}

func fqRiskTrader(t *testing.T, id string) (*AutoTrader, *fakeFutures, *fqFuturesBroker) {
	t.Helper()
	at := fqPaperTrader(t, fqTestStore(t), id, "crypto", &paperTestPrices{mark: 100})
	cfg := fqEnableRisk(at)
	cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{"NEWUSDT":0.5}`))
	fqSetOwned(t, at, "2026-10-31")
	f := newFakeFutures(10000, map[string]float64{"XUSDT": 100, "YUSDT": 100, "NEWUSDT": 100})
	return at, f, newFQFuturesBroker(f, 3, 0.2, 0.2)
}

func fqRiskState(t *testing.T, at *AutoTrader) *store.FactorQuantState {
	t.Helper()
	state, err := at.fqState()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestFactorQuantRiskPeakSeedAndRatchet(t *testing.T) {
	for _, tc := range []struct {
		name, source              string
		historical, current, peak float64
	}{
		{"initial", "initial", 9000, 8000, 10000},
		{"snapshots", "fq-history", 12000, 11000, 12000},
		{"current", "current", 11000, 13000, 13000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, f, b := fqRiskTrader(t, "fq-peak")
			cfg := at.config.StrategyConfig.FactorQuantConfig
			cfg.MaxDrawdownPct, cfg.DrawdownWarnPct, cfg.DailyLossPct = fqPtr(0), fqPtr(0), fqPtr(0)
			start := fqDay("2026-10-30")
			at.saveFactorQuantRecord(fqBuildSkipRecord(fqTestTargets("2026-10-30", nil), fqDecision{Code: fqCodeStatus}, cfg, start))
			for _, snap := range []*store.EquitySnapshot{
				{TraderID: at.id, TotalEquity: 50000, Timestamp: start.Add(-time.Hour)}, // pre-fq strategy
				{TraderID: at.id, TotalEquity: tc.historical, Timestamp: fqDay("2026-10-30")},
				{TraderID: at.id, TotalEquity: tc.historical - 100, Timestamp: fqDay("2026-10-31")},
				{TraderID: "other", TotalEquity: 999999},
			} {
				if err := at.store.Equity().Save(snap); err != nil {
					t.Fatal(err)
				}
			}
			f.equity = tc.current
			now := fqDay("2026-11-01")
			var logs bytes.Buffer
			previousOutput := logger.Log.Out
			logger.Log.SetOutput(&logs)
			defer logger.Log.SetOutput(previousOutput)
			at.factorQuantTick(b, now)
			fqNear(t, fqRiskState(t, at).PeakEquity, tc.peak, "seeded peak")
			f.equity = tc.peak + 500
			at.factorQuantTick(b, now.Add(5*time.Minute))
			f.equity = tc.peak - 500
			at.factorQuantTick(b, now.Add(10*time.Minute))
			state := fqRiskState(t, at)
			fqNear(t, state.PeakEquity, tc.peak+500, "ratcheted peak")
			fqNear(t, state.DayStartEquity, tc.current, "unchanged daily baseline")
			durable, _ := at.store.FactorQuant().Get(at.id)
			fqNear(t, durable.PeakEquity, tc.peak+500, "durable peak")
			if strings.Count(logs.String(), "权益峰值初始化") != 1 || !strings.Contains(logs.String(), "source="+tc.source) || !strings.Contains(logs.String(), "at="+now.Format(time.RFC3339Nano)) || !strings.Contains(logs.String(), "fq_since="+start.Format(time.RFC3339Nano)) {
				t.Fatalf("missing/duplicate seed source: %s", logs.String())
			}
		})
	}
}

func TestFactorQuantRiskPeakQueryFailureFallsBack(t *testing.T) {
	for _, model := range []any{&store.EquitySnapshot{}, &store.DecisionRecordDB{}} {
		t.Run(reflect.TypeOf(model).Elem().Name(), func(t *testing.T) {
			at, f, b := fqRiskTrader(t, "fq-peak-fallback")
			at.saveFactorQuantRecord(fqBuildSkipRecord(fqTestTargets("2026-10-30", nil), fqDecision{Code: fqCodeStatus}, at.config.StrategyConfig.FactorQuantConfig, fqDay("2026-10-30")))
			if err := at.store.GormDB().Migrator().DropTable(model); err != nil {
				t.Fatal(err)
			}
			f.equity = 11000
			ready, halted := at.factorQuantRiskCheck(b, fqDay("2026-11-01"))
			fqNear(t, fqRiskState(t, at).PeakEquity, 11000, "fallback peak")
			if !ready || halted || len(f.calls) != 0 {
				t.Fatalf("fallback unexpectedly halted/traded: ready=%v halted=%v calls=%v", ready, halted, f.calls)
			}
		})
	}
}

func TestFactorQuantRiskReusedIDIgnoresPreFactorQuantEquity(t *testing.T) {
	for _, withMarker := range []bool{false, true} {
		for _, current := range []float64{10000, 11000} {
			t.Run(fmt.Sprintf("marker=%v/current=%g", withMarker, current), func(t *testing.T) {
				at, f, b := fqRiskTrader(t, "fq-reused-id")
				start := fqDay("2026-11-01")
				if err := at.store.Equity().Save(&store.EquitySnapshot{TraderID: at.id, Timestamp: start.Add(-time.Hour), TotalEquity: 50000}); err != nil {
					t.Fatal(err)
				}
				if withMarker {
					at.saveFactorQuantRecord(fqBuildSkipRecord(fqTestTargets("2026-11-01", nil), fqDecision{Code: fqCodeStatus}, at.config.StrategyConfig.FactorQuantConfig, start))
					if err := at.store.Equity().Save(&store.EquitySnapshot{TraderID: at.id, Timestamp: start, TotalEquity: 10000}); err != nil {
						t.Fatal(err)
					}
				}
				f.equity = current
				at.factorQuantTick(b, start)
				state := fqRiskState(t, at)
				fqNear(t, state.PeakEquity, math.Max(at.initialBalance, current), "reused id peak")
				if state.HaltKind != "" || state.WarnActive || len(f.calls) != 0 {
					t.Fatalf("pre-fq history affected risk: state=%+v calls=%v", state, f.calls)
				}
			})
		}
	}
}

func TestFactorQuantRiskHistoryPeakSurvivesStateLoss(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("row lost=%v", lost), func(t *testing.T) {
			at, f, b := fqRiskTrader(t, "fq-lost-peak")
			cfg := at.config.StrategyConfig.FactorQuantConfig
			cfg.DrawdownWarnPct, cfg.DailyLossPct = fqPtr(0), fqPtr(0)
			now := fqDay("2026-11-01")
			at.saveFactorQuantRecord(fqBuildSkipRecord(fqTestTargets("2026-10-31", nil), fqDecision{Code: fqCodeStatus}, cfg, now.Add(-time.Hour)))
			if err := at.store.Equity().Save(&store.EquitySnapshot{TraderID: at.id, Timestamp: now.Add(-2 * time.Hour), TotalEquity: 50000}); err != nil {
				t.Fatal(err)
			}
			f.equity = 14000
			at.factorQuantTick(b, now) // writes a real fq-era peak snapshot
			f.equity = 11000
			at.factorQuantTick(b, now.Add(5*time.Minute))
			if lost {
				if err := at.store.GormDB().Where("trader_id = ?", at.id).Delete(&store.FactorQuantState{}).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := at.store.FactorQuant().SetPeakEquity(at.id, 0); err != nil {
				t.Fatal(err)
			}
			at.fq = factorQuantRuntime{} // also discard the in-process risk snapshot
			f.equity = 10000
			at.factorQuantTick(b, now.Add(10*time.Minute))
			state := fqRiskState(t, at)
			fqNear(t, state.PeakEquity, 14000, "restored fq peak")
			if state.HaltKind != fqHaltDrawdown {
				t.Fatalf("real fq drawdown forgotten: %+v", state)
			}
		})
	}
}

func TestFactorQuantRiskWarningHysteresis(t *testing.T) {
	at, f, b := fqRiskTrader(t, "fq-warning")
	at.config.StrategyConfig.FactorQuantConfig.DailyLossPct = fqPtr(0)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	for i, equity := range []float64{8500, 8400, 9000, 9250} {
		f.equity = equity
		at.factorQuantTick(b, now.Add(time.Duration(i+1)*5*time.Minute))
		if !fqRiskState(t, at).WarnActive || len(fqRecords(t, at)) != 1 {
			t.Fatalf("warning flapped at equity %v", equity)
		}
	}
	f.equity = 9251 // strictly below warn/2, not at the boundary
	at.factorQuantTick(b, now.Add(25*time.Minute))
	if fqRiskState(t, at).WarnActive {
		t.Fatal("warning did not re-arm")
	}
	f.equity = 8500
	at.factorQuantTick(b, now.Add(30*time.Minute))
	recs := fqRecords(t, at)
	if len(recs) != 2 || len(f.calls) != 0 || !strings.Contains(recs[0].CoTTrace, "组合回撤预警") {
		t.Fatalf("warning records=%+v orders=%v", recs, f.calls)
	}
}

func TestFactorQuantRiskDrawdownLatchSurvivesRestart(t *testing.T) {
	at, f, b := fqRiskTrader(t, "fq-drawdown")
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.DrawdownWarnPct, cfg.DailyLossPct = fqPtr(0), fqPtr(0)
	f.pos["XUSDT:long"], f.pos["XUSDT:short"] = &fakePos{"XUSDT", "long", 10, 100}, &fakePos{"XUSDT", "short", 10, 100}
	f.stops["XUSDT:LONG"], f.stops["XUSDT:SHORT"] = 80, 120
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	at.fqSetRetry("2026-10-31", 2)
	f.equity = 7500
	at.factorQuantTick(b, now.Add(5*time.Minute))
	state := fqRiskState(t, at)
	if state.HaltKind != fqHaltDrawdown || state.HaltedAt != now.Add(5*time.Minute).UnixMilli() || len(f.pos) != 0 || len(f.stops) != 0 || state.RetryAsOf != "" {
		t.Fatalf("kill failed: state=%+v positions=%v stops=%v", state, f.pos, f.stops)
	}
	recs := fqRecords(t, at)
	if len(recs) != 1 || !recs[0].Success || len(recs[0].Decisions) != 2 {
		t.Fatalf("kill record=%+v", recs)
	}
	// Fresh targets and recovered equity cannot reopen a latched account.
	cfg.TargetsPath = writeTargetsFile(t, filepath.Dir(cfg.TargetsPath), targetsJSON("crypto", "ok", "2026-11-03", 3, `{"NEWUSDT":0.5}`))
	f.equity = 11000
	at.factorQuantTick(b, fqDay("2026-11-04"))
	restarted := fqPaperTrader(t, at.store, at.id, "crypto", &paperTestPrices{mark: 100})
	rcfg := fqEnableRisk(restarted)
	rcfg.DrawdownWarnPct, rcfg.DailyLossPct, rcfg.TargetsPath = fqPtr(0), fqPtr(0), cfg.TargetsPath
	restarted.factorQuantTick(b, fqDay("2026-11-04").Add(5*time.Minute))
	if fqRiskState(t, restarted).HaltKind != fqHaltDrawdown || fqCountCalls(f, "open_") != 0 || len(fqRecords(t, at)) != 1 {
		t.Fatalf("latch lost/re-recorded on restart: calls=%v", f.calls)
	}
	if count, err := at.store.Equity().GetCount(at.id); err != nil || count != 4 {
		t.Fatalf("halt stopped equity snapshots: count=%d err=%v", count, err)
	}
	// Startup with an empty, halted account must not re-place paper stops.
	restarted.factorQuantInitStops(b)
	if fqCountCalls(f, "stop ") != 0 || len(f.stops) != 0 {
		t.Fatalf("startup placed stops without positions: %v", f.calls)
	}
}

func TestFactorQuantRiskFlattenRetriesDeduplicate(t *testing.T) {
	at, f, b := fqRiskTrader(t, "fq-kill-retry")
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.DrawdownWarnPct, cfg.DailyLossPct = fqPtr(0), fqPtr(0)
	f.pos["XUSDT:short"], f.pos["YUSDT:long"] = &fakePos{"XUSDT", "short", 10, 100}, &fakePos{"YUSDT", "long", 10, 100}
	f.failClose["XUSDT"] = true
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	f.equity = 7500
	for i := 1; i <= 3; i++ {
		at.factorQuantTick(b, now.Add(time.Duration(i)*5*time.Minute))
	}
	recs := fqRecords(t, at)
	if len(recs) != 1 || recs[0].Success || fqCountCalls(f, "close_short XUSDT") != 3 || fqCountCalls(f, "close_long YUSDT") != 1 {
		t.Fatalf("retries not deduplicated: records=%+v calls=%v", recs, f.calls)
	}
	// The failure key also survives a new runtime.
	at.fq = factorQuantRuntime{}
	at.factorQuantTick(b, now.Add(20*time.Minute))
	if len(fqRecords(t, at)) != 1 {
		t.Fatal("restart repeated failed flatten record")
	}
	f.failClose["XUSDT"] = false
	at.factorQuantTick(b, now.Add(25*time.Minute))
	at.factorQuantTick(b, now.Add(30*time.Minute))
	recs = fqRecords(t, at)
	if len(recs) != 2 || !recs[0].Success || len(f.pos) != 0 || !strings.Contains(recs[0].CoTTrace, "清仓完成") {
		t.Fatalf("completion not recorded exactly once: %+v", recs)
	}
}

func TestFactorQuantRiskResetIsIdempotent(t *testing.T) {
	at, f, b := fqRiskTrader(t, "fq-reset")
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.DailyLossPct, cfg.DrawdownWarnPct = fqPtr(0), fqPtr(0)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	f.equity = 7500
	at.factorQuantTick(b, now.Add(5*time.Minute))
	cfg.KillResetID = "reset-1"
	at.factorQuantTick(b, now.Add(10*time.Minute))
	state := fqRiskState(t, at)
	if state.HaltKind != "" || state.PeakEquity != 7500 || state.WarnActive || state.KillResetApplied != "reset-1" {
		t.Fatalf("reset failed: %+v", state)
	}
	f.equity = 8000
	at.factorQuantTick(b, now.Add(15*time.Minute))
	f.equity = 7000
	at.factorQuantTick(b, now.Add(20*time.Minute))
	if fqRiskState(t, at).PeakEquity != 8000 || len(fqRecords(t, at)) != 2 {
		t.Fatal("same reset ID reapplied")
	}
	// The same ID remains applied after losing all in-memory state.
	at.fq = factorQuantRuntime{}
	at.factorQuantTick(b, now.Add(25*time.Minute))
	if fqRiskState(t, at).PeakEquity != 8000 || len(fqRecords(t, at)) != 2 {
		t.Fatal("reset ID not durable")
	}
	cfg.KillResetID = "reset-2"
	at.factorQuantTick(b, now.Add(30*time.Minute))
	recs := fqRecords(t, at)
	if fqRiskState(t, at).PeakEquity != 7000 || len(recs) != 3 || !strings.Contains(recs[0].CoTTrace, "熔断已手动复位") {
		t.Fatalf("new reset ID ignored: %+v", recs)
	}
}

func TestFactorQuantRiskDailyUTCExpiryAndNormalGate(t *testing.T) {
	at, f, b := fqRiskTrader(t, "fq-daily")
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.Horizon, cfg.MaxDrawdownPct, cfg.DrawdownWarnPct = 1, fqPtr(0), fqPtr(0)
	fqSetOwned(t, at, "2026-11-01")
	cfg.TargetsPath = writeTargetsFile(t, filepath.Dir(cfg.TargetsPath), targetsJSON("crypto", "ok", "2026-11-01", 1, `{"NEWUSDT":0.5}`))
	f.pos["XUSDT:long"] = &fakePos{"XUSDT", "long", 10, 100}
	// Local date is already Nov 2; the anchor and halt must still use UTC Nov 1.
	now := time.Date(2026, 11, 1, 23, 45, 0, 0, time.UTC).In(time.FixedZone("UTC+8", 8*3600))
	at.factorQuantTick(b, now)
	f.equity = 9300 // exact 7% loss
	at.factorQuantTick(b, now.Add(5*time.Minute))
	state := fqRiskState(t, at)
	if state.HaltKind != fqHaltDaily || state.HaltDay != "2026-11-01" || state.DayStartDate != "2026-11-01" || len(f.pos) != 0 {
		t.Fatalf("daily halt failed: %+v positions=%v", state, f.pos)
	}
	f.equity = 10000
	cfg.KillResetID = "does-not-clear-daily"
	at.factorQuantTick(b, now.Add(10*time.Minute))
	at.fq = factorQuantRuntime{}
	at.factorQuantTick(b, now.Add(14*time.Minute))
	if fqRiskState(t, at).HaltKind != fqHaltDaily || fqCountCalls(f, "open_") != 0 {
		t.Fatal("recovery, reset or restart cleared daily halt")
	}
	at.factorQuantTick(b, now.Add(15*time.Minute)) // UTC midnight
	state = fqRiskState(t, at)
	if state.HaltKind != "" || state.HaltDay != "" || state.DayStartDate != "2026-11-02" || state.DayStartEquity != 10000 || fqCountCalls(f, "open_") != 0 {
		t.Fatalf("expiry/same as_of gate failed: %+v calls=%v", state, f.calls)
	}
	cfg.TargetsPath = writeTargetsFile(t, filepath.Dir(cfg.TargetsPath), targetsJSON("crypto", "ok", "2026-11-02", 1, `{"NEWUSDT":0.5}`))
	at.factorQuantTick(b, now.Add(20*time.Minute))
	if fqCountCalls(f, "open_long NEWUSDT") != 1 || fqRiskState(t, at).LastAsOf != "2026-11-02" {
		t.Fatalf("new as_of did not rebalance: %v", f.calls)
	}
}

func TestFactorQuantRiskDrawdownUpgradesDaily(t *testing.T) {
	at, f, b := fqRiskTrader(t, "fq-upgrade")
	at.config.StrategyConfig.FactorQuantConfig.DrawdownWarnPct = fqPtr(0)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	f.equity = 9300
	at.factorQuantTick(b, now.Add(5*time.Minute))
	if fqRiskState(t, at).HaltKind != fqHaltDaily {
		t.Fatal("daily halt not triggered")
	}
	f.equity = 7500
	at.factorQuantTick(b, now.Add(10*time.Minute))
	at.factorQuantTick(b, fqDay("2026-11-02"))
	if state := fqRiskState(t, at); state.HaltKind != fqHaltDrawdown || state.HaltDay != "" {
		t.Fatalf("drawdown did not override daily halt: %+v", state)
	}
}

type fqRiskAccountErrorBroker struct{ fqBroker }

func (b fqRiskAccountErrorBroker) Account() (*fqAccount, error) {
	return nil, errors.New("account unavailable")
}

func TestFactorQuantRiskAccountErrorSkipsEntireTick(t *testing.T) {
	at, f, b := fqRiskTrader(t, "fq-account-error")
	before := *fqRiskState(t, at)
	at.config.StrategyConfig.FactorQuantConfig.KillResetID = "must-not-apply"
	at.factorQuantTick(fqRiskAccountErrorBroker{b}, fqDay("2026-11-01"))
	after := *fqRiskState(t, at)
	count, err := at.store.Equity().GetCount(at.id)
	if !reflect.DeepEqual(before, after) || len(f.calls) != 0 || len(fqRecords(t, at)) != 0 || err != nil || count != 0 || at.fq.riskSet {
		t.Fatalf("account failure mutated state/traded: before=%+v after=%+v calls=%v snapshots=%d", before, after, f.calls, count)
	}
}

func TestFactorQuantRiskStoreFailureKeepsMemoryHalt(t *testing.T) {
	at, f, b := fqRiskTrader(t, "fq-store-failure")
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.DrawdownWarnPct, cfg.DailyLossPct = fqPtr(0), fqPtr(0)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	// Setters use INSERT ... ON CONFLICT; fail writes while allowing reads.
	if err := at.store.GormDB().Exec("CREATE TRIGGER fq_fail_write BEFORE INSERT ON factor_quant_state BEGIN SELECT RAISE(FAIL, 'risk write failed'); END").Error; err != nil {
		t.Fatal(err)
	}
	f.equity = 7500
	at.factorQuantTick(b, now.Add(5*time.Minute))
	durable, err := at.store.FactorQuant().Get(at.id)
	if err != nil || durable.HaltKind != "" || fqRiskState(t, at).HaltKind != fqHaltDrawdown {
		t.Fatalf("failure injection/memory halt failed: durable=%+v err=%v", durable, err)
	}
	cfg.TargetsPath = writeTargetsFile(t, filepath.Dir(cfg.TargetsPath), targetsJSON("crypto", "ok", "2026-11-03", 3, `{"NEWUSDT":0.5}`))
	f.equity = 11000
	at.factorQuantTick(b, fqDay("2026-11-04"))
	if fqRiskState(t, at).HaltKind != fqHaltDrawdown || len(f.calls) != 0 || len(fqRecords(t, at)) != 1 {
		t.Fatalf("failed persistence reopened trading: %v", f.calls)
	}
}

func TestFactorQuantRiskDisabledRetainsNormalRebalance(t *testing.T) {
	at, f, b := fqRiskTrader(t, "fq-disabled")
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.MaxDrawdownPct, cfg.DrawdownWarnPct, cfg.DailyLossPct = fqPtr(0), fqPtr(0), fqPtr(0)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	f.equity = 1000
	cfg.TargetsPath = writeTargetsFile(t, filepath.Dir(cfg.TargetsPath), targetsJSON("crypto", "ok", "2026-11-03", 3, `{"NEWUSDT":0.5}`))
	at.factorQuantTick(b, fqDay("2026-11-04"))
	if state := fqRiskState(t, at); state.HaltKind != "" || state.WarnActive || fqCountCalls(f, "open_long NEWUSDT") != 1 || len(fqRecords(t, at)) != 1 {
		t.Fatalf("disabled breakers changed trading: state=%+v calls=%v", state, f.calls)
	}
}

func TestFactorQuantRiskUSFlattensHoldings(t *testing.T) {
	quotes := map[string]float64{"AAPLBUSDT": 100}
	fqStubUS(t, quotes)
	at := fqPaperTrader(t, fqTestStore(t), "fq-us-kill", "us", &paperTestPrices{mark: 100})
	cfg := fqEnableRisk(at)
	cfg.DrawdownWarnPct, cfg.DailyLossPct, cfg.USFeeBps, cfg.USSlippageBps = fqPtr(0), fqPtr(0), fqPtr(0), fqPtr(0)
	cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("us", "ok", "2026-11-01", 1, `{"AAPLBUSDT":1}`))
	b, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	quotes["AAPLBUSDT"] = 75
	at.factorQuantTick(b, now.Add(5*time.Minute))
	open, err := at.store.StockPaper().ListOpen(at.id)
	if err != nil || len(open) != 0 || fqRiskState(t, at).HaltKind != fqHaltDrawdown {
		t.Fatalf("US kill failed: positions=%+v err=%v", open, err)
	}
	acct, _ := at.store.StockPaper().GetAccount(at.id)
	fqNear(t, acct.Cash, 7500, "US liquidation proceeds")
	recs := fqRecords(t, at)
	if len(recs) != 2 || !recs[0].Success || len(recs[0].Decisions) != 1 || recs[0].Decisions[0].Action != "close_long" {
		t.Fatalf("US sell record=%+v", recs)
	}
}

func TestFactorQuantRiskStatus(t *testing.T) {
	for _, kind := range []string{"", fqHaltDaily, fqHaltDrawdown} {
		t.Run(kind, func(t *testing.T) {
			at, _, _ := fqRiskTrader(t, "fq-status")
			now := fqDay("2026-11-01")
			at.fqNow = func() time.Time { return now }
			for _, save := range []func() error{
				func() error { return at.store.FactorQuant().SetPeakEquity(at.id, 12500) },
				func() error { return at.store.FactorQuant().SetDayStart(at.id, "2026-11-01", 11000) },
				func() error { return at.store.FactorQuant().SetHalt(at.id, kind, "2026-11-01", "测试", 123) },
			} {
				if err := save(); err != nil {
					t.Fatal(err)
				}
			}
			status := at.RiskStatus() // actual paper account equity is 10000
			if status.AccountDrawdownBasis != "peak" || status.AccountDrawdownCap != 25 || status.DayStartEquity != 11000 || status.AccountBreakerActive != (kind == fqHaltDrawdown) || status.DailyHalted != (kind == fqHaltDaily) || status.ReduceOnly != (kind != "") {
				t.Fatalf("risk status=%+v", status)
			}
			fqNear(t, status.AccountDrawdownPct, 20, "peak drawdown percent")
			fqNear(t, status.DailyLossCapPct, 7, "daily loss cap percent")
			fqNear(t, status.DailyLossPct, 100.0/11, "daily loss percent")
			cfg := at.config.StrategyConfig.FactorQuantConfig
			cfg.MaxDrawdownPct, cfg.DailyLossPct = fqPtr(0), fqPtr(0)
			status = at.RiskStatus()
			if status.DailyLossCapPct != 0 || status.AccountDrawdownCap != 0 {
				t.Fatal("disabled caps not exposed as zero")
			}
			// A newer in-memory halt wins over the persisted read model.
			state := fqRiskState(t, at)
			state.HaltKind, state.PeakEquity = fqHaltDrawdown, 20000
			at.fqSaveRisk(state, func(*store.FactorQuantStore) error { return errors.New("write failed") })
			status = at.RiskStatus()
			if !status.AccountBreakerActive || !status.ReduceOnly || math.Abs(status.AccountDrawdownPct-50) > 1e-6 {
				t.Fatalf("read model ignored memory: %+v", status)
			}
		})
	}
}

type fqRiskFixedAccountBroker struct {
	fqBroker
	acct *fqAccount
}

func (b fqRiskFixedAccountBroker) Account() (*fqAccount, error) { return b.acct, nil }

// A bad quote (mark 0) or a zero equity read must never fire a liquidation.
func TestFactorQuantRiskUnreliableEquityNeverHalts(t *testing.T) {
	for _, tc := range []struct {
		name string
		acct *fqAccount
	}{
		{"zero mark", &fqAccount{Equity: 1000, Positions: []fqPosition{{Symbol: "XUSDT", Side: fqSideLong, Qty: 1, Entry: 100, Mark: 0}}}},
		{"zero equity", &fqAccount{Equity: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, _, b := fqRiskTrader(t, "fq-unreliable")
			now := fqDay("2026-11-01")
			at.factorQuantTick(fqRiskFixedAccountBroker{fqBroker: b, acct: &fqAccount{Equity: 10000}}, now)
			at.factorQuantTick(fqRiskFixedAccountBroker{fqBroker: b, acct: tc.acct}, now.Add(5*time.Minute))
			state := fqRiskState(t, at)
			if state.HaltKind != "" || state.WarnActive {
				t.Fatalf("unreliable equity must not halt or warn: %+v", state)
			}
			fqNear(t, state.PeakEquity, 10000, "peak unchanged")
			fqNear(t, state.DayStartEquity, 10000, "day start unchanged")
		})
	}
}
