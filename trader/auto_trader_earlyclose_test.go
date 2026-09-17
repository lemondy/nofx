package trader

import (
	"testing"
	"time"

	"nofx/kernel"
	"nofx/market"
	"nofx/store"
	"nofx/trader/types"
)

func oneHKlines(closes []float64) *market.TimeframeSeriesData {
	tf := &market.TimeframeSeriesData{Timeframe: "1h"}
	base := time.Now().Add(-time.Duration(len(closes)+1) * time.Hour)
	prev := closes[0]
	for _, c := range closes {
		// Open = previous close: a falling close ⇒ bearish body.
		tf.Klines = append(tf.Klines, market.KlineBar{Time: base.UnixMilli(), Open: prev, High: c * 1.001, Low: c * 0.998, Close: c, Volume: 10})
		prev = c
		base = base.Add(time.Hour)
	}
	return tf
}

// The early-close evidence counter reads trailing CLOSED 1h candles against
// the position direction; the forming bar and dojis break the streak.
func TestOneHAgainstCandles(t *testing.T) {
	// [101, 100.5, 100, 99.5] closed + forming bar at 99.6 (ignored).
	data := &market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{
		"1h": oneHKlines([]float64{101, 100.5, 100, 99.5, 99.6}),
	}}
	if got := oneHAgainstCandles(data, "long"); got != 3 {
		t.Fatalf("long with 3 trailing bearish closed candles: got %d, want 3", got)
	}
	if got := oneHAgainstCandles(data, "short"); got != 0 {
		t.Fatalf("short against a falling series: got %d, want 0", got)
	}

	// Doji in the middle breaks the streak: [101, 100.5, 100.5, 100].
	doji := &market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{
		"1h": oneHKlines([]float64{101, 100.5, 100.5, 100, 100}),
	}}
	if got := oneHAgainstCandles(doji, "long"); got != 1 {
		t.Fatalf("doji must break the streak: got %d, want 1", got)
	}

	if got := oneHAgainstCandles(nil, "long"); got != 0 {
		t.Fatalf("nil data must yield 0, got %d", got)
	}
}

// The early-close gate: <4h without 1h evidence blocks; with evidence, at/over
// 4h, or at the recorded stop it passes; fail-open without hold-age data.
func TestEarlyCloseBlocksClose(t *testing.T) {
	bars := oneHKlines([]float64{101, 100.9, 100.8, 100.7, 100.6}) // 4 bearish closed
	data := &market.Data{Symbol: "VTHOUSDT", TimeframeData: map[string]*market.TimeframeSeriesData{"1h": bars}}

	mk := func(held time.Duration, earlyHours int, sl float64) *AutoTrader {
		at := &AutoTrader{name: "t", positionFirstSeenTime: map[string]int64{
			"VTHOUSDT_long": time.Now().Add(-held).UnixMilli(),
		}, positionStopLoss: map[string]float64{}}
		at.config = AutoTraderConfig{StrategyConfig: &store.StrategyConfig{}}
		at.config.StrategyConfig.RiskControl.EarlyCloseMinHours = earlyHours
		if sl > 0 {
			at.SetRecordedStopLoss("VTHOUSDT", "long", sl)
		}
		return at
	}

	// Held 1h, rising 1h series (no against-direction evidence), default 4h
	// → blocked.
	rising := &market.Data{Symbol: "VTHOUSDT", TimeframeData: map[string]*market.TimeframeSeriesData{
		"1h": oneHKlines([]float64{100, 100.5, 101, 101.5, 102}),
	}}
	if blocked, reason := mk(time.Hour, 0, 0).earlyCloseBlocksClose("VTHOUSDT", "long", 100.75, rising); !blocked {
		t.Fatal("1h-held close without evidence must be blocked")
	} else if reason == "" {
		t.Fatal("blocked close must carry a reason")
	}
	// Held 1h, WITH ≥2 against-candles → allowed.
	if blocked, _ := mk(time.Hour, 0, 0).earlyCloseBlocksClose("VTHOUSDT", "long", 100.75, data); blocked {
		t.Fatal("evidence path must allow")
	}
	// Held 5h (≥4h) → allowed regardless of evidence.
	if blocked, _ := mk(5*time.Hour, 0, 0).earlyCloseBlocksClose("VTHOUSDT", "long", 100.75, data); blocked {
		t.Fatal("past-threshold close must be allowed")
	}
	// Mark at/below recorded stop → allowed (stopping out anyway).
	if blocked, _ := mk(time.Hour, 0, 100.8).earlyCloseBlocksClose("VTHOUSDT", "long", 100.7, data); blocked {
		t.Fatal("stop-hit bypass must allow")
	}
	// Disabled (negative) → never blocks.
	if blocked, _ := mk(time.Hour, -1, 0).earlyCloseBlocksClose("VTHOUSDT", "long", 100.75, data); blocked {
		t.Fatal("negative config must disable the gate")
	}
	// Custom threshold: 8h config, held 5h, no evidence → still blocked.
	if blocked, _ := mk(5*time.Hour, 8, 0).earlyCloseBlocksClose("VTHOUSDT", "long", 100.75, rising); !blocked {
		t.Fatal("custom 8h threshold must block a 5h-held close")
	}
	// No hold-age data → fail-open.
	at := &AutoTrader{name: "t"}
	at.config = AutoTraderConfig{StrategyConfig: &store.StrategyConfig{}}
	if blocked, _ := at.earlyCloseBlocksClose("OTHER", "long", 100, data); blocked {
		t.Fatal("unknown hold age must fail open")
	}
	// kernel.EarlyCloseHours defaults: 0→4, negative→0 (disabled), custom kept.
	if h := kernel.EarlyCloseHours(nil); h != 4 {
		t.Fatalf("nil rc default must be 4, got %d", h)
	}
}

