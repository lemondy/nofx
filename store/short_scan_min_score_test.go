package store

import (
	"encoding/json"
	"testing"
)

func TestEffectiveShortScanMinScore(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, DefaultShortScanMinScore},  // unset → built-in default
		{-5, DefaultShortScanMinScore}, // nonsense → default
		{40, 40},
		{55, 55},
		{70.5, 70.5},
		{150, 100}, // capped
	}
	for _, c := range cases {
		cs := CoinSourceConfig{ShortScanMinScore: c.in}
		if got := cs.EffectiveShortScanMinScore(); got != c.want {
			t.Errorf("ShortScanMinScore=%v: got %v, want %v", c.in, got, c.want)
		}
	}
}

// The field must round-trip under the JSON name the UI writes, and an old
// config without it must resolve to the default (no behavior change).
func TestShortScanMinScoreJSON(t *testing.T) {
	var cs CoinSourceConfig
	if err := json.Unmarshal([]byte(`{"use_short_scan":true,"short_scan_min_score":45}`), &cs); err != nil {
		t.Fatal(err)
	}
	if cs.EffectiveShortScanMinScore() != 45 {
		t.Fatalf("got %v, want 45", cs.EffectiveShortScanMinScore())
	}
	var old CoinSourceConfig
	if err := json.Unmarshal([]byte(`{"use_short_scan":true,"short_scan_limit":8}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.EffectiveShortScanMinScore() != DefaultShortScanMinScore {
		t.Fatalf("legacy config: got %v, want default %v", old.EffectiveShortScanMinScore(), DefaultShortScanMinScore)
	}
}
