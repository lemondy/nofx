package kernel

import (
	"strings"
	"testing"

	"nofx/market"
)

func negate(rs []float64) []float64 {
	out := make([]float64, len(rs))
	for i, r := range rs {
		out[i] = -r
	}
	return out
}

func mkFromReturns(rets []float64, base float64) []float64 {
	out := make([]float64, len(rets)+1)
	out[0] = base
	for i, r := range rets {
		out[i+1] = out[i] * (1 + r)
	}
	return out
}

func mkReturns(drift float64, n int) []float64 {
	rets := make([]float64, n)
	for i := range rets {
		rets[i] = drift * (1 + 0.4*float64(i%3-1)/2)
	}
	return rets
}

func mkMarketDataWith1h(closes []float64) *market.Data {
	d := &market.Data{Symbol: "TESTUSDT"}
	tf := &market.TimeframeSeriesData{}
	for _, c := range closes {
		tf.Klines = append(tf.Klines, market.KlineBar{Open: c * 0.999, High: c * 1.001, Low: c * 0.998, Close: c})
	}
	d.TimeframeData = map[string]*market.TimeframeSeriesData{"1h": tf}
	return d
}

func TestPearsonOfReturns(t *testing.T) {
	rets := mkReturns(0.01, 30)
	a := mkFromReturns(rets, 100)
	same := mkFromReturns(rets, 50)        // identical returns → corr 1
	inv := mkFromReturns(negate(rets), 50) // mirrored returns → corr -1
	if r := pearsonOfReturns(a, same); r < 0.99 {
		t.Fatalf("identical returns: pearson = %.3f, want ~1", r)
	}
	if r := pearsonOfReturns(a, inv); r > -0.99 {
		t.Fatalf("mirrored returns: pearson = %.3f, want ~-1", r)
	}
	if r := pearsonOfReturns(a[:5], same); r != 0 {
		t.Fatalf("too-short series must return 0, got %.3f", r)
	}
}

func TestConcentrationWarnings(t *testing.T) {
	a := mkFromReturns(mkReturns(0.01, 30), 100)
	b := mkFromReturns(mkReturns(0.012, 30), 80)         // near-identical return shape
	c := mkFromReturns(negate(mkReturns(0.011, 30)), 80) // inverse
	ctx := &Context{
		MarketDataMap: map[string]*market.Data{
			"AAAUSDT": mkMarketDataWith1h(a),
			"BBBUSDT": mkMarketDataWith1h(b),
			"CCCUSDT": mkMarketDataWith1h(c),
		},
		Positions: []PositionInfo{
			{Symbol: "BBBUSDT", Side: "long"},
			{Symbol: "CCCUSDT", Side: "short"},
		},
	}
	warns := concentrationWarnings(ctx, "AAAUSDT")
	if len(warns) != 1 || !strings.Contains(warns[0], "BBBUSDT") {
		t.Fatalf("expected one warning vs BBBUSDT, got %v", warns)
	}
	if strings.Contains(warns[0], "同向beta") || !strings.Contains(warns[0], "集中敞口还是对冲") {
		t.Fatalf("candidate direction is unknown; warning must stay neutral: %v", warns)
	}
	// Missing market data fails open.
	if warns := concentrationWarnings(&Context{}, "AAAUSDT"); len(warns) != 0 {
		t.Fatalf("no-data must produce no warnings, got %v", warns)
	}
}

func TestRecentHistoryNote(t *testing.T) {
	ctx := &Context{SymbolStats: map[string]*TraderHistoryStat{
		"NEARUSDT": {ClosedTrades: 4, Wins: 1, WinRatePct: 25, RealizedPnL: -0.371},
		"ZECUSDT":  {ClosedTrades: 3, Wins: 2, WinRatePct: 67, RealizedPnL: 0.59},
	}}
	if note := recentHistoryNote(ctx, "NEARUSDT"); note == "" {
		t.Fatal("loss-heavy history must produce a note")
	}
	if note := recentHistoryNote(ctx, "ZECUSDT"); note != "" {
		t.Fatalf("profitable history must not be annotated, got %s", note)
	}
	if note := recentHistoryNote(ctx, "UNSEENUSDT"); note != "" {
		t.Fatalf("no-history symbol must be silent, got %s", note)
	}
}

func TestExpectancyR(t *testing.T) {
	// 40% win rate, RR 1.5 → 0.4×1.5 − 0.6 = 0
	if r := expectancyR(40, 1.5, 1.0); r < -0.001 || r > 0.001 {
		t.Fatalf("breakeven case: expectancy_r = %.3f, want ~0", r)
	}
	// 55% win rate, RR 1.3 → 0.55×1.3 − 0.45 = +0.265
	if r := expectancyR(55, 1.3, 1.0); r < 0.264 || r > 0.266 {
		t.Fatalf("positive case: expectancy_r = %.3f, want ~0.265", r)
	}
	if r := expectancyR(30, 1.0, 1.0); r >= 0 {
		t.Fatalf("negative case: expectancy_r = %.3f, want < 0", r)
	}
	if r := expectancyR(50, 2.0, 0); r != 0 {
		t.Fatal("no losing trades data → 0 (undefined)")
	}
}
