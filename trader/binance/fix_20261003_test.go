package binance

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/adshao/go-binance/v2/futures"
)

func TestRejectedStopReplacementRestoresOriginalExactID(t *testing.T) {
	for _, visible := range []bool{true, false} {
		t.Run(strconv.FormatBool(visible), func(t *testing.T) {
			posts := 0
			cancelled := []string{}
			oldLive := true
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/fapi/v1/exchangeInfo":
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"symbols": []map[string]interface{}{{"symbol": "XUSDT", "pricePrecision": 1, "filters": []map[string]string{{"filterType": "PRICE_FILTER", "tickSize": "0.1"}}}}})
				case "/fapi/v1/openOrders":
					_ = json.NewEncoder(w).Encode([]interface{}{})
				case "/fapi/v1/openAlgoOrders":
					orders := []map[string]interface{}{}
					if oldLive || (posts >= 3 && visible) {
						orders = append(orders, map[string]interface{}{"algoId": 1, "symbol": "XUSDT", "orderType": "STOP_MARKET", "positionSide": "LONG", "triggerPrice": "95", "quantity": "0", "closePosition": true})
					}
					_ = json.NewEncoder(w).Encode(orders)
				case "/fapi/v1/algoOrder":
					if r.Method == http.MethodDelete {
						body, _ := io.ReadAll(r.Body)
						form, _ := url.ParseQuery(string(body))
						cancelled = append(cancelled, form.Get("algoId"))
						oldLive = false
						_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": "200"})
						return
					}
					posts++
					if posts <= 2 {
						w.WriteHeader(400)
						code := -4130
						if posts == 2 {
							code = -2010
						}
						_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": code, "msg": "simulated rejection"})
						return
					}
					_ = r.ParseForm()
					if r.Form.Get("triggerPrice") != "95" {
						t.Errorf("restored wrong trigger: %s", r.Form.Get("triggerPrice"))
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"algoId": 2, "symbol": "XUSDT"})
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.Error(w, "unexpected", 404)
				}
			}))
			defer srv.Close()
			client := futures.NewClient("key", "secret")
			client.BaseURL = srv.URL
			client.HTTPClient = srv.Client()
			trader := &FuturesTrader{client: client}
			err := trader.SetStopLoss("XUSDT", "LONG", 1, 99)
			if err == nil || posts != 3 || len(cancelled) != 1 || cancelled[0] != "1" {
				t.Fatalf("replacement err=%v posts=%d canceled=%v", err, posts, cancelled)
			}
			if !visible && !strings.Contains(err.Error(), "restoration unconfirmed") {
				t.Fatalf("invisible restoration falsely confirmed: %v", err)
			}
		})
	}
}
