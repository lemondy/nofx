"""Offline tests for the full-universe archive tooling (zip files built in memory, _get stubbed)."""
import contextlib
import datetime as dt
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock
import zipfile

from scripts.factor_mining import data_fetch, full_universe

DAY = data_fetch.DAY_MS

KLINE_HEADER = ('open_time,open,high,low,close,volume,close_time,quote_volume,'
                'count,taker_buy_volume,taker_buy_quote_volume,ignore')
FUNDING_HEADER = 'calc_time,funding_interval_hours,last_funding_rate'


def ms(y, m, d, hour=0):
    t = dt.datetime(y, m, d, hour, tzinfo=dt.timezone.utc)
    return int(t.timestamp() * 1000)


def zip_bytes(name, text):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, 'w', zipfile.ZIP_DEFLATED) as zf:
        zf.writestr(name, text)
    return buf.getvalue()


def kline_csv(rows, header=True):
    """Binance 1d kline CSV; rows are (open_ms, open, high, low, close, volume, quote_volume)."""
    body = '\n'.join(f'{t},{o},{h},{l},{c},{v},{t + DAY - 1},{qv},100,5,5,0'
                     for t, o, h, l, c, v, qv in rows)
    return (KLINE_HEADER + '\n' if header else '') + body + '\n'


def funding_csv(rows, header=True):
    """Binance fundingRate CSV; rows are (calc_time_ms, interval_hours, rate)."""
    body = '\n'.join(f'{t},{iv},{r}' for t, iv, r in rows)
    return (FUNDING_HEADER + '\n' if header else '') + body + '\n'


def put_archive(dest, data, checksum=None):
    """Write an archive plus its .CHECKSUM (a wrong hash when ``checksum`` is given)."""
    dest = Path(dest)
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_bytes(data)
    text = checksum if checksum is not None else f'{full_universe.sha256(data)}  {dest.name}'
    dest.with_name(dest.name + '.CHECKSUM').write_text(text)
    return dest


def krow(date, o=1.0, h=2.0, l=0.5, c=1.5, vol=10.0, vwap=2.0):
    return [date, o, h, l, c, vol, vwap]


def stub_get(checksum_text, data, calls=None):
    """Stands in for full_universe._get: serves CHECKSUM text and archive bytes from memory."""
    def _get(opener, url, sleep=None):
        if calls is not None:
            calls.append(url)
        return checksum_text.encode() if url.endswith('.CHECKSUM') else data
    return _get


class ClassifyTests(unittest.TestCase):
    def test_keeps_usdt_coin_perps_marks_absent_and_drops_the_rest(self):
        info = {'symbols': [
            dict(symbol='BTCUSDT', contractType='PERPETUAL', quoteAsset='USDT',
                 underlyingType='COIN', status='TRADING', onboardDate=ms(2019, 9, 1),
                 deliveryDate=4133404800000),                      # 2100 sentinel -> delivery None
            dict(symbol='LUNAUSDT', contractType='PERPETUAL', quoteAsset='USDT',
                 underlyingType='COIN', status='SETTLING', onboardDate=ms(2021, 1, 1),
                 deliveryDate=ms(2025, 6, 27)),                    # settled perp -> real delivery date
            dict(symbol='AAPLUSDT', contractType='TRADIFI_PERPETUAL', quoteAsset='USDT',
                 underlyingType='COIN', status='TRADING', onboardDate=1, deliveryDate=0),
            dict(symbol='BTCDOMUSDT', contractType='PERPETUAL', quoteAsset='USDT',
                 underlyingType='INDEX', status='TRADING', onboardDate=1, deliveryDate=0),
            dict(symbol='XYZUSDT', contractType='PERPETUAL', quoteAsset='USDT',
                 underlyingType='PREMARKET', status='TRADING', onboardDate=1, deliveryDate=0),
            dict(symbol='BTCBUSD', contractType='PERPETUAL', quoteAsset='BUSD',
                 underlyingType='COIN', status='TRADING', onboardDate=1, deliveryDate=0),
            dict(symbol='ETHUSDC', contractType='PERPETUAL', quoteAsset='USDC',
                 underlyingType='COIN', status='TRADING', onboardDate=1, deliveryDate=0),
        ]}
        universe = full_universe.classify(info, ['BTCUSDT', 'LUNAUSDT', 'AAPLUSDT', 'BTCDOMUSDT',
                                                 'XYZUSDT', 'BTCBUSD', 'ETHUSDC', 'FTTUSDT'])
        self.assertEqual(set(universe), {'BTCUSDT', 'LUNAUSDT', 'FTTUSDT'})
        self.assertEqual(universe['BTCUSDT'],
                         dict(status='TRADING', onboard='2019-09-01', delivery=None))
        self.assertEqual(universe['LUNAUSDT'],
                         dict(status='SETTLING', onboard='2021-01-01', delivery='2025-06-27'))
        self.assertEqual(universe['FTTUSDT'],                      # no longer in exchangeInfo
                         dict(status='ABSENT', onboard=None, delivery=None))

    def test_dedupes_vision_symbols(self):
        universe = full_universe.classify({'symbols': []}, ['FTTUSDT', 'FTTUSDT'])
        self.assertEqual(list(universe), ['FTTUSDT'])


