"""Frozen fq_us size-tier split (U1 mega_large, U2 mid_small, U3 50/50 split) S1 study.

python -m scripts.backtest.fq_us_size_tier_split_study --out /tmp/fqus_split
All execution and accounting use Backtrader (scripts.backtest.fq_us_momentum_study).
"""
from __future__ import annotations

import argparse
from concurrent.futures import ProcessPoolExecutor, as_completed
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

from scripts.factor_mining import tiers
from scripts.factor_mining.quant_targets import _capped_invvol
from . import evolution_ledger
from . import fq_us_momentum_stress as stress
from . import fq_us_momentum_study as s1
from . import fq_us_volscale_study as vs
from .incremental_filter import z_threshold

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-15-fq-us-size-tier-split-s1-prereg.md'
PREREG_SHA256 = '632a19fb313f5b1fd18b997442e967fb34144f7efadc0e012036917348931abf'
SEED = 20261115
PLANNED_K = 11
US_MEGA_ADV20 = tiers.US_MEGA_LARGE_ADV20  # $500,000,000
ADV20_WINDOW = 20
INITIAL_CASH = s1.PARAMS['initial_cash']
BASELINE_VERSION = 'mom_ls_invvol_v1'

ARMS = (
    ('U1', 'fq-us-mega-large-only-v1', 9),
    ('U2', 'fq-us-mid-small-only-v1', 10),
    ('U3', 'fq-us-tier-dual-sleeve-50-50-v1', 11),
)


def verify_prereg() -> bytes:
    raw = PREREG.read_bytes()
    if hashlib.sha256(raw).hexdigest() != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run')
    return raw


def compute_causal_adv20(frames: dict[str, pd.DataFrame], universe: list[str], calendar: pd.DatetimeIndex) -> pd.DataFrame:
    """Strictly causal 20-day mean dollar volume (close * volume <= t)."""
    close = pd.DataFrame({s: frames[s]['close'].where(s1.valid_rows(frames[s])) for s in universe}).reindex(calendar)
    vol = pd.DataFrame({s: frames[s]['volume'] for s in universe}).reindex(calendar)
    dv = (close * vol).where(close.notna() & vol.ge(0))
    return dv.rolling(ADV20_WINDOW, min_periods=ADV20_WINDOW).mean()


def decision_subset(features: dict, date: pd.Timestamp, allowed_symbols: list[str], *, min_eligible: int = 15) -> dict:
    """Apply baseline mom_ls_invvol_v1 selection restricted to allowed_symbols."""
    symbols = sorted(allowed_symbols)
    result = dict(
        date=str(pd.Timestamp(date).date()),
        eligible=symbols,
        eligible_count=len(symbols),
        quintile_count=0,
        signals={},
        volatility={},
        long={},
        short={},
        L={},
    )
    if len(symbols) < min_eligible:
        return result
    signals = features['signal'].loc[date, symbols]
    if not np.isfinite(signals).all():
        raise ValueError(f'{date}: eligible instrument has undefined momentum')
    ordered = signals.sort_values(kind='stable')
    n = max(1, math.floor(0.2 * len(symbols)))
    longs, shorts = list(ordered.index[-n:]), list(ordered.index[:n])
    vol = features['volatility'].loc[date, sorted(set(longs + shorts))]
    if not (np.isfinite(vol) & vol.gt(0)).all():
        raise ValueError(f'{date}: selected instrument has undefined inverse volatility')
    result.update(
        quintile_count=n,
        signals=signals.to_dict(),
        volatility=vol.to_dict(),
        long=_capped_invvol(vol.loc[longs], 1.0, 0.20).to_dict(),
        short=(-_capped_invvol(vol.loc[shorts], 1.0, 0.20)).to_dict(),
        L=_capped_invvol(vol.loc[longs], 1.0, 0.10).to_dict(),
    )
    return result


