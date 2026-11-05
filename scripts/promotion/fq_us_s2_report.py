"""fq_us momentum S2 report (design docs/architecture/FQ_US_MOMENTUM_S2_DESIGN_2026-11-05.md T3).

Read-only over the production SQLite database. Verdict rules are the
pre-registered S2 rules in docs/research/2026-11-05-fq-us-momentum-prereg.md:

- equity series: last snapshot of each Mon-Fri UTC date after --start, anchored
  on the last snapshot at or before --start (or the first after it when none
  exists);
- statistics: daily equity returns, mean, Newey-West (Bartlett, lag 20) z via
  scripts.factor_mining.evaluate.nw_t, cumulative return, max drawdown;
- sample gates: >= 126 trading days and >= 6 rebalances, otherwise `pending`;
- `fail` when cumulative return <= 0 or the 25% drawdown breaker fired;
- `pass` when z >= 1.65; anything else is `inconclusive`, extendable once to
  252 days; at n_days >= 252 a final `inconclusive` is reported as
  `fail_extended_inconclusive`.

Pair mode (--control/--candidate, pre-registered in
docs/research/2026-11-05-fq-us-volscale-pair-prereg.md 判定):

- common window: the weekday UTC dates present in BOTH arms after --start;
  daily sampling per arm reuses the single-trader logic above, and every
  per-arm statistic is computed on the common dates only;
- per arm: annualized Sharpe = mean / sd(ddof=1) × √252, max drawdown,
  cumulative return, and the same kill-switch rule (drawdown latch tripped at
  or after --start, or a computed daily max drawdown >= 25%);
- common rebalances: successful rebalance records of both arms since start,
  intersected by as_of. The as_of key is parsed from the `as_of=YYYY-MM-DD`
  line Go renders into cot_trace (trader/auto_trader_factor_quant.go);
  records without a parseable as_of fall back to their UTC date;
- verdict order (pre-registered): control kill fired and candidate clean →
  `pass` (early); candidate kill fired → `fail` (early); n_days < 126 or
  common rebalances < 6 → `pending`; stage 1 (126 <= n < 252): `pass` when
  Sharpe_candidate >= Sharpe_control and MaxDD_candidate < MaxDD_control,
  `fail` when Sharpe_candidate < Sharpe_control and MaxDD_candidate >=
  MaxDD_control, else `inconclusive`; at n >= 252: `pass` only when both
  conditions hold, else `fail`.

Tables and queries (documented as required by T3):

- rebalance count — decision_records (written by Go saveFactorQuantRecord ->
  saveDecision -> DecisionStore.LogDecision, store/decision.go). A real
  rebalance/flatten/retry attempt renders its summary with cot_trace starting
  "因子量化调仓（" and success = (no failed orders); skip ticks render
  "因子量化 · 未调仓（" instead and are excluded:

      SELECT timestamp FROM decision_records
      WHERE trader_id = ? AND success = 1
        AND cot_trace LIKE '因子量化调仓（%'
        AND substr(timestamp, 1, 10) >= ?  -- start date, safe text pruning;
                                           -- exact comparison happens in Python
                                           -- because GORM stores
                                           -- 'YYYY-MM-DD HH:MM:SS.f+00:00'

- kill switch — factor_quant_state row for the trader. The drawdown latch is
  halt_kind = 'drawdown' (trader/auto_trader_factor_quant_risk.go sets it when
  drawdown >= EffectiveMaxDrawdownPct, default 25%); halted_at is unix
  *milliseconds*. Fired when the latch is tripped at/after --start (or tripped
  with a zero/unknown timestamp). As a fallback, a computed max drawdown of the
  daily equity series >= 25% also fires, so a trader that stopped before its
  risk tick persisted the latch is still judged fail.
"""
import argparse
import json
import math
import re
import sqlite3
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import quote

from scripts.backtest.evolution_ledger import append_event, json_safe, read_rows

UTC = timezone.utc
NW_LAG = 20
MIN_DAYS = 126
MIN_REBALANCES = 6
EXTENSION_DAYS = 252
Z_THRESHOLD = 1.65
MAX_DRAWDOWN = 0.25
IDEA_ID = 'fq-us-momentum-ls-v1'
BASELINE_VERSION = 'fq_us:no_validated_factors'
REBALANCE_PREFIX = '因子量化调仓（'
SHARPE_ANNUAL_DAYS = 252
PAIR_IDEA_ID = 'fq-us-volscale-pair-v1'
PAIR_BASELINE_VERSION = 'mom_ls_invvol_v1'
AS_OF_PATTERN = re.compile(r'as_of=(\d{4}-\d{2}-\d{2})')
REBALANCE_KEY_METHOD = (
    '共同调仓按 as_of 求交集：取 decision_records 里 success=1 且 cot_trace 以'
    '「因子量化调仓（」开头的记录，主键用 cot_trace 第二行 Go 写入的 '
    'as_of=YYYY-MM-DD（trader/auto_trader_factor_quant.go 渲染）；'
    '该行缺失或不可解析时退回记录 timestamp 的 UTC 日期')

try:  # frozen evaluator; identical fallback below keeps the script standalone
    from scripts.factor_mining.evaluate import nw_t
except Exception:  # pragma: no cover - numpy-less environments
    nw_t = None


