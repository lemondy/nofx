"""Frozen S1 type-B screen: AI-attributed longs vetoed by weak F1 ranks.

Run the preregistered dry run with --no-ledger before the official run.
SQLite is read-only; only the output directory and optional ledger are written.
"""
from __future__ import annotations

import argparse
from bisect import bisect_right
from collections import Counter, defaultdict
from contextlib import closing
from datetime import datetime, timedelta, timezone
import json
import math
from pathlib import Path
import sqlite3
import sys
import time

import numpy as np
import pandas as pd

from scripts.backtest import evolution_ledger, incremental_filter as filters
from . import config, ensemble, panel, quant_targets, universe_expansion_study
from .pit60_hedge_study import (END_BT, FORWARD_START, sha256, snapshot_root,
                                truncate_panels)
from .wide_pool_study import ic_summary, json_safe, number, panel_digest, rank_ic

REPO = Path(__file__).resolve().parents[2]
PREREG = 'docs/research/2026-11-04-llm-long-factor-veto-prereg.md'
DATA = Path('/Users/zhangyun/workspace/nofx/data')
FULL_DIR = DATA / 'factor_mining/crypto_full'
BASE_DIR = DATA / 'factor_mining/crypto'
DB = DATA / 'data.db'
OUT = FULL_DIR / 'study/llm_long_veto'
DEFAULT_LEDGER = DATA.parent / evolution_ledger.DEFAULT_PATH
FROZEN = {
    'prereg_20261104b/f1_selected.json': '68bfeffe293f11f8ba373e58362d310ba900f9c6168daa236535e805a0dacf04',
    'study/pit60_btchedge/report.json': '615d8e3e9d04de69465056a5363b3c65fc038eefb8b82520c13143bab2ab7cab',
    'prereg_20261104/ensemble.json': 'f53fb2a5c15f5cf246b251350cc365bf5b862b756c8b952fd220c31af1870eb0',
    'validation.json': '5c0fdc770abff256f2ccce92e8ba25a5a84bdf596abd99ed9db6ebce6dc1e2a5',
}
TRADERS = [
    'd7ff3764_f6b845ba-f7b2-4b27-8501-fcf6af94f276_deepseek_1787982996',
    'a8eb1bdf_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_273a584a-0c4d-43cf-9ef9-758a7c1d5bea',
    'ec830bb2_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_bde3f7ab-001a-4dc6-9cfc-1acbb09f287d',
    '895dc8b3_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_82245a32-c08d-4678-b0bd-f5cbb1003b02',
]
UNTIL = datetime(2026, 11, 1, tzinfo=timezone.utc)
THRESHOLD, ALT_THRESHOLDS, SHORT_THRESHOLD = .20, (.10, .30), .80
ATTRIB_WINDOW_MS = 24 * 3600 * 1000
MIN_SCORED_SHARE = .80
IDEA_ID, TRACK, CHANGE_TYPE, STAGE = 'llm-long-veto-f1-q20-v1', 'crypto_llm', 'B', 'S1'
BASELINE_VERSION = 'crypto_llm_hist_20260917_20261031'
SEED, PLACEBO_N = 20261103, 2000
HYPOTHESIS = 'LLM 加密交易员：因子弱币做多否决（B 类）'
OPEN_ACTIONS = {'open_long': 'long', 'open_long_limit': 'long',
                'open_short': 'short', 'open_short_limit': 'short'}


def guard_inputs(full_dir):
    actual = {name: sha256(Path(full_dir) / name) for name in FROZEN}
    drift = {name: dict(expected=FROZEN[name], actual=value)
             for name, value in actual.items() if value != FROZEN[name]}
    if drift:
        raise ValueError(f'frozen input sha256 mismatch: {drift}')
    return actual


