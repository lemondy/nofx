"""Run the frozen 2026-11-05 US broad-universe classic-factor S1 study."""
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import sys
import time

import numpy as np
import pandas as pd

from . import evaluate
from scripts.backtest import evolution_ledger

REPO = Path(__file__).resolve().parents[2]
PREREG = 'docs/research/2026-11-05-us-broad-classic-factors-prereg.md'
IDEA = 'us-broad-classic-v1'
EXPECTED_HASHES = {
    'broad_constituents': 'a7b7f254b863da613ec2f174fefdb1c74ba3648a2525499178cefcfd7c42813c',
    'broad_daily': '2e621c842446cb82ddb1f6dd8744b333bacd8817f0e6ef4ff39543a8f9fc4814',
    'spy': 'd3da8ca2f80491181d0ea513e6888c1a509766da56089895c71d0174c4d421e3',
    'bstock_daily': '67b64d0577f9d24ebd6908e51f17d85ec3559e6f77064b736d02b99d2337d82c',
    'bstock_instrument_types': '44a9695513d538f8a46bc73d1def61afc218723d5b706fd76f62ab06386a57be',
}
FACTORS = dict(F1='rev21', F2='rev5', F3='mom12_1', F4='mom6_1', F5='lowvol63',
               F6='ivol63', F7='high52', F8='max21', F9='bab', F10='amihud')
HORIZONS = (5, 10, 20)
MIN_NAMES = 30
ONE_WAY_COST = .001
PERIODS = dict(train=('2012-11-01', '2021-12-31'),
               val=('2022-01-01', '2024-12-31'), forward=('2025-01-01', None))
DEVIATIONS = [
    '预注册未指定缺日时的交易日轴：统一使用 SPY 日期；缺日保留 NaN，不补价、不压缩个股时间轴。'
    '所有滚动窗口要求窗口内完整观测；有效日线指 OHLC 有限且为正、成交量有限且非负。',
    '预注册未指定标准差自由度：F5/F6 使用样本标准差（ddof=1）；F6 在当期同一 63 日窗口内'
    '以含截距 OLS 的全部残差计算标准差，非逐日不同回归的末端残差串；SPY 零方差时 OLS 不可识别，置 NaN。',
    '预注册未指定两半与正月份口径：各段有效 IC 按日期排序，前 floor(n/2) 日为前半，余日为后半；'
    '正月份占比为月均 IC>0 的有效月份数/至少一个有效 IC 的月份数。',
    '严格按信号日期划分训练/验证/前向段；预注册未要求边界清洗，故不额外删去跨段的 t+h 标签。'
    '前 252 根日线通过累计有效日线数>=253 的时点可用性条件预热。',
    '辅助多空未指定调仓相位、余数、权重与终止成本：每段首个 SPY 日期起每 h 日调仓，'
    '暖机或不足 30 只的日期跳过但不移动相位；稳定按符号顺序破同值，五组余数从最低组起分配。'
    '最高/最低组分别等权 +1/-1，按价格漂移后的实际权重与新目标权重之差的绝对成交额收取单边 10bp；'
    '首期开仓、末期平仓均收费。报每期及复合收益，不年化。选股只用当期信息；'
    '已选股票若缺 t+h 价格，该期收益置为不可观测并单列，不按未来价格重选股票。',
    '辅助相关矩阵未指定口径：宽池通过的 (因子,h) 按因子去重，在宽池前向段取逐日截面 Spearman '
    '相关的时间均值，每对至少 30 只有效股票；另报每对有效日数，不参与门槛。',
    '账本接口额外要求 baseline_version；本研究无配对基线，填写 no_baseline。K 由 append_event 锁内计算，'
    '不改变固定的 30 次检验及训练阈值 3.14。',
    '沿用 evaluate.nw_t 的 Bartlett 核及自协方差除以 n 的口径；其少于 20 日直接返回 0 的限制'
    '未在预注册中规定，故短序列用相同公式直接计算；零长期方差时沿用其有限大值约定。'
    '每个持有期显式传入 lag=h-1。',
]


