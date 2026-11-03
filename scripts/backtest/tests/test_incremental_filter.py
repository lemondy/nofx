"""Deterministic type-B screens and read-only SQLite boundaries; no live DB."""

from contextlib import closing, redirect_stderr, redirect_stdout
from datetime import datetime, timedelta
import hashlib
import io
import json
from pathlib import Path
import random
import sqlite3
from statistics import stdev
import tempfile
import unittest
from unittest.mock import patch

from scripts.backtest import evolution_ledger as ledger
from scripts.backtest import incremental_filter as filters
from scripts.promotion import report


START = datetime(2026, 10, 5, tzinfo=report.UTC)
SCHEMA = """
CREATE TABLE trader_positions (
    id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL, symbol TEXT NOT NULL,
    side TEXT NOT NULL DEFAULT 'LONG', status TEXT DEFAULT 'OPEN',
    entry_time INTEGER NOT NULL, exit_time INTEGER,
    realized_pnl REAL DEFAULT 0, fee REAL DEFAULT 0,
    initial_stop_loss REAL DEFAULT 0, entry_price REAL NOT NULL,
    quantity REAL NOT NULL, entry_quantity REAL DEFAULT 999
);
CREATE TABLE trader_equity_snapshots (
    id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL,
    timestamp DATETIME NOT NULL, total_equity REAL NOT NULL DEFAULT 0
);
CREATE TABLE gate_shadow_blocks (
    id INTEGER PRIMARY KEY, trader_id TEXT, symbol TEXT, direction TEXT,
    blocked_codes TEXT, entry_price REAL, stop_price REAL, take_profit REAL,
    plan_rr REAL, horizon_hours INTEGER, created_at DATETIME,
    outcome TEXT, exit_price REAL, evaluated_at DATETIME,
    outcome_48h TEXT, exit_price_48h REAL, evaluated_at_48h DATETIME, entry_basis TEXT
);
"""


def trades(n=400):
    # Both chronological halves contain equal numbers of winners and losers.
    return [{"trade_id": i + 1, "trader_id": "a", "symbol": "BTCUSDT", "side": "LONG",
             "entry_time": report.iso(START + timedelta(hours=i)),
             "exit_time": report.iso(START + timedelta(hours=i + 1)),
             "entry_price": 100, "initial_stop_loss": 90,
             "r": -1.0 if i % 2 == 0 else 1.0, "net_pnl_usdt": -10 if i % 2 == 0 else 10}
            for i in range(n)]


def worst_mask(rows):
    return {str(row["trade_id"]): row["r"] < 0 for row in rows}


def shadow_row(**extra):
    return {"trader_id": "a", "symbol": "BTCUSDT", "direction": "long",
            "blocked_codes": "A", "entry_price": 100.0, "stop_price": 90.0,
            "take_profit": 120.0, "plan_rr": 2.0, "horizon_hours": 8,
            "created_at": report.iso(START), "outcome": "tp_first", "exit_price": 120.0,
            "outcome_48h": "timeout", "exit_price_48h": 105.0,
            "entry_basis": "live_price", **extra}


