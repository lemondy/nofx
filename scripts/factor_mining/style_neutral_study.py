"""Preregistered, offline style-neutralization S1; all reported P&L is Backtrader."""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import time
from datetime import datetime, timezone

import numpy as np
import pandas as pd

from . import config, ensemble, ensemble_forward_study as study, evaluate, panel
from .expr import evaluate_expr
from scripts.backtest.evolution_ledger import append_event


H = 3
VAL_RANGE = ('2025-05-08', study.VAL_RANGE[1])
STYLES = ('S1', 'S2', 'S3', 'S4', 'S5')
EXPECTED_HASHES = {
    'crypto.aggregate': 'f60602b5feb8061702afc4a2c53296e3c87ab077669477c9635a0da9826cc4f0',
    'crypto.zoo': 'cd9de8374450155e0f575da9e0ae8967722728910b16c064fb8f5eb0a17ad96e',
    'crypto.ensemble': '5e3599f89dcaf4b73d4712a674c720004f25cd77eaaf08fb9c0f441a083feeb1',
    'us.aggregate': 'a262c75ee127a51deb79a51e001b38e0f87ca15083026bc32f42b4a64b9f358a',
}


def input_hashes(root, market):
    """Match find MARKET -type f | LC_ALL=C sort | xargs cat | shasum."""
    root = Path(root)
    paths = sorted((p for p in (root / market).rglob('*')
                    if p.is_file() and not p.is_symlink()),
                   key=lambda p: os.fsencode(p.relative_to(root).as_posix()))
    if not paths:
        raise ValueError(f'empty snapshot: {root / market}')
    digest = hashlib.sha256()
    for path in paths:
        with path.open('rb') as stream:
            for block in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(block)
    result = {f'{market}.aggregate': digest.hexdigest()}
    for name in ('zoo', 'ensemble'):
        result[f'{market}.{name}'] = hashlib.sha256(
            (root / market / f'{name}.json').read_bytes()).hexdigest()
    return result, len(paths)


def guard_hashes(actual, allow_input_drift=False, expected=None):
    expected = EXPECTED_HASHES if expected is None else expected
    mismatches = {key: dict(expected=expected[key], actual=value)
                  for key, value in actual.items() if key in expected and value != expected[key]}
    if mismatches and not allow_input_drift:
        raise ValueError('input hash mismatch; refusing to run: ' + json.dumps(mismatches))
    return mismatches


def style_variables(panels, market):
    """Trailing calendar bars, full 30-bar windows, pairwise 40/60 beta."""
    close, volume = panels.fields['close'], panels.fields['volume']
    returns = close.pct_change(fill_method=None).replace([np.inf, -np.inf], np.nan)
    if market == 'crypto':
        benchmark = returns['BTCUSDT']
        short, medium = 7, 30
    elif market == 'us':
        if panels.benchmark_close is None:
            raise ValueError('US styles require SPY close')
        benchmark = panels.benchmark_close.pct_change(fill_method=None)
        short, medium = 21, 126
    else:
        raise ValueError(f'unknown market: {market}')
    # Both moments use the same pairwise observations for every name.
    x = pd.DataFrame(np.broadcast_to(benchmark.to_numpy()[:, None], close.shape),
                     index=close.index, columns=close.columns).where(returns.notna())
    y = returns.where(np.isfinite(x))
    mx, my = x.rolling(60, min_periods=40).mean(), y.rolling(60, min_periods=40).mean()
    covariance = (x * y).rolling(60, min_periods=40).mean() - mx * my
    variance = (x * x).rolling(60, min_periods=40).mean() - mx * mx
    amount = (close * volume).rolling(30, min_periods=30).mean()
    styles = dict(S1=np.log(amount.where(amount > 0)),
                  S2=covariance / variance.where(variance > 0),
                  S3=returns.rolling(30, min_periods=30).std(ddof=1),
                  S4=close / close.shift(short) - 1,
                  S5=close / close.shift(medium) - 1)
    return {key: value.replace([np.inf, -np.inf], np.nan) for key, value in styles.items()}


