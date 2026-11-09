"""Compare paper traders without changing the database or trader configuration.

Run from the repository root: python -m scripts.promotion.report
All calculations use the standard library; calculation functions do no I/O.
"""

from __future__ import annotations

import argparse
from collections import defaultdict
from contextlib import closing, ExitStack
from datetime import datetime, timedelta, timezone
import json
import math
import os
from pathlib import Path
import sqlite3
import sys
import tempfile

try:  # The nightly runtime dir only gets quant_audit.py via deploy.sh; never let the audit break the report.
    from scripts.backtest.quant_audit import audit_returns
except ImportError:
    audit_returns = None
from scripts.backtest import evolution_ledger
from scripts.promotion.create_pair import main_repo_root, verify_prereg


UTC = timezone.utc
REPO_ROOT = Path(__file__).resolve().parents[2]
CAVEAT = (
    "ready 要求至少 30 个配对日、两侧各 50 笔平仓，且 USDT/日配对差 CI95 下界通过非劣效；"
    "非劣效不等于已证明更优；"
    "上线前仍需人工确认并保持小仓位/可回滚"
)
PNL_BASIS = "net_pnl_usdt = realized_pnl - fee（realized_pnl 为未扣手续费盈亏）"
AUDIT_MIN_WINDOW_PERIODS = 20
AUDIT_BENCHMARK = "BTCUSDT 买入持有（UTC 日收盘，对数收益）"
MIN_PAIRED_DAYS = 30
MIN_CLOSED_TRADES = 50
# One cent per day limits the tolerated expected paper-account loss to 0.30
# USDT over 30 days. Never compare this absolute amount to return ratios.
NONINFERIORITY_MARGIN = 0.01  # USDT/day


def as_utc(value: str | datetime) -> datetime:
    """Parse GORM/ISO timestamps; legacy naive database times mean UTC."""
    parsed = value if isinstance(value, datetime) else datetime.fromisoformat(value.replace("Z", "+00:00"))
    return parsed.replace(tzinfo=UTC) if parsed.tzinfo is None else parsed.astimezone(UTC)


def iso(value: datetime) -> str:
    return as_utc(value).isoformat().replace("+00:00", "Z")


def midnight(value: datetime) -> datetime:
    return as_utc(value).replace(hour=0, minute=0, second=0, microsecond=0)


def finite(value) -> float | None:
    try:
        result = float(value)
    except (ValueError, TypeError, OverflowError):
        return None
    return result if math.isfinite(result) else None


def finite_sum(values) -> float | None:
    values = list(values)
    if any(value is None for value in values):
        return None
    try:
        return finite(math.fsum(values))
    except (ValueError, OverflowError):
        return None


def mean(values) -> float | None:
    values = list(values)
    total = finite_sum(values)
    return finite(total / len(values)) if values and total is not None else None


