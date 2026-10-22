"""Latest closed-day ensemble scores for reference only; never used in selection.

Factors whose forward validation FAILED are left out of these scores (see the guard below);
ensemble.json itself is untouched by forward results."""
from collections import Counter
import datetime as dt
from pathlib import Path

import numpy as np
import pandas as pd

from . import store
from .config import MARKETS
from .data_fetch import ROOT
from .ensemble import ensemble_scores, select_ensemble_factors


def export_live_scores(market, factors, panels, *, root=ROOT, values_cache=None, now=None, selected_ids=None):
    """Atomically write a snapshot, or remove a stale file if no horizon is usable.

    Use the latest panel date strictly before today in UTC, without falling back
    to older dates for thin cross sections. ``universe_size`` counts the mask's
    eligible names on that date, before filtering each horizon's finite scores.
    Inputs, including the caller's expression cache, are never modified.
    selected_ids ({horizon: [factor ids]}, e.g. from ensemble.json) pins the factors
    per horizon instead of the default top-20 accepted; a horizon missing from it is skipped.
    """
    generated = pd.Timestamp(now if now is not None else dt.datetime.now(dt.timezone.utc))
    generated = (generated.tz_localize('UTC') if generated.tzinfo is None
                 else generated.tz_convert('UTC'))
    path = Path(root) / market / 'live_scores.json'
    dates = panels.mask.index[pd.to_datetime(panels.mask.index, utc=True) < generated.normalize()]
    if not len(dates):
        path.unlink(missing_ok=True)
        return None
    as_of = dates.max()
    mask = panels.mask.loc[as_of]
    output = dict(version=1, market=market, generated_at=generated.isoformat(),
                  as_of=as_of.strftime('%Y-%m-%d'), universe_size=int(mask.sum()), horizons={})
    for h in MARKETS[market]['horizons']:
        if selected_ids is None:
            selected = select_ensemble_factors(factors, h)
        else:
            by_id = {f['id']: f for f in factors if f['horizon'] == h}
            selected = [by_id[i] for i in selected_ids.get(h) or selected_ids.get(str(h)) or [] if i in by_id]
        # Consumption-layer guard, NOT ensemble selection: ensemble.json / zoo stay forward-blind
        # (forward results must not shape selection), but a factor whose forward validation has
        # FAILED is not shown to the AI as evidence. Its own forward evaluation keeps running.
        excluded = sum((f.get('forward') or {}).get('verdict') == 'failed' for f in selected)
        selected = [f for f in selected if (f.get('forward') or {}).get('verdict') != 'failed']
        if not selected:
            continue
        row = ensemble_scores(selected, panels, values_cache=values_cache).loc[as_of]
        row = row[mask & np.isfinite(row)]
        if len(row) < 10:
            continue
        percentiles = row.rank(pct=True)
        output['horizons'][str(h)] = dict(
            factor_count=len(selected),
            forward_failed_excluded=excluded,
            forward=dict(Counter((f.get('forward') or {}).get('verdict', 'none') for f in selected)),
            scores={str(symbol): dict(z=round(float(z), 4), pct=round(float(percentiles[symbol]), 4))
                    for symbol, z in row.items()})
    if not output['horizons']:
        path.unlink(missing_ok=True)
        return None
    store.write_json(path, output)
    return output
