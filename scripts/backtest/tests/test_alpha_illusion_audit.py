import math
import unittest

import numpy as np
import pandas as pd

from pathlib import Path
import sqlite3
import tempfile

from scripts.backtest.alpha_illusion_audit import (
    classify_symbol,
    compute_ece,
    lo_sharpe_se,
    min_track_record_length,
    parse_utc_timestamps,
    run_audit,
)


class AlphaIllusionAuditTests(unittest.TestCase):
    def test_compute_ece_perfect_and_overconfident(self):
        # 50 predictions at 0.8 with 80% accuracy -> ECE = 0
        conf = np.full(50, 0.80)
        y = np.array([1.0] * 40 + [0.0] * 10)
        res = compute_ece(conf, y, n_bins=10)
        self.assertAlmostEqual(res["ece"], 0.0, places=6)

        # Overconfident: 0.80 claimed confidence, 40% realized accuracy -> ECE = 0.40
        y_bad = np.array([1.0] * 20 + [0.0] * 30)
        res_bad = compute_ece(conf, y_bad, n_bins=10)
        self.assertAlmostEqual(res_bad["ece"], 0.40, places=6)

    def test_lo_sharpe_se_and_mintrl(self):
        # Lo (2002) SE for period SR=0.1, T=100 -> sqrt((1 + 0.005)/100)
        stats = lo_sharpe_se(0.1, 100, periods_per_year=365)
        expected_se = math.sqrt((1.0 + 0.5 * 0.01) / 100.0)
        self.assertAlmostEqual(stats["se_period"], expected_se, places=10)
        self.assertAlmostEqual(stats["sr_ann"], 0.1 * math.sqrt(365), places=10)

        # MinTRL with normal returns, alpha=0.05 (z=1.64485), SR=0.1 -> ~272 periods
        mt = min_track_record_length(0.1, 0.0, skew=0.0, kurt=3.0, alpha=0.05)
        self.assertGreater(mt, 250)
        self.assertLess(mt, 300)
        self.assertEqual(min_track_record_length(0.0), math.inf)

    def test_classify_symbol(self):
        self.assertEqual(classify_symbol("AAPLBUSDT"), "tokenized_us_etf")
        self.assertEqual(classify_symbol("NVDAUSDT"), "tokenized_us_etf")
        self.assertEqual(classify_symbol("BTCUSDT"), "crypto_major")
        self.assertEqual(classify_symbol("ARIAUSDT"), "crypto_alt")

    def test_parse_utc_timestamps_mixed(self):
        s = pd.Series([1788007405533, 1788007405, "2026-08-29T12:00:00Z"])
        ts = parse_utc_timestamps(s)
        self.assertEqual(ts.dt.year.tolist(), [2026, 2026, 2026])
        self.assertTrue(ts.notna().all())

    def test_run_audit_subtracts_fees_from_gross_realized_pnl(self):
        with tempfile.TemporaryDirectory() as td:
            db_path = Path(td) / "test.db"
            conn = sqlite3.connect(str(db_path))
            conn.executescript(
                """
                CREATE TABLE traders (id TEXT, name TEXT, strategy_id TEXT, exchange_id TEXT, is_running INT);
                CREATE TABLE ai_charges (trader_id TEXT, cost_usd REAL);
                CREATE TABLE decision_records (trader_id TEXT, timestamp TEXT, decisions TEXT, execution_log TEXT);
                CREATE TABLE trader_positions (
                    id TEXT, trader_id TEXT, symbol TEXT, side TEXT,
                    entry_price REAL, exit_price REAL, initial_stop_loss REAL,
                    entry_quantity REAL, realized_pnl REAL, fee REAL,
                    entry_time TEXT, exit_time TEXT, close_reason TEXT,
                    entry_order_id TEXT, source TEXT, status TEXT
                );
                INSERT INTO traders VALUES ('t1', 'mac-nofx', 's1', 'e1', 1);
                INSERT INTO ai_charges VALUES ('t1', 1.5);
                INSERT INTO trader_positions VALUES (
                    'p1', 't1', 'BTCUSDT', 'long',
                    100.0, 101.0, 90.0,
                    10.0, 10.0, 12.0,
                    '2026-10-01T00:00:00Z', '2026-10-01T04:00:00Z', 'tp',
                    'ord1', 'ai', 'closed'
                );
                """
            )
            conn.commit()
            conn.close()

            res = run_audit(db_path)
            t = res["traders"][0]
            # realized_pnl = +10.0 is pre-fee gross PnL; fee = 12.0 => net_pnl = -2.0 (a net loss!)
            self.assertAlmostEqual(t["gross_pnl"], 10.0, places=4)
            self.assertAlmostEqual(t["fees"], 12.0, places=4)
            self.assertAlmostEqual(t["net_pnl"], -2.0, places=4)
            self.assertAlmostEqual(t["net_after_ai_usd"], -3.5, places=4)
            self.assertAlmostEqual(t["win_rate"], 0.0, places=6)
            # risk_usd = |100 - 90| * 10 = 100 => net r_mult = -2.0 / 100 = -0.02
            self.assertAlmostEqual(t["avg_r"], -0.02, places=6)


if __name__ == "__main__":
    unittest.main()
