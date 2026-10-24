package coinank_api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"nofx/provider/coinank"
	"nofx/provider/coinank/coinank_enum"
	"nofx/security"
	"strconv"
	"time"
)

const MainApiUrl = "https://api.coinank.com"

// Kline open free kline from coinank
func Kline(ctx context.Context, symbol string, exchange coinank_enum.Exchange, ts int64, side coinank_enum.Side, size int,
	interval coinank_enum.Interval) ([]coinank.KlineResult, error) {
	paramsMap := make(map[string]string, 6)
	paramsMap["symbol"] = symbol
	paramsMap["exchange"] = string(exchange)
	paramsMap["side"] = string(side)
	paramsMap["size"] = strconv.Itoa(size)
	paramsMap["ts"] = strconv.FormatInt(ts, 10)
	paramsMap["interval"] = string(interval)
	resp, err := get(ctx, "/api/kline/list/open", paramsMap)
	if err != nil {
		return nil, err
	}
	var result coinank.CoinankResponse[[][]float64]
	err = json.Unmarshal([]byte(resp), &result)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", coinank.ErrInvalidKlineData, err)
	}
	if !result.Success {
		return nil, coinank.HttpError
	}
	return coinank.ParseKlineRows(result.Data)
}

func get(ctx context.Context, path string, paramsMap map[string]string) (string, error) {
	data := url.Values{}
	for key, value := range paramsMap {
		data.Add(key, value)
	}
	fullURL := fmt.Sprintf("%s%s?%s", MainApiUrl, path, data.Encode())
	request, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := security.ReadResponseBody(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

var client = &http.Client{
	Timeout: 30 * time.Second,
}
