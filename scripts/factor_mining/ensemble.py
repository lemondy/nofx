"""Equal-weight, direction-adjusted cross-sectional z-score ensembles."""
import math
from statistics import NormalDist

import numpy as np
import pandas as pd

from . import options
from .evaluate import evaluate_factor, long_short
from .expr import evaluate_expr
from .greedy_ensemble import greedy_ensemble_factors

DEFAULT_IC_DEDUP_MAX_CORR = 0.50
DEFAULT_IC_DEDUP_MIN_OVERLAP = 12
DEFAULT_DSR_MIN_PROB = 0.95
_EULER_GAMMA = 0.5772156649015329
_NORM = NormalDist()


def _ic_monthly_map(factor, train_end_ym=None):
    out = {}
    for row in factor.get('ic_monthly') or []:
        if not isinstance(row, (list, tuple)) or len(row) < 2:
            continue
        ym, val = str(row[0]), row[1]
        if train_end_ym is not None and ym > train_end_ym:
            continue
        if isinstance(val, (int, float)) and math.isfinite(val):
            out[ym] = float(val)
    return out


def _pair_monthly_corr(map_a, map_b, min_overlap=DEFAULT_IC_DEDUP_MIN_OVERLAP):
    common = sorted(set(map_a) & set(map_b))
    if len(common) < min_overlap:
        return None
    a = np.array([map_a[m] for m in common], dtype=float)
    b = np.array([map_b[m] for m in common], dtype=float)
    sa, sb = float(a.std(ddof=1)), float(b.std(ddof=1))
    if sa <= 1e-15 or sb <= 1e-15:
        return None
    return float(np.corrcoef(a, b)[0, 1])


def deduplicate_by_ic_corr(
    candidates,
    *,
    max_corr=DEFAULT_IC_DEDUP_MAX_CORR,
    min_overlap=DEFAULT_IC_DEDUP_MIN_OVERLAP,
    train_end_ym=None,
):
    """Greedy train-only monthly-IC deduplication (keeps factor iff |corr| < max_corr with all kept)."""
    kept = []
    kept_maps = []
    dropped = []
    for factor in candidates:
        fmap = _ic_monthly_map(factor, train_end_ym=train_end_ym)
        best_corr = None
        best_with = None
        for prev, pmap in zip(kept, kept_maps):
            c = _pair_monthly_corr(fmap, pmap, min_overlap=min_overlap)
            if c is not None and math.isfinite(c):
                if best_corr is None or abs(c) > abs(best_corr):
                    best_corr = c
                    best_with = prev['id']
        if best_corr is not None and abs(best_corr) >= max_corr:
            dropped.append({'id': factor['id'], 'max_corr': round(best_corr, 4), 'corr_with': best_with})
            continue
        kept.append(factor)
        kept_maps.append(fmap)
    return kept, dropped


def _expected_max_sr(trials, sr_var, sr_mean=0.0):
    """Bailey & Lopez de Prado (2014) Eq. (1): expected max SR over N iid trials."""
    if trials <= 1 or sr_var <= 0 or not math.isfinite(sr_var):
        return float(sr_mean)
    z1 = _NORM.inv_cdf(1.0 - 1.0 / trials)
    z2 = _NORM.inv_cdf(1.0 - 1.0 / (trials * math.e))
    return float(sr_mean + math.sqrt(sr_var) * ((1.0 - _EULER_GAMMA) * z1 + _EULER_GAMMA * z2))


def _effective_trials_from_corr(corr_matrix, m_trials):
    """Bailey & Lopez de Prado (2014) Appendix A.3 Eq. (8)-(9): N_eff = rho_eff + (1 - rho_eff) * M."""
    m = max(int(m_trials), 1)
    if m <= 1 or corr_matrix.size == 0:
        return 0.0, 0.0, float(m)
    c = np.asarray(corr_matrix, dtype=float)
    k = c.shape[0]
    if k < 2:
        return 0.0, 0.0, float(m)
    off = c[~np.eye(k, dtype=bool)]
    off = off[np.isfinite(off)]
    avg_rho = float(np.mean(off)) if len(off) else 0.0
    avg_rho_clipped = min(1.0, max(-1.0 / max(k - 1, 1), avg_rho))
    rho_eff = max(0.0, avg_rho_clipped)
    n_eff = rho_eff + (1.0 - rho_eff) * m
    return avg_rho, rho_eff, float(n_eff)


