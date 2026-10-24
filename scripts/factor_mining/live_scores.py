"""Latest closed-day ensemble scores for reference only; never used in selection.

Factors whose forward validation FAILED are left out of these scores (see the guard below);
ensemble.json itself is untouched by forward results."""
from collections import Counter
import copy
import datetime as dt
import os
from pathlib import Path
import sys

import numpy as np
import pandas as pd

from . import store
from .config import MARKETS
from .data_fetch import ROOT
from .ensemble import ensemble_scores, select_ensemble_factors
from .expr import evaluate_expr
from .panel import ALT_FIELD_MARKETS, FIELD_NAMES, Panels, _read_alt, _read_symbol


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


def load_scoring_panels(core, symbols, *, root=ROOT, as_of, now):
    """Separate scoring-only panel; core calendar/mask are copied, never rebuilt.

    Existing core columns outside the as_of mask are extension candidates too;
    their observed closes come directly from the panel, without extra file I/O.
    CSRank/CSZ/CSDemean see the additional columns. This can change ext_scores
    and detail, including core detail, but NEVER the legacy core scores: those
    are exported above on the original panel/cache and only copied below.
    """
    from .ext_universe import valid_symbol

    as_of = pd.Timestamp(as_of)
    generated = pd.Timestamp(now)
    generated = (generated.tz_localize('UTC') if generated.tzinfo is None
                 else generated.tz_convert('UTC'))
    latest_closed = generated.normalize().tz_localize(None) - pd.Timedelta(days=1)
    base = Path(root) / 'crypto/ext'
    frames, skipped = {}, {}
    extended = []
    minimum = MARKETS['crypto']['min_history']
    mask = core.mask.loc[as_of]
    for symbol in mask.index[~mask]:
        observed = core.fields['close'].loc[:as_of, symbol].dropna()
        if observed.empty:
            skipped[symbol] = 'no_data'
        elif len(observed) < minimum:
            skipped[symbol] = f'history<{minimum}d'
        elif as_of not in observed.index or as_of < latest_closed:
            skipped[symbol] = 'stale'
        else:
            extended.append(symbol)
    for symbol in dict.fromkeys(symbols):
        if symbol in core.mask.columns:
            continue
        if not valid_symbol(symbol):
            skipped[symbol] = 'no_data'
            continue
        frame = _read_symbol(base / 'daily' / f'{symbol}.json').loc[:as_of]
        if frame.empty:
            skipped[symbol] = 'no_data'
        elif len(frame) < minimum:
            skipped[symbol] = f'history<{minimum}d'
        elif as_of not in frame.index or as_of < latest_closed:
            skipped[symbol] = 'stale'
        else:
            frames[symbol] = frame
    columns = list(core.mask.columns) + list(frames)
    calendar = core.mask.index
    fields = {}
    for name in FIELD_NAMES:
        extra = pd.DataFrame({s: f[name].reindex(calendar) for s, f in frames.items()}, index=calendar)
        fields[name] = pd.concat([core.fields[name], extra], axis=1).reindex(columns=columns)
    for name in ALT_FIELD_MARKETS['crypto']:
        alt = {s: _read_alt(base / 'alt_daily' / name / f'{s}.json') for s in frames}
        if name in core.fields or any(len(a) for a in alt.values()):
            original = core.fields.get(name, pd.DataFrame(index=calendar, columns=core.mask.columns,
                                                          dtype=float))
            extra = pd.DataFrame({s: a.reindex(calendar) for s, a in alt.items()}, index=calendar)
            fields[name] = pd.concat([original, extra], axis=1).reindex(columns=columns)
    mask = core.mask.reindex(columns=columns, fill_value=False).copy()
    return (Panels(fields, mask, core.benchmark_close, core.benchmark_open, core.meta, core.windows),
            extended + list(frames), skipped)


def _selected_live_factors(factors, horizon, selected_ids):
    if selected_ids is None:
        selected = select_ensemble_factors(factors, horizon)
    else:
        by_id = {f['id']: f for f in factors if f['horizon'] == horizon}
        ids = selected_ids.get(horizon) or selected_ids.get(str(horizon)) or []
        selected = [by_id[i] for i in ids if i in by_id]
    return [f for f in selected if (f.get('forward') or {}).get('verdict') != 'failed']