def concatenated_sha256(paths):
    """Hash raw file bytes, with no separators or intermediate hex digests."""
    paths = sorted(paths, key=lambda p: p.name)
    if not paths:
        raise ValueError('daily input directory contains no JSON files')
    digest = hashlib.sha256()
    for path in paths:
        with path.open('rb') as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b''):
                digest.update(chunk)
    return digest.hexdigest()


def guard_inputs(broad_root, bstock_root, allow_input_drift=False):
    broad, bstock = Path(broad_root), Path(bstock_root)
    actual = {
        'broad_constituents': concatenated_sha256([broad / 'constituents_20261105.json']),
        'broad_daily': concatenated_sha256((broad / 'daily').glob('*.json')),
        'spy': concatenated_sha256([broad / 'benchmark/SPY.json']),
        'bstock_daily': concatenated_sha256((bstock / 'daily').glob('*.json')),
        'bstock_instrument_types': concatenated_sha256([broad / 'bstock_instrument_types_20261105.json']),
    }
    drift = {key: dict(expected=EXPECTED_HASHES[key], actual=value)
             for key, value in actual.items() if value != EXPECTED_HASHES[key]}
    if drift and not allow_input_drift:
        raise ValueError(f'frozen input sha256 mismatch: {drift}; --allow-input-drift required')
    return actual, drift


def read_daily(path):
    frame = pd.DataFrame(json.loads(Path(path).read_text(encoding='utf-8')),
                         columns=['date', 'open', 'high', 'low', 'close', 'volume', 'typical'])
    frame['date'] = pd.to_datetime(frame['date'], errors='raise')
    frame = frame.set_index('date').sort_index()
    if frame.empty or frame.index.has_duplicates:
        raise ValueError(f'empty daily input or duplicate dates: {path}')
    prices = frame[['open', 'high', 'low', 'close']].astype(float)
    volume = frame['volume'].astype(float)
    valid = (np.isfinite(prices).all(axis=1) & prices.gt(0).all(axis=1) &
             np.isfinite(volume) & volume.ge(0))
    return pd.DataFrame(dict(close=prices['close'].where(valid), volume=volume.where(valid)))


def load_universes(broad_root, bstock_root, frozen=True):
    broad, bstock = Path(broad_root), Path(bstock_root)
    members = json.loads((broad / 'constituents_20261105.json').read_text())['sp500']
    members = {symbol.replace('.', '-') for symbol in members}
    broad_files = {p.stem: p for p in sorted((broad / 'daily').glob('*.json'))}
    if not set(broad_files) <= members:
        raise ValueError('broad daily files contain symbols outside the registered SP500 universe')
    types = json.loads((broad / 'bstock_instrument_types_20261105.json').read_text())
    bstock_files = {p.stem: p for p in sorted((bstock / 'daily').glob('*.json'))}
    if set(bstock_files) - types.keys():
        raise ValueError('bStock daily symbol has no registered instrumentType')
    transfer_files = {symbol: path for symbol, path in bstock_files.items() if types[symbol] == 'EQUITY'}
    excluded = sorted(set(bstock_files) - transfer_files.keys())
    if frozen and (len(broad_files) != 498 or len(transfer_files) != 90 or len(excluded) != 11):
        raise ValueError('frozen universe counts disagree with preregistration (498 / 90 / 11)')
    spy = read_daily(broad / 'benchmark/SPY.json')['close']
    calendar = spy.index
    universes = {}
    for name, files in (('U', broad_files), ('T', transfer_files)):
        frames = {symbol: read_daily(path) for symbol, path in files.items()}
        # Reindex explicitly; pandas must not compress a missing stock date.
        close = pd.DataFrame({s: f['close'] for s, f in frames.items()}).reindex(calendar)
        volume = pd.DataFrame({s: f['volume'] for s, f in frames.items()}).reindex(calendar)
        universes[name] = dict(close=close, volume=volume)
    return universes, spy, dict(broad_symbols=sorted(broad_files),
                               missing_constituents=sorted(members - broad_files.keys()),
                               transfer_symbols=sorted(transfer_files), excluded_bstock_symbols=excluded)


