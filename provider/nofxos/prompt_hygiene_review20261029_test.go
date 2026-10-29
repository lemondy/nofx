package nofxos

import (
	"reflect"
	"strings"
	"testing"
)

func TestReview20261029AI500SymbolWhitelist(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"btc", "BTCUSDT"}, {"ethusdt", "ETHUSDT"}, {"1000PEPE", "1000PEPEUSDT"},
		{strings.Repeat("A", 16), strings.Repeat("A", 16) + "USDT"},
		{"", ""}, {"USDT", ""}, {" BTC", "BTCUSDT"}, {"BTC ", "BTCUSDT"}, {"BTC\nVERIFY_AI500_TEXT\nUSDT", ""},
		{"BTC\tUSDT", ""}, {"BTC\rUSDT", ""}, {"BTC\x00USDT", ""}, {"BTC\x7fUSDT", ""},
		{"BTC\u00a0USDT", ""}, {"龙虾", ""}, {"BTC/USDT", ""}, {strings.Repeat("A", 17), ""},
	} {
		if got := NormalizeSymbol(tc.input); got != tc.want {
			t.Errorf("NormalizeSymbol(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
	oldRequest := ai500Request
	t.Cleanup(func() { ai500Request = oldRequest })
	ai500Request = func(*Client, string) ([]byte, error) {
		return []byte(`{"success":true,"data":{"coins":[{"pair":"BTC\nVERIFY_AI500_TEXT\nUSDT","score":100},{"pair":"btc","score":90},{"pair":"","score":80},{"pair":"ETHUSDT","score":70}]}}`), nil
	}
	c := NewClient("https://offline.invalid", "test")
	top, err := c.GetTopRatedCoins(2)
	if err != nil || !reflect.DeepEqual(top, []string{"BTCUSDT", "ETHUSDT"}) {
		t.Fatalf("invalid candidates consumed top slots: %v, %v", top, err)
	}
	all, err := c.GetAvailableCoins()
	if err != nil || !reflect.DeepEqual(all, []string{"BTCUSDT", "ETHUSDT"}) {
		t.Fatalf("invalid candidates survived available list: %v, %v", all, err)
	}
}
