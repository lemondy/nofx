package market

import (
	"math"
	"testing"
	"time"
)

// generateTestKlines generates test K-line data
func generateTestKlines(count int) []Kline {
	klines := make([]Kline, count)
	for i := 0; i < count; i++ {
		// Generate simulated price data with some fluctuation
		basePrice := 100.0
		variance := float64(i%10) * 0.5
		open := basePrice + variance
		high := open + 1.0
		low := open - 0.5
		close := open + 0.3
		volume := 1000.0 + float64(i*100)

		klines[i] = Kline{
			OpenTime:  int64(i * 180000), // 3-minute interval
			Open:      open,
			High:      high,
			Low:       low,
			Close:     close,
			Volume:    volume,
			CloseTime: int64((i+1)*180000 - 1),
		}
	}
	return klines
}

// TestCalculateIntradaySeries_VolumeCollection tests Volume data collection
func TestCalculateIntradaySeries_VolumeCollection(t *testing.T) {
	tests := []struct {
		name           string
		klineCount     int
		expectedVolLen int
	}{
		{
			name:           "Normal case - 20 K-lines",
			klineCount:     20,
			expectedVolLen: 10, // Should collect latest 10
		},
		{
			name:           "Exactly 10 K-lines",
			klineCount:     10,
			expectedVolLen: 10,
		},
		{
			name:           "Less than 10 K-lines",
			klineCount:     5,
			expectedVolLen: 5, // Should return all 5
		},
		{
			name:           "More than 10 K-lines",
			klineCount:     30,
			expectedVolLen: 10, // Should only return latest 10
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			klines := generateTestKlines(tt.klineCount)
			data := calculateIntradaySeries(klines)

			if data == nil {
				t.Fatal("calculateIntradaySeries returned nil")
			}

			if len(data.Volume) != tt.expectedVolLen {
				t.Errorf("Volume length = %d, want %d", len(data.Volume), tt.expectedVolLen)
			}

			// Verify Volume data correctness
			if len(data.Volume) > 0 {
				// Calculate expected start index
				start := tt.klineCount - 10
				if start < 0 {
					start = 0
				}

				// Verify first Volume value
				expectedFirstVolume := klines[start].Volume
				if data.Volume[0] != expectedFirstVolume {
					t.Errorf("First volume = %.2f, want %.2f", data.Volume[0], expectedFirstVolume)
				}

				// Verify last Volume value
				expectedLastVolume := klines[tt.klineCount-1].Volume
				lastVolume := data.Volume[len(data.Volume)-1]
				if lastVolume != expectedLastVolume {
					t.Errorf("Last volume = %.2f, want %.2f", lastVolume, expectedLastVolume)
				}
			}
		})
	}
}

// TestCalculateIntradaySeries_VolumeValues tests Volume value correctness
func TestCalculateIntradaySeries_VolumeValues(t *testing.T) {
	klines := []Kline{
		{Close: 100.0, Volume: 1000.0, High: 101.0, Low: 99.0, Open: 100.0},
		{Close: 101.0, Volume: 1100.0, High: 102.0, Low: 100.0, Open: 101.0},
		{Close: 102.0, Volume: 1200.0, High: 103.0, Low: 101.0, Open: 102.0},
		{Close: 103.0, Volume: 1300.0, High: 104.0, Low: 102.0, Open: 103.0},
		{Close: 104.0, Volume: 1400.0, High: 105.0, Low: 103.0, Open: 104.0},
		{Close: 105.0, Volume: 1500.0, High: 106.0, Low: 104.0, Open: 105.0},
		{Close: 106.0, Volume: 1600.0, High: 107.0, Low: 105.0, Open: 106.0},
		{Close: 107.0, Volume: 1700.0, High: 108.0, Low: 106.0, Open: 107.0},
		{Close: 108.0, Volume: 1800.0, High: 109.0, Low: 107.0, Open: 108.0},
		{Close: 109.0, Volume: 1900.0, High: 110.0, Low: 108.0, Open: 109.0},
	}

	data := calculateIntradaySeries(klines)

	expectedVolumes := []float64{1000.0, 1100.0, 1200.0, 1300.0, 1400.0, 1500.0, 1600.0, 1700.0, 1800.0, 1900.0}

	if len(data.Volume) != len(expectedVolumes) {
		t.Fatalf("Volume length = %d, want %d", len(data.Volume), len(expectedVolumes))
	}

	for i, expected := range expectedVolumes {
		if data.Volume[i] != expected {
			t.Errorf("Volume[%d] = %.2f, want %.2f", i, data.Volume[i], expected)
		}
	}
}

