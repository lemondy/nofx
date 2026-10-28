package binance

import (
	"context"
	"fmt"
	"math"
	"nofx/logger"
	"strconv"
	"strings"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

// GetPositions gets all positions (with cache)
func (t *FuturesTrader) GetPositions() ([]map[string]interface{}, error) {
	// First check if cache is valid
	t.positionsCacheMutex.RLock()
	if t.cachedPositions != nil && time.Since(t.positionsCacheTime) < t.cacheDuration {
		cacheAge := time.Since(t.positionsCacheTime)
		// review 2026-10-07 B1-4: capture the snapshot before invalidation can clear it.
		positions := t.cachedPositions
		t.positionsCacheMutex.RUnlock()
		logger.Infof("✓ Using cached position information (cache age: %.1f seconds ago)", cacheAge.Seconds())
		return positions, nil
	}
	t.positionsCacheMutex.RUnlock()

	// review 2026-10-07 B1-4: serialize cache misses with invalidation and
	// recheck after acquiring the write lock. An in-flight fetch must finish
	// publishing before invalidation, rather than repopulate a cleared cache.
	t.positionsCacheMutex.Lock()
	defer t.positionsCacheMutex.Unlock()
	if t.cachedPositions != nil && time.Since(t.positionsCacheTime) < t.cacheDuration {
		return t.cachedPositions, nil
	}

	// Cache expired or doesn't exist, call API
	logger.Infof("🔄 Cache expired, calling Binance API to get position information...")
	positions, err := t.fetchPositions()
	if err != nil {
		err = fmt.Errorf("failed to get positions: %w", WrapAuthError("GetPositions", err))
		if positions == nil {
			return nil, err
		}
	}

	var result []map[string]interface{}
	for _, pos := range positions {
		posAmt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
		if posAmt == 0 {
			continue // Skip positions with zero amount
		}

		posMap := make(map[string]interface{})
		posMap["symbol"] = pos.Symbol
		posMap["positionAmt"], _ = strconv.ParseFloat(pos.PositionAmt, 64)
		posMap["entryPrice"], _ = strconv.ParseFloat(pos.EntryPrice, 64)
		posMap["markPrice"], _ = strconv.ParseFloat(pos.MarkPrice, 64)
		posMap["unRealizedProfit"], _ = strconv.ParseFloat(pos.UnRealizedProfit, 64)
		posMap["leverage"], _ = strconv.ParseFloat(pos.Leverage, 64)
		posMap["liquidationPrice"], _ = strconv.ParseFloat(pos.LiquidationPrice, 64)
		// Note: Binance SDK doesn't expose updateTime field, will fallback to local tracking

		// Determine direction
		if posAmt > 0 {
			posMap["side"] = "long"
		} else {
			posMap["side"] = "short"
		}

		result = append(result, posMap)
	}

	// Keep a successful v2 fallback available while propagating v3 auth/IP
	// errors. Do not cache away the error that the auth probe must observe.
	if err != nil {
		return result, err
	}

	// Update cache
	t.cachedPositions = result
	t.positionsCacheTime = time.Now()

	return result, nil
}

// SetMarginMode sets margin mode
func (t *FuturesTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
	// Every order carries LONG or SHORT positionSide. A failed constructor-time
	// mode switch must not leave the executor trading in one-way mode.
	if err := t.setDualSidePosition(); err != nil {
		return fmt.Errorf("cannot confirm Binance Hedge Mode for %s: %w", symbol, err)
	}
	mode, err := t.client.NewGetPositionModeService().Do(context.Background())
	if err != nil {
		return fmt.Errorf("cannot verify Binance Hedge Mode for %s: %w", symbol, err)
	}
	if mode == nil || !mode.DualSidePosition {
		return fmt.Errorf("cannot open %s: Binance account is not in Hedge Mode", symbol)
	}
	var marginType futures.MarginType
	if isCrossMargin {
		marginType = futures.MarginTypeCrossed
	} else {
		marginType = futures.MarginTypeIsolated
	}

	// Try to set margin mode
	err = t.client.NewChangeMarginTypeService().
		Symbol(symbol).
		MarginType(marginType).
		Do(context.Background())

	if err == nil {
		t.invalidateSymbolConfig(symbol)
	}

	marginModeStr := "Cross Margin"
	if !isCrossMargin {
		marginModeStr = "Isolated Margin"
	}

	if err != nil && !contains(err.Error(), "No need to change margin type") &&
		!contains(err.Error(), "Margin type cannot be changed if there exists position") {
		return fmt.Errorf("cannot set %s margin mode for %s: %w", marginModeStr, symbol, err)
	}
	// An existing position can prevent changing the contract's mode. Read the
	// actual symbol configuration rather than treating that error as success.
	configs, err := t.client.NewGetSymbolConfigService().Symbol(symbol).Do(context.Background())
	if err != nil {
		return fmt.Errorf("cannot verify %s margin mode for %s: %w", marginModeStr, symbol, err)
	}
	for _, config := range configs {
		if config != nil && config.Symbol == symbol {
			if !strings.EqualFold(config.MarginType, string(marginType)) {
				return fmt.Errorf("%s margin mode is %s, expected %s", symbol, config.MarginType, marginType)
			}
			t.invalidateSymbolConfig(symbol)
			logger.Infof("  ✓ %s margin mode verified as %s", symbol, marginModeStr)
			return nil
		}
	}
	return fmt.Errorf("cannot verify %s margin mode for %s: symbol configuration missing", marginModeStr, symbol)
}

// SetLeverage sets leverage (with smart detection and cooldown period)
func (t *FuturesTrader) SetLeverage(symbol string, leverage int) error {
	// First try to get current leverage (from position information)
	currentLeverage := 0
	positions, err := t.GetPositions()
	if err == nil {
		for _, pos := range positions {
			if pos["symbol"] == symbol {
				if lev, ok := pos["leverage"].(float64); ok {
					currentLeverage = int(lev)
					break
				}
			}
		}
	}

	// If current leverage is already the target leverage, skip
	if currentLeverage == leverage && currentLeverage > 0 {
		logger.Infof("  ✓ %s leverage is already %dx, no need to change", symbol, leverage)
		return nil
	}

	// Change leverage
	_, err = t.client.NewChangeLeverageService().
		Symbol(symbol).
		Leverage(leverage).
		Do(context.Background())

	if err != nil {
		// If error message contains "No need to change", leverage is already the target value
		if contains(err.Error(), "No need to change") {
			t.invalidateSymbolConfig(symbol)
			logger.Infof("  ✓ %s leverage is already %dx", symbol, leverage)
			return nil
		}
		return fmt.Errorf("failed to set leverage: %w", err)
	}

	t.invalidateSymbolConfig(symbol)
	logger.Infof("  ✓ %s leverage changed to %dx", symbol, leverage)

	// Wait 5 seconds after changing leverage (to avoid cooldown period errors)
	logger.Infof("  ⏱ Waiting 5 seconds for cooldown period...")
	time.Sleep(5 * time.Second)

	return nil
}

// GetMarketPrice gets market price
func (t *FuturesTrader) GetMarketPrice(symbol string) (float64, error) {
	prices, err := t.client.NewListPricesService().Symbol(symbol).Do(context.Background())
	if err != nil {
		return 0, fmt.Errorf("failed to get price: %w", err)
	}

	if len(prices) == 0 {
		return 0, fmt.Errorf("price not found")
	}

	price, err := strconv.ParseFloat(prices[0].Price, 64)
	if err != nil {
		return 0, err
	}

	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, fmt.Errorf("invalid %s market price %g", symbol, price)
	}
	// Reuse normal execution/cycle quote reads; stop preflight never polls REST.
	t.observeStopPrice(symbol, price, false)
	return price, nil
}

