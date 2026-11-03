"""B 类过滤器离线筛选与既有闸门的描述性影子分析。

Run as python -m scripts.backtest.incremental_filter <subcommand>.
Database access is read-only; the calculation functions perform no I/O.
"""

from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from contextlib import closing
from datetime import datetime, timedelta
import hashlib
import json
import math
import os
from pathlib import Path
import random
import re
import sqlite3
from statistics import NormalDist, stdev
import sys

from scripts.backtest import evolution_ledger as ledger
from scripts.promotion import report


QUEUE_NOTE = "screen_pass/strong 仅进入线上配对队列，不直接上线"
SHADOW_NOTE = "影子结果按计划止损止盈先到先算，非大模型逐周期管理的真实离场"
UTC = report.UTC


def weekly_statistics(samples: list[tuple[datetime, float]]) -> dict:
    """Trade-weighted mean, clustered on UTC Monday *entry* week.

    Each cluster's influence is sum(R - avgR) / N. Use report.cluster_se's
    G/(G-1) correction and normal CI; like report.weekly_r_comparison, fewer
    than three weeks gives no SE/CI (two clusters make the sandwich unusable).
    """
    n = len(samples)
    avg = report.mean(r for _, r in samples)
    buckets = defaultdict(list)
    for timestamp, r in samples:
        day = report.midnight(timestamp)
        buckets[day - timedelta(days=day.weekday())].append(r)
    influences = [math.fsum((r - avg) / n for r in values)
                  for values in buckets.values()] if avg is not None else []
    se = report.cluster_se(influences) if len(buckets) >= 3 else None
    return {"n": n, "avg_r": avg, "n_weeks": len(buckets),
            "se": se, "ci95": report.ci95(avg, se)}


def placebo_distribution(values: list[float], p: float, seed: int, placebo_n: int) -> list[float]:
    """Uniform fixed-count random drops, with an independent seeded RNG."""
    if placebo_n < 2:
        raise ValueError("placebo-n 至少为 2")
    n, count = len(values), round(p * len(values))
    if not 0 <= p <= 1 or not 0 < count < n:
        return []
    rng = random.Random(seed)
    avg = math.fsum(values) / n
    deltas = []
    for _ in range(placebo_n):
        dropped = set(rng.sample(range(n), count))
        # Sum the kept set directly, matching the observed statistic even at
        # floating-point ties; total minus dropped can change strict ranks.
        kept_avg = math.fsum(r for i, r in enumerate(values) if i not in dropped) / (n - count)
        deltas.append(kept_avg - avg)
    return deltas


def mean_delta(values: list[float], drops: list[bool]) -> float | None:
    kept = report.mean(r for r, drop in zip(values, drops) if not drop)
    avg = report.mean(values)
    return kept - avg if kept is not None and avg is not None else None


def filter_statistics(trades: list[dict], mask: dict[str, bool], *,
                      seed: int = 20261103, placebo_n: int = 2000) -> dict:
    """Missing mask entries <=5% are retained and explicitly counted as errors.

    Above 5%, abort performance calculations. Time halves are the first
    floor(N/2) and remaining rows sorted by UTC entry time, then trade ID.
    A half with no kept trades has no delta and cannot pass either screen.
    """
    if placebo_n < 2:
        raise ValueError("placebo-n 至少为 2")
    ordered = sorted(trades, key=lambda row: (report.as_utc(row["entry_time"]), str(row["trade_id"])))
    n = len(ordered)
    missing = sum(str(row["trade_id"]) not in mask for row in ordered)
    drops = [mask.get(str(row["trade_id"]), False) for row in ordered]
    dropped = sum(drops)
    p = dropped / n if n else 0.0
    coverage_error = missing / n if n else 0.0
    reasons = []
    if n < 100:
        reasons.append("总交易数不足 100")
    if coverage_error > 0.05:
        reasons.append("掩码缺失比例超过 5%，已中止绩效计算")
    if p in (0, 1):
        reasons.append("剔除率为 0 或 1")
    result = {"n": n, "dropped_n": dropped, "p": p, "trade_ratio": (n - dropped) / n if n else 0.0,
              "mask_missing": missing, "mask_coverage_error": coverage_error,
              "mask_extra": len(mask.keys() - {str(row["trade_id"]) for row in ordered}),
              "invalid_reasons": reasons, "delta": None, "se": None, "z": None,
              "placebo_pct": None, "dev_delta": None, "holdout_delta": None,
              "all_avg_r": None, "kept_avg_r": None, "dropped_avg_r": None,
              "dropped_avg_r_ci95": None, "dropped_cluster_se": None,
              "dropped_n_weeks": 0, "total_r_change": None}
    if coverage_error > 0.05:
        return result
    values = [row["r"] for row in ordered]
    dropped_stats = weekly_statistics([(report.as_utc(row["entry_time"]), row["r"])
                                       for row, drop in zip(ordered, drops) if drop])
    delta = mean_delta(values, drops)
    placebo = placebo_distribution(values, p, seed, placebo_n)
    se = stdev(placebo) if placebo else None
    z = None
    if se is not None and delta is not None:
        z = delta / se if se else (0.0 if delta == 0 else math.copysign(math.inf, delta))
    half = n // 2
    result.update(delta=delta, se=se, z=z,
                  placebo_pct=sum(d < delta for d in placebo) / len(placebo) if placebo else None,
                  dev_delta=mean_delta(values[:half], drops[:half]),
                  holdout_delta=mean_delta(values[half:], drops[half:]),
                  all_avg_r=report.mean(values),
                  kept_avg_r=report.mean(r for r, drop in zip(values, drops) if not drop),
                  dropped_avg_r=dropped_stats["avg_r"], dropped_avg_r_ci95=dropped_stats["ci95"],
                  dropped_cluster_se=dropped_stats["se"], dropped_n_weeks=dropped_stats["n_weeks"],
                  total_r_change=-math.fsum(r for r, drop in zip(values, drops) if drop))
    return result


