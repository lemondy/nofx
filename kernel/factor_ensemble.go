package kernel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nofx/logger"
	"nofx/market"
)

// FactorEnsembleRef is opt-in research evidence only. No gate, score,
// candidate filter, or execution decision reads it.
type FactorEnsembleRef struct {
	AsOf       string                      `json:"as_of"`
	AgeDays    int                         `json:"age_days"` // latest closed UTC day minus as_of
	SnapshotID string                      `json:"snapshot_id"`
	Scope      string                      `json:"scope,omitempty"`
	ScopeNote  string                      `json:"scope_note,omitempty"`
	Reason     string                      `json:"reason,omitempty"`
	Horizons   map[string]FactorHorizonRef `json:"horizons,omitempty"`
}

type FactorHorizonRef struct {
	Pct     float64        `json:"pct"`
	Z       float64        `json:"z"`
	Factors int            `json:"factors"`
	Forward string         `json:"forward"`
	Bull    *int           `json:"bull,omitempty"`
	Bear    *int           `json:"bear,omitempty"`
	Neutral *int           `json:"neutral,omitempty"`
	Top     []FactorTopRef `json:"top,omitempty"`
}

type FactorTopRef struct {
	ID      string   `json:"id"`
	Family  string   `json:"family,omitempty"`
	Z       float64  `json:"z"`
	ValT    *float64 `json:"val_t,omitempty"`
	Forward string   `json:"forward,omitempty"`
}

const factorEnsembleMaxFileSize = 8 << 20

type factorEnsembleScores struct {
	Version        int                        `json:"version"`
	Market         string                     `json:"market"`
	AsOf           string                     `json:"as_of"`
	RunID          json.RawMessage            `json:"run_id"`
	Horizons       map[string]json.RawMessage `json:"horizons"`
	Universe       json.RawMessage            `json:"universe"`
	Factors        json.RawMessage            `json:"factors"`
	Skipped        json.RawMessage            `json:"skipped"`
	asOfDate       time.Time
	snapshotID     string
	futureWarnOnce sync.Once
	extendedFormat bool
	horizons       map[string]factorEnsembleHorizonScores
	factors        map[string]json.RawMessage
	skipped        map[string]json.RawMessage
}

type factorEnsembleHorizonScores struct {
	FactorCount int                        `json:"factor_count"`
	Forward     map[string]int             `json:"forward"`
	Scores      map[string]json.RawMessage `json:"scores"`
	ExtScores   json.RawMessage            `json:"ext_scores"`
	Detail      json.RawMessage            `json:"detail"`
	FactorIDs   json.RawMessage            `json:"factor_ids"`
	extScores   map[string]json.RawMessage
	detail      map[string]json.RawMessage
}

type factorEnsembleFileCache struct {
	mu       sync.Mutex
	path     string
	mtime    time.Time
	scores   *factorEnsembleScores // metadata immutable after publication; warning uses sync.Once
	lastWarn time.Time
}

var liveFactorEnsembleCache factorEnsembleFileCache

func factorEnsembleScoresPath() string {
	dir := os.Getenv("FACTOR_MINING_DIR")
	if dir == "" {
		dir = "data/factor_mining"
	}
	return filepath.Join(dir, "crypto", "live_scores.json")
}

