"""Offline, descriptive 2008 stress study of mom_ls_invvol_v1.

Native execution is reused from S1. Equalization and risk rules are explicitly
labeled post-processing of settled native daily equity, never order simulators.
"""
from __future__ import annotations

import argparse
import bisect
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

from . import fq_us_momentum_study as s1


FROZEN_UNIVERSE = Path(__file__).resolve().parents[1] / 'factor_mining/us_momentum_universe.json'
INITIAL_CASH = 100_000.0
FOCUS_WINDOWS = {
    'crash': ('2007-10-09', '2009-03-09'),
    'rebound': ('2009-03-09', '2009-12-31'),
}
PATH_LABELS = {
    'independent': '独立双腿合计（S1）', 'equalized': '等权重置多空（后处理）',
    'long': '多头腿（原生）', 'short': '空头腿（原生）',
    'L': 'L 只做多（原生）', 'SPY': 'SPY 持有（原生）',
    'drawdown_latch': 'a 回撤锁定（后处理）',
    'daily_loss': 'b 单日止损（后处理）', 'combined': 'c 双规则（后处理）',
}
NOTES = [
    '本研究仅作描述，不作裁决，不计入任何 K，也不写入演化账本。',
    '两个股票池均由今天的名单回看历史，存在幸存者偏差；sp500 是 2026-11-05 的现有成分，并非当年的标普成分。缺失数据不补造，也不将抓取失败等同于当时未上市。',
    'Yahoo 复权 OHLC 与成交量原样用于 S1 特征及价格/流动性门槛；复权后的 5 美元门槛不等于当年的未复权成交价门槛。未模拟退市、历史成分、借券可得性或永续合约当时可交易性。',
    '选股与执行直接复用 S1：至少 253 根有效线、收盘价至少 5、20 日成交额中位数至少 1000 万；12−1 动量、30 日样本波动率倒数、两端各 20%，不足 15 只时空仓。',
    '每腿初始 50000；腿内目标敞口 1、单名上限 0.20，相当于总资金名义各 0.5、单名上限 0.10；名字太少时上限会使实际敞口不足。L 的敞口 1、单名上限 0.10。',
    '沿用 S1 的 SPY 有效日历、每 20 日调仓和完整周期截断，保留最后周期边界当天的调仓与费用；不强制终点清仓。首日建仓成本计入累计收益。',
    '原生 Backtrader 使用 S1 同收盘成交与收盘后 1 秒同价结算回调、分数股、先卖后买和含费现金约束；多头/L/SPY 每次成交 13 bp，空头 7 bp，无额外滑点或资金费。',
    '等权重置为后处理：E_t=E_k×(0.5×Long_t/Long_k+0.5×Short_t/Short_k)。首日从两腿初始现金计入建仓费；调仓日先按上一周期计算结算后权益，再以该收盘为新锚点，精确重设各 0.5。',
    '等权重置忽略了按总权益重新分配资金后，调仓成交规模差异带来的佣金变化；仍沿用独立袖套的持仓/现金约束、稀疏池低敞口与收益路径，并非共享账户的原生回测。',
    '风控覆盖均为后处理：回撤达到 25%（含等于）当日收盘锁定，初始 100000 也计入峰值；单日下跌达到 7%（含等于）当日收盘停止，至下一调仓日恢复。平仓与恢复均不额外扣费。',
    '单日覆盖在恢复调仓日即接入基础路径当日的收盘对收盘收益；这是题定日频近似，无法复现收盘重入前后的盘中价格与实际新仓。空仓天数指收益被跳过的交易日，不含触发当天；另给出收盘为空仓的天数。',
    '双规则以覆盖后权益更新峰值并检查阈值；同日同时达到两阈值时优先记录永久回撤锁定。单日停机后，基础路径仍继续，仅在恢复后链式接续其日收益。',
    '实盘逐 tick 盘中检查，日收盘覆盖会低估触发次数（UNDERSTATES triggers）；未计盘中低点、额外成交成本、重入仓位差异，因此不是实盘风控的精确重放。',
    '窗口收益采用起止日收盘权益比，起点当日收益不计入窗口；非交易日起点向后、终点向前取样。年度收益以前一年最后观测收盘为基数；首年从初始现金开始，2011H1 截至本次最后完整周期边界。',
    '最差 20 日按 20 个日收益连乘（含首日前初始现金锚点）；最差单日含首次建仓费。回撤峰谷日期来自每日收盘，初始资金峰值标为首日收盘前。',
]


