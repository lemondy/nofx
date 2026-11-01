package paper

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"nofx/fundsledger"
	"nofx/logger"
)

// Perpetual funding for the paper futures simulator (see the package doc for
// the rules). Fetching happens outside the state mutex inside Advance; the
// charge itself runs inside Advance's single mutateLocked so the wallet change,
// the position watermark and the account_income row commit together.

const (
	// fundingGrace is how long after a settlement boundary the venue may still
	// be publishing the event; queries keep polling inside it.
	fundingGrace = 10 * time.Minute
	// fundingPollGap spaces polls inside the grace window.
	fundingPollGap = 2 * time.Minute
	// fundingWarnEvery rate-limits fetch-failure logs per symbol.
	fundingWarnEvery = 10 * time.Minute
	// fundingCacheMax bounds the per-symbol event cache.
	fundingCacheMax = 500
	// Failed batches pause all funding queries for this trader.
	fundingBackoffMin = time.Minute
	fundingBackoffMax = 15 * time.Minute
	// fundingMaxPerAdvance bounds the work in one funding batch.
	fundingMaxPerAdvance = 8
)

// fundingPageSize mirrors the venue's page limit: a full page means more may
// follow. A variable so tests can use a small page.
var fundingPageSize = 1000

// fundingBoundary is the latest settlement boundary at or before now for a
// symbol settling every intervalH hours (aligned to 00:00 UTC).
func fundingBoundary(now time.Time, intervalH int) time.Time {
	if intervalH < 1 || intervalH > 24 {
		intervalH = 1
	}
	now = now.UTC()
	h := now.Hour() - now.Hour()%intervalH
	return time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, time.UTC)
}

// fundingDueLocked reports whether a new settlement of symbol could exist.
func (p *PaperFuturesTrader) fundingDueLocked(symbol string, now time.Time) bool {
	interval := p.fundIntervalH[symbol]
	if interval < 1 {
		interval = 1
	}
	boundary := fundingBoundary(now, interval)
	if cached := p.fundCache[symbol]; len(cached) > 0 && !cached[len(cached)-1].Time.Before(boundary) {
		return false // the current settlement is already known
	}
	last := p.fundQueried[symbol]
	if last.IsZero() {
		return true
	}
	grace := boundary.Add(fundingGrace)
	if now.Before(grace) {
		return now.Sub(last) >= fundingPollGap && last.Before(grace)
	}
	return last.Before(grace) // one final query after the grace window
}

// fundingNeedsLocked lists, per symbol with open positions, the earliest
// fundingTime (ms) still to be charged, for symbols whose query is due.
func (p *PaperFuturesTrader) fundingNeedsLocked(now time.Time) map[string]int64 {
	if p.funding == nil || len(p.s.Positions) == 0 || now.Before(p.fundBackoffUntil) {
		return nil
	}
	needs := map[string]int64{}
	for _, pos := range p.s.Positions {
		from := pos.FundingThrough + 1
		if open := pos.OpenedAt.UnixMilli(); from < open {
			from = open
		}
		if cached := p.fundCache[pos.Symbol]; len(cached) > 0 {
			if next := cached[len(cached)-1].Time.UnixMilli() + 1; from < next {
				from = next // cache already covers everything before this
			}
		}
		if cur, ok := needs[pos.Symbol]; !ok || from < cur {
			needs[pos.Symbol] = from
		}
	}
	for symbol := range needs {
		if !p.fundingDueLocked(symbol, now) {
			delete(needs, symbol)
		} else if p.fundIntervalH[symbol] == 0 {
			// Include earlier settlements to learn the interval on the first
			// query. Settlement ignores events before each position's open.
			needs[symbol] -= (25 * time.Hour).Milliseconds()
		}
	}
	return needs
}

