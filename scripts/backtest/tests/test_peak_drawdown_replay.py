import contextlib
import hashlib
import io
import json
import sqlite3
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from scripts.backtest import peak_drawdown_replay as replay


def snapshots(equities):
    return [
        {"timestamp": f"2026-11-01 {index:02d}:00:00+00:00", "total_equity": equity}
        for index, equity in enumerate(equities)
    ]


class PeakDrawdownReplayTest(unittest.TestCase):
    def test_first_trigger_counts_and_strict_reset(self):
        curve = snapshots([
            10000, 13000, 11700, 11700, 11800, 11700, 11050, 10400,
            12000, 9000, 8500, 8000, 9100, 9000, 10000, 13000,
        ])
        result = replay.replay_trader(
            {"id": "llm", "name": "测试", "initial_balance": 10000}, curve
        )
        for basis, cap, first, count in [
            ("initial", 10, 9, 2),
            ("initial", 15, 10, 1),
            ("initial", 20, 11, 1),
            ("peak", 10, 2, 3),
            ("peak", 15, 6, 2),
            ("peak", 20, 7, 2),
        ]:
            with self.subTest(basis=basis, cap=cap):
                self.assertEqual(result["caps"][basis][str(cap)], {
                    "first_trigger_at": curve[first]["timestamp"],
                    "trigger_count": count,
                })
        self.assertEqual(result["max_initial_drawdown_pct"], 20)
        self.assertAlmostEqual(result["max_peak_drawdown_pct"], 5000 / 13000 * 100)

    def test_peak_uses_only_past_equity_and_never_decreases(self):
        curve = snapshots([10000, 13000, 11500, 12000, 14000, 12500, 15000])
        result = replay.replay_trader(
            {"id": "llm", "name": "测试", "initial_balance": 10000}, curve
        )
        self.assertEqual(result["caps"]["initial"]["10"]["trigger_count"], 0)
        peak = result["caps"]["peak"]["10"]
        self.assertEqual(peak["first_trigger_at"], curve[2]["timestamp"])
        self.assertEqual(peak["trigger_count"], 2)
        self.assertEqual(result["max_initial_drawdown_pct"], 0)
        self.assertAlmostEqual(result["max_peak_drawdown_pct"], 1500 / 13000 * 100)

    def test_initial_floor_zero_and_negative_equity(self):
        curve = snapshots([9000, 0, -100, 10000, 9000])
        result = replay.replay_trader(
            {"id": "llm", "name": "测试", "initial_balance": 10000}, curve
        )
        for basis in ("initial", "peak"):
            self.assertEqual(result["caps"][basis]["10"]["first_trigger_at"], curve[0]["timestamp"])
            self.assertEqual(result["caps"][basis]["10"]["trigger_count"], 2)
        self.assertEqual(result["max_peak_drawdown_pct"], 101)
        self.assertEqual(result["max_initial_drawdown_pct"], 101)

    def test_invalid_initial_and_snapshots_are_disclosed(self):
        for initial in (0, -1, None, float("inf")):
            with self.subTest(initial=initial):
                result = replay.replay_trader(
                    {"id": "bad", "name": "测试", "initial_balance": initial},
                    snapshots([10000, 8000]),
                )
                self.assertEqual(result["status"], "invalid_initial_balance")
                self.assertIsNone(result["max_peak_drawdown_pct"])
                self.assertEqual(result["caps"]["initial"]["10"]["trigger_count"], 0)
                json.dumps(result, allow_nan=False)
        for curve in ([], snapshots([None, float("inf"), float("nan")])):
            result = replay.replay_trader(
                {"id": "empty", "name": "测试", "initial_balance": 10000}, curve
            )
            self.assertIsNone(result["max_peak_drawdown_pct"])
            self.assertEqual(result["invalid_snapshot_count"], len(curve))
        result = replay.replay_trader(
            {"id": "skip", "name": "测试", "initial_balance": 10000},
            snapshots([9000, None, 9000]),
        )
        self.assertEqual(result["caps"]["initial"]["10"]["trigger_count"], 1)

    def make_database(self, root):
        path = root / "source #with?spaces.db"
        with sqlite3.connect(path) as connection:
            connection.executescript("""
                CREATE TABLE traders (id TEXT PRIMARY KEY, name TEXT, initial_balance REAL);
                CREATE TABLE trader_equity_snapshots (
                    id INTEGER PRIMARY KEY, trader_id TEXT, timestamp TEXT, total_equity REAL
                );
                INSERT INTO traders VALUES ('a', '交易员|甲', 10000), ('b', '乙', 20000),
                    ('empty', '无快照', 10000), ('bad', '无初始资金', 0);
                INSERT INTO trader_equity_snapshots VALUES
                    (3, 'a', '2026-11-01 02:00:00+00:00', 11500),
                    (2, 'a', '2026-11-01 01:00:00+00:00', 13000),
                    (1, 'a', '2026-11-01 00:00:00+00:00', 10000),
                    (4, 'b', '2026-11-01 00:00:00+00:00', 17000),
                    (5, 'orphan', '2026-11-01 00:00:00+00:00', 10000),
                    (6, 'a', '2026-11-01 02:00:00+00:00', 13000),
                    (7, 'a', '2026-11-01 02:00:00+00:00', 11500);
            """)
        return path

    def test_database_ordering_per_trader_and_outputs(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = self.make_database(root)
            before = hashlib.sha256(path.read_bytes()).hexdigest()
            with contextlib.redirect_stdout(io.StringIO()) as output:
                replay.main(["--db", str(path), "--out", str(root / "out")])
            report = json.loads((root / "out" / "peak_drawdown_replay.json").read_text())
            traders = {row["trader_id"]: row for row in report["traders"]}
            self.assertEqual(traders["a"]["caps"]["peak"]["10"], {
                "first_trigger_at": "2026-11-01 02:00:00+00:00", "trigger_count": 2,
            })
            self.assertEqual(traders["a"]["caps"]["initial"]["10"]["trigger_count"], 0)
            self.assertEqual(traders["b"]["caps"]["initial"]["15"]["trigger_count"], 1)
            self.assertEqual(traders["empty"]["snapshot_count"], 0)
            self.assertEqual(report["orphan_trader_ids"], ["orphan"])
            markdown = (root / "out" / "peak_drawdown_replay.md").read_text()
            self.assertEqual(markdown, output.getvalue())
            self.assertIn("最大峰值回撤", markdown)
            self.assertIn("交易员\\|甲", markdown)
            self.assertEqual(hashlib.sha256(path.read_bytes()).hexdigest(), before)

    def test_readonly_uri_rejects_writes_and_does_not_create_missing_db(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = self.make_database(root)
            with patch.object(replay.sqlite3, "connect", wraps=sqlite3.connect) as connect:
                connection = replay.open_readonly(path)
            connect.assert_called_once_with(path.resolve().as_uri() + "?mode=ro", uri=True)
            with contextlib.closing(connection):
                with self.assertRaisesRegex(sqlite3.OperationalError, "readonly"):
                    connection.execute("UPDATE traders SET initial_balance = 1")
            missing = root / "missing.db"
            with self.assertRaises(sqlite3.OperationalError):
                replay.open_readonly(missing)
            self.assertFalse(missing.exists())


if __name__ == "__main__":
    unittest.main()
