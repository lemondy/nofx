"""Offline, read-only-data tests for the causal Brooks feature library."""

import json
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest.brooks import BrooksParams, STRENGTH_RULES, compute_features


def bars(rows):
    return pd.DataFrame(rows, columns=["open", "high", "low", "close"],
                        index=pd.date_range("2020-01-01", periods=len(rows)), dtype=float).assign(volume=1.0)


def mirror(frame, center=200.0):
    x = frame.copy()
    for key in ("open", "close"):
        x[key] = center - frame[key]
    x["high"], x["low"] = center - frame.low, center - frame.high
    return x


def synthetic(n=140, seed=871):
    rng = np.random.default_rng(seed)
    change = rng.normal(size=n) + np.sin(np.arange(n) / 11)
    close = 100 + change.cumsum()
    op = np.r_[100, close[:-1]] + rng.normal(0, 0.2, n)
    return pd.DataFrame(dict(open=op, high=np.maximum(op, close) + rng.uniform(0, 1, n),
        low=np.minimum(op, close) - rng.uniform(0, 1, n), close=close,
        volume=rng.uniform(0, 1000, n)), index=pd.date_range("2020-01-01", periods=n, tz="UTC"))


def short_params(**kwargs):
    return replace(BrooksParams(), atr_period=2, ema_period=3,
                   strong_body_lookback=2, **kwargs)


