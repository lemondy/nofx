package api

import (
	"net/http"

	"nofx/market/usstock"
	"nofx/store"

	"github.com/gin-gonic/gin"
)

// us_stock (design 2026-10-09 §7): bStock pair list for the strategy editor and
// the paper-ledger view.

// usstockListSymbols is the symbol source (test seam).
var usstockListSymbols = usstock.ListSymbols

// handleUSStockSymbols returns the tradable bStock pairs.
func (s *Server) handleUSStockSymbols(c *gin.Context) {
	infos, err := usstockListSymbols(c.Request.Context())
	if err != nil {
		SafeInternalError(c, "List US stock symbols", err)
		return
	}
	out := make([]gin.H, 0, len(infos))
	for _, in := range infos {
		out = append(out, gin.H{"symbol": in.Symbol, "underlying": in.Underlying, "base_asset": in.BaseAsset})
	}
	c.JSON(http.StatusOK, gin.H{"symbols": out})
}

// handleUSStockPaper returns the paper account and its open / closed positions.
func (s *Server) handleUSStockPaper(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("trader_id")
	if _, err := s.store.Trader().GetForUser(userID, traderID); err != nil {
		SafeNotFound(c, "Trader")
		return
	}
	if at, err := s.traderManager.GetTrader(traderID); err == nil && at != nil && at.IsStockStrategy() {
		if view, verr := at.StockPaperView(); verr == nil {
			c.JSON(http.StatusOK, view)
			return
		}
	}
	// Trader not loaded in memory: serve the ledger straight from the store,
	// valuing open positions at average cost.
	acct, err := s.store.StockPaper().GetAccount(traderID)
	if err != nil {
		SafeInternalError(c, "Get paper account", err)
		return
	}
	open, err := s.store.StockPaper().ListOpen(traderID)
	if err != nil {
		SafeInternalError(c, "List paper positions", err)
		return
	}
	closed, err := s.store.StockPaper().ListClosed(traderID, 50)
	if err != nil {
		SafeInternalError(c, "List paper positions", err)
		return
	}
	equity := 0.0
	if acct != nil {
		equity = acct.Cash
	}
	for _, p := range open {
		equity += p.Quantity * p.AvgPrice
	}
	c.JSON(http.StatusOK, gin.H{"account": acct, "equity": equity, "open_positions": open, "closed_positions": closed})
}

// strategyIsUSStock reports whether the strategy row holds a us_stock config.
func (s *Server) strategyIsUSStock(userID, strategyID string) bool {
	if strategyID == "" {
		return false
	}
	strat, err := s.store.Strategy().Get(userID, strategyID)
	if err != nil || strat == nil {
		return false
	}
	cfg, err := strat.ParseConfig()
	return err == nil && cfg != nil && cfg.StrategyType == store.StrategyTypeUSStock
}
