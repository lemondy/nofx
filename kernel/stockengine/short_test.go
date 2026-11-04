package stockengine

import (
	"math"
	"strings"
	"testing"
	"time"

	"nofx/market/usstock"
	"nofx/store"
)

func shortContext() *Context {
	c := testContext()
	c.Config.ShortMode = store.StockShortPaper
	c.Paper = true
	for _, s := range c.Snapshots {
		s.Shortable = true
		s.PerpQuote = &usstock.PerpQuote{Symbol: strings.TrimSuffix(s.Symbol, "BUSDT") + "USDT", Price: 100, Time: c.Now}
	}
	return c
}
func shortDecision() Decision {
	d := buy("AAPLBUSDT")
	d.Action = ActionOpenShort
	d.StopLoss = 104
	return d
}
func shortHolding(c *Context) {
	c.Positions = []Position{{Symbol: "AAPLBUSDT", Side: "SHORT", Quantity: 5, AvgPrice: 110, Price: 100, StopPrice: 106, InitialStop: 115}}
	c.Account.Exposure = 500
	c.Account.ShortExposure = 500
}

func TestShortValidationRules(t *testing.T) {
	cases := []struct {
		name, code string
		setup      func(*Context, *Decision)
	}{
		{"open", "", func(c *Context, d *Decision) {}},
		{"off", "SHORT_DISABLED", func(c *Context, d *Decision) { c.Config.ShortMode = "off" }},
		{"live config", "SHORT_DISABLED", func(c *Context, d *Decision) { v := false; c.Config.PaperTrading = &v }},
		{"unmapped", "NOT_SHORTABLE", func(c *Context, d *Decision) { c.Snapshots[0].Shortable = false }},
		{"opposite long held", "OPPOSITE_HELD", func(c *Context, d *Decision) { holding(c) }},
		{"opposite short held", "OPPOSITE_HELD", func(c *Context, d *Decision) { shortHolding(c); *d = buy(d.Symbol) }},
		{"already short", "ALREADY_HELD", func(c *Context, d *Decision) { shortHolding(c) }},
		{"add not held", "NOT_HELD", func(c *Context, d *Decision) { d.Action = ActionAddShort }},
		{"closed", "SESSION_CLOSED", func(c *Context, d *Decision) { c.Session = usstock.SessionClosed }},
		{"pre disabled", "SESSION_NOT_ALLOWED", func(c *Context, d *Decision) { c.Session = usstock.SessionPre }},
		{"low confidence", "LOW_CONFIDENCE", func(c *Context, d *Decision) { d.Confidence = 79 }},
		{"confidence boundary", "", func(c *Context, d *Decision) { d.Confidence = 80 }},
		{"missing daily", "DATA_INSUFFICIENT", func(c *Context, d *Decision) { c.Snapshots[0].MissingTF = []string{usstock.TF1d} }},
		{"stale daily", "DATA_STALE", func(c *Context, d *Decision) {
			c.Snapshots[0].DataThrough = map[string]string{usstock.TF1d: "2020-01-01"}
		}},
		{"reference stale", "BSTOCK_REF_STALE", func(c *Context, d *Decision) { c.Snapshots[0].Quote.RefFresh = false }},
		{"divergence", "BSTOCK_DIVERGENCE", func(c *Context, d *Decision) { c.Snapshots[0].Quote.DivergencePct = 2 }},
		{"perp missing", "PERP_PRICE_MISSING", func(c *Context, d *Decision) { c.Snapshots[0].PerpQuote = nil }},
		{"perp zero", "PERP_PRICE_MISSING", func(c *Context, d *Decision) { c.Snapshots[0].PerpQuote.Price = 0 }},
		// The LLM answers minutes after the snapshot; age is enforced at the entry re-quote.
		{"perp minutes old", "", func(c *Context, d *Decision) { c.Snapshots[0].PerpQuote.Time = c.Now.Add(-6 * time.Minute) }},
		{"bad entry type", "ENTRY_INVALID", func(c *Context, d *Decision) { d.EntryType = "other" }},
		{"low limit", "LIMIT_INVALID", func(c *Context, d *Decision) { d.EntryType = EntryLimit; d.LimitPrice = 98.99 }},
		{"limit boundary", "", func(c *Context, d *Decision) { d.EntryType = EntryLimit; d.LimitPrice = 99; d.StopLoss = 103 }},
		{"zero limit", "LIMIT_INVALID", func(c *Context, d *Decision) { d.EntryType = EntryLimit; d.LimitPrice = 0 }},
		{"zero stop", "STOP_INVALID", func(c *Context, d *Decision) { d.StopLoss = 0 }},
		{"stop below", "STOP_INVALID", func(c *Context, d *Decision) { d.StopLoss = 99 }},
		{"stop at entry", "STOP_INVALID", func(c *Context, d *Decision) { d.StopLoss = 100 }},
		{"stop nan", "STOP_INVALID", func(c *Context, d *Decision) { d.StopLoss = math.NaN() }},
		{"stop too near", "STOP_OUT_OF_BAND", func(c *Context, d *Decision) { d.StopLoss = 102 }},
		{"stop too far", "STOP_OUT_OF_BAND", func(c *Context, d *Decision) { d.StopLoss = 107 }},
		{"minimum band", "", func(c *Context, d *Decision) { d.StopLoss = 103 }},
		{"maximum band", "", func(c *Context, d *Decision) { d.StopLoss = 106 }},
		{"tp above", "TP_INVALID", func(c *Context, d *Decision) { d.TakeProfit = 101 }},
		{"tp negative", "TP_INVALID", func(c *Context, d *Decision) { d.TakeProfit = -1 }},
		{"tp valid", "", func(c *Context, d *Decision) { d.TakeProfit = 90 }},
		{"combined positions", "MAX_POSITIONS", func(c *Context, d *Decision) { holding(c); d.Symbol = "MSFTBUSDT"; c.Config.MaxPositions = 1 }},
		{"winning add", "", func(c *Context, d *Decision) { shortHolding(c); d.Action = ActionAddShort }},
		{"losing add", "ADD_WHILE_LOSING", func(c *Context, d *Decision) {
			shortHolding(c)
			d.Action = ActionAddShort
			c.Positions[0].AvgPrice = 99
		}},
		{"breakeven add", "ADD_WHILE_LOSING", func(c *Context, d *Decision) {
			shortHolding(c)
			d.Action = ActionAddShort
			c.Positions[0].AvgPrice = 100
		}},
		{"above initial stop add", "ADD_WHILE_LOSING", func(c *Context, d *Decision) {
			shortHolding(c)
			d.Action = ActionAddShort
			c.Positions[0].InitialStop = 99
		}},
		{"invalid reduce", "REDUCE_INVALID", func(c *Context, d *Decision) { shortHolding(c); d.Action = ActionReduceShort; d.ReduceFraction = 1 }},
		{"valid reduce", "", func(c *Context, d *Decision) { shortHolding(c); d.Action = ActionReduceShort; d.ReduceFraction = .5 }},
		{"adjust below", "STOP_INVALID", func(c *Context, d *Decision) { shortHolding(c); d.Action = ActionAdjustStop; d.StopLoss = 99 }},
		{"loosen", "STOP_LOOSEN", func(c *Context, d *Decision) { shortHolding(c); d.Action = ActionAdjustStop; d.StopLoss = 107 }},
		{"tighten", "", func(c *Context, d *Decision) { shortHolding(c); d.Action = ActionAdjustStop; d.StopLoss = 103 }},
		{"close closed", "", func(c *Context, d *Decision) {
			shortHolding(c)
			d.Action = ActionCloseShort
			c.Session = usstock.SessionClosed
			c.Snapshots = nil
		}},
		{"thesis", "THESIS_REQUIRED", func(c *Context, d *Decision) { d.Thesis = "" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := shortContext()
			d := shortDecision()
			tt.setup(c, &d)
			assertCode(t, Validate(c, []Decision{d})[0], tt.code)
		})
	}
}

