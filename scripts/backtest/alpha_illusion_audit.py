"""Empirical P1-P6 audit of nofx LLM trading path (Ye et al., AlphaIllusion 2026).

Usage:
    PYTHONDONTWRITEBYTECODE=1 .venv/bin/python -m scripts.backtest.alpha_illusion_audit

Reads ``data/data.db`` strictly read-only. Computes:
  - P4 Epistemic Calibration: ECE (Eq. 3), reliability bins, Spearman corr(conf, R)
  - P5 Gross-to-Net + Inference Cost (Eq. 1): Gross PnL vs Exchange Fees vs AI token costs
  - P5/2.4 Window Uncertainty (Lo 2002 Eq. 2): Period/Annualized Sharpe, SE(SR), MinTRL
  - P2 Universe Mix: Tokenized stocks/ETFs vs Crypto majors vs Altcoins breakdown
"""
from __future__ import annotations

import argparse
from dataclasses import dataclass
import json
import math
from pathlib import Path
import sqlite3
from statistics import NormalDist
from typing import Any

import numpy as np
import pandas as pd


REPO = Path(__file__).resolve().parents[2]
DEFAULT_DB = REPO / "data" / "data.db"

CRYPTO_MAJORS = {
    "BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT", "XRPUSDT",
    "DOGEUSDT", "ADAUSDT", "AVAXUSDT", "LINKUSDT", "LTCUSDT",
    "TONUSDT", "SUIUSDT", "TRXUSDT", "DOTUSDT", "BCHUSDT",
}


def compute_ece(confidences: np.ndarray, outcomes: np.ndarray, n_bins: int = 5) -> dict[str, Any]:
    """Expected Calibration Error (AlphaIllusion Eq. 3, Guo et al. 2017).

    Parameters
    ----------
    confidences:
        Predicted win probabilities in [0, 1].
    outcomes:
        Binary outcomes in {0, 1} (1 = profitable trade).
    n_bins:
        Number of equal-width bins across populated range [min_bin, 1.0] or fixed 10%-wide bins.
    """
    c = np.asarray(confidences, dtype=float)
    y = np.asarray(outcomes, dtype=float)
    valid = np.isfinite(c) & np.isfinite(y)
    c, y = np.clip(c[valid], 0.0, 1.0), y[valid]
    n = len(c)
    if n == 0:
        return {"n": 0, "ece": None, "bins": []}

    edges = np.linspace(0.0, 1.0, n_bins + 1)
    ece = 0.0
    bins_out: list[dict[str, Any]] = []
    for i in range(n_bins):
        lo, hi = edges[i], edges[i + 1]
        if i == n_bins - 1:
            mask = (c >= lo) & (c <= hi)
        else:
            mask = (c >= lo) & (c < hi)
        cnt = int(mask.sum())
        if cnt == 0:
            continue
        acc = float(y[mask].mean())
        avg_conf = float(c[mask].mean())
        ece += (cnt / n) * abs(acc - avg_conf)
        bins_out.append({
            "range": f"[{lo:.2f}, {hi:.2f}{']' if i == n_bins - 1 else ')'}",
            "count": cnt,
            "avg_conf": round(avg_conf, 4),
            "win_rate": round(acc, 4),
            "gap": round(acc - avg_conf, 4),
        })
    return {"n": n, "ece": float(ece), "mean_conf": float(c.mean()), "mean_win": float(y.mean()), "bins": bins_out}


def lo_sharpe_se(sr: float, n_obs: int, periods_per_year: int = 365) -> dict[str, float | None]:
    """Lo (2002) period & annualized Sharpe standard error (AlphaIllusion Eq. 2)."""
    if n_obs < 3 or not math.isfinite(sr):
        return {"sr_period": None, "se_period": None, "sr_ann": None, "se_ann": None, "ci95_ann": None}
    se_period = math.sqrt((1.0 + 0.5 * sr * sr) / n_obs)
    ann = math.sqrt(periods_per_year)
    sr_ann = sr * ann
    se_ann = se_period * ann
    return {
        "sr_period": float(sr),
        "se_period": float(se_period),
        "sr_ann": float(sr_ann),
        "se_ann": float(se_ann),
        "ci95_ann": [float(sr_ann - 1.96 * se_ann), float(sr_ann + 1.96 * se_ann)],
    }


def min_track_record_length(sr_period: float, sr_bench: float = 0.0, skew: float = 0.0, kurt: float = 3.0, alpha: float = 0.05) -> float:
    """Minimum Track Record Length (Bailey & Lopez de Prado 2012/2014) in period units."""
    if sr_period <= sr_bench or not math.isfinite(sr_period):
        return math.inf
    z = NormalDist().inv_cdf(1.0 - alpha)
    var_adj = max(1e-12, 1.0 - skew * sr_period + ((kurt - 1.0) / 4.0) * (sr_period ** 2))
    return 1.0 + var_adj * ((z / (sr_period - sr_bench)) ** 2)


