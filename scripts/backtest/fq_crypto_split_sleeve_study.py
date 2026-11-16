"""Frozen fq_crypto dual-sleeve size-tier split (S2_mid, S1_split = 50% M3 + 50% S2_mid) S1 study.

python -m scripts.backtest.fq_crypto_split_sleeve_study --out /tmp/fq_crypto_split
Inputs are read-only. Portfolio execution and accounting use Backtrader + CarryBroker + hourly S20.
"""
from __future__ import annotations

import argparse
from concurrent.futures import ProcessPoolExecutor, as_completed
import datetime as dt
import hashlib
import json
from pathlib import Path

import numpy as np
import pandas as pd

from scripts.factor_mining import panel, quant_targets, tiers
from scripts.factor_mining.ensemble import ensemble_scores
from . import evolution_ledger
from . import fq_us_momentum_stress as stress
from .fq_crypto_size_tier_study import (
    PRIMARY_FACTORS,
    build_subset_target,
    exposure_summary,
    mean_daily_gross,
)
from .fq_shortexit_funding_study import (
    KLINES,
    SNAPSHOT,
    comparison,
    execute,
    load_hourly,
    validate_hourly,
    verify_snapshot,
)
from .fq_stop_tp_study import sharpe, simple_returns
from .incremental_filter import z_threshold

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-15-fq-crypto-split-sleeve-s1-prereg.md'
PREREG_SHA256 = 'cffe493f78a008c9e168a51d56af6cce8c5ffa6ff6521274af206f0a87077e28'
SEED = 20261116
PLANNED_K = 14
BASELINE_VERSION = 'ls_invvol_protected_v1_h3_7f'

ARMS = (
    ('S2_mid', 'fq-crypto-mid-small-rebuild-v1', 13),
    ('S1_split', 'fq-crypto-tier-dual-sleeve-50-50-v1', 14),
)


def verify_prereg() -> bytes:
    raw = PREREG.read_bytes()
    if hashlib.sha256(raw).hexdigest() != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run')
    return raw


def combine_equalized_sleeves(run_a: dict, run_b: dict, rebalances: list[str], *, initial_capital: float = 1000.0) -> dict:
    """Combine two independent daily_equity runs with 50/50 reset on rebalance dates."""
    dates = sorted(run_a['daily_equity'])
    if dates != sorted(run_b['daily_equity']):
        raise ValueError('Aligned daily_equity dates required for 50/50 split')
    curve_a = [dict(date=d, equity=0.5 * float(run_a['daily_equity'][d])) for d in dates]
    curve_b = [dict(date=d, equity=0.5 * float(run_b['daily_equity'][d])) for d in dates]
    reb = [d for d in rebalances if d in set(dates)]
    if dates[0] not in reb:
        reb = [dates[0]] + reb
    eq_curve = stress.equalized_path(curve_a, curve_b, reb, initial_cash=initial_capital)
    daily_equity = {r['date']: float(r['equity']) for r in eq_curve}

    out = dict(daily_equity=daily_equity, post_processing=True)
    rets = simple_returns(out)
    total_ret = eq_curve[-1]['equity'] / initial_capital - 1.0
    n_days = len(eq_curve)
    cagr = (eq_curve[-1]['equity'] / initial_capital) ** (365.0 / n_days) - 1.0 if n_days else 0.0
    peak, maxdd = initial_capital, 0.0
    for r in eq_curve:
        peak = max(peak, r['equity'])
        maxdd = max(maxdd, 1.0 - r['equity'] / peak)
    out['metrics'] = dict(
        return_pct=float(total_ret * 100.0),
        cagr_pct=float(cagr * 100.0),
        sharpe=float(sharpe(rets)),
        maxdd_pct=float(maxdd * 100.0),
        worst_3d_return_pct=float(((1 + rets).rolling(3).apply(np.prod, raw=True) - 1).min() * 100.0),
    )
    return out