func TestShortSizingMarginAndCycleCaps(t *testing.T) {
	c := shortContext()
	d := shortDecision()
	d.StopLoss = 110
	size, err := SizeOrder(c, d)
	if err != nil {
		t.Fatal(err)
	}
	near(t, size.Quantity, 10)
	near(t, size.RiskUSDT, 100)
	c.Account.Available = 100000 // short-sale proceeds are not new margin
	c.Account.Exposure = 9500
	c.Config.MaxTotalExposurePct = 100
	size, err = SizeOrder(c, d)
	if err != nil {
		t.Fatal(err)
	}
	near(t, size.Notional, 497.5)
	if size.LimitedBy != "available" {
		t.Fatal(size)
	}
	c = shortContext()
	c.Account.ShortExposure = 4900
	c.Account.Exposure = 4900
	size, err = SizeOrder(c, d)
	if err != nil {
		t.Fatal(err)
	}
	near(t, size.Notional, 100)
	if size.LimitedBy != "short_exposure_cap" {
		t.Fatal(size)
	}
	c = shortContext()
	c.Config.MaxShortExposurePct = .2
	a := shortDecision()
	b := a
	b.Symbol = "MSFTBUSDT"
	vs := Validate(c, []Decision{a, b})
	assertCode(t, vs[0], "")
	assertCode(t, vs[1], "NO_CAPACITY")
	c = shortContext()
	c.Config.MaxPositionPct = 100
	c.Config.MaxTotalExposurePct = 100
	c.Config.RiskPerTradePct = 5
	c.Config.MaxShortExposurePct = 1
	c.Account.Exposure = 9900
	size, err = SizeOrder(c, d)
	if err != nil {
		t.Fatal(err)
	}
	near(t, size.Notional, 99.5)
	c.Regime = RegimeScale{Enabled: true, Scale: .5}
	c.Account.Exposure = 0
	size, err = SizeOrder(c, d)
	if err != nil {
		t.Fatal(err)
	}
	near(t, size.RiskUSDT, 250)
}