def z_threshold(k: int) -> float:
    if k < 1:
        raise ValueError("K 必须至少为 1")
    return NormalDist().inv_cdf(1 - 0.05 / (2 * k))


def decide_verdict(stats: dict, k: int) -> str:
    """S1 type-B verdicts, in their declared first-match order."""
    if stats["n"] < 100 or stats["mask_coverage_error"] > 0.05 or stats["p"] in (0, 1):
        return "invalid"
    if stats["p"] > 0.5:
        return "fewer_trades"
    both_positive = all(stats[key] is not None and stats[key] > 0 for key in ("dev_delta", "holdout_delta"))
    z, pct = stats["z"], stats["placebo_pct"]
    if z is not None and pct is not None and both_positive and pct >= 0.90:
        if z >= z_threshold(k):
            return "strong"
        if z >= 1.0:
            return "screen_pass"
    if pct is not None and pct < 0.90 and stats["total_r_change"] > 0:
        return "fewer_trades"
    return "fail"


def read_closed_trades(connection: sqlite3.Connection, traders: list[str] | None = None,
                       since: datetime | None = None, until: datetime | None = None) -> tuple[list[dict], dict]:
    """Use read_trader's CLOSED trader_positions source, bounded on entry time.

    R and its exclusions come directly from report.trade_statistics, one row
    at a time so its validated R can be attached to the original trade ID.
    since is inclusive and until exclusive, both in UTC.
    """
    clauses, params = ["status = 'CLOSED'"], []
    if traders:
        clauses.append(f"trader_id IN ({','.join('?' for _ in traders)})")
        params.extend(traders)
    for timestamp, operator in ((since, ">="), (until, "<")):
        if timestamp is not None:
            clauses.append(f"entry_time {operator} ?")
            params.append(math.ceil(report.as_utc(timestamp).timestamp() * 1000))
    rows = connection.execute(
        "SELECT id, trader_id, symbol, side, entry_time, exit_time, realized_pnl, fee, "
        "initial_stop_loss, entry_price, quantity FROM trader_positions WHERE " +
        " AND ".join(clauses) + " ORDER BY entry_time, id", params)
    exported, excluded = [], Counter()
    total = 0
    for raw in rows:
        total += 1
        row = dict(raw)
        try:
            stats, samples = report.trade_statistics([row])
            if not samples:
                excluded.update(stats["r_excluded_reasons"])
                continue
            entered = datetime.fromtimestamp(row["entry_time"] / 1000, UTC)
        except (ValueError, TypeError, OverflowError, OSError):
            excluded["missing_or_invalid_time"] += 1
            continue
        exported.append({"trade_id": row["id"], "trader_id": row["trader_id"],
                         "symbol": row["symbol"], "side": row["side"],
                         "entry_time": report.iso(entered), "exit_time": report.iso(samples[0][0]),
                         "entry_price": row["entry_price"], "initial_stop_loss": row["initial_stop_loss"],
                         "r": samples[0][1], "net_pnl_usdt": stats["net_pnl_usdt"]})
    return exported, {"total": total, "exported": len(exported), "excluded": total - len(exported),
                      "excluded_reasons": dict(excluded)}


