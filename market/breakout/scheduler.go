package breakout

import (
	"sort"
	"sync"
	"time"

	"nofx/logger"
)

// Scheduler keeps a fresh top-N breakout/breakdown snapshot computed every
// 5 minutes from the top-volume Binance perps. Both the Data page and the
// "piggy dash" (猪猪冲刺) coin source read from this snapshot.
type Scheduler struct {
	mu             sync.RWMutex
	snapshot       []ScanResult
	updatedAt      time.Time
	shortSnapshot  []ShortSignal
	shortUpdatedAt time.Time
	running        bool
	scanning       bool // a runOnce is in flight (background ticker or RefreshNow)
	stop           chan struct{}
	once           sync.Once

	// knobs (overridable in tests)
	Symbols    int // how many top-volume symbols to analyze
	Concurrent int
	Interval   time.Duration
	// Second-universe board sizes (user 2026-09-12): 热门/涨幅/跌幅榜 each
	// contribute up to N symbols; the union minus the primary volume top
	// joins the scan set and the SCORE picks the final top — early
	// breakouts on not-yet-top-volume symbols stop being invisible.
	BoardHot  int
	BoardGain int
	BoardLose int
}

var defaultScheduler = &Scheduler{
	Symbols:    20,
	Concurrent: 4,
	Interval:   5 * time.Minute,
}

// DefaultScheduler returns the process-wide scheduler.
func DefaultScheduler() *Scheduler { return defaultScheduler }

// Start launches the background loop (first run immediately, then every Interval).
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stop = make(chan struct{})
	s.mu.Unlock()

	go func() {
		logger.Infof("🐷 Breakout scheduler started (interval: %v, symbols: %d)", s.Interval, s.Symbols)
		s.mu.Lock()
		s.scanning = true
		s.mu.Unlock()
		s.runOnce(nil)

		// Tuning loop: first pass shortly after the first snapshot, then weekly —
		// 72h let the sigmoids overfit one regime's tail.
		go func() {
			time.Sleep(30 * time.Second)
			s.runTuning()
			tuneTicker := time.NewTicker(168 * time.Hour)
			defer tuneTicker.Stop()
			for {
				select {
				case <-tuneTicker.C:
					s.runTuning()
				case <-s.stop:
					return
				}
			}
		}()

		// Slow-top short universe: a separate, cheaper cadence (30 min) —
		// its prefilter costs one 1d-klines call per liquid perp. The same
		// loop samples short signals hourly and runs the weight tuner daily.
		go func() {
			refreshSlowTops()
			slowTicker := time.NewTicker(30 * time.Minute)
			sampleTicker := time.NewTicker(time.Hour)
			tuneTicker := time.NewTicker(24 * time.Hour)
			defer slowTicker.Stop()
			defer sampleTicker.Stop()
			defer tuneTicker.Stop()
			for {
				select {
				case <-slowTicker.C:
					refreshSlowTops()
				case <-sampleTicker.C:
					shorts, at := s.ShortSnapshot()
					if len(shorts) > 0 {
						SampleShortSignals(shorts, at)
					}
				case <-tuneTicker.C:
					RunShortTuner(time.Now())
				case <-s.stop:
					return
				}
			}
		}()

		ticker := time.NewTicker(s.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.mu.Lock()
				// Skip if a RefreshNow scan is still in flight — its result is
				// fresh enough and double-scanning just burns rate limits.
				if s.scanning {
					s.mu.Unlock()
					continue
				}
				s.scanning = true
				s.mu.Unlock()
				s.runOnce(nil)
			case <-s.stop:
				return
			}
		}
	}()
}

// runTuning executes a backtest pass and micro-adjusts parameters.
// Recovers from panics: tuning is best-effort and must never take down trading.
func (s *Scheduler) runTuning() {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("🚨 Breakout tuning panicked (recovered): %v", r)
		}
	}()
	// 15 symbols: the old top-10 (majors only) under-covered the high-ATR
	// alts the candidate pool actually trades.
	symbols, err := TopVolumeSymbols(15)
	if err != nil {
		logger.Warnf("⚠️ Breakout tuning: failed to list symbols: %v", err)
		return
	}
	hasBTC := false
	for _, sym := range symbols {
		if sym == "BTCUSDT" {
			hasBTC = true
			break
		}
	}
	if !hasBTC && len(symbols) > 0 {
		symbols[len(symbols)-1] = "BTCUSDT"
	}
	if _, err := TuneFromBacktest(symbols); err != nil {
		logger.Warnf("⚠️ Breakout tuning failed: %v", err)
	}
}

// Stop terminates the loop.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		close(s.stop)
		s.running = false
	}
}

