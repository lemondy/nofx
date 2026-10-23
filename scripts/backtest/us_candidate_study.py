"""Preregistered offline candidate research; portfolios use native stocks Cerebro.

Run --stage select before --stage evaluate. The latter opens holdout once and
saves raw native runs; --stage report can re-render without reopening labels.
"""
import argparse
from dataclasses import asdict
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

from .stocks_cache import BASE, output_root
from .stocks_data import SPY, candidate_basket, candidate_state, load_study

VARIANTS = (('V0', 5, 'NONE'), ('V1', 10, 'NONE'), ('V2', 15, 'NONE'),
            ('V3', 5, 'T1'), ('V4', 5, 'T2'), ('V5', 10, 'T1'), ('V6', 10, 'T2'))
SPLITS = {'train': ('2020-01-01', '2023-12-31'),
          'validation': ('2024-01-01', '2024-12-31'),
          'oos': ('2025-01-01', '2099-12-31')}
HORIZONS = (5, 10, 20)
COST_LOG = math.log(1-.001)-math.log(1+.001)
PROTOCOL = dict(variants=VARIANTS, splits=SPLITS, horizons=HORIZONS,
    momentum='close[t-21]/close[t-252]-1; own observed daily bars; close>SMA200',
    states='up: close>EMA20>EMA50; down: close<EMA20<EMA50; else transition',
    ema='adjust=False; first-close seed; min_periods=span; own observed bars',
    filters={'T1': 'close>SMA200 and close>EMA50', 'T2': 'close>SMA200 and up'},
    filter_order='filter entire eligible momentum universe, then take N',
    pool_quality='equal-date mean of constituent log returns; require all selected labels; empty pool omitted',
    class_quality='pooled stock-date observations within unfiltered top15; date-cluster sandwich SE',
    label='close[t+k]/close[t] on SPY calendar; no fill; endpoint within same split',
    cost='log((1-.001)/(1+.001)); every signal assumes a standalone round trip',
    basket='chain-linked index of daily equal-weight observed stock simple returns; no fill; no orders; EMA50',
    selection='highest train+validation paired-date 10d net top5 mean versus V0; tie T1; OOS not consulted',
    inference='2000 whole-date bootstrap; cluster SE; native daily difference circular moving-block bootstrap L10 B2000',
    regime_timing='signal: state at t close; portfolio return t: state at previous SPY close',
    seed=20261023, bootstrap=2000, block=10, rebalance=10,
    cash=100000, commission=.001, slippage=0,
    warning='Current bStock universe: severe survivorship/selection bias, including relative comparisons.')


def save(path, value):
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False, allow_nan=False))


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def close_panel(study):
    calendar = study.daily[SPY].frame.index
    return pd.DataFrame({s: h.frame['close'].reindex(calendar)
                         for s, h in study.daily.items() if s != SPY}, index=calendar)


def market_state(close):
    """Descriptive basket index only, no orders, shares, cash, or P&L ledger."""
    returns = close.pct_change(fill_method=None).mean(axis=1)
    index = (1+returns.fillna(0)).cumprod()
    ema = index.ewm(span=50, adjust=False, min_periods=50).mean()
    state = pd.Series('unavailable', index=close.index)
    state.loc[index < ema] = 'drawdown'
    state.loc[index >= ema] = 'normal'
    return pd.DataFrame(dict(basket_index=index, ema50=ema, state=state))


def forward_labels(close, horizon, start, end):
    """t to t+k SPY closes, never beyond data or the split boundary."""
    if horizon <= 0:
        raise ValueError('horizon must be positive')
    positions = np.arange(max(0, len(close)-horizon))
    positions = positions[(close.index[positions] >= pd.Timestamp(start)) &
                          (close.index[positions+horizon] <= pd.Timestamp(end))]
    labels = pd.DataFrame(np.nan, index=close.index, columns=close.columns)
    # Slice before division: selection never even computes OOS return labels.
    labels.iloc[positions] = np.log(close.iloc[positions+horizon].to_numpy()/
                                   close.iloc[positions].to_numpy())
    return labels


