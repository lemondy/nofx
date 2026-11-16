"""S0 Hidden-Sharpe pre-screening diagnostic gate stratified by liquidity tier."""
from __future__ import annotations

import argparse
from dataclasses import asdict
import json
import math
from pathlib import Path
from typing import Sequence

import numpy as np
import pandas as pd

from scripts.backtest.hidden_sharpe_check import (
    compute_hidden_sharpe_at_z,
    estimate_hidden_sharpe_grid,
)

from . import store, tiers
from .config import MARKETS
from .data_fetch import ROOT
from .expr import ExprError, evaluate_expr
from .panel import Panels, load_panels, make_splits
from .zoo import seed_factors


def _infer_market(panels: Panels, market: str | None) -> str:
    if market is not None:
        if market not in MARKETS:
            raise ValueError(f'market must be one of {tuple(MARKETS)}, got {market!r}')
        return market
    meta_market = (panels.meta or {}).get('market')
    if meta_market in MARKETS:
        return meta_market
    return 'us' if panels.benchmark_open is not None else 'crypto'


def build_managed_factor_returns(
    factors: Sequence[dict],
    panels: Panels,
    horizon: int,
    *,
    market: str | None = None,
    tier: str = 'all',
    min_names: int = 10,
) -> pd.DataFrame:
    """Build (T x P) non-overlapping train-only factor-managed portfolio returns."""
    mkt = _infer_market(panels, market)
    tiers.validate_tier(tier)
    if horizon < 1:
        raise ValueError('horizon must be positive')

    splits = make_splits(panels.mask.index, horizon, panels.windows)
    train_pos = np.flatnonzero(splits['train'].to_numpy(dtype=bool))
    if not len(train_pos):
        return pd.DataFrame(index=pd.DatetimeIndex([], name='date'))

    train_last = int(train_pos[-1])
    max_ret_pos = train_last + 1 + horizon
    if max_ret_pos >= len(panels.mask.index):
        valid_pos = train_pos[train_pos + 1 + horizon < len(panels.mask.index)]
        if not len(valid_pos):
            return pd.DataFrame(index=pd.DatetimeIndex([], name='date'))
        train_pos = valid_pos
        train_last = int(train_pos[-1])
        max_ret_pos = train_last + 1 + horizon

    # Slice strictly to train endpoints so val/holdout bars are never touched.
    train_fields = {name: frame.iloc[:train_last + 1] for name, frame in panels.fields.items()}
    train_panels = Panels(
        train_fields,
        panels.mask.iloc[:train_last + 1],
        panels.benchmark_close.iloc[:train_last + 1] if panels.benchmark_close is not None else None,
        panels.benchmark_open.iloc[:train_last + 1] if panels.benchmark_open is not None else None,
        panels.meta,
        panels.windows,
    )
    tier_m = tiers.tier_mask(train_panels, mkt, tier)

    open_seg = panels.fields['open'].iloc[:max_ret_pos + 1]
    entry = open_seg.shift(-1).iloc[train_pos]
    exit_ = open_seg.shift(-1 - horizon).iloc[train_pos]
    raw = (exit_ / entry - 1.0).replace([np.inf, -np.inf], np.nan)
    excess = raw.sub(raw.where(tier_m).mean(axis=1), axis=0)

    sample_pos = train_pos[::horizon]
    sample_dates = panels.mask.index[sample_pos]
    r_sample = excess.loc[sample_dates].to_numpy(dtype=float)
    m_sample = tier_m.loc[sample_dates].to_numpy(dtype=bool)

    cols: dict[str, np.ndarray] = {}
    seen_ids: set[str] = set()
    for factor in factors:
        fid = str(factor.get('id') or factor.get('name') or factor['expr'])
        if fid in seen_ids:
            continue
        seen_ids.add(fid)
        expr = factor['expr']
        direction = float(factor.get('direction', 1.0))
        try:
            values = evaluate_expr(expr, train_fields) * direction
        except (ExprError, ValueError, KeyError, TypeError):
            continue
        v_sample = (
            values.reindex(index=sample_dates, columns=tier_m.columns)
            .to_numpy(dtype=float)
        )
        valid = m_sample & np.isfinite(r_sample) & np.isfinite(v_sample)
        counts = valid.sum(axis=1)
        v_clean = np.where(valid, v_sample, np.nan)
        r_clean = np.where(valid, r_sample, 0.0)

        period_rets = np.full(len(sample_dates), np.nan, dtype=float)
        ok_rows = np.flatnonzero(counts >= min_names)
        if len(ok_rows):
            v_ok = v_clean[ok_rows]
            r_ok = r_clean[ok_rows]
            means = np.nanmean(v_ok, axis=1, keepdims=True)
            stds = np.nanstd(v_ok, axis=1, ddof=1, keepdims=True)
            centered = v_ok - means
            z = np.where(stds > 1e-15, centered / np.where(stds > 1e-15, stds, 1.0), np.nan)
            z = np.where(valid[ok_rows], z, 0.0)
            l1 = np.sum(np.abs(z), axis=1, keepdims=True)
            has_w = (stds[:, 0] > 1e-15) & (l1[:, 0] > 1e-15)
            weights = np.where(l1 > 1e-15, z / np.where(l1 > 1e-15, l1, 1.0), 0.0)
            rets = np.sum(weights * r_ok, axis=1)
            period_rets[ok_rows[has_w]] = rets[has_w]

        if int(np.isfinite(period_rets).sum()) >= 20:
            cols[fid] = period_rets

    if not cols:
        return pd.DataFrame(index=sample_dates)
    df = pd.DataFrame(cols, index=sample_dates)
    return df.dropna(axis=0, how='all')


