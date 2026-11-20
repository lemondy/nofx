"""Frozen fq_crypto S1 (K=16) Short-Squeeze Defense & Stop-Loss Cooldown + PR #121 4-Switch Ablation.

Strictly bound to frozen pre-registration:
  docs/research/2026-11-20-fq-crypto-squeeze-cooldown-s1-prereg.md
  PREREG_SHA256 = c4d5478e8a3a713721e793c586cbd4497e166019f6c2064c1063b69e766e1c67

All portfolio backtests execute strictly through ``scripts/backtest/nt_engine.py``
(NautilusTrader 1.222.0 + QuantStats 0.0.86) with native ``CarryFundingModule`` (8h funding)
and native ``PortfolioRiskOverlay`` (STOP_MARKET, vol-adaptive Chandelier, drawdown_scale,
stop-loss cooldown, rolling alpha-decay exit).
"""
from __future__ import annotations

import argparse
from concurrent.futures import ProcessPoolExecutor, as_completed
import datetime as dt
import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np
import pandas as pd

from scripts.backtest import evolution_ledger, incremental_filter, ltw_crypto_study as ltw, nt_engine
from scripts.factor_mining import ensemble, panel, quant_targets


REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / "docs/research/2026-11-20-fq-crypto-squeeze-cooldown-s1-prereg.md"
PREREG_SHA256 = "c4d5478e8a3a713721e793c586cbd4497e166019f6c2064c1063b69e766e1c67"
LEDGER = REPO / "data/evolution/ledger.jsonl"

PLANNED_K = 16
SEED = 20261120
INITIAL_CAPITAL = 100_000.0
COMMISSION = 0.0005
SLIPPAGE = 0.0003
MIN_PLACEBO_DRAWS = 200
STOP_PCT = 0.20
COOLDOWN_DAYS = 10
VOL_SPIKE_RATIO = 2.0
RET3_QUANTILE = 0.90


def verify_prereg() -> bytes:
    raw = PREREG.read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    if digest != PREREG_SHA256:
        raise ValueError(f"Preregistration SHA-256 mismatch: {digest} != {PREREG_SHA256}")
    return raw


def _ema(arr: np.ndarray, period: int) -> np.ndarray:
    """Exact parity with Go fqEMA."""
    if len(arr) == 0:
        return np.array([], dtype=float)
    out = np.empty(len(arr), dtype=float)
    k = 2.0 / (float(period) + 1.0)
    out[0] = float(arr[0])
    for i in range(1, len(arr)):
        out[i] = float(arr[i]) * k + out[i - 1] * (1.0 - k)
    return out


def compute_btc_regimes(btc_df: pd.DataFrame) -> dict[dt.date, str]:
    """Strictly causal BTC daily regime (exact parity with Go fqDetectDailyRegime)."""
    closes = btc_df["close"].dropna().sort_index()
    vals = closes.to_numpy(dtype=float)
    e20 = _ema(vals, 20)
    e50 = _ema(vals, 50)
    out: dict[dt.date, str] = {}
    for i, ts in enumerate(closes.index):
        if i + 1 < 60:
            out[ts.date()] = "unknown"
        elif e20[i] > e50[i] and vals[i] > e20[i]:
            out[ts.date()] = "bull"
        elif e20[i] < e50[i] and vals[i] < e20[i]:
            out[ts.date()] = "bear"
        else:
            out[ts.date()] = "chop"
    return out


def precompute_causal_vol_atr(
    frames: dict[str, pd.DataFrame],
) -> tuple[dict[dt.date, dict[str, float]], dict[dt.date, dict[str, float]]]:
    """Strictly causal daily vol30 (ddof=1) and ATR14 matching Go fqCalcVol30/fqCalcATR14."""
    vol30_by_day: dict[dt.date, dict[str, float]] = {}
    atr14_by_day: dict[dt.date, dict[str, float]] = {}
    for sym, df in frames.items():
        c = df["close"].astype(float)
        h = df["high"].astype(float)
        lo = df["low"].astype(float)
        rets = c.pct_change(fill_method=None)
        v30 = rets.rolling(30, min_periods=30).std(ddof=1)
        prev_c = c.shift(1)
        tr = pd.concat([(h - lo), (h - prev_c).abs(), (lo - prev_c).abs()], axis=1).max(axis=1)
        atr = tr.rolling(14, min_periods=14).mean()
        for ts, val in v30.dropna().items():
            if math.isfinite(float(val)) and float(val) > 0:
                vol30_by_day.setdefault(ts.date(), {})[sym] = float(val)
        for ts, val in atr.dropna().items():
            if math.isfinite(float(val)) and float(val) > 0:
                atr14_by_day.setdefault(ts.date(), {})[sym] = float(val)
    return vol30_by_day, atr14_by_day


