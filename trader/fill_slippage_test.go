package trader

import (
	"testing"

	"nofx/kernel"
	"nofx/store"
)

// 09-28 review P3: the old reportFillSlippageRR computed realized RR AFTER
// the distance-preserving reanchor — which equals the planned RR by
// construction, so the alert was structurally dead. The rewrite reports
// fill-vs-checked slippage and alerts past fillSlippageAlertBps.
func TestReportFillSlippage(t *testing.T) {
	at := riskTestTrader(store.RiskControlConfig{})
	d := &kernel.Decision{Symbol: "TESTUSDT", Action: "open_long"}

	// Routine fill: 30bps — logged at most, never alerted.
	if bps, alerted := at.reportFillSlippage(d, 100, 100.3); alerted || bps < 29 || bps > 31 {
		t.Fatalf("30bps fill: alerted=%v bps=%.1f, want false/~30", alerted, bps)
	}

	// Violent fill: 1.5% = 150bps — crosses the alert line.
	if bps, alerted := at.reportFillSlippage(d, 100, 101.5); !alerted || bps < 149 || bps > 151 {
		t.Fatalf("150bps fill: alerted=%v bps=%.1f, want true/~150", alerted, bps)
	}

	// Slippage in the FAVORABLE direction still counts (absolute) — a fill
	// 1.2% better than checked is the same entry-quality information.
	if _, alerted := at.reportFillSlippage(d, 100, 98.8); !alerted {
		t.Fatal("favorable 120bps fill should still alert (absolute distance)")
	}

	// Degenerate inputs.
	if bps, alerted := at.reportFillSlippage(d, 0, 100); bps != 0 || alerted {
		t.Fatal("zero checked price must be a no-op")
	}
	if bps, alerted := at.reportFillSlippage(d, 100, 100); bps != 0 || alerted {
		t.Fatal("exact fill must be a no-op")
	}
}
