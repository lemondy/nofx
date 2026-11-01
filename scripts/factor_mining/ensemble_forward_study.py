"""Frozen-constituent, offline ensemble forward study; never writes the data root.

All experiment constants below implement the 2026-11-01 preregistration.
No selection, sign changes, fitted parameters or network calls are performed.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
from pathlib import Path
import sys
import time
from datetime import datetime, timezone

import numpy as np
import pandas as pd

from . import config, ensemble, evaluate, panel
from .sensitivity_20261028 import holding_funding, funding_adjusted_ls
from scripts.backtest import quantjourney as q

HORIZONS = (1, 3, 7)
FORWARD_START = '2026-05-04'
VAL_RANGE = ('2025-05-04', '2026-04-30')
SEED = 20261101
BOOTSTRAPS = 2000
PLACEBOS = 500
COMMISSION = 0.0005
SLIPPAGE = 0.0003
ONE_WAY_COST = COMMISSION + SLIPPAGE


def load_frozen(root):
    """Read each snapshot once; resolve by BOTH id and horizon, fail closed."""
    documents, provenance = {}, {}
    for name in ('ensemble', 'zoo'):
        path = Path(root) / 'crypto' / (name + '.json')
        data = path.read_bytes()
        documents[name] = json.loads(data)
        provenance[name] = dict(path=str(path.resolve()), sha256=hashlib.sha256(data).hexdigest(),
                                generated_at=documents[name].get('generated_at'))
    lookup = {}
    for factor in documents['zoo']['factors']:
        key = (factor['id'], int(factor['horizon']))
        if key in lookup:
            raise ValueError(f'duplicate zoo entry: {key}')
        lookup[key] = factor
    factors = {}
    for h in HORIZONS:
        entry = documents['ensemble']['horizons'].get(str(h))
        if not entry or not entry.get('factor_ids'):
            raise ValueError(f'empty frozen ensemble: h={h}')
        ids = entry['factor_ids']
        if len(ids) != len(set(ids)):
            raise ValueError(f'duplicate ensemble ids: h={h}')
        factors[h] = []
        for id_ in ids:
            if (id_, h) not in lookup:
                raise ValueError(f'ensemble id missing from zoo at same horizon: {id_}/{h}')
            f = lookup[id_, h]
            if not isinstance(f.get('expr'), str) or not f['expr'] or f.get('direction') not in (-1, 1):
                raise ValueError(f'invalid frozen expression/direction: {id_}/{h}')
            factors[h].append(dict(id=id_, horizon=h, expr=f['expr'], direction=f['direction']))
    return factors, documents['ensemble'], provenance


def forward_segment(excess, start):
    # Slice BEFORE touching labels: pre-F values do not enter even validity checks.
    segment = pd.Series(False, index=excess.index)
    visible = excess.loc[excess.index >= pd.Timestamp(start)]
    segment.loc[visible.index] = np.isfinite(visible).any(axis=1)
    return segment


def signal_metrics(score, excess, raw, mask, funding, h, start, cfg):
    visible = score.index >= pd.Timestamp(start)
    score, excess, raw, mask, funding = (x.loc[visible] for x in (score, excess, raw, mask, funding))
    segment = forward_segment(excess, start)
    ic = evaluate.cs_rank_ic(score, excess, mask, cfg['min_names']).loc[segment].dropna()
    net, churn = evaluate.long_short(score, raw, mask, segment, h, cfg)
    funded = funding_adjusted_ls(score, raw, funding, mask, segment, h, cfg)
    mean = float(ic.mean()) if len(ic) else None
    sd = float(ic.std(ddof=1)) if len(ic) > 1 else 0.
    t = evaluate.nw_t(ic, h)
    rsd = float(net.std(ddof=1)) if len(net) > 1 else 0.
    midpoint = (score.index[0] + (score.index[-1] - score.index[0]) / 2) if len(score) else pd.NaT
    halves = [ic.loc[ic.index <= midpoint], ic.loc[ic.index > midpoint]]
    a1 = mean is not None and mean > 0 and t >= 1.65
    a2 = funded['ls_ret'] is not None and funded['ls_ret'] > 0
    return dict(h=h, n_days=len(ic), segment_days=int(segment.sum()),
                first_signal=day(ic.index[0]) if len(ic) else None,
                last_signal=day(ic.index[-1]) if len(ic) else None,
                ic=mean, icir=mean / sd if mean is not None and sd > 1e-15 else 0.,
                nw_t=t, ic_halves=[float(x.mean()) if len(x) else None for x in halves],
                ls_annual=float(net.mean() * 365 / h) if len(net) else None,
                ls_sharpe=float(net.mean() / rsd * math.sqrt(365 / h)) if rsd > 1e-15 else 0.,
                turnover=float(churn.mean()) if len(churn) else None,
                funding=funded, A1=bool(a1), A2=bool(a2), passed=bool(a1 and a2))


def signal_positions(index, start, period):
    index = pd.DatetimeIndex(index)
    if period < 1 or not index.is_unique or not index.is_monotonic_increasing:
        raise ValueError('positive period and unique increasing calendar required')
    start = pd.Timestamp(start)
    if start not in index:
        raise ValueError(f'signal start missing from panel calendar: {start}')
    return np.arange(index.get_loc(start), len(index), period, dtype=int)


def rank_weights(values, eligible, fraction=.2):
    """Stable ties use panel column order; n<2 is cash (no disjoint legs)."""
    if not 0 < fraction <= .5:
        raise ValueError('fraction must be in (0, .5]')
    values = np.asarray(values, float)
    names = np.flatnonzero(np.asarray(eligible, bool) & np.isfinite(values))
    weights = np.zeros(len(values))
    if len(names) < 2:
        return weights
    count = max(1, math.floor(len(names) * fraction))
    ordered = names[np.argsort(values[names], kind='stable')]
    weights[ordered[:count]] = -.5 / count
    weights[ordered[-count:]] = .5 / count
    return weights


def build_targets(score, mask, start, period, fraction=.2):
    if not score.index.equals(mask.index) or not score.columns.equals(mask.columns):
        raise ValueError('score and mask axes differ')
    positions = signal_positions(score.index, start, period)
    result = pd.DataFrame(0., index=score.index, columns=score.columns)
    for j, i in enumerate(positions):
        stop = positions[j + 1] if j + 1 < len(positions) else len(score)
        result.iloc[i:stop] = rank_weights(score.iloc[i], mask.iloc[i], fraction)
    return result


def panel_frames(panels, index):
    fields = ('open', 'high', 'low', 'close', 'volume')
    frames = {}
    for symbol in panels.mask.columns:
        frame = pd.DataFrame({name: panels.fields[name][symbol].reindex(index) for name in fields})
        # Keep missing observations absent; run_weights owns availability/marking.
        frame = frame.dropna(subset=list(fields))
        q.validate_frame(frame)
        frames[symbol] = frame
    return frames


def funding_rates(funding, index):
    return {symbol: {d.date(): float(v) for d, v in funding[symbol].reindex(index).dropna().items()
                     if np.isfinite(v)} for symbol in funding.columns}


def daily_statistics(equity):
    equity = pd.Series(equity, dtype=float).sort_index()
    equity.index = pd.to_datetime(equity.index)
    returns = equity.div(equity.shift(1, fill_value=q.CAPITAL)) - 1
    sd = float(returns.std(ddof=1)) if len(returns) > 1 else 0.
    midpoint = equity.index[0] + (equity.index[-1] - equity.index[0]) / 2
    halves = [returns.loc[returns.index <= midpoint], returns.loc[returns.index > midpoint]]
    initial_curve = np.r_[q.CAPITAL, equity.to_numpy()]
    peaks = np.maximum.accumulate(initial_curve)
    total = float(equity.iloc[-1] / q.CAPITAL - 1)
    return dict(total_return=total,
                annual_return=float((1 + total) ** (365 / len(equity)) - 1) if total > -1 else None,
                sharpe=float(returns.mean() / sd * math.sqrt(365)) if sd > 1e-15 else 0.,
                max_drawdown=float(np.max(1 - initial_curve / peaks)), n_days=len(equity),
                half_returns=[float((1 + x).prod() - 1) for x in halves],
                half_ranges=[[day(x.index[0]), day(x.index[-1])] if len(x) else [] for x in halves]), returns


def block_bootstrap(returns, repetitions=BOOTSTRAPS, seed=SEED):
    x = np.asarray(returns, float)
    if not np.isfinite(x).all():
        raise ValueError('nonfinite bootstrap returns')
    # Full, nonoverlapping seven-day blocks; omit the final incomplete block.
    count = len(x) // 7
    if not count:
        raise ValueError('at least seven daily returns required')
    blocks = x[:count * 7].reshape(count, 7).mean(axis=1)
    rng = np.random.default_rng(seed)
    means = blocks[rng.integers(0, count, size=(repetitions, count))].mean(axis=1)
    return dict(probability_positive=float(np.mean(means > 0)), repetitions=repetitions,
                seed=seed, block_days=7, n_blocks=count, omitted_tail_days=len(x) % 7,
                mean_quantiles=np.quantile(means, [.025, .5, .975]).tolist())


def beta_to_btc(returns, btc_close):
    joined = pd.concat([returns.rename('strategy'), btc_close.pct_change(fill_method=None).rename('btc')],
                       axis=1).loc[returns.index].dropna()
    if len(joined) < 2 or joined.btc.var(ddof=0) <= 1e-30:
        return dict(beta=None, alpha=None, n=len(joined))
    beta, alpha = np.linalg.lstsq(np.column_stack([joined.btc, np.ones(len(joined))]),
                                joined.strategy, rcond=None)[0]
    return dict(beta=float(beta), alpha=float(alpha), n=len(joined))


def audit_fills(run, targets, opens):
    """Check native fills, missing execution prices, held stale marks, delistings."""
    index, symbols = targets.index, targets.columns
    changed = np.r_[True, np.any(np.diff(targets.to_numpy(), axis=0) != 0, axis=1)]
    scheduled = {i + 1 for i in np.flatnonzero(changed) if i + 1 < len(index)}
    delayed, unexpected = [], []
    fills_by_bar = {}
    for fill in run['fills']:
        actual = index.get_loc(pd.Timestamp(fill['date']).normalize())
        intended = int(fill['signal_bar']) + 1
        if actual != int(fill['execution_bar']):
            raise ValueError('fill date/execution_bar mismatch')
        if actual != intended:
            missing = opens[fill['symbol']].iloc[intended:actual].isna().all()
            (delayed if actual > intended and missing else unexpected).append(fill)
        elif actual not in scheduled:
            unexpected.append(fill)
        fills_by_bar.setdefault(actual, []).append(fill)
    if unexpected:
        raise ValueError(f'unscheduled native fills: {unexpected[:3]}')
    quantities = pd.Series(0., index=symbols)
    missing_intents, held_missing, terminal = [], [], []
    observed_opens = opens.reindex(index)
    for i, date in enumerate(index):
        missing = observed_opens.iloc[i].isna()
        if i in scheduled:
            for symbol in symbols[missing & ((targets.iloc[i - 1] != 0) | (quantities.abs() > 1e-12))]:
                missing_intents.append(dict(date=day(date), symbol=symbol))
        for fill in fills_by_bar.get(i, []):
            quantities[fill['symbol']] += fill['size']
        for symbol in symbols[missing & (quantities.abs() > 1e-12)]:
            held_missing.append(dict(date=day(date), symbol=symbol))
    for symbol in symbols[quantities.abs() > 1e-12]:
        last = observed_opens[symbol].last_valid_index()
        if last is None or last < index[-1]:
            terminal.append(dict(symbol=symbol, last_observed=day(last) if last is not None else None,
                                 final_quantity=float(quantities[symbol])))
    return dict(scheduled_execution_days=len(scheduled), fills=len(run['fills']),
                delayed_fills=len(delayed), delayed_fill_details=delayed, unexpected_fills=0,
                missing_open_intents=len(missing_intents), missing_open_details=missing_intents,
                held_missing_price_days=len(held_missing), held_missing_details=held_missing,
                terminal_stale_positions=terminal)


def backtest(panels, score, start, end, h, fraction=.2, period=None):
    period = h if period is None else period
    # Build only to the declared endpoint, so val signals cannot read forward data.
    score = score.loc[:end]
    targets = build_targets(score, panels.mask.loc[score.index], start, period, fraction).loc[start:end]
    if not targets.index.equals(pd.date_range(start, end, freq='D')):
        raise ValueError('B requires a complete daily crypto calendar')
    frames = panel_frames(panels, targets.index)
    run = q.run_weights(frames, targets, market='crypto', commission=COMMISSION, slippage=SLIPPAGE,
                        funding=funding_rates(panels.fields['funding'], targets.index),
                        policy=q.RebalancePolicy(None, rebalance_on_signal_change=True), keep_fills=True)
    stats, returns = daily_statistics(run['daily_equity'])
    stats.update({k: run['metrics'][k] for k in ('turnover', 'commission_paid', 'funding_paid', 'trades',
                                               'funding_observations', 'funding_covered', 'average_exposure')})
    audit = audit_fills(run, targets, panels.fields['open'])
    return dict(fraction=fraction, period=period, metrics=stats, execution_audit=audit,
                daily_equity=run['daily_equity'], initial_capital=q.CAPITAL,
                engine_version=run['engine_version']), returns, targets


def vectorized_replica(panels, score, start, end, period, fraction=.2, *, permutations=0, seed=SEED,
                       cost=ONE_WAY_COST, include_funding=True):
    """Batch placebo paths with actual fixed shares between changed targets.

    Entry t+1 open; next changed target's t+1 open; final mark at last close.
    Cost is abs traded notional * 8bp. Funding uses fixed entry notional and
    holding_funding: entry UTC day included, exit UTC day excluded. This timing
    intentionally follows the registered replica, unlike CarryBroker's carry.
    Missing rates use zero, matching the native engine, with counts reported.
    """
    index = score.loc[start:end].index
    full_score = score.loc[:end]
    positions = signal_positions(index, start, period)
    columns = score.columns
    opens = panels.fields['open'].reindex(index=index, columns=columns)
    close = panels.fields['close'].reindex(index=index, columns=columns).ffill().fillna(1.).to_numpy()
    available = opens.notna().to_numpy()
    open_marks = opens.ffill().fillna(1.).to_numpy()
    funding = panels.fields['funding'].reindex(index=index, columns=columns)
    daily_funding = funding.fillna(0.).to_numpy() * (3. if include_funding else 0.)
    scores = full_score.reindex(index).to_numpy()
    masks = panels.mask.reindex(index=index, columns=columns).to_numpy()
    paths = permutations + 1
    rng = np.random.default_rng(seed)
    weights = np.zeros((paths, len(positions), len(columns)))
    for j, i in enumerate(positions):
        names = np.flatnonzero(masks[i] & np.isfinite(scores[i]))
        weights[0, j] = rank_weights(scores[i], masks[i], fraction)
        for p in range(1, paths):
            shuffled = scores[i].copy()
            shuffled[names] = rng.permutation(shuffled[names])
            weights[p, j] = rank_weights(shuffled, masks[i], fraction)
    quantities = np.zeros((paths, len(columns)))
    entry_nominal = quantities.copy()
    cash = np.full(paths, q.CAPITAL)
    daily = np.full((paths, len(index)), q.CAPITAL)
    previous = np.zeros_like(quantities)
    missing_executions = np.zeros(paths, dtype=int)
    funding_check_error = 0.
    accrual_cache = {}
    for j, signal in enumerate(positions):
        entry = signal + 1
        if entry >= len(index):
            continue
        stop = min(positions[j + 1] + 1, len(index)) if j + 1 < len(positions) else len(index)
        desired = weights[:, j]
        change = np.any(desired != previous, axis=1)
        previous = desired.copy()
        prices = open_marks[entry]
        nav = cash + quantities @ prices
        current = quantities * prices / nav[:, None]
        feasible = desired.copy()
        keep = ~available[entry]
        missing_executions += change & np.any(keep & ((desired != 0) | (np.abs(quantities) > 1e-12)), axis=1)
        if keep.any():
            feasible[:, keep] = current[:, keep]
            gross = np.abs(desired[:, ~keep]).sum(axis=1)
            budget = np.maximum(np.abs(desired).sum(axis=1) - np.abs(feasible[:, keep]).sum(axis=1), 0.)
            scale = np.divide(budget, gross, out=np.zeros(paths), where=gross > 1e-12)
            feasible[:, ~keep] = desired[:, ~keep] * scale[:, None]
        new_quantity = feasible * nav[:, None] / prices
        churn = np.abs((new_quantity - quantities) * prices).sum(axis=1)
        quantities[change] = new_quantity[change]
        cash[change] = nav[change] - (new_quantity[change] * prices).sum(axis=1) - cost * churn[change]
        entry_nominal[change] = quantities[change] * prices
        length = stop - entry
        if include_funding:
            if length not in accrual_cache:
                accrual_cache[length] = holding_funding(funding.fillna(0.), length).to_numpy()
            accrued = accrual_cache[length][signal]
            daily_sum = daily_funding[entry:stop].sum(axis=0)
            funding_check_error = max(funding_check_error, float(np.max(np.abs(accrued - daily_sum))))
        for i in range(entry, stop):
            cash -= entry_nominal @ daily_funding[i]
            daily[:, i] = cash + quantities @ close[i]
        # Aggregate path funding is exactly the holding_funding interval charge.
    returns = daily / np.column_stack([np.full(paths, q.CAPITAL), daily[:, :-1]]) - 1
    totals = daily[:, -1] / q.CAPITAL - 1
    return dict(main_total_return=float(totals[0]), placebo_total_returns=totals[1:].tolist(),
                percentile=float(np.mean(totals[1:] <= totals[0])) if permutations else None,
                seed=seed, permutations=permutations,
                daily_equity={day(d): float(v) for d, v in zip(index, daily[0])},
                main_daily_returns=pd.Series(returns[0], index=index),
                missing_execution_path_events=int(missing_executions.sum()),
                missing_funding_cells=int(funding.isna().to_numpy().sum()),
                holding_funding_check_error=funding_check_error)


def spent_intervals(root, start, end):
    files, overlaps = [], []
    for path in (Path(root) / 'holdout_spent.jsonl', Path(root) / 'crypto/holdout_spent.jsonl'):
        if not path.exists():
            continue
        files.append(str(path.resolve()))
        for line_no, line in enumerate(path.read_text().splitlines(), 1):
            if not line.strip():
                continue
            record = json.loads(line)
            if record.get('market', 'crypto') != 'crypto':
                continue
            bounds = record.get('holdout_range', record.get('range'))
            if not bounds and record.get('start') and record.get('end'):
                bounds = [record['start'], record['end']]
            if not bounds or len(bounds) != 2:
                raise ValueError(f'unrecognized spent ledger range: {path}:{line_no}')
            a, b = map(pd.Timestamp, bounds)
            if a <= pd.Timestamp(end) and b >= pd.Timestamp(start):
                overlaps.append(dict(file=str(path.resolve()), line=line_no, interval=list(bounds),
                                     overlap=[day(max(a, pd.Timestamp(start))), day(min(b, pd.Timestamp(end)))],
                                     record=record))
    return dict(existing_files=files, overlapping_intervals=overlaps)


def run_study(root, progress=print):
    started = time.monotonic()
    factors, frozen, provenance = load_frozen(root)
    progress('已冻结 ensemble/zoo 成分及哈希；加载只读面板。')
    panels = panel.load_panels('crypto', root)
    if 'funding' not in panels.fields or 'BTCUSDT' not in panels.mask:
        raise ValueError('funding and BTCUSDT are required')
    splits = {h: panel.make_splits(panels.mask.index, h, panels.windows) for h in HORIZONS}
    starts = {splits[h]['ranges']['holdout'][0] for h in HORIZONS}
    if starts != {FORWARD_START}:
        raise ValueError(f'preregistered common F mismatch: {starts}, expected {FORWARD_START}')
    end = day(panels.mask.index[-1])
    spent = spent_intervals(root, FORWARD_START, end)
    result = dict(design=dict(F=FORWARD_START, end=end, val_range=list(VAL_RANGE), horizons=list(HORIZONS),
                              seed=SEED, bootstraps=BOOTSTRAPS, placebos=PLACEBOS,
                              commission=COMMISSION, slippage=SLIPPAGE, capital=q.CAPITAL,
                              thresholds=dict(A1='IC>0 and NW t>=1.65', A2='funded Q5-Q1 annual>0',
                                              B1='net total>0 and daily Sharpe>0', B2='bootstrap P(mean>0)>=0.80',
                                              B3='both calendar-half net returns>0', B4='abs(BTC OLS beta)<=0.3',
                                              B5='at least 3/4 positive variants', B6='placebo percentile>=0.90')),
                  generated_at=datetime.now(timezone.utc).isoformat(), provenance=provenance,
                  factors=factors, panel_meta=panels.meta, split_ranges={h: splits[h]['ranges'] for h in HORIZONS},
                  spent=spent, A=[], B=[], conclusions=[],
                  frozen_train_val={h: {s: frozen['horizons'][str(h)][s] for s in ('train', 'val')}
                                    for h in HORIZONS})
    # Each score is computed once by the original ensemble function, no forward selection.
    for h in HORIZONS:
        progress(f'h={h}: 计算冻结 ensemble 分数及 A 前向校验。')
        score = ensemble.ensemble_scores(factors[h], panels)
        excess, raw = panel.forward_labels(panels, h, FORWARD_START)
        metrics = signal_metrics(score, excess, raw, panels.mask, holding_funding(panels.fields['funding'], h),
                                 h, FORWARD_START, config.MARKETS['crypto'])
        result['A'].append(metrics)
        if h == 1:
            result['conclusions'].append(dict(h=h, message='h=1 预先排除 B；' +
                                              ('A 通过，仍维持仅作参考' if metrics['passed'] else
                                               'ensemble 前向未验证，维持仅作参考')))
            progress('h=1: A 已完成；按预声明排除 B。')
            continue
        if not metrics['passed']:
            failed = [k for k in ('A1', 'A2') if not metrics[k]]
            result['conclusions'].append(dict(h=h, failed=failed, message='ensemble 前向未验证，维持仅作参考'))
            progress(f'h={h}: A 未通过（{",".join(failed)}），不运行 B。')
            continue
        progress(f'h={h}: A 通过；运行四个 Backtrader 变体及 val 对照。')
        variants, main, returns, targets = [], None, None, None
        for fraction in (.2, .33):
            for period in (h, 2 * h):
                run, rets, weights = backtest(panels, score, FORWARD_START, end, h, fraction, period)
                variants.append(run)
                if fraction == .2 and period == h:
                    main, returns, targets = run, rets, weights
        validation, _, _ = backtest(panels, score, *VAL_RANGE, h)
        bootstrap = block_bootstrap(returns)
        beta = beta_to_btc(returns, panels.fields['close']['BTCUSDT'])
        progress(f'h={h}: 运行 500 次固定 seed 安慰剂向量化复刻。')
        replica = vectorized_replica(panels, score, FORWARD_START, end, h, permutations=PLACEBOS)
        comparison = dict(total_return_difference=replica['main_total_return'] - main['metrics']['total_return'],
                          daily_return_correlation=float(returns.corr(replica.pop('main_daily_returns'))))
        # Diagnostic only: never a new gate or a revised experiment threshold.
        comparison['explanation'] = (
            '向量化按固定入场名义与 holding_funding（含入场日、排除出场日）扣费；'
            'Backtrader 按开盘实际隔夜持仓名义扣费（排除新入场、包含出场日）。'
            '佣金+滑点在向量化合并为名义换手×0.08%，Backtrader 为逐笔滑点成交价及佣金；'
            '成本后的权益、名义漂移和缺价时保留持仓均可能使路径分叉。'
            '逐日估值均为收盘，最终都保留未平仓头寸；差异不用于调整参数或门槛。')
        m = main['metrics']
        gates = dict(B1=m['total_return'] > 0 and m['sharpe'] > 0,
                     B2=bootstrap['probability_positive'] >= .80,
                     B3=all(v > 0 for v in m['half_returns']),
                     B4=beta['beta'] is not None and abs(beta['beta']) <= .3,
                     B5=sum(v['metrics']['total_return'] > 0 for v in variants) >= 3,
                     B6=replica['percentile'] >= .90)
        failed = [k for k, passed in gates.items() if not passed]
        result['B'].append(dict(h=h, main=main, variants=variants, validation=validation,
                                bootstrap=bootstrap, btc_ols=beta, replica=replica, comparison=comparison,
                                gates=gates, failed=failed, passed=not failed))
        result['conclusions'].append(dict(h=h, failed=failed,
            message='不建议做独立组合' if failed else '建议以小资金在模拟盘运行市场中性因子组合（需用户决定）'))
    btc = panels.fields['close']['BTCUSDT']
    result['btc_buy_hold'] = {}
    for name, (start, stop) in {'forward': (FORWARD_START, end), 'val': VAL_RANGE}.items():
        # Close immediately before F through final close; includes F's daily return.
        pos = btc.index.get_loc(pd.Timestamp(start))
        base = btc.iloc[pos - 1] if pos else btc.loc[start]
        result['btc_buy_hold'][name] = dict(start=start, end=stop, initial_close_date=day(btc.index[max(0, pos - 1)]),
                                           total_return=float(btc.loc[stop] / base - 1))
    for name, meta in provenance.items():
        if hashlib.sha256(Path(meta['path']).read_bytes()).hexdigest() != meta['sha256']:
            raise ValueError(f'frozen input changed during study: {name}')
    result['elapsed_seconds'] = time.monotonic() - started
    return result


def day(value):
    return pd.Timestamp(value).strftime('%Y-%m-%d')


def fmt(value, digits=4):
    return f'{value:.{digits}f}' if value is not None and np.isfinite(value) else 'NA'


def pct(value):
    return fmt(value * 100, 3) + '%' if value is not None else 'NA'


def table(headers, rows):
    return '\n'.join(['| ' + ' | '.join(headers) + ' |', '| ' + ' | '.join(['---'] * len(headers)) + ' |'] +
                     ['| ' + ' | '.join(str(x).replace('|', '/').replace('\n', ' ') for x in row) + ' |' for row in rows])


def a_table(result):
    return table(['h', 'n_days', 'IC', 'ICIR', 'NW t', '前/后半 IC', 'Q5−Q1 年化净收益', 'Sharpe',
                  '换手', '含资金费年化', 'A1', 'A2'],
                 [[a['h'], a['n_days'], fmt(a['ic']), fmt(a['icir']), fmt(a['nw_t']),
                   '/'.join(fmt(x) for x in a['ic_halves']), pct(a['ls_annual']), fmt(a['ls_sharpe']),
                   fmt(a['turnover']), pct(a['funding']['ls_ret']), flag(a['A1']), flag(a['A2'])] for a in result['A']])


def b_table(result):
    if not result['B']:
        return table(['h', 'B 主设定', 'B1–B6', '四变体', '安慰剂分位', '向量化/BT差异'],
                     [[a['h'], 'A2未通过，未运行' if not a['A2'] else 'A1未通过，未运行',
                       '未评估', '未运行', 'NA', 'NA'] for a in result['A'] if a['h'] in (3, 7)])
    rows = []
    for b in result['B']:
        m = b['main']['metrics']
        rows.append([b['h'], pct(m['total_return']), pct(m['annual_return']), fmt(m['sharpe']),
                     pct(m['max_drawdown']), fmt(m['turnover']), fmt(m['commission_paid']),
                     fmt(m['funding_paid']), m['trades'], '/'.join(pct(v) for v in m['half_returns'])])
    return table(['h', '净总收益', '年化', '日 Sharpe×√365', '最大回撤', '换手x', '手续费',
                  '资金费净付出', '成交笔数', '前/后半净收益'], rows)


def flag(value):
    return '通过' if value else '未通过'


def render_report(result):
    d = result['design']
    lines = ['# 冻结 crypto ensemble 前向校验与市场中性组合', '',
             f"前向信号区间 {d['F']}～{d['end']}；初始资金 {q.CAPITAL:g}。"
             '研究仅离线读取本地数据；未重选成分、未翻转方向、未调整预声明参数/门槛、未接入交易代码。', '',
             '## 冻结输入', '']
    for name, meta in result['provenance'].items():
        lines.append(f"- {name}.json：sha256 `{meta['sha256']}`；generated_at：{meta['generated_at'] or '缺失（原文件无此字段）'}。")
    lines.append('- 冻结成分数：' + '、'.join(f"h={h}: {len(factors)}" for h, factors in result['factors'].items()) + '。')
    lines += ['', f"面板：{json.dumps(result['panel_meta'], ensure_ascii=False)}", '',
              'ensemble 成分及 zoo 表达式/direction 完整保存于 report.json；三个 h 的 holdout 起点由 make_splits 取得并断言均为 2026-05-04。', '',
              'holdout_spent 只报告，不修改：' + json.dumps(result['spent'], ensure_ascii=False), '',
              '## A：信号层前向结果', '', a_table(result), '',
              'IC 只在 ≥F 且 excess 存在有限值的信号行计算；n_days 是有效日 IC 数。'
              'NW lag=h；ICIR 不年化；Q5−Q1 为冻结评估器的双腿各 1 倍口径，年化=均值×365/h。'
              '含资金费调用 holding_funding 与 funding_adjusted_ls；资金费选中腿缺失会令 A2 不通过。'
              '两半 IC 按完整前向日期区间中点划分，末尾缺少 t+1+h 价格的标签不计入。', '',
              'h=1 因事先已知 train/val 扣费多空均负，预先排除 B。', '',
              '## train/val 原文对照（仅报告，来自 ensemble.json）', '',
              '```json', json.dumps(result['frozen_train_val'], ensure_ascii=False, indent=2), '```', '',
              '## B：Backtrader 主设定', '', b_table(result), '',
              'B 仅运行 A1/A2 同时通过的 h∈{3,7}。分位数量=floor(n×fraction)、至少1；'
              'n=0/1 时空仓以避免多空重叠。稳定排序按面板列顺序破同分。主目标多头+0.5、空头−0.5；'
              '信号从 F 起每 h 行，区间保持目标，仅目标变化才在下一开盘交易。持有期间实际敞口随价格漂移。'
              '最后信号持有到最后收盘，不伪造末日平仓或扣平仓费。', '',
              '佣金 5bp、滑点 3bp；真实原生订单/持仓/现金。资金费每日按隔夜实际持仓×当日开盘×日均费率×3，'
              '多头付正费、空头收正费；资金费净付出为负代表净收入。缺费率按引擎规则记0并报告覆盖。'
              '年化=(末值/CAPITAL)^(365/日数)−1；日收益含 F 零收益行，Sharpe=简单日收益均值/样本标准差×√365。'
              '最大回撤包含初始 CAPITAL；换手=成交绝对名义/成交前权益的累计和。', '',
              '引擎缺价规则：只向前填充估值，缺 open 不交易且保留原股数，剩余可交易腿按原规则缩放；'
              '不会自动延后重试，下一次目标变化才有机会调整。终端缺价持仓不强制平仓。', '']
    if not result['B']:
        lines += ['无符合预声明条件的 B 候选；B 门槛、变体与安慰剂均未运行。', '']
    for b in result['B']:
        h, m = b['h'], b['main']['metrics']
        lines += [f'## h={h}：门槛、稳健性与安慰剂', '',
                  table(['门槛', '观测值', '结果'], [
                      ['B1', f"总收益 {pct(m['total_return'])}；Sharpe {fmt(m['sharpe'])}", flag(b['gates']['B1'])],
                      ['B2', f"P(均值>0)={pct(b['bootstrap']['probability_positive'])}", flag(b['gates']['B2'])],
                      ['B3', '/'.join(pct(v) for v in m['half_returns']), flag(b['gates']['B3'])],
                      ['B4', f"BTC OLS beta={fmt(b['btc_ols']['beta'])}；n={b['btc_ols']['n']}", flag(b['gates']['B4'])],
                      ['B5', f"{sum(v['metrics']['total_return'] > 0 for v in b['variants'])}/4 变体正收益", flag(b['gates']['B5'])],
                      ['B6', f"安慰剂分位={pct(b['replica']['percentile'])}", flag(b['gates']['B6'])]]), '',
                  table(['分位', '调仓周期', '净总收益', 'Sharpe', '最大回撤', '成交笔数'],
                        [[pct(v['fraction']), v['period'], pct(v['metrics']['total_return']),
                          fmt(v['metrics']['sharpe']), pct(v['metrics']['max_drawdown']), v['metrics']['trades']]
                         for v in b['variants']]), '',
                  f"Bootstrap：2000 次、seed={SEED}，7 日不重叠完整块，{b['bootstrap']['n_blocks']} 块；"
                  f"末尾不足7日的 {b['bootstrap']['omitted_tail_days']} 日只在 bootstrap 中舍弃。"
                  f"B3 两半区间：{m['half_ranges']}。OLS 含截距，BTC 使用面板 close 的简单日收益。", '',
                  f"500 次安慰剂 seed={SEED}；在每个信号日合格币内部打乱分数，固定资格、调仓日、分位和成本。"
                  '分位=500 次总收益中 ≤主设定向量化总收益的比例。', '',
                  f"向量化主设定净收益 {pct(b['replica']['main_total_return'])}；Backtrader {pct(m['total_return'])}；"
                  f"差（向量化−BT）{fmt(b['comparison']['total_return_difference'] * 100, 4)} 个百分点；"
                  f"日收益相关 {fmt(b['comparison']['daily_return_correlation'], 6)}。", '',
                  b['comparison']['explanation'], '',
                  f"holding_funding 与向量化逐日固定名义费率累计最大误差：{b['replica']['holding_funding_check_error']:.3g}；"
                  f"缺费率面板格 {b['replica']['missing_funding_cells']}；"
                  f"全部复刻路径缺开盘执行事件 {b['replica']['missing_execution_path_events']}。", '',
                  '执行/缺价核对：', '',
                  table(['设定', '成交', '非调仓成交', '延后成交', '缺open意图', '持仓缺价日', '资金费覆盖/持仓日'],
                        [[f"{pct(v['fraction'])}/{v['period']}日", v['execution_audit']['fills'],
                          v['execution_audit']['unexpected_fills'], v['execution_audit']['delayed_fills'],
                          v['execution_audit']['missing_open_intents'], v['execution_audit']['held_missing_price_days'],
                          f"{v['metrics']['funding_covered']}/{v['metrics']['funding_observations']}"] for v in b['variants']]), '',
                  '末尾缺价持仓（主设定）：' + json.dumps(b['main']['execution_audit']['terminal_stale_positions'], ensure_ascii=False), '',
                  f"val 同规则 Backtrader 对照（有选择偏差，不作门槛）：收益 {pct(b['validation']['metrics']['total_return'])}，"
                  f"Sharpe {fmt(b['validation']['metrics']['sharpe'])}。", '']
    lines += ['## BTC 持有对照（仅报告，不作门槛）', '',
              table(['区间', '收益', '起始收盘日'], [[name, pct(x['total_return']), x['initial_close_date']]
                                                   for name, x in result['btc_buy_hold'].items()]), '',
              'BTC 对照为区间前一日收盘买入至区间末收盘的无费用现货持有收益，与 OLS 日收益口径一致。', '',
              '## 结论', '']
    for c in result['conclusions']:
        failed = '；未过门槛：' + ', '.join(c['failed']) if c.get('failed') else ''
        lines.append(f"- h={c['h']}：{c['message']}{failed}。")
    lines += ['', '限制：当前存活币宇宙存在幸存者偏差；仅冻结当前 ensemble，不代表历史时点已部署该成分。'
              '前向标签未参与本研究选择；日均资金费×3 无法复原真实结算时点/频次。'
              'Backtrader 使用虚拟抵押现金支持原生空头，策略权益扣除虚拟现金；未模拟强平、借券可得性、'
              '最小下单量、市场冲击与容量。向量化含费与 Backtrader 资金费时点/名义不同，须结合差异一起解读。'
              '本次前向区间在本报告中已被读取，不应再次当作新的独立未见样本。', '',
              f"运行耗时 {result['elapsed_seconds']:.3f} 秒。完整数值、预声明和冻结成分见 report.json。"
              + ('B 每日净值、门槛及500次安慰剂也保存在 JSON。' if result['B'] else
                 '本次 B 未运行，JSON 不包含虚构的净值、门槛判定或安慰剂。'), '']
    return '\n'.join(lines)


def json_safe(value):
    if isinstance(value, dict):
        return {str(k): json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(v) for v in value]
    if isinstance(value, (bool, np.bool_)):
        return bool(value)
    if isinstance(value, np.integer):
        return int(value)
    if isinstance(value, (float, np.floating)):
        return float(value) if math.isfinite(value) else None
    return value


def validate_outputs(root, outputs):
    root = Path(root).resolve()
    resolved = [Path(p).resolve() for p in outputs]
    if len(set(resolved)) != len(resolved):
        raise ValueError('Markdown and JSON paths must differ')
    for path in resolved:
        if path == root or root in path.parents:
            raise ValueError(f'output must not be inside read-only --root: {path}')
    return resolved


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--root', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--json', required=True, type=Path, dest='json_path')
    args = parser.parse_args(argv)
    try:
        out, json_path = validate_outputs(args.root, [args.out, args.json_path])
    except ValueError as error:
        parser.error(str(error))
    result = run_study(args.root, progress=lambda msg: print(msg, file=sys.stderr, flush=True))
    markdown = render_report(result)
    payload = json.dumps(json_safe(result), ensure_ascii=False, indent=2, allow_nan=False)
    for path in (out, json_path):
        path.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(markdown, encoding='utf-8')
    json_path.write_text(payload + '\n', encoding='utf-8')
    print(a_table(result))
    print(b_table(result))
    for c in result['conclusions']:
        print(f"h={c['h']}：{c['message']}" + (f"；未过 {','.join(c['failed'])}" if c.get('failed') else ''))
    print(f"报告：{out}\nJSON：{json_path}\n运行耗时：{result['elapsed_seconds']:.3f}s")
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
