package trader

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"nofx/kernel"
	"nofx/store"
)

type accountDrawdownExchange struct {
	Trader
	equity float64
}

func (m *accountDrawdownExchange) GetBalance() (map[string]interface{}, error) {
	return map[string]interface{}{"totalEquity": m.equity}, nil
}

func (m *accountDrawdownExchange) GetPositions() ([]map[string]interface{}, error) {
	return nil, nil
}

func accountDrawdownTestTrader(t *testing.T, basis string) *AutoTrader {
	t.Helper()
	at := riskTestTrader(store.RiskControlConfig{
		AccountDrawdownBasis: basis, AccountMaxDrawdownPct: 10,
		DailyMaxLossPct: -1, MaxAccountRiskPct: -1, MaxNetDirectionalRiskPct: -1,
		EarlyCloseMinHours: -1,
	})
	at.id, at.userID, at.exchangeID = "llm", "u", t.Name()
	at.initialBalance = 10000
	at.trader = &accountDrawdownExchange{equity: 10000}
	t.Cleanup(func() {
		unregisterAccountTrader(at)
		dailyBaselineRegMu.Lock()
		delete(dailyBaselineReg, "peak:llm_peak:"+at.dailyBaselineKey())
		dailyBaselineRegMu.Unlock()
	})
	return at
}

