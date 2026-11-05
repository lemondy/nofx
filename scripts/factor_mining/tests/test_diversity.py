"""Synthetic-only checks for selection and shared miner integration fixtures."""
from collections import Counter
import copy
import datetime as dt
import itertools
import os
from pathlib import Path
import random
from unittest.mock import patch

import numpy as np
import pandas as pd
import pytest

from scripts.factor_mining import diversity, evaluate, miner, options, store
from scripts.factor_mining.diversity import diverse_select
from scripts.factor_mining.expr import expr_hash
from scripts.factor_mining.panel import forward_target, load_panels, make_splits
from scripts.factor_mining.tests.synth import SIGNAL_EXPR, write_market


@pytest.mark.parametrize('corr, accepted', [(.799999, True), (.8, False), (.800001, False),
                                          (-.799999, True), (-.8, False), (-.95, False)])
def test_strict_absolute_correlation_threshold(corr, accepted):
    items = [dict(expr='$close'), dict(expr='$open')]
    with patch.object(diversity, 'cross_corr', return_value=corr):
        selected, stats = diverse_select(items, 20, lambda f: f, [])
    assert len(selected) == 1 + accepted
    assert stats['n_rejected_corr'] == int(not accepted)


def test_real_rank_correlations_reject_positive_and_negative_duplicates():
    dates = pd.date_range('2020-01-01', periods=4)
    base = pd.DataFrame([[1., 2., 3., 4., 5.]] * 4, index=dates)
    frames = dict(a=base, b=base * 2, c=-base, d=base[[2, 4, 0, 3, 1]].set_axis(base.columns, axis=1))
    items = [dict(expr=k) for k in frames]
    selected, stats = diverse_select(items, 20, lambda f: frames[f['expr']], dates,
                                     family_fn=lambda e: e)
    assert [f['expr'] for f in selected] == ['a', 'd']
    assert stats['n_rejected_corr'] == 2


def test_family_quota_and_rank_laziness():
    items = [dict(expr=f'Mean($close,{w})') for w in range(2, 10)] + [dict(expr='Std($close,5)')]
    ranked = []
    with patch.object(diversity, 'cross_corr', return_value=0):
        selected, stats = diverse_select(items, 20, lambda f: ranked.append(f) or f, [])
    assert selected == items[:4] + items[-1:]
    assert ranked == selected
    assert stats == dict(n_scanned=9, n_selected=5, n_rejected_corr=0, n_unscorable=0,
                         n_rejected_family=4, n_families=2)


def test_scan_cap_and_lazy_input():
    seen = []

    def items():
        for i in range(400):
            seen.append(i)
            yield dict(expr=str(i))

    with patch.object(diversity, 'cross_corr', return_value=0):
        selected, stats = diverse_select(items(), 400, lambda f: f, [], family_fn=lambda e: e)
    assert len(selected) == stats['n_scanned'] == 300
    assert seen == list(range(300))


def test_k_stops_ranking_and_is_deterministic():
    items = [dict(expr=str(i)) for i in range(10)]
    calls = []
    with patch.object(diversity, 'cross_corr', return_value=0):
        a = diverse_select(items, 3, lambda f: calls.append(f) or f, [], family_fn=lambda e: e)
        b = diverse_select(items, 3, lambda f: f, [], family_fn=lambda e: e)
        empty = diverse_select(items, 0, lambda f: pytest.fail('ranked beyond k'), [])
    assert a == b
    assert a[0] == calls == items[:3]
    assert a[1]['n_scanned'] == 3
    assert empty[0] == [] and empty[1]['n_scanned'] == 0


