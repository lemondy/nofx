"""Frozen fq_crypto S1 size-tier guardrails (M1 cap, M2 filter, M3 rebuild) study.

python -m scripts.backtest.fq_crypto_size_tier_study --out /tmp/fqcst
Inputs are read-only. Portfolio execution and accounting use Backtrader.
"""
from __future__ import annotations

import argparse
from collections import Counter
from concurrent.futures import ProcessPoolExecutor, as_completed
import datetime as dt
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

from scripts.factor_mining import panel, quant_targets, store, tiers
from scripts.factor_mining.ensemble import ensemble_scores
from . import evolution_ledger
from .fq_shortexit_funding_study import (
    DATA,
    KLINES,
    LEDGER,
    SNAPSHOT,
    aligned_delta,
    comparison,
    execute,
    load_hourly,
    validate_hourly,
    verify_snapshot,
)
from .fq_stop_tp_study import simple_returns
from .incremental_filter import z_threshold

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-15-fq-crypto-size-tier-s1-prereg.md'
PREREG_SHA256 = '59857028c109a589e8b717049d30adee6c0111a2ef9f5790e560a66cf5afa11c'
SEED = 20261115
PLANNED_K = 12
GO_CRYPTO_MEGA_ADV20 = 100_000_000.0
GO_ADV20_WINDOW = 20

PRIMARY_FACTORS = (
    {
        'id': 'f_d462040d',
        'horizon': 3,
        'expr': '(CSRank(EMA(Log(($high/$low)),40))-CSRank(IdxMax($close,20)))',
        'direction': -1,
    },
    {
        'id': 'f_25a5a0db',
        'horizon': 3,
        'expr': 'Corr(($vwap/Ref($close,1)),Log((($volume/Ref($volume,3))+1)),60)',
        'direction': -1,
    },
    {
        'id': 'f_880af0cb',
        'horizon': 3,
        'expr': '(CSRank(Mean((($high-$low)/$close),30))+CSRank(Slope($close,20)))',
        'direction': -1,
    },
    {
        'id': 'f_3afb2c9b',
        'horizon': 3,
        'expr': '(CSRank($premium)+CSRank(Mean((($high-$low)/$close),30)))',
        'direction': -1,
    },
    {
        'id': 'f_0ce8b2ee',
        'horizon': 3,
        'expr': '(CSRank(EMA(Std(($high/$low),10),30))-CSRank(EMA($funding,30)))',
        'direction': -1,
    },
    {
        'id': 'f_7b6ef396',
        'horizon': 3,
        'expr': 'Mean((CSRank(EMA(($high/$low),30))-CSRank((Quantile($open,10,0.9)/$vwap))),40)',
        'direction': -1,
    },
    {
        'id': 'f_a40b13e0',
        'horizon': 3,
        'expr': 'Mean((CSRank(Max(($high/$low),20))-CSRank((Quantile($open,7,0.9)/$close))),90)',
        'direction': -1,
    },
)
SECONDARY_FACTORS = (
    {
        'id': 'f_0ce8b2ee',
        'horizon': 3,
        'expr': '(CSRank(EMA(Std(($high/$low),10),30))-CSRank(EMA($funding,30)))',
        'direction': -1,
    },
)

ARMS = (
    ('M1', 'B', 'fq-crypto-max-mid-small-25-v1', 10),
    ('M2', 'B', 'fq-crypto-mega-large-filter-v1', 11),
    ('M3', 'E', 'fq-crypto-mega-large-rebuild-v1', 12),
)

NOTES = [
    '仅使用冻结快照 snapshot_20261108 与小时线 klines，主分析采用 2026-11-15 冻结 7 因子 prod ensemble；次要描述表采用单因子 f_0ce8b2ee。',
    'M1/M2 的分层判定严格复现 Go fqCalcADV20 口径：仅取 <= 信号日 t 的最近 20 根有效 close*volume 日线均值（>=20 根），阈值 $100M，零未来数据。',
    'M3 调用 quant_targets.build_targets(..., tier="mega_large")，采用 Python tiers.tier_mask（<= t 的 20 根 vwap*volume 均值 >= $100M），合格币 < 15 时沿用上期。',
    '每组各自配对等暴露对照 B_x(Arm)（按分段日均总暴露比 rho 缩放 B 权重），并附 200 次同数量/同规则随机安慰剂检验；两者均需通过才不判为降杠杆或随机选币错觉。',
    '所有执行、止损、资金费与净值均由 Backtrader + CarryBroker + 小时线 S20 路径原生计算。',
]


