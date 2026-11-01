"""Offline tests for crypto funding/premium side files and alt accumulation (stub HTTP)."""
import contextlib
import datetime as dt
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import urllib.error
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import alt_accum, data_fetch, run_mining, store
from scripts.factor_mining.panel import load_panels
from scripts.factor_mining.tests.synth import write_market

H = 3600_000
DAY = data_fetch.DAY_MS


def ms(y, m, d, hour=0, minute=0, sec=0, milli=0):
    t = dt.datetime(y, m, d, hour, minute, sec, tzinfo=dt.timezone.utc)
    return int(t.timestamp() * 1000) + milli


class StubHttp:
    """Serves fundingRate / premiumIndexKlines / futures-data pages from memory."""
    def __init__(self, funding=(), premium=(), stats=None, fail=()):
        self.funding, self.premium, self.stats = list(funding), list(premium), stats or {}
        self.fail, self.calls = set(fail), []

    def get(self, url, params=None):
        self.calls.append((url, dict(params or {})))
        symbol = params['symbol']
        if symbol in self.fail:
            raise RuntimeError(f'{url}: boom')
        if url.endswith('/fapi/v1/fundingRate'):
            rows = [r for r in self.funding if params['startTime'] <= r['fundingTime'] <= params['endTime']]
            return rows[:params['limit']]
        if url.endswith('/fapi/v1/premiumIndexKlines'):
            rows = [k for k in self.premium if k[0] >= params['startTime']]
            return rows[:params['limit']]
        return self.stats[url.rsplit('/', 1)[1]][-params['limit']:]


def funding(t, rate):
    return {'symbol': 'XUSDT', 'fundingTime': t, 'fundingRate': f'{rate:.8f}', 'markPrice': '1'}


def pk(open_ms, close):
    return [open_ms, '0', '0', '0', f'{close}', '0', open_ms + DAY - 1, '0', 0, '0', '0', '0']


