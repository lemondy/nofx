package store

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"nofx/fundsledger"

	"gorm.io/gorm"
)

func vfStatsJSON(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func vfNumber(t *testing.T, out map[string]interface{}, key string, want float64) {
	t.Helper()
	got, ok := out[key].(float64)
	if !ok || math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %.10f (%+v)", key, out[key], want, out)
	}
}

func vfPosition(t *testing.T, st *Store, p TraderPosition) TraderPosition {
	t.Helper()
	if err := st.GormDB().Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

func vfFunding(t *testing.T, st *Store, kind, exchange, symbol, id string, amount float64, at int64) {
	t.Helper()
	if err := st.AccountIncome().SaveForKind(exchange, kind, []fundsledger.Income{{TranID: id, IncomeType: "FUNDING_FEE", Symbol: symbol, Asset: "USDT", Amount: amount, TimeMS: at}}); err != nil {
		t.Fatal(err)
	}
}

func TestReview20261028VFFunding445AndNetStatistics(t *testing.T) {
	st := newTestStore(t)
	p := vfPosition(t, st, TraderPosition{ID: 445, TraderID: "t", ExchangeID: "e", ExchangeType: "binance", Symbol: "OKLOUSDT", Side: "SHORT", Status: "CLOSED", EntryTime: 100, ExitTime: 200, RealizedPnL: .75279999, Fee: .01305472})
	vfFunding(t, st, fundsledger.LiveFutures, "e", p.Symbol, "funding", .04941784, 150)
	// Same account's spot ledger, other accounts/symbols and out-of-lifecycle rows must be excluded.
	vfFunding(t, st, fundsledger.LiveSpot, "e", p.Symbol, "spot", 999, 150)
	vfFunding(t, st, fundsledger.LiveFutures, "foreign", p.Symbol, "foreign", 999, 150)
	vfFunding(t, st, fundsledger.LiveFutures, "e", "OTHERUSDT", "symbol", 999, 150)
	vfFunding(t, st, fundsledger.LiveFutures, "e", p.Symbol, "early", 999, 99)
	vfFunding(t, st, fundsledger.LiveFutures, "e", p.Symbol, "late", 999, 201)
	rows, err := st.Position().GetClosedPositions("t", 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	out := vfStatsJSON(t, rows[0])
	vfNumber(t, out, "funding_fee", .04941784)
	vfNumber(t, out, "net_pnl", .78916311)
	vfNumber(t, out, "realized_pnl", .75279999)
	if out["funding_unallocated"] != false {
		t.Fatal(out)
	}
	// A negative trade becomes a winner after funding; use net for all outcome metrics.
	vfPosition(t, st, TraderPosition{TraderID: "t", ExchangeID: "e", Symbol: "WINUSDT", Side: "LONG", Status: "CLOSED", EntryTime: 100, ExitTime: 200, RealizedPnL: -.1, Fee: .1})
	vfFunding(t, st, fundsledger.LiveFutures, "e", "WINUSDT", "win", .3, 150)
	vfPosition(t, st, TraderPosition{TraderID: "t", ExchangeID: "e", Symbol: "LOSSUSDT", Side: "LONG", Status: "CLOSED", EntryTime: 100, ExitTime: 200, RealizedPnL: -.5})
	stats, err := st.Position().GetFullStats("t", 100)
	if err != nil {
		t.Fatal(err)
	}
	out = vfStatsJSON(t, stats)
	vfNumber(t, out, "total_funding", .34941784)
	vfNumber(t, out, "total_net_pnl", .38916311)
	vfNumber(t, out, "total_pnl", .03974527) // Legacy fee-only field is preserved.
	vfNumber(t, out, "unallocated_funding", 0)
	if stats.WinTrades != 2 || math.Abs(stats.ProfitFactor-.88916311/.5) > 1e-9 || math.Abs(stats.SharpeRatio-calculateSharpeRatioFromPnls([]float64{.78916311, .1, -.5})) > 1e-9 {
		t.Fatalf("net metrics: %+v", stats)
	}
	for _, get := range []func() (interface{}, error){
		func() (interface{}, error) { return st.Position().GetSymbolStats("t", 10) },
		func() (interface{}, error) { return st.Position().GetDirectionStats("t") },
		func() (interface{}, error) { return st.Position().GetHoldingTimeStats("t") },
	} {
		if _, err := get(); err != nil {
			t.Fatal(err)
		}
	}
	symbols, _ := st.Position().GetSymbolStats("t", 10)
	for _, s := range symbols {
		if s.Symbol == "WINUSDT" && (s.WinRate != 100 || math.Abs(s.AvgPnL-.1) > 1e-9) {
			t.Fatalf("symbol net: %+v", s)
		}
	}
	dirs, _ := st.Position().GetDirectionStats("t")
	for _, d := range dirs {
		if d.Side == "LONG" && (d.WinRate != 50 || math.Abs(d.AvgPnL-(-.2)) > 1e-9) {
			t.Fatalf("direction net: %+v", d)
		}
	}
}

func TestReview20261028VFFundingOverlapAndBoundaries(t *testing.T) {
	for _, other := range []struct {
		name, trader, side, status string
		entry, exit                int64
	}{
		{"other_trader", "other", "LONG", "CLOSED", 100, 200},
		{"hedge", "t", "SHORT", "CLOSED", 100, 200},
		{"open_peer", "other", "LONG", "OPEN", 100, 0},
		{"inclusive_boundary", "other", "LONG", "CLOSED", 150, 300},
	} {
		t.Run(other.name, func(t *testing.T) {
			st := newTestStore(t)
			vfPosition(t, st, TraderPosition{TraderID: "t", ExchangeID: "e", Symbol: "XUSDT", Side: "LONG", Status: "CLOSED", EntryTime: 100, ExitTime: 200, RealizedPnL: 1, Fee: .1})
			vfPosition(t, st, TraderPosition{TraderID: other.trader, ExchangeID: "e", Symbol: "XUSDT", Side: other.side, Status: other.status, EntryTime: other.entry, ExitTime: other.exit})
			vfFunding(t, st, fundsledger.LiveFutures, "e", "XUSDT", "ambiguous", -.25, 150)
			rows, err := st.Position().GetClosedPositions("t", 1)
			if err != nil {
				t.Fatal(err)
			}
			out := vfStatsJSON(t, rows[0])
			vfNumber(t, out, "funding_fee", 0)
			if out["funding_unallocated"] != true {
				t.Fatal(out)
			}
			stats, err := st.Position().GetFullStats("t", 100)
			if err != nil {
				t.Fatal(err)
			}
			out = vfStatsJSON(t, stats)
			vfNumber(t, out, "unallocated_funding", -.25)
			vfNumber(t, out, "unallocated_funding_count", 1) // Count income once even for both hedge legs.
		})
	}
}

func TestReview20261028VFFundingPaperAndNoFunding(t *testing.T) {
	st := newTestStore(t)
	vfPosition(t, st, TraderPosition{TraderID: "paper", ExchangeID: "p", ExchangeType: "paper", Symbol: "XUSDT", Status: "CLOSED", EntryTime: 100, ExitTime: 200, RealizedPnL: 2, Fee: .1})
	vfFunding(t, st, fundsledger.Paper, "p", "XUSDT", "entry", .2, 100)
	vfFunding(t, st, fundsledger.Paper, "p", "XUSDT", "exit", -.1, 200)
	vfFunding(t, st, fundsledger.LiveFutures, "p", "XUSDT", "wrong_kind", 999, 150)
	rows, err := st.Position().GetClosedPositions("paper", 10)
	if err != nil {
		t.Fatal(err)
	}
	vfNumber(t, vfStatsJSON(t, rows[0]), "net_pnl", 2)
	vfPosition(t, st, TraderPosition{TraderID: "plain", Symbol: "XUSDT", Status: "CLOSED", RealizedPnL: 2, Fee: .1})
	rows, err = st.Position().GetClosedPositions("plain", 10)
	if err != nil {
		t.Fatal(err)
	}
	vfNumber(t, vfStatsJSON(t, rows[0]), "net_pnl", 1.9)
}

func TestReview20261028VFEquityCashFlowsAndMissingBase(t *testing.T) {
	st := newTestStore(t)
	if err := st.GormDB().Create(&Trader{ID: "t", UserID: "u", ExchangeID: "e"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.GormDB().Create(&Exchange{ID: "e", UserID: "u", ExchangeType: "binance"}).Error; err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	// +100 deposit, -50 withdrawal: only the final 20% trading loss is drawdown.
	for i, eq := range []float64{100, 200, 150, 120} {
		if err := st.Equity().Save(&EquitySnapshot{TraderID: "t", Timestamp: base.Add(time.Duration(i) * time.Hour), TotalEquity: eq}); err != nil {
			t.Fatal(err)
		}
	}
	for i, flow := range []struct {
		typ    string
		amount float64
		at     time.Time
	}{{"TRANSFER", 100, base.Add(time.Hour)}, {"WITHDRAW", -50, base.Add(2 * time.Hour)}} {
		if err := st.AccountIncome().Save("e", []fundsledger.Income{{TranID: time.Duration(i).String(), IncomeType: flow.typ, Amount: flow.amount, Asset: "USDT", TimeMS: flow.at.UnixMilli()}}); err != nil {
			t.Fatal(err)
		}
	}
	// Equity must work even with zero closed trades and unknown initial capital.
	stats, err := st.Position().GetFullStats("t", 0)
	if err != nil {
		t.Fatal(err)
	}
	vfNumber(t, vfStatsJSON(t, stats), "max_drawdown_pct", 20)
	if vfStatsJSON(t, stats)["max_drawdown_basis"] != "equity_twr" {
		t.Fatal(stats)
	}
	vfPosition(t, st, TraderPosition{TraderID: "no_base", Status: "CLOSED", RealizedPnL: -10})
	stats, err = st.Position().GetFullStats("no_base", 0)
	if err != nil {
		t.Fatal(err)
	}
	vfNumber(t, vfStatsJSON(t, stats), "max_drawdown_pct", 0)
	if vfStatsJSON(t, stats)["max_drawdown_basis"] != "unavailable" {
		t.Fatal(stats)
	}
	stats, err = st.Position().GetFullStats("no_base", 100)
	if err != nil {
		t.Fatal(err)
	}
	vfNumber(t, vfStatsJSON(t, stats), "max_drawdown_pct", 10)
	if vfStatsJSON(t, stats)["max_drawdown_basis"] != "closed_trades" {
		t.Fatal(stats)
	}
}

func TestReview20261028VFTWRDrawdownIgnoresTransfers(t *testing.T) {
	at := func(h int) time.Time { return time.Unix(0, 0).Add(time.Duration(h) * time.Hour) }
	// Withdrawal 100 -> 60 with -40 flow is not a drawdown; raw equity would show 40%.
	pts := []equityPoint{{at(0), 100}, {at(1), 60}}
	if dd, ok := twrMaxDrawdown(pts, []cashFlow{{at(1), -40}}); !ok || dd.Pct != 0 {
		t.Fatalf("withdrawal dd=%v ok=%v, want 0", dd, ok)
	}
	if dd, ok := twrMaxDrawdown(pts, nil); !ok || math.Abs(dd.Pct-40) > 1e-9 {
		t.Fatalf("no-flow dd=%v ok=%v, want 40", dd, ok)
	}
	// Deposit +100 at the end of a flat period is not recovery of a prior loss.
	pts = []equityPoint{{at(0), 100}, {at(1), 80}, {at(2), 180}}
	if dd, ok := twrMaxDrawdown(pts, []cashFlow{{at(2), 100}}); !ok || math.Abs(dd.Pct-20) > 1e-9 {
		t.Fatalf("deposit dd=%v, want 20", dd)
	}
	// Flows before the first snapshot are ignored; non-positive opening equity is skipped.
	if _, ok := twrMaxDrawdown([]equityPoint{{at(0), 0}, {at(1), 5}}, nil); ok {
		t.Fatal("zero opening equity must not produce a basis")
	}
}

// One bulk funding query + one peer query regardless of the position count.
func TestReview20261028VFFundingAllocationIsBulk(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 40; i++ {
		sym := "S" + string(rune('A'+i%26)) + "USDT"
		start := int64(1000 * (i + 1))
		vfPosition(t, st, TraderPosition{TraderID: "t", ExchangeID: "e", Symbol: sym + string(rune('0'+i/26)), Side: "LONG", Status: "CLOSED", EntryTime: start, ExitTime: start + 500, RealizedPnL: 1})
		vfFunding(t, st, fundsledger.LiveFutures, "e", sym+string(rune('0'+i/26)), "f"+sym+string(rune('0'+i/26)), .01, start+10)
	}
	queries := 0
	db := st.GormDB()
	const cb = "vf_count_queries"
	if err := db.Callback().Query().Before("gorm:query").Register(cb, func(*gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove(cb)
	rows, err := st.Position().GetClosedPositions("t", 100)
	if err != nil || len(rows) != 40 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if queries > 3 { // positions + funding + peers
		t.Fatalf("expected a constant number of queries, got %d", queries)
	}
	for _, r := range rows {
		if math.Abs(r.FundingFee-.01) > 1e-12 {
			t.Fatalf("funding %+v", r)
		}
	}
}
