"""Offline preregistered fq_us volatility scaling S1; no data writes or fetching."""
from __future__ import annotations

import argparse
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

from . import evolution_ledger
from . import fq_us_momentum_stress as stress
from . import fq_us_momentum_study as s1


PREREG_PATH = 'docs/research/2026-11-05-fq-us-volscale-prereg.md'
DATA_ROOT = Path('/Users/zhangyun/workspace/nofx/data')
FACTOR_ROOT = DATA_ROOT / 'factor_mining'
SP500 = FACTOR_ROOT / 'us_broad/constituents_20261105.json'
DATASETS = {
    'D1': dict(daily=FACTOR_ROOT / 'us_stress/daily', universe='sp500',
               spy=FACTOR_ROOT / 'us_stress/daily/SPY.json'),
    'D2': dict(daily=FACTOR_ROOT / 'us_momentum/snapshot_20261105/daily',
               universe='bstock90',
               spy=FACTOR_ROOT / 'us_momentum/snapshot_20261105/daily/SPY.json'),
    'D3': dict(daily=FACTOR_ROOT / 'us_broad/daily', universe='sp500',
               spy=FACTOR_ROOT / 'us_broad/benchmark/SPY.json'),
}
EXPECTED_HASHES = {
    'D1': '46ec51b880a7a8cc724c71be3650f19e6d4e1880e3c05fc0710c4bfdef44a256',
    'D2': '5bd711af9dbeef6d2131005aea89a5fc65897e3d47485145bd3868448d3e8042',
    'D3': '2e621c842446cb82ddb1f6dd8744b333bacd8817f0e6ef4ff39543a8f9fc4814',
}
INITIAL_CASH = s1.PARAMS['initial_cash']
NOTES = [
    '三组独立形成信号；未缩放组合在收盘调仓，调仓日收益先来自旧持仓，再按总权益重设多空各 0.5；固定股数持有至下次调仓，不扣成本。',
    '歧义口径：首个建仓日没有此前持仓，u_d 留空，不补造零收益；此前未满足选股条件的历史不虚构持仓。缺价沿用此前有效收盘，与原生袖套一致。',
    'σ̂ 为含当日最近 126 个 u_d 的样本标准差；σ_ref 为含当日所有有效日度 σ̂ 的扩展中位数，至少 252 个值；两者不足时 k=1。日度计算、只在调仓日应用，绝不使用未来数据。',
    '歧义口径：σ̂=0 时取 k=1；σ̂>0 且 σ_ref=0 时取 k=0。标准差均为日度单位，不年化，比例不变。',
    'B 和 V 均调用原生 Backtrader 双腿；V 袖套目标为基础袖套权重乘 k，空余为现金；现有单名上限保持同比例缩放，少量入选时不重新归一化。',
    '组合严格复用压力研究的 50/50 等权重置后处理，袖套权益份额各 0.5，k 已包含在袖内持仓；不再把组合公式乘 k。该公式忽略重分配资金引起的佣金规模变化。',
    '沿用原规则的 SPY 有效日历、完整 20 日周期截断、终点调仓及成本；多头每次成交 13 bp，空头 7 bp，分数股、同收盘原生成交、现金约束；不强制终点清仓。',
    '歧义口径：夏普用收盘简单日收益及样本标准差（ddof=1），年化乘 √252，零无风险利率；含首日相对初始现金的建仓成本。回撤亦含初始现金峰值。',
    '歧义口径：平均总敞口为逐日收盘实际持仓绝对市值/组合权益的算术平均，按同一等权重置公式折算两腿；调仓日收盘份额重置为各 0.5。另列目标敞口均值。',
    'D1 的 25% 熔断复用收盘回撤锁定覆盖，触发日收益保留、此后永久现金；覆盖不加平仓费，日频会低估盘中触发。判定使用未覆盖路径夏普/回撤与 V 的触发标志。',
    '分年从上年末收盘计；窗口按起止收盘比，不含起点当日收益，非交易日起点向后、终点向前取样。',
    '三组均已被查看过，现有成分回看存在幸存者偏差；未模拟退市、借券与历史可交易性。结论仅为 S1 筛查，不改现行 S2。',
]


def daily_hash(daily):
    files = sorted(Path(daily).glob('*.json'), key=lambda p: p.name)
    if not files:
        raise ValueError('daily/*.json is empty')
    digest = hashlib.sha256()
    for path in files:
        digest.update(path.read_bytes())
    return digest.hexdigest(), [p.name for p in files]


