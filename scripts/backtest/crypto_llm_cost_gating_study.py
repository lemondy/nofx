"""crypto_llm AI-Cost Event-Gating & Cadence S1 Study (G1 K=10, G2 K=11).

Strictly bound to frozen pre-registration:
  docs/research/2026-11-18-crypto-llm-cost-gating-s1-prereg.md
  PREREG_SHA256 = 6bed6419559e76e7b80f97cf377c2aaa18c98febafdb1e24d4123ed63656c0aa

Reads ``data/data.db`` strictly read-only and queries NAS MariaDB ``crypto_klines``
(1h bars via ``nas_klines.fetch_crypto_bars(..., fmt="dict")``) with strict
causality ``close_time < decision_ts_ms``.
"""
from __future__ import annotations

import argparse
from bisect import bisect_right
import datetime as dt
import hashlib
import json
import math
from pathlib import Path
import sqlite3
from typing import Any

import numpy as np
import pandas as pd

from scripts.backtest import evolution_ledger, incremental_filter, nas_klines
from scripts.backtest.alpha_illusion_audit import parse_utc_timestamps
from scripts.promotion import report as promo_report


REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / "docs/research/2026-11-18-crypto-llm-cost-gating-s1-prereg.md"
PREREG_SHA256 = "6bed6419559e76e7b80f97cf377c2aaa18c98febafdb1e24d4123ed63656c0aa"
DEFAULT_DB = REPO / "data" / "data.db"
LEDGER = REPO / "data/evolution/ledger.jsonl"

UNIT_AI_COST_USD = 0.01
SEED = 20261118
PLACEBO_N = 2000

POOLED_6_PAPER = [
    ("因子 ensemble 参考字段-对照", "a8eb1bdf_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_273a584a-0c4d-43cf-9ef9-758a7c1d5bea"),
    ("因子 ensemble 参考字段-候选", "a2dc5705_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_89bba6d6-adab-4eed-ad8b-60cfb1cfac2a"),
    ("提前减仓 0.8R（1/3）-候选", "ec830bb2_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_bde3f7ab-001a-4dc6-9cfc-1acbb09f287d"),
    ("提前减仓 0.8R（1/3）-对照", "895dc8b3_76fc_444b-b392_68941e73041c_glm_82245a32-c08d-4678-b0bd-f5cbb1003b02"),
    ("C1 限价带宽入场（执行层）-候选", "d211acdd_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_889471e5-66c8-4a00-9269-995089508c41"),
    ("C1 限价带宽入场（执行层）-对照", "00a1dd1f_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_d712a9cc-86b9-4c16-9843-329a4380dda1"),
]
# Wait: verify exact trader_id strings dynamically by trader name in DB to avoid any copy typo!
POOLED_6_NAMES = [
    "因子 ensemble 参考字段-对照",
    "因子 ensemble 参考字段-候选",
    "提前减仓 0.8R（1/3）-候选",
    "提前减仓 0.8R（1/3）-对照",
    "C1 限价带宽入场（执行层）-候选",
    "C1 限价带宽入场（执行层）-对照",
]
MAC_NOFX_NAME = "mac-nofx"
PLANNED_K = 11


def verify_prereg() -> bytes:
    raw = PREREG.read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    if digest != PREREG_SHA256:
        raise ValueError(f"Preregistration SHA-256 mismatch: {digest} != {PREREG_SHA256}")
    return raw


def is_all_candidates_blocked(cv_json: str | None) -> bool:
    """Return True iff candidate_verdicts is non-empty JSON list and every item is 'blocked'."""
    if not cv_json or cv_json.strip() in ("", "[]", "null"):
        return False
    try:
        cvs = json.loads(cv_json)
    except Exception:
        return False
    if not isinstance(cvs, list) or not cvs:
        return False
    dicts = [x for x in cvs if isinstance(x, dict)]
    return bool(dicts) and all(str(x.get("status") or "") == "blocked" for x in dicts)


