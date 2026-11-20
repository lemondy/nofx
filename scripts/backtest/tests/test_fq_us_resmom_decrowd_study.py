"""Unit tests for fq_us_resmom_decrowd_study (SHA-256 lock, Residual Momentum, De-Crowding)."""
from __future__ import annotations

import unittest

import numpy as np
import pandas as pd

from scripts.backtest import fq_us_resmom_decrowd_study as mod


class TestFQUSResMomDecrowdStudy(unittest.TestCase):
    def test_prereg_sha256_verified(self):
        raw = mod.verify_prereg()
        self.assertGreater(len(raw), 500)

    def test_residual_momentum_removes_beta_and_is_causal(self):
        rng = np.random.default_rng(7)
        n = 300
        dates = pd.bdate_range("2022-01-03", periods=n)
        spy = pd.Series(rng.normal(0.0015, 0.012, size=n), index=dates)
        # BETA_STOCK: pure 2.5x SPY + tiny noise (high raw mom, ~0 residual mom)
        # ALPHA_STOCK: 0.5x SPY + steady +0.12%/day idiosyncratic drift (high residual mom)
        # LOSER_STOCK: 1.0x SPY - 0.12%/day drift
        rets = pd.DataFrame(
            {
                "BETA": 2.5 * spy + rng.normal(0.0, 0.003, size=n),
                "ALPHA": 0.5 * spy + 0.0012 + rng.normal(0.0, 0.005, size=n),
                "LOSER": 1.0 * spy - 0.0012 + rng.normal(0.0, 0.005, size=n),
            },
            index=dates,
        )
        loc = 260
        ser1, betas1 = mod.compute_residual_momentum_at(rets, spy, loc, ["BETA", "ALPHA", "LOSER"])
        self.assertAlmostEqual(betas1["BETA"], 2.5, delta=0.15)
        self.assertGreater(ser1["ALPHA"], ser1["BETA"])
        self.assertGreater(ser1["ALPHA"], ser1["LOSER"])

        # Causality: corrupting future rows > loc must NOT change scores at loc
        rets2 = rets.copy()
        rets2.iloc[loc + 1 :] = -0.50
        ser2, _ = mod.compute_residual_momentum_at(rets2, spy, loc, ["BETA", "ALPHA", "LOSER"])
        pd.testing.assert_series_equal(ser1, ser2)

    def test_decrowd_penalizes_correlated_cluster_and_raises_neff(self):
        rng = np.random.default_rng(11)
        n = 100
        dates = pd.bdate_range("2024-01-02", periods=n)
        theme = rng.normal(0.0, 0.02, size=n)
        # 4 cloned AI stocks + 1 independent stock with identical volatility
        rets = pd.DataFrame(
            {
                "CLONE1": theme + rng.normal(0.0, 0.002, size=n),
                "CLONE2": theme + rng.normal(0.0, 0.002, size=n),
                "CLONE3": theme + rng.normal(0.0, 0.002, size=n),
                "CLONE4": theme + rng.normal(0.0, 0.002, size=n),
                "INDEP": rng.normal(0.0, 0.02, size=n),
            },
            index=dates,
        )
        syms = list(rets.columns)
        pen, mean_corr = mod.compute_decrowd_penalties(rets, n - 1, syms)
        self.assertGreater(mean_corr, 0.4)
        self.assertLess(pen["INDEP"], pen["CLONE1"] * 0.5)

        # Equal weights vs de-crowded weights: N_eff must increase!
        w_eq = {s: 0.1 for s in syms}
        inv = 1.0 / pen
        w_dc = {s: float(0.5 * inv[s] / inv.sum()) for s in syms}
        neff_eq = mod.compute_leg_neff(rets, n - 1, w_eq)
        neff_dc = mod.compute_leg_neff(rets, n - 1, w_dc)
        self.assertGreater(neff_dc, neff_eq)

    def test_h1_placebo_sufficient_stats_exact_parity(self):
        rng = np.random.default_rng(42)
        dates = pd.bdate_range("2020-01-01", periods=300)
        syms = [f"S{i}" for i in range(25)]
        spy = pd.Series(rng.normal(0.0005, 0.01, len(dates)), index=dates)
        rets = pd.DataFrame({s: 1.2 * spy + rng.normal(0.0, 0.015, len(dates)) for s in syms}, index=dates)
        rets.iloc[10:20, 3] = np.nan
        vol = pd.DataFrame(0.02, index=dates, columns=syms)
        _, betas_map = mod.compute_residual_momentum_at(rets, spy, 280, syms)
        metas = [{"date": dates[280], "date_loc": 280, "eligible": syms, "betas": betas_map}]
        pre = mod._precompute_h1_placebo_stats(rets, spy, vol, metas)[0]

        b_syms = pre["b_syms"]
        b_vals = pre["betas"].copy()
        np.random.default_rng(mod.SEED_H1).shuffle(b_vals)
        sig_ref, _ = mod.compute_residual_momentum_at(
            rets, spy, 280, syms, beta_override=dict(zip(b_syms, b_vals.tolist()))
        )

        m_cnt = pre["M"]
        res_sum = pre["A"] - b_vals * pre["B"]
        sum_sq = pre["C"] - 2.0 * b_vals * pre["D"] + (b_vals * b_vals) * pre["E"]
        res_sd = np.sqrt(np.maximum(0.0, (sum_sq - (res_sum * res_sum) / m_cnt) / (m_cnt - 1.0)))
        scores_fast = pd.Series(res_sum / (res_sd * np.sqrt(m_cnt)), index=b_syms)
        diff = float((scores_fast - sig_ref.reindex(b_syms)).abs().max())
        self.assertLess(diff, 1e-12)


if __name__ == "__main__":
    unittest.main()
