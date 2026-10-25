package trader

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"nofx/kernel"
	"nofx/store"
	"nofx/trader/binance"
)

// 2026-10-21 area2 #1 residual: an UNKNOWN entry submit must keep its
// cycle stop-risk reservation (the order may be live) until the pending
// reconciliation resolves it, without being counted twice.

func unknownReserveTrader() *AutoTrader {
	at := riskTestTrader(store.RiskControlConfig{MaxAccountRiskPct: 5, MaxNetDirectionalRiskPct: -1, RiskPerTradePct: 2})
	at.cycleSymbolRiskReservedUSD = map[string]float64{}
	return at
}

func unknownSubmitErr() error {
	return fmt.Errorf("failed to open long position: %w", &binance.SubmitUnknownError{Symbol: "XUSDT", ClientOrderID: "x-abc123"})
}

// Simulates the loop's failure branch for one reserved open.
func failOpen(at *AutoTrader, d *kernel.Decision, err error) {
	if !at.holdUnknownSubmitReservation(err, d) {
		at.releaseCycleReservation(d)
	}
}

func bookReservation(at *AutoTrader, d *kernel.Decision, side string) {
	at.cycleRiskReservedUSD += d.CycleReservedRiskUSD
	if side == "short" {
		at.cycleShortRiskReservedUSD += d.CycleReservedRiskUSD
	} else {
		at.cycleLongRiskReservedUSD += d.CycleReservedRiskUSD
	}
	at.cycleSymbolRiskReservedUSD[d.Symbol] += d.CycleReservedRiskUSD
}

func TestReview1021UnknownSubmitKeepsReservationPlainFailureReleasesAll(t *testing.T) {
	plan := pendingEntry{Symbol: "XUSDT", Price: 100, Quantity: 1, StopLoss: 95, Leverage: 2}

	// UNKNOWN + tracked: every counter keeps the reservation.
	at := unknownReserveTrader()
	d := &kernel.Decision{Action: "open_long", Symbol: "XUSDT", CycleReservedRiskUSD: 30}
	bookReservation(at, d, "long")
	err := at.trackUnknownSubmit(unknownSubmitErr(), "long", plan)
	failOpen(at, d, err)
	if at.cycleRiskReservedUSD != 30 || at.cycleLongRiskReservedUSD != 30 || at.cycleSymbolRiskReservedUSD["XUSDT"] != 30 {
		t.Fatalf("UNKNOWN submit must keep the reservation: total=%v long=%v sym=%v",
			at.cycleRiskReservedUSD, at.cycleLongRiskReservedUSD, at.cycleSymbolRiskReservedUSD)
	}
	pe := at.getPendingEntry("XUSDT", "long")
	if pe == nil || pe.HeldRiskUSD != 30 || !pe.HeldInCycle {
		t.Fatalf("tracked pending must remember the held reservation: %+v", pe)
	}

	// Plain failure: released on total, per-side AND per-symbol counters.
	at = unknownReserveTrader()
	d = &kernel.Decision{Action: "open_short", Symbol: "YUSDT", CycleReservedRiskUSD: 30}
	bookReservation(at, d, "short")
	failOpen(at, d, errors.New("margin insufficient"))
	if at.cycleRiskReservedUSD != 0 || at.cycleShortRiskReservedUSD != 0 || at.cycleSymbolRiskReservedUSD["YUSDT"] != 0 {
		t.Fatalf("plain failure must release every counter: total=%v short=%v sym=%v",
			at.cycleRiskReservedUSD, at.cycleShortRiskReservedUSD, at.cycleSymbolRiskReservedUSD)
	}

	// A re-entry blocked by an OLD unknown entry is not itself UNKNOWN: released.
	at = unknownReserveTrader()
	_ = at.trackUnknownSubmit(unknownSubmitErr(), "long", plan)
	d = &kernel.Decision{Action: "open_long", Symbol: "XUSDT", CycleReservedRiskUSD: 30}
	bookReservation(at, d, "long")
	failOpen(at, d, at.unknownSubmitBlocks("XUSDT", "long"))
	if at.cycleRiskReservedUSD != 0 {
		t.Fatalf("blocked re-entry never submitted, must release: %v", at.cycleRiskReservedUSD)
	}
}