// load accepts a path so tests can use isolated files. Invalid snapshots are
// cached too; a changed mtime retries them without serving the previous file.
func (c *factorEnsembleFileCache) load(path string) *factorEnsembleScores {
	c.mu.Lock()
	defer c.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		c.path, c.mtime, c.scores = "", time.Time{}, nil
		c.warnLocked(path, err)
		return nil
	}
	if c.path == path && c.mtime.Equal(info.ModTime()) {
		return c.scores
	}
	c.path, c.mtime, c.scores = path, info.ModTime(), nil
	if !info.Mode().IsRegular() || info.Size() > factorEnsembleMaxFileSize {
		c.warnLocked(path, fmt.Errorf("expected a regular file of at most 8 MiB"))
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		c.warnLocked(path, err)
		return nil
	}
	defer f.Close()
	// The nightly writer can replace the file between Stat and Open.
	info, err = f.Stat()
	if err != nil {
		c.warnLocked(path, err)
		return nil
	}
	c.mtime = info.ModTime()
	if !info.Mode().IsRegular() || info.Size() > factorEnsembleMaxFileSize {
		c.warnLocked(path, fmt.Errorf("expected a regular file of at most 8 MiB"))
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(f, factorEnsembleMaxFileSize+1))
	if err != nil {
		c.warnLocked(path, err)
		return nil
	}
	if len(raw) > factorEnsembleMaxFileSize {
		c.warnLocked(path, fmt.Errorf("file exceeds 8 MiB"))
		return nil
	}
	var scores factorEnsembleScores
	if err := json.Unmarshal(raw, &scores); err != nil {
		c.warnLocked(path, err)
		return nil
	}
	if scores.Version != 1 || scores.Market != "crypto" {
		c.warnLocked(path, fmt.Errorf("expected version 1 and market crypto"))
		return nil
	}
	scores.asOfDate, err = time.Parse(time.DateOnly, scores.AsOf)
	if err != nil {
		c.warnLocked(path, err)
		return nil
	}
	_ = json.Unmarshal(scores.RunID, &scores.snapshotID)
	if strings.TrimSpace(scores.snapshotID) == "" {
		digest := sha256.Sum256(raw)
		scores.snapshotID = fmt.Sprintf("%x", digest[:6])
	}
	// Optional additions are decoded independently: malformed metadata or one
	// extended score must not discard the core scores or another horizon.
	scores.extendedFormat = len(scores.Universe) > 0 || len(scores.Factors) > 0 || len(scores.Skipped) > 0
	scores.factors = factorEnsembleObject(scores.Factors)
	scores.skipped = factorEnsembleObject(scores.Skipped)
	scores.horizons = make(map[string]factorEnsembleHorizonScores)
	for horizon, raw := range scores.Horizons {
		var h factorEnsembleHorizonScores
		if json.Unmarshal(raw, &h) != nil {
			continue
		}
		if len(h.ExtScores) > 0 || len(h.Detail) > 0 || len(h.FactorIDs) > 0 {
			scores.extendedFormat = true
		}
		h.extScores = factorEnsembleObject(h.ExtScores)
		h.detail = factorEnsembleObject(h.Detail)
		scores.horizons[horizon] = h
	}
	c.scores = &scores
	return c.scores
}

// Called only with c.mu held; all failure types share one hourly budget.
func (c *factorEnsembleFileCache) warnLocked(path string, err error) {
	now := time.Now()
	if c.lastWarn.IsZero() || now.Sub(c.lastWarn) >= time.Hour {
		c.lastWarn = now
		logger.Warnf("[factor-ensemble] Ignoring %s: %v", path, err)
	}
}