def guard_hashes(datasets, allow_input_drift=False):
    hashes, files, mismatches = {}, {}, {}
    for name in datasets:
        hashes[name], files[name] = daily_hash(DATASETS[name]['daily'])
        if hashes[name] != EXPECTED_HASHES[name]:
            mismatches[name] = dict(expected=EXPECTED_HASHES[name], actual=hashes[name])
    if mismatches and not allow_input_drift:
        raise ValueError('preregistered input hash mismatch: ' + json.dumps(mismatches))
    return hashes, files, mismatches


def _read_frame(path):
    rows = json.loads(Path(path).read_bytes())
    frame = pd.DataFrame([r[:6] for r in rows],
                         columns=['date', 'open', 'high', 'low', 'close', 'volume'])
    frame.index = pd.DatetimeIndex(pd.to_datetime(frame.pop('date')))
    if not frame.index.is_monotonic_increasing or frame.index.has_duplicates:
        raise ValueError(f'{path}: dates must be unique and ascending')
    frame = frame.astype(float)
    valid = s1.valid_rows(frame)
    return frame.where(valid, np.nan), dict(rows=len(frame), valid_rows=int(valid.sum()),
                                          invalid_rows=int((~valid).sum()))


def load_dataset(name):
    spec = DATASETS[name]
    if name in ('D1', 'D2'):
        loaded = stress.load_inputs(spec['daily'], SP500, stress.FROZEN_UNIVERSE)
        frames, calendar = loaded['frames'], loaded['calendar']
        requested = loaded['requested'][spec['universe']]
        quality = loaded['data_quality']
    else:
        requested = sorted({s.replace('.', '-') for s in json.loads(SP500.read_bytes())['sp500']})
        frames, quality = {}, {}
        for path in sorted(spec['daily'].glob('*.json')):
            frame, quality[path.stem] = _read_frame(path)
            if frame.close.notna().any():
                frames[path.stem] = frame
        frames['SPY'], quality['SPY'] = _read_frame(spec['spy'])
        calendar = frames['SPY'].dropna().index
        for symbol, frame in frames.items():
            quality[symbol]['off_calendar_rows'] = int((~frame.index.isin(calendar)).sum())
    if calendar.empty:
        raise ValueError('SPY has no valid trading calendar')
    universe_path = stress.FROZEN_UNIVERSE if name == 'D2' else SP500
    return dict(frames=frames, calendar=calendar, requested=requested, data_quality=quality,
                inputs=dict(daily=str(spec['daily']), spy=str(spec['spy']),
                            universe=str(universe_path)),
                auxiliary_hashes=dict(universe=hashlib.sha256(universe_path.read_bytes()).hexdigest(),
                                      spy=hashlib.sha256(spec['spy'].read_bytes()).hexdigest()))


def unscaled_returns(close, decisions):
    """Cost-free self-financing holdings, reset only after each decision close."""
    if close.empty or not decisions or str(close.index[0].date()) != decisions[0]['date']:
        raise ValueError('close calendar must start at the first decision')
    if not close.index.is_monotonic_increasing or close.index.has_duplicates:
        raise ValueError('close dates must be unique and ascending')
    by_date = {d['date']: d for d in decisions}
    if not set(by_date) <= {str(d.date()) for d in close.index}:
        raise ValueError('decision dates must be on the close calendar')
    marks = close.ffill()
    shares = pd.Series(dtype=float)
    cash = previous = 1.0
    values = []
    for i, (date, prices) in enumerate(marks.iterrows()):
        held = prices.reindex(shares.index)
        if not np.isfinite(held).all() or (held <= 0).any():
            raise ValueError(f'{date}: unpriced holding')
        equity = cash + float((shares * held).sum())
        if not math.isfinite(equity) or equity <= 0:
            raise ValueError('unscaled signal equity must be finite and positive')
        values.append(equity / previous - 1 if i else np.nan)
        previous = equity
        decision = by_date.get(str(date.date()))
        if decision is not None:
            # decision_at returns sleeve weights, twice the total-capital weights.
            weights = pd.Series({s: 0.5 * w for leg in ('long', 'short')
                                 for s, w in decision[leg].items()}, dtype=float)
            entry = prices.reindex(weights.index)
            if not np.isfinite(entry).all() or (entry <= 0).any():
                raise ValueError(f'{date}: unpriced target')
            shares = equity * weights / entry
            cash = equity * (1 - float(weights.sum()))
    return pd.Series(values, index=close.index, name='u_d')


