"""Frozen wide-pool methodology on synthetic panels and temporary ledgers only."""
import hashlib
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest import evolution_ledger
from scripts.factor_mining import ensemble, evaluate, panel, wide_pool_study as s
from scripts.factor_mining.tests.test_ensemble_forward_study import synthetic


def factor(id_, icir=1., ic=.05, direction=1, expr='$close'):
    return dict(id=id_, expr=expr, horizon=3, direction=direction, family='other',
                status='candidate', train=dict(ic=ic, icir=icir))


def statistics(z=2.1):
    return dict(delta=.001, se=.001/z, z=z, n_days=140, n_weeks=20,
                dev_delta=.001, holdout_delta=.001)


class SelectionTests(unittest.TestCase):
    def test_greedy_absolute_threshold_cap_order_and_determinism(self):
        panels, _ = synthetic(n=100, names=60)
        rng = np.random.default_rng(11)
        dates, _ = s.sample_train_dates(panels.mask.index[:80])
        factors = [factor(f'f{i:03d}') for i in range(44)]
        values = {f['id']: pd.DataFrame(rng.normal(size=panels.mask.shape),
                                       index=panels.mask.index, columns=panels.mask.columns) for f in factors}
        # The first duplicate is negatively correlated; absolute correlation must reject it.
        values['f001'] = -values['f000']
        first, audit = s.greedy_select(list(reversed(factors)), panels, dates, values)
        second, audit2 = s.greedy_select(factors, panels, dates, values)
        self.assertEqual([f['id'] for f in first], [f['id'] for f in second])
        self.assertEqual(audit, audit2)
        self.assertEqual(len(first), 40)
        self.assertEqual(first[0]['id'], 'f000')
        self.assertNotIn('f001', [f['id'] for f in first])
        ranks = [(values[f['id']].loc[dates] * f['direction']).where(panels.mask.loc[dates])
                 .rank(axis=1, method='average') for f in first]
        for i, a in enumerate(ranks):
            for b in ranks[:i]:
                self.assertLess(abs(evaluate.cross_corr(a, b, dates)), .5)

    def test_exact_threshold_is_rejected_and_train_only(self):
        panels, score = synthetic(n=60, names=20)
        factors = [factor('a'), factor('b')]
        dates, _ = s.sample_train_dates(panels.mask.index[:30])
        def correlation(a, b, sample):
            self.assertTrue(a.index.equals(dates))
            self.assertTrue(b.index.equals(dates))
            self.assertTrue(sample.equals(dates))
            return -.5
        with patch.object(s.evaluate, 'cross_corr', side_effect=correlation):
            kept, audit = s.greedy_select(factors, panels, dates, dict(a=score, b=-score))
        self.assertEqual([f['id'] for f in kept], ['a'])
        self.assertFalse(audit[-1]['selected'])

    def test_train_metadata_matches_shortest_horizon_and_h3_purge(self):
        panels, _ = synthetic(n=1800, names=5)
        panels.windows = dict(train=1095, val=365, holdout=180)
        shortest = panel.make_splits(panels.mask.index, 1, panels.windows)
        zoo = dict(horizons={'1': {}, '3': {}, '7': {}}, split=dict(
            windows=panels.windows, train=list(shortest['ranges']['train']), val=list(shortest['ranges']['val'])))
        dates, audit = s.train_dates(panels, zoo)
        expected = panel.make_splits(panels.mask.index, 3, panels.windows)
        pd.testing.assert_index_equal(dates, panels.mask.index[expected['train']])
        self.assertTrue(audit['purge_adjusted_agrees'])
        zoo['split']['train'][0] = '1900-01-01'
        with self.assertRaises(AssertionError):
            s.train_dates(panels, zoo)

    def test_weights_sum_to_one_and_halve_departure_from_equal(self):
        factors = [factor(str(i), ic=ic) for i, ic in enumerate([.01, .02, .04, .13])]
        weights = s.shrunk_weights(factors)
        raw = np.array([.01, .02, .04, .13]) / .2
        self.assertAlmostEqual(weights.sum(), 1., places=15)
        np.testing.assert_allclose(weights - .25, .5 * (raw - .25), atol=1e-15)
        self.assertGreater(weights.min(), raw.min())
        self.assertLess(weights.max(), raw.max())

    def test_weighted_z_equals_existing_ensemble_with_missing_and_constant_rows(self):
        panels, score = synthetic(n=70, names=20)
        factors = [factor('a'), factor('b', direction=-1), factor('c')]
        values = dict(a=score.copy(), b=score.pow(2), c=score.shift(1))
        values['a'].iloc[5:10, :3] = np.nan
        values['b'].iloc[15] = 4.
        for frame in values.values():
            frame.iloc[20] = np.nan
        panels.mask.iloc[25:30, :4] = False
        actual = s.weighted_scores(factors, panels, [1/3]*3, values)
        expected = ensemble.ensemble_scores(factors, panels, values_cache=values)
        pd.testing.assert_frame_equal(actual, expected, atol=1e-14, rtol=1e-14)
        self.assertTrue(actual.iloc[20].isna().all())
        # Missing values are removed from the weight denominator for each name.
        weighted = s.weighted_scores(factors, panels, [.1, .2, .7], values)
        z = [s.factor_z(f, panels, values) for f in factors]
        self.assertAlmostEqual(weighted.iloc[6, 0], (.2*z[1].iloc[6, 0] + .7*z[2].iloc[6, 0])/.9)

    def test_disk_cache_evaluates_an_expression_only_once(self):
        panels, _ = synthetic(n=40, names=6)
        panels.fields['vwap'] = panels.fields['close'].copy()
        factors = [factor('a'), factor('b')]
        with tempfile.TemporaryDirectory(dir=s.REPO) as tmp:
            cache = s.ValuesCache(panels, factors, tmp)
            with patch.object(s, 'evaluate_expr', wraps=s.evaluate_expr) as evaluate_expr:
                first = cache['a']
                second = cache['b']
                third = cache['a']
                self.assertEqual(evaluate_expr.call_count, 1)
            pd.testing.assert_frame_equal(first, second)
            pd.testing.assert_frame_equal(first, third)


