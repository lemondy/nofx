package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// handleGetAICosts returns AI charges for a specific trader
func (s *Server) handleGetAICosts(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Query("trader_id")
	period := c.DefaultQuery("period", "today")

	if traderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trader_id is required"})
		return
	}
	// Ownership: the trader must belong to the caller (IDOR fix 2026-09-25).
	if _, err := s.store.Trader().GetForUser(userID, traderID); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Trader not found for this user"})
		return
	}

	charges, total, err := s.store.AICharge().GetCharges(traderID, period)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"charges": charges,
		"total":   total,
		"count":   len(charges),
	})
}

// handleGetAICostsSummary returns AI cost summary across all traders
func (s *Server) handleGetAICostsSummary(c *gin.Context) {
	userID := c.GetString("user_id")
	period := c.DefaultQuery("period", "today")

	// Scope to the caller's OWN traders (IDOR fix 2026-09-25): the global
	// summary aggregated every user's AI spend.
	traders, err := s.store.Trader().List(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list traders"})
		return
	}
	total, count := 0.0, int64(0)
	byModel := map[string]float64{}
	for _, t := range traders {
		tTotal, tCount, tByModel := s.store.AICharge().GetSummaryForTrader(t.ID, period)
		total += tTotal
		count += tCount
		for m, v := range tByModel {
			byModel[m] += v
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"total":    total,
		"count":    count,
		"by_model": byModel,
	})
}
