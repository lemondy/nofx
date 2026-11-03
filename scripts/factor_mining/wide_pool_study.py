"""Run the frozen 2026-11-03 crypto wide-pool S1 study without input writes."""
from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import json
import math
from pathlib import Path
from statistics import NormalDist
import sys
import time

import numpy as np
import pandas as pd

from . import config, ensemble, evaluate, panel, quant_targets
from .expr import evaluate_expr
from scripts.backtest import evolution_ledger, quantjourney as q
from scripts.promotion.report import cluster_se

REPO = Path(__file__).resolve().parents[2]
PREREG = 'docs/research/2026-11-03-wide-pool-ensemble-prereg.md'
EXPECTED_HASHES = {
    'zoo.json': 'fffb42c16ddb013829371f9998f282b741752435b0d95f046ba667071667011a',
    'ensemble.json': 'b1b899851724f7c09ee44b055b01fa0256659692ae08ae6f57f364353001f4e1',
}
N = 40
CORR_THRESHOLD = .5
SEED = 20261103
PLACEBOS = 200
FORWARD_START = '2026-05-04'
VAL_RANGE = ('2025-05-04', '2026-04-30')
COMMISSION = .0005
SLIPPAGE = .0003
IDEA = 'wide-pool-ens-h3-v1'


def guard_inputs(root, allow_input_drift=False):
    actual = {name: hashlib.sha256((Path(root) / 'crypto' / name).read_bytes()).hexdigest()
              for name in EXPECTED_HASHES}
    drift = {name: dict(expected=EXPECTED_HASHES[name], actual=value)
             for name, value in actual.items() if value != EXPECTED_HASHES[name]}
    if drift and not allow_input_drift:
        raise ValueError(f'frozen input sha256 mismatch: {drift}; explicit --allow-input-drift required')
    return actual, drift


def panel_digest(panels):
    digest = hashlib.sha256()
    digest.update(panels.mask.index.asi8.tobytes())
    digest.update(json.dumps(list(panels.mask.columns)).encode())
    digest.update(panels.mask.to_numpy().tobytes())
    for name, frame in sorted(panels.fields.items()):
        digest.update(name.encode())
        digest.update(frame.to_numpy().tobytes())
    return digest.hexdigest()


class ValuesCache:
    """Disk-backed raw expression values; never keep the whole zoo in RAM."""

    def __init__(self, panels, factors, directory):
        self.panels = panels
        self.factors = {f['id']: f for f in factors}
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.evaluated = 0
        self.visited = set()

    def __getitem__(self, id_):
        factor = self.factors[id_]
        key = hashlib.sha256(factor['expr'].encode()).hexdigest()
        path = self.directory / (key + '.npy')
        if not path.exists():
            values = evaluate_expr(factor['expr'], self.panels.fields)
            np.save(path, values.to_numpy(), allow_pickle=False)
            self.evaluated += 1
        self.visited.add(id_)
        return pd.DataFrame(np.load(path, mmap_mode='r', allow_pickle=False),
                            index=self.panels.mask.index, columns=self.panels.mask.columns)


def train_dates(panels, zoo):
    """Check miner's shortest-horizon metadata, then use the actual h=3 train."""
    windows = zoo['split']['windows']
    assert windows == panels.windows, 'zoo/panel rolling windows disagree'
    shortest = min(int(h) for h in zoo['horizons'])
    metadata_split = panel.make_splits(panels.mask.index, shortest, windows)
    for name in ('train', 'val'):
        assert list(metadata_split['ranges'][name]) == zoo['split'][name], f'zoo {name} drift'
    split = panel.make_splits(panels.mask.index, 3, windows)
    # The only range difference must be the horizon-dependent purged tail.
    zoo_train = panels.mask.index[(panels.mask.index >= pd.Timestamp(zoo['split']['train'][0])) &
                                  (panels.mask.index <= pd.Timestamp(zoo['split']['train'][1]))]
    dates = panels.mask.index[split['train']]
    trim = 3 - shortest
    expected = zoo_train[:-trim] if trim else zoo_train
    assert dates.equals(expected), 'h=3 train differs beyond the documented purge adjustment'
    audit = dict(source='panel.make_splits(h=3)', windows=windows,
                 zoo_metadata_horizon=shortest, zoo_train=zoo['split']['train'],
                 train=list(split['ranges']['train']), metadata_agrees=True,
                 purge_adjusted_agrees=True)
    return dates, audit


