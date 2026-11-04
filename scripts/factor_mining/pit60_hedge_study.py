"""Frozen PIT60/BTC-hedge S1 screen; offline inputs, no live configuration writes."""
from __future__ import annotations

import argparse
from collections import Counter
from concurrent.futures import ProcessPoolExecutor
from contextlib import contextmanager
import datetime as dt
import hashlib
import json
import multiprocessing
import os
from pathlib import Path
import sys
import tempfile
import time

import numpy as np
import pandas as pd

from . import config, ensemble, evaluate, options, panel, quant_targets, universe_expansion_study
from .expr import ExprError, evaluate_expr
from .wide_pool_study import (daily_returns, ic_summary, json_safe, leg_contributions,
                              number, paired_diff, panel_digest, rank_ic, verdict as wide_verdict)
from scripts.backtest import evolution_ledger, quantjourney as q

REPO = Path(__file__).resolve().parents[2]
PREREG = 'docs/research/2026-11-04-pit60-btchedge-prereg.md'
DATA = Path('/Users/zhangyun/workspace/nofx/data')
BASE_DIR = DATA / 'factor_mining/crypto'
FULL_DIR = DATA / 'factor_mining/crypto_full'
SNAPSHOT_DIR = FULL_DIR / 'prereg_20261104'
DEFAULT_LEDGER = DATA / 'evolution/ledger.jsonl'
EXPECTED_HASHES = {
    'zoo.json': 'aef7c307efb23c01272131497c0fe012b119be42ec76d919c266cee74dee6e3e',
    'ensemble.json': 'f53fb2a5c15f5cf246b251350cc365bf5b862b756c8b952fd220c31af1870eb0',
}
END_BT = '2026-10-31'
FORWARD_START = '2026-05-07'
VAL_RANGE = ('2025-05-07', '2026-05-01')
K_SEL = 16502
IDEA = 'pit60-btchedge-h3-v1'
COMMISSION, SLIPPAGE = .0005, .0003
DEFAULT_WORKERS = min(8, os.cpu_count() or 1)
STATUSES = ('accepted', 'candidate', 'rejected', 'redundant', 'invalid_expr', 'no_coverage')
_EVAL_STATE = None


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def guard_inputs(snapshot_dir, allow_input_drift=False):
    actual = {name: sha256(Path(snapshot_dir) / name) for name in EXPECTED_HASHES}
    drift = {name: dict(expected=EXPECTED_HASHES[name], actual=value)
             for name, value in actual.items() if value != EXPECTED_HASHES[name]}
    if drift and not allow_input_drift:
        raise ValueError(f'frozen input sha256 mismatch: {drift}; --allow-input-drift required')
    return actual, drift


@contextmanager
def snapshot_root(base_dir, snapshot_dir, *, truncated=False):
    """Symlink frozen inputs; S0 alone gets independent, date-truncated JSON copies."""
    base, snapshot = Path(base_dir).resolve(), Path(snapshot_dir).resolve()
    with tempfile.TemporaryDirectory(prefix='pit60-') as tmp:
        crypto = Path(tmp) / 'crypto'
        crypto.mkdir()
        for name in ('data_meta.json', 'zoo.json', 'ensemble.json'):
            source = (base if name == 'data_meta.json' else snapshot) / name
            (crypto / name).symlink_to(source)
        for name in ('daily', 'alt_daily'):
            source = base / name
            if not truncated:
                (crypto / name).symlink_to(source, target_is_directory=True)
                continue
            (crypto / name).mkdir()
            pattern = '*.json' if name == 'daily' else '*/*.json'
            for path in sorted(source.glob(pattern)):
                target = crypto / name / path.relative_to(source)
                target.parent.mkdir(parents=True, exist_ok=True)
                rows = json.loads(path.read_text(encoding='utf-8'))
                rows = [r for r in rows if r[0] <= END_BT]
                target.write_text(json.dumps(rows, allow_nan=False), encoding='utf-8')
        yield Path(tmp)


def truncate_panels(panels):
    """Keep selection objects intact; all execution fields/masks end at END_BT."""
    keep = panels.mask.index <= pd.Timestamp(END_BT)
    fields = {k: v.loc[keep].copy() for k, v in panels.fields.items()}
    mask = panels.mask.loc[keep].copy()
    meta = dict(panels.meta or {}, end=str(mask.index[-1].date()),
                bars=int(fields['close'].notna().to_numpy().sum()))
    return panel.Panels(fields, mask,
                        None if panels.benchmark_close is None else panels.benchmark_close.loc[keep].copy(),
                        None if panels.benchmark_open is None else panels.benchmark_open.loc[keep].copy(),
                        meta, panels.windows)


