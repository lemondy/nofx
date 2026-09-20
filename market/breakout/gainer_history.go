package breakout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"nofx/logger"
)

// Gainer history pool (历史涨幅池). The 24h-gainer board only sees coins
// pumping RIGHT NOW: a coin that pumped hard days ago and has since rolled
// over — often the cleanest short structure on the whole board — silently
// drops off the live ranking and never gets analyzed (user 2026-09-19).
// This module persists a rolling record of each day's top gainer snapshots
// (近N天每日涨幅Top20,默认N=7) so ScanShorts can merge those fading pumps
// back into the candidate universe regardless of today's 24h rank.
//
// Persistence follows the package's other durable state (params.json, the
// short-tuner journal): one JSON file under data/, atomically rewritten
// (temp + rename) after each recording run and pruned to the window on
// write. Days are UTC dates; each (day, symbol) entry keeps the PEAK 24h
// change seen that day, so a pump that faded before the next snapshot still
// counts as that day's gainer.

const (
	// DefaultShortScanHistoryDays — merged window when the strategy config
	// leaves the knob at 0.
	DefaultShortScanHistoryDays = 7
	// DefaultShortScanHistoryMax — cap on EXTRA history-pool symbols analyzed
	// per scan. The pool after a full week holds ~100+ unique symbols;
	// analyzing all of them would roughly triple the scanner's fapi budget.
	DefaultShortScanHistoryMax = 30
	// shortScanHistoryMaxCap — hard ceiling for the config knob; higher
	// values clamp down (a 200-symbol scan every 5 min would 429 the shared
	// egress IP and stall every scanner).
	shortScanHistoryMaxCap = 100
	// gainerHistBoardKeep — how many rows of the live board feed the daily
	// record (user spec: 每日涨幅Top20).
	gainerHistBoardKeep = 20
	// gainerHistPerDayCap bounds per-day churn: the board's top-20 rotates,
	// and a day's record is the union of every top-20 seen that day. 40 ≈
	// 2× the nominal board size — enough for a full day of rotation.
	gainerHistPerDayCap = 40
	// gainerHistKeepBuffer — days beyond the configured window kept on disk,
	// so shrinking the window in the UI doesn't destroy data the user may
	// re-enable a day later.
	gainerHistKeepBuffer = 2
)

// ResolveShortScanHistoryDays maps the raw config value to an effective
// window: 0 = built-in default, negative = disabled (returns 0). The setter,
// the prompt legend and the engine all read through this — single
// definition, no divergent defaults.
func ResolveShortScanHistoryDays(raw int) int {
	if raw == 0 {
		return DefaultShortScanHistoryDays
	}
	if raw < 0 {
		return 0
	}
	return raw
}

// ResolveShortScanHistoryMax maps the raw config value to the effective
// extra-symbol cap: 0/negative = built-in default, values above the hard
// ceiling clamp down.
func ResolveShortScanHistoryMax(raw int) int {
	if raw <= 0 {
		return DefaultShortScanHistoryMax
	}
	if raw > shortScanHistoryMaxCap {
		return shortScanHistoryMaxCap
	}
	return raw
}

// Scanner-side knobs. The scheduler and the kernel share one scan cache, so
// both callers must resolve the SAME values or the cache ping-pongs between
// two universe definitions and re-scans on every call — hence package-level
// config instead of per-call parameters. The kernel syncs the strategy's
// values each cycle; the scheduler picks them up for free.
var (
	shortHistCfgMu   sync.Mutex
	shortHistDaysCfg = DefaultShortScanHistoryDays
	shortHistMaxCfg  = DefaultShortScanHistoryMax
	// shortHistLastWarn rate-limits the flip warning below: with multiple
	// traders running different strategies, each engine re-syncs its own
	// values EVERY cycle and the knob ping-pongs — one warning per window
	// is enough to surface it (round-4 review R4-13).
	shortHistLastWarn time.Time
)

// SetShortScanHistoryConfig syncs the strategy-side knobs into the scanner
// (raw values; days ≤ 0 resolves via ResolveShortScanHistoryDays, so a
// negative config disables the pool). Idempotent — safe to call per cycle.
// NOTE: this is a process-global single value shared with the scheduler's
// scan cache. A value change now warns (once per 10 minutes): legitimate UI
// saves warn once, while multi-trader strategies with different values
// ping-pong this every cycle and need the noise surfaced.
func SetShortScanHistoryConfig(rawDays, rawMax int) {
	shortHistCfgMu.Lock()
	defer shortHistCfgMu.Unlock()
	days := ResolveShortScanHistoryDays(rawDays)
	max := ResolveShortScanHistoryMax(rawMax)
	if (days != shortHistDaysCfg || max != shortHistMaxCfg) && time.Since(shortHistLastWarn) > 10*time.Minute {
		shortHistLastWarn = time.Now()
		logger.Warnf("short-scan history pool config changed %dd/%dmax → %dd/%dmax — this is a PROCESS-GLOBAL scanner knob: multiple traders running different short_scan_history values will fight over it every cycle (last writer wins)",
			shortHistDaysCfg, shortHistMaxCfg, days, max)
	}
	shortHistDaysCfg = days
	shortHistMaxCfg = max
}

func shortScanHistoryDays() int {
	shortHistCfgMu.Lock()
	defer shortHistCfgMu.Unlock()
	return shortHistDaysCfg
}

