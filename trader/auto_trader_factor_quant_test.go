package trader

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nofx/market/usstock"
	"nofx/store"
	"nofx/trader/paper"
)

// ------------------------------------------------------------ helpers

func fqPtr(v float64) *float64 { return &v }

func fqNear(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func fqTestTargets(asOf string, weights map[string]float64) *quantTargets {
	d, _ := time.Parse(factorDateFmt, asOf)
	return &quantTargets{Version: 1, Market: "crypto", Status: quantStatusOK, AsOf: asOf, asOfDate: d, RunID: "run-" + asOf,
		Horizon: 3, Rule: quantRule{Name: "ls_invvol_protected_v1"}, Weights: weights}
}

func fqDay(s string) time.Time { d, _ := time.Parse(factorDateFmt, s); return d.Add(2 * time.Hour) }

// fakeFutures is an in-memory futures venue: market orders fill at price[sym].
type fakeFutures struct {
	mu        sync.Mutex
	equity    float64
	prices    map[string]float64
	pos       map[string]*fakePos // key sym:side
	calls     []string
	failOpen  map[string]bool
	failStop  map[string]bool
	failClose map[string]bool
	lev       map[string]int
	stops     map[string]float64
	nextID    int
}

type fakePos struct {
	sym, side  string
	qty, entry float64
}

func newFakeFutures(equity float64, prices map[string]float64) *fakeFutures {
	return &fakeFutures{equity: equity, prices: prices, pos: map[string]*fakePos{}, failOpen: map[string]bool{},
		failStop: map[string]bool{}, failClose: map[string]bool{}, lev: map[string]int{}, stops: map[string]float64{}}
}

func (f *fakeFutures) log(format string, a ...interface{}) {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}

func (f *fakeFutures) GetBalance() (map[string]interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]interface{}{"totalWalletBalance": f.equity, "totalUnrealizedProfit": 0.0, "totalEquity": f.equity, "availableBalance": f.equity}, nil
}

func (f *fakeFutures) GetPositions() ([]map[string]interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]interface{}
	for _, p := range f.pos {
		amt := p.qty
		if p.side == "short" {
			amt = -p.qty
		}
		out = append(out, map[string]interface{}{"symbol": p.sym, "side": p.side, "positionAmt": amt, "entryPrice": p.entry,
			"markPrice": f.prices[p.sym], "unRealizedProfit": 0.0, "leverage": 3.0})
	}
	return out, nil
}

func (f *fakeFutures) open(sym, side string, q float64, lev int) (map[string]interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("open_%s %s %.8g lev=%d", side, sym, q, lev)
	if f.failOpen[sym] {
		return nil, errors.New("margin insufficient")
	}
	key := sym + ":" + side
	delete(f.stops, sym+":"+strings.ToUpper(side))
	px := f.prices[sym]
	if p := f.pos[key]; p != nil {
		p.entry = (p.entry*p.qty + px*q) / (p.qty + q)
		p.qty += q
	} else {
		f.pos[key] = &fakePos{sym: sym, side: side, qty: q, entry: px}
	}
	f.nextID++
	return map[string]interface{}{"orderId": int64(f.nextID), "avgPrice": px, "executedQty": q}, nil
}

func (f *fakeFutures) OpenLong(s string, q float64, l int) (map[string]interface{}, error) {
	return f.open(s, "long", q, l)
}
func (f *fakeFutures) OpenShort(s string, q float64, l int) (map[string]interface{}, error) {
	return f.open(s, "short", q, l)
}

func (f *fakeFutures) closePos(sym, side string, q float64) (map[string]interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("close_%s %s %.8g", side, sym, q)
	if f.failClose[sym] {
		return nil, errors.New("close rejected")
	}
	p := f.pos[sym+":"+side]
	if p == nil {
		return nil, errors.New("no position")
	}
	if q == 0 || q >= p.qty {
		q = p.qty
		delete(f.pos, sym+":"+side)
		delete(f.stops, sym+":"+strings.ToUpper(side))
	} else {
		p.qty -= q
	}
	f.nextID++
	return map[string]interface{}{"orderId": int64(f.nextID), "avgPrice": f.prices[sym], "executedQty": q}, nil
}
func (f *fakeFutures) CloseLong(s string, q float64) (map[string]interface{}, error) {
	return f.closePos(s, "long", q)
}
func (f *fakeFutures) CloseShort(s string, q float64) (map[string]interface{}, error) {
	return f.closePos(s, "short", q)
}
func (f *fakeFutures) SetLeverage(s string, l int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("leverage %s %d", s, l)
	f.lev[s] = l
	return nil
}
func (f *fakeFutures) GetMarketPrice(s string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.prices[s]
	if !ok {
		return 0, errors.New("no price")
	}
	return p, nil
}
func (f *fakeFutures) SetStopLoss(s, side string, q, price float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("stop %s %s %.8g @%.8g", s, side, q, price)
	if f.failStop[s] {
		return errors.New("stop rejected")
	}
	f.stops[s+":"+side] = price
	return nil
}
func (f *fakeFutures) CancelStopLossOrdersForSide(sym, side string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stops, sym+":"+side)
	return nil
}
func (f *fakeFutures) FormatQuantity(s string, q float64) (string, error) {
	// lot step 0.001, min 0.001
	v := math.Floor(q*1000+1e-9) / 1000
	if v < 0.001 {
		return "", errors.New("MIN_QTY")
	}
	return fmt.Sprintf("%.3f", v), nil
}

// ------------------------------------------------------------ targets reader

