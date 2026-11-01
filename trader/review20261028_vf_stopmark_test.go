package trader

import "testing"

type vfMarkObserver struct {
	Trader
	symbol string
	mark   float64
}

func (o *vfMarkObserver) ObserveStopMarkPrice(symbol string, mark float64) {
	o.symbol, o.mark = symbol, mark
}

// The MARK price a stop decision used is handed to adapters that can pre-check
// the trigger (review 2026-10-28 #2); adapters without the capability are fine.
func TestReview20261028VFNoteStopMark(t *testing.T) {
	obs := &vfMarkObserver{}
	(&AutoTrader{trader: obs}).noteStopMark("XUSDT", 100.2)
	if obs.symbol != "XUSDT" || obs.mark != 100.2 {
		t.Fatalf("mark not forwarded: %+v", obs)
	}
	(&AutoTrader{trader: nil}).noteStopMark("XUSDT", 1) // must not panic
}