// TestCalculateIntradaySeries_ATR14 tests ATR14 calculation
func TestCalculateIntradaySeries_ATR14(t *testing.T) {
	tests := []struct {
		name          string
		klineCount    int
		expectZero    bool
		expectNonZero bool
	}{
		{
			name:          "Sufficient data - 20 K-lines",
			klineCount:    20,
			expectNonZero: true,
		},
		{
			name:          "Exactly 15 K-lines (ATR14 requires at least 15)",
			klineCount:    15,
			expectNonZero: true,
		},
		{
			name:       "Insufficient data - 14 K-lines",
			klineCount: 14,
			expectZero: true,
		},
		{
			name:       "Insufficient data - 10 K-lines",
			klineCount: 10,
			expectZero: true,
		},
		{
			name:       "Insufficient data - 5 K-lines",
			klineCount: 5,
			expectZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			klines := generateTestKlines(tt.klineCount)
			data := calculateIntradaySeries(klines)

			if data == nil {
				t.Fatal("calculateIntradaySeries returned nil")
			}

			if tt.expectZero && data.ATR14 != 0 {
				t.Errorf("ATR14 = %.3f, expected 0 (insufficient data)", data.ATR14)
			}

			if tt.expectNonZero && data.ATR14 <= 0 {
				t.Errorf("ATR14 = %.3f, expected > 0", data.ATR14)
			}
		})
	}
}

// TestCalculateATR tests ATR calculation function
func TestCalculateATR(t *testing.T) {
	tests := []struct {
		name       string
		klines     []Kline
		period     int
		expectZero bool
	}{
		{
			name: "Normal calculation - sufficient data",
			klines: []Kline{
				{High: 102.0, Low: 100.0, Close: 101.0},
				{High: 103.0, Low: 101.0, Close: 102.0},
				{High: 104.0, Low: 102.0, Close: 103.0},
				{High: 105.0, Low: 103.0, Close: 104.0},
				{High: 106.0, Low: 104.0, Close: 105.0},
				{High: 107.0, Low: 105.0, Close: 106.0},
				{High: 108.0, Low: 106.0, Close: 107.0},
				{High: 109.0, Low: 107.0, Close: 108.0},
				{High: 110.0, Low: 108.0, Close: 109.0},
				{High: 111.0, Low: 109.0, Close: 110.0},
				{High: 112.0, Low: 110.0, Close: 111.0},
				{High: 113.0, Low: 111.0, Close: 112.0},
				{High: 114.0, Low: 112.0, Close: 113.0},
				{High: 115.0, Low: 113.0, Close: 114.0},
				{High: 116.0, Low: 114.0, Close: 115.0},
			},
			period:     14,
			expectZero: false,
		},
		{
			name: "Insufficient data - equal to period",
			klines: []Kline{
				{High: 102.0, Low: 100.0, Close: 101.0},
				{High: 103.0, Low: 101.0, Close: 102.0},
			},
			period:     2,
			expectZero: true,
		},
		{
			name: "Insufficient data - less than period",
			klines: []Kline{
				{High: 102.0, Low: 100.0, Close: 101.0},
			},
			period:     14,
			expectZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atr := calculateATR(tt.klines, tt.period)

			if tt.expectZero {
				if atr != 0 {
					t.Errorf("calculateATR() = %.3f, expected 0 (insufficient data)", atr)
				}
			} else {
				if atr <= 0 {
					t.Errorf("calculateATR() = %.3f, expected > 0", atr)
				}
			}
		})
	}
}

