package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"nofx/trader/types"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

type fundsRoundTrip func(*http.Request) (*http.Response, error)

func (f fundsRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func incomeTrader(t *testing.T, rows []*futures.IncomeHistory) (*FuturesTrader, *int) {
	t.Helper()
	calls := 0
	c := futures.NewClient("key", "secret")
	c.HTTPClient = &http.Client{Transport: fundsRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/fapi/v1/income" || q.Has("incomeType") || q.Get("limit") != "1000" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		lo, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
		hi, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
		out := []*futures.IncomeHistory{}
		for _, row := range rows {
			if row.Time >= lo && row.Time <= hi {
				out = append(out, row)
				if len(out) == 1000 {
					break
				}
			}
		}
		body, _ := json.Marshal(out)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	return &FuturesTrader{client: c}, &calls
}
func TestFundsIncomeAllTypesPaginationKeepsSameMillisecond(t *testing.T) {
	rows := make([]*futures.IncomeHistory, 0, 1001)
	for i := 0; i < 1001; i++ {
		at := int64(100)
		if i >= 999 {
			at = 200
		}
		typ := []string{"TRANSFER", "REALIZED_PNL", "COMMISSION", "FUNDING_FEE", "API_REBATE"}[i%5]
		rows = append(rows, &futures.IncomeHistory{TranID: int64(i + 1), IncomeType: typ, Asset: "USDT", Income: "1", Time: at, Info: "retained"})
	}
	tr, calls := incomeTrader(t, rows)
	out, err := tr.ReadIncomeHistory(context.Background(), time.UnixMilli(0), time.UnixMilli(300))
	if err != nil || len(out) != 1001 || *calls < 3 {
		t.Fatalf("len=%d calls=%d err=%v", len(out), *calls, err)
	}
	seen := map[string]bool{}
	for _, r := range out {
		if seen[r.TranID] {
			t.Fatal("duplicated income")
		}
		seen[r.TranID] = true
		if r.Info != "retained" {
			t.Fatal(r)
		}
	}
}
func TestFundsIncomeSaturatedMillisecondIsExplicitFailure(t *testing.T) {
	rows := make([]*futures.IncomeHistory, 1000)
	for i := range rows {
		rows[i] = &futures.IncomeHistory{TranID: int64(i), IncomeType: "COMMISSION", Asset: "USDT", Income: "-1", Time: 100}
	}
	tr, _ := incomeTrader(t, rows)
	_, err := tr.ReadIncomeHistory(context.Background(), time.UnixMilli(100), time.UnixMilli(100))
	if !errors.Is(err, types.ErrIncomeHistoryUnavailable) {
		t.Fatal(err)
	}
}
func TestFundsIncomeInvalidAmountAndAPIError(t *testing.T) {
	tr, _ := incomeTrader(t, []*futures.IncomeHistory{{TranID: 1, IncomeType: "TRANSFER", Income: "NaN", Time: 100}})
	if _, err := tr.ReadIncomeHistory(context.Background(), time.UnixMilli(0), time.UnixMilli(300)); !errors.Is(err, types.ErrIncomeHistoryUnavailable) {
		t.Fatal(err)
	}
	tr.client.HTTPClient.Transport = fundsRoundTrip(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("permission denied") })
	if _, err := tr.ReadIncomeHistory(context.Background(), time.UnixMilli(0), time.UnixMilli(300)); !errors.Is(err, types.ErrIncomeHistoryUnavailable) {
		t.Fatal(err)
	}
}
