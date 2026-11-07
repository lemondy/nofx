"""Frozen fq_crypto multi-feature ridge/RFF S1; offline, numpy/pandas only."""
from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
import math
from pathlib import Path
import sys
import time

import numpy as np
import pandas as pd

from . import ensemble, ensemble_forward_study as study, panel
from .expr import evaluate_expr
from .msrr_study import VAL_RANGE, dated, factor_values, paired_statistics, target_backtest
from .style_neutral_study import EXPECTED_HASHES, guard_hashes, input_hashes, weekly_statistics
from scripts.backtest.evolution_ledger import append_event
from scripts.backtest.incremental_filter import z_threshold


H = 3
Q = 2048
CHUNK = 2048
FOLDS = 5
EMBARGO = 5
MIN_NAMES = 10
MULTIPLIERS = (1e-4, 1e-3, 1e-2, 1e-1, 1., 10.)
FEATURE_HASH = '5511b1596ff4fb4679f01bfcf89c430a379bfca316f5f79d1eeae16a1ccc4e42'
PREREG = 'docs/research/2026-11-07-fq-crypto-ml-multifeature-prereg.md'
FEATURES = (
    ('mom_1', '$ret'),
    ('mom_3', '$close / Ref($close, 3) - 1'),
    ('mom_7', '$close / Ref($close, 7) - 1'),
    ('mom_14', '$close / Ref($close, 14) - 1'),
    ('mom_30', '$close / Ref($close, 30) - 1'),
    ('mom_60', '$close / Ref($close, 60) - 1'),
    ('mom_120', '$close / Ref($close, 120) - 1'),
    ('mom_7_30', 'Ref($close, 7) / Ref($close, 30) - 1'),
    ('vol_7', 'Std($ret, 7)'),
    ('vol_30', 'Std($ret, 30)'),
    ('vol_90', 'Std($ret, 90)'),
    ('skew_30', 'Skew($ret, 30)'),
    ('kurt_30', 'Kurt($ret, 30)'),
    ('max_30', 'Max($ret, 30)'),
    ('min_30', 'Min($ret, 30)'),
    ('hl_14', 'Mean(($high - $low) / $close, 14)'),
    ('beta_60', 'Cov($ret, ($ret - CSDemean($ret)), 60) / Var(($ret - CSDemean($ret)), 60)'),
    ('ivol_30', 'Std(CSDemean($ret), 30)'),
    ('dvol_30', 'Log(Mean($close * $volume, 30))'),
    ('vtrend', 'Log(Mean($volume, 7) / Mean($volume, 60))'),
    ('amihud_30', 'Mean(Abs($ret) / ($close * $volume), 30)'),
    ('vcv_30', 'Std($volume, 30) / Mean($volume, 30)'),
    ('pvcorr_30', 'Corr($close, $volume, 30)'),
    ('avcorr_30', 'Corr(Abs($ret), $volume, 30)'),
    ('rpos_30', '($close - Min($low, 30)) / (Max($high, 30) - Min($low, 30))'),
    ('rpos_90', '($close - Min($low, 90)) / (Max($high, 90) - Min($low, 90))'),
    ('dhigh_60', '$close / Max($high, 60) - 1'),
    ('vwapdev', '$close / $vwap - 1'),
    ('madev_20', '$close / Mean($close, 20) - 1'),
    ('idxmax_30', 'IdxMax($high, 30)'),
    ('body', '($close - $open) / ($high - $low)'),
    ('body_7', 'Mean(($close - $open) / $open, 7)'),
    ('fund_1', '$funding'),
    ('fund_7', 'Mean($funding, 7)'),
    ('fund_30', 'Mean($funding, 30)'),
    ('fund_chg', 'Mean($funding, 7) - Mean($funding, 30)'),
    ('prem_1', '$premium'),
    ('prem_7', 'Mean($premium, 7)'),
)


def guard_features(features=None):
    features = FEATURES if features is None else features
    digest = hashlib.sha256(''.join(f'{n}\t{e}\n' for n, e in features).encode()).hexdigest()
    if digest != FEATURE_HASH:
        raise ValueError('feature hash mismatch; refusing to run')
    return digest


def rank_map(values, mask, *, label=False):
    """Half-rank mapping; missing features are neutral, labels stay missing."""
    visible = values.where(mask & np.isfinite(values))
    n = visible.count(axis=1).replace(0, np.nan)
    ranked = (visible.rank(axis=1, method='average').sub(.5).div(n, axis=0) - .5)
    good = mask.sum(axis=1).ge(MIN_NAMES)
    if label:
        return ranked.where(good, np.nan, axis=0)
    return (ranked * math.sqrt(12)).fillna(0.).where(good, 0., axis=0)


