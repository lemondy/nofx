"""Offline seed/mutation/LLM search supervised by a frozen evaluator.

The persisted split envelope uses the shortest horizon; every horizon has its
own h+2 purged masks internally (the file schema has one shared date envelope).
"""
import ast
from collections import Counter, defaultdict
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import random
import re
import signal
import subprocess
import time
from typing import TypedDict

from . import allocation, evaluate, feedback, forward, llm_memory, options, panel, zoo
from .config import CORR_REDUNDANT, MARKETS
from .data_fetch import ROOT
from .ensemble import build_ensemble
from .expr import OPERATORS, ExprError, canonical, dsl_help, evaluate_expr, expr_hash, validate_expr
from .families import family_fine
from .panel import forward_target, load_panels, make_splits
from . import store


class Candidate(TypedDict):
    expr: str
    source: str
    family: str


def family(expr):
    if re.search(r'\b(Corr|Cov)\(', expr):
        return 'price_volume'
    if '$volume' in expr or re.search(r'\bRank\(', expr):
        return 'volume'
    if re.search(r'\b(Std|Skew|Kurt|Mad)\(', expr):
        return 'volatility'
    if re.search(r'\b(Delta|Ref)\(\$close', expr):
        return 'momentum'
    return 'other'


def mutate(expr, rng, pool) -> list[str]:
    """Make validated local AST edits, wrappers, and pool combinations."""
    original = canonical(expr)
    tree = ast.parse(re.sub(r'\$([a-z]+)', r'FM_\1', expr), mode='eval')
    variants = []
    windows = (3, 5, 7, 10, 14, 20, 30, 40, 60, 90, 120)
    groups = (('Mean', 'EMA', 'WMA', 'Median'), ('Std', 'Mad'), ('Max', 'Min'),
              ('Corr', 'Cov'), ('Skew', 'Kurt'))
    def emit():
        variants.append(re.sub(r'\bFM_([a-z]+)\b', r'$\1', ast.unparse(tree)))
    for node in ast.walk(tree):
        if isinstance(node, ast.Call):
            name = node.func.id
            if OPERATORS[name]['window_arg']:
                pos = 2 if name in ('Corr', 'Cov') else 1
                old = node.args[pos]
                near = sorted(windows, key=lambda w: (abs(w-old.value), w))
                for window in [w for w in near if w != old.value][:2]:
                    node.args[pos] = ast.Constant(window)
                    emit()
                node.args[pos] = old
            for group in groups:
                if name in group:
                    choices = [op for op in group if op != name]
                    rng.shuffle(choices)
                    for op in choices:
                        node.func.id = op
                        emit()
                    node.func.id = name
        elif isinstance(node, ast.Name) and node.id in ('FM_close', 'FM_vwap', 'FM_open'):
            old = node.id
            for field in ('FM_close', 'FM_vwap', 'FM_open'):
                if field != old:
                    node.id = field
                    emit()
            node.id = old
    window = rng.choice(windows)
    variants.extend((f'CSRank({expr})', f'Mean({expr},{window})', f'Delta({expr},{window})'))
    if pool:
        other = rng.choice(pool)
        other = other['expr'] if isinstance(other, dict) else other
        variants.extend(f'CSRank({expr}){op}CSRank({other})' for op in ('+', '-', '*'))
    rng.shuffle(variants)
    valid, seen = [], {original}
    for variant in variants:
        try:
            key = canonical(variant)
        except ValueError:
            continue
        if key not in seen:
            valid.append(key)
            seen.add(key)
    return valid


def model_call(workdir, prompt_path, out_path):
    command = [str(Path.home() / '.local/bin/opencode-task'), str(workdir), str(prompt_path),
               'zhipuai-coding-plan/glm-5.3-flash', str(out_path)]
    process = subprocess.Popen(command, cwd=workdir, stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, start_new_session=True)
    captured = b''
    try:
        captured, _ = process.communicate(timeout=15 * 60)
        if process.returncode:
            raise RuntimeError(f'opencode-task exited {process.returncode}')
    finally:
        # Kill the complete process group, including grandchildren after early exit.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
        if not out_path.exists():
            out_path.write_bytes(captured)