def parse_decisions_meta(dec_json: str | None) -> tuple[list[str], bool, list[dict[str, str]]]:
    """Extract (opened_order_ids, has_ai_close, close_items) from cycle decisions JSON."""
    if not dec_json or dec_json.strip() in ("", "[]", "null"):
        return [], False, []
    try:
        decs = json.loads(dec_json)
    except Exception:
        return [], False, []
    if not isinstance(decs, list):
        return [], False, []
    open_oids: list[str] = []
    has_close = False
    closes: list[dict[str, str]] = []
    for d in decs:
        if not isinstance(d, dict):
            continue
        act = str(d.get("action") or "")
        if act.startswith("open_"):
            oid = str(d.get("entry_order_id") or d.get("order_id") or "")
            if oid and oid != "0":
                open_oids.append(oid)
        elif act.startswith("close_") or act.startswith("reduce_") or act.startswith("partial_"):
            has_close = True
            closes.append({"symbol": str(d.get("symbol") or ""), "action": act})
    return open_oids, has_close, closes


def build_causal_1h_btc_index(bars_dict: list[dict[str, Any]]) -> tuple[list[int], list[dict[str, float]]]:
    """Index 1h bars sorted by close_time_ms for O(log N) strictly causal lookup."""
    ordered = sorted(bars_dict, key=lambda b: int(b["close_time"]))
    close_times: list[int] = []
    feats: list[dict[str, float]] = []
    for b in ordered:
        ct = int(b["close_time"])
        o = float(b["open"])
        h = float(b["high"])
        l = float(b["low"])
        c = float(b["close"])
        if o <= 0:
            continue
        close_times.append(ct)
        feats.append(
            {
                "abs_ret_1h": abs(c / o - 1.0),
                "range_1h": (h - l) / o,
                "close": c,
            }
        )
    return close_times, feats


def is_btc_quiet_causal(
    decision_ts_ms: int,
    btc_close_times: list[int],
    btc_feats: list[dict[str, float]],
    *,
    max_abs_ret: float = 0.008,
    max_range: float = 0.015,
    max_staleness_ms: int = 7_200_000,
) -> bool:
    """Strictly causal check on most recent closed BTC 1h bar (`close_time < decision_ts_ms`).

    Fail-open (returns False -> do NOT suppress LLM) if NAS data is stale/missing.
    """
    idx = bisect_right(btc_close_times, decision_ts_ms - 1) - 1
    if idx < 0:
        return False
    if decision_ts_ms - btc_close_times[idx] > max_staleness_ms:
        return False
    f = btc_feats[idx]
    return f["abs_ret_1h"] < max_abs_ret and f["range_1h"] < max_range


def apply_gate_rules(
    cycles: list[dict[str, Any]],
    btc_close_times: list[int],
    btc_feats: list[dict[str, float]],
    *,
    mode: str,
) -> list[bool]:
    """Return boolean mask `skipped[i]` over a single trader's chronological real-LLM cycles."""
    skipped: list[bool] = []
    last_executed_ms: int | None = None
    prev_all_blocked = False

    for cy in cycles:
        ts_ms = int(cy["ts_ms"])
        all_blk = bool(cy["all_blocked"])
        if mode == "G1":
            skip = all_blk
        elif mode == "G2":
            if all_blk:
                skip = True
            else:
                newly_unblocked = prev_all_blocked and (not all_blk)
                within_15m = (last_executed_ms is not None) and ((ts_ms - last_executed_ms) < 15 * 60 * 1000)
                if within_15m and not newly_unblocked:
                    skip = is_btc_quiet_causal(ts_ms, btc_close_times, btc_feats)
                else:
                    skip = False
        else:
            raise ValueError(f"Unknown gate mode: {mode}")

        skipped.append(skip)
        if not skip:
            last_executed_ms = ts_ms
        prev_all_blocked = all_blk
    return skipped