def feature_panels(panels, factors, progress=lambda _: None):
    guard_features()
    names, transformed, raw_styles = [], [], {}
    train = panel.make_splits(panels.mask.index, H, panels.windows)['train']
    eligible = panels.mask.to_numpy() & train.to_numpy()[:, None]
    denominator = int(eligible.sum())
    if denominator == 0:
        raise ValueError('empty train-eligible sample')
    shares = {}
    directed = factor_values(panels, factors, panels.mask.index[-1])
    specifications = [(n, e, None) for n, e in FEATURES] + [
        (f['id'], f['expr'], directed[f['id']]) for f in factors]
    for name, expression, value in specifications:
        progress(f'特征 {len(names) + 1}/45：{name}')
        if value is None:
            value = evaluate_expr(expression, panels.fields)
        shares[name] = float((eligible & ~np.isfinite(value.to_numpy())).sum() / denominator)
        names.append(name)
        transformed.append(rank_map(value, panels.mask).to_numpy())
        if name in ('vol_30', 'beta_60', 'dvol_30'):
            raw_styles[name] = value
    if len(names) != 45:
        raise ValueError('the preregistered feature count must be 45')
    return np.stack(transformed, axis=2), names, shares, raw_styles


def training_sample(cube, labels, mask, dates):
    valid = (mask.to_numpy() & np.isfinite(labels.to_numpy()) &
             mask.sum(axis=1).ge(MIN_NAMES).to_numpy()[:, None] &
             np.asarray(dates, bool)[:, None])
    rows, coins = np.nonzero(valid)
    return cube[rows, coins], labels.to_numpy()[rows, coins], rows


def permute_labels(y, dates):
    """One RNG, sorted dates, and panel column order within every date."""
    result = np.array(y, copy=True)
    rng = np.random.default_rng(20261108)
    for date in np.unique(dates):
        use = np.flatnonzero(dates == date)
        result[use] = rng.permutation(y[use])
    return result


def random_weights(p=45, q=Q):
    return np.random.default_rng(20261107).standard_normal((p, q))


def random_features(x, omega):
    phase = (x @ omega) / math.sqrt(omega.shape[0])
    result = np.empty((len(x), 2 * omega.shape[1]))
    np.sin(phase, out=result[:, :omega.shape[1]])
    np.cos(phase, out=result[:, omega.shape[1]:])
    result *= math.sqrt(1 / omega.shape[1])
    return result


def design(x, omega):
    return x if omega is None else random_features(x, omega)


def gram(x, y, omega=None, chunk=CHUNK):
    """Accumulate unnormalised sufficient statistics, one feature chunk at a time."""
    p = x.shape[1] if omega is None else 2 * omega.shape[1]
    g = np.zeros((p, p))
    rhs = np.zeros((p,) + y.shape[1:])
    for first in range(0, len(x), chunk):
        last = first + chunk
        s = design(x[first:last], omega)
        g += s.T @ s
        rhs += s.T @ y[first:last]
        del s
    return g, rhs


def ridge(x, y, lam):
    g, rhs = gram(x, y)
    return np.linalg.solve(g / len(x) + lam * np.eye(x.shape[1]), rhs / len(x))


def spectral_ridge(g, rhs, n, lambdas):
    """Reuse one symmetric eigensystem across lambdas and both label arms."""
    eigenvalues, vectors = np.linalg.eigh(g / n)
    projected = vectors.T @ (rhs / n)
    return np.stack([vectors @ (projected / (eigenvalues[:, None] + lam))
                     for lam in lambdas], axis=1)


def predict(x, beta, omega=None, chunk=CHUNK):
    shape = beta.shape[1:]
    flat = beta.reshape((beta.shape[0], -1))
    out = np.empty((len(x), flat.shape[1]))
    for first in range(0, len(x), chunk):
        s = design(x[first:first + chunk], omega)
        out[first:first + chunk] = s @ flat
        del s
    return out.reshape((len(x),) + shape)


def cv_indices(n):
    if n < FOLDS:
        raise ValueError('at least five training dates required')
    folds = []
    for evaluation in np.array_split(np.arange(n), FOLDS):
        keep = np.ones(n, bool)
        keep[max(0, evaluation[0] - EMBARGO):min(n, evaluation[-1] + EMBARGO + 1)] = False
        training = np.flatnonzero(keep)
        if not len(training):
            raise ValueError('empty CV training fold')
        folds.append((training, evaluation))
    return folds


