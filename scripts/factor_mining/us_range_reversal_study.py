"""Frozen US range-position / six-month reversal S1 screen.

Run writes reports only. Record appends V1 then V2 through the shared ledger.
All windows and forward labels are calculated on each ticker's own bars.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
from pathlib import Path
from statistics import NormalDist
import sys
import time

import numpy as np
import pandas as pd

from scripts.backtest import evolution_ledger
from . import evaluate

REPO = Path(__file__).resolve().parents[2]
PREREG = 'docs/research/2026-11-05-us-range-reversal-prereg.md'
DATA = Path('/Users/zhangyun/workspace/nofx/data/factor_mining/us/daily')
DEFAULT_LEDGER = DATA.parents[3] / evolution_ledger.DEFAULT_PATH
EXPECTED_HASH = '67b64d0577f9d24ebd6908e51f17d85ec3559e6f77064b736d02b99d2337d82c'
EXCLUDED = ('SPY', 'SMH', 'EWY', 'TMF', 'DRAM', 'KSTR', 'INTW', 'MVLL', 'SKUU', 'SKDD', 'SNXX')
START, END = '2020-11-03', '2026-11-02'
TRACK, BASELINE_VERSION = 'us_llm', 'us_stock:TopMomentum:mom12_1:top5'
VARIANTS = {'V1': ('range_pos', 'us-range-pos-v1'), 'V2': ('mom6', 'us-mom6-reversal-v1')}
SIGNALS = {'B': ('mom12_1', False), 'V1': ('range_pos', True), 'V2': ('mom6', True)}
N, MIN_ELIGIBLE, LAG, SEED, PLACEBOS = 5, 10, 19, 20261105, 200
STAT_FIELDS = ('delta', 'se', 'z', 'dev_delta', 'holdout_delta', 'n_days')


def guard_inputs(data, allow_input_drift=False):
    paths = sorted(Path(data).glob('*.json'), key=lambda p: p.name)
    if not paths:
        raise ValueError('no *.json input files')
    digest = hashlib.sha256()
    for path in paths:
        digest.update(path.read_bytes())
    actual = digest.hexdigest()
    drift = {} if actual == EXPECTED_HASH else dict(expected=EXPECTED_HASH, actual=actual)
    if drift and not allow_input_drift:
        raise ValueError(f'frozen input sha256 mismatch: {drift}; explicit --allow-input-drift required')
    return actual, drift


def ticker_features(bars):
    """Calculate before reindexing: a missing master date must remain missing."""
    bars = bars.sort_index()
    if bars.index.has_duplicates:
        raise ValueError('duplicate ticker bar dates')
    close = bars['close'].astype(float)
    low = close.rolling(252, min_periods=252).min()
    high = close.rolling(252, min_periods=252).max()
    result = pd.DataFrame(dict(
        close=close, sma200=close.rolling(200, min_periods=200).mean(),
        mom12_1=close.shift(21) / close.shift(252) - 1,
        range_pos=(close - low) / (high - low).where(high != low),
        mom6=close / close.shift(126) - 1))
    for h in (5, 10, 20):
        result[f'fwd{h}'] = close.shift(-h) / close - 1
    return result.replace([np.inf, -np.inf], np.nan)


def align_panels(bars):
    if 'SPY' not in bars:
        raise ValueError('SPY is required for the master calendar')
    calendar = bars['SPY'].index.sort_values()
    if calendar.has_duplicates or calendar.empty:
        raise ValueError('SPY calendar must be nonempty and unique')
    features = {ticker: ticker_features(frame) for ticker, frame in sorted(bars.items())
                if ticker not in EXCLUDED}
    if not features:
        raise ValueError('empty stock universe')
    fields = next(iter(features.values())).columns
    return {field: pd.DataFrame({ticker: frame[field].reindex(calendar)
                                 for ticker, frame in features.items()}, index=calendar)
            for field in fields}


def load_panels(data, frozen=True):
    bars = {}
    for path in sorted(Path(data).glob('*.json')):
        rows = json.loads(path.read_text(encoding='utf-8'))
        frame = pd.DataFrame(rows, columns=['date', 'open', 'high', 'low', 'close', 'volume', 'typical'])
        frame.index = pd.DatetimeIndex(pd.to_datetime(frame.pop('date')))
        bars[path.stem] = frame.loc[START:END]
    if frozen and (len(bars) != 101 or not set(EXCLUDED).issubset(bars)):
        raise ValueError('frozen universe must have 101 files including all 11 exclusions')
    panels = align_panels(bars)
    if frozen and panels['close'].shape[1] != 90:
        raise ValueError('frozen stock universe must contain exactly 90 names')
    return panels


def eligible_pool(panels, use_sma=True):
    eligible = np.isfinite(panels['mom12_1']) & np.isfinite(panels['fwd20'])
    if use_sma:
        eligible &= panels['close'] > panels['sma200']
    return eligible


def top_five(values, eligible, ascending):
    # Sorting the index first makes stable value ties alphabetical in both directions.
    valid = values.loc[eligible & np.isfinite(values)].sort_index()
    return valid.sort_values(ascending=ascending, kind='stable').index[:N].tolist()


def select_groups(panels, use_sma=True):
    eligible = eligible_pool(panels, use_sma)
    selections = {arm: {} for arm in SIGNALS}
    for date in eligible.index[eligible.sum(axis=1) >= MIN_ELIGIBLE]:
        for arm, (signal, ascending) in SIGNALS.items():
            members = top_five(panels[signal].loc[date], eligible.loc[date], ascending)
            if len(members) == N:
                selections[arm][date] = members
    returns = {arm: pd.Series({date: float(panels['fwd20'].loc[date, members].mean())
                               for date, members in days.items()}, dtype=float).sort_index()
               for arm, days in selections.items()}
    return returns, selections, eligible


def nw_se(series, lag):
    """Companion SE with exactly evaluate.nw_t's Bartlett weights and zero floor."""
    x = np.asarray(series, dtype=float)
    x = x[np.isfinite(x)]
    n = len(x)
    if n < 20:
        return None
    mean = float(x.mean())
    centered = x - mean
    variance = float(np.dot(centered, centered) / n)
    for k in range(1, min(int(lag), n - 1) + 1):
        variance += 2 * (1 - k / (lag + 1)) * float(np.dot(centered[k:], centered[:-k]) / n)
    if variance <= 1e-30:
        return abs(mean) / 1e12 if mean else 0.0
    return math.sqrt(variance / n)


