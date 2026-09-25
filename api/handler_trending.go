package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nofx/logger"
	"nofx/provider/vergex"

	"github.com/gin-gonic/gin"
)

// Vergex trending data proxy (https://vergex.trade/trending).
// All endpoints are public; responses are cached in-memory for a short TTL
// so the dashboard doesn't hammer the upstream on every page view.
//
// vergex.trade challenges every non-browser client behind Cloudflare, so the
// backend's own requests may 403. The web UI therefore fetches vergex
// directly (real browsers pass the challenge, CORS is open) and relays the
// payloads to /api/trending/relay, which feeds both this proxy cache and the
// provider/vergex client used by strategy coin sources.

const (
	vergexBaseURL     = "https://vergex.trade"
	vergexHTTPTimeout = 15 * time.Second
	vergexCacheTTL    = 30 * time.Second
	// vergexRelayTTL is how long a browser-relayed payload stays usable. The
	// UI re-relays every minute while the data page is open.
	vergexRelayTTL = 10 * time.Minute
)

var (
	vergexCache   = map[string]vergexCacheEntry{}
	vergexCacheMu sync.RWMutex
	vergexClient  = &http.Client{Timeout: vergexHTTPTimeout}
)

type vergexCacheEntry struct {
	data    []byte
	expires time.Time
	relay   bool // fed through /api/trending/relay, uses vergexRelayTTL
}

// fetchVergexJSON GETs a vergex endpoint (path with encoded query) and returns
// the raw JSON body, serving from cache when fresh.
func fetchVergexJSON(pathWithQuery string) ([]byte, error) {
	vergexCacheMu.RLock()
	if entry, ok := vergexCache[pathWithQuery]; ok && time.Now().Before(entry.expires) {
		vergexCacheMu.RUnlock()
		return entry.data, nil
	}
	vergexCacheMu.RUnlock()

	resp, err := vergexClient.Get(vergexBaseURL + pathWithQuery)
	if err != nil {
		return nil, fmt.Errorf("vergex request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vergex returned status %d for %s", resp.StatusCode, pathWithQuery)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		return nil, fmt.Errorf("vergex read body failed: %w", err)
	}

	vergexCacheMu.Lock()
	vergexCache[pathWithQuery] = vergexCacheEntry{data: body, expires: time.Now().Add(vergexCacheTTL)}
	// Opportunistic cleanup so the map doesn't grow unbounded.
	if len(vergexCache) > 256 {
		now := time.Now()
		for k, v := range vergexCache {
			if now.After(v.expires) {
				delete(vergexCache, k)
			}
		}
	}
	vergexCacheMu.Unlock()

	return body, nil
}

func vergexProxyJSON(c *gin.Context, pathWithQuery string) {
	data, err := fetchVergexJSON(pathWithQuery)
	if err != nil {
		logger.Warnf("⚠️ Vergex trending fetch failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Trending data source unavailable"})
		return
	}
	c.Data(http.StatusOK, "application/json", data)
}

// handleTrendingCrypto proxies /trending-crypto (net_flow / oi / depth / rates / price tabs).
func (s *Server) handleTrendingCrypto(c *gin.Context) {
	tab := c.DefaultQuery("tab", "net_flow")
	switch tab {
	case "net_flow", "oi", "price":
		duration := c.DefaultQuery("duration", "1h")
		switch duration {
		case "5m", "15m", "30m", "1h", "4h", "8h", "12h", "24h":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid duration, must be one of 5m,15m,30m,1h,4h,8h,12h,24h"})
			return
		}
	case "depth", "rates":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tab, must be one of net_flow,oi,depth,rates,price"})
		return
	}

	limit := clampQueryInt(c, "limit", 50, 1, 100)

	query := url.Values{}
	query.Set("tab", tab)
	if tab == "net_flow" || tab == "oi" || tab == "price" {
		query.Set("duration", c.DefaultQuery("duration", "1h"))
	}
	query.Set("limit", fmt.Sprintf("%d", limit))

	vergexProxyJSON(c, "/trending-crypto?"+query.Encode())
}