// TestCalculateATR_TrueRange tests ATR True Range calculation correctness
func TestCalculateATR_TrueRange(t *testing.T) {
	// Create a simple test case, manually calculate expected ATR
	klines := []Kline{
		{High: 50.0, Low: 48.0, Close: 49.0}, // TR = 2.0
		{High: 51.0, Low: 49.0, Close: 50.0}, // TR = max(2.0, 2.0, 1.0) = 2.0
		{High: 52.0, Low: 50.0, Close: 51.0}, // TR = max(2.0, 2.0, 1.0) = 2.0
		{High: 53.0, Low: 51.0, Close: 52.0}, // TR = 2.0
		{High: 54.0, Low: 52.0, Close: 53.0}, // TR = 2.0
	}

	atr := calculateATR(klines, 3)

	// Expected calculation:
	// TR[1] = max(51-49, |51-49|, |49-49|) = 2.0
	// TR[2] = max(52-50, |52-50|, |50-50|) = 2.0
	// TR[3] = max(53-51, |53-51|, |51-51|) = 2.0
	// Initial ATR = (2.0 + 2.0 + 2.0) / 3 = 2.0
	// TR[4] = max(54-52, |54-52|, |52-52|) = 2.0
	// Smoothed ATR = (2.0*2 + 2.0) / 3 = 2.0

	expectedATR := 2.0
	tolerance := 0.01 // Allow small floating point error

	if math.Abs(atr-expectedATR) > tolerance {
		t.Errorf("calculateATR() = %.3f, want approximately %.3f", atr, expectedATR)
	}
}

// TestCalculateIntradaySeries_ConsistencyWithOtherIndicators tests Volume and other indicators consistency
func TestCalculateIntradaySeries_ConsistencyWithOtherIndicators(t *testing.T) {
	klines := generateTestKlines(30)
	data := calculateIntradaySeries(klines)

	// All arrays should exist
	if data.MidPrices == nil {
		t.Error("MidPrices should not be nil")
	}
	if data.Volume == nil {
		t.Error("Volume should not be nil")
	}

	// MidPrices and Volume should have the same length (both latest 10)
	if len(data.MidPrices) != len(data.Volume) {
		t.Errorf("MidPrices length (%d) should equal Volume length (%d)",
			len(data.MidPrices), len(data.Volume))
	}

	// All Volume values should be > 0
	for i, vol := range data.Volume {
		if vol <= 0 {
			t.Errorf("Volume[%d] = %.2f, should be > 0", i, vol)
		}
	}
}

// TestCalculateIntradaySeries_EmptyKlines tests empty K-line data
func TestCalculateIntradaySeries_EmptyKlines(t *testing.T) {
	klines := []Kline{}
	data := calculateIntradaySeries(klines)

	if data == nil {
		t.Fatal("calculateIntradaySeries should not return nil for empty klines")
	}

	// All slices should be empty
	if len(data.MidPrices) != 0 {
		t.Errorf("MidPrices length = %d, want 0", len(data.MidPrices))
	}
	if len(data.Volume) != 0 {
		t.Errorf("Volume length = %d, want 0", len(data.Volume))
	}

	// ATR14 should be 0 (insufficient data)
	if data.ATR14 != 0 {
		t.Errorf("ATR14 = %.3f, want 0", data.ATR14)
	}
}

// TestCalculateIntradaySeries_VolumePrecision tests Volume precision preservation
func TestCalculateIntradaySeries_VolumePrecision(t *testing.T) {
	klines := []Kline{
		{Close: 100.0, Volume: 1234.5678, High: 101.0, Low: 99.0},
		{Close: 101.0, Volume: 9876.5432, High: 102.0, Low: 100.0},
		{Close: 102.0, Volume: 5555.1111, High: 103.0, Low: 101.0},
	}

	data := calculateIntradaySeries(klines)

	expectedVolumes := []float64{1234.5678, 9876.5432, 5555.1111}

	for i, expected := range expectedVolumes {
		if data.Volume[i] != expected {
			t.Errorf("Volume[%d] = %.4f, want %.4f (precision not preserved)",
				i, data.Volume[i], expected)
		}
	}
}

// TestIsStaleData_NormalData tests that normal fluctuating data returns false
func TestIsStaleData_NormalData(t *testing.T) {
	klines := []Kline{
		{Close: 100.0, Volume: 1000},
		{Close: 100.5, Volume: 1200},
		{Close: 99.8, Volume: 900},
		{Close: 100.2, Volume: 1100},
		{Close: 100.1, Volume: 950},
	}

	result := isStaleData(klines, "BTCUSDT")

	if result {
		t.Error("Expected false for normal fluctuating data, got true")
	}
}