def _curve_values(curve):
    dates = [r['date'] for r in curve]
    values = [float(r['equity']) for r in curve]
    if not dates or dates != sorted(set(dates)):
        raise ValueError('equity dates must be nonempty, unique and ascending')
    if any(not math.isfinite(v) or v <= 0 for v in values):
        raise ValueError('equity must be finite and positive')
    return dates, values


def equalized_path(long_curve, short_curve, rebalance_dates, *, initial_cash=INITIAL_CASH):
    """Chain period-local 50/50 sleeve ratios, resetting after boundary costs."""
    dates, longs = _curve_values(long_curve)
    short_dates, shorts = _curve_values(short_curve)
    rebalances = set(rebalance_dates)
    if dates != short_dates or dates[0] not in rebalances or not rebalances <= set(dates):
        raise ValueError('aligned sleeves and in-calendar rebalance dates are required')
    anchor_long = anchor_short = initial_cash / 2
    anchor_equity = initial_cash
    curve = []
    for date, long, short in zip(dates, longs, shorts, strict=True):
        equity = anchor_equity * (0.5 * long / anchor_long + 0.5 * short / anchor_short)
        curve.append(dict(date=date, equity=equity))
        if date in rebalances:
            anchor_long, anchor_short, anchor_equity = long, short, equity
    return curve


def kill_switch_overlay(curve, rebalance_dates, *, drawdown=False, daily_loss=False,
                        initial_cash=INITIAL_CASH):
    """Apply close-only risk overlays; only invested sessions chain returns."""
    dates, values = _curve_values(curve)
    rebalances = set(rebalance_dates)
    equity = previous_base = peak = initial_cash
    halted = latched = False
    result, triggers, resumptions = [], [], []
    flat_sessions = flat_at_close_sessions = 0
    for date, base in zip(dates, values, strict=True):
        if halted and not latched and date in rebalances:
            halted = False
            resumptions.append(date)
        invested = not halted
        previous_equity = equity
        if invested:
            equity *= base / previous_base
        else:
            flat_sessions += 1
        peak = max(peak, equity)
        if not latched and drawdown and equity <= 0.75 * peak:
            triggers.append(dict(date=date, kind='drawdown_latch', equity=equity,
                                 running_peak=peak, drawdown=1 - equity / peak))
            latched = halted = True
        elif invested and daily_loss and equity <= 0.93 * previous_equity:
            triggers.append(dict(date=date, kind='daily_loss', equity=equity,
                                 daily_return=equity / previous_equity - 1))
            halted = True
        flat_at_close_sessions += int(halted)
        result.append(dict(date=date, equity=equity, invested=invested,
                           flat_at_close=halted, latched=latched))
        previous_base = base
    return dict(post_processing=True, descriptive_only=True, equity_curve=result,
                triggers=triggers, resume_dates=resumptions, flat_sessions=flat_sessions,
                flat_at_close_sessions=flat_at_close_sessions,
                rules=dict(drawdown_latch=drawdown, daily_loss=daily_loss))


