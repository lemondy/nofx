"""Synthetic tests only, except read-only verbatim comparison with the appendix."""
import contextlib
import io
import json
import operator
from pathlib import Path
import re
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd
from pandas.testing import assert_frame_equal

from scripts.factor_mining import alpha101 as a, alpha101_study as study, evaluate
from scripts.factor_mining.tests.synth import planted


def frame(values):
    values = np.asarray(values, dtype=float)
    if values.ndim == 1:
        values = values[:, None]
    return pd.DataFrame(values, index=pd.date_range('2020-01-01', periods=len(values)))


def random_panel():
    panels = planted(rows=340, columns=23, seed=771)
    # synth.planted uses vwap=close; independent nondegenerate vwap is essential
    # to exercise the many original formulae containing close-vwap.
    rng = np.random.default_rng(741)
    panels.fields['vwap'] = (panels.fields['open'] + panels.fields['close']) / 2
    panels.fields['vwap'] += (panels.fields['high'] - panels.fields['low']) * rng.uniform(-.05, .05, panels.mask.shape)
    return panels


class OperatorTests(unittest.TestCase):
    def test_rank_average_ties_and_all_observed_names(self):
        x = frame([[3, 1, 1, np.nan], [np.nan, 9, 9, np.nan]])
        assert_frame_equal(a.rank(x), frame([[1, .5, .5, np.nan], [np.nan, .75, .75, np.nan]]))

    def test_delay_delta_sum_mean_stddev(self):
        x = frame([1, 2, 4, 8])
        np.testing.assert_allclose(a.delay(x, 2).to_numpy().ravel(), [np.nan, np.nan, 1, 2], equal_nan=True)
        np.testing.assert_allclose(a.delta(x, 2).to_numpy().ravel(), [np.nan, np.nan, 3, 6], equal_nan=True)
        self.assertEqual(a.sum(x, 3).iloc[-1, 0], 14)
        self.assertEqual(a.mean(x, 3).iloc[-1, 0], 14 / 3)
        self.assertAlmostEqual(a.stddev(x, 3).iloc[-1, 0], np.std([2, 4, 8], ddof=1))
        self.assertTrue(a.stddev(x, 1).isna().all().all())

    def test_product_and_full_window_nan_rules(self):
        x = frame([2, 3, 4, np.nan, 8, 9])
        self.assertEqual(a.product(x, 3).iloc[2, 0], 24)
        for op in (a.sum, a.product, a.mean, a.stddev, a.ts_min, a.ts_max,
                   a.ts_rank, a.ts_argmax, a.ts_argmin, a.decay_linear):
            with self.subTest(op=op.__name__):
                got = op(x, 3)
                self.assertTrue(got.iloc[:2].isna().all().all())
                self.assertTrue(got.iloc[3:].isna().all().all())

    def test_ts_rank_average_ties_divides_by_window(self):
        got = a.ts_rank(frame([2, 5, 2, 2]), 3)
        self.assertEqual(got.iloc[2, 0], 1.5 / 3)
        self.assertEqual(got.iloc[3, 0], 1.5 / 3)

    def test_argmax_argmin_oldest_one_today_d_earliest_tie(self):
        x = frame([5, 1, 5, 0])
        self.assertEqual(a.ts_argmax(x, 3).iloc[2, 0], 1)
        self.assertEqual(a.ts_argmax(x, 3).iloc[3, 0], 2)
        self.assertEqual(a.ts_argmin(x, 3).iloc[2, 0], 2)
        self.assertEqual(a.ts_argmin(x, 3).iloc[3, 0], 3)
        self.assertEqual(a.ts_argmin(frame([1, 1, 2]), 3).iloc[-1, 0], 1)

    def test_decay_linear_increases_weight_towards_today(self):
        self.assertEqual(a.decay_linear(frame([2, 5, 8]), 3).iloc[-1, 0], (2 + 10 + 24) / 6)

    def test_scale_absolute_row_sum_and_zero_row(self):
        x = frame([[-2, 1, 3, np.nan], [0, 0, 0, np.nan]])
        got = a.scale(x)
        self.assertAlmostEqual(got.iloc[0].abs().sum(), 1.)
        self.assertTrue(got.iloc[1].isna().all())
        self.assertAlmostEqual(a.scale(x, 3).iloc[0].abs().sum(), 3.)

    def test_signedpower_and_elementwise_power(self):
        x = frame([[-2, -4, 0, 2]])
        assert_frame_equal(a.signedpower(x, 2), frame([[-4, -16, 0, 4]]))
        assert_frame_equal(a.power(x, .5), frame([[np.nan, np.nan, 0, np.sqrt(2)]]))
        assert_frame_equal(a.power(x, frame([[2, .5, 1, 3]])), frame([[4, np.nan, 0, 8]]))

    def test_floor_windows_including_lags_and_minimum_one(self):
        x = frame([2, 5, 8, 4, 1])
        for op in (a.delay, a.delta, a.sum, a.product, a.mean, a.stddev, a.ts_min,
                   a.ts_max, a.ts_rank, a.ts_argmax, a.ts_argmin, a.decay_linear):
            with self.subTest(op=op.__name__):
                assert_frame_equal(op(x, 3.92795), op(x, 3))
        assert_frame_equal(a.correlation(x, x, 3.92795), a.correlation(x, x, 3))
        assert_frame_equal(a.covariance(x, x, 3.92795), a.covariance(x, x, 3))
        assert_frame_equal(a.sum(x, .3), x)

    def test_min_max_dispatch_scalar_vs_series(self):
        x, y = frame([2, 5, 8, 4]), frame([7, 3, 1, np.nan])
        assert_frame_equal(a.min(x, 3.92795), a.ts_min(x, 3))
        assert_frame_equal(a.max(x, 3), a.ts_max(x, 3))
        assert_frame_equal(a.min(x, y), frame([2, 3, 1, np.nan]))
        assert_frame_equal(a.max(x, y), frame([7, 5, 8, np.nan]))

    def test_comparison_or_and_ternary_propagate_nan(self):
        x, y = frame([1, 2, np.nan, 4]), frame([2, 2, 1, np.nan])
        assert_frame_equal(a.lt(x, y), frame([1, 0, np.nan, np.nan]))
        assert_frame_equal(a.gt(x, 1), frame([0, 1, np.nan, 1]))
        assert_frame_equal(a.eq(x, y), frame([0, 1, np.nan, np.nan]))
        cond = a.logical_or(a.lt(x, y), a.eq(x, y))
        assert_frame_equal(cond, frame([1, 1, np.nan, np.nan]))
        assert_frame_equal(a.where(cond, 7, -3), frame([7, 7, np.nan, np.nan]))
        assert_frame_equal(a.where(frame([1, 0, np.nan]), frame([1, 2, 3]), 9), frame([1, 9, np.nan]))

    def test_correlation_covariance_and_constant_windows(self):
        x, y = frame([1, 3, 2, 7]), frame([8, 1, 6, 5])
        self.assertAlmostEqual(a.correlation(x, y, 3).iloc[-1, 0], np.corrcoef([3, 2, 7], [1, 6, 5])[0, 1])
        self.assertAlmostEqual(a.covariance(x, y, 3).iloc[-1, 0], np.cov([3, 2, 7], [1, 6, 5], ddof=1)[0, 1])
        constant = frame([1e12] * 4)
        self.assertTrue(a.correlation(constant, x, 3).isna().all().all())
        self.assertTrue(a.correlation(x, constant, 3).isna().all().all())
        self.assertEqual(a.covariance(constant, x, 3).iloc[-1, 0], 0.)
        y.iloc[-2, 0] = np.nan
        self.assertTrue(np.isnan(a.correlation(x, y, 3).iloc[-1, 0]))
        self.assertTrue(np.isnan(a.covariance(x, y, 3).iloc[-1, 0]))

    def test_indneutralize_nested_log_abs_sign_and_derived_inputs(self):
        x = frame([[1, 3, np.nan], [-3, 0, 3]])
        centered = frame([[-1, 1, np.nan], [-3, 0, 3]])
        assert_frame_equal(a.indneutralize(x, 'sector'), centered)
        assert_frame_equal(a.indneutralize(a.indneutralize(x)), centered)
        assert_frame_equal(a.abs(x), frame([[1, 3, np.nan], [3, 0, 3]]))
        assert_frame_equal(a.sign(x), frame([[1, 1, np.nan], [-1, 0, 1]]))
        assert_frame_equal(a.log(frame([1, np.e, 0, -1])), frame([0, 1, np.nan, np.nan]))
        p = a.Inputs({'close': frame([2, 4, 2]), 'volume': frame([1, 2, 3]), 'vwap': frame([2, 4, 6])})
        assert_frame_equal(p.returns, frame([np.nan, 1, -.5]))
        assert_frame_equal(p.adv(2), frame([np.nan, 5, 13]))


class RegistryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.panels = random_panel()

    def test_registry_complete_metadata_and_cap_skipped(self):
        self.assertEqual(set(a.ALPHAS), set(range(1, 102)))
        for n, item in a.ALPHAS.items():
            self.assertIsInstance(item['formula'], str)
            self.assertTrue(callable(item['fn']))
            self.assertIsInstance(item['uses'], set)
            self.assertIsInstance(item['indneutral'], bool)
            self.assertEqual(item['skipped'], 'needs cap' if n == 56 else None)
        self.assertEqual(a.ALPHAS[56]['skipped'], 'needs cap')
        self.assertEqual(a.ALPHAS[7]['uses'], {'close', 'volume', 'vwap'})
        self.assertTrue(a.ALPHAS[100]['indneutral'])
        with self.assertRaisesRegex(ValueError, 'needs cap'):
            a.ALPHAS[56]['fn'](self.panels.fields)

    def test_all_non_skipped_formulas_shape_finite_and_no_input_mutation(self):
        original = {k: v.copy() for k, v in self.panels.fields.items()}
        for n, item in a.ALPHAS.items():
            if item['skipped']:
                continue
            with self.subTest(alpha=n):
                result = item['fn'](self.panels.fields)
                self.assertIsInstance(result, pd.DataFrame)
                self.assertEqual(result.shape, self.panels.mask.shape)
                self.assertTrue(result.index.equals(self.panels.mask.index))
                self.assertTrue(result.columns.equals(self.panels.mask.columns))
                self.assertTrue(np.isfinite(result.to_numpy()).any())
                self.assertFalse(np.isinf(result.to_numpy()).any())
                self.assertTrue(all(dtype.kind == 'f' for dtype in result.dtypes))
        for key, value in original.items():
            assert_frame_equal(self.panels.fields[key], value)

    def test_every_alpha_causal_including_nan_positions(self):
        perturbed = {k: v.copy() for k, v in self.panels.fields.items()}
        rng = np.random.default_rng(441)
        for values in perturbed.values():
            values.iloc[-10:] *= rng.uniform(.4, 3, size=values.iloc[-10:].shape)
            values.iloc[-4, 2] = np.nan
        for n, item in a.ALPHAS.items():
            if item['skipped']:
                continue
            with self.subTest(alpha=n):
                before = item['fn'](self.panels.fields).iloc[:-10]
                after = item['fn'](perturbed).iloc[:-10]
                assert_frame_equal(before, after, check_exact=True)

    def test_formula_verbatim_local_appendix(self):
        source = Path('/Users/zhangyun/workspace/nofx/data/alpha101/page1.md')
        if not source.exists():
            self.skipTest('local Alpha101 source is unavailable')
        text = source.read_text(encoding='utf-8').split('附录一:世坤Alpha101因子表达式', 1)[1].split('附录二:', 1)[0]
        originals = {int(n): formula for n, formula in re.findall(r'^Alpha#(\d+): (.*)$', text, re.M)}
        self.assertEqual(set(originals), set(range(1, 102)))
        for n, formula in originals.items():
            with self.subTest(alpha=n):
                self.assertEqual(a.ALPHAS[n]['formula'], formula)

    def test_nested_formula_29_95_100_structure(self):
        p = a.Inputs(self.panels.fields)
        x = -a.rank(a.delta(p.close - 1, 5))
        x = a.ts_min(a.rank(a.rank(x)), 2)
        x = a.log(a.sum(x, 1))
        x = a.product(a.rank(a.rank(a.scale(x))), 1)
        expected29 = a.ts_min(x, 5) + a.ts_rank(a.delay(-p.returns, 6), 5)
        assert_frame_equal(a.ALPHAS[29]['fn'](p.fields), expected29)
        corr = a.correlation(a.sum((p.high + p.low) / 2, 19), a.sum(p.adv(40), 19), 12)
        expected95 = a.lt(a.rank(p.open - a.ts_min(p.open, 12)), a.ts_rank(a.power(a.rank(corr), 5), 11))
        assert_frame_equal(a.ALPHAS[95]['fn'](p.fields), expected95)
        imbalance = ((p.close - p.low) - (p.high - p.close)) / (p.high - p.low) * p.volume
        first = a.indneutralize(a.indneutralize(a.rank(imbalance)))
        second = a.correlation(p.close, a.rank(p.adv(20)), 5) - a.rank(a.ts_argmin(p.close, 30))
        expected100 = -(1.5 * a.scale(first) - a.scale(a.indneutralize(second))) * (p.volume / p.adv(20))
        assert_frame_equal(a.ALPHAS[100]['fn'](p.fields), expected100)

    def test_all_manual_translations_match_independent_source_trees(self):
        """Test-only Pratt parser checks nesting/signs/constants for all 100.

        Production never uses this parser; the separate verbatim-source test
        ties these strings to the local paper appendix.
        """
        inputs = a.Inputs(self.panels.fields)
        funcs = {name: getattr(a, name) for name in
                 ('rank', 'delay', 'delta', 'sum', 'product', 'stddev', 'ts_min', 'ts_max', 'ts_rank',
                  'ts_argmax', 'ts_argmin', 'decay_linear', 'scale', 'signedpower', 'indneutralize',
                  'correlation', 'covariance', 'min', 'max', 'abs', 'sign', 'log')}
        operations = {'+': operator.add, '-': operator.sub, '*': operator.mul, '/': operator.truediv,
                      '^': a.power, '<': a.lt, '>': a.gt, '==': a.eq, '||': a.logical_or}
        precedence = {'||': 2, '<': 3, '>': 3, '==': 3, '+': 4, '-': 4, '*': 5, '/': 5, '^': 6}

        class SourceTree:
            def __init__(self, source):
                self.tokens = re.findall(r'\d+(?:\.\d*)?|\.\d+|[A-Za-z_][A-Za-z_0-9.]*|==|\|\||[^\s]', source)
                self.position = 0

            def peek(self):
                return self.tokens[self.position] if self.position < len(self.tokens) else None

            def pop(self):
                token = self.peek()
                self.position += 1
                return token

            def expect(self, token):
                actual = self.pop()
                if actual != token:
                    raise AssertionError(f'expected {token}, got {actual}')

            def expression(self, level=0):
                token = self.pop()
                if token == '-':
                    left = -self.expression(7)
                elif token == '(':
                    left = self.expression()
                    self.expect(')')
                elif re.fullmatch(r'\d+(?:\.\d*)?|\.\d+', token):
                    left = float(token)
                elif self.peek() == '(':
                    self.pop()
                    args = [self.expression()]
                    while self.peek() == ',':
                        self.pop()
                        args.append(self.expression())
                    self.expect(')')
                    left = funcs[token.lower()](*args)
                elif token.lower().startswith('indclass.'):
                    left = token
                elif token.startswith('adv'):
                    left = inputs.adv(int(token[3:]))
                else:
                    left = getattr(inputs, token)
                while True:
                    op = self.peek()
                    if op == '?' and level <= 1:
                        self.pop()
                        yes = self.expression()
                        self.expect(':')
                        left = a.where(left, yes, self.expression(1))
                    elif op in precedence and precedence[op] >= level:
                        self.pop()
                        right = self.expression(precedence[op] + (0 if op == '^' else 1))
                        left = operations[op](left, right)
                    else:
                        return left

        for n, item in a.ALPHAS.items():
            if item['skipped']:
                continue
            with self.subTest(alpha=n), np.errstate(all='ignore'):
                tree = SourceTree(item['formula'])
                expected = a.finite(tree.expression())
                self.assertIsNone(tree.peek())
                assert_frame_equal(item['fn'](inputs.fields), expected, rtol=1e-12, atol=1e-14)


