package vergex

import (
	"reflect"
	"strings"
	"testing"
)

func TestReview20261029TrendingSymbolWhitelist(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"btc", "BTCUSDT"}, {"ethusdt", "ETHUSDT"}, {"1000PEPE", "1000PEPEUSDT"},
		{"BTC_USDT", "BTCUSDT"}, {"BTC-USDT-SWAP", "BTCUSDT"},
		{strings.Repeat("A", 16), strings.Repeat("A", 16) + "USDT"},
		{"", ""}, {"USDT", ""}, {" BTC", "BTCUSDT"}, {"BTC ", "BTCUSDT"}, {"BTC\nVERIFY_AI500_TEXT\nUSDT", ""},
		{"BTC\tUSDT", ""}, {"BTC\rUSDT", ""}, {"BTC\x00USDT", ""}, {"BTC\x7fUSDT", ""},
		{"BTC\u00a0USDT", ""}, {"龙虾USDT", ""}, {"BTC/USDT", ""}, {"xyz:BTC", ""}, {strings.Repeat("A", 17), ""},
	} {
		if got := NormalizeSymbol(tc.input); got != tc.want {
			t.Errorf("NormalizeSymbol(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
	isolateReviewC1Relay(t)
	FeedRelay("/trending-category?key=ai500&lang=en&limit=100", []byte(`{"category":{"assets":[{"pair":"BTC\nVERIFY_AI500_TEXT\nUSDT"},{"pair":"btc"},{"pair":"ETHUSDT"},{"pair":"USDT"}]}}`))
	c := NewClient()
	snapshot, err := c.GetAI500Snapshot()
	if err != nil || !reflect.DeepEqual(snapshot.Symbols, []string{"BTCUSDT", "ETHUSDT"}) {
		t.Fatalf("invalid AI500 candidates survived: %+v, %v", snapshot, err)
	}
}