def volatility_signal(returns):
    sigma = returns.rolling(126, min_periods=126).std(ddof=1)
    reference = sigma.expanding(min_periods=252).median()
    k = pd.Series(1.0, index=returns.index)
    ready = reference.notna() & sigma.gt(0)
    k.loc[ready] = (reference.loc[ready] / sigma.loc[ready]).clip(lower=0, upper=1)
    return pd.DataFrame(dict(u_d=returns, sigma_hat=sigma, sigma_ref=reference,
                             sigma_observations=sigma.notna().cumsum(), k=k))


def scaled_targets(decisions, scales, leg):
    return {d['date']: {s: w * scales[d['date']] for s, w in d[leg].items()}
            for d in decisions}


def distribution(values):
    values = np.asarray(values, dtype=float)
    return dict(n=len(values), min=float(values.min()), max=float(values.max()),
                mean=float(values.mean()), median=float(np.median(values)),
                p05=float(np.quantile(values, 0.05)), p25=float(np.quantile(values, 0.25)),
                p75=float(np.quantile(values, 0.75)), p95=float(np.quantile(values, 0.95)),
                below_one=int((values < 1).sum()), at_one=int((values == 1).sum()))


def describe_path(curve, initial_cash, *, d1=False, fills=(), post_processing=False):
    metrics = s1.path_metrics(curve, initial_cash, fills)
    levels = np.array([initial_cash] + [r['equity'] for r in curve])
    returns = levels[1:] / levels[:-1] - 1
    sd = float(returns.std(ddof=1)) if len(returns) >= 2 else 0.0
    metrics['sharpe'] = float(returns.mean() / sd * math.sqrt(252)) if sd > 0 else None
    metrics.update(stress._drawdown_dates(curve, initial_cash))
    if d1:
        metrics['focus_windows'] = {k: stress.window_metrics(curve, *window)
                                    for k, window in stress.FOCUS_WINDOWS.items()}
    if post_processing:
        for key in ('traded_notional', 'turnover', 'annualized_turnover', 'commission'):
            metrics[key] = None
    return metrics


def gross_exposure(long_curve, short_curve, rebalances):
    """Translate native sleeve notionals to the same equalized capital path."""
    anchor_long = anchor_short = INITIAL_CASH / 2
    rebalances = set(rebalances)
    result = []
    for long, short in zip(long_curve, short_curve, strict=True):
        if long['date'] != short['date']:
            raise ValueError('sleeve dates must align')
        left, right = long['equity'] / anchor_long, short['equity'] / anchor_short
        fraction_long = left / (left + right)
        if long['date'] in rebalances:
            fraction_long = 0.5
            anchor_long, anchor_short = long['equity'], short['equity']
        gross = [sum(abs(p['value']) for p in row['positions'].values()) / row['equity']
                 for row in (long, short)]
        result.append(dict(date=long['date'], gross_exposure=(
            fraction_long * gross[0] + (1 - fraction_long) * gross[1])))
    return result


def run_arm(frames, sessions, decisions, scales, signal, *, d1=False):
    legs = {}
    for leg in ('long', 'short'):
        print(f"  {'V' if any(k < 1 for k in scales.values()) else 'B/k=1'} {leg}", flush=True)
        legs[leg] = s1.run_sleeve(frames, sessions, scaled_targets(decisions, scales, leg),
            cash=INITIAL_CASH / 2, commission=s1.PARAMS[f'{leg}_cost_per_fill'],
            short=(leg == 'short'))
        legs[leg]['metrics'] = describe_path(legs[leg]['equity_curve'], INITIAL_CASH / 2,
                                           d1=d1, fills=legs[leg]['fills'])
    rebalances = [d['date'] for d in decisions]
    long, short = [legs[leg]['equity_curve'] for leg in ('long', 'short')]
    curve = stress.equalized_path(long, short, rebalances)
    independent = [dict(date=a['date'], equity=a['equity'] + b['equity'])
                   for a, b in zip(long, short, strict=True)]
    exposure = gross_exposure(long, short, rebalances)
    rows = []
    for d in decisions:
        row = signal.loc[pd.Timestamp(d['date'])]
        rows.append(dict(date=d['date'], k=scales[d['date']], sigma_hat=row.sigma_hat,
                         sigma_ref=row.sigma_ref, sigma_observations=int(row.sigma_observations),
                         target_gross_exposure=0.5 * scales[d['date']] * sum(
                             abs(w) for leg in ('long', 'short') for w in d[leg].values())))
    arm = dict(post_processing=True, equity_curve=curve,
               metrics=describe_path(curve, INITIAL_CASH, d1=d1, post_processing=True),
               legs=legs, signal_rebalances=rows, k_distribution=distribution(list(scales.values())),
               mean_gross_exposure=float(np.mean([r['gross_exposure'] for r in exposure])),
               gross_exposure_curve=exposure,
               mean_rebalance_target_gross_exposure=float(np.mean([r['target_gross_exposure']
                                                                   for r in rows])),
               independent_sleeves=dict(equity_curve=independent,
                   metrics=describe_path(independent, INITIAL_CASH, d1=d1,
                                        fills=legs['long']['fills'] + legs['short']['fills'])),
               period_table=s1.period_table(curve, decisions))
    if d1:
        overlay = stress.kill_switch_overlay(curve, rebalances, drawdown=True)
        overlay['triggered'] = bool(overlay['triggers'])
        overlay['metrics'] = describe_path(overlay['equity_curve'], INITIAL_CASH, d1=True,
                                            post_processing=True)
        arm['kill_switch'] = overlay
    return arm


