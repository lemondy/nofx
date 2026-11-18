package okx

import (
	"strconv"
	"testing"
	"time"
)

func TestFormatQuantityReturnsBaseUnits(t *testing.T) {
	for _, mult := range []float64{0.01, 10} {
		tr := &OKXTrader{instrumentsCacheTime: time.Now(), instrumentsCache: map[string]*OKXInstrument{"X-USDT-SWAP": {CtVal: mult, LotSz: 1, MinSz: 1}}}
		got, err := tr.FormatQuantity("XUSDT", 2.75*mult)
		if err != nil {
			t.Fatal(err)
		}
		q, err := strconv.ParseFloat(got, 64)
		if err != nil || q != 2*mult {
			t.Fatalf("mult=%g got=%s want=%g", mult, got, 2*mult)
		}
	}
}
