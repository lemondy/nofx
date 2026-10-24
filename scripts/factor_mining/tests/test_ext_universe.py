"""Extension discovery/fetch tests: temporary SQLite and get-only fake HTTP."""
import datetime as dt
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from scripts.factor_mining import data_fetch, ext_universe as ext, store

NOW = dt.datetime(2026, 10, 24, 2, tzinfo=dt.timezone.utc)
NOW_MS = int(NOW.timestamp() * 1000)
DAY = data_fetch.DAY_MS


def contract(symbol, **kwargs):
    return dict(symbol=symbol, status='TRADING', contractType='PERPETUAL', quoteAsset='USDT',
                underlyingType='COIN', underlyingSubType=[], onboardDate=NOW_MS - 100 * DAY) | kwargs


class FakeHttp:
    def __init__(self, contracts=(), fail=()):
        self.contracts, self.fail, self.calls = list(contracts), set(fail), []

    def get(self, url, params=None):
        params = params or {}
        self.calls.append((url, dict(params)))
        symbol = params.get('symbol')
        if symbol in self.fail:
            raise OSError('fake offline')
        if url.endswith('/exchangeInfo'):
            return {'symbols': self.contracts}
        if url.endswith('/fundingRate'):
            return [{'fundingTime': NOW_MS - 2 * DAY, 'fundingRate': '0.001'}]
        if url.endswith('/premiumIndexKlines'):
            return [[NOW_MS - 2 * DAY - 2 * 3600_000, '0', '0', '0', '0.01']]
        if url.endswith('/klines'):
            return [[NOW_MS - 2 * DAY - 2 * 3600_000, '1', '2', '0.5', '1.5', '10', 0, '15']]
        raise AssertionError(url)


def make_db(path, candidates=(), orders=()):
    with sqlite3.connect(path) as connection:
        connection.execute('CREATE TABLE decision_records (timestamp TEXT, candidate_coins TEXT)')
        connection.execute('CREATE TABLE trader_orders (created_at INTEGER, symbol TEXT)')
        connection.executemany('INSERT INTO decision_records VALUES (?, ?)', candidates)
        connection.executemany('INSERT INTO trader_orders VALUES (?, ?)', orders)