def paired_verdict(datasets):
    checks = {}
    for name in DATASETS:
        dataset = datasets.get(name)
        if dataset is None:
            checks[name] = dict(available=False, passed=False)
            continue
        arms = dataset['arms']
        b, v = [arms[a]['metrics'] for a in ('B', 'V')]
        sharpe = all(m['sharpe'] is not None and math.isfinite(m['sharpe']) for m in (b, v))
        check = dict(available=True, sharpe_noninferior=bool(sharpe and v['sharpe'] >= b['sharpe']),
                     max_drawdown_lower=bool(v['max_drawdown'] < b['max_drawdown']))
        if name == 'D1':
            check['kill_switch_clear'] = not arms['V']['kill_switch']['triggered']
        check['passed'] = all(check.values())
        checks[name] = check
    return dict(verdict='screen_pass' if all(c['passed'] for c in checks.values()) else 'fail',
                checks=checks, complete=all(c['available'] for c in checks.values()))


def run_study(datasets=('D1', 'D2', 'D3'), allow_input_drift=False):
    hashes, files, mismatches = guard_hashes(datasets, allow_input_drift)
    report = dict(prereg_path=PREREG_PATH, preregistered_K=4, engine='backtrader',
                  baseline_version='mom_ls_invvol_v1', rule='mom_ls_invvol_volscale_v1',
                  params=dict(base=s1.PARAMS, sigma_window=126, sigma_ddof=1,
                              reference='expanding daily median', reference_min_values=252,
                              cap=1.0, rebalance_only=True, kill_switch_drawdown=0.25),
                  input_hashes=hashes, expected_input_hashes={d: EXPECTED_HASHES[d] for d in datasets},
                  input_daily_files=files, input_drift=mismatches, notes=NOTES,
                  deviations=[n for n in NOTES if n.startswith('歧义口径')], datasets={})
    if mismatches:
        report['deviations'].append('显式使用 --allow-input-drift，输入偏离冻结哈希，本次不是冻结输入复现。')
    if set(datasets) != set(DATASETS):
        report['deviations'].append('只运行数据子集，缺少三组同时成立的证据，判定按其他情况记 fail。')
    for name in datasets:
        print(f'{name}: loading frozen inputs', flush=True)
        loaded = load_dataset(name)
        frames, calendar = loaded.pop('frames'), loaded.pop('calendar')
        universe = sorted(set(loaded['requested']) & set(frames))
        features = s1.build_features(frames, universe, calendar)
        sessions, decisions = s1.rebalance_schedule(features, calendar)
        close = features['close'].ffill().reindex(sessions)
        signal = volatility_signal(unscaled_returns(close, decisions))
        stock_frames = {s: frames[s] for s in universe}
        b_scales = {d['date']: 1.0 for d in decisions}
        v_scales = {d['date']: float(signal.loc[pd.Timestamp(d['date']), 'k']) for d in decisions}
        arms = {arm: run_arm(stock_frames, sessions, decisions, scales, signal, d1=(name == 'D1'))
                for arm, scales in [('B', b_scales), ('V', v_scales)]}
        dataset = dict(**loaded, universe=universe,
                       unavailable=sorted(set(loaded['requested']) - set(universe)),
                       start=str(sessions[0].date()), end=str(sessions[-1].date()),
                       trading_sessions=len(sessions), n=len(decisions) - 1,
                       trailing_sessions_excluded=int((calendar > sessions[-1]).sum()),
                       rebalance_dates=[d['date'] for d in decisions], decisions=decisions, arms=arms,
                       signal_daily=[dict(date=str(d.date()), **row) for d, row in
                                     signal.to_dict(orient='index').items()])
        if name == 'D2':
            total = arms['B']['independent_sleeves']['metrics']['total_return']
            dataset['s1_reproduction'] = dict(expected_total_return=3.878078, expected_n=62,
                actual_total_return=total, actual_n=dataset['n'],
                tolerance=0.0000005, passed=abs(total - 3.878078) <= 0.0000005 and dataset['n'] == 62)
            if not dataset['s1_reproduction']['passed']:
                raise ValueError('D2 S1 reproduction failed: ' + json.dumps(dataset['s1_reproduction']))
        quality = loaded['data_quality']
        if any(q['invalid_rows'] or q.get('off_calendar_rows', 0) for q in quality.values()):
            report['deviations'].append(f'{name} 存在无效或日历外日线，按原规则排除，详见 data_quality。')
        report['datasets'][name] = dataset
        report['engine_version'] = arms['B']['legs']['long']['engine_version']
        print(f"{name}: complete, B Sharpe={arms['B']['metrics']['sharpe']:.6f}, "
              f"V Sharpe={arms['V']['metrics']['sharpe']:.6f}", flush=True)
    report.update(paired_verdict(report['datasets']))
    return report


