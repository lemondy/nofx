package kernel

import (
	"nofx/store"
)

// Anchor offset scaling (review 2026-09-07): a fixed percent offset is either
// most of a quiet symbol's ATR (the pullback never arrives, GTC orders time
// out) or a hair off the live price for violent movers (fills like a market
// order, no slippage protection). The offset and the anchor-vs-structure
// breathing-room threshold therefore scale with the execution timeframe's
// ATR, clamped to a sane band, and fall back to the configured fixed percents
// when ATR is unavailable or the strategy pins mode "fixed".

const (
	// Defaults applied when the strategy config leaves the fields zero —
	// older configs predate them and should ride the volatility-scaled
	// default without a JSON migration.
	DefaultOffsetATRMult  = 0.5  // offset = 0.5 × ATR(execution TF)
	DefaultOffsetMinPct   = 0.15 // clamp floor, percent of live price
	DefaultOffsetMaxPct   = 1.2  // clamp ceiling
	DefaultOffsetFixedPct = 0.5  // fixed fallback when ATR is unavailable

	AnchorOffsetModeATR   = "atr"
	AnchorOffsetModeFixed = "fixed"
)

// AnchorOffsetConfig carries the limit-entry anchor parameters from
// risk_control (limit_entry_offset_mode / _atr_mult / _min_pct / _max_pct,
// with LimitEntryOffsetPct as the fixed value).
type AnchorOffsetConfig struct {
	Mode    string
	FixedPct float64
	ATRMult float64
	MinPct  float64
	MaxPct  float64
}

// AnchorOffsetFromRiskControl extracts the anchor-offset parameters from the
// strategy's risk_control block.
func AnchorOffsetFromRiskControl(rc *store.RiskControlConfig) AnchorOffsetConfig {
	if rc == nil {
		return AnchorOffsetConfig{}
	}
	return AnchorOffsetConfig{
		Mode:    rc.LimitEntryOffsetMode,
		FixedPct: rc.LimitEntryOffsetPct,
		ATRMult: rc.LimitEntryOffsetATRMult,
		MinPct:  rc.LimitEntryOffsetMinPct,
		MaxPct:  rc.LimitEntryOffsetMaxPct,
	}
}

// normalized applies zero-value defaults. Mode "fixed" must be set
// explicitly; anything else (empty included) rides the ATR default.
func (c AnchorOffsetConfig) normalized() AnchorOffsetConfig {
	if c.Mode != AnchorOffsetModeFixed {
		c.Mode = AnchorOffsetModeATR
	}
	if c.FixedPct <= 0 {
		c.FixedPct = DefaultOffsetFixedPct
	}
	if c.ATRMult <= 0 {
		c.ATRMult = DefaultOffsetATRMult
	}
	if c.MinPct <= 0 {
		c.MinPct = DefaultOffsetMinPct
	}
	if c.MaxPct < c.MinPct {
		c.MaxPct = DefaultOffsetMaxPct
	}
	return c
}