def eligibility_mask(close, volume):
    valid = np.isfinite(close) & close.gt(0) & np.isfinite(volume) & volume.ge(0)
    dollars = (close * volume).where(valid)
    return (valid & valid.cumsum().ge(253) & close.ge(5) &
            dollars.rolling(20, min_periods=20).median().ge(10_000_000))


def rolling_ols(returns, spy_returns, window):
    """Return same-window intercept OLS beta and sample residual volatility."""
    x = spy_returns.reindex(returns.index).to_numpy(dtype=float)[:, None]
    y = returns.to_numpy(dtype=float)
    valid = np.isfinite(x) & np.isfinite(y)
    xx = pd.DataFrame(np.where(valid, x, np.nan), index=returns.index, columns=returns.columns)
    yy = returns.where(valid)
    # Shared pairwise masks guarantee all moments use the same complete window.
    variance_x = xx.rolling(window, min_periods=window).var(ddof=1)
    variance_y = yy.rolling(window, min_periods=window).var(ddof=1)
    covariance = yy.rolling(window, min_periods=window).cov(xx, ddof=1)
    beta = covariance.div(variance_x.where(variance_x.gt(0)))
    residual_variance = (variance_y - beta * covariance).clip(lower=0)
    return beta, np.sqrt(residual_variance)


def compute_factors(close, volume, spy_close):
    returns = close.pct_change(fill_method=None)
    spy_returns = spy_close.reindex(close.index).pct_change(fill_method=None)
    _, ivol = rolling_ols(returns, spy_returns, 63)
    beta, _ = rolling_ols(returns, spy_returns, 252)
    dollars = (close * volume).where((close * volume).gt(0))
    amihud = returns.abs().div(dollars).rolling(21, min_periods=21).mean()
    factors = dict(
        F1=-(close / close.shift(21) - 1),
        F2=-(close / close.shift(5) - 1),
        F3=close.shift(21) / close.shift(252) - 1,
        F4=close.shift(21) / close.shift(126) - 1,
        F5=-returns.rolling(63, min_periods=63).std(ddof=1),
        F6=-ivol,
        F7=close / close.rolling(252, min_periods=252).max(),
        F8=-returns.rolling(21, min_periods=21).max(),
        F9=-beta,
        F10=np.log(amihud.where(amihud.gt(0))),
    )
    return {key: frame.where(np.isfinite(frame)) for key, frame in factors.items()}


def forward_label(close, h):
    return close.shift(-h) / close - 1


def segment_masks(dates, h):
    complete = np.arange(len(dates)) + h < len(dates)
    return {name: pd.Series((dates >= pd.Timestamp(start)) &
                            ((dates <= pd.Timestamp(end)) if end else True) & complete, index=dates)
            for name, (start, end) in PERIODS.items()}


def nw_t(series, lag):
    """Reuse the evaluator without adding its unregistered 20-day minimum."""
    x = np.asarray(series, dtype=float)
    x = x[np.isfinite(x)]
    n = len(x)
    if n >= 20:
        return evaluate.nw_t(x, lag)
    if not n:
        return 0.
    mean = float(x.mean())
    centered = x - mean
    variance = float(np.dot(centered, centered) / n)
    for k in range(1, min(lag, n - 1) + 1):
        variance += 2 * (1 - k / (lag + 1)) * float(np.dot(centered[k:], centered[:-k]) / n)
    if variance <= 1e-30:
        return float(np.copysign(1e12, mean)) if mean else 0.
    return float(mean / np.sqrt(variance / n))


