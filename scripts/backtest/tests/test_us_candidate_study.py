import copy
import json
import math
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest.stocks import Config, run_strategy
from scripts.backtest.stocks_cache import BASE, convert_rows, output_root, rebuild
from scripts.backtest.stocks_data import History, SPY, candidate_basket, candidate_state, load_study
from scripts.backtest.tests.test_stocks import bars, rising, study_of
from scripts.backtest.us_candidate_study import (COST_LOG, VARIANTS, block_difference,
    estimate, forward_labels, market_state, observations, select_filter)


def candidate_fixture():
    # Highest momentum name rolled over >50 days ago but remains above SMA200.
    down = bars(280, lambda i: 100 if i < 150 else
                600 if i < 200 else 600-3*(i-199))
    # A one-day bounce puts close above EMA50 while EMA20 remains below EMA50.
    transition = copy.deepcopy(down)
    transition[252].update(open=510, high=511, low=509, close=510)
    up = bars(280, lambda i: 100+i*.5)
    stocks = {'ABUSDT': down, 'BBUSDT': transition}
    stocks.update({f'U{i:02d}BUSDT': copy.deepcopy(up) for i in range(16)})
    return study_of(stocks, rising(280))


class CacheConversionTest(unittest.TestCase):
    def test_format_validation_dates_duplicates_and_load_study(self):
        rows = [[str(t.date()), 10, 12, 9, 11, 100, 10.5]
                for t in pd.bdate_range('2020-01-01', periods=260)]
        rows += [[rows[0][0], 11, 13, 10, 12, 200, 11],
                 [rows[0][0], 10, 12, -1, 11, 100, 10.5],
                 ['2020-02-30', 10, 12, 9, 11, 100, 10.5],
                 ['2020-01-01', 10, 12, 9, math.nan, 100, 10.5]]
        doc = convert_rows(rows, 'AAPL')
        self.assertEqual(doc['series']['symbol'], 'AAPLBUSDT')
        self.assertEqual(doc['series']['timeframe'], '1d')
        self.assertEqual(doc['series']['source'], 'yahoo(factor_mining)')
        h = History(doc['series']['bars'])
        self.assertEqual((len(h.keys), h.removed, h.bars[0]['close']), (260, 0, 12))
        self.assertEqual(doc['series']['bars'][0]['open_time'], '2020-01-01T00:00:00Z')
        BASE.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=BASE) as folder:
            root = Path(folder)
            source, cache = root/'source', root/'cache'
            source.mkdir()
            for symbol in ('AAPL', 'SPY'):
                (source/(symbol+'.json')).write_text(json.dumps(rows))
            before = (source/'AAPL.json').read_bytes()
            with patch('socket.socket', side_effect=AssertionError('network forbidden')):
                manifest = rebuild(source, cache)
                study = load_study(cache)
            self.assertEqual(set(study.histories), {'AAPLBUSDT', SPY})
            self.assertEqual(manifest['symbols']['AAPL']['removed'], 4)
            self.assertEqual(before, (source/'AAPL.json').read_bytes())
            with self.assertRaises(ValueError):
                rebuild(source, cache, '1h')

    def test_protected_output_and_symlink_are_rejected(self):
        for path in ('/Users/zhangyun/workspace/nofx/data', BASE/'../../escape'):
            with self.assertRaises(ValueError):
                output_root(path)
        BASE.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=BASE) as folder:
            link = Path(folder)/'bad'
            link.symlink_to('/Users/zhangyun/workspace/nofx/data', target_is_directory=True)
            with self.assertRaises(ValueError):
                output_root(link)


