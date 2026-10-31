"""Persistent incremental cache + read-only validation for backtest market data.

Layout under --data-root (default /Users/zhangyun/workspace/nofx/data):

    quantjourney/prices/<SYM>.json
        rows [date 'YYYY-MM-DD', open, high, low, close, adjclose, volume]
        Yahoo chart API; close is split-adjusted, adjclose also dividend-adjusted.
    volrix/klines/<SYM>_<interval>.json
        rows [open_ms, open, high, low, close, volume, quote_volume], closed bars only.
    factor_mining/crypto/daily/<SYM>.json
        rows [date, open, high, low, close, volume, quote_volume]  (validated read-only here,
        maintained by the factor-mining overnight run -- never written by this tool)
    factor_mining/crypto/alt_daily/funding/<SYM>.json
        rows [date, rate]  (read-only here)

Downloaded data is never deleted.  ``us`` and ``klines`` update incrementally and
every write is atomic (temp file in the same directory + os.replace).  The only
whole-file replacement happens when Yahoo's adjustment basis changed (a new
dividend/split recalculates the whole adjclose series); the previous file is kept
as <SYM>.json.bak-<YYYYMMDD>.

Usage:
    python -m scripts.backtest.market_cache us SPY QQQ
    python -m scripts.backtest.market_cache klines BTCUSDT 1h --start 2019-12-26
    python -m scripts.backtest.market_cache check --us SPY --klines BTCUSDT_1h [--json out.json]
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import random
import shutil
import sys
import tempfile
import time
import urllib.request
from collections import Counter
from pathlib import Path
from zoneinfo import ZoneInfo

DEFAULT_DATA_ROOT = '/Users/zhangyun/workspace/nofx/data'
DEFAULT_PROXY = 'http://127.0.0.1:7890'

FULL_START = dt.date(2000, 1, 1)
FULL_START_S = 946684800  # 2000-01-01 00:00 UTC, same as the legacy fetch script
US_INCREMENTAL_LOOKBACK_DAYS = 10
NEW_YORK = ZoneInfo('America/New_York')
US_SETTLE_TIME = dt.time(16, 30)  # a session's bar is final half an hour after the close
ADJUSTMENT_TOL = 1e-6

KLINE_MS = {'1m': 60_000, '5m': 300_000, '15m': 900_000, '30m': 1_800_000,
            '1h': 3_600_000, '4h': 14_400_000, '1d': 86_400_000}
BINANCE_KLINES_URL = 'https://fapi.binance.com/fapi/v1/klines'
YAHOO_CHART_URL = 'https://query1.finance.yahoo.com/v8/finance/chart/{sym}'
NASDAQ_QUOTE_URL = ('https://api.nasdaq.com/api/quote/{sym}/historical'
                    '?assetclass={cls}&fromdate=&todate=&limit=9999')
NASDAQ_UA = ('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 '
             '(KHTML, like Gecko) Chrome/124.0 Safari/537.36')
NASDAQ_STOCKS_CLASS = frozenset({'KO', 'PEP'})  # everything else is compared as etf

US_DIR = 'quantjourney/prices'
KLINES_DIR = 'volrix/klines'
CRYPTO_DAILY_DIR = 'factor_mining/crypto/daily'
FUNDING_DIR = 'factor_mining/crypto/alt_daily/funding'
CALENDAR_SYMBOL = 'SPY'

DETAIL_SAMPLE_CAP = 5      # dates shown per violation when printing
JSON_SAMPLE_CAP = 100      # list entries kept per violation in the JSON report


# --------------------------------------------------------------------------- #
# small helpers
# --------------------------------------------------------------------------- #

def date_to_epoch(day):
    """UTC midnight epoch seconds of a datetime.date."""
    return int(dt.datetime(day.year, day.month, day.day, tzinfo=dt.timezone.utc).timestamp())


def date_to_ms(day):
    return date_to_epoch(day) * 1000


def rel_diff(a, b):
    """Relative difference |a-b| / |b|, guarded against a zero denominator."""
    return abs(a - b) / max(abs(b), 1e-12)


def _fmt_ts(ms):
    return dt.datetime.fromtimestamp(int(ms) / 1000, dt.timezone.utc).strftime('%Y-%m-%d %H:%M')


def _fmt_samples(items, cap=DETAIL_SAMPLE_CAP, fmt=str):
    items = list(items)
    if not items:
        return ''
    shown = [fmt(x) for x in items[:cap]]
    more = len(items) - len(shown)
    return ', '.join(shown) + (f' (+{more} more)' if more > 0 else '')


# --------------------------------------------------------------------------- #
# HTTP layer (retry with exponential backoff + 0.3-0.5s pacing between requests)
# --------------------------------------------------------------------------- #

_last_request_at = [0.0]


def _pace(min_gap=0.3, max_gap=0.5):
    gap = random.uniform(min_gap, max_gap)
    wait = _last_request_at[0] + gap - time.monotonic()
    if wait > 0:
        time.sleep(wait)
    _last_request_at[0] = time.monotonic()


def http_json(url, proxy=DEFAULT_PROXY, headers=None, timeout=30, retries=3, log=None):
    """GET url and parse the JSON body. Empty --proxy disables any proxy."""
    handler = urllib.request.ProxyHandler({} if not proxy else {'http': proxy, 'https': proxy})
    opener = urllib.request.build_opener(handler)
    base_headers = {'User-Agent': 'Mozilla/5.0'}
    if headers:
        base_headers.update(headers)
    last_exc = None
    for attempt in range(retries):
        _pace()
        try:
            request = urllib.request.Request(url, headers=base_headers)
            with opener.open(request, timeout=timeout) as resp:
                return json.load(resp)
        except Exception as exc:  # transport or parse error -> retry
            last_exc = exc
            if log:
                log(f'http retry {attempt + 1}/{retries}: {url}: {exc}')
            time.sleep(2 ** attempt)
    raise RuntimeError(f'GET failed after {retries} attempts: {url}: {last_exc}')


# --------------------------------------------------------------------------- #
# fetchers (module level so tests can replace them with fakes)
# --------------------------------------------------------------------------- #

def fetch_yahoo_rows(symbol, period1_s, period2_s, proxy=DEFAULT_PROXY):
    """Yahoo daily chart -> [date, open, high, low, close, adjclose, volume] rows."""
    url = (f'https://query1.finance.yahoo.com/v8/finance/chart/{symbol}'
           f'?period1={int(period1_s)}&period2={int(period2_s)}'
           '&interval=1d&includeAdjustedClose=true&events=div,splits')
    payload = http_json(url, proxy)
    chart = payload.get('chart') or {}
    results = chart.get('result') or []
    if not results:
        err = (chart.get('error') or {}).get('description') or 'no result'
        raise RuntimeError(f'yahoo returned no data for {symbol}: {err}')
    result = results[0]
    quote = result['indicators']['quote'][0]
    adjclose = result['indicators'].get('adjclose')
    adj = adjclose[0]['adjclose'] if adjclose else quote['close']
    gmtoffset = result['meta'].get('gmtoffset', 0)
    rows = []
    for i, ts in enumerate(result.get('timestamp') or []):
        vals = [quote['open'][i], quote['high'][i], quote['low'][i],
                quote['close'][i], adj[i], quote['volume'][i]]
        if any(v is None for v in vals):
            continue
        day = dt.datetime.fromtimestamp(ts + gmtoffset, dt.timezone.utc).strftime('%Y-%m-%d')
        rows.append([day, *vals])
    return rows


def fetch_binance_klines(symbol, interval, start_ms, end_ms, proxy=DEFAULT_PROXY, page_limit=1500):
    """Binance USDT-M futures klines, paged from start_ms until end_ms (exclusive-ish)."""
    if interval not in KLINE_MS:
        raise ValueError(f'unsupported interval {interval!r}')
    step = KLINE_MS[interval]
    rows = []
    cursor = int(start_ms)
    end_ms = int(end_ms)
    while cursor < end_ms:
        url = (f'https://fapi.binance.com/fapi/v1/klines?symbol={symbol}&interval={interval}'
               f'&startTime={cursor}&limit={page_limit}')
        page = http_json(url, proxy)
        if not page:
            break
        rows.extend([int(r[0]), float(r[1]), float(r[2]), float(r[3]), float(r[4]), float(r[5]), float(r[7])]
                    for r in page)
        nxt = int(page[-1][0]) + step
        if nxt <= cursor:  # server not advancing -> avoid an endless loop
            break
        cursor = nxt
    return rows


def _nasdaq_number(text):
    """Parse nasdaq.com numbers like '$1,234.56'; N/A/empty -> None."""
    if text is None:
        return None
    s = str(text).replace('$', '').replace(',', '').strip()
    if not s or s == 'N/A':
        return None
    return float(s)


def parse_nasdaq_trades(payload):
    """nasdaq.com historical payload -> [iso_date, open, high, low, close] rows.

    Placeholder rows with volume 'N/A' (open == high == low == close) are skipped."""
    table = (payload.get('data') or {}).get('tradesTable') or {}
    out = []
    for row in table.get('rows') or []:
        if str(row.get('volume', '')).strip() == 'N/A':
            continue
        try:
            day = dt.datetime.strptime(str(row.get('date')), '%m/%d/%Y').date().isoformat()
            vals = []
            for key in ('open', 'high', 'low', 'close'):
                v = _nasdaq_number(row.get(key))
                if v is None:
                    raise ValueError(f'bad {key}')
                vals.append(v)
        except (ValueError, TypeError):
            continue
        out.append([day, *vals])
    return out


def fetch_nasdaq_daily(symbol, assetclass='stocks', proxy=DEFAULT_PROXY):
    url = NASDAQ_QUOTE_URL.format(sym=symbol, cls=assetclass)
    payload = http_json(url, proxy, headers={'Accept': 'application/json', 'User-Agent': NASDAQ_UA})
    return parse_nasdaq_trades(payload)


# --------------------------------------------------------------------------- #
# storage: load + atomic write (never delete an existing data file)
# --------------------------------------------------------------------------- #

def load_rows(path):
    with open(path, 'r', encoding='utf-8') as fh:
        data = json.load(fh)
    if not isinstance(data, list):
        raise ValueError(f'expected a JSON list of rows in {path}')
    return data


def atomic_write_rows(path, rows):
    """Write rows as compact JSON atomically.

    The data is written to a temp file in the same directory, then os.replace'd
    over the target.  Any failure removes the temp file and leaves the existing
    data file untouched -- existing data is never deleted."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=path.name + '.', suffix='.tmp')
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as fh:
            json.dump(rows, fh, allow_nan=False, separators=(',', ':'))
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


