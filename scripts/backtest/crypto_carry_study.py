"""Frozen funding-carry S1. Only `fetch` uses the network.

python -m scripts.backtest.crypto_carry_study --help
Native Backtrader two-feed execution is checked against a daily arithmetic oracle.
"""
from __future__ import annotations

import argparse
from bisect import bisect_left, bisect_right
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import math
from pathlib import Path
import statistics
import time
from urllib.parse import urlencode

from . import evolution_ledger, market_cache
from .backtrader_adapter import _backtrader
from .incremental_filter import weekly_statistics

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-09-crypto-funding-carry-s1-prereg.md'
PREREG_SHA256 = 'd220cabc59551e794e6b7751d49f6fd4ef8939e877fa4f3711f27bf82febdeca'
LEDGER = REPO / 'data/evolution/ledger.jsonl'
DAY = 86_400_000
HOUR = 3_600_000
PRIMARY = ('BTCUSDT', 'ETHUSDT')
SECONDARY = ('SOLUSDT', 'BNBUSDT', 'XRPUSDT', 'DOGEUSDT')
SYMBOLS = PRIMARY + SECONDARY
SPOT_FEE = .001
PERP_FEE = .0005
SLIPPAGE = .0003
ENTRY_APR = .10
EXIT_APR = .02
MARGIN_FACTOR = 1 + .25 / .75 - .005
CAPITAL = 1.  # One unit per independent sleeve, two units for the primary portfolio.
TOLERANCE = 1e-9
NOTES = [
    '每个 sleeve 初始资本归一为 1，BTC 与 ETH 初始各半，期间不再平衡。',
    '信号窗口为成交日开盘前的 [open−7日, open)，严格排除成交时刻结算。',
    '日末资金费归属 (open, close]，开仓时刻不收费，开盘退出时刻的结算已计入前一日。',
    '日线无法确定击穿时刻，统一记为当日 close，先结算 t≤close 的资金费再强平。'
    '现货强平原价取 min(现货当日最高价, 永续击穿价)，随后两腿各按方向扣滑点及手续费。',
    '击穿阈值以含滑点的永续实际开仓价计算。次日可按此前七日信号重新开仓。',
    '手续费按含滑点的实际成交额收取。成本包括手续费和相对原价的不利滑点，基差盈亏用原价。',
    'Backtrader 以原生现货多仓和等量 stocklike 空仓负债表示永续，卖空所得与负债等额抵消。'
    '经济现金为 broker现金减未平仓永续开仓名义，权益等于经济现金加现货市值加永续未实现盈亏。',
    '事件点数据源在真实开盘或强平原价上嵌入方向滑点，原生 Market/cheat-on-close 撮合。'
    '结算、成交和日末估值的同毫秒事件按顺序回放，另加成交确认点，不产生额外日收益。',
    '缺日比例分母为全部预注册 UTC 交易日。容许范围内缺日不成交，缺失腿只按此前收盘估值，'
    '不回填开高低价。缺 markPrice 且结算当日无永续收盘价时无法回退，判 invalid。',
    '结算周期由相邻间隔的局部众数分段。窗口为当前及前后三个间隔，只用直接观测的 4h/8h，'
    '并列时用最近的直接间隔，直接出现 4h 间隔时周期至多为 4h。'
    '校验允许时间戳距 4h 网格 1 秒内的交易所抖动，'
    '信号和现金流仍使用未经取整的毫秒时间。首尾缺失按相邻分段周期外推。',
    '主检验须有首个信号所需的七日前资金费记录。面板匹配分母为全部交易日，缺行计不匹配。',
    '描述币种从上市且具备七日资金费历史后开始，不把上市前日期记为零收益。',
    '期末保留持仓按收盘估值，不额外平仓。逐日核对采用 abs差/max(1,abs两侧权益)≤1e-9，'
    '资金费、成本和基差拆分也逐日核对。MaxDD 的初始峰值为初始资本。'
    '周数不足三周时 z 缺失，SE=0 时 z 为 0 或带符号无穷。',
    '资金费转负期间定义为连续 rate<0 的结算段，段末为下一次非负结算或研究期末。'
    '报告该区间日末权益相对段前日末权益的最大净亏损，以及负资金费支出。',
    '数据校验或实现核对失败仍输出 invalid 报告。描述项不替代主检验，不额外累计 K。',
]
LIMITATIONS = [
    '预注册已承认资金费曾作为因子输入，标的选择不是完全盲选，且存在幸存者偏差。',
    '日线无法还原盘中基差和强平路径。缺失日的持仓估值、结算周期推断均有不确定性。',
    '不模拟交易所对手风险、账户划转延迟及 ADL。离线结果仅用于独立策略筛选。',
]


def timestamp_ms(value):
    if isinstance(value, int) and not isinstance(value, bool):
        return value
    if not isinstance(value, str):
        raise ValueError('时间必须为整数 UTC 毫秒或 ISO 日期字符串')
    parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return int(parsed.timestamp() * 1000)


SINCE = timestamp_ms('2020-01-01T00:00:00Z')
UNTIL = timestamp_ms('2026-11-07T00:00:00Z')  # Exclusive, includes November 6.
RECENT_START = timestamp_ms('2025-11-07T00:00:00Z')
FUNDING_START = timestamp_ms('2019-09-01T00:00:00Z')  # Before BTCUSDT perp listing.


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def verify_prereg(path=None):
    raw = Path(PREREG if path is None else path).read_bytes()
    if sha256(raw) != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run/output')
    return raw


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.tmp')
    temporary.write_text(json.dumps(evolution_ledger.json_safe(value), ensure_ascii=False,
                                    indent=2, allow_nan=False) + '\n', encoding='utf-8')
    temporary.replace(path)