def assert_splits(panels, zoo):
    assert panels.windows == zoo['split']['windows'], 'zoo/panel windows disagree'
    splits = panel.make_splits(panels.mask.index, 3, panels.windows)
    for name, expected in (('train', ('2022-05-08', '2025-05-01')), ('val', VAL_RANGE)):
        assert tuple(splits['ranges'][name]) == expected, f'h=3 {name} range drift: {splits["ranges"]}'
    assert splits['ranges']['holdout'][0] == FORWARD_START, 'h=3 holdout start drift'
    return splits


def _evaluate_candidate(factor):
    # Only a small factor dict crosses the process boundary. Panels and sealed
    # train/val targets are inherited via fork, never pickled once per task.
    panels, excess, raw, splits, cfg = _EVAL_STATE
    row = {k: factor[k] for k in ('id', 'expr', 'family', 'first_seen', 'run_id')}
    row['horizon'] = 3
    try:
        values = evaluate_expr(row['expr'], panels.fields)
    except ExprError as error:
        return dict(row, status='invalid_expr', error=str(error))
    metrics = evaluate.evaluate_factor(values, excess, raw, panels.mask, splits, 3, cfg)
    if metrics is None:
        return dict(row, status='no_coverage')
    row.update(metrics)
    row['status'] = evaluate.classify(row['train'], row['val'], K_SEL, evaluate.t_required(K_SEL))
    return row


def remove_redundancy(rows, panels, splits):
    """Miner's signed rank correlation/order, restricted to accepted factors."""
    dates = panels.mask.index[splits['train'] | splits['val']][::5]
    kept = []
    ordered = sorted((f for f in rows if f['status'] == 'accepted'),
                     key=lambda f: (-f['train']['t'], f['first_seen'], f['run_id'], f['id']))
    for factor in ordered:
        values = evaluate_expr(factor['expr'], panels.fields)
        rank = (values.loc[dates] * factor['direction']).where(panels.mask.loc[dates]).rank(
            axis=1, method='average')
        del values
        factor['corr_max'], factor['corr_with'] = 0., None
        for other, other_rank in kept:
            corr = evaluate.cross_corr(rank, other_rank, dates)
            if corr > factor['corr_max']:
                factor['corr_max'], factor['corr_with'] = corr, other['id']
        if factor['corr_max'] >= config.CORR_REDUNDANT:
            factor['status'] = 'redundant'
        else:
            kept.append((factor, rank))
    return rows


def reaccept(panels, pool, *, workers=DEFAULT_WORKERS, progress=lambda _: None):
    global _EVAL_STATE
    if workers < 1:
        raise ValueError('workers must be positive')
    assert not options.enabled(options.GREEDY_ENSEMBLE), 'FM_GREEDY_ENSEMBLE must be off'
    splits = panel.make_splits(panels.mask.index, 3, panels.windows)
    excess, raw = panel.forward_target(panels, 3)
    cfg = dict(config.MARKETS['crypto'])
    cfg.pop('direction', None)
    previous = _EVAL_STATE
    _EVAL_STATE = panels, excess, raw, splits, cfg
    rows = []

    def collect(results):
        for i, row in enumerate(results, 1):
            rows.append(row)
            if i % 200 == 0:
                progress(f're-acceptance {i}/{len(pool)} expressions')

    try:
        if workers == 1:
            collect(map(_evaluate_candidate, pool))
        else:
            with ProcessPoolExecutor(max_workers=workers, mp_context=multiprocessing.get_context('fork')) as executor:
                collect(executor.map(_evaluate_candidate, pool, chunksize=1))
    finally:
        _EVAL_STATE = previous
    remove_redundancy(rows, panels, splits)
    selected = ensemble.select_ensemble_factors(rows, 3)
    return rows, selected


