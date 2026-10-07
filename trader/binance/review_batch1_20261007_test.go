package binance

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"nofx/trader/types"
)

// review 2026-10-07 B1-3/B1-4: inject SDK responses without opening any sockets.
type batch1Transport func(*http.Request) (*http.Response, error)

func (f batch1Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func batch1Response(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body)), Request: r,
	}
}

func batch1FuturesTrader(transport batch1Transport) *FuturesTrader {
	client := futures.NewClient("batch1-key", "batch1-secret")
	client.HTTPClient = &http.Client{Transport: transport}
	return &FuturesTrader{client: client, cacheDuration: time.Hour}
}

const batch1PositionsJSON = `[{"symbol":"XUSDT","positionAmt":"-1","entryPrice":"100","markPrice":"95","unRealizedProfit":"5","leverage":"2","liquidationPrice":"140","positionSide":"SHORT"}]`
const batch1BalanceJSON = `{"totalWalletBalance":"100","availableBalance":"80","totalUnrealizedProfit":"5"}`

// review 2026-10-07 B1-3: a rejected leverage change must abort both entry directions.
func TestB1LimitOrderRejectsLeverageFailure(t *testing.T) {
	for _, side := range []string{"BUY", "SELL"} {
		t.Run(side, func(t *testing.T) {
			leverageCalls, orderCalls := 0, 0
			tr := batch1FuturesTrader(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/fapi/v1/exchangeInfo":
					return batch1Response(r, http.StatusOK, `{"symbols":[{"symbol":"XUSDT","filters":[{"filterType":"LOT_SIZE","stepSize":"0.1","minQty":"0.1","maxQty":"100"},{"filterType":"PRICE_FILTER","tickSize":"0.01"},{"filterType":"MIN_NOTIONAL","notional":"10"}]}]}`), nil
				case "/fapi/v2/positionRisk":
					return batch1Response(r, http.StatusOK, "[]"), nil
				case "/fapi/v1/leverage":
					leverageCalls++
					return batch1Response(r, http.StatusBadRequest, `{"code":-2019,"msg":"Margin is insufficient."}`), nil
				case "/fapi/v1/order":
					orderCalls++
					return batch1Response(r, http.StatusOK, `{"orderId":123,"symbol":"XUSDT","status":"NEW"}`), nil
				default:
					return nil, fmt.Errorf("unexpected endpoint: %s", r.URL.Path)
				}
			})
			result, err := tr.PlaceLimitOrder(&types.LimitOrderRequest{
				Symbol: "XUSDT", Side: side, Quantity: 1, Price: 100, Leverage: 3,
			})
			if err == nil || !strings.Contains(err.Error(), "failed to set leverage") || result != nil {
				t.Fatalf("leverage failure not returned: result=%v err=%v", result, err)
			}
			if leverageCalls != 1 || orderCalls != 0 {
				t.Fatalf("leverage calls=%d order submissions=%d", leverageCalls, orderCalls)
			}
		})
	}
}

func batch1ReadCache(t *testing.T, tr *FuturesTrader, balance bool) {
	t.Helper()
	if balance {
		snapshot, err := tr.GetBalance()
		if err != nil || snapshot == nil || snapshot["totalEquity"] != 105.0 {
			t.Errorf("balance snapshot=%v err=%v", snapshot, err)
		}
	} else {
		snapshot, err := tr.GetPositions()
		if err != nil || len(snapshot) != 1 || snapshot[0]["positionAmt"] != -1.0 {
			t.Errorf("position snapshot=%v err=%v", snapshot, err)
		}
	}
}

func batch1CacheTrader() *FuturesTrader {
	return batch1FuturesTrader(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/fapi/v2/positionRisk":
			return batch1Response(r, http.StatusOK, batch1PositionsJSON), nil
		case "/fapi/v2/account":
			return batch1Response(r, http.StatusOK, batch1BalanceJSON), nil
		default:
			return nil, fmt.Errorf("unexpected endpoint: %s", r.URL.Path)
		}
	})
}

