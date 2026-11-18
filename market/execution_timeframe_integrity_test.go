package market

import (
	"math"
	"testing"
)

func TestExecutionFiveMinuteTimeframe(t *testing.T) {
	ks := make([]Kline, 25)
	for i := range ks {
		ks[i] = Kline{Close: float64(100 + i)}
	}
	tf := executionTimeframeData(ks, nil, nil, nil, "5m")
	if tf["3m"] != nil || tf["5m"] == nil || tf["5m"].Timeframe != "5m" {
		t.Fatalf("wrong short timeframe: %+v", tf)
	}
	if got := calculatePriceChangeByBars(ks, "5m", 60, 124); math.Abs(got-(124.0-112)/112*100) > 1e-9 {
		t.Fatalf("wrong hour span %v", got)
	}
}
