import unittest

import numpy as np
import pandas as pd

from scripts.factor_mining import evaluate
from scripts.factor_mining.panel import Panels
from scripts.factor_mining.sensitivity_20261028 import (
    funding_adjusted_ls, holding_funding, old_universe, still_accepted,
)


class SensitivityTests(unittest.TestCase):
    def test_old_universe_uses_valid_history_strictly_before_cutoff(self):
        dates = pd.date_range('2021-10-01', periods=180)
        close = pd.DataFrame({'OLD': 1., 'NEW': np.nan}, index=dates)
        close.loc['2022-01-01':, 'NEW'] = 2.
        fields = {name: close.copy() for name in ('open', 'high', 'low', 'close', 'volume', 'vwap')}
        sub = old_universe(Panels(fields, close.notna(), windows={'train': 100, 'val': 40, 'holdout': 20}))
        self.assertEqual(list(sub.mask.columns), ['OLD'])
        self.assertFalse(sub.mask['OLD'].iloc[:59].any())
        self.assertTrue(sub.mask['OLD'].iloc[59:].all())
        self.assertTrue(sub.mask.index.equals(close.index))

    def test_holding_funding_alignment_and_missing_are_not_filled(self):
        f = pd.DataFrame({'A': [99., 1., 2., 3., 88.]})
        got = holding_funding(f, 2)
        self.assertEqual(got['A'].iloc[0], 9.)  # 3 * (day 1 + day 2)
        self.assertEqual(got['A'].iloc[1], 15.)
        self.assertTrue(np.isnan(got['A'].iloc[-2]))
        f.iloc[2] = np.nan
        self.assertTrue(np.isnan(holding_funding(f, 2)['A'].iloc[0]))

    def test_funding_sign_costs_fixed_selection_and_missing_selected_legs(self):
        dates = pd.date_range('2025-01-01', periods=8)
        values = pd.DataFrame(np.tile(np.arange(20), (8, 1)), index=dates, dtype=float)
        raw = values * 0 + 0.1
        mask = raw.notna()
        rates = values * 0 + 0.0001
        rates.iloc[:, -4:] = 0.0002  # Long leg pays twice the short leg's receipt.
        accrued = holding_funding(rates, 2)
        segment = pd.Series([True] * 4 + [False] * 4, index=dates)
        cfg = {'min_names': 15, 'one_way_cost': 0.001, 'periods_per_year': 365}
        base, _ = evaluate.long_short(values, raw, mask, segment, 2, cfg)
        got = funding_adjusted_ls(values, raw, accrued, mask, segment, 2, cfg)
        expected = (base.mean() - 0.0006) * 365 / 2
        self.assertAlmostEqual(got['ls_ret'], expected)
        self.assertEqual(got['missing_legs'], 0)
        # Missing a middle-bucket coin must not change selection or costs.
        accrued.iloc[:, 10] = np.nan
        self.assertAlmostEqual(funding_adjusted_ls(values, raw, accrued, mask, segment, 2, cfg)['ls_ret'], expected)
        accrued.iloc[0, 19] = np.nan
        missing = funding_adjusted_ls(values, raw, accrued, mask, segment, 2, cfg)
        self.assertIsNone(missing['ls_ret'])
        self.assertEqual(missing['missing_legs'], 1)

    def test_funding_loss_or_unknown_funding_fails_acceptance(self):
        metrics = {'train': {'ic_halves': [0.1, 0.1], 'ic': 0.1, 't': 10, 'ls_ret': 1},
                   'val': {'ic': 0.1, 't': 10, 'ls_ret': 1}}
        self.assertTrue(still_accepted(metrics, 100))
        for ret in (0., -0.1, None):
            self.assertFalse(still_accepted(metrics, 100, {'train': {'ls_ret': 1}, 'val': {'ls_ret': ret}}))