def ic_summary(ic, h):
    sample = ic.replace([np.inf, -np.inf], np.nan).dropna().sort_index()
    middle = len(sample) // 2
    halves = (sample.iloc[:middle], sample.iloc[middle:])
    monthly = sample.groupby(sample.index.to_period('M')).mean()
    return dict(ic=float(sample.mean()) if len(sample) else None,
                t=nw_t(sample, h - 1), nw_lag=h - 1, n_days=len(sample),
                start=str(sample.index[0].date()) if len(sample) else None,
                end=str(sample.index[-1].date()) if len(sample) else None,
                ic_halves=[float(x.mean()) if len(x) else None for x in halves],
                half_n_days=[len(x) for x in halves],
                half_ranges=[[str(x.index[0].date()), str(x.index[-1].date())] if len(x) else []
                             for x in halves],
                positive_month_share=float(monthly.gt(0).mean()) if len(monthly) else None,
                n_months=len(monthly), monthly_ic={str(d): float(v) for d, v in monthly.items()},
                daily_ic={str(d.date()): float(v) for d, v in sample.items()})


def gates(broad, transfer):
    def positive(value):
        return value is not None and np.isfinite(value) and value > 0

    train, val, forward = (broad[key] for key in ('train', 'val', 'forward'))
    g1 = (positive(train['ic']) and train['t'] >= 3.14 and
          all(positive(value) for value in train['ic_halves']))
    g2 = positive(val['ic']) and val['t'] >= 1.65
    g3 = positive(forward['ic'])
    g4 = positive(transfer['forward']['ic']) and transfer['forward']['t'] >= 1.
    return dict(G1=bool(g1), G2=bool(g2), G3=bool(g3), G4=bool(g4),
                broad_pass=bool(g1 and g2 and g3), transfer_pass=bool(g1 and g2 and g3 and g4))


def quintile_returns(values, labels, mask, segment, h):
    """Nonoverlapping close-to-close sleeves, with actual notional trading costs."""
    positions = np.flatnonzero(np.asarray(segment, dtype=bool))[::h]
    score, raw, eligible = values.to_numpy(), labels.to_numpy(), mask.to_numpy()
    records, missing = [], []
    previous_end = np.zeros(values.shape[1])
    previous_exit = None

    def liquidate():
        if records and records[-1]['exit_cost'] == 0:
            last = records[-1]
            last['exit_cost'] = ONE_WAY_COST * last.pop('end_notional')
            last['net_return'] -= last['exit_cost']

    for position in positions:
        valid = eligible[position] & np.isfinite(score[position])
        if valid.sum() < MIN_NAMES:
            continue
        names = np.flatnonzero(valid)
        order = names[np.argsort(score[position, names], kind='stable')]
        groups = np.array_split(order, 5)
        short, long = groups[0], groups[-1]
        selected = np.r_[short, long]
        if not np.isfinite(raw[position, selected]).all():
            missing.append(str(values.index[position].date()))
            continue
        if previous_exit != position:
            liquidate()
            previous_end = np.zeros(values.shape[1])
        weights = np.zeros(values.shape[1])
        weights[long], weights[short] = 1 / len(long), -1 / len(short)
        selected_returns = np.where(np.isfinite(raw[position]), raw[position], 0.)
        gross = float(np.dot(weights, selected_returns))
        turnover = float(np.abs(weights - previous_end).sum())
        entry_cost = ONE_WAY_COST * turnover
        net = gross - entry_cost
        end_weights = weights * (1 + selected_returns)
        if records:
            records[-1].pop('end_notional', None)
        records.append(dict(date=str(values.index[position].date()),
                            exit_date=str(values.index[position + h].date()),
                            n_long=len(long), n_short=len(short),
                            long_symbols=[str(values.columns[i]) for i in long],
                            short_symbols=[str(values.columns[i]) for i in short],
                            long_return=float(raw[position, long].mean()),
                            short_return=float(raw[position, short].mean()),
                            gross_return=gross, net_return=net, entry_turnover=turnover,
                            entry_cost=entry_cost, exit_cost=0.,
                            end_notional=float(np.abs(end_weights).sum())))
        previous_end = end_weights / (1 + net) if net > -1 else np.zeros_like(weights)
        previous_exit = position + h if net > -1 else None
    liquidate()
    gross = np.array([row['gross_return'] for row in records])
    net = np.array([row['net_return'] for row in records])
    return dict(n_periods=len(records), missing_endpoint_dates=missing,
                gross_mean=float(gross.mean()) if len(gross) else None,
                net_mean=float(net.mean()) if len(net) else None,
                gross_cumulative=float(np.prod(1 + gross) - 1) if len(gross) else None,
                net_cumulative=float(np.prod(1 + net) - 1) if len(net) else None,
                bankrupt=bool((net <= -1).any()), one_way_cost=ONE_WAY_COST, periods=records)


