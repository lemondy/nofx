package kernel

import (
	"fmt"
	"math"
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
		"open_long":           true,
		"open_short":          true,
		"open_long_limit":     true,
		"open_short_limit":    true,
		"close_long":          true,
		"close_short":         true,
		"adjust_stop_loss":    true,
		"partial_close_long":  true,
		"partial_close_short": true,
		"hold":                true,
		"wait":                true,
	}

	if !validActions[d.Action] {
		return fmt.Errorf("invalid action: %s", d.Action)
	}
	if d.Action == "hold" && !hasPosition {
		return fmt.Errorf("hold requires an existing position; use wait when flat")
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
	// Required decision annotations are dataset inputs, not trading gates.
	// Backfill omissions from program-owned hard-gate evidence rather than
	// accepting sparse rows or rejecting an otherwise safe wait/hold batch.
	backfillDecisionAnnotations(d, hasPosition, gs)
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
		} else if d.NextTrigger == "" {
			d.NextTrigger = "下一周期方向信号更新 + RECHECK_ALL_HARD_GATES"
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

		// Round-4 review R4-9: the old RR check here was a provably-dead
		// no-op (its synthetic entry sat at the 20% point of the SL→TP span,
		// forcing RR ≡ 4.0 against a hardcoded 3.0 floor) while REAL min-RR
		// enforcement is the hard_entry_gate's rr_scan verdict plus the
		// executor's checkRR — both reading the configured
		// min_risk_reward_ratio. Nothing re-added here: a kernel-side RR
		// gate must price the actual decision values against the CONFIGURED
		// floor, or it just becomes another drift bomb.
	}

	return nil
}

func backfillDecisionAnnotations(d *Decision, hasPosition bool, gs *GateState) {
	isOpen := strings.HasPrefix(d.Action, "open_long") || strings.HasPrefix(d.Action, "open_short")
	if d.Action == "wait" || isOpen {
		if d.EntryQuality == nil {
			q := d.Confidence
			if q < 0 {
				q = 0
			}
			d.EntryQuality = &q
		}
		if d.BlockingFactors == nil {
			d.BlockingFactors = []string{}
		}
	}
	if d.Action == "wait" {
		codes := gateFailureCodes(gs, d.WaitBias)
		if len(d.BlockingFactors) == 0 {
			for _, code := range codes {
				if factor := blockingFactorForGateCode(code); factor != "" {
					d.BlockingFactors = append(d.BlockingFactors, factor)
				}
			}
			d.BlockingFactors = NormalizeBlockingFactors(d.BlockingFactors)
		}
		if len(d.BlockingFactors) == 0 {
			if d.WaitBias == "long" || d.WaitBias == "short" {
				d.BlockingFactors = []string{"WAIT_PULLBACK"}
			} else {
				d.BlockingFactors = []string{"RANGE_NO_DIRECTION"}
			}
		}
		if len(d.NoTradeReasons) == 0 {
			for _, code := range codes {
				if len(d.NoTradeReasons) == 4 {
					break
				}
				if !containsString(d.NoTradeReasons, code) {
					d.NoTradeReasons = append(d.NoTradeReasons, code)
				}
			}
		}
		if len(d.NoTradeReasons) == 0 {
			d.NoTradeReasons = []string{"未形成合规入场条件"}
		}
		if len(d.NoTradeReasons) > 4 {
			d.NoTradeReasons = d.NoTradeReasons[:4]
		}
		if len(d.NoTradeReasons) == 1 {
			d.NoTradeReasons = append(d.NoTradeReasons, "等待下一周期重新评估")
		}
	}
	if d.Action == "hold" && hasPosition {
		if d.ManagementQuality == nil {
			q := 50
			d.ManagementQuality = &q
		}
		if d.ManagementFlags == nil {
			d.ManagementFlags = []string{}
		}
		if len(d.NoTradeReasons) == 0 {
			d.NoTradeReasons = []string{"持仓管理条件未触发", "继续按保护单管理"}
		} else if len(d.NoTradeReasons) > 4 {
			d.NoTradeReasons = d.NoTradeReasons[:4]
		}
	}
}

