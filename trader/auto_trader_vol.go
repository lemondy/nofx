// Volatility-targeted position management and rule-based trailing stop.
//
// 规则1/2: every cycle, each open position's notional is compared against the
// volatility target (equity × risk% ÷ ATR%) inside an 80/120 hysteresis band —
// inside the band nothing happens, above 120% the excess is reduced (adds are
// left to the AI's judgment and only surfaced in logs).
//
// 规则3/4: exits are structural and one-way. The trailing stop arms at 1.5×
// the initial stop distance and trails 2×ATR(1h), monotonic tighten-only; once
// price travels the full TP distance the fixed TP order is converted into the
// trend-run (cancelled, the trailing stop takes over). SL is never widened.
package trader

import (
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	notify "nofx/telegram/notify"
	"strings"
	"time"
)

const (
	volBandLow  = 0.80 // hysteresis band
	volBandHigh = 1.20
	// Minimum interval between volatility resizes of one position — the band
	// bounds the trigger, this bounds the churn (fees/slippage erosion).
	volResizeCooldown = time.Hour
	trailArmMult      = 1.5 // arm the trail at 1.5× the initial stop distance
	trailATRMult      = 2.0 // trail distance: 2×ATR(1h)
	trailMinImprove   = 0.1 // % improvement required before moving the SL
)

// volTargetNotional: equity × risk% ÷ ATR% — the volatility-targeted position
// value. atrPct is in percent (1.5 = 1.5%).
func volTargetNotional(equity, riskPct, atrPct float64) float64 {
	if equity <= 0 || riskPct <= 0 || atrPct <= 0 {
		return 0
	}
	return equity * (riskPct / 100) / (atrPct / 100)
}

// resizeAction classifies actual vs target notional through the hysteresis band.
func resizeAction(actual, target float64) string {
	if target <= 0 || actual <= 0 {
		return "none"
	}
	ratio := actual / target
	switch {
	case ratio > volBandHigh:
		return "reduce"
	case ratio < volBandLow:
		return "underweight" // surfaced, never auto-added
	default:
		return "none"
	}
}

// moveStopExchange re-points a position's exchange stop to newSL. Binance
// allows only ONE closePosition stop per direction (-4130), so the old order
// is cancelled FIRST — place-first always fails when a stop exists. The
// seconds-level unprotected window is backstopped by the protection
// watchdog, which re-places the recorded stop if the new placement fails.
func (at *AutoTrader) moveStopExchange(symbol, side string, newSL float64) error {
	_ = at.trader.CancelStopLossOrders(symbol)
	return at.trader.SetStopLoss(symbol, strings.ToUpper(side), 0, newSL)
}

// trailingDecision computes the rule-based stop move for one position.
// Returns (newSL, move, trailingActive). The trail arms at 1.5× the initial
// stop distance and candidates 2×ATR from the mark price; a move only happens
// when it TIGHTENS the stop by ≥ trailMinImprove% of price. Never widens.
func trailingDecision(side string, entry, initialSL, markPrice, atrPct, currentSL float64) (float64, bool, bool) {
	initialDist := math.Abs(entry - initialSL)
	if initialDist <= 0 || markPrice <= 0 || atrPct <= 0 {
		return currentSL, false, false
	}
	mark := markPrice
	profitDist := 0.0
	if side == "long" {
		profitDist = (mark - entry) / entry * 100
	} else {
		profitDist = (entry - mark) / entry * 100
	}
	armed := profitDist >= trailArmMult*(initialDist/entry*100)
	if !armed {
		return currentSL, false, false
	}
	atrPrice := atrPct / 100 * mark
	var candidate float64
	if side == "long" {
		candidate = mark - trailATRMult*atrPrice
		if candidate <= currentSL*(1+trailMinImprove/100) {
			return currentSL, false, true // armed but no meaningful tightening yet
		}
		return math.Max(currentSL, candidate), candidate > currentSL, true
	}
	candidate = mark + trailATRMult*atrPrice
	if candidate >= currentSL*(1-trailMinImprove/100) {
		return currentSL, false, true
	}
	return math.Min(currentSL, candidate), candidate < currentSL, true
}

// tpRunnerDue: price has travelled the full planned TP distance — the fixed
// TP order converts into the trend-run (cancelled, trailing SL takes over).
func tpRunnerDue(side string, entry, takeProfit, markPrice float64) bool {
	if takeProfit <= 0 || markPrice <= 0 {
		return false
	}
	tpDist := math.Abs(takeProfit-entry) / entry * 100
	if tpDist <= 0 {
		return false
	}
	if side == "long" {
		return (markPrice-entry)/entry*100 >= tpDist
	}
	return (entry-markPrice)/entry*100 >= tpDist
}

