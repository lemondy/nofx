package binance

import (
	"context"
	"fmt"
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
		t.positionsCacheMutex.RUnlock()
		logger.Infof("✓ Using cached position information (cache age: %.1f seconds ago)", cacheAge.Seconds())
		return t.cachedPositions, nil
	}
	t.positionsCacheMutex.RUnlock()

	// Cache expired or doesn't exist, call API
	logger.Infof("🔄 Cache expired, calling Binance API to get position information...")
	positions, err := t.client.NewGetPositionRiskService().Do(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get positions: %w", WrapAuthError("GetPositions", err))
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

	// Update cache
	t.positionsCacheMutex.Lock()
	t.cachedPositions = result
	t.positionsCacheTime = time.Now()
	t.positionsCacheMutex.Unlock()

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
			logger.Infof("  ✓ %s leverage is already %dx", symbol, leverage)
			return nil
		}
		return fmt.Errorf("failed to set leverage: %w", err)
	}

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

	return price, nil
}

// CalculatePositionSize calculates position size
func (t *FuturesTrader) CalculatePositionSize(balance, riskPercent, price float64, leverage int) float64 {
	riskAmount := balance * (riskPercent / 100.0)
	positionValue := riskAmount * float64(leverage)
	quantity := positionValue / price
	return quantity
}

// GetMinNotional gets minimum notional value (Binance requirement)
func (t *FuturesTrader) GetMinNotional(symbol string) float64 {
	// Use conservative default value of 10 USDT to ensure order passes exchange validation
	return 10.0
}

// CheckMinNotional checks if order meets minimum notional value requirement
func (t *FuturesTrader) CheckMinNotional(symbol string, quantity float64) error {
	price, err := t.GetMarketPrice(symbol)
	if err != nil {
		return fmt.Errorf("failed to get market price: %w", err)
	}

	notionalValue := quantity * price
	minNotional := t.GetMinNotional(symbol)

	if notionalValue < minNotional {
		return fmt.Errorf(
			"order amount %.2f USDT is below minimum requirement %.2f USDT (quantity: %.4f, price: %.4f)",
			notionalValue, minNotional, quantity, price,
		)
	}

	return nil
}

// GetSymbolPrecision gets the quantity precision for a trading pair
func (t *FuturesTrader) GetSymbolPrecision(symbol string) (int, error) {
	exchangeInfo, err := t.client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return 0, fmt.Errorf("failed to get trading rules: %w", err)
	}

	for _, s := range exchangeInfo.Symbols {
		if s.Symbol == symbol {
			// Get precision from LOT_SIZE filter
			for _, filter := range s.Filters {
				if filter["filterType"] == "LOT_SIZE" {
					stepSize := filter["stepSize"].(string)
					precision := calculatePrecision(stepSize)
					logger.Infof("  %s quantity precision: %d (stepSize: %s)", symbol, precision, stepSize)
					return precision, nil
				}
			}
		}
	}

	logger.Infof("  ⚠ %s precision information not found, using default precision 3", symbol)
	return 3, nil // Default precision is 3
}

// FormatQuantity formats quantity to correct precision
func (t *FuturesTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	precision, err := t.GetSymbolPrecision(symbol)
	if err != nil {
		// If retrieval fails, use default format
		return fmt.Sprintf("%.3f", quantity), nil
	}

	format := fmt.Sprintf("%%.%df", precision)
	return fmt.Sprintf(format, quantity), nil
}

// GetSymbolPricePrecision gets the price precision for a trading pair
func (t *FuturesTrader) GetSymbolPricePrecision(symbol string) (int, error) {
	exchangeInfo, err := t.client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return 0, fmt.Errorf("failed to get trading rules: %w", err)
	}

	for _, s := range exchangeInfo.Symbols {
		if s.Symbol == symbol {
			// Get precision from PRICE_FILTER filter
			for _, filter := range s.Filters {
				if filter["filterType"] == "PRICE_FILTER" {
					tickSize := filter["tickSize"].(string)
					precision := calculatePrecision(tickSize)
					return precision, nil
				}
			}
		}
	}

	// Default to 2 decimal places for price
	return 2, nil
}

// symbolTickSize returns the raw PRICE_FILTER.tickSize string for a symbol
// ("" when absent). The RAW string matters: quantizeToTick needs the tick's
// own decimal count, and the tick VALUE may be a non-power-of-ten (0.025)
// that decimal-place rounding alone cannot quantize to (round-4 review
// R4-15).
func (t *FuturesTrader) symbolTickSize(symbol string) (string, error) {
	exchangeInfo, err := t.client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return "", fmt.Errorf("failed to get trading rules: %w", err)
	}
	for _, s := range exchangeInfo.Symbols {
		if s.Symbol == symbol {
			for _, filter := range s.Filters {
				if filter["filterType"] == "PRICE_FILTER" {
					return filter["tickSize"].(string), nil
				}
			}
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
