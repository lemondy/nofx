"""Offline implementation of the frozen fq_crypto arithmetic admission study."""
from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import math
from pathlib import Path
from statistics import NormalDist
import sys
import time

import numpy as np
import pandas as pd

from . import config, ensemble, ensemble_forward_study as study, evaluate, expr, panel
from .ml_multifeature_study import guard_hashes
from .style_neutral_study import input_hashes, weekly_statistics
from scripts.backtest.evolution_ledger import append_event, next_k, read_rows


PREREG = 'docs/research/2026-11-07-fq-crypto-admission-arith-prereg.md'
IDEA = 'fq-crypto-admission-arith-v1'
HORIZONS = (1, 3, 7)
GATES = ('original', 'S_f', 'A0', 'A')
Z_K = NormalDist().inv_cdf(1 - .05 / 14)
DATA_ROOT = Path('/Users/zhangyun/workspace/nofx/data')
HISTORY_LEDGER = DATA_ROOT / 'evolution/ledger.jsonl'


def ls_t(returns):
    """Use the sample SD of nonoverlapping holding-period returns, without HAC."""
    x = np.asarray(returns, dtype=float)
    if len(x) < 2 or not np.isfinite(x).all():
        return math.nan
    mean, sd = float(x.mean()), float(x.std(ddof=1))
    return mean / (sd / math.sqrt(len(x))) if sd else (
        math.copysign(math.inf, mean) if mean else math.nan)


def return_statistics(returns):
    x = np.asarray(returns, dtype=float)
    return dict(n=len(x), mean=float(x.mean()) if len(x) else None,
                sample_sd=float(x.std(ddof=1)) if len(x) > 1 else None, ls_t=ls_t(x))


def evaluator_values(values, mask):
    """Apply evaluate_factor's constant-cross-section screen before direction."""
    eligible = values.where(mask)
    span = eligible.max(axis=1) - eligible.min(axis=1)
    scale = eligible.abs().max(axis=1).clip(lower=1.)
    return values.where(span > 10 * np.finfo(float).eps * scale, axis=0)