def percentile_ranks(values, mask):
    return values.where(mask & np.isfinite(values)).rank(axis=1, method='average', pct=True) - .5


class Neutralizer:
    """Cache style ranks; one small numpy lstsq per eligible date and factor."""

    def __init__(self, styles, mask, fit_rows=None):
        self.mask = mask
        self.ranks = {key: percentile_ranks(styles[key], mask) for key in STYLES}
        self.x = np.stack([self.ranks[key].to_numpy() for key in STYLES], axis=2)
        self.valid = np.isfinite(self.x).all(axis=2) & mask.to_numpy()
        self.rows = np.arange(len(mask)) if fit_rows is None else np.flatnonzero(fit_rows)

    def fit(self, values):
        y = percentile_ranks(values, self.mask).to_numpy()
        residual = np.full(y.shape, np.nan)
        r2 = np.full(len(y), np.nan)
        valid = self.valid & np.isfinite(y)
        for i in self.rows:
            use = valid[i]
            if use.sum() < 15:
                continue
            x = np.column_stack((np.ones(use.sum()), self.x[i, use]))
            target = y[i, use]
            error = target - x @ np.linalg.lstsq(x, target, rcond=None)[0]
            residual[i, use] = error
            sst = np.sum((target - target.mean()) ** 2)
            if sst > 0:
                r2[i] = 1 - np.dot(error, error) / sst
        return (pd.DataFrame(residual, index=values.index, columns=values.columns),
                pd.Series(r2, index=values.index))


def ic_statistics(values, excess, mask, rows, lag):
    ic = evaluate.cs_rank_ic(values.loc[rows], excess.loc[rows], mask.loc[rows], 15).dropna()
    return dict(ic=float(ic.mean()) if len(ic) else None, nw_t=evaluate.nw_t(ic, lag), n=len(ic))


def distribution(values):
    x = np.asarray(list(values), dtype=float)
    x = x[np.isfinite(x)]
    return dict(n=len(x), mean=float(x.mean()) if len(x) else None,
                **{name: float(np.quantile(x, q)) if len(x) else None
                   for name, q in [('min', 0), ('p25', .25), ('median', .5), ('p75', .75), ('max', 1)]})


def weekly_statistics(delta):
    """CR0 mean SE, ISO Monday weeks, exactly the preregistered formula."""
    if not isinstance(delta.index, pd.DatetimeIndex) or not delta.index.is_unique:
        raise ValueError('unique dated daily differences required')
    x = delta.to_numpy(dtype=float)
    if not len(x) or not np.isfinite(x).all():
        raise ValueError('nonempty finite paired daily differences required')
    mean = float(x.mean())
    weeks = delta.index.normalize() - pd.to_timedelta(delta.index.weekday, unit='D')
    sums = pd.Series(x - mean).groupby(weeks).sum()
    se = float(np.sqrt(np.dot(sums, sums)) / len(x))
    z = mean / se if se else math.copysign(math.inf, mean) if mean else 0.
    return dict(n=len(x), n_weeks=len(sums), mean_delta=mean, se=se, z=z,
                cluster_sums={study.day(d): float(v) for d, v in sums.items()})


def verdict(z, val_mean, forward_mean):
    return ('screen_pass' if z is not None and not math.isnan(z) and z >= 1.
            and val_mean is not None and val_mean > 0
            and forward_mean is not None and forward_mean > 0 else 'fail')


def factor_description(factor, values, residual, r2, styles, panels, labels, rows):
    direction = factor['direction']
    result = {key: factor[key] for key in ('id', 'horizon', 'expr', 'direction')}
    result['style_r2_val'] = float(r2.loc[rows['val']].mean())
    result['style_r2_val_days'] = int(r2.loc[rows['val']].notna().sum())
    result['segments'] = {}
    for name, selected in rows.items():
        result['segments'][name] = dict(
            raw=ic_statistics(values * direction, labels[name], panels.mask, selected, factor['horizon']),
            neutral=ic_statistics(residual * direction, labels[name], panels.mask, selected, factor['horizon']),
            style_correlations={key: ic_statistics(values * direction, value, panels.mask,
                                                   selected, factor['horizon'])['ic']
                                for key, value in styles.items()})
    return result


