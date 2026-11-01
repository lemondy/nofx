"""Offline synthetic checks for predeclared probability estimates and BT execution."""
import math
import unittest

import numpy as np
import pandas as pd

from scripts.backtest import probability_study as p
from scripts.backtest import volrix


def frame(closes, opens=None, start='2000-01-01', frequency='D'):
    c = np.asarray(closes, dtype=float)
    o = c if opens is None else np.asarray(opens, dtype=float)
    return pd.DataFrame(dict(open=o, close=c, high=np.maximum(o, c),
                             low=np.minimum(o, c), volume=1.),
                        index=pd.date_range(start, periods=len(c), freq=frequency))


class VarianceRatioTests(unittest.TestCase):
    def test_iid_gaussian_random_walk(self):
        returns = np.random.default_rng(31415).normal(0, .01, 20000)
        for q in p.Q_DAILY:
            with self.subTest(q=q):
                result = p.variance_ratio(returns, q)
                self.assertLess(abs(result['vr']-1), .15)
                self.assertLess(abs(result['z']), 3)

    def test_positive_and_negative_ar1(self):
        for phi in (.2, -.2):
            innovations = np.random.default_rng(442).normal(size=62000)
            returns = np.zeros(len(innovations))
            for i in range(1, len(returns)):
                returns[i] = phi*returns[i-1]+innovations[i]
            result = p.variance_ratio(returns[2000:], 2)
            self.assertAlmostEqual(result['vr'], 1+phi, delta=.025)
            self.assertGreater(result['z'] if phi > 0 else -result['z'], 5)

    def test_unbiased_overlap_and_heteroskedastic_variance(self):
        r = np.array([.01, -.03, .02, -.01, .05, .03, -.02, .04])
        q, n = 3, len(r)
        e = r-r.mean()
        v1 = sum(e*e)/(n-1)
        vq = sum((sum(r[i:i+q])-q*r.mean())**2 for i in range(n-q+1))/(q*(n-q+1)*(1-q/n))
        theta = sum((2*(q-j)/q)**2*sum(e[j:]**2*e[:-j]**2)/sum(e*e)**2 for j in range(1, q))
        result = p.variance_ratio(r, q)
        self.assertAlmostEqual(result['vr'], vq/v1)
        self.assertAlmostEqual(result['theta'], theta)

    def test_holm_known_values_and_original_order(self):
        np.testing.assert_allclose(p.holm_adjust([.01, .04, .03, .002]), [.03, .06, .06, .008])
        np.testing.assert_allclose(p.holm_adjust([.9, .9, 1.]), [1., 1., 1.])


