package trader

import (
	"math"
	"nofx/logger"
	"nofx/market"
)

func (at *AutoTrader) cachedOIMillions(symbol string) (float64, bool) {
	if at.oiValueFn != nil {
		return at.oiValueFn(symbol)
	}
	return market.CachedOpenInterestMillions(symbol)
}
func (at *AutoTrader) discountThinOISize(symbol string, size float64) float64 {
	if size <= 0 || at.config.StrategyConfig == nil || thinRiskMajor(symbol) || market.IsBStockSymbol(symbol) {
		return size
	}
	cfg := at.config.StrategyConfig
	threshold := cfg.RiskControl.EffectiveThinOIFullSizeMillions(cfg.CoinSource.EffectiveMinOIMillions())
	floor := cfg.RiskControl.EffectiveThinOIMinSizeFactor()
	if threshold <= 0 || floor <= 0 {
		return size
	}
	oi, ok := at.cachedOIMillions(symbol)
	if !ok || oi < 0 || math.IsNaN(oi) || math.IsInf(oi, 0) {
		logger.Infof("[RISK CONTROL] %s cached OI unknown — position discount skipped", symbol)
		return size
	}
	factor := math.Min(1, math.Max(floor, oi/threshold))
	if factor < 1 {
		logger.Infof("[RISK CONTROL] %s OI %.2fM < %.2fM — size %.2f × %.3f", symbol, oi, threshold, size, factor)
	}
	return size * factor
}
func (at *AutoTrader) thinForSpread(symbol string) bool {
	if market.IsBStockSymbol(symbol) {
		return false
	}
	cfg := at.config.StrategyConfig
	oi, ok := at.cachedOIMillions(symbol)
	if ok && oi >= 0 && !math.IsNaN(oi) && !math.IsInf(oi, 0) {
		threshold := cfg.RiskControl.EffectiveThinOIFullSizeMillions(cfg.CoinSource.EffectiveMinOIMillions())
		return threshold > 0 && oi < threshold
	}
	// Only BTC/ETH have an existing explicit major-coin classification. Do
	// not infer that a pumped, high-volume altcoin is a liquid major.
	return !thinRiskMajor(symbol)
}

// Exact symbols: ETHFI/BTCDOM are altcoins, not BTC/ETH exemptions.
func thinRiskMajor(symbol string) bool {
	s := market.Normalize(symbol)
	return s == "BTCUSDT" || s == "ETHUSDT"
}
