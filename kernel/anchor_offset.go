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
	Mode     string
	FixedPct float64
	ATRMult  float64
	MinPct   float64
	MaxPct   float64
}

// AnchorOffsetFromRiskControl extracts the anchor-offset parameters from the
// strategy's risk_control block.
func AnchorOffsetFromRiskControl(rc *store.RiskControlConfig) AnchorOffsetConfig {
	if rc == nil {
		return AnchorOffsetConfig{}
	}
	return AnchorOffsetConfig{
		Mode:     rc.LimitEntryOffsetMode,
		FixedPct: rc.LimitEntryOffsetPct,
		ATRMult:  rc.LimitEntryOffsetATRMult,
		MinPct:   rc.LimitEntryOffsetMinPct,
		MaxPct:   rc.LimitEntryOffsetMaxPct,
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
// EXACT TF only: when the execution dataset lacked 15m this used to fall back
// to the longest available series (4h ATR ≈ 8× a quiet 15m's) — the
// supply-zone gate then ran a wildly inflated threshold against anchors the
// prompt-side suppression had blessed, rejecting the same setup every cycle
// (2026-09-19 BTCUSDT loop). Missing TF → 0 → AnchorBreathingPct's fixed
// fallback, the same convention the prompt side uses when ATR is unavailable.
func (sig *SymbolSignal) ExecutionATRPct() float64 {
	if sig == nil {
		return 0
	}
	if tf := sig.Timeframes[sig.RoleTFs.ExecutionTF]; tf != nil {
		return tf.ATRPct
	}
	return 0
}

// DefaultPumpGuard4hPct — the 4h trend-window return at/above which a coin
// counts as an extended pump and longs need a confirmed pullback.
const DefaultPumpGuard4hPct = 20.0

// PumpGuard4h resolves the extended-pump long guard threshold from
// risk_control: 0 = built-in default, negative = guard disabled (returns 0).
// Single definition — the prompt builder, the signal layer and the trader's
// execution recompute all read through this.
func PumpGuard4h(rc *store.RiskControlConfig) float64 {
	if rc == nil || rc.PumpGuard4hPct == 0 {
		return DefaultPumpGuard4hPct
	}
	if rc.PumpGuard4hPct < 0 {
		return 0
	}
	return rc.PumpGuard4hPct
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
// When tp_full_yields_to_lock is set, the FULL tier yields too; default off
// (the 09-21 exit experiment is measuring the current ladder — flipping the
// full tier mid-experiment would invalidate its R-distribution retest).
func TpTierAction(pnlPct float64, trimDone bool, rc *store.RiskControlConfig) string {
	lockActive := ProfitLockRMult(rc) > 0
	if full := TpFullProfitPct(rc); full > 0 && pnlPct >= full {
		if !(lockActive && rc != nil && rc.TpFullYieldsToLock) {
			return "full"
		}
	}
	if lockActive && rc != nil && rc.TrimYieldsToLock() {
		return "" // 09-21 default: the 1R/50% lock supersedes the ROE trim tier
	}
	// tp_trim_yields_to_lock=false: the trim tier stays live alongside the
	// lock — division of labor (trim trims once at its threshold, the lock
	// only moves the stop to breakeven).
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

// ProfitLockTargets returns (bePrice, trim) for a position given its R
// multiple — the two 1R actions of the profit lock. bePrice > 0 means "move
// the stop to this price"; 0 means the stop already sits at-or-beyond it.
// initialSL is the OPENING-risk stop the position was planned with (the fixed
// R anchor; the trader persists it per position and never rewrites it);
// currentSL is the LIVE recorded stop and is what gates the breakeven check:
// once the lock has armed, the live stop sits at-or-above bePrice, so the
// condition goes false and arms exactly once — idempotent with no separate
// bookkeeping. initialSL and currentSL are intentionally different numbers —
// do not collapse the anchor onto the live stop: every tighten above entry
// would then lower the 1R bar (ONDOUSDT 2026-09-17: +0.49% read as 1.89R
// after two tightens, trimming at 0.352 instead of true 1R 0.3578).
// beOffsetR parks the stop PAST entry (entry ± beOffsetR × opening-risk
// distance) so a post-lock pullback can't scratch the runner back to flat
// (09-21 user experiment; resolve via ProfitLockBreakevenOffsetR).
func ProfitLockTargets(side string, entry, initialSL, currentSL, markPrice, lockR, beOffsetR float64) (float64, bool) {
	if lockR <= 0 || entry <= 0 || initialSL <= 0 || markPrice <= 0 {
		return 0, false
	}
	initialDistPct := (entry - initialSL) / entry * 100
	if initialDistPct < 0 {
		initialDistPct = -initialDistPct
	}
	if initialDistPct <= 0 {
		return 0, false
	}
	var pnlDist float64
	if side == "long" {
		pnlDist = (markPrice - entry) / entry * 100
	} else {
		pnlDist = (entry - markPrice) / entry * 100
	}
	rm := pnlDist / initialDistPct
	if rm < lockR {
		return 0, false
	}
	// Breakeven (+ optional R-offset): the recorded stop still leaves room
	// on the entry side of the target level.
	be := entry
	if beOffsetR > 0 {
		rDist := entry - initialSL
		if rDist < 0 {
			rDist = -rDist
		}
		if side == "long" {
			be = entry + rDist*beOffsetR
		} else {
			be = entry - rDist*beOffsetR
		}
	}
	if (side == "long" && currentSL < be) || (side == "short" && currentSL > be) {
		return be, true
	}
	return 0, true
}

// ProfitLockBreakevenOffsetR resolves how far past entry the 1R lock parks
// the stop, in R units: 0/unset = 0.2 default (09-21 user experiment: pure
// breakeven runners got scratched back to flat by post-lock noise); negative
// = 0 (classic breakeven at entry). Shared by the trader's lock and the
// prompt builder.
func ProfitLockBreakevenOffsetR(rc *store.RiskControlConfig) float64 {
	if rc == nil || rc.ProfitLockBEOffsetR == 0 {
		return 0.2
	}
	if rc.ProfitLockBEOffsetR < 0 {
		return 0
	}
	return rc.ProfitLockBEOffsetR
}

// TPCloseFraction resolves the fraction of the position the take-profit algo
// closes at the planned structure level: 0/unset = 0.5 default (09-21 user
// experiment — a resting full-size TP structurally sold every spike top:
// BTCUSDT 2026-09-21, TP filled 83000 and price printed 84275 in the same
// minute); negative = 1.0 (legacy full close); anything else clamps to
// [0.05, 1]. The TRADER collapses this to 1.0 when the trailing stop is
// disabled — a runner without a ratchet just gives the move back.
func TPCloseFraction(rc *store.RiskControlConfig) float64 {
	f := 0.5
	if rc != nil {
		if rc.TPCloseFraction < 0 {
			f = 1.0
		} else if rc.TPCloseFraction > 0 {
			f = rc.TPCloseFraction
		}
	}
	if f < 0.05 {
		f = 0.05
	}
	if f > 1.0 {
		f = 1.0
	}
	return f
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

// EffectiveMaxVendorDivergencePct resolves the vendor-vs-live divergence
// hard-gate threshold (percent): 0/unset = 1% default (user directive
// 09-19: beyond 1% the vendor tick is too far off the live price for
// entry/SL/TP precision to mean anything); negative = gate disabled (house
// convention, mirrors MaxSpreadPct).
func EffectiveMaxVendorDivergencePct(rc *store.RiskControlConfig) float64 {
	if rc == nil {
		return 1
	}
	if rc.MaxVendorDivergencePct < 0 {
		return -1 // disabled
	}
	if rc.MaxVendorDivergencePct == 0 {
		return 1
	}
	return rc.MaxVendorDivergencePct
}