def memberships(study):
    classes, pools = [], []
    for t in study.calendar:
        for s in candidate_basket(study, t, 15):
            h = study.daily[s]
            classes.append((t, s, candidate_state(h, h.index[t])))
        for variant, n, trend_filter in VARIANTS:
            names = candidate_basket(study, t, n, trend_filter)
            for s in names:
                pools.append((t, s, variant))
    return (pd.DataFrame(classes, columns=['date', 'symbol', 'group']),
            pd.DataFrame(pools, columns=['date', 'symbol', 'group']))


def observations(members, labels, pool=False):
    """Return date x group sums/counts, preserving missing endpoint diagnostics."""
    long = labels.rename_axis(index='date', columns='symbol').stack().rename('value')
    rows = members.join(long, on=['date', 'symbol'])
    grouped = rows.groupby(['date', 'group'])['value']
    agg = grouped.agg(['sum', 'count', 'size'])
    if pool:
        # Missing future price never silently promotes a lower-ranked name.
        good = (agg['count'] == agg['size']) & agg['count'].gt(0)
        sums = (agg['sum']/agg['count']).where(good).unstack('group')
        counts = good.astype(float).unstack('group').fillna(0)
    else:
        sums = agg['sum'].unstack('group')
        counts = agg['count'].unstack('group').fillna(0)
    missing = rows.groupby('group')['value'].agg(['size', 'count'])
    return sums.fillna(0), counts, missing


