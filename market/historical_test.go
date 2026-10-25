package market

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type historicalRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn historicalRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestGetKlinesRangeInvalidSymbol(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		invalid bool
	}{
		{"invalid symbol", 400, `{"code":-1121,"msg":"Invalid symbol."}`, true},
		{"invalid symbol spaced", 400, `{"msg": "Invalid symbol.", "code": -1121}`, true},
		{"other 400", 400, `{"code":-1100,"msg":"Illegal characters."}`, false},
		{"message alone", 400, `{"msg":"Invalid symbol."}`, false},
		{"code in message", 400, `{"code":-1100,"msg":"-1121 Invalid symbol."}`, false},
		{"malformed JSON", 400, `{"code":-1121`, false},
		{"upstream failure", 503, "Service unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise GetKlinesRange's real HTTP/error path without opening a
			// listener or requiring network access in the sandbox.
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			calls := 0
			http.DefaultTransport = historicalRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host != "fapi.binance.com" || req.URL.Path != "/fapi/v1/klines" || req.URL.Query().Get("symbol") != Normalize("AAPLUSDT") {
					t.Fatalf("unexpected klines URL: %s", req.URL)
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			start := time.Unix(1_700_000_000, 0)
			bars, err := GetKlinesRange("AAPLUSDT", "1m", start, start.Add(time.Minute))
			if err == nil {
				t.Fatal("expected API rejection")
			}
			if calls != 1 || len(bars) != 0 {
				t.Fatalf("calls=%d bars=%v, want one failed request", calls, bars)
			}
			if got := errors.Is(err, ErrInvalidSymbol); got != tc.invalid {
				t.Fatalf("errors.Is(ErrInvalidSymbol)=%v, want %v: %v", got, tc.invalid, err)
			}
			want := fmt.Sprintf("binance klines api returned status %d: %s", tc.status, tc.body)
			if err.Error() != want {
				t.Fatalf("API error text changed: got %q, want %q", err.Error(), want)
			}
		})
	}
}
