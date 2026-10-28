package breakout

import "time"

// Version 2 used an old hourly close as entry; version 3 records a ticker
// observation or a closed one-minute fallback. Legacy rows remain readable.
const shortPriceLabelVersion = 3

type PriceObservation struct {
	Price      float64 `json:"price"`
	Source     string  `json:"price_source"`
	ObservedAt int64   `json:"price_observed_at"` // UTC Unix milliseconds
}

func (p PriceObservation) valid() bool {
	return p.Price > 0 && trendFinite(p.Price) && p.ObservedAt > 0 &&
		(p.Source == "ticker_24h_last" || p.Source == "closed_1m")
}

func tickerPrice(price float64, observedAt int64, now time.Time) PriceObservation {
	p := PriceObservation{price, "ticker_24h_last", observedAt}
	if !p.valid() || observedAt > now.UnixMilli() {
		return PriceObservation{}
	}
	return p
}

// DataSource exposes closed candles, but has no live quote method. Callers
// with a ticker snapshot replace this fallback without changing indicators.
func latestClosedMinutePrice(ds DataSource, now time.Time) PriceObservation {
	bars, err := ds.Klines("1m", 2)
	if err != nil {
		return PriceObservation{}
	}
	var latest PriceObservation
	for _, b := range bars {
		at := b.OpenTime + time.Minute.Milliseconds()
		p := PriceObservation{b.Close, "closed_1m", at}
		if b.OpenTime > 0 && p.valid() && at <= now.UnixMilli() && at > latest.ObservedAt {
			latest = p
		}
	}
	return latest
}

func shortEntryTime(s shortSample) int64 {
	if s.LabelVersion == shortPriceLabelVersion {
		return s.PriceObservedAt
	}
	return s.TS
}

func shortSampleLabelVersion(s shortSample) int {
	if s.LabelVersion == shortPriceLabelVersion {
		return shortPriceLabelVersion
	}
	return 2 // Preserve the old entry convention when refreshing legacy exits.
}
