"""Preregistered forward study: synthetic inputs only, no network or data cache writes."""
import contextlib
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import ensemble_forward_study as s
from scripts.factor_mining.panel import Panels, forward_labels


def synthetic(n=50, names=10):
    index = pd.date_range('2026-05-01', periods=n)
    columns = pd.Index(['BTCUSDT'] + [f'COIN{i}USDT' for i in range(1, names)])
    rng = np.random.default_rng(123)
    prices = 100 * np.exp(np.cumsum(rng.normal(.0001, .004, (n, names)), axis=0))
    close = pd.DataFrame(prices, index=index, columns=columns)
    fields = dict(open=close.copy(), close=close.copy(), high=close * 1.01, low=close * .99,
                  volume=close * 0 + 1000, funding=close * 0)
    mask = pd.DataFrame(True, index=index, columns=columns)
    score = pd.DataFrame(rng.normal(size=(n, names)), index=index, columns=columns)
    return Panels(fields, mask), score


def snapshots(root):
    return {str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest()
            for p in root.rglob('*') if p.is_file()}


class TargetTests(unittest.TestCase):
    def test_selection_equal_weight_net_gross_schedule_and_pre_F(self):
        p, score = synthetic(n=13, names=10)
        score.iloc[:] = np.arange(10)
        start = score.index[2]
        weights = s.build_targets(score, p.mask, start, 3)
        np.testing.assert_array_equal(weights.iloc[:2], 0)
        expected = [-.25, -.25, 0, 0, 0, 0, 0, 0, .25, .25]
        np.testing.assert_allclose(weights.iloc[2], expected)
        np.testing.assert_allclose(weights.loc[start:].sum(axis=1), 0, atol=1e-15)
        np.testing.assert_allclose(weights.loc[start:].abs().sum(axis=1), 1)
        np.testing.assert_array_equal(weights.iloc[2:5], np.tile(expected, (3, 1)))

    def test_only_scores_available_at_signal_and_mask_are_used(self):
        p, score = synthetic(n=13)
        first = s.build_targets(score, p.mask, score.index[2], 4)
        score.iloc[3:] *= -10000
        second = s.build_targets(score, p.mask, score.index[2], 4)
        pd.testing.assert_frame_equal(first.iloc[:6], second.iloc[:6])
        self.assertFalse(first.iloc[6:].equals(second.iloc[6:]))
        score.iloc[2, 0] = np.inf
        p.mask.iloc[2, 1] = False
        third = s.build_targets(score, p.mask, score.index[2], 4)
        self.assertEqual(third.iloc[2, 0], 0)
        self.assertEqual(third.iloc[2, 1], 0)

    def test_small_universe_disjoint_legs_and_cash_when_impossible(self):
        for n in (2, 3, 4):
            weights = s.rank_weights(np.arange(n), np.ones(n, bool))
            self.assertEqual(weights[0], -.5)
            self.assertEqual(weights[-1], .5)
            self.assertEqual(weights.sum(), 0)
            self.assertEqual(np.abs(weights).sum(), 1)
        np.testing.assert_array_equal(s.rank_weights([1], [True]), [0])
        np.testing.assert_array_equal(s.rank_weights([1, 2], [False, False]), [0, 0])

    def test_stable_ties_and_33_percent_floor(self):
        weights = s.rank_weights(np.ones(10), np.ones(10, bool), .33)
        np.testing.assert_allclose(weights[:3], -1 / 6)
        np.testing.assert_array_equal(weights[3:7], 0)
        np.testing.assert_allclose(weights[7:], 1 / 6)


class ForwardTests(unittest.TestCase):
    def test_forward_segment_only_at_or_after_F_and_finite_rows(self):
        p, _ = synthetic(n=20)
        start = p.mask.index[5]
        excess, _ = forward_labels(p, 3, start)
        self.assertTrue(excess.iloc[:5].isna().all().all())
        excess.iloc[7] = np.nan
        excess.iloc[8] = np.inf
        segment = s.forward_segment(excess, start)
        self.assertFalse(segment.iloc[:5].any())
        self.assertFalse(segment.iloc[7])
        self.assertFalse(segment.iloc[8])
        self.assertFalse(segment.iloc[-4:].any())
        self.assertTrue(segment.iloc[5])

    def test_A_does_not_read_pre_F_labels_even_poisoned(self):
        p, score = synthetic(n=60, names=20)
        start = score.index[10]
        excess, raw = forward_labels(p, 3, start)
        accrued = s.holding_funding(p.fields['funding'], 3)
        before = s.signal_metrics(score, excess, raw, p.mask, accrued, 3, start, s.config.MARKETS['crypto'])
        excess.iloc[:10] = np.arange(20) * 1e200
        raw.iloc[:10] = -1e200
        original = s.evaluate.cs_rank_ic
        with patch.object(s.evaluate, 'cs_rank_ic', wraps=original) as ic:
            after = s.signal_metrics(score, excess, raw, p.mask, accrued, 3, start, s.config.MARKETS['crypto'])
        self.assertEqual(before, after)
        self.assertGreaterEqual(ic.call_args.args[1].index.min(), start)


