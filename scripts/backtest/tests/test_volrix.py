"""Synthetic offline causal-signal and native broker contract checks."""
import json
import math
import tempfile
import unittest
from pathlib import Path

import numpy as np
import pandas as pd

from scripts.backtest import volrix as v


def frame(closes, start='2023-01-01', freq='D', opens=None):
    c = np.asarray(closes, dtype=float)
    o = c if opens is None else np.asarray(opens, dtype=float)
    return pd.DataFrame(dict(open=o, high=np.maximum(o, c)+1,
                             low=np.minimum(o, c)-1, close=c, volume=1.),
                        index=pd.date_range(start, periods=len(c), freq=freq))


class Signals(unittest.TestCase):
    def test_every_rule_is_causal(self):
        rng = np.random.default_rng(8317)
        c = 100*np.exp(np.cumsum(rng.normal(.0001, .025, 620)))
        points = sorted(set(rng.integers(0, len(c), 65)) | {0, 1, 9, 19, 20, 24, 63, 199, 251, 272, 619})
        for name, (func, params, tf, _) in v.RULES.items():
            with self.subTest(rule=name):
                df = frame(c, freq={'1d': 'D', '1h': 'h', '5m': '5min'}[tf])
                full = func(df, **params)
                self.assertEqual(full.shape, (len(c),))
                self.assertTrue(np.isfinite(full).all())
                self.assertTrue((abs(full) <= 1).all())
                for i in points:
                    self.assertAlmostEqual(func(df.iloc[:i+1], **params)[-1], full[i], msg=f'{name} at {i}')
                np.testing.assert_array_equal(full, func(df.copy(), **params))

    def test_max20_includes_current_and_full_warmup(self):
        np.testing.assert_array_equal(v.max20_positions(frame([1, 2, 3, 2, 3, 4]), window=3), [0, 0, 1, 0, 1, 1])

    def test_turtle_prior_channels_and_no_same_bar_reverse(self):
        df = frame([100, 100, 100, 103, 104, 99, 96, 101])
        np.testing.assert_array_equal(v.turtle_positions(df, entry_window=3, exit_window=2), [0, 0, 0, 1, 1, 0, -1, 0])

    def test_high52_hysteresis(self):
        np.testing.assert_array_equal(v.high52_positions(frame([100, 100, 100, 96, 89, 85, 95]), window=3), [0, 0, 1, 1, 0, 0, 1])

    def test_post_down_hold_and_cooldown(self):
        r = np.zeros(20)
        r[[1, 3, 11, 12]] = -.03
        df = frame(100*np.cumprod(1+r))
        expected = np.zeros(20)
        expected[1:6] = 1
        expected[12:17] = 1
        np.testing.assert_array_equal(v.post_down_positions(df), expected)
        run = v.run_rule(df, expected, commission=0, slippage=0)
        self.assertEqual(run['entry_dates'][0], '2023-01-03')
        self.assertEqual(run['closed_trades_by_date'][0], '2023-01-08')

    def test_post_down_vol_uses_previous_returns(self):
        r = np.array([0, .01, -.01, .01, -.01, -.05, 0, 0])
        p = v.post_down_vol_positions(frame(100*np.cumprod(1+r)), rv_window=4)
        self.assertEqual(p[4], 0)
        self.assertEqual(p[5], 1)

    def test_tom_calendar_and_leap_year(self):
        df = frame(np.full(65, 100), start='2024-01-28')
        p = pd.Series(v.tom_positions(df), index=df.index)
        for day, expected in [('2024-01-29', 0), ('2024-01-30', 1), ('2024-02-02', 1), ('2024-02-03', 0),
                              ('2024-02-27', 0), ('2024-02-28', 1), ('2024-03-03', 0)]:
            self.assertEqual(p[day], expected, day)

    def test_vol_managed_real_month_end_only(self):
        r = np.resize(np.array([.05, -.04, .10, -.08]), 42)
        r[29:31] = .01
        df = frame(100*np.cumprod(1+r))
        p = v.vol_managed_positions(df, rv_window=2, history_window=3)
        np.testing.assert_array_equal(p[:30], np.zeros(30))
        np.testing.assert_array_equal(p[30:], np.ones(12))
        self.assertEqual(v.vol_managed_positions(df.iloc[:25], rv_window=2, history_window=3)[-1], 0)

    def test_faber_month_end_only(self):
        p = v.faber_positions(frame(np.arange(100, 142)), window=3)
        np.testing.assert_array_equal(p[:30], np.zeros(30))
        np.testing.assert_array_equal(p[30:], np.ones(12))

    def test_ewmac_vt_holds_sizing_signal_between_flips(self):
        rng = np.random.default_rng(17)
        df = frame(100*np.exp(np.cumsum(rng.normal(.001, .03, 400))))
        sign = v.ewmac_positions(df, fast=4, slow=12)
        p = v.ewmac_vt_positions(df, fast=4, slow=12, rv_window=5)
        for i in range(13, len(p)):
            if sign[i] == sign[i-1]:
                self.assertEqual(p[i], p[i-1])

    def test_supertrend_bands_and_wilder_seed(self):
        df = frame([100, 100, 100, 110, 112, 90, 89])
        np.testing.assert_array_equal(v.supertrend_positions(df, atr_window=2, multiplier=1), [0, 0, 0, 1, 1, 0, 0])

    def test_donchian_stays_in_after_first_break(self):
        np.testing.assert_array_equal(v.donchian_sar_positions(frame([100, 100, 103, 102, 98, 99]), window=2), [0, 0, 1, 1, -1, -1])