// Equity 1000, account cap 5% = 50 USDT. Held 30 + candidate 20 fits exactly
// ONCE; counting the pending entry on top of the reservation (60+20) would
// wrongly fill the budget.
func TestReview1021HeldReservationNotDoubleCounted(t *testing.T) {
	at := unknownReserveTrader()
	ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}}
	d := &kernel.Decision{Action: "open_long", Symbol: "XUSDT", CycleReservedRiskUSD: 30}
	bookReservation(at, d, "long")
	// computed pending risk = 10 x |100-97| = 30, same as the reservation.
	err := at.trackUnknownSubmit(unknownSubmitErr(), "long", pendingEntry{Symbol: "XUSDT", Price: 100, Quantity: 10, StopLoss: 97})
	failOpen(at, d, err)

	cand := &kernel.Decision{Action: "open_short", Symbol: "ZUSDT", PositionSizeUSD: 1000, StopLoss: 102} // 20U
	if blocked, reason, _ := at.accountRiskExposureBlocks(cand, 100, ctx); blocked {
		t.Fatalf("held reservation counted twice within the cycle: %s", reason)
	}
}

// Next cycle: counters reset, the pending entry keeps charging the retained
// reservation - even when its own price fields can't reproduce it (market entry
// whose fill price is unknown) - until the exchange answers.
func TestReview1021HeldReservationSurvivesCycleResetUntilResolved(t *testing.T) {
	at := unknownReserveTrader()
	ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}}
	d := &kernel.Decision{Action: "open_long", Symbol: "XUSDT", CycleReservedRiskUSD: 40}
	bookReservation(at, d, "long")
	// Price 0: computed pending risk is 0, only the held amount can carry it.
	err := at.trackUnknownSubmit(unknownSubmitErr(), "long", pendingEntry{Symbol: "XUSDT", Quantity: 10, StopLoss: 96})
	failOpen(at, d, err)
	at.settleHeldReservations() // end of batch
	pe := at.getPendingEntry("XUSDT", "long")
	if pe.HeldInCycle {
		t.Fatal("settle must hand the reservation over to the pending accounting")
	}
	// Next cycle: counters reset to zero.
	at.cycleRiskReservedUSD, at.cycleLongRiskReservedUSD, at.cycleSymbolRiskReservedUSD = 0, 0, map[string]float64{}

	cand := &kernel.Decision{Action: "open_short", Symbol: "ZUSDT", PositionSizeUSD: 1000, StopLoss: 102} // 20U; 40+20 > 50
	if blocked, _, _ := at.accountRiskExposureBlocks(cand, 100, ctx); !blocked {
		t.Fatal("UNKNOWN entry's retained 40 USDT must keep counting after the cycle counters reset")
	}

	// Never landed: tracking dropped -> reservation fully released.
	at.dropPendingEntry("XUSDT", "long")
	if blocked, reason, _ := at.accountRiskExposureBlocks(cand, 100, ctx); blocked {
		t.Fatalf("resolved-as-never-landed must free the budget: %s", reason)
	}
}

// Adopted by the exchange answer: the held floor is cleared (the real pending /
// position risk takes over, no stale extra).
func TestReview1021HeldReservationClearedWhenAdopted(t *testing.T) {
	m := &review03Mock{}
	at := review03Trader(m)
	d := &kernel.Decision{Action: "open_long", Symbol: "XUSDT", CycleReservedRiskUSD: 30}
	err := at.trackUnknownSubmit(unknownSubmitErr(), "long", pendingEntry{Symbol: "XUSDT", Price: 100, Quantity: 1, StopLoss: 95, Leverage: 2})
	if !at.holdUnknownSubmitReservation(err, d) {
		t.Fatal("expected retention")
	}
	m.status = map[string]interface{}{"orderId": int64(42), "status": "NEW", "executedQty": 0.0, "avgPrice": 0.0}
	pe := at.getPendingEntry("XUSDT", "long")
	if !at.resolveUnknownEntry(pe) || pe.OrderID != "42" {
		t.Fatalf("unknown entry not adopted: %+v", pe)
	}
	if pe.HeldRiskUSD != 0 {
		t.Fatalf("held floor must clear on adoption, got %v", pe.HeldRiskUSD)
	}
	at.settleHeldReservations()
	if got := pendingEntryStopRisk(pe); math.Abs(got-5) > 1e-9 {
		t.Fatalf("adopted pending reverts to computed risk 1x5, got %v", got)
	}
}
