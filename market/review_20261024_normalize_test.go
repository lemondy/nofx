package market

import "testing"

func TestReviewC1NormalizeWhitespace(t *testing.T) {
	for raw, want := range map[string]string{
		" BTCUSDT \t": "BTCUSDT", "\neth ": "ETHUSDT",
		" BTC-USDT-SWAP ": "BTCUSDT", " kPEPE ": "KPEPEUSDT",
		" 1000PEPE ": "1000PEPEUSDT", " xyz:TSLA ": "xyz:TSLA",
	} {
		if got := Normalize(raw); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", raw, got, want)
		}
	}
}
