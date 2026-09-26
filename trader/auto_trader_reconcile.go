package trader

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"nofx/kernel"
	"nofx/logger"
	"nofx/store"
	notify "nofx/telegram/notify"
	"nofx/trader/binance"
	"nofx/trader/types"
	"strings"
	"time"
)

// ============================================================================
// Startup reconciliation for limit-entry orders
//
// pendingEntries is memory-only; a restart loses ownership of any resting
// GTC limit entry: nobody manages its expiry and a fill would go unprotected
// (SL/TP are placed by the pending lifecycle on FILLED). Three mechanisms
// close that hole:
//
//  1. Write-through shadow rows (store.PendingEntryDB) — set/drop mirror the
//     in-memory map on every transition.
//  2. On-exchange class tags — limit entries are placed with a "lim-" client
//     order ID (getBrOrderIDFor), so the exchange itself says which resting
//     LIMIT orders belong to this subsystem.
//  3. ReconcilePendingEntries on startup — shadow rows re-claim live orders;
//     anything left tagged "lim-" but unclaimed is an orphan and gets
//     cancelled; untagged orders (grid, protective, pre-tag legacy) are NEVER
//     touched.
// ============================================================================

// entryTagPrefix marks AI limit-ENTRY orders inside the client order ID
// (binance.getBrOrderIDFor carries it after the broker referral prefix).
// Grid orders, protective algo orders, and everything placed before this tag
// existed carry no entry tag — reconciliation treats those as unowned and
// never cancels them.
const entryTagPrefix = "lim-"

// entryClientID builds the per-order client ID tag: "lim-<hash4>-<ms>"
// (exactly 22 chars — the space left by the broker prefix inside Binance's
// 32-char cap). The 4-hex trader hash disambiguates multiple traders sharing
// one exchange account — a restart of trader A must not claim trader B's
// order. getBrOrderIDFor truncates from the right, so the "lim-<hash>" head
// always survives.
func (at *AutoTrader) entryClientID() string {
	return fmt.Sprintf("lim-%s-%d", traderHash4(at.id), time.Now().UnixMilli())
}

// traderHash4 is the stable 4-hex trader fingerprint embedded in entry tags.
func traderHash4(traderID string) string {
	sum := sha256.Sum256([]byte(traderID))
	return hex.EncodeToString(sum[:2])
}

// entryOrderOwnedBy reports whether an exchange client order ID carries the
// entry tag AND this trader's hash. The ID arrives in the full on-exchange
// form "<broker prefix>lim-<hash4>-<ms>" (binance.BrIDPrefix) — strip that
// first; empty/untagged IDs (pre-tag legacy, grid, protective) → false, never
// ours to judge.
func entryOrderOwnedBy(clientID, traderID string) bool {
	tag := strings.TrimPrefix(clientID, binance.BrIDPrefix)
	if tag == clientID || !strings.HasPrefix(tag, entryTagPrefix) {
		return false
	}
	rest := tag[len(entryTagPrefix):]
	return len(rest) >= 5 && rest[:4] == traderHash4(traderID) && rest[4] == '-'
}

// ReconcilePendingEntries runs once at trader startup, after pendingEntries
// maps exist and before the first decision cycle. Steps:
//
//  1. shadow rows → re-claim: verify each persisted order on the exchange;
//     still resting → back into pendingEntries; gone (filled while offline /
//     cancelled externally / expired) → resolve accordingly (fill = place
//     protection NOW; cancel = drop row).
//  2. exchange scan → orphan sweep: open LIMIT orders tagged lim-<this
//     trader> with no shadow row and no map entry are unowned leftovers
//     (pre-shadow era or a failed write-through) — cancelled for
//     re-evaluation. Untagged orders are left alone.
func (at *AutoTrader) ReconcilePendingEntries() {
	if at.exchange != "binance" || at.store == nil {
		return
	}
	grid, ok := at.trader.(interface {
		GetOpenOrders(symbol string) ([]types.OpenOrder, error)
		CancelOrder(symbol, orderID string) error
	})
	if !ok {
		return
	}

	logger.Infof("🔧 [%s] Pending-entry reconciliation: checking shadow rows + open orders", at.name)

	// owned tracks every order with a legitimate claim: map entries (re-claimed
	// or placed this session) plus rows we couldn't verify. The orphan sweep
	// only cancels tagged orders absent from here.
	owned := map[string]bool{}
	at.pendingEntriesMu.RLock()
	for _, pe := range at.pendingEntries {
		owned[pe.Symbol+"|"+pe.OrderID] = true
	}
	at.pendingEntriesMu.RUnlock()

	// --- Step 1: re-claim from shadow rows ------------------------------
	rows, err := at.store.PendingEntry().List(at.id)
	if err != nil {
		logger.Infof("⚠️ [%s] Pending-entry reconciliation: shadow row list failed: %v", at.name, err)
	}
	for _, row := range rows {
		status, err := at.trader.GetOrderStatus(row.Symbol, row.OrderID)
		if err != nil {
			// Unknowable state (network/auth). Keep the row AND count the
			// order as owned — the orphan sweep below would otherwise cancel
			// an order whose status we merely failed to read.
			logger.Infof("⚠️ [%s] Pending-entry %s (order %s) status check failed: %v — row kept, order treated as owned",
				at.name, row.Symbol, row.OrderID, err)
			owned[row.Symbol+"|"+row.OrderID] = true
			continue
		}
		st, _ := status["status"].(string)
		switch strings.ToUpper(st) {
		case "NEW", "PARTIALLY_FILLED":
			pe := &pendingEntry{
				Symbol: row.Symbol, Side: row.Side, Price: row.Price,
				Quantity: row.Quantity, StopLoss: row.StopLoss, TakeProfit: row.TakeProfit,
				Leverage: row.Leverage, OrderID: row.OrderID, PlacedAt: row.PlacedAt,
			}
			at.setPendingEntry(pe)
			owned[row.Symbol+"|"+row.OrderID] = true
			logger.Infof("🔧 [%s] Pending-entry re-claimed: %s %s %.6g @ %.6g (order %s)", at.name, pe.Symbol, pe.Side, pe.Quantity, pe.Price, pe.OrderID)
		case "FILLED":
			at.finalizePendingFill(row)
		default: // CANCELED / EXPIRED / REJECTED
			if err := at.store.PendingEntry().Delete(at.id, row.Symbol, row.Side); err != nil {
				logger.Infof("⚠️ [%s] Pending-entry %s: shadow row delete failed: %v", at.name, row.Symbol, err)
			}
			logger.Infof("🔧 [%s] Pending-entry %s (order %s) already %s while offline — row dropped", at.name, row.Symbol, row.OrderID, st)
		}
	}

	// --- Step 2: orphan sweep on the exchange ---------------------------
	// A tagged open order with no legitimate claim is unowned.
	open, err := grid.GetOpenOrders("")
	if err != nil {
		logger.Infof("⚠️ [%s] Pending-entry reconciliation: open-order scan failed: %v", at.name, err)
		return
	}
	for _, o := range open {
		if !entryOrderOwnedBy(o.ClientID, at.id) {
			continue // grid, protective, legacy — not ours to judge
		}
		if owned[o.Symbol+"|"+o.OrderID] {
			continue
		}
		if err := grid.CancelOrder(o.Symbol, o.OrderID); err != nil {
			logger.Infof("⚠️ [%s] Orphan limit entry %s (order %s) cancel failed: %v", at.name, o.Symbol, o.OrderID, err)
			continue
		}
		logger.Infof("🔧 [%s] Orphan limit entry cancelled: %s (order %s, client %s) — no owner after restart, re-evaluated next cycle",
			at.name, o.Symbol, o.OrderID, o.ClientID)
		notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🔧 孤儿限价单已撤销 %s</b>\n<i>重启后无主(order %s),已撤单重评</i>", notify.Escape(o.Symbol), o.OrderID))
	}
}

