import json
from pathlib import Path
import random
import subprocess
import tempfile
import unittest
from contextlib import nullcontext
from unittest.mock import patch, Mock

from scripts.factor_mining import evaluate, miner, store
from scripts.factor_mining.expr import canonical, expr_hash, validate_expr
from scripts.factor_mining.miner import Miner, build_status_events, family, llm_propose, llm_propose_batches, mutate, summarize_outcomes
from scripts.factor_mining.tests.synth import SIGNAL_EXPR, SIGNAL_SEED, write_market


class LlmFeedbackTests(unittest.TestCase):
    def test_summary_is_train_only_ranked_and_capped(self):
        outcomes = [dict(expr=f'e{i}', horizon=1, status='accepted', train_t=float(i), train_ic=.01 * i,
                         val_ic=987654.321, val_t=55., reason='ok', train_screen='pass') for i in range(30)]
        summary = summarize_outcomes(outcomes)
        self.assertEqual(len(summary), 14)
        self.assertEqual(summary[0]['expr'], 'e29')
        blob = json.dumps(summary)
        for leaked in ('987654', 'val_', 'status', 'accepted', 'reason'):
            self.assertNotIn(leaked, blob)

    def test_after_batch_feedback_reaches_next_prompt_without_validation_terms(self):
        prompts = []

        def runner(workdir, prompt_path, out_path):
            prompts.append(Path(prompt_path).read_text(encoding='utf-8'))
            out_path.write_text(json.dumps([f'Mean($close,{len(prompts) + 2})']), encoding='utf-8')

        def after(fresh):
            return json.dumps([dict(expr=fresh[0], train_t=3.2, train_screen='pass')])

        with tempfile.TemporaryDirectory() as root:
            llm_propose_batches('crypto', '[]', '[]', 40, runner, root, lambda: False, after)
        self.assertEqual(len(prompts), 2)
        self.assertNotIn('"train_t": 3.2', prompts[0])
        self.assertIn('"train_t": 3.2', prompts[1])
        self.assertIn('Mean($close,3)', prompts[1])
        for text in prompts:
            self.assertNotIn('val', text)

    def test_run_market_feeds_each_llm_batch_back_before_the_next_one(self):
        seen_prompts = []
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)

            def fake(work, prompt, out):
                text = prompt.read_text()
                seen_prompts.append(text)
                self.assertNotIn('val', text)
                k = len(seen_prompts)
                out.write_text(json.dumps([SIGNAL_EXPR, f'Mean($close,{k + 4})', f'Std($close,{k + 4})']))

            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                search.run_market('crypto', n_mutations=0, n_llm=40, budget_s=120, seed=2,
                                  use_llm=True, runner=fake)
        self.assertEqual(len(seen_prompts), 2)
        self.assertNotIn('"train_screen"', seen_prompts[0])   # nothing evaluated yet
        self.assertIn('"train_screen":', seen_prompts[1])      # batch 1 outcomes came back
        self.assertIn(SIGNAL_EXPR, seen_prompts[1])


class RejectReasonTests(unittest.TestCase):
    def test_reasons_agree_with_classify(self):
        from scripts.factor_mining import evaluate, feedback
        base = dict(t=4., ic=.02, ls_ret=.1, ic_halves=[.02, .02])
        grid = []
        for t in (1., 4.):
            for halves in ([.02, .02], [.02, -.02]):
                for ic in (.02, -.02):
                    for vic in (.02, -.02):
                        for vt in (.5, 3.):
                            for ls in (.1, -.1):
                                grid.append((dict(base, t=t, ic_halves=halves, ic=ic),
                                             dict(ic=vic, t=vt, ls_ret=ls)))
        for train, val in grid:
            reason = feedback.reject_reason(train, val, 3.)
            verdict = evaluate.classify(train, val, 100, 3.)
            self.assertEqual(reason == 'ok', verdict == 'accepted', (train, val, reason, verdict))
            self.assertEqual(verdict == 'rejected', feedback.train_screen(train, 3.) != 'pass', (train, val))