// TestIsStaleData_PriceFreezeWithZeroVolume tests that frozen price + zero volume returns true
func TestIsStaleData_PriceFreezeWithZeroVolume(t *testing.T) {
	klines := []Kline{
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
	}

	result := isStaleData(klines, "DOGEUSDT")

	if !result {
		t.Error("Expected true for frozen price + zero volume, got false")
	}
}

// TestIsStaleData_PriceFreezeWithVolume tests that frozen price but normal volume returns false
func TestIsStaleData_PriceFreezeWithVolume(t *testing.T) {
	klines := []Kline{
		{Close: 100.0, Volume: 1000},
		{Close: 100.0, Volume: 1200},
		{Close: 100.0, Volume: 900},
		{Close: 100.0, Volume: 1100},
		{Close: 100.0, Volume: 950},
	}

	result := isStaleData(klines, "STABLECOIN")

	if result {
		t.Error("Expected false for frozen price but normal volume (low volatility market), got true")
	}
}

// TestIsStaleData_InsufficientData tests that insufficient data (<5 klines) returns false
func TestIsStaleData_InsufficientData(t *testing.T) {
	klines := []Kline{
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
	}

	result := isStaleData(klines, "BTCUSDT")

	if result {
		t.Error("Expected false for insufficient data (<5 klines), got true")
	}
}

// TestIsStaleData_ExactlyFiveKlines tests edge case with exactly 5 klines
func TestIsStaleData_ExactlyFiveKlines(t *testing.T) {
	// Stale case: exactly 5 frozen klines with zero volume
	staleKlines := []Kline{
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
		{Close: 100.0, Volume: 0},
	}

	result := isStaleData(staleKlines, "TESTUSDT")
	if !result {
		t.Error("Expected true for exactly 5 frozen klines with zero volume, got false")
	}

	// Normal case: exactly 5 klines with fluctuation
	normalKlines := []Kline{
		{Close: 100.0, Volume: 1000},
		{Close: 100.1, Volume: 1100},
		{Close: 99.9, Volume: 900},
		{Close: 100.0, Volume: 1000},
		{Close: 100.05, Volume: 950},
	}

	result = isStaleData(normalKlines, "TESTUSDT")
	if result {
		t.Error("Expected false for exactly 5 normal klines, got true")
	}
}

// TestIsStaleData_WithinTolerance tests price changes within tolerance (0.01%)
func TestIsStaleData_WithinTolerance(t *testing.T) {
	// Price changes within 0.01% tolerance should be treated as frozen
	basePrice := 10000.0
	tolerance := 0.0001                        // 0.01%
	smallChange := basePrice * tolerance * 0.5 // Half of tolerance

	klines := []Kline{
		{Close: basePrice, Volume: 1000},
		{Close: basePrice + smallChange, Volume: 1000},
		{Close: basePrice - smallChange, Volume: 1000},
		{Close: basePrice, Volume: 1000},
		{Close: basePrice + smallChange, Volume: 1000},
	}

	result := isStaleData(klines, "BTCUSDT")

	// Should return false because there's normal volume despite tiny price changes
	if result {
		t.Error("Expected false for price within tolerance but with volume, got true")
	}
}

// TestIsStaleData_MixedScenario tests realistic scenario with some history before freeze
func TestIsStaleData_MixedScenario(t *testing.T) {
	// Simulate: normal trading → suddenly freezes
	klines := []Kline{
		{Close: 100.0, Volume: 1000}, // Normal
		{Close: 100.5, Volume: 1200}, // Normal
		{Close: 100.2, Volume: 1100}, // Normal
		{Close: 50.0, Volume: 0},     // Freeze starts
		{Close: 50.0, Volume: 0},     // Frozen
		{Close: 50.0, Volume: 0},     // Frozen
		{Close: 50.0, Volume: 0},     // Frozen
		{Close: 50.0, Volume: 0},     // Frozen (last 5 are all frozen)
	}

	result := isStaleData(klines, "DOGEUSDT")

	// Should detect stale data based on last 5 klines
	if !result {
		t.Error("Expected true for frozen last 5 klines with zero volume, got false")
	}
}