def append_ledger(path, report):
    def build(k, previous_rows):
        if k != 4:
            report['deviations'].append(
                f'账本口径：预注册 K=4；隔离账本按已有记录计 K={k}，不补造历史行。')
        return dict(stage='S1', change_type='C', idea_id='fq-us-momentum-volscale-v1',
                    baseline_version='mom_ls_invvol_v1', verdict=report['verdict'],
                    preregistered_K=4, prereg_path=PREREG_PATH, report_path=report.get('report_path'),
                    datasets={name: {arm: dict(sharpe=data['arms'][arm]['metrics']['sharpe'],
                        max_drawdown=data['arms'][arm]['metrics']['max_drawdown'],
                        kill_switch_triggered=data['arms'][arm].get('kill_switch', {}).get('triggered'))
                        for arm in ('B', 'V')} for name, data in report['datasets'].items()},
                    checks=report['checks'], input_hashes=report['input_hashes'],
                    input_drift=report['input_drift'], deviations=report['deviations'])
    return evolution_ledger.append_event(path, 'fq_us', build)


def render_markdown(report):
    def pct(value):
        return '无' if value is None else f'{value:+.4%}'

    def number(value):
        return '无' if value is None else f'{value:.6f}'

    lines = ['# fq_us 动量波动率缩放：预注册 S1', '',
             f"判定：**{report['verdict']}**。三组须同时满足夏普不下降、最大回撤严格下降，且 D1 缩放版不触发 25% 熔断。", '',
             'B 为基础规则，V 为缩放规则；下表均为原生双腿经 50/50 等权重置后处理的组合。', '',
             '| 数据 | 规则 | 夏普 | 最大回撤 | 累计收益 | 平均总敞口 |',
             '|---|---|---:|---:|---:|---:|']
    for name, dataset in report['datasets'].items():
        for label, arm in dataset['arms'].items():
            m = arm['metrics']
            lines.append(f"| {name} | {label} | {number(m['sharpe'])} | {m['max_drawdown']:.4%} | {pct(m['total_return'])} | {arm['mean_gross_exposure']:.4%} |")
    if 'D2' in report['datasets']:
        check = report['datasets']['D2']['s1_reproduction']
        lines += ['', f"D2 的 B 独立双腿 S1 复现：{'通过' if check['passed'] else '失败'}；累计 {pct(check['actual_total_return'])}（预期 +387.8078%），n={check['actual_n']}（预期 62）。"]
    for name, dataset in report['datasets'].items():
        lines += ['', f'## {name}', '',
                  f"区间 {dataset['start']} → {dataset['end']}，{dataset['trading_sessions']} 日，{dataset['n']} 个完整周期；名单 {len(dataset['requested'])} 只，有数据 {len(dataset['universe'])} 只，尾部排除 {dataset['trailing_sessions_excluded']} 日。",
                  f"条件：`{json.dumps(report['checks'][name], ensure_ascii=False)}`。", '']
        for label, arm in dataset['arms'].items():
            m, k = arm['metrics'], arm['k_distribution']
            initial = '（初始资金）' if m['drawdown_peak_is_initial'] else ''
            lines.append(f"- {label} 最大回撤峰谷：{m['drawdown_peak_date']}{initial} → {m['drawdown_trough_date']}；k 最小/中位/均值/最大 {k['min']:.6f}/{k['median']:.6f}/{k['mean']:.6f}/{k['max']:.6f}，缩放 {k['below_one']}/{k['n']} 次。")
            if name == 'D1':
                windows, kill = m['focus_windows'], arm['kill_switch']
                trigger = kill['triggers'][0]['date'] if kill['triggered'] else '未触发'
                lines.append(f"- {label} 崩盘窗（2007-10-09→2009-03-09）{pct(windows['crash']['total_return'])}；反弹窗（2009-03-09→2009-12-31）{pct(windows['rebound']['total_return'])}；25% 熔断：{trigger}，覆盖后累计 {pct(kill['metrics']['total_return'])}。")
    lines += ['', '## 偏离与歧义处理', ''] + [f'- {d}' for d in report['deviations']]
    lines += ['', '## 口径与局限', ''] + [f'- {n}' for n in report['notes'] if not n.startswith('歧义口径')]
    lines += ['', '## 分年收益', '', '| 数据 | 年份 | B | V |', '|---|---|---:|---:|']
    for name, dataset in report['datasets'].items():
        for year in dataset['arms']['B']['metrics']['yearly_returns']:
            lines.append(f"| {name} | {year} | " + ' | '.join(
                pct(dataset['arms'][arm]['metrics']['yearly_returns'].get(year)) for arm in ('B', 'V')) + ' |')
    lines += ['', '## 输入与账本', '', f'预注册：`{PREREG_PATH}`。']
    for name, digest in report['input_hashes'].items():
        lines.append(f'- {name} 日线 SHA256：`{digest}`；匹配：{name not in report["input_drift"]}。')
    row = report.get('ledger_row')
    lines += ['', f"预注册 K=4；本次隔离账本 K={row['K']}。" if row else '本次未写账本；预注册 K=4。',
              '逐日信号、调仓 k/σ̂/σ_ref、k 分布、实际敞口、原生多空腿及成交、窗口和熔断路径详见 report.json。']
    return '\n'.join(lines) + '\n'