func TestQuantTargetsReader(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := loadQuantTargets(filepath.Join(dir, "nope.json"), "crypto"); !errors.Is(err, errQuantTargetsMissing) {
		t.Fatalf("missing file: %v", err)
	}
	good := `{"version":1,"market":"crypto","status":"ok","reason":null,"generated_at":"2026-11-01T19:00:00+00:00","as_of":"2026-10-31","run_id":"r","horizon":3,
"rule":{"name":"ls_invvol_protected_v1"},"factor_ids":["f1"],"universe_size":60,"weights":{"AAVEUSDT":0.04,"AGTUSDT":-0.04},"excluded":{"AKEUSDT":"short_ret7_top_decile"},"pct":{"AAVEUSDT":0.8}}`
	tg, err := loadQuantTargets(write("good.json", good), "crypto")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Status != "ok" || tg.AsOf != "2026-10-31" || tg.Horizon != 3 || tg.Rule.Name != "ls_invvol_protected_v1" || len(tg.Weights) != 2 || tg.Weights["AGTUSDT"] != -0.04 {
		t.Fatalf("parsed = %+v", tg)
	}
	if tg.asOfDate.Format(factorDateFmt) != "2026-10-31" {
		t.Fatal("as_of not parsed")
	}
	for name, body := range map[string]string{
		"corrupt":     good[:len(good)/2],
		"empty":       "",
		"trailing":    good + "garbage",
		"version":     strings.Replace(good, `"version":1`, `"version":2`, 1),
		"market":      strings.Replace(good, `"market":"crypto"`, `"market":"us"`, 1),
		"nostatus":    strings.Replace(good, `"status":"ok",`, "", 1),
		"bad as_of":   strings.Replace(good, `2026-10-31`, `10/31/2026`, 1),
		"huge weight": strings.Replace(good, `0.04,"AGT`, `7,"AGT`, 1),
		"nan weight":  strings.Replace(good, `-0.04}`, `"x"}`, 1),
	} {
		if _, err := loadQuantTargets(write(name+".json", body), "crypto"); !errors.Is(err, errQuantTargetsInvalid) {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
	// status != ok with empty weights is a valid file.
	idle := `{"version":1,"market":"us","status":"no_validated_factors","reason":"no ensemble","as_of":"2026-10-31","horizon":5,"weights":{}}`
	tg, err = loadQuantTargets(write("idle.json", idle), "us")
	if err != nil || tg.Status != "no_validated_factors" || tg.reasonText() != "no ensemble" || len(tg.Weights) != 0 {
		t.Fatalf("idle: %+v %v", tg, err)
	}
}

func TestQuantTargetsPathResolution(t *testing.T) {
	t.Setenv("FACTOR_MINING_DIR", "/data/fm")
	if got := factorQuantTargetsPath(&store.FactorQuantConfig{Market: "us"}); got != filepath.Join("/data/fm", "us", "quant_targets.json") {
		t.Fatalf("env path = %s", got)
	}
	if got := factorQuantTargetsPath(&store.FactorQuantConfig{Market: "crypto", TargetsPath: "/x/y.json"}); got != "/x/y.json" {
		t.Fatalf("override = %s", got)
	}
	t.Setenv("FACTOR_MINING_DIR", "")
	if got := factorQuantTargetsPath(&store.FactorQuantConfig{Market: "crypto"}); got != filepath.Join("data", "factor_mining", "crypto", "quant_targets.json") {
		t.Fatalf("default = %s", got)
	}
}

// ------------------------------------------------------------ rebalance decision

func TestRebalanceDecision(t *testing.T) {
	cfg := &store.FactorQuantConfig{Market: "crypto"} // horizon 3, stale 5
	tg := fqTestTargets("2026-10-31", map[string]float64{"AUSDT": 0.5})
	now := fqDay("2026-11-01")

	t.Run("first rebalance when flat", func(t *testing.T) {
		d := fqShouldRebalance(tg, cfg, "", false, now)
		if !d.Rebalance || d.Horizon != 3 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("never rebalanced but holding positions", func(t *testing.T) {
		d := fqShouldRebalance(tg, cfg, "", true, now)
		if d.Rebalance || d.Code != fqCodeUnmanaged || !d.noisy() {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("status not ok", func(t *testing.T) {
		idle := fqTestTargets("2026-10-31", nil)
		idle.Status = "no_validated_factors"
		r := "没有可用 ensemble"
		idle.Reason = &r
		d := fqShouldRebalance(idle, cfg, "", false, now)
		if d.Rebalance || d.Code != fqCodeStatus || !strings.Contains(d.Reason, "no_validated_factors") || !strings.Contains(d.Reason, r) {
			t.Fatalf("%+v", d)
		}
		if d2 := fqShouldRebalance(idle, cfg, "2026-10-20", true, now); d2.Rebalance {
			t.Fatal("status != ok must never rebalance even with a prior rebalance")
		}
	})
	t.Run("stale", func(t *testing.T) {
		d := fqShouldRebalance(tg, cfg, "", false, fqDay("2026-11-07")) // age 7 > 5
		if d.Rebalance || d.Code != fqCodeStale {
			t.Fatalf("%+v", d)
		}
		if d := fqShouldRebalance(tg, cfg, "", false, fqDay("2026-11-05")); !d.Rebalance { // age 5 == limit
			t.Fatalf("age == max_stale_days must still trade: %+v", d)
		}
		strict := &store.FactorQuantConfig{Market: "crypto", MaxStaleDays: 1}
		if d := fqShouldRebalance(tg, strict, "", false, fqDay("2026-11-03")); d.Rebalance || d.Code != fqCodeStale {
			t.Fatalf("custom max_stale_days: %+v", d)
		}
	})
	t.Run("horizon mismatch", func(t *testing.T) {
		h5 := fqTestTargets("2026-10-31", map[string]float64{"AUSDT": 0.5})
		h5.Horizon = 5
		d := fqShouldRebalance(h5, cfg, "", false, now)
		if d.Rebalance || d.Code != fqCodeHorizon {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("us follows the file horizon", func(t *testing.T) {
		us := &store.FactorQuantConfig{Market: "us"}
		h5 := fqTestTargets("2026-10-31", map[string]float64{"AAPLBUSDT": 1})
		h5.Horizon = 5
		d := fqShouldRebalance(h5, us, "2026-10-28", true, fqDay("2026-11-01"))
		if d.Rebalance || d.Code != fqCodeTooSoon || d.Horizon != 5 { // gap 3 < 5
			t.Fatalf("%+v", d)
		}
		d = fqShouldRebalance(h5, us, "2026-10-26", true, fqDay("2026-11-01"))
		if !d.Rebalance { // gap 5
			t.Fatalf("%+v", d)
		}
	})
	t.Run("too soon / due", func(t *testing.T) {
		if d := fqShouldRebalance(tg, cfg, "2026-10-31", true, now); d.Rebalance || d.Code != fqCodeTooSoon || d.noisy() {
			t.Fatalf("same as_of must not rebalance twice: %+v", d)
		}
		if d := fqShouldRebalance(tg, cfg, "2026-10-30", true, now); d.Rebalance || d.Code != fqCodeTooSoon {
			t.Fatalf("gap 1 < 3: %+v", d)
		}
		if d := fqShouldRebalance(tg, cfg, "2026-10-29", true, now); d.Rebalance || d.Code != fqCodeTooSoon {
			t.Fatalf("gap 2 < 3 must wait: %+v", d)
		}
		if d := fqShouldRebalance(tg, cfg, "2026-10-28", true, now); !d.Rebalance { // gap 3
			t.Fatalf("gap == horizon must trade: %+v", d)
		}
		if d := fqShouldRebalance(tg, cfg, "2026-11-05", true, now); d.Rebalance { // older than last
			t.Fatalf("older targets must not trade: %+v", d)
		}
	})
	t.Run("invalid dates", func(t *testing.T) {
		fut := fqTestTargets("2026-11-09", map[string]float64{"A": 0.1})
		if d := fqShouldRebalance(fut, cfg, "", false, now); d.Rebalance || d.Code != fqCodeInvalid {
			t.Fatalf("future as_of: %+v", d)
		}
		noAsOf := fqTestTargets("2026-10-31", map[string]float64{"A": 0.1})
		noAsOf.asOfDate = time.Time{}
		if d := fqShouldRebalance(noAsOf, cfg, "", false, now); d.Rebalance || d.Code != fqCodeInvalid {
			t.Fatalf("missing as_of: %+v", d)
		}
		if d := fqShouldRebalance(tg, cfg, "garbage", false, now); d.Rebalance || d.Code != fqCodeInvalid {
			t.Fatalf("bad last_as_of: %+v", d)
		}
	})
}

// ------------------------------------------------------------ planner

func fqLot(step float64) func(string, float64, float64) (float64, error) {
	return func(_ string, qty, _ float64) (float64, error) { return math.Floor(qty/step+1e-9) * step, nil }
}

func plannedBy(p fqPlan, sym string) []fqOrder {
	var out []fqOrder
	for _, o := range p.Orders {
		if o.Symbol == sym {
			out = append(out, o)
		}
	}
	return out
}

func TestPlannerSizingNeutralGross(t *testing.T) {
	plan := fqPlanOrders(fqPlanInput{
		Equity: 10000, CapitalFraction: 1, Band: 0.1, MinOrder: 10,
		Prices:  map[string]float64{"AUSDT": 10, "BUSDT": 20, "CUSDT": 5, "DUSDT": 50},
		Weights: map[string]float64{"AUSDT": 0.3, "BUSDT": 0.2, "CUSDT": -0.25, "DUSDT": -0.25},
		Format:  fqLot(0.001),
	})
	if plan.Budget != 10000 || len(plan.Orders) != 4 || len(plan.Skips) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	var long, short float64
	for _, o := range plan.Orders {
		if o.Kind != fqKindOpen {
			t.Fatalf("expected opens only: %+v", o)
		}
		if o.Side == fqSideLong {
			long += o.Notional
		} else {
			short += o.Notional
		}
	}
	fqNear(t, long, 5000, "long gross")
	fqNear(t, short, 5000, "short gross")
	if o := plannedBy(plan, "AUSDT")[0]; o.Action() != "open_long" || math.Abs(o.Qty-300) > 1e-9 {
		t.Fatalf("A = %+v", o)
	}
	if o := plannedBy(plan, "CUSDT")[0]; o.Action() != "open_short" || math.Abs(o.Qty-500) > 1e-9 {
		t.Fatalf("C = %+v", o)
	}
}

func TestPlannerCapitalFraction(t *testing.T) {
	plan := fqPlanOrders(fqPlanInput{Equity: 10000, CapitalFraction: 0.5, Band: 0.1, MinOrder: 10,
		Prices: map[string]float64{"AUSDT": 10}, Weights: map[string]float64{"AUSDT": 0.4}, Format: fqLot(0.001)})
	fqNear(t, plan.Budget, 5000, "budget")
	if len(plan.Orders) != 1 {
		t.Fatalf("%+v", plan)
	}
	fqNear(t, plan.Orders[0].Notional, 2000, "notional") // 0.4 * 5000
}

func TestPlannerOrderingClosesReducesIncreases(t *testing.T) {
	plan := fqPlanOrders(fqPlanInput{
		Equity: 10000, CapitalFraction: 1, Band: 0.1, MinOrder: 10,
		Positions: []fqPosition{
			{Symbol: "ZGONE", Side: fqSideLong, Qty: 10, Mark: 100},     // absent -> close
			{Symbol: "FLIPUSDT", Side: fqSideShort, Qty: 100, Mark: 10}, // wrong side -> close + reopen long
			{Symbol: "BIGUSDT", Side: fqSideLong, Qty: 100, Mark: 10},   // 1000 held, target 500 -> reduce
			{Symbol: "SMALLUSDT", Side: fqSideShort, Qty: 10, Mark: 10}, // 100 held, target 500 -> increase
			{Symbol: "SAMEUSDT", Side: fqSideLong, Qty: 50, Mark: 10},   // 500 held, target 520 -> within band
			{Symbol: "AFTERUSDT", Side: fqSideLong, Qty: 1, Mark: 10},   // 10 held, target 0 -> close (alphabetical first)
		},
		Prices:  map[string]float64{"ZGONE": 100, "FLIPUSDT": 10, "BIGUSDT": 10, "SMALLUSDT": 10, "SAMEUSDT": 10, "NEWUSDT": 10, "AFTERUSDT": 10},
		Weights: map[string]float64{"FLIPUSDT": 0.05, "BIGUSDT": 0.05, "SMALLUSDT": -0.05, "SAMEUSDT": 0.052, "NEWUSDT": -0.05},
		Format:  fqLot(0.001),
	})
	var order []string
	for _, o := range plan.Orders {
		order = append(order, o.Kind+":"+o.Symbol)
	}
	// closes first (alphabetical), then reductions, then opens/increases (alphabetical).
	want := []string{
		"close:AFTERUSDT", "close:FLIPUSDT", "close:ZGONE",
		"reduce:BIGUSDT",
		"open:FLIPUSDT", "open:NEWUSDT", "increase:SMALLUSDT",
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v\nwant    %v", order, want)
	}
	flip := plannedBy(plan, "FLIPUSDT")
	if flip[0].Action() != "close_short" || !flip[0].CloseAll || flip[1].Action() != "open_long" {
		t.Fatalf("flip = %+v", flip)
	}
	if r := plannedBy(plan, "BIGUSDT")[0]; r.Action() != "close_long" || r.Kind != fqKindReduce || math.Abs(r.Qty-50) > 1e-9 {
		t.Fatalf("reduce = %+v", r)
	}
	if inc := plannedBy(plan, "SMALLUSDT")[0]; inc.Action() != "open_short" || math.Abs(inc.Qty-40) > 1e-9 {
		t.Fatalf("increase = %+v", inc)
	}
	if s := plannedBy(plan, "SAMEUSDT"); len(s) != 0 {
		t.Fatalf("in-band symbol traded: %+v", s)
	}
	found := false
	for _, s := range plan.Skips {
		if s.Symbol == "SAMEUSDT" && strings.Contains(s.Reason, "调仓带") {
			found = true
		}
	}
	if !found {
		t.Fatalf("band skip not reported: %+v", plan.Skips)
	}
}

func TestPlannerBandBoundary(t *testing.T) {
	mk := func(held float64) fqPlan {
		return fqPlanOrders(fqPlanInput{Equity: 10000, CapitalFraction: 1, Band: 0.1, MinOrder: 1,
			Positions: []fqPosition{{Symbol: "AUSDT", Side: fqSideLong, Qty: held, Mark: 10}},
			Prices:    map[string]float64{"AUSDT": 10}, Weights: map[string]float64{"AUSDT": 0.1}, Format: fqLot(0.001)})
	}
	// target 1000: held 1000 -> nothing; held 905 (diff 95 < 100) -> nothing; held 890 (diff 110) -> increase; held 1120 (diff 120) -> reduce
	if len(mk(100).Orders) != 0 || len(mk(90.5).Orders) != 0 {
		t.Fatal("inside the band must not trade")
	}
	if o := mk(89).Orders; len(o) != 1 || o[0].Kind != fqKindIncrease {
		t.Fatalf("below band: %+v", o)
	}
	if o := mk(112).Orders; len(o) != 1 || o[0].Kind != fqKindReduce {
		t.Fatalf("above band: %+v", o)
	}
}

func TestPlannerMinOrderAndFormat(t *testing.T) {
	plan := fqPlanOrders(fqPlanInput{Equity: 1000, CapitalFraction: 1, Band: 0.1, MinOrder: 10,
		Prices:  map[string]float64{"TINYUSDT": 10, "LOTUSDT": 100000, "ERRUSDT": 10, "OKUSDT": 10, "NOPRICEUSDT": 0},
		Weights: map[string]float64{"TINYUSDT": 0.005, "LOTUSDT": 0.5, "ERRUSDT": 0.2, "OKUSDT": 0.2, "NOPRICEUSDT": 0.1},
		Format: func(s string, q, p float64) (float64, error) {
			if s == "ERRUSDT" {
				return 0, errors.New("MIN_QTY")
			}
			if s == "LOTUSDT" {
				return 0, nil // below the venue minimum
			}
			return math.Floor(q*1000) / 1000, nil
		}})
	if len(plan.Orders) != 1 || plan.Orders[0].Symbol != "OKUSDT" {
		t.Fatalf("orders = %+v", plan.Orders)
	}
	reasons := map[string]string{}
	for _, s := range plan.Skips {
		reasons[s.Symbol] = s.Reason
	}
	for _, sym := range []string{"TINYUSDT", "LOTUSDT", "ERRUSDT", "NOPRICEUSDT"} {
		if reasons[sym] == "" {
			t.Errorf("%s not reported as skipped: %v", sym, reasons)
		}
	}
	if !strings.Contains(reasons["TINYUSDT"], "最小下单额") || !strings.Contains(reasons["NOPRICEUSDT"], "价格") {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestPlannerClosesIgnoreMinOrderAndMissingPrice(t *testing.T) {
	plan := fqPlanOrders(fqPlanInput{Equity: 1000, CapitalFraction: 1, Band: 0.1, MinOrder: 100,
		Positions: []fqPosition{{Symbol: "DUSTUSDT", Side: fqSideShort, Qty: 0.001, Mark: 2}, {Symbol: "NOPXUSDT", Side: fqSideLong, Qty: 5, Mark: 3}},
		Prices:    map[string]float64{}, Weights: map[string]float64{}})
	if len(plan.Orders) != 2 {
		t.Fatalf("absent symbols must be closed whatever their size / price feed: %+v", plan.Orders)
	}
	for _, o := range plan.Orders {
		if !o.CloseAll || o.Kind != fqKindClose {
			t.Fatalf("%+v", o)
		}
	}
}

func TestPlannerLongOnlyIgnoresShorts(t *testing.T) {
	plan := fqPlanOrders(fqPlanInput{Equity: 1000, CapitalFraction: 1, Band: 0.1, MinOrder: 10, LongOnly: true,
		Prices:  map[string]float64{"AAPLBUSDT": 100, "TSLABUSDT": 100},
		Weights: map[string]float64{"AAPLBUSDT": 0.5, "TSLABUSDT": -0.5}})
	if len(plan.Orders) != 1 || plan.Orders[0].Symbol != "AAPLBUSDT" || plan.Orders[0].Side != fqSideLong {
		t.Fatalf("%+v", plan.Orders)
	}
}

func TestPlannerReduceCoveringWholePositionBecomesClose(t *testing.T) {
	plan := fqPlanOrders(fqPlanInput{Equity: 1000, CapitalFraction: 1, Band: 0.1, MinOrder: 10,
		Positions: []fqPosition{{Symbol: "AUSDT", Side: fqSideLong, Qty: 10, Mark: 10}},
		Prices:    map[string]float64{"AUSDT": 10}, Weights: map[string]float64{"AUSDT": 0.001}, Format: fqLot(1)})
	// target 1, held 100: diff 99 -> qty 9.9 -> lot 9 < 10 held: reduce, not close.
	if o := plan.Orders; len(o) != 1 || o[0].Kind != fqKindReduce {
		t.Fatalf("%+v", o)
	}
	plan = fqPlanOrders(fqPlanInput{Equity: 1000, CapitalFraction: 1, Band: 0.1, MinOrder: 10,
		Positions: []fqPosition{{Symbol: "AUSDT", Side: fqSideLong, Qty: 10, Mark: 10}},
		Prices:    map[string]float64{"AUSDT": 10}, Weights: map[string]float64{"AUSDT": 0.0001}, Format: nil})
	// target 0.1: diff 99.9 -> 9.99 < 10 -> reduce again (a dust target is not a close).
	if o := plan.Orders; len(o) != 1 || o[0].Kind != fqKindReduce {
		t.Fatalf("%+v", o)
	}
}

// ------------------------------------------------------------ crypto executor

func TestFuturesBrokerExecutesPlanAndPlacesBothStops(t *testing.T) {
	f := newFakeFutures(10000, map[string]float64{"LUSDT": 10, "SUSDT": 20, "OLDUSDT": 5})
	f.pos["OLDUSDT:long"] = &fakePos{sym: "OLDUSDT", side: "long", qty: 100, entry: 5}
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	tg := fqTestTargets("2026-10-31", map[string]float64{"LUSDT": 0.5, "SUSDT": -0.5})
	rep := fqRunRebalance(b, tg, fqPlanConfig{CapitalFraction: 1, Band: 0.1, MinOrder: 10})
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}
	if len(rep.Results) != 3 || rep.failedCount() != 0 {
		t.Fatalf("results = %+v", rep.Results)
	}
	// close first, then opens.
	if rep.Results[0].Order.Symbol != "OLDUSDT" || rep.Results[0].Order.Action() != "close_long" {
		t.Fatalf("first order = %+v", rep.Results[0].Order)
	}
	if _, held := f.pos["OLDUSDT:long"]; held {
		t.Fatal("absent symbol not closed")
	}
	long, short := f.pos["LUSDT:long"], f.pos["SUSDT:short"]
	if long == nil || short == nil {
		t.Fatalf("positions: %+v", f.pos)
	}
	fqNear(t, long.qty*10, 5000, "long notional")
	fqNear(t, short.qty*20, 5000, "short notional")
	if f.lev["LUSDT"] != 3 || f.lev["SUSDT"] != 3 {
		t.Fatalf("leverage not set per symbol: %v", f.lev)
	}
	stop, ok := f.stops["SUSDT:SHORT"]
	if !ok || math.Abs(stop-24) > 1e-9 { // entry 20 * 1.2
		t.Fatalf("short stop = %v (%v) calls=%v", stop, ok, f.calls)
	}
	fqNear(t, f.stops["LUSDT:LONG"], 8, "long stop")
	var shortRes fqResult
	for _, r := range rep.Results {
		if r.Order.Symbol == "SUSDT" {
			shortRes = r
		}
	}
	fqNear(t, shortRes.StopPrice, 24, "recorded stop")
	if rep.After == nil || len(rep.After.Positions) != 2 {
		t.Fatalf("after snapshot = %+v", rep.After)
	}
}

func TestFuturesBrokerRestopsAfterIncreasingShort(t *testing.T) {
	f := newFakeFutures(10000, map[string]float64{"SUSDT": 20})
	f.pos["SUSDT:short"] = &fakePos{sym: "SUSDT", side: "short", qty: 100, entry: 18} // 2000 held at mark 20, average entry 18
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	tg := fqTestTargets("2026-10-31", map[string]float64{"SUSDT": -0.5})
	rep := fqRunRebalance(b, tg, fqPlanConfig{CapitalFraction: 1, Band: 0.1, MinOrder: 10})
	if rep.Err != nil || len(rep.Results) != 1 || rep.Results[0].Order.Kind != fqKindIncrease {
		t.Fatalf("%+v %v", rep.Results, rep.Err)
	}
	// increase 150 @20 -> avg entry = (18*100+20*150)/250 = 19.2 -> stop 23.04
	if stop := f.stops["SUSDT:SHORT"]; math.Abs(stop-23.04) > 1e-9 {
		t.Fatalf("stop after increase = %v", stop)
	}
}

func TestFuturesBrokerDisabledAndReducing(t *testing.T) {
	f := newFakeFutures(10000, map[string]float64{"SUSDT": 20})
	b := newFQFuturesBroker(f, 3, 0, 0) // stop disabled
	rep := fqRunRebalance(b, fqTestTargets("2026-10-31", map[string]float64{"SUSDT": -0.5}), fqPlanConfig{CapitalFraction: 1, Band: 0.1, MinOrder: 10})
	if rep.failedCount() != 0 || len(f.stops) != 0 {
		t.Fatalf("stop placed with short_stop_pct=0: %v", f.stops)
	}
	f2 := newFakeFutures(10000, map[string]float64{"SUSDT": 20})
	f2.pos["SUSDT:short"] = &fakePos{sym: "SUSDT", side: "short", qty: 1000, entry: 20}
	b2 := newFQFuturesBroker(f2, 3, 0.2, 0.2)
	rep = fqRunRebalance(b2, fqTestTargets("2026-10-31", map[string]float64{"SUSDT": -0.5}), fqPlanConfig{CapitalFraction: 1, Band: 0.1, MinOrder: 10})
	if len(rep.Results) != 1 || rep.Results[0].Order.Kind != fqKindReduce || len(f2.stops) != 1 {
		t.Fatalf("reduce must re-place stops: %+v %v", rep.Results, f2.stops)
	}
}

func TestFuturesBrokerFailureContinues(t *testing.T) {
	f := newFakeFutures(10000, map[string]float64{"AUSDT": 10, "BUSDT": 10, "CUSDT": 10, "DUSDT": 10})
	f.failOpen["AUSDT"] = true
	f.failStop["CUSDT"] = true
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	tg := fqTestTargets("2026-10-31", map[string]float64{"AUSDT": 0.2, "BUSDT": 0.2, "CUSDT": -0.2, "DUSDT": -0.2})
	rep := fqRunRebalance(b, tg, fqPlanConfig{CapitalFraction: 1, Band: 0.1, MinOrder: 10})
	if rep.Err != nil || len(rep.Results) != 4 {
		t.Fatalf("%+v %v", rep.Results, rep.Err)
	}
	by := map[string]fqResult{}
	for _, r := range rep.Results {
		by[r.Order.Symbol] = r
	}
	if by["AUSDT"].OK || !strings.Contains(by["AUSDT"].Err, "margin") {
		t.Fatalf("A = %+v", by["AUSDT"])
	}
	if !by["BUSDT"].OK || !by["DUSDT"].OK || by["DUSDT"].StopPrice == 0 {
		t.Fatalf("later orders must still run: B=%+v D=%+v", by["BUSDT"], by["DUSDT"])
	}
	if !by["CUSDT"].OK || by["CUSDT"].StopErr == "" || !by["CUSDT"].failed() {
		t.Fatalf("stop failure must be recorded on a filled order: %+v", by["CUSDT"])
	}
	if rep.failedCount() != 2 {
		t.Fatalf("failed = %d", rep.failedCount())
	}
}

func TestRebalancePriceFailureSkipsOnlyThatSymbol(t *testing.T) {
	f := newFakeFutures(10000, map[string]float64{"AUSDT": 10}) // BUSDT has no price
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	tg := fqTestTargets("2026-10-31", map[string]float64{"AUSDT": 0.3, "BUSDT": 0.3})
	rep := fqRunRebalance(b, tg, fqPlanConfig{CapitalFraction: 1, Band: 0.1, MinOrder: 10})
	if len(rep.Results) != 1 || rep.PriceErrs["BUSDT"] == "" {
		t.Fatalf("%+v %v", rep.Results, rep.PriceErrs)
	}
}

func TestRebalanceAbortsOnBadEquity(t *testing.T) {
	f := newFakeFutures(0, map[string]float64{"AUSDT": 10})
	rep := fqRunRebalance(newFQFuturesBroker(f, 3, 0.2, 0.2), fqTestTargets("2026-10-31", map[string]float64{"AUSDT": 0.3}), fqPlanConfig{CapitalFraction: 1, Band: 0.1, MinOrder: 10})
	if rep.Err == nil || len(rep.Results) != 0 {
		t.Fatalf("%+v", rep)
	}
}

// ------------------------------------------------------------ paper-only guard

func fqTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "fq.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func fqStrategy(market string) *store.StrategyConfig {
	return &store.StrategyConfig{StrategyType: store.StrategyTypeFactorQuant, FactorQuantConfig: &store.FactorQuantConfig{Market: market}}
}

func TestFactorQuantRejectsLiveExchanges(t *testing.T) {
	st := fqTestStore(t)
	for _, market := range []string{"crypto", "us"} {
		for _, exchange := range []string{"binance", "bybit", "okx", "hyperliquid", "binance_stocks", ""} {
			cfg := AutoTraderConfig{ID: "fq-" + market + "-" + exchange, Name: "fq", Exchange: exchange, InitialBalance: 1000,
				StrategyConfig: fqStrategy(market), BinanceAPIKey: "live-key", BinanceSecretKey: "live-secret"}
			at, err := NewAutoTrader(cfg, st, "u")
			if err == nil {
				t.Fatalf("%s/%q: live exchange accepted", market, exchange)
			}
			if at != nil {
				t.Fatalf("%s/%q: trader built despite error", market, exchange)
			}
			if exchange != "" && !strings.Contains(err.Error(), "模拟盘") {
				t.Fatalf("%s/%q: error not the paper-only message: %v", market, exchange, err)
			}
		}
	}
	// A missing or invalid factor_quant config never constructs either.
	bad := AutoTraderConfig{ID: "fq-nocfg", Name: "fq", Exchange: "paper", ExchangeID: "x", InitialBalance: 1000,
		StrategyConfig: &store.StrategyConfig{StrategyType: store.StrategyTypeFactorQuant}}
	if _, err := NewAutoTrader(bad, st, "u"); err == nil {
		t.Fatal("factor_quant without config accepted")
	}
}

func TestFactorQuantBrokerRefusesNonPaperTrader(t *testing.T) {
	at := &AutoTrader{exchange: "paper", config: AutoTraderConfig{StrategyConfig: fqStrategy("crypto")}, trader: nil}
	if _, err := at.newFactorQuantBroker(); err == nil {
		t.Fatal("crypto broker built on a non-paper executor")
	}
	at.exchange = "binance"
	if _, err := at.newFactorQuantBroker(); err == nil {
		t.Fatal("broker built for a live exchange")
	}
	at = &AutoTrader{exchange: "binance", config: AutoTraderConfig{StrategyConfig: fqStrategy("us")}}
	if _, err := at.newFactorQuantBroker(); err == nil {
		t.Fatal("us broker built for a live exchange")
	}
}

// ------------------------------------------------------------ integration: paper account

func fqPaperTrader(t *testing.T, st *store.Store, id string, market string, prices *paperTestPrices) *AutoTrader {
	t.Helper()
	cfg := AutoTraderConfig{ID: id, Name: "FQ " + market, Exchange: "paper", ExchangeID: id, InitialBalance: 10000,
		PaperConfig:      &paper.Config{ExchangeID: id, InitialBalance: 10000, TakerFeeBps: 5, MakerFeeBps: 2, SlippageBps: 0, DefaultLeverage: 3},
		PaperPriceSource: prices, StrategyConfig: fqStrategy(market)}
	// Legacy execution tests use independent fake equity (often 1000 against
	// this paper account's 10000). Keep portfolio breakers off in this fixture;
	// risk tests explicitly enable the defaults with matching account equity.
	cfg.StrategyConfig.FactorQuantConfig.MaxDrawdownPct = fqPtr(0)
	cfg.StrategyConfig.FactorQuantConfig.DrawdownWarnPct = fqPtr(0)
	cfg.StrategyConfig.FactorQuantConfig.DailyLossPct = fqPtr(0)
	at, err := NewAutoTrader(cfg, st, "u")
	if err != nil {
		t.Fatalf("NewAutoTrader: %v", err)
	}
	t.Cleanup(at.Stop)
	return at
}

func writeTargetsFile(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "quant_targets.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func targetsJSON(market, status, asOf string, horizon int, weights string) string {
	return fmt.Sprintf(`{"version":1,"market":%q,"status":%q,"reason":null,"generated_at":"%sT19:00:00+00:00","as_of":%q,"run_id":"run-%s","horizon":%d,"rule":{"name":"ls_invvol_protected_v1"},"weights":%s,"excluded":{"XUSDT":"short_ret7_top_decile"}}`,
		market, status, asOf, asOf, asOf, horizon, weights)
}

func TestFactorQuantTickOnPaperAccount(t *testing.T) {
	st := fqTestStore(t)
	prices := &paperTestPrices{mark: 100}
	at := fqPaperTrader(t, st, "fq-crypto-1", "crypto", prices)
	if !at.IsFactorQuantStrategy() || at.isFactorQuantUS() {
		t.Fatal("strategy detection")
	}
	if _, ok := at.trader.(*paper.PaperFuturesTrader); !ok {
		t.Fatalf("executor %T", at.trader)
	}
	broker, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "ok", "2026-10-31", 3, `{"LONGUSDT":0.5,"SHORTUSDT":-0.5}`))
	day := fqDay("2026-11-01")
	at.fqNow = func() time.Time { return day }

	at.factorQuantTick(broker, day)

	positions, err := at.trader.GetPositions()
	if err != nil || len(positions) != 2 {
		t.Fatalf("positions %v %v", positions, err)
	}
	var sawShort bool
	for _, p := range positions {
		if p["symbol"] == "SHORTUSDT" {
			sawShort = true
			if p["side"] != "short" || math.Abs(p["positionAmt"].(float64)+50) > 1e-6 { // 5000 / 100
				t.Fatalf("short = %v", p)
			}
		}
	}
	if !sawShort {
		t.Fatal("no short opened")
	}
	// The short stop sits on the paper account at entry*1.2.
	orders, err := at.trader.GetOpenOrders("SHORTUSDT")
	if err != nil || len(orders) != 1 || !strings.Contains(orders[0].Type, "STOP") || math.Abs(orders[0].StopPrice-120) > 1e-6 {
		t.Fatalf("short stop orders %+v %v", orders, err)
	}
	state, err := st.FactorQuant().Get(at.id)
	if err != nil || state == nil || state.LastAsOf != "2026-10-31" || state.LastRule != "ls_invvol_protected_v1" {
		t.Fatalf("state %+v %v", state, err)
	}
	recs, err := st.Decision().GetLatestRecords(at.id, 10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("decision records %d %v", len(recs), err)
	}
	r := recs[0]
	for _, want := range []string{"2026-10-31", "ls_invvol_protected_v1", "horizon=3", "open_short", "SHORTUSDT", "止损（空）", "仅模拟盘"} {
		if !strings.Contains(r.CoTTrace, want) {
			t.Errorf("record summary lacks %q:\n%s", want, r.CoTTrace)
		}
	}
	if !r.Success || len(r.Decisions) != 2 || r.SystemPrompt != "" || r.InputPrompt != "" || r.RawResponse != "" {
		t.Fatalf("record = %+v", r)
	}
	snaps, err := st.Equity().GetByTimeRange(at.id, time.Unix(0, 0), time.Now().AddDate(1, 0, 0))
	if err != nil || len(snaps) != 1 || snaps[0].TotalEquity <= 0 || snaps[0].PositionCount != 2 {
		t.Fatalf("equity snapshots %+v %v", snaps, err)
	}

	// Same targets a tick later: no second rebalance, no new decision record.
	at.factorQuantTick(broker, day.Add(5*time.Minute))
	recs, _ = st.Decision().GetLatestRecords(at.id, 10)
	if len(recs) != 1 {
		t.Fatalf("same as_of rebalanced twice: %d records", len(recs))
	}

	// A restart (fresh AutoTrader on the same store) must not rebalance again either.
	at.Stop()
	restarted := fqPaperTrader(t, st, "fq-crypto-1", "crypto", prices)
	restarted.config.StrategyConfig.FactorQuantConfig.TargetsPath = at.config.StrategyConfig.FactorQuantConfig.TargetsPath
	b2, err := restarted.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	restarted.factorQuantTick(b2, day.Add(10*time.Minute))
	recs, _ = st.Decision().GetLatestRecords(at.id, 10)
	if len(recs) != 1 {
		t.Fatalf("restart rebalanced twice: %d records", len(recs))
	}
	// Targets whose as_of is only 2 days after the last rebalance (< horizon 3) wait...
	restarted.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "ok", "2026-11-02", 3, `{"LONGUSDT":0.5,"SHORTUSDT":-0.5}`))
	restarted.factorQuantTick(b2, fqDay("2026-11-03"))
	recs, _ = st.Decision().GetLatestRecords(at.id, 10)
	if len(recs) != 1 {
		t.Fatalf("rebalanced before the horizon: %d records", len(recs))
	}
	// ...and as_of 3 days after (== horizon) rebalances again.
	restarted.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "ok", "2026-11-03", 3, `{"LONGUSDT":0.5,"SHORTUSDT":-0.5}`))
	restarted.factorQuantTick(b2, fqDay("2026-11-04"))
	recs, _ = st.Decision().GetLatestRecords(at.id, 10)
	if len(recs) != 2 {
		t.Fatalf("horizon reached but no rebalance: %d records", len(recs))
	}
	if state, _ := st.FactorQuant().Get(at.id); state == nil || state.LastAsOf != "2026-11-03" {
		t.Fatalf("state after second rebalance %+v", state)
	}
}

func TestFactorQuantTickSkipsAndRecordsOncePerAsOf(t *testing.T) {
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-crypto-2", "crypto", &paperTestPrices{mark: 100})
	broker, _ := at.newFactorQuantBroker()
	dir := t.TempDir()
	path := writeTargetsFile(t, dir, targetsJSON("crypto", "insufficient_universe", "2026-10-31", 3, `{}`))
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = path
	day := fqDay("2026-11-01")
	for i := 0; i < 5; i++ {
		at.factorQuantTick(broker, day.Add(time.Duration(i)*5*time.Minute))
	}
	recs, _ := st.Decision().GetLatestRecords(at.id, 10)
	if len(recs) != 1 {
		t.Fatalf("status skip recorded %d times, want once per as_of", len(recs))
	}
	if !strings.Contains(recs[0].CoTTrace, "insufficient_universe") || !strings.Contains(recs[0].CoTTrace, "不开新仓") {
		t.Fatalf("skip record = %s", recs[0].CoTTrace)
	}
	if state, _ := st.FactorQuant().Get(at.id); state != nil && state.LastAsOf != "" {
		t.Fatalf("a skipped day must not mark a rebalance: %+v", state)
	}
	if positions, _ := at.trader.GetPositions(); len(positions) != 0 {
		t.Fatalf("positions opened on a non-ok status: %v", positions)
	}
	// The next as_of is a new skip record; a restart does not repeat the old one.
	restarted := fqPaperTrader(t, st, "fq-crypto-2", "crypto", &paperTestPrices{mark: 100})
	restarted.config.StrategyConfig.FactorQuantConfig.TargetsPath = path
	b2, _ := restarted.newFactorQuantBroker()
	restarted.factorQuantTick(b2, day.Add(time.Hour))
	if recs, _ = st.Decision().GetLatestRecords(at.id, 10); len(recs) != 1 {
		t.Fatalf("restart repeated the skip record: %d", len(recs))
	}
	restarted.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "insufficient_universe", "2026-11-01", 3, `{}`))
	restarted.factorQuantTick(b2, fqDay("2026-11-02"))
	if recs, _ = st.Decision().GetLatestRecords(at.id, 10); len(recs) != 2 {
		t.Fatalf("new as_of must record once more: %d", len(recs))
	}
}

func TestFactorQuantTickSurvivesBadTargetsFiles(t *testing.T) {
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-crypto-3", "crypto", &paperTestPrices{mark: 100})
	broker, _ := at.newFactorQuantBroker()
	dir := t.TempDir()
	cfg := at.config.StrategyConfig.FactorQuantConfig
	day := fqDay("2026-11-01")

	cfg.TargetsPath = filepath.Join(dir, "missing.json")
	at.factorQuantTick(broker, day)
	good := targetsJSON("crypto", "ok", "2026-10-31", 3, `{"LONGUSDT":0.5,"SHORTUSDT":-0.5}`)
	cfg.TargetsPath = writeTargetsFile(t, dir, good[:len(good)/2]) // half-written file
	at.factorQuantTick(broker, day)
	cfg.TargetsPath = writeTargetsFile(t, dir, strings.Replace(good, `"version":1`, `"version":9`, 1))
	at.factorQuantTick(broker, day)

	if positions, _ := at.trader.GetPositions(); len(positions) != 0 {
		t.Fatalf("traded on an unreadable file: %v", positions)
	}
	if recs, _ := st.Decision().GetLatestRecords(at.id, 10); len(recs) != 0 {
		t.Fatalf("unreadable files must only skip the tick, got %d records", len(recs))
	}
	// The file becomes valid: the very next tick trades.
	cfg.TargetsPath = writeTargetsFile(t, dir, good)
	at.factorQuantTick(broker, day)
	if positions, _ := at.trader.GetPositions(); len(positions) != 2 {
		t.Fatalf("positions after the file became valid: %v", positions)
	}
}

func TestFactorQuantRefusesUnmanagedPositions(t *testing.T) {
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-crypto-4", "crypto", &paperTestPrices{mark: 100})
	broker, _ := at.newFactorQuantBroker()
	if _, err := at.trader.(*paper.PaperFuturesTrader).OpenLong("MANUALUSDT", 1, 3); err != nil {
		t.Fatal(err)
	}
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{"LONGUSDT":0.5,"SHORTUSDT":-0.5}`))
	at.factorQuantTick(broker, fqDay("2026-11-01"))
	positions, _ := at.trader.GetPositions()
	if len(positions) != 1 || positions[0]["symbol"] != "MANUALUSDT" {
		t.Fatalf("positions = %v", positions)
	}
	recs, _ := st.Decision().GetLatestRecords(at.id, 5)
	if len(recs) != 1 || !strings.Contains(recs[0].CoTTrace, "没有调仓记录") {
		t.Fatalf("records = %+v", recs)
	}
}

// ------------------------------------------------------------ US paper ledger

func fqStubUS(t *testing.T, prices map[string]float64) {
	t.Helper()
	oldQuote, oldLookup := stockGetQuote, stockLookupSymbol
	stockGetQuote = func(_ context.Context, sym string) (*usstock.Quote, error) {
		p, ok := prices[sym]
		if !ok {
			return nil, fmt.Errorf("unknown bStock %s", sym)
		}
		return &usstock.Quote{Symbol: sym, BStockPrice: p}, nil
	}
	stockLookupSymbol = func(_ context.Context, sym string) (usstock.SymbolInfo, bool) {
		if _, ok := prices[sym]; !ok {
			return usstock.SymbolInfo{}, false
		}
		return usstock.SymbolInfo{Symbol: sym, StepSize: 0.01, MinQty: 0.01, MinNotional: 5, TickSize: 0.01}, true
	}
	t.Cleanup(func() { stockGetQuote, stockLookupSymbol = oldQuote, oldLookup })
}

func TestFactorQuantUSPaperLedgerLongOnly(t *testing.T) {
	st := fqTestStore(t)
	fqStubUS(t, map[string]float64{"AAPLBUSDT": 200, "MSFTBUSDT": 400})
	at := fqPaperTrader(t, st, "fq-us-1", "us", &paperTestPrices{mark: 100})
	if !at.isFactorQuantUS() {
		t.Fatal("us detection")
	}
	at.config.StrategyConfig.FactorQuantConfig.USFeeBps = fqPtr(0) // this test is about the ledger mechanics; costs have their own test
	at.config.StrategyConfig.FactorQuantConfig.USSlippageBps = fqPtr(0)
	broker, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := broker.(*fqStockBroker); !ok {
		t.Fatalf("broker %T", broker)
	}
	if at.stockTrader != nil {
		t.Fatal("a live spot executor was built")
	}
	dir := t.TempDir()
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.TargetsPath = writeTargetsFile(t, dir, strings.Replace(targetsJSON("us", "no_validated_factors", "2026-10-31", 5, `{}`), `"market":"us"`, `"market":"us"`, 1))
	day := fqDay("2026-11-01")
	at.factorQuantTick(broker, day)
	if open, _ := st.StockPaper().ListOpen(at.id); len(open) != 0 {
		t.Fatalf("idle status traded: %+v", open)
	}

	cfg.TargetsPath = writeTargetsFile(t, dir, targetsJSON("us", "ok", "2026-11-01", 5, `{"AAPLBUSDT":0.6,"MSFTBUSDT":0.4}`))
	at.factorQuantTick(broker, fqDay("2026-11-02"))
	open, err := st.StockPaper().ListOpen(at.id)
	if err != nil || len(open) != 2 {
		t.Fatalf("open = %+v %v", open, err)
	}
	for _, p := range open {
		switch p.Symbol {
		case "AAPLBUSDT":
			fqNear(t, p.Quantity*200, 6000, "AAPL notional")
		case "MSFTBUSDT":
			fqNear(t, p.Quantity*400, 4000, "MSFT notional")
		}
	}
	acct, _ := st.StockPaper().GetAccount(at.id)
	fqNear(t, acct.Cash, 0, "cash after full investment")
	if state, _ := st.FactorQuant().Get(at.id); state == nil || state.LastAsOf != "2026-11-01" {
		t.Fatalf("state %+v", state)
	}

	// Next rebalance (>= 5 days later): drop MSFT, keep AAPL in band -> only a close.
	cfg.TargetsPath = writeTargetsFile(t, dir, targetsJSON("us", "ok", "2026-11-06", 5, `{"AAPLBUSDT":0.6}`))
	at.factorQuantTick(broker, fqDay("2026-11-07"))
	open, _ = st.StockPaper().ListOpen(at.id)
	if len(open) != 1 || open[0].Symbol != "AAPLBUSDT" {
		t.Fatalf("after second rebalance: %+v", open)
	}
	acct, _ = st.StockPaper().GetAccount(at.id)
	fqNear(t, acct.Cash, 4000, "cash after closing MSFT")

	info, err := at.GetAccountInfo()
	if err != nil || info["paper"] != true || info["position_count"] != 1 {
		t.Fatalf("account info %v %v", info, err)
	}
	pos, err := at.GetPositions()
	if err != nil || len(pos) != 1 || pos[0]["symbol"] != "AAPLBUSDT" {
		t.Fatalf("positions %v %v", pos, err)
	}
	snaps, _ := st.Equity().GetByTimeRange(at.id, time.Unix(0, 0), time.Now().AddDate(1, 0, 0))
	if len(snaps) < 2 {
		t.Fatalf("equity snapshots: %d", len(snaps))
	}
}

func TestStockBrokerCashGuardAndUnknownSymbol(t *testing.T) {
	st := fqTestStore(t)
	fqStubUS(t, map[string]float64{"AAPLBUSDT": 200})
	at := fqPaperTrader(t, st, "fq-us-2", "us", &paperTestPrices{mark: 100})
	b := &fqStockBroker{at: at}
	// Not enough cash: the ledger refuses to go negative.
	res := b.Place(fqOrder{Symbol: "AAPLBUSDT", Kind: fqKindOpen, Side: fqSideLong, Qty: 1000, Price: 200, Notional: 200000})
	if res.OK || !strings.Contains(res.Err, "现金不足") {
		t.Fatalf("overspend accepted: %+v", res)
	}
	if res := b.Place(fqOrder{Symbol: "AAPLBUSDT", Kind: fqKindOpen, Side: fqSideShort, Qty: 1, Price: 200}); res.OK {
		t.Fatal("us ledger accepted a short")
	}
	if q, err := b.FormatQty("NOPE", 1, 1); err == nil || q != 0 {
		t.Fatalf("unknown symbol formatted: %v %v", q, err)
	}
	if q, err := b.FormatQty("AAPLBUSDT", 0.0049, 200); err != nil || q != 0 {
		t.Fatalf("below lot should be 0: %v %v", q, err)
	}
}

// ------------------------------------------------------------ run() dispatch

func TestRunFactorQuantBranchSkipsAIAndFuturesMonitors(t *testing.T) {
	st := fqTestStore(t)
	if err := st.Trader().Create(&store.Trader{ID: "fq-run", UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	prices := &paperTestPrices{mark: 100}
	at := fqPaperTrader(t, st, "fq-run", "crypto", prices)
	dir := t.TempDir()
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "ok", "2026-10-31", 3, `{"LONGUSDT":0.5,"SHORTUSDT":-0.5}`))
	at.fqNow = func() time.Time { return fqDay("2026-11-01") }
	// Any LLM call would hit this client: it must stay untouched.
	at.mcpClient = nil
	futures := 0
	onFuturesMonitorsStart = func(*AutoTrader) { futures++ }
	t.Cleanup(func() { onFuturesMonitorsStart = nil })

	if err := at.beginRun(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- at.run() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if state, _ := st.FactorQuant().Get(at.id); state != nil && state.LastAsOf == "2026-10-31" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	at.Stop()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if futures != 0 {
		t.Fatal("futures AI monitors started for a factor_quant trader")
	}
	if positions, _ := at.trader.GetPositions(); len(positions) != 2 {
		t.Fatalf("the loop did not rebalance: %v", positions)
	}
	if recs, _ := st.Decision().GetLatestRecords(at.id, 10); len(recs) != 1 || recs[0].SystemPrompt != "" {
		t.Fatalf("records %+v", recs)
	}
}

func TestFactorQuantUSCostsSlippageFeeAndCashCheck(t *testing.T) {
	st := fqTestStore(t)
	fqStubUS(t, map[string]float64{"AAPLBUSDT": 200, "MSFTBUSDT": 400})
	at := fqPaperTrader(t, st, "fq-us-costs", "us", &paperTestPrices{mark: 100})
	// Defaults: fee 10 bp, slippage 3 bp.
	b, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	sb := b.(*fqStockBroker)
	if sb.feeBps != 10 || sb.slipBps != 3 {
		t.Fatalf("default costs fee=%v slip=%v", sb.feeBps, sb.slipBps)
	}
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("us", "ok", "2026-11-01", 5, `{"AAPLBUSDT":0.6,"MSFTBUSDT":0.4}`))
	at.factorQuantTick(b, fqDay("2026-11-02"))

	open, err := st.StockPaper().ListOpen(at.id)
	if err != nil || len(open) != 2 {
		t.Fatalf("open = %+v %v", open, err)
	}
	cash0 := 10000.0
	spent, fees := 0.0, 0.0
	for _, p := range open {
		quote := map[string]float64{"AAPLBUSDT": 200, "MSFTBUSDT": 400}[p.Symbol]
		fqNear(t, p.AvgPrice, quote*1.0003, p.Symbol+" fill price (buy slips up)")
		wantFee := p.Quantity * p.AvgPrice * 0.001
		fqNear(t, p.Fee, wantFee, p.Symbol+" recorded fee")
		spent += p.Quantity * p.AvgPrice
		fees += p.Fee
	}
	acct, _ := st.StockPaper().GetAccount(at.id)
	fqNear(t, acct.Cash, cash0-spent-fees, "cash = start - cost - fees")
	if acct.Cash < -1e-9 {
		t.Fatalf("cost buffer should keep a fully invested target affordable, cash=%v", acct.Cash)
	}
	if fees <= 0 {
		t.Fatal("no fee recorded")
	}
	recs, _ := st.Decision().GetLatestRecords(at.id, 5)
	if len(recs) != 1 || !strings.Contains(recs[0].CoTTrace, "手续费") || !strings.Contains(recs[0].CoTTrace, "1+成本") {
		t.Fatalf("record lacks cost detail: %+v", recs)
	}

	// Selling slips down and pays the fee.
	cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("us", "ok", "2026-11-06", 5, `{"AAPLBUSDT":0.6}`))
	cashBefore := acct.Cash
	at.factorQuantTick(b, fqDay("2026-11-07"))
	closed, _ := st.StockPaper().ListClosed(at.id, 5)
	if len(closed) != 1 || closed[0].Symbol != "MSFTBUSDT" {
		t.Fatalf("closed = %+v", closed)
	}
	c := closed[0]
	fqNear(t, c.ExitPrice, 400*(1-0.0003), "sell fill price slips down")
	buyFee := 0.0
	for _, p := range open {
		if p.Symbol == "MSFTBUSDT" {
			buyFee = p.Fee
		}
	}
	sellFee := c.ExitedQty * c.ExitPrice * 0.001
	fqNear(t, c.Fee, buyFee+sellFee, "closed row carries buy + sell fees")
	acct2, _ := st.StockPaper().GetAccount(at.id)
	fqNear(t, acct2.Cash, cashBefore+c.ExitedQty*c.ExitPrice-sellFee, "cash after the sell")
}

func TestStockBrokerCashCheckIncludesFee(t *testing.T) {
	st := fqTestStore(t)
	fqStubUS(t, map[string]float64{"AAPLBUSDT": 100})
	at := fqPaperTrader(t, st, "fq-us-cashfee", "us", &paperTestPrices{mark: 100})
	b := &fqStockBroker{at: at, feeBps: 100, slipBps: 0} // 1% fee
	// Notional 10000 == cash, but the fee pushes the cost over: must be refused.
	res := b.Place(fqOrder{Symbol: "AAPLBUSDT", Kind: fqKindOpen, Side: fqSideLong, Qty: 100, Price: 100, Notional: 10000})
	if res.OK || !strings.Contains(res.Err, "含手续费") {
		t.Fatalf("cost + fee > cash accepted: %+v", res)
	}
	res = b.Place(fqOrder{Symbol: "AAPLBUSDT", Kind: fqKindOpen, Side: fqSideLong, Qty: 99, Price: 100, Notional: 9900})
	if !res.OK {
		t.Fatalf("affordable order refused: %+v", res)
	}
	fqNear(t, res.Fee, 99, "fee")
	acct, _ := st.StockPaper().GetAccount(at.id)
	fqNear(t, acct.Cash, 10000-9900-99, "cash")
}

// ------------------------------------------------------------ retries and stale flatten

func fqSetOwned(t *testing.T, at *AutoTrader, asOf string) {
	t.Helper()
	if err := at.store.FactorQuant().SetRebalanced(at.id, asOf, "owned-run", "owned-rule", fqDay(asOf)); err != nil {
		t.Fatal(err)
	}
}

func fqCheckRetry(t *testing.T, at *AutoTrader, asOf string, attempts int) *store.FactorQuantState {
	t.Helper()
	state, err := at.store.FactorQuant().Get(at.id)
	if err != nil || state == nil || state.RetryAsOf != asOf || state.RetryAttempts != attempts {
		t.Fatalf("retry state=%+v err=%v, want as_of=%q attempts=%d", state, err, asOf, attempts)
	}
	return state
}

func fqRecords(t *testing.T, at *AutoTrader) []*store.DecisionRecord {
	t.Helper()
	recs, err := at.store.Decision().GetLatestRecords(at.id, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Store returns oldest first; these checks inspect the most recent pass.
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs
}

func fqCountCalls(f *fakeFutures, prefix string) int {
	n := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

func TestFactorQuantRetryReplansAndRepairsShortStops(t *testing.T) {
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-retry", "crypto", &paperTestPrices{mark: 10})
	fqSetOwned(t, at, "2026-10-28")
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{"LONGUSDT":0.5,"SHORTUSDT":-0.5}`))
	f := newFakeFutures(1000, map[string]float64{"FAILUSDT": 10, "CLOSEDUSDT": 10, "LONGUSDT": 10, "SHORTUSDT": 10})
	f.pos["FAILUSDT:long"] = &fakePos{"FAILUSDT", "long", 10, 10}
	f.pos["CLOSEDUSDT:long"] = &fakePos{"CLOSEDUSDT", "long", 10, 10}
	f.failClose["FAILUSDT"], f.failStop["SHORTUSDT"] = true, true
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	state := fqCheckRetry(t, at, "2026-10-31", 0)
	if state.LastAsOf != "2026-10-31" || len(f.pos) != 3 {
		t.Fatalf("first pass state=%+v positions=%+v", state, f.pos)
	}

	// Recover from a restart using only the persisted retry marker.
	restarted := fqPaperTrader(t, st, at.id, "crypto", &paperTestPrices{mark: 10})
	restarted.config.StrategyConfig.FactorQuantConfig.TargetsPath = cfg.TargetsPath
	f.failClose["FAILUSDT"] = false
	restarted.factorQuantTick(b, now.Add(5*time.Minute))
	fqCheckRetry(t, restarted, "2026-10-31", 1) // stop still fails, despite zero failed market orders
	if fqCountCalls(f, "open_long LONGUSDT") != 1 || fqCountCalls(f, "open_short SHORTUSDT") != 1 || fqCountCalls(f, "close_long CLOSEDUSDT") != 1 || fqCountCalls(f, "close_long FAILUSDT") != 2 {
		t.Fatalf("retry resent successful orders or missed failed close: %v", f.calls)
	}
	recs := fqRecords(t, restarted)
	if len(recs) != 2 || recs[0].Success || !strings.Contains(recs[0].CoTTrace, "重试第 1 次") || !strings.Contains(recs[0].CoTTrace, "补放止损（空） SHORTUSDT 失败") {
		t.Fatalf("failed protection retry record=%+v", recs)
	}

	f.failStop["SHORTUSDT"] = false
	restarted.factorQuantTick(b, now.Add(10*time.Minute))
	state = fqCheckRetry(t, restarted, "", 0)
	if state.LastAsOf != "2026-10-31" || fqCountCalls(f, "stop SHORTUSDT") != 4 {
		t.Fatalf("stop missing after open, rebalance or retry: state=%+v calls=%v", state, f.calls)
	}
	fqNear(t, f.stops["SHORTUSDT:SHORT"], 12, "repaired stop")
	recs = fqRecords(t, restarted)
	if len(recs) != 3 || !recs[0].Success || !strings.Contains(recs[0].CoTTrace, "重试第 2 次") {
		t.Fatalf("successful protection retry record=%+v", recs)
	}
	calls := len(f.calls)
	restarted.factorQuantTick(b, now.Add(15*time.Minute))
	if len(f.calls) != calls || len(fqRecords(t, restarted)) != 3 {
		t.Fatal("successful retry was not cleared")
	}
}

