package fundsledger

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func ts(s string) time.Time {
	v, e := time.Parse(time.RFC3339, s)
	if e != nil {
		panic(e)
	}
	return v
}
func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-7 {
		t.Fatalf("got %.10f want %.10f", got, want)
	}
}
func options() Options { return Options{Now: ts("2026-10-24T02:00:00Z"), Days: 90, Location: time.UTC} }
func input() AccountInput {
	return AccountInput{TraderID: "a", Name: "A", Kind: LiveFutures, InitialBalance: 100, Snapshots: []Snapshot{{Time: ts("2026-10-22T00:00:00Z"), Equity: 100}, {Time: ts("2026-10-23T12:00:00Z"), Equity: 100}}}
}
func income(typ string, amount float64) Income {
	return Income{IncomeType: typ, TranID: typ, Asset: "USDT", Amount: amount, TimeMS: ts("2026-10-23T10:00:00Z").UnixMilli()}
}
func day(out Overview, date string) Daily {
	for _, d := range out.Daily {
		if d.Date == date {
			return d
		}
	}
	panic("missing day " + date)
}

func TestTradingOnlyAndIncomePriority(t *testing.T) {
	in := input()
	in.Snapshots[1].Equity = 106.7
	in.Snapshots[1].Unrealized = 2
	in.Trades = []Trade{{Time: ts("2026-10-23T10:00:00Z"), PnL: 999, Fees: 999}}
	in.Income = []Income{income("REALIZED_PNL", 5), income("COMMISSION", -.5), income("FUNDING_FEE", -.1), income("API_REBATE", .3)}
	out := Aggregate([]AccountInput{in}, options())
	d := day(out, "2026-10-23")
	approx(t, d.TradingPnL, 5)
	approx(t, d.Fees, .5)
	approx(t, d.Funding, -.1)
	approx(t, d.OtherIncome, .3)
	approx(t, d.UnrealizedChange, 2)
	approx(t, d.Unexplained, 0)
	if len(out.Flows) != 0 || d.TransferSource != "none" {
		t.Fatal(out)
	}
	approx(t, out.Accounts[0].AdjustedPnL, 6.7)
	// Old position-only history must remain usable before income retention.
	in.Income = nil
	in.Trades[0].PnL = 5
	in.Trades[0].Fees = .5
	in.Snapshots[1].Equity = 106.5
	out = Aggregate([]AccountInput{in}, options())
	approx(t, day(out, "2026-10-23").TradingPnL, 5)
	approx(t, out.Accounts[0].NetTransfers, 0)
}
func TestConfirmedAndResidualTransfers(t *testing.T) {
	for _, tt := range []struct {
		name                       string
		delta, confirmed, inferred float64
		source                     string
	}{{"confirmed", 50, 50, 0, "exchange"}, {"partial", 110, 50, 60, "mixed"}, {"opposite residual", 20, 50, -30, "mixed"}, {"small noise", 51, 50, 0, "exchange"}} {
		t.Run(tt.name, func(t *testing.T) {
			in := input()
			in.Snapshots[1].Equity += tt.delta
			in.Income = []Income{income("TRANSFER", tt.confirmed)}
			out := Aggregate([]AccountInput{in}, options())
			d := day(out, "2026-10-23")
			approx(t, d.NetTransfer, tt.confirmed+tt.inferred)
			if d.TransferSource != tt.source {
				t.Fatal(d)
			}
			got := 0.0
			for _, f := range out.Flows {
				if !f.Confirmed {
					got += f.Amount
				}
			}
			approx(t, got, tt.inferred)
			approx(t, d.Unexplained, tt.delta-tt.confirmed-tt.inferred)
		})
	}
}
func TestInferenceStrictThresholds(t *testing.T) {
	for _, open := range []float64{100, 1000} {
		for _, sign := range []float64{-1, 1} {
			for _, extra := range []float64{-.001, 0, .001} {
				in := input()
				in.InitialBalance = open
				threshold := math.Max(2, .02*open)
				change := sign * (threshold + extra)
				in.Snapshots[0].Equity = open
				in.Snapshots[1].Equity = open + change
				out := Aggregate([]AccountInput{in}, options())
				if extra > 0 {
					approx(t, out.Accounts[0].NetTransfers, change)
					if len(out.Flows) != 1 {
						t.Fatal(out)
					}
				} else {
					approx(t, out.Accounts[0].NetTransfers, 0)
					approx(t, day(out, "2026-10-23").Unexplained, change)
				}
			}
		}
	}
}
func TestMissingSnapshotDaysAndConfirmedGap(t *testing.T) {
	in := input()
	in.Snapshots[0].Time = ts("2026-10-20T00:00:00Z")
	in.Snapshots[1].Equity = 155
	r := income("TRANSFER", 50)
	r.TimeMS = ts("2026-10-21T12:00:00Z").UnixMilli()
	in.Income = []Income{r}
	in.Trades = []Trade{{Time: ts("2026-10-22T12:00:00Z"), PnL: 5}}
	out := Aggregate([]AccountInput{in}, options())
	if len(out.Flows) != 1 || !out.Flows[0].Confirmed {
		t.Fatal(out.Flows)
	}
	for _, date := range []string{"2026-10-21", "2026-10-22"} {
		d := day(out, date)
		approx(t, d.OpenEquity, 100)
		approx(t, d.CloseEquity, 100)
	}
	approx(t, out.Accounts[0].NetTransfers, 50)
	approx(t, out.Accounts[0].Unexplained, 0)
	// Without known flows the missing dates must have no invented transfers.
	in.Income = nil
	in.Trades = nil
	out = Aggregate([]AccountInput{in}, options())
	if len(out.Flows) != 1 || out.Flows[0].Date != "2026-10-23" {
		t.Fatal(out.Flows)
	}
	approx(t, out.Flows[0].Amount, 55)
}
func TestTimezoneFirstDayAndPreSnapshotIncome(t *testing.T) {
	in := input()
	in.Snapshots = []Snapshot{{Time: ts("2026-10-14T15:59:00Z"), Equity: 100, Unrealized: 1}, {Time: ts("2026-10-14T16:01:00Z"), Equity: 535, Unrealized: 1}}
	r := income("TRANSFER", 100)
	r.TimeMS = ts("2026-10-14T15:00:00Z").UnixMilli()
	in.Income = []Income{r}
	o := options()
	o.Location, _ = time.LoadLocation("Asia/Shanghai")
	out := Aggregate([]AccountInput{in}, o)
	if out.Flows[0].Date != "2026-10-15" {
		t.Fatal(out.Flows)
	}
	approx(t, day(out, "2026-10-14").DeltaEquity, 0)
	approx(t, day(out, "2026-10-15").NetTransfer, 435)
	// Same natural day: the opening value is the first snapshot, not zero.
	o.Location = time.UTC
	out = Aggregate([]AccountInput{in}, o)
	approx(t, day(out, "2026-10-14").OpenEquity, 100)
	approx(t, out.Accounts[0].NetTransfers, 435)
}
func TestModifiedDietzHandCalculation(t *testing.T) {
	in := input()
	in.Snapshots = []Snapshot{{Time: ts("2026-10-20T00:00:00Z"), Equity: 100}, {Time: ts("2026-10-22T00:00:00Z"), Equity: 602}}
	r := income("TRANSFER", 500)
	r.TimeMS = ts("2026-10-21T00:00:00Z").UnixMilli()
	in.Income = []Income{r}
	in.Trades = []Trade{{Time: ts("2026-10-21T12:00:00Z"), PnL: 2}}
	out := Aggregate([]AccountInput{in}, options())
	a := out.Accounts[0]
	approx(t, a.AdjustedPnL, 2)
	approx(t, a.AdjustedReturnPct, 2.0/(100+500*.5)*100)
	approx(t, a.RawReturnPct, 502)
}
func TestPaperExcludedFromBothTotals(t *testing.T) {
	live := input()
	paper := input()
	paper.TraderID = "paper"
	paper.Kind = Paper
	paper.InitialBalance = 10000
	paper.Snapshots[1].Equity = 90000
	o := options()
	out := Aggregate([]AccountInput{live, paper}, o)
	if len(out.Accounts) != 1 {
		t.Fatal(out)
	}
	o.IncludePaper = true
	out = Aggregate([]AccountInput{live, paper}, o)
	if len(out.Accounts) != 2 {
		t.Fatal(out)
	}
	approx(t, out.Totals.CurrentEquity, 100)
	approx(t, day(out, "2026-10-23").CloseEquity, 100)
	for _, d := range out.DailyTotal {
		approx(t, d.CloseEquity, 100)
	}
}
func TestUSStockTwoLargeDepositsAreNotProfits(t *testing.T) {
	in := input()
	in.Kind = LiveSpot
	in.Name = "us-stock"
	in.Snapshots = []Snapshot{{Time: ts("2026-10-13T14:00:00Z"), Equity: 100}, {Time: ts("2026-10-14T12:00:00Z"), Equity: 535}, {Time: ts("2026-10-21T12:00:00Z"), Equity: 535.5}, {Time: ts("2026-10-22T12:00:00Z"), Equity: 727}, {Time: ts("2026-10-24T01:00:00Z"), Equity: 727.7}}
	out := Aggregate([]AccountInput{in}, options())
	a := out.Accounts[0]
	approx(t, a.NetTransfers, 626.5)
	if math.Abs(a.AdjustedReturnPct) > 1 || a.RawReturnPct < 600 {
		t.Fatal(a)
	}
	if len(out.Flows) != 2 {
		t.Fatal(out.Flows)
	}
	o := options()
	o.Days = 1
	recent := Aggregate([]AccountInput{in}, o)
	approx(t, recent.Accounts[0].NetTransfers, a.NetTransfers)
	approx(t, recent.Accounts[0].AdjustedReturnPct, a.AdjustedReturnPct)
	if len(recent.Flows) != 0 {
		t.Fatal(recent.Flows)
	}
}
func TestNoSnapshotsAndNonStableIncome(t *testing.T) {
	in := input()
	in.Income = []Income{income("TRANSFER", 100)}
	in.Income[0].Asset = "BTC"
	out := Aggregate([]AccountInput{in}, options())
	approx(t, out.Accounts[0].NetTransfers, 0)
	if len(out.Accounts[0].Warnings) == 0 {
		t.Fatal(out)
	}
	in.Snapshots = nil
	out = Aggregate([]AccountInput{in}, options())
	if len(out.Accounts) != 1 || len(out.Accounts[0].Warnings) == 0 {
		t.Fatal(out)
	}
	approx(t, out.Totals.AdjustedPnL, 0)
}

