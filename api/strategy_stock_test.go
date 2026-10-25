package api

import (
	"strings"
	"testing"

	"nofx/store"
)

func usStockCfg(sc *store.StockConfig) *store.StrategyConfig {
	return &store.StrategyConfig{StrategyType: store.StrategyTypeUSStock, StockConfig: sc}
}

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestValidateStrategyConfig_USStock(t *testing.T) {
	// Paper default, regular only: no warnings, and no NofxOS key warning.
	cfg := usStockCfg(&store.StockConfig{Symbols: []string{"AAPLBUSDT"}})
	cfg.Indicators.EnableQuantData = true // crypto-only check must not fire
	if ws := validateStrategyConfig(cfg); len(ws) != 0 {
		t.Fatalf("paper default: unexpected warnings %v", ws)
	}

	// Live mode warns.
	live := usStockCfg(&store.StockConfig{Symbols: []string{"AAPLBUSDT"}, PaperTrading: boolPtr(false)})
	if ws := validateStrategyConfig(live); !hasWarning(ws, "LIVE") {
		t.Fatalf("live mode: want LIVE warning, got %v", ws)
	}

	// Extended sessions warn.
	ext := usStockCfg(&store.StockConfig{Symbols: []string{"AAPLBUSDT"}, Sessions: store.StockSessions{PreMarket: true}})
	if ws := validateStrategyConfig(ext); !hasWarning(ws, "Pre-market") {
		t.Fatalf("pre-market: want warning, got %v", ws)
	}

	// Divergence guard disabled warns.
	nodiv := usStockCfg(&store.StockConfig{Symbols: []string{"AAPLBUSDT"}, MaxDivergencePct: -1})
	if ws := validateStrategyConfig(nodiv); !hasWarning(ws, "Divergence") {
		t.Fatalf("divergence disabled: want warning, got %v", ws)
	}

	// Invalid config is surfaced (and does not panic) — missing stock_config
	// and empty symbols.
	if ws := validateStrategyConfig(usStockCfg(nil)); !hasWarning(ws, "invalid") {
		t.Fatalf("nil stock_config: want invalid warning, got %v", ws)
	}
	if ws := validateStrategyConfig(usStockCfg(&store.StockConfig{})); !hasWarning(ws, "invalid") {
		t.Fatalf("empty symbols: want invalid warning, got %v", ws)
	}

	// Hard rejection happens in StrategyConfig.Validate (used by create/update).
	if err := usStockCfg(nil).Validate(); err == nil {
		t.Fatalf("Validate must reject us_stock without stock_config")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate must accept a minimal us_stock config: %v", err)
	}
}