def output_paths(out, ledger=None):
    """Allow a separate real ledger while protecting the actual input folders."""
    out = Path(out).resolve()
    ledger = Path(ledger).resolve() if ledger else out / 'ledger.jsonl'
    for spec in DATASETS.values():
        daily = spec['daily'].resolve()
        if out.is_relative_to(daily) or ledger.is_relative_to(daily):
            raise ValueError('outputs must not be inside an input daily directory')
        if ledger == spec['spy'].resolve():
            raise ValueError('ledger must not overwrite an input calendar')
    if ledger in (SP500.resolve(), stress.FROZEN_UNIVERSE.resolve()):
        raise ValueError('ledger must not overwrite an input universe')
    return out, ledger


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    group = parser.add_mutually_exclusive_group()
    group.add_argument('--ledger', type=Path)
    group.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--allow-input-drift', action='store_true')
    parser.add_argument('--datasets', default='D1,D2,D3')
    args = parser.parse_args(argv)
    datasets = args.datasets.split(',')
    if not datasets or len(set(datasets)) != len(datasets) or any(d not in DATASETS for d in datasets):
        parser.error('--datasets must contain unique names from D1,D2,D3')
    out, ledger = output_paths(args.out, args.ledger)
    if any((out / name).exists() for name in ('report.json', 'report.md')):
        raise FileExistsError('refusing to overwrite an existing study; use a fresh --out')
    report = run_study(datasets, args.allow_input_drift)
    out.mkdir(parents=True, exist_ok=True)
    report['report_path'] = str(out / 'report.json')
    if not args.no_ledger:
        report['ledger_row'] = append_ledger(ledger, report)
    safe = evolution_ledger.json_safe(report)
    (out / 'report.json').write_text(json.dumps(safe, ensure_ascii=False, indent=2,
                                               allow_nan=False) + '\n', encoding='utf-8')
    (out / 'report.md').write_text(render_markdown(report), encoding='utf-8')
    print(f"verdict={report['verdict']}", flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