def nw_t_fallback(series, lag) -> float:
    """Pure-python transcription of evaluate.nw_t (Bartlett/Newey-West HAC z)."""
    x = [float(v) for v in series if isinstance(v, (int, float)) and math.isfinite(float(v))]
    n = len(x)
    if n < 20:
        return 0.0
    mean = sum(x) / n
    centered = [v - mean for v in x]
    variance = sum(c * c for c in centered) / n
    for k in range(1, min(int(lag), n - 1) + 1):
        variance += 2 * (1 - k / (lag + 1)) * sum(a * b for a, b in zip(centered[k:], centered[:-k])) / n
    if variance <= 1e-30:
        return math.copysign(1e12, mean) if mean else 0.0
    return mean / math.sqrt(variance / n)


def hac_z(returns, lag=NW_LAG) -> float:
    if nw_t is not None:
        return float(nw_t(returns, lag))
    return nw_t_fallback(returns, lag)


def parse_instant(value) -> datetime:
    """Parse CLI ISO8601 or DB timestamp text into an aware UTC datetime.

    GORM/SQLite stores datetimes as 'YYYY-MM-DD HH:MM:SS[.ffffff]+00:00'.
    """
    if isinstance(value, datetime):
        moment = value
    else:
        text = str(value).strip()
        if text.endswith('Z'):
            text = text[:-1] + '+00:00'
        moment = datetime.fromisoformat(text)
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=UTC)
    return moment.astimezone(UTC)


def iso(moment) -> str:
    return moment.astimezone(UTC).strftime('%Y-%m-%dT%H:%M:%SZ')


def open_db(path) -> sqlite3.Connection:
    """Open the database strictly read-only (mode=ro fails on missing files)."""
    uri = 'file:' + quote(str(Path(path).resolve())) + '?mode=ro'
    return sqlite3.connect(uri, uri=True)


def load_snapshots(conn, trader_id, start: datetime):
    """Return (baseline, points) as [(datetime, equity)] chronologically sorted.

    points are the last snapshot of every Mon-Fri UTC date at/after start;
    baseline is the last snapshot at or before start (else the first after it).
    A baseline falling inside the daily pool is removed from it so the anchor
    is never repeated as a sample point.
    """
    start_date = start.strftime('%Y-%m-%d')
    rows = []
    for ts_text, equity in conn.execute(
            'SELECT timestamp, total_equity FROM trader_equity_snapshots '
            'WHERE trader_id = ? AND substr(timestamp, 1, 10) >= ? '
            'ORDER BY timestamp ASC', (trader_id, start_date)):
        rows.append((parse_instant(ts_text), float(equity)))
    before = []
    for ts_text, equity in conn.execute(
            'SELECT timestamp, total_equity FROM trader_equity_snapshots '
            'WHERE trader_id = ? AND substr(timestamp, 1, 10) <= ? '
            'ORDER BY timestamp DESC', (trader_id, start_date)):
        moment = parse_instant(ts_text)
        if moment <= start:
            before.append((moment, float(equity)))
    rows.sort(key=lambda item: item[0])

    baseline = before[0] if before else (rows[0] if rows else None)
    pool = [(ts, eq) for ts, eq in rows if ts >= start]
    if baseline is not None:  # the anchor never repeats as a sample point
        pool = [(ts, eq) for ts, eq in pool if ts > baseline[0]]
    last_of_day = {}
    for ts, eq in pool:
        if ts.weekday() < 5:  # NYSE trading days are Mon-Fri UTC dates per T3
            last_of_day[ts.date()] = (ts, eq)
    points = [last_of_day[date] for date in sorted(last_of_day)]
    return baseline, points


def returns_over(baseline, points):
    """Daily returns r_d = E_d / E_{d-1} - 1 over consecutive points."""
    series = ([baseline] if baseline else []) + list(points)
    returns = []
    for (_, previous), (_, equity) in zip(series, series[1:]):
        if math.isfinite(previous) and math.isfinite(equity) and previous > 0:
            returns.append(equity / previous - 1.0)
    return returns


def max_drawdown(baseline, points) -> float:
    series = [eq for _, eq in ([baseline] if baseline else []) + list(points) if math.isfinite(eq)]
    peak, drawdown = float('-inf'), 0.0
    for equity in series:
        peak = max(peak, equity)
        if peak > 0:
            drawdown = max(drawdown, (peak - equity) / peak)
    return drawdown


def count_rebalances(conn, trader_id, start: datetime) -> int:
    """Successful rebalance records since start (query documented in docstring)."""
    start_date = start.strftime('%Y-%m-%d')
    count = 0
    for (ts_text,) in conn.execute(
            "SELECT timestamp FROM decision_records "
            "WHERE trader_id = ? AND success = 1 "
            "AND cot_trace LIKE ? "
            "AND substr(timestamp, 1, 10) >= ?",
            (trader_id, REBALANCE_PREFIX + '%', start_date)):
        if parse_instant(ts_text) >= start:
            count += 1
    return count


