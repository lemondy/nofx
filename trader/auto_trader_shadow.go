package trader

import (
	"fmt"
	"math"
	"strings"
	"time"

	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
)

// ============================================================================
// Gate shadow blocks (09-21 user directive): every cycle, blocked directions
// that had a complete would-be trade (entry + stop_plan + structural TP) are
// recorded and evaluated against the later price path once the horizon
// matures. This turns the numeric gate thresholds into calibratable data —
// "did the blocked trades would have won?" instead of theory-only audits.
// Purely observational: nothing here feeds back into trading decisions.
// ============================================================================

// gateShadowHorizon: how long after the block the counterfactual is resolved
// against 15m candles. 8h covers the strategy's typical trade duration (the
// 30d median hold is well under it) without dragging the evaluation queue.
const gateShadowHorizon = 8 * time.Hour

// shadowBarDur is the candle duration used by both shadow evaluation passes.
const shadowBarDur = 15 * time.Minute

// gateShadowHorizon48 (E1, QUANT_REVIEW 09-22): second pass. At 8h the live
// data resolved 39/56 as `timeout` — structural TPs rarely trigger that
// fast, so the tp_first/sl_first split (the datum any threshold calibration
// needs) barely exists. The same counterfactual is re-walked at 48h so the
// structural TP gets a real chance to print.
const gateShadowHorizon48 = 48 * time.Hour

// recordGateShadowBlocks runs right after cycleGateStates is captured (post
// prompt-build, pre-execution — the same verdicts the model is about to see).
func (at *AutoTrader) recordGateShadowBlocks(states map[string]*kernel.GateState, cycleNumber int) {
	if at.store == nil || len(states) == 0 {
		return
	}
	now := time.Now().UTC()
	for symbol, gs := range states {
		if gs == nil {
			continue
		}
		for _, d := range []struct {
			dir     string
			allowed bool
			entry   float64
			basis   string
			sl      float64
			tp      float64
			failed  []string
		}{
			{"long", gs.LongAllowed, gs.LongEntryPrice, gs.LongEntryBasis, gs.LongStopPlanPrice, gs.LongTakeProfit, gs.LongFailed},
			{"short", gs.ShortAllowed, gs.ShortEntryPrice, gs.ShortEntryBasis, gs.ShortStopPlanPrice, gs.ShortTakeProfit, gs.ShortFailed},
		} {
			// Only COMPLETE counterfactuals are informative: no plan (the
			// very reason for the block) leaves nothing to evaluate.
			if d.allowed || d.entry <= 0 || d.sl <= 0 || d.tp <= 0 || len(d.failed) == 0 {
				continue
			}
			risk := d.entry - d.sl
			if d.dir == "short" {
				risk = d.sl - d.entry
			}
			reward := d.tp - d.entry
			if d.dir == "short" {
				reward = d.entry - d.tp
			}
			if risk <= 0 || reward <= 0 {
				continue
			}
			created, err := at.store.GateShadow().CreateIfIdle(&store.GateShadowBlock{
				TraderID:     at.id,
				Symbol:       symbol,
				Direction:    d.dir,
				CycleNumber:  cycleNumber,
				BlockedCodes: strings.Join(d.failed, ","),
				EntryPrice:   d.entry,
				EntryBasis:   d.basis,
				StopPrice:    d.sl,
				TakeProfit:   d.tp,
				PlanRR:       reward / risk,
				HorizonHours: int(gateShadowHorizon.Hours()),
				CreatedAt:    now,
			})
			if err != nil {
				logger.Infof("⚠️ [%s] gate shadow: record %s %s failed: %v", at.name, symbol, d.dir, err)
			} else if created {
				logger.Infof("👻 [%s] gate shadow: %s %s blocked (%s) entry %.6g SL %.6g TP %.6g RR %.2f — will evaluate in %dh",
					at.name, symbol, d.dir, strings.Join(d.failed, "+"), d.entry, d.sl, d.tp, reward/risk, int(gateShadowHorizon.Hours()))
			}
		}
	}
}

