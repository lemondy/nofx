package trader

import (
	"fmt"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	notify "nofx/telegram/notify"
	"nofx/trader/types"
	"strings"
	"time"
)

// pendingEntry tracks one placed limit-entry order through its lifecycle:
// placed → filled (SL/TP placed at the exact limit price) | expired (N cycles
// unfilled → cancelled) | invalidated (price crossed the SL before entry) |
// replaced (a newer decision for the same symbol).
type pendingEntry struct {
	Symbol     string
	Side       string // "long" / "short"
	Price      float64
	Quantity   float64
	StopLoss   float64
	TakeProfit float64
	Leverage   int
	OrderID    string
	PlacedAt   time.Time
	Cycles     int
}

func (at *AutoTrader) setPendingEntry(pe *pendingEntry) {
	at.pendingEntriesMu.Lock()
	defer at.pendingEntriesMu.Unlock()
	if at.pendingEntries == nil {
		at.pendingEntries = make(map[string]*pendingEntry)
	}
	at.pendingEntries[pe.Symbol] = pe
	at.persistPendingEntry(pe)
}

func (at *AutoTrader) dropPendingEntry(symbol string) {
	at.pendingEntriesMu.Lock()
	delete(at.pendingEntries, symbol)
	at.pendingEntriesMu.Unlock()
	// The shadow row must die with the map state — a surviving row would make
	// the next startup re-claim an order that is already gone.
	if at.store != nil {
		if err := at.store.PendingEntry().Delete(at.id, symbol); err != nil {
			logger.Infof("⚠️ [%s] drop pending entry %s: shadow row delete failed: %v", at.name, symbol, err)
		}
	}
}

// persistPendingEntry is the write-through half of the pending state: the row
// is what a restart uses to re-claim the resting order (expiry management +
// protective placement on fill). A failed write means the entry could rest
// unowned after a restart — the startup tag-scan catches it as a fallback.
func (at *AutoTrader) persistPendingEntry(pe *pendingEntry) {
	if at.store == nil {
		return
	}
	err := at.store.PendingEntry().Upsert(&store.PendingEntryDB{
		TraderID:   at.id,
		Symbol:     pe.Symbol,
		Side:       pe.Side,
		Price:      pe.Price,
		Quantity:   pe.Quantity,
		StopLoss:   pe.StopLoss,
		TakeProfit: pe.TakeProfit,
		Leverage:   pe.Leverage,
		OrderID:    pe.OrderID,
		PlacedAt:   pe.PlacedAt,
	})
	if err != nil {
		logger.Infof("⚠️ [%s] persist pending entry %s (order %s) failed: %v — restart would orphan it until the tag-scan drops it",
			at.name, pe.Symbol, pe.OrderID, err)
	}
}

func (at *AutoTrader) getPendingEntry(symbol string) *pendingEntry {
	at.pendingEntriesMu.RLock()
	defer at.pendingEntriesMu.RUnlock()
	return at.pendingEntries[symbol]
}

// validateLimitEntryPrice sanity-checks the trigger price against the live
// market: buy limits sit below (pullback), sell limits above (rally), within a
// 0.1%-5% band — outside it the order is either a disguised market order or a
// stale far-away one.
func validateLimitEntryPrice(action string, limitPrice, livePrice float64) error {
	if limitPrice <= 0 || livePrice <= 0 {
		return fmt.Errorf("invalid prices: limit %.6g live %.6g", limitPrice, livePrice)
	}
	var distPct float64
	if action == "open_long_limit" {
		distPct = (livePrice - limitPrice) / livePrice * 100
	} else {
		distPct = (limitPrice - livePrice) / livePrice * 100
	}
	if distPct < 0.1 {
		return fmt.Errorf("trigger price %.6g is at/above market %.6g — use the market action for immediate entries", limitPrice, livePrice)
	}
	if distPct > 5 {
		return fmt.Errorf("trigger price %.6g is %.1f%% away from market %.6g — too far, re-evaluate when price approaches", limitPrice, distPct, livePrice)
	}
	return nil
}

