package paper

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"nofx/market"
	"nofx/market/bnweight"
	"nofx/store"
)

// fakeFunding is an injected FundingSource; it never touches the network.
type fakeFunding struct {
	mu         sync.Mutex
	events     map[string][]FundingEvent
	calls      []string
	err        error
	pageLimit  int  // >0: return at most this many events (oldest first)
	ignoreFrom bool // return events regardless of `from` (exercises the watermark guards)
}

func (f *fakeFunding) FundingEvents(symbol string, from, to time.Time) ([]FundingEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, symbol+"@"+from.Format(time.RFC3339))
	if f.err != nil {
		return nil, f.err
	}
	var out []FundingEvent
	for _, e := range f.events[symbol] {
		if e.Time.After(to) || (!f.ignoreFrom && e.Time.Before(from)) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if f.pageLimit > 0 && len(out) > f.pageLimit {
		out = out[:f.pageLimit]
	}
	return out, nil
}
func (f *fakeFunding) add(symbol string, at time.Time, rate, mark float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[symbol] = append(f.events[symbol], FundingEvent{Time: at, Rate: rate, MarkPrice: mark})
}
func (f *fakeFunding) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }
func (f *fakeFunding) calledSymbols() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	symbols := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		symbol, _, _ := strings.Cut(call, "@")
		symbols = append(symbols, symbol)
	}
	return symbols
}

var day0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func setupFunding(t *testing.T) (*PaperFuturesTrader, *store.Store, *fakeMarket, *fakeFunding) {
	t.Helper()
	ff := &fakeFunding{events: map[string][]FundingEvent{}}
	p, st, f := setup(t, Config{InitialBalance: 10000, Funding: ff})
	return p, st, f, ff
}

// advanceTo moves the fake clock to at and settles.
func advanceTo(t *testing.T, p *PaperFuturesTrader, f *fakeMarket, at time.Time, mark float64) {
	t.Helper()
	f.mu.Lock()
	delta := at.Sub(f.at)
	f.mu.Unlock()
	f.step(delta, mark)
	must(t, p.Advance(at))
}

func fundingIncome(t *testing.T, st *store.Store, exchangeID string) []store.AccountIncome {
	t.Helper()
	var rows []store.AccountIncome
	must(t, st.GormDB().Where("exchange_id = ? AND income_type = ?", exchangeID, "FUNDING_FEE").Order("time_ms ASC, tran_id ASC").Find(&rows).Error)
	return rows
}

func TestFundingLongPaysShortReceives(t *testing.T) {
	p, st, f, ff := setupFunding(t)
	_, err := p.OpenLong("BTCUSDT", 2, 10)
	must(t, err)
	_, err = p.OpenShort("BTCUSDT", 1, 10)
	must(t, err)
	before := summary(t, p).WalletBalance
	ff.add("BTCUSDT", day0.Add(8*time.Hour+5*time.Millisecond), 0.0001, 101)
	advanceTo(t, p, f, day0.Add(8*time.Hour+30*time.Second), 100)

	// long pays 2*101*0.0001, short receives 1*101*0.0001
	longPay, shortGet := 2*101*0.0001, 1*101*0.0001
	s := summary(t, p)
	near(t, s.WalletBalance, before-longPay+shortGet)
	near(t, s.TotalFunding, -longPay+shortGet)
	fund := p.PositionFunding()
	near(t, fund["BTCUSDT:LONG"], -longPay)
	near(t, fund["BTCUSDT:SHORT"], shortGet)
	rows := fundingIncome(t, st, "acct")
	if len(rows) != 2 {
		t.Fatalf("income rows = %+v", rows)
	}
	sum := 0.0
	for _, r := range rows {
		sum += r.Amount
		if r.AccountKind != "paper" || r.Asset != "USDT" || r.Category != "funding" || r.Symbol != "BTCUSDT" {
			t.Fatalf("row = %+v", r)
		}
	}
	near(t, sum, s.TotalFunding)
	// Funding is not realized PnL, and it is not a fee.
	near(t, s.RealizedPnL, 0)
}