def leg_backtests(panels, targets):
    """Standalone +/-0.5 gross legs, on the same initial total capital."""
    frames = study.panel_frames(panels, targets.index)
    funding = study.funding_rates(panels.fields['funding'], targets.index)
    result = {}
    for name, weights in [('long', targets.clip(lower=0)), ('short', targets.clip(upper=0))]:
        run = study.q.run_weights(
            frames, weights, market='crypto', commission=study.COMMISSION, slippage=study.SLIPPAGE,
            funding=funding, policy=study.q.RebalancePolicy(None, rebalance_on_signal_change=True))
        stats, returns = study.daily_statistics(run['daily_equity'])
        result[name] = dict(metrics=stats, engine_version=run['engine_version'],
                            daily_equity=run['daily_equity'], daily_returns=dated(returns))
    return result


def dated(series):
    return {study.day(d): float(v) for d, v in series.items()}


def crypto_study(panels, factors, zoo, progress=print):
    index = panels.mask.index
    splits = panel.make_splits(index, H, panels.windows)
    end = study.day(index[-1])
    ranges = dict(val=VAL_RANGE, forward=(study.FORWARD_START, end))
    rows = {name: pd.Series((index >= pd.Timestamp(a)) & (index <= pd.Timestamp(b)), index=index)
            for name, (a, b) in ranges.items()}
    val_excess, _ = panel.forward_target(panels, H)
    fwd_excess, fwd_raw = panel.forward_labels(panels, H, study.FORWARD_START)
    rows['forward_ic'] = study.forward_segment(fwd_excess, study.FORWARD_START)
    labels = dict(val=val_excess, forward=fwd_excess)
    styles = style_variables(panels, 'crypto')
    neutralizer = Neutralizer(styles, panels.mask, rows['val'] | rows['forward'])
    raw_cache, neutral_cache, descriptions = {}, {}, []
    accepted = sorted((f for f in zoo['factors'] if f['status'] == 'accepted'),
                      key=lambda f: (int(f['horizon']), f['id']))
    members = {(f['id'], f['horizon']) for f in factors}
    zoo_rows, target_cache = [], {H: val_excess}
    for f in accepted:
        values = evaluate_expr(f['expr'], panels.fields)
        residual, r2 = neutralizer.fit(values)
        h = int(f['horizon'])
        if h not in target_cache:
            target_cache[h] = panel.forward_target(panels, h)[0]
        # Zoo IC uses its own registered horizon and purged labels.
        raw_ic = ic_statistics(values * f['direction'], target_cache[h], panels.mask, rows['val'], h)
        neutral_ic = ic_statistics(residual * f['direction'], target_cache[h], panels.mask, rows['val'], h)
        zoo_rows.append(dict(id=f['id'], horizon=h, style_r2_val=float(r2.loc[rows['val']].mean()),
                             raw=raw_ic, neutral=neutral_ic,
                             ic_change=(neutral_ic['ic'] - raw_ic['ic'])
                             if raw_ic['ic'] is not None and neutral_ic['ic'] is not None else None))
        if (f['id'], h) in members:
            raw_cache[f['id']], neutral_cache[f['id']] = values, residual
            descriptions.append(factor_description(f, values, residual, r2, styles, panels, labels,
                                                    dict(val=rows['val'], forward=rows['forward_ic'])))
    # Resolve ensemble members even if a drifted zoo no longer marks them accepted.
    for f in factors:
        if f['id'] not in raw_cache:
            values = evaluate_expr(f['expr'], panels.fields)
            residual, r2 = neutralizer.fit(values)
            raw_cache[f['id']], neutral_cache[f['id']] = values, residual
            descriptions.append(factor_description(f, values, residual, r2, styles, panels, labels,
                                                    dict(val=rows['val'], forward=rows['forward_ic'])))
    baseline = ensemble.ensemble_scores(factors, panels, values_cache=raw_cache)
    neutral = ensemble.ensemble_scores(factors, panels, values_cache=neutral_cache)
    reference = ensemble.ensemble_scores(factors, panels)
    pd.testing.assert_frame_equal(baseline, reference, check_exact=True)
    style_factors, training = [], {}
    for key in STYLES:
        stats = ic_statistics(styles[key], val_excess, panels.mask, splits['train'], H)
        direction = -1 if stats['ic'] is not None and stats['ic'] < 0 else 1
        training[key] = dict(stats, direction=direction)
        style_factors.append(dict(id=key, direction=direction))
    composite = ensemble.ensemble_scores(style_factors, panels, values_cache=styles)
    scores = dict(B=baseline, N=neutral, style_only=composite)
    results, differences, arm_returns = {}, {}, {}
    for name, (start, stop) in ranges.items():
        results[name] = dict(range=[start, stop], arms={})
        selected = rows['val'] if name == 'val' else rows['forward_ic']
        returns = {}
        for arm, score in scores.items():
            progress(f'加密 {name}/{arm}：Backtrader 独立回测。')
            run, rets, targets = study.backtest(panels, score, start, stop, H)
            arm_returns[name, arm] = rets
            description = dict(backtest=run, daily_returns=dated(rets),
                               ic=ic_statistics(score, labels[name], panels.mask, selected, H))
            if arm in ('B', 'N'):
                description['legs'] = leg_backtests(panels, targets)
                returns[arm] = rets
            results[name]['arms'][arm] = description
        if not returns['B'].index.equals(returns['N'].index):
            raise ValueError('B/N daily calendars differ')
        delta = returns['N'] - returns['B']
        differences[name] = delta
        results[name]['paired'] = dict(weekly_statistics(delta), daily_delta=dated(delta))
    combined = weekly_statistics(pd.concat(list(differences.values())).sort_index())
    combined['verdict'] = verdict(combined['z'], results['val']['paired']['mean_delta'],
                                  results['forward']['paired']['mean_delta'])
    progress('核对原研究 h=3：相同冻结分数、前向调用和原验证窗口。')
    old_val, _, _ = study.backtest(panels, reference, *study.VAL_RANGE, H)
    reference_forward, reference_returns, _ = study.backtest(
        panels, reference, study.FORWARD_START, end, H)
    pd.testing.assert_series_equal(reference_returns, arm_returns['forward', 'B'], check_exact=True)
    # The legacy signal helper's P&L diagnostics are discarded: only IC is reused.
    legacy = study.signal_metrics(reference, fwd_excess, fwd_raw, panels.mask,
                                   study.holding_funding(panels.fields['funding'], H),
                                   H, study.FORWARD_START, config.MARKETS['crypto'])
    actual_ic = results['forward']['arms']['B']['ic']
    if not np.isclose(legacy['ic'], actual_ic['ic'], atol=1e-14, rtol=0):
        raise ValueError('reference forward signal IC mismatch')
    reproduction = dict(score_exact_match=True, forward_same_score_window_and_backtest=True,
                        forward_daily_returns_exact_match=True,
                        reference_forward_metrics=reference_forward['metrics'],
                        forward_metrics=results['forward']['arms']['B']['backtest']['metrics'],
                        original_val_range=list(study.VAL_RANGE), original_val_metrics=old_val['metrics'],
                        prereg_val_range=list(VAL_RANGE),
                        prereg_val_metrics=results['val']['arms']['B']['backtest']['metrics'],
                        reference_forward_ic={k: legacy[k] for k in ('ic', 'nw_t', 'n_days')},
                        archived_report_available=False,
                        note='复现原模块 h=3 执行路径；未提供历史报告数值，不能声称与历史归档逐值一致。')
    return dict(panel_meta=panels.meta, splits=splits['ranges'], factors=descriptions,
                segments=results, paired=combined, reproduction=reproduction,
                style_only_training=training,
                accepted_zoo=dict(n=len(zoo_rows), rows=zoo_rows,
                                  r2=distribution(f['style_r2_val'] for f in zoo_rows),
                                  raw_ic=distribution(f['raw']['ic'] for f in zoo_rows),
                                  neutral_ic=distribution(f['neutral']['ic'] for f in zoo_rows),
                                  ic_change=distribution(f['ic_change'] for f in zoo_rows)))


