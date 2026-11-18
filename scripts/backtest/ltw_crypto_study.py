"""Liu, Tsyvinski & Wu (2022) Crypto Risk Factors OOS Replication & fq_crypto K=15 S1 Study.

Strictly bound to frozen pre-registration:
  docs/research/2026-11-18-ltw-crypto-factors-s1-prereg.md
  PREREG_SHA256 = 4eb43cf0de37e8dbd1ec8b0b24baae1cc9454972e14afb4adef498574906c920

Executes all portfolios strictly through ``scripts/backtest/nt_engine.py``
(NautilusTrader 1.222.0 + QuantStats 0.0.86) on NAS MariaDB ``crypto_klines``.
"""
from __future__ import annotations

import argparse
from concurrent.futures import ProcessPoolExecutor, as_completed
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
from typing import Any

import numpy as np
import pandas as pd

from scripts.backtest import evolution_ledger, incremental_filter, nas_klines, nt_engine
from scripts.factor_mining import ensemble, panel, quant_targets


REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / "docs/research/2026-11-18-ltw-crypto-factors-s1-prereg.md"
PREREG_SHA256 = "4eb43cf0de37e8dbd1ec8b0b24baae1cc9454972e14afb4adef498574906c920"
LEDGER = REPO / "data/evolution/ledger.jsonl"

PLANNED_K = 15
SEED = 20261118
INITIAL_CAPITAL = 100_000.0
COMMISSION = 0.0005
SLIPPAGE = 0.0003


def verify_prereg() -> bytes:
    raw = PREREG.read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    if digest != PREREG_SHA256:
        raise ValueError(f"Preregistration SHA-256 mismatch: {digest} != {PREREG_SHA256}")
    return raw


def cs_zscore(series: pd.Series) -> pd.Series:
    """Cross-sectional zero-mean, unit-std normalization."""
    s = pd.to_numeric(series, errors="coerce").dropna()
    if len(s) == 0:
        return pd.Series(dtype=float)
    sd = float(s.std(ddof=1)) if len(s) > 1 else 0.0
    if not math.isfinite(sd) or sd <= 1e-12:
        return pd.Series(0.0, index=s.index)
    return (s - float(s.mean())) / sd


def newey_west_ols(y: np.ndarray, X: np.ndarray, lags: int = 7) -> dict[str, Any]:
    """OLS with Newey-West HAC standard errors (Bartlett kernel)."""
    y = np.asarray(y, dtype=float)
    X = np.asarray(X, dtype=float)
    n, k = X.shape
    if n <= k + lags + 2:
        return {"n": int(n), "beta": [0.0] * k, "se": [math.nan] * k, "t": [math.nan] * k, "r2": 0.0}
    xtx_inv = np.linalg.pinv(X.T @ X)
    beta = xtx_inv @ (X.T @ y)
    resid = y - X @ beta
    u = X * resid[:, None]
    omega = u.T @ u
    for lag in range(1, lags + 1):
        w = 1.0 - lag / (lags + 1.0)
        g = u[lag:].T @ u[:-lag]
        omega += w * (g + g.T)
    cov = (n / max(1, n - k)) * (xtx_inv @ omega @ xtx_inv)
    se = np.sqrt(np.maximum(np.diag(cov), 1e-18))
    t_stats = beta / se
    tss = float(np.sum((y - y.mean()) ** 2))
    rss = float(np.sum(resid ** 2))
    r2 = 1.0 - rss / tss if tss > 1e-15 else 0.0
    return {
        "n": int(n),
        "beta": [float(b) for b in beta],
        "se": [float(s) for s in se],
        "t": [float(t) for t in t_stats],
        "r2": float(r2),
    }