// limitEntryTooFar reports the stale far-away half of the trigger band: the
// anchor sits more than 5% from market on the entry side. An uncrossed
// far-away anchor must be rejected, not parked — it can only fill after an
// un-modeled 5%+ move in the entry direction, i.e. the plan is already wrong.
func limitEntryTooFar(action string, limitPrice, livePrice float64) bool {
	if limitPrice <= 0 || livePrice <= 0 {
		return true // invalid prices: nothing placeable to park
	}
	var distPct float64
	if action == "open_long_limit" {
		distPct = (livePrice - limitPrice) / livePrice * 100
	} else {
		distPct = (limitPrice - livePrice) / livePrice * 100
	}
	return distPct > 5
}

// limitEntryMarketFallbackEnabled reports whether a limit entry whose anchor
// has been crossed by the live price converts to a market entry. Default on:
// the anchor is pre-computed at prompt-build time and the AI latency window
// (minutes) lets price drift through it — a crossed anchor means the planned
// pullback/rally already arrived, which is exactly when the order was meant
// to fill, not a reason to reject.
func (at *AutoTrader) limitEntryMarketFallbackEnabled() bool {
	if at.config.StrategyConfig == nil || at.config.StrategyConfig.RiskControl.LimitEntryMarketFallback == nil {
		return true
	}
	return *at.config.StrategyConfig.RiskControl.LimitEntryMarketFallback
}

// anchorCrossedByMarket reports whether the live price has crossed the limit
// anchor from the entry side: long anchor at/above live (the pullback came in
// deeper than planned), short anchor at/below live. slCrossed additionally
// flags the setup as dead — the market traded past the planned stop before
// the entry could fill. A zero SL can't be crossed; the missing-SL rule is
// validateOpenRisk's, not this check's.
func anchorCrossedByMarket(side string, anchorPrice, livePrice, stopLoss float64) (crossed, slCrossed bool) {
	if stopLoss <= 0 {
		return false, false
	}
	if side == "long" {
		crossed = anchorPrice >= livePrice
		slCrossed = crossed && livePrice <= stopLoss
	} else {
		crossed = anchorPrice <= livePrice
		slCrossed = crossed && livePrice >= stopLoss
	}
	return crossed, slCrossed
}

// executeOpenLimitLongWithRecord places a GTC limit entry and registers the
// pending state. All risk gates anchor at the LIMIT price — the exact price
// the order fills at, eliminating market-order RR skew.
func (at *AutoTrader) executeOpenLimitLongWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	return at.executeOpenLimit(decision, actionRecord, "long")
}

func (at *AutoTrader) executeOpenLimitShortWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	return at.executeOpenLimit(decision, actionRecord, "short")
}