// finalizePendingFill resolves a shadow row whose order filled while the
// process was down: drop the row and place protective orders anchored at the
// limit price — the exact price the risk gates validated at.
func (at *AutoTrader) finalizePendingFill(row *store.PendingEntryDB) {
	// (R6: the AI-managed mark below is written BEFORE this row delete in
	// the flow — ownership persists even if the delete fails.)
	if err := at.store.PendingEntry().Delete(at.id, row.Symbol, row.Side); err != nil {
		logger.Infof("⚠️ [%s] Pending-entry %s: shadow row delete failed: %v", at.name, row.Symbol, err)
	}
	// Position may have been closed externally in the offline window; check
	// before placing protection, otherwise the protective orders would open a
	// position out of thin air (reduce-only semantics differ per exchange).
	positions, err := at.trader.GetPositions()
	if err != nil {
		logger.Infof("⚠️ [%s] Pending-entry %s FILLED while offline: position check failed: %v — protection NOT placed, manual check needed",
			at.name, row.Symbol, err)
		notify.Notify("RISK", at.name, fmt.Sprintf("<b>⚠️ %s 限价单离线期间成交</b>\n<i>持仓校验失败(%v),保护单未挂——请人工核查</i>", notify.Escape(row.Symbol), err))
		return
	}
	posSide := "long"
	if row.Side == "short" {
		posSide = "short"
	}
	found := false
	for _, pos := range positions {
		if pos["symbol"] == row.Symbol && pos["side"] == posSide {
			found = true
			break
		}
	}
	if !found {
		logger.Infof("🔧 [%s] Pending-entry %s FILLED while offline but no %s position exists now (closed externally?) — row dropped, no protection placed",
			at.name, row.Symbol, posSide)
		return
	}
	// R6 (2026-09-26 review): the offline fill IS an AI fill (this is the
	// AI's own pending entry) — mark ownership BEFORE deleting the pending
	// row, so the hands-off watchdog can never classify it manual later.
	at.markAIManaged(row.Symbol, row.Side)
	posKey := row.Symbol + "_" + row.Side
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()
	at.SetRecordedStopLoss(row.Symbol, row.Side, row.StopLoss)
	at.SetInitialStopLoss(row.Symbol, row.Side, row.StopLoss) // 1R anchor — write-once
	at.ClearPeakPnLCache(row.Symbol, row.Side)
	positionSide := "LONG"
	if row.Side == "short" {
		positionSide = "SHORT"
	}
	at.placeProtectiveOrders(&kernel.Decision{
		Symbol: row.Symbol, Action: "open_" + row.Side,
		StopLoss: row.StopLoss, TakeProfit: row.TakeProfit,
	}, positionSide, row.Quantity, row.Price, row.Price)
	logger.Infof("✅ [%s] Pending-entry %s FILLED while offline: protective orders placed at limit price %.6g (SL %.6g / TP %.6g)",
		at.name, row.Symbol, row.Price, row.StopLoss, row.TakeProfit)
	notify.Notify("ORDER", at.name, fmt.Sprintf("<b>📌 限价入场离线成交 %s</b>\n<i>%s @ %.6g,保护单已补挂</i>", notify.Escape(row.Symbol), row.Side, row.Price))
}