def verify_prereg():
    raw = PREREG.read_bytes()
    if hashlib.sha256(raw).hexdigest() != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run/output')
    return raw


def causal_go_tier_map(panels, date, symbols):
    """Exact causal mirror of Go fqCalcADV20 + fqClassifyTier using close*volume <= date."""
    close_df = panels.fields['close'].loc[:date]
    vol_df = panels.fields['volume'].loc[:date]
    out = {}
    for sym in symbols:
        if sym not in close_df.columns or sym not in vol_df.columns:
            continue
        c = close_df[sym]
        v = vol_df[sym]
        valid = np.isfinite(c) & (c > 0) & np.isfinite(v) & (v >= 0)
        dv = (c * v).loc[valid].tail(GO_ADV20_WINDOW)
        if len(dv) < GO_ADV20_WINDOW:
            continue
        adv = float(dv.mean())
        if not np.isfinite(adv) or adv <= 0:
            continue
        out[sym] = 'mega_large' if adv >= GO_CRYPTO_MEGA_ADV20 else 'mid_small'
    return out


def apply_go_guardrail(weights, tier_by_sym, *, size_tier='all', max_mid_small_pct=0.0):
    """Exact mirror of Go fqApplySizeTierGuardrails without lookahead."""
    out = dict(weights)
    changed = False
    if size_tier in ('mega_large', 'mid_small'):
        for sym in list(out):
            if tier_by_sym.get(sym) != size_tier:
                del out[sym]
                changed = True
    if max_mid_small_pct > 0:
        mid_gross = sum(abs(w) for sym, w in out.items() if tier_by_sym.get(sym) == 'mid_small')
        cap_gross = max_mid_small_pct / 100.0
        if mid_gross > cap_gross + 1e-12 and mid_gross > 0:
            scale = cap_gross / mid_gross
            for sym in list(out):
                if tier_by_sym.get(sym) == 'mid_small':
                    nw = math.trunc(out[sym] * scale * 1e6) / 1e6
                    if nw == 0.0:
                        del out[sym]
                    else:
                        out[sym] = nw
                    changed = True
    return out, changed


def build_subset_target(scores_row, eligible_subset, vol, ret7, history, rule):
    """Fast deterministic mirror of quant_targets.build_targets on an explicit symbol subset."""
    eligible = [s for s in eligible_subset if s in scores_row.index and np.isfinite(scores_row[s])
                and s in vol.index and np.isfinite(vol[s]) and vol[s] > 0]
    if len(eligible) < 15:
        return None
    sub_scores = scores_row.loc[eligible].sort_values(kind='stable')
    count = max(1, int(np.floor(len(sub_scores) * rule['quantile'])))
    longs, shorts = sub_scores.index[-count:], sub_scores.index[:count]
    r7 = ret7.reindex(sub_scores.index).where(lambda x: np.isfinite(x))
    threshold = r7.quantile(rule['short_exclude_ret7_quantile'])
    kept_shorts = [
        s for s in shorts
        if not (np.isfinite(r7[s]) and r7[s] >= threshold) and history[s] >= rule['min_listing_days']
    ]
    if not kept_shorts:
        return None
    weights = pd.concat([
        quant_targets._capped_invvol(vol.loc[longs], rule['long_gross'], rule['max_name_weight']),
        -quant_targets._capped_invvol(vol.loc[kept_shorts], rule['short_gross'], rule['max_name_weight']),
    ])
    return {str(s): round(float(w), 6) for s, w in weights.items() if round(float(w), 6)}


