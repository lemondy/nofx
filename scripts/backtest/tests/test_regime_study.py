"""Deterministic causal feature and full-screen controls; no network or trading."""
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

import numpy as np
import pandas as pd

from scripts.backtest import regime_study as s


def bars_from_returns(returns):
    close = 100*np.exp(np.cumsum(returns))
    op = np.r_[close[0], close[:-1]]
    return pd.DataFrame({"open": op, "high": np.maximum(op, close)*1.001,
                         "low": np.minimum(op, close)*.999, "close": close, "volume": 1.},
                        index=pd.date_range("2020-01-01", periods=len(close), freq="D", tz="UTC"))


class FeatureTests(unittest.TestCase):
    def test_known_trend_ar_and_vr(self):
        rng = np.random.default_rng(219)
        trending = bars_from_returns(.006+rng.normal(0, .001, 3000))
        walk = bars_from_returns(rng.normal(0, .01, 6000))
        ar = np.zeros(6000)
        noise = rng.normal(0, .01, len(ar))
        for k in range(1, len(ar)):
            ar[k] = -.8*ar[k-1]+noise[k]
        ft = s.compute_features(trending, 96)
        fw = s.compute_features(walk, 96)
        fa = s.compute_features(bars_from_returns(ar), 96)
        self.assertGreater(ft.er.dropna().median(), .95)
        self.assertLess(fw.er.dropna().median(), .2)
        self.assertLess(fa.rho1.dropna().median(), -.7)
        self.assertLess(fa.vr4.dropna().median(), .3)
        self.assertAlmostEqual(fw.vr4.dropna().mean(), 1., delta=.15)
        vr = s.variance_ratio(np.log(walk.close).diff(), 96, 4)
        np.testing.assert_allclose(vr.z, (vr.vr-1)/np.sqrt(2*7*3/(3*4*96)), equal_nan=True)

    def test_jump_squeeze_and_exact_drawdown(self):
        rng = np.random.default_rng(8)
        r = rng.normal(0, .01, 800)
        r[550] = -.3
        f = s.compute_features(bars_from_returns(r), 40)
        self.assertGreater(f.jump_share.iloc[550], .65)
        self.assertEqual(f.last_jump.iloc[550], 1)
        self.assertEqual(f.bars_since_jump.iloc[550], 0)
        self.assertEqual(f.bars_since_jump.iloc[553], 3)
        r = np.r_[rng.normal(0, .02, 250), rng.normal(size=100)*np.geomspace(.001, .000001, 100), .10,
                  rng.normal(0, .01, 80)]
        squeeze = s.compute_features(bars_from_returns(r), 40)
        self.assertGreater(squeeze.squeeze_duration.iloc[290:350].max(), 4)
        self.assertLess(squeeze.bb_percentile.iloc[300:330].median(), .3)
        self.assertGreater(squeeze.breakout.iloc[350], 4)
        c = bars_from_returns(r).close.iloc[311:351].to_numpy()
        self.assertAlmostEqual(squeeze.drawdown.iloc[350], np.max(1-c/np.maximum.accumulate(c)))

    def test_every_feature_and_binning_truncation(self):
        frame = s.synthetic_panel(seed=44, bars=650, assets=1, planted=True)[0][1]
        frame.iloc[430, frame.columns.get_loc("close")] *= .6
        frame.iloc[430, frame.columns.get_loc("low")] *= .6
        for n in (20, 40, 60):
            full = s.compute_features(frame, n)
            for cut in (165, 279, 431, 505, 649):
                prefix = s.compute_features(frame.iloc[:cut], n)
                for name in (*s.FEATURES, "vr4_z", "vr8_z", "state"):
                    with self.subTest(n=n, cut=cut, feature=name):
                        np.testing.assert_allclose(prefix[name].iloc[-1], full[name].iloc[cut-1],
                                                   rtol=1e-8, atol=1e-8, equal_nan=True)
                        self.assertEqual(s.expanding_terciles(prefix[name]).iloc[-1],
                                         s.expanding_terciles(full[name]).iloc[cut-1])

    def test_cross_section_is_daily_and_causal(self):
        # Six assets insufficient by protocol; twelve permit causal snapshots.
        panel = s.synthetic_panel(seed=18, bars=1800, assets=12, timeframe="1h")
        base, _ = s.prepare_base(panel, "1h")
        arrays = s.build_features(panel, 48)
        full = s.cross_section_buckets(arrays["raw"][:, 0], base)
        cut = 1750
        sub = [(name, f.iloc[:cut]) for name, f in panel]
        partial_base, _ = s.prepare_base(sub, "1h")
        partial = s.build_features(sub, 48)
        buckets = s.cross_section_buckets(partial["raw"][:, 0], partial_base)
        expected = np.concatenate([full[j*1800:j*1800+cut] for j in range(12)])
        np.testing.assert_array_equal(buckets, expected)
        self.assertTrue((buckets >= 0).any())

    def test_gap_never_bridged_and_history_is_point_in_time(self):
        panel = s.synthetic_panel(bars=500, assets=2)
        frame = panel[0][1].copy()
        frame.iloc[300] = np.nan
        f = s.compute_features(frame, 40)
        self.assertTrue(f.iloc[300:381].isna().all().all())
        base, _ = s.prepare_base([("old", panel[0][1]), ("new", panel[1][1].iloc[-50:])], "1d")
        self.assertFalse(base["eligible"][-50:].any())
        self.assertTrue(base["eligible"][:500].any())


