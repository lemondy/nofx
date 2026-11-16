"""Offline side-by-side parity & performance benchmark: NautilusTrader + QuantStats vs Backtrader.

Read-only against existing frozen snapshots; writes no ledger events or production configs.
Usage:
    PYTHONDONTWRITEBYTECODE=1 .venv/bin/python -m scripts.backtest.nt_parity
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import time

import numpy as np
import pandas as pd

from scripts.factor_mining import panel, quant_targets
from scripts.factor_mining.ensemble import ensemble_scores
from . import fq_us_momentum_study as fq_us
from .nt_engine import CarryConfig, compute_metrics, run_portfolio
from .quantjourney import CAPITAL, RebalancePolicy, run_weights

REPO = Path(__file__).resolve().parents[2]
US_SNAPSHOT = REPO / "data/factor_mining/us_momentum/snapshot_20261105"
CRYPTO_SNAPSHOT = REPO / "data/factor_mining/fq_exit_funding/snapshot_20261108"


def _corr_pair(a: pd.Series, b: pd.Series) -> tuple[float, float]:
    joined = pd.concat([a.rename("a"), b.rename("b")], axis=1).dropna()
    if len(joined) < 5:
        return 0.0, 0.0
    eq_corr = float(joined["a"].corr(joined["b"]))
    ra = joined["a"].pct_change().dropna()
    rb = joined["b"].pct_change().dropna()
    ret_corr = float(ra.corr(rb)) if len(ra) >= 5 else 0.0
    return eq_corr, ret_corr


def run_us_long_only_parity(snapshot: Path = US_SNAPSHOT) -> dict:
    """1:1 single-sleeve parity check (90 US equities Long-only, same cash & 13bp fee)."""
    universe, _, frames, calendar, _ = fq_us.load_snapshot(snapshot)
    features = fq_us.build_features(frames, universe, calendar)
    sessions, decisions = fq_us.rebalance_schedule(features, calendar)
    stock_frames = {s: frames[s].loc[sessions[0] : sessions[-1]] for s in universe}

    t0 = time.perf_counter()
    bt_long = fq_us.run_sleeve(
        {s: frames[s] for s in universe},
        sessions,
        {r["date"]: r["long"] for r in decisions},
        cash=50_000.0,
        commission=0.0013,
    )
    bt_sec = time.perf_counter() - t0
    bt_eq = pd.Series(
        [a["equity"] for a in bt_long["equity_curve"]],
        index=pd.DatetimeIndex(pd.to_datetime([a["date"] for a in bt_long["equity_curve"]], utc=True)),
        dtype=float,
    )
    bt_metrics = compute_metrics(bt_eq, periods_per_year=252)

    tgt_rows = {pd.Timestamp(dec["date"], tz="UTC"): dict(dec["long"]) for dec in decisions}
    targets_df = pd.DataFrame.from_dict(tgt_rows, orient="index").reindex(columns=universe, fill_value=0.0).sort_index()

    t1 = time.perf_counter()
    nt_res = run_portfolio(
        stock_frames,
        targets_df,
        market="us",
        initial_capital=50_000.0,
        execution="close",
        commission=0.0013,
        slippage=0.0,
        leverage=1.0,
    )
    nt_sec = time.perf_counter() - t1

    eq_corr, ret_corr = _corr_pair(bt_eq, nt_res["equity"])
    return {
        "benchmark": "fq_us_long_only_1to1 (90 equities)",
        "bars": len(sessions),
        "bt_time_s": round(bt_sec, 3),
        "nt_time_s": round(nt_sec, 3),
        "speedup": round(bt_sec / nt_sec, 2) if nt_sec > 0 else None,
        "eq_corr": round(eq_corr, 6),
        "ret_corr": round(ret_corr, 6),
        "backtrader": {
            "final_equity": round(float(bt_eq.iloc[-1]), 2),
            "cagr": round(bt_metrics["cagr"], 4),
            "sharpe": round(bt_metrics["sharpe"], 4),
            "max_dd": round(bt_metrics["max_drawdown"], 4),
            "commission": round(float(bt_long["metrics"]["commission"]), 2),
            "fills": len(bt_long["fills"]),
        },
        "nautilus": {
            "final_equity": round(float(nt_res["equity"].iloc[-1]), 2),
            "cagr": round(nt_res["metrics"]["cagr"], 4),
            "sharpe": round(nt_res["metrics"]["sharpe"], 4),
            "max_dd": round(nt_res["metrics"]["max_drawdown"], 4),
            "commission": round(float(nt_res["commission"]), 2),
            "fills": int(nt_res["fill_count"]),
        },
    }


def run_us_momentum_parity(snapshot: Path = US_SNAPSHOT) -> dict:
    universe, _, frames, calendar, _ = fq_us.load_snapshot(snapshot)
    features = fq_us.build_features(frames, universe, calendar)
    sessions, decisions = fq_us.rebalance_schedule(features, calendar)
    stock_frames = {s: frames[s].loc[sessions[0] : sessions[-1]] for s in universe}

    # 1. Legacy Backtrader dual-sleeve benchmark (50k L + 50k S stitched)
    t0 = time.perf_counter()
    bt_long = fq_us.run_sleeve(
        {s: frames[s] for s in universe},
        sessions,
        {r["date"]: r["long"] for r in decisions},
        cash=50_000.0,
        commission=0.0013,
    )
    bt_short = fq_us.run_sleeve(
        {s: frames[s] for s in universe},
        sessions,
        {r["date"]: r["short"] for r in decisions},
        cash=50_000.0,
        commission=0.0007,
        short=True,
    )
    bt_sec = time.perf_counter() - t0
    bt_eq = pd.Series(
        [a["equity"] + b["equity"] for a, b in zip(bt_long["equity_curve"], bt_short["equity_curve"])],
        index=pd.DatetimeIndex(pd.to_datetime([a["date"] for a in bt_long["equity_curve"]], utc=True)),
        dtype=float,
    )
    bt_metrics = compute_metrics(bt_eq, periods_per_year=252)

    # 2. NautilusTrader unified margin account (100k L+S combined, 50%L + 50%S target weights)
    # Using exact half-weight scaling (0.5 * L + 0.5 * S) matches 50k/50k gross allocation cleanly.
    tgt_rows = {}
    for dec in decisions:
        combined = {}
        for s, w in dec["long"].items():
            combined[s] = 0.5 * float(w)
        for s, w in dec["short"].items():
            combined[s] = combined.get(s, 0.0) + 0.5 * float(w)
        tgt_rows[pd.Timestamp(dec["date"], tz="UTC")] = combined
    targets_df = pd.DataFrame.from_dict(tgt_rows, orient="index").reindex(columns=universe, fill_value=0.0).sort_index()

    t1 = time.perf_counter()
    nt_res = run_portfolio(
        stock_frames,
        targets_df,
        market="us",
        initial_capital=100_000.0,
        execution="close",
        commission=0.0010,  # blended L/S avg of 13bp / 7bp
        slippage=0.0,
        leverage=4.0,
    )
    nt_sec = time.perf_counter() - t1

    eq_corr, ret_corr = _corr_pair(bt_eq, nt_res["equity"])
    return {
        "benchmark": "fq_us_momentum_ls (90 equities)",
        "bars": len(sessions),
        "bt_time_s": round(bt_sec, 3),
        "nt_time_s": round(nt_sec, 3),
        "speedup": round(bt_sec / nt_sec, 2) if nt_sec > 0 else None,
        "eq_corr": round(eq_corr, 6),
        "ret_corr": round(ret_corr, 6),
        "backtrader": {
            "final_equity": round(float(bt_eq.iloc[-1]), 2),
            "cagr": round(bt_metrics["cagr"], 4),
            "sharpe": round(bt_metrics["sharpe"], 4),
            "max_dd": round(bt_metrics["max_drawdown"], 4),
            "commission": round(bt_long["metrics"]["commission"] + bt_short["metrics"]["commission"], 2),
            "fills": len(bt_long["fills"]) + len(bt_short["fills"]),
        },
        "nautilus": {
            "final_equity": round(float(nt_res["equity"].iloc[-1]), 2),
            "cagr": round(nt_res["metrics"]["cagr"], 4),
            "sharpe": round(nt_res["metrics"]["sharpe"], 4),
            "max_dd": round(nt_res["metrics"]["max_drawdown"], 4),
            "commission": round(nt_res["commission"], 2),
            "fills": nt_res["fill_count"],
        },
    }


def run_crypto_portfolio_parity(snapshot: Path = CRYPTO_SNAPSHOT) -> dict:
    zoo = json.loads((snapshot / "crypto/zoo.json").read_text())
    ens = json.loads((snapshot / "crypto/ensemble.json").read_text())
    wanted = set(ens["horizons"]["3"]["factor_ids"]) if "horizons" in ens else set(ens["3"]["factor_ids"])
    factors = [f for f in zoo["factors"] if f["id"] in wanted]
    panels = panel.load_panels("crypto", root=snapshot)
    scores = ensemble_scores(factors, panels)

    dates = scores.index[-240:]  # last ~8 months covering val/forward window cleanly
    targets = pd.DataFrame(np.nan, index=dates, columns=scores.columns)
    for d in dates[::3]:
        row = scores.loc[d].where(panels.mask.loc[d]).dropna()
        if row.empty:
            continue
        pct = row.rank(pct=True)
        entry = {
            "factor_ids": [f["id"] for f in factors],
            "scores": {str(s): {"z": round(float(z), 4), "pct": round(float(pct[s]), 4)} for s, z in row.items()},
        }
        snap = {"as_of": d.strftime("%Y-%m-%d"), "run_id": "parity", "horizons": {"3": entry}}
        res = quant_targets.build_targets("crypto", snap, panels, now=d + pd.to_timedelta(1, unit="D"))
        if res["status"] == "ok":
            targets.loc[d] = pd.Series(res["weights"], dtype=float).reindex(scores.columns, fill_value=0.0)
    targets = targets.ffill().fillna(0.0)

    frames = {}
    for sym in scores.columns:
        o = panels.fields["open"][sym].reindex(dates)
        h = panels.fields["high"][sym].reindex(dates)
        l = panels.fields["low"][sym].reindex(dates)
        c = panels.fields["close"][sym].reindex(dates)
        v = panels.fields["volume"][sym].reindex(dates).fillna(0.0).clip(lower=0.0)
        df = pd.DataFrame({"open": o, "high": h, "low": l, "close": c, "volume": v}, index=dates)
        if not np.isfinite(df[["open", "high", "low", "close"]].to_numpy()).all() or (df[["open", "high", "low", "close"]].to_numpy() <= 0).any():
            continue
        df["high"] = np.maximum(df["high"], df[["open", "close"]].max(axis=1))
        df["low"] = np.minimum(df["low"], df[["open", "close"]].min(axis=1))
        frames[sym] = df
    targets = targets.reindex(columns=sorted(frames), fill_value=0.0)

    rates = {}
    if "funding" in panels.fields:
        f_df = panels.fields["funding"]
        for sym in f_df.columns:
            col = f_df[sym].dropna()
            rates[sym] = {pd.Timestamp(idx).date(): float(val) for idx, val in col.items()}

    # 1. Legacy Backtrader + CarryBroker (quantjourney.run_weights)
    t0 = time.perf_counter()
    bt_res = run_weights(
        frames,
        targets,
        market="crypto",
        policy=RebalancePolicy("D"),
        commission=0.0005,
        slippage=0.0003,
        funding=rates,
        charge_carry=True,
    )
    bt_sec = time.perf_counter() - t0
    bt_eq = pd.Series(
        [float(v) for v in bt_res["daily_equity"].values()],
        index=pd.DatetimeIndex(pd.to_datetime(list(bt_res["daily_equity"].keys()), utc=True)),
        dtype=float,
    )
    bt_metrics = compute_metrics(bt_eq, periods_per_year=365)

    # 2. NautilusTrader unified engine + CarryFundingModule
    rebal_targets = targets.iloc[::3]
    t1 = time.perf_counter()
    nt_res = run_portfolio(
        frames,
        rebal_targets,
        market="crypto",
        initial_capital=float(CAPITAL),
        execution="next_open",
        commission=0.0005,
        slippage=0.0003,
        carry=CarryConfig(market="crypto", funding_rates=rates, funding_multiplier=3.0),
        periods_per_year=365,
    )
    nt_sec = time.perf_counter() - t1

    eq_corr, ret_corr = _corr_pair(bt_eq, nt_res["equity"])
    return {
        "benchmark": "fq_crypto_portfolio (15 perps + funding)",
        "bars": len(dates),
        "bt_time_s": round(bt_sec, 3),
        "nt_time_s": round(nt_sec, 3),
        "speedup": round(bt_sec / nt_sec, 2) if nt_sec > 0 else None,
        "eq_corr": round(eq_corr, 6),
        "ret_corr": round(ret_corr, 6),
        "backtrader": {
            "final_equity": round(float(bt_eq.iloc[-1]), 2),
            "cagr": round(bt_metrics["cagr"], 4),
            "sharpe": round(bt_metrics["sharpe"], 4),
            "max_dd": round(bt_metrics["max_drawdown"], 4),
            "commission": round(float(bt_res["metrics"]["commission_paid"]), 2),
            "funding_paid": round(float(bt_res["metrics"]["funding_paid"]), 2),
            "fills": int(bt_res["metrics"]["trades"]),
        },
        "nautilus": {
            "final_equity": round(float(nt_res["equity"].iloc[-1]), 2),
            "cagr": round(nt_res["metrics"]["cagr"], 4),
            "sharpe": round(nt_res["metrics"]["sharpe"], 4),
            "max_dd": round(nt_res["metrics"]["max_drawdown"], 4),
            "commission": round(float(nt_res["commission"]), 2),
            "funding_paid": round(float(nt_res["carry"].get("funding_paid", 0.0)), 2),
            "fills": int(nt_res["fill_count"]),
        },
    }


def run_benchmarks(benchmark: str = "all") -> list[dict]:
    results = []
    if benchmark in ("all", "us_long") and US_SNAPSHOT.is_dir():
        results.append(run_us_long_only_parity())
    if benchmark in ("all", "us_ls") and US_SNAPSHOT.is_dir():
        results.append(run_us_momentum_parity())
    if benchmark in ("all", "crypto") and CRYPTO_SNAPSHOT.is_dir():
        results.append(run_crypto_portfolio_parity())
    return results


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(prog="python -m scripts.backtest.nt_parity")
    ap.add_argument("--benchmark", choices=["all", "us_long", "us_ls", "crypto"], default="all")
    ap.add_argument("--out", help="Optional path to write JSON benchmark report")
    a = ap.parse_args(argv)
    results = run_benchmarks(a.benchmark)
    payload = json.dumps(results, indent=2, ensure_ascii=False)
    if a.out:
        Path(a.out).parent.mkdir(parents=True, exist_ok=True)
        Path(a.out).write_text(payload + "\n")
    print(payload)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
