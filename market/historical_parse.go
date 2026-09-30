package market

import (
	"fmt"
	"math"
)

func parseHistoricalKline(row []interface{}) (Kline, error) {
	if len(row) < 7 {
		return Kline{}, fmt.Errorf("expected at least 7 fields")
	}
	var values [7]float64
	for i := range values {
		v, err := parseFloat(row[i])
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return Kline{}, fmt.Errorf("invalid field %d", i)
		}
		values[i] = v
	}
	if values[0] < 0 || values[6] < values[0] || values[1] <= 0 || values[2] <= 0 || values[3] <= 0 || values[4] <= 0 || values[5] < 0 {
		return Kline{}, fmt.Errorf("invalid candle range")
	}
	return Kline{OpenTime: int64(values[0]), Open: values[1], High: values[2], Low: values[3], Close: values[4], Volume: values[5], CloseTime: int64(values[6])}, nil
}