// review 2026-10-07 B1-4: callers retain valid snapshots after cache invalidation.
func TestB1AccountCacheSnapshotsSurviveInvalidation(t *testing.T) {
	tr := batch1CacheTrader()
	positions, err := tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	balance, err := tr.GetBalance()
	if err != nil {
		t.Fatal(err)
	}
	positions, err = tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	balance, err = tr.GetBalance()
	if err != nil {
		t.Fatal(err)
	}
	tr.InvalidateAccountCache()
	if len(positions) != 1 || positions[0]["positionAmt"] != -1.0 || balance["totalEquity"] != 105.0 {
		t.Fatalf("returned snapshots lost after invalidation: positions=%v balance=%v", positions, balance)
	}
}

// review 2026-10-07 B1-4: exercise both cache hits and misses against concurrent invalidation.
func TestB1AccountCacheConcurrentInvalidation(t *testing.T) {
	tr := batch1CacheTrader()
	batch1ReadCache(t, tr, false)
	batch1ReadCache(t, tr, true)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		wg.Add(1)
		go func(balance bool) {
			defer wg.Done()
			<-start
			for i := 0; i < 128; i++ {
				batch1ReadCache(t, tr, balance)
			}
		}(reader%2 == 0)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 256; i++ {
			tr.InvalidateAccountCache()
		}
	}()
	close(start)
	wg.Wait()
}

// review 2026-10-07 B1-4: invalidation must clear an in-flight fetch's writeback.
func TestB1CacheMissInvalidationOrdering(t *testing.T) {
	for _, balance := range []bool{false, true} {
		t.Run(fmt.Sprintf("balance_%t", balance), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int32
			tr := batch1FuturesTrader(func(r *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					close(started)
					<-release
				}
				if balance {
					return batch1Response(r, http.StatusOK, batch1BalanceJSON), nil
				}
				return batch1Response(r, http.StatusOK, batch1PositionsJSON), nil
			})
			fetched := make(chan struct{})
			go func() {
				defer close(fetched)
				batch1ReadCache(t, tr, balance)
			}()
			<-started
			invalidating, invalidated := make(chan struct{}), make(chan struct{})
			go func() {
				close(invalidating)
				tr.InvalidateAccountCache()
				close(invalidated)
			}()
			<-invalidating
			select {
			case <-invalidated:
				t.Error("invalidation bypassed the in-flight cache publication")
			case <-time.After(20 * time.Millisecond):
			}
			unblock()
			<-fetched
			<-invalidated
			tr.positionsCacheMutex.RLock()
			positionsCleared := tr.cachedPositions == nil
			tr.positionsCacheMutex.RUnlock()
			tr.balanceCacheMutex.RLock()
			balanceCleared := tr.cachedBalance == nil
			tr.balanceCacheMutex.RUnlock()
			if !positionsCleared || !balanceCleared {
				t.Fatal("in-flight request repopulated the invalidated cache")
			}
			batch1ReadCache(t, tr, balance)
			if calls.Load() != 2 {
				t.Fatalf("next read reused an invalidated snapshot: fetches=%d", calls.Load())
			}
		})
	}
}

// review 2026-10-07 B1-4: queued misses recheck the snapshot published by the first reader.
func TestB1ConcurrentCacheMissesReuseSnapshot(t *testing.T) {
	for _, balance := range []bool{false, true} {
		t.Run(fmt.Sprintf("balance_%t", balance), func(t *testing.T) {
			var calls atomic.Int32
			tr := batch1FuturesTrader(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if balance {
					return batch1Response(r, http.StatusOK, batch1BalanceJSON), nil
				}
				return batch1Response(r, http.StatusOK, batch1PositionsJSON), nil
			})
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					batch1ReadCache(t, tr, balance)
				}()
			}
			close(start)
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("concurrent misses fetched %d times instead of reusing the snapshot", calls.Load())
			}
		})
	}
}