func computeFactorEnsemble(data *market.Data, now time.Time) *FactorEnsembleRef {
	if data == nil {
		return nil
	}
	path := factorEnsembleScoresPath()
	scores := liveFactorEnsembleCache.load(path)
	if scores == nil {
		return nil
	}
	// Recheck against the caller's clock even on cache hits and historical replays.
	utc := now.UTC()
	today := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	latestClosed := today.AddDate(0, 0, -1)
	if scores.asOfDate.After(latestClosed) {
		scores.futureWarnOnce.Do(func() {
			logger.Warnf("[factor-ensemble] Ignoring %s: as_of %s is after latest closed UTC day %s", path, scores.AsOf, latestClosed.Format(time.DateOnly))
		})
		return nil
	}
	ageDays := int(latestClosed.Sub(scores.asOfDate) / (24 * time.Hour))
	freshHorizons := make(map[string]factorEnsembleHorizonScores)
	for horizon, h := range scores.horizons {
		if ageDays <= factorEnsembleMaxAge(horizon) {
			freshHorizons[horizon] = h
		}
	}
	if len(freshHorizons) == 0 {
		return nil
	}
	// Scope is per symbol. Prefer the core everywhere if a malformed snapshot
	// lists the same symbol in both universes, even in different fresh horizons.
	core, extended := false, false
	for _, h := range freshHorizons {
		if _, ok := h.Scores[data.Symbol]; ok {
			core = true
		}
		if _, ok := h.extScores[data.Symbol]; ok {
			extended = true
		}
	}
	if !core && !extended {
		// Old snapshots retain their exact output. Reuse the existing xyz and
		// stock classifiers in addition to requiring a plain USDT crypto pair.
		if !scores.extendedFormat || len(data.Symbol) <= len("USDT") || !strings.HasSuffix(data.Symbol, "USDT") || strings.Contains(data.Symbol, ":") {
			return nil
		}
		if market.IsXyzDexAsset(data.Symbol) || market.IsBStockSymbol(data.Symbol) {
			return nil
		}
		reason := "not_in_universe"
		var skipped string
		if json.Unmarshal(scores.skipped[data.Symbol], &skipped) == nil && skipped != "" {
			reason = skipped
		}
		return &FactorEnsembleRef{AsOf: scores.AsOf, AgeDays: ageDays, SnapshotID: scores.snapshotID, Scope: "none", Reason: reason}
	}
	ref := &FactorEnsembleRef{AsOf: scores.AsOf, AgeDays: ageDays, SnapshotID: scores.snapshotID, Horizons: make(map[string]FactorHorizonRef)}
	if scores.extendedFormat {
		ref.Scope = "core"
		if !core {
			ref.Scope = "extended"
			ref.ScopeNote = "该币不在因子验证宇宙(流动性前60)内，因子未在此档位验证，分数仅相对核心宇宙外推，参考权重应低于 core"
		}
	}
	for horizon, h := range freshHorizons {
		symbolScores := h.Scores
		if !core {
			symbolScores = h.extScores
		}
		raw, ok := symbolScores[data.Symbol]
		if !ok {
			continue
		}
		// Decode separately so overflow in one score cannot discard other
		// horizons. Pointers distinguish missing/null values from genuine zero.
		var score struct {
			Pct *float64 `json:"pct"`
			Z   *float64 `json:"z"`
		}
		if json.Unmarshal(raw, &score) != nil || score.Pct == nil || score.Z == nil {
			continue
		}
		pct, z := *score.Pct, *score.Z
		if math.IsNaN(pct) || math.IsInf(pct, 0) || math.IsNaN(z) || math.IsInf(z, 0) || pct < 0 || pct > 1 {
			continue
		}
		verdicts := make([]string, 0, len(h.Forward))
		for verdict := range h.Forward {
			verdicts = append(verdicts, verdict)
		}
		sort.Strings(verdicts)
		parts := make([]string, 0, len(verdicts))
		for _, verdict := range verdicts {
			parts = append(parts, fmt.Sprintf("%s×%d", verdict, h.Forward[verdict]))
		}
		href := FactorHorizonRef{
			Pct: pct, Z: z, Factors: h.FactorCount, Forward: strings.Join(parts, ","),
		}
		addFactorDetail(&href, h.detail[data.Symbol], scores.factors)
		ref.Horizons[horizon] = href
	}
	if len(ref.Horizons) == 0 {
		return nil
	}
	return ref
}

// Age is measured from the most recent closed UTC daily candle, not today.
func factorEnsembleMaxAge(horizon string) int {
	h, err := strconv.Atoi(horizon)
	if err != nil || h <= 0 {
		return -1
	}
	if h == 3 {
		return 2
	}
	return min(h, 3)
}

func factorEnsembleObject(raw json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

func factorEnsembleFiniteNumber(raw json.RawMessage) *float64 {
	var value *float64
	if json.Unmarshal(raw, &value) != nil || value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	return value
}

func addFactorDetail(ref *FactorHorizonRef, raw json.RawMessage, metadata map[string]json.RawMessage) {
	bull, bear, neutral := 0, 0, 0
	var top []FactorTopRef
	for id, rawZ := range factorEnsembleObject(raw) {
		z := factorEnsembleFiniteNumber(rawZ)
		if z == nil {
			continue
		}
		switch {
		case *z >= 0.5:
			bull++
		case *z <= -0.5:
			bear++
		default:
			neutral++
		}
		top = append(top, FactorTopRef{ID: id, Z: *z})
	}
	if len(top) == 0 {
		return
	}
	ref.Bull, ref.Bear, ref.Neutral = &bull, &bear, &neutral
	sort.Slice(top, func(i, j int) bool {
		if math.Abs(top[i].Z) == math.Abs(top[j].Z) {
			return top[i].ID < top[j].ID
		}
		return math.Abs(top[i].Z) > math.Abs(top[j].Z)
	})
	if len(top) > 3 {
		top = top[:3]
	}
	for i := range top {
		fields := factorEnsembleObject(metadata[top[i].ID])
		// Decode each displayed field separately so bad val_t (or unrelated
		// metadata such as val_ic) cannot hide a valid family/forward status.
		_ = json.Unmarshal(fields["family"], &top[i].Family)
		top[i].ValT = factorEnsembleFiniteNumber(fields["val_t"])
		var forward string
		if json.Unmarshal(fields["forward"], &forward) == nil {
			switch forward {
			case "pending", "confirmed", "inconclusive", "failed", "none":
				top[i].Forward = forward
			}
		}
	}
	ref.Top = top
}