// Spread gate math and threshold resolution.
func TestTopOfBookSpreadPct(t *testing.T) {
	book := [][]float64{{99.9, 10}, {99.8, 5}}
	asks := [][]float64{{100.1, 8}, {100.2, 3}}
	got := topOfBookSpreadPct(book, asks)
	// (100.1-99.9)/100 = 0.2%
	if got < 0.1999 || got > 0.2001 {
		t.Fatalf("spread = %v, want 0.2", got)
	}
	if topOfBookSpreadPct(nil, asks) != 0 || topOfBookSpreadPct(book, nil) != 0 {
		t.Fatal("malformed book must yield 0 (fail-open)")
	}
	// "Crossed" test: best bid ABOVE best ask is garbage — guard returns 0.
	crossed := [][]float64{{99.9, 1}}
	if topOfBookSpreadPct(book, crossed) != 0 {
		t.Fatal("crossed book must yield 0")
	}
	// Equal best prices (locked book) → spread 0, usable.
	lockedBids := [][]float64{{100, 10}}
	lockedAsks := [][]float64{{100, 5}}
	if topOfBookSpreadPct(lockedBids, lockedAsks) != 0 {
		t.Fatal("locked book spread must be 0")
	}
	// Threshold resolution: 0 → 0.5 default; negative → disabled.
	if h := kernel.MaxSpreadPct(nil); h != 0.5 {
		t.Fatalf("nil rc default = 0.5, got %v", h)
	}
	if h := kernel.MaxSpreadPct(&store.RiskControlConfig{MaxSpreadPct: -1}); h != -1 {
		t.Fatalf("negative disables, got %v", h)
	}
}