def compute_ensemble_dsr(
    ensemble_metrics,
    all_factors_h,
    h,
    ppy,
    m_trials,
    *,
    min_dsr=DEFAULT_DSR_MIN_PROB,
    max_corr_sample=400,
    seed=20261118,
):
    """Compute Bailey & Lopez de Prado (2014) DSR(N_eff) for a horizon ensemble."""
    ann = float(ppy) / float(h)
    m_total = max(int(m_trials or 0), len(all_factors_h), 1)

    sr_period_va = []
    monthly_vecs = []
    months_ref = None
    for f in all_factors_h:
        va_sr = (f.get('val') or {}).get('ls_sharpe')
        if isinstance(va_sr, (int, float)) and math.isfinite(va_sr):
            sr_period_va.append(float(va_sr) / math.sqrt(ann))
        dmap = _ic_monthly_map(f)
        if dmap:
            if months_ref is None and len(dmap) >= 24:
                months_ref = sorted(dmap)
            monthly_vecs.append(dmap)

    avg_rho, rho_eff, n_eff = 0.0, 0.0, float(m_total)
    if months_ref and len(monthly_vecs) >= 2:
        pick = monthly_vecs
        if len(monthly_vecs) > max_corr_sample:
            rng = np.random.default_rng(seed)
            idx = rng.choice(len(monthly_vecs), size=max_corr_sample, replace=False)
            pick = [monthly_vecs[i] for i in idx]
        mat = pd.DataFrame([{m: d.get(m, np.nan) for m in months_ref} for d in pick]).T
        cmat = mat.corr(min_periods=12).to_numpy()
        avg_rho, rho_eff, n_eff = _effective_trials_from_corr(cmat, m_total)

    sr_var = float(np.var(sr_period_va, ddof=1)) if len(sr_period_va) > 1 else 0.0
    val_m = (ensemble_metrics or {}).get('val') or {}
    t_obs = max(3, int((val_m.get('n') or 360) / h))
    sr_ens = float(val_m.get('ls_sharpe') or 0.0) / math.sqrt(ann)
    sr0 = _expected_max_sr(max(1.0, n_eff), sr_var, sr_mean=0.0)
    denom_sq = 1.0 + 0.5 * (sr_ens ** 2)
    z = (sr_ens - sr0) * math.sqrt(t_obs - 1.0) / math.sqrt(denom_sq) if denom_sq > 1e-12 else 0.0
    dsr = float(_NORM.cdf(z)) if (math.isfinite(z) and sr_var > 0) else (1.0 if sr_ens > 0 and m_total <= 1 else 0.0)
    if sr_var <= 0 and m_total > 1:
        dsr = float(_NORM.cdf(z)) if math.isfinite(z) else 0.0
    passed = bool(dsr >= min_dsr)
    return {
        'm_trials': int(m_total),
        'avg_rho': round(avg_rho, 4),
        'rho_eff': round(rho_eff, 4),
        'n_eff': round(n_eff, 1),
        'sr_var': round(sr_var, 6),
        'sr_period': round(sr_ens, 4),
        'sr0': round(sr0, 4),
        't_obs': int(t_obs),
        'z': round(z, 4),
        'dsr': round(dsr, 4),
        'min_dsr': float(min_dsr),
        'pass': passed,
        'status': 'PASS' if passed else 'OVERFIT_WARNING',
    }


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


def build_ensemble(
    market,
    accepted,
    panels,
    targets,
    splits,
    cfg,
    *,
    values_cache=None,
    all_factors=None,
    horizons_meta=None,
):
    output = {'horizons': {}}
    ic_dedup_on = options.enabled(options.IC_DEDUP)
    dsr_gate_on = options.enabled(options.DSR_GATE)
    ppy = int(cfg.get('periods_per_year', 365 if market == 'crypto' else 252))

    for h in cfg['horizons']:
        base_candidates = select_ensemble_factors(accepted, h, None)
        dedup_dropped = []
        if ic_dedup_on and base_candidates:
            train_range = ((splits.get(h) or {}).get('ranges') or {}).get('train') or (None, None)
            train_end_ym = str(train_range[1])[:7] if train_range[1] else None
            base_candidates, dedup_dropped = deduplicate_by_ic_corr(
                base_candidates,
                max_corr=float(cfg.get('ic_dedup_max_corr', DEFAULT_IC_DEDUP_MAX_CORR)),
                train_end_ym=train_end_ym,
            )

        if options.enabled(options.GREEDY_ENSEMBLE) and targets and 'train' in splits[h]:
            factors = greedy_ensemble_factors(base_candidates, panels,
                                              targets[h][0], splits[h]['train'], h, cfg, values_cache)
        else:
            factors = base_candidates[:20]

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
        entry = dict(factor_ids=[f['id'] for f in factors],
                     train=metrics['train'], val=metrics['val'], curve={'val': curve})
        if ic_dedup_on:
            entry['ic_dedup'] = {
                'max_corr': float(cfg.get('ic_dedup_max_corr', DEFAULT_IC_DEDUP_MAX_CORR)),
                'dropped': dedup_dropped,
            }

        if dsr_gate_on:
            pool_h = [f for f in (all_factors if all_factors is not None else accepted) if f.get('horizon') == h]
            h_meta = (horizons_meta or {}).get(str(h)) or {}
            m_trials = int(h_meta.get('trials') or len(pool_h) or 1)
            dsr_info = compute_ensemble_dsr(
                metrics,
                pool_h,
                h,
                ppy,
                m_trials,
                min_dsr=float(cfg.get('dsr_min_prob', DEFAULT_DSR_MIN_PROB)),
            )
            entry['dsr'] = dsr_info
            if not dsr_info['pass']:
                output['horizons'][str(h)] = None
                output.setdefault('dsr_rejected', {})[str(h)] = entry
                continue
        output['horizons'][str(h)] = entry
    return output