func clampPct(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// AnchorOffsetPct resolves the effective anchor offset (percent below/above
// the live price) for a symbol whose yardstick ATR is atrPct percent.
func AnchorOffsetPct(atrPct float64, cfg AnchorOffsetConfig) float64 {
	cfg = cfg.normalized()
	if cfg.Mode == AnchorOffsetModeATR && atrPct > 0 {
		return clampPct(cfg.ATRMult*atrPct, cfg.MinPct, cfg.MaxPct)
	}
	return cfg.FixedPct
}

// AnchorATRPct returns the volatility yardstick for the anchor offset: ATR(1h),
// the trend timeframe's rhythm (user directive 2026-09-10 — the execution TF's
// ATR tracked 15m micro-noise and pinned low-vol symbols to the clamp floor).
// 1h is a role TF in every strategy so it is always populated; the execution
// TF is the fallback when 1h is somehow missing.
func (sig *SymbolSignal) AnchorATRPct() float64 {
	if sig == nil {
		return 0
	}
	if t1h, ok := sig.Timeframes["1h"]; ok && t1h != nil {
		return t1h.ATRPct
	}
	if t := primaryTFSignal(sig, sig.RoleTFs.ExecutionTF); t != nil {
		return t.ATRPct
	}
	return 0
}

// ExecutionATRPct returns the ATR (percent) of the signal's execution
// timeframe — the yardstick for the breathing-room threshold, NOT the offset
// (2026-09-10: threshold == offset on the 1h scale degenerates — a 0.5×ATR(1h)
// corridor is so deep that price sits under some 1h pivot most of the time in
// chop, suppressing ~80% of anchors; the spec's step-4 check is 0.5×ATR(执行周期)).
func (sig *SymbolSignal) ExecutionATRPct() float64 {
	if sig == nil {
		return 0
	}
	tf := primaryTFSignal(sig, sig.RoleTFs.ExecutionTF)
	if tf != nil {
		return tf.ATRPct
	}
	return 0
}

// AnchorBreathingPct resolves the breathing-room threshold between an anchor
// and the opposite-side structure. Same scaling shape and the same ATR(1h)
// yardstick as the offset so the prompt-time suppression (kernel) and the
// execution-time supply-zone gate (trader) always agree; fixedPct is the
// legacy open_reject_supply_pct fallback used in fixed mode or when ATR is
// unavailable.
func AnchorBreathingPct(atrPct float64, cfg AnchorOffsetConfig, fixedPct float64) float64 {
	cfg = cfg.normalized()
	if cfg.Mode == AnchorOffsetModeATR && atrPct > 0 {
		return clampPct(cfg.ATRMult*atrPct, cfg.MinPct, cfg.MaxPct)
	}
	return fixedPct
}

// EarlyCloseHours resolves the early-close hold threshold in hours (user
// directive 2026-09-11): 0/unset = the spec default 4h; negative disables the
// gate. Shared by the trader's early-close gate and the prompt builder so the
// rendered hour count always matches what the gate enforces.
func EarlyCloseHours(rc *store.RiskControlConfig) int {
	if rc == nil {
		return 4
	}
	if rc.EarlyCloseMinHours < 0 {
		return 0 // disabled
	}
	if rc.EarlyCloseMinHours == 0 {
		return 4
	}
	return rc.EarlyCloseMinHours
}

// TpTrimProfitPct / TpFullProfitPct resolve the TP-ladder thresholds on
// leveraged PnL%: 0/unset = spec defaults (trim 10, full 25); negative turns
// that tier off. Shared by the drawdown monitor and the prompt builder.
func TpTrimProfitPct(rc *store.RiskControlConfig) float64 {
	if rc == nil || rc.TpTrimProfitPct < 0 {
		return -1 // tier off
	}
	if rc.TpTrimProfitPct == 0 {
		return 10
	}
	return rc.TpTrimProfitPct
}

func TpFullProfitPct(rc *store.RiskControlConfig) float64 {
	if rc == nil || rc.TpFullProfitPct < 0 {
		return -1 // tier off
	}
	if rc.TpFullProfitPct == 0 {
		return 25
	}
	return rc.TpFullProfitPct
}

// TpTierAction maps the current leveraged PnL% to the ladder step: "full"
// (close everything), "trim" (market-trim 1/3, once per position), or "".
// While the R-based profit lock is active (ProfitLockRMult > 0, the default)
// the ROE trim tier yields to it — the lock already trims 50% at 1R and the
// remaining half must run to the structural target, not get whittled again.
func TpTierAction(pnlPct float64, trimDone bool, rc *store.RiskControlConfig) string {
	if full := TpFullProfitPct(rc); full > 0 && pnlPct >= full {
		return "full"
	}
	if ProfitLockRMult(rc) > 0 {
		return "" // 1R/50% lock supersedes the ROE trim tier
	}
	if trim := TpTrimProfitPct(rc); trim > 0 && !trimDone && pnlPct >= trim {
		return "trim"
	}
	return ""
}

// ProfitLockRMult resolves the R-multiple that arms the profit lock
// (breakeven stop + 50% trim): 0/unset = 1.0 default; negative = disabled.
func ProfitLockRMult(rc *store.RiskControlConfig) float64 {
	if rc == nil {
		return 1.0
	}
	if rc.ProfitLockAtR < 0 {
		return 0 // disabled
	}
	if rc.ProfitLockAtR == 0 {
		return 1.0
	}
	return rc.ProfitLockAtR
}

// ProfitLockTargets returns (breakeven, trim) for a position given its R
// multiple — the two 1R actions of the profit lock. currentSL breaks the
// breakeven check once the stop already sits at/beyond entry.
func ProfitLockTargets(side string, entry, initialSL, currentSL, markPrice, lockR float64) (bool, bool) {
	if lockR <= 0 || entry <= 0 || initialSL <= 0 || markPrice <= 0 {
		return false, false
	}
	initialDist := (entry - initialSL) / entry * 100
	if initialDist < 0 {
		initialDist = -initialDist
	}
	if initialDist <= 0 {
		return false, false
	}
	var pnlDist float64
	if side == "long" {
		pnlDist = (markPrice - entry) / entry * 100
	} else {
		pnlDist = (entry - markPrice) / entry * 100
	}
	rm := pnlDist / initialDist
	if rm < lockR {
		return false, false
	}
	// Breakeven: the recorded stop still leaves room on the entry side.
	breakeven := (side == "long" && currentSL < entry) || (side == "short" && currentSL > entry)
	return breakeven, true
}

// MaxSpreadPct resolves the order-book spread gate threshold (percent of
// mid): 0/unset = 0.5% default; negative = gate disabled. Shared by the
// trader's spread gate and the prompt builder.
func MaxSpreadPct(rc *store.RiskControlConfig) float64 {
	if rc == nil {
		return 0.5
	}
	if rc.MaxSpreadPct < 0 {
		return -1 // disabled
	}
	if rc.MaxSpreadPct == 0 {
		return 0.5
	}
	return rc.MaxSpreadPct
}

// R1Price resolves the resting 1R trim price: entry ± the initial stop
// distance (long above, short below). Degenerate basis (stop == entry, e.g.
// post-breakeven with no recorded history) returns entry itself — the trim
// then rests at breakeven and locks on any dip back to it.
func R1Price(side string, entry, initialSL float64) float64 {
	if entry <= 0 || initialSL <= 0 {
		return 0
	}
	if side == "long" {
		return entry + (entry - initialSL)
	}
	return entry - (initialSL - entry)
}