// TestIsStaleData_EmptyKlines tests edge case with empty slice
func TestIsStaleData_EmptyKlines(t *testing.T) {
	klines := []Kline{}

	result := isStaleData(klines, "BTCUSDT")

	if result {
		t.Error("Expected false for empty klines, got true")
	}
}

func TestCalculateDonchian(t *testing.T) {
	// Create test klines with known high/low values
	klines := []Kline{
		{High: 100, Low: 90},
		{High: 105, Low: 88},
		{High: 102, Low: 92},
		{High: 108, Low: 85},
		{High: 103, Low: 91},
	}

	upper, lower := ExportCalculateDonchian(klines, 5)

	if upper != 108 {
		t.Errorf("Expected upper = 108, got %v", upper)
	}
	if lower != 85 {
		t.Errorf("Expected lower = 85, got %v", lower)
	}
}

func TestCalculateDonchian_PartialPeriod(t *testing.T) {
	klines := []Kline{
		{High: 100, Low: 90},
		{High: 105, Low: 88},
	}

	upper, lower := ExportCalculateDonchian(klines, 10)

	// Should use all available klines when period > len(klines)
	if upper != 105 {
		t.Errorf("Expected upper = 105, got %v", upper)
	}
	if lower != 88 {
		t.Errorf("Expected lower = 88, got %v", lower)
	}
}

func TestCalculateDonchian_InvalidPeriod(t *testing.T) {
	klines := []Kline{
		{High: 100, Low: 90},
	}

	// Zero period should return (0, 0)
	upper, lower := ExportCalculateDonchian(klines, 0)
	if upper != 0 || lower != 0 {
		t.Errorf("Expected (0, 0) for zero period, got (%v, %v)", upper, lower)
	}

	// Negative period should return (0, 0)
	upper, lower = ExportCalculateDonchian(klines, -1)
	if upper != 0 || lower != 0 {
		t.Errorf("Expected (0, 0) for negative period, got (%v, %v)", upper, lower)
	}
}

func TestCalculateBoxData(t *testing.T) {
	// Create synthetic kline data
	klines := make([]Kline, 500)
	for i := 0; i < 500; i++ {
		basePrice := 100.0
		klines[i] = Kline{
			High:  basePrice + float64(i%10),
			Low:   basePrice - float64(i%10),
			Close: basePrice,
		}
	}

	box := ExportCalculateBoxData(klines, 100.0)

	if box.ShortUpper == 0 || box.ShortLower == 0 {
		t.Error("Short box should not be zero")
	}
	if box.MidUpper == 0 || box.MidLower == 0 {
		t.Error("Mid box should not be zero")
	}
	if box.LongUpper == 0 || box.LongLower == 0 {
		t.Error("Long box should not be zero")
	}
	if box.CurrentPrice != 100.0 {
		t.Errorf("Expected CurrentPrice = 100.0, got %v", box.CurrentPrice)
	}
}

