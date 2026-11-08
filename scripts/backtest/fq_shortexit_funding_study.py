"""Frozen fq_crypto S1 short giveback and funding-exclusion study.

python -m scripts.backtest.fq_shortexit_funding_study --out /tmp/fqsef
Inputs are read-only. Portfolio execution and accounting use Backtrader.
"""
from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from concurrent.futures import ProcessPoolExecutor, as_completed
import datetime as dt
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

from scripts.factor_mining import panel, quant_targets, store
from scripts.factor_mining.ensemble import ensemble_scores
from . import evolution_ledger
from .fq_stop_tp_study import simple_returns, sharpe
from .incremental_filter import weekly_statistics, z_threshold
from .quantjourney import CAPITAL, PerNameExitPolicy, RebalancePolicy, run_weights

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-08-fq-crypto-shortexit-funding-prereg.md'
PREREG_SHA256 = 'a0d26ba5834d0f6ba98724398e5a5dce718a3670017f1aff0c87f776ce019499'
DATA = Path('/Users/zhangyun/workspace/nofx/data')
SNAPSHOT = DATA / 'factor_mining/fq_exit_funding/snapshot_20261108'
KLINES = DATA / 'volrix/klines'
LEDGER = DATA / 'evolution/ledger.jsonl'
SNAPSHOT_SHA256 = '02de1af2f12421e123739256732071a7632532b33e9a5985522c2965dc59c316'
FILE_HASHES = {
    'zoo.json': '1e3c46c53b6509402feecc1607b35578f6eb43ad560f9af7b8ce65bc4c930989',
    'ensemble.json': '46b79ccd7a3bd942abba62ddc6c771df02649032b00ea45ecae320c89a4919fb',
}
PRIMARY = ['f_0ce8b2ee']
SECONDARY = ['f_0ce8b2ee', 'f_eda12cac']
SEED = 20261108
S20 = PerNameExitPolicy(long_stop=.2, short_stop=.2)
GIVEBACK = PerNameExitPolicy(long_stop=.2, short_stop=.2, short_giveback=(.20, .50))
NOTES = [
    '被持有币日校验采用 B 的非零目标，包含首日信号与止损后等待期，两个区间合并计算比例。',
    '小时缺口当天沿用日线固定止损；不启动或移动回吐止损，保留此前小时路径状态。',
    '保持原引擎时序，首日收盘信号次日开盘执行；两个区间独立空仓起步。',
    '资金费多空归因按原生成交逐日恢复前一日收盘数量，乘当日开盘与实际 rate×3；'
    '每段合计与 broker.funding_paid 核对。缺价采用引擎相同的前填估值。',
    '提前离场代价=(成交价−下次实际目标阶梯变化日收盘价)/成交价；'
    '变化日为信号日，若段内无下一次变化则记缺失，不用段末替代。',
    'F 描述项仅使用有完整 t+3 收盘及三天有限 funding 的样本；缺失不按零补齐。'
    '各币各调仓期作为观测，按信号日 UTC 自然周聚类，未作权重加权。',
    '周聚类 SE 为零时，均值为正/负的 z 为正/负无穷，均值为零时 z=0；不足三周时 z 缺失。',
    '相同输出目录已有 hourly_sha256 时必须匹配，跨目录重跑通过结果中的哈希核对。',
    'daily_exposure 在使用 exit_policy 的路径返回，保留未使用退出策略时的原结果整包哈希。'
    '净值非正时暴露记零，沿用现有暴露统计口径。',
    '两组任一 invalid 时整次不写账本；账本实际 K 由 append_event 计算，另记预注册 K=7/8，判定阈值固定 K=8。',
    '只修改指定研究文件，研究日志交由正式运行者登记。冒烟安慰剂数量不参与正式账本。',
]


def verify_prereg():
    raw = PREREG.read_bytes()
    if hashlib.sha256(raw).hexdigest() != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run/output')
    return raw