class ExtraUniverseTests(unittest.TestCase):
    def test_recent_frequency_top_k_read_only_db_and_filter_reasons(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            db = root / 'activity #1.db'
            syms = ['AUSDT', 'BUSDT', 'CUSDT', 'COREUSDT', 'APRUSDT', 'STOCKUSDT',
                    'TAGUSDT', 'MISSUSDT', 'NEWUSDT', 'PAUSEDUSDT', 'DELIVERYUSDT',
                    'QUOTEUSDT', 'UNKNOWNUSDT', 'DATEUSDT']
            make_db(db, [(NOW.isoformat(), json.dumps(syms + ['AUSDT'])),
                         ((NOW - dt.timedelta(days=15)).isoformat(), '["OLDUSDT"]'),
                         (NOW.isoformat(), 'bad JSON'), (NOW.isoformat(), '{}'),
                         ((NOW - dt.timedelta(days=14)).isoformat(), '["BUSDT"]'),
                         ((NOW + dt.timedelta(days=1)).isoformat(), '["FUTUREUSDT"]')],
                    [(NOW_MS, 'busdt'), (NOW_MS - 15 * DAY, 'OLDUSDT'),
                     (NOW_MS + DAY, 'FUTUREUSDT')])
            core = root / 'crypto/daily/COREUSDT.json'
            store.write_json(core, [])
            info = [contract(s) for s in syms if s != 'UNKNOWNUSDT']
            changes = {'STOCKUSDT': {'underlyingType': 'INDEX'},
                       'TAGUSDT': {'underlyingSubType': ['TRADFI_STOCK']},
                       'NEWUSDT': {'onboardDate': NOW_MS - 59 * DAY},
                       'PAUSEDUSDT': {'status': 'SETTLING'},
                       'DELIVERYUSDT': {'contractType': 'CURRENT_QUARTER'},
                       'QUOTEUSDT': {'quoteAsset': 'USDC'}, 'DATEUSDT': {'onboardDate': None}}
            for item in info:
                item.update(changes.get(item['symbol'], {}))
                if item['symbol'] == 'MISSUSDT':
                    item.pop('underlyingType')
            before = db.read_bytes()
            http = FakeHttp(info)
            with patch.dict(os.environ, NOFX_DB=str(db)), \
                 patch.object(ext.sqlite3, 'connect', wraps=sqlite3.connect) as connect:
                result = ext.refresh_extra_symbols(root, http=http, now=NOW, limit=2, log=lambda *_: None)
            self.assertEqual(result['symbols'], ['BUSDT', 'AUSDT'])
            self.assertEqual(result['frequencies'], {'BUSDT': 3, 'AUSDT': 2})
            self.assertEqual(result['filtered'], {key: 1 for key in (
                'core', 'excluded', 'underlying_type', 'tradfi', 'history<60d', 'not_trading',
                'not_perpetual', 'not_usdt', 'not_listed', 'onboard_date', 'limit')} |
                {'underlying_type': 2})
            self.assertEqual(db.read_bytes(), before)
            self.assertTrue(connect.call_args.args[0].endswith('?mode=ro'))
            self.assertTrue(connect.call_args.kwargs['uri'])
            self.assertEqual(len(http.calls), 1)
            self.assertEqual(store.read_json(root / 'crypto/ext/extra_symbols.json'), result)
            self.assertEqual(list(core.parent.iterdir()), [core])

    def test_missing_subtypes_and_types_are_robust_and_tradfi_fails_closed(self):
        for fields, expected in [({'underlyingSubType': None}, None),
                                 ({'underlyingSubType': 'TRADFI'}, 'tradfi'),
                                 ({'underlyingSubType': ['tradfi_commodity']}, 'tradfi'),
                                 ({'underlyingSubType': {}}, 'underlying_subtype'),
                                 ({'underlyingSubType': 42}, 'underlying_subtype'),
                                 ({'underlyingType': None}, 'underlying_type'),
                                 ({'underlyingType': 'STOCK'}, 'underlying_type'),
                                 ({'onboardDate': NOW_MS - 60 * DAY}, None)]:
            with self.subTest(fields=fields):
                self.assertEqual(ext._filter_reason('XUSDT', contract('XUSDT', **fields),
                                                    set(), NOW_MS, 60), expected)
        item = contract('XUSDT')
        item.pop('underlyingSubType')
        self.assertIsNone(ext._filter_reason('XUSDT', item, set(), NOW_MS, 60))

    def test_db_missing_corrupt_or_schema_failure_keeps_cached_file_without_http(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            path = root / 'crypto/ext/extra_symbols.json'
            previous = {'symbols': ['OLDUSDT'], 'generated_at': 'old', 'frequencies': {'OLDUSDT': 9}}
            store.write_json(path, previous)
            before = path.read_bytes()
            for kind in ('missing', 'corrupt', 'schema'):
                db = root / f'{kind}.db'
                if kind == 'corrupt':
                    db.write_text('corrupt sqlite')
                elif kind == 'schema':
                    sqlite3.connect(db).close()
                http = FakeHttp()
                result = ext.refresh_extra_symbols(root, http=http, db_path=db, now=NOW, log=lambda *_: None)
                self.assertEqual(result, previous)
                self.assertEqual(path.read_bytes(), before)
                self.assertEqual(http.calls, [])
                if kind == 'missing':
                    self.assertFalse(db.exists())

    def test_exchange_or_atomic_write_failure_preserves_cache(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            db = root / 'test.db'
            make_db(db, [(NOW.isoformat(), '["XUSDT"]')])
            path = root / 'crypto/ext/extra_symbols.json'
            previous = {'symbols': ['OLDUSDT']}
            store.write_json(path, previous)
            for patcher in (patch.object(FakeHttp, 'get', side_effect=OSError('network')),
                            patch.object(FakeHttp, 'get', return_value={'symbols': []}),
                            patch.object(store.os, 'replace', side_effect=OSError('disk'))):
                with patcher:
                    self.assertEqual(ext.refresh_extra_symbols(root, http=FakeHttp([contract('XUSDT')]),
                                                               db_path=db, now=NOW, log=lambda *_: None), previous)
                self.assertEqual(store.read_json(path), previous)


class ExtraFetchTests(unittest.TestCase):
    def test_only_ext_files_incremental_overlap_and_isolated_failures(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            core = root / 'crypto/daily/COREUSDT.json'
            store.write_json(core, [['untouched']])
            before = {p.name: p.read_bytes() for p in core.parent.iterdir()}
            http = FakeHttp(fail={'BADUSDT'})
            result = ext.fetch_extra_data(['XUSDT', 'BADUSDT', 'COREUSDT'], root, http=http,
                                           now_ms=NOW_MS, log=lambda *_: None)
            self.assertEqual(result['ok'], 1)
            self.assertEqual(set(result['failed']), {'BADUSDT', 'COREUSDT'})
            self.assertEqual(result['alt']['ok'], 2)
            for path in ('daily/XUSDT.json', 'alt_daily/funding/XUSDT.json',
                         'alt_daily/premium/XUSDT.json', 'data_meta.json'):
                self.assertTrue((root / 'crypto/ext' / path).exists())
            self.assertEqual({p.name: p.read_bytes() for p in core.parent.iterdir()}, before)
            self.assertFalse((root / 'crypto/alt_daily').exists())
            http.calls.clear()
            ext.fetch_extra_data(['XUSDT'], root, http=http, now_ms=NOW_MS + DAY, log=lambda *_: None)
            starts = [params['startTime'] for _, params in http.calls]
            self.assertEqual(starts, [data_fetch._day_ms('2026-10-22') - 3 * DAY] * 3)

    def test_deadline_stops_pagination_and_uses_existing_data(self):
        clock = [0.]

        class Slow(FakeHttp):
            def get(self, url, params=None):
                clock[0] += 901
                return super().get(url, params)

        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'crypto/ext/daily/XUSDT.json'
            store.write_json(path, [['2026-10-21', 1, 2, .5, 1.5, 10, 1.5]])
            before = path.read_bytes()
            http = Slow()
            result = ext.fetch_extra_data(['XUSDT', 'YUSDT'], root, http=http, now_ms=NOW_MS,
                                           clock=lambda: clock[0], deadline=900, log=lambda *_: None)
            self.assertTrue(result['timed_out'])
            self.assertEqual(len(http.calls), 1)
            self.assertEqual(set(result['failed']), {'XUSDT', 'YUSDT'})
            self.assertEqual(path.read_bytes(), before)

    def test_real_http_retry_and_timeout_respect_budget_without_network(self):
        clock, sleeps = [0.], []

        def sleep(seconds):
            sleeps.append(seconds)
            clock[0] += seconds

        http = data_fetch.Http(proxy='', spacing=.8, sleep=sleep, clock=lambda: clock[0], deadline=1)
        with patch.object(http.opener, 'open', side_effect=OSError('fake network')) as request:
            with self.assertRaises(TimeoutError):
                http.get('https://example.invalid')
        self.assertAlmostEqual(clock[0], 1.)
        self.assertAlmostEqual(request.call_args.kwargs['timeout'], .2)
        self.assertEqual(request.call_count, 1)

    def test_metadata_or_corrupt_symbol_failures_do_not_raise(self):
        with tempfile.TemporaryDirectory() as root:
            store.write_json(Path(root) / 'crypto/ext/daily/XUSDT.json', {'malformed': 1})
            with patch.object(store.os, 'replace', side_effect=OSError('disk')):
                result = ext.fetch_extra_data(['XUSDT', 'YUSDT'], root, http=FakeHttp(), now_ms=NOW_MS,
                                               log=lambda *_: None)
            self.assertEqual(set(result['failed']), {'XUSDT', 'YUSDT'})


if __name__ == '__main__':
    unittest.main()