class StatisticsTests(unittest.TestCase):
    def test_placebo_deterministic_fixed_count_and_local_rng(self):
        values, p, seed, n = list(range(-20, 20)), 0.234, 20261103, 50
        global_state = random.getstate()
        actual = filters.placebo_distribution(values, p, seed, n)
        self.assertEqual(actual, filters.placebo_distribution(values, p, seed, n))
        self.assertNotEqual(actual, filters.placebo_distribution(values, p, seed + 1, n))
        self.assertEqual(random.getstate(), global_state)
        rng = random.Random(seed)
        expected = []
        for _ in range(n):
            dropped = set(rng.sample(range(len(values)), round(p * len(values))))
            kept = [r for i, r in enumerate(values) if i not in dropped]
            expected.append(report.mean(kept) - report.mean(values))
        self.assertEqual(actual, expected)

    def test_worst_drops_have_high_percentile_and_positive_halves(self):
        rows = trades()
        mask = worst_mask(rows)
        result = filters.filter_statistics(list(reversed(rows)), mask)
        self.assertEqual(result["n"], 400)
        self.assertEqual(result["p"], 0.5)
        self.assertEqual(result["trade_ratio"], 0.5)
        self.assertEqual(result["delta"], 1.0)
        self.assertEqual(result["dev_delta"], 1.0)
        self.assertEqual(result["holdout_delta"], 1.0)
        self.assertGreater(result["placebo_pct"], 0.99)
        self.assertGreater(result["z"], 10)
        self.assertAlmostEqual(result["se"], stdev(filters.placebo_distribution([r["r"] for r in rows], 0.5, 20261103, 2000)))
        self.assertAlmostEqual(result["z"], result["delta"] / result["se"])
        self.assertEqual(result["total_r_change"], 200)
        self.assertEqual(result["dropped_avg_r"], -1)
        self.assertEqual(result["dropped_avg_r_ci95"], [-1, -1])
        self.assertEqual(filters.decide_verdict(result, 1), "strong")

    def test_random_mask_is_near_middle_of_placebo(self):
        rows = trades()
        selected = set(random.Random(7).sample(range(1, 401), 100))
        mask = {str(row["trade_id"]): row["trade_id"] in selected for row in rows}
        result = filters.filter_statistics(rows, mask)
        self.assertGreater(result["placebo_pct"], 0.25)
        self.assertLess(result["placebo_pct"], 0.75)
        self.assertLess(abs(result["z"]), 1)

    def test_fewer_trades_rate_rule_precedes_strong(self):
        rows = trades()
        mask = {str(row["trade_id"]): row["r"] < 0 or row["trade_id"] <= 4 for row in rows}
        stats = filters.filter_statistics(rows, mask)
        self.assertGreater(stats["p"], 0.5)
        self.assertGreater(stats["z"], filters.z_threshold(1))
        self.assertGreater(stats["placebo_pct"], 0.9)
        self.assertEqual(filters.decide_verdict(stats, 1), "fewer_trades")

    def test_total_r_improves_by_trading_less_and_strict_placebo_ties(self):
        rows = [{**row, "r": -1.0} for row in trades()]
        mask = {str(row["trade_id"]): i % 4 == 0 for i, row in enumerate(rows)}
        stats = filters.filter_statistics(rows, mask)
        self.assertEqual(stats["p"], 0.25)
        self.assertEqual(stats["delta"], 0)
        self.assertEqual(stats["se"], 0)
        self.assertEqual(stats["z"], 0)
        self.assertEqual(stats["placebo_pct"], 0)  # Ties are not strictly below.
        self.assertEqual(stats["total_r_change"], 100)
        self.assertEqual(filters.decide_verdict(stats, 1), "fewer_trades")

    def test_strict_percentile_does_not_rank_float_cancellation_as_improvement(self):
        rows = [{**row, "r": 0.1} for row in trades(100)]
        mask = {str(row["trade_id"]): i < 3 for i, row in enumerate(rows)}
        stats = filters.filter_statistics(rows, mask)
        self.assertEqual(stats["delta"], 0)
        self.assertEqual(stats["placebo_pct"], 0)
        self.assertEqual(stats["se"], 0)
        self.assertEqual(filters.decide_verdict(stats, 1), "fail")

    def test_invalid_paths_and_coverage_boundary(self):
        for n, drop in ((0, False), (99, True), (100, False), (100, True)):
            with self.subTest(n=n, drop=drop):
                rows = trades(n)
                stats = filters.filter_statistics(rows, {str(row["trade_id"]): drop for row in rows})
                self.assertEqual(filters.decide_verdict(stats, 1), "invalid")
        rows = trades(200)
        mask = worst_mask(rows)
        for row in rows[:10]:
            del mask[str(row["trade_id"])]
        stats = filters.filter_statistics(rows, mask)
        self.assertEqual(stats["mask_missing"], 10)
        self.assertEqual(stats["mask_coverage_error"], 0.05)
        self.assertNotEqual(filters.decide_verdict(stats, 1), "invalid")
        self.assertEqual(stats["dropped_n"], 95)  # Missing IDs are kept.
        del mask[str(rows[10]["trade_id"])]
        with patch.object(filters, "placebo_distribution", side_effect=AssertionError("must abort")):
            stats = filters.filter_statistics(rows, mask)
        self.assertEqual(stats["mask_missing"], 11)
        self.assertIsNone(stats["delta"])
        self.assertEqual(filters.decide_verdict(stats, 1), "invalid")
        rows = trades(99)
        self.assertEqual(filters.decide_verdict(filters.filter_statistics(rows, worst_mask(rows)), 1), "invalid")

    def test_verdict_boundaries_and_order(self):
        base = dict(n=100, p=0.5, mask_coverage_error=0.05, z=1.0, placebo_pct=0.90,
                    dev_delta=0.1, holdout_delta=0.1, total_r_change=-1)
        self.assertEqual(filters.decide_verdict(base, 1), "screen_pass")
        self.assertEqual(filters.decide_verdict({**base, "z": filters.z_threshold(1)}, 1), "strong")
        self.assertEqual(filters.decide_verdict({**base, "z": 0.999}, 1), "fail")
        self.assertEqual(filters.decide_verdict({**base, "holdout_delta": 0}, 1), "fail")
        self.assertEqual(filters.decide_verdict({**base, "dev_delta": None}, 1), "fail")
        self.assertEqual(filters.decide_verdict({**base, "placebo_pct": 0.899}, 1), "fail")
        self.assertEqual(filters.decide_verdict({**base, "placebo_pct": 0.899, "total_r_change": 1}, 1), "fewer_trades")

    def test_half_with_no_kept_trades_cannot_pass(self):
        rows = trades()
        mask = {str(row["trade_id"]): i < len(rows) // 2 for i, row in enumerate(rows)}
        stats = filters.filter_statistics(rows, mask)
        self.assertIsNone(stats["dev_delta"])
        self.assertEqual(filters.decide_verdict(stats, 1), "fail")

    def test_weekly_cluster_influences_and_utc_monday(self):
        samples = [(START, 1), (START, 3), (START + timedelta(days=7), 4),
                   (report.as_utc("2026-10-19T00:30:00+08:00"), 8)]
        stats = filters.weekly_statistics(samples)
        # Last timestamp is UTC Sunday, still in the second week.
        self.assertEqual(stats["n_weeks"], 2)
        self.assertEqual(stats["avg_r"], 4)
        # Two clusters: no SE/CI, matching report.weekly_r_comparison.
        self.assertIsNone(stats["se"])
        self.assertIsNone(stats["ci95"])
        self.assertIsNone(filters.weekly_statistics(samples[:2])["ci95"])
        self.assertIsNone(filters.weekly_statistics([])["avg_r"])

    def test_placebo_n_must_support_sample_standard_deviation(self):
        with self.assertRaises(ValueError):
            filters.placebo_distribution([1, 2, 3], 0.3, 1, 1)


class FileTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.trades_path = self.root / "trades.jsonl"
        self.mask_path = self.root / "mask.jsonl"
        self.ledger_path = self.root / "ledger.jsonl"
        self.rows = trades()
        self.write_inputs(self.rows, worst_mask(self.rows))

    def write_inputs(self, rows, mask):
        self.trades_path.write_text("".join(json.dumps(row) + "\n" for row in rows))
        self.mask_path.write_text("".join(json.dumps({"trade_id": key, "drop": drop}) + "\n" for key, drop in mask.items()))

    def args(self, *extra):
        return ["evaluate", "--trades", str(self.trades_path), "--mask", str(self.mask_path),
                "--idea-id", "test-filter", "--hypothesis", "过滤最差开仓", "--track", "crypto_llm",
                "--baseline-version", "hash0", "--ledger", str(self.ledger_path),
                "--placebo-n", "100", *extra]

    def run_cli(self, *extra):
        stdout, stderr = io.StringIO(), io.StringIO()
        with redirect_stdout(stdout), redirect_stderr(stderr):
            status = filters.main(self.args(*extra))
        return status, stdout.getvalue(), stderr.getvalue()

    def test_k_threshold_grows_from_existing_track_rows_and_verdict_uses_it(self):
        def old(**extra):
            return dict(stage="S1", change_type="B", idea_id="old", baseline_version="h0", verdict="fail", **extra)

        ledger.append_event(self.ledger_path, "crypto_llm", lambda *_: old())
        ledger.append_event(self.ledger_path, "crypto_llm", lambda *_: {**old(), "verdict": "invalid"})
        ledger.append_event(self.ledger_path, "crypto_llm", lambda *_: {**old(), "stage": "S2"})
        ledger.append_event(self.ledger_path, "us_llm", lambda *_: old())
        before = self.ledger_path.read_bytes()
        stats = filters.filter_statistics(self.rows, worst_mask(self.rows), placebo_n=100)
        stats["z"] = 2.1  # Strong at K=1, screen_pass at K=2.
        self.assertEqual(filters.decide_verdict(stats, 1), "strong")
        with patch.object(filters, "filter_statistics", return_value=stats):
            status, output, error = self.run_cli()
        self.assertEqual(status, 0, error)
        row = ledger.read_rows(self.ledger_path)[-1]
        self.assertEqual(row["K"], 2)
        self.assertGreater(row["z_K"], filters.z_threshold(1))
        self.assertEqual(row["z_K"], filters.z_threshold(2))
        self.assertEqual(row["verdict"], "screen_pass")
        self.assertEqual(row["baseline_version"], "hash0")
        self.assertEqual(row["stage"], "S1")
        self.assertEqual(row["change_type"], "B")
        self.assertTrue(self.ledger_path.read_bytes().startswith(before))
        self.assertEqual(row["trades_sha256"], hashlib.sha256(self.trades_path.read_bytes()).hexdigest())
        self.assertEqual(row["mask_sha256"], hashlib.sha256(self.mask_path.read_bytes()).hexdigest())
        self.assertTrue(output.rstrip().endswith(filters.QUEUE_NOTE))

    def test_no_ledger_only_prints_and_reads_existing_k(self):
        status, output, error = self.run_cli("--no-ledger")
        self.assertEqual(status, 0, error)
        self.assertFalse(self.ledger_path.exists())
        self.assertIn("仅打印预览", output)
        ledger.append_event(self.ledger_path, "crypto_llm", lambda *_: dict(
            stage="S1", change_type="B", idea_id="old", baseline_version="h0", verdict="fail"))
        before = self.ledger_path.read_bytes()
        status, output, error = self.run_cli("--no-ledger")
        self.assertEqual(status, 0, error)
        self.assertIn("K=2", output)
        self.assertEqual(before, self.ledger_path.read_bytes())

    def test_coverage_abort_records_invalid_without_using_k(self):
        mask = worst_mask(self.rows)
        for row in self.rows[:21]:
            del mask[str(row["trade_id"])]
        self.write_inputs(self.rows, mask)
        status, output, error = self.run_cli()
        self.assertEqual(status, 2, error)
        row = ledger.read_rows(self.ledger_path)[-1]
        self.assertEqual(row["verdict"], "invalid")
        self.assertIsNone(row["K"])
        self.assertIsNone(row["delta"])
        self.assertEqual(row["mask_missing"], 21)
        self.assertEqual(ledger.next_k(ledger.read_rows(self.ledger_path), "crypto_llm"), 1)
        self.assertIn("掩码缺失=21", output)
        self.assertTrue(output.rstrip().endswith(filters.QUEUE_NOTE))

    def test_invalid_inputs_do_not_append(self):
        for payload in ('{"trade_id":1,"drop":"false"}\n',
                        '{"trade_id":1,"drop":false}\n{"trade_id":"1","drop":true}\n',
                        '[]\n', 'bad json\n'):
            with self.subTest(payload=payload):
                self.mask_path.write_text(payload)
                status, _, error = self.run_cli()
                self.assertEqual(status, 2)
                self.assertTrue(error)
                self.assertFalse(self.ledger_path.exists())
        for row in ({**self.rows[0], "r": float("inf")},
                    {**self.rows[0], "entry_time": "bad"}):
            self.write_inputs([row], {"1": True})
            self.assertEqual(self.run_cli()[0], 2)
        self.write_inputs([self.rows[0], self.rows[0]], {"1": True})
        self.assertEqual(self.run_cli()[0], 2)


class DatabaseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.db = self.root / "fixture.db"
        self.connection = sqlite3.connect(self.db)
        self.connection.row_factory = sqlite3.Row
        self.addCleanup(self.connection.close)
        self.connection.executescript(SCHEMA)

    def position(self, **extra):
        row = {"trader_id": "a", "symbol": "BTCUSDT", "side": "LONG", "status": "CLOSED",
               "entry_time": int(START.timestamp() * 1000),
               "exit_time": int((START + timedelta(days=1)).timestamp() * 1000),
               "realized_pnl": 3.0, "fee": 1.0, "initial_stop_loss": 99.0,
               "entry_price": 100.0, "quantity": 2.0, **extra}
        self.insert("trader_positions", row)

    def insert(self, table, row):
        self.connection.execute(f"INSERT INTO {table} ({','.join(row)}) VALUES ({','.join('?' for _ in row)})", tuple(row.values()))

    def test_export_r_matches_report_exclusions_fees_quantity_and_extremes(self):
        self.position()
        self.position(realized_pnl=0.5)
        self.position(initial_stop_loss=0)
        self.position(initial_stop_loss=None)
        self.position(initial_stop_loss=100)
        self.position(initial_stop_loss=100 + 1e-10)
        self.position(quantity=0)
        self.position(quantity=-1)
        self.position(realized_pnl=float("inf"))
        self.position(fee=None)
        self.position(realized_pnl=69)
        self.position(side="SHORT", initial_stop_loss=101, realized_pnl=-31)
        raw = [dict(row) for row in self.connection.execute("SELECT * FROM trader_positions")]
        expected, samples = report.trade_statistics(raw)
        exported, counts = filters.read_closed_trades(self.connection, ["a"], START, START + timedelta(days=2))
        reported, _ = report.read_trader(self.connection, "a", START, START + timedelta(days=2))
        self.assertEqual(counts["total"], len(raw))
        self.assertEqual(counts["exported"], expected["r_n"])
        self.assertEqual(counts["excluded_reasons"], expected["r_excluded_reasons"])
        self.assertEqual([row["r"] for row in exported], [r for _, r in samples])
        self.assertEqual([row["net_pnl_usdt"] for row in exported], [2, -0.5, 68, -32])
        self.assertEqual([row["r"] for row in exported], [1, -0.25, 34, -16])
        self.assertEqual(report.mean(row["r"] for row in exported), reported["avg_r"])
        self.assertEqual(exported[-1]["side"], "SHORT")
        self.assertTrue(all(row["entry_time"].endswith("Z") and row["exit_time"].endswith("Z") for row in exported))

    def test_export_entry_window_repeatable_traders_and_readonly_cli(self):
        until = START + timedelta(days=2)
        self.position(trader_id="a", exit_time=int((until + timedelta(days=2)).timestamp() * 1000))
        self.position(trader_id="b")
        self.position(trader_id="other")
        self.position(entry_time=int((START - timedelta(milliseconds=1)).timestamp() * 1000))
        self.position(entry_time=int(until.timestamp() * 1000))
        self.position(status="OPEN")
        self.connection.commit()
        before = self.db.read_bytes()
        out = self.root / "export/trades.jsonl"
        stdout = io.StringIO()
        with redirect_stdout(stdout):
            status = filters.main(["export-trades", "--db", str(self.db), "--trader", "a", "--trader", "b",
                                   "--since", "2026-10-05", "--until", "2026-10-07", "--out", str(out)])
        self.assertEqual(status, 0)
        exported = [json.loads(line) for line in out.read_text().splitlines()]
        self.assertEqual([row["trader_id"] for row in exported], ["a", "b"])
        self.assertEqual(set(exported[0]), {"trade_id", "trader_id", "symbol", "side", "entry_time", "exit_time",
                                           "entry_price", "initial_stop_loss", "r", "net_pnl_usdt"})
        self.assertEqual(self.db.read_bytes(), before)
        with closing(report.open_readonly(self.db)) as readonly:
            self.assertEqual(readonly.execute("PRAGMA query_only").fetchone()[0], 1)
            with self.assertRaises(sqlite3.OperationalError):
                readonly.execute("DELETE FROM trader_positions")

    def test_shadow_sl_tp_timeout_both_directions_and_horizons_from_sqlite(self):
        cases = [(shadow_row(outcome="sl_first", exit_price=0), -1, 0.5),
                 (shadow_row(), 2, 0.5),
                 (shadow_row(outcome="timeout", exit_price=95), -0.5, 0.5),
                 (shadow_row(direction="short", stop_price=110, take_profit=70,
                             outcome="tp_first", outcome_48h="sl_first"), 3, -1),
                 (shadow_row(direction="short", stop_price=110, take_profit=70,
                             outcome="timeout", exit_price=90, outcome_48h="tp_first"), 1, 3)]
        for row, _, _ in cases:
            self.insert("gate_shadow_blocks", row)
        for raw, (_, expected8, expected48) in zip(self.connection.execute("SELECT * FROM gate_shadow_blocks"), cases):
            self.assertEqual(filters.shadow_r(dict(raw), 8), (expected8, None))
            self.assertEqual(filters.shadow_r(dict(raw), 48), (expected48, None))

    def test_shadow_unresolved_unfilled_and_invalid_stops_are_counted(self):
        for outcome in (None, "", "no_data", "unfilled", "unknown"):
            self.insert("gate_shadow_blocks", shadow_row(outcome=outcome))
        for stop in (None, 0, -1, 100, 100.00000001, 110):
            self.insert("gate_shadow_blocks", shadow_row(stop_price=stop))
        result = filters.shadow_analysis(self.connection, since=START, min_n=1, as_of=START + timedelta(days=1))
        self.assertEqual(result["total"], 11)
        self.assertEqual(result["codes"], [])
        self.assertEqual(result["excluded_reasons"], {"unresolved": 2, "no_data": 1, "unfilled": 1, "unknown": 1, "invalid_stop": 6})
        self.assertEqual(filters.shadow_r(shadow_row(outcome_48h=""), 48), (None, "unresolved"))
        self.assertEqual(filters.shadow_r(shadow_row(direction="short", stop_price=90))[1], "invalid_stop")
        self.assertEqual(filters.shadow_r(shadow_row(outcome="timeout", exit_price=0))[1], "invalid_exit_or_tp")

    def test_shadow_code_groups_same_traders_period_and_false_block_flag(self):
        end = START + timedelta(days=22)
        for week in range(3):
            timestamp = report.iso(START + timedelta(days=7 * week))
            self.insert("gate_shadow_blocks", shadow_row(blocked_codes="A, B,A", created_at=timestamp))
            self.insert("gate_shadow_blocks", shadow_row(trader_id="b", blocked_codes="B", created_at=timestamp,
                                                          outcome="timeout", exit_price=100))
        self.position(realized_pnl=-1, fee=1)  # a: -1R
        self.position(trader_id="b", realized_pnl=7, fee=1)  # b: +3R
        self.position(trader_id="other", realized_pnl=201)
        self.position(realized_pnl=201, entry_time=int((START - timedelta(seconds=1)).timestamp() * 1000))
        self.position(realized_pnl=201, entry_time=int(end.timestamp() * 1000))
        self.insert("gate_shadow_blocks", shadow_row(created_at="2026-10-05T00:30:00+08:00"))  # UTC before since.
        self.insert("gate_shadow_blocks", shadow_row(created_at=report.iso(end)))
        result = filters.shadow_analysis(self.connection, since=START, as_of=end, min_n=3)
        a, b = result["codes"]
        self.assertEqual(result["total"], 6)
        self.assertEqual((a["code"], a["n"], a["avg_r"]), ("A", 3, 2))
        self.assertEqual(a["ci95"], [2, 2])
        self.assertEqual(a["executed_avg_r"], -1)
        self.assertEqual(a["executed_n"], 1)
        self.assertTrue(a["possible_false_block"])
        self.assertEqual((b["code"], b["n"], b["avg_r"]), ("B", 6, 1))
        self.assertEqual(b["executed_avg_r"], 1)
        self.assertEqual(b["executed_n"], 2)
        self.assertFalse(b["possible_false_block"])
        restricted = filters.shadow_analysis(self.connection, ["a"], START, as_of=end, min_n=4)
        self.assertEqual(restricted["codes"], [])
        self.assertEqual(restricted["below_min_n_codes"], 2)

    def test_shadow_cli_caveat_and_no_ledger(self):
        self.insert("gate_shadow_blocks", shadow_row())
        self.position()
        self.connection.commit()
        before = self.db.read_bytes()
        with redirect_stdout(io.StringIO()) as stdout, patch.object(ledger, "append_event", side_effect=AssertionError("descriptive only")):
            status = filters.main(["shadow", "--db", str(self.db), "--since", "2026-10-05", "--horizon", "48", "--min-n", "1"])
        self.assertEqual(status, 0)
        self.assertIn("0.500000", stdout.getvalue())
        self.assertTrue(stdout.getvalue().rstrip().endswith(filters.SHADOW_NOTE))
        self.assertEqual(self.db.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()


class ReviewFixTests(unittest.TestCase):
    def test_code_family_collapses_numeric_parameters(self):
        from scripts.backtest.incremental_filter import code_family
        self.assertEqual(code_family("NEG_EDGE_SCORE_+0_LT_80"), "NEG_EDGE_SCORE_#_LT_#")
        self.assertEqual(code_family("NEG_EDGE_RR_1.29_LT_2.0"), "NEG_EDGE_RR_#_LT_#")
        self.assertEqual(code_family("VOL_HOLDOFF_30m"), "VOL_HOLDOFF_30m")
        self.assertEqual(code_family("NEG_EDGE_TREND_MISALIGNED"), "NEG_EDGE_TREND_MISALIGNED")

    def test_weekly_statistics_needs_three_weeks_for_ci(self):
        from datetime import datetime, timezone
        from scripts.backtest.incremental_filter import weekly_statistics
        two = [(datetime(2026, 10, d, tzinfo=timezone.utc), r) for d, r in ((5, 1.), (6, -1.), (12, .5), (13, .2))]
        self.assertIsNone(weekly_statistics(two)["ci95"])
        three = two + [(datetime(2026, 10, 19, tzinfo=timezone.utc), .3)]
        self.assertIsNotNone(weekly_statistics(three)["ci95"])
