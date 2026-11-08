"""S0 statistics for the frozen 2026-11-08 macro-event-window preregistration.

Read local closed hourly candles and event times, cache all nine hit series,
and compare matched observations without executing orders or accounting P&L.
The CLI writes only report.json and summary.md in the requested output folder.
"""

from __future__ import annotations

import argparse
from bisect import bisect_left, bisect_right
import csv
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import io
import json
import math
from pathlib import Path
import random
import re
import sqlite3
from zoneinfo import ZoneInfo

import numpy as np


HOUR_MS = 3_600_000
WEEK_MS = 7 * 24 * HOUR_MS
EXCLUSION_MS = 48 * HOUR_MS
START_MS = 1_577_836_800_000  # 2020-01-01 UTC
SEED = 20261108
PLACEBO_RUNS = 1000
MAX_DRAWS = 20
KS = (1.0, 1.5, 2.5)
HORIZONS = (6, 12, 24)
MAIN = (1.5, 12)
SYMBOLS = ("BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT", "XRPUSDT", "DOGEUSDT")
EVENT_TYPES = ("FOMC", "CPI", "NFP")
EVENT_HEADER = ("event_type", "release_date_et", "release_time_et",
                "reference_period", "source", "note")
ET = ZoneInfo("America/New_York")
REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / "docs/research/2026-11-08-macro-event-window-s0-prereg.md"
NOTES = [
    "ATR 使用入场前 14 根 TR，并要求 t-15h 至 t 的 K 线连续，首个可能入场为第 16 根。",
    "所有入场和绝对收益观测均从 2020-01-01 UTC 起，且其前至少已有 15 根 K 线。",
    "提前时长分桶为 (0,1]、(1,2]、…、(5,6] 小时，包含整点发布前恰好 6 小时的入场。",
    "描述性均值按有效的币种×时点等权，每个处理观测先对其有效对照取均值。D 和 CI 按事件等权。",
    "绝对收益剖面的对照在事件 K 线开盘整点做 48h 排除，随后沿用同一对照锚点的相对位置。"
    "各相对位置单独检查当根及前收完整性，至少需要两个有效对照。",
    "ATR 为零时 hit 仍按触及定义计算，区间/ATR 的零分母观测丢弃。",
    "placebo 每个事件最多抽取 20 次，允许不同假事件重合，48h 排除始终使用全部真实事件。"
    "无有效事件的重复记为 null，分位分母只含有定义的 placebo D。",
    "事件少于两个时 SE 和 CI 为 null，缺失统计量无法满足对应判定条件。",
    "非 1000 次 placebo 或币种子集属于诊断运行，仍展示同一机械判定规则。",
    "数据库 SHA256 是主文件字节哈希，所选三列的 SQL 快照另有 SHA256，后者包含 WAL 中可见的行。",
]


@dataclass(frozen=True)
class Event:
    event_type: str
    release_ms: int
    release_date_et: str = ""
    release_time_et: str = ""
    reference_period: str = ""
    source: str = ""
    note: str = ""

    @property
    def year(self) -> str:
        return (self.release_date_et or datetime.fromtimestamp(
            self.release_ms / 1000, ET).strftime("%Y-%m-%d"))[:4]


@dataclass
class SymbolCache:
    index: dict[int, int]
    atr: np.ndarray
    hits: dict[tuple[float, int], np.ndarray]
    ranges: dict[int, np.ndarray]
    abs_returns: np.ndarray
    metadata: dict


@dataclass(frozen=True)
class Pair:
    symbol: str
    entry_ms: int
    treatment: float
    control: float
    control_count: int

    @property
    def difference(self) -> float:
        return self.treatment - self.control


def utc_text(timestamp_ms: int) -> str:
    return datetime.fromtimestamp(timestamp_ms / 1000, timezone.utc).isoformat()


