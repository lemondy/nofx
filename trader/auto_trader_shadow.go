package trader

import (
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
// against 1h candles. 8h covers the strategy's typical trade duration (the
// 30d median hold is well under it) without dragging the evaluation queue.
const gateShadowHorizon = 8 * time.Hour

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
			sl      float64
			tp      float64
			failed  []string
		}{
			{"long", gs.LongAllowed, gs.LongEntryPrice, gs.LongStopPlanPrice, gs.LongTakeProfit, gs.LongFailed},
			{"short", gs.ShortAllowed, gs.ShortEntryPrice, gs.ShortStopPlanPrice, gs.ShortTakeProfit, gs.ShortFailed},
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

// evaluateGateShadowBlocks resolves matured counterfactuals against 1h
// candles: which of TP / SL was touched first within the horizon (a bar
// touching BOTH counts as sl_first — conservative, the stop is assumed hit
// intra-bar before the target). Two passes: 8h writes `outcome`, 48h writes
// `outcome_48h`.
func (at *AutoTrader) evaluateGateShadowBlocks() {
	if at.store == nil {
		return
	}
	// --- 8h pass ---
	rows, err := at.store.GateShadow().ListMatured(at.id, time.Now().UTC().Add(-gateShadowHorizon))
	if err != nil {
		return
	}
	for _, row := range rows {
		data, err := at.getMarketTimeframes(row.Symbol, []string{"1h"}, "1h", 32)
		if err != nil || data == nil {
			continue // transient — retry next cycle
		}
		tf := data.TimeframeData["1h"]
		if tf == nil || len(tf.Klines) == 0 {
			at.finishShadow(row, "no_data", 0)
			continue
		}
		outcome, exit := resolveShadowOutcome(row, tf.Klines, row.CreatedAt)
		at.finishShadow(row, outcome, exit)
	}

	// --- 48h pass ---
	rows48, err := at.store.GateShadow().ListMatured48(at.id, time.Now().UTC().Add(-gateShadowHorizon48))
	if err != nil {
		return
	}
	for _, row := range rows48 {
		data, err := at.getMarketTimeframes(row.Symbol, []string{"1h"}, "1h", 56)
		if err != nil || data == nil {
			continue // transient — retry next cycle
		}
		tf := data.TimeframeData["1h"]
		if tf == nil || len(tf.Klines) == 0 {
			at.finishShadow48(row, "no_data", 0)
			continue
		}
		outcome, exit := resolveShadowOutcome(row, tf.Klines, row.CreatedAt)
		at.finishShadow48(row, outcome, exit)
	}
}

// resolveShadowOutcome walks 1h candles from the block's creation to the
// first TP/SL touch (same-bar both-touch → sl_first), else times out at the
// last close.
func resolveShadowOutcome(row *store.GateShadowBlock, klines []market.KlineBar, start time.Time) (string, float64) {
	outcome, exit := "timeout", 0.0
	for _, k := range klines {
		barEnd := time.UnixMilli(k.Time).Add(time.Hour)
		if barEnd.Before(start) {
			continue // bar closed before the block existed
		}
		if k.High <= 0 {
			continue
		}
		hitTP := (row.Direction == "long" && k.High >= row.TakeProfit) ||
			(row.Direction == "short" && k.Low <= row.TakeProfit && k.Low > 0)
		hitSL := (row.Direction == "long" && k.Low <= row.StopPrice) ||
			(row.Direction == "short" && k.High >= row.StopPrice)
		if hitSL {
			// same-bar both-touch → conservative sl_first
			return "sl_first", row.StopPrice
		}
		if hitTP {
			return "tp_first", row.TakeProfit
		}
		exit = k.Close
	}
	if outcome == "timeout" && exit == 0 {
		exit = row.EntryPrice
	}
	return outcome, exit
}

func (at *AutoTrader) finishShadow(row *store.GateShadowBlock, outcome string, exit float64) {
	if err := at.store.GateShadow().MarkEvaluated(row.ID, outcome, exit, time.Now().UTC()); err != nil {
		logger.Infof("⚠️ [%s] gate shadow: mark %s #%d failed: %v", at.name, row.Symbol, row.ID, err)
		return
	}
	risk := row.EntryPrice - row.StopPrice
	if row.Direction == "short" {
		risk = row.StopPrice - row.EntryPrice
	}
	pnlR := 0.0
	if risk > 0 {
		pnl := exit - row.EntryPrice
		if row.Direction == "short" {
			pnl = row.EntryPrice - exit
		}
		pnlR = pnl / risk
	}
	logger.Infof("👻 [%s] gate shadow resolved: %s %s (%s) → %s, %.2fR | would-be RR %.2f",
		at.name, row.Symbol, row.Direction, row.BlockedCodes, outcome, pnlR, row.PlanRR)
}

// finishShadow48 mirrors finishShadow for the 48h pass.
func (at *AutoTrader) finishShadow48(row *store.GateShadowBlock, outcome string, exit float64) {
	if err := at.store.GateShadow().MarkEvaluated48(row.ID, outcome, exit, time.Now().UTC()); err != nil {
		logger.Infof("⚠️ [%s] gate shadow 48h: mark %s #%d failed: %v", at.name, row.Symbol, row.ID, err)
		return
	}
	risk := row.EntryPrice - row.StopPrice
	if row.Direction == "short" {
		risk = row.StopPrice - row.EntryPrice
	}
	pnlR := 0.0
	if risk > 0 {
		pnl := exit - row.EntryPrice
		if row.Direction == "short" {
			pnl = row.EntryPrice - exit
		}
		pnlR = pnl / risk
	}
	logger.Infof("👻 [%s] gate shadow resolved(48h): %s %s (%s) → %s, %.2fR | would-be RR %.2f (8h verdict: %s)",
		at.name, row.Symbol, row.Direction, row.BlockedCodes, outcome, pnlR, row.PlanRR, row.Outcome)
}
