"""Deflated Sharpe Ratio & Effective Independent Trials Audit (Bailey & Lopez de Prado 2014).

Reads ``data/factor_mining/{crypto,us}`` and ``data/evolution/ledger.jsonl``,
audits ``scripts/backtest/quantjourney.py:deflated_sharpe``, and replays live
``fq_crypto`` h=3 (7-factor) and h=7 (f_8dd9bbca) portfolios on NAS MariaDB
crypto K-lines via ``scripts/backtest/nt_engine.py`` (NautilusTrader + QuantStats).
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import math
from pathlib import Path
from statistics import NormalDist
from typing import Any

import numpy as np
import pandas as pd

from scripts.backtest import nas_klines, nt_engine, quantjourney
from scripts.factor_mining import ensemble, evaluate, expr, panel, quant_targets


REPO = Path(__file__).resolve().parents[2]
EULER_GAMMA = 0.5772156649015329
NORM = NormalDist()


def expected_max_sr(trials: float, sr_var: float, sr_mean: float = 0.0) -> float:
    """Bailey & Lopez de Prado (2014) Eq. (1): expected max SR over N iid trials."""
    if trials <= 1 or sr_var <= 0 or not math.isfinite(sr_var):
        return float(sr_mean)
    z1 = NORM.inv_cdf(1.0 - 1.0 / trials)
    z2 = NORM.inv_cdf(1.0 - 1.0 / (trials * math.e))
    return float(sr_mean + math.sqrt(sr_var) * ((1.0 - EULER_GAMMA) * z1 + EULER_GAMMA * z2))


def dsr_probability(
    sr: float,
    t_len: int,
    skew: float,
    kurt: float,
    trials: float,
    sr_var: float,
    sr_mean: float = 0.0,
) -> dict[str, float]:
    """Bailey & Lopez de Prado (2014) Eq. (2) Deflated Sharpe Ratio & benchmark SR0."""
    if t_len < 3 or not math.isfinite(sr) or trials < 1:
        return {"sr": float(sr) if math.isfinite(sr) else 0.0, "sr0": 0.0, "dsr": 0.0}
    sr0 = expected_max_sr(trials, sr_var, sr_mean=sr_mean)
    denom_sq = 1.0 - skew * sr + ((kurt - 1.0) / 4.0) * (sr ** 2)
    if denom_sq <= 1e-12:
        return {"sr": float(sr), "sr0": float(sr0), "dsr": 0.0}
    z = (sr - sr0) * math.sqrt(t_len - 1.0) / math.sqrt(denom_sq)
    return {"sr": float(sr), "sr0": float(sr0), "dsr": float(NORM.cdf(z)), "z": float(z)}


def dsr_from_series(returns: np.ndarray | pd.Series, trials: float, sr_var: float, sr_mean: float = 0.0) -> dict[str, float]:
    a = np.asarray(returns, dtype=float)
    a = a[np.isfinite(a)]
    if len(a) < 4:
        return {"t_len": len(a), "sr": 0.0, "skew": 0.0, "kurt": 3.0, "sr0": 0.0, "dsr": 0.0, "z": 0.0}
    sd = float(a.std(ddof=1))
    if sd <= 1e-15:
        return {"t_len": len(a), "sr": 0.0, "skew": 0.0, "kurt": 3.0, "sr0": 0.0, "dsr": 0.0, "z": 0.0}
    mu = float(a.mean())
    sr = mu / sd
    c = a - mu
    m2 = float(np.mean(c ** 2))
    skew = float(np.mean(c ** 3) / (m2 ** 1.5))
    kurt = float(np.mean(c ** 4) / (m2 ** 2))
    out = dsr_probability(sr, len(a), skew, kurt, trials, sr_var, sr_mean=sr_mean)
    out.update(t_len=int(len(a)), skew=skew, kurt=kurt)
    return out


def effective_trials_from_corr(corr_matrix: np.ndarray, m_trials: int) -> dict[str, float]:
    """Bailey & Lopez de Prado (2014) Appendix A.3 Eq. (8)-(9): N_hat = rho_hat + (1 - rho_hat) * M."""
    m = int(m_trials)
    if m <= 1 or corr_matrix.size == 0:
        return {"avg_rho": 0.0, "m_trials": m, "n_eff": float(max(m, 1))}
    c = np.asarray(corr_matrix, dtype=float)
    k = c.shape[0]
    if k < 2:
        return {"avg_rho": 0.0, "m_trials": m, "n_eff": float(m)}
    off = c[~np.eye(k, dtype=bool)]
    off = off[np.isfinite(off)]
    avg_rho = float(np.mean(off)) if len(off) else 0.0
    avg_rho_clipped = min(1.0, max(-1.0 / max(k - 1, 1), avg_rho))
    # Use |avg_rho| or positive floor 0 for redundancy shrinkage per Eq. (9) interpretation
    rho_eff = max(0.0, avg_rho_clipped)
    n_eff = rho_eff + (1.0 - rho_eff) * m
    return {"avg_rho": avg_rho, "rho_eff": rho_eff, "m_trials": m, "n_eff": float(n_eff)}


def _audit_zoo(
    market: str,
    ppy: int,
    max_corr_sample: int = 400,
    seed: int = 20261118,
    root: Path | None = None,
) -> dict[str, Any]:
    fm_root = root or (REPO / "data/factor_mining")
    zoo_path = fm_root / market / "zoo.json"
    ens_path = fm_root / market / "ensemble.json"
    if not zoo_path.exists():
        return {"market": market, "missing": True}
    zoo_raw = json.loads(zoo_path.read_text(encoding="utf-8"))
    factors = zoo_raw.get("factors", []) if isinstance(zoo_raw, dict) else zoo_raw
    horizons_meta = (zoo_raw.get("horizons") or {}) if isinstance(zoo_raw, dict) else {}
    ens_raw = json.loads(ens_path.read_text(encoding="utf-8")) if ens_path.exists() else {}

    by_h: dict[int, list[dict[str, Any]]] = {}
    for f in factors:
        h = int(f.get("horizon") or 1)
        by_h.setdefault(h, []).append(f)

    all_horizons = sorted(
        set(by_h.keys()) | {int(k) for k in horizons_meta.keys() if str(k).isdigit()}
    )

    rng = np.random.default_rng(seed)
    summary_by_h = {}
    total_raw_trials = 0
    for h in all_horizons:
        items = by_h.get(h, [])
        h_meta = horizons_meta.get(str(h)) or {}
        m_trials = max(int(h_meta.get("trials") or 0), len(items), 1)
        total_raw_trials += int(h_meta.get("trials") or len(items))

        # Convert annualized ls_sharpe in zoo -> non-annualized period Sharpe (period = h days)
        # Recall evaluate.py: ls_sharpe = rmean / rstd * sqrt(ppy / h)
        ann = ppy / h
        sr_period_tr = []
        sr_period_va = []
        monthly_vecs = []
        months_ref: list[str] | None = None
        for f in items:
            tr_sr = (f.get("train") or {}).get("ls_sharpe")
            va_sr = (f.get("val") or {}).get("ls_sharpe")
            if isinstance(tr_sr, (int, float)) and math.isfinite(tr_sr):
                sr_period_tr.append(float(tr_sr) / math.sqrt(ann))
            if isinstance(va_sr, (int, float)) and math.isfinite(va_sr):
                sr_period_va.append(float(va_sr) / math.sqrt(ann))
            icm = f.get("ic_monthly") or []
            if icm:
                dmap = {str(m): float(v) for m, v in icm if isinstance(v, (int, float)) and math.isfinite(v)}
                if months_ref is None and len(dmap) >= 24:
                    months_ref = sorted(dmap)
                if dmap:
                    monthly_vecs.append(dmap)

        # Estimate pairwise monthly-IC correlation across retained subset, applied to true M_trials
        n_eff_info = {"avg_rho": 0.0, "rho_eff": 0.0, "n_eff": float(m_trials), "m_trials": m_trials}
        if months_ref and len(monthly_vecs) >= 2:
            pick = monthly_vecs
            if len(monthly_vecs) > max_corr_sample:
                idx = rng.choice(len(monthly_vecs), size=max_corr_sample, replace=False)
                pick = [monthly_vecs[i] for i in idx]
            mat = pd.DataFrame([{m: d.get(m, np.nan) for m in months_ref} for d in pick]).T
            cmat = mat.corr(min_periods=12).to_numpy()
            n_eff_info = effective_trials_from_corr(cmat, m_trials)

        var_tr = float(np.var(sr_period_tr, ddof=1)) if len(sr_period_tr) > 1 else 0.0
        var_va = float(np.var(sr_period_va, ddof=1)) if len(sr_period_va) > 1 else 0.0
        mean_va = float(np.mean(sr_period_va)) if sr_period_va else 0.0
        max_va = float(np.max(sr_period_va)) if sr_period_va else 0.0

        ens_h = ((ens_raw.get("horizons") or {}).get(str(h)) or {})
        ens_dsr = None
        if ens_h:
            val_m = ens_h.get("val") or {}
            t_obs = max(3, int((val_m.get("n") or 360) / h))
            sr_ens = float(val_m.get("ls_sharpe") or 0.0) / math.sqrt(ann)
            dsr_neff = dsr_probability(sr_ens, t_obs, 0.0, 3.0, max(1.0, n_eff_info["n_eff"]), var_va)
            ens_dsr = {
                "factor_ids": ens_h.get("factor_ids"),
                "val_ls_sharpe_ann": val_m.get("ls_sharpe"),
                "val_ls_ret_ann": val_m.get("ls_ret"),
                "n_non_overlap_val": t_obs,
                "sr_period": round(sr_ens, 5),
                "dsr_raw_M_normal_approx": dsr_probability(sr_ens, t_obs, 0.0, 3.0, m_trials, var_va),
                "dsr_neff_normal_approx": dsr_neff,
                "moments_note": "normal_approx_on_summary_only_see_nt_replay_for_fat_tail_kurtosis",
                "gate_status": "PASS" if dsr_neff["dsr"] >= 0.95 else "OVERFIT_WARNING",
            }

        summary_by_h[str(h)] = {
            "m_trials": m_trials,
            "retained_count": len(items),
            "count": m_trials,
            "corr_var_basis": "retained_zoo_subset",
            "train_sr_var": round(var_tr, 6),
            "val_sr_var": round(var_va, 6),
            "val_sr_mean": round(mean_va, 5),
            "val_sr_max": round(max_va, 5),
            "n_eff": {k: round(float(v), 4) for k, v in n_eff_info.items()},
            "ensemble": ens_dsr,
        }
    return {
        "market": market,
        "total_zoo": len(factors),
        "total_raw_trials": total_raw_trials,
        "horizons": summary_by_h,
    }


def _replay_live_crypto_nt_dsr(zoo_summary: dict[str, Any], root: Path | None = None) -> dict[str, Any]:
    """Build exact NautilusTrader daily L-S backtest on NAS MariaDB crypto_klines for live h=3 & h=7."""
    fm_root = root or (REPO / "data/factor_mining")
    panels = panel.load_panels("crypto", str(fm_root), tier="all")
    syms = sorted(panels.fields["close"].columns)
    cfg = nas_klines.load_config(timeout=30.0)
    nas_frames: dict[str, pd.DataFrame] = {}
    with nas_klines.ReadOnlyMysqlClient(cfg) as cli:
        for sym in syms:
            rows = nas_klines.fetch_crypto_bars(
                sym, interval="1d", start="2025-05-01", end="2026-11-16", client=cli
            )
            if not rows:
                continue
            df = pd.DataFrame(rows, columns=["date", "open", "high", "low", "close", "volume", "vwap"])
            df["date"] = pd.to_datetime(df["date"], utc=True)
            df = df.set_index("date")[["open", "high", "low", "close", "volume"]].astype(float).sort_index()
            if not df.empty:
                nas_frames[sym] = df

    if not nas_frames:
        return {"skipped_reason": "no_nas_crypto_frames"}
    max_nas_ts = max(df.index.max() for df in nas_frames.values())
    zoo_raw = json.loads((fm_root / "crypto/zoo.json").read_text(encoding="utf-8"))
    fmap = {f["id"]: f for f in zoo_raw.get("factors", [])}
    ens_raw = json.loads((fm_root / "crypto/ensemble.json").read_text(encoding="utf-8"))

    funding_rates: dict[str, dict[Any, float]] = {}
    if "funding" in panels.fields:
        f_df = panels.fields["funding"]
        for sym in f_df.columns:
            col = f_df[sym].dropna()
            funding_rates[str(sym)] = {pd.Timestamp(idx).date(): float(val) for idx, val in col.items()}
    carry_cfg = nt_engine.CarryConfig(
        market="crypto",
        funding_rates=funding_rates,
        default_funding_rate=0.0001,
        funding_multiplier=3.0,
    )

    # Compute causal ensemble scores directly from panels
    results = {}
    for h_str in ("3", "7"):
        h = int(h_str)
        ens_h = (ens_raw.get("horizons") or {}).get(h_str) or {}
        fids = ens_h.get("factor_ids") or []
        if not fids:
            continue
        selected_factors = [fmap[fid] for fid in fids if fid in fmap]
        scores = ensemble.ensemble_scores(selected_factors, panels)
        splits = panel.make_splits(panels.mask.index, h=h, windows=panels.windows)

        # Build target weights cleanly using causal quant_targets rule every h days across val+holdout
        oos_mask = splits["val"] | splits["holdout"]
        cutoff_naive = max_nas_ts.tz_localize(None) - dt.timedelta(days=1)
        oos_dates = [d for d in panels.mask.index[oos_mask] if d <= cutoff_naive]
        cols = sorted(nas_frames)
        targets = pd.DataFrame(np.nan, index=pd.DatetimeIndex([pd.Timestamp(d, tz="UTC") for d in oos_dates]), columns=cols)
        for d in oos_dates[::h]:
            row = scores.loc[d].where(panels.mask.loc[d]).dropna()
            if row.empty:
                continue
            pct = row.rank(pct=True)
            entry = {"factor_ids": fids, "scores": {str(s): {"z": round(float(z), 4), "pct": round(float(pct[s]), 4)} for s, z in row.items()}}
            snap = {"as_of": d.strftime("%Y-%m-%d"), "run_id": f"dsr-audit-h{h}", str(h): entry, "horizons": {str(h): entry}}
            if h == 3:
                tgt = quant_targets.build_targets("crypto", snap, panels, now=d + dt.timedelta(days=1))
                w_map = tgt.get("weights") or {} if tgt.get("status") == "ok" else {}
            else:
                sub_scores = row
                close_sub = panels.fields["close"].loc[:d, sub_scores.index]
                vol = close_sub.tail(31).pct_change(fill_method=None).rolling(30, min_periods=30).std(ddof=1).iloc[-1]
                valid_syms = [s for s in sub_scores.index if s in nas_frames and pd.notna(vol.get(s)) and vol.get(s) > 0]
                if len(valid_syms) < 15:
                    continue
                k_leg = max(1, int(math.floor(len(valid_syms) * 0.2)))
                ordered = sub_scores.loc[valid_syms].sort_values(kind="mergesort")
                shorts = list(ordered.index[:k_leg])
                longs = list(ordered.index[-k_leg:])
                w_l = quant_targets._capped_invvol(vol.loc[longs], 0.5, 0.1)
                w_s = -quant_targets._capped_invvol(vol.loc[shorts], 0.5, 0.1)
                w_map = {**w_l.to_dict(), **w_s.to_dict()}
            if w_map:
                ts_utc = pd.Timestamp(d, tz="UTC")
                targets.loc[ts_utc] = pd.Series(w_map, dtype=float).reindex(cols, fill_value=0.0)

        targets = targets.dropna(how="all")
        if targets.empty:
            continue
        eval_start = pd.Timestamp(oos_dates[0], tz="UTC")
        eval_end = pd.Timestamp(oos_dates[-1], tz="UTC")
        start_iso = (eval_start - dt.timedelta(days=2)).strftime("%Y-%m-%d")
        end_iso = (eval_end + dt.timedelta(days=5)).strftime("%Y-%m-%d")
        sub_frames = {s: df.loc[start_iso:end_iso] for s, df in nas_frames.items() if not df.loc[start_iso:end_iso].empty}
        targets = targets[[c for c in targets.columns if c in sub_frames]]

        bt = nt_engine.run_portfolio(
            sub_frames,
            targets,
            market="crypto",
            initial_capital=100_000.0,
            execution="next_open",
            commission=0.0005,
            slippage=0.0003,
            carry=carry_cfg,
            periods_per_year=365,
        )
        eq = bt["equity"].loc[eval_start:eval_end]
        clipped_metrics = nt_engine.compute_metrics(eq, periods_per_year=365, initial_capital=100_000.0)
        rets_daily = eq.pct_change().dropna()
        rets_h = eq.iloc[::h].pct_change().dropna()
        h_info = (zoo_summary.get("horizons") or {}).get(h_str) or {}
        sr_var_h = float(h_info.get("val_sr_var") or 0.001)
        sr_var_daily = sr_var_h / float(h)
        m_trials = int(h_info.get("m_trials") or h_info.get("count") or 1)
        n_eff = float((h_info.get("n_eff") or {}).get("n_eff") or m_trials)

        dsr_h_n1 = dsr_from_series(rets_h, 1, sr_var_h)
        dsr_h_neff = dsr_from_series(rets_h, n_eff, sr_var_h)
        dsr_h_raw_m = dsr_from_series(rets_h, m_trials, sr_var_h)
        dsr_d_neff = dsr_from_series(rets_daily, n_eff, sr_var_daily)
        results[f"h={h}"] = {
            "nt_metrics": clipped_metrics,
            "funding_paid_usd": round(float(bt["carry"].get("funding_paid", 0.0)), 2),
            "commission_paid_usd": round(float(bt.get("commission", 0.0)), 2),
            "n_daily_obs": len(rets_daily),
            "n_h_period_obs": len(rets_h),
            "daily_skew": round(dsr_d_neff["skew"], 4),
            "daily_kurt_pearson": round(dsr_d_neff["kurt"], 4),
            "daily_sr": round(dsr_d_neff["sr"], 5),
            "daily_dsr_neff": round(dsr_d_neff["dsr"], 4),
            "h_period_skew": round(dsr_h_n1["skew"], 4),
            "h_period_kurt_pearson": round(dsr_h_n1["kurt"], 4),
            "h_period_sr": round(dsr_h_n1["sr"], 5),
            "h_period_dsr_single_trial": round(dsr_h_n1["dsr"], 4),
            "h_period_dsr_effective_N": {
                "n_eff": round(n_eff, 1),
                "sr0_h": round(dsr_h_neff["sr0"], 5),
                "dsr": round(dsr_h_neff["dsr"], 4),
                "z": round(dsr_h_neff["z"], 3),
                "gate_status": "PASS" if dsr_h_neff["dsr"] >= 0.95 else "OVERFIT_WARNING",
            },
            "h_period_dsr_raw_M": {
                "m_trials": m_trials,
                "sr0_h": round(dsr_h_raw_m["sr0"], 5),
                "dsr": round(dsr_h_raw_m["dsr"], 4),
                "z": round(dsr_h_raw_m["z"], 3),
            },
        }
    return results


def audit_quantjourney_parity() -> dict[str, Any]:
    """Compare scripts/backtest/quantjourney.py:deflated_sharpe against Eq. (1)-(2)."""
    rng = np.random.default_rng(42)
    rets = rng.normal(0.001, 0.015, size=250)
    qj_val = quantjourney.deflated_sharpe(rets, trials=50, sharpe_variance=0.001)
    our_val = dsr_from_series(rets, trials=50, sr_var=0.001)["dsr"]
    return {
        "quantjourney_dsr": round(float(qj_val), 12),
        "exact_dsr": round(float(our_val), 12),
        "abs_diff": float(abs(qj_val - our_val)),
        "exact_match": abs(qj_val - our_val) < 1e-12,
    }


def format_markdown_report(audit: dict[str, Any]) -> str:
    """Render concise read-only nightly DSR & N_eff quality report."""
    lines = [
        f"# 因子库 Deflated Sharpe Ratio 夜间只读质检（{audit.get('generated_at', '')[:10]}）",
        "",
        "## 1. Zoo 有效独立试次数 `N_eff` 与验证集 DSR",
        "",
        "| 市场 | `h` | 真实总试次 `M` (留存样本) | 平均月度 IC 相关 `ρ̂` | 有效独立试次 `N_eff` | 验证期 `SR_val` 均值 | Ensemble 年化 SR | `DSR(N_eff)` | 门控状态 |",
        "|---|---:|---:|---:|---:|---:|---:|---:|---|",
    ]
    for mkey in ("crypto_zoo", "us_zoo"):
        zdoc = audit.get(mkey) or {}
        if zdoc.get("missing"):
            continue
        mkt = zdoc.get("market", mkey)
        for h, hinfo in sorted((zdoc.get("horizons") or {}).items(), key=lambda x: int(x[0])):
            neff = hinfo.get("n_eff") or {}
            ens = hinfo.get("ensemble") or {}
            dsr_val = ((ens.get("dsr_neff_normal_approx") or {}).get("dsr"))
            ens_sr = ens.get("val_ls_sharpe_ann")
            gate = ens.get("gate_status") or "NO_ENSEMBLE"
            m_tr = hinfo.get("m_trials", hinfo.get("count", 0))
            ret_cnt = hinfo.get("retained_count", 0)
            lines.append(
                f"| {mkt} | `h={h}` | {m_tr} ({ret_cnt}) | {neff.get('avg_rho', 0.0):.4f} | "
                f"{neff.get('n_eff', 0.0):.1f} | {hinfo.get('val_sr_mean', 0.0):.4f} | "
                f"{f'{ens_sr:.3f}' if ens_sr is not None else '—'} | "
                f"{f'{dsr_val:.4f}' if dsr_val is not None else '—'} | `{gate}` |"
            )

    replay = audit.get("nt_live_crypto_replay") or {}
    lines.extend(["", "## 2. NautilusTrader `val+holdout` 真实净值 DSR 审计", ""])
    if "skipped_reason" in replay:
        lines.append(f"- 跳过实盘口径重放：`{replay['skipped_reason']}`")
    elif not replay:
        lines.append("- 未启用或无加密回放结果")
    else:
        lines.append("| 主链 | 样本期总收益 | QuantStats Sharpe | MaxDD | 日峰度 `kurt` | `N_eff` | `DSR(N_eff)` | 门控状态 |")
        lines.append("|---|---:|---:|---:|---:|---:|---:|---|")
        for hlabel, info in sorted(replay.items()):
            m = info.get("nt_metrics") or {}
            eff = info.get("h_period_dsr_effective_N") or {}
            lines.append(
                f"| `{hlabel}` | {m.get('total_return', 0.0) * 100:+.2f}% | {m.get('sharpe', 0.0):.3f} | "
                f"{m.get('max_drawdown', 0.0) * 100:.2f}% | {info.get('daily_kurt_pearson', 0.0):.2f} | "
                f"{eff.get('n_eff', 0.0):.1f} | **{eff.get('dsr', 0.0):.4f}** | `{eff.get('gate_status', '')}` |"
            )
    lines.append("")
    return "\n".join(lines)


def run_audit(include_nt_replay: bool = True, *, root: Path | None = None) -> dict[str, Any]:
    fm_root = root or (REPO / "data/factor_mining")
    parity = audit_quantjourney_parity()
    crypto_zoo = _audit_zoo("crypto", ppy=365, root=fm_root)
    us_zoo = _audit_zoo("us", ppy=252, root=fm_root)

    ledger_path = REPO / "data/evolution/ledger.jsonl"
    ledger_hurdles = {}
    if ledger_path.exists():
        ledger_rows = [json.loads(l) for l in ledger_path.read_text(encoding="utf-8").splitlines() if l.strip()]
        tracks: dict[str, int] = {}
        for r in ledger_rows:
            if r.get("stage") == "S1" and r.get("verdict") != "invalid":
                t = str(r.get("track"))
                tracks[t] = tracks.get(t, 0) + 1
        for t, k in sorted(tracks.items()):
            z_bonf = NORM.inv_cdf(1.0 - 0.05 / (2.0 * max(k, 1)))
            z_evt_indep = expected_max_sr(max(k, 1), 1.0)
            n_eff_50 = 0.5 + 0.5 * k
            z_evt_corr50 = expected_max_sr(n_eff_50, 1.0)
            ledger_hurdles[t] = {
                "K": k,
                "bonferroni_z_K": round(z_bonf, 4),
                "evt_indep_hurdle": round(z_evt_indep, 4),
                "evt_rho0.5_hurdle": round(z_evt_corr50, 4),
            }

    out: dict[str, Any] = {
        "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "quantjourney_parity": parity,
        "crypto_zoo": crypto_zoo,
        "us_zoo": us_zoo,
        "ledger_hurdles": ledger_hurdles,
    }
    if include_nt_replay:
        try:
            out["nt_live_crypto_replay"] = _replay_live_crypto_nt_dsr(crypto_zoo, root=fm_root)
        except Exception as exc:
            out["nt_live_crypto_replay"] = {"skipped_reason": f"{type(exc).__name__}: {exc}"}
    return out


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--root", type=Path, default=REPO / "data/factor_mining")
    ap.add_argument("--no-replay", action="store_true")
    ap.add_argument("--out-json", type=Path, default=None)
    ap.add_argument("--out-md", type=Path, default=None)
    args = ap.parse_args(argv)
    res = run_audit(include_nt_replay=not args.no_replay, root=args.root)
    payload = json.dumps(res, indent=2, ensure_ascii=False)
    if args.out_json is not None:
        args.out_json.parent.mkdir(parents=True, exist_ok=True)
        args.out_json.write_text(payload + "\n", encoding="utf-8")
    if args.out_md is not None:
        args.out_md.parent.mkdir(parents=True, exist_ok=True)
        args.out_md.write_text(format_markdown_report(res), encoding="utf-8")
    if args.out_json is None and args.out_md is None:
        print(payload)
    else:
        # Concise summary line for nightly logs
        c_h3 = (((res.get("crypto_zoo") or {}).get("horizons") or {}).get("3") or {})
        nt_h3 = ((res.get("nt_live_crypto_replay") or {}).get("h=3") or {})
        eff = nt_h3.get("h_period_dsr_effective_N") or {}
        print(
            f"dsr_audit: crypto h=3 M={c_h3.get('count')} N_eff={((c_h3.get('n_eff') or {}).get('n_eff'))} "
            f"NT_DSR={eff.get('dsr', 'skipped')} gate={eff.get('gate_status', 'N/A')}"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