def _drawdown_dates(curve, initial_cash):
    peak, peak_date, peak_initial = initial_cash, curve[0]['date'], True
    worst = dict(drawdown_peak_date=None, drawdown_trough_date=None,
                 drawdown_peak_is_initial=False)
    max_dd = 0.0
    for row in curve:
        if row['equity'] > peak:
            peak, peak_date, peak_initial = row['equity'], row['date'], False
        dd = 1 - row['equity'] / peak
        if dd > max_dd:
            max_dd = dd
            worst = dict(drawdown_peak_date=peak_date, drawdown_trough_date=row['date'],
                         drawdown_peak_is_initial=peak_initial)
    return worst


def window_metrics(curve, start, end):
    """Describe a close-to-close window, without including its first day's return."""
    dates, _ = _curve_values(curve)
    left, right = bisect.bisect_left(dates, start), bisect.bisect_right(dates, end)
    subset = curve[left:right]
    common = dict(requested_start=start, requested_end=end)
    if len(subset) < 2:
        return dict(**common, available=False, total_return=None)
    initial = subset[0]['equity']
    return dict(**common, available=True, start=subset[0]['date'], end=subset[-1]['date'],
                observations=len(subset), return_sessions=len(subset) - 1,
                **s1.path_metrics(subset, initial), **_drawdown_dates(subset, initial))


def _worst_period(curve, initial_cash, sessions):
    levels = [initial_cash] + [r['equity'] for r in curve]
    if len(curve) < sessions:
        return None
    end = min(range(sessions, len(levels)), key=lambda i: levels[i] / levels[i - sessions])
    start = end - sessions
    return dict(total_return=levels[end] / levels[start] - 1, sessions=sessions,
                start=curve[start - 1]['date'] if start else curve[0]['date'],
                end=curve[end - 1]['date'], start_is_initial_cash=(start == 0))


def describe_path(curve, initial_cash, fills=(), *, post_processing=False):
    _curve_values(curve)
    result = s1.path_metrics(curve, initial_cash, fills)
    result.update(_drawdown_dates(curve, initial_cash))
    result.update(calendar_year_returns={str(y) if y < 2011 else '2011H1':
                                        result['yearly_returns'].get(str(y))
                                        for y in range(2006, 2012)},
                  focus_windows={k: window_metrics(curve, *v) for k, v in FOCUS_WINDOWS.items()},
                  worst_20_sessions=_worst_period(curve, initial_cash, 20),
                  worst_single_day=_worst_period(curve, initial_cash, 1))
    if post_processing:
        # Native fees are already embedded; no overlay fills or turnover exist.
        for key in ('traded_notional', 'turnover', 'annualized_turnover', 'commission'):
            result[key] = None
    return result


def load_inputs(daily, sp500, frozen=FROZEN_UNIVERSE):
    daily, sp500, frozen = Path(daily), Path(sp500), Path(frozen)
    files = sorted(daily.glob('*.json'), key=lambda p: p.name)
    if not files:
        raise ValueError('daily/*.json is empty')
    digest = hashlib.sha256()
    frames, quality = {}, {}
    for path in files:
        raw = path.read_bytes()
        digest.update(raw)
        rows = json.loads(raw)
        frame = pd.DataFrame([r[:6] for r in rows],
                             columns=['date', 'open', 'high', 'low', 'close', 'volume'])
        frame.index = pd.DatetimeIndex(pd.to_datetime(frame.pop('date')))
        if not frame.index.is_monotonic_increasing or frame.index.has_duplicates:
            raise ValueError(f'{path.stem}: dates must be unique and ascending')
        frame = frame.astype(float)
        valid = s1.valid_rows(frame)
        quality[path.stem] = dict(rows=len(frame), valid_rows=int(valid.sum()),
                                 invalid_rows=int((~valid).sum()))
        if valid.any():
            frames[path.stem] = frame.where(valid, np.nan)
    if 'SPY' not in frames:
        raise ValueError('SPY has no valid trading calendar')
    calendar = frames['SPY'].dropna().index
    for symbol, frame in frames.items():
        quality[symbol]['off_calendar_rows'] = int((~frame.index.isin(calendar)).sum())
    frozen_bytes, sp500_bytes = frozen.read_bytes(), sp500.read_bytes()
    requested = dict(bstock90=sorted(set(json.loads(frozen_bytes)['symbols'])),
                     sp500=sorted({s.replace('.', '-') for s in json.loads(sp500_bytes)['sp500']}))
    if len(requested['bstock90']) != 90:
        raise ValueError('frozen bStock universe must have exactly 90 names')
    meta_path = daily.parent / 'meta.json'
    meta_bytes = meta_path.read_bytes() if meta_path.exists() else None
    return dict(frames=frames, calendar=calendar, requested=requested, data_quality=quality,
                input_hashes=dict(daily=digest.hexdigest(),
                                  sp500=hashlib.sha256(sp500_bytes).hexdigest(),
                                  bstock90=hashlib.sha256(frozen_bytes).hexdigest(),
                                  meta=hashlib.sha256(meta_bytes).hexdigest() if meta_bytes else None),
                input_daily_files=[p.name for p in files],
                fetch_log=json.loads(meta_bytes) if meta_bytes else None)


