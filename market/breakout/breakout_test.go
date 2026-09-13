package breakout

import (
	"fmt"
	"testing"
)

// mockDS serves per-interval synthetic series, padding at the FRONT so the
// tail (the scenario candles) always stays at the end.
type mockDS struct {
	series  map[string][]Kline // interval -> candles (tail = scenario)
	spot    []Kline
	depth   *DepthSnapshot
	oi      []OIPoint
	funding []FundingPoint
	ls      []LongShortPoint
}

func (m *mockDS) Klines(interval string, limit int) ([]Kline, error) {
	src, ok := m.series[interval]
	if !ok || len(src) == 0 {
		// fall back to a generic series (first registered)
		for _, v := range m.series {
			src = v
			break
		}
	}
	if len(src) == 0 {
		return nil, fmt.Errorf("no klines")
	}
	pad := make([]Kline, 0, limit)
	first := src[0]
	for len(pad)+len(src) < limit {
		pad = append(pad, first)
	}
	out := append(pad, src...)
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (m *mockDS) SpotKlines(interval string, limit int) ([]Kline, error) {
	if len(m.spot) == 0 {
		return m.Klines(interval, limit)
	}
	pad := make([]Kline, 0, limit)
	first := m.spot[0]
	for len(pad)+len(m.spot) < limit {
		pad = append(pad, first)
	}
	out := append(pad, m.spot...)
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (m *mockDS) Depth1Pct() (*DepthSnapshot, error) {
	if m.depth != nil {
		return m.depth, nil
	}
	return nil, fmt.Errorf("no depth")
}

func (m *mockDS) OIHistory(period string, limit int) ([]OIPoint, error) {
	if len(m.oi) == 0 {
		return nil, fmt.Errorf("no oi")
	}
	pad := make([]OIPoint, 0, limit)
	first := m.oi[0]
	for len(pad)+len(m.oi) < limit {
		pad = append(pad, first)
	}
	out := append(pad, m.oi...)
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (m *mockDS) LongShortRatio(period string, limit int) ([]LongShortPoint, error) {
	if len(m.ls) == 0 {
		return nil, fmt.Errorf("no long/short ratio")
	}
	pad := make([]LongShortPoint, 0, limit)
	first := m.ls[0]
	for len(pad)+len(m.ls) < limit {
		pad = append(pad, first)
	}
	out := append(pad, m.ls...)
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (m *mockDS) FundingInfo() (*FundingInfo, error) {
	if len(m.funding) == 0 {
		return nil, fmt.Errorf("no funding")
	}
	return &FundingInfo{NextRate: m.funding[len(m.funding)-1].Rate, IntervalHours: 8}, nil
}

func (m *mockDS) FundingHistory(limit int) ([]FundingPoint, error) {
	if len(m.funding) == 0 {
		return nil, fmt.Errorf("no funding")
	}
	pad := make([]FundingPoint, 0, limit)
	first := m.funding[0]
	for len(pad)+len(m.funding) < limit {
		pad = append(pad, first)
	}
	out := append(pad, m.funding...)
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func mkCandle(price, vol, takerPct float64) Kline {
	return Kline{
		Open: price, High: price * 1.001, Low: price * 0.999, Close: price,
		Volume: vol, QuoteVolume: vol * price, Trades: 100,
		TakerBuyBase: vol * takerPct, TakerBuyQty: vol * takerPct * price,
	}
}

// breakoutSetup: consolidation below L, then a 5-bar ramp that crosses L at
// bar -3. risingOI selects the OI trend; extremeFunding crowns the current
// funding rate at the top of its 30d distribution.
func breakoutSetup(L float64, risingOI, extremeFunding bool) *mockDS {
	m := &mockDS{series: map[string][]Kline{}}

	// Daily: 90 candles closing below L, highs touching L → 20d/60d high = L.
	day := make([]Kline, 90)
	for i := range day {
		day[i] = mkCandle(L*0.98, 50_000, 0.5)
		day[i].High = L
		day[i].Low = L * 0.975
	}
	m.series["1d"] = day

	// Intraday shape shared by 15m and 1h: flat then ramp.
	build := func(n int) []Kline {
		out := make([]Kline, 0, n)
		for i := 0; i < n-5; i++ {
			out = append(out, mkCandle(L*0.985, 1000, 0.5))
		}
		ramp := []float64{L * 0.99, L * 0.998, L * 1.008, L * 1.02, L * 1.035}
		for i, p := range ramp {
			out = append(out, mkCandle(p, 1500+float64(i)*1000, 0.7))
		}
		return out
	}
	m.series["15m"] = build(700)
	m.series["1h"] = build(170)

	// OI: steady rise or fall across the last day (5m bars).
	for i := 0; i < 288; i++ {
		v := 100.0
		if risingOI {
			v = 100 + float64(i)*0.5
		} else {
			v = 244 - float64(i)*0.5
		}
		m.oi = append(m.oi, OIPoint{OI: v, Value: v, TS: int64(i) * 300_000})
	}

	// Funding: history cycles mid-range; current is either mid (calm) or max (crowded).
	for i := 0; i < 99; i++ {
		m.funding = append(m.funding, FundingPoint{Rate: 0.00005 * float64(i%10), TS: int64(i)})
	}
	if extremeFunding {
		m.funding = append(m.funding, FundingPoint{Rate: 0.003, TS: 1000})
	} else {
		m.funding = append(m.funding, FundingPoint{Rate: 0.00025, TS: 1000})
	}

	imb := 0.2
	m.depth = &DepthSnapshot{Mid: L * 1.035, BidQty1Pct: 120, AskQty1Pct: 80, Imbalance: imb}
	return m
}

func TestAnalyzeBreakoutRisingOI(t *testing.T) {
	ds := breakoutSetup(0.10, true, false)
	rep, err := Analyze("TESTUSDT", ds)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	up := rep.Breakout
	if up.Score < 60 {
		t.Errorf("healthy breakout should score >=60, got %.2f (15m=%.2f 1h=%.2f)", up.Score, up.Score15m, up.Score1h)
	}
	if up.Grade != "strong" && up.Grade != "medium" {
		t.Errorf("expected strong/medium grade, got %s", up.Grade)
	}
	tf := rep.Timeframes["1h"][DirUp]
	if tf.Alpha != alphaHealthyOI {
		t.Errorf("price up + OI up should give α=1.0, got %.2f", tf.Alpha)
	}
	if !tf.Confirmed {
		t.Error("price above level should be pullback-confirmed")
	}
	if tf.LevelSource != "20d_high" {
		t.Errorf("expected trigger level 20d_high, got %s (%.4f)", tf.LevelSource, tf.Level)
	}
}

func TestAnalyzeBreakoutFallingOI_Squeezed(t *testing.T) {
	ds := breakoutSetup(0.10, false, false)
	rep, err := Analyze("TESTUSDT", ds)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	tf := rep.Timeframes["1h"][DirUp]
	if tf.Alpha != alphaWeakOI {
		t.Errorf("price up + OI down should give α=0.6, got %.2f", tf.Alpha)
	}
	repH, _ := Analyze("TESTUSDT", breakoutSetup(0.10, true, false))
	if rep.Breakout.Score >= repH.Breakout.Score {
		t.Errorf("squeeze breakout (%.2f) should score lower than healthy breakout (%.2f)",
			rep.Breakout.Score, repH.Breakout.Score)
	}
}

func TestAnalyzeCrowdedFunding(t *testing.T) {
	repC, _ := Analyze("TESTUSDT", breakoutSetup(0.10, true, false))
	repX, _ := Analyze("TESTUSDT", breakoutSetup(0.10, true, true))

	tfX := repX.Timeframes["1h"][DirUp]
	if tfX.Beta != betaCrowded {
		t.Errorf("extreme funding should give β=0.85, got %.2f", tfX.Beta)
	}
	if repX.Breakout.Score >= repC.Breakout.Score {
		t.Errorf("crowded breakout (%.2f) should score lower than calm (%.2f)",
			repX.Breakout.Score, repC.Breakout.Score)
	}
	if repC.Timeframes["1h"][DirUp].Beta != 1.0 {
		t.Error("calm funding should keep β=1.0")
	}
}

func TestAnalyzeFlat_NoBreakout(t *testing.T) {
	m := &mockDS{series: map[string][]Kline{
		"1d":  flat(90, 100),
		"15m": flat(700, 100),
		"1h":  flat(170, 100),
	}}
	rep, err := Analyze("TESTUSDT", m)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if rep.Breakout.Score > 60 || rep.Breakdown.Score > 60 {
		t.Errorf("flat market scores should be <=60, got up=%.2f down=%.2f",
			rep.Breakout.Score, rep.Breakdown.Score)
	}
	if rep.Selected != DirUp && rep.Selected != DirDown {
		t.Errorf("unexpected selected direction %q", rep.Selected)
	}
}

func TestAnalyzeBreakdownDirection(t *testing.T) {
	// Mirror scenario: consolidation above L, ramp down through it.
	L := 0.10
	m := &mockDS{series: map[string][]Kline{}}
	day := make([]Kline, 90)
	for i := range day {
		day[i] = mkCandle(L*1.02, 50_000, 0.5)
		day[i].Low = L
		day[i].High = L * 1.025
	}
	m.series["1d"] = day
	build := func(n int) []Kline {
		out := make([]Kline, 0, n)
		for i := 0; i < n-5; i++ {
			out = append(out, mkCandle(L*1.015, 1000, 0.5))
		}
		ramp := []float64{L * 1.01, L * 1.002, L * 0.992, L * 0.98, L * 0.965}
		for i, p := range ramp {
			out = append(out, mkCandle(p, 1500+float64(i)*1000, 0.3)) // taker sell dominant
		}
		return out
	}
	m.series["15m"] = build(700)
	m.series["1h"] = build(170)
	for i := 0; i < 288; i++ {
		v := 100 + float64(i)*0.5
		m.oi = append(m.oi, OIPoint{OI: v, Value: v, TS: int64(i) * 300_000})
	}
	for i := 0; i < 100; i++ {
		m.funding = append(m.funding, FundingPoint{Rate: 0.0001, TS: int64(i)})
	}
	m.depth = &DepthSnapshot{Mid: L * 0.965, BidQty1Pct: 80, AskQty1Pct: 120, Imbalance: -0.2}

	rep, err := Analyze("TESTUSDT", m)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if rep.Breakdown.Score < 60 {
		t.Errorf("healthy breakdown should score >=60, got %.2f", rep.Breakdown.Score)
	}
	if rep.Selected != DirDown {
		t.Errorf("expected selected=breakdown, got %s (up=%.2f down=%.2f)",
			rep.Selected, rep.Breakout.Score, rep.Breakdown.Score)
	}
	tf := rep.Timeframes["1h"][DirDown]
	if tf.Alpha != alphaHealthyOI {
		t.Errorf("price down + OI up should give α=1.0, got %.2f", tf.Alpha)
	}
}

func TestLevelSourcesPresent(t *testing.T) {
	rep, err := Analyze("TESTUSDT", breakoutSetup(0.10, true, false))
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(rep.Levels) < 8 {
		t.Errorf("expected >=8 candidate levels (HH/LL/BB/VPVR/Fib), got %d", len(rep.Levels))
	}
	names := map[string]bool{}
	for _, lv := range rep.Levels {
		names[lv.Name] = true
	}
	for _, want := range []string{"20d_high", "60d_low", "bb_upper_1d", "vpvr_poc"} {
		if !names[want] {
			t.Errorf("missing level source %s", want)
		}
	}
}

func flat(n int, p float64) []Kline {
	out := make([]Kline, n)
	for i := range out {
		out[i] = mkCandle(p, 1000, 0.5)
	}
	return out
}
