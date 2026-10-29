package binance

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"testing"

	"github.com/adshao/go-binance/v2/futures"
	"nofx/market"
)

func TestReview20261029LotFilterParity(t *testing.T) {
	lot := func(kind, minQty, step string) map[string]interface{} {
		return map[string]interface{}{"filterType": kind, "minQty": minQty, "stepSize": step, "maxQty": "1000"}
	}
	for _, tc := range []struct {
		name    string
		filters []map[string]interface{}
		want    market.SymbolLotFilter
	}{
		{"market stricter", []map[string]interface{}{
			lot("LOT_SIZE", "0.01", "0.001"), lot("MARKET_LOT_SIZE", "0.1", "0.01"),
			{"filterType": "MIN_NOTIONAL", "notional": "50"},
		}, market.SymbolLotFilter{MinQty: 0.1, StepSize: 0.01, MinNotional: 50}},
		{"limit stricter", []map[string]interface{}{
			lot("LOT_SIZE", "0.2", "0.01"), lot("MARKET_LOT_SIZE", "0.1", "0.001"),
			{"filterType": "NOTIONAL", "minNotional": "20"},
		}, market.SymbolLotFilter{MinQty: 0.2, StepSize: 0.01, MinNotional: 20}},
		{"independent maxima", []map[string]interface{}{
			lot("LOT_SIZE", "0.2", "0.001"), lot("MARKET_LOT_SIZE", "0.1", "0.01"),
			{"filterType": "MIN_NOTIONAL", "notional": "", "minNotional": "5"},
		}, market.SymbolLotFilter{MinQty: 0.2, StepSize: 0.01, MinNotional: 5}},
		{"market missing and notional missing", []map[string]interface{}{
			lot("LOT_SIZE", "0.025", "0.025"),
		}, market.SymbolLotFilter{MinQty: 0.025, StepSize: 0.025, MinNotional: 10}},
		{"notional precedence", []map[string]interface{}{
			lot("LOT_SIZE", "0.1", "0.1"),
			{"filterType": "NOTIONAL", "notional": "15", "minNotional": "25"},
		}, market.SymbolLotFilter{MinQty: 0.1, StepSize: 0.1, MinNotional: 15}},
		{"invalid notional", []map[string]interface{}{
			lot("LOT_SIZE", "0.1", "0.1"),
			{"filterType": "MIN_NOTIONAL", "notional": "NaN"},
			{"filterType": "NOTIONAL", "minNotional": "Inf"},
		}, market.SymbolLotFilter{MinQty: 0.1, StepSize: 0.1, MinNotional: 10}},
		{"first valid notional", []map[string]interface{}{
			lot("LOT_SIZE", "0.01", "0.01"),
			{"filterType": "MIN_NOTIONAL", "notional": "-1"},
			{"filterType": "NOTIONAL", "minNotional": "12"},
			{"filterType": "MIN_NOTIONAL", "notional": "30"},
		}, market.SymbolLotFilter{MinQty: 0.01, StepSize: 0.01, MinNotional: 12}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := futures.Symbol{Symbol: "PARITYUSDT", Status: "TRADING", Filters: tc.filters}
			body, err := json.Marshal(struct {
				Symbols []futures.Symbol `json:"symbols"`
			}{Symbols: []futures.Symbol{s}})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			tr := fapiTestTrader(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/fapi/v1/exchangeInfo" {
					t.Fatalf("unexpected request: %s", r.URL)
				}
				_, _ = io.WriteString(w, string(body))
			})
			planned := market.ParseSymbolLotFilter(s.Filters)
			if planned != tc.want {
				t.Fatalf("planned=%+v, want %+v", planned, tc.want)
			}
			if got := tr.symbolMinNotional(s); got != planned.MinNotional {
				t.Fatalf("adapter minNotional=%g, market=%g", got, planned.MinNotional)
			}
			marketRules, err := tr.getMarketOrderRules(s.Symbol)
			if err != nil {
				t.Fatal(err)
			}
			limitRules, err := tr.getSymbolOrderRules(s.Symbol)
			if err != nil {
				t.Fatal(err)
			}
			wantQty := math.Max(marketRules.MinQty, limitRules.MinQty)
			wantStep := math.Max(marketRules.StepSize, limitRules.StepSize)
			if planned.MinQty != wantQty || planned.StepSize != wantStep {
				t.Fatalf("planned=%+v, market rules=%+v, limit rules=%+v", planned, marketRules, limitRules)
			}
			if marketRules.MinNotional != planned.MinNotional || limitRules.MinNotional != planned.MinNotional {
				t.Fatalf("notional diverged: planned=%g market=%g limit=%g", planned.MinNotional, marketRules.MinNotional, limitRules.MinNotional)
			}
			if calls != 1 {
				t.Fatalf("adapter requests=%d, want 1 offline snapshot", calls)
			}
		})
	}
}