def timestamp_ns(value):
    """Keep nanoseconds, including decisions just after the entry boundary."""
    timestamp = pd.Timestamp(value)
    if pd.isna(timestamp):
        raise ValueError('missing timestamp')
    timestamp = (timestamp.tz_localize('UTC') if timestamp.tzinfo is None
                 else timestamp.tz_convert('UTC'))
    return timestamp.value


def read_decisions(conn, trader):
    index = defaultdict(list)
    counts = Counter(records=0, skipped_json=0, invalid_decisions=0, invalid_timestamps=0)
    for record in conn.execute('SELECT timestamp, decisions FROM decision_records WHERE trader_id=?',
                               (trader,)):
        counts['records'] += 1
        try:
            decisions = json.loads(record['decisions'])
        except (ValueError, TypeError):
            counts['skipped_json'] += 1
            continue
        if not isinstance(decisions, list):
            counts['skipped_json'] += 1
            continue
        for decision in decisions:
            if not isinstance(decision, dict):
                counts['invalid_decisions'] += 1
                continue
            action = decision.get('action')
            side = OPEN_ACTIONS.get(action) if isinstance(action, str) else None
            if side is None or decision.get('success') is not True:
                continue
            symbol = decision.get('symbol')
            if not isinstance(symbol, str) or not symbol:
                counts['invalid_decisions'] += 1
                continue
            try:
                stamp = timestamp_ns(decision.get('timestamp') or record['timestamp'])
            except (ValueError, TypeError, OverflowError):
                counts['invalid_timestamps'] += 1
                continue
            index[side, symbol].append(stamp)
    return {key: sorted(values) for key, values in index.items()}, dict(counts)


def ai_attributed(trade, decisions):
    stamps = decisions.get((trade['side'].lower(), trade['symbol']), [])
    entered = timestamp_ns(trade['entry_time'])
    position = bisect_right(stamps, entered) - 1
    return position >= 0 and entered - stamps[position] <= ATTRIB_WINDOW_MS * 1_000_000


def read_samples(conn):
    longs, shorts, summaries, exports = [], [], {}, {}
    for trader in TRADERS:
        rows, exports[trader] = filters.read_closed_trades(conn, [trader], until=UNTIL)
        decisions, parse_counts = read_decisions(conn, trader)
        summary = dict(trader_id=trader, decisions=parse_counts)
        for side, sample in (('long', longs), ('short', shorts)):
            selected = [row for row in rows if row['side'].lower() == side]
            attributed = [row for row in selected if ai_attributed(row, decisions)]
            ids = {str(row['trade_id']) for row in attributed}
            excluded = [row for row in selected if str(row['trade_id']) not in ids]
            sample.extend(attributed)
            summary[side] = dict(total=len(selected), attributed=len(attributed), excluded=len(excluded),
                                 excluded_symbols=dict(Counter(row['symbol'] for row in excluded)))
        summaries[trader[:8]] = summary
    return longs, shorts, summaries, exports


def percentiles(score, panels):
    return score.where(panels.mask).replace([np.inf, -np.inf], np.nan).rank(axis=1, pct=True)


def score_trades(trades, pct, panels):
    annotated = []
    for trade in trades:
        date = pd.Timestamp(trade['entry_time']).tz_convert('UTC').tz_localize(None).normalize()
        date -= timedelta(days=1)
        symbol, reason, value = trade['symbol'], None, None
        if symbol not in panels.mask.columns or symbol not in pct.columns:
            reason = 'not_in_panel'
        elif date not in panels.mask.index or date not in pct.index:
            reason = 'no_row'
        elif not panels.mask.at[date, symbol]:
            reason = 'not_in_mask'
        elif not np.isfinite(pct.at[date, symbol]):
            reason = 'nonfinite'
        else:
            value = float(pct.at[date, symbol])
        annotated.append(dict(trade, pct=value, scored=reason is None, unscored_reason=reason,
                              score_date=str(date.date())))
    return annotated


