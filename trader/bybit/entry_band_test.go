package bybit

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type bandTickTransport func(*http.Request) (*http.Response, error)

func (f bandTickTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEntryBandTickRules(t *testing.T) {
	old := bybitHTTP
	t.Cleanup(func() { bybitHTTP = old })
	for _, tc := range []struct {
		body string
		want float64
		bad  bool
	}{
		{`{"retCode":0,"result":{"list":[{"priceFilter":{"tickSize":"0.025"}}]}}`, .025, false},
		{`{"retCode":1,"result":{"list":[]}}`, 0, true},
		{`{"retCode":0,"result":{"list":[{"priceFilter":{}}]}}`, 0, true},
	} {
		bybitHTTP = &http.Client{Transport: bandTickTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Query().Get("symbol") != "XUSDT" {
				t.Fatal("wrong symbol")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
		})}
		got, err := (&BybitTrader{}).GetSymbolTickSize("XUSDT")
		if (err != nil) != tc.bad || got != tc.want {
			t.Fatalf("tick=%g err=%v", got, err)
		}
	}
}
