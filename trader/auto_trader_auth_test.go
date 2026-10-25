package trader

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/kernel"
	"nofx/trader/binance"
)

type authBalanceMock struct {
	entryMock
	mu            sync.Mutex
	balanceCalls  int
	positionCalls int
	balanceErr    error
	positionErr   error
	balanceFn     func() error
}

func (m *authBalanceMock) GetBalance() (map[string]interface{}, error) {
	m.mu.Lock()
	m.balanceCalls++
	err, fn := m.balanceErr, m.balanceFn
	m.mu.Unlock()
	if fn != nil {
		err = fn()
	}
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"totalEquity": 1000.0, "availableBalance": 1000.0}, nil
}

func (m *authBalanceMock) GetPositions() ([]map[string]interface{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.positionCalls++
	return nil, m.positionErr
}

func (m *authBalanceMock) setBalanceError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.balanceErr = err
}

func (m *authBalanceMock) calls() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.balanceCalls, m.positionCalls
}

type authTestClock struct{ nanos atomic.Int64 }

func (c *authTestClock) now() time.Time      { return time.Unix(0, c.nanos.Load()) }
func (c *authTestClock) add(d time.Duration) { c.nanos.Add(int64(d)) }

type authTestAlerts struct {
	mu       sync.Mutex
	messages []string
}

func (a *authTestAlerts) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.messages...)
}

func authFixture(t *testing.T) (*AutoTrader, *authBalanceMock, *authTestClock, *authTestAlerts) {
	t.Helper()
	clock := &authTestClock{}
	clock.nanos.Store(time.Date(2026, 10, 25, 0, 44, 0, 0, time.UTC).UnixNano())
	m := &authBalanceMock{}
	at := &AutoTrader{name: "auth-test", trader: m, authNow: clock.now, isRunning: true}
	alerts := &authTestAlerts{}
	previous := authNotify
	authNotify = func(kind, title, message string) {
		if kind != "ALERT" || title != at.name {
			t.Errorf("wrong notification destination: %s / %s", kind, title)
		}
		alerts.mu.Lock()
		defer alerts.mu.Unlock()
		alerts.messages = append(alerts.messages, message)
	}
	t.Cleanup(func() { authNotify = previous })
	return at, m, clock, alerts
}

func authTestError(ip string) error {
	return fmt.Errorf("%w: Invalid API-key, IP, or permissions; request ip: %s", binance.ErrAuthOrIP, ip)
}

func TestAuthRecoveryDueAndSuccess(t *testing.T) {
	at, m, clock, alerts := authFixture(t)
	at.probeAuthRecovery()
	at.markAuthBlocked(authTestError("1.2.3.4"))
	clock.add(time.Minute - time.Nanosecond)
	at.probeAuthRecovery()
	if calls, _ := m.calls(); calls != 0 {
		t.Fatalf("not due: GetBalance called %d times", calls)
	}
	clock.add(time.Nanosecond)
	at.probeAuthRecovery()
	if calls, _ := m.calls(); calls != 1 || at.isAuthBlocked() {
		t.Fatalf("due success did not recover: calls=%d blocked=%v", calls, at.isAuthBlocked())
	}
	at.authMu.Lock()
	reset := at.authBlockedReason == "" && at.authBlockedSince.IsZero() && at.authNextProbeAt.IsZero() &&
		at.authProbeCount == 0 && at.authLastAlertAt.IsZero() && at.authLastProbeError == "" && !at.authProbeInFlight
	at.authMu.Unlock()
	if !reset {
		t.Fatal("recovery left stale backoff/alert state")
	}
	messages := alerts.snapshot()
	if len(messages) != 2 || !strings.Contains(messages[1], "✅ Binance 认证已恢复，交易继续（被拦 0 小时 1 分，探测 1 次）") {
		t.Fatalf("wrong recovery notification: %v", messages)
	}
	if !strings.Contains(messages[0], "无需重启，会自动探测恢复") {
		t.Fatalf("initial alert still requires restart: %s", messages[0])
	}
	clock.add(2 * time.Hour)
	at.probeAuthRecovery()
	at.clearAuthBlocked()
	if calls, _ := m.calls(); calls != 1 || len(alerts.snapshot()) != 2 {
		t.Fatal("recovered trader kept probing or notifying")
	}
}