def _extended_snapshot(snapshot, factors, core, symbols, *, root, selected_ids, now):
    output = copy.deepcopy(snapshot)
    as_of = pd.Timestamp(output['as_of'])
    scoring, extended, skipped = load_scoring_panels(core, symbols, root=root, as_of=as_of, now=now)
    mask = scoring.mask.loc[as_of]
    metadata, scored = {}, set()
    for horizon, entry in output['horizons'].items():
        selected = _selected_live_factors(factors, int(horizon), selected_ids)
        if len(selected) != entry['factor_count']:
            raise ValueError(f'factor selection does not match snapshot horizon {horizon}')
        ids = [f['id'] for f in selected]
        total = pd.Series(0., index=mask.index)
        count = total.copy()
        factor_z = {}
        for factor in selected:
            # Recompute only in this isolated panel. Never reuse or mutate the
            # mining expression cache, which has a different set of columns.
            values = evaluate_expr(factor['expr'], scoring.fields).loc[as_of] * factor['direction']
            values = values.where(np.isfinite(values))
            reference = values.where(mask)
            deviation = reference.std()
            z = ((values - reference.mean()) / deviation if deviation > 1e-15
                 else pd.Series(np.nan, index=values.index))
            z = z.where(np.isfinite(z))
            total += z.fillna(0.)
            count += z.notna()
            factor_z[factor['id']] = z
            validation = factor.get('val') or {}
            verdict = (factor.get('forward') or {}).get('verdict', 'none')
            item = dict(
                family=factor.get('family'), horizon=factor['horizon'], direction=factor['direction'],
                expr=factor['expr'][:120], val_t=validation.get('t'), val_ic=validation.get('ic'),
                forward=verdict if verdict in {'pending', 'confirmed', 'inconclusive', 'failed'} else 'none')
            if factor['id'] not in metadata:
                metadata[factor['id']] = item
            else:
                # IDs identify expressions, and can recur with different
                # direction/validation/forward metadata at another horizon.
                # Keep the first occurrence's contracted fields stable, and
                # retain every occurrence in an additive map on collisions.
                first = metadata[factor['id']]
                if 'by_horizon' not in first:
                    first['by_horizon'] = {str(first['horizon']): first.copy()}
                first['by_horizon'][horizon] = item
        combined = total.div(count.where(count > 0))
        reference = combined[mask & np.isfinite(combined)]
        ext_scores = {}
        if len(reference):
            for symbol in extended:
                if count[symbol] * 2 < len(selected) or not np.isfinite(combined[symbol]):
                    continue
                value = float(combined[symbol])
                # Empirical CDF of core-mask ensemble z's, without rounding and
                # without including the extension symbol itself (ties use <=).
                ext_scores[symbol] = dict(z=round(value, 4),
                                          pct=round(float((reference <= value).mean()), 4))
        scored.update(ext_scores)
        detail = {}
        for symbol in list(mask.index[mask]) + list(ext_scores):
            detail[symbol] = {key: round(float(z[symbol]), 2) for key, z in factor_z.items()
                              if np.isfinite(z[symbol])}
        entry.update(factor_ids=ids, ext_scores=ext_scores, detail=detail)
    skipped.update({s: 'factors<half' for s in extended if s not in scored})
    output.update(universe=dict(core=output['universe_size'], extended=len(scored)),
                  factors=metadata, skipped=skipped)
    return output


def extend_live_scores(factors, core, symbols, *, root=ROOT, selected_ids=None,
                       now=None, not_before=None, log=None):
    """Atomically append scoring evidence; on any exception leave the file intact.

    The original exporter and every legacy field/value remain untouched. The
    optional freshness guard avoids enriching an old snapshot if tonight's
    core export failed. FM_EXT_UNIVERSE=0 performs no I/O and adds no fields.
    """
    if os.environ.get('FM_EXT_UNIVERSE', '1') == '0':
        return None
    log = log or (lambda message: print(f'crypto ext: {message}', file=sys.stderr))
    try:
        path = Path(root) / 'crypto/live_scores.json'
        snapshot = store.read_json(path)
        if not snapshot or snapshot.get('market') != 'crypto' or not snapshot.get('horizons'):
            return None
        if not_before is not None and pd.Timestamp(snapshot['generated_at']) < pd.Timestamp(not_before):
            log('old live snapshot retained; core export did not complete this run')
            return None
        output = _extended_snapshot(snapshot, factors, core, symbols, root=root,
                                    selected_ids=selected_ids, now=now or snapshot['generated_at'])
        store.write_json(path, output)
        return output
    except Exception as exc:
        log(f'live score extension skipped: {type(exc).__name__}: {exc}')
        return None