def veto_mask(trades, threshold=THRESHOLD, *, short=False):
    return {str(row['trade_id']): bool(row['scored'] and
            (row['pct'] >= threshold if short else row['pct'] <= threshold)) for row in trades}


def compare_rank_ic(actual, expected, tolerance=1e-9):
    a, b = actual.get('daily', {}), expected.get('daily', {})
    missing, extra = sorted(b.keys() - a.keys()), sorted(a.keys() - b.keys())
    errors, invalid = [], []
    for name, left, right in [('mean', actual.get('mean'), expected.get('mean'))] + [
            (day, a[day], b[day]) for day in sorted(a.keys() & b.keys())]:
        if left is None or right is None or not np.isfinite(left) or not np.isfinite(right):
            invalid.append(name)
        else:
            errors.append(abs(left - right))
    maximum = max(errors, default=None)
    matched = not (missing or extra or invalid) and maximum is not None and maximum <= tolerance
    return dict(matched=matched, tolerance=tolerance, max_abs_error=maximum,
                missing_days=missing, extra_days=extra, nonfinite=invalid,
                actual=actual, expected=expected)


def reproduce_rank_ic(factors, full60, expected):
    # Exact pit60_hedge_study RankIC path: score before truncating labels/mask.
    score = ensemble.ensemble_scores(factors, full60)
    truncated = truncate_panels(full60)
    excess, _ = panel.forward_labels(truncated, 3, FORWARD_START)
    dates = truncated.mask.index[truncated.mask.index >= pd.Timestamp(FORWARD_START)]
    actual = ic_summary(rank_ic(score.loc[dates], excess.loc[dates], truncated.mask.loc[dates]))
    return compare_rank_ic(actual, expected), truncated


def check_export(conn, trades):
    mismatches, counts = [], {}
    for trader in TRADERS:
        rows, counts[trader] = filters.read_closed_trades(conn, [trader])
        by_id = {str(row['trade_id']): row for row in rows}
        for trade in trades:
            if trade['trader_id'] != trader:
                continue
            key = str(trade['trade_id'])
            if key not in by_id or trade['r'] != by_id[key]['r']:
                mismatches.append(dict(trade_id=trade['trade_id'], sample_r=trade['r'],
                                       export_r=by_id[key]['r'] if key in by_id else None))
    return dict(matched=not mismatches, mismatches=mismatches, export_counts=counts)


def coverage(trades):
    n = len(trades)
    scored = sum(row['scored'] for row in trades)
    return dict(n=n, scored_n=scored, unscored_n=n - scored, scored_share=scored / n if n else 0.)


def ledger_result(stats, s0, trades_hash, mask_hash, prereg_hash, ledger_path, no_ledger):
    def build(_k, previous):
        k = evolution_ledger.next_k(previous, TRACK)
        verdict = 'invalid' if not s0['ok'] else filters.decide_verdict(stats, k)
        return {**stats, 'stage': STAGE, 'change_type': CHANGE_TYPE, 'track': TRACK,
                'baseline_version': BASELINE_VERSION, 'idea_id': IDEA_ID, 'hypothesis': HYPOTHESIS,
                'verdict': verdict, 'K': None if verdict == 'invalid' else k,
                'attempted_K': k,
                'z_K': filters.z_threshold(k), 'seed': SEED, 'placebo_n': PLACEBO_N,
                'trades_sha256': trades_hash, 'mask_sha256': mask_hash,
                'prereg': PREREG, 'prereg_sha256': prereg_hash,
                's0_ok': s0['ok'], 's0_reasons': s0['reasons'],
                'scored_n': s0['coverage']['scored_n'], 'unscored_n': s0['coverage']['unscored_n'],
                'threshold': THRESHOLD}
    if no_ledger:
        return build(None, evolution_ledger.read_rows(ledger_path))
    return evolution_ledger.append_event(ledger_path, TRACK, build)


def group_statistics(trades):
    stats = filters.weekly_statistics([(filters.report.as_utc(row['entry_time']), row['r'])
                                       for row in trades])
    return dict(stats, total_r=math.fsum(row['r'] for row in trades))


