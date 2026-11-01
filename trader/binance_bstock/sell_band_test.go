package binance_bstock

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	binance "github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/common"
	"nofx/trader/types"
)

func TestSellPriceBand(t *testing.T) {
	for _, tc := range []struct {
		name         string
		average      bool
		intersection bool
		floor, ceil  float64
		down, up     float64
	}{
		{"last price sell side", false, false, 90, 110, .9, 1.1},
		{"average sell side", true, false, 90.03, 110.03, .9, 1.1},
		{"intersection and inward ticks", true, true, 95.03, 105.03, .95, 1.05},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, f, _ := fixture(t)
			pf := f.symbols[0].Filters[3]
			pf["askMultiplierDown"], pf["askMultiplierUp"] = "0.9", "1.1"
			pf["bidMultiplierDown"], pf["bidMultiplierUp"] = "0.5", "2"
			if tc.average {
				pf["avgPriceMins"] = 5
			}
			if tc.intersection {
				f.symbols[0].Filters = append(f.symbols[0].Filters, map[string]interface{}{
					"filterType": "PERCENT_PRICE", "multiplierDown": "0.95", "multiplierUp": "1.05", "avgPriceMins": 5,
				})
			}
			f.handle = func(r requestRecord) (interface{}, int, bool) {
				if r.path == "/api/v3/avgPrice" {
					if r.params.Get("symbol") != "AAPLBUSDT" {
						t.Fatal(r.params)
					}
					return map[string]interface{}{"mins": 5, "price": "100.03"}, 200, true
				}
				return nil, 0, false
			}
			band, err := tr.SellPriceBand("AAPLBUSDT")
			if err != nil {
				t.Fatal(err)
			}
			near(t, band.Floor, tc.floor)
			near(t, band.Ceiling, tc.ceil)
			near(t, band.DownMultiplier, tc.down)
			near(t, band.UpMultiplier, tc.up)
			wantAvg, wantLast, ref := 0, 1, 100.0
			if tc.average {
				wantAvg, wantLast, ref = 1, 0, 100.03
			}
			near(t, band.Ref, ref)
			if len(requestsTo(f, "/api/v3/avgPrice", "GET")) != wantAvg || len(requestsTo(f, "/api/v3/ticker/price", "GET")) != wantLast {
				t.Fatalf("reference price requests: %+v", f.requests)
			}
		})
	}
}

func TestSellPriceBandMixedReferences(t *testing.T) {
	tr, f, _ := fixture(t)
	f.symbols[0].Filters[3]["avgPriceMins"] = 5
	f.symbols[0].Filters = append(f.symbols[0].Filters, map[string]interface{}{
		"filterType": "PERCENT_PRICE", "multiplierDown": "0.9", "multiplierUp": "1.1", "avgPriceMins": 0,
	})
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path == "/api/v3/avgPrice" {
			return map[string]interface{}{"mins": 5, "price": "120"}, 200, true
		}
		return nil, 0, false
	}
	band, err := tr.SellPriceBand("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	near(t, band.Floor, 96)
	near(t, band.Ceiling, 110)
	near(t, band.DownMultiplier, .9)
	near(t, band.UpMultiplier, 1.1)
	if len(requestsTo(f, "/api/v3/avgPrice", "GET")) != 1 || len(requestsTo(f, "/api/v3/ticker/price", "GET")) != 1 {
		t.Fatal(f.requests)
	}
}

func TestSellPriceBandWithoutPercentFilter(t *testing.T) {
	tr, f, _ := fixture(t)
	f.symbols[0].Filters = f.symbols[0].Filters[:3]
	band, err := tr.SellPriceBand("AAPLBUSDT")
	if err != nil || band != (types.SellBand{}) {
		t.Fatalf("band=%+v err=%v", band, err)
	}
	if len(f.requests) != 1 || f.requests[0].path != "/api/v3/exchangeInfo" {
		t.Fatalf("unnecessary price read: %+v", f.requests)
	}
}

func TestProtectionSellBandValidationKeepsOldStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		tp   float64
		edit func(map[string]interface{})
		ref  string
		band bool
	}{
		{"below floor", 0, func(f map[string]interface{}) { f["askMultiplierDown"] = "0.9" }, "100", true},
		{"tp above ceiling", 121, nil, "100", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, f, _ := fixture(t)
			pf := f.symbols[0].Filters[3]
			pf["avgPriceMins"] = 5
			if tc.edit != nil {
				tc.edit(pf)
			}
			old := stopOrder(-1)
			f.orders = []*binance.Order{old}
			f.handle = func(r requestRecord) (interface{}, int, bool) {
				if r.path == "/api/v3/avgPrice" {
					if tc.ref == "error" {
						return map[string]interface{}{"code": -1000, "msg": "unavailable"}, 500, true
					}
					return map[string]interface{}{"mins": 5, "price": tc.ref}, 200, true
				}
				return nil, 0, false
			}
			_, err := tr.SetProtection("AAPLBUSDT", 2, 90, 89.5, tc.tp)
			if err == nil || errors.Is(err, types.ErrStopBelowSellBand) != tc.band {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, r := range f.requests {
				if r.method == http.MethodDelete || r.method == http.MethodPost {
					t.Fatalf("band validation mutated protection: %+v", r)
				}
			}
			if len(f.orders) != 1 || f.orders[0] != old {
				t.Fatal("old stop lost")
			}
		})
	}
}

func TestProtectionExchangePercentFailureIsBandError(t *testing.T) {
	for _, tp := range []float64{0, 120} {
		for _, kind := range []string{"PERCENT_PRICE", "PERCENT_PRICE_BY_SIDE"} {
			t.Run(fmt.Sprintf("%g/%s", tp, kind), func(t *testing.T) {
				tr, f, _ := fixture(t)
				f.orders = []*binance.Order{stopOrder(-1)}
				f.handle = func(r requestRecord) (interface{}, int, bool) {
					if r.method == "POST" && (r.path == "/api/v3/order" || r.path == "/api/v3/orderList/oco") {
						return map[string]interface{}{"code": -1013, "msg": "Filter failure: " + kind}, 400, true
					}
					return nil, 0, false
				}
				_, err := tr.SetProtection("AAPLBUSDT", 2, 90, 89.5, tp)
				var api *common.APIError
				if !errors.Is(err, types.ErrStopBelowSellBand) || !strings.Contains(err.Error(), "protection canceled; replacement") || !errors.As(err, &api) || api.Code != -1013 {
					t.Fatalf("lost classification or exchange error: %v", err)
				}
				if len(requestsTo(f, "/api/v3/order", "DELETE")) != 1 {
					t.Fatal("expected old protection cancelled before exchange rejection")
				}
			})
		}
	}
}

// An unreadable band must not block the replacement: the caller has already
// retired the old legs, and the exchange remains authoritative.
func TestProtectionUnreadableSellBandStillPlaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]interface{})
		ref  string
	}{
		{"missing multiplier", func(f map[string]interface{}) { delete(f, "askMultiplierDown") }, "100"},
		{"invalid multiplier", func(f map[string]interface{}) { f["askMultiplierUp"] = "NaN" }, "100"},
		{"invalid reference", nil, "0"},
		{"average unavailable", nil, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, f, _ := fixture(t)
			pf := f.symbols[0].Filters[3]
			pf["avgPriceMins"] = 5
			if tc.edit != nil {
				tc.edit(pf)
			}
			f.orders = []*binance.Order{stopOrder(-1)}
			f.handle = func(r requestRecord) (interface{}, int, bool) {
				if r.path == "/api/v3/avgPrice" {
					if tc.ref == "error" {
						return map[string]interface{}{"code": -1000, "msg": "unavailable"}, 500, true
					}
					return map[string]interface{}{"mins": 5, "price": tc.ref}, 200, true
				}
				return nil, 0, false
			}
			p, err := tr.SetProtection("AAPLBUSDT", 2, 90, 89.5, 0)
			if err != nil || p == nil || p.StopOrderID == "" {
				t.Fatalf("replacement not placed: %+v %v", p, err)
			}
		})
	}
}
