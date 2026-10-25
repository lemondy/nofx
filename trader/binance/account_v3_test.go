package binance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"nofx/logger"
)

const fapiPositionsV2JSON = `[
{"symbol":"XUSDT","positionSide":"LONG","positionAmt":"2","entryPrice":"100","markPrice":"110","unRealizedProfit":"20","liquidationPrice":"50","leverage":"3"},
{"symbol":"XUSDT","positionSide":"SHORT","positionAmt":"-1","entryPrice":"100","markPrice":"110","unRealizedProfit":"-10","liquidationPrice":"180","leverage":"3"},
{"symbol":"YUSDT","positionSide":"BOTH","positionAmt":"-2","entryPrice":"200","markPrice":"190","unRealizedProfit":"20","liquidationPrice":"250","leverage":"5"},
{"symbol":"ZEROUSDT","positionSide":"BOTH","positionAmt":"0"}]`

func fapiPositionsV3JSON() string {
	return strings.ReplaceAll(strings.ReplaceAll(fapiPositionsV2JSON, `,"leverage":"3"`, ""), `,"leverage":"5"`, "")
}

type fapiReply struct {
	status int
	body   string
}
type fapiV3Fixture struct {
	mu              sync.Mutex
	counts          map[string]int
	replies         map[string]fapiReply
	transportErrors map[string]error
}

func newFapiV3Fixture(t *testing.T) (*FuturesTrader, *fapiV3Fixture, *fapiTestClock) {
	t.Helper()
	f := &fapiV3Fixture{counts: make(map[string]int), replies: map[string]fapiReply{
		"/fapi/v2/positionRisk":      {200, fapiPositionsV2JSON},
		"/fapi/v3/positionRisk":      {200, fapiPositionsV3JSON()},
		"/fapi/v2/account":           {200, batch1BalanceJSON},
		"/fapi/v3/account":           {200, batch1BalanceJSON},
		"config:XUSDT":               {200, `[{"symbol":"XUSDT","leverage":3,"marginType":"CROSSED"}]`},
		"config:YUSDT":               {200, `[{"symbol":"YUSDT","leverage":5,"marginType":"ISOLATED"}]`},
		"/fapi/v1/leverage":          {200, `{"symbol":"XUSDT","leverage":7,"maxNotionalValue":"1000000"}`},
		"/fapi/v1/marginType":        {200, `{"code":200,"msg":"success"}`},
		"/fapi/v1/positionSide/dual": {200, `{"dualSidePosition":true}`},
	}}
	tr := fapiTestTrader(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if key == "/fapi/v1/symbolConfig" {
			if r.URL.Query().Get("symbol") == "" {
				t.Error("symbolConfig requested the full table")
			}
			key = "config:" + r.URL.Query().Get("symbol")
		}
		f.mu.Lock()
		f.counts[key]++
		reply, ok := f.replies[key]
		f.mu.Unlock()
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 404)
			return
		}
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	})
	f.transportErrors = make(map[string]error)
	base := tr.client.HTTPClient.Transport
	tr.client.HTTPClient.Transport = batch1Transport(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		err := f.transportErrors[r.URL.Path]
		if err != nil {
			f.counts[r.URL.Path]++
		}
		f.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return base.RoundTrip(r)
	})
	clock := newFapiTestClock()
	tr.nowFn = clock.now
	return tr, f, clock
}
func (f *fapiV3Fixture) set(path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[path] = fapiReply{status, body}
}
func (f *fapiV3Fixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[path]
}

func (f *fapiV3Fixture) setTransportError(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transportErrors[path] = err
}

func fapiErrorHook(t *testing.T) *logtest.Hook {
	t.Helper()
	previous := logger.Log.ReplaceHooks(make(logrus.LevelHooks))
	hook := logtest.NewLocal(logger.Log)
	t.Cleanup(func() { logger.Log.ReplaceHooks(previous) })
	return hook
}
func assertFapiErrorLog(t *testing.T, hook *logtest.Hook, detail string) {
	t.Helper()
	for _, e := range hook.AllEntries() {
		if e.Level == logrus.ErrorLevel && strings.Contains(e.Message, detail) {
			return
		}
	}
	t.Fatalf("no Error log containing %q", detail)
}

