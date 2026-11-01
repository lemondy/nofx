package paper

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type readLatencyMarket struct {
	source                    PriceSource
	delay                     time.Duration
	priceErrors, candleErrors map[string]error
	mu                        sync.Mutex
	calls, inFlight, maxCalls int
}

func (f *readLatencyMarket) begin() {
	f.mu.Lock()
	f.calls++
	f.inFlight++
	if f.inFlight > f.maxCalls {
		f.maxCalls = f.inFlight
	}
	f.mu.Unlock()
	time.Sleep(f.delay)
}

func (f *readLatencyMarket) end() {
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}

func (f *readLatencyMarket) stats() (calls, maxInFlight int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.maxCalls
}

func (f *readLatencyMarket) Price(symbol string) (float64, error) {
	f.begin()
	defer f.end()
	if err := f.priceErrors[symbol]; err != nil {
		return 0, err
	}
	return f.source.Price(symbol)
}

func (f *readLatencyMarket) Candles1m(symbol string, since time.Time) ([]Candle, error) {
	f.begin()
	defer f.end()
	if err := f.candleErrors[symbol]; err != nil {
		return nil, err
	}
	return f.source.Candles1m(symbol, since)
}

func TestAdvanceFetchesObservationsConcurrently(t *testing.T) {
	const symbols = 12
	const delay = 50 * time.Millisecond
	p, _, f := setup(t, Config{})
	for i := 0; i < symbols; i++ {
		_, err := p.OpenLong(fmt.Sprintf("SYM%02dUSDT", i), 1, 10)
		must(t, err)
	}
	market := &readLatencyMarket{source: f, delay: delay}
	p.price = market
	f.step(time.Second, 100)
	start := time.Now()
	must(t, p.Advance(f.now()))
	elapsed := time.Since(start)
	if limit := symbols * 2 * delay / 2; elapsed >= limit {
		t.Fatalf("Advance took %s, want less than %s (sequential: %s)", elapsed, limit, symbols*2*delay)
	}
	calls, maxInFlight := market.stats()
	if calls != symbols*2 {
		t.Fatalf("market calls = %d, want %d", calls, symbols*2)
	}
	if maxInFlight <= 1 || maxInFlight > 6 {
		t.Fatalf("max in-flight market calls = %d, want 2..6", maxInFlight)
	}
	t.Logf("Advance: %s for %d calls; max in-flight: %d", elapsed, calls, maxInFlight)
}

func TestAdvanceObservationErrorsAreDeterministic(t *testing.T) {
	for _, firstFailure := range []string{"price", "candles"} {
		t.Run(firstFailure, func(t *testing.T) {
			p, _, f := setup(t, Config{})
			for _, symbol := range []string{"ZZZUSDT", "AAAUSDT"} {
				_, err := p.OpenLong(symbol, 1, 10)
				must(t, err)
			}
			want := errors.New("AAAUSDT " + firstFailure + " unavailable")
			market := &readLatencyMarket{
				source:       f,
				delay:        time.Millisecond,
				priceErrors:  map[string]error{"ZZZUSDT": errors.New("ZZZUSDT price unavailable")},
				candleErrors: map[string]error{},
			}
			if firstFailure == "price" {
				market.priceErrors["AAAUSDT"] = want
			} else {
				// The lexicographically first symbol fails after its second
				// call, even though ZZZUSDT fails on its first call.
				market.candleErrors["AAAUSDT"] = want
			}
			p.price = market
			since := f.now()
			f.step(time.Second, 100)
			for i := 0; i < 10; i++ {
				if err := p.Advance(f.now()); err != want {
					t.Fatalf("attempt %d: got %v, want original error %v", i, err, want)
				}
			}
			if !p.s.LastAdvance.Equal(since) {
				t.Fatal("failed observations advanced the cursor")
			}
		})
	}
}

func TestReadsReuseFreshAdvance(t *testing.T) {
	p, _, f := setup(t, Config{})
	_, err := p.OpenLong("BTCUSDT", 1, 10)
	must(t, err)
	market := &readLatencyMarket{source: f}
	p.price = market
	f.step(time.Second, 100)
	must(t, p.Advance(f.now()))
	if calls, _ := market.stats(); calls != 2 {
		t.Fatalf("initial Advance made %d market calls, want 2", calls)
	}

	f.step(time.Second, 110)
	_, err = p.GetBalance()
	must(t, err)
	f.step(readFreshWindow-2*time.Second, 115)
	positions, err := p.GetPositions()
	must(t, err)
	near(t, positions[0]["markPrice"].(float64), 100)
	if calls, _ := market.stats(); calls != 2 {
		t.Fatalf("two fresh reads made additional market calls: total = %d, want 2", calls)
	}

	// The strict freshness comparison expires at exactly 20 seconds.
	f.step(time.Second, 120)
	_, err = p.GetBalance()
	must(t, err)
	if calls, _ := market.stats(); calls != 4 {
		t.Fatalf("read at freshness boundary made %d total market calls, want 4", calls)
	}
	positions, err = p.GetPositions()
	must(t, err)
	near(t, positions[0]["markPrice"].(float64), 120)
	if calls, _ := market.stats(); calls != 4 {
		t.Fatalf("read after refresh made additional market calls: total = %d, want 4", calls)
	}

	f.step(readFreshWindow+time.Nanosecond, 130)
	positions, err = p.GetPositions()
	must(t, err)
	near(t, positions[0]["markPrice"].(float64), 130)
	if calls, _ := market.stats(); calls != 6 {
		t.Fatalf("read past freshness window made %d total market calls, want 6", calls)
	}
}