# --------------------------------------------------------------------------- #
# US daily prices: incremental update with adjustment-basis guard
# --------------------------------------------------------------------------- #

def clean_us_rows(rows):
    """Drop rows containing None / malformed rows, dedupe by date, sort ascending."""
    by_date = {}
    for row in rows:
        if not isinstance(row, list) or len(row) < 7:
            continue
        if any(v is None for v in row[:7]):
            continue
        by_date[row[0]] = [row[0], row[1], row[2], row[3], row[4], row[5], row[6]]
    return [by_date[d] for d in sorted(by_date)]


def drop_unsettled_us(rows, now_s):
    """Drop bars for sessions that have not closed yet (Yahoo returns a live bar intraday).

    Without this a bar fetched mid-session is cached, its close changes by the next
    run and the adjustment-basis guard would force a full redownload every time."""
    now_ny = dt.datetime.fromtimestamp(now_s, NEW_YORK)
    last_settled = now_ny.date() if now_ny.time() >= US_SETTLE_TIME else now_ny.date() - dt.timedelta(days=1)
    return [r for r in rows if str(r[0]) <= last_settled.isoformat()]


def merge_us(old_rows, new_rows, tol=ADJUSTMENT_TOL):
    """Merge fetched rows into the cached rows.

    New rows overwrite overlapping dates and append new dates; older dates are
    preserved.  Returns (merged_rows, basis_break) where basis_break is None or
    (date, reason) when an overlapping date shows a changed close or a changed
    adjclose/close ratio relative to the cached file -- i.e. Yahoo recomputed the
    adjustment series and a full redownload is required."""
    old_by = {r[0]: r for r in old_rows if isinstance(r, list) and r}
    # The newest cached bar may predate drop_unsettled_us (legacy fetch), so it is
    # overwritten without counting as an adjustment change.
    newest_old = max(old_by) if old_by else None
    new_by = {}
    for row in new_rows:
        if not isinstance(row, list) or len(row) < 7 or any(v is None for v in row[:7]):
            continue
        new_by[row[0]] = row
    basis_break = None
    for day in sorted(new_by):
        old_row = old_by.get(day)
        if old_row is None or day == newest_old:
            continue
        new_row = new_by[day]
        if rel_diff(old_row[4], new_row[4]) > tol:
            basis_break = (day, f'close rel diff {rel_diff(old_row[4], new_row[4]):.3g}')
            break
        if old_row[4] and new_row[4]:
            ratio_old, ratio_new = old_row[5] / old_row[4], new_row[5] / new_row[4]
            if rel_diff(ratio_old, ratio_new) > tol:
                basis_break = (day, f'adjclose/close ratio rel diff {rel_diff(ratio_old, ratio_new):.3g}')
                break
    merged = dict(old_by)
    merged.update(new_by)
    return [merged[d] for d in sorted(merged)], basis_break


