"""Offline runner for the frozen 2026-11-06 US fundamental-factor S1 study."""
from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import sys
import time

import numpy as np
import pandas as pd

from scripts.backtest import evolution_ledger
from . import evaluate, fundamentals
from .us_broad_classic_study import (
    eligibility_mask, forward_label, ic_summary, quintile_returns, segment_masks,
)

REPO = Path(__file__).resolve().parents[2]
PREREG = 'docs/research/2026-11-06-us-fundamental-factors-prereg.md'
IDEA = 'us-fundamental-factors-v1'
EXPECTED_HASHES = dict(
    raw='f3acff6ea5ef883fa473d9fce262d60094c6b99cc7a4ad5903b7f2e2de22ce28',
    prices='bafef7e6ef2fa8123753089bf427661fae8967c525c9d08e5ea9c7288bc60605',
)
FACTORS = dict(F1='GPA', F2='ROE', F3='BM', F4='EP', F5='SP', F6='ACC',
               F7='AG', F8='SUE', F9='EAR')
HORIZONS = (20, 60)
MIN_NAMES = 30
TRAIN_T = 2.99
PERIODS = dict(train=('2012-11-01', '2021-12-31'),
               val=('2022-01-01', '2024-12-31'), forward=('2025-01-01', None))
FOREIGN = frozenset('ARM ASML B BABA NBIS NOK PDD TSM BN BOT BSP SKHY'.split())
DEVIATIONS = [
    '未明确缺日与有效日线口径：统一使用 SPY 日期轴；不补价。有效日线为 close 有限且为正、'
    'volume 有限且非负；累计至少 253 根，20 日流动性窗口要求完整观测。筛选用 close，标签用 adjclose。',
    'SUE 的历史版本按 usable_date 依次生效，同日同季度取长表最后一条；最新季度按 period_end '
    '确定，不回退到较旧的有值季度。标准差取该时点已知、截至该季度最近最多 8 个有限 d4（忽略缺失），'
    '至少 4 个、ddof=1；历史重述只从自身可用日起改变计算。90 日从最新季度该版本 usable_date 起算。',
    'EAR 同一生效日事件合并；最新满足 e+1≤t 的事件即使缺价格也不回退。三日收益窗口要求个股与 '
    'SPY 各三根日收益完整，包含 e−1、e、e+1，90 日从 e 起算；事件使用数据层的 8-K/8-K/A '
    'Item 2.02 及美东收盘生效规则。',
    '有效 IC 按日期排序，前 floor(n/2) 日为前半、余日为后半；正月份占比为月均 IC>0 '
    '的有效月份数除以至少一日有效 IC 的月份数。可用股票数含筛选及因子非空，另报有标签配对数；'
    '中位数覆盖段内所有有完整 h 日标签的 SPY 日期（含不足 30 只的日期）。',
    '按信号日期划分三段，不清洗跨段的 t+h 标签；前向截止 SPY 日历上最后一个有完整 h 日标签的日期。'
    '沿用宽池研究的 NW 实现：Bartlett 核、自协方差除以 n、短于 20 日也用同式；显式 lag=h−1。',
    '辅助多空沿用宽池研究：各段首个 SPY 日期起每 h 日调仓，跳过不足 30 只而不移动相位；'
    '同值按代码稳定排序，五组余数从最低组分配，最高/最低组分别等权 +1/−1。按价格漂移后的实际'
    '权重差绝对成交额收单边 10bp，含初始开仓与最终平仓；报告每期均值及复合收益，不年化。'
    '选股不使用未来价格可用性，选中股票缺终点价格时该期不可观测并单列。',
    '辅助相关未指定期间：宽池通过的 (因子,h) 按因子去重，在宽池前向段计算每日截面 Spearman '
    '的时间均值，每对至少 30 只；同时报告各因子与 adjclose[t−21]/adjclose[t−252]−1 的相关及有效日数。',
    '账本 K 由 append_event 锁内计算，仅作记账；训练阈值固定 2.99，不随 K 改变。'
    'CLI 未指定账本时默认输出目录中的 ledger.jsonl，--no-ledger 禁止写账本。',
]


