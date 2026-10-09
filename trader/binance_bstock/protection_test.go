package binance_bstock

import (
	"errors"
	"math"
	"testing"

	binance "github.com/adshao/go-binance/v2"
)

func stopOrder(list int64) *binance.Order {
	return &binance.Order{Symbol: "AAPLBUSDT", OrderID: 10, OrderListId: list, ClientOrderID: "nxbs_sl_old", Side: binance.SideTypeSell, Type: binance.OrderTypeStopLossLimit, Price: "89.5", StopPrice: "90", OrigQuantity: "2", ExecutedQuantity: "0", Status: binance.OrderStatusTypeNew}
}
func tpOrder(list int64) *binance.Order {
	return &binance.Order{Symbol: "AAPLBUSDT", OrderID: 11, OrderListId: list, ClientOrderID: "nxbs_tp_old", Side: binance.SideTypeSell, Type: binance.OrderTypeLimitMaker, Price: "120", OrigQuantity: "2", ExecutedQuantity: "0", Status: binance.OrderStatusTypeNew}
}

func TestSetProtectionOCOAndStopOnly(t *testing.T) {
	tr, f, _ := fixture(t)
	tr.client.TimeOffset = 250
	p, err := tr.SetProtection("AAPLBUSDT", 2.0009, 90.019, 89.509, 120.019)
	if err != nil {
		t.Fatal(err)
	}
	if p.OrderListID != "9" || p.StopOrderID != "102" || p.TakeProfitID != "101" {
		t.Fatal(p)
	}
	near(t, p.Quantity, 2)
	near(t, p.StopPrice, 90.01)
	near(t, p.StopLimitPrice, 89.50)
	near(t, p.TakeProfit, 120.01)
	requests := requestsTo(f, "/api/v3/orderList/oco", "POST")
	if len(requests) != 1 {
		t.Fatal(requests)
	}
	r := requests[0]
	for key, want := range map[string]string{"symbol": "AAPLBUSDT", "side": "SELL", "quantity": "2", "aboveType": "LIMIT_MAKER", "abovePrice": "120.01", "belowType": "STOP_LOSS_LIMIT", "belowStopPrice": "90.01", "belowPrice": "89.5", "belowTimeInForce": "GTC", "timestamp": "1699999999750", "recvWindow": "5000"} {
		if r.params.Get(key) != want {
			t.Fatalf("%s=%q want %q", key, r.params.Get(key), want)
		}
	}
	for _, key := range []string{"listClientOrderId", "aboveClientOrderId", "belowClientOrderId"} {
		tagged(t, r.params.Get(key))
	}
	if r.params.Get("aboveClientOrderId") == r.params.Get("belowClientOrderId") {
		t.Fatal("duplicate leg IDs")
	}
	if len(requestsTo(f, "/api/v3/order/oco", "")) != 0 {
		t.Fatal("deprecated OCO endpoint used")
	}
	p, err = tr.SetProtection("AAPLBUSDT", 1, 90, 89.5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.OrderListID != "" || p.TakeProfitID != "" || p.StopOrderID != "100" {
		t.Fatal(p)
	}
	r = requestsTo(f, "/api/v3/order", "POST")[0]
	for key, want := range map[string]string{"type": "STOP_LOSS_LIMIT", "side": "SELL", "timeInForce": "GTC", "stopPrice": "90", "price": "89.5", "quantity": "1"} {
		if r.params.Get(key) != want {
			t.Fatal(r.params)
		}
	}
	tagged(t, r.params.Get("newClientOrderId"))
}

func TestProtectionValidationLeavesOldStopLive(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		qty, stop, limit, tp float64
		message              string
	}{
		{"stop at market", 1, 100, 99, 120, "invalid protection"},
		{"tp at market", 1, 90, 89, 100, "invalid protection"},
		{"stop limit too high", 1, 90, 91, 120, "invalid protection"},
		{"excess inventory", 11, 90, 89, 120, "exceeds held balance"},
		{"dust", 0.001, 90, 89, 120, "NOTIONAL"},
		{"negative tp", 1, 90, 89, -1, "nonnegative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, f, _ := fixture(t)
			f.orders = []*binance.Order{stopOrder(-1)}
			_, err := tr.SetProtection("AAPLBUSDT", tc.qty, tc.stop, tc.limit, tc.tp)
			requireError(t, err, tc.message)
			for _, r := range f.requests {
				if r.method == "DELETE" || r.method == "POST" {
					t.Fatalf("invalid protection mutated exchange: %+v", r)
				}
			}
		})
	}
}