def date_range(series):
    return [str(series.index[0].date()), str(series.index[-1].date())] if len(series) else [None, None]


def paired_diff(variant, baseline):
    diff = (variant - baseline).replace([np.inf, -np.inf], np.nan).dropna().sort_index()
    cut = (len(diff) + 1) // 2
    dev, holdout = diff.iloc[:cut], diff.iloc[cut:]
    return dict(delta=float(diff.mean()) if len(diff) else None, se=nw_se(diff, LAG),
                z=evaluate.nw_t(diff, LAG), n_days=len(diff), date_range=date_range(diff),
                dev_delta=float(dev.mean()) if len(dev) else None,
                holdout_delta=float(holdout.mean()) if len(holdout) else None,
                halves=dict(dev=dict(n_days=len(dev), date_range=date_range(dev)),
                            holdout=dict(n_days=len(holdout), date_range=date_range(holdout))),
                daily_diff={str(d.date()): float(v) for d, v in diff.items()})


def verdict(stats, k):
    if k < 1:
        raise ValueError('K must be at least 1')
    z_k = NormalDist().inv_cdf(1 - .05 / (2 * k))
    if stats['n_days'] < 100:
        result = 'invalid'
    elif (stats['z'] is not None and stats['dev_delta'] is not None and
          stats['holdout_delta'] is not None and
          stats['dev_delta'] > 0 and stats['holdout_delta'] > 0):
        result = 'strong' if stats['z'] >= z_k else 'screen_pass' if stats['z'] >= 1. else 'fail'
    else:
        result = 'fail'
    return dict(verdict=result, z_K=z_k, K=k if result != 'invalid' else None)


def turnover(selections):
    days = sorted(selections)
    changes = {str(day.date()): 1 - len(set(selections[previous]) & set(selections[day])) / N
               for previous, day in zip(days, days[1:])}
    return dict(mean=float(np.mean(list(changes.values()))) if changes else None,
                n_transitions=len(changes), daily=changes)


