package binance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/common"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func assertFapiV3Cooldown(t *testing.T, tr *FuturesTrader, account bool, want time.Time) {
	t.Helper()
	s := tr.readState()
	var until time.Time
	var enabled bool
	if account {
		s.accountMu.Lock()
		until, enabled = s.accountDisabledUntil, s.useAccountV3
		s.accountMu.Unlock()
	} else {
		s.positionMu.Lock()
		until, enabled = s.positionDisabledUntil, s.usePositionV3
		s.positionMu.Unlock()
	}
	if !until.Equal(want) || enabled {
		t.Fatalf("cooldown until=%v enabled=%t; want until=%v enabled=false", until, enabled, want)
	}
}

func readFapiV3Snapshot(t *testing.T, tr *FuturesTrader, account bool) error {
	t.Helper()
	if account {
		got, err := tr.GetBalance()
		if got["totalWalletBalance"] != float64(100) || got["totalEquity"] != float64(105) {
			t.Fatalf("wrong account snapshot: %v err=%v", got, err)
		}
		return err
	}
	got, err := tr.GetPositions()
	if len(got) != 3 || got[0]["positionAmt"] != float64(2) || got[2]["leverage"] != float64(5) {
		t.Fatalf("wrong position snapshot: %v err=%v", got, err)
	}
	return err
}

func fapiV3CooldownLogCount(hook *logtest.Hook, level logrus.Level) int {
	count := 0
	for _, e := range hook.AllEntries() {
		if e.Level == level && strings.Contains(e.Message, "v3 cooling down") {
			count++
		}
	}
	return count
}

func TestFuturesV3TransientCooldownAndRecovery(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		err        error
		auth       bool
	}{
		{name: "EOF", err: io.EOF},
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "proxy-reset", err: errors.New("proxy: connection reset by peer")},
		{name: "500-parameter-code", status: 500, body: `{"code":-1102,"msg":"server error"}`},
		{name: "503-proxy-body", status: 503, body: "upstream unavailable"},
		{name: "1000", status: 400, body: `{"code":-1000,"msg":"unknown"}`},
		{name: "1001", status: 400, body: `{"code":-1001,"msg":"disconnected"}`},
		{name: "1003", status: 429, body: `{"code":-1003,"msg":"rate limited"}`},
		{name: "1007", status: 400, body: `{"code":-1007,"msg":"timeout"}`},
		{name: "1008", status: 400, body: `{"code":-1008,"msg":"overloaded"}`},
		{name: "2015", status: 401, body: `{"code":-2015,"msg":"IP rejected"}`, auth: true},
		{name: "2014", status: 401, body: `{"code":-2014,"msg":"API key format"}`, auth: true},
	}
	for _, account := range []bool{false, true} {
		for _, adopted := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("account=%t/adopted=%t/%s", account, adopted, tc.name), func(t *testing.T) {
					hook := fapiErrorHook(t)
					tr, f, clock := newFapiV3Fixture(t)
					path := "positionRisk"
					if account {
						path = "account"
					}
					v2, v3 := "/fapi/v2/"+path, "/fapi/v3/"+path
					initial := 0
					if adopted {
						if err := readFapiV3Snapshot(t, tr, account); err != nil {
							t.Fatal(err)
						}
						initial = 1
						tr.InvalidateAccountCache()
					}
					if tc.err != nil {
						f.setTransportError(v3, tc.err)
					} else {
						f.set(v3, tc.status, tc.body)
					}
					// A successful v2 fallback means the account is readable: even an
					// auth/IP error from v3 alone (a rotating proxy can exit through a
					// non-whitelisted IP on just one of two concurrent requests) must
					// NOT surface, or the caller would discard good data and the
					// trader would be falsely auth-blocked.
					if err := readFapiV3Snapshot(t, tr, account); err != nil {
						t.Fatalf("v3 %s error leaked despite successful v2 fallback: %v", tc.name, err)
					}
					assertFapiV3Cooldown(t, tr, account, clock.now().Add(5*time.Minute))
					if f.count(v2) != initial+1 || f.count(v3) != initial+1 {
						t.Fatalf("fallback did not reuse/fetch exactly one v2 snapshot: %+v", f.counts)
					}
					// Healthy upstreams must not be probed before the deadline.
					f.setTransportError(v3, nil)
					if account {
						f.set(v3, 200, batch1BalanceJSON)
					} else {
						f.set(v3, 200, fapiPositionsV3JSON())
					}
					clock.advance(5*time.Minute - time.Nanosecond)
					for i := 0; i < 3; i++ {
						tr.InvalidateAccountCache()
						if err := readFapiV3Snapshot(t, tr, account); err != nil {
							t.Fatal(err)
						}
					}
					// Recreation must not bypass the endpoint/credential cooldown.
					recreated := &FuturesTrader{client: tr.client, nowFn: clock.now}
					if err := readFapiV3Snapshot(t, recreated, account); err != nil {
						t.Fatal(err)
					}
					if f.count(v3) != initial+1 || f.count(v2) != initial+5 {
						t.Fatalf("cooldown did not read v2 only: %+v", f.counts)
					}
					if fapiV3CooldownLogCount(hook, logrus.WarnLevel) != 1 || fapiV3CooldownLogCount(hook, logrus.ErrorLevel) != 0 {
						t.Fatal("transient cooldown must log Warn once and no semantic Error")
					}
					clock.advance(time.Nanosecond)
					tr.InvalidateAccountCache()
					if err := readFapiV3Snapshot(t, tr, account); err != nil {
						t.Fatal(err)
					}
					if f.count(v2) != initial+6 || f.count(v3) != initial+2 {
						t.Fatalf("expiry skipped complete reconciliation: %+v", f.counts)
					}
					tr.InvalidateAccountCache()
					if err := readFapiV3Snapshot(t, tr, account); err != nil {
						t.Fatal(err)
					}
					if f.count(v2) != initial+6 || f.count(v3) != initial+3 {
						t.Fatalf("healthy v3 was not restored: %+v", f.counts)
					}
				})
			}
		}
	}
}