def classify_symbol(sym: str) -> str:
    s = str(sym or "").upper()
    if s.endswith("BUSDT") or s in {"NVDAUSDT", "TSLAUSDT", "AAPLUSDT", "AMZNUSDT", "METAUSDT", "MSFTUSDT", "GOOGUSDT", "COINUSDT", "MSTRUSDT", "SPYUSDT", "QQQUSDT", "GLDUSDT", "SLVUSDT", "INTCUSDT", "AMDUSDT"}:
        return "tokenized_us_etf"
    if s in CRYPTO_MAJORS:
        return "crypto_major"
    return "crypto_alt"


def _spearman(x: np.ndarray, y: np.ndarray) -> float | None:
    valid = np.isfinite(x) & np.isfinite(y)
    if valid.sum() < 10:
        return None
    sx = pd.Series(x[valid]).rank(method="average")
    sy = pd.Series(y[valid]).rank(method="average")
    if sx.std(ddof=1) <= 1e-12 or sy.std(ddof=1) <= 1e-12:
        return None
    return float(sx.corr(sy))


def _load_order_confidence_map(conn: sqlite3.Connection) -> dict[tuple[str, str], dict[str, Any]]:
    """Extract (trader_id, entry_order_id) -> {confidence, action, entry_path, ts} from decision_records."""
    out: dict[tuple[str, str], dict[str, Any]] = {}
    cur = conn.cursor()
    rows = cur.execute(
        "SELECT trader_id, timestamp, decisions FROM decision_records WHERE decisions IS NOT NULL AND decisions != '[]'"
    )
    for trader_id, ts, dec_json in rows:
        try:
            decs = json.loads(dec_json)
        except Exception:
            continue
        if not isinstance(decs, list):
            continue
        for d in decs:
            if not isinstance(d, dict):
                continue
            action = str(d.get("action") or "")
            if not action.startswith("open_"):
                continue
            oid = d.get("entry_order_id") or d.get("order_id")
            if oid in (None, "", 0, "0"):
                continue
            conf = d.get("confidence")
            try:
                conf_val = float(conf) if conf is not None else math.nan
            except (TypeError, ValueError):
                conf_val = math.nan
            out[(str(trader_id), str(oid))] = {
                "confidence": conf_val,
                "action": action,
                "symbol": str(d.get("symbol") or ""),
                "entry_path": d.get("entry_path"),
                "leverage": d.get("leverage"),
                "ts": ts,
            }
    return out


def parse_utc_timestamps(series: pd.Series) -> pd.Series:
    """Parse mixed epoch-ms integers/floats, epoch-seconds, or ISO strings into UTC Timestamps."""
    if len(series) == 0:
        return pd.Series(pd.DatetimeIndex([], tz="UTC"), index=series.index)
    num = pd.to_numeric(series, errors="coerce")
    out = pd.Series(pd.NaT, index=series.index, dtype="datetime64[ns, UTC]")
    is_num = num.notna()
    if is_num.any():
        vals = num[is_num]
        # Epoch ms >= 1e12 (~year 2001 in ms); epoch s < 1e12
        ms_mask = vals.abs() >= 1e12
        if ms_mask.any():
            out.loc[vals.index[ms_mask]] = pd.to_datetime(vals[ms_mask], unit="ms", utc=True)
        sec_mask = ~ms_mask
        if sec_mask.any():
            out.loc[vals.index[sec_mask]] = pd.to_datetime(vals[sec_mask], unit="s", utc=True)
    non_num = ~is_num
    if non_num.any():
        out.loc[series.index[non_num]] = pd.to_datetime(series[non_num], errors="coerce", utc=True)
    return out


