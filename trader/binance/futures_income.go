package binance

import (
	"context"
	"fmt"
	"math"
	"nofx/fundsledger"
	"nofx/market/bnweight"
	"nofx/trader/types"
	"strconv"
	"time"
)

var _ types.IncomeHistoryReader = (*FuturesTrader)(nil)

// ReadIncomeHistory reads every income type. Binance keeps about three months.
// Saturated windows are split and read in time order: advancing to lastTime+1
// would silently lose entries sharing that millisecond. The pinned SDK exposes
// no page parameter; a saturated single millisecond is an explicit error.
func (t *FuturesTrader) ReadIncomeHistory(ctx context.Context, start, end time.Time) ([]fundsledger.Income, error) {
	ctx = bnweight.WithPriority(ctx, bnweight.Normal) // same priority/transport as historyCtx
	out := []fundsledger.Income{}
	var read func(int64, int64) error
	read = func(lo, hi int64) error {
		rows, err := t.client.NewGetIncomeHistoryService().StartTime(lo).EndTime(hi).Limit(1000).Do(ctx)
		if err != nil {
			return fmt.Errorf("%w: futures income: %w", types.ErrIncomeHistoryUnavailable, err)
		}
		if len(rows) >= 1000 {
			if lo == hi {
				return fmt.Errorf("%w: income pagination saturated at millisecond %d", types.ErrIncomeHistoryUnavailable, lo)
			}
			mid := lo + (hi-lo)/2
			if err := read(lo, mid); err != nil {
				return err
			}
			return read(mid+1, hi)
		}
		for _, r := range rows {
			if r.Time < lo || r.Time > hi {
				return fmt.Errorf("%w: income outside requested window", types.ErrIncomeHistoryUnavailable)
			}
			amount, err := strconv.ParseFloat(r.Income, 64)
			if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) {
				return fmt.Errorf("%w: invalid income amount", types.ErrIncomeHistoryUnavailable)
			}
			out = append(out, fundsledger.Income{TranID: strconv.FormatInt(r.TranID, 10), IncomeType: r.IncomeType, Asset: r.Asset, Symbol: r.Symbol, Amount: amount, TimeMS: r.Time, Info: r.Info})
		}
		return nil
	}
	for lo := start.UnixMilli(); lo <= end.UnixMilli(); {
		hi := min(lo+7*24*60*60*1000-1, end.UnixMilli())
		if err := read(lo, hi); err != nil {
			return out, err
		}
		lo = hi + 1
	}
	return out, nil
}