def build_tier_decisions(
    frames: dict[str, pd.DataFrame],
    universe: list[str],
    calendar: pd.DatetimeIndex,
    *,
    min_eligible: int = 15,
) -> tuple[pd.DatetimeIndex, list[dict], dict[str, list[dict]]]:
    features = s1.build_features(frames, universe, calendar)
    adv20 = compute_causal_adv20(frames, universe, calendar)
    sessions, base_decisions = s1.rebalance_schedule(features, calendar)
    non_etf = {s for s in universe if s not in tiers.US_ETFS}

    u1_decisions, u2_decisions = [], []
    for bd in base_decisions:
        date = pd.Timestamp(bd['date'])
        adv_row = adv20.loc[date]
        elig = [s for s in bd['eligible'] if s in non_etf and np.isfinite(adv_row.get(s, np.nan))]
        mega = [s for s in elig if float(adv_row[s]) >= US_MEGA_ADV20]
        mid = [s for s in elig if float(adv_row[s]) < US_MEGA_ADV20]
        u1_decisions.append(decision_subset(features, date, mega, min_eligible=min_eligible))
        u2_decisions.append(decision_subset(features, date, mid, min_eligible=min_eligible))

    return sessions, base_decisions, {'B': base_decisions, 'U1': u1_decisions, 'U2': u2_decisions}


def _active_frames(frames: dict[str, pd.DataFrame], targets: dict[str, dict[str, float]]) -> dict[str, pd.DataFrame]:
    """Restrict frames passed to Backtrader to symbols traded in targets (plus first symbol if empty).

    Untouched zero-weight symbols never receive orders and have identical broker cash/equity.
    """
    used = sorted({sym for weights in targets.values() for sym in weights})
    if not used:
        used = [sorted(frames)[0]]
    return {s: frames[s] for s in used}


def run_ls_arm(
    stock_frames: dict[str, pd.DataFrame],
    sessions: pd.DatetimeIndex,
    decisions: list[dict],
    scales: dict[str, float] | None = None,
    *,
    full_frames_for_baseline: bool = False,
    compute_exposure: bool = True,
) -> dict:
    scales = scales or {d['date']: 1.0 for d in decisions}
    rebalances = [d['date'] for d in decisions]
    legs = {}
    for leg in ('long', 'short'):
        tmap = {d['date']: {s: w * scales[d['date']] for s, w in d[leg].items()} for d in decisions}
        feed_frames = stock_frames if full_frames_for_baseline else _active_frames(stock_frames, tmap)
        legs[leg] = s1.run_sleeve(
            feed_frames,
            sessions,
            tmap,
            cash=INITIAL_CASH / 2,
            commission=s1.PARAMS[f'{leg}_cost_per_fill'],
            short=(leg == 'short'),
        )
        legs[leg]['metrics'] = vs.describe_path(
            legs[leg]['equity_curve'], INITIAL_CASH / 2, fills=legs[leg]['fills']
        )
    long_c, short_c = legs['long']['equity_curve'], legs['short']['equity_curve']
    curve = stress.equalized_path(long_c, short_c, rebalances, initial_cash=INITIAL_CASH)
    independent = [
        dict(date=a['date'], equity=a['equity'] + b['equity']) for a, b in zip(long_c, short_c, strict=True)
    ]
    fills = legs['long']['fills'] + legs['short']['fills']
    out = dict(
        equity_curve=curve,
        metrics=vs.describe_path(curve, INITIAL_CASH, post_processing=True),
        independent_sleeves=dict(
            equity_curve=independent,
            metrics=vs.describe_path(independent, INITIAL_CASH, fills=fills),
        ),
        legs=legs,
        period_table=s1.period_table(curve, decisions),
    )
    if compute_exposure:
        exp = vs.gross_exposure(long_c, short_c, rebalances)
        out['mean_gross_exposure'] = float(np.mean([r['gross_exposure'] for r in exp]))
    return out