// runOnce refreshes the snapshot. onDone (when non-nil) fires after the
// snapshot is stored — used by RefreshNow to unblock a cold-start waiter.
func (s *Scheduler) runOnce(onDone func()) {
	start := time.Now()
	symbols, err := TopVolumeSymbols(s.Symbols)
	if err != nil {
		logger.Warnf("⚠️ Breakout scheduler: failed to list symbols: %v", err)
		s.mu.Lock()
		s.scanning = false
		s.mu.Unlock()
		if onDone != nil {
			onDone()
		}
		return
	}
	// Second universe: 热门/涨幅/跌幅榜 union minus the primary top-volume
	// set. The final top-20 is whatever the breakout score ranks — board
	// membership only buys a symbol into the SCAN, not into the pool.
	seen := map[string]bool{}
	for _, sym := range symbols {
		seen[sym] = true
	}
	hotN, gainN, loseN := s.BoardHot, s.BoardGain, s.BoardLose
	if hotN <= 0 {
		hotN, gainN, loseN = 20, 20, 20
	}
	if hot, gain, lose, berr := BoardLists(hotN, gainN, loseN); berr == nil {
		added := 0
		for _, sym := range append(append(hot, gain...), lose...) {
			if !seen[sym] {
				seen[sym] = true
				symbols = append(symbols, sym)
				added++
			}
		}
		logger.Infof("🐷 Second universe: +%d board symbols (hot %d, gain %d, lose %d), scan set %d",
			added, len(hot), len(gain), len(lose), len(symbols))
	} else {
		logger.Warnf("⚠️ Second universe unavailable (%v) — primary-only scan", berr)
	}
	// BTC drives altcoin breakout quality — make sure it is always scanned.
	hasBTC := false
	for _, sym := range symbols {
		if sym == "BTCUSDT" {
			hasBTC = true
			break
		}
	}
	if !hasBTC {
		symbols = append(symbols, "BTCUSDT")
	}

	results := AnalyzeMany(symbols, s.Concurrent)

	// BTC market regime: alt breakouts against the BTC direction fail more.
	regime := "chop"
	for _, r := range results {
		if r.Symbol != "BTCUSDT" {
			continue
		}
		if r.Direction == DirUp && r.Score >= GetParams().MediumThreshold {
			regime = "btc_bull"
		} else if r.Direction == DirDown && r.Score >= GetParams().MediumThreshold {
			regime = "btc_bear"
		}
	}

	// Cross-sectional percentile + regime factor.
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	n := len(results)
	for i := range results {
		results[i].Percentile = round2(float64(n-i) / float64(n) * 100)
		if results[i].Symbol == "BTCUSDT" {
			continue
		}
		switch {
		case regime == "btc_bull" && results[i].Direction == DirDown,
			regime == "btc_bear" && results[i].Direction == DirUp:
			results[i].Score = round2(results[i].Score * 0.85)
			results[i].Regime = regime
		case regime == "chop":
			results[i].Score = round2(results[i].Score * 0.95)
			results[i].Regime = regime
		default:
			results[i].Regime = regime
		}
	}

	// Re-sort after adjustment.
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })

	s.mu.Lock()
	s.snapshot = results
	s.updatedAt = time.Now()
	s.mu.Unlock()
	logger.Infof("🐷 Breakout snapshot updated: %d symbols in %v (regime: %s)", len(results), time.Since(start).Round(time.Millisecond), regime)

	// Short scan rides the same cadence: rank the top 24h gainers by
	// short-suitability. Best-effort — a short-scan failure never touches
	// the main breakout snapshot.
	if shorts, shortAt, err := ScanShorts(ShortScanUniverse); err != nil {
		logger.Warnf("⚠️ Short scan failed: %v", err)
	} else {
		s.mu.Lock()
		s.shortSnapshot = shorts
		s.shortUpdatedAt = shortAt
		s.mu.Unlock()
		logger.Infof("🩸 Short scan updated: %d candidates in %v", len(shorts), time.Since(start).Round(time.Millisecond))
	}

	s.mu.Lock()
	s.scanning = false
	s.mu.Unlock()
	if onDone != nil {
		onDone()
	}
}

// Snapshot returns the current top list and its timestamp (may be empty before
// the first run completes).
func (s *Scheduler) Snapshot() ([]ScanResult, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ScanResult, len(s.snapshot))
	copy(out, s.snapshot)
	return out, s.updatedAt
}

// ShortSnapshot returns the cached short-scan ranking (may be empty before the
// first background scan completes; ScanShorts can compute one on demand).
func (s *Scheduler) ShortSnapshot() ([]ShortSignal, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ShortSignal, len(s.shortSnapshot))
	copy(out, s.shortSnapshot)
	return out, s.shortUpdatedAt
}

// TopSymbols returns up to limit symbols from the snapshot, strongest first,
// optionally filtered by direction ("breakout" / "breakdown" / "" for both).
// SymbolDirection pairs a snapshot symbol with its selected direction.
type SymbolDirection struct {
	Symbol    string
	Direction string // "up" | "down"
}

// TopSymbolsWithDirection is TopSymbols plus each symbol's selected
// direction — the candidate pool needs it to detect cross-scanner conflicts
// (short_scan "short" vs piggy_dash "up").
func (s *Scheduler) TopSymbolsWithDirection(limit int, direction string) []SymbolDirection {
	if limit <= 0 {
		limit = 5
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]SymbolDirection, 0, limit)
	for _, r := range s.snapshot {
		if direction != "" && r.Direction != direction {
			continue
		}
		out = append(out, SymbolDirection{Symbol: r.Symbol, Direction: r.Direction})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (s *Scheduler) TopSymbols(limit int, direction string) []string {
	if limit <= 0 {
		limit = 5
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]string, 0, limit)
	for _, r := range s.snapshot {
		if direction != "" && r.Direction != direction {
			continue
		}
		out = append(out, r.Symbol)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// RefreshNow runs one snapshot refresh synchronously so a cold (empty)
// snapshot is filled with fresh Binance-computed data before the caller
// re-reads TopSymbols. Waits for any in-flight scan (background ticker or a
// concurrent RefreshNow) up to maxWait instead of double-scanning.
func (s *Scheduler) RefreshNow(maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	for {
		s.mu.Lock()
		if !s.scanning {
			s.scanning = true
			s.mu.Unlock()
			s.runOnce(nil)
			return
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TopByGrade returns strong/medium signals first, sorted by score descending.
// It keeps only entries with grade weak or better (drops noise).
func (s *Scheduler) TopByGrade(limit int) []ScanResult {
	if limit <= 0 {
		limit = 10
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]ScanResult, 0, len(s.snapshot))
	for _, r := range s.snapshot {
		if r.Grade == "noise" {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// StartDefault starts the process-wide scheduler once.
func StartDefault() {
	defaultScheduler.once.Do(func() { defaultScheduler.Start() })
}
