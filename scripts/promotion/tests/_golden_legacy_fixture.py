"""Build the deterministic legacy fixture and dump the report dict as JSON.

Run with the PRE-CHANGE code to capture the golden, and by the regression test
to rebuild the same fixture with the POST-CHANGE code:

    python -m scripts.promotion.tests._golden_legacy_fixture [output.json]

Temporary databases only.
"""

from contextlib import closing
from datetime import datetime, timedelta
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import unittest.mock as mock

from scripts.promotion import create_pair as pair_cli
from scripts.promotion import report as promotion

START = datetime(2026, 10, 1, tzinfo=promotion.UTC)
AS_OF = START + timedelta(days=40, hours=12)


def schema(connection: sqlite3.Connection) -> None:
    connection.executescript("""
    CREATE TABLE trader_positions (
        id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL, symbol TEXT NOT NULL,
        status TEXT DEFAULT 'OPEN', entry_time INTEGER NOT NULL, exit_time INTEGER,
        realized_pnl REAL DEFAULT 0, fee REAL DEFAULT 0,
        initial_stop_loss REAL DEFAULT 0, entry_price REAL NOT NULL,
        quantity REAL NOT NULL
    );
    CREATE TABLE trader_equity_snapshots (
        id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL,
        timestamp DATETIME NOT NULL, total_equity REAL NOT NULL DEFAULT 0
    );
    """)


def populate(db_path: Path) -> None:
    with closing(sqlite3.connect(db_path)) as connection:
        schema(connection)
        for trader, pnl, gain in (("candidate", 0.2, 1.5), ("control", 0.1, 0.8)):
            for i in range(50):
                exit_at = START + timedelta(days=39 * i / 49, hours=6)
                connection.execute(
                    "INSERT INTO trader_positions (trader_id,symbol,status,entry_time,exit_time,"
                    "realized_pnl,fee,initial_stop_loss,entry_price,quantity) VALUES (?,?,?,?,?,?,?,?,?,?)",
                    (trader, "BTCUSDT", "CLOSED", int(START.timestamp() * 1000),
                     int(exit_at.timestamp() * 1000), pnl, 0.02, 99, 100, 1))
            for i in range(41):
                moment = START + timedelta(days=i, hours=20)
                connection.execute(
                    "INSERT INTO trader_equity_snapshots (trader_id,timestamp,total_equity) VALUES (?,?,?)",
                    (trader, promotion.iso(moment), 1000 + gain * i))


def pairs() -> list[dict]:
    # An old pair: the prereg lacks the metric fields added 2026-11-04 and is
    # hashed over exactly the keys it carries, as the old code did.
    prereg = {**vars(pair_cli.PreregOptions("crypto_llm", "config-v1", "ensemble", "A")),
              "registered_at": "2026-09-30T00:00:00Z"}
    for key in ("metric", "margin_daily", "trade_ratio_max", "min_short_trades",
                "min_trades_scope", "extend_se_daily"):
        del prereg[key]
    prereg["prereg_hash"] = pair_cli.prereg_hash(prereg)
    assert pair_cli.verify_prereg(prereg)
    registered = {"id": "legacy-prereg", "title": "因子 ensemble", "feature": "enable_factor_ensemble",
                  "candidate_trader_id": "candidate", "control_trader_id": "control",
                  "live_trader_id": "", "started_at": promotion.iso(START),
                  "min_days": 7, "min_trades": 20, "prereg": prereg}
    plain = {**registered, "id": "legacy-plain", "prereg": None}
    del plain["prereg"]
    return [registered, plain]


def build(db_path: Path) -> dict:
    with mock.patch.object(promotion, "load_btc_returns", return_value=({}, "测试：无基准")), \
         mock.patch.object(promotion, "audit_returns", None):
        return promotion.build_report(pairs(), db_path, AS_OF)


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="golden-legacy-") as temporary:
        db_path = Path(temporary) / "fixture.db"
        populate(db_path)
        payload = json.dumps(build(db_path), ensure_ascii=False, indent=1, sort_keys=True)
    if len(sys.argv) > 1:
        Path(sys.argv[1]).write_text(payload, encoding="utf-8")
        print(f"written {sys.argv[1]} ({len(payload)} chars)")
    else:
        print(payload)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