def passing_correlations(factors, mask, results):
    ids = [factor for factor in FACTORS if any(row['factor'] == factor and row['gates']['broad_pass']
                                               for row in results)]
    forward = mask.index >= pd.Timestamp(PERIODS['forward'][0])
    matrix, counts = {key: {} for key in ids}, {key: {} for key in ids}
    for i, left in enumerate(ids):
        for right in ids[i:]:
            daily = evaluate.cs_rank_ic(factors[left].loc[forward], factors[right].loc[forward],
                                        mask.loc[forward], MIN_NAMES).dropna()
            value = float(daily.mean()) if len(daily) else None
            matrix[left][right] = matrix[right][left] = value
            counts[left][right] = counts[right][left] = len(daily)
    return dict(factor_ids=ids, matrix=matrix, n_days=counts,
                universe='U', segment='forward', method='mean_daily_cross_sectional_spearman')


def ledger_result(results, hashes, ledger_path, drift, allow_input_drift):
    passing = lambda key: [dict(factor=row['factor'], h=row['h']) for row in results if row['gates'][key]]
    broad, transfer = passing('broad_pass'), passing('transfer_pass')

    def build(_k, _previous):
        return dict(track='fq_us', stage='S1', change_type='E', idea_id=IDEA,
                    baseline_version='no_baseline', verdict='screen_pass' if transfer else 'fail',
                    n_pass_broad=len(broad), n_pass_transfer=len(transfer),
                    passing_broad=broad, passing_transfer=transfer, input_hashes=hashes,
                    prereg_path=PREREG, input_drift=drift, allow_input_drift=allow_input_drift)

    if ledger_path is not None:
        return evolution_ledger.append_event(ledger_path, 'fq_us', build)
    return build(None, [])


def validate_outputs(broad_root, bstock_root, out, ledger_path):
    out = Path(out).resolve()
    reports = [(out / name).resolve() for name in ('report.json', 'report.md')]
    outputs = [out, *reports]
    if ledger_path is not None:
        ledger = Path(ledger_path).resolve()
        if ledger == out or ledger in reports:
            raise ValueError('ledger and report paths must differ')
        outputs.append(ledger)
    roots = [Path(p).resolve() for p in (broad_root, bstock_root)]
    for path in outputs:
        if any(path == root or root in path.parents for root in roots):
            raise ValueError(f'output must not be inside read-only data roots: {path}')
    return out