func TestFundingNegativeRateAndMarkFallback(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	_, err := p.OpenLong("BTCUSDT", 3, 10)
	must(t, err)
	before := summary(t, p).WalletBalance
	// negative rate: the long RECEIVES; MarkPrice 0: the freshest quote (105) is used.
	ff.add("BTCUSDT", day0.Add(8*time.Hour), -0.0002, 0)
	advanceTo(t, p, f, day0.Add(8*time.Hour+time.Minute), 105)
	near(t, summary(t, p).WalletBalance, before+3*105*0.0002)
}

func TestFundingEventBeforeOpenNotCharged(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	ff.ignoreFrom = true // the source returns old events anyway: the simulator must still skip them
	advanceTo(t, p, f, day0.Add(9*time.Hour), 100)
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.01, 100) // before the position exists
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	before := summary(t, p).WalletBalance
	advanceTo(t, p, f, day0.Add(9*time.Hour+30*time.Minute), 100)
	near(t, summary(t, p).WalletBalance, before)
	if got := p.PositionFunding()["BTCUSDT:LONG"]; got != 0 {
		t.Fatalf("funding charged for an event before the open: %v", got)
	}
	// An event stamped exactly at the open instant counts (position existed at settlement).
	ff.add("BTCUSDT", day0.Add(9*time.Hour), 0.001, 100)
	advanceTo(t, p, f, day0.Add(17*time.Hour), 100)
	near(t, summary(t, p).WalletBalance, before-1*100*0.001)
}

func TestFundingWatermarkSurvivesRestartNoDoubleChargeAndCatchUp(t *testing.T) {
	p, st, f, ff := setupFunding(t)
	ff.ignoreFrom = true // prove the persisted watermark, not the query range, prevents repeats
	_, err := p.OpenLong("BTCUSDT", 2, 10)
	must(t, err)
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.0001, 100)
	advanceTo(t, p, f, day0.Add(8*time.Hour+time.Minute), 100)
	charged := summary(t, p).TotalFunding
	near(t, charged, -2*100*0.0001)

	// "Restart": a new simulator on the same store. Two more events happened while down.
	ff.add("BTCUSDT", day0.Add(16*time.Hour), 0.0003, 100)
	ff.add("BTCUSDT", day0.Add(24*time.Hour), -0.0001, 100)
	cfg := Config{InitialBalance: 10000, Funding: ff, ExchangeID: "acct", Now: f.now}
	f.step(24*time.Hour+time.Minute-8*time.Hour-time.Minute, 100) // clock at day0+24h+1min
	p2 := New(cfg, st, f)
	must(t, p2.initErr)
	t.Cleanup(p2.StopOrderSync)
	must(t, p2.Advance(f.now()))
	want := charged - 2*100*0.0003 + 2*100*0.0001
	near(t, summary(t, p2).TotalFunding, want)
	// Another restart and several advances charge nothing more.
	p3 := New(cfg, st, f)
	must(t, p3.initErr)
	for i := 0; i < 3; i++ {
		advanceTo(t, p3, f, f.now().Add(time.Minute), 100)
	}
	near(t, summary(t, p3).TotalFunding, want)
	if n := len(fundingIncome(t, st, "acct")); n != 3 {
		t.Fatalf("income rows = %d, want 3 (one per event)", n)
	}
	// The wallet in the persisted row agrees with the in-memory state.
	row, err := st.PaperLedger().Get("acct")
	must(t, err)
	near(t, row.WalletBalance, summary(t, p3).WalletBalance)
}

func TestFundingReopenedPositionNotChargedWhileFlat(t *testing.T) {
	p, st, f, ff := setupFunding(t)
	ff.ignoreFrom = true
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.001, 100) // charged to the first position
	f.step(8*time.Hour+time.Minute, 100)
	_, err = p.CloseLong("BTCUSDT", 0)
	must(t, err)
	first := summary(t, p).TotalFunding
	near(t, first, -1*100*0.001)

	ff.add("BTCUSDT", day0.Add(16*time.Hour), 0.002, 100) // flat: nobody pays
	f.step(8*time.Hour, 100)
	must(t, p.Advance(f.now()))
	near(t, summary(t, p).TotalFunding, first)

	f.step(30*time.Minute, 100)
	_, err = p.OpenLong("BTCUSDT", 1, 10) // reopened after both events
	must(t, err)
	ff.add("BTCUSDT", day0.Add(24*time.Hour), 0.004, 100)
	advanceTo(t, p, f, day0.Add(24*time.Hour+time.Minute), 100)
	near(t, summary(t, p).TotalFunding, first-1*100*0.004) // only the event after the reopen
	if got := p.PositionFunding()["BTCUSDT:LONG"]; got > -0.3999 || got < -0.4001 {
		t.Fatalf("reopened position funding = %v", got)
	}
	if n := len(fundingIncome(t, st, "acct")); n != 2 {
		t.Fatalf("income rows = %d", n)
	}
}

