package binance

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

func TestMain(m *testing.M) {
	// Legacy tests never persist rules. Persistence tests inject t.TempDir().
	FuturesRulesCachePath = ""
	// Auth diagnostics also stay offline. All exchange fixtures install their
	// own in-memory transport; reject any unintended default-client traffic.
	http.DefaultTransport = batch1Transport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "api4.ipify.org", "ipv4.icanhazip.com", "api.ipify.org":
			return batch1Response(r, http.StatusOK, "127.0.0.1"), nil
		default:
			return nil, fmt.Errorf("unexpected network request in offline test: %s", r.URL.Host)
		}
	})
	os.Exit(m.Run())
}

// Use httptest's HTTP handler/recorder with the real SDK, without opening a
// socket or allowing any requests to reach Binance in the restricted sandbox.
func fapiTestTrader(t *testing.T, handler http.HandlerFunc) *FuturesTrader {
	t.Helper()
	client := futures.NewClient("fapi-key", "fapi-secret")
	client.BaseURL = futuresTestURL()
	client.HTTPClient = &http.Client{Timeout: time.Second, Transport: batch1Transport(func(r *http.Request) (*http.Response, error) {
		if r.Body == nil {
			r.Body = http.NoBody
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})}
	return &FuturesTrader{client: client, cacheDuration: time.Hour}
}

type fapiTestClock struct{ nanos atomic.Int64 }

func newFapiTestClock() *fapiTestClock {
	c := &fapiTestClock{}
	c.nanos.Store(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *fapiTestClock) now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *fapiTestClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }
