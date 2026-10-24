package coinank_api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"nofx/provider/coinank"
	"nofx/provider/coinank/coinank_enum"
)

type klineRoundTripFunc func(*http.Request) (*http.Response, error)

func (f klineRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestC2KlineRejectsInvalidResponse(t *testing.T) {
	for _, data := range []string{`[[]]`, `[[1,2,3,4,5,6,7,8]]`,
		`[[2,3,100,101,102,99,1,1,1],[1,2,100,101,102,99,1,1,1]]`,
		`[[1,2,100,101,102,99,1,1,1],[1,2,100,101,102,99,1,1,1]]`,
		`[[1,2,1e309,101,102,99,1,1,1]]`, `[[2,1,100,101,102,99,1,1,1]]`} {
		t.Run(data, func(t *testing.T) {
			old := client
			client = &http.Client{Transport: klineRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"data":` + data + `}`))}, nil
			})}
			t.Cleanup(func() { client = old })
			rows, err := Kline(context.Background(), "BTCUSDT", coinank_enum.Binance, 1000, coinank_enum.To, 2, coinank_enum.Hour1)
			if !errors.Is(err, coinank.ErrInvalidKlineData) || rows != nil {
				t.Fatalf("invalid rows accepted: rows=%v err=%v", rows, err)
			}
		})
	}
}

func TestC2KlineWebsocketSkipsShortRows(t *testing.T) {
	input := make(chan string, 3)
	input <- `{"op":"push","success":true,"args":"kline:BTCUSDT","data":[]}`
	input <- `{"op":"push","success":true,"args":"kline:BTCUSDT","data":[1,2,3,4,5,6,7,8]}`
	input <- `{"op":"push","success":true,"args":"kline:BTCUSDT","data":[1,2,100,101,102,99,10,20,3]}`
	close(input)
	klines, _ := handleResponse(input, true, false)
	count := 0
	for row := range klines {
		count++
		if row.Data.Open != 100 || row.Data.StartTime != 1 {
			t.Fatalf("valid row changed: %+v", row.Data)
		}
	}
	if count != 1 {
		t.Fatalf("got %d rows, want only the valid row", count)
	}
}
