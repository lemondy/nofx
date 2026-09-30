package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
	"sync"
	"time"
)

func requestLimits() gin.HandlerFunc {
	type bucket struct {
		count int
		reset time.Time
	}
	var mu sync.Mutex
	buckets := make(map[string]bucket)
	return func(c *gin.Context) {
		const maxBody = 2 << 20
		if c.Request.ContentLength > maxBody {
			c.AbortWithStatus(http.StatusRequestEntityTooLarge)
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBody)
		if !strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Next()
			return
		}
		now := time.Now()
		key, cap := c.ClientIP(), 600
		p := c.Request.URL.Path
		if p == "/api/klines" || p == "/api/symbols" || strings.HasPrefix(p, "/api/breakout") || strings.HasPrefix(p, "/api/trending/") || p == "/api/competition" || strings.HasPrefix(p, "/api/equity-history") {
			key += ":market"
			cap = 120
		}
		if p == "/api/login" || p == "/api/register" || p == "/api/reset-password" {
			key += ":auth"
			cap = 10
		}
		mu.Lock()
		if len(buckets) >= 10000 {
			for k, b := range buckets {
				if !now.Before(b.reset) {
					delete(buckets, k)
				}
			}
		}
		b, exists := buckets[key]
		if !exists && len(buckets) >= 10000 {
			mu.Unlock()
			c.AbortWithStatus(http.StatusTooManyRequests)
			return
		}
		if !now.Before(b.reset) {
			b = bucket{reset: now.Add(time.Minute)}
		}
		b.count++
		buckets[key] = b
		mu.Unlock()
		if b.count > cap {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests"})
			return
		}
		c.Next()
	}
}