func TestAuthBlockedBackoffAndLatestIP(t *testing.T) {
	at, m, clock, alerts := authFixture(t)
	at.markAuthBlocked(authTestError("1.2.3.4"))
	delay := time.Minute
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		clock.add(delay - time.Nanosecond)
		at.probeAuthRecovery()
		if calls, _ := m.calls(); calls != i {
			t.Fatalf("probe %d ran early: %d calls", i+1, calls)
		}
		clock.add(time.Nanosecond)
		latest := authTestError(fmt.Sprintf("10.0.0.%d", i+1))
		m.setBalanceError(latest)
		at.probeAuthRecovery()
		blocked, reason := at.authBlockStatus()
		at.authMu.Lock()
		gotDelay, count := at.authNextProbeAt.Sub(clock.now()), at.authProbeCount
		at.authMu.Unlock()
		if !blocked || reason != latest.Error() || gotDelay != want || count != i+1 {
			t.Fatalf("probe %d: blocked=%v reason=%q delay=%s count=%d", i+1, blocked, reason, gotDelay, count)
		}
		at.probeAuthRecovery()
		if calls, _ := m.calls(); calls != i+1 {
			t.Fatalf("duplicate probe: %d calls, want %d", calls, i+1)
		}
		delay = want
	}
	if len(alerts.snapshot()) != 1 {
		t.Fatalf("auth failures repeated first alert: %v", alerts.snapshot())
	}
	m.setBalanceError(nil)
	clock.add(delay)
	at.probeAuthRecovery()
	messages := alerts.snapshot()
	if at.isAuthBlocked() || !strings.Contains(messages[len(messages)-1], "探测 6 次") {
		t.Fatalf("failed attempts missing from recovery count: %v", messages)
	}
	at.markAuthBlocked(authTestError("10.0.0.9"))
	at.authMu.Lock()
	defer at.authMu.Unlock()
	if at.authProbeCount != 0 || at.authNextProbeAt.Sub(clock.now()) != time.Minute {
		t.Fatal("new blocking episode inherited old backoff")
	}
}

func TestAuthBlockedInconclusiveProbe(t *testing.T) {
	for _, err := range []error{io.EOF, context.DeadlineExceeded} {
		t.Run(err.Error(), func(t *testing.T) {
			at, m, clock, alerts := authFixture(t)
			original := authTestError("1.2.3.4")
			at.markAuthBlocked(original)
			m.setBalanceError(err)
			clock.add(time.Minute)
			at.probeAuthRecovery()
			blocked, reason := at.authBlockStatus()
			at.authMu.Lock()
			delay, failure := at.authNextProbeAt.Sub(clock.now()), at.authLastProbeError
			at.authMu.Unlock()
			if !blocked || reason != original.Error() || delay != time.Minute || failure != err.Error() || len(alerts.snapshot()) != 1 {
				t.Fatalf("inconclusive result changed auth state: blocked=%v reason=%q delay=%s alerts=%v", blocked, reason, delay, alerts.snapshot())
			}
		})
	}
}

func TestAuthBlockedReminderThrottleAndRecovery(t *testing.T) {
	at, m, clock, alerts := authFixture(t)
	at.markAuthBlocked(authTestError("1.2.3.4"))
	m.setBalanceError(authTestError("10.0.0.1"))
	clock.add(29 * time.Minute)
	at.probeAuthRecovery()
	if len(alerts.snapshot()) != 1 {
		t.Fatal("reminded within 30 minutes")
	}
	clock.add(time.Minute)
	m.setBalanceError(authTestError("10.0.0.2"))
	at.probeAuthRecovery()
	at.probeAuthRecovery()
	messages := alerts.snapshot()
	if len(messages) != 2 || !strings.Contains(messages[1], "仍被拦（已 0 小时 30 分") || !strings.Contains(messages[1], "request ip: 10.0.0.2") {
		t.Fatalf("missing/throttled reminder or old IP: %v", messages)
	}
	clock.add(29 * time.Minute)
	m.setBalanceError(io.EOF)
	at.probeAuthRecovery()
	if len(alerts.snapshot()) != 2 {
		t.Fatal("reminded again within 30 minutes")
	}
	clock.add(time.Minute) // next probe is not due; the reminder is due
	at.probeAuthRecovery()
	messages = alerts.snapshot()
	if len(messages) != 3 || !strings.Contains(messages[2], "已 1 小时 0 分") ||
		!strings.Contains(messages[2], "EOF") || !strings.Contains(messages[2], "request ip: 10.0.0.2") {
		t.Fatalf("reminder lost latest probe error or last auth IP: %v", messages)
	}
	m.setBalanceError(nil)
	clock.add(4 * time.Minute)
	at.probeAuthRecovery()
	clock.add(time.Hour)
	at.probeAuthRecovery()
	if got := alerts.snapshot(); len(got) != 4 || !strings.Contains(got[3], "✅ Binance 认证已恢复") {
		t.Fatalf("continued reminders after recovery: %v", got)
	}
}

