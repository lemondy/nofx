"""Diagnostic: overlapping-label RankIC t inflation in the frozen miner evaluator.

Read-only on data/factor_mining. Compares evaluate.nw_t(lag=h), NW lag=4h, plain t
and non-overlapping (every h-th day) t, plus archived H1 uniform HAC z,
alternative B z and evaluator z, on (a) the zoo's accepted factors and
(b) random factors independent of returns (iid and AR(1) phi=0.98) evaluated on
the real train-segment targets. See docs/research/2026-11-08-ic-overlap-audit.md.

  python -m scripts.factor_mining.ic_overlap_audit <crypto|us> <seeds> <out.json>
"""
import json
import math
from statistics import NormalDist
import sys
import time

import numpy as np
import pandas as pd

from . import evaluate
from .config import MARKETS
from .data_fetch import ROOT
from .expr import evaluate_expr
from .panel import forward_target, load_panels, make_splits


def uniform_hac_variance(x, h):
    """Return uniform HAC lag=h variance and whether Bartlett lag=2h was used."""
    x = np.asarray(x, dtype=float)
    n = len(x)
    if not n:
        return 0.0, True
    centered = x - x.mean()
    gamma0 = float(np.dot(centered, centered) / n)
    variance = gamma0
    for k in range(1, min(int(h), n - 1) + 1):
        variance += 2 * float(np.dot(centered[k:], centered[:-k]) / n)
    if variance > 1e-30:
        return variance, False
    lag = 2 * h
    variance = gamma0
    for k in range(1, min(int(lag), n - 1) + 1):
        variance += 2 * (1 - k / (lag + 1)) * float(np.dot(centered[k:], centered[:-k]) / n)
    return variance, True