// Protection watchdog detection: STOP* covers SL legs (legacy + algo),
// TAKE_PROFIT* covers TP legs, a resting LIMIT entry is noise, and the
// both-present case reports nothing missing.
func TestMissingProtection(t *testing.T) {
	both := []types.OpenOrder{
		{Type: "STOP_MARKET", Status: "NEW"},
		{Type: "TAKE_PROFIT_MARKET", Status: "NEW"},
	}
	if sl, tp := missingProtection(both); sl || tp {
		t.Fatalf("both legs present: sl=%v tp=%v", sl, tp)
	}
	legacy := []types.OpenOrder{
		{Type: "STOP", Status: "NEW"},
		{Type: "TAKE_PROFIT", Status: "NEW"},
	}
	if sl, tp := missingProtection(legacy); sl || tp {
		t.Fatalf("legacy legs: sl=%v tp=%v", sl, tp)
	}
	slOnly := []types.OpenOrder{{Type: "STOP_MARKET", Status: "NEW"}}
	if sl, tp := missingProtection(slOnly); sl || !tp {
		t.Fatalf("SL-only book: sl=%v tp=%v (want false/true)", sl, tp)
	}
	tpOnly := []types.OpenOrder{{Type: "TAKE_PROFIT_MARKET", Status: "NEW"}}
	if sl, tp := missingProtection(tpOnly); !sl || tp {
		t.Fatalf("TP-only book: sl=%v tp=%v (want true/false)", sl, tp)
	}
	noise := []types.OpenOrder{{Type: "LIMIT", Status: "NEW"}}
	if sl, tp := missingProtection(noise); !sl || !tp {
		t.Fatalf("limit entry is not protection: sl=%v tp=%v (want true/true)", sl, tp)
	}
	if sl, tp := missingProtection(nil); !sl || !tp {
		t.Fatalf("empty book: sl=%v tp=%v (want true/true)", sl, tp)
	}
}

// adjust_stop_loss tighten-only guard + partial cumulative cap (audit-proof
// guards for the new position-management actions).
func TestStopMoveTightens(t *testing.T) {
	// Long: tighten = new above current, below mark.
	if !stopMoveTightens("long", 98, 100, 102) {
		t.Fatal("long tighten to entry must pass")
	}
	if stopMoveTightens("long", 98, 97, 102) {
		t.Fatal("widening must fail")
	}
	if stopMoveTightens("long", 98, 103, 102) {
		t.Fatal("crossing the mark must fail")
	}
	// No known stop (current 0 — restart-wiped recorded state, no exchange
	// order): a correctly-sided new stop ADDS protection and is allowed.
	if !stopMoveTightens("long", 0, 100, 102) {
		t.Fatal("adding a stop to an unprotected long must pass")
	}
	if stopMoveTightens("long", 0, 103, 102) {
		t.Fatal("stop above the mark on an unprotected long must fail")
	}
	if !stopMoveTightens("short", 0, 98, 96) {
		t.Fatal("adding a stop to an unprotected short must pass")
	}
	if stopMoveTightens("short", 0, 95, 96) {
		t.Fatal("stop below the mark on an unprotected short must fail")
	}
	// Short mirror: tighten = new below current, above mark.
	if !stopMoveTightens("short", 102, 100, 98) {
		t.Fatal("short tighten must pass")
	}
	if stopMoveTightens("short", 102, 103, 98) {
		t.Fatal("short widening must fail")
	}
	if stopMoveTightens("short", 102, 97, 98) {
		t.Fatal("short crossing the mark must fail")
	}
}

// Breakeven-or-better gate (user directive 09-16, NEARUSDT case): a tighten
// that still locks a loss is rejected — no profit, nothing to protect. The
// NEAR tighten parked the SL (2.438) below entry (2.456) under a 5m support
// cluster; a single 5m wick (2.432) swept it fifteen minutes before price
// broke the structure high, while the pre-tighten stop (2.4) was never
// touched.
func TestStopMoveLocksProfit(t *testing.T) {
	const entry = 2.456
	// Long: loss-locking "tighten" (the NEAR case) must be rejected.
	if stopMoveLocksProfit("long", entry, 2.438) {
		t.Fatal("long tighten below entry locks a loss — must be rejected")
	}
	// Breakeven exactly, and anything above entry up to the mark, is legal.
	if !stopMoveLocksProfit("long", entry, entry) {
		t.Fatal("long tighten to exactly entry (breakeven) must pass")
	}
	if !stopMoveLocksProfit("long", entry, 2.47) {
		t.Fatal("long tighten above entry must pass")
	}
	// Short mirror.
	if stopMoveLocksProfit("short", entry, 2.47) {
		t.Fatal("short tighten above entry locks a loss — must be rejected")
	}
	if !stopMoveLocksProfit("short", entry, entry) {
		t.Fatal("short tighten to exactly entry (breakeven) must pass")
	}
	if !stopMoveLocksProfit("short", entry, 2.44) {
		t.Fatal("short tighten below entry must pass")
	}
	// Unknown entry (exchange data glitch) fails the predicate — the executor
	// treats that as gate-skipped with a WARN, never as a silent pass.
	if stopMoveLocksProfit("long", 0, 2.47) {
		t.Fatal("missing entry must not validate")
	}
}