def sample_train_dates(dates):
    # Miner uses every fifth visible date. Fix its phase with the registered seed.
    offset = int(np.random.default_rng(SEED).integers(5))
    return dates[offset::5], offset


def greedy_select(pool, panels, dates_sample, values_cache, progress=lambda _: None):
    if not dates_sample.isin(panels.mask.index).all():
        raise ValueError('sample dates outside panel')
    kept, ranks, audit = [], [], []
    for factor in sorted(pool, key=lambda f: (-f['train']['icir'], f['id'])):
        values = values_cache[factor['id']].loc[dates_sample] * factor['direction']
        rank = values.where(panels.mask.loc[dates_sample]).rank(axis=1, method='average')
        correlations = [abs(evaluate.cross_corr(rank, other, dates_sample)) for other in ranks]
        corr_max = max(correlations, default=0.)
        selected = corr_max < CORR_THRESHOLD
        audit.append(dict(id=factor['id'], corr_max=corr_max, selected=selected))
        if selected:
            kept.append(factor)
            ranks.append(rank)
            progress(f'greedy visited={len(audit)} selected={len(kept)}/{N} id={factor["id"]}')
        elif len(audit) % 100 == 0:
            progress(f'greedy visited={len(audit)} selected={len(kept)}/{N}')
        if len(kept) == N:
            break
    return kept, audit


def shrunk_weights(factors):
    ic = np.asarray([f['train']['ic'] for f in factors], dtype=float)
    if not len(ic):
        return np.array([], dtype=float)
    if not np.isfinite(ic).all() or (ic <= 0).any():
        raise ValueError('registered train IC weights require strictly positive finite ICs')
    return .5 / len(ic) + .5 * ic / ic.sum()


def factor_z(factor, panels, values_cache, dates=None):
    values = values_cache[factor['id']]
    mask = panels.mask
    if dates is not None:
        values, mask = values.loc[dates], mask.loc[dates]
    values = (values * factor['direction']).where(mask)
    centered = values.sub(values.mean(axis=1), axis=0)
    return centered.div(values.std(axis=1).where(lambda x: x > 1e-15), axis=0)


def weighted_scores(factors, panels, weights, values_cache):
    weights = np.asarray(weights, dtype=float)
    if len(weights) != len(factors) or not np.isfinite(weights).all() or (weights < 0).any():
        raise ValueError('one finite nonnegative weight per factor required')
    total = pd.DataFrame(0., index=panels.mask.index, columns=panels.mask.columns)
    denominator = total.copy()
    for factor, weight in zip(factors, weights):
        z = factor_z(factor, panels, values_cache)
        total += z.fillna(0.) * weight
        denominator += z.notna() * weight
    return total.div(denominator.where(denominator > 0))


def daily_returns(run):
    equity = pd.Series(run['daily_equity'], dtype=float).sort_index()
    equity.index = pd.to_datetime(equity.index, utc=True)
    return equity / equity.shift(1, fill_value=q.CAPITAL) - 1


