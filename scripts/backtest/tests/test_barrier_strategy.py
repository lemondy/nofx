"""Offline synthetic native OCO, accounting, sample and gate checks."""
import math
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest import barrier_strategy as b


def frame(rows=None, n=7, start='2024-01-01'):
    rows = rows if rows is not None else [(100., 101., 99., 100.)]*n
    df = pd.DataFrame(rows, columns=['open', 'high', 'low', 'close'],
                      index=pd.date_range(start, periods=len(rows)))
    df['volume'] = 1000.
    return df


def run(df, **kw):
    settings = dict(scales=np.full(len(df), .1), interval=100, commission=0., slippage=0.)
    settings.update(kw)
    return b.run_barrier(df, **settings)


class NativeBarrierTests(unittest.TestCase):
    def test_oco_take_profit_cancels_stop(self):
        df = frame()
        df.iloc[2, :4] = [110., 125., 105., 120.]
        r = run(df)
        self.assertEqual([x['role'] for x in r['fills']], ['entry', 'take_profit'])
        self.assertAlmostEqual(r['fills'][1]['price'], 100*math.exp(.2))
        self.assertAlmostEqual(r['closed'][0]['pnlcomm'], 10*(100*math.exp(.2)-100))
        self.assertTrue(any(x['role'] == 'stop_loss' and x['status'] == 'Canceled' for x in r['order_statuses']))
        self.assertEqual(r['metrics']['trades'], 1)
        self.assertEqual(r['metrics']['win_rate'], 1.)
        self.assertEqual(r['metrics']['average_holding_days'], 1.)

    def test_oco_stop_cancels_take_profit(self):
        df = frame()
        df.iloc[2, :4] = [100., 101., 89., 95.]
        r = run(df)
        self.assertEqual([x['role'] for x in r['fills']], ['entry', 'stop_loss'])
        self.assertAlmostEqual(r['fills'][1]['price'], 100*math.exp(-.1))
        self.assertTrue(any(x['role'] == 'take_profit' and x['status'] == 'Canceled' for x in r['order_statuses']))

    def test_timeout_cancels_oco_then_next_open_market_exit(self):
        df = frame()
        df.iloc[4, :4] = [102., 103., 99., 100.]
        r = run(df, max_days=2)
        self.assertEqual([x['role'] for x in r['fills']], ['entry', 'timeout'])
        self.assertEqual(r['fills'][1]['date'][:10], '2024-01-05')
        self.assertEqual(r['fills'][1]['signal_date'][:10], '2024-01-04')
        self.assertEqual(r['fills'][1]['price'], 102.)
        self.assertEqual(r['metrics']['average_holding_days'], 3.)
        canceled = [x['role'] for x in r['order_statuses'] if x['status'] == 'Canceled']
        self.assertCountEqual(canceled, ['stop_loss', 'take_profit'])

    def test_stop_gap_executes_at_open(self):
        df = frame()
        df.iloc[2, :4] = [80., 85., 75., 82.]
        r = run(df)
        self.assertEqual(r['fills'][1]['role'], 'stop_loss')
        self.assertEqual(r['fills'][1]['price'], 80.)
        self.assertAlmostEqual(r['metrics']['total_return'], -.2)
        costed = run(df, commission=.0001, slippage=.0002)
        self.assertAlmostEqual(costed['fills'][0]['price'], 100*1.0002)
        self.assertAlmostEqual(costed['fills'][1]['price'], 80*.9998)
        self.assertAlmostEqual(costed['commission_paid'], 10*(100*1.0002+80*.9998)*.0001)

    def test_average_exposure_uses_native_position_and_nav(self):
        df = frame(n=6)
        r = run(df)
        self.assertAlmostEqual(r['metrics']['average_exposure'], 5/6)
        self.assertEqual(list(r['daily_exposure'].values()), [0., 1., 1., 1., 1., 1.])
        df['open'] = df['close'] = np.arange(6)*2.+100
        df['high'], df['low'] = df.close+1, df.close-1
        r = run(df)
        expected = [0.]+[(10*df.close.iloc[i])/(1000+10*(df.close.iloc[i]-102)) for i in range(1, 6)]
        np.testing.assert_allclose(list(r['daily_exposure'].values()), expected, atol=1e-9)
        self.assertAlmostEqual(r['metrics']['average_exposure'], np.mean(expected))

    def test_matched_exposure_calls_shared_implementation(self):
        df = frame(n=6)
        df['open'] = df['close'] = np.arange(6)+100.
        df['high'], df['low'] = df.close+1, df.close-1
        with patch.object(b.orders, 'matched_exposure_logs', wraps=b.orders.matched_exposure_logs) as matched:
            r = run(df)
        matched.assert_called_once()
        args = matched.call_args.args
        np.testing.assert_allclose(args[0], np.log(df.close/df.close.shift(fill_value=100.)))
        self.assertEqual(args[1], r['metrics']['average_exposure'])
        np.testing.assert_allclose(r['matched_benchmark_logs'], np.log1p(args[1]*df.close.pct_change().fillna(0.)))

    def test_oos_does_not_enter_gate_exposure_or_returns(self):
        df = frame(n=10, start='2025-12-27')
        df.iloc[5:, :4] *= 1.5
        full, prefix = run(df), run(df.iloc[:5])
        self.assertEqual(full['gate_logs'], prefix['gate_logs'])
        self.assertEqual(full['metrics']['average_exposure'], prefix['metrics']['average_exposure'])
        self.assertEqual(full['metrics']['sharpe'], prefix['metrics']['sharpe'])
        self.assertAlmostEqual(full['metrics']['oos_return'], .5)
        self.assertEqual(prefix['metrics']['oos_return'], None)
        self.assertEqual(full['exposure_observations'], 5)

    def test_entry_phase_and_decision_barrier_anchor(self):
        df = frame(n=12)
        scales = np.r_[np.full(4, np.nan), np.full(8, .1)]
        df.iloc[6, :4] = [110., 111., 109., 110.]
        df.iloc[7, :4] = [110., 125., 105., 120.]
        r = run(df, scales=scales, interval=5)
        self.assertEqual(r['fills'][0]['signal_date'][:10], '2024-01-06')
        self.assertEqual(r['fills'][0]['date'][:10], '2024-01-07')
        self.assertEqual(r['fills'][0]['price'], 110.)
        self.assertAlmostEqual(r['fills'][1]['price'], 100*math.exp(.2))

    def test_entry_bar_oco_activation_and_native_double_touch_order(self):
        df = frame()
        df.iloc[1, :4] = [100., 140., 70., 100.]
        df.iloc[2, :4] = [100., 140., 70., 100.]
        r = run(df)
        self.assertEqual(r['fills'][1]['date'][:10], '2024-01-03')
        self.assertEqual(r['fills'][1]['role'], 'stop_loss')

    def test_funding_and_commission_are_native_cash_charges(self):
        df = frame(n=6)
        r = run(df, commission=.001, funding={x: .001 for x in df.index})
        self.assertAlmostEqual(r['commission_paid'], 1.)
        self.assertAlmostEqual(r['funding_paid'], 12.)
        self.assertAlmostEqual(r['metrics']['total_return'], -.013, delta=1e-8)