func TestFactorQuantRetryFailedCloseThenSuccess(t *testing.T) {
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-close-retry", "crypto", &paperTestPrices{mark: 10})
	fqSetOwned(t, at, "2026-10-28")
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{"NEWUSDT":0.5}`))
	f := newFakeFutures(1000, map[string]float64{"OLDUSDT": 10, "NEWUSDT": 10})
	f.pos["OLDUSDT:long"] = &fakePos{"OLDUSDT", "long", 10, 10}
	f.failClose["OLDUSDT"] = true
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	fqCheckRetry(t, at, "2026-10-31", 0)
	f.failClose["OLDUSDT"] = false
	at.factorQuantTick(b, now.Add(5*time.Minute))
	fqCheckRetry(t, at, "", 0)
	if fqCountCalls(f, "open_long NEWUSDT") != 1 || fqCountCalls(f, "close_long OLDUSDT") != 2 || len(f.pos) != 1 {
		t.Fatalf("retry plan calls=%v positions=%v", f.calls, f.pos)
	}
	recs := fqRecords(t, at)
	if len(recs) != 2 || !recs[0].Success || !strings.Contains(recs[0].CoTTrace, "调仓带内") || !strings.Contains(recs[0].CoTTrace, "重试第 1 次") {
		t.Fatalf("retry record=%+v", recs)
	}
}

func TestFactorQuantRetryCap(t *testing.T) {
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-retry-cap", "crypto", &paperTestPrices{mark: 10})
	fqSetOwned(t, at, "2026-10-28")
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{}`))
	f := newFakeFutures(1000, map[string]float64{"OLDUSDT": 10})
	f.pos["OLDUSDT:long"] = &fakePos{"OLDUSDT", "long", 10, 10}
	f.failClose["OLDUSDT"] = true
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	for i := 1; i <= fqMaxRetryAttempts; i++ {
		at.factorQuantTick(b, now.Add(time.Duration(i)*5*time.Minute))
		if i < fqMaxRetryAttempts {
			fqCheckRetry(t, at, "2026-10-31", i)
		}
	}
	fqCheckRetry(t, at, "", 0)
	for i := 7; i < 10; i++ {
		at.factorQuantTick(b, now.Add(time.Duration(i)*5*time.Minute))
	}
	if fqCountCalls(f, "close_long OLDUSDT") != 7 {
		t.Fatalf("expected initial + 6 retries: %v", f.calls)
	}
	recs := fqRecords(t, at)
	exhausted := 0
	for _, r := range recs {
		if strings.Contains(r.CoTTrace, "重试次数已耗尽") {
			exhausted++
		}
	}
	if len(recs) != 7 || exhausted != 1 {
		t.Fatalf("records=%d exhausted=%d", len(recs), exhausted)
	}
}