class FundingDailyTests(unittest.TestCase):
    def test_settlement_belongs_to_its_own_utc_day_no_lookahead(self):
        # Day 2025-03-02: settlements at 00:00 (with a few ms jitter), 08:00, 16:00.
        # The 2025-03-03 00:00 settlement happens at the close of 03-02 and must not leak in.
        settlements = [(ms(2025, 3, 2, 0, 0, 0, 7), 0.0001), (ms(2025, 3, 2, 8), 0.0002),
                       (ms(2025, 3, 2, 16), 0.0003), (ms(2025, 3, 3, 0), 0.0100),
                       (ms(2025, 3, 3, 8), 0.0200)]
        rows = data_fetch.funding_daily(settlements, today='2025-03-04')
        self.assertEqual([r[0] for r in rows], ['2025-03-02', '2025-03-03'])
        self.assertAlmostEqual(rows[0][1], 0.0002)                  # mean of day-t settlements only
        self.assertAlmostEqual(rows[1][1], 0.0150)
        # Unfinished (today) days are never emitted.
        self.assertEqual([r[0] for r in data_fetch.funding_daily(settlements, today='2025-03-03')], ['2025-03-02'])

    def test_funding_rows_paginates_and_skips_today(self):
        now = ms(2025, 3, 10, 5)
        start = ms(2025, 3, 1)
        settlements = [funding(start + i * 8 * H, 1e-4 * (i // 3)) for i in range(28)]  # through 03-10 08:00
        settlements = [r for r in settlements if r['fundingTime'] <= now]
        http = StubHttp(funding=settlements)
        with patch.object(data_fetch, 'FUNDING_PAGE', 4):      # force several pages
            rows = data_fetch.funding_rows(http, 'XUSDT', start, now)
        self.assertEqual(rows[0][0], '2025-03-01')
        self.assertEqual(rows[-1][0], '2025-03-09')            # 03-10 is still forming
        self.assertAlmostEqual(rows[1][1], 1e-4)
        self.assertGreater(len(http.calls), 1)


class PremiumRowsTests(unittest.TestCase):
    def test_close_of_completed_days_only(self):
        now = ms(2025, 3, 5, 3)
        http = StubHttp(premium=[pk(ms(2025, 3, d), 0.001 * d) for d in (1, 2, 3, 4, 5)])
        rows = data_fetch.premium_rows(http, 'XUSDT', ms(2025, 3, 1), now)
        self.assertEqual(rows, [['2025-03-01', 0.001], ['2025-03-02', 0.002],
                                ['2025-03-03', 0.003], ['2025-03-04', 0.004]])


class UpdateAltTests(unittest.TestCase):
    def test_incremental_merge_failure_isolation_and_no_lookahead_in_panel(self):
        now = ms(2025, 3, 6, 1)
        settlements = [funding(ms(2025, 3, d, h), 1e-4 * d) for d in range(1, 7) for h in (0, 8, 16)]
        with tempfile.TemporaryDirectory() as root:
            http = StubHttp(funding=settlements, premium=[pk(ms(2025, 3, d), 1e-3 * d) for d in range(1, 7)],
                            fail={'BADUSDT'})
            result = data_fetch.update_alt(root, ['XUSDT', 'BADUSDT'], http, now, log=lambda *_: None)
            self.assertEqual(result['ok'], 2)
            self.assertEqual(sorted(result['failed']), ['funding/BADUSDT', 'premium/BADUSDT'])
            fpath = Path(root) / 'crypto/alt_daily/funding/XUSDT.json'
            first = json.loads(fpath.read_text())
            self.assertEqual([r[0] for r in first], [f'2025-03-0{d}' for d in range(1, 6)])
            # A later run only re-reads the last days and extends the file.
            later = ms(2025, 3, 8, 1)
            more = settlements + [funding(ms(2025, 3, 7, h), 7e-4) for h in (0, 8, 16)]
            http2 = StubHttp(funding=more, premium=[pk(ms(2025, 3, d), 1e-3 * d) for d in range(1, 8)])
            data_fetch.update_alt(root, ['XUSDT'], http2, later, log=lambda *_: None)
            second = json.loads(fpath.read_text())
            self.assertEqual([r[0] for r in second][-3:], ['2025-03-05', '2025-03-06', '2025-03-07'])
            self.assertLess(min(c[1]['startTime'] for c in http2.calls if c[1]['symbol'] == 'XUSDT'),
                            ms(2025, 3, 5))          # 3-day overlap, not a full refetch
            self.assertGreater(min(c[1]['startTime'] for c in http2.calls), ms(2025, 3, 1))

    def test_panel_value_on_day_t_ignores_settlements_after_t(self):
        with tempfile.TemporaryDirectory() as root:
            panels = write_market(root, rows=100, columns=2)
            dates = panels.mask.index
            t = 50
            t_ms = int(dates[t].timestamp() * 1000)
            settlements = [funding(t_ms + k * 8 * H, 1e-4 if k < 3 else 9.0) for k in range(-3, 6)]
            settlements = [dict(s, fundingTime=s['fundingTime']) for s in settlements]
            rows = data_fetch.funding_daily([(s['fundingTime'], float(s['fundingRate'])) for s in settlements],
                                            today='2100-01-01')
            path = Path(root) / 'crypto/alt_daily/funding/S000.json'
            path.parent.mkdir(parents=True)
            path.write_text(json.dumps(rows))
            fund = load_panels('crypto', root).fields['funding']['S000']
            self.assertAlmostEqual(fund.iloc[t], 1e-4)              # only t's own three settlements
            self.assertAlmostEqual(fund.iloc[t + 1], 9.0)
            self.assertAlmostEqual(fund.iloc[t - 1], 1e-4)

    def test_update_crypto_wires_alt_and_never_fails_main_flow(self):
        now = ms(2025, 3, 6, 1)
        klines = [[ms(2025, 3, d), '1', '2', '0.5', '1.5', '10', ms(2025, 3, d) + DAY - 1, '15']
                  for d in range(1, 6)]

        class Both(StubHttp):
            def get(self, url, params=None):
                if url.endswith('/fapi/v1/klines'):
                    return klines
                if url.endswith('fundingRate'):
                    raise RuntimeError('funding endpoint down')
                return super().get(url, params)

        with tempfile.TemporaryDirectory() as root:
            http = Both(premium=[pk(ms(2025, 3, d), 1e-3) for d in range(1, 7)])
            out = data_fetch.update('crypto', root=root, http=http, symbols=['XUSDT'], now_ms=now,
                                    log=lambda *_: None, accumulate=False)
            self.assertEqual(out['ok'], 1)
            self.assertEqual(out['failed'], {})
            meta = json.loads((Path(root) / 'crypto/data_meta.json').read_text())
            self.assertEqual(sorted(meta['alt']['failed']), ['funding/XUSDT'])
            self.assertTrue((Path(root) / 'crypto/daily/XUSDT.json').exists())
            self.assertTrue((Path(root) / 'crypto/alt_daily/premium/XUSDT.json').exists())
            self.assertFalse((Path(root) / 'alt').exists())          # accumulation is opt-in
            # US market and alt=False never touch the alt endpoints.
            http.calls.clear()
            data_fetch.update('crypto', root=root, http=http, symbols=['XUSDT'], now_ms=now,
                              log=lambda *_: None, alt=False)
            self.assertTrue(all('klines' in c[0] and 'premium' not in c[0] for c in http.calls))


class AccumTests(unittest.TestCase):
    def test_accumulates_finished_days_dedupes_and_survives_failures(self):
        now = ms(2025, 3, 10, 6)
        def rows(first_day, last_day, base=1.0):
            return [{'symbol': 'XUSDT', 'sumOpenInterest': str(base + d), 'sumOpenInterestValue': '5',
                     'timestamp': ms(2025, 3, d)} for d in range(first_day, last_day + 1)]
        http = StubHttp(stats={'openInterestHist': rows(1, 10), 'takerlongshortRatio': [
            {'buySellRatio': '1.1', 'buyVol': '3', 'sellVol': '2', 'timestamp': ms(2025, 3, 9)}]},
            fail={'BADUSDT'})
        with tempfile.TemporaryDirectory() as root:
            accum = Path(root) / 'alt'
            out = alt_accum.accumulate(['XUSDT', 'BADUSDT'], http, now, accum, kinds=['oi', 'taker'],
                                       log=lambda *_: None)
            self.assertEqual(out['ok'], 2)
            self.assertEqual(sorted(out['failed']), ['oi/BADUSDT', 'taker/BADUSDT'])
            stored = alt_accum.read_accum(accum, 'oi', 'XUSDT')
            self.assertEqual([r['date'] for r in stored], [f'2025-03-{d:02d}' for d in range(1, 10)])  # today dropped
            self.assertEqual(stored[0]['sumOpenInterest'], 2.0)
            self.assertNotIn('symbol', stored[0])
            self.assertEqual(alt_accum.days_accumulated(accum, 'oi', 'XUSDT'), 9)
            # Later run: window moved on; old days are kept and the overlap is not duplicated.
            http.stats['openInterestHist'] = rows(5, 12, base=100.)
            alt_accum.accumulate(['XUSDT'], http, ms(2025, 3, 13), accum, kinds=['oi'], log=lambda *_: None)
            stored = alt_accum.read_accum(accum, 'oi', 'XUSDT')
            self.assertEqual([r['date'] for r in stored], [f'2025-03-{d:02d}' for d in range(1, 13)])
            self.assertEqual(stored[0]['sumOpenInterest'], 2.0)            # kept
            self.assertEqual(stored[6]['sumOpenInterest'], 107.0)          # overlap refreshed once
            self.assertEqual(alt_accum.days_accumulated(accum, 'oi', 'XUSDT'), 12)
            self.assertEqual(alt_accum.read_accum(accum, 'oi', 'NOPE'), [])

    def test_update_accumulates_only_when_enabled_into_given_dir(self):
        now = ms(2025, 3, 6, 1)
        klines = [[ms(2025, 3, d), '1', '2', '0.5', '1.5', '10', ms(2025, 3, d) + DAY - 1, '15']
                  for d in range(1, 6)]
        oi = [{'sumOpenInterest': '1', 'timestamp': ms(2025, 3, d)} for d in range(1, 6)]

        class Many(StubHttp):
            def get(self, url, params=None):
                if url.endswith('/fapi/v1/klines'):
                    return klines
                if '/futures/data/' in url:
                    return oi
                return super().get(url, params)

        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as other:
            data_fetch.update('crypto', root=root, http=Many(), symbols=['XUSDT'], now_ms=now,
                              log=lambda *_: None, accumulate=True, accum_dir=other)
            self.assertEqual(sorted(p.name for p in Path(other).iterdir()), sorted(alt_accum.KINDS))
            self.assertFalse((Path(root) / 'alt').exists())


class FakeOpener:
    """Stands in for a urllib opener: ``script(url)`` returns a JSON-able value or raises."""
    def __init__(self, script):
        self.script, self.calls = script, []

    def open(self, req, timeout=None):
        self.calls.append(req.full_url)
        return io.BytesIO(json.dumps(self.script(req.full_url)).encode())


def failing(exc=ConnectionResetError(54, 'Connection reset by peer')):
    def script(url):
        raise exc
    return script


def make_http(script=None, proxy='', rand=lambda: 0.5, **kw):
    """Http with no real networking: fake openers, recorded sleeps, a frozen clock."""
    sleeps = []
    http = data_fetch.Http(spacing=0, proxy=proxy, sleep=sleeps.append, clock=lambda: 0.0, rand=rand, **kw)
    http.openers = [FakeOpener(script or failing()) for _ in http.openers]
    return http, sleeps


def backoffs(sleeps):
    return [x for x in sleeps if x > 0]


class HttpRetryTests(unittest.TestCase):
    def test_jitter_stays_within_25_percent_and_retries_are_bounded(self):
        for rand, factor in ((lambda: 0.0, 0.75), (lambda: 0.5, 1.0), (lambda: 1.0, 1.25)):
            http, sleeps = make_http(rand=rand, breaker=0)
            with self.assertRaises(RuntimeError):
                http.get('https://x/y', {'a': 1})
            self.assertEqual(len(http.opener.calls), data_fetch.RETRIES)        # finite: 5 attempts
            self.assertEqual(backoffs(sleeps), [2 * factor, 4 * factor, 8 * factor, 16 * factor])
        import random
        rng = random.Random(1)
        http, sleeps = make_http(rand=rng.random, breaker=0)
        for _ in range(20):
            with self.assertRaises(RuntimeError):
                http.get('https://x/y')
        base = [2, 4, 8, 16] * 20
        got = backoffs(sleeps)
        self.assertEqual(len(got), len(base))
        self.assertTrue(all(0.75 * b <= g <= 1.25 * b for b, g in zip(base, got)))
        self.assertGreater(len({round(g / b, 6) for b, g in zip(base, got)}), 10)   # actually random

    def test_success_returns_json(self):
        http, sleeps = make_http(lambda url: {'ok': 1})
        self.assertEqual(http.get('https://x/y'), {'ok': 1})
        self.assertEqual(backoffs(sleeps), [])


class HttpBreakerTests(unittest.TestCase):
    def test_opens_after_n_consecutive_failures_then_fails_instantly(self):
        http, sleeps = make_http(breaker=3)
        for _ in range(3):
            with self.assertRaises(RuntimeError) as cm:
                http.get('https://x/y')
            self.assertNotIsInstance(cm.exception, data_fetch.CircuitOpen)
        calls, slept = len(http.opener.calls), len(sleeps)
        self.assertEqual(calls, 3 * data_fetch.RETRIES)
        with self.assertRaises(data_fetch.CircuitOpen) as cm:
            http.get('https://x/z')
        self.assertIn('fetch circuit open', str(cm.exception))
        self.assertIn('Connection reset', str(cm.exception))
        self.assertEqual((len(http.opener.calls), len(sleeps)), (calls, slept))   # no request, no wait

    def test_success_resets_the_count(self):
        def script(url):
            if url.endswith('/ok'):
                return {'ok': True}
            raise ConnectionResetError('reset')
        http, _ = make_http(script, breaker=3)
        for _ in range(5):
            for _ in range(2):
                with self.assertRaises(RuntimeError):
                    http.get('https://x/bad')
            http.get('https://x/ok')
        self.assertIsNone(http.open_reason)
        for _ in range(3):
            with self.assertRaises(RuntimeError):
                http.get('https://x/bad')
        with self.assertRaises(data_fetch.CircuitOpen):
            http.get('https://x/ok')

    def test_http_4xx_means_network_is_fine(self):
        http, _ = make_http(failing(urllib.error.HTTPError('u', 400, 'bad symbol', {}, None)), breaker=2)
        for _ in range(6):
            with self.assertRaises(RuntimeError) as cm:
                http.get('https://x/y')
            self.assertNotIsInstance(cm.exception, data_fetch.CircuitOpen)
        self.assertEqual(len(http.opener.calls), 6)       # 4xx is not retried either

    def test_threshold_from_env_and_zero_disables(self):
        with patch.dict(os.environ, {'FM_FETCH_BREAKER': '2'}):
            http, _ = make_http()
        self.assertEqual(http.breaker, 2)
        with patch.dict(os.environ, {'FM_FETCH_BREAKER': '0'}):
            http, _ = make_http()
        for _ in range(12):
            with self.assertRaises(RuntimeError) as cm:
                http.get('https://x/y')
            self.assertNotIsInstance(cm.exception, data_fetch.CircuitOpen)
        with patch.dict(os.environ, {'FM_FETCH_BREAKER': 'junk'}):
            self.assertEqual(make_http()[0].breaker, data_fetch.BREAKER_DEFAULT)
        with patch.dict(os.environ):
            os.environ.pop('FM_FETCH_BREAKER', None)
            self.assertEqual(make_http()[0].breaker, 8)

    def test_update_fails_remaining_symbols_fast_after_circuit_opens(self):
        http, sleeps = make_http(breaker=2)
        symbols = [f'S{i}USDT' for i in range(6)]
        logs = []
        with tempfile.TemporaryDirectory() as root:
            out = data_fetch.update('crypto', root=root, http=http, symbols=symbols,
                                    now_ms=ms(2025, 3, 6, 1), log=logs.append, accumulate=False)
            self.assertEqual(out['ok'], 0)
            self.assertEqual(sorted(out['failed']), symbols)
            self.assertNotIn('circuit open', out['failed']['S0USDT'])
            self.assertNotIn('circuit open', out['failed']['S1USDT'])     # the request that tripped it
            for sym in symbols[2:]:
                self.assertIn('circuit open', out['failed'][sym])
            self.assertEqual(len(http.opener.calls), 2 * data_fetch.RETRIES)   # nothing after the trip
            self.assertEqual(len(backoffs(sleeps)), 2 * (data_fetch.RETRIES - 1))
            meta = json.loads((Path(root) / 'crypto/data_meta.json').read_text())
            self.assertEqual(len(meta['failed']), 6)

    def test_update_alt_fails_remaining_fast_after_circuit_opens(self):
        http, sleeps = make_http(breaker=2)
        with tempfile.TemporaryDirectory() as root:
            out = data_fetch.update_alt(root, ['AUSDT', 'BUSDT', 'CUSDT'], http, ms(2025, 3, 6, 1),
                                        log=lambda *_: None)
        self.assertEqual(out['ok'], 0)
        self.assertEqual(len(out['failed']), 6)
        self.assertEqual(sum('circuit open' in v for v in out['failed'].values()), 4)
        self.assertEqual(len(http.opener.calls), 2 * data_fetch.RETRIES)

    def test_update_raising_stub_circuit_open_is_handled_without_sleep(self):
        class Dead(StubHttp):
            def get(self, url, params=None):
                self.calls.append(url)
                raise data_fetch.CircuitOpen('fetch circuit open: proxy down')
        stub = Dead()
        with tempfile.TemporaryDirectory() as root:
            out = data_fetch.update('crypto', root=root, http=stub, symbols=['AUSDT', 'BUSDT', 'CUSDT'],
                                    now_ms=ms(2025, 3, 6, 1), log=lambda *_: None, alt=False)
        self.assertEqual(len(stub.calls), 1)                      # later symbols never reach http
        self.assertTrue(all('circuit open' in v for v in out['failed'].values()))
        self.assertEqual(len(out['failed']), 3)


class HttpProxyRotationTests(unittest.TestCase):
    def test_candidates_parse_from_comma_separated_string(self):
        http = data_fetch.Http(proxy=' http://a:1 , http://b:2,, ')
        self.assertEqual(http.proxies, ['http://a:1', 'http://b:2'])
        self.assertEqual(len(http.openers), 2)
        self.assertEqual(data_fetch.Http(proxy=['http://a:1']).proxies, ['http://a:1'])
        self.assertEqual(len(data_fetch.Http(proxy='').openers), 1)      # direct

    def test_default_proxy_is_unchanged_single_candidate(self):
        self.assertEqual(data_fetch._proxy_list('http://127.0.0.1:7890'), ['http://127.0.0.1:7890'])

    def test_rotates_after_consecutive_failures_and_stays_on_the_working_one(self):
        http, _ = make_http(proxy='http://a:1,http://b:2', breaker=0)
        http.openers = [FakeOpener(failing()), FakeOpener(lambda url: {'via': 'b'})]
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(http.get('https://x/1'), {'via': 'b'})
        self.assertEqual((len(http.openers[0].calls), len(http.openers[1].calls)), (data_fetch.ROTATE_AFTER, 1))
        self.assertEqual(http.proxy_index, 1)
        self.assertIn('http://b:2', err.getvalue())
        http.get('https://x/2')
        self.assertEqual(len(http.openers[0].calls), data_fetch.ROTATE_AFTER)    # sticky on b
        self.assertEqual(http.proxy_index, 1)

    def test_rotation_wraps_when_all_candidates_fail(self):
        http, _ = make_http(proxy='http://a:1,http://b:2', breaker=0)
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(RuntimeError):
            http.get('https://x/1')
        total = sum(len(o.calls) for o in http.openers)
        self.assertEqual(total, data_fetch.RETRIES)
        self.assertEqual(http.proxy_index, 0)           # a,a,b,b,a -> rotated a->b->a

    def test_single_candidate_never_rotates(self):
        http, _ = make_http(proxy='http://a:1', breaker=0)
        with self.assertRaises(RuntimeError):
            http.get('https://x/1')
        self.assertEqual(http.proxy_index, 0)
        self.assertEqual(len(http.opener.calls), data_fetch.RETRIES)


class FetchOnlyTests(unittest.TestCase):
    def run_cli(self, root, fetched, *more):
        update = (patch.object(run_mining.data_fetch, 'update', side_effect=fetched)
                  if isinstance(fetched, Exception) else
                  patch.object(run_mining.data_fetch, 'update', return_value=fetched))
        with update as mock, contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            code = run_mining.main(['--market', 'crypto', '--root', str(root), '--fetch-only', *more])
        return code, mock

    def test_exit_codes(self):
        cases = [
            ('all_ok', {'ok': 100, 'failed': {}}, 0),
            ('few_failures', {'ok': 90, 'failed': {f'S{i}': 'x' for i in range(10)}}, 0),
            ('exactly_25_percent', {'ok': 75, 'failed': {f'S{i}': 'x' for i in range(25)}}, 0),
            ('over_25_percent', {'ok': 74, 'failed': {f'S{i}': 'x' for i in range(26)}}, 3),
            ('everything_failed', {'ok': 0, 'failed': {f'S{i}': 'fetch circuit open' for i in range(100)}}, 3),
            ('exception', RuntimeError('exchangeInfo: reset'), 3),
        ]
        for name, fetched, want in cases:
            with self.subTest(name), tempfile.TemporaryDirectory() as root:
                code, mock = self.run_cli(root, fetched)
                self.assertEqual(code, want)
                mock.assert_called_once_with('crypto', root=Path(root))

    def test_status_reset_no_mining_no_run_record(self):
        with tempfile.TemporaryDirectory() as root, \
             patch.object(run_mining, 'Miner', side_effect=AssertionError('no mining')):
            code, _ = self.run_cli(root, {'ok': 1, 'failed': {}})
            self.assertEqual(code, 0)
            status = store.read_json(Path(root) / 'status.json')
            self.assertFalse(status['running'])
            self.assertIsNone(status['current'])
            self.assertFalse((Path(root) / 'runs.jsonl').exists())
            code, _ = self.run_cli(root, RuntimeError('boom'))
            self.assertEqual(code, 3)
            self.assertFalse(store.read_json(Path(root) / 'status.json')['running'])

    def test_holds_the_lock_while_fetching_and_respects_an_existing_one(self):
        with tempfile.TemporaryDirectory() as root:
            seen = []
            def update(market, root):
                try:
                    with store.FileLock(root):
                        seen.append('free')
                except store.AlreadyRunning:
                    seen.append('locked')
                    status = store.read_json(Path(root) / 'status.json')
                    seen.append(status['running'] and status['current']['stage'])
                return {'ok': 1, 'failed': {}}
            with patch.object(run_mining.data_fetch, 'update', side_effect=update), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(run_mining.main(['--market', 'crypto', '--root', root, '--fetch-only']), 0)
            self.assertEqual(seen, ['locked', 'fetch'])
            with store.FileLock(root):
                code, mock = self.run_cli(root, {'ok': 1, 'failed': {}})
            self.assertEqual(code, 2)
            mock.assert_not_called()

    def test_conflicts_with_no_fetch_and_all_markets_run_in_turn(self):
        with tempfile.TemporaryDirectory() as root, contextlib.redirect_stderr(io.StringIO()), \
             self.assertRaises(SystemExit):
            run_mining.main(['--market', 'crypto', '--root', root, '--fetch-only', '--no-fetch'])
        with tempfile.TemporaryDirectory() as root:
            results = {'crypto': {'ok': 1, 'failed': {}}, 'us': {'ok': 0, 'failed': {'A': 'x'}}}
            with patch.object(run_mining.data_fetch, 'update', side_effect=lambda m, root: results[m]) as mock, \
                 contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(run_mining.main(['--market', 'all', '--root', root, '--fetch-only']), 3)
            self.assertEqual([c.args[0] for c in mock.call_args_list], ['crypto', 'us'])


FAKE_PYTHON = """#!/bin/sh
# Fake interpreter: logs every call; --fetch-only exits per the per-market script in $FAKE_DIR.
echo "$*" >> "$FAKE_DIR/calls.log"
case "$*" in
*--fetch-only*)
    market=$4
    n=$(cat "$FAKE_DIR/$market.n" 2>/dev/null || echo 0)
    echo $((n + 1)) > "$FAKE_DIR/$market.n"
    code=$(sed -n "$((n + 1))p" "$FAKE_DIR/$market.codes")
    echo "fake fetch $market round $((n + 1)) -> ${code:-0}"
    exit "${code:-0}" ;;
esac
exit 0
"""


class NightlyScriptTests(unittest.TestCase):
    SCRIPT = Path(__file__).resolve().parents[1] / 'run_nightly.sh'

    def run_script(self, codes, **env):
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp) / 'python'
            fake.write_text(FAKE_PYTHON)
            fake.chmod(0o755)
            for market, seq in codes.items():
                (Path(tmp) / f'{market}.codes').write_text(''.join(f'{c}\n' for c in seq))
            root = Path(tmp) / 'fm'
            full = dict(os.environ, AR_PYTHON=str(fake), FAKE_DIR=tmp, FACTOR_MINING_DIR=str(root),
                        FM_FETCH_RETRY_SLEEP='0', PROMOTION_DIR=str(Path(tmp) / 'promo'),
                        NOFX_DB=str(Path(tmp) / 'none.db'))
            full.update(env)
            result = subprocess.run(['sh', str(self.SCRIPT)], env=full, capture_output=True, text=True, timeout=60)
            calls = (Path(tmp) / 'calls.log').read_text().splitlines()
            return result, (root / 'nightly.log').read_text(), calls

    def test_retries_until_fetch_succeeds_then_mines(self):
        result, log, calls = self.run_script({'crypto': [3, 3, 0], 'us': [0]})
        self.assertEqual(result.returncode, 0)
        fetches = [c for c in calls if '--fetch-only' in c]
        self.assertEqual(sum('--market crypto' in c for c in fetches), 3)
        self.assertEqual(sum('--market us' in c for c in fetches), 1)
        self.assertIn('fetch preflight OK: crypto', log)
        self.assertNotIn('fetch preflight FAILED', log)
        mining = [c for c in calls if '--fetch-only' not in c and 'run_mining' in c]
        self.assertEqual(len(mining), 2)
        # The pre-flight of a market precedes its mining run.
        self.assertLess(calls.index(next(c for c in calls if '--market crypto --fetch-only' in c)),
                        calls.index(next(c for c in mining if '--market crypto' in c)))

    def test_gives_up_after_retries_but_still_runs_the_normal_flow(self):
        result, log, calls = self.run_script({'crypto': [3] * 10, 'us': [3] * 10}, FM_FETCH_RETRIES='2')
        self.assertEqual(result.returncode, 0)
        self.assertEqual(sum('--market crypto --fetch-only' in c for c in calls), 2)
        self.assertEqual(log.count('fetch preflight FAILED'), 2)
        self.assertEqual(len([c for c in calls if '--fetch-only' not in c and 'run_mining' in c]), 2)
        self.assertIn('promotion report', log)

    def test_time_budget_caps_the_rounds(self):
        result, log, calls = self.run_script({'crypto': [3] * 10, 'us': [0]}, FM_FETCH_RETRIES='9',
                                             FM_FETCH_RETRY_SLEEP='5', FM_FETCH_BUDGET='5')
        self.assertEqual(sum('--market crypto --fetch-only' in c for c in calls), 1)
        self.assertIn('budget', log)
        self.assertIn('fetch preflight FAILED: crypto', log)

    def test_lock_held_elsewhere_does_not_loop_and_zero_retries_disables(self):
        _, log, calls = self.run_script({'crypto': [2] * 10, 'us': [0]})
        self.assertEqual(sum('--market crypto --fetch-only' in c for c in calls), 1)
        self.assertIn('holds the lock', log)
        _, log, calls = self.run_script({}, FM_FETCH_RETRIES='0')
        self.assertFalse(any('--fetch-only' in c for c in calls))
        self.assertNotIn('fetch preflight', log)


if __name__ == '__main__':
    unittest.main()