def evaluate_cohort_gate(
    cycles_by_trader: dict[str, list[dict[str, Any]]],
    trades_df: pd.DataFrame,
    btc_close_times: list[int],
    btc_feats: list[dict[str, float]],
    *,
    mode: str,
    planned_k: int = PLANNED_K,
    placebo_n: int = PLACEBO_N,
    seed: int = SEED,
    unit_cost: float = UNIT_AI_COST_USD,
) -> dict[str, Any]:
    """Run causal gate G1 or G2 across cohort, compute daily true-net delta, weekly SE, and placebo."""
    skip_map: dict[int, bool] = {}
    skipped_counts_by_trader: dict[str, tuple[int, int]] = {}
    daily_ai_saved: dict[str, float] = {}
    total_cycles = 0
    total_skipped_cycles = 0

    for tid, cy_list in cycles_by_trader.items():
        mask = apply_gate_rules(cy_list, btc_close_times, btc_feats, mode=mode)
        n_tr = len(cy_list)
        n_sk = sum(mask)
        skipped_counts_by_trader[tid] = (n_sk, n_tr)
        total_cycles += n_tr
        total_skipped_cycles += n_sk
        for cy, sk in zip(cy_list, mask):
            skip_map[int(cy["id"])] = sk
            day = cy["utc_day"]
            if day not in daily_ai_saved:
                daily_ai_saved[day] = 0.0
            if sk:
                daily_ai_saved[day] += unit_cost

    n_trades = int(len(trades_df))
    matched_mask = trades_df["dr_id"].notna()
    missing_trades = int((~matched_mask).sum())
    coverage_error = (missing_trades / n_trades) if n_trades else 1.0

    if coverage_error > 0.05 or total_cycles == 0:
        return {
            "mode": mode,
            "verdict": "invalid",
            "n_trades": n_trades,
            "missing_trades": missing_trades,
            "coverage_error": round(coverage_error, 4),
        }

    # Map matched trades onto cycle skip decision
    dropped_trade_flags = []
    daily_trade_delta: dict[str, float] = {d: 0.0 for d in daily_ai_saved}
    trade_filter_mask: dict[str, bool] = {}
    filter_trade_rows: list[dict[str, Any]] = []

    for row in trades_df.itertuples(index=False):
        dr_id = int(row.dr_id) if pd.notna(row.dr_id) else None
        is_drop = bool(skip_map.get(dr_id, False)) if dr_id is not None else False
        dropped_trade_flags.append(is_drop)
        day = str(row.cycle_utc_day) if pd.notna(row.cycle_utc_day) else str(row.entry_utc_day)
        daily_trade_delta.setdefault(day, 0.0)
        if is_drop:
            # Dropping trade removes its net_pnl -> delta_trade = -net_pnl
            daily_trade_delta[day] -= float(row.net_pnl)
        if pd.notna(row.r_mult):
            trade_filter_mask[str(row.id)] = is_drop
            filter_trade_rows.append(
                {
                    "trade_id": str(row.id),
                    "entry_time": row.entry_iso,
                    "r": float(row.r_mult),
                    "net_pnl_usdt": float(row.net_pnl),
                }
            )

    all_days = sorted(daily_ai_saved.keys() | daily_trade_delta.keys())
    daily_deltas = [
        (day, daily_ai_saved.get(day, 0.0) + daily_trade_delta.get(day, 0.0))
        for day in all_days
    ]
    n_days = len(daily_deltas)
    half = n_days // 2
    dev_delta = float(np.mean([v for _, v in daily_deltas[:half]])) if half > 0 else None
    ho_delta = float(np.mean([v for _, v in daily_deltas[half:]])) if n_days - half > 0 else None

    weekly_samples = [
        (dt.datetime.strptime(day, "%Y-%m-%d").replace(tzinfo=dt.timezone.utc), float(v))
        for day, v in daily_deltas
    ]
    ws = incremental_filter.weekly_statistics(weekly_samples)
    avg_d, se_d = ws["avg_r"], ws["se"]
    z_stat = (avg_d / se_d) if (avg_d is not None and se_d is not None and se_d > 0) else 0.0
    z_k = float(incremental_filter.z_threshold(planned_k))

    # Observed totals
    total_ai_saved = sum(daily_ai_saved.values())
    total_trade_delta = sum(daily_trade_delta.values())
    obs_total_true_delta = total_ai_saved + total_trade_delta
    obs_mean_daily_delta = obs_total_true_delta / max(1, n_days)

    base_trade_net = float(trades_df["net_pnl"].sum())
    base_ai_cost = total_cycles * unit_cost
    base_true_net = base_trade_net - base_ai_cost
    cand_true_net = base_true_net + obs_total_true_delta

    # 2000x matched cycle-skip placebo (same skip count per trader across real LLM cycles)
    rng = np.random.default_rng(seed)
    # Pre-index per-trader cycle trade net_pnl for ultra-fast vectorized placebo draws
    cycle_trade_pnl: dict[int, float] = {}
    for row in trades_df.itertuples(index=False):
        if pd.notna(row.dr_id):
            cid = int(row.dr_id)
            cycle_trade_pnl[cid] = cycle_trade_pnl.get(cid, 0.0) + float(row.net_pnl)

    trader_arrays = []
    for tid, cy_list in cycles_by_trader.items():
        n_sk, n_tr = skipped_counts_by_trader[tid]
        pnls = np.array([cycle_trade_pnl.get(int(cy["id"]), 0.0) for cy in cy_list], dtype=float)
        trader_arrays.append((n_sk, n_tr, pnls))

    placebo_totals = np.empty(placebo_n, dtype=float)
    for idx_p in range(placebo_n):
        tot = 0.0
        for n_sk, n_tr, pnls in trader_arrays:
            if n_sk <= 0:
                continue
            if n_sk >= n_tr:
                tot += n_tr * unit_cost - float(pnls.sum())
            else:
                pick = rng.choice(n_tr, size=n_sk, replace=False)
                tot += n_sk * unit_cost - float(pnls[pick].sum())
        placebo_totals[idx_p] = tot
    placebo_mean_daily = placebo_totals / max(1, n_days)
    placebo_pct = float(np.mean(placebo_mean_daily < obs_mean_daily_delta))

    # Pure trade-filter metrics (R side)
    dropped_trades = sum(dropped_trade_flags)
    p_trades = dropped_trades / max(1, n_trades)
    if filter_trade_rows and 0 < sum(trade_filter_mask.values()) < len(filter_trade_rows):
        trade_r_stats = incremental_filter.filter_statistics(
            filter_trade_rows, trade_filter_mask, seed=seed, placebo_n=min(placebo_n, 2000)
        )
    else:
        trade_r_stats = {"n": len(filter_trade_rows), "dropped_n": sum(trade_filter_mask.values()), "delta": 0.0}

    skip_rate = total_skipped_cycles / max(1, total_cycles)
    if skip_rate <= 0.0 or skip_rate >= 1.0:
        verdict = "invalid"
    elif p_trades > 0.50 or placebo_pct < 0.90:
        verdict = "fewer_trades"
    elif z_stat < 1.0 or (dev_delta is None or dev_delta <= 0.0) or (ho_delta is None or ho_delta <= 0.0):
        verdict = "fail"
    elif z_stat >= z_k:
        verdict = "strong"
    else:
        verdict = "screen_pass"

    return {
        "mode": mode,
        "verdict": verdict,
        "planned_K": planned_k,
        "z_K": round(z_k, 4),
        "n_cycles_real_llm": total_cycles,
        "n_cycles_skipped": total_skipped_cycles,
        "cycle_skip_rate": round(skip_rate, 4),
        "n_trades": n_trades,
        "missing_trades": missing_trades,
        "coverage_error": round(coverage_error, 4),
        "n_trades_dropped": dropped_trades,
        "p_trades_dropped": round(p_trades, 4),
        "n_days": n_days,
        "n_weeks": int(ws["n_weeks"]),
        "baseline_trade_net_usd": round(base_trade_net, 2),
        "baseline_ai_cost_usd": round(base_ai_cost, 2),
        "baseline_true_net_usd": round(base_true_net, 2),
        "candidate_true_net_usd": round(cand_true_net, 2),
        "ai_cost_saved_usd": round(total_ai_saved, 2),
        "dropped_trades_net_impact_usd": round(total_trade_delta, 2),
        "total_true_net_delta_usd": round(obs_total_true_delta, 2),
        "mean_daily_true_net_delta_usd": round(obs_mean_daily_delta, 4),
        "se_weekly_daily_usd": round(float(se_d), 4) if se_d is not None else None,
        "z": round(float(z_stat), 4),
        "dev_delta_daily_usd": round(dev_delta, 4) if dev_delta is not None else None,
        "holdout_delta_daily_usd": round(ho_delta, 4) if ho_delta is not None else None,
        "placebo_n": placebo_n,
        "placebo_mean_daily_usd": round(float(placebo_mean_daily.mean()), 4),
        "placebo_pct": round(placebo_pct, 4),
        "trade_r_side": {
            "n_r": trade_r_stats.get("n"),
            "dropped_r": trade_r_stats.get("dropped_n"),
            "delta_avg_r": round(float(trade_r_stats["delta"]), 5) if trade_r_stats.get("delta") is not None else None,
            "z_r": round(float(trade_r_stats["z"]), 3) if trade_r_stats.get("z") is not None else None,
        },
    }


