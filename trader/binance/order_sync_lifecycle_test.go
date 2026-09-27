package binance

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestOrderSyncLoopStopsAndRejectsDuplicateStart(t *testing.T) {
	trader := &FuturesTrader{}
	var calls atomic.Int32
	called := make(chan struct{}, 16)
	run := func() {
		calls.Add(1)
		called <- struct{}{}
	}

	if !trader.startOrderSyncLoop(5*time.Millisecond, run) {
		t.Fatal("first order-sync start must succeed")
	}
	select {
	case <-called: // immediate first sync
	case <-time.After(time.Second):
		t.Fatal("initial order sync did not run")
	}
	if trader.startOrderSyncLoop(5*time.Millisecond, run) {
		t.Fatal("duplicate order-sync start must be ignored")
	}
	select {
	case <-called: // at least one ticker sync
	case <-time.After(time.Second):
		t.Fatal("periodic order sync did not run")
	}

	trader.StopOrderSync()
	stoppedAt := calls.Load()
	time.Sleep(25 * time.Millisecond)
	if got := calls.Load(); got != stoppedAt {
		t.Fatalf("order sync kept running after stop: calls %d -> %d", stoppedAt, got)
	}
	// Stop is deliberately idempotent because config reload and shutdown may
	// converge on the same trader instance.
	trader.StopOrderSync()
}

func TestStopOrderSyncWaitsForInFlightSync(t *testing.T) {
	trader := &FuturesTrader{}
	started := make(chan struct{})
	release := make(chan struct{})
	if !trader.startOrderSyncLoop(time.Hour, func() {
		close(started)
		<-release
	}) {
		t.Fatal("order-sync start failed")
	}
	<-started

	stopped := make(chan struct{})
	go func() {
		trader.StopOrderSync()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned before the in-flight sync exited")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop did not join the order-sync goroutine")
	}
}
