"""Preregistered fq_crypto MSRR H1 S1, offline and frozen after training."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import time

import numpy as np
import pandas as pd

from . import ensemble, ensemble_forward_study as study, panel
from .expr import evaluate_expr
from .style_neutral_study import EXPECTED_HASHES, guard_hashes, input_hashes, weekly_statistics
from scripts.backtest.evolution_ledger import append_event


H = 3
VAL_RANGE = ('2025-05-08', '2026-04-30')
Z_MULTIPLIERS = (.001, .01, .1, 1., 10., 100.)
GRID_DIVISOR = 15
FOLDS = 5
EMBARGO = 1
CAP = .10
PERIODS_PER_YEAR = 365 / H
PREREGISTERED_K = 4
PREREG = 'docs/research/2026-11-07-fq-crypto-msrr-prereg.md'


def factor_values(panels, factors, end):
    """Evaluate only the declared prefix, multiplying direction before ranking."""
    fields = {key: value.loc[:end] for key, value in panels.fields.items()}
    return {f['id']: evaluate_expr(f['expr'], fields) * f['direction'] for f in factors}


def leg_names(factors, include_btc=True):
    return [f"{f['id']}/{side}" for f in factors for side in ('L', 'S')] + (['BTC'] if include_btc else [])


def leg_weights(values, eligible, columns, include_btc=True):
    """Rows are factor L/S legs (+1/-1), followed by the unmasked BTC leg."""
    values = np.asarray(values, dtype=float)
    if values.ndim != 2 or values.shape[1] != len(columns):
        raise ValueError('factor rows must match coin columns')
    legs = np.zeros((2 * len(values) + int(include_btc), len(columns)))
    for k, row in enumerate(values):
        ranked = study.rank_weights(row, eligible, .2)
        legs[2 * k] = ranked.clip(min=0) * 2
        legs[2 * k + 1] = ranked.clip(max=0) * 2
    if include_btc:
        legs[-1, columns.get_loc('BTCUSDT')] = 1.
    return legs


def training_returns(panels, factors, start, end):
    """Gross close-to-close managed returns; never read an endpoint past end."""
    close = panels.fields['close'].loc[:end]
    values = factor_values(panels, factors, end)
    mask = panels.mask.loc[close.index]
    positions = study.signal_positions(close.index, start, H)
    rows, dates, ends = [], [], []
    dropped, empty = 0, 0
    for first, last in zip(positions[:-1], positions[1:]):
        legs = leg_weights([values[f['id']].iloc[first] for f in factors],
                           mask.iloc[first], close.columns)
        entry, exit_ = close.iloc[first].to_numpy(), close.iloc[last].to_numpy()
        valid = np.isfinite(entry) & (entry > 0) & np.isfinite(exit_) & (exit_ > 0)
        dropped += int(np.count_nonzero(legs[:, ~valid]))
        legs[:, ~valid] = 0.
        gross = np.abs(legs).sum(axis=1)
        empty += int(np.count_nonzero(gross == 0))
        legs = np.divide(legs, gross[:, None], out=np.zeros_like(legs), where=gross[:, None] > 0)
        rets = np.zeros(len(entry))
        rets[valid] = exit_[valid] / entry[valid] - 1
        rows.append(legs @ rets)
        dates.append(close.index[first])
        ends.append(study.day(close.index[last]))
    result = pd.DataFrame(rows, index=pd.DatetimeIndex(dates), columns=leg_names(factors))
    return result, dict(range=[str(start), str(end)], n_periods=len(result),
                        period_ends=ends, dropped_coin_leg_observations=dropped,
                        empty_leg_periods=empty)


def standardise(returns):
    x = np.asarray(returns, dtype=float)
    if x.ndim != 2 or len(x) < 2 or not np.isfinite(x).all():
        raise ValueError('at least two finite managed-return rows required')
    sigma = x.std(axis=0, ddof=1)
    if not np.isfinite(sigma).all() or np.any(sigma <= 0):
        raise ValueError('positive training standard deviations required')
    return x / sigma, sigma


def ridge_msrr(returns, z):
    """Solve the uncentered ridge MSRR normal equations in standardised units."""
    if not np.isfinite(z) or z <= 0:
        raise ValueError('positive finite ridge z required')
    x, sigma = standardise(returns)
    beta_tilde = np.linalg.solve(x.T @ x / len(x) + z * np.eye(x.shape[1]), x.mean(axis=0))
    return dict(beta=beta_tilde / sigma, beta_tilde=beta_tilde, sigma=sigma)


def z_grid(returns):
    x, _ = standardise(returns)
    lambda_bar = float(np.trace(x.T @ x / len(x)) / GRID_DIVISOR)
    return lambda_bar, np.asarray(Z_MULTIPLIERS) * lambda_bar


def cv_indices(n):
    """Keep every evaluation row; embargo its immediate training neighbours."""
    if n < FOLDS:
        raise ValueError('at least five periods required for block CV')
    folds = []
    for evaluation in np.array_split(np.arange(n), FOLDS):
        keep = np.ones(n, dtype=bool)
        keep[max(0, evaluation[0] - EMBARGO):min(n, evaluation[-1] + EMBARGO + 1)] = False
        train = np.flatnonzero(keep)
        if len(train) < 2:
            raise ValueError('CV training fold needs at least two periods')
        folds.append((train, evaluation))
    return folds


def annualised_sharpe(returns):
    x = np.asarray(returns, dtype=float)
    sd = float(x.std(ddof=1)) if len(x) > 1 else 0.
    return float(x.mean() / sd * math.sqrt(PERIODS_PER_YEAR)) if sd > 1e-15 else 0.


def choose_z(cv):
    """Exact Sharpe ties select the larger absolute z, without rounding."""
    return max(cv, key=lambda row: (row['sharpe'], row['z']))['z']


def fit_model(returns):
    lambda_bar, grid = z_grid(returns)
    x = returns.to_numpy(dtype=float)
    folds = cv_indices(len(x))
    cv = []
    for multiplier, z in zip(Z_MULTIPLIERS, grid):
        oof = np.full(len(x), np.nan)
        for train, evaluation in folds:
            fit = ridge_msrr(x[train], z)
            oof[evaluation] = x[evaluation] @ fit['beta']
        cv.append(dict(multiplier=multiplier, z=float(z), sharpe=annualised_sharpe(oof),
                       oof_returns=dated(pd.Series(oof, index=returns.index))))
    chosen = choose_z(cv)
    fit = ridge_msrr(x, chosen)
    insample = pd.Series(x @ fit['beta'], index=returns.index)
    return dict(columns=list(returns.columns), **{key: value.tolist() for key, value in fit.items()},
                lambda_bar=lambda_bar, chosen_z=chosen, cv=cv,
                cv_folds=[dict(train=train.tolist(), evaluation=evaluation.tolist())
                          for train, evaluation in folds],
                train_sharpe=annualised_sharpe(insample), train_returns=dated(insample))


def normalise_cap(weights):
    weights = np.asarray(weights, dtype=float)
    if not np.isfinite(weights).all():
        raise ValueError('finite coin weights required')
    gross = np.abs(weights).sum()
    return np.clip(weights / gross, -CAP, CAP) if gross > 0 else np.zeros_like(weights)


def build_targets(panels, factors, beta, start, end, *, include_btc=True, values=None):
    """Same segment-anchored signal calendar and prefix truncation as arm B."""
    mask = panels.mask.loc[:end]
    values = factor_values(panels, factors, end) if values is None else {
        key: value.loc[:end] for key, value in values.items()}
    for value in values.values():
        if not value.index.equals(mask.index) or not value.columns.equals(mask.columns):
            raise ValueError('factor and mask axes differ')
    beta = np.asarray(beta, dtype=float)
    if beta.shape != (2 * len(factors) + int(include_btc),) or not np.isfinite(beta).all():
        raise ValueError('finite beta matching managed legs required')
    positions = study.signal_positions(mask.index, start, H)
    targets = pd.DataFrame(0., index=mask.index, columns=mask.columns)
    for j, i in enumerate(positions):
        stop = positions[j + 1] if j + 1 < len(positions) else len(mask)
        legs = leg_weights([values[f['id']].iloc[i] for f in factors],
                           mask.iloc[i], mask.columns, include_btc)
        targets.iloc[i:stop] = normalise_cap(beta @ legs)
    return targets.loc[start:end]


def btc_weight_share(rebalance_weights):
    gross = rebalance_weights.abs().sum(axis=1)
    share = rebalance_weights['BTCUSDT'].abs().div(gross.where(gross > 0)).fillna(0.)
    if share.empty:
        raise ValueError('rebalance weights required for BTC share')
    return float(share.mean())


def verdict(z, val_mean, forward_mean, btc_share):
    return ('screen_pass' if z is not None and not math.isnan(z) and z >= 1.65
            and val_mean is not None and val_mean > 0
            and forward_mean is not None and forward_mean > 0
            and btc_share is not None and 0 <= btc_share <= .5 else 'fail')


def target_backtest(panels, targets, start, end):
    """Use precisely the execution arguments and audit of study.backtest."""
    targets = targets.loc[:end].loc[start:end]
    if not targets.index.equals(pd.date_range(start, end, freq='D')):
        raise ValueError('M requires a complete daily crypto calendar')
    frames = study.panel_frames(panels, targets.index)
    run = study.q.run_weights(
        frames, targets, market='crypto', commission=study.COMMISSION, slippage=study.SLIPPAGE,
        funding=study.funding_rates(panels.fields['funding'], targets.index),
        policy=study.q.RebalancePolicy(None, rebalance_on_signal_change=True), keep_fills=True)
    stats, returns = study.daily_statistics(run['daily_equity'])
    stats.update({key: run['metrics'][key] for key in (
        'turnover', 'commission_paid', 'funding_paid', 'trades',
        'funding_observations', 'funding_covered', 'average_exposure')})
    audit = study.audit_fills(run, targets, panels.fields['open'])
    return dict(period=H, metrics=stats, execution_audit=audit, daily_equity=run['daily_equity'],
                initial_capital=study.q.CAPITAL, engine_version=run['engine_version']), returns


def split_targets(targets):
    """All net BTC exposure goes to BTC; split other coins by their final sign."""
    other = targets.copy()
    other['BTCUSDT'] = 0.
    btc = targets * 0.
    btc['BTCUSDT'] = targets['BTCUSDT']
    return dict(long=other.clip(lower=0), short=other.clip(upper=0), BTC=btc)


def leg_backtests(panels, targets, start, end):
    result = {}
    for name, weights in split_targets(targets).items():
        run, returns = target_backtest(panels, weights, start, end)
        result[name] = dict(backtest=run, daily_returns=dated(returns))
    return result


def dated(series):
    return {study.day(d): float(value) for d, value in series.items()}


def paired_statistics(candidate, baseline):
    if not candidate.index.equals(baseline.index):
        raise ValueError('paired daily calendars differ')
    delta = candidate - baseline
    return dict(weekly_statistics(delta), daily_delta=dated(delta)), delta


def crypto_study(panels, factors, progress=print):
    splits = panel.make_splits(panels.mask.index, H, panels.windows)
    start, end = splits['ranges']['train']
    progress(f'训练管理组合：{start}～{end}；参数仅在训练段估计。')
    managed, training = training_returns(panels, factors, start, end)
    models = dict(M=fit_model(managed), M0=fit_model(managed.iloc[:, :-1]))
    training['managed_returns'] = {key: dated(managed[key]) for key in managed}
    final = study.day(panels.mask.index[-1])
    ranges = dict(val=VAL_RANGE, forward=(study.FORWARD_START, final))
    raw = {f['id']: evaluate_expr(f['expr'], panels.fields) for f in factors}
    directed = {f['id']: raw[f['id']] * f['direction'] for f in factors}
    baseline = ensemble.ensemble_scores(factors, panels, values_cache=raw)
    reference = ensemble.ensemble_scores(factors, panels)
    pd.testing.assert_frame_equal(baseline, reference, check_exact=True)
    results, differences, rebalances, baseline_forward = {}, dict(M=[], M0=[]), [], None
    for name, (start, end) in ranges.items():
        progress(f'{name}/B：原基线 Backtrader 回测。')
        run, b_returns, _ = study.backtest(panels, baseline, start, end, H)
        if name == 'forward':
            baseline_forward = b_returns
        segment = dict(range=[start, end], arms=dict(B=dict(backtest=run, daily_returns=dated(b_returns))))
        for arm, model in models.items():
            progress(f'{name}/{arm}：冻结 β 的 Backtrader 回测。')
            targets = build_targets(panels, factors, model['beta'], start, end,
                                    include_btc=arm == 'M', values=directed)
            run, returns = target_backtest(panels, targets, start, end)
            segment['arms'][arm] = dict(backtest=run, daily_returns=dated(returns))
            paired, delta = paired_statistics(returns, b_returns)
            segment['paired' if arm == 'M' else 'paired_M0_descriptive'] = paired
            differences[arm].append(delta)
            positions = study.signal_positions(targets.index, start, H)
            rebalance = targets.iloc[positions]
            segment['arms'][arm]['rebalance_targets'] = {
                study.day(d): {coin: float(w) for coin, w in row.items()} for d, row in rebalance.iterrows()}
            if arm == 'M':
                rebalances.append(rebalance)
                segment['btc_weight_share'] = btc_weight_share(rebalance)
                progress(f'{name}/M：多头、空头、BTC 三部分分别回测（只描述）。')
                segment['arms'][arm]['legs'] = leg_backtests(panels, targets, start, end)
        results[name] = segment
    share = btc_weight_share(pd.concat(rebalances).sort_index())
    combined = weekly_statistics(pd.concat(differences['M']).sort_index())
    combined['verdict'] = verdict(combined['z'], results['val']['paired']['mean_delta'],
                                  results['forward']['paired']['mean_delta'], share)
    descriptive = weekly_statistics(pd.concat(differences['M0']).sort_index())
    progress('复现核对：原模块、参考分数、前向每日收益必须完全相同。')
    ref_run, ref_returns, _ = study.backtest(panels, reference, *ranges['forward'], H)
    pd.testing.assert_series_equal(ref_returns, baseline_forward, check_exact=True)
    return dict(panel_meta=panels.meta, splits=splits['ranges'], factors=factors, training=training,
                models=models, segments=results, paired=combined, btc_weight_share=share,
                paired_M0_descriptive=descriptive,
                reproduction=dict(score_exact_match=True, forward_daily_returns_exact_match=True,
                                  reference_forward_metrics=ref_run['metrics'],
                                  note='同快照原模块执行路径逐值复现，未核验历史归档报告。'))


def append_ledger(path, result):
    c = result['crypto']
    return append_event(path, 'fq_crypto', lambda k, previous: dict(
        stage='S1', change_type='E', idea_id='fq-crypto-msrr-ridge-v1',
        baseline_version='ensemble@5e3599f8', verdict=c['paired']['verdict'],
        preregistered_K=PREREGISTERED_K, z=c['paired']['z'], paired=c['paired'],
        segment_delta={name: segment['paired']['mean_delta'] for name, segment in c['segments'].items()},
        btc_weight_share=c['btc_weight_share'], chosen_z=c['models']['M']['chosen_z'],
        input_hashes=result['hashes'], deviations=result['deviations']))


def render_report(result):
    c, fmt, pct, table = result['crypto'], study.fmt, study.pct, study.table
    p, m = c['paired'], c['models']['M']
    lines = ['# fq_crypto MSRR 岭收缩 H1：S1 离线筛选', '',
             f"结论：**{p['verdict']}**；z={fmt(p['z'], 8)}；合并日均 Δ={pct(p['mean_delta'])}；"
             f"周聚类 SE={pct(p['se'])}；BTC 权重占比={pct(c['btc_weight_share'])}。", '',
             f"预注册：`{PREREG}`（冻结提交 b91ae01c）。本研究共跑了 M 和 M₀ 两组，K 只按 M 记；"
             'M₀ 和所有分解均只描述，不参与判定。验证和前向至少被四项旧研究看过，不构成样本外证据。', '',
             '## 主检验：M − B', '',
             table(['分段', '区间', 'B 净收益', 'M 净收益', '日均 Δ', '周聚类 SE', 'z'],
                   [[name, '～'.join(s['range']), pct(s['arms']['B']['backtest']['metrics']['total_return']),
                     pct(s['arms']['M']['backtest']['metrics']['total_return']), pct(s['paired']['mean_delta']),
                     pct(s['paired']['se']), fmt(s['paired']['z'], 8)] for name, s in c['segments'].items()] +
                   [['合并', f"{p['n']} 日 / {p['n_weeks']} 自然周", '分段独立', '分段独立',
                     pct(p['mean_delta']), pct(p['se']), fmt(p['z'], 8)]]), '',
             'screen_pass 当且仅当 z≥1.65、验证和前向日均 Δ 均严格大于 0、BTC 权重占比≤0.5。'
             'SE 复用 weekly_statistics：按周一分组，CR0，无有限样本修正。'
             'BTC 占比在两段全部调仓日上合并等权平均，使用封顶后的净币级权重。', '',
             '## 冻结的 β 与交叉验证', '',
             f"训练区间 {'～'.join(c['training']['range'])}；完整 {c['training']['n_periods']} 期；"
             f"λ̄={fmt(m['lambda_bar'], 10)}；选中收缩 z={fmt(m['chosen_z'], 10)}；"
             f"训练样本内毛收益 Sharpe={fmt(m['train_sharpe'], 8)}（仅描述）。", '',
             table(['管理组合', 'β（原始）', 'β̃（标准化）', '训练 σ'],
                   [[name, fmt(beta, 10), fmt(tilde, 10), fmt(sigma, 10)]
                    for name, beta, tilde, sigma in zip(m['columns'], m['beta'], m['beta_tilde'], m['sigma'])]), '',
             table(['λ̄ 倍数', '收缩 z', 'CV 毛收益 Sharpe', '选中'],
                   [[row['multiplier'], fmt(row['z'], 10), fmt(row['sharpe'], 8),
                     '是' if row['z'] == m['chosen_z'] else ''] for row in m['cv']]), '',
             '5 个连续块使用 numpy.array_split；评估块保持完整，训练集剔除评估块及两侧各一期。'
             '每折仅用训练折标准差缩放，不去均值；绝对 z 网格先由全训练段固定，精确平局取更大 z。'
             'CV 及样本内 Sharpe 按 √(365/3) 年化。逐折索引、逐期管理收益和 OOF 收益见 report.json。', '',
             '## B、M 分段指标', '',
             table(['分段', '组', '年化收益', 'Sharpe', '最大回撤', '换手', '手续费', '资金费'],
                   [[name, arm, pct(a['metrics']['annual_return']), fmt(a['metrics']['sharpe']),
                     pct(a['metrics']['max_drawdown']), fmt(a['metrics']['turnover']),
                     fmt(a['metrics']['commission_paid']), fmt(a['metrics']['funding_paid'])]
                    for name, s in c['segments'].items() for arm in ('B', 'M') for a in [s['arms'][arm]['backtest']]]), '',
             '均由 Backtrader run_weights 执行；两段分别从相同初始资金开始。手续费 5bp、滑点 3bp、'
             '实际资金费，RebalancePolicy(None, rebalance_on_signal_change=True)，保留并审计成交。'
             'M 先归一总绝对权重为 1，再逐币截断到 0.10，超出部分留现金；调仓间目标不变。'
             '日收益 Sharpe 按 √365 年化，包含首日现金收益与最终收盘估值，不伪造平仓。', '',
             '## M 币级分解（只描述）', '',
             table(['分段', '部分', '净收益', 'Sharpe', '手续费', '资金费'],
                   [[name, leg, pct(a['metrics']['total_return']), fmt(a['metrics']['sharpe']),
                     fmt(a['metrics']['commission_paid']), fmt(a['metrics']['funding_paid'])]
                    for name, s in c['segments'].items() for leg, run in s['arms']['M']['legs'].items()
                    for a in [run['backtest']]]), '',
             '将最终币权重中 BTCUSDT 的全部净敞口单列，其余按正负分为多头和空头。'
             '各部分保留原权重、相同初始总资金和执行政策，分别回测；权益漂移使收益不必加总为 M。', '',
             '## M₀：去除 BTC 管理列（只描述，不参与判定）', '',
             f"M₀ 独立进行同一 z 选择与重估：λ̄={fmt(c['models']['M0']['lambda_bar'], 10)}；"
             f"选中 z={fmt(c['models']['M0']['chosen_z'], 10)}；"
             f"训练样本内 Sharpe={fmt(c['models']['M0']['train_sharpe'], 8)}。", '',
             table(['分段', 'M₀ 净收益', 'M₀ Sharpe', 'M₀ − B 日均 Δ', '周聚类 SE', 'z'],
                   [[name, pct(s['arms']['M0']['backtest']['metrics']['total_return']),
                     fmt(s['arms']['M0']['backtest']['metrics']['sharpe']),
                     pct(s['paired_M0_descriptive']['mean_delta']), pct(s['paired_M0_descriptive']['se']),
                     fmt(s['paired_M0_descriptive']['z'], 8)] for name, s in c['segments'].items()] +
                   [['合并', '分段独立', '—', pct(c['paired_M0_descriptive']['mean_delta']),
                     pct(c['paired_M0_descriptive']['se']), fmt(c['paired_M0_descriptive']['z'], 8)]]), '',
             'M₀ 的全部 β、CV 网格与逐日配对差保存在 report.json。M₀ 去掉专用 BTC 列，'
             '因子腿仍可选中 BTCUSDT；不额外剔除该币。', '',
             '## B 复现与审计', '',
             f"缓存分数与 ensemble_scores 参考分数完全相同：{c['reproduction']['score_exact_match']}；"
             f"参考 backtest 前向每日收益严格相等：{c['reproduction']['forward_daily_returns_exact_match']}。"
             + c['reproduction']['note'], '',
             '训练只用 tₚ₊₁≤训练终点的完整收盘到收盘收益，不扣成本和资金费；'
             f"剔除缺失价格的币腿观测 {c['training']['dropped_coin_leg_observations']} 个，"
             f"空腿期数 {c['training']['empty_leg_periods']}。各段调仓目标、逐日权益、资金费覆盖与成交审计见 JSON。", '',
             '## 偏离与字面解释', '']
    lines += ['- ' + item for item in result['deviations']]
    lines += ['', '## 输入哈希与来源', '']
    lines += [f'- {key}：`{value}`。' for key, value in result['hashes'].items()]
    lines += ['', f"输入文件数：{result['file_counts']['crypto']}。运行前后重新计算哈希；不匹配默认拒绝。", '',
              '机制与判定依据为预注册的“管理组合、估计 β、币级权重、判定”各节；'
              '实现复用 style_neutral_study 的哈希与周聚类统计、ensemble_forward_study 的基线与执行函数。', '',
              '## 研究局限与用途', '',
              '训练段曾用于挑选七个因子，样本内表现偏乐观；验证及前向已被反复研究，存在多重检验和选择偏差。'
              '沿用冻结名单，无法消除幸存者偏差；训练三日收益不重叠，CV 另设一期隔离。'
              '训练毛收益目标没有成本或资金费，最终判定使用扣费后的 Backtrader 配对收益。'
              '本基线与线上 ls_invvol_protected_v1 不同，只能与既有 S1 口径比较。', '',
              '账本：' + (json.dumps(result['ledger'], ensure_ascii=False) if result.get('ledger') else '未写入（--no-ledger）。'), '',
              ('增量筛选通过：仅可另行预注册在线配对设计并申请批准；本次不改线上。'
               if p['verdict'] == 'screen_pass' else '未通过增量筛选：保留失败结果，不改线上，不推进 H3。'), '',
              f"运行命令：`{result['command']}`。耗时 {result['elapsed_seconds']:.2f} 秒；参数保持冻结。", '']
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
    outputs = [args.out / 'report.json', args.out / 'report.md'] + ([ledger_path] if ledger_path else [])
    validate_outputs(args.root, outputs)
    # Reject the output directory itself even before constructing output files.
    validate_outputs(args.root, [args.out])
    started = time.monotonic()
    hashes, count = input_hashes(args.root, 'crypto')
    mismatches = guard_hashes(hashes, args.allow_input_drift)
    print('输入哈希已核对；开始冻结的 MSRR 研究。', flush=True)
    frozen, _, provenance = study.load_frozen(args.root)
    factors = frozen[H]
    if len(factors) != 7:
        raise ValueError('the preregistered h=3 ensemble must contain seven factors')
    panels = panel.load_panels('crypto', args.root)
    if 'funding' not in panels.fields or 'BTCUSDT' not in panels.mask.columns:
        raise ValueError('crypto funding and BTCUSDT are required')
    command = (f'python -m scripts.factor_mining.msrr_study --root {args.root.resolve()} '
               f'--out {args.out.resolve()} ' +
               (f'--ledger {ledger_path.resolve()}' if ledger_path else '--no-ledger') +
               (' --allow-input-drift' if args.allow_input_drift else ''))
    result = dict(generated_at=datetime.now(timezone.utc).isoformat(), hashes=hashes,
                  file_counts=dict(crypto=count), hash_mismatches=mismatches, provenance=provenance,
                  command=command,
                  design=dict(h=H, z_multipliers=list(Z_MULTIPLIERS), grid_divisor=GRID_DIVISOR,
                              std_ddof=1, folds=FOLDS, embargo=EMBARGO, cap=CAP,
                              val_range=list(VAL_RANGE), forward_start=study.FORWARD_START,
                              commission=study.COMMISSION, slippage=study.SLIPPAGE,
                              preregistered_K=PREREGISTERED_K, candidates=['M', 'M0_descriptive']),
                  deviations=[
                      '标准差自由度未指定：统一取样本标准差 ddof=1；CV 每折只用训练折 σ，不用评估折 σ。',
                      'z 网格按全训练段标准化二阶矩固定为绝对数值，各折不重新缩放 z；M₀ 按“同一程序”字面保留公式的除数 15，虽仅有 14 列。',
                      '连续块按 numpy.array_split 分成五块（余数给前块）；隔离指从训练集剔除评估块前后各一期，评估块自身保持完整。平局使用未舍入 Sharpe 的精确相等。',
                      '训练腿剔除缺失/非正入场或终点价格后按原符号重新归一；整腿无剩余币时记现金收益 0；若训练标准差为零则报错，不增设数值参数。',
                      'BTC 占比使用封顶后的 BTCUSDT 全部净权重（含因子腿叠加），全现金调仓日占比记 0；分解先单列 BTCUSDT，其余按正负拆分。',
                      '训练范围严格使用 make_splits；验证和前向固定采用预注册日期，不使用滚动后的 val/holdout 起点。',
                      '临时空账本的 K 由 append_event 自动累计，预注册 K=4 另记 preregistered_K，不伪造历史行；仅新增指定两份代码文件，研究日志由主 agent 后续汇总。'])
    if mismatches:
        result['deviations'].append('--allow-input-drift 显式放行哈希差异：' + json.dumps(mismatches))
    result['crypto'] = crypto_study(panels, factors, lambda msg: print(msg, flush=True))
    if input_hashes(args.root, 'crypto')[0] != hashes:
        raise ValueError('snapshot changed during study; refusing to publish')
    result['elapsed_seconds'] = time.monotonic() - started
    result = study.json_safe(result)
    args.out.mkdir(parents=True, exist_ok=True)
    if ledger_path:
        event = append_ledger(ledger_path, result)
        result['ledger'] = {key: event[key] for key in (
            'K', 'preregistered_K', 'track', 'stage', 'change_type', 'idea_id', 'baseline_version', 'verdict')}
        result['ledger']['path'] = str(ledger_path.resolve())
    (args.out / 'report.json').write_text(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + '\n', encoding='utf-8')
    (args.out / 'report.md').write_text(render_report(result), encoding='utf-8')
    print(f"结论：{result['crypto']['paired']['verdict']}；z={result['crypto']['paired']['z']}；报告 {args.out}", flush=True)
    return result


if __name__ == '__main__':
    main()