func TestFundingFetchErrorDoesNotBreakAdvance(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	_, err := p.OpenLong("BTCUSDT", 2, 10)
	must(t, err)
	before := summary(t, p).WalletBalance
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.0001, 100)
	ff.err = errors.New("fundingRate: 503")
	advanceTo(t, p, f, day0.Add(8*time.Hour+time.Minute), 100) // must not fail
	near(t, summary(t, p).WalletBalance, before)
	// Orders still work during the outage.
	if _, err := p.CloseLong("BTCUSDT", 1); err != nil {
		t.Fatalf("order failed during funding outage: %v", err)
	}
	ff.err = nil
	advanceTo(t, p, f, day0.Add(8*time.Hour+3*time.Minute), 100)
	// The next tick retried and caught up: charged once, on the quantity held now.
	near(t, p.PositionFunding()["BTCUSDT:LONG"], -1*100*0.0001)
}

func TestFundingFailureStopsBatchAndBacksOff(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	for _, symbol := range []string{"SOLUSDT", "ETHUSDT", "BTCUSDT"} {
		_, err := p.OpenLong(symbol, 1, 10)
		must(t, err)
	}
	ff.err = errors.New("fundingRate: 403")
	at := day0.Add(8*time.Hour + time.Minute)
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		advanceTo(t, p, f, at, 100)
		if got := ff.callCount(); got != i+1 {
			t.Fatalf("failure %d: calls = %v, want only the first symbol attempted", i+1, ff.calls)
		}
		if p.fundBackoff != want || !p.fundBackoffUntil.Equal(at.Add(want)) {
			t.Fatalf("failure %d: backoff = %s until %s, want %s until %s", i+1, p.fundBackoff, p.fundBackoffUntil, want, at.Add(want))
		}
		advanceTo(t, p, f, at.Add(want/2), 100)
		if got := ff.callCount(); got != i+1 || p.fundBackoff != want {
			t.Fatalf("inside backoff: calls = %d, backoff = %s", got, p.fundBackoff)
		}
		at = p.fundBackoffUntil // equality must allow the next attempt
	}
	for _, symbol := range ff.calledSymbols() {
		if symbol != "BTCUSDT" {
			t.Fatalf("queried remaining symbol after a failure: %v", ff.calls)
		}
	}
	ff.err = nil
	before := ff.callCount()
	advanceTo(t, p, f, at, 100)
	if got := ff.calledSymbols()[before:]; !reflect.DeepEqual(got, []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"}) {
		t.Fatalf("successful batch = %v", got)
	}
	if p.fundBackoff != 0 || !p.fundBackoffUntil.IsZero() {
		t.Fatalf("successful batch did not reset backoff: %s until %s", p.fundBackoff, p.fundBackoffUntil)
	}
	ff.err = errors.New("fundingRate: timeout")
	advanceTo(t, p, f, day0.Add(9*time.Hour+20*time.Minute), 100)
	if ff.callCount() != before+4 || p.fundBackoff != time.Minute {
		t.Fatalf("failure after recovery: calls = %v, backoff = %s", ff.calls, p.fundBackoff)
	}
}

