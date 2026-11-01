package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/market"
	"nofx/market/bnweight"
)

func review20261028Candles(end time.Time, interval string, limit int, base, latest float64) [][]interface{} {
	bar := market.TimeframeDuration(interval)
	lastOpen := end.Truncate(bar)
	rows := make([][]interface{}, limit)
	for i := range rows {
		open := lastOpen.Add(time.Duration(i-limit+1) * bar)
		price, openPrice := base, base
		if i == limit-1 {
			price, openPrice = latest, 110 // forming 4h-boundary scenario
		}
		rows[i] = []interface{}{float64(open.UnixMilli()), fmt.Sprint(openPrice), "112", "99", fmt.Sprint(price), "0", float64(open.Add(bar).Add(-time.Millisecond).UnixMilli())}
	}
	return rows
}

func setupRankingReview20261028(t *testing.T) time.Time {
	t.Helper()
	oldGet, oldNow := binanceGetFn, binanceNow
	now := time.Date(2026, 10, 28, 12, 1, 0, 0, time.UTC)
	binanceNow = func() time.Time { return now }
	tickersMu.Lock()
	oldTickers, oldTickersAt := tickersCache, tickersFetched
	tickersCache = nil
	for i := 0; i < binanceFetchLimit; i++ {
		tickersCache = append(tickersCache, binance24hrTicker{Symbol: fmt.Sprintf("REVIEW%03dUSDT", i), LastPrice: 111, QuoteVolume: 1e9 - float64(i)})
	}
	tickersFetched = time.Now()
	tickersMu.Unlock()
	oiRankMu.Lock()
	oldOI, oldOIKey, oldOIAt := oiRankCache, oiRankCacheKey, oiRankFetchedAt
	oiRankCache, oiRankCacheKey, oiRankFetchedAt = nil, "", time.Time{}
	oiRankMu.Unlock()
	priceRankMu.Lock()
	oldPrice, oldPriceKey, oldPriceAt := priceRankCache, priceRankCacheKey, priceRankFetched
	priceRankCache, priceRankCacheKey, priceRankFetched = nil, "", time.Time{}
	priceRankMu.Unlock()
	t.Cleanup(func() {
		binanceGetFn, binanceNow = oldGet, oldNow
		tickersMu.Lock()
		tickersCache, tickersFetched = oldTickers, oldTickersAt
		tickersMu.Unlock()
		oiRankMu.Lock()
		oiRankCache, oiRankCacheKey, oiRankFetchedAt = oldOI, oldOIKey, oldOIAt
		oiRankMu.Unlock()
		priceRankMu.Lock()
		priceRankCache, priceRankCacheKey, priceRankFetched = oldPrice, oldPriceKey, oldPriceAt
		priceRankMu.Unlock()
	})
	return now
}

func TestReview20261028RankingRolling4hScenarioTwo(t *testing.T) {
	now := setupRankingReview20261028(t)
	binanceGetFn = func(_ context.Context, path string, out interface{}) error {
		u, _ := url.Parse(path)
		q := u.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		_, weight, bucket := bnweight.Classify("GET", u.Path, q)
		if q.Get("interval") != "3m" || limit >= 100 || weight != 1 || bucket != bnweight.BucketIP1M {
			return fmt.Errorf("rolling 4h must keep the weight-1 tier: %s", path)
		}
		body, err := json.Marshal(review20261028Candles(now, "3m", limit, 100, 111))
		if err != nil {
			return err
		}
		return json.Unmarshal(body, out)
	}
	got, price, err := binanceKlineChange(context.Background(), "XUSDT", "4h")
	if err != nil || math.Abs(got-11) > 1e-9 || price != 111 {
		t.Fatalf("rolling 4h=%g price=%g err=%v; want +11%%, not +0.91%%", got, price, err)
	}
}

func TestReview20261028RankingShortHistoryUnknown(t *testing.T) {
	now := setupRankingReview20261028(t)
	binanceGetFn = func(_ context.Context, _ string, out interface{}) error {
		body, _ := json.Marshal(review20261028Candles(now, "3m", 2, 110, 111))
		return json.Unmarshal(body, out)
	}
	if _, _, err := binanceKlineChange(context.Background(), "XUSDT", "4h"); err == nil {
		t.Fatal("short ranking history fabricated a rolling 4h return")
	}
}

func TestReview20261028OIRankingPairsObservationTimes(t *testing.T) {
	now := setupRankingReview20261028(t)
	start, end := now.Truncate(time.Hour).Add(-time.Hour), now.Truncate(time.Hour)
	var priceCalls atomic.Int32
	binanceGetFn = func(_ context.Context, path string, out interface{}) error {
		if strings.HasPrefix(path, "/futures/data/openInterestHist?") {
			return json.Unmarshal([]byte(fmt.Sprintf(`[{"sumOpenInterest":"100","sumOpenInterestValue":"1000000","timestamp":%d},{"sumOpenInterest":"150","sumOpenInterestValue":"1500000","timestamp":%d}]`, start.UnixMilli(), end.UnixMilli())), out)
		}
		u, _ := url.Parse(path)
		q := u.Query()
		if q.Get("endTime") != strconv.FormatInt(end.UnixMilli(), 10) {
			return fmt.Errorf("price window not paired to OI observations: %s", path)
		}
		priceCalls.Add(1)
		limit, _ := strconv.Atoi(q.Get("limit"))
		rows := review20261028Candles(end, q.Get("interval"), limit, 100, 999)
		// The forming close (999) is AFTER the OI end; the last closed
		// observation is 110. Pairing must ignore the forming close.
		rows[len(rows)-2][4] = "110"
		body, _ := json.Marshal(rows)
		return json.Unmarshal(body, out)
	}
	data, err := binanceOIRanking(context.Background(), "1h", 20)
	if err != nil || len(data.TopPositions) != 20 {
		t.Fatalf("ranking=%+v err=%v", data, err)
	}
	if got := data.TopPositions[0].PriceDeltaPercent; math.Abs(got-10) > 1e-9 {
		t.Fatalf("OI-matched price change=%g want 10", got)
	}
	if got := priceCalls.Load(); got != binanceFetchLimit {
		t.Fatalf("alignment used %d price calls, want one per symbol", got)
	}
}