def split_statistics(trades, mask):
    return {name: group_statistics([row for row in trades if select(mask[str(row['trade_id'])])])
            for name, select in (('all', lambda _: True), ('kept', lambda drop: not drop),
                                 ('dropped', lambda drop: drop))}


def descriptive_filter(trades, threshold=THRESHOLD, *, short=False):
    stats = filters.filter_statistics(trades, veto_mask(trades, threshold, short=short),
                                     seed=SEED, placebo_n=PLACEBO_N)
    return dict(stats, threshold=threshold, verdict='descriptive_only', coverage=coverage(trades))


def vetoed_features(trades, mask, panels):
    selected = [row for row in trades if mask[str(row['trade_id'])]]
    symbols = sorted({row['symbol'] for row in selected})
    close = panels.fields['close'][symbols]
    amount = (panels.fields['volume'][symbols] * panels.fields['vwap'][symbols]).rolling(20).median()
    volatility = np.log(close).diff().rolling(30).std()
    first = {symbol: close[symbol].first_valid_index() for symbol in symbols}
    results = []
    for row in selected:
        date, symbol = pd.Timestamp(row['score_date']), row['symbol']
        age = (date - first[symbol]).days if first[symbol] is not None else None
        median, vol = amount.at[date, symbol], volatility.at[date, symbol]
        results.append(dict(trade_id=row['trade_id'], symbol=symbol, score_date=row['score_date'],
                            median_amount_20d=float(median) if np.isfinite(median) else None,
                            log_return_std_30d=float(vol) if np.isfinite(vol) else None,
                            listing_age_days=age, listing_under_90d=age < 90 if age is not None else None))
    return results


def descriptives(trades, shorts, pct, full, f2_panels):
    mask = veto_mask(trades)
    quintiles = []
    for i in range(5):
        rows = [row for row in trades if row['scored'] and i / 5 < row['pct'] <= (i + 1) / 5]
        quintiles.append(dict(bin=f'({i / 5:.1f},{(i + 1) / 5:.1f}]',
                              **group_statistics(rows)))
    unscored = [row for row in trades if not row['scored']]
    reasons = Counter((row['symbol'], row['unscored_reason']) for row in unscored)
    return dict(
        split=split_statistics(trades, mask),
        per_trader={trader[:8]: split_statistics([row for row in trades if row['trader_id'] == trader], mask)
                    for trader in TRADERS},
        quintiles=quintiles,
        alternative_thresholds={str(t): descriptive_filter(trades, t) for t in ALT_THRESHOLDS},
        f2={name: descriptive_filter(score_trades(trades, values, p))
            for name, (values, p) in f2_panels.items()},
        short_veto=descriptive_filter(score_trades(shorts, pct, full), SHORT_THRESHOLD, short=True),
        vetoed_features=vetoed_features(trades, mask, full),
        unscored=dict(group_statistics(unscored), symbols=[dict(symbol=symbol, reason=reason, n=n)
                     for (symbol, reason), n in sorted(reasons.items())]))


def write_jsonl(path, rows):
    Path(path).write_text(''.join(json.dumps(json_safe(row), ensure_ascii=False, allow_nan=False,
                                          separators=(',', ':')) + '\n' for row in rows), encoding='utf-8')


def previous_run(out):
    if not (out / 'report.json').exists():
        return None
    report = json.loads((out / 'report.json').read_text(encoding='utf-8'))
    trades, digest = filters.read_jsonl(out / 'trades.jsonl')
    return dict(report=report, trades=trades, sha256=digest)


