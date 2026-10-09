package breakout

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
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

// Tune-attempt marker: the durable "we last ran a tuning pass at T" record.
// Deliberately separate from params.json — BacktestAt only updates when a
// pass CHANGES values, so a steady-state pass that finds nothing to adjust
// would leave the stale marker in place and re-trigger forever. Written on
// every COMPLETED pass; failures leave it alone (the 10-min retry handles
// them). Override in tests via setTuneMarkerPath.
var tuneMarkerPath = "data/breakout_last_tune"

func setTuneMarkerPath(p string) { tuneMarkerPath = p }

func lastTuneAttempt() time.Time {
	b, err := os.ReadFile(tuneMarkerPath)
	if err != nil {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func markTuneAttempt() {
	if err := atomicWriteJSON(tuneMarkerPath, []byte(strconv.FormatInt(time.Now().UnixMilli(), 10))); err != nil {
		logger.Errorf("⚠️ tune marker persist FAILED: %v", err)
	}
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

		// Breakout tuning: due-ness reads the DURABLE marker file (written on
		// every completed pass, changed or not — BacktestAt in params.json
		// only moves when values actually changed, which would re-trigger
		// hourly in the no-change case). The old blind 168h ticker reset on
		// every deploy and could starve tuning indefinitely; a failed pass
		// (429 on the shared proxy IP) still retries after 10 minutes.
		go func() {
			time.Sleep(30 * time.Second)
			const tuneInterval = 7 * 24 * time.Hour
			pendingRetry := false
			// P1 (2026-09-26 re-review): a marker written by the OLD binary
			// reporting SUCCESS on a starved run (signals < minimum) must not
			// silence tuning until its 7-day expiry — treat it as due.
			markerStarved := func() bool {
				sum, err := LoadBacktestSummary()
				if err != nil || sum == nil {
					return false // no summary → the marker alone decides
				}
				return sum.Signals < btMinSample
			}
			due := func() bool {
				la := lastTuneAttempt()
				if la.IsZero() || time.Since(la) >= tuneInterval {
					return true
				}
				return markerStarved()
			}
			starvedUntil := time.Time{}
			run := func() {
				err := s.runTuning()
				switch {
				case err == nil:
					markTuneAttempt()
					pendingRetry = false
					starvedUntil = time.Time{}
				case errors.Is(err, ErrStarved):
					// P0 (2026-09-26 review): starvation is structural, not
					// transient — retry daily, not every 10 minutes.
					pendingRetry = false
					starvedUntil = time.Now().Add(24 * time.Hour)
				default:
					pendingRetry = true
				}
			}
			if due() {
				run()
			}
			hour := time.NewTicker(time.Hour)
			retry := time.NewTicker(10 * time.Minute)
			defer hour.Stop()
			defer retry.Stop()
			for {
				select {
				case <-hour.C:
					// P1: starvedUntil gates THIS branch too — the 24h backoff
					// was only consulted in the 10-minute retry path, while
					// due() stayed true after starved runs (no success marker)
					// and re-ran the backtest hourly.
					if due() && time.Now().After(starvedUntil) {
						run()
					}
				case <-retry.C:
					if pendingRetry && time.Now().After(starvedUntil) {
						run()
					}
				case <-s.stop:
					return
				}
			}
		}()

		// Slow-top short universe: a separate, cheaper cadence (30 min) —
		// its prefilter costs one 1d-klines call per liquid perp. The same
		// loop samples short signals hourly and runs the short-weight tuner
		// on every slow tick: RunShortTuner is idempotent (Evaluated flags)
		// and cheap (one batched ticker call), so the 30-min cadence makes
		// evaluation catch up within half an hour of samples maturing —
		// restart-proof, unlike the old 24h ticker that a deploy could reset
		// forever (the tuner never fired again after 09-07).
		go func() {
			refreshSlowTops()
			RunShortTuner(time.Now())
			RunTrendShortShadow(time.Now())
			slowTicker := time.NewTicker(30 * time.Minute)
			sampleTicker := time.NewTicker(time.Hour)
			defer slowTicker.Stop()
			defer sampleTicker.Stop()
			for {
				select {
				case <-slowTicker.C:
					refreshSlowTops()
					RunShortTuner(time.Now())
					RunTrendShortShadow(time.Now())
				case <-sampleTicker.C:
					shorts, at := s.ShortSnapshot()
					if len(shorts) > 0 {
						SampleShortSignals(shorts, at)
					}
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
// Returns false when the pass could not run (list-symbols or backtest error,
// e.g. a 429 on the shared proxy IP) so the caller can retry soon instead of
// sleeping the full weekly interval.
func (s *Scheduler) runTuning() (runErr error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("🚨 Breakout tuning panicked (recovered): %v", r)
			runErr = fmt.Errorf("panic: %v", r)
		}
	}()
	// 30 symbols: the old top-15 under-covered the high-ATR alts the
	// candidate pool actually trades (E2, QUANT_REVIEW 09-22 — the replay
	// must see the universe the strategy trades, not only majors).
	symbols, err := TopVolumeSymbols(30)
	if err != nil {
		logger.Warnf("⚠️ Breakout tuning: failed to list symbols: %v", err)
		return err
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
		return err
	}
	return nil
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
	// 2026-10-03 review: the ONLY scan entry without a recover — a panic in
	// AnalyzeMany/ScanShorts ran on the background ticker goroutine (or a
	// RefreshNow caller's goroutine) and took the whole process down.
	//
	// 2026-10-06 review P2-11 (independently reproduced): the recover logged
	// but never reset `scanning` — after a recovered panic the 5-min ticker
	// skipped every cycle (`if s.scanning { continue }`) and RefreshNow spun
	// to its deadline, freezing both boards until restart.
	//
	// Fix-recheck 2026-10-06: cleanup AND the callback are owned by THIS
	// defer alone — the body fires nothing, so every path (return, error,
	// panic) observes the same order: scanning cleared first, onDone fired
	// exactly once after it.
	var onDoneOnce sync.Once
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("🐷 Breakout scan panicked (recovered): %v\n%s", r, debug.Stack())
		}
		s.mu.Lock()
		s.scanning = false
		s.mu.Unlock()
		if onDone != nil {
			onDoneOnce.Do(onDone)
		}
	}()
	start := time.Now()
	symbols, err := TopVolumeSymbols(s.Symbols)
	if err != nil {
		logger.Warnf("⚠️ Breakout scheduler: failed to list symbols: %v", err)
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

	// BTC market regime — the SAME 4h-EMA classifier the short pipeline uses
	// (btcRegimePenalty), so both scanners label the market identically.
	// Review 2026-10-04 #3: the old "BTC's own piggy score ≥ medium" read
	// said btc_bull exactly when BTC itself was the strongest breaker — a
	// momentum echo, not a regime — and fell back to chop (uniform ×0.95,
	// no relative differentiation) most of the time. Failure to fetch BTC
	// 4h bars keeps the old chop default.
	regime := "chop"
	if btc4h, berr := NewBinanceDS("BTCUSDT").Klines("4h", 84); berr == nil && len(btc4h) >= 30 {
		if _, r := btcRegimePenalty(btc4h); r != "" {
			regime = r
		}
	}
	applyRegimeAdjustment(results, regime)

	s.mu.Lock()
	s.snapshot = results
	s.updatedAt = time.Now()
	s.mu.Unlock()
	logger.Infof("🐷 Breakout snapshot updated: %d symbols in %v (regime: %s)", len(results), time.Since(start).Round(time.Millisecond), regime)

	// Short scan rides the same cadence: rank the top 24h gainers by
	// short-suitability. Best-effort — a short-scan failure never touches
	// the main breakout snapshot.
	// 0/0 = default history-pool knobs — the scheduler has no strategy
	// context; per-strategy values ride the kernel's own ScanShorts call (A4).
	if shorts, shortAt, err := ScanShorts(ShortScanUniverse, 0, 0); err != nil {
		logger.Warnf("⚠️ Short scan failed: %v", err)
	} else {
		s.mu.Lock()
		s.shortSnapshot = shorts
		s.shortUpdatedAt = shortAt
		s.mu.Unlock()
		logger.Infof("🩸 Short scan updated: %d candidates in %v", len(shorts), time.Since(start).Round(time.Millisecond))
	}

	// scanning reset and the onDone callback live in the entry defer
	// (panic-safe, exactly-once, always AFTER the flag clears).
}

// applyRegimeAdjustment applies the regime haircuts, then re-grades and
// re-ranks. Pure so the ordering contract is unit-testable. Review
// 2026-10-04 #3: the Score haircut used to leave the Grade at its
// pre-adjustment band (a strong alt discounted to 72 kept its "strong"
// label) and Percentile was stamped from the pre-haircut order — both now
// derive from the FINAL scores.
func applyRegimeAdjustment(results []ScanResult, regime string) {
	for i := range results {
		if results[i].Symbol == "BTCUSDT" {
			continue // the regime driver itself is never haircut
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
		results[i].Grade = gradeOf(results[i].Score)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	n := len(results)
	for i := range results {
		results[i].Percentile = round2(float64(n-i) / float64(n) * 100)
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
// SymbolDirection pairs a snapshot symbol with its selected direction plus
// the scan-time quality facts (score/grade/pattern) — the candidate pool
// needs the direction for cross-scanner conflict detection and the grade/
// pattern for its quality floor (review 2026-10-04 #4: the snapshot is a
// ranking, not a filter).
type SymbolDirection struct {
	Symbol    string
	Direction string // "up" | "down"
	Score     float64
	Grade     string // strong / medium / weak / noise (scan-time, post-regime)
	Pattern   string // breakout / retest_hold / extended / approach
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
		out = append(out, SymbolDirection{Symbol: r.Symbol, Direction: r.Direction, Score: r.Score, Grade: r.Grade, Pattern: r.Pattern})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// TopSymbolsWithDirectionAge is TopSymbolsWithDirection plus the snapshot's
// age (time since updatedAt) — candidate selection must be able to refuse a
// stale board instead of minting trades off a dead scanner (F20, 2026-10-01
// review). A never-scanned scheduler reports an "ancient" age so the
// cold-start refresh path triggers.
func (s *Scheduler) TopSymbolsWithDirectionAge(limit int, direction string) ([]SymbolDirection, time.Duration) {
	age := 365 * 24 * time.Hour // "ancient" sentinel before the first scan
	s.mu.RLock()
	if !s.updatedAt.IsZero() {
		age = time.Since(s.updatedAt)
	}
	defer s.mu.RUnlock()

	out := make([]SymbolDirection, 0, limit)
	for _, r := range s.snapshot {
		if direction != "" && r.Direction != direction {
			continue
		}
		out = append(out, SymbolDirection{Symbol: r.Symbol, Direction: r.Direction, Score: r.Score, Grade: r.Grade, Pattern: r.Pattern})
		if len(out) >= limit {
			break
		}
	}
	return out, age
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

// RefreshNow runs one snapshot refresh synchronously so a cold (empty) or
// stale snapshot is filled with fresh Binance-computed data before the caller
// re-reads TopSymbols. Waits for any in-flight scan (background ticker or a
// concurrent RefreshNow) up to maxWait instead of double-scanning: a waiter
// returns as soon as the snapshot it is waiting for LANDS (updatedAt advances
// past the wait start) — it never claims a second back-to-back scan.
func (s *Scheduler) RefreshNow(maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	s.mu.RLock()
	waitedFor := s.updatedAt // a scan completing after this instant satisfies us
	s.mu.RUnlock()
	for {
		s.mu.Lock()
		if !s.scanning {
			if s.updatedAt.After(waitedFor) {
				// The scan we were waiting for already landed — do NOT start
				// another one (the old loop re-claimed the flag and ran a
				// duplicate full scan back-to-back).
				s.mu.Unlock()
				return
			}
			s.scanning = true
			s.mu.Unlock()
			started := time.Now()
			s.runOnce(nil)
			// 2026-10-03 review: RefreshNow's maxWait bounds only the WAIT —
			// surface scans that exceed it so callers know the sync refresh
			// overran their budget.
			if took := time.Since(started); took > maxWait {
				logger.Warnf("🐷 Synchronous refresh took %s (caller budget %s) — Binance slow or board large", took.Round(time.Second), maxWait)
			}
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

// SetSnapshotForTest seeds the snapshot buffer so downstream fan-in tests
// (kernel mixed-pool assembly) can exercise piggy-dash reads without a
// Binance round-trip. updatedAt is stamped "now" so staleness gates pass.
func (s *Scheduler) SetSnapshotForTest(results []ScanResult) {
	s.mu.Lock()
	s.snapshot = results
	s.updatedAt = time.Now()
	s.mu.Unlock()
}
