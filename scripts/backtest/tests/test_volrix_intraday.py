"""Synthetic-only acceptance tests; no local market files or network required."""
from contextlib import redirect_stderr
from io import StringIO
import math
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest import volrix_intraday as vi


def bars(n=12, start='2024-01-01 12:00', minutes=5):
    return pd.DataFrame({'open': 100., 'high': 100.5, 'low': 99.5, 'close': 100., 'volume': 10.},
                        index=pd.date_range(start, periods=n, freq=f'{minutes}min', tz='UTC'))


def proposals(frame, points):
    s = pd.DataFrame({'side': 0, 'trigger': np.nan, 'stop': np.nan, 'atr': 10.}, index=frame.index)
    for i, side, trigger, stop in points:
        s.iloc[i] = [side, trigger, stop, 10.]
    return s


class IndicatorsTest(unittest.TestCase):
    def test_indicator_and_signal_prefix_causality(self):
        rng = np.random.default_rng(17)
        f = bars(950, '2024-01-01')
        c = 100+np.cumsum(rng.normal(0, 0.45, len(f)))
        f['open'] = np.r_[100, c[:-1]]
        f['close'] = c
        f['high'] = np.maximum(f.open, f.close)+rng.uniform(.05, .7, len(f))
        f['low'] = np.minimum(f.open, f.close)-rng.uniform(.05, .7, len(f))
        f['volume'] = rng.uniform(1, 40, len(f))
        full = vi.build_features(f)
        signals = {r: vi.build_setups(full, r, .1) for r in vi.RULES}
        for cut in (13, 27, 53, 100, 201, 288, 289, 400, 700, 949):
            prefix = vi.build_features(f.iloc[:cut])
            pd.testing.assert_frame_equal(full.iloc[:cut], prefix)
            for rule in vi.RULES:
                pd.testing.assert_frame_equal(signals[rule].iloc[:cut], vi.build_setups(prefix, rule, .1))

    def test_vwap_resets_every_utc_day(self):
        f = bars(4, '2024-01-01 23:50')
        f.loc[:, ['open', 'high', 'low', 'close']] = np.array([100, 110, 200, 220])[:, None]
        f['volume'] = [1, 3, 2, 1]
        got = vi.build_features(f).vwap.to_numpy()
        np.testing.assert_allclose(got, [100, 107.5, 200, 620/3])

    def test_sma_seeded_ema_and_wilder(self):
        np.testing.assert_allclose(vi.smooth([1, 2, 3, 4, 5], 3), [np.nan, np.nan, 2, 3, 4], equal_nan=True)
        np.testing.assert_allclose(vi.smooth([1, 2, 3, 4, 5], 3, wilder=True),
                                   [np.nan, np.nan, 2, 8/3, 31/9], equal_nan=True)
        f = bars(20)
        f['close'] = np.arange(100., 120.)
        f['open'] = f.close
        f['high'], f['low'] = f.close+1, f.close-1
        features = vi.build_features(f)
        self.assertEqual(features.atr14.iloc[13], 2.)
        self.assertEqual(features.rsi3.iloc[3], 100.)

    def test_visible_cloud_is_past_calculation(self):
        f = bars(150)
        f['high'] += np.arange(150)*.1
        f['low'] += np.arange(150)*.05
        x = vi.build_features(f)
        t = 120
        j = t-26
        expected = (f.high.iloc[j-51:j+1].max()+f.low.iloc[j-51:j+1].min())/2
        self.assertAlmostEqual(x.cloud_b.iloc[t], expected)
        self.assertTrue(math.isnan(x.cloud_b.iloc[76]))

    def test_resample_uses_only_three_complete_aligned_bars(self):
        f = bars(8, '2024-01-01 00:05')
        r = vi.resample_15m(f)
        self.assertEqual(list(r.index.strftime('%H:%M')), ['00:15', '00:30'])
        self.assertEqual(r.volume.tolist(), [30., 30.])
        short = vi.resample_15m(f.iloc[:-1])
        self.assertEqual(list(short.index.strftime('%H:%M')), ['00:15'])

    def test_holy_grail_requires_prior_arming_and_expires(self):
        f = vi.build_features(bars(14, minutes=15))
        f['sma20'] = np.arange(len(f))*.1+100
        f['adx14'] = [29, 31]+[30]*12
        f['low'] = 102.
        f.loc[f.index[[1, 2, 12]], 'low'] = 99.
        s = vi.build_setups(f, 'HOLY_GRAIL', .1)
        self.assertEqual(s.side.iloc[1], 0)  # cannot touch and arm simultaneously
        self.assertEqual(s.side.iloc[2], 1)
        self.assertEqual(s.trigger.iloc[2], f.high.iloc[2]+.1)
        f['low'] = 102.
        f.loc[f.index[12], 'low'] = 99.
        self.assertEqual(vi.build_setups(f, 'HOLY_GRAIL', .1).side.iloc[12], 0)

    def test_nr7_strict_width_and_each_bars_ema(self):
        f = vi.build_features(bars(40))
        f['ema20'] = 90.
        f['high'], f['low'] = 102., 98.
        f.loc[f.index[-1], ['high', 'low']] = [101., 99.]
        self.assertEqual(vi.build_setups(f, 'NR7', .1).side.iloc[-1], 1)
        f.loc[f.index[-2], ['high', 'low']] = [101., 99.]
        self.assertEqual(vi.build_setups(f, 'NR7', .1).side.iloc[-1], 0)

    def test_rsi_two_candle_mirror_and_tick(self):
        f = vi.build_features(bars(60, minutes=15))
        f['ema50'], f['rsi3'] = 90., 90.
        f.loc[f.index[-2], ['open', 'high', 'low', 'close']] = [100, 102, 99, 101]
        f.loc[f.index[-1], ['open', 'high', 'low', 'close']] = [101, 104, 100, 103]
        s = vi.build_setups(f, 'RSI3_2C', .1)
        self.assertEqual(s.side.iloc[-1], 1)
        self.assertEqual(s.trigger.iloc[-1], 104.1)
        self.assertEqual(s.stop.iloc[-1], 98.9)
        f['ema50'], f['rsi3'] = 110., 10.
        f.loc[f.index[-2], ['open', 'high', 'low', 'close']] = [103, 104, 101, 102]
        f.loc[f.index[-1], ['open', 'high', 'low', 'close']] = [102, 103, 99, 100]
        s = vi.build_setups(f, 'RSI3_2C', .01)
        self.assertEqual(s.side.iloc[-1], -1)
        self.assertAlmostEqual(s.trigger.iloc[-1], 98.99)
        self.assertAlmostEqual(s.stop.iloc[-1], 104.01)