def kill_switch(conn, trader_id, start: datetime, computed_dd: float):
    """(fired, info): drawdown latch in factor_quant_state since start, or a
    computed daily max drawdown >= 25% as fallback."""
    row = conn.execute(
        'SELECT halt_kind, halt_day, halt_reason, halted_at FROM factor_quant_state '
        'WHERE trader_id = ?', (trader_id,)).fetchone()
    info = {'latch_found': row is not None, 'sources': []}
    if row is not None:
        info.update(halt_kind=row[0] or '', halt_day=row[1] or '',
                    halt_reason=row[2] or '', halted_at=row[3] or 0)
        tripped_at = None
        if info['halted_at'] > 0:
            tripped_at = datetime.fromtimestamp(info['halted_at'] / 1000.0, tz=UTC)
            info['halted_at_iso'] = iso(tripped_at)
        if info['halt_kind'] == 'drawdown' and (tripped_at is None or tripped_at >= start):
            info['sources'].append('state_latch')
    if computed_dd >= MAX_DRAWDOWN:
        info['sources'].append('computed_drawdown')
    return bool(info['sources']), info


def decide(n_days, rebalances, cum_return, kill, z):
    """Pre-registered verdict plus the human-readable reasons behind it."""
    reasons = []
    if n_days < MIN_DAYS:
        reasons.append(f'样本不足：{n_days} 个日收益 < {MIN_DAYS}')
    if rebalances < MIN_REBALANCES:
        reasons.append(f'调仓不足：{rebalances} 次 < {MIN_REBALANCES}')
    if n_days < MIN_DAYS or rebalances < MIN_REBALANCES:
        return 'pending', reasons
    if cum_return is not None and cum_return <= 0:
        reasons.append('累计收益 ≤ 0')
    if kill:
        reasons.append('25% 回撤熔断已触发')
    if cum_return is not None and cum_return <= 0 or kill:
        return 'fail', reasons
    if z >= Z_THRESHOLD:
        reasons.append(f'z={z:.2f} ≥ {Z_THRESHOLD}')
        return 'pass', reasons
    if n_days >= EXTENSION_DAYS:
        reasons.append(f'z={z:.2f} < {Z_THRESHOLD}；样本已达 {EXTENSION_DAYS} 天，'
                       f'inconclusive 按预注册定为 fail_extended_inconclusive')
        return 'fail_extended_inconclusive', reasons
    reasons.append(f'z={z:.2f} < {Z_THRESHOLD}；允许延长一次至 {EXTENSION_DAYS} 天')
    return 'inconclusive', reasons


def sample_stage(n_days) -> str:
    if n_days < MIN_DAYS:
        return 'stage_1'
    if n_days < EXTENSION_DAYS:
        return 'extension'
    return 'extension_final'


def analyze(conn, trader_id, start, lag=NW_LAG):
    baseline, points = load_snapshots(conn, trader_id, start)
    returns = returns_over(baseline, points)
    cum = None
    if baseline is not None and points:
        last = points[-1][1]
        if math.isfinite(baseline[1]) and math.isfinite(last) and baseline[1] > 0:
            cum = last / baseline[1] - 1.0
    dd = max_drawdown(baseline, points)
    rebalances = count_rebalances(conn, trader_id, start)
    kill, kill_info = kill_switch(conn, trader_id, start, dd)
    return {
        'baseline': baseline, 'points': points, 'returns': returns,
        'n_days': len(returns),
        'mean': sum(returns) / len(returns) if returns else None,
        'z': hac_z(returns, lag) if returns else None,
        'cum': cum, 'max_drawdown': dd, 'rebalances': rebalances,
        'kill': kill, 'kill_info': kill_info,
    }


def build_report(conn, trader_id, start, now, lag=NW_LAG):
    stats = analyze(conn, trader_id, start, lag)
    verdict, reasons = decide(stats['n_days'], stats['rebalances'], stats['cum'],
                              stats['kill'], stats['z'])
    return {
        'idea_id': IDEA_ID,
        'trader_id': trader_id,
        'start': iso(start),
        'generated_at': iso(now),
        'stage': 'S2',
        'sample_stage': sample_stage(stats['n_days']),
        'n_days': stats['n_days'],
        'n_points': len(stats['points']),
        'mean_daily_return': stats['mean'],
        'nw_z': stats['z'],
        'nw_lag': lag,
        'cum_return': stats['cum'],
        'max_drawdown': stats['max_drawdown'],
        'rebalances': stats['rebalances'],
        'rebalance_query': (
            "SELECT timestamp FROM decision_records WHERE trader_id = ? AND success = 1 "
            "AND cot_trace LIKE '因子量化调仓（%' AND substr(timestamp, 1, 10) >= ? "
            '-- 记录由 Go saveFactorQuantRecord 写入；精确的时间比较在 Python 里做'),
        'kill_fired': stats['kill'],
        'kill_sources': stats['kill_info']['sources'],
        'kill_state': {k: v for k, v in stats['kill_info'].items() if k != 'sources'},
        'baseline': {'timestamp': iso(stats['baseline'][0]), 'total_equity': stats['baseline'][1]}
        if stats['baseline'] else None,
        'first_point': {'timestamp': iso(stats['points'][0][0]),
                        'total_equity': stats['points'][0][1]} if stats['points'] else None,
        'last_point': {'timestamp': iso(stats['points'][-1][0]),
                       'total_equity': stats['points'][-1][1]} if stats['points'] else None,
        'verdict': verdict,
        'reasons': reasons,
        'thresholds': {'min_days': MIN_DAYS, 'min_rebalances': MIN_REBALANCES,
                       'z': Z_THRESHOLD, 'max_drawdown': MAX_DRAWDOWN,
                       'extension_days': EXTENSION_DAYS, 'nw_lag': lag},
    }


