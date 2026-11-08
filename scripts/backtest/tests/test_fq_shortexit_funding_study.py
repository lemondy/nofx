"""Preregistered gates, funding targets, input guards. No real data reads."""
from contextlib import ExitStack, redirect_stderr
import datetime as dt
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest import fq_shortexit_funding_study as s
from scripts.factor_mining import quant_targets
from scripts.factor_mining.panel import Panels


def synthetic_panels():
    dates = pd.date_range('2024-01-01', periods=130)
    symbols = [f'S{i:02}' for i in range(20)]
    # Different phases avoid ties in the existing seven-day return exclusion.
    close = pd.DataFrame({symbol: 100 * np.cumprod(1 + .01 * np.sin(np.arange(len(dates)) + j))
                          for j, symbol in enumerate(symbols)}, index=dates)
    fields = dict(close=close, open=close.copy(), high=close * 1.01, low=close * .99,
                  volume=close * 0 + 1000., funding=close * 0)
    return Panels(fields, pd.DataFrame(True, index=dates, columns=symbols))


class FundingTargets(unittest.TestCase):
    def setUp(self):
        self.panels = synthetic_panels()
        self.date = self.panels.mask.index[110]
        self.shorts = list(self.panels.mask.columns[:6])
        self.weights = dict(L=.1, **{sym: -round(float(w), 6) for sym, w in
                           quant_targets._capped_invvol(pd.Series(np.arange(1., 7.), index=self.shorts), .5, .1).items()})
        self.vol = pd.Series(np.arange(1., 7.), index=self.shorts)

    def test_fewer_than_five_finite_values_not_excluded(self):
        dates = pd.date_range(self.date - dt.timedelta(days=6), self.date)
        funding = self.panels.fields['funding']
        funding.loc[dates, self.shorts[0]] = [np.nan, np.inf, -np.inf, -.001, -.001, -.001, -.001]
        excluded, _ = s.funding_exclusions(self.panels, self.date, self.shorts)
        self.assertNotIn(self.shorts[0], excluded)
        funding.loc[dates[0], self.shorts[0]] = -.001
        self.assertIn(self.shorts[0], s.funding_exclusions(self.panels, self.date, self.shorts)[0])

    def test_strict_funding_boundary_and_causal_window(self):
        dates = pd.date_range(self.date - dt.timedelta(days=6), self.date)
        funding = self.panels.fields['funding']
        funding.loc[dates, self.shorts[0]] = -.0001
        funding.loc[dates, self.shorts[1]] = -.0001000001
        funding.loc[self.date + dt.timedelta(days=1):, self.shorts[0]] = -10.
        excluded, means = s.funding_exclusions(self.panels, self.date, self.shorts)
        self.assertNotIn(self.shorts[0], excluded)
        self.assertIn(self.shorts[1], excluded)
        self.assertAlmostEqual(means[self.shorts[0]], -.0001)

    def test_reweight_and_all_short_excluded(self):
        excluded = self.shorts[:2]
        rebuilt = s.reweight_shorts(self.weights, self.vol, excluded)
        expected = -quant_targets._capped_invvol(self.vol.loc[self.shorts[2:]], .5, .1)
        self.assertEqual(rebuilt['L'], .1)
        self.assertEqual({k: v for k, v in rebuilt.items() if v < 0},
                         {k: round(float(v), 6) for k, v in expected.items()})
        self.assertEqual(s.reweight_shorts(self.weights, self.vol, self.shorts), {'L': .1})

    def test_full_historical_loop_matches_baseline_when_no_exclusions(self):
        scores = self.panels.fields['close'].copy()
        scores[:] = np.arange(len(scores.columns))
        dates = self.panels.mask.index[100:]
        factors = [dict(id='test')]
        expected, _, _ = quant_targets._historical_targets(self.panels, scores, factors, dates)
        actual, records = s.funding_targets(self.panels, scores, factors, dates)
        pd.testing.assert_frame_equal(actual, expected)
        self.assertTrue(any(r['status'] == 'ok' for r in records))
        self.assertEqual(sum(len(r['excluded']) for r in records), 0)
        expected_vol = self.panels.fields['close'].loc[:dates[0]].tail(31).pct_change(fill_method=None).std(ddof=1)
        pd.testing.assert_series_equal(s.short_volatility(self.panels, dates[0]), expected_vol, check_names=False)

    def test_non_ok_carries_previous_and_all_excluded_clears_short(self):
        dates = self.panels.mask.index[100:107]
        scores = self.panels.fields['close'].copy()
        score_weights = dict(S19=.1, S00=-.1)
        snapshots = [dict(status='ok', weights=score_weights, excluded={}),
                     dict(status='insufficient_universe', weights={}, excluded={}),
                     dict(status='ok', weights=score_weights, excluded={})]
        self.panels.fields['funding'].loc[:dates[0], 'S00'] = -.001
        # The first valid period excludes every short. A non-ok period carries it.
        with patch.object(quant_targets, 'build_targets', side_effect=snapshots):
            targets, records = s.funding_targets(self.panels, scores, [dict(id='test')], dates)
        self.assertTrue(records[0]['all_short_excluded'])
        self.assertEqual(targets.loc[dates[0], 'S00'], 0.)
        self.assertEqual(targets.loc[dates[0], 'S19'], .1)
        pd.testing.assert_series_equal(targets.loc[dates[0]], targets.loc[dates[3]], check_names=False)

    def test_placebo_count_seed_and_segment_rng_order(self):
        dates = pd.date_range(self.date, periods=10)
        template = pd.DataFrame(0., index=dates, columns=['L'] + self.shorts)
        records = [dict(date=str(date.date()), status='ok', weights=self.weights, shorts=self.shorts,
                        excluded=self.shorts[:count], vol=self.vol.to_dict())
                   for date, count in zip(dates[::3], [0, 2, 6, 1])]
        a, exclusions_a = s.placebo_targets(template, records, np.random.default_rng(s.SEED + 2))
        b, exclusions_b = s.placebo_targets(template, records, np.random.default_rng(s.SEED + 2))
        pd.testing.assert_frame_equal(a, b)
        self.assertEqual(exclusions_a, exclusions_b)
        self.assertEqual([len(x) for x in exclusions_a], [0, 2, 6, 1])
        self.assertTrue((a.loc[dates[6], self.shorts] == 0).all())
        rng = np.random.default_rng(s.SEED + 2)
        _, first = s.placebo_targets(template, records, rng)
        _, second = s.placebo_targets(template, records, rng)
        independent = np.random.default_rng(s.SEED + 2)
        expected = [list(independent.choice(self.shorts, size=n, replace=False))
                    for n in [0, 2, 6, 1, 0, 2, 6, 1]]
        self.assertEqual(first + second, expected)


