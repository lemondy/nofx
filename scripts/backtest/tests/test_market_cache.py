"""Offline tests for scripts.backtest.market_cache (no network, injectable fetchers)."""
import datetime as dt
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from scripts.backtest import market_cache as mc

HOUR = mc.KLINE_MS['1h']
DAY_MS = mc.KLINE_MS['1d']


def us_row(day, close=100.0, ratio=1.0, volume=1000.0):
    d = day if isinstance(day, str) else day.isoformat()
    return [d, close - 0.5, close + 1.0, close - 1.0, close, round(close * ratio, 12), volume]


def us_series(start, n, close0=100.0, ratio=1.0):
    d0 = dt.date.fromisoformat(start)
    return [us_row(d0 + dt.timedelta(days=i), close0 + i, ratio) for i in range(n)]


def kline_bar(open_ms, base=10.0, close=None):
    return [int(open_ms), float(base), float(base) + 1.0, float(base) - 1.0,
            float(base) + 0.5 if close is None else float(close), 3.0, 30.0]


def quiet(*_args, **_kwargs):
    pass


class UsUpdateTests(unittest.TestCase):
    def test_first_full_download_sorts_dedupes_and_drops_none(self):
        rows = us_series('2024-01-01', 5)
        rows.append(list(rows[2]))            # duplicate date
        rows.append([None, 1.0, 1.0, 1.0, 1.0, 1.0, 10.0])  # None row -> dropped
        shuffled = [rows[3], rows[0], rows[4], rows[2], rows[2], rows[1], rows[5]]
        calls = []

        def fetch(symbol, p1, p2, proxy):
            calls.append((symbol, p1, p2, proxy))
            return shuffled

        with tempfile.TemporaryDirectory() as td:
            res = mc.update_us('SPY', td, proxy='', fetch=fetch, now_s=1_800_000_000, log=quiet)
            self.assertEqual(res['mode'], 'full')
            path = Path(td) / 'quantjourney/prices/SPY.json'
            saved = json.loads(path.read_text())
            self.assertEqual([r[0] for r in saved],
                             ['2024-01-01', '2024-01-02', '2024-01-03', '2024-01-04', '2024-01-05'])
            self.assertEqual(len(calls), 1)
            self.assertEqual(calls[0][0], 'SPY')
            self.assertEqual(calls[0][1], mc.FULL_START_S)   # full download starts at 2000-01-01
            self.assertEqual(calls[0][2], 1_800_000_000)
            self.assertEqual(calls[0][3], '')

    def test_incremental_merge_keeps_old_dates_and_appends_new(self):
        old = us_series('2024-01-01', 10)          # 2024-01-01 .. 2024-01-10
        overlap = [list(r) for r in old[-3:]]      # 2024-01-08 .. 2024-01-10 unchanged
        fresh = us_series('2024-01-11', 2)
        window = overlap + fresh
        calls = []

        def fetch(symbol, p1, p2, proxy):
            calls.append((p1, p2))
            return window

        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'quantjourney/prices/SPY.json'
            mc.atomic_write_rows(path, old)
            expected_p1 = mc.date_to_epoch(dt.date(2024, 1, 10)) - 10 * 86400
            res = mc.update_us('SPY', td, fetch=fetch, now_s=mc.date_to_epoch(dt.date(2024, 1, 13)),
                               log=quiet)
            self.assertEqual(res['mode'], 'merge')
            self.assertEqual(res['new_days'], 2)
            self.assertEqual(calls, [(expected_p1, mc.date_to_epoch(dt.date(2024, 1, 13)))])
            saved = json.loads(path.read_text())
            self.assertEqual(len(saved), 12)
            self.assertEqual([r[0] for r in saved][:7],
                             [f'2024-01-0{i}' for i in range(1, 8)])   # early dates untouched
            self.assertEqual(saved[-2:], fresh)
            self.assertEqual(saved[7:10], overlap)

    def test_adjustment_ratio_change_triggers_full_replace_and_backup(self):
        old = us_series('2024-01-01', 10, ratio=1.0)
        window = [us_row(r[0], r[4], ratio=1.2) for r in old[-3:]]  # adjclose basis recomputed
        full = us_series('2024-01-01', 10, ratio=1.2)
        calls = []

        def fetch(symbol, p1, p2, proxy):
            calls.append(p1)
            return full if p1 == mc.FULL_START_S else window

        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'quantjourney/prices/SPY.json'
            mc.atomic_write_rows(path, old)
            res = mc.update_us('SPY', td, fetch=fetch, now_s=1_800_000_000, log=quiet)
            self.assertEqual(res['mode'], 'full-replace')
            backup = Path(td) / f'quantjourney/prices/SPY.json.bak-{dt.date.today():%Y%m%d}'
            self.assertTrue(backup.exists(), 'old file must be kept as .bak-<YYYYMMDD>')
            self.assertEqual(json.loads(backup.read_text()), old)
            self.assertEqual(json.loads(path.read_text()), full)
            self.assertEqual(calls[0], mc.date_to_epoch(dt.date(2024, 1, 10)) - 10 * 86400)
            self.assertEqual(calls[1], mc.FULL_START_S)  # redownload asked for full history

    def test_close_change_on_overlap_also_triggers_full_replace(self):
        old = us_series('2024-01-01', 6)
        window = us_series('2024-01-01', 6, close0=999.0)  # split recalculated raw closes
        full = [list(r) for r in window]

        def fetch(symbol, p1, p2, proxy):
            return full if p1 == mc.FULL_START_S else window

        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'quantjourney/prices/GLD.json'
            mc.atomic_write_rows(path, old)
            res = mc.update_us('GLD', td, fetch=fetch, now_s=1_800_000_000, log=quiet)
            self.assertEqual(res['mode'], 'full-replace')
            self.assertEqual(json.loads(path.read_text()), full)

    def test_live_session_bar_is_not_cached_and_does_not_force_redownload(self):
        old = us_series('2024-01-01', 10)                  # .. 2024-01-10 (a Wednesday)
        settled = [list(r) for r in old[-3:]] + [us_row('2024-01-11', 111.0)]
        live = settled + [us_row('2024-01-12', 150.0)]     # session still open
        # 2024-01-12 12:00 New York: the 01-12 bar is live, 01-11 is final.
        noon = int(dt.datetime(2024, 1, 12, 12, 0, tzinfo=mc.NEW_YORK).timestamp())
        evening = int(dt.datetime(2024, 1, 12, 17, 0, tzinfo=mc.NEW_YORK).timestamp())
        final = settled + [us_row('2024-01-12', 112.0)]
        calls = []

        def fetch(symbol, p1, p2, proxy):
            calls.append(p1)
            return live if p2 == noon else final

        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'quantjourney/prices/SPY.json'
            mc.atomic_write_rows(path, old)
            mc.update_us('SPY', td, fetch=fetch, now_s=noon, log=quiet)
            self.assertEqual(json.loads(path.read_text())[-1][0], '2024-01-11')
            res = mc.update_us('SPY', td, fetch=fetch, now_s=evening, log=quiet)
            self.assertEqual(res['mode'], 'merge')
            self.assertNotIn(mc.FULL_START_S, calls)
            self.assertEqual(json.loads(path.read_text())[-1], us_row('2024-01-12', 112.0))

    def test_newest_cached_bar_may_change_without_full_replace(self):
        old = us_series('2024-01-01', 10)
        window = [list(r) for r in old[-3:-1]] + [us_row('2024-01-10', 555.0)] + us_series('2024-01-11', 1)

        def fetch(symbol, p1, p2, proxy):
            self.assertNotEqual(p1, mc.FULL_START_S)
            return window

        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'quantjourney/prices/SPY.json'
            mc.atomic_write_rows(path, old)
            res = mc.update_us('SPY', td, fetch=fetch, now_s=1_800_000_000, log=quiet)
            self.assertEqual(res['mode'], 'merge')
            self.assertEqual(json.loads(path.read_text())[9][4], 555.0)

    def test_truncated_redownload_leaves_cache_and_writes_no_backup(self):
        old = us_series('2024-01-01', 10, ratio=1.0)
        window = [us_row(r[0], r[4], ratio=1.2) for r in old[-3:]]

        def fetch(symbol, p1, p2, proxy):
            return old[:3] if p1 == mc.FULL_START_S else window   # broken full history

        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'quantjourney/prices/SPY.json'
            mc.atomic_write_rows(path, old)
            with self.assertRaises(RuntimeError):
                mc.update_us('SPY', td, fetch=fetch, now_s=1_800_000_000, log=quiet)
            self.assertEqual(json.loads(path.read_text()), old)
            self.assertEqual(list(path.parent.glob('*.bak-*')), [])