// CalculatePositionSize calculates position size
func (t *FuturesTrader) CalculatePositionSize(balance, riskPercent, price float64, leverage int) float64 {
	riskAmount := balance * (riskPercent / 100.0)
	positionValue := riskAmount * float64(leverage)
	quantity := positionValue / price
	return quantity
}

// GetMinNotional shares the exchange-rule snapshot used by LIMIT orders.
func (t *FuturesTrader) GetMinNotional(symbol string) float64 {
	s, err := t.cachedSymbolRules(symbol)
	if err != nil {
		return t.fallbackMinNotional(symbol, err)
	}
	return t.symbolMinNotional(s)
}

// CheckMinNotional applies the exchange floor after market quantity rounding.
// Reductions deliberately do not call this opening-risk check.
func (t *FuturesTrader) CheckMinNotional(symbol string, quantity float64) error {
	price, err := t.GetMarketPrice(symbol)
	if err != nil {
		return fmt.Errorf("failed to get market price: %w", err)
	}
	return validateOrderNotional(symbol, quantity, price, t.GetMinNotional(symbol))
}

// GetSymbolPrecision gets the quantity precision for a trading pair
func (t *FuturesTrader) GetSymbolPrecision(symbol string) (int, error) {
	rules, err := t.getMarketOrderRules(symbol)
	if err != nil {
		return 0, err
	}
	precision := calculatePrecision(rules.StepSizeText)
	logger.Infof("  %s quantity precision: %d (stepSize: %s)", symbol, precision, rules.StepSizeText)
	return precision, nil
}