VERDICT_LABELS = {
    'pending': '样本未达标，继续观察',
    'pass': '通过',
    'fail': '不通过',
    'inconclusive': '尚不能判定，延长至 252 天',
    'fail_extended_inconclusive': '延长后仍不能判定，按预注册定为不通过',
}


def summary(report) -> str:
    def pct(value):
        return '无' if value is None else f'{value * 100:.2f}%'

    z_text = '无' if report['nw_z'] is None else f"{report['nw_z']:.2f}"
    lines = [
        f"fq_us S2 报告（{report['idea_id']}，预注册口径）",
        f"交易员 {report['trader_id']}",
        f"起始 {report['start']}，报告时间 {report['generated_at']}，阶段 {report['sample_stage']}",
        f"样本：{report['n_days']} 个日收益（{report['n_points']} 个交易日点），"
        f"调仓 {report['rebalances']} 次",
        f"净值：累计 {pct(report['cum_return'])}，最大回撤 {pct(report['max_drawdown'])}，"
        f"日收益均值 "
        f"{'无' if report['mean_daily_return'] is None else format(report['mean_daily_return'], '.6f')}"
        f"，NW z(lag={report['nw_lag']}) {z_text}",
        f"熔断：{'已触发（' + ','.join(report['kill_sources']) + '）' if report['kill_fired'] else '未触发'}",
        f"判定：{report['verdict']}（{VERDICT_LABELS[report['verdict']]}）",
    ]
    for reason in report['reasons']:
        lines.append(f'  - {reason}')
    return '\n'.join(lines)


def render_markdown(report) -> str:
    def pct(value):
        return '无' if value is None else f'{value * 100:.2f}%'

    z_text = '无' if report['nw_z'] is None else f"{report['nw_z']:.4f}"
    mean_text = '无' if report['mean_daily_return'] is None else f"{report['mean_daily_return']:.6f}"
    kill_line = ('已触发：' + '、'.join(report['kill_sources'])) if report['kill_fired'] else '未触发'
    lines = [
        f"# fq_us 动量多空 S2 报告（{report['idea_id']}）",
        '',
        f"- 交易员：`{report['trader_id']}`",
        f"- S2 起始：{report['start']}；报告时间：{report['generated_at']}",
        f"- 样本阶段：{report['sample_stage']}（第一阶段 ≤ {MIN_DAYS} 天，允许延长一次至 "
        f"{EXTENSION_DAYS} 天）",
        f"- **判定：{report['verdict']}（{VERDICT_LABELS[report['verdict']]}）**",
        '',
        '## 样本与净值',
        '',
        '| 指标 | 值 |',
        '|---|---|',
        f"| 基准点 | {report['baseline']['timestamp']} 权益 "
        f"{report['baseline']['total_equity']:.2f} |" if report['baseline'] else '| 基准点 | 无快照 |',
        f"| 首个日点 | {report['first_point']['timestamp']} 权益 "
        f"{report['first_point']['total_equity']:.2f} |" if report['first_point'] else '| 首个日点 | 无 |',
        f"| 最后日点 | {report['last_point']['timestamp']} 权益 "
        f"{report['last_point']['total_equity']:.2f} |" if report['last_point'] else '| 最后日点 | 无 |',
        f"| 日收益数 n_days | {report['n_days']}（要求 ≥ {MIN_DAYS}） |",
        f"| 调仓次数 | {report['rebalances']}（要求 ≥ {MIN_REBALANCES}） |",
        f"| 累计收益 | {pct(report['cum_return'])} |",
        f"| 最大回撤 | {pct(report['max_drawdown'])}（熔断线 {MAX_DRAWDOWN:.0%}） |",
        f"| 日收益均值 | {mean_text} |",
        f"| NW z（Bartlett lag={report['nw_lag']}） | {z_text}（通过线 ≥ {Z_THRESHOLD}） |",
        '',
        '## 熔断与调仓口径',
        '',
        f'- 熔断：{kill_line}。优先读 `factor_quant_state` 的回撤闩锁 '
        f'（halt_kind=drawdown，halted_at 为 Unix 毫秒，只认起始之后的触发），'
        f'再以日线最大回撤 ≥ {MAX_DRAWDOWN:.0%} 兜底。',
        '- 调仓：`decision_records`（Go `saveFactorQuantRecord` 写入）里 '
        '`success=1` 且 `cot_trace` 以「因子量化调仓（」开头的记录，时间过滤在 Python 精确比较；'
        '「因子量化 · 未调仓（」开头的跳过记录不计入。',
        '',
        '## 判定依据',
        '',
    ]
    lines += [f'- {reason}' for reason in report['reasons']] or ['- 无']
    lines += [
        '',
        '## 预注册规则（docs/research/2026-11-05-fq-us-momentum-prereg.md）',
        '',
        f'- 样本 ≥ {MIN_DAYS} 个交易日且调仓 ≥ {MIN_REBALANCES} 次，否则 `pending`；',
        f'- `fail`：累计收益 ≤ 0 或触发 {MAX_DRAWDOWN:.0%} 回撤熔断；',
        f'- `pass`：z ≥ {Z_THRESHOLD}；',
        f'- 其余 `inconclusive`，只允许延长一次到 {EXTENSION_DAYS} 天；到期仍不能判定，'
        '最终记 `fail_extended_inconclusive`。',
        '',
    ]
    return '\n'.join(lines)