def daily_ic_rows(prediction, label, dates):
    """Pairwise-finite daily Spearman, average ties, at least two observations."""
    frame = pd.DataFrame(dict(p=prediction, y=label, date=dates))
    frame = frame.loc[np.isfinite(frame.p) & np.isfinite(frame.y)]
    if frame.empty:
        return pd.Series(dtype=float)
    ranks = frame.groupby('date')[['p', 'y']].rank(method='average')
    centered = ranks - ranks.groupby(frame.date).transform('mean')
    cross = (centered.p * centered.y).groupby(frame.date).sum()
    squares = centered.pow(2).groupby(frame.date).sum()
    return (cross / np.sqrt(squares.p * squares.y)).dropna()


def choose_lambda(cv):
    usable = [r for r in cv if r['ic'] is not None and np.isfinite(r['ic'])]
    if not usable:
        raise ValueError('no finite CV daily IC')
    return max(usable, key=lambda r: (r['ic'], r['lambda']))


def fit_pair(x, y, dates, omega=None, progress=print, *, noise_labels=None):
    """True/noise arms share features, folds and fixed full-training scales."""
    noise_labels = permute_labels(y, dates) if noise_labels is None else noise_labels
    targets = np.column_stack((y, noise_labels))
    unique, codes = np.unique(dates, return_inverse=True)
    progress(f'累计全训练 Gram：N={len(x)}，P={x.shape[1] if omega is None else 2 * omega.shape[1]}')
    full_g, full_rhs = gram(x, targets, omega)
    scale = float(np.trace(full_g) / len(x) / full_g.shape[0])
    lambdas = np.asarray(MULTIPLIERS) * scale
    daily = [[[] for _ in lambdas] for _ in range(2)]
    fold_records = []
    for k, (train, evaluation) in enumerate(cv_indices(len(unique))):
        train_rows = np.isin(codes, train)
        eval_rows = np.isin(codes, evaluation)
        embargo_rows = ~train_rows & ~eval_rows
        progress(f'CV {k + 1}/5：减去评估块 {eval_rows.sum()} 行及隔离 {embargo_rows.sum()} 行；分解 Gram。')
        held_g, held_rhs = gram(x[eval_rows], targets[eval_rows], omega)
        embargo_g, embargo_rhs = gram(x[embargo_rows], targets[embargo_rows], omega)
        fold_g = full_g - held_g - embargo_g
        fold_rhs = full_rhs - held_rhs - embargo_rhs
        del held_g, embargo_g
        betas = spectral_ridge(fold_g, fold_rhs, int(train_rows.sum()), lambdas)
        del fold_g
        predictions = predict(x[eval_rows], betas, omega)
        for arm in range(2):
            for j in range(len(lambdas)):
                daily[arm][j].append(daily_ic_rows(predictions[:, j, arm], targets[eval_rows, arm], dates[eval_rows]))
        fold_records.append(dict(train_dates=unique[train].tolist(), eval_dates=unique[evaluation].tolist(),
                                 embargo_dates=unique[np.unique(codes[embargo_rows])].tolist(),
                                 train_rows=int(train_rows.sum()), eval_rows=int(eval_rows.sum())))
        progress(f'CV {k + 1}/5 完成。')
    cvs = [[dict(multiplier=mult, **{'lambda': float(lam)},
                 ic=float(pd.concat(daily[a][j]).mean()), n_days=len(pd.concat(daily[a][j])))
            for j, (mult, lam) in enumerate(zip(MULTIPLIERS, lambdas))] for a in range(2)]
    chosen = [choose_lambda(cv) for cv in cvs]
    progress('CV 完成；按选定 λ 全训练重估并冻结。')
    # Two different chosen lambdas use the same full eigensystem.
    fits = spectral_ridge(full_g, full_rhs, len(x), [r['lambda'] for r in chosen])
    betas = np.column_stack((fits[:, 0, 0], fits[:, 1, 1]))
    del fits
    models = [dict(chosen_lambda=chosen[a]['lambda'], chosen_multiplier=chosen[a]['multiplier'],
                   scale=scale, cv=cvs[a], beta=betas[:, a].tolist(), cv_folds=fold_records,
                   n_train=len(x), n_train_dates=len(unique)) for a in range(2)]
    return models, (full_g, full_rhs[:, 0])