class SimulationTests(unittest.TestCase):
    def test_bootstrap_reproducible(self):
        returns = np.random.default_rng(5).normal(.001, .02, 80)
        a = s.block_bootstrap(returns)
        self.assertEqual(a, s.block_bootstrap(returns))
        self.assertEqual(a['repetitions'], 2000)
        self.assertEqual(a['omitted_tail_days'], 3)

    def test_placebo_reproducible(self):
        p, score = synthetic()
        args = (p, score, score.index[3], score.index[-1], 3)
        a = s.vectorized_replica(*args, permutations=20)
        b = s.vectorized_replica(*args, permutations=20)
        self.assertEqual(a['placebo_total_returns'], b['placebo_total_returns'])
        self.assertEqual(a['percentile'], b['percentile'])
        self.assertGreater(len(set(a['placebo_total_returns'])), 1)

    def test_replica_matches_native_total_without_funding(self):
        p, score = synthetic(n=80)
        start, end = score.index[2], score.index[-1]
        native, _, _ = s.backtest(p, score, start, end, 3)
        replica = s.vectorized_replica(p, score, start, end, 3, include_funding=False)
        self.assertLess(abs(native['metrics']['total_return'] - replica['main_total_return']), .00005)
        self.assertEqual(native['execution_audit']['unexpected_fills'], 0)
        self.assertEqual(native['execution_audit']['delayed_fills'], 0)

    def test_unchanged_target_has_no_drift_trades(self):
        p, score = synthetic(n=25)
        score.iloc[:] = np.arange(score.shape[1])
        native, _, _ = s.backtest(p, score, score.index[2], score.index[-1], 3)
        self.assertEqual(native['metrics']['trades'], 4)
        self.assertEqual(native['execution_audit']['scheduled_execution_days'], 1)

    def test_replica_funding_includes_entry_day_fixed_nominal(self):
        p, score = synthetic(n=8, names=2)
        for key in ('open', 'high', 'low', 'close'):
            p.fields[key].iloc[:] = 100
        score.iloc[:] = [0, 1]
        p.fields['funding'].iloc[:] = [0, .001]
        replica = s.vectorized_replica(p, score, score.index[2], score.index[-1], 3, cost=0)
        # Five held days, long nominal 500, 3 settlements/day, no short charge.
        self.assertAlmostEqual(replica['main_total_return'], -7.5 / 1000)
        self.assertLess(replica['holding_funding_check_error'], 1e-15)

    def test_missing_open_preserves_native_rules_and_is_reported(self):
        p, score = synthetic(n=20)
        score.iloc[:] = np.arange(score.shape[1])
        for name in ('open', 'high', 'low', 'close', 'volume'):
            p.fields[name].iloc[3, -1] = np.nan
        native, _, _ = s.backtest(p, score, score.index[2], score.index[-1], 3)
        self.assertEqual(native['execution_audit']['missing_open_intents'], 1)
        self.assertEqual(native['execution_audit']['delayed_fills'], 0)
        self.assertEqual(native['execution_audit']['unexpected_fills'], 0)


