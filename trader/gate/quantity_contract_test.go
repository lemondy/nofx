package gate

import (
	"github.com/gateio/gateapi-go/v6"
	"strconv"
	"testing"
)

func TestFormatQuantityReturnsBaseUnits(t *testing.T) {
	for _, mult := range []float64{0.01, 10} {
		tr := &GateTrader{contractsCache: map[string]*gateapi.Contract{"X_USDT": {QuantoMultiplier: strconv.FormatFloat(mult, 'f', -1, 64)}}}
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
