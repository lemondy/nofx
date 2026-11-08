import json
import math
from pathlib import Path
from statistics import NormalDist
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import ic_overlap_audit as audit
from scripts.factor_mining.tests.synth import SIGNAL_EXPR, planted


class IcOverlapAuditTest(unittest.TestCase):
    def test_stats_include_hac_and_matching_offset_zero_evaluator_z(self):
        ic = pd.Series(np.linspace(-.5, .8, 150))
        ic.iloc[[0, 6, 30]] = np.nan
        h = 3
        sample = ic.to_numpy()[::h]
        sample = sample[np.isfinite(sample)]
        raw = audit.plain_t(sample)
        expected = -NormalDist().inv_cdf(audit.evaluate.student_t_sf(raw, len(sample) - 1))
        self.assertAlmostEqual(audit.nonoverlap_z(ic, h), expected)
        self.assertAlmostEqual(audit.nonoverlap_z(-ic, h), -expected)
        stats = audit.stats(ic, h)
        self.assertNotIn('overlap', stats)
        self.assertEqual(stats['hac'], audit.uniform_hac_t(ic, h))
        self.assertEqual(stats['evaluator'], expected)
        self.assertEqual(stats['nonoverlap_z'], expected)
        self.assertTrue(math.isnan(audit.nonoverlap_z(ic.iloc[:5], h)))
        self.assertTrue(math.isnan(audit.nonoverlap_z(np.ones(30), h)))
        self.assertEqual(audit.evaluate.overlap_t(np.ones(30), h), 1e12)

    def test_null_and_accepted_report_all_conventions_and_hac_fallback_counts(self):
        panels = planted(rows=300, columns=15)
        variance = audit.uniform_hac_variance

        def forced_fallback(x, h):
            return variance(x, h)[0], True

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'crypto').mkdir()
            zoo = dict(factors=[dict(id='signal', expr=SIGNAL_EXPR, direction=1, horizon=3,
                                     status='accepted', train=dict(t=3.), val=dict(t=2.))])
            (root / 'crypto' / 'zoo.json').write_text(json.dumps(zoo), encoding='utf-8')
            out = root / 'audit.json'
            with patch.object(audit, 'ROOT', root), \
                 patch.object(audit, 'load_panels', return_value=panels), \
                 patch.object(audit.sys, 'argv', ['audit', 'crypto', '2', str(out)]), \
                 patch.object(audit, 'uniform_hac_variance', side_effect=forced_fallback):
                audit.main()
            result = json.loads(out.read_text())
        self.assertEqual(len(result['null_summary']), 6)
        for segment in ('train', 'val'):
            stats = result['accepted'][0][segment]
            self.assertNotIn('overlap', stats)
            self.assertIn('hac', stats)
            self.assertEqual(stats['evaluator'], stats['nonoverlap_z'])
        for key, summary in result['null_summary'].items():
            self.assertEqual(summary['fallback_count'], 2)
            self.assertNotIn('overlap', summary)
            self.assertEqual(result['null'][key]['evaluator'], result['null'][key]['nonoverlap_z'])
            for name in ('hac', 'nonoverlap_z', 'evaluator'):
                self.assertEqual(len(result['null'][key][name]), 2)
                self.assertEqual(set(summary[name]), {'rej196', 'rej165_onesided', 'sd'})

    def test_evaluator_matches_alternative_b_for_series_and_arrays(self):
        rng = np.random.default_rng(20261108)
        for h in (1, 3, 5, 7, 10, 20):
            for n in (101, 301):
                x = rng.normal(.01, .2, n)
                x[[0, 6, 30]] = np.nan
                self.assertGreaterEqual(np.isfinite(x[::h]).sum(), 5)
                for ic in (x, pd.Series(x)):
                    with self.subTest(h=h, n=n, kind=type(ic).__name__):
                        self.assertEqual(audit.evaluate.overlap_t(ic, h), audit.nonoverlap_z(ic, h))
                        self.assertEqual(audit.evaluate.overlap_t(-ic, h), audit.nonoverlap_z(-ic, h))
        with patch.object(audit.evaluate, 'student_t_sf', return_value=0.):
            self.assertEqual(audit.evaluate.overlap_t(x, 20), audit.nonoverlap_z(x, 20))

    def test_uniform_hac_variance_matches_archived_formula_and_clips_lags(self):
        x = np.array([1., 3., 2., 4., 5.])
        centered = x - x.mean()
        for h in (1, 3, 10):
            variance = np.dot(centered, centered) / len(x)
            variance += 2 * sum(np.dot(centered[k:], centered[:-k]) / len(x)
                                for k in range(1, min(h, len(x) - 1) + 1))
            fallback = variance <= 1e-30
            if fallback:
                variance = np.dot(centered, centered) / len(x)
                variance += 2 * sum((1 - k / (2 * h + 1)) *
                                    np.dot(centered[k:], centered[:-k]) / len(x)
                                    for k in range(1, min(2 * h, len(x) - 1) + 1))
            actual, used_fallback = audit.uniform_hac_variance(x, h)
            self.assertEqual(used_fallback, fallback)
            self.assertAlmostEqual(actual, variance)
        self.assertEqual(audit.uniform_hac_variance([], 3), (0., True))
        variance, fallback = audit.uniform_hac_variance(np.tile([-1., 1.], 50) + .2, 3)
        self.assertTrue(fallback)
        self.assertGreater(variance, 0.)

    def test_uniform_hac_t_preserves_archived_df_nan_compaction_and_boundaries(self):
        x = np.arange(20, dtype=float) - 9
        with patch.object(audit, 'uniform_hac_variance', return_value=(5., False)), \
             patch.object(audit.evaluate, 'student_t_sf', return_value=1e-50) as sf:
            self.assertAlmostEqual(audit.uniform_hac_t(x, 3), -NormalDist().inv_cdf(1e-50))
            sf.assert_called_once_with(1., 5)
        with patch.object(audit, 'uniform_hac_variance', return_value=(5., False)), \
             patch.object(audit.evaluate, 'student_t_sf', return_value=0.):
            self.assertEqual(audit.uniform_hac_t(x, 30), 1.)
        with patch.object(audit.evaluate, 'student_t_sf', return_value=.5) as sf:
            audit.uniform_hac_t(x, 30)
            self.assertEqual(sf.call_args.args[1], 1)
        for n in (0, 1, 19):
            self.assertEqual(audit.uniform_hac_t(np.ones(n), 3), 0.)
        for mean, expected in ((1., 1e12), (-1., -1e12), (0., 0.)):
            self.assertEqual(audit.uniform_hac_t(np.full(30, mean), 3), expected)
        with_nans = np.insert(x, [3, 7, 10], np.nan)
        self.assertEqual(audit.uniform_hac_t(with_nans, 3), audit.uniform_hac_t(x, 3))
        self.assertAlmostEqual(audit.uniform_hac_t(-x, 3), -audit.uniform_hac_t(x, 3))

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