class PairedTests(unittest.TestCase):
    def test_weekly_influences_monday_utc_and_halves(self):
        dates = pd.date_range('2026-05-04', periods=140, tz='UTC')
        diff = np.repeat(np.arange(20) * .0001, 7) + .0002
        result = s.paired_diff(pd.Series(diff + .001, index=dates), pd.Series(.001, index=dates))
        expected = s.cluster_se([(x - diff.mean()) / 20 for x in diff[::7]])
        self.assertAlmostEqual(result['se'], expected, places=15)
        self.assertAlmostEqual(result['delta'], diff.mean(), places=15)
        self.assertAlmostEqual(result['dev_delta'], diff[:70].mean(), places=15)
        self.assertAlmostEqual(result['holdout_delta'], diff[70:].mean(), places=15)
        self.assertEqual(result['n_weeks'], 20)
        # These timestamps are Monday locally but Sunday in UTC: one cluster.
        a = pd.Series([.01, .02], index=pd.to_datetime(['2026-05-10T23:00Z', '2026-05-11T01:00Z']))
        a.index = a.index.tz_convert('Asia/Shanghai')
        self.assertEqual(s.paired_diff(a, a*0)['n_weeks'], 2)

    def test_fewer_than_three_weeks_has_no_se(self):
        dates = pd.date_range('2026-05-04', periods=14)
        result = s.paired_diff(pd.Series(.01, index=dates), pd.Series(0., index=dates))
        self.assertIsNone(result['se'])
        self.assertIsNone(result['z'])

    def test_tiers_boundaries_and_positive_halves(self):
        self.assertEqual(s.verdict(statistics(), 1, 40)['verdict'], 'strong')
        self.assertEqual(s.verdict(statistics(), 2, 40)['verdict'], 'screen_pass')
        self.assertEqual(s.verdict(statistics(1.), 1, 40)['verdict'], 'screen_pass')
        self.assertEqual(s.verdict(statistics(.999), 1, 40)['verdict'], 'fail')
        for change in (dict(n_days=99),):
            self.assertEqual(s.verdict(dict(statistics(), **change), 1, 40)['verdict'], 'invalid')
        self.assertEqual(s.verdict(statistics(), 1, 19)['verdict'], 'invalid')
        self.assertEqual(s.verdict(statistics(), 1, 40, False)['verdict'], 'invalid')
        self.assertEqual(s.verdict(statistics(), 1, 20)['verdict'], 'strong')
        for change in (dict(dev_delta=0.), dict(holdout_delta=-.001)):
            self.assertEqual(s.verdict(dict(statistics(9.), **change), 1, 40)['verdict'], 'fail')
        boundary = s.verdict(statistics(), 1, 40)['z_K']
        self.assertEqual(s.verdict(statistics(boundary), 1, 40)['verdict'], 'strong')

    def test_k_growth_inside_shared_ledger_invalid_does_not_count(self):
        with tempfile.TemporaryDirectory(dir=s.REPO) as tmp:
            path = Path(tmp) / 'ledger.jsonl'
            def append(stats):
                return s.ledger_result(stats, 40, True, s.EXPECTED_HASHES, 1.1, 55., path, {}, False)
            first = append(statistics())
            invalid = append(dict(statistics(), n_days=99))
            second = append(statistics())
            self.assertEqual((first['K'], invalid['K'], second['K']), (1, None, 2))
            self.assertEqual(first['verdict'], 'strong')
            self.assertEqual(second['verdict'], 'screen_pass')
            self.assertGreater(second['z_K'], first['z_K'])
            self.assertEqual(first['baseline_version'], s.EXPECTED_HASHES['ensemble.json'][:16] + ':ls_invvol_protected_v1')
            self.assertEqual(first['prereg_path'], s.PREREG)
            self.assertEqual(len(evolution_ledger.read_rows(path)), 3)


