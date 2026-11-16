"""Offline unit tests for fq_crypto_split_sleeve_study."""
from __future__ import annotations

import numpy as np

from scripts.backtest import fq_crypto_split_sleeve_study as s


def test_combine_equalized_sleeves_50_50():
    run_a = {'daily_equity': {'2025-05-04': 1000.0, '2025-05-05': 1100.0, '2025-05-06': 1210.0}}
    run_b = {'daily_equity': {'2025-05-04': 1000.0, '2025-05-05': 900.0, '2025-05-06': 990.0}}
    comb = s.combine_equalized_sleeves(run_a, run_b, ['2025-05-04'])
    # Day 1 -> 1000; Day 2 -> 0.5*1100 + 0.5*900 = 1000; Day 3 -> 0.5*1210 + 0.5*990 = 1100
    assert np.isclose(comb['daily_equity']['2025-05-05'], 1000.0)
    assert np.isclose(comb['daily_equity']['2025-05-06'], 1100.0)


def test_prereg_sha_matches():
    assert len(s.verify_prereg()) > 100