class CandidateSignalTest(unittest.TestCase):
    def test_strict_states_seed_and_future_mutation(self):
        up, down, flat = History(rising(300)), History(bars(300, lambda i: 300-i*.2)), History(bars(300))
        self.assertEqual(candidate_state(up, 49), 'up')
        self.assertEqual(candidate_state(down, 49), 'down')
        self.assertEqual(candidate_state(flat, 49), 'transition')
        self.assertEqual(candidate_state(up, 48), 'unavailable')
        original = rising(300)
        changed = copy.deepcopy(original)
        for row in changed[260:]:
            row.update(open=900, high=901, low=899, close=900)
        h1, h2 = History(original), History(changed)
        np.testing.assert_equal(h1.ema20[:260], h2.ema20[:260])
        np.testing.assert_equal(h1.ema50[:260], h2.ema50[:260])
        self.assertEqual([candidate_state(h1, i) for i in range(260)],
                         [candidate_state(h2, i) for i in range(260)])
        self.assertAlmostEqual(h1.ema20[19], pd.Series([b['close'] for b in original[:20]])
                               .ewm(span=20, adjust=False).mean().iloc[-1])

    def test_labels_use_spy_positions_no_fill_and_no_boundary_leak(self):
        dates = pd.to_datetime(['2023-12-27', '2023-12-28', '2023-12-29', '2024-01-02', '2024-01-03'])
        close = pd.DataFrame({'A': [100, 110, 121, 200, 220], 'B': [10, 11, np.nan, 20, 22]}, index=dates)
        labels = forward_labels(close, 2, '2023-01-01', '2023-12-31')
        self.assertAlmostEqual(labels.iloc[0]['A'], math.log(1.21))
        self.assertTrue(labels.iloc[1:].isna().all().all())
        self.assertTrue(labels['B'].isna().all())
        full = forward_labels(close, 2, '2023-01-01', '2025-01-01')
        self.assertTrue(full.tail(2).isna().all().all())
        self.assertAlmostEqual(full.iloc[2]['A'], math.log(220/121))
        with self.assertRaises(ValueError):
            forward_labels(close, 0, '2023-01-01', '2025-01-01')
        self.assertAlmostEqual(COST_LOG, math.log(.999/1.001))

    def test_filtered_pool_and_all_preregistered_native_selections(self):
        study = candidate_fixture()
        t = study.calendar[0]
        self.assertEqual(candidate_state(study.daily['ABUSDT'], 252), 'down')
        self.assertEqual(candidate_state(study.daily['BBUSDT'], 252), 'transition')
        raw = candidate_basket(study, t, 15)
        self.assertEqual(raw[:2], ['ABUSDT', 'BBUSDT'])
        for variant, n, trend_filter in VARIANTS:
            with self.subTest(variant=variant):
                expected = ([f'U{i:02d}BUSDT' for i in range(n)] if trend_filter == 'T2' else
                            ['BBUSDT']+[f'U{i:02d}BUSDT' for i in range(n-1)] if trend_filter == 'T1' else
                            ['ABUSDT', 'BBUSDT']+[f'U{i:02d}BUSDT' for i in range(n-2)])
                self.assertEqual(candidate_basket(study, t, n, trend_filter), expected)
                result = run_strategy(study, Config(max_positions=n, candidate_filter=trend_filter), 'MOM_TOP')
                self.assertEqual([d['symbol'] for d in result['decisions'][0]['targets']], expected)
                self.assertTrue(all(d['weight'] == 1/n for d in result['decisions'][0]['targets']))
                for name in candidate_basket(study, t, n, trend_filter):
                    h = study.daily[name]
                    self.assertGreater(h.bars[252]['close'], h.ind[252].trend)
                    if trend_filter == 'T1':
                        self.assertGreater(h.bars[252]['close'], h.ema50[252])
                    if trend_filter == 'T2':
                        self.assertEqual(candidate_state(h, 252), 'up')

    def test_short_pool_preserves_cash_and_rejects_other_strategies(self):
        study = study_of({'UPBUSDT': rising(280)}, rising(280))
        r = run_strategy(study, Config(max_positions=5, candidate_filter='T2'), 'MOM_TOP')
        self.assertEqual(r['decisions'][0]['targets'], [dict(symbol='UPBUSDT', weight=.2)])
        self.assertLess(r['metrics']['avg_invested_weight'], .25)
        with self.assertRaises(ValueError):
            run_strategy(study, Config(candidate_filter='T1'), 'SPY_BH')
        with self.assertRaises(ValueError):
            Config(timeframe='1h', candidate_filter='T1').validate()

    def test_market_state_is_scale_invariant_and_causal(self):
        close = pd.DataFrame({'A': np.arange(100, 200.), 'B': np.arange(20, 120.)})
        original = market_state(close)
        scaled = market_state(close*pd.Series({'A': 100., 'B': .5}))
        np.testing.assert_allclose(original['basket_index'], scaled['basket_index'])
        changed = close.copy()
        changed.iloc[80:] *= .1
        pd.testing.assert_frame_equal(original.iloc[:80], market_state(changed).iloc[:80])

    def test_pool_missing_endpoint_discards_whole_date(self):
        dates = pd.date_range('2024-01-01', periods=2)
        labels = pd.DataFrame({'A': [.1, .2], 'B': [np.nan, .4]}, index=dates)
        members = pd.DataFrame([(t, s, 'V0') for t in dates for s in ['A', 'B']],
                               columns=['date', 'symbol', 'group'])
        sums, counts, _ = observations(members, labels, pool=True)
        self.assertEqual(counts['V0'].tolist(), [0, 1])
        self.assertAlmostEqual(sums.iloc[1]['V0'], .3)

    def test_selection_never_reads_oos_labels(self):
        dates = pd.date_range('2023-01-01', '2025-02-01', freq='B')
        close = pd.DataFrame({'A': np.exp(np.arange(len(dates))*.001),
                              'B': np.exp(np.arange(len(dates))*.002)}, index=dates)
        pools = pd.DataFrame([(t, 'A' if v == 'V0' else 'B', v)
                              for t in dates for v in ('V0', 'V3', 'V4')],
                             columns=['date', 'symbol', 'group'])
        first = select_filter(None, close, pools)
        close.loc['2025-01-01':, 'B'] *= 1000
        self.assertEqual(first, select_filter(None, close, pools))
        self.assertEqual(first['selected'], 'T1')


class InferenceTest(unittest.TestCase):
    def test_cluster_se_matches_two_independent_date_means(self):
        result = estimate([2., 6.], [2., 2.])
        self.assertAlmostEqual(result['mean'], 2.)
        self.assertAlmostEqual(result['se'], 1.)
        contrast = estimate([[2., 0.], [6., 4.]], [[2., 2.], [2., 2.]], [1, -1])
        self.assertAlmostEqual(contrast['mean'], 1.)
        self.assertAlmostEqual(contrast['se'], 0.)
        self.assertEqual(contrast['ci'], [1., 1.])

    def test_block_paired_constant_and_regime_mask(self):
        x = np.array([.01, -.02]*30)
        result = block_difference(x, x > 0)
        self.assertEqual(result['dates'], 30)
        self.assertAlmostEqual(result['mean'], .01)
        np.testing.assert_allclose(result['ci'], [.01, .01])
        zero = block_difference(np.zeros(30))
        self.assertEqual(zero['ci'], [0., 0.])


if __name__ == '__main__':
    unittest.main()