class GateTests(unittest.TestCase):
    def test_predeclared_dsr_and_exact_ratios(self):
        s = np.tile([-.001, .003], 100)
        base = np.tile([-.002, .002], 100)
        etfs = {asset: dict(gate_logs=(s+i*.00001).tolist(), matched_benchmark_logs=base.tolist(),
                            metrics=dict(sharpe=b.orders.sharpe(s+i*.00001, 252), benchmark_sharpe=0.))
                for i, asset in enumerate(b.US10)}
        platform = [dict(metrics=dict(sharpe=1. if i < 11 else -1., benchmark_sharpe=0.)) for i in range(15)]
        with patch.object(b.orders, 'deflated_sharpe', wraps=b.orders.deflated_sharpe) as dsr:
            a = b.evaluate_gates(etfs, platform)
        self.assertEqual(dsr.call_args.args[1], 528)
        expected_var = np.var([r['metrics']['sharpe']/math.sqrt(252) for r in etfs.values()], ddof=1)
        self.assertAlmostEqual(dsr.call_args.args[2], expected_var)
        self.assertEqual(a['verdict'], 'PASS')
        self.assertEqual(a['platform_wins'], 11)
        platform[10]['metrics']['sharpe'] = -1.
        for asset in b.US10[:5]:
            etfs[asset]['metrics']['benchmark_sharpe'] = 100.
        a = b.evaluate_gates(etfs, platform)
        self.assertFalse(a['gates']['G4'])
        self.assertFalse(a['gates']['G5'])