class InputAndCliTests(unittest.TestCase):
    def write_snapshots(self, root, missing=False):
        crypto = root / 'crypto'
        crypto.mkdir(parents=True)
        frozen = {'horizons': {str(h): dict(factor_ids=['same_id'], train={}, val={}) for h in s.HORIZONS}}
        zoo = dict(generated_at='2026-10-31T00:00:00Z', factors=[
            dict(id='same_id', horizon=h, expr='$close', direction=1) for h in s.HORIZONS if not (missing and h == 3)])
        (crypto / 'ensemble.json').write_text(json.dumps(frozen))
        (crypto / 'zoo.json').write_text(json.dumps(zoo))

    def test_missing_constituent_at_same_horizon_is_error(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.write_snapshots(root, missing=True)
            with self.assertRaisesRegex(ValueError, 'missing from zoo at same horizon'):
                s.load_frozen(root)

    def test_snapshot_hash_and_missing_generated_at_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.write_snapshots(root)
            factors, _, provenance = s.load_frozen(root)
            self.assertEqual([len(factors[h]) for h in s.HORIZONS], [1, 1, 1])
            self.assertIsNone(provenance['ensemble']['generated_at'])
            self.assertEqual(provenance['ensemble']['sha256'], hashlib.sha256(
                (root / 'crypto/ensemble.json').read_bytes()).hexdigest())

    def test_CLI_rejects_output_inside_root_before_loading(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with patch.object(s, 'run_study') as study, contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as error:
                    s.main(['--root', str(root), '--out', str(root / 'bad.md'), '--json', str(root / 'bad.json')])
            self.assertEqual(error.exception.code, 2)
            study.assert_not_called()
            self.assertEqual(list(root.iterdir()), [])

    def test_CLI_real_synthetic_root_is_unchanged(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            root = base / 'read_only_input'
            self.write_snapshots(root)
            crypto = root / 'crypto'
            (crypto / 'daily').mkdir()
            (crypto / 'alt_daily/funding').mkdir(parents=True)
            dates = pd.date_range('2021-01-01', '2026-10-30')
            for symbol in ('BTCUSDT', 'SYNTHUSDT'):
                rows = [[s.day(d), 100, 101, 99, 100, 1000, 100] for d in dates]
                (crypto / 'daily' / (symbol + '.json')).write_text(json.dumps(rows))
                (crypto / 'alt_daily/funding' / (symbol + '.json')).write_text(
                    json.dumps([[s.day(d), 0] for d in dates]))
            before = snapshots(root)
            out, payload = base / 'report.md', base / 'report.json'
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(s.main(['--root', str(root), '--out', str(out), '--json', str(payload)]), 0)
            self.assertEqual(before, snapshots(root))
            result = json.loads(payload.read_text())
            self.assertEqual(result['design']['F'], s.FORWARD_START)
            self.assertEqual(len(result['A']), 3)
            self.assertEqual(result['B'], [])
            self.assertTrue(out.is_file())

    def test_passing_A_runs_all_native_variants_val_and_500_placebos(self):
        # Deterministic cross-sectional trends exercise the otherwise gated B path.
        dates = pd.date_range('2021-01-01', '2026-10-30')
        columns = ['BTCUSDT'] + [f'SYNTH{i}USDT' for i in range(1, 20)]
        prices = 100 * np.exp(np.arange(len(dates))[:, None] * np.arange(1, 21)[None, :] * .0001)
        close = pd.DataFrame(prices, index=dates, columns=columns)
        fields = dict(open=close.copy(), high=close * 1.01, low=close * .99, close=close.copy(),
                      vwap=close.copy(), volume=close * 0 + 1000, funding=close * 0)
        panels = Panels(fields, close.notna(), meta=dict(end='2026-10-30'),
                        windows=dict(s.config.MARKETS['crypto']['windows']))
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.write_snapshots(root)
            with patch.object(s.panel, 'load_panels', return_value=panels):
                result = s.run_study(root, progress=lambda message: None)
        self.assertTrue(all(a['passed'] for a in result['A']))
        self.assertEqual([b['h'] for b in result['B']], [3, 7])
        for b in result['B']:
            self.assertEqual(len(b['variants']), 4)
            self.assertEqual(len(b['replica']['placebo_total_returns']), 500)
            self.assertEqual(set(b['gates']), {'B1', 'B2', 'B3', 'B4', 'B5', 'B6'})
            self.assertEqual(b['main']['metrics']['n_days'], 180)
            self.assertEqual(b['validation']['metrics']['n_days'], 362)
            self.assertEqual(b['main']['execution_audit']['unexpected_fills'], 0)
            self.assertGreater(b['comparison']['daily_return_correlation'], .99)
        self.assertIn('B6', s.render_report(result))
        json.dumps(s.json_safe(result), allow_nan=False)

    def test_spent_overlap_is_read_only_and_inclusive(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            records = [dict(market='crypto', holdout_range=['2026-04-01', '2026-05-04']),
                       dict(market='crypto', holdout_range=['2025-01-01', '2025-02-01']),
                       dict(market='us', holdout_range=['2026-06-01', '2026-07-01'])]
            (root / 'holdout_spent.jsonl').write_text('\n'.join(json.dumps(x) for x in records))
            before = snapshots(root)
            result = s.spent_intervals(root, s.FORWARD_START, '2026-10-30')
            self.assertEqual(len(result['overlapping_intervals']), 1)
            self.assertEqual(result['overlapping_intervals'][0]['overlap'], ['2026-05-04', '2026-05-04'])
            self.assertEqual(before, snapshots(root))


if __name__ == '__main__':
    unittest.main()