def run_universe(frames, calendar, requested):
    universe = sorted(set(requested) & set(frames))
    features = s1.build_features(frames, universe, calendar)
    sessions, decisions = s1.rebalance_schedule(features, calendar)
    rebalances = [d['date'] for d in decisions]
    stock_frames = {s: frames[s] for s in universe}
    paths = {}
    for key, cash, cost in [('long', 50_000.0, 0.0013), ('short', 50_000.0, 0.0007),
                             ('L', INITIAL_CASH, 0.0013)]:
        paths[key] = s1.run_sleeve(stock_frames, sessions, {d['date']: d[key] for d in decisions},
                                   cash=cash, commission=cost, short=(key == 'short'))
    paths['SPY'] = s1.run_sleeve({'SPY': frames['SPY']}, sessions,
                                {rebalances[0]: {'SPY': 1.0}},
                                cash=INITIAL_CASH, commission=0.0013)
    long_curve, short_curve = [paths[k]['equity_curve'] for k in ('long', 'short')]
    paths['independent'] = dict(engine='backtrader', post_processing=False,
        initial_cash=INITIAL_CASH,
        equity_curve=[dict(date=a['date'], equity=a['equity'] + b['equity'])
                      for a, b in zip(long_curve, short_curve, strict=True)])
    paths['equalized'] = dict(post_processing=True, initial_cash=INITIAL_CASH,
        equity_curve=equalized_path(long_curve, short_curve, rebalances))
    for key, dd, daily in [('drawdown_latch', True, False), ('daily_loss', False, True),
                           ('combined', True, True)]:
        paths[key] = dict(initial_cash=INITIAL_CASH, **kill_switch_overlay(
            paths['equalized']['equity_curve'], rebalances, drawdown=dd, daily_loss=daily))
    for key, path in paths.items():
        fills = (paths['long']['fills'] + paths['short']['fills']
                 if key == 'independent' else path.get('fills', ()))
        path.update(descriptive_only=True, post_processing=path.get('post_processing', False))
        path['metrics'] = describe_path(path['equity_curve'], path['initial_cash'], fills,
                                        post_processing=path['post_processing'])
    return dict(universe=universe, requested_universe=requested,
                unavailable=sorted(set(requested) - set(universe)),
                start=str(sessions[0].date()), end=str(sessions[-1].date()),
                trading_sessions=len(sessions), rebalance_dates=rebalances,
                trailing_sessions_excluded=int((calendar > sessions[-1]).sum()),
                eligible_counts=[{k: d[k] for k in ('date', 'eligible_count', 'quintile_count')}
                                 for d in decisions], decisions=decisions, paths=paths)


