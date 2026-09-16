package kernel

import (
	"fmt"
	"nofx/logger"
	"nofx/market"
	"strings"
)

// ============================================================================
// Decision Validation
// ============================================================================

// LimitAnchorTolerancePct is the max allowed drift between the model's
// open_*_limit price and the pre-computed anchor before the price is snapped
// back to the anchor (models sometimes do their own arithmetic despite the
// "copy, don't calculate" instruction).
const LimitAnchorTolerancePct = 0.05

// correctLimitAnchors snaps open_long_limit / open_short_limit prices back to
// the pre-computed limit_buy_price / limit_sell_price shown in the prompt when
// they drift beyond tolerance. Symbols without a stored anchor (not in the
// prompt, data-incomplete) pass through untouched.
//
// Suppressed anchors (LimitBuy/LimitSell = 0) are fail-closed: the prompt
// forbids opening that side, so a model-invented price is downgraded to wait
// instead of reaching the exchange (entry_rule_triggered can still be true —
// the heuristic never sees the suppression, review 2026-09-07).
func correctLimitAnchors(decisions []Decision, anchors map[string]*LimitAnchor, tolerancePct float64) {
	for i := range decisions {
		d := &decisions[i]
		if d.Action != "open_long_limit" && d.Action != "open_short_limit" {
			continue
		}
		a, ok := anchors[market.Normalize(d.Symbol)]
		if !ok || a == nil {
			continue
		}
		expected := a.LimitBuy
		if d.Action == "open_short_limit" {
			expected = a.LimitSell
		}
		if expected <= 0 {
			logger.Infof("🚫 [%s] %s downgraded to wait: the shown %s anchor was suppressed (0), model price %.6g discarded",
				d.Symbol, d.Action, map[bool]string{true: "limit_sell", false: "limit_buy"}[d.Action == "open_short_limit"], d.Price)
			d.WaitBias = map[bool]string{true: "short", false: "long"}[d.Action == "open_short_limit"]
			d.Action = "wait"
			d.Price = 0
			d.BlockingFactors = []string{"ANCHOR_SUPPRESSED"}
			d.NoTradeReasons = append(d.NoTradeReasons, "挂单锚点被程序抑制,该方向本周期禁止开仓")
			continue
		}
		if d.Price <= 0 {
			// Placeholder zero from the "unknown → 0" output rule: the anchor
			// itself is valid, so copy it (the prompt's "copy, don't calculate"
			// contract) instead of failing the whole batch in validateDecision.
			logger.Infof("📐 [%s] %s placeholder price %.4g → anchor %.6g",
				d.Symbol, d.Action, d.Price, expected)
			d.Price = expected
			continue
		}
		dev := (d.Price - expected) / expected * 100
		if dev < 0 {
			dev = -dev
		}
		if dev <= tolerancePct {
			continue
		}
		logger.Infof("📐 [%s] %s anchor corrected: %.6g → %.6g (model deviation %.3f%% > %.2f%%)",
			d.Symbol, d.Action, d.Price, expected, dev, tolerancePct)
		d.Price = expected
	}
}

func validateDecisions(decisions []Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int, btcEthPosRatio, altcoinPosRatio float64, minPositionSize float64, positionSymbols map[string]bool, gateStates map[string]*GateState) error {
	for i := range decisions {
		if err := validateDecision(&decisions[i], accountEquity, btcEthLeverage, altcoinLeverage, btcEthPosRatio, altcoinPosRatio, minPositionSize, positionSymbols[market.Normalize(decisions[i].Symbol)], gateStates[market.Normalize(decisions[i].Symbol)]); err != nil {
			return fmt.Errorf("decision #%d validation failed: %w", i+1, err)
		}
	}
	return nil
}