def _fetch_nas_crypto_frames(
    symbols: list[str], *, start: str = "2020-01-01", end: str = "2026-11-15"
) -> tuple[dict[str, pd.DataFrame], pd.DataFrame]:
    """Fetch 1d OHLCV + quote_volume from NAS MariaDB hitting composite PK."""
    cfg = nas_klines.load_config(timeout=45.0)
    frames: dict[str, pd.DataFrame] = {}
    qvol_cols: dict[str, pd.Series] = {}
    with nas_klines.ReadOnlyMysqlClient(cfg) as cli:
        for sym in sorted(symbols):
            rows = nas_klines.fetch_crypto_bars(
                sym, interval="1d", start=start, end=end, fmt="panel", client=cli
            )
            if not rows:
                continue
            df = pd.DataFrame(rows, columns=["date", "open", "high", "low", "close", "volume", "vwap"])
            df["date"] = pd.to_datetime(df["date"], utc=True)
            df = df.set_index("date").sort_index()
            df = df[~df.index.duplicated(keep="last")]
            num = df[["open", "high", "low", "close", "volume", "vwap"]].astype(float)
            qv = (num["volume"] * num["vwap"]).where(num["vwap"] > 0, num["volume"] * num["close"])
            qvol_cols[sym] = qv
            frames[sym] = num[["open", "high", "low", "close", "volume"]]
    qvol_df = pd.DataFrame(qvol_cols).sort_index()
    return frames, qvol_df


DEFAULT_CRYPTO_FUNDING_8H = 0.0001
MIN_PLACEBO_DRAWS = 200


def _build_crypto_carry(panels_obj: panel.Panels | None = None) -> nt_engine.CarryConfig:
    rates: dict[str, dict[Any, float]] = {}
    if panels_obj is not None and "funding" in panels_obj.fields:
        f_df = panels_obj.fields["funding"]
        for sym in f_df.columns:
            col = f_df[sym].dropna()
            rates[str(sym)] = {pd.Timestamp(idx).date(): float(val) for idx, val in col.items()}
    return nt_engine.CarryConfig(
        market="crypto",
        funding_rates=rates,
        default_funding_rate=DEFAULT_CRYPTO_FUNDING_8H,
        funding_multiplier=3.0,
    )


def _compute_s1_verdict(
    *,
    z_stat: float,
    z_k: float,
    val_delta: float,
    ho_delta: float,
    placebo_pct: float | None,
    placebo_n: int,
    min_placebo_n: int = MIN_PLACEBO_DRAWS,
) -> str:
    if z_stat < 1.0 or val_delta <= 0.0 or ho_delta <= 0.0:
        return "fail"
    if placebo_n < min_placebo_n or placebo_pct is None:
        return "insufficient_placebo"
    if placebo_pct < 0.90:
        return "fewer_trades"
    if z_stat >= z_k:
        return "strong"
    return "screen_pass"


def _run_nt(
    sub_frames: dict[str, pd.DataFrame],
    targets: pd.DataFrame,
    *,
    eval_start: pd.Timestamp | str | None = None,
    eval_end: pd.Timestamp | str | None = None,
    carry: nt_engine.CarryConfig | None = None,
) -> dict[str, Any]:
    cols = [c for c in targets.columns if c in sub_frames]
    tgt = targets[cols].dropna(how="all")
    if tgt.empty:
        raise ValueError("Empty targets for NautilusTrader run")
    s_anchor = pd.Timestamp(eval_start) if eval_start is not None else tgt.index.min()
    e_anchor = pd.Timestamp(eval_end) if eval_end is not None else tgt.index.max()
    if s_anchor.tzinfo is None:
        s_anchor = s_anchor.tz_localize("UTC")
    else:
        s_anchor = s_anchor.tz_convert("UTC")
    if e_anchor.tzinfo is None:
        e_anchor = e_anchor.tz_localize("UTC")
    else:
        e_anchor = e_anchor.tz_convert("UTC")

    start_iso = (min(s_anchor, tgt.index.min()) - dt.timedelta(days=2)).strftime("%Y-%m-%d")
    end_iso = (max(e_anchor, tgt.index.max()) + dt.timedelta(days=5)).strftime("%Y-%m-%d")
    trimmed = {
        s: f.loc[start_iso:end_iso]
        for s, f in sub_frames.items()
        if s in cols and not f.loc[start_iso:end_iso].empty
    }
    tgt = tgt[[c for c in tgt.columns if c in trimmed]]
    carry_cfg = carry if carry is not None else _build_crypto_carry(None)
    bt = nt_engine.run_portfolio(
        trimmed,
        tgt,
        market="crypto",
        initial_capital=INITIAL_CAPITAL,
        execution="next_open",
        commission=COMMISSION,
        slippage=SLIPPAGE,
        carry=carry_cfg,
        periods_per_year=365,
    )
    full_eq = bt["equity"]
    clipped_eq = full_eq.loc[s_anchor:e_anchor]
    prior = full_eq.loc[full_eq.index < s_anchor]
    start_cap = float(prior.iloc[-1]) if len(prior) else INITIAL_CAPITAL
    bt["equity"] = clipped_eq
    bt["metrics"] = nt_engine.compute_metrics(clipped_eq, periods_per_year=365, initial_capital=start_cap)
    return bt


