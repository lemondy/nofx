import contextlib
import io
import random
import unittest
from unittest.mock import patch

try:
    import numpy  # noqa: F401
    import sklearn  # noqa: F401
    from scripts.backtest import mleval
except ImportError:
    mleval = None

MODELS = ["logit_L2", "ridge"]


def make_rows(n, signal, seed=1, n_noise=10):
    """feat f0 在 signal=True 时强预测两个标签;其余特征纯噪声。"""
    rnd = random.Random(seed)
    rows = []
    for i in range(n):
        feat = {f"f{j}": rnd.gauss(0, 1) for j in range(n_noise + 1)}
        base = 2.0 * feat["f0"] if signal else 0.0
        rows.append(dict(t0_ms=i * 6 * 60 * 60_000, label_params={"horizon_h": 72},
                         feat=feat, r_base=base + rnd.gauss(0, 1), r_plain=base + rnd.gauss(0, 1)))
    return rows


def run(rows, **kw):
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        results, line = mleval.run(rows, models=MODELS, seed=3, **kw)
    return results, line, buf.getvalue()


@unittest.skipUnless(mleval is not None, "需要 numpy + scikit-learn")
class MlEvalTest(unittest.TestCase):
    def test_average_ranks_with_ties(self):
        numpy.testing.assert_array_equal(mleval._rank([3, 1, 1, 2]), [3, 0.5, 0.5, 2])
        self.assertAlmostEqual(mleval.spear(numpy.tile([0., 0., 1., 2.], 10),
                                          numpy.tile([0., 1., 1., 2.], 10)), 5 / 6)

    def test_purge_and_embargo_apply_to_models_and_nested(self):
        rows = make_rows(300, True)
        # Serially correlated labels and heterogeneous holding horizons.
        y = 0.0
        for i, row in enumerate(rows):
            y = 0.95 * y + row['r_base']
            row['r_base'] = y
            row['label_params']['horizon_h'] = 120 if i % 5 == 0 else 24
        d = mleval.Data(rows[::-1], 'r_base')
        seen, original = [], mleval.purged_train_indices
        def checked(data, a, embargo_ms=mleval.EMBARGO_MS):
            tr = original(data, a, embargo_ms)
            self.assertTrue(len(tr))
            self.assertTrue(numpy.all(data.t0_ms[tr] + data.horizon_ms[tr] <=
                                      data.t0_ms[a] - embargo_ms))
            self.assertTrue(numpy.all(data.t0_ms[tr] < data.t0_ms[a]))
            self.assertLess(len(tr), a)
            seen.append(a)
            return tr
        with patch.object(mleval, 'purged_train_indices', side_effect=checked):
            pred, start = mleval.walk_forward(d, d.y, d.yb, mleval.model_factories()['ridge'], 3, 0.4)
            self.assertTrue(numpy.isfinite(pred[start:]).all())
            self.assertTrue(numpy.isnan(pred[:start]).all())
            mleval.run_nested(d, 3, 0.4, 7, out=lambda _: None)
        self.assertEqual(seen[:3], seen[3:])
        # Endpoint equality is retained without embargo; an extra millisecond
        # of label overlap is purged.
        d.horizon_ms[:] = 24 * mleval.HOUR_MS
        tr = original(d, 100, 0)
        self.assertIn(96, tr)
        d.horizon_ms[96] += 1
        self.assertNotIn(96, original(d, 100, 0))

    def test_daily_blocks_preserve_dependence_and_widen_null(self):
        rng = numpy.random.default_rng(20261028)
        # Strong intraday autocorrelation, independent daily shocks.
        yy = numpy.repeat(rng.normal(size=80), 24)
        times = numpy.arange(len(yy)) * mleval.HOUR_MS
        keep = numpy.tile(numpy.r_[numpy.ones(10 * 24, bool), numpy.zeros(10 * 24, bool)], 4)
        block, iid = [], []
        for _ in range(1000):
            perm = mleval.block_permutation(times, rng)
            reshaped = perm.reshape(-1, 24)
            numpy.testing.assert_array_equal(numpy.diff(reshaped, axis=1), numpy.ones((80, 23), int))
            self.assertEqual(len(numpy.unique(perm)), len(yy))
            block.append(yy[perm][keep].sum())
            iid.append(yy[rng.permutation(len(yy))][keep].sum())
        self.assertGreaterEqual(numpy.var(block), numpy.var(iid))

    def test_short_history_cannot_pass_after_purge(self):
        rows = make_rows(80, True)
        for i, row in enumerate(rows):
            row['t0_ms'] = i * 60_000
        d = mleval.Data(rows, 'r_base')
        pred, _ = mleval.walk_forward(d, d.y, d.yb, mleval.model_factories()['ridge'], 3, 0.4)
        self.assertTrue(numpy.isnan(pred).all())
        result = mleval.run_models(d, 3, 0.4, 3, numpy.random.default_rng(1), ['ridge'], out=lambda _: None)
        self.assertEqual(result['ridge']['p50'], 1.0)

    def test_strong_feature_passes(self):
        results, line, text = run(make_rows(300, True), perms=400)
        self.assertTrue(line.startswith("PASS: candidate filter"), line)
        for lab in mleval.LABELS:
            r = results[lab]
            self.assertTrue(all("f0" in c for c in r["nested"]["chosen"]), r["nested"]["chosen"])
            self.assertLess(r["nested"]["p"], 0.005)
            self.assertLess(min(r["models"]["ridge"]["p70"], r["models"]["ridge"]["p50"]), 0.01)
        self.assertIn("检验数=10", text)  # 2 个 label × (2 模型 × 2 阈值 + 1 嵌套)

    def test_pure_noise_has_no_deployable_filter(self):
        _, line, _ = run(make_rows(300, False), perms=200)
        self.assertEqual(line, "NO DEPLOYABLE FILTER")

    def test_single_label_cannot_pass(self):
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            _, line = mleval.run(make_rows(300, True), ("r_base",), models=MODELS, seed=3, perms=400)
        self.assertTrue(line.startswith("NO DEPLOYABLE FILTER"), line)

    def test_too_few_perms_warns_and_cannot_pass(self):
        _, line, text = run(make_rows(300, True), perms=50)
        self.assertIn("--perms >=", text)
        self.assertEqual(line, "NO DEPLOYABLE FILTER")

    def test_coverage_filter_drops_sparse_features(self):
        rows = make_rows(120, True)
        for r in rows[:60]:
            r["feat"]["f1"] = None  # 覆盖率 50%
        d = mleval.Data(rows, "r_base", 0.8)
        self.assertNotIn("f1", d.keys)
        self.assertIn("f0", d.keys)


if __name__ == "__main__":
    unittest.main()