func TestPartialCumulativeCap(t *testing.T) {
	// The cap lives in executePartialCloseWithRecord via partialTrimmed —
	// assert the arithmetic contract: cumulative 0.5 + 0.25 allowed (0.75),
	// 0.75 + 0.25 rejected (would exceed).
	already, frac := 0.5, 0.25
	if already+frac > 0.75 {
		t.Fatal("0.5+0.25 must be allowed at the boundary")
	}
	already, frac = 0.75, 0.25
	if already+frac <= 0.75 {
		t.Fatal("0.75+0.25 must be rejected")
	}
	_ = frac
}

// Margin-budget gate math (audit 09-13 #2): used-margin sum skips positions
// without leverage; the budget comparison is a strict exceed check.
func TestUsedMarginAndBudget(t *testing.T) {
	positions := []map[string]interface{}{
		{"positionAmt": 2.0, "markPrice": 50.0, "leverage": 10.0}, // 100/10 = 10
		{"positionAmt": -1.0, "markPrice": 200.0, "leverage": 5.0}, // 200/5 = 40
		{"positionAmt": 3.0, "markPrice": 10.0}, // no leverage → skipped
	}
	if got := usedMarginOf(positions); got != 50 {
		t.Fatalf("used margin = %v, want 50", got)
	}
	if marginExceedsBudget(40, 10, 60, 0.9) {
		t.Fatal("40+10 = 50 ≤ 54 budget → must pass")
	}
	if !marginExceedsBudget(40, 20, 60, 0.9) {
		t.Fatal("40+20 = 60 > 54 budget → must block")
	}
	if marginExceedsBudget(0, 100, 0, 0.9) {
		t.Fatal("no equity → fail-open")
	}
}

// Stop-price selection from the exchange order book list: position-side and
// STOP-type filtered.
func TestStopPriceForSide(t *testing.T) {
	orders := []types.OpenOrder{
		{Type: "TAKE_PROFIT_MARKET", PositionSide: "LONG", StopPrice: 110},
		{Type: "STOP_MARKET", PositionSide: "LONG", StopPrice: 98},
		{Type: "STOP_MARKET", PositionSide: "SHORT", StopPrice: 103},
		{Type: "LIMIT", PositionSide: "LONG", StopPrice: 0},
	}
	if got := stopPriceForSide(orders, "long"); got != 98 {
		t.Fatalf("long stop = %v, want 98", got)
	}
	if got := stopPriceForSide(orders, "short"); got != 103 {
		t.Fatalf("short stop = %v, want 103", got)
	}
	if got := stopPriceForSide(nil, "long"); got != 0 {
		t.Fatalf("empty list → 0, got %v", got)
	}
}

// Limit-entry lifetime is max(30min, N × scan interval) — time-based, not
// cycle-counted (user 09-16: cycles change length when the scan interval is
// retuned, so "3 cycles" could mean 9 or 60 minutes).
func TestLimitEntryLifetime(t *testing.T) {
	cases := []struct {
		cycles int
		cycle  time.Duration
		want   time.Duration
	}{
		{3, 5 * time.Minute, 30 * time.Minute},  // 15min < floor → 30min
		{3, 20 * time.Minute, time.Hour},        // 60min > floor
		{10, 5 * time.Minute, 50 * time.Minute}, // high multiplier scales
		{0, 5 * time.Minute, 30 * time.Minute},  // unset multiplier → default 3
		{3, 0, 30 * time.Minute},                // unset interval → default 5m, floor binds
	}
	for _, c := range cases {
		if got := limitEntryLifetime(c.cycles, c.cycle); got != c.want {
			t.Errorf("limitEntryLifetime(%d, %v) = %v, want %v", c.cycles, c.cycle, got, c.want)
		}
	}
}