def read_json(path):
    return json.loads(Path(path).read_text(encoding='utf-8'))


def concatenated_sha256(paths):
    """Concatenate bytes in the supplied order; callers choose the frozen sort."""
    paths = list(paths)
    if not paths:
        raise ValueError('no files to hash')
    digest = hashlib.sha256()
    for path in paths:
        with Path(path).open('rb') as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b''):
                digest.update(chunk)
    return digest.hexdigest()


def coverage_path(fund_root):
    return Path(fund_root).parent / 'fundamentals_probe/coverage_20261106.json'


def guard_inputs(fund_root, allow_input_drift=False):
    root = Path(fund_root)
    manifest, meta = read_json(root / 'manifest.json'), read_json(root / 'prices_meta.json')
    # Exactly the fetcher's full-path sort, including nested submissions pages.
    raw_paths = sorted(os.path.join(dp, f) for dp, _, fs in os.walk(root / 'raw')
                       for f in fs if f.endswith('.gz'))
    price_paths = sorted((root / 'prices').glob('*.json'), key=lambda p: p.name)
    actual = dict(raw=concatenated_sha256(raw_paths), prices=concatenated_sha256(price_paths))
    for key, path in dict(manifest=root / 'manifest.json', prices_meta=root / 'prices_meta.json',
                          coverage_probe=coverage_path(root)).items():
        actual[key] = concatenated_sha256([path])
    expected = dict(raw_prereg=('raw', EXPECTED_HASHES['raw']),
                    raw_manifest=('raw', manifest['raw_sha256']),
                    prices_prereg=('prices', EXPECTED_HASHES['prices']),
                    prices_meta=('prices', meta['sha256']))
    drift = {key: dict(expected=digest, actual=actual[field])
             for key, (field, digest) in expected.items() if actual[field] != digest}
    if drift and not allow_input_drift:
        raise ValueError(f'frozen input sha256 mismatch: {drift}; --allow-input-drift required')
    return actual, drift


def read_prices(path):
    frame = pd.DataFrame(read_json(path)['rows'], columns=['date', 'close', 'adjclose', 'volume'])
    frame['date'] = pd.to_datetime(frame['date'])
    frame = frame.set_index('date').sort_index().astype(float)
    if frame.empty or frame.index.has_duplicates:
        raise ValueError(f'empty prices or duplicate dates: {path}')
    for name in ('close', 'adjclose'):
        frame[name] = frame[name].where(np.isfinite(frame[name]) & frame[name].gt(0))
    frame['volume'] = frame['volume'].where(np.isfinite(frame['volume']) & frame['volume'].ge(0))
    return frame


def has_us_gaap_10q(path):
    if not path.exists():
        return False
    with gzip.open(path, 'rt', encoding='utf-8') as source:
        data = json.load(source)
    return any(row.get('form') == '10-Q'
               for concept in data.get('facts', {}).get('us-gaap', {}).values()
               for rows in concept.get('units', {}).values() for row in rows)


