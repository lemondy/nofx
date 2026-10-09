package usstock

import (
	"context"
	"os"
	"testing"
	"time"
)

// Explicitly opt in: USSTOCK_LIVE=1 GOCACHE=$PWD/.gocache go test
// ./market/usstock/ -run TestLiveSmoke -v. This test makes public GETs only.
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("USSTOCK_LIVE") != "1" {
		t.Skip("set USSTOCK_LIVE=1 to enable public endpoint smoke test")
	}
	resetCaches()
	t.Cleanup(resetCaches)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	symbols, err := ListSymbols(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("US bStock pairs: %d", len(symbols))
	if info, ok := LookupSymbol(ctx, "AAPLBUSDT"); !ok || info.Underlying != "AAPL" {
		t.Fatalf("AAPL registry: %+v %v", info, ok)
	}
	for _, symbol := range []string{"BNBUSDT", "ARBUSDT", "SHIBUSDT", "QNTBUSDT"} {
		if _, ok := LookupSymbol(ctx, symbol); ok {
			t.Errorf("crypto matched: %s", symbol)
		}
	}
	for _, tc := range []struct {
		tf   string
		need int
	}{{TF15m, 5}, {TF1d, 220}} {
		series, err := GetSeries(ctx, "AAPLBUSDT", tc.tf, tc.need, true)
		if err != nil {
			t.Fatal(err)
		}
		requireAscending(t, series.Bars)
		t.Logf("%s: source=%s bars=%d first=%s last=%s", tc.tf, series.Source, len(series.Bars), series.Bars[0].OpenTime, series.Bars[len(series.Bars)-1].OpenTime)
	}
	quote, err := GetQuote(ctx, "AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quote: %+v", quote)
}