def _compute_ref_at_z(returns_matrix: np.ndarray, z_ref: float) -> dict:
    T, P = returns_matrix.shape
    R_bar_raw = np.mean(returns_matrix, axis=0)
    raw_lambda_bar = float(np.sum(returns_matrix ** 2) / (float(T) * float(P)))
    scale_factor = 1.0 / math.sqrt(raw_lambda_bar)
    R_bar = R_bar_raw * scale_factor
    R_scaled = returns_matrix * scale_factor

    if P <= T:
        Sigma_hat = (R_scaled.T @ R_scaled) / float(T)
        eigvals, eigvecs = np.linalg.eigh(Sigma_hat)
        eigvals = np.maximum(0.0, eigvals)
        proj_sq = (eigvecs.T @ R_bar) ** 2
    else:
        _, s_vals, Vt = np.linalg.svd(R_scaled / math.sqrt(float(T)), full_matrices=False)
        pos_eigs = s_vals ** 2
        proj_pos = (Vt @ R_bar) ** 2
        rem_norm_sq = max(0.0, float(np.dot(R_bar, R_bar) - np.sum(proj_pos)))
        n_zero = P - len(pos_eigs)
        eigvals = np.concatenate([np.zeros(n_zero, dtype=float), pos_eigs])
        proj_zeros = np.zeros(n_zero, dtype=float)
        if n_zero > 0:
            proj_zeros[0] = rem_norm_sq
        proj_sq = np.concatenate([proj_zeros, proj_pos])

    return asdict(compute_hidden_sharpe_at_z(R_bar, eigvals, proj_sq, P=P, T=T, z=float(z_ref)))


