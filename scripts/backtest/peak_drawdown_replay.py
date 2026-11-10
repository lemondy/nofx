"""按原始权益曲线描述账户回撤，不模拟熔断后的交易或收益。

实盘手动交易和出入金未经调整，出金可能误触发峰值熔断。
峰值选项默认关闭，仅建议模拟盘或独立账户使用。
"""

import argparse
import json
import math
import sqlite3
from contextlib import closing
from pathlib import Path


CAPS = (10, 15, 20)
NOTES = [
    "描述性重放，逐 trader 统计，不合并账户 peer，不模拟熔断对后续交易的影响。",
    "初始基准取 traders.initial_balance，峰值从初始资金起算，只使用截至当时的权益。",
    "回撤大于等于阈值算触发，严格小于阈值才复位，仅统计未触发到触发的切换。",
    "快照按 timestamp、id 排序，首次时间保留数据库原始时间文本。缺失快照不插值。",
    "初始资金无效时两种口径均不计算，缺少 trader 元数据的快照单独列出。",
    "最大回撤最低为 0；零或负权益照实纳入，空值或非有限权益跳过且不复位。无有效快照时最大回撤为空。",
    "实盘有手动交易和出入金，出金可能误触发峰值熔断。该选项默认关闭，仅建议模拟盘或独立账户使用。",
]


def open_readonly(db_path):
    """mode=ro also prevents silently creating a database for a mistyped path."""
    uri = Path(db_path).expanduser().resolve().as_uri() + "?mode=ro"
    connection = sqlite3.connect(uri, uri=True)
    connection.row_factory = sqlite3.Row
    return connection


def replay_trader(trader, snapshots):
    """Consume snapshots already ordered by time, including zero/negative equity."""
    initial = trader["initial_balance"]
    valid_initial = initial is not None and math.isfinite(initial) and initial > 0
    results = {
        basis: {
            str(cap): {"first_trigger_at": None, "trigger_count": 0}
            for cap in CAPS
        }
        for basis in ("initial", "peak")
    }
    active = {basis: {cap: False for cap in CAPS} for basis in results}
    maximum = {basis: 0.0 if valid_initial else None for basis in results}
    peak = initial if valid_initial else None
    count = 0
    invalid = 0
    valid_snapshots = 0
    for snapshot in snapshots:
        count += 1
        equity = snapshot["total_equity"]
        if equity is None or not math.isfinite(equity):
            invalid += 1
            continue
        if not valid_initial:
            continue
        valid_snapshots += 1
        peak = max(peak, equity)
        drawdowns = {
            "initial": (initial - equity) / initial * 100,
            "peak": (peak - equity) / peak * 100,
        }
        for basis, drawdown in drawdowns.items():
            maximum[basis] = max(maximum[basis], drawdown)
            for cap in CAPS:
                triggered = drawdown >= cap
                result = results[basis][str(cap)]
                if triggered and not active[basis][cap]:
                    result["trigger_count"] += 1
                    if result["first_trigger_at"] is None:
                        result["first_trigger_at"] = snapshot["timestamp"]
                active[basis][cap] = triggered
    if not valid_snapshots:
        maximum = {basis: None for basis in results}
    return {
        "trader_id": trader["id"],
        "name": trader["name"],
        "initial_balance": initial if initial is None or math.isfinite(initial) else None,
        "status": "ok" if valid_initial else "invalid_initial_balance",
        "snapshot_count": count,
        "invalid_snapshot_count": invalid,
        "max_initial_drawdown_pct": maximum["initial"],
        "max_peak_drawdown_pct": maximum["peak"],
        "caps": results,
    }


def replay_database(db_path):
    with closing(open_readonly(db_path)) as connection:
        # One read transaction keeps metadata and curves consistent if the
        # source is concurrently written. No schema changes or write queries.
        connection.execute("BEGIN")
        traders = connection.execute(
            "SELECT id, name, initial_balance FROM traders ORDER BY id"
        ).fetchall()
        rows = []
        for trader in traders:
            snapshots = connection.execute(
                "SELECT timestamp, total_equity FROM trader_equity_snapshots "
                "WHERE trader_id = ? ORDER BY timestamp, id",
                (trader["id"],),
            )
            rows.append(replay_trader(trader, snapshots))
        orphans = connection.execute(
            "SELECT DISTINCT trader_id FROM trader_equity_snapshots "
            "WHERE trader_id NOT IN (SELECT id FROM traders) ORDER BY trader_id"
        ).fetchall()
    return {
        "db": str(Path(db_path).expanduser().resolve()),
        "caps_pct": list(CAPS),
        "notes": NOTES,
        "traders": rows,
        "orphan_trader_ids": [row["trader_id"] for row in orphans],
    }


def render_markdown(report):
    lines = ["# 账户回撤描述性重放", ""]
    lines.extend(f"- {note}" for note in report["notes"])
    lines.extend([
        "",
        "| 交易员 | 阈值 | 初始口径首次触发 | 次数 | 峰值口径首次触发 | 次数 | 最大初始回撤 | 最大峰值回撤 |",
        "| --- | --- | --- | --- | --- | --- | --- | --- |",
    ])
    for trader in report["traders"]:
        label = f"{trader['name']} ({trader['trader_id']})"
        label = label.replace("|", "\\|").replace("\n", " ").replace("\r", " ")
        if trader["status"] != "ok":
            label += "（初始资金无效）"
        if not trader["snapshot_count"]:
            label += "（无快照）"
        maxima = [
            "—" if trader[key] is None else f"{trader[key]:.2f}%"
            for key in ("max_initial_drawdown_pct", "max_peak_drawdown_pct")
        ]
        for cap in report["caps_pct"]:
            initial = trader["caps"]["initial"][str(cap)]
            peak = trader["caps"]["peak"][str(cap)]
            lines.append(
                f"| {label} | {cap}% | {initial['first_trigger_at'] or '—'} | "
                f"{initial['trigger_count']} | {peak['first_trigger_at'] or '—'} | "
                f"{peak['trigger_count']} | {maxima[0]} | {maxima[1]} |"
            )
    if report["orphan_trader_ids"]:
        lines.extend(["", "缺少 traders 元数据的交易员（未计算）", ""])
        lines.extend(f"- {trader_id}" for trader_id in report["orphan_trader_ids"])
    return "\n".join(lines) + "\n"


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args(argv)
    report = replay_database(args.db)
    markdown = render_markdown(report)
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / "peak_drawdown_replay.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=2, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    (args.out / "peak_drawdown_replay.md").write_text(markdown, encoding="utf-8")
    print(markdown, end="")


if __name__ == "__main__":
    main()