func TestProtectionReplacementCancelsFirst(t *testing.T) {
	tr, f, _ := fixture(t)
	f.orders = []*binance.Order{stopOrder(9), tpOrder(9)}
	f.balances = []binance.Balance{{Asset: "AAPLB", Free: "0", Locked: "2"}}
	_, err := tr.SetProtection("AAPLBUSDT", 2, 91, 90.5, 121)
	if err != nil {
		t.Fatal(err)
	}
	deleteAt, postAt := -1, -1
	for i, r := range f.requests {
		if r.method == "DELETE" {
			deleteAt = i
		}
		if r.path == "/api/v3/orderList/oco" {
			postAt = i
		}
	}
	if deleteAt < 0 || postAt < deleteAt {
		t.Fatal(f.requests)
	}
	if len(requestsTo(f, "/api/v3/orderList", "DELETE")) != 1 {
		t.Fatal("OCO canceled more than once")
	}
}

func TestGetProtectionReconstructsTaggedOrders(t *testing.T) {
	tr, f, _ := fixture(t)
	stop, tp := stopOrder(9), tpOrder(9)
	stop.ExecutedQuantity = "0.5"
	manual := stopOrder(-1)
	manual.OrderID = 500
	manual.ClientOrderID = "manual_stop"
	f.orders = []*binance.Order{manual, tp, stop, &binance.Order{OrderID: 600, OrderListId: -1, ClientOrderID: "nxbs_entry", Side: binance.SideTypeBuy, Type: binance.OrderTypeLimit, OrigQuantity: "3"}}
	p, err := tr.GetProtection("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if p.OrderListID != "9" || p.StopOrderID != "10" || p.TakeProfitID != "11" {
		t.Fatal(p)
	}
	near(t, p.Quantity, 1.5)
	near(t, p.StopPrice, 90)
	near(t, p.StopLimitPrice, 89.5)
	near(t, p.TakeProfit, 120)
	open, err := tr.GetOpenOrders("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 4 || open[2].Type != "STOP_LOSS_LIMIT" || open[2].PositionSide != "LONG" || open[2].ClientID != "nxbs_sl_old" {
		t.Fatal(open)
	}
	f.orders = nil
	p, err = tr.GetProtection("AAPLBUSDT")
	if err != nil || p != nil {
		t.Fatalf("empty protection: %v %v", p, err)
	}
}

func TestSetStopLossPreservesTakeProfit(t *testing.T) {
	tr, f, _ := fixture(t)
	f.orders = []*binance.Order{tpOrder(-1)}
	if err := tr.SetStopLoss("AAPLBUSDT", "LONG", 2, 92); err != nil {
		t.Fatal(err)
	}
	r := requestsTo(f, "/api/v3/orderList/oco", "POST")[0]
	if r.params.Get("abovePrice") != "120" || r.params.Get("belowStopPrice") != "92" || r.params.Get("belowPrice") != "91.54" {
		t.Fatal(r.params)
	}
	if len(requestsTo(f, "/api/v3/order", "DELETE")) != 1 {
		t.Fatal("old TP was not canceled")
	}
}

func TestSetTakeProfitPreservesStop(t *testing.T) {
	tr, f, _ := fixture(t)
	f.orders = []*binance.Order{stopOrder(-1)}
	if err := tr.SetTakeProfit("AAPLBUSDT", "LONG", 2, 125); err != nil {
		t.Fatal(err)
	}
	r := requestsTo(f, "/api/v3/orderList/oco", "POST")[0]
	if r.params.Get("abovePrice") != "125" || r.params.Get("belowStopPrice") != "90" || r.params.Get("belowPrice") != "89.5" {
		t.Fatal(r.params)
	}
}