def parent_case(root, *, enabled, poison=False, llm=False):
    """Use real train metrics with controlled factor panels, never real data.

    Thirty legal expressions include independent same-family candidates and
    perfectly correlated/anti-correlated pairs. The old zoo carries deliberately
    stale metrics so the assertions also cover refresh-before-selection.
    """
    write_market(root, rows=240, columns=20)
    panels = load_panels('crypto', root)
    horizons = miner.MARKETS['crypto']['horizons']
    splits = {h: make_splits(panels.mask.index, h, panels.windows) for h in horizons}
    targets = {h: forward_target(panels, h) for h in horizons}
    expressions = ([f'Mean($close,{i})' for i in range(2, 16)] +
                   [f'Std($close,{i})' for i in range(2, 10)] +
                   [f'Corr($close,$volume,{i})' for i in range(12, 20)])
    rng = np.random.default_rng(41)
    frames = {e: pd.DataFrame(rng.normal(size=panels.mask.shape),
                              index=panels.mask.index, columns=panels.mask.columns) for e in expressions}
    frames[expressions[1]] = frames[expressions[0]] * 2
    frames[expressions[2]] = -frames[expressions[0]]
    frames[expressions[3]] = frames[expressions[0]].copy()
    # Masked-out names must not be allowed to create apparent diversity.
    panels.mask.iloc[:, :2] = False
    frames[expressions[3]].iloc[:, :2] = rng.normal(size=(len(panels.mask), 2)) * 1000
    if poison:
        val = splits[min(horizons)]['val']
        for frame in frames.values():
            frame.loc[val] = rng.normal(size=frame.loc[val].shape) * 1000
    refreshed, old = [], []
    for h in horizons:
        for e in expressions:
            metrics = evaluate.evaluate_factor(frames[e], *targets[h], panels.mask, splits[h], h,
                                               miner.MARKETS['crypto'])
            assert metrics is not None
            f = dict(expr=e, id=expr_hash(e), horizon=h, source='historical', family=miner.family(e),
                     first_seen='2019-01-01', run_id='old', status='rejected', corr_max=0., corr_with=None,
                     **metrics)
            refreshed.append(f)
            old.append(dict(f, train=dict(f['train'], t=-999.)))
    store.write_zoo('crypto', dict(factors=old), root)
    store.append_ledger('crypto', [dict(horizon=f['horizon'], id=f['id'], expr=f['expr'],
                                       status='rejected', run_id='old') for f in old], root)
    seeds = [dict(expr=e, family=miner.family(e)) for e in expressions[:23]]
    seed_pool = [dict(expr=s['expr'], family=s['family'], source='seed') for s in seeds]
    legacy_rng = random.Random(41)
    legacy_pool = list(itertools.chain.from_iterable(
        sorted((f for f in refreshed if f['horizon'] == h), key=lambda f: (-f['train']['t'], f['id']))[:20]
        for h in horizons))
    legacy_pool += legacy_rng.sample(seed_pool, 20)
    legacy_parent = legacy_rng.choice(legacy_pool)
    captured = {}

    def mutate(expr, rng, pool):
        captured.update(pool=copy.deepcopy(pool), parent=expr, rng_state=rng.getstate())
        return [SIGNAL_EXPR]

    def model(work, prompt, out):
        captured['prompt'] = prompt.read_text(encoding='utf-8')
        out.write_text('[]', encoding='utf-8')

    original_eval = miner.evaluate_expr
    counts = Counter()

    def values(expr, fields):
        counts[expr] += 1
        return frames[expr] if expr in frames else original_eval(expr, fields)

    env = {options.DIVERSE_PARENTS: '1' if enabled else '0',
           options.FAMILY_WEIGHT: '0', options.LLM_MEMORY: '0', options.GREEDY_ENSEMBLE: '0'}
    with patch.dict(os.environ, env), patch.object(miner, 'load_panels', return_value=panels), \
         patch.object(miner, 'evaluate_expr', side_effect=values), \
         patch.object(miner.zoo, 'seed_factors', return_value=seeds), \
         patch.object(miner, 'mutate', side_effect=mutate):
        miner.Miner(root).run_market(
            'crypto', n_mutations=1, n_llm=int(llm), budget_s=120, seed=41, use_llm=llm, runner=model,
            now=dt.datetime(2026, 11, 6, 19, tzinfo=dt.timezone.utc))
    return dict(captured=captured, row=store.read_jsonl(Path(root) / 'runs.jsonl')[-1],
                panels=panels, splits=splits, frames=frames, refreshed=refreshed, counts=counts,
                legacy_pool=legacy_pool, legacy_parent=legacy_parent, legacy_rng=legacy_rng)



def test_none_rank_is_skipped_and_counted():
    dates = pd.date_range('2024-01-01', periods=3)
    ranks = {'a': None, 'b': pd.DataFrame([[1., 2.], [1., 2.], [1., 2.]], index=dates)}
    items = [dict(expr='$close', id='a'), dict(expr='$open', id='b')]
    chosen, stats = diversity.diverse_select(items, 2, lambda f: ranks[f['id']], dates)
    assert [f['id'] for f in chosen] == ['b']
    assert stats['n_unscorable'] == 1