func TestFundingBatchCapAndOldestQueryFirst(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	var symbols []string
	for i := 0; i < 20; i++ {
		symbol := fmt.Sprintf("S%02dUSDT", i)
		symbols = append(symbols, symbol)
		_, err := p.OpenLong(symbol, 1, 10)
		must(t, err)
	}
	// Never-queried symbols precede older successful queries. Different ages
	// override alphabetical order, with alphabetical ties within the same age.
	p.fundQueried[symbols[16]] = day0.Add(6 * time.Hour)
	p.fundQueried[symbols[17]] = day0.Add(5 * time.Hour)
	p.fundQueried[symbols[18]] = day0.Add(5 * time.Hour)
	p.fundQueried[symbols[19]] = day0.Add(4 * time.Hour)
	want := append(append([]string(nil), symbols[:16]...), symbols[19], symbols[17], symbols[18], symbols[16])
	for i, count := range []int{8, 16, 20} {
		advanceTo(t, p, f, day0.Add(8*time.Hour+time.Minute+time.Duration(i)*time.Second), 100)
		if got := ff.calledSymbols(); !reflect.DeepEqual(got, want[:count]) {
			t.Fatalf("advance %d: queries = %v, want %v", i+1, got, want[:count])
		}
	}
	// When everyone is due again, the first batch is now the oldest.
	advanceTo(t, p, f, day0.Add(8*time.Hour+20*time.Minute), 100)
	if got := ff.calledSymbols()[20:]; !reflect.DeepEqual(got, symbols[:8]) {
		t.Fatalf("next batch = %v, want oldest %v", got, symbols[:8])
	}
}

func TestFundingNoFetchWithoutPositions(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.01, 100)
	for i := 1; i <= 5; i++ {
		advanceTo(t, p, f, day0.Add(time.Duration(i)*time.Hour), 100)
	}
	if ff.callCount() != 0 {
		t.Fatalf("funding fetched with no open position: %v", ff.calls)
	}
	// No source at all: positions are simply not charged.
	q, _, f2 := setup(t, Config{InitialBalance: 10000})
	_, err := q.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	advanceTo(t, q, f2, day0.Add(9*time.Hour), 100)
	near(t, summary(t, q).TotalFunding, 0)
}

func TestFundingFetchOnlyWhenSettlementCouldExist(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	// Two past events 8h apart teach the interval (8h).
	ff.add("BTCUSDT", day0.Add(-16*time.Hour), 0.0001, 100)
	ff.add("BTCUSDT", day0.Add(-8*time.Hour), 0.0001, 100)
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	ff.ignoreFrom = true
	advanceTo(t, p, f, day0.Add(time.Minute), 100) // first tick: always queries (and learns the 8h interval)
	if n := ff.callCount(); n != 1 {
		t.Fatalf("first advance calls = %d", n)
	}
	// No 8h boundary until 08:00: one final query once the 00:00 grace window closed, then silence.
	for h := 1; h <= 7; h++ {
		advanceTo(t, p, f, day0.Add(time.Duration(h)*time.Hour+20*time.Minute), 100)
	}
	if n := ff.callCount(); n != 2 {
		t.Fatalf("polls between boundaries = %d, want 2 (%v)", n, ff.calls)
	}
	// Past the 08:00 boundary the symbol is queried again, then goes quiet after the grace window.
	advanceTo(t, p, f, day0.Add(8*time.Hour+20*time.Minute), 100)
	after := ff.callCount()
	if after != 3 {
		t.Fatalf("calls after the boundary = %d, want 3", after)
	}
	advanceTo(t, p, f, day0.Add(9*time.Hour), 100)
	advanceTo(t, p, f, day0.Add(12*time.Hour), 100)
	if n := ff.callCount(); n != after {
		t.Fatalf("polled again before the next boundary: %v", ff.calls)
	}
}

func TestFundingLateEventPickedUpInsideGrace(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	// The venue publishes the 08:00 record a few minutes late.
	advanceTo(t, p, f, day0.Add(8*time.Hour+10*time.Second), 100)
	near(t, summary(t, p).TotalFunding, 0)
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.0005, 100)
	advanceTo(t, p, f, day0.Add(8*time.Hour+2*time.Minute+30*time.Second), 100)
	near(t, summary(t, p).TotalFunding, -1*100*0.0005)
}

func TestFundingCachedBoundaryStopsGracePolling(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.001, 100)
	advanceTo(t, p, f, day0.Add(8*time.Hour+10*time.Second), 100)
	for _, elapsed := range []time.Duration{2*time.Minute + 30*time.Second, 5 * time.Minute, 9*time.Minute + 59*time.Second, 10 * time.Minute, 20 * time.Minute} {
		advanceTo(t, p, f, day0.Add(8*time.Hour+elapsed), 100)
	}
	if n := ff.callCount(); n != 1 {
		t.Fatalf("covered settlement was polled again: %v", ff.calls)
	}
	advanceTo(t, p, f, day0.Add(9*time.Hour+10*time.Second), 100)
	if n := ff.callCount(); n != 2 {
		t.Fatalf("next boundary was not queried: %v", ff.calls)
	}
}