func TestFactorQuantRetryRequiresSameFreshOKTargets(t *testing.T) {
	for _, tc := range []struct {
		name, status, asOf, lastAsOf, now string
		horizon                           int
		cleared                           bool
	}{
		{"file moved on", "ok", "2026-11-01", "2026-10-31", "2026-11-01", 3, true},
		{"status not ok", "stale_scores", "2026-10-31", "2026-10-31", "2026-11-01", 3, false},
		{"stale", "ok", "2026-10-31", "2026-10-31", "2026-11-06", 3, false},
		{"last differs", "ok", "2026-10-31", "2026-11-01", "2026-11-01", 3, false},
		{"horizon differs", "ok", "2026-10-31", "2026-10-31", "2026-11-01", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := fqPaperTrader(t, fqTestStore(t), "fq-retry-gate", "crypto", &paperTestPrices{mark: 10})
			fqSetOwned(t, at, tc.lastAsOf)
			if err := at.store.FactorQuant().SetRetry(at.id, "2026-10-31", 2); err != nil {
				t.Fatal(err)
			}
			at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", tc.status, tc.asOf, tc.horizon, `{}`))
			f := newFakeFutures(1000, map[string]float64{"OLDUSDT": 10})
			f.pos["OLDUSDT:long"] = &fakePos{"OLDUSDT", "long", 10, 10}
			at.factorQuantTick(newFQFuturesBroker(f, 3, 0.2, 0.2), fqDay(tc.now))
			if len(f.calls) != 0 {
				t.Fatalf("unexpected orders: %v", f.calls)
			}
			if tc.cleared {
				fqCheckRetry(t, at, "", 0)
				if len(fqRecords(t, at)) != 0 {
					t.Fatal("file move must silently clear retry")
				}
			} else {
				fqCheckRetry(t, at, "2026-10-31", 2)
			}
		})
	}
}