func (at *AutoTrader) executeOpenLimit(decision *kernel.Decision, actionRecord *store.DecisionAction, side string) error {
	logger.Infof("  📌 Open %s (LIMIT): %s @ %.6g", side, decision.Symbol, decision.Price)

	grid, ok := at.trader.(interface {
		PlaceLimitOrder(req *types.LimitOrderRequest) (*types.LimitOrderResult, error)
		CancelOrder(symbol, orderID string) error
	})
	if !ok {
		return fmt.Errorf("exchange %s does not support limit entries", at.exchange)
	}

	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}
	// Slot accounting: open positions + resting limit entries on OTHER
	// symbols + this order. Resting entries must occupy slots too, otherwise
	// several unfilled limits could all fill into more positions than the cap.
	if err := at.enforceMaxPositions(nextSlotCount(len(positions), at.snapshotPendingSymbols(), decision.Symbol)); err != nil {
		return err
	}
	posSide := "long"
	if side == "short" {
		posSide = "short"
	}
	for _, pos := range positions {
		if pos["symbol"] == decision.Symbol && pos["side"] == posSide {
			return fmt.Errorf("❌ %s already has %s position", decision.Symbol, posSide)
		}
	}

	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return err
	}
	livePrice := marketData.CurrentPrice
	at.stampEntryPath(actionRecord, marketData)

	// Spread gate: a wide book eats the anchor edge — the limit would sit
	// inside the spread and never fill at value.
	if blocked, reason := at.spreadBlocksOpen(decision.Symbol); blocked {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %s", decision.Action, decision.Symbol, reason)
	}

	// Trigger price sanity: pullback/rally band. When the live price has
	// already crossed the anchor (the AI latency window moved the market
	// through the pre-computed trigger), the pullback/rally the anchor was
	// waiting for has arrived — convert to a market entry instead of
	// rejecting, unless the stop was crossed too (setup dead) or the
	// fallback is disabled. An uncrossed failure only survives when the
	// anchor is a tick from market (the pullback is on its final leg and the
	// GTC fills at once); a far-away anchor is stale and is rejected here —
	// the old fall-through placed it anyway (review 2026-09-09).
	if limitErr := validateLimitEntryPrice("open_"+side+"_limit", decision.Price, livePrice); limitErr != nil {
		crossed, slCrossed := anchorCrossedByMarket(side, decision.Price, livePrice, decision.StopLoss)
		if crossed {
			if slCrossed {
				return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: anchor %.6g already crossed (live %.6g) and price is at/beyond SL %.6g — setup invalidated",
					decision.Action, decision.Symbol, decision.Price, livePrice, decision.StopLoss)
			}
			if at.limitEntryMarketFallbackEnabled() {
				logger.Infof("  🔄 %s anchor %.6g crossed by market %.6g — converting to market entry", decision.Symbol, decision.Price, livePrice)
				// The market paths don't manage pending state (only the
				// open_long/open_short dispatch does), so drop a replaced
				// resting order here — otherwise it would survive next to
				// the fresh market position.
				if old := at.getPendingEntry(decision.Symbol); old != nil {
					_ = grid.CancelOrder(old.Symbol, old.OrderID)
					at.dropPendingEntry(old.Symbol)
					logger.Infof("  🔄 Cancelled replaced pending limit entry for %s (@ %.6g)", old.Symbol, old.Price)
				}
				if side == "long" {
					return at.executeOpenLongWithRecord(decision, actionRecord)
				}
				return at.executeOpenShortWithRecord(decision, actionRecord)
			}
			return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %v", decision.Action, decision.Symbol, limitErr)
		}
		if limitEntryTooFar("open_"+side+"_limit", decision.Price, livePrice) {
			return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %v", decision.Action, decision.Symbol, limitErr)
		}
	}

	// Supply-zone gate: an anchor parked right at the opposite-side structure
	// (below resistance for longs / above support for shorts) has no room —
	// reject so the model re-anchors or skips the setup.
	if at.config.StrategyConfig != nil && at.config.StrategyConfig.RiskControl.OpenRejectSupplyPct > 0 {
		rc := at.config.StrategyConfig.RiskControl
		offsetCfg := kernel.AnchorOffsetFromRiskControl(&rc)
		sig, sigErr := kernel.ComputeSymbolSignals(decision.Symbol, marketData, kernel.SignalOptions{
			Now:                     time.Now(),
			PrimaryTF:               "15m",
			CurrentPrice:            livePrice,
			LimitEntryOffsetPct:     rc.LimitEntryOffsetPct,
			LimitEntryOffsetMode:    rc.LimitEntryOffsetMode,
			LimitEntryOffsetATRMult: rc.LimitEntryOffsetATRMult,
			LimitEntryOffsetMinPct:  rc.LimitEntryOffsetMinPct,
			LimitEntryOffsetMaxPct:  rc.LimitEntryOffsetMaxPct,
		})
		if sigErr == nil && sig != nil {
			// Same execution-TF-scaled breathing threshold the prompt-side
			// suppression used (the fill must not land right under a ceiling)
			// — recomputed on fresh data so the gate and the pre-filter can
			// never disagree about what "no room" means.
			threshold := kernel.AnchorBreathingPct(sig.ExecutionATRPct(), offsetCfg, rc.OpenRejectSupplyPct)
			if blocked, reason := entrySupplyZoneBlocks(sig, side, decision.Price, threshold); blocked {
				return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %s", decision.Action, decision.Symbol, reason)
			}
		}
	}

	// All risk gates anchor at the LIMIT price — the exact fill price.
	if err := at.validateOpenRisk(decision, decision.Price, oneHourATRPct(marketData), fourHourATRPct(marketData)); err != nil {
		return err
	}

	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	equity := 0.0
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		equity = eq
	} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
		equity = eq
	} else if avail, ok := balance["availableBalance"].(float64); ok {
		equity = avail
	}

	adjusted, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
	if wasCapped {
		decision.PositionSizeUSD = adjusted
	}
	decision.PositionSizeUSD = at.clampSizeToRisk(decision, decision.PositionSizeUSD, equity, decision.Price)
	if err := at.enforceMinPositionSize(decision.PositionSizeUSD); err != nil {
		return err
	}

	// Margin-budget gate: (used + new) margin ≤ max_margin_usage × equity —
	// the prompt states the budget, this enforces it (audit 09-13 #2).
	if blocked, reason := at.marginBudgetBlocksOpen(decision.Symbol, decision.PositionSizeUSD, float64(decision.Leverage), equity); blocked {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %s", decision.Action, decision.Symbol, reason)
	}
	quantity := decision.PositionSizeUSD / decision.Price
	actionRecord.Quantity = quantity
	actionRecord.Price = decision.Price

	if err := at.trader.SetMarginMode(decision.Symbol, at.config.IsCrossMargin); err != nil {
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
	}

	// Replace any pending entry for this symbol (new decision supersedes).
	if old := at.getPendingEntry(decision.Symbol); old != nil {
		_ = grid.CancelOrder(old.Symbol, old.OrderID)
		at.dropPendingEntry(old.Symbol)
		logger.Infof("  🔄 Replaced pending limit entry for %s (@ %.6g)", old.Symbol, old.Price)
	}

	sideStr := "BUY"
	posSideStr := "LONG"
	if side == "short" {
		sideStr, posSideStr = "SELL", "SHORT"
	}
	res, err := grid.PlaceLimitOrder(&types.LimitOrderRequest{
		Symbol:       decision.Symbol,
		Side:         sideStr,
		PositionSide: posSideStr,
		Price:        decision.Price,
		Quantity:     quantity,
		Leverage:     decision.Leverage,
		ClientID:     at.entryClientID(),
	})
	if err != nil {
		return fmt.Errorf("failed to place limit entry: %w", err)
	}

	at.setPendingEntry(&pendingEntry{
		Symbol: decision.Symbol, Side: side, Price: decision.Price,
		Quantity: quantity, StopLoss: decision.StopLoss, TakeProfit: decision.TakeProfit,
		Leverage: decision.Leverage, OrderID: res.OrderID, PlacedAt: time.Now(),
	})
	actionRecord.OrderID = 0 // string order id lives in the pending state
	logger.Infof("  ✓ Limit entry placed: %s %s %.6g @ %.6g (order %s), SL %.6g / TP %.6g",
		decision.Symbol, side, quantity, decision.Price, res.OrderID, decision.StopLoss, decision.TakeProfit)
	return nil
}

