package binance

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/store"
)

// review 2026-10-07 B2-A/B2-B/B2-C: SDK transport fixtures never open sockets.
func batch2SyncStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func batch2SyncTrade(id int, symbol, side, positionSide string, qty, price, pnl, fee float64, ms int64) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "orderId": id + 10000, "symbol": symbol, "side": side, "positionSide": positionSide,
		"qty": fmt.Sprint(qty), "price": fmt.Sprint(price), "realizedPnl": fmt.Sprint(pnl),
		"commission": fmt.Sprint(fee), "commissionAsset": "USDT", "time": ms,
	}
}

func batch2SyncResponse(r *http.Request, value interface{}) (*http.Response, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return batch1Response(r, http.StatusOK, string(body)), nil
}

func batch2SyncTransport(trades []map[string]interface{}) batch1Transport {
	return func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/fapi/v1/income":
			start, _ := strconv.ParseInt(r.URL.Query().Get("startTime"), 10, 64)
			incomes := []map[string]interface{}{}
			for _, trade := range trades {
				if trade["time"].(int64) >= start {
					incomes = append(incomes, map[string]interface{}{"symbol": trade["symbol"], "income": "1", "time": trade["time"]})
				}
			}
			return batch2SyncResponse(r, incomes)
		case "/fapi/v2/positionRisk", "/fapi/v1/algoOpenOrders":
			return batch1Response(r, http.StatusOK, "[]"), nil
		case "/fapi/v1/userTrades":
			start, _ := strconv.ParseInt(r.URL.Query().Get("startTime"), 10, 64)
			rows := []map[string]interface{}{}
			for _, trade := range trades {
				// Intentionally replay existing IDs on fromId requests to test the DB receipt guard.
				if trade["symbol"] == r.URL.Query().Get("symbol") && trade["time"].(int64) >= start {
					rows = append(rows, trade)
				}
			}
			return batch2SyncResponse(r, rows)
		default:
			return nil, fmt.Errorf("unexpected sync endpoint: %s", r.URL.Path)
		}
	}
}

func batch2Position(t *testing.T, st *store.Store, symbol, side string) store.TraderPosition {
	t.Helper()
	var row store.TraderPosition
	if err := st.GormDB().Where("symbol = ? AND side = ?", symbol, side).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

// review 2026-10-07 B2-A: background and reconcile callers using separate clients serialize.
func TestB2ConcurrentAccountSyncAppliesTradesOnce(t *testing.T) {
	st := batch2SyncStore(t)
	ms := time.Now().Add(-time.Minute).UnixMilli()
	trades := []map[string]interface{}{
		batch2SyncTrade(1, "XUSDT", "BUY", "LONG", 3, 100, 0, 0.3, ms),
		batch2SyncTrade(2, "XUSDT", "SELL", "LONG", 1, 110, 10, 0.1, ms+1),
		batch2SyncTrade(3, "XUSDT", "SELL", "SHORT", 2, 100, 0, 0.2, ms+2),
		batch2SyncTrade(4, "XUSDT", "SELL", "LONG", 2, 105, 10, 0.2, ms+3),
		batch2SyncTrade(5, "XUSDT", "BUY", "SHORT", 1, 90, 10, 0.1, ms+4),
	}
	base := batch2SyncTransport(trades)
	started, release := make(chan struct{}), make(chan struct{})
	var incomeCalls atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	transport := func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/fapi/v1/income" && r.URL.Query().Get("incomeType") == "COMMISSION" {
			if incomeCalls.Add(1) == 1 {
				close(started)
				<-release
			}
		}
		return base(r)
	}
	first, second := batch1FuturesTrader(transport), batch1FuturesTrader(transport)
	results := make(chan error, 2)
	go func() { results <- first.SyncOrdersFromBinance("b2", t.Name(), "binance", st) }()
	<-started
	go func() { results <- second.SyncOrdersFromBinance("b2", t.Name(), "binance", st) }()
	time.Sleep(30 * time.Millisecond)
	if incomeCalls.Load() != 1 {
		t.Error("second client entered sync while the first still owned the account")
	}
	unblock()
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	long, short := batch2Position(t, st, "XUSDT", "LONG"), batch2Position(t, st, "XUSDT", "SHORT")
	if long.Status != "CLOSED" || long.Quantity != 3 || long.RealizedPnL != 20 || math.Abs(long.Fee-0.6) > 1e-9 {
		t.Fatalf("long applied more than once: %+v", long)
	}
	if short.Status != "OPEN" || short.Quantity != 1 || short.RealizedPnL != 10 || math.Abs(short.Fee-0.3) > 1e-9 {
		t.Fatalf("short applied more than once: %+v", short)
	}
	var fills int64
	if err := st.GormDB().Model(&store.TraderFill{}).Count(&fills).Error; err != nil || fills != 5 {
		t.Fatalf("fills=%d err=%v", fills, err)
	}
}