def prepare_interval_targets(panels, scores, factors, dates):
    columns = scores.columns
    b_df = pd.DataFrame(np.nan, index=dates, columns=columns)
    m3_df = pd.DataFrame(np.nan, index=dates, columns=columns)
    s2_df = pd.DataFrame(np.nan, index=dates, columns=columns)

    mega_mask_all = tiers.tier_mask(panels, 'crypto', 'mega_large')
    mid_mask_all = tiers.tier_mask(panels, 'crypto', 'mid_small')
    rule = quant_targets._rule('crypto')
    records = []
    changed = {'S2_mid': 0, 'S1_split': 0}

    for date in dates[::3]:
        row = scores.loc[date].where(panels.mask.loc[date])
        row = row[np.isfinite(row)]
        pct = row.rank(pct=True)
        entry = dict(
            factor_ids=[f['id'] for f in factors],
            scores={str(s): dict(z=round(float(z), 4), pct=round(float(pct[s]), 4)) for s, z in row.items()},
        )
        snapshot = dict(as_of=date.strftime('%Y-%m-%d'), run_id='frozen-split-sleeve-study', horizons={'3': entry})
        base_target = quant_targets.build_targets('crypto', snapshot, panels, now=date + dt.timedelta(days=1))
        m3_target = quant_targets.build_targets('crypto', snapshot, panels, now=date + dt.timedelta(days=1), tier='mega_large')
        s2_target = quant_targets.build_targets('crypto', snapshot, panels, now=date + dt.timedelta(days=1), tier='mid_small')

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
        mega_cnt = int(
            (mega_mask_all.loc[date].fillna(False) & np.isfinite(score_series) & np.isfinite(vol) & vol.gt(0)).sum()
        )
        mid_cnt = int(
            (mid_mask_all.loc[date].fillna(False) & np.isfinite(score_series) & np.isfinite(vol) & vol.gt(0)).sum()
        )

        if base_target['status'] == 'ok':
            b_df.loc[date] = pd.Series(base_target['weights'], dtype=float).reindex(columns, fill_value=0.0)
        if m3_target['status'] == 'ok':
            m3_df.loc[date] = pd.Series(m3_target['weights'], dtype=float).reindex(columns, fill_value=0.0)
        if s2_target['status'] == 'ok':
            s2_df.loc[date] = pd.Series(s2_target['weights'], dtype=float).reindex(columns, fill_value=0.0)
            if base_target['status'] != 'ok' or s2_target['weights'] != base_target['weights']:
                changed['S2_mid'] += 1
        if (m3_target['status'] == 'ok' and m3_target['weights'] != base_target.get('weights')) or (
            s2_target['status'] == 'ok' and s2_target['weights'] != base_target.get('weights')
        ):
            changed['S1_split'] += 1

        records.append(
            dict(
                date=date.strftime('%Y-%m-%d'),
                core_eligible=core_eligible,
                mega_count=mega_cnt,
                mid_count=mid_cnt,
                score_series=score_series.dropna().to_dict(),
                vol=vol.dropna().to_dict(),
                ret7=ret7.dropna().to_dict(),
                history=history.to_dict(),
            )
        )

    return (
        {
            'B': b_df.ffill().fillna(0.0),
            'M3_mega': m3_df.ffill().fillna(0.0),
            'S2_mid': s2_df.ffill().fillna(0.0),
        },
        records,
        changed,
    )