class KlinesUpdateTests(unittest.TestCase):
    def test_first_download_requires_start_and_drops_unfinished(self):
        t0 = mc.date_to_ms(dt.date(2024, 1, 1))
        now = t0 + 3 * HOUR + 60_000
        bars = [kline_bar(t0 + i * HOUR, 10 + i) for i in range(4)]  # 4th bar still open
        calls = []

        def fetch(symbol, interval, start_ms, end_ms, proxy):
            calls.append((interval, start_ms, end_ms))
            return bars

        with tempfile.TemporaryDirectory() as td:
            with self.assertRaises(SystemExit):
                mc.update_klines('BTCUSDT', '1h', td, fetch=fetch, now_ms=now, log=quiet)
            self.assertEqual(calls, [])  # nothing requested without --start
            res = mc.update_klines('BTCUSDT', '1h', td, start='2024-01-01',
                                   fetch=fetch, now_ms=now, log=quiet)
            self.assertEqual(res['mode'], 'full')
            self.assertEqual(calls, [('1h', t0, now)])
            saved = json.loads((Path(td) / 'volrix/klines/BTCUSDT_1h.json').read_text())
            self.assertEqual([r[0] for r in saved], [t0, t0 + HOUR, t0 + 2 * HOUR])

    def test_incremental_starts_after_last_dedupes_and_drops_unfinished(self):
        t0 = mc.date_to_ms(dt.date(2024, 1, 1))
        old = [kline_bar(t0 + i * HOUR, 10 + i) for i in range(3)]
        now = t0 + 6 * HOUR
        dup = kline_bar(t0 + 2 * HOUR, 12.0, close=12.9)     # overlap: refreshed values win
        fresh = [kline_bar(t0 + 3 * HOUR, 13.0), kline_bar(t0 + 4 * HOUR, 14.0)]
        unfinished = kline_bar(t0 + 5 * HOUR + HOUR, 99.0)   # opens after `now` window end? not closed
        calls = []

        def fetch(symbol, interval, start_ms, end_ms, proxy):
            calls.append(start_ms)
            return [dup] + fresh + [kline_bar(t0 + 5 * HOUR, 15.0), unfinished]

        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'volrix/klines/BTCUSDT_1h.json'
            mc.atomic_write_rows(path, old)
            res = mc.update_klines('BTCUSDT', '1h', td, fetch=fetch, now_ms=now, log=quiet)
            self.assertEqual(res['mode'], 'merge')
            self.assertEqual(calls, [t0 + 3 * HOUR])  # request starts at last bar + step
            saved = json.loads(path.read_text())
            self.assertEqual([r[0] for r in saved], [t0 + i * HOUR for i in range(6)])
            self.assertEqual(saved[2][4], 12.9)       # new fetch overwrote the overlapping bar
            self.assertEqual(saved[5][4], 15.5)
            self.assertNotIn(unfinished[0], [r[0] for r in saved])

    def test_up_to_date_skips_refetch_and_rewrite(self):
        t0 = mc.date_to_ms(dt.date(2024, 1, 1))
        old = [kline_bar(t0, 10.0)]
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'volrix/klines/ETHUSDT_4h.json'
            mc.atomic_write_rows(path, old)
            before = path.read_text()
            res = mc.update_klines('ETHUSDT', '4h', td,
                                   fetch=lambda *a: (_ for _ in ()).throw(AssertionError('no fetch')),
                                   now_ms=t0 + HOUR, log=quiet)
            self.assertEqual(res['mode'], 'up-to-date')
            self.assertEqual(path.read_text(), before)


