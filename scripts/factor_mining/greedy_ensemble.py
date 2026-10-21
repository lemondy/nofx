"""Incremental greedy ensemble selection (opt-in via FM_GREEDY_ENSEMBLE=1).

Walks candidates by train t (descending) and keeps one only if the equal-weight
ensemble's TRAIN-segment rank-IC Newey-West t improves by at least MIN_GAIN.
Labels outside train_mask are never touched: every frame is sliced to the
training rows before any statistic.
"""
import numpy as np

from .evaluate import cs_rank_ic, nw_t
from .expr import evaluate_expr

MAX_FACTORS = 20
MIN_GAIN = 0.01


def greedy_ensemble_factors(candidates, panels, excess, train_mask, h, cfg, values_cache=None):
    train = np.asarray(train_mask, dtype=bool)
    if not train.any():
        return []
    label = excess.loc[train]
    mask = panels.mask.loc[train]
    total = count = None
    kept, best_t = [], None
    for factor in sorted(candidates, key=lambda f: (-f['train']['t'], f['id'])):
        if len(kept) >= MAX_FACTORS:
            break
        values = (values_cache[factor['id']] if values_cache is not None and factor['id'] in values_cache
                  else evaluate_expr(factor['expr'], panels.fields))
        # Same transform as ensemble.ensemble_scores, on the training rows only.
        values = (values.loc[train] * factor['direction']).where(mask)
        centered = values.sub(values.mean(axis=1), axis=0)
        z = centered.div(values.std(axis=1).where(lambda x: x > 1e-15), axis=0)
        new_total = z.fillna(0.) if total is None else total + z.fillna(0.)
        new_count = z.notna().astype(float) if count is None else count + z.notna()
        score = new_total.div(new_count.where(new_count > 0))
        ic = cs_rank_ic(score, label, mask, cfg['min_names'])
        t = nw_t(ic.dropna(), h)
        if best_t is None or (t > best_t and t >= best_t + MIN_GAIN * abs(best_t)):
            kept.append(factor)
            total, count, best_t = new_total, new_count, t
    return kept