// review 2026-10-07 B2-B: a failed position write rolls back its fill, holds
// the rest of THAT symbol and the cursor, but never blocks other symbols
// (one poison trade used to abort the whole sync, freezing all bookkeeping).
func TestB2SyncRetriesFailedPositionWrite(t *testing.T) {
	st := batch2SyncStore(t)
	ms := time.Now().Add(-time.Minute).UnixMilli()
	trades := []map[string]interface{}{
		batch2SyncTrade(1, "XUSDT", "BUY", "LONG", 1, 100, 0, 0.1, ms),
		batch2SyncTrade(2, "XUSDT", "BUY", "LONG", 1, 110, 0, 0.1, ms+1),
		batch2SyncTrade(3, "XUSDT", "BUY", "LONG", 1, 120, 0, 0.1, ms+2),
		batch2SyncTrade(4, "YUSDT", "BUY", "LONG", 1, 50, 0, 0.1, ms+3),
	}
	tr := batch1FuturesTrader(batch2SyncTransport(trades))
	if err := st.GormDB().Exec("CREATE TRIGGER b2_fail BEFORE UPDATE ON trader_positions BEGIN SELECT RAISE(ABORT, 'injected position failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	// The trigger blocks UPDATEs (XUSDT's 2nd fill); YUSDT's open is an INSERT.
	if err := tr.SyncOrdersFromBinance("b2", t.Name(), "binance", st); err != nil {
		t.Fatalf("a per-trade accounting failure must not abort the sync: %v", err)
	}
	for _, model := range []interface{}{&store.TraderOrder{}, &store.TraderFill{}} {
		var count int64
		// X#1 and Y#4 committed; X#2 rolled back; X#3 held behind it.
		if err := st.GormDB().Model(model).Count(&count).Error; err != nil || count != 2 {
			t.Fatalf("receipts after partial failure: count=%d err=%v (want 2)", count, err)
		}
	}
	if y := batch2Position(t, st, "YUSDT", "LONG"); y.Quantity != 1 {
		t.Fatalf("other symbol must still book despite XUSDT failure: %+v", y)
	}
	binanceSyncStateMutex.RLock()
	_, advanced := binanceSyncState[t.Name()]
	binanceSyncStateMutex.RUnlock()
	if advanced {
		t.Fatal("failed sync advanced the memory cursor")
	}
	if err := st.GormDB().Exec("DROP TRIGGER b2_fail").Error; err != nil {
		t.Fatal(err)
	}
	if err := tr.SyncOrdersFromBinance("b2", t.Name(), "binance", st); err != nil {
		t.Fatal(err)
	}
	row := batch2Position(t, st, "XUSDT", "LONG")
	if row.Quantity != 3 || row.EntryPrice != 110 || math.Abs(row.Fee-0.3) > 1e-9 {
		t.Fatalf("retry accounting wrong: %+v", row)
	}
	if y := batch2Position(t, st, "YUSDT", "LONG"); y.Quantity != 1 {
		t.Fatalf("retry must not re-apply the already-booked YUSDT fill: %+v", y)
	}
}

// review 2026-10-07 B2-C: a flat second symbol closing in the saved fill's second is discovered.
func TestB2RestartOverlapFindsSameSecondOtherSymbol(t *testing.T) {
	st := batch2SyncStore(t)
	ms := time.Now().Add(-time.Minute).Truncate(time.Second).UnixMilli()
	order := &store.TraderOrder{TraderID: "b2", ExchangeID: t.Name(), ExchangeType: "binance", ExchangeOrderID: "100", Symbol: "XUSDT", Side: "BUY", PositionSide: "LONG", Type: "MARKET", OrderAction: "open_long", Quantity: 1, Price: 100}
	fill := &store.TraderFill{TraderID: "b2", ExchangeID: t.Name(), ExchangeType: "binance", ExchangeOrderID: "10100", ExchangeTradeID: "100", Symbol: "XUSDT", Side: "BUY", Quantity: 1, Price: 100, Commission: 0.1, CommissionAsset: "USDT", CreatedAt: ms + 100}
	if applied, err := st.Order().ApplyTrade(order, fill); err != nil || !applied {
		t.Fatalf("seed failed: applied=%t err=%v", applied, err)
	}
	y := &store.TraderPosition{TraderID: "b2", ExchangeID: t.Name(), Symbol: "YUSDT", Side: "LONG", Quantity: 1, EntryQuantity: 1, EntryPrice: 100, Status: "CLOSED", ExitTime: ms, CloseReason: "netting_reconcile"}
	if err := st.GormDB().Create(y).Error; err != nil {
		t.Fatal(err)
	}
	trades := []map[string]interface{}{
		batch2SyncTrade(100, "XUSDT", "BUY", "LONG", 1, 100, 0, 0.1, ms+100),
		batch2SyncTrade(200, "YUSDT", "SELL", "LONG", 1, 107, 7, 0.1, ms+300),
	}
	tr := batch1FuturesTrader(batch2SyncTransport(trades))
	for i := 0; i < 2; i++ {
		binanceSyncStateMutex.Lock()
		delete(binanceSyncState, t.Name())
		binanceSyncStateMutex.Unlock()
		if err := tr.SyncOrdersFromBinance("b2", t.Name(), "binance", st); err != nil {
			t.Fatal(err)
		}
	}
	xrow, yrow := batch2Position(t, st, "XUSDT", "LONG"), batch2Position(t, st, "YUSDT", "LONG")
	if xrow.Quantity != 1 || xrow.Fee != 0.1 || yrow.RealizedPnL != 7 || yrow.Fee != 0.1 {
		t.Fatalf("restart missed or repeated a trade: X=%+v Y=%+v", xrow, yrow)
	}
}

// review 2026-10-07 B2-C: DB OPEN rows discover closes absent from income and live positions.
func TestB2SyncDiscoversDatabaseOpenSymbols(t *testing.T) {
	st := batch2SyncStore(t)
	row := &store.TraderPosition{TraderID: "b2", ExchangeID: t.Name(), Symbol: "YUSDT", Side: "LONG", Quantity: 1, EntryQuantity: 1, EntryPrice: 100, Status: "OPEN"}
	if err := st.GormDB().Create(row).Error; err != nil {
		t.Fatal(err)
	}
	base := batch2SyncTransport([]map[string]interface{}{batch2SyncTrade(200, "YUSDT", "SELL", "LONG", 1, 107, 7, 0.1, time.Now().UnixMilli())})
	tr := batch1FuturesTrader(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/fapi/v1/income" {
			return batch1Response(r, http.StatusOK, "[]"), nil
		}
		return base(r)
	})
	if err := tr.SyncOrdersFromBinance("b2", t.Name(), "binance", st); err != nil {
		t.Fatal(err)
	}
	got := batch2Position(t, st, "YUSDT", "LONG")
	if got.Status != "CLOSED" || got.RealizedPnL != 7 || got.Fee != 0.1 {
		t.Fatalf("DB open symbol not synced: %+v", got)
	}
}