func TestFactorQuantStaleFlatten(t *testing.T) {
	for _, source := range []string{"stale", "missing", "corrupt", "status"} {
		t.Run(source, func(t *testing.T) {
			at := fqPaperTrader(t, fqTestStore(t), "fq-flatten", "crypto", &paperTestPrices{mark: 10})
			fqSetOwned(t, at, "2026-10-23")
			if err := at.store.FactorQuant().SetRetry(at.id, "2026-10-23", 2); err != nil {
				t.Fatal(err)
			}
			cfg := at.config.StrategyConfig.FactorQuantConfig
			dir := t.TempDir()
			switch source {
			case "missing":
				cfg.TargetsPath = filepath.Join(dir, "missing.json")
			case "corrupt":
				cfg.TargetsPath = writeTargetsFile(t, dir, "{broken")
			case "status":
				cfg.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "stale_scores", "2026-10-23", 3, `{}`))
			default:
				cfg.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "ok", "2026-10-23", 3, `{"IGNOREDUSDT":0.5}`))
			}
			// Both sides of one symbol, including dust: every position must close.
			f := newFakeFutures(1000, map[string]float64{"BOTHUSDT": 10, "DUSTUSDT": 10})
			f.pos["BOTHUSDT:long"] = &fakePos{"BOTHUSDT", "long", 10, 10}
			f.pos["BOTHUSDT:short"] = &fakePos{"BOTHUSDT", "short", 5, 10}
			f.pos["DUSTUSDT:long"] = &fakePos{"DUSTUSDT", "long", 0.00001, 10}
			b := newFQFuturesBroker(f, 3, 0.2, 0.2)
			now := fqDay("2026-11-01")
			at.factorQuantTick(b, now) // age 9 > default 8
			state := fqCheckRetry(t, at, "", 0)
			if len(f.pos) != 0 || len(f.calls) != 3 || state.LastAsOf != "2026-10-23" {
				t.Fatalf("flatten positions=%+v calls=%v state=%+v", f.pos, f.calls, state)
			}
			recs := fqRecords(t, at)
			if len(recs) != 1 || !recs[0].Success || !strings.Contains(recs[0].CoTTrace, "目标已 9 天未更新（上次调仓 as_of=2026-10-23，阈值 8 天），自动清仓") || !strings.Contains(recs[0].CoTTrace, "owned-rule") {
				t.Fatalf("flatten record=%+v", recs)
			}
			for i := 1; i <= 3; i++ {
				at.factorQuantTick(b, now.Add(time.Duration(i)*5*time.Minute))
			}
			flattenRecords := 0
			for _, r := range fqRecords(t, at) {
				if strings.Contains(r.CoTTrace, "自动清仓") {
					flattenRecords++
				}
			}
			if flattenRecords != 1 || len(f.calls) != 3 {
				t.Fatal("repeated flatten after positions were closed")
			}

			// The unchanged last_as_of leaves the normal gate open for fresh targets.
			f.prices["NEWUSDT"] = 10
			cfg.TargetsPath = writeTargetsFile(t, dir, targetsJSON("crypto", "ok", "2026-10-31", 3, `{"NEWUSDT":0.5}`))
			at.factorQuantTick(b, now.Add(20*time.Minute))
			if state, _ := at.store.FactorQuant().Get(at.id); state.LastAsOf != "2026-10-31" || f.pos["NEWUSDT:long"] == nil || fqCountCalls(f, "open_long NEWUSDT") != 1 {
				t.Fatalf("fresh targets did not normally rebalance: state=%+v calls=%v", state, f.calls)
			}
		})
	}
}

func TestFactorQuantFlattenSafetyAndBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, lastAsOf string
		flattenDays    int
	}{
		{"disabled", "2026-10-01", -1},
		{"unmanaged", "", 1},
		{"threshold equality", "2026-10-24", 0}, // age 8 is still allowed
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := fqPaperTrader(t, fqTestStore(t), "fq-no-flatten", "crypto", &paperTestPrices{mark: 10})
			if tc.lastAsOf != "" {
				fqSetOwned(t, at, tc.lastAsOf)
			}
			cfg := at.config.StrategyConfig.FactorQuantConfig
			cfg.FlattenAfterDays = tc.flattenDays
			cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-01", 3, `{}`))
			f := newFakeFutures(1000, map[string]float64{"OLDUSDT": 10})
			f.pos["OLDUSDT:long"] = &fakePos{"OLDUSDT", "long", 10, 10}
			at.factorQuantTick(newFQFuturesBroker(f, 3, 0.2, 0.2), fqDay("2026-11-01"))
			if len(f.calls) != 0 || len(f.pos) != 1 {
				t.Fatalf("unsafe flatten calls=%v positions=%v", f.calls, f.pos)
			}
		})
	}
}

func TestFactorQuantFlattenFailureRetriesWithoutRecordSpam(t *testing.T) {
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-flatten-retry", "crypto", &paperTestPrices{mark: 10})
	fqSetOwned(t, at, "2026-10-23")
	path := writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-23", 3, `{}`))
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = path
	f := newFakeFutures(1000, map[string]float64{"FAILUSDT": 10, "CLOSEDUSDT": 10})
	f.pos["FAILUSDT:short"] = &fakePos{"FAILUSDT", "short", 10, 10}
	f.pos["CLOSEDUSDT:long"] = &fakePos{"CLOSEDUSDT", "long", 10, 10}
	f.failClose["FAILUSDT"] = true
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now)
	if recs := fqRecords(t, at); len(recs) != 1 || recs[0].Success {
		t.Fatalf("initial flatten failure records=%+v", recs)
	}
	// A restart must preserve the first-attempt dedupe, even if the file changes status.
	restarted := fqPaperTrader(t, st, at.id, "crypto", &paperTestPrices{mark: 10})
	restarted.config.StrategyConfig.FactorQuantConfig.TargetsPath = path
	writeTargetsFile(t, filepath.Dir(path), targetsJSON("crypto", "stale_scores", "2026-10-24", 3, `{}`))
	for i := 1; i <= 3; i++ {
		restarted.factorQuantTick(b, now.Add(time.Duration(i)*5*time.Minute))
	}
	if len(fqRecords(t, restarted)) != 1 || fqCountCalls(f, "close_short FAILUSDT") != 4 || fqCountCalls(f, "close_long CLOSEDUSDT") != 1 {
		t.Fatalf("flatten retries spammed records or resent filled closes: calls=%v", f.calls)
	}
	f.failClose["FAILUSDT"] = false
	restarted.factorQuantTick(b, now.Add(20*time.Minute))
	recs := fqRecords(t, restarted)
	if len(recs) != 2 || !recs[0].Success || len(f.pos) != 0 || !strings.Contains(recs[0].CoTTrace, "自动清仓") {
		t.Fatalf("missing successful flatten completion: recs=%+v positions=%v", recs, f.pos)
	}
	if state, _ := st.FactorQuant().Get(at.id); state.LastAsOf != "2026-10-23" {
		t.Fatalf("flatten advanced last_as_of: %+v", state)
	}
}