def score_panel(cube, mask, beta, omega=None):
    good = mask.to_numpy() & mask.sum(axis=1).ge(MIN_NAMES).to_numpy()[:, None]
    rows, coins = np.nonzero(good)
    values = np.full(mask.shape, np.nan)
    values[rows, coins] = predict(cube[rows, coins], np.asarray(beta), omega)
    return pd.DataFrame(values, index=mask.index, columns=mask.columns)


def ic_summary(values):
    values = values.dropna()
    n = len(values)
    mean = float(values.mean()) if n else None
    sd = float(values.std(ddof=1)) if n > 1 else 0.
    t = mean / (sd / math.sqrt(n)) if sd else None
    return dict(mean=mean, t=t, n=n)


def panel_ic(score, labels, mask, dates):
    visible = mask.to_numpy() & np.asarray(dates)[:, None]
    rows, coins = np.nonzero(visible)
    return ic_summary(daily_ic_rows(score.to_numpy()[rows, coins], labels.to_numpy()[rows, coins], rows))


def verdict(n, shares, bad_prediction, z, z_noise, val_delta, forward_delta, z_k):
    if n < 100 or any(v > .5 for v in shares.values()) or bad_prediction:
        return 'invalid'
    if (z is not None and z_noise is not None and val_delta is not None and forward_delta is not None
            and val_delta > 0 and forward_delta > 0 and z_noise >= 1.):
        if z >= z_k:
            return 'strong'
        if z >= 1.:
            return 'screen_pass'
    return 'fail'


def leg_backtests(panels, targets, start, end):
    """Preserve BTC in its signed leg; there is no separate BTC managed column."""
    result = {}
    for leg, weights in dict(long=targets.clip(lower=0), short=targets.clip(upper=0)).items():
        run, returns = target_backtest(panels, weights, start, end)
        result[leg] = dict(backtest=run, daily_returns=dated(returns))
    return result