def build_placebo_pair(dates, columns, records, rng):
    """Partition core_eligible randomly into mega_n and remaining mid_n sleeves."""
    mega_df = pd.DataFrame(np.nan, index=dates, columns=columns)
    mid_df = pd.DataFrame(np.nan, index=dates, columns=columns)
    rule = quant_targets._rule('crypto')
    for rec in records:
        date = pd.Timestamp(rec['date'])
        core = rec['core_eligible']
        k_mega = min(rec['mega_count'], len(core))
        if k_mega >= 15:
            mega_sub = list(rng.choice(core, size=k_mega, replace=False))
            mega_set = set(mega_sub)
            mid_sub = [s for s in core if s not in mega_set]
        else:
            mega_sub = []
            mid_sub = list(core)
        if len(mega_sub) >= 15:
            w1 = build_subset_target(
                pd.Series(rec['score_series'], dtype=float),
                mega_sub,
                pd.Series(rec['vol'], dtype=float),
                pd.Series(rec['ret7'], dtype=float),
                pd.Series(rec['history'], dtype=float),
                rule,
            )
            if w1 is not None:
                mega_df.loc[date] = pd.Series(w1, dtype=float).reindex(columns, fill_value=0.0)
        if len(mid_sub) >= 15:
            w2 = build_subset_target(
                pd.Series(rec['score_series'], dtype=float),
                mid_sub,
                pd.Series(rec['vol'], dtype=float),
                pd.Series(rec['ret7'], dtype=float),
                pd.Series(rec['history'], dtype=float),
                rule,
            )
            if w2 is not None:
                mid_df.loc[date] = pd.Series(w2, dtype=float).reindex(columns, fill_value=0.0)
    return mega_df.ffill().fillna(0.0), mid_df.ffill().fillna(0.0)


_CTX = None


def _init_placebo(ctx):
    global _CTX
    _CTX = ctx


def _placebo_job(seed_idx: int):
    panels, hourly, segments = _CTX
    rng = np.random.default_rng(SEED + seed_idx)
    s2_diffs, split_diffs = [], []
    for dates, columns, records, base_run, rebalances in segments:
        mega_t, mid_t = build_placebo_pair(dates, columns, records, rng)
        r_mega = execute(panels, mega_t, hourly, giveback=False, daily=False, describe=False)
        r_mid = execute(panels, mid_t, hourly, giveback=False, daily=False, describe=False)
        r_split = combine_equalized_sleeves(r_mega, r_mid, rebalances)
        b_rets = simple_returns(base_run)
        s2_diffs.append(simple_returns(r_mid) - b_rets)
        split_diffs.append(simple_returns(r_split) - b_rets)
    return (
        seed_idx,
        float(pd.concat(s2_diffs).mean()),
        float(pd.concat(split_diffs).mean()),
    )


def verdict_arm(stats: dict) -> str:
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


