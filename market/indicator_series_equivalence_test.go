package market

import (
	"math"
	"math/rand"
	"testing"
)

func TestSeriesPrefixEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for _, n := range []int{0, 8, 15, 26, 50, 100, 500} {
		ks := make([]Kline, n)
		for i := range ks {
			ks[i] = Kline{Close: 100 + rng.Float64()*10}
		}
		for _, count := range []int{10, 60, 600} {
			got := calculateTimeframeSeries(ks, "1h", count)
			start := n - count
			if start < 0 {
				start = 0
			}
			cases := []struct {
				got []float64
				min int
				fn  func([]Kline) float64
			}{
				{got.EMA20Values, 19, func(k []Kline) float64 { return calculateEMA(k, 20) }},
				{got.EMA50Values, 49, func(k []Kline) float64 { return calculateEMA(k, 50) }},
				{got.MACDValues, 25, calculateMACD},
				{got.RSI7Values, 7, func(k []Kline) float64 { return calculateRSI(k, 7) }},
				{got.RSI14Values, 14, func(k []Kline) float64 { return calculateRSI(k, 14) }},
			}
			for _, c := range cases {
				j := 0
				for i := start; i < n; i++ {
					if i < c.min {
						continue
					}
					want := c.fn(ks[:i+1])
					if j >= len(c.got) || math.Abs(c.got[j]-want) > 1e-9 {
						t.Fatalf("n=%d i=%d series mismatch", n, i)
					}
					j++
				}
				if j != len(c.got) {
					t.Fatal("length mismatch")
				}
			}
		}
	}
}