func TestAuthBlockedAlertIntervalInjection(t *testing.T) {
	at, m, clock, alerts := authFixture(t)
	at.authAlertInterval = 2 * time.Minute
	at.markAuthBlocked(authTestError("1.2.3.4"))
	m.setBalanceError(io.EOF)
	clock.add(2 * time.Minute)
	at.probeAuthRecovery()
	if len(alerts.snapshot()) != 2 {
		t.Fatal("injected reminder interval not used")
	}
}

func TestAuthBlockedMarkIdempotentAndConcurrent(t *testing.T) {
	at, _, clock, alerts := authFixture(t)
	err := authTestError("1.2.3.4")
	at.markAuthBlocked(nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); at.markAuthBlocked(err) }()
	}
	wg.Wait()
	if len(alerts.snapshot()) != 1 {
		t.Fatalf("concurrent mark repeated initial alert: %v", alerts.snapshot())
	}
	clock.add(20 * time.Minute)
	at.markAuthBlocked(authTestError("10.0.0.1"))
	at.authMu.Lock()
	unchanged := at.authBlockedSince.Equal(clock.now().Add(-20*time.Minute)) &&
		at.authNextProbeAt.Equal(at.authBlockedSince.Add(time.Minute)) && at.authLastAlertAt.Equal(at.authBlockedSince) && at.authProbeCount == 0
	at.authMu.Unlock()
	if !unchanged || len(alerts.snapshot()) != 1 {
		t.Fatal("repeated mark reset the episode or sent another first alert")
	}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				at.markAuthBlocked(err)
				at.isAuthBlocked()
				at.authBlockStatus()
				at.clearAuthBlocked()
			}
		}()
	}
	wg.Wait()
	at.clearAuthBlocked()
	if at.isAuthBlocked() {
		t.Fatal("concurrent marks/clears left blocked state")
	}
}

func TestAuthRecoverySingleFlightAndStaleSuccess(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear_and_remark=%v", reset), func(t *testing.T) {
			at, m, clock, alerts := authFixture(t)
			at.markAuthBlocked(authTestError("1.2.3.4"))
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			m.balanceFn = func() error { close(started); <-release; return nil }
			clock.add(time.Minute)
			go func() { defer close(done); at.probeAuthRecovery() }()
			<-started
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); at.probeAuthRecovery() }()
			}
			wg.Wait()
			if calls, _ := m.calls(); calls != 1 {
				t.Errorf("concurrent probes made %d balance calls", calls)
			}
			if reset {
				at.clearAuthBlocked()
			}
			latest := authTestError("10.0.0.1")
			at.markAuthBlocked(latest)
			before := len(alerts.snapshot())
			close(release)
			<-done
			if blocked, reason := at.authBlockStatus(); !blocked || reason != latest.Error() || len(alerts.snapshot()) != before {
				t.Fatalf("stale success cleared newer rejection: blocked=%v reason=%q alerts=%v", blocked, reason, alerts.snapshot())
			}
			m.balanceFn = nil
			at.probeAuthRecovery()
			if calls, _ := m.calls(); calls != 1 {
				t.Fatal("stale success allowed retry less than a minute later")
			}
			clock.add(time.Minute)
			at.probeAuthRecovery()
			if at.isAuthBlocked() {
				t.Fatal("fresh success failed to recover")
			}
		})
	}
}

func TestAuthProbeMinimumInterval(t *testing.T) {
	for i := -1; i < 100; i++ {
		if d := authProbeDelay(i); d < 60*time.Second {
			t.Fatalf("probe %d delay is below 60 seconds: %s", i, d)
		}
	}
	at, m, clock, _ := authFixture(t)
	at.markAuthBlocked(authTestError("1.2.3.4"))
	m.balanceFn = func() error { clock.add(30 * time.Second); return io.EOF }
	clock.add(time.Minute)
	at.probeAuthRecovery()
	clock.add(time.Minute - time.Nanosecond)
	at.probeAuthRecovery()
	if calls, _ := m.calls(); calls != 1 {
		t.Fatal("retry was scheduled less than 60 seconds after probe completion")
	}
	clock.add(time.Nanosecond)
	at.probeAuthRecovery()
	if calls, _ := m.calls(); calls != 2 {
		t.Fatal("retry not run after full minimum interval")
	}
}