def shadow_r(row: dict, horizon: int = 8) -> tuple[float | None, str | None]:
    """Would-be R, without fees, following auto_trader_shadow.go's outcomes.

    sl_first = -1 (including same-bar TP/SL); tp_first = signed TP distance
    divided by risk; timeout uses the horizon's closing exit price. Long
    stops must lie below entry and short stops above it. Empty outcomes,
    no_data (incomplete candles), and unfilled limit anchors have no R.
    Never fall back from an unresolved 48h outcome to the 8h outcome.
    """
    if horizon not in (8, 48):
        raise ValueError("horizon 必须为 8 或 48")
    outcome = row.get("outcome_48h" if horizon == 48 else "outcome")
    if outcome not in ("sl_first", "tp_first", "timeout"):
        return None, "unresolved" if not outcome else str(outcome)
    entry, stop = report.finite(row.get("entry_price")), report.finite(row.get("stop_price"))
    sign = {"long": 1, "short": -1}.get(row.get("direction"))
    if sign is None:
        return None, "invalid_direction"
    if entry is None or stop is None or entry <= 0 or stop <= 0:
        return None, "invalid_stop"
    risk = (entry - stop) * sign
    if risk <= max(1e-12, abs(entry) * 1e-9):
        return None, "invalid_stop"
    if outcome == "sl_first":
        return -1.0, None
    price_key = "take_profit" if outcome == "tp_first" else "exit_price_48h" if horizon == 48 else "exit_price"
    price = report.finite(row.get(price_key))
    if price is None or price <= 0 or (outcome == "tp_first" and (price - entry) * sign <= 0):
        return None, "invalid_exit_or_tp"
    r = report.finite((price - entry) / risk * sign)
    return (r, None) if r is not None else (None, "nonfinite_r")


_NUMERIC_TOKEN = re.compile(r"(?<=_)[+-]?\d+(?:\.\d+)?(?=_|$)")


def code_family(code: str) -> str:
    """Collapse numeric parameters: NEG_EDGE_SCORE_+0_LT_80 -> NEG_EDGE_SCORE_#_LT_#."""
    return _NUMERIC_TOKEN.sub("#", code)


def shadow_analysis(connection: sqlite3.Connection, traders: list[str] | None = None,
                    since: datetime | None = None, *, horizon: int = 8,
                    min_n: int = 30, as_of: datetime | None = None, raw_codes: bool = False) -> dict:
    """Compare each code with executions by its contributing traders, same window."""
    as_of = report.as_utc(as_of or datetime.now(UTC))
    since = report.as_utc(since) if since else None
    clauses, params = ["created_at < ?"], [(as_of + timedelta(days=2)).date().isoformat()]
    if since:
        clauses.append("created_at >= ?")
        params.append((since - timedelta(days=1)).date().isoformat())
    if traders:
        clauses.append(f"trader_id IN ({','.join('?' for _ in traders)})")
        params.extend(traders)
    groups, group_traders, excluded = defaultdict(list), defaultdict(set), Counter()
    total, start = 0, since
    for raw in connection.execute("SELECT * FROM gate_shadow_blocks WHERE " + " AND ".join(clauses), params):
        row = dict(raw)
        try:
            timestamp = report.as_utc(row["created_at"])
        except (ValueError, TypeError, AttributeError):
            excluded["invalid_created_at"] += 1
            continue
        if timestamp >= as_of or (since and timestamp < since):
            continue
        total += 1
        start = min(start, timestamp) if start else timestamp
        r, reason = shadow_r(row, horizon)
        if reason:
            excluded[reason] += 1
            continue
        codes = {code.strip() if raw_codes else code_family(code.strip())
                 for code in (row.get("blocked_codes") or "").split(",") if code.strip()}
        if not codes:
            excluded["missing_blocked_code"] += 1
        for code in codes:
            groups[code].append((timestamp, r))
            group_traders[code].add(row["trader_id"])
    selected = sorted(set().union(*group_traders.values())) if group_traders else []
    executed, export_counts = read_closed_trades(connection, selected, start, as_of) if selected else ([], {})
    results = []
    for code, samples in sorted(groups.items()):
        if len(samples) < min_n:
            continue
        stats = weekly_statistics(samples)
        same_traders = group_traders[code]
        baseline = [row["r"] for row in executed if row["trader_id"] in same_traders]
        avg = report.mean(baseline)
        results.append({"code": code, **stats, "traders": sorted(same_traders),
                        "executed_n": len(baseline), "executed_avg_r": avg,
                        "possible_false_block": bool(stats["ci95"] and avg is not None and stats["ci95"][0] > avg)})
    return {"since": report.iso(start) if start else None, "until": report.iso(as_of),
            "horizon": horizon, "total": total, "excluded_reasons": dict(excluded),
            "export_counts": export_counts, "below_min_n_codes": sum(len(s) < min_n for s in groups.values()),
            "codes": results}