def write_report(report, out_dir: Path):
    out_dir.mkdir(parents=True, exist_ok=True)
    json_path = out_dir / 'report.json'
    md_path = out_dir / 'report.md'
    json_path.write_text(json.dumps(json_safe(report), ensure_ascii=False, indent=2,
                                   allow_nan=False) + '\n', encoding='utf-8')
    md_path.write_text(render_markdown(report), encoding='utf-8')
    return json_path, md_path


def ledger_row(report):
    """One evolution_ledger row (stage S2, change_type E); pending never lands."""
    return {
        'stage': 'S2',
        'change_type': 'E',
        'idea_id': IDEA_ID,
        'baseline_version': BASELINE_VERSION,
        'verdict': report['verdict'],
        'n_days': report['n_days'],
        'rebalances': report['rebalances'],
        'nw_z': report['nw_z'],
        'cum_return': report['cum_return'],
        'max_drawdown': report['max_drawdown'],
        'kill_fired': report['kill_fired'],
        'sample_stage': report['sample_stage'],
        'start': report['start'],
        'window_end': report['last_point']['timestamp'] if report['last_point'] else None,
        'trader_id': report['trader_id'],
        'report': 'scripts/promotion/fq_us_s2_report.py',
    }


def append_ledger(report, path) -> dict:
    def build(k, previous):
        row = ledger_row(report)
        row['previous_verdicts'] = [r.get('verdict') for r in previous
                                    if r.get('track') == 'fq_us' and r.get('stage') == 'S2'][-5:]
        return row

    return append_event(Path(path), 'fq_us', build)


# --- 配对模式（idea_id fq-us-volscale-pair-v1）-----------------------------
# 预注册：docs/research/2026-11-05-fq-us-volscale-pair-prereg.md「判定」。


def sharpe_ratio(returns):
    """年化夏普 = 日均收益 ÷ 样本标准差(ddof=1) × √252（预注册口径）。

    常数序列没有有限夏普：均值为 0 时返回 None，否则沿用 nw_t_fallback 对
    零方差的处理，按均值符号给 ±1e12，使比较保持确定性。
    """
    n = len(returns)
    if n < 2:
        return None
    mean = sum(returns) / n
    variance = sum((r - mean) ** 2 for r in returns) / (n - 1)
    if variance <= 0.0:
        return math.copysign(1e12, mean) if mean else None
    return mean / math.sqrt(variance) * math.sqrt(SHARPE_ANNUAL_DAYS)


def rebalance_asofs(conn, trader_id, start):
    """One arm's rebalance keys since start (same records as count_rebalances).

    Key: the `as_of=YYYY-MM-DD` line Go renders into cot_trace
    (trader/auto_trader_factor_quant.go, second line of the rebalance
    summary); records without a parseable as_of fall back to their UTC date.
    """
    start_date = start.strftime('%Y-%m-%d')
    keys = set()
    for ts_text, cot_trace in conn.execute(
            "SELECT timestamp, cot_trace FROM decision_records "
            "WHERE trader_id = ? AND success = 1 "
            "AND cot_trace LIKE ? "
            "AND substr(timestamp, 1, 10) >= ?",
            (trader_id, REBALANCE_PREFIX + '%', start_date)):
        if parse_instant(ts_text) < start:
            continue
        match = AS_OF_PATTERN.search(cot_trace or '')
        keys.add(match.group(1) if match else ts_text[:10])
    return keys


def count_common_rebalances(conn, control_id, candidate_id, start):
    """(count, sorted keys): rebalances both arms executed since start."""
    common = (rebalance_asofs(conn, control_id, start)
              & rebalance_asofs(conn, candidate_id, start))
    return len(common), sorted(common)


def analyze_pair_arm(conn, trader_id, start, window_dates):
    """One arm's stats restricted to the common window (sampling logic reused)."""
    baseline, points = load_snapshots(conn, trader_id, start)
    by_date = {ts.date(): (ts, eq) for ts, eq in points}
    common_points = [by_date[date] for date in window_dates]
    returns = returns_over(baseline, common_points)
    cum = None
    if baseline is not None and common_points:
        last = common_points[-1][1]
        if math.isfinite(baseline[1]) and math.isfinite(last) and baseline[1] > 0:
            cum = last / baseline[1] - 1.0
    dd = max_drawdown(baseline, common_points)
    kill, kill_info = kill_switch(conn, trader_id, start, dd)
    return {
        'trader_id': trader_id, 'baseline': baseline, 'points': common_points,
        'returns': returns, 'n_days': len(returns),
        'mean': sum(returns) / len(returns) if returns else None,
        'sharpe': sharpe_ratio(returns), 'cum': cum, 'max_drawdown': dd,
        'rebalances': count_rebalances(conn, trader_id, start),
        'kill': kill, 'kill_info': kill_info,
    }