func gateFailureCodes(gs *GateState, waitBias string) []string {
	if gs == nil {
		return nil
	}
	var source []string
	switch waitBias {
	case "long":
		source = gs.LongFailed
	case "short":
		source = gs.ShortFailed
	default:
		source = append(append([]string{}, gs.LongFailed...), gs.ShortFailed...)
	}
	out := make([]string, 0, len(source))
	for _, code := range source {
		if code != "" && !containsString(out, code) {
			out = append(out, code)
		}
	}
	return out
}

func blockingFactorForGateCode(code string) string {
	switch {
	case strings.HasPrefix(code, "RR_MAX_"):
		return "RR_LOW"
	case code == "LIMIT_ANCHOR_SUPPRESSED":
		return "ANCHOR_SUPPRESSED"
	case strings.HasPrefix(code, "MICRO_TREND_"):
		return "TIMING_GATE"
	case code == "EXTENDED_PUMP_UNCONFIRMED":
		return "EXTENDED_PUMP"
	case strings.HasPrefix(code, "STOP_PLAN_"):
		return "STRUCTURE_CONFLICT"
	case code == "DATA_INSUFFICIENT":
		return "DATA_INSUFFICIENT"
	case code == "MIN_SIZE_DEAD_ZONE":
		return "MIN_SIZE"
	case code == "LOSS_STREAK_BANNED":
		return "LOSS_STREAK_BAN"
	case strings.HasPrefix(code, "VENDOR_DIVERGENCE_"):
		return "VENDOR_DIVERGENCE"
	case strings.HasPrefix(code, "CONSENSUS_OPPOSED_"):
		return "CONSENSUS_OPPOSED"
	case code == "POOR_HISTORY":
		return "POOR_HISTORY"
	case code == "STOCK_WEEKEND":
		return "MARKET_CLOSED"
	default:
		return "CONFLICT_UNRESOLVED"
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// StopPlanTolerancePct: same echo-drift allowance as the limit anchors — the
// prompt's contract is verbatim adoption, so anything beyond rounding noise
// gets snapped.
const StopPlanTolerancePct = 0.05

// correctStopLossToPlan snaps an open decision's stop_loss to the
// precomputed stop_plan_price for its direction (09-19 audit: the model
// echoed its own 2.13% stop on ZEC against the gated plan — the executor
// band check rejected the whole output and the cycle's analysis was wasted).
// The gated plan IS the trade: plan, gate and checkRR price the same stop,
// so snapping makes the executed risk exactly the gated risk. Decisions on
// symbols without a plan (gate blocked with STOP_PLAN_*, or no floor
// configured) pass through untouched — the executor's own gates judge them.
func correctStopLossToPlan(decisions []Decision, gates map[string]*GateState, tolerancePct float64) {
	for i := range decisions {
		d := &decisions[i]
		isLong := strings.HasPrefix(d.Action, "open_long")
		isShort := strings.HasPrefix(d.Action, "open_short")
		if !isLong && !isShort {
			continue
		}
		gs, ok := gates[market.Normalize(d.Symbol)]
		if !ok || gs == nil {
			continue
		}
		plan := gs.LongStopPlanPrice
		if !isLong {
			plan = gs.ShortStopPlanPrice
		}
		if plan <= 0 {
			continue // no plan this direction — nothing authoritative to snap to
		}
		if d.StopLoss <= 0 {
			// Placeholder zero from the "unknown → 0" output rule: copy the
			// plan instead of tripping the missing-stop rejection.
			logger.Infof("📐 [%s] %s placeholder stop_loss → stop_plan %.6g",
				d.Symbol, d.Action, plan)
			d.StopLoss = plan
			continue
		}
		dev := (d.StopLoss - plan) / plan * 100
		if math.Abs(dev) <= tolerancePct {
			continue
		}
		logger.Infof("📐 [%s] %s stop_loss %.6g → stop_plan %.6g (%+.2f%% drift — the gated plan is the trade)",
			d.Symbol, d.Action, d.StopLoss, plan, dev)
		d.StopLoss = plan
	}
}