class BarrierTests(unittest.TestCase):
    def test_brownian_formula_zero_small_drift_and_monte_carlo(self):
        self.assertEqual(p.p_up(0., .2, .1, .08), .08/.18)
        self.assertAlmostEqual(p.p_up(1e-14, .2, .1, .08), .08/.18, places=10)
        # 5000 fine-step Brownian paths, stopped at first contact, no GBM drift.
        for nu in (.02, -.02):
            rng = np.random.default_rng(108)
            x = np.zeros(5000)
            live = np.ones(5000, dtype=bool)
            up = np.zeros(5000, dtype=bool)
            dt = .0001
            for _ in range(100000):
                indexes = np.flatnonzero(live)
                if not len(indexes):
                    break
                x[indexes] += nu*dt+.2*math.sqrt(dt)*rng.normal(size=len(indexes))
                won = x[indexes] >= .1
                lost = x[indexes] <= -.08
                up[indexes[won]] = True
                live[indexes[won | lost]] = False
            self.assertFalse(live.any())
            self.assertLess(abs(up.mean()-p.p_up(nu, .2, .1, .08)), .02)
        self.assertAlmostEqual(p.p_up(100, .2, .1, .08), 1.)
        self.assertAlmostEqual(p.p_up(-100, .2, .1, .08), 0.)

    def test_zero_log_drift_ohlc_from_fine_steps(self):
        rng = np.random.default_rng(1701)
        n, steps, sigma = 60000, 64, .01
        increments = rng.normal(0, sigma/math.sqrt(steps), size=(n, steps))
        intra = np.cumsum(increments, axis=1)
        starts = np.r_[0., np.cumsum(intra[:, -1])[:-1]]
        closes = 100*np.exp(starts+intra[:, -1])
        opens = 100*np.exp(starts)
        df = frame(closes, opens, start='1900-01-01')
        df['high'] = 100*np.exp(starts+np.maximum(0., intra.max(axis=1)))
        df['low'] = 100*np.exp(starts+np.minimum(0., intra.min(axis=1)))
        for ku, kd in [(1, 1), (2, 1)]:
            result = p.barrier_events(df, np.full(n, sigma), ku, kd)
            outcomes = np.array([x['outcome'] for x in result['events']])
            hits = np.isin(outcomes, [-1, 1])
            observed = (outcomes == 1).sum()/hits.sum()
            self.assertLess(abs(observed-kd/(ku+kd)), .03)
            summary = p.summarize_barriers(result['events'], ku, kd, 0., sigma, 1, 0., 200)
            self.assertLess(abs(summary['expectation_z']), 3)
            self.assertEqual(summary['drift_expectation_s'], 0.)

    def test_gap_exit_uses_open_and_precedes_intrabar_double_touch(self):
        df = frame([100.]*33)
        df.loc[df.index[1], ['open', 'high', 'low', 'close']] = [80., 130., 75., 100.]
        long = p.barrier_events(df, np.full(33, .1), 2, 1, interval=1)['events'][0]
        self.assertEqual(long['outcome'], -1)
        self.assertEqual(long['value'], -1.)
        self.assertAlmostEqual(long['gap_value'], math.log(.8)/.1)
        self.assertEqual(long['holding_days'], 1.)
        summary = p.summarize_barriers([long], 2, 1, .001, .1, 1, .0006, 20)
        self.assertAlmostEqual(summary['gap_net_expectation_s'], math.log(.8)/.1-.006)
        self.assertAlmostEqual(summary['expectation_excess_s'], math.log(.8)/.1-.01)
        short = p.barrier_events(df, np.full(33, .1), 1, 2, direction=-1, interval=1)['events'][0]
        self.assertEqual(short['outcome'], 1)
        self.assertAlmostEqual(short['gap_value'], -long['gap_value'])

    def test_drift_probability_uses_each_local_sigma(self):
        events = [dict(date=pd.Timestamp(f'2020-0{i+1}-01'), s=s, outcome=out,
                       value=value, gap_value=value, holding_days=days)
                  for i, (s, out, value, days) in enumerate([(.01, 1, 2., 3.), (.03, -1, -1., 7.)])]
        for direction in (1, -1):
            summary = p.summarize_barriers(events, 2, 1, .001, .05, direction, 0., 30)
            expected = np.mean([p.p_up(direction*.001, e['s'], 2*e['s'], e['s']) for e in events])
            old = np.mean([p.p_up(direction*.001, .05, 2*e['s'], e['s']) for e in events])
            self.assertAlmostEqual(summary['p_drift'], expected)
            self.assertAlmostEqual(summary['p_drift_global'], old)
            self.assertGreater(abs(expected-old), .01)
            drift = np.mean([direction*.001*e['holding_days']/e['s'] for e in events])
            self.assertAlmostEqual(summary['drift_expectation_s'], drift)
            self.assertAlmostEqual(summary['expectation_excess_s'], .5-drift)

    def test_double_touch_without_fine_is_ambiguous(self):
        df = frame([100.]*33)
        df.loc[df.index[1], ['high', 'low']] = [120., 80.]
        result = p.barrier_events(df, np.full(33, .1), 1, 1, interval=1)
        self.assertEqual(result['events'][0]['outcome'], 2)
        summary = p.summarize_barriers(result['events'], 1, 1, 0, .1, 1, .0016, 20)
        self.assertEqual(summary['ambiguous'], 1)
        self.assertEqual(summary['hits'], 0)
        self.assertEqual(result['right_censored'], 30)

    def test_fine_resolves_first_hit_and_requires_complete_parent(self):
        df = frame([100.]*33)
        df.loc[df.index[1], ['high', 'low']] = [120., 80.]
        fine = frame([100.]*288, start=str(df.index[1]), frequency='5min')
        fine.loc[fine.index[2], 'high'] = 120.
        fine.loc[fine.index[9], 'low'] = 80.
        args = (df, np.full(33, .1), 1, 1)
        self.assertEqual(p.barrier_events(*args, interval=1, fine=fine)['events'][0]['outcome'], 1)
        self.assertEqual(p.barrier_events(*args, interval=1, direction=-1, fine=fine)['events'][0]['outcome'], -1)
        incomplete = fine.drop(fine.index[100])
        self.assertEqual(p.barrier_events(*args, interval=1, fine=incomplete)['events'][0]['outcome'], 2)
        fine.loc[fine.index[2], 'low'] = 80.
        self.assertEqual(p.barrier_events(*args, interval=1, fine=fine)['events'][0]['outcome'], 2)

    def test_fine_gap_uses_resolved_bar_open_and_actual_close_time(self):
        df = frame([100.]*33)
        df.loc[df.index[1], ['high', 'low']] = [130., 75.]
        fine = frame([100.]*288, start=str(df.index[1]), frequency='5min')
        fine.loc[fine.index[2], ['open', 'high', 'low']] = [80., 100., 75.]
        fine.loc[fine.index[9], 'high'] = 130.
        event = p.barrier_events(df, np.full(33, .1), 2, 1, interval=1, fine=fine)['events'][0]
        self.assertEqual(event['outcome'], -1)
        self.assertAlmostEqual(event['gap_value'], math.log(.8)/.1)
        self.assertEqual(event['exit_date'], fine.index[2]+pd.Timedelta(300, unit='s'))
        self.assertAlmostEqual(event['holding_days'], 15/1440)

    def test_volatility_strictly_before_entry_and_month_bootstrap_seed(self):
        df = frame(100*np.exp(np.cumsum(np.random.default_rng(77).normal(0, .02, 100))))
        original = p.entry_scales(df, df, '1d')
        self.assertTrue(np.isnan(original[:31]).all())
        expected = np.diff(np.log(df.close.iloc[:31].to_numpy())).std(ddof=1)
        self.assertAlmostEqual(original[31], expected)
        changed = df.copy()
        changed.iloc[60:, changed.columns.get_loc('close')] *= 10
        amended = p.entry_scales(changed, changed, '1d')
        np.testing.assert_allclose(original[:61], amended[:61], equal_nan=True)
        a = p.month_weights(df.index, 100)
        b = p.month_weights(df.index, 100)
        np.testing.assert_array_equal(a[1], b[1])
        np.testing.assert_array_equal(a[1].sum(axis=1), np.full(100, a[1].shape[1]))

    def test_timeout_actual_return_and_per_entry_cost(self):
        events = [dict(date=pd.Timestamp('2020-01-01'), s=.1, outcome=1, value=2.),
                  dict(date=pd.Timestamp('2020-02-01'), s=.2, outcome=-1, value=-1.),
                  dict(date=pd.Timestamp('2020-03-01'), s=.1, outcome=0, value=.4),
                  dict(date=pd.Timestamp('2020-04-01'), s=.1, outcome=2, value=math.nan)]
        for event in events:
            event.update(gap_value=event['value'], holding_days=1.)
        s = p.summarize_barriers(events, 2, 1, 0, .1, 1, .0016, 100)
        self.assertEqual(s['p_hat'], .5)
        self.assertAlmostEqual(s['expectation_s'], 1.4/3)
        self.assertAlmostEqual(s['net_expectation_s'], (1.4-.016-.008-.016)/3)

    def test_segment_end_requires_same_complete_horizon_even_for_early_hits(self):
        df = frame(100*np.exp(np.cumsum(np.random.default_rng(909).normal(0, .02, 120))))
        df['high'] *= 2  # All entries touch at least one barrier on the next bar.
        result = p.study_p2({('BTCUSDT', '1d'): df}, {}, ['BTCUSDT'], 20, lambda _: None)
        for row in result['records']:
            # Entries 35,40 have a complete 30-day horizon within the first72 bars.
            # Later early hits in that segment cannot change eligibility.
            self.assertEqual(row['front']['n'], 2)
            self.assertEqual(row['full']['n'], 11)
            self.assertEqual(row['back']['n'], 3)
            self.assertEqual(row['boundary_censored'], 6)


