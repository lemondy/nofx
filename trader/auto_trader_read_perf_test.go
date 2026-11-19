package trader

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/market/usstock"
	"nofx/store"
)

type slowReadTrader struct {
	Trader
	delay time.Duration
}

func (s *slowReadTrader) GetBalance() (map[string]interface{}, error) {
	time.Sleep(s.delay)
	return map[string]interface{}{
		"totalWalletBalance":    1000.0,
		"availableBalance":      900.0,
		"totalUnrealizedProfit": 25.0,
		"totalEquity":           1025.0,
	}, nil
}

func (s *slowReadTrader) GetPositions() ([]map[string]interface{}, error) {
	time.Sleep(s.delay)
	return []map[string]interface{}{
		{
			"symbol":           "BTCUSDT",
			"side":             "long",
			"positionAmt":      0.01,
			"entryPrice":       50000.0,
			"markPrice":        52500.0,
			"unRealizedProfit": 25.0,
			"leverage":         10.0,
			"liquidationPrice": 40000.0,
		},
	}, nil
}

func TestGetAccountInfoAndRiskStatusFetchBalanceAndPositionsConcurrently(t *testing.T) {
	const delay = 60 * time.Millisecond
	at := &AutoTrader{
		id:             "t-par",
		name:           "par-trader",
		initialBalance: 1000,
		trader:         &slowReadTrader{delay: delay},
		config:         AutoTraderConfig{ID: "t-par"},
	}

	start := time.Now()
	info, err := at.GetAccountInfo()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GetAccountInfo failed: %v", err)
	}
	if info["total_equity"].(float64) != 1025.0 {
		t.Fatalf("total_equity = %v, want 1025", info["total_equity"])
	}
	if elapsed >= 2*delay-10*time.Millisecond {
		t.Fatalf("GetAccountInfo took %s (looks serial; delay=%s)", elapsed, delay)
	}

	start = time.Now()
	rs := at.RiskStatus()
	elapsed = time.Since(start)
	if rs.EquityUnknown || rs.PositionsUnknown || rs.Equity != 1025.0 || len(rs.Positions) != 1 {
		t.Fatalf("RiskStatus unexpected: %+v", rs)
	}
	if elapsed >= 2*delay-10*time.Millisecond {
		t.Fatalf("RiskStatus took %s (looks serial; delay=%s)", elapsed, delay)
	}
}

func TestFactorQuantUSAccountForUIConcurrentQuotesAndCoalescing(t *testing.T) {
	st, err := store.New(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	now := time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC)
	at := &AutoTrader{
		id:             "fq-us-ui",
		name:           "fq-us-ui",
		exchange:       "paper",
		initialBalance: 10000,
		store:          st,
		fqNow:          func() time.Time { return now },
		config: AutoTraderConfig{
			ID: "fq-us-ui",
			StrategyConfig: &store.StrategyConfig{
				StrategyType: store.StrategyTypeFactorQuant,
				FactorQuantConfig: &store.FactorQuantConfig{
					Market: "us",
				},
			},
		},
	}

	if _, err := st.StockPaper().EnsureAccount(at.id, 10000); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	const numSyms = 6
	for i := 0; i < numSyms; i++ {
		sym := fmt.Sprintf("SYM%dBUSDT", i)
		if _, _, err := st.StockPaper().OpenOrAddWithFee(at.id, sym, 1, 100, 0, 0, 0, now); err != nil {
			t.Fatalf("OpenOrAddWithFee %s: %v", sym, err)
		}
	}

	origQuote := stockGetQuote
	defer func() { stockGetQuote = origQuote }()
	var quoteCalls int32
	const quoteDelay = 50 * time.Millisecond
	stockGetQuote = func(_ context.Context, symbol string) (*usstock.Quote, error) {
		atomic.AddInt32(&quoteCalls, 1)
		time.Sleep(quoteDelay)
		return &usstock.Quote{Symbol: symbol, BStockPrice: 110}, nil
	}

	// Simulate /account + /positions + /risk-status arriving concurrently.
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 3)
	wg.Add(3)
	go func() { defer wg.Done(); _, errs[0] = at.GetAccountInfo() }()
	go func() { defer wg.Done(); _, errs[1] = at.GetPositions() }()
	go func() {
		defer wg.Done()
		rs := at.RiskStatus()
		if rs.Equity <= 0 {
			errs[2] = fmt.Errorf("expected positive equity, got %v", rs.Equity)
		}
	}()
	wg.Wait()
	elapsed := time.Since(start)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("caller %d failed: %v", i, e)
		}
	}
	// All 3 UI endpoints must coalesce into 1 Account() call = 6 quotes total,
	// and all 6 quotes must run concurrently (~1x quoteDelay, well under 6x).
	if got := atomic.LoadInt32(&quoteCalls); got != numSyms {
		t.Fatalf("quoteCalls = %d, want %d (coalesced across 3 UI readers)", got, numSyms)
	}
	if elapsed >= 3*quoteDelay {
		t.Fatalf("3 concurrent US factor_quant UI calls took %s (serial 6 quotes = %s)", elapsed, numSyms*quoteDelay)
	}

	// A fill via Place must immediately invalidate the UI cache so the next read refreshes.
	broker, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatalf("newFactorQuantBroker: %v", err)
	}
	res := broker.Place(fqOrder{Symbol: "SYM0BUSDT", Kind: fqKindOpen, Side: fqSideLong, Qty: 1, Price: 110})
	if !res.OK {
		t.Fatalf("Place failed: %+v", res)
	}
	if _, err := at.GetAccountInfo(); err != nil {
		t.Fatalf("GetAccountInfo after fill: %v", err)
	}
	if got := atomic.LoadInt32(&quoteCalls); got != numSyms*2 {
		t.Fatalf("quoteCalls after fill invalidation = %d, want %d", got, numSyms*2)
	}
}
