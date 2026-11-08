"""Synthetic hourly paths, native fills, and frozen daily-path regression."""
import hashlib
import json
import unittest

import numpy as np
import pandas as pd

from scripts.backtest import quantjourney as q
from scripts.factor_mining.store import finite


def hourly_frame(days=3, overrides=None):
    index = pd.date_range('2024-01-01', periods=days * 24, freq='h')
    frame = pd.DataFrame(100., index=index, columns=['open', 'high', 'low', 'close'])
    for offset, bar in (overrides or {}).items():
        frame.iloc[offset] = bar
    return frame


def daily_frame(hourly):
    frame = hourly.resample('D').agg(dict(open='first', high='max', low='min', close='last'))
    frame['volume'] = 1000.
    return frame


def execute(hourly, *, weights=None, giveback=False, slippage=0., exit_path=True):
    daily = daily_frame(hourly)
    targets = pd.DataFrame({'A': [-.5] * len(daily) if weights is None else weights}, index=daily.index)
    return q.run_weights({'A': daily}, targets, market='crypto', commission=0., slippage=slippage,
                         policy=q.RebalancePolicy(None, rebalance_on_signal_change=True), keep_fills=True,
                         exit_policy=q.PerNameExitPolicy(long_stop=.2, short_stop=.2,
                                                        short_giveback=(.2, .5) if giveback else None),
                         exit_path={'A': hourly} if exit_path else None)