def et_to_utc_ms(date: str, time: str) -> int:
    """Accept ISO local dates and HH:MM or HH:MM:SS, rejecting DST ambiguity."""
    if not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", date):
        raise ValueError(f"invalid ET date {date!r}")
    if not re.fullmatch(r"[0-9]{2}:[0-9]{2}(:[0-9]{2})?", time):
        raise ValueError(f"invalid ET time {time!r}")
    local = datetime.fromisoformat(f"{date}T{time}")
    candidates = set()
    for fold in (0, 1):
        utc = local.replace(tzinfo=ET, fold=fold).astimezone(timezone.utc)
        if utc.astimezone(ET).replace(tzinfo=None) == local:
            candidates.add(int(utc.timestamp() * 1000))
    if len(candidates) != 1:
        raise ValueError(f"ambiguous or nonexistent ET time {date} {time}")
    return candidates.pop()


def parse_events(content: str) -> list[Event]:
    reader = csv.DictReader(io.StringIO(content.lstrip("\ufeff"), newline=""), strict=True)
    if reader.fieldnames != list(EVENT_HEADER):
        raise ValueError(f"event CSV header must be {','.join(EVENT_HEADER)}")
    events = []
    seen = set()
    for row in reader:
        if None in row or any(value is None for value in row.values()):
            raise ValueError(f"event CSV line {reader.line_num}: wrong field count")
        values = {key: value.strip() for key, value in row.items()}
        if values["event_type"] not in EVENT_TYPES:
            raise ValueError(f"event CSV line {reader.line_num}: invalid event_type")
        key = (values["event_type"], values["release_date_et"])
        if key in seen:
            raise ValueError(f"event CSV line {reader.line_num}: duplicate {key}")
        try:
            timestamp = et_to_utc_ms(values["release_date_et"], values["release_time_et"])
        except ValueError as error:
            raise ValueError(f"event CSV line {reader.line_num}: {error}") from error
        events.append(Event(release_ms=timestamp, **values))
        seen.add(key)
    if not events:
        raise ValueError("event CSV contains no events")
    return sorted(events, key=lambda event: (event.release_ms, event.event_type))