class CheckUsTests(unittest.TestCase):
    CALENDAR = [f'2024-01-{d:02d}' for d in range(1, 10)]  # 2024-01-01 .. 2024-01-09

    def test_detects_duplicates_ohlc_violations_and_calendar_holes(self):
        rows = us_series('2024-01-01', 8)                       # covers 01-01 .. 01-08
        del rows[4]                                             # 2024-01-05 missing -> calendar hole
        by_day = {r[0]: r for r in rows}
        rows.append(list(by_day['2024-01-03']))                 # duplicate 2024-01-03
        by_day['2024-01-04'][3] = by_day['2024-01-04'][1] + 5.0  # low > open -> OHLC violation
        by_day['2024-01-06'][6] = -5.0                          # negative volume
        by_day['2024-01-07'][1:5] = [1.0, 1.2, 0.0, 0.0]        # zero close -> non-positive price
        rows.append(us_row('2023-12-31', close=49.0))           # extra vs SPY calendar
        rep = mc.check_us(rows, self.CALENDAR)
        self.assertEqual(rep['dup_dates'], ['2024-01-03'])
        self.assertEqual(rep['ohlc_violations'], ['2024-01-04'])
        self.assertEqual(rep['negative_volume'], ['2024-01-06'])
        self.assertEqual(rep['nonpositive_price'], ['2024-01-07'])
        self.assertEqual(rep['calendar_missing'], ['2024-01-05'])
        self.assertEqual(rep['calendar_extra'], ['2023-12-31'])
        self.assertEqual(rep['status'], 'WARN')
        self.assertEqual(mc.check_us(us_series('2024-01-01', 5), self.CALENDAR)['status'], 'OK')

    def test_detects_adjustment_ratio_jump_and_huge_adjusted_return(self):
        rows = us_series('2024-01-01', 3, ratio=1.0)
        rows += us_series('2024-01-04', 2, close0=103.0, ratio=1.5)  # +50% ratio jump in one day
        rep = mc.check_us(rows, self.CALENDAR)
        self.assertEqual(rep['ratio_jumps'], ['2024-01-04'])
        self.assertEqual(rep['adj_return_jumps'], ['2024-01-04'])