def _apply_go_regime_tilt(
    base_tgt: dict[str, Any],
    *,
    regime: str,
    close_slice: pd.DataFrame,
    date: pd.Timestamp,
) -> dict[str, float]:
    """Exact parity with Go fqApplyRegimeTilt for crypto."""
    base_w = dict(base_tgt.get("weights") or {})
    if regime not in ("bull", "bear") or base_tgt.get("status") != "ok":
        return base_w
    pct_map = base_tgt.get("pct") or {}
    excluded = base_tgt.get("excluded") or {}
    ordered = sorted(
        [(s, float(p)) for s, p in pct_map.items() if math.isfinite(float(p))],
        key=lambda kv: (kv[1], kv[0]),
    )
    n = len(ordered)
    if n < 15:
        return base_w
    counter_n = min(10, max(1, n // 3))
    favored_n = min(20, n - counter_n)
    if regime == "bull":
        long_n, short_n = favored_n, counter_n
    else:
        long_n, short_n = counter_n, favored_n
    short_cands = [s for s, _ in ordered[:short_n]]
    long_cands = [s for s, _ in ordered[n - long_n :]]

    # Causal vol30, listing count, and ret7 on `date`
    rets = close_slice.tail(31).pct_change(fill_method=None)
    vol30 = rets.rolling(30, min_periods=30).std(ddof=1).iloc[-1]
    history = close_slice.notna().sum()
    ret7_row = close_slice.loc[date] / close_slice.reindex([date - dt.timedelta(days=7)]).iloc[0] - 1.0
    valid_r7 = ret7_row.reindex([s for s, _ in ordered]).dropna()
    r7_thr = float(valid_r7.quantile(0.90)) if len(valid_r7) > 0 else math.inf

    longs = [s for s in long_cands if not excluded.get(s) and math.isfinite(vol30.get(s, np.nan)) and vol30.get(s, 0) > 0]
    shorts = []
    for s in short_cands:
        if excluded.get(s):
            continue
        if int(history.get(s, 0)) < 90:
            continue
        r7 = ret7_row.get(s, np.nan)
        if math.isfinite(r7) and float(r7) >= r7_thr:
            continue
        v = vol30.get(s, np.nan)
        if not math.isfinite(v) or float(v) <= 0:
            continue
        shorts.append(s)

    if not longs or not shorts:
        return base_w

    total = float(len(longs) + len(shorts))
    if regime == "bull":
        long_gross = max(len(longs) / total, 0.5)
        short_gross = 1.0 - long_gross
        if len(longs) * 0.10 + 1e-9 < short_gross:
            return base_w
    else:
        short_gross = max(len(shorts) / total, 0.5)
        long_gross = 1.0 - short_gross
        if len(shorts) * 0.10 + 1e-9 < long_gross:
            return base_w

    lv = vol30.loc[longs]
    sv = vol30.loc[shorts] ** 2  # Go uses 1/sigma^2 on short leg during tilt
    lw = quant_targets._capped_invvol(lv, long_gross, 0.10)
    sw = -quant_targets._capped_invvol(sv, short_gross, 0.10)
    merged = {s: round(float(w), 6) for s, w in pd.concat([lw, sw]).items() if round(float(w), 6) != 0}
    lg = sum(w for w in merged.values() if w > 0)
    sg = sum(-w for w in merged.values() if w < 0)
    if lg <= 0 or sg <= 0:
        return base_w
    if (regime == "bull" and lg + 1e-6 < sg) or (regime == "bear" and sg + 1e-6 < lg):
        return base_w
    return merged


def build_c1_weights_at(
    panels_obj: panel.Panels,
    scores_row: pd.Series,
    date: pd.Timestamp,
    *,
    universal_invvar_short: bool = True,
    squeeze_guard: bool = True,
) -> tuple[dict[str, float], dict[str, Any]]:
    """Build strictly causal C1 target weights on `date` plus diagnostic metadata."""
    rule = quant_targets._rule("crypto")
    close = panels_obj.fields["close"].loc[:date].reindex(columns=panels_obj.mask.columns)
    close = close.where(np.isfinite(close) & close.gt(0))
    rets = close.tail(rule["vol_window"] + 1).pct_change(fill_method=None)
    vol30 = rets.rolling(rule["vol_window"], min_periods=rule["vol_window"]).std(ddof=1).iloc[-1]
    vol5 = rets.tail(6).rolling(5, min_periods=5).std(ddof=1).iloc[-1]

    scores = scores_row.reindex(close.columns)
    eligible = panels_obj.mask.loc[date].fillna(False) & np.isfinite(scores) & np.isfinite(vol30) & vol30.gt(0)
    scores = scores[eligible]
    meta: dict[str, Any] = {"extra_dropped": 0, "base_shorts": [], "c1_shorts": [], "longs": []}
    if len(scores) < 15:
        return {}, meta

    ordered = scores.sort_values(kind="stable")
    count = max(1, int(np.floor(len(ordered) * rule["quantile"])))
    longs = list(ordered.index[-count:])
    raw_shorts = list(ordered.index[:count])

    ret7 = close.loc[date] / close.reindex([date - dt.timedelta(days=7)]).iloc[0] - 1.0
    ret7 = ret7.reindex(scores.index).where(lambda r: np.isfinite(r))
    r7_thr = float(ret7.quantile(rule["short_exclude_ret7_quantile"]))
    history = close.notna().sum()

    base_shorts = []
    for sym in raw_shorts:
        if np.isfinite(ret7.get(sym, np.nan)) and float(ret7[sym]) >= r7_thr:
            continue
        if int(history.get(sym, 0)) < rule["min_listing_days"]:
            continue
        base_shorts.append(sym)
    if not base_shorts:
        return {}, meta

    shorts = list(base_shorts)
    extra_dropped = 0
    if squeeze_guard:
        ret3 = close.loc[date] / close.reindex([date - dt.timedelta(days=3)]).iloc[0] - 1.0
        ret3 = ret3.reindex(scores.index).where(lambda r: np.isfinite(r))
        r3_thr = float(ret3.quantile(RET3_QUANTILE)) if ret3.notna().any() else math.inf
        filtered = []
        for sym in base_shorts:
            r3 = ret3.get(sym, np.nan)
            v5 = vol5.get(sym, np.nan)
            v30 = vol30.get(sym, np.nan)
            hit_r3 = math.isfinite(r3) and float(r3) >= r3_thr
            hit_vspike = math.isfinite(v5) and math.isfinite(v30) and float(v30) > 0 and (float(v5) / float(v30)) >= VOL_SPIKE_RATIO
            if hit_r3 or hit_vspike:
                extra_dropped += 1
                continue
            filtered.append(sym)
        if filtered:
            shorts = filtered
        else:
            extra_dropped = 0

    meta.update(
        extra_dropped=extra_dropped,
        base_shorts=base_shorts,
        c1_shorts=shorts,
        longs=longs,
        vol30={s: float(vol30[s]) for s in set(longs) | set(base_shorts)},
    )

    lw = quant_targets._capped_invvol(vol30.loc[longs], rule["long_gross"], rule["max_name_weight"])
    sv_series = (vol30.loc[shorts] ** 2) if universal_invvar_short else vol30.loc[shorts]
    sw = -quant_targets._capped_invvol(sv_series, rule["short_gross"], rule["max_name_weight"])
    weights = {str(s): round(float(w), 6) for s, w in pd.concat([lw, sw]).items() if round(float(w), 6) != 0}
    return weights, meta


def _daily_snapshots(
    panels_obj: panel.Panels,
    z7_df: pd.DataFrame,
    dates: list[pd.Timestamp],
    btc_regimes: dict[dt.date, str],
    *,
    regime_tilt: bool = False,
) -> tuple[pd.DataFrame, dict[dt.date, dict[str, float]], dict[dt.date, dict[str, float]], dict[dt.date, set[str]]]:
    """Build h=3 scheduled targets plus daily pct/targets/excluded maps for rolling_exit."""
    cols = sorted(panels_obj.fields["close"].columns)
    rebal_set = set(dates[::3])
    sched_rows: dict[pd.Timestamp, dict[str, float]] = {}
    daily_pct: dict[dt.date, dict[str, float]] = {}
    daily_tgt: dict[dt.date, dict[str, float]] = {}
    daily_excl: dict[dt.date, set[str]] = {}

    close_full = panels_obj.fields["close"].reindex(columns=panels_obj.mask.columns)
    for d in dates:
        row = z7_df.loc[d].where(panels_obj.mask.loc[d]).dropna()
        if row.empty:
            continue
        pct = row.rank(pct=True)
        entry = {
            "factor_ids": ["fq-crypto-h3"],
            "scores": {str(s): {"z": round(float(z), 4), "pct": round(float(pct[s]), 4)} for s, z in row.items()},
        }
        snap = {"as_of": d.strftime("%Y-%m-%d"), "run_id": "fq-study", "3": entry, "horizons": {"3": entry}}
        tgt = quant_targets.build_targets("crypto", snap, panels_obj, now=d + dt.timedelta(days=1))
        if tgt.get("status") != "ok":
            continue
        w = dict(tgt.get("weights") or {})
        if regime_tilt:
            reg = btc_regimes.get(d.date(), "unknown")
            w = _apply_go_regime_tilt(tgt, regime=reg, close_slice=close_full.loc[:d], date=d)
        daily_pct[d.date()] = {str(k): float(v) for k, v in (tgt.get("pct") or {}).items()}
        daily_tgt[d.date()] = w
        daily_excl[d.date()] = set((tgt.get("excluded") or {}).keys())
        if d in rebal_set and w:
            sched_rows[pd.Timestamp(d, tz="UTC")] = w

    tgt_df = pd.DataFrame.from_dict(sched_rows, orient="index").reindex(columns=cols, fill_value=0.0).sort_index()
    return tgt_df, daily_pct, daily_tgt, daily_excl


def _build_c1_schedule(
    panels_obj: panel.Panels,
    z7_df: pd.DataFrame,
    dates: list[pd.Timestamp],
    cols: list[str],
    *,
    universal_invvar_short: bool = True,
    squeeze_guard: bool = True,
) -> tuple[pd.DataFrame, list[dict[str, Any]]]:
    rows: dict[pd.Timestamp, dict[str, float]] = {}
    metas: list[dict[str, Any]] = []
    for d in dates[::3]:
        w, m = build_c1_weights_at(
            panels_obj,
            z7_df.loc[d],
            d,
            universal_invvar_short=universal_invvar_short,
            squeeze_guard=squeeze_guard,
        )
        m["date"] = d
        metas.append(m)
        if w:
            rows[pd.Timestamp(d, tz="UTC")] = w
    tgt_df = pd.DataFrame.from_dict(rows, orient="index").reindex(columns=cols, fill_value=0.0).sort_index()
    return tgt_df, metas


def _run_nt_with_overlay(
    sub_frames: dict[str, pd.DataFrame],
    targets: pd.DataFrame,
    *,
    eval_start: pd.Timestamp,
    eval_end: pd.Timestamp,
    carry: nt_engine.CarryConfig,
    risk_overlay: nt_engine.PortfolioRiskOverlay | None = None,
) -> dict[str, Any]:
    cols = [c for c in targets.columns if c in sub_frames]
    tgt = targets[cols].dropna(how="all")
    s_anchor = pd.Timestamp(eval_start, tz="UTC") if pd.Timestamp(eval_start).tzinfo is None else pd.Timestamp(eval_start)
    e_anchor = pd.Timestamp(eval_end, tz="UTC") if pd.Timestamp(eval_end).tzinfo is None else pd.Timestamp(eval_end)
    start_iso = (min(s_anchor, tgt.index.min()) - dt.timedelta(days=2)).strftime("%Y-%m-%d")
    end_iso = (max(e_anchor, tgt.index.max()) + dt.timedelta(days=5)).strftime("%Y-%m-%d")
    trimmed = {
        s: f.loc[start_iso:end_iso]
        for s, f in sub_frames.items()
        if s in cols and not f.loc[start_iso:end_iso].empty
    }
    tgt = tgt[[c for c in tgt.columns if c in trimmed]]
    bt = nt_engine.run_portfolio(
        trimmed,
        tgt,
        market="crypto",
        initial_capital=INITIAL_CAPITAL,
        execution="next_open",
        commission=COMMISSION,
        slippage=SLIPPAGE,
        carry=carry,
        risk_overlay=risk_overlay,
        periods_per_year=365,
    )
    full_eq = bt["equity"]
    clipped_eq = full_eq.loc[s_anchor:e_anchor]
    prior = full_eq.loc[full_eq.index < s_anchor]
    start_cap = float(prior.iloc[-1]) if len(prior) else INITIAL_CAPITAL
    bt["equity"] = clipped_eq
    bt["metrics"] = nt_engine.compute_metrics(clipped_eq, periods_per_year=365, initial_capital=start_cap)
    return bt


def _summarize_pair(bt_val: dict[str, Any], bt_ho: dict[str, Any]) -> dict[str, Any]:
    r_val = bt_val["equity"].pct_change().dropna()
    r_ho = bt_ho["equity"].pct_change().dropna()
    r_all = pd.concat([r_val, r_ho])
    eq_comb = (1.0 + r_all).cumprod() * INITIAL_CAPITAL
    m_comb = nt_engine.compute_metrics(eq_comb, periods_per_year=365, initial_capital=INITIAL_CAPITAL)
    return {
        "val": {
            "total_return": round(float(bt_val["metrics"]["total_return"]), 4),
            "sharpe": round(float(bt_val["metrics"]["sharpe"]), 3),
            "max_drawdown": round(float(bt_val["metrics"]["max_drawdown"]), 4),
            "win_rate": round(float(bt_val["metrics"]["win_rate"]), 4),
            "commission": round(float(bt_val["commission"]), 2),
            "funding": round(float(bt_val["carry"].get("funding_paid", 0.0)), 2),
            "fills": int(bt_val["fill_count"]),
            "risk_stats": bt_val["risk_stats"],
        },
        "holdout": {
            "total_return": round(float(bt_ho["metrics"]["total_return"]), 4),
            "sharpe": round(float(bt_ho["metrics"]["sharpe"]), 3),
            "max_drawdown": round(float(bt_ho["metrics"]["max_drawdown"]), 4),
            "win_rate": round(float(bt_ho["metrics"]["win_rate"]), 4),
            "commission": round(float(bt_ho["commission"]), 2),
            "funding": round(float(bt_ho["carry"].get("funding_paid", 0.0)), 2),
            "fills": int(bt_ho["fill_count"]),
            "risk_stats": bt_ho["risk_stats"],
        },
        "combined": {
            "total_return": round(float(m_comb["total_return"]), 4),
            "sharpe": round(float(m_comb["sharpe"]), 3),
            "max_drawdown": round(float(m_comb["max_drawdown"]), 4),
            "calmar": round(float(m_comb["calmar"]), 3),
            "total_stops": int(bt_val["risk_stats"]["stop_loss_count"] + bt_ho["risk_stats"]["stop_loss_count"]),
            "total_commission": round(float(bt_val["commission"] + bt_ho["commission"]), 2),
            "total_funding": round(
                float(bt_val["carry"].get("funding_paid", 0.0) + bt_ho["carry"].get("funding_paid", 0.0)), 2
            ),
            "total_fills": int(bt_val["fill_count"] + bt_ho["fill_count"]),
            "total_rolling_exits": int(
                bt_val["risk_stats"]["rolling_exit_count"] + bt_ho["risk_stats"]["rolling_exit_count"]
            ),
            "total_cooldown_blocks": int(
                bt_val["risk_stats"]["cooldown_block_count"] + bt_ho["risk_stats"]["cooldown_block_count"]
            ),
        },
    }


def _paired_daily_stats(
    r_cand_val: pd.Series, r_cand_ho: pd.Series, r_base_val: pd.Series, r_base_ho: pd.Series
) -> dict[str, Any]:
    d_val = (r_cand_val - r_base_val.reindex(r_cand_val.index).fillna(0.0)).dropna()
    d_ho = (r_cand_ho - r_base_ho.reindex(r_cand_ho.index).fillna(0.0)).dropna()
    d_all = pd.concat([d_val, d_ho]).dropna()
    n = int(len(d_all))
    mean_all = float(d_all.mean()) if n else 0.0
    sd_all = float(d_all.std(ddof=1)) if n >= 2 else 0.0
    se = sd_all / math.sqrt(n) if n >= 2 else 0.0
    z = mean_all / se if se > 0 else 0.0
    return {
        "n_days": n,
        "mean_delta_daily_bps": round(mean_all * 1e4, 3),
        "val_delta_daily_bps": round(float(d_val.mean()) * 1e4, 3) if len(d_val) else 0.0,
        "ho_delta_daily_bps": round(float(d_ho.mean()) * 1e4, 3) if len(d_ho) else 0.0,
        "mean_delta_raw": mean_all,
        "val_delta_raw": float(d_val.mean()) if len(d_val) else 0.0,
        "ho_delta_raw": float(d_ho.mean()) if len(d_ho) else 0.0,
        "z_stat": round(z, 3),
    }


def _extract_stop_schedule(fills: list[dict[str, Any]]) -> list[tuple[dt.date, str]]:
    """Return list of (stop_date, stopped_position_side) from Nautilus fills log."""
    out: list[tuple[dt.date, str]] = []
    for f in fills:
        if f.get("reason") != "stop_loss":
            continue
        # Stop fill BUY closes a short; SELL closes a long.
        pos_side = "short" if f["side"] == "BUY" else "long"
        out.append((pd.Timestamp(f["ts"]).date(), pos_side))
    return out


def _build_placebo_targets(
    metas: list[dict[str, Any]],
    cols: list[str],
    stop_events: list[tuple[dt.date, str]],
    rng: np.random.Generator,
) -> pd.DataFrame:
    """Preregistered matched random placebo for C1:
    - Randomly drop the exact same number of short candidates as C1's extra squeeze filter.
    - Randomly permute the sigma^2 vector across remaining short candidates.
    - Apply random 10-day cooldown bans matching C1's exact stop event dates & sides.
    """
    rule = quant_targets._rule("crypto")
    # Assign each C1 stop event to a random symbol from the nearest rebalance's active side
    bans: list[tuple[dt.date, dt.date, str, str]] = []  # (start, end, sym, side)
    rebal_by_date = [(m["date"].date(), m) for m in metas]
    for stop_day, stop_side in stop_events:
        # Find latest rebalance <= stop_day
        cand_syms: list[str] = []
        for rd, m in reversed(rebal_by_date):
            if rd <= stop_day:
                cand_syms = list(m["longs"] if stop_side == "long" else m["base_shorts"])
                break
        if cand_syms:
            picked = str(cand_syms[int(rng.integers(0, len(cand_syms)))])
            bans.append((stop_day, stop_day + dt.timedelta(days=COOLDOWN_DAYS), picked, stop_side))

    rows: dict[pd.Timestamp, dict[str, float]] = {}
    for m in metas:
        d = m["date"]
        dd = d.date()
        longs = list(m["longs"])
        base_shorts = list(m["base_shorts"])
        if not longs or not base_shorts:
            continue
        vol30 = m["vol30"]

        # Random extra short drops matching C1's count
        k_drop = int(m["extra_dropped"])
        shorts = list(base_shorts)
        if 0 < k_drop < len(shorts):
            drop_idx = set(int(i) for i in rng.choice(len(shorts), size=k_drop, replace=False))
            shorts = [s for idx, s in enumerate(shorts) if idx not in drop_idx]

        # Apply random cooldown bans active on dd
        active_long_bans = {sym for s0, s1, sym, sd in bans if sd == "long" and s0 <= dd < s1}
        active_short_bans = {sym for s0, s1, sym, sd in bans if sd == "short" and s0 <= dd < s1}
        longs_f = [s for s in longs if s not in active_long_bans] or longs
        shorts_f = [s for s in shorts if s not in active_short_bans] or shorts

        lv = pd.Series({s: vol30[s] for s in longs_f}, dtype=float)
        # Randomly permute sigma^2 values across shorts_f
        sv_vals = np.array([vol30[s] ** 2 for s in shorts_f], dtype=float)
        rng.shuffle(sv_vals)
        sv = pd.Series(sv_vals, index=shorts_f, dtype=float)

        lw = quant_targets._capped_invvol(lv, rule["long_gross"], rule["max_name_weight"])
        sw = -quant_targets._capped_invvol(sv, rule["short_gross"], rule["max_name_weight"])
        w = {str(s): round(float(v), 6) for s, v in pd.concat([lw, sw]).items() if round(float(v), 6) != 0}
        if w:
            rows[pd.Timestamp(d, tz="UTC")] = w
    return pd.DataFrame.from_dict(rows, orient="index").reindex(columns=cols, fill_value=0.0).sort_index()


_PLACEBO_CTX: dict[str, Any] = {}


def _init_placebo_worker(ctx: dict[str, Any]) -> None:
    global _PLACEBO_CTX
    _PLACEBO_CTX = ctx


def _run_placebo_draw(draw_idx: int) -> tuple[float, float, float]:
    ctx = _PLACEBO_CTX
    rng = np.random.default_rng(SEED + draw_idx)
    t_val = _build_placebo_targets(ctx["metas_val"], ctx["cols"], ctx["stops_val"], rng)
    t_ho = _build_placebo_targets(ctx["metas_ho"], ctx["cols"], ctx["stops_ho"], rng)
    overlay = nt_engine.PortfolioRiskOverlay(long_stop_pct=STOP_PCT, short_stop_pct=STOP_PCT)
    bt_val = _run_nt_with_overlay(
        ctx["sub_val"], t_val, eval_start=ctx["val_start"], eval_end=ctx["val_end"], carry=ctx["carry"], risk_overlay=overlay
    )
    bt_ho = _run_nt_with_overlay(
        ctx["sub_ho"], t_ho, eval_start=ctx["ho_start"], eval_end=ctx["ho_end"], carry=ctx["carry"], risk_overlay=overlay
    )
    r_val = bt_val["equity"].pct_change().dropna()
    r_ho = bt_ho["equity"].pct_change().dropna()
    stats = _paired_daily_stats(r_val, r_ho, ctx["base_r_val"], ctx["base_r_ho"])
    return stats["mean_delta_raw"], stats["val_delta_raw"], stats["ho_delta_raw"]


def run_study(*, placebo_n: int = 200, workers: int = 6, write_ledger: bool = False) -> dict[str, Any]:
    if write_ledger and placebo_n < MIN_PLACEBO_DRAWS:
        raise ValueError(f"Formal S1 ledger write requires placebo_n >= {MIN_PLACEBO_DRAWS}")
    verify_prereg()

    panels_obj = panel.load_panels("crypto", tier="all")
    carry_cfg = ltw._build_crypto_carry(panels_obj)
    syms = sorted(panels_obj.fields["close"].columns)
    frames, _ = ltw._fetch_nas_crypto_frames(syms, start="2020-01-01", end="2026-11-15")
    max_nas_naive = max(f.index.max() for f in frames.values()).tz_localize(None) - dt.timedelta(days=1)

    zoo_raw = json.loads((REPO / "data/factor_mining/crypto/zoo.json").read_text(encoding="utf-8"))
    fmap = {f["id"]: f for f in zoo_raw.get("factors", [])}
    ens_raw = json.loads((REPO / "data/factor_mining/crypto/ensemble.json").read_text(encoding="utf-8"))
    fids = ens_raw["horizons"]["3"]["factor_ids"]
    z7_df = ensemble.ensemble_scores([fmap[fid] for fid in fids], panels_obj)

    splits = panel.make_splits(panels_obj.mask.index, h=3, windows=panels_obj.windows)
    val_dates = [d for d in panels_obj.mask.index[splits["val"]] if d <= max_nas_naive]
    ho_dates = [d for d in panels_obj.mask.index[splits["holdout"]] if d <= max_nas_naive]
    cols = sorted(frames)

    def _trim(dates: list[pd.Timestamp]) -> dict[str, pd.DataFrame]:
        s0 = (dates[0] - dt.timedelta(days=2)).strftime("%Y-%m-%d")
        s1 = (dates[-1] + dt.timedelta(days=5)).strftime("%Y-%m-%d")
        return {s: f.loc[s0:s1] for s, f in frames.items() if not f.loc[s0:s1].empty}

    sub_val = _trim(val_dates)
    sub_ho = _trim(ho_dates)
    btc_regimes = compute_btc_regimes(frames["BTCUSDT"])
    vol30_map, atr14_map = precompute_causal_vol_atr(frames)

    # Pre-compute daily schedules (base & regime-tilted) for ablation
    t_base_val, pct_base_val, dtgt_base_val, excl_base_val = _daily_snapshots(
        panels_obj, z7_df, val_dates, btc_regimes, regime_tilt=False
    )
    t_base_ho, pct_base_ho, dtgt_base_ho, excl_base_ho = _daily_snapshots(
        panels_obj, z7_df, ho_dates, btc_regimes, regime_tilt=False
    )
    t_tilt_val, pct_tilt_val, dtgt_tilt_val, excl_tilt_val = _daily_snapshots(
        panels_obj, z7_df, val_dates, btc_regimes, regime_tilt=True
    )
    t_tilt_ho, pct_tilt_ho, dtgt_tilt_ho, excl_tilt_ho = _daily_snapshots(
        panels_obj, z7_df, ho_dates, btc_regimes, regime_tilt=True
    )

    # --- PART 1: PR #121 4-Switch Ablation (Descriptive Diagnostic, Non-K) ---
    ablation_specs = [
        ("B0_no_stops", False, 0.0, False, False, False),
        ("B_S20_baseline", False, STOP_PCT, False, False, False),
        ("B_plus_regime_tilt", True, STOP_PCT, False, False, False),
        ("B_plus_rolling_exit", False, STOP_PCT, True, False, False),
        ("B_plus_vol_adaptive_stop", False, STOP_PCT, False, True, False),
        ("B_plus_drawdown_scale", False, STOP_PCT, False, False, True),
        ("ALL4_live_paper", True, STOP_PCT, True, True, True),
        ("ALL4_minus_rolling_exit", True, STOP_PCT, False, True, True),
        ("ALL4_minus_regime_tilt", False, STOP_PCT, True, True, True),
        ("ALL4_minus_vol_adaptive", True, STOP_PCT, True, False, True),
        ("ALL4_minus_drawdown_scale", True, STOP_PCT, True, True, False),
    ]

    ablation: dict[str, Any] = {}
    bt_b_val = None
    bt_b_ho = None
    for name, tilt, stop_pct, roll, v_adapt, dd_sc in ablation_specs:
        tv = t_tilt_val if tilt else t_base_val
        th = t_tilt_ho if tilt else t_base_ho
        ov_val = (
            nt_engine.PortfolioRiskOverlay(
                long_stop_pct=stop_pct,
                short_stop_pct=stop_pct,
                vol_adaptive_stop=v_adapt,
                horizon=3,
                drawdown_scale=dd_sc,
                rolling_exit=roll,
                daily_pct=(pct_tilt_val if tilt else pct_base_val) if roll else {},
                daily_targets=(dtgt_tilt_val if tilt else dtgt_base_val) if roll else {},
                daily_excluded=(excl_tilt_val if tilt else excl_base_val) if roll else {},
                vol30=vol30_map if v_adapt else {},
                atr14=atr14_map if v_adapt else {},
            )
            if (stop_pct > 0 or roll or dd_sc)
            else None
        )
        ov_ho = (
            nt_engine.PortfolioRiskOverlay(
                long_stop_pct=stop_pct,
                short_stop_pct=stop_pct,
                vol_adaptive_stop=v_adapt,
                horizon=3,
                drawdown_scale=dd_sc,
                rolling_exit=roll,
                daily_pct=(pct_tilt_ho if tilt else pct_base_ho) if roll else {},
                daily_targets=(dtgt_tilt_ho if tilt else dtgt_base_ho) if roll else {},
                daily_excluded=(excl_tilt_ho if tilt else excl_base_ho) if roll else {},
                vol30=vol30_map if v_adapt else {},
                atr14=atr14_map if v_adapt else {},
            )
            if (stop_pct > 0 or roll or dd_sc)
            else None
        )
        bv = _run_nt_with_overlay(sub_val, tv, eval_start=val_dates[0], eval_end=val_dates[-1], carry=carry_cfg, risk_overlay=ov_val)
        bh = _run_nt_with_overlay(sub_ho, th, eval_start=ho_dates[0], eval_end=ho_dates[-1], carry=carry_cfg, risk_overlay=ov_ho)
        ablation[name] = _summarize_pair(bv, bh)
        if name == "B_S20_baseline":
            bt_b_val, bt_b_ho = bv, bh

    assert bt_b_val is not None and bt_b_ho is not None
    base_r_val = bt_b_val["equity"].pct_change().dropna()
    base_r_ho = bt_b_ho["equity"].pct_change().dropna()

    # --- PART 2: Pre-Registered S1 Candidate C1 (K=16) ---
    t_c1_val, metas_val = _build_c1_schedule(panels_obj, z7_df, val_dates, cols, universal_invvar_short=True, squeeze_guard=True)
    t_c1_ho, metas_ho = _build_c1_schedule(panels_obj, z7_df, ho_dates, cols, universal_invvar_short=True, squeeze_guard=True)
    c1_overlay = nt_engine.PortfolioRiskOverlay(
        long_stop_pct=STOP_PCT,
        short_stop_pct=STOP_PCT,
        stop_cooldown_days=COOLDOWN_DAYS,
        cooldown_redistribute=True,
        max_name_weight=0.10,
    )
    bt_c1_val = _run_nt_with_overlay(sub_val, t_c1_val, eval_start=val_dates[0], eval_end=val_dates[-1], carry=carry_cfg, risk_overlay=c1_overlay)
    bt_c1_ho = _run_nt_with_overlay(sub_ho, t_c1_ho, eval_start=ho_dates[0], eval_end=ho_dates[-1], carry=carry_cfg, risk_overlay=c1_overlay)
    c1_summary = _summarize_pair(bt_c1_val, bt_c1_ho)
    c1_r_val = bt_c1_val["equity"].pct_change().dropna()
    c1_r_ho = bt_c1_ho["equity"].pct_change().dropna()
    paired = _paired_daily_stats(c1_r_val, c1_r_ho, base_r_val, base_r_ho)

    # Descriptive component attribution (non-K, for understanding C1 internals)
    attribution: dict[str, Any] = {}
    for attr_name, invvar, sq, cd in [
        ("C1a_invvar_only", True, False, 0),
        ("C1b_squeeze_guard_only", False, True, 0),
        ("C1c_cooldown_only", False, False, COOLDOWN_DAYS),
    ]:
        tv_a, _ = _build_c1_schedule(panels_obj, z7_df, val_dates, cols, universal_invvar_short=invvar, squeeze_guard=sq)
        th_a, _ = _build_c1_schedule(panels_obj, z7_df, ho_dates, cols, universal_invvar_short=invvar, squeeze_guard=sq)
        ov_a = nt_engine.PortfolioRiskOverlay(
            long_stop_pct=STOP_PCT, short_stop_pct=STOP_PCT, stop_cooldown_days=cd, cooldown_redistribute=True, max_name_weight=0.10
        )
        bv_a = _run_nt_with_overlay(sub_val, tv_a, eval_start=val_dates[0], eval_end=val_dates[-1], carry=carry_cfg, risk_overlay=ov_a)
        bh_a = _run_nt_with_overlay(sub_ho, th_a, eval_start=ho_dates[0], eval_end=ho_dates[-1], carry=carry_cfg, risk_overlay=ov_a)
        attribution[attr_name] = {
            "summary": _summarize_pair(bv_a, bh_a),
            "paired": _paired_daily_stats(
                bv_a["equity"].pct_change().dropna(), bh_a["equity"].pct_change().dropna(), base_r_val, base_r_ho
            ),
        }

    # --- PART 3: Matched Random Placebo (n = placebo_n) ---
    stops_val = _extract_stop_schedule(bt_c1_val["fills"])
    stops_ho = _extract_stop_schedule(bt_c1_ho["fills"])
    placebo_deltas: list[float] = []
    if placebo_n > 0:
        ctx = {
            "metas_val": metas_val,
            "metas_ho": metas_ho,
            "cols": cols,
            "stops_val": stops_val,
            "stops_ho": stops_ho,
            "sub_val": sub_val,
            "sub_ho": sub_ho,
            "val_start": val_dates[0],
            "val_end": val_dates[-1],
            "ho_start": ho_dates[0],
            "ho_end": ho_dates[-1],
            "carry": carry_cfg,
            "base_r_val": base_r_val,
            "base_r_ho": base_r_ho,
        }
        if workers <= 1:
            _init_placebo_worker(ctx)
            for i in range(placebo_n):
                placebo_deltas.append(_run_placebo_draw(i)[0])
        else:
            with ProcessPoolExecutor(max_workers=workers, initializer=_init_placebo_worker, initargs=(ctx,)) as pool:
                futs = [pool.submit(_run_placebo_draw, i) for i in range(placebo_n)]
                for fut in as_completed(futs):
                    placebo_deltas.append(fut.result()[0])

    placebo_pct = (
        float(sum(1 for d in placebo_deltas if d < paired["mean_delta_raw"]) / len(placebo_deltas))
        if placebo_deltas
        else None
    )
    z_k = float(incremental_filter.z_threshold(PLANNED_K))
    verdict = ltw._compute_s1_verdict(
        z_stat=float(paired["z_stat"]),
        z_k=z_k,
        val_delta=float(paired["val_delta_raw"]),
        ho_delta=float(paired["ho_delta_raw"]),
        placebo_pct=placebo_pct,
        placebo_n=len(placebo_deltas),
        min_placebo_n=MIN_PLACEBO_DRAWS,
    )

    result = {
        "prereg_sha256": PREREG_SHA256,
        "engine": f"nautilus_trader {nt_engine.nt_version()}",
        "windows": {
            "val": [str(val_dates[0].date()), str(val_dates[-1].date()), len(val_dates)],
            "holdout": [str(ho_dates[0].date()), str(ho_dates[-1].date()), len(ho_dates)],
        },
        "ablation": ablation,
        "s1_k16": {
            "planned_k": PLANNED_K,
            "z_k": round(z_k, 3),
            "baseline": ablation["B_S20_baseline"],
            "candidate_c1": c1_summary,
            "paired": paired,
            "placebo": {
                "n": len(placebo_deltas),
                "percentile": round(placebo_pct, 4) if placebo_pct is not None else None,
                "mean_delta_bps": round(float(np.mean(placebo_deltas)) * 1e4, 3) if placebo_deltas else None,
                "p90_delta_bps": round(float(np.quantile(placebo_deltas, 0.90)) * 1e4, 3) if placebo_deltas else None,
            },
            "verdict": verdict,
        },
        "attribution": attribution,
    }

    if write_ledger:
        def _build_row(k_now: int, _prev: list[dict[str, Any]]) -> dict[str, Any]:
            if k_now != PLANNED_K:
                raise ValueError(f"Expected fq_crypto next_k={PLANNED_K}, got {k_now}")
            return {
                "stage": "S1",
                "change_type": "E",
                "idea_id": "fq-crypto-squeeze-cooldown-invvar-v1",
                "baseline_version": "ls_invvol_protected_v1_s20",
                "verdict": verdict,
                "preregistered_K": PLANNED_K,
                "planned_arm_K": PLANNED_K,
                "actual_ledger_K": k_now,
                "delta": paired["mean_delta_raw"],
                "z": paired["z_stat"],
                "z_threshold": round(z_k, 3),
                "val_delta": paired["val_delta_raw"],
                "holdout_delta": paired["ho_delta_raw"],
                "placebo_pct": round(placebo_pct, 4) if placebo_pct is not None else None,
                "placebo_n": len(placebo_deltas),
                "combined_sharpe_base": ablation["B_S20_baseline"]["combined"]["sharpe"],
                "combined_sharpe_cand": c1_summary["combined"]["sharpe"],
                "combined_maxdd_base": ablation["B_S20_baseline"]["combined"]["max_drawdown"],
                "combined_maxdd_cand": c1_summary["combined"]["max_drawdown"],
                "prereg_path": str(PREREG.relative_to(REPO)),
                "prereg_sha256": PREREG_SHA256,
                "engine": f"nautilus_trader {nt_engine.nt_version()}",
            }

        evolution_ledger.append_event(LEDGER, "fq_crypto", _build_row)
    return result


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--placebo-n", type=int, default=200)
    ap.add_argument("--workers", type=int, default=6)
    ap.add_argument("--write-ledger", action="store_true")
    ap.add_argument("--out", type=Path, default=None)
    args = ap.parse_args()
    res = run_study(placebo_n=args.placebo_n, workers=args.workers, write_ledger=args.write_ledger)
    text = json.dumps(res, indent=2, ensure_ascii=False)
    if args.out is not None:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(text + "\n", encoding="utf-8")
    print(text)


if __name__ == "__main__":
    main()