func TestCancelTakeProfitLeavesLoneStop(t *testing.T) {
	tr, f, _ := fixture(t)
	stop, tp := stopOrder(9), tpOrder(9)
	stop.ExecutedQuantity = "0.5"
	f.orders = []*binance.Order{stop, tp}
	if err := tr.CancelTakeProfitOrders("AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
	if len(requestsTo(f, "/api/v3/orderList", "DELETE")) != 1 {
		t.Fatal("list not canceled")
	}
	r := requestsTo(f, "/api/v3/order", "POST")[0]
	if r.params.Get("type") != "STOP_LOSS_LIMIT" || r.params.Get("stopPrice") != "90" || r.params.Get("price") != "89.5" || r.params.Get("quantity") != "1.5" {
		t.Fatal(r.params)
	}
	if len(requestsTo(f, "/api/v3/orderList/oco", "POST")) != 0 {
		t.Fatal("TP was re-created")
	}
}

func TestCancelStopLossLeavesLoneTakeProfit(t *testing.T) {
	tr, f, _ := fixture(t)
	f.orders = []*binance.Order{stopOrder(9), tpOrder(9)}
	if err := tr.CancelStopLossOrders("AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
	r := requestsTo(f, "/api/v3/order", "POST")[0]
	if r.params.Get("type") != "LIMIT_MAKER" || r.params.Get("price") != "120" || !stringsHasTP(r.params.Get("newClientOrderId")) {
		t.Fatal(r.params)
	}
}
func stringsHasTP(s string) bool {
	return len(s) > len("nxbs_tp_") && s[:len("nxbs_tp_")] == "nxbs_tp_"
}

func TestSellAllCancelsProtectionAndReloadsBalance(t *testing.T) {
	tr, f, _ := fixture(t)
	f.orders = []*binance.Order{stopOrder(9), tpOrder(9)}
	f.balances = []binance.Balance{{Asset: "AAPLB", Free: "0.25", Locked: "1.7509"}}
	o, err := tr.SellMarket("AAPLBUSDT", 0)
	if err != nil {
		t.Fatal(err)
	}
	if o.Side != "SELL" {
		t.Fatal(o)
	}
	r := requestsTo(f, "/api/v3/order", "POST")[0]
	if r.params.Get("quantity") != "2" || r.params.Get("type") != "MARKET" {
		t.Fatal(r.params)
	}
	deleteAt, accountAt, postAt := -1, -1, -1
	for i, r := range f.requests {
		if r.method == "DELETE" {
			deleteAt = i
		}
		if r.path == "/api/v3/account" {
			accountAt = i
		}
		if r.path == "/api/v3/order" && r.method == "POST" {
			postAt = i
		}
	}
	if deleteAt < 0 || accountAt < deleteAt || postAt < accountAt {
		t.Fatal(f.requests)
	}
}

func TestProtectionCancellationOwnershipAndFailure(t *testing.T) {
	tr, f, _ := fixture(t)
	manual := stopOrder(-1)
	manual.ClientOrderID = "manual"
	f.orders = []*binance.Order{manual}
	if err := tr.CancelStopOrders("AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
	if len(requestsTo(f, "/api/v3/order", "DELETE")) != 0 {
		t.Fatal("manual protection canceled")
	}
	// CancelAllOrders touches program (nxbs_) orders only: the manual order
	// survives and no symbol-wide cancel is sent.
	owned := stopOrder(-1)
	owned.OrderID = 777
	f.orders = []*binance.Order{manual, owned}
	if err := tr.CancelAllOrders("AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
	if len(requestsTo(f, "/api/v3/openOrders", "DELETE")) != 0 {
		t.Fatal("symbol-wide cancel would wipe manual orders")
	}
	if dels := requestsTo(f, "/api/v3/order", "DELETE"); len(dels) != 1 {
		t.Fatalf("expected exactly the one program order cancelled, got %d", len(dels))
	}
	f.orders = []*binance.Order{stopOrder(-1)}
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path == "/api/v3/order" && r.method == "DELETE" {
			return map[string]interface{}{"code": -2010, "msg": "cancel rejected"}, 400, true
		}
		return nil, 0, false
	}
	_, err := tr.SetProtection("AAPLBUSDT", 1, 90, 89, 120)
	requireError(t, err, "cancel protection")
	if len(requestsTo(f, "/api/v3/orderList/oco", "POST")) != 0 {
		t.Fatal("replacement sent after cancellation failure")
	}
}

func TestOCOResponseRequiresLegIDs(t *testing.T) {
	tr, f, _ := fixture(t)
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path == "/api/v3/orderList/oco" {
			return map[string]interface{}{"orderListId": 9}, 200, true
		}
		return nil, 0, false
	}
	_, err := tr.SetProtection("AAPLBUSDT", 1, 90, 89, 120)
	requireError(t, err, "reconcile")
}

func TestStopLossBufferUsesDecimalPrice(t *testing.T) {
	tr, f, _ := fixture(t)
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path == "/api/v3/ticker/price" {
			return map[string]interface{}{"symbol": "AAPLBUSDT", "price": "120"}, 200, true
		}
		return nil, 0, false
	}
	if err := tr.SetStopLoss("AAPLBUSDT", "LONG", 1, 114); err != nil {
		t.Fatal(err)
	}
	r := requestsTo(f, "/api/v3/order", "POST")[0]
	if r.params.Get("price") != "113.43" || r.params.Get("stopPrice") != "114" {
		t.Fatal(r.params)
	}
	for _, stop := range []float64{math.NaN(), math.Inf(1), 0, -1} {
		requireError(t, tr.SetStopLoss("AAPLBUSDT", "LONG", 1, stop), "invalid stop price")
	}
	if len(requestsTo(f, "/api/v3/order", "POST")) != 1 {
		t.Fatal("invalid stop created an order")
	}
}

func TestProtectionReplacementFailureExpiresCaches(t *testing.T) {
	for _, tp := range []float64{0, 120} {
		t.Run(value(tp), func(t *testing.T) {
			tr, f, _ := fixture(t)
			if _, err := tr.GetBalance(); err != nil {
				t.Fatal(err)
			}
			if _, err := tr.GetPositions(); err != nil {
				t.Fatal(err)
			}
			f.orders = []*binance.Order{stopOrder(-1)}
			f.handle = func(r requestRecord) (interface{}, int, bool) {
				if r.method == "POST" && (r.path == "/api/v3/order" || r.path == "/api/v3/orderList/oco") {
					return map[string]interface{}{"code": -2015, "msg": "Invalid API-key, IP, or permissions for action."}, 400, true
				}
				return nil, 0, false
			}
			_, err := tr.SetProtection("AAPLBUSDT", 1, 90, 89, tp)
			requireError(t, err, "protection canceled; replacement")
			if !errors.Is(err, ErrAuthOrPermission) {
				t.Fatal("replacement error lost permission classification")
			}
			if len(requestsTo(f, "/api/v3/order", "DELETE")) != 1 || len(f.orders) != 0 {
				t.Fatal("old protection was not canceled")
			}
			if tr.balance != nil || tr.positions != nil || !tr.history["AAPLBUSDT"].at.IsZero() {
				t.Fatal("failed replacement left stale snapshots")
			}
		})
	}
}

func TestCancelOrderRefusesManualOrders(t *testing.T) {
	tr, f, _ := fixture(t)
	manual := stopOrder(-1)
	manual.ClientOrderID = "manual"
	manual.OrderID = 55
	f.orders = []*binance.Order{manual}
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path == "/api/v3/order" && r.method == "GET" {
			return manual, 200, true
		}
		return nil, 0, false
	}
	requireError(t, tr.CancelOrder("AAPLBUSDT", "55"), "not a program order")
	if len(requestsTo(f, "/api/v3/order", "DELETE")) != 0 {
		t.Fatal("manual order cancelled")
	}
}