def uniform_hac_t(series, h) -> float:
    """Archived primary H1 statistic, which failed null calibration.

    Uniform HAC t with overlap-window df, expressed on the normal z scale.
    """
    x = np.asarray(series, dtype=float)
    x = x[np.isfinite(x)]
    n = len(x)
    if n < 20:
        return 0.0
    mean = float(x.mean())
    variance, _ = uniform_hac_variance(x, h)
    if variance <= 1e-30:
        return math.copysign(1e12, mean) if mean else 0.0
    raw = float(mean / math.sqrt(variance / n))
    sf = evaluate.student_t_sf(abs(raw), max(1, n // h - 1))
    if sf == 0:
        return raw
    # Symmetry avoids losing the upper tail when 1-sf rounds to one.
    return math.copysign(-NormalDist().inv_cdf(sf), raw)


def plain_t(x):
    x = np.asarray(x, float)
    x = x[np.isfinite(x)]
    if len(x) < 5:
        return float('nan')
    sd = x.std(ddof=1)
    return float(x.mean() / (sd / math.sqrt(len(x)))) if sd > 0 else float('nan')


def nonoverlap_ts(ic, h):
    x = ic.to_numpy()
    return [plain_t(x[o::h]) for o in range(h)]


def nonoverlap_z(ic, h):
    """Alternative B uses calendar offset zero and df after removing NaNs."""
    x = np.asarray(ic, dtype=float)[::h]
    x = x[np.isfinite(x)]
    raw = plain_t(x)
    if not math.isfinite(raw):
        return raw
    sf = evaluate.student_t_sf(abs(raw), len(x) - 1)
    return math.copysign(-NormalDist().inv_cdf(sf), raw) if sf else raw


def acf(ic, lags):
    x = ic.dropna().to_numpy()
    x = x - x.mean()
    d = float(np.dot(x, x))
    return [float(np.dot(x[k:], x[:-k]) / d) for k in lags]


def stats(ic, h):
    no = nonoverlap_ts(ic, h)
    return dict(n=int(ic.notna().sum()), ic=float(ic.mean()),
                nw_h=evaluate.nw_t(ic.dropna(), h),
                nw_4h=evaluate.nw_t(ic.dropna(), 4 * h),
                hac=uniform_hac_t(ic, h), nonoverlap_z=nonoverlap_z(ic, h),
                evaluator=evaluate.overlap_t(ic, h),
                plain=plain_t(ic),
                nonoverlap_median=float(np.nanmedian(no)), nonoverlap_min=float(np.nanmin(no)),
                nonoverlap_max=float(np.nanmax(no)))


def ar1_panel(rng, shape, phi):
    eps = rng.standard_normal(shape)
    out = np.empty(shape)
    out[0] = eps[0]
    s = math.sqrt(1 - phi * phi)
    for i in range(1, shape[0]):
        out[i] = phi * out[i - 1] + s * eps[i]
    return out


def main():
    market, seeds, out = sys.argv[1], int(sys.argv[2]), sys.argv[3]
    cfg = MARKETS[market]
    t0 = time.time()
    panels = load_panels(market)
    zoo = json.load(open(ROOT / market / 'zoo.json', encoding='utf-8'))
    accepted = [f for f in zoo['factors'] if f.get('status') == 'accepted']
    result = dict(market=market, horizons=list(cfg['horizons']), accepted=[], null={})
    splits = {h: make_splits(panels.mask.index, h, panels.windows) for h in cfg['horizons']}
    targets = {h: forward_target(panels, h) for h in cfg['horizons']}
    print('loaded', round(time.time() - t0, 1), 's', flush=True)

    for f in accepted:
        h = f['horizon']
        values = evaluate_expr(f['expr'], panels.fields) * f['direction']
        ic = evaluate.cs_rank_ic(values, targets[h][0], panels.mask, cfg['min_names'])
        row = dict(id=f['id'], h=h, expr=f['expr'], zoo_train_t=f['train']['t'], zoo_val_t=f['val']['t'])
        for seg in ('train', 'val'):
            row[seg] = stats(ic.loc[splits[h][seg]], h)
        row['acf'] = dict(zip([1, h // 2 or 1, h, 2 * h], acf(ic.loc[splits[h]['train']], [1, h // 2 or 1, h, 2 * h])))
        result['accepted'].append(row)
        print(f['id'], h, {k: round(v, 2) for k, v in row['train'].items() if isinstance(v, float)}, flush=True)

    # Null: random factors independent of returns, evaluated on real targets.
    shape = panels.mask.shape
    for kind, phi in (('iid', 0.0), ('ar1_0.98', 0.98)):
        for h in cfg['horizons']:
            result['null'][f'{kind}_h{h}'] = dict(nw_h=[], nw_4h=[], plain=[], nonoverlap0=[],
                                                hac=[], nonoverlap_z=[], evaluator=[], ic=[], fallback_count=0)
        for s in range(seeds):
            rng = np.random.default_rng(20261108 + s)
            vals = rng.standard_normal(shape) if phi == 0 else ar1_panel(rng, shape, phi)
            vals = pd.DataFrame(vals, index=panels.mask.index, columns=panels.mask.columns)
            for h in cfg['horizons']:
                ic = evaluate.cs_rank_ic(vals, targets[h][0], panels.mask, cfg['min_names'])
                tr = ic.loc[splits[h]['train']]
                d = result['null'][f'{kind}_h{h}']
                d['nw_h'].append(evaluate.nw_t(tr.dropna(), h))
                d['nw_4h'].append(evaluate.nw_t(tr.dropna(), 4 * h))
                d['plain'].append(plain_t(tr))
                d['nonoverlap0'].append(plain_t(tr.to_numpy()[::h]))
                d['hac'].append(uniform_hac_t(tr, h))
                d['evaluator'].append(evaluate.overlap_t(tr, h))
                d['nonoverlap_z'].append(nonoverlap_z(tr, h))
                sample = tr.dropna().to_numpy()
                if len(sample) >= 20:
                    _, fallback = uniform_hac_variance(sample, h)
                    d['fallback_count'] += int(fallback)
                d['ic'].append(float(tr.mean()))
            if s % 50 == 0:
                print(kind, s, round(time.time() - t0, 1), 's', flush=True)
    summary = {}
    for key, d in result['null'].items():
        summary[key] = {m: dict(rej196=float(np.mean(np.abs(v) > 1.96)),
                                rej165_onesided=float(np.mean(np.asarray(v) > 1.65)),
                                sd=float(np.std(v)))
                        for m, v in d.items() if m not in ('ic', 'fallback_count')}
        summary[key]['fallback_count'] = d['fallback_count']
        summary[key]['ic_abs_p95'] = float(np.quantile(np.abs(d['ic']), 0.95))
    result['null_summary'] = summary
    result['seeds'] = seeds
    result['train_range'] = {h: splits[h]['ranges']['train'] for h in cfg['horizons']}
    with open(out, 'w', encoding='utf-8') as fh:
        json.dump(result, fh, indent=1)
    print(json.dumps(summary, indent=1))


if __name__ == '__main__':
    main()