class OrdersTest(unittest.TestCase):
    def run_bt(self, f, rule, points, funding=None, funded=False, **kw):
        with patch.dict(vi.COSTS, {'test': (0., 0., funded)}):
            return vi.run_backtest(f, rule, cost='test', funding=funding,
                                   setups=proposals(f, points), trace=True, **kw)

    def test_stop_entry_canceled_after_three_bars(self):
        f = bars(8)
        f.loc[f.index[4], 'high'] = 112.
        r = self.run_bt(f, 'NR7', [(0, 1, 110., 95.)])
        self.assertEqual(r['trades'], [])
        canceled = [o for o in r['orders'] if o['role'] == 'entry' and o['status'] == 'Canceled']
        self.assertEqual(len(canceled), 1)
        self.assertEqual(canceled[0]['bar_open_ms'], f.index[3].value//1_000_000)

    def test_third_bar_can_fill_and_signal_bar_cannot(self):
        f = bars(8)
        f.loc[f.index[0], 'high'] = 120.
        f.loc[f.index[3], 'high'] = 111.
        r = self.run_bt(f, 'NR7', [(0, 1, 110., 95.)])
        self.assertEqual(len(r['trades']), 1)
        self.assertEqual(r['trades'][0]['entry_ms'], f.index[3].value//1_000_000)
        self.assertEqual(r['trades'][0]['entry_price'], 110.)
        self.assertEqual(r['trades'][0]['target'], 140.)

    def test_same_bar_stop_wins_over_target_long_and_short(self):
        for side in (1, -1):
            with self.subTest(side=side):
                f = bars(6)
                trigger, stop = (101., 95.) if side > 0 else (99., 105.)
                f.loc[f.index[1], ['high', 'low']] = [102., 98.]
                f.loc[f.index[2], ['high', 'low']] = [120., 80.]
                r = self.run_bt(f, 'NR7', [(0, side, trigger, stop)])
                t = r['trades'][0]
                self.assertEqual(t['exit_reason'], 'sl')
                self.assertEqual(t['exit_price'], stop)
                self.assertLess(t['gross_pnl'], 0)
                self.assertEqual(r['reconciliation']['native_closed_trades'], 1)
                self.assertAlmostEqual(r['metrics']['net_pnl'], t['net_pnl'], places=8)
                self.assertFalse(r['metrics']['open_trades'])

    def test_entry_bar_stop_uses_native_trade_and_commission(self):
        f = bars(4)
        f.loc[f.index[1], ['high', 'low']] = [120., 80.]
        with patch.dict(vi.COSTS, {'test': (.0005, 0., False)}):
            r = vi.run_backtest(f, 'NR7', cost='test', setups=proposals(f, [(0, 1, 101., 95.)]))
        t = r['trades'][0]
        self.assertEqual(t['entry_ms'], t['exit_ms'])
        self.assertEqual(t['exit_price'], 95.)
        self.assertAlmostEqual(t['commission'], (101+95)*(.0005*1000/101))
        self.assertAlmostEqual(r['reconciliation']['broker_net_pnl'], t['net_pnl'], places=8)
        self.assertEqual(r['native']['TradeAnalyzer']['total']['closed'], 1)

    def test_limit_requires_one_tick_through_and_gap_is_native(self):
        f = bars(5)
        f.loc[f.index[1], 'high'] = 102.
        f.loc[f.index[2], 'high'] = 113.  # target 113 touched, not through
        f.loc[f.index[3], ['open', 'high', 'low', 'close']] = [114., 115., 113.5, 114.]
        r = self.run_bt(f, 'NR7', [(0, 1, 101., 95.)])
        t = r['trades'][0]
        self.assertEqual(t['exit_ms'], f.index[3].value//1_000_000)
        self.assertAlmostEqual(t['exit_price'], 114.)
        self.assertEqual(t['exit_reason'], 'tp')

    def test_session_market_flatten_next_day_open(self):
        f = bars(4, '2024-01-01 23:50')
        r = self.run_bt(f, 'VWAP_RECLAIM', [(0, 1, np.nan, np.nan)])
        t = r['trades'][0]
        self.assertEqual(t['entry_ms'], f.index[1].value//1_000_000)
        self.assertEqual(t['exit_ms'], f.index[2].value//1_000_000)
        self.assertEqual(t['exit_reason'], 'session')
        self.assertEqual(r['metrics']['open_trades'], 0)

    def test_session_cancels_pending_and_never_enters_at_last_close(self):
        f = bars(4, '2024-01-01 23:50')
        f.loc[f.index[2], 'high'] = 120.
        r = self.run_bt(f, 'NR7', [(0, 1, 110., 95.), (1, 1, 110., 95.)])
        self.assertEqual(r['trades'], [])

    def test_pdh_pdl_each_direction_once_per_day(self):
        f = bars(294, '2024-01-01')
        f.loc[f.index[2], 'low'] = 94.
        f.loc[f.index[6], 'high'] = 106.
        points = [(0, 1, np.nan, 95), (3, 1, np.nan, 95), (4, -1, np.nan, 105),
                  (7, -1, np.nan, 105), (288, 1, np.nan, 95)]
        r = self.run_bt(f, 'PDH_PDL', points)
        self.assertEqual([t['side'] for t in r['trades']], [1, -1, 1])
        days = [pd.Timestamp(t['entry_ms'], unit='ms', tz='UTC').date().isoformat() for t in r['trades']]
        self.assertEqual(days, ['2024-01-01', '2024-01-01', '2024-01-02'])

    def test_trailing_stop_only_improves_and_is_effective_next_bar(self):
        for side in (1, -1):
            stop = 90. if side > 0 else 110.
            for peak in ([100, 101.25, 101, 103, 99] if side > 0 else [100, 98.75, 99, 97, 101]):
                new = vi.trailing_stop(100., 90. if side > 0 else 110., stop, peak, side)
                self.assertGreaterEqual(side*(new-stop), 0)
                stop = new
        f = bars(5)
        f.loc[f.index[1], ['high', 'low']] = [103.5, 90.5]
        f.loc[f.index[2], ['high', 'low']] = [102., 93.]
        r = self.run_bt(f, 'PDH_PDL', [(0, 1, np.nan, 90.)])
        t = r['trades'][0]
        self.assertEqual(t['exit_ms'], f.index[2].value//1_000_000)
        self.assertAlmostEqual(t['exit_price'], 93.5)
        self.assertAlmostEqual(r['stop_updates'][0]['stop'], 93.5)

    def test_funding_directions_and_native_cash_reconciliation(self):
        for side in (1, -1):
            for rate in (.0001, -.0001):
                with self.subTest(side=side, rate=rate):
                    f = bars(4, '2024-01-01 23:50')
                    r = self.run_bt(f, 'VWAP_RECLAIM', [(0, side, np.nan, np.nan)],
                                    funding={'2024-01-02': rate}, funded=True)
                    flow = -side*rate*1000
                    self.assertEqual(len(r['funding_events']), 1)
                    self.assertAlmostEqual(r['funding_events'][0]['cashflow'], flow)
                    self.assertAlmostEqual(r['metrics']['funding_total'], -flow)
                    self.assertAlmostEqual(r['metrics']['net_pnl'], flow, places=8)
                    self.assertAlmostEqual(r['trades'][0]['net_pnl'], flow, places=8)
                    self.assertAlmostEqual(r['native']['TradeAnalyzer']['pnl']['net']['total'], 0.)

    def test_entry_exactly_at_settlement_is_not_charged(self):
        f = bars(3, '2024-01-01 07:55')
        r = self.run_bt(f, 'VWAP_RECLAIM', [(0, 1, np.nan, np.nan)], funding={}, funded=True)
        self.assertEqual(r['funding_events'], [])

    def test_missing_funding_is_not_silently_zero(self):
        f = bars(4, '2024-01-01 23:50')
        with self.assertRaisesRegex(ValueError, '缺少 .*资金费率'):
            self.run_bt(f, 'VWAP_RECLAIM', [(0, 1, np.nan, np.nan)], funded=True)

    def test_rsi_max_two_actual_entries_per_session(self):
        f = bars(12, minutes=15)
        f.loc[f.index[[1, 4, 7]], ['high', 'low']] = [120., 80.]
        points = [(i, 1, 101., 95.) for i in (0, 3, 6)]
        r = self.run_bt(f, 'RSI3_2C', points)
        self.assertEqual(r['metrics']['trades'], 2)
        self.assertEqual(r['metrics']['entries'], 2)

    def test_ichi_exit_is_next_open_on_completed_close(self):
        f = vi.build_features(bars(5))
        f['supertrend'] = [90., 90., 101., 90., 90.]
        s = proposals(f, [(0, 1, np.nan, np.nan)])
        with patch.dict(vi.COSTS, {'test': (0., 0., False)}):
            r = vi.run_backtest(f, 'ICHI_ST', cost='test', features=f, setups=s)
        t = r['trades'][0]
        self.assertEqual(t['exit_ms'], f.index[3].value//1_000_000)
        self.assertEqual(t['exit_reason'], 'supertrend')

    def test_all_funding_boundaries_use_existing_position(self):
        f = bars(104, '2024-01-01 07:50')
        r = self.run_bt(f, 'VWAP_RECLAIM', [(0, 1, np.nan, np.nan)],
                        funding={'2024-01-01': .0001}, funded=True)
        hours = [pd.Timestamp(e['time_ms'], unit='ms', tz='UTC').hour for e in r['funding_events']]
        self.assertEqual(hours, [8, 16])
        self.assertAlmostEqual(r['metrics']['funding_total'], .2)
        self.assertAlmostEqual(r['metrics']['net_pnl'], -.2, places=8)

    def test_vwap_sl_target_use_signal_atr_and_actual_entry(self):
        f = bars(4)
        f.loc[f.index[1], ['open', 'high', 'low', 'close']] = [105., 106., 104., 105.]
        s = proposals(f, [(0, 1, np.nan, np.nan)])
        s['atr'] = 2.
        with patch.dict(vi.COSTS, {'test': (0., 0., False)}):
            r = vi.run_backtest(f, 'VWAP_RECLAIM', cost='test', setups=s)
        t = r['trades'][0]
        self.assertEqual(t['entry_price'], 105.)
        self.assertEqual(t['initial_stop'], 102.)
        self.assertEqual(t['target'], 109.)
        self.assertEqual(t['exit_price'], 100.)  # native adverse opening gap
        self.assertEqual(t['actual_entry_notional'], 1050.)

    def test_no_fake_end_of_data_liquidation(self):
        r = self.run_bt(bars(3), 'VWAP_RECLAIM', [(0, 1, np.nan, np.nan)])
        self.assertEqual(r['metrics']['open_trades'], 1)
        self.assertNotIn('exit_ms', r['trades'][0])


class ReportingTest(unittest.TestCase):
    def test_missing_local_data_is_explicit(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(FileNotFoundError, '缺少本地 5m 数据'):
                vi.load_bars(Path(tmp)/'absent.json')

    def test_cli_missing_data_exits_explicitly(self):
        with tempfile.TemporaryDirectory() as tmp, redirect_stderr(StringIO()) as err:
            self.assertEqual(vi.main(['--data-root', tmp]), 2)
            self.assertIn('缺少本地 5m 数据', err.getvalue())

    def test_admission_uses_one_coin_for_all_conditions(self):
        def result(symbol, sharpe, half, bps, ret=10.):
            return {'rule': 'NR7', 'cost': 'real', 'symbol': symbol,
                    'metrics': {'sharpe': sharpe, 'first_half': {'sharpe': half},
                                'second_half': {'sharpe': half}, 'avg_net_bps': bps, 'return_pct': ret}}
        a = result('BTCUSDT', .9, -.1, 6.)
        b = result('ETHUSDT', .7, .1, 6.)
        gate = vi.admission([a, b], ['NR7'])['NR7']
        self.assertEqual(gate['status'], 'FAIL')
        self.assertEqual(gate['first_failed_condition'], vi.ADMISSION[1])
        a['metrics']['first_half']['sharpe'] = .1
        a['metrics']['second_half']['sharpe'] = .1
        a['metrics']['avg_net_bps'] = 4.99
        self.assertEqual(vi.admission([a, b], ['NR7'])['NR7']['first_failed_condition'], vi.ADMISSION[2])
        a['metrics']['avg_net_bps'] = 5.
        self.assertEqual(vi.admission([a, b], ['NR7'])['NR7']['status'], 'PASS')
        b['metrics']['return_pct'] = 0.
        self.assertEqual(vi.admission([a, b], ['NR7'])['NR7']['first_failed_condition'], vi.ADMISSION[3])

    def test_entry_matching_is_one_to_one(self):
        source = [{'entryTime': '2024-01-01 12:00:00', 'transactionType': 'buy'}]
        ms = pd.Timestamp(source[0]['entryTime'], tz='UTC').value//1_000_000
        local = [{'entry_ms': ms, 'side': 1}, {'entry_ms': ms+300_000, 'side': 1}]
        m = vi.match_entries(local, source)
        self.assertEqual(m['matched'], 1)
        self.assertEqual(m['local_match_pct'], 50.)
        self.assertEqual(m['source_match_pct'], 100.)

    def test_metrics_fixed_capital_and_date_halves(self):
        daily = [{'date': f'2024-01-0{i+1}', 'pnl': x} for i, x in enumerate([10., -5., 20., -2.])]
        m = vi.metrics(daily, [], 8., 3., .1)
        self.assertAlmostEqual(m['return_pct'], 2.3)
        self.assertAlmostEqual(m['max_drawdown_pct'], .8)
        self.assertAlmostEqual(m['first_half']['return_pct'], .5)
        self.assertAlmostEqual(m['second_half']['return_pct'], 1.8)
        self.assertEqual(m['midpoint'], '2024-01-02T12:00:00')
        daily[0]['pnl'] = -2000
        self.assertIsNone(vi.metrics(daily, [], 8., 3., .1)['cagr_pct'])


if __name__ == '__main__':
    unittest.main()