class KellyExecutionTests(unittest.TestCase):
    def test_kelly_formulas(self):
        self.assertAlmostEqual(p.kelly_fraction(.08, .2), 2.)
        self.assertAlmostEqual(p.kelly_growth(1, .08, .2), .06)
        self.assertAlmostEqual(p.kelly_growth(2, .08, .2), .08)
        self.assertAlmostEqual(p.kelly_growth(4, .08, .2), 0.)
        self.assertLess(p.kelly_growth(4.1, .08, .2), 0.)

    def test_daily_rebalance_native_equity_matches_constant_return(self):
        n, r, f = 31, .01, .5
        closes = 100*(1+r)**np.arange(n)
        opens = np.r_[closes[0], closes[:-1]]
        result = p.run_daily_rebalance(frame(closes, opens), f, commission=0, slippage=0)
        self.assertEqual(result['rebalance_decisions'], n)
        # Every executable daily decision fills; the terminal close has no next open.
        self.assertEqual(result['fill_count'], n-1)
        expected = 1000*(1+f*r)**np.arange(n)
        np.testing.assert_allclose(list(result['daily_equity'].values()), expected, rtol=2e-8)
        self.assertEqual(result['commission_paid'], 0.)
        self.assertEqual(result['engine'], 'backtrader')

    def test_native_fee_and_funding_direction(self):
        df = frame([100.]*5)
        result = p.run_daily_rebalance(df, .5, commission=.001, slippage=0,
                                       funding={x: .001 for x in df.index})
        self.assertGreater(result['commission_paid'], .49)
        self.assertGreater(result['funding_paid'], 4.)
        self.assertLess(result['final_equity'], 995.1)
        self.assertEqual(result['funding_days'], 5)

    def test_run_rule_default_cap_unchanged_and_explicit_cap(self):
        df = frame([100., 101., 102.])
        for f in (1.00001, -1.00001):
            with self.assertRaisesRegex(ValueError, 'positions'):
                volrix.run_rule(df, [f]*len(df), commission=0, slippage=0)
        result = volrix.run_rule(df, [2.]*len(df), commission=0, slippage=0, max_abs_target=2.)
        self.assertEqual(result['fills'][0]['size'], 20.)


if __name__ == '__main__':
    unittest.main()
