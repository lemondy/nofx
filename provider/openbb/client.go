package openbb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"nofx/logger"
	"nofx/security"
)

// ============================================================================
// OpenBB sidecar client (user directive 2026-09-25): an OpenBB Platform REST
// API running locally (uvicorn openbb_core.api.rest_api:app, default
// 127.0.0.1:6900) provides vendor-diversified data the primary stack lacks:
// yfinance crypto/equity history (KLINE FALLBACK tier 3) and news search
// (prompt enrichment). The sidecar is OPTIONAL infrastructure — every call
// is short-timeout, cached, and fails open; when it's down the system runs
// exactly as before.
//
// Setup (one-time, on this Mac):
//   uv venv --python 3.12 .venv-openbb
//   uv pip install --python .venv-openbb/bin/python openbb
//   .venv-openbb/bin/python -m uvicorn openbb_core.api.rest_api:app \
//       --host 127.0.0.1 --port 6900 --workers 1
// Override the URL with OPENBB_API_URL when the sidecar listens elsewhere.
// ============================================================================

var (
	baseURL    = ""
	clientOnce sync.Once
	httpClient *http.Client
	mu         sync.RWMutex
	// newsCache: one fetch per symbol per TTL — the prompt rebuilds every
	// cycle but news moves on minutes, not seconds.
	newsCache   = map[string]newsCacheEntry{}
	newsCacheMu sync.Mutex
)

const newsCacheTTL = 15 * time.Minute

type newsCacheEntry struct {
	items []NewsItem
	at    time.Time
}

func baseURLResolved() string {
	mu.RLock()
	defer mu.RUnlock()
	if baseURL != "" {
		return baseURL
	}
	if v := os.Getenv("OPENBB_API_URL"); v != "" {
		baseURL = strings.TrimRight(v, "/")
		return baseURL
	}
	baseURL = "http://127.0.0.1:6900/api/v1"
	return baseURL
}

func client() *http.Client {
	clientOnce.Do(func() {
		// Same proxy semantics as every other outbound call — the local
		// sidecar must BYPASS it, so use a dedicated no-proxy transport.
		httpClient = &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{Proxy: nil},
		}
	})
	return httpClient
}

// Available probes the sidecar (cheap, cached 5 min) — callers use this to
// decide whether the enrichment layer exists at all.
var (
	availMu      sync.Mutex
	availState   bool
	availChecked time.Time
)

func Available() bool {
	availMu.Lock()
	defer availMu.Unlock()
	if time.Since(availChecked) < 5*time.Minute {
		return availState
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURLResolved()+"/coverage", nil)
	if err != nil {
		availState, availChecked = false, time.Now()
		return false
	}
	resp, err := client().Do(req)
	if err != nil {
		availState, availChecked = false, time.Now()
		return false
	}
	resp.Body.Close()
	availState, availChecked = resp.StatusCode == http.StatusOK, time.Now()
	return availState
}

// NewsItem is one headline, prompt-ready.
type NewsItem struct {
	Date    string `json:"date"`
	Title   string `json:"title"`
	Site    string `json:"site,omitempty"`
	URL     string `json:"url,omitempty"`
	Symbols string `json:"symbols,omitempty"`
}

// NewsForSymbol returns recent headlines for one base symbol (BTC/SAGA/...).
// Provider chain: the sidecar's news/company command (benzinga/fmp/... per
// what the platform has installed; this box ships the news router). Bounded:
// max 6 items, 15-min cache, any failure → empty (absent, never padded).
func NewsForSymbol(base string) []NewsItem {
	newsCacheMu.Lock()
	if e, ok := newsCache[base]; ok && time.Since(e.at) < newsCacheTTL {
		newsCacheMu.Unlock()
		return e.items
	}
	newsCacheMu.Unlock()

	items := fetchNews(base)
	newsCacheMu.Lock()
	newsCache[base] = newsCacheEntry{items: items, at: time.Now()}
	// bound the cache
	if len(newsCache) > 200 {
		for k := range newsCache {
			delete(newsCache, k)
			break
		}
	}
	newsCacheMu.Unlock()
	return items
}

func fetchNews(base string) []NewsItem {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	q := url.Values{}
	q.Set("symbol", strings.ToUpper(base))
	q.Set("limit", "8")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURLResolved()+"/news/company?"+q.Encode(), nil)
	if err != nil {
		return nil
	}
	resp, err := client().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Debugf("openbb news %s: status %d", base, resp.StatusCode)
		return nil
	}
	var payload struct {
		Results []NewsItem `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}
	out := make([]NewsItem, 0, 6)
	for _, it := range payload.Results {
		if strings.TrimSpace(it.Title) == "" {
			continue
		}
		out = append(out, it)
		if len(out) >= 6 {
			break
		}
	}
	return out
}

// CryptoKlines pulls historical crypto candles via the sidecar (provider
// yfinance on this box). Symbol mapping: BASE → BASE-USD. Returns
// [msUTC, open, high, low, close, volume] rows like Binance's raw format.
// Altcoin coverage on yfinance is spotty — callers treat this as the LAST
// tier of the kline fallback chain.
func CryptoKlines(ctx context.Context, base string, interval string, limit int) ([][]float64, error) {
	q := url.Values{}
	q.Set("symbol", strings.ToUpper(base)+"-USD")
	q.Set("provider", "yfinance")
	q.Set("interval", mapYFInterval(interval))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURLResolved()+"/crypto/price/historical?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openbb klines: status %d", resp.StatusCode)
	}
	var payload struct {
		Results []struct {
			Date    time.Time `json:"date"`
			Open    float64   `json:"open"`
			High    float64   `json:"high"`
			Low     float64   `json:"low"`
			Close   float64   `json:"close"`
			Volume  float64   `json:"volume"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	rows := make([][]float64, 0, len(payload.Results))
	for _, r := range payload.Results {
		if r.Close <= 0 {
			continue
		}
		rows = append(rows, []float64{
			float64(r.Date.UnixMilli()), r.Open, r.High, r.Low, r.Close, r.Volume,
		})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("openbb klines: empty result for %s", base)
	}
	return rows, nil
}

func mapYFInterval(interval string) string {
	switch interval {
	case "1m", "2m", "5m", "15m", "30m", "60m", "90m", "1h":
		if interval == "1h" {
			return "60m"
		}
		if interval == "30m" {
			return "30m"
		}
		if interval == "15m" {
			return "15m"
		}
		if interval == "5m" {
			return "5m"
		}
		if interval == "1m" {
			return "1m"
		}
		if interval == "2m" {
			return "2m"
		}
		return "60m"
	case "1d", "1w":
		if interval == "1w" {
			return "1wk"
		}
		return "1d"
	case "4h":
		return "60m" // yfinance has no 4h — callers aggregate or accept 1h
	}
	return "60m"
}

// _ keeps security imported for parity with other providers (proxy policy
// lives there; the sidecar deliberately bypasses it).
var _ = security.SafeHTTPClient
