package hyperliquid

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type reviewC1RT func(*http.Request) (*http.Response, error)

func (f reviewC1RT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReviewC1DelistedFilteredBeforeRanking(t *testing.T) {
	assetMappingCache.RLock()
	old, existed := assetMappingCache.byURL[hyperliquidInfoURL]
	assetMappingCache.RUnlock()
	t.Cleanup(func() {
		assetMappingCache.Lock()
		if existed {
			assetMappingCache.byURL[hyperliquidInfoURL] = old
		} else {
			delete(assetMappingCache.byURL, hyperliquidInfoURL)
		}
		assetMappingCache.Unlock()
	})
	p := &CoinProvider{httpClient: &http.Client{Transport: reviewC1RT(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"universe":[{"name":"DEAD","isDelisted":true},{"name":"kPEPE"},{"name":"BTC"}]},[{"dayNtlVlm":"999"},{"dayNtlVlm":"50"},{"dayNtlVlm":"20"}]]`))}, nil
	})}}
	all, err := p.GetAllCoins(context.Background())
	if err != nil || len(all) != 2 {
		t.Fatalf("all=%v err=%v", all, err)
	}
	main, err := p.GetMainCoins(context.Background(), 1)
	if err != nil || len(main) != 1 || main[0].Symbol != "kPEPE" {
		t.Fatalf("main=%v err=%v", main, err)
	}
}

func TestReviewC1ContractMappingKeepsNamesIDsAndUnits(t *testing.T) {
	mapping := AssetMappings([]AssetInfo{{Name: "BTC"}, {Name: "DEAD", IsDelisted: true}, {Name: "kPEPE"}, {Name: "PEPE"}, {Name: "1000PEPE"}})
	for _, tc := range []struct {
		symbol, name   string
		id, multiplier int
	}{
		{"KPEPEUSDT", "kPEPE", 2, 1000}, {" PEPE ", "PEPE", 3, 1}, {"1000pepeusdt", "1000PEPE", 4, 1000}, {"DEAD", "DEAD", 1, 1},
	} {
		asset, err := ResolveAsset(tc.symbol, mapping)
		if err != nil || asset.Name != tc.name || asset.AssetID != tc.id || asset.Multiplier != tc.multiplier {
			t.Fatalf("%s => %+v, %v", tc.symbol, asset, err)
		}
	}
	for _, unknown := range []string{"NOPE", "", " PEPE1000 "} {
		if _, err := ResolveAsset(unknown, mapping); err == nil {
			t.Fatalf("unknown %q accepted", unknown)
		}
	}
	if _, err := ResolveAsset("PEPE", AssetMappings([]AssetInfo{{Name: "kPEPE"}})); err == nil {
		t.Fatal("PEPE aliased to kPEPE")
	}
	if _, err := ResolveAsset("1000PEPE", AssetMappings([]AssetInfo{{Name: "kPEPE"}})); err == nil {
		t.Fatal("1000PEPE aliased to kPEPE")
	}
}

func TestReviewC1CandlesRestoreRawNameAndRejectUnknown(t *testing.T) {
	requests := 0
	c := NewClient()
	c.apiURL = "https://review-c1-candles.invalid/info"
	t.Cleanup(func() {
		assetMappingCache.Lock()
		delete(assetMappingCache.byURL, c.apiURL)
		assetMappingCache.Unlock()
	})
	c.client.Transport = reviewC1RT(func(r *http.Request) (*http.Response, error) {
		var req struct {
			Type string            `json:"type"`
			Req  CandleRequestBody `json:"req"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		body := `{"universe":[{"name":"BTC"},{"name":"kPEPE"}]}`
		if req.Type == "candleSnapshot" {
			requests++
			if req.Req.Coin != "kPEPE" {
				t.Fatalf("venue received %q", req.Req.Coin)
			}
			body = `[{"s":"kPEPE","c":"0.01"}]`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if _, err := c.GetCandles(context.Background(), "KPEPEUSDT", "1h", 1); err != nil {
		t.Fatal(err)
	}
	for _, unknown := range []string{"PEPEUSDT", "1000PEPEUSDT", "UNKNOWN"} {
		if _, err := c.GetCandles(context.Background(), unknown, "1h", 1); err == nil {
			t.Fatalf("unknown %q requested candles", unknown)
		}
	}
	if requests != 1 {
		t.Fatalf("candle requests = %d, want 1", requests)
	}
}

func TestReviewC1KnownCandleMappingSurvivesMetadataOutage(t *testing.T) {
	c := NewClient()
	c.apiURL = "https://review-c1-outage.invalid/info"
	rememberAssetMappings(c.apiURL, []AssetInfo{{Name: "BTC"}, {Name: "kPEPE", IsDelisted: true}})
	assetMappingCache.Lock()
	stale := assetMappingCache.byURL[c.apiURL]
	stale.fetchedAt = time.Now().Add(-2 * cacheDuration)
	assetMappingCache.byURL[c.apiURL] = stale
	assetMappingCache.Unlock()
	t.Cleanup(func() {
		assetMappingCache.Lock()
		delete(assetMappingCache.byURL, c.apiURL)
		assetMappingCache.Unlock()
	})
	candles := 0
	c.client.Transport = reviewC1RT(func(r *http.Request) (*http.Response, error) {
		var req CandleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Type == "meta" {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":"down"}`))}, nil
		}
		if req.Req.Coin != "kPEPE" {
			t.Fatalf("unexpected contract %q", req.Req.Coin)
		}
		candles++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
	})
	if _, err := c.GetCandles(context.Background(), "KPEPEUSDT", "1h", 1); err != nil {
		t.Fatalf("known position data blocked: %v", err)
	}
	if _, err := c.GetCandles(context.Background(), "PEPEUSDT", "1h", 1); err == nil {
		t.Fatal("unknown unit accepted during outage")
	}
	if candles != 1 {
		t.Fatalf("candle requests=%d", candles)
	}
}
