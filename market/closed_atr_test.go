package market

import (
	"testing"
	"time"
)

func TestShortClosedATR(t *testing.T) {
	ks := make([]Kline, 15)
	for i := range ks {
		ks[i] = Kline{OpenTime: time.Now().Add(time.Duration(i-14) * time.Hour).UnixMilli(), High: 12, Low: 9, Close: 10}
	}
	if got := calculateTimeframeSeries(ks, "1h", 15).ATR14; got != 0 {
		t.Fatalf("forming bar leaked into ATR: %v", got)
	}
}