def crypto_study(panels, factors, progress=print):
    splits = panel.make_splits(panels.mask.index, H, panels.windows)
    ranges = dict(val=VAL_RANGE, forward=(study.FORWARD_START, study.day(panels.mask.index[-1])))
    cube, names, shares, styles = feature_panels(panels, factors, progress)
    labels_raw = panel.forward_target(panels, H)[0]
    labels = rank_map(labels_raw, panels.mask, label=True)
    x, y, dates = training_sample(cube, labels, panels.mask, splits['train'])
    omega = random_weights()
    noise_labels = permute_labels(y, dates)
    models, sufficient = {}, {}
    for arm, noise, weights in [('R', 'N_R', None), ('RF', 'N_RF', omega)]:
        progress(f'{arm}/{noise}：训练和交叉验证。')
        fitted, sufficient[arm] = fit_pair(x, y, dates, weights, progress, noise_labels=noise_labels)
        models.update({arm: fitted[0], noise: fitted[1]})
    scores = {'B': ensemble.ensemble_scores(factors, panels)}
    for arm in models:
        progress(f'{arm}：分块预测全部可用日期。')
        scores[arm] = score_panel(cube, panels.mask, models[arm]['beta'], omega if 'RF' in arm else None)
        expected = panels.mask.to_numpy() & panels.mask.sum(axis=1).ge(MIN_NAMES).to_numpy()[:, None]
        models[arm]['nonfinite_predictions'] = int((expected & ~np.isfinite(scores[arm].to_numpy())).sum())
    # Forward labels are touched only after every selection arm is frozen.
    forward_labels = panel.forward_labels(panels, H, ranges['forward'][0])[0]
    for arm, model in models.items():
        model['ic'] = dict(train=panel_ic(scores[arm], labels, panels.mask, splits['train']))
        if arm.startswith('N_'):
            permuted = labels.copy()
            # Use exactly the same training sample, including the minimum-name filter.
            valid = panels.mask.to_numpy() & np.isfinite(labels.to_numpy()) & splits['train'].to_numpy()[:, None]
            valid &= panels.mask.sum(axis=1).ge(MIN_NAMES).to_numpy()[:, None]
            train_rows, train_coins = np.nonzero(valid)
            permuted.values[train_rows, train_coins] = noise_labels
            model['ic']['train_permuted_fit_target'] = panel_ic(scores[arm], permuted, panels.mask, splits['train'])
        for segment, (start, end) in ranges.items():
            visible = (panels.mask.index >= start) & (panels.mask.index <= end)
            model['ic'][segment] = panel_ic(scores[arm], forward_labels if segment == 'forward' else labels,
                                             panels.mask, visible)
    segments, daily_returns = {}, {}
    for segment, (start, end) in ranges.items():
        result = dict(range=[start, end], arms={}, paired={}, score_correlations={},
                      skipped_dates=int(panels.mask.loc[start:end].sum(axis=1).lt(MIN_NAMES).sum()))
        daily_returns[segment] = {}
        visible = (panels.mask.index >= start) & (panels.mask.index <= end)
        for arm, score in scores.items():
            progress(f'{segment}/{arm}：原口径 Backtrader 回测。')
            run, returns, targets = study.backtest(panels, score, start, end, H)
            result['arms'][arm] = dict(backtest=run, daily_returns=dated(returns))
            daily_returns[segment][arm] = returns
            if arm != 'B':
                result['paired'][arm] = paired_statistics(returns, daily_returns[segment]['B'])[0]
                result['score_correlations'][arm] = {name: panel_ic(score, other, panels.mask, visible)
                                                      for name, other in dict(B=scores['B'], **styles).items()}
            if arm in ('R', 'RF'):
                # backtest already used build_targets/rank_weights; verify identical decomposition input.
                reference_targets = study.build_targets(score.loc[:end], panels.mask.loc[:end], start, H).loc[start:end]
                pd.testing.assert_frame_equal(targets, reference_targets, check_exact=True)
                progress(f'{segment}/{arm}：多头/空头腿分别回测。')
                result['arms'][arm]['legs'] = leg_backtests(panels, targets, start, end)
        segments[segment] = result
    paired = {}
    for arm, noise, k in [('R', 'N_R', 5), ('RF', 'N_RF', 6)]:
        delta = pd.concat([daily_returns[s][arm] - daily_returns[s]['B'] for s in ranges]).sort_index()
        noise_delta = pd.concat([daily_returns[s][arm] - daily_returns[s][noise] for s in ranges]).sort_index()
        stats = weekly_statistics(delta)
        stats.update(z_noise=weekly_statistics(noise_delta)['z'], noise=weekly_statistics(noise_delta), z_K=z_threshold(k),
                     preregistered_K=k, segment_delta={s: segments[s]['paired'][arm]['mean_delta'] for s in ranges})
        stats['verdict'] = verdict(stats['n'], shares, models[arm]['nonfinite_predictions'] > 0,
                                   stats['z'], stats['z_noise'], stats['segment_delta']['val'],
                                   stats['segment_delta']['forward'], stats['z_K'])
        paired[arm] = stats
    progress('复现核对：B 前向每日收益与参考 ensemble 路径严格相等。')
    reference = ensemble.ensemble_scores(factors, panels)
    pd.testing.assert_frame_equal(scores['B'], reference, check_exact=True)
    ref_run, ref_returns, _ = study.backtest(panels, reference, *ranges['forward'], H)
    pd.testing.assert_series_equal(ref_returns, daily_returns['forward']['B'], check_exact=True)
    # Descriptive train+val refits preserve the already chosen absolute lambda.
    val_x, val_y, _ = training_sample(cube, labels, panels.mask, splits['val'])
    descriptive = {}
    for arm, weights in [('R', None), ('RF', omega)]:
        progress(f'{arm}′：训练加验证重估（仅描述，不重新选择 λ）。')
        vg, vrhs = gram(val_x, val_y, weights)
        tg, trhs = sufficient.pop(arm)
        total_n = len(x) + len(val_x)
        coefficient = spectral_ridge(tg + vg, (trhs + vrhs)[:, None], total_n,
                                     [models[arm]['chosen_lambda']])[:, 0, 0]
        del vg, tg
        score = score_panel(cube, panels.mask, coefficient, weights)
        run, returns, _ = study.backtest(panels, score, *ranges['forward'], H)
        descriptive[arm + '_prime'] = dict(backtest=run, daily_returns=dated(returns),
                                          paired=paired_statistics(returns, daily_returns['forward']['B'])[0],
                                          chosen_lambda=models[arm]['chosen_lambda'], beta=coefficient.tolist(),
                                          n_train=total_n)
    return dict(panel_meta=panels.meta, splits=splits['ranges'], factors=factors, feature_names=names,
                nonfinite_shares=shares, models=models, segments=segments, paired=paired,
                descriptive_refits=descriptive, reproduction=dict(score_exact_match=True,
                forward_daily_returns_exact_match=True, reference_forward_metrics=ref_run['metrics']))


