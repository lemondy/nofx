package manager

import (
	"errors"
	"strings"
	"testing"

	"nofx/trader"
)

// startRecorder replaces the real AutoTrader start step so tests never run a
// trading loop; it records exactly which instance pointer was started.
// review 2026-10-07 B1-5
type startRecorder struct {
	started []*trader.AutoTrader
	err     error
}

func (r *startRecorder) start(t *trader.AutoTrader) error {
	r.started = append(r.started, t)
	return r.err
}

// setMapInstance registers t under id the same way addTraderFromStore does
// (map write under tm.mu; callers already hold loadMu in production).
func setMapInstance(t *testing.T, tm *TraderManager, id string, at *trader.AutoTrader) {
	t.Helper()
	tm.mu.Lock()
	tm.traders[id] = at
	tm.mu.Unlock()
}

// review 2026-10-07 B1-5 (P0-1): StartTrader must start the instance
// currently registered in the map and report its name.
func TestStartTraderStartsCurrentInstance(t *testing.T) {
	tm := NewTraderManager()
	rec := &startRecorder{}
	tm.startFn = rec.start

	a := &trader.AutoTrader{}
	setMapInstance(t, tm, "t1", a)

	name, err := tm.StartTrader("t1")
	if err != nil {
		t.Fatalf("StartTrader returned error: %v", err)
	}
	if name != a.GetName() {
		t.Fatalf("StartTrader name = %q, want instance name %q", name, a.GetName())
	}
	if len(rec.started) != 1 || rec.started[0] != a {
		t.Fatalf("started = %v, want exactly the map instance %p", rec.started, a)
	}

	// Unknown id must fail without starting anything.
	if _, err := tm.StartTrader("missing"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("StartTrader(missing) err = %v, want does-not-exist error", err)
	}
	if len(rec.started) != 1 {
		t.Fatalf("unknown id must not start anything, started = %v", rec.started)
	}
}

// review 2026-10-07 B1-5 (P0-1): the update path reloads a trader in the
// background — RemoveTraderAndThen evicts the old instance and the loader
// registers a replacement (both under loadMu). StartTrader must start the
// replacement currently in the map and never the evicted pointer, which is
// what used to leave an orphan loop trading outside the manager.
func TestStartTraderAfterReloadSwapStartsOnlyCurrentInstance(t *testing.T) {
	tm := NewTraderManager()
	rec := &startRecorder{}
	tm.startFn = rec.start

	old := &trader.AutoTrader{}
	setMapInstance(t, tm, "t1", old)

	// Simulate the reload chain: evict (and stop) the old instance, then
	// register the freshly built replacement.
	tm.RemoveTraderAndThen("t1", nil)
	fresh := &trader.AutoTrader{}
	setMapInstance(t, tm, "t1", fresh)

	name, err := tm.StartTrader("t1")
	if err != nil {
		t.Fatalf("StartTrader returned error: %v", err)
	}
	if name != fresh.GetName() {
		t.Fatalf("StartTrader name = %q, want reloaded instance name %q", name, fresh.GetName())
	}
	if len(rec.started) != 1 || rec.started[0] != fresh {
		t.Fatalf("started = %v, want exactly the reloaded instance %p", rec.started, fresh)
	}
	for _, st := range rec.started {
		if st == old {
			t.Fatal("evicted instance must never be started")
		}
	}
}

// review 2026-10-07 B1-5: ErrAlreadyRunning keeps its existing semantics —
// StartTrader surfaces it unchanged and callers keep their tolerance.
func TestStartTraderPassesThroughAlreadyRunning(t *testing.T) {
	tm := NewTraderManager()
	rec := &startRecorder{err: trader.ErrAlreadyRunning}
	tm.startFn = rec.start

	a := &trader.AutoTrader{}
	setMapInstance(t, tm, "t1", a)

	name, err := tm.StartTrader("t1")
	if !errors.Is(err, trader.ErrAlreadyRunning) {
		t.Fatalf("StartTrader err = %v, want ErrAlreadyRunning", err)
	}
	if name != a.GetName() {
		t.Fatalf("StartTrader name = %q, want instance name %q", name, a.GetName())
	}
	if len(rec.started) != 1 || rec.started[0] != a {
		t.Fatalf("started = %v, want exactly the map instance %p", rec.started, a)
	}
}
