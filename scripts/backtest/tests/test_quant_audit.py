"""Signal timing, causality and reusable audit helpers; deterministic offline data."""

import json
import math
import random
import unittest

from scripts.backtest import quant_audit as audit


def random_walk(n=4000, seed=7):
    rng = random.Random(seed)
    closes = [100.0]
    for _ in range(n):
        closes.append(closes[-1] * math.exp(rng.gauss(0.0006, 0.02)))
    return closes


def future_signal(closes):
    return [int(closes[i + 1] > closes[i]) for i in range(len(closes) - 1)] + [0]


def old_apply(closes, positions):
    """Reproduce the original same-bar implementation only for the regression test."""
    return [(math.log(closes[i] / closes[i - 1]) if positions[i] else 0.04 / 365)
            - (0.001 / 2 if positions[i] != positions[i - 1] else 0)
            for i in range(1, len(closes))]


class TimingTests(unittest.TestCase):
    def test_previous_close_exposure_and_cost_on_effective_bar(self):
        closes = [100, 110, 121, 133.1, 146.41, 161.051]
        positions = [0, 1, 1, 0, 1, 0]
        actual = audit.apply_positions(closes, positions, rf_annual=0.0365, cost_rt=0.002)
        expected = [0.0001, math.log(1.1) - 0.001, math.log(1.1),
                    0.0001 - 0.001, math.log(1.1) - 0.001]
        for i, (value, want) in enumerate(zip(actual, expected), 1):
            with self.subTest(bar=i):
                self.assertAlmostEqual(value, want)

    def test_initial_entry_is_charged_and_final_signal_is_not_executed(self):
        actual = audit.apply_positions([100, 110, 121], [1, 0, 1], rf_annual=0, cost_rt=0.002)
        self.assertAlmostEqual(actual[0], math.log(1.1) - 0.001)
        self.assertEqual(actual[1], -0.001)
        self.assertEqual(audit.apply_positions([100], [1]), [])
        self.assertEqual(audit.apply_positions([], []), [])
        with self.assertRaises(ValueError):
            audit.apply_positions([100, 110], [1])

    def test_both_checks_detect_explicit_future_data(self):
        closes = random_walk()
        positions = future_signal(closes)
        self.assertTrue(audit.flag_lookahead_signal(future_signal, closes))
        self.assertTrue(audit.flag_lookahead(positions, closes))

    def test_causal_200_day_trend_passes_both_checks(self):
        closes = random_walk()
        positions = audit.trend_rule_positions(closes)
        self.assertTrue(any(positions))
        self.assertFalse(audit.flag_lookahead_signal(audit.trend_rule_positions, closes))
        self.assertFalse(audit.flag_lookahead(positions, closes))

    def test_truncation_checks_late_cuts_and_validates_shape(self):
        closes = [100, 99, 98, 97, 96, 95, 96]
        self.assertTrue(audit.flag_lookahead_signal(future_signal, closes, cuts=[6]))
        with self.assertRaises(ValueError):
            audit.flag_lookahead_signal(lambda c: [0], closes)
        with self.assertRaises(ValueError):
            audit.flag_lookahead_signal(audit.trend_rule_positions, closes, cuts=[len(closes)])

    def test_same_bar_leak_disappears_under_next_bar_execution(self):
        closes = random_walk()
        # These close-time signals leak the current return under old execution.
        positions = [0] + [int(closes[i] > closes[i - 1]) for i in range(1, len(closes))]
        old = audit.sharpe(old_apply(closes, positions), 365)
        fixed = audit.sharpe(audit.apply_positions(closes, positions), 365)
        self.assertGreater(old, 10)
        self.assertLess(abs(fixed), 1)
        self.assertGreater(old - fixed, 10)
        # An explicit next-bar oracle still leaks after fixing execution timing.
        # The truncation check, rather than execution lag alone, must catch it.
        future = future_signal(closes)
        self.assertGreater(audit.sharpe(audit.apply_positions(closes, future), 365), 10)
        self.assertLess(abs(audit.sharpe(old_apply(closes, future), 365)), 1)