def _nt_subperiod_metrics(eq: pd.Series, start: str, end: str) -> dict[str, Any]:
    sub = eq.loc[start:end]
    if len(sub) < 10:
        return {"periods": int(len(sub)), "total_return": None, "sharpe": None, "max_drawdown": None}
    m = nt_engine.compute_metrics(sub, periods_per_year=365)
    return {
        "periods": int(m.get("periods") or len(sub)),
        "total_return": round(float(m["total_return"]), 4) if m.get("total_return") is not None else None,
        "cagr": round(float(m["cagr"]), 4) if m.get("cagr") is not None else None,
        "sharpe": round(float(m["sharpe"]), 3) if m.get("sharpe") is not None else None,
        "max_drawdown": round(float(m["max_drawdown"]), 4) if m.get("max_drawdown") is not None else None,
        "win_rate": round(float(m["win_rate"]), 4) if m.get("win_rate") is not None else None,
    }


def _build_quantile_weights(
    signal_row: pd.Series,
    *,
    weighting: str = "ew",
    adv_row: pd.Series | None = None,
    quantile: float = 0.2,
    cap: float = 0.25,
) -> dict[str, float]:
    s = signal_row.dropna()
    if len(s) < 15:
        return {}
    k_leg = max(1, int(math.floor(len(s) * quantile)))
    ordered = s.sort_values(kind="mergesort")
    shorts = list(ordered.index[:k_leg])
    longs = list(ordered.index[-k_leg:])

    def _leg_w(names: list[str], sign: float) -> dict[str, float]:
        if weighting == "ew" or adv_row is None:
            w = 0.5 / len(names)
            return {n: sign * w for n in names}
        adv = adv_row.reindex(names).fillna(0.0).clip(lower=0.0)
        if float(adv.sum()) <= 0:
            w = 0.5 / len(names)
            return {n: sign * w for n in names}
        # Water-fill proportional to ADV20 with single-name cap
        rem = 0.5
        active = list(names)
        weights = pd.Series(0.0, index=names)
        while active and rem > 1e-12:
            sub = adv.loc[active]
            tot = float(sub.sum())
            prop = (sub / tot * rem) if tot > 0 else pd.Series(rem / len(active), index=active)
            over = prop[prop >= cap].index.tolist()
            if not over:
                weights.loc[active] = prop
                break
            weights.loc[over] = cap
            rem -= len(over) * cap
            active = [a for a in active if a not in over]
        return {n: sign * float(weights[n]) for n in names if weights[n] > 0}

    out = _leg_w(longs, 1.0)
    out.update(_leg_w(shorts, -1.0))
    return out


def _build_double_sort_2x3_cmom(r21_row: pd.Series, adv_row: pd.Series) -> dict[str, float]:
    """LTW Eq. (3) 2x3 Size x Momentum factor: (SH + BH)/2 - (SL + BL)/2."""
    valid = r21_row.dropna().index.intersection(adv_row.dropna().index)
    if len(valid) < 18:
        return {}
    r21 = r21_row.loc[valid]
    adv = adv_row.loc[valid]
    med_adv = float(adv.median())
    q30, q70 = float(r21.quantile(0.30)), float(r21.quantile(0.70))
    small = adv <= med_adv
    big = ~small
    sh = r21.index[small & (r21 >= q70)].tolist()
    bh = r21.index[big & (r21 >= q70)].tolist()
    sl = r21.index[small & (r21 <= q30)].tolist()
    bl = r21.index[big & (r21 <= q30)].tolist()
    if not (sh and bh and sl and bl):
        return {}
    w: dict[str, float] = {}
    for s in sh:
        w[s] = w.get(s, 0.0) + 0.25 / len(sh)
    for s in bh:
        w[s] = w.get(s, 0.0) + 0.25 / len(bh)
    for s in sl:
        w[s] = w.get(s, 0.0) - 0.25 / len(sl)
    for s in bl:
        w[s] = w.get(s, 0.0) - 0.25 / len(bl)
    return w