class Verdicts(unittest.TestCase):
    def test_g_all_branches_and_boundaries(self):
        stats = dict(hourly_valid=True, val_delta=.001, forward_delta=.002, z=s.z_threshold(8), bx_delta=.0001)
        cases = [({}, 'strong'), ({'z': 1.}, 'screen_pass'), ({'bx_delta': 0.}, 'exposure_only'),
                 ({'hourly_valid': False}, 'invalid'), ({'z': .999}, 'fail'),
                 ({'val_delta': 0.}, 'fail'), ({'forward_delta': -1.}, 'fail'), ({'z': None}, 'fail')]
        for changes, verdict in cases:
            with self.subTest(changes=changes):
                self.assertEqual(s.verdict_g(dict(stats, **changes)), verdict)

    def test_f_all_branches_and_boundaries(self):
        stats = dict(hourly_valid=True, val_delta=.001, forward_delta=.002, z=s.z_threshold(8),
                     excluded_count=1, placebo_pct=.90)
        cases = [({}, 'strong'), ({'z': 1.}, 'screen_pass'), ({'placebo_pct': .899}, 'fewer_trades'),
                 ({'hourly_valid': False}, 'invalid'), ({'excluded_count': 0}, 'invalid'),
                 ({'z': .999}, 'fail'), ({'forward_delta': 0.}, 'fail'),
                 ({'z': None}, 'fail'), ({'placebo_pct': None}, 'fail')]
        for changes, verdict in cases:
            with self.subTest(changes=changes):
                self.assertEqual(s.verdict_f(dict(stats, **changes)), verdict)

    def test_alignment_and_week_cluster_statistics(self):
        dates = pd.date_range('2025-01-01', periods=40).strftime('%Y-%m-%d')
        base = dict(daily_equity=dict.fromkeys(dates, 1000.))
        candidate = dict(daily_equity=dict(zip(dates, 1000 * np.cumprod(np.tile([1.001, .9995], 20)))))
        intervals = {name: dict(variants=dict(B=base, G=candidate)) for name in ('val', 'forward')}
        stats = s.comparison(intervals, 'G')
        self.assertAlmostEqual(stats['mean_delta'], .00025)
        self.assertGreater(stats['z'], 1.)
        with self.assertRaisesRegex(ValueError, 'dates differ'):
            s.aligned_delta(dict(daily_equity={dates[0]: 1000.}), base)