def run_study(*, workers: int = 4, placebo: int = 200, progress=print) -> dict:
    prereg_before = verify_prereg()
    inputs = verify_snapshot(SNAPSHOT)
    panels = panel.load_panels('crypto', SNAPSHOT)
    hourly, hourly_sha256 = load_hourly(panels.mask.columns, KLINES)
    inputs['hourly_sha256'] = hourly_sha256

    scores = ensemble_scores(list(PRIMARY_FACTORS), panels)
    prepared, base_targets = {}, {}
    for name, start, stop in (('val', '2025-05-04', '2026-04-30'), ('forward', '2026-05-04', '2026-11-06')):
        dates = panels.mask.index[(panels.mask.index >= pd.Timestamp(start)) & (panels.mask.index <= pd.Timestamp(stop))]
        targets_map, records, changed = prepare_interval_targets(panels, scores, PRIMARY_FACTORS, dates)
        prepared[name] = dict(dates=dates, targets=targets_map, records=records, changed=changed)
        base_targets[name] = targets_map['B']

    validation = validate_hourly(panels, base_targets, hourly)
    if not validation['valid']:
        raise RuntimeError('Hourly validation failed: ' + json.dumps(validation))

    intervals = {}
    segments = []
    for name, item in prepared.items():
        rebalances = [d.strftime('%Y-%m-%d') for d in item['dates'][::3]]
        variants, exposure, rho = {}, {}, {}
        for variant in ('B', 'M3_mega', 'S2_mid'):
            run = execute(panels, item['targets'][variant], hourly, giveback=False, daily=False, describe=True)
            variants[variant] = run
            exposure[variant] = exposure_summary(run)
            progress(f'{name} {variant} completed', flush=True)

        variants['S1_split'] = combine_equalized_sleeves(variants['M3_mega'], variants['S2_mid'], rebalances)
        exposure['S1_split'] = {
            k: 0.5 * (exposure['M3_mega'][k] + exposure['S2_mid'][k]) for k in ('long', 'short', 'gross')
        }

        base_gross = mean_daily_gross(variants['B'])
        for arm in ('S2_mid', 'S1_split'):
            r_val = exposure[arm]['gross'] / base_gross
            rho[arm] = float(r_val)
            bx_run = execute(panels, item['targets']['B'] * r_val, hourly, giveback=False, daily=False, describe=True)
            variants[f'B_x_{arm}'] = bx_run
            exposure[f'B_x_{arm}'] = exposure_summary(bx_run)

        intervals[name] = dict(
            start=str(item['dates'][0].date()),
            end=str(item['dates'][-1].date()),
            changed_rebalances=item['changed'],
            variants=variants,
            exposure=exposure,
            rho=rho,
        )
        segments.append((item['dates'], scores.columns, item['records'], dict(daily_equity=variants['B']['daily_equity']), rebalances))

    placebo_means = {'S2_mid': [], 'S1_split': []}
    if placebo > 0:
        with ProcessPoolExecutor(max_workers=workers, initializer=_init_placebo, initargs=((panels, hourly, segments),)) as pool:
            jobs = [pool.submit(_placebo_job, i) for i in range(placebo)]
            for idx, fut in enumerate(as_completed(jobs), 1):
                _, m_mid, m_split = fut.result()
                placebo_means['S2_mid'].append(m_mid)
                placebo_means['S1_split'].append(m_split)
                if idx % 40 == 0 or idx == placebo:
                    progress(f'Placebo {idx}/{placebo}', flush=True)

    statistics, verdicts = {}, {}
    total_changed = {arm: sum(prepared[n]['changed'][arm] for n in prepared) for arm, *_ in ARMS}
    for arm, *_ in ARMS:
        st = comparison(intervals, arm, 'B')
        st['z_K'] = z_threshold(PLANNED_K)
        st['hourly_valid'] = True
        st['changed_rebalances'] = total_changed[arm]
        bx_st = comparison(intervals, arm, f'B_x_{arm}')
        st['bx_delta'] = bx_st['mean_delta']
        st['bx_z'] = bx_st['z']
        pm = placebo_means[arm]
        st['placebo_pct'] = float(np.mean(np.asarray(pm) < st['mean_delta'])) if pm else None
        statistics[arm] = st
        verdicts[arm] = verdict_arm(st)

    if verify_prereg() != prereg_before:
        raise ValueError('Preregistration changed during run')

    return dict(
        prereg=dict(path=str(PREREG.relative_to(REPO)), sha256=PREREG_SHA256),
        inputs=inputs,
        planned_K=PLANNED_K,
        z_K=z_threshold(PLANNED_K),
        placebo_n=placebo,
        intervals=intervals,
        statistics=statistics,
        verdicts=verdicts,
    )


def append_ledger(path: Path, result: dict) -> list[dict]:
    rows = []
    for arm, idea_id, k_num in ARMS:
        st = result['statistics'][arm]
        verdict = result['verdicts'][arm]

        def build(k, _prev, _st=st, _v=verdict, _idea=idea_id, _k=k_num):
            return dict(
                stage='S1',
                change_type='E',
                idea_id=_idea,
                baseline_version=BASELINE_VERSION,
                verdict=_v,
                preregistered_K=PLANNED_K,
                planned_arm_K=_k,
                actual_ledger_K=k,
                delta=_st['mean_delta'],
                se=_st['se'],
                z=_st['z'],
                val_delta=_st['val_delta'],
                forward_delta=_st['forward_delta'],
                bx_delta=_st['bx_delta'],
                placebo_pct=_st['placebo_pct'],
                prereg_path=result['prereg']['path'],
                prereg_sha256=result['prereg']['sha256'],
                report_path=result.get('report_path'),
            )

        rows.append(evolution_ledger.append_event(path, 'fq_crypto', build))
    return rows