// The forming candle of each timeframe must be refreshed with the live ticker
// price so every prompt section quotes the same "current price". Regression
// for the narrative-vs-JSON systematic price lag: the kline vendor's forming
// candle close sits frozen for a long time, so the daily narrative quoted
// prices 4-7% behind the structured signal JSON on trending days.
func TestRefreshFormingCandle(t *testing.T) {
	now := time.Now()

	sd := &TimeframeSeriesData{Timeframe: "1d"}
	// Closed candle yesterday (must never be touched).
	closedStart := now.Add(-25 * time.Hour)
	sd.Klines = append(sd.Klines, KlineBar{Time: closedStart.UnixMilli(), Open: 98, High: 101, Low: 97, Close: 100, Volume: 10})
	sd.MidPrices = append(sd.MidPrices, 99)
	// Forming candle today with a vendor close frozen at 100.
	formingStart := now.Add(-30 * time.Minute)
	sd.Klines = append(sd.Klines, KlineBar{Time: formingStart.UnixMilli(), Open: 100, High: 102, Low: 99, Close: 100, Volume: 5})
	sd.MidPrices = append(sd.MidPrices, 100.5)

	refreshFormingCandle("1d", sd, 107)

	closed := sd.Klines[0]
	if closed.Close != 100 || closed.High != 101 {
		t.Fatalf("closed candle must be untouched, got close=%.2f high=%.2f", closed.Close, closed.High)
	}
	forming := sd.Klines[1]
	if forming.Close != 107 {
		t.Fatalf("forming candle close = %.2f, want 107 (live price)", forming.Close)
	}
	if forming.High != 107 || forming.Low != 99 {
		t.Fatalf("forming candle High/Low not widened correctly: H=%.2f L=%.2f", forming.High, forming.Low)
	}
	if mid := sd.MidPrices[1]; mid != (107+99)/2 {
		t.Fatalf("MidPrices not realigned: %.2f", mid)
	}

	// Implausible ticker (>±80% from candle close) → no patch.
	sd2 := &TimeframeSeriesData{Timeframe: "1d", Klines: []KlineBar{{Time: formingStart.UnixMilli(), Open: 100, High: 102, Low: 99, Close: 100}}}
	refreshFormingCandle("1d", sd2, 600)
	if sd2.Klines[0].Close != 100 {
		t.Fatalf("implausible ticker must not patch, got close=%.2f", sd2.Klines[0].Close)
	}

	// Extreme but plausible mover (+80% meme pump) → must be patched, this is
	// exactly where the frozen vendor close is most misleading.
	sd5 := &TimeframeSeriesData{Timeframe: "1d", Klines: []KlineBar{{Time: formingStart.UnixMilli(), Open: 100, High: 130, Low: 99, Close: 110}}}
	refreshFormingCandle("1d", sd5, 180)
	if sd5.Klines[0].Close != 180 || sd5.Klines[0].High != 180 {
		t.Fatalf("extreme mover must be patched, got close=%.2f high=%.2f", sd5.Klines[0].Close, sd5.Klines[0].High)
	}

	// Unknown timeframe label → no patch (duration table returns 0).
	sd3 := &TimeframeSeriesData{Timeframe: "3d", Klines: []KlineBar{{Time: formingStart.UnixMilli(), Open: 100, High: 102, Low: 99, Close: 100}}}
	refreshFormingCandle("3d", sd3, 107)
	if sd3.Klines[0].Close != 100 {
		t.Fatalf("unknown timeframe must not patch, got close=%.2f", sd3.Klines[0].Close)
	}

	// Downward move: Close lowered, Low widened, High untouched.
	sd4 := &TimeframeSeriesData{Timeframe: "1d", Klines: []KlineBar{{Time: formingStart.UnixMilli(), Open: 100, High: 102, Low: 99, Close: 100}}}
	refreshFormingCandle("1d", sd4, 95)
	if sd4.Klines[0].Close != 95 || sd4.Klines[0].Low != 95 || sd4.Klines[0].High != 102 {
		t.Fatalf("downward patch wrong: %+v", sd4.Klines[0])
	}
}

// GetWithExchange must populate TimeframeData — every execution-side
// volatility yardstick (stop-band floor 1.5×ATR(1h), cap 2×ATR(4h),
// vol-target/trailing) reads it. It used to be left nil, so all of those
// gates silently ran on ATR=0: the cap degenerated to the fixed 8% and
// rejected healthy wide stops (AKEUSDT 09-16) while the floor never
// enforced at all.
func TestExecutionTimeframeData(t *testing.T) {
	mk := func(n int, base float64) []Kline {
		ks := make([]Kline, n)
		p := base
		for i := range ks {
			p *= 1.003
			ks[i] = Kline{OpenTime: int64(i), Open: p * 0.999, High: p * 1.002, Low: p * 0.997, Close: p, Volume: 10}
		}
		return ks
	}

	tf := executionTimeframeData(mk(100, 2.4), mk(100, 2.4), mk(100, 2.4))
	for _, key := range []string{"3m", "1h", "4h"} {
		if tf[key] == nil || len(tf[key].Klines) == 0 {
			t.Fatalf("timeframe %q missing from execution data", key)
		}
	}

	// 1h fetch failed (best-effort): floor's ATR chain rides 4h instead.
	tf = executionTimeframeData(mk(100, 2.4), nil, mk(100, 2.4))
	if tf["1h"] != nil {
		t.Fatal("1h must be absent when its fetch failed")
	}
	if tf["3m"] == nil || tf["4h"] == nil {
		t.Fatal("3m/4h must always be present")
	}
}

