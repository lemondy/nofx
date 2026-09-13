// Package breakout implements the breakout/breakdown signal engine:
// six indicator dimensions scored 0-100, a weighted composite with OI
// quality factor α and funding-crowding factor β, multi-timeframe resonance
// and pullback confirmation. All data comes from Binance public market
// endpoints (no API key required).
//
// On-chain flow (exchange netflow, whale transfers) is not part of the
// Binance API — dimension 3 uses Binance-available proxies instead: taker
// buy/sell flow, OI notional change, and spot-vs-futures divergence.
package breakout

import (
	"math"
	"sort"
)

// ema returns the exponential moving average series (seeded with the first value).
func ema(xs []float64, period int) []float64 {
	if len(xs) == 0 {
		return nil
	}
	out := make([]float64, len(xs))
	k := 2.0 / (float64(period) + 1.0)
	out[0] = xs[0]
	for i := 1; i < len(xs); i++ {
		out[i] = xs[i]*k + out[i-1]*(1-k)
	}
	return out
}

// atr returns Wilder's ATR series over the candles.
func atr(h, l, c []float64, period int) []float64 {
	n := len(c)
	if n == 0 {
		return nil
	}
	tr := make([]float64, n)
	tr[0] = h[0] - l[0]
	for i := 1; i < n; i++ {
		pc := c[i-1]
		tr[i] = math.Max(h[i]-l[i], math.Max(math.Abs(h[i]-pc), math.Abs(l[i]-pc)))
	}
	out := make([]float64, n)
	out[0] = tr[0]
	for i := 1; i < n; i++ {
		if i < period {
			out[i] = out[i-1] + (tr[i]-out[i-1])/float64(i+1)
		} else {
			out[i] = (out[i-1]*(float64(period)-1) + tr[i]) / float64(period)
		}
	}
	return out
}

// rsiLast returns the latest Wilder RSI value.
func rsiLast(c []float64, period int) float64 {
	n := len(c)
	if n < period+1 {
		return 50
	}
	var gains, losses float64
	for i := 1; i <= period; i++ {
		d := c[i] - c[i-1]
		if d > 0 {
			gains += d
		} else {
			losses -= d
		}
	}
	ag, al := gains/float64(period), losses/float64(period)
	for i := period + 1; i < n; i++ {
		d := c[i] - c[i-1]
		g, l := 0.0, 0.0
		if d > 0 {
			g = d
		} else {
			l = -d
		}
		ag = (ag*(float64(period)-1) + g) / float64(period)
		al = (al*(float64(period)-1) + l) / float64(period)
	}
	if al == 0 {
		return 100
	}
	return 100 - 100/(1+ag/al)
}

// macdHistLast returns the latest MACD histogram (12/26/9).
func macdHistLast(c []float64) float64 {
	if len(c) < 35 {
		return 0
	}
	f := ema(c, 12)
	s := ema(c, 26)
	line := make([]float64, len(c))
	for i := range c {
		line[i] = f[i] - s[i]
	}
	sig := ema(line, 9)
	return line[len(line)-1] - sig[len(sig)-1]
}

// boll returns the middle/upper/lower Bollinger bands (default 20, 2σ).
func boll(c []float64, period int, mult float64) (mid, up, low float64) {
	if len(c) < period {
		last := c[len(c)-1]
		return last, last, last
	}
	w := c[len(c)-period:]
	sum := 0.0
	for _, v := range w {
		sum += v
	}
	mid = sum / float64(period)
	variance := 0.0
	for _, v := range w {
		variance += (v - mid) * (v - mid)
	}
	sd := math.Sqrt(variance / float64(period))
	return mid, mid + mult*sd, mid - mult*sd
}

// sigmoidScore maps x onto 0-100 with a logistic curve centered at `center`
// and horizontal scale `width` (both in x units). Robust against outliers.
func sigmoidScore(x, center, width float64) float64 {
	if width <= 0 {
		width = 1
	}
	return 100 / (1 + math.Exp(-(x-center)/width))
}

// winsorize clamps values beyond mean±3σ onto the boundary (3σ outlier filter).
func winsorize(xs []float64) []float64 {
	if len(xs) == 0 {
		return xs
	}
	mean := 0.0
	for _, v := range xs {
		mean += v
	}
	mean /= float64(len(xs))
	variance := 0.0
	for _, v := range xs {
		variance += (v - mean) * (v - mean)
	}
	sd := math.Sqrt(variance / float64(len(xs)))
	if sd == 0 {
		return append([]float64(nil), xs...)
	}
	lo, hi := mean-3*sd, mean+3*sd
	out := make([]float64, len(xs))
	for i, v := range xs {
		if v < lo {
			v = lo
		} else if v > hi {
			v = hi
		}
		out[i] = v
	}
	return out
}

// percentile returns the percentile (0-100) of v within series after 3σ
// winsorization.
func percentile(series []float64, v float64) float64 {
	if len(series) == 0 {
		return 50
	}
	w := winsorize(series)
	sort.Float64s(w)
	// count of elements <= v
	le := sort.Search(len(w), func(i int) bool { return w[i] > v })
	return float64(le) / float64(len(w)) * 100
}

// meanTR returns the mean true range over the last n bars (ATR multiple baseline).
func meanTR(h, l, c []float64, n int) float64 {
	limit := n
	if limit > len(c) {
		limit = len(c)
	}
	if limit == 0 {
		return 0
	}
	sum := 0.0
	for i := len(c) - limit; i < len(c); i++ {
		tr := h[i] - l[i]
		if i > 0 {
			pc := c[i-1]
			tr = math.Max(tr, math.Max(math.Abs(h[i]-pc), math.Abs(l[i]-pc)))
		}
		sum += tr
	}
	return sum / float64(limit)
}

// volumeProfile computes POC / VAH / VAL from typical prices weighted by
// quote volume (simplified VPVR over the supplied bars).
func volumeProfile(tp, w []float64, bins int) (poc, vah, val float64) {
	if len(tp) == 0 || bins <= 0 {
		return 0, 0, 0
	}
	min, max := tp[0], tp[0]
	for _, v := range tp {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if max <= min {
		return tp[len(tp)-1], max, min
	}
	step := (max - min) / float64(bins)
	vols := make([]float64, bins)
	total := 0.0
	for i, p := range tp {
		idx := int((p - min) / step)
		if idx >= bins {
			idx = bins - 1
		}
		vols[idx] += w[i]
		total += w[i]
	}
	pocIdx := 0
	for i, v := range vols {
		if v > vols[pocIdx] {
			pocIdx = i
		}
	}
	// Expand the value area from POC until 70% of volume is covered.
	lo, hi := pocIdx, pocIdx
	covered := vols[pocIdx]
	for covered < 0.7*total && (lo > 0 || hi < bins-1) {
		var below, above float64
		if lo > 0 {
			below = vols[lo-1]
		}
		if hi < bins-1 {
			above = vols[hi+1]
		}
		if above >= below && hi < bins-1 {
			hi++
			covered += above
		} else if lo > 0 {
			lo--
			covered += below
		} else {
			break
		}
	}
	return min + (float64(pocIdx)+0.5)*step, min + float64(hi+1)*step, min + float64(lo)*step
}

// gradeOf maps a composite score to the signal grade using the (tunable)
// grade thresholds.
func gradeOf(score float64) string {
	p := GetParams()
	switch {
	case score >= p.StrongThreshold:
		return "strong"
	case score >= p.MediumThreshold:
		return "medium"
	case score >= 40:
		return "weak"
	default:
		return "noise"
	}
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, v := range xs {
		s += v
	}
	return s / float64(len(xs))
}
