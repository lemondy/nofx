package trader

import (
	"errors"
	"fmt"
	"nofx/store"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func lifecycleFixture(t *testing.T) *AutoTrader {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Trader().Create(&store.Trader{ID: "t", UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	return &AutoTrader{id: "t", userID: "u", store: st, config: AutoTraderConfig{ScanInterval: time.Minute, StrategyConfig: &store.StrategyConfig{StrategyType: "grid_trading", GridConfig: &store.GridStrategyConfig{Symbol: "BTCUSDT", GridCount: 1}}}}
}

func TestConcurrentRunReservationAndEarlyFailureCleanup(t *testing.T) {
	at := lifecycleFixture(t)
	var wg sync.WaitGroup
	var reserved atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := at.beginRun()
			if err == nil {
				reserved.Add(1)
			} else if !errors.Is(err, ErrAlreadyRunning) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if reserved.Load() != 1 {
		t.Fatalf("reserved %d loops", reserved.Load())
	}
	if err := at.run(); err == nil {
		t.Fatal("unsafe grid should fail")
	}
	at.Stop()
	row, err := at.store.Trader().GetForUser("u", "t")
	if err != nil || row.IsRunning || at.GetStatus()["is_running"] != false {
		t.Fatalf("stale state %+v %v", row, err)
	}
	if err := at.beginRun(); err != nil {
		t.Fatal("cannot restart", err)
	}
	_ = at.run()
	at.Stop()
}

type blockedGridTrader struct {
	Trader
	entered, release chan struct{}
}

func (t *blockedGridTrader) GetMarketPrice(string) (float64, error) {
	close(t.entered)
	<-t.release
	return 0, fmt.Errorf("fake upstream failure")
}

func TestStartNonBlockingStopJoinsAndDuplicateRejected(t *testing.T) {
	at := lifecycleFixture(t)
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "BTCUSDT", GridCount: 10, TotalInvestment: 100, Leverage: 2, LowerPrice: 100, UpperPrice: 200}
	fake := &blockedGridTrader{entered: make(chan struct{}), release: make(chan struct{})}
	at.trader = fake
	if err := at.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.entered:
	case <-time.After(time.Second):
		t.Fatal("loop did not start")
	}
	if err := at.Start(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("duplicate start", err)
	}
	stopped := make(chan struct{})
	go func() { at.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop did not join in-flight operation")
	case <-time.After(10 * time.Millisecond):
	}
	close(fake.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop hung")
	}
	if at.GetStatus()["is_running"] != false {
		t.Fatal("state not stopped")
	}
}

func TestUninitializedGridRiskSafe(t *testing.T) {
	at := &AutoTrader{}
	if at.GetGridRiskInfo() == nil {
		t.Fatal("nil risk")
	}
	at.config.StrategyConfig = &store.StrategyConfig{GridConfig: &store.GridStrategyConfig{Symbol: "BTCUSDT"}}
	if at.GetGridRiskInfo() == nil {
		t.Fatal("nil uninitialized risk")
	}
}