def prepare_interval_targets(panels, scores, factors, dates):
    """Build B, M1, M2, M3 target schedules and placebo metadata on one interval."""
    columns = scores.columns
    b_df = pd.DataFrame(np.nan, index=dates, columns=columns)
    m1_df = pd.DataFrame(np.nan, index=dates, columns=columns)
    m2_df = pd.DataFrame(np.nan, index=dates, columns=columns)
    m3_df = pd.DataFrame(np.nan, index=dates, columns=columns)

    mega_mask_all = tiers.tier_mask(panels, 'crypto', 'mega_large')
    rule = quant_targets._rule('crypto')
    records = []
    changed_counts = {'M1': 0, 'M2': 0, 'M3': 0}

    for date in dates[::3]:
        row = scores.loc[date].where(panels.mask.loc[date])
        row = row[np.isfinite(row)]
        pct = row.rank(pct=True)
        entry = dict(
            factor_ids=[f['id'] for f in factors],
            scores={str(s): dict(z=round(float(z), 4), pct=round(float(pct[s]), 4)) for s, z in row.items()},
        )
        snapshot = dict(as_of=date.strftime('%Y-%m-%d'), run_id='frozen-size-tier-study', horizons={'3': entry})
        base_target = quant_targets.build_targets('crypto', snapshot, panels, now=date + dt.timedelta(days=1))
        m3_target = quant_targets.build_targets(
            'crypto', snapshot, panels, now=date + dt.timedelta(days=1), tier='mega_large'
        )

        # Causal risk fields up to date for fast M3 random-subset placebo.
        close_sub = panels.fields['close'].loc[:date].reindex(columns=columns)
        close_sub = close_sub.where(np.isfinite(close_sub) & close_sub.gt(0))
        returns = close_sub.tail(rule['vol_window'] + 1).pct_change(fill_method=None)
        vol = returns.rolling(rule['vol_window'], min_periods=rule['vol_window']).std(ddof=1).iloc[-1]
        ret7 = close_sub.loc[date] / close_sub.reindex([date - dt.timedelta(days=7)]).iloc[0] - 1.0
        history = close_sub.notna().sum()
        score_series = pd.Series({s: v['z'] for s, v in entry['scores'].items()}, dtype=float).reindex(columns)
        core_eligible = list(
            columns[panels.mask.loc[date].fillna(False) & np.isfinite(score_series) & np.isfinite(vol) & vol.gt(0)]
        )
        mega_eligible = list(
            columns[mega_mask_all.loc[date].fillna(False) & np.isfinite(score_series) & np.isfinite(vol) & vol.gt(0)]
        )

        rec = dict(
            date=date.strftime('%Y-%m-%d'),
            b_status=base_target['status'],
            m3_status=m3_target['status'],
            b_weights=base_target['weights'],
            tier_by_sym={},
            mid_count=0,
            non_mega_count=0,
            core_eligible=core_eligible,
            mega_count=len(mega_eligible),
            score_series=score_series.dropna().to_dict(),
            vol=vol.dropna().to_dict(),
            ret7=ret7.dropna().to_dict(),
            history=history.to_dict(),
        )

        if base_target['status'] == 'ok':
            w_b = base_target['weights']
            b_df.loc[date] = pd.Series(w_b, dtype=float).reindex(columns, fill_value=0.0)
            tier_map = causal_go_tier_map(panels, date, list(w_b))
            w_m1, ch1 = apply_go_guardrail(w_b, tier_map, size_tier='all', max_mid_small_pct=25.0)
            w_m2, ch2 = apply_go_guardrail(w_b, tier_map, size_tier='mega_large', max_mid_small_pct=0.0)
            m1_df.loc[date] = pd.Series(w_m1, dtype=float).reindex(columns, fill_value=0.0)
            m2_df.loc[date] = pd.Series(w_m2, dtype=float).reindex(columns, fill_value=0.0)
            if ch1:
                changed_counts['M1'] += 1
            if ch2:
                changed_counts['M2'] += 1
            rec['tier_by_sym'] = tier_map
            rec['mid_count'] = sum(1 for s in w_b if tier_map.get(s) == 'mid_small')
            rec['non_mega_count'] = sum(1 for s in w_b if tier_map.get(s) != 'mega_large')

        if m3_target['status'] == 'ok':
            w_m3 = m3_target['weights']
            m3_df.loc[date] = pd.Series(w_m3, dtype=float).reindex(columns, fill_value=0.0)
            if base_target['status'] != 'ok' or w_m3 != base_target['weights']:
                changed_counts['M3'] += 1

        records.append(rec)

    return (
        {
            'B': b_df.ffill().fillna(0.0),
            'M1': m1_df.ffill().fillna(0.0),
            'M2': m2_df.ffill().fillna(0.0),
            'M3': m3_df.ffill().fillna(0.0),
        },
        records,
        changed_counts,
    )


