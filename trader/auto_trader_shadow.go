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
// intra-bar before the target).
func (at *AutoTrader) evaluateGateShadowBlocks() {
	if at.store == nil {
		return
	}
	rows, err := at.store.GateShadow().ListMatured(at.id, time.Now().UTC().Add(-gateShadowHorizon))
	if err != nil || len(rows) == 0 {
		return
	}
	for _, row := range rows {
		data, err := market.GetWithTimeframes(row.Symbol, []string{"1h"}, "1h", 32)
		if err != nil || data == nil {
			continue // transient — retry next cycle
		}
		tf := data.TimeframeData["1h"]
		if tf == nil || len(tf.Klines) == 0 {
			at.finishShadow(row, "no_data", 0)
			continue
		}
		start := row.CreatedAt
		outcome, exit := "timeout", 0.0
		for _, k := range tf.Klines {
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
				outcome, exit = "sl_first", row.StopPrice
				break
			}
			if hitTP {
				outcome, exit = "tp_first", row.TakeProfit
				break
			}
			exit = k.Close
		}
		if outcome == "timeout" && exit == 0 {
			exit = row.EntryPrice
		}
		at.finishShadow(row, outcome, exit)
	}
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