def _next_backup_path(path, stamp):
    """<SYM>.json.bak-<YYYYMMDD>, plus -2, -3... if a backup already exists (never overwrite)."""
    candidate = path.parent / f'{path.stem}.json.bak-{stamp}'
    n = 1
    while candidate.exists():
        n += 1
        candidate = path.parent / f'{path.stem}.json.bak-{stamp}-{n}'
    return candidate


def update_us(symbol, data_root, proxy=DEFAULT_PROXY, fetch=None, now_s=None, log=print):
    """Incrementally update data_root/quantjourney/prices/<SYM>.json.

    Missing file -> full download from 2000-01-01.
    Existing file -> fetch only the last cached date minus a small overlap, merge
    by date.  If any overlapping date shows a changed close or adjclose/close
    ratio (new dividend/split -> Yahoo recomputed history), redownload the full
    history and replace the file, keeping the old one as .bak-<YYYYMMDD>."""
    fetch = fetch or fetch_yahoo_rows
    path = Path(data_root) / US_DIR / f'{symbol}.json'
    end_s = int(time.time()) if now_s is None else int(now_s)
    old_rows = load_rows(path) if path.exists() else []
    if not old_rows:
        rows = drop_unsettled_us(clean_us_rows(fetch(symbol, FULL_START_S, end_s, proxy)), end_s)
        if not rows:
            raise RuntimeError(f'{symbol}: full download returned no rows; nothing written')
        atomic_write_rows(path, rows)
        span = f' ({rows[0][0]}..{rows[-1][0]})' if rows else ''
        log(f'{symbol}: full download -> {len(rows)} rows{span}')
        return {'symbol': symbol, 'mode': 'full', 'rows': len(rows)}

    last_day = max(str(r[0]) for r in old_rows if isinstance(r, list) and r)
    start_s = date_to_epoch(dt.date.fromisoformat(last_day)) - US_INCREMENTAL_LOOKBACK_DAYS * 86400
    window = drop_unsettled_us(fetch(symbol, start_s, end_s, proxy), end_s)
    merged, basis_break = merge_us(old_rows, window)
    if basis_break is None:
        old_days = {r[0] for r in old_rows if isinstance(r, list) and r}
        new_days = sum(1 for r in merged if r[0] not in old_days)
        atomic_write_rows(path, merged)
        log(f'{symbol}: incremental merge -> +{new_days} new days, {len(merged)} rows total '
            f'(last {merged[-1][0]})')
        return {'symbol': symbol, 'mode': 'merge', 'rows': len(merged), 'new_days': new_days}

    day, why = basis_break
    log(f'{symbol}: adjustment basis changed at {day} ({why}) -> full redownload')
    stamp = dt.date.today().strftime('%Y%m%d')
    backup = _next_backup_path(path, stamp)
    rows = drop_unsettled_us(clean_us_rows(fetch(symbol, FULL_START_S, end_s, proxy)), end_s)
    if len(rows) < 0.9*len(old_rows) or rows[-1][0] < last_day:
        # A truncated/failed redownload must never replace a good cache.
        raise RuntimeError(f'{symbol}: redownload returned {len(rows)} rows (cached {len(old_rows)}, '
                           f'last {rows[-1][0] if rows else None}); cache left unchanged')
    shutil.copy2(path, backup)
    atomic_write_rows(path, rows)
    log(f'{symbol}: replaced with {len(rows)} rows ({rows[0][0]}..{rows[-1][0]}), '
        f'old file kept as {backup.name}')
    return {'symbol': symbol, 'mode': 'full-replace', 'rows': len(rows), 'backup': backup.name}