class LlmBatchTests(unittest.TestCase):
    def test_large_n_uses_several_capped_calls_and_dedupes(self):
        calls = []

        def runner(workdir, prompt_path, out_path):
            calls.append(Path(prompt_path).read_text(encoding='utf-8'))
            k = len(calls)
            exprs = [f'Mean($close,{k + 2})'] + ['Mean($close,3)'] * 2
            out_path.write_text(json.dumps(exprs), encoding='utf-8')

        with tempfile.TemporaryDirectory() as root:
            exprs, stats = llm_propose_batches('crypto', '[]', '[]', 45, runner, root)
        self.assertEqual(len(calls), 3)  # 45 -> 20 + 20 + 5
        self.assertIn('Mean($close,3)', calls[1])  # later calls are told what exists
        self.assertEqual(len(exprs), len(set(exprs)))
        self.assertEqual(stats['llm_proposed'], 3 * 3)

    def test_failed_batch_does_not_stop_the_rest_and_budget_stops_early(self):
        seen = []

        def flaky(workdir, prompt_path, out_path):
            seen.append(1)
            if len(seen) == 1:
                raise RuntimeError('boom')
            out_path.write_text('["Mean($close,5)"]', encoding='utf-8')

        with tempfile.TemporaryDirectory() as root:
            exprs, stats = llm_propose_batches('us', '[]', '[]', 40, flaky, root)
            self.assertEqual(exprs, ['Mean($close,5)'])
            self.assertIn('boom', stats['llm_error'])
            seen.clear()
            exprs, stats = llm_propose_batches('us', '[]', '[]', 60, flaky, root + '/b', lambda: True)
        self.assertEqual(len(seen), 1)  # out of time before batch 2