def us_study(panels, zoo, progress=print):
    h = 10
    splits = panel.make_splits(panels.mask.index, h, panels.windows)
    excess, _ = panel.forward_target(panels, h)
    styles = style_variables(panels, 'us')
    neutralizer = Neutralizer(styles, panels.mask, splits['val'])
    candidates = sorted((f for f in zoo['factors'] if int(f['horizon']) == h
                         and f['status'] == 'candidate'), key=lambda f: (-f['train']['t'], f['id']))[:300]
    rows = []
    for i, f in enumerate(candidates):
        if i % 50 == 0:
            progress(f'美股描述：{i}/{len(candidates)}，只计算验证段 OLS/IC。')
        values = evaluate_expr(f['expr'], panels.fields)
        residual, r2 = neutralizer.fit(values)
        raw = ic_statistics(values * f['direction'], excess, panels.mask, splits['val'], h)
        neutral = ic_statistics(residual * f['direction'], excess, panels.mask, splits['val'], h)
        rows.append(dict(id=f['id'], horizon=h, expr=f['expr'], direction=f['direction'],
                         frozen_train_t=f['train']['t'], style_r2_val=float(r2.loc[splits['val']].mean()),
                         raw=raw, neutral=neutral,
                         validation_t_pass=neutral['n'] > 0 and neutral['nw_t'] >= 1.65))
    return dict(skipped=False, panel_meta=panels.meta, ranges=splits['ranges'], n=len(rows), rows=rows,
                nw_lag=h, validation_t_pass=sum(f['validation_t_pass'] for f in rows),
                raw_validation_t_pass=sum(f['raw']['n'] > 0 and f['raw']['nw_t'] >= 1.65 for f in rows),
                r2=distribution(f['style_r2_val'] for f in rows),
                raw_ic=distribution(f['raw']['ic'] for f in rows),
                neutral_ic=distribution(f['neutral']['ic'] for f in rows),
                neutral_nw_t=distribution(f['neutral']['nw_t'] for f in rows))


