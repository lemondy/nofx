"""Frozen crypto_llm pool & size-tier split S1 study (C1, C2, C3 vs mac-nofx).

Run: python -m scripts.backtest.crypto_llm_tier_split_study --out /tmp/crypto_llm_tier
Database access is strictly read-only.
"""
from __future__ import annotations

import argparse
from contextlib import closing
import hashlib
import json
from pathlib import Path
import sqlite3

import numpy as np
import pandas as pd

from scripts.factor_mining.panel import _read_symbol, load_panels
from . import evolution_ledger
from .incremental_filter import (
    decide_verdict,
    filter_statistics,
    read_closed_trades,
    weekly_statistics,
    z_threshold,
)

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-15-crypto-llm-tier-filter-s1-prereg.md'
PREREG_SHA256 = '7894940951ac402dea3196373efdb63a07c510d280fe8bce1b86987b0bc18474'
SEED = 20261103
PLACEBO_N = 2000
PLANNED_K = 9
MEGA_ADV20_USD = 100_000_000.0
BASELINE_VERSION = 'd7ff3764_conservative_ai500_piggy_shortscan'

MAC_NOFX_TRADERS = [
    'd7ff3764_f6b845ba-f7b2-4b27-8501-fcf6af94f276_deepseek_1787982996',
]
POOLED_5_TRADERS = MAC_NOFX_TRADERS + [
    'a8eb1bdf_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_273a584a-0c4d-43cf-9ef9-758a7c1d5bea',
    'a2dc5705_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_89bba6d6-adab-4eed-ad8b-60cfb1cfac2a',
    'ec830bb2_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_bde3f7ab-001a-4dc6-9cfc-1acbb09f287d',
    '895dc8b3_76fc_444b-b392_68941e73041c_glm_82245a32-c08d-4678-b0bd-f5cbb1003b02',
]
ARMS = (
    ('C1', 'crypto-llm-verified-crypto-pool-v1', 7, 'B'),
    ('C2', 'crypto-llm-core100-pool-v1', 8, 'B'),
    ('C3', 'crypto-llm-mega-large-tier-v1', 9, 'E'),
)


def verify_prereg() -> bytes:
    raw = PREREG.read_bytes()
    if hashlib.sha256(raw).hexdigest() != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run')
    return raw


def build_causal_adv20_tables(factor_root: Path) -> tuple[set[str], dict[str, pd.Series]]:
    """Build causal 20-day close*volume tables from crypto/daily + crypto/ext/daily."""
    panels = load_panels('crypto', str(factor_root))
    cd_syms = {str(s) for s in panels.fields['close'].columns}
    adv_tables: dict[str, pd.Series] = {}
    for sym in cd_syms:
        dv = (panels.fields['close'][sym] * panels.fields['volume'][sym]).dropna()
        adv_tables[sym] = dv.rolling(20, min_periods=20).mean().dropna()
    ext_dir = factor_root / 'crypto' / 'ext' / 'daily'
    if ext_dir.exists():
        for path in sorted(ext_dir.glob('*.json')):
            sym = path.stem
            if sym in adv_tables:
                continue
            df = _read_symbol(path)
            if df.empty:
                continue
            dv = (df['close'] * df['volume']).dropna()
            adv_tables[sym] = dv.rolling(20, min_periods=20).mean().dropna()
    return cd_syms, adv_tables


def classify_trade_causal(
    symbol: str,
    entry_time: str,
    cd_syms: set[str],
    adv_tables: dict[str, pd.Series],
) -> dict:
    """Strictly causal tier classification using daily bars <= (entry_date_utc - 1 day)."""
    sig_day = pd.Timestamp(entry_time).tz_convert('UTC').tz_localize(None).normalize() - pd.Timedelta(days=1)
    in_cd = symbol in cd_syms
    adv_val = None
    series = adv_tables.get(symbol)
    if series is not None and not series.empty:
        prior = series.loc[:sig_day]
        if not prior.empty:
            val = float(prior.iloc[-1])
            if np.isfinite(val):
                adv_val = val
    if adv_val is None:
        tier = 'non_crypto_or_unmapped'
    elif adv_val >= MEGA_ADV20_USD:
        tier = 'mega_large'
    else:
        tier = 'mid_small'
    return dict(
        sig_day=str(sig_day.date()),
        in_cd=in_cd,
        has_adv20=(adv_val is not None),
        adv20=adv_val,
        tier=tier,
    )