func shortScanHistoryMax() int {
	shortHistCfgMu.Lock()
	defer shortHistCfgMu.Unlock()
	return shortHistMaxCfg
}

// GainerQuote is one row of the live 24h gainer board.
type GainerQuote struct {
	Symbol string
	ChgPct float64
	Price  float64
}

type gainerHistEntry struct {
	Symbol string  `json:"s"`
	Chg    float64 `json:"chg"` // peak 24h change % recorded for this day
	Price  float64 `json:"p"`   // price at the recorded peak
	TS     int64   `json:"ts"`  // unix ms of the recording
}

type gainerHistoryFile struct {
	Days map[string][]gainerHistEntry `json:"days"` // "2006-01-02" (UTC) -> entries
}

var (
	gainerHistMu   sync.Mutex
	gainerHistPath = "data/gainer_history.json"
)

func setGainerHistoryPath(p string) { gainerHistPath = p }

// recordGainerHistory merges the live gainer board into the daily pool and
// persists it. Best-effort: a failure logs and keeps trading. Only the
// first gainerHistBoardKeep rows (the day's nominal Top-20) feed the pool.
func recordGainerHistory(board []GainerQuote, now time.Time) {
	if len(board) == 0 {
		return
	}
	gainerHistMu.Lock()
	defer gainerHistMu.Unlock()
	hist := loadGainerHistoryLocked()
	day := now.UTC().Format("2006-01-02")
	// Max-merge: keep each symbol's peak change for the day, so a pump that
	// faded between two snapshots still counts as that day's gainer.
	bySym := make(map[string]gainerHistEntry, len(hist.Days[day])+len(board))
	for _, e := range hist.Days[day] {
		bySym[e.Symbol] = e
	}
	for _, q := range board[:min(len(board), gainerHistBoardKeep)] {
		if old, ok := bySym[q.Symbol]; !ok || q.ChgPct > old.Chg {
			bySym[q.Symbol] = gainerHistEntry{Symbol: q.Symbol, Chg: q.ChgPct, Price: q.Price, TS: now.UnixMilli()}
		}
	}
	entries := make([]gainerHistEntry, 0, len(bySym))
	for _, e := range bySym {
		entries = append(entries, e)
	}
	// Bound pathological board rotation: strongest pumps win.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Chg > entries[j].Chg })
	if len(entries) > gainerHistPerDayCap {
		entries = entries[:gainerHistPerDayCap]
	}
	hist.Days[day] = entries
	// Prune past the window (+buffer). Floor the keep at the default window
	// so a temporarily-disabled pool doesn't starve.
	keep := shortScanHistoryDays()
	if keep < DefaultShortScanHistoryDays {
		keep = DefaultShortScanHistoryDays
	}
	cutoff := now.UTC().AddDate(0, 0, -(keep + gainerHistKeepBuffer - 1)).Format("2006-01-02")
	for d := range hist.Days {
		if d < cutoff {
			delete(hist.Days, d)
		}
	}
	if err := saveGainerHistoryLocked(hist); err != nil {
		logger.Warnf("⚠️ Gainer history pool: save failed: %v", err)
	}
}

// loadGainerHistory reads the pool from disk (empty when absent — the pool
// needs ~a day of uptime to fill, and a missing file must not fail the scan).
func loadGainerHistory() *gainerHistoryFile {
	gainerHistMu.Lock()
	defer gainerHistMu.Unlock()
	return loadGainerHistoryLocked()
}

func loadGainerHistoryLocked() *gainerHistoryFile {
	hist := &gainerHistoryFile{Days: map[string][]gainerHistEntry{}}
	b, err := os.ReadFile(gainerHistPath)
	if err != nil {
		return hist
	}
	_ = json.Unmarshal(b, hist)
	if hist.Days == nil {
		hist.Days = map[string][]gainerHistEntry{}
	}
	return hist
}

func saveGainerHistoryLocked(hist *gainerHistoryFile) error {
	if err := os.MkdirAll(filepath.Dir(gainerHistPath), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(hist)
	if err != nil {
		return err
	}
	tmp := gainerHistPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, gainerHistPath)
}

// historyUniverseCandidates merges the recorded pool over the window into a
// deduped candidate list: strongest recorded pump first, capped at max.
// Skips symbols already covered by the live gainer board, and symbols with
// no live quote — delisted, or volume faded below the board's liquidity
// floor (an illiquid fading pump is squeeze fuel, not a short candidate).
func historyUniverseCandidates(hist *gainerHistoryFile, now time.Time, days int, index map[string]GainerQuote, covered map[string]bool, max int) []GainerQuote {
	if hist == nil || days <= 0 || max <= 0 {
		return nil
	}
	cutoff := now.UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	type ranked struct {
		q    GainerQuote // live quote (the analysis input must be current)
		peak float64     // recorded peak pump — the merit order
	}
	var rs []ranked
	seen := make(map[string]bool, len(rs))
	for day, entries := range hist.Days {
		if day < cutoff {
			continue
		}
		for _, e := range entries {
			if covered[e.Symbol] || seen[e.Symbol] {
				continue
			}
			q, ok := index[e.Symbol]
			if !ok {
				continue
			}
			seen[e.Symbol] = true
			rs = append(rs, ranked{q: q, peak: e.Chg})
		}
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].peak > rs[j].peak })
	if len(rs) > max {
		rs = rs[:max]
	}
	out := make([]GainerQuote, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.q)
	}
	return out
}