def _load_cohort_from_db(
    conn: sqlite3.Connection,
    trader_names: list[str],
    *,
    min_ts_iso: str | None = None,
) -> tuple[dict[str, list[dict[str, Any]]], pd.DataFrame, dict[str, Any]]:
    name_map = dict(conn.execute("SELECT name, id FROM traders").fetchall())
    cycles_by_trader: dict[str, list[dict[str, Any]]] = {}
    oid_to_cycle: dict[tuple[str, str], dict[str, Any]] = {}
    trade_rows: list[dict[str, Any]] = []
    ai_close_events: list[dict[str, Any]] = []

    for tname in trader_names:
        tid = name_map.get(tname)
        if not tid:
            continue
        clauses = ["trader_id = ?", "ai_request_duration_ms > 0"]
        params: list[Any] = [tid]
        if min_ts_iso:
            clauses.append("timestamp >= ?")
            params.append(min_ts_iso)
        dr_rows = conn.execute(
            f"SELECT id, timestamp, candidate_verdicts, decisions FROM decision_records WHERE {' AND '.join(clauses)} ORDER BY id ASC",
            params,
        ).fetchall()

        cy_list: list[dict[str, Any]] = []
        for rid, ts_raw, cv_raw, dec_raw in dr_rows:
            ts_utc = promo_report.as_utc(str(ts_raw))
            ts_ms = int(ts_utc.timestamp() * 1000)
            utc_day = ts_utc.strftime("%Y-%m-%d")
            all_blk = is_all_candidates_blocked(cv_raw)
            open_oids, has_close, closes = parse_decisions_meta(dec_raw)
            cy_rec = {
                "id": int(rid),
                "trader_id": tid,
                "trader_name": tname,
                "ts_ms": ts_ms,
                "utc_day": utc_day,
                "all_blocked": all_blk,
                "has_close": has_close,
            }
            cy_list.append(cy_rec)
            for oid in open_oids:
                oid_to_cycle[(tid, oid)] = cy_rec
            if closes:
                for cl in closes:
                    ai_close_events.append({**cy_rec, **cl})
        cycles_by_trader[tid] = cy_list

        pos_rows = conn.execute(
            """
            SELECT id, symbol, side, entry_price, exit_price, initial_stop_loss,
                   quantity, realized_pnl, fee, entry_time, exit_time, close_reason, entry_order_id
            FROM trader_positions
            WHERE trader_id = ? AND LOWER(status) = 'closed'
            """,
            (tid,),
        ).fetchall()
        for pid, sym, side, ep, xp, isl, qty, pnl, fee, etime, xtime, reason, eoid in pos_rows:
            et_ts = parse_utc_timestamps(pd.Series([etime])).iloc[0]
            if pd.isna(et_ts):
                continue
            if min_ts_iso:
                cut_ts = pd.Timestamp(promo_report.as_utc(min_ts_iso))
                if et_ts < cut_ts:
                    continue
            pnl_f = float(pnl or 0.0)
            fee_f = float(fee or 0.0)
            net_pnl = pnl_f - fee_f
            r_mult = math.nan
            if ep and isl and qty and ep > 0 and isl > 0 and qty > 0:
                dist = abs(float(ep) - float(isl))
                if dist > 1e-12:
                    r_mult = net_pnl / (dist * float(qty))
            cy_match = oid_to_cycle.get((tid, str(eoid or "")))
            trade_rows.append(
                {
                    "id": pid,
                    "trader_id": tid,
                    "trader_name": tname,
                    "symbol": sym,
                    "side": side,
                    "entry_iso": et_ts.isoformat().replace("+00:00", "Z"),
                    "entry_utc_day": et_ts.strftime("%Y-%m-%d"),
                    "net_pnl": net_pnl,
                    "r_mult": r_mult,
                    "close_reason": reason,
                    "entry_order_id": eoid,
                    "dr_id": cy_match["id"] if cy_match else np.nan,
                    "cycle_utc_day": cy_match["utc_day"] if cy_match else None,
                }
            )

    trades_df = pd.DataFrame(trade_rows)
    return cycles_by_trader, trades_df, {"ai_close_events": ai_close_events}