// processVolTargetAndTrailing runs once per decision cycle over all open
// positions: volatility rescale (reduce-only) and trailing stop management.
func (at *AutoTrader) processVolTargetAndTrailing() {
	if at.config.StrategyConfig == nil ||
		(!at.config.StrategyConfig.RiskControl.VolTargetEnabled && !at.config.StrategyConfig.RiskControl.TrailingStopEnabled && kernel.ProfitLockRMult(&at.config.StrategyConfig.RiskControl) <= 0) {
		return
	}
	positions, err := at.trader.GetPositions()
	if err != nil {
		return
	}
	equity := 0.0
	if balance, err := at.trader.GetBalance(); err == nil {
		if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
			equity = eq
		} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
			equity = eq
		}
	}
	riskPct := at.config.StrategyConfig.RiskControl.RiskPerTradePct
	if riskPct <= 0 {
		riskPct = 1.5
	}
	volEnabled := at.config.StrategyConfig.RiskControl.VolTargetEnabled
	trailEnabled := at.config.StrategyConfig.RiskControl.TrailingStopEnabled

	for _, pos := range positions {
		symbol, _ := pos["symbol"].(string)
		side, _ := pos["side"].(string)
		markPrice, _ := pos["markPrice"].(float64)
		qty, _ := pos["positionAmt"].(float64)
		if symbol == "" || markPrice <= 0 || qty == 0 {
			continue
		}
		if qty < 0 {
			qty = -qty
		}
		posKey := symbol + "_" + side
		initialSL := at.GetRecordedStopLoss(symbol, side)
		if initialSL <= 0 {
			continue // pre-upgrade position or unknown stop — leave alone
		}
		data, err := market.GetWithExchange(symbol, at.exchange)
		if err != nil {
			continue
		}
		atrPct := oneHourATRPct(data)

		// ── 规则1/2: volatility-targeted rescale (reduce-only) ──
		if volEnabled && equity > 0 && atrPct > 0 {
			target := volTargetNotional(equity, riskPct, atrPct)
			actual := qty * markPrice
			if minSize := at.config.StrategyConfig.RiskControl.MinPositionSize; target < minSize {
				// target below tradable size — close the position entirely
				if _, err := at.reducePosition(symbol, side, qty); err == nil {
					logger.Infof("📉 [%s] Vol-target: %s notional %.2f below min size (target %.2f) — closed", at.name, symbol, actual, target)
					notify.Notify("ORDER", at.name, fmt.Sprintf("<b>📉 波动率调仓 %s</b>\\n目标仓位 %.2f 低于最小交易单位,已清仓", notify.Escape(symbol), target))
				}
				continue
			}
			switch resizeAction(actual, target) {
			case "reduce":
				if at.volResizeAllowed(posKey) {
					reduceQty := qty - target/markPrice
					if _, err := at.reducePosition(symbol, side, reduceQty); err == nil {
						at.markVolResize(posKey)
						logger.Infof("📉 [%s] Vol-target reduce: %s %.2f → %.2f USDT (ATR %.2f%%, band >120%%)", at.name, symbol, actual, target, atrPct)
						notify.Notify("ORDER", at.name, fmt.Sprintf("<b>📉 波动率减仓 %s</b>\\n敞口 %.2f → %.2f USDT(目标=ATR目标位,滞后带>120%%)", notify.Escape(symbol), actual, target))
					}
				}
			case "underweight":
				logger.Infof("📊 [%s] Vol-target: %s underweight (ratio < 80%%) — add-back is AI's call if the signal still holds", at.name, symbol)
			}
		}

		// ── 1R profit lock (user 2026-09-12): breakeven stop + 50% trim ──
		if lockR := kernel.ProfitLockRMult(&at.config.StrategyConfig.RiskControl); lockR > 0 {
			entry := posEntryPrice(pos)
			// The R anchor is the OPENING stop — the plan the position was
			// sized against — deliberately NOT the live recorded stop. Every
			// tighten above entry would otherwise compress the R distance and
			// fire the trim on pocket change (ONDOUSDT 2026-09-17: live SL
			// tightened 0.3428 → 0.3512 made +0.49% read as 1.89R → 50%
			// trimmed at 0.352 instead of true 1R at 0.3578). initialSL here
			// carries the live stop only as a legacy fallback for positions
			// with no persisted anchor.
			//
			// The live stop (passed as currentSL) still gates breakeven: once
			// armed, SetRecordedStopLoss overwrites it to `entry`, so the
			// breakeven condition goes false and arms exactly once — no extra
			// "already armed" bookkeeping. Trim idempotency is r1TrimDone.
			anchorSL := at.initialStopAnchor(symbol, side, initialSL)
			breakeven, trim := kernel.ProfitLockTargets(side, entry, anchorSL, initialSL, markPrice, lockR)
			if breakeven {
				if err := at.moveStopExchange(symbol, side, entry); err != nil {
					logger.Infof("⚠️ [%s] Breakeven SL place failed for %s: %v", at.name, symbol, err)
				} else {
					at.SetRecordedStopLoss(symbol, side, entry)
					logger.Infof("🔒 [%s] Breakeven armed: %s %dR reached — SL moved to entry %.6g", at.name, symbol, int(lockR), entry)
					notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🔒 保本止损 %s</b>\n浮盈达 %.0fR,止损已移至开仓价 <code>%.6g</code>——最差结果保本出局", notify.Escape(symbol), lockR, entry))
				}
			}
			if trim {
				at.tpTrimMutex.Lock()
				done := at.r1TrimDone[posKey]
				at.tpTrimMutex.Unlock()
				if !done {
					minSize := at.config.StrategyConfig.RiskControl.MinPositionSize
					remainder := qty * 0.5 * markPrice
					if minSize > 0 && remainder < minSize {
						// remainder would be dust — lock the whole thing instead
						if err := at.emergencyClosePosition(symbol, side); err == nil {
							at.tpTrimMutex.Lock()
							at.r1TrimDone[posKey] = true
							at.tpTrimDone[posKey] = true
							at.tpTrimMutex.Unlock()
							logger.Infof("🎯 [%s] 1R lock: %s remainder below min size — closed fully instead of trimming", at.name, symbol)
							notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🎯 1R 止盈 %s</b>\n浮盈达 %.0fR,剩余仓位低于最小单位,已全部平仓锁定利润", notify.Escape(symbol), lockR))
						} else {
							logger.Infof("❌ [%s] 1R full close failed (%s): %v", at.name, symbol, err)
						}
						continue
					}
					if _, err := at.reducePosition(symbol, side, qty*0.5); err == nil {
						at.tpTrimMutex.Lock()
						at.r1TrimDone[posKey] = true
						at.tpTrimDone[posKey] = true // the ROE trim tier is consumed by the 1R lock
						at.tpTrimMutex.Unlock()
						logger.Infof("🎯 [%s] 1R trim: %s 50%% trimmed @ %.6g (breakeven SL, rest rides to structural TP)", at.name, symbol, markPrice)
						notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🎯 1R 止盈减仓 %s</b>\n浮盈达 %.0fR,市价减仓 50%% 锁定利润,剩余仓位止损已保本、继续持有", notify.Escape(symbol), lockR))
					} else {
						logger.Infof("❌ [%s] 1R trim failed (%s) — retries next cycle", at.name, symbol)
					}
				}
			}
		}

		// ── 规则3/4: rule-based trailing stop + TP runner ──
		if trailEnabled {
			currentSL := initialSL
			newSL, move, armed := trailingDecision(side, posEntryPrice(pos), initialSL, markPrice, atrPct, currentSL)
			if armed {
				if move {
					if err := at.moveStopExchange(symbol, side, newSL); err != nil {
						logger.Infof("⚠️ [%s] Trailing SL place failed for %s: %v", at.name, symbol, err)
					} else {
						at.SetRecordedStopLoss(symbol, side, newSL)
						logger.Infof("🔒 [%s] Trailing stop moved: %s SL → %.6g (armed %.1f%%, trail 2×ATR)", at.name, symbol, newSL, trailArmMult)
						notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🔒 移动止损 %s</b>\\nSL 推进至 <code>%.6g</code>(2×ATR 跟踪,只紧不松)", notify.Escape(symbol), newSL))
					}
				}
				// 规则5: once price travels the full TP distance, convert the
				// fixed TP into the trend-run (trailing stop takes over).
				var tp float64
				if at.config.StrategyConfig != nil {
					tp = at.getOpenTakeProfit(symbol, side)
				}
				if tp > 0 && tpRunnerDue(side, posEntryPrice(pos), tp, markPrice) && !at.tpRunnerDone(posKey) {
					_ = at.trader.CancelTakeProfitOrders(symbol)
					at.markTPRunner(posKey)
					logger.Infof("🏃 [%s] TP runner: %s reached its TP distance — fixed TP cancelled, 2×ATR trail takes over", at.name, symbol)
					notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🏃 止盈转趋势跟踪 %s</b>\\n到达计划止盈距离,固定止盈单撤除,剩余仓位由移动止损接管", notify.Escape(symbol)))
				}
			}
		}
	}
}