def llm_propose(market, pool_text, failed_text, n, runner, workdir, avoid_text='[]', feedback_text='[]',
                history_text=None):
    stats = dict(llm_proposed=0, llm_valid=0, llm_invalid=0, llm_error=None)
    workdir = Path(workdir).resolve()
    workdir.mkdir(parents=True, exist_ok=True)
    prompt_path, out_path = workdir / 'prompt.md', workdir / 'out.md'
    description = ('虚拟币日线，点时刻成交额前60，持有1/3/7日。' if market == 'crypto'
                   else '美股日线，SPY基准，持有5/10/20个交易日。')
    history = (f'前几晚的探索记录（每晚训练集 top5 与初筛失败数，仅 train 指标；避免重复同类失败写法）：\n'
               f'{history_text}\n' if history_text else '')
    prompt = (f'你是量化因子研究员。{description}\n{dsl_help()}\n'
              f'训练集 top-10（仅 train 指标）：\n{pool_text}\n'
              f'{history}'
              f'最近10个失败表达式：\n{failed_text}\n'
              f'本次已提出过（不要重复或仅改常数）：\n{avoid_text}\n'
              f'上一批你提出的表达式在训练集上的结果（train_t 为训练集t值；train_screen 为 pass 表示通过训练集初筛，'
              f'fail:原因 表示未通过）。沿 pass 且 train_t 高的结构继续改进，避开同类失败原因的写法：\n{feedback_text}\n'
              f'请提出最多{min(max(n, 0), 20)}条新表达式。只输出一个JSON数组 ["expr",...]，'
              '禁止解释，禁止输出代码，禁止读写其它文件。\n')
    prompt_path.write_text(prompt, encoding='utf-8')
    # A repeated, fixed run_id must not accidentally parse an earlier response.
    out_path.unlink(missing_ok=True)
    try:
        (runner or model_call)(workdir, prompt_path, out_path)
        text = out_path.read_text(encoding='utf-8')
        decoder, arrays = json.JSONDecoder(), []
        for match in re.finditer(r'\[', text):
            try:
                value, end = decoder.raw_decode(text, match.start())
                if isinstance(value, list):
                    arrays.append((end, -match.start(), value))
            except ValueError:
                continue
        if not arrays:
            raise ValueError('no JSON array in LLM output')
        proposals = max(arrays, key=lambda x: x[:2])[2][:min(max(n, 0), 20)]
        stats['llm_proposed'] = len(proposals)
        valid = []
        for expr in proposals:
            try:
                validate_expr(expr)
                valid.append(canonical(expr))
                stats['llm_valid'] += 1
            except (TypeError, ValueError):
                stats['llm_invalid'] += 1
        return valid, stats
    except Exception as exc:
        stats['llm_error'] = f'{type(exc).__name__}: {exc}'
        return [], stats


LLM_BATCH = 20
LLM_MAX_BATCHES = 10


