package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ============================================================================
// Strategy config version history (user request 2026-10-04: "策略有迭代升级
// 流程，参数会变化，前端页面看不到对应版本和参数迭代后的效果").
//
// Every persisted config change becomes an append-only row: full config
// snapshot + canonical hash + a field-level diff against the previous row.
// Realized performance per version is computed on read by joining the trade
// journal over each version's active window (entry_time ∈ [changed_at,
// next.changed_at)), so old trades need no backfill and closed books stay
// stable.
//
// Write paths, all funneled through RecordConfigChange (hash-deduped, so the
// per-cycle drift detector and the API save handler can both fire safely):
//   - strategy create/duplicate  → baseline row (source "create"/"duplicate")
//   - PUT /api/strategies/:id    → "save"
//   - per-cycle drift detector (direct DB writes bypassing the API) → "external"
//   - lazy baseline on the versions GET endpoint → "baseline"
// ============================================================================

// StrategyConfigVersion one append-only snapshot of strategies.config.
type StrategyConfigVersion struct {
	ID         int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	StrategyID string `gorm:"column:strategy_id;not null;index:idx_strategy_versions_strategy" json:"strategy_id"`
	// ConfigHash is the canonical full-config hash (identity of the snapshot).
	ConfigHash string `gorm:"column:config_hash;not null;default:'';index:idx_strategy_versions_hash" json:"config_hash"`
	// RiskHash mirrors trader.RiskControlHash — the same value decision
	// records carry as config_hash, so cycles can be attributed to versions.
	RiskHash string `gorm:"column:risk_hash;not null;default:''" json:"risk_hash"`
	// Source: create | duplicate | baseline | save | external.
	Source string `gorm:"column:source;not null;default:''" json:"source"`
	// Summary is a JSON-encoded []ConfigDiffEntry (empty array for baselines).
	Summary string `gorm:"column:summary;not null;default:'[]'" json:"summary"`
	// Config is the canonical full-config JSON snapshot.
	Config    string `gorm:"column:config;not null" json:"config"`
	ChangedAt int64  `gorm:"column:changed_at;not null;default:0;index:idx_strategy_versions_changed" json:"changed_at"` // Unix ms UTC
	CreatedAt int64  `gorm:"column:created_at;not null;default:0" json:"created_at"`                                     // Unix ms UTC
}

// TableName returns the table name
func (StrategyConfigVersion) TableName() string { return "strategy_config_versions" }

// ConfigDiffEntry one changed leaf path. Old/New are JSON-encoded values
// ("" = field absent on that side). Long strings are truncated server-side.
type ConfigDiffEntry struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// VersionStats realized performance of trades ENTERED during a version's
// active window. NET caliber throughout (RealizedPnL − Fee), matching
// JournalStats — the account keeps the net figure.
type VersionStats struct {
	Trades       int     `json:"trades"`
	Wins         int     `json:"wins"`
	Losses       int     `json:"losses"`
	WinRate      float64 `json:"win_rate"`      // percent
	NetPnL       float64 `json:"net_pnl"`       // USDT, net of fees
	AvgWin       float64 `json:"avg_win"`       // USDT
	AvgLoss      float64 `json:"avg_loss"`      // USDT (positive number)
	ProfitFactor float64 `json:"profit_factor"` // gross win / gross loss, 0 when no losses
	AvgR         float64 `json:"avg_r"`         // net PnL ÷ opening risk (|entry−planned SL|×qty) when the plan exists
	RCount       int     `json:"r_count"`       // trades with a computable R
}

func (s *StrategyStore) initVersionTables() error {
	return s.db.AutoMigrate(&StrategyConfigVersion{})
}

// CanonicalConfigJSON normalizes a config JSON string: parse → re-marshal
// (Go sorts map keys, so field order/whitespace differences between a raw DB
// row and a struct marshal vanish). Unparseable input round-trips trimmed.
func CanonicalConfigJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return trimmed
	}
	b, err := json.Marshal(v)
	if err != nil {
		return trimmed
	}
	return string(b)
}

// hashCanonical is the version identity: 12 hex chars of SHA-256 over the
// canonical JSON. Short on purpose — it is a display/correlation key, not a
// security boundary.
func hashCanonical(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:6])
}

// HashConfigJSON hashes a raw config JSON string canonically.
func HashConfigJSON(raw string) string { return hashCanonical(CanonicalConfigJSON(raw)) }