def validate_rows(rows, kind):
    """Reject malformed or ambiguous rows before merge or performance calculations."""
    if not isinstance(rows, list):
        raise ValueError(f'{kind} 必须为行数组')
    previous = -1
    for row in rows:
        width = 3 if kind == 'funding' else 6
        if not isinstance(row, list) or len(row) != width:
            raise ValueError(f'{kind} 行必须有 {width} 列')
        t = row[0]
        if not isinstance(t, int) or isinstance(t, bool) or t <= previous:
            raise ValueError(f'{kind} 时间重复、乱序或非整数毫秒')
        values = row[1:] if kind != 'funding' else row[1:2]
        if not all(isinstance(v, (int, float)) and not isinstance(v, bool)
                   and math.isfinite(v) for v in values):
            raise ValueError(f'{kind} 数值无效')
        if kind == 'funding':
            mark = row[2]
            if mark is not None and (not isinstance(mark, (int, float))
                                     or isinstance(mark, bool) or not math.isfinite(mark) or mark <= 0):
                raise ValueError('markPrice 必须为正数或 null')
        else:
            o, h, l, c, volume = row[1:]
            if t % DAY or min(o, h, l, c) <= 0 or not l <= min(o, c) <= max(o, c) <= h or volume < 0:
                raise ValueError(f'{kind} 日线对齐或 OHLCV 无效')
        previous = t


def file_metadata(path, rows):
    return dict(sha256=sha256(Path(path).read_bytes()), rows=len(rows),
                start_ms=rows[0][0] if rows else None, end_ms=rows[-1][0] if rows else None)


def settlement_periods(times):
    """Local modal adjacent intervals, then contiguous constant-period segments.

    Long gaps never vote for a longer regime. A single missing 4h settlement
    therefore remains a missing settlement when surrounded by 4h observations.
    """
    gaps = [b - a for a, b in zip(times, times[1:])]
    periods = []
    for i in range(len(gaps)):
        neighbors = [(j, gaps[j]) for j in range(max(0, i - 3), min(len(gaps), i + 4))
                     if gaps[j] in (4 * HOUR, 8 * HOUR)]
        counts = Counter(value for _, value in neighbors)
        if not counts:
            raise ValueError('无法从相邻间隔众数识别 4h/8h 结算周期')
        maximum = max(counts.values())
        candidates = {value for value, count in counts.items() if count == maximum}
        mode = min((abs(j - i), j, value) for j, value in neighbors if value in candidates)[2]
        # A directly observed 4h gap cannot belong to an 8h regime, including
        # very short-lived 4h regimes near a boundary of the modal window.
        periods.append(min(mode, gaps[i]) if gaps[i] in (4 * HOUR, 8 * HOUR) else mode)
    return periods