class CheckCryptoTests(unittest.TestCase):
    def test_detects_gaps_ohlc_violation_and_open_gap(self):
        rows = [
            ['2024-01-01', 100.0, 101.0, 99.0, 100.5, 10.0, 1000.0],
            ['2024-01-02', 100.5, 102.0, 100.0, 101.0, 10.0, 1000.0],
            ['2024-01-03', 101.0, 101.5, 100.5, 101.0, 10.0, 1000.0],
            # 2024-01-04 missing entirely -> gap of 1 day
            ['2024-01-05', 103.5, 104.0, 103.0, 103.5, 10.0, 1000.0],  # open +2.48% vs prev close
            ['2024-01-06', 103.0, 106.0, 103.5, 104.0, 10.0, 1000.0],  # low above open -> violation
        ]
        rep = mc.check_crypto_daily(rows)
        self.assertEqual(rep['gap_segments'], [['2024-01-03', '2024-01-05', 1]])
        self.assertEqual(rep['gap_days'], 1)
        self.assertEqual(rep['open_gaps'], ['2024-01-05'])
        self.assertEqual(rep['ohlc_violations'], ['2024-01-06'])
        self.assertEqual(rep['status'], 'WARN')

    def test_funding_coverage_counts_missing_days_and_warns_on_big_rate(self):
        price = [['2024-01-0%d' % d, 1.0, 1.0, 1.0, 1.0, 1.0] for d in range(1, 5)]
        funding = [['2024-01-01', 0.0001], ['2024-01-03', -0.0002], ['2024-01-04', 0.02]]
        rep = mc.check_funding_coverage(price, funding)
        self.assertEqual(rep['missing_dates'], ['2024-01-02'])
        self.assertEqual(rep['missing_days'], 1)
        self.assertEqual(rep['max_abs_rate'], 0.02)
        self.assertTrue(rep['warn'])


class CheckKlinesTests(unittest.TestCase):
    def test_detects_gap_missing_bars_and_zero_volume(self):
        t0 = mc.date_to_ms(dt.date(2024, 1, 1))
        bars = [kline_bar(t0, 10.0), kline_bar(t0 + HOUR, 11.0),
                kline_bar(t0 + 3 * HOUR, 13.0), kline_bar(t0 + 4 * HOUR, 14.0)]
        bars[3][5] = 0.0
        rep = mc.check_klines(bars, HOUR)
        self.assertEqual(rep['missing_bars'], 1)
        self.assertEqual(rep['gap_segments'], [[t0 + HOUR, t0 + 3 * HOUR, 1]])
        self.assertEqual(rep['zero_volume_bars'], [t0 + 4 * HOUR])
        self.assertEqual(rep['status'], 'WARN')
        self.assertEqual(mc.check_klines([kline_bar(t0 + i * HOUR, 10 + i) for i in range(5)],
                                         HOUR)['status'], 'OK')

    def test_5m_vs_1h_aggregation_mismatch(self):
        hour0 = mc.date_to_ms(dt.date(2024, 1, 1))
        h1 = [[hour0, 10.0, 11.0, 9.0, 10.5, 1.0, 1.0],
              [hour0 + HOUR, 10.5, 12.0, 10.0, 11.0, 1.0, 1.0]]
        fine = []
        for open_ms in range(hour0, hour0 + HOUR, mc.KLINE_MS['5m']):
            bar = kline_bar(open_ms, 10.0, close=10.5)     # aggregates exactly to the 1h bar
            bar[2], bar[3] = 11.0, 9.0
            fine.append(bar)
        tail = kline_bar(hour0 + HOUR, 10.5, close=11.2)         # closes off vs the 1h bar
        tail[2], tail[3] = 12.0, 10.0
        fine.append(tail)
        rep = mc.check_aggregate_consistency(fine, h1, HOUR)
        self.assertEqual(rep['common_bars'], 2)
        self.assertEqual(rep['mismatched_bars'], 1)
        self.assertEqual(rep['samples'][0]['time_ms'], hour0 + HOUR)
        self.assertEqual(rep['status'], 'WARN')