# --------------------------------------------------------------------------- #
# Binance klines: incremental update
# --------------------------------------------------------------------------- #

def clean_kline_rows(rows, step_ms, now_ms=None):
    """Dedupe by open_ms, sort ascending, drop unfinished bars (open+step > now)."""
    by_open = {}
    for row in rows:
        if not isinstance(row, list) or len(row) < 7 or any(v is None for v in row[:7]):
            continue
        open_ms = int(row[0])
        if now_ms is not None and open_ms + step_ms > now_ms:
            continue  # bar has not closed yet
        by_open[open_ms] = [open_ms, float(row[1]), float(row[2]), float(row[3]),
                            float(row[4]), float(row[5]), float(row[6])]
    return [by_open[t] for t in sorted(by_open)]


def merge_klines(old_rows, new_rows, step_ms, now_ms=None):
    """Append new bars; on overlap the freshly fetched bar wins."""
    by_open = {int(r[0]): r for r in old_rows if isinstance(r, list) and r}
    for row in clean_kline_rows(new_rows, step_ms, now_ms):
        by_open[row[0]] = row
    return [by_open[t] for t in sorted(by_open)]


def update_klines(symbol, interval, data_root, proxy=DEFAULT_PROXY, start=None, fetch=None,
                  now_ms=None, log=print):
    """Incrementally update data_root/volrix/klines/<SYM>_<interval>.json.

    Existing file -> request from the bar after the last cached one and append.
    Missing file -> download from --start (required).  Unfinished bars are
    dropped; bars are deduped and sorted."""
    if interval not in KLINE_MS:
        raise ValueError(f'unsupported interval {interval!r}; choose from {sorted(KLINE_MS)}')
    fetch = fetch or fetch_binance_klines
    step = KLINE_MS[interval]
    now_ms = int(time.time() * 1000) if now_ms is None else int(now_ms)
    path = Path(data_root) / KLINES_DIR / f'{symbol}_{interval}.json'
    old_rows = load_rows(path) if path.exists() else []
    if old_rows:
        last_ms = max(int(r[0]) for r in old_rows if isinstance(r, list) and r)
        start_ms = last_ms + step
        if start_ms > now_ms:
            log(f'{symbol} {interval}: up to date (last bar closes {_fmt_ts(last_ms + step)})')
            return {'symbol': symbol, 'interval': interval, 'mode': 'up-to-date', 'rows': len(old_rows)}
        fresh = fetch(symbol, interval, start_ms, now_ms, proxy)
        merged = merge_klines(old_rows, fresh, step, now_ms)
        atomic_write_rows(path, merged)
        log(f'{symbol} {interval}: +{len(merged) - len(old_rows)} bars -> {len(merged)} rows '
            f'(through {_fmt_ts(merged[-1][0])})')
        return {'symbol': symbol, 'interval': interval, 'mode': 'merge', 'rows': len(merged)}

    if not start:
        raise SystemExit(f'{symbol} {interval}: --start YYYY-MM-DD is required when '
                         f'no cache file exists yet')
    start_ms = date_to_ms(dt.date.fromisoformat(start))
    rows = clean_kline_rows(fetch(symbol, interval, start_ms, now_ms, proxy), step, now_ms)
    atomic_write_rows(path, rows)
    span = f' ({_fmt_ts(rows[0][0])}..{_fmt_ts(rows[-1][0])})' if rows else ''
    log(f'{symbol} {interval}: full download from {start} -> {len(rows)} rows{span}')
    return {'symbol': symbol, 'interval': interval, 'mode': 'full', 'rows': len(rows)}


# --------------------------------------------------------------------------- #
# checks
# --------------------------------------------------------------------------- #

def _ohlc_ok(row):
    """low <= min(open, close) and high >= max(open, close) for row[1:5]."""
    o, h, low, c = row[1], row[2], row[3], row[4]
    return low <= min(o, c) and h >= max(o, c)


def _rows_by_date(rows, width=7):
    by = {}
    for row in rows:
        if isinstance(row, list) and len(row) >= width:
            by.setdefault(row[0], row)
    return by


def _status(*counts, warn=False):
    return 'WARN' if warn or any(c for c in counts) else 'OK'


def check_us(rows, calendar=None):
    """Validate US daily rows [date, o, h, l, c, adjclose, volume]."""
    dates = [str(r[0]) for r in rows if isinstance(r, list) and r]
    by = _rows_by_date(rows)
    days = sorted(by)
    dup_dates = sorted(d for d, n in Counter(dates).items() if n > 1)
    disorder = [[dates[i - 1], dates[i]] for i in range(1, len(dates)) if dates[i] < dates[i - 1]]
    ohlc_bad = [d for d in days if len(by[d]) >= 5 and not _ohlc_ok(by[d])]
    nonpos = [d for d in days if any(by[d][i] is not None and by[d][i] <= 0 for i in range(1, 5))]
    neg_vol = [d for d in days if len(by[d]) >= 7 and by[d][6] is not None and by[d][6] < 0]
    ratio_jumps, adj_ret_jumps = [], []
    prev_adj, prev_ratio = None, None
    for d in days:
        row = by[d]
        if len(row) < 6 or row[4] in (None, 0) or row[5] is None:
            prev_adj, prev_ratio = None, None
            continue
        ratio = row[5] / row[4]
        if prev_ratio and rel_diff(ratio, prev_ratio) > 0.20:
            ratio_jumps.append(d)  # raw close not adjusted for a split
        if prev_adj and rel_diff(row[5], prev_adj) > 0.25:
            adj_ret_jumps.append(d)
        prev_adj, prev_ratio = row[5], ratio
    missing, extra = [], []
    if calendar and days:
        cal_set = set(calendar)
        day_set = set(days)
        missing = [d for d in calendar if days[0] <= d <= days[-1] and d not in day_set]
        extra = sorted(day_set - cal_set)
    return {'rows': len(rows), 'first': days[0] if days else None, 'last': days[-1] if days else None,
            'dup_dates': dup_dates, 'disorder': disorder, 'ohlc_violations': ohlc_bad,
            'nonpositive_price': nonpos, 'negative_volume': neg_vol, 'ratio_jumps': ratio_jumps,
            'adj_return_jumps': adj_ret_jumps, 'calendar_missing': missing, 'calendar_extra': extra,
            'status': _status(len(dup_dates), len(disorder), len(ohlc_bad), len(nonpos),
                              len(neg_vol), len(ratio_jumps), len(adj_ret_jumps),
                              len(missing), len(extra))}