class KlineRowsTests(unittest.TestCase):
    def test_header_and_headerless_csvs_parse_to_rows(self):
        rows = [(ms(2021, 5, 1), '1', '2', '0.5', '1.5', '10', '20'),
                (ms(2021, 5, 2), '2', '3', '1', '2.5', '4', '10')]
        with tempfile.TemporaryDirectory() as tmp:
            for name, header in (('BTCUSDT-1d-2021-05.zip', True),
                                 ('BTCUSDT-1d-2021-04-noheader.zip', False)):
                path = Path(tmp) / name
                path.write_bytes(zip_bytes(name[:-4] + '.csv', kline_csv(rows, header=header)))
                got = full_universe.kline_rows(path)
                self.assertEqual([r[0] for r in got], ['2021-05-01', '2021-05-02'])  # UTC date of open ms
                self.assertEqual(got[0], ['2021-05-01', 1.0, 2.0, 0.5, 1.5, 10.0, 2.0])
                self.assertAlmostEqual(got[1][6], 10 / 4)         # vwap = quote_volume / volume


class FundingSettlementsTests(unittest.TestCase):
    def test_parses_calc_time_and_rate_skipping_header(self):
        rows = [(ms(2025, 3, 1, 8), 8, '0.00010000'), (ms(2025, 3, 1, 16), 8, '-0.00100000')]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'BTCUSDT-fundingRate-2025-03.zip'
            path.write_bytes(zip_bytes('BTCUSDT-fundingRate-2025-03.csv', funding_csv(rows)))
            self.assertEqual(full_universe.funding_settlements(path),
                             [(ms(2025, 3, 1, 8), 1e-4), (ms(2025, 3, 1, 16), -1e-3)])


class CheckRowsTests(unittest.TestCase):
    def test_reports_duplicates_gaps_invalid_ohlc_and_zero_volume(self):
        rows = [krow('2025-03-01', vol=10.0),
                krow('2025-03-01', vol=12.0),                       # duplicate date
                krow('2025-03-02', h=0.5, c=2.0),                   # high < close -> invalid
                krow('2025-03-05', vol=4.0),                        # gap: 03-03 and 03-04 missing
                krow('2025-03-06', vol=0.0)]
        rep = full_universe.check_rows(rows)
        self.assertEqual((rep['rows'], rep['first'], rep['last']), (4, '2025-03-01', '2025-03-06'))
        self.assertEqual(rep['duplicates'], 1)
        self.assertEqual(rep['invalid_ohlc'], 1)
        self.assertEqual(rep['invalid_dates'], ['2025-03-02'])
        self.assertEqual(rep['gaps'], 1)
        self.assertEqual(rep['missing_days'], 2)
        self.assertEqual(rep['longest_gap'], ('2025-03-02', '2025-03-05', 2))
        self.assertEqual(rep['zero_volume_days'], 1)

    def test_clean_rows_report_no_problems_and_empty_rows_give_none(self):
        clean = [krow(f'2025-03-0{d}') for d in range(1, 4)]
        rep = full_universe.check_rows(clean)
        self.assertEqual((rep['duplicates'], rep['gaps'], rep['missing_days'],
                          rep['invalid_ohlc'], rep['zero_volume_days']), (0, 0, 0, 0, 0))
        rep = full_universe.check_rows([])
        self.assertEqual((rep['rows'], rep['first'], rep['last'], rep['longest_gap']),
                         (0, None, None, None))


