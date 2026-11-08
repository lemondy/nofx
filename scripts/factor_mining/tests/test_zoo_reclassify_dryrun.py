from collections import Counter
from copy import deepcopy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import data_fetch, evaluate, store, zoo_reclassify_dryrun as dryrun
from scripts.factor_mining.config import MARKETS
from scripts.factor_mining.expr import ExprError, evaluate_expr, expr_hash
from scripts.factor_mining.miner import Miner
from scripts.factor_mining.panel import forward_target, load_panels, make_splits
from scripts.factor_mining.tests.synth import SIGNAL_EXPR, write_market


class ZooReclassifyDryrunTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name) / 'data'
        self.out = Path(self.tmp.name) / 'reports'
        write_market(self.root, rows=340, columns=15)
        self._write_zoo('crypto')
        self.root_patch = patch.object(data_fetch, 'ROOT', self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)

    def _write_zoo(self, market):
        cfg = MARKETS[market]
        panels = load_panels(market, self.root)
        splits = {h: make_splits(panels.mask.index, h, panels.windows) for h in cfg['horizons']}
        factors = {}
        for expr, horizons in ((SIGNAL_EXPR, cfg['horizons']), ('$volume', cfg['horizons'][:1])):
            values = evaluate_expr(expr, panels.fields)
            for h in horizons:
                with patch.object(evaluate, 'overlap_t', side_effect=evaluate.nw_t):
                    metrics = evaluate.evaluate_factor(values, *forward_target(panels, h), panels.mask,
                                                       splits[h], h, cfg)
                factor = dict(id=expr_hash(expr), expr=expr, horizon=h, first_seen='2026-01-01',
                              run_id='fixture', status='rejected', **metrics)
                factors[h, factor['id']] = factor
        ledger = [dict(horizon=h, id=f['id']) for (h, _), f in factors.items()]
        ledger += [dict(horizon=h, id='f_other_trial') for h in cfg['horizons']]
        Miner._classify_and_correlate(factors, Counter(row['horizon'] for row in ledger),
                                      panels, splits, cfg)
        store.write_zoo(market, dict(generated_at='2026-11-08T00:00:00Z', factors=list(factors.values())), self.root)
        store.append_ledger(market, ledger, self.root)
        # These live files must remain byte-for-byte untouched by the dry run.
        for name in ('ensemble.json', 'live_scores.json', 'quant_targets.json', 'status_events.jsonl'):
            (self.root / market / name).write_text('fixture\n', encoding='utf-8')

    def _snapshot_data(self):
        return {str(path.relative_to(self.root)): (path.stat().st_mtime_ns,
                                                  path.read_bytes() if path.is_file() else None)
                for path in [self.root, *self.root.rglob('*')]}

    def _add_factor(self, expr, horizons=(1,)):
        zoo = store.read_json(self.root / 'crypto' / 'zoo.json')
        template = zoo['factors'][0]
        for h in horizons:
            zoo['factors'].append(dict(deepcopy(template), expr=expr, id=expr_hash(expr), horizon=h))
        store.write_zoo('crypto', zoo, self.root)

    def test_cli_writes_reports_with_full_schema_and_keeps_data_unchanged(self):
        before = self._snapshot_data()
        env = dict(os.environ, FACTOR_MINING_DIR=str(self.root), PYTHONDONTWRITEBYTECODE='1')
        env.pop('PYTHONPATH', None)
        result = subprocess.run([sys.executable, '-m', 'scripts.factor_mining.zoo_reclassify_dryrun',
                                 '--market', 'crypto', '--out', str(self.out)],
                                cwd=Path(__file__).resolve().parents[3], env=env,
                                text=True, capture_output=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('重评估表达式', result.stdout)
        report = json.loads((self.out / 'crypto.json').read_text())
        self.assertEqual(report['market'], 'crypto')
        self.assertEqual(report['zoo_generated_at'], '2026-11-08T00:00:00Z')
        self.assertEqual(report['factor_count'], 4)
        self.assertEqual(report['K'], {'1': 3, '3': 2, '7': 2})
        self.assertEqual(report['old_status_sanity'], dict(matches=4, total=4, ratio=1.))
        self.assertEqual(report['missing_field_retained_count'], 0)
        self.assertNotIn('fallback_count', report)
        self.assertNotIn('fallback_count_by_horizon', report)
        for name, path in (('zoo.json', self.root / 'crypto' / 'zoo.json'),
                           ('ledger.jsonl', self.root / 'crypto' / 'ledger.jsonl'),
                           ('evaluate.py', Path(evaluate.__file__))):
            self.assertEqual(report['input_sha256'][name], hashlib.sha256(path.read_bytes()).hexdigest())
        self.assertEqual(sum(sum(row.values()) for row in report['status_transitions'].values()), 4)
        for h, row in report['horizons'].items():
            for convention in ('old', 'new'):
                self.assertEqual(set(row[convention]['counts']), set(dryrun.STATUSES))
                for factor in row[convention]['accepted']:
                    self.assertEqual(set(factor), {'id', 'train_t', 'val_t'})
                self.assertEqual(row['ensemble'][convention],
                                 [f['id'] for f in row[convention]['accepted']][:20])
            self.assertEqual(row['ensemble']['same'], row['ensemble']['old'] == row['ensemble']['new'])
        self.assertTrue(report['horizons']['1']['old']['accepted'])
        markdown = (self.out / 'crypto.md').read_text()
        self.assertIn('入选因子', markdown)
        self.assertNotIn('回退', markdown)
        self.assertEqual(self._snapshot_data(), before)

    def test_us_market_and_limit_keep_full_ledger_K_and_group_expressions(self):
        write_market(self.root, market='us', rows=340, columns=15)
        self._write_zoo('us')
        before = self._snapshot_data()
        with patch.object(dryrun, 'evaluate_expr', wraps=evaluate_expr) as expressions, \
             patch.object(evaluate, 'evaluate_factor', wraps=evaluate.evaluate_factor) as metrics:
            report = dryrun.run_dryrun('us', self.out, limit=2)
        self.assertEqual(expressions.call_count, 1)
        self.assertEqual(metrics.call_count, 4)
        self.assertEqual(report['factor_count'], 2)
        self.assertEqual(report['zoo_factor_count'], 4)
        self.assertEqual(report['K'], {'5': 3, '10': 2, '20': 2})
        self.assertEqual(sum(report['horizons']['20']['old']['counts'].values()), 0)
        self.assertEqual(self._snapshot_data(), before)

    def test_out_inside_data_root_or_symlink_to_it_is_rejected_before_loading(self):
        alias = Path(self.tmp.name) / 'alias'
        alias.symlink_to(self.root, target_is_directory=True)
        before = self._snapshot_data()
        for out in (self.root, self.root / 'reports', alias / 'reports'):
            with self.subTest(out=out), patch.object(dryrun, 'load_panels') as load:
                with self.assertRaisesRegex(ValueError, '数据根之外'):
                    dryrun.run_dryrun('crypto', out)
                load.assert_not_called()
        with self.assertRaises(SystemExit) as error:
            dryrun.main(['--market', 'crypto', '--out', str(self.root)])
        self.assertEqual(error.exception.code, 2)
        self.assertEqual(self._snapshot_data(), before)

    def test_missing_fields_preserve_old_accepted_snapshots_in_both_passes(self):
        self._add_factor('$funding', (1, 3))
        zoo = store.read_json(self.root / 'crypto' / 'zoo.json')
        missing = [f for f in zoo['factors'] if f['expr'] == '$funding']
        self.assertTrue(all(f['status'] == 'accepted' for f in missing))
        before = self._snapshot_data()
        report = dryrun.run_dryrun('crypto', self.out)
        self.assertEqual(report['missing_field_retained_count'], 2)
        self.assertEqual(report['missing_field_expression_count'], 1)
        for factor in missing:
            for convention in ('old', 'new'):
                accepted = report['horizons'][str(factor['horizon'])][convention]['accepted']
                self.assertIn(dict(id=factor['id'], train_t=factor['train']['t'], val_t=factor['val']['t']), accepted)
        self.assertEqual(self._snapshot_data(), before)

    def test_legacy_pass_receives_nan_series_and_uses_nw_t_compaction(self):
        panels = load_panels('crypto', self.root)
        cfg = MARKETS['crypto']
        splits = {h: make_splits(panels.mask.index, h, panels.windows) for h in cfg['horizons']}
        targets = {h: forward_target(panels, h) for h in cfg['horizons']}
        factors = store.read_json(self.root / 'crypto' / 'zoo.json')['factors'][:1]
        ic = pd.Series(np.linspace(-.1, .3, len(panels.mask)), index=panels.mask.index)
        ic.iloc[[0, 3, 6, 30]] = np.nan
        with patch.object(evaluate, 'cs_rank_ic', side_effect=lambda values, *args: ic.loc[values.index]), \
             patch.object(evaluate, 'nw_t', wraps=evaluate.nw_t) as legacy:
            old, new, missing = dryrun._reevaluate(factors, panels, targets, splits, cfg)
        self.assertEqual(legacy.call_count, 2)
        self.assertTrue(legacy.call_args_list[0].args[0].isna().any())
        factor = factors[0]
        key = factor['horizon'], factor['id']
        for segment in ('train', 'val'):
            sample = ic.loc[splits[factor['horizon']][segment]]
            self.assertEqual(old[key][segment]['t'], evaluate.nw_t(sample.dropna(), factor['horizon']))
            self.assertEqual(new[key][segment]['t'], evaluate.overlap_t(sample, factor['horizon']))
        self.assertEqual(missing, {})

    def test_none_metrics_are_dropped_separately(self):
        self._add_factor('0')
        original_overlap = evaluate.overlap_t
        evaluator = evaluate.evaluate_factor

        def drop_new_h3(*args):
            if args[-2] == 3 and evaluate.overlap_t is original_overlap:
                return None
            return evaluator(*args)

        with patch.object(evaluate, 'evaluate_factor', side_effect=drop_new_h3):
            report = dryrun.run_dryrun('crypto', self.out)
        self.assertEqual(report['horizons']['1']['old']['counts']['dropped'], 1)
        self.assertEqual(report['horizons']['1']['new']['counts']['dropped'], 1)
        self.assertEqual(report['horizons']['3']['old']['counts']['dropped'], 0)
        self.assertEqual(report['horizons']['3']['new']['counts']['dropped'], 1)
        self.assertIs(evaluate.overlap_t, original_overlap)

    def test_statistic_patches_restore_after_old_or_new_evaluation_failure(self):
        overlap = evaluate.overlap_t
        evaluator = evaluate.evaluate_factor
        for convention in ('old', 'new'):
            def fail(*args):
                is_new = evaluate.overlap_t is overlap
                if is_new == (convention == 'new'):
                    raise RuntimeError('evaluation failed')
                return evaluator(*args)

            with self.subTest(convention=convention), \
                 patch.object(evaluate, 'evaluate_factor', side_effect=fail):
                with self.assertRaisesRegex(RuntimeError, 'evaluation failed'):
                    dryrun.run_dryrun('crypto', self.out)
            self.assertIs(evaluate.overlap_t, overlap)
        self.assertFalse(self.out.exists())

    def test_other_expression_errors_propagate_and_restore_patches(self):
        overlap = evaluate.overlap_t
        with patch.object(dryrun, 'evaluate_expr', side_effect=ExprError('invalid expression')):
            with self.assertRaisesRegex(ExprError, 'invalid expression'):
                dryrun.run_dryrun('crypto', self.out)
        self.assertIs(evaluate.overlap_t, overlap)
        self.assertFalse(self.out.exists())


if __name__ == '__main__':
    unittest.main()
