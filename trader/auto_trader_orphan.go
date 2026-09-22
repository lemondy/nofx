package trader

import (
	"fmt"
	"nofx/market"
	notify "nofx/telegram/notify"
	"strings"
	"time"

	"nofx/logger"
	"nofx/store"
)

// ============================================================================
// Orphaned position-row reconciliation (2026-09-21)
//
// In one-way margin mode a position can vanish from the exchange WITHOUT a
// single close attributed to it: a later opposite-side open on the same
// symbol nets it away, and every netting fill is booked to the OTHER side's
// row. BTWUSDT: a SHORT row stayed OPEN for two days while the exchange was
// flat, because the netting BUY (and the sells after it) were all recorded
// as open_long/close_long of the successor LONG position.
//
// Impact is reporting-only — decisions and the protection watchdog read the
// exchange live (GetPositions), never these rows — but the phantom row
// pollutes the positions page and the trade stats. This guard runs once per
// cycle and closes DB OPEN rows whose (symbol, side) the exchange no longer
// holds.
//
// Bookkeeping is deliberately conservative: the row's already-accumulated
// realized PnL and fees are kept as-is (the netting PnL is attributed inside
// the fills of the netting orders and is not re-derivable per-row here), and
// the exit is stamped with the live ticker price purely as a marker.
// ============================================================================

// orphanPositionMinAge guards against racing the open path and the
// positions cache: a row younger than this is never auto-closed, so a
// just-opened position (or a stale GetPositions cache snapshot) can never
// be mistaken for an orphan.
const orphanPositionMinAge = 30 * time.Minute

// orphanedRows returns the DB open rows whose (symbol, side) key is absent
// from the exchange's live position set and whose age exceeds minAge. Side
// comparison is case-insensitive; a zero EntryTime falls back to UpdatedAt.
func orphanedRows(rows []*store.TraderPosition, live map[string]bool, now time.Time, minAge time.Duration) []*store.TraderPosition {
	var orphans []*store.TraderPosition
	for _, row := range rows {
		key := market.Normalize(row.Symbol) + "_" + strings.ToLower(row.Side)
		if live[key] {
			continue
		}
		openedMs := row.EntryTime
		if openedMs <= 0 {
			openedMs = row.UpdatedAt
		}
		if openedMs <= 0 || now.Sub(time.UnixMilli(openedMs)) < minAge {
			continue
		}
		orphans = append(orphans, row)
	}
	return orphans
}

// reconcileOrphanedPositionRows is the per-cycle entry point (called from
// runCycle, same goroutine as every other bookkeeping pass — no locking).
// Any failure mode is fail-open: a blind or half-blind cycle closes nothing.
func (at *AutoTrader) reconcileOrphanedPositionRows() {
	if at.store == nil || at.config.StrategyConfig == nil || at.config.StrategyConfig.StrategyType == "grid_trading" {
		return // grid keeps its own position books
	}
	rows, err := at.store.Position().GetOpenPositions(at.id)
	if err != nil || len(rows) == 0 {
		return
	}
	exchangePositions, err := at.trader.GetPositions()
	if err != nil {
		return // can't see the exchange — close nothing this cycle
	}
	live := make(map[string]bool, len(exchangePositions))
	leverageOf := make(map[string]int, len(exchangePositions))
	for _, p := range exchangePositions {
		symbol, _ := p["symbol"].(string)
		side, _ := p["side"].(string)
		if symbol == "" || side == "" {
			continue
		}
		key := market.Normalize(symbol) + "_" + side
		live[key] = true
		if lev, ok := p["leverage"].(float64); ok && lev > 0 {
			leverageOf[key] = int(lev)
		}
	}
	// D3 (QUANT_REVIEW 2026-09-22): backfill the real exchange leverage onto
	// OPEN rows. The sync path hardcodes 1 (fills carry no leverage), so the
	// journal's margin/ROI calibers — and every statistic derived from them
	// — ran on a false 1x. Runs before the orphan pass; fail-open per row.
	for _, row := range rows {
		key := market.Normalize(row.Symbol) + "_" + strings.ToLower(row.Side)
		lev := leverageOf[key]
		if lev <= 0 || row.Leverage == lev {
			continue
		}
		if err := at.store.Position().UpdatePositionLeverage(row.ID, lev); err != nil {
			logger.Infof("⚠️ [%s] leverage backfill: %s %s row #%d → %dx failed: %v", at.name, row.Symbol, row.Side, row.ID, lev, err)
			continue
		}
		logger.Infof("🔧 [%s] leverage backfill: %s %s row #%d corrected 1x→%dx (journal margin/ROI calibers now honest)", at.name, row.Symbol, row.Side, row.ID, lev)
	}
	now := time.Now()
	for _, row := range orphanedRows(rows, live, now, orphanPositionMinAge) {
		exitPrice := 0.0
		if p, err := market.NewAPIClient().GetCurrentPrice(row.Symbol); err == nil && p > 0 {
			exitPrice = p
		}
		if exitPrice <= 0 {
			// No honest exit price this cycle (xyz dex assets, ticker
			// hiccup, delisting) — retry next cycle rather than stamp 0.
			logger.Infof("🧹 [%s] orphan reconcile: %s %s row #%d has no live price — retry next cycle", at.name, row.Symbol, row.Side, row.ID)
			continue
		}
		if err := at.store.Position().ClosePositionFully(row.ID, exitPrice, "", now.UnixMilli(), row.RealizedPnL, row.Fee, "netting_reconcile"); err != nil {
			logger.Infof("⚠️ [%s] orphan reconcile: closing %s %s row #%d failed: %v", at.name, row.Symbol, row.Side, row.ID, err)
			continue
		}
		logger.Infof("🧹 [%s] orphan reconcile: %s %s row #%d closed (exchange holds no such position — one-way netting or manual close; entry %.6g qty %.6g, exit stamped %.6g, kept accumulated PnL %.4f)",
			at.name, row.Symbol, row.Side, row.ID, row.EntryPrice, row.Quantity, exitPrice, row.RealizedPnL)
		notify.Notify("RISK", at.name, fmt.Sprintf(
			"<b>🧹 幽灵仓位行收口 %s (%s)</b>\n<i>交易所已无此仓位(单向模式净额对冲或手动平仓),DB 行已自动关闭,累计盈亏保持原归因不变</i>",
			notify.Escape(row.Symbol), strings.ToUpper(row.Side[:1])+row.Side[1:]))
	}
}
