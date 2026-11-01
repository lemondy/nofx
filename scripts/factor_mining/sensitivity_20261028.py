"""Read-only crypto universe/funding sensitivity; never changes the experiment.

Run from the repository root:
  python -m scripts.factor_mining.sensitivity_20261028 --root /path/to/factor_mining
The only output is the requested Markdown file, outside the input data tree.
"""
from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import os
from pathlib import Path

import numpy as np
import pandas as pd

from . import evaluate, store
from .config import MARKETS
from .data_fetch import ROOT
from .expr import evaluate_expr
from .panel import Panels, forward_target, load_panels, make_splits


CUTOFF = pd.Timestamp('2022-01-01')
# data_fetch.funding_daily stores a MEAN, not the day's total. Historical
# settlement counts are absent; 3 assumes the documented 8h schedule. This is
# an explicit sensitivity assumption, not an exact reconstruction of cashflows.
FUNDING_SETTLEMENTS_PER_DAY = 3
DEFAULT_OUTPUT = Path(__file__).resolve().parents[2] / 'docs/research/factor-mining-sensitivity-20261028.md'


def old_universe(panels, cutoff=CUTOFF):
    """Restrict valid OHLCV histories, then rerun the original liquidity policy."""
    close = panels.fields['close']
    symbols = close.columns[close.loc[close.index < cutoff].notna().any()]
    fields = {key: frame.loc[:, symbols].copy() for key, frame in panels.fields.items()}
    cfg = MARKETS['crypto']
    observed = fields['close'].notna()
    mask = observed & observed.cumsum().ge(cfg['min_history'])
    amount = (fields['volume'] * fields['vwap']).rolling(20, min_periods=20).mean()
    mask &= amount.where(mask).rank(axis=1, ascending=False, method='first').le(cfg['universe_n'])
    return Panels(fields, mask, windows=panels.windows)


def holding_funding(funding, h, settlements_per_day=FUNDING_SETTLEMENTS_PER_DAY):
    """Entry open[t+1], exit open[t+1+h]: sum UTC days t+1 through t+h.

    Include the entry day's 00:00 settlement, exclude exit-day settlements.
    Fixed entry notional per leg; missing days stay NaN, never forward-filled.
    """
    if h < 1 or not np.isfinite(settlements_per_day) or settlements_per_day <= 0:
        raise ValueError('positive horizon and settlements_per_day required')
    total = funding.shift(-1).copy()
    for day in range(2, h + 1):
        total += funding.shift(-day)
    return total * settlements_per_day