def append_ledger(path, result):
    return append_event(path, 'fq_crypto', lambda k, previous: dict(
        stage='S1', change_type='E', idea_id='fq-crypto-style-neutral-v1',
        baseline_version='ensemble@5e3599f8', verdict=result['crypto']['paired']['verdict'],
        preregistered_K=3, z=result['crypto']['paired']['z'], paired=result['crypto']['paired'],
        segment_delta={name: s['paired']['mean_delta'] for name, s in result['crypto']['segments'].items()},
        ensemble_ic={name: {arm: s['arms'][arm]['ic'] for arm in ('B', 'N')}
                     for name, s in result['crypto']['segments'].items()},
        input_hashes=result['hashes'], deviations=result['deviations']))


def render_report(result):
    c, fmt, pct, table = result['crypto'], study.fmt, study.pct, study.table
    p = c['paired']
    lines = ['# 风格中性化 S1：加密 ensemble 与美股描述', '',
             f"结论：**{p['verdict']}**；合并日均 Δ={pct(p['mean_delta'])}，周聚类 SE={pct(p['se'])}，"
             f"z={fmt(p['z'])}；{p['n']} 日、{p['n_weeks']} 个自然周。", '',
             '验证和前向都已在旧研究中看过，本次仅为离线筛选，不构成样本外证据。', '',
             '## 主检验', '',
             table(['分段', 'B 净收益', 'B Sharpe', 'N 净收益', 'N Sharpe', '日均 Δ', 'z', '判定'],
                   [[name, pct(s['arms']['B']['backtest']['metrics']['total_return']),
                     fmt(s['arms']['B']['backtest']['metrics']['sharpe']),
                     pct(s['arms']['N']['backtest']['metrics']['total_return']),
                     fmt(s['arms']['N']['backtest']['metrics']['sharpe']),
                     pct(s['paired']['mean_delta']), fmt(s['paired']['z']), '仅描述']
                    for name, s in c['segments'].items()] +
                   [['合并', '分段独立回测', '—', '分段独立回测', '—', pct(p['mean_delta']), fmt(p['z']), p['verdict']]]), '',
             '两段各自从现金开始；每 3 日调仓，多空各 0.5 毛敞口，前后 20% 等权。'
             '佣金 5bp、滑点 3bp、实际资金费；所有列示收益均由 Backtrader 产生。'
             'Sharpe 按日收益样本标准差、年化 √365；含首日现金收益和最终收盘估值，不伪造平仓。', '',
             'SE=√Σ周[Σ日(Δ−合并均值)]²/n，以星期一分组，不加有限样本修正；'
             'screen_pass 当且仅当合并 z≥1 且两段日均 Δ 都严格大于 0。', '',
             '## Ensemble IC 与分腿收益', '',
             table(['分段', '组', '秩 IC', 'NW t（lag=3）', 'IC 日数', '多头净收益', '空头净收益'],
                   [[name, arm, fmt(a['ic']['ic']), fmt(a['ic']['nw_t']), a['ic']['n'],
                     pct(a['legs']['long']['metrics']['total_return']),
                     pct(a['legs']['short']['metrics']['total_return'])]
                    for name, s in c['segments'].items() for arm in ('B', 'N') for a in [s['arms'][arm]]]), '',
             '分腿为各自从相同初始总资金独立运行、保留原 ±0.5 目标权重的 Backtrader 组合；'
             '空头收益为做空损益，包含资金费与成本。独立权益漂移使两腿收益不必加总为双腿收益。', '',
             '## 只用五个风格的合成分数', '',
             table(['风格', '训练 IC', '训练 NW t', '冻结方向'],
                   [[key, fmt(v['ic']), fmt(v['nw_t']), v['direction']] for key, v in c['style_only_training'].items()]), '',
             table(['分段', 'IC', 'NW t', '多空净收益', 'Sharpe'],
                   [[name, fmt(a['ic']['ic']), fmt(a['ic']['nw_t']), pct(a['backtest']['metrics']['total_return']),
                     fmt(a['backtest']['metrics']['sharpe'])]
                    for name, s in c['segments'].items() for a in [s['arms']['style_only']]]), '',
             '方向仅按训练 IC 符号确定（零或缺失取 +1），原始风格值按 ensemble_scores 截面 z 分数等权合成。', '',
             '## 七个因子的风格解释与 IC', '',
             'R² 为验证段每日带截距回归 R² 的均值；IC 与风格相关均沿冻结 direction，'
             '每列先在 mask 内独立百分位排名，再对风格和因子完整的行做 OLS，少于 15 名则整日缺失。', '',
             table(['因子', '方向', '验证 R²', '分段', '原始 IC / t', '中性 IC / t', 'S1 相关', 'S2 相关',
                    'S3 相关', 'S4 相关', 'S5 相关'],
                   [[f['id'], f['direction'], fmt(f['style_r2_val']), name,
                     f"{fmt(s['raw']['ic'])} / {fmt(s['raw']['nw_t'])}",
                     f"{fmt(s['neutral']['ic'])} / {fmt(s['neutral']['nw_t'])}",
                     *[fmt(s['style_correlations'][key]) for key in STYLES]]
                    for f in c['factors'] for name, s in f['segments'].items()]), '',
             'S1=log(30日平均成交额 close×volume)；S2=60日对 BTC/SPY 简单收益的含截距 beta（至少40对）；'
             'S3=30日简单收益样本标准差；S4=7日（美股21日）动量；S5=30日（美股126日）动量。', '',
             '## Accepted zoo 描述', '', f"全部 accepted 条目 {c['accepted_zoo']['n']} 个（按因子 ID 与持有期计）。", '',
             distribution_table(c['accepted_zoo'], ['r2', 'raw_ic', 'neutral_ic', 'ic_change']), '',
             '每个 zoo 条目使用自己的 h 日超额收益标签和 NW lag=h；验证日期裁剪到主检验区间。'
             '变化=中性 IC−原始 IC；逐条明细保存于 report.json。', '',
             '## 美股描述', '']
    us = result['us']
    if us['skipped']:
        lines += ['由 --skip-us 跳过。', '']
    else:
        lines += [f"仅选 h=10、status=candidate、训练 t 降序的前 {us['n']} 个（同分按 ID），"
                  f"验证段 {us['ranges']['val'][0]}～{us['ranges']['val'][1]}。方向保持冻结；NW lag=10。"
                  f"原始 t≥1.65：{us['raw_validation_t_pass']}；中性化后 t≥1.65：{us['validation_t_pass']}。"
                  '通过只指描述性 IC t 门槛，不代表 accepted 或交易可用，不计入 K。', '',
                  distribution_table(us, ['r2', 'raw_ic', 'neutral_ic', 'neutral_nw_t']), '',
                  '逐候选表达式、冻结训练 t、R²、原始/中性 IC 与 t 均保存在 report.json；未计算美股收益。', '']
    r = c['reproduction']
    lines += ['## B 臂复现核对', '',
              f"直接调用原 ensemble_scores 与缓存版本逐格完全相同：{r['score_exact_match']}。"
              '前向 B 使用相同 h=3 分数、FORWARD_START、终点与 backtest 调用，独立重算每日收益逐格完全一致；'
              f"IC 与 signal_metrics 一致：{fmt(r['reference_forward_ic']['ic'], 8)}。", '',
              table(['窗口', 'B 净收益', 'B Sharpe'],
                    [['原研究 val：' + '～'.join(r['original_val_range']),
                      pct(r['original_val_metrics']['total_return']), fmt(r['original_val_metrics']['sharpe'])],
                     ['本次 val：' + '～'.join(r['prereg_val_range']),
                      pct(r['prereg_val_metrics']['total_return']), fmt(r['prereg_val_metrics']['sharpe'])],
                     ['前向：' + '～'.join(c['segments']['forward']['range']),
                      pct(r['forward_metrics']['total_return']), fmt(r['forward_metrics']['sharpe'])]]), '', r['note'], '',
              '## 偏离与字面解释', '']
    lines += ['- ' + item for item in result['deviations']]
    lines += ['', '## 输入哈希', '']
    lines += [f'- {key}：`{value}`。' for key, value in result['hashes'].items()]
    lines += ['', '输入文件数：' + json.dumps(result['file_counts'], ensure_ascii=False) + '。', '',
              '## 账本与用途', '']
    ledger = result.get('ledger')
    lines += [f"账本：{json.dumps(ledger, ensure_ascii=False)}" if ledger else '未写账本（--no-ledger）。', '',
              ('screen_pass 仅允许另行预注册在线配对设计，不直接上线。' if p['verdict'] == 'screen_pass' else
               'fail：记录风格解释与中性化效果，线上不做改动。'), '',
              f"运行耗时 {result['elapsed_seconds']:.2f} 秒；参数与预注册保持冻结。", '']
    return '\n'.join(lines)


