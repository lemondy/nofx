"""Offline unit tests for scripts.backtest.nas_klines (no network/DB access)."""
import datetime as dt
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.backtest import nas_klines


class StubClient:
    def __init__(self, cols=(), rows=()):
        self.cols = list(cols)
        self.rows = [list(r) for r in rows]
        self.queries = []

    def query(self, sql):
        self.queries.append(sql)
        return self.cols, self.rows


class NasKlinesTests(unittest.TestCase):
    def test_load_config_reads_dotenv_and_env_override(self):
        with tempfile.TemporaryDirectory() as td:
            env_file = Path(td) / '.env'
            env_file.write_text(
                'NAS_MYSQL_HOST=192.168.50.244\n'
                'NAS_MYSQL_PORT=3307\n'
                'NAS_MYSQL_USER=nofx\n'
                'NAS_MYSQL_PASSWORD="secret"\n'
                'NAS_MYSQL_DB=nofx\n',
                encoding='utf-8',
            )
            with patch.dict(os.environ, {}, clear=True):
                cfg = nas_klines.load_config(env_path=env_file)
                self.assertEqual(cfg.host, '192.168.50.244')
                self.assertEqual(cfg.port, 3307)
                self.assertEqual(cfg.user, 'nofx')
                self.assertEqual(cfg.password, 'secret')
                self.assertEqual(cfg.database, 'nofx')
            with patch.dict(os.environ, {'NAS_MYSQL_PORT': '3308', 'NAS_MYSQL_PASSWORD': 'override'}, clear=True):
                cfg2 = nas_klines.load_config(env_path=env_file)
                self.assertEqual(cfg2.port, 3308)
                self.assertEqual(cfg2.password, 'override')

    def test_readonly_client_blocks_mutating_sql(self):
        cli = nas_klines.ReadOnlyMysqlClient(nas_klines.NasMysqlConfig(password='pw'))
        for bad in ('DELETE FROM crypto_klines', 'UPDATE us_daily SET close=0', 'DROP TABLE us_daily', 'INSERT INTO x VALUES (1)'):
            with self.assertRaises(ValueError):
                cli.query(bad)

    def test_validate_ident_rejects_sql_injection(self):
        with self.assertRaises(ValueError):
            nas_klines.fetch_crypto_bars("BTCUSDT'; DROP TABLE x; --", client=StubClient())
        with self.assertRaises(ValueError):
            nas_klines.fetch_us_daily("AAPL' OR '1'='1", client=StubClient())

    def test_crypto_formats_and_closed_only_filter(self):
        day1 = int(dt.datetime(2026, 11, 13, 0, 0, tzinfo=dt.timezone.utc).timestamp() * 1000)
        day2 = day1 + 86_400_000
        rows = [
            [str(day1), '100', '110', '95', '105', '200', str(day1 + 86_399_999), '21000',
             '2026-11-13 00:00:00', '500', '110', '11550'],
            # Incomplete bar (closes after now_ms) must be excluded when closed_only=True
            [str(day2), '105', '112', '104', '108', '50', str(day2 + 86_399_999), '5400',
             '2026-11-14 00:00:00', '120', '25', '2700'],
        ]
        now_ms = day2 + 3_600_000  # 01:00 UTC of day2

        cli = StubClient(rows=rows)
        panel = nas_klines.fetch_crypto_bars('BTCUSDT', '1d', closed_only=True, now_ms=now_ms, fmt='panel', client=cli)
        self.assertEqual(panel, [['2026-11-13', 100.0, 110.0, 95.0, 105.0, 200.0, 105.0]])
        self.assertIn("symbol = 'BTCUSDT'", cli.queries[0])

        kl = nas_klines.fetch_crypto_bars('BTCUSDT', '1d', closed_only=False, now_ms=now_ms, fmt='klines', client=StubClient(rows=rows))
        self.assertEqual(len(kl), 2)
        self.assertEqual(kl[0], [day1, 100.0, 110.0, 95.0, 105.0])

        mc = nas_klines.fetch_crypto_bars('BTCUSDT', '1d', closed_only=True, now_ms=now_ms, fmt='market_cache', client=StubClient(rows=rows))
        self.assertEqual(mc, [[day1, 100.0, 110.0, 95.0, 105.0, 200.0, 21000.0]])

        dc = nas_klines.fetch_crypto_bars('BTCUSDT', '1d', closed_only=True, now_ms=now_ms, fmt='dict', client=StubClient(rows=rows))
        self.assertEqual(dc[0]['count'], 500)
        self.assertEqual(dc[0]['taker_buy_volume'], 110.0)

    def test_bulk_crypto_and_us_panel_rows(self):
        day1 = int(dt.datetime(2026, 11, 13, 0, 0, tzinfo=dt.timezone.utc).timestamp() * 1000)
        now_ms = day1 + 2 * 86_400_000
        cr_rows = [
            ['BTCUSDT', str(day1), '100', '110', '90', '105', '10', str(day1 + 86_399_999), '1050', '2026-11-13 00:00:00', '5', '6', '630'],
            ['ETHUSDT', str(day1), '10', '12', '9', '11', '20', str(day1 + 86_399_999), '220', '2026-11-13 00:00:00', '8', '10', '110'],
        ]
        res_cr = nas_klines.bulk_crypto_panel_rows(['ETHUSDT', 'BTCUSDT'], now_ms=now_ms, client=StubClient(rows=cr_rows))
        self.assertEqual(res_cr['BTCUSDT'], [['2026-11-13', 100.0, 110.0, 90.0, 105.0, 10.0, 105.0]])
        self.assertEqual(res_cr['ETHUSDT'], [['2026-11-13', 10.0, 12.0, 9.0, 11.0, 20.0, 11.0]])

        us_rows = [
            ['AAPL', '2026-11-13', '357', '360', '355', '356', '1000', '0', '0', 'yahoo'],
        ]
        res_us = nas_klines.bulk_us_panel_rows(['AAPL', 'MSFT'], today_utc='2026-11-14', client=StubClient(rows=us_rows))
        self.assertEqual(len(res_us['AAPL']), 1)
        self.assertAlmostEqual(res_us['AAPL'][0][6], (360.0 + 355.0 + 356.0) / 3.0)
        self.assertEqual(res_us['MSFT'], [])


if __name__ == '__main__':
    unittest.main()