// FormatQuantity formats quantity to correct precision
func (t *FuturesTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	rules, err := t.getMarketOrderRules(symbol)
	if err != nil {
		return "", err
	}
	return formatQuantityWithRules(symbol, quantity, rules)
}

type symbolOrderRules struct {
	StepSizeText   string
	StepSize       float64
	MinQty         float64
	MaxQty         float64
	MinNotional    float64
	QuantityFilter string
}

// LIMIT quantities use LOT_SIZE; market sizing and execution use MARKET_LOT_SIZE.
func (t *FuturesTrader) getSymbolOrderRules(symbol string) (symbolOrderRules, error) {
	return t.symbolOrderRulesFor(symbol, "LOT_SIZE")
}

func (t *FuturesTrader) getMarketOrderRules(symbol string) (symbolOrderRules, error) {
	return t.symbolOrderRulesFor(symbol, "MARKET_LOT_SIZE")
}

func (t *FuturesTrader) symbolOrderRulesFor(symbol, quantityFilter string) (symbolOrderRules, error) {
	s, err := t.cachedSymbolRules(symbol)
	if err != nil {
		return symbolOrderRules{}, fmt.Errorf("failed to get trading rules: %w", err)
	}
	rules := symbolOrderRules{MinNotional: t.symbolMinNotional(s), QuantityFilter: quantityFilter}
	for _, filter := range s.Filters {
		if filter["filterType"] != quantityFilter {
			continue
		}
		rules.StepSizeText, _ = filter["stepSize"].(string)
		rules.StepSize, _ = strconv.ParseFloat(rules.StepSizeText, 64)
		minText, _ := filter["minQty"].(string)
		maxText, _ := filter["maxQty"].(string)
		rules.MinQty, _ = strconv.ParseFloat(minText, 64)
		rules.MaxQty, _ = strconv.ParseFloat(maxText, 64)
		if !positiveFinite(rules.StepSize) || math.IsNaN(rules.MinQty) || math.IsNaN(rules.MaxQty) || rules.MinQty < 0 || rules.MaxQty < 0 || (rules.MaxQty > 0 && rules.MaxQty < rules.MinQty) {
			return symbolOrderRules{}, fmt.Errorf("%s %s trading rule is invalid", symbol, quantityFilter)
		}
		return rules, nil
	}
	if quantityFilter == "MARKET_LOT_SIZE" {
		// A snapshot without MARKET_LOT_SIZE (never seen on real Binance
		// data) keeps the pre-existing LOT_SIZE basis; the exchange still
		// rejects an oversize market order, so no risk is added.
		t.warnRulesOnce(symbol, "market-lot-missing", "MARKET_LOT_SIZE missing; market orders use LOT_SIZE")
		return t.symbolOrderRulesFor(symbol, "LOT_SIZE")
	}
	return symbolOrderRules{}, fmt.Errorf("%s %s trading rule is missing", symbol, quantityFilter)
}

// Unknown market rules must block additions. Reductions can still use the
// known LOT_SIZE grid when MARKET_LOT_SIZE is absent from a partial snapshot.
func (t *FuturesTrader) formatReduceQuantity(symbol string, quantity float64) (string, error) {
	rules, err := t.getMarketOrderRules(symbol)
	if err != nil {
		t.warnRulesOnce(symbol, "reduce-lot-fallback", "MARKET_LOT_SIZE unavailable; reduction uses LOT_SIZE: %v", err)
		rules, err = t.getSymbolOrderRules(symbol)
	}
	if err != nil {
		return "", err
	}
	return formatQuantityWithRules(symbol, quantity, rules)
}