def build_placebo_targets(dates, columns, records, arm, rng):
    """Build placebo weight schedule matching the selected arm's exact perturbation size."""
    df = pd.DataFrame(np.nan, index=dates, columns=columns)
    rule = quant_targets._rule('crypto')
    for rec in records:
        date = pd.Timestamp(rec['date'])
        if arm in ('M1', 'M2'):
            if rec['b_status'] != 'ok':
                continue
            w_b = rec['b_weights']
            syms = list(w_b)
            if arm == 'M1':
                k = min(rec['mid_count'], len(syms))
                chosen = set(rng.choice(syms, size=k, replace=False)) if k else set()
                pseudo_tiers = {s: ('mid_small' if s in chosen else 'mega_large') for s in syms}
                w_p, _ = apply_go_guardrail(w_b, pseudo_tiers, size_tier='all', max_mid_small_pct=25.0)
            else:
                k = min(rec['non_mega_count'], len(syms))
                chosen = set(rng.choice(syms, size=k, replace=False)) if k else set()
                pseudo_tiers = {s: ('mid_small' if s in chosen else 'mega_large') for s in syms}
                w_p, _ = apply_go_guardrail(w_b, pseudo_tiers, size_tier='mega_large', max_mid_small_pct=0.0)
            df.loc[date] = pd.Series(w_p, dtype=float).reindex(columns, fill_value=0.0)
        elif arm == 'M3':
            core = rec['core_eligible']
            k = min(rec['mega_count'], len(core))
            if k < 15:
                continue
            subset = list(rng.choice(core, size=k, replace=False))
            w_p = build_subset_target(
                pd.Series(rec['score_series'], dtype=float),
                subset,
                pd.Series(rec['vol'], dtype=float),
                pd.Series(rec['ret7'], dtype=float),
                pd.Series(rec['history'], dtype=float),
                rule,
            )
            if w_p is not None:
                df.loc[date] = pd.Series(w_p, dtype=float).reindex(columns, fill_value=0.0)
    return df.ffill().fillna(0.0)


def mean_daily_gross(run):
    exposures = run.get('daily_exposure') or {}
    if not exposures:
        return 0.0
    return float(np.mean([float(e['long']) + float(e['short']) for e in exposures.values()]))


def exposure_summary(run):
    exposures = run.get('daily_exposure') or {}
    if not exposures:
        return {'long': 0.0, 'short': 0.0, 'gross': 0.0}
    l_mean = float(np.mean([float(e['long']) for e in exposures.values()]))
    s_mean = float(np.mean([float(e['short']) for e in exposures.values()]))
    return {'long': l_mean, 'short': s_mean, 'gross': l_mean + s_mean}


def verdict_arm(stats):
    if not stats['hourly_valid'] or stats['changed_rebalances'] == 0:
        return 'invalid'
    positive = stats['val_delta'] > 0 and stats['forward_delta'] > 0
    z = stats['z']
    if z is None or z < 1.0 or not positive:
        return 'fail'
    if stats['bx_delta'] is None or stats['bx_delta'] <= 0:
        return 'exposure_only'
    if stats['placebo_pct'] is None or stats['placebo_pct'] < 0.90:
        return 'fewer_trades'
    return 'strong' if z >= z_threshold(PLANNED_K) else 'screen_pass'


_PLACEBO_CTX = None


def _init_placebo(ctx):
    global _PLACEBO_CTX
    _PLACEBO_CTX = ctx


def _placebo_job(task):
    arm, seed_idx = task
    panels, hourly, segments = _PLACEBO_CTX
    rng = np.random.default_rng(SEED + seed_idx)
    diffs = []
    for dates, columns, records, baseline in segments:
        p_targets = build_placebo_targets(dates, columns, records, arm, rng)
        run = execute(panels, p_targets, hourly, giveback=False, daily=False, describe=False)
        diffs.append(aligned_delta(run, baseline))
    return arm, seed_idx, float(pd.concat(diffs).mean())