def verify_snapshot(snapshot):
    base = Path(snapshot) / 'crypto'
    paths = sorted((p for p in base.rglob('*') if p.is_file()),
                   key=lambda p: ('./' + p.relative_to(base).as_posix()).encode('utf-8'))
    digest = hashlib.sha256()
    hashes = {}
    for path in paths:
        raw = path.read_bytes()
        digest.update(raw)
        if path.parent == base and path.name in FILE_HASHES:
            hashes[path.name] = hashlib.sha256(raw).hexdigest()
    if len(paths) != 303 or digest.hexdigest() != SNAPSHOT_SHA256 or hashes != FILE_HASHES:
        raise ValueError(f'Frozen snapshot mismatch: files={len(paths)}, sha256={digest.hexdigest()}, files_sha256={hashes}')
    return dict(snapshot_sha256=digest.hexdigest(), snapshot_files=len(paths), files_sha256=hashes)


def load_hourly(symbols, klines_dir):
    """Hash the original JSON row values, with symbols and time rows ordered."""
    start = pd.Timestamp('2025-05-01').value // 1_000_000
    end = pd.Timestamp('2026-11-07').value // 1_000_000
    selected, hourly = {}, {}
    missing = [str(Path(klines_dir) / f'{s}_1h.json') for s in sorted(symbols)
               if not (Path(klines_dir) / f'{s}_1h.json').is_file()]
    if missing:
        raise FileNotFoundError('Missing hourly files: ' + ', '.join(missing))
    for symbol in sorted(symbols):
        path = Path(klines_dir) / f'{symbol}_1h.json'
        rows = json.loads(path.read_text(encoding='utf-8'))
        if not isinstance(rows, list) or any(not isinstance(r, list) or len(r) != 7 for r in rows):
            raise ValueError(f'{path}: expected seven-column hourly row arrays')
        rows = [r for r in rows if start <= r[0] < end]
        times = pd.DatetimeIndex(pd.to_datetime([r[0] for r in rows], unit='ms'))
        if (not times.is_unique or not times.is_monotonic_increasing
                or not times.equals(times.floor('h'))):
            raise ValueError(f'{path}: timestamps must be unique ascending UTC hour starts')
        values = np.asarray([r[1:5] for r in rows], dtype=float).reshape(-1, 4)
        if (not np.isfinite(values).all() or (values <= 0).any()
                or (values[:, 1] < values.max(axis=1)).any()
                or (values[:, 2] > values.min(axis=1)).any()):
            raise ValueError(f'{path}: invalid hourly OHLC')
        selected[symbol] = rows
        hourly[symbol] = pd.DataFrame(values, index=times, columns=['open', 'high', 'low', 'close'])
    raw = json.dumps(selected, sort_keys=True, separators=(',', ':')).encode()
    return hourly, hashlib.sha256(raw).hexdigest()


def validate_hourly(panels, baseline_targets, hourly):
    held = complete = matched = missing = 0
    failures = []
    for name, targets in baseline_targets.items():
        for symbol in targets:
            for date in targets.index[targets[symbol].ne(0)]:
                held += 1
                frame = hourly[symbol]
                bars = frame.loc[(frame.index >= date) & (frame.index < date + dt.timedelta(days=1))]
                if len(bars) != 24:
                    missing += 1
                    failures.append(dict(interval=name, symbol=symbol, date=str(date.date()), hours=len(bars)))
                    continue
                complete += 1
                daily = dict(open=float(bars.open.iloc[0]), high=float(bars.high.max()),
                             low=float(bars.low.min()), close=float(bars.close.iloc[-1]))
                reference = float(panels.fields['close'].loc[date, symbol])
                error = abs(daily['close'] / reference - 1) if np.isfinite(reference) and reference > 0 else np.inf
                matched += error <= .002
                if error > .002:
                    failures.append(dict(interval=name, symbol=symbol, date=str(date.date()),
                                         hourly_daily=daily, panel_close=reference, close_relative_error=error))
    coverage = missing / held if held else 1.
    agreement = matched / complete if complete else 0.
    return dict(valid=held > 0 and coverage <= .02 and agreement >= .99,
                held_definition='B nonzero targets', held_symbol_days=held, complete_symbol_days=complete,
                missing_symbol_days=missing, missing_fraction=coverage,
                close_match_fraction=agreement, failures=failures)


def funding_exclusions(panels, date, shorts):
    values = panels.fields['funding'].reindex(
        index=pd.date_range(date - dt.timedelta(days=6), date), columns=shorts)
    values = values.where(np.isfinite(values))
    # fsum avoids a repeated exact threshold rounding below itself in pandas.
    means = {s: math.fsum(values[s].dropna()) / values[s].count() if values[s].count() else np.nan
             for s in shorts}
    excluded = [s for s in shorts if values[s].count() >= 5 and means[s] < -.0001]
    return excluded, {s: float(means[s]) for s in shorts}