def load_universes(fund_root, frozen=True):
    root = Path(fund_root)
    entries = read_json(root / 'manifest.json')['tickers']
    broad_members = {t for t, e in entries.items() if 'sp500' in e.get('groups', [])}
    bstock = {t for t, e in entries.items() if 'bstock90' in e.get('groups', [])}
    transfer = bstock - FOREIGN
    actual_10q = {t for t in bstock if has_us_gaap_10q(
        root / 'raw/companyfacts' / f'CIK{str(entries[t]["cik"]).removeprefix("CIK").zfill(10)}.json.gz')}
    probe = read_json(coverage_path(root))['bstock']
    probe_10q = {t for t, e in probe.items() if e['kind'] == '10-Q'}
    if actual_10q != transfer or set(probe) != bstock or probe_10q != transfer:
        raise ValueError('T mismatch: manifest bstock90 minus foreign, us-gaap 10-Q, and coverage probe disagree')
    if frozen and (len(bstock) != 90 or len(transfer) != 78 or bstock & FOREIGN != FOREIGN):
        raise ValueError('frozen universe counts disagree (bstock90=90 / T=78 / foreign=12)')
    available = {p.stem for p in (root / 'prices').glob('*.json')}
    if transfer - available:
        raise ValueError(f'T missing price files: {sorted(transfer - available)}')
    symbols = dict(U=sorted(broad_members & available), T=sorted(transfer))
    union = sorted(set(symbols['U']) | transfer)
    spy = read_prices(root / 'prices/SPY.json')['adjclose']
    frames = {t: read_prices(root / 'prices' / f'{t}.json') for t in union}
    prices = {name: pd.DataFrame({t: frame[name] for t, frame in frames.items()}).reindex(spy.index)
              for name in ('close', 'adjclose', 'volume')}
    composition = dict(broad_symbols=symbols['U'], transfer_symbols=symbols['T'],
                       bstock90_symbols=sorted(bstock), excluded_foreign=sorted(bstock & FOREIGN),
                       missing_broad_prices=sorted(broad_members - available),
                       raw_10q_symbols=sorted(actual_10q), probe_10q_symbols=sorted(probe_10q),
                       cross_check_pass=True)
    return symbols, prices, spy, composition


def positive_ratio(numerator, denominator):
    return numerator.div(denominator.where(np.isfinite(denominator) & denominator.gt(0)))


def sue_factor(table, index, columns):
    """Replay quarterly versions; neither future revisions nor EPS enter early."""
    result = pd.DataFrame(np.nan, index=index, columns=columns)
    for ticker, rows in table.groupby('ticker', sort=False):
        if ticker not in result:
            continue
        rows = rows.sort_values('usable_date', kind='stable')
        state, dates, values, anchors = {}, [], [], []
        for usable, batch in rows.groupby('usable_date', sort=True):
            for row in batch.itertuples(index=False):
                state[row.period_end] = (row.d4, pd.Timestamp(row.usable_date))
            latest = max(state)
            current, anchor = state[latest]
            history = [state[end][0] for end in sorted(state) if end <= latest
                       and pd.notna(state[end][0]) and np.isfinite(state[end][0])][-8:]
            std = float(np.std(history, ddof=1)) if len(history) >= 4 else np.nan
            value = current / std if pd.notna(current) and np.isfinite(std) and std > 0 else np.nan
            dates.append(pd.Timestamp(usable))
            values.append(value)
            anchors.append(anchor)
        snapshots = pd.DataFrame(dict(value=values, anchor=anchors), index=pd.DatetimeIndex(dates))
        # Row-wise asof preserves a newly revealed NaN instead of filling it away.
        daily = snapshots.reindex(index, method='ffill')
        age = (index.to_series() - daily['anchor']).dt.days
        result[ticker] = daily['value'].where(age.between(0, 90))
    return result


def ear_factor(events, adjclose, spy):
    """Expose [e-1,e,e+1] compounded excess returns only at e+1 close."""
    index = adjclose.index
    result = adjclose * np.nan
    returns = adjclose.pct_change(fill_method=None)
    spy_returns = spy.reindex(index).pct_change(fill_method=None)
    cumulative = (1 + returns).rolling(3, min_periods=3).apply(np.prod, raw=True)
    benchmark = (1 + spy_returns).rolling(3, min_periods=3).apply(np.prod, raw=True)
    excess = cumulative.sub(benchmark, axis=0)
    for ticker, rows in events.groupby('ticker', sort=False):
        if ticker not in result:
            continue
        positions = index.get_indexer(pd.to_datetime(sorted(set(rows.effective_date))))
        positions = positions[(positions >= 0) & (positions + 1 < len(index))]
        if not len(positions):
            continue
        snapshot = pd.DataFrame(dict(value=excess[ticker].iloc[positions + 1].to_numpy(),
                                     event=index[positions]), index=index[positions + 1])
        daily = snapshot.reindex(index, method='ffill')
        age = (index.to_series() - daily['event']).dt.days
        result[ticker] = daily['value'].where(age.between(0, 90))
    return result


