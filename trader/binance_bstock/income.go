package binance_bstock

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"nofx/fundsledger"
	"nofx/trader/types"
	"strconv"
	"strings"
	"time"

	binance "github.com/adshao/go-binance/v2"
)

var _ types.IncomeHistoryReader = (*BStockTrader)(nil)

// SDK v2.8.9 has all three history services, but its capital response structs
// drop walletType. Preserve this field with a narrow signed GET, otherwise
// funding-wallet deposits/withdrawals would be misreported as spot transfers.
type depositIncome struct {
	binance.Deposit
	WalletType   *int  `json:"walletType"`
	CompleteTime int64 `json:"completeTime"`
}
type withdrawIncome struct {
	binance.Withdraw
	WalletType *int `json:"walletType"`
}

func incomeAmount(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("invalid history amount")
	}
	return v, nil
}

// ReadIncomeHistory only queries history GETs. Successful subsets are returned
// with ErrIncomeHistoryUnavailable when another endpoint or asset fails.
func (t *BStockTrader) ReadIncomeHistory(ctx context.Context, start, end time.Time) ([]fundsledger.Income, error) {
	out := []fundsledger.Income{}
	var failures []error
	fail := func(op string, e error) { failures = append(failures, fmt.Errorf("%s: %w", op, e)) }
	// A strict sub-90-day window satisfies deposit/withdraw history limits.
	for lo := start.UnixMilli(); lo <= end.UnixMilli(); {
		hi := min(lo+89*24*60*60*1000-1, end.UnixMilli())
		for offset := 0; ; offset += 1000 {
			var rows []depositIncome
			p := url.Values{"startTime": {id(lo)}, "endTime": {id(hi)}, "limit": {"1000"}, "offset": {strconv.Itoa(offset)}}
			if err := t.signedContext(ctx, http.MethodGet, "/sapi/v1/capital/deposit/hisrec", p, &rows); err != nil {
				fail("deposit history", err)
				break
			}
			for _, r := range rows {
				if r.Status != 1 {
					continue
				}
				if r.WalletType == nil {
					fail("deposit history", fmt.Errorf("walletType absent, skipped"))
					continue
				}
				if *r.WalletType != 0 {
					continue
				}
				amount, err := incomeAmount(r.Amount)
				if err != nil {
					fail("deposit history", err)
					continue
				}
				at := r.InsertTime
				if r.CompleteTime > 0 {
					at = r.CompleteTime
				}
				out = append(out, fundsledger.Income{TranID: "deposit/" + r.Coin + "/" + r.Network + "/" + r.TxID, IncomeType: "DEPOSIT", Asset: r.Coin, Amount: amount, TimeMS: at, Info: "现货钱包充值"})
			}
			if len(rows) < 1000 {
				break
			}
			if err := ctx.Err(); err != nil {
				return out, fmt.Errorf("%w: %w", types.ErrIncomeHistoryUnavailable, err)
			}
		}
		for offset := 0; ; offset += 1000 {
			var rows []withdrawIncome
			p := url.Values{"startTime": {id(lo)}, "endTime": {id(hi)}, "limit": {"1000"}, "offset": {strconv.Itoa(offset)}}
			if err := t.signedContext(ctx, http.MethodGet, "/sapi/v1/capital/withdraw/history", p, &rows); err != nil {
				fail("withdraw history", err)
				break
			}
			for _, r := range rows {
				if r.Status != 6 {
					continue
				}
				if r.WalletType == nil {
					fail("withdraw history", fmt.Errorf("walletType absent, skipped"))
					continue
				}
				if *r.WalletType != 0 {
					continue
				}
				amount, err := incomeAmount(r.Amount)
				if err != nil {
					fail("withdraw history", err)
					continue
				}
				// Binance reports the actual wallet debit time in completeTime.
				at, err := time.ParseInLocation("2006-01-02 15:04:05", r.CompleteTime, time.UTC)
				if err != nil {
					fail("withdraw history", fmt.Errorf("completeTime missing/invalid, skipped"))
					continue
				}
				fee := 0.0
				if r.TransactionFee != "" {
					fee, err = incomeAmount(r.TransactionFee)
					if err != nil {
						fail("withdraw history", err)
						continue
					}
				}
				out = append(out, fundsledger.Income{TranID: "withdraw/" + r.ID, IncomeType: "WITHDRAW", Asset: r.Coin, Amount: -(amount + fee), TimeMS: at.UnixMilli(), Info: "现货钱包提现（含网络手续费）"})
			}
			if len(rows) < 1000 {
				break
			}
			if err := ctx.Err(); err != nil {
				return out, fmt.Errorf("%w: %w", types.ErrIncomeHistoryUnavailable, err)
			}
		}
		lo = hi + 1
	}
	// Only transfers crossing MAIN (spot) affect this account's equity. Do not
	// query both sides of a non-spot movement or count futures/funding twice.
	directions := []string{"MAIN_UMFUTURE", "UMFUTURE_MAIN", "MAIN_CMFUTURE", "CMFUTURE_MAIN", "MAIN_MARGIN", "MARGIN_MAIN", "MAIN_FUNDING", "FUNDING_MAIN", "MAIN_OPTION", "OPTION_MAIN", "MAIN_PORTFOLIO_MARGIN", "PORTFOLIO_MARGIN_MAIN"}
	for _, direction := range directions {
		for page := 1; ; page++ {
			rows, err := t.client.NewListUserUniversalTransferService().Type(binance.UserUniversalTransferType(direction)).StartTime(start.UnixMilli()).EndTime(end.UnixMilli()).Current(page).Size(100).Do(ctx)
			if err != nil {
				fail("universal transfer "+direction, err)
				break
			}
			if rows == nil {
				fail("universal transfer", fmt.Errorf("empty response"))
				break
			}
			for _, r := range rows.Results {
				if r == nil {
					fail("universal transfer", fmt.Errorf("empty record"))
					continue
				}
				if r.Status != binance.UserUniversalTransferStatusTypeConfirmed {
					continue
				}
				amount, err := incomeAmount(r.Amount)
				if err != nil {
					fail("universal transfer", err)
					continue
				}
				if strings.HasPrefix(direction, "MAIN_") {
					amount = -amount
				}
				out = append(out, fundsledger.Income{TranID: "transfer/" + id(r.TranId), IncomeType: "TRANSFER_" + direction, Asset: r.Asset, Amount: amount, TimeMS: r.Timestamp, Info: direction})
			}
			if page*100 >= int(rows.Total) || len(rows.Results) < 100 {
				break
			}
		}
		if err := ctx.Err(); err != nil {
			fail("history context", err)
			break
		}
	}
	if len(failures) > 0 {
		return out, fmt.Errorf("%w: %w", types.ErrIncomeHistoryUnavailable, errors.Join(failures...))
	}
	return out, nil
}
