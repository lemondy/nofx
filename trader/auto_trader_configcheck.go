package trader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"nofx/logger"
	"nofx/store"
	notify "nofx/telegram/notify"
	"time"
)

// ============================================================================
// Config-drift self-check (user directive 2026-09-15, after the
// sl_min_atr_mult revert incident): the strategy config can be rewritten in
// the DB (UI save with a stale in-memory form) while this process keeps
// enforcing what it LOADED at start — nothing used to notice. Now every
// cycle re-hashes the DB row's risk_control and compares it against the
// hash of what this process loaded; a mismatch alerts once per window, and
// every decision record is stamped with the hash that was actually in force
// so post-mortems can tell which parameter set produced each trade.
// ============================================================================

// RiskControlHash hashes the risk_control block canonically (Go struct
// marshaling is field-order deterministic). Empty when rc is nil.
func RiskControlHash(rc *store.RiskControlConfig) string {
	if rc == nil {
		return ""
	}
	b, err := json.Marshal(rc)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// checkConfigDrift compares the strategy row currently in the DB against the
// config this process loaded at start. Mismatch means SOMETHING (usually a
// UI save) changed the persisted config after this process started — the
// process is now enforcing parameters nobody can see in the UI, or the UI
// reverted a backend-side change. Alert deduped; a mismatch never blocks
// trading (the loaded config stays authoritative for this process).
func (at *AutoTrader) checkConfigDrift() {
	if at.store == nil || at.strategyID == "" || at.loadedConfigHash == "" {
		return
	}
	strategy, err := at.store.Strategy().Get(at.userID, at.strategyID)
	if err != nil || strategy == nil {
		return // row gone/unreadable — manager-level concern, not a drift signal
	}
	cfg, err := strategy.ParseConfig()
	if err != nil || cfg == nil {
		return
	}
	dbHash := RiskControlHash(&cfg.RiskControl)
	if dbHash == at.loadedConfigHash {
		return
	}
	// Version history (2026-10-04): a drift means the config changed outside
	// the UI save path (backend script / raw SQL / another writer). Record it
	// so the version timeline shows WHO-WROTE-WHAT-WHEN even for writes that
	// never went through the API. Hash-deduped; best-effort.
	if loadedJSON, err := json.Marshal(at.config.StrategyConfig); err == nil {
		if err := at.store.Strategy().RecordConfigChange(at.strategyID, string(loadedJSON), 0, strategy.Config, "external"); err != nil {
			logger.Warnf("⚠️ [%s] failed to record external config version: %v", at.name, err)
		}
	}
	_, push := at.gateNotifyRecord("cfgdrift", time.Now())
	logger.Warnf("⚠️ [%s] CONFIG DRIFT: strategy %s risk_control in the DB differs from what this process loaded at start (db %s ≠ loaded %s) — the process keeps enforcing its loaded values until restarted",
		at.name, at.strategyID, dbHash, at.loadedConfigHash)
	if push {
		notify.Notify("ALERT", at.name, fmt.Sprintf(
			"<b>⚠️ 策略配置漂移</b>\n<i>数据库中的 risk_control 已与进程启动时加载的配置不一致(进程继续用启动时的旧值)——通常是策略页保存或后端改库所致。确认期望值后重启进程以加载新配置(30 分钟去重)</i>"))
	}
}

// seedConfigVersionBaseline anchors version 1 of the strategy's config
// history when the trader starts (2026-10-07 review: the history only began
// when someone opened the versions page, so the config a trader actually ran
// under before that had no row and its trades no version to attribute to).
// No-op once any version exists; best-effort.
func (at *AutoTrader) seedConfigVersionBaseline() {
	if at.store == nil || at.strategyID == "" {
		return
	}
	strategy, err := at.store.Strategy().Get(at.userID, at.strategyID)
	if err != nil || strategy == nil {
		return
	}
	if err := at.store.Strategy().EnsureConfigVersionBaseline(at.strategyID, strategy.Config, "baseline", strategy.UpdatedAt.UnixMilli()); err != nil {
		logger.Warnf("⚠️ [%s] config version baseline failed for %s: %v", at.name, at.strategyID, err)
	}
}
