package trader

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"nofx/market"
	"nofx/market/bnweight"
	"nofx/store"
)

var stockFundingSettlements = func(symbol string, start, end time.Time) ([]market.FundingSettlement, error) {
	return market.GetFundingSettlementsPriority(symbol, start, end, bnweight.Normal)
}

// Settle before changing quantity as well as on ticks, so a settlement crossed
// between ticks uses the quantity held at that boundary. Only applied events
// advance the durable watermark; failed reads are retried after a restart.
func (at *AutoTrader) stockApplyFunding(ctx context.Context, p *store.StockPaperPosition, price float64, now time.Time) error {
	if p.EffectiveSide() != store.StockPaperShort {
		return nil
	}
	info, ok := stockLookupSymbol(ctx, p.Symbol)
	if !ok || info.PerpSymbol == "" {
		return fmt.Errorf("NOT_SHORTABLE: %s", p.Symbol)
	}
	last := max(p.LastFundingTime, p.OpenedAt)
	for last < now.UnixMilli() {
		if err := ctx.Err(); err != nil {
			return err
		}
		events, err := stockFundingSettlements(info.PerpSymbol, time.UnixMilli(last+1), now)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		sort.Slice(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })
		before := last
		for _, event := range events {
			ms := event.Time.UnixMilli()
			if ms <= last || event.Time.After(now) {
				continue
			}
			mark := event.MarkPrice
			if mark <= 0 || math.IsNaN(mark) || math.IsInf(mark, 0) {
				mark = price
			}
			if err := at.store.StockPaper().ApplyShortFunding(p.ID, event.Time, event.Rate, mark); err != nil {
				return err
			}
			last = ms
		}
		// The existing history helper returns at most one 1000-event page.
		if len(events) < 1000 || last == before {
			return nil
		}
	}
	return nil
}
