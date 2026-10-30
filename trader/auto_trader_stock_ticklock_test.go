package trader

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Done is evaluated by the caller while waiting, then by the queued acquirer.
type stockTickLockContext struct {
	context.Context
	calls chan struct{}
}

func (ctx *stockTickLockContext) Done() <-chan struct{} {
	select {
	case ctx.calls <- struct{}{}:
	default:
	}
	return ctx.Context.Done()
}

func stockTickLockWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func stockTickLockAvailable(t *testing.T, mu *sync.Mutex) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	retry := time.NewTicker(time.Millisecond)
	defer retry.Stop()
	for {
		if mu.TryLock() {
			mu.Unlock()
			return
		}
		select {
		case <-retry.C:
		case <-deadline.C:
			t.Fatal("queued acquisition leaked the mutex")
		}
	}
}

func TestLockWithContextFastPath(t *testing.T) {
	var mu sync.Mutex
	if !lockWithContext(context.Background(), &mu) {
		t.Fatal("uncontended acquisition failed")
	}
	if mu.TryLock() {
		mu.Unlock()
		t.Fatal("successful acquisition did not hold the mutex")
	}
	mu.Unlock()
	stockTickLockAvailable(t, &mu)
}

func TestLockWithContextAlreadyCancelled(t *testing.T) {
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if lockWithContext(ctx, &mu) {
		mu.Unlock()
		t.Fatal("already cancelled context acquired the mutex")
	}
	stockTickLockAvailable(t, &mu)
}

func TestLockWithContextCancelledWhileWaiting(t *testing.T) {
	var mu sync.Mutex
	mu.Lock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(mu.Unlock) }
	ctx, cancel := context.WithCancel(context.Background())
	observed := &stockTickLockContext{Context: ctx, calls: make(chan struct{}, 2)}
	done := make(chan struct{})
	var locked bool
	go func() {
		locked = lockWithContext(observed, &mu)
		if locked {
			mu.Unlock()
		}
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		release()
		stockTickLockWait(t, done, "lock helper cleanup")
	})
	stockTickLockWait(t, observed.calls, "lock helper to wait")
	cancel()
	stockTickLockWait(t, done, "cancelled lock helper")
	if locked {
		t.Fatal("cancelled waiter acquired the mutex")
	}
	release()
	stockTickLockWait(t, observed.calls, "abandoned acquisition")
	stockTickLockAvailable(t, &mu)
}

func TestStockProtectionTickWaitsForAccountLock(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "runs_after_waiting"
		if cancelled {
			name = "cancelled_while_waiting"
		}
		t.Run(name, func(t *testing.T) {
			fx := newStockFixture(t, false, stockTestNow, func(int) string { return holdAAPL })
			fx.openLiveOwned(t, 16.666, 16.666)
			if err := fx.at.stockPlaceProtection(context.Background(), "AAPLBUSDT", 16.666, 115, 0, 120); err != nil {
				t.Fatal(err)
			}
			fx.spot.cancelProtection("AAPLBUSDT")
			fx.spot.setProt = nil
			// Use the account mutex shared with a crypto peer.
			fx.at.exchangeID = t.Name()
			peer := &AutoTrader{userID: fx.at.userID, exchangeID: fx.at.exchangeID}
			t.Cleanup(func() { unregisterAccountTrader(peer); unregisterAccountTrader(fx.at) })
			mu := fx.at.stockMutex()
			if mu != peer.executionMutex() {
				t.Fatal("live stock and peer do not share the execution mutex")
			}
			mu.Lock()
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(mu.Unlock) }
			ctx, cancel := context.WithCancel(context.Background())
			observed := &stockTickLockContext{Context: ctx, calls: make(chan struct{}, 2)}
			done := make(chan struct{})
			go func() {
				fx.at.stockProtectionTick(observed)
				close(done)
			}()
			t.Cleanup(func() {
				cancel()
				release()
				stockTickLockWait(t, done, "protection tick cleanup")
			})
			stockTickLockWait(t, observed.calls, "protection tick to wait")
			select {
			case <-done:
				t.Fatal("protection tick returned while the account lock was held")
			default:
			}
			if fx.at.stock.tickCount != 0 || len(fx.spot.setProt) != 0 {
				t.Fatal("protection tick ran while the account lock was held")
			}
			if cancelled {
				cancel()
				stockTickLockWait(t, done, "cancelled protection tick")
				if fx.at.stock.tickCount != 0 || len(fx.spot.setProt) != 0 {
					t.Fatal("cancelled protection tick ran")
				}
				release()
			} else {
				release()
				stockTickLockWait(t, done, "protection tick")
				if fx.at.stock.tickCount != 1 || len(fx.spot.setProt) != 1 {
					t.Fatalf("tick did not re-place protection: ticks=%d placements=%v", fx.at.stock.tickCount, fx.spot.setProt)
				}
			}
			stockTickLockWait(t, observed.calls, "queued tick acquisition")
			stockTickLockAvailable(t, mu)
		})
	}
}