def decide_pair(n_days, common_rebalances, control, candidate):
    """Pre-registered pair verdict (order as written in the prereg) + reasons."""
    reasons = []
    if control['kill'] and not candidate['kill']:
        reasons.append('对照触发 25% 回撤熔断而候选未触发，按预注册提前判 pass')
        return 'pass', reasons
    if candidate['kill']:
        reasons.append('候选触发 25% 回撤熔断，按预注册提前判 fail')
        return 'fail', reasons
    if n_days < MIN_DAYS:
        reasons.append(f'样本不足：共同窗口 {n_days} 个日收益 < {MIN_DAYS}')
    if common_rebalances < MIN_REBALANCES:
        reasons.append(f'共同调仓不足：{common_rebalances} 次 < {MIN_REBALANCES}')
    if n_days < MIN_DAYS or common_rebalances < MIN_REBALANCES:
        return 'pending', reasons

    def fmt(value):
        return '无' if value is None else f'{value:.2f}'

    sharpe_c, sharpe_k = candidate['sharpe'], control['sharpe']
    dd_c, dd_k = candidate['max_drawdown'], control['max_drawdown']
    sharpe_ok = sharpe_c is not None and sharpe_k is not None and sharpe_c >= sharpe_k
    sharpe_worse = sharpe_c is not None and sharpe_k is not None and sharpe_c < sharpe_k
    dd_ok = dd_c < dd_k
    stats = (f'Sharpe 候选 {fmt(sharpe_c)} 对 对照 {fmt(sharpe_k)}，'
             f'MaxDD 候选 {dd_c:.2%} 对 对照 {dd_k:.2%}')
    if n_days >= EXTENSION_DAYS:
        if sharpe_ok and dd_ok:
            reasons.append(f'延长阶段（n={n_days}）：两项条件同时满足（{stats}），判 pass')
            return 'pass', reasons
        reasons.append(f'延长阶段（n={n_days}）：两项条件未同时满足（{stats}），判 fail')
        return 'fail', reasons
    if sharpe_ok and dd_ok:
        reasons.append(f'第一阶段（n={n_days}）：候选 Sharpe 不变差且回撤更小（{stats}），判 pass')
        return 'pass', reasons
    if sharpe_worse and not dd_ok:
        reasons.append(f'第一阶段（n={n_days}）：候选 Sharpe 更差且回撤不更小（{stats}），判 fail')
        return 'fail', reasons
    reasons.append(f'第一阶段（n={n_days}）：两项条件互有胜负（{stats}），'
                   f'判 inconclusive，延长一次至 {EXTENSION_DAYS} 天')
    return 'inconclusive', reasons


def build_pair_report(conn, control_id, candidate_id, start, now):
    """Pair report: per-arm stats on the common window + pre-registered verdict."""
    _, control_points = load_snapshots(conn, control_id, start)
    _, candidate_points = load_snapshots(conn, candidate_id, start)
    window_dates = sorted({ts.date() for ts, _ in control_points}
                          & {ts.date() for ts, _ in candidate_points})
    arms = {role: analyze_pair_arm(conn, trader_id, start, window_dates)
            for role, trader_id in (('control', control_id), ('candidate', candidate_id))}
    n_days = min(arms['control']['n_days'], arms['candidate']['n_days'])
    common_rebalances, common_keys = count_common_rebalances(conn, control_id,
                                                             candidate_id, start)
    verdict, reasons = decide_pair(n_days, common_rebalances,
                                   arms['control'], arms['candidate'])

    def arm_summary(arm):
        return {
            'trader_id': arm['trader_id'],
            'baseline': {'timestamp': iso(arm['baseline'][0]),
                         'total_equity': arm['baseline'][1]} if arm['baseline'] else None,
            'first_point': {'timestamp': iso(arm['points'][0][0]),
                            'total_equity': arm['points'][0][1]} if arm['points'] else None,
            'last_point': {'timestamp': iso(arm['points'][-1][0]),
                           'total_equity': arm['points'][-1][1]} if arm['points'] else None,
            'n_days': arm['n_days'],
            'mean_daily_return': arm['mean'],
            'daily_returns': arm['returns'],
            'sharpe': arm['sharpe'],
            'cum_return': arm['cum'],
            'max_drawdown': arm['max_drawdown'],
            'rebalances': arm['rebalances'],
            'kill_fired': arm['kill'],
            'kill_sources': arm['kill_info']['sources'],
            'kill_state': {k: v for k, v in arm['kill_info'].items() if k != 'sources'},
        }

    return {
        'idea_id': PAIR_IDEA_ID,
        'mode': 'pair',
        'control': arm_summary(arms['control']),
        'candidate': arm_summary(arms['candidate']),
        'start': iso(start),
        'generated_at': iso(now),
        'stage': 'S2',
        'sample_stage': sample_stage(n_days),
        'n_days': n_days,
        'window_dates': [date.isoformat() for date in window_dates],
        'window_start': window_dates[0].isoformat() if window_dates else None,
        'window_end': window_dates[-1].isoformat() if window_dates else None,
        'common_rebalances': common_rebalances,
        'common_rebalance_asofs': common_keys,
        'rebalance_key_method': REBALANCE_KEY_METHOD,
        'verdict': verdict,
        'reasons': reasons,
        'thresholds': {'min_days': MIN_DAYS, 'min_rebalances': MIN_REBALANCES,
                       'max_drawdown': MAX_DRAWDOWN, 'extension_days': EXTENSION_DAYS,
                       'sharpe_annual_days': SHARPE_ANNUAL_DAYS},
    }