def compare_previous(previous, trades, digest, stats):
    if previous is None:
        return None
    old = {str(row['trade_id']): row for row in previous['trades']}
    new = {str(row['trade_id']): row for row in trades}
    return dict(previous_wrote_ledger=previous['report']['ledger']['written'],
                previous_trades_sha256=previous['sha256'], trades_sha256=digest,
                trades_identical=digest == previous['sha256'],
                stats_identical=all(json_safe(stats[k]) == previous['report']['primary'][k] for k in stats),
                added_trade_ids=sorted(new.keys() - old.keys()),
                removed_trade_ids=sorted(old.keys() - new.keys()),
                changed_trade_ids=sorted(k for k in new.keys() & old.keys() if new[k] != old[k]))


def run_study(db=DB, full_dir=FULL_DIR, base_dir=BASE_DIR, snapshot_dir=None, out=OUT,
              ledger_path=DEFAULT_LEDGER, *, no_ledger=False, progress=lambda _: None):
    started = time.monotonic()
    full_dir, base_dir, out = Path(full_dir), Path(base_dir), Path(out)
    snapshot_dir = Path(snapshot_dir) if snapshot_dir is not None else full_dir / 'prereg_20261104'
    # Always guard before any panel load, database open, or output mutation.
    hashes = guard_inputs(full_dir)
    prereg_hash = sha256(REPO / PREREG)
    previous = previous_run(out)
    factors = json.loads((full_dir / 'prereg_20261104b/f1_selected.json').read_text())['factors']
    expected = json.loads((full_dir / 'study/pit60_btchedge/report.json').read_text())['rank_ic']['UH']
    progress('冻结输入校验通过；只读导出交易与 AI 归因')
    with closing(sqlite3.connect(Path(db).resolve().as_uri() + '?mode=ro', uri=True)) as conn:
        conn.row_factory = sqlite3.Row
        longs, shorts, attribution, exports = read_samples(conn)
        export_check = check_export(conn, longs)
    progress(f'AI 归因样本：做多 {len(longs)}，做空 {len(shorts)}；加载全量面板')
    # Fix only in-process panel settings; _panels restores universe_n itself.
    cfg = config.MARKETS['crypto']
    old_history, old_n = cfg['min_history'], cfg['universe_n']
    try:
        cfg['min_history'] = 60
        full_all = universe_expansion_study._panels(full_dir, None)
        score = ensemble.ensemble_scores(factors, full_all)
        full = truncate_panels(full_all)
        pct = percentiles(score, full_all).loc[full.mask.index]
        digests = dict(full_all=panel_digest(full_all), full_cutoff=panel_digest(full))
        trades = score_trades(longs, pct, full)
        progress('S0：全量时点 top 60 RankIC 逐日复现')
        full60 = universe_expansion_study._panels(full_dir, 60)
        rank_check, truncated60 = reproduce_rank_ic(factors, full60, expected)
        digests.update(full60=panel_digest(full60), full60_cutoff=panel_digest(truncated60))
        progress('描述统计：F2 在基础 top 60 与全量面板打分')
        with snapshot_root(base_dir, snapshot_dir) as root:
            f2, frozen = quant_targets._load_frozen(root)
            if frozen['ensemble.json']['sha256'] != hashes['prereg_20261104/ensemble.json']:
                raise ValueError('frozen F2 ensemble sha256 mismatch')
            cfg['universe_n'] = 60
            base = panel.load_panels('crypto', root)
        base_cutoff = truncate_panels(base)
        digests.update(base60=panel_digest(base), base60_cutoff=panel_digest(base_cutoff))
        f2_panels = dict(base60=(percentiles(ensemble.ensemble_scores(f2, base), base).loc[base_cutoff.mask.index],
                               base_cutoff),
                         full_all=(percentiles(ensemble.ensemble_scores(f2, full_all), full_all).loc[full.mask.index],
                                   full))
    finally:
        cfg['min_history'], cfg['universe_n'] = old_history, old_n
    cov = coverage(trades)
    reasons = []
    if not rank_check['matched']:
        reasons.append('F1 RankIC 均值或逐日值未在 1e-9 内复现（或日期集合不同）')
    if cov['scored_share'] < MIN_SCORED_SHARE:
        reasons.append('有分数的做多样本占比不足 80%')
    if not export_check['matched']:
        reasons.append('交易导出 R 与未加 until 的逐笔导出不一致')
    s0 = dict(ok=not reasons, reasons=reasons, rank_ic=rank_check, coverage=cov, export=export_check)
    descriptive = descriptives(trades, shorts, pct, full, f2_panels)
    # Refuse input drift during computation as well as at startup.
    if guard_inputs(full_dir) != hashes or sha256(REPO / PREREG) != prereg_hash:
        raise ValueError('frozen inputs or prereg changed during study')
    for name in ('ensemble.json', 'zoo.json'):
        if sha256(snapshot_dir / name) != frozen[name]['sha256']:
            raise ValueError(f'frozen F2 {name} changed during study')
    out.mkdir(parents=True, exist_ok=True)
    write_jsonl(out / 'trades.jsonl', trades)
    mask = veto_mask(trades)
    write_jsonl(out / 'mask.jsonl', [dict(trade_id=row['trade_id'], drop=mask[str(row['trade_id'])])
                                   for row in trades])
    exported_trades, trades_hash = filters.read_jsonl(out / 'trades.jsonl')
    masks, mask_hash = filters.read_jsonl(out / 'mask.jsonl')
    stats = filters.filter_statistics(exported_trades, {str(row['trade_id']): row['drop'] for row in masks},
                                     seed=SEED, placebo_n=PLACEBO_N)
    comparison = compare_previous(previous, exported_trades, trades_hash, stats)
    progress(f'S0={s0["ok"]}；主检验完成；' + ('不写账本' if no_ledger else '追加 S1 账本'))
    row = ledger_result(stats, s0, trades_hash, mask_hash, prereg_hash, ledger_path, no_ledger)
    deviations = [
        '因果分数在未截断面板上计算，随后用于交易查值、特征和 RankIC 标签的面板均截断至 '
        f'{END_BT}；不使用截止日之后的标签或交易。',
        '特征按日历行滚动：20 日成交额中位数和 30 日对数收益样本标准差（ddof=1），'
        '分别要求 20 和 30 个有效观测；上市年龄按全量面板首个非空 close 的日历日差计算。',
    ]
    if row['attempted_K'] != 1:
        deviations.append(f'账本本次尝试 K={row["attempted_K"]}，与预期 K=1 不同，按工具阈值如实判定。')
    if comparison and not (comparison['trades_identical'] and comparison['stats_identical']):
        deviations.append('与上次运行的交易或统计不同；以本次为准，差异见 run_comparison。'
                          '新增平仓可能改变历史入场样本。')
    result = dict(idea_id=IDEA_ID, prereg=PREREG, prereg_sha256=prereg_hash,
                  inputs=dict(db=str(Path(db).resolve()), full_dir=str(full_dir.resolve()),
                              base_dir=str(base_dir.resolve()), snapshot_dir=str(snapshot_dir.resolve())),
                  input_hashes=hashes, expected_hashes=FROZEN, f2_frozen=frozen,
                  panel_digests=digests, factors=dict(f1=factors, f2=f2),
                  parameters=dict(until=UNTIL.isoformat(), end_bt=END_BT, forward_start=FORWARD_START,
                                  threshold=THRESHOLD, short_threshold=SHORT_THRESHOLD,
                                  alternative_thresholds=ALT_THRESHOLDS, min_history=60, universe_n=None,
                                  min_scored_share=MIN_SCORED_SHARE, attrib_window_ms=ATTRIB_WINDOW_MS,
                                  seed=SEED, placebo_n=PLACEBO_N),
                  export_counts=exports, attribution=attribution, s0=s0, primary=row,
                  descriptives=descriptive, run_comparison=comparison, deviations=deviations,
                  ledger=dict(path=str(Path(ledger_path).resolve()), written=not no_ledger, row=row),
                  elapsed_seconds=time.monotonic() - started)
    (out / 'report.json').write_text(json.dumps(json_safe(result), ensure_ascii=False, indent=2,
                                               allow_nan=False) + '\n', encoding='utf-8')
    (out / 'report.md').write_text(render_report(result), encoding='utf-8')
    validate_outputs(out, ledger_path, not no_ledger)
    return result