func TestFuturesV3AdoptionPreservesPositionAndBalanceMaps(t *testing.T) {
	tr, f, _ := newFapiV3Fixture(t)
	positions, err := tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	balance, err := tr.GetBalance()
	if err != nil {
		t.Fatal(err)
	}
	tr.InvalidateAccountCache()
	positions2, err := tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	balance2, err := tr.GetBalance()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(positions, positions2) || !reflect.DeepEqual(balance, balance2) {
		t.Fatal("v3 output changed between reads")
	}
	if f.count("/fapi/v2/positionRisk") != 1 || f.count("/fapi/v3/positionRisk") != 2 || f.count("/fapi/v2/account") != 1 || f.count("/fapi/v3/account") != 2 {
		t.Fatalf("wrong v2/v3 counts: %+v", f.counts)
	}
	if f.count("config:XUSDT") != 1 || f.count("config:YUSDT") != 1 || f.count("config:ZEROUSDT") != 0 {
		t.Fatal("config reads not limited to nonzero symbols")
	}
	legacyClient := futures.NewClient("legacy-key", "legacy-secret")
	legacyClient.BaseURL = futuresTestURL()
	legacyClient.HTTPClient = tr.client.HTTPClient
	legacy := &FuturesTrader{client: legacyClient}
	s := legacy.readState()
	s.positionDisabledUntil = legacy.now().Add(6 * time.Hour)
	s.accountDisabledUntil = legacy.now().Add(6 * time.Hour)
	wantPositions, err := legacy.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	wantBalance, err := legacy.GetBalance()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wantPositions, positions) || !reflect.DeepEqual(wantBalance, balance) {
		t.Fatalf("v2/v3 map mismatch:\nv2=%#v %#v\nv3=%#v %#v", wantPositions, wantBalance, positions, balance)
	}
	if len(positions) != 3 || len(positions[0]) != 8 || positions[0]["leverage"] != float64(3) || positions[1]["side"] != "short" || positions[2]["side"] != "short" || balance["totalEquity"] != float64(105) {
		t.Fatalf("map contract changed: %#v %#v", positions, balance)
	}
}

func TestFuturesV3PositionSemanticCooldown(t *testing.T) {
	base := fapiPositionsV3JSON()
	cases := []struct {
		name, body, detail string
		status             int
	}{
		{"symbol-set", strings.Replace(base, "YUSDT", "OTHERUSDT", 1), "set mismatch", 200},
		{"hedge-side", strings.Replace(base, `"SHORT"`, `"LONG"`, 1), "duplicate", 200},
		{"amount", strings.Replace(base, `"positionAmt":"2"`, `"positionAmt":"3"`, 1), "positionAmt mismatch", 200},
		{"entry", strings.Replace(base, `"entryPrice":"100"`, `"entryPrice":"105"`, 1), "entryPrice mismatch", 200},
		{"mark", strings.Replace(base, `"markPrice":"110"`, `"markPrice":"112"`, 1), "markPrice mismatch", 200},
		{"liquidation", strings.Replace(base, `"liquidationPrice":"50"`, `"liquidationPrice":"51"`, 1), "liquidationPrice mismatch", 200},
		{"pnl", strings.Replace(base, `"unRealizedProfit":"20"`, `"unRealizedProfit":"200"`, 1), "unRealizedProfit mismatch", 200},
		{"404", `{"code":-1000,"msg":"not found"}`, "not found", 404},
		{"api-error", `{"code":-1102,"msg":"invalid parameter"}`, "invalid parameter", 400},
		{"parse", `[{`, "unexpected end", 200},
		{"null", `null`, "null", 200},
		{"null-row", `[null]`, "null", 200},
		{"nan", strings.Replace(base, `"positionAmt":"2"`, `"positionAmt":"NaN"`, 1), "invalid", 200},
		{"missing-number", strings.Replace(base, `"entryPrice":"100",`, "", 1), "invalid", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := fapiErrorHook(t)
			tr, f, clock := newFapiV3Fixture(t)
			f.set("/fapi/v3/positionRisk", tc.status, tc.body)
			got, err := tr.GetPositions()
			if err != nil || len(got) != 3 || got[0]["positionAmt"] != float64(2) {
				t.Fatalf("fallback got=%v err=%v", got, err)
			}
			assertFapiErrorLog(t, hook, tc.detail)
			assertFapiV3Cooldown(t, tr, false, clock.now().Add(6*time.Hour))
			clock.advance(6*time.Hour - time.Nanosecond)
			tr.InvalidateAccountCache()
			_, err = tr.GetPositions()
			if err != nil {
				t.Fatal(err)
			}
			// A recreated trader shares the same cooldown, but expiry allows a
			// complete v2+v3 reconciliation again.
			recreated := &FuturesTrader{client: tr.client, nowFn: clock.now}
			_, err = recreated.GetPositions()
			if err != nil {
				t.Fatal(err)
			}
			if f.count("/fapi/v3/positionRisk") != 1 || f.count("/fapi/v2/positionRisk") != 3 {
				t.Fatalf("fallback retried v3: %+v", f.counts)
			}
			f.set("/fapi/v3/positionRisk", 200, base)
			clock.advance(time.Nanosecond)
			tr.InvalidateAccountCache()
			if _, err := tr.GetPositions(); err != nil {
				t.Fatal(err)
			}
			tr.InvalidateAccountCache()
			if _, err := tr.GetPositions(); err != nil {
				t.Fatal(err)
			}
			if f.count("/fapi/v3/positionRisk") != 3 || f.count("/fapi/v2/positionRisk") != 4 {
				t.Fatalf("cooldown expiry did not revalidate and recover: %+v", f.counts)
			}
		})
	}
}