func assertDailyReconciliation(t *testing.T, out Overview) {
	t.Helper()
	sums := map[string]DayAmounts{}
	for _, d := range out.Daily {
		explained := d.TradingPnL - d.Fees + d.Funding + d.OtherIncome + d.UnrealizedChange
		approx(t, d.DeltaEquity, explained+d.NetTransfer+d.Unexplained)
		sum := sums[d.Date]
		addDay(&sum, d.DayAmounts)
		sums[d.Date] = sum
	}
	if len(out.DailyTotal) != len(sums) {
		t.Fatalf("daily_total dates: got %d want %d", len(out.DailyTotal), len(sums))
	}
	for _, d := range out.DailyTotal {
		if d.DayAmounts != sums[d.Date] {
			t.Fatalf("daily_total %s: got %+v want %+v", d.Date, d.DayAmounts, sums[d.Date])
		}
		explained := d.TradingPnL - d.Fees + d.Funding + d.OtherIncome + d.UnrealizedChange
		approx(t, d.DeltaEquity, explained+d.NetTransfer+d.Unexplained)
	}
}

func TestTimingOffsetMacNofxConfirmedWithdrawal(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	in := AccountInput{TraderID: "mac-nofx", Name: "mac-nofx", Kind: LiveFutures, InitialBalance: 1000,
		Snapshots: []Snapshot{
			{Time: ts("2026-08-30T00:00:00+08:00"), Equity: 1000},
			{Time: ts("2026-08-31T23:00:00+08:00"), Equity: 1000},
			// September 1 has no snapshot; the withdrawal appears on September 2.
			{Time: ts("2026-09-02T23:00:00+08:00"), Equity: 843},
		},
		Income: []Income{
			{TranID: "withdrawal", IncomeType: "TRANSFER", Asset: "USDT", Amount: -244.97, TimeMS: ts("2026-08-31T10:00:00+08:00").UnixMilli()},
			{TranID: "pnl", IncomeType: "REALIZED_PNL", Asset: "USDT", Amount: 87.97, TimeMS: ts("2026-09-02T10:00:00+08:00").UnixMilli()},
		}}
	o := Options{Now: ts("2026-09-02T23:30:00+08:00"), Days: 4, Location: loc}
	out := Aggregate([]AccountInput{in}, o)
	if len(out.Flows) != 1 || !out.Flows[0].Confirmed || out.Flows[0].Type != "transfer_out" {
		t.Fatalf("only the confirmed withdrawal should remain: %+v", out.Flows)
	}
	approx(t, out.Flows[0].Amount, -244.97)
	withdrawal, settled := day(out, "2026-08-31"), day(out, "2026-09-02")
	approx(t, withdrawal.NetTransfer, -244.97)
	approx(t, withdrawal.Unexplained, 244.97)
	approx(t, settled.DeltaEquity, -157)
	approx(t, settled.NetTransfer, 0)
	approx(t, settled.Unexplained, -244.97)
	if withdrawal.TransferSource != "exchange" || settled.TransferSource != "none" {
		t.Fatalf("sources should revert to exchange/none: %+v / %+v", withdrawal, settled)
	}
	a := out.Accounts[0]
	approx(t, a.NetTransfers, -244.97)
	approx(t, out.Totals.NetTransfers, -244.97)
	approx(t, a.Unexplained, 0)
	approx(t, out.Totals.Unexplained, 0)
	approx(t, a.AdjustedPnL, 87.97)
	warnings := strings.Join(a.Warnings, "\n")
	for _, want := range []string{"2 笔推断转账因快照时点错位互相抵消", "08-31 +244.97", "09-02 -244.97"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("warnings missing %q: %s", want, warnings)
		}
	}
	assertDailyReconciliation(t, out)
	// Pair over all history before filtering, even when only the second day is visible.
	o.Days = 1
	recent := Aggregate([]AccountInput{in}, o)
	if len(recent.Flows) != 0 {
		t.Fatalf("paired inferred flow leaked across display cutoff: %+v", recent.Flows)
	}
	approx(t, recent.Accounts[0].NetTransfers, a.NetTransfers)
	approx(t, day(recent, "2026-09-02").Unexplained, -244.97)
	assertDailyReconciliation(t, recent)
}