def long_sleeve(panels, snapshot, *, now=None):
    """Exact rounded positive sleeve of build_targets, independent of short gates."""
    h, entry = quant_targets._scores_entry('crypto', snapshot)
    result = dict(as_of=snapshot.get('as_of'), status='no_validated_factors', reason='no_core_scores',
                  n_eligible=0, long_weights={})
    if not entry:
        return result
    as_of = pd.Timestamp(snapshot.get('as_of'))
    if pd.isna(as_of):
        return dict(result, status='stale_scores', reason='missing_as_of')
    as_of = quant_targets._utc(as_of).normalize()
    if (quant_targets._utc(now).normalize() - as_of).days > h + 2:
        return dict(result, status='stale_scores', reason='as_of_older_than_horizon_plus_2_days')
    date = as_of.tz_localize(None)
    if panels is None or date not in panels.mask.index or date not in panels.fields['close'].index:
        return dict(result, status='insufficient_universe', reason='as_of_missing_from_panels')
    rule = quant_targets._rule('crypto')
    close = panels.fields['close'].loc[:date].reindex(columns=panels.mask.columns)
    close = close.where(np.isfinite(close) & close.gt(0))
    returns = close.tail(rule['vol_window'] + 1).pct_change(fill_method=None)
    vol = returns.rolling(rule['vol_window'], min_periods=rule['vol_window']).std(ddof=1).iloc[-1]
    scores = pd.Series({s: v.get('z', np.nan) for s, v in entry['scores'].items()}, dtype=float)
    scores = scores.reindex(close.columns)
    eligible = panels.mask.loc[date].fillna(False) & np.isfinite(scores) & np.isfinite(vol) & vol.gt(0)
    scores = scores[eligible]
    result['n_eligible'] = len(scores)
    if len(scores) < 15:
        return dict(result, status='insufficient_universe', reason='fewer_than_15_eligible_symbols')
    ordered = scores.sort_values(kind='stable')
    count = max(1, int(np.floor(len(ordered) * rule['quantile'])))
    longs = ordered.index[-count:]
    weights = quant_targets._capped_invvol(vol.loc[longs], rule['long_gross'], rule['max_name_weight'])
    result.update(status='ok', reason=None, long_weights={
        str(s): round(float(w), 6) for s, w in weights.items() if round(float(w), 6)})
    return result


def beta_ols(symbol_returns, btc_returns):
    """OLS with intercept, pairwise finite observations; degenerate BTC -> beta 1."""
    paired = pd.concat([symbol_returns.rename('y'), btc_returns.rename('x')], axis=1)
    paired = paired.loc[np.isfinite(paired).all(axis=1)]
    if len(paired) < 20:
        return 1.
    x, y = (paired[c].to_numpy() for c in ('x', 'y'))
    x, y = x - x.mean(), y - y.mean()
    variance = float(x @ x)
    return float(np.clip((x @ y) / variance, 0., 3.)) if variance > 0 else 1.


def hedge_targets(panels, snapshot, *, now=None, beta_fn=None):
    result = dict(long_sleeve(panels, snapshot, now=now), weights={}, h=None, mean_beta=None)
    if result['status'] != 'ok':
        return result
    date = quant_targets._utc(snapshot['as_of']).normalize().tz_localize(None)
    close = panels.fields['close'].loc[:date]
    btc = close.loc[date].get('BTCUSDT', np.nan)
    if not np.isfinite(btc) or btc <= 0:
        return dict(result, status='insufficient_universe', reason='missing_btc_close_at_as_of')
    close = close.where(np.isfinite(close) & close.gt(0))
    returns = close.tail(31).pct_change(fill_method=None).tail(30)
    weights = result['long_weights']
    beta_fn = beta_fn or beta_ols
    betas = {s: float(np.clip(beta_fn(returns[s], returns['BTCUSDT']), 0., 3.)) for s in weights}
    beta_gross = sum(w * betas[s] for s, w in weights.items())
    h = min(.5, beta_gross)
    net = dict(weights)
    net['BTCUSDT'] = net.get('BTCUSDT', 0.) - h
    result.update(weights={s: w for s, w in net.items() if w}, h=h, betas=betas,
                  mean_beta=beta_gross / sum(weights.values()) if weights else None)
    return result


def hedge_historical_targets(panels, score, factors, dates, *, beta_fn=None):
    targets = pd.DataFrame(np.nan, index=dates, columns=score.columns)
    signals = []
    for date in dates[::3]:
        row = score.loc[date].where(panels.mask.loc[date])
        row = row[np.isfinite(row)]
        pct = row.rank(pct=True)
        entry = dict(factor_ids=[f['id'] for f in factors], scores={
            str(s): dict(z=round(float(z), 4), pct=round(float(pct[s]), 4)) for s, z in row.items()})
        snapshot = dict(as_of=date.strftime('%Y-%m-%d'), run_id='frozen-h3-study', horizons={'3': entry})
        target = hedge_targets(panels, snapshot, now=date + dt.timedelta(days=1), beta_fn=beta_fn)
        if target['status'] == 'ok':
            targets.loc[date] = pd.Series(target['weights'], dtype=float).reindex(score.columns, fill_value=0.)
        signals.append(target)
    return targets.ffill().fillna(0.), signals


def short_tail(panels, targets, dates):
    observations, losses, tail_losses = [], 0., 0.
    opens = panels.fields['open']
    for date in dates[::3]:
        position = opens.index.get_loc(date)
        if position + 4 >= len(opens):
            continue
        for symbol, weight in targets.loc[date].items():
            if weight >= 0:
                continue
            entry, exit_ = opens[symbol].iloc[[position + 1, position + 4]]
            if not np.isfinite([entry, exit_]).all() or entry <= 0 or exit_ <= 0:
                continue
            ret = float(exit_ / entry - 1)
            loss = -weight * max(ret, 0.)
            observations.append(ret)
            losses += loss
            tail_losses += loss if ret > .3 else 0.
    return dict(n=len(observations), share_gt_30=float(np.mean(np.asarray(observations) > .3))
                if observations else None, loss_share_from_gt_30=tail_losses / losses if losses else None)


