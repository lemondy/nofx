package market

import "testing"

// 09-28 review P3: the settlement-interval estimate was the unweighted MEAN
// of the fetched gaps — Binance throttles new listings through 1h→4h→8h, and
// an interval change inside the window poisoned the mean (gaps [1,4,4,8,8]
// → mean 5.0h), skewing the annualized funding that feeds crowding math.
// The MEDIAN is robust to the change boundary.
func TestMedianFundingIntervalHours(t *testing.T) {
	// hour offsets producing gaps [1,4,4,8,8] (a 1h→4h→8h listing ramp)
	times := []int64{0, 3.6e6, 5 * 3.6e6, 9 * 3.6e6, 17 * 3.6e6, 25 * 3.6e6}
	got := medianFundingIntervalHours(times)
	if got != 4 {
		t.Fatalf("median gap = %.2f, want 4.0 (mean would be 5.0)", got)
	}

	// Stable 8h cadence → unchanged.
	stable := []int64{0, 8 * 3.6e6, 16 * 3.6e6, 24 * 3.6e6}
	if got := medianFundingIntervalHours(stable); got != 8 {
		t.Fatalf("stable 8h median = %.2f, want 8", got)
	}

	// Even count → mean of the two middle values.
	even := []int64{0, 4 * 3.6e6, 8 * 3.6e6, 16 * 3.6e6, 20 * 3.6e6} // gaps 4,4,8,4 → sorted 4,4,4,8 → 4
	if got := medianFundingIntervalHours(even); got != 4 {
		t.Fatalf("even-count median = %.2f, want 4", got)
	}

	// No valid gaps → 0 (caller falls back to the 8h×3 default).
	if got := medianFundingIntervalHours([]int64{0}); got != 0 {
		t.Fatalf("single timestamp must yield 0, got %.2f", got)
	}
}