def funding_adjusted_ls(values, raw, accrued, mask, segment, h, cfg):
    """Same frozen Q5/Q1 selection and costs; long pays, short receives.

    A missing selected rate makes the full-segment estimate unavailable. Do
    not quietly select a different portfolio or assume missing funding is zero.
    """
    eligible_values = values.where(mask)
    span = eligible_values.max(axis=1) - eligible_values.min(axis=1)
    scale = eligible_values.abs().max(axis=1).clip(lower=1.)
    values = values.where(span > 10 * np.finfo(float).eps * scale, axis=0)
    net, _ = evaluate.long_short(values, raw, mask, segment, h, cfg)
    positions = np.flatnonzero(np.asarray(segment, bool))[::h]
    v, r, f = (frame.to_numpy() for frame in (values, raw, accrued))
    universe = mask.to_numpy()
    missing, selected = 0, 0
    for i in positions:
        eligible = universe[i] & np.isfinite(v[i]) & np.isfinite(r[i])
        count = int(eligible.sum())
        if count < max(15, cfg['min_names']):
            continue
        order = np.argsort(np.where(eligible, v[i], np.inf), kind='stable')
        sizes = np.full(5, count // 5)
        sizes[:count % 5] += 1
        legs = np.r_[order[:sizes[0]], order[count - sizes[-1]:count]]
        selected += len(legs)
        missing += int((~np.isfinite(f[i, legs])).sum())
    if missing or not len(net):
        return dict(ls_ret=None, missing_legs=missing, selected_legs=selected, n_portfolios=len(net))
    # Raw has NaNs in exactly the original places, so the evaluator keeps the
    # same names/turnover/fees. Subtracting funding also credits the short leg.
    # Missing rates on unselected names cannot alter bucket sizes/selection.
    # Selected names were checked above; the temporary zeros are never held.
    funded, _ = evaluate.long_short(values, raw - accrued.fillna(0), mask, segment, h, cfg)
    if not net.index.equals(funded.index):
        raise ValueError('funding unexpectedly changed portfolio dates')
    return dict(ls_ret=float(funded.mean()) * cfg['periods_per_year'] / h,
                missing_legs=missing, selected_legs=selected, n_portfolios=len(net))


def still_accepted(metrics, K, funding=None):
    if metrics is None:
        return False
    train, val = dict(metrics['train']), dict(metrics['val'])
    if funding is not None:
        if any(funding[s]['ls_ret'] is None for s in ('train', 'val')):
            return False
        train['ls_ret'], val['ls_ret'] = funding['train']['ls_ret'], funding['val']['ls_ret']
    return evaluate.classify(train, val, K, evaluate.t_required(K)) == 'accepted'


def analyse(root, settlements_per_day=FUNDING_SETTLEMENTS_PER_DAY):
    root = Path(root)
    zoo = store.read_json(root / 'crypto/zoo.json')
    factors = sorted((f for f in zoo['factors'] if f['status'] == 'accepted'),
                     key=lambda f: (f['horizon'], f['id']))
    ledger = store.read_jsonl(root / 'crypto/ledger.jsonl')
    trials = Counter(r['horizon'] for r in ledger)
    panels = load_panels('crypto', root)
    subset = old_universe(panels)
    if 'funding' not in panels.fields:
        raise ValueError('crypto panel has no funding field')
    cfg = MARKETS['crypto']
    full_values, subset_values, targets, accruals = {}, {}, {}, {}
    results, max_drift = [], 0.0
    ranges = {}
    for factor in factors:
        h, key = factor['horizon'], factor['expr']
        if key not in full_values:
            full_values[key] = evaluate_expr(key, panels.fields)
            subset_values[key] = evaluate_expr(key, subset.fields)
        if h not in targets:
            splits = make_splits(panels.mask.index, h, panels.windows)
            targets[h] = (splits, forward_target(panels, h), forward_target(subset, h))
            accruals[h] = holding_funding(panels.fields['funding'], h, settlements_per_day)
            ranges[h] = splits['ranges']
        splits, (excess, raw), (sub_excess, sub_raw) = targets[h]
        # Keep the registered training direction fixed; no reselection on the
        # sensitivity subsample, no holdout labels, and no evaluator changes.
        fixed_cfg = {**cfg, 'direction': factor['direction']}
        base = evaluate.evaluate_factor(full_values[key], excess, raw, panels.mask, splits, h, fixed_cfg)
        sub = evaluate.evaluate_factor(subset_values[key], sub_excess, sub_raw, subset.mask, splits, h, fixed_cfg)
        if base is None:
            raise ValueError(f"current accepted factor is no longer evaluable: {factor['id']}/{h}")
        funding = {s: funding_adjusted_ls(full_values[key] * factor['direction'], raw, accruals[h],
                                        panels.mask, splits[s], h, cfg) for s in ('train', 'val')}
        joint = {s: funding_adjusted_ls(subset_values[key] * factor['direction'], sub_raw,
                                      accruals[h].reindex(columns=subset.mask.columns), subset.mask,
                                      splits[s], h, cfg) for s in ('train', 'val')}
        for segment in ('train', 'val'):
            for metric in ('ic', 't', 'ls_ret'):
                max_drift = max(max_drift, abs(base[segment][metric] - factor[segment][metric]))
        results.append(dict(factor=factor, base=base, subset=sub, funding=funding, joint=joint,
                            subset_pass=still_accepted(sub, trials[h]),
                            funding_pass=still_accepted(base, trials[h], funding),
                            joint_pass=still_accepted(sub, trials[h], joint)))
    return dict(zoo=zoo, trials=trials, panels=panels, subset=subset, results=results,
                ranges=ranges, max_drift=max_drift, settlements_per_day=settlements_per_day)


def render_report(result, root):
    rows = result['results']
    def pair(metrics, name):
        if metrics is None:
            return 'NA'
        values = [metrics[s][name] for s in ('train', 'val')]
        return ' / '.join('NA' if v is None else (f'{v:.2%}' if name == 'ls_ret' else f'{v:.4f}')
                          for v in values)
    def passed(value):
        return '是' if value else '否'
    p, sub = result['panels'], result['subset']
    counts = {name: sum(r[name] for r in rows) for name in ('subset_pass', 'funding_pass', 'joint_pass')}
    hashes = {name: hashlib.sha256(Path(__file__).with_name(name).read_bytes()).hexdigest()
              for name in ('evaluate.py', 'expr.py')}
    lines = [
        '# 因子挖掘敏感性分析（2026-10-28）', '',
        f"只读数据根目录：`{Path(root).resolve()}`。zoo 快照：`{result['zoo']['generated_at']}`；"
        f"有效日线截至 {p.meta['end']}。仅选择 zoo 当前 `status == accepted` 的 {len(rows)} 个因子 × 持有期条目。", '',
        f"币池：原 {len(p.mask.columns)} 币，保留 2022-01-01 前至少有一条有效 OHLCV 的 {len(sub.mask.columns)} 币。"
        '子池重新计算截面算子、截面目标基准、60 日历史资格和 20 日成交额前 60 排名；沿用原日期日历、切分、成本、持有期及训练方向。', '',
        '全部 IC、HAC t（lag=h）、分层收益均调用冻结 evaluate.py；不读取 holdout 标签，不重搜表达式或翻转方向。'
        '表中每格为 **train / val**；ls_ret 为扣交易成本后的算术年化多空收益。原值来自 zoo；'
        f"同窗全池重算与原 IC/t/ls_ret 的最大绝对差为 {result['max_drift']:.3g}。", '',
        '资金费口径：panel 的 funding 是日内结算费率的**平均值**，缓存未保留每日结算次数。'
        f"本次按每天 {result['settlements_per_day']:g} 次结算估算日总费率，逐币累加 t+1 至 t+h 的 h 个完整 UTC 日；"
        '入场 t+1 开盘、出场 t+1+h 开盘，包含入场日 00:00，排除出场日。以每腿入场名义本金计费；'
        '多头付、空头收，负费率反向。费率不复利，也未按持有期内价格变化调整名义本金。'
        '结算频次可能变化，因此含费结果是敏感性估算，精确现金流需原始结算事件和持仓名义金额。'
        '保留原分层持仓与换手，缺失选中币的资金费则该段记 NA，绝不按零补齐或换币。', '',
        '是否仍满足 accepted：沿用 ledger 当前每周期 K 和冻结 classify（train t 门槛、正 IC、两半符号一致；'
        'val IC>0、t≥1.65；两段 ls_ret>0）。子池/资金费/联合三列分别只改币池、只加资金费、两者同时改；'
        '不重新判因子间冗余，也不将结果写回 zoo/ledger。', '',
        f"汇总：币池敏感性仍满足 **{counts['subset_pass']}/{len(rows)}**；"
        f"资金费敏感性仍满足 **{counts['funding_pass']}/{len(rows)}**；"
        f"联合敏感性仍满足 **{counts['joint_pass']}/{len(rows)}**。", '',
        '| 因子 | h(日) | 原 IC | 子池 IC | 原 t | 子池 t | 原 ls_ret | 子池 ls_ret | 含资金费 ls_ret（原池） | 子池/资金费/联合仍 accepted |',
        '|---|---:|---:|---:|---:|---:|---:|---:|---:|---|',
    ]
    for row in rows:
        f = row['factor']
        flags = ' / '.join(passed(row[k]) for k in ('subset_pass', 'funding_pass', 'joint_pass'))
        lines.append(f"| {f['id']} | {f['horizon']} | {pair(f, 'ic')} | {pair(row['subset'], 'ic')} | "
                     f"{pair(f, 't')} | {pair(row['subset'], 't')} | {pair(f, 'ls_ret')} | "
                     f"{pair(row['subset'], 'ls_ret')} | {pair(row['funding'], 'ls_ret')} | {flags} |")
    lines += ['', '切分及多重检验门槛：', '', '| h | K | train | val | train t 门槛 |',
              '|---:|---:|---|---|---:|']
    for h, ranges in sorted(result['ranges'].items()):
        K = result['trials'][h]
        lines.append(f"| {h} | {K} | {' ～ '.join(ranges['train'])} | {' ～ '.join(ranges['val'])} | {evaluate.t_required(K):.6f} |")
    missing = sum(v['missing_legs'] for r in rows for name in ('funding', 'joint') for v in r[name].values())
    selected = sum(v['selected_legs'] for r in rows for name in ('funding', 'joint') for v in r[name].values())
    lines += ['', f'资金费覆盖：所选多空腿 × 调仓日中缺失 {missing}/{selected}（全池与子池合计，重复持仓重复计数）。', '',
              '保留币：' + '、'.join(sub.mask.columns) + '。', '',
              '局限：该子池仍来自 2026-10-18 的存活快照，不能消除退市合约缺失造成的幸存者偏差。'
              '完整修复需要 data.binance.vision 的历史合约数据，需联网，本次未做。'
              '本报告提供是否另开评估器实验的证据，不修改冻结评估器或当前实验状态。', '',
              '复现（从仓库根目录）：', '', '```bash',
              f'python3 -m scripts.factor_mining.sensitivity_20261028 --root {Path(root).resolve()}', '```', '',
              '冻结源文件 SHA256：', '']
    lines.extend(f'- `{name}`：`{digest}`' for name, digest in hashes.items())
    return '\n'.join(lines) + '\n'


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(os.environ.get('FACTOR_MINING_DIR', ROOT)))
    parser.add_argument('--output', type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument('--funding-settlements-per-day', type=float, default=FUNDING_SETTLEMENTS_PER_DAY)
    args = parser.parse_args(argv)
    # Reject an accidental output path into any part of the read-only data tree.
    if args.output.resolve().is_relative_to(args.root.resolve().parent):
        parser.error('--output must be outside the research data directory')
    result = analyse(args.root, args.funding_settlements_per_day)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(render_report(result, args.root), encoding='utf-8')
    counts = {key: sum(row[key] for row in result['results'])
              for key in ('subset_pass', 'funding_pass', 'joint_pass')}
    print(f"wrote {args.output}; accepted={len(result['results'])}; {counts}")
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
