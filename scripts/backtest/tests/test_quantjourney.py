"""Synthetic tests: QJ ports, native Backtrader execution, costs and gates."""
import math
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest import quantjourney as q


def frame(closes, *, opens=None, start='2024-01-01', freq='D'):
    c = np.asarray(closes, dtype=float)
    o = c if opens is None else np.asarray(opens, dtype=float)
    return pd.DataFrame(dict(open=o, high=np.maximum(o, c)+1,
                             low=np.minimum(o, c)-.5, close=c, volume=100.),
                        index=pd.date_range(start, periods=len(c), freq=freq))


def run_single(prices, targets, **kwargs):
    return q.run_weights({'A': prices}, pd.DataFrame({'A': targets}, index=prices.index),
                         commission=0., slippage=0., keep_fills=True, **kwargs)


class CalendarAndPolicy(unittest.TestCase):
    def test_bme_bqe_and_incomplete_tail(self):
        dates = pd.bdate_range('2024-01-01', '2024-04-17').difference(pd.to_datetime(['2024-03-29']))
        month = dates[q.calendar_flags(dates, q.RebalancePolicy('BME'))]
        quarter = dates[q.calendar_flags(dates, q.RebalancePolicy('BQE'))]
        self.assertEqual(month.strftime('%Y-%m-%d').tolist(), ['2024-01-31', '2024-02-29', '2024-03-28'])
        self.assertEqual(quarter.strftime('%Y-%m-%d').tolist(), ['2024-03-28'])

    def test_weekly_holiday_and_monday_snap(self):
        dates = pd.bdate_range('2024-03-25', '2024-04-12').difference(pd.to_datetime(['2024-03-29', '2024-04-01']))
        friday = dates[q.calendar_flags(dates, q.RebalancePolicy('W', weekday=4))]
        monday = dates[q.calendar_flags(dates, q.RebalancePolicy('W', weekday=0))]
        self.assertEqual(friday.strftime('%Y-%m-%d').tolist(), ['2024-03-28', '2024-04-05', '2024-04-12'])
        self.assertEqual(monday.strftime('%Y-%m-%d').tolist(), ['2024-03-25', '2024-04-02', '2024-04-08'])

    def test_drift_strictly_above_band(self):
        c = q.RebalanceController(q.RebalancePolicy(None, drift_threshold=.05))
        c.decide(np.array([.5, .5]), np.array([0., 0.]), 1000, initial=True)
        self.assertIsNone(c.decide(np.array([.5, .5]), np.array([.54, .46]), 1000))
        intent = c.decide(np.array([.5, .5]), np.array([.56, .44]), 1000)
        self.assertEqual(intent[2], 'drift')
        np.testing.assert_allclose(intent[0], [.5, .5])

    def test_partial_only_breached_assets_and_native_fills(self):
        c = q.RebalanceController(q.RebalancePolicy('D', drift_threshold=.1, partial_rebalance=True))
        desired, keep, _ = c.decide(np.array([.45, .21, .2]), np.array([.25, .2, .2]), 1000, calendar=True)
        np.testing.assert_array_equal(keep, [False, True, True])
        np.testing.assert_allclose(desired[1:], [.2, .2])
        f = frame([100]*4)
        w = pd.DataFrame([[.2,.2,.2], [.45,.21,.2], [.45,.21,.2], [.45,.21,.2]], index=f.index, columns=list('ABC'))
        r = q.run_weights({s:f for s in 'ABC'}, w, policy=q.RebalancePolicy('D', drift_threshold=.1, partial_rebalance=True),
                          commission=0, slippage=0, charge_carry=False, keep_fills=True)
        later = [t for t in r['fills'] if t['execution_bar'] > 1]
        self.assertEqual({t['symbol'] for t in later}, {'A'})

    def test_signal_changes_use_post_normalization_targets(self):
        c = q.RebalanceController(q.RebalancePolicy(None, rebalance_on_signal_change=True, signal_change_threshold=.01))
        c.decide(np.array([.5, .5]), np.zeros(2), 1000, initial=True)
        self.assertIsNone(c.decide(np.array([.505, .495]), np.array([.5,.5]), 1000))
        self.assertEqual(c.decide(np.array([.6,.4]), np.array([.5,.5]), 1000)[2], 'signal')

    def test_circuit_breaker_flattens_and_cools_n_bars(self):
        prices = frame([100,100,80,80,80,80,80,80])
        policy = q.RebalancePolicy(None, max_drawdown_trigger=-.15, circuit_breaker_cooldown_days=2)
        r = run_single(prices, [1.]*8, policy=policy, charge_carry=False)
        self.assertEqual([(f['execution_bar'], f['reason']) for f in r['fills']],
                         [(1,'initial'), (3,'circuit_breaker'), (6,'post_cooldown')])
        self.assertLess(r['fills'][1]['size'], 0)
        self.assertGreater(r['fills'][2]['size'], 0)
        self.assertEqual(r['metrics']['rebalance_stats']['circuit_breaker'], 1)

    def test_literal_w10_positive_threshold_bug_is_preserved(self):
        c = q.RebalanceController(q.RULES['W10'].policy)
        c.decide(np.zeros(2), np.zeros(2),1000,initial=True)
        self.assertEqual(c.decide(np.zeros(2),np.zeros(2),1000)[2], 'circuit_breaker')

    def test_te_and_turnover_budget(self):
        c = q.RebalanceController(q.RebalancePolicy(None, tracking_error_threshold=.01,
                                  tracking_error_window=3, max_annual_turnover=.5))
        target, current = np.array([.5]), np.array([.5])
        for i, r in enumerate([.01,-.01]):
            self.assertIsNone(c.decide(target,current,1000,portfolio_return=r,benchmark_return=0))
        intent = c.decide(target,current,1000,portfolio_return=.01,benchmark_return=0)
        self.assertEqual(intent[2], 'tracking_error')
        # Calendar and TE are both vetoed when the gross churn budget is spent.
        self.assertIsNone(c.decide(np.array([0.]),current,1000,calendar=True,
                                  portfolio_return=0,benchmark_return=0,executed_turnover=.2))
        self.assertEqual(c.stats['turnover_veto'],1)