func TestFundingUnknownIntervalLooksBackWithoutChargingPreOpen(t *testing.T) {
	p, st, f, ff := setupFunding(t)
	open := day0.Add(9 * time.Hour)
	advanceTo(t, p, f, open, 100)
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	before := summary(t, p).WalletBalance
	for h := -16; h <= 16; h += 8 {
		ff.add("BTCUSDT", day0.Add(time.Duration(h)*time.Hour), 0.001, 100)
	}
	advanceTo(t, p, f, day0.Add(16*time.Hour+time.Minute), 100)
	wantCall := "BTCUSDT@" + open.Add(-25*time.Hour).Format(time.RFC3339)
	if !reflect.DeepEqual(ff.calls, []string{wantCall}) {
		t.Fatalf("first query = %v, want %s", ff.calls, wantCall)
	}
	if p.fundIntervalH["BTCUSDT"] != 8 || len(p.fundCache["BTCUSDT"]) != 5 {
		t.Fatalf("interval = %d, cached = %v", p.fundIntervalH["BTCUSDT"], p.fundCache["BTCUSDT"])
	}
	near(t, summary(t, p).WalletBalance, before-100*0.001)
	near(t, p.PositionFunding()["BTCUSDT:LONG"], -100*0.001)
	rows := fundingIncome(t, st, "acct")
	if len(rows) != 1 || rows[0].TimeMS != day0.Add(16*time.Hour).UnixMilli() {
		t.Fatalf("lookback charged pre-open events: %+v", rows)
	}
}

func TestFundingReadSettlesCacheWithoutFetching(t *testing.T) {
	p, st, f, ff := setupFunding(t)
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	before := summary(t, p).WalletBalance
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.001, 100)
	ff.add("BTCUSDT", day0.Add(9*time.Hour), 0.002, 100)
	p.mu.Lock()
	p.mergeFundingLocked("BTCUSDT", ff.events["BTCUSDT"][:1])
	p.mu.Unlock()
	f.step(9*time.Hour+time.Minute, 100)
	balance, err := p.GetBalance()
	must(t, err)
	near(t, balance["totalWalletBalance"].(float64), before-100*0.001)
	near(t, p.PositionFunding()["BTCUSDT:LONG"], -100*0.001)
	if ff.callCount() != 0 || len(p.fundQueried) != 0 {
		t.Fatalf("read fetched funding: calls = %v, queried = %v", ff.calls, p.fundQueried)
	}
	f.step(time.Second, 100)
	balance, err = p.GetBalance()
	must(t, err)
	near(t, balance["totalWalletBalance"].(float64), before-100*0.001)
	if ff.callCount() != 0 || len(fundingIncome(t, st, "acct")) != 1 {
		t.Fatalf("repeated read fetched or recharged funding: %v", ff.calls)
	}
	// An explicit Advance still fetches and catches up the uncached event.
	advanceTo(t, p, f, f.now().Add(time.Second), 100)
	if ff.callCount() != 1 {
		t.Fatalf("explicit Advance did not fetch funding: %v", ff.calls)
	}
	near(t, p.PositionFunding()["BTCUSDT:LONG"], -100*(0.001+0.002))
}

func TestFundingRepeatedAdvanceDoesNotRecharge(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.0001, 100)
	advanceTo(t, p, f, day0.Add(8*time.Hour+time.Minute), 100)
	advanceTo(t, p, f, day0.Add(8*time.Hour+2*time.Minute), 100)
	advanceTo(t, p, f, day0.Add(8*time.Hour+30*time.Minute), 100)
	near(t, summary(t, p).TotalFunding, -1*100*0.0001)
}

