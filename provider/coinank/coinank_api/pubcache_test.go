package coinank_api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"nofx/market/pubcache"
	"nofx/provider/coinank/coinank_enum"
)

func TestKlinePubCacheFailureRetryAndIndependentParsing(t *testing.T) {
	old := client
	t.Cleanup(func() { client = old })
	now := time.Unix(1000, 0)
	c := pubcache.New(pubcache.Options{Now: func() time.Time { return now }})
	calls := 0
	client = &http.Client{Transport: c.Wrap(klineRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/kline/list/open" || r.URL.Query().Get("ts") != "1000" || r.URL.Query().Get("size") != "2" {
			t.Fatalf("explicit historical query changed: %s", r.URL)
		}
		body := `{"success":true,"data":[[1,2,100,101,102,99,10,20,3]]}`
		if calls == 1 {
			body = `{"success":false,"code":"429"}` // HTTP 200 vendor failure
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))}
	fetch := func() (float64, error) {
		rows, err := Kline(context.Background(), "BTCUSDT", coinank_enum.Binance, 1000, coinank_enum.To, 2, coinank_enum.Hour1)
		if err != nil {
			return 0, err
		}
		close := rows[0].Close
		rows[0].Close = -1
		return close, nil
	}
	if _, err := fetch(); err == nil {
		t.Fatal("vendor failure must surface")
	}
	for i := 0; i < 2; i++ {
		if close, err := fetch(); err != nil || close != 101 {
			t.Fatalf("retry/copy: close=%v err=%v", close, err)
		}
	}
	if calls != 2 {
		t.Fatalf("failed response cached or success not shared: calls=%d", calls)
	}
	now = now.Add(pubcache.KlineTTL)
	fetch()
	if calls != 3 {
		t.Fatalf("expired response reused: calls=%d", calls)
	}
}

func TestKlinePubCachePreservesExplicitHistoricalCursors(t *testing.T) {
	old := client
	t.Cleanup(func() { client = old })
	c := pubcache.New(pubcache.Options{})
	var cursors []string
	client = &http.Client{Transport: c.Wrap(klineRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		cursors = append(cursors, r.URL.Query().Get("ts"))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"success":true,"data":[[1,2,100,101,102,99,10,20,3]]}`))}, nil
	}))}
	// Neither timestamp is aligned to a 15s latest window; both must retain
	// their exact value, use separate entries, and then share only their own URL.
	for _, ts := range []int64{1700000000123, 1700000000456, 1700000000123} {
		if _, err := Kline(context.Background(), "BTCUSDT", coinank_enum.Binance, ts, coinank_enum.To, 2, coinank_enum.Hour1); err != nil {
			t.Fatal(err)
		}
	}
	if len(cursors) != 2 || cursors[0] != "1700000000123" || cursors[1] != "1700000000456" || c.Stats().Hits != 1 {
		t.Fatalf("historical cursors changed or mixed: cursors=%v stats=%+v", cursors, c.Stats())
	}
}