class MinerTests(unittest.TestCase):
    def test_mutations_legal_unique_and_reproducible(self):
        pool = [SIGNAL_EXPR, '$volume', 'Mean($close,10)']
        for expression in ('Mean($close,10)', 'Std($ret,20)', 'Corr($close,$volume,30)',
                           'Skew($ret,5)', 'Max($open,5)', SIGNAL_EXPR):
            a = mutate(expression, random.Random(81), pool)
            self.assertEqual(a, mutate(expression, random.Random(81), pool))
            self.assertGreater(len(a), 3)
            self.assertEqual(len(a), len(set(a)))
            self.assertNotIn(canonical(expression), a)
            for expr in a:
                validate_expr(expr)
        variants = mutate('Mean($close,10)', random.Random(81), pool)
        self.assertTrue(any('EMA' in e for e in variants))
        self.assertTrue(any('$vwap' in e for e in variants))
        self.assertTrue(any('Mean($close,7)' in e for e in variants))
        self.assertTrue(any('Mean($close,14)' in e for e in variants))

    def test_family_priority(self):
        self.assertEqual(family('Corr($close,$volume,5)'), 'price_volume')
        self.assertEqual(family('Std($volume,5)'), 'volume')
        self.assertEqual(family('Kurt($ret,5)'), 'volatility')
        self.assertEqual(family('Delta($close,5)'), 'momentum')
        self.assertEqual(family('$open'), 'other')

    def test_llm_last_array_invalid_counts_and_errors(self):
        with tempfile.TemporaryDirectory() as root:
            def fake(work, prompt, out):
                self.assertEqual(work, Path(root).resolve())
                text = prompt.read_text()
                self.assertIn('train_t', text)
                self.assertNotIn('val', text)
                out.write_text('示例 ["$high"]\n结果\n```json\n' +
                               json.dumps(['$close', '__import__("os")', '$volume', 12]) + '\n```')
            exprs, stats = llm_propose('crypto', '[{"expr":"$close","train_t":3}]',
                                      '[]', 20, fake, root)
            self.assertEqual(exprs, ['$close', '$volume'])
            self.assertEqual((stats['llm_proposed'], stats['llm_valid'], stats['llm_invalid']), (4, 2, 2))
            self.assertIsNone(stats['llm_error'])
            self.assertEqual({p.name for p in Path(root).iterdir()}, {'prompt.md', 'out.md'})
            def bad(*args):
                raise RuntimeError('fake model failed')
            exprs, stats = llm_propose('us', '[]', '[]', 20, bad, root)
            self.assertEqual(exprs, [])
            self.assertIn('fake model failed', stats['llm_error'])
            self.assertEqual(stats['llm_invalid'], 0)

    def test_model_deadline_kills_process_group(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            process = Mock(pid=12345)
            process.communicate.side_effect = subprocess.TimeoutExpired('fake', 900)
            with patch.object(miner.subprocess, 'Popen', return_value=process) as popen, \
                 patch.object(miner.os, 'killpg') as kill:
                with self.assertRaises(subprocess.TimeoutExpired):
                    miner.model_call(root, root/'prompt.md', root/'out.md')
                self.assertTrue(popen.call_args.kwargs['start_new_session'])
                self.assertEqual(process.communicate.call_args.kwargs['timeout'], 900)
                kill.assert_called_once()

    def test_redundancy_dedup_and_history_reclassification(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            seeds = [SIGNAL_SEED, dict(SIGNAL_SEED, expr=f'2*({SIGNAL_EXPR})')]
            with patch.object(miner.zoo, 'seed_factors', return_value=seeds):
                output = search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120,
                                            seed=1, use_llm=False)
                first = store.read_ledger('crypto', root)
                h1 = [f for f in output['factors'] if f['horizon'] == 1]
                self.assertEqual({f['status'] for f in h1}, {'accepted', 'redundant'})
                redundant = next(f for f in h1 if f['status'] == 'redundant')
                self.assertAlmostEqual(redundant['corr_max'], 1.)
                self.assertIsNotNone(redundant['corr_with'])
                search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=1, use_llm=False)
                self.assertEqual(first, store.read_ledger('crypto', root))
            # A refreshed historical experiment clears the old threshold, but
            # additional ledger trials must still demote it under the new K.
            previous = store.read_json(Path(root)/'crypto/zoo.json')
            accepted = next(f for f in previous['factors'] if f['horizon'] == 1 and f['status'] == 'accepted')
            refreshed_t = previous['horizons']['1']['t_req'] + .001
            evaluator = miner.evaluate.evaluate_factor
            def near_threshold(*args):
                metrics = evaluator(*args)
                if metrics is not None:
                    metrics['train']['t'] = refreshed_t
                return metrics
            extra = [dict(first[0], id=f'f_history{i:05}', status='rejected', train_t=0.) for i in range(100)]
            store.append_ledger('crypto', extra, root)
            with patch.object(miner.zoo, 'seed_factors', return_value=seeds), \
                 patch.object(miner.evaluate, 'evaluate_factor', side_effect=near_threshold):
                output = search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120,
                                            seed=1, use_llm=False)
            updated = next(f for f in output['factors'] if f['horizon'] == 1 and f['id'] == accepted['id'])
            self.assertEqual(updated['status'], 'rejected')
            self.assertEqual(updated['status_history'][-1]['from'], 'accepted')
            self.assertEqual(updated['status_history'][-1]['to'], 'rejected')
            self.assertEqual(output['horizons']['1']['trials'], 102)
            for h in output['horizons'].values():
                self.assertEqual(h['trials'], sum(h[s] for s in ('accepted', 'candidate', 'rejected', 'redundant')))

    def test_llm_train_only_view_invalid_and_valid_trial_accounting(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=2, use_llm=False)
                path = Path(root)/'crypto/zoo.json'
                old = store.read_json(path)
                for f in old['factors']:
                    f['val']['ic'] = 987654.321
                store.write_json(path, old)
                before = len(store.read_ledger('crypto', root))
                def fake(work, prompt, out):
                    text = prompt.read_text()
                    self.assertNotIn('987654', text)
                    self.assertNotIn('val', text)
                    self.assertEqual({p.name for p in work.iterdir()}, {'prompt.md'})
                    out.write_text(json.dumps(['$volume', 'bad()', '0', SIGNAL_EXPR]))
                search.run_market('crypto', n_mutations=0, n_llm=20, budget_s=120, seed=2,
                                  use_llm=True, runner=fake)
            run = store.read_jsonl(Path(root)/'runs.jsonl')[-1]
            self.assertEqual((run['llm_proposed'], run['llm_valid'], run['llm_invalid']), (4, 3, 1))
            self.assertEqual(len(store.read_ledger('crypto', root)) - before, 3)
            self.assertEqual(run['trials_total'], before+3)
            self.assertTrue(run['invalid'] >= 3)

    def test_refresh_groups_ids_recomputes_direction_drops_invalid_and_updates_pool(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                before = search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120,
                                           seed=1, use_llm=False)
            self.assertEqual(len(before['factors']), 3)
            ledger = store.read_ledger('crypto', root)
            expression, evaluator = miner.evaluate_expr, miner.evaluate.evaluate_factor
            def reversed_values(expr, fields):
                return -expression(expr, fields)
            def refreshed(*args):
                if args[-2] == 7:
                    return None
                metrics = evaluator(*args)
                metrics['train']['t'] = 0.  # Keep correlation/ensemble out of this call-count test.
                return metrics
            def mutation(expr, rng, pool):
                self.assertTrue(pool)
                self.assertTrue(all(f['direction'] == -1 and f['train']['t'] == 0.
                                    and f['horizon'] != 7 for f in pool))
                return []
            def proposal(work, prompt, out):
                top = json.loads(prompt.read_text().split('训练集 top-10（仅 train 指标）：\n')[1].split('\n')[0])
                self.assertEqual(len(top), 2)
                self.assertTrue(all(f['train_t'] == 0. for f in top))
                out.write_text('[]')
            with patch.object(miner.zoo, 'seed_factors', return_value=[]), \
                 patch.object(miner, 'evaluate_expr', side_effect=reversed_values) as expr_calls, \
                 patch.object(miner.evaluate, 'evaluate_factor', side_effect=refreshed) as metric_calls, \
                 patch.object(miner, 'mutate', side_effect=mutation) as mutations:
                after = search.run_market('crypto', n_mutations=1, n_llm=1, budget_s=120,
                                          seed=2, use_llm=True, runner=proposal)
            self.assertEqual(expr_calls.call_count, 1)
            self.assertEqual(metric_calls.call_count, 3)
            self.assertGreater(mutations.call_count, 0)
            self.assertEqual({f['horizon'] for f in after['factors']}, {1, 3})
            self.assertTrue(all(f['direction'] == -1 for f in after['factors']))
            self.assertEqual(ledger, store.read_ledger('crypto', root))
            self.assertEqual(after['horizons']['7']['trials'], 1)
            self.assertEqual(after['horizons']['7']['rejected'], 1)
            self.assertEqual(store.read_jsonl(Path(root)/'runs.jsonl')[-1]['reevaluated'], 3)

    def test_refresh_budget_preserves_old_snapshot_and_skips_candidates(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120,
                                  seed=1, use_llm=False)
            files = [Path(root)/'crypto'/name for name in ('zoo.json', 'ensemble.json', 'ledger.jsonl')]
            previous = [path.read_bytes() for path in files]
            evaluator = miner.evaluate.evaluate_factor
            for budget, expected_count in ((0, 0), (3, 2)):
                current = [0.]
                def slow(*args):
                    metrics = evaluator(*args)
                    metrics['train']['t'] = 0.
                    current[0] += 2.
                    return metrics
                with self.subTest(budget=budget), \
                     patch.object(miner.time, 'monotonic', side_effect=lambda: current[0]), \
                     patch.object(miner.evaluate, 'evaluate_factor', side_effect=slow), \
                     patch.object(miner.zoo, 'seed_factors', side_effect=AssertionError('candidate generation')):
                    output = search.run_market('crypto', n_mutations=10, n_llm=1, budget_s=budget,
                                               seed=2, use_llm=True)
                self.assertEqual(output, store.read_json(files[0]))
                self.assertEqual([path.read_bytes() for path in files], previous)
                row = store.read_jsonl(Path(root)/'runs.jsonl')[-1]
                self.assertTrue(row['budget_exhausted'])
                self.assertEqual(row['reevaluated'], expected_count)
                self.assertIsNone(row['error'])
                self.assertFalse(store.read_json(Path(root)/'status.json')['running'])

    def test_budget_finishes_normally(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root, rows=150, columns=15)
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                out = Miner(root).run_market('crypto', n_mutations=0, n_llm=0, budget_s=0,
                                             seed=1, use_llm=False)
            self.assertFalse(out['factors'])
            self.assertEqual(store.read_ledger('crypto', root), [])
            self.assertTrue(store.read_jsonl(Path(root)/'runs.jsonl')[-1]['budget_exhausted'])
            self.assertFalse(store.read_json(Path(root)/'status.json')['running'])