class ExecutionTests(unittest.TestCase):
    def test_synthetic_cli_writes_both_reports_one_ledger_row_and_keeps_inputs(self):
        panels, _ = synthetic(n=1800, names=20)
        dates = pd.date_range('2022-01-01', periods=1800)
        for frame in list(panels.fields.values()) + [panels.mask]:
            frame.index = dates
        panels.fields['vwap'] = panels.fields['close'].copy()
        panels.windows = dict(train=1095, val=365, holdout=180)
        split = panel.make_splits(dates, 1, panels.windows)
        factors = [factor(f'f{i:03d}') for i in range(42)]
        zoo = dict(factors=factors, horizons={'1': {}, '3': {}}, split=dict(
            windows=panels.windows, train=list(split['ranges']['train']), val=list(split['ranges']['val'])))
        with tempfile.TemporaryDirectory(dir=s.REPO) as tmp:
            root, out = Path(tmp)/'data', Path(tmp)/'out'
            (root/'crypto').mkdir(parents=True)
            (root/'crypto/zoo.json').write_text(json.dumps(zoo))
            (root/'crypto/ensemble.json').write_text(json.dumps(dict(horizons={'3': dict(factor_ids=['f000'])})))
            before, _ = s.guard_inputs(root, True)
            ledger = out/'ledger.jsonl'
            with patch.object(s.panel, 'load_panels', return_value=panels), \
                 contextlib.redirect_stderr(io.StringIO()), contextlib.redirect_stdout(io.StringIO()):
                s.main(['--root', str(root), '--out', str(out), '--ledger', str(ledger), '--allow-input-drift'])
            result = json.loads((out/'report.json').read_text())
            self.assertEqual(result['primary']['verdict'], 'invalid')
            self.assertEqual(result['baseline_reproduction']['forward']['relative_error'], 0.)
            self.assertTrue(result['allow_input_drift'])
            self.assertEqual(len(evolution_ledger.read_rows(ledger)), 1)
            self.assertEqual(s.guard_inputs(root, True)[0], before)
            self.assertIn('deviations', (out/'report.md').read_text())
            self.assertEqual(result['cache']['evaluated'], 1)

    def test_baseline_matches_quant_targets_backtest_on_same_synthetic_panel(self):
        panels, _ = synthetic(n=550, names=20)
        dates = pd.date_range('2025-05-01', periods=550)
        for frame in list(panels.fields.values()) + [panels.mask]:
            frame.index = dates
        panels.fields['vwap'] = panels.fields['close'].copy()
        factors = [factor('a')]
        score = ensemble.ensemble_scores(factors, panels)
        forward_dates = dates[dates >= pd.Timestamp(s.FORWARD_START)]
        actual = s.run_arm(panels, score, factors, forward_dates)
        with patch.object(s.panel, 'load_panels', return_value=panels), \
             patch.object(s.quant_targets, '_load_frozen', return_value=(factors, {})):
            reference = s.quant_targets.backtest('synthetic-unused-root')
        expected = reference['intervals']['forward']['strategies']['ls_invvol_protected_v1']
        self.assertEqual(actual['daily_equity'], expected['daily_equity'])
        self.assertTrue(actual['ok'])
        json.dumps(s.json_safe(actual), allow_nan=False)
        self.assertEqual([signal['as_of'] for signal in actual['signals']],
                         [str(d.date()) for d in forward_dates[::3]])

    def test_actual_fill_leg_pnl_includes_flips_funding_and_costs(self):
        panels, _ = synthetic(n=30, names=4)
        dates = panels.mask.index
        targets = panels.mask.astype(float) * 0
        targets.iloc[:9, 0] = .3
        targets.iloc[:9, 1] = -.3
        targets.iloc[9:19, 0] = -.25
        targets.iloc[9:19, 1] = .25
        panels.fields['funding'].iloc[:] = .0001
        frames, rates, targets = s.quant_targets._frames_rates(panels, targets)
        run = s.q.run_weights(frames, targets, market='crypto', commission=s.COMMISSION,
                             slippage=s.SLIPPAGE, funding=rates, keep_fills=True,
                             policy=s.q.RebalancePolicy(frequency=None, rebalance_on_signal_change=True))
        legs = s.leg_contributions(run, frames, rates, dates)
        net = legs['long']['net_pnl'] + legs['short']['net_pnl']
        final = list(run['daily_equity'].values())[-1]
        self.assertAlmostEqual(net, final - s.q.CAPITAL, places=5)
        for key in ('commission_paid', 'funding_paid'):
            self.assertAlmostEqual(legs['long'][key] + legs['short'][key], run['metrics'][key], places=7)
        self.assertGreater(legs['long']['slippage_paid'], 0)
        self.assertGreater(legs['short']['slippage_paid'], 0)

    def test_placebo_is_equal_weight_rank_ic_only_and_deterministic(self):
        panels, score = synthetic(n=35, names=20)
        pool = [factor(f'f{i:03d}', expr='$close') for i in range(42)]
        rng = np.random.default_rng(61)
        values = {f['id']: score + rng.normal(size=score.shape) for f in pool}
        excess, _ = panel.forward_labels(panels, 3, '2026-05-04')
        dates = panels.mask.index[panels.mask.index >= pd.Timestamp('2026-05-04')]
        first = s.placebo_study(pool, panels, dates, excess, values, .1, lambda _: None)
        second = s.placebo_study(pool, panels, dates, excess, values, .1, lambda _: None)
        self.assertEqual(first, second)
        self.assertEqual(len(first['factor_ids']), 200)
        by_id = {f['id']: f for f in pool}
        members = [by_id[id_] for id_ in first['factor_ids'][0]]
        score = ensemble.ensemble_scores(members, panels, values_cache=values)
        expected = s.rank_ic(score.loc[dates], excess.loc[dates], panels.mask.loc[dates]).mean()
        self.assertAlmostEqual(first['rank_ic_means'][0], expected, places=14)