class MissingDaysTests(unittest.TestCase):
    def test_interior_gaps_and_extension_to_until(self):
        rows = [krow('2025-03-04'), krow('2025-03-01')]             # unsorted input is fine
        self.assertEqual(full_universe.missing_days(rows), ['2025-03-02', '2025-03-03'])
        self.assertEqual(full_universe.missing_days(rows, until='2025-03-06'),
                         ['2025-03-02', '2025-03-03', '2025-03-05', '2025-03-06'])
        self.assertEqual(full_universe.missing_days(rows, until='2025-03-04'),
                         ['2025-03-02', '2025-03-03'])              # until itself already present
        self.assertEqual(full_universe.missing_days([krow('2025-03-01')], until='2025-03-01'), [])
        self.assertEqual(full_universe.missing_days([]), [])


class FetchFileTests(unittest.TestCase):
    KEY = 'data/futures/um/monthly/klines/BTCUSDT/1d/BTCUSDT-1d-2021-05.zip'
    DATA = zip_bytes('BTCUSDT-1d-2021-05.csv',
                     kline_csv([(ms(2021, 5, 1), 1, 2, 0.5, 1.5, 10, 20)]))

    def fetch(self, dest, checksum_text, data=None):
        calls = []
        with mock.patch.object(full_universe, '_get', stub_get(checksum_text, data or self.DATA, calls)):
            res = full_universe.fetch_file(object(), self.KEY, dest)
        return res, calls

    def test_good_checksum_downloads_and_writes_zip_and_checksum(self):
        text = f'{full_universe.sha256(self.DATA)}  {self.KEY}'
        with tempfile.TemporaryDirectory() as tmp:
            dest = Path(tmp) / 'BTCUSDT-1d-2021-05.zip'
            res, calls = self.fetch(dest, text)
            self.assertEqual(res, 'ok')
            self.assertEqual([c[len(full_universe.VISION):] for c in calls],
                             [self.KEY + '.CHECKSUM', self.KEY])    # checksum first, then archive
            self.assertEqual(dest.read_bytes(), self.DATA)
            self.assertEqual(dest.with_name(dest.name + '.CHECKSUM').read_text(), text)

    def test_second_call_is_cached_without_any_download(self):
        with tempfile.TemporaryDirectory() as tmp:
            dest = Path(tmp) / 'BTCUSDT-1d-2021-05.zip'
            self.fetch(dest, f'{full_universe.sha256(self.DATA)}  x')

            def boom(opener, url, sleep=None):
                raise AssertionError('network call on a verified file')
            with mock.patch.object(full_universe, '_get', boom):
                self.assertEqual(full_universe.fetch_file(object(), self.KEY, dest), 'cached')
            self.assertEqual(dest.read_bytes(), self.DATA)

    def test_bad_checksum_keeps_only_the_mismatch_copy(self):
        with tempfile.TemporaryDirectory() as tmp:
            dest = Path(tmp) / 'BTCUSDT-1d-2021-05.zip'
            res, _ = self.fetch(dest, '0' * 64 + '  wrong')
            self.assertEqual(res, 'mismatch')
            self.assertFalse(dest.exists())                         # never becomes a trusted zip
            self.assertFalse(dest.with_name(dest.name + '.CHECKSUM').exists())
            mismatch = dest.with_name(dest.name + '.mismatch')
            self.assertEqual(mismatch.read_bytes(), self.DATA)      # kept for inspection

    def test_existing_unverified_zip_is_renamed_not_deleted(self):
        old = b'old unverified bytes'
        with tempfile.TemporaryDirectory() as tmp:
            dest = Path(tmp) / 'BTCUSDT-1d-2021-05.zip'
            dest.write_bytes(old)                                   # no .CHECKSUM next to it
            res, _ = self.fetch(dest, f'{full_universe.sha256(self.DATA)}  x')
            self.assertEqual(res, 'ok')
            self.assertEqual(dest.read_bytes(), self.DATA)
            prevs = list(Path(tmp).glob('BTCUSDT-1d-2021-05.zip.prev*'))
            self.assertEqual(len(prevs), 1)
            self.assertEqual(prevs[0].read_bytes(), old)            # old copy survives


