package stockengine

import (
	"nofx/market/usstock"
	"nofx/store"
)

// TODO(integration): replace with store resolvers.
// These local resolvers intentionally preserve zero-value defaults and nil booleans.
func effectivePreset(cfg *store.StockConfig) string {
	if cfg == nil || cfg.Preset == "" {
		return store.StockPresetSwing
	}
	return cfg.Preset
}
func allowRegular(cfg *store.StockConfig) bool {
	return cfg == nil || cfg.Sessions.Regular == nil || *cfg.Sessions.Regular
}
func allowPreMarket(cfg *store.StockConfig) bool  { return cfg != nil && cfg.Sessions.PreMarket }
func allowAfterHours(cfg *store.StockConfig) bool { return cfg != nil && cfg.Sessions.AfterHours }
func yahooFallback(cfg *store.StockConfig) bool {
	return cfg == nil || cfg.DataFallbackYahoo == nil || *cfg.DataFallbackYahoo
}
func isPaper(cfg *store.StockConfig) bool {
	return cfg == nil || cfg.PaperTrading == nil || *cfg.PaperTrading
}
func defaultFloat(v, fallback float64) float64 {
	if v == 0 {
		return fallback
	}
	return v
}
func effectiveMaxDivergencePct(cfg *store.StockConfig) float64 {
	if cfg == nil {
		return 1
	}
	return defaultFloat(cfg.MaxDivergencePct, 1)
}
func effectiveMaxPositionPct(cfg *store.StockConfig) float64 {
	if cfg == nil {
		return 20
	}
	return defaultFloat(cfg.MaxPositionPct, 20)
}
func effectiveMaxTotalExposurePct(cfg *store.StockConfig) float64 {
	if cfg == nil {
		return 80
	}
	return defaultFloat(cfg.MaxTotalExposurePct, 80)
}
func effectiveMaxPositions(cfg *store.StockConfig) int {
	if cfg == nil || cfg.MaxPositions == 0 {
		return 5
	}
	return cfg.MaxPositions
}
func effectiveRiskPerTradePct(cfg *store.StockConfig) float64 {
	if cfg == nil {
		return 1
	}
	return defaultFloat(cfg.RiskPerTradePct, 1)
}
func effectiveStopATRBand(cfg *store.StockConfig) (float64, float64) {
	lo, hi := 1.5, 3.0
	if effectivePreset(cfg) == store.StockPresetPosition {
		lo, hi = 2, 4
	}
	if cfg != nil {
		lo = defaultFloat(cfg.StopATRMin, lo)
		hi = defaultFloat(cfg.StopATRMax, hi)
	}
	return lo, hi
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