class StudyTests(unittest.TestCase):
    @staticmethod
    def metrics(train_t=10., val_t=4., ls_ret=1.):
        return {'direction': 1, 'train': {'ic': .1, 't': train_t, 'ic_halves': [.1, .1], 'ls_ret': ls_ret},
                'val': {'ic': .1, 't': val_t, 'ls_ret': ls_ret}}

    def test_k_counts_every_ledger_row_and_only_evaluable_study_rows(self):
        ledger = study.ledger_counts([{'horizon': 1, 'id': 'same'}, {'horizon': 1, 'id': 'same'}, {'horizon': 3}])
        rows = [{'horizon': h, 'metrics': metrics} for h, metrics in
                [(1, self.metrics()), (1, None), (3, self.metrics()), (7, None)]]
        self.assertEqual(study.classify_rows(rows, ledger), {1: 3, 3: 2, 7: 0})
        self.assertEqual(rows[0]['status'], 'accepted')
        self.assertEqual(rows[1]['status'], 'not_evaluable')
        self.assertEqual(rows[0]['t_req'], evaluate.t_required(3))

    def test_classification_is_frozen_and_integration_gate(self):
        rows = [{'horizon': 1, 'metrics': m} for m in
                (self.metrics(), self.metrics(train_t=.5), self.metrics(val_t=.5), self.metrics(ls_ret=-1))]
        study.classify_rows(rows, study.ledger_counts([]))
        self.assertEqual([r['status'] for r in rows], ['accepted', 'rejected', 'candidate', 'candidate'])
        self.assertTrue(study.integration_pass('accepted', False, 'us'))
        self.assertTrue(study.integration_pass('accepted', False, 'crypto', True))
        for status, redundant, market, funded in [('accepted', True, 'us', None), ('candidate', False, 'us', None),
                                                   ('accepted', False, 'crypto', None), ('accepted', False, 'crypto', False)]:
            self.assertFalse(study.integration_pass(status, redundant, market, funded))

    def test_holm_stepdown_and_normal_two_sided_p(self):
        self.assertEqual(study.holm_passes([.03, .001, .04, 1.]), [False, True, False, False])
        self.assertEqual(study.holm_passes([.001, .02, .049]), [True, True, True])
        self.assertEqual(study.holm_passes([]), [])

    def test_reject_output_under_root_and_symlink_before_analysis(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            root = base / 'data'
            root.mkdir()
            link = base / 'link'
            link.symlink_to(root, target_is_directory=True)
            with patch.object(study, 'analyse') as analyse, contextlib.redirect_stderr(io.StringIO()):
                for out in (root / 'report.md', link / 'report.md'):
                    with self.assertRaises(SystemExit):
                        study.main(['--root', str(root), '--out', str(out), '--json', str(base / 'out.json')])
                analyse.assert_not_called()
            self.assertEqual(list(root.iterdir()), [])

    def test_study_main_reads_root_only_and_reuses_factor_values(self):
        panels = random_panel()
        panels.meta = dict(market='crypto', start=str(panels.mask.index[0].date()),
                           end=str(panels.mask.index[-1].date()), symbols=len(panels.mask.columns))
        # Funding is synthetic and known; the entire fixture predates 2022.
        panels.fields['funding'] = panels.fields['close'] * 0 + .0001
        calls = []
        original = a.ALPHAS[101]

        def counted(fields):
            calls.append(len(fields['close'].columns))
            return original['fn'](fields)

        with tempfile.TemporaryDirectory() as temp:
            base, root = Path(temp), Path(temp) / 'input'
            (root / 'crypto').mkdir(parents=True)
            (root / 'crypto/zoo.json').write_text('{"factors": []}')
            (root / 'crypto/ledger.jsonl').write_text('{"horizon": 1}\n')
            def snapshot():
                return {str(p.relative_to(root)): (p.read_bytes(), p.stat().st_mtime_ns)
                        for p in root.rglob('*') if p.is_file()}
            before = snapshot()
            registry = {101: {**original, 'fn': counted}, 56: a.ALPHAS[56]}
            with patch.dict(study.ALPHAS, registry, clear=True), patch.object(study, 'load_panels', return_value=panels), \
                    patch.object(study.zoo, 'seed_factors', return_value=[]), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(study.main(['--root', str(root), '--markets', 'crypto',
                                             '--out', str(base / 'out/report.md'), '--json', str(base / 'out/report.json')]), 0)
            self.assertEqual(before, snapshot())
            payload = json.loads((base / 'out/report.json').read_text())
            self.assertEqual(len(payload['markets']['crypto']['rows']), 3)
            self.assertEqual(calls.count(23), 2)  # One full-pool and one old-pool sensitivity evaluation.
            self.assertEqual([r['alpha'] for r in payload['integrable']], [101, 101, 101])
            self.assertIn('holdout', (base / 'out/report.md').read_text())


if __name__ == '__main__':
    unittest.main()