class FrozenInputs(unittest.TestCase):
    def test_hourly_window_canonical_hash_and_missing_file(self):
        start = int(pd.Timestamp('2025-05-01').timestamp() * 1000)
        end = int(pd.Timestamp('2026-11-07').timestamp() * 1000)
        rows = [[t, 100, 101, 99, 100, 1, 100] for t in (start - 3600000, start, start + 3600000, end)]
        with tempfile.TemporaryDirectory() as tmp:
            for symbol in ('B', 'A'):
                (Path(tmp) / f'{symbol}_1h.json').write_text(json.dumps(rows))
            hourly, digest = s.load_hourly(['B', 'A'], tmp)
            hourly2, digest2 = s.load_hourly(['A', 'B'], tmp)
            expected = hashlib.sha256(json.dumps(dict(A=rows[1:3], B=rows[1:3]),
                                                sort_keys=True, separators=(',', ':')).encode()).hexdigest()
            self.assertEqual(digest, expected)
            self.assertEqual(digest, digest2)
            pd.testing.assert_frame_equal(hourly['A'], hourly2['A'])
            self.assertEqual(list(hourly['A'].index), [pd.Timestamp('2025-05-01'), pd.Timestamp('2025-05-01T01:00')])
            with self.assertRaisesRegex(FileNotFoundError, 'C_1h.json'):
                s.load_hourly(['C'], tmp)

    def test_hourly_coverage_and_close_checks(self):
        date = pd.Timestamp('2025-05-04')
        panels = Panels({'close': pd.DataFrame({'A': [100.]}, index=[date])},
                        pd.DataFrame({'A': [True]}, index=[date]))
        targets = {'val': pd.DataFrame({'A': [.1]}, index=[date])}
        hourly = pd.DataFrame(100., index=pd.date_range(date, periods=24, freq='h'),
                              columns=['open', 'high', 'low', 'close'])
        self.assertTrue(s.validate_hourly(panels, targets, {'A': hourly})['valid'])
        hourly.loc[hourly.index[-1], 'close'] = 101.
        invalid = s.validate_hourly(panels, targets, {'A': hourly})
        self.assertFalse(invalid['valid'])
        self.assertEqual(s.verdict_g({'hourly_valid': invalid['valid']}), 'invalid')
        self.assertEqual(s.verdict_f({'hourly_valid': invalid['valid']}), 'invalid')
        missing = s.validate_hourly(panels, targets, {'A': hourly.iloc[:-1]})
        self.assertFalse(missing['valid'])
        self.assertEqual(missing['missing_fraction'], 1.)

    def test_freeze_checks_fail_closed_and_snapshot_byte_order(self):
        with tempfile.TemporaryDirectory() as tmp:
            prereg = Path(tmp) / 'prereg.md'
            prereg.write_text('changed')
            with patch.object(s, 'PREREG', prereg), self.assertRaisesRegex(ValueError, 'sha256 mismatch'):
                s.verify_prereg()
            base = Path(tmp) / 'crypto'
            base.mkdir()
            for name in ('zoo.json', 'ensemble.json'):
                (base / name).write_bytes(name.encode())
            with self.assertRaisesRegex(ValueError, 'snapshot mismatch'):
                s.verify_snapshot(tmp)
            for i in range(301):
                (base / f'x{i:03}').write_bytes(str(i).encode())
            expected = hashlib.sha256(b''.join(p.read_bytes() for p in sorted(base.iterdir()))).hexdigest()
            hashes = {name: hashlib.sha256(name.encode()).hexdigest() for name in ('zoo.json', 'ensemble.json')}
            with patch.object(s, 'SNAPSHOT_SHA256', expected), patch.object(s, 'FILE_HASHES', hashes):
                self.assertEqual(s.verify_snapshot(tmp)['snapshot_files'], 303)

    def test_output_paths_rejected_in_inputs_including_symlinks(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            snapshot, klines = root / 'snapshot', root / 'klines'
            snapshot.mkdir()
            klines.mkdir()
            (root / 'link').symlink_to(snapshot, target_is_directory=True)
            for out in (snapshot, snapshot / 'results', klines / 'results', root / 'link/results'):
                with self.subTest(out=out), self.assertRaisesRegex(ValueError, '--out'):
                    s.validate_output(out, snapshot, klines)
                with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as exc:
                    s.main(['--out', str(out), '--snapshot', str(snapshot), '--klines-dir', str(klines)])
                self.assertEqual(exc.exception.code, 2)
            self.assertEqual(s.validate_output(root / 'out', snapshot, klines), (root / 'out').resolve())

    def test_smoke_forbids_ledger_and_invalid_never_appends(self):
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            s.main(['--out', '/tmp/fqsef_test', '--placebo', '2', '--write-ledger'])
        for result in (dict(smoke=True), dict(smoke=False, placebo={'n': 2}),
                       dict(smoke=False, placebo={'n': 200}, verdicts=dict(G='invalid', F='fail'))):
            with patch.object(s.evolution_ledger, 'append_event') as append:
                self.assertEqual(s.write_ledger(result, '/unused'), [])
                append.assert_not_called()

    def test_synthetic_study_with_process_pool_and_report(self):
        dates = pd.date_range('2025-05-04', periods=9).union(pd.date_range('2026-10-29', periods=9))
        symbols = ['L'] + [f'S{i}' for i in range(6)]
        close = pd.DataFrame(100., index=dates, columns=symbols)
        fields = {name: close.copy() for name in ('open', 'high', 'low', 'close', 'volume')}
        fields['funding'] = close * 0
        fields['funding']['S0'] = -.001
        panels = Panels(fields, close.gt(0))
        hourly_index = pd.DatetimeIndex([date + dt.timedelta(hours=h) for date in dates for h in range(24)])
        hourly = {sym: pd.DataFrame(100., index=hourly_index, columns=['open', 'high', 'low', 'close']) for sym in symbols}
        weights = dict(L=.1, **{sym: -.083333 for sym in symbols[1:]})
        frozen_factors = [dict(id=i, horizon=3, expr='$close', direction=1) for i in s.SECONDARY]
        with tempfile.TemporaryDirectory() as tmp, ExitStack() as stack:
            base = Path(tmp) / 'crypto'
            base.mkdir()
            (base / 'zoo.json').write_text(json.dumps(dict(factors=frozen_factors)))
            stack.enter_context(patch.object(s, 'verify_prereg', return_value=b'frozen'))
            stack.enter_context(patch.object(s, 'verify_snapshot', return_value={}))
            stack.enter_context(patch.object(s.panel, 'load_panels', return_value=panels))
            stack.enter_context(patch.object(s, 'load_hourly', return_value=(hourly, 'hourly-digest')))
            stack.enter_context(patch.object(s, 'ensemble_scores', return_value=close))
            stack.enter_context(patch.object(s.quant_targets, 'build_targets', return_value=dict(status='ok', weights=weights,
                                                                                               excluded={}, as_of='synthetic', universe_size=7)))
            stack.enter_context(patch.object(s, 'short_volatility', return_value=pd.Series(.1, index=symbols)))
            result = s.study(tmp, workers=2, placebo=2, progress=lambda *args, **kwargs: None)
            self.assertTrue(result['smoke'])
            self.assertEqual(len(result['placebo']['means']), 2)
            self.assertEqual(result['validation']['close_match_fraction'], 1.)
            self.assertEqual(set(result['secondary']), {'val', 'forward'})
            self.assertGreater(result['statistics']['F']['excluded_count'], 0)
            self.assertIn('周聚类', s.report(result))
            self.assertEqual(set(result['last_week_return_pct']), {'B', 'G', 'F'})
            store_path = Path(tmp) / 'result.json'
            s.store.write_json(store_path, result)
            self.assertEqual(s.store.read_json(store_path)['inputs']['hourly_sha256'], 'hourly-digest')

    def test_ledger_rows_have_preregistered_fields(self):
        stats = dict(mean_delta=.001, se=.0002, z=5., val_delta=.002, forward_delta=.001)
        result = dict(smoke=False, placebo=dict(n=200), verdicts=dict(G='strong', F='screen_pass'),
                      statistics=dict(G=dict(stats, bx_delta=.0001), F=dict(stats, placebo_pct=.91)), inputs={'hourly_sha256': 'abc'})
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'ledger.jsonl'
            rows = s.write_ledger(result, path)
            self.assertEqual([r['K'] for r in rows], [1, 2])
            self.assertEqual([r['preregistered_K'] for r in rows], [7, 8])
            self.assertEqual([r['change_type'] for r in rows], ['C', 'B'])
            self.assertEqual(rows[0]['baseline_version'], 'f_0ce8b2ee@zoo1e3c46c5+S20h')
            self.assertEqual(rows[1]['placebo_pct'], .91)
            self.assertEqual(len(path.read_text().splitlines()), 2)


if __name__ == '__main__':
    unittest.main()