PAIR_VERDICT_LABELS = {
    'pending': '样本未达标，继续观察',
    'pass': '通过',
    'fail': '不通过',
    'inconclusive': '尚不能判定，延长一次至 252 天',
}


def pair_summary(data) -> str:
    def pct(value):
        return '无' if value is None else f'{value * 100:.2f}%'

    def sharpe_text(value):
        return '无' if value is None else f'{value:.2f}'

    def kill_text(arm):
        return ('已触发（' + '、'.join(arm['kill_sources']) + '）'
                if arm['kill_fired'] else '未触发')

    control, candidate = data['control'], data['candidate']
    lines = [
        f"fq_us S2 配对报告（{data['idea_id']}，预注册口径）",
        f"对照 {control['trader_id']}",
        f"候选 {candidate['trader_id']}",
        f"起始 {data['start']}，报告时间 {data['generated_at']}，阶段 {data['sample_stage']}",
        f"共同窗口：{data['n_days']} 个共同日收益"
        f"（{data['window_start'] or '无'} … {data['window_end'] or '无'}），"
        f"共同调仓 {data['common_rebalances']} 次",
        f"对照：夏普 {sharpe_text(control['sharpe'])}，最大回撤 {pct(control['max_drawdown'])}，"
        f"累计 {pct(control['cum_return'])}，自身调仓 {control['rebalances']} 次，"
        f"熔断 {kill_text(control)}",
        f"候选：夏普 {sharpe_text(candidate['sharpe'])}，最大回撤 {pct(candidate['max_drawdown'])}，"
        f"累计 {pct(candidate['cum_return'])}，自身调仓 {candidate['rebalances']} 次，"
        f"熔断 {kill_text(candidate)}",
        f"判定：{data['verdict']}（{PAIR_VERDICT_LABELS[data['verdict']]}）",
    ]
    for reason in data['reasons']:
        lines.append(f'  - {reason}')
    return '\n'.join(lines)


def render_pair_markdown(data) -> str:
    def pct(value):
        return '无' if value is None else f'{value * 100:.2f}%'

    def sharpe_text(value):
        return '无' if value is None else f'{value:.4f}'

    def kill_text(arm):
        return '已触发：' + '、'.join(arm['kill_sources']) if arm['kill_fired'] else '未触发'

    control, candidate = data['control'], data['candidate']
    lines = [
        f"# fq_us 波动率缩放在线配对 S2 报告（{data['idea_id']}）",
        '',
        f"- 对照：`{control['trader_id']}`（mom_ls_invvol_v1）",
        f"- 候选：`{candidate['trader_id']}`（mom_ls_invvol_volscale_v1）",
        f"- S2 起始：{data['start']}；报告时间：{data['generated_at']}",
        f"- 样本阶段：{data['sample_stage']}（判定要求 ≥ {MIN_DAYS} 个共同交易日、"
        f"≥ {MIN_REBALANCES} 次共同调仓，可延长一次至 {EXTENSION_DAYS} 天）",
        f"- **判定：{data['verdict']}（{PAIR_VERDICT_LABELS[data['verdict']]}）**",
        '',
        '## 共同窗口',
        '',
        '| 指标 | 值 |',
        '|---|---|',
        f"| 共同交易日点 | {len(data['window_dates'])}"
        f"（{data['window_start'] or '无'} … {data['window_end'] or '无'}） |",
        f"| 共同日收益 n_days | {data['n_days']} |",
        f"| 共同调仓 | {data['common_rebalances']} 次 |",
        f'| as_of 口径 | {data["rebalance_key_method"]} |',
        '',
        '## 两组统计（公共窗口内）',
        '',
        '| 指标 | 对照 | 候选 |',
        '|---|---|---|',
        f"| 夏普（日均 ÷ sd(ddof=1) × √{SHARPE_ANNUAL_DAYS}） "
        f"| {sharpe_text(control['sharpe'])} | {sharpe_text(candidate['sharpe'])} |",
        f"| 最大回撤 | {pct(control['max_drawdown'])} | {pct(candidate['max_drawdown'])} |",
        f"| 累计收益 | {pct(control['cum_return'])} | {pct(candidate['cum_return'])} |",
        f"| 日收益均值 | "
        f"{'无' if control['mean_daily_return'] is None else format(control['mean_daily_return'], '.6f')} "
        f"| "
        f"{'无' if candidate['mean_daily_return'] is None else format(candidate['mean_daily_return'], '.6f')} |",
        f"| 日收益数 | {control['n_days']} | {candidate['n_days']} |",
        f"| 自身调仓次数 | {control['rebalances']} | {candidate['rebalances']} |",
        f"| {MAX_DRAWDOWN:.0%} 回撤熔断 | {kill_text(control)} | {kill_text(candidate)} |",
        '',
        '## 判定依据',
        '',
    ]
    lines += [f'- {reason}' for reason in data['reasons']] or ['- 无']
    lines += [
        '',
        '## 预注册规则（docs/research/2026-11-05-fq-us-volscale-pair-prereg.md）',
        '',
        '- 对照触发 25% 熔断而候选未触发 → `pass`（提前）；候选触发熔断 → `fail`（提前）；',
        f'- 共同窗口 ≥ {MIN_DAYS} 个交易日且共同调仓 ≥ {MIN_REBALANCES} 次，否则 `pending`；',
        '- 第一阶段（126 ≤ n < 252）：候选 Sharpe ≥ 对照且候选 MaxDD 更小 → `pass`；'
        '候选 Sharpe 更差且 MaxDD 不更小 → `fail`；其余 `inconclusive`，延长一次；',
        f'- n ≥ {EXTENSION_DAYS}：两项条件同时满足 → `pass`，否则 `fail`。',
        '',
    ]
    return '\n'.join(lines)


