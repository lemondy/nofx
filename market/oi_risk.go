package market

import (
	"math"
	"time"
)

func finitePositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

type oiPrice struct {
	price float64
	at    time.Time
}
type oiSample struct {
	at    time.Time
	value float64
}

// These maps share oiDataMu. A bounded circular buffer retains at least
// three hours at the maximum sample rate (one point / 15s). Cache hits do
// not fabricate new observations; only successful upstream fetches sample.
const oiRingCapacity = 721

type oiRing struct {
	samples []oiSample
	next    int
}

func (r *oiRing) newest(offset int) oiSample {
	i := r.next - 1 - offset
	if i < 0 {
		i += len(r.samples)
	}
	return r.samples[i]
}
func (r *oiRing) push(s oiSample) {
	if len(r.samples) < oiRingCapacity {
		r.samples = append(r.samples, s)
		r.next = len(r.samples) % oiRingCapacity
	} else {
		r.samples[r.next] = s
		r.next = (r.next + 1) % oiRingCapacity
	}
}

var oiPrices = map[string]oiPrice{}
var oiSamples = map[string]*oiRing{}

func cacheOIPrice(symbol string, price float64) {
	if !finitePositive(price) {
		return
	}
	oiDataMu.Lock()
	defer oiDataMu.Unlock()
	oiPrices[Normalize(symbol)] = oiPrice{price, oiNow()}
}
func recordOISampleLocked(symbol string, now time.Time, value float64) {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	symbol = Normalize(symbol)
	ring := oiSamples[symbol]
	if ring == nil {
		ring = &oiRing{}
		oiSamples[symbol] = ring
	}
	if len(ring.samples) > 0 && now.Sub(ring.newest(0).at) < 15*time.Second {
		return
	}
	ring.push(oiSample{now, value})
}

// CachedOpenInterestMillions is nonblocking with respect to network I/O.
// OI is contracts/base units; multiply by a separately cached live quote.
// Stale/missing OI OR price is unknown, never a synthetic zero.
func CachedOpenInterestMillions(symbol string) (float64, bool) {
	oiDataMu.Lock()
	defer oiDataMu.Unlock()
	symbol = Normalize(symbol)
	e, ok := oiDataCache[symbol]
	p, priced := oiPrices[symbol]
	now := oiNow()
	if !ok || e.data == nil || !priced || now.Sub(e.at) < 0 || now.Sub(e.at) > oiDataTTL || now.Sub(p.at) < 0 || now.Sub(p.at) > 5*time.Minute || !finitePositive(p.price) {
		return 0, false
	}
	v := e.data.Latest * p.price / 1e6
	return v, v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// CachedOIChange1hPct measures contract OI, avoiding price-driven notional
// inflation. A reference must be 60-70m old and the newest sample <=3m old.
// Cold start / gaps fail open until a genuine hour of observations exists.
func CachedOIChange1hPct(symbol string) (float64, bool) {
	oiDataMu.Lock()
	defer oiDataMu.Unlock()
	ring := oiSamples[Normalize(symbol)]
	if ring == nil || len(ring.samples) < 2 {
		return 0, false
	}
	now := oiNow()
	last := ring.newest(0)
	if age := now.Sub(last.at); age < 0 || age > oiDataTTL {
		return 0, false
	}
	target := last.at.Add(-time.Hour)
	for offset := 1; offset < len(ring.samples); offset++ {
		ref := ring.newest(offset)
		if !ref.at.After(target) {
			if target.Sub(ref.at) > 10*time.Minute || !finitePositive(ref.value) {
				return 0, false
			}
			return (last.value/ref.value - 1) * 100, true
		}
	}
	return 0, false
}