def combine_dual_sleeve(u1_arm: dict, u2_arm: dict, decisions: list[dict]) -> dict:
    """Combine U1 and U2 equalized equity paths 50/50 at every rebalance date."""
    rebalances = [d['date'] for d in decisions]
    c1 = [dict(date=r['date'], equity=0.5 * r['equity']) for r in u1_arm['equity_curve']]
    c2 = [dict(date=r['date'], equity=0.5 * r['equity']) for r in u2_arm['equity_curve']]
    curve = stress.equalized_path(c1, c2, rebalances, initial_cash=INITIAL_CASH)
    out = dict(
        equity_curve=curve,
        metrics=vs.describe_path(curve, INITIAL_CASH, post_processing=True),
        period_table=s1.period_table(curve, decisions),
    )
    if 'mean_gross_exposure' in u1_arm and 'mean_gross_exposure' in u2_arm:
        out['mean_gross_exposure'] = 0.5 * (u1_arm['mean_gross_exposure'] + u2_arm['mean_gross_exposure'])
    return out


def paired_delta_stats(arm_periods: list[dict], base_periods: list[dict]) -> dict:
    deltas = np.array([a['net_return'] - b['net_return'] for a, b in zip(arm_periods, base_periods, strict=True)], dtype=float)
    n = len(deltas)
    mean = float(deltas.mean())
    sd = float(deltas.std(ddof=1)) if n >= 2 else 0.0
    se = sd / math.sqrt(n) if n >= 2 else None
    z = (mean / se) if (se and se > 0) else (0.0 if mean == 0 else math.copysign(math.inf, mean))
    half = n // 2
    dev_delta = float(deltas[:half].mean()) if half else None
    hold_delta = float(deltas[half:].mean()) if n - half else None
    return dict(n=n, mean_delta=mean, sd=sd, se=se, z=z, dev_delta=dev_delta, holdout_delta=hold_delta)


_CTX = None


def _init_placebo(ctx):
    global _CTX
    _CTX = ctx


def _placebo_job(seed_idx: int):
    stock_frames, rebal_sessions, features, meta_dates, base_periods = _CTX
    rng = np.random.default_rng(SEED + seed_idx)
    p1_dec, p2_dec = [], []
    for item in meta_dates:
        date = pd.Timestamp(item['date'])
        pool = item['elig']
        n_mega = min(item['n_mega'], len(pool))
        chosen = set(rng.choice(pool, size=n_mega, replace=False)) if n_mega else set()
        mega_sub = [s for s in pool if s in chosen]
        mid_sub = [s for s in pool if s not in chosen]
        p1_dec.append(decision_subset(features, date, mega_sub, min_eligible=15))
        p2_dec.append(decision_subset(features, date, mid_sub, min_eligible=15))

    p1 = run_ls_arm(stock_frames, rebal_sessions, p1_dec, compute_exposure=False)
    p2 = run_ls_arm(stock_frames, rebal_sessions, p2_dec, compute_exposure=False)
    p3 = combine_dual_sleeve(p1, p2, p1_dec)
    return (
        seed_idx,
        paired_delta_stats(p1['period_table'], base_periods)['mean_delta'],
        paired_delta_stats(p2['period_table'], base_periods)['mean_delta'],
        paired_delta_stats(p3['period_table'], base_periods)['mean_delta'],
    )


def _main_arm_job(task):
    tag, stock_frames, sessions, dec, scales, full_flag = task
    return tag, run_ls_arm(stock_frames, sessions, dec, scales=scales, full_frames_for_baseline=full_flag)


def decide_arm_verdict(stats: dict, d2_stats: dict | None, *, changed: bool, d2_repro_ok: bool) -> str:
    if not d2_repro_ok or not changed:
        return 'invalid'
    if (
        stats['z'] is None
        or stats['z'] < 1.0
        or stats['dev_delta'] is None
        or stats['dev_delta'] <= 0
        or stats['holdout_delta'] is None
        or stats['holdout_delta'] <= 0
    ):
        return 'fail'
    if stats['placebo_pct'] is None or stats['placebo_pct'] < 0.90:
        return 'fewer_trades'
    d2_ok = d2_stats is not None and d2_stats['mean_delta'] > 0
    if stats['z'] >= z_threshold(PLANNED_K) and d2_ok:
        return 'strong'
    return 'screen_pass'


