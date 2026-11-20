"""Frozen fq_us S1 (K=12 Residual Momentum, K=13 Correlation De-Crowding) Study.

Strictly bound to frozen pre-registration:
  docs/research/2026-11-20-fq-us-resmom-decrowd-s1-prereg.md
  PREREG_SHA256 = 4955dd207754e112d129b44ddc110cfb74c65191d46de61d7b7a02d19cc55714

All portfolio backtests execute strictly through ``scripts/backtest/nt_engine.py``
(NautilusTrader 1.222.0 + QuantStats 0.0.86) with ``CarryConfig(market="us")``.
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

from scripts.backtest import (
    evolution_ledger,
    fq_us_momentum_study as s1,
    fq_us_volscale_study as vs,
    incremental_filter,
    ltw_crypto_study as ltw,
    nt_engine,
)
from scripts.factor_mining.quant_targets import _capped_invvol


REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / "docs/research/2026-11-20-fq-us-resmom-decrowd-s1-prereg.md"
PREREG_SHA256 = "4955dd207754e112d129b44ddc110cfb74c65191d46de61d7b7a02d19cc55714"
LEDGER = REPO / "data/evolution/ledger.jsonl"

K_RESMOM = 12
K_DECROWD = 13
SEED_H1 = 20261120
SEED_H2 = 20261121
MIN_PLACEBO_DRAWS = 200
INITIAL_CAPITAL = 100_000.0
COMMISSION = 0.0010  # Midpoint of 13bp long / 7bp short
SLIPPAGE = 0.0
US_CARRY = nt_engine.CarryConfig(
    market="us",
    us_short_borrow_annual=0.005,
    us_margin_finance_annual=0.05,
)


def verify_prereg() -> bytes:
    raw = PREREG.read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    if digest != PREREG_SHA256:
        raise ValueError(f"Preregistration SHA-256 mismatch: {digest} != {PREREG_SHA256}")
    return raw


def compute_residual_momentum_at(
    returns_df: pd.DataFrame,
    spy_rets: pd.Series,
    date_loc: int,
    eligible_syms: list[str],
    *,
    beta_override: dict[str, float] | None = None,
) -> tuple[pd.Series, dict[str, float]]:
    """Strictly causal Blitz, Huij & Martens (2011) 12-1 standardized residual momentum.

    Estimation window: [date_loc - 251, date_loc] (252 trading days).
    Momentum accumulation window: [date_loc - 251, date_loc - 21] (skip last 21 trading days).
    """
    if date_loc < 252 or not eligible_syms:
        return pd.Series(dtype=float), {}
    est_slice = slice(date_loc - 251, date_loc + 1)
    r_mat = returns_df.iloc[est_slice][eligible_syms].to_numpy(dtype=float)  # (252, M)
    spy_v = spy_rets.iloc[est_slice].to_numpy(dtype=float)  # (252,)
    n_rows, n_cols = r_mat.shape

    valid = np.isfinite(r_mat) & np.isfinite(spy_v)[:, None]
    counts = valid.sum(axis=0)
    spy_b = np.where(valid, spy_v[:, None], 0.0)
    r_b = np.where(valid, r_mat, 0.0)

    denom = np.maximum(counts, 1)
    mean_x = spy_b.sum(axis=0) / denom
    mean_y = r_b.sum(axis=0) / denom
    dx = np.where(valid, spy_v[:, None] - mean_x[None, :], 0.0)
    dy = np.where(valid, r_mat - mean_y[None, :], 0.0)
    var_x = (dx * dx).sum(axis=0)
    cov_xy = (dx * dy).sum(axis=0)

    betas_est = np.where((counts >= 126) & (var_x > 1e-12), cov_xy / np.where(var_x > 1e-12, var_x, 1.0), np.nan)
    betas_map = {sym: float(betas_est[i]) for i, sym in enumerate(eligible_syms) if np.isfinite(betas_est[i])}

    if beta_override is not None:
        betas_use = np.array([beta_override.get(sym, np.nan) for sym in eligible_syms], dtype=float)
    else:
        betas_use = betas_est

    alphas_use = mean_y - betas_use * mean_x
    resid = r_mat - (alphas_use[None, :] + betas_use[None, :] * spy_v[:, None])
    resid = np.where(valid, resid, np.nan)

    # 12-1 window: exclude the most recent 21 trading days -> indices [0 : n_rows - 21]
    mom_res = resid[: max(0, n_rows - 21), :]
    mom_cnt = np.isfinite(mom_res).sum(axis=0)
    with np.errstate(invalid="ignore"):
        res_sum = np.nansum(mom_res, axis=0)
        res_sd = np.nanstd(mom_res, axis=0, ddof=1)

    ok = np.isfinite(betas_use) & (counts >= 126) & (mom_cnt >= 100) & np.isfinite(res_sd) & (res_sd > 1e-12)
    scores = np.where(ok, res_sum / (np.where(ok, res_sd, 1.0) * np.sqrt(np.maximum(mom_cnt, 1))), np.nan)
    ser = pd.Series(scores, index=eligible_syms, dtype=float).dropna()
    return ser, betas_map


def compute_decrowd_penalties(
    returns_df: pd.DataFrame,
    date_loc: int,
    syms: list[str],
) -> tuple[pd.Series, float]:
    """Strictly causal 60d mean positive pairwise correlation penalty per name in `syms`.

    Returns (penalty_series >= 0.25, mean_pairwise_corr).
    """
    if len(syms) <= 1 or date_loc < 30:
        return pd.Series(1.0, index=syms, dtype=float), 0.0
    start_idx = max(0, date_loc - 59)
    sub = returns_df.iloc[start_idx : date_loc + 1][syms]
    corr = sub.corr(min_periods=30)
    c_mat = corr.to_numpy(dtype=float)
    n = len(syms)
    off_mask = ~np.eye(n, dtype=bool)
    pos_c = np.where(off_mask & np.isfinite(c_mat), np.maximum(0.0, c_mat), np.nan)
    raw_c = np.where(off_mask & np.isfinite(c_mat), c_mat, np.nan)
    with np.errstate(invalid="ignore"):
        avg_pos = np.nanmean(pos_c, axis=1)
        mean_pair = float(np.nanmean(raw_c)) if np.isfinite(raw_c).any() else 0.0
    avg_pos = np.where(np.isfinite(avg_pos), avg_pos, 1.0)
    pen = pd.Series(np.maximum(0.25, avg_pos), index=syms, dtype=float)
    return pen, mean_pair


def compute_leg_neff(returns_df: pd.DataFrame, date_loc: int, weights: dict[str, float]) -> float:
    """Effective number of independent bets N_eff = 1 / (w_norm^T C w_norm) for a single leg."""
    syms = [s for s, w in weights.items() if abs(w) > 0]
    if len(syms) <= 1 or date_loc < 30:
        return float(len(syms))
    start_idx = max(0, date_loc - 59)
    corr = returns_df.iloc[start_idx : date_loc + 1][syms].corr(min_periods=30).fillna(0.0).to_numpy(dtype=float)
    np.fill_diagonal(corr, 1.0)
    w = np.array([abs(weights[s]) for s in syms], dtype=float)
    s_w = float(w.sum())
    if s_w <= 0:
        return 0.0
    w = w / s_w
    denom = float(w @ corr @ w)
    return (1.0 / denom) if denom > 1e-12 else float(len(syms))


def build_arm_targets(
    frames: dict[str, pd.DataFrame],
    universe: list[str],
    calendar: pd.DatetimeIndex,
    *,
    signal_mode: str = "raw",  # 'raw' | 'resmom'
    weight_mode: str = "invvol",  # 'invvol' | 'decrowd'
    volscale: bool = False,
) -> tuple[pd.DataFrame, list[dict[str, Any]], dict[str, Any]]:
    """Build strictly causal target weight DataFrame & per-rebalance metadata."""
    features = s1.build_features(frames, universe, calendar)
    sessions, base_decisions = s1.rebalance_schedule(features, calendar)
    close_df = features["close"]
    rets_df = close_df.pct_change(fill_method=None)
    spy_close = frames["SPY"]["close"].reindex(calendar)
    spy_rets = spy_close.pct_change(fill_method=None)

    cal_pos = {d: i for i, d in enumerate(calendar)}
    rebal_rows: dict[pd.Timestamp, dict[str, float]] = {}
    metas: list[dict[str, Any]] = []
    long_corrs: list[float] = []
    short_corrs: list[float] = []
    long_neffs: list[float] = []
    short_neffs: list[float] = []

    # Build unscaled decisions first (needed if volscale=True)
    decisions_out: list[dict[str, Any]] = []
    for bd in base_decisions:
        d = pd.Timestamp(bd["date"])
        d_loc = cal_pos[d]
        elig = list(bd["eligible"])
        dec = dict(date=bd["date"], long={}, short={}, eligible=elig)
        if len(elig) < s1.PARAMS["min_eligible"]:
            decisions_out.append(dec)
            metas.append(dict(date=d, date_loc=d_loc, eligible=elig, longs=[], shorts=[], betas={}, vol={}, pen_l={}, pen_s={}))
            continue

        betas_map: dict[str, float] = {}
        if signal_mode == "resmom":
            sig, betas_map = compute_residual_momentum_at(rets_df, spy_rets, d_loc, elig)
            if len(sig) < s1.PARAMS["min_eligible"]:
                decisions_out.append(dec)
                metas.append(dict(date=d, date_loc=d_loc, eligible=elig, longs=[], shorts=[], betas=betas_map, vol={}, pen_l={}, pen_s={}))
                continue
        else:
            sig = features["signal"].loc[d, elig].dropna()

        ordered = sig.sort_values(kind="stable")
        n = max(1, math.floor(0.2 * len(ordered)))
        longs = list(ordered.index[-n:])
        shorts = list(ordered.index[:n])
        vol = features["volatility"].loc[d, sorted(set(longs + shorts))]
        pen_l, corr_l = compute_decrowd_penalties(rets_df, d_loc, longs)
        pen_s, corr_s = compute_decrowd_penalties(rets_df, d_loc, shorts)
        long_corrs.append(corr_l)
        short_corrs.append(corr_s)

        if weight_mode == "decrowd":
            lw = _capped_invvol(vol.loc[longs] * pen_l, 0.5, 0.10)
            sw = -_capped_invvol(vol.loc[shorts] * pen_s, 0.5, 0.10)
        else:
            lw = _capped_invvol(vol.loc[longs], 0.5, 0.10)
            sw = -_capped_invvol(vol.loc[shorts], 0.5, 0.10)

        lw_dict = {str(k): round(float(v), 6) for k, v in lw.items() if round(float(v), 6) != 0}
        sw_dict = {str(k): round(float(v), 6) for k, v in sw.items() if round(float(v), 6) != 0}
        long_neffs.append(compute_leg_neff(rets_df, d_loc, lw_dict))
        short_neffs.append(compute_leg_neff(rets_df, d_loc, sw_dict))

        # Store 1.0-scaled leg weights inside dec for vs.unscaled_returns compatibility
        dec["long"] = {k: v * 2.0 for k, v in lw_dict.items()}
        dec["short"] = {k: v * 2.0 for k, v in sw_dict.items()}
        decisions_out.append(dec)
        metas.append(
            dict(
                date=d,
                date_loc=d_loc,
                eligible=elig,
                longs=longs,
                shorts=shorts,
                betas=betas_map,
                vol={s: float(vol[s]) for s in set(longs + shorts)},
                pen_l=pen_l.to_dict(),
                pen_s=pen_s.to_dict(),
            )
        )
        merged = dict(lw_dict)
        merged.update(sw_dict)
        rebal_rows[pd.Timestamp(d, tz="UTC")] = merged

    if volscale and decisions_out:
        # Causal Volscale k_t using exact fq_us_volscale_study formula (126d sigma / expanding median >= 252d)
        start_d = pd.Timestamp(decisions_out[0]["date"])
        close_sub = close_df.loc[start_d : pd.Timestamp(decisions_out[-1]["date"])]
        u_rets = vs.unscaled_returns(close_sub, decisions_out)
        scales_df = vs.volatility_signal(u_rets)
        k_map = {str(ts.date()): float(v) for ts, v in scales_df["k"].items()}
        for ts in list(rebal_rows.keys()):
            k = k_map.get(str(ts.date()), 1.0)
            rebal_rows[ts] = {s: round(w * k, 6) for s, w in rebal_rows[ts].items()}

    cols = sorted({s for row in rebal_rows.values() for s in row})
    tgt_df = pd.DataFrame.from_dict(rebal_rows, orient="index").reindex(columns=cols, fill_value=0.0).sort_index()
    crowd_stats = {
        "mean_long_corr": round(float(np.mean(long_corrs)), 4) if long_corrs else 0.0,
        "mean_short_corr": round(float(np.mean(short_corrs)), 4) if short_corrs else 0.0,
        "mean_long_neff": round(float(np.mean(long_neffs)), 2) if long_neffs else 0.0,
        "mean_short_neff": round(float(np.mean(short_neffs)), 2) if short_neffs else 0.0,
        "rebalances": len(tgt_df),
        "sessions_start": str(sessions[0].date()),
        "sessions_end": str(sessions[-1].date()),
    }
    return tgt_df, metas, crowd_stats


def _make_us_session(
    frames: dict[str, pd.DataFrame],
    universe: list[str],
    *,
    sessions_start: pd.Timestamp,
    sessions_end: pd.Timestamp,
) -> nt_engine.PortfolioEngineSession:
    s_iso = (sessions_start - dt.timedelta(days=5)).strftime("%Y-%m-%d")
    e_iso = (sessions_end + dt.timedelta(days=5)).strftime("%Y-%m-%d")
    trimmed = {s: frames[s].loc[s_iso:e_iso] for s in universe if s in frames and not frames[s].loc[s_iso:e_iso].empty}
    if not trimmed:
        raise ValueError("Empty US frames for session")
    return nt_engine.PortfolioEngineSession(
        trimmed,
        market="us",
        initial_capital=INITIAL_CAPITAL,
        execution="close",
        commission=COMMISSION,
        slippage=SLIPPAGE,
        carry=US_CARRY,
        periods_per_year=252,
    )


def _run_us_nt(
    frames: dict[str, pd.DataFrame],
    targets: pd.DataFrame,
    *,
    sessions_start: pd.Timestamp,
    sessions_end: pd.Timestamp,
    session: nt_engine.PortfolioEngineSession | None = None,
    compute_full_metrics: bool = True,
) -> dict[str, Any]:
    if session is not None:
        bt = session.run(targets, compute_full_metrics=compute_full_metrics)
    else:
        used = [c for c in targets.columns if c in frames and (targets[c].abs() > 0).any()]
        if not used:
            raise ValueError("Empty US targets")
        with _make_us_session(frames, used, sessions_start=sessions_start, sessions_end=sessions_end) as sess:
            bt = sess.run(targets, compute_full_metrics=compute_full_metrics)
    s_utc = pd.Timestamp(sessions_start, tz="UTC")
    e_utc = pd.Timestamp(sessions_end, tz="UTC")
    eq = bt["equity"].loc[s_utc:e_utc]
    bt["equity"] = eq
    if compute_full_metrics:
        bt["metrics"] = nt_engine.compute_metrics(eq, periods_per_year=252, initial_capital=INITIAL_CAPITAL)
    return bt


def period_returns_from_equity(eq: pd.Series, rebal_dates: list[pd.Timestamp]) -> np.ndarray:
    """Compute net returns over each consecutive rebalance interval [d_i, d_{i+1}]."""
    vals = []
    for i in range(len(rebal_dates) - 1):
        t0 = pd.Timestamp(rebal_dates[i], tz="UTC")
        t1 = pd.Timestamp(rebal_dates[i + 1], tz="UTC")
        if t0 in eq.index and t1 in eq.index and float(eq.loc[t0]) > 0:
            vals.append(float(eq.loc[t1]) / float(eq.loc[t0]) - 1.0)
    return np.asarray(vals, dtype=float)


def paired_period_stats(arm_p: np.ndarray, base_p: np.ndarray) -> dict[str, Any]:
    deltas = arm_p - base_p
    n = len(deltas)
    mean = float(deltas.mean()) if n else 0.0
    sd = float(deltas.std(ddof=1)) if n >= 2 else 0.0
    se = sd / math.sqrt(n) if n >= 2 else 0.0
    z = (mean / se) if se > 0 else 0.0
    half = n // 2
    h1 = float(deltas[:half].mean()) if half > 0 else 0.0
    h2 = float(deltas[half:].mean()) if n - half > 0 else 0.0
    return {
        "n_periods": int(n),
        "mean_delta_bps": round(mean * 1e4, 2),
        "half1_delta_bps": round(h1 * 1e4, 2),
        "half2_delta_bps": round(h2 * 1e4, 2),
        "mean_delta_raw": mean,
        "half1_delta_raw": h1,
        "half2_delta_raw": h2,
        "z_stat": round(z, 3),
    }


def _summarize_bt(bt: dict[str, Any], crowd: dict[str, Any]) -> dict[str, Any]:
    m = bt["metrics"]
    return {
        "total_return": round(float(m["total_return"]), 4),
        "cagr": round(float(m["cagr"]), 4),
        "sharpe": round(float(m["sharpe"]), 3),
        "sortino": round(float(m["sortino"]), 3),
        "calmar": round(float(m["calmar"]), 3),
        "max_drawdown": round(float(m["max_drawdown"]), 4),
        "volatility": round(float(m["volatility"]), 4),
        "win_rate": round(float(m["win_rate"]), 4),
        "commission": round(float(bt["commission"]), 2),
        "borrow": round(float(bt["carry"].get("borrow_paid", 0.0)), 2),
        "financing": round(float(bt["carry"].get("financing_paid", 0.0)), 2),
        "fills": int(bt["fill_count"]),
        "crowding": crowd,
    }


# --- Fast Parallel Placebo Workers (Strictly NautilusTrader nt_engine) ---
_US_CTX: dict[str, Any] = {}


def _init_us_worker(ctx: dict[str, Any]) -> None:
    global _US_CTX
    _US_CTX = dict(ctx)
    _US_CTX["session"] = _make_us_session(
        ctx["frames"],
        ctx["universe"],
        sessions_start=ctx["s_start"],
        sessions_end=ctx["s_end"],
    )


def _precompute_h1_placebo_stats(
    rets_df: pd.DataFrame,
    spy_rets: pd.Series,
    vol_df: pd.DataFrame,
    metas_h1: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    """Precompute exact 12-1 residual sufficient statistics per rebalance date for O(N) placebo draws."""
    pre: list[dict[str, Any]] = []
    for m in metas_h1:
        elig = m["eligible"]
        betas_map = m["betas"]
        if len(betas_map) < s1.PARAMS["min_eligible"]:
            continue
        date_loc = int(m["date_loc"])
        if date_loc < 252:
            continue
        est_slice = slice(date_loc - 251, date_loc + 1)
        r_mat = rets_df.iloc[est_slice][elig].to_numpy(dtype=float)
        spy_v = spy_rets.iloc[est_slice].to_numpy(dtype=float)
        n_rows = r_mat.shape[0]

        valid = np.isfinite(r_mat) & np.isfinite(spy_v)[:, None]
        counts = valid.sum(axis=0)
        denom = np.maximum(counts, 1)
        mean_x = np.where(valid, spy_v[:, None], 0.0).sum(axis=0) / denom
        mean_y = np.where(valid, r_mat, 0.0).sum(axis=0) / denom

        mom_end = max(0, n_rows - 21)
        v_m = valid[:mom_end, :]
        dy_m = np.where(v_m, r_mat[:mom_end, :] - mean_y[None, :], 0.0)
        dx_m = np.where(v_m, spy_v[:mom_end, None] - mean_x[None, :], 0.0)
        m_cnt = v_m.sum(axis=0)

        # Restrict to symbols with valid estimated beta (identical to beta_override keys in compute_residual_momentum_at)
        b_syms = sorted(betas_map.keys())
        elig_pos = {s: idx for idx, s in enumerate(elig)}
        idx_arr = np.array([elig_pos[s] for s in b_syms], dtype=int)
        vol_row = vol_df.loc[m["date"], b_syms].to_numpy(dtype=float)

        pre.append(
            {
                "date": m["date"],
                "b_syms": b_syms,
                "betas": np.array([betas_map[s] for s in b_syms], dtype=float),
                "counts": counts[idx_arr],
                "M": m_cnt[idx_arr].astype(float),
                "A": dy_m.sum(axis=0)[idx_arr],
                "B": dx_m.sum(axis=0)[idx_arr],
                "C": (dy_m * dy_m).sum(axis=0)[idx_arr],
                "D": (dy_m * dx_m).sum(axis=0)[idx_arr],
                "E": (dx_m * dx_m).sum(axis=0)[idx_arr],
                "vol": vol_row,
            }
        )
    return pre


def _placebo_h1_draw(draw_idx: int) -> float:
    """H1 placebo: randomly permute SPY betas across eligible stocks at each rebalance."""
    ctx = _US_CTX
    rng = np.random.default_rng(SEED_H1 + draw_idx)
    rebal_rows: dict[pd.Timestamp, dict[str, float]] = {}

    for st in ctx["h1_stats"]:
        b_syms = st["b_syms"]
        b_vals = st["betas"].copy()
        rng.shuffle(b_vals)

        m_cnt = st["M"]
        res_sum = st["A"] - b_vals * st["B"]
        sum_sq = st["C"] - 2.0 * b_vals * st["D"] + (b_vals * b_vals) * st["E"]
        denom_dof = np.maximum(m_cnt - 1.0, 1.0)
        var_ddof1 = np.maximum(0.0, (sum_sq - (res_sum * res_sum) / np.maximum(m_cnt, 1.0)) / denom_dof)
        res_sd = np.sqrt(var_ddof1)

        ok = (
            np.isfinite(b_vals)
            & (st["counts"] >= 126)
            & (m_cnt >= 100)
            & np.isfinite(res_sd)
            & (res_sd > 1e-12)
        )
        valid_idx = np.flatnonzero(ok)
        if len(valid_idx) < s1.PARAMS["min_eligible"]:
            continue
        scores = res_sum[valid_idx] / (res_sd[valid_idx] * np.sqrt(m_cnt[valid_idx]))
        order = np.argsort(scores, kind="stable")
        n = max(1, math.floor(0.2 * len(order)))
        short_pos = valid_idx[order[:n]]
        long_pos = valid_idx[order[-n:]]
        longs = [b_syms[i] for i in long_pos]
        shorts = [b_syms[i] for i in short_pos]
        lw = _capped_invvol(pd.Series(st["vol"][long_pos], index=longs, dtype=float), 0.5, 0.10)
        sw = -_capped_invvol(pd.Series(st["vol"][short_pos], index=shorts, dtype=float), 0.5, 0.10)
        rebal_rows[pd.Timestamp(st["date"], tz="UTC")] = {
            str(k): round(float(v), 6) for k, v in pd.concat([lw, sw]).items() if round(float(v), 6) != 0
        }

    cols = sorted({s for r in rebal_rows.values() for s in r})
    tgt = pd.DataFrame.from_dict(rebal_rows, orient="index").reindex(columns=cols, fill_value=0.0).sort_index()
    bt = _run_us_nt(
        ctx["frames"],
        tgt,
        sessions_start=ctx["s_start"],
        sessions_end=ctx["s_end"],
        session=ctx.get("session"),
        compute_full_metrics=False,
    )
    p_rets = period_returns_from_equity(bt["equity"], ctx["rebal_dates"])
    return float((p_rets - ctx["base_p"]).mean())


def _placebo_h2_draw(draw_idx: int) -> float:
    """H2 placebo: randomly permute correlation penalty vector within each leg at each rebalance."""
    ctx = _US_CTX
    rng = np.random.default_rng(SEED_H2 + draw_idx)
    rebal_rows: dict[pd.Timestamp, dict[str, float]] = {}

    for m in ctx["metas_h2"]:
        longs, shorts = m["longs"], m["shorts"]
        if not longs or not shorts:
            continue
        vol = m["vol"]
        pl_vals = np.array([m["pen_l"][s] for s in longs], dtype=float)
        ps_vals = np.array([m["pen_s"][s] for s in shorts], dtype=float)
        rng.shuffle(pl_vals)
        rng.shuffle(ps_vals)
        lv = pd.Series([vol[s] * pl_vals[i] for i, s in enumerate(longs)], index=longs, dtype=float)
        sv = pd.Series([vol[s] * ps_vals[i] for i, s in enumerate(shorts)], index=shorts, dtype=float)
        lw = _capped_invvol(lv, 0.5, 0.10)
        sw = -_capped_invvol(sv, 0.5, 0.10)
        rebal_rows[pd.Timestamp(m["date"], tz="UTC")] = {
            str(k): round(float(v), 6) for k, v in pd.concat([lw, sw]).items() if round(float(v), 6) != 0
        }

    cols = sorted({s for r in rebal_rows.values() for s in r})
    tgt = pd.DataFrame.from_dict(rebal_rows, orient="index").reindex(columns=cols, fill_value=0.0).sort_index()
    bt = _run_us_nt(
        ctx["frames"],
        tgt,
        sessions_start=ctx["s_start"],
        sessions_end=ctx["s_end"],
        session=ctx.get("session"),
        compute_full_metrics=False,
    )
    p_rets = period_returns_from_equity(bt["equity"], ctx["rebal_dates"])
    return float((p_rets - ctx["base_p"]).mean())


def _eval_dataset(name: str, *, include_volscale: bool = True) -> dict[str, Any]:
    ds = vs.load_dataset(name)
    frames, calendar, universe = ds["frames"], ds["calendar"], [s for s in ds["requested"] if s in ds["frames"]]
    arms_spec = [
        ("B_base", "raw", "invvol", False),
        ("H1_resmom", "resmom", "invvol", False),
        ("H2_decrowd", "raw", "decrowd", False),
        ("H1_H2_combo", "resmom", "decrowd", False),
    ]
    if include_volscale:
        arms_spec.extend([
            ("B_volscale", "raw", "invvol", True),
            ("H1_volscale", "resmom", "invvol", True),
            ("H2_volscale", "raw", "decrowd", True),
            ("H1_H2_volscale", "resmom", "decrowd", True),
        ])

    out: dict[str, Any] = {}
    artifacts: dict[str, Any] = {"ds": ds, "universe": universe}
    base_p = None
    rebal_dates: list[pd.Timestamp] = []

    built = []
    for arm_name, sig_m, wt_m, vsc in arms_spec:
        tgt, metas, crowd = build_arm_targets(frames, universe, calendar, signal_mode=sig_m, weight_mode=wt_m, volscale=vsc)
        built.append((arm_name, tgt, metas, crowd))

    s_start = pd.Timestamp(built[0][3]["sessions_start"])
    s_end = pd.Timestamp(built[0][3]["sessions_end"])
    with _make_us_session(frames, universe, sessions_start=s_start, sessions_end=s_end) as sess:
        for arm_name, tgt, metas, crowd in built:
            bt = _run_us_nt(frames, tgt, sessions_start=s_start, sessions_end=s_end, session=sess)
            if arm_name == "B_base":
                rebal_dates = [pd.Timestamp(m["date"]) for m in metas]
                base_p = period_returns_from_equity(bt["equity"], rebal_dates)
                artifacts.update(s_start=s_start, s_end=s_end, rebal_dates=rebal_dates, base_p=base_p)
            p_rets = period_returns_from_equity(bt["equity"], rebal_dates)
            entry = {"summary": _summarize_bt(bt, crowd)}
            if base_p is not None and arm_name != "B_base":
                entry["paired_vs_base"] = paired_period_stats(p_rets, base_p)
            out[arm_name] = entry
            if arm_name in ("H1_resmom", "H2_decrowd"):
                artifacts[f"metas_{arm_name}"] = metas
                artifacts[f"paired_{arm_name}"] = entry["paired_vs_base"]

    return {"results": out, "artifacts": artifacts}


def run_study(
    *,
    placebo_n: int = 200,
    workers: int = 4,
    write_ledger: bool = False,
    reuse_main: Path | None = None,
    checkpoint: Path | None = None,
    arm_filter: str = "all",
    draw_start: int = 0,
    draw_end: int | None = None,
) -> dict[str, Any]:
    if write_ledger and placebo_n < MIN_PLACEBO_DRAWS:
        raise ValueError(f"Formal S1 ledger write requires placebo_n >= {MIN_PLACEBO_DRAWS}")
    verify_prereg()
    vs.guard_hashes(("D1", "D2", "D3"), allow_input_drift=False)

    if reuse_main is not None:
        cached = json.loads(reuse_main.read_text(encoding="utf-8"))
        if cached.get("prereg_sha256") != PREREG_SHA256:
            raise RuntimeError("Cached main arms prereg_sha256 mismatch")
        if cached.get("engine") != f"nautilus_trader {nt_engine.nt_version()}":
            raise RuntimeError("Cached main arms engine mismatch")
        ds3 = vs.load_dataset("D3")
        frames3, cal3 = ds3["frames"], ds3["calendar"]
        univ3 = [s for s in ds3["requested"] if s in frames3]
        tgt_b, metas_b, crowd_b = build_arm_targets(frames3, univ3, cal3, signal_mode="raw", weight_mode="invvol", volscale=False)
        _, metas_h1, _ = build_arm_targets(frames3, univ3, cal3, signal_mode="resmom", weight_mode="invvol", volscale=False)
        _, metas_h2, _ = build_arm_targets(frames3, univ3, cal3, signal_mode="raw", weight_mode="decrowd", volscale=False)
        s_start = pd.Timestamp(crowd_b["sessions_start"])
        s_end = pd.Timestamp(crowd_b["sessions_end"])
        rebal_dates = [pd.Timestamp(m["date"]) for m in metas_b]
        with _make_us_session(frames3, univ3, sessions_start=s_start, sessions_end=s_end) as sess:
            bt_b = _run_us_nt(frames3, tgt_b, sessions_start=s_start, sessions_end=s_end, session=sess)
        base_p = period_returns_from_equity(bt_b["equity"], rebal_dates)
        # Verify cached B_base total_return matches fresh NT run exactly
        fresh_ret = round(float(bt_b["metrics"]["total_return"]), 4)
        cached_ret = cached["D3"]["B_base"]["summary"]["total_return"]
        if abs(fresh_ret - cached_ret) > 1e-6:
            raise RuntimeError(f"Cached B_base return {cached_ret} != fresh {fresh_ret}")
        d3 = {
            "results": cached["D3"],
            "artifacts": {
                "ds": ds3,
                "universe": univ3,
                "s_start": s_start,
                "s_end": s_end,
                "rebal_dates": rebal_dates,
                "base_p": base_p,
                "metas_H1_resmom": metas_h1,
                "metas_H2_decrowd": metas_h2,
                "paired_H1_resmom": cached["D3"]["H1_resmom"]["paired_vs_base"],
                "paired_H2_decrowd": cached["D3"]["H2_decrowd"]["paired_vs_base"],
            },
        }
        d2 = {"results": cached["D2"]}
        d1 = {"results": cached["D1"]}
        print("[fq_us] Reused verified D3/D2/D1 main arms; B_base fresh parity OK.", flush=True)
    else:
        d3 = _eval_dataset("D3", include_volscale=True)
        d2 = _eval_dataset("D2", include_volscale=True)
        d1 = _eval_dataset("D1", include_volscale=True)

    # Run H1 (K=12) and H2 (K=13) placebos on primary D3
    art = d3["artifacts"]
    ds3 = art["ds"]
    frames3 = ds3["frames"]
    cal3 = ds3["calendar"]
    univ3 = art["universe"]
    feat3 = s1.build_features(frames3, univ3, cal3)
    rets3 = feat3["close"].pct_change(fill_method=None)
    spy3 = frames3["SPY"]["close"].reindex(cal3).pct_change(fill_method=None)
    h1_stats = _precompute_h1_placebo_stats(rets3, spy3, feat3["volatility"], art["metas_H1_resmom"])

    # Trim worker frames to exact [s_start - 5d, s_end + 5d] window to minimize IPC/RAM
    s_iso = (art["s_start"] - dt.timedelta(days=5)).strftime("%Y-%m-%d")
    e_iso = (art["s_end"] + dt.timedelta(days=5)).strftime("%Y-%m-%d")
    trimmed_frames = {
        s: frames3[s].loc[s_iso:e_iso]
        for s in univ3
        if s in frames3 and not frames3[s].loc[s_iso:e_iso].empty
    }
    h2_lean = [
        {"date": m["date"], "longs": m["longs"], "shorts": m["shorts"], "vol": m["vol"], "pen_l": m["pen_l"], "pen_s": m["pen_s"]}
        for m in art["metas_H2_decrowd"]
    ]
    ctx = {
        "frames": trimmed_frames,
        "universe": univ3,
        "h1_stats": h1_stats,
        "metas_h2": h2_lean,
        "s_start": art["s_start"],
        "s_end": art["s_end"],
        "rebal_dates": art["rebal_dates"],
        "base_p": art["base_p"],
    }
    # Release heavy raw DataFrames from parent process before spawning workers
    import gc
    del ds3, frames3, feat3, rets3, spy3
    art.pop("ds", None)
    gc.collect()

    ckpt_data: dict[str, dict[str, float]] = {"h1": {}, "h2": {}}
    if checkpoint is not None and checkpoint.exists():
        loaded = json.loads(checkpoint.read_text(encoding="utf-8"))
        ckpt_data["h1"] = {str(k): float(v) for k, v in loaded.get("h1", {}).items()}
        ckpt_data["h2"] = {str(k): float(v) for k, v in loaded.get("h2", {}).items()}

    def _save_ckpt() -> None:
        if checkpoint is not None:
            checkpoint.parent.mkdir(parents=True, exist_ok=True)
            tmp = checkpoint.with_suffix(".tmp")
            tmp.write_text(json.dumps(ckpt_data, sort_keys=True), encoding="utf-8")
            tmp.replace(checkpoint)

    lo = max(0, int(draw_start))
    hi = min(placebo_n, int(draw_end) if draw_end is not None else placebo_n)
    todo_h1 = [i for i in range(lo, hi) if arm_filter in ("all", "h1") and str(i) not in ckpt_data["h1"]]
    todo_h2 = [i for i in range(lo, hi) if arm_filter in ("all", "h2") and str(i) not in ckpt_data["h2"]]

    if todo_h1 or todo_h2:
        if workers <= 1:
            _init_us_worker(ctx)
            try:
                for i in todo_h1:
                    ckpt_data["h1"][str(i)] = _placebo_h1_draw(i)
                    _save_ckpt()
                for i in todo_h2:
                    ckpt_data["h2"][str(i)] = _placebo_h2_draw(i)
                    _save_ckpt()
            finally:
                if "session" in _US_CTX:
                    _US_CTX["session"].close()
        else:
            with ProcessPoolExecutor(max_workers=workers, initializer=_init_us_worker, initargs=(ctx,)) as pool:
                if todo_h1:
                    fut_map1 = {pool.submit(_placebo_h1_draw, i): i for i in todo_h1}
                    done1 = 0
                    for f in as_completed(fut_map1):
                        i = fut_map1[f]
                        ckpt_data["h1"][str(i)] = f.result()
                        done1 += 1
                        if done1 % 10 == 0 or done1 == len(todo_h1):
                            _save_ckpt()
                            print(f"[fq_us] H1 placebos: {len(ckpt_data['h1'])}/{placebo_n}", flush=True)
                if todo_h2:
                    fut_map2 = {pool.submit(_placebo_h2_draw, i): i for i in todo_h2}
                    done2 = 0
                    for f in as_completed(fut_map2):
                        i = fut_map2[f]
                        ckpt_data["h2"][str(i)] = f.result()
                        done2 += 1
                        if done2 % 10 == 0 or done2 == len(todo_h2):
                            _save_ckpt()
                            print(f"[fq_us] H2 placebos: {len(ckpt_data['h2'])}/{placebo_n}", flush=True)

    p_h1 = [ckpt_data["h1"][str(i)] for i in range(placebo_n) if str(i) in ckpt_data["h1"]]
    p_h2 = [ckpt_data["h2"][str(i)] for i in range(placebo_n) if str(i) in ckpt_data["h2"]]

    def _grade(k_num: int, paired: dict[str, Any], p_draws: list[float]) -> dict[str, Any]:
        z_k = float(incremental_filter.z_threshold(k_num))
        pct = float(sum(1 for d in p_draws if d < paired["mean_delta_raw"]) / len(p_draws)) if p_draws else None
        verdict = ltw._compute_s1_verdict(
            z_stat=float(paired["z_stat"]),
            z_k=z_k,
            val_delta=float(paired["half1_delta_raw"]),
            ho_delta=float(paired["half2_delta_raw"]),
            placebo_pct=pct,
            placebo_n=len(p_draws),
            min_placebo_n=MIN_PLACEBO_DRAWS,
        )
        return {
            "k": k_num,
            "z_k": round(z_k, 3),
            "paired": paired,
            "placebo": {
                "n": len(p_draws),
                "percentile": round(pct, 4) if pct is not None else None,
                "mean_delta_bps": round(float(np.mean(p_draws)) * 1e4, 2) if p_draws else None,
                "p90_delta_bps": round(float(np.quantile(p_draws, 0.90)) * 1e4, 2) if p_draws else None,
            },
            "verdict": verdict,
        }

    grade_h1 = _grade(K_RESMOM, art["paired_H1_resmom"], p_h1)
    grade_h2 = _grade(K_DECROWD, art["paired_H2_decrowd"], p_h2)

    result = {
        "prereg_sha256": PREREG_SHA256,
        "engine": f"nautilus_trader {nt_engine.nt_version()}",
        "s1_primary_D3": {
            "H1_resmom_K12": grade_h1,
            "H2_decrowd_K13": grade_h2,
        },
        "D3": d3["results"],
        "D2": d2["results"],
        "D1": d1["results"],
    }

    if write_ledger:
        for cand_id, k_num, gr, arm_key in [
            ("fq-us-residual-momentum-v1", K_RESMOM, grade_h1, "H1_resmom"),
            ("fq-us-decrowd-corr-invvol-v1", K_DECROWD, grade_h2, "H2_decrowd"),
        ]:
            p_cnt = int(gr["placebo"]["n"])
            z_val = float(gr["paired"]["z_stat"])
            if gr["verdict"] != "fail" or z_val >= 1.0:
                if p_cnt < MIN_PLACEBO_DRAWS:
                    raise ValueError(
                        f"Non-futility/passing S1 ledger write for {cand_id} strictly requires >= {MIN_PLACEBO_DRAWS} placebos (got {p_cnt})"
                    )
            elif p_cnt < 30:
                raise ValueError(
                    f"Even deterministic-fail futility write for {cand_id} requires >= 30 NautilusTrader placebos (got {p_cnt})"
                )

            def _build_us_row(
                k_now: int,
                _prev: list[dict[str, Any]],
                _cid: str = cand_id,
                _k_num: int = k_num,
                _gr: dict[str, Any] = gr,
                _arm: str = arm_key,
            ) -> dict[str, Any]:
                if k_now != _k_num:
                    raise ValueError(f"Expected fq_us next_k={_k_num}, got {k_now}")
                row = {
                    "stage": "S1",
                    "change_type": "E",
                    "idea_id": _cid,
                    "baseline_version": "mom_ls_invvol_v1",
                    "verdict": _gr["verdict"],
                    "preregistered_K": _k_num,
                    "planned_arm_K": _k_num,
                    "actual_ledger_K": k_now,
                    "delta": _gr["paired"]["mean_delta_raw"],
                    "z": _gr["paired"]["z_stat"],
                    "z_threshold": _gr["z_k"],
                    "half1_delta": _gr["paired"]["half1_delta_raw"],
                    "half2_delta": _gr["paired"]["half2_delta_raw"],
                    "placebo_pct": _gr["placebo"]["percentile"],
                    "placebo_n": _gr["placebo"]["n"],
                    "prereg_placebo_n": MIN_PLACEBO_DRAWS,
                    "d3_sharpe_base": d3["results"]["B_base"]["summary"]["sharpe"],
                    "d3_sharpe_cand": d3["results"][_arm]["summary"]["sharpe"],
                    "d3_maxdd_base": d3["results"]["B_base"]["summary"]["max_drawdown"],
                    "d3_maxdd_cand": d3["results"][_arm]["summary"]["max_drawdown"],
                    "prereg_path": str(PREREG.relative_to(REPO)),
                    "prereg_sha256": PREREG_SHA256,
                    "engine": f"nautilus_trader {nt_engine.nt_version()}",
                }
                if _gr["placebo"]["n"] < MIN_PLACEBO_DRAWS:
                    row["placebo_note"] = (
                        f"futility_early_stop_at_{_gr['placebo']['n']}_of_{MIN_PLACEBO_DRAWS}: "
                        f"primary z={_gr['paired']['z_stat']} < 1.0 already locks S1 verdict=fail"
                    )
                return row

            evolution_ledger.append_event(LEDGER, "fq_us", _build_us_row)
    return result


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--placebo-n", type=int, default=200)
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--write-ledger", action="store_true")
    ap.add_argument("--reuse-main", type=Path, default=None)
    ap.add_argument("--checkpoint", type=Path, default=None)
    ap.add_argument("--arm", choices=("all", "h1", "h2"), default="all")
    ap.add_argument("--start", type=int, default=0)
    ap.add_argument("--end", type=int, default=None)
    ap.add_argument("--out", type=Path, default=None)
    args = ap.parse_args()
    res = run_study(
        placebo_n=args.placebo_n,
        workers=args.workers,
        write_ledger=args.write_ledger,
        reuse_main=args.reuse_main,
        checkpoint=args.checkpoint,
        arm_filter=args.arm,
        draw_start=args.start,
        draw_end=args.end,
    )
    text = json.dumps(res, indent=2, ensure_ascii=False)
    if args.out is not None:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(text + "\n", encoding="utf-8")
    print(text)


if __name__ == "__main__":
    main()