class NativeShortTests(unittest.TestCase):
    def short(self, df, **kw):
        return run(df, direction='short', cutoff=None, annual_days=365, **kw)

    def test_oco_take_profit_buys_and_cancels_stop(self):
        df = frame()
        df.iloc[2, :4] = [100., 102., 75., 80.]
        r = self.short(df)
        self.assertEqual([f['role'] for f in r['fills']], ['entry', 'take_profit'])
        self.assertEqual(r['fills'][0]['size'], -10.)
        self.assertEqual(r['fills'][1]['size'], 10.)
        self.assertAlmostEqual(r['fills'][1]['price'], 100*math.exp(-.2))
        self.assertAlmostEqual(r['closed'][0]['pnlcomm'], 10*(100-100*math.exp(-.2)))
        self.assertEqual(r['metrics']['win_rate'], 1.)
        self.assertEqual(r['metrics']['average_holding_days'], 1.)
        self.assertFalse(r['metrics']['open_at_gate'])
        self.assertTrue(any(o['role'] == 'stop_loss' and o['status'] == 'Canceled' for o in r['order_statuses']))

    def test_oco_stop_loss_buys_and_cancels_take(self):
        df = frame()
        df.iloc[2, :4] = [100., 112., 99., 110.]
        r = self.short(df)
        self.assertEqual([f['role'] for f in r['fills']], ['entry', 'stop_loss'])
        self.assertAlmostEqual(r['fills'][1]['price'], 100*math.exp(.1))
        self.assertLess(r['closed'][0]['net_pnl'], 0.)
        self.assertTrue(any(o['role'] == 'take_profit' and o['status'] == 'Canceled' for o in r['order_statuses']))

    def test_timeout_cancels_oco_and_buys_next_open(self):
        df = frame()
        df.iloc[4, :4] = [102., 103., 99., 100.]
        r = self.short(df, max_days=2)
        self.assertEqual([f['role'] for f in r['fills']], ['entry', 'timeout'])
        self.assertEqual(r['fills'][1]['signal_date'][:10], '2024-01-04')
        self.assertEqual(r['fills'][1]['date'][:10], '2024-01-05')
        self.assertEqual(r['fills'][1]['price'], 102.)
        self.assertEqual(r['fills'][1]['size'], 10.)
        self.assertAlmostEqual(r['closed'][0]['net_pnl'], -20.)
        self.assertEqual(r['metrics']['average_holding_days'], 3.)
        self.assertCountEqual([o['role'] for o in r['order_statuses'] if o['status'] == 'Canceled'],
                              ['stop_loss', 'take_profit'])

    def test_stop_gap_executes_at_open_with_native_costs(self):
        df = frame()
        df.iloc[2, :4] = [130., 135., 125., 132.]
        r = self.short(df)
        self.assertEqual(r['fills'][1]['role'], 'stop_loss')
        self.assertEqual(r['fills'][1]['price'], 130.)
        self.assertAlmostEqual(r['metrics']['total_return'], -.3)
        r = self.short(df, commission=.0005, slippage=.0003)
        self.assertAlmostEqual(r['fills'][0]['price'], 100*.9997)
        self.assertAlmostEqual(r['fills'][1]['price'], 130*1.0003)
        self.assertAlmostEqual(r['commission_paid'], 10*(100*.9997+130*1.0003)*.0005)

    def test_double_touch_is_stop_first_and_entry_bar_unprotected(self):
        df = frame()
        df.iloc[1:3, :4] = [100., 140., 70., 100.]
        r = self.short(df)
        self.assertEqual(r['fills'][1]['role'], 'stop_loss')
        self.assertEqual(r['fills'][1]['date'][:10], '2024-01-03')

    def test_positive_funding_increases_short_equity(self):
        df = frame(n=6)
        plain = self.short(df)
        r = self.short(df, commission=.001, funding={t: .001 for t in df.index})
        self.assertAlmostEqual(r['commission_paid'], 1.)
        self.assertAlmostEqual(r['funding_paid'], -12.)
        self.assertAlmostEqual(r['metrics']['funding_income'], 12.)
        self.assertAlmostEqual(r['metrics']['total_return'], .011, delta=1e-8)
        gains = np.asarray(list(r['daily_equity'].values()))-np.asarray(list(plain['daily_equity'].values()))
        np.testing.assert_allclose(gains, [0., -1., 2., 5., 8., 11.], atol=1e-6)
        closed = self.short(df, max_days=2, funding={t: .001 for t in df.index})
        self.assertAlmostEqual(closed['closed'][0]['funding'], -9.)
        self.assertAlmostEqual(closed['closed'][0]['net_pnl'], 9.)

    def test_short_phase_scale_anchor_and_open_position(self):
        df = frame(n=12)
        scales = np.r_[np.full(4, np.nan), np.full(8, .1)]
        df.iloc[6, :4] = [90., 91., 89., 90.]
        df.iloc[7, :4] = [90., 91., 75., 80.]
        r = self.short(df, scales=scales, interval=5)
        self.assertEqual(r['fills'][0]['signal_date'][:10], '2024-01-06')
        self.assertEqual(r['fills'][0]['date'][:10], '2024-01-07')
        self.assertAlmostEqual(r['fills'][1]['price'], 100*math.exp(-.2))
        self.assertTrue(self.short(frame())['metrics']['open_at_gate'])

    def test_short_benchmark_simple_returns_and_ruin(self):
        logs, at = b.short_benchmark_logs([0., -.2, -2., .5], .5)
        self.assertEqual(at, 2)  # equality at -100% is also bankruptcy
        np.testing.assert_allclose(logs[:2], [0., math.log(.9)])
        self.assertEqual(logs[2], -math.inf)
        self.assertEqual(logs[3], 0.)
        self.assertEqual(b.short_json_safe(dict(sharpe=-math.inf)), dict(sharpe='-inf'))
        df = frame(n=8)
        df.iloc[2, :4] = [100., 102., 75., 80.]
        df.iloc[3:, :4] = [2000., 2001., 1999., 2000.]
        r = self.short(df)
        self.assertTrue(r['metrics']['benchmark_bankrupt'])
        self.assertEqual(r['metrics']['benchmark_bankrupt_date'][:10], '2024-01-04')
        self.assertEqual(r['metrics']['benchmark_sharpe'], -math.inf)
        self.assertFalse(r['metrics']['bankrupt'])
        self.assertTrue(b._beats(r))
        exposure = r['metrics']['average_exposure']
        expected, _ = b.short_benchmark_logs(-df.close.pct_change().fillna(0.), exposure)
        np.testing.assert_allclose(r['matched_benchmark_logs'], expected)

    def test_entry_bar_insolvency_leaves_one_native_oco_exit(self):
        df = frame()
        df.iloc[1, :4] = [100., 260., 99., 250.]
        df.iloc[2:, :4] = [240., 245., 235., 240.]
        r = self.short(df)
        self.assertEqual([f['role'] for f in r['fills']], ['entry', 'stop_loss'])
        self.assertEqual(sum(f['size'] for f in r['fills']), 0.)
        self.assertTrue(r['metrics']['bankrupt'])
        self.assertIsNone(r['metrics']['sharpe'])
        self.assertAlmostEqual(r['daily_simple_returns'][1], -1.5)
        self.assertAlmostEqual(r['metrics']['total_return'], -1.4)


