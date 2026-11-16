import csv
import io
import json
import os
import tempfile
import unittest
from contextlib import redirect_stderr
from unittest.mock import patch

from scripts.backtest.cli import main
from scripts.backtest.exitsim import run_variants
from scripts.backtest.nt_engine import nt_version
from scripts.backtest.tests.test_exitsim import P, T


class BacktestCliTest(unittest.TestCase):
    def test_missing_framework_exits_without_fallback(self):
        err = io.StringIO()
        with patch.dict('sys.modules', {'nautilus_trader': None}), redirect_stderr(err):
            self.assertEqual(main(['exit']), 2)
        self.assertIn('不会回退到自写模拟器', err.getvalue())
        self.assertIn('requirements.txt', err.getvalue())

        err_bt = io.StringIO()
        with patch.dict('sys.modules', {'backtrader': None}), redirect_stderr(err_bt):
            self.assertEqual(main(['exit', '--engine', 'backtrader']), 2)
        self.assertIn('不会回退到自写模拟器', err_bt.getvalue())

    def test_csv_records_engine_and_actual_variant_parameters(self):
        tr = T([(100, 100, 100, 100), (100, 101, 99, 100), (100, 111, 99, 110)])
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, 'result.csv')
            run_variants([tr], P(), [['baseline', {}], ['capped', {'tp_cap': 1}]],
                         csv_path=path, commission=0, slippage=0, out=lambda _: None)
            with open(path) as f:
                rows = list(csv.DictReader(f))
            self.assertEqual([r['engine'] for r in rows], ['nautilus_trader', 'nautilus_trader'])
            self.assertTrue(all(r['engine_version'] == nt_version() for r in rows))
            self.assertEqual(json.loads(rows[1]['params'])['tp_cap'], 1)
            self.assertAlmostEqual(float(rows[0]['sumR']), 2.0, places=4)
            self.assertAlmostEqual(float(rows[1]['sumR']), 1.0, places=4)

            bt_path = os.path.join(d, 'result_bt.csv')
            run_variants([tr], P(), [['baseline', {}], ['capped', {'tp_cap': 1}]],
                         csv_path=bt_path, commission=0, slippage=0, out=lambda _: None,
                         engine='backtrader')
            with open(bt_path) as f:
                bt_rows = list(csv.DictReader(f))
            self.assertEqual([r['engine'] for r in bt_rows], ['backtrader', 'backtrader'])
            self.assertTrue(all(r['engine_version'] == '1.9.78.123' for r in bt_rows))

    def test_invalid_execution_settings_are_rejected(self):
        with redirect_stderr(io.StringIO()):
            self.assertEqual(main(['exit', '--commission', 'nan']), 2)
            self.assertEqual(main(['exit', '--limit-valid-min', '0']), 2)


if __name__ == '__main__':
    unittest.main()