def render_report(result):
    p, d = result['primary'], result['descriptives']
    lines = ['# LLM 因子弱币做多否决：S1 研究', '',
             f'结论：**{p["verdict"]}**；K={p["K"]}；z_K={number(p["z_K"])}。', '',
             '**评估区间及币池覆盖率已被看过。S1 仅用于决定是否准备 S2，决定性证据需来自新交易。**', '',
             f'预注册：`{PREREG}`；SHA256：`{result["prereg_sha256"]}`。', '',
             '## 主检验', '', '| 指标 | 数值 |', '| --- | ---: |']
    for label, key in [('做多 n', 'n'), ('否决 n', 'dropped_n'), ('剔除率 p', 'p'),
                       ('ΔavgR', 'delta'), ('安慰剂 SE', 'se'), ('z', 'z'), ('z_K', 'z_K'),
                       ('安慰剂分位', 'placebo_pct'), ('dev Δ', 'dev_delta'), ('holdout Δ', 'holdout_delta'),
                       ('保留 avgR', 'kept_avg_r'), ('否决 avgR', 'dropped_avg_r'),
                       ('全体 avgR', 'all_avg_r'), ('总 R 变化', 'total_r_change')]:
        lines.append(f'| {label} | {number(p[key])} |')
    lines += ['', f'无效原因：{p["invalid_reasons"]}；掩码覆盖错误={p["mask_coverage_error"]}。', '',
              '## S0', '', f'通过：{result["s0"]["ok"]}；原因：{result["s0"]["reasons"]}。',
              f'RankIC 最大绝对误差：{number(result["s0"]["rank_ic"]["max_abs_error"], 12)}；'
              f'缺失日：{result["s0"]["rank_ic"]["missing_days"]}；'
              f'额外日：{result["s0"]["rank_ic"]["extra_days"]}。',
              f'覆盖率：{result["s0"]["coverage"]}；交易 R 导出一致：{result["s0"]["export"]["matched"]}。', '',
              '## 描述统计', '', '以下分析不进入账本，不改变主判定。', '',
              '### 保留与否决（含按交易员）', '',
              '| 交易员 | 组 | n | avgR | 周聚类 95% CI | 总 R |', '| --- | --- | ---: | ---: | --- | ---: |']
    for trader, groups in [('全部', d['split']), *d['per_trader'].items()]:
        for name, stats in groups.items():
            lines.append(f'| {trader} | {name} | {stats["n"]} | {number(stats["avg_r"])} | '
                         f'{stats["ci95"]} | {number(stats["total_r"])} |')
    lines += ['', '### 分位五等分', '', '| 分位 | n | avgR |', '| --- | ---: | ---: |']
    for q in d['quintiles']:
        lines.append(f'| {q["bin"]} | {q["n"]} | {number(q["avg_r"])} |')
    lines += ['', '### 其他阈值、F2 和对称做空否决', '',
              '| 描述组 | n | 剔除 n | 覆盖率 | Δ | SE | z | 安慰剂分位 |',
              '| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
    for name, stats in [*[(f'F1 ≤ {t}', v) for t, v in d['alternative_thresholds'].items()],
                        *[(f'F2 {k}', v) for k, v in d['f2'].items()], ('做空 F1 ≥ 0.80', d['short_veto'])]:
        lines.append('| ' + ' | '.join([name, str(stats['n']), str(stats['dropped_n']),
                     number(stats['coverage']['scored_share'])] +
                     [number(stats[k]) for k in ('delta', 'se', 'z', 'placebo_pct')]) + ' |')
    lines += ['', '### 被否决交易特征', '',
              '| trade_id | 币名 | 取值日 | 20 日成交额中位数 | 30 日波动 | 上市天数 | <90 日 |',
              '| --- | --- | --- | ---: | ---: | ---: | --- |']
    for row in d['vetoed_features']:
        lines.append('| ' + ' | '.join([str(row[k]) for k in ('trade_id', 'symbol', 'score_date')] +
                     [number(row[k]) for k in ('median_amount_20d', 'log_return_std_30d', 'listing_age_days')] +
                     [str(row['listing_under_90d'])]) + ' |')
    lines += ['', '### 无分数交易', '',
              json.dumps(json_safe(d['unscored']), ensure_ascii=False), '', '### AI 归因与导出计数', '']
    for trader, summary in result['attribution'].items():
        lines.append(f'- {trader}：{json.dumps(summary, ensure_ascii=False)}')
    lines += ['', f'导出计数：{json.dumps(result["export_counts"], ensure_ascii=False)}', '',
              '## 偏离与歧义说明（deviations）', '']
    lines.extend('- ' + item for item in result['deviations'])
    lines += ['', f'与上次运行比较：{json.dumps(result["run_comparison"], ensure_ascii=False)}', '',
              f'账本：{result["ledger"]["path"]}；已写入：{result["ledger"]["written"]}。',
              f'交易 SHA256：`{p["trades_sha256"]}`；掩码 SHA256：`{p["mask_sha256"]}`。',
              filters.QUEUE_NOTE]
    return '\n'.join(lines) + '\n'


def validate_outputs(out, ledger_path, wrote_ledger):
    out = Path(out)
    trades, trades_hash = filters.read_jsonl(out / 'trades.jsonl')
    masks, mask_hash = filters.read_jsonl(out / 'mask.jsonl')
    report = json.loads((out / 'report.json').read_text(encoding='utf-8'))
    if not (out / 'report.md').read_text(encoding='utf-8').strip():
        raise ValueError('empty report.md')
    row = report['primary']
    if row['trades_sha256'] != trades_hash or row['mask_sha256'] != mask_hash:
        raise ValueError('output sha256 mismatch')
    ids = {str(t['trade_id']) for t in trades}
    if len(ids) != len(trades) or len(masks) != len(trades) or {
            str(m['trade_id']) for m in masks} != ids or any(type(m['drop']) is not bool for m in masks):
        raise ValueError('trade/mask coverage mismatch or duplicate trade_id')
    if any(t['side'].lower() != 'long' for t in trades):
        raise ValueError('primary sample must be long-only')
    if wrote_ledger:
        rows = [r for r in evolution_ledger.read_rows(ledger_path) if r.get('track') == TRACK]
        if not rows or rows[-1].get('idea_id') != IDEA_ID or rows[-1].get('trades_sha256') != trades_hash:
            raise ValueError('last crypto_llm ledger row does not match outputs')
        evolution_ledger.validate(rows[-1])


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--db', type=Path, default=DB)
    parser.add_argument('--full-dir', type=Path, default=FULL_DIR)
    parser.add_argument('--base-dir', type=Path, default=BASE_DIR)
    parser.add_argument('--snapshot-dir', type=Path)
    parser.add_argument('--out', type=Path, default=OUT)
    parser.add_argument('--ledger', type=Path, default=DEFAULT_LEDGER)
    parser.add_argument('--no-ledger', action='store_true')
    args = parser.parse_args(argv)
    result = run_study(args.db, args.full_dir, args.base_dir, args.snapshot_dir, args.out, args.ledger,
                       no_ledger=args.no_ledger,
                       progress=lambda message: print(message, file=sys.stderr, flush=True))
    print(filters.evaluation_summary(result['primary']), flush=True)
    print(f'reports: {args.out / "report.json"}, {args.out / "report.md"}', flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
