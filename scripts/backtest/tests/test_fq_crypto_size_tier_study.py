"""Offline unit tests for fq_crypto_size_tier_study."""
from __future__ import annotations

import numpy as np
import pandas as pd

from scripts.factor_mining import panel
from scripts.backtest import fq_crypto_size_tier_study as s


def _make_panels():
    dates = pd.date_range('2025-01-01', periods=30, freq='D')
    cols = ['BTCUSDT', 'ETHUSDT', 'SMALL1', 'SMALL2']
    close = pd.DataFrame(100.0, index=dates, columns=cols)
    vol = pd.DataFrame({
        'BTCUSDT': 2_000_000.0,   # ADV = $200M -> mega_large
        'ETHUSDT': 1_500_000.0,   # ADV = $150M -> mega_large
        'SMALL1': 200_000.0,      # ADV = $20M -> mid_small
        'SMALL2': 100_000.0,      # ADV = $10M -> mid_small
    }, index=dates)
    # Future spike after index 24 must NEVER leak into day 24 classification.
    vol.loc[dates[25]:, 'SMALL1'] = 5_000_000.0
    fields = {
        'open': close.copy(),
        'high': close.copy(),
        'low': close.copy(),
        'close': close.copy(),
        'volume': vol,
        'vwap': close.copy(),
    }
    mask = pd.DataFrame(True, index=dates, columns=cols)
    return panel.Panels(fields, mask), dates


def test_causal_go_tier_map_no_lookahead():
    panels, dates = _make_panels()
    tmap = s.causal_go_tier_map(panels, dates[24], ['BTCUSDT', 'SMALL1', 'SMALL2'])
    assert tmap == {'BTCUSDT': 'mega_large', 'SMALL1': 'mid_small', 'SMALL2': 'mid_small'}
    # Insufficient < 20 bars returns empty for that symbol.
    tmap_early = s.causal_go_tier_map(panels, dates[10], ['BTCUSDT', 'SMALL1'])
    assert tmap_early == {}


def test_apply_go_guardrail_cap_and_filter():
    weights = {'BTCUSDT': 0.25, 'SMALL1': 0.25, 'SMALL2': -0.30, 'ETHUSDT': -0.20}
    tier_map = {'BTCUSDT': 'mega_large', 'ETHUSDT': 'mega_large', 'SMALL1': 'mid_small', 'SMALL2': 'mid_small'}

    w_m1, ch1 = s.apply_go_guardrail(weights, tier_map, max_mid_small_pct=25.0)
    assert ch1 is True
    assert w_m1['BTCUSDT'] == 0.25
    assert w_m1['ETHUSDT'] == -0.20
    mid_gross = abs(w_m1['SMALL1']) + abs(w_m1['SMALL2'])
    assert np.isclose(mid_gross, 0.25, atol=1e-5)

    w_m2, ch2 = s.apply_go_guardrail(weights, tier_map, size_tier='mega_large')
    assert ch2 is True
    assert w_m2 == {'BTCUSDT': 0.25, 'ETHUSDT': -0.20}


def test_verdict_hierarchy():
    assert s.verdict_arm({'hourly_valid': False, 'changed_rebalances': 5}) == 'invalid'
    assert s.verdict_arm({'hourly_valid': True, 'changed_rebalances': 0}) == 'invalid'
    assert s.verdict_arm({
        'hourly_valid': True, 'changed_rebalances': 5, 'z': 0.8, 'val_delta': 0.1, 'forward_delta': 0.1
    }) == 'fail'
    assert s.verdict_arm({
        'hourly_valid': True, 'changed_rebalances': 5, 'z': 1.5, 'val_delta': 0.1, 'forward_delta': 0.1,
        'bx_delta': -0.001, 'placebo_pct': 0.99,
    }) == 'exposure_only'
    assert s.verdict_arm({
        'hourly_valid': True, 'changed_rebalances': 5, 'z': 1.5, 'val_delta': 0.1, 'forward_delta': 0.1,
        'bx_delta': 0.001, 'placebo_pct': 0.80,
    }) == 'fewer_trades'
    assert s.verdict_arm({
        'hourly_valid': True, 'changed_rebalances': 5, 'z': 1.5, 'val_delta': 0.1, 'forward_delta': 0.1,
        'bx_delta': 0.001, 'placebo_pct': 0.95,
    }) == 'screen_pass'
    assert s.verdict_arm({
        'hourly_valid': True, 'changed_rebalances': 5, 'z': 3.5, 'val_delta': 0.1, 'forward_delta': 0.1,
        'bx_delta': 0.001, 'placebo_pct': 0.95,
    }) == 'strong'


def test_prereg_hash_matches():
    assert len(s.verify_prereg()) > 100
