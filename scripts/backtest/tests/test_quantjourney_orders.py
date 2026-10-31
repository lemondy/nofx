"""Synthetic order semantics / admission tests. No market files or network."""
import math
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest import quantjourney_orders as q

bt = q.bt


def frame(rows=None, n=8):
    rows = rows or [(100., 101., 99., 100.)]*n
    f = pd.DataFrame(rows, columns=['open', 'high', 'low', 'close'], dtype=float,
                     index=pd.date_range('2024-01-01', periods=len(rows)))
    f['volume'] = 1000.
    return f


def execute(f, callback, *, evaluation=True, slip=0., rates=None, market='crypto'):
    """A real Cerebro/BackBroker for every assertion about fills or P&L."""
    class Probe(bt.Strategy):
        def __init__(self):
            self.events, self.statuses, self.created = [], [], []
            self.available = {'TEST': np.ones(len(f), bool)}
            self.broker.strategy = self

        def next(self):
            callback(self, len(self.data)-1)

        def notify_order(self, o):
            self.statuses.append((o.ref, o.status))
            if o.status == o.Completed:
                self.events.append(dict(date=bt.num2date(o.executed.dt).date().isoformat(),
                                        price=o.executed.price, size=o.executed.size, ref=o.ref))

    c = bt.Cerebro(stdstats=False)
    b = q.OrdersBroker(evaluation=evaluation, rates=rates, market=market)
    b.setcash(q.BROKER_CASH)
    b.set_slippage_perc(slip, slip_open=True, slip_out=True)
    c.setbroker(b)
    c.adddata(bt.feeds.PandasData(dataname=f), name='TEST')
    c.addstrategy(Probe)
    return c.run()[0]


def run_recording_orders(f, rule, **kwargs):
    """Record actual strategy submissions while retaining native execution."""
    orders = []
    submit = q.OrderStrategy.submit

    def record(st, d, size, exectype=bt.Order.Market, **kw):
        o = submit(st, d, size, exectype, **kw)
        if o is not None:
            orders.append(dict(buy=o.isbuy(), exectype=exectype, price=o.created.price,
                               close=float(d.close[0]), prior_high=st.p.arrays[d._name]['prior_close_high'][len(d)-1]))
        return o

    with patch.object(q.OrderStrategy, 'submit', record):
        result = q.run_rule({'LOW': f}, rule, keep_fills=True, **kwargs)
    return result, orders


