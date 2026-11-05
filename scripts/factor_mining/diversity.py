"""Deterministic, train-only greedy selection from a best-first item stream."""
from collections import Counter
from itertools import islice

from .evaluate import cross_corr
from .families import family_fine


def diverse_select(items, k, rank_of, dates, *, max_corr=0.8, per_family=4,
                   scan=300, family_fn=family_fine):
    """Return selected expression mappings and mutually exclusive scan counts.

    The caller owns ordering, dates, masking and direction. No rank is requested
    outside the scanned prefix (or after k selections). Family rejection takes
    precedence, saving rank work when a quota is already full. Only accepted
    ranks are retained; rejected panels can be released immediately.
    """
    selected, ranks, families = [], [], Counter()
    stats = dict(n_scanned=0, n_selected=0, n_rejected_corr=0,
                 n_rejected_family=0, n_unscorable=0, n_families=0)
    if k <= 0:
        return selected, stats
    for item in islice(items, max(0, scan)):
        stats['n_scanned'] += 1
        family = family_fn(item['expr'])
        if families[family] >= per_family:
            stats['n_rejected_family'] += 1
            continue
        rank = rank_of(item)
        if rank is None:
            # e.g. an optional data field vanished; never crash parent selection.
            stats['n_unscorable'] += 1
            continue
        # Strict inequality deliberately rejects both +0.8 and -0.8.
        if any(abs(cross_corr(rank, other, dates)) >= max_corr for other in ranks):
            stats['n_rejected_corr'] += 1
            continue
        selected.append(item)
        ranks.append(rank)
        families[family] += 1
        if len(selected) >= k:
            break
    stats.update(n_selected=len(selected), n_families=len(families))
    return selected, stats
