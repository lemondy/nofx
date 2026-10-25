package trader

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exit-path classification truth table (2026-09-29 close_reason repair:
// 251/254 closed rows were indistinguishable 'sync').
func TestClassifyExitReason(t *testing.T) {
	cases := []struct {
		name   string
		side   string
		exit   float64
		entry  float64
		sl     float64
		tp     float64
		runner bool
		intent string
		want   string
	}{
		{"loss-side stop long", "long", 96.2, 100, 96.0, 110, false, "", "stop_loss"},
		{"loss-side stop short", "short", 103.8, 100, 104.0, 90, false, "", "stop_loss"},
		{"trailing stop long (SL past entry)", "long", 108.9, 100, 109.0, 120, true, "", "trailing_stop"},
		{"breakeven stop long (SL at entry)", "long", 100.1, 100, 100.0, 120, false, "", "trailing_stop"},
		{"structure TP", "long", 119.9, 100, 96.0, 120, false, "", "take_profit"},
		{"runner exit near no static level", "long", 128.4, 100, 105.0, 120, true, "", "trend_runner"},
		{"program close by intent", "long", 105.5, 100, 96.0, 120, false, "drawdown_protect", "drawdown_protect"},
		{"manual close on exchange", "long", 102.3, 100, 96.0, 120, false, "", "external"},
		{"price evidence outranks stale intent", "long", 96.1, 100, 96.0, 120, false, "tp_trim", "stop_loss"},
		{"no exit price → stays sync", "long", 0, 100, 96, 120, false, "", "sync"},
	}
	for _, tc := range cases {
		if got := classifyExitReason(tc.side, tc.exit, tc.entry, tc.sl, tc.tp, tc.runner, tc.intent); got != tc.want {
			t.Errorf("%s: classifyExitReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// MAE/MFE replay over synthetic 1m bars, side-aware, R conversion vs the
// write-once opening stop.
func TestComputeExitExcursions(t *testing.T) {
	bars := []market.Kline{
		{High: 101, Low: 99},
		{High: 104, Low: 100}, // favorable extreme 104
		{High: 102, Low: 97},  // adverse extreme 97
		{High: 103, Low: 101},
	}
	mae, mfe := computeExitExcursions(bars, "long", 100)
	if math.Abs(mae-3) > 1e-9 || math.Abs(mfe-4) > 1e-9 {
		t.Fatalf("long: mae %.2f mfe %.2f, want 3 / 4", mae, mfe)
	}
	mae, mfe = computeExitExcursions(bars, "short", 100)
	if math.Abs(mae-4) > 1e-9 || math.Abs(mfe-3) > 1e-9 {
		t.Fatalf("short: mae %.2f mfe %.2f, want 4 / 3 (mirrored)", mae, mfe)
	}
	// R conversion: opening stop 2% away → 3% adverse = 1.5R.
	if r := excursionR(3, 100, 98); math.Abs(r-1.5) > 1e-9 {
		t.Fatalf("excursionR = %.3f, want 1.5", r)
	}
	if r := excursionR(3, 100, 0); r != 0 {
		t.Fatalf("no anchor → 0R, got %.3f", r)
	}
	if mae, mfe := computeExitExcursions(nil, "long", 100); mae != 0 || mfe != 0 {
		t.Fatalf("no bars → zeros, got %.2f/%.2f", mae, mfe)
	}
}

// Close-intent registry: consume pops once, TTL drops stale intents.
func TestCloseIntentRegistry(t *testing.T) {
	at := &AutoTrader{closeIntents: make(map[string]closeIntent)}
	at.markCloseIntent("BTCUSDT", "long", "tp_trim")
	if got := at.consumeCloseIntent("btcusdt", "LONG"); got != "tp_trim" {
		t.Fatalf("first consume = %q, want tp_trim (key normalization)", got)
	}
	if got := at.consumeCloseIntent("BTCUSDT", "long"); got != "" {
		t.Fatalf("second consume = %q, want \"\" (popped)", got)
	}
	// Stale intent expires.
	at.closeIntents[keyOf("ETHUSDT", "short")] = closeIntent{reason: "ai_close", at: time.Now().Add(-2 * closeIntentTTL)}
	if got := at.consumeCloseIntent("ETHUSDT", "short"); got != "" {
		t.Fatalf("stale intent = %q, want \"\"", got)
	}
}

func newExitReplayTestTrader(t *testing.T) *AutoTrader {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "exit-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &AutoTrader{id: t.Name(), name: t.Name(), exchange: "binance", store: st}
}

func addExitReplayTestPosition(t *testing.T, at *AutoTrader, held time.Duration, exit time.Time) store.TraderPosition {
	t.Helper()
	row := store.TraderPosition{
		TraderID: at.id, Symbol: "BTCUSDT", Side: "LONG", Status: "CLOSED",
		EntryPrice: 100, InitialStopLoss: 98, Quantity: 1,
		EntryTime: exit.Add(-held).UnixMilli(), ExitTime: exit.UnixMilli(),
	}
	if err := at.store.GormDB().Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func exitReplayTestValues(t *testing.T, at *AutoTrader, id int64) [4]float64 {
	t.Helper()
	var row store.TraderPosition
	if err := at.store.GormDB().First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	return [4]float64{row.MAEPct, row.MFEPct, row.MAER, row.MFER}
}

func stubExitReplayKlines(t *testing.T, fn func(string, string, time.Time, time.Time) ([]market.Kline, error)) {
	t.Helper()
	previous := klinesRangeFn
	klinesRangeFn = fn
	t.Cleanup(func() { klinesRangeFn = previous })
}

func captureExitReplayLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := logger.Log.Out
	logger.Log.SetOutput(&logs)
	t.Cleanup(func() { logger.Log.SetOutput(previous) })
	return &logs
}

func TestBackfillExitExcursionsUnreplayableDuration(t *testing.T) {
	for _, held := range []time.Duration{time.Millisecond, 4800 * time.Millisecond, time.Minute - time.Millisecond, 31 * 24 * time.Hour} {
		t.Run(held.String(), func(t *testing.T) {
			at := newExitReplayTestTrader(t)
			row := addExitReplayTestPosition(t, at, held, time.Now())
			logs := captureExitReplayLogs(t)
			stubExitReplayKlines(t, func(string, string, time.Time, time.Time) ([]market.Kline, error) {
				t.Fatal("unreplayable duration must not request klines")
				return nil, nil
			})
			at.backfillExitExcursions()
			at.backfillExitExcursions()
			if got := exitReplayTestValues(t, at, row.ID); got != [4]float64{-1, -1, -1, -1} {
				t.Fatalf("excursions = %v, want four sentinels", got)
			}
			if held < time.Minute && strings.Count(logs.String(), "持仓不足 1 分钟，1m K 线无法回放") != 1 {
				t.Fatalf("expected one sub-minute explanation, got %s", logs.String())
			}
		})
	}
}

func TestBackfillExitExcursionsInvalidSymbol(t *testing.T) {
	for _, exchange := range []string{"binance", "paper"} {
		t.Run(exchange, func(t *testing.T) {
			at := newExitReplayTestTrader(t)
			at.exchange = exchange
			row := addExitReplayTestPosition(t, at, time.Minute, time.Now())
			calls := 0
			logs := captureExitReplayLogs(t)
			stubExitReplayKlines(t, func(string, string, time.Time, time.Time) ([]market.Kline, error) {
				calls++
				return nil, fmt.Errorf("historical klines: %w", market.ErrInvalidSymbol)
			})
			at.backfillExitExcursions()
			at.backfillExitExcursions()
			if calls != 1 || len(at.exitReplayRetries) != 0 {
				t.Fatalf("permanent error retried: calls=%d retries=%v", calls, at.exitReplayRetries)
			}
			if got := exitReplayTestValues(t, at, row.ID); got != [4]float64{-1, -1, -1, -1} {
				t.Fatalf("excursions = %v, want four sentinels", got)
			}
			if strings.Count(logs.String(), "Binance 无效符号 (-1121)") != 1 {
				t.Fatalf("expected one invalid-symbol explanation, got %s", logs.String())
			}
		})
	}
}

func TestBackfillExitExcursionsRetryBackoff(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"empty klines", nil},
		{"other 400", errors.New(`binance klines api returned status 400: {"code":-1100,"msg":"Illegal characters."}`)},
		{"transient error", errors.New("upstream timeout")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := newExitReplayTestTrader(t)
			row := addExitReplayTestPosition(t, at, time.Minute, time.Now())
			logs := captureExitReplayLogs(t)
			calls := 0
			stubExitReplayKlines(t, func(string, string, time.Time, time.Time) ([]market.Kline, error) {
				calls++
				return nil, tc.err
			})
			wantBackoffs := []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour, 12 * time.Hour, 12 * time.Hour, 12 * time.Hour}
			for failure := 1; failure <= 8; failure++ {
				before := time.Now()
				at.backfillExitExcursions()
				after := time.Now()
				if calls != failure {
					t.Fatalf("failure %d: calls=%d", failure, calls)
				}
				if failure == 8 {
					if got := exitReplayTestValues(t, at, row.ID); got != [4]float64{-1, -1, -1, -1} {
						t.Fatalf("retry exhaustion excursions = %v, want sentinels", got)
					}
					if len(at.exitReplayRetries) != 0 {
						t.Fatalf("exhausted row retained after sentinel write: %v", at.exitReplayRetries)
					}
				} else {
					if got := exitReplayTestValues(t, at, row.ID); got != [4]float64{} {
						t.Fatalf("failure %d prematurely marked permanent: %v", failure, got)
					}
					at.exitReplayMu.Lock()
					retry := at.exitReplayRetries[row.ID]
					at.exitReplayMu.Unlock()
					want := wantBackoffs[failure-1]
					if retry.failures != failure || retry.nextAttempt.Before(before.Add(want)) || retry.nextAttempt.After(after.Add(want)) {
						t.Fatalf("failure %d: retry=%+v, want backoff %v", failure, retry, want)
					}
				}
				logCount := logs.Len()
				at.backfillExitExcursions()
				if calls != failure || logs.Len() != logCount {
					t.Fatalf("waiting/finished row requested or logged again at failure %d", failure)
				}
				if failure < 8 {
					// Advance only this row's retry deadline; no wall-clock sleeps.
					at.exitReplayMu.Lock()
					retry := at.exitReplayRetries[row.ID]
					retry.nextAttempt = time.Now().Add(-time.Second)
					at.exitReplayRetries[row.ID] = retry
					at.exitReplayMu.Unlock()
				}
			}
			if strings.Count(logs.String(), "replay deferred") != 5 || strings.Count(logs.String(), "replay abandoned") != 1 || strings.Count(logs.String(), "[WARN]") != 1 {
				t.Fatalf("expected five backoff logs and one exhaustion warning, got %s", logs.String())
			}
			for failure := 1; failure <= 5; failure++ {
				if !strings.Contains(logs.String(), fmt.Sprintf("第 %d 次失败，下次最早 ", failure)) {
					t.Fatalf("missing attempt/deadline in backoff log %d", failure)
				}
			}
		})
	}
}

func TestBackfillExitExcursionsQueueSkipsBackoff(t *testing.T) {
	at := newExitReplayTestTrader(t)
	at.exitReplayRetries = make(map[int64]exitReplayRetry)
	now := time.Now()
	var rows []store.TraderPosition
	for i := 0; i < 7; i++ {
		row := addExitReplayTestPosition(t, at, 2*time.Minute, now.Add(-time.Duration(i)*time.Minute))
		rows = append(rows, row)
		if i < 3 {
			at.exitReplayRetries[row.ID] = exitReplayRetry{failures: 1, nextAttempt: now.Add(time.Hour)}
		}
	}
	var requested []int64
	stubExitReplayKlines(t, func(_ string, _ string, start, _ time.Time) ([]market.Kline, error) {
		requested = append(requested, start.UnixMilli())
		return []market.Kline{{High: 104, Low: 97}}, nil
	})
	at.backfillExitExcursions()
	if len(requested) != exitBackfillPerCycle {
		t.Fatalf("requests=%v, want exactly %d", requested, exitBackfillPerCycle)
	}
	for i, start := range requested {
		if start != rows[i+3].EntryTime {
			t.Fatalf("request %d entry=%d, want older row #%d", i, start, rows[i+3].ID)
		}
	}
	for i, row := range rows {
		want := [4]float64{}
		if i >= 3 && i < 6 {
			want = [4]float64{3, 4, 1.5, 2}
		}
		if got := exitReplayTestValues(t, at, row.ID); got != want {
			t.Fatalf("row %d excursions=%v, want %v", i, got, want)
		}
	}
	at.backfillExitExcursions()
	if len(requested) != 4 || requested[3] != rows[6].EntryTime {
		t.Fatalf("next cycle did not reach last row: %v", requested)
	}
}

func TestBackfillExitExcursionsSuccessClearsRetry(t *testing.T) {
	at := newExitReplayTestTrader(t)
	row := addExitReplayTestPosition(t, at, 2*time.Minute, time.Now())
	stubExitReplayKlines(t, func(symbol, timeframe string, start, end time.Time) ([]market.Kline, error) {
		if symbol != row.Symbol || timeframe != "1m" || start.UnixMilli() != row.EntryTime || end.UnixMilli() != row.ExitTime {
			t.Fatalf("unexpected replay request: %s %s %s %s", symbol, timeframe, start, end)
		}
		return []market.Kline{{High: 104, Low: 97}}, nil
	})
	at.exitReplayRetries = map[int64]exitReplayRetry{row.ID: {failures: 3, nextAttempt: time.Now().Add(-time.Second)}}
	at.backfillExitExcursions()
	if got := exitReplayTestValues(t, at, row.ID); got != [4]float64{3, 4, 1.5, 2} {
		t.Fatalf("successful replay=%v, want [3 4 1.5 2]", got)
	}
	if len(at.exitReplayRetries) != 0 {
		t.Fatalf("successful replay retained retry state: %v", at.exitReplayRetries)
	}
}

func TestBackfillExitExcursionsSkipsOtherExchanges(t *testing.T) {
	stubExitReplayKlines(t, func(string, string, time.Time, time.Time) ([]market.Kline, error) {
		t.Fatal("non-Binance exchange must not request klines")
		return nil, nil
	})
	// A nil store also proves this returns before touching the work queue.
	for _, exchange := range []string{"bybit", "hyperliquid", "binance_stocks"} {
		at := &AutoTrader{exchange: exchange}
		at.backfillExitExcursions()
	}
}

func TestBackfillExitExcursionsConcurrentRetry(t *testing.T) {
	at := newExitReplayTestTrader(t)
	row := addExitReplayTestPosition(t, at, time.Minute, time.Now())
	calls := 0
	stubExitReplayKlines(t, func(string, string, time.Time, time.Time) ([]market.Kline, error) {
		calls++
		return nil, errors.New("upstream timeout")
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(at.backfillExitExcursions)
	}
	wg.Wait()
	if calls != 1 || at.exitReplayRetries[row.ID].failures != 1 {
		t.Fatalf("concurrent passes duplicated a retry: calls=%d state=%+v", calls, at.exitReplayRetries[row.ID])
	}
}