class BuildTests(unittest.TestCase):
    def test_build_writes_daily_funding_and_validation_from_local_zips(self):
        now_ms = ms(2025, 3, 4)                                     # today == 2025-03-04
        monthly = zip_bytes('AUSDT-1d-2025-03.csv', kline_csv([
            (ms(2025, 3, 1), '1', '2', '0.5', '1.5', '10', '20'),   # vwap 2.0
            (ms(2025, 3, 2), '1', '2', '0.5', '1.5', '0', '0'),     # zero volume -> dropped
            (ms(2025, 3, 3), '1', '2', '0.5', '1.5', '4', '8'),
            (ms(2025, 3, 4), '1', '2', '0.5', '1.5', '4', '8'),     # today -> excluded
        ]))
        bad_zip = zip_bytes('AUSDT-1d-2025-04.csv', kline_csv([(ms(2025, 4, 1), 1, 2, 0.5, 1.5, 4, 8)]))
        busdt = zip_bytes('BUSDT-1d-2025-03.csv', kline_csv([
            (ms(2025, 3, 1), '1', '2', '0.5', '1.5', '10', '20'),
            (ms(2025, 3, 3), '1', '2', '0.5', '1.5', '10', '20'),   # 03-02 gap
        ]))
        funding = zip_bytes('AUSDT-fundingRate-2025-03.csv', funding_csv([
            (ms(2025, 3, 1, 0), 8, '0.00010000'), (ms(2025, 3, 1, 8), 8, '0.00020000'),
            (ms(2025, 3, 1, 16), 8, '0.00030000'), (ms(2025, 3, 2, 0), 8, '0.01000000')]))
        with tempfile.TemporaryDirectory() as tmp, tempfile.TemporaryDirectory() as live:
            out = Path(tmp)
            (out / 'manifest.json').write_text(json.dumps({'symbols': {
                'AUSDT': {'status': 'TRADING', 'onboard': '2021-01-01', 'delivery': None},
                'BUSDT': {'status': 'ABSENT', 'onboard': None, 'delivery': None}}}))
            put_archive(out / 'raw/klines_1d/AUSDT/AUSDT-1d-2025-03.zip', monthly)
            put_archive(out / 'raw/klines_1d/AUSDT/AUSDT-1d-2025-04.zip', bad_zip, checksum='bad')
            put_archive(out / 'raw/klines_1d/BUSDT/BUSDT-1d-2025-03.zip', busdt)
            put_archive(out / 'raw/funding/AUSDT/AUSDT-fundingRate-2025-03.zip', funding)
            # A live reference that disagrees on both shared closes -> vs_live.mismatched.
            data_fetch.write_symbol(Path(live) / 'AUSDT.json',
                                    [krow('2025-03-01', c=9.9), krow('2025-03-03', c=9.9)])

            with contextlib.redirect_stdout(io.StringIO()):
                summary = full_universe.build(out, live_daily=live, now_ms=now_ms, log=lambda *_: None)

            daily = json.loads((out / 'daily/AUSDT.json').read_text())
            self.assertEqual([r[0] for r in daily], ['2025-03-01', '2025-03-03'])  # zero-vol + today dropped
            self.assertEqual(daily[0], ['2025-03-01', 1.0, 2.0, 0.5, 1.5, 10.0, 2.0])
            bus = json.loads((out / 'daily/BUSDT.json').read_text())
            self.assertEqual([r[0] for r in bus], ['2025-03-01', '2025-03-03'])

            fund = json.loads((out / 'alt_daily/funding/AUSDT.json').read_text())
            self.assertEqual([r[0] for r in fund], ['2025-03-01', '2025-03-02'])
            self.assertAlmostEqual(fund[0][1], 2e-4)                # mean of day-t settlements only
            self.assertAlmostEqual(fund[1][1], 1e-2)

            reps = json.loads((out / 'validation.json').read_text())['symbols']
            self.assertEqual(reps['AUSDT']['checksum_failed'], ['AUSDT-1d-2025-04.zip'])
            self.assertEqual(reps['AUSDT']['zero_volume_days'], 1)
            self.assertEqual(reps['AUSDT']['last_bar'], '2025-03-03')
            self.assertEqual(reps['AUSDT']['vs_live']['shared'], 2)
            self.assertEqual(reps['AUSDT']['vs_live']['mismatched'], 2)
            self.assertGreater(reps['AUSDT']['vs_live']['max_rel_close_diff'], 0.1)
            self.assertNotIn('vs_live', reps['BUSDT'])              # no reference file -> no cross-check
            self.assertEqual((reps['BUSDT']['gaps'], reps['BUSDT']['missing_days']), (1, 1))

            self.assertEqual(summary['symbols'], 2)
            self.assertEqual(summary['checksum_failed'], 1)
            self.assertEqual(summary['vs_live_symbols'], 1)
            self.assertEqual(summary['vs_live_mismatched'], 2)
            self.assertEqual(summary['missing_days'], 2)            # BUSDT 03-02 + AUSDT's zero-vol 03-02
            self.assertEqual(summary['by_status'], {'ABSENT': 1, 'TRADING': 1})

    def test_unverified_funding_zip_counts_as_checksum_failed(self):
        klines = zip_bytes('CUSDT-1d-2025-03.csv',
                           kline_csv([(ms(2025, 3, 1), 1, 2, 0.5, 1.5, 10, 20)]))
        funding = zip_bytes('CUSDT-fundingRate-2025-03.csv',
                            funding_csv([(ms(2025, 3, 1, 8), 8, '0.01')]))
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            (out / 'manifest.json').write_text(json.dumps({'symbols': {
                'CUSDT': {'status': 'SETTLING', 'onboard': '2021-01-01', 'delivery': '2025-06-27'}}}))
            put_archive(out / 'raw/klines_1d/CUSDT/CUSDT-1d-2025-03.zip', klines)
            put_archive(out / 'raw/funding/CUSDT/CUSDT-fundingRate-2025-03.zip', funding, checksum='nope')
            with contextlib.redirect_stdout(io.StringIO()):
                summary = full_universe.build(out, live_daily=out / 'nope', now_ms=ms(2025, 3, 4),
                                              log=lambda *_: None)
            self.assertEqual(summary['checksum_failed'], 1)
            self.assertFalse((out / 'alt_daily/funding/CUSDT.json').exists())
            self.assertEqual(summary['with_klines'], 1)             # klines are still built
            self.assertFalse((out / 'nope').exists())               # reference dir never touched


class MainTests(unittest.TestCase):
    def test_refuses_to_write_into_the_live_mining_directory(self):
        # No stubs at all: the guard must fire before any network access happens.
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            full_universe.main(['build', '--out', str(data_fetch.ROOT / 'crypto')])


if __name__ == '__main__':
    unittest.main()