def short_volatility(panels, date):
    close = panels.fields['close'].loc[:date].reindex(columns=panels.mask.columns)
    close = close.where(np.isfinite(close) & close.gt(0))
    returns = close.tail(31).pct_change(fill_method=None)
    return returns.rolling(30, min_periods=30).std(ddof=1).iloc[-1]


def reweight_shorts(weights, vol, excluded):
    shorts = [s for s, w in weights.items() if w < 0 and s not in excluded]
    result = {s: w for s, w in weights.items() if w > 0}
    if shorts:
        sleeve = -quant_targets._capped_invvol(vol.loc[shorts], .5, .1)
        result.update({s: round(float(w), 6) for s, w in sleeve.items() if round(float(w), 6)})
    return result


def funding_targets(panels, scores, factors, dates):
    """Mirror _historical_targets, including rounding, clock and non-ok carry."""
    targets = pd.DataFrame(np.nan, index=dates, columns=scores.columns)
    records = []
    for date in dates[::3]:
        row = scores.loc[date].where(panels.mask.loc[date])
        row = row[np.isfinite(row)]
        pct = row.rank(pct=True)
        entry = dict(factor_ids=[f['id'] for f in factors], scores={
            str(s): dict(z=round(float(z), 4), pct=round(float(pct[s]), 4)) for s, z in row.items()})
        snapshot = dict(as_of=date.strftime('%Y-%m-%d'), run_id='frozen-h3-study', horizons={'3': entry})
        target = quant_targets.build_targets('crypto', snapshot, panels, now=date + dt.timedelta(days=1))
        record = dict(date=date.strftime('%Y-%m-%d'), status=target['status'],
                      weights=target['weights'], existing_excluded=target['excluded'],
                      shorts=[], excluded=[], funding_mean={}, vol={})
        if target['status'] == 'ok':
            weights = target['weights']
            shorts = [s for s, w in weights.items() if w < 0 and s not in target['excluded']]
            vol = short_volatility(panels, date)
            rebuilt = reweight_shorts(weights, vol, [])
            for symbol in shorts:
                if abs(rebuilt.get(symbol, 0.) - weights[symbol]) > 1e-6:
                    raise AssertionError(f'Inverse-vol parity failed: {date} {symbol}')
            excluded, means = funding_exclusions(panels, date, shorts)
            filtered = reweight_shorts(weights, vol, excluded)
            targets.loc[date] = pd.Series(filtered, dtype=float).reindex(scores.columns, fill_value=0.)
            record.update(shorts=shorts, excluded=excluded, funding_mean=means,
                          vol={s: float(vol[s]) for s in shorts}, weights_f=filtered,
                          exclusion_reason='short_funding_lt_neg1bp',
                          all_short_excluded=bool(shorts) and len(shorts) == len(excluded))
        records.append(record)
    return targets.ffill().fillna(0.), records


def placebo_targets(template, records, rng):
    targets = template.copy() * np.nan
    exclusions = []
    for record in records:
        if record['status'] != 'ok':
            exclusions.append([])
            continue
        shorts, count = record['shorts'], len(record['excluded'])
        excluded = list(rng.choice(shorts, size=count, replace=False))
        weights = reweight_shorts(record['weights'], pd.Series(record['vol']), excluded)
        targets.loc[pd.Timestamp(record['date'])] = pd.Series(weights, dtype=float).reindex(targets.columns, fill_value=0.)
        exclusions.append(excluded)
    return targets.ffill().fillna(0.), exclusions


def verdict_g(stats):
    if not stats['hourly_valid']:
        return 'invalid'
    positive = stats['val_delta'] > 0 and stats['forward_delta'] > 0
    z = stats['z']
    if z is not None and z >= 1 and positive:
        if stats['bx_delta'] <= 0:
            return 'exposure_only'
        return 'strong' if z >= z_threshold(8) else 'screen_pass'
    return 'fail'


def verdict_f(stats):
    if not stats['hourly_valid'] or stats['excluded_count'] == 0:
        return 'invalid'
    positive = stats['val_delta'] > 0 and stats['forward_delta'] > 0
    z, percentile = stats['z'], stats['placebo_pct']
    if z is not None and z >= 1 and positive and percentile is not None:
        if percentile < .90:
            return 'fewer_trades'
        return 'strong' if z >= z_threshold(8) else 'screen_pass'
    return 'fail'


def aligned_delta(candidate, baseline):
    a, b = simple_returns(candidate), simple_returns(baseline)
    if not a.index.equals(b.index):
        raise ValueError('Daily return dates differ')
    return a - b


def comparison(intervals, candidate, baseline='B'):
    halves = {name: aligned_delta(interval['variants'][candidate], interval['variants'][baseline])
              for name, interval in intervals.items()}
    delta = pd.concat(list(halves.values()))
    weekly = weekly_statistics([(pd.Timestamp(day).to_pydatetime().replace(tzinfo=dt.timezone.utc), float(v))
                                for day, v in delta.items()])
    avg, se = weekly['avg_r'], weekly['se']
    z = None
    if se is not None:
        if se > 0:
            z = avg / se
        elif avg:
            z = math.copysign(math.inf, avg)
        else:
            z = 0.
    return dict(weekly, mean_delta=float(delta.mean()), z=z, z_K=z_threshold(8),
                val_delta=float(halves['val'].mean()), forward_delta=float(halves['forward'].mean()),
                daily_delta=delta.to_dict())


def split_funding(run, frames, rates):
    """Attribute native carried quantities before each day's opening trades."""
    quantities = defaultdict(float)
    by_day = defaultdict(list)
    for fill in run['fills']:
        by_day[fill['date'][:10]].append(fill)
    paid = dict(long=0., short=0.)
    dates = pd.to_datetime(list(run['daily_equity']))
    opens = {s: f.reindex(dates)['open'].ffill().fillna(1.) for s, f in frames.items()}
    for date in dates:
        for symbol, quantity in quantities.items():
            cost = quantity * opens[symbol].loc[date] * rates[symbol].get(date.date(), 0.) * 3
            if quantity:
                paid['long' if quantity > 0 else 'short'] += cost
        for fill in by_day[str(date.date())]:
            quantities[fill['symbol']] += fill['size']
    if not np.isclose(sum(paid.values()), run['metrics']['funding_paid'], rtol=1e-9, atol=1e-8):
        raise AssertionError('Long/short funding attribution differs from native CarryBroker')
    return paid


def execute(panels, targets, hourly, *, giveback=False, daily=False, describe=True):
    frames, rates, targets = quant_targets._frames_rates(panels, targets)
    run = run_weights(frames, targets, market='crypto', commission=.0005, slippage=.0003,
                      funding=rates, policy=RebalancePolicy(None, rebalance_on_signal_change=True),
                      exit_policy=GIVEBACK if giveback else S20,
                      exit_path=None if daily else {s: hourly[s] for s in frames}, keep_fills=describe)
    if describe:
        returns = simple_returns(run)
        metrics = run['metrics']
        metrics['sharpe_log'], metrics['sharpe'] = metrics['sharpe'], float(sharpe(returns))
        metrics['worst_3d_return_pct'] = float(((1 + returns).rolling(3).apply(np.prod, raw=True) - 1).min() * 100)
        metrics['funding_by_side'] = split_funding(run, frames, rates)
        run['daily_returns'] = returns.to_dict()
    return run


def distribution(values):
    values = np.asarray(values, dtype=float)
    return dict(n=len(values), mean=float(values.mean()) if len(values) else None,
                quantiles={str(q): float(np.quantile(values, q)) if len(values) else None
                           for q in (0., .05, .25, .5, .75, .95, 1.)})


def giveback_description(run, targets, panels):
    changed = targets.ne(targets.shift()).any(axis=1)
    changes = targets.index[changed]
    events = []
    for original in run['exit_events']:
        if original['kind'] != 'giveback_stop':
            continue
        event = dict(original, g_lock=(original['E'] - original['price']) / original['E'])
        later = changes[changes >= pd.Timestamp(original['date'])]
        # A signal at this exit day's close is the next change after its open.
        next_date = later[0] if len(later) else None
        close = panels.fields['close'].loc[next_date, original['symbol']] if next_date is not None else np.nan
        event.update(next_target_change=str(next_date.date()) if next_date is not None else None,
                     post_exit_decline=(original['price'] - float(close)) / original['price']
                     if np.isfinite(close) else None)
        events.append(event)
    return dict(events=events, g_lock=distribution([e['g_lock'] for e in events]),
                post_exit_decline=distribution([e['post_exit_decline'] for e in events if e['post_exit_decline'] is not None]),
                no_next_change=sum(e['next_target_change'] is None for e in events))