class BuildStatusEventsTests(unittest.TestCase):
    def test_kinds_fields_and_drops_last(self):
        factors = {
            (1, 'new_acc'): dict(status='accepted', first_seen='2026-01-01'),
            (1, 'new_red'): dict(status='redundant', first_seen='2026-01-01'),
            (3, 'demo'): dict(status='rejected', first_seen='2025-01-01'),
            (3, 're'): dict(status='accepted', first_seen='2025-01-01'),
            (3, 'same'): dict(status='candidate', first_seen='2025-01-01'),
            (3, 'trans'): dict(status='candidate', first_seen='2025-01-01'),
        }
        old = {
            (3, 'demo'): dict(status='accepted', first_seen='2025-01-01'),
            (3, 'gone'): dict(status='accepted', first_seen='2024-01-01'),
            (3, 're'): dict(status='candidate', first_seen='2025-01-01'),
            (3, 'same'): dict(status='candidate', first_seen='2025-01-01'),
            (3, 'trans'): dict(status='rejected', first_seen='2025-01-01'),
            (7, 'gone2'): dict(status='rejected', first_seen='2024-01-01'),
        }
        events, promoted = build_status_events(
            'run-1', 'crypto', 'ts', factors, old,
            {(1, 'new_acc'): 'accepted', (1, 'new_red'): 'accepted'}, {1: 4, 3: 9, 7: 2},
            {(1, 'new_acc'), (1, 'new_red'), (3, 'demo'), (3, 're'), (3, 'same'), (3, 'trans')},
            {(3, 're')})
        self.assertEqual(len(events), 7)  # 5 transitions + 2 drops, unchanged 'same' skipped
        self.assertEqual(events[:5], sorted(events[:5], key=lambda e: (e['horizon'], e['id'])))
        by_key = {(e['horizon'], e['id']): e for e in events}
        first_acc = by_key[1, 'new_acc']
        self.assertEqual(first_acc['kind'], 'first_admission')
        self.assertIsNone(first_acc['from'])
        self.assertEqual((first_acc['to'], first_acc['ledger_status']), ('accepted', 'accepted'))
        self.assertEqual((first_acc['K'], first_acc['t_required']), (4, evaluate.t_required(4)))
        self.assertEqual(first_acc['first_seen'], '2026-01-01')
        self.assertEqual((first_acc['run_id'], first_acc['market'], first_acc['ts']), ('run-1', 'crypto', 'ts'))
        # Ledger said accepted at first classify, but the run-final status is redundant.
        first_red = by_key[1, 'new_red']
        self.assertEqual((first_red['kind'], first_red['ledger_status'], first_red['to']),
                         ('first_admission', 'accepted', 'redundant'))
        self.assertEqual((by_key[3, 're']['kind'], by_key[3, 're']['from'], by_key[3, 're']['to']),
                         ('re_promotion', 'candidate', 'accepted'))
        self.assertIsNone(by_key[3, 're']['ledger_status'])
        self.assertEqual(by_key[3, 'demo']['kind'], 'demotion')
        self.assertEqual(by_key[3, 'trans']['kind'], 'transition')
        self.assertEqual(promoted, [(3, 're')])
        # Drops come last with to=None and the previous run-final status as from.
        self.assertEqual([e['kind'] for e in events[-2:]], ['dropped', 'dropped'])
        self.assertEqual([(e['horizon'], e['id']) for e in events[-2:]], [(3, 'gone'), (7, 'gone2')])
        self.assertEqual(by_key[3, 'gone']['from'], 'accepted')
        self.assertIsNone(by_key[3, 'gone']['to'])
        self.assertEqual(by_key[7, 'gone2']['K'], 2)
        # A first-ever promotion (no earlier acceptance anywhere) is not a re_promotion.
        events2, promoted2 = build_status_events(
            'run-2', 'us', 'ts', {(5, 'p'): dict(status='accepted', first_seen='d')},
            {(5, 'p'): dict(status='candidate', first_seen='d')}, {}, {5: 3}, {(5, 'p')}, set())
        self.assertEqual((events2[0]['kind'], events2[0]['from'], events2[0]['to']),
                         ('promotion', 'candidate', 'accepted'))
        self.assertEqual(promoted2, [(5, 'p')])