def compute_factors(panel, adjclose, spy):
    mean_equity = (panel['equity'] + panel['equity_prev_year']) / 2
    mean_assets = (panel['assets'] + panel['assets_prev_year']) / 2
    factors = dict(
        F1=positive_ratio(panel['ttm_gross_profit'], panel['assets']),
        F2=positive_ratio(panel['ttm_net_income'], mean_equity),
        F3=positive_ratio(panel['equity'].where(panel['equity'].gt(0)), panel['market_cap']),
        F4=positive_ratio(panel['ttm_net_income'], panel['market_cap']),
        F5=positive_ratio(panel['ttm_revenue'], panel['market_cap']),
        F6=-positive_ratio(panel['ttm_net_income'] - panel['ttm_cfo'], mean_assets),
        F7=-(positive_ratio(panel['assets'], panel['assets_prev_year']) - 1),
        F8=sue_factor(panel.sue, adjclose.index, adjclose.columns),
        F9=ear_factor(panel.events, adjclose, spy),
    )
    return {key: frame.where(np.isfinite(frame)) for key, frame in factors.items()}


def gates(broad, transfer):
    def positive(value):
        return value is not None and np.isfinite(value) and value > 0

    train, val, forward = (broad[seg] for seg in PERIODS)
    flags = dict(
        G1=positive(train['ic']) and train['t'] >= TRAIN_T and
        len(train['ic_halves']) == 2 and all(positive(x) for x in train['ic_halves']),
        G2=positive(val['ic']) and val['t'] >= 1.65,
        G3=positive(forward['ic']),
        G4=positive(transfer['forward']['ic']) and transfer['forward']['t'] >= 1.,
    )
    flags = {key: bool(value) for key, value in flags.items()}
    failed = [key for key, passed in flags.items() if not passed]
    return dict(**flags, broad_pass=all(flags[g] for g in ('G1', 'G2', 'G3')),
                transfer_pass=all(flags.values()), failing_gates=failed,
                first_failing_gate=failed[0] if failed else None)


def passing_correlations(factors, adjclose, mask, results):
    ids = [f for f in FACTORS if any(r['factor'] == f and r['gates']['broad_pass'] for r in results)]
    forward = mask.index >= pd.Timestamp(PERIODS['forward'][0])
    matrix, counts, momentum = {f: {} for f in ids}, {f: {} for f in ids}, {}
    mom = adjclose.shift(21) / adjclose.shift(252) - 1

    def correlation(left, right):
        daily = evaluate.cs_rank_ic(left.loc[forward], right.loc[forward], mask.loc[forward], MIN_NAMES).dropna()
        return dict(correlation=float(daily.mean()) if len(daily) else None, n_days=len(daily))

    for i, left in enumerate(ids):
        momentum[left] = correlation(factors[left], mom)
        for right in ids[i:]:
            pair = correlation(factors[left], factors[right])
            matrix[left][right] = matrix[right][left] = pair['correlation']
            counts[left][right] = counts[right][left] = pair['n_days']
    return dict(factor_ids=ids, matrix=matrix, n_days=counts, momentum_12_1=momentum,
                universe='U', segment='forward', method='mean_daily_cross_sectional_spearman')


def ledger_result(results, hashes, ledger_path, drift, allow_input_drift):
    passing = lambda key: [dict(factor=r['factor'], h=r['h']) for r in results if r['gates'][key]]
    broad, transfer = passing('broad_pass'), passing('transfer_pass')

    def build(_k, _previous):
        return dict(track='fq_us', stage='S1', change_type='E', idea_id=IDEA,
                    baseline_version='fq_us:mom_ls_invvol_v1',
                    verdict='screen_pass' if transfer else 'fail',
                    n_pass_broad=len(broad), n_pass_transfer=len(transfer),
                    passing_broad=broad, passing_transfer=transfer, input_hashes=hashes,
                    prereg_path=PREREG, input_drift=drift, allow_input_drift=allow_input_drift)

    return evolution_ledger.append_event(ledger_path, 'fq_us', build) if ledger_path is not None else build(None, [])