def evaluate_s0_gate(
    returns_df: pd.DataFrame | np.ndarray,
    *,
    periods_per_year: int | float,
    z_grid: Sequence[float] = (1e-4, 1e-3, 1e-2, 1e-1),
    z_ref: float = 1e-2,
) -> dict:
    """Run Hidden-Sharpe S0 diagnostic gate on a (T x P) managed-return matrix."""
    if isinstance(returns_df, pd.DataFrame):
        df = returns_df.dropna(axis=1, how='all')
        clean = df.dropna(axis=0, how='any')
        if len(clean) < 10 and df.shape[1] >= 2:
            non_empty_rows = max(1, df.dropna(axis=0, how='all').shape[0])
            keep = df.notna().sum(axis=0) >= int(0.8 * non_empty_rows)
            if int(keep.sum()) >= 2:
                clean = df.loc[:, keep].dropna(axis=0, how='any')
        mat = clean.to_numpy(dtype=float) if not clean.empty else np.empty((0, df.shape[1]), dtype=float)
    else:
        arr = np.asarray(returns_df, dtype=float)
        if arr.ndim != 2:
            mat = np.empty((0, 0), dtype=float)
        else:
            mat = arr[np.isfinite(arr).all(axis=1)]

    T, P = mat.shape if mat.ndim == 2 else (0, 0)
    if T < 10 or P < 2:
        return {
            'status': 'insufficient_data',
            'verdict': 'insufficient_data',
            'T': int(T),
            'P': int(P),
            'c': float(P) / float(T) if T > 0 else 0.0,
            'periods_per_year': float(periods_per_year),
            'z_ref': float(z_ref),
        }

    raw_lambda_bar = float(np.sum(mat ** 2) / (float(T) * float(P)))
    if not np.isfinite(raw_lambda_bar) or raw_lambda_bar <= 1e-20:
        return {
            'status': 'insufficient_data',
            'verdict': 'insufficient_data',
            'T': int(T),
            'P': int(P),
            'c': float(P) / float(T),
            'periods_per_year': float(periods_per_year),
            'z_ref': float(z_ref),
        }

    grid_res = estimate_hidden_sharpe_grid(
        mat,
        z_grid=tuple(z_grid),
        annualization_factor=float(periods_per_year),
        normalize_trace=True,
    )
    ref = next((g for g in grid_res['grid'] if abs(g['z'] - float(z_ref)) < 1e-12), None)
    if ref is None:
        ref = _compute_ref_at_z(mat, float(z_ref))

    theta0 = float(ref['theta_0'])
    theta1 = float(ref['theta_1'])
    se_theta0 = float(ref['se_theta0'])
    se_theta1 = float(ref['se_theta1'])
    if se_theta0 > 1e-12:
        theta0_z = theta0 / se_theta0
    else:
        theta0_z = math.copysign(1e12, theta0) if theta0 != 0.0 else 0.0
    theta1_over_theta0 = (theta1 / theta0) if theta0 > 1e-12 else None

    if theta0 <= 0.0 or theta0_z < 1.65:
        verdict = 'fail_noise_overfit'
    elif theta1 <= 1e-8 or (theta0 > 0.0 and theta1 / theta0 < 0.05):
        verdict = 'linear_dominant_no_hidden_alpha'
    else:
        verdict = 'pass_hidden_alpha_detected'

    return {
        'status': 'ok',
        'verdict': verdict,
        'T': int(grid_res['T']),
        'P': int(grid_res['P']),
        'c': float(grid_res['c']),
        'periods_per_year': float(periods_per_year),
        'z_ref': float(z_ref),
        'theta0': theta0,
        'theta1': theta1,
        'se_theta0': se_theta0,
        'se_theta1': se_theta1,
        'theta0_z': float(theta0_z),
        'theta1_over_theta0': float(theta1_over_theta0) if theta1_over_theta0 is not None else None,
        'theta_hat': float(ref['theta_hat']),
        'Z_star': float(ref['Z_star']),
        'Z_star_prime': float(ref['Z_star_prime']),
        'H1_L_sr_ann': float(grid_res['combined_H1_L_sr_ann']),
        'H1_U_sr_ann': (
            float(grid_res['combined_H1_U_sr_ann'])
            if math.isfinite(grid_res['combined_H1_U_sr_ann'])
            else None
        ),
        'ridge_oos_sr_ceiling_ann': float(grid_res['ridge_oos_sr_ceiling_ann']),
        'best_lb_z': float(grid_res['best_lb_z']),
        'best_ub_z': float(grid_res['best_ub_z']),
        'grid': grid_res['grid'],
    }


def _select_diagnostic_factors(market: str, root: Path, horizon: int, max_factors: int) -> tuple[list[dict], str]:
    zoo_doc = store.read_json(Path(root) / market / 'zoo.json', {})
    raw_factors = (zoo_doc or {}).get('factors') or []
    qualified = [
        f for f in raw_factors
        if f.get('horizon') == horizon and f.get('status') in ('accepted', 'candidate')
    ]
    qualified.sort(key=lambda f: (-(f.get('train') or {}).get('t', 0.0), str(f.get('id', ''))))
    if len(qualified) >= 5:
        return qualified[:max_factors], 'zoo'
    seeds = seed_factors(market)
    seen = {f.get('id') for f in qualified if f.get('id')}
    combined = list(qualified)
    for s in seeds:
        if s['id'] not in seen:
            seen.add(s['id'])
            combined.append(s)
        if len(combined) >= max_factors:
            break
    return combined[:max_factors], 'seed_fallback'