func positiveFinite(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

func formatQuantityWithRules(symbol string, quantity float64, rules symbolOrderRules) (string, error) {
	if quantity <= 0 || math.IsNaN(quantity) || math.IsInf(quantity, 0) {
		return "", fmt.Errorf("%s quantity must be positive, got %.8f", symbol, quantity)
	}
	if rules.MaxQty > 0 && quantity > rules.MaxQty+rules.StepSize*1e-9 {
		return "", fmt.Errorf("MAX_QTY: %s %s quantity %.8f exceeds maxQty %.8f (automatic splitting is disabled)", symbol, rules.QuantityFilter, quantity, rules.MaxQty)
	}
	// Always round DOWN so exchange precision handling can never increase the
	// program-owned risk budget.
	steps := math.Floor(quantity/rules.StepSize + 1e-9)
	formattedValue := steps * rules.StepSize
	precision := calculatePrecision(rules.StepSizeText)
	formatted := fmt.Sprintf("%.*f", precision, formattedValue)
	if formattedValue <= 0 || (rules.MinQty > 0 && formattedValue+rules.StepSize*1e-9 < rules.MinQty) {
		return "", fmt.Errorf("MIN_QTY: %s quantity %.8f rounds down to %s (stepSize=%s, minQty=%.*f)",
			symbol, quantity, formatted, rules.StepSizeText, precision, rules.MinQty)
	}
	if rules.MaxQty > 0 && formattedValue > rules.MaxQty+rules.StepSize*1e-9 {
		return "", fmt.Errorf("MAX_QTY: %s %s quantity %s exceeds maxQty %.*f (automatic splitting is disabled)", symbol, rules.QuantityFilter, formatted, precision, rules.MaxQty)
	}
	return formatted, nil
}

// GetSymbolPricePrecision gets the price precision for a trading pair
func (t *FuturesTrader) GetSymbolPricePrecision(symbol string) (int, error) {
	s, err := t.cachedSymbolRules(symbol)
	if err != nil {
		return 0, fmt.Errorf("failed to get trading rules: %w", err)
	}
	for _, filter := range s.Filters {
		if filter["filterType"] == "PRICE_FILTER" {
			tickSize, _ := filter["tickSize"].(string)
			return calculatePrecision(tickSize), nil
		}
	}
	return 2, nil
}

// symbolTickSize returns the raw PRICE_FILTER.tickSize string, preserving the
// tick's decimal count and non-power-of-ten grids used by quantizeToTick.
func (t *FuturesTrader) symbolTickSize(symbol string) (string, error) {
	s, err := t.cachedSymbolRules(symbol)
	if err != nil {
		return "", fmt.Errorf("failed to get trading rules: %w", err)
	}
	for _, filter := range s.Filters {
		if filter["filterType"] == "PRICE_FILTER" {
			tickSize, _ := filter["tickSize"].(string)
			return tickSize, nil
		}
	}
	return "", nil
}

// formatAlgoTriggerPrice renders an algo-order trigger on the symbol's real
// tick grid when the tick size is available, falling back to decimal-place
// formatting when it is not. All algo-order trigger prices (SL/TP) go
// through here.
func (t *FuturesTrader) formatAlgoTriggerPrice(symbol string, price float64) (string, error) {
	if tickStr, err := t.symbolTickSize(symbol); err == nil && tickStr != "" {
		if tick, perr := strconv.ParseFloat(tickStr, 64); perr == nil && tick > 0 {
			return quantizeToTick(price, tick, calculatePrecision(tickStr))
		}
	}
	prec, _ := t.GetSymbolPricePrecision(symbol)
	return formatTriggerPrice(price, prec)
}

// FormatPrice formats price to correct precision
func (t *FuturesTrader) FormatPrice(symbol string, price float64) (string, error) {
	precision, err := t.GetSymbolPricePrecision(symbol)
	if err != nil {
		// If retrieval fails, use default format
		return fmt.Sprintf("%.2f", price), nil
	}

	format := fmt.Sprintf("%%.%df", precision)
	return fmt.Sprintf(format, price), nil
}