class NativeExecution(unittest.TestCase):
    def test_previous_close_signal_fills_exact_next_open_quantity(self):
        f = frame([100,111,121], opens=[100,110,120])
        r = run_single(f,[1.,0.,0.],charge_carry=False)
        fill = r['fills'][0]
        self.assertEqual(fill['signal_bar'],0)
        self.assertEqual(fill['execution_bar'],1)
        self.assertEqual(fill['price'],110)
        self.assertAlmostEqual(fill['size'],1000/110)
        self.assertAlmostEqual(r['daily_equity']['2024-01-01'],1000)
        self.assertAlmostEqual(r['daily_equity']['2024-01-02'],1000+1000/110)

    def test_reproduction_next_close_is_different_from_evaluation(self):
        f = frame([100,111,121], opens=[100,110,120])
        a = run_single(f,[1.,0.,0.],charge_carry=False)
        b = run_single(f,[1.,0.,0.],charge_carry=False,mode='reproduction')
        self.assertEqual(a['fills'][0]['price'],110)
        self.assertEqual(b['fills'][0]['price'],111)
        self.assertEqual(b['fills'][0]['execution_bar'],1)
        self.assertAlmostEqual(b['daily_equity']['2024-01-02'],1000)
        self.assertNotAlmostEqual(a['daily_equity']['2024-01-03'],b['daily_equity']['2024-01-03'])

    def test_no_signal_or_sizing_from_future_close_or_extrema(self):
        f = frame([100,111,121], opens=[100,110,120])
        modified = f.copy()
        modified.loc[modified.index[1], ['close','high']] = [500,501]
        a = run_single(f,[.5,0.,0.],charge_carry=False)
        b = run_single(modified,[.5,0.,0.],charge_carry=False)
        self.assertEqual(a['fills'][0]['price'],b['fills'][0]['price'])
        self.assertEqual(a['fills'][0]['size'],b['fills'][0]['size'])
        # Future high/low do not cap fixed bps open slippage.
        flat = frame([100]*3)
        flat[['high','low']] = 100.
        r = q.run_weights({'A':flat},pd.DataFrame({'A':[.5,0,0]},index=flat.index),
                          commission=0,slippage=.01,charge_carry=False,keep_fills=True)
        self.assertEqual(r['fills'][0]['price'],101.)

    def test_last_close_signal_never_fills_without_next_bar(self):
        r = run_single(frame([100]*3),[0.,0.,1.],charge_carry=False)
        self.assertEqual(r['fills'],[])

    def test_reproduction_preserves_qj_fast_partial_weight_state_bug(self):
        a, b = frame([100,100,110,110]), frame([100]*4)
        weights = pd.DataFrame({'A':.2,'B':.2},index=a.index)
        policy = q.RebalancePolicy('D',drift_threshold=.1,partial_rebalance=True)
        evaluate = q.run_weights({'A':a,'B':b},weights,policy=policy,commission=0,slippage=0,
                                 charge_carry=False,keep_fills=True)
        reproduce = q.run_weights({'A':a,'B':b},weights,policy=policy,commission=0,slippage=0,
                                  mode='reproduction',charge_carry=False,keep_fills=True)
        self.assertEqual(len(evaluate['fills']),2)
        self.assertGreater(len(reproduce['fills']),2)

    def test_funding_and_borrow_signs_and_financing(self):
        f = frame([100]*4)
        dates = {t.date():.001 for t in f.index}
        common = dict(policy=q.RebalancePolicy(None), market='crypto', funding={'A':dates})
        long = run_single(f,[1.]*4,**common)
        short = run_single(f,[-1.]*4,**common)
        self.assertAlmostEqual(long['metrics']['funding_paid'],6.)
        self.assertAlmostEqual(short['metrics']['funding_paid'],-6.)
        self.assertAlmostEqual(long['daily_equity']['2024-01-04'],994.)
        self.assertAlmostEqual(short['daily_equity']['2024-01-04'],1006.)
        us_short = run_single(f,[-1.]*4,policy=q.RebalancePolicy(None))
        us_long = run_single(f,[1.]*4,policy=q.RebalancePolicy(None))
        lever = run_single(f,[2.]*4,policy=q.RebalancePolicy(None))
        self.assertGreater(us_short['metrics']['borrow_paid'],0)
        self.assertEqual(us_long['metrics']['borrow_paid'],0)
        self.assertGreater(lever['metrics']['financing_paid'],0)
        self.assertLess(lever['daily_equity']['2024-01-04'],1000)

    def test_us_carry_includes_weekend_days(self):
        f = frame([100]*4,start='2024-01-04',freq='B')  # Thu Fri Mon Tue
        r = run_single(f,[-1.]*4,policy=q.RebalancePolicy(None))
        self.assertAlmostEqual(r['metrics']['borrow_paid'],1000*.005*4/365)

    def test_gate_exposure_excludes_post_publication_positions(self):
        f = frame([100]*6,start='2025-12-29')
        common = dict(charge_carry=False,exposure_end='2025-12-31')
        steady = run_single(f,[.15]*6,**common)
        changed = run_single(f,[.15,.15,.15,.9,.9,.9],**common)
        prefix = run_single(f.iloc[:3],[.15]*3,**common)
        for run in (steady,changed,prefix):
            self.assertAlmostEqual(run['metrics']['gate_average_exposure'],.1)
        self.assertNotEqual(steady['metrics']['average_exposure'],changed['metrics']['average_exposure'])
        self.assertEqual(list(steady['daily_equity'].values())[:3],list(changed['daily_equity'].values())[:3])

    def test_missing_prelisting_asset_is_never_bought(self):
        a, b = frame([100]*6), frame([100]*3,start='2024-01-04')
        close = pd.concat({'A':a.close,'B':b.close},axis=1)
        weights = close.notna().astype(float)*.4
        r = q.run_weights({'A':a,'B':b},weights,commission=0,slippage=0,charge_carry=False,keep_fills=True)
        trades = [t for t in r['fills'] if t['symbol']=='B']
        self.assertTrue(trades)
        self.assertTrue(all(t['execution_bar']>=4 for t in trades))