def treatment_times(release_ms: int) -> tuple[int, ...]:
    first = ((release_ms - 6 * HOUR_MS + HOUR_MS - 1) // HOUR_MS) * HOUR_MS
    return tuple(range(first, release_ms, HOUR_MS))


class Calendar:
    """Cache the eight same-UTC-hour controls using the fixed real calendar."""

    def __init__(self, events: list[Event]):
        self.releases = sorted(event.release_ms for event in events)
        self._controls: dict[int, tuple[int, ...]] = {}

    def ordinary(self, timestamp_ms: int) -> bool:
        position = bisect_left(self.releases, timestamp_ms)
        neighbors = self.releases[max(0, position - 1):position + 1]
        return all(abs(timestamp_ms - release) >= EXCLUSION_MS for release in neighbors)

    def controls(self, entry_ms: int) -> tuple[int, ...]:
        if entry_ms not in self._controls:
            self._controls[entry_ms] = tuple(
                candidate for m in (1, 2, 3, 4) for sign in (-1, 1)
                if self.ordinary(candidate := entry_ms + sign * m * WEEK_MS))
        return self._controls[entry_ms]


def precompute(rows) -> SymbolCache:
    """Use compact sorted arrays and exact timestamps, never fill missing hours."""
    candles = np.asarray(rows, dtype=float)
    if candles.ndim != 2 or candles.shape[1] != 7 or not len(candles):
        raise ValueError("expected a nonempty seven-column candle array")
    if not np.isfinite(candles).all():
        raise ValueError("candles must contain finite numbers")
    raw_times = candles[:, 0]
    if np.any(raw_times < 0) or np.any(raw_times % HOUR_MS != 0):
        raise ValueError("open_ms must be nonnegative UTC hourly integers")
    times = raw_times.astype(np.int64)
    if np.any(np.diff(times) <= 0):
        raise ValueError("open_ms must be unique and strictly increasing")
    opening, high, low, close = candles[:, 1:5].T
    if (np.any(candles[:, 1:5] <= 0) or np.any(high < np.maximum(opening, close))
            or np.any(low > np.minimum(opening, close)) or np.any(candles[:, 5:] < 0)):
        raise ValueError("invalid OHLC prices or negative volumes")
    n = len(times)
    atr = np.full(n, np.nan)
    eligible = (times >= START_MS) & (np.arange(n) >= 15)
    if n > 15:
        tr = np.maximum(high[1:], close[:-1]) - np.minimum(low[1:], close[:-1])
        means = np.lib.stride_tricks.sliding_window_view(tr, 14).mean(axis=1)
        atr[15:] = means[:n - 15]
        continuous = times[15:] - times[:-15] == 15 * HOUR_MS
        atr[15:] = np.where(continuous, atr[15:], np.nan)
    hits = {}
    ranges = {}
    for horizon in HORIZONS:
        minimum = np.full(n, np.nan)
        maximum = np.full(n, np.nan)
        complete = np.zeros(n, dtype=bool)
        if n >= horizon:
            count = n - horizon + 1
            minimum[:count] = np.lib.stride_tricks.sliding_window_view(low, horizon).min(axis=1)
            maximum[:count] = np.lib.stride_tricks.sliding_window_view(high, horizon).max(axis=1)
            complete[:count] = times[horizon - 1:] - times[:count] == (horizon - 1) * HOUR_MS
        valid = eligible & complete & np.isfinite(atr)
        for k in KS:
            long_hit = minimum <= opening - k * atr
            short_hit = maximum >= opening + k * atr
            hits[k, horizon] = np.where(valid, (long_hit.astype(float) + short_hit) / 2, np.nan)
        ranges[horizon] = np.divide(maximum - minimum, atr, out=np.full(n, np.nan),
                                    where=valid & (atr > 0))
    abs_returns = np.full(n, np.nan)
    adjacent = (np.diff(times) == HOUR_MS) & eligible[1:]
    abs_returns[1:] = np.where(adjacent, np.abs(np.log(close[1:]) - np.log(close[:-1])), np.nan)
    metadata = dict(rows=n, start_ms=int(times[0]), end_ms=int(times[-1]),
                    start_utc=utc_text(int(times[0])), end_utc=utc_text(int(times[-1])),
                    missing_hours=int(np.sum(np.diff(times) // HOUR_MS - 1)),
                    gap_intervals=int(np.sum(np.diff(times) > HOUR_MS)),
                    valid_hits={f"k={k:g},H={h}": int(np.isfinite(values).sum())
                                for (k, h), values in hits.items()})
    return SymbolCache({int(t): i for i, t in enumerate(times)}, atr, hits, ranges,
                       abs_returns, metadata)


def match_pair(symbol: str, cache: SymbolCache, values: np.ndarray,
               entry_ms: int, controls: tuple[int, ...]) -> Pair | None:
    position = cache.index.get(entry_ms)
    if position is None or not math.isfinite(treatment := float(values[position])):
        return None
    control_values = []
    for timestamp in controls:
        position = cache.index.get(timestamp)
        if position is not None and math.isfinite(value := float(values[position])):
            control_values.append(value)
    if len(control_values) < 2:
        return None
    return Pair(symbol, entry_ms, treatment, sum(control_values) / len(control_values),
                len(control_values))


def event_pairs(release_ms: int, caches: dict[str, SymbolCache], calendar: Calendar,
                k: float = MAIN[0], horizon: int = MAIN[1], *, ranges: bool = False) -> list[Pair]:
    pairs = []
    for entry_ms in treatment_times(release_ms):
        controls = calendar.controls(entry_ms)
        for symbol, cache in caches.items():
            values = cache.ranges[horizon] if ranges else cache.hits[k, horizon]
            pair = match_pair(symbol, cache, values, entry_ms, controls)
            if pair is not None:
                pairs.append(pair)
    return pairs


def mean(values) -> float | None:
    values = list(values)
    return sum(values) / len(values) if values else None


def cluster_statistics(event_differences) -> dict:
    differences = list(event_differences)
    n = len(differences)
    difference = mean(differences)
    se = float(np.std(differences, ddof=1) / math.sqrt(n)) if n >= 2 else None
    ci = [difference - 1.96 * se, difference + 1.96 * se] if se is not None else [None, None]
    return dict(D=difference, SE=se, CI95=ci, N_e=n)


def summarize_events(groups: list[list[Pair]]) -> dict:
    result = cluster_statistics(mean(pair.difference for pair in group) for group in groups if group)
    result["matched_pairs"] = sum(len(group) for group in groups)
    result["excluded_events"] = len(groups) - result["N_e"]
    return result


def paired_means(pairs: list[Pair]) -> dict:
    treatment = mean(pair.treatment for pair in pairs)
    control = mean(pair.control for pair in pairs)
    ratio = treatment / control if control is not None and control > 0 else None
    return dict(treatment_mean=treatment, control_mean=control, ratio=ratio,
                matched_pairs=len(pairs), control_observations=sum(pair.control_count for pair in pairs))


def draw_placebo_times(events: list[Event], calendar: Calendar,
                       rng: random.Random) -> list[int | None]:
    shifted = []
    for event in events:
        accepted = None
        for _ in range(MAX_DRAWS):
            m = rng.choice((1, 2, 3, 4))
            sign = rng.choice((-1, 1))
            candidate = event.release_ms + sign * m * WEEK_MS
            if calendar.ordinary(candidate):
                accepted = candidate
                break
        shifted.append(accepted)
    return shifted


def placebo(events: list[Event], caches: dict[str, SymbolCache], calendar: Calendar,
            real_d: float | None, runs: int = PLACEBO_RUNS) -> dict:
    if runs < 0:
        raise ValueError("placebo runs must be nonnegative")
    # Only eight possible shifts per real event. Freeze their event-level means
    # before randomization, including missing-data exclusions and real controls.
    candidates = {event.release_ms + sign * m * WEEK_MS for event in events
                  for m in (1, 2, 3, 4) for sign in (-1, 1)}
    cached_d = {}
    for timestamp in sorted(candidates):
        if calendar.ordinary(timestamp):
            pairs = event_pairs(timestamp, caches, calendar)
            cached_d[timestamp] = mean(pair.difference for pair in pairs)
    rng = random.Random(SEED)
    differences = []
    accepted_counts = []
    contributing_counts = []
    for _ in range(runs):
        shifted = draw_placebo_times(events, calendar, rng)
        values = [cached_d[t] for t in shifted if t is not None and cached_d[t] is not None]
        differences.append(mean(values))
        accepted_counts.append(sum(t is not None for t in shifted))
        contributing_counts.append(len(values))
    valid = [value for value in differences if value is not None]
    lt = mean(float(value < real_d) for value in valid) if real_d is not None else None
    le = mean(float(value <= real_d) for value in valid) if real_d is not None else None
    return dict(seed=SEED, requested_runs=runs, valid_runs=len(valid), D=differences,
                percentile_lt=lt, percentile_le=le, accepted_events=accepted_counts,
                contributing_events=contributing_counts,
                dropped_near_real_events=[len(events) - count for count in accepted_counts],
                cached_shifted_events=len(cached_d))


def decision(main: dict, null: dict) -> dict:
    checks = dict(D_positive=main["D"] is not None and main["D"] > 0,
                  CI_lower_positive=main["CI95"][0] is not None and main["CI95"][0] > 0,
                  placebo_percentile_at_least_90=null["percentile_lt"] is not None
                  and null["percentile_lt"] >= .90)
    established = all(checks.values())
    return dict(mechanism_established=established,
                text="机制成立" if established else "机制不成立", checks=checks)


def return_profile(events: list[Event], caches: dict[str, SymbolCache], calendar: Calendar) -> list[dict]:
    result = []
    for offset in range(-6, 7):
        pairs = []
        for event in events:
            anchor = (event.release_ms // HOUR_MS) * HOUR_MS
            controls = tuple(t + offset * HOUR_MS for t in calendar.controls(anchor))
            for symbol, cache in caches.items():
                pair = match_pair(symbol, cache, cache.abs_returns,
                                  anchor + offset * HOUR_MS, controls)
                if pair is not None:
                    pairs.append(pair)
        result.append(dict(offset_hours=offset, **paired_means(pairs)))
    return result


def analyze(events: list[Event], caches: dict[str, SymbolCache], runs: int = PLACEBO_RUNS) -> dict:
    calendar = Calendar(events)
    groups = [event_pairs(event.release_ms, caches, calendar) for event in events]
    main = summarize_events(groups)
    null = placebo(events, caches, calendar, main["D"], runs)
    by_type = {kind: summarize_events([group for event, group in zip(events, groups)
                                      if event.event_type == kind]) for kind in EVENT_TYPES}
    by_year = {year: summarize_events([group for event, group in zip(events, groups)
                                      if event.year == year]) for year in sorted({e.year for e in events})}
    by_symbol = {symbol: summarize_events([[pair for pair in group if pair.symbol == symbol]
                                          for group in groups]) for symbol in caches}
    grid = []
    for k in KS:
        for horizon in HORIZONS:
            stats = main if (k, horizon) == MAIN else summarize_events([
                event_pairs(event.release_ms, caches, calendar, k, horizon) for event in events])
            grid.append(dict(k=k, H=horizon, **stats))
    lead_buckets = []
    for bucket in range(6):
        pairs = [pair for event, group in zip(events, groups) for pair in group
                 if bucket * HOUR_MS < event.release_ms - pair.entry_ms <= (bucket + 1) * HOUR_MS]
        lead_buckets.append(dict(lead_lower_hours=bucket, lead_upper_hours=bucket + 1,
                                 **paired_means(pairs)))
    range_pairs = [pair for event in events
                   for pair in event_pairs(event.release_ms, caches, calendar, ranges=True)]
    event_values = [dict(event_type=event.event_type, release_ms=event.release_ms,
                         release_utc=utc_text(event.release_ms), year=event.year,
                         d_e=mean(pair.difference for pair in group), matched_pairs=len(group))
                    for event, group in zip(events, groups)]
    return dict(main=main, placebo=null, verdict=decision(main, null), event_differences=event_values,
                by_event_type=by_type, sensitivity_grid=grid, by_lead_hours=lead_buckets,
                by_year=by_year, by_symbol=by_symbol, range_atr=paired_means(range_pairs),
                absolute_return_profile=return_profile(events, caches, calendar))


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def live_counts(path: Path, events: list[Event]) -> dict:
    """Read only the three needed columns, including both real and paper rows."""
    path = Path(path).resolve()
    file_hash = sha256_file(path)
    connection = sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)
    traders = {}
    snapshot = hashlib.sha256()
    releases = sorted(event.release_ms for event in events)
    try:
        connection.execute("PRAGMA query_only = ON")
        connection.execute("BEGIN")
        rows = connection.execute(
            "SELECT trader_id, symbol, entry_time FROM trader_positions "
            "WHERE status = 'CLOSED' AND substr(symbol, -4) = 'USDT' "
            "ORDER BY trader_id, symbol, entry_time")
        for trader_id, symbol, entry_time in rows:
            snapshot.update((json.dumps([trader_id, symbol, entry_time], ensure_ascii=False,
                                        separators=(",", ":")) + "\n").encode("utf-8"))
            trader = traders.setdefault(str(trader_id), dict(total_closed=0, in_event_window=0,
                                                            invalid_entry_time=0))
            trader["total_closed"] += 1
            valid = (isinstance(entry_time, (int, float)) and math.isfinite(entry_time)
                     and int(entry_time) == entry_time)
            if not valid:
                trader["invalid_entry_time"] += 1
                continue
            # First strictly later release implements the half-open union of
            # event windows. Overlapping windows count each trade just once.
            position = bisect_right(releases, entry_time)
            if position < len(releases) and releases[position] - entry_time <= 6 * HOUR_MS:
                trader["in_event_window"] += 1
    finally:
        connection.close()
    for trader in traders.values():
        trader["fraction"] = trader["in_event_window"] / trader["total_closed"]
    return dict(skipped=False, path=str(path), file_sha256=file_hash,
                selected_rows_sha256=snapshot.hexdigest(), traders=traders)


def event_inventory(events: list[Event]) -> dict:
    years = sorted({event.year for event in events})
    return dict(total=len(events),
                by_type={kind: sum(e.event_type == kind for e in events) for kind in EVENT_TYPES},
                by_year={year: sum(e.year == year for e in events) for year in years},
                by_year_type={year: {kind: sum(e.year == year and e.event_type == kind for e in events)
                                      for kind in EVENT_TYPES} for year in years})


def study(data_root: Path, symbols=SYMBOLS, runs: int = PLACEBO_RUNS, no_live: bool = False) -> dict:
    root = Path(data_root).resolve()
    event_path = root / "macro_events" / "macro_events.csv"
    event_content = event_path.read_bytes()
    events = parse_events(event_content.decode("utf-8-sig"))
    hashes = {str(event_path): hashlib.sha256(event_content).hexdigest()}
    caches = {}
    for symbol in symbols:
        path = root / "volrix" / "klines" / f"{symbol}_1h.json"
        content = path.read_bytes()
        try:
            caches[symbol] = precompute(json.loads(content))
        except (ValueError, TypeError) as error:
            raise ValueError(f"{path}: {error}") from error
        hashes[str(path)] = hashlib.sha256(content).hexdigest()
    live = dict(skipped=True, traders={}) if no_live else live_counts(root / "data.db", events)
    if not no_live:
        hashes[live["path"]] = live["file_sha256"]
    return dict(version=1, preregistration=dict(path=str(PREREG.relative_to(REPO)),
                                               sha256=sha256_file(PREREG)),
                parameters=dict(seed=SEED, k=MAIN[0], H=MAIN[1], atr_period=14,
                                start_ms=START_MS, start_utc=utc_text(START_MS),
                                pre_event_hours=6, control_week_multiples=[1, 2, 3, 4],
                                exclusion_hours=48, minimum_controls=2, placebo_max_draws=MAX_DRAWS,
                                ks=list(KS), horizons=list(HORIZONS), ci_multiplier=1.96,
                                placebo_percentile_threshold=.90),
                invocation=dict(data_root=str(root), symbols=list(symbols), placebo=runs, no_live=no_live),
                diagnostic_run=runs != PLACEBO_RUNS or set(symbols) != set(SYMBOLS),
                input_sha256=hashes, events=event_inventory(events),
                symbols={symbol: cache.metadata for symbol, cache in caches.items()},
                implementation_choices=NOTES, live_counts=live, **analyze(events, caches, runs))


def markdown_report(result: dict) -> str:
    def number(value):
        return "NA" if value is None else f"{value:.6f}"

    def stats_row(label, stats):
        return (f"| {label} | {number(stats['D'])} | {number(stats['SE'])} | "
                f"[{number(stats['CI95'][0])}, {number(stats['CI95'][1])}] | {stats['N_e']} |")

    def means_table(title, rows):
        output = [f"## {title}", "", "| 分组 | 处理均值 | 对照均值 | 比值 | 有效配对数 |",
                  "| --- | ---: | ---: | ---: | ---: |"]
        for label, row in rows:
            output.append(f"| {label} | {number(row['treatment_mean'])} | {number(row['control_mean'])} | "
                          f"{number(row['ratio'])} | {row['matched_pairs']} |")
        return output + [""]

    null = result["placebo"]
    lines = ["# 宏观事件窗口 S0 统计", "",
             f"机械判定为**{result['verdict']['text']}**。主参数 k=1.5、H=12。", "",
             f"Placebo 有效次数 {null['valid_runs']}/{null['requested_runs']}，种子 {SEED}，"
             f"P(假 D < 真实 D)={number(null['percentile_lt'])}，"
             f"P(假 D ≤ 真实 D)={number(null['percentile_le'])}。", ""]
    if result["diagnostic_run"]:
        lines += ["本次为诊断运行，币种集合或 placebo 次数与完整登记运行不同。", ""]
    stats_header = ["| 分组 | D | SE | 95% CI | 有效事件数 |", "| --- | ---: | ---: | --- | ---: |"]
    lines += ["## 主统计与事件类型", ""] + stats_header + [stats_row("全部", result["main"])]
    lines += [stats_row(kind, row) for kind, row in result["by_event_type"].items()] + [""]
    lines += ["## 敏感性网格", ""] + stats_header
    lines += [stats_row(f"k={row['k']:g}, H={row['H']}", row) for row in result["sensitivity_grid"]] + [""]
    for title, key in (("按年份", "by_year"), ("按币种", "by_symbol")):
        lines += [f"## {title}", ""] + stats_header
        lines += [stats_row(label, row) for label, row in result[key].items()] + [""]
    lines += means_table("提前时长触发率", [
        (f"({row['lead_lower_hours']},{row['lead_upper_hours']}] 小时", row)
        for row in result["by_lead_hours"]])
    lines += means_table("观察窗区间/ATR", [("H=12", result["range_atr"])])
    lines += means_table("事件 K 线前后绝对对数收益", [
        (f"{row['offset_hours']:+d}", row) for row in result["absolute_return_profile"]])
    lines += ["## 已平仓 USDT 交易笔数", ""]
    if result["live_counts"]["skipped"]:
        lines += ["本次使用 --no-live，跳过笔数统计。", ""]
    else:
        lines += ["| trader_id | 总笔数 | 事件窗内笔数 | 比例 |", "| --- | ---: | ---: | ---: |"]
        for trader, row in result["live_counts"]["traders"].items():
            label = trader.replace("|", "\\|").replace("\n", " ")
            lines.append(f"| {label} | {row['total_closed']} | {row['in_event_window']} | {number(row['fraction'])} |")
        lines.append("")
    lines += ["## 输入覆盖", "", "| 事件类型 | 条数 |", "| --- | ---: |"]
    lines += [f"| {kind} | {count} |" for kind, count in result["events"]["by_type"].items()]
    lines += ["", "| 年份 | FOMC | CPI | NFP | 合计 |", "| --- | ---: | ---: | ---: | ---: |"]
    lines += [f"| {year} | {counts['FOMC']} | {counts['CPI']} | {counts['NFP']} | "
              f"{result['events']['by_year'][year]} |"
              for year, counts in result["events"]["by_year_type"].items()]
    lines += ["", "| 币种 | 首根 UTC | 末根 UTC | 缺失小时数 |", "| --- | --- | --- | ---: |"]
    lines += [f"| {symbol} | {row['start_utc']} | {row['end_utc']} | {row['missing_hours']} |"
              for symbol, row in result["symbols"].items()]
    lines += ["", "## 实现口径", ""] + [f"- {note}" for note in result["implementation_choices"]]
    lines += ["", "1h 高低点触及仅刻画机制。存活币种、事件窗重叠和既往行情使用等局限见预注册。"
              "完整数值、逐事件差值和输入 SHA256 见 report.json。", ""]
    return "\n".join(lines)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data-root", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--symbols", nargs="+", default=list(SYMBOLS))
    parser.add_argument("--placebo", type=int, default=PLACEBO_RUNS)
    parser.add_argument("--no-live", action="store_true")
    args = parser.parse_args(argv)
    if args.placebo < 0:
        parser.error("--placebo must be nonnegative")
    if (len(set(args.symbols)) != len(args.symbols)
            or any(not re.fullmatch(r"[A-Z0-9]+USDT", symbol) for symbol in args.symbols)):
        parser.error("--symbols must contain unique uppercase USDT symbols")
    out = args.out.resolve()
    if out.is_relative_to(args.data_root.resolve()):
        parser.error("--out must be outside the input data directory")
    try:
        result = study(args.data_root, args.symbols, args.placebo, args.no_live)
    except (OSError, ValueError, TypeError, csv.Error, sqlite3.Error) as error:
        parser.exit(1, f"input error: {error}\n")
    encoded = json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + "\n"
    summary = markdown_report(result)
    out.mkdir(parents=True, exist_ok=True)
    (out / "report.json").write_text(encoded, encoding="utf-8")
    (out / "summary.md").write_text(summary, encoding="utf-8")
    print(f"{result['verdict']['text']}，报告已写入 {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