def run_arm(panels, score, factors, dates, rule='protected'):
    """A failed arm is reportable data, including failures in auxiliary arms."""
    try:
        if len(dates) < 2:
            raise ValueError('not enough execution dates')
        if rule == 'hedge':
            targets, signals = hedge_historical_targets(panels, score, factors, dates)
        elif rule == 'protected':
            targets, _, signals = quant_targets._historical_targets(panels, score, factors, dates)
        else:
            raise ValueError(f'unknown rule: {rule}')
        tail = short_tail(panels, targets, dates) if rule == 'protected' else None
        frames, rates, targets = quant_targets._frames_rates(panels, targets)
        run = q.run_weights(frames, targets, market='crypto', commission=COMMISSION, slippage=SLIPPAGE,
                            funding=rates, keep_fills=True,
                            policy=q.RebalancePolicy(frequency=None, rebalance_on_signal_change=True))
        metrics = run['metrics']
        metrics['sharpe_log'], metrics['sharpe'] = metrics['sharpe'], metrics['sharpe_simple_rf']
        run['legs'] = leg_contributions(run, frames, rates, dates)
        run['signals'] = signals
        if rule == 'hedge':
            ok_signals = [s for s in signals if s['status'] == 'ok']
            run['hedge_summary'] = {key: float(np.mean([s[key] for s in ok_signals]))
                                    if ok_signals else None for key in ('h', 'mean_beta')}
        else:
            run['short_tail'] = tail
        run['ok'] = bool(not metrics['bankrupt'] and np.isfinite(daily_returns(run)).all())
        if not run['ok']:
            run['error'] = 'bankrupt or non-finite daily returns'
        return run
    except Exception as error:
        return dict(ok=False, error=f'{type(error).__name__}: {error}')


def paired_statistics(new, base):
    stats = paired_diff(new, base)
    # Reuse the frozen weekly-cluster statistic, replacing only the halves rule.
    diff = pd.Series(stats['daily_diff'], dtype=float)
    middle = (len(diff) + 1) // 2
    halves = (diff.iloc[:middle], diff.iloc[middle:])
    stats.update(dev_delta=float(halves[0].mean()) if len(halves[0]) else None,
                 holdout_delta=float(halves[1].mean()) if len(halves[1]) else None,
                 half_ranges=[[x.index[0], x.index[-1]] if len(x) else [] for x in halves])
    return stats


def verdict(stats, k, accepted_count, backtests_ok=True, s0_ok=True):
    # Reuse sister-study tier/boundary logic; its minimum 20 is replaced by 5.
    result = wide_verdict(stats, k, 20, backtests_ok)
    reasons = result['invalid_reasons']
    if not s0_ok:
        reasons.append('S0 reproduction failed')
    if accepted_count < 5:
        reasons.append('accepted factors fewer than 5')
    if reasons:
        result.update(verdict='invalid', K=None)
    return result


def reproduce_baseline(root, panels, factors):
    try:
        reference = quant_targets.backtest(root)
        interval = reference['intervals']['forward']
        dates = panels.mask.index[(panels.mask.index >= pd.Timestamp(interval['start'])) &
                                  (panels.mask.index <= pd.Timestamp(interval['end']))]
        actual = run_arm(panels, ensemble.ensemble_scores(factors, panels), factors, dates)
        expected_run = interval['strategies']['ls_invvol_protected_v1']
        expected = pd.Series(expected_run['daily_equity'], dtype=float).sort_index()
        observed = pd.Series(actual.get('daily_equity', {}), dtype=float).sort_index()
        same_dates = expected.index.equals(observed.index) and len(expected) > 0
        errors = ((observed - expected).abs() / expected.abs()) if same_dates else pd.Series(dtype=float)
        maximum = float(errors.max()) if len(errors) and np.isfinite(errors).all() else None
        matched = bool(actual['ok'] and same_dates and maximum is not None and maximum <= 1e-9)
        return dict(matched=matched, start=interval['start'], end=interval['end'], n_days=len(expected),
                    same_dates=same_dates, max_relative_error=maximum, tolerance=1e-9,
                    error=None if matched else actual.get('error', 'daily equity reproduction mismatch'))
    except Exception as error:
        return dict(matched=False, tolerance=1e-9, error=f'{type(error).__name__}: {error}')