func posEntryPrice(pos map[string]interface{}) float64 {
	if v, ok := pos["entryPrice"].(float64); ok {
		return v
	}
	return 0
}

// initialStopAnchor resolves the opening-risk stop for the 1R lock: the stop
// planned at entry, frozen per position (in-memory write-once + set-if-empty
// stamp on the OPEN position row) so stop adjustments and restarts can't pull
// the R bar along. Resolution order: in-memory anchor → persisted row → the
// live recorded stop as a legacy fallback (pre-anchor positions), which then
// freezes deterministically on first sight.
func (at *AutoTrader) initialStopAnchor(symbol, side string, liveSL float64) float64 {
	if sl := at.GetInitialStopLoss(symbol, side); sl > 0 {
		// Re-stamp on every scan: the position row is created by trade sync
		// shortly after open, later than the in-memory anchor — this heals
		// the row cheaply (the UPDATE is a no-op once stamped).
		at.SetInitialStopLoss(symbol, side, sl)
		return sl
	}
	if at.store != nil {
		// Position rows store side as "LONG"/"SHORT"; the exchange feed here
		// is lowercase.
		if pos, err := at.store.Position().GetOpenPositionBySymbol(at.id, symbol, strings.ToUpper(side)); err == nil && pos != nil && pos.InitialStopLoss > 0 {
			at.SetInitialStopLoss(symbol, side, pos.InitialStopLoss)
			return pos.InitialStopLoss
		}
	}
	if liveSL > 0 {
		at.SetInitialStopLoss(symbol, side, liveSL)
	}
	return liveSL
}