def append_ledger(path, result):
    events = []
    for arm, idea, k in [('R', 'fq-crypto-ml-ridge-v1', 5), ('RF', 'fq-crypto-ml-rff-v1', 6)]:
        c = result['crypto']
        stats, model = c['paired'][arm], c['models'][arm]
        events.append(append_event(path, 'fq_crypto', lambda actual_k, previous: dict(
            stage='S1', change_type='E', idea_id=idea, baseline_version='ensemble@5e3599f8',
            preregistered_K=k, verdict=stats['verdict'], z=stats['z'], z_noise=stats['z_noise'], z_K=stats['z_K'],
            paired=stats, segment_delta=stats['segment_delta'], chosen_lambda=model['chosen_lambda'],
            chosen_multiplier=model['chosen_multiplier'], input_hashes=result['hashes'], feature_hash=FEATURE_HASH,
            prereg_path=PREREG, deviations=result['deviations'])))
    return events


def render_report(result):
    c, fmt, pct, table = result['crypto'], study.fmt, study.pct, study.table
    lines = ['# fq_crypto 多特征机器学习：S1 离线筛选', '',
             f'预注册：`{PREREG}`；38 通用特征加 7 个定向 ensemble 因子，共 45 维。', '',
             '本研究共跑了 R、RF、N_R、N_RF、R′、RF′ 六组；只有 R 和 RF 计入 K。'
             '验证和前向已被至少五项研究查看，只作离线筛选，不构成样本外证据。', '']
    for arm, p in c['paired'].items():
        model = c['models'][arm]
        lines += [f"{arm} 判定：**{p['verdict']}**；z={fmt(p['z'], 8)}；z_noise={fmt(p['z_noise'], 8)}；"
                  f"z_K={fmt(p['z_K'], 8)}（K={p['preregistered_K']}）；验证日均 Δ={pct(p['segment_delta']['val'])}；"
                  f"前向日均 Δ={pct(p['segment_delta']['forward'])}；λ={fmt(model['chosen_lambda'], 12)}"
                  f"（倍数 {model['chosen_multiplier']}）。", '']
    lines += ['## IC 表（全部只描述）', '',
              table(['组', '训练 IC（真实标签）', '训练 t', '训练天数', '验证 IC', '验证 t', '验证天数', '前向 IC', '前向 t', '前向天数'],
                    [[arm] + [v for seg in ('train', 'val', 'forward')
                              for v in (fmt(m['ic'][seg]['mean'], 8), fmt(m['ic'][seg]['t'], 6), m['ic'][seg]['n'])]
                     for arm, m in c['models'].items()]), '',
              't=日均 IC/(日 IC 样本标准差/√天数)，非 HAC。噪声臂训练 IC 同时对真实标签展示；'
              '其拟合用置换标签的训练 IC：' + '；'.join(
                  f"{a}={fmt(c['models'][a]['ic']['train_permuted_fit_target']['mean'], 8)}"
                  for a in ('N_R', 'N_RF')) + '。', '',
              '## 分段配对与执行', '',
              table(['分段', '组', '净收益', '年化收益', 'Sharpe', '最大回撤', '换手', '手续费', '资金费', '日均 Δ 对 B'],
                    [[seg, arm, pct(m['total_return']), pct(m['annual_return']), fmt(m['sharpe']),
                      pct(m['max_drawdown']), fmt(m['turnover']), fmt(m['commission_paid']), fmt(m['funding_paid']),
                      pct(s['paired'][arm]['mean_delta']) if arm != 'B' else '—']
                     for seg, s in c['segments'].items() for arm, a in s['arms'].items()
                     for m in [a['backtest']['metrics']]]), '',
              'B、四个冻结模型和两个描述性重训模型均调用 ensemble_forward_study.backtest(panels, score, start, end, 3)。'
              '每 3 日、前后各 20%、多空各 0.5；手续费 5bp、滑点 3bp、实际资金费。两段独立起始。'
              '配对统计复用 weekly_statistics（ISO 周，CR0）；两段合并共 '
              f"{c['paired']['R']['n']} 日。", '',
              '可用币少于 10 的跳过日期：' + '；'.join(
                  f"{seg} {s['skipped_dates']} 日" for seg, s in c['segments'].items()) + '。', '',
              '## λ 选择（训练段五折，固定全训练尺度）', '',
              table(['组', '尺度', '倍数', '绝对 λ', 'CV 日均 IC', '选中'],
                    [[arm, fmt(m['scale'], 12), r['multiplier'], fmt(r['lambda'], 12), fmt(r['ic'], 8),
                      '是' if r['lambda'] == m['chosen_lambda'] else '']
                     for arm, m in c['models'].items() for r in m['cv']]), '',
              '## 多空腿（只描述）', '',
              table(['分段', '组', '腿', '净收益', 'Sharpe', '手续费', '资金费'],
                    [[seg, arm, leg, pct(m['total_return']), fmt(m['sharpe']), fmt(m['commission_paid']), fmt(m['funding_paid'])]
                     for seg, s in c['segments'].items() for arm in ('R', 'RF')
                     for leg, a in s['arms'][arm]['legs'].items() for m in [a['backtest']['metrics']]]), '',
              '## 分数相关与风格暴露（只描述）', '',
              table(['分段', '组', '对 B', '对 vol_30', '对 beta_60', '对 dvol_30'],
                    [[seg, arm] + [fmt(a[key]['mean'], 8) for key in ('B', 'vol_30', 'beta_60', 'dvol_30')]
                     for seg, s in c['segments'].items() for arm, a in s['score_correlations'].items()]), '',
              '## 训练加验证重训（只描述，不参与判定）', '',
              table(['组', '绝对 λ（不变）', '前向净收益', '前向日均 Δ 对 B', '前向 z'],
                    [[arm.replace('_prime', '′'), fmt(a['chosen_lambda'], 12), pct(a['backtest']['metrics']['total_return']),
                      pct(a['paired']['mean_delta']), fmt(a['paired']['z'], 8)] for arm, a in c['descriptive_refits'].items()]), '',
              '## R 的 45 个系数（按绝对值降序）', '',
              table(['特征', 'β'], [[name, fmt(beta, 12)] for name, beta in sorted(
                  zip(c['feature_names'], c['models']['R']['beta']), key=lambda pair: -abs(pair[1]))]), '',
              '## 训练可用池原始非有限比例', '',
              table(['特征', '非有限比例'], [[name, pct(value)] for name, value in c['nonfinite_shares'].items()]), '',
              '## 偏离与字面解释', '']
    lines += ['- ' + d for d in result['deviations']]
    lines += [f"- R′/RF′ 的 train+val 按 make_splits 的两段掩码取样，验证标签日期为 "
              f"{'～'.join(c['splits']['val'])}，与主回测验证段截止 2026-04-30 不同。"
              f"最后标签需要 {(datetime.strptime(c['splits']['val'][1], '%Y-%m-%d') + timedelta(days=H + 1)).strftime('%Y-%m-%d')} "
              '开盘，因此描述性重训的信息终点晚于前向起点；不能当作完整前向可部署的因果对照，不参与判定。']
    lines += ['', '## 哈希、复现和局限', '']
    lines += [f'- {key}：`{value}`。' for key, value in result['hashes'].items()]
    lines += [f'- 通用特征清单：`{FEATURE_HASH}`。', '',
              f"运行前后哈希一致；输入共 {result['file_counts']['crypto']} 文件。"
              f"B 分数严格复现：{c['reproduction']['score_exact_match']}；"
              f"B 前向每日收益严格复现：{c['reproduction']['forward_daily_returns_exact_match']}。", '',
              '训练标签重叠，CV 剔除两侧各 5 个训练日期。冻结存活币池存在幸存者偏差；七因子曾在训练段选择；'
              '通用特征设计已知低波动、低 beta 和资金费历史结论。多重检验按预注册 K=5/6；不做参数追调。'
              '前向标签只在冻结模型后用于描述。若失败，按预注册关闭当前数据上的多特征学习方向，等待新数据证据。', '',
              '账本：' + (json.dumps(result['ledger'], ensure_ascii=False) if result.get('ledger') else '未写入。'), '',
              f"运行命令：`{result['command']}`。研究运行次数 {result['attempt']}，耗时 {result['elapsed_seconds']:.2f} 秒。"
              '参数保持冻结；未改正式配置。完整折索引、系数、逐日收益及审计见 report.json。', '']
    return '\n'.join(lines)