class RiskAndSignals(unittest.TestCase):
    def test_vol_target_leverage_cap_and_prior_window(self):
        index = pd.bdate_range('2024-01-01',periods=16)
        w = pd.DataFrame({'A':.6,'B':.4},index=index)
        returns = pd.DataFrame({'A':np.resize([.002,-.002],16),'B':np.resize([.003,-.003],16)},index=index)
        model = q.VolTargetModel(.4,4,1.5,'D')
        actual = model.adjust(w,returns)
        self.assertLessEqual(actual.abs().sum(axis=1).max(),1.5+1e-10)
        self.assertAlmostEqual(actual.iloc[5].sum(),1.5)
        changed = returns.copy(); changed.iloc[5]=10.
        np.testing.assert_allclose(model.adjust(w,changed).iloc[5],actual.iloc[5])

    def test_erc_diagonal_equal_and_unequal_variance(self):
        model = q.RiskParityModel()
        np.testing.assert_allclose(model.solve_erc(np.diag([.04,.04])),[.5,.5])
        covariance = np.diag([.04,.16])
        w = model.solve_erc(covariance)
        np.testing.assert_allclose(w,[2/3,1/3],rtol=1e-8)
        rc = w*(covariance@w)
        self.assertAlmostEqual(rc[0],rc[1])

    def test_position_cap_redistributes_without_activating_zero(self):
        w = pd.DataFrame([[.8,.1,.1,0.]],columns=list('ABCD'))
        actual = q.PositionLimitModel(.4).adjust(w)
        np.testing.assert_allclose(actual.iloc[0],[.4,.3,.3,0.])
        self.assertAlmostEqual(actual.iloc[0].sum(),1.)

    def test_all_25_signals_and_overlays_are_prefix_causal(self):
        rng = np.random.default_rng(923)
        c = pd.DataFrame(100*np.exp(np.cumsum(rng.normal(.0005,.02,(340,10)),axis=0)),
                         index=pd.date_range('2022-01-01',periods=340),columns=q.COINS)
        for rule in q.RULES:
            with self.subTest(rule=rule):
                data = c.iloc[:,:2] if rule in ('W13','W14') else c
                full = q.compute_weights(rule,data,market='crypto')
                self.assertTrue(np.isfinite(full.to_numpy()).all())
                for count in (63,127,253,321):
                    prefix = q.compute_weights(rule,data.iloc[:count],market='crypto')
                    np.testing.assert_allclose(prefix.iloc[-1],full.iloc[count-1],atol=1e-10)

    def test_declared_primaries_and_original_universes(self):
        self.assertEqual(len(q.RULES),25)
        self.assertEqual(len(q.US_RULES),19)
        for name,spec in q.RULES.items():
            self.assertIn(len(spec.primary),(1,2))
            self.assertTrue(all(p in spec.parameters for p in spec.primary))
        self.assertEqual(q.RULES['W01'].universe,q.SECTORS)
        self.assertEqual(q.RULES['W14'].universe,('EWA','EWC'))