def paired_diff(new, base):
    paired = pd.concat([new.rename('new'), base.rename('base')], axis=1).sort_index()
    paired = paired.loc[np.isfinite(paired).all(axis=1)]
    diff = paired['new'] - paired['base']
    index = pd.DatetimeIndex(diff.index)
    index = index.tz_localize('UTC') if index.tz is None else index.tz_convert('UTC')
    diff.index = index
    delta = float(diff.mean()) if len(diff) else None
    weeks = index.normalize() - pd.to_timedelta(index.weekday, unit='D')
    influences = ((diff - delta) / len(diff)).groupby(weeks).sum() if len(diff) else pd.Series(dtype=float)
    se = cluster_se(influences.tolist()) if len(influences) >= 3 else None
    z = (delta / se if se > 0 else math.copysign(math.inf, delta) if delta else 0.) if se is not None else None
    halves = (diff.iloc[:len(diff)//2], diff.iloc[len(diff)//2:])
    return dict(delta=delta, se=se, z=z, n_days=len(diff), n_weeks=len(influences),
                dev_delta=float(halves[0].mean()) if len(halves[0]) else None,
                holdout_delta=float(halves[1].mean()) if len(halves[1]) else None,
                half_ranges=[[str(x.index[0].date()), str(x.index[-1].date())] if len(x) else []
                             for x in halves],
                daily_diff={str(d.date()): float(v) for d, v in diff.items()})


def verdict(stats, k, selected_count, backtests_ok=True):
    z_k = -NormalDist().inv_cdf(.05 / (2 * max(1, k)))
    reasons = []
    if stats['n_days'] < 100:
        reasons.append('配对日少于 100')
    if not backtests_ok:
        reasons.append('任一主检验组回测失败')
    if selected_count < 20:
        reasons.append('入选因子少于 20')
    if reasons:
        result = 'invalid'
    elif (stats['dev_delta'] is not None and stats['holdout_delta'] is not None and
          stats['dev_delta'] > 0 and stats['holdout_delta'] > 0 and stats['z'] is not None):
        result = 'strong' if stats['z'] >= z_k else 'screen_pass' if stats['z'] >= 1. else 'fail'
    else:
        result = 'fail'
    return dict(verdict=result, K=k if result != 'invalid' else None, z_K=z_k,
                threshold_K=k, invalid_reasons=reasons)


def leg_contributions(run, frames, rates, dates):
    """Attribute actual fills, marked holdings, costs and carry by position side."""
    symbols = list(frames)
    opens = np.column_stack([frames[s]['open'].reindex(dates).ffill().fillna(1.) for s in symbols])
    closes = np.column_stack([frames[s]['close'].reindex(dates).ffill().fillna(1.) for s in symbols])
    by_day = {}
    for fill in run['fills']:
        by_day.setdefault(pd.Timestamp(fill['date']).date(), []).append(fill)
    lookup = {symbol: i for i, symbol in enumerate(symbols)}
    quantities = np.zeros(len(symbols))
    previous_marks = opens[0]
    parts = {side: dict(gross_pnl=0., slippage_paid=0., commission_paid=0., funding_paid=0.)
             for side in ('long', 'short')}
    daily = {side: {} for side in parts}
    max_residual = 0.
    equity = pd.Series(run['daily_equity'], dtype=float)
    previous_nav = q.CAPITAL
    for i, date in enumerate(dates):
        before = {side: sum(p['gross_pnl'] - p['slippage_paid'] - p['commission_paid'] - p['funding_paid']
                            for p in [parts[side]]) for side in parts}
        for j, quantity in enumerate(quantities):
            if quantity:
                part = parts['long' if quantity > 0 else 'short']
                part['gross_pnl'] += quantity * (opens[i, j] - previous_marks[j])
                part['funding_paid'] += quantity * opens[i, j] * rates[symbols[j]].get(date.date(), 0.) * 3
        for fill in by_day.get(date.date(), []):
            j, size = lookup[fill['symbol']], fill['size']
            old = quantities[j]
            closing = min(abs(old), abs(size)) if old * size < 0 else 0.
            for amount, side in ((closing, 'long' if old > 0 else 'short'),
                                 (abs(size) - closing, 'long' if size > 0 else 'short')):
                if amount:
                    part = parts[side]
                    fraction = amount / abs(size)
                    part['slippage_paid'] += size * (fill['price'] - opens[i, j]) * fraction
                    part['commission_paid'] += fill['commission'] * fraction
            quantities[j] += size
        for j, quantity in enumerate(quantities):
            if quantity:
                parts['long' if quantity > 0 else 'short']['gross_pnl'] += quantity * (closes[i, j] - opens[i, j])
        pnl = 0.
        for side, part in parts.items():
            net = part['gross_pnl'] - part['slippage_paid'] - part['commission_paid'] - part['funding_paid']
            change = net - before[side]
            daily[side][str(date.date())] = change
            pnl += change
        nav = float(equity.loc[str(date.date())])
        max_residual = max(max_residual, abs(pnl - (nav - previous_nav)))
        previous_nav, previous_marks = nav, closes[i]
    # Native virtual cash is 1e8; allow its rounding, not a changed P&L model.
    assert max_residual < 1e-5, f'leg attribution failed: {max_residual}'
    for side, part in parts.items():
        part['net_pnl'] = part['gross_pnl'] - part['slippage_paid'] - part['commission_paid'] - part['funding_paid']
        part['contribution_pct_initial'] = part['net_pnl'] / q.CAPITAL * 100
        part['daily_net_pnl'] = daily[side]
    return dict(**parts, max_daily_reconciliation_error=max_residual)


def run_arm(panels, score, factors, dates):
    targets, _, signals = quant_targets._historical_targets(panels, score, factors, dates)
    frames, rates, targets = quant_targets._frames_rates(panels, targets)
    run = q.run_weights(frames, targets, market='crypto', commission=COMMISSION,
                        slippage=SLIPPAGE, funding=rates, keep_fills=True,
                        policy=q.RebalancePolicy(frequency=None, rebalance_on_signal_change=True))
    run['metrics']['sharpe_log'] = run['metrics']['sharpe']
    run['metrics']['sharpe'] = run['metrics']['sharpe_simple_rf']
    run['legs'] = leg_contributions(run, frames, rates, dates)
    run['signals'] = signals
    run['ok'] = bool(not run['metrics']['bankrupt'] and np.isfinite(daily_returns(run)).all())
    return run


def rank_ic(score, excess, mask):
    return evaluate.cs_rank_ic(score, excess, mask, config.MARKETS['crypto']['min_names']).dropna()


def ic_summary(ic):
    return dict(mean=float(ic.mean()) if len(ic) else None, nw_t=evaluate.nw_t(ic, 3),
                n_days=len(ic), daily={str(d.date()): float(v) for d, v in ic.items()})


def placebo_study(pool, panels, dates, excess, values_cache, main_ic, progress):
    # Accumulate every draw in one pass over unique factors. Raw values are
    # evaluated at most once, and no 3650-factor in-memory tensor is needed.
    ordered = sorted(pool, key=lambda f: f['id'])
    rng = np.random.default_rng(SEED)
    draws = [rng.choice(len(ordered), N, replace=False).tolist() for _ in range(PLACEBOS)]
    memberships = {}
    for draw, members in enumerate(draws):
        for member in members:
            memberships.setdefault(member, []).append(draw)
    totals = np.zeros((PLACEBOS, len(dates), panels.mask.shape[1]))
    counts = np.zeros(totals.shape, dtype=np.int16)
    for j, (member, draw_ids) in enumerate(sorted(memberships.items())):
        z = factor_z(ordered[member], panels, values_cache, dates).to_numpy()
        valid = np.isfinite(z)
        for draw in draw_ids:
            totals[draw] += np.where(valid, z, 0.)
            counts[draw] += valid
        if (j + 1) % 100 == 0:
            progress(f'placebo factors={j + 1}/{len(memberships)}')
    means, n_days = [], []
    for draw in range(PLACEBOS):
        score = np.divide(totals[draw], counts[draw], out=np.full(totals[draw].shape, np.nan),
                          where=counts[draw] > 0)
        ic = rank_ic(pd.DataFrame(score, index=dates, columns=panels.mask.columns),
                     excess.loc[dates], panels.mask.loc[dates])
        means.append(float(ic.mean()) if len(ic) else None)
        n_days.append(len(ic))
    finite = [v for v in means if v is not None and np.isfinite(v)]
    percentile = (100 * sum(v <= main_ic for v in finite) / len(finite)
                  if finite and main_ic is not None else None)
    return dict(draws=PLACEBOS, factors_per_draw=N, seed=SEED, percentile=percentile,
                percentile_definition='100 × count(placebo RankIC mean <= main RankIC mean) / valid draws',
                rank_ic_means=means, n_days=n_days, valid_draws=len(finite),
                factor_ids=[[ordered[i]['id'] for i in draw] for draw in draws])


def ledger_result(stats, selected_count, backtests_ok, hashes, turnover_ratio, placebo_pct,
                  ledger_path, input_drift, allow_input_drift):
    def build(k, _previous):
        return dict(stats, **verdict(stats, k, selected_count, backtests_ok), track='fq_crypto',
                    change_type='E', stage='S1', idea_id=IDEA,
                    baseline_version=hashes['ensemble.json'][:16] + ':ls_invvol_protected_v1',
                    trade_ratio=turnover_ratio, placebo_pct=placebo_pct, input_hashes=hashes,
                    prereg_path=PREREG, allow_input_drift=allow_input_drift, input_drift=input_drift)
    if ledger_path is not None:
        return evolution_ledger.append_event(ledger_path, 'fq_crypto', build)
    reference_path = REPO / evolution_ledger.DEFAULT_PATH
    return build(evolution_ledger.next_k(evolution_ledger.read_rows(reference_path), 'fq_crypto'), [])


def run_study(root, out, ledger_path=None, allow_input_drift=False, progress=print):
    started = time.monotonic()
    hashes, drift = guard_inputs(root, allow_input_drift)
    baseline_factors, frozen = quant_targets._load_frozen(root)
    assert all(frozen[name]['sha256'] == value for name, value in hashes.items()), 'inputs changed while loading'
    zoo = json.loads((Path(root) / 'crypto/zoo.json').read_text())
    pool = [f for f in zoo['factors'] if f['horizon'] == 3 and
            f['status'] in ('accepted', 'candidate', 'redundant')]
    assert len({f['id'] for f in pool}) == len(pool), 'duplicate h=3 pool ids'
    if not drift:
        assert len(pool) == 3650 and len(baseline_factors) == 14, 'frozen pool size mismatch'
    panels = panel.load_panels('crypto', root)
    digest = panel_digest(panels)
    dates, split_audit = train_dates(panels, zoo)
    sample, offset = sample_train_dates(dates)
    deviations = [
        'zoo.split 为 miner 保存的最短周期 h=1 汇总；原始 TRAIN 尾日 2025-05-02 与 h=3 不一致。'
        f'采用相同冻结窗口的 make_splits(h=3)，TRAIN 为 {split_audit["train"][0]}～{split_audit["train"][1]}；'
        '已断言 h=1 元数据完全一致，且 h=3 仅额外清除两日尾部。',
        f'预注册未给相关性子样本数量/抽法细节：沿用 miner 每 5 日步长，以种子 {SEED} 固定起始相位 {offset}；'
        '先限制 TRAIN，再构造有方向、核心 mask 内的 average 秩帧，取逐日相关均值的绝对值。',
        '辅助项未规定腿贡献分摊：按原生成交和实际持仓拆分多空毛损益、滑点、手续费及资金费，'
        '贡献以初始资金百分比报告，并核对两腿之和与净值变动。',
        '安慰剂池按 id 固定顺序，使用 numpy.default_rng(20261103) 无放回抽取每组 40 个；'
        '分位定义为 200 组 RankIC 均值中小于等于主变体均值的比例（百分数）。',
    ]
    if drift:
        deviations.append('显式允许冻结输入漂移；详见 input_drift，结果不再是原冻结输入研究。')
    cache = ValuesCache(panels, pool + baseline_factors, Path(out) / '.cache' / digest)
    progress(f'panel={panels.mask.shape}; pool={len(pool)}; TRAIN={split_audit["train"]}; sample={len(sample)}')
    selected, selection_audit = greedy_select(pool, panels, sample, cache, progress)
    if len(selected) < N:
        deviations.append(f'遍历全池仍只入选 {len(selected)}/{N}，不放宽阈值；权重按实际入选数归一化，'
                          '少于 20 个时按预注册判 invalid。')
    weights = shrunk_weights(selected)
    scores = dict(baseline=ensemble.ensemble_scores(baseline_factors, panels, values_cache=cache),
                  weighted_40=weighted_scores(selected, panels, weights, cache),
                  equal_40=ensemble.ensemble_scores(selected, panels, values_cache=cache))
    factors = dict(baseline=baseline_factors, weighted_40=selected, equal_40=selected)
    intervals, returns = {}, {}
    for name, start, stop in (('forward', FORWARD_START, panels.mask.index[-1]), ('val', *VAL_RANGE)):
        execution_dates = panels.mask.index[(panels.mask.index >= pd.Timestamp(start)) &
                                            (panels.mask.index <= pd.Timestamp(stop))]
        runs, returns[name] = {}, {}
        for arm, score in scores.items():
            progress(f'backtest {name}/{arm} days={len(execution_dates)}')
            try:
                run = run_arm(panels, score, factors[arm], execution_dates)
            except (ValueError, RuntimeError) as error:
                run = dict(ok=False, error=f'{type(error).__name__}: {error}')
            runs[arm] = run
            returns[name][arm] = daily_returns(run) if 'daily_equity' in run else pd.Series(dtype=float)
        intervals[name] = dict(start=str(execution_dates[0].date()), end=str(execution_dates[-1].date()),
                               selection_bias=name == 'val', arms=runs,
                               paired=paired_diff(returns[name]['weighted_40'], returns[name]['baseline']),
                               equal_weight_paired=paired_diff(returns[name]['equal_40'], returns[name]['baseline']))
    progress('independent quant_targets.backtest baseline reproduction')
    reference = quant_targets.backtest(root)
    assert panel_digest(panel.load_panels('crypto', root)) == digest, 'panel changed during backtests'
    reproduction = {}
    for name in ('forward', 'val'):
        expected = reference['intervals'][name]['strategies']['ls_invvol_protected_v1']['daily_equity']
        actual = intervals[name]['arms']['baseline'].get('daily_equity', {})
        if actual:
            expected_final = list(expected.values())[-1]
            actual_final = list(actual.values())[-1]
            error = abs(actual_final - expected_final) / abs(expected_final)
            assert error <= 1e-9, f'{name} baseline reproduction failed: {error}'
            reproduction[name] = dict(expected_final_equity=expected_final, actual_final_equity=actual_final,
                                      relative_error=error, tolerance=1e-9, matched=True)
        else:
            reproduction[name] = dict(matched=False, reason='基线回测失败')
    forward_dates = panels.mask.index[panels.mask.index >= pd.Timestamp(FORWARD_START)]
    excess, _ = panel.forward_labels(panels, 3, FORWARD_START)
    ics = {arm: rank_ic(score.loc[forward_dates], excess.loc[forward_dates], panels.mask.loc[forward_dates])
           for arm, score in scores.items()}
    paired_ic = pd.concat([ics['weighted_40'].rename('new'), ics['baseline'].rename('base')], axis=1).dropna()
    ic_difference = ic_summary(paired_ic['new'] - paired_ic['base'])
    placebo = placebo_study(pool, panels, forward_dates, excess, cache, ics['weighted_40'].mean(), progress)
    arms = intervals['forward']['arms']
    base_turnover = arms['baseline'].get('metrics', {}).get('turnover')
    new_turnover = arms['weighted_40'].get('metrics', {}).get('turnover')
    ratio = new_turnover / base_turnover if base_turnover and new_turnover is not None else None
    ok = all(arms[arm]['ok'] for arm in ('baseline', 'weighted_40'))
    assert guard_inputs(root, allow_input_drift)[0] == hashes, 'inputs changed during study'
    stats = intervals['forward']['paired']
    row = ledger_result(stats, len(selected), ok, hashes, ratio,
                        None if placebo['percentile'] is None else placebo['percentile'] / 100, ledger_path,
                        drift, allow_input_drift)
    result = dict(idea_id=IDEA, prereg_path=PREREG,
                  prereg_sha256=hashlib.sha256((REPO / PREREG).read_bytes()).hexdigest(),
                  input_hashes=hashes, expected_hashes=EXPECTED_HASHES, input_drift=drift,
                  allow_input_drift=allow_input_drift, frozen=frozen, panel=panels.meta, panel_sha256=digest,
                  parameters=dict(N=N, correlation_threshold=CORR_THRESHOLD, shrinkage=.5, seed=SEED,
                                  horizon=3, commission=COMMISSION, slippage=SLIPPAGE,
                                  forward_start=FORWARD_START, val_range=VAL_RANGE, placebo_draws=PLACEBOS),
                  split_audit=split_audit, correlation_sample=dict(offset=offset, stride=5,
                      dates=[str(d.date()) for d in sample]),
                  pool_size=len(pool), selected_factors=[dict(f, weight=float(w)) for f, w in zip(selected, weights)],
                  family_counts=dict(Counter(f['family'] for f in selected)), selection_audit=selection_audit,
                  intervals=intervals, baseline_reproduction=reproduction,
                  rank_ic={arm: ic_summary(ic) for arm, ic in ics.items()}, paired_rank_ic=ic_difference,
                  turnover_ratio=ratio, placebo=placebo,
                  primary=dict(stats, **{key: row[key] for key in ('verdict', 'K', 'z_K', 'threshold_K', 'invalid_reasons')}),
                  ledger=dict(path=str(ledger_path) if ledger_path is not None else None,
                              written=ledger_path is not None, row=row), deviations=deviations,
                  cache=dict(evaluated=cache.evaluated, visited=len(cache.visited), path=str(cache.directory)),
                  elapsed_seconds=time.monotonic() - started)
    return result


def number(value, digits=6):
    return 'NA' if value is None else f'{value:.{digits}f}'


def json_safe(value):
    if isinstance(value, np.generic):
        value = value.item()
    if isinstance(value, dict):
        return {key: json_safe(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(item) for item in value]
    return evolution_ledger.json_safe(value)


def render_report(result):
    p = result['primary']
    lines = ['# 加密宽因子池组合：预注册研究结果', '',
             f'结论：**{p["verdict"]}**；Δ={number(p["delta"])} / 日，SE={number(p["se"])}，'
             f'z={number(p["z"], 4)}，K={p["K"]}，z_K={number(p["z_K"], 4)}。', '',
             '**前向区间已被先前研究使用，动机受其结果影响；成对差仍受数据污染限制，只作 S1 筛选。**', '',
             f'预注册：`{PREREG}`；运行耗时：{result["elapsed_seconds"]:.3f} 秒。', '',
             '## 主检验', '',
             '| 指标 | 数值 |', '| --- | ---: |',
             f'| 配对日 / UTC 周一周数 | {p["n_days"]} / {p["n_weeks"]} |',
             f'| 每日收益差 Δ | {number(p["delta"])} |', f'| 按周聚类 SE | {number(p["se"])} |',
             f'| z / z_K | {number(p["z"], 4)} / {number(p["z_K"], 4)} |',
             f'| 前半 Δ | {number(p["dev_delta"])} |', f'| 后半 Δ | {number(p["holdout_delta"])} |',
             f'| K / 判定 | {p["K"]} / {p["verdict"]} |',
             f'| 主变体 / 基线换手率比 | {number(result["turnover_ratio"], 4)} |', '',
             f'前后半范围：{p["half_ranges"]}。均值差单位为日简单收益；首日收益以初始资金 1000 计算。',
             'SE 使用每周 Σ(d_t−Δ)/n 的 influence，调用 promotion.report.cluster_se，含 G/(G−1) 修正；少于 3 周不给 SE。',
             f'账本：{result["ledger"]["path"] or "未写入（K 参考工作树默认账本的下一次尝试）"}。', '',
             '## 各组表现', '',
             '| 区间 | 组 | 总收益 % | 年化 % | Sharpe | 最大回撤 % | 累计双向换手 | 手续费 | 资金费净付出 |',
             '| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
    for interval_name, interval in result['intervals'].items():
        for arm, run in interval['arms'].items():
            m = run.get('metrics', {})
            values = [interval_name, arm] + [number(m.get(k), 4) for k in
                ('return_pct', 'cagr_pct', 'sharpe', 'maxdd_pct', 'turnover', 'commission_paid', 'funding_paid')]
            lines.append('| ' + ' | '.join(values) + ' |')
            if run.get('error'):
                lines.append(f'回测失败：{run["error"]}')
    val = result['intervals']['val']['paired']
    eq = result['intervals']['forward']['equal_weight_paired']
    lines += ['', '## 验证段与等权对照', '',
              f'验证段 {VAL_RANGE[0]}～{VAL_RANGE[1]}：Δ={number(val["delta"])}，SE={number(val["se"])}，'
              f'z={number(val["z"], 4)}，配对日={val["n_days"]}；前/后半 Δ='
              f'{number(val["dev_delta"])} / {number(val["holdout_delta"])}。',
              '基线成分经过验证段筛选，此处对基线有利；不影响主判定。', '',
              f'入选因子等权对照（不入账）：前向 Δ={number(eq["delta"])}，SE={number(eq["se"])}，'
              f'z={number(eq["z"], 4)}；前/后半 Δ={number(eq["dev_delta"])} / {number(eq["holdout_delta"])}。', '',
              '## 前向 RankIC 与安慰剂', '',
              '| 组 | RankIC 均值 | NW t（lag=3） | 有效日 |', '| --- | ---: | ---: | ---: |']
    for arm, ic in dict(result['rank_ic'], paired_difference=result['paired_rank_ic']).items():
        lines.append(f'| {arm} | {number(ic["mean"])} | {number(ic["nw_t"], 4)} | {ic["n_days"]} |')
    placebo = result['placebo']
    lines += ['', f'200 次不去相关、每次 40 因子等权安慰剂：主变体位于 **{number(placebo["percentile"], 2)}%** 分位；'
              f'有效组 {placebo["valid_draws"]}/200，种子 {SEED}。仅算前向 RankIC，不回测、不计 K。',
              '标签使用 open[t+1+h]/open[t+1]−1 的横截面超额收益；最后 h+1 日无完整标签，不计 RankIC。', '',
              '## 多空腿贡献', '',
              '| 区间 | 组 | 腿 | 毛损益 | 滑点 | 手续费 | 资金费净付出 | 净损益 | 初始资金贡献 % |',
              '| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |']
    for name, interval in result['intervals'].items():
        for arm, run in interval['arms'].items():
            for side in ('long', 'short'):
                leg = run.get('legs', {}).get(side, {})
                values = [name, arm, side] + [number(leg.get(k), 4) for k in
                    ('gross_pnl', 'slippage_paid', 'commission_paid', 'funding_paid', 'net_pnl', 'contribution_pct_initial')]
                lines.append('| ' + ' | '.join(values) + ' |')
    lines += ['', '## 入选因子与族分布', '',
              f'池子 {result["pool_size"]}；考察 {len(result["selection_audit"])}；入选 {len(result["selected_factors"])}/{N}。',
              f'族分布：{json.dumps(result["family_counts"], ensure_ascii=False)}。', '',
              '| id | 表达式 | 方向 | 族 | 训练 IC | 训练 ICIR | 权重 |',
              '| --- | --- | ---: | --- | ---: | ---: | ---: |']
    for f in result['selected_factors']:
        lines.append(f'| {f["id"]} | `{f["expr"]}` | {f["direction"]} | {f["family"]} | '
                     f'{number(f["train"]["ic"])} | {number(f["train"]["icir"])} | {number(f["weight"], 8)} |')
    lines += ['', '## 输入与复现核对', '']
    for name, digest in result['input_hashes'].items():
        lines.append(f'- {name}：`{digest}`。')
    lines += [f'- allow_input_drift={result["allow_input_drift"]}；实际漂移：{result["input_drift"]}。',
              f'- TRAIN 核对：{json.dumps(result["split_audit"], ensure_ascii=False)}。',
              f'- 基线复现：{json.dumps(result["baseline_reproduction"], ensure_ascii=False)}。',
              '- 三组均调用 quant_targets._historical_targets → _frames_rates → quantjourney.run_weights；'
              '每 3 日信号、次日开盘执行、核心流动性 mask、相同保护规则，手续费 0.0005、滑点 0.0003、真实资金费。',
              '- 年化、简单日收益 Sharpe、最大回撤及双向成交换手沿用 quant_targets；各区间从空仓开始，末日不伪造平仓。', '',
              '## deviations（偏离与歧义解释）', '']
    lines += ['- ' + item for item in result['deviations']]
    lines += ['', '通过筛选仅表示可进入预登记线上配对；本研究不创建交易员或修改正式配置。', '']
    return '\n'.join(lines)


def validate_outputs(root, out, ledger_path):
    root, out = Path(root).resolve(), Path(out).resolve()
    paths = [out / 'report.json', out / 'report.md']
    if ledger_path is not None:
        paths.append(Path(ledger_path).resolve())
    if len(set(paths)) != len(paths):
        raise ValueError('ledger and report paths must differ')
    for path in paths + [out]:
        if path == root or root in path.parents:
            raise ValueError(f'output must not be inside read-only data root: {path}')
        if path != REPO and REPO not in path.parents:
            raise ValueError(f'output must be inside this worktree: {path}')
    return out


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    ledger = parser.add_mutually_exclusive_group()
    ledger.add_argument('--ledger', type=Path, default=REPO / evolution_ledger.DEFAULT_PATH)
    ledger.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--allow-input-drift', action='store_true')
    args = parser.parse_args(argv)
    ledger_path = None if args.no_ledger else args.ledger.resolve()
    try:
        out = validate_outputs(args.root, args.out, ledger_path)
        guard_inputs(args.root, args.allow_input_drift)
    except ValueError as error:
        parser.error(str(error))
    result = run_study(args.root, out, ledger_path, args.allow_input_drift,
                       progress=lambda s: print(s, file=sys.stderr, flush=True))
    out.mkdir(parents=True, exist_ok=True)
    (out / 'report.json').write_text(json.dumps(json_safe(result), ensure_ascii=False,
                                               indent=2, allow_nan=False) + '\n', encoding='utf-8')
    (out / 'report.md').write_text(render_report(result), encoding='utf-8')
    print(f'verdict={result["primary"]["verdict"]}; K={result["primary"]["K"]}; '
          f'z={number(result["primary"]["z"], 4)}; wall_seconds={result["elapsed_seconds"]:.3f}')
    print(f'reports: {out / "report.md"}, {out / "report.json"}')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