def rank_ic_study(panels):
    result = {}
    for signal in ('range_pos', 'mom6', 'mom12_1'):
        result[signal] = {}
        for h in (5, 10, 20):
            values, target = panels[signal], panels[f'fwd{h}']
            ic = evaluate.cs_rank_ic(values, target, values.notna(), min_names=2).dropna()
            result[signal][str(h)] = dict(mean=float(ic.mean()) if len(ic) else None,
                                         nw_t=evaluate.nw_t(ic, h - 1), lag=h - 1,
                                         n_days=len(ic), date_range=date_range(ic),
                                         daily={str(d.date()): float(v) for d, v in ic.items()})
    return result


def placebo_study(panels, eligible, baseline, variants, progress):
    rng = np.random.default_rng(SEED)
    days = baseline.index
    pools = [np.flatnonzero(eligible.loc[d].to_numpy()) for d in days]
    labels = panels['fwd20'].reindex(days).to_numpy()
    base = baseline.to_numpy()
    means = []
    # Each repeat is one complete date-ordered random portfolio path.
    for repeat in range(PLACEBOS):
        differences = [float(labels[i, rng.choice(pool, N, replace=False)].mean() - base[i])
                       for i, pool in enumerate(pools)]
        means.append(float(np.mean(differences)) if differences else None)
        if (repeat + 1) % 50 == 0:
            progress(f'placebo={repeat + 1}/{PLACEBOS}')
    finite = [v for v in means if v is not None and math.isfinite(v)]
    percentiles = {arm: 100 * sum(v <= stats['delta'] for v in finite) / len(finite)
                   if finite and stats['delta'] is not None else None
                   for arm, stats in variants.items()}
    return dict(seed=SEED, repeats=PLACEBOS, n_days=len(days), mean_deltas=means,
                percentile_definition='100 × count(random mean Δ <= variant mean Δ) / valid repeats',
                percentile=percentiles)


def ledger_row(report, arm, k):
    stats = report['primary'][arm]
    return dict({key: stats[key] for key in STAT_FIELDS}, **verdict(stats, k), track=TRACK,
                stage='S1', change_type='E', idea_id=VARIANTS[arm][1],
                baseline_version=BASELINE_VERSION, trade_ratio=stats['trade_ratio'],
                placebo_pct=stats['placebo_pct'], input_hashes=report['input_hashes'],
                prereg_path=PREREG)


def dry_verdicts(report, rows):
    previous = list(rows)
    for arm in VARIANTS:
        k = evolution_ledger.next_k(previous, TRACK)
        row = ledger_row(report, arm, k)
        report['primary'][arm]['expected'] = dict(verdict=row['verdict'], K=row['K'],
                                                  threshold_K=k, z_K=row['z_K'])
        previous.append(row)


def record_report(report_path, ledger_path):
    report = json.loads(Path(report_path).read_text(encoding='utf-8'))
    if report['prereg_path'] != PREREG:
        raise ValueError('report does not belong to this preregistration')
    ledger_path = Path(ledger_path).resolve()
    data = Path(report['data_path']).resolve()
    if ledger_path == data or data in ledger_path.parents:
        raise ValueError('ledger must not be inside read-only data root')
    if ledger_path in (Path(report_path).resolve(), Path(report_path).with_suffix('.md').resolve()):
        raise ValueError('ledger and report paths must differ')

    def refuse_duplicates(previous):
        for row in previous:
            if (row.get('idea_id') in {item[1] for item in VARIANTS.values()} and
                    row.get('input_hashes', {}).get('daily_json') == report['input_hashes']['daily_json']):
                raise ValueError(f'duplicate idea_id and input hash already in ledger: {row["idea_id"]}')

    # Refuse a partially recorded pair as well; never silently append a duplicate.
    refuse_duplicates(evolution_ledger.read_rows(ledger_path))
    recorded = []
    for arm in VARIANTS:
        def build(k, previous, arm=arm):
            for row in previous:
                if (row.get('idea_id') == VARIANTS[arm][1] and
                        row.get('input_hashes', {}).get('daily_json') == report['input_hashes']['daily_json']):
                    raise ValueError(f'duplicate idea_id and input hash already in ledger: {row["idea_id"]}')
            return ledger_row(report, arm, k)
        recorded.append(evolution_ledger.append_event(ledger_path, TRACK, build))
    return recorded