class StatisticsTests(unittest.TestCase):
    def test_forward_alignment_and_frozen_threshold(self):
        r = np.zeros(100)
        r[70] = .20
        frame = bars_from_returns(r+1e-5)
        t = s.forward_targets(frame, "1d")
        self.assertAlmostEqual(t[3]["return"][67], .20003)
        self.assertAlmostEqual(t[3]["return"][70], .00003)
        self.assertAlmostEqual(np.exp(t[3]["rv"][67]), .20001**2+2e-10)
        self.assertEqual(t[1]["tail"][69], 1)
        self.assertEqual(t[1]["tail"][70], 0)
        self.assertTrue(np.isnan(t[7]["return"][-7:]).all())

    def test_calendar_split_purge_and_future_endpoints(self):
        frames = s.synthetic_panel(bars=1000, assets=2)
        base, meta = s.prepare_base(frames, "1d")
        cutoff = pd.Timestamp(meta["split"]["cutoff"])
        dates = frames[0][1].index
        self.assertLess(int(dates[np.flatnonzero(base["train"][:1000])[-1]].value)+7*86_400_000_000_000, int(cutoff.value))
        self.assertGreaterEqual(dates[np.flatnonzero(base["holdout"][:1000])[0]], cutoff)
        self.assertEqual(meta["split"]["purge_bars"], 7)

    def test_bh_calibration_and_student_distribution(self):
        np.testing.assert_allclose(s.bh_adjust([.001, .02, .2, 1]), [.004, .04, 4*.2/3, 1])
        self.assertAlmostEqual(s.student_p(2, 100), .048212178731, places=10)
        rng = np.random.default_rng(101)
        any_rejected = [np.any(s.bh_adjust(rng.random(1000)) <= s.FDR) for _ in range(1500)]
        self.assertAlmostEqual(np.mean(any_rejected), s.FDR, delta=.035)

    def test_same_date_asset_replication_does_not_inflate_t(self):
        rng = np.random.default_rng(2)
        x = np.column_stack([np.ones(500), rng.normal(size=500)])
        y = .1*x[:, 1]+rng.normal(size=500)
        days = np.arange(500)
        a = s.ols(x, y, days, 7, 1)
        b = s.ols(np.repeat(x, 20, axis=0), np.repeat(y, 20), np.repeat(days, 20), 7, 1)
        self.assertAlmostEqual(a["t"], b["t"], delta=.02)

    def test_har_holdout_r2_uses_training_coefficients(self):
        rng = np.random.default_rng(77)
        n = 700
        controls = rng.normal(size=(n, 3))
        feature = rng.normal(size=n)
        y = controls[:, 0]+feature+rng.normal(0, .2, n)
        train, frozen = s.rv_test(feature[:400], y[:400], controls[:400], np.arange(400), 3, 1)
        hold, _ = s.rv_test(feature[400:], -y[400:], controls[400:], np.arange(300), 3, 1, frozen)
        self.assertGreater(train["delta_r2"], .3)
        self.assertLess(hold["delta_r2"], 0)
        self.assertLess(hold["coef"], 0)

    def test_identical_cost_cancels_in_difference(self):
        x = np.tile([0, 2], 300)
        y = np.sin(np.arange(600))*.02+.002*(x == 2)
        a, _ = s.contrast_test(x, y, np.arange(600), 1, 1, .0015)
        self.assertAlmostEqual(a["top"]-a["bottom"], a["net_difference"])
        self.assertAlmostEqual(a["top_net"]-a["bottom_net"], a["net_difference"])


class PipelineTests(unittest.TestCase):
    def test_full_null_walk_screen_and_holdout_confirmation(self):
        # Full 2,955-hypothesis screen, all features/windows/targets/methods.
        summaries = []
        # These 14 validation seeds were fixed AFTER the HAC specification was
        # calibrated on seeds 0..11,19,31. They are not calibration replays.
        for seed in range(100, 114):
            with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
                summary, train, hold = s.run_synthetic_control(directory, seed=seed)
                summaries.append(summary)
                self.assertEqual(len(train), 2955)
                self.assertLessEqual(summary["train_fdr"]/summary["tests"], .10)
                self.assertLessEqual(summary["effective"], summary["holdout_confirmed"])
                with self.assertRaisesRegex(ValueError, "single-use"):
                    s.evaluate_holdout(directory, "1d", "crypto")
                with self.assertRaisesRegex(ValueError, "Frozen experiment"):
                    s.run_training(directory, "1d", "crypto", s.synthetic_panel())
        train_n = sum(x["train_fdr"] for x in summaries)
        hold_n = sum(x["holdout_confirmed"] for x in summaries)
        print("NULL_CONTROLS", json.dumps(summaries))
        # Under the global null, FDR = P(any rejection). With 14 replicates a
        # 0..4 rejection-family count is a deliberately broad finite-sample band.
        self.assertLessEqual(sum(x["train_fdr"] > 0 for x in summaries), 4)
        self.assertLessEqual(hold_n, max(1, train_n*.25))
        self.assertLessEqual(sum(x["effective"] for x in summaries), 1)
        # No requirement for exactly 10% rejections under a global null: that is NOT FDR.

    def test_planted_high_er_momentum_detected_train_and_holdout(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
            _, train, hold = s.run_synthetic_control(directory, seed=5, planted=True, bars=1600, assets=16)
        tr = train[(train.feature == "er") & (train.target == "T1") & (train.method == "ts") & (train.window == 40)]
        ho = hold[(hold.feature == "er") & (hold.target == "T1") & (hold.method == "ts") & (hold.window == 40)]
        self.assertTrue(tr.candidate.any())
        self.assertTrue((tr.coef > 0).any())
        self.assertTrue(ho.confirmed.any())
        self.assertTrue(ho.effective.any())


if __name__ == "__main__":
    unittest.main()