// evaluateGateShadowBlocks resolves matured counterfactuals against 15m
// candles: which of TP / SL was touched first within the horizon (a bar
// touching BOTH counts as sl_first — conservative, the stop is assumed hit
// intra-bar before the target). Two passes: 8h writes `outcome`, 48h writes
// `outcome_48h`.
func (at *AutoTrader) evaluateGateShadowBlocks() {
	if at.store == nil {
		return
	}
	fillWindow := limitEntryLifetime(at.limitEntryMaxCycles(), at.config.ScanInterval)
	// --- 8h pass ---
	rows, err := at.store.GateShadow().ListMatured(at.id, time.Now().UTC().Add(-gateShadowHorizon))
	if err != nil {
		return
	}
	for _, row := range rows {
		now := time.Now().UTC()
		data, err := at.getMarketTimeframes(row.Symbol, []string{"15m"}, "15m", shadowKlineCount(row.CreatedAt, now))
		if err != nil || data == nil {
			continue // transient — retry next cycle
		}
		tf := data.TimeframeData["15m"]
		if tf == nil || len(tf.Klines) == 0 {
			at.finishShadow(row, "no_data", 0)
			continue
		}
		outcome, exit := resolveShadowOutcome(row, tf.Klines, gateShadowHorizon, fillWindow, now)
		at.finishShadow(row, outcome, exit)
	}

	// --- 48h pass ---
	rows48, err := at.store.GateShadow().ListMatured48(at.id, time.Now().UTC().Add(-gateShadowHorizon48))
	if err != nil {
		return
	}
	for _, row := range rows48 {
		now := time.Now().UTC()
		data, err := at.getMarketTimeframes(row.Symbol, []string{"15m"}, "15m", shadowKlineCount(row.CreatedAt, now))
		if err != nil || data == nil {
			continue // transient — retry next cycle
		}
		tf := data.TimeframeData["15m"]
		if tf == nil || len(tf.Klines) == 0 {
			at.finishShadow48(row, "no_data", 0)
			continue
		}
		outcome, exit := resolveShadowOutcome(row, tf.Klines, gateShadowHorizon48, fillWindow, now)
		at.finishShadow48(row, outcome, exit)
	}
}

// review 2026-10-08 E: reach back to the block even after a late evaluation.
// Count 15m bars; the 1500-bar market/Binance cap covers about 15.6 days.
// Older rows with incomplete coverage resolve as no_data.
func shadowKlineCount(start, now time.Time) int {
	count := math.Ceil(float64(now.Sub(start))/float64(shadowBarDur)) + 2
	if count > 1500 {
		return 1500
	}
	if count < 2 {
		return 2
	}
	return int(count)
}

// resolveShadowOutcome walks a complete, closed 15m window, then resolves
// fills and TP/SL touches. review 2026-10-08 D/E.
func resolveShadowOutcome(row *store.GateShadowBlock, klines []market.KlineBar, horizon, fillWindow time.Duration, now time.Time) (outcome string, exit float64) {
	start, end := row.CreatedAt, row.CreatedAt.Add(horizon)
	expected := start.Truncate(shadowBarDur)
	if expected.Before(start) {
		expected = expected.Add(shadowBarDur)
	}
	lastClose := end.Truncate(shadowBarDur)
	bars := make([]market.KlineBar, 0, len(klines))
	for _, k := range klines {
		open := time.UnixMilli(k.Time)
		closeTime := open.Add(shadowBarDur)
		// Drop the bar containing the block: its extremes may predate it.
		// Dropping it loses at most 15m of the fill window (review 2026-10-08 E).
		if open.Before(start) || closeTime.After(end) || closeTime.After(now) {
			continue
		}
		// Validate coverage before scoring even an early TP/SL; missing
		// first, interior or last candles must never produce a partial verdict.
		if !open.Equal(expected) || k.High <= 0 || k.Low <= 0 || k.Close <= 0 {
			return "no_data", 0
		}
		bars = append(bars, k)
		expected = closeTime
	}
	if len(bars) == 0 || !expected.Equal(lastClose) {
		return "no_data", 0
	}

	filled := row.EntryBasis != "limit_anchor" // live_price and legacy ""
	// A bar may fill if its open precedes expiry; after dropping the
	// containing bar, a 30m lifetime leaves 1–2 candidate 15m bars.
	fillEnd := start.Add(fillWindow)
	for _, k := range bars {
		hitTP := (row.Direction == "long" && k.High >= row.TakeProfit) ||
			(row.Direction == "short" && k.Low <= row.TakeProfit && k.Low > 0)
		hitSL := (row.Direction == "long" && k.Low <= row.StopPrice) ||
			(row.Direction == "short" && k.High >= row.StopPrice)
		if !filled {
			if !time.UnixMilli(k.Time).Before(fillEnd) {
				return "unfilled", 0
			}
			filled = (row.Direction == "long" && k.Low <= row.EntryPrice) ||
				(row.Direction == "short" && k.High >= row.EntryPrice)
			if !filled {
				continue
			}
			if hitSL {
				return "sl_first", row.StopPrice
			}
			// Fill-bar TP may have printed before entry; count only its SL.
			continue
		}
		if hitSL {
			// same-bar both-touch → conservative sl_first
			return "sl_first", row.StopPrice
		}
		if hitTP {
			return "tp_first", row.TakeProfit
		}
	}
	if !filled {
		return "unfilled", 0
	}
	return "timeout", bars[len(bars)-1].Close
}

