package trader

import (
	"errors"
	"math"
	"nofx/market"
	"nofx/store"
	"testing"
	"time"
)

func TestFastMoveWindow(t *testing.T) {
	now := time.Now()
	for _, side := range []string{"long", "short"} {
		st := &fastMoveState{}
		st.observe(now, 100, 5*time.Minute, side)
		a, f := st.observe(now.Add(time.Minute), 110, 5*time.Minute, side)
		if side == "long" && (a != 0 || math.Abs(f-10) > 1e-9) {
			t.Fatal(a, f)
		}
		if side == "short" && (math.Abs(a-10) > 1e-9 || f != 0) {
			t.Fatal(a, f)
		}
		a, f = st.observe(now.Add(2*time.Minute), 99, 5*time.Minute, side)
		if side == "long" && math.Abs(a-10) > 1e-9 {
			t.Fatal(a)
		}
		if side == "short" && math.Abs(f-10) > 1e-9 {
			t.Fatal(f)
		}
		a, f = st.observe(now.Add(8*time.Minute), 100, 5*time.Minute, side)
		if a != 0 || f != 0 {
			t.Fatal("expired extrema reused", a, f)
		}
	}
}
func TestFastAdverseTrimCooldownAndNoREST(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		t.Run(side, func(t *testing.T) {
			at, m, _ := newRealtimeTestTrader(store.RiskControlConfig{FastFavorableATRMult: -1, MinPositionSize: 1})
			qty, peak, retrace := 1.0, 110.0, 106.0
			if side == "short" {
				qty, peak, retrace = -1, 90, 94
				m.side = side
				m.qty = -1
			}
			pos := realtimePos(side, qty)
			at.SetRecordedStopLoss("XUSDT", side, 100)
			now := time.Now()
			at.processFastMove(pos, peak, 1, now)
			if !at.processFastMove(pos, retrace, 1, now.Add(time.Minute)) {
				t.Fatal("expected 50% trim")
			}
			at.processFastMove(pos, peak, 1, now.Add(15*time.Minute))
			if at.processFastMove(pos, retrace, 1, now.Add(16*time.Minute)) {
				t.Fatal("cooldown ignored")
			}
			at.processFastMove(pos, peak, 1, now.Add(31*time.Minute))
			if !at.processFastMove(pos, retrace, 1, now.Add(32*time.Minute)) {
				t.Fatal("cooldown did not expire")
			}
			if m.closeCalls+m.closeShorts != 2 || m.positionsCalls+m.priceCalls != 0 {
				t.Fatalf("close=%d/%d REST=%d/%d", m.closeCalls, m.closeShorts, m.positionsCalls, m.priceCalls)
			}
			at.processFastMove(pos, peak, 1, now.Add(63*time.Minute))
			if at.processFastMove(pos, retrace, 1, now.Add(64*time.Minute)) {
				t.Fatal("existing cumulative 75% guard bypassed")
			}
			at.ClearPeakPnLCache("XUSDT", side)
			if at.fastMoves["XUSDT_"+side] != nil {
				t.Fatal("new position inherited window/cooldown")
			}
		})
	}
}
func TestFastFavorableOnlyTightens(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		t.Run(side, func(t *testing.T) {
			at, m, _ := newRealtimeTestTrader(store.RiskControlConfig{FastAdverseATRMult: -1})
			pos := realtimePos(side, 1)
			old, mark, tighter := 90.0, 105.0, 103.0
			want := 100.2
			if side == "short" {
				old, mark, tighter, want = 110, 95, 97, 99.8
				m.side = side
			}
			at.SetRecordedStopLoss("XUSDT", side, old)
			now := time.Now()
			at.processFastMove(pos, 100, 1, now)
			at.processFastMove(pos, mark, 1, now.Add(time.Second))
			if got := at.GetRecordedStopLoss("XUSDT", side); math.Abs(got-want) > 1e-9 {
				t.Fatalf("breakeven fee buffer %g want %g", got, want)
			}
			at.SetRecordedStopLoss("XUSDT", side, tighter)
			if side == "long" {
				mark = 109
			} else {
				mark = 91
			}
			at.processFastMove(pos, mark, 1, now.Add(2*time.Second))
			if got := at.GetRecordedStopLoss("XUSDT", side); got != tighter || m.slCalls != 1 {
				t.Fatal("loosened existing lock", got, m.slCalls)
			}
			if m.positionsCalls+m.priceCalls != 0 {
				t.Fatal("fast path polled REST")
			}
		})
	}
}
func TestFastProtectionBreakevenFallbackFailureAndOff(t *testing.T) {
	at, m, _ := newRealtimeTestTrader(store.RiskControlConfig{FastFavorableATRMult: -1, MinPositionSize: 1})
	now := time.Now()
	pos := realtimePos("long", 1)
	at.SetRecordedStopLoss("XUSDT", "long", 90)
	at.processFastMove(pos, 101, 0, now)
	if at.processFastMove(pos, 97, 0, now.Add(time.Second)) || m.closeCalls+m.slCalls != 0 {
		t.Fatal("loss-side trim bypassed breakeven rule")
	}
	at.ClearPeakPnLCache("XUSDT", "long")
	at.processFastMove(pos, 110, 0, now)
	m.closeErr = errors.New("rejected")
	at.processFastMove(pos, 106, 0, now.Add(time.Second))
	if !at.fastMoves["XUSDT_long"].lastTrim.IsZero() {
		t.Fatal("failed trim consumed cooldown")
	}
	m.closeErr = nil
	if !at.processFastMove(pos, 106, 0, now.Add(2*time.Second)) {
		t.Fatal("fixed fallback/retry failed")
	}
	if len(m.actions) < 2 || m.actions[0] != "stop:100" {
		t.Fatal("trim before secured breakeven", m.actions)
	}
	at.config.StrategyConfig.RiskControl.FastWindowMinutes = -1
	at.processFastMove(pos, 120, 1, now.Add(3*time.Second))
	if at.processFastMove(pos, 105, 1, now.Add(4*time.Second)) {
		t.Fatal("disabled feature fired")
	}
}
func TestFastProtectionHandsOffManual(t *testing.T) {
	at, m, _ := newRealtimeTestTrader(store.RiskControlConfig{})
	at.store = profitLockTestStore(t)
	at.id = "manual-fast"
	pos := realtimePos("long", 1)
	now := time.Now()
	at.processFastMove(pos, 100, 1, now)
	at.processFastMove(pos, 110, 1, now.Add(time.Second))
	at.processFastMove(pos, 101, 1, now.Add(2*time.Second))
	if m.slCalls+m.closeCalls+m.positionsCalls+m.priceCalls != 0 || len(at.fastMoves) != 0 {
		t.Fatal("manual position touched")
	}
}