func TestFactorQuantPaperFlattenCancelsShortStop(t *testing.T) {
	at := fqPaperTrader(t, fqTestStore(t), "fq-flatten-paper", "crypto", &paperTestPrices{mark: 100})
	b, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-23", 3, `{"SHORTUSDT":-0.5}`))
	at.factorQuantTick(b, fqDay("2026-10-24"))
	if orders, err := at.trader.GetOpenOrders("SHORTUSDT"); err != nil || len(orders) != 1 {
		t.Fatalf("initial short stop=%+v %v", orders, err)
	}
	at.factorQuantTick(b, fqDay("2026-11-01"))
	if positions, err := at.trader.GetPositions(); err != nil || len(positions) != 0 {
		t.Fatalf("paper flatten positions=%+v %v", positions, err)
	}
	if orders, err := at.trader.GetOpenOrders("SHORTUSDT"); err != nil || len(orders) != 0 {
		t.Fatalf("paper flatten left stops=%+v %v", orders, err)
	}
}

// A transient rebalance preflight error must not suppress retry protection.
type fqPreflightFailureBroker struct {
	*fqFuturesBroker
	failAccount  bool
	accountCalls int
}

func (b *fqPreflightFailureBroker) Account() (*fqAccount, error) {
	b.accountCalls++
	// Fail the rebalance preflight after the portfolio-risk account read.
	if b.failAccount && b.accountCalls == 2 {
		b.failAccount = false
		return nil, errors.New("transient account failure")
	}
	return b.fqFuturesBroker.Account()
}

