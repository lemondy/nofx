package trader

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"nofx/kernel"
	"nofx/store"
)

// review 2026-10-07 B2-D/B2-E: static exchange snapshots avoid network and mock state races.
type batch2RiskExchange struct{ Trader }

func (*batch2RiskExchange) GetBalance() (map[string]interface{}, error) {
	return map[string]interface{}{"totalEquity": 100.0}, nil
}
func (*batch2RiskExchange) GetPositions() ([]map[string]interface{}, error) { return nil, nil }

func batch2RiskTrader(t *testing.T) *AutoTrader {
	t.Helper()
	at := riskTestTrader(store.RiskControlConfig{DailyMaxLossPct: 5, EarlyCloseMinHours: -1})
	at.id, at.userID, at.exchangeID = "b2", "b2", t.Name()
	at.trader = &batch2RiskExchange{}
	t.Cleanup(func() {
		unregisterAccountTrader(at)
		dailyBaselineRegMu.Lock()
		delete(dailyBaselineReg, at.dailyBaselineKey())
		dailyBaselineRegMu.Unlock()
	})
	return at
}

// review 2026-10-07 B2-D: failed durable reads cannot publish 100 over the saved 120.
func TestB2DailyBaselineFailureBlocksOpensAndRecovers(t *testing.T) {
	at := batch2RiskTrader(t)
	st, err := store.New(filepath.Join(t.TempDir(), "risk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	at.store = st
	today := time.Now().UTC().Format("2006-01-02")
	if _, err := st.RiskState().AnchorDayBaseline(at.dailyBaselineKey(), today, 120); err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	if err := st.GormDB().Callback().Query().Before("gorm:query").Register("b2_fail_baseline", func(tx *gorm.DB) {
		if fail.Load() && tx.Statement.Table == "risk_baselines" {
			tx.AddError(errors.New("injected baseline read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	at.anchorDailyBaseline(100)
	dailyBaselineRegMu.Lock()
	_, published := dailyBaselineReg[at.dailyBaselineKey()]
	dailyBaselineRegMu.Unlock()
	if published || at.dayStartEquity != 0 || at.dayStartDay != "" {
		t.Fatalf("failed durable read published a baseline: shared=%t local=%s/%.2f", published, at.dayStartDay, at.dayStartEquity)
	}
	rc := at.config.StrategyConfig.RiskControl
	if reason := at.dailyLossHaltBlocks(rc, 100); !strings.Contains(reason, "baseline unknown") {
		t.Fatalf("unknown baseline allowed opens: %q", reason)
	}
	decisions := []kernel.Decision{
		{Action: "open_long", Symbol: "XUSDT", Price: 100, StopLoss: 95, TakeProfit: 110},
		{Action: "open_long_limit", Symbol: "XUSDT", Price: 99, StopLoss: 95, TakeProfit: 110},
		{Action: "open_short", Symbol: "XUSDT", Price: 100, StopLoss: 105, TakeProfit: 90},
		{Action: "open_short_limit", Symbol: "XUSDT", Price: 101, StopLoss: 105, TakeProfit: 90},
		{Action: "close_long", Symbol: "XUSDT"},
		{Action: "close_short", Symbol: "XUSDT"},
	}
	got := at.applyHardRiskGates(decisions, &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 100}})
	if len(got) != 2 || got[0].Action != "close_long" || got[1].Action != "close_short" {
		t.Fatalf("unknown baseline must block only opens: %+v", got)
	}
	fail.Store(false)
	at.anchorDailyBaseline(100)
	if at.dayStartEquity != 120 || at.dayStartDay != today {
		t.Fatalf("recovered baseline=%s/%.2f, want %s/120", at.dayStartDay, at.dayStartEquity, today)
	}
	if reason := at.dailyLossHaltBlocks(rc, 100); reason == "" || strings.Contains(reason, "unknown") {
		t.Fatalf("recovered 16.67%% loss did not halt: %q", reason)
	}
	peer := &AutoTrader{exchangeID: at.exchangeID, store: st}
	if baseline, ok := peer.inheritDailyBaseline(today); !ok || baseline != 120 {
		t.Fatalf("peer inherited baseline=%.2f ok=%t", baseline, ok)
	}
	if status := at.RiskStatus(); status.DayStartEquity != 120 || !status.DailyHalted {
		t.Fatalf("recovered status=%+v", status)
	}
}

// review 2026-10-07 B2-E: HTTP readers share the writers' account lock for both fields.
func TestB2RiskStatusConcurrentExecutionSnapshots(t *testing.T) {
	at := batch2RiskTrader(t)
	pe := &pendingEntry{Symbol: "XUSDT", Side: "long", Quantity: 1, Price: 100, PlacedAt: time.Now()}
	at.setPendingEntry(pe)
	mu := at.executionMutex()
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			mu.Lock()
			at.anchorDailyBaseline(120)
			pe.ExecutedQty = float64(i%11) / 10
			mu.Unlock()
		}
	}()
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 500; i++ {
				status := at.RiskStatus()
				if status.DayStartEquity != 0 && status.DayStartEquity != 120 {
					t.Errorf("inconsistent day anchor: %.2f", status.DayStartEquity)
				}
				if len(status.Pending) != 1 || status.Pending[0].FilledQty < 0 || status.Pending[0].FilledQty > 1 {
					t.Errorf("inconsistent pending snapshot: %+v", status.Pending)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}