def validate_outputs(data, out):
    data, out = Path(data).resolve(), Path(out).resolve()
    for path in (out, out / 'report.json', out / 'report.md'):
        path = path.resolve()
        if path == data or data in path.parents:
            raise ValueError('output must not be inside read-only data root')
        if path != REPO and REPO not in path.parents:
            raise ValueError('output must be inside this worktree')
    return out


def run_study(data, out, allow_input_drift=False, progress=print):
    started = time.monotonic()
    out = validate_outputs(data, out)
    actual, drift = guard_inputs(data, allow_input_drift)
    panels = load_panels(data, frozen=not drift)
    returns, selections, eligible = select_groups(panels)
    if not all(returns[arm].index.equals(returns['B'].index) for arm in VARIANTS):
        raise ValueError('primary variants must have the same eligible dates as baseline')
    progress(f'universe={panels["close"].shape[1]}; primary_days={len(returns["B"])}')
    primary = {arm: paired_diff(returns[arm], returns['B']) for arm in VARIANTS}
    turnovers = {arm: turnover(days) for arm, days in selections.items()}
    placebo = placebo_study(panels, eligible, returns['B'], primary, progress)
    pool_returns = panels['fwd20'].mean(axis=1)
    absolute = {arm: dict(mean_fwd20=float(sample.mean()) if len(sample) else None,
                           pool_mean_fwd20=float(pool_returns.reindex(sample.index).mean()) if len(sample) else None,
                           excess_vs_pool=float((sample - pool_returns).dropna().mean()) if len(sample) else None,
                           n_days=len(sample), date_range=date_range(sample))
                for arm, sample in returns.items()}
    for arm, stats in primary.items():
        base_turn, variant_turn = turnovers['B']['mean'], turnovers[arm]['mean']
        stats['trade_ratio'] = (variant_turn / base_turn if base_turn else
                                (math.inf if variant_turn else None))
        stats['placebo_pct'] = placebo['percentile'][arm]
    no_sma_returns, _, no_sma_eligible = select_groups(panels, use_sma=False)
    no_sma = {arm: paired_diff(no_sma_returns[arm], no_sma_returns['B']) for arm in VARIANTS}
    deviations = [
        '辅助 RankIC 未规定最小截面数量：只要求至少 2 个有限信号/标签配对，常数秩截面无 IC；信号按原始定义，不反转符号。',
        '辅助无 SMA200 对比保留 mom12_1、fwd20 与合格池至少 10 只的限制；无值信号不参与该组排序，不足 5 只时该组当日无值。',
        '安慰剂使用 numpy.default_rng(20261105)，按重复序号、日期升序、代码字母序从合格池无放回抽 5 只；分位为随机均值小于等于变体均值的比例，单位 %。',
        '换手按相邻保留日期计算 1−交集数量/5，首日不计建仓；跨跳过日期比较相邻可评估组合。',
        '辅助币池等权基准为当日有 fwd20 的全部个股（无动量/SMA200 限制），先截面等权再对各组相同日期等权平均。',
        'NW 沿用 nw_t 的短样本与零方差约定：少于 20 日时 t=0、SE 无值；方差≤1e−30 时非零均值的 z 饱和为 ±1e12，SE=|均值|/1e12；零均值时 z=0、SE=0。',
    ]
    if drift:
        deviations.append('显式 --allow-input-drift 允许输入哈希漂移；使用实际文件扣除 11 个排除代码，结果不再是原冻结输入研究。')
    rows = evolution_ledger.read_rows(DEFAULT_LEDGER)
    report = dict(prereg_path=PREREG, prereg_sha256=hashlib.sha256((REPO / PREREG).read_bytes()).hexdigest(),
                  data_path=str(Path(data).resolve()), input_hashes=dict(daily_json=actual),
                  expected_input_sha256=EXPECTED_HASH, allow_input_drift=allow_input_drift, input_drift=drift,
                  universe=dict(size=panels['close'].shape[1], symbols=list(panels['close'].columns),
                                excluded=list(EXCLUDED), survivors_only=True),
                  calendar=dict(source='SPY', n_days=len(panels['close']),
                                date_range=date_range(panels['close'])),
                  parameters=dict(top_n=N, min_eligible=MIN_ELIGIBLE, holding_bars=20, nw_lag=LAG,
                                  start=START, end=END, seed=SEED, placebo_repeats=PLACEBOS),
                  ledger=dict(path=str(DEFAULT_LEDGER), wrote=False, track=TRACK,
                              existing_count=sum(r.get('track') == TRACK and evolution_ledger.counts_toward_k(r)
                                                 for r in rows)),
                  primary=primary,
                  secondary=dict(rank_ic=rank_ic_study(panels), no_sma200=no_sma, placebo=placebo,
                                 turnover=turnovers, absolute_returns=absolute),
                  audit=dict(eligible_counts={str(d.date()): int(v) for d, v in eligible.sum(axis=1).items()},
                             no_sma_eligible_counts={str(d.date()): int(v) for d, v in no_sma_eligible.sum(axis=1).items()},
                             selections={arm: {str(d.date()): members for d, members in days.items()}
                                         for arm, days in selections.items()},
                             daily_returns={arm: {str(d.date()): float(v) for d, v in sample.items()}
                                            for arm, sample in returns.items()}),
                  deviations=deviations)
    dry_verdicts(report, rows)
    if guard_inputs(data, True)[0] != actual:
        raise ValueError('inputs changed while loading/running')
    report['elapsed_seconds'] = time.monotonic() - started
    out.mkdir(parents=True, exist_ok=True)
    (out / 'report.json').write_text(json.dumps(evolution_ledger.json_safe(report), ensure_ascii=False,
                                               indent=2, allow_nan=False) + '\n', encoding='utf-8')
    (out / 'report.md').write_text(render_report(report), encoding='utf-8')
    return report