func TestFastCooldownSurvivesEntryAveraging(t *testing.T) {
	at, _, _ := newRealtimeTestTrader(store.RiskControlConfig{FastFavorableATRMult: -1, MinPositionSize: 1})
	pos := realtimePos("long", 1)
	now := time.Now()
	at.SetRecordedStopLoss("XUSDT", "long", 100)
	at.processFastMove(pos, 110, 1, now)
	at.processFastMove(pos, 106, 1, now.Add(time.Second))
	pos["entryPrice"] = 101.0
	at.processFastMove(pos, 112, 1, now.Add(2*time.Second))
	if at.processFastMove(pos, 106, 1, now.Add(3*time.Second)) {
		t.Fatal("averaging entry reset cooldown")
	}
}
func TestFastProtectionStreamIntegration(t *testing.T) {
	at, m, r := newRealtimeTestTrader(store.RiskControlConfig{FastFavorableATRMult: -1, ProfitLockAtR: -1, BreakevenArmR: -1, PeakDrawdownArmR: -1, MinPositionSize: 1})
	now := time.Now()
	marks := primeRealtimePosition(at, r, "long", now)
	at.SetRecordedStopLoss("XUSDT", "long", 100)
	marks.price = 110
	at.fastProfitPass(r, now)
	marks.price = 106
	at.fastProfitPass(r, now.Add(5*time.Second))
	if m.closeCalls != 1 || m.closedQty != .5 || m.positionsCalls+m.priceCalls != 0 {
		t.Fatalf("stream did not trim without REST: calls=%d qty=%g polls=%d", m.closeCalls, m.closedQty, m.positionsCalls+m.priceCalls)
	}
	if qty := r.positions["XUSDT_long"].pos["positionAmt"]; qty != .5 {
		t.Fatal("stream retained pre-trim quantity", qty)
	}
}
func TestFastATRCacheMissingStaleAndExpiry(t *testing.T) {
	at := riskTestTrader(store.RiskControlConfig{})
	at.cacheFastATR("XUSDT", &market.Data{CurrentPrice: 100})
	if at.cachedFastATR("XUSDT") != 0 {
		t.Fatal("missing 1h series must use fixed fallback")
	}
	at.cacheFastATR("XUSDT", freshnessData("XUSDT", 100, allFreshLags))
	if at.cachedFastATR("XUSDT") <= 0 {
		t.Fatal("fresh hourly ATR missing")
	}
	at.cacheFastATR("XUSDT", freshnessData("XUSDT", 100, stale1hLags))
	if at.cachedFastATR("XUSDT") != 0 {
		t.Fatal("stale hourly ATR reused")
	}
	at.cacheFastATR("XUSDT", freshnessData("XUSDT", 100, allFreshLags))
	v := at.fastATR["XUSDT"]
	v.at = time.Now().Add(-11 * time.Minute)
	at.fastATR["XUSDT"] = v
	if at.cachedFastATR("XUSDT") != 0 {
		t.Fatal("expired ATR reused")
	}
}

func TestFastProtectionDoesNotStackWithFreshRTrim(t *testing.T) {
	at, m, r := newRealtimeTestTrader(store.RiskControlConfig{FastFavorableATRMult: -1, ProfitLockAtR: 1, BreakevenArmR: -1, PeakDrawdownArmR: -1, MinPositionSize: 1})
	now := time.Now()
	marks := primeRealtimePosition(at, r, "long", now)
	pos := r.positions["XUSDT_long"].pos
	at.processFastMove(pos, 115, 0, now)
	marks.price = 111
	at.fastProfitPass(r, now.Add(time.Second))
	if m.closeCalls != 1 || r.positions["XUSDT_long"].pos["positionAmt"] != .5 {
		t.Fatalf("stacked fast/R trims: closes=%d position=%v", m.closeCalls, r.positions["XUSDT_long"].pos)
	}
	at.fastProfitPass(r, now.Add(2*time.Second))
	if m.closeCalls != 2 || m.closedQty != .25 {
		t.Fatalf("next pass should reduce half the remainder: closes=%d qty=%g", m.closeCalls, m.closedQty)
	}
	at.processFastMove(pos, 120, 0, now.Add(31*time.Minute))
	if at.processFastMove(pos, 110, 0, now.Add(32*time.Minute)) {
		t.Fatal("R+fast reductions exceeded cumulative 75%")
	}
}