func TestFundingFullPageContinuesOnNextTick(t *testing.T) {
	old := fundingPageSize
	fundingPageSize = 2
	t.Cleanup(func() { fundingPageSize = old })
	p, _, f, ff := setupFunding(t)
	ff.pageLimit = 2
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	for h := 8; h <= 40; h += 8 { // five events, two per page
		ff.add("BTCUSDT", day0.Add(time.Duration(h)*time.Hour), 0.001, 100)
	}
	advanceTo(t, p, f, day0.Add(41*time.Hour), 100)
	near(t, summary(t, p).TotalFunding, -2*100*0.001)
	advanceTo(t, p, f, day0.Add(41*time.Hour+time.Minute), 100)
	near(t, summary(t, p).TotalFunding, -4*100*0.001)
	advanceTo(t, p, f, day0.Add(41*time.Hour+2*time.Minute), 100)
	near(t, summary(t, p).TotalFunding, -5*100*0.001)
}

func TestFundingChargedBeforeLaterStopOut(t *testing.T) {
	p, _, f, ff := setupFunding(t)
	_, err := p.OpenShort("BTCUSDT", 1, 10)
	must(t, err)
	must(t, p.SetStopLoss("BTCUSDT", "SHORT", 1, 110))
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.001, 100)
	// One Advance spans the settlement (08:00) and a later stop-out candle (08:30).
	stopBar := bar(day0.Add(8*time.Hour+30*time.Minute), 100, 112, 111)
	f.step(9*time.Hour, 111, stopBar)
	must(t, p.Advance(f.now()))
	if positions, _ := p.GetPositions(); len(positions) != 0 {
		t.Fatalf("stop did not fire: %v", positions)
	}
	near(t, summary(t, p).TotalFunding, 1*100*0.001) // the short was still open at 08:00
}

func TestFundingCanLiquidate(t *testing.T) {
	// Funding is part of the wallet liquidation tests see.
	ff := &fakeFunding{events: map[string][]FundingEvent{}}
	p, _, f := setup(t, Config{InitialBalance: 1000, Funding: ff})
	_, err := p.OpenLong("BTCUSDT", 9.5, 10) // notional 950, thin buffer
	must(t, err)
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 1.1, 100) // absurd rate: ~1045 USDT, more than the wallet
	advanceTo(t, p, f, day0.Add(8*time.Hour+time.Minute), 100)
	if positions, _ := p.GetPositions(); len(positions) != 0 {
		t.Fatalf("funding did not reach the liquidation test: %v", positions)
	}
}

func TestMarketPriceSourceImplementsFundingViaLowPriority(t *testing.T) {
	var _ FundingSource = (*marketPriceSource)(nil)
	var gotPrio bnweight.Priority = -1
	old := getFundingSettlements
	getFundingSettlements = func(symbol string, from, to time.Time, prio bnweight.Priority) ([]market.FundingSettlement, error) {
		gotPrio = prio
		return []market.FundingSettlement{{Time: day0, Rate: 0.0001, MarkPrice: 50}}, nil
	}
	t.Cleanup(func() { getFundingSettlements = old })
	src := NewMarketPriceSource().(FundingSource)
	ev, err := src.FundingEvents("BTCUSDT", day0, day0.Add(time.Hour))
	must(t, err)
	if len(ev) != 1 || ev[0].Rate != 0.0001 || ev[0].MarkPrice != 50 || gotPrio != bnweight.Low {
		t.Fatalf("%+v prio=%v", ev, gotPrio)
	}
}

// Funding reaches the existing position statistics through account_income
// (kind "paper"): the closed position's net P&L includes it.
func TestFundingAllocatedToClosedPositionStats(t *testing.T) {
	p, st, f, ff := setupFunding(t)
	must(t, p.SyncOrdersFromBinance("t1", "acct", "paper", st))
	_, err := p.OpenLong("BTCUSDT", 2, 10)
	must(t, err)
	ff.add("BTCUSDT", day0.Add(8*time.Hour), 0.0001, 100)
	f.step(8*time.Hour+time.Minute, 100)
	_, err = p.CloseLong("BTCUSDT", 0)
	must(t, err)
	stats, err := st.Position().GetPositionStats("t1")
	must(t, err)
	near(t, stats["total_funding"].(float64), -2*100*0.0001)
	near(t, stats["total_net_pnl"].(float64), stats["total_pnl"].(float64)-2*100*0.0001)
}