def number(value, digits=6):
    return 'NA' if value is None else f'{value:.{digits}f}'


def render_report(report):
    lines = ['# 美股区间位置 / 6 个月反转：S1 预注册结果', '',
             '**本研究为幸存者股票池：仅含 2026-11 仍在 Binance 的 bStock 个股，不代表全市场。**', '',
             f'币池：{report["universe"]["size"]} 只个股；排除 11 个 ETF/杠杆或反向 ETF，SPY 仅作日历。',
             f'主日历：{report["calendar"]["date_range"]}，{report["calendar"]["n_days"]} 日；所有窗口/前向收益按个股自己的 bar 序号。',
             f'输入 sha256：`{report["input_hashes"]["daily_json"]}`；允许漂移：{report["allow_input_drift"]}；实际漂移：{bool(report["input_drift"])}。',
             f'预注册：`{PREREG}`；研究运行耗时：{report["elapsed_seconds"]:.3f} 秒。', '',
             '## 主检验（预期判定）', '',
             '以下 K / z_K / verdict 均为只读账本的**预期**值；最终判定由 record 在 append_event 锁内分配 K 后计算。',
             f'账本：`{report["ledger"]["path"]}`；track=us_llm 已计数 {report["ledger"]["existing_count"]} 条；本次未写账本。',
             'Δ 是每日选出的前 5 等权 fwd20 原始收益差，不是单日策略收益；不计成本。SE 为 Bartlett Newey-West，lag=19。', '',
             '| 变体 | 起日 → 止日 | n_days | mean Δ | NW SE | z | 前半 Δ | 后半 Δ | 预期 K | 预期 z_K | 预期 verdict |',
             '| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |']
    for arm, stats in report['primary'].items():
        expected = stats['expected']
        lines.append(f'| {arm} {VARIANTS[arm][0]} | {" → ".join(str(d) for d in stats["date_range"])} | '
                     f'{stats["n_days"]} | {number(stats["delta"])} | {number(stats["se"])} | {number(stats["z"], 4)} | '
                     f'{number(stats["dev_delta"])} | {number(stats["holdout_delta"])} | {expected["K"]} | '
                     f'{number(expected["z_K"], 4)} | {expected["verdict"]} |')
    lines += ['', '按日期前 ceil(n/2) 日为前半，其余为后半：']
    for arm, stats in report['primary'].items():
        lines.append(f'- {arm}：前半 {stats["halves"]["dev"]}；后半 {stats["halves"]["holdout"]}。')
    lines += ['', '判定顺序：n_days<100 → invalid；z≥z_K 且两半均>0 → strong；z≥1 且两半均>0 → screen_pass；其余 fail。',
              'z_K = Φ⁻¹(1−0.05/(2K))；V1 先于 V2。通过仅可进入线上配对队列，不修改扫描排序。', '',
              '## 辅助 1：全币池每日截面 Spearman RankIC', '',
              '| 原始信号 | 前向 bar 数 | 日数 | RankIC 均值 | NW t | lag |',
              '| --- | ---: | ---: | ---: | ---: | ---: |']
    for signal, horizons in report['secondary']['rank_ic'].items():
        for h, stats in horizons.items():
            lines.append(f'| {signal} | {h} | {stats["n_days"]} | {number(stats["mean"])} | {number(stats["nw_t"], 4)} | {stats["lag"]} |')
    lines += ['', '此项不加合格池限制；只保留信号与对应 fwd_h 均有值的个股。', '',
              '## 辅助 2：去掉 SMA200 条件', '',
              '| 变体 | 起日 → 止日 | n_days | mean Δ | NW SE | z | 前半 Δ | 后半 Δ |',
              '| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |']
    for arm, stats in report['secondary']['no_sma200'].items():
        lines.append(f'| {arm} | {" → ".join(str(d) for d in stats["date_range"])} | {stats["n_days"]} | '
                     f'{number(stats["delta"])} | {number(stats["se"])} | {number(stats["z"], 4)} | '
                     f'{number(stats["dev_delta"])} | {number(stats["holdout_delta"])} |')
    placebo = report['secondary']['placebo']
    lines += ['', '## 辅助 3：随机前 5 安慰剂', '',
              f'固定种子 {SEED}，重复 {PLACEBOS} 次，每日从同一合格池无放回抽 5 只；分位=100×count(随机 mean Δ≤变体 mean Δ)/200。']
    for arm, pct in placebo['percentile'].items():
        lines.append(f'- {arm} 的 mean Δ 在安慰剂中的分位：{number(pct, 2)}%。')
    finite = [v for v in placebo['mean_deltas'] if v is not None]
    if finite:
        lines.append(f'随机 mean Δ：均值 {number(float(np.mean(finite)))}，最小 {number(min(finite))}，最大 {number(max(finite))}。200 个均值保存在 report.json。')
    lines += ['', '## 辅助 4：相邻日期前 5 成员换手', '',
              '| 组 | 平均日换手 | 日期转换数 | 变体 / B 换手比 |', '| --- | ---: | ---: | ---: |']
    for arm, stats in report['secondary']['turnover'].items():
        ratio = report['primary'][arm]['trade_ratio'] if arm in VARIANTS else 1.
        lines.append(f'| {arm} | {number(stats["mean"])} | {stats["n_transitions"]} | {number(ratio, 4)} |')
    lines += ['', '## 辅助 5：fwd20 绝对收益与币池等权差', '',
              '| 组 | 前 5 fwd20 均值 | 同日全币池等权均值 | 超额均值 |', '| --- | ---: | ---: | ---: |']
    for arm, stats in report['secondary']['absolute_returns'].items():
        lines.append(f'| {arm} | {number(stats["mean_fwd20"])} | {number(stats["pool_mean_fwd20"])} | {number(stats["excess_vs_pool"])} |')
    lines += ['', '## 偏离与解释（deviations）', '']
    lines += [f'- {item}' for item in report['deviations']]
    return '\n'.join(lines) + '\n'


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    run = commands.add_parser('run', help='compute reports without ledger writes')
    run.add_argument('--data', type=Path, required=True)
    run.add_argument('--out', type=Path, required=True)
    run.add_argument('--allow-input-drift', action='store_true')
    record = commands.add_parser('record', help='append V1 then V2 with locked K and duplicate refusal')
    record.add_argument('--report', type=Path, required=True)
    record.add_argument('--ledger', type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == 'record':
            rows = record_report(args.report, args.ledger)
            for row in rows:
                print(f'{row["idea_id"]}: verdict={row["verdict"]}; K={row["K"]}; z_K={row["z_K"]:.6f}')
        else:
            result = run_study(args.data, args.out, args.allow_input_drift,
                               progress=lambda s: print(s, file=sys.stderr, flush=True))
            for arm, stats in result['primary'].items():
                expected = stats['expected']
                print(f'{arm}: expected_verdict={expected["verdict"]}; expected_K={expected["K"]}; '
                      f'delta={number(stats["delta"])}; z={number(stats["z"], 4)}')
            print(f'wall_seconds={result["elapsed_seconds"]:.3f}; reports: {args.out / "report.md"}, {args.out / "report.json"}')
    except (ValueError, OSError, KeyError) as error:
        parser.error(str(error))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
