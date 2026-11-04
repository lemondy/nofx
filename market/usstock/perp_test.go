package usstock

import (
	"context"
	"net/http"
	"testing"
)

func TestRegistryPerpMappingStrict(t *testing.T) {
	setupHTTP(t, et("2026-10-09 12:00"), nil)
	for _, tc := range []struct {
		kind, status, underlying string
		want                     bool
	}{
		{"TRADIFI_PERPETUAL", "TRADING", "EQUITY", true},
		{"PERPETUAL", "TRADING", "EQUITY", false},
		{"TRADIFI_PERPETUAL", "BREAK", "EQUITY", false},
		{"TRADIFI_PERPETUAL", "TRADING", "COIN", false},
	} {
		httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/api/v3/exchangeInfo" {
				return response(200, spotFixture), nil
			}
			return response(200, `{"symbols":[{"symbol":"AAPLUSDT","baseAsset":"AAPL","quoteAsset":"USDT","contractType":"`+tc.kind+`","status":"`+tc.status+`","underlyingType":"`+tc.underlying+`"},{"baseAsset":"SPY","underlyingType":"EQUITY"}]}`), nil
		})
		infos, err := loadSymbols(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		mapped := ""
		for _, info := range infos {
			if info.Symbol == "AAPLBUSDT" {
				mapped = info.PerpSymbol
			}
		}
		if (mapped == "AAPLUSDT") != tc.want {
			t.Fatalf("%+v mapping=%q", tc, mapped)
		}
	}
}

func TestPerpQuoteLastPriceAndTimestamp(t *testing.T) {
	now := et("2026-10-09 12:00")
	setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/fapi/v1/ticker/price" || r.URL.Query().Get("symbol") != "AAPLUSDT" {
			t.Fatalf("unexpected %s", r.URL)
		}
		return response(200, `{"symbol":"AAPLUSDT","price":"123.45"}`), nil
	})
	q, err := GetPerpQuote(context.Background(), "AAPLUSDT")
	if err != nil || q.Price != 123.45 || !q.Time.Equal(now) {
		t.Fatalf("%+v %v", q, err)
	}
	httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `{"symbol":"AAPLUSDT","price":"NaN"}`), nil
	})
	if _, err := GetPerpQuote(context.Background(), "AAPLUSDT"); err == nil {
		t.Fatal("NaN accepted")
	}
}