def check_crypto_daily(rows):
    """Validate crypto daily rows [date, o, h, l, c, vol, ...] (read-only)."""
    dates = [str(r[0]) for r in rows if isinstance(r, list) and r]
    by = _rows_by_date(rows, width=5)
    days = sorted(by)
    dup_dates = sorted(d for d, n in Counter(dates).items() if n > 1)
    gaps, gap_days = [], 0
    for a, b in zip(days, days[1:]):
        delta = (dt.date.fromisoformat(b) - dt.date.fromisoformat(a)).days
        if delta > 1:
            gaps.append([a, b, delta - 1])
            gap_days += delta - 1
    ohlc_bad = [d for d in days if not _ohlc_ok(by[d])]
    open_gaps = []
    for a, b in zip(days, days[1:]):
        prev_close, cur_open = by[a][4], by[b][1]
        if prev_close and cur_open is not None and rel_diff(cur_open, prev_close) > 0.02:
            open_gaps.append(b)
    return {'rows': len(rows), 'first': days[0] if days else None, 'last': days[-1] if days else None,
            'dup_dates': dup_dates, 'gap_segments': gaps, 'gap_days': gap_days,
            'ohlc_violations': ohlc_bad, 'open_gaps': open_gaps,
            'status': _status(len(dup_dates), gap_days, len(ohlc_bad), len(open_gaps))}


def check_funding_coverage(price_rows, funding_rows):
    """Funding coverage vs price dates: missing days and max |rate| (>1% warns)."""
    price_days = sorted({str(r[0]) for r in price_rows if isinstance(r, list) and r})
    funding_dates = {str(r[0]) for r in funding_rows if isinstance(r, list) and r}
    missing = [d for d in price_days if d not in funding_dates]
    rates = [abs(float(r[1])) for r in funding_rows
             if isinstance(r, list) and len(r) >= 2 and r[1] is not None]
    max_rate = max(rates) if rates else None
    return {'price_days': len(price_days), 'funding_rows': len(funding_rows),
            'missing_days': len(missing), 'missing_dates': missing,
            'max_abs_rate': max_rate, 'warn': bool(max_rate is not None and max_rate > 0.01)}