def json_safe(value):
    """Recursively replace non-finite floats, including configuration metadata."""
    if isinstance(value, float):
        return finite(value)
    if isinstance(value, dict):
        return {key: json_safe(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(item) for item in value]
    return value


def trade_statistics(trades: list[dict], *, gross_r: bool = False) -> tuple[dict, list[tuple[datetime, float]]]:
    """Closed rows, already restricted by exit_time; R uses quantity, not entry_quantity.

    gross_r (stock_paper_positions) divides the raw realized_pnl by the risk;
    the default nets the fee first, as trader_positions always did.
    """
    nets, fees, gross, r_samples, extremes = [], [], [], [], []
    excluded = defaultdict(int)
    for row in trades:
        pnl, fee = finite(row.get("realized_pnl")), finite(row.get("fee"))
        net = finite(pnl - fee) if pnl is not None and fee is not None else None
        gross.append(pnl)
        fees.append(fee)
        nets.append(net)
        entry = finite(row.get("entry_price"))
        stop = finite(row.get("initial_stop_loss"))
        quantity = finite(row.get("quantity"))
        if entry is None or entry <= 0 or stop is None or stop <= 0:
            excluded["missing_or_invalid_price_stop"] += 1
            continue
        distance = abs(entry - stop)
        if distance <= max(1e-12, abs(entry) * 1e-9):
            excluded["near_zero_stop_distance"] += 1
            continue
        if quantity is None or quantity <= 0:
            excluded["missing_or_invalid_quantity"] += 1
            continue
        risk = finite(distance * quantity)
        basis = pnl if gross_r else net
        r = finite(basis / risk) if basis is not None and risk is not None and risk > 0 else None
        if r is None:
            excluded["nonfinite_pnl_or_risk"] += 1
            continue
        exited = datetime.fromtimestamp(row["exit_time"] / 1000, UTC)
        r_samples.append((exited, r))
        if abs(r) > 15:
            extremes.append({"position_id": row["id"], "symbol": row.get("symbol"),
                             "exit_time": iso(exited), "r": r, "net_pnl_usdt": net,
                             "initial_risk_usdt": risk})
    n = len(trades)
    invalid = sum(value is None for value in nets)
    return {
        "n": n, "net_pnl_usdt": finite_sum(nets), "gross_pnl_usdt": finite_sum(gross),
        "fees_usdt": finite_sum(fees), "pnl_basis": PNL_BASIS,
        "pnl_invalid": invalid,
        "win_rate": sum(value > 0 for value in nets if value is not None) / n if n and not invalid else None,
        "avg_r": mean(value for _, value in r_samples), "r_n": len(r_samples),
        "r_excluded": n - len(r_samples), "r_excluded_reasons": dict(excluded),
        "r_extreme_count": len(extremes), "r_extremes": extremes,
    }, r_samples


def equity_statistics(snapshots: list[dict], started_at: datetime, as_of: datetime) -> dict:
    """Last snapshot per UTC day; forward-fill only between observed endpoints.

    No backfill, and no extension after the last snapshot. Period return uses
    the first actual snapshot, while MaxDD uses the daily close equity curve.
    """
    valid, invalid = [], 0
    for row in snapshots:
        try:
            timestamp = as_utc(row["timestamp"])
        except (ValueError, TypeError, AttributeError):
            invalid += 1
            continue
        if not started_at <= timestamp <= as_of:
            continue
        equity = finite(row["total_equity"])
        if equity is None:
            invalid += 1
        else:
            valid.append((timestamp, row.get("id", 0), equity))
    valid.sort(key=lambda row: (row[0], row[1]))
    closes = {midnight(timestamp): (timestamp, equity) for timestamp, _, equity in valid}
    curve, previous, peak, drawdowns = [], None, None, []
    filled = 0
    if closes:
        day, end = min(closes), max(closes)
        last = None
        while day <= end:
            observed = day in closes
            last = closes.get(day, last)
            timestamp, equity = last
            daily_return = finite(equity / previous - 1) if previous is not None and previous > 0 else None
            peak = equity if peak is None else max(peak, equity)
            if peak > 0:
                drawdowns.append(finite((peak - equity) / peak))
            curve.append({"date": iso(day), "equity": equity, "return": daily_return,
                          "snapshot_at": iso(timestamp), "forward_filled": not observed})
            filled += not observed
            previous = equity
            day += timedelta(days=1)
    first, last = (valid[0], valid[-1]) if valid else (None, None)
    return {
        "snapshot_count": len(valid), "snapshot_invalid": invalid,
        "first_snapshot_at": iso(first[0]) if first else None,
        "last_snapshot_at": iso(last[0]) if last else None,
        "first_equity_usdt": first[2] if first else None,
        "last_equity_usdt": last[2] if last else None,
        "period_return": finite(last[2] / first[2] - 1) if first and first[2] > 0 else None,
        "max_drawdown": max((d for d in drawdowns if d is not None), default=None),
        "forward_filled_days": filled, "daily_equity": curve,
    }


def completed_log_returns(stats: dict, as_of: datetime) -> dict[str, float]:
    """Use existing daily returns, converting arithmetic to log only for completed UTC days."""
    result = {}
    for row in stats["daily_equity"]:
        value = finite(row["return"])
        if value is not None and value > -1 and as_utc(row["date"]) < midnight(as_of):
            result[row["date"]] = math.log1p(value)
    return result


def load_btc_returns(as_of: datetime) -> tuple[dict[str, float], str | None]:
    """Read the local factor cache; any missing/corrupt benchmark remains advisory only."""
    path = "BTCUSDT.json"
    try:
        override = os.environ.get("FACTOR_MINING_DIR")
        directory = Path(override) if override is not None else main_repo_root() / "data/factor_mining"
        path = directory / "crypto/daily/BTCUSDT.json"
        rows = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(rows, list) or not rows:
            raise ValueError("BTC 日线必须是非空数组")
        closes = {}
        for row in rows:
            if not isinstance(row, list) or len(row) < 5 or not isinstance(row[0], str):
                raise ValueError("BTC 日线行必须包含 [date, open, high, low, close, ...]")
            day = as_utc(row[0])
            if row[0] != day.date().isoformat():
                raise ValueError("BTC 日期必须为 YYYY-MM-DD")
            close = finite(row[4])
            if close is None or close <= 0 or day in closes:
                raise ValueError("BTC 收盘价无效或日期重复")
            closes[day] = close
        days = sorted(day for day in closes if day < midnight(as_of))
        returns = {iso(day): math.log(closes[day]) - math.log(closes[previous])
                   for previous, day in zip(days, days[1:]) if day - previous == timedelta(days=1)}
        if not returns:
            raise ValueError("BTC 没有相邻的已完成 UTC 日收益")
        return returns, None
    except (OSError, ValueError, TypeError, OverflowError) as error:
        return {}, f"BTC 基准不可用（{path}）：{error}"


def pair_audit(candidate: dict, control: dict, as_of: datetime,
               btc_returns: dict[str, float], btc_error: str | None) -> dict:
    """Append-only reference audit, independent of promotion verdict/reasons."""
    cand = completed_log_returns(candidate, as_of)
    ctrl = completed_log_returns(control, as_of)

    def aligned(system, benchmark):
        if audit_returns is None:
            return {"verdict": "UNAVAILABLE", "reason": "quant_audit 模块不可用（运行目录未同步 scripts/backtest/quant_audit.py）"}
        days = sorted(system.keys() & benchmark.keys())
        result = audit_returns([system[day] for day in days], [benchmark[day] for day in days],
                               365, min_window_periods=AUDIT_MIN_WINDOW_PERIODS)
        return {**result, "n_days": len(days)}

    def vs_btc(system):
        if btc_error or not system.keys() & btc_returns.keys():
            return {"verdict": "UNAVAILABLE", "reason": btc_error or "与 BTC 基准没有公共的已完成 UTC 收益日期"}
        if audit_returns is None:
            return aligned(system, btc_returns)
        return aligned(system, btc_returns)

    cand_btc, ctrl_btc = vs_btc(cand), vs_btc(ctrl)
    return {
        "note": "信号层收益审计：仅供参考，不改变 verdict；样本不足时只给出样本数",
        "min_window_periods": AUDIT_MIN_WINDOW_PERIODS,
        "benchmark": AUDIT_BENCHMARK if any(result["verdict"] != "UNAVAILABLE"
                                            for result in (cand_btc, ctrl_btc)) else None,
        "candidate_vs_benchmark": cand_btc,
        "control_vs_benchmark": ctrl_btc,
        "candidate_vs_control": aligned(cand, ctrl),
    }


def cluster_se(influences: list[float]) -> float | None:
    """Finite-cluster corrected SE: sqrt(G/(G-1) * sum(influence_g**2))."""
    if len(influences) < 2:
        return None
    # hypot avoids overflow from directly squaring large, finite influences.
    return finite(math.hypot(*influences) * math.sqrt(len(influences) / (len(influences) - 1)))


def ci95(estimate: float | None, se: float | None):
    return [finite(estimate - 1.96 * se), finite(estimate + 1.96 * se)] if estimate is not None and se is not None else None


def paired_daily_comparison(candidate: dict, control: dict, as_of: datetime) -> dict:
    """One difference per completed UTC day: days, rather than snapshots, are clusters."""
    def returns(stats):
        return {row["date"]: row["return"] for row in stats["daily_equity"]
                if row["return"] is not None and as_utc(row["date"]) < midnight(as_of)}

    cand, ctrl = returns(candidate), returns(control)
    sequence = [{"date": day, "candidate_return": cand[day], "control_return": ctrl[day],
                 "diff": finite(cand[day] - ctrl[day])} for day in sorted(cand.keys() & ctrl.keys())]
    sequence = [row for row in sequence if row["diff"] is not None]
    diffs = [row["diff"] for row in sequence]
    estimate, n = mean(diffs), len(diffs)
    se = cluster_se([(value - estimate) / n for value in diffs]) if n >= 10 and estimate is not None else None
    def daily_pnl(stats):
        curve = stats["daily_equity"]
        return {row["date"]: finite(row["equity"] - previous["equity"])
                for previous, row in zip(curve, curve[1:])
                if as_utc(row["date"]) - as_utc(previous["date"]) == timedelta(days=1)}

    cand_pnl, ctrl_pnl = daily_pnl(candidate), daily_pnl(control)
    pnl_diffs = [finite(cand_pnl[row["date"]] - ctrl_pnl[row["date"]]) for row in sequence
                 if cand_pnl.get(row["date"]) is not None and ctrl_pnl.get(row["date"]) is not None]
    pnl_diffs = [value for value in pnl_diffs if value is not None]
    pnl_mean, pnl_n = mean(pnl_diffs), len(pnl_diffs)
    pnl_se = cluster_se([(value - pnl_mean) / pnl_n for value in pnl_diffs]) if pnl_n >= 10 else None
    c_return, b_return = candidate["period_return"], control["period_return"]
    aligned_c = finite(math.prod(1 + row["candidate_return"] for row in sequence) - 1) if n else None
    aligned_b = finite(math.prod(1 + row["control_return"] for row in sequence) - 1) if n else None
    return {"n_days": n, "mean_diff": estimate, "cluster_se": se, "ci95": ci95(estimate, se),
            "pnl_usdt": {"n_days": pnl_n, "mean_diff": pnl_mean, "cluster_se": pnl_se,
                         "ci95": ci95(pnl_mean, pnl_se), "unit": "USDT/day",
                         "noninferiority_margin": NONINFERIORITY_MARGIN},
            "cumulative_return_diff": finite(c_return - b_return) if c_return is not None and b_return is not None else None,
            "aligned_candidate_return": aligned_c, "aligned_control_return": aligned_b,
            "aligned_cumulative_return_diff": finite(aligned_c - aligned_b) if aligned_c is not None and aligned_b is not None else None,
            "ci_note": None if n >= 10 else "完整配对日不足 10 日，仅报告均值，不报告标准误或 CI",
            "daily_diffs": sequence}


def weekly_r_comparison(candidate: list[tuple[datetime, float]],
                        control: list[tuple[datetime, float]], min_trades: int) -> dict:
    """Trade-weighted avgR difference; joint cluster by UTC Monday exit week.

    For each week, influence = sum_c(R-avg_c)/N_c - sum_b(R-avg_b)/N_b.
    Weeks with trades on only one side still contribute to the joint variance.
    """
    result = {"candidate_r_n": len(candidate), "control_r_n": len(control),
              "n_weeks": 0, "mean_diff": None, "cluster_se": None, "ci95": None,
              "weeks": [], "note": None}
    if len(candidate) < min_trades or len(control) < min_trades:
        result["note"] = f"两侧有效 R 成交均需至少 {min_trades} 笔，当前不足"
        return result
    avg_c, avg_b = mean(r for _, r in candidate), mean(r for _, r in control)
    if avg_c is None or avg_b is None:
        result["note"] = "有效 R 均值不可计算"
        return result
    buckets = defaultdict(lambda: [[], []])
    for side, samples in enumerate((candidate, control)):
        for timestamp, r in samples:
            day = midnight(timestamp)
            buckets[day - timedelta(days=day.weekday())][side].append(r)
    influences = []
    for week, (cand, ctrl) in sorted(buckets.items()):
        influence = finite_sum([(r - avg_c) / len(candidate) for r in cand] +
                               [-(r - avg_b) / len(control) for r in ctrl])
        influences.append(influence)
        result["weeks"].append({"week_start": iso(week), "candidate_n": len(cand),
                                "control_n": len(ctrl), "candidate_avg_r": mean(cand),
                                "control_avg_r": mean(ctrl), "influence": influence})
    n = len(buckets)
    estimate = finite(avg_c - avg_b)
    se = cluster_se(influences) if n >= 3 and all(i is not None for i in influences) else None
    result.update(n_weeks=n, mean_diff=estimate, cluster_se=se, ci95=ci95(estimate, se),
                  note=None if n >= 3 else "退出周不足 3 周，仅报告 avgR 差，不报告标准误或 CI")
    return result


def decide_verdict(elapsed_days: float, min_days: float, min_trades: int,
                   candidate: dict, control: dict, paired: dict) -> tuple[str, list[str]]:
    insufficient, performance = [], []
    if elapsed_days < min_days:
        insufficient.append(f"已运行 {elapsed_days:.2f} 天，少于要求的 {min_days:g} 天")
    required_trades = max(min_trades, MIN_CLOSED_TRADES)
    for title, stats in (("候选", candidate), ("对照", control)):
        if stats["n"] < required_trades:
            insufficient.append(f"{title}已平仓 {stats['n']} 笔，少于要求的 {required_trades} 笔")
    pnl_paired = paired.get("pnl_usdt", {})
    if min(paired.get("n_days", 0), pnl_paired.get("n_days", 0)) < MIN_PAIRED_DAYS:
        insufficient.append(f"完整 USDT 配对日不足 {MIN_PAIRED_DAYS} 日")
    interval = pnl_paired.get("ci95")
    lower = finite(interval[0]) if interval and len(interval) == 2 else None
    if lower is None or lower <= -NONINFERIORITY_MARGIN:
        performance.append(f"USDT/日配对差 CI95 下界不可计算或不大于 {-NONINFERIORITY_MARGIN:g}，未通过非劣效")
    pnl = finite(candidate["net_pnl_usdt"])
    if pnl is None or pnl <= 0:
        performance.append("候选净盈亏不可计算或不大于 0 USDT")
    c_return, b_return = finite(candidate["period_return"]), finite(control["period_return"])
    if c_return is None or b_return is None:
        performance.append("候选或对照缺少有效权益快照，期间收益率无法比较")
    elif c_return < b_return:
        performance.append("候选期间收益率低于对照")
    diff = finite(paired["mean_diff"])
    if diff is None or diff < 0:
        performance.append("完整日配对收益差均值不可计算或小于 0")
    verdict = "insufficient" if insufficient else "hold" if performance else "ready"
    return verdict, insufficient + performance


def decide_incremental(elapsed: float, prereg: dict, candidate: dict, control: dict,
                       weekly: dict) -> tuple[str, list[str]]:
    delta, se = finite(weekly["mean_diff"]), finite(weekly["cluster_se"])
    lower = finite(delta - 1.645 * se) if delta is not None and se is not None else None
    ratio = candidate["n"] / control["n"] if control["n"] else None
    if elapsed < prereg["harm_check_days"]:
        return "insufficient", [f"尚未达到伤害检查期 {prereg['harm_check_days']:g} 天"]
    if delta is not None and se is not None and delta < -2 * se:
        return "harm_stop", ["avgR 差 < −2·SE，建议提前终止配对"]
    # Preregs from before 2026-11-04 lack these keys; the defaults reproduce the
    # hardcoded behavior those pairs were judged with.
    needed = candidate["r_n"] if prereg.get("min_trades_scope", "both") == "candidate" \
        else min(candidate["r_n"], control["r_n"])
    if elapsed < prereg["min_days"] or needed < prereg["min_trades"]:
        return "insufficient", [f"需运行至少 {prereg['min_days']:g} 天且两侧各有 {prereg['min_trades']} 笔有效 R"]
    min_short = prereg.get("min_short_trades")
    if min_short is not None and (candidate.get("short_n") or 0) < min_short:
        return "insufficient", [f"候选 SHORT 平仓 {candidate.get('short_n') or 0} 笔，少于要求的 {min_short} 笔"]
    upper = prereg.get("trade_ratio_max", 1.5)
    if (delta is not None and delta > 0 and lower is not None and lower > -prereg["margin_r"]
            and ratio is not None and ratio >= 0.67 and (upper is None or ratio <= upper)):
        return "promote", ["avgR 增量、单侧 95% 下界及平仓数比均通过预登记门槛"]
    if delta is not None and delta > 0 and ratio is not None and ratio < 0.67:
        return "fewer_trades", ["avgR 为正增量但平仓数比 < 0.67，转入交易频率问题"]
    if se is not None and se > prereg["extend_se"] and elapsed < prereg["min_days"] + prereg["extend_days"]:
        return "extend", [f"SE 仍偏大，最多延长至 {prereg['min_days'] + prereg['extend_days']:g} 天"]
    return "reject", ["未通过预登记增量门槛（包括正式判定期内无法计算 SE）"]


def daily_return_comparison(candidate: dict, control: dict, as_of: datetime) -> dict:
    """Paired difference of daily equity returns; weeks (UTC Monday) are clusters.

    A trader's daily return is last-snapshot-of-day d over last snapshot of
    day d−1 (equity_statistics), completed UTC days only; the paired days are
    the days present for both arms.
    """
    def returns(stats):
        return {row["date"]: row["return"] for row in stats["daily_equity"]
                if row["return"] is not None and as_utc(row["date"]) < midnight(as_of)}

    cand, ctrl = returns(candidate), returns(control)
    sequence = [{"date": day, "candidate_return": cand[day], "control_return": ctrl[day],
                 "diff": finite(cand[day] - ctrl[day])} for day in sorted(cand.keys() & ctrl.keys())]
    sequence = [row for row in sequence if row["diff"] is not None]
    n = len(sequence)
    estimate = mean(row["diff"] for row in sequence)
    buckets = defaultdict(list)
    for row in sequence:
        day = as_utc(row["date"])
        buckets[day - timedelta(days=day.weekday())].append(row["diff"])
    influences = [finite_sum((value - estimate) / n for value in rows) for rows in buckets.values()] \
        if estimate is not None else []
    se = cluster_se(influences) if n and all(value is not None for value in influences) else None
    return {"n_days": n, "n_weeks": len(buckets), "mean_diff": estimate, "cluster_se": se,
            "ci95": ci95(estimate, se), "daily_diffs": sequence,
            "note": None if se is not None else "配对日不足两个聚类周或均值不可计算，不报告标准误或 CI"}


def decide_daily_return(elapsed: float, prereg: dict, candidate: dict, control: dict,
                        daily: dict) -> tuple[str, list[str]]:
    """Same ordered table as decide_incremental, on the daily-return metric.

    margin_daily replaces margin_r, trade_ratio_max (null = no upper bound)
    replaces the hardcoded 1.5, extend_se_daily replaces extend_se, and the
    sample floors count closed trades (candidate only when
    min_trades_scope=candidate) plus the candidate's SHORT closes.
    """
    delta, se = finite(daily["mean_diff"]), finite(daily["cluster_se"])
    lower = finite(delta - 1.645 * se) if delta is not None and se is not None else None
    ratio = candidate["n"] / control["n"] if control["n"] else None
    if elapsed < prereg["harm_check_days"]:
        return "insufficient", [f"尚未达到伤害检查期 {prereg['harm_check_days']:g} 天"]
    if delta is not None and se is not None and delta < -2 * se:
        return "harm_stop", ["每日收益差 < −2·SE，建议提前终止配对"]
    reasons = []
    if elapsed < prereg["min_days"]:
        reasons.append(f"已运行 {elapsed:.2f} 天，少于要求的 {prereg['min_days']:g} 天")
    min_trades = prereg["min_trades"]
    if candidate["n"] < min_trades:
        reasons.append(f"候选已平仓 {candidate['n']} 笔，少于要求的 {min_trades} 笔")
    if prereg.get("min_trades_scope", "both") != "candidate" and control["n"] < min_trades:
        reasons.append(f"对照已平仓 {control['n']} 笔，少于要求的 {min_trades} 笔")
    min_short = prereg.get("min_short_trades")
    short_n = candidate.get("short_n") or 0
    if min_short is not None and short_n < min_short:
        reasons.append(f"候选 SHORT 平仓 {short_n} 笔，少于要求的 {min_short} 笔")
    if reasons:
        return "insufficient", reasons
    upper = prereg.get("trade_ratio_max", 1.5)
    if (delta is not None and delta > 0 and lower is not None and lower > -prereg.get("margin_daily", 0.0002)
            and ratio is not None and ratio >= 0.67 and (upper is None or ratio <= upper)):
        return "promote", ["每日收益差增量、单侧 95% 下界及平仓数比均通过预登记门槛"]
    if delta is not None and delta > 0 and ratio is not None and ratio < 0.67:
        return "fewer_trades", ["每日收益差为正增量但平仓数比 < 0.67，转入交易频率问题"]
    if (se is not None and se > prereg.get("extend_se_daily", 0.0001)
            and elapsed < prereg["min_days"] + prereg["extend_days"]):
        return "extend", [f"周聚类 SE 仍偏大，最多延长至 {prereg['min_days'] + prereg['extend_days']:g} 天"]
    return "reject", ["未通过预登记增量门槛（包括正式判定期内无法计算 SE）"]


def us_stock_extra_fields(candidate: dict, control: dict) -> dict:
    """Informational short-mode context; never part of any verdict."""
    long_c, long_b = finite(candidate.get("long_avg_r")), finite(control.get("long_avg_r"))
    return {"note": "做空配对辅助指标，仅报告，不参与判定",
            "candidate_short_avg_r": finite(candidate.get("short_avg_r")),
            "candidate_short_n": candidate.get("short_n"),
            "candidate_long_avg_r": long_c,
            "control_long_avg_r": long_b,
            "long_avg_r_diff": finite(long_c - long_b) if long_c is not None and long_b is not None else None,
            "max_drawdown": {"candidate": candidate.get("max_drawdown"), "control": control.get("max_drawdown")},
            "candidate_short_funding_usdt": candidate.get("short_funding_usdt")}


def incremental_fields(pair: dict, as_of: datetime, elapsed: float, candidate: dict,
                       control: dict, weekly: dict, daily: dict | None = None) -> dict:
    metrics = daily if daily is not None else weekly
    delta, se = finite(metrics["mean_diff"]), finite(metrics["cluster_se"])
    if daily is not None:
        verdict, reasons = decide_daily_return(elapsed, pair["prereg"], candidate, control, daily)
    else:
        verdict, reasons = decide_incremental(elapsed, pair["prereg"], candidate, control, weekly)
    if pair.get("promoted_at"):
        if as_of >= as_utc(pair["holdback_until"]):
            verdict, reasons = "holdback_done", ["保留对照期已结束"]
        elif delta is not None and se is not None and delta < -2 * se:
            verdict, reasons = "rollback_suggested", ["晋升后 avgR 差 < −2·SE，建议人工审批回滚" if daily is None
                                                      else "晋升后每日收益差 < −2·SE，建议人工审批回滚"]
        else:
            verdict, reasons = "holdback_ok", ["保留对照期内未触发回滚条件；缺少 SE 时不能排除伤害"]
    result = {"delta": delta, "se": se,
              "lower_bound": finite(delta - 1.645 * se) if delta is not None and se is not None else None,
              "trade_ratio": candidate["n"] / control["n"] if control["n"] else None,
              "retroactive": bool(pair["prereg"].get("retroactive", False)),
              "evaluation_started_at": pair.get("promoted_at") or pair["started_at"],
              "evaluation_closed_counts": {"candidate": candidate["n"], "control": control["n"]},
              "incremental_weekly_avg_r": weekly, "verdict": verdict, "reasons": reasons}
    if daily is not None:
        result["incremental_metric"] = "daily_return"
        result["incremental_daily_return"] = daily
    return result


def normalize_pair(pair: dict) -> dict:
    pair = dict(pair)
    for key in ("id", "candidate_trader_id", "control_trader_id", "started_at"):
        if not isinstance(pair.get(key), str) or not pair[key].strip():
            raise ValueError(f"pair 缺少有效字段：{key}")
    if pair["candidate_trader_id"] == pair["control_trader_id"]:
        raise ValueError(f"{pair['id']}：候选与对照必须是不同交易员")
    if pair.get("live_trader_id") is not None and not isinstance(pair["live_trader_id"], str):
        raise ValueError(f"{pair['id']}：live_trader_id 必须是字符串或 null")
    pair["started_at"] = iso(as_utc(pair["started_at"]))
    days = finite(pair.get("min_days", 7))
    trades = finite(pair.get("min_trades", 20))
    if days is None or days < 7:
        raise ValueError(f"{pair['id']}：min_days 必须至少为 7 天")
    if trades is None or not trades.is_integer() or trades < 1:
        raise ValueError(f"{pair['id']}：min_trades 必须是正整数")
    pair.update(min_days=days, min_trades=int(trades))
    if "prereg" in pair and pair.get("promoted_at"):
        promoted = as_utc(pair["promoted_at"])
        until = as_utc(pair["holdback_until"])
        if promoted < as_utc(pair["started_at"]) or until <= promoted:
            raise ValueError(f"{pair['id']}：保留对照时间必须晚于启动时间且顺序有效")
        pair.update(promoted_at=iso(promoted), holdback_until=iso(until))
    return pair


def open_readonly(db_path: Path | str) -> sqlite3.Connection:
    """Do not use immutable=1: a live database may have committed data in WAL."""
    connection = sqlite3.connect(Path(db_path).resolve().as_uri() + "?mode=ro", uri=True, timeout=30)
    connection.row_factory = sqlite3.Row
    connection.execute("PRAGMA query_only = ON")
    return connection


def read_strategy_config(connection: sqlite3.Connection, trader_id: str) -> dict | None:
    """Parse the trader's strategy config; anything unreadable means not us_stock."""
    try:
        row = connection.execute(
            "SELECT s.config FROM traders t JOIN strategies s ON s.id = t.strategy_id WHERE t.id = ?",
            (trader_id,)).fetchone()
    except sqlite3.Error:
        return None
    if row is None or row[0] is None:
        return None
    try:
        config = json.loads(row[0])
    except (ValueError, TypeError):
        return None
    return config if isinstance(config, dict) else None


def stock_paper_rows(connection: sqlite3.Connection, trader_id: str,
                     start_ms: int, end_ms: int) -> list[dict]:
    """Closed us_stock paper rows, mapped onto the trader_positions column names.

    R uses avg_price/initial_stop/entry_quantity, exit time is closed_at. The
    side/funding columns were added later: a database without them means every
    row is a LONG with zero funding.
    """
    statement = ("SELECT id, symbol, closed_at AS exit_time, realized_pnl, fee, "
                 "initial_stop AS initial_stop_loss, avg_price AS entry_price, "
                 "entry_quantity AS quantity{extra} FROM stock_paper_positions "
                 "WHERE trader_id = ? AND status = 'CLOSED' AND closed_at >= ? AND closed_at <= ? "
                 "ORDER BY closed_at, id")
    rows, last_error = None, None
    for extra in (", side, funding", ", side", ""):
        try:
            rows = [dict(row) for row in
                    connection.execute(statement.format(extra=extra), (trader_id, start_ms, end_ms))]
            break
        except sqlite3.OperationalError as error:
            last_error = error
    if rows is None:
        raise last_error
    for row in rows:
        side = row.get("side")
        row["side"] = side if side in ("LONG", "SHORT") else "LONG"
        funding = finite(row.get("funding"))
        row["funding"] = funding if funding is not None else 0.0
    return rows


def read_trader(connection: sqlite3.Connection, trader_id: str,
                started_at: datetime, as_of: datetime) -> tuple[dict, list]:
    # Millisecond bounds are inclusive, without rounding rows into the window.
    start_ms = math.ceil(started_at.timestamp() * 1000)
    end_ms = math.floor(as_of.timestamp() * 1000)
    us_stock = (read_strategy_config(connection, trader_id) or {}).get("strategy_type") == "us_stock"
    if us_stock:
        # Paper us_stock trades never appear in trader_positions.
        rows = stock_paper_rows(connection, trader_id, start_ms, end_ms)
        open_positions = connection.execute(
            "SELECT COUNT(*) FROM stock_paper_positions WHERE trader_id = ? AND status = 'OPEN' AND opened_at <= ?",
            (trader_id, end_ms)).fetchone()[0]
    else:
        rows = [dict(row) for row in connection.execute(
            "SELECT id, symbol, exit_time, realized_pnl, fee, initial_stop_loss, entry_price, quantity "
            "FROM trader_positions WHERE trader_id = ? AND status = 'CLOSED' "
            "AND exit_time >= ? AND exit_time <= ? ORDER BY exit_time, id",
            (trader_id, start_ms, end_ms))]
        open_positions = connection.execute(
            "SELECT COUNT(*) FROM trader_positions WHERE trader_id = ? AND status = 'OPEN' AND entry_time <= ?",
            (trader_id, end_ms)).fetchone()[0]
    # Use the trader/time index, padding whole dates for GORM's space/T and
    # offset encodings. Exact UTC bounds are applied after parsing, above.
    lower = (started_at - timedelta(days=1)).date().isoformat()
    upper = (as_of + timedelta(days=2)).date().isoformat()
    snapshots = [dict(row) for row in connection.execute(
        "SELECT id, timestamp, total_equity FROM trader_equity_snapshots "
        "WHERE trader_id = ? AND timestamp >= ? AND timestamp < ? ORDER BY timestamp, id",
        (trader_id, lower, upper))]
    stats, r_samples = trade_statistics(rows, gross_r=us_stock)
    if us_stock:
        # Long/short splits and accrued funding are informational context for
        # the short-mode pair; a missing side means every row is a LONG.
        longs = [row for row in rows if row["side"] != "SHORT"]
        shorts = [row for row in rows if row["side"] == "SHORT"]
        short_stats, _ = trade_statistics(shorts, gross_r=True)
        long_stats, _ = trade_statistics(longs, gross_r=True)
        stats.update(long_n=long_stats["n"], short_n=short_stats["n"],
                     long_avg_r=long_stats["avg_r"], short_avg_r=short_stats["avg_r"],
                     short_funding_usdt=finite_sum(row["funding"] for row in shorts))
    stats.update(equity_statistics(snapshots, started_at, as_of))
    stats.update(trader_id=trader_id, open_positions=open_positions)
    return stats, r_samples


def build_report(pairs: list[dict], db_path: Path | str, as_of: datetime) -> dict:
    """Read every configured pair in one consistent, bounded read transaction."""
    as_of = as_utc(as_of)
    def tampered(pair):
        return "prereg" in pair and not verify_prereg(pair["prereg"])

    pairs = [dict(pair) if tampered(pair) else normalize_pair(pair) for pair in pairs]
    if len({pair["id"] for pair in pairs}) != len(pairs):
        raise ValueError("pair id 不可重复")
    report = {"schema_version": 1, "generated_at": iso(as_of), "pairs": [],
              "caveats": CAVEAT, "methodology": {
                  "pnl": PNL_BASIS,
                  "r": "净盈亏 / (abs(entry_price - initial_stop_loss) * quantity)；|R|>15 保留并提示",
                  "rates": "胜率、收益率、MaxDD 均为小数比例，非百分数",
                  "equity": "UTC 日末最后快照，观测区间内缺日向前填充；MaxDD 基于日末权益；期间收益率以首个实际快照为基准",
                  "daily_comparison": "完整 UTC 日收益取交集，不含当天；日为聚类，SE=sqrt(G/(G-1)*sum(influence^2))；G<10 不给 SE/CI",
                  "noninferiority": f"相同配对日的日末权益增量差（USDT/日）CI95 下界 > {-NONINFERIORITY_MARGIN:g}；"
                                    f"配对日 >= {MIN_PAIRED_DAYS}，两侧平仓各 >= max(min_trades, {MIN_CLOSED_TRADES})；"
                                    "权益增量含浮盈亏，外部资金流会影响结果，比较账户需保持资金口径一致",
                  "weekly_r": "两侧有效 R 笔数均达 min_trades 后，按退出时间的 UTC 周一聚类；交易加权 avgR 差；联合周<3 不给 SE/CI",
                  "ci": "近似正态 95% CI = 均值差 +/- 1.96*聚类 SE",
                  "open_positions": "当前 status=OPEN 的持仓数，包含 started_at 前入场的持仓",
                  "scope": "仅统计，不校验模拟盘标识或两侧配置一致性，不自动修改任何交易员配置",
              }}
    if not pairs:
        return report  # Empty configuration must work even without a database.

    def ended(pair):
        # An ended pair (stopped traders, e.g. after an offline S1) is listed,
        # never re-scored: its elapsed days would otherwise keep growing.
        if pair.get("ended_at") is None:
            return False
        if as_utc(pair["ended_at"]) < as_utc(pair["started_at"]):
            raise ValueError(f"{pair['id']}: ended_at 早于 started_at")
        return True

    trusted = any(not tampered(pair) and not ended(pair) for pair in pairs)
    btc_returns, btc_error = load_btc_returns(as_of) if trusted else ({}, None)
    with ExitStack() as stack:
        connection = stack.enter_context(closing(open_readonly(db_path))) if trusted else None
        if connection is not None:
            connection.execute("BEGIN")
        cache = {}
        for pair in pairs:
            if tampered(pair):
                report["pairs"].append({"id": pair["id"], "verdict": "prereg_tampered"})
                continue
            if ended(pair):
                start, stop = as_utc(pair["started_at"]), as_utc(pair["ended_at"])
                reason = pair.get("end_reason") or "配对已结束"
                report["pairs"].append({
                    **{key: pair.get(key) for key in ("id", "title", "feature", "candidate_trader_id",
                                                      "control_trader_id", "started_at", "min_days", "min_trades")},
                    "ended_at": iso(stop), "end_reason": reason, "verdict": "ended",
                    "elapsed_days": (stop - start).total_seconds() / 86400,
                    "reasons": [reason], "warnings": [], "caveats": CAVEAT})
                continue
            start = as_utc(pair["started_at"])

            def trader(trader_id, window_start=start):
                key = (trader_id, window_start)
                if key not in cache:
                    cache[key] = read_trader(connection, trader_id, window_start, as_of)
                return cache[key]

            candidate, cand_r = trader(pair["candidate_trader_id"])
            control, ctrl_r = trader(pair["control_trader_id"])
            paired = paired_daily_comparison(candidate, control, as_of)
            weekly = weekly_r_comparison(cand_r, ctrl_r, pair["min_trades"])
            elapsed = max(0.0, (as_of - start).total_seconds() / 86400)
            verdict, reasons = decide_verdict(elapsed, pair["min_days"], pair["min_trades"], candidate, control, paired)
            warnings = []
            item = {**pair, "elapsed_days": elapsed, "candidate": candidate, "control": control,
                    "paired_daily": paired, "weekly_avg_r": weekly, "verdict": verdict,
                    "reasons": reasons, "caveats": CAVEAT, "warnings": warnings}
            if "prereg" in pair:
                inc_c, inc_b = candidate, control
                if pair.get("promoted_at"):
                    # Strictly after promotion, including already-open trades that exit later.
                    after = as_utc(pair["promoted_at"]) + timedelta(microseconds=1)
                    inc_c, cand_r = trader(pair["candidate_trader_id"], after)
                    inc_b, ctrl_r = trader(pair["control_trader_id"], after)
                inc_weekly = weekly_r_comparison(cand_r, ctrl_r, 1)
                item.update(legacy_verdict=verdict, legacy_reasons=reasons)
                if pair["prereg"].get("metric", "avg_r") == "daily_return":
                    inc_daily = daily_return_comparison(inc_c, inc_b, as_of)
                    item.update(incremental_fields(pair, as_of, elapsed, inc_c, inc_b, inc_weekly, inc_daily),
                                us_stock_extra=us_stock_extra_fields(candidate, control))
                else:
                    item.update(incremental_fields(pair, as_of, elapsed, inc_c, inc_b, inc_weekly))
            if pair.get("live_trader_id"):
                item["live"], _ = trader(pair["live_trader_id"])
            for role in ("candidate", "control", "live"):
                if role not in item:
                    continue
                stats = item[role]
                for field, label in (("r_extreme_count", "笔 |R|>15 极端值，已保留在 avgR 中，请核查原始成交"),
                                     ("r_excluded", "笔未计入 R 统计"),
                                     ("pnl_invalid", "笔净盈亏无效，汇总净盈亏及胜率置 null"),
                                     ("snapshot_invalid", "条无效权益快照被排除"),
                                     ("forward_filled_days", "个权益缺失日向前填充，可能低估波动")):
                    if stats[field]:
                        warnings.append(f"{role}：{stats[field]} {label}")
                if not stats["snapshot_count"]:
                    warnings.append(f"{role}：期间无有效权益快照")
            item["audit"] = pair_audit(candidate, control, as_of, btc_returns, btc_error)
            report["pairs"].append(item)
    return json_safe(report)


class _AlreadyRecorded(Exception):
    pass


def record_ledger(report: dict, path: Path) -> None:
    """Check deduplication inside append_event's existing exclusive ledger lock."""
    for item in report["pairs"]:
        verdict = item["verdict"]
        if "prereg" not in item or verdict not in {"harm_stop", "promote", "fewer_trades", "reject", "rollback_suggested"}:
            continue
        prereg = item["prereg"]
        stage = "S3" if verdict == "rollback_suggested" else "S2"

        def build(k, previous):
            if any(row.get("pair_id") == item["id"] and row.get("stage") == stage and row.get("verdict") == verdict
                   for row in previous):
                raise _AlreadyRecorded
            delta, se = item["delta"], item["se"]
            z = delta / se if delta is not None and se is not None and se > 0 else None
            return {"stage": stage, "verdict": verdict, "pair_id": item["id"],
                    "idea_id": prereg["idea_id"], "change_type": prereg["change_type"],
                    "baseline_version": prereg["baseline_version"], "prereg_hash": prereg["prereg_hash"],
                    "delta": delta, "se": se, "z": z, "trade_ratio": item["trade_ratio"],
                    "placebo_pct": None, "retroactive": item["retroactive"]}

        try:
            evolution_ledger.append_event(path, prereg["track"], build)
        except _AlreadyRecorded:
            pass


def load_pairs(promotion_dir: Path) -> list[dict]:
    path = promotion_dir / "pairs.json"
    if not path.exists():
        return []
    config = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(config, dict) or not isinstance(config.get("pairs"), list):
        raise ValueError("pairs.json 必须是包含 pairs 数组的对象")
    if any(not isinstance(pair, dict) for pair in config["pairs"]):
        raise ValueError("pairs 中每一项必须是对象")
    return config["pairs"]


def write_report(path: Path, report: dict) -> None:
    """Write next to destination, fsync, then atomically replace; clean up on failure."""
    payload = json.dumps(json_safe(report), ensure_ascii=False, indent=2, allow_nan=False) + "\n"
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent,
                                         prefix=".report-", suffix=".tmp", delete=False) as handle:
            temporary = Path(handle.name)
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def summary(report: dict) -> str:
    def number(value, percent=False):
        return "null" if value is None else f"{value * 100:.2f}%" if percent else f"{value:.6f}"

    lines = [f"模拟盘晋升报告 {report['generated_at']} | pairs={len(report['pairs'])}"]
    for item in report["pairs"]:
        if item["verdict"] == "prereg_tampered":
            lines.append(f"[{item['id']}] prereg_tampered")
            continue
        if item["verdict"] == "ended":
            lines.append(f"[{item['id']}] ended（已结束 {item['ended_at']}，运行 {item['elapsed_days']:.2f} 天）")
            lines.append(f"  - {item['end_reason']}")
            continue
        verdict = "insufficient（样本不足）" if item['verdict'] == 'insufficient' else item['verdict']
        lines.append(f"[{item['id']}] {verdict} | 已运行 {item['elapsed_days']:.2f} 天")
        if "prereg" in item:
            p = item["prereg"]
            unit = "/日" if item.get("incremental_metric") == "daily_return" else " R"
            lines.append(f"  增量: Δ={number(item['delta'])}{unit}, SE={number(item['se'])}, "
                         f"单侧95%下界={number(item['lower_bound'])}, 平仓数比={number(item['trade_ratio'])}, "
                         f"统计起点={item['evaluation_started_at']}, legacy_verdict={item['legacy_verdict']}")
            metric_text = ""
            if "metric" in p:
                ratio_text = "不设上限" if p["trade_ratio_max"] is None else f"{p['trade_ratio_max']:g}"
                short_text = "不限" if p["min_short_trades"] is None else str(p["min_short_trades"])
                metric_text = (f", metric={p['metric']}, margin_daily={p['margin_daily']:g}, "
                               f"trade_ratio_max={ratio_text}, min_trades_scope={p['min_trades_scope']}, "
                               f"min_short_trades={short_text}, extend_se_daily={p['extend_se_daily']:g}")
            lines.append(f"  预登记: track={p['track']}, baseline={p['baseline_version']}, idea={p['idea_id']}, "
                         f"类型={p['change_type']}, margin_r={p['margin_r']:g}, "
                         f"min_days={p['min_days']:g}, min_trades={p['min_trades']}, "
                         f"harm_check_days={p['harm_check_days']:g}, extend_days={p['extend_days']:g}, "
                         f"extend_se={p['extend_se']:g}, registered_at={p['registered_at']}, "
                         f"prereg_hash={p['prereg_hash']}, retroactive={item['retroactive']}"
                         + metric_text)
        for role, label in (("candidate", "候选"), ("control", "对照"), ("live", "正式参照")):
            if role in item:
                stats = item[role]
                lines.append(f"  {label}: n={stats['n']}, 净盈亏={number(stats['net_pnl_usdt'])} USDT, "
                             f"胜率={number(stats['win_rate'], True)}, avgR={number(stats['avg_r'])}, "
                             f"R排除={stats['r_excluded']}, 极端R={stats['r_extreme_count']}, "
                             f"MaxDD={number(stats['max_drawdown'], True)}, "
                             f"收益率={number(stats['period_return'], True)}, 开仓数={stats['open_positions']}")
        paired = item["paired_daily"]
        lines.append(f"  配对日={paired['n_days']}, 日收益差均值={number(paired['mean_diff'], True)}, "
                     f"SE={number(paired['cluster_se'], True)}, CI95={paired['ci95']}, "
                     f"累计收益差={number(paired['cumulative_return_diff'], True)}")
        pnl = paired['pnl_usdt']
        lines.append(f"  日盈亏配对差={number(pnl['mean_diff'])} USDT/日, CI95={pnl['ci95']}, "
                     f"非劣效要求下界 > {-NONINFERIORITY_MARGIN:g} USDT/日")
        weekly = item["weekly_avg_r"]
        lines.append(f"  avgR差={number(weekly['mean_diff'])}, 聚类周={weekly['n_weeks']}, "
                     f"SE={number(weekly['cluster_se'])}, CI95={weekly['ci95']}")
        if item.get("incremental_metric") == "daily_return":
            daily = item["incremental_daily_return"]
            lines.append(f"  日收益配对: 配对日={daily['n_days']}, 聚类周={daily['n_weeks']}, "
                         f"周聚类SE={number(daily['cluster_se'], True)}, CI95={daily['ci95']}")
        extra = item.get("us_stock_extra")
        if extra:
            drawdowns = extra["max_drawdown"]
            lines.append(f"  做空辅助(仅报告): 候选空头 avgR={number(extra['candidate_short_avg_r'])}, "
                         f"空头笔数={extra['candidate_short_n']}, 多头avgR差={number(extra['long_avg_r_diff'])}, "
                         f"空头资金费={number(extra['candidate_short_funding_usdt'])} USDT, "
                         f"MaxDD 候选={number(drawdowns['candidate'], True)}, 对照={number(drawdowns['control'], True)}")
        for reason in item["reasons"] + item["warnings"]:
            lines.append(f"  - {reason}")
        if "audit" in item:
            audit = item["audit"]

            def audit_label(key):
                result = audit[key]
                label = result["verdict"]
                if label == "INSUFFICIENT_SAMPLE":
                    label += f"({result['n_days']}/{result['required']}天)"
                return label

            lines.append(f"  审计: 候选 vs BTC {audit_label('candidate_vs_benchmark')} | "
                         f"对照 vs BTC {audit_label('control_vs_benchmark')} | "
                         f"候选 vs 对照 {audit_label('candidate_vs_control')}")
    lines.append(report["caveats"])
    return "\n".join(lines)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ledger", nargs="?", const="", default=None, metavar="PATH",
                        help="追加最终 S2/S3 判定并去重；无 PATH 时用主仓库 data/evolution/ledger.jsonl；默认不记账")
    args = parser.parse_args(argv)
    promotion_dir = Path(os.environ.get("PROMOTION_DIR", REPO_ROOT / "data/promotion"))
    db_path = Path(os.environ.get("NOFX_DB", REPO_ROOT / "data/data.db"))
    try:
        report = build_report(load_pairs(promotion_dir), db_path, datetime.now(UTC))
        write_report(promotion_dir / "report.json", report)
        if args.ledger is not None:
            ledger_path = Path(args.ledger) if args.ledger else main_repo_root() / evolution_ledger.DEFAULT_PATH
            record_ledger(report, ledger_path)
    except (OSError, ValueError, sqlite3.Error) as error:
        print(f"晋升报告失败：{error}", file=sys.stderr)
        return 1
    print(summary(report))
    print(f"报告已写入 {promotion_dir / 'report.json'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
