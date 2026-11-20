"""Unit tests for fq_crypto_squeeze_cooldown_study (SHA-256 lock, causality, C1 mechanics)."""
from __future__ import annotations

import datetime as dt
import unittest

import numpy as np
import pandas as pd

from scripts.backtest import fq_crypto_squeeze_cooldown_study as mod
from scripts.factor_mining import panel


class TestFQCryptoSqueezeCooldownStudy(unittest.TestCase):
    def test_prereg_sha256_verified(self):
        raw = mod.verify_prereg()
        self.assertGreater(len(raw), 500)

    def test_btc_regime_strictly_causal(self):
        idx = pd.DatetimeIndex(pd.date_range("2025-01-01", periods=100, tz="UTC"))
        closes = np.linspace(50.0, 150.0, 100)
        df = pd.DataFrame({"close": closes}, index=idx)
        reg1 = mod.compute_btc_regimes(df)
        self.assertEqual(reg1[idx[10].date()], "unknown")
        self.assertEqual(reg1[idx[80].date()], "bull")

        # Crash after day 80 must NOT affect day 80's regime
        df2 = df.copy()
        df2.iloc[81:, 0] = 10.0
        reg2 = mod.compute_btc_regimes(df2)
        self.assertEqual(reg1[idx[80].date()], reg2[idx[80].date()])

    def test_c1_invvar_and_squeeze_guard(self):
        # 80 symbols, 120 days; SYM00 is bottom-ranked short with dead-cat 3d squeeze; SYM01 has 4x vol
        dates = pd.date_range("2025-01-01", periods=120, freq="D")
        rng = np.random.default_rng(42)
        syms = [f"SYM{i:02d}USDT" for i in range(80)]
        close_data = {}
        for i, s in enumerate(syms):
            vol = 0.015 if i != 1 else 0.06  # SYM01 has 4x higher vol than peers
            r = rng.normal(0.0, vol, size=len(dates))
            c = 100.0 * np.cumprod(1.0 + r)
            if i == 0:
                # Crash -35% at t-6..t-4 then violent 3-day squeeze rebound (+25%):
                # ret7 stays negative (baseline ret7 misses it!), while ret3 & vol5/vol30 spike catch it!
                c[-7:-3] = c[-8] * np.array([0.85, 0.75, 0.68, 0.65])
                c[-3:] = c[-4] * np.array([1.08, 1.18, 1.25])
            close_data[s] = c
        close_df = pd.DataFrame(close_data, index=dates)
        mask_df = pd.DataFrame(True, index=dates, columns=syms)
        p = panel.Panels({"close": close_df}, mask_df)
        d = dates[-1]
        scores = pd.Series({s: float(i) for i, s in enumerate(syms)})

        w_c1, meta = mod.build_c1_weights_at(p, scores, d, universal_invvar_short=True, squeeze_guard=True)
        # SYM00 (bottom score) must be excluded by squeeze guard (ret3/vol5 spike)
        self.assertNotIn("SYM00USDT", w_c1)
        self.assertGreaterEqual(meta["extra_dropped"], 1)

        # Compare SYM01 (high vol short) weight under 1/sigma^2 vs 1/sigma
        w_invvol, _ = mod.build_c1_weights_at(p, scores, d, universal_invvar_short=False, squeeze_guard=True)
        if "SYM01USDT" in w_c1 and "SYM01USDT" in w_invvol:
            self.assertLess(abs(w_c1["SYM01USDT"]), abs(w_invvol["SYM01USDT"]))


if __name__ == "__main__":
    unittest.main()