// changes are daily equity changes after the first snapshot; skipped days still
// change equity, but have no observed close until a later snapshot.
func timingOffsetInput(open float64, changes []float64, skipped map[int]bool) (AccountInput, Options) {
	in := input()
	in.InitialBalance = open
	start := ts("2026-10-10T12:00:00Z")
	in.Snapshots = []Snapshot{{Time: start, Equity: open}}
	equity := open
	for i, change := range changes {
		equity += change
		if !skipped[i] {
			in.Snapshots = append(in.Snapshots, Snapshot{Time: start.AddDate(0, 0, i+1), Equity: equity})
		}
	}
	return in, Options{Now: start.AddDate(0, 0, len(changes)), Days: len(changes) + 1, Location: time.UTC}
}

func TestTimingOffsetPairingRules(t *testing.T) {
	for _, tt := range []struct {
		name    string
		open    float64
		changes []float64
		skipped map[int]bool
		want    map[string]float64
	}{
		{name: "opposite equal amounts", changes: []float64{100, -100}, want: map[string]float64{}},
		{name: "opposite starting with withdrawal", changes: []float64{-100, 100}, want: map[string]float64{}},
		{name: "amount difference exceeds threshold", changes: []float64{100, -70}, want: map[string]float64{"2026-10-11": 100, "2026-10-12": -70}},
		{name: "three natural days inclusive", changes: []float64{100, 0, 0, -100}, want: map[string]float64{}},
		{name: "over three days without missing snapshots", changes: []float64{100, 0, 0, 0, -100}, want: map[string]float64{"2026-10-11": 100, "2026-10-15": -100}},
		{name: "over three days with intervening missing snapshot", changes: []float64{100, 0, 0, 0, -100}, skipped: map[int]bool{2: true}, want: map[string]float64{}},
		{name: "missing snapshot before pair does not extend window", changes: []float64{0, 100, 0, 0, 0, -100}, skipped: map[int]bool{0: true}, want: map[string]float64{"2026-10-12": 100, "2026-10-16": -100}},
		{name: "same sign", changes: []float64{100, 100}, want: map[string]float64{"2026-10-11": 100, "2026-10-12": 100}},
		{name: "paired once and earliest tie wins", changes: []float64{100, -100, -100}, want: map[string]float64{"2026-10-13": -100}},
		{name: "closest amount before nearest date", changes: []float64{100, -105, -100}, want: map[string]float64{"2026-10-12": -105}},
		{name: "earliest unpaired flow has priority", changes: []float64{100, 105, -100}, want: map[string]float64{"2026-10-12": 105}},
		{name: "larger opening equity sets threshold", changes: []float64{1000, -1025}, want: map[string]float64{}},
		{name: "minimum threshold inclusive", open: 50, changes: []float64{10, -8}, want: map[string]float64{}},
		{name: "minimum threshold exceeded", open: 50, changes: []float64{10, -7.99}, want: map[string]float64{"2026-10-11": 10, "2026-10-12": -7.99}},
		{name: "multiple pairs", changes: []float64{100, -100, 200, -200}, want: map[string]float64{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			open := tt.open
			if open == 0 {
				open = 1000
			}
			in, o := timingOffsetInput(open, tt.changes, tt.skipped)
			out := Aggregate([]AccountInput{in}, o)
			if len(out.Flows) != len(tt.want) {
				t.Fatalf("remaining flows: got %+v want %v", out.Flows, tt.want)
			}
			for _, f := range out.Flows {
				amount, ok := tt.want[f.Date]
				if !ok || f.Source != "inferred" || f.Confirmed {
					t.Fatalf("unexpected flow: %+v", f)
				}
				approx(t, f.Amount, amount)
			}
			for _, d := range out.Daily {
				approx(t, d.NetTransfer, tt.want[d.Date])
				wantSource := "none"
				if _, ok := tt.want[d.Date]; ok {
					wantSource = "inferred"
				}
				if d.TransferSource != wantSource {
					t.Fatalf("%s source: got %s want %s", d.Date, d.TransferSource, wantSource)
				}
			}
			removed := 0
			unexplained := 0.0
			for _, d := range out.Daily {
				unexplained += d.Unexplained
				if math.Abs(d.Unexplained) > 1e-7 {
					removed++
				}
			}
			warnings := strings.Join(out.Accounts[0].Warnings, "\n")
			if removed > 0 && !strings.Contains(warnings, fmt.Sprintf("%d 笔推断转账因快照时点错位互相抵消", removed)) {
				t.Fatalf("missing cancellation count: %s", warnings)
			}
			if removed == 0 && strings.Contains(warnings, "因快照时点错位互相抵消") {
				t.Fatalf("unexpected cancellation warning: %s", warnings)
			}
			approx(t, out.Accounts[0].Unexplained, unexplained)
			approx(t, out.Totals.Unexplained, unexplained)
			// Preserve the pre-cancellation cumulative net transfer, even for
			// approximately offsetting amounts within the tolerance.
			net := 0.0
			for _, change := range tt.changes {
				net += change
			}
			approx(t, out.Accounts[0].NetTransfers, net)
			approx(t, out.Totals.NetTransfers, net)
			assertDailyReconciliation(t, out)
		})
	}
}

