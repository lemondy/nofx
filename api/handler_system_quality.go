package api

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// systemQualityFailure is one classified failure bucket.
type systemQualityFailure struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
	Sample   string `json:"sample"`
}

// systemQualityHour is one hourly bucket for the trend table.
type systemQualityHour struct {
	Hour        string  `json:"hour"` // "01-02 15:00" local
	Cycles      int     `json:"cycles"`
	Failures    int     `json:"failures"`
	AvgDuration float64 `json:"avg_duration_ms"` // 0 when no AI call in the bucket
}

// classifyFailure buckets an error message into a coarse category the
// quality page can chart. Keyword-based on purpose: error messages are free
// text assembled across the stack.
func classifyFailure(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case containsAny(m, "ip", "permission", "-2015", "api-key", "api key", "auth", "whitelist"):
		return "AUTH"
	case containsAny(m, "binance", "apierror", "exchange", "-40", "-100", "-201", "-4"):
		return "EXCHANGE"
	case containsAny(m, "timeout", "timed out", "deadline", "connection reset", "eof", "refused", "context canceled"):
		return "NETWORK"
	case containsAny(m, "model", "llm", "ai ", "completion", "token", "rate limit", "429", "500", "503", "overloaded"):
		return "AI_CALL"
	case containsAny(m, "failed to build trading context", "market data", "candidate"):
		return "DATA"
	default:
		return "OTHER"
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// handleSystemQuality serves the system-quality aggregation for one trader:
// per-cycle AI-call duration stats, cycle success rate, failure breakdown by
// category, and hourly buckets — the "is the system itself healthy" view as
// opposed to trading performance. GET /api/system-quality?trader_id=&hours=24
func (s *Server) handleSystemQuality(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	traderID := c.Query("trader_id")
	if traderID == "" {
		SafeBadRequest(c, "trader_id is required")
		return
	}
	// Ownership: the trader must belong to the caller.
	traders, err := s.store.Trader().ListAll()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list traders"})
		return
	}
	owned := false
	for _, t := range traders {
		if t.ID == traderID && t.UserID == userID {
			owned = true
			break
		}
	}
	if !owned {
		c.JSON(http.StatusForbidden, gin.H{"error": "Trader not found for this user"})
		return
	}
	hours := 24
	if h := c.Query("hours"); h != "" {
		if parsed, err := time.ParseDuration(h + "h"); err == nil && parsed > 0 {
			hours = int(parsed.Hours())
		}
	}
	if hours > 24*7 {
		hours = 24 * 7 // cap at one week
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)

	rows, err := s.store.Decision().GetQualityRecordsSince(traderID, since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate"})
		return
	}

	// ── Aggregates ──
	total := len(rows)
	successful := 0
	var durations []int64
	failures := map[string]*systemQualityFailure{}
	hourlyMap := map[string]*systemQualityHour{}
	bucketAICnt := map[string]int{}
	for _, r := range rows {
		if r.Success {
			successful++
		} else {
			cat := classifyFailure(r.ErrorMessage)
			f, ok := failures[cat]
			if !ok {
				f = &systemQualityFailure{Category: cat}
				failures[cat] = f
			}
			f.Count++
			if f.Sample == "" && r.ErrorMessage != "" {
				f.Sample = r.ErrorMessage
			}
		}
		if r.AIRequestDurationMs > 0 {
			durations = append(durations, r.AIRequestDurationMs)
		}
		hourKey := r.Timestamp.Local().Format("01-02 15:00")
		hb, ok := hourlyMap[hourKey]
		if !ok {
			hb = &systemQualityHour{Hour: hourKey}
			hourlyMap[hourKey] = hb
		}
		hb.Cycles++
		if !r.Success {
			hb.Failures++
		}
		if r.AIRequestDurationMs > 0 {
			hb.AvgDuration += float64(r.AIRequestDurationMs)
			bucketAICnt[hourKey]++
		}
	}
	hourly := make([]systemQualityHour, 0, len(hourlyMap))
	for _, hb := range hourlyMap {
		if n := bucketAICnt[hb.Hour]; n > 0 {
			hb.AvgDuration /= float64(n)
		} else {
			hb.AvgDuration = 0
		}
		hourly = append(hourly, *hb)
	}
	sort.Slice(hourly, func(i, j int) bool { return hourly[i].Hour < hourly[j].Hour })

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	aiStats := gin.H{"count": len(durations)}
	if n := len(durations); n > 0 {
		sum := int64(0)
		for _, d := range durations {
			sum += d
		}
		pct := func(p float64) int64 {
			idx := int(math.Ceil(p/100*float64(n))) - 1
			if idx < 0 {
				idx = 0
			}
			if idx >= n {
				idx = n - 1
			}
			return durations[idx]
		}
		aiStats = gin.H{
			"count":  n,
			"avg_ms": sum / int64(n),
			"p50_ms": pct(50),
			"p95_ms": pct(95),
			"max_ms": durations[n-1],
			"min_ms": durations[0],
		}
	}

	failureList := make([]systemQualityFailure, 0, len(failures))
	for _, f := range failures {
		failureList = append(failureList, *f)
	}
	sort.Slice(failureList, func(i, j int) bool { return failureList[i].Count > failureList[j].Count })

	successRate := 0.0
	if total > 0 {
		successRate = float64(successful) / float64(total) * 100
	}

	c.JSON(http.StatusOK, gin.H{
		"trader_id":         traderID,
		"window_hours":      hours,
		"total_cycles":      total,
		"successful_cycles": successful,
		"failed_cycles":     total - successful,
		"success_rate_pct":  math.Round(successRate*10) / 10,
		"ai_calls":          aiStats,
		"failures":          failureList,
		"hourly":            hourly,
	})
}