// fetchFunding queries up to fundingMaxPerAdvance due symbols, oldest query
// first, WITHOUT holding mu during requests. The first failure stops the batch
// and backs off all funding queries; it never fails Advance.
func (p *PaperFuturesTrader) fetchFunding(needs map[string]int64, now time.Time) {
	if len(needs) == 0 {
		return
	}
	p.mu.Lock()
	if now.Before(p.fundBackoffUntil) {
		p.mu.Unlock()
		return
	}
	symbols := make([]string, 0, len(needs))
	for s := range needs {
		symbols = append(symbols, s)
	}
	sort.Slice(symbols, func(i, j int) bool {
		a, b := p.fundQueried[symbols[i]], p.fundQueried[symbols[j]]
		if a.IsZero() != b.IsZero() {
			return a.IsZero()
		}
		if a.Equal(b) {
			return symbols[i] < symbols[j]
		}
		return a.Before(b)
	})
	if len(symbols) > fundingMaxPerAdvance {
		symbols = symbols[:fundingMaxPerAdvance]
	}
	p.mu.Unlock()
	for _, symbol := range symbols {
		ev, err := p.funding.FundingEvents(symbol, time.UnixMilli(needs[symbol]).UTC(), now)
		p.mu.Lock()
		if err != nil {
			p.fundBackoff = min(max(2*p.fundBackoff, fundingBackoffMin), fundingBackoffMax)
			p.fundBackoffUntil = now.Add(p.fundBackoff)
			if now.Sub(p.fundWarnAt[symbol]) >= fundingWarnEvery {
				p.fundWarnAt[symbol] = now
				logger.Warnf("paper funding [%s]: fetching %s settlements failed, backing off for %s: %v", p.displayName, symbol, p.fundBackoff, err)
			}
			p.mu.Unlock()
			return
		}
		p.mergeFundingLocked(symbol, ev)
		if len(ev) >= fundingPageSize {
			// More pages may follow: stay due so the next tick continues.
			p.fundQueried[symbol] = time.Time{}
		} else {
			p.fundQueried[symbol] = now
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.fundBackoff = 0
	p.fundBackoffUntil = time.Time{}
	p.mu.Unlock()
}

func (p *PaperFuturesTrader) mergeFundingLocked(symbol string, events []FundingEvent) {
	if len(events) == 0 {
		return
	}
	seen := map[int64]bool{}
	merged := make([]FundingEvent, 0, len(p.fundCache[symbol])+len(events))
	for _, ev := range append(append([]FundingEvent(nil), p.fundCache[symbol]...), events...) {
		ms := ev.Time.UnixMilli()
		if ev.Time.IsZero() || seen[ms] {
			continue
		}
		seen[ms] = true
		ev.Time = ev.Time.UTC()
		merged = append(merged, ev)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Time.Before(merged[j].Time) })
	if len(merged) > fundingCacheMax {
		merged = merged[len(merged)-fundingCacheMax:]
	}
	p.fundCache[symbol] = merged
	// Learn the settlement interval from consecutive events (1h/2h/4h/8h...).
	if n := len(merged); n >= 2 {
		d := merged[n-1].Time.Sub(merged[n-2].Time)
		if h := int(d / time.Hour); d%time.Hour == 0 && h >= 1 && h <= 24 && 24%h == 0 {
			p.fundIntervalH[symbol] = h
		}
	}
}

type fundingCharge struct {
	key string
	pos *position
	ev  FundingEvent
}

// settleFundingLocked charges every cached funding event with fundingTime <=
// cutoff that a position has not yet paid and that is not older than the
// position. Must run inside mutateLocked's fn: it mutates the wallet, the
// positions' watermarks and queues the FUNDING_FEE rows for the same commit.
func (p *PaperFuturesTrader) settleFundingLocked(cutoff time.Time) {
	if p.funding == nil || len(p.s.Positions) == 0 {
		return
	}
	var due []fundingCharge
	for key, pos := range p.s.Positions {
		open := pos.OpenedAt.UnixMilli()
		for _, ev := range p.fundCache[pos.Symbol] {
			ms := ev.Time.UnixMilli()
			if ms <= pos.FundingThrough || ms < open || ev.Time.After(cutoff) {
				continue
			}
			due = append(due, fundingCharge{key, pos, ev})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].ev.Time.Equal(due[j].ev.Time) {
			return due[i].ev.Time.Before(due[j].ev.Time)
		}
		return due[i].key < due[j].key
	})
	for _, c := range due {
		mark := c.ev.MarkPrice
		if !positive(mark) {
			mark = p.s.Marks[c.pos.Symbol]
		}
		if !positive(mark) {
			mark = c.pos.EntryPrice
		}
		direction := 1.0 // LONG pays a positive rate
		if c.pos.Side == "SHORT" {
			direction = -1
		}
		amount := -direction * c.pos.Quantity * mark * c.ev.Rate // positive = received
		ms := c.ev.Time.UnixMilli()
		p.s.Wallet += amount
		p.s.FundingFees += amount
		c.pos.Funding += amount
		c.pos.FundingThrough = ms
		p.pendingIncome = append(p.pendingIncome, fundsledger.Income{
			TranID:     fmt.Sprintf("paper-funding:%s:%s:%d", c.pos.Symbol, strings.ToLower(c.pos.Side), ms),
			IncomeType: "FUNDING_FEE",
			Symbol:     c.pos.Symbol,
			Asset:      "USDT",
			Amount:     amount,
			TimeMS:     ms,
			Info: fmt.Sprintf(`{"side":%q,"qty":%v,"mark":%v,"rate":%v}`,
				c.pos.Side, c.pos.Quantity, mark, c.ev.Rate),
		})
	}
}
