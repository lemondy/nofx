package types

import (
	"math"
	"sync/atomic"
	"testing"
	"time"
)

func TestFloorQuantityNeverRoundsUp(t *testing.T) {
	for _, tc := range []struct{ q, step, min, max, want float64 }{
		{0.3, 0.1, 0, 0, 0.3}, {1.99, 1, 0, 0, 1}, {1.234, 0.01, 0, 0, 1.23},
		{100, 0.1, 0, 2.05, 2}, {0.000000019, 0.00000001, 0, 0, 0.00000001},
	} {
		got, err := FloorQuantity(tc.q, tc.step, tc.min, tc.max)
		if err != nil || got != tc.want || got > tc.q {
			t.Fatalf("%+v: got %v, %v", tc, got, err)
		}
	}
	for _, tc := range [][4]float64{{0, 1, 0, 0}, {-1, 1, 0, 0}, {0.09, 0.1, 0, 0}, {1, 0, 0, 0}, {1, 0.1, 2, 0}, {math.NaN(), 1, 0, 0}, {1, math.Inf(1), 0, 0}, {1, 0.1, 0, math.NaN()}} {
		if _, err := FloorQuantity(tc[0], tc[1], tc[2], tc[3]); err == nil {
			t.Fatalf("accepted unsafe quantity %v", tc)
		}
	}
}

func TestSyncLoopDuplicateStopAndRestart(t *testing.T) {
	var loop SyncLoop
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	if !loop.Start(time.Millisecond, func() {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}) {
		t.Fatal("start")
	}
	if loop.Start(time.Millisecond, func() { t.Error("duplicate callback") }) {
		t.Fatal("duplicate start accepted")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback never started")
	}
	stopped := make(chan struct{})
	go func() { loop.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop did not join callback")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop hung")
	}
	count := calls.Load()
	loop.Stop()
	restarted := make(chan struct{}, 1)
	if !loop.Start(time.Millisecond, func() {
		select {
		case restarted <- struct{}{}:
		default:
		}
	}) {
		t.Fatal("restart failed")
	}
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart did not run")
	}
	loop.Stop()
	if calls.Load() != count {
		t.Fatal("old callback ran after stop")
	}
}

func TestSyncLoopSurvivesCallbackPanic(t *testing.T) {
	var loop SyncLoop
	var count atomic.Int32
	continued := make(chan struct{}, 1)
	loop.Start(time.Millisecond, func() {
		if count.Add(1) == 1 {
			panic("malformed upstream response")
		}
		select {
		case continued <- struct{}{}:
		default:
		}
	})
	defer loop.Stop()
	select {
	case <-continued:
	case <-time.After(time.Second):
		t.Fatal("synchronizer died on panic")
	}
}