// handleTrendingHL proxies /trending-hl (Hyperliquid universe categories).
func (s *Server) handleTrendingHL(c *gin.Context) {
	category := c.Query("category")
	switch category {
	case "crypto", "stocks", "indices", "commodities", "fx":
	case "other":
		if c.Query("sub") != "preipo" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sub for category other, must be preipo"})
			return
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid category, must be one of crypto,stocks,indices,commodities,fx,other"})
		return
	}

	query := url.Values{}
	query.Set("category", category)
	if category == "other" {
		query.Set("sub", "preipo")
	}

	vergexProxyJSON(c, "/trending-hl?"+query.Encode())
}

// handleTrendingCategory proxies /trending-category (AI500 / prediction featured picks).
func (s *Server) handleTrendingCategory(c *gin.Context) {
	key := c.Query("key")
	switch key {
	case "ai500", "prediction":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid key, must be ai500 or prediction"})
		return
	}

	lang := c.DefaultQuery("lang", "en")
	if lang != "en" && lang != "zh" {
		lang = "en"
	}

	query := url.Values{}
	query.Set("lang", lang)
	query.Set("key", key)

	vergexProxyJSON(c, "/trending-category?"+query.Encode())
}

// handleTrendingFlow proxies vergex /api/v1/data-intelligence/flow/markets —
// the endpoint powering the official trending page's net inflow/outflow card.
// When vergex is unreachable (Cloudflare challenges the backend, and the API
// rejects cross-origin browser calls so no relay can feed it), fall back to a
// Binance OI-delta derived payload so the card still renders.
func (s *Server) handleTrendingFlow(c *gin.Context) {
	window := c.DefaultQuery("window", "1h")
	switch window {
	case "5m", "15m", "1h", "4h", "8h", "12h", "24h":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window, must be one of 5m,15m,1h,4h,8h,12h,24h"})
		return
	}

	limit := clampQueryInt(c, "limit", 25, 1, 25)

	query := url.Values{}
	query.Set("window", window)
	query.Set("limit", fmt.Sprintf("%d", limit))

	data, err := fetchVergexJSON("/api/v1/data-intelligence/flow/markets?" + query.Encode())
	if err != nil {
		logger.Warnf("⚠️ Vergex flow unavailable (%v), using Binance OI-delta fallback", err)
		data, err = flowFallbackJSON(window, limit)
		if err != nil {
			logger.Warnf("⚠️ Net-flow fallback failed: %v", err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "Trending data source unavailable"})
			return
		}
	}
	c.Data(http.StatusOK, "application/json", data)
}

func clampQueryInt(c *gin.Context, name string, def, min, max int) int {
	raw := c.Query(name)
	if raw == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// vergexRelayablePrefixes lists the upstream paths the browser may relay.
var vergexRelayablePrefixes = []string{
	"/trending-category",
	"/trending-crypto",
	"/trending-hl",
	"/api/v1/data-intelligence/",
}

// handleTrendingRelay caches a vergex payload fetched by the web UI's browser
// (POST /api/trending/relay?path=<urlencoded upstream path>, JSON body). The
// entry feeds the GET proxy endpoints below and the provider/vergex client
// used by strategy coin sources.
func (s *Server) handleTrendingRelay(c *gin.Context) {
	rawPath := c.Query("path")
	if rawPath == "" || !strings.HasPrefix(rawPath, "/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing or invalid path"})
		return
	}
	relayable := false
	for _, prefix := range vergexRelayablePrefixes {
		if strings.HasPrefix(rawPath, prefix) {
			relayable = true
			break
		}
	}
	if !relayable {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is not a relayable vergex endpoint"})
		return
	}

	// 2MB cap: vergex payloads are tens of KB; anything larger is abuse or a
	// memory DoS vector (auth'd caller or not).
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 2<<20))
	if err != nil || len(body) == 0 || !json.Valid(body) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be a non-empty JSON payload"})
		return
	}

	expires := time.Now().Add(vergexRelayTTL)
	vergexCacheMu.Lock()
	vergexCache[rawPath] = vergexCacheEntry{data: body, expires: expires, relay: true}
	if len(vergexCache) > 256 {
		now := time.Now()
		for k, e := range vergexCache {
			if now.After(e.expires) {
				delete(vergexCache, k)
			}
		}
	}
	vergexCacheMu.Unlock()

	vergex.FeedRelay(rawPath, body)

	c.Status(http.StatusNoContent)
}
