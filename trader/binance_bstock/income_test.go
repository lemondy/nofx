package binance_bstock

import (
	"context"
	"errors"
	"fmt"
	"nofx/trader/types"
	"strconv"
	"testing"
	"time"
)

func TestFundsSpotIncomeGETOnlyWalletScopeAndPagination(t *testing.T) {
	tr, f, clock := fixture(t)
	when := clock.UnixMilli()
	depositPages := 0
	transferPages := 0
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.method != "GET" {
			t.Fatalf("history called %s %s", r.method, r.path)
		}
		switch r.path {
		case "/sapi/v1/capital/deposit/hisrec":
			depositPages++
			if r.params.Get("limit") != "1000" {
				t.Fatal(r.params)
			}
			rows := []map[string]interface{}{}
			offset, _ := strconv.Atoi(r.params.Get("offset"))
			n := 1000
			if offset == 1000 {
				n = 1
			}
			for i := 0; i < n; i++ {
				wallet := 0
				if offset == 1000 {
					wallet = 1
				}
				rows = append(rows, map[string]interface{}{"amount": "1", "coin": "USDT", "network": "ETH", "txId": fmt.Sprint(offset + i), "status": 1, "walletType": wallet, "insertTime": when})
			}
			return rows, 200, true
		case "/sapi/v1/capital/withdraw/history":
			return []map[string]interface{}{{"id": "spot", "amount": "10", "transactionFee": ".1", "coin": "USDT", "status": 6, "walletType": 0, "completeTime": clock.UTC().Format("2006-01-02 15:04:05")}, {"id": "funding", "amount": "1000", "coin": "USDT", "status": 6, "walletType": 1}}, 200, true
		case "/sapi/v1/asset/transfer":
			if r.params.Get("size") != "100" {
				t.Fatal(r.params)
			}
			direction := r.params.Get("type")
			rows := []map[string]interface{}{}
			total := 0
			if direction == "MAIN_UMFUTURE" {
				transferPages++
				total = 101
				page, _ := strconv.Atoi(r.params.Get("current"))
				n := 100
				if page == 2 {
					n = 1
				}
				for i := 0; i < n; i++ {
					rows = append(rows, map[string]interface{}{"asset": "USDT", "amount": "1", "status": "CONFIRMED", "tranId": page*100 + i, "timestamp": when})
				}
			}
			if direction == "UMFUTURE_MAIN" {
				total = 1
				rows = append(rows, map[string]interface{}{"asset": "USDC", "amount": "30", "status": "CONFIRMED", "tranId": 9999, "timestamp": when})
			}
			return map[string]interface{}{"total": total, "rows": rows}, 200, true
		}
		t.Fatalf("unexpected endpoint %s", r.path)
		return nil, 0, false
	}
	out, err := tr.ReadIncomeHistory(context.Background(), clock.Add(-24*time.Hour), *clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1103 || depositPages != 2 || transferPages != 2 {
		t.Fatalf("rows %d pages %d/%d", len(out), depositPages, transferPages)
	}
	sum := 0.0
	for _, r := range out {
		sum += r.Amount
	}
	near(t, sum, 1000-10.1-101+30)
}
func TestFundsSpotPartialPermissionAndMissingWalletAreRecognizable(t *testing.T) {
	for _, missingWallet := range []bool{false, true} {
		t.Run(fmt.Sprint(missingWallet), func(t *testing.T) {
			tr, f, clock := fixture(t)
			f.handle = func(r requestRecord) (interface{}, int, bool) {
				if r.method != "GET" {
					t.Fatal(r)
				}
				switch r.path {
				case "/sapi/v1/capital/deposit/hisrec":
					row := map[string]interface{}{"amount": "500", "coin": "USDT", "txId": "deposit", "status": 1, "insertTime": clock.UnixMilli()}
					if !missingWallet {
						row["walletType"] = 0
					}
					return []map[string]interface{}{row}, 200, true
				case "/sapi/v1/capital/withdraw/history":
					return map[string]interface{}{"code": -2015, "msg": "Invalid API-key, IP, or permissions for action."}, 401, true
				case "/sapi/v1/asset/transfer":
					return map[string]interface{}{"total": 0, "rows": []interface{}{}}, 200, true
				}
				return nil, 0, false
			}
			out, err := tr.ReadIncomeHistory(context.Background(), clock.Add(-time.Hour), *clock)
			if !errors.Is(err, types.ErrIncomeHistoryUnavailable) {
				t.Fatal(err)
			}
			want := 1
			if missingWallet {
				want = 0
			}
			if len(out) != want {
				t.Fatal(out)
			}
		})
	}
}
