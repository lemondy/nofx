"""Family-weighted parent selection for mutation (opt-in via FM_FAMILY_WEIGHT=1).

Only TRAIN statistics (ledger train_t) are used; validation results never enter.
"""
from collections import Counter

SMOOTHING = 10


def family_yield(ledger, family_fn, pass_t=3.0):
    """Per-family share of ledger trials with train_t >= pass_t, shrunk toward the
    overall share with SMOOTHING pseudo-trials so tiny families do not swing."""
    trials, passes = Counter(), Counter()
    for row in ledger:
        t = row.get('train_t')
        if t is None:
            continue
        family = family_fn(row['expr'])
        trials[family] += 1
        passes[family] += t >= pass_t
    total = sum(trials.values())
    prior = sum(passes.values()) / total if total else 0.
    return {f: (passes[f] + SMOOTHING * prior) / (n + SMOOTHING) for f, n in trials.items()}


def pool_weights(pool, yields, family_fn, explore=0.3):
    """Selection probabilities for pool items: `explore` mass uniform, the rest
    proportional to the item's family yield. Sums to 1, every item > 0."""
    if not pool:
        return []
    uniform = 1 / len(pool)
    raw = [yields.get(family_fn(item['expr']), 0.) for item in pool]
    total = sum(raw)
    if total <= 0:
        return [uniform] * len(pool)
    return [explore * uniform + (1 - explore) * r / total for r in raw]