def render_markdown(result: dict) -> str:
    def fnum(v, fmt='.4f'):
        return 'NA' if v is None else format(v, fmt)

    lines = [
        '# fq_crypto 分层独立运行与 50/50 双袖套拆分 S1 报告',
        '',
        f"- 预注册：`{result['prereg']['path']}` (`{result['prereg']['sha256'][:12]}...`)",
        f"- 保守强阈值：`z_K(14) = {result['z_K']:.4f}`；`placebo_n = {result['placebo_n']}`。",
        '',
        '## 判定汇总（相对现行 7 因子 `B = tier:all`）',
        '',
        '| 变体 | 结论 | 拼接 Δ | val Δ | forward Δ | z | vs B_x Δ | 安慰剂分位 |',
        '|---|---|---:|---:|---:|---:|---:|---:|',
    ]
    for arm, *_ in ARMS:
        st = result['statistics'][arm]
        lines.append(
            f"| {arm} | **{result['verdicts'][arm]}** | {fnum(st['mean_delta'], '.6f')} | "
            f"{fnum(st['val_delta'], '.6f')} | {fnum(st['forward_delta'], '.6f')} | "
            f"{fnum(st['z'], '+.2f')} | {fnum(st['bx_delta'], '.6f')} | {fnum(st['placebo_pct'], '.1%')} |"
        )
    lines += [
        '',
        '## 各段明细',
        '',
        '| 区间 | 组别 | 累计收益 % | Sharpe | 最大回撤 % | 最差3日 % | 日均总敞口 |',
        '|---|---|---:|---:|---:|---:|---:|',
    ]
    for name in ('val', 'forward'):
        iv = result['intervals'][name]
        for variant in ('B', 'M3_mega', 'S2_mid', 'S1_split'):
            m = iv['variants'][variant]['metrics']
            exp = iv['exposure'][variant]['gross']
            lines.append(
                f"| {name} | {variant} | {m['return_pct']:+.2f}% | {m['sharpe']:.3f} | "
                f"{m['maxdd_pct']:.2f}% | {m['worst_3d_return_pct']:+.2f}% | {exp:.3f} |"
            )
    return '\n'.join(lines) + '\n'


def _to_json_builtin(obj):
    if isinstance(obj, dict):
        return {str(k): _to_json_builtin(v) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [_to_json_builtin(v) for v in obj]
    if isinstance(obj, (np.bool_,)):
        return bool(obj)
    if isinstance(obj, (np.integer,)):
        return int(obj)
    if isinstance(obj, (np.floating,)):
        val = float(obj)
        return val if np.isfinite(val) else ('inf' if val > 0 else '-inf' if val < 0 else None)
    if isinstance(obj, np.ndarray):
        return [_to_json_builtin(v) for v in obj.tolist()]
    if isinstance(obj, pd.Timestamp):
        return str(obj)
    return evolution_ledger.json_safe(obj)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--workers', type=int, default=4)
    parser.add_argument('--placebo', type=int, default=200)
    parser.add_argument('--write-ledger', action='store_true')
    args = parser.parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)
    result = run_study(workers=args.workers, placebo=args.placebo)
    result['report_path'] = str(args.out / 'report.json')
    if args.write_ledger:
        result['ledger_rows'] = append_ledger(REPO / 'data/evolution/ledger.jsonl', result)
    safe = _to_json_builtin(result)
    (args.out / 'report.json').write_text(json.dumps(safe, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    (args.out / 'report.md').write_text(render_markdown(result), encoding='utf-8')
    for arm, *_ in ARMS:
        st = result['statistics'][arm]
        print(f"{arm}: verdict={result['verdicts'][arm]} z={st['z']:+.3f} val_d={st['val_delta']:+.6f} fwd_d={st['forward_delta']:+.6f} placebo={st['placebo_pct']}")
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