class NativeOrderTests(unittest.TestCase):
    def test_close_signal_next_open_fill(self):
        f = frame([(100, 105, 99, 104), (110, 113, 109, 112), (112, 113, 111, 112)])
        st = execute(f, lambda s, i: s.buy(size=1) if i == 0 else None)
        self.assertEqual(st.events[0]['date'], '2024-01-02')
        self.assertEqual(st.events[0]['price'], 110)

    def test_limit_requires_touch_and_gets_gap_improvement(self):
        f = frame([(100, 101, 99, 100), (100, 101, 96, 99), (90, 94, 89, 92)])
        st = execute(f, lambda s, i: s.buy(size=1, exectype=bt.Order.Limit, price=95) if i == 0 else None)
        self.assertEqual(len(st.events), 1)
        self.assertEqual(st.events[0]['date'], '2024-01-03')
        self.assertEqual(st.events[0]['price'], 90)

    def test_limit_exact_touch_and_slippage_cannot_cross_limit(self):
        f = frame([(100, 101, 99, 100), (100, 101, 95, 99)])
        st = execute(f, lambda s, i: s.buy(size=1, exectype=bt.Order.Limit, price=95) if i == 0 else None, slip=.01)
        self.assertEqual(st.events[0]['price'], 95)
        f.iloc[1] = [94.9, 100, 94, 99, 1000]
        st = execute(f, lambda s, i: s.buy(size=1, exectype=bt.Order.Limit, price=95) if i == 0 else None, slip=.01)
        self.assertEqual(st.events[0]['price'], 95)

    def test_sell_limit_gap_price(self):
        f = frame([(100, 101, 99, 100), (110, 112, 109, 111)])
        st = execute(f, lambda s, i: s.sell(size=1, exectype=bt.Order.Limit, price=105) if i == 0 else None)
        self.assertEqual(st.events[0]['price'], 110)

    def test_stop_touch_and_gap(self):
        f = frame([(100, 101, 99, 100), (102, 104, 101, 103), (110, 112, 109, 111)])
        st = execute(f, lambda s, i: s.buy(size=1, exectype=bt.Order.Stop, price=105) if i == 0 else None)
        self.assertEqual(st.events[0]['price'], 110)
        f.iloc[1] = [102, 106, 101, 103, 1000]
        st = execute(f, lambda s, i: s.buy(size=1, exectype=bt.Order.Stop, price=105) if i == 0 else None)
        self.assertEqual(st.events[0]['price'], 105)

    def test_stoplimit_trigger_without_fill_then_later_fill(self):
        f = frame([(100, 101, 99, 100), (110, 112, 109, 111), (108, 109, 107, 108), (104, 107, 103, 106)])
        def submit(s, i):
            if i == 0:
                s.created.append(s.buy(size=1, exectype=bt.Order.StopLimit, price=105, plimit=106))
            if i == 2:
                self.assertTrue(s.created[0].triggered)
                self.assertEqual(s.events, [])
        st = execute(f, submit)
        self.assertEqual(st.events[0]['price'], 104)
        self.assertEqual(st.events[0]['date'], '2024-01-04')

    def test_stoplimit_activation_uses_trigger_not_pretrigger_open(self):
        f = frame([(100, 101, 99, 100), (100, 110, 99, 108)])
        st = execute(f, lambda s, i: s.buy(size=1, exectype=bt.Order.StopLimit, price=105, plimit=106) if i == 0 else None)
        self.assertEqual(st.events[0]['price'], 105)

    def test_stoplimit_no_trigger_and_sell_gap_unfilled(self):
        f = frame([(100, 101, 99, 100), (100, 104, 99, 102), (101, 104, 100, 103)])
        st = execute(f, lambda s, i: s.buy(size=1, exectype=bt.Order.StopLimit, price=105, plimit=106) if i == 0 else None)
        self.assertEqual(st.events, [])
        f.iloc[1] = [90, 93, 89, 91, 1000]
        f.iloc[2] = [91, 93, 90, 92, 1000]
        st = execute(f, lambda s, i: s.sell(size=1, exectype=bt.Order.StopLimit, price=96, plimit=95) if i == 0 else None)
        self.assertEqual(st.events, [])

    def test_trailing_stop_tracks_high_not_close(self):
        f = frame([(100, 100, 100, 100), (105, 110, 104, 105), (106, 111, 105, 106), (105, 106, 99, 102)])
        st = execute(f, lambda s, i: s.sell(size=1, exectype=bt.Order.StopTrail, trailpercent=.10) if i == 0 else None)
        self.assertEqual(len(st.events), 1)
        self.assertAlmostEqual(st.events[0]['price'], 99.9)
        self.assertEqual(st.events[0]['date'], '2024-01-04')

    def test_trailing_limit_freezes_after_activation(self):
        f = frame([(100, 100, 100, 100), (110, 111, 108, 110), (90, 95, 89, 92), (94, 98, 93, 97)])
        def submit(s, i):
            if i == 0:
                s.created.append(s.sell(size=1, exectype=bt.Order.StopTrailLimit,
                                        price=100, plimit=98, trailamount=10))
        st = execute(f, submit)
        # Anchor111 -> stop101, limit99. Gap90 cannot fill. Later high98 still <99.
        self.assertEqual(st.events, [])
        self.assertTrue(st.created[0].triggered)
        self.assertAlmostEqual(st.created[0].created.pricelimit, 99)

    def test_bracket_stop_priority_oco_and_entry_bar_stop(self):
        f = frame([(100, 101, 99, 100), (100, 110, 90, 102), (100, 110, 90, 102)])
        def submit(s, i):
            if i == 0:
                s.created.extend(s.buy_bracket(size=1, exectype=bt.Order.Market, stopprice=95, limitprice=105))
        st = execute(f, submit)
        self.assertEqual([x['price'] for x in st.events], [100, 95])
        self.assertEqual(st.events[0]['date'], st.events[1]['date'])
        self.assertEqual(st.position.size, 0)
        self.assertIn((st.created[2].ref, bt.Order.Canceled), st.statuses)

    def test_reproduction_bracket_children_start_next_bar(self):
        f = frame([(100, 101, 99, 100), (100, 110, 90, 102), (100, 110, 90, 102)])
        def submit(s, i):
            if i == 0:
                s.buy_bracket(size=1, exectype=bt.Order.Market, stopprice=95, limitprice=105)
        st = execute(f, submit, evaluation=False)
        self.assertEqual(st.events[1]['date'], '2024-01-03')
        self.assertEqual(st.events[1]['price'], 95)

    def test_bracket_take_profit_cancels_stop(self):
        f = frame([(100, 101, 99, 100), (100, 101, 99, 100), (106, 108, 105, 107), (90, 91, 89, 90)])
        def submit(s, i):
            if i == 0:
                s.created.extend(s.buy_bracket(size=1, exectype=bt.Order.Market, stopprice=95, limitprice=105))
        st = execute(f, submit)
        self.assertEqual([x['price'] for x in st.events], [100, 106])
        self.assertEqual(st.position.size, 0)
        self.assertIn((st.created[1].ref, bt.Order.Canceled), st.statuses)

    def test_oco_fill_cancels_other_leg(self):
        f = frame([(100, 101, 99, 100), (100, 103, 97, 100), (110, 115, 90, 100)])
        def submit(s, i):
            if i == 0:
                a = s.buy(size=1, exectype=bt.Order.Limit, price=98)
                b = s.buy(size=1, exectype=bt.Order.Stop, price=102, oco=a)
                a.addinfo(priority=4)
                b.addinfo(priority=4)
                s.created.extend([a, b])
        st = execute(f, submit)
        self.assertEqual(len(st.events), 1)
        self.assertEqual(st.events[0]['price'], 98)
        self.assertIn((st.created[1].ref, bt.Order.Canceled), st.statuses)

    def test_ttl_last_bar_can_fill_and_expiry_blocks_later_fill(self):
        f = frame([(100, 101, 99, 100), (100, 101, 99, 100), (100, 101, 95, 100), (90, 91, 89, 90)])
        def submit(s, i):
            if i == 0:
                o = s.buy(size=1, exectype=bt.Order.Limit, price=95)
                o.addinfo(ttl=2)
                s.created.append(o)
        self.assertEqual(execute(f, submit).events[0]['date'], '2024-01-03')
        f.iloc[2] = [100, 101, 99, 100, 1000]
        st = execute(f, submit)
        self.assertEqual(st.events, [])
        self.assertIn((st.created[0].ref, bt.Order.Expired), st.statuses)

    def test_oco_expired_first_leg_does_not_skip_second_legs_final_attempt(self):
        f = frame([(100, 101, 99, 100), (100, 104, 96, 100), (100, 106, 96, 100)])
        def submit(s, i):
            if i == 0:
                a = s.buy(size=1, exectype=bt.Order.Limit, price=95)
                b = s.buy(size=1, exectype=bt.Order.Stop, price=105, oco=a)
                a.addinfo(priority=4, ttl=2)
                b.addinfo(priority=4, ttl=2)
        st = execute(f, submit)
        self.assertEqual(len(st.events), 1)
        self.assertEqual(st.events[0]['price'], 105)

    def test_funding_direction_and_actual_cash_settlement(self):
        self.assertAlmostEqual(q.funding_cost(2, 100, .001), .6)
        self.assertAlmostEqual(q.funding_cost(-2, 100, .001), -.6)
        rates = {'TEST': {d.date(): .001 for d in frame().index}}
        for direction in (1, -1):
            st = execute(frame(), lambda s, i: (s.buy if direction > 0 else s.sell)(size=2) if i == 0 else None, rates=rates)
            self.assertAlmostEqual(st.broker.funding_paid, direction*.6*6)
            self.assertAlmostEqual(st.broker.getvalue()-q.BROKER_CASH, -direction*.6*6, delta=1e-6)

    def test_us_borrow_and_leverage_cost_use_strategy_equity(self):
        short = execute(frame(), lambda s, i: s.sell(size=2) if i == 0 else None, market='us')
        self.assertAlmostEqual(short.broker.borrow_paid, 200*.005/365*6)
        # 2000 long notional on real capital1000, despite virtual broker cash.
        long = execute(frame(), lambda s, i: s.buy(size=20) if i == 0 else None, market='us')
        self.assertGreater(long.broker.financing_paid, 1000*.05/365*6)
        self.assertLess(long.broker.financing_paid, 1001*.05/365*6)


