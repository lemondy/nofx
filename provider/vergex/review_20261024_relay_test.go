package vergex

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type reviewC1RT func(*http.Request) (*http.Response, error)

func (f reviewC1RT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func isolateReviewC1Relay(t *testing.T) {
	t.Helper()
	relayMu.Lock()
	old := relayCache
	relayCache = map[string]relayEntry{}
	relayMu.Unlock()
	oldNow := relayNow
	t.Cleanup(func() { relayMu.Lock(); relayCache = old; relayMu.Unlock(); relayNow = oldNow })
}

func TestReviewC1RelayErrorMustNotReplaceSuccess(t *testing.T) {
	isolateReviewC1Relay(t)
	for _, tc := range []struct{ path, good string }{
		{"/trending-category?key=ai500", `{"category":{"assets":[]}}`},
		{"/trending-crypto?tab=oi&duration=1h", `{"top":[],"low":[]}`},
		{"/api/v1/data-intelligence/flow/markets?window=1h", `{"data":{"inflow":[],"outflow":[]}}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			FeedRelay(tc.path, []byte(tc.good))
			for _, bad := range []string{`{"error":"temporarily unavailable"}`, `{"success":false}`, `{}`, `{"category":null}`, `{"top":null}`, `{"top":"oops"}`} {
				FeedRelay(tc.path, []byte(bad))
				got, ok := relayLookup(tc.path)
				if !ok || !bytes.Equal(got, []byte(tc.good)) {
					t.Fatalf("error replaced successful empty board: %s", got)
				}
			}
		})
	}
}

func TestReviewC1RelayHTTPStatusAndHealth(t *testing.T) {
	isolateReviewC1Relay(t)
	now := time.Now()
	relayNow = func() time.Time { return now }
	path := "/trending-category?key=ai500&limit=100"
	good := []byte(`{"category":{"assets":[{"pair":"BTCUSDT"}]}}`)
	if err := feedRelayHTTP(path, 200, good); err != nil {
		t.Fatal(err)
	}
	before, _ := relayLookupEntry(path)
	now = now.Add(relayTTL - time.Second)
	health := newRelayHealth(before.fetchedAt)
	for _, status := range []int{403, 503} {
		// Even a structurally valid ranking under an error HTTP status is a failure.
		err := feedRelayHTTP(path, status, []byte(`{"category":{"assets":[]}}`))
		if err == nil {
			t.Fatalf("HTTP %d accepted", status)
		}
		health.record(err == nil, err, now)
		after, _ := relayLookupEntry(path)
		if !bytes.Equal(after.body, before.body) || !after.fetchedAt.Equal(before.fetchedAt) || !after.expires.Equal(before.expires) {
			t.Fatal("HTTP error replaced or renewed cache")
		}
	}
	if health.failures != 2 || health.everOK {
		t.Fatalf("errors marked healthy: %+v", health)
	}
	now = before.expires
	if _, ok := relayLookup(path); ok {
		t.Fatal("relay served at expiration boundary")
	}
	if err := feedRelayHTTP(path, 200, []byte(`{"category":{"assets":[]}}`)); err != nil {
		t.Fatalf("legal empty board rejected: %v", err)
	}
}

func TestReviewC1AI500SnapshotRetainsSourceLifetimeAndFullList(t *testing.T) {
	isolateReviewC1Relay(t)
	now := time.Now()
	original := now
	relayNow = func() time.Time { return now }
	path := "/trending-category?key=ai500&limit=100"
	if err := FeedRelay(path, []byte(`{"category":{"assets":[{"pair":" BTC-USDT "},{"pair":"ETHUSDT"},{"pair":"btcusdt"}]}}`)); err != nil {
		t.Fatal(err)
	}
	c := NewClient()
	if small, err := c.GetAI500Symbols(1); err != nil || len(small) != 1 {
		t.Fatalf("small=%v err=%v", small, err)
	}
	if err := FeedRelay("/trending-category?key=ai500&limit=1", []byte(`{"category":{"assets":[{"pair":"BTCUSDT"}]}}`)); err == nil {
		t.Fatal("small relay payload accepted into shared full-list cache")
	}
	now = now.Add(29 * time.Minute)
	snapshot, err := c.GetAI500Snapshot()
	if err != nil || len(snapshot.Symbols) != 2 || snapshot.Symbols[0] != "BTCUSDT" || snapshot.Symbols[1] != "ETHUSDT" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if !snapshot.FetchedAt.Equal(original) || !snapshot.ExpiresAt.Equal(original.Add(relayTTL)) {
		t.Fatalf("cache renewed: %+v", snapshot)
	}
}

func TestReviewC1DirectHTTPErrorsAreNotEmptyRankings(t *testing.T) {
	isolateReviewC1Relay(t)
	for _, body := range []string{`{"error":"unavailable"}`, `{}`, `{"top":null}`, `{"top":[{"rank":"bad"}]}`} {
		c := NewClient()
		c.http.Transport = reviewC1RT(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if got, err := c.GetOITopSymbols(10); err == nil {
			t.Fatalf("error payload became successful empty board: %s => %v", body, got)
		}
	}
}

func TestReviewC1DirectAI500AlwaysFetchesFullRanking(t *testing.T) {
	isolateReviewC1Relay(t)
	c := NewClient()
	c.http.Transport = reviewC1RT(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("limit") != fmt.Sprint(maxLimit) {
			t.Fatalf("partial upstream request: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"category":{"assets":[]}}`))}, nil
	})
	if got, err := c.GetAI500Symbols(1); err != nil || len(got) != 0 {
		t.Fatalf("legal empty=%v err=%v", got, err)
	}
}
