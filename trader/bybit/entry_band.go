package bybit

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"nofx/security"
)

// GetSymbolTickSize reads the same public instrument endpoint as getQtyStep.
// It is requested only for a new band entry, after the market risk gates.
func (t *BybitTrader) GetSymbolTickSize(symbol string) (float64, error) {
	resp, err := bybitHTTP.Get("https://api.bybit.com/v5/market/instruments-info?category=linear&symbol=" + url.QueryEscape(symbol))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := security.ReadResponseBody(resp.Body)
	if err != nil {
		return 0, err
	}
	var result struct {
		RetCode int `json:"retCode"`
		Result  struct {
			List []struct {
				PriceFilter struct {
					TickSize string `json:"tickSize"`
				} `json:"priceFilter"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}
	if resp.StatusCode != 200 || result.RetCode != 0 || len(result.Result.List) == 0 {
		return 0, fmt.Errorf("%s price tick unavailable", symbol)
	}
	return strconv.ParseFloat(result.Result.List[0].PriceFilter.TickSize, 64)
}