def check_klines(rows, step_ms):
    """Validate kline rows [open_ms, o, h, l, c, volume, quote_volume]."""
    opens = [int(r[0]) for r in rows if isinstance(r, list) and r]
    uniq = sorted(set(opens))
    dup_bars = sorted(t for t, n in Counter(opens).items() if n > 1)
    expected = (uniq[-1] - uniq[0]) // step_ms + 1 if uniq else 0
    missing_bars = expected - len(uniq)
    gap_segments = [[a, b, (b - a) // step_ms - 1]
                    for a, b in zip(uniq, uniq[1:]) if b - a != step_ms]
    by = {int(r[0]): r for r in rows if isinstance(r, list) and len(r) >= 5}
    ohlc_bad = [t for t in uniq if not _ohlc_ok(by[t])]
    zero_vol = [t for t in uniq if by[t][5] is not None and float(by[t][5]) == 0.0]
    return {'rows': len(rows), 'first_ms': uniq[0] if uniq else None,
            'last_ms': uniq[-1] if uniq else None, 'dup_bars': dup_bars,
            'missing_bars': missing_bars, 'gap_segments': gap_segments,
            'ohlc_violations': ohlc_bad, 'zero_volume_bars': zero_vol,
            'status': _status(len(dup_bars), missing_bars, len(ohlc_bad))}


def aggregate_klines(rows, bucket_ms):
    """Aggregate fine rows into bucket_ms bars: first open, max high, min low, last close."""
    agg = {}
    for row in sorted((r for r in rows if isinstance(r, list) and len(r) >= 5), key=lambda r: int(r[0])):
        t = int(row[0])
        bucket = t - t % bucket_ms
        cur = agg.get(bucket)
        if cur is None:
            agg[bucket] = [float(row[1]), float(row[2]), float(row[3]), float(row[4])]
        else:
            cur[1] = max(cur[1], float(row[2]))
            cur[2] = min(cur[2], float(row[3]))
            cur[3] = float(row[4])
    return agg


def check_aggregate_consistency(fine_rows, coarse_rows, bucket_ms, tol=1e-6):
    """Compare coarse bars (e.g. 1h) against fine rows aggregated into the same buckets."""
    agg = aggregate_klines(fine_rows, bucket_ms)
    common, mismatched = 0, []
    for row in coarse_rows:
        if not isinstance(row, list) or len(row) < 5:
            continue
        t = int(row[0])
        if t % bucket_ms:
            continue
        bucket = agg.get(t)
        if bucket is None:
            continue
        common += 1
        for k, field in enumerate(('open', 'high', 'low', 'close')):
            if rel_diff(float(row[1 + k]), bucket[k]) > tol:
                mismatched.append({'time_ms': t, 'field': field,
                                   'coarse': float(row[1 + k]), 'aggregated': bucket[k]})
                break
    return {'common_bars': common, 'mismatched_bars': len(mismatched), 'samples': mismatched[:10],
            'status': _status(len(mismatched))}


# --------------------------------------------------------------------------- #
# cross-checks (optional, need network)
# --------------------------------------------------------------------------- #

def cross_check_us_nasdaq(symbol, rows, proxy=DEFAULT_PROXY, fetch=None, years=10):
    """Compare our raw close against nasdaq.com historical closes (last `years` years)."""
    fetch = fetch or fetch_nasdaq_daily
    assetclass = 'stocks' if symbol in NASDAQ_STOCKS_CLASS else 'etf'
    theirs_rows = fetch(symbol, assetclass, proxy)
    theirs = {r[0]: r[4] for r in theirs_rows}
    ours = {r[0]: r[4] for r in rows if isinstance(r, list) and len(r) >= 5}
    cutoff = (dt.date.today() - dt.timedelta(days=int(365.25 * years))).isoformat()
    days = sorted(d for d in ours if d in theirs and d >= cutoff)
    mismatches = [[d, ours[d], theirs[d], rel_diff(ours[d], theirs[d])]
                  for d in days if rel_diff(ours[d], theirs[d]) > 0.005]
    return {'source': 'nasdaq', 'assetclass': assetclass, 'compared_days': len(days),
            'mismatch_days': len(mismatches), 'samples': mismatches[:10],
            'status': _status(len(mismatches))}


def cross_check_crypto_daily(symbol, rows, proxy=DEFAULT_PROXY, fetch=None, now_ms=None):
    """Compare crypto daily OHLC against Binance fapi 1d klines (full history)."""
    fetch = fetch or fetch_binance_klines
    now_ms = int(time.time() * 1000) if now_ms is None else int(now_ms)
    theirs = {}
    for row in fetch(symbol, '1d', 0, now_ms, proxy):
        day = dt.datetime.fromtimestamp(int(row[0]) / 1000, dt.timezone.utc).date().isoformat()
        theirs.setdefault(day, (row[1], row[2], row[3], row[4]))
    ours = {}
    for row in rows:
        if isinstance(row, list) and len(row) >= 5:
            ours.setdefault(str(row[0]), tuple(float(x) for x in row[1:5]))
    days = sorted(d for d in ours if d in theirs)
    mismatches = []
    for d in days:
        worst = max(rel_diff(a, b) for a, b in zip(ours[d], theirs[d]))
        if worst > 1e-6:
            mismatches.append([d, list(ours[d]), list(theirs[d])])
    return {'source': 'binance-1d', 'compared_days': len(days), 'mismatch_days': len(mismatches),
            'samples': mismatches[:10], 'status': _status(len(mismatches))}


# --------------------------------------------------------------------------- #
# check-all orchestration
# --------------------------------------------------------------------------- #

def parse_kline_spec(spec):
    sym, _, interval = str(spec).rpartition('_')
    if not sym or interval not in KLINE_MS:
        raise ValueError(f"kline spec must look like SYM_INTERVAL (e.g. BTCUSDT_1h), got {spec!r}")
    return sym, interval


def _default_us_symbols(us_dir):
    return sorted(p.stem for p in us_dir.glob('*.json') if '.bak-' not in p.name)


def _default_crypto_symbols(crypto_dir):
    return sorted(p.stem for p in crypto_dir.glob('*.json'))


def _default_kline_specs(klines_dir):
    specs = []
    for p in sorted(klines_dir.glob('*.json')):
        try:
            sym, interval = parse_kline_spec(p.stem)
        except ValueError:
            continue
        specs.append(f'{sym}_{interval}')
    return specs


def check_all(data_root, us=None, crypto=None, klines=None, cross_check=False,
              proxy=DEFAULT_PROXY, fetch_nasdaq=None, fetch_binance=None):
    """Read-only validation.  Category args: None = skip, [] = all files, [names] = subset."""
    root = Path(data_root)
    report = {'data_root': str(root)}

    if us is not None:
        us_dir = root / US_DIR
        symbols = us or _default_us_symbols(us_dir)
        calendar = []
        spy_path = us_dir / f'{CALENDAR_SYMBOL}.json'
        if spy_path.exists():
            calendar = sorted({str(r[0]) for r in load_rows(spy_path) if isinstance(r, list) and r})
        symbols_rep = {}
        for sym in symbols:
            path = us_dir / f'{sym}.json'
            if not path.exists():
                symbols_rep[sym] = {'error': 'missing file', 'status': 'MISSING'}
                continue
            rows = load_rows(path)
            rep = check_us(rows, calendar)
            symbols_rep[sym] = rep
            if cross_check:
                try:
                    rep['cross_check'] = cross_check_us_nasdaq(sym, rows, proxy, fetch_nasdaq)
                except Exception as exc:
                    rep['cross_check'] = {'error': str(exc)}
        report['us'] = {'calendar_symbol': CALENDAR_SYMBOL, 'calendar_days': len(calendar),
                        'symbols': symbols_rep}

    if crypto is not None:
        daily_dir = root / CRYPTO_DAILY_DIR
        funding_dir = root / FUNDING_DIR
        symbols_rep = {}
        for sym in (crypto or _default_crypto_symbols(daily_dir)):
            path = daily_dir / f'{sym}.json'
            if not path.exists():
                symbols_rep[sym] = {'error': 'missing file', 'status': 'MISSING'}
                continue
            rows = load_rows(path)
            rep = check_crypto_daily(rows)
            funding_path = funding_dir / f'{sym}.json'
            if funding_path.exists():
                funding = check_funding_coverage(rows, load_rows(funding_path))
                rep['status'] = _status(1 if rep['status'] == 'WARN' else 0, warn=funding['warn'])
            else:
                funding = {'error': 'no funding file', 'missing_days': rep.get('rows')}
            rep['funding'] = funding
            if cross_check and funding.get('error') is None:
                try:
                    rep['cross_check'] = cross_check_crypto_daily(sym, rows, proxy, fetch_binance)
                except Exception as exc:
                    rep['cross_check'] = {'error': str(exc)}
            symbols_rep[sym] = rep
        report['crypto'] = {'symbols': symbols_rep}

    if klines is not None:
        klines_dir = root / KLINES_DIR
        specs = klines or _default_kline_specs(klines_dir)
        files_rep, loaded = {}, {}
        for spec in specs:
            sym, interval = parse_kline_spec(spec)
            path = klines_dir / f'{spec}.json'
            if not path.exists():
                files_rep[spec] = {'error': 'missing file', 'status': 'MISSING'}
                continue
            rows = load_rows(path)
            loaded[spec] = rows
            files_rep[spec] = check_klines(rows, KLINE_MS[interval])
        pairs = {}
        by_symbol = {}
        for spec in specs:
            sym, interval = parse_kline_spec(spec)
            by_symbol.setdefault(sym, {})[interval] = spec
        for sym, intervals in sorted(by_symbol.items()):
            if '1h' in intervals and '5m' in intervals \
                    and intervals['1h'] in loaded and intervals['5m'] in loaded:
                pairs[sym] = check_aggregate_consistency(loaded[intervals['5m']],
                                                         loaded[intervals['1h']], KLINE_MS['1h'])
        report['klines'] = {'files': files_rep, 'pairs': pairs}

    if cross_check:
        cross = {}
        for cat in ('us', 'crypto'):
            for sym, rep in ((report.get(cat) or {}).get('symbols') or {}).items():
                cc = rep.get('cross_check')
                if cc:
                    cross[sym] = cc
        report['cross_check'] = cross
    return report


# --------------------------------------------------------------------------- #
# report printing
# --------------------------------------------------------------------------- #

def _table(headers, body):
    widths = [max([len(h)] + [len(str(r[i])) for r in body if i < len(r)])
              for i, h in enumerate(headers)]
    sep = '  '.join('-' * w for w in widths)
    lines = ['  '.join(h.ljust(widths[i]) for i, h in enumerate(headers)), sep]
    for r in body:
        lines.append('  '.join(str(r[i]).ljust(widths[i]) for i in range(len(headers))))
    return '\n'.join(lines)


def print_report(report, out=None):
    out = out or sys.stdout
    p = lambda line='': print(line, file=out)

    if report.get('us'):
        us = report['us']
        p(f"== US daily ({US_DIR}) | calendar {us['calendar_symbol']}: "
          f"{us['calendar_days']} trading days ==")
        headers = ['symbol', 'rows', 'first', 'last', 'dup', 'disorder', 'ohlc', 'px<=0', 'vol<0',
                   'ratio>20%', 'adjret>25%', 'cal_miss', 'cal_extra', 'status']
        body = []
        for sym, rep in us['symbols'].items():
            if 'error' in rep:
                body.append([sym, *['-'] * 12, rep['status']])
                continue
            body.append([sym, rep['rows'], rep['first'], rep['last'],
                         len(rep['dup_dates']), len(rep['disorder']), len(rep['ohlc_violations']),
                         len(rep['nonpositive_price']), len(rep['negative_volume']),
                         len(rep['ratio_jumps']), len(rep['adj_return_jumps']),
                         len(rep['calendar_missing']), len(rep['calendar_extra']), rep['status']])
        p(_table(headers, body))
        for sym, rep in us['symbols'].items():
            for key in ('dup_dates', 'ohlc_violations', 'nonpositive_price', 'negative_volume',
                        'ratio_jumps', 'adj_return_jumps', 'calendar_missing', 'calendar_extra'):
                if rep.get(key):
                    p(f'  {sym} {key} ({len(rep[key])}): {_fmt_samples(rep[key])}')

    if report.get('crypto'):
        p(f'== Crypto daily ({CRYPTO_DAILY_DIR}, read-only) + funding coverage ==')
        headers = ['symbol', 'rows', 'first', 'last', 'dup', 'gap_days', 'ohlc', 'open_gap>2%',
                   'fund_miss', 'max|rate|', 'status']
        body = []
        for sym, rep in report['crypto']['symbols'].items():
            if 'error' in rep:
                body.append([sym, *['-'] * 9, rep['status']])
                continue
            fund = rep.get('funding') or {}
            max_rate = fund.get('max_abs_rate')
            body.append([sym, rep['rows'], rep['first'], rep['last'], len(rep['dup_dates']),
                         rep['gap_days'], len(rep['ohlc_violations']), len(rep['open_gaps']),
                         fund.get('missing_days', '-'),
                         f'{max_rate:.4g}' if max_rate is not None else '-', rep['status']])
        p(_table(headers, body))
        for sym, rep in report['crypto']['symbols'].items():
            for key in ('dup_dates', 'gap_segments', 'ohlc_violations', 'open_gaps'):
                if rep.get(key):
                    p(f'  {sym} {key} ({len(rep[key])}): {_fmt_samples(rep[key])}')
            fund = rep.get('funding') or {}
            if fund.get('missing_dates'):
                p(f"  {sym} funding missing days ({fund['missing_days']}): "
                  f"{_fmt_samples(fund['missing_dates'])}")
            if fund.get('warn'):
                p(f"  {sym} WARNING funding |rate| max {fund['max_abs_rate']:.4g} > 1%")
            if fund.get('error'):
                p(f"  {sym} funding: {fund['error']}")

    if report.get('klines'):
        p(f'== Klines ({KLINES_DIR}) ==')
        headers = ['file', 'rows', 'first', 'last', 'dup', 'missing', 'ohlc', 'zero_vol', 'status']
        body = []
        for spec, rep in report['klines']['files'].items():
            if 'error' in rep:
                body.append([spec, *['-'] * 7, rep['status']])
                continue
            body.append([spec, rep['rows'], _fmt_ts(rep['first_ms']) if rep['first_ms'] else '-',
                         _fmt_ts(rep['last_ms']) if rep['last_ms'] else '-',
                         len(rep['dup_bars']), rep['missing_bars'], len(rep['ohlc_violations']),
                         len(rep['zero_volume_bars']), rep['status']])
        p(_table(headers, body))
        for spec, rep in report['klines']['files'].items():
            for key, fmt in (('dup_bars', _fmt_ts), ('ohlc_violations', _fmt_ts),
                             ('zero_volume_bars', _fmt_ts)):
                if rep.get(key):
                    p(f'  {spec} {key} ({len(rep[key])}): {_fmt_samples(rep[key], fmt=fmt)}')
            if rep.get('gap_segments'):
                segs = [f'{_fmt_ts(a)} -> {_fmt_ts(b)} (+{k})' for a, b, k in rep['gap_segments']]
                p(f"  {spec} gaps ({rep['missing_bars']} bars): {_fmt_samples(segs)}")
        for sym, rep in (report['klines'].get('pairs') or {}).items():
            p(f'  {sym} 5m aggregated vs 1h: {rep["common_bars"]} common hours, '
              f'{rep["mismatched_bars"]} mismatched (>1e-6)')
            for s in rep['samples']:
                p(f'    {_fmt_ts(s["time_ms"])} {s["field"]}: 1h={s["coarse"]} agg={s["aggregated"]}')

    cross = report.get('cross_check') or {}
    if cross:
        p('== Cross-check (network) ==')
        for sym, rep in cross.items():
            if 'error' in rep:
                p(f'  {sym}: ERROR {rep["error"]}')
                continue
            p(f'  {sym} vs {rep["source"]}' + (f' [{rep["assetclass"]}]' if rep.get('assetclass') else '')
              + f': compared {rep["compared_days"]} days, mismatches {rep["mismatch_days"]}'
              + (f' ({rep["samples"][0][0]} first: ours={rep["samples"][0][1]} theirs={rep["samples"][0][2]})'
                 if rep.get('samples') else ''))


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #

def main(argv=None):
    parser = argparse.ArgumentParser(
        prog='python -m scripts.backtest.market_cache', description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--data-root', default=DEFAULT_DATA_ROOT,
                        help=f'data root directory (default: {DEFAULT_DATA_ROOT})')
    parser.add_argument('--proxy', default=DEFAULT_PROXY,
                        help=f'HTTP(S) proxy, empty string disables it (default: {DEFAULT_PROXY})')
    sub = parser.add_subparsers(dest='command', required=True)

    p_us = sub.add_parser('us', help='incrementally update Yahoo US daily prices')
    p_us.add_argument('symbols', nargs='+', metavar='SYM')

    p_kl = sub.add_parser('klines', help='incrementally update Binance USDT-M klines')
    p_kl.add_argument('symbol')
    p_kl.add_argument('interval', choices=sorted(KLINE_MS))
    p_kl.add_argument('--start', metavar='YYYY-MM-DD',
                      help='first date; required when no cache file exists yet')

    p_ck = sub.add_parser('check', help='read-only validation of cached data')
    p_ck.add_argument('--us', nargs='*', default=None, metavar='SYM',
                      help='US symbols (no list = every file)')
    p_ck.add_argument('--crypto', nargs='*', default=None, metavar='SYM',
                      help='crypto daily symbols (no list = every file)')
    p_ck.add_argument('--klines', nargs='*', default=None, metavar='SYM_INTERVAL',
                      help='kline files (no list = every file)')
    p_ck.add_argument('--json', dest='json_out', metavar='OUT.json',
                      help='also write the full report as JSON')
    p_ck.add_argument('--cross-check', action='store_true',
                      help='compare against nasdaq.com / binance (needs network)')
    args = parser.parse_args(argv)

    if args.command == 'us':
        failures = 0
        for sym in args.symbols:
            try:
                update_us(sym, args.data_root, args.proxy)
            except Exception as exc:
                failures += 1
                print(f'{sym}: FAILED: {exc}', file=sys.stderr)
        return 1 if failures else 0

    if args.command == 'klines':
        update_klines(args.symbol, args.interval, args.data_root, args.proxy, start=args.start)
        return 0

    report = check_all(args.data_root, us=args.us, crypto=args.crypto, klines=args.klines,
                       cross_check=args.cross_check, proxy=args.proxy)
    print_report(report)
    if args.json_out:
        json_path = Path(args.json_out)
        json_path.parent.mkdir(parents=True, exist_ok=True)
        json_path.write_text(json.dumps(report, indent=2, allow_nan=False) + '\n',
                             encoding='utf-8')
        print(f'report written to {json_path}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