def _run_factor_set(panels, hourly, factors, *, workers=4, placebo=200, run_controls=True, progress=print):
    scores = ensemble_scores(list(factors), panels)
    prepared, base_targets = {}, {}
    for name, start, stop in (('val', '2025-05-04', '2026-04-30'), ('forward', '2026-05-04', '2026-11-06')):
        dates = panels.mask.index[(panels.mask.index >= pd.Timestamp(start)) & (panels.mask.index <= pd.Timestamp(stop))]
        if len(dates) < 3:
            raise ValueError(f'Insufficient dates for {name}')
        targets_map, records, changed = prepare_interval_targets(panels, scores, factors, dates)
        prepared[name] = dict(dates=dates, targets=targets_map, records=records, changed=changed)
        base_targets[name] = targets_map['B']

    validation = validate_hourly(panels, base_targets, hourly)
    if not validation['valid']:
        return dict(validation=validation, intervals={}, statistics={}, verdicts={a: 'invalid' for a, *_ in ARMS})

    intervals = {}
    segments = []
    for name, item in prepared.items():
        interval = dict(
            start=str(item['dates'][0].date()),
            end=str(item['dates'][-1].date()),
            changed_rebalances=item['changed'],
            variants={},
            exposure={},
            rho={},
        )
        for variant in ('B', 'M1', 'M2', 'M3'):
            run = execute(panels, item['targets'][variant], hourly, giveback=False, daily=False, describe=True)
            interval['variants'][variant] = run
            interval['exposure'][variant] = exposure_summary(run)
            progress(f'{name} {variant} completed', flush=True)

        if run_controls:
            base_gross = mean_daily_gross(interval['variants']['B'])
            if base_gross <= 0:
                raise ValueError(f'{name}: B has zero mean gross exposure; rho undefined')
            for arm in ('M1', 'M2', 'M3'):
                rho = mean_daily_gross(interval['variants'][arm]) / base_gross
                interval['rho'][arm] = float(rho)
                bx_targets = item['targets']['B'] * rho
                bx_run = execute(panels, bx_targets, hourly, giveback=False, daily=False, describe=True)
                interval['variants'][f'B_x_{arm}'] = bx_run
                interval['exposure'][f'B_x_{arm}'] = exposure_summary(bx_run)
                progress(f'{name} B_x_{arm} completed (rho={rho:.4f})', flush=True)

        intervals[name] = interval
        segments.append((item['dates'], scores.columns, item['records'], dict(daily_equity=interval['variants']['B']['daily_equity'])))

    placebo_means = {a: [] for a in ('M1', 'M2', 'M3')}
    if run_controls and placebo > 0:
        tasks = [(arm, s) for arm in ('M1', 'M2', 'M3') for s in range(placebo)]
        raw_means = {a: [None] * placebo for a in ('M1', 'M2', 'M3')}
        with ProcessPoolExecutor(
            max_workers=workers, initializer=_init_placebo, initargs=((panels, hourly, segments),)
        ) as pool:
            jobs = {pool.submit(_placebo_job, t): t for t in tasks}
            for idx, future in enumerate(as_completed(jobs), 1):
                arm, s_idx, val = future.result()
                raw_means[arm][s_idx] = val
                if idx % 30 == 0 or idx == len(tasks):
                    progress(f'Placebo completed {idx}/{len(tasks)}', flush=True)
        placebo_means = raw_means

    statistics, verdicts = {}, {}
    total_changed = {
        arm: sum(prepared[name]['changed'][arm] for name in prepared)
        for arm in ('M1', 'M2', 'M3')
    }
    for arm, *_ in ARMS:
        st = comparison(intervals, arm, 'B')
        st['z_K'] = z_threshold(PLANNED_K)
        st['hourly_valid'] = True
        st['changed_rebalances'] = total_changed[arm]
        if run_controls:
            bx_st = comparison(intervals, arm, f'B_x_{arm}')
            st['bx_delta'] = bx_st['mean_delta']
            st['bx_z'] = bx_st['z']
            pm = placebo_means[arm]
            st['placebo_pct'] = float(np.mean(np.asarray(pm) < st['mean_delta'])) if pm else None
        else:
            st['bx_delta'] = None
            st['bx_z'] = None
            st['placebo_pct'] = None
        statistics[arm] = st
        verdicts[arm] = verdict_arm(st) if run_controls else 'descriptive'

    return dict(
        validation=validation,
        intervals=intervals,
        statistics=statistics,
        verdicts=verdicts,
        placebo_means=placebo_means,
    )


