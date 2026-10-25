package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"nofx/netmeter"
)

// handleNetTraffic serves the process-lifetime network usage monitor shown on
// the settings page: cumulative HTTP payload bytes per host (request +
// response bodies; header/TLS overhead excluded) plus wire bytes for raw
// websocket feed connections. In-memory only — resets on restart.
// GET /api/net-traffic
func (s *Server) handleNetTraffic(c *gin.Context) {
	if c.GetString("user_id") == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	totals, hosts := netmeter.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"since":       totals.Since.UTC().Format("2006-01-02T15:04:05Z"),
		"since_unix":  totals.Since.Unix(),
		"requests":    totals.Requests,
		"http_up":     totals.HTTPUp,
		"http_down":   totals.HTTPDown,
		"ws_up":       totals.WsUp,
		"ws_down":     totals.WsDown,
		"hosts":       hosts,
	})
}