func TestShortParseRepairAndPrompts(t *testing.T) {
	ds, err := ParseDecisions(shortDecisionContract)
	if err != nil || len(ds) != 2 || ds[0].Action != ActionOpenShort {
		t.Fatalf("%v %v", ds, err)
	}
	for _, action := range []string{ActionOpenShort, ActionAddShort, ActionReduceShort, ActionCloseShort} {
		if !validAction(action) {
			t.Fatal(action)
		}
	}
	c := shortContext()
	shortHolding(c)
	c.Snapshots[0].Cycle = &CycleAssessment{Phase: PhaseDowntrend, ShortSetup: true}
	c.Universe = []ScanResult{{Symbol: "AAPLBUSDT", Shortable: true, Cycle: *c.Snapshots[0].Cycle, Universe: UniverseRow{Price: 100}}}
	for _, lang := range []string{"en", "zh"} {
		sys := BuildSystemPrompt(c.Config, ResolvePreset(c.Config), lang)
		for _, token := range []string{"open_short", "add_short", "reduce_short", "close_short", "TRADIFI_PERPETUAL", "80", "1R"} {
			if !strings.Contains(sys, token) {
				t.Fatalf("%s missing %s", lang, token)
			}
		}
		user := BuildUserPrompt(c, lang)
		for _, token := range []string{"short_setup=true", "shortable", "side=SHORT", "unrealized=9.09%", "R=2.00R", "perp_price=100.0000"} {
			if !strings.Contains(user, token) {
				t.Fatalf("%s missing %s:\n%s", lang, token, user)
			}
		}
	}
}

func TestTopShortPriorityAndSort(t *testing.T) {
	r := &ScanReport{Results: []ScanResult{
		{Symbol: "A", Momentum: MomentumAssessment{Valid: true, BelowSMA200: true, Mom12_1Pct: -50}},
		{Symbol: "B", Cycle: CycleAssessment{ShortSetup: true}, Momentum: MomentumAssessment{Valid: true, Mom12_1Pct: -10}},
		{Symbol: "C", Cycle: CycleAssessment{ShortSetup: true}, Momentum: MomentumAssessment{Valid: true, Mom12_1Pct: -20}},
		{Symbol: "D", Momentum: MomentumAssessment{Valid: true, AboveSMA200: true, Mom12_1Pct: -100}},
		{Symbol: "E"},
	}}
	picks := r.TopShort(3, nil)
	if len(picks) != 3 || picks[0].Symbol != "C" || picks[1].Symbol != "B" || picks[2].Symbol != "A" {
		t.Fatal(picks)
	}
	picks = r.TopShort(1, map[string]bool{"C": true})
	if len(picks) != 1 || picks[0].Symbol != "B" {
		t.Fatal(picks)
	}
}

func TestShortSetupNeedsFallingEMAAndNegativeRS(t *testing.T) {
	bars := cycleTestBars(cycleTestPath(220, 180, -.25), nil)
	spy := cycleTestBars(cycleTestPath(220, 100, .1), nil)
	c := ClassifyCycle(bars, spy)
	if c.Phase != PhaseDowntrend || !c.ShortSetup || c.LongSetup || c.Score != 0 {
		t.Fatalf("%+v", c)
	}
	if c := ClassifyCycle(bars, nil); c.ShortSetup {
		t.Fatal("unknown RS cannot authorize short", c)
	}
	drop := cycleTestBars(append(cycleTestPath(80, 100, 0), 101, 101.5, 101.7, 101.6, 98), map[int]float64{84: 200})
	c = ClassifyCycle(drop, cycleTestBars(cycleTestPath(85, 100, .1), nil))
	if c.Phase != PhaseWedgeDrop || !c.ShortSetup {
		t.Fatalf("%+v", c)
	}
}

func TestShortEntryRepair(t *testing.T) {
	c := shortContext()
	for _, action := range []string{ActionOpenShort, ActionAddShort} {
		if action == ActionAddShort {
			shortHolding(c)
		}
		bad := `{"symbol":"AAPLBUSDT","action":"` + action + `","stop_loss":101,"confidence":85,"thesis":"downtrend","invalidation":"swing high"}`
		good := strings.Replace(bad, `"stop_loss":101`, `"stop_loss":104`, 1)
		client := &scriptedClient{responses: []string{obj(bad), obj(good)}}
		r, err := Decide(c, client, "en")
		if err != nil || len(r.Repaired) != 1 || !r.Verdicts[0].Accepted {
			t.Fatalf("%+v %v", r, err)
		}
	}
}

func TestShortMomentumFallbackStrictlyBelowSMA200(t *testing.T) {
	flat := ClassifyMomentum(momentumBars(253, func(int) float64 { return 100 }))
	down := ClassifyMomentum(momentumBars(253, func(i int) float64 { return 400 - float64(i) }))
	report := &ScanReport{Results: []ScanResult{{Symbol: "FLAT", Momentum: flat}, {Symbol: "DOWN", Momentum: down}}}
	picks := report.TopShort(3, nil)
	if len(picks) != 1 || picks[0].Symbol != "DOWN" {
		t.Fatal(picks)
	}
}