class VerdictTests(unittest.TestCase):
    def test_insufficient_windows_only_return_counts(self):
        for n in (0, 1, 7, 39, 40, 47):
            with self.subTest(n=n):
                result = audit.verdict([0.001] * n, [0] * n, 365)
                self.assertEqual(result.verdict, audit.VERDICT_INSUFFICIENT)
                self.assertFalse(result.detail["sample_sufficient"])
                self.assertEqual(result.detail["train_n"], int(n * 0.6))
                self.assertEqual(result.detail["holdout_n"], n - int(n * 0.6))
                self.assertEqual(result.detail["required"], 48)
                self.assertEqual(result.detail["min_window_periods"], 20)
                self.assertNotIn("train_cagr", result.detail)
                self.assertIn("20", str(result))
        self.assertNotEqual(audit.verdict([0.001] * 48, [0] * 48, 365).verdict,
                            audit.VERDICT_INSUFFICIENT)

    def test_three_verdicts_with_sufficient_samples(self):
        positive = [0.001, 0.002] * 60
        negative = [-0.001, -0.002] * 60
        flat = [0.0] * 120
        for system, benchmark, expected in (
            (positive, flat, audit.VERDICT_VALIDATED),
            (negative, flat, audit.VERDICT_NO_EDGE),
            ([r * 2 for r in positive], positive, audit.VERDICT_TRADEOFF),
        ):
            # A larger mean and identical Sharpe wins CAGR but not Sharpe.
            with self.subTest(expected=expected):
                result = audit.audit_returns(system, benchmark, 365)
                self.assertEqual(result["verdict"], expected)
                self.assertTrue(result["sample_sufficient"])
                for window in ("train", "holdout"):
                    for metric in ("cagr", "sharpe", "cagr_bench", "sharpe_bench", "n", "n_bench"):
                        self.assertIn(f"{window}_{metric}", result)
                json.dumps(result, allow_nan=False)

    def test_finite_filter_is_paired_counted_and_order_preserving(self):
        system = [float("nan"), 0.001, 0.002, 0.003, float("inf"), 0.004, 0.005, 0.006]
        benchmark = [0.9, 0, 0, float("inf"), 0, 0, float("nan"), 0]
        result = audit.audit_returns(system, benchmark, 365, train_frac=0.5, min_window_periods=2)
        self.assertEqual(result["filtered"], {"system": 2, "benchmark": 2, "pairs": 4})
        self.assertEqual(result["n_periods"], 4)
        self.assertEqual(result["train_n"], 2)
        self.assertEqual(result["holdout_n"], 2)
        self.assertAlmostEqual(result["train_cagr"], audit.cagr([0.001, 0.002], 365))
        self.assertAlmostEqual(result["holdout_cagr"], audit.cagr([0.004, 0.006], 365))
        self.assertEqual(audit.time_split(list(range(10))), (list(range(6)), list(range(6, 10))))
        json.dumps(result, allow_nan=False)

    def test_minimum_can_be_configured_and_invalid_inputs_rejected(self):
        self.assertEqual(audit.verdict([0] * 4, [0] * 4, 365, 0.5, 2).verdict, audit.VERDICT_NO_EDGE)
        for args in (([0], [], 365), ([], [], 365, 1), ([], [], 365, 0.6, 0), ([], [], 0)):
            with self.subTest(args=args), self.assertRaises(ValueError):
                audit.verdict(*args)
        self.assertEqual(audit.sharpe([], 365), 0)
        self.assertEqual(audit.sharpe([0.1], 365), 0)