def run_audit(db_path: Path = DEFAULT_DB) -> dict[str, Any]:
    if not db_path.exists():
        raise FileNotFoundError(f"Database not found: {db_path}")
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        traders = {
            r[0]: {"name": r[1], "strategy_id": r[2], "exchange_id": r[3], "is_running": r[4]}
            for r in conn.execute("SELECT id, name, strategy_id, exchange_id, is_running FROM traders").fetchall()
        }
        ai_charges = {
            r[0]: {"calls": int(r[1]), "cost_usd": float(r[2] or 0.0)}
            for r in conn.execute("SELECT trader_id, COUNT(*), SUM(cost_usd) FROM ai_charges GROUP BY trader_id").fetchall()
        }
        conf_map = _load_order_confidence_map(conn)

        pos_rows = conn.execute("""
            SELECT id, trader_id, symbol, side, entry_price, exit_price, initial_stop_loss,
                   entry_quantity, realized_pnl, fee, entry_time, exit_time, close_reason,
                   entry_order_id, source
            FROM trader_positions
            WHERE LOWER(status) = 'closed'
        """).fetchall()
        action_counts: dict[str, int] = {}
        for (dec_j,) in conn.execute("SELECT decisions FROM decision_records WHERE decisions IS NOT NULL AND decisions != '[]'"):
            try:
                decs = json.loads(dec_j)
            except Exception:
                continue
            if isinstance(decs, list):
                for d in decs:
                    if isinstance(d, dict):
                        a = str(d.get("action") or "unknown")
                        action_counts[a] = action_counts.get(a, 0) + 1
    finally:
        conn.close()

    records: list[dict[str, Any]] = []
    for row in pos_rows:
        (pid, tid, sym, side, ep, xp, isl, qty, pnl, fee, etime, xtime, reason, eoid, src) = row
        pnl_f = float(pnl or 0.0)
        fee_f = float(fee or 0.0)
        # trader_positions.realized_pnl stores pre-fee gross PnL (consistent with scripts/promotion/report.py).
        gross_f = pnl_f
        net_f = pnl_f - fee_f
        risk_usd = None
        r_mult = math.nan
        if ep and isl and qty and ep > 0 and isl > 0 and qty > 0:
            dist = abs(float(ep) - float(isl))
            if dist > 1e-12:
                risk_usd = dist * float(qty)
                r_mult = net_f / risk_usd
        meta = conf_map.get((str(tid), str(eoid or "")), {})
        records.append({
            "id": pid,
            "trader_id": tid,
            "trader_name": traders.get(tid, {}).get("name") or tid,
            "symbol": sym,
            "asset_class": classify_symbol(sym),
            "side": side,
            "net_pnl": net_f,
            "fee": fee_f,
            "gross_pnl": gross_f,
            "risk_usd": risk_usd,
            "r_mult": r_mult,
            "win": 1.0 if net_f > 0 else 0.0,
            "confidence": meta.get("confidence", math.nan),
            "entry_time": etime,
            "exit_time": xtime,
            "close_reason": reason,
            "matched_decision": bool(meta),
        })

    df = pd.DataFrame(records)
    # 1. Per-trader P5 gross-to-net + AI cost + Lo(2002) SE(SR) on daily PnL series
    trader_summaries: list[dict[str, Any]] = []
    for tid, grp in df.groupby("trader_id"):
        name = grp["trader_name"].iloc[0]
        ai = ai_charges.get(tid, {"calls": 0, "cost_usd": 0.0})
        gross_sum = float(grp["gross_pnl"].sum())
        fee_sum = float(grp["fee"].sum())
        net_sum = float(grp["net_pnl"].sum())
        ai_cost = float(ai["cost_usd"])
        net_after_ai = net_sum - ai_cost

        # Group by UTC close date for daily Sharpe & Lo(2002) SE
        x_dates = parse_utc_timestamps(grp["exit_time"]).dropna()
        if len(x_dates) > 0:
            daily_pnl = grp.loc[x_dates.index].assign(d=x_dates.dt.floor("D")).groupby("d")["net_pnl"].sum()
            # Reindex to contiguous calendar days so inactive days have 0 PnL (no active-day selection bias)
            full_idx = pd.date_range(daily_pnl.index.min(), daily_pnl.index.max(), freq="D", tz="UTC")
            daily_full = daily_pnl.reindex(full_idx, fill_value=0.0)
            sd = float(daily_full.std(ddof=1)) if len(daily_full) > 1 else 0.0
            sr_day = float(daily_full.mean() / sd) if sd > 1e-12 else 0.0
            lo_stats = lo_sharpe_se(sr_day, len(daily_full), 365)
            sk = float(daily_full.skew()) if len(daily_full) >= 3 and sd > 1e-12 else 0.0
            ku = float(daily_full.kurt() + 3.0) if len(daily_full) >= 4 and sd > 1e-12 else 3.0
            mintrl = min_track_record_length(sr_day, 0.0, sk, ku)
            n_days = len(daily_full)
            first_close = x_dates.min().strftime("%Y-%m-%d")
            last_close = x_dates.max().strftime("%Y-%m-%d")
        else:
            lo_stats = lo_sharpe_se(0.0, 0)
            mintrl = math.inf
            n_days = 0
            first_close = None
            last_close = None

        r_valid = grp["r_mult"].dropna()
        trader_summaries.append({
            "trader_id": tid,
            "name": name,
            "trades": int(len(grp)),
            "win_rate": round(float(grp["win"].mean()), 4),
            "avg_r": round(float(r_valid.mean()), 4) if len(r_valid) else None,
            "gross_pnl": round(gross_sum, 2),
            "fees": round(fee_sum, 2),
            "net_pnl": round(net_sum, 2),
            "ai_calls": ai["calls"],
            "ai_cost_usd": round(ai_cost, 2),
            "net_after_ai_usd": round(net_after_ai, 2),
            "first_close": first_close,
            "last_close": last_close,
            "calendar_days": int(n_days),
            "sr_ann": round(lo_stats["sr_ann"], 3) if lo_stats["sr_ann"] is not None else None,
            "se_ann": round(lo_stats["se_ann"], 3) if lo_stats["se_ann"] is not None else None,
            "ci95_ann": [round(x, 3) for x in lo_stats["ci95_ann"]] if lo_stats["ci95_ann"] else None,
            "min_trl_days": round(mintrl, 1) if math.isfinite(mintrl) else "inf",
        })
    trader_summaries.sort(key=lambda x: x["trades"], reverse=True)

    # 2. P4 Calibration across all matched LLM decisions with numeric confidence
    matched = df[df["confidence"].notna() & np.isfinite(df["confidence"])].copy()
    matched["conf_prob"] = matched["confidence"] / 100.0
    ece_all = compute_ece(matched["conf_prob"].to_numpy(), matched["win"].to_numpy(), n_bins=10)
    # Also compute custom fine-grained bins around actual LLM operating range (60..100)
    fine_bins = []
    for lo, hi in [(0, 70), (70, 75), (75, 80), (80, 85), (85, 101)]:
        sub = matched[(matched["confidence"] >= lo) & (matched["confidence"] < hi)]
        if len(sub) == 0:
            continue
        r_sub = sub["r_mult"].dropna()
        fine_bins.append({
            "conf_bucket": f"[{lo}, {hi})",
            "n": int(len(sub)),
            "avg_conf": round(float(sub["confidence"].mean()), 2),
            "win_rate": round(float(sub["win"].mean()), 4),
            "avg_r": round(float(r_sub.mean()), 4) if len(r_sub) else None,
            "net_pnl": round(float(sub["net_pnl"].sum()), 2),
        })
    corr_conf_win = _spearman(matched["confidence"].to_numpy(), matched["win"].to_numpy())
    corr_conf_r = _spearman(matched["confidence"].to_numpy(), matched["r_mult"].to_numpy())

    # 3. P2 Universe / Asset-Class Breakdown (overall + mac-nofx specifically)
    asset_mix_all = []
    for cls, grp in df.groupby("asset_class"):
        rv = grp["r_mult"].dropna()
        asset_mix_all.append({
            "asset_class": cls,
            "trades": int(len(grp)),
            "win_rate": round(float(grp["win"].mean()), 4),
            "avg_r": round(float(rv.mean()), 4) if len(rv) else None,
            "gross_pnl": round(float(grp["gross_pnl"].sum()), 2),
            "net_pnl": round(float(grp["net_pnl"].sum()), 2),
            "fees": round(float(grp["fee"].sum()), 2),
        })

    mac_df = df[df["trader_name"] == "mac-nofx"]
    mac_mix = []
    for cls, grp in mac_df.groupby("asset_class"):
        rv = grp["r_mult"].dropna()
        mac_mix.append({
            "asset_class": cls,
            "trades": int(len(grp)),
            "win_rate": round(float(grp["win"].mean()), 4),
            "avg_r": round(float(rv.mean()), 4) if len(rv) else None,
            "net_pnl": round(float(grp["net_pnl"].sum()), 2),
        })

    return {
        "total_closed_trades": int(len(df)),
        "matched_confidence_trades": int(len(matched)),
        "traders": trader_summaries,
        "p4_calibration": {
            "ece_10bin": round(ece_all["ece"], 4) if ece_all["ece"] is not None else None,
            "mean_conf_prob": round(ece_all["mean_conf"], 4) if ece_all.get("mean_conf") is not None else None,
            "empirical_win_rate": round(ece_all["mean_win"], 4) if ece_all.get("mean_win") is not None else None,
            "overconfidence_gap": round(ece_all["mean_conf"] - ece_all["mean_win"], 4) if ece_all.get("mean_conf") is not None else None,
            "spearman_conf_win": round(corr_conf_win, 4) if corr_conf_win is not None else None,
            "spearman_conf_r": round(corr_conf_r, 4) if corr_conf_r is not None else None,
            "operating_bins": fine_bins,
        },
        "p2_asset_mix_all": asset_mix_all,
        "p2_asset_mix_mac_nofx": mac_mix,
        "p3_decision_action_counts": dict(sorted(action_counts.items(), key=lambda x: -x[1])),
    }


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", type=Path, default=DEFAULT_DB)
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args(argv)
    res = run_audit(args.db)
    print(json.dumps(res, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