def run_study(
    *,
    placebo_n: int = 100,
    workers: int = 8,
    allow_input_drift: bool = False,
    cache_dir: Path | None = None,
    seed_end: int | None = None,
) -> dict:
    verify_prereg()
    hashes, files, mismatches = vs.guard_hashes(('D2', 'D3'), allow_input_drift)
    if cache_dir is not None:
        cache_dir.mkdir(parents=True, exist_ok=True)
        (cache_dir / 'placebo').mkdir(parents=True, exist_ok=True)

    ds_reports = {}
    for name in ('D2', 'D3'):
        print(f'{name}: loading frozen inputs', flush=True)
        loaded = vs.load_dataset(name)
        frames, calendar = loaded.pop('frames'), loaded.pop('calendar')
        universe = sorted(set(loaded['requested']) & set(frames))
        stock_frames = {s: frames[s] for s in universe}
        sessions, base_dec, dec_map = build_tier_decisions(frames, universe, calendar, min_eligible=15)
        adv20 = compute_causal_adv20(frames, universe, calendar)
        non_etf = {s for s in universe if s not in tiers.US_ETFS}

        main_cache = (cache_dir / f'{name}_main.json') if cache_dir else None
        if main_cache is not None and main_cache.exists():
            print(f'  loaded {name} main arms from cache', flush=True)
            cached = json.loads(main_cache.read_text(encoding='utf-8'))
            arms = cached['arms']
            volscale_arms = cached['volscale_arms']
            u2_min10 = cached['u2_min10']
            s1_repro = cached['s1_reproduction']
        else:
            close_mat = pd.DataFrame(
                {s: frames[s]['close'].where(s1.valid_rows(frames[s])) for s in universe}
            ).reindex(sessions).ffill()

            tasks = []
            for code in ('B', 'U1', 'U2'):
                tasks.append((('base', code), stock_frames, sessions, dec_map[code], None, (code == 'B')))
                sig = vs.volatility_signal(vs.unscaled_returns(close_mat, dec_map[code]))
                v_scales = {d['date']: float(sig.loc[pd.Timestamp(d['date']), 'k']) for d in dec_map[code]}
                tasks.append((('volscale', code), stock_frames, sessions, dec_map[code], v_scales, False))

            arms, volscale_arms = {}, {}
            print(f'  running {name} 6 sleeves in parallel (workers={min(workers, len(tasks))})...', flush=True)
            with ProcessPoolExecutor(max_workers=min(workers, len(tasks))) as pool:
                for tag, res in pool.map(_main_arm_job, tasks):
                    kind, code = tag
                    if kind == 'base':
                        arms[code] = res
                    else:
                        volscale_arms[code] = res

            arms['U3'] = combine_dual_sleeve(arms['U1'], arms['U2'], dec_map['B'])
            volscale_arms['U3'] = combine_dual_sleeve(volscale_arms['U1'], volscale_arms['U2'], dec_map['B'])

            s1_repro = None
            if name == 'D2':
                actual_ret = arms['B']['independent_sleeves']['metrics']['total_return']
                actual_n = len(dec_map['B']) - 1
                s1_repro = dict(
                    expected_total_return=3.878078,
                    actual_total_return=actual_ret,
                    expected_n=62,
                    actual_n=actual_n,
                    passed=abs(actual_ret - 3.878078) <= 5e-7 and actual_n == 62,
                )
                if not s1_repro['passed']:
                    raise ValueError('D2 S1 reproduction failed: ' + json.dumps(s1_repro))

            u2_min10 = None
            if name == 'D2':
                _, _, dec10 = build_tier_decisions(frames, universe, calendar, min_eligible=10)
                u2_min10 = run_ls_arm(stock_frames, sessions, dec10['U2'])

            if main_cache is not None:
                # Trim heavy fills/order_events before caching main checkpoint
                def _slim(arm_dict):
                    slim_d = {k: v for k, v in arm_dict.items() if k != 'legs'}
                    if 'legs' in arm_dict:
                        slim_d['legs'] = {
                            leg: {'metrics': lv['metrics']} for leg, lv in arm_dict['legs'].items()
                        }
                    return slim_d

                payload = evolution_ledger.json_safe(
                    dict(
                        arms={k: _slim(v) for k, v in arms.items()},
                        volscale_arms={k: _slim(v) for k, v in volscale_arms.items()},
                        u2_min10=_slim(u2_min10) if u2_min10 else None,
                        s1_reproduction=s1_repro,
                    )
                )
                main_cache.write_text(json.dumps(payload), encoding='utf-8')

        # Placebo on D3
        placebo = {'U1': [], 'U2': [], 'U3': []}
        if name == 'D3' and placebo_n > 0:
            target_end = placebo_n if seed_end is None else min(placebo_n, seed_end)
            missing_seeds = []
            cached_seeds = {}
            for s_idx in range(target_end):
                pfile = (cache_dir / 'placebo' / f'seed_{s_idx:04d}.json') if cache_dir else None
                if pfile is not None and pfile.exists():
                    cached_seeds[s_idx] = json.loads(pfile.read_text(encoding='utf-8'))
                else:
                    missing_seeds.append(s_idx)

            if missing_seeds:
                features = s1.build_features(frames, universe, calendar)
                meta_dates = []
                for bd in base_dec:
                    date = pd.Timestamp(bd['date'])
                    adv_row = adv20.loc[date]
                    elig = [s for s in bd['eligible'] if s in non_etf and np.isfinite(adv_row.get(s, np.nan))]
                    n_mega = sum(1 for s in elig if float(adv_row[s]) >= US_MEGA_ADV20)
                    meta_dates.append(dict(date=bd['date'], elig=elig, n_mega=n_mega))
                base_periods = arms['B']['period_table']
                rebal_sessions = pd.DatetimeIndex([pd.Timestamp(d['date']) for d in base_dec])
                ctx = (stock_frames, rebal_sessions, features, meta_dates, base_periods)
                with ProcessPoolExecutor(max_workers=workers, initializer=_init_placebo, initargs=(ctx,)) as pool:
                    jobs = [pool.submit(_placebo_job, i) for i in missing_seeds]
                    for idx, fut in enumerate(as_completed(jobs), 1):
                        s_idx, d1, d2, d3 = fut.result()
                        rec = dict(seed_idx=s_idx, U1=d1, U2=d2, U3=d3)
                        cached_seeds[s_idx] = rec
                        if cache_dir is not None:
                            (cache_dir / 'placebo' / f'seed_{s_idx:04d}.json').write_text(
                                json.dumps(rec), encoding='utf-8'
                            )
                        done = len(cached_seeds)
                        if idx % 10 == 0 or idx == len(missing_seeds):
                            print(f'  D3 placebo cached {done}/{target_end}', flush=True)

            if seed_end is not None and seed_end < placebo_n:
                return dict(partial_cache_only=True, cached_placebo=len(cached_seeds), target=placebo_n)

            for s_idx in range(placebo_n):
                rec = cached_seeds[s_idx]
                placebo['U1'].append(rec['U1'])
                placebo['U2'].append(rec['U2'])
                placebo['U3'].append(rec['U3'])

        # Correlation U1 vs U2 period returns
        r1 = [p['net_return'] for p in arms['U1']['period_table']]
        r2 = [p['net_return'] for p in arms['U2']['period_table']]
        corr_u1_u2 = float(np.corrcoef(r1, r2)[0, 1]) if len(r1) >= 2 else None

        comparisons = {}
        for code in ('U1', 'U2', 'U3'):
            st = paired_delta_stats(arms[code]['period_table'], arms['B']['period_table'])
            if name == 'D3' and placebo[code]:
                st['placebo_pct'] = float(sum(p < st['mean_delta'] for p in placebo[code]) / len(placebo[code]))
            else:
                st['placebo_pct'] = None
            comparisons[code] = st

        ds_reports[name] = dict(
            start=str(sessions[0].date()),
            end=str(sessions[-1].date()),
            n_periods=len(base_dec) - 1,
            arms=arms,
            volscale_arms=volscale_arms,
            comparisons=comparisons,
            corr_u1_u2=corr_u1_u2,
            s1_reproduction=s1_repro,
            u2_min10=u2_min10,
            tier_counts=[
                dict(
                    date=bd['date'],
                    all=bd['eligible_count'],
                    mega=dec_map['U1'][i]['eligible_count'],
                    mid=dec_map['U2'][i]['eligible_count'],
                )
                for i, bd in enumerate(base_dec)
            ],
        )

    verdicts = {}
    d2_ok_repro = bool(ds_reports['D2']['s1_reproduction']['passed'])
    for code, idea_id, k_num in ARMS:
        st = ds_reports['D3']['comparisons'][code]
        d2_st = ds_reports['D2']['comparisons'][code]
        changed = any(
            abs(a['net_return'] - b['net_return']) > 1e-9
            for a, b in zip(
                ds_reports['D3']['arms'][code]['period_table'],
                ds_reports['D3']['arms']['B']['period_table'],
            )
        )
        verdicts[code] = dict(
            idea_id=idea_id,
            k_num=k_num,
            verdict=decide_arm_verdict(st, d2_st, changed=changed, d2_repro_ok=d2_ok_repro),
            **st,
            d2_mean_delta=d2_st['mean_delta'],
            d2_z=d2_st['z'],
        )

    return dict(
        prereg_path=str(PREREG.relative_to(REPO)),
        prereg_sha256=PREREG_SHA256,
        planned_K=PLANNED_K,
        z_K=z_threshold(PLANNED_K),
        placebo_n=placebo_n,
        baseline_version=BASELINE_VERSION,
        input_hashes=hashes,
        input_drift=mismatches,
        datasets=ds_reports,
        verdicts=verdicts,
    )