class StatisticsAndAdmission(unittest.TestCase):
    def test_dsr_known_numerical_example(self):
        # SR=.1/day, T=1000, normal moments, N=20, Var[SR]=.0001:
        # E[max SR]=.0190070795118; z=2.55356414971; Phi(z)=.994668668247.
        self.assertAlmostEqual(q.dsr_from_moments(.1,1000,0.,3.,20,.0001),.9946686682467962,places=12)
        self.assertLess(q.dsr_from_moments(.01,1000,0.,3.,20,.0001),.5)
        self.assertEqual(q.deflated_sharpe([0.]*100,20,.0001),0.)
        self.assertEqual(q.deflated_sharpe([math.nan]*100,20,.0001),0.)

    def test_dsr_lower_tail_preserves_small_probabilities(self):
        # SR=-1, T=151, normal moments, SR0=0 gives z=-10 exactly.
        self.assertTrue(math.isclose(q.dsr_from_moments(-1.,151,0.,3.,1,0.),
                                     7.61985302416047e-24,rel_tol=1e-12))

    def test_dsr_known_return_series_with_nonnormal_moments(self):
        # T=80; daily SR=.296518305041; skew=-.233178238297;
        # Pearson kurtosis=1.76350777338; N=44, V=.0004.
        returns = np.tile([-.02,.01,.03,-.01,.02,.04,-.03,.015],10)
        self.assertAlmostEqual(q.deflated_sharpe(returns,44,.0004),.9841914857485007,places=12)

    def test_fixed_fifteen_percent_exposure_has_no_timing_edge(self):
        simple = np.tile([.10,-.08],500)
        benchmark = np.log1p(simple)
        strategy = np.log1p(.15*simple)  # rebalance to 15% every day
        gate = q.admission(strategy,benchmark,365,dsr=.99,plateau='PLATEAU',average_exposure=.15)
        original = gate['full_exposure_audit']
        self.assertTrue(all(original[w+'_sharpe'] > original[w+'_sharpe_bench'] for w in ('train','holdout')))
        self.assertFalse(gate['gates']['G1'])
        for window in ('train','holdout'):
            self.assertAlmostEqual(gate['audit'][window+'_sharpe'],gate['audit'][window+'_sharpe_bench'])

    def test_real_timing_edge_beats_matched_exposure_in_both_windows(self):
        simple = np.tile([.10,-.08],500)
        # A perfectly predictive synthetic signal holds 30% before each up
        # day and cash before each down day: mean exposure remains 15%.
        exposure = np.tile([.30,0.],500)
        gate = q.admission(np.log1p(exposure*simple),np.log1p(simple),365,
                           dsr=.99,plateau='PLATEAU',average_exposure=float(exposure.mean()))
        self.assertTrue(gate['gates']['G1'])
        self.assertEqual(gate['status'],'PASS')

    def test_matched_benchmark_cash_leverage_and_invalid_returns(self):
        benchmark = np.log1p([.1,-.2,.05])
        np.testing.assert_array_equal(q.matched_exposure_benchmark(benchmark,0.),np.zeros(3))
        np.testing.assert_array_equal(q.matched_exposure_benchmark(benchmark,1.),benchmark)
        np.testing.assert_allclose(q.matched_exposure_benchmark(benchmark,2.),np.log1p([.2,-.4,.1]))
        np.testing.assert_array_equal(q.matched_exposure_benchmark([0.]*100,2.),np.zeros(100))
        cash = q.admission(.001+.002*np.sin(np.arange(100)),[0.]*100,365,
                           dsr=.99,plateau='PLATEAU',average_exposure=2.)
        self.assertTrue(cash['gates']['G1'])
        self.assertEqual(cash['audit'],cash['full_exposure_audit'])
        for invalid in (-1.,math.nan,math.inf):
            with self.subTest(exposure=invalid), self.assertRaises(ValueError):
                q.matched_exposure_benchmark(benchmark,invalid)
        # Bankruptcy cannot be filtered out to turn G1 into a pass; G3 still
        # tests the strategy's own two halves independently of that benchmark.
        bankrupt = np.zeros(100); bankrupt[50] = math.log(.4)
        gate = q.admission(.001+.002*np.sin(np.arange(100)),bankrupt,365,
                           dsr=.99,plateau='PLATEAU',average_exposure=2.)
        self.assertFalse(gate['gates']['G1'])
        self.assertTrue(gate['gates']['G3'])

    def test_study_uses_own_dsr_and_groups_btc_eth_once_for_variance(self):
        cases = {}
        simple = np.tile([.10,-.08],100)
        benchmark = np.log1p(simple)

        def fake_case(rule,market,root):
            symbols = ('BTC','ETH') if market == 'crypto' and rule in q.SINGLE else ('basket',)
            rows = []
            for symbol in symbols:
                if symbol == 'BTC':
                    own = np.log1p(np.tile([.3,0.],100)*simple)
                elif symbol == 'ETH':
                    own = np.log1p(.15*simple)
                else:
                    own = int(rule[1:])*.0001+.002*np.sin(np.arange(200))
                row = dict(rule=rule,market=market,symbol=symbol,_s=own.tolist(),_b=benchmark.tolist(),
                           gate_metrics=dict(average_exposure=.15),sweep=dict(status='PLATEAU'))
                if rule in q.INTRADAY:
                    # The intraday gate remains decisive despite failed daily
                    # diagnostics, including DSR and the parameter plateau.
                    row['intraday'] = dict(status='PASS',checks={})
                    row['sweep']['status'] = 'N/A（日内专用门槛）'
                cases[(rule,market,symbol)] = own
                rows.append(row)
            return rows,None

        with patch.object(q,'_case_job',side_effect=fake_case):
            result = q.run_study(market='all',workers=1,progress=lambda message: None)
        # Intraday W07-W09 have their own gate and stay out of the DSR pool.
        self.assertEqual(result['trials'],41)
        self.assertEqual(len(result['results']),48)
        own_scores, excess_scores = {}, {}
        for (rule,market,symbol),own in cases.items():
            if rule in q.INTRADAY:
                continue
            own_scores.setdefault((rule,market),[]).append(q.qa.sharpe(own.tolist(),1))
            excess_scores.setdefault((rule,market),[]).append(q.qa.sharpe((own-benchmark).tolist(),1))
        variance = float(np.var([np.mean(v) for v in own_scores.values()],ddof=1))
        excess_variance = float(np.var([np.mean(v) for v in excess_scores.values()],ddof=1))
        self.assertAlmostEqual(result['sharpe_variance'],variance,places=14)
        self.assertAlmostEqual(result['excess_sharpe_variance_original'],excess_variance,places=14)
        self.assertNotAlmostEqual(variance,excess_variance)
        for row in result['results']:
            with self.subTest(rule=row['rule'],market=row['market'],symbol=row['symbol']):
                own = cases[(row['rule'],row['market'],row['symbol'])]
                gate = row['admission']
                self.assertAlmostEqual(gate['dsr'],q.deflated_sharpe(own,41,variance),places=14)
                self.assertAlmostEqual(gate['dsr_excess_original'],
                                       q.deflated_sharpe(own-benchmark,41,excess_variance),places=14)
                if row['rule'] in q.INTRADAY:
                    self.assertEqual(gate['status'],'PASS')
                    self.assertFalse(gate['gates']['G4'])
                else:
                    self.assertEqual(gate['gates']['G2'],gate['dsr'] >= .95)
        single = {r['symbol']:r['admission'] for r in result['results']
                  if r['rule'] == 'W01' and r['market'] == 'crypto'}
        self.assertTrue(single['BTC']['gates']['G1'])
        self.assertFalse(single['ETH']['gates']['G1'])
        self.assertNotEqual(single['BTC']['dsr'],single['ETH']['dsr'])
        self.assertEqual(result['pass_summary']['W01/crypto'],'FAIL')

    def test_constructed_pass_fail_gates(self):
        returns = (.001 + .002*np.sin(np.arange(200))).tolist()
        benchmark = [0.]*len(returns)
        passed = q.admission(returns,benchmark,252,dsr=.99,plateau='PLATEAU',cross_positive_fraction=.6)
        self.assertEqual(passed['status'],'PASS')
        for kwargs, failed in [({'dsr':.949},'G2'),({'plateau':'LONE_PEAK'},'G4'),
                               ({'cross_positive_fraction':.59},'G5')]:
            params = dict(dsr=.99,plateau='PLATEAU',cross_positive_fraction=.6)
            params.update(kwargs)
            self.assertIn(failed,q.admission(returns,benchmark,252,**params)['failed'])
        loss = [-r for r in returns]
        rejected = q.admission(loss,benchmark,252,dsr=.99,plateau='PLATEAU')
        self.assertIn('G1',rejected['failed'])
        self.assertIn('G3',rejected['failed'])
        self.assertNotIn('G5',rejected['gates'])

    def test_insufficient_samples_cannot_pass_g1(self):
        gate = q.admission([.01]*10,[0.]*10,252,dsr=1.,plateau='PLATEAU')
        self.assertIn('G1',gate['failed'])

    def test_time_alignment_and_bankruptcy_are_not_filtered_away(self):
        a = dict(daily_equity={'2024-01-01':1000.,'2024-01-02':0.,'2024-01-03':-1.})
        b = dict(daily_equity={'2024-01-01':1000.,'2024-01-02':1000.,'2024-01-03':1000.})
        s,bench = q._aligned_logs(a,b)
        self.assertEqual(len(s),3)
        self.assertTrue(s.iloc[1:].isna().all())
        gate = q.admission(s.tolist(),bench.tolist(),252,dsr=0,plateau='PLATEAU')
        self.assertEqual(gate['status'],'FAIL')


if __name__ == '__main__':
    unittest.main()