def study(snapshot=SNAPSHOT, klines_dir=KLINES, *, workers=4, placebo=200,
          expected_hourly_sha256=None, progress=print):
    prereg_before = verify_prereg()
    inputs = verify_snapshot(snapshot)
    panels = panel.load_panels('crypto', snapshot)
    if panels.mask.index[-1] != pd.Timestamp('2026-11-06'):
        raise ValueError('Frozen panel must end on 2026-11-06')
    hourly, hourly_sha256 = load_hourly(panels.mask.columns, klines_dir)
    if expected_hourly_sha256 is not None and hourly_sha256 != expected_hourly_sha256:
        raise ValueError('hourly_sha256 changed from previous output; refusing rerun')
    inputs['hourly_sha256'] = hourly_sha256

    primary = _run_factor_set(
        panels, hourly, PRIMARY_FACTORS, workers=workers, placebo=placebo, run_controls=True, progress=progress
    )
    secondary = {}
    if primary['validation']['valid']:
        progress('Running secondary single-factor f_0ce8b2ee descriptive check...', flush=True)
        secondary = _run_factor_set(
            panels, hourly, SECONDARY_FACTORS, workers=1, placebo=0, run_controls=False, progress=progress
        )

    result = dict(
        version=1,
        market='crypto',
        horizon=3,
        engine='backtrader',
        smoke=placebo != 200,
        prereg=dict(path=str(PREREG.relative_to(REPO)), sha256=PREREG_SHA256),
        inputs=inputs,
        snapshot=str(Path(snapshot).resolve()),
        klines_dir=str(Path(klines_dir).resolve()),
        factors=dict(primary=list(PRIMARY_FACTORS), secondary=list(SECONDARY_FACTORS)),
        panel=panels.meta,
        validation=primary['validation'],
        intervals=primary['intervals'],
        statistics=primary['statistics'],
        verdicts=primary['verdicts'],
        placebo=dict(n=placebo, seed=SEED, means=primary.get('placebo_means', {})),
        secondary=secondary,
        execution_notes=NOTES,
        initial_capital=1000.0,
        commission=0.0005,
        slippage=0.0003,
        planned_K=PLANNED_K,
        z_K=z_threshold(PLANNED_K),
    )
    if verify_prereg() != prereg_before:
        raise ValueError('Preregistration changed during study; refusing output')
    return result


def _fmt(v, digits=6):
    return 'NA' if v is None else f'{v:.{digits}f}'


