package hyperliquid

import "math"

// EntryBandPriceTick mirrors roundPriceToSigfigs, whose tick varies with the
// price magnitude. Returning the grid lets the caller round away from market.
func (t *HyperliquidTrader) EntryBandPriceTick(_ string, price float64) (float64, error) {
	if price == 0 {
		return 1, nil
	}
	return math.Pow10(int(math.Floor(math.Log10(math.Abs(price)))) - 4), nil
}