def run_part_a_and_factors(
    frames: dict[str, pd.DataFrame], qvol_df: pd.DataFrame
) -> tuple[dict[str, Any], dict[str, pd.Series]]:
    """Part A: OOS replication of LTW 9 anomalies + CMKT/CSMB/CMOM series (2020-2026)."""
    close_df = pd.DataFrame({s: f["close"] for s, f in frames.items()}).sort_index()
    qvol_aligned = qvol_df.reindex(index=close_df.index, columns=close_df.columns)

    # Strictly causal LTW characteristic panels
    adv20 = qvol_aligned.rolling(20, min_periods=20).mean()
    std_qvol20 = qvol_aligned.rolling(20, min_periods=20).std(ddof=1)
    max21_c = close_df.rolling(21, min_periods=21).max()
    bar_count = close_df.notna().cumsum()
    eligible_mat = (bar_count >= 30) & (close_df > 0) & (adv20 > 0)

    sigs: dict[str, pd.DataFrame] = {
        "SIZE": -adv20,
        "PRC": -np.log(close_df.where(close_df > 0)),
        "MAXDPRC_21": -max21_c,
        "PRCVOL_20": -adv20,
        "STDPRCVOL_20": -std_qvol20,
        "r7": close_df / close_df.shift(7) - 1.0,
        "r14": close_df / close_df.shift(14) - 1.0,
        "r21": close_df / close_df.shift(21) - 1.0,
        "r28": close_df / close_df.shift(28) - 1.0,
    }

    max_ts = close_df.index.max() - pd.Timedelta(days=1)
    reb_dates = [d for d in close_df.loc["2020-02-01":].index[::7] if d <= max_ts]
    cols = sorted(frames)

    def _run_spec(name: str, builder) -> tuple[dict[str, Any], pd.Series]:
        tgt = pd.DataFrame(np.nan, index=pd.DatetimeIndex(reb_dates), columns=cols)
        for d in reb_dates:
            elig = eligible_mat.loc[d]
            if int(elig.sum()) < 15:
                continue
            w = builder(d, elig)
            if w:
                tgt.loc[d] = pd.Series(w, dtype=float).reindex(cols, fill_value=0.0)
        tgt = tgt.dropna(how="all")
        bt = _run_nt(frames, tgt)
        eq = bt["equity"]
        rets = eq.pct_change().dropna()
        t_stat = float(rets.mean() / (rets.std(ddof=1) / math.sqrt(len(rets)))) if len(rets) > 10 and rets.std(ddof=1) > 0 else 0.0
        summary = {
            "name": name,
            "full": _nt_subperiod_metrics(eq, "2020-02-01", "2026-11-15"),
            "t_stat_daily": round(t_stat, 2),
            "sub_2020_2022": _nt_subperiod_metrics(eq, "2020-02-01", "2022-12-31"),
            "sub_2023_2026": _nt_subperiod_metrics(eq, "2023-01-01", "2026-11-15"),
        }
        return summary, rets

    anomalies_ew: dict[str, Any] = {}
    factor_rets: dict[str, pd.Series] = {}

    for key, mat in sigs.items():
        summ, rets = _run_spec(f"{key}_EW", lambda d, elig, m=mat: _build_quantile_weights(m.loc[d].where(elig), weighting="ew"))
        anomalies_ew[key] = summ
        if key == "SIZE":
            factor_rets["CSMB"] = rets
        elif key == "r21":
            factor_rets["CMOM_EW"] = rets

    # Value/ADV20 weighted sensitivities + r21 size splits + CMKT + 2x3 CMOM
    sensitivities: dict[str, Any] = {}
    for key in ("SIZE", "r21"):
        summ, _ = _run_spec(
            f"{key}_ADV20W",
            lambda d, elig, m=sigs[key]: _build_quantile_weights(m.loc[d].where(elig), weighting="vw", adv_row=adv20.loc[d]),
        )
        sensitivities[f"{key}_VW"] = summ

    def _r21_half(above: bool):
        def _b(d, elig):
            adv_d = adv20.loc[d].where(elig).dropna()
            if len(adv_d) < 30:
                return {}
            med = float(adv_d.median())
            sub_idx = adv_d.index[adv_d >= med] if above else adv_d.index[adv_d < med]
            return _build_quantile_weights(sigs["r21"].loc[d].reindex(sub_idx), weighting="ew")
        return _b

    sensitivities["r21_above_median_ADV20"], _ = _run_spec("r21_above_median_ADV20", _r21_half(True))
    sensitivities["r21_below_median_ADV20"], _ = _run_spec("r21_below_median_ADV20", _r21_half(False))

    # CMKT: ADV20-weighted long-only market (gross +1.0, cap 0.25)
    def _cmkt(d, elig):
        adv_d = adv20.loc[d].where(elig).dropna().clip(lower=0.0)
        if len(adv_d) < 10 or float(adv_d.sum()) <= 0:
            return {}
        rem = 1.0
        active = list(adv_d.index)
        w = pd.Series(0.0, index=active)
        while active and rem > 1e-12:
            sub = adv_d.loc[active]
            prop = sub / float(sub.sum()) * rem
            over = prop[prop >= 0.25].index.tolist()
            if not over:
                w.loc[active] = prop
                break
            w.loc[over] = 0.25
            rem -= len(over) * 0.25
            active = [a for a in active if a not in over]
        return {s: float(v) for s, v in w.items() if v > 0}

    cmkt_summ, cmkt_rets = _run_spec("CMKT", _cmkt)
    cmom23_summ, cmom23_rets = _run_spec("CMOM_2x3", lambda d, elig: _build_double_sort_2x3_cmom(sigs["r21"].loc[d].where(elig), adv20.loc[d].where(elig)))
    sensitivities["CMKT"] = cmkt_summ
    sensitivities["CMOM_2x3"] = cmom23_summ
    factor_rets["CMKT"] = cmkt_rets
    factor_rets["CMOM_2x3"] = cmom23_rets

    return {"anomalies_ew": anomalies_ew, "sensitivities": sensitivities}, factor_rets