def validate_outputs(fund_root, out, ledger_path):
    out, root = Path(out).resolve(), Path(fund_root).resolve()
    reports = [(out / name).resolve() for name in ('report.json', 'report.md')]
    outputs = [out, *reports]
    if ledger_path is not None:
        ledger = Path(ledger_path).resolve()
        if ledger == out or ledger in reports:
            raise ValueError('ledger and report paths must differ')
        outputs.append(ledger)
    for path in outputs:
        if path == root or root in path.parents or path == coverage_path(root).resolve():
            raise ValueError(f'output targets read-only input: {path}')
    return out


def run_study(fund_root, out, ledger_path=None, allow_input_drift=False, progress=print):
    started = time.monotonic()
    validate_outputs(fund_root, out, ledger_path)
    hashes, drift = guard_inputs(fund_root, allow_input_drift)
    symbols, prices, spy, composition = load_universes(fund_root, frozen=not drift)
    progress(f'inputs verified; U={len(symbols["U"])}; T={len(symbols["T"])}; rebuilding PIT panel')
    root = Path(fund_root)
    panel = fundamentals.build_panel(root / 'raw', root / 'prices', prices['close'].columns,
                                     start=str(spy.index[0].date()), end=str(spy.index[-1].date()))
    factors = compute_factors(panel, prices['adjclose'], spy)
    eligible = eligibility_mask(prices['close'], prices['volume'])
    progress('PIT panel rebuilt; all nine factors and eligibility computed')
    results = []
    for factor, name in FACTORS.items():
        for h in HORIZONS:
            row = dict(factor=factor, name=name, h=h)
            labels = forward_label(prices['adjclose'], h)
            masks = segment_masks(spy.index, h)
            for universe, tickers in symbols.items():
                values, target, mask = factors[factor][tickers], labels[tickers], eligible[tickers]
                available = mask & np.isfinite(values)
                paired = available & np.isfinite(target)
                ic = evaluate.cs_rank_ic(values, target, mask, MIN_NAMES)
                statistics = {}
                for seg, visible in masks.items():
                    summary = ic_summary(ic.loc[visible], h)
                    summary['median_available_names'] = float(available.loc[visible].sum(axis=1).median()) if visible.any() else None
                    summary['median_paired_names'] = float(paired.loc[visible].sum(axis=1).median()) if visible.any() else None
                    statistics[seg] = summary
                row['broad' if universe == 'U' else 'transfer'] = statistics
                if universe == 'U':
                    row['broad_ls'] = {seg: quintile_returns(values, target, mask, visible, h)
                                       for seg, visible in masks.items()}
            row['gates'] = gates(row['broad'], row['transfer'])
            results.append(row)
        progress(f'{factor} {name}: fixed horizons evaluated')
    correlations = passing_correlations({f: v[symbols['U']] for f, v in factors.items()},
                                        prices['adjclose'][symbols['U']], eligible[symbols['U']], results)
    if guard_inputs(fund_root, allow_input_drift)[0] != hashes:
        raise ValueError('inputs changed during study')
    ledger_row = ledger_result(results, hashes, ledger_path, drift, allow_input_drift)
    source_hashes = {name: concatenated_sha256([REPO / 'scripts/factor_mining' / name])
                     for name in ('us_fundamental_factor_study.py', 'fundamentals.py',
                                  'us_broad_classic_study.py', 'evaluate.py')}
    return dict(idea_id=IDEA, prereg_path=PREREG,
                prereg_sha256=concatenated_sha256([REPO / PREREG]), source_hashes=source_hashes,
                fund_root=str(root.resolve()), input_hashes=hashes, expected_hashes=EXPECTED_HASHES,
                input_drift=drift, allow_input_drift=allow_input_drift,
                parameters=dict(horizons=list(HORIZONS), n_tests=18, min_names=MIN_NAMES,
                                nw_lag='h-1', train_t=TRAIN_T, val_t=1.65, transfer_t=1.,
                                history=253, min_close=5, liquidity_window=20,
                                min_median_dollar_volume=10_000_000, one_way_cost=.001,
                                sue_window=8, sue_min_count=4, sue_ddof=1,
                                sue_max_age_days=90, ear_max_age_days=90, periods=PERIODS),
                composition=composition, panel_metadata=panel.metadata,
                stale_suppression=panel.stale_suppression, panel_diagnostics=panel.diagnostics,
                results=results, passing_correlations=correlations,
                **{key: ledger_row[key] for key in ('verdict', 'n_pass_broad', 'n_pass_transfer',
                                                    'passing_broad', 'passing_transfer')},
                ledger=dict(path=str(ledger_path) if ledger_path is not None else None,
                            written=ledger_path is not None, row=ledger_row),
                deviations=DEVIATIONS + (['显式允许输入哈希漂移，本结果不代表原冻结输入研究。'] if drift else []),
                elapsed_seconds=time.monotonic() - started)