class HourlyExits(unittest.TestCase):
    def test_short_stop_native_slippage(self):
        run = execute(hourly_frame(overrides={25: (100, 130, 99, 110)}), slippage=.0003)
        event, = run['exit_events']
        entry = run['fills'][0]['price']
        self.assertEqual(event['kind'], 'stop_loss')
        self.assertAlmostEqual(event['trigger'], entry * 1.2)
        self.assertAlmostEqual(event['price'], entry * 1.2 * 1.0003)
        self.assertEqual(event['hour'], '2024-01-02T01:00:00')
        self.assertEqual(event['quantity_after'], 0.)

    def test_short_gap_uses_hour_open(self):
        run = execute(hourly_frame(overrides={25: (130, 135, 125, 132)}))
        event, = run['exit_events']
        self.assertEqual(event['trigger'], 120.)
        self.assertEqual(event['reference_price'], 130.)
        self.assertEqual(event['price'], 130.)

    def test_giveback_arms_next_hour_and_uses_prior_low(self):
        h = hourly_frame(overrides={24: (100, 110, 70, 75), 25: (75, 90, 74, 88)})
        event, = execute(h, giveback=True)['exit_events']
        self.assertEqual(event['kind'], 'giveback_stop')
        self.assertEqual(event['hour'], '2024-01-02T01:00:00')
        self.assertEqual(event['m'], 70.)
        self.assertEqual(event['E'], 100.)
        self.assertAlmostEqual(event['g_star'], .3)
        self.assertEqual(event['price'], 70 + .5 * (100 - 70))

    def test_new_low_ratchets_only_for_following_hour(self):
        h = hourly_frame(overrides={24: (100, 110, 75, 78), 25: (78, 85, 60, 70),
                                   26: (70, 82, 68, 80)})
        event, = execute(h, giveback=True)['exit_events']
        self.assertEqual(event['hour'], '2024-01-02T02:00:00')
        self.assertEqual(event['m'], 60.)
        self.assertEqual(event['price'], 80.)

    def test_giveback_gap_and_slippage(self):
        h = hourly_frame(overrides={24: (100, 110, 70, 75), 25: (95, 98, 90, 95)})
        event, = execute(h, giveback=True, slippage=.0003)['exit_events']
        self.assertEqual(event['kind'], 'giveback_stop')
        self.assertAlmostEqual(event['price'], 95 * 1.0003)

    def test_target_epoch_resets_low_and_activation(self):
        h = hourly_frame()
        h.iloc[24:48] = (75, 79, 70, 75)
        h.iloc[24] = (100, 110, 70, 75)
        h.iloc[48:] = (94, 98, 90, 94)
        # Day-two close changes the target, executing day three at the open.
        run = execute(h, weights=[-.5, -.4, -.4], giveback=True)
        self.assertEqual(run['exit_events'], [])
        self.assertEqual(run['metrics']['rebalance_stats']['signal'], 1)
        unreset = execute(h, giveback=True)
        self.assertEqual(unreset['exit_events'][0]['kind'], 'giveback_stop')

    def test_long_ignores_giveback(self):
        h = hourly_frame(overrides={24: (100, 160, 90, 110), 25: (110, 115, 85, 90)})
        self.assertEqual(execute(h, weights=[.5] * 3, giveback=True)['exit_events'], [])

    def test_incomplete_day_falls_back_to_daily_stop(self):
        h = hourly_frame(overrides={25: (100, 130, 99, 110)}).drop(pd.Timestamp('2024-01-02T23:00'))
        run = execute(h, giveback=True)
        event, = run['exit_events']
        self.assertEqual(event['kind'], 'stop_loss')
        self.assertEqual(event['price'], 120.)
        self.assertNotIn('hour', event)
        self.assertEqual(run['metrics']['hourly_fallback_days'], 1)
        self.assertEqual(run['metrics']['hourly_days'], 0)

    def test_complete_day_counts_and_close_exposure(self):
        h = hourly_frame()
        f = daily_frame(h)
        targets = pd.DataFrame(dict(A=.5, B=-.5), index=f.index)
        run = q.run_weights(dict(A=f, B=f), targets, market='crypto', commission=0., slippage=0.,
                            policy=q.RebalancePolicy(None, rebalance_on_signal_change=True),
                            exit_policy=q.PerNameExitPolicy(long_stop=.2, short_stop=.2), exit_path=dict(A=h, B=h))
        self.assertEqual(run['daily_exposure']['2024-01-01'], dict(long=0., short=0.))
        self.assertEqual(run['daily_exposure']['2024-01-02'], dict(long=.5, short=.5))
        self.assertEqual(run['metrics']['hourly_days'], 4)
        self.assertEqual(run['metrics']['hourly_fallback_days'], 0)
        self.assertEqual(run['metrics']['exit_trigger_counts'], dict(stop_loss=0, giveback_stop=0))

    def test_daily_path_matches_before_change_bytes(self):
        f = pd.DataFrame([(100, 101, 99, 100), (100, 105, 98, 103), (103, 104, 98, 99),
                          (99, 103, 70, 80), (80, 104, 79, 102), (102, 104, 100, 103)],
                         columns=['open', 'high', 'low', 'close'],
                         index=pd.date_range('2024-01-01', periods=6), dtype=float)
        f['volume'] = 1000.
        targets = pd.DataFrame({'A': [.5, .5, -.4, -.4, 0, 0]}, index=f.index)
        kwargs = dict(market='crypto', keep_fills=True,
                      funding={'A': {d.date(): .001 for d in f.index}},
                      policy=q.RebalancePolicy(None, rebalance_on_signal_change=True),
                      exit_policy=q.PerNameExitPolicy(long_stop=.2, short_stop=.2))
        implicit = finite(q.run_weights({'A': f}, targets, **kwargs))
        explicit = finite(q.run_weights({'A': f}, targets, exit_path=None, **kwargs))
        self.assertEqual(implicit, explicit)
        legacy = {k: v for k, v in explicit.items() if k != 'daily_exposure'}
        # Captured from the unmodified engine before this extension.
        self.assertEqual(hashlib.sha256(json.dumps(legacy, sort_keys=True).encode()).hexdigest(),
                         'a2665fa8003b69ab111771da52e9f22a45f1eafe75d789640a538300fd1327b1')

    def test_illegal_policy_combinations(self):
        h = hourly_frame()
        f = daily_frame(h)
        t = pd.DataFrame({'A': -.5}, index=f.index)
        with self.assertRaisesRegex(ValueError, 'requires exit_path'):
            q.run_weights({'A': f}, t, exit_policy=q.PerNameExitPolicy(short_giveback=(.2, .5)))
        for policy in (q.PerNameExitPolicy(trailing_activation=.2, trailing_distance=.1),
                       q.PerNameExitPolicy(take_profit=((.2, .5),)),
                       q.PerNameExitPolicy(breakeven_after_tier=True),
                       q.PerNameExitPolicy(volatility_stop=True)):
            with self.subTest(policy=policy), self.assertRaisesRegex(ValueError, 'supports only'):
                q.run_weights({'A': f}, t, exit_policy=policy, exit_path={'A': h})
        for pair in ((0., .5), (.2, 1.), (np.nan, .5), (.2,)):
            with self.subTest(pair=pair), self.assertRaises(ValueError):
                q.PerNameExitPolicy(short_giveback=pair)


if __name__ == '__main__':
    unittest.main()