class CheckAllAndPrintTests(unittest.TestCase):
    def test_check_all_wires_up_report_and_print(self):
        t0 = mc.date_to_ms(dt.date(2024, 1, 1))
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            mc.atomic_write_rows(root / 'quantjourney/prices/SPY.json',
                                 us_series('2024-01-01', 9))
            bad = us_series('2024-01-01', 8)
            bad.append(list(bad[2]))                             # duplicate
            mc.atomic_write_rows(root / 'quantjourney/prices/XYZ.json', bad)
            mc.atomic_write_rows(root / 'factor_mining/crypto/daily/BTCUSDT.json',
                                 [['2024-01-01', 1.0, 1.0, 1.0, 1.0, 1.0, 1.0]])
            mc.atomic_write_rows(root / 'factor_mining/crypto/alt_daily/funding/BTCUSDT.json',
                                 [['2024-01-01', 0.0001]])
            mc.atomic_write_rows(root / 'volrix/klines/BTCUSDT_1h.json',
                                 [[t0, 10.0, 11.0, 9.0, 10.5, 1.0, 1.0]])
            five = [kline_bar(t0 + i * mc.KLINE_MS['5m'], 10.0, close=10.5) for i in range(12)]
            five[0][2], five[0][3] = 11.0, 9.0
            mc.atomic_write_rows(root / 'volrix/klines/BTCUSDT_5m.json', five)

            report = mc.check_all(root, us=['XYZ'], crypto=['BTCUSDT'],
                                  klines=['BTCUSDT_1h', 'BTCUSDT_5m'])
            self.assertEqual(report['us']['symbols']['XYZ']['status'], 'WARN')
            self.assertEqual(report['us']['symbols']['XYZ']['dup_dates'], ['2024-01-03'])
            self.assertEqual(report['us']['calendar_days'], 9)
            self.assertEqual(report['crypto']['symbols']['BTCUSDT']['status'], 'OK')
            self.assertEqual(report['crypto']['symbols']['BTCUSDT']['funding']['missing_days'], 0)
            pair = report['klines']['pairs']['BTCUSDT']
            self.assertEqual(pair['mismatched_bars'], 0)
            self.assertEqual(report['klines']['files']['BTCUSDT_1h']['status'], 'OK')

            missing = mc.check_all(root, us=['NOPE'], crypto=['GONE'], klines=['BTCUSDT_4h'])
            self.assertEqual(missing['us']['symbols']['NOPE']['status'], 'MISSING')
            self.assertEqual(missing['crypto']['symbols']['GONE']['status'], 'MISSING')
            self.assertEqual(missing['klines']['files']['BTCUSDT_4h']['status'], 'MISSING')

            out = io.StringIO()
            mc.print_report(report, out=out)
            text = out.getvalue()
            for needle in ('XYZ', 'dup', 'BTCUSDT', '5m aggregated vs 1h', 'fund_miss'):
                self.assertIn(needle, text)