def diagnose_market_tiers(
    market: str,
    root: str | Path = ROOT,
    *,
    horizon: int | None = None,
    max_factors: int = 45,
) -> dict:
    """Run S0 Hidden-Sharpe diagnostics across all, mega_large, and mid_small tiers."""
    if market not in MARKETS:
        raise ValueError(f'market must be one of {tuple(MARKETS)}, got {market!r}')
    cfg = MARKETS[market]
    h = (3 if 3 in cfg['horizons'] else cfg['horizons'][0]) if horizon is None else int(horizon)
    if h < 1:
        raise ValueError('horizon must be positive')

    panels = load_panels(market, root, tier='all')
    factors, source_label = _select_diagnostic_factors(market, Path(root), h, max_factors)
    periods_per_year = float(cfg['periods_per_year']) / float(h)
    splits = make_splits(panels.mask.index, h, panels.windows)
    train_mask = splits['train']

    tier_results: dict[str, dict] = {}
    for tier in tiers.VALID_TIERS:
        t_mask = tiers.tier_mask(panels, market, tier)
        rets = build_managed_factor_returns(factors, panels, h, market=market, tier=tier)
        diag = evaluate_s0_gate(rets, periods_per_year=periods_per_year)
        train_counts = t_mask.loc[train_mask].sum(axis=1)
        diag['universe_as_of'] = int(t_mask.iloc[-1].sum())
        diag['mean_universe_train'] = round(float(train_counts.mean()), 2) if len(train_counts) else 0.0
        tier_results[tier] = diag

    warnings: list[str] = []
    mega_v = tier_results['mega_large'].get('verdict')
    mid_v = tier_results['mid_small'].get('verdict')
    all_v = tier_results['all'].get('verdict')
    if mega_v in ('linear_dominant_no_hidden_alpha', 'fail_noise_overfit'):
        warnings.append(
            f'mega_large tier verdict={mega_v}: avoid complex nonlinear nesting; '
            'restrict to low-turnover linear/trend features or focus nonlinear search on mid_small.'
        )
    if all_v == 'fail_noise_overfit':
        warnings.append('all-universe S0 verdict=fail_noise_overfit: current factor set fails debiased GRS gate.')

    if mid_v == 'pass_hidden_alpha_detected' and mega_v != 'pass_hidden_alpha_detected':
        recommendation = 'stratify: focus nonlinear mining on mid_small; constrain mega_large to linear/low-turnover'
    elif all_v == 'pass_hidden_alpha_detected':
        recommendation = 'proceed: hidden alpha detected across universe; monitor tier differences'
    elif mega_v == 'pass_hidden_alpha_detected':
        recommendation = 'focus_mega_large: hidden alpha concentrated in mega_large'
    else:
        recommendation = 'caution: low debiased hidden alpha; enforce linear discipline and strict K control'

    return {
        'market': market,
        'horizon': h,
        'factor_source': source_label,
        'factor_count': len(factors),
        'periods_per_year': periods_per_year,
        'train_range': list(splits['ranges']['train']),
        'tiers': tier_results,
        'warnings': warnings,
        'recommendation': recommendation,
    }


def _format_report(report: dict) -> str:
    lines = [
        f"S0 Hidden-Sharpe Diagnostics | market={report['market']} h={report['horizon']} "
        f"source={report['factor_source']} factors={report['factor_count']} "
        f"train={report['train_range'][0]}..{report['train_range'][1]}",
        '-' * 96,
    ]
    for tier_name in tiers.VALID_TIERS:
        d = report['tiers'][tier_name]
        if d['status'] != 'ok':
            lines.append(
                f"  {tier_name:<10} | status={d['status']} T={d['T']} P={d['P']} "
                f"univ(last/train)={d['universe_as_of']}/{d['mean_universe_train']:.1f}"
            )
            continue
        ratio = f"{d['theta1_over_theta0']:.3f}" if d['theta1_over_theta0'] is not None else 'NA'
        lines.append(
            f"  {tier_name:<10} | {d['verdict']:<31} | T={d['T']:<3} P={d['P']:<2} c={d['c']:.2f} "
            f"| theta0={d['theta0']:.4f}(z={d['theta0_z']:.2f}) theta1={d['theta1']:.4f} "
            f"t1/t0={ratio} | H1_L_SR={d['H1_L_sr_ann']:.2f} ceil={d['ridge_oos_sr_ceiling_ann']:.2f} "
            f"Z*'={d['Z_star_prime']:.2f} | univ={d['universe_as_of']}/{d['mean_universe_train']:.1f}"
        )
    for w in report['warnings']:
        lines.append(f'  WARNING: {w}')
    lines.append(f"  Recommendation: {report['recommendation']}")
    return '\n'.join(lines)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--market', choices=('us', 'crypto'), required=True)
    parser.add_argument('--horizon', type=int, default=None)
    parser.add_argument('--max-factors', type=int, default=45)
    parser.add_argument('--root', type=Path, default=ROOT)
    parser.add_argument('--json', action='store_true', dest='as_json')
    args = parser.parse_args(argv)
    report = diagnose_market_tiers(
        args.market,
        args.root,
        horizon=args.horizon,
        max_factors=args.max_factors,
    )
    if args.as_json:
        print(json.dumps(store.finite(report), ensure_ascii=False, indent=2))
    else:
        print(_format_report(report))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