def _build_split_targets(
    panels_obj: panel.Panels,
    scores_df: pd.DataFrame,
    dates: list[pd.Timestamp],
    cols: list[str],
) -> pd.DataFrame:
    targets = pd.DataFrame(np.nan, index=pd.DatetimeIndex([pd.Timestamp(d, tz="UTC") for d in dates]), columns=cols)
    for d in dates[::3]:
        row = scores_df.loc[d].where(panels_obj.mask.loc[d]).dropna()
        if row.empty:
            continue
        pct = row.rank(pct=True)
        entry = {
            "factor_ids": ["ltw-study"],
            "scores": {str(s): {"z": round(float(z), 4), "pct": round(float(pct[s]), 4)} for s, z in row.items()},
        }
        snap = {"as_of": d.strftime("%Y-%m-%d"), "run_id": "ltw-s1", "3": entry, "horizons": {"3": entry}}
        tgt = quant_targets.build_targets("crypto", snap, panels_obj, now=d + dt.timedelta(days=1))
        if tgt.get("status") == "ok" and tgt.get("weights"):
            targets.loc[pd.Timestamp(d, tz="UTC")] = pd.Series(tgt["weights"], dtype=float).reindex(cols, fill_value=0.0)
    return targets.dropna(how="all")


def _worker_placebo_draw(args: tuple) -> tuple[float, float, float]:
    """Worker evaluating one random permutation of r21 blended 50/50 with z_7f on val & holdout."""
    (seed_val, panels_obj, z7_df, r21_df, val_dates, ho_dates, sub_val, sub_ho, base_val_rets, base_ho_rets, cols) = args
    rng = np.random.default_rng(seed_val)
    perm_scores = pd.DataFrame(np.nan, index=z7_df.index, columns=z7_df.columns)
    for d in val_dates[::3] + ho_dates[::3]:
        elig = panels_obj.mask.loc[d] & z7_df.loc[d].notna() & r21_df.loc[d].notna()
        idx = elig.index[elig]
        if len(idx) == 0:
            continue
        z7_cs = cs_zscore(z7_df.loc[d, idx])
        zr_cs = cs_zscore(r21_df.loc[d, idx])
        shuffled = zr_cs.to_numpy().copy()
        rng.shuffle(shuffled)
        perm_scores.loc[d, idx] = 0.5 * z7_cs + 0.5 * pd.Series(shuffled, index=idx)

    carry_cfg = _build_crypto_carry(panels_obj)
    t_val = _build_split_targets(panels_obj, perm_scores, val_dates, cols)
    t_ho = _build_split_targets(panels_obj, perm_scores, ho_dates, cols)
    bt_val = _run_nt(sub_val, t_val, eval_start=val_dates[0], eval_end=val_dates[-1], carry=carry_cfg)
    bt_ho = _run_nt(sub_ho, t_ho, eval_start=ho_dates[0], eval_end=ho_dates[-1], carry=carry_cfg)
    r_val = bt_val["equity"].pct_change().dropna()
    r_ho = bt_ho["equity"].pct_change().dropna()
    d_val = float((r_val - base_val_rets.reindex(r_val.index).fillna(0.0)).mean())
    d_ho = float((r_ho - base_ho_rets.reindex(r_ho.index).fillna(0.0)).mean())
    d_all = float(pd.concat([r_val - base_val_rets, r_ho - base_ho_rets]).dropna().mean())
    return d_all, d_val, d_ho