class BrooksGeometryTests(unittest.TestCase):
    def test_binary_geometry_and_zero_range(self):
        x = bars([(10, 12, 8, 10), (8, 12, 8, 12), (11, 12, 8, 10), (10, 10, 10, 10)])
        f = compute_features(x)
        self.assertEqual(f.is_doji.tolist(), [True, False, True, True])
        self.assertTrue((f.is_doji ^ f.is_trend).all())
        self.assertEqual(f.loc[x.index[1], ["body", "range", "upper_tail", "lower_tail", "close_pos"]].tolist(), [4, 4, 0, 0, 1])
        self.assertEqual(f.close_pos.iloc[-1], 0.5)
        self.assertEqual(f.body_ratio.iloc[-1], 0)
        self.assertFalse(np.isinf(f.select_dtypes("number").to_numpy()).any())

    def test_atr_wilder_seed_and_strong_tail_filter(self):
        x = bars([(10, 12, 8, 10), (10, 16, 10, 14), (14, 16, 12, 15)])
        f = compute_features(x, short_params())
        self.assertTrue(np.isnan(f.atr.iloc[0]))
        self.assertEqual(f.atr.iloc[1], 5)
        self.assertEqual(f.atr.iloc[2], 4.5)
        self.assertEqual(f.ema20.iloc[2], x.close.ewm(span=3, adjust=False).mean().iloc[2])
        x = bars([(10, 11, 9, 10)] * 12 + [(10, 20, 10, 14)])
        self.assertFalse(compute_features(x).strong_bull.iloc[-1])

    def test_ii_iii_ioi_and_equal_boundaries(self):
        x = bars([(5, 10, 0, 5), (5, 9, 1, 5), (5, 8, 2, 5),
                  (5, 10, 0, 5), (5, 9, 1, 5), (5, 9, 1, 5), (5, 9, 1, 5)])
        f = compute_features(x)
        self.assertEqual(f.ii.tolist(), [False, False, True, False, False, True, True])
        self.assertTrue(f.ioi.iloc[4])
        self.assertTrue(f.iii.iloc[6])
        self.assertTrue(f.inside.iloc[5])
        self.assertFalse(f.outside.iloc[5])
        self.assertFalse(compute_features(bars([(5, 10, 0, 5), (5, 10, -1, 5)])).outside.iloc[-1])

    def test_h1_to_h4_separation_reset_and_l_mirror(self):
        highs = [14, 13, 13.5, 13.6, 12, 12.5, 12.6, 11, 11.5, 10, 10.5, 15, 14, 14.5]
        x = bars([(h - 1, h, h - 2, h - 1) for h in highs])
        f = compute_features(x)
        self.assertEqual(f.h_entry.tolist(), [0, 0, 1, 0, 0, 2, 0, 0, 3, 0, 4, 0, 0, 1])
        self.assertEqual(f.h_count.iloc[4], 1)  # descending high arms; does not erase H1
        self.assertEqual(f.h_count.iloc[11], 0)
        self.assertTrue(f.h_reset.iloc[11])
        reflected = compute_features(mirror(x))
        for name in ["count", "entry", "reset"]:
            np.testing.assert_array_equal(f["h_" + name], reflected["l_" + name])
        for n in range(1, 5):
            np.testing.assert_array_equal(f[f"h{n}"], reflected[f"l{n}"])

    def test_equal_high_does_not_rearm(self):
        highs = [14, 13, 13.5, 13.5, 13.8]
        f = compute_features(bars([(h - 1, h, h - 2, h - 1) for h in highs]))
        self.assertEqual(f.h_entry.tolist(), [0, 0, 1, 0, 0])

    def test_pivots_emit_at_confirmation_not_at_extreme(self):
        x = bars([(9, 10, 8, 9), (10, 11, 9, 10), (12, 13, 11, 12),
                  (11, 12, 10, 11), (10, 11, 9, 10)])
        f = compute_features(x)
        self.assertEqual(f.swing_high_confirmed.tolist(), [False, False, False, False, True])
        self.assertEqual(f.last_swing_high.iloc[-1], 13)
        self.assertEqual(f.last_swing_high.iloc[2], 0)

    def test_reversal_excessive_overlap_f1_f2_and_mirror(self):
        prev = (10, 12, 8, 10)
        good = compute_features(bars([prev, (7, 10, 5, 9.8)]), short_params())
        bad = compute_features(bars([prev, (7.5, 11, 7, 10)]), short_params())
        self.assertTrue(good.reversal_bull_valid.iloc[-1])
        self.assertTrue(good.reversal_bull_f2.iloc[-1])
        self.assertAlmostEqual(good.reversal_bull_tail_ratio.iloc[-1], 0.4)
        self.assertTrue(bad.reversal_bull_excessive_overlap.iloc[-1])
        self.assertFalse(bad.reversal_bull_valid.iloc[-1])
        self.assertFalse(bad.reversal_bull_f3.iloc[-1])
        equality = compute_features(bars([prev, (6, 10, 6, 9)]))
        self.assertFalse(equality.reversal_bull_excessive_overlap.iloc[-1])
        reflected = compute_features(mirror(bars([prev, (7, 10, 5, 9.8)])), short_params())
        for suffix in ("minimum", "valid", "f1", "f2", "f3", "f5", "excessive_overlap"):
            self.assertEqual(good["reversal_bull_" + suffix].iloc[-1], reflected["reversal_bear_" + suffix].iloc[-1])

    def test_f4_is_delayed_and_f5_uses_close_not_high(self):
        x = bars([(10, 12, 8, 10), (7, 10, 5, 9.8), (9.8, 15, 9.8, 15)])
        f = compute_features(x, short_params())
        self.assertFalse(f.reversal_bull_f4.iloc[1])
        self.assertTrue(f.reversal_bull_f4.iloc[2])
        self.assertTrue(f.reversal_bull_f5.iloc[2])
        high_only = compute_features(bars([(10, 12, 8, 10), (8, 20, 5, 9)]))
        self.assertFalse(high_only.reversal_bull_f5.iloc[-1])
        self.assertEqual(high_only.reversal_bull_extremes_reversed.iloc[-1], 0)