def append_ledger(path: Path, report: dict) -> list[dict]:
    rows = []
    for code, idea_id, k_num in ARMS:
        v = report['verdicts'][code]
        d3_m = report['datasets']['D3']['arms'][code]['metrics']
        d3_b = report['datasets']['D3']['arms']['B']['metrics']

        def build(k, _prev, _v=v, _code=code, _idea=idea_id, _k=k_num, _dm=d3_m, _db=d3_b):
            return dict(
                stage='S1',
                change_type='E',
                idea_id=_idea,
                baseline_version=BASELINE_VERSION,
                verdict=_v['verdict'],
                preregistered_K=PLANNED_K,
                planned_arm_K=_k,
                actual_ledger_K=k,
                delta=_v['mean_delta'],
                se=_v['se'],
                z=_v['z'],
                placebo_pct=_v['placebo_pct'],
                dev_delta=_v['dev_delta'],
                holdout_delta=_v['holdout_delta'],
                d3_sharpe=_dm['sharpe'],
                d3_base_sharpe=_db['sharpe'],
                d3_max_drawdown=_dm['max_drawdown'],
                d3_base_max_drawdown=_db['max_drawdown'],
                d2_mean_delta=_v['d2_mean_delta'],
                d2_z=_v['d2_z'],
                prereg_path=report['prereg_path'],
                prereg_sha256=report['prereg_sha256'],
                input_hashes=report['input_hashes'],
                report_path=report.get('report_path'),
            )

        rows.append(evolution_ledger.append_event(path, 'fq_us', build))
    return rows


