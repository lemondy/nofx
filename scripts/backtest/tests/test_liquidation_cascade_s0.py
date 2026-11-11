"""Synthetic-only tests; never access market data, trade outcomes, or the network."""
from argparse import Namespace
from contextlib import ExitStack, redirect_stderr
from datetime import timedelta
import hashlib
import http.client
import io
import json
import math
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
import urllib.error
import zipfile

import numpy as np
import pandas as pd

from scripts.backtest import liquidation_cascade_s0 as study

HOUR, DAY, STEP = study.HOUR, study.DAY, study.STEP
BASE = study.ms('2023-01-01')
B_BASE = study.ms('2026-09-29')


def zip_bytes(text):
    result = io.BytesIO()
    with zipfile.ZipFile(result, 'w', zipfile.ZIP_DEFLATED) as archive:
        archive.writestr('synthetic.csv', text)
    return result.getvalue()


def write_zip(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(zip_bytes(text))


def bars(start=BASE, hours=240, price=100.):
    index = np.arange(start, start + hours * HOUR, STEP, dtype=np.int64)
    return pd.DataFrame(dict(open=price, high=price, low=price, close=price), index=index)


def feature_frame(times, *, r=-.04, sigma=.01, doi=math.log(.95)):
    return pd.DataFrame(dict(r_1h=r, sigma=sigma, dOI_1h=doi), index=np.array(times, dtype=np.int64))


def bars_csv(frame, *, header=False, micros=False):
    lines = ['open_time,open,high,low,close,volume,close_time'] if header else []
    for t, row in frame.iterrows():
        stamp = int(t) * (1000 if micros else 1)
        lines.append(','.join(map(str, [stamp, row['open'], row['high'], row['low'], row['close'], 1, stamp + STEP - 1])))
    return '\n'.join(lines) + '\n'


def metrics_csv(day, symbol='TESTUSDT', n=288, drop_at=None):
    lines = ['create_time,symbol,sum_open_interest,sum_open_interest_value']
    start = study.ms(day)
    for i in range(n):
        t = start + i * STEP
        oi = 94 if t == drop_at else 100
        lines.append(f'{pd.Timestamp(t, unit="ms").strftime("%Y-%m-%d %H:%M:%S")},{symbol},{oi},10000')
    return '\n'.join(lines) + '\n'


def daily_rows(start, n, *, volume=1, price=100):
    return [[d.isoformat(), price, price, price, price, volume, price]
            for d in study.dates(start, pd.Timestamp(start).date() + timedelta(days=n))]


class PoolAndPlanTests(unittest.TestCase):
    def test_top60_listing_cutoff_missing_days_and_no_future_turnover(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for i in range(61):
                rows = daily_rows('2022-12-02', 30, volume=i + 1)
                if i == 0:
                    rows.insert(0, ['2022-12-01', 100, 100, 100, 100, 1e15, 100])
                    rows.append(['2023-01-01', 100, 100, 100, 100, 1e15, 100])
                study.write_json(root / f'T{i:03}USDT.json', rows)
            study.write_json(root / 'NEWUSDT.json', daily_rows('2022-12-03', 29, volume=1e15))
            study.write_json(root / 'SINGLEUSDT.json', daily_rows('2022-12-02', 1, volume=99999))
            pools, hashes = study.select_pools(root, ['2023-01'])
            chosen = pools['2023-01']
            self.assertEqual(len(chosen), 60)
            self.assertEqual(chosen, ['SINGLEUSDT'] + [f'T{i:03}USDT' for i in range(60, 1, -1)])
            self.assertNotIn('NEWUSDT', chosen)
            self.assertEqual(len(hashes), 63)

    def test_plan_warmup_cross_month_b_daily_and_deduped_urls(self):
        pools = {'2023-01': ['TESTUSDT'], '2023-02': ['TESTUSDT']}
        trade = dict(symbol='OTHERUSDT', side='SHORT', entry_time=B_BASE)
        with patch.object(study, 'select_pools', return_value=(pools, {'TESTUSDT.json': 'hash'})):
            plan = study.make_plan('/synthetic', [trade])
        files = plan['files']
        self.assertEqual(len({r['path'] for r in files}), len(files))
        metrics = {r['period'] for r in files if r['symbol'] == 'TESTUSDT' and r['kind'] == 'metrics'}
        self.assertEqual(min(metrics), '2022-12-24')
        self.assertEqual(max(metrics), '2023-02-28')
        klines = [r for r in files if r['symbol'] == 'TESTUSDT' and r['kind'] == 'klines5m']
        self.assertEqual({r['period'] for r in klines}, {'2022-12', '2023-01', '2023-02', '2023-03-01', '2023-03-02'})
        other = [r for r in files if r['symbol'] == 'OTHERUSDT']
        self.assertEqual({r['period'] for r in other if r['kind'] == 'metrics'},
                         {d.isoformat() for d in study.dates('2026-09-22', '2026-11-09')})
        november = [r for r in other if r['kind'] == 'klines5m' and r['period'].startswith('2026-11')]
        self.assertEqual(len(november), 9)
        self.assertTrue(all('/daily/klines/OTHERUSDT/5m/' in r['url'] for r in november))
        self.assertEqual(plan['counts']['total'], len(files))
        self.assertIn('BTCUSDT', {r['symbol'] for r in files})

    def test_default_plan_has_forty_months_and_btc_entire_a_range(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            study.write_json(root / 'TESTUSDT.json', daily_rows('2022-12-01', 1))
            plan = study.make_plan(root, [dict(symbol='TESTUSDT', side='SHORT', entry_time=B_BASE)])
        self.assertEqual(len(plan['pools']), 40)
        btc_metrics = {r['period'] for r in plan['files'] if r['symbol'] == 'BTCUSDT' and r['kind'] == 'metrics'}
        self.assertTrue({'2022-12-24', '2026-04-30'} <= btc_metrics)
        btc_k = {r['period'] for r in plan['files'] if r['symbol'] == 'BTCUSDT' and r['kind'] == 'klines5m'}
        self.assertTrue({'2022-12', '2026-04', '2026-05-01', '2026-05-02'} <= btc_k)


class DefinitionTests(unittest.TestCase):
    def test_sigma_excludes_current_and_future_and_requires_150_valid_values(self):
        original = bars(hours=230)
        t = BASE + 200 * HOUR
        hour_ends = original.index + STEP
        # The ending-at-t close is a very large shock in just one variant.
        original['close'] = 100 * np.exp(.0001 * np.sin((hour_ends - BASE) / HOUR))
        original['high'] = original[['open', 'close']].max(axis=1)
        original['low'] = original[['open', 'close']].min(axis=1)
        changed = original.copy()
        changed.loc[t - STEP, ['close', 'low']] = .1
        changed.loc[changed.index >= t, ['open', 'close', 'high', 'low']] = 1e6
        oi = pd.Series(100., index=np.arange(BASE, BASE + 231 * HOUR, HOUR))
        first, second = study.hourly_features(original, oi), study.hourly_features(changed, oi)
        self.assertAlmostEqual(first.loc[t, 'sigma'], second.loc[t, 'sigma'], places=14)
        self.assertLess(second.loc[t, 'r_1h'], -6)
        expected = first.loc[t - 168 * HOUR:t - HOUR, 'r_1h'].std(ddof=1)
        self.assertAlmostEqual(first.loc[t, 'sigma'], expected, places=14)
        self.assertTrue(pd.isna(first.loc[BASE + 150 * HOUR, 'sigma']))
        self.assertTrue(pd.isna(first.loc[BASE + 151 * HOUR, 'sigma']))
        self.assertTrue(pd.notna(first.loc[BASE + 152 * HOUR, 'sigma']))
        self.assertEqual(first.loc[t, 'close'], original.loc[t - STEP, 'close'])
        # A missing previous endpoint cannot be filled using the next observation.
        missing = original.drop(t - HOUR - STEP)
        missing_features = study.hourly_features(missing, oi.drop(t - HOUR))
        self.assertTrue(pd.isna(missing_features.loc[t, 'r_1h']))
        self.assertTrue(pd.isna(missing_features.loc[t, 'dOI_1h']))

    def test_c_d_middle_exact_log_threshold_and_crash_boundary(self):
        boundary = math.log(.95)
        doi = [boundary, np.nextafter(boundary, -np.inf), np.nextafter(boundary, np.inf),
               -.01, np.nextafter(-.01, np.inf), math.log(.99), np.nan, 0, boundary]
        frame = feature_frame(BASE + np.arange(len(doi)) * HOUR)
        frame['dOI_1h'] = doi
        frame['r_1h'] = -.03
        frame.iloc[-2, frame.columns.get_loc('r_1h')] = np.nextafter(-.03, np.inf)
        frame.iloc[-1, frame.columns.get_loc('sigma')] = np.nan
        self.assertEqual(study.classify(frame).tolist(), ['C', 'C', 'M', 'M', 'D', 'M', '', '', ''])
        for m in study.MS:
            frame = feature_frame([BASE], doi=math.log(1 - m))
            self.assertEqual(study.classify(frame, m=m).iloc[0], 'C')

    def test_24h_dedupe_is_per_symbol_category_and_kept_anchor(self):
        t = np.array([0, 1, 23, 24, 25, 47, 48]) * HOUR + BASE
        frame = pd.DataFrame(dict(category=['C', 'D', 'C', 'C', 'D', 'C', 'C']), index=t)
        selected = study.deduplicate(frame)
        self.assertEqual(selected.index.tolist(), list(t[[0, 1, 3, 4, 6]]))
        mixed = pd.DataFrame(dict(category=['C', 'C', 'C'], symbol=['A', 'B', 'A']),
                             index=[BASE, BASE + HOUR, BASE + 2 * HOUR])
        self.assertEqual(study.deduplicate(mixed)['symbol'].tolist(), ['A', 'B'])

    def test_delayed_entry_end_price_all_horizons_and_btc_subtraction(self):
        t = BASE + HOUR
        raw = bars(hours=30)
        raw.loc[t, ['open', 'high', 'low', 'close']] = 500  # skipped bar
        raw.loc[t + STEP, ['open', 'high', 'low', 'close']] = 80
        for h in study.HORIZONS:
            raw.loc[t + h * HOUR, ['open', 'high', 'low', 'close']] = 80 * math.exp(h / 100)
        benchmark = bars(hours=30)
        benchmark.loc[t + STEP, ['open', 'high', 'low', 'close']] = 50
        for h in study.HORIZONS:
            benchmark.loc[t + h * HOUR, ['open', 'high', 'low', 'close']] = 50 * math.exp(h / 200)
        btc = study.forward_returns(benchmark, [t])
        candidates = feature_frame([t]).assign(category='C')
        events, counts = study.attach_returns(candidates, raw, btc, 'TESTUSDT')
        self.assertEqual(counts['forward_missing'], 0)
        for h in study.HORIZONS:
            self.assertAlmostEqual(events.loc[t, f'R_{h}h'], h / 100)
            self.assertAlmostEqual(events.loc[t, f'R_{h}h_ex_btc'], h / 200)
        own, _ = study.attach_returns(candidates, raw, btc, 'BTCUSDT')
        self.assertTrue(own.filter(like='_ex_btc').isna().all().all())

    def test_missing_internal_bar_drops_event_and_no_dedupe_replacement(self):
        t = BASE + HOUR
        raw = bars(hours=30).drop(t + 23 * HOUR + STEP)
        candidates = feature_frame([t, t + HOUR]).assign(category='C')
        chosen = study.deduplicate(candidates)
        events, counts = study.attach_returns(chosen, raw, symbol='TESTUSDT')
        self.assertTrue(events.empty)
        self.assertEqual(counts['candidates'], 1)
        self.assertEqual(counts['forward_missing'], 1)
        self.assertEqual(counts['forward_missing_by_h'], {'1': 0, '4': 0, '12': 0, '24': 1})
        self.assertTrue(pd.notna(study.forward_returns(raw, [t], (4,)).iloc[0, 0]))
        self.assertTrue(study.forward_returns(bars(hours=1), [t]).isna().all().all())

    def test_missing_btc_does_not_change_primary_raw_sample(self):
        t = BASE + HOUR
        candidates = feature_frame([t]).assign(category='C')
        btc = study.forward_returns(bars(hours=30).drop(t + 3 * HOUR), [t])
        events, counts = study.attach_returns(candidates, bars(hours=30), btc, 'TESTUSDT')
        self.assertEqual(len(events), 1)
        self.assertEqual(counts['btc_forward_missing'], 1)
        self.assertTrue(events.filter(like='_ex_btc').isna().all().all())


class StatisticsTests(unittest.TestCase):
    def test_cr1_matches_hand_computed_matrix_sandwich_and_streaming(self):
        # Two observations in each of three UTC-day clusters; groups overlap dates.
        frame = pd.DataFrame(dict(category=['C', 'D', 'C', 'D', 'C', 'D'],
                                  R_4h=[.04, .01, .02, -.02, -.01, .00]),
                             index=BASE + np.array([0, 1, 24, 25, 48, 49]) * HOUR)
        x = np.column_stack((np.ones(6), [1, 0, 1, 0, 1, 0]))
        y = frame['R_4h'].to_numpy()
        bread = np.linalg.inv(x.T @ x)
        beta = bread @ x.T @ y
        residual = y - x @ beta
        meat = np.zeros((2, 2))
        for i in (0, 2, 4):
            score = x[i:i + 2].T @ residual[i:i + 2]
            meat += np.outer(score, score)
        covariance = 3 / 2 * 5 / 4 * bread @ meat @ bread
        expected_se = math.sqrt(covariance[1, 1])
        result = study.cluster_ols(frame)
        self.assertAlmostEqual(result['delta'], beta[1], places=14)
        self.assertAlmostEqual(result['SE'], expected_se, places=14)
        self.assertAlmostEqual(result['t'], beta[1] / expected_se, places=14)
        self.assertEqual(result['clusters'], 3)
        accumulator = study.CR1Stats()
        accumulator.add(frame.iloc[:3])
        accumulator.add(frame.iloc[3:])
        self.assertEqual(accumulator.summary(), result)

    def test_cr1_undefined_small_and_zero_variance_samples(self):
        for categories, offsets in [(['C'], [0]), (['C', 'D'], [0, 1]),
                                     (['C', 'D', 'C', 'D'], [0, 1, 24, 25])]:
            frame = pd.DataFrame(dict(category=categories, R_4h=.1), index=BASE + np.array(offsets) * HOUR)
            self.assertIsNone(study.cluster_ols(frame)['t'])
        empty = pd.DataFrame(columns=['category', 'R_4h'], index=pd.Index([], dtype='int64'))
        self.assertIsNone(study.cluster_ols(empty)['delta'])

    def test_four_verdict_rules_fail_individually_and_inclusive_thresholds(self):
        main = dict(N_C=100, N_D=100, delta=.01, t=2., mean_C=.02)
        halves = dict(H1=dict(delta=.001), H2=dict(delta=.002))
        self.assertEqual(study.decision(main, halves)['verdict'], 'holds')
        changes = [('sample_size', dict(N_C=99)), ('sample_size', dict(N_D=99)),
                   ('positive_significant_delta', dict(delta=0)),
                   ('positive_significant_delta', dict(t=1.999)),
                   ('positive_significant_delta', dict(t=None)),
                   ('positive_c_mean', dict(mean_C=0))]
        for key, update in changes:
            with self.subTest(update=update):
                result = study.decision(dict(main, **update), halves)
                self.assertFalse(result['mechanism_holds'])
                self.assertFalse(result['checks'][key])
                self.assertEqual(sum(not v for v in result['checks'].values()), 1)
                self.assertEqual(result['verdict'], 'insufficient' if key == 'sample_size' else 'not_holds')
        for half in ('H1', 'H2'):
            for delta in (0, -.01, None):
                result = study.decision(main, dict(halves, **{half: dict(delta=delta)}))
                self.assertEqual(result['verdict'], 'not_holds')
                self.assertFalse(result['checks']['both_halves_positive'])

    def test_s1_requires_mechanism_and_at_least_ten_percent_shorts(self):
        self.assertTrue(study.qualifies_for_s1(True, .10))
        self.assertTrue(study.qualifies_for_s1(True, 31 / 309))
        self.assertFalse(study.qualifies_for_s1(True, 30 / 309))
        self.assertFalse(study.qualifies_for_s1(True, None))
        self.assertFalse(study.qualifies_for_s1(False, 1.))


class TradeCountTests(unittest.TestCase):
    def test_missing_prices_do_not_misreport_present_oi_as_missing(self):
        oi = pd.Series(100., index=B_BASE + np.arange(-6, 8) * HOUR)
        features = study.hourly_features(bars(start=B_BASE, hours=0), oi).assign(category='')
        result = study.trade_overlap([dict(symbol='TESTUSDT', side='SHORT', entry_time=B_BASE + HOUR)],
                                     {'TESTUSDT': features})
        self.assertEqual(result['missing_oi_trades'], 0)
        self.assertEqual(result['by_side']['SHORT']['missing_signal_inputs'], 1)

    def test_open_closed_interval_side_counts_and_no_outcome_access(self):
        times = B_BASE + np.arange(-5, 11) * HOUR
        frame = feature_frame(times, doi=0).assign(category='')
        frame.loc[B_BASE, 'category'] = 'C'
        offsets = [0, 1, 4 * HOUR, 4 * HOUR + 1, HOUR, 7 * HOUR]

        class ForbiddenFields(dict):
            def __getitem__(self, key):
                if key not in ('symbol', 'side', 'entry_time'):
                    raise AssertionError(f'forbidden access: {key}')
                return super().__getitem__(key)

            def get(self, key, default=None):
                return self[key]

        trades = [ForbiddenFields(symbol='TESTUSDT', side='SHORT' if i < 4 else 'LONG',
                                  entry_time=B_BASE + offset, r=object(), price=object(), pnl=object())
                  for i, offset in enumerate(offsets)]
        result = study.trade_overlap(trades, {'TESTUSDT': frame})
        self.assertEqual(result['by_side']['SHORT']['after_cascade'], 2)
        self.assertEqual(result['by_side']['SHORT']['fraction'], .5)
        self.assertEqual(result['by_side']['LONG']['after_cascade'], 1)
        self.assertEqual(result['by_side']['LONG']['fraction'], .5)
        self.assertEqual(result['missing_oi_trades'], 0)
        frame.loc[B_BASE + 2 * HOUR, 'dOI_1h'] = np.nan
        second = study.trade_overlap(trades, {'TESTUSDT': frame})
        self.assertEqual(second['by_side']['SHORT']['missing_oi'], 2)
        self.assertEqual(second['by_side']['SHORT']['fraction'], .5)  # full denominator
        absent = study.trade_overlap(trades, {})
        self.assertEqual(absent['missing_oi_trades'], 6)

    def test_jsonl_projects_three_fields_hash_guard_and_r_is_irrelevant(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'trades.jsonl'
            results = []
            for r in (-10000, {'would': 'reveal future P&L'}, None):
                path.write_text(json.dumps(dict(symbol='TESTUSDT', side='SHORT', entry_time=B_BASE + HOUR,
                                                r=r, entry_price=123, realized_pnl=999)) + '\n')
                with patch.object(study, 'TRADES_SHA256', study.file_sha256(path)), \
                        patch.object(study, 'N_TRADES', 1), patch.object(study, 'N_TRADE_SYMBOLS', 1):
                    results.append(study.load_trades(path))
            self.assertEqual(results[0], results[1])
            self.assertEqual(results[1], results[2])
            self.assertEqual(set(results[0][0]), {'symbol', 'side', 'entry_time'})
            with self.assertRaisesRegex(ValueError, 'trades sha256'):
                study.load_trades(path)


class ArchiveAndCheckTests(unittest.TestCase):
    def test_kline_headers_ms_microseconds_and_duplicate_invalid_ohlc(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            expected = bars(hours=2)
            for header in (False, True):
                for micros in (False, True):
                    path = root / 'bars.zip'
                    write_zip(path, bars_csv(expected, header=header, micros=micros))
                    pd.testing.assert_frame_equal(study.read_klines([path]), expected, check_dtype=False)
            with self.assertRaisesRegex(ValueError, '重复'):
                study.read_klines([path, path])
            illegal = expected.copy()
            illegal.iloc[0, illegal.columns.get_loc('high')] = 99
            write_zip(path, bars_csv(illegal))
            with self.assertRaisesRegex(ValueError, 'OHLC'):
                study.read_klines([path])
            write_zip(path, bars_csv(expected).replace(str(BASE), str(BASE + 1), 1))
            with self.assertRaisesRegex(ValueError, '时间戳'):
                study.read_klines([path])

    def test_metrics_280_day_rule_nonpositive_oi_and_exact_hour_coverage(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            paths = []
            for day, n in [('2023-01-01', 279), ('2023-01-02', 280), ('2023-01-03', 288)]:
                path = root / f'TESTUSDT-metrics-{day}.zip'
                content = metrics_csv(day, n=n)
                if n == 288:
                    content = content.replace('00:00:00,TESTUSDT,100,', '00:00:00,TESTUSDT,0,')
                write_zip(path, content)
                paths.append(path)
            oi, meta = study.read_metrics(paths, 'TESTUSDT', ['2023-01-01', '2023-01-02', '2023-01-03', '2023-01-04'])
            self.assertNotIn(BASE, oi.index)
            self.assertEqual(oi.loc[BASE + DAY], 100)
            self.assertTrue(pd.isna(oi.loc[BASE + 2 * DAY]))
            self.assertEqual(meta['days_below_280'], ['2023-01-01'])
            self.assertEqual(meta['absent_days'], ['2023-01-04'])
            self.assertEqual(meta['valid_hourly_oi'], 47)
            self.assertAlmostEqual(meta['hourly_coverage'], 47 / 96)
            write_zip(paths[-1], metrics_csv('2023-01-03').replace('TESTUSDT', 'OTHERUSDT'))
            with self.assertRaisesRegex(ValueError, 'metrics'):
                study.read_metrics(paths, 'TESTUSDT', [])

    def test_metrics_drops_offgrid_and_next_day_rows_then_applies_280_rule(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            # Shifted day: 00:05 .. next-day 00:00 (288 rows, last one out of day).
            shifted = metrics_csv('2023-01-01').splitlines()
            shifted = [shifted[0]] + [
                f'{pd.Timestamp(BASE + (i + 1) * STEP, unit="ms").strftime("%Y-%m-%d %H:%M:%S")},TESTUSDT,100,10000'
                for i in range(288)]
            a = root / 'TESTUSDT-metrics-2023-01-01.zip'
            write_zip(a, '\n'.join(shifted) + '\n')
            # Whole day off-grid by 46 seconds -> no usable rows.
            b = root / 'TESTUSDT-metrics-2023-01-02.zip'
            write_zip(b, metrics_csv('2023-01-02').replace(':00,TESTUSDT', ':46,TESTUSDT'))
            oi, meta = study.read_metrics([a, b], 'TESTUSDT', ['2023-01-01', '2023-01-02'])
            self.assertEqual(meta['dropped_rows'], {'2023-01-01': 1, '2023-01-02': 288})
            self.assertEqual(meta['day_row_counts'], {'2023-01-01': 287, '2023-01-02': 0})
            self.assertEqual(meta['days_below_280'], ['2023-01-02'])
            self.assertNotIn(BASE, oi.index)
            self.assertNotIn(BASE + DAY, oi.index)
            self.assertEqual(oi.loc[BASE + HOUR], 100)

    def test_daily_check_excludes_incomplete_days_and_reports_mismatch(self):
        raw = bars(hours=72)
        daily = pd.DataFrame(dict(close=[100., 99., 100.]),
                             index=pd.to_datetime(['2023-01-01', '2023-01-02', '2023-01-03']))
        result = study.daily_close_check(raw.drop(BASE + 2 * DAY + STEP), daily)
        self.assertEqual(result['compared'], 2)
        self.assertEqual(result['mismatches'], 1)
        self.assertEqual(result['incomplete_days'], 1)
        self.assertEqual(result['mismatch_days'][0]['day'], '2023-01-02')


class FetchTests(unittest.TestCase):
    def test_404_retry_atomic_write_and_verified_resume(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            files = [dict(path=f'raw/metrics/TESTUSDT/{name}.zip', url=f'https://synthetic/{name}')
                     for name in ('ok', 'retry', 'missing', 'existing', 'corrupt')]
            plan = dict(files=files)
            study.write_json(root / 'plan.json', plan)
            payload = zip_bytes('header\nvalue\n')
            existing = root / files[3]['path']
            existing.parent.mkdir(parents=True)
            existing.write_bytes(payload)
            corrupt = root / files[4]['path']
            corrupt.write_bytes(b'corrupt')
            study.write_json(root / 'manifest.json', dict(version=1, missing=[], errors=[], files={
                files[3]['path']: dict(sha256=study.file_sha256(existing)),
                files[4]['path']: dict(sha256=hashlib.sha256(payload).hexdigest())}))
            calls = []

            def fetch(url, *, proxy, timeout):
                self.assertEqual(proxy, 'http://synthetic-proxy')
                self.assertEqual(timeout, 60)
                calls.append(url)
                if url.endswith('missing'):
                    raise urllib.error.HTTPError(url, 404, 'missing', {}, None)
                if url.endswith('retry') and calls.count(url) < 3:
                    raise urllib.error.URLError('temporary')
                return payload

            original_replace = Path.replace
            atomic = []

            def replace(path, target):
                if path.name.endswith('.part'):
                    self.assertTrue(path.exists())
                    self.assertEqual(path.read_bytes(), payload)
                    atomic.append(target)
                return original_replace(path, target)

            sleeper = Mock()
            with patch.object(Path, 'replace', replace):
                manifest = study.fetch_plan(root, plan, workers=1, proxy='http://synthetic-proxy',
                                            fetch=fetch, sleeper=sleeper)
            self.assertEqual(len(manifest['files']), 4)
            self.assertEqual(len(manifest['missing']), 1)
            self.assertEqual(manifest['missing'][0]['status'], 404)
            self.assertEqual(calls.count('https://synthetic/retry'), 3)
            self.assertEqual(calls.count('https://synthetic/missing'), 1)
            self.assertNotIn('https://synthetic/existing', calls)
            self.assertEqual(len(atomic), 3)
            self.assertEqual(sleeper.call_count, 2)
            self.assertFalse(list(root.rglob('*.part')))
            forbidden = Mock(side_effect=AssertionError('verified files and 404 must be skipped'))
            resumed = study.fetch_plan(root, plan, fetch=forbidden)
            forbidden.assert_not_called()
            self.assertEqual(resumed, manifest)

    def test_non404_three_retries_error_manifest_and_successful_later_resume(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            item = dict(path='raw/metrics/TESTUSDT/error.zip', url='https://synthetic/error')
            plan = dict(files=[item])
            study.write_json(root / 'plan.json', plan)
            failing = Mock(side_effect=urllib.error.HTTPError(item['url'], 503, 'temporary', {}, None))
            sleeper = Mock()
            result = study.fetch_plan(root, plan, fetch=failing, sleeper=sleeper)
            self.assertEqual(failing.call_count, 4)
            self.assertEqual(sleeper.call_count, 3)
            self.assertEqual(result['errors'][0]['attempts'], 4)
            self.assertFalse((root / item['path']).exists())
            self.assertFalse(list(root.rglob('*.part')))
            good = Mock(return_value=zip_bytes('a\n1\n'))
            resumed = study.fetch_plan(root, plan, fetch=good)
            self.assertEqual(resumed['errors'], [])
            self.assertEqual(resumed['files'][item['path']]['sha256'], study.file_sha256(root / item['path']))

    def test_truncated_http_body_and_invalid_zip_are_retried_without_partial_final(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            item = dict(path='raw/metrics/TESTUSDT/error.zip', url='https://synthetic/error')
            plan = dict(files=[item])
            study.write_json(root / 'plan.json', plan)
            payload = zip_bytes('a\n1\n')
            fetch = Mock(side_effect=[http.client.IncompleteRead(b'partial'), b'not a zip', payload])
            manifest = study.fetch_plan(root, plan, fetch=fetch, sleeper=Mock())
            self.assertEqual(fetch.call_count, 3)
            self.assertEqual(manifest['errors'], [])
            self.assertEqual((root / item['path']).read_bytes(), payload)
            self.assertFalse(list(root.rglob('*.part')))


class SyntheticCommandTests(unittest.TestCase):
    def fixture(self, root):
        """Small archives, real plan structure, explicit 404s for every absent file."""
        daily = root / 'daily'
        daily.mkdir()
        data = root / 'data'
        export = root / 'trades.jsonl'
        trade = dict(symbol='TESTUSDT', side='SHORT', entry_time=B_BASE + DAY + 2 * HOUR,
                     r={'forbidden': 'outcome'}, price='forbidden', pnl='forbidden')
        export.write_text(json.dumps(trade) + '\n')
        pools = {'2023-01': ['TESTUSDT', 'BTCUSDT'], '2024-09': ['TESTUSDT', 'BTCUSDT']}
        archives = {}
        for symbol in ('TESTUSDT', 'BTCUSDT'):
            daily_data = []
            for start in ('2023-01-01', '2024-09-01', '2026-09-22'):
                raw = bars(study.ms(start), hours=12 * 24)
                if symbol == 'TESTUSDT':
                    increments = np.where(np.arange(len(raw)) // 12 % 2, .0001, -.0001)
                    for days_after in (8, 9):
                        increments[days_after * 288 - 1] = -.08
                        increments[days_after * 288 + 1] = .01
                    close = 100 * np.exp(np.cumsum(increments))
                    opens = np.r_[100, close[:-1]]
                    raw = pd.DataFrame(dict(open=opens, close=close, high=np.maximum(opens, close),
                                            low=np.minimum(opens, close)), index=raw.index)
                month = start[:7]
                archives[f'raw/klines5m/{symbol}/{symbol}-5m-{month}.zip'] = bars_csv(raw, header=True, micros=True)
                for day, group in raw.groupby(raw.index // DAY):
                    day_text = pd.Timestamp(int(day) * DAY, unit='ms').strftime('%Y-%m-%d')
                    close = float(group['close'].iloc[-1])
                    daily_data.append([day_text, close, close, close, close, 1000, close])
                    drop_at = study.ms(start) + 8 * DAY if symbol == 'TESTUSDT' else None
                    archives[f'raw/metrics/{symbol}/{symbol}-metrics-{day_text}.zip'] = metrics_csv(day_text, symbol, drop_at=drop_at)
            study.write_json(daily / f'{symbol}.json', daily_data)
        hashes = {p.name: study.file_sha256(p) for p in daily.glob('*.json')}
        with patch.object(study, 'select_pools', return_value=(pools, hashes)):
            plan = study.make_plan(daily, [study.project_trade(trade)])
        study.write_json(data / 'plan.json', plan)
        manifest = dict(version=1, plan_sha256=study.file_sha256(data / 'plan.json'), files={}, missing=[], errors=[])
        for item in plan['files']:
            if item['path'] in archives:
                path = data / item['path']
                write_zip(path, archives[item['path']])
                manifest['files'][item['path']] = dict(item, sha256=study.file_sha256(path))
            else:
                manifest['missing'].append(dict(item, status=404))
        study.write_json(data / 'manifest.json', manifest)
        return data, daily, export

    def frozen_synthetic(self, export):
        stack = ExitStack()
        stack.enter_context(patch.object(study, 'TRADES_SHA256', study.file_sha256(export)))
        stack.enter_context(patch.object(study, 'N_TRADES', 1))
        stack.enter_context(patch.object(study, 'N_TRADE_SYMBOLS', 1))
        stack.enter_context(patch.object(study.urllib.request.OpenerDirector, 'open',
                                       side_effect=AssertionError('offline path attempted network')))
        return stack

    def test_offline_run_complete_reports_no_b_forward_returns_and_no_overwrite(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            # Patch the expected synthetic export hash before building its plan.
            with patch.object(study, 'TRADES_SHA256', 'placeholder'):
                data, daily, export = self.fixture(root)
            digest = study.file_sha256(export)
            plan = json.loads((data / 'plan.json').read_bytes())
            plan['trades_sha256'] = digest
            study.write_json(data / 'plan.json', plan)
            manifest = json.loads((data / 'manifest.json').read_bytes())
            manifest['plan_sha256'] = study.file_sha256(data / 'plan.json')
            study.write_json(data / 'manifest.json', manifest)
            args = ['run', '--data-dir', str(data), '--daily-dir', str(daily), '--trades', str(export)]
            original_forwards = study.forward_returns

            def only_a_forwards(candles, times, horizons=study.HORIZONS):
                self.assertTrue(all(t < study.ms(study.A_END) for t in times))
                return original_forwards(candles, times, horizons)

            with self.frozen_synthetic(export), patch.object(study, 'forward_returns', side_effect=only_a_forwards), patch('builtins.print'):
                self.assertEqual(study.main(args), 0)
            out = data / 's0_v1'
            result_text = (out / 'results.json').read_text()
            result = json.loads(result_text)
            self.assertNotIn('NaN', result_text)
            self.assertNotIn('Infinity', result_text)
            self.assertEqual(result['verdict'], 'insufficient')
            self.assertTrue(result['valid'])
            self.assertEqual(result['data_check']['mismatches'], 0)
            self.assertEqual(len(result['sensitivity_grid']), 9)
            self.assertEqual(set(result['halves']), {'H1', 'H2'})
            self.assertEqual(result['B']['by_side']['SHORT']['after_cascade'], 1)
            self.assertEqual(result['B']['short_fraction'], 1.)
            # Entire absent tail archives remain in exclusion denominators.
            self.assertEqual(result['exclusions']['TESTUSDT']['pool_hours'], (31 + 30) * 24)
            self.assertGreater(result['exclusions']['TESTUSDT']['unconditional_forward_missing'], 0)
            self.assertFalse(result['s1_eligible'])
            events = [json.loads(line) for line in (out / 'events.jsonl').read_text().splitlines()]
            self.assertTrue(any(row['segment'] == 'A' for row in events))
            self.assertTrue(any(row['segment'] == 'B' for row in events))
            for row in events:
                if row['segment'] == 'B':
                    self.assertFalse(any(key.startswith('R_') for key in row))
            report = (out / 'report.md').read_text()
            for heading in ('四条判定规则', '敏感性网格', '其他 H 与扣 BTC', '全部急跌', '无条件基准',
                            '0.16%', 'B 级联后入场', '每币 metrics', '下载404', '实现口径'):
                self.assertIn(heading, report)
            self.assertEqual(result['prereg_sha256'], study.PREREG_SHA256)
            self.assertEqual(len(result['git_head']), 40)
            self.assertFalse(list(out.glob('*.tmp')))
            with self.frozen_synthetic(export), redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
                study.main(args)
            self.assertEqual(caught.exception.code, 1)
            self.assertEqual((out / 'results.json').read_text(), result_text)
            # Daily mismatch stops before any returns; debug computes but stays invalid.
            bad_daily = daily / 'TESTUSDT.json'
            rows = json.loads(bad_daily.read_bytes())
            for row in rows:
                row[4] *= 1.1
            study.write_json(bad_daily, rows)
            plan['daily_sha256'][bad_daily.name] = study.file_sha256(bad_daily)
            study.write_json(data / 'plan.json', plan)
            manifest['plan_sha256'] = study.file_sha256(data / 'plan.json')
            study.write_json(data / 'manifest.json', manifest)
            failed_out = root / 'failed'
            with self.frozen_synthetic(export), patch.object(study, 'analyze', side_effect=AssertionError('must preflight first')), patch('builtins.print'):
                self.assertEqual(study.main(args + ['--out', str(failed_out)]), 1)
            failed = json.loads((failed_out / 'results.json').read_bytes())
            self.assertEqual(failed['verdict'], 'invalid')
            self.assertNotIn('main', failed)
            self.assertGreaterEqual(failed['data_check']['mismatch_fraction'], .01)
            debug_out = root / 'debug'
            with self.frozen_synthetic(export), patch('builtins.print'):
                self.assertEqual(study.main(args + ['--out', str(debug_out), '--allow-data-check-fail']), 1)
            debug = json.loads((debug_out / 'results.json').read_bytes())
            self.assertEqual(debug['verdict'], 'invalid')
            self.assertIn('diagnostic_verdict', debug)
            self.assertFalse(debug['mechanism_holds'])
            self.assertFalse(debug['s1_eligible'])

    def test_run_rejects_prereg_sha_before_reading_inputs_or_creating_output(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            prereg = root / 'modified.md'
            prereg.write_text('not the frozen document')
            with patch.object(study, 'PREREG', prereg), patch.object(study, 'load_trades') as load, \
                    redirect_stderr(io.StringIO()) as errors, self.assertRaises(SystemExit) as caught:
                study.main(['run', '--data-dir', str(root), '--out', str(root / 'out')])
            self.assertEqual(caught.exception.code, 1)
            self.assertIn('prereg sha256 mismatch', errors.getvalue())
            load.assert_not_called()
            self.assertFalse((root / 'out').exists())
            options = Namespace(data_dir=root, out=root / 'out')
            with patch.object(study, 'PREREG', prereg), self.assertRaisesRegex(ValueError, 'prereg sha256'):
                study.run_study(options, [])


if __name__ == '__main__':
    unittest.main()