class NativeRunner(unittest.TestCase):
    def test_market_next_open_no_terminal_fill(self):
        r = v.run_rule(frame([100, 105, 100], opens=[100, 110, 100]), [1, 0, 1], commission=0, slippage=0)
        self.assertEqual(r['engine'], 'backtrader')
        self.assertEqual(len(r['fills']), 2)
        self.assertEqual(r['fills'][0]['date'], '2023-01-02')
        self.assertEqual(r['fills'][0]['price'], 110)
        self.assertEqual(r['fills'][0]['size'], 10)
        self.assertEqual(r['fills'][0]['signal_time'], '2023-01-01T00:00:00')
        self.assertAlmostEqual(r['daily_equity']['2023-01-03'], 900)
        self.assertAlmostEqual(r['metrics']['return_pct'], -10)
        self.assertEqual(r['metrics']['trades'], 1)
        self.assertAlmostEqual(r['metrics']['maxdd_pct'], 10)

    def test_funding_direction_and_final_day_booked(self):
        df = frame([100]*4)
        funding = {d: .001 for d in df.index}
        for direction in [1, -1]:
            with self.subTest(direction=direction):
                r = v.run_rule(df, np.full(4, direction), commission=0, slippage=0, funding=funding)
                self.assertAlmostEqual(r['metrics']['funding_paid'], direction*6)
                self.assertAlmostEqual(r['daily_equity']['2023-01-04'], 1000-direction*6)
                self.assertAlmostEqual(r['metrics']['return_pct'], -direction*.6)

    def test_intraday_funding_only_first_utc_bar(self):
        df = frame([100]*49, freq='h')
        r = v.run_rule(df, np.ones(49), commission=0, slippage=0,
                       funding={pd.Timestamp('2023-01-01'): .001, pd.Timestamp('2023-01-02'): .001, pd.Timestamp('2023-01-03'): .001})
        self.assertAlmostEqual(r['metrics']['funding_paid'], 6)
        self.assertEqual(len(r['daily_equity']), 3)
        self.assertAlmostEqual(r['daily_equity']['2023-01-03'], 994)

    def test_volrix_has_no_commission(self):
        r = v.run_rule(frame([100]*4), [1, 0, -1, 0], sizing='fixed', **v.COSTS['volrix'])
        self.assertEqual(r['metrics']['commission_paid'], 0)
        self.assertTrue(all(f['commission'] == 0 for f in r['fills']))
        self.assertAlmostEqual(r['fills'][0]['price'], 100.05)

    def test_buy_and_hold_buys_once_and_only_pays_entry_cost_in_both_modes(self):
        df = frame([100, 200, 300, 400])
        for sizing in ('fixed', 'compound'):
            with self.subTest(sizing=sizing):
                r = v.run_rule(df, np.ones(4), sizing=sizing, commission=.001, slippage=0)
                self.assertEqual(len(r['fills']), 1)
                self.assertEqual(r['fills'][0]['size'], 10)
                self.assertAlmostEqual(r['metrics']['commission_paid'], 2)
                self.assertAlmostEqual(r['daily_equity']['2023-01-04'], 2998)

    def test_second_entry_compounds_profit_and_costs_while_fixed_keeps_notional(self):
        df = frame([100, 100, 150, 150, 100, 100, 120])
        targets = [1, 1, 0, 0, 1, 1, 1]
        for sizing, quantity in [('compound', 15), ('fixed', 10)]:
            with self.subTest(sizing=sizing):
                r = v.run_rule(df, targets, sizing=sizing, commission=0, slippage=0)
                self.assertEqual([f['size'] for f in r['fills']], [10, -10, quantity])
                self.assertAlmostEqual(r['daily_equity']['2023-01-07'], 1500+quantity*20)
        # Default sizing includes already paid commissions and funding in equity.
        r = v.run_rule(df, targets, commission=.001, slippage=0,
                       funding={'2023-01-03': .001, '2023-01-04': .001})
        self.assertEqual(r['sizing'], 'compound')
        self.assertAlmostEqual(r['metrics']['funding_paid'], 9)
        self.assertAlmostEqual(r['fills'][2]['size'], (1500-1-1.5-9)/100, places=7)

    def test_compound_bankruptcy_blocks_reversal_and_all_later_entries(self):
        for peak in (200, 250):
            with self.subTest(peak=peak):
                # A constant short target remains untouched after insolvency;
                # the next changed target may only close it, even after recovery.
                df = frame([100, 100, peak, 100, 100, 100, 100])
                r = v.run_rule(df, [-1, -1, -1, 1, 0, 1, 1], commission=0, slippage=0)
                self.assertEqual([f['size'] for f in r['fills']], [-10, 10])
                self.assertEqual([f['date'] for f in r['fills']], ['2023-01-02', '2023-01-05'])
                self.assertTrue(r['metrics']['bankrupt'])
                self.assertEqual(r['entry_dates'], ['2023-01-02'])

    def test_compound_nonpositive_equity_on_flip_only_closes_position(self):
        df = frame([100, 100, 250, 250, 250, 250])
        r = v.run_rule(df, [-1, -1, 1, 0, 1, 1], commission=0, slippage=0)
        self.assertEqual([f['size'] for f in r['fills']], [-10, 10])
        self.assertAlmostEqual(r['daily_equity']['2023-01-06'], -500)
        self.assertTrue(r['metrics']['bankrupt'])

    def test_target_changes_resize_from_signal_close_then_hold_quantity(self):
        df = frame([100, 120, 80, 50, 75, 60, 70, 80])
        r = v.run_rule(df, [1, 1, 1, -.5, -.5, -.5, 0, 0], sizing='fixed', commission=0, slippage=0)
        self.assertEqual([f['size'] for f in r['fills']], [10, -20, 10])
        self.assertEqual([f['date'] for f in r['fills']], ['2023-01-02', '2023-01-05', '2023-01-08'])
        eq = r['daily_equity']
        self.assertAlmostEqual(eq['2023-01-03']-eq['2023-01-02'], 10*(80-120))
        self.assertAlmostEqual(eq['2023-01-06']-eq['2023-01-05'], -10*(60-75))

    def test_ewmac_vt_quantity_is_fixed_between_flips(self):
        df = frame([100, 100, 100, 110, 130, 140, 150, 160, 140, 120, 100, 80, 70, 60, 50, 40])
        p = v.ewmac_vt_positions(df, fast=2, slow=4, rv_window=3)
        rv = df.close.pct_change(fill_method=None).rolling(3).std(ddof=1)*math.sqrt(365)
        long_quantity = min(1, .2/rv.iloc[3])*1000/110
        for sizing in ('fixed', 'compound'):
            with self.subTest(sizing=sizing):
                run = v.run_rule(df, p, sizing=sizing, commission=0, slippage=0)
                self.assertEqual(len(run['fills']), 2)
                self.assertEqual([f['date'] for f in run['fills']], ['2023-01-05', '2023-01-11'])
                capital = 1000 if sizing == 'fixed' else 1000+long_quantity*(120-130)
                short_quantity = -min(1, .2/rv.iloc[9])*capital/120
                self.assertAlmostEqual(run['fills'][0]['size'], long_quantity)
                self.assertAlmostEqual(sum(f['size'] for f in run['fills']), short_quantity)
                eq = run['daily_equity']
                self.assertAlmostEqual(eq['2023-01-08']-eq['2023-01-07'], long_quantity*10, places=6)
                self.assertAlmostEqual(eq['2023-01-14']-eq['2023-01-13'], short_quantity*(-10), places=6)

    def test_analyzers_and_json_serialization(self):
        r = v.run_rule(frame([100, 101, 99, 102]), [1, -1, 0, 0], **v.COSTS['real'])
        self.assertIn('trade_analyzer', r['native'])
        self.assertIn('drawdown', r['native'])
        self.assertIn('time_return', r['native'])
        json.dumps(v.json_safe(r), allow_nan=False)

    def test_half_split_preserves_boundary_return(self):
        r = v.run_rule(frame([100, 100, 110, 120]), np.ones(4), commission=0, slippage=0)
        halves = v._halves(r)
        self.assertAlmostEqual(halves['first']['return_pct'], 0)
        self.assertAlmostEqual(halves['second']['return_pct'], 20)

    def test_nonpositive_capital_is_not_silently_filtered(self):
        stats = v.equity_statistics({'2023-01-01': 1000., '2023-01-02': -100., '2023-01-03': 200.})
        self.assertTrue(stats['bankrupt'])
        self.assertIsNone(stats['sharpe'])
        self.assertGreater(stats['maxdd_pct'], 100)
        part = v.equity_statistics({'2023-01-03': 200.}, initial=-100.)
        self.assertIsNone(part['cagr_pct'])
        self.assertIsNone(part['return_pct'])

    def test_validation_and_missing_files(self):
        with self.assertRaisesRegex(ValueError, 'positions'):
            v.run_rule(frame([100]*3), [1, np.nan, 0], **v.COSTS['real'])
        with self.assertRaisesRegex(ValueError, 'sizing'):
            v.run_rule(frame([100]*3), [1, 1, 1], sizing='invalid', **v.COSTS['real'])
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(ValueError, '缺少本地 5m'):
                v.load_frame(Path(tmp)/'missing.json', '5m')
            with self.assertRaisesRegex(ValueError, '缺少本地'):
                v.run_study(['DONCHIAN_SAR_5M'], root=tmp, progress=lambda x: None)


