"""Opt-in switches (all default OFF): greedy ensemble, family-weighted mutation, LLM memory."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import allocation, ensemble, live_scores, llm_memory, miner, options, store
from scripts.factor_mining.config import MARKETS
from scripts.factor_mining.expr import expr_hash
from scripts.factor_mining.families import family_fine
from scripts.factor_mining.greedy_ensemble import greedy_ensemble_factors
from scripts.factor_mining.miner import Miner, llm_propose, llm_propose_batches
from scripts.factor_mining.panel import Panels, forward_target, load_panels, make_splits
from scripts.factor_mining.tests.synth import SIGNAL_EXPR, SIGNAL_SEED, write_market


def switch(name):
    return patch.dict(os.environ, {name: '1'})


def synthetic(rows=300, names=30, seed=3):
    rng = np.random.default_rng(seed)
    index = pd.date_range('2021-01-01', periods=rows)
    cols = [f'S{i:02}' for i in range(names)]
    frame = lambda x: pd.DataFrame(x, index=index, columns=cols)
    label = frame(rng.normal(size=(rows, names)))
    noisy = lambda: label + frame(rng.normal(size=(rows, names))) * 4
    cache = dict(f1=noisy(), f2=noisy(), noise=frame(rng.normal(size=(rows, names))))
    cache['dup'] = cache['f1'] * 2.
    factors = [dict(id=k, expr='$close', direction=1, horizon=1, status='accepted', train=dict(t=t))
               for k, t in (('f1', 5.), ('dup', 4.5), ('f2', 4.), ('noise', 2.))]
    panels = Panels({'close': label}, frame(np.ones((rows, names), dtype=bool)))
    train = pd.Series(np.arange(rows) < 200, index=index)
    return panels, label, train, factors, cache


class OptionsTests(unittest.TestCase):
    def test_all_switches_default_off(self):
        with patch.dict(os.environ, clear=True):
            for name in (options.GREEDY_ENSEMBLE, options.FAMILY_WEIGHT, options.LLM_MEMORY):
                self.assertFalse(options.enabled(name))
        with patch.dict(os.environ, {options.LLM_MEMORY: '0'}):
            self.assertFalse(options.enabled(options.LLM_MEMORY))

    def test_run_nightly_does_not_enable_any_switch(self):
        text = (Path(miner.__file__).with_name('run_nightly.sh')).read_text()
        self.assertNotIn('FM_GREEDY_ENSEMBLE', text)
        self.assertNotIn('FM_FAMILY_WEIGHT', text)
        self.assertNotIn('FM_LLM_MEMORY', text)


class GreedyEnsembleTests(unittest.TestCase):
    cfg = dict(min_names=15)

    def test_keeps_only_factors_that_improve_train_t(self):
        panels, label, train, factors, cache = synthetic()
        kept = greedy_ensemble_factors(factors, panels, label, train, 1, self.cfg, cache)
        # dup carries no new information; noise dilutes; f2 is an independent view of the label.
        self.assertEqual([f['id'] for f in kept], ['f1', 'f2'])

    def test_labels_outside_train_are_never_read(self):
        panels, label, train, factors, cache = synthetic()
        expected = [f['id'] for f in greedy_ensemble_factors(factors, panels, label, train, 1, self.cfg, cache)]
        for fill in (np.nan, 1e9, -5.):
            poisoned = label.copy()
            poisoned.loc[~train.to_numpy()] = fill
            got = greedy_ensemble_factors(factors, panels, poisoned, train, 1, self.cfg, cache)
            self.assertEqual([f['id'] for f in got], expected)

    def test_caps_at_twenty_and_handles_empty_inputs(self):
        panels, label, train, factors, cache = synthetic()
        many, cache2 = [], {}
        rng = np.random.default_rng(9)
        for i in range(40):
            cache2[f'g{i}'] = label + pd.DataFrame(rng.normal(size=label.shape) * 6,
                                                   index=label.index, columns=label.columns)
            many.append(dict(factors[0], id=f'g{i}', train=dict(t=10. - i * .1)))
        self.assertLessEqual(len(greedy_ensemble_factors(many, panels, label, train, 1, self.cfg, cache2)), 20)
        self.assertEqual(greedy_ensemble_factors([], panels, label, train, 1, self.cfg, cache), [])
        self.assertEqual(greedy_ensemble_factors(factors, panels, label, train & False, 1, self.cfg, cache), [])

    def test_build_ensemble_uses_greedy_only_when_enabled(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            panels = load_panels('crypto', root)
            cfg = MARKETS['crypto']
            targets = {h: forward_target(panels, h) for h in cfg['horizons']}
            splits = {h: make_splits(panels.mask.index, h, panels.windows) for h in cfg['horizons']}
            twin = f'({SIGNAL_EXPR})*2'
            factors = [dict(id=expr_hash(e), expr=e, horizon=1, direction=1, status='accepted', train=dict(t=t))
                       for e, t in ((SIGNAL_EXPR, 10.), (twin, 9.))]
            off = ensemble.build_ensemble('crypto', factors, panels, targets, splits, cfg)
            self.assertEqual(len(off['horizons']['1']['factor_ids']), 2)
            with switch(options.GREEDY_ENSEMBLE):
                on = ensemble.build_ensemble('crypto', factors, panels, targets, splits, cfg)
                self.assertEqual(on['horizons']['1']['factor_ids'], [expr_hash(SIGNAL_EXPR)])
            with patch.object(ensemble, 'greedy_ensemble_factors', side_effect=AssertionError):
                again = ensemble.build_ensemble('crypto', factors, panels, targets, splits, cfg)
            self.assertEqual(again, off)       # switch off again: greedy path untouched

    def test_live_scores_selected_ids_pin_the_ensemble_factors(self):
        index = pd.date_range('2026-10-17', periods=5)
        close = pd.DataFrame(np.tile(np.arange(1., 16.), (5, 1)), index=index,
                             columns=[f'S{i:03}' for i in range(15)])
        panels = Panels({**{n: close.copy() for n in ('open', 'high', 'low', 'close', 'vwap')},
                         'volume': close ** 2}, close.notna())
        factors = [dict(id='f_close', horizon=1, expr='$close', direction=1, status='accepted', train={'t': 5.}),
                   dict(id='f_vol', horizon=1, expr='$volume', direction=-1, status='accepted', train={'t': 4.})]
        now = pd.Timestamp('2026-10-20T01:00:00Z').to_pydatetime()
        with tempfile.TemporaryDirectory() as root:
            default = live_scores.export_live_scores('crypto', factors, panels, root=root, now=now)
            self.assertEqual(default['horizons']['1']['factor_count'], 2)
            pinned = live_scores.export_live_scores('crypto', factors, panels, root=root, now=now,
                                                    selected_ids={1: ['f_vol']})
            self.assertEqual(pinned['horizons']['1']['factor_count'], 1)
            self.assertLess(pinned['horizons']['1']['scores']['S014']['z'], 0)   # direction -1 on volume
            as_str = live_scores.export_live_scores('crypto', factors, panels, root=root, now=now,
                                                    selected_ids={'1': ['f_close']})
            self.assertGreater(as_str['horizons']['1']['scores']['S014']['z'], 0)
            self.assertIsNone(live_scores.export_live_scores('crypto', factors, panels, root=root, now=now,
                                                             selected_ids={}))

    def test_miner_live_scores_match_ensemble_json_when_enabled(self):
        twin = dict(expr=f'({SIGNAL_EXPR})*2', family='momentum', name='TWIN')
        with tempfile.TemporaryDirectory() as root, switch(options.GREEDY_ENSEMBLE):
            write_market(root)
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED, twin]):
                Miner(root).run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=1, use_llm=False)
            ens = store.read_json(Path(root) / 'crypto' / 'ensemble.json')['horizons']
            live = store.read_json(Path(root) / 'crypto' / 'live_scores.json')
            for h, entry in ens.items():
                if entry:
                    self.assertEqual(live['horizons'][h]['factor_count'], len(entry['factor_ids']))
            self.assertEqual(len(ens['1']['factor_ids']), 1)


class FamilyWeightTests(unittest.TestCase):
    def test_family_yield_is_smoothed_train_share(self):
        ledger = ([dict(expr=f'a{i}', train_t=5.) for i in range(10)] +
                  [dict(expr=f'b{i}', train_t=1.) for i in range(10)] + [dict(expr='c', train_t=None)])
        yields = allocation.family_yield(ledger, lambda e: e[0])
        self.assertEqual(set(yields), {'a', 'b'})
        self.assertAlmostEqual(yields['a'], (10 + allocation.SMOOTHING * .5) / (10 + allocation.SMOOTHING))
        self.assertAlmostEqual(yields['b'], (0 + allocation.SMOOTHING * .5) / (10 + allocation.SMOOTHING))
        self.assertEqual(allocation.family_yield([], lambda e: e), {})
        # pass threshold is inclusive and configurable
        self.assertEqual(allocation.family_yield([dict(expr='a', train_t=3.)], str, pass_t=3.0)['a'], 1.)

    def test_family_yield_ignores_validation_fields(self):
        base = [dict(expr='a', train_t=5., status='candidate', val_ic=.1)] * 3
        poisoned = [dict(r, status='rejected', val_ic=-9., val_t=-9.) for r in base]
        self.assertEqual(allocation.family_yield(base, str), allocation.family_yield(poisoned, str))

    def test_pool_weights_properties(self):
        pool = [dict(expr='a1'), dict(expr='a2'), dict(expr='b1'), dict(expr='z1')]
        family = lambda e: e[0]
        weights = allocation.pool_weights(pool, dict(a=.8, b=.2), family)
        self.assertAlmostEqual(sum(weights), 1.)
        self.assertTrue(all(w > 0 for w in weights))          # unknown family z still explored
        self.assertGreater(weights[0], weights[2])
        self.assertAlmostEqual(weights[0], weights[1])
        self.assertAlmostEqual(weights[3], .3 / 4)             # explore share only
        self.assertEqual(allocation.pool_weights([], dict(a=1.), family), [])
        self.assertEqual(allocation.pool_weights(pool, {}, family), [.25] * 4)

    def test_miner_weights_parents_only_when_enabled(self):
        def run(env):
            with tempfile.TemporaryDirectory() as root, patch.dict(os.environ, env), \
                 patch.object(miner.allocation, 'pool_weights', wraps=allocation.pool_weights) as spy:
                write_market(root)
                with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                    Miner(root).run_market('crypto', n_mutations=8, n_llm=0, budget_s=120, seed=4,
                                           use_llm=False)
                return spy, [r['expr'] for r in store.read_ledger('crypto', root)]
        spy, off_exprs = run({})
        spy.assert_not_called()
        spy, on_exprs = run({options.FAMILY_WEIGHT: '1'})
        spy.assert_called_once()
        self.assertIs(spy.call_args.args[2], family_fine)
        self.assertGreater(len(on_exprs), 0)
        # default stays reproducible
        self.assertEqual(off_exprs, run({})[1])


class LlmMemoryTests(unittest.TestCase):
    @staticmethod
    def items(prefix, n=7, fails=3):
        return [dict(expr=f'{prefix}{i}', horizon=1, train_t=float(i), train_ic=.01,
                     train_screen='fail:x' if i < fails else 'pass') for i in range(n)]

    def test_record_and_history_keep_last_three_nights_train_only(self):
        with tempfile.TemporaryDirectory() as root:
            for night in range(5):
                llm_memory.record_batch(root, 'crypto', f'run{night}', 0, self.items(f'n{night}_'))
                llm_memory.record_batch(root, 'crypto', f'run{night}', 1, self.items(f'm{night}_', 2, 1))
            llm_memory.record_batch(root, 'crypto', 'current', 0, self.items('cur_'))
            past = llm_memory.history(root, 'crypto', 'current')
            self.assertEqual(len(past), 3)
            last = past[-1]                       # run4 is the most recent night before current
            self.assertEqual([i['expr'] for i in last['top']], ['n4_6', 'n4_5', 'n4_4', 'n4_3', 'n4_2'])
            self.assertEqual((last['fails'], last['total']), (3 + 1, 9))
            self.assertNotIn('cur_', json.dumps(past))
            self.assertNotIn('run1', json.dumps(past))
            self.assertNotIn('val', json.dumps(past))
            self.assertEqual(len(llm_memory.history(root, 'crypto', 'current', nights=1)), 1)
            self.assertEqual(llm_memory.history(root, 'crypto', 'current', nights=0), [])
            self.assertEqual(llm_memory.history(root, 'us', 'current'), [])

    def test_prompt_unchanged_without_history_and_val_free_with_it(self):
        prompts = []

        def runner(workdir, prompt_path, out_path):
            prompts.append(Path(prompt_path).read_text(encoding='utf-8'))
            out_path.write_text('["Mean($close,5)"]', encoding='utf-8')

        history = json.dumps([dict(night='2026-10-20', top=[dict(expr='Mean($close,9)', train_t=3.5)],
                                   fails=4, total=14)])
        with tempfile.TemporaryDirectory() as root:
            llm_propose('crypto', '[]', '[]', 5, runner, Path(root) / 'a')
            llm_propose('crypto', '[]', '[]', 5, runner, Path(root) / 'b', history_text=history)
            llm_propose_batches('crypto', '[]', '[]', 40, runner, Path(root) / 'c', history_text=history)
        self.assertNotIn('前几晚', prompts[0])
        self.assertIn('前几晚的探索记录', prompts[1])
        self.assertIn('Mean($close,9)', prompts[1])
        self.assertTrue(all('前几晚' in p for p in prompts[1:]))
        for text in prompts:
            self.assertNotIn('val', text)

    def test_miner_records_batches_and_next_night_prompt_sees_them(self):
        prompts = []

        def fake(work, prompt, out):
            prompts.append(prompt.read_text())
            self.assertNotIn('val', prompts[-1])
            out.write_text(json.dumps([SIGNAL_EXPR, f'Mean($close,{len(prompts) + 4})']))

        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            memory_file = Path(root) / 'crypto' / 'llm_memory.jsonl'
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                Miner(root).run_market('crypto', n_mutations=0, n_llm=20, budget_s=120, seed=1,
                                       use_llm=True, runner=fake)          # switch off
                self.assertFalse(memory_file.exists())
                self.assertNotIn('前几晚', prompts[0])
                with switch(options.LLM_MEMORY):
                    Miner(root).run_market('crypto', n_mutations=0, n_llm=20, budget_s=120, seed=2,
                                           use_llm=True, runner=fake)
                    self.assertTrue(memory_file.exists())
                    self.assertNotIn('前几晚', prompts[1])                  # nothing recorded before this night
                    Miner(root).run_market('crypto', n_mutations=0, n_llm=20, budget_s=120, seed=3,
                                           use_llm=True, runner=fake)
            self.assertIn('前几晚的探索记录', prompts[2])
            rows = store.read_jsonl(memory_file)
            self.assertEqual(len(rows), 2)
            self.assertEqual({k for item in rows[0]['items'] for k in item},
                             {'expr', 'horizon', 'train_t', 'train_ic', 'train_screen'})
            self.assertNotIn('val', json.dumps(rows))


if __name__ == '__main__':
    unittest.main()
