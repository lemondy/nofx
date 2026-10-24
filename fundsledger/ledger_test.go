package fundsledger

import (
	"math"
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