class StatusEventLogTests(unittest.TestCase):
    def test_first_run_writes_first_admission_for_every_new_trial(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            seeds = [SIGNAL_SEED, dict(SIGNAL_SEED, expr=f'2*({SIGNAL_EXPR})')]
            with patch.object(miner.zoo, 'seed_factors', return_value=seeds):
                output = search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120,
                                           seed=1, use_llm=False)
            events = store.read_status_events('crypto', root)
            final = {(f['horizon'], f['id']): f for f in output['factors']}
            self.assertEqual({(e['horizon'], e['id']) for e in events}, set(final))
            self.assertTrue(all(e['kind'] == 'first_admission' for e in events))
            run_id = store.read_jsonl(Path(root)/'runs.jsonl')[0]['run_id']
            ledger = {(r['horizon'], r['id']): r['status'] for r in store.read_ledger('crypto', root)}
            self.assertEqual(events, sorted(events, key=lambda e: (e['horizon'], e['id'])))
            for e in events:
                key = (e['horizon'], e['id'])
                self.assertIsNone(e['from'])
                self.assertEqual(e['to'], final[key]['status'])
                self.assertEqual(e['ledger_status'], ledger[key])
                self.assertEqual(e['first_seen'], final[key]['first_seen'])
                self.assertEqual((e['run_id'], e['market']), (run_id, 'crypto'))
                self.assertEqual(e['K'], output['horizons'][str(e['horizon'])]['trials'])
                self.assertEqual(e['t_required'], output['horizons'][str(e['horizon'])]['t_req'])
            # First classify accepted the duplicate; the run-final redundancy
            # dedupe demotes it, so ledger_status='accepted' but to='redundant'.
            redundant = [e for e in events if e['horizon'] == 1 and e['to'] == 'redundant']
            self.assertEqual(len(redundant), 1)
            self.assertEqual(redundant[0]['ledger_status'], 'accepted')
            for f in output['factors']:
                self.assertEqual(f['first_final_status'], f['status'])
                self.assertEqual(f['promotions'], 1 if f['status'] == 'accepted' else 0)

    def test_second_run_unchanged_writes_nothing_and_changes_get_kinds(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                first = search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120,
                                          seed=1, use_llm=False)
                events_1 = store.read_status_events('crypto', root)
                accepted = next(f for f in first['factors'] if f['status'] == 'accepted')
                # Same data: nothing changes, so the second run appends nothing.
                search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=1, use_llm=False)
                self.assertEqual(store.read_status_events('crypto', root), events_1)
                # Weak validation demotes the accepted factor to candidate.
                evaluator = evaluate.evaluate_factor
                def weak_val(*args):
                    metrics = evaluator(*args)
                    if metrics is not None:
                        metrics['val']['t'] = 0.
                    return metrics
                with patch.object(evaluate, 'evaluate_factor', side_effect=weak_val):
                    third = search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120,
                                              seed=1, use_llm=False)
            run_id = store.read_jsonl(Path(root)/'runs.jsonl')[2]['run_id']
            new = [e for e in store.read_status_events('crypto', root) if e['run_id'] == run_id]
            self.assertTrue(new)
            demotion = next(e for e in new if (e['horizon'], e['id']) == (accepted['horizon'], accepted['id']))
            self.assertEqual((demotion['kind'], demotion['from'], demotion['to']),
                             ('demotion', 'accepted', 'candidate'))
            self.assertEqual(demotion['to'],
                             next(f['status'] for f in third['factors']
                                  if (f['horizon'], f['id']) == (accepted['horizon'], accepted['id'])))
            self.assertIsNone(demotion['ledger_status'])
            for e in new:
                self.assertNotEqual(e['kind'], 'first_admission')

    def test_re_promotion_across_three_runs(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            evaluator = evaluate.evaluate_factor
            def weak_val(*args):
                metrics = evaluator(*args)
                if metrics is not None:
                    metrics['val']['t'] = 0.
                return metrics
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=1, use_llm=False)
                with patch.object(evaluate, 'evaluate_factor', side_effect=weak_val):
                    search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=1, use_llm=False)
                output = search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120,
                                           seed=1, use_llm=False)
            events = store.read_status_events('crypto', root)
            sequences = {}
            for e in events:
                sequences.setdefault((e['horizon'], e['id']), []).append(e)
            full = [(k, v) for k, v in sequences.items()
                    if [e['kind'] for e in v] == ['first_admission', 'demotion', 're_promotion']]
            self.assertTrue(full)
            for key, seq in full:
                self.assertEqual([e['to'] for e in seq], ['accepted', 'candidate', 'accepted'])
                self.assertEqual(seq[2]['from'], 'candidate')
            key = full[0][0]
            factor = next(f for f in output['factors'] if (f['horizon'], f['id']) == key)
            self.assertEqual(factor['promotions'], 2)
            self.assertEqual(factor['first_final_status'], 'accepted')

    def test_re_promotion_detected_from_zoo_history_without_events(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=1, use_llm=False)
            zoo = store.read_json(Path(root)/'crypto/zoo.json')
            factor = next(f for f in zoo['factors'] if f['status'] == 'accepted')
            factor['status'] = 'candidate'
            factor['status_history'] = [dict(ts='earlier', run_id='old', **{'from': 'accepted'}, to='candidate')]
            store.write_json(Path(root)/'crypto/zoo.json', zoo)
            # Pre-log era: no status_events file, acceptance only in the truncated history.
            (Path(root)/'crypto/status_events.jsonl').unlink()
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=1, use_llm=False)
            events = store.read_status_events('crypto', root)
            match = [e for e in events if (e['horizon'], e['id']) == (factor['horizon'], factor['id'])]
            self.assertEqual([e['kind'] for e in match], ['re_promotion'])
            self.assertEqual((match[0]['from'], match[0]['to']), ('candidate', 'accepted'))
            updated = store.read_json(Path(root)/'crypto/zoo.json')['factors']
            self.assertEqual(next(f for f in updated if f['id'] == factor['id'])['promotions'], 2)

    def test_status_events_are_never_truncated(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            evaluator = evaluate.evaluate_factor
            def weak_val(*args):
                metrics = evaluator(*args)
                if metrics is not None:
                    metrics['val']['t'] = 0.
                return metrics
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=1, use_llm=False)
                for i in range(6):  # accepted -> candidate -> accepted, three times over
                    guard = (patch.object(evaluate, 'evaluate_factor', side_effect=weak_val)
                             if i % 2 == 0 else nullcontext())
                    with guard:
                        search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120,
                                          seed=1, use_llm=False)
            zoo = store.read_json(Path(root)/'crypto/zoo.json')
            key = (1, expr_hash(SIGNAL_EXPR))
            factor = next(f for f in zoo['factors'] if (f['horizon'], f['id']) == key)
            events = [e for e in store.read_status_events('crypto', root)
                      if (e['horizon'], e['id']) == key]
            self.assertEqual([e['kind'] for e in events],
                             ['first_admission'] + ['demotion', 're_promotion'] * 3)
            self.assertEqual(len(events), 7)
            # The zoo history still truncates at 5; the event log is the full record.
            self.assertLessEqual(len(factor['status_history']), 5)
            self.assertEqual(factor['promotions'], 4)

    def test_budget_stop_before_classification_writes_no_events(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            search = Miner(root)
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                search.run_market('crypto', n_mutations=0, n_llm=0, budget_s=120, seed=1, use_llm=False)
            path = Path(root)/'crypto/status_events.jsonl'
            before = path.read_bytes()
            # First monotonic read sets the run clock to 0; every later read is
            # past the budget, so the run stops during reevaluate, before classify.
            ticks = iter([0.])
            with patch.object(miner.time, 'monotonic',
                              side_effect=lambda: next(ticks, 1e9)), \
                 patch.object(miner.zoo, 'seed_factors', side_effect=AssertionError('no candidates')):
                search.run_market('crypto', n_mutations=10, n_llm=1, budget_s=120, seed=2, use_llm=True)
            self.assertEqual(path.read_bytes(), before)
            self.assertTrue(store.read_jsonl(Path(root)/'runs.jsonl')[-1]['budget_exhausted'])