def funding_long_short(values, raw, mask, segment, h, cfg, funding):
    """Add carry to the frozen evaluator's exact portfolios and net returns."""
    base, turnover = evaluate.long_short(values, raw, mask, segment, h, cfg)
    rates = funding.reindex(index=values.index, columns=values.columns)
    rates = rates.where(np.isfinite(rates))
    accrued = study.holding_funding(rates.fillna(0.), h).to_numpy()
    observed = rates.notna().to_numpy()
    positions = np.flatnonzero(np.asarray(segment, dtype=bool))[::h]
    v, r, universe = values.to_numpy(), raw.to_numpy(), mask.to_numpy()
    dates, adjustments, rows = [], [], []
    covered = selected = 0
    for i in positions:
        eligible = universe[i] & np.isfinite(v[i]) & np.isfinite(r[i])
        count = int(eligible.sum())
        if count < max(15, cfg['min_names']):
            continue
        order = np.argsort(np.where(eligible, v[i], np.inf), kind='stable')
        sizes = np.full(5, count // 5)
        sizes[:count % 5] += 1
        short = order[:sizes[0]]
        long = order[count - sizes[-1]:count]
        if i + h >= len(values):
            raise ValueError('selected holding period exceeds the funding calendar')
        long_f, short_f = float(accrued[i, long].mean()), float(accrued[i, short].mean())
        legs = np.r_[long, short]
        selected += h * len(legs)
        covered += int(observed[i + 1:i + h + 1, legs].sum())
        dates.append(values.index[i])
        adjustments.append(-long_f + short_f)
        rows.append(dict(long_price=float(r[i, long].mean()), short_price=float(-r[i, short].mean()),
                         long_funding=-long_f, short_funding=short_f))
    if not base.index.equals(pd.DatetimeIndex(dates)):
        raise AssertionError('funding portfolios differ from evaluate.long_short')
    net = base + np.asarray(adjustments)
    if not np.any(rates.fillna(0.).to_numpy()):
        pd.testing.assert_series_equal(net, base, check_exact=True)
    return dict(base=base, net=net, turnover=turnover, legs=pd.DataFrame(rows, index=base.index),
                coverage=dict(covered=covered, selected=selected,
                              fraction=covered / selected if selected else None))


def ic_pass(factor, K, t_req):
    return evaluate.classify(dict(factor['train'], ls_ret=1.),
                             dict(factor['val'], ls_ret=1.), K, t_req) == 'accepted'


def gate_status(factor, K, t_req, gate='original', metrics=None):
    original = evaluate.classify(factor['train'], factor['val'], K, t_req)
    if gate == 'original' or original == 'rejected':
        return original
    if gate not in GATES:
        raise ValueError(f'unknown gate: {gate}')
    if not ic_pass(factor, K, t_req):
        return 'candidate'
    if metrics is None:
        raise ValueError('IC-passing factors require arithmetic metrics')
    if gate == 'S_f':
        passed = all(metrics[s]['funded']['mean'] is not None and
                     metrics[s]['funded']['mean'] > 0 for s in ('train', 'val'))
    else:
        name = 'unfunded' if gate == 'A0' else 'funded'
        passed = all(metrics[s][name]['ls_t'] >= 1.65 for s in ('train', 'val'))
    return 'accepted' if passed else 'candidate'


def reclassify(factors, K, panels, splits, cfg, values_cache, gate='original', metrics=None):
    """Reproduce Miner's ordering, positive correlation rule and kept-set loop."""
    result = {}
    for h in cfg['horizons']:
        dates = panels.mask.index[splits[h]['train'] | splits[h]['val']][::5]
        kept = []
        current = sorted((f for f in factors if f['horizon'] == h),
                         key=lambda f: (-f['train']['t'], f['first_seen'], f['run_id'], f['id']))
        for factor in current:
            key = (h, factor['id'])
            f = dict(factor)
            f['status'] = gate_status(f, K[h], evaluate.t_required(K[h]), gate,
                                      metrics.get(key) if metrics is not None else None)
            f['gate_status'] = f['status']
            f['corr_max'], f['corr_with'] = 0., None
            if f['status'] in ('accepted', 'candidate'):
                values = values_cache[f['id']].loc[dates] * f['direction']
                rank = values.where(panels.mask.loc[dates]).rank(axis=1, method='average')
                for other, other_rank in kept:
                    corr = evaluate.cross_corr(rank, other_rank, dates)
                    if corr > f['corr_max']:
                        f['corr_max'], f['corr_with'] = corr, other['id']
                if f['corr_max'] >= config.CORR_REDUNDANT:
                    f['status'] = 'redundant'
                elif f['status'] == 'accepted':
                    kept.append((f, rank))
            result[key] = f
    return result


def reproduce_s0(factors, K, panels, splits, cfg, values_cache):
    classified = reclassify(factors, K, panels, splits, cfg, values_cache)
    mismatches = [dict(horizon=f['horizon'], id=f['id'], expected=f['status'],
                       actual=classified[f['horizon'], f['id']]['status']) for f in factors
                  if f['status'] != classified[f['horizon'], f['id']]['status']]
    result = dict(n=len(factors), status_mismatches=mismatches, accepted_checked=0,
                  ls_comparisons=0, max_ls_error=0., ls_mismatches=[])
    for f in factors:
        if f['status'] != 'accepted':
            continue
        h = f['horizon']
        values = evaluator_values(values_cache[f['id']], panels.mask) * f['direction']
        excess, raw = panel.forward_target(panels, h)
        # Reuse evaluate_factor as an independent check of the metric path.
        reproduced = evaluate.evaluate_factor(values_cache[f['id']], excess, raw, panels.mask,
                                               splits[h], h, dict(cfg, direction=f['direction']))
        if reproduced is None:
            raise AssertionError(f'accepted factor became unevaluable: {f["id"]}/{h}')
        for segment in ('train', 'val'):
            net, _ = evaluate.long_short(values, raw, panels.mask, splits[h][segment], h, cfg)
            actual = float(net.mean() * cfg['periods_per_year'] / h)
            error = abs(actual - f[segment]['ls_ret'])
            result['max_ls_error'] = max(result['max_ls_error'], error)
            result['ls_comparisons'] += 1
            if error > 1e-9 or abs(actual - reproduced[segment]['ls_ret']) > 1e-9:
                result['ls_mismatches'].append(dict(id=f['id'], horizon=h, segment=segment,
                                                    actual=actual, expected=f[segment]['ls_ret']))
        result['accepted_checked'] += 1
    result['passed'] = not mismatches and not result['ls_mismatches']
    return result


def snapshot_trials(path, generated_at, zoo):
    cutoff = pd.Timestamp(generated_at)
    counts = Counter()
    total = 0
    for line in Path(path).read_text().splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        if pd.Timestamp(row['ts']) <= cutoff:
            counts[int(row['horizon'])] += 1
            total += 1
    result = {}
    for h in HORIZONS:
        metadata = zoo['horizons'][str(h)]
        required = evaluate.t_required(counts[h])
        if counts[h] != metadata['trials'] or abs(required - metadata['t_req']) > 1e-12:
            raise ValueError(f'snapshot miner-ledger K/t_req disagrees with zoo: h={h}')
        result[h] = dict(K=counts[h], t_req=required)
    return result, total


class ValuesCache:
    """Disk-backed float64 expression panels, bounded process memory, no rounding."""

    def __init__(self, directory, factors, panels, progress=print):
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.expressions = {f['id']: f['expr'] for f in factors}
        self.panels = panels
        self.progress = progress
        self.evaluated = set()

    def __getitem__(self, factor_id):
        expression = self.expressions[factor_id]
        digest = hashlib.sha256(expression.encode()).hexdigest()[:16]
        path = self.directory / f'{factor_id}-{digest}.npy'
        if not path.exists():
            values = expr.evaluate_expr(expression, self.panels.fields)
            temporary = path.with_suffix('.tmp')
            with temporary.open('wb') as stream:
                np.save(stream, values.to_numpy(), allow_pickle=False)
            temporary.replace(path)
        self.evaluated.add(factor_id)
        if len(self.evaluated) % 200 == 0 and factor_id != getattr(self, '_last_progress', None):
            self.progress(f'已读取表达式缓存 {len(self.evaluated)}/{len(self.expressions)}')
            self._last_progress = factor_id
        array = np.load(path, mmap_mode='r', allow_pickle=False)
        if array.shape != self.panels.mask.shape:
            raise ValueError('expression cache shape mismatch')
        return pd.DataFrame(array, index=self.panels.mask.index, columns=self.panels.mask.columns)


def verdict(n, bad_scores, z, halves, *, empty=False, unchanged=False):
    if empty:
        return 'no_factors'
    if unchanged:
        return 'unchanged'
    if n < 120 or bad_scores:
        return 'invalid'
    if z >= Z_K:
        return 'strong'
    if z >= 1 and all(x > 0 for x in halves):
        return 'screen_pass'
    return 'fail'


def paired_statistics(candidate, baseline):
    pair = pd.concat([candidate.rename('candidate'), baseline.rename('baseline')], axis=1,
                     join='inner').sort_index()
    if not np.isfinite(pair.to_numpy()).all():
        raise ValueError('nonfinite backtest daily returns')
    delta = pair.candidate - pair.baseline
    result = weekly_statistics(delta)
    midpoint = delta.index[0] + (delta.index[-1] - delta.index[0]) / 2
    halves = [delta.loc[delta.index <= midpoint], delta.loc[delta.index > midpoint]]
    result.update(half_means=[float(x.mean()) for x in halves],
                  half_ranges=[[study.day(x.index[0]), study.day(x.index[-1])] for x in halves],
                  total_return_delta=float((1 + pair.candidate).prod() - (1 + pair.baseline).prod()),
                  daily_delta={study.day(d): float(v) for d, v in delta.items()}, z_K=Z_K)
    return result


def append_ledger(path, result):
    if result['verdict'] in ('no_factors', 'unchanged'):
        return None

    def build(k, previous):
        if k != 7 or next_k(previous, 'fq_crypto') != 7:
            raise ValueError('fq_crypto ledger must have exactly 6 counted S1 rows; K must be 7')
        if any(r.get('idea_id') == IDEA and r.get('stage') == 'S1' for r in previous):
            raise ValueError('this S1 idea is already recorded')
        return dict(stage='S1', change_type='E', idea_id=IDEA,
                    baseline_version='ensemble@5e3599f8', verdict=result['verdict'],
                    preregistered_K=7, z=result['paired']['forward']['z'], z_K=Z_K,
                    paired=result['paired'], input_hashes=result['hashes'],
                    prereg_path=PREREG, deviations=result['deviations'])

    return append_event(path, 'fq_crypto', build)


def validate_outputs(root, out, ledger=None, miner_ledger=None):
    root, out = Path(root).resolve(), Path(out).resolve()
    outputs = [out] + [out / name for name in
                       ('report.json', 'report.md', 'factors.csv', 'attempts.jsonl', 'run.log')]
    if ledger is not None:
        outputs.append(Path(ledger).resolve())
    study.validate_outputs(root, outputs)
    if ledger is not None and (out in Path(ledger).resolve().parents and
                               (out / 'cache' == Path(ledger).resolve() or
                                out / 'cache' in Path(ledger).resolve().parents)):
        raise ValueError('ledger must not overwrite expression cache')
    for path in outputs:
        if miner_ledger is not None and path == Path(miner_ledger).resolve():
            raise ValueError('output must not overwrite miner ledger')
    return out


def factor_metrics(factor, values, panels, splits, raw, funding, cfg):
    values = evaluator_values(values, panels.mask) * factor['direction']
    result = {}
    for segment in ('train', 'val'):
        run = funding_long_short(values, raw, panels.mask, splits[segment], factor['horizon'], cfg, funding)
        result[segment] = dict(ls_ret=factor[segment]['ls_ret'],
                               unfunded=return_statistics(run['base']), funded=return_statistics(run['net']),
                               funding_coverage=run['coverage'])
    return result


def unit_leg_description(run):
    result = dict(unfunded=return_statistics(run['base']), funded=return_statistics(run['net']),
                  funding_coverage=run['coverage'])
    for leg in ('long', 'short'):
        values = run['legs'][leg + '_price'] + run['legs'][leg + '_funding']
        result[leg] = dict(mean_price=float(run['legs'][leg + '_price'].mean()),
                           mean_funding=float(run['legs'][leg + '_funding'].mean()),
                           mean_net_before_cost=float(values.mean()), arithmetic_sum=float(values.sum()),
                           compounded_return=float((1 + values).prod() - 1), n=len(values))
    return result


def score_correlations(score, baseline, vol, panels, segments):
    result = {}
    for name, selected in segments.items():
        result[name] = {}
        for key, other in [('B', baseline), ('vol_30', vol)]:
            corr = evaluate.cs_rank_ic(score.loc[selected], other.loc[selected],
                                       panels.mask.loc[selected], 2).dropna()
            result[name][key] = dict(n=len(corr), mean=float(corr.mean()) if len(corr) else None)
    return result


def count_table(factors, classified, K):
    rows = []
    for h in HORIZONS:
        group = [f for f in factors if f['horizon'] == h]
        for gate in GATES:
            statuses = Counter(classified[gate][h, f['id']]['status'] for f in group)
            # Redundancy is separate from failure of the arithmetic admission gate.
            pre_redundancy_rejected = sum(
                ic_pass(f, K[h], evaluate.t_required(K[h])) and
                classified[gate][h, f['id']].get('gate_status',
                    evaluate.classify(f['train'], f['val'], K[h], evaluate.t_required(K[h]))) != 'accepted'
                for f in group)
            rows.append(dict(horizon=h, gate=gate, total=len(group),
                             **{s: statuses[s] for s in ('accepted', 'candidate', 'redundant', 'rejected')},
                             ic_pass=sum(ic_pass(f, K[h], evaluate.t_required(K[h])) for f in group),
                             ic_gate_rejected=pre_redundancy_rejected))
    return rows


def render_report(result):
    table, fmt = study.table, study.fmt
    lines = ['# fq_crypto 算术多空准入影响研究', '',
             f"判定 **{result['verdict']}**。采用预注册的 S1 离线筛选流程。", '',
             f"预注册 `{PREREG}`。生成时间 `{result['generated_at']}`。", '',
             '## 输入与 S0', '',
             f"S0 {'通过' if result['s0']['passed'] else '未通过'}。共 {result['s0']['n']} 个状态，"
             f"状态差异 {len(result['s0']['status_mismatches'])} 个。"
             f"accepted 核对 {result['s0']['accepted_checked']} 个，ls_ret 比较 {result['s0']['ls_comparisons']} 次，"
             f"最大误差 {result['s0']['max_ls_error']:.12g}。", '',
             table(['h', '挖掘 K', 't_req', '训练', '验证'],
                   [[h, result['miner_trials'][str(h)]['K'], fmt(result['miner_trials'][str(h)]['t_req'], 10),
                     '～'.join(result['splits'][str(h)]['train']), '～'.join(result['splits'][str(h)]['val'])]
                    for h in HORIZONS]), '',
             f"完整 IC 门槛通过 {result['evaluation']['ic_pairs']} 个因子/horizon，"
             f"涉及 {result['evaluation']['ic_unique']} 个表达式。"
             f"实际计算 {result['evaluation']['evaluated_unique']} 个表达式。", '',
             '## 各门槛数量', '',
             table(['h', '门槛', 'accepted', 'candidate', 'redundant', 'rejected', 'IC通过', 'IC通过但收益门槛拒绝'],
                   [[r[k] for k in ('horizon', 'gate', 'accepted', 'candidate', 'redundant', 'rejected',
                                    'ic_pass', 'ic_gate_rejected')] for r in result['counts']]), '',
             '数量以 zoo 中保留的因子为分母。收益门槛拒绝在冗余循环之前统计。', '',
             '## 线上 7 因子', '',
             table(['因子', '段', '旧ls_ret年化', '未含资金费均值', '含资金费均值', 't未含', 't含'],
                   [[f['id'], segment, fmt(f['metrics'][segment]['ls_ret'], 8),
                     fmt(f['metrics'][segment]['unfunded']['mean'], 8),
                     fmt(f['metrics'][segment]['funded']['mean'], 8),
                     fmt(f['metrics'][segment]['unfunded']['ls_t'], 6),
                     fmt(f['metrics'][segment]['funded']['ls_t'], 6)]
                    for f in result['live'] for segment in ('train', 'val')]), '',
             table(['因子', 'S_f通过', 'A0通过', 'A通过', 'S_f状态', 'A0状态', 'A状态'],
                   [[f['id']] + [f['passes'][g] for g in ('S_f', 'A0', 'A')] +
                    [f['statuses'][g] for g in ('S_f', 'A0', 'A')] for f in result['live']]), '',
             '## B′ 组成与风格', '',
             f"B′ 因子数为 {len(result['candidate'])}。", '',
             table(['因子', '表达式', '族', '训练IC t', '训练ls_t', '验证ls_t'],
                   [[f['id'], f['expr'], f['family'], fmt(f['train_t'], 6), fmt(f['train_ls_t'], 6),
                     fmt(f['val_ls_t'], 6)] for f in result['candidate']]), '',
             table(['段', '对 B 的截面秩相关', '对 vol_30 的截面秩相关'],
                   [[segment, fmt(c['B']['mean'], 6), fmt(c['vol_30']['mean'], 6)]
                    for segment, c in result['correlations'].items()]), '',
             '## 分段回测与训练单位腿描述', '',
             table(['段', '组', '净收益', '夏普', '最大回撤', '资金费金额', '换手'],
                   [[segment, arm] + [fmt(run['metrics'][k], 8) for k in
                      ('total_return', 'sharpe', 'max_drawdown', 'funding_paid', 'turnover')]
                    for segment, arms in result['backtests'].items() for arm, run in arms.items()]), '',
             table(['组', '单位腿', '价格均值/期', '资金费均值/期', '含资金费未扣交易费均值/期', '复利描述'],
                   [[arm, leg] + [fmt(description[leg][k], 8) for k in
                      ('mean_price', 'mean_funding', 'mean_net_before_cost', 'compounded_return')]
                    for arm, description in result['train_units'].items() for leg in ('long', 'short')]), '',
             '训练仅描述每条腿名义为 1 的非重叠持有期，复利为单位腿数学描述。验证和前向采用 Backtrader 原生净值。', '',
             '## 资金费覆盖', '',
             table(['段', '可用mask格', '有资金费格', '覆盖率'],
                   [[s, c['eligible'], c['covered'], fmt(c['fraction'], 6)]
                    for s, c in result['funding_coverage'].items()]), '',
             '缺失资金费记为 0。逐因子训练/验证选中腿逐日覆盖率和原生回测持仓覆盖计数保存在 JSON。', '',
             '## 配对判定', '']
    if result['paired']:
        lines += [table(['段', '天数', '周数', '日均Δ', '周聚类SE', 'z', '前半Δ均值', '后半Δ均值', '累计收益差'],
                        [[s, p['n'], p['n_weeks']] + [fmt(p[k], 8) for k in ('mean_delta', 'se', 'z')] +
                         [fmt(x, 8) for x in p['half_means']] + [fmt(p['total_return_delta'], 8)]
                         for s, p in result['paired'].items()]), '']
    else:
        lines += ['B′ 为空或与 B 相同，配对 z、前后半段 Δ 均值及前向累计收益差均不适用。', '']
    lines += [f"预注册 S1 K=7，z_K={Z_K:.10f}。前向区间 {study.FORWARD_START}～{result['panel_meta']['end']}。"
              f"判定 {result['verdict']}。", '',
              f"账本写入 {bool(result.get('ledger_event'))}。已有计数 S1 为 {result['ledger_count_before']}。", '',
              '## 偏离与歧义处理', '']
    lines += [f'- {d}' for d in result['deviations']]
    lines += ['', '## 局限与证据索引', '',
              '- 前向数据已经多项研究使用，约 180 天的统计效力有限，本轮只作筛选。',
              '- 仅改变快照既有候选的准入，未重新挖掘。币池有幸存者偏差。验证段参与选因子，配对结果只作描述。',
              '- 收益标签采用 t+1 到 t+1+h 开盘。持有期不重叠，ls_t 用样本标准差，不作 HAC。',
              '- 未做参数搜索。新增收益门槛固定为 1.65。主判定只用前向段，周聚类 SE 和 z_K 沿用预注册。',
              '- 实现依据为 evaluate.classify/long_short/evaluate_factor/cross_corr/cs_rank_ic、Miner._classify_and_correlate、ensemble.select_ensemble_factors/ensemble_scores、panel.make_splits/forward_target。',
              '- 资金费累积复用 ensemble_forward_study.holding_funding。订单与盈亏复用该模块 backtest，样式采用 ML 的 Std($ret, 30)。',
              f"- 命令 `{result['command']}`。",
              f"- 本目录研究启动 {result['attempt']} 次。输入哈希、运行耗时、逐日配对差、执行审计见 report.json。", '']
    return '\n'.join(lines)


def run_study(root, miner_ledger, out, allow_input_drift=False, progress=print):
    started = time.monotonic()
    root, out = Path(root).resolve(), Path(out).resolve()
    # Existing research helpers take the parent of the market directory.
    parent = root.parent
    hashes, file_count = input_hashes(parent, 'crypto')
    mismatches = guard_hashes(hashes, allow_input_drift)
    frozen, _, provenance = study.load_frozen(parent)
    zoo = json.loads((root / 'zoo.json').read_text())
    factors = zoo['factors']
    if len(factors) != 13157 or len(frozen[3]) != 7:
        raise ValueError('the registered snapshot requires 13,157 factors and seven live h=3 factors')
    panels = panel.load_panels('crypto', parent)
    cfg = config.MARKETS['crypto']
    splits = {h: panel.make_splits(panels.mask.index, h, panels.windows) for h in HORIZONS}
    miner_trials, cutoff_rows = snapshot_trials(miner_ledger, zoo['generated_at'], zoo)
    K = {h: miner_trials[h]['K'] for h in HORIZONS}
    live_ids = {f['id'] for f in frozen[3]}
    eligible = [f for f in factors if ic_pass(f, K[f['horizon']], miner_trials[f['horizon']]['t_req'])]
    eligible_keys = {(f['horizon'], f['id']) for f in eligible}
    evaluated = [f for f in factors if (f['horizon'], f['id']) in eligible_keys or
                 (f['horizon'] == 3 and f['id'] in live_ids)]
    out.mkdir(parents=True, exist_ok=True)
    attempts_path = out / 'attempts.jsonl'
    attempt = len(read_rows(attempts_path)) + 1
    with attempts_path.open('a') as stream:
        stream.write(json.dumps(dict(attempt=attempt, started_at=datetime.now(timezone.utc).isoformat())) + '\n')
    correlation_factors = [f for f in factors if evaluate.classify(
        f['train'], f['val'], K[f['horizon']], miner_trials[f['horizon']]['t_req']) != 'rejected']
    cache = ValuesCache(out / 'cache' / hashes['crypto.aggregate'], correlation_factors + evaluated,
                        panels, progress)
    progress(f'完整 IC 通过 {len(eligible)} 个因子/horizon，{len({f["id"] for f in eligible})} 个表达式。'
             f'含冗余核对共 {len(cache.expressions)} 个表达式，先做 S0。')
    s0 = reproduce_s0(factors, K, panels, splits, cfg, cache)
    if not s0['passed']:
        (out / 'report.json').write_text(json.dumps(study.json_safe(dict(s0=s0)), ensure_ascii=False, indent=2))
        (out / 'report.md').write_text('# S0 复现未通过\n\n' + json.dumps(s0, ensure_ascii=False, indent=2))
        raise ValueError('S0 failed; no new admission metrics or S1 were computed')
    progress(f"S0 通过，{s0['n']} 个状态一致，ls_ret 最大误差 {s0['max_ls_error']:.3g}。")
    funding = panels.fields.get('funding', pd.DataFrame(np.nan, index=panels.mask.index,
                                                       columns=panels.mask.columns))
    targets = {h: panel.forward_target(panels, h)[1] for h in HORIZONS}
    metrics = {}
    for i, f in enumerate(evaluated, 1):
        h = f['horizon']
        metrics[h, f['id']] = factor_metrics(f, cache[f['id']], panels, splits[h], targets[h], funding, cfg)
        if i % 200 == 0:
            progress(f'算术多空统计 {i}/{len(evaluated)}')
    classified = {gate: reclassify(factors, K, panels, splits, cfg, cache, gate, metrics) for gate in GATES}
    accepted = list(classified['A'].values())
    candidate = ensemble.select_ensemble_factors(accepted, 3)
    baseline_factors = frozen[3]
    baseline = ensemble.ensemble_scores(baseline_factors, panels, values_cache=cache)
    candidate_score = ensemble.ensemble_scores(candidate, panels, values_cache=cache) if candidate else None
    segments = {s: splits[3][s] for s in ('train', 'val')}
    segments['forward'] = pd.Series(panels.mask.index >= pd.Timestamp(study.FORWARD_START),
                                     index=panels.mask.index)
    coverage = {}
    for name, selected in segments.items():
        eligible_cells = panels.mask.loc[selected]
        denominator = int(eligible_cells.to_numpy().sum())
        covered = int((eligible_cells & np.isfinite(funding.loc[selected])).to_numpy().sum())
        coverage[name] = dict(eligible=denominator, covered=covered,
                              fraction=covered / denominator if denominator else None)
    live, csv_rows = [], []
    for f in evaluated:
        h, id_ = f['horizon'], f['id']
        key = h, id_
        statuses = {g: classified[g][key]['status'] for g in GATES}
        passes = {g: gate_status(f, K[h], miner_trials[h]['t_req'], g, metrics[key]) == 'accepted'
                  for g in GATES}
        row = dict(id=id_, horizon=h, expr=f['expr'], old_status=f['status'], **statuses)
        for seg in ('train', 'val'):
            row.update({f'{seg}_{label}': value for label, value in dict(
                ls_ret=metrics[key][seg]['ls_ret'],
                unfunded_mean=metrics[key][seg]['unfunded']['mean'],
                funded_mean=metrics[key][seg]['funded']['mean'],
                unfunded_ls_t=metrics[key][seg]['unfunded']['ls_t'],
                funded_ls_t=metrics[key][seg]['funded']['ls_t'],
                funding_coverage=metrics[key][seg]['funding_coverage']['fraction']).items()})
        csv_rows.append(row)
        if h == 3 and id_ in live_ids:
            live.append(dict(id=id_, expr=f['expr'], metrics=metrics[key], statuses=statuses, passes=passes))
    composition = [dict(id=f['id'], expr=f['expr'], family=f['family'], direction=f['direction'],
                         train_t=f['train']['t'], train_ls_t=metrics[3, f['id']]['train']['funded']['ls_t'],
                         val_ls_t=metrics[3, f['id']]['val']['funded']['ls_t']) for f in candidate]
    bad_scores = 0
    if candidate_score is not None:
        bad_scores = int((panels.mask.loc[segments['forward']] &
                          ~np.isfinite(candidate_score.loc[segments['forward']])).to_numpy().sum())
    unchanged = {f['id'] for f in candidate} == live_ids
    result = dict(generated_at=datetime.now(timezone.utc).isoformat(), hashes=hashes,
                  hash_mismatches=mismatches, file_count=file_count, provenance=provenance,
                  miner_trials=miner_trials, miner_ledger_cutoff=zoo['generated_at'],
                  miner_ledger_counted_rows=cutoff_rows, panel_meta=panels.meta,
                  splits={h: splits[h]['ranges'] for h in HORIZONS}, s0=s0,
                  evaluation=dict(ic_pairs=len(eligible), ic_unique=len({f['id'] for f in eligible}),
                                  evaluated_pairs=len(evaluated), evaluated_unique=len(cache.evaluated),
                                  correlation_extra_unique=len(set(cache.expressions) -
                                                               {f['id'] for f in evaluated})),
                  counts=count_table(factors, classified, K), live=live, candidate=composition,
                  funding_coverage=coverage, bad_forward_eligible_scores=bad_scores,
                  correlations={}, backtests={}, train_units={}, paired={}, attempt=attempt,
                  ledger_count_before=next_k(read_rows(HISTORY_LEDGER), 'fq_crypto') - 1,
                  deviations=[
                      'CLI 的 root 指向 crypto 目录，现有 load_frozen/load_panels/input_hashes 接收其父目录，已作路径适配。',
                      '性能提示与预注册完整冗余循环冲突。未通过完整 IC 的 candidate 也会随 kept 集变化而改变冗余状态。按预注册优先，收益统计仅计算完整 IC 通过者及线上因子，冗余核对额外计算 688 个表达式，未冻结其状态。',
                      'zoo.horizons 的 rejected 数包含未保留表达式的历史试验，本表只计 zoo.factors 中 13,157 条，与逐因子 S0 一致。',
                      '验证段按 h=3 的 make_splits 原样取 2025-05-08～2026-05-02，前向段按预注册固定 2026-05-04 开始。',
                      'B′ 与 B 相同按因子 id 集合判断，顺序不影响等权合成。非有限分数仅在前向 mask 内检查，mask 外缺失是正常不可交易格。',
                      'ls_t 在 n<2、非有限收益或零均值零方差时无定义，收益门槛不通过。非零常数收益的 t 取相应符号无穷。',
                      '训练单位腿描述按 evaluator 的相同入场及五分位，不运行训练订单回测。B′ 为空时仍报告 B 的训练、验证和前向描述。',
                      '仅新增任务指定的两个源码文件，报告、缓存和隔离账本写输出目录，研究日志由主 agent 后续登记。',
                  ], command='python -m scripts.factor_mining.admission_arith_study ' + ' '.join(sys.argv[1:]))
    if mismatches:
        result['deviations'].append('--allow-input-drift 放行哈希差异 ' + json.dumps(mismatches))
    if attempt > 1:
        result['deviations'].append(f'研究启动 {attempt} 次，之前启动见 attempts.jsonl，崩溃后仅修实现。')
    arms = {'B': baseline}
    if candidate_score is not None:
        arms['B_prime'] = candidate_score
        vol = expr.evaluate_expr('Std($ret, 30)', panels.fields)
        result['correlations'] = score_correlations(candidate_score, baseline, vol, panels, segments)
    for arm, score in arms.items():
        run = funding_long_short(evaluator_values(score, panels.mask), targets[3], panels.mask,
                                 splits[3]['train'], 3, cfg, funding)
        result['train_units'][arm] = unit_leg_description(run)
    returns = {}
    for segment, bounds in [('val', splits[3]['ranges']['val']),
                             ('forward', (study.FORWARD_START, panels.meta['end']))]:
        result['backtests'][segment] = {}
        for arm, score in arms.items():
            if arm == 'B_prime' and bad_scores:
                continue
            progress(f'Backtrader {segment} {arm}')
            run, daily, _ = study.backtest(panels, score, *bounds, 3)
            result['backtests'][segment][arm] = run
            returns[segment, arm] = daily
        if (segment, 'B_prime') in returns:
            result['paired'][segment] = paired_statistics(returns[segment, 'B_prime'], returns[segment, 'B'])
    forward = result['paired'].get('forward', {})
    result['verdict'] = verdict(forward.get('n', 0), bool(bad_scores), forward.get('z', math.nan),
                                forward.get('half_means', [math.nan, math.nan]),
                                empty=not candidate, unchanged=unchanged)
    # Invalid scores still constitute a real S1 and carry explicit unavailable statistics.
    if candidate and not unchanged and 'forward' not in result['paired']:
        result['paired']['forward'] = dict(n=0, n_weeks=0, z=None, z_K=Z_K, mean_delta=None, se=None,
                                           half_means=[None, None], total_return_delta=None)
    if input_hashes(parent, 'crypto')[0] != hashes:
        raise ValueError('snapshot changed during the study')
    result['elapsed_seconds'] = time.monotonic() - started
    pd.DataFrame(csv_rows).to_csv(out / 'factors.csv', index=False)
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True, type=Path)
    parser.add_argument('--miner-ledger', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    group = parser.add_mutually_exclusive_group()
    group.add_argument('--ledger', type=Path)
    group.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--allow-input-drift', action='store_true')
    args = parser.parse_args(argv)
    ledger = None if args.no_ledger else args.ledger or args.out / 'ledger.jsonl'
    validate_outputs(args.root, args.out, ledger, args.miner_ledger)
    result = run_study(args.root, args.miner_ledger, args.out, args.allow_input_drift,
                       lambda message: print(message, flush=True))
    if ledger is not None and result['verdict'] not in ('no_factors', 'unchanged'):
        if not ledger.exists():
            history = read_rows(HISTORY_LEDGER)
            if next_k(history, 'fq_crypto') != 7:
                raise ValueError('historical ledger must contain exactly six counted fq_crypto S1 rows')
            ledger.parent.mkdir(parents=True, exist_ok=True)
            with ledger.open('xb') as stream:
                stream.write(HISTORY_LEDGER.read_bytes())
            result['deviations'].append('隔离账本初次创建时逐字节复制只读正式账本历史，再由 append_event 锁内核对已有 6 条计数 S1。')
        result['ledger_count_before'] = next_k(read_rows(ledger), 'fq_crypto') - 1
        result['ledger_event'] = append_ledger(ledger, result)
    result = study.json_safe(result)
    (args.out / 'report.json').write_text(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + '\n')
    (args.out / 'report.md').write_text(render_report(result), encoding='utf-8')
    print(f"判定 {result['verdict']}，B′ 因子数 {len(result['candidate'])}，z_K={Z_K:.10f}", flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