class Evaluation(unittest.TestCase):
    def test_equal_weight_portfolio_normalizes_at_common_start_and_never_rebalances(self):
        def run(equity):
            return dict(metrics=v.equity_statistics(equity), daily_equity=equity)
        a = run({'2022-12-31': 900., '2023-01-01': 1000., '2023-01-02': -100., '2023-01-03': 200.})
        b = run({'2023-01-01': 1200., '2023-01-02': 1500., '2023-01-03': 1400., '2023-01-04': 1600.})
        runs = {'A': a, 'B': b}
        self.assertTrue(a['metrics']['bankrupt'])
        cross = v.cross_section(runs, runs)
        expected = v.equity_statistics({'2023-01-01': 1000., '2023-01-02': (-100+1250)/2,
                                        '2023-01-03': (200+1400/1200*1000)/2})
        for key in ('portfolio', 'benchmark_portfolio'):
            portfolio = cross[key]
            self.assertEqual(portfolio['initial_capital'], 1000)
            self.assertEqual((portfolio['start'], portfolio['end']), ('2023-01-01', '2023-01-03'))
            self.assertFalse(portfolio['metrics']['bankrupt'])
            for metric in ('sharpe', 'cagr_pct', 'maxdd_pct'):
                self.assertAlmostEqual(portfolio['metrics'][metric], expected[metric])
            self.assertNotIn('daily_equity', portfolio)
            self.assertNotIn('daily_returns', portfolio['metrics'])

    def test_portfolio_cannot_normalize_nonpositive_common_start(self):
        run = dict(metrics=v.equity_statistics({'2023-01-01': 0.}),
                   daily_equity={'2023-01-01': 0.})
        cross = v.cross_section({'A': run}, {'A': run})
        self.assertIsNone(cross['portfolio'])
        self.assertIsNone(cross['benchmark_portfolio'])

    def test_default_in_grid_and_size_limit(self):
        for name, (_, defaults, _, _) in v.RULES.items():
            grid, key, _, _ = v.parameter_grid(name)
            self.assertIn(key, grid, name)
            self.assertEqual(grid[key], defaults, name)
            self.assertLessEqual(len(grid), 125)

    def test_date_matching_is_one_to_one(self):
        r = v._date_overlap(['2023-01-01', '2023-01-03'], ['2023-01-02'])
        self.assertEqual(r['tolerant_match_rate'], 1)
        self.assertEqual(r['tolerant_jaccard'], .5)

    def test_admission_requires_same_coin_and_all_four_conditions(self):
        def row(sharpe, dd, audit):
            return dict(run=dict(metrics=dict(sharpe=sharpe, maxdd_pct=dd)),
                        halves={k: dict(sharpe=.1) for k in ('first', 'second')},
                        benchmark=dict(metrics=dict(maxdd_pct=40)), audit=dict(verdict=audit))
        main = {'BTCUSDT': row(1, 20, 'NO_EDGE'), 'ETHUSDT': row(.4, 20, 'VALIDATED')}
        cross, sweep = dict(positive_sharpe_ratio=.6), dict(verdict='PLATEAU')
        self.assertTrue(v.admission(main, cross, sweep)['reason'].startswith('条件2'))
        main['BTCUSDT']['audit']['verdict'] = 'TRADE_OFF'
        self.assertEqual(v.admission(main, cross, sweep)['verdict'], 'PASS')
        self.assertTrue(v.admission(main, None, None)['reason'].startswith('条件3'))
        self.assertTrue(v.admission(main, cross, None)['reason'].startswith('条件4'))

    def test_cli_only_adds_volrix_parser(self):
        from scripts.backtest.cli import build_parser
        a = build_parser().parse_args(['volrix', '--rules', 'MAX20', '--out', 'x.md', '--json', 'x.json'])
        self.assertEqual(a.rules, 'MAX20')
        self.assertEqual(a.universe_min_years, 3)

    def test_complete_offline_report_on_fixture(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            dates = pd.date_range('2023-01-01', periods=90)
            for symbol in ('BTCUSDT', 'ETHUSDT', 'SOLUSDT'):
                df = frame(100+np.arange(90)*.1+np.sin(np.arange(90)))
                path = root/'factor_mining/crypto/daily'/f'{symbol}.json'
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(json.dumps([[str(d.date()), *row.tolist(), 0] for d, row in df.iterrows()]))
                path = root/'factor_mining/crypto/alt_daily/funding'/f'{symbol}.json'
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(json.dumps([[str(d.date()), .0001] for d in dates]))
            for name in ('MAX20', 'EWMAC_VT'):
                slug = v.RULES[name][3]
                path = root/'volrix/detail'/f'{slug}.json'
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(json.dumps(dict(data=dict(performance=dict(windowStart='2023-01-01', windowEnd='2023-03-31',
                    returnPercent=1., cagrPct=1., maxDrawdownPct=1., sharpeRatio=1., totalTrades=1)))))
                path = root/'volrix/trades'/f'{slug}.json'
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(json.dumps(dict(data=dict(items=[dict(entryDate='2023-01-22', exitDate='2023-01-23')]))))
            result = v.run_study(['MAX20'], symbols=('BTCUSDT', 'ETHUSDT', 'SOLUSDT'),
                                 universe_min_years=.1, root=root, progress=lambda _: None)
            report = v.render_report(result)
            self.assertIn('复现核对', report)
            self.assertIn('PASS/FAIL 汇总', report)
            self.assertIn('MaxDD', report)
            self.assertEqual([r['rule'] for r in result['reproduction']], ['MAX20', 'EWMAC_VT'])
            self.assertTrue(all(r['comparison_basis'] == 'entry_dates' for r in result['reproduction']))
            self.assertTrue(all(r['sizing'] == 'fixed' and r['cost_model'] == 'volrix' for r in result['reproduction']))
            # Source exits at 00:59:59 on exitDate: only the entry date is held.
            self.assertEqual(result['reproduction'][0]['held_overlap']['volrix'], 1)
            self.assertIn('MAX20', v.admission_table(result))
            self.assertEqual(result['rules']['MAX20']['cross_section']['n'], 3)
            self.assertEqual(len(result['rules']['MAX20']['main']['BTCUSDT']['run']['daily_equity']), 90)
            self.assertEqual(len(result['rules']['MAX20']['main']['BTCUSDT']['benchmark']['fills']), 1)
            for row in result['rules']['MAX20']['main'].values():
                for key in ('run', 'benchmark'):
                    self.assertEqual(row[key]['sizing'], 'compound')
                    self.assertEqual(row[key]['commission'], v.COSTS['real']['commission'])
            extra = result['rules']['MAX20']['main']['SOLUSDT']
            for key in ('run', 'benchmark'):
                self.assertNotIn('daily_equity', extra[key])
                self.assertNotIn('fills', extra[key])
                self.assertNotIn('native', extra[key])
                self.assertNotIn('daily_returns', extra[key]['metrics'])
            cross = result['rules']['MAX20']['cross_section']
            for key in ('per_symbol', 'benchmark_per_symbol'):
                self.assertNotIn('daily_returns', cross[key]['SOLUSDT'])
            json.dumps(v.json_safe(result), allow_nan=False)


if __name__ == '__main__':
    unittest.main()
