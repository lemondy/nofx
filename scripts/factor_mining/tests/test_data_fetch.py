"""Offline tests for crypto funding/premium side files and alt accumulation (stub HTTP)."""
import datetime as dt
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import alt_accum, data_fetch
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


if __name__ == '__main__':
    unittest.main()