def exclusion_description(panels, records):
    samples = {'excluded': [], 'retained': []}
    details, missing = [], 0
    for record in records:
        date = pd.Timestamp(record['date'])
        end = date + dt.timedelta(days=3)
        for symbol in record['shorts']:
            prices = panels.fields['close'][symbol].reindex([date, end])
            funding = panels.fields['funding'][symbol].reindex(pd.date_range(date + dt.timedelta(days=1), end))
            if not np.isfinite(prices).all() or not prices.gt(0).all() or not np.isfinite(funding).all():
                missing += 1
                continue
            value = float(-(prices.iloc[-1] / prices.iloc[0] - 1) + 3 * funding.sum())
            group = 'excluded' if symbol in record['excluded'] else 'retained'
            samples[group].append((date.to_pydatetime().replace(tzinfo=dt.timezone.utc), value))
            details.append(dict(date=record['date'], symbol=symbol, group=group, short_return=value))
    return dict(count_distribution=dict(Counter(len(r['excluded']) for r in records)),
                all_short_excluded_dates=[r['date'] for r in records if r.get('all_short_excluded')],
                excluded_count=sum(len(r['excluded']) for r in records), missing_return_samples=missing,
                holding_period_returns={k: weekly_statistics(v) for k, v in samples.items()}, details=details)


_PLACEBO_CONTEXT = None


def _init_placebo(context):
    global _PLACEBO_CONTEXT
    _PLACEBO_CONTEXT = context


def _placebo_seed(seed):
    panels, hourly, segments = _PLACEBO_CONTEXT
    rng = np.random.default_rng(SEED + seed)
    differences = []
    for targets, records, baseline in segments:  # val, then forward, one RNG
        random_targets, _ = placebo_targets(targets, records, rng)
        run = execute(panels, random_targets, hourly, describe=False)
        differences.append(aligned_delta(run, baseline))
    return float(pd.concat(differences).mean())


