package lighter

import "math"

func (t *LighterTraderV2) GetSymbolTickSize(symbol string) (float64, error) {
	rules, err := t.getMarketInfo(symbol)
	if err != nil {
		return 0, err
	}
	return math.Pow10(-rules.PriceDecimals), nil
}
