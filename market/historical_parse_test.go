package market

import (
	"math"
	"testing"
)

func TestMalformedHistoricalKlinesDoNotPanic(t *testing.T) {
	for _, row := range [][]interface{}{{}, {1.0}, {1.0, "bad", "2", "1", "1", "1", 2.0}, {1.0, "1", "2", "1", "1", "1", math.NaN()}} {
		if _, err := parseHistoricalKline(row); err == nil {
			t.Fatalf("accepted malformed %v", row)
		}
	}
	if _, err := parseHistoricalKline([]interface{}{1.0, "1", "2", "1", "1", "10", 2.0}); err != nil {
		t.Fatal(err)
	}
}