def run_stress(daily, sp500):
    inputs = load_inputs(daily, sp500)
    frames, calendar = inputs.pop('frames'), inputs.pop('calendar')
    requested = inputs.pop('requested')
    universes = {}
    for name, symbols in requested.items():
        print(f'{name}: running native Backtrader sleeves', flush=True)
        universes[name] = dict(survivorship_biased=True,
                              **run_universe(frames, calendar, symbols))
    params = {key: s1.PARAMS[key] for key in (
        'rule', 'min_valid_bars', 'min_close', 'liquidity_window', 'min_median_dollar_volume',
        'signal', 'rebalance_trading_days', 'min_eligible', 'quantile', 'volatility_window',
        'volatility_ddof', 'long_gross', 'short_gross', 'cap', 'sleeve_cap', 'initial_cash',
        'long_initial_cash', 'short_initial_cash', 'long_cost_per_fill', 'short_cost_per_fill',
        'funding_rate', 'variant_L')}
    params.update(drawdown_latch_fraction=0.25, daily_loss_fraction=0.07,
                  calendar='valid SPY dates', schedule='S1 complete periods, final boundary included',
                  equalization='50/50 after settled rebalance close; initial entry costs included',
                  daily_resume='apply base daily return on next rebalance date',
                  flat_sessions='sessions whose base return was skipped',
                  extra_overlay_cost=0.0, equalization_commission_size_adjustment=False,
                  daily_hash='sha256 of raw daily/*.json bytes concatenated in filename order')
    return dict(descriptive_only=True, counts_toward_K=False, engine='backtrader',
                engine_version=universes['bstock90']['paths']['long']['engine_version'],
                params=params, inputs=dict(daily=str(Path(daily).resolve()),
                                           sp500=str(Path(sp500).resolve()),
                                           bstock90=str(FROZEN_UNIVERSE)),
                calendar_start=str(calendar[0].date()), calendar_end=str(calendar[-1].date()),
                calendar_sessions=len(calendar), notes=NOTES, universes=universes, **inputs)


def _pct(value):
    return '—' if value is None else f'{value:+.2%}'