func TestLLMAccountDrawdownBasis(t *testing.T) {
	for _, tc := range []struct {
		name, basis, want string
		equity            float64
	}{
		{"default after profit", "", "", 11500},
		{"initial after profit", "initial", "", 11500},
		{"peak after profit", "peak", "account drawdown 11.54% from peak 13000.00 ≥ cap 10.0% — reduce-only mode", 11500},
		{"default at cap", "", "account drawdown 10.00% ≥ cap 10.0% — reduce-only mode", 9000},
		{"initial at cap", "initial", "account drawdown 10.00% ≥ cap 10.0% — reduce-only mode", 9000},
		{"peak at cap", "peak", "account drawdown 10.00% from peak 13000.00 ≥ cap 10.0% — reduce-only mode", 11700},
		{"peak recovered", "peak", "", 12000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := accountDrawdownTestTrader(t, tc.basis)
			if reason := at.accountOpenHaltReason(13000); reason != "" {
				t.Fatalf("rising equity blocked: %s", reason)
			}
			if reason := at.accountOpenHaltReason(tc.equity); reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

func TestLLMAccountPeakRatchetAndSnapshot(t *testing.T) {
	at := accountDrawdownTestTrader(t, "peak")
	// Normal cycle entry point, including a hold/close-only cycle with cap off.
	at.config.StrategyConfig.RiskControl.AccountMaxDrawdownPct = 0
	at.saveEquitySnapshot(&kernel.Context{Account: kernel.AccountInfo{TotalEquity: 13000}})
	for _, tc := range []struct{ equity, peak float64 }{
		{12000, 13000}, {14000, 14000}, {8000, 14000},
	} {
		got, err := at.accountPeakEquity(tc.equity, true)
		if err != nil || got != tc.peak {
			t.Fatalf("equity %v peak = %v, %v, want %v", tc.equity, got, err, tc.peak)
		}
	}
	at.config.StrategyConfig.RiskControl.AccountMaxDrawdownPct = 10
	if reason := at.accountOpenHaltReason(12000); !strings.Contains(reason, "from peak 14000.00") {
		t.Fatalf("peak reset after enabling cap: %s", reason)
	}
}

func TestLLMAccountPeakSurvivesRestart(t *testing.T) {
	for _, source := range []string{"durable high water", "own history", "inactive peer history"} {
		t.Run(source, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "restart.db")
			st, err := store.New(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			at := accountDrawdownTestTrader(t, "peak")
			at.store = st
			if source == "durable high water" {
				if reason := at.accountOpenHaltReason(13000); reason != "" {
					t.Fatal(reason)
				}
			} else {
				historicalID := "inactive"
				if source == "own history" {
					historicalID = at.id
				}
				if err := st.Trader().Create(&store.Trader{ID: historicalID, UserID: at.userID, ExchangeID: at.exchangeID}); err != nil {
					t.Fatal(err)
				}
				if err := st.Equity().Save(&store.EquitySnapshot{TraderID: historicalID, TotalEquity: 13000}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = store.New(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ResetDailyBaselineRegistryForTest() // simulate loss of all process anchors
			reloaded := accountDrawdownTestTrader(t, "peak")
			reloaded.store = st
			reloaded.id = "new-instance"
			if reason := reloaded.accountOpenHaltReason(11500); !strings.Contains(reason, "from peak 13000.00") {
				t.Fatalf("restart cleared peak: %q", reason)
			}
			peak, found, err := st.RiskState().DayBaseline("llm_peak:"+at.dailyBaselineKey(), "__peak__")
			if err != nil || !found || peak != 13000 {
				t.Fatalf("durable peak = %v, %v, %v", peak, found, err)
			}
		})
	}
}

func TestLLMAccountDrawdownPendingPeer(t *testing.T) {
	for _, basis := range []string{"", "initial", "peak"} {
		t.Run("basis="+basis, func(t *testing.T) {
			peer := accountDrawdownTestTrader(t, basis)
			caller := accountDrawdownTestTrader(t, "")
			caller.id = "caller"
			caller.config.StrategyConfig.RiskControl.AccountMaxDrawdownPct = 0
			registerAccountTrader(caller)
			registerAccountTrader(peer)
			peer.saveEquitySnapshot(&kernel.Context{Account: kernel.AccountInfo{TotalEquity: 13000}})
			caller.trader.(*accountDrawdownExchange).equity = 11500
			reason := caller.pendingAccountHaltReason()
			if (reason != "") != (basis == "peak") {
				t.Fatalf("pending peer basis %q reason = %q", basis, reason)
			}
			if basis == "peak" && !strings.Contains(reason, "from peak 13000.00") {
				t.Fatalf("peak basis missing in reason: %q", reason)
			}
			caller.trader.(*accountDrawdownExchange).equity = 9000
			reason = caller.pendingAccountHaltReason()
			if basis != "peak" && reason != "account drawdown breaker active" {
				t.Fatalf("legacy pending reason changed: %q", reason)
			}
		})
	}
}

func TestLLMAccountDrawdownHardGatesAllowReductions(t *testing.T) {
	for _, basis := range []string{"", "initial", "peak"} {
		t.Run("basis="+basis, func(t *testing.T) {
			at := accountDrawdownTestTrader(t, basis)
			at.saveEquitySnapshot(&kernel.Context{Account: kernel.AccountInfo{TotalEquity: 13000}})
			equity := 9000.0
			if basis == "peak" {
				equity = 11500
			}
			ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: equity}, CandidateCoins: []kernel.CandidateCoin{{Symbol: "YUSDT"}}}
			var decisions []kernel.Decision
			for _, action := range []string{"open_long", "open_short", "open_long_limit", "open_short_limit", "close_long", "close_short", "reduce_long", "reduce_short", "hold"} {
				decisions = append(decisions, kernel.Decision{Symbol: "YUSDT", Action: action})
			}
			got := at.applyHardRiskGates(decisions, ctx)
			if !reflect.DeepEqual(got, decisions[4:]) {
				t.Fatalf("breaker changed reductions: %+v", got)
			}
			for _, d := range decisions[:4] {
				reason := at.popFilterReason(d)
				want := "account drawdown ≥ cap — reduce-only mode"
				if basis == "peak" {
					want = "account drawdown 11.54% from peak 13000.00 ≥ cap 10.0% — reduce-only mode"
				}
				if reason != want {
					t.Fatalf("%s reason = %q, want %q", d.Action, reason, want)
				}
			}
		})
	}
}

func TestLLMAccountDrawdownStatusIsReadOnly(t *testing.T) {
	for _, basis := range []string{"", "initial", "peak"} {
		t.Run("basis="+basis, func(t *testing.T) {
			at := accountDrawdownTestTrader(t, basis)
			at.store = profitLockTestStore(t)
			if _, err := at.store.RiskState().UpdatePeak("llm_peak:"+at.dailyBaselineKey(), 13000); err != nil {
				t.Fatal(err)
			}
			at.trader.(*accountDrawdownExchange).equity = 11500
			status := at.RiskStatus()
			if basis == "peak" {
				if status.AccountDrawdownBasis != "peak" || status.AccountPeakEquity != 13000 || !status.AccountBreakerActive || status.AccountDrawdownUnknown {
					t.Fatalf("peak status = %+v", status)
				}
			} else if status.AccountDrawdownBasis != "initial" || status.AccountDrawdownPct != -15 || status.AccountBreakerActive || status.AccountPeakEquity != 0 {
				t.Fatalf("legacy status changed: %+v", status)
			}
			at.trader.(*accountDrawdownExchange).equity = 15000
			if status = at.RiskStatus(); basis == "peak" && status.AccountPeakEquity != 15000 {
				t.Fatalf("status omitted current equity: %+v", status)
			}
			persisted, _, err := at.store.RiskState().DayBaseline("llm_peak:"+at.dailyBaselineKey(), "__peak__")
			if err != nil || persisted != 13000 {
				t.Fatalf("dashboard mutated durable peak: %v, %v", persisted, err)
			}
		})
	}
}

func TestLLMAccountPeakStoreFailureBlocksOnlyNewRisk(t *testing.T) {
	for _, table := range []string{"risk_baselines", "trader_equity_snapshots"} {
		t.Run(table, func(t *testing.T) {
			at := accountDrawdownTestTrader(t, "peak")
			at.store = profitLockTestStore(t)
			review28QueryFailure(t, at.store, table)
			// Scan uses Row callbacks; close the database to also cover the
			// historical MAX query without adding a special production seam.
			if table == "trader_equity_snapshots" {
				if err := at.store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if reason := at.accountOpenHaltReason(10000); !strings.Contains(reason, "from peak unavailable") {
				t.Fatalf("unverified peak allowed new risk: %q", reason)
			}
			status := at.RiskStatus()
			if !status.AccountDrawdownUnknown || !status.AccountBreakerActive {
				t.Fatalf("unknown peak not shown: %+v", status)
			}
			ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 10000}, CandidateCoins: []kernel.CandidateCoin{{Symbol: "YUSDT"}}}
			got := at.applyHardRiskGates([]kernel.Decision{{Symbol: "YUSDT", Action: "open_long"}, {Symbol: "YUSDT", Action: "reduce_long"}}, ctx)
			if len(got) != 1 || got[0].Action != "reduce_long" {
				t.Fatalf("store failure changed reductions: %+v", got)
			}
		})
	}
}

func TestLLMAccountPeakRetryRetainsObservation(t *testing.T) {
	at := accountDrawdownTestTrader(t, "peak")
	at.store = profitLockTestStore(t)
	// The LLM peak has its own namespace alongside the pre-existing grid peak.
	if _, err := at.store.RiskState().UpdatePeak(at.dailyBaselineKey(), 10000); err != nil {
		t.Fatal(err)
	}
	review28QueryFailure(t, at.store, "risk_baselines")
	if reason := at.accountOpenHaltReason(15000); !strings.Contains(reason, "from peak unavailable") {
		t.Fatalf("store failure allowed new risk: %q", reason)
	}
	if err := at.store.GormDB().Callback().Query().Remove("review28:risk_baselines"); err != nil {
		t.Fatal(err)
	}
	if reason := at.accountOpenHaltReason(13000); !strings.Contains(reason, "from peak 15000.00") {
		t.Fatalf("retry lost the observed high-water mark: %q", reason)
	}
	for _, tc := range []struct {
		key  string
		peak float64
	}{
		{at.dailyBaselineKey(), 10000},
		{"llm_peak:" + at.dailyBaselineKey(), 15000},
	} {
		peak, found, err := at.store.RiskState().DayBaseline(tc.key, "__peak__")
		if err != nil || !found || peak != tc.peak {
			t.Fatalf("durable peak %s = %v, %v, %v, want %v", tc.key, peak, found, err, tc.peak)
		}
	}
}
