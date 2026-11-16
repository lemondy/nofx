"""Offline unit tests for crypto_llm_tier_split_study."""
from __future__ import annotations

import pandas as pd

from scripts.backtest import crypto_llm_tier_split_study as s


def test_classify_trade_causal_no_lookahead():
    idx = pd.date_range('2026-10-01', periods=25, freq='D')
    # First 21 days = $50M -> mid_small; day 22 spikes to $500M
    vals = [50_000_000.0] * 21 + [500_000_000.0] * 4
    series = pd.Series(vals, index=idx)
    # Rolling 20-day mean on idx[20] uses idx[1..20] = $50M
    adv = series.rolling(20, min_periods=20).mean().dropna()
    adv_tables = {'COINUSDT': adv}
    cd_syms = {'COINUSDT'}

    # Entry on 2026-10-22 uses sig_day = 2026-10-21 (idx[20]), BEFORE the day-22 spike!
    ann = s.classify_trade_causal('COINUSDT', '2026-10-22T12:00:00Z', cd_syms, adv_tables)
    assert ann['sig_day'] == '2026-10-21'
    assert ann['tier'] == 'mid_small'
    assert ann['adv20'] == 50_000_000.0

    # Unknown symbol defaults to non_crypto_or_unmapped
    ann_unk = s.classify_trade_causal('AAPLBUSDT', '2026-10-22T12:00:00Z', cd_syms, adv_tables)
    assert ann_unk['tier'] == 'non_crypto_or_unmapped'
    assert ann_unk['has_adv20'] is False


def test_prereg_sha_matches():
    assert len(s.verify_prereg()) > 100