def tier_breakdown(trades: list[dict], annotations: dict[str, dict]) -> dict:
    """Compute descriptive stats (n, avg_r, weekly SE, 95% CI, net_pnl) per tier."""
    from scripts.promotion import report as promo_report

    groups: dict[str, list[dict]] = {
        'all': list(trades),
        'mega_large': [],
        'mid_small': [],
        'non_crypto_or_unmapped': [],
        'in_cd': [],
        'out_cd': [],
    }
    for row in trades:
        ann = annotations[str(row['trade_id'])]
        groups[ann['tier']].append(row)
        groups['in_cd' if ann['in_cd'] else 'out_cd'].append(row)

    out = {}
    for key, subset in groups.items():
        samples = [(promo_report.as_utc(r['entry_time']), r['r']) for r in subset]
        ws = weekly_statistics(samples) if subset else {'n': 0, 'avg_r': None, 'n_weeks': 0, 'se': None, 'ci95': None}
        total_r = float(sum(r['r'] for r in subset))
        net_pnl = float(sum(r['net_pnl_usdt'] for r in subset))
        win_rate = float(sum(1 for r in subset if r['r'] > 0) / len(subset)) if subset else None
        out[key] = {
            **ws,
            'win_rate': win_rate,
            'total_r': total_r,
            'net_pnl_usdt': net_pnl,
        }
    return out


def run_cohort(
    trades: list[dict],
    cd_syms: set[str],
    adv_tables: dict[str, pd.Series],
    *,
    seed: int = SEED,
    placebo_n: int = PLACEBO_N,
) -> dict:
    annotations = {
        str(row['trade_id']): classify_trade_causal(row['symbol'], row['entry_time'], cd_syms, adv_tables)
        for row in trades
    }
    masks = {
        'C1': {tid: not ann['has_adv20'] for tid, ann in annotations.items()},
        'C2': {tid: not ann['in_cd'] for tid, ann in annotations.items()},
        'C3': {tid: ann['tier'] != 'mega_large' for tid, ann in annotations.items()},
    }
    arms = {}
    for code, idea_id, k_num, change_type in ARMS:
        st = filter_statistics(trades, masks[code], seed=seed, placebo_n=placebo_n)
        st['z_K'] = z_threshold(PLANNED_K)
        st['idea_id'] = idea_id
        st['k_num'] = k_num
        st['change_type'] = change_type
        st['verdict'] = decide_verdict(st, PLANNED_K)
        arms[code] = st

    breakdown = tier_breakdown(trades, annotations)
    return dict(
        n_trades=len(trades),
        start=trades[0]['entry_time'] if trades else None,
        end=trades[-1]['entry_time'] if trades else None,
        arms=arms,
        breakdown=breakdown,
    )


def run_study(db_path: Path | None = None, factor_root: Path | None = None) -> dict:
    verify_prereg()
    db = db_path or (REPO / 'data/data.db')
    froot = factor_root or (REPO / 'data/factor_mining')
    cd_syms, adv_tables = build_causal_adv20_tables(froot)

    with closing(sqlite3.connect(f'file:{db}?mode=ro', uri=True)) as conn:
        conn.row_factory = sqlite3.Row
        primary_trades, primary_meta = read_closed_trades(conn, traders=MAC_NOFX_TRADERS)
        pooled_trades, pooled_meta = read_closed_trades(conn, traders=POOLED_5_TRADERS)

    primary = run_cohort(primary_trades, cd_syms, adv_tables)
    pooled = run_cohort(pooled_trades, cd_syms, adv_tables)
    return dict(
        prereg_path=str(PREREG.relative_to(REPO)),
        prereg_sha256=PREREG_SHA256,
        planned_K=PLANNED_K,
        seed=SEED,
        placebo_n=PLACEBO_N,
        baseline_version=BASELINE_VERSION,
        primary_meta=primary_meta,
        pooled_meta=pooled_meta,
        primary=primary,
        pooled=pooled,
    )


def append_ledger(path: Path, report: dict) -> list[dict]:
    rows = []
    primary_arms = report['primary']['arms']
    for code, idea_id, k_num, change_type in ARMS:
        st = primary_arms[code]

        def build(k, _previous, _st=st, _idea=idea_id, _k=k_num, _ct=change_type):
            return dict(
                stage='S1',
                change_type=_ct,
                idea_id=_idea,
                baseline_version=BASELINE_VERSION,
                verdict=_st['verdict'],
                preregistered_K=PLANNED_K,
                planned_arm_K=_k,
                actual_ledger_K=k,
                delta=_st['delta'],
                se=_st['se'],
                z=_st['z'],
                trade_ratio=_st['trade_ratio'],
                placebo_pct=_st['placebo_pct'],
                dev_delta=_st['dev_delta'],
                holdout_delta=_st['holdout_delta'],
                all_avg_r=_st['all_avg_r'],
                kept_avg_r=_st['kept_avg_r'],
                dropped_avg_r=_st['dropped_avg_r'],
                n=_st['n'],
                dropped_n=_st['dropped_n'],
                prereg_path=report['prereg_path'],
                prereg_sha256=report['prereg_sha256'],
                report_path=report.get('report_path'),
            )

        rows.append(evolution_ledger.append_event(path, 'crypto_llm', build))
    return rows