def read_jsonl(path: Path) -> tuple[list[dict], str]:
    payload = path.read_bytes()
    rows = []
    for line_n, line in enumerate(payload.decode("utf-8").splitlines(), 1):
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except ValueError as error:
            raise ValueError(f"{path}:{line_n} JSON 无效：{error}") from error
        if not isinstance(row, dict):
            raise ValueError(f"{path}:{line_n} 必须是 JSON 对象")
        rows.append(row)
    return rows, hashlib.sha256(payload).hexdigest()


def trade_key(row: dict) -> str:
    value = row.get("trade_id")
    if type(value) not in (int, str) or not str(value).strip():
        raise ValueError("trade_id 必须为非空字符串或整数")
    return str(value)


def evaluate_files(options) -> dict:
    trades, trades_hash = read_jsonl(options.trades)
    masks, mask_hash = read_jsonl(options.mask)
    seen, mask = set(), {}
    for row in trades:
        key = trade_key(row)
        if key in seen:
            raise ValueError(f"交易文件含重复 trade_id：{key}")
        seen.add(key)
        r = report.finite(row.get("r"))
        if r is None or isinstance(row.get("r"), bool):
            raise ValueError(f"交易 {key} 的 R 无效")
        row["r"] = r
        try:
            report.as_utc(row.get("entry_time"))
        except (ValueError, TypeError, AttributeError) as error:
            raise ValueError(f"交易 {key} 的 entry_time 无效") from error
    for row in masks:
        key = trade_key(row)
        if key in mask or type(row.get("drop")) is not bool:
            raise ValueError(f"掩码 {key} 重复或 drop 不是布尔值")
        mask[key] = row["drop"]
    stats = filter_statistics(trades, mask, seed=options.seed, placebo_n=options.placebo_n)

    def build(_k, previous):
        # Recompute inside append_event's lock; never use a pre-lock verdict.
        k = ledger.next_k(previous, options.track)
        verdict = decide_verdict(stats, k)
        return {**stats, "stage": "S1", "change_type": "B", "track": options.track,
                "baseline_version": options.baseline_version, "idea_id": options.idea_id,
                "hypothesis": options.hypothesis, "verdict": verdict,
                "K": None if verdict == "invalid" else k, "z_K": z_threshold(k),
                "seed": options.seed, "placebo_n": options.placebo_n,
                "trades_sha256": trades_hash, "mask_sha256": mask_hash}

    if options.no_ledger:
        return build(None, ledger.read_rows(options.ledger))
    return ledger.append_event(options.ledger, options.track, build)


def number(value) -> str:
    return "null" if value is None else f"{value:.6f}"


def evaluation_summary(row: dict) -> str:
    lines = [f"B 类 S1 筛选：{row['idea_id']} → {row['verdict']} | K={row['K']}，z_K={number(row['z_K'])}",
             f"交易 n={row['n']}，剔除={row['dropped_n']}，p={row['p']:.2%}，保留比例={row['trade_ratio']:.2%}",
             f"掩码缺失={row['mask_missing']}（{row['mask_coverage_error']:.2%}），额外 ID={row['mask_extra']}；缺失≤5%时按保留处理",
             f"ΔavgR={number(row['delta'])}，安慰剂 SE={number(row['se'])}，z={number(row['z'])}，安慰剂分位={number(row['placebo_pct'])}",
             f"前半 dev Δ={number(row['dev_delta'])}，后半 holdout Δ={number(row['holdout_delta'])}",
             f"被剔除 avgR={number(row['dropped_avg_r'])}，周聚类 CI95={row['dropped_avg_r_ci95']}（{row['dropped_n_weeks']} 周），总 R 变化={number(row['total_r_change'])}",
             f"seed={row['seed']}，安慰剂次数={row['placebo_n']}",
             f"交易 SHA256={row['trades_sha256']}；掩码 SHA256={row['mask_sha256']}"]
    lines.extend(f"无效原因：{reason}" for reason in row["invalid_reasons"])
    return "\n".join([*lines, QUEUE_NOTE])