def estimate(sums, counts, weights=None, seed=PROTOCOL['seed']):
    """Intercept/group-mean sandwich clustered by date and whole-date bootstrap.

    Each column is a group; contrasts preserve contemporaneous covariance.
    """
    x, n = np.asarray(sums, float), np.asarray(counts, float)
    if x.ndim == 1:
        x, n = x[:, None], n[:, None]
    used = n.sum(axis=1) > 0
    x, n = x[used], n[used]
    days = len(x)
    w = np.ones(x.shape[1]) if weights is None else np.asarray(weights)
    total = n.sum(axis=0)
    if days < 2 or (total == 0).any():
        return dict(mean=None, se=None, normal_ci=[None, None], ci=[None, None], dates=days,
                    observations=total.astype(int).tolist())
    means = x.sum(axis=0)/total
    influence = ((x-n*means)/total) @ w
    se = math.sqrt(days/(days-1)*float(influence @ influence))
    point = float(means @ w)
    rng = np.random.default_rng(seed)
    samples = []
    for _ in range(PROTOCOL['bootstrap']//100):
        indices = rng.integers(0, days, (100, days))
        denominator = n[indices].sum(axis=1)
        values = np.divide(x[indices].sum(axis=1), denominator,
                           out=np.full_like(denominator, np.nan), where=denominator > 0) @ w
        samples.extend(values[np.isfinite(values)].tolist())
    return dict(mean=point, se=se, normal_ci=[point-1.96*se, point+1.96*se],
                ci=np.quantile(samples, [.025, .975]).tolist(), dates=days,
                observations=total.astype(int).tolist())


def pair_estimate(sums, counts, a, b):
    valid = counts[a].gt(0) & counts[b].gt(0)
    # Date-balanced paired differences; class contrasts use pooled means below.
    delta = sums.loc[valid, a]/counts.loc[valid, a]-sums.loc[valid, b]/counts.loc[valid, b]
    return estimate(delta, np.ones(len(delta)))


def select_filter(study, close, pools):
    parts = []
    for split in ('train', 'validation'):
        labels = forward_labels(close, 10, *SPLITS[split])
        sums, counts, _ = observations(pools, labels, pool=True)
        valid = counts[['V0', 'V3', 'V4']].gt(0).all(axis=1)
        parts.append(sums.loc[valid, ['V0', 'V3', 'V4']])
    paired = pd.concat(parts).sort_index()
    if paired.empty:
        raise ValueError('no common training/validation dates for T1/T2 selection')
    scores = {f: float((paired[v]-paired['V0']).mean()) for f, v in [('T1', 'V3'), ('T2', 'V4')]}
    return dict(selected=max(('T1', 'T2'), key=lambda f: scores[f]),
                paired_10d_net_difference=scores, common_dates=len(paired),
                first=str(paired.index[0]), last=str(paired.index[-1]),
                rule=PROTOCOL['selection'])


def signal_analysis(study, close, regime, classes, pools):
    results = []
    for split, bounds in SPLITS.items():
        for horizon in HORIZONS:
            labels = forward_labels(close, horizon, *bounds)
            for kind, members, is_pool in [('class', classes, False), ('pool', pools, True)]:
                sums, counts, missing = observations(members, labels, is_pool)
                for state in ('all', 'drawdown', 'normal'):
                    dates = sums.index if state == 'all' else sums.index[regime['state'].reindex(sums.index).eq(state)]
                    ss, nn = sums.loc[dates], counts.loc[dates]
                    for group in ss.columns:
                        est = estimate(ss[group], nn[group])
                        # Number of eligible member observations within this split/state,
                        # including censored endpoints, for an honest missing-label rate.
                        eligible = members['date'].between(pd.Timestamp(bounds[0]), pd.Timestamp(bounds[1]))
                        if state != 'all':
                            eligible &= members['date'].map(regime['state']).eq(state)
                        group_rows = members.loc[eligible & members['group'].eq(group)]
                        valid_rows = group_rows.join(labels.rename_axis(index='date', columns='symbol')
                            .stack().rename('value'), on=['date', 'symbol'])
                        est.update(eligible_observations=len(group_rows),
                                   missing_labels=int(valid_rows['value'].isna().sum()))
                        results.append(dict(split=split, horizon=horizon, state=state, kind=kind,
                            group=group, gross=est,
                            net=dict(mean=est['mean']+COST_LOG if est['mean'] is not None else None,
                                     ci=[v+COST_LOG if v is not None else None for v in est['ci']])))
                    if kind == 'class':
                        for c in ('down', 'up'):
                            if c not in ss:
                                ss[c], nn[c] = 0., 0.
                        contrast = estimate(ss[['down', 'up']], nn[['down', 'up']], [1, -1])
                        results.append(dict(split=split, horizon=horizon, state=state,
                            kind='class_difference', group='down-up', gross=contrast, net=contrast))
                    else:
                        for group in ('V1', 'V2', 'V3', 'V4', 'V5', 'V6'):
                            results.append(dict(split=split, horizon=horizon, state=state,
                                kind='pool_difference', group=group+'-V0',
                                gross=pair_estimate(ss, nn, group, 'V0')))
    return results


def block_difference(values, mask=None, seed=PROTOCOL['seed']):
    """Paired circular moving blocks on the ORIGINAL full trading-day axis.

    Regime-conditioned estimates resample return and mask together, retaining
    calendar gaps instead of concatenating drawdown episodes into adjacent days.
    """
    x = np.asarray(values, float)
    valid = np.isfinite(x)
    mask = valid if mask is None else (np.asarray(mask, bool) & valid)
    n, included = len(x), int(mask.sum())
    if not included:
        return dict(mean=None, ci=[None, None], dates=0)
    rng = np.random.default_rng(seed)
    b, length = PROTOCOL['bootstrap'], PROTOCOL['block']
    starts = rng.integers(0, n, (b, math.ceil(n/length)))
    indices = ((starts[..., None]+np.arange(length)) % n).reshape(b, -1)[:, :n]
    chosen = mask[indices]
    denominator = chosen.sum(axis=1)
    numerator = np.where(chosen, x[indices], 0.).sum(axis=1)
    boot = numerator[denominator > 0]/denominator[denominator > 0]
    return dict(mean=float(x[mask].mean()), ci=np.quantile(boot, [.025, .975]).tolist(),
                dates=included, block=length, replications=b)


def native_runs(study, out):
    from .stocks import Config, run_strategy
    results = {}
    for variant, n, trend_filter in VARIANTS + (('SPY_BH', 5, 'NONE'), ('EW_BH', 5, 'NONE')):
        print('Backtrader running '+variant, flush=True)
        cfg = Config(max_positions=n, candidate_filter=trend_filter)
        name = variant if variant in ('SPY_BH', 'EW_BH') else 'MOM_TOP'
        result = run_strategy(study, cfg, name)
        result['candidate_config'] = asdict(cfg)
        result['variant'] = variant
        save(out/(variant+'.json'), result)
        results[variant] = result
    return results


def portfolio_analysis(runs, regime):
    from .stocks_report import curve_risk_metrics
    frame = pd.DataFrame({v: pd.Series(r['analyzers']['time_return'], dtype=float)
                          for v, r in runs.items()})
    frame.index = pd.to_datetime(frame.index)
    frame = frame.sort_index().iloc[1:]  # common initial zero, no evaluation interval
    lagged = regime['state'].shift(1).reindex(frame.index)
    results = {}
    for variant, run in runs.items():
        m = dict(run['metrics'])
        curve = pd.DataFrame(run['equity_curve'])
        curve['time'] = pd.to_datetime(curve['time'])
        equity = curve.set_index('time')['equity']
        # Read-only turnover on native fills, marked by preceding native close.
        traded = 0.
        for fill in run['fills']:
            previous = equity.loc[equity.index < pd.Timestamp(fill['time'])]
            denominator = previous.iloc[-1] if len(previous) else equity.iloc[0]
            traded += abs(fill['size']*fill['price'])/denominator
        years = (equity.index[-1]-equity.index[0]).days/365.25
        m.update(fills=len(run['fills']), turnover_two_way=traded,
                 turnover_two_way_annual=traded/years)
        differences, periods = {}, {}
        for split, bounds in dict(full=('1900-01-01', '2099-12-31'), **SPLITS).items():
            rows = frame.loc[bounds[0]:bounds[1]]
            delta = rows[variant]-rows['V0']
            differences[split] = {state: block_difference(delta,
                None if state == 'all' else lagged.reindex(rows.index).eq(state))
                for state in ('all', 'drawdown', 'normal')}
            # Include previous native close as anchor, preserving boundary return.
            positions = np.flatnonzero(curve['time'].between(pd.Timestamp(bounds[0]), pd.Timestamp(bounds[1])))
            if len(positions) > 1:
                start, end = max(0, positions[0]-1), positions[-1]+1
                periods[split] = curve_risk_metrics(run['equity_curve'][start:end])
        results[variant] = dict(metrics=m, differences=differences, periods=periods,
            missing_open_orders=run['missing_open_orders'],
            pending_orders_at_end=run['pending_orders_at_end'])
    return results


def pct(value):
    return 'NA' if value is None else f'{value*100:.2f}%'


def interval(est, scale=100):
    if est['mean'] is None:
        return 'NA'
    return f"{est['mean']*scale:+.2f} [{est['ci'][0]*scale:+.2f}, {est['ci'][1]*scale:+.2f}]"


def findings(result):
    lookup = {(r['split'], r['state'], r['horizon'], r['kind'], r['group']): r
              for r in result['signals']}
    selected = result['selection']['selected']
    variant = 'V3' if selected == 'T1' else 'V4'
    signal = lookup[('oos', 'all', 10, 'pool_difference', variant+'-V0')]['gross']
    native = result['portfolios'][variant]['differences']['oos']
    improvement = native['all']['ci'][0] > 0 and signal['ci'][0] > 0
    positive_class_train = [h for h in HORIZONS if
        lookup[('train', 'all', h, 'class_difference', 'down-up')]['gross']['mean'] > 0]
    positive_class_val = [h for h in HORIZONS if
        lookup[('validation', 'all', h, 'class_difference', 'down-up')]['gross']['mean'] > 0]
    crossing = sum(lookup[('oos', state, h, 'class_difference', 'down-up')]['gross']['ci'][0] <= 0 <=
                   lookup[('oos', state, h, 'class_difference', 'down-up')]['gross']['ci'][1]
                   for state in ('all', 'drawdown') for h in HORIZONS)
    drawdown_days = lookup[('oos', 'drawdown', 10, 'class_difference', 'down-up')]['gross']['dates']
    chosen_score = result['selection']['paired_10d_net_difference'][selected]
    lines = ['## 主要发现与结论', '',
        f"训练+验证选中的 {selected} 在选择期相对V0的10日候选均值差为 {chosen_score*100:+.2f} 个百分点。样本外该定义相对V0的10日候选均值差为 {interval(signal)} 个百分点；组合样本外日收益差为 {interval(native['all'], 10000)} bp/日，回撤期为 {interval(native['drawdown'], 10000)} bp/日。样本外候选与组合区间{'均支持正差' if improvement else '未共同支持正差，改善证据不足'}。", '',
        '样本外趋势分类（毛对数收益；类别差扣费后相同）：', '',
        '| 状态 | 期限 | 下行均值 | 上升均值 | 下行−上升，百分点 [整日95%CI] |',
        '|---|---:|---:|---:|---:|']
    for state in ('all', 'drawdown'):
        for h in HORIZONS:
            down = lookup[('oos', state, h, 'class', 'down')]['gross']
            up = lookup[('oos', state, h, 'class', 'up')]['gross']
            diff = lookup[('oos', state, h, 'class_difference', 'down-up')]['gross']
            lines.append(f"| {state} | {h} | {pct(down['mean'])} | {pct(up['mean'])} | {interval(diff)} |")
    significant = []
    for v, _, _ in VARIANTS[1:]:
        if result['portfolios'][v]['differences']['full']['all']['ci'][1] < 0:
            significant.append(v)
    drawdown_negative = [v for v, _, _ in VARIANTS[1:] if
        result['portfolios'][v]['differences']['oos']['drawdown']['ci'][1] < 0]
    improved = [v for v, _, _ in VARIANTS[1:] if
        result['portfolios'][v]['differences']['oos']['all']['ci'][0] > 0]
    widened = '；'.join(f"{v} CAGR {pct(result['portfolios'][v]['metrics']['cagr'])}、"
                       f"Sharpe {result['portfolios'][v]['metrics']['sharpe']:.3f}"
                       for v in ('V1', 'V2'))
    lines += ['',
        f"训练期下行组相对上升组的正均值差期限为 {positive_class_train} 日，验证期为 {positive_class_val} 日；样本外六个“期限×全体/回撤”差异区间中 {crossing} 个跨零。样本外回撤期10日分类差覆盖 {drawdown_days} 个日历日期。不能把描述性的分类差解释为趋势状态的因果效应。", '',
        f"扩大N的全期结果：{widened}。须同时比较下表的波动、最大回撤和区间。全期平均日收益差区间完全低于零的变体为 {', '.join(significant) or '无'}；样本外回撤期区间完全低于零的变体为 {', '.join(drawdown_negative) or '无'}。上述为未校正多重比较的描述性证据。", '',
        f"样本外全体日收益差区间完全高于零的预注册变体为 {', '.join(improved) or '无，收益改善证据不足'}。{selected}是按预定规则选出的定义，不因样本外表现再次替换。部分变体回撤下降可以作为风险取舍观察，不能据此事后更换目标。候选扩大仍可能影响AI的可选机会，但本研究无法验证这种效应。", '']
    return lines


def render_report(result):
    selection, coverage = result['selection'], result['coverage']
    lines = ['# 美股候选池预注册研究', '',
        '**严重存活者/选择偏差：宇宙为当前已上线 bStock 的标的。绝对收益被高估；只有变体之间的相对比较可供参考，偏差也可能影响相对比较，不能外推为可交易收益。**', '',
        f"源数据：{result['data_start']} 至 {result['data_end']}；有效信号/组合：{coverage['first_signal_close']} 至 {coverage['last_mark_close']}，{coverage['evaluation_bars']} 个 SPY 交易日、{coverage['universe_size']} 只股票。2020 年没有有效动量信号；训练期不足四个完整年份。", '',
        f"预先选择：**{selection['selected']}**；训练+验证共同日期 {selection['common_dates']}，10 日扣费相对 V0 均值差：T1 {selection['paired_10d_net_difference']['T1']*100:+.2f} 个百分点，T2 {selection['paired_10d_net_difference']['T2']*100:+.2f} 个百分点。选择文件在打开样本外标签前写入。全部七种组合为固定预注册变体，无样本外调参。", '',
        *findings(result),
        '## 方法与可复核边界', '',
        '- 12–1 动量按每只股票已观察日线计算，要求 close>SMA200；EMA 为 adjust=False、首收盘种子、满 span 才可用。严格不等号，等号归过渡。T1 再要求 close>EMA50；T2 再要求 close>EMA20>EMA50。先过滤完整合格排名，再取 N，可能递补至原前15名以外。',
        '- 信号分类取原前15名；前瞻为 t 收盘至第 k 个后续 SPY 交易日收盘对数收益，不填补价格，不跨训练/验证/样本外边界。扣费项为 log((1−0.001)/(1+0.001))，约 −0.20 个百分点。分类统计为股票-日期等权；候选组合统计先逐日对候选均值、再日期等权，空池不计，不是持仓收益。任一候选缺未来价则该池该日标签弃用；缺失/边界删失计数见 JSON。',
        '- 大盘状态用每日有相邻收盘价股票的等权简单收益链式指数与自身 EMA50 比较；不按原始股价算术均值，以免名义价格和 IPO 改变篮子水平。无订单/账务。信号按 t 状态分层；组合 t 日收益按 t−1 收盘状态分层，避免用当日跌幅反向定义条件。',
        '- 信号报告按日期聚类的 sandwich SE（G/(G−1) 修正）、正态区间与整日自助法2000次百分位95%区间；表列 bootstrap 区间。down−up 是两类池化均值之差，保留同日协方差；候选池差用共同日期的成对差。重叠5/10/20日标签仍有跨日相关：日期聚类与独立整日 bootstrap 未解决这点，区间可能偏窄。',
        '- 组合完全调用现有 stocks.run_strategy 原生 Cerebro：10个SPY交易日调仓、1/maxpos、每边0.1%佣金、无滑点、次日开盘、小数股、现金/槽位缩量、期末盯市；不足N留现金。SPY_BH与EW_BH沿用现有适配（EW固定首日宇宙、63根再平衡）。成交笔数为原生成交记录数，完整平仓另列；双边换手=Σ|成交额|/前收盘权益，年化除实际年数。',
        '- 组合区间为相对V0的平均简单日收益差（bp/日），不是CAGR差：同日成对、原交易日轴圆形移动区块长10日、2000次；回撤期与状态掩码一起抽块，不把不同回撤段首尾拼接。所有区间未做多重比较校正，Sharpe无风险利率0。', '',
        '## A：趋势分类前瞻收益', '',
        '均值/整日95%区间单位为%，SE单位为百分点；净值列仅扣假设双边交易成本。', '',
        '| 时间段 | 状态 | k | 类别 | 观测数/日期数 | 毛均值 [95%CI] | 聚类SE | 净均值 |',
        '|---|---|---:|---|---:|---:|---:|---:|']
    for r in result['signals']:
        if r['kind'] not in ('class', 'class_difference'):
            continue
        e = r['gross']
        lines.append(f"| {r['split']} | {r['state']} | {r['horizon']} | {r['group']} | {sum(e['observations'])}/{e['dates']} | {interval(e)} | {pct(e['se'])} | {pct(r['net']['mean'])} |")
    lines += ['', '## A：候选池均值与成对差', '',
        'V0=top5，V1=top10，V2=top15，V3=T1 top5，V4=T2 top5，V5=T1 top10，V6=T2 top10。均值为候选标签统计，剩余现金未进入此表；现金效应由B测量。', '',
        '| 时间段 | 状态 | k | 池 | 有效日期 | 毛均值 [95%CI] % | 聚类SE | 净均值 % | 相对V0 [95%CI] % |',
        '|---|---|---:|---|---:|---:|---:|---:|---:|']
    lookup = {(r['split'], r['state'], r['horizon'], r['kind'], r['group']): r for r in result['signals']}
    for r in result['signals']:
        if r['kind'] != 'pool':
            continue
        delta = lookup.get((r['split'], r['state'], r['horizon'], 'pool_difference', r['group']+'-V0'))
        lines.append(f"| {r['split']} | {r['state']} | {r['horizon']} | {r['group']} | {r['gross']['dates']} | {interval(r['gross'])} | {pct(r['gross']['se'])} | {pct(r['net']['mean'])} | {interval(delta['gross']) if delta else '0'} |")
    lines += ['', '## B：原生 Backtrader 机械组合', '',
        '| 变体 | CAGR | 年化波动 | Sharpe | MaxDD | 平均暴露 | 成交/完整平仓 | 年化双边换手 | 佣金($) |',
        '|---|---:|---:|---:|---:|---:|---:|---:|---:|']
    for v, p in result['portfolios'].items():
        m = p['metrics']
        lines.append(f"| {v} | {pct(m['cagr'])} | {pct(m['annualized_vol'])} | {m['sharpe']:.3f} | {pct(m['max_drawdown'])} | {pct(m['avg_invested_weight'])} | {m['fills']}/{m['closed_trades']} | {m['turnover_two_way_annual']:.2f}x | {m['total_commission']:.2f} |")
    lines += ['', '### 分年收益（首尾为不完整年份）', '',
        '| 变体 | '+' | '.join(result['portfolios']['V0']['metrics']['calendar_year_returns'])+' |',
        '|---|'+'---:|'*len(result['portfolios']['V0']['metrics']['calendar_year_returns'])]
    for v, p in result['portfolios'].items():
        lines.append('| '+v+' | '+' | '.join(pct(x) for x in p['metrics']['calendar_year_returns'].values())+' |')
    lines += ['', '### 相对 V0 的日收益差，bp/日 [区块95%区间]', '',
        '| 变体 | 时段 | 全部 | 回撤期（前收盘状态） | 非回撤期 |',
        '|---|---|---:|---:|---:|']
    for v, p in result['portfolios'].items():
        for split, states in p['differences'].items():
            lines.append(f"| {v} | {split} | "+' | '.join(interval(states[s], 10000) for s in ('all', 'drawdown', 'normal'))+' |')
    lines += ['', '### 分割时段组合指标（连续持仓，边界以前收盘作锚）', '',
        '| 变体 | 时段 | CAGR | 波动 | Sharpe | MaxDD |', '|---|---|---:|---:|---:|---:|']
    for v, p in result['portfolios'].items():
        for split, m in p['periods'].items():
            if split == 'full':
                continue
            lines.append(f"| {v} | {split} | {pct(m['cagr'])} | {pct(m['annualized_vol'])} | {m['sharpe']:.3f} | {pct(m['max_drawdown'])} |")
    lines += ['', '## 局限与结论边界', '',
        '当前bStock宇宙带来严重存活者/选择偏差，且包含近期上市、主题与部分杠杆产品；Yahoo复权underlying常规时段日线不是bStock可成交报价。仅约6年输入、约5年有效回测且集中于单一美股行情，2020年及此前不可研究。新上市股票需自身253根才可进入；EW基准不含后来IPO。', '',
        '固定7变体并不构成7次独立试验；同时比较多期限/状态会增加偶然显著，10日区块也可能不足以涵盖长程依赖。样本外自2025年至数据末日只评估一次，不做再选择。训练+验证选出的定义也只有有限检验力。分类组的动量排名、股票和日期组成可能不同，分类差不是趋势状态的因果效应。缺失未来价格、IPO和最后k天标签删失也可能改变统计组成。', '',
        'AI逐笔决策不可回测。本研究没有检验downtrend/wedge_drop真实程序状态、EMA10/20回踩、1.5–3ATR止损或置信度规则；简化EMA分类不等于线上完整结构分类。候选更多≠更多开仓，候选标签优劣≠AI收益改进。', '',
        '结果文件：PROTOCOL.json、selection.json、holdout_opened.json、results.json、market_state.csv、V0.json…V6.json、SPY_BH.json、EW_BH.json、cache/manifest.json。JSON保留聚类正态区间、标签缺失数、每日权益、原生分析器和全部成交，方便审计。', '',
        '## 复现与验收', '',
        '在本worktree根目录运行；缓存转换仅只读源目录，所有产物位于 /private/tmp/claude-501/uscand/。首次研究依次运行：', '',
        '```bash',
        'env -u PYTHONPATH PYTHONPATH=. /Users/zhangyun/workspace/nofx/.venv/bin/python -m scripts.backtest.stocks_cache --tf 1d',
        'env -u PYTHONPATH PYTHONPATH=. /Users/zhangyun/workspace/nofx/.venv/bin/python -m scripts.backtest.us_candidate_study --stage select',
        'env -u PYTHONPATH PYTHONPATH=. /Users/zhangyun/workspace/nofx/.venv/bin/python -m scripts.backtest.us_candidate_study --stage evaluate',
        'env -u PYTHONPATH PYTHONPATH=. /Users/zhangyun/workspace/nofx/.venv/bin/python -m unittest discover -s scripts/backtest/tests -v',
        '```', '',
        '本目录已冻结选择并打开过样本外，重复select/evaluate会拒绝。读取已保存结果重排报告用 --stage report，不重新评估标签。测试日志见 tests.log；数据指纹和原生成交/佣金/仓位核对见 audit.json。未提交或推送代码。']
    return '\n'.join(lines)+'\n'


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--cache', default=str(BASE/'cache'))
    p.add_argument('--out', default=str(BASE))
    p.add_argument('--stage', choices=['select', 'evaluate', 'report', 'all'], default='all')
    a = p.parse_args(argv)
    out = output_root(a.out)
    out.mkdir(parents=True, exist_ok=True)
    if a.stage == 'report':
        result = json.loads((out/'results.json').read_text())
        (out/'REPORT.md').write_text(render_report(result))
        return
    protocol_file = out/'PROTOCOL.json'
    if protocol_file.exists() and json.loads(protocol_file.read_text()) != json.loads(json.dumps(PROTOCOL)):
        raise ValueError('protocol changed after registration')
    save(protocol_file, PROTOCOL)
    cache_digest = hashlib.sha256((Path(a.cache)/'manifest.json').read_bytes()).hexdigest()
    study = load_study(a.cache)
    close = close_panel(study)
    regime = market_state(close)
    classes, pools = memberships(study)
    if a.stage in ('select', 'all'):
        if (out/'holdout_opened.json').exists():
            raise ValueError('holdout already opened; use --stage report to render saved results')
        selection = select_filter(study, close, pools)
        selection.update(protocol_hash=fingerprint(PROTOCOL), cache_manifest_hash=cache_digest)
        path = out/'selection.json'
        if path.exists() and json.loads(path.read_text()) != selection:
            raise ValueError('selection already frozen with different data or rule')
        save(path, selection)
        print('frozen selection: '+json.dumps(selection, ensure_ascii=False), flush=True)
    if a.stage == 'select':
        return
    selection = json.loads((out/'selection.json').read_text())
    if selection['protocol_hash'] != fingerprint(PROTOCOL) or selection['cache_manifest_hash'] != cache_digest:
        raise ValueError('frozen selection protocol/data mismatch')
    marker = out/'holdout_opened.json'
    with marker.open('x') as f:
        json.dump(dict(selection=selection, opened_at=pd.Timestamp.now(tz='UTC').isoformat()), f, indent=2)
    print('holdout opened once; computing signal inference', flush=True)
    signals = signal_analysis(study, close, regime, classes, pools)
    save(out/'signals.json', signals)
    regime.to_csv(out/'market_state.csv')
    runs = native_runs(study, out)
    result = dict(protocol=PROTOCOL, selection=selection, coverage=study.coverage,
        data_start=str(close.index[0]), data_end=str(close.index[-1]), signals=signals,
        portfolios=portfolio_analysis(runs, regime))
    save(out/'results.json', result)
    (out/'REPORT.md').write_text(render_report(result))
    print('wrote '+str(out/'REPORT.md'), flush=True)


if __name__ == '__main__':
    main()
