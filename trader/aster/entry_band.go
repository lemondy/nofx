package aster

import "math"

// GetSymbolTickSize shares the limit path's cached symbol rules.
func (t *AsterTrader) GetSymbolTickSize(symbol string) (float64, error) {
	rules, err := t.getPrecision(symbol)
	if err != nil {
		return 0, err
	}
	if rules.TickSize > 0 {
		return rules.TickSize, nil
	}
	return math.Pow10(-rules.PricePrecision), nil
}
