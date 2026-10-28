package binance

import (
	"fmt"
	"math"
	"strconv"
)

// QuantityRules exposes the symbol's LOT_SIZE step and minQty from the
// adapter's cached exchange rules (no extra REST call beyond the rules cache
// every order already goes through). review 2026-10-28 #3/#6: the execution
// layer sizes split take-profits on the same grid FormatQuantity floors to,
// so the planned TP quantity, the coverage check and the placed order agree.
func (t *FuturesTrader) QuantityRules(symbol string) (step, minQty float64, err error) {
	rules, err := t.getSymbolOrderRules(symbol)
	if err != nil {
		return 0, 0, err
	}
	return rules.StepSize, rules.MinQty, nil
}

// QuantizeTriggerPrice returns the trigger price exactly as SetStopLoss /
// SetTakeProfit would send it (tick-grid rounding via formatAlgoTriggerPrice).
// review 2026-10-28 #8: post-fill risk must be computed from the stop that
// actually rests on the exchange, not the pre-rounding plan value.
func (t *FuturesTrader) QuantizeTriggerPrice(symbol string, price float64) (float64, error) {
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, fmt.Errorf("invalid trigger price %g", price)
	}
	s, err := t.formatAlgoTriggerPrice(symbol, price)
	if err != nil {
		return 0, err
	}
	q, err := strconv.ParseFloat(s, 64)
	if err != nil || q <= 0 || math.IsNaN(q) || math.IsInf(q, 0) {
		return 0, fmt.Errorf("unparseable trigger price %q", s)
	}
	return q, nil
}