def study(snapshot=SNAPSHOT, klines_dir=KLINES, *, workers=4, placebo=200,
          expected_hourly_sha256=None, progress=print):
    prereg_before = verify_prereg()
    inputs = verify_snapshot(snapshot)
    panels = panel.load_panels('crypto', snapshot)
    if panels.mask.index[-1] != pd.Timestamp('2026-11-06'):
        raise ValueError('Frozen panel must end on 2026-11-06')
    if 'funding' not in panels.fields:
        raise ValueError('Frozen panel has no funding field')
    hourly, hourly_sha256 = load_hourly(panels.mask.columns, klines_dir)
    if expected_hourly_sha256 is not None and hourly_sha256 != expected_hourly_sha256:
        raise ValueError('hourly_sha256 changed from previous output; refusing rerun')
    inputs['hourly_sha256'] = hourly_sha256
    zoo = json.loads((Path(snapshot) / 'crypto/zoo.json').read_bytes())
    by_id = {f['id']: f for f in zoo['factors'] if f['horizon'] == 3}
    factors = {group: [{k: by_id[i][k] for k in ('id', 'horizon', 'expr', 'direction')} for i in ids]
               for group, ids in (('primary', PRIMARY), ('secondary', SECONDARY))}
    scores = ensemble_scores(factors['primary'], panels)
    baselines, prepared = {}, {}
    for name, start, stop in (('val', '2025-05-04', '2026-04-30'), ('forward', '2026-05-04', '2026-11-06')):
        dates = panels.mask.index[(panels.mask.index >= pd.Timestamp(start)) & (panels.mask.index <= pd.Timestamp(stop))]
        if len(dates) < 3:
            raise ValueError(f'Insufficient dates for {name}')
        targets, _, signals = quant_targets._historical_targets(panels, scores, factors['primary'], dates)
        baselines[name] = targets
        prepared[name] = dict(dates=dates, signals=signals)
    validation = validate_hourly(panels, baselines, hourly)
    result = dict(version=1, market='crypto', horizon=3, engine='backtrader', smoke=placebo != 200,
                  prereg=dict(path=str(PREREG.relative_to(REPO)), sha256=PREREG_SHA256), inputs=inputs,
                  snapshot=str(Path(snapshot).resolve()), klines_dir=str(Path(klines_dir).resolve()),
                  factors=factors, panel=panels.meta, validation=validation, intervals={}, secondary={},
                  execution_notes=NOTES, placebo=dict(n=placebo, seed=SEED, means=[]),
                  initial_capital=CAPITAL, commission=.0005, slippage=.0003,
                  trials=dict(G=1, F=1, K_G=7, K_F=8, z_K=z_threshold(8)))
    if not validation['valid']:
        result['verdicts'] = dict(G='invalid', F='invalid')
        if verify_prereg() != prereg_before:
            raise ValueError('Preregistration changed during study')
        return result
    segments = []
    for name, prepared_interval in prepared.items():
        dates = prepared_interval['dates']
        targets = baselines[name]
        f_targets, records = funding_targets(panels, scores, factors['primary'], dates)
        interval = dict(start=str(dates[0].date()), end=str(dates[-1].date()),
                        signals=prepared_interval['signals'], funding_signals=records, variants={})
        for variant, weights in (('B', targets), ('G', targets), ('F', f_targets)):
            interval['variants'][variant] = execute(panels, weights, hourly, giveback=variant == 'G')
            progress(f'{name} {variant} completed', flush=True)
        exposures = {v: np.mean([e['short'] for e in interval['variants'][v]['daily_exposure'].values()]) for v in ('B', 'G')}
        if exposures['B'] <= 0:
            raise ValueError(f'{name}: B has zero mean short exposure; rho undefined')
        rho = float(exposures['G'] / exposures['B'])
        interval['rho'] = rho
        interval['variants']['B_x'] = execute(panels, targets.where(targets >= 0, targets * rho), hourly)
        interval['variants']['B_daily'] = execute(panels, targets, hourly, daily=True)
        interval['giveback'] = giveback_description(interval['variants']['G'], targets, panels)
        interval['exclusions'] = exclusion_description(panels, records)
        result['intervals'][name] = interval
        baseline = dict(daily_equity=interval['variants']['B']['daily_equity'])
        segments.append((targets, records, baseline))
    stats_g = comparison(result['intervals'], 'G')
    stats_g.update(hourly_valid=True, bx_delta=comparison(result['intervals'], 'G', 'B_x')['mean_delta'])
    stats_f = comparison(result['intervals'], 'F')
    stats_f.update(hourly_valid=True, excluded_count=sum(i['exclusions']['excluded_count'] for i in result['intervals'].values()))
    means = [None] * placebo
    if placebo:
        with ProcessPoolExecutor(max_workers=workers, initializer=_init_placebo,
                                 initargs=((panels, hourly, segments),)) as pool:
            jobs = {pool.submit(_placebo_seed, seed): seed for seed in range(placebo)}
            for completed, future in enumerate(as_completed(jobs), 1):
                means[jobs[future]] = future.result()
                if completed % 10 == 0 or completed == placebo:
                    progress(f'Placebo completed {completed}/{placebo}', flush=True)
    result['placebo']['means'] = means
    stats_f['placebo_pct'] = float(np.mean(np.asarray(means) < stats_f['mean_delta'])) if means else None
    result['statistics'] = dict(G=stats_g, F=stats_f, G_vs_B_x=comparison(result['intervals'], 'G', 'B_x'),
                                B_daily_vs_B_hourly=comparison(result['intervals'], 'B_daily'))
    result['verdicts'] = dict(G=verdict_g(stats_g), F=verdict_f(stats_f))
    secondary_scores = ensemble_scores(factors['secondary'], panels)
    for name, item in prepared.items():
        targets, _, _ = quant_targets._historical_targets(panels, secondary_scores, factors['secondary'], item['dates'])
        f_targets, records = funding_targets(panels, secondary_scores, factors['secondary'], item['dates'])
        result['secondary'][name] = dict(variants={
            v: execute(panels, t, hourly, giveback=v == 'G') for v, t in (('B', targets), ('G', targets), ('F', f_targets))},
            funding_signals=records)
    result['secondary_statistics'] = {v: comparison(result['secondary'], v) for v in ('G', 'F')}
    result['last_week_return_pct'] = {
        v: float(((1 + simple_returns(result['intervals']['forward']['variants'][v]).loc['2026-11-01':'2026-11-06']).prod() - 1) * 100)
        for v in ('B', 'G', 'F')}
    if verify_prereg() != prereg_before:
        raise ValueError('Preregistration changed during study; refusing output')
    return result