def render_markdown(report: dict) -> str:
    def fnum(val, fmt='.4f'):
        return '无' if val is None else format(val, fmt)

    lines = [
        '# crypto_llm 候选币池与市值分层过滤 S1 报告',
        '',
        f"- 预注册：`{report['prereg_path']}` (`{report['prereg_sha256'][:12]}...`)",
        f"- 主样本：`mac-nofx` 实盘（合法 R 交易 `n={report['primary']['n_trades']}` / 总平仓 `{report['primary_meta']['total']}`，窗口 `{report['primary']['start'][:10]}..{report['primary']['end'][:10]}`）",
        f"- 次级交叉样本：5 个同池加密 LLM 交易员汇总（`n={report['pooled']['n_trades']}`，窗口 `{report['pooled']['start'][:10]}..{report['pooled']['end'][:10]}`）",
        f"- 保守强阈值：`z_K(9) = {z_threshold(PLANNED_K):.4f}`；`placebo_n = {report['placebo_n']}`。",
        '',
        '## 主样本（mac-nofx）判定结果（相对全池 `all_avg_r`）',
        '',
        '| 变体 | 过滤规则 | 保留/总笔数 | 剔除率 p | delta (avgR) | SE | z | 安慰剂分位 | dev_delta | holdout_delta | 结论 |',
        '|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|',
    ]
    labels = {
        'C1': '仅保留有 20d 日线历史加密币',
        'C2': '仅保留 crypto/daily 100 币池',
        'C3': '仅保留 mega_large (ADV20 >= $100M)',
    }
    for cohort_name, cohort in (('主样本 mac-nofx', report['primary']), ('次级样本 pooled-5', report['pooled'])):
        if cohort_name != '主样本 mac-nofx':
            lines += [
                '',
                f'## {cohort_name}（参考对照）',
                '',
                '| 变体 | 过滤规则 | 保留/总笔数 | 剔除率 p | delta (avgR) | SE | z | 安慰剂分位 | dev_delta | holdout_delta | 结论 |',
                '|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|',
            ]
        for code, *_ in ARMS:
            st = cohort['arms'][code]
            kept_n = st['n'] - st['dropped_n']
            lines.append(
                f"| {code} | {labels[code]} | {kept_n}/{st['n']} | {st['p']:.1%} | "
                f"{fnum(st['delta'], '+.4f')} | {fnum(st['se'])} | {fnum(st['z'], '+.2f')} | "
                f"{fnum(st['placebo_pct'], '.1%')} | {fnum(st['dev_delta'], '+.4f')} | "
                f"{fnum(st['holdout_delta'], '+.4f')} | **{st['verdict']}** |"
            )

        lines += [
            '',
            f'### {cohort_name} 分层独立表现（周聚类 95% CI）',
            '',
            '| 分层 | 笔数 | 胜率 | 笔均 R | 周聚类 SE | 95% CI | 累计 R | 净盈亏 USDT |',
            '|---|---:|---:|---:|---:|---|---:|---:|',
        ]
        for tier_key, tier_label in (
            ('all', '全池（现行基线）'),
            ('mega_large', '加密大盘 mega_large (ADV20 >= $100M)'),
            ('mid_small', '加密中小盘 mid_small (ADV20 < $100M)'),
            ('non_crypto_or_unmapped', '非加密/无 20d 历史微盘'),
            ('in_cd', 'crypto/daily 100 币面板内'),
            ('out_cd', 'crypto/daily 100 币面板外'),
        ):
            b = cohort['breakdown'][tier_key]
            ci = b['ci95']
            ci_str = f"[{ci[0]:+.4f}, {ci[1]:+.4f}]" if ci else '无'
            lines.append(
                f"| {tier_label} | {b['n']} | {fnum(b['win_rate'], '.1%')} | {fnum(b['avg_r'], '+.4f')} | "
                f"{fnum(b['se'])} | {ci_str} | {b['total_r']:+.2f} | {b['net_pnl_usdt']:+.2f} |"
            )

    return '\n'.join(lines) + '\n'


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--write-ledger', action='store_true')
    args = parser.parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)
    report = run_study()
    report['report_path'] = str(args.out / 'report.json')
    if args.write_ledger:
        report['ledger_rows'] = append_ledger(REPO / 'data/evolution/ledger.jsonl', report)
    safe = evolution_ledger.json_safe(report)
    (args.out / 'report.json').write_text(json.dumps(safe, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    (args.out / 'report.md').write_text(render_markdown(report), encoding='utf-8')
    for code, *_ in ARMS:
        st = report['primary']['arms'][code]
        print(
            f"{code}: delta={st['delta']:+.4f} z={st['z']:+.2f} p={st['p']:.1%} "
            f"placebo={st['placebo_pct']:.1%} verdict={st['verdict']}"
        )
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