def funding_coverage(rows, start, end):
    """Count missing settlements, including sample boundaries, by inferred regime."""
    grid = 4 * HOUR
    times = [round(row[0] / grid) * grid for row in rows]
    if (len(times) < 2 or len(set(times)) != len(times)
            or any(abs(t - row[0]) > 1000 for t, row in zip(times, rows))):
        raise ValueError('资金费时间不符合 4h/8h 网格，或结算样本不足/重复')
    periods = settlement_periods(times)
    missing = set()
    inconsistent = []
    segments = []
    for i, period in enumerate(periods):
        left, right = times[i:i + 2]
        if not segments or segments[-1]['period_hours'] != period // HOUR:
            segments.append(dict(start_ms=left, end_ms=right, period_hours=period // HOUR))
        else:
            segments[-1]['end_ms'] = right
        if right > start and left < end:
            if (right - left) % period:
                inconsistent.append([left, right, period])
            missing.update(t for t in range(left + period, min(right, end), period) if t >= start)
    if times[0] > start:
        missing.update(t for t in range(times[0] - periods[0], start - 1, -periods[0]) if t < end)
    if times[-1] < end:
        missing.update(t for t in range(times[-1] + periods[-1], end, periods[-1]) if t >= start)
    observed = sum(start <= t < end for t in times)
    expected = observed + len(missing)
    fraction = len(missing) / expected if expected else 1.
    return dict(valid=not inconsistent and fraction <= .005, observed=observed, expected=expected,
                missing=len(missing), missing_fraction=fraction, inconsistent_intervals=inconsistent,
                segments=segments)


def fetch_data(data_dir, sleep=.3, *, http=None, fetch_perp=None, sleeper=time.sleep, now_ms=None):
    """Incremental full-listing fetch. Dependencies are injectable for offline tests.

    Successful requests are merged atomically per file. Historical rows win ties.
    The persisted prefix flag avoids re-fetching the pre-listing empty range.
    Futures pagination uses market_cache's built-in >=0.3s HTTP pacing.
    --sleep also separates high-level requests, including different files.
    """
    if not math.isfinite(sleep) or sleep < .3:
        raise ValueError('--sleep 必须至少为 0.3 秒')
    http = market_cache.http_json if http is None else http
    fetch_perp = market_cache.fetch_binance_klines if fetch_perp is None else fetch_perp
    now = int(time.time() * 1000) if now_ms is None else now_ms
    base = Path(data_dir)
    manifest_path = base / 'manifest.json'
    manifest = json.loads(manifest_path.read_bytes()) if manifest_path.exists() else dict(version=1, files={})
    if not isinstance(manifest, dict) or not isinstance(manifest.get('files'), dict):
        raise ValueError('manifest 必须包含 files 对象')
    requested = False

    def request(call, *args, **kwargs):
        nonlocal requested
        if requested:
            sleeper(sleep)
        requested = True
        return call(*args, **kwargs)

    for symbol in SYMBOLS:
        for kind in ('funding', 'spot_1d', 'perp_1d'):
            name = f'{kind}/{symbol}.json'
            path = base / name
            rows = json.loads(path.read_bytes()) if path.exists() else []
            validate_rows(rows, kind)
            cached = {row[0]: row for row in rows}
            recorded = manifest['files'].get(name, {})
            if not isinstance(recorded, dict) or (rows and any(
                    recorded.get(k) != v for k, v in file_metadata(path, rows).items())):
                recorded = {}  # Untrusted metadata cannot hide a missing prefix.
            ranges = []
            # Binance treats fundingRate startTime=0 as "absent" and returns the
            # latest 500 rows, so funding pages from the first USDT-M listing
            # and always re-checks its prefix (a cached tail is not a listing).
            first = FUNDING_START if kind == 'funding' else 0
            if not rows:
                ranges.append((first, now))
            else:
                if kind == 'funding' or not recorded.get('from_listing'):
                    ranges.append((first, rows[0][0]))
                if kind == 'funding':
                    grid_times = [round(row[0] / (4 * HOUR)) * (4 * HOUR) for row in rows]
                    periods = settlement_periods(grid_times) if len(rows) > 1 else []
                    ranges.extend((a[0] + 1, b[0]) for a, b, period in zip(rows, rows[1:], periods)
                                  if b[0] - a[0] > period + 1000)
                else:
                    ranges.extend((a[0] + DAY, b[0]) for a, b in zip(rows, rows[1:]) if b[0] - a[0] > DAY)
                # Current-day bars are still forming. Keep cached bytes and only
                # download fully completed new daily bars on subsequent runs.
                ranges.append((rows[-1][0] + (1 if kind == 'funding' else DAY), now))
            for start, end in ranges:
                cursor = start
                while cursor < end:
                    if kind == 'funding':
                        query = urlencode(dict(symbol=symbol, startTime=cursor, endTime=end - 1, limit=1000))
                        page = request(http, 'https://fapi.binance.com/fapi/v1/fundingRate?' + query,
                                       proxy=market_cache.DEFAULT_PROXY)
                        if not isinstance(page, list):
                            raise ValueError(f'{name} 下载响应必须为数组')
                        downloaded = [[int(r['fundingTime']), float(r['fundingRate']),
                                       float(r['markPrice']) if r.get('markPrice') not in (None, '') else None]
                                      for r in page]
                        limit, step = 1000, 1
                    elif kind == 'spot_1d':
                        query = urlencode(dict(symbol=symbol, interval='1d', startTime=cursor,
                                               endTime=end - 1, limit=1000))
                        page = request(http, 'https://api.binance.com/api/v3/klines?' + query,
                                       proxy=market_cache.DEFAULT_PROXY)
                        if not isinstance(page, list):
                            raise ValueError(f'{name} 下载响应必须为数组')
                        downloaded = [[int(r[0]), *map(float, r[1:6])] for r in page]
                        limit, step = 1000, DAY
                    else:
                        downloaded = request(fetch_perp, symbol, '1d', cursor, end,
                                             proxy=market_cache.DEFAULT_PROXY, page_limit=1500)
                        downloaded = [row[:6] for row in downloaded]
                        limit, step = 1500, DAY
                    validate_rows(downloaded, kind)
                    if not downloaded:
                        break
                    nxt = downloaded[-1][0] + step
                    if nxt <= cursor:
                        raise ValueError(f'{name} 下载分页未推进')
                    for row in downloaded:
                        complete = kind == 'funding' or row[0] + DAY <= now
                        if start <= row[0] < end and complete:
                            cached.setdefault(row[0], row)
                    rows = [cached[t] for t in sorted(cached)]
                    write_json(path, rows)
                    manifest['files'][name] = dict(file_metadata(path, rows), from_listing=False)
                    write_json(manifest_path, manifest)
                    cursor = nxt
                    if len(downloaded) < limit:
                        break
            rows = [cached[t] for t in sorted(cached)]
            write_json(path, rows)
            manifest['files'][name] = dict(file_metadata(path, rows), from_listing=True, fetched_until_ms=now)
            write_json(manifest_path, manifest)
    return manifest


def load_symbol(data_dir, panel_daily, symbol, manifest):
    data, hashes = {}, {}
    for kind in ('funding', 'spot_1d', 'perp_1d'):
        name = f'{kind}/{symbol}.json'
        path = Path(data_dir) / name
        rows = json.loads(path.read_bytes())
        validate_rows(rows, kind)
        actual = file_metadata(path, rows)
        recorded = manifest.get('files', {}).get(name, {})
        if not isinstance(recorded, dict) or any(recorded.get(k) != v for k, v in actual.items()):
            raise ValueError(f'{name} manifest 哈希、行数或区间不一致')
        hashes[name] = actual['sha256']
        data[kind] = rows
    panel_path = Path(panel_daily) / f'{symbol}.json'
    raw = panel_path.read_bytes()
    panel_rows = json.loads(raw)
    if not isinstance(panel_rows, list):
        raise ValueError('因子面板必须为行数组')
    closes = {}
    for row in panel_rows:
        if not isinstance(row, list) or len(row) < 5:
            raise ValueError('因子面板行必须含 date,open,high,low,close')
        t, close = timestamp_ms(row[0]), float(row[4])
        if t in closes or t % DAY or not math.isfinite(close) or close <= 0:
            raise ValueError('因子面板收盘或日期重复/无效')
        closes[t] = close
    data['panel_close'] = closes
    hashes['panel_daily/' + panel_path.name] = sha256(raw)
    return data, hashes


def validate_data(data, start, end):
    if end <= start or start % DAY or end % DAY:
        raise ValueError('校验区间必须为非空 UTC 整日区间')
    reasons, details = [], {}
    days = list(range(start, end, DAY))
    for kind in ('spot_1d', 'perp_1d'):
        indexed = {r[0]: r for r in data[kind]}
        missing = sum(t not in indexed for t in days)
        details[kind] = dict(missing=missing, expected=len(days), missing_fraction=missing / len(days))
        if missing / len(days) > .005:
            reasons.append(f'{kind} 日线缺失超过 0.5%')
        if not any(t <= start for t in indexed):
            reasons.append(f'{kind} 缺少首日及此前估值价格')
    try:
        details['funding'] = funding_coverage(data['funding'], start, end)
        if not details['funding']['valid']:
            reasons.append('资金费结算缺失超过 0.5% 或结算间隔不一致')
        if not data['funding'] or data['funding'][0][0] > start - 7 * DAY:
            reasons.append('缺少首个信号所需的七日前资金费历史')
    except ValueError as exc:
        reasons.append(str(exc))
    perp = {r[0]: r for r in data['perp_1d']}
    panel = data['panel_close']
    matched = sum(t in perp and t in panel and abs(perp[t][4] / panel[t] - 1) <= .001 + 1e-12 for t in days)
    details['panel'] = dict(matched=matched, expected=len(days), matched_fraction=matched / len(days))
    if matched / len(days) < .99:
        reasons.append('永续与因子面板收盘价偏差≤0.1%的比例低于 99%')
    return dict(valid=not reasons, reasons=reasons, details=details)


def signal_apr(funding, open_ms):
    """Decision at previous close, with a half-open seven-calendar-day window."""
    times = [row[0] for row in funding]
    left, right = bisect_left(times, open_ms - 7 * DAY), bisect_left(times, open_ms)
    return math.fsum(row[1] for row in funding[left:right]) / 7 * 365


def prepare_days(spot, perp, start, end):
    maps = [{r[0]: r for r in rows} for rows in (spot, perp)]
    previous = []
    for mapping in maps:
        eligible = [t for t in mapping if t < start]
        previous.append(mapping[max(eligible)][4] if eligible else None)
    days = []
    for t in range(start, end, DAY):
        bars = [mapping.get(t) for mapping in maps]
        closes = [bar[4] if bar is not None else prior for bar, prior in zip(bars, previous)]
        if any(close is None for close in closes):
            raise ValueError('缺少可以向前沿用的日末估值')
        days.append(dict(time_ms=t, spot=bars[0], perp=bars[1], closes=closes, tradable=all(bars)))
        previous = closes
    return days


def funding_events(funding, opened, closed, perp):
    """No entry-time cash flow. Midnight fallback uses its OWN UTC day's close."""
    times = [r[0] for r in funding]
    events = []
    for t, rate, mark in funding[bisect_right(times, opened):bisect_right(times, closed)]:
        fallback = mark is None
        if fallback:
            day = t // DAY * DAY
            if day not in perp:
                raise ValueError(f'{t} markPrice 缺失且结算当日无永续收盘价')
            mark = perp[day][4]
        events.append(dict(time_ms=t, rate=rate, mark_price=mark, fallback=fallback))
    return events


def arithmetic_reference(spot, perp, funding, start, end, *, entry_apr=ENTRY_APR,
                         exit_apr=EXIT_APR, always=False, zero_cost=False, capital=CAPITAL):
    """Independent daily economic cash/spot/perpetual-PnL oracle, never scored."""
    days = prepare_days(spot, perp, start, end)
    perp_map = {r[0]: r for r in perp}
    cash, q = capital, 0.
    spot_entry = perp_entry = spot_raw_entry = perp_raw_entry = 0.
    funding_total = costs = realized_basis = 0.
    curve = []
    fee_s, fee_p, slip = (0., 0., 0.) if zero_cost else (SPOT_FEE, PERP_FEE, SLIPPAGE)
    for day in days:
        t = day['time_ms']
        apr = signal_apr(funding, t)
        if day['tradable']:
            so, po = day['spot'][1], day['perp'][1]
            if q and not always and apr < exit_apr:
                sx, px = so * (1 - slip), po * (1 + slip)
                cash += q * sx + q * (perp_entry - px) - q * (sx * fee_s + px * fee_p)
                costs += q * ((so + po) * slip + sx * fee_s + px * fee_p)
                realized_basis += q * ((so - spot_raw_entry) + (perp_raw_entry - po))
                q = 0.
            elif not q and (always or apr >= entry_apr) and cash > 0:
                q = .75 * cash / so
                spot_entry, perp_entry = so * (1 + slip), po * (1 - slip)
                spot_raw_entry, perp_raw_entry = so, po
                cash -= q * spot_entry + q * (spot_entry * fee_s + perp_entry * fee_p)
                costs += q * ((so + po) * slip + spot_entry * fee_s + perp_entry * fee_p)
        if q:
            for event in funding_events(funding, t, t + DAY, perp_map):
                flow = q * event['rate'] * event['mark_price']
                cash += flow
                funding_total += flow
            threshold = perp_entry * MARGIN_FACTOR
            if day['tradable'] and day['perp'][2] >= threshold:
                so, po = min(day['spot'][2], threshold), threshold
                sx, px = so * (1 - slip), po * (1 + slip)
                cash += q * sx + q * (perp_entry - px) - q * (sx * fee_s + px * fee_p)
                costs += q * ((so + po) * slip + sx * fee_s + px * fee_p)
                realized_basis += q * ((so - spot_raw_entry) + (perp_raw_entry - po))
                q = 0.
        sc, pc = day['closes']
        equity = cash + q * sc + q * (perp_entry - pc)
        basis = realized_basis + q * ((sc - spot_raw_entry) + (perp_raw_entry - pc))
        curve.append(dict(time_ms=t, equity=equity, funding=funding_total, costs=costs, basis_pnl=basis))
    return curve


def execution_events(spot, perp, funding, start, end, *, entry_apr=ENTRY_APR,
                     exit_apr=EXIT_APR, always=False, zero_cost=False):
    """Build a price-only event tape. Position sizes/equity come from the broker.

    This control path does not call the arithmetic oracle. Orders are known from
    causal APR and fixed raw OHLC; a daily high is used solely for that day's
    preregistered margin rule, with its explicitly disclosed end-of-day timing.
    """
    events = []
    holding, entry_price = False, None
    perp_map = {r[0]: r for r in perp}
    slip = 0. if zero_cost else SLIPPAGE
    for day in prepare_days(spot, perp, start, end):
        t, closes = day['time_ms'], day['closes']
        apr = signal_apr(funding, t)
        if day['tradable']:
            raw = [day['spot'][1], day['perp'][1]]
            opening = not holding and (always or apr >= entry_apr)
            exiting = holding and not always and apr < exit_apr
            if opening or exiting:
                kind = 'entry' if opening else 'exit'
                prices = [raw[0] * (1 + (slip if opening else -slip)),
                          raw[1] * (1 + (-slip if opening else slip))]
                events.append(dict(kind=kind, time_ms=t, prices=prices, raw=raw, reason='signal', apr=apr))
                events.append(dict(kind='settle', time_ms=t, prices=raw))
                holding = opening
                if opening:
                    entry_price = prices[1]
        exposed = holding
        if holding:
            for event in funding_events(funding, t, t + DAY, perp_map):
                events.append(dict(event, kind='funding', prices=closes))
            threshold = entry_price * MARGIN_FACTOR
            if day['tradable'] and day['perp'][2] >= threshold:
                raw = [min(day['spot'][2], threshold), threshold]
                events.append(dict(kind='exit', time_ms=t + DAY, raw=raw,
                                   prices=[raw[0] * (1 - slip), raw[1] * (1 + slip)], reason='margin'))
                events.append(dict(kind='settle', time_ms=t + DAY, prices=closes))
                holding = False
        events.append(dict(kind='mark', time_ms=t + DAY, day_ms=t, prices=closes, exposed=exposed))
    return events


def simulate_sleeve(spot, perp, funding, start, end, *, entry_apr=ENTRY_APR,
                    exit_apr=EXIT_APR, always=False, zero_cost=False, capital=CAPITAL):
    """Two native feeds, native fees/positions/MTM and broker.add_cash funding."""
    bt = _backtrader()
    options = dict(entry_apr=entry_apr, exit_apr=exit_apr, always=always, zero_cost=zero_cost)
    events = execution_events(spot, perp, funding, start, end, **options)
    if not events:
        raise ValueError('回测区间为空')

    class EventFeed(bt.feed.DataBase):
        params = (('leg', 0),)

        def start(self):
            super().start()
            self.events = iter(events)

        def _load(self):
            event = next(self.events, None)
            if event is None:
                return False
            self.lines.datetime[0] = bt.date2num(datetime.fromtimestamp(event['time_ms'] / 1000, timezone.utc))
            price = event['prices'][self.p.leg]
            for line in (self.lines.open, self.lines.high, self.lines.low, self.lines.close):
                line[0] = price
            self.lines.volume[0] = self.lines.openinterest[0] = 0
            return True

    class CarryReplay(bt.Strategy):
        def __init__(self):
            self.curve, self.fills, self.funding_flows = [], [], []
            self.funding_total = self.costs = self.raw_cashflow = 0.
            self.entries = self.exits = self.margin_breaches = self.exposed_days = 0

        def next(self):
            event = events[len(self.data0) - 1]
            kind = event['kind']
            q = self.getposition(self.data0).size
            if kind in ('entry', 'exit'):
                if kind == 'entry':
                    if self.broker.getvalue() <= 0:
                        raise ValueError('sleeve 权益非正，无法按预注册重新开仓')
                    q = .75 * self.broker.getvalue() / event['raw'][0]
                for leg, data in enumerate(self.datas):
                    buying = (kind == 'entry') == (leg == 0)
                    order = (self.buy if buying else self.sell)(data=data, size=q, exectype=bt.Order.Market)
                    order.addinfo(event=event, leg=leg)
            elif kind == 'funding':
                flow = q * event['rate'] * event['mark_price']
                self.broker.add_cash(flow)
                self.funding_total += flow
                self.funding_flows.append(dict(time_ms=event['time_ms'], cashflow=flow,
                                               rate=event['rate'], fallback=event['fallback']))
            elif kind == 'mark':
                short = self.getposition(self.data1)
                equity = self.broker.getvalue()
                cash = self.broker.getcash() + short.size * short.price
                economic_value = cash + q * event['prices'][0] + short.size * (event['prices'][1] - short.price)
                if abs(equity - economic_value) > TOLERANCE * max(1., abs(equity)):
                    raise ValueError('Backtrader 权益与经济现金恒等式不一致')
                basis = self.raw_cashflow + q * event['prices'][0] + short.size * event['prices'][1]
                self.curve.append(dict(time_ms=event['day_ms'], equity=equity, cash=cash, quantity=q,
                                       funding=self.funding_total, costs=self.costs, basis_pnl=basis))
                self.exposed_days += event['exposed']

        def notify_order(self, order):
            if order.status == bt.Order.Completed:
                event, leg = order.info['event'], order.info['leg']
                price, size = order.executed.price, order.executed.size
                expected, raw = event['prices'][leg], event['raw'][leg]
                if abs(price - expected) > 4 * math.ulp(expected):
                    raise ValueError('Backtrader 未按指定开盘/强平价格成交')
                self.costs += order.executed.comm + abs(size) * abs(price - raw)
                self.raw_cashflow -= size * raw
                self.fills.append(dict(kind=event['kind'], reason=event['reason'], leg=('spot', 'perp')[leg],
                                       time_ms=event['time_ms'], price=price, raw_price=raw,
                                       size=size, commission=order.executed.comm))
                if leg == 0:
                    self.entries += event['kind'] == 'entry'
                    self.exits += event['kind'] == 'exit'
                    self.margin_breaches += event['reason'] == 'margin'
            elif order.status in (bt.Order.Margin, bt.Order.Rejected, bt.Order.Canceled, bt.Order.Expired):
                raise ValueError(f'Backtrader 订单失败 {order.getstatusname()}')

    cerebro = bt.Cerebro(stdstats=False)
    for leg, name in enumerate(('spot', 'perp')):
        cerebro.adddata(EventFeed(leg=leg), name=name)
        fee = 0. if zero_cost else (SPOT_FEE, PERP_FEE)[leg]
        cerebro.broker.setcommission(commission=fee, stocklike=True, name=name)
    cerebro.addstrategy(CarryReplay)
    cerebro.broker.setcash(capital)
    cerebro.broker.set_coc(True)
    replay = cerebro.run(runonce=False)[0]
    reference = arithmetic_reference(spot, perp, funding, start, end, capital=capital, **options)
    reconciliation = reconcile_curves(replay.curve, reference, capital)
    if not reconciliation['valid']:
        raise ValueError(f'Backtrader 与算术参考或拆分不一致 {reconciliation}')
    return dict(initial_capital=capital, daily=replay.curve, fills=replay.fills,
                funding_events=replay.funding_flows, mark_fallbacks=sum(e['fallback'] for e in replay.funding_flows),
                entries=replay.entries, exits=replay.exits, margin_breaches=replay.margin_breaches,
                holding_days=replay.exposed_days, holding_fraction=replay.exposed_days / len(reference),
                reconciliation=reconciliation, engine='backtrader', engine_version=bt.__version__)


def reconcile_curves(actual, reference, capital):
    """Relative-or-absolute tolerance, with an independent PnL decomposition check."""
    worst = 0.
    if len(actual) != len(reference) or not actual:
        return dict(valid=False, max_scaled_error=None)
    for a, b in zip(actual, reference):
        if a['time_ms'] != b['time_ms']:
            return dict(valid=False, max_scaled_error=None)
        scale = max(1., abs(a['equity']), abs(b['equity']))
        for key in ('equity', 'funding', 'costs', 'basis_pnl'):
            error = abs(a[key] - b[key]) / scale
            if not math.isfinite(error):
                return dict(valid=False, max_scaled_error=None)
            worst = max(worst, error)
        error = abs(a['equity'] - capital - (a['funding'] - a['costs'] + a['basis_pnl'])) / scale
        if not math.isfinite(error):
            return dict(valid=False, max_scaled_error=None)
        worst = max(worst, error)
    return dict(valid=worst <= TOLERANCE, max_scaled_error=worst, tolerance=TOLERANCE)


def daily_metrics(curve, capital):
    samples, returns = [], []
    previous, peak, max_dd = capital, capital, 0.
    for row in curve:
        if previous <= 0:
            raise ValueError('前一日权益非正，日收益无定义')
        if not math.isfinite(row['equity']):
            raise ValueError('日末权益非有限数，日收益无定义')
        value = row['equity'] / previous - 1
        returns.append(value)
        samples.append((datetime.fromtimestamp(row['time_ms'] / 1000, timezone.utc), value))
        peak = max(peak, row['equity'])
        max_dd = max(max_dd, 1 - row['equity'] / peak)
        previous = row['equity']
    weekly = weekly_statistics(samples)
    avg, se = weekly['avg_r'], weekly['se']
    z = None if se is None else (avg / se if se else (0. if avg == 0 else math.copysign(math.inf, avg)))
    stdev = statistics.stdev(returns) if len(returns) >= 2 else None
    annual = lambda values: statistics.mean(values) * 365 if values else None
    half = len(returns) // 2
    return dict(n=len(returns), annual_return=annual(returns), annual_volatility=stdev * math.sqrt(365) if stdev is not None else None,
                sharpe=statistics.mean(returns) / stdev * math.sqrt(365) if stdev else None,
                z=z, se=se, n_weeks=weekly['n_weeks'], max_dd=max_dd,
                first_half_annual=annual(returns[:half]), second_half_annual=annual(returns[half:]),
                recent_annual=annual([r for row, r in zip(curve, returns) if RECENT_START <= row['time_ms'] < UNTIL]),
                total_return=previous / capital - 1, final_equity=previous,
                daily_returns=[dict(time_ms=row['time_ms'], return_=r) for row, r in zip(curve, returns)])


def decide_verdict(stats, valid=True):
    if not valid or not stats:
        return 'invalid'
    annual, first, second = (stats[k] for k in ('annual_return', 'first_half_annual', 'second_half_annual'))
    positive = all(v is not None and v > 0 for v in (annual, first, second))
    if (positive and annual >= .05 and stats['z'] is not None and stats['z'] >= 1.96
            and stats['recent_annual'] is not None and stats['recent_annual'] > 0 and stats['max_dd'] <= .10):
        return 'pass'
    return 'weak' if positive else 'fail'


def combine_sleeves(sleeves):
    dates = [[r['time_ms'] for r in sleeve['daily']] for sleeve in sleeves]
    if not dates or any(values != dates[0] for values in dates):
        raise ValueError('主 sleeve 日历不一致')
    curve = [dict(time_ms=t, **{key: math.fsum(s['daily'][i][key] for s in sleeves)
                               for key in ('equity', 'funding', 'costs', 'basis_pnl')})
             for i, t in enumerate(dates[0])]
    capital = math.fsum(s['initial_capital'] for s in sleeves)
    stats = daily_metrics(curve, capital)
    last = curve[-1]
    stats.update(funding_income=last['funding'], costs=last['costs'], basis_pnl=last['basis_pnl'],
                 total_pnl=last['equity'] - capital,
                 entries=sum(s['entries'] for s in sleeves), exits=sum(s['exits'] for s in sleeves),
                 margin_breaches=sum(s['margin_breaches'] for s in sleeves),
                 mark_fallbacks=sum(s['mark_fallbacks'] for s in sleeves),
                 holding_fraction=sum(s['holding_days'] for s in sleeves) / sum(len(s['daily']) for s in sleeves))
    return dict(statistics=stats, daily=curve)


def yearly_returns(curve, capital):
    years = {}
    previous = capital
    for row in curve:
        year = str(datetime.fromtimestamp(row['time_ms'] / 1000, timezone.utc).year)
        years.setdefault(year, []).append(row['equity'] / previous - 1)
        previous = row['equity']
    return {year: dict(n=len(values), annual_return=statistics.mean(values) * 365,
                       compounded_return=math.prod(1 + v for v in values) - 1) for year, values in years.items()}


def negative_funding_loss(sleeve, funding, end):
    curve = sleeve['daily']
    times = [r['time_ms'] + DAY for r in curve]
    periods, begin = [], None
    for t, rate, _ in funding:
        if rate < 0 and begin is None:
            begin = t
        elif rate >= 0 and begin is not None:
            periods.append((begin, t))
            begin = None
    if begin is not None:
        periods.append((begin, end))
    losses = []
    start = curve[0]['time_ms']
    for left, right in periods:
        left, right = max(left, start), min(right, end)
        if left >= right:
            continue
        before = bisect_left(times, left) - 1
        base = curve[before]['equity'] if before >= 0 else sleeve['initial_capital']
        values = [r['equity'] for r in curve if left <= r['time_ms'] + DAY <= right]
        loss = max(0., base - min([base, *values]))
        losses.append(dict(start_ms=left, end_ms=right, max_loss=loss, max_loss_fraction=loss / base if base > 0 else None))
    return dict(max_loss=max((p['max_loss'] for p in losses), default=0.),
                max_loss_fraction=max((p['max_loss_fraction'] for p in losses if p['max_loss_fraction'] is not None), default=0.),
                negative_funding_paid=-math.fsum(min(0., e['cashflow']) for e in sleeve['funding_events']), periods=losses)


def study(data_dir, panel_daily):
    before = verify_prereg()
    base = Path(data_dir)
    result = dict(version=1, engine='backtrader', prereg_sha256=PREREG_SHA256, K=1, z_K=1.96,
                  track='crypto_carry', change_type='E', idea_id='crypto-funding-carry-v1',
                  trials=0, sleeve_runs_attempted=0, inputs={}, validation={}, invalid_reasons=[], verdict='invalid',
                  statistics=None, primary=None, descriptions=None, notes=NOTES, limitations=LIMITATIONS)
    try:
        raw = (base / 'manifest.json').read_bytes()
        manifest = json.loads(raw)
        if not isinstance(manifest.get('files'), dict):
            raise ValueError('manifest 必须包含 files 对象')
        result['inputs']['manifest_sha256'] = sha256(raw)
    except (OSError, ValueError, TypeError, AttributeError) as exc:
        result['invalid_reasons'].append(f'manifest 无效 {exc}')
        return result
    loaded = {}
    for symbol in PRIMARY:
        try:
            data, hashes = load_symbol(base, panel_daily, symbol, manifest)
            result['inputs'].update(hashes)
            loaded[symbol] = data
            validation = validate_data(data, SINCE, UNTIL)
            result['validation'][symbol] = validation
            result['invalid_reasons'].extend(f'{symbol} {r}' for r in validation['reasons'])
        except (OSError, ValueError, TypeError, KeyError, IndexError) as exc:
            result['invalid_reasons'].append(f'{symbol} 输入无效 {exc}')
    if not result['invalid_reasons']:
        def execute(symbol, start=SINCE, **options):
            data = loaded[symbol]
            result['sleeve_runs_attempted'] += 1
            return simulate_sleeve(data['spot_1d'], data['perp_1d'], data['funding'], start, UNTIL, **options)

        try:
            result['trials'] = 1
            sleeves = {s: execute(s) for s in PRIMARY}
            main = combine_sleeves(list(sleeves.values()))
            result['primary'] = dict(main, sleeves=sleeves)
            result['statistics'] = main['statistics']
            result['verdict'] = decide_verdict(result['statistics'])
            descriptions = dict(yearly=yearly_returns(main['daily'], 2 * CAPITAL),
                                negative_funding={s: negative_funding_loss(sleeves[s], loaded[s]['funding'], UNTIL) for s in PRIMARY},
                                secondary={}, sensitivity=[])
            result['descriptions'] = descriptions
            for name, options in (('always_hold', dict(always=True)), ('zero_cost', dict(zero_cost=True))):
                descriptions[name] = combine_sleeves([execute(s, **options) for s in PRIMARY])
            descriptions['sensitivity'].append(dict(entry_apr=ENTRY_APR, exit_apr=EXIT_APR,
                                                    statistics=main['statistics'], primary=True))
            for entry in (.05, .15):
                for exit_ in (0., .05):
                    comparison = combine_sleeves([execute(s, entry_apr=entry, exit_apr=exit_) for s in PRIMARY])
                    descriptions['sensitivity'].append(dict(entry_apr=entry, exit_apr=exit_, primary=False, **comparison))
            for symbol in SECONDARY:
                try:
                    data, hashes = load_symbol(base, panel_daily, symbol, manifest)
                    loaded[symbol] = data
                    result['inputs'].update(hashes)
                    first = max(SINCE, data['spot_1d'][0][0], data['perp_1d'][0][0], data['funding'][0][0] + 7 * DAY)
                    start = (first + DAY - 1) // DAY * DAY
                    if start >= UNTIL:
                        raise ValueError('上市后没有具备七日历史的可用交易日')
                    validation = validate_data(data, start, UNTIL)
                    if validation['valid']:
                        sleeve = execute(symbol, start=start)
                        descriptions['secondary'][symbol] = dict(verdict='descriptive_only', start_ms=start,
                            validation=validation, sleeve=sleeve, **combine_sleeves([sleeve]),
                            negative_funding=negative_funding_loss(sleeve, data['funding'], UNTIL))
                    else:
                        descriptions['secondary'][symbol] = dict(verdict='invalid', validation=validation)
                except (OSError, ValueError, TypeError, KeyError, IndexError) as exc:
                    descriptions['secondary'][symbol] = dict(verdict='invalid', reason=str(exc))
        except (ValueError, RuntimeError) as exc:
            result['invalid_reasons'].append(str(exc))
            result['verdict'] = 'invalid'
    if verify_prereg() != before:
        raise ValueError('Preregistration changed during study; refusing output')
    return result


def report(result):
    def summary(value):
        if isinstance(value, dict):
            return {k: summary(v) for k, v in value.items() if k not in ('daily', 'daily_returns', 'fills', 'funding_events', 'sleeve', 'sleeves')}
        if isinstance(value, list):
            return [summary(v) for v in value]
        return value

    lines = ['# 资金费 carry 离线 S1', '', f'判定 `{result["verdict"]}`。独立策略主检验 BTC 与 ETH 等初始资本。', '',
             f'预注册 SHA256 `{PREREG_SHA256}`。track `crypto_carry`，类型 E，K=1，z 门槛 1.96。', '',
             f'主检验尝试次数 {result["trials"]}。始终持有、零成本、四个敏感性组合和四个单币结果仅供描述。']
    lines += ['', f'包含描述项的 sleeve 回放调用共 {result["sleeve_runs_attempted"]} 次。成功回放均通过参考核对。']
    if result['invalid_reasons']:
        lines += ['', '## 无效原因', ''] + ['- ' + r for r in result['invalid_reasons']]
    for title, value in (('输入校验', result['validation']), ('主统计与收益拆分', result['statistics']),
                         ('描述项', result['descriptions']), ('冻结输入', result['inputs'])):
        if value is not None:
            lines += ['', '## ' + title, '', '```json',
                      json.dumps(evolution_ledger.json_safe(summary(value)), ensure_ascii=False, indent=2), '```']
    action = {'pass': '值得模拟验证。现货腿和模拟盘接入需另行设计并取得用户同意。',
              'weak': '有信号但不足以接入，保留研究记录。', 'fail': '不接入，保留失败结果。',
              'invalid': '数据或实现核对未通过，证据不足。'}[result['verdict']]
    lines += ['', '## 后续', '', action, '', '## 执行口径与补充约定', '']
    lines += ['- ' + note for note in NOTES]
    lines += ['', '## 局限', ''] + ['- ' + note for note in LIMITATIONS]
    lines += ['', '逐日权益、逐次成交、资金费与参考核对误差见 results.json。', '']
    return '\n'.join(lines)


def write_ledger(result, path):
    def build(k, previous):
        if result['verdict'] != 'invalid' and k != 1:
            raise ValueError(f'实际账本 K={k} 与预注册 K=1 不一致，拒绝追加')
        stats = result['statistics'] or {}
        fields = ('annual_return', 'z', 'first_half_annual', 'second_half_annual', 'recent_annual', 'max_dd')
        return dict(track='crypto_carry', stage='S1', change_type='E', idea_id='crypto-funding-carry-v1',
                    baseline_version='independent-carry@' + PREREG_SHA256, verdict=result['verdict'],
                    preregistered_K=1, z_K=1.96, prereg_sha256=PREREG_SHA256,
                    input_hashes=result['inputs'], trials=result['trials'], **{k: stats.get(k) for k in fields})
    return evolution_ledger.append_event(path, 'crypto_carry', build)


def validate_output(out, data_dir, panel_daily, ledger):
    protected = [Path(data_dir).resolve(), Path(panel_daily).resolve()]
    destinations = [(Path(out) / name).resolve() for name in ('results.json', 'report.md')]
    if ledger is not None:
        destinations.append(Path(ledger).resolve())
    if (len(set(destinations)) != len(destinations)
            or any(p == PREREG.resolve() or any(p.is_relative_to(root) for root in protected) for p in destinations)):
        raise ValueError('输出与账本不可覆盖输入或彼此覆盖')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    fetch = sub.add_parser('fetch')
    fetch.add_argument('--data-dir', type=Path, default=REPO / 'data/carry')
    fetch.add_argument('--sleep', type=float, default=.3)
    run = sub.add_parser('run')
    run.add_argument('--data-dir', type=Path, default=REPO / 'data/carry')
    run.add_argument('--panel-daily', type=Path, default=REPO / 'data/factor_mining/crypto/daily')
    run.add_argument('--out', type=Path, required=True)
    run.add_argument('--write-ledger', type=Path, nargs='?', const=LEDGER)
    args = parser.parse_args(argv)
    try:
        if args.command == 'fetch':
            fetch_data(args.data_dir, args.sleep)
        else:
            validate_output(args.out, args.data_dir, args.panel_daily, args.write_ledger)
            result = study(args.data_dir, args.panel_daily)
            write_json(args.out / 'results.json', result)
            (args.out / 'report.md').write_text(report(result), encoding='utf-8')
            if args.write_ledger is not None:
                result['ledger'] = write_ledger(result, args.write_ledger)
                write_json(args.out / 'results.json', result)
            print(f'VERDICT {result["verdict"]}')
            return 1 if result['verdict'] == 'invalid' else 0
    except (OSError, ValueError) as exc:
        parser.error(str(exc))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
