"""Advisory audit integration using the report suite's SQLite/snapshot fixtures."""

from datetime import timedelta
import json
import math
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from scripts.backtest import quant_audit
from scripts.promotion import report as promotion
from scripts.promotion.tests.test_report import NOW, SCHEMA, START, pair, snapshots


def equities(log_rets):
    curve = [100.0]
    for value in log_rets:
        curve.append(curve[-1] * math.exp(value))
    return curve


class AuditReportTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix=".test-audit-", dir=Path(__file__).parent)
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.db = self.directory / "paper.db"
        with sqlite3.connect(self.db) as connection:
            connection.executescript(SCHEMA)
        environment = patch.dict(os.environ, {"FACTOR_MINING_DIR": str(self.directory / "factors")})
        environment.start()
        self.addCleanup(environment.stop)
        self.btc = self.directory / "factors/crypto/daily/BTCUSDT.json"

    def write_btc(self, n=120, start=START):
        self.btc.parent.mkdir(parents=True, exist_ok=True)
        closes = equities([0.0007 + (0.001 if i % 2 else -0.001) for i in range(n)])
        rows = [[(start + timedelta(days=i)).date().isoformat(), close, close, close, close]
                for i, close in enumerate(closes)]
        self.btc.write_text(json.dumps(rows), encoding="utf-8")
        return rows

    def populate(self, trader_id, log_rets):
        with sqlite3.connect(self.db) as connection:
            for row in snapshots(equities(log_rets)):
                connection.execute("INSERT INTO trader_equity_snapshots (trader_id,timestamp,total_equity) VALUES (?,?,?)",
                                   (trader_id, row["timestamp"], row["total_equity"]))

    def test_seven_days_all_insufficient_and_counts_only(self):
        self.write_btc()
        self.populate("candidate", [0.002, 0.001, -0.001, 0.001, 0, 0.002, -0.001])
        self.populate("control", [0.001] * 7)
        report = promotion.build_report([pair()], self.db, NOW)
        item = report["pairs"][0]
        audit = item["audit"]
        self.assertEqual(audit["min_window_periods"], 20)
        self.assertEqual(audit["benchmark"], promotion.AUDIT_BENCHMARK)
        for key in ("candidate_vs_benchmark", "control_vs_benchmark", "candidate_vs_control"):
            with self.subTest(key=key):
                result = audit[key]
                self.assertEqual(result["verdict"], "INSUFFICIENT_SAMPLE")
                self.assertEqual(result["n_days"], 7)
                self.assertEqual(result["required"], 48)
                self.assertEqual((result["train_n"], result["holdout_n"]), (4, 3))
                self.assertFalse(result["sample_sufficient"])
                self.assertNotIn("train_cagr", result)
        self.assertIn("候选 vs BTC INSUFFICIENT_SAMPLE(7/48天)", promotion.summary(report))
        self.assertIn("候选 vs 对照 INSUFFICIENT_SAMPLE(7/48天)", promotion.summary(report))
        json.dumps(report, allow_nan=False)

    def test_120_days_three_state_verdicts_and_all_window_fields(self):
        rows = self.write_btc()
        cand_rets = [0.002 + (0.0005 if i % 2 else -0.0005) for i in range(120)]
        ctrl_rets = [0.001 + (0.001 if i % 2 else -0.001) for i in range(120)]
        self.populate("candidate", cand_rets)
        self.populate("control", ctrl_rets)
        as_of = START + timedelta(days=121)
        report = promotion.build_report([pair()], self.db, as_of)
        audit = report["pairs"][0]["audit"]
        btc_rets = [math.log(rows[i][4] / rows[i - 1][4]) for i in range(1, len(rows))]
        for key, system, benchmark in (
            ("candidate_vs_benchmark", cand_rets, btc_rets),
            ("control_vs_benchmark", ctrl_rets, btc_rets),
            ("candidate_vs_control", cand_rets, ctrl_rets),
        ):
            with self.subTest(key=key):
                result = audit[key]
                self.assertIn(result["verdict"], ("DATA_VALIDATED", "TRADE_OFF", "NO_EDGE"))
                self.assertEqual(result["n_days"], 120)
                self.assertTrue(result["sample_sufficient"])
                expected = quant_audit.audit_returns(system, benchmark, 365)
                for window, length in (("train", 72), ("holdout", 48)):
                    self.assertEqual(result[f"{window}_n"], length)
                    self.assertEqual(result[f"{window}_n_bench"], length)
                    for metric in ("cagr", "cagr_bench", "sharpe", "sharpe_bench"):
                        self.assertAlmostEqual(result[f"{window}_{metric}"], expected[f"{window}_{metric}"])
        json.dumps(report, allow_nan=False)

    def test_missing_corrupt_or_unmatched_benchmark_never_breaks_report(self):
        self.populate("candidate", [0.001] * 7)
        self.populate("control", [0.0] * 7)
        for condition in ("missing", "bad_json", "bad_row", "bad_price", "no_dates"):
            with self.subTest(condition=condition):
                self.btc.unlink(missing_ok=True)
                if condition != "missing":
                    self.write_btc(start=START - timedelta(days=200) if condition == "no_dates" else START)
                    if condition == "bad_json":
                        self.btc.write_text("{broken")
                    elif condition == "bad_row":
                        self.btc.write_text('[{"date":"2026-10-01"}]')
                    elif condition == "bad_price":
                        self.btc.write_text('[["2026-10-01", 1, 1, 1, "NaN"]]')
                result = promotion.build_report([pair()], self.db, NOW)
                audit = result["pairs"][0]["audit"]
                self.assertIsNone(audit["benchmark"])
                for key in ("candidate_vs_benchmark", "control_vs_benchmark"):
                    self.assertEqual(audit[key]["verdict"], "UNAVAILABLE")
                    self.assertTrue(audit[key]["reason"])
                self.assertEqual(audit["candidate_vs_control"]["verdict"], "INSUFFICIENT_SAMPLE")
                promotion.write_report(self.directory / "report.json", result)
                self.assertIn("UNAVAILABLE", promotion.summary(result))

    def test_only_common_completed_dates_are_used_without_filling_btc_gaps(self):
        rows = self.write_btc(n=8)
        # A missing BTC date must not turn its following return into a two-day return.
        self.btc.write_text(json.dumps([row for i, row in enumerate(rows) if i != 4]))
        self.populate("candidate", [0.001] * 8)
        self.populate("control", [0] * 8)
        as_of = START + timedelta(days=8, hours=23)
        audit = promotion.build_report([pair()], self.db, as_of)["pairs"][0]["audit"]
        self.assertEqual(audit["candidate_vs_benchmark"]["n_days"], 5)  # Days 1,2,3,6,7 only.
        self.assertEqual(audit["control_vs_benchmark"]["n_days"], 5)
        self.assertEqual(audit["candidate_vs_control"]["n_days"], 7)  # Day 8 incomplete.

    def test_append_only_audit_cannot_change_any_existing_field(self):
        self.write_btc()
        self.populate("candidate", [0.002] * 40)
        self.populate("control", [0.001] * 40)
        with sqlite3.connect(self.db) as connection:
            for trader in ("candidate", "control"):
                for _ in range(50):
                    connection.execute(
                        "INSERT INTO trader_positions (trader_id,symbol,status,entry_time,exit_time,"
                        "realized_pnl,fee,initial_stop_loss,entry_price,quantity) VALUES (?,?,?,?,?,?,?,?,?,?)",
                        (trader, "BTCUSDT", "CLOSED", int(START.timestamp() * 1000),
                         int((START + timedelta(days=1)).timestamp() * 1000), 3, 1, 99, 100, 2))
        as_of = START + timedelta(days=41)
        audited = promotion.build_report([pair()], self.db, as_of)
        self.assertEqual(audited["pairs"][0]["verdict"], "ready")
        self.btc.unlink()
        missing = promotion.build_report([pair()], self.db, as_of)
        for report in (audited, missing):
            del report["pairs"][0]["audit"]
        self.assertEqual(audited, missing)

    def test_factor_dir_override_and_main_checkout_default(self):
        self.write_btc()
        with patch.object(promotion, "main_repo_root", return_value=self.directory) as resolve_root:
            # The env override wins even if the main checkout has no data folder.
            returns, error = promotion.load_btc_returns(NOW)
            self.assertIsNone(error)
            self.assertTrue(returns)
            resolve_root.assert_not_called()
            default = self.directory / "data/factor_mining/crypto/daily/BTCUSDT.json"
            default.parent.mkdir(parents=True)
            default.write_bytes(self.btc.read_bytes())
            with patch.dict(os.environ):
                os.environ.pop("FACTOR_MINING_DIR", None)
                self.assertEqual(promotion.load_btc_returns(NOW), (returns, None))


if __name__ == "__main__":
    unittest.main()


class AuditImportFallbackTests(unittest.TestCase):
    """The nightly runtime dir may lack quant_audit.py: the report must degrade, not crash."""

    def test_pair_audit_degrades_when_module_missing(self):
        import scripts.promotion.report as report
        stats = {"daily_equity": []}
        original = report.audit_returns
        report.audit_returns = None
        try:
            result = report.pair_audit(stats, stats, report.as_utc("2026-10-24T00:00:00Z"), {}, None)
        finally:
            report.audit_returns = original
        for key in ("candidate_vs_benchmark", "control_vs_benchmark", "candidate_vs_control"):
            self.assertEqual(result[key]["verdict"], "UNAVAILABLE")