class StatisticsAndRulesTests(unittest.TestCase):
    def test_dsr_known_numerical_example(self):
        # Strategy's own log returns, rf=0. Symmetric two-point sample:
        # skew=0, Pearson kurtosis=1, T=100,
        # SR=.25*sqrt(.99). N=10, cross-SR variance=.0025.
        x = np.tile([-.003, .005], 50)
        # SR0=.07872991506728749, z=1.6916472358357775, independently
        # reduced to the Gaussian CDF using symmetry of the two-point sample.
        self.assertAlmostEqual(q.deflated_sharpe(x, 10, .0025), .9546433727903324, places=12)

    def test_dsr_skew_and_nonexcess_kurtosis_known_example(self):
        x = np.tile([-.002, -.001, 0, .003, .006], 20)
        # SR=.40809564574345357, skew=.569229028952313,
        # Pearson kurtosis=1.833391562581885 (not excess kurtosis).
        self.assertAlmostEqual(q.deflated_sharpe(x, 10, .0025), .9998731430150306, places=12)

    def test_family_dsr_uses_own_returns_and_separate_excess_variance(self):
        base = np.tile([-.004, .004], 50)
        family = [({}, base+mean, .01+base*.5, 365) for mean in (.001, .0008, .0012)]
        # Three rule/market portfolios, including any intraday member, count
        # once each. Own SRs=(.25,.20,.30)*sqrt(.99), sample variance=.002475.
        variance, excess_variance = q.apply_family_dsr(family, 3)
        self.assertAlmostEqual(variance, .002475, places=14)
        self.assertAlmostEqual(excess_variance, .0099, places=14)
        for (row, s, b, periods), expected in zip(family, (.9799570073698542, .9403669908082002, .9945807296587905)):
            self.assertAlmostEqual(row['dsr'], expected, places=12)
            self.assertEqual(row['dsr_excess'], 0)
            admission = q.gate_decision(s, b, periods, row['dsr'], 'PLATEAU')
            self.assertEqual(admission['gates']['G2'], expected >= .95)

    def test_dsr_single_trial_and_invalid(self):
        x = np.tile([-.003, .005], 50)
        self.assertGreater(q.deflated_sharpe(x, 1, 0), .99)
        self.assertEqual(q.deflated_sharpe([0]*100, 10, .1), 0)
        self.assertEqual(q.deflated_sharpe([0, float('nan'), 1], 10, .1), 0)
        with self.assertRaises(ValueError):
            q.deflated_sharpe(x, 0, 0)

    def test_gates_constructed_sequences(self):
        s = np.tile([-.001, .003], 100)
        b = np.tile([-.002, .002], 100)
        g = q.gate_decision(s, b, 365, .96, 'PLATEAU', .6)
        self.assertEqual(g['verdict'], 'PASS')
        failed = q.gate_decision(-s, b, 365, .94, 'LONE PEAK', .5)
        self.assertEqual(failed['failed'], ['G1', 'G2', 'G3', 'G4', 'G5'])
        # G1 requires only Sharpe, not audit_returns' combined CAGR verdict.
        s = np.tile([.0005, .0006], 100)
        b = np.tile([-.01, .02], 100)
        self.assertTrue(q.gate_decision(s, b, 252, .96, 'PLATEAU')['gates']['G1'])

    def test_g1_fixed_fifteen_percent_position_loses_mechanical_advantage(self):
        # No timing: rebalance to exactly 15% of the same benchmark each day.
        simple = np.tile([-.08, .09], 100)
        benchmark = np.log1p(simple)
        strategy = np.log1p(.15*simple)
        old = q.audit_returns(strategy.tolist(), benchmark.tolist(), 365)
        self.assertTrue(all(old[w+'_sharpe'] > old[w+'_sharpe_bench']
                            for w in ('train', 'holdout')))
        new = q.gate_decision(strategy, benchmark, 365, .96, 'PLATEAU', average_exposure=.15)
        self.assertFalse(new['gates']['G1'])
        for w in ('train', 'holdout'):
            self.assertAlmostEqual(new['full_exposure_audit'][w+'_sharpe_bench'], old[w+'_sharpe_bench'])
            self.assertAlmostEqual(new['audit'][w+'_sharpe'], new['audit'][w+'_sharpe_bench'])
        self.assertAlmostEqual(new['matched_benchmark_sharpe'], q.sharpe(strategy.tolist(), 365))

    def test_g1_causal_timing_beats_matched_exposure_in_both_windows(self):
        # Persistent synthetic regimes; yesterday's positive return triggers
        # a 30% position today. No current/future return enters the signal.
        simple = np.tile([-.08]*5+[.09]*5, 20)
        exposure = .3*(np.r_[0., simple[:-1]] > 0)
        strategy, benchmark = np.log1p(exposure*simple), np.log1p(simple)
        decision = q.gate_decision(strategy, benchmark, 365, .96, 'PLATEAU',
                                   average_exposure=float(exposure.mean()))
        self.assertEqual(decision['verdict'], 'PASS')
        for w in ('train', 'holdout'):
            self.assertGreater(decision['audit'][w+'_sharpe'], decision['audit'][w+'_sharpe_bench'])

    def test_matched_exposure_scales_simple_returns_and_retains_leverage(self):
        benchmark = np.log1p([-.1, .2, 0.])
        for exposure, expected in ((0., [1., 1., 1.]), (.15, [.985, 1.03, 1.]),
                                   (1., [.9, 1.2, 1.]), (1.5, [.85, 1.3, 1.])):
            with self.subTest(exposure=exposure):
                np.testing.assert_allclose(np.exp(q.matched_exposure_logs(benchmark, exposure)), expected)
        np.testing.assert_array_equal(q.matched_exposure_logs(np.zeros(100), 2.5), np.zeros(100))
        for exposure in (-.1, float('nan'), float('inf')):
            with self.assertRaises(ValueError):
                q.matched_exposure_logs(benchmark, exposure)
        # A leveraged benchmark losing its capital cannot silently become a
        # valid comparison through clipping or removal of the failed dates.
        failed = q.gate_decision(np.tile([-.001, .003], 50), np.log1p(np.tile([-.6, .7], 50)),
                                365, .96, 'PLATEAU', average_exposure=2.)
        self.assertFalse(failed['gates']['G1'])
        self.assertTrue(failed['gates']['G3'])

    def test_intraday_admission_requires_same_coin_and_other_positive(self):
        m = dict(sharpe=.9, halves=[.01, .02], average_net_bps=6, total_return=.03)
        n = dict(sharpe=.1, halves=[-.01, .02], average_net_bps=-1, total_return=.01)
        rows = {'BTC': {'metrics': m}, 'ETH': {'metrics': n}}
        self.assertEqual(q.intraday_decision(rows)['verdict'], 'PASS')
        n['total_return'] = -.01
        self.assertEqual(q.intraday_decision(rows)['verdict'], 'FAIL')

    def test_features_prefix_causal(self):
        rng = np.random.default_rng(231)
        f = frame(n=300)
        f['close'] = 100+np.cumsum(rng.normal(0, .5, len(f)))
        f['open'], f['high'], f['low'] = f.close, f.close+1, f.close-1
        full = q.features(f, {'lookback': 55})
        for cut in (15, 30, 60, 130, 250):
            pd.testing.assert_frame_equal(q.features(f.iloc[:cut], {'lookback': 55}), full.iloc[:cut])

    def test_portfolio_uses_shared_nav_not_average_single_runs(self):
        f = frame(n=80)
        f['open'] = f['close'] = np.arange(80)+100.
        f['high'], f['low'] = f.close+1, f.close-1
        r = q.run_rule({'A': f, 'B': f}, 'O01', market='us')
        self.assertGreater(r['metrics']['average_exposure'], .1)
        self.assertEqual(len(r['entries']), 2)
        self.assertLess(r['metrics']['average_exposure'], .5)

    def test_g1_mean_exposure_excludes_us_oos_without_changing_the_run(self):
        f = frame(n=100)
        f.index = pd.date_range('2025-11-01', periods=len(f))
        f['open'] = f['close'] = np.arange(len(f))+100.
        f['high'], f['low'] = f.close+1, f.close-1
        cutoff = '2025-12-31'
        full = q.run_rule({'A': f, 'B': f}, 'O01', market='us')
        cut = q.run_rule({'A': f, 'B': f}, 'O01', market='us', exposure_end=cutoff)
        prefix = q.run_rule({'A': f.loc[:cutoff], 'B': f.loc[:cutoff]}, 'O01', market='us')
        self.assertAlmostEqual(cut['metrics']['average_exposure'], prefix['metrics']['average_exposure'])
        self.assertLess(cut['metrics']['average_exposure'], full['metrics']['average_exposure'])
        self.assertEqual(cut['daily_equity'], full['daily_equity'])
        self.assertEqual(cut['entries'], full['entries'])

    def test_rule_parameter_coverage(self):
        self.assertEqual(set(q.DEFAULTS), set(q.RULES))
        for r in q.RULES:
            self.assertIn(len(q.DEFAULTS[r]), (1, 2))
            self.assertEqual(len(q.grid_around(q.DEFAULTS[r])), 5)

    def test_stop_rule_waits_for_each_assets_history_after_listing(self):
        f = frame(n=80)
        f['open'] = f['close'] = np.arange(80)+100.
        f['high'], f['low'] = f.close+1, f.close-1
        r = q.run_rule({'A': f, 'B': f.iloc[45:]}, 'O05', market='crypto', keep_fills=True)
        late = [x for x in r['fills'] if x['symbol'] == 'B']
        self.assertTrue(late)
        self.assertTrue(all(x['date'] >= '2024-03-07' for x in late))

    def test_original_minimum_commission_scaled_with_account(self):
        c = q.OriginalCommission(per_share=True)
        self.assertAlmostEqual(c.getcommission(.01, 100), .01)
        self.assertAlmostEqual(c.getcommission(10, 100), .05)

    def test_crypto_stop_breakout_above_prior_high_waits_for_touch(self):
        f = frame([(.08, .0801, .0799, .08)]*12)
        f.iloc[-1, f.columns.get_loc('high')] = .0802
        r, orders = run_recording_orders(f, 'O05', market='crypto', params={'lookback': 2})
        stops = [o for o in orders if o['buy'] and o['exectype'] == bt.Order.Stop]
        self.assertTrue(stops)
        for o in stops:
            self.assertAlmostEqual(o['price'], .08016, places=12)
            self.assertGreater(o['price'], o['prior_high'])
            self.assertGreater(o['price'], o['close'])
        # Earlier bars never touch .08016: a cent-rounded .08 would fill early.
        self.assertEqual(len(r['fills']), 1)
        self.assertEqual(r['fills'][0]['date'], f.index[-1].date().isoformat())
        self.assertGreaterEqual(r['fills'][0]['price'], .08016)

    def test_crypto_pullback_limit_below_close_does_not_fill_without_dip(self):
        f = frame([(.0795+i*.0001, .07955+i*.0001, .07945+i*.0001, .0795+i*.0001) for i in range(8)])
        r, orders = run_recording_orders(f, 'O04', market='crypto', params={'slow': 2})
        limits = [o for o in orders if o['buy'] and o['exectype'] == bt.Order.Limit]
        self.assertTrue(limits)
        for o in limits:
            self.assertAlmostEqual(o['price'], o['close']*.99, places=12)
            self.assertLess(o['price'], o['close'])
        self.assertEqual(r['fills'], [])

    def test_us_stop_and_limit_keep_cent_ticks_in_both_modes(self):
        for reproduction in (False, True):
            for rule, params, reference, multiplier in (('O04', {'slow': 2}, 'close', .99),
                                                        ('O05', {'lookback': 2}, 'prior_high', 1.002)):
                with self.subTest(reproduction=reproduction, rule=rule):
                    f = frame([(100.123+i*.01, 100.128+i*.01, 100.118+i*.01, 100.123+i*.01) for i in range(8)])
                    _, orders = run_recording_orders(f, rule, market='us', params=params, reproduction=reproduction)
                    self.assertTrue(orders)
                    for o in orders:
                        if o['buy']:
                            self.assertEqual(o['price'], round(o[reference]*multiplier, 2))

    def test_subcent_crypto_limit_stays_positive_and_can_fill(self):
        f = frame(n=50)
        f['open'] = f['close'] = np.linspace(.004, .001, 50)
        f['high'], f['low'] = f.close*1.01, f.close*.99
        r, orders = run_recording_orders(f, 'O03', market='crypto')
        buys = [o for o in orders if o['buy']]
        self.assertTrue(buys)
        for o in buys:
            self.assertGreater(o['price'], 0)
            self.assertLess(o['price'], o['close'])
            self.assertAlmostEqual(o['price'], o['close']*.985, places=12)
        self.assertTrue(r['fills'])
        self.assertTrue(all(x['price'] > 0 for x in r['fills']))

    def test_monthly_benchmark_holiday_session_rebalances_next_open(self):
        f = frame(n=8)
        f.index = pd.to_datetime(['2024-05-24', '2024-05-28', '2024-05-29', '2024-05-30',
                                  '2024-05-31', '2024-06-03', '2024-06-04', '2024-06-05'])
        other = f.copy()
        other['close'] = other['open'] = np.arange(8)*10.+100
        other['high'], other['low'] = other.close+1, other.close-1
        r = q.run_rule({'A': f, 'B': other}, 'O01', market='us', benchmark=True, keep_fills=True)
        rebalances = [x for x in r['fills'] if x['date'] == '2024-06-03']
        self.assertEqual(len(rebalances), 2)
        self.assertTrue(all(x['signal_time'].startswith('2024-05-31') for x in rebalances))

    def test_intraday_basket_benchmark_rebalances_once_at_month_end_close(self):
        f = frame(n=6)
        f.index = pd.date_range('2024-01-31 23:45', periods=6, freq='5min')
        other = f.copy()
        other['open'] = other['close'] = np.arange(6)+100.
        other['high'], other['low'] = other.close+1, other.close-1
        r = q.run_rule({'A': f, 'B': other}, 'O15', benchmark=True, keep_fills=True)
        times = [x['signal_time'] for x in r['fills']]
        self.assertEqual(len(times), 4)
        self.assertEqual(times.count('2024-01-31T23:55:00'), 2)
        self.assertNotIn('2024-01-31T23:50:00', times)

    def test_metrics_half_returns_retain_boundary_return(self):
        m = q.metrics([1100, 1210, 1331, 1464.1])
        np.testing.assert_allclose(m['halves'], [.21, .21])
        self.assertAlmostEqual((1+m['halves'][0])*(1+m['halves'][1])-1, m['total_return'])


if __name__ == '__main__':
    unittest.main()
