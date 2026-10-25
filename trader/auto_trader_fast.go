package trader

import (
	"math"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"time"
)

type fastMark struct {
	at    time.Time
	price float64
}
type fastMoveState struct {
	entry    float64
	marks    []fastMark
	lastTrim time.Time
	warnedAt time.Time
}
type fastATRValue struct {
	pct float64
	at  time.Time
}

func (at *AutoTrader) cacheFastATR(symbol string, data *market.Data) {
	at.fastMovesMu.Lock()
	defer at.fastMovesMu.Unlock()
	if at.fastATR == nil {
		at.fastATR = map[string]fastATRValue{}
	}
	if data == nil || data.TimeframeData["1h"] == nil {
		delete(at.fastATR, symbol)
		return
	}
	if names, _ := staleAmong(data, "1h"); len(names) > 0 {
		delete(at.fastATR, symbol)
		return
	}
	series := *data.TimeframeData["1h"]
	series.Timeframe = "1h" // retain the closed-hour ATR convention even on legacy snapshots
	pct := atrPercentFromSeries(&series)
	if pct <= 0 || math.IsNaN(pct) || math.IsInf(pct, 0) {
		delete(at.fastATR, symbol)
		return
	}
	at.fastATR[symbol] = fastATRValue{pct, time.Now()}
}
func (at *AutoTrader) cachedFastATR(symbol string) float64 {
	at.fastMovesMu.Lock()
	defer at.fastMovesMu.Unlock()
	v := at.fastATR[symbol]
	if age := time.Since(v.at); age < 0 || age > 10*time.Minute {
		return 0
	}
	return v.pct
}

// Rolling excursions end at the current mark, measured from preceding
// extrema. This catches both a one-way dump and a pump followed by reversal.
func (s *fastMoveState) observe(now time.Time, price float64, window time.Duration, side string) (adverse, favorable float64) {
	start := 0
	for start < len(s.marks) && now.Sub(s.marks[start].at) > window {
		start++
	}
	s.marks = s.marks[start:]
	if len(s.marks) > 0 && !now.After(s.marks[len(s.marks)-1].at) {
		return 0, 0
	}
	hi, lo := price, price
	for _, p := range s.marks {
		hi = math.Max(hi, p.price)
		lo = math.Min(lo, p.price)
	}
	s.marks = append(s.marks, fastMark{now, price})
	if side == "long" {
		return (hi - price) / hi * 100, (price - lo) / lo * 100
	}
	return (price - lo) / lo * 100, (hi - price) / hi * 100
}

