"""Equal-weight, direction-adjusted cross-sectional z-score ensembles."""
import numpy as np

from . import options
from .evaluate import evaluate_factor, long_short
from .expr import evaluate_expr
from .greedy_ensemble import greedy_ensemble_factors


def ensemble_scores(factors, panels, *, values_cache=None):
    total = panels.fields['close'] * 0.0
    total[:] = 0.0
    count = total.copy()
    for factor in factors:
        values = (values_cache[factor['id']] if values_cache is not None
                  else evaluate_expr(factor['expr'], panels.fields))
        values = (values * factor['direction']).where(panels.mask)
        centered = values.sub(values.mean(axis=1), axis=0)
        z = centered.div(values.std(axis=1).where(lambda x: x > 1e-15), axis=0)
        total += z.fillna(0.)
        count += z.notna()
    return total.div(count.where(count > 0))


def select_ensemble_factors(accepted, horizon, limit=20):
    return sorted((f for f in accepted if f['horizon'] == horizon and f['status'] == 'accepted'),
                  key=lambda f: (-f['train']['t'], f['id']))[:limit]


def build_ensemble(market, accepted, panels, targets, splits, cfg, *, values_cache=None):
    output = {'horizons': {}}
    for h in cfg['horizons']:
        if options.enabled(options.GREEDY_ENSEMBLE) and targets and 'train' in splits[h]:
            factors = greedy_ensemble_factors(select_ensemble_factors(accepted, h, None), panels,
                                              targets[h][0], splits[h]['train'], h, cfg, values_cache)
        else:
            factors = select_ensemble_factors(accepted, h)
        if not factors:
            output['horizons'][str(h)] = None
            continue
        score = ensemble_scores(factors, panels, values_cache=values_cache)
        excess, raw = targets[h]
        metrics = evaluate_factor(score, excess, raw, panels.mask, splits[h], h, dict(cfg, direction=1))
        if metrics is None:
            output['horizons'][str(h)] = None
            continue
        net, _ = long_short(score, raw, panels.mask, splits[h]['val'], h, cfg)
        # The initial capital point precedes the first holding-period return.
        start = splits[h]['ranges']['val'][0]
        curve = [[start, 1.0]] if start is not None else []
        curve.extend([d.strftime('%Y-%m-%d'), float(v)]
                     for d, v in zip(net.index, np.cumprod(1 + net.to_numpy())))
        output['horizons'][str(h)] = dict(factor_ids=[f['id'] for f in factors],
                                         train=metrics['train'], val=metrics['val'], curve={'val': curve})
    return output
