package breakout

import (
	"testing"
)

// ============================================================================
// 09-28 review P1/P2 pins: the VolumeFade dimension was INVERTED (negative
// sigmoid width silently coerced to 1 → volume PERSISTENCE scored higher
// than a heavy fade, and the ×0.8 momentum-trap discount could never fire);
// the Overbought widths -7/-8 were coerced to 1, turning the RSI ramp into
// a near-step over the 80-90 band.
// ============================================================================

func volumeFadeFixture(vols ...float64) *ShortSignal {
	k := make([]Kline, 0, 84)
	for i := 0; i < 84; i++ {
		v := 1000.0
		if i < len(vols) {
			// vols are the TAIL volumes (last bars).
			idx := len(vols) - 1 - i
			if idx >= 0 {
				_ = idx
			}
		}
		_ = v
		k = append(k, mkCandle(100+float64(i)*0.01, v, 50))
	}
	// Overwrite the tail volumes (mkCandle's QuoteVolume = vol*price).
	tail := len(vols)
	if tail > 0 {
		k = k[:len(k)-tail]
		for _, v := range vols {
			k = append(k, mkCandle(100+float64(len(k))*0.01, v, 50))
		}
	}
	m := &mockDS{
		series:  map[string][]Kline{"1h": k, "4h": k},
		oi:      []OIPoint{{OI: 250, Value: 25_000_000, TS: 1}, {OI: 260, Value: 26_000_000, TS: 2}},
		funding: []FundingPoint{{Rate: 0.0001, TS: 1}},
	}
	sig, err := AnalyzeShort("TESTUSDT", 5, nil, m)
	if err != nil {
		return nil
	}
	return sig
}

// A heavy volume fade (recent quote volume a fraction of the 24-bar peak)
// must score HIGH; persistent volume at the peak must score LOW — the
// dimension is "fade strength", decreasing in the recent/peak ratio.
func TestVolumeFadeDecreasingInRatio(t *testing.T) {
	fade := volumeFadeFixture(50, 50, 50)
	if fade == nil {
		t.Fatal("AnalyzeShort failed on the fade fixture")
	}
	persistent := volumeFadeFixture(1000, 1000, 1000)
	if persistent == nil {
		t.Fatal("AnalyzeShort failed on the persistent fixture")
	}
	if fade.Components.VolumeFade <= 60 {
		t.Fatalf("heavy fade must score >60, got %.1f", fade.Components.VolumeFade)
	}
	if persistent.Components.VolumeFade >= 40 {
		t.Fatalf("volume at peak must score <40, got %.1f", persistent.Components.VolumeFade)
	}
	if fade.Components.VolumeFade <= persistent.Components.VolumeFade {
		t.Fatalf("fade %.1f must outrank persistence %.1f", fade.Components.VolumeFade, persistent.Components.VolumeFade)
	}
}

// With the sane ±7/±8 widths the RSI ramp is smooth, not a step: 85 sits
// mid-slope (~61), 95 saturates (~95). Pins the positive-width call.
func TestOverboughtSlopeSmooth(t *testing.T) {
	mid := sigmoidScore(85, 82, 7)
	if mid < 50 || mid > 75 {
		t.Fatalf("RSI 85 at width 7 should sit mid-slope (~61), got %.1f", mid)
	}
	hot := sigmoidScore(95, 82, 7)
	if hot < 85 {
		t.Fatalf("RSI 95 should saturate high, got %.1f", hot)
	}
	// The old coerced-width step: 80 → ~12, 85 → ~95 — a 83-point jump for
	// five RSI points must no longer be reachable; the sane slope gives
	// 80→~43 (just below center).
	if v := sigmoidScore(80, 82, 7); v > 50 {
		t.Fatalf("RSI 80 just below center must stay under 50, got %.1f", v)
	}
}
