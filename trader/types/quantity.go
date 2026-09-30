package types

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// FloorQuantity quantizes entry size in decimal units without increasing exposure.
func FloorQuantity(quantity, step, min, max float64) (float64, error) {
	if min < 0 || max < 0 || math.IsNaN(min) || math.IsInf(min, 0) || math.IsNaN(max) || math.IsInf(max, 0) {
		return 0, fmt.Errorf("invalid quantity limits")
	}
	if quantity <= 0 || step <= 0 || math.IsNaN(quantity) || math.IsInf(quantity, 0) || math.IsNaN(step) || math.IsInf(step, 0) {
		return 0, fmt.Errorf("invalid quantity or step")
	}
	if max > 0 && quantity > max {
		quantity = max
	}
	q, _ := new(big.Rat).SetString(strconv.FormatFloat(quantity, 'f', -1, 64))
	s, _ := new(big.Rat).SetString(strconv.FormatFloat(step, 'f', -1, 64))
	ratio := new(big.Rat).Quo(q, s)
	lots := new(big.Int).Quo(ratio.Num(), ratio.Denom())
	aligned, _ := new(big.Rat).Mul(new(big.Rat).SetInt(lots), s).Float64()
	if aligned <= 0 || aligned < min {
		return 0, fmt.Errorf("quantity is below minimum lot size")
	}
	return aligned, nil
}
