package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"nofx/store"
)

func TestReview20261028RemManualCloseMissingPrice(t *testing.T) {
	srv, _, _, _, traderID := idorFixture(t)
	for _, price := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		srv.recordClosePositionOrder(traderID, "ex", "kucoin", "XUSDT", "LONG", 10, price, map[string]interface{}{"orderId": "missing-price"})
	}
	orders, err := srv.store.Order().GetTraderOrders(traderID, 100)
	if err != nil || len(orders) != 0 {
		t.Fatalf("invented FILLED orders: %+v %v", orders, err)
	}
	var fills int64
	if err := srv.store.GormDB().Model(&store.TraderFill{}).Count(&fills).Error; err != nil || fills != 0 {
		t.Fatalf("invented fills=%d err=%v", fills, err)
	}
	// A venue receipt wins over the fallback and supports numeric strings.
	srv.recordClosePositionOrder(traderID, "ex", "kucoin", "XUSDT", "LONG", 10, 99, map[string]interface{}{"orderId": "receipt", "avgPrice": "101.25"})
	orders, err = srv.store.Order().GetTraderOrders(traderID, 100)
	if err != nil || len(orders) != 1 || orders[0].AvgFillPrice != 101.25 {
		t.Fatalf("receipt not recorded: %+v err=%v", orders, err)
	}
	actual, err := srv.store.Order().GetOrderFills(orders[0].ID)
	if err != nil || len(actual) != 1 || actual[0].Price != 101.25 {
		t.Fatalf("fills %+v err=%v", actual, err)
	}
	// Kucoin's immediate close response uses fillPrice.
	srv.recordClosePositionOrder(traderID, "ex", "kucoin", "YUSDT", "SHORT", 1, 0, map[string]interface{}{"orderId": "fill-price", "fillPrice": 17.0})
	orders, err = srv.store.Order().GetTraderOrders(traderID, 100)
	if err != nil || len(orders) != 2 {
		t.Fatalf("fill receipt lost: %+v err=%v", orders, err)
	}
}

func TestReview20261028RemEquityTimestampUTC(t *testing.T) {
	srv, _, _, _, traderID := idorFixture(t)
	timestamp := time.Date(2026, 10, 28, 16, 30, 12, 0, time.FixedZone("CST", 8*3600))
	if err := srv.store.Equity().Save(&store.EquitySnapshot{TraderID: traderID, Timestamp: timestamp, TotalEquity: 100, Balance: 100}); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.GET("/equity", srv.handleEquityHistory)
	r.GET("/batch", srv.handleEquityHistoryBatch)
	for _, path := range []string{"/equity?trader_id=" + traderID, "/batch?trader_ids=" + traderID} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		// Both response shapes must contain the exact same zoned instant.
		var body interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var timestamps []string
		var visit func(interface{})
		visit = func(v interface{}) {
			switch x := v.(type) {
			case map[string]interface{}:
				for k, c := range x {
					if k == "timestamp" {
						timestamps = append(timestamps, c.(string))
					} else {
						visit(c)
					}
				}
			case []interface{}:
				for _, c := range x {
					visit(c)
				}
			}
		}
		visit(body)
		if len(timestamps) != 1 || timestamps[0] != "2026-10-28T08:30:12Z" {
			t.Fatalf("%s timestamps=%v body=%s", path, timestamps, w.Body.String())
		}
	}
}