def render_markdown(report):
    lines = ['# fq_us 动量规则：2008 压力测试（描述性）', '',
             '只作描述，不作裁决，不计入任何 K。所有基础交易与账户盈亏均由 Backtrader 执行；等权重置与风控覆盖为明确标注的后处理。', '',
             f"日历：{report['calendar_start']} → {report['calendar_end']}（SPY，{report['calendar_sessions']} 个交易日）；Backtrader {report['engine_version']}。", '',
             f"输入日线 SHA256（{len(report['input_daily_files'])} 个文件按文件名排序拼接）：`{report['input_hashes']['daily']}`。", '',
             '## 口径与近似', '']
    lines.extend(f'- {note}' for note in report['notes'])
    # Put both universes' main results before the long eligibility appendix.
    for name, universe in report['universes'].items():
        paths = universe['paths']
        lines += ['', f'## {name}', '',
                  f"名单 {len(universe['requested_universe'])} 只，有有效数据 {len(universe['universe'])} 只；区间 {universe['start']} → {universe['end']}，{universe['trading_sessions']} 日，{len(universe['rebalance_dates'])} 次调仓；末尾排除 {universe['trailing_sessions_excluded']} 个不完整周期交易日。存在幸存者偏差。", '',
                  '| 路径 | 总收益 | 最大回撤 | 峰日 → 谷日 | 崩盘窗收益 | 反弹窗收益 |',
                  '|---|---:|---:|---|---:|---:|']
        for key, label in PATH_LABELS.items():
            m = paths[key]['metrics']
            peak = m['drawdown_peak_date'] or '—'
            if m['drawdown_peak_is_initial']:
                peak += '（收盘前初始资金）'
            lines.append(f"| {label} | {_pct(m['total_return'])} | {m['max_drawdown']:.2%} | {peak} → {m['drawdown_trough_date'] or '—'} | {_pct(m['focus_windows']['crash']['total_return'])} | {_pct(m['focus_windows']['rebound']['total_return'])} |")
        lines += ['', '崩盘窗：2007-10-09 → 2009-03-09；反弹窗：2009-03-09 → 2009-12-31。', '',
                  '| 路径 | 2006 | 2007 | 2008 | 2009 | 2010 | 2011H1（样本截止） |',
                  '|---|---:|---:|---:|---:|---:|---:|']
        for key, label in PATH_LABELS.items():
            years = paths[key]['metrics']['calendar_year_returns']
            lines.append('| ' + label + ' | ' + ' | '.join(_pct(v) for v in years.values()) + ' |')
        lines += ['', '| 路径 | 最差 20 日收益 | 起点 → 终点 | 最差单日收益 | 日期 |',
                  '|---|---:|---|---:|---|']
        for key, label in PATH_LABELS.items():
            m = paths[key]['metrics']
            worst, day = m['worst_20_sessions'], m['worst_single_day']
            span = (f"{worst['start']}{'（收盘前初始资金）' if worst['start_is_initial_cash'] else ''} → {worst['end']}"
                    if worst else '—')
            lines.append(f"| {label} | {_pct(worst['total_return'] if worst else None)} | {span} | {_pct(day['total_return'])} | {day['end']} |")
        lines += ['', '| 风控覆盖 | 触发日期（类型） | 恢复日期 | 跳过收益的空仓日 | 收盘空仓日 |',
                  '|---|---|---|---:|---:|']
        for key in ('drawdown_latch', 'daily_loss', 'combined'):
            p = paths[key]
            triggers = '、'.join(f"{t['date']}（{'回撤锁定' if t['kind'] == 'drawdown_latch' else '单日止损'}）" for t in p['triggers']) or '无'
            lines.append(f"| {PATH_LABELS[key]} | {triggers} | {'、'.join(p['resume_dates']) or '无'} | {p['flat_sessions']} | {p['flat_at_close_sessions']} |")
        eq = paths['equalized']['metrics']
        lines += ['', f"描述：等权重置路径最大回撤 {eq['max_drawdown']:.2%}，崩盘窗 {_pct(eq['focus_windows']['crash']['total_return'])}，反弹窗 {_pct(eq['focus_windows']['rebound']['total_return'])}。以上为冻结规则在该名单及执行近似下的历史路径。"]
    for name, universe in report['universes'].items():
        lines += ['', f'## {name}：数据覆盖与每次调仓可用数量', '',
                  '有数据：' + '、'.join(universe['universe']) + '。', '',
                  '缺失或无有效数据：' + ('、'.join(universe['unavailable']) or '无') + '。', '',
                  '| 调仓日 | 可用数 | 每端五分位数 |', '|---|---:|---:|']
        lines.extend(f"| {d['date']} | {d['eligible_count']} | {d['quintile_count']} |"
                     for d in universe['eligible_counts'])
    lines += ['', '完整日权益、原生成交/订单/现金调整、选股与权重、数据质量、输入清单和其余指标见同目录 report.json。', '']
    return '\n'.join(lines)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--daily', type=Path, required=True)
    parser.add_argument('--sp500', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args(argv)
    daily, out = args.daily.resolve(), args.out.resolve()
    if out == daily or out.is_relative_to(daily) or daily.is_relative_to(out):
        parser.error('--out must be separate from --daily')
    if out == args.sp500.resolve() or out == FROZEN_UNIVERSE.resolve():
        parser.error('--out must not overwrite an input')
    if any((out / name).exists() for name in ('report.json', 'report.md')):
        raise FileExistsError('report already exists; choose an unused --out directory')
    # Fail on output permissions before spending time on native runs.
    out.mkdir(parents=True, exist_ok=True)
    report = run_stress(daily, args.sp500)
    (out / 'report.json').write_text(
        json.dumps(report, ensure_ascii=False, indent=2, allow_nan=False) + '\n', encoding='utf-8')
    (out / 'report.md').write_text(render_markdown(report), encoding='utf-8')
    print(f"reports: {out / 'report.json'} ; {out / 'report.md'}", flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