class CrossCheckTests(unittest.TestCase):
    def test_nasdaq_payload_parse_skips_na_placeholder_rows(self):
        payload = {'data': {'tradesTable': {'rows': [
            {'date': '01/02/2024', 'close': '$100.50', 'open': '$101.00',
             'high': '$101.50', 'low': '$100.00', 'volume': '1,000,000'},
            {'date': '01/01/2024', 'close': '$100.00', 'open': '$100.00',
             'high': '$100.00', 'low': '$100.00', 'volume': 'N/A'},   # placeholder -> skipped
            {'date': '12/31/2023', 'close': 'N/A', 'open': '$99.00',
             'high': '$99.50', 'low': '$98.50', 'volume': '500'},
        ]}}}
        rows = mc.parse_nasdaq_trades(payload)
        self.assertEqual(rows, [['2024-01-02', 101.0, 101.5, 100.0, 100.5]])

    def test_us_cross_check_counts_close_mismatches(self):
        ours = [['2024-01-0%d' % d, 0, 0, 0, 100.0, 100.0, 0] for d in range(1, 4)]
        theirs = [['2024-01-01', 100, 101, 99, 100.0],
                  ['2024-01-02', 100, 101, 99, 101.0],            # +1% -> flagged
                  ['2024-01-03', 100, 101, 99, 100.03]]           # within tolerance
        rep = mc.cross_check_us_nasdaq('SPY', ours, fetch=lambda s, c, p: theirs)
        self.assertEqual(rep['assetclass'], 'etf')
        self.assertEqual(rep['compared_days'], 3)
        self.assertEqual(rep['mismatch_days'], 1)

    def test_ko_pep_use_stocks_assetclass(self):
        mc.cross_check_us_nasdaq('KO', [['2024-01-02', 0, 0, 0, 1.0, 1.0, 0]],
                                 fetch=lambda s, c, p: self.assertEqual(c, 'stocks') or [])

    def test_crypto_cross_check_compares_full_history_ohlc(self):
        day0 = mc.date_to_ms(dt.date(2024, 1, 1))
        kl = [[day0, 1.0, 2.0, 0.5, 1.5, 1.0, 1.0],
              [day0 + DAY_MS, 1.5, 1.6, 1.4, 1.55, 1.0, 1.0],
              [day0 + 2 * DAY_MS, 1.55, 1.7, 1.5, 1.60, 1.0, 1.0]]
        ours = [['2024-01-01', 1.0, 2.0, 0.5, 1.5, 1.0, 1.0],
                ['2024-01-02', 1.5, 1.6, 1.4, 1.9, 1.0, 1.0],     # close differs -> flagged
                ['2024-01-04', 1.0, 1.0, 1.0, 1.0, 1.0, 1.0]]     # not on exchange -> ignored
        rep = mc.cross_check_crypto_daily('BTCUSDT', ours, fetch=lambda s, i, a, b, p: kl)
        self.assertEqual(rep['compared_days'], 2)
        self.assertEqual(rep['mismatch_days'], 1)


class AtomicWriteTests(unittest.TestCase):
    def test_failed_replace_keeps_original_and_cleans_temp(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'quantjourney/prices/SPY.json'
            mc.atomic_write_rows(path, us_series('2024-01-01', 3))
            original = path.read_text()
            with mock.patch.object(mc.os, 'replace', side_effect=OSError('disk full')):
                with self.assertRaises(OSError):
                    mc.atomic_write_rows(path, us_series('2025-01-01', 3))
            self.assertEqual(path.read_text(), original, 'original data must survive a failed write')
            self.assertEqual([p.name for p in path.parent.iterdir()], ['SPY.json'])

    def test_failed_dump_inside_update_keeps_cached_file(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'quantjourney/prices/SPY.json'
            old = us_series('2024-01-01', 10)
            mc.atomic_write_rows(path, old)
            original = path.read_text()
            window = [list(r) for r in old[-3:]] + us_series('2024-01-11', 2)
            with mock.patch.object(mc.os, 'replace', side_effect=OSError('disk full')):
                with self.assertRaises(OSError):
                    mc.update_us('SPY', td,
                                 fetch=lambda s, p1, p2, x: window,
                                 now_s=mc.date_to_epoch(dt.date(2024, 1, 13)), log=quiet)
            self.assertEqual(path.read_text(), original)
            self.assertEqual([p.name for p in path.parent.iterdir()], ['SPY.json'])


class HelperTests(unittest.TestCase):
    def test_parse_kline_spec(self):
        self.assertEqual(mc.parse_kline_spec('BTCUSDT_5m'), ('BTCUSDT', '5m'))
        with self.assertRaises(ValueError):
            mc.parse_kline_spec('BTCUSDT')
        with self.assertRaises(ValueError):
            mc.parse_kline_spec('BTCUSDT_2h')

    def test_backup_paths_never_collide(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / 'SPY.json'
            path.write_text('x')
            first = mc._next_backup_path(path, '20240101')
            self.assertEqual(first.name, 'SPY.json.bak-20240101')
            first.write_text('old')
            second = mc._next_backup_path(path, '20240101')
            self.assertEqual(second.name, 'SPY.json.bak-20240101-2')


if __name__ == '__main__':
    unittest.main()