def llm_propose_batches(market, pool_text, failed_text, n, runner, workdir, out_of_time=lambda: False,
                        after_batch=None, history_text=None):
    """One llm_propose call is capped at LLM_BATCH expressions, so a larger n is
    served by several calls; later calls are told what was already proposed.
    A failed batch is recorded in llm_error and the remaining ones still run."""
    total = dict(llm_proposed=0, llm_valid=0, llm_invalid=0, llm_error=None)
    exprs, seen = [], set()
    feedback = '[]'
    remaining = max(n, 0)
    for i in range(min(LLM_MAX_BATCHES, -(-remaining // LLM_BATCH))):
        if i and out_of_time():
            break
        batch_dir = Path(workdir) / f'batch{i}' if i else Path(workdir)
        avoid = json.dumps(exprs[-40:], ensure_ascii=False)
        got, stats = llm_propose(market, pool_text, failed_text, min(remaining, LLM_BATCH), runner, batch_dir,
                                 avoid, feedback, history_text)
        remaining -= LLM_BATCH
        for key in ('llm_proposed', 'llm_valid', 'llm_invalid'):
            total[key] += stats[key]
        if stats['llm_error']:
            total['llm_error'] = stats['llm_error'] if not total['llm_error'] else total['llm_error'] + '; ' + stats['llm_error']
        fresh = []
        for expr in got:
            if expr not in seen:
                seen.add(expr)
                exprs.append(expr)
                fresh.append(expr)
        if after_batch is not None:
            feedback = after_batch(fresh)
    return exprs, total


def summarize_outcomes(outcomes, limit=14):
    """Compact TRAIN-ONLY feedback for the next LLM batch (best first).

    Validation/holdout results, final status and val-based reasons are deliberately
    absent: showing them would let the search adapt to the validation segment."""
    def pick(o):
        return {k: (round(v, 3) if isinstance(v, float) else v) for k, v in o.items()
                if k in ('expr', 'horizon', 'train_t', 'train_ic', 'train_screen')}
    ranked = sorted(outcomes, key=lambda o: -o.get('train_t', float('-inf')))
    return [pick(o) for o in ranked[:limit]]


def frozen_hashes():
    return {'evaluator_sha256': hashlib.sha256(Path(evaluate.__file__).read_bytes()).hexdigest(),
            'expr_sha256': hashlib.sha256(Path(__file__).with_name('expr.py').read_bytes()).hexdigest()}


class Miner:
    def __init__(self, root=ROOT):
        self.root = Path(root)
        self.fetch_error = None
        self.fetch_seconds = 0.

    def run_market(self, market, *, n_mutations, n_llm, budget_s, seed, use_llm, runner=None, now=None):
        started = now() if callable(now) else now
        started = started or dt.datetime.now(dt.timezone.utc)
        run_id = f'{market}-{started.strftime("%Y%m%dT%H%M%S%fZ")}-{seed}'
        stamp = started.isoformat()
        clock = time.monotonic()
        hashes = frozen_hashes()
        row = dict(run_id=run_id, market=market, started_at=stamp, finished_at=None,
                   seconds=0., reevaluated=0, forward_updated=0, seed_evaluated=0, mutation_evaluated=0, llm_proposed=0,
                   llm_valid=0, llm_invalid=0, llm_evaluated=0, invalid=0, new_accepted=0,
                   new_candidate=0, trials_total=0, error=self.fetch_error,
                   seed=seed, budget_exhausted=False, **hashes)
        def stage(name):
            store.write_status(dict(running=True, current=dict(market=market, stage=name, started_at=stamp),
                                    **hashes), self.root)
        try:
            stage('seed')
            cfg = MARKETS[market]
            panels = load_panels(market, self.root)
            store.write_status({'markets': {market: panels.meta}}, self.root)
            splits = {h: make_splits(panels.mask.index, h, panels.windows) for h in cfg['horizons']}
            targets = {h: forward_target(panels, h) for h in cfg['horizons']}
            ledger = store.read_ledger(market, self.root)
            seen = {(r['horizon'], r['id']) for r in ledger}
            K = Counter(r['horizon'] for r in ledger)
            row['trials_total'] = sum(K.values())
            old_zoo = store.read_json(self.root / market / 'zoo.json', {'factors': []})
            factors = {(f['horizon'], f['id']): dict(f) for f in old_zoo['factors']}
            old_forward = {key: f['forward'] for key, f in factors.items() if 'forward' in f}
            old_status = {key: f['status'] for key, f in factors.items()}
            # One expression evaluation per id across refresh, redundancy,
            # forward display and ensemble construction in this run.
            values_cache = {}
            def values_for(key, expr):
                if key not in values_cache:
                    values_cache[key] = evaluate_expr(expr, panels.fields)
                return values_cache[key]
            def discard_unused_values(key, entries=None, *, finished=False):
                # Rejected expressions with no registration cannot be promoted
                # by a growing K. Do not keep their full panels for the run.
                if entries is None:
                    entries = [factors[h, key] for h in cfg['horizons'] if (h, key) in factors]
                if ((finished or all((h, key) in seen for h in cfg['horizons'])) and
                        not any('forward' in f or evaluate.classify(
                            f['train'], f['val'], K[f['horizon']], evaluate.t_required(K[f['horizon']]))
                            != 'rejected' for f in entries)):
                    values_cache.pop(key, None)
            # Refresh every retained (id, horizon) before constructing the search pool.
            # Commit the refresh as a batch: a budget stop preserves the old snapshot.
            stage('reevaluate')
            grouped = defaultdict(list)
            for key, factor in factors.items():
                grouped[factor['id']].append((key, factor))
            refreshed = {}
            for group in grouped.values():
                if time.monotonic() - clock >= budget_s:
                    row['budget_exhausted'] = True
                    return old_zoo
                try:
                    values = values_for(group[0][1]['id'], group[0][1]['expr'])
                except ExprError as exc:
                    if '缺少字段' not in str(exc):
                        raise
                    # An optional data field (e.g. $funding) vanished: keep the previous
                    # snapshot of this factor rather than crash the run or silently drop it.
                    for key, factor in group:
                        refreshed[key] = factor
                    continue
                for key, factor in group:
                    if time.monotonic() - clock >= budget_s:
                        row['budget_exhausted'] = True
                        return old_zoo
                    h = factor['horizon']
                    metrics = evaluate.evaluate_factor(values, *targets[h], panels.mask, splits[h], h, cfg)
                    row['reevaluated'] += 1
                    if time.monotonic() - clock >= budget_s:
                        row['budget_exhausted'] = True
                        return old_zoo
                    if metrics is not None:
                        refreshed[key] = dict(factor, **metrics)
                discard_unused_values(group[0][1]['id'],
                                      [refreshed[key] for key, _ in group if key in refreshed])
            factors = refreshed
            for key in list(values_cache):
                discard_unused_values(key)
            seeds = [dict(expr=s['expr'], source='seed', family=s['family']) for s in zoo.seed_factors(market)]
            rng = random.Random(seed)
            pool = []
            for h in cfg['horizons']:
                pool.extend(sorted(({k: v for k, v in f.items() if k != 'forward'}
                                    for f in factors.values() if f['horizon'] == h),
                                   key=lambda f: (-f['train']['t'], f['id']))[:20])
            pool.extend(rng.sample(seeds, min(20, len(seeds))))
            candidate_ids = set()
            def add(expr, source, category=None, into=None):
                try:
                    key = expr_hash(expr)
                except ValueError:
                    row['invalid'] += 1
                    return False
                if key in candidate_ids or all((h, key) in seen for h in cfg['horizons']):
                    return False
                candidate_ids.add(key)
                into.append(dict(expr=expr, source=source, family=category or family(expr)))
                return True
            seed_batch, mutation_batch, llm_batch = [], [], []
            for s in seeds:
                add(s['expr'], 'seed', s['family'], seed_batch)
            made = 0
            weights = None
            if options.enabled(options.FAMILY_WEIGHT):
                # Train-only ledger stats (history before this run's evaluations).
                weights = allocation.pool_weights(
                    pool, allocation.family_yield(ledger, family_fine), family_fine)
            for _ in range(max(0, n_mutations) * 10):
                if made >= n_mutations or not pool:
                    break
                parent = rng.choice(pool) if weights is None else rng.choices(pool, weights)[0]
                for expr in mutate(parent['expr'], rng, pool):
                    made += add(expr, 'mutation', None, mutation_batch)
                    if made >= n_mutations:
                        break
            store.append_ledger(market, [], self.root)

            def out_of_time():
                return time.monotonic() - clock >= budget_s

            def evaluate_batch(batch):
                """Evaluate candidates in order; returns per-candidate outcomes for feedback."""
                outcomes = []
                for i, candidate in enumerate(batch):
                    if i % 20 == 0 and out_of_time():
                        row['budget_exhausted'] = True
                        break
                    if i == 0 or candidate['source'] != batch[i-1]['source']:
                        stage(candidate['source'])
                    try:
                        key = expr_hash(candidate['expr'])
                        values = values_for(key, candidate['expr'])
                    except Exception:
                        row['invalid'] += 1
                        outcomes.append(dict(expr=candidate['expr'], status='invalid', reason='cannot_evaluate',
                                             train_screen='fail:cannot_evaluate'))
                        continue
                    valid_candidate = False
                    per_horizon = []
                    for h in cfg['horizons']:
                        if (h, key) in seen:
                            continue
                        metrics = evaluate.evaluate_factor(values, *targets[h], panels.mask, splits[h], h, cfg)
                        if metrics is None:
                            row['invalid'] += 1
                            continue
                        K[h] += 1
                        seen.add((h, key))
                        t_req = evaluate.t_required(K[h])
                        state = evaluate.classify(metrics['train'], metrics['val'], K[h], t_req)
                        per_horizon.append(dict(
                            horizon=h, status=state, train_t=metrics['train']['t'], train_ic=metrics['train']['ic'],
                            val_ic=metrics['val']['ic'], val_t=metrics['val']['t'],
                            reason=feedback.reject_reason(metrics['train'], metrics['val'], t_req),
                            train_screen=feedback.train_screen(metrics['train'], t_req)))
                        factors[h, key] = dict(id=key, **candidate, horizon=h, first_seen=started.date().isoformat(),
                                               run_id=run_id, status=state, corr_max=0., corr_with=None, **metrics)
                        if (h, key) in old_forward:
                            factors[h, key]['forward'] = old_forward[h, key]
                        trial = dict(ts=stamp, run_id=run_id, id=key, expr=candidate['expr'],
                                     source=candidate['source'], horizon=h, train_t=metrics['train']['t'],
                                     train_ic=metrics['train']['ic'], status=state)
                        # Persist each valid trial before another horizon can fail.
                        store.append_ledger(market, [trial], self.root)
                        ledger.append(trial)
                        row['trials_total'] = sum(K.values())
                        if not valid_candidate:
                            row[candidate['source'] + '_evaluated'] += 1
                        valid_candidate = True
                    discard_unused_values(key, finished=True)
                    if per_horizon:
                        best = max(per_horizon, key=lambda r: r['train_t'])
                        outcomes.append(dict(expr=candidate['expr'], **best))
                    else:
                        outcomes.append(dict(expr=candidate['expr'], status='invalid', reason='low_coverage_or_seen',
                                         train_screen='fail:low_coverage'))
                return outcomes

            evaluate_batch(seed_batch)
            if use_llm and n_llm > 0 and not out_of_time():
                stage('llm')
                top = sorted(factors.values(), key=lambda f: (-f['train']['t'], f['id']))[:10]
                # Construct the model view explicitly; never serialize zoo/ledger rows.
                pool_text = json.dumps([dict(expr=f['expr'], horizon=f['horizon'], train_t=f['train']['t'])
                                        for f in top], ensure_ascii=False)
                failures = [r['expr'] for r in ledger if r['status'] == 'rejected'][-10:]

                def after_batch(got):
                    """Evaluate one LLM batch right away; its outcomes steer the next batch."""
                    fresh = []
                    for expr in got:
                        add(expr, 'llm', None, fresh)
                    llm_batch.extend(fresh)
                    summary = summarize_outcomes(evaluate_batch(fresh))
                    if memory:
                        llm_memory.record_batch(self.root, market, run_id, batches[0], summary)
                        batches[0] += 1
                    return json.dumps(summary, ensure_ascii=False)

                memory = options.enabled(options.LLM_MEMORY)
                batches = [0]
                history_text = None
                if memory:
                    past = llm_memory.history(self.root, market, run_id)
                    history_text = json.dumps(past, ensure_ascii=False) if past else None
                _, stats = llm_propose_batches(market, pool_text, json.dumps(failures), n_llm, runner,
                                               self.root / 'llm_work' / run_id, out_of_time, after_batch,
                                               history_text)
                row.update(stats)
            evaluate_batch(mutation_batch)
            row['trials_total'] = sum(K.values())
            self._classify_and_correlate(factors, K, panels, splits, cfg, values_cache)
            for key, factor in factors.items():
                previous = old_status.get(key)
                if previous and previous != factor['status']:
                    history = list(factor.get('status_history', []))
                    history.append(dict(ts=stamp, run_id=run_id, **{'from': previous, 'to': factor['status']}))
                    factor['status_history'] = history[-5:]
                if previous != factor['status'] and factor['status'] in ('accepted', 'candidate'):
                    row['new_' + factor['status']] += 1
            # All selection is finished. Commit display updates as a batch so a
            # budget stop leaves every previous forward record intact.
            to_update = [f for f in factors.values()
                         if 'forward' in f or f['status'] == 'accepted']
            updates = {}
            if to_update:
                stage('forward')
            labels = {}
            for f in to_update:
                # Registration is cheap and must freeze the actual first
                # acceptance even if the metric update has no budget left.
                forward.register(f, splits[f['horizon']]['ranges'], started.date().isoformat())
            for f in to_update:
                h = f['horizon']
                if time.monotonic() - clock >= budget_s:
                    row['budget_exhausted'] = True
                    break
                if h not in labels:
                    start = min(item['forward']['start'] for item in to_update if item['horizon'] == h)
                    labels[h] = panel.forward_labels(panels, h, start)
                updates[h, f['id']] = forward.evaluate_forward(
                    f, values_cache[f['id']], panels, h, cfg, labels=labels[h])
                if time.monotonic() - clock >= budget_s:
                    row['budget_exhausted'] = True
                    break
            else:
                for key, result in updates.items():
                    factors[key]['forward'] = result
                row['forward_updated'] = len(updates)
            forward_counts = forward.summarize(factors.values())
            horizons = {}
            for h in cfg['horizons']:
                counts = Counter(f['status'] for f in factors.values() if f['horizon'] == h)
                # Pruned rejected rows still count toward both trials and rejections.
                counts['rejected'] += K[h] - sum(counts.values())
                horizons[str(h)] = dict(trials=K[h], t_req=evaluate.t_required(K[h]),
                                        forward=forward_counts.get(str(h), dict.fromkeys(forward.VERDICTS, 0)),
                                        **{s: counts[s] for s in ('accepted', 'candidate', 'rejected', 'redundant')})
            priority = {'accepted': 0, 'candidate': 1, 'redundant': 2, 'rejected': 3}
            ordered = sorted(factors.values(), key=lambda f: (priority[f['status']], -f['train']['t'], f['id'], f['horizon']))
            rejected = 0
            retained = []
            for f in ordered:
                if f['status'] == 'rejected':
                    rejected += 1
                    if rejected > 200:
                        continue
                retained.append(f)
            shortest = min(cfg['horizons'])
            split = {s: splits[shortest]['ranges'][s] for s in ('train', 'val')}
            split.update(rolling=splits[shortest]['rolling'], windows=panels.windows, holdout_sealed=True)
            output = dict(market=market, generated_at=stamp, split=split, horizons=horizons, factors=retained)
            # Preserve measured train/val metrics even if ensemble construction fails.
            store.write_zoo(market, output, self.root)
            stage('ensemble')
            ensemble = build_ensemble(market, retained, panels, targets, splits, cfg,
                                      values_cache=values_cache)
            store.write_ensemble(market, ensemble, self.root)
            try:
                from .live_scores import export_live_scores
                selected_ids = None
                if options.enabled(options.GREEDY_ENSEMBLE):
                    # Live reference scores must use exactly the ensemble.json factors.
                    selected_ids = {int(h): v['factor_ids'] for h, v in ensemble['horizons'].items() if v}
                export_live_scores(market, retained, panels, root=self.root, values_cache=values_cache,
                                   selected_ids=selected_ids)
            except Exception as exc:
                import sys
                print(f'live_scores export failed for {market}: {type(exc).__name__}: {exc}', file=sys.stderr)
            return output
        except BaseException as exc:
            error = f'{type(exc).__name__}: {exc}'
            row['error'] = '; '.join(filter(None, (row['error'], error)))
            raise
        finally:
            row['finished_at'] = store.timestamp()
            row['seconds'] = round(time.monotonic() - clock + self.fetch_seconds, 6)
            try:
                store.write_status(dict(running=False, current=None, **hashes), self.root)
            finally:
                store.append_run(row, self.root)

    @staticmethod
    def _classify_and_correlate(factors, K, panels, splits, cfg, values_cache=None):
        def ranks(f, dates):
            values = (values_cache[f['id']] if values_cache is not None
                      else evaluate_expr(f['expr'], panels.fields))
            values = values.loc[dates] * f['direction']
            return values.where(panels.mask.loc[dates]).rank(axis=1, method='average')
        for h in cfg['horizons']:
            visible = splits[h]['train'] | splits[h]['val']
            dates = panels.mask.index[visible][::5]
            # Only accepted factors are compared again. Keep their ranks for this
            # horizon so a large pool cannot thrash an LRU and recompute expressions.
            kept = []
            current = sorted((f for f in factors.values() if f['horizon'] == h),
                             key=lambda f: (-f['train']['t'], f['first_seen'], f['run_id'], f['id']))
            for f in current:
                f['status'] = evaluate.classify(f['train'], f['val'], K[h], evaluate.t_required(K[h]))
                f['corr_max'], f['corr_with'] = 0., None
                if f['status'] not in ('accepted', 'candidate'):
                    continue
                rank = ranks(f, dates)
                for other, other_rank in kept:
                    corr = evaluate.cross_corr(rank, other_rank, dates)
                    if corr > f['corr_max']:
                        f['corr_max'], f['corr_with'] = corr, other['id']
                if f['corr_max'] >= CORR_REDUNDANT:
                    f['status'] = 'redundant'
                elif f['status'] == 'accepted':
                    kept.append((f, rank))