def utc_date(value: str) -> datetime:
    try:
        parsed = datetime.strptime(value, "%Y-%m-%d").replace(tzinfo=UTC)
        if parsed.date().isoformat() != value:
            raise ValueError(value)
        return parsed
    except ValueError as error:
        raise argparse.ArgumentTypeError("日期必须为 YYYY-MM-DD（UTC）") from error


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    export = commands.add_parser("export-trades", help="只读导出基线真实平仓交易")
    export.add_argument("--trader", action="append", required=True)
    export.add_argument("--until", type=utc_date, help="UTC 入场日期，不含此日")
    export.add_argument("--out", type=Path, required=True)
    shadow = commands.add_parser("shadow", help="描述性分析既有闸门，无账本及判定")
    shadow.add_argument("--trader", action="append")
    shadow.add_argument("--horizon", type=int, choices=(8, 48), default=8)
    shadow.add_argument("--min-n", type=int, default=30)
    shadow.add_argument("--raw-codes", action="store_true", help="不合并数值参数，按原始拦截代码分组")
    for sub in (export, shadow):
        sub.add_argument("--since", type=utc_date, help="UTC 入场/影子创建日期，含此日")
        sub.add_argument("--db", type=Path, default=Path(os.environ.get("NOFX_DB", report.REPO_ROOT / "data/data.db")))
    evaluate = commands.add_parser("evaluate", help="固定剔除比例的随机安慰剂筛选")
    evaluate.add_argument("--trades", type=Path, required=True)
    evaluate.add_argument("--mask", type=Path, required=True)
    evaluate.add_argument("--idea-id", required=True)
    evaluate.add_argument("--hypothesis", required=True)
    evaluate.add_argument("--track", choices=ledger.TRACKS, required=True)
    evaluate.add_argument("--baseline-version", required=True)
    evaluate.add_argument("--ledger", type=Path, default=Path(ledger.DEFAULT_PATH))
    evaluate.add_argument("--seed", type=int, default=20261103)
    evaluate.add_argument("--placebo-n", type=int, default=2000)
    evaluate.add_argument("--no-ledger", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.command == "evaluate":
            row = evaluate_files(args)
            print("账本：仅打印预览" if args.no_ledger else f"账本：已追加 {args.ledger}")
            print(evaluation_summary(row))
            return 2 if row["mask_coverage_error"] > 0.05 else 0
        if args.command == "export-trades" and args.since and args.until and args.until <= args.since:
            raise ValueError("until 必须晚于 since")
        if args.command == "shadow" and args.min_n < 1:
            raise ValueError("min-n 必须至少为 1")
        with closing(report.open_readonly(args.db)) as connection:
            connection.execute("BEGIN")
            if args.command == "export-trades":
                rows, counts = read_closed_trades(connection, args.trader, args.since, args.until)
                args.out.parent.mkdir(parents=True, exist_ok=True)
                args.out.write_text("".join(json.dumps(row, ensure_ascii=False, allow_nan=False) + "\n" for row in rows), encoding="utf-8")
                print(f"真实平仓={counts['total']}，导出={counts['exported']}，排除={counts['excluded']}")
                print(f"排除原因：{json.dumps(counts['excluded_reasons'], ensure_ascii=False)}")
                print(f"入场时间：since 含、until 不含（UTC）；{report.PNL_BASIS}")
                print(f"输出：{args.out}")
            else:
                result = shadow_analysis(connection, args.trader, args.since, horizon=args.horizon, min_n=args.min_n,
                                         raw_codes=args.raw_codes)
                print(f"影子闸门描述性分析（{args.horizon}h）：{result['since']} 至 {result['until']}（不含）")
                print(f"影子记录={result['total']}，排除原因={result['excluded_reasons']}，不足 min-n 的代码数={result['below_min_n_codes']}")
                print(f"同期真实交易统计：{result['export_counts']}")
                print("代码 | n | avgR | 周聚类 CI95 | 聚类周数 | 同交易员执行 n / avgR | 提示")
                for row in result["codes"]:
                    flag = "可能误杀" if row["possible_false_block"] else ""
                    interval = "/".join(number(v) for v in row["ci95"]) if row["ci95"] else "null（不足三周）"
                    print(f"{row['code']} | {row['n']} | {number(row['avg_r'])} | {interval} | {row['n_weeks']} | "
                          f"{row['executed_n']} / {number(row['executed_avg_r'])} | {flag}")
                print("每条记录计入其每个拦截代码；各代码对照取贡献该代码的交易员，按同一入场时间窗汇总。仅作描述，不写账本、不作判定。")
                print(SHADOW_NOTE)
        return 0
    except (OSError, ValueError, sqlite3.Error) as error:
        print(f"错误：{error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