// reducePosition closes part of a position (market).
func (at *AutoTrader) reducePosition(symbol, side string, qty float64) (map[string]interface{}, error) {
	if side == "long" {
		return at.trader.CloseLong(symbol, qty)
	}
	return at.trader.CloseShort(symbol, qty)
}

// volResizeAllowed / markVolResize: per-position resize cooldown.
func (at *AutoTrader) volResizeAllowed(posKey string) bool {
	at.volResizeMu.Lock()
	defer at.volResizeMu.Unlock()
	if at.volResizeLast == nil {
		at.volResizeLast = make(map[string]time.Time)
	}
	return time.Since(at.volResizeLast[posKey]) >= volResizeCooldown
}

func (at *AutoTrader) markVolResize(posKey string) {
	at.volResizeMu.Lock()
	defer at.volResizeMu.Unlock()
	if at.volResizeLast == nil {
		at.volResizeLast = make(map[string]time.Time)
	}
	at.volResizeLast[posKey] = time.Now()
}

// getOpenTakeProfit reads the recorded TP for an open position (0 if unknown).
func (at *AutoTrader) getOpenTakeProfit(symbol, side string) float64 {
	// The TP lives on the exchange; the recorded decision value is the
	// practical source. Re-deriving it from algo orders costs an API call per
	// position per cycle — use the last known decision TP instead.
	at.openTPMu.RLock()
	defer at.openTPMu.RUnlock()
	return at.openTP[symbol+"_"+side]
}

func (at *AutoTrader) recordOpenTakeProfit(symbol, side string, tp float64) {
	at.openTPMu.Lock()
	defer at.openTPMu.Unlock()
	if at.openTP == nil {
		at.openTP = make(map[string]float64)
	}
	at.openTP[symbol+"_"+side] = tp
}

func (at *AutoTrader) tpRunnerDone(posKey string) bool {
	at.volResizeMu.Lock()
	defer at.volResizeMu.Unlock()
	return at.tpRunnerDoneMap[posKey]
}

func (at *AutoTrader) markTPRunner(posKey string) {
	at.volResizeMu.Lock()
	defer at.volResizeMu.Unlock()
	if at.tpRunnerDoneMap == nil {
		at.tpRunnerDoneMap = make(map[string]bool)
	}
	at.tpRunnerDoneMap[posKey] = true
}