def report(result):
    lines = ['# fq_crypto 空头浮盈回吐与负资金费率排除 S1', '',
             f'预注册 SHA256 `{PREREG_SHA256}`。', '',
             f'G 判定 `{result["verdicts"]["G"]}`。F 判定 `{result["verdicts"]["F"]}`。'
             f'冒烟运行 `{result["smoke"]}`。', '',
             'val 与 forward 均已被其他研究读取，包含已观察的 11 月首周。这里只作 S1 离线筛选。', '',
             '## 小时线校验', '',
             '```json', json.dumps(store.finite({k: v for k, v in result['validation'].items() if k != 'failures'}),
                                  ensure_ascii=False, indent=2), '```', '']
    for label, intervals in (('主因子组', result['intervals']), ('次要因子组（描述）', result['secondary'])):
        lines += [f'## {label}', '',
                  '| 区间 | 组 | 收益 % | 年化 % | Sharpe | 最大回撤 % | 最差3日 % | 换手 | 手续费 | 滑点 | 多头资金费 | 空头资金费 | 固定止损 | 回吐退出 |',
                  '| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
        for name, interval in intervals.items():
            for variant, run in interval['variants'].items():
                m = run['metrics']
                values = [name, variant] + [_fmt(m[k]) for k in ('return_pct', 'cagr_pct', 'sharpe', 'maxdd_pct',
                                                                 'worst_3d_return_pct', 'turnover', 'commission_paid', 'slippage_paid')]
                values += [_fmt(m['funding_by_side'][side]) for side in ('long', 'short')]
                values += [str(m['exit_trigger_counts'].get(k, 0)) for k in ('stop_loss', 'giveback_stop')]
                lines.append('| ' + ' | '.join(values) + ' |')
        lines.append('')
    if 'statistics' in result:
        lines += ['## 配对日收益差', '',
                  '| 比较 | 拼接均值 | val 均值 | forward 均值 | 周聚类 SE | z |',
                  '| --- | ---: | ---: | ---: | ---: | ---: |']
        for name, stats in dict(result['statistics'], **{f'secondary_{k}': v for k, v in result['secondary_statistics'].items()}).items():
            lines.append('| ' + ' | '.join([name] + [_fmt(stats[k]) for k in ('mean_delta', 'val_delta', 'forward_delta', 'se', 'z')]) + ' |')
        lines += ['', f'z_K={z_threshold(8):.6f}；G−B_x 均值={_fmt(result["statistics"]["G"]["bx_delta"])}；'
                  f'F 安慰剂分位={_fmt(result["statistics"]["F"]["placebo_pct"])}。', '',
                  '## 回吐退出与排除描述', '',
                  '| 区间 | ρ | 回吐次数 | 锁盈均值 | 提前离场代价均值 | F 排除总数 | 全排除期数 |',
                  '| --- | ---: | ---: | ---: | ---: | ---: | ---: |']
        for name, interval in result['intervals'].items():
            g, f = interval['giveback'], interval['exclusions']
            lines.append('| ' + ' | '.join([name, _fmt(interval['rho']), str(g['g_lock']['n']),
                         _fmt(g['g_lock']['mean']), _fmt(g['post_exit_decline']['mean']),
                         str(f['excluded_count']), str(len(f['all_short_excluded_dates']))]) + ' |')
        lines += ['', '| 区间 | 空头样本 | n | 均值 | 95% CI |', '| --- | --- | ---: | ---: | --- |']
        for name, interval in result['intervals'].items():
            for group, stats in interval['exclusions']['holding_period_returns'].items():
                lines.append(f'| {name} | {group} | {stats["n"]} | {_fmt(stats["avg_r"])} | {stats["ci95"]} |')
            lines += ['', f'{name} 每期排除数分布 `{interval["exclusions"]["count_distribution"]}`。',
                      f'锁盈分位 `{interval["giveback"]["g_lock"]["quantiles"]}`。',
                      f'提前离场代价分位 `{interval["giveback"]["post_exit_decline"]["quantiles"]}`。', '']
        lines += ['', '## 11 月首周累计收益（描述）', '',
                  '| 组 | 2026-11-01 至 2026-11-06 收益 % |', '| --- | ---: |']
        lines += [f'| {v} | {_fmt(value)} |' for v, value in result['last_week_return_pct'].items()]
    lines += ['', '## 执行口径与补充选择', ''] + ['- ' + note for note in NOTES]
    lines += ['', '## 冻结输入', '', '```json', json.dumps(result['inputs'], ensure_ascii=False, indent=2), '```', '',
              '手续费与滑点已含净值，资金费正值为净付出。简单日收益含初始资金到首日，Sharpe 使用样本标准差与 √365。'
              '最差三日按相邻三日复合收益。小时回吐更新从下一小时生效，每次实际目标阶梯变化重置。', '',
              '逐笔退出、日净值和暴露、资金费归因、F 信号、描述样本与安慰剂均值均见 results.json。'
              '当前存活币池有幸存者偏差，小时缺失采用日线近似，未模拟强平、容量或冲击。', '']
    return '\n'.join(lines)


def _fmt(value):
    return 'NA' if value is None else f'{value:.8f}'


def validate_output(out, snapshot, klines_dir):
    out = Path(out).resolve()
    if any(out.is_relative_to(Path(root).resolve()) for root in (snapshot, klines_dir)):
        raise ValueError('--out must be outside snapshot and klines directories')
    return out


def write_ledger(result, path):
    if result['smoke'] or result['placebo']['n'] != 200 or 'invalid' in result['verdicts'].values():
        return []
    rows = []
    for variant, change_type, idea_id, planned_k in (
            ('G', 'C', 'fq-crypto-short-giveback-v1', 7),
            ('F', 'B', 'fq-crypto-short-funding-excl-v1', 8)):
        stats = result['statistics'][variant]
        def build(k, previous):
            return dict(track='fq_crypto', stage='S1', change_type=change_type, idea_id=idea_id,
                        baseline_version='f_0ce8b2ee@zoo1e3c46c5+S20h', verdict=result['verdicts'][variant],
                        delta=stats['mean_delta'], se=stats['se'], z=stats['z'],
                        dev_delta=stats['val_delta'], holdout_delta=stats['forward_delta'],
                        bx_delta=stats.get('bx_delta'), placebo_pct=stats.get('placebo_pct'),
                        z_K=z_threshold(8), preregistered_K=planned_k, input_hashes=result['inputs'],
                        prereg_sha256=PREREG_SHA256, trials=1)
        rows.append(evolution_ledger.append_event(path, 'fq_crypto', build))
    return rows


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--snapshot', type=Path, default=SNAPSHOT)
    parser.add_argument('--klines-dir', type=Path, default=KLINES)
    parser.add_argument('--workers', type=int, default=4)
    parser.add_argument('--placebo', type=int, default=200)
    parser.add_argument('--write-ledger', action='store_true')
    parser.add_argument('--ledger', type=Path, default=LEDGER)
    args = parser.parse_args(argv)
    try:
        out = validate_output(args.out, args.snapshot, args.klines_dir)
        if args.workers < 1 or args.placebo < 0:
            raise ValueError('--workers must be positive; --placebo must be nonnegative')
        if args.write_ledger and args.placebo != 200:
            raise ValueError('Smoke runs cannot write the ledger (--placebo must be 200)')
        if args.write_ledger:
            validate_output(args.ledger, args.snapshot, args.klines_dir)
        prereg_before = verify_prereg()
        previous = store.read_json(out / 'results.json', {})
        expected = previous.get('inputs', {}).get('hourly_sha256')
        result = study(args.snapshot, args.klines_dir, workers=args.workers, placebo=args.placebo,
                       expected_hourly_sha256=expected)
        if verify_prereg() != prereg_before:
            raise ValueError('Preregistration changed during study; refusing output')
    except (ValueError, OSError) as exc:
        parser.error(str(exc))
    out.mkdir(parents=True, exist_ok=True)
    store.write_json(out / 'results.json', result)
    (out / 'report.md').write_text(report(result), encoding='utf-8')
    if args.write_ledger:
        write_ledger(result, args.ledger)
    print(f'VERDICT G: {result["verdicts"]["G"]}')
    print(f'VERDICT F: {result["verdicts"]["F"]}')
    return 1 if 'invalid' in result['verdicts'].values() else 0


if __name__ == '__main__':
    raise SystemExit(main())