def ledger_result(stats, accepted_count, backtests_ok, s0_ok, hashes, turnover_ratio,
                  gross_exposure_ratio, ledger_path, drift, allow_input_drift):
    def build(k, _previous):
        return dict(stats, **verdict(stats, k, accepted_count, backtests_ok, s0_ok),
                    track='fq_crypto', change_type='E', change_types=['E', 'C'], stage='S1', idea_id=IDEA,
                    baseline_version=hashes['ensemble.json'][:16] + ':ls_invvol_protected_v1',
                    prereg_path=PREREG, prereg_sha256=sha256(REPO / PREREG), input_hashes=hashes,
                    turnover_ratio=turnover_ratio, gross_exposure_ratio=gross_exposure_ratio,
                    allow_input_drift=allow_input_drift, input_drift=drift)
    if ledger_path is not None:
        return evolution_ledger.append_event(ledger_path, 'fq_crypto', build)
    return build(evolution_ledger.next_k(evolution_ledger.read_rows(DEFAULT_LEDGER), 'fq_crypto'), [])


def _ratio(new, base):
    return new / base if new is not None and base is not None and base != 0 else None


def run_study(base_dir, snapshot_dir, full_dir, *, ledger_path=None, allow_input_drift=False,
              workers=DEFAULT_WORKERS, progress=lambda _: None):
    started = time.monotonic()
    assert not options.enabled(options.GREEDY_ENSEMBLE), 'FM_GREEDY_ENSEMBLE must be off'
    hashes, drift = guard_inputs(snapshot_dir, allow_input_drift)
    validation_hash = sha256(Path(full_dir) / 'validation.json')
    zoo = json.loads((Path(snapshot_dir) / 'zoo.json').read_text(encoding='utf-8'))
    pool = [f for f in zoo['factors'] if f['horizon'] == 3]
    assert len({f['id'] for f in pool}) == len(pool), 'duplicate h=3 factor ids'
    with snapshot_root(base_dir, snapshot_dir) as root:
        factors, frozen = quant_targets._load_frozen(root)
        assert all(frozen[name]['sha256'] == digest for name, digest in hashes.items()), 'snapshot changed'
        if not drift:
            assert len(pool) == 3866 and len(factors) == 11, 'frozen factor counts drift'
        base_selection = panel.load_panels('crypto', root)
        full_selection = universe_expansion_study._panels(full_dir, 60)
        split_audit = {}
        for name, p in (('base', base_selection), ('full', full_selection)):
            splits = assert_splits(p, zoo)
            split_audit[name] = dict(windows=p.windows, ranges=splits['ranges'])
        progress(f'panels base={base_selection.mask.shape} full={full_selection.mask.shape}; pool={len(pool)}')
        rows, selected = reaccept(full_selection, pool, workers=workers, progress=progress)
        counts = Counter(f['status'] for f in rows)
        baseline_score = ensemble.ensemble_scores(factors, base_selection)
        new_score = ensemble.ensemble_scores(selected, full_selection)
        base, full = truncate_panels(base_selection), truncate_panels(full_selection)
        digests = {name: dict(selection=panel_digest(selection), backtest=panel_digest(bt))
                   for name, selection, bt in (('base', base_selection, base), ('full', full_selection, full))}
        arms = dict(B=(base, baseline_score, factors, 'protected'),
                    UH=(full, new_score, selected, 'hedge'), U=(full, new_score, selected, 'protected'),
                    H=(base, baseline_score, factors, 'hedge'))
        intervals, returns = {}, {}
        for name, start, end in (('forward', FORWARD_START, END_BT), ('val', *VAL_RANGE)):
            runs, returns[name] = {}, {}
            for arm, (p, score, members, rule) in arms.items():
                dates = p.mask.index[(p.mask.index >= pd.Timestamp(start)) & (p.mask.index <= pd.Timestamp(end))]
                progress(f'backtest {name}/{arm} days={len(dates)}')
                runs[arm] = run_arm(p, score, members, dates, rule)
                returns[name][arm] = daily_returns(runs[arm]) if 'daily_equity' in runs[arm] else pd.Series(dtype=float)
            intervals[name] = dict(start=start, end=end, selection_bias=name == 'val', arms=runs,
                                   paired=paired_statistics(returns[name]['UH'], returns[name]['B']))
        progress('S0: quant_targets.backtest on truncated snapshot copies')
        with snapshot_root(base_dir, snapshot_dir, truncated=True) as truncated_root:
            reproduction = reproduce_baseline(truncated_root, base, factors)
    decomposition = {f'{a}-{b}': paired_statistics(returns['forward'][a], returns['forward'][b])
                     for a, b in (('U', 'B'), ('H', 'B'), ('UH', 'U'), ('UH', 'H'))}
    deltas = [intervals['forward']['paired']['delta'], decomposition['U-B']['delta'], decomposition['H-B']['delta']]
    interaction = deltas[0] - deltas[1] - deltas[2] if all(x is not None for x in deltas) else None
    # Labels and IC use the execution cutoff too: no November outcomes enter diagnostics.
    ics = {}
    for name, p, score in (('B', base, baseline_score), ('UH', full, new_score)):
        excess, _ = panel.forward_labels(p, 3, FORWARD_START)
        dates = p.mask.index[p.mask.index >= pd.Timestamp(FORWARD_START)]
        ics[name] = ic_summary(rank_ic(score.loc[dates], excess.loc[dates], p.mask.loc[dates]))
    forward_dates = full.mask.index[full.mask.index >= pd.Timestamp(FORWARD_START)]
    full_mask = full.mask.loc[forward_dates]
    aligned_base = base.mask.reindex(index=forward_dates, columns=full_mask.columns, fill_value=False)
    composition = dict(mean_daily_overlap=float((full_mask & aligned_base).sum(axis=1).mean()),
                       full_only_symbols=sorted(s for s in full_mask.columns
                                                if s not in base.mask.columns and full_mask[s].any()))
    forward_arms = intervals['forward']['arms']
    metrics = {arm: run.get('metrics', {}) for arm, run in forward_arms.items()}
    turnover_ratio = _ratio(metrics['UH'].get('turnover'), metrics['B'].get('turnover'))
    exposure_ratio = _ratio(metrics['UH'].get('average_exposure'), metrics['B'].get('average_exposure'))
    assert guard_inputs(snapshot_dir, allow_input_drift)[0] == hashes, 'snapshot changed during study'
    assert sha256(Path(full_dir) / 'validation.json') == validation_hash, 'validation.json changed'
    input_hashes = dict(hashes, **{'validation.json': validation_hash})
    primary_stats = intervals['forward']['paired']
    row = ledger_result(primary_stats, counts['accepted'], all(forward_arms[a]['ok'] for a in ('B', 'UH')),
                        reproduction['matched'], input_hashes, turnover_ratio, exposure_ratio,
                        ledger_path, drift, allow_input_drift)
    overlap = sorted(set(f['id'] for f in factors) & set(f['id'] for f in selected))
    deviations = [
        '选择日历保留至 2026-11-02，以复现快照 h=3 的 trailing 切分；评估器只使用 train/val 标签。'
        '执行面板的所有字段和因果 mask 在内存截断至 2026-10-31；S0 另用截断 JSON 副本。',
        '去冗余只检查已被 classify 验收为 accepted 的因子；candidate 不参与组合，不改变组合选择。'
        '排序、train∪val 每五日采样及有方向秩的相关性沿用 miner。',
        'S0 使用 quant_targets 固定的前向起点 2026-05-04；主检验另从预注册的 2026-05-07 开始。',
        '账本仅允许 A–E，故 change_type=E，另记 change_types=[E,C] 表示组合改动。',
        '预注册未定义平均 beta 的加权方式：按多头目标权重加权（截断后的 beta，除以多头总额），'
        '再对有效信号作时间等权平均；BTC 收益零方差时 OLS 不可识别，回退 beta=1。',
        '腿贡献复用 wide_pool_study 的实际成交、持仓、成本和资金费归因；总敞口比使用引擎实际持仓平均毛敞口。'
        '空头尾部按每个调仓日实际沿用的目标权重计数，包括非 ok 信号保留的上期权重。',
    ]
    if drift:
        deviations.append('显式允许冻结输入漂移；哈希及差异已记录，此结果不是原冻结输入研究。')
    return dict(idea_id=IDEA, prereg_path=PREREG, prereg_sha256=sha256(REPO / PREREG),
                inputs=dict(base_dir=str(Path(base_dir).resolve()), snapshot_dir=str(Path(snapshot_dir).resolve()),
                            full_dir=str(Path(full_dir).resolve())), input_hashes=input_hashes,
                expected_hashes=EXPECTED_HASHES, input_drift=drift, allow_input_drift=allow_input_drift,
                frozen=frozen, panels=dict(base=base.meta, full=full.meta), panel_digests=digests,
                parameters=dict(horizon=3, K_SEL=K_SEL, t_required=evaluate.t_required(K_SEL),
                                universe_n=60, workers=workers, commission=COMMISSION, slippage=SLIPPAGE,
                                end_bt=END_BT, forward_start=FORWARD_START, val_range=VAL_RANGE),
                split_audit=split_audit, reacceptance=dict(pool_size=len(pool), counts={s: counts[s] for s in STATUSES},
                    selected=selected, factors=rows, snapshot_overlap_ids=overlap, snapshot_overlap_count=len(overlap)),
                intervals=intervals, decomposition=decomposition, interaction=interaction, rank_ic=ics,
                universe_composition=composition, baseline_reproduction=reproduction,
                turnover_ratio=turnover_ratio, gross_exposure_ratio=exposure_ratio,
                primary=dict(primary_stats, **{k: row[k] for k in ('verdict', 'K', 'z_K', 'threshold_K', 'invalid_reasons')}),
                ledger=dict(path=str(ledger_path) if ledger_path is not None else None,
                            written=ledger_path is not None, row=row), deviations=deviations,
                elapsed_seconds=time.monotonic() - started)