def write_pair_report(data, out_dir: Path):
    out_dir.mkdir(parents=True, exist_ok=True)
    json_path = out_dir / 'report.json'
    md_path = out_dir / 'report.md'
    json_path.write_text(json.dumps(json_safe(data), ensure_ascii=False, indent=2,
                                    allow_nan=False) + '\n', encoding='utf-8')
    md_path.write_text(render_pair_markdown(data), encoding='utf-8')
    return json_path, md_path


def pair_ledger_row(data):
    """One evolution_ledger row (stage S2, change_type C); pending never lands."""
    def arm_stats(arm):
        return {'trader_id': arm['trader_id'], 'n_days': arm['n_days'],
                'sharpe': arm['sharpe'], 'cum_return': arm['cum_return'],
                'max_drawdown': arm['max_drawdown'], 'kill_fired': arm['kill_fired']}

    return {
        'stage': 'S2',
        'change_type': 'C',
        'idea_id': PAIR_IDEA_ID,
        'baseline_version': PAIR_BASELINE_VERSION,
        'verdict': data['verdict'],
        'n_days': data['n_days'],
        'common_rebalances': data['common_rebalances'],
        'sample_stage': data['sample_stage'],
        'start': data['start'],
        'window_end': data['window_end'],
        'control': arm_stats(data['control']),
        'candidate': arm_stats(data['candidate']),
        'report': 'scripts/promotion/fq_us_s2_report.py',
    }


def append_pair_ledger(data, path) -> dict:
    def build(k, previous):
        row = pair_ledger_row(data)
        row['previous_verdicts'] = [r.get('verdict') for r in previous
                                    if r.get('track') == 'fq_us'
                                    and r.get('idea_id') == PAIR_IDEA_ID][-5:]
        return row

    return append_event(Path(path), 'fq_us', build)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--db', required=True, help='SQLite 数据库路径（只读打开）')
    parser.add_argument('--trader-id', default=None, help='fq_us 交易员 id（单交易员模式）')
    parser.add_argument('--control', default=None, help='配对模式：对照交易员 id（现行规则）')
    parser.add_argument('--candidate', default=None, help='配对模式：候选交易员 id（新规则）')
    parser.add_argument('--start', required=True, help='S2 起始时间 ISO8601，如 2026-11-01T00:00:00Z')
    parser.add_argument('--out', default=None, help='输出目录（写 report.json 与 report.md）')
    parser.add_argument('--ledger', default=None, help='evolution_ledger 路径；pending 不记账')
    parser.add_argument('--now', default=None, help='报告时间 ISO8601（默认当前时间）')
    args = parser.parse_args(argv)
    pair = args.control is not None or args.candidate is not None
    if pair != (args.control is not None and args.candidate is not None) or (pair and args.trader_id):
        print('fq_us S2 报告失败：单交易员模式用 --trader-id，配对模式需同时给出 '
              '--control 与 --candidate，两者不能混用', file=sys.stderr)
        return 1
    if not pair and not args.trader_id:
        print('fq_us S2 报告失败：需要 --trader-id（单交易员模式）'
              '或 --control 与 --candidate（配对模式）', file=sys.stderr)
        return 1
    try:
        start = parse_instant(args.start)
        now = parse_instant(args.now) if args.now else datetime.now(UTC)
        conn = open_db(args.db)
    except (ValueError, OSError, sqlite3.Error) as error:
        print(f'fq_us S2 报告失败：{error}', file=sys.stderr)
        return 1
    try:
        with conn:
            report = (build_pair_report(conn, args.control, args.candidate, start, now)
                      if pair else build_report(conn, args.trader_id, start, now))
    except sqlite3.Error as error:
        print(f'fq_us S2 报告失败：{error}', file=sys.stderr)
        return 1
    finally:
        conn.close()
    if args.out:
        json_path, md_path = (write_pair_report(report, Path(args.out)) if pair
                              else write_report(report, Path(args.out)))
        print(f'报告已写入 {json_path} 与 {md_path}')
    if args.ledger:
        if report['verdict'] == 'pending':
            print('判定为 pending，按约定不写 evolution_ledger')
        else:
            row = (append_pair_ledger(report, args.ledger) if pair
                   else append_ledger(report, args.ledger))
            print(f"evolution_ledger 已记账 K={row.get('K')} verdict={row.get('verdict')}")
    print(pair_summary(report) if pair else summary(report))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