func TestAuthBlockedMonitorAndCycleGuards(t *testing.T) {
	at, m, clock, _ := authFixture(t)
	at.markAuthBlocked(authTestError("1.2.3.4"))
	decide := func(*kernel.Context) (*kernel.FullDecision, error) {
		t.Fatal("blocked cycle called AI")
		return nil, nil
	}
	// A blocked cycle skips AI/decisions. Order maintenance and the protection
	// loops are NOT gated by the flag (they behave exactly as before), so this
	// test only pins the probe and the AI gate, not their exchange reads.
	if err := at.runCycleWithDecision(decide); err != nil {
		t.Fatal(err)
	}
	// Probe not due yet: the minute monitor must not touch GetBalance.
	at.safeCheckPositionDrawdown()
	if balances, _ := m.calls(); balances != 0 {
		t.Fatalf("not-due monitor probed the exchange: balance=%d", balances)
	}
	// Probe due but still rejected: exactly one read-only probe, still blocked.
	clock.add(time.Minute)
	m.setBalanceError(authTestError("10.0.0.1"))
	at.safeCheckPositionDrawdown()
	if balances, _ := m.calls(); balances != 1 || !at.isAuthBlocked() {
		t.Fatalf("due rejected probe: balance=%d blocked=%v", balances, at.isAuthBlocked())
	}
	// Next probe succeeds: block cleared by the monitor, no restart.
	clock.add(time.Minute)
	m.setBalanceError(nil)
	at.safeCheckPositionDrawdown()
	if balances, _ := m.calls(); balances != 2 || at.isAuthBlocked() {
		t.Fatalf("minute monitor did not resume on success: balance=%d blocked=%v", balances, at.isAuthBlocked())
	}
	// Reuse the established empty/idle-cycle fixture to exercise the next
	// regular cycle without market-data or AI dependencies.
	normal := c2Trader(&c2PendingMock{audit05Mock: audit05Mock{flat: true}})
	at.config, at.strategyEngine = normal.config, normal.strategyEngine
	at.lastResetTime = clock.now()
	if err := at.runCycleWithDecision(decide); err != nil {
		t.Fatal(err)
	}
	if balances, _ := m.calls(); balances <= 2 {
		t.Fatal("next cycle stayed paused after monitor recovery")
	}
}

func TestAuthBlockedContextMarksBothReadFailures(t *testing.T) {
	for _, positions := range []bool{false, true} {
		t.Run(fmt.Sprintf("positions=%v", positions), func(t *testing.T) {
			at, m, _, alerts := authFixture(t)
			err := authTestError("10.0.0.1")
			if positions {
				m.positionErr = err
			} else {
				m.setBalanceError(err)
			}
			if _, err := at.buildTradingContext(); err == nil || !at.isAuthBlocked() || len(alerts.snapshot()) != 1 {
				t.Fatalf("context failure not marked: err=%v blocked=%v alerts=%v", err, at.isAuthBlocked(), alerts.snapshot())
			}
		})
	}
}

func TestAuthBlockedShortReasonKeepsIPAndEscapes(t *testing.T) {
	at, m, clock, alerts := authFixture(t)
	err := authTestError("10.0.0.1")
	err = fmt.Errorf("<proxy>& %s: %w", strings.Repeat("失败", 200), err)
	at.markAuthBlocked(err)
	m.setBalanceError(err)
	clock.add(30 * time.Minute)
	at.probeAuthRecovery()
	for _, message := range alerts.snapshot() {
		if !strings.Contains(message, "request ip: 10.0.0.1") || !strings.Contains(message, "&lt;proxy&gt;&amp;") {
			t.Fatalf("short alert lost IP or contains unescaped HTML: %s", message)
		}
	}
}

func TestAuthBlockedReminderKeepsNewerMarkedIP(t *testing.T) {
	at, m, clock, alerts := authFixture(t)
	previous := fmt.Errorf("%s: %w", strings.Repeat("old error ", 40), authTestError("1.2.3.4"))
	at.markAuthBlocked(previous)
	m.setBalanceError(previous)
	clock.add(time.Minute)
	at.probeAuthRecovery()
	latest := fmt.Errorf("%s: %w", strings.Repeat("new error ", 40), authTestError("10.0.0.1"))
	at.markAuthBlocked(latest)
	// Exercise an alert between probes, after a concurrent path has updated
	// the auth reason. Both long errors must keep their respective IPs.
	at.authMu.Lock()
	at.authNextProbeAt = clock.now().Add(time.Hour)
	at.authMu.Unlock()
	clock.add(29 * time.Minute)
	at.probeAuthRecovery()
	messages := alerts.snapshot()
	if len(messages) != 2 || !strings.Contains(messages[1], "request ip: 10.0.0.1") || !strings.Contains(messages[1], "request ip: 1.2.3.4") {
		t.Fatalf("truncation lost the latest marked IP: %v", messages)
	}
}