def distribution_table(result, keys):
    labels = dict(r2='风格 R²', raw_ic='原始 IC', neutral_ic='中性 IC', ic_change='IC 变化', neutral_nw_t='中性 NW t')
    return study.table(['统计项', '有效数', '均值', '最小', '25%', '中位', '75%', '最大'],
                       [[labels[key], result[key]['n'], *[study.fmt(result[key][q])
                         for q in ('mean', 'min', 'p25', 'median', 'p75', 'max')]] for key in keys])


def validate_outputs(roots, outputs):
    for root in roots:
        study.validate_outputs(root, outputs)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--crypto-root', required=True, type=Path)
    parser.add_argument('--us-root', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    group = parser.add_mutually_exclusive_group()
    group.add_argument('--ledger', type=Path)
    group.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--allow-input-drift', action='store_true')
    parser.add_argument('--skip-us', action='store_true')
    args = parser.parse_args(argv)
    ledger_path = None if args.no_ledger else args.ledger or args.out / 'ledger.jsonl'
    paths = [args.out / 'report.json', args.out / 'report.md'] + ([ledger_path] if ledger_path else [])
    validate_outputs([args.crypto_root, args.us_root], paths)
    started = time.monotonic()
    hashes, count = input_hashes(args.crypto_root, 'crypto')
    counts = dict(crypto=count)
    if not args.skip_us:
        us_hashes, counts['us'] = input_hashes(args.us_root, 'us')
        hashes.update(us_hashes)
    mismatches = guard_hashes(hashes, args.allow_input_drift)
    print('输入哈希已核对；开始冻结研究。', flush=True)
    frozen, _, provenance = study.load_frozen(args.crypto_root)
    crypto_panels = panel.load_panels('crypto', args.crypto_root)
    if 'funding' not in crypto_panels.fields:
        raise ValueError('crypto funding is required')
    zoo = json.loads((args.crypto_root / 'crypto/zoo.json').read_text())
    result = dict(generated_at=datetime.now(timezone.utc).isoformat(), hashes=hashes, file_counts=counts,
                  hash_mismatches=mismatches, provenance=provenance,
                  design=dict(h=H, val_range=list(VAL_RANGE), forward_start=study.FORWARD_START,
                              commission=study.COMMISSION, slippage=study.SLIPPAGE, preregistered_K=3),
                  deviations=[
                      '原模块 VAL_RANGE 从 2025-05-04 开始；主检验严格采用新预注册的 2025-05-08，另回测旧窗口核对。',
                      '当前快照 make_splits 的 holdout 起点已滚动到 2026-05-08；前向仍固定为预注册的 2026-05-04，未沿用原 run_study 的起点断言。',
                      'incremental_filter.weekly_statistics 与 replay_score.weighted_cluster 均含 G/(G−1) 修正；无法直接复用以满足指定公式，使用本脚本 weekly_statistics（CR0，无修正）。',
                      '预注册未指定 std 自由度、普通滚动窗口最少数及秩缺失处理细节：std 取 ddof=1，30日窗口需完整30个观测；beta 按最近60行至少40对有效收益；各变量先独立排名再完整案例回归。',
                      '原始因子 IC 与风格相关沿冻结 direction；US 候选字面限定 status=candidate，前300按训练 t、同分按 ID；美股 NW lag=10。',
                      'signal_metrics 仅复用前向 IC 字段作核对，其内部旧式多空诊断全部丢弃；报告收益及判定收益一律来自 Backtrader。',
                      '未提供历史 ensemble_forward_study 归档结果；B 核对为同快照原模块执行路径复现，不声称核实历史报告。',
                      '本地空账本由 append_event 自动计算 K；预注册 K=3 另存 preregistered_K，不伪造历史行。'])
    if mismatches:
        result['deviations'].append('--allow-input-drift 显式放行哈希差异：' + json.dumps(mismatches))
    result['crypto'] = crypto_study(crypto_panels, frozen[H], zoo, lambda msg: print(msg, flush=True))
    result['us'] = (dict(skipped=True) if args.skip_us else us_study(
        panel.load_panels('us', args.us_root),
        json.loads((args.us_root / 'us/zoo.json').read_text()), lambda msg: print(msg, flush=True)))
    # Recheck all input bytes before publishing or appending a ledger event.
    after = input_hashes(args.crypto_root, 'crypto')[0]
    if not args.skip_us:
        after.update(input_hashes(args.us_root, 'us')[0])
    if hashes != after:
        raise ValueError('snapshot changed during study; refusing to publish')
    result['elapsed_seconds'] = time.monotonic() - started
    result = study.json_safe(result)
    args.out.mkdir(parents=True, exist_ok=True)
    if ledger_path:
        event = append_ledger(ledger_path, result)
        result['ledger'] = dict(path=str(ledger_path.resolve()), K=event['K'], preregistered_K=3,
                                track=event['track'], stage=event['stage'], change_type=event['change_type'],
                                idea_id=event['idea_id'], baseline_version=event['baseline_version'],
                                verdict=event['verdict'])
    (args.out / 'report.json').write_text(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + '\n')
    (args.out / 'report.md').write_text(render_report(result), encoding='utf-8')
    print(f"结论：{result['crypto']['paired']['verdict']}；z={result['crypto']['paired']['z']}；报告 {args.out}", flush=True)
    return result


if __name__ == '__main__':
    main()