func TestReview20261028RankingCachesRespectRequestsAndCopies(t *testing.T) {
	now := setupRankingReview20261028(t)
	var oiCalls, priceCalls atomic.Int32
	binanceGetFn = func(_ context.Context, path string, out interface{}) error {
		if strings.HasPrefix(path, "/futures/data/openInterestHist?") {
			oiCalls.Add(1)
			return json.Unmarshal([]byte(`[{"sumOpenInterest":"100","sumOpenInterestValue":"1000000"},{"sumOpenInterest":"150","sumOpenInterestValue":"1500000"}]`), out)
		}
		priceCalls.Add(1)
		u, _ := url.Parse(path)
		q := u.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		last := 102.0
		if q.Get("interval") == "3m" {
			last = 111
		}
		body, _ := json.Marshal(review20261028Candles(now, q.Get("interval"), limit, 100, last))
		return json.Unmarshal(body, out)
	}
	ctx := context.Background()
	oi5, err := binanceOIRanking(ctx, " 1H ", 5)
	if err != nil || len(oi5.TopPositions) != 5 || len(oi5.LowPositions) != 5 {
		t.Fatalf("OI top5=%+v err=%v", oi5, err)
	}
	oi5.TopPositions[0].Symbol, oi5.LowPositions[0].Symbol = "CORRUPTED", "CORRUPTED"
	oi20, err := binanceOIRanking(ctx, "1h", 20)
	if err != nil || len(oi20.TopPositions) != 20 || len(oi20.LowPositions) != 20 || oi20.TopPositions[0].Symbol == "CORRUPTED" || oi20.LowPositions[0].Symbol == "CORRUPTED" {
		t.Fatalf("OI top20/copy=%+v err=%v", oi20, err)
	}
	if got := oiCalls.Load(); got != binanceFetchLimit {
		t.Fatalf("deeper OI request refetched full cache: %d calls", got)
	}
	oi20.TopPositions[0].OIDeltaValue = -1
	oiAgain, _ := binanceOIRanking(ctx, "1h", 20)
	if oiAgain.TopPositions[0].OIDeltaValue != 500000 {
		t.Fatal("OI cache-hit slice aliases the cache")
	}

	p1, err := binancePriceRanking(ctx, "1h", 5)
	if err != nil || len(p1.Durations["1h"].Top) != 5 {
		t.Fatalf("price 1h/top5=%+v err=%v", p1, err)
	}
	// The OI enrichment depth=150 must bypass the earlier top5/top20 trims.
	if len(oiRankCache.TopPositions) != binanceFetchLimit || p1.Durations["1h"].Top[0].OIDeltaValue != 500000 {
		t.Fatal("price OI enrichment hit a shallow OI cache")
	}
	p1.Durations["1h"].Top[0].PriceDelta = -999
	p1deep, err := binancePriceRanking(ctx, "1h", 20)
	if err != nil || len(p1deep.Durations["1h"].Top) != 20 || math.Abs(p1deep.Durations["1h"].Top[0].PriceDelta-.02) > 1e-9 {
		t.Fatalf("price 1h/top20=%+v err=%v", p1deep, err)
	}
	p4, err := binancePriceRanking(ctx, "4h", 20)
	if err != nil || len(p4.Durations) != 1 || p4.Durations["4h"] == nil || len(p4.Durations["4h"].Top) != 20 || math.Abs(p4.Durations["4h"].Top[0].PriceDelta-.11) > 1e-9 {
		t.Fatalf("price 4h/top20=%+v err=%v", p4, err)
	}
	p4.Durations["4h"].Top[0].Symbol = "CORRUPTED"
	p4.Durations["4h"].Low[0].Symbol = "CORRUPTED"
	delete(p4.Durations, "4h")
	p4copy, _ := binancePriceRanking(ctx, "4h", 5)
	if len(p4copy.Durations["4h"].Top) != 5 || p4copy.Durations["4h"].Top[0].Symbol == "CORRUPTED" || p4copy.Durations["4h"].Low[0].Symbol == "CORRUPTED" {
		t.Fatal("price result aliases cached map/durations/slices")
	}

	if _, err := binancePriceRanking(ctx, "4h,1h", 5); err != nil {
		t.Fatal(err)
	}
	before := priceCalls.Load()
	normalized, err := binancePriceRanking(ctx, "\t1H, 4h,1h\n", 20)
	if err != nil || len(normalized.Durations) != 2 || len(normalized.Durations["4h"].Top) != 20 || priceCalls.Load() != before {
		t.Fatalf("normalized cache key missed: %+v err=%v", normalized, err)
	}
	// Concurrent readers may mutate their own results without racing either
	// the stored ranking or another request's enrichment/copy.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := binancePriceRanking(ctx, "1h,4h", 5)
			if err != nil {
				t.Error(err)
				return
			}
			d.Durations["1h"].Top[0].OIDeltaValue = -1
			d.Durations["4h"].Low[0].Symbol = "LOCAL"
		}()
	}
	wg.Wait()
	if got := priceRankCache.Durations["1h"].Top[0].OIDeltaValue; got != 500000 {
		t.Fatalf("concurrent caller mutated cache: %g", got)
	}
}
