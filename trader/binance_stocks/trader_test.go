package binance_stocks

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The adapter has no account-wide position endpoint: GetPositions must fail
// instead of reporting a successful empty (flat) account.
func TestGetPositionsFailsInsteadOfReportingFlatAccount(t *testing.T) {
	tr := &StocksTrader{}
	pos, err := tr.GetPositions()
	if err == nil {
		t.Fatalf("GetPositions returned success with %v; want an error", pos)
	}
}

// stocksServer fakes the three endpoints GetBalance touches. quotes maps a
// ticker to its price string; a missing ticker answers 404.
func stocksServer(t *testing.T, balances string, quotes map[string]string) *StocksTrader {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/sapi/v3/account", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"balances":` + balances + `}`))
	})
	mux.HandleFunc("/sapi/v1/equity/market/exchangeInfo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"symbols":[{"symbol":"AAPL"},{"symbol":"NVDA"}]}`))
	})
	mux.HandleFunc("/sapi/v1/equity/market/quote", func(w http.ResponseWriter, r *http.Request) {
		px, ok := quotes[r.URL.Query().Get("symbol")]
		if !ok {
			http.Error(w, `{"code":-1121,"msg":"no quote"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"symbol":"x","price":"` + px + `"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("BINANCE_STOCKS_BASE", srv.URL)
	return NewStocksTrader("k", "s")
}

// Stock holdings must count at market value, otherwise buying shares looks
// like a drawdown (cash drops, equity does not come back).
func TestGetBalanceIncludesStockHoldingsAtMarketValue(t *testing.T) {
	tr := stocksServer(t,
		`[{"asset":"USDT","free":"100","locked":"20"},{"asset":"AAPL","free":"2","locked":"1"},{"asset":"BTC","free":"5","locked":"0"}]`,
		map[string]string{"AAPL": "150"})
	bal, err := tr.GetBalance()
	if err != nil {
		t.Fatal(err)
	}
	// 120 cash + 3*150 holdings; BTC is not an equity and is ignored.
	if got := bal["totalEquity"].(float64); got != 570 {
		t.Fatalf("totalEquity %v, want 570", got)
	}
	if got := bal["availableBalance"].(float64); got != 100 {
		t.Fatalf("availableBalance %v, want 100", got)
	}
}

// A missing quote must be an error, not an understated equity.
func TestGetBalanceErrorsWhenAHoldingCannotBeValued(t *testing.T) {
	tr := stocksServer(t,
		`[{"asset":"USDT","free":"100","locked":"0"},{"asset":"NVDA","free":"4","locked":"0"}]`,
		map[string]string{})
	bal, err := tr.GetBalance()
	if err == nil {
		t.Fatalf("GetBalance succeeded with %v; want an error when the quote is unavailable", bal)
	}
	if !strings.Contains(err.Error(), "NVDA") {
		t.Fatalf("error should name the holding: %v", err)
	}
}

// Cash-only accounts need no equity lookups.
func TestGetBalanceCashOnly(t *testing.T) {
	tr := stocksServer(t, `[{"asset":"USDC","free":"50","locked":"5"}]`, nil)
	bal, err := tr.GetBalance()
	if err != nil || bal["totalEquity"].(float64) != 55 {
		t.Fatalf("cash-only: %v %v", bal, err)
	}
}