def render_report(result):
    p = result['primary']
    lines = ['# 时点 top 60 + BTC 对冲：预注册 S1 研究', '',
             f'结论：**{p["verdict"]}**；Δ={number(p["delta"])} / 日，SE={number(p["se"])}，'
             f'z={number(p["z"], 4)}，K={p["K"]}，z_K={number(p["z_K"], 4)}。', '',
             '**前向区间已被看过，N=60 的选择看过 val；S1 仅用于筛选，决定性证据需来自后续 S2。**', '',
             f'预注册：`{PREREG}`；耗时 {result["elapsed_seconds"]:.3f} 秒。', '', '## 主检验', '',
             '| 指标 | 数值 |', '| --- | ---: |']
    for key in ('delta', 'se', 'z', 'z_K', 'n_days', 'n_weeks', 'dev_delta', 'holdout_delta'):
        lines.append(f'| {key} | {number(p[key])} |')
    lines += [f'| 换手率比 UH/B | {number(result["turnover_ratio"])} |',
              f'| 实际平均毛敞口比 UH/B | {number(result["gross_exposure_ratio"])} |', '',
              f'前后半：{p["half_ranges"]}；奇数中间日归前半。无效原因：{p["invalid_reasons"]}。',
              '日简单收益首日对初始资金 1000；SE 按 UTC 周一周聚类，含 G/(G−1) 修正。', '',
              '## 各组表现', '',
              '| 区间 | 组 | 收益 % | 年化 % | Sharpe | 最大回撤 % | 换手 | 手续费 | 资金费净付出 |',
              '| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
    errors = []
    for name, interval in result['intervals'].items():
        for arm, run in interval['arms'].items():
            m = run.get('metrics', {})
            lines.append('| ' + ' | '.join([name, arm] + [number(m.get(k), 4) for k in (
                'return_pct', 'cagr_pct', 'sharpe', 'maxdd_pct', 'turnover', 'commission_paid', 'funding_paid')]) + ' |')
            if run.get('error'):
                errors.append(f'回测失败 {name}/{arm}：{run["error"]}')
    for error in errors:
        lines += ['', error]
    lines += ['', '## 四组拆解（描述）', '', '| 比较 | Δ | SE | z | 配对日 |', '| --- | ---: | ---: | ---: | ---: |']
    for name, stats in result['decomposition'].items():
        lines.append('| ' + ' | '.join([name] + [number(stats[k]) for k in ('delta', 'se', 'z', 'n_days')]) + ' |')
    val = result['intervals']['val']['paired']
    lines += ['', f'交互项 Δ(UH−B)−Δ(U−B)−Δ(H−B)={number(result["interaction"])}。拆解不能单独促成上线。', '',
              '## 验证段（描述）', '', f'{VAL_RANGE[0]}～{VAL_RANGE[1]}：UH−B Δ={number(val["delta"])}，'
              f'SE={number(val["se"])}，z={number(val["z"])}，n={val["n_days"]}。两组均用 val 选因子，有选择偏差。', '',
              '## 前向 RankIC', '', '| 组 | 均值 | NW t（lag=3） | 日数 |', '| --- | ---: | ---: | ---: |']
    for arm, ic in result['rank_ic'].items():
        lines.append(f'| {arm} | {number(ic["mean"])} | {number(ic["nw_t"])} | {ic["n_days"]} |')
    lines += ['', '## 多空腿贡献与对冲', '',
              '| 区间 | 组 | 腿 | 毛损益 | 滑点 | 手续费 | 资金费 | 净损益 | 初始资金贡献 % |',
              '| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |']
    hedge_summaries = []
    for name, interval in result['intervals'].items():
        for arm, run in interval['arms'].items():
            for side in ('long', 'short'):
                leg = run.get('legs', {}).get(side, {})
                lines.append('| ' + ' | '.join([name, arm, side] + [number(leg.get(k), 4) for k in (
                    'gross_pnl', 'slippage_paid', 'commission_paid', 'funding_paid', 'net_pnl',
                    'contribution_pct_initial')]) + ' |')
            if 'hedge_summary' in run:
                hedge_summaries.append(f'{name}/{arm}：平均 h={number(run["hedge_summary"]["h"])}，'
                                       f'平均 beta={number(run["hedge_summary"]["mean_beta"])}。')
    for summary in hedge_summaries:
        lines += ['', summary]
    lines += ['', '## 空头右尾（前向）', '', '| 组 | 名次观测 n | 三日涨幅 >30% 占比 | 加权空头亏损份额 |',
              '| --- | ---: | ---: | ---: |']
    for arm in ('B', 'U'):
        tail = result['intervals']['forward']['arms'][arm].get('short_tail', {})
        lines.append('| ' + ' | '.join([arm] + [number(tail.get(k)) for k in
                                               ('n', 'share_gt_30', 'loss_share_from_gt_30')]) + ' |')
    acceptance = result['reacceptance']
    lines += ['', '## 重新验收与币池', '', f'状态计数：{acceptance["counts"]}；'
              f'快照组合重合 {acceptance["snapshot_overlap_count"]}：{acceptance["snapshot_overlap_ids"]}。', '',
              '| id | 表达式 | 族 | 方向 | train IC | train t | val IC | val t |',
              '| --- | --- | --- | ---: | ---: | ---: | ---: | ---: |']
    for f in acceptance['selected']:
        lines.append(f'| {f["id"]} | `{f["expr"].replace("|", "&#124;")}` | {f["family"]} | {f["direction"]} | '
                     + ' | '.join(number(f[seg][k]) for seg in ('train', 'val') for k in ('ic', 't')) + ' |')
    lines += ['', f'前向币池构成：{result["universe_composition"]}。', '', '## 输入与 S0 复现', '']
    for key in ('inputs', 'input_hashes', 'prereg_sha256', 'panel_digests', 'split_audit', 'input_drift',
                'baseline_reproduction', 'ledger'):
        value = result[key]
        if key == 'ledger':
            value = {k: value[k] for k in ('path', 'written')}
        lines.append(f'- {key}：{json.dumps(json_safe(value), ensure_ascii=False)}')
    lines += ['', '每三日发信号、次日开盘成交，手续费 0.0005、滑点 0.0003、实际资金费；'
              '各区间从空仓开始，末日不伪造平仓。完整信号、净值及重验收清单见 report.json。', '',
              '## deviations（偏离与歧义解释）', '']
    lines += ['- ' + deviation for deviation in result['deviations']]
    return '\n'.join(lines) + '\n'


def validate_outputs(base_dir, snapshot_dir, out, ledger_path):
    base, snapshot, out = (Path(p).resolve() for p in (base_dir, snapshot_dir, out))
    if out == base or base in out.parents or out == snapshot:
        raise ValueError('out must not be inside base crypto/ or equal to snapshot dir')
    reports = [out / 'report.json', out / 'report.md']
    if ledger_path is not None:
        ledger = Path(ledger_path).resolve()
        if ledger in reports or ledger == out:
            raise ValueError('ledger and report paths must differ')
        if ledger == base or base in ledger.parents or ledger == snapshot or snapshot in ledger.parents:
            raise ValueError('ledger must not overwrite study inputs')
    return out


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    ledger = parser.add_mutually_exclusive_group()
    ledger.add_argument('--ledger', type=Path, default=DEFAULT_LEDGER)
    ledger.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--workers', type=int, default=DEFAULT_WORKERS)
    parser.add_argument('--allow-input-drift', action='store_true')
    parser.add_argument('--base-dir', type=Path, default=BASE_DIR)
    parser.add_argument('--snapshot-dir', type=Path, default=SNAPSHOT_DIR)
    parser.add_argument('--full-dir', type=Path, default=FULL_DIR)
    args = parser.parse_args(argv)
    ledger_path = None if args.no_ledger else args.ledger.resolve()
    try:
        if args.workers < 1:
            raise ValueError('workers must be positive')
        out = validate_outputs(args.base_dir, args.snapshot_dir, args.out, ledger_path)
        guard_inputs(args.snapshot_dir, args.allow_input_drift)
    except ValueError as error:
        parser.error(str(error))
    result = run_study(args.base_dir, args.snapshot_dir, args.full_dir, ledger_path=ledger_path,
                       allow_input_drift=args.allow_input_drift, workers=args.workers,
                       progress=lambda s: print(s, file=sys.stderr, flush=True))
    out.mkdir(parents=True, exist_ok=True)
    (out / 'report.json').write_text(json.dumps(json_safe(result), ensure_ascii=False, indent=2,
                                               allow_nan=False) + '\n', encoding='utf-8')
    (out / 'report.md').write_text(render_report(result), encoding='utf-8')
    print(f'verdict={result["primary"]["verdict"]}; K={result["primary"]["K"]}; '
          f'z={number(result["primary"]["z"], 4)}; elapsed_seconds={result["elapsed_seconds"]:.3f}')
    print(f'reports: {out / "report.json"}, {out / "report.md"}')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