func TestFuturesV3BalanceSemanticCooldown(t *testing.T) {
	for _, tc := range []struct {
		name, body, detail string
		status             int
	}{
		{"wallet", `{"totalWalletBalance":"101","availableBalance":"80","totalUnrealizedProfit":"5"}`, "totalWalletBalance mismatch", 200},
		{"available", `{"totalWalletBalance":"100","availableBalance":"90","totalUnrealizedProfit":"5"}`, "availableBalance mismatch", 200},
		{"pnl", `{"totalWalletBalance":"100","availableBalance":"80","totalUnrealizedProfit":"6"}`, "totalUnrealizedProfit mismatch", 200},
		{"404", `{"code":-1000,"msg":"not found"}`, "not found", 404},
		{"api-error", `{"code":-1102,"msg":"invalid parameter"}`, "invalid parameter", 400},
		{"parse", `{`, "unexpected end", 200},
		{"missing-total", `{}`, "invalid", 200},
		{"nan", `{"totalWalletBalance":"NaN","availableBalance":"80","totalUnrealizedProfit":"5"}`, "invalid", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := fapiErrorHook(t)
			tr, f, clock := newFapiV3Fixture(t)
			f.set("/fapi/v3/account", tc.status, tc.body)
			got, err := tr.GetBalance()
			if err != nil || got["totalEquity"] != float64(105) {
				t.Fatalf("fallback got=%v err=%v", got, err)
			}
			assertFapiErrorLog(t, hook, tc.detail)
			assertFapiV3Cooldown(t, tr, true, clock.now().Add(6*time.Hour))
			clock.advance(6*time.Hour - time.Nanosecond)
			tr.InvalidateAccountCache()
			_, err = tr.GetBalance()
			if err != nil {
				t.Fatal(err)
			}
			recreated := &FuturesTrader{client: tr.client, nowFn: clock.now}
			_, err = recreated.GetBalance()
			if err != nil {
				t.Fatal(err)
			}
			if f.count("/fapi/v3/account") != 1 || f.count("/fapi/v2/account") != 3 {
				t.Fatalf("fallback retried v3: %+v", f.counts)
			}
			f.set("/fapi/v3/account", 200, batch1BalanceJSON)
			clock.advance(time.Nanosecond)
			tr.InvalidateAccountCache()
			if _, err := tr.GetBalance(); err != nil {
				t.Fatal(err)
			}
			tr.InvalidateAccountCache()
			if _, err := tr.GetBalance(); err != nil {
				t.Fatal(err)
			}
			if f.count("/fapi/v3/account") != 3 || f.count("/fapi/v2/account") != 4 {
				t.Fatalf("cooldown expiry did not revalidate and recover: %+v", f.counts)
			}
		})
	}
}