class RankTieTests(unittest.TestCase):
    def test_average_ranks_and_tied_correlation(self):
        self.assertEqual(audit._ranks([3, 1, 1, 2]), [3, 0.5, 0.5, 2])
        self.assertAlmostEqual(audit._spearman([0, 0, 1, 2], [0, 1, 1, 2]), 5 / 6)
        self.assertEqual(audit._spearman([1] * 20, list(range(20))), 0)

    def test_constant_sizes_do_not_flag_martingale_in_200_noise_trials(self):
        import random
        rng = random.Random(20261028)
        flags = sum(audit.flag_martingale([1.0] * 200, [rng.gauss(0, 1) for _ in range(200)])
                    for _ in range(200))
        self.assertLessEqual(flags, 2)

    def test_increasing_size_after_losses_is_still_detected(self):
        returns = [-1.0, -1.0, 1.0, -1.0] * 50
        streak, sizes = 0, [1.0]
        for ret in returns[:-1]:
            streak = streak + 1 if ret < 0 else 0
            sizes.append(1.0 + streak)
        self.assertTrue(audit.flag_martingale(sizes, returns))


class SweepTests(unittest.TestCase):
    def test_plateau_and_lone_peak(self):
        grid = {i: {"x": i} for i in range(5)}
        plateau = audit.sweep_plateau(lambda p: 1 - abs(p["x"] - 2) / 3, grid, 2)
        peak = audit.sweep_plateau(lambda p: 1 if p["x"] == 2 else -1, grid, 2)
        self.assertEqual(plateau.plateau_ratio, 1)
        self.assertFalse(plateau.lone_peak)
        self.assertEqual(peak.plateau_ratio, 0.2)
        self.assertTrue(peak.lone_peak)
        self.assertEqual(set(peak.grid), set(grid))
        self.assertFalse(audit.sweep_plateau(lambda p: 0, grid, 2).lone_peak)
        self.assertFalse(audit.sweep_plateau(lambda p: 1 if p["x"] == 2 else -1, grid, 2,
                                            min_pass_ratio=0.2).lone_peak)
        self.assertFalse(audit.sweep_plateau(lambda p: 1, grid, 2, score_threshold=1).lone_peak)

    def test_grid_named_or_tuple_preserves_integer_and_sign_rules(self):
        defaults = {"window": 9, "exit": -0.03}
        named = audit.grid_around(defaults)
        self.assertEqual(len(named), 25)
        self.assertEqual(named[(9, -0.03)], defaults)
        self.assertEqual({key[0] for key in named}, set(audit._grid(9, cast=int)))
        self.assertEqual({key[1] for key in named}, set(audit._grid(-0.03)))
        self.assertTrue(all(type(p["window"]) is int and p["exit"] < 0 for p in named.values()))
        tuples = audit.grid_around((9, -0.03))
        self.assertEqual(set(tuples), set(named))
        self.assertTrue(all(key == value for key, value in tuples.items()))

    def test_trend_sweep_preserves_result_interface_and_output(self):
        closes = random_walk(500)
        result = audit.sweep_trend_rule(closes, 365, defaults=(20, 0.01, -0.03))
        self.assertIsInstance(result, audit.SweepResult)
        self.assertEqual(result.default_key, (20, 0.01, -0.03))
        self.assertEqual(len(result.grid), 125)
        for (ma, enter, exit_), score in result.grid.items():
            want = audit.sharpe(audit.apply_positions(closes, audit.trend_rule_positions(closes, ma, enter, exit_)), 365)
            self.assertAlmostEqual(score, want)
        self.assertIn("sharpe=", str(result))
        self.assertIn("邻域通过率", str(result))
        # Original steps=2 omits the default point; keep that API behavior.
        partial = audit.sweep_trend_rule(closes, 365, defaults=(20, 0.01, -0.03), steps=2)
        self.assertEqual(len(partial.grid), 8)
        self.assertNotIn(partial.default_key, partial.grid)
        self.assertFalse(partial.lone_peak)


if __name__ == "__main__":
    unittest.main()