// processFastMove consumes a fresh mark/position snapshot. No market data
// or position REST polling here: only actual stop/close order operations.
// All callers hold executionMutex. Returns true only for a successful trim,
// updating this snapshot's quantity so later rules cannot use the old size.
func (at *AutoTrader) processFastMove(pos map[string]interface{}, mark, atrPct float64, now time.Time) bool {
	if at.config.StrategyConfig == nil {
		return false
	}
	symbol, _ := pos["symbol"].(string)
	side, _ := pos["side"].(string)
	entry, _ := pos["entryPrice"].(float64)
	signedQty, _ := pos["positionAmt"].(float64)
	qty := math.Abs(signedQty)
	if (side != "long" && side != "short") || !at.isAIManaged(symbol, side) || entry <= 0 || mark <= 0 || qty <= 0 || math.IsNaN(mark+entry+qty) || math.IsInf(mark+entry+qty, 0) {
		return false
	}
	rc := at.config.StrategyConfig.RiskControl
	window := rc.EffectiveFastWindowMinutes()
	adverseMult, favorableMult := rc.EffectiveFastAdverseATRMult(), rc.EffectiveFastFavorableATRMult()
	if window <= 0 || (adverseMult <= 0 && favorableMult <= 0) {
		return false
	}
	key := symbol + "_" + side
	at.fastMovesMu.Lock()
	if at.fastMoves == nil {
		at.fastMoves = map[string]*fastMoveState{}
	}
	st := at.fastMoves[key]
	if st == nil {
		st = &fastMoveState{entry: entry}
		at.fastMoves[key] = st
	} else if st.entry != entry {
		// Averaging/adds change entry but do not create a fresh cooldown.
		st.entry, st.marks = entry, nil
	}
	adverse, favorable := st.observe(now, mark, time.Duration(window*float64(time.Minute)), side)
	lastTrim := st.lastTrim
	if atrPct <= 0 || math.IsNaN(atrPct) || math.IsInf(atrPct, 0) {
		if st.warnedAt.IsZero() || now.Sub(st.warnedAt) >= 30*time.Minute {
			logger.Infof("[RISK CONTROL] %s fast protection ATR unavailable — fixed %.2f%% fallback", symbol, rc.EffectiveFastFallbackPct())
			st.warnedAt = now
		}
		atrPct = 0
	}
	at.fastMovesMu.Unlock()
	adverseLimit, favorableLimit := adverseMult*atrPct, favorableMult*atrPct
	if atrPct == 0 {
		if adverseMult > 0 {
			adverseLimit = rc.EffectiveFastFallbackPct()
		}
		if favorableMult > 0 {
			favorableLimit = rc.EffectiveFastFallbackPct()
		}
	}
	if favorableLimit > 0 && favorable >= favorableLimit {
		// Cover the configured round-trip fees/slippage, at least 10bps. Only
		// move when still below/above mark and strictly tighter than current SL.
		buffer := math.Max(.001, rc.EffectiveEntryRoundTripCostBps()/10000)
		target := entry * (1 + buffer)
		current := at.GetRecordedStopLoss(symbol, side)
		tighter, legal := current <= 0 || target > current, target < mark
		if side == "short" {
			target = entry * (1 - buffer)
			tighter = current <= 0 || target < current
			legal = target > mark
		}
		if tighter && legal {
			if err := at.moveStopExchange(symbol, side, target, qty); err != nil {
				logger.Warnf("[RISK CONTROL] %s fast favorable stop failed: %v", symbol, err)
			} else {
				at.SetRecordedStopLoss(symbol, side, target)
			}
		}
	}
	fraction, cooldown := rc.EffectiveFastAdverseReduceFrac(), rc.EffectiveFastAdverseCooldownMinutes()
	if adverseLimit <= 0 || adverse < adverseLimit || fraction <= 0 || cooldown <= 0 || (!lastTrim.IsZero() && now.Sub(lastTrim) < time.Duration(cooldown*float64(time.Minute))) {
		return false
	}
	// Preserve the existing pre-breakeven no-trim contract, even on a violent
	// loss: skip rather than weaken that rule. Snapshot qty prevents fast REST.
	if err := at.ensureBreakevenBeforeAutomatedTrim(symbol, side, entry, mark, qty); err != nil {
		logger.Infof("[RISK CONTROL] %s fast adverse trim deferred: breakeven not secured: %v", symbol, err)
		return false
	}
	snapshot := map[string]interface{}{"symbol": symbol, "side": side, "positionAmt": signedQty, "markPrice": mark}
	decision := &kernel.Decision{Symbol: symbol, Action: "partial_close_" + side, CloseFraction: fraction}
	if err := at.executePartialCloseWithRecord(decision, &store.DecisionAction{}, side, snapshot); err != nil {
		logger.Warnf("[RISK CONTROL] %s fast adverse trim failed: %v", symbol, err)
		return false
	}
	at.fastMovesMu.Lock()
	st.lastTrim = now
	st.marks = []fastMark{{now, mark}}
	at.fastMovesMu.Unlock()
	pos["positionAmt"] = signedQty * (1 - fraction)
	logger.Infof("[RISK CONTROL] %s %s fast adverse %.2f%% >= %.2f%% — reduced %.0f%%", symbol, side, adverse, adverseLimit, fraction*100)
	return true
}