func TestTimingOffsetNeverPairsConfirmedTransfers(t *testing.T) {
	for _, confirmedDay := range []int{0, 1} {
		in, o := timingOffsetInput(1000, []float64{100, -100}, nil)
		amount := 100.0
		if confirmedDay == 1 {
			amount = -100
		}
		in.Income = []Income{{IncomeType: "TRANSFER", TranID: "confirmed", Asset: "USDT", Amount: amount,
			TimeMS: in.Snapshots[confirmedDay+1].Time.Add(-time.Hour).UnixMilli()}}
		out := Aggregate([]AccountInput{in}, o)
		if len(out.Flows) != 2 {
			t.Fatalf("confirmed and inferred flows must both remain: %+v", out.Flows)
		}
		confirmed := 0
		for _, f := range out.Flows {
			if f.Confirmed {
				confirmed++
				approx(t, f.Amount, amount)
			}
		}
		if confirmed != 1 {
			t.Fatalf("confirmed flow count: %d", confirmed)
		}
		assertDailyReconciliation(t, out)
	}
}

func TestTimingOffsetPairsWithinAccountOnly(t *testing.T) {
	in, o := timingOffsetInput(1000, []float64{100, 0}, nil)
	other, _ := timingOffsetInput(1000, []float64{0, -100}, nil)
	other.TraderID, other.Name = "other", "Other"
	out := Aggregate([]AccountInput{in, other}, o)
	if len(out.Flows) != 2 {
		t.Fatalf("different accounts must not pair: %+v", out.Flows)
	}
	approx(t, out.Accounts[0].NetTransfers, 100)
	approx(t, out.Accounts[1].NetTransfers, -100)
	assertDailyReconciliation(t, out)
}

func TestTimingOffsetUsesNaturalDaysAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	in, o := timingOffsetInput(1000, []float64{100, 0, 0, -100}, nil)
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, loc)
	for i := range in.Snapshots {
		in.Snapshots[i].Time = start.AddDate(0, 0, i)
	}
	o.Now, o.Location = in.Snapshots[4].Time, loc
	if o.Now.Sub(in.Snapshots[1].Time) != 73*time.Hour {
		t.Fatal("fixture must span the DST fallback in three natural days")
	}
	out := Aggregate([]AccountInput{in}, o)
	if len(out.Flows) != 0 {
		t.Fatalf("three local dates must pair despite exceeding 72 hours: %+v", out.Flows)
	}
	assertDailyReconciliation(t, out)
}