class BrooksStateAndPatternTests(unittest.TestCase):
    def test_climax_ends_on_reverse_inside_or_long_tail(self):
        run = [(100, 102, 100, 102), (102, 104, 102, 104), (104, 106, 104, 106)]
        stops = [(106, 106, 103, 103), (104.5, 106, 104.5, 105.8), (106, 110.5, 106, 108)]
        for stop in stops:
            with self.subTest(stop=stop):
                x = bars(run + [stop, stop])
                f = compute_features(x)
                self.assertTrue(f.climax_bull_active.iloc[2])
                self.assertTrue(f.climax_bull_end.iloc[3])
                self.assertFalse(f.climax_bull_end.iloc[4])
                self.assertTrue(compute_features(mirror(x)).climax_bear_end.iloc[3])

    def test_ema_gap_twenty_touch_reclaim_and_side(self):
        x = bars([(100 + 3 * i, 102 + 3 * i, 100 + 3 * i, 102 + 3 * i) for i in range(30)])
        p = short_params()
        f = compute_features(x, p)
        self.assertFalse(f.ema_gap_bull_20.iloc[20])
        self.assertTrue(f.ema_gap_bull_20.iloc[21])
        self.assertEqual(f.ema_gap_bull_run.iloc[21], 20)
        last_ema, last_close = f.ema20.iloc[-1], x.close.iloc[-1]
        touch = pd.DataFrame([dict(open=last_close, high=last_close + 1,
            low=last_ema - 1, close=last_close, volume=1)],
            index=pd.date_range(x.index[0], periods=len(x) + 1)[-1:], dtype=float)
        g = compute_features(pd.concat([x, touch]), p)
        self.assertTrue(g.ema_gap_bull_first_touch.iloc[-1])
        self.assertTrue(g.ema_gap_bull_touch_reclaim.iloc[-1])
        self.assertEqual(g.ema_gap_bull_run.iloc[-1], 0)
        reflected = compute_features(mirror(pd.concat([x, touch])), p)
        self.assertTrue(reflected.ema_gap_bear_touch_reclaim.iloc[-1])
        # A gap to the opposite side without crossing the EMA is not a touch.
        jump = touch.copy()
        jump.loc[:, ["open", "high", "low", "close"]] = [last_ema - 10, last_ema - 9, last_ema - 12, last_ema - 11]
        self.assertFalse(compute_features(pd.concat([x, jump]), p).ema_gap_bull_first_touch.iloc[-1])

    def test_always_in_long_uncertain_short_and_reversal(self):
        x = bars([(100, 101, 99, 100)] * 12 + [(100, 104, 100, 104),
                 (103, 103, 100, 101), (101, 101, 99, 99), (99, 99, 90, 90), (90, 120, 90, 120)])
        f = compute_features(x, short_params())
        self.assertEqual(f.always_in.iloc[-5:].tolist(), [1, 1, 0, -1, 1])
        self.assertEqual(f.always_in_age.iloc[-1], 0)
        np.testing.assert_array_equal(f.always_in, -compute_features(mirror(x), short_params()).always_in)

    def test_always_in_follow_through_and_failed_breakout(self):
        p = short_params(always_in_reversal_bars=100, breakout_lookback=3)
        base = [(100, 101, 99, 100)] * 12
        x = bars(base + [(100, 104, 100, 104), (104, 107, 104, 107), (107, 110, 107, 110)])
        f = compute_features(x, p)
        self.assertEqual(f.always_in.iloc[-3:].tolist(), [0, 0, 1])
        self.assertTrue(f.breakout_bull_follow_confirm.iloc[-1])
        failed = compute_features(bars(base + [(100, 100, 90, 90), (90, 102, 90, 102)]), p)
        self.assertTrue(failed.failed_breakout_bear.iloc[-1])
        self.assertEqual(failed.always_in.iloc[-1], 1)

    def test_micro_channels_flat_rejection_and_climactic_run(self):
        x = bars([(100 + i, 101 + i, 100 + i, 101 + i) for i in range(35)])
        p = short_params()
        f = compute_features(x, p)
        self.assertTrue(f.micro_channel_bull.iloc[-1])
        self.assertEqual(f.micro_channel_bull_length.iloc[-1], p.micro_max_bars)
        self.assertTrue(f.micro_bull_climactic.iloc[-1])
        self.assertTrue(compute_features(mirror(x), p).micro_channel_bear.iloc[-1])
        flat = compute_features(bars([(100, 101, 99, 100)] * 35), p)
        self.assertFalse(flat.micro_channel_bull.any())
        self.assertFalse(flat.micro_channel_bear.any())

    def test_micro_gap_delayed_and_measuring_gap_on_pullback(self):
        base = [(100, 101, 99, 100)] * 12
        x = bars(base + [(100, 104, 100, 104), (105, 108, 105, 108), (106, 107, 105, 107)])
        f = compute_features(x, short_params())
        self.assertTrue(f.micro_gap_bull.iloc[-2])
        self.assertTrue(f.measuring_gap_bull.iloc[-1])
        self.assertFalse(f.measuring_gap_bull.iloc[-3])

    def test_h2_failure_is_later_first_violation(self):
        highs = [14, 13, 13.5, 13.6, 12, 12.5, 12.6, 11, 10]
        x = bars([(h - 1, h, h - 2, h - 1) for h in highs])
        f = compute_features(x)
        self.assertTrue(f.h2.iloc[5])
        self.assertFalse(f.h2_failed.iloc[5])
        self.assertEqual(np.flatnonzero(f.h2_failed).tolist(), [7])
        np.testing.assert_array_equal(f.h2_failed, compute_features(mirror(x)).l2_failed)

    def test_strength_directional_fraction_and_absence_need_history(self):
        x = bars([(100 + i, 101 + i, 100 + i, 101 + i) for i in range(60)])
        f = compute_features(x)
        self.assertFalse(f.strength_ready.iloc[37])
        self.assertTrue(f.strength_ready.iloc[38])
        self.assertTrue((f.strength_bull_score.iloc[:38] == 0).all())
        self.assertTrue(f.strength_bull_03.iloc[-1])
        self.assertFalse(f.strength_bear_03.iloc[-1])
        self.assertFalse(f.strength_bull_17.iloc[-1])  # overlap alone is no pullback
        dojis = bars([(100, 102, 99, 100.2)] * 60)
        self.assertFalse(compute_features(dojis).strength_bull_03.iloc[-1])

    def test_final_flag_candidate_breakout_and_failure(self):
        p = short_params(flag_width_atr=3)
        run = [(100 + i, 101 + i, 100 + i, 101 + i) for i in range(20)]
        flag = [(120, 120.5, 119.5, 120), (120, 120.4, 119.6, 120), (120, 120.3, 119.7, 120)]
        x = bars(run + flag + [(120, 122, 120, 122), (122, 122, 119, 120)])
        f = compute_features(x, p)
        self.assertTrue(f.final_flag_bull_candidate.iloc[-3])
        self.assertTrue(f.final_flag_bull_breakout.iloc[-2])
        self.assertTrue(f.final_flag_bull_failure.iloc[-1])
        reflected = compute_features(mirror(x), p)
        self.assertTrue(reflected.final_flag_bear_candidate.iloc[-3])
        self.assertTrue(reflected.final_flag_bear_failure.iloc[-1])


