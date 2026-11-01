package trader

import (
	"testing"

	"nofx/kernel"
	"nofx/trader/types"
)

// A split TP short of the quantized target (here 2 resting vs target 3) is
// retired and re-armed whole instead of topped up with a 1-lot leg: adapters
// that replace a side's TP on placement (paper) would otherwise swap the
// resting leg for the top-up and loop every watchdog pass.
func TestReview20261029_WatchdogReArmsShortSplitTPWhole(t *testing.T) {
	at, m := exec1028Watchdog(t, kernel.ExitModeTrend, 5, []types.OpenOrder{exec1028SL(95), exec1028TP("tp0", 110, 2)})
	at.processProtectionWatchdog()
	if m.cancelTP != 1 {
		t.Fatalf("short TP not retired: cancels=%d", m.cancelTP)
	}
	if len(m.tpQtys) != 1 || m.tpQtys[0] != 3 {
		t.Fatalf("TP re-armed at %v, want one leg at the target 3", m.tpQtys)
	}
	at.processProtectionWatchdog()
	if m.cancelTP != 1 || len(m.tpQtys) != 1 {
		t.Fatalf("not idempotent: cancels=%d placements=%v", m.cancelTP, m.tpQtys)
	}
}

// With no resting TP at all there is nothing to retire: plain placement.
func TestReview20261029_WatchdogMissingSplitTPIsPlacedWithoutCancel(t *testing.T) {
	at, m := exec1028Watchdog(t, kernel.ExitModeTrend, 5, []types.OpenOrder{exec1028SL(95)})
	at.processProtectionWatchdog()
	if m.cancelTP != 0 || len(m.tpQtys) != 1 || m.tpQtys[0] != 3 {
		t.Fatalf("cancels=%d placements=%v, want 0 cancels and one leg of 3", m.cancelTP, m.tpQtys)
	}
}
