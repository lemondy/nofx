"""按交易线登记当前基线；只写 baselines.json，数据库始终只读。"""

from __future__ import annotations

import argparse
from contextlib import closing
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import sqlite3
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from scripts.backtest import evolution_ledger
from scripts.promotion.create_pair import atomic_write, decode, document_bytes, main_repo_root, open_database, pairs_lock


def load_registry(path: Path) -> dict:
    if not path.exists():
        return {}
    registry = decode(path.read_text(encoding="utf-8"))
    if not isinstance(registry, dict):
        raise ValueError("baselines.json 必须是按 track 索引的对象")
    for track, entry in registry.items():
        if not isinstance(entry, dict) or "current" not in entry or "previous" not in entry:
            raise ValueError(f"{track} 必须包含 current 和 previous")
        for version in (entry["current"], entry["previous"]):
            if version is not None and (not isinstance(version, dict) or
                                        any(key not in version for key in ("strategy_id", "config_hash", "set_at", "note"))):
                raise ValueError(f"{track} 基线缺少 strategy_id/config_hash/set_at/note")
    return registry


def latest_config_hash(db_path: Path, strategy_id: str) -> str:
    with closing(open_database(db_path)) as connection:
        # Match store.StrategyStore.LatestConfigVersion: changed_at, then id.
        required = {"id", "strategy_id", "config_hash", "risk_hash", "source", "summary", "config",
                    "changed_at", "created_at"}
        columns = {row["name"] for row in connection.execute("PRAGMA table_info(strategy_config_versions)")}
        if missing := required - columns:
            raise ValueError(f"strategy_config_versions 缺少字段：{', '.join(sorted(missing))}")
        row = connection.execute(
            "SELECT config_hash FROM strategy_config_versions WHERE strategy_id = ? "
            "ORDER BY changed_at DESC, id DESC LIMIT 1", (strategy_id,)).fetchone()
        if row is None or not isinstance(row["config_hash"], str) or not row["config_hash"].strip():
            raise ValueError(f"策略 {strategy_id} 没有可用配置版本")
        return row["config_hash"]


def update_registry(path: Path, track: str, *, current=None, rollback=False, yes=False) -> dict:
    if track not in evolution_ledger.TRACKS:
        raise ValueError(f"track 必须是 {'/'.join(evolution_ledger.TRACKS)}")

    def plan():
        registry = load_registry(path)
        old = registry.get(track, {"current": None, "previous": None})
        if rollback:
            if old["previous"] is None:
                raise ValueError(f"{track} 没有上一基线可回滚")
            entry = {"current": old["previous"], "previous": old["current"]}
        else:
            entry = {"current": current, "previous": old["current"]}
        return {**registry, track: entry}

    if not yes:
        return plan()
    with pairs_lock(path.parent):
        registry = plan()
        atomic_write(path, document_bytes(registry))
        return registry


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for command in ("show", "set", "rollback"):
        sub = commands.add_parser(command, help={"show": "显示每条线的当前基线", "set": "登记基线（默认 dry-run）",
                                                "rollback": "交换当前与上一基线（默认 dry-run）"}[command])
        sub.add_argument("--registry", type=Path, help="默认主仓库 data/evolution/baselines.json")
        if command != "show":
            sub.add_argument("--track", choices=evolution_ledger.TRACKS, required=True)
            sub.add_argument("--yes", action="store_true", help="确认写入 JSON；不修改交易员配置")
        if command == "set":
            sub.add_argument("--strategy-id", required=True)
            sub.add_argument("--note", required=True)
            sub.add_argument("--db", type=Path, help="默认 NOFX_DB 或主仓库 data/data.db；只读")
    args = parser.parse_args(argv)
    try:
        path = args.registry if args.registry is not None else main_repo_root() / "data/evolution/baselines.json"
        if args.command == "show":
            registry = load_registry(path)
            for track in evolution_ledger.TRACKS:
                print(f"{track}: {json.dumps(registry.get(track, {}).get('current'), ensure_ascii=False)}")
            return 0
        current = None
        if args.command == "set":
            if not args.strategy_id.strip() or not args.note.strip():
                raise ValueError("strategy-id 和 note 不可为空")
            db_path = args.db if args.db is not None else Path(os.environ.get("NOFX_DB") or main_repo_root() / "data/data.db")
            current = {"strategy_id": args.strategy_id, "config_hash": latest_config_hash(db_path, args.strategy_id),
                       "set_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"), "note": args.note}
        registry = update_registry(path, args.track, current=current, rollback=args.command == "rollback", yes=args.yes)
        print(f"{'已写入' if args.yes else 'DRY-RUN（追加 --yes 才写入）'} {path}")
        print(json.dumps({args.track: registry[args.track]}, ensure_ascii=False, indent=2))
        return 0
    except (OSError, ValueError, sqlite3.Error) as error:
        print(f"基线登记失败：{error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
