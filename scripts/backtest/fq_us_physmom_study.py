"""Frozen fq_us physical-momentum S1; offline inputs, native Backtrader arms.

Importing this module does not read data or run the study. The reviewer must
explicitly invoke the CLI to evaluate the frozen D3 and D2 snapshots.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

from . import evolution_ledger
from . import fq_us_momentum_study as s1
from . import fq_us_volscale_study as volscale
from . import incremental_filter


PREREG_PATH = 'docs/research/2026-11-11-physical-momentum-s1-prereg.md'
PREREGISTERED_K = 8
Z_K = incremental_filter.z_threshold(8)
PLACEBO_DRAWS = 20
PLACEBO_SEED = 20261111
WINDOW = 231
SKIP = 21
MIN_MASS_DAYS = 200
MAX_FALLBACK_SHARE = 0.05
MIN_PERIODS = 100
D3_B_EXPECTED_TOTAL = -0.03356662925829279
D3_B_REL_TOL = 1e-6
D2_B_EXPECTED_TOTAL = 1.337151667353913
DATASETS = {name: volscale.DATASETS[name] for name in ('D3', 'D2')}
EXPECTED_HASHES = {name: volscale.EXPECTED_HASHES[name] for name in DATASETS}
INITIAL_CASH = volscale.INITIAL_CASH
BASELINE_VERSION = 'fq_us:mom_ls_invvol_v1'
NOTES = [
    '本研究是成交量加权 12-1 动量的改造，不是原文周度反转的复现。D3、D2 已被查看，现有成分回看存在幸存者偏差，不构成样本外证据。',
    'B、C 及安慰剂只替换排序信号；股票可用性、收盘价、流动性、波动率倒数权重、并列规则、调仓时钟与成本全部复用原规则。',
    '收益为相邻 SPY 日历收盘价的对数比，不跨缺价填充；正且有限的成交量与有限收益同时成立才有质量。窗口为 t−251…t−21，共 231 个收益。',
    '歧义口径：窗口起始历史不足时按已有有限收益计算，正质量日不足 200 或质量和为零则回退；没有有限收益时信号缺失，回退标志为真。基线可用性门槛仍原样保留。',
    '歧义口径：回退比例按所有调仓日期（含终点调仓）的可用股票-日期计数，不按入选或成交计数；分母为零时比例缺失并判 invalid。',
    '两腿分别调用 volscale.run_arm → momentum.run_sleeve 的原生 Backtrader，k 恒为 1；占位 σ̂/σ_ref 为 NaN、观测数为 0，不计算波动缩放信号。',
    '组合复用 50/50 等权重置后处理；调仓日先按旧锚点计算结算后权益，再重设锚点。忽略资金重分配引起的佣金规模差异，与冻结 B 一致。',
    '周期收益复用 period_table，以各臂自身扣费后起止收盘权益之比计算；首日建仓费进入累计收益但不重复进入周期比值，终点调仓照常扣费。多头每笔 13 bp，空头 7 bp，资金费为零。',
    '歧义口径：多头腿、空头腿的 Δ 各用其原生独立袖套权益计算；分年 Δ 按完整周期起点年份归组取算术平均，不把 Δ 连乘。另列两臂自然年收益之差。',
    '歧义口径：重合比例为交集数/基线该腿入选数；空组记缺失。秩相关为有限信号的平均秩 Pearson（Spearman），不足两只或任一信号无截面变化时缺失；只统计完整周期起点，汇总排除缺失。',
    '歧义口径：安慰剂按代码升序，使用同一个 RNG 连续抽取 20 次；只平移 close 非缺失行上的成交量，其他行原样保留。N<127 时偏移为 0、记录该股且不消耗随机数。',
    '歧义口径：安慰剂保留逐期 Δ、统计、每股偏移与短历史记录，不保留全部原生路径，以免 20 份持仓审计占用过多内存；B 只计算一次。严格大于才计入分位。',
    '歧义口径：D3 B 相对误差使用冻结预期收益的绝对值作分母，绝对容差为零；失败只记 invalid，不抛出异常。D2 B 对照值只作描述，不参与判定。',
    '实现修正（2026-11-12，首跑崩溃后、未见 C 结果前）：30 日波动率缺失或非正的股票-日期无法按波动率倒数定权，统一从 B、C 与安慰剂的可用池剔除。D3 只有 1 个调仓股票-日期受影响（HUBB 2021-06-09，30 日内缺 1 根收盘），首跑在安慰剂第 14 次选中它时报错，未写报告和账本。B 的入选与权重需与未剔除时逐期相同，否则记录并由复现门槛判 invalid。',
    '哈希不一致直接停止，不允许输入漂移；固定同时运行 D3、D2，不运行 D1、不搜索参数。',
    '歧义口径：CLI 必须显式选择 --ledger 或 --no-ledger；invalid 返回退出码 1，写账本但不计 K。',
    '组合夏普、最大回撤和累计收益复用 volscale.describe_path；组合换手按原后处理口径不提供，另列两腿原生换手和独立双腿合计换手作描述。',
    'S1 的 strong/screen_pass 只用于筛查；不改线上规则或交易配置，不自动进入 S2，不启动交易或下单。',
]


def _aligned(close, volume):
    if not close.index.equals(volume.index) or not close.columns.equals(volume.columns):
        raise ValueError('close and volume must share the same calendar and symbols')


def physical_momentum(close, volume):
    """Causal p(2), using calendar positions t-251 through t-21 inclusive."""
    _aligned(close, volume)
    with np.errstate(divide='ignore', invalid='ignore'):
        returns = np.log(close / close.shift(1))
    returns = returns.where(np.isfinite(returns))
    mass = volume.where(np.isfinite(volume) & volume.gt(0) & returns.notna(), 0.0)
    mass_sum = mass.rolling(WINDOW, min_periods=1).sum().shift(SKIP)
    weighted_sum = (mass * returns.fillna(0.0)).rolling(WINDOW, min_periods=1).sum().shift(SKIP)
    n_mass = mass.gt(0).rolling(WINDOW, min_periods=1).sum().shift(SKIP)
    fallback = n_mass.fillna(0).lt(MIN_MASS_DAYS) | mass_sum.eq(0)
    equal_mean = returns.rolling(WINDOW, min_periods=1).mean().shift(SKIP)
    signal = (weighted_sum / mass_sum.where(mass_sum.gt(0))).mask(fallback, equal_mean)
    return dict(signal=signal, fallback=fallback)


def investable_features(features):
    """Drop stock-dates whose inverse-volatility weight is undefined (all arms alike)."""
    vol = features['volatility']
    defined = np.isfinite(vol) & vol.gt(0)
    return dict(features, eligible=features['eligible'] & defined), features['eligible'] & ~defined


def _selection(decisions):
    return [(d['date'], d['long'], d['short'], d['L']) for d in decisions]


def candidate_features(features, volume):
    """Replace only the ranking signal; all baseline filters remain intact."""
    return dict(features, signal=physical_momentum(features['close'], volume)['signal'])


def candidate_schedule(features, volume, calendar, baseline_decisions):
    candidate = candidate_features(features, volume)
    sessions, decisions = s1.rebalance_schedule(candidate, calendar)
    assert [d['date'] for d in decisions] == [d['date'] for d in baseline_decisions], \
        'candidate and baseline decision dates must align'
    return candidate, sessions, decisions


def fallback_usage(eligible, fallback, decisions):
    """Count eligible stock-dates, including the terminal rebalance."""
    _aligned(eligible, fallback)
    dates = pd.DatetimeIndex([d['date'] for d in decisions])
    available = eligible.loc[dates]
    total = int(available.to_numpy().sum())
    used = int((available & fallback.loc[dates]).to_numpy().sum())
    return dict(eligible_stock_dates=total, fallback_stock_dates=used,
                share=used / total if total else None)


def run_arm_unscaled(frames, sessions, decisions, *, label):
    """The frozen volscale B arm, with dummy diagnostic fields and k=1."""
    print(f'{label}: native Backtrader arm (long + short)', flush=True)
    dates = pd.DatetimeIndex([d['date'] for d in decisions])
    signal = pd.DataFrame(dict(sigma_hat=np.nan, sigma_ref=np.nan,
                               sigma_observations=0), index=dates)
    scales = {d['date']: 1.0 for d in decisions}
    return volscale.run_arm(frames, sessions, decisions, scales, signal, d1=False)


def paired_periods(baseline, candidate):
    """Assert pairing before forming C minus B; never silently join dates."""
    assert len(baseline) == len(candidate), 'period counts must align'
    rows = []
    for b, c in zip(baseline, candidate, strict=True):
        assert all(b[key] == c[key] for key in ('k', 'start', 'end', 'trading_days')), \
            'period k and dates must align'
        rows.append(dict(k=b['k'], start=b['start'], end=b['end'],
                         trading_days=b['trading_days'], baseline_return=b['net_return'],
                         candidate_return=c['net_return'], delta=c['net_return'] - b['net_return']))
    return rows


def paired_statistics(baseline, candidate):
    rows = paired_periods(baseline, candidate)
    stats = s1.period_statistics([r['delta'] for r in rows])
    # The study's sole verdict additionally gates on placebos, D2 and validity.
    stats.pop('verdict')
    return dict(period_table=rows, **stats)


def compare_arms(baseline, candidate, decisions):
    result = paired_statistics(baseline['period_table'], candidate['period_table'])
    result['legs'] = {leg: paired_statistics(
        s1.period_table(baseline['legs'][leg]['equity_curve'], decisions),
        s1.period_table(candidate['legs'][leg]['equity_curve'], decisions))
        for leg in ('long', 'short')}
    years = sorted({r['start'][:4] for r in result['period_table']})
    result['per_year'] = {}
    for year in years:
        values = [r['delta'] for r in result['period_table'] if r['start'].startswith(year)]
        result['per_year'][year] = dict(n=len(values), mean=float(np.mean(values)))
    b_years, c_years = [arm['metrics']['yearly_returns'] for arm in (baseline, candidate)]
    assert b_years.keys() == c_years.keys(), 'arm years must align'
    result['yearly_return_delta'] = {year: c_years[year] - b_years[year] for year in b_years}
    return result


def selection_diagnostics(features, candidate, baseline_decisions, candidate_decisions):
    assert [d['date'] for d in baseline_decisions] == [d['date'] for d in candidate_decisions], \
        'diagnostic decision dates must align'
    rows = []
    for k, (b, c) in enumerate(zip(baseline_decisions[:-1], candidate_decisions[:-1], strict=True), 1):
        date = pd.Timestamp(b['date'])
        names = features['eligible'].columns[features['eligible'].loc[date]]
        signals = pd.DataFrame(dict(b=features['signal'].loc[date, names],
                                    c=candidate['signal'].loc[date, names]))
        signals = signals.replace([np.inf, -np.inf], np.nan).dropna()
        corr = None
        if len(signals) >= 2 and signals.b.nunique() > 1 and signals.c.nunique() > 1:
            corr = float(signals.b.rank(method='average').corr(signals.c.rank(method='average')))
        row = dict(k=k, start=b['date'], end=baseline_decisions[k]['date'],
                   eligible_count=len(names), rank_count=len(signals), spearman=corr)
        for leg in ('long', 'short'):
            left, right = set(b[leg]), set(c[leg])
            row[leg + '_overlap'] = len(left & right) / len(left) if left else None
        rows.append(row)
    result = dict(period_table=rows)
    for key in ('spearman', 'long_overlap', 'short_overlap'):
        values = [r[key] for r in rows if r[key] is not None]
        result['median_' + key] = float(np.median(values)) if values else None
    return result


def shift_volume(close, volume, rng):
    """One preregistered circular placebo draw, in sorted symbol order."""
    _aligned(close, volume)
    shifted = volume.copy()
    offsets, short_histories = {}, {}
    for symbol in sorted(close.columns):
        valid = close[symbol].notna()
        n = int(valid.sum())
        offset = int(rng.integers(63, n - 63 + 1)) if n >= 127 else 0
        if n < 127:
            short_histories[symbol] = n
        offsets[symbol] = offset
        shifted.loc[valid, symbol] = np.roll(volume.loc[valid, symbol].to_numpy(), offset)
    return dict(volume=shifted, offsets=offsets, short_histories=short_histories)


def placebo_summary(candidate_mean, means):
    if len(means) != PLACEBO_DRAWS or not np.isfinite(means).all():
        raise ValueError('exactly 20 finite placebo means are required')
    values = np.asarray(means, dtype=float)
    return dict(placebo_pct=float((candidate_mean > values).sum() / PLACEBO_DRAWS),
                distribution=dict(n=len(values), min=float(values.min()), max=float(values.max()),
                    mean=float(values.mean()), median=float(np.median(values)),
                    sd=float(values.std(ddof=1)), p05=float(np.quantile(values, 0.05)),
                    p25=float(np.quantile(values, 0.25)), p75=float(np.quantile(values, 0.75)),
                    p95=float(np.quantile(values, 0.95))))


def baseline_reproduction(total, expected):
    relative_error = abs(total - expected) / abs(expected)
    return dict(expected_total_return=expected, actual_total_return=total,
                relative_error=relative_error, relative_tolerance=D3_B_REL_TOL,
                passed=math.isfinite(total) and relative_error <= D3_B_REL_TOL)


def validity_checks(stats, fallback_share, baseline_reproduced):
    return dict(enough_periods=stats['n'] >= MIN_PERIODS,
                fallback_within_limit=fallback_share is not None and 0 <= fallback_share <= MAX_FALLBACK_SHARE,
                baseline_reproduced=bool(baseline_reproduced))


def decide_verdict(stats, *, fallback_share, baseline_reproduced, placebo_pct, d2_mean_delta):
    """Frozen first-match D3 validity and strong/screen_pass/fail gates."""
    if not all(validity_checks(stats, fallback_share, baseline_reproduced).values()):
        return 'invalid'
    positive = all(stats['halves'][half]['mean'] is not None and stats['halves'][half]['mean'] > 0
                   for half in ('first', 'second'))
    if (stats['z'] is not None and positive and placebo_pct is not None and placebo_pct >= 0.90
            and d2_mean_delta is not None and d2_mean_delta >= 0):
        if stats['z'] >= Z_K:
            return 'strong'
        if stats['z'] >= 1.0:
            return 'screen_pass'
    return 'fail'


def guard_hashes():
    """Use the existing frozen guard; deliberately expose no drift override."""
    return volscale.guard_hashes(tuple(DATASETS))


def prereg_sha256():
    path = Path(__file__).resolve().parents[2] / PREREG_PATH
    return hashlib.sha256(path.read_bytes()).hexdigest()


def run_study():
    hashes, files, mismatches = guard_hashes()
    report = dict(prereg_path=PREREG_PATH, prereg_sha256=prereg_sha256(),
                  preregistered_K=PREREGISTERED_K, z_threshold=Z_K, engine='backtrader',
                  baseline_version=BASELINE_VERSION, rule='mom_ls_invvol_physmom_v1',
                  params=dict(base=s1.PARAMS, window=WINDOW, skip=SKIP, min_mass_days=MIN_MASS_DAYS,
                              max_fallback_share=MAX_FALLBACK_SHARE, min_periods=MIN_PERIODS,
                              placebo_draws=PLACEBO_DRAWS, placebo_seed=PLACEBO_SEED,
                              d3_b_expected_total=D3_B_EXPECTED_TOTAL, d3_b_rel_tol=D3_B_REL_TOL),
                  input_hashes=hashes, expected_input_hashes=EXPECTED_HASHES,
                  input_daily_files=files, input_drift=mismatches, notes=list(NOTES),
                  deviations=[n for n in NOTES if n.startswith(('歧义口径', '实现修正'))], datasets={})
    for name in DATASETS:
        print(f'{name}: loading frozen inputs', flush=True)
        loaded = volscale.load_dataset(name)
        frames, calendar = loaded.pop('frames'), loaded.pop('calendar')
        universe = sorted(set(loaded['requested']) & set(frames))
        raw_features = s1.build_features(frames, universe, calendar)
        features, undefined_vol = investable_features(raw_features)
        volume = pd.DataFrame({s: frames[s].volume for s in universe}).reindex(calendar)
        sessions, decisions_b = s1.rebalance_schedule(features, calendar)
        raw_sessions, raw_decisions_b = s1.rebalance_schedule(raw_features, calendar)
        decision_index = pd.DatetimeIndex([d['date'] for d in decisions_b])
        vol_filter = dict(
            excluded_decision_stock_dates=[
                dict(date=str(d.date()), symbol=s) for d, row in undefined_vol.loc[decision_index].iterrows()
                for s in row.index[row]],
            baseline_selection_unchanged=bool(raw_sessions.equals(sessions)
                                              and _selection(raw_decisions_b) == _selection(decisions_b)))
        print(f"{name}: undefined-vol exclusions on rebalance dates="
              f"{len(vol_filter['excluded_decision_stock_dates'])}, "
              f"B selection unchanged={vol_filter['baseline_selection_unchanged']}", flush=True)
        del raw_features, raw_decisions_b
        features_c, sessions_c, decisions_c = candidate_schedule(features, volume, calendar, decisions_b)
        assert sessions.equals(sessions_c), 'candidate and baseline sessions must align'
        fallback = fallback_usage(features['eligible'],
                                  physical_momentum(features['close'], volume)['fallback'], decisions_c)
        stock_frames = {s: frames[s] for s in universe}
        arms = dict(B=run_arm_unscaled(stock_frames, sessions, decisions_b, label=f'{name} B'),
                    C=run_arm_unscaled(stock_frames, sessions, decisions_c, label=f'{name} C'))
        comparison = compare_arms(arms['B'], arms['C'], decisions_b)
        expected = D3_B_EXPECTED_TOTAL if name == 'D3' else D2_B_EXPECTED_TOTAL
        reproduction = dict(baseline_reproduction(arms['B']['metrics']['total_return'], expected),
                            descriptive_only=(name == 'D2'))
        dataset = dict(**loaded, universe=universe,
                       unavailable=sorted(set(loaded['requested']) - set(universe)),
                       start=str(sessions[0].date()), end=str(sessions[-1].date()),
                       trading_sessions=len(sessions), n=comparison['n'],
                       trailing_sessions_excluded=int((calendar > sessions[-1]).sum()),
                       rebalance_dates=[d['date'] for d in decisions_b],
                       decisions=dict(B=decisions_b, C=decisions_c), arms=arms,
                       comparison=comparison, fallback=fallback, baseline_reproduction=reproduction,
                       vol_filter=vol_filter,
                       selection=selection_diagnostics(features, features_c, decisions_b, decisions_c))
        report['datasets'][name] = dataset
        report['engine_version'] = arms['B']['legs']['long']['engine_version']
        if any(q['invalid_rows'] or q.get('off_calendar_rows', 0) for q in loaded['data_quality'].values()):
            report['deviations'].append(f'{name} 存在无效或日历外日线，按原规则排除，详见 data_quality。')
        if name == 'D3':
            draws = []
            rng = np.random.default_rng(PLACEBO_SEED)
            for draw in range(1, PLACEBO_DRAWS + 1):
                shifted = shift_volume(features['close'], volume, rng)
                _, placebo_sessions, decisions = candidate_schedule(
                    features, shifted['volume'], calendar, decisions_b)
                assert sessions.equals(placebo_sessions), 'placebo and baseline sessions must align'
                arm = run_arm_unscaled(stock_frames, sessions, decisions, label=f'D3 placebo {draw:02}/20')
                stats = paired_statistics(arms['B']['period_table'], arm['period_table'])
                draws.append(dict(draw=draw, **stats, offsets=shifted['offsets'],
                                  short_histories=shifted['short_histories'], metrics=arm['metrics']))
                print(f"D3 placebo {draw:02}/20: mean Δ={stats['mean']:+.8f}", flush=True)
                del arm
            report['placebo'] = dict(seed=PLACEBO_SEED, draws=draws,
                **placebo_summary(comparison['mean'], [d['mean'] for d in draws]))
        print(f"{name}: complete, n={comparison['n']}, mean Δ={comparison['mean']:+.8f}, "
              f"z={comparison['z']}, B reproduction={reproduction['passed']}", flush=True)
    primary = report['datasets']['D3']
    report.update({key: primary['comparison'][key] for key in ('n', 'mean', 'sd', 'z', 'halves')})
    report.update(placebo_pct=report['placebo']['placebo_pct'],
                  d2_mean_delta=report['datasets']['D2']['comparison']['mean'],
                  fallback_share=primary['fallback']['share'],
                  baseline_reproduced=primary['baseline_reproduction']['passed'])
    report['validity_checks'] = validity_checks(primary['comparison'], report['fallback_share'],
                                              report['baseline_reproduced'])
    report['invalid_reasons'] = [key for key, passed in report['validity_checks'].items() if not passed]
    report['verdict'] = decide_verdict(primary['comparison'], fallback_share=report['fallback_share'],
        baseline_reproduced=report['baseline_reproduced'], placebo_pct=report['placebo_pct'],
        d2_mean_delta=report['d2_mean_delta'])
    return report


def append_ledger(path, report):
    def build(k, previous_rows):
        if k != PREREGISTERED_K:
            report['deviations'].append(
                f'账本口径：预注册 K=8；按已有有效 S1 记录计算下一 K={k}，不补造历史行；invalid 不计 K。')
        return dict(stage='S1', change_type='E', idea_id='fq-us-physical-momentum-v1',
                    baseline_version=BASELINE_VERSION, verdict=report['verdict'],
                    preregistered_K=PREREGISTERED_K, prereg_path=PREREG_PATH,
                    prereg_sha256=report['prereg_sha256'], report_path=report.get('report_path'),
                    n=report['n'], mean_delta=report['mean'], z=report['z'], halves=report['halves'],
                    placebo_pct=report['placebo_pct'], d2_mean_delta=report['d2_mean_delta'],
                    fallback_share=report['fallback_share'], input_hashes=report['input_hashes'],
                    baseline_reproduced=report['baseline_reproduced'],
                    invalid_reasons=report['invalid_reasons'], deviations=report['deviations'])
    return evolution_ledger.append_event(path, 'fq_us', build)


def render_markdown(report):
    def number(value):
        return '无' if value is None else f'{value:.6f}'

    def pct(value):
        return '无' if value is None else f'{value:+.4%}'

    lines = ['# fq_us 成交量加权物理动量：预注册 S1', '',
             f"判定：**{report['verdict']}**；预注册 K=8，strong 门槛 z≥{Z_K:.6f}。", '',
             '本研究使用已看过的 D3/D2，仅为离线筛选；不能据此修改线上规则。B 为 mom12_1，C 为 p2_vol；组合均为原生 Backtrader 双腿经 50/50 等权重置后处理。', '',
             '| 数据 | n | mean Δ | z | 前半 mean Δ | 后半 mean Δ | 回退股票-日期 | 回退比例 |',
             '|---|---:|---:|---:|---:|---:|---:|---:|']
    for name, dataset in report['datasets'].items():
        stats, fallback = dataset['comparison'], dataset['fallback']
        lines.append(f"| {name} | {stats['n']} | {pct(stats['mean'])} | {number(stats['z'])} | "
                     f"{pct(stats['halves']['first']['mean'])} | {pct(stats['halves']['second']['mean'])} | "
                     f"{fallback['fallback_stock_dates']}/{fallback['eligible_stock_dates']} | {pct(fallback['share'])} |")
    lines += ['', f"D3 安慰剂分位：{pct(report['placebo_pct'])}（严格大于，20 次）；D2 mean Δ：{pct(report['d2_mean_delta'])}。",
              'strong/screen_pass 均要求两半均值>0、安慰剂分位≥90%、D2 mean Δ≥0；screen_pass 要求 z≥1。',
              f"有效性检查：`{json.dumps(report['validity_checks'], ensure_ascii=False)}`；invalid 原因：{', '.join(report['invalid_reasons']) or '无'}。", '',
              '## 两臂表现', '', '| 数据 | 臂 | 夏普 | 最大回撤 | 累计收益 | 原生独立双腿年化换手（描述） |',
              '|---|---|---:|---:|---:|---:|']
    for name, dataset in report['datasets'].items():
        for label, arm in dataset['arms'].items():
            m = arm['metrics']
            turnover = arm['independent_sleeves']['metrics']['annualized_turnover']
            lines.append(f"| {name} | {label} | {number(m['sharpe'])} | {pct(m['max_drawdown'])} | {pct(m['total_return'])} | {number(turnover)} |")
        check = dataset['baseline_reproduction']
        role = '仅描述，不参与判定' if check['descriptive_only'] else ('通过' if check['passed'] else '失败，invalid')
        lines += ['', f"{name} B 复现：{role}；实际 {number(check['actual_total_return'])}，预期 {number(check['expected_total_return'])}，相对误差 {number(check['relative_error'])}，容差 {D3_B_REL_TOL:g}。"]
    lines += ['', '## 安慰剂分布', '', '| n | 最小 | 5% | 25% | 中位 | 均值 | 75% | 95% | 最大 | 标准差 |',
              '|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|']
    dist = report['placebo']['distribution']
    lines.append(f"| {dist['n']} | " + ' | '.join(pct(dist[k]) for k in
                 ('min', 'p05', 'p25', 'median', 'mean', 'p75', 'p95', 'max', 'sd')) + ' |')
    lines += ['', '| 抽样 | mean Δ | z | N<127 股票数 |', '|---:|---:|---:|---:|']
    for draw in report['placebo']['draws']:
        lines.append(f"| {draw['draw']} | {pct(draw['mean'])} | {number(draw['z'])} | {len(draw['short_histories'])} |")
    lines += ['', '## 描述性比较', '', '| 数据 | 多头腿 mean Δ | 空头腿 mean Δ | 多头重合中位 | 空头重合中位 | 秩相关中位 |',
              '|---|---:|---:|---:|---:|---:|']
    for name, dataset in report['datasets'].items():
        legs, selection = dataset['comparison']['legs'], dataset['selection']
        lines.append(f"| {name} | {pct(legs['long']['mean'])} | {pct(legs['short']['mean'])} | "
                     f"{pct(selection['median_long_overlap'])} | {pct(selection['median_short_overlap'])} | {number(selection['median_spearman'])} |")
    lines += ['', '## 分年 Δ', '', '| 数据 | 年份 | 完整周期 n | 周期 mean Δ | 自然年 C−B 收益 |',
              '|---|---|---:|---:|---:|']
    for name, dataset in report['datasets'].items():
        comparison = dataset['comparison']
        for year, delta in comparison['yearly_return_delta'].items():
            group = comparison['per_year'].get(year, {})
            lines.append(f"| {name} | {year} | {group.get('n', 0)} | {pct(group.get('mean'))} | {pct(delta)} |")
    lines += ['', '## 偏离与歧义处理', ''] + [f'- {n}' for n in report['deviations']]
    lines += ['', '## 口径与局限', ''] + [f'- {n}' for n in report['notes'] if not n.startswith('歧义口径')]
    lines += ['', '## 输入与账本', '', f'预注册：`{PREREG_PATH}`；SHA256：`{report["prereg_sha256"]}`。']
    for name, digest in report['input_hashes'].items():
        lines.append(f'- {name} 日线 SHA256：`{digest}`。')
    row = report.get('ledger_row')
    lines += ['', f"账本 K={row['K']}；预注册 K=8；计入 K：{row['counts_toward_k']}。" if row else '本次未写账本；预注册 K=8。',
              '完整周期 Δ、两腿、分年、逐期重合与秩相关、B/C 原生路径及成交、安慰剂偏移详见 report.json。']
    return '\n'.join(lines) + '\n'


def output_paths(out, ledger=None):
    out, ledger = volscale.output_paths(out, ledger)
    if ledger in (out / 'report.json', out / 'report.md',
                  (Path(__file__).resolve().parents[2] / PREREG_PATH).resolve()):
        raise ValueError('ledger must not overwrite a report or preregistration')
    return out, ledger


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument('--ledger', type=Path)
    group.add_argument('--no-ledger', action='store_true')
    args = parser.parse_args(argv)
    out, ledger = output_paths(args.out, args.ledger)
    if any((out / name).exists() for name in ('report.json', 'report.md')):
        raise FileExistsError('refusing to overwrite an existing study; use a fresh --out')
    report = run_study()
    out.mkdir(parents=True, exist_ok=True)
    report['report_path'] = str(out / 'report.json')
    if not args.no_ledger:
        report['ledger_row'] = append_ledger(ledger, report)
    safe = evolution_ledger.json_safe(report)
    (out / 'report.json').write_text(json.dumps(safe, ensure_ascii=False, indent=2,
                                               allow_nan=False) + '\n', encoding='utf-8')
    (out / 'report.md').write_text(render_markdown(report), encoding='utf-8')
    print(f"verdict={report['verdict']} z={report['z']} n={report['n']}", flush=True)
    return 1 if report['verdict'] == 'invalid' else 0


if __name__ == '__main__':
    raise SystemExit(main())