def number(value, digits=4):
    return 'NA' if value is None else f'{value:.{digits}f}'


def render_report(result):
    def stats(cell):
        return f'{number(cell["ic"])} / {number(cell["t"], 3)} / {cell["n_days"]}'

    lines = ['# 美股基本面因子：预注册 S1 研究', '',
             f'结论：**{result["verdict"]}**；宽池通过 {result["n_pass_broad"]}/18，'
             f'可迁移通过 {result["n_pass_transfer"]}/18。', '',
             f'预注册：`{PREREG}`；耗时 {result["elapsed_seconds"]:.3f} 秒。', '',
             f'股票池：U={len(result["composition"]["broad_symbols"])}，T='
             f'{len(result["composition"]["transfer_symbols"])}；T 已与原始 us-gaap 10-Q 和覆盖探针交叉核对。', '',
             '**宽池为当前标普成分股，存在幸存者偏差，结论可能偏乐观。**', '',
             '## 三段与门槛', '',
             '每日截面 RankIC 至少 30 只，NW 滞后 h−1；G1：训练 IC>0、t≥2.99（18 次 Bonferroni）'
             '且按有效日数平分的两半 IC 均>0；G2：验证 IC>0、t≥1.65；G3：宽池前向 IC>0；'
             'G4：迁移前向 IC>0、t≥1.0。按未舍入数值判定。', '',
             '| 因子 | h | 训练 IC / t / 日数 | 验证 IC / t / 日数 | 前向 IC / t / 日数 | '
             '迁移前向 IC / t / 日数 | G1/G2/G3/G4 | 宽池/迁移 | 失败门槛 |',
             '| --- | ---: | --- | --- | --- | --- | --- | --- | --- |']
    for row in result['results']:
        g = row['gates']
        cells = [row['factor'] + ' ' + row['name'], str(row['h'])]
        cells += [stats(row['broad'][seg]) for seg in PERIODS]
        cells += [stats(row['transfer']['forward']),
                  '/'.join('是' if g[key] else '否' for key in ('G1', 'G2', 'G3', 'G4')),
                  '/'.join('通过' if g[key] else '失败' for key in ('broad_pass', 'transfer_pass')),
                  ', '.join(g['failing_gates']) or '无']
        lines.append('| ' + ' | '.join(cells) + ' |')
    lines += ['', '## 稳定性与覆盖（仅描述）', '',
              '可用数中位数覆盖各段所有完整标签日期；正月占比按月均 IC。', '',
              '| 因子/h | 训练前半/后半 IC | 宽池正月占比（训/验/前） | 宽池可用数中位数（训/验/前） | '
              '迁移训练 IC/t | 迁移验证 IC/t |', '| --- | --- | --- | --- | --- | --- |']
    for row in result['results']:
        lines.append('| ' + ' | '.join([
            f'{row["factor"]}/{row["h"]}',
            '/'.join(number(v) for v in row['broad']['train']['ic_halves']),
            '/'.join(number(row['broad'][seg]['positive_month_share'], 3) for seg in PERIODS),
            '/'.join(number(row['broad'][seg]['median_available_names'], 1) for seg in PERIODS),
            stats(row['transfer']['train']), stats(row['transfer']['val'])]) + ' |')
    lines += ['', '## 宽池五分位多空（仅描述）', '',
              '等权最高组减最低组，每 h 日调仓，单边 10bp；各格为毛/净复合收益。逐期收益、均值、'
              '成本、缺终点日期、每日/月度 IC 及迁移各段覆盖指标详见 report.json。', '',
              '| 因子/h | 训练毛/净 | 验证毛/净 | 前向毛/净 |', '| --- | --- | --- | --- |']
    for row in result['results']:
        cells = [f'{row["factor"]}/{row["h"]}']
        cells += ['/'.join(number(row['broad_ls'][seg][key]) for key in ('gross_cumulative', 'net_cumulative'))
                  for seg in PERIODS]
        lines.append('| ' + ' | '.join(cells) + ' |')
    lines += ['', '## 宽池通过因子相关及 12-1 动量（仅描述）', '']
    corr = result['passing_correlations']
    ids = corr['factor_ids']
    if ids:
        lines += ['| 因子 | ' + ' | '.join(ids) + ' | 动量相关 / 日数 |',
                  '| --- | ' + ' | '.join(['---:'] * len(ids)) + ' | --- |']
        for left in ids:
            mom = corr['momentum_12_1'][left]
            lines.append('| ' + left + ' | ' + ' | '.join(number(corr['matrix'][left][right]) for right in ids)
                         + f' | {number(mom["correlation"])} / {mom["n_days"]} |')
    else:
        lines.append('无宽池通过因子，相关矩阵及动量相关为空。')
    lines += ['', '## 数据层时点规则', '',
              '从 raw 重建，未读取预计算面板。申报日之后第一个 SPY 交易日可用；重述从自身可用日起替换。'
              '流量、资产/权益/份额及市值距最新期间超过 200 自然日置空；上年资产/权益取最新期间减一年 '
              '±45 日内最近值，并随最新期间过期。股数与 close 为今日拆股口径。EPS Q4 优先 FY 减前三季度，'
              '其次 FY 减九个月，均为近似。', '', '## 输入哈希与账本', '']
    for name, digest in result['input_hashes'].items():
        lines.append(f'- {name}：`{digest}`。')
    lines += [f'- 预注册：`{result["prereg_sha256"]}`。',
              f'- 输入漂移：{result["input_drift"]}；允许漂移：{result["allow_input_drift"]}。',
              f'- 账本：{result["ledger"]["path"] or "未写入（--no-ledger）"}；'
              'fq_us / S1 / E / us-fundamental-factors-v1 / fq_us:mom_ls_invvol_v1。', '',
              '## deviations（歧义解释与偏离）', '']
    lines += ['- ' + item for item in result['deviations']]
    return '\n'.join(lines) + '\n'


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--fund-root', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    ledger = parser.add_mutually_exclusive_group()
    ledger.add_argument('--ledger', type=Path)
    ledger.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--allow-input-drift', action='store_true')
    args = parser.parse_args(argv)
    ledger_path = None if args.no_ledger else (args.ledger or args.out / 'ledger.jsonl').resolve()
    try:
        out = validate_outputs(args.fund_root, args.out, ledger_path)
        result = run_study(args.fund_root, out, ledger_path, args.allow_input_drift,
                           progress=lambda message: print(message, file=sys.stderr, flush=True))
        markdown = render_report(result)
        serialized = json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + '\n'
    except (ValueError, FileNotFoundError) as error:
        parser.error(str(error))
    out.mkdir(parents=True, exist_ok=True)
    (out / 'report.json').write_text(serialized, encoding='utf-8')
    (out / 'report.md').write_text(markdown, encoding='utf-8')
    print(f'verdict={result["verdict"]}; n_pass_broad={result["n_pass_broad"]}; '
          f'n_pass_transfer={result["n_pass_transfer"]}; wall_seconds={result["elapsed_seconds"]:.3f}')
    print(f'reports: {out / "report.json"}, {out / "report.md"}')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