// 3m→15m aggregation must preserve OHLC extremes and sum volume per bucket —
// the execution-side dataset's 15m series is built this way (zero extra
// vendor calls), and the supply-zone breathing gate reads its ATR.
func TestAggregateKlines15m(t *testing.T) {
	base := time.Date(2026, 9, 19, 13, 0, 0, 0, time.UTC)
	var src []Kline
	// Bucket 13:00: bars at 0,3,6,9,12 min. Open=first, Close=last,
	// High/Low = extremes, Volume summed.
	mk := func(mins int, o, h, l, c, v float64) Kline {
		return Kline{OpenTime: base.Add(time.Duration(mins) * time.Minute).UnixMilli(),
			Open: o, High: h, Low: l, Close: c, Volume: v}
	}
	src = append(src,
		mk(0, 100, 102, 99, 101, 3),
		mk(3, 101, 104, 100, 103, 4),
		mk(6, 103, 103.5, 98, 99, 5),
		mk(9, 99, 101, 97, 100, 6),
		mk(12, 100, 105, 100, 104, 7),
		// Bucket 13:15: single bar
		mk(15, 104, 106, 103, 105, 10),
	)
	got := aggregateKlines(src, 15*time.Minute)
	if len(got) != 2 {
		t.Fatalf("buckets = %d, want 2", len(got))
	}
	b0, b1 := got[0], got[1]
	if b0.Open != 100 || b0.Close != 104 || b0.High != 105 || b0.Low != 97 {
		t.Fatalf("bucket0 OHLC = %v/%v/%v/%v, want 100/105/97/104", b0.Open, b0.High, b0.Low, b0.Close)
	}
	if b0.Volume != 25 {
		t.Fatalf("bucket0 volume = %v, want 25", b0.Volume)
	}
	if b0.OpenTime != base.UnixMilli() || b1.OpenTime != base.Add(15*time.Minute).UnixMilli() {
		t.Fatalf("bucket OpenTimes not aligned to 15m boundaries: %v / %v", b0.OpenTime, b1.OpenTime)
	}
	if b1.Open != 104 || b1.High != 106 || b1.Volume != 10 {
		t.Fatalf("bucket1 = %v", b1)
	}
	if aggregateKlines(nil, 15*time.Minute) != nil {
		t.Fatal("empty input must yield nil")
	}
}

// The execution-side dataset must carry a 15m series (aggregated from 3m) —
// without it ExecutionATRPct used to fall back to the 4h ATR and the
// supply-zone gate ran a ~8× inflated threshold (2026-09-19 BTCUSDT loop).
func TestExecutionTimeframeDataHas15m(t *testing.T) {
	base := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	var k3m []Kline
	for i := 0; i < 100; i++ {
		p := 81000 + float64(i%10)*2
		k3m = append(k3m, Kline{
			OpenTime: base.Add(time.Duration(i) * 3 * time.Minute).UnixMilli(),
			Open:     p, High: p * 1.0015, Low: p * 0.9985, Close: p * 1.0002, Volume: 10,
		})
	}
	var k4h []Kline
	for i := 0; i < 100; i++ {
		p := 80000 + float64(i)*5
		k4h = append(k4h, Kline{
			OpenTime: base.Add(time.Duration(i) * 4 * time.Hour).UnixMilli(),
			Open:     p, High: p * 1.02, Low: p * 0.98, Close: p, Volume: 100,
		})
	}
	tfd := executionTimeframeData(k3m, nil, k4h)
	k15, ok := tfd["15m"]
	if !ok {
		t.Fatal("15m series missing from execution dataset")
	}
	// 100×3m bars span exactly 20 15m buckets.
	if len(k15.Klines) != 20 {
		t.Fatalf("15m bars = %d, want 20", len(k15.Klines))
	}
	// Last bucket = the trailing forming one (13:45+ open beyond the series)
	// stays in place; the kernel's settlement logic drops it natively.
	if last := k15.Klines[len(k15.Klines)-1]; last.Open <= 0 || last.High < last.Low {
		t.Fatalf("last 15m bucket malformed: %+v", last)
	}
}