class BrooksContractTests(unittest.TestCase):
    def test_all_columns_causal_at_every_prefix(self):
        x = synthetic(85)
        # No I/O is involved; every output column, every cutoff, including warmup.
        for p in (BrooksParams(), short_params(swing_right=1, strength_window=7)):
            full = compute_features(x, p)
            for t in range(len(x)):
                with self.subTest(t=t, atr=p.atr_period):
                    short = compute_features(x.iloc[:t + 1], p)
                    pd.testing.assert_frame_equal(full.iloc[:t + 1], short, check_exact=True)

    def test_synthetic_finite_after_warmup_input_unchanged_and_score_weights(self):
        x = synthetic(550)
        saved = x.copy(deep=True)
        f = compute_features(x)
        pd.testing.assert_frame_equal(x, saved)
        self.assertEqual(len(f), len(x))
        self.assertFalse(f.loc[f.warmup_ready].isna().any().any())
        self.assertFalse(np.isinf(f.select_dtypes("number").to_numpy()).any())
        self.assertTrue(all(dtype.kind in "bif" for dtype in f.dtypes))
        self.assertEqual(f.strength_max_score.iloc[-1], 28)
        for side in ("bull", "bear"):
            score = sum(f[f"strength_{side}_{n:02d}"].astype(int) * weight for n, (_, weight) in STRENGTH_RULES.items())
            pd.testing.assert_series_equal(f[f"strength_{side}_score"], score, check_names=False)
            np.testing.assert_allclose(f[f"strength_{side}_normalized"], score / 28)
            self.assertTrue(f[f"strength_{side}_normalized"].between(0, 1).all())

    def test_dimensionless_features_scale_and_direction_mirror(self):
        x = synthetic(150)
        a = compute_features(x)
        scaled = x * 1000
        b = compute_features(scaled)
        raw = {"body", "range", "upper_tail", "lower_tail", "true_range", "atr", "ema20", "mean_prior_body", "last_swing_high", "last_swing_low"}
        for col in a.columns:
            if col not in raw:
                np.testing.assert_allclose(a[col], b[col], equal_nan=True, atol=1e-10, err_msg=col)
        reflected = compute_features(mirror(x, 1000))
        for suffix in ("score", "normalized"):
            np.testing.assert_allclose(a["strength_bull_" + suffix], reflected["strength_bear_" + suffix])

    def test_constant_zero_ranges_and_empty_schema(self):
        x = bars([(100, 100, 100, 100)] * 80)
        f = compute_features(x)
        self.assertTrue(f.is_doji.all())
        self.assertTrue((f.always_in == 0).all())
        self.assertFalse(f.loc[f.warmup_ready].isna().any().any())
        empty = compute_features(x.iloc[:0])
        self.assertEqual(list(empty.columns), list(f.columns))
        self.assertTrue(empty.empty)

    def test_validation(self):
        x = synthetic(10)
        for bad in (x.iloc[::-1], pd.concat([x, x]), x.drop(columns="volume"),
                    x.assign(high=x.low - 1), x.assign(close=np.nan), x.assign(volume=-1),
                    x.reset_index(drop=True)):
            with self.subTest(), self.assertRaises(ValueError):
                compute_features(bad)
        for kwargs in (dict(atr_period=0), dict(atr_period=2.5), dict(swing_right=0),
                       dict(strength_trend_fraction=1.1), dict(flag_width_atr=-1), dict(ema_period=True)):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                BrooksParams(**kwargs)

    def test_real_daily_read_only_smoke_and_causality(self):
        root = Path("/Users/zhangyun/workspace/nofx/data/factor_mining/crypto/daily")
        paths = sorted(root.glob("*.json"))
        if not paths:
            self.skipTest("local daily data unavailable; synthetic tests remain mandatory")
        path = paths[0]
        before = path.stat()
        # data_fetch.py: [UTC date, open, high, low, close, volume, vwap].
        with patch("socket.socket", side_effect=AssertionError("network forbidden")):
            with path.open(encoding="utf-8") as handle:
                rows = json.load(handle)
            x = pd.DataFrame(rows, columns=["date", "open", "high", "low", "close", "volume", "vwap"])
            x.index = pd.to_datetime(x.pop("date"), utc=True)
            full = compute_features(x)
            self.assertGreater(len(full), BrooksParams().warmup_bars)
            self.assertFalse(full.loc[full.warmup_ready].isna().any().any())
            for t in (BrooksParams().warmup_bars - 1, len(x) // 2, len(x) - 2):
                pd.testing.assert_frame_equal(full.iloc[:t + 1], compute_features(x.iloc[:t + 1]), check_exact=True)
        after = path.stat()
        self.assertEqual((before.st_size, before.st_mtime_ns), (after.st_size, after.st_mtime_ns))


if __name__ == "__main__":
    unittest.main()