func TestFactorQuantRetryProtectsShortsAfterPreflightError(t *testing.T) {
	at := fqPaperTrader(t, fqTestStore(t), "fq-retry-preflight", "crypto", &paperTestPrices{mark: 10})
	fqSetOwned(t, at, "2026-10-31")
	if err := at.store.FactorQuant().SetRetry(at.id, "2026-10-31", 0); err != nil {
		t.Fatal(err)
	}
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{"SHORTUSDT":-0.5}`))
	f := newFakeFutures(1000, map[string]float64{"SHORTUSDT": 10})
	f.pos["SHORTUSDT:short"] = &fakePos{"SHORTUSDT", "short", 50, 10}
	b := &fqPreflightFailureBroker{fqFuturesBroker: newFQFuturesBroker(f, 3, 0.2, 0.2), failAccount: true}
	at.factorQuantTick(b, fqDay("2026-11-01"))
	fqCheckRetry(t, at, "2026-10-31", 1)
	fqNear(t, f.stops["SHORTUSDT:SHORT"], 12, "stop after preflight error")
	recs := fqRecords(t, at)
	if len(recs) != 1 || recs[0].Success || !strings.Contains(recs[0].CoTTrace, "transient account failure") || !strings.Contains(recs[0].CoTTrace, "补放止损（空） SHORTUSDT") {
		t.Fatalf("preflight retry record=%+v", recs)
	}
	at.factorQuantTick(b, fqDay("2026-11-01").Add(5*time.Minute))
	fqCheckRetry(t, at, "", 0)
	if fqCountCalls(f, "open_short") != 0 {
		t.Fatalf("protection retry reopened short: %v", f.calls)
	}
}

func TestFactorQuantFlattenOnTooSoonWithZeroEquity(t *testing.T) {
	at := fqPaperTrader(t, fqTestStore(t), "fq-flatten-too-soon", "crypto", &paperTestPrices{mark: 10})
	fqSetOwned(t, at, "2026-10-30")
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.FlattenAfterDays = 1
	cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{"NEWUSDT":0.5}`))
	f := newFakeFutures(0, map[string]float64{"OLDUSDT": 10})
	f.pos["OLDUSDT:long"] = &fakePos{"OLDUSDT", "long", 10, 10}
	at.factorQuantTick(newFQFuturesBroker(f, 3, 0.2, 0.2), fqDay("2026-11-01"))
	if len(f.pos) != 0 || fqCountCalls(f, "close_long OLDUSDT") != 1 {
		t.Fatalf("zero-equity position was not flattened: positions=%v calls=%v", f.pos, f.calls)
	}
	if recs := fqRecords(t, at); len(recs) != 1 || !recs[0].Success || !strings.Contains(recs[0].CoTTrace, "阈值 1 天") {
		t.Fatalf("configured threshold/too-soon flatten records=%+v", recs)
	}
}

func TestFactorQuantUSStaleFlattenUsesFileHorizon(t *testing.T) {
	fqStubUS(t, map[string]float64{"AAPLBUSDT": 100})
	at := fqPaperTrader(t, fqTestStore(t), "fq-us-flatten", "us", &paperTestPrices{mark: 100})
	b, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("us", "ok", "2026-10-19", 5, `{"AAPLBUSDT":1}`))
	at.factorQuantTick(b, fqDay("2026-10-20"))
	// h=5 + stale=7 => flatten after 12 days, strictly greater than the threshold.
	at.factorQuantTick(b, fqDay("2026-10-31"))
	if positions, err := at.store.StockPaper().ListOpen(at.id); err != nil || len(positions) != 1 {
		t.Fatalf("US flattened at threshold equality: %+v %v", positions, err)
	}
	at.factorQuantTick(b, fqDay("2026-11-01"))
	if positions, err := at.store.StockPaper().ListOpen(at.id); err != nil || len(positions) != 0 {
		t.Fatalf("US stale positions=%+v %v", positions, err)
	}
	recs := fqRecords(t, at)
	if !recs[0].Success || !strings.Contains(recs[0].CoTTrace, "阈值 12 天") || !strings.Contains(recs[0].CoTTrace, "close_long AAPLBUSDT") {
		t.Fatalf("US flatten record=%+v", recs[0])
	}
	if state, _ := at.store.FactorQuant().Get(at.id); state.LastAsOf != "2026-10-19" {
		t.Fatalf("US flatten advanced last_as_of: %+v", state)
	}
}

func TestFactorQuantRetryFailedFlipCloseKeepsFilledTargetSide(t *testing.T) {
	for _, oldSide := range []string{fqSideLong, fqSideShort} {
		t.Run(oldSide, func(t *testing.T) {
			at := fqPaperTrader(t, fqTestStore(t), "fq-retry-flip", "crypto", &paperTestPrices{mark: 10})
			fqSetOwned(t, at, "2026-10-28")
			weight, targetSide := "0.5", fqSideLong
			if oldSide == fqSideLong {
				weight, targetSide = "-0.5", fqSideShort
			}
			at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{"FLIPUSDT":`+weight+`}`))
			f := newFakeFutures(1000, map[string]float64{"FLIPUSDT": 10})
			f.pos["FLIPUSDT:"+oldSide] = &fakePos{"FLIPUSDT", oldSide, 10, 10}
			f.failClose["FLIPUSDT"] = true
			b := newFQFuturesBroker(f, 3, 0.2, 0.2)
			now := fqDay("2026-11-01")
			at.factorQuantTick(b, now)
			fqCheckRetry(t, at, "2026-10-31", 0)
			if len(f.pos) != 2 {
				t.Fatalf("expected both sides after failed flip close: %+v", f.pos)
			}
			f.failClose["FLIPUSDT"] = false
			at.factorQuantTick(b, now.Add(5*time.Minute))
			fqCheckRetry(t, at, "", 0)
			if len(f.pos) != 1 || f.pos["FLIPUSDT:"+targetSide] == nil || fqCountCalls(f, "close_"+oldSide+" FLIPUSDT") != 2 || fqCountCalls(f, "open_"+targetSide+" FLIPUSDT") != 1 {
				t.Fatalf("failed flip close not reconciled: positions=%+v calls=%v", f.pos, f.calls)
			}
		})
	}
}