// snapshotPendingSymbols returns the symbols currently holding a resting
// limit entry in the pending map (copy under lock).
func (at *AutoTrader) snapshotPendingSymbols() []string {
	at.pendingEntriesMu.RLock()
	defer at.pendingEntriesMu.RUnlock()
	syms := make([]string, 0, len(at.pendingEntries))
	for sym := range at.pendingEntries {
		syms = append(syms, sym)
	}
	return syms
}

// nextSlotCount computes the position-slot count AFTER placing a new entry
// for symbol: open positions + resting entries on other symbols + 1.
func nextSlotCount(openPositions int, pendingSymbols []string, symbol string) int {
	n := openPositions + 1
	for _, s := range pendingSymbols {
		if s != symbol {
			n++
		}
	}
	return n
}

// processPendingEntries runs once per decision cycle: finalizes fills
// (SL/TP placed exactly at the limit price), cancels expired or invalidated
// orders, and drops externally-cancelled ones.
func (at *AutoTrader) processPendingEntries() {
	at.pendingEntriesMu.Lock()
	symbols := make([]string, 0, len(at.pendingEntries))
	for sym := range at.pendingEntries {
		symbols = append(symbols, sym)
	}
	snapshot := make(map[string]*pendingEntry, len(at.pendingEntries))
	for sym, pe := range at.pendingEntries {
		snapshot[sym] = pe
	}
	at.pendingEntriesMu.Unlock()
	if len(snapshot) == 0 {
		return
	}

	maxCycles := 3
	if at.config.StrategyConfig != nil && at.config.StrategyConfig.RiskControl.LimitEntryMaxCycles > 0 {
		maxCycles = at.config.StrategyConfig.RiskControl.LimitEntryMaxCycles
	}

	for _, sym := range symbols {
		pe := snapshot[sym]
		status, err := at.trader.GetOrderStatus(pe.Symbol, pe.OrderID)
		if err != nil {
			logger.Infof("📌 [%s] Limit entry %s status check failed: %v", at.name, pe.Symbol, err)
			continue
		}
		st, _ := status["status"].(string)
		switch strings.ToUpper(st) {
		case "FILLED":
			posKey := pe.Symbol + "_" + pe.Side
			at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()
			at.SetRecordedStopLoss(pe.Symbol, pe.Side, pe.StopLoss)
			// Fresh position — never inherit a previous trade's peak PnL.
			at.ClearPeakPnLCache(pe.Symbol, pe.Side)
			positionSide := "LONG"
			if pe.Side == "short" {
				positionSide = "SHORT"
			}
			at.placeProtectiveOrders(&kernel.Decision{
				Symbol: pe.Symbol, Action: "open_" + pe.Side,
				StopLoss: pe.StopLoss, TakeProfit: pe.TakeProfit,
			}, positionSide, pe.Quantity, pe.Price, pe.Price) // fill == limit price: exact anchor
			at.dropPendingEntry(pe.Symbol)
			logger.Infof("✅ [%s] Limit entry FILLED: %s %s @ %.6g — protective orders anchored", at.name, pe.Symbol, pe.Side, pe.Price)
			notify.Notify("ORDER", at.name, fmt.Sprintf("<b>📌 限价入场成交 %s</b>\n<i>%s @ %.6g,保护单已挂</i>", notify.Escape(pe.Symbol), pe.Side, pe.Price))
		case "CANCELED", "EXPIRED", "REJECTED":
			at.dropPendingEntry(pe.Symbol)
			logger.Infof("📌 [%s] Limit entry %s %s externally (%s) — state dropped", at.name, pe.Symbol, st, st)
		default: // NEW / PARTIALLY_FILLED
			pe.Cycles++
			// Invalidation: price crossed the SL before entry — setup is dead.
			if md, err := market.GetWithExchange(pe.Symbol, at.exchange); err == nil && md.CurrentPrice > 0 {
				invalid := (pe.Side == "long" && md.CurrentPrice <= pe.StopLoss) ||
					(pe.Side == "short" && md.CurrentPrice >= pe.StopLoss)
				if invalid {
					_ = at.cancelPending(pe)
					logger.Infof("📌 [%s] Limit entry %s invalidated: price crossed SL before entry", at.name, pe.Symbol)
					continue
				}
			}
			if pe.Cycles >= maxCycles {
				_ = at.cancelPending(pe)
				logger.Infof("📌 [%s] Limit entry %s expired after %d cycles — cancelled for re-evaluation", at.name, pe.Symbol, pe.Cycles)
			} else {
				logger.Infof("📌 [%s] Limit entry %s pending (%d/%d cycles)", at.name, pe.Symbol, pe.Cycles, maxCycles)
			}
		}
	}
}

// cancelPending cancels the exchange order and drops the pending state.
func (at *AutoTrader) cancelPending(pe *pendingEntry) error {
	grid, ok := at.trader.(interface {
		CancelOrder(symbol, orderID string) error
	})
	if !ok {
		at.dropPendingEntry(pe.Symbol)
		return fmt.Errorf("exchange does not support order cancellation")
	}
	err := grid.CancelOrder(pe.Symbol, pe.OrderID)
	at.dropPendingEntry(pe.Symbol)
	if err != nil {
		logger.Infof("📌 [%s] Cancel pending entry %s (order %s): %v", at.name, pe.Symbol, pe.OrderID, err)
	}
	notify.Notify("ORDER", at.name, fmt.Sprintf("<b>📌 限价单已撤销 %s</b>\n<i>%s @ %.6g 未成交,撤销重评</i>", notify.Escape(pe.Symbol), pe.Side, pe.Price))
	return err
}
