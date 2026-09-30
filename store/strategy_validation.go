package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Validate rejects unsafe input before persistence and again before execution.
// Zero retains documented defaults; negative opt-out knobs retain their semantics.
func (c *StrategyConfig) Validate() error {
	if c == nil {
		return fmt.Errorf("strategy is required")
	}
	if _, err := json.Marshal(c); err != nil {
		return fmt.Errorf("strategy contains non-finite values: %w", err)
	}
	if c.StrategyType != "" && c.StrategyType != "ai_trading" && c.StrategyType != "grid_trading" {
		return fmt.Errorf("invalid strategy_type")
	}
	r := c.RiskControl
	if r.MaxPositions < 0 || r.MaxPositions > MaxPositions {
		return fmt.Errorf("max_positions must be between 0 and %d", MaxPositions)
	}
	if r.BTCETHMaxLeverage < 0 || r.BTCETHMaxLeverage > 125 || r.AltcoinMaxLeverage < 0 || r.AltcoinMaxLeverage > 125 {
		return fmt.Errorf("leverage must be between 0 and 125")
	}
	if r.MaxMarginUsage < 0 || r.MaxMarginUsage > 1 {
		return fmt.Errorf("max_margin_usage must be between 0 and 1")
	}
	if r.BTCETHMaxPositionValueRatio < 0 || r.BTCETHMaxPositionValueRatio > 125 || r.AltcoinMaxPositionValueRatio < 0 || r.AltcoinMaxPositionValueRatio > 125 {
		return fmt.Errorf("invalid position value ratio")
	}
	if r.MinPositionSize < 0 || r.MinRiskRewardRatio < 0 || r.MinConfidence < 0 || r.MinConfidence > 100 || r.MinHoldMinutes < 0 || r.TPCloseFraction > 1 {
		return fmt.Errorf("invalid risk control threshold")
	}
	k := c.Indicators.Klines
	if k.PrimaryCount < 0 || k.PrimaryCount > MaxKlineCount || k.LongerCount < 0 || k.LongerCount > MaxKlineCount || len(k.SelectedTimeframes) > MaxTimeframes {
		return fmt.Errorf("kline configuration exceeds limits")
	}
	validTF := map[string]bool{"1m": true, "3m": true, "5m": true, "15m": true, "30m": true, "1h": true, "2h": true, "4h": true, "6h": true, "8h": true, "12h": true, "1d": true, "3d": true, "1w": true, "1M": true}
	for _, tf := range append(append([]string{}, k.SelectedTimeframes...), k.PrimaryTimeframe, k.LongerTimeframe) {
		if tf != "" && !validTF[tf] {
			return fmt.Errorf("unsupported timeframe: %s", tf)
		}
	}
	if c.StrategyType == "grid_trading" {
		return c.GridConfig.Validate()
	}
	return nil
}

func (g *GridStrategyConfig) Validate() error {
	if g == nil {
		return fmt.Errorf("grid configuration is required")
	}
	if _, err := json.Marshal(g); err != nil {
		return err
	}
	if strings.TrimSpace(g.Symbol) == "" || g.GridCount < 5 || g.GridCount > 50 {
		return fmt.Errorf("grid symbol is required and grid_count must be 5..50")
	}
	if g.TotalInvestment <= 0 || g.Leverage < 1 || g.Leverage > 20 {
		return fmt.Errorf("grid investment must be positive and leverage must be 1..20")
	}
	if !g.UseATRBounds && (g.LowerPrice <= 0 || g.UpperPrice <= g.LowerPrice) {
		return fmt.Errorf("grid requires 0 < lower_price < upper_price")
	}
	if g.MaxDrawdownPct < 0 || g.MaxDrawdownPct > 100 || g.StopLossPct < 0 || g.StopLossPct > 100 || g.DailyLossLimitPct < 0 || g.DailyLossLimitPct > 100 || g.DirectionBiasRatio < 0 || g.DirectionBiasRatio > 1 {
		return fmt.Errorf("invalid grid risk threshold")
	}
	if g.Distribution != "" && g.Distribution != "uniform" && g.Distribution != "gaussian" && g.Distribution != "pyramid" {
		return fmt.Errorf("invalid grid distribution")
	}
	return nil
}
