package market

import (
	"encoding/json"
	"testing"
)

// Review 2026-10-28 #17: the full fetched history rides along with the display
// window for indicator warm-up, and never leaks into serialized output.
func TestReview20261028IndicatorKlinesKeptButNotSerialized(t *testing.T) {
	klines := make([]Kline, 200)
	for i := range klines {
		p := 100 + float64(i)
		klines[i] = Kline{OpenTime: int64(i) * 3600_000, Open: p, High: p + 1, Low: p - 1, Close: p, Volume: 1}
	}

	td := calculateTimeframeSeries(klines, "1h", 60)
	if len(td.Klines) != 60 || len(td.IndicatorKlines) != 200 {
		t.Fatalf("display=%d indicator=%d, want 60/200", len(td.Klines), len(td.IndicatorKlines))
	}
	if td.IndicatorKlines[199] != td.Klines[59] || td.IndicatorKlines[0].Time != klines[0].OpenTime {
		t.Fatalf("indicator history must be the full series ending at the display tail")
	}
	raw, err := json.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for k := range back {
		if k == "IndicatorKlines" || k == "indicator_klines" {
			t.Fatalf("IndicatorKlines leaked into JSON: %s", raw)
		}
	}
	if len(back["klines"].([]any)) != 60 {
		t.Fatalf("display window changed in JSON")
	}

	// Display already is the full history: nothing extra is kept.
	if full := calculateTimeframeSeries(klines, "1h", 200); full.IndicatorKlines != nil {
		t.Fatalf("count>=len must not duplicate the history")
	}
}
