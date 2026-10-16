package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/provider/openbb"
	"nofx/store"
)

// stubLiveMarket makes BuildUserPrompt hermetic. The BTC 4h regime is served
// from btc4h through binanceGetFn; every other Binance path fails (the same
// fail-open a vendor outage gets), and the sentiment / CFTC blocks render
// absent. The Binance caches are cleared before AND after, so neither a warm
// live cache leaks in nor the canned closes leak out to later tests.
//
// Without this, a fixture that is long-only (short side already blocked)
// flips to double-blocked whenever the LIVE BTC 4h trend is down
// (BTC_4H_DOWNTREND) — correct production behavior, but a flaky test.
func stubLiveMarket(t *testing.T, btc4h []float64) {
	t.Helper()
	resetCaches := func() {
		btcClosesMu.Lock()
		btc4hClosesCache, btc4hClosesFetched, btc4hLastAttempt, btc4hInflight = nil, time.Time{}, time.Time{}, nil
		btcClosesCache, btcClosesFetched = nil, time.Time{}
		btcClosesMu.Unlock()
		tickersMu.Lock()
		tickersCache, tickersFetched = nil, time.Time{}
		tickersMu.Unlock()
	}
	oldGet, oldSentiment, oldCot := binanceGetFn, marketSentimentFn, cftcCotBitcoinFn
	t.Cleanup(func() {
		binanceGetFn, marketSentimentFn, cftcCotBitcoinFn = oldGet, oldSentiment, oldCot
		resetCaches()
	})
	resetCaches()

	// Closed bars plus one forming bar, which binanceBTC4hCloses drops.
	candles := make([][]interface{}, 0, len(btc4h)+1)
	for _, c := range append(append([]float64(nil), btc4h...), btc4h[len(btc4h)-1]) {
		s := strconv.FormatFloat(c, 'f', -1, 64)
		candles = append(candles, []interface{}{0, s, s, s, s, "0"})
	}
	body, err := json.Marshal(candles)
	if err != nil {
		t.Fatal(err)
	}
	binanceGetFn = func(_ context.Context, path string, out interface{}) error {
		if strings.Contains(path, "/fapi/v1/klines?symbol=BTCUSDT&interval=4h") {
			return json.Unmarshal(body, out)
		}
		return errors.New("stubLiveMarket: no network in tests")
	}
	marketSentimentFn = func() *market.MarketSentiment { return nil }
	cftcCotBitcoinFn = func() *openbb.CotBitcoin { return nil }
}

// neutralBTC4h: flat closes → EMA20 == EMA50 → breakout.BTC4hRegime "chop"
// (known, neither downtrend nor strong bull) — no BTC code on either side.
func neutralBTC4h() []float64 {
	out := make([]float64, 300)
	for i := range out {
		out[i] = 60000
	}
	return out
}

// downtrendBTC4h: steadily falling closes → EMA20 < EMA50 with price below
// EMA20 → "btc_bear", the live shape that broke the fixtures.
func downtrendBTC4h() []float64 {
	out := make([]float64, 300)
	p := 80000.0
	for i := range out {
		out[i] = p
		p *= 0.997
	}
	return out
}

// okCandidateCtx is the long-only OKUSDT fixture shared by the prompt tests:
// rising bars, short side blocked by CONSENSUS_OPPOSED.
func okCandidateCtx() *Context {
	now := time.Now()
	return &Context{
		CandidateCoins: []CandidateCoin{{Symbol: "OKUSDT", Sources: []string{"ai500"}}},
		MarketDataMap: map[string]*market.Data{
			"OKUSDT": withVendor(&market.Data{Symbol: "OKUSDT", CurrentPrice: 1.0,
				TimeframeData: map[string]*market.TimeframeSeriesData{
					"15m": buildTF("15m", now, 80, 1.0, false),
					"1h":  buildTF("1h", now, 80, 1.0, false),
					"4h":  buildTF("4h", now, 80, 1.0, false),
				}}, 0.05),
		},
	}
}

// Pins the production behavior the flaky tests tripped over: a BTC 4h
// downtrend adds BTC_4H_DOWNTREND to the long side, so a long-only candidate
// becomes double-blocked and compresses to the one-row table.
func TestBTC4hDowntrendCompressesLongOnlyCandidate(t *testing.T) {
	stubLiveMarket(t, downtrendBTC4h())
	engine := NewStrategyEngine(&store.StrategyConfig{})
	ctx := okCandidateCtx()

	prompt := engine.BuildUserPrompt(ctx)
	if !strings.Contains(prompt, "双向硬门拦截") || !strings.Contains(prompt, "BTC_4H_DOWNTREND") {
		t.Fatalf("BTC downtrend should double-block and compress the long-only candidate:\n%s", prompt)
	}
	if strings.Contains(prompt, `"hard_entry_gate"`) {
		t.Fatalf("full JSON still rendered for a double-blocked candidate:\n%s", prompt)
	}
	gs := ctx.GateStates[market.Normalize("OKUSDT")]
	if gs == nil || gs.LongAllowed || gs.ShortAllowed {
		t.Fatalf("GateStates should be double-blocked, got %+v", gs)
	}

	// Same fixture under a neutral BTC regime keeps its allowed long side —
	// the downtrend alone is what flips it.
	stubLiveMarket(t, neutralBTC4h())
	ctx = okCandidateCtx()
	if prompt := engine.BuildUserPrompt(ctx); !strings.Contains(prompt, `"hard_entry_gate"`) {
		t.Fatalf("neutral BTC regime should keep the full JSON:\n%s", prompt)
	}
	if gs := ctx.GateStates[market.Normalize("OKUSDT")]; gs == nil || !gs.LongAllowed {
		t.Fatalf("long side should be allowed under a neutral BTC regime, got %+v", gs)
	}
}