def run_study(*, placebo_n: int = 200, workers: int = 6, write_ledger: bool = False) -> dict[str, Any]:
    if write_ledger and placebo_n < MIN_PLACEBO_DRAWS:
        raise ValueError(f"Formal S1 ledger write requires placebo_n >= {MIN_PLACEBO_DRAWS} (got {placebo_n})")
    verify_prereg()

    panels_obj = panel.load_panels("crypto", tier="all")
    carry_cfg = _build_crypto_carry(panels_obj)
    syms = sorted(panels_obj.fields["close"].columns)
    frames, qvol_df = _fetch_nas_crypto_frames(syms, start="2020-01-01", end="2026-11-15")
    max_nas_naive = max(f.index.max() for f in frames.values()).tz_localize(None) - dt.timedelta(days=1)

    # Part A + LTW factor return series
    part_a, factor_rets = run_part_a_and_factors(frames, qvol_df)

    # Baseline 7-factor h=3 scores
    zoo_raw = json.loads((REPO / "data/factor_mining/crypto/zoo.json").read_text(encoding="utf-8"))
    fmap = {f["id"]: f for f in zoo_raw.get("factors", [])}
    ens_raw = json.loads((REPO / "data/factor_mining/crypto/ensemble.json").read_text(encoding="utf-8"))
    fids = ens_raw["horizons"]["3"]["factor_ids"]
    z7_df = ensemble.ensemble_scores([fmap[fid] for fid in fids], panels_obj)

    close_p = panels_obj.fields["close"]
    r21_df = close_p / close_p.shift(21) - 1.0
    cols = sorted(frames)

    # Part B1: Spanning regression over train+val+holdout continuous B run (2022-05-22 .. 2026-11-13)
    splits = panel.make_splits(panels_obj.mask.index, h=3, windows=panels_obj.windows)
    all_dates = [d for d in panels_obj.mask.index if d >= pd.Timestamp("2022-05-22") and d <= max_nas_naive]
    t_b_full = _build_split_targets(panels_obj, z7_df, all_dates, cols)
    bt_b_full = _run_nt(frames, t_b_full, eval_start=all_dates[0], eval_end=all_dates[-1], carry=carry_cfg)
    b_full_rets = bt_b_full["equity"].pct_change().dropna()

    reg_df = pd.DataFrame({
        "B": b_full_rets,
        "CMKT": factor_rets["CMKT"],
        "CSMB": factor_rets["CSMB"],
        "CMOM": factor_rets["CMOM_2x3"],
    }).dropna()
    X = np.column_stack([np.ones(len(reg_df)), reg_df[["CMKT", "CSMB", "CMOM"]].to_numpy()])
    ols_23 = newey_west_ols(reg_df["B"].to_numpy(), X, lags=7)
    part_b1 = {
        "n_days": ols_23["n"],
        "alpha_ann_pct": round(ols_23["beta"][0] * 365.0 * 100.0, 2),
        "t_alpha": round(ols_23["t"][0], 3),
        "betas": {
            "CMKT": round(ols_23["beta"][1], 4),
            "CSMB": round(ols_23["beta"][2], 4),
            "CMOM_2x3": round(ols_23["beta"][3], 4),
        },
        "t_betas": {
            "CMKT": round(ols_23["t"][1], 2),
            "CSMB": round(ols_23["t"][2], 2),
            "CMOM_2x3": round(ols_23["t"][3], 2),
        },
        "r2": round(ols_23["r2"], 4),
    }

    # Part B2: Preregistered S1 (val + holdout, separate clean runs resetting capital)
    val_dates = [d for d in panels_obj.mask.index[splits["val"]] if d <= max_nas_naive]
    ho_dates = [d for d in panels_obj.mask.index[splits["holdout"]] if d <= max_nas_naive]

    # Build C1 score panel causally: 0.5*cs_zscore(z7) + 0.5*cs_zscore(r21)
    c1_df = pd.DataFrame(np.nan, index=z7_df.index, columns=z7_df.columns)
    for d in val_dates[::3] + ho_dates[::3]:
        elig = panels_obj.mask.loc[d] & z7_df.loc[d].notna() & r21_df.loc[d].notna()
        idx = elig.index[elig]
        if len(idx) == 0:
            continue
        c1_df.loc[d, idx] = 0.5 * cs_zscore(z7_df.loc[d, idx]) + 0.5 * cs_zscore(r21_df.loc[d, idx])

    def _trim_frames(dates: list[pd.Timestamp]) -> dict[str, pd.DataFrame]:
        s0 = (dates[0] - dt.timedelta(days=2)).strftime("%Y-%m-%d")
        s1 = (dates[-1] + dt.timedelta(days=5)).strftime("%Y-%m-%d")
        return {s: f.loc[s0:s1] for s, f in frames.items() if not f.loc[s0:s1].empty}

    sub_val = _trim_frames(val_dates)
    sub_ho = _trim_frames(ho_dates)

    t_b_val = _build_split_targets(panels_obj, z7_df, val_dates, cols)
    t_b_ho = _build_split_targets(panels_obj, z7_df, ho_dates, cols)
    t_c_val = _build_split_targets(panels_obj, c1_df, val_dates, cols)
    t_c_ho = _build_split_targets(panels_obj, c1_df, ho_dates, cols)

    if t_b_val.equals(t_c_val) and t_b_ho.equals(t_c_ho):
        raise ValueError("Candidate weights identical to baseline; invalid test")

    bt_b_val = _run_nt(sub_val, t_b_val, eval_start=val_dates[0], eval_end=val_dates[-1], carry=carry_cfg)
    bt_b_ho = _run_nt(sub_ho, t_b_ho, eval_start=ho_dates[0], eval_end=ho_dates[-1], carry=carry_cfg)
    bt_c_val = _run_nt(sub_val, t_c_val, eval_start=val_dates[0], eval_end=val_dates[-1], carry=carry_cfg)
    bt_c_ho = _run_nt(sub_ho, t_c_ho, eval_start=ho_dates[0], eval_end=ho_dates[-1], carry=carry_cfg)

    r_b_val = bt_b_val["equity"].pct_change().dropna()
    r_b_ho = bt_b_ho["equity"].pct_change().dropna()
    r_c_val = bt_c_val["equity"].pct_change().dropna()
    r_c_ho = bt_c_ho["equity"].pct_change().dropna()

    d_val = r_c_val - r_b_val
    d_ho = r_c_ho - r_b_ho
    delta = pd.concat([d_val, d_ho]).dropna()

    weekly = incremental_filter.weekly_statistics(
        [(pd.Timestamp(day).to_pydatetime().replace(tzinfo=dt.timezone.utc), float(v)) for day, v in delta.items()]
    )
    avg, se = weekly["avg_r"], weekly["se"]
    z_stat = (avg / se) if (se is not None and se > 0 and avg is not None) else 0.0
    z_k = float(incremental_filter.z_threshold(PLANNED_K))

    # 200x cross-sectional permutation placebo
    placebo_deltas: list[float] = []
    if placebo_n > 0:
        tasks = [
            (SEED + i, panels_obj, z7_df, r21_df, val_dates, ho_dates, sub_val, sub_ho, r_b_val, r_b_ho, cols)
            for i in range(placebo_n)
        ]
        if workers > 1 and placebo_n > 1:
            with ProcessPoolExecutor(max_workers=workers) as ex:
                futs = [ex.submit(_worker_placebo_draw, t) for t in tasks]
                for fut in as_completed(futs):
                    d_all, _, _ = fut.result()
                    placebo_deltas.append(d_all)
        else:
            for t in tasks:
                d_all, _, _ = _worker_placebo_draw(t)
                placebo_deltas.append(d_all)

    placebo_pct = float(np.mean([p < float(delta.mean()) for p in placebo_deltas])) if placebo_deltas else None
    val_delta = float(d_val.mean())
    ho_delta = float(d_ho.mean())
    verdict = _compute_s1_verdict(
        z_stat=float(z_stat),
        z_k=z_k,
        val_delta=val_delta,
        ho_delta=ho_delta,
        placebo_pct=placebo_pct,
        placebo_n=len(placebo_deltas),
    )

    s1_result = {
        "idea_id": "fq-crypto-ltw-cmom21-blend-v1",
        "track": "fq_crypto",
        "change_type": "E",
        "planned_K": PLANNED_K,
        "z_K": round(z_k, 4),
        "verdict": verdict,
        "mean_delta_daily": float(delta.mean()),
        "se_weekly": float(se) if se is not None else None,
        "z": round(float(z_stat), 4),
        "n_days": int(len(delta)),
        "n_weeks": int(weekly["n_weeks"]),
        "val_delta_daily": val_delta,
        "holdout_delta_daily": ho_delta,
        "placebo_n": int(len(placebo_deltas)),
        "placebo_pct": round(placebo_pct, 4) if placebo_pct is not None else None,
        "placebo_mean_delta": float(np.mean(placebo_deltas)) if placebo_deltas else None,
        "arms": {
            "B_val": bt_b_val["metrics"],
            "C1_val": bt_c_val["metrics"],
            "B_holdout": bt_b_ho["metrics"],
            "C1_holdout": bt_c_ho["metrics"],
        },
        "ranges": {
            "val": [val_dates[0].strftime("%Y-%m-%d"), val_dates[-1].strftime("%Y-%m-%d")],
            "holdout": [ho_dates[0].strftime("%Y-%m-%d"), ho_dates[-1].strftime("%Y-%m-%d")],
        },
    }

    if write_ledger:
        evolution_ledger.append_event(
            LEDGER,
            "fq_crypto",
            lambda k, prev: {
                "stage": "S1",
                "change_type": "E",
                "idea_id": "fq-crypto-ltw-cmom21-blend-v1",
                "baseline_version": "ls_invvol_protected_v1_h3_7f",
                "verdict": verdict,
                "preregistered_K": PLANNED_K,
                "planned_arm_K": PLANNED_K,
                "actual_ledger_K": k,
                "delta": float(delta.mean()),
                "se": float(se) if se is not None else None,
                "z": float(z_stat),
                "z_threshold": z_k,
                "val_delta": val_delta,
                "holdout_delta": ho_delta,
                "placebo_pct": placebo_pct,
                "prereg_path": str(PREREG.relative_to(REPO)),
                "prereg_sha256": PREREG_SHA256,
                "engine": f"nautilus_trader=={nt_engine.nt_version()}",
            },
        )

    return {
        "prereg_sha256": PREREG_SHA256,
        "nt_version": nt_engine.nt_version(),
        "universe_count": len(frames),
        "part_a_replication": part_a,
        "part_b1_spanning": part_b1,
        "part_b2_s1": s1_result,
    }


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--placebo-n", type=int, default=200)
    ap.add_argument("--workers", type=int, default=6)
    ap.add_argument("--write-ledger", action="store_true")
    ap.add_argument("--out", type=Path, default=None)
    args = ap.parse_args(argv)
    res = run_study(placebo_n=args.placebo_n, workers=args.workers, write_ledger=args.write_ledger)
    txt = json.dumps(res, indent=2, ensure_ascii=False, default=str)
    if args.out is not None:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(txt, encoding="utf-8")
    print(txt)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
