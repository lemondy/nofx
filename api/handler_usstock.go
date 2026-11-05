package api

import (
	"net/http"
	"time"

	"nofx/kernel/stockengine"
	"nofx/market/usstock"
	"nofx/store"

	"github.com/gin-gonic/gin"
)

// us_stock (design 2026-10-09 §7): bStock pair list for the strategy editor and
// the paper-ledger view.

// usstockListSymbols is the symbol source (test seam).
var usstockListSymbols = usstock.ListSymbols

// Price-cycle scan seams (test).
var (
	usstockCachedScan = stockengine.CachedScanReport
	usstockWarmScan   = stockengine.WarmScanReport
)

// handleUSStockScan returns the cached Kell price-cycle scan of all listed
// bStock pairs. Without a cached report it starts a background scan and
// answers {"scanning":true,...}; the page polls until a report exists.
func (s *Server) handleUSStockScan(c *gin.Context) {
	report := usstockCachedScan()
	if report == nil {
		usstockWarmScan(0)
		c.JSON(http.StatusOK, gin.H{"scanning": true, "generated_at": nil, "results": []stockengine.ScanResult{}, "skipped": map[string]string{}})
		return
	}
	results, skipped := report.Results, report.Skipped
	if results == nil {
		results = []stockengine.ScanResult{}
	}
	if skipped == nil {
		skipped = map[string]string{}
	}
	c.JSON(http.StatusOK, gin.H{"scanning": false, "generated_at": report.GeneratedAt.Format(time.RFC3339), "results": results, "skipped": skipped})
}

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
	marks := map[string]float64{}
	if at, err := s.traderManager.GetTrader(traderID); err == nil && at != nil {
		if at.IsStockStrategy() {
			if view, verr := at.StockPaperView(); verr == nil {
				c.JSON(http.StatusOK, view)
				return
			}
		} else if at.IsFactorQuantUSStrategy() {
			// The factor US reader marks shorts on perps and settles funding.
			// Read cash below afterwards so the ledger view includes settlement.
			if positions, perr := at.GetPositions(); perr == nil {
				for _, p := range positions {
					sym, _ := p["symbol"].(string)
					marks[sym], _ = p["mark_price"].(float64)
				}
			}
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
	type markedPosition struct {
		*store.StockPaperPosition
		Side          string  `json:"side"`
		MarkPrice     float64 `json:"mark_price"`
		MarketValue   float64 `json:"market_value"`
		UnrealizedPnL float64 `json:"unrealized_pnl"`
	}
	marked := make([]markedPosition, 0, len(open))
	for _, p := range open {
		mark := marks[p.Symbol]
		if mark <= 0 {
			mark = p.AvgPrice
		}
		value := p.MarketValue(mark)
		pnl := (mark - p.AvgPrice) * p.Quantity
		if p.EffectiveSide() == store.StockPaperShort {
			pnl = -pnl
		}
		equity += value
		marked = append(marked, markedPosition{p, p.EffectiveSide(), mark, value, pnl})
	}
	c.JSON(http.StatusOK, gin.H{"account": acct, "equity": equity, "open_positions": marked, "closed_positions": closed})
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
