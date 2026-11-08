import math
import unittest

import numpy as np
import pandas as pd

from scripts.factor_mining import ic_overlap_audit as audit


class IcOverlapAuditTest(unittest.TestCase):
    def test_plain_t_matches_formula_and_ignores_nan(self):
        x = np.array([0.1, 0.3, np.nan, 0.2, 0.4, 0.0])
        v = x[np.isfinite(x)]
        expected = v.mean() / (v.std(ddof=1) / math.sqrt(len(v)))
        self.assertAlmostEqual(audit.plain_t(x), expected)
        self.assertTrue(math.isnan(audit.plain_t([1.0, 2.0])))

    def test_nonoverlap_uses_every_hth_day_per_offset(self):
        ic = pd.Series(np.arange(30, dtype=float) % 3 + np.linspace(0, 1, 30))
        ts = audit.nonoverlap_ts(ic, 3)
        self.assertEqual(len(ts), 3)
        for offset, t in enumerate(ts):
            self.assertAlmostEqual(t, audit.plain_t(ic.to_numpy()[offset::3]))

    def test_ar1_panel_is_stationary_with_unit_variance(self):
        rng = np.random.default_rng(1)
        x = audit.ar1_panel(rng, (4000, 50), 0.9)
        self.assertAlmostEqual(float(x[500:].var()), 1.0, delta=0.08)
        lag1 = np.corrcoef(x[501:].ravel(), x[500:-1].ravel())[0, 1]
        self.assertAlmostEqual(float(lag1), 0.9, delta=0.02)

    def test_overlap_inflates_plain_t_for_persistent_factor_on_synthetic_panel(self):
        rng = np.random.default_rng(7)
        days, names, h = 1500, 40, 10
        daily = rng.standard_normal((days + h + 1, names)) * 0.02
        fwd = np.array([daily[i + 1:i + 1 + h].sum(axis=0) for i in range(days)])
        target = pd.DataFrame(fwd)
        mask = pd.DataFrame(True, index=target.index, columns=target.columns)
        plain, nonov = [], []
        for seed in range(30):
            vals = pd.DataFrame(audit.ar1_panel(np.random.default_rng(seed), (days, names), 0.98))
            ic = audit.evaluate.cs_rank_ic(vals, target, mask, 15)
            plain.append(abs(audit.plain_t(ic)))
            nonov.append(abs(audit.plain_t(ic.to_numpy()[::h])))
        self.assertGreater(np.std(plain), 1.8 * np.std(nonov))


if __name__ == '__main__':
    unittest.main()