def render_markdown(report: dict) -> str:
    def pct(val, signed=False):
        return '无' if val is None else format(val, '+.2%' if signed else '.2%')

    def fnum(val, fmt='.4f'):
        return '无' if val is None else format(val, fmt)

    lines = [
        '# fq_us 市值/流动性分层拆分（U1 / U2 / U3）S1 报告',
        '',
        f"- 预注册：`{report['prereg_path']}` (`{report['prereg_sha256'][:12]}...`)",
        f"- D2 基准 S1 独立双腿复现：`{pct(report['datasets']['D2']['s1_reproduction']['actual_total_return'], True)}` (预期 `+387.81%`, passed=`{report['datasets']['D2']['s1_reproduction']['passed']}`)",
        f"- 保守强阈值：`z_K(11) = {report['z_K']:.4f}`；`D3 placebo_n = {report['placebo_n']}`。",
        '',
        '## D3 主判定表（相对现行线上 `B = mom_ls_invvol_v1` 配对期差）',
        '',
        '| 变体 | 期均 Δ | z | dev_delta | holdout_delta | 安慰剂分位 | D2 期均 Δ (z) | 结论 |',
        '|---|---:|---:|---:|---:|---:|---|---:|',
    ]
    for code, *_ in ARMS:
        v = report['verdicts'][code]
        lines.append(
            f"| {code} ({v['idea_id']}) | {pct(v['mean_delta'], True)} | {fnum(v['z'], '+.2f')} | "
            f"{pct(v['dev_delta'], True)} | {pct(v['holdout_delta'], True)} | {fnum(v['placebo_pct'], '.1%')} | "
            f"{pct(v['d2_mean_delta'], True)} ({fnum(v['d2_z'], '+.2f')}) | **{v['verdict']}** |"
        )

    for ds in ('D3', 'D2'):
        d = report['datasets'][ds]
        lines += [
            '',
            f"## {ds} ({d['start']} .. {d['end']}, n={d['n_periods']}, corr(U1,U2)={fnum(d['corr_u1_u2'], '.3f')})",
            '',
            '| 组别 | 累计收益 | Sharpe | MaxDD | 日均总敞口 | 叠加 Volscale 收益 / Sharpe / MaxDD |',
            '|---|---:|---:|---:|---:|---|',
        ]
        for code in ('B', 'U1', 'U2', 'U3'):
            m = d['arms'][code]['metrics']
            vm = d['volscale_arms'][code]['metrics']
            lines.append(
                f"| {code} | {pct(m['total_return'], True)} | {fnum(m['sharpe'])} | {pct(m['max_drawdown'])} | "
                f"{pct(d['arms'][code]['mean_gross_exposure'])} | "
                f"{pct(vm['total_return'], True)} / {fnum(vm['sharpe'])} / {pct(vm['max_drawdown'])} |"
            )
        if ds == 'D2' and d.get('u2_min10'):
            m10 = d['u2_min10']['metrics']
            lines.append(f"\n- D2 `U2 (min_eligible=10)` 敏感性：累计 `{pct(m10['total_return'], True)}`, Sharpe `{fnum(m10['sharpe'])}`, MaxDD `{pct(m10['max_drawdown'])}`。")

    return '\n'.join(lines) + '\n'


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--placebo', type=int, default=100)
    parser.add_argument('--workers', type=int, default=8)
    parser.add_argument('--seed-end', type=int, default=None)
    parser.add_argument('--write-ledger', action='store_true')
    args = parser.parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)
    report = run_study(
        placebo_n=args.placebo,
        workers=args.workers,
        cache_dir=args.out / 'cache',
        seed_end=args.seed_end,
    )
    if report.get('partial_cache_only'):
        print(f"partial cache saved: {report['cached_placebo']}/{report['target']}")
        return 0
    report['report_path'] = str(args.out / 'report.json')
    if args.write_ledger:
        report['ledger_rows'] = append_ledger(REPO / 'data/evolution/ledger.jsonl', report)
    safe = evolution_ledger.json_safe(report)
    (args.out / 'report.json').write_text(json.dumps(safe, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    (args.out / 'report.md').write_text(render_markdown(report), encoding='utf-8')
    for code, *_ in ARMS:
        v = report['verdicts'][code]
        print(
            f"{code}: d3_delta={v['mean_delta']:+.4%} z={v['z']:+.2f} placebo={v['placebo_pct']:.1%} "
            f"d2_delta={v['d2_mean_delta']:+.4%} verdict={v['verdict']}"
        )
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