// HashStrategyConfig hashes an already-parsed config (same value as
// HashConfigJSON of the row it was written from — both sides canonicalize
// through the struct's field-deterministic marshal).
func HashStrategyConfig(cfg *StrategyConfig) string {
	if cfg == nil {
		return hashCanonical("{}")
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	return hashCanonical(string(b))
}

// HashRiskControlOf hashes only the risk_control block — byte-identical to
// trader.RiskControlHash (contract test pins this), which is what
// decision_records.config_hash stores.
func HashRiskControlOf(cfg *StrategyConfig) string {
	if cfg == nil {
		return ""
	}
	b, err := json.Marshal(cfg.RiskControl)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// diffLimit caps the stored diff entries; beyond it a truncation marker is
// appended so the count is still visible.
const diffLimit = 40

// diffValueMaxLen truncates long leaf values (prompt sections are KB-scale).
const diffValueMaxLen = 120

// flattenConfig reduces a parsed config to leaf paths. Nested maps recurse;
// everything else (arrays, scalars) is JSON-encoded as one leaf.
func flattenConfig(prefix string, v interface{}, out map[string]string) {
	switch t := v.(type) {
	case map[string]interface{}:
		if len(t) == 0 {
			out[prefix] = "{}"
			return
		}
		for k, child := range t {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			flattenConfig(path, child, out)
		}
	default:
		b, err := json.Marshal(t)
		if err != nil {
			b = []byte(fmt.Sprintf("%v", t))
		}
		out[prefix] = string(b)
	}
}

func truncateDiffValue(s string) string {
	if len(s) <= diffValueMaxLen {
		return s
	}
	return s[:diffValueMaxLen] + "…"
}

// diffConfigs returns the sorted field-level differences between two parsed
// configs.
func diffConfigs(old, new map[string]interface{}) []ConfigDiffEntry {
	flatOld := map[string]string{}
	flatNew := map[string]string{}
	flattenConfig("", old, flatOld)
	flattenConfig("", new, flatNew)

	paths := map[string]bool{}
	for p := range flatOld {
		paths[p] = true
	}
	for p := range flatNew {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	diff := make([]ConfigDiffEntry, 0, len(sorted))
	for _, p := range sorted {
		o, hasOld := flatOld[p]
		n, hasNew := flatNew[p]
		if hasOld && hasNew && o == n {
			continue
		}
		entry := ConfigDiffEntry{Path: p, Old: truncateDiffValue(o), New: truncateDiffValue(n)}
		if !hasOld {
			entry.Old = ""
		}
		if !hasNew {
			entry.New = ""
		}
		diff = append(diff, entry)
	}
	if len(diff) > diffLimit {
		diff = append(diff[:diffLimit], ConfigDiffEntry{
			Path: fmt.Sprintf("…共 %d 处变更", len(diff)),
		})
	}
	return diff
}

func parseConfigMap(canonical string) map[string]interface{} {
	m := map[string]interface{}{}
	if err := json.Unmarshal([]byte(canonical), &m); err != nil {
		return map[string]interface{}{}
	}
	return m
}

// versionSummaryJSON encodes the diff for storage; baselines store "[]".
func versionSummaryJSON(diff []ConfigDiffEntry) string {
	if diff == nil {
		diff = []ConfigDiffEntry{}
	}
	b, err := json.Marshal(diff)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// LatestConfigVersion returns the newest recorded version row for a strategy
// (gorm.ErrRecordNotFound when the strategy has no history yet).
func (s *StrategyStore) LatestConfigVersion(strategyID string) (*StrategyConfigVersion, error) {
	var row StrategyConfigVersion
	err := s.db.Where("strategy_id = ?", strategyID).
		Order("changed_at DESC, id DESC").First(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// ListConfigVersions returns all version rows oldest-first.
func (s *StrategyStore) ListConfigVersions(strategyID string) ([]*StrategyConfigVersion, error) {
	var rows []*StrategyConfigVersion
	err := s.db.Where("strategy_id = ?", strategyID).
		Order("changed_at ASC, id ASC").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// EnsureConfigVersionBaseline seeds version 1 from the CURRENT config when
// the strategy has no history yet — so existing strategies get a version
// anchor on their very first look at this feature (and create/duplicate
// record their starting point via RecordInitialVersion). No-op when any row
// already exists. source labels the anchor ("create"/"duplicate"/"baseline").
func (s *StrategyStore) EnsureConfigVersionBaseline(strategyID, configJSON, source string, changedAtMs int64) error {
	var count int64
	if err := s.db.Model(&StrategyConfigVersion{}).
		Where("strategy_id = ?", strategyID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	return s.RecordInitialVersion(strategyID, configJSON, source, changedAtMs)
}

// RecordInitialVersion unconditionally writes the first version row for a
// strategy (no dedup, no diff — Summary stays "[]").
func (s *StrategyStore) RecordInitialVersion(strategyID, configJSON, source string, changedAtMs int64) error {
	canonical := CanonicalConfigJSON(configJSON)
	if canonical == "{}" {
		return nil // nothing meaningful to anchor
	}
	if changedAtMs <= 0 {
		changedAtMs = time.Now().UTC().UnixMilli()
	}
	if source == "" {
		source = "baseline"
	}
	var parsed StrategyConfig
	_ = json.Unmarshal([]byte(canonical), &parsed)
	row := &StrategyConfigVersion{
		StrategyID: strategyID,
		ConfigHash: hashCanonical(canonical),
		RiskHash:   HashRiskControlOf(&parsed),
		Source:     source,
		Summary:    "[]",
		Config:     canonical,
		ChangedAt:  changedAtMs,
		CreatedAt:  time.Now().UTC().UnixMilli(),
	}
	return s.db.Create(row).Error
}

// RecordConfigChange appends a version row when newConfigJSON differs from
// the strategy's last recorded version. oldConfigJSON/oldChangedAtMs describe
// the config being replaced (used to seed the baseline when no history
// exists yet). Hash-deduped: re-recording an identical config is a no-op, so
// the API save path and the per-cycle drift detector can both call it.
func (s *StrategyStore) RecordConfigChange(strategyID, oldConfigJSON string, oldChangedAtMs int64, newConfigJSON, source string) error {
	canonicalNew := CanonicalConfigJSON(newConfigJSON)
	if canonicalNew == "{}" {
		return nil
	}
	newHash := hashCanonical(canonicalNew)

	latest, latestErr := s.LatestConfigVersion(strategyID)
	if latestErr == nil {
		if latest.ConfigHash == newHash {
			return nil // already recorded (save path + drift detector race)
		}
	} else if latestErr != gorm.ErrRecordNotFound {
		return latestErr
	}

	canonicalOld := CanonicalConfigJSON(oldConfigJSON)
	oldHash := hashCanonical(canonicalOld)
	if oldHash == newHash {
		return nil
	}

	var diff []ConfigDiffEntry
	var riskHash string
	if latestErr == nil {
		// Normal path: diff against the last recorded snapshot.
		diff = diffConfigs(parseConfigMap(latest.Config), parseConfigMap(canonicalNew))
	} else if canonicalOld != "{}" {
		// First-ever record with a known predecessor: anchor the predecessor
		// as the baseline, then diff old → new.
		if err := s.RecordInitialVersion(strategyID, oldConfigJSON, "baseline", oldChangedAtMs); err != nil {
			return fmt.Errorf("seed config baseline: %w", err)
		}
		diff = diffConfigs(parseConfigMap(canonicalOld), parseConfigMap(canonicalNew))
	} else {
		// No history and no known predecessor (direct DB write before any
		// baseline): the current state becomes the baseline.
		if err := s.RecordInitialVersion(strategyID, canonicalNew, "baseline", 0); err != nil {
			return fmt.Errorf("seed config baseline: %w", err)
		}
		return nil
	}

	var parsed StrategyConfig
	_ = json.Unmarshal([]byte(canonicalNew), &parsed)
	riskHash = HashRiskControlOf(&parsed)

	row := &StrategyConfigVersion{
		StrategyID: strategyID,
		ConfigHash: newHash,
		RiskHash:   riskHash,
		Source:     source,
		Summary:    versionSummaryJSON(diff),
		Config:     canonicalNew,
		ChangedAt:  time.Now().UTC().UnixMilli(),
		CreatedAt:  time.Now().UTC().UnixMilli(),
	}
	return s.db.Create(row).Error
}

// StatsForWindow aggregates journal trades whose ENTRY time falls inside
// [fromMs, toMs) (toMs = 0 → open-ended) across the given traders.
func (s *TradeJournalStore) StatsForWindow(traderIDs []string, fromMs, toMs int64) (*VersionStats, error) {
	stats := &VersionStats{}
	if len(traderIDs) == 0 {
		return stats, nil
	}
	q := s.db.Model(&TradeJournalDB{}).Where("trader_id IN ?", traderIDs)
	if fromMs > 0 {
		q = q.Where("entry_time >= ?", fromMs)
	}
	if toMs > 0 {
		q = q.Where("entry_time < ?", toMs)
	}
	var entries []*TradeJournalDB
	if err := q.Find(&entries).Error; err != nil {
		return nil, err
	}

	var grossWin, grossLoss, rSum float64
	for _, e := range entries {
		stats.Trades++
		net := e.RealizedPnL - e.Fee
		stats.NetPnL += net
		switch {
		case net > 0:
			stats.Wins++
			grossWin += net
		case net < 0:
			stats.Losses++
			grossLoss += -net
		}
		// R = net PnL ÷ opening risk, when the planned stop allows computing it.
		if risk := absF(e.EntryPrice-e.PlannedStopLoss) * e.Quantity; risk > 0 && e.PlannedStopLoss > 0 {
			stats.RCount++
			rSum += net / risk
		}
	}
	if stats.Trades > 0 {
		stats.WinRate = float64(stats.Wins) / float64(stats.Trades) * 100
	}
	if stats.Wins > 0 {
		stats.AvgWin = grossWin / float64(stats.Wins)
	}
	if stats.Losses > 0 {
		stats.AvgLoss = grossLoss / float64(stats.Losses)
	}
	if grossLoss > 0 {
		stats.ProfitFactor = grossWin / grossLoss
	}
	if stats.RCount > 0 {
		stats.AvgR = rSum / float64(stats.RCount)
	}
	return stats, nil
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
