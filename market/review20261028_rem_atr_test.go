package market

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestReview20261028RemATRGolden(t *testing.T) {
	// Hand-calculated Wilder reference; TA-Lib is unavailable locally.
	// TR[1..14] = 5,5,8,7,6,11,6,8,5,8,8,8,7,7; sum=99, seed=99/14.
	// Subsequent TRs = 6,8,8,8,8,7,7,10,8,7,8,8,7,8,8,10,7.
	// Each subsequent golden value is (previous*13+TR)/14, using exact fractions.
	bars := []Kline{
		{Open: 100, High: 110, Low: 90, Close: 100},
		{Open: 101, High: 104, Low: 99, Close: 102},
		{Open: 103, High: 107, Low: 102, Close: 106},
		{Open: 110, High: 114, Low: 109, Close: 112},
		{Open: 111, High: 113, Low: 106, Close: 108},
		{Open: 108, High: 111, Low: 105, Close: 110},
		{Open: 103, High: 105, Low: 99, Close: 101},
		{Open: 102, High: 106, Low: 100, Close: 105},
		{Open: 106, High: 112, Low: 104, Close: 110},
		{Open: 110, High: 111, Low: 106, Close: 107},
		{Open: 109, High: 115, Low: 108, Close: 113},
		{Open: 118, High: 121, Low: 116, Close: 120},
		{Open: 119, High: 122, Low: 114, Close: 116},
		{Open: 115, High: 117, Low: 110, Close: 112},
		{Open: 111, High: 116, Low: 109, Close: 114},
		{Open: 115, High: 119, Low: 113, Close: 118},
		{Open: 122, High: 126, Low: 120, Close: 125},
		{Open: 124, High: 125, Low: 117, Close: 119},
		{Open: 115, High: 118, Low: 111, Close: 113},
		{Open: 114, High: 120, Low: 112, Close: 118},
		{Open: 119, High: 123, Low: 116, Close: 121},
		{Open: 120, High: 122, Low: 115, Close: 117},
		{Open: 110, High: 114, Low: 107, Close: 109},
		{Open: 110, High: 116, Low: 108, Close: 114},
		{Open: 115, High: 119, Low: 112, Close: 117},
		{Open: 121, High: 125, Low: 119, Close: 123},
		{Open: 122, High: 124, Low: 116, Close: 118},
		{Open: 119, High: 122, Low: 115, Close: 120},
		{Open: 121, High: 127, Low: 119, Close: 125},
		{Open: 124, High: 126, Low: 118, Close: 120},
		{Open: 113, High: 117, Low: 110, Close: 112},
		{Open: 114, High: 119, Low: 112, Close: 117},
	}
	want := []float64{
		7.071428571428571, 6.994897959183674, 7.066690962099125,
		7.133355893377759, 7.195259043850776, 7.252740540718579,
		7.234687644952966, 7.21792424174204, 7.416643938760466,
		7.458312228849003, 7.425575641074074, 7.466605952425926,
		7.504705527252646, 7.468655132448886, 7.506608337273965,
		7.541850598897253, 7.717432698976021, 7.66618750619202,
	}
	h, l, c := review20261028RemOHLC(bars)
	got := WilderATRSeries(h, l, c, 14)
	for i, v := range got {
		expected := 0.0
		if i >= 14 {
			expected = want[i-14]
		}
		if math.Abs(v-expected) > 1e-12 {
			t.Fatalf("bar %d: %.16g want %.16g", i, v, expected)
		}
		if math.Abs(v-calculateATR(bars[:i+1], 14)) > 1e-12 {
			t.Fatalf("calculateATR disagrees at bar %d", i)
		}
	}
	h[0], l[0] = 1000, 1
	again := WilderATRSeries(h, l, c, 14)
	for i, v := range got {
		if v != again[i] {
			t.Fatalf("TR[0] leaked into index %d", i)
		}
	}
}

func TestReview20261028RemATRWarmup(t *testing.T) {
	for _, p := range []int{-1, 0, 1, 14, 15} {
		h, l, c := make([]float64, 15), make([]float64, 15), make([]float64, 15)
		for i := range h {
			h[i], l[i], c[i] = 12, 8, 10
		}
		out := WilderATRSeries(h, l, c, p)
		if len(out) != 15 {
			t.Fatal("series must remain index-aligned")
		}
		for i, v := range out {
			want := 0.0
			if p > 0 && i >= p {
				want = 4
			}
			if v != want {
				t.Fatalf("period %d index %d: %v want %v", p, i, v, want)
			}
		}
	}
	if len(WilderATRSeries(nil, nil, nil, 14)) != 0 {
		t.Fatal("nonempty result for empty input")
	}
	if got := WilderATRSeries(nil, nil, []float64{1, 2}, 1); got[1] != 0 {
		t.Fatal("mismatched inputs must be unavailable")
	}
}

func review20261028RemOHLC(bars []Kline) (h, l, c []float64) {
	for _, b := range bars {
		h, l, c = append(h, b.High), append(l, b.Low), append(c, b.Close)
	}
	return
}

// Old breakout and stockengine terminal values agree once both are seeded.
func review20261028RemATRImpact(bars []Kline) float64 {
	old := 0.0
	for i, b := range bars {
		tr := b.High - b.Low
		if i > 0 {
			tr = math.Max(tr, math.Max(math.Abs(b.High-bars[i-1].Close), math.Abs(b.Low-bars[i-1].Close)))
		}
		if i < 14 {
			old += (tr - old) / float64(i+1)
		} else {
			old = (old*13 + tr) / 14
		}
	}
	h, l, c := review20261028RemOHLC(bars)
	series := WilderATRSeries(h, l, c, 14)
	v := series[len(series)-1]
	if v == 0 {
		return 0
	}
	return math.Abs(old-v) / v * 100
}

func TestReview20261028RemATRImpact(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "kernel", "testdata", "liquidity_sweep_ict.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Bars [][]float64 `json:"bars_5m"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	series := make(map[string][][]float64)
	for i, c := range fixture.Cases {
		series[string(rune('A'+i))] = c.Bars
	}
	review20261028RemLogImpact(t, "synthetic fixture", series)
	// Optional, read-only local Binance OHLC cache; .state.json files are ignored.
	if dir := os.Getenv("ATR_REVIEW_CACHE_DIR"); dir != "" {
		paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		series = make(map[string][][]float64)
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var rows [][]float64
			if json.Unmarshal(data, &rows) == nil {
				series[filepath.Base(p)] = rows
			}
		}
		if len(series) == 0 {
			t.Fatal("no cached candle series found")
		}
		review20261028RemLogImpact(t, "real cache", series)
	}
}

func review20261028RemLogImpact(t *testing.T, name string, series map[string][][]float64) {
	t.Helper()
	for _, n := range []int{15, 30, 60, 65, 168, 169, 200, 260, 864, 865} {
		sum, maximum, count, worst := 0.0, 0.0, 0, ""
		for name, raw := range series {
			if len(raw) < n {
				continue
			}
			var bars []Kline
			for _, r := range raw[len(raw)-n:] {
				if len(r) < 5 {
					t.Fatal("invalid OHLC row")
				}
				bars = append(bars, Kline{Open: r[1], High: r[2], Low: r[3], Close: r[4]})
			}
			diff := review20261028RemATRImpact(bars)
			sum, count = sum+diff, count+1
			if diff > maximum {
				maximum, worst = diff, name
			}
		}
		if count > 0 {
			t.Logf("%s n=%d series=%d max=%.9g%% mean=%.9g%% worst=%s", name, n, count, maximum, sum/float64(count), worst)
		}
	}
}