class InputTests(unittest.TestCase):
    def test_hash_guard_refuses_drift_and_records_explicit_override(self):
        with tempfile.TemporaryDirectory(dir=s.REPO) as tmp:
            root = Path(tmp)
            (root / 'crypto').mkdir()
            for name in s.EXPECTED_HASHES:
                (root / 'crypto' / name).write_text('{}')
            with self.assertRaisesRegex(ValueError, 'sha256 mismatch'):
                s.guard_inputs(root)
            actual, drift = s.guard_inputs(root, True)
            self.assertEqual(set(drift), set(s.EXPECTED_HASHES))
            self.assertEqual(actual['zoo.json'], hashlib.sha256(b'{}').hexdigest())
            with patch.object(s, 'EXPECTED_HASHES', actual):
                self.assertEqual(s.guard_inputs(root), (actual, {}))
            with self.assertRaises(SystemExit):
                s.main(['--root', str(root), '--out', str(root/'out'), '--no-ledger'])
            self.assertFalse((root / 'out').exists())

    def test_cannot_write_inputs_or_outside_worktree_or_collide_with_ledger(self):
        root = s.REPO / '.synthetic_input'
        with self.assertRaisesRegex(ValueError, 'read-only'):
            s.validate_outputs(root, root/'out', None)
        with self.assertRaisesRegex(ValueError, 'inside this worktree'):
            s.validate_outputs(root, Path('/tmp/wide_pool_out'), None)
        out = s.REPO / '.synthetic_out'
        with self.assertRaisesRegex(ValueError, 'must differ'):
            s.validate_outputs(root, out, out/'report.json')


if __name__ == '__main__':
    unittest.main()
