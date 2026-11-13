package bitget

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type bandTickTransport func(*http.Request) (*http.Response, error)

func (f bandTickTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEntryBandNonDecimalTickRule(t *testing.T) {
	trader := &BitgetTrader{contractsCache: make(map[string]*BitgetContract)}
	trader.httpClient = &http.Client{Transport: bandTickTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":"00000","data":[{"symbol":"XUSDT","pricePlace":"3","priceEndStep":"25"}]}`)), Header: make(http.Header)}, nil
	})}
	tick, err := trader.GetSymbolTickSize("XUSDT")
	if err != nil || tick != .025 {
		t.Fatalf("tick=%g err=%v", tick, err)
	}
}
