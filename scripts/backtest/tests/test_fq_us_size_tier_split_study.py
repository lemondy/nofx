"""Offline unit tests for fq_us_size_tier_split_study."""
from __future__ import annotations

import numpy as np
import pandas as pd

from scripts.backtest import fq_us_size_tier_split_study as s


def test_causal_adv20_no_lookahead():
    dates = pd.bdate_range('2025-01-01', periods=30)
    cols = ['MEGA', 'MID']
    # MEGA: 100 * 6_000_000 = $600M; MID: 100 * 1_000_000 = $100M, then spikes on day 25
    frames = {}
    for sym, v0 in (('MEGA', 6_000_000.0), ('MID', 1_000_000.0)):
        vol = np.full(30, v0)
        if sym == 'MID':
            vol[25:] = 20_000_000.0
        frames[sym] = pd.DataFrame(
            {
                'open': 100.0,
                'high': 101.0,
                'low': 99.0,
                'close': 100.0,
                'volume': vol,
            },
            index=dates,
        )
    adv = s.compute_causal_adv20(frames, cols, dates)
    assert adv.loc[dates[24], 'MEGA'] >= 500_000_000.0
    assert adv.loc[dates[24], 'MID'] == 100_000_000.0  # Future spike on day 25 never leaks!
    assert pd.isna(adv.loc[dates[10], 'MEGA'])  # < 20 bars returns NaN


def test_active_frames_matches_full_frames():
    dates = pd.bdate_range('2025-01-01', periods=6)
    frames = {}
    for idx, sym in enumerate(('AAA', 'BBB', 'CCC')):
        px = 100.0 + idx * 10 + np.arange(6, dtype=float)
        frames[sym] = pd.DataFrame(
            {'open': px, 'high': px + 1, 'low': px - 1, 'close': px, 'volume': 1_000_000.0},
            index=dates,
        )
    decisions = [
        {'date': str(dates[0].date()), 'long': {'AAA': 1.0}, 'short': {'BBB': -1.0}},
        {'date': str(dates[4].date()), 'long': {'AAA': 1.0}, 'short': {'BBB': -1.0}},
    ]
    full = s.run_ls_arm(frames, dates, decisions, full_frames_for_baseline=True)
    trimmed = s.run_ls_arm(frames, dates, decisions, full_frames_for_baseline=False)
    assert full['equity_curve'] == trimmed['equity_curve']


def test_combine_dual_sleeve_preserves_capital_anchor():
    dates = ['2025-01-01', '2025-01-02', '2025-01-03']
    u1 = {'equity_curve': [{'date': dates[0], 'equity': 100_000.0}, {'date': dates[1], 'equity': 110_000.0}, {'date': dates[2], 'equity': 121_000.0}]}
    u2 = {'equity_curve': [{'date': dates[0], 'equity': 100_000.0}, {'date': dates[1], 'equity': 90_000.0}, {'date': dates[2], 'equity': 99_000.0}]}
    comb = s.combine_dual_sleeve(u1, u2, [{'date': dates[0]}, {'date': dates[2]}])
    assert np.isclose(comb['equity_curve'][0]['equity'], 100_000.0)
    assert np.isclose(comb['equity_curve'][1]['equity'], 100_000.0)
    assert np.isclose(comb['equity_curve'][2]['equity'], 110_000.0)


def test_rebal_sessions_matches_full_daily_period_table():
    dates = pd.bdate_range('2025-01-01', periods=10)
    frames = {}
    for idx, sym in enumerate(('AAA', 'BBB')):
        px = 100.0 + idx * 10 + np.arange(10, dtype=float)
        frames[sym] = pd.DataFrame(
            {'open': px, 'high': px + 1, 'low': px - 1, 'close': px, 'volume': 1_000_000.0},
            index=dates,
        )
    decisions = [
        {'date': str(dates[0].date()), 'long': {'AAA': 1.0}, 'short': {'BBB': -1.0}},
        {'date': str(dates[4].date()), 'long': {'AAA': 1.0}, 'short': {'BBB': -1.0}},
        {'date': str(dates[9].date()), 'long': {'AAA': 1.0}, 'short': {'BBB': -1.0}},
    ]
    full = s.run_ls_arm(frames, dates, decisions)
    rebal_sessions = pd.DatetimeIndex([pd.Timestamp(d['date']) for d in decisions])
    fast = s.run_ls_arm(frames, rebal_sessions, decisions)
    for a, b in zip(full['period_table'], fast['period_table'], strict=True):
        assert np.isclose(a['net_return'], b['net_return'], atol=1e-12)


def test_prereg_sha_matches():
    assert len(s.verify_prereg()) > 100