def run_study(broad_root, bstock_root, out, ledger_path=None, allow_input_drift=False, progress=print):
    started = time.monotonic()
    validate_outputs(broad_root, bstock_root, out, ledger_path)
    hashes, drift = guard_inputs(broad_root, bstock_root, allow_input_drift)
    universes, spy, composition = load_universes(broad_root, bstock_root, frozen=not drift)
    progress(f'inputs verified; U={universes["U"]["close"].shape}; T={universes["T"]["close"].shape}')
    for name, data in universes.items():
        data['mask'] = eligibility_mask(data['close'], data['volume'])
        data['factors'] = compute_factors(data['close'], data['volume'], spy)
        progress(f'{name}: factors and per-date eligibility computed')
    results = []
    for factor, factor_name in FACTORS.items():
        for h in HORIZONS:
            row = dict(factor=factor, name=factor_name, h=h)
            for name, data in universes.items():
                labels = forward_label(data['close'], h)
                masks = segment_masks(data['close'].index, h)
                values = data['factors'][factor]
                ic = evaluate.cs_rank_ic(values, labels, data['mask'], MIN_NAMES)
                statistics = {seg: ic_summary(ic.loc[visible], h) for seg, visible in masks.items()}
                ls_segments = masks if name == 'U' else {'forward': masks['forward']}
                row['broad' if name == 'U' else 'transfer'] = statistics
                row['broad_ls' if name == 'U' else 'transfer_ls'] = {
                    seg: quintile_returns(values, labels, data['mask'], visible, h)
                    for seg, visible in ls_segments.items()}
            row['gates'] = gates(row['broad'], row['transfer'])
            results.append(row)
        progress(f'{factor} {factor_name}: all three fixed horizons evaluated')
    correlations = passing_correlations(universes['U']['factors'], universes['U']['mask'], results)
    if guard_inputs(broad_root, bstock_root, allow_input_drift)[0] != hashes:
        raise ValueError('inputs changed during study')
    row = ledger_result(results, hashes, ledger_path, drift, allow_input_drift)
    deviations = list(DEVIATIONS)
    if drift:
        deviations.append('显式允许冻结输入漂移；详见 input_drift，此结果不再是原冻结输入研究。')
    eligibility = {}
    for name, data in universes.items():
        count = data['mask'].sum(axis=1)
        eligibility[name] = dict(n_symbols=data['close'].shape[1],
                                 daily_eligible={str(d.date()): int(v) for d, v in count.items()})
    return dict(idea_id=IDEA, prereg_path=PREREG,
                prereg_sha256=hashlib.sha256((REPO / PREREG).read_bytes()).hexdigest(),
                inputs=dict(broad_root=str(Path(broad_root).resolve()), bstock_root=str(Path(bstock_root).resolve())),
                input_hashes=hashes, expected_hashes=EXPECTED_HASHES,
                input_drift=drift, allow_input_drift=allow_input_drift,
                parameters=dict(horizons=list(HORIZONS), n_tests=30, min_names=MIN_NAMES,
                                nw_lag='h-1', train_t=3.14, val_t=1.65, transfer_t=1.,
                                history=253, min_close=5, liquidity_window=20,
                                min_median_dollar_volume=10_000_000, one_way_cost=ONE_WAY_COST,
                                periods=PERIODS),
                composition=composition, eligibility=eligibility, results=results,
                passing_correlations=correlations,
                verdict=row['verdict'], n_pass_broad=row['n_pass_broad'], n_pass_transfer=row['n_pass_transfer'],
                passing_broad=row['passing_broad'], passing_transfer=row['passing_transfer'],
                ledger=dict(path=str(ledger_path) if ledger_path is not None else None,
                            written=ledger_path is not None, row=row),
                deviations=deviations, elapsed_seconds=time.monotonic() - started)


def number(value, digits=4):
    return 'NA' if value is None else f'{value:.{digits}f}'