func TestFuturesV3SixHourRecheckAndRuntimeFallback(t *testing.T) {
	for _, account := range []bool{false, true} {
		t.Run(fmt.Sprintf("account=%t", account), func(t *testing.T) {
			tr, f, clock := newFapiV3Fixture(t)
			v2, v3 := "/fapi/v2/positionRisk", "/fapi/v3/positionRisk"
			read := func() {
				t.Helper()
				tr.InvalidateAccountCache()
				if account {
					if _, err := tr.GetBalance(); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := tr.GetPositions(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if account {
				v2, v3 = "/fapi/v2/account", "/fapi/v3/account"
			}
			read()
			clock.advance(6*time.Hour - time.Nanosecond)
			read()
			if f.count(v2) != 1 || f.count(v3) != 2 {
				t.Fatal("rechecked before six hours")
			}
			clock.advance(time.Nanosecond)
			read()
			if f.count(v2) != 2 || f.count(v3) != 3 {
				t.Fatalf("no six-hour recheck: %+v", f.counts)
			}
			f.set(v3, 200, `{`)
			read()
			read()
			if f.count(v2) != 4 || f.count(v3) != 4 {
				t.Fatalf("runtime parse failure did not enter cooldown: %+v", f.counts)
			}
			assertFapiV3Cooldown(t, tr, account, clock.now().Add(6*time.Hour))
			if account {
				f.set(v3, 200, batch1BalanceJSON)
			} else {
				f.set(v3, 200, fapiPositionsV3JSON())
			}
			clock.advance(6 * time.Hour)
			read()
			read()
			if f.count(v2) != 5 || f.count(v3) != 6 {
				t.Fatalf("runtime cooldown did not recover with a full recheck: %+v", f.counts)
			}
		})
	}
}

func TestFuturesV3SixHourMismatchCooldown(t *testing.T) {
	for _, account := range []bool{false, true} {
		t.Run(fmt.Sprintf("account=%t", account), func(t *testing.T) {
			hook := fapiErrorHook(t)
			tr, f, clock := newFapiV3Fixture(t)
			if account {
				_, _ = tr.GetBalance()
			} else {
				_, _ = tr.GetPositions()
			}
			clock.advance(6 * time.Hour)
			tr.InvalidateAccountCache()
			if account {
				f.set("/fapi/v3/account", 200, strings.Replace(batch1BalanceJSON, `"100"`, `"150"`, 1))
				if _, err := tr.GetBalance(); err != nil {
					t.Fatal(err)
				}
			} else {
				f.set("/fapi/v3/positionRisk", 200, `[]`)
				if _, err := tr.GetPositions(); err != nil {
					t.Fatal(err)
				}
			}
			assertFapiErrorLog(t, hook, "cooling down for 6h0m0s")
			assertFapiV3Cooldown(t, tr, account, clock.now().Add(6*time.Hour))
		})
	}
}

func TestFuturesV3LeverageFailureFallsBackEntireSnapshotAndRecovers(t *testing.T) {
	for _, reply := range []fapiReply{{200, `[]`}, {200, `[{"symbol":"YUSDT","leverage":0,"marginType":"ISOLATED"}]`}, {404, `{"code":-1000,"msg":"missing"}`}, {200, `{`}} {
		t.Run(reply.body, func(t *testing.T) {
			tr, f, clock := newFapiV3Fixture(t)
			// First prove adoption, then lose one of the active symbols' config.
			_, _ = tr.GetPositions()
			tr.invalidateSymbolConfig("YUSDT")
			f.set("config:YUSDT", reply.status, reply.body)
			got, err := tr.GetPositions()
			if err != nil || len(got) != 3 || got[2]["leverage"] != float64(5) {
				t.Fatalf("partial/missing leverage escaped: %v %v", got, err)
			}
			if f.count("/fapi/v2/positionRisk") != 2 {
				t.Fatal("did not fetch entire v2 snapshot")
			}
			assertFapiV3Cooldown(t, tr, false, clock.now().Add(5*time.Minute))
			f.set("config:YUSDT", 200, `[{"symbol":"YUSDT","leverage":5,"marginType":"ISOLATED"}]`)
			tr.InvalidateAccountCache()
			_, err = tr.GetPositions()
			if err != nil {
				t.Fatal(err)
			}
			if f.count("/fapi/v2/positionRisk") != 3 || f.count("/fapi/v3/positionRisk") != 2 {
				t.Fatal("retried config during cooldown")
			}
			clock.advance(5 * time.Minute)
			tr.InvalidateAccountCache()
			if _, err = tr.GetPositions(); err != nil {
				t.Fatal(err)
			}
			if f.count("/fapi/v2/positionRisk") != 4 || f.count("/fapi/v3/positionRisk") != 3 {
				t.Fatal("config cooldown did not end with a full recheck")
			}
		})
	}
}

func TestFuturesV3LeverageTTLAndSettersInvalidate(t *testing.T) {
	tr, f, clock := newFapiV3Fixture(t)
	_, _ = tr.GetPositions()
	clock.advance(5*time.Minute - time.Nanosecond)
	tr.InvalidateAccountCache()
	_, _ = tr.GetPositions()
	if f.count("config:XUSDT") != 1 {
		t.Fatal("leverage cache expired early")
	}
	clock.advance(time.Nanosecond)
	tr.InvalidateAccountCache()
	_, _ = tr.GetPositions()
	if f.count("config:XUSDT") != 2 {
		t.Fatal("leverage cache not refreshed after five minutes")
	}
	// Keep the production five-second cooldown; verify a successful POST also
	// invalidates the output cache so its old leverage cannot bypass config reads.
	if err := tr.SetLeverage("XUSDT", 7); err != nil {
		t.Fatal(err)
	}
	f.set("config:XUSDT", 200, `[{"symbol":"XUSDT","leverage":7,"marginType":"CROSSED"}]`)
	got, err := tr.GetPositions()
	if err != nil || got[0]["leverage"] != float64(7) {
		t.Fatalf("old leverage reused: %v %v", got, err)
	}
	if f.count("config:XUSDT") != 3 {
		t.Fatal("SetLeverage failed to invalidate configuration")
	}
	if err := tr.SetMarginMode("XUSDT", true); err != nil {
		t.Fatal(err)
	}
	_, err = tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	if f.count("config:XUSDT") != 5 {
		t.Fatalf("SetMarginMode failed to invalidate: requests=%d", f.count("config:XUSDT"))
	}
}

func TestFuturesV3LeverageMismatchCooldown(t *testing.T) {
	hook := fapiErrorHook(t)
	tr, f, clock := newFapiV3Fixture(t)
	f.set("config:XUSDT", 200, `[{"symbol":"XUSDT","leverage":4,"marginType":"CROSSED"}]`)
	got, err := tr.GetPositions()
	if err != nil || got[0]["leverage"] != float64(3) {
		t.Fatalf("unverified leverage returned: %v %v", got, err)
	}
	assertFapiErrorLog(t, hook, "leverage mismatch")
	assertFapiV3Cooldown(t, tr, false, clock.now().Add(6*time.Hour))
	tr.InvalidateAccountCache()
	_, _ = tr.GetPositions()
	if f.count("/fapi/v3/positionRisk") != 1 {
		t.Fatal("retried mismatching leverage source")
	}
	f.set("config:XUSDT", 200, `[{"symbol":"XUSDT","leverage":3,"marginType":"CROSSED"}]`)
	clock.advance(6 * time.Hour)
	tr.InvalidateAccountCache()
	if _, err := tr.GetPositions(); err != nil {
		t.Fatal(err)
	}
	if f.count("/fapi/v3/positionRisk") != 2 || f.count("/fapi/v2/positionRisk") != 3 {
		t.Fatal("leverage mismatch cooldown did not revalidate")
	}
}

func TestFuturesV3Tolerances(t *testing.T) {
	var v2 []*futures.PositionRisk
	var v3 []*futures.PositionRiskV3
	if err := json.Unmarshal([]byte(fapiPositionsV2JSON), &v2); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(fapiPositionsV3JSON()), &v3); err != nil {
		t.Fatal(err)
	}
	v3[0].MarkPrice = "110.5"
	v3[0].UnRealizedProfit = "21"
	if err := reconcilePositions(v2, v3); err != nil {
		t.Fatalf("reasonable price/PnL movement rejected: %v", err)
	}
	v3[0].MarkPrice = "111"
	if err := reconcilePositions(v2, v3); err == nil {
		t.Fatal("mark price beyond 0.5% accepted")
	}
	v3[0].MarkPrice = "110"
	v3[0].UnRealizedProfit = "21"
	if err := reconcilePositions(v2, v3); err == nil {
		t.Fatal("PnL discrepancy hidden by an unused mark-price tolerance")
	}
	v3[0].UnRealizedProfit = "20"
	v3[0].LiquidationPrice = "50.05" // 0.1% cross-margin price movement
	if err := reconcilePositions(v2, v3); err != nil {
		t.Fatalf("0.1%% liquidation price movement rejected: %v", err)
	}
	v3[0].LiquidationPrice = "50.5" // 1% must still fail
	if err := reconcilePositions(v2, v3); err == nil {
		t.Fatal("1% liquidation price movement accepted")
	}
	a := &futures.Account{TotalWalletBalance: "100", AvailableBalance: "80", TotalUnrealizedProfit: "5"}
	b := &futures.AccountV3{TotalWalletBalance: "100.001", AvailableBalance: "80.1", TotalUnrealizedProfit: "5.001"}
	if err := reconcileAccount(a, b); err != nil {
		t.Fatalf("account tolerance rejected: %v", err)
	}
}

func TestFuturesV3AuthErrorsRemainRecognizable(t *testing.T) {
	// WrapAuthError's existing IP diagnostic must also stay inside the fake
	// transport in this test; never contact external IP lookup services.
	previous := http.DefaultTransport
	http.DefaultTransport = batch1Transport(func(r *http.Request) (*http.Response, error) { return batch1Response(r, 200, "127.0.0.1"), nil })
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, account := range []bool{false, true} {
		for _, authVersion := range []string{"v2", "v3"} {
			t.Run(fmt.Sprintf("account=%t/auth=%s", account, authVersion), func(t *testing.T) {
				tr, f, _ := newFapiV3Fixture(t)
				path := "positionRisk"
				if account {
					path = "account"
				}
				f.set("/fapi/v3/"+path, 500, `{"code":-1001,"msg":"offline"}`)
				f.set("/fapi/v2/"+path, 500, `{"code":-1001,"msg":"offline"}`)
				f.set("/fapi/"+authVersion+"/"+path, 401, `{"code":-2015,"msg":"permissions"}`)
				var err error
				if account {
					_, err = tr.GetBalance()
				} else {
					_, err = tr.GetPositions()
				}
				if !IsAuthOrIPError(err) || !errors.Is(err, ErrAuthOrIP) {
					t.Fatalf("v3 auth error lost: %v", err)
				}
			})
		}
	}
}

func TestFuturesV3InitialLeverageFailureDoesNotSkipSourceValidation(t *testing.T) {
	hook := fapiErrorHook(t)
	tr, f, clock := newFapiV3Fixture(t)
	f.set("config:YUSDT", 200, `[]`)
	got, err := tr.GetPositions()
	if err != nil || len(got) != 3 || got[2]["leverage"] != float64(5) {
		t.Fatalf("fallback=%v err=%v", got, err)
	}
	f.set("config:YUSDT", 200, `[{"symbol":"YUSDT","leverage":6,"marginType":"ISOLATED"}]`)
	clock.advance(5 * time.Minute)
	tr.InvalidateAccountCache()
	got, err = tr.GetPositions()
	if err != nil || got[2]["leverage"] != float64(5) {
		t.Fatalf("unvalidated source escaped: %v %v", got, err)
	}
	assertFapiErrorLog(t, hook, "leverage mismatch")
	if f.count("/fapi/v2/positionRisk") != 2 {
		t.Fatal("unfinished reconciliation was skipped")
	}
}

func TestFuturesV3FirstProbeRequestsOverlap(t *testing.T) {
	for _, account := range []bool{false, true} {
		t.Run(fmt.Sprintf("account=%t", account), func(t *testing.T) {
			var mu sync.Mutex
			entered := 0
			both := make(chan struct{})
			tr := fapiTestTrader(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				entered++
				if entered == 2 {
					close(both)
				}
				mu.Unlock()
				select {
				case <-both:
				case <-time.After(time.Second):
					t.Error("v2/v3 probes did not overlap")
				}
				if account {
					_, _ = io.WriteString(w, batch1BalanceJSON)
				} else {
					_, _ = io.WriteString(w, `[]`)
				}
			})
			if account {
				if _, err := tr.GetBalance(); err != nil {
					t.Fatal(err)
				}
			} else {
				if got, err := tr.GetPositions(); err != nil || got != nil {
					t.Fatalf("empty map semantics changed: %v %v", got, err)
				}
			}
		})
	}
}

func TestFuturesV3ConcurrentMissesShareValidation(t *testing.T) {
	tr, f, _ := newFapiV3Fixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if _, err := tr.GetBalance(); err != nil {
					t.Error(err)
				}
			} else {
				if _, err := tr.GetPositions(); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	if f.count("/fapi/v2/positionRisk") != 1 || f.count("/fapi/v3/positionRisk") != 1 || f.count("/fapi/v2/account") != 1 || f.count("/fapi/v3/account") != 1 {
		t.Fatalf("concurrent misses repeated validation: %+v", f.counts)
	}
}
