import hashlib
import unittest

import numpy as np
import pandas as pd

from scripts.backtest.ltw_crypto_study import (
    PREREG,
    PREREG_SHA256,
    _build_double_sort_2x3_cmom,
    _build_quantile_weights,
    _compute_s1_verdict,
    _run_nt,
    cs_zscore,
    newey_west_ols,
    verify_prereg,
)


class LtwCryptoStudyTests(unittest.TestCase):
    def test_prereg_sha256_matches_frozen_doc(self):
        raw = verify_prereg()
        self.assertEqual(hashlib.sha256(raw).hexdigest(), PREREG_SHA256)
        self.assertTrue(PREREG.exists())

    def test_cs_zscore_and_quantile_weights(self):
        s = pd.Series({f"C{i}": float(i) for i in range(20)})
        z = cs_zscore(s)
        self.assertAlmostEqual(float(z.mean()), 0.0, places=10)
        self.assertAlmostEqual(float(z.std(ddof=1)), 1.0, places=10)

        w_ew = _build_quantile_weights(s, weighting="ew", quantile=0.2)
        long_sum = sum(v for v in w_ew.values() if v > 0)
        short_sum = sum(v for v in w_ew.values() if v < 0)
        self.assertAlmostEqual(long_sum, 0.5, places=10)
        self.assertAlmostEqual(short_sum, -0.5, places=10)
        self.assertEqual(len(w_ew), 8)  # top 4 + bottom 4

        adv = pd.Series({f"C{i}": float((i + 1) * 1e6) for i in range(20)})
        w_vw = _build_quantile_weights(s, weighting="vw", adv_row=adv, quantile=0.2, cap=0.25)
        self.assertAlmostEqual(sum(v for v in w_vw.values() if v > 0), 0.5, places=10)
        self.assertAlmostEqual(sum(v for v in w_vw.values() if v < 0), -0.5, places=10)
        self.assertLessEqual(max(abs(v) for v in w_vw.values()), 0.25 + 1e-12)

    def test_double_sort_2x3_cmom_neutral(self):
        syms = [f"C{i}" for i in range(24)]
        r21 = pd.Series({s: float(i) for i, s in enumerate(syms)})
        adv = pd.Series({s: float((i % 6) + 1) * 1e7 for i, s in enumerate(syms)})
        w = _build_double_sort_2x3_cmom(r21, adv)
        self.assertAlmostEqual(sum(w.values()), 0.0, places=10)
        self.assertAlmostEqual(sum(v for v in w.values() if v > 0), 0.5, places=10)
        self.assertAlmostEqual(sum(v for v in w.values() if v < 0), -0.5, places=10)

    def test_newey_west_ols_recovers_slope_and_alpha(self):
        rng = np.random.default_rng(123)
        n = 300
        x = rng.normal(0.0, 0.02, size=n)
        y = 0.001 + 1.5 * x + rng.normal(0.0, 0.005, size=n)
        X = np.column_stack([np.ones(n), x])
        res = newey_west_ols(y, X, lags=5)
        self.assertAlmostEqual(res["beta"][0], 0.001, delta=0.001)
        self.assertAlmostEqual(res["beta"][1], 1.5, delta=0.1)
        self.assertGreater(res["t"][1], 10.0)

    def test_run_nt_clips_buffer_bars_and_applies_carry(self):
        idx = pd.date_range("2025-05-01", periods=12, freq="1D", tz="UTC")
        # Buffer bar after split_end (May 8..12) has an artificial 5x spike; clipped split [May 3..May 7] must NOT include it!
        closes = [100.0] * 8 + [500.0, 500.0, 500.0, 500.0]
        df = pd.DataFrame(
            {"open": [100.0] * 12, "high": closes, "low": [99.0] * 12, "close": closes, "volume": [1e6] * 12},
            index=idx,
        )
        tgt = pd.DataFrame({"BTC": [0.5]}, index=[idx[2]])  # May 3
        bt = _run_nt({"BTC": df}, tgt, eval_start=idx[2], eval_end=idx[6])
        self.assertEqual(bt["equity"].index.min(), idx[2])
        self.assertEqual(bt["equity"].index.max(), idx[6])
        self.assertLess(float(bt["equity"].iloc[-1]), 105_000.0)
        self.assertGreater(float(bt["carry"].get("funding_paid", 0.0)), 0.0)

    def test_s1_verdict_requires_placebo_budget(self):
        # Without >=200 placebos (e.g. --placebo-n 0), high z_stat must NOT be promoted to 'strong'
        self.assertEqual(
            _compute_s1_verdict(z_stat=4.0, z_k=2.8, val_delta=0.002, ho_delta=0.002, placebo_pct=None, placebo_n=0),
            "insufficient_placebo",
        )
        self.assertEqual(
            _compute_s1_verdict(z_stat=4.0, z_k=2.8, val_delta=0.002, ho_delta=0.002, placebo_pct=0.98, placebo_n=50),
            "insufficient_placebo",
        )
        self.assertEqual(
            _compute_s1_verdict(z_stat=4.0, z_k=2.8, val_delta=0.002, ho_delta=0.002, placebo_pct=0.98, placebo_n=200),
            "strong",
        )


if __name__ == "__main__":
    unittest.main()
