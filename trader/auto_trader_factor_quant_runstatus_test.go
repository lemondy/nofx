package trader

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

func TestFactorQuantRunStatusTooSoonAndRing(t *testing.T) {
	at, f, broker := fqRiskTrader(t, "fq-run-ring")
	now := fqDay("2026-11-01")
	for i := 0; i < 15; i++ {
		f.equity = 10000 + float64(i)
		at.factorQuantTick(broker, now.Add(time.Duration(i)*factorQuantTickInterval))
		run := at.factorQuantRiskStatus().FactorQuant
		if run == nil || run.LastTick == nil || run.LastTick.Code != fqCodeTooSoon || run.LastTick.Message == "" {
			t.Fatalf("tick %d: missing too_soon outcome: %+v", i, run)
		}
		wantCount := i + 1
		if wantCount > 12 {
			wantCount = 12
		}
		if len(run.RecentTicks) != wantCount || !run.LastTick.At.Equal(now.Add(time.Duration(i)*factorQuantTickInterval)) {
			t.Fatalf("tick %d: wrong ring length or latest tick: %+v", i, run)
		}
		for j, tick := range run.RecentTicks {
			if !tick.At.Equal(now.Add(time.Duration(i-j)*factorQuantTickInterval)) || tick.Equity != 10000+float64(i-j) {
				t.Fatalf("tick %d, history %d: wrong order or equity: %+v", i, j, tick)
			}
		}
		// Mutating an API response must not change the next runtime snapshot.
		run.RecentTicks[0].Code = "changed by reader"
	}
	if len(f.calls) != 0 || len(fqRecords(t, at)) != 0 {
		t.Fatalf("observing too_soon changed orders or records: %v", f.calls)
	}
}

func TestFactorQuantRunStatusRebalanceAndStateFailure(t *testing.T) {
	for _, market := range []string{"crypto", "us"} {
		t.Run(market, func(t *testing.T) {
			at, _, broker := fqRiskTrader(t, "fq-run-rebalance")
			cfg := at.config.StrategyConfig.FactorQuantConfig
			cfg.Market = market
			horizon := 3
			if market == "us" {
				horizon = 5 // An unset US horizon follows the targets file.
			}
			cfg.TargetsPath = writeTargetsFile(t, filepath.Dir(cfg.TargetsPath), targetsJSON(market, "ok", "2026-11-05", horizon, `{"NEWUSDT":0.5}`))
			now := fqDay("2026-11-06")
			at.fqNow = func() time.Time { return now }
			at.factorQuantTick(broker, now)
			at.fqSetRetry("2026-11-05", 2)
			check := func(run *FactorQuantRunStatus) {
				t.Helper()
				if run == nil || run.LastTick == nil || run.LastTick.Code != "rebalanced" || len(run.RecentTicks) != 1 {
					t.Fatalf("missing rebalance tick: %+v", run)
				}
				if !strings.Contains(run.LastTick.Message, "笔下单") || !strings.Contains(run.LastTick.Message, "笔失败") || !strings.Contains(run.LastTick.Message, "个跳过") {
					t.Fatalf("rebalance summary lacks counts: %s", run.LastTick.Message)
				}
				wantNext := "2026-11-08"
				if market == "us" {
					wantNext = "2026-11-10"
				}
				if run.TargetsAsOf != "2026-11-05" || run.TargetsStatus != "ok" || run.TargetsRule != "ls_invvol_protected_v1" || run.LastRebalanceAsOf != "2026-11-05" || run.NextEligibleAsOf != wantNext || run.Horizon != horizon || run.TickIntervalSec != 300 || run.RetryAsOf != "2026-11-05" || run.RetryAttempts != 2 {
					t.Fatalf("wrong run metadata: %+v", run)
				}
			}
			run := at.factorQuantRiskStatus().FactorQuant
			check(run)
			if run.LastRebalanceAt == nil || !run.LastRebalanceAt.Equal(now) {
				t.Fatalf("wrong rebalance time: %+v", run.LastRebalanceAt)
			}
			if err := at.store.GormDB().Migrator().DropTable(&store.FactorQuantState{}); err != nil {
				t.Fatal(err)
			}
			status := at.factorQuantRiskStatus()
			if !status.DailyHaltUnknown {
				t.Fatal("state read failure not injected")
			}
			check(status.FactorQuant)
		})
	}
}

func TestFactorQuantRunStatusHalted(t *testing.T) {
	for _, kind := range []string{fqHaltDrawdown, fqHaltDaily} {
		t.Run(kind, func(t *testing.T) {
			at, f, broker := fqRiskTrader(t, "fq-run-halt")
			cfg := at.config.StrategyConfig.FactorQuantConfig
			cfg.DrawdownWarnPct = fqPtr(0)
			if kind == fqHaltDaily {
				cfg.MaxDrawdownPct = fqPtr(0)
			}
			now := fqDay("2026-11-01")
			at.factorQuantTick(broker, now)
			f.equity = 7000
			at.factorQuantTick(broker, now.Add(factorQuantTickInterval))
			tick := at.factorQuantRiskStatus().FactorQuant.LastTick
			if tick.Code != "halted" || !strings.Contains(tick.Message, kind) || !strings.Contains(tick.Message, "熔断清仓") {
				t.Fatalf("missing halt kind/reason: %+v", tick)
			}
		})
	}
}

type fqRunStatusBroker struct {
	fqBroker
	account func() (*fqAccount, error)
}

func (b fqRunStatusBroker) Account() (*fqAccount, error) { return b.account() }

func TestFactorQuantRunStatusRiskSkipAndPanic(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		account    func() (*fqAccount, error)
	}{
		{"account failure", "risk_skip", func() (*fqAccount, error) { return nil, errors.New("账户不可用") }},
		{"invalid equity", "risk_skip", func() (*fqAccount, error) { return &fqAccount{Equity: math.NaN()}, nil }},
		{"nil account", "risk_skip", func() (*fqAccount, error) { return nil, nil }},
		{"panic", "panic", func() (*fqAccount, error) { panic("测试异常") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, _, broker := fqRiskTrader(t, "fq-run-error")
			now := fqDay("2026-11-01")
			at.fqNow = func() time.Time { return now }
			at.factorQuantTickSafe(fqRunStatusBroker{fqBroker: broker, account: tc.account})
			run := at.factorQuantRiskStatus().FactorQuant
			if len(run.RecentTicks) != 1 || run.LastTick.Code != tc.code || run.LastTick.Message == "" || !run.LastTick.At.Equal(now) || run.LastTick.Equity != 0 {
				t.Fatalf("error tick missing or duplicated: %+v", run)
			}
			if tc.code == "panic" && run.LastTick.Message != "测试异常" {
				t.Fatalf("panic value lost: %+v", run.LastTick)
			}
			// Recovery and early returns must leave the next tick usable.
			now = now.Add(factorQuantTickInterval)
			at.factorQuantTickSafe(broker)
			run = at.factorQuantRiskStatus().FactorQuant
			if len(run.RecentTicks) != 2 || run.LastTick.Code != fqCodeTooSoon {
				t.Fatalf("next tick did not recover/reset: %+v", run)
			}
		})
	}
}