def report(result):
    lines = [
        '# fq_crypto 市值/流动性分层护栏 S1 离线筛选报告',
        '',
        f'预注册 `{result["prereg"]["path"]}` SHA256: `{PREREG_SHA256}`。',
        f'冒烟模式: `{result["smoke"]}`（placebo={result["placebo"]["n"]}）。',
        '',
        '## 判定汇总（主分析：7 因子生产 ensemble）',
        '',
        '| 变体 | 结论 | 拼接 Δ 均值 | val Δ | forward Δ | 周聚类 SE | z | z_K(12) | vs B_x Δ | 安慰剂分位 | 改权重期数 |',
        '|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|',
    ]
    for arm, *_ in ARMS:
        st = result['statistics'].get(arm) or {}
        lines.append(
            '| '
            + ' | '.join([
                arm,
                f'`{result["verdicts"][arm]}`',
                _fmt(st.get('mean_delta')),
                _fmt(st.get('val_delta')),
                _fmt(st.get('forward_delta')),
                _fmt(st.get('se')),
                _fmt(st.get('z'), 4),
                _fmt(st.get('z_K'), 4),
                _fmt(st.get('bx_delta')),
                _fmt(st.get('placebo_pct'), 3),
                str(st.get('changed_rebalances', 0)),
            ])
            + ' |'
        )
    lines += ['', '## 各区间绩效与暴露（主分析 7 因子）', '',
              '| 区间 | 组 | 收益 % | 年化 % | Sharpe | 最大回撤 % | 最差3日 % | 换手 | 手续费 | 滑点 | 多头资金费 | 空头资金费 | S20 止损 | 日均总暴露 | ρ |',
              '|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|']
    for name, interval in result['intervals'].items():
        for variant, run in interval['variants'].items():
            m = run['metrics']
            exp = interval['exposure'].get(variant, {})
            rho = interval['rho'].get(variant.replace('B_x_', ''), None) if variant.startswith('B_x_') else None
            lines.append(
                '| '
                + ' | '.join([
                    name,
                    variant,
                    _fmt(m['return_pct'], 2),
                    _fmt(m['cagr_pct'], 2),
                    _fmt(m['sharpe'], 3),
                    _fmt(m['maxdd_pct'], 2),
                    _fmt(m['worst_3d_return_pct'], 2),
                    _fmt(m['turnover'], 2),
                    _fmt(m['commission_paid'], 2),
                    _fmt(m['slippage_paid'], 2),
                    _fmt(m['funding_by_side']['long'], 2),
                    _fmt(m['funding_by_side']['short'], 2),
                    str(m['exit_trigger_counts'].get('stop_loss', 0)),
                    _fmt(exp.get('gross'), 3),
                    _fmt(rho, 4),
                ])
                + ' |'
            )
    if result.get('secondary') and result['secondary'].get('intervals'):
        lines += ['', '## 次要描述：单因子 `f_0ce8b2ee` 对照', '',
                  '| 区间 | 组 | 收益 % | Sharpe | 最大回撤 % | 日均总暴露 |',
                  '|---|---|---:|---:|---:|---:|']
        for name, interval in result['secondary']['intervals'].items():
            for variant, run in interval['variants'].items():
                m = run['metrics']
                exp = interval['exposure'].get(variant, {})
                lines.append(
                    f'| {name} | {variant} | {_fmt(m["return_pct"], 2)} | {_fmt(m["sharpe"], 3)} | '
                    f'{_fmt(m["maxdd_pct"], 2)} | {_fmt(exp.get("gross"), 3)} |'
                )
        lines += ['', '| 变体 (f_0ce8b2ee) | 拼接 Δ 均值 | val Δ | forward Δ | z |',
                  '|---|---:|---:|---:|---:|']
        for arm, *_ in ARMS:
            st = result['secondary']['statistics'][arm]
            lines.append(
                f'| {arm} | {_fmt(st["mean_delta"])} | {_fmt(st["val_delta"])} | '
                f'{_fmt(st["forward_delta"])} | {_fmt(st["z"], 4)} |'
            )
    lines += ['', '## 小时线校验', '', '```json',
              json.dumps(store.finite({k: v for k, v in result['validation'].items() if k != 'failures'}),
                         ensure_ascii=False, indent=2),
              '```', '']
    return '\n'.join(lines)


def validate_output(out, snapshot, klines_dir):
    out = Path(out).resolve()
    if any(out.is_relative_to(Path(root).resolve()) for root in (snapshot, klines_dir)):
        raise ValueError('--out must be outside snapshot and klines directories')
    return out


def write_ledger(result, path):
    if result['smoke'] or result['placebo']['n'] != 200 or 'invalid' in result['verdicts'].values():
        return []
    rows = []
    for arm, change_type, idea_id, planned_k in ARMS:
        st = result['statistics'][arm]
        def build(k, previous, _arm=arm, _ct=change_type, _id=idea_id, _pk=planned_k, _st=st):
            return dict(
                track='fq_crypto',
                stage='S1',
                change_type=_ct,
                idea_id=_id,
                baseline_version='prod7@snap20261108+S20h',
                verdict=result['verdicts'][_arm],
                delta=_st['mean_delta'],
                se=_st['se'],
                z=_st['z'],
                dev_delta=_st['val_delta'],
                holdout_delta=_st['forward_delta'],
                bx_delta=_st.get('bx_delta'),
                placebo_pct=_st.get('placebo_pct'),
                z_K=z_threshold(PLANNED_K),
                preregistered_K=_pk,
                input_hashes=result['inputs'],
                prereg_sha256=PREREG_SHA256,
                trials=1,
            )
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
        result = study(
            args.snapshot,
            args.klines_dir,
            workers=args.workers,
            placebo=args.placebo,
            expected_hourly_sha256=expected,
        )
        if verify_prereg() != prereg_before:
            raise ValueError('Preregistration changed during study; refusing output')
    except (ValueError, OSError) as exc:
        parser.error(str(exc))
    out.mkdir(parents=True, exist_ok=True)
    store.write_json(out / 'results.json', result)
    (out / 'report.md').write_text(report(result), encoding='utf-8')
    if args.write_ledger:
        write_ledger(result, args.ledger)
    for arm, *_ in ARMS:
        print(f'VERDICT {arm}: {result["verdicts"][arm]}')
    return 1 if 'invalid' in result['verdicts'].values() else 0


if __name__ == '__main__':
    raise SystemExit(main())
