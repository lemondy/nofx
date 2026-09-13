package api

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"nofx/market/breakout"

	"github.com/gin-gonic/gin"
)

// ── Result cache: symbols are analyzed on demand, shared across requests ──

type breakoutCacheEntry struct {
	report  *breakout.Report
	err     error
	expires time.Time
	fetched time.Time
}

var (
	breakoutCache   = map[string]*breakoutCacheEntry{}
	breakoutCacheMu sync.RWMutex
	scanCache       *struct {
		results []breakout.ScanResult
		expires time.Time
	}
	breakoutInflight sync.Map // symbol -> *sync.WaitGroup-ish (singleflight-lite)
)

const (
	breakoutCacheTTL  = 60 * time.Second
	breakoutCacheBusy = 5 * time.Second // stale-while-error window
	scanCacheTTL      = 2 * time.Minute
)

// getBreakoutReport returns a cached or freshly computed report.
func getBreakoutReport(symbol string) (*breakout.Report, error) {
	breakoutCacheMu.RLock()
	if e, ok := breakoutCache[symbol]; ok && time.Now().Before(e.expires) {
		r, err := e.report, e.err
		breakoutCacheMu.RUnlock()
		return r, err
	}
	breakoutCacheMu.RUnlock()

	breakoutCacheMu.Lock()
	// Re-check under write lock (another request may have just filled it).
	if e, ok := breakoutCache[symbol]; ok && time.Now().Before(e.expires) {
		breakoutCacheMu.Unlock()
		return e.report, e.err
	}
	breakoutCacheMu.Unlock()

	report, err := breakout.Analyze(symbol, breakout.NewBinanceDS(symbol))

	breakoutCacheMu.Lock()
	breakoutCache[symbol] = &breakoutCacheEntry{
		report: report, err: err,
		fetched: time.Now(),
		// On error, retry after a short window; success lives for the full TTL.
		expires: time.Now().Add(breakoutCacheTTL),
	}
	breakoutCacheMu.Unlock()
	return report, err
}

// handleBreakout GET /api/breakout?symbol=BTCUSDT
func (s *Server) handleBreakout(c *gin.Context) {
	symbol := strings.ToUpper(strings.TrimSpace(c.Query("symbol")))
	if symbol == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "symbol parameter is required"})
		return
	}

	report, err := getBreakoutReport(symbol)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "breakout analysis failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, report)
}

// handleBreakoutScan GET /api/breakout/scan?limit=10&concurrency=4
func (s *Server) handleBreakoutScan(c *gin.Context) {
	limit := 10
	if v, err := strconv.Atoi(c.DefaultQuery("limit", "10")); err == nil && v > 0 && v <= 30 {
		limit = v
	}
	conc := 4
	if v, err := strconv.Atoi(c.DefaultQuery("concurrency", "4")); err == nil && v > 0 && v <= 8 {
		conc = v
	}

	breakoutCacheMu.RLock()
	if scanCache != nil && time.Now().Before(scanCache.expires) {
		res := scanCache.results
		breakoutCacheMu.RUnlock()
		c.JSON(http.StatusOK, gin.H{
			"generated_at": time.Now().UTC(),
			"count":        len(res),
			"results":      res,
		})
		return
	}
	breakoutCacheMu.RUnlock()

	symbols, err := breakout.TopVolumeSymbols(30)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to list symbols: " + err.Error()})
		return
	}
	if len(symbols) > limit {
		symbols = symbols[:limit]
	}

	results := breakout.AnalyzeMany(symbols, conc)

	breakoutCacheMu.Lock()
	scanCache = &struct {
		results []breakout.ScanResult
		expires time.Time
	}{results: results, expires: time.Now().Add(scanCacheTTL)}
	breakoutCacheMu.Unlock()

	c.JSON(http.StatusOK, gin.H{
		"generated_at": time.Now().UTC(),
		"count":        len(results),
		"results":      results,
	})
}

// handleBreakoutSnapshot GET /api/breakout/snapshot
// Returns the 5-minute background scan (all grades) for the Data page ranking.
func (s *Server) handleBreakoutSnapshot(c *gin.Context) {
	results, updatedAt := breakout.DefaultScheduler().Snapshot()
	if len(results) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"generated_at": time.Now().UTC(),
			"count":        0,
			"results":      []breakout.ScanResult{},
			"warming_up":   true,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"generated_at": updatedAt.UTC(),
		"count":        len(results),
		"results":      results,
	})
}

// handleBreakoutShortScan GET /api/breakout/short-scan?limit=25
// Ranks the top 24h gainers among Binance USDT-M perps by short-suitability
// (RSI exhaustion, EMA extension, wick rejection, volume fade, funding/OI
// crowding). Serves the scheduler snapshot when fresh, else computes live.
func (s *Server) handleBreakoutShortScan(c *gin.Context) {
	limit := 25
	if v, err := strconv.Atoi(c.DefaultQuery("limit", "25")); err == nil && v > 0 && v <= 50 {
		limit = v
	}

	results, updatedAt := breakout.DefaultScheduler().ShortSnapshot()
	if len(results) < limit {
		// Cold snapshot (or fewer candidates than requested) — compute live;
		// ScanShorts shares the scheduler's 2-minute cache so this is cheap.
		if live, at, err := breakout.ScanShorts(50); err == nil {
			results, updatedAt = live, at
		}
	}
	if len(results) > limit {
		results = results[:limit]
	}
	if len(results) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "short scan warming up, retry shortly"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"generated_at": updatedAt.UTC(),
		"count":        len(results),
		"results":      results,
	})
}

// handleBreakoutParams GET /api/breakout/params
// Transparency endpoint: current tunable parameters + last backtest summary.
func (s *Server) handleBreakoutParams(c *gin.Context) {
	params := breakout.GetParams()
	resp := gin.H{"params": params}
	if summary, err := breakout.LoadBacktestSummary(); err == nil {
		resp["backtest"] = summary
	}
	c.JSON(http.StatusOK, resp)
}