func (at *AutoTrader) finishShadow(row *store.GateShadowBlock, outcome string, exit float64) {
	if err := at.store.GateShadow().MarkEvaluated(row.ID, outcome, exit, time.Now().UTC()); err != nil {
		logger.Infof("⚠️ [%s] gate shadow: mark %s #%d failed: %v", at.name, row.Symbol, row.ID, err)
		return
	}
	// review 2026-10-08 C: no_data rows (exit 0) polluted sum_r/avg_r, so
	// the log carries an R only for a real verdict (shadowRLabel gives n/a).
	logger.Infof("👻 [%s] gate shadow resolved: %s %s (%s) → %s, %s | would-be RR %.2f",
		at.name, row.Symbol, row.Direction, row.BlockedCodes, outcome, shadowRLabel(row, outcome, exit), row.PlanRR)
}

// finishShadow48 mirrors finishShadow for the 48h pass.
func (at *AutoTrader) finishShadow48(row *store.GateShadowBlock, outcome string, exit float64) {
	if err := at.store.GateShadow().MarkEvaluated48(row.ID, outcome, exit, time.Now().UTC()); err != nil {
		logger.Infof("⚠️ [%s] gate shadow 48h: mark %s #%d failed: %v", at.name, row.Symbol, row.ID, err)
		return
	}
	// review 2026-10-08 C: no_data rows (exit 0) polluted sum_r/avg_r, so
	// the log carries an R only for a real verdict (shadowRLabel gives n/a).
	logger.Infof("👻 [%s] gate shadow resolved(48h): %s %s (%s) → %s, %s | would-be RR %.2f (8h verdict: %s)",
		at.name, row.Symbol, row.Direction, row.BlockedCodes, outcome, shadowRLabel(row, outcome, exit), row.PlanRR, row.Outcome)
}

// shadowRLabel renders the R column of a resolved-shadow log line, "n/a" when
// the row has no usable R. review 2026-10-08 C: no_data rows (exit 0) polluted
// sum_r/avg_r, so they log n/a instead of a bogus R (the outcomes this file
// produces follow the same rule as shadowR in api/gate_shadow.go).
func shadowRLabel(row *store.GateShadowBlock, outcome string, exit float64) string {
	risk := row.EntryPrice - row.StopPrice
	if row.Direction == "short" {
		risk = row.StopPrice - row.EntryPrice
	}
	if outcome == "no_data" || exit <= 0 || risk <= 0 {
		return "n/a"
	}
	pnl := exit - row.EntryPrice
	if row.Direction == "short" {
		pnl = row.EntryPrice - exit
	}
	return fmt.Sprintf("%.2fR", pnl/risk)
}
