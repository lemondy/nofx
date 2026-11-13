package bitget

import "math"

func (t *BitgetTrader) GetSymbolTickSize(symbol string) (float64, error) {
	rules, err := t.getContract(t.convertSymbol(symbol))
	if err != nil {
		return 0, err
	}
	step := rules.PriceEndStep
	if step <= 0 {
		step = 1
	}
	return step * math.Pow10(-rules.PricePlace), nil
}
