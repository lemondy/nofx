package stockengine

import (
	"nofx/market/usstock"
	"nofx/store"
)

// Config knobs resolve through the store's StockConfig resolvers (one
// definition shared with validation and the UI defaults); these thin wrappers
// keep the engine's call sites short.
func effectivePreset(cfg *store.StockConfig) string { return cfg.EffectivePreset() }
func allowRegular(cfg *store.StockConfig) bool      { return cfg.AllowRegular() }
func allowPreMarket(cfg *store.StockConfig) bool    { return cfg.AllowPreMarket() }
func allowAfterHours(cfg *store.StockConfig) bool   { return cfg.AllowAfterHours() }
func yahooFallback(cfg *store.StockConfig) bool     { return cfg.YahooFallback() }
func isPaper(cfg *store.StockConfig) bool           { return cfg.IsPaper() }
func effectiveMaxDivergencePct(cfg *store.StockConfig) float64 {
	return cfg.EffectiveMaxDivergencePct()
}
func effectiveMaxPositionPct(cfg *store.StockConfig) float64 { return cfg.EffectiveMaxPositionPct() }
func effectiveMaxTotalExposurePct(cfg *store.StockConfig) float64 {
	return cfg.EffectiveMaxTotalExposurePct()
}
func effectiveMaxPositions(cfg *store.StockConfig) int        { return cfg.EffectiveMaxPositions() }
func effectiveRiskPerTradePct(cfg *store.StockConfig) float64 { return cfg.EffectiveRiskPerTradePct() }
func effectiveStopATRBand(cfg *store.StockConfig) (float64, float64) {
	return cfg.EffectiveStopATRBand()
}

// ResolvePreset resolves the style, ET decision rhythm and ATR stop band.
func ResolvePreset(cfg *store.StockConfig) Preset {
	p := Preset{Name: effectivePreset(cfg), TrendTF: usstock.TF1d, EntryTF: usstock.TF1h,
		DecisionTimes: []string{"09:45", "12:30", "15:30"}, MaxHoldDays: 30}
	if p.Name == store.StockPresetPosition {
		p.TrendTF, p.EntryTF = usstock.TF1w, usstock.TF1d
		p.DecisionTimes, p.MaxHoldDays = []string{"15:30"}, 0
	}
	p.StopATRMin, p.StopATRMax = effectiveStopATRBand(cfg)
	return p
}

func sessionAllowed(cfg *store.StockConfig, session usstock.Session) bool {
	switch session {
	case usstock.SessionRegular:
		return allowRegular(cfg)
	case usstock.SessionPre:
		return allowPreMarket(cfg)
	case usstock.SessionAfter:
		return allowAfterHours(cfg)
	default:
		return false
	}
}

func contextPreset(ctx *Context) Preset {
	if ctx.Preset.Name == "" {
		return ResolvePreset(ctx.Config)
	}
	return ctx.Preset
}
