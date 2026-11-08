import math
from statistics import NormalDist
import time
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import evaluate, greedy_ensemble
from scripts.factor_mining.config import MARKETS
from scripts.factor_mining.evaluate import (classify, cross_corr, cs_rank_ic,
    evaluate_factor, long_short, nw_t, overlap_t, segment_metrics, student_t_sf,
    t_required)
from scripts.factor_mining.expr import evaluate_expr
from scripts.factor_mining.panel import forward_target, make_splits
from scripts.factor_mining.tests.synth import SIGNAL_EXPR, planted


class EvaluateTests(unittest.TestCase):
    def test_student_t_sf_known_values_and_symmetry(self):
        for t, df, expected, tolerance in ((2., 10, .0366940173853702, 1e-13),
                                           # Finite df adds about 1.4e-7 to the normal limit.
                                           (1.96, 1e6, .0249979, 2e-7),
                                           (0., 36, .5, 1e-15), (1., 1, .25, 1e-14)):
            with self.subTest(t=t, df=df):
                self.assertAlmostEqual(student_t_sf(t, df), expected, delta=tolerance)
                self.assertAlmostEqual(student_t_sf(-t, df), 1 - student_t_sf(t, df), delta=1e-14)

    def test_student_t_sf_tail_matches_independent_simpson_integration(self):
        # Substitute t=sqrt(df)*tan(theta) in the normalized Student density.
        # Composite Simpson integration supplies an independent tail reference.
        t, df, steps = 4.65, 36, 10000
        theta = np.linspace(math.atan(t / math.sqrt(df)), math.pi / 2, steps + 1)
        density = np.cos(theta) ** (df - 1)
        coefficient = math.exp(math.lgamma((df + 1) / 2) - math.lgamma(df / 2)) / math.sqrt(math.pi)
        integral = (theta[-1] - theta[0]) / (3 * steps) * (
            density[0] + density[-1] + 4 * density[1:-1:2].sum() + 2 * density[2:-1:2].sum())
        expected = coefficient * integral
        self.assertGreater(expected, 2e-5)
        self.assertLess(expected, 3e-5)
        self.assertLess(abs(student_t_sf(t, df) / expected - 1), 1e-4)

    def test_overlap_matches_nonoverlapping_student_z_formula(self):
        x = np.linspace(-.5, .8, 100)
        h = 7
        sample = x[::h]
        raw = sample.mean() / (sample.std(ddof=1) / math.sqrt(len(sample)))
        expected = -NormalDist().inv_cdf(student_t_sf(raw, len(sample) - 1))
        self.assertAlmostEqual(overlap_t(x, h), expected, places=14)
        self.assertAlmostEqual(overlap_t(-x, h), -expected, places=14)
        self.assertEqual(overlap_t(x, h), overlap_t(pd.Series(x), h))

    def test_overlap_h1_equals_full_sample_student_z(self):
        x = np.linspace(-.5, .8, 80)
        x[[0, 8, 11]] = np.nan
        sample = x[np.isfinite(x)]
        raw = sample.mean() / (sample.std(ddof=1) / math.sqrt(len(sample)))
        expected = -NormalDist().inv_cdf(student_t_sf(raw, len(sample) - 1))
        self.assertAlmostEqual(overlap_t(x, 1), expected, places=14)

    def test_overlap_triangular_autocorrelation_null_distribution(self):
        rng = np.random.default_rng(20261108)
        h, n = 20, 3000
        overlap, bartlett = [], []
        for _ in range(300):
            x = np.convolve(rng.normal(size=n + h - 1), np.ones(h), mode='valid')
            overlap.append(overlap_t(x, h))
            bartlett.append(nw_t(x, h))
        self.assertGreater(np.std(overlap), .85)
        self.assertLess(np.std(overlap), 1.15)
        self.assertGreater(np.std(bartlett), 1.15)
        self.assertGreater(np.std(bartlett), 1.1 * np.std(overlap))

    def test_overlap_short_samples_and_saturation(self):
        for n in (0, 1, 19):
            with self.subTest(n=n):
                self.assertEqual(overlap_t(np.r_[np.ones(n), [np.nan] * 20], 1), 0.)
        # Enough valid calendar days, but only four sampled observations.
        self.assertEqual(overlap_t(np.arange(20.), 5), 0.)
        x = np.arange(30.)
        x[[0, 5]] = np.nan
        self.assertEqual(overlap_t(x, 5), 0.)
        for mean, expected in ((1., 1e12), (-1., -1e12), (0., 0.)):
            with self.subTest(mean=mean):
                self.assertEqual(overlap_t(np.full(30, mean), 3), expected)
                near_constant = np.full(30, mean) + np.tile([-1e-16, 1e-16], 15)
                self.assertEqual(overlap_t(near_constant, 3), expected)

    def test_overlap_samples_calendar_before_removing_nan(self):
        x = np.linspace(-.5, .8, 100)
        h = 3
        # Missing sampled days are dropped. Day 1 never replaces missing day 0.
        x[[0, 6, 30]] = np.nan
        sample = x[::h]
        sample = sample[np.isfinite(sample)]
        raw = sample.mean() / (sample.std(ddof=1) / math.sqrt(len(sample)))
        expected = -NormalDist().inv_cdf(student_t_sf(raw, len(sample) - 1))
        self.assertAlmostEqual(overlap_t(x, h), expected, places=14)
        self.assertNotAlmostEqual(overlap_t(x, h), overlap_t(x[np.isfinite(x)], h))
        # Neither missing nor changed unsampled days can shift the anchor.
        x[[1, 2, 7, 8]] = np.nan
        x[4] = 1000.
        self.assertAlmostEqual(overlap_t(x, h), expected, places=14)

    def test_overlap_uses_sample_df_and_stable_tail_conversion(self):
        x = np.arange(24, dtype=float) - 9
        h = 4
        x[0] = np.nan  # Exactly five sampled observations remain.
        sample = x[::h][1:]
        raw = float(sample.mean() / (sample.std(ddof=1) / math.sqrt(len(sample))))
        with patch.object(evaluate, 'student_t_sf', return_value=1e-50) as sf:
            self.assertAlmostEqual(overlap_t(x, h), -NormalDist().inv_cdf(1e-50))
            sf.assert_called_once_with(raw, 4)
        with patch.object(evaluate, 'student_t_sf', return_value=0.):
            self.assertEqual(overlap_t(x, h), raw)
            self.assertEqual(overlap_t(-x, h), -raw)
        with patch.object(evaluate, 'student_t_sf', return_value=.5) as sf:
            self.assertEqual(overlap_t(x, h), 0.)
            sf.assert_called_once_with(raw, 4)
        # At exactly 20 valid days the statistic is evaluated.
        with patch.object(evaluate, 'student_t_sf', return_value=.1) as sf:
            overlap_t(np.arange(20.), 4)
            self.assertEqual(sf.call_args.args[1], 4)

    def test_segment_metrics_passes_uncompacted_segment_to_overlap_t(self):
        index = pd.date_range('2020', periods=40)
        values = pd.DataFrame(np.tile(np.arange(15.), (40, 1)), index=index)
        ic = pd.Series(np.linspace(-.1, .3, 40), index=index)
        segment = pd.Series(np.arange(40) >= 7, index=index)
        ic.iloc[[7, 10, 20]] = np.nan
        with patch.object(evaluate, 'overlap_t', return_value=123.) as statistic:
            metrics = segment_metrics(ic, values, values * .001, values.notna(), segment, 3, MARKETS['crypto'])
        self.assertEqual(metrics['t'], 123.)
        statistic.assert_called_once()
        pd.testing.assert_series_equal(statistic.call_args.args[0], ic.loc[segment])
        self.assertEqual(statistic.call_args.args[1], 3)
        sample = ic.loc[segment].dropna()
        self.assertEqual(metrics['n'], len(sample))
        self.assertEqual(metrics['ic'], sample.mean())
        self.assertEqual(metrics['icir'], sample.mean() / sample.std(ddof=1))

    def test_greedy_ensemble_passes_uncompacted_training_ic(self):
        panels = planted(rows=100, columns=15)
        index = panels.mask.index
        train = pd.Series(np.arange(100) >= 7, index=index)
        ic = pd.Series(np.linspace(-.1, .3, train.sum()), index=index[train])
        ic.iloc[[0, 3, 20]] = np.nan
        factor = dict(id='signal', expr=SIGNAL_EXPR, direction=1, train=dict(t=3.))
        with patch.object(greedy_ensemble, 'cs_rank_ic', return_value=ic) as rank_ic, \
             patch.object(greedy_ensemble, 'overlap_t', return_value=3.) as statistic:
            kept = greedy_ensemble.greedy_ensemble_factors(
                [factor], panels, forward_target(panels, 3)[0], train, 3, MARKETS['crypto'])
        self.assertEqual(kept, [factor])
        pd.testing.assert_index_equal(rank_ic.call_args.args[1].index, ic.index)
        statistic.assert_called_once()
        pd.testing.assert_series_equal(statistic.call_args.args[0], ic)
        self.assertEqual(statistic.call_args.args[1], 3)

    def test_nw_t_compacts_nan_series_before_legacy_calculation(self):
        x = pd.Series(np.linspace(-.5, .8, 50))
        x.iloc[[0, 3, 6, 30]] = np.nan
        for h in (1, 3, 7):
            with self.subTest(h=h):
                self.assertEqual(nw_t(x, h), nw_t(x.dropna(), h))

    def test_signals_noise_and_direction_multiple_seeds(self):
        for seed in (3, 17, 42, 100):
            panels = planted(rows=750, seed=seed)
            targets = forward_target(panels, 1)
            splits = make_splits(panels.mask.index, 1)
            signal = evaluate_expr(SIGNAL_EXPR, panels.fields)
            for sign in (1, -1):
                result = evaluate_factor(signal*sign, *targets, panels.mask, splits, 1, MARKETS['crypto'])
                self.assertEqual(result['direction'], sign)
                self.assertEqual(classify(result['train'], result['val'], 200, t_required(200)), 'accepted')
            noise = pd.DataFrame(np.random.default_rng(seed+900).normal(size=signal.shape),
                                 index=signal.index, columns=signal.columns)
            result = evaluate_factor(noise, *targets, panels.mask, splits, 1, MARKETS['crypto'])
            self.assertEqual(classify(result['train'], result['val'], 200, t_required(200)), 'rejected')

    def test_threshold_and_classification_boundaries(self):
        self.assertEqual(t_required(1), 2.)
        values = [t_required(k) for k in range(1, 1000)]
        self.assertTrue(all(a <= b for a, b in zip(values, values[1:])))
        train = dict(ic=.02, t=5., ls_ret=.05, ic_halves=[.01, .03])
        val = dict(ic=.01, t=1.65, ls_ret=.01)
        self.assertEqual(classify(train, val, 10, 3.), 'accepted')
        self.assertEqual(classify(train, dict(val, t=1.64), 10, 3.), 'candidate')
        self.assertEqual(classify(train, dict(val, ls_ret=0), 10, 3.), 'candidate')
        self.assertEqual(classify(dict(train, ls_ret=-.01), val, 10, 3.), 'candidate')
        self.assertEqual(classify(dict(train, ic_halves=[-.01, .03]), val, 10, 3.), 'rejected')
        self.assertEqual(classify(dict(train, ic_halves=[0., .03]), val, 10, 3.), 'rejected')
        self.assertEqual(classify(train, val, 10, 6.), 'rejected')

    def test_hac_noise_distribution_and_autocorrelation(self):
        rng = np.random.default_rng(481)
        estimates = np.array([nw_t(rng.normal(size=600), 5) for _ in range(300)])
        self.assertLess(abs(estimates.mean()), .18)
        self.assertGreater(estimates.std(), .8)
        self.assertLess(estimates.std(), 1.2)
        x = np.zeros(2000)
        for t in range(1, len(x)):
            x[t] = .9*x[t-1] + rng.normal()
        x += .5
        naive = x.mean()/x.std(ddof=1)*np.sqrt(len(x))
        self.assertLess(abs(nw_t(x, 20)), abs(naive))
        self.assertEqual(nw_t([1.]*19, 3), 0.)
        self.assertTrue(np.isfinite(nw_t([1.]*30, 3)))

    def test_tied_rank_pairwise_mask_and_min_names(self):
        index = pd.date_range('2020', periods=2)
        values = pd.DataFrame([[1, 1, 3, 4, 99], [1, 2, 3, 4, 5]], index=index)
        target = pd.DataFrame([[2, 2, 4, 3, -999], [1, 2, np.nan, 4, 5]], index=index)
        mask = values.notna()
        mask.iloc[:, -1] = False
        ic = cs_rank_ic(values, target, mask, 4)
        expected = np.corrcoef([1.5, 1.5, 3, 4], [1.5, 1.5, 4, 3])[0, 1]
        self.assertAlmostEqual(ic.iloc[0], expected)
        self.assertTrue(np.isnan(ic.iloc[1]))
        self.assertAlmostEqual(cross_corr(values, values, index), 1.)

    def test_turnover_costs_and_segment_schedule_hand_calculation(self):
        index = pd.date_range('2020', periods=5)
        v = pd.DataFrame(np.tile(np.arange(15), (5, 1)), index=index, dtype=float)
        raw = v*.001
        v.iloc[2, :3], v.iloc[2, -3:] = [12, 13, 14], [0, 1, 2]
        segment = pd.Series(True, index=index)
        cfg = dict(MARKETS['crypto'], one_way_cost=.001)
        net, turnover = long_short(v, raw, v.notna(), segment, 2, cfg)
        np.testing.assert_allclose(net, [.012-.004, -.012-.004, .012-.004])
        np.testing.assert_allclose(turnover, [1, 1, 1])
        v.iloc[2] = v.iloc[0]
        net, turnover = long_short(v, raw, v.notna(), segment, 2, cfg)
        np.testing.assert_allclose(net, [.008, .012, .012])
        np.testing.assert_allclose(turnover, [1, 0, 0])
        # A warmup miss doesn't move the h-day schedule to day 1.
        v.iloc[0] = np.nan
        net, _ = long_short(v, raw, raw.notna(), segment, 2, cfg)
        self.assertEqual(list(net.index), [index[2], index[4]])
        metric = segment_metrics(pd.Series(.1, index=index), v, raw, raw.notna(), segment, 2, cfg)
        self.assertAlmostEqual(metric['ls_ret'], .01 * 365/2)

    def test_validity_and_future_values_causality(self):
        panels = planted(rows=350)
        index = panels.mask.index
        splits = make_splits(index, 1)
        targets = forward_target(panels, 1)
        values = evaluate_expr(SIGNAL_EXPR, panels.fields)
        for invalid in (values*0, values*float('nan'), 1+values*1e-16):
            self.assertIsNone(evaluate_factor(invalid, *targets, panels.mask, splits, 1, MARKETS['crypto']))
        sparse = values.copy()
        sparse.iloc[:300] = np.nan
        self.assertIsNone(evaluate_factor(sparse, *targets, panels.mask, splits, 1, MARKETS['crypto']))
        full = cs_rank_ic(values, targets[0], panels.mask, 15)
        altered = values.copy()
        altered.iloc[151:] = np.random.default_rng(22).normal(size=altered.iloc[151:].shape)
        modified = cs_rank_ic(altered, targets[0], panels.mask, 15)
        pd.testing.assert_series_equal(full.iloc[:151], modified.iloc[:151])
        prefix = cs_rank_ic(values.iloc[:151], targets[0].iloc[:151], panels.mask.iloc[:151], 15)
        pd.testing.assert_series_equal(full.iloc[:151], prefix)

    def test_vectorized_performance(self):
        panels = planted(rows=2500, columns=200)
        values = evaluate_expr(SIGNAL_EXPR, panels.fields)
        targets = forward_target(panels, 1)
        splits = make_splits(values.index, 1)
        begin = time.perf_counter()
        result = evaluate_factor(values, *targets, panels.mask, splits, 1, MARKETS['crypto'])
        elapsed = time.perf_counter()-begin
        self.assertIsNotNone(result)
        self.assertLess(elapsed, .6, f'2500x200 single horizon took {elapsed:.3f}s')
