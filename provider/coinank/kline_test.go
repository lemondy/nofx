package coinank

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"nofx/provider/coinank/coinank_enum"
)

func c2KlineRow(start, end float64) []float64 {
	return []float64{start, end, 100, 101, 102, 99, 10, 20, 3}
}

func TestC2ParseKlineRowsValidation(t *testing.T) {
	for _, rows := range [][][]float64{
		{nil}, {{1, 2}}, {c2KlineRow(2, 1)}, {c2KlineRow(-1, 1)}, {c2KlineRow(1, math.MaxFloat64)},
		{c2KlineRow(1, 2), c2KlineRow(1, 3)}, {c2KlineRow(2, 3), c2KlineRow(1, 2)},
		{c2KlineRow(1, 4), c2KlineRow(2, 3)},
	} {
		if got, err := ParseKlineRows(rows); got != nil || !errors.Is(err, ErrInvalidKlineData) {
			t.Errorf("invalid rows accepted: rows=%v got=%v err=%v", rows, got, err)
		}
	}
	for column := 0; column < 9; column++ {
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			row := c2KlineRow(1, 2)
			row[column] = value
			if got, err := ParseKlineRows([][]float64{row}); got != nil || !errors.Is(err, ErrInvalidKlineData) {
				t.Errorf("column %d accepted non-finite value: got=%v err=%v", column, got, err)
			}
		}
	}
	rows, err := ParseKlineRows([][]float64{append(c2KlineRow(1, 2), 123), c2KlineRow(3, 4)})
	if err != nil || len(rows) != 2 || rows[0] != (KlineResult{StartTime: 1, EndTime: 2, Open: 100, Close: 101, High: 102, Low: 99, Volume: 10, Quantity: 20, Count: 3}) {
		t.Fatalf("valid row mapping changed: %v %v", rows, err)
	}
}

type c2RoundTripFunc func(*http.Request) (*http.Response, error)

func (f c2RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestC2AuthenticatedKlineRejectsShortRows(t *testing.T) {
	old := client
	client = &http.Client{Transport: c2RoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"data":[[]]}`))}, nil
	})}
	t.Cleanup(func() { client = old })
	c := NewCoinankClient("https://offline.test", "test")
	rows, err := c.Kline(context.Background(), "BTCUSDT", coinank_enum.Binance, 0, 10, 2, coinank_enum.Hour1)
	if rows != nil || !errors.Is(err, ErrInvalidKlineData) {
		t.Fatalf("invalid response accepted: %v %v", rows, err)
	}
}