def render_report(result):
    lines = ['# 美股宽池经典因子：预注册 S1 研究', '',
             f'结论：**{result["verdict"]}**；宽池通过 {result["n_pass_broad"]}/30，'
             f'可迁移通过 {result["n_pass_transfer"]}/30。', '',
             f'预注册：`{PREREG}`；运行耗时：{result["elapsed_seconds"]:.3f} 秒。', '',
             '**宽池采用当前成分股，存在幸存者偏差，结论可能偏乐观；通过仅表示进入下一步单独评审。**', '',
             '## 三段与迁移检验', '',
             '每日 RankIC 至少 30 只；NW 滞后 h−1。训练 t≥3.14 且前后半 IC>0，验证 t≥1.65，'
             '宽池前向 IC>0；迁移前向 IC>0 且 t≥1.0。所有门槛均按未舍入数值判定。', '',
             '| 因子 | h | 训练 IC / t / 日数 | 验证 IC / t / 日数 | 前向 IC / t / 日数 | '
             '迁移前向 IC / t / 日数 | G1/G2/G3/G4 | 宽池 / 迁移 |',
             '| --- | ---: | --- | --- | --- | --- | --- | --- |']
    def stats(cell):
        return f'{number(cell["ic"])} / {number(cell["t"], 3)} / {cell["n_days"]}'

    for row in result['results']:
        cells = [row['factor'] + ' ' + row['name'], str(row['h'])]
        cells += [stats(row['broad'][seg]) for seg in PERIODS]
        cells += [stats(row['transfer']['forward']),
                  '/'.join('✓' if row['gates'][g] else '×' for g in ('G1', 'G2', 'G3', 'G4')),
                  ' / '.join('✓' if row['gates'][g] else '×' for g in ('broad_pass', 'transfer_pass'))]
        lines.append('| ' + ' | '.join(cells) + ' |')
    lines += ['', '## 稳定性与多空收益（辅助，不参与门槛）', '',
              '正月占比按月均 IC；多空为最高五分位减最低五分位，单边成本 10bp，收益为复合总收益。'
              '完整三段多空、IC 半段、月度及每日数据和迁移训练/验证指标见 report.json。', '',
              '| 因子 / h | 训练前半 / 后半 IC | 宽池正月占比（训/验/前） | '
              '宽池前向多空（毛/净） | 迁移前向多空（毛/净） |',
              '| --- | --- | --- | --- | --- |']
    for row in result['results']:
        halves = ' / '.join(number(v) for v in row['broad']['train']['ic_halves'])
        months = ' / '.join(number(row['broad'][seg]['positive_month_share'], 3) for seg in PERIODS)
        returns = [' / '.join(number(row[key]['forward'][metric]) for metric in ('gross_cumulative', 'net_cumulative'))
                   for key in ('broad_ls', 'transfer_ls')]
        lines.append('| ' + ' | '.join([f'{row["factor"]} / {row["h"]}', halves, months, *returns]) + ' |')
    lines += ['', f'宽池通过：{result["passing_broad"]}。', f'可迁移通过：{result["passing_transfer"]}。', '',
              '## 通过因子相关矩阵', '']
    corr = result['passing_correlations']
    ids = corr['factor_ids']
    if ids:
        lines += ['| 因子 | ' + ' | '.join(ids) + ' |', '| --- | ' + ' | '.join(['---:'] * len(ids)) + ' |']
        lines += ['| ' + left + ' | ' + ' | '.join(number(corr['matrix'][left][right]) for right in ids) + ' |'
                  for left in ids]
    else:
        lines += ['无宽池通过因子，矩阵为空。']
    lines += ['', '## 输入与账本', '']
    for name, digest in result['input_hashes'].items():
        lines.append(f'- {name}：`{digest}`。')
    lines += [f'- allow_input_drift={result["allow_input_drift"]}；input_drift={result["input_drift"]}。',
              f'- 账本：{result["ledger"]["path"] or "未写入（--no-ledger）"}。', '',
              '## deviations（偏离与歧义解释）', '']
    lines += ['- ' + item for item in result['deviations']]
    return '\n'.join(lines) + '\n'


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--broad-root', type=Path, required=True)
    parser.add_argument('--bstock-root', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    ledger = parser.add_mutually_exclusive_group()
    ledger.add_argument('--ledger', type=Path, default=REPO / evolution_ledger.DEFAULT_PATH)
    ledger.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--allow-input-drift', action='store_true')
    args = parser.parse_args(argv)
    ledger_path = None if args.no_ledger else args.ledger.resolve()
    try:
        out = validate_outputs(args.broad_root, args.bstock_root, args.out, ledger_path)
        result = run_study(args.broad_root, args.bstock_root, out, ledger_path, args.allow_input_drift,
                           progress=lambda message: print(message, file=sys.stderr, flush=True))
    except (ValueError, FileNotFoundError) as error:
        parser.error(str(error))
    out.mkdir(parents=True, exist_ok=True)
    (out / 'report.json').write_text(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + '\n',
                                    encoding='utf-8')
    (out / 'report.md').write_text(render_report(result), encoding='utf-8')
    print(f'verdict={result["verdict"]}; n_pass_broad={result["n_pass_broad"]}; '
          f'n_pass_transfer={result["n_pass_transfer"]}; wall_seconds={result["elapsed_seconds"]:.3f}')
    print(f'reports: {out / "report.json"}, {out / "report.md"}')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