func TestFuturesV3CooldownWarnOncePerPeriod(t *testing.T) {
	for _, account := range []bool{false, true} {
		t.Run(fmt.Sprintf("account=%t", account), func(t *testing.T) {
			hook := fapiErrorHook(t)
			tr, f, clock := newFapiV3Fixture(t)
			path := "positionRisk"
			if account {
				path = "account"
			}
			v3 := "/fapi/v3/" + path
			f.set(v3, 500, "server unavailable")
			for cycle := 1; cycle <= 2; cycle++ {
				for i := 0; i < 5; i++ {
					tr.InvalidateAccountCache()
					if err := readFapiV3Snapshot(t, tr, account); err != nil {
						t.Fatal(err)
					}
				}
				if f.count(v3) != cycle || fapiV3CooldownLogCount(hook, logrus.WarnLevel) != cycle {
					t.Fatal("requests/logs are not bounded to once per cooldown")
				}
				assertFapiV3Cooldown(t, tr, account, clock.now().Add(5*time.Minute))
				clock.advance(5 * time.Minute)
			}
		})
	}
}

func TestFuturesV3LeveragePersistentFailureCooldown(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		t.Run(fmt.Sprintf("adopted=%t", adopted), func(t *testing.T) {
			hook := fapiErrorHook(t)
			tr, f, clock := newFapiV3Fixture(t)
			initial := 0
			if adopted {
				if err := readFapiV3Snapshot(t, tr, false); err != nil {
					t.Fatal(err)
				}
				initial = 1
				tr.invalidateSymbolConfig("YUSDT")
			}
			f.set("config:YUSDT", 403, `{"code":-1102,"msg":"config unavailable"}`)
			for cycle := 1; cycle <= 2; cycle++ {
				// Keep the existing snapshot TTL: five consecutive public reads
				// reuse one full v2 fallback snapshot per cooldown cycle.
				for i := 0; i < 5; i++ {
					if err := readFapiV3Snapshot(t, tr, false); err != nil {
						t.Fatal(err)
					}
				}
				if f.count("/fapi/v3/positionRisk") != initial+cycle || f.count("config:YUSDT") != initial+cycle || f.count("/fapi/v2/positionRisk") != initial+cycle+5*(cycle-1) {
					t.Fatalf("persistent config failure amplified requests: %+v", f.counts)
				}
				assertFapiV3Cooldown(t, tr, false, clock.now().Add(5*time.Minute))
				// Forced cache misses still need fresh v2 data, but must not
				// retry v3/config or emit another cooldown warning.
				for i := 0; i < 5; i++ {
					tr.InvalidateAccountCache()
					if err := readFapiV3Snapshot(t, tr, false); err != nil {
						t.Fatal(err)
					}
				}
				if f.count("/fapi/v3/positionRisk") != initial+cycle || f.count("config:YUSDT") != initial+cycle || fapiV3CooldownLogCount(hook, logrus.WarnLevel) != cycle {
					t.Fatal("cache misses retried v3/config during cooldown")
				}
				clock.advance(5 * time.Minute)
				tr.InvalidateAccountCache()
			}
		})
	}
}

// When v2 fails as well, the auth/IP error must stay detectable so the
// trader's recovery probe (PR #26) can act.
func TestFuturesV3FallbackErrorKeepsAuthWhenV2AlsoFails(t *testing.T) {
	authErr := &common.APIError{Code: -2015, Message: "Invalid API-key, IP, or permissions for action"}
	if err := futuresV3FallbackError("GetPositions", authErr, nil); err != nil {
		t.Fatalf("successful v2 fallback must suppress v3 errors, got %v", err)
	}
	err := futuresV3FallbackError("GetPositions", authErr, authErr)
	if err == nil || !IsAuthOrIPError(err) {
		t.Fatalf("both sources failed with auth error but it is not detectable: %v", err)
	}
	if err := futuresV3FallbackError("GetPositions", errors.New("connection reset by peer"), errors.New("EOF")); err == nil {
		t.Fatal("both sources failed but no error returned")
	}
}
