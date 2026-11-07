"""Frozen H3 descriptive short-leg accounting; offline and without ledger writes."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import math
from pathlib import Path
import sys
import time

import numpy as np
import pandas as pd

from . import ensemble, ensemble_forward_study as study, ml_multifeature_study as ml, panel


H = 3
PREREG = 'docs/research/2026-11-07-fq-crypto-shortleg-convexity-prereg.md'
RANGES = dict(train=('2022-05-09', '2025-05-02'),
              val=('2025-05-08', '2026-04-30'))
DEVIATIONS = [
    '验证加前向由两段独立起始的期记录拼接，保留段间空档和各段末期收盘估值，不跨空档建仓；主判定分量为两段算术和。',
    '末期使用副本的最终收盘估值；T 为进场日到下一进场日的日数，末期为到最终日的日数。末期资金费仍包含最终日，严格沿用副本。',
    '未变目标继续持有原份额；缺失开盘时保留旧仓并缩放可交易仓位，资金费基数也逐项照副本更新。逐币实际数量、资金费基数和交易成本用于独立重建逐日净值。',
    '描述性等权分解、IC 和波动分组剔除缺失进场或出场原始价格的币期并计数，不用填充价格制造收益。实际净值核对保留这些仓位并沿用副本的前填估值。',
    '波动五分组先在信号日可用且分数和波动率有限的币中排序，再以 array_split 分成五个尽量等大的组；并列按面板列序，余数优先低波动组。分组不使用未来价格。',
    '波动描述和 OLS 使用全部可用币期，包含中间三组；CSV 的 neutral 表示未入多空腿。资金费列 funding 为正费率累计，F 为有方向的腿收益，neutral 的 F 为零。',
    '逐信号 IC 使用双方有限交集、平均秩和至少两个非恒定观测；Pearson 的 score 秩映射在信号日可用币上先计算，再筛掉缺失收益。t 使用样本标准差且不作 HAC。',
    '尾部份额按每期腿内等权贡献计算，保留分母符号；分母为零时无定义。背离比较使用逐期五分位价差的算术和，不含成本与资金费。',
    '复利同时列单位腿的价格及含资金费收益，以及验证、前向原 Backtrader 的半仓独立腿净收益；后者用于与 ML 报告核对。训练窗口内部分币尚无 K 线，既有加载器拒绝空表，训练仅列单位腿复利。',
    '按任务仅新增两个源码文件，结果仅写指定输出目录；本研究不计 K，不写账本及研究日志。',
]


def decomposition(returns, leg, funding):
    """Return signed per-unit-leg P/L/C/F for equally weighted observations."""
    r = np.asarray(returns, dtype=float)
    f = np.asarray(funding, dtype=float)
    if leg not in ('long', 'short') or not len(r):
        raise ValueError('a nonempty long or short leg is required')
    if not np.isfinite(r).all() or np.any(r <= -1) or not np.isfinite(f).all():
        raise ValueError('finite returns above -1 and finite funding required')
    sign = 1. if leg == 'long' else -1.
    log = np.log1p(r)
    return dict(P=float(sign * r.mean()), L=float(sign * log.mean()),
                C=float(sign * (r - log).mean()), F=float(-sign * f.mean()))


def verdict(p, log, convexity):
    rho = convexity / p if p < 0 else None
    if p >= 0 or rho < 1 / 3:
        label = '机制不成立'
    elif log >= 0:
        label = '机制成立（强）'
    else:
        label = '机制部分成立'
    return dict(rho=rho, verdict=label)


def ex_ante_vol(close):
    """Use 30 daily log returns ending at t, without filling missing prices."""
    logs = np.log(close.where(close > 0))
    return logs.diff().rolling(30, min_periods=20).std(ddof=1)


def quintiles(values, eligible):
    """Stable ascending groups, with any remainder assigned to lower groups."""
    values = np.asarray(values, dtype=float)
    names = np.flatnonzero(np.asarray(eligible, bool) & np.isfinite(values))
    result = np.zeros(len(values), dtype=int)
    ordered = names[np.argsort(values[names], kind='stable')]
    for group, members in enumerate(np.array_split(ordered, 5), 1):
        result[members] = group
    return result


def tail_share(rows):
    selected = rows.loc[rows.leg.eq('short') & rows.included].copy()
    if selected.empty:
        return dict(n=0, tail_n=0, C_share=None, P_share=None)
    count = selected.groupby(['segment', 'period']).R.transform('size')
    p = -selected.R / count
    c = -selected.c / count
    tail = selected.R.gt(.30)
    return dict(n=len(selected), tail_n=int(tail.sum()),
                C_share=float(c[tail].sum() / c.sum()) if c.sum() else None,
                P_share=float(p[tail].sum() / p.sum()) if p.sum() else None,
                tail_C=float(c[tail].sum()), total_C=float(c.sum()),
                tail_P=float(p[tail].sum()), total_P=float(p.sum()))


def _pearson(a, b):
    a, b = np.asarray(a, float), np.asarray(b, float)
    if len(a) < 2:
        return np.nan
    a, b = a - a.mean(), b - b.mean()
    denominator = np.linalg.norm(a) * np.linalg.norm(b)
    return float(a @ b / denominator) if denominator else np.nan


def signal_comparison(rows):
    """Compute one cross-sectional observation on a common price sample."""
    visible = rows.loc[rows.eligible & rows.included]
    sr = _pearson(visible.score.rank(method='average'), visible.R.rank(method='average'))
    sl = _pearson(visible.score.rank(method='average'), visible.l.rank(method='average'))
    np.testing.assert_allclose(sr, sl, atol=1e-12, rtol=0, equal_nan=True)
    long, short = visible.loc[visible.leg.eq('long')], visible.loc[visible.leg.eq('short')]
    return dict(spearman_R=sr, spearman_l=sl,
                pearson_rank_R=_pearson(visible.rank_score, visible.R),
                pearson_rank_l=_pearson(visible.rank_score, visible.l),
                spread_R=float(long.R.mean() - short.R.mean()),
                spread_l=float(long.l.mean() - short.l.mean()))


def build_periods(panels, score, start, end, segment, arm, sigma=None):
    """Account at open-to-open boundaries and retain each coin's fixed shares.

    The period NAV recursion uses exit marks. Daily marks are reconstructed
    separately from the exported quantities, never from the reference equity.
    """
    index = score.loc[start:end].index
    if not index.equals(pd.date_range(start, end, freq='D')):
        raise ValueError('a complete daily crypto calendar is required')
    columns = score.columns
    opens = panels.fields['open'].reindex(index=index, columns=columns)
    closes = panels.fields['close'].reindex(index=index, columns=columns)
    open_marks = opens.ffill().fillna(1.).to_numpy()
    close_marks = closes.ffill().fillna(1.).to_numpy()
    funding = panels.fields['funding'].reindex(index=index, columns=columns).fillna(0.)
    rates = funding.to_numpy() * 3
    scores = score.reindex(index).to_numpy()
    masks = panels.mask.reindex(index=index, columns=columns).to_numpy()
    sigma = ex_ante_vol(panels.fields['close']) if sigma is None else sigma
    vols = sigma.reindex(index=index, columns=columns).to_numpy()
    positions = study.signal_positions(index, start, H)
    quantities = np.zeros(len(columns))
    entry_nominal = quantities.copy()
    previous = quantities.copy()
    nav = float(study.q.CAPITAL)
    records, period_records = [], []
    counts = dict(missing_execution_path_events=0, stale_position_periods=0,
                  excluded_eligible_coin_periods=0, excluded_selected_coin_periods=0,
                  signals_without_entry=0, unchanged_target_periods=0)
    accrual_cache = {}
    for j, signal in enumerate(positions):
        entry = signal + 1
        if entry >= len(index):
            counts['signals_without_entry'] += 1
            continue
        stop = min(positions[j + 1] + 1, len(index)) if j + 1 < len(positions) else len(index)
        final = stop == len(index)
        prices = open_marks[entry]
        exit_prices = close_marks[-1] if final else open_marks[stop]
        available = opens.iloc[entry].notna().to_numpy()
        eligible = masks[signal] & np.isfinite(scores[signal])
        desired = study.rank_weights(scores[signal], masks[signal])
        change = bool(np.any(desired != previous))
        previous = desired.copy()
        old_quantity = quantities.copy()
        keep = ~available
        missing_intent = keep & ((desired != 0) | (np.abs(quantities) > 1e-12))
        counts['missing_execution_path_events'] += int(change and missing_intent.any())
        counts['stale_position_periods'] += int((keep & (np.abs(quantities) > 1e-12)).sum())
        counts['unchanged_target_periods'] += int(not change)
        if change:
            feasible = desired.copy()
            if keep.any():
                feasible[keep] = quantities[keep] * prices[keep] / nav
                gross = np.abs(desired[~keep]).sum()
                budget = max(np.abs(desired).sum() - np.abs(feasible[keep]).sum(), 0.)
                scale = budget / gross if gross > 1e-12 else 0.
                feasible[~keep] = desired[~keep] * scale
            quantities = feasible * nav / prices
            entry_nominal = quantities * prices
        costs = study.ONE_WAY_COST * np.abs((quantities - old_quantity) * prices)
        length = stop - entry
        if length not in accrual_cache:
            accrual_cache[length] = study.holding_funding(funding, length).to_numpy()
        accrued = accrual_cache[length][signal]
        np.testing.assert_allclose(accrued, rates[entry:stop].sum(axis=0), atol=1e-12, rtol=0)
        r = exit_prices / prices - 1
        log = np.log1p(r)
        c = r - log
        observed_exit = closes.iloc[-1].notna().to_numpy() if final else opens.iloc[stop].notna().to_numpy()
        included = available & observed_exit & np.isfinite(r) & (r > -1)
        counts['excluded_eligible_coin_periods'] += int((eligible & ~included).sum())
        counts['excluded_selected_coin_periods'] += int(((desired != 0) & ~included).sum())
        groups = quintiles(vols[signal], eligible)
        rank_score = np.full(len(columns), np.nan)
        n = int(eligible.sum())
        if n:
            ranks = pd.Series(scores[signal, eligible]).rank(method='average').to_numpy()
            rank_score[eligible] = ((ranks - .5) / n - .5) * math.sqrt(12)
        t_days = (index[-1] - index[entry]).days if final else (index[stop] - index[entry]).days
        price_pnl = quantities * (exit_prices - prices)
        funding_pnl = -entry_nominal * accrued
        next_nav = nav + float(price_pnl.sum() + funding_pnl.sum() - costs.sum())
        period = study.day(index[signal])
        for k, coin in enumerate(columns):
            leg = 'long' if desired[k] > 0 else 'short' if desired[k] < 0 else 'neutral'
            sign = float(np.sign(desired[k]))
            records.append(dict(arm=arm, segment=segment, period=period, coin=coin, leg=leg,
                signal_date=period, entry_date=study.day(index[entry]),
                exit_date=study.day(index[-1] if final else index[stop]), final_close=final,
                holding_days=t_days, R=float(r[k]), l=float(log[k]), c=float(c[k]),
                funding=float(accrued[k]), F=float(-sign * accrued[k]), sigma_hat=float(vols[signal, k]),
                predicted_convexity=float(vols[signal, k] ** 2 * t_days / 2), vol_quintile=int(groups[k]),
                score=float(scores[signal, k]), rank_score=float(rank_score[k]), eligible=bool(eligible[k]),
                included=bool(included[k]), entry_available=bool(available[k]), exit_available=bool(observed_exit[k]),
                quantity=float(quantities[k]), previous_quantity=float(old_quantity[k]),
                entry_price=float(prices[k]), exit_price=float(exit_prices[k]),
                funding_basis=float(entry_nominal[k]), cost=float(costs[k]),
                price_pnl=float(price_pnl[k]), funding_pnl=float(funding_pnl[k]), entry_nav=nav))
        period_records.append(dict(period=period, entry=entry, stop=stop, entry_nav=nav,
                                   exit_nav=next_nav, net_return=next_nav / nav - 1,
                                   cost=float(costs.sum()), target_changed=change))
        nav = next_nav
    return pd.DataFrame(records), period_records, counts


def reconstruct_daily(panels, rows, periods, index):
    """Rebuild all daily marks exclusively from coin-period accounting fields."""
    equity = pd.Series(float(study.q.CAPITAL), index=index)
    columns = panels.mask.columns
    close = panels.fields['close'].reindex(index=index, columns=columns).ffill().fillna(1.)
    rates = panels.fields['funding'].reindex(index=index, columns=columns).fillna(0.) * 3
    by_period = {key: group.set_index('coin').reindex(columns) for key, group in rows.groupby('period')}
    for period in periods:
        group = by_period[period['period']]
        cash = period['entry_nav'] - float((group.quantity * group.entry_price).sum()) - float(group.cost.sum())
        for i in range(period['entry'], period['stop']):
            cash -= float(group.funding_basis.to_numpy() @ rates.iloc[i].to_numpy())
            equity.iloc[i] = cash + float(group.quantity.to_numpy() @ close.iloc[i].to_numpy())
    return equity


def reconcile(panels, score, start, end, rows, periods, counts):
    reference = study.vectorized_replica(panels, score, start, end, H)
    equity = reconstruct_daily(panels, rows, periods, score.loc[start:end].index)
    daily = equity / equity.shift(1, fill_value=study.q.CAPITAL) - 1
    error = float((daily - reference['main_daily_returns']).abs().max())
    ref_equity = pd.Series(reference['daily_equity'])
    ref_equity.index = pd.to_datetime(ref_equity.index)
    total = float(equity.iloc[-1] / study.q.CAPITAL - 1)
    period_total = float(np.prod([1 + p['net_return'] for p in periods]) - 1)
    np.testing.assert_allclose(daily, reference['main_daily_returns'], atol=1e-9, rtol=0)
    np.testing.assert_allclose([total, period_total], reference['main_total_return'], atol=1e-9, rtol=0)
    assert counts['missing_execution_path_events'] == reference['missing_execution_path_events']
    return dict(main_total_return=reference['main_total_return'], reconstructed_total_return=total,
                compounded_period_return=period_total, max_abs_daily_return_error=error,
                max_abs_equity_error=float((equity - ref_equity).abs().max()),
                total_return_error=abs(total - reference['main_total_return']),
                missing_funding_cells=reference['missing_funding_cells'],
                holding_funding_check_error=reference['holding_funding_check_error'],
                daily_equity={study.day(d): float(v) for d, v in equity.items()},
                periods=periods)


def describe(rows):
    legs = {}
    for leg in ('long', 'short'):
        selected = rows.loc[rows.leg.eq(leg) & rows.included]
        components = [decomposition(g.R, leg, g.funding)
                      for _, g in selected.groupby(['segment', 'period'], sort=False)]
        totals = {key: float(sum(p[key] for p in components)) for key in ('P', 'L', 'C', 'F')}
        totals.update(n_periods=len(components), n_coin_periods=len(selected),
                      compounded_price=float(np.prod([1 + p['P'] for p in components]) - 1),
                      compounded_price_funding=float(np.prod([1 + p['P'] + p['F'] for p in components]) - 1))
        np.testing.assert_allclose(totals['P'], totals['L'] + totals['C'], atol=1e-12, rtol=1e-12)
        legs[leg] = totals
    daily = []
    for (segment, period), group in rows.groupby(['segment', 'period'], sort=False):
        daily.append(dict(segment=segment, period=period, **signal_comparison(group)))
    metrics = ('spearman_R', 'spearman_l', 'pearson_rank_R', 'pearson_rank_l', 'spread_R', 'spread_l')
    ic = {key: ml.ic_summary(pd.Series([d[key] for d in daily], dtype=float)) for key in metrics}
    vol_rows = rows.loc[rows.eligible & rows.included & rows.vol_quintile.gt(0)]
    vol = []
    for group in range(1, 6):
        selected = vol_rows.loc[vol_rows.vol_quintile.eq(group)]
        vol.append(dict(quintile=group, n=len(selected), mean_R=float(selected.R.mean()),
                        mean_l=float(selected.l.mean()), mean_c=float(selected.c.mean()),
                        predicted_convexity=float(selected.predicted_convexity.mean()),
                        mean_funding=float(selected.funding.mean()),
                        short_share=float(selected.leg.eq('short').mean())))
    regression = dict(n=len(vol_rows), slope=None, intercept=None, r2=None)
    if len(vol_rows) >= 2 and vol_rows.predicted_convexity.var() > 0:
        x = np.column_stack([np.ones(len(vol_rows)), vol_rows.predicted_convexity])
        y = vol_rows.c.to_numpy()
        beta = np.linalg.lstsq(x, y, rcond=None)[0]
        sst = float(np.sum((y - y.mean()) ** 2))
        regression.update(intercept=float(beta[0]), slope=float(beta[1]),
                          r2=float(1 - np.sum((y - x @ beta) ** 2) / sst) if sst else None)
    signs = pd.DataFrame(daily)[['spread_R', 'spread_l']].dropna()
    arithmetic, log = float(signs.spread_R.sum()), float(signs.spread_l.sum())
    return dict(legs=legs, net_convexity=.5 * (legs['long']['C'] + legs['short']['C']),
                **verdict(legs['short']['P'], legs['short']['L'], legs['short']['C']),
                ic=ic, signal_statistics=daily, vol_quintiles=vol, ols=regression,
                tail=tail_share(rows),
                sign_check=dict(arithmetic_ls=arithmetic, log_ls=log, n=len(signs),
                                same_sign=bool(np.sign(arithmetic) == np.sign(log)),
                                matching_signal_signs=int((np.sign(signs.spread_R) == np.sign(signs.spread_l)).sum())))


def rebuild_r(panels, factors, report, progress=print):
    model = report['crypto']['models']['R']
    lam = float(model['chosen_lambda'])
    cube, names, _, _ = ml.feature_panels(panels, factors, progress)
    labels = ml.rank_map(panel.forward_target(panels, H)[0], panels.mask, label=True)
    splits = panel.make_splits(panels.mask.index, H, panels.windows)
    x, y, dates = ml.training_sample(cube, labels, panels.mask, splits['train'])
    # Reuse the original two-RHS spectral fit without any lambda selection.
    targets = np.column_stack([y, ml.permute_labels(y, dates)])
    gram, rhs = ml.gram(x, targets)
    beta = ml.spectral_ridge(gram, rhs, len(x), [lam])[:, 0, 0]
    score = ml.score_panel(cube, panels.mask, beta)
    forward = panel.forward_labels(panels, H, study.FORWARD_START)[0]
    checks = {}
    for segment, target, start, end in [('val', labels, *RANGES['val']),
                                       ('forward', forward, study.FORWARD_START, study.day(panels.mask.index[-1]))]:
        visible = (panels.mask.index >= start) & (panels.mask.index <= end)
        actual = ml.panel_ic(score, target, panels.mask, visible)
        expected = model['ic'][segment]['mean']
        checks[segment] = dict(actual=actual, expected=expected,
                               abs_error=abs(actual['mean'] - expected) if actual['mean'] is not None else None)
    result = dict(chosen_lambda=lam, n_train=len(x), n_train_dates=len(np.unique(dates)),
                  feature_names=names, ic_checks=checks,
                  available=all(v['abs_error'] is not None and v['abs_error'] <= 1e-9 for v in checks.values()))
    try:
        for check in checks.values():
            if check['actual']['mean'] is None:
                raise AssertionError('no finite reproduction IC')
            np.testing.assert_allclose(check['actual']['mean'], check['expected'], atol=1e-9, rtol=0)
    except AssertionError:
        result['reason'] = '冻结 R 的验证或前向 IC 未能在 1e-9 内复现，停止报告 R。'
        return None, result
    return score, result


def validate_out(root, out):
    root, out = Path(root).resolve(), Path(out).resolve()
    parent = root.parent if root.name == 'crypto' else root
    paths = [out, *(out / name for name in ('report.json', 'report.md', 'periods.csv'))]
    study.validate_outputs(parent, paths)
    return parent, out


def _number(value):
    return '—' if value is None else f'{value:.10g}'


def render_report(result):
    table, fmt = study.table, _number
    lines = ['# fq_crypto 空头腿凸性分解', '',
             f'冻结预注册 `{PREREG}`。本研究仅描述，不计 K。所有收益分量均为小数，按单位腿名义逐期算术求和。', '',
             '## 主判定（验证加前向）', '']
    for arm, segments in result['arms'].items():
        combined = segments['val_forward']
        short = combined['legs']['short']
        lines += [f"{arm}　**{combined['verdict']}**　ρ={fmt(combined['rho'])}，"
                  f"P_S={fmt(short['P'])}，L_S={fmt(short['L'])}，C_S={fmt(short['C'])}，F_S={fmt(short['F'])}。", '']
    reproduction = result['R_reproduction']
    lines += [f"R 冻结模型复现 {'通过' if reproduction['available'] else '失败，R 不可用'}，"
              f"λ={fmt(reproduction['chosen_lambda'])}，训练币期={reproduction['n_train']}。", '',
              table(['分段', '重建 IC', 'ML 报告 IC', '绝对误差'],
                    [[seg, fmt(c['actual']['mean']), fmt(c['expected']), fmt(c['abs_error'])]
                     for seg, c in reproduction['ic_checks'].items()]), '',
              '## 分段两腿分量和复利', '',
              table(['臂', '分段', '腿', '期数', 'P', 'L', 'C', 'F', '价格复利', '价格及资金费复利', 'Backtrader 半仓净复利'],
                    [[arm, seg, leg, v['n_periods'], *[fmt(v[k]) for k in ('P', 'L', 'C', 'F', 'compounded_price', 'compounded_price_funding')],
                      fmt(s.get('native_legs', {}).get(leg))]
                     for arm, segments in result['arms'].items() for seg, s in segments.items()
                     for leg, v in s['legs'].items()]), '',
              '单位腿复利为 ∏(1+P)−1 和 ∏(1+P+F)−1，均未扣交易成本。'
              'Backtrader 列复用 ML 的独立半仓腿回测，计手续费、滑点和其原生资金费时序。', '',
              '## 净凸性、尾部和背离', '',
              table(['臂', '分段', '净凸性', '尾部 C_S 份额', '尾部 P_S 份额', '尾部币期/空头币期', '算术 L/S', '对数 L/S', '同号'],
                    [[arm, seg, fmt(s['net_convexity']), fmt(s['tail']['C_share']), fmt(s['tail']['P_share']),
                      f"{s['tail']['tail_n']}/{s['tail']['n']}", fmt(s['sign_check']['arithmetic_ls']),
                      fmt(s['sign_check']['log_ls']), '是' if s['sign_check']['same_sign'] else '否']
                     for arm, segments in result['arms'].items() for seg, s in segments.items()]), '',
              '## 与副本的逐日及逐期核对', '',
              table(['臂', '分段', '副本总收益', '重建总收益', '逐期复利', '最大日收益误差', '缺失执行事件', '剔除可用币期/入腿币期'],
                    [[arm, seg, fmt(s['reconciliation']['main_total_return']), fmt(s['reconciliation']['reconstructed_total_return']),
                      fmt(s['reconciliation']['compounded_period_return']), fmt(s['reconciliation']['max_abs_daily_return_error']),
                      s['availability']['missing_execution_path_events'],
                      f"{s['availability']['excluded_eligible_coin_periods']}/{s['availability']['excluded_selected_coin_periods']}"]
                     for arm, segments in result['arms'].items() for seg, s in segments.items() if seg != 'val_forward']), '',
              '逐日由 CSV 的实际固定份额、进场 NAV、资金费基数和 8bp 交易成本重建。逐期收益使用下一开盘估值，末期用最终收盘。'
              '验证加前向的净值由两段收益相乘，分量则相加。', '',
              '## IC 与五分位价差（日均及 t）', '',
              table(['臂', '分段', '指标', '均值', 't', '信号数'],
                    [[arm, seg, metric, fmt(v['mean']), fmt(v['t']), v['n']]
                     for arm, segments in result['arms'].items() for seg, s in segments.items() for metric, v in s['ic'].items()]), '',
              '每个可进场的三日信号为一个观测。Spearman 两种收益逐截面断言相等，容差 1e-12。', '',
              '## 事前波动五分组', '',
              table(['臂', '分段', '组', '币期', 'mean R', 'mean l', 'mean c', 'σ̂²T/2', '累计资金费率', '空头比例'],
                    [[arm, seg, v['quintile'], v['n'], *[fmt(v[k]) for k in ('mean_R', 'mean_l', 'mean_c', 'predicted_convexity', 'mean_funding', 'short_share')]]
                     for arm, segments in result['arms'].items() for seg, s in segments.items() for v in s['vol_quintiles']]), '',
              '## 合并币期 OLS', '',
              table(['臂', '分段', '币期', '斜率', '截距', 'R²'],
                    [[arm, seg, s['ols']['n'], fmt(s['ols']['slope']), fmt(s['ols']['intercept']), fmt(s['ols']['r2'])]
                     for arm, segments in result['arms'].items() for seg, s in segments.items()]), '',
              '## ML 分腿原结果核对', '',
              table(['分段', '腿', '本次 Backtrader', 'ML 报告', '差值'],
                    [[seg, leg, fmt(v['actual']), fmt(v['expected']), fmt(v['delta'])]
                     for seg, legs in result['ML_leg_comparison'].items() for leg, v in legs.items()]), '',
              '## 歧义处理与偏离', '', *['- ' + d for d in result['deviations']], '',
              '## 局限与证据边界', '',
              '验证和前向已被多项研究看过，训练只作描述。快照币池存在幸存者偏差。'
              'P=L+C 是会计恒等式，凸性来自已实现路径，描述性回归和 t 值不证明因果，'
              '币期有重复个体且未作独立性修正。本结果不构成样本外有效、增量筛选或模拟验证证据。', '',
              f"本次运行序号 {result['attempt']}，输入文件 {result['file_count']} 个。输入哈希在运行前后复查。", '']
    return '\n'.join(lines)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--ml-report', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--allow-input-drift', action='store_true')
    args = parser.parse_args(argv)
    root, out = validate_out(args.root, args.out)
    ml.guard_features()
    hashes, count = ml.input_hashes(root, 'crypto')
    mismatches = ml.guard_hashes(hashes, args.allow_input_drift)
    report_bytes = args.ml_report.read_bytes()
    reference = json.loads(report_bytes)
    frozen, _, provenance = study.load_frozen(root)
    if len(frozen[H]) != 7:
        raise ValueError('the frozen h=3 ensemble must contain seven factors')
    panels = panel.load_panels('crypto', root)
    out.mkdir(parents=True, exist_ok=True)
    report_path = out / 'report.json'
    attempt = json.loads(report_path.read_text()).get('attempt', 0) + 1 if report_path.exists() else 1
    started = time.monotonic()
    result = dict(status='running', attempt=attempt, prereg=PREREG, hashes=hashes,
                  hash_mismatches=mismatches, feature_hash=ml.FEATURE_HASH, file_count=count,
                  ml_report_sha256=hashlib.sha256(report_bytes).hexdigest(), provenance=provenance,
                  generated_at=datetime.now(timezone.utc).isoformat(), deviations=list(DEVIATIONS),
                  counts_toward_k=False, command='python -m scripts.factor_mining.shortleg_convexity_study ' +
                  ' '.join(sys.argv[1:] if argv is None else map(str, argv)))
    report_path.write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    if mismatches:
        result['deviations'].append('--allow-input-drift 放行输入漂移 ' + json.dumps(mismatches))
    print('输入哈希已核对，重建冻结 R，不重跑交叉验证。', flush=True)
    score_r, result['R_reproduction'] = rebuild_r(panels, frozen[H], reference, lambda msg: print(msg, flush=True))
    scores = dict(B=ensemble.ensemble_scores(frozen[H], panels))
    if score_r is not None:
        scores['R'] = score_r
    else:
        result['deviations'].append(result['R_reproduction']['reason'])
    ranges = dict(RANGES, forward=(study.FORWARD_START, study.day(panels.mask.index[-1])))
    sigma = ex_ante_vol(panels.fields['close'])
    result.update(arms={}, panel_meta=panels.meta, ranges=ranges, ML_leg_comparison={})
    all_rows = []
    for arm, score in scores.items():
        segments, arm_rows = {}, []
        for segment, (start, end) in ranges.items():
            print(f'{arm}/{segment} 逐币分解与逐日核对。', flush=True)
            rows, periods, counts = build_periods(panels, score, start, end, segment, arm, sigma)
            summary = describe(rows)
            summary['availability'] = counts
            summary['reconciliation'] = reconcile(panels, score, start, end, rows, periods, counts)
            if segment != 'train':
                targets = study.build_targets(score.loc[:end], panels.mask.loc[:end], start, H).loc[start:end]
                native = ml.leg_backtests(panels, targets, start, end)
                summary['native_legs'] = {leg: run['backtest']['metrics']['total_return'] for leg, run in native.items()}
            if arm == 'R' and segment in ('val', 'forward'):
                old = reference['crypto']['segments'][segment]['arms']['R']['legs']
                result['ML_leg_comparison'][segment] = {
                    leg: dict(actual=value, expected=old[leg]['backtest']['metrics']['total_return'],
                              delta=value - old[leg]['backtest']['metrics']['total_return'])
                    for leg, value in summary['native_legs'].items()}
            segments[segment] = summary
            arm_rows.append(rows)
        combined = pd.concat([r for r in arm_rows if r.segment.iloc[0] != 'train'], ignore_index=True)
        segments['val_forward'] = describe(combined)
        segments['val_forward']['native_legs'] = {
            leg: (1 + segments['val']['native_legs'][leg]) * (1 + segments['forward']['native_legs'][leg]) - 1
            for leg in ('long', 'short')}
        result['arms'][arm] = segments
        all_rows.extend(arm_rows)
    if ml.input_hashes(root, 'crypto')[0] != hashes or args.ml_report.read_bytes() != report_bytes:
        raise ValueError('inputs changed during study; refusing to publish')
    result.update(status='complete', elapsed_seconds=time.monotonic() - started)
    result = study.json_safe(result)
    pd.concat(all_rows, ignore_index=True).to_csv(out / 'periods.csv', index=False)
    report_path.write_text(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + '\n', encoding='utf-8')
    (out / 'report.md').write_text(render_report(result), encoding='utf-8')
    for arm, segments in result['arms'].items():
        v = segments['val_forward']
        print(f"{arm} {v['verdict']} rho={v['rho']} short={v['legs']['short']}", flush=True)
    print(f'报告 {out / "report.md"}', flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