class ShortBasketTests(unittest.TestCase):
    def test_daily_simple_equal_weights_and_prelisting_exclusion(self):
        dates = pd.date_range('2024-01-01', periods=3)
        s, bankrupt = b.equal_weight_logs({
            'old': pd.Series(np.log1p([.1, .2, -.1]), index=dates),
            'new': pd.Series(np.log1p([-.1, .3]), index=dates[1:])})
        np.testing.assert_allclose(s, np.log1p([.1, .05, .1]))
        self.assertIsNone(bankrupt)
        ruined, _ = b.equal_weight_logs({'dead': pd.Series([0., -math.inf, 0.], index=dates),
                                         'live': pd.Series([0., 0., 0.], index=dates)})
        np.testing.assert_allclose(ruined, [0., math.log(.5), 0.])
        all_dead, at = b.equal_weight_logs({'dead': pd.Series([0., -math.inf, 0.], index=dates)})
        self.assertEqual(at[:10], '2024-01-02')
        self.assertEqual(all_dead.iloc[1], -math.inf)
        invalid, _ = b.equal_weight_logs({'bad': pd.Series([0., np.nan, 0.], index=dates),
                                          'live': pd.Series([0., 0., 0.], index=dates)})
        self.assertTrue(math.isnan(invalid.iloc[1]))

    @staticmethod
    def synthetic_run(s, start='2024-01-01', benchmark=None):
        dates = pd.date_range(start, periods=len(s))
        return dict(daily_equity={t.isoformat(): 1000. for t in dates}, gate_logs=list(s),
                    matched_benchmark_logs=list(np.zeros(len(s)) if benchmark is None else benchmark),
                    metrics=dict(sharpe=b.orders.sharpe(s, 365) if np.isfinite(s).all() else None,
                                 benchmark_sharpe=0.))

    def test_windows_split_each_coin_own_length(self):
        runs = {'old': self.synthetic_run(np.arange(10)*.001),
                'new': self.synthetic_run(np.arange(5)*.002, '2024-01-06')}
        front = b.aggregate_short(runs, 'front')
        back = b.aggregate_short(runs, 'back')
        self.assertEqual(front['dates'][-1][:10], '2024-01-08')
        self.assertEqual(back['dates'][0][:10], '2024-01-07')
        self.assertAlmostEqual(front['gate_logs'][5], math.log1p((math.expm1(.005)+0.)/2))
        self.assertAlmostEqual(back['gate_logs'][-1], math.log1p((math.expm1(.009)+math.expm1(.008))/2))

    def test_subaccount_simple_loss_over_100_percent_is_aggregated_before_log(self):
        runs = {'loss': self.synthetic_run([0., np.nan, 0.]),
                'flat': self.synthetic_run([0., 0., 0.])}
        runs['loss']['daily_simple_returns'] = [0., -1.5, 0.]
        runs['loss']['metrics']['sharpe'] = None
        basket = b.aggregate_short(runs)
        np.testing.assert_allclose(basket['gate_logs'], [0., math.log(.25), 0.])
        a = b.evaluate_short_gates(runs, basket, [dict(metrics=dict(sharpe=0., benchmark_sharpe=0.))])
        self.assertFalse(a['gates']['G2'])
        self.assertIsNone(a['dsr'])
        self.assertEqual(a['dsr_invalid_coins'], ['loss'])

    def test_predeclared_dsr_32_coin_variance_21_neighbors_and_ratios(self):
        s = np.tile([-.001, .003], 100)
        runs = {str(i): self.synthetic_run(s+i*.000001) for i in range(32)}
        basket = b.aggregate_short(runs)
        platform = [dict(metrics=dict(sharpe=1. if i < 15 else -1., benchmark_sharpe=0.)) for i in range(21)]
        with patch.object(b.orders, 'deflated_sharpe', wraps=b.orders.deflated_sharpe) as dsr:
            a = b.evaluate_short_gates(runs, basket, platform)
        self.assertEqual(dsr.call_args.args[1], 528)
        np.testing.assert_allclose(dsr.call_args.args[0], basket['gate_logs'])
        self.assertAlmostEqual(dsr.call_args.args[2], np.var([r['metrics']['sharpe']/math.sqrt(365)
                                                           for r in runs.values()], ddof=1))
        self.assertEqual(a['verdict'], 'PASS')
        self.assertEqual(a['platform_wins'], 15)
        platform[14]['metrics']['sharpe'] = -1.
        for i in range(13):
            runs[str(i)]['metrics']['benchmark_sharpe'] = 100.
        a = b.evaluate_short_gates(runs, basket, platform)
        self.assertFalse(a['gates']['G4'])
        self.assertFalse(a['gates']['G5'])


if __name__ == '__main__':
    unittest.main()