func validateDecision(d *Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int, btcEthPosRatio, altcoinPosRatio float64, minPositionSize float64, hasPosition bool, gs *GateState) error {
	validActions := map[string]bool{
		"open_long":        true,
		"open_short":       true,
		"open_long_limit":  true,
		"open_short_limit": true,
		"close_long":       true,
		"close_short":      true,
		"adjust_stop_loss": true,
		"partial_close_long":  true,
		"partial_close_short": true,
		"hold":             true,
		"wait":             true,
	}

	if !validActions[d.Action] {
		return fmt.Errorf("invalid action: %s", d.Action)
	}

	// Position-management actions carry required fields (user 2026-09-13:
	// expose the backend's stop-move/partial-close capabilities to the AI).
	if d.Action == "adjust_stop_loss" && d.StopLoss <= 0 {
		return fmt.Errorf("adjust_stop_loss requires stop_loss (the new stop price)")
	}
	if (d.Action == "partial_close_long" || d.Action == "partial_close_short") &&
		(d.CloseFraction <= 0 || d.CloseFraction > 0.5) {
		return fmt.Errorf("partial_close requires close_fraction in (0, 0.5] — full exits use close_*")
	}

	// wait_bias: empty | long | short, only meaningful on wait (hold keeps
	// the bias it entered with implicitly; open/close carry no bias).
	if d.WaitBias != "" && d.WaitBias != "long" && d.WaitBias != "short" {
		return fmt.Errorf("invalid wait_bias %q (must be long/short or empty)", d.WaitBias)
	}

	// wait_state / next_trigger hygiene (review 2026-09-15 points 11/12):
	// unknown tags, non-wait carriers and states contradicting wait_bias are
	// STRIPPED, never batch-fatal — this is dataset annotation, not a risk
	// gate. A directional state back-fills wait_bias (they are two views of
	// one verdict, and the enum is the stricter source).
	if d.Action != "wait" {
		d.WaitState, d.NextTrigger = "", ""
	} else if d.WaitState != "" {
		conflict := (strings.HasSuffix(d.WaitState, "_LONG") && d.WaitBias == "short") ||
			(strings.HasSuffix(d.WaitState, "_SHORT") && d.WaitBias == "long")
		if !IsValidWaitState(d.WaitState) || conflict {
			d.WaitState = ""
		} else if strings.HasSuffix(d.WaitState, "_LONG") {
			d.WaitBias = "long"
		} else if strings.HasSuffix(d.WaitState, "_SHORT") {
			d.WaitBias = "short"
		}
		if rt := []rune(d.NextTrigger); len(rt) > 180 {
			d.NextTrigger = string(rt[:180])
		}
	}

	// Backtest-dataset hygiene: strip non-vocabulary tags, clamp quality.
	if len(d.BlockingFactors) > 0 {
		d.BlockingFactors = NormalizeBlockingFactors(d.BlockingFactors)
	}
	// Position-management self-assessment hygiene (hold on open positions):
	// clamp quality, strip non-vocabulary flags.
	if len(d.ManagementFlags) > 0 {
		d.ManagementFlags = NormalizeManagementFlags(d.ManagementFlags)
	}
	if d.ManagementQuality != nil {
		q := *d.ManagementQuality
		if q < 0 {
			q = 0
		}
		if q > 100 {
			q = 100
		}
		d.ManagementQuality = &q
	}
	// Stage is DERIVED, never declared (schema-redundancy audit: waits
	// 09-13, all actions 09-16). wait: an explicit wait_state wins;
	// otherwise wait_bias + blocking_factors determine NO_SETUP/WATCH/READY.
	// Everything else is a pure (action, hasPosition) lookup — the model's
	// declaration, if any, is overwritten.
	if d.Action == "wait" {
		// wait_state is DERIVED from (wait_bias, hard gate) — program truth
		// replaces whatever the model declared (09-16 schema-redundancy).
		// Missing gate snapshot (data-incomplete symbol) → empty state, the
		// stage falls back to the bias/blockers rule below.
		d.WaitState = DeriveWaitStateFromGate(d.WaitBias, gs)
		if d.WaitState == "" || d.WaitState == "BLOCKED" {
			d.NextTrigger = "" // only WATCH_*/READY_* carry a re-check trigger
		}
		if st := WaitStateToStage(d.WaitState, d.WaitBias); st != "" {
			d.Stage = st
		} else {
			d.Stage = DeriveWaitStage(d.WaitBias, d.BlockingFactors)
		}
	} else {
		d.Stage = DeriveDecisionStage(d.Action, hasPosition)
	}
	if d.EntryQuality != nil {
		q := *d.EntryQuality
		if q < 0 {
			q = 0
		}
		if q > 100 {
			q = 100
		}
		d.EntryQuality = &q
	}

	if d.Action == "open_long_limit" || d.Action == "open_short_limit" {
		if d.Price <= 0 {
			return fmt.Errorf("%s requires price (the limit/trigger level)", d.Action)
		}
	}

	isOpen := d.Action == "open_long" || d.Action == "open_short" ||
		d.Action == "open_long_limit" || d.Action == "open_short_limit"
	isLongOpen := d.Action == "open_long" || d.Action == "open_long_limit"
	if isOpen {
		maxLeverage := altcoinLeverage
		posRatio := altcoinPosRatio
		maxPositionValue := accountEquity * posRatio
		if d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" {
			maxLeverage = btcEthLeverage
			posRatio = btcEthPosRatio
			maxPositionValue = accountEquity * posRatio
		}

		if d.Leverage <= 0 {
			return fmt.Errorf("leverage must be greater than 0: %d", d.Leverage)
		}
		if d.Leverage > maxLeverage {
			logger.Infof("⚠️  [Leverage Fallback] %s leverage exceeded (%dx > %dx), auto-adjusting to limit %dx",
				d.Symbol, d.Leverage, maxLeverage, maxLeverage)
			d.Leverage = maxLeverage
		}
		if d.PositionSizeUSD <= 0 {
			return fmt.Errorf("position size must be greater than 0: %.2f", d.PositionSizeUSD)
		}

		// The binding minimum is the strategy-config min_position_size
		// (user 09-16: never hardcode — the snapshot min_size block and the
		// executor enforce the SAME config value; a hardcoded 12 here once
		// rejected openings the strategy's min 5 had declared legal).
		// ≤0/0 passed in means the caller skipped resolution — apply the
		// executor-mirrored default rather than silently disabling the floor.
		if minPositionSize <= 0 {
			minPositionSize = MinPositionSizeDefaultUSDT
		}
		if d.PositionSizeUSD < minPositionSize {
			return fmt.Errorf("opening amount too small (%.2f USDT), must be ≥%.2f USDT (strategy min_position_size)", d.PositionSizeUSD, minPositionSize)
		}

		tolerance := maxPositionValue * 0.01
		if d.PositionSizeUSD > maxPositionValue+tolerance {
			if d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" {
				return fmt.Errorf("BTC/ETH single coin position value cannot exceed %.0f USDT (%.1fx account equity), actual: %.0f", maxPositionValue, posRatio, d.PositionSizeUSD)
			} else {
				return fmt.Errorf("altcoin single coin position value cannot exceed %.0f USDT (%.1fx account equity), actual: %.0f", maxPositionValue, posRatio, d.PositionSizeUSD)
			}
		}
		if d.StopLoss <= 0 || d.TakeProfit <= 0 {
			return fmt.Errorf("stop loss and take profit must be greater than 0")
		}

		if isLongOpen {
			if d.StopLoss >= d.TakeProfit {
				return fmt.Errorf("for long positions, stop loss price must be less than take profit price")
			}
		} else {
			if d.StopLoss <= d.TakeProfit {
				return fmt.Errorf("for short positions, stop loss price must be greater than take profit price")
			}
		}

		var entryPrice float64
		if isLongOpen {
			entryPrice = d.StopLoss + (d.TakeProfit-d.StopLoss)*0.2
		} else {
			entryPrice = d.StopLoss - (d.StopLoss-d.TakeProfit)*0.2
		}

		var riskPercent, rewardPercent, riskRewardRatio float64
		if isLongOpen {
			riskPercent = (entryPrice - d.StopLoss) / entryPrice * 100
			rewardPercent = (d.TakeProfit - entryPrice) / entryPrice * 100
			if riskPercent > 0 {
				riskRewardRatio = rewardPercent / riskPercent
			}
		} else {
			riskPercent = (d.StopLoss - entryPrice) / entryPrice * 100
			rewardPercent = (entryPrice - d.TakeProfit) / entryPrice * 100
			if riskPercent > 0 {
				riskRewardRatio = rewardPercent / riskPercent
			}
		}

		if riskRewardRatio < 3.0 {
			return fmt.Errorf("risk/reward ratio too low (%.2f:1), must be ≥3.0:1 [risk: %.2f%% reward: %.2f%%] [stop loss: %.2f take profit: %.2f]",
				riskRewardRatio, riskPercent, rewardPercent, d.StopLoss, d.TakeProfit)
		}
	}

	return nil
}