def validate_outputs(root, outputs):
    return study.validate_outputs(root, outputs)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    group = parser.add_mutually_exclusive_group()
    group.add_argument('--ledger', type=Path)
    group.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--allow-input-drift', action='store_true')
    args = parser.parse_args(argv)
    ledger_path = None if args.no_ledger else args.ledger or args.out / 'ledger.jsonl'
    validate_outputs(args.root, [args.out])
    paths = [args.out / 'report.json', args.out / 'report.md', args.out / 'attempts.jsonl']
    validate_outputs(args.root, paths + ([ledger_path] if ledger_path else []))
    guard_features()
    hashes, count = input_hashes(args.root, 'crypto')
    mismatches = guard_hashes(hashes, args.allow_input_drift)
    print('输入和特征哈希已核对；开始冻结的多特征 ML 研究。', flush=True)
    frozen, _, provenance = study.load_frozen(args.root)
    if len(frozen[H]) != 7:
        raise ValueError('the frozen h=3 ensemble must contain seven factors')
    panels = panel.load_panels('crypto', args.root)
    started = time.monotonic()
    args.out.mkdir(parents=True, exist_ok=True)
    attempts_path = args.out / 'attempts.jsonl'
    old = [json.loads(line) for line in attempts_path.read_text().splitlines()] if attempts_path.exists() else []
    attempt = len(old) + 1
    with attempts_path.open('a') as stream:
        stream.write(json.dumps(dict(attempt=attempt, started_at=datetime.now(timezone.utc).isoformat())) + '\n')
    deviations = [
        'style_neutral_study.percentile_ranks 是 rank/n−0.5，不含半秩偏移；另写 rank_map 严格采用 (rank−0.5)/n−0.5，特征再乘 √12。',
        'λ 尺度由全训练样本的特征 Gram 一次计算；每折沿用相同绝对 λ，不用评估标签计算尺度。R′/RF′也保留所选绝对 λ。',
        '日期与 msrr_study 一致：验证固定 2025-05-08～2026-04-30，前向固定 2026-05-04 至面板末日；当前 split 的 val 起点为 2025-05-08、holdout 起点为 2026-05-08。',
        '非有限比例的分母按字面取所有 train-split×mask 可用格，未按标签有限性筛除；少于10币日期也计入该检查。',
        '逐日 Spearman 使用双方有限交集、平均秩，至少两个非恒定观测；未另设有限标签数量门槛。噪声臂训练 IC 对真实标签和置换拟合标签均报告。',
        '训练终点区分信号日与信息日：forward_target 的最后训练日 t 标签必需 t+4 开盘；严格保留此定义，因果性测试在最后标签所需开盘之后扰动全部价格，并另检验训练信号日后收盘价及标签扰动。',
        '多空腿按 score 经 build_targets/rank_weights 得到的最终正负权重分解，BTC 随其正负归属，不复用 MSRR 的单列 BTC 拆法。',
        'RF 分块为2048行；同一特征 Gram 的特征分解复用于所有 λ 和真/噪声 RHS，等价于闭式岭回归。未保存或物化全体随机特征矩阵。',
        '本地隔离账本的实际 K 由 append_event 计算；判定 z_K 使用预注册 K=5/6，另存 preregistered_K，不伪造历史行。',
        '按任务仅新增两个源码文件；研究结果和临时账本写入 .ml_out，研究日志与正式账本留主 agent 后续登记。',
    ]
    if attempt > 1:
        deviations.append(f'研究启动 {attempt} 次，之前启动记录见 attempts.jsonl；此前崩溃后只修实现，未改冻结参数。')
    if mismatches:
        deviations.append('--allow-input-drift 放行输入漂移：' + json.dumps(mismatches))
    result = dict(generated_at=datetime.now(timezone.utc).isoformat(), hashes=hashes, feature_hash=FEATURE_HASH,
                  file_counts=dict(crypto=count), provenance=provenance, hash_mismatches=mismatches,
                  attempt=attempt, deviations=deviations,
                  command='python -m scripts.factor_mining.ml_multifeature_study ' + ' '.join(sys.argv[1:] if argv is None else argv))
    result['crypto'] = crypto_study(panels, frozen[H], lambda msg: print(msg, flush=True))
    if input_hashes(args.root, 'crypto')[0] != hashes:
        raise ValueError('snapshot changed during study; refusing to publish')
    result['elapsed_seconds'] = time.monotonic() - started
    # Keep nonfinite verdict inputs until after classification; JSON encodes missing diagnostics as null.
    result = study.json_safe(result)
    if ledger_path:
        result['ledger'] = [dict(path=str(ledger_path.resolve()), **event) for event in append_ledger(ledger_path, result)]
    (args.out / 'report.json').write_text(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + '\n')
    (args.out / 'report.md').write_text(render_report(result), encoding='utf-8')
    for arm, p in result['crypto']['paired'].items():
        print(f"{arm}：{p['verdict']}；z={p['z']}；z_noise={p['z_noise']}；z_K={p['z_K']}", flush=True)
    print(f'报告：{args.out / "report.md"}', flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