def run_study(*, db_path: Path = DEFAULT_DB, write_ledger: bool = False) -> dict[str, Any]:
    verify_prereg()

    # Fetch causal BTCUSDT 1h bars from NAS MariaDB
    cfg = nas_klines.load_config(timeout=30.0)
    with nas_klines.ReadOnlyMysqlClient(cfg) as cli:
        btc_1h = nas_klines.fetch_crypto_bars("BTCUSDT", interval="1h", start="2026-08-28", end="2026-11-20", fmt="dict", client=cli)
    btc_close_times, btc_feats = build_causal_1h_btc_index(btc_1h)

    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        # 1. Primary Cohort: pooled-6-paper
        cy_p6, df_p6, meta_p6 = _load_cohort_from_db(conn, POOLED_6_NAMES)
        g1_p6 = evaluate_cohort_gate(cy_p6, df_p6, btc_close_times, btc_feats, mode="G1", planned_k=11)
        g2_p6 = evaluate_cohort_gate(cy_p6, df_p6, btc_close_times, btc_feats, mode="G2", planned_k=11)

        # 2. Standalone Audit on mac-nofx (phantom ai_charges + post-2026-10-10 cycle savings & matched subset)
        mac_tid = conn.execute("SELECT id FROM traders WHERE name = ?", (MAC_NOFX_NAME,)).fetchone()[0]
        mac_cycle_audit = conn.execute(
            """
            SELECT COUNT(*),
                   SUM(CASE WHEN ai_request_duration_ms = 0 OR ai_request_duration_ms IS NULL THEN 1 ELSE 0 END),
                   SUM(CASE WHEN ai_request_duration_ms > 0 THEN 1 ELSE 0 END),
                   SUM(CASE WHEN decisions LIKE '%Regime skip%' THEN 1 ELSE 0 END)
            FROM decision_records WHERE trader_id = ?
            """,
            (mac_tid,),
        ).fetchone()
        mac_charges = conn.execute("SELECT COUNT(*), ROUND(SUM(cost_usd), 4) FROM ai_charges WHERE trader_id = ?", (mac_tid,)).fetchone()
        cy_mac, df_mac, _ = _load_cohort_from_db(conn, [MAC_NOFX_NAME], min_ts_iso="2026-10-10 09:31:46+00:00")
        mac_matched_df = df_mac[df_mac["dr_id"].notna()].copy() if not df_mac.empty else df_mac
        mac_g1_cycles = sum(sum(apply_gate_rules(v, btc_close_times, btc_feats, mode="G1")) for v in cy_mac.values())
        mac_g2_cycles = sum(sum(apply_gate_rules(v, btc_close_times, btc_feats, mode="G2")) for v in cy_mac.values())
        mac_real_post = sum(len(v) for v in cy_mac.values())
        # Count matched trades dropped on mac-nofx post Oct 10
        mac_skip_g1 = {int(cy["id"]): sk for cy_list in cy_mac.values() for cy, sk in zip(cy_list, apply_gate_rules(cy_list, btc_close_times, btc_feats, mode="G1"))}
        mac_skip_g2 = {int(cy["id"]): sk for cy_list in cy_mac.values() for cy, sk in zip(cy_list, apply_gate_rules(cy_list, btc_close_times, btc_feats, mode="G2"))}
        mac_drop_g1 = [r for r in mac_matched_df.itertuples() if mac_skip_g1.get(int(r.dr_id), False)]
        mac_drop_g2 = [r for r in mac_matched_df.itertuples() if mac_skip_g2.get(int(r.dr_id), False)]
    finally:
        conn.close()

    # Check ai_close sensitivity on pooled-6-paper
    ai_close_audit = {}
    for mode in ("G1", "G2"):
        skipped_ids = {
            int(cy["id"])
            for cy_list in cy_p6.values()
            for cy, sk in zip(cy_list, apply_gate_rules(cy_list, btc_close_times, btc_feats, mode=mode))
            if sk
        }
        affected_closes = [ev for ev in meta_p6["ai_close_events"] if int(ev["id"]) in skipped_ids]
        ai_close_audit[mode] = {
            "total_ai_close_cycles": len(meta_p6["ai_close_events"]),
            "skipped_ai_close_cycles": len(affected_closes),
        }

    out = {
        "prereg_sha256": PREREG_SHA256,
        "nas_btc_1h_bars": len(btc_1h),
        "primary_pooled_6_paper": {
            "G1_all_blocked_skip": g1_p6,
            "G2_quiet_15m_skip": g2_p6,
            "ai_close_cycle_sensitivity": ai_close_audit,
        },
        "mac_nofx_audit": {
            "total_cycles": int(mac_cycle_audit[0]),
            "zero_duration_cycles_no_llm": int(mac_cycle_audit[1]),
            "real_llm_cycles": int(mac_cycle_audit[2]),
            "regime_skip_cycles": int(mac_cycle_audit[3]),
            "recorded_ai_charges_count": int(mac_charges[0]),
            "recorded_ai_charges_usd": float(mac_charges[1] or 0.0),
            "phantom_overbilled_cycles_approx": int(mac_charges[0]) - int(mac_cycle_audit[2]),
            "post_oct10_real_llm_cycles": mac_real_post,
            "post_oct10_g1_skipped_cycles": mac_g1_cycles,
            "post_oct10_g2_skipped_cycles": mac_g2_cycles,
            "post_oct10_oid_matched_trades": int(len(mac_matched_df)),
            "post_oct10_g1_dropped_matched_trades": len(mac_drop_g1),
            "post_oct10_g1_dropped_net_usd": round(sum(float(r.net_pnl) for r in mac_drop_g1), 2),
            "post_oct10_g2_dropped_matched_trades": len(mac_drop_g2),
            "post_oct10_g2_dropped_net_usd": round(sum(float(r.net_pnl) for r in mac_drop_g2), 2),
        },
    }

    if write_ledger:
        for k_num, idea_id, res in (
            (10, "crypto-llm-all-blocked-skip-v1", g1_p6),
            (11, "crypto-llm-quiet-cadence-15m-skip-v1", g2_p6),
        ):
            def _build_row(k, _prev, _k_num=k_num, _idea=idea_id, _res=res):
                return {
                    "stage": "S1",
                    "change_type": "B",
                    "idea_id": _idea,
                    "baseline_version": "crypto_llm_pooled6_unthrottled_v1",
                    "verdict": _res["verdict"],
                    "preregistered_K": PLANNED_K,
                    "planned_arm_K": _k_num,
                    "actual_ledger_K": k,
                    "delta": _res.get("mean_daily_true_net_delta_usd"),
                    "se": _res.get("se_weekly_daily_usd"),
                    "z": _res.get("z"),
                    "z_threshold": _res.get("z_K"),
                    "dev_delta": _res.get("dev_delta_daily_usd"),
                    "holdout_delta": _res.get("holdout_delta_daily_usd"),
                    "placebo_pct": _res.get("placebo_pct"),
                    "cycle_skip_rate": _res.get("cycle_skip_rate"),
                    "p_trades_dropped": _res.get("p_trades_dropped"),
                    "prereg_path": str(PREREG.relative_to(REPO)),
                    "prereg_sha256": PREREG_SHA256,
                }

            evolution_ledger.append_event(LEDGER, "crypto_llm", _build_row)
    return out


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", type=Path, default=DEFAULT_DB)
    ap.add_argument("--write-ledger", action="store_true")
    ap.add_argument("--out", type=Path, default=None)
    args = ap.parse_args(argv)
    res = run_study(db_path=args.db, write_ledger=args.write_ledger)
    txt = json.dumps(res, indent=2, ensure_ascii=False)
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(txt + "\n", encoding="utf-8")
    print(txt)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
