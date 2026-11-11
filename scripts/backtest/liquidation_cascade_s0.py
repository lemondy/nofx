"""Frozen liquidation-cascade S0 (forward-return research, no orders or P&L).

Only ``fetch`` can access the network. ``plan`` and ``run`` are offline.
python -m scripts.backtest.liquidation_cascade_s0 {plan,fetch,run} --help
"""
from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from concurrent.futures import FIRST_COMPLETED, ThreadPoolExecutor, wait
from datetime import date, datetime, timedelta, timezone
import hashlib
import io
import json
import math
from pathlib import Path
import re
import subprocess
import time
import urllib.error
import urllib.request
import zipfile

import numpy as np
import pandas as pd

from .market_cache import DEFAULT_PROXY

REPO = Path(__file__).resolve().parents[2]
DATA = Path('/Users/zhangyun/workspace/nofx/data')
PREREG = REPO / 'docs/research/2026-11-11-liquidation-cascade-s0-prereg.md'
PREREG_SHA256 = 'da0dfb6541e25f7d75bf4010fecaa9ff0ce44a0c6b50f59afb7f26736f84fdd0'
TRADES_SHA256 = '8abfa110e9ae7e84981150572ea1954178f263f9ba1a0690163d305157382121'
N_TRADES, N_TRADE_SYMBOLS = 439, 115
BASE_URL = 'https://data.binance.vision/data/futures/um'
STEP, HOUR, DAY = 300_000, 3_600_000, 86_400_000
A_START, A_END = '2023-01-01', '2026-05-01'  # exclusive end
B_START, B_END = '2026-09-22', '2026-11-09'
HALF = '2024-09-01'
HORIZONS = (1, 4, 12, 24)
KS, MS = (2.5, 3., 4.), (.03, .05, .08)
MAIN_K, MAIN_M = 3., .05
CHOICES = [
    'm 的表格符号有歧义，遵从冻结主参数：C 为 dOI≤ln(1-m)；D 按字面为 dOI>-0.01（不是 ln(0.99)）。',
    'σ 使用样本标准差（ddof=1），窗口严格为 t-168h 至 t-1h 的168个网格，至少150个有效收益；不前填。',
    '月初距离首根日线至少30个自然日才入池；成交额窗口为 [月初-30天,月初)，缺日为零；同分按币名排序。',
    '24h 去重按保留事件向后滚动，恰好24h可保留；C、D独立，先去重再检查前向缺失，不递补。',
    'A 每个事件要求本币全部四个前向窗口完整，任何一个缺失即整事件丢弃；BTC缺失仅剔除扣BTC描述样本，不改变原始主检验。',
    'BTC本身不计算扣BTC收益；BTCUSDT、ETHUSDT、其余币分别报告，另报BTC/ETH合组。',
    '全部急跌忽略OI，仍按同币24h取首个；无条件基准不要求OI或σ、不去重，只要求4h前向完整。',
    'B 不去重，也不检查或计算前向收益；事件时间仅限登记B段，σ只使用已有价格历史中此前168h的整点收益。',
    'B 缺OI交易：入场前4h内任一可能事件整点的OI端点缺失即计数，即使已匹配其他C也计数；比例分母仍为全部同side交易。',
    'metrics少于280行的整个UTC日OI缺失；重复时间、错币、错日、非5m网格等结构错误拒绝解释。',
    '日收盘比对只使用完整288根5m的UTC日与面板的交集；不完整日单独报告，不用盘中末价当日收盘。无可比日也视为数据检查失败。',
    'CR1=G/(G-1)×(N-1)/(N-2)×聚类夹心方差；少于2日、缺类别、或零SE时t为null，不能通过显著性条件。',
    'σ为零时仍严格按r≤-kσ分类，不另加未登记的最小波动门槛。',
    '币名含非ASCII字符的永续（如中文名）照常参与时点top60排名，但不下载、不替补，入选月份列在plan.unfetchable。',
    'fetch其他错误最多重试3次（共4次尝试），404永久记录并在续传时跳过；已有文件须与manifest的SHA256一致才跳过。',
]


def file_sha256(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b''):
            digest.update(chunk)
    return digest.hexdigest()


def json_safe(value):
    if isinstance(value, dict):
        return {str(k): json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(v) for v in value]
    if isinstance(value, np.generic):
        return json_safe(value.item())
    if isinstance(value, float) and not math.isfinite(value):
        return None
    return value


def json_bytes(value):
    return (json.dumps(json_safe(value), ensure_ascii=False, sort_keys=True,
                       allow_nan=False, separators=(',', ':')) + '\n').encode('utf-8')


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.tmp')
    temporary.write_bytes(json_bytes(value))
    temporary.replace(path)


def ms(text):
    return int(pd.Timestamp(text, tz='UTC').timestamp() * 1000)


def utc_text(timestamp):
    return datetime.fromtimestamp(int(timestamp) / 1000, timezone.utc).isoformat()


def month_after(day, offset=1):
    n = day.year * 12 + day.month - 1 + offset
    return date(n // 12, n % 12 + 1, 1)


def dates(start, end):
    """Calendar days, exclusive end."""
    day = date.fromisoformat(start) if isinstance(start, str) else start
    end = date.fromisoformat(end) if isinstance(end, str) else end
    while day < end:
        yield day
        day += timedelta(days=1)


def check_prereg():
    if file_sha256(PREREG) != PREREG_SHA256:
        raise ValueError('prereg sha256 mismatch，拒绝运行')


def project_trade(row):
    # Intentionally access exactly these three fields, never R/prices/P&L.
    trade = {key: row[key] for key in ('symbol', 'side', 'entry_time')}
    if not re.fullmatch(r'[A-Z0-9]+USDT', trade['symbol']):
        raise ValueError('交易symbol须为USDT大写币名')
    if trade['side'] not in ('LONG', 'SHORT') or type(trade['entry_time']) is not int:
        raise ValueError('交易side须为LONG/SHORT，entry_time须为UTC整数毫秒')
    if not ms(B_START) <= trade['entry_time'] < ms(B_END):
        raise ValueError('交易entry_time超出冻结B段')
    return trade


def load_trades(path):
    # Hash transport bytes, then discard every unselected JSON field immediately.
    if file_sha256(path) != TRADES_SHA256:
        raise ValueError('trades sha256 mismatch，拒绝读取交易')
    with Path(path).open(encoding='utf-8') as source:
        trades = [project_trade(json.loads(line)) for line in source if line.strip()]
    if len(trades) != N_TRADES or len({t['symbol'] for t in trades}) != N_TRADE_SYMBOLS:
        raise ValueError('冻结交易笔数/币数不符')
    return trades


def read_daily(path):
    rows = json.loads(Path(path).read_bytes())
    if not rows:
        return pd.DataFrame(columns=['turnover', 'close'], index=pd.DatetimeIndex([]))
    frame = pd.DataFrame(rows)
    if frame.shape[1] != 7:
        raise ValueError(f'{path}: 日线须为[date,o,h,l,c,volume,vwap]')
    index = pd.to_datetime(frame[0], format='%Y-%m-%d', errors='raise')
    if index.duplicated().any():
        raise ValueError(f'{path}: 日线日期重复')
    values = frame[[4, 5, 6]].apply(pd.to_numeric, errors='raise').to_numpy(float)
    if not np.isfinite(values).all() or (values[:, 0] <= 0).any() or (values[:, 1:] < 0).any():
        raise ValueError(f'{path}: 日线收盘/量/vwap非法')
    return pd.DataFrame({'turnover': values[:, 1] * values[:, 2], 'close': values[:, 0]},
                        index=pd.DatetimeIndex(index)).sort_index()


def select_pools(daily_dir, months=None, top_n=60):
    months = list(months) if months is not None else list(pd.period_range('2023-01', '2026-04', freq='M').astype(str))
    scores = {month: [] for month in months}
    windows = {month: (pd.Timestamp(date.fromisoformat(month + '-01') - timedelta(days=30)),
                       pd.Timestamp(month + '-01')) for month in months}
    hashes = {}
    paths = sorted(Path(daily_dir).glob('*.json'))
    if not paths:
        raise ValueError('daily目录没有JSON日线')
    for path in paths:
        symbol = path.stem
        if not re.fullmatch(r'\w+USDT', symbol):
            raise ValueError(f'非法日线币名: {symbol}')
        hashes[path.name] = file_sha256(path)
        daily = read_daily(path)
        if daily.empty:
            continue
        for month in months:
            start, end = windows[month]
            if daily.index[0] > start:
                continue
            turnover = daily.loc[(daily.index >= start) & (daily.index < end), 'turnover'].sum()
            scores[month].append((float(turnover), symbol))
    pools = {month: [symbol for _, symbol in sorted(items, key=lambda x: (-x[0], x[1]))[:top_n]]
             for month, items in scores.items()}
    return pools, hashes


def make_plan(daily_dir, trades):
    pools, hashes = select_pools(daily_dir)
    metrics = defaultdict(set)
    monthly = defaultdict(set)
    daily_k = defaultdict(set)
    # Non-ASCII perps (e.g. Chinese names) still rank in the pool, but are not
    # downloaded and are reported; no 61st-ranked replacement is drawn.
    unfetchable = {month: [s for s in symbols if not re.fullmatch(r'[A-Z0-9]+USDT', s)]
                   for month, symbols in pools.items()}
    for month, symbols in pools.items():
        start = date.fromisoformat(month + '-01')
        end = month_after(start)
        for symbol in (set(symbols) - set(unfetchable[month])) | {'BTCUSDT'}:
            metrics[symbol].update(d.isoformat() for d in dates(start - timedelta(days=8), end))
            monthly[symbol].update((month, month_after(start, -1).strftime('%Y-%m')))
            daily_k[symbol].update(d.isoformat() for d in dates(end, end + timedelta(days=2)))
    b_symbols = sorted({t['symbol'] for t in trades} | {'BTCUSDT'})
    for symbol in b_symbols:
        metrics[symbol].update(d.isoformat() for d in dates(B_START, B_END))
        monthly[symbol].update(('2026-09', '2026-10'))
        daily_k[symbol].update(d.isoformat() for d in dates('2026-11-01', '2026-11-10'))
    files = []
    for symbol in sorted(metrics):
        for day in sorted(metrics[symbol]):
            name = f'{symbol}-metrics-{day}.zip'
            files.append(dict(kind='metrics', symbol=symbol, period=day,
                              path=f'raw/metrics/{symbol}/{name}',
                              url=f'{BASE_URL}/daily/metrics/{symbol}/{name}'))
        for month in sorted(monthly[symbol]):
            name = f'{symbol}-5m-{month}.zip'
            files.append(dict(kind='klines5m', symbol=symbol, period=month,
                              path=f'raw/klines5m/{symbol}/{name}',
                              url=f'{BASE_URL}/monthly/klines/{symbol}/5m/{name}'))
        for day in sorted(daily_k[symbol]):
            if day[:7] in monthly[symbol]:
                continue  # monthly archive already supplies this day, no duplicate bars
            name = f'{symbol}-5m-{day}.zip'
            files.append(dict(kind='klines5m', symbol=symbol, period=day,
                              path=f'raw/klines5m/{symbol}/{name}',
                              url=f'{BASE_URL}/daily/klines/{symbol}/5m/{name}'))
    counts = dict(Counter(item['kind'] for item in files))
    counts.update(total=len(files), symbols=len(metrics), pool_months=len(pools),
                  pool_symbol_months=sum(map(len, pools.values())),
                  b_trades=len(trades), b_symbols=len({t['symbol'] for t in trades}))
    return dict(version=1, prereg_sha256=PREREG_SHA256, trades_sha256=TRADES_SHA256,
                daily_dir=str(Path(daily_dir).resolve()), daily_sha256=hashes,
                a_range=[A_START, A_END], b_range=[B_START, B_END],
                pools=pools, unfetchable={m: v for m, v in unfetchable.items() if v},
                b_symbols=b_symbols, files=files, counts=counts)


def load_plan(data_dir):
    plan = json.loads((Path(data_dir) / 'plan.json').read_bytes())
    if (plan['version'] != 1 or plan['prereg_sha256'] != PREREG_SHA256
            or plan['trades_sha256'] != TRADES_SHA256
            or plan['a_range'] != [A_START, A_END] or plan['b_range'] != [B_START, B_END]):
        raise ValueError('plan冻结输入/时间范围不符')
    seen = set()
    for item in plan['files']:
        symbol, period, kind = item['symbol'], item['period'], item['kind']
        if not re.fullmatch(r'[A-Z0-9]+USDT', symbol) or kind not in ('metrics', 'klines5m'):
            raise ValueError('非法plan币名/文件种类')
        daily = len(period) == 10
        datetime.strptime(period, '%Y-%m-%d' if daily else '%Y-%m')
        if kind == 'metrics':
            name = f'{symbol}-metrics-{period}.zip'
            url = f'{BASE_URL}/daily/metrics/{symbol}/{name}'
            if not daily:
                raise ValueError('metrics必须是日文件')
        else:
            name = f'{symbol}-5m-{period}.zip'
            url = f'{BASE_URL}/{"daily" if daily else "monthly"}/klines/{symbol}/5m/{name}'
        if item['path'] != f'raw/{kind}/{symbol}/{name}' or item['url'] != url or item['path'] in seen:
            raise ValueError('plan路径/URL非法或重复')
        seen.add(item['path'])
    return plan


def http_zip(url, *, proxy=DEFAULT_PROXY, timeout=60):
    """The sole network boundary, called only by fetch_plan."""
    opener = urllib.request.build_opener(urllib.request.ProxyHandler(
        {'http': proxy, 'https': proxy} if proxy else {}))
    with opener.open(url, timeout=timeout) as response:
        return response.read()


def fetch_plan(data_dir, plan, *, workers=8, proxy=DEFAULT_PROXY, fetch=None, sleeper=time.sleep):
    if workers < 1:
        raise ValueError('workers必须≥1')
    fetch = http_zip if fetch is None else fetch
    root = Path(data_dir)
    manifest_path = root / 'manifest.json'
    manifest = (json.loads(manifest_path.read_bytes()) if manifest_path.exists()
                else dict(version=1, files={}, missing=[], errors=[]))
    missing = {item['path']: item for item in manifest.get('missing', [])}
    errors = {item['path']: item for item in manifest.get('errors', [])}
    manifest['plan_sha256'] = file_sha256(root / 'plan.json')

    def pending():
        for item in plan['files']:
            key = item['path']
            path = root / key
            old = manifest['files'].get(key)
            if path.exists() and old and file_sha256(path) == old['sha256']:
                continue
            if key in missing and not path.exists():
                continue
            yield item

    def download(item):
        path = root / item['path']
        part = path.with_name(path.name + '.part')
        for attempt in range(4):
            try:
                content = fetch(item['url'], proxy=proxy, timeout=60)
                with zipfile.ZipFile(io.BytesIO(content)) as archive:
                    if archive.testzip() is not None or not archive.namelist():
                        raise ValueError('zip CRC失败或为空')
                path.parent.mkdir(parents=True, exist_ok=True)
                part.write_bytes(content)
                digest = file_sha256(part)
                part.replace(path)
                return 'files', dict(item, sha256=digest, bytes=len(content), attempts=attempt + 1)
            except urllib.error.HTTPError as error:
                if error.code == 404:
                    part.unlink(missing_ok=True)
                    return 'missing', dict(item, status=404)
                last = str(error)
            except Exception as error:
                # Includes truncated HTTP bodies and errors from injected clients;
                # BaseException (interrupt/cancellation) is deliberately not retried.
                last = str(error)
            part.unlink(missing_ok=True)
            if attempt < 3:
                sleeper(min(2 ** attempt, 4))
        return 'errors', dict(item, error=last, attempts=4)

    def checkpoint():
        manifest['missing'] = sorted(missing.values(), key=lambda r: r['path'])
        manifest['errors'] = sorted(errors.values(), key=lambda r: r['path'])
        write_json(manifest_path, manifest)

    # Bound both queued work and completed payloads. Checkpoint every 100 files;
    # per-file rewriting of a ~100k-entry manifest would be quadratic.
    iterator = iter(pending())
    completed = 0
    with ThreadPoolExecutor(max_workers=workers) as executor:
        futures = set()
        try:
            while True:
                while len(futures) < 2 * workers:
                    item = next(iterator, None)
                    if item is None:
                        break
                    futures.add(executor.submit(download, item))
                if not futures:
                    break
                done, futures = wait(futures, return_when=FIRST_COMPLETED)
                for future in done:
                    kind, record = future.result()
                    key = record['path']
                    missing.pop(key, None)
                    errors.pop(key, None)
                    manifest['files'].pop(key, None)
                    if kind == 'files':
                        manifest['files'][key] = record
                    else:
                        (missing if kind == 'missing' else errors)[key] = record
                    completed += 1
                    if completed % 100 == 0:
                        checkpoint()
        finally:
            checkpoint()
    return manifest


def csv_member(path, **kwargs):
    with zipfile.ZipFile(path) as archive:
        names = [n for n in archive.namelist() if n.lower().endswith('.csv')]
        if len(names) != 1:
            raise ValueError(f'{path}: zip须包含恰好一个CSV')
        with archive.open(names[0]) as source:
            return pd.read_csv(source, **kwargs)


def read_klines(paths):
    parts = []
    for path in paths:
        frame = csv_member(path, header=None, usecols=list(range(6)), dtype=str)
        if len(frame) and frame.iloc[0, 0].lstrip('\ufeff').strip().lower() == 'open_time':
            frame = frame.iloc[1:]
        numeric = frame.apply(pd.to_numeric, errors='raise').to_numpy(float)
        if not len(numeric):
            continue
        # Exact integer conversion; present epoch microseconds remain below 2**53.
        stamp = numeric[:, 0]
        if not np.isfinite(stamp).all() or (stamp != np.floor(stamp)).any():
            raise ValueError(f'{path}: 时间戳非法')
        stamp = np.where(stamp >= 10**14, stamp / 1000, stamp)
        if (stamp % STEP != 0).any() or (stamp < 10**12).any():
            raise ValueError(f'{path}: 5m时间戳须为毫秒或微秒并对齐网格')
        o, h, l, c, volume = numeric[:, 1:].T
        if (not np.isfinite(numeric).all() or (numeric[:, 1:5] <= 0).any()
                or (volume < 0).any() or (h < np.maximum(o, c)).any()
                or (l > np.minimum(o, c)).any() or (h < l).any()):
            raise ValueError(f'{path}: OHLC/volume非法')
        parts.append(pd.DataFrame(numeric[:, 1:5], columns=['open', 'high', 'low', 'close'],
                                  index=stamp.astype(np.int64)))
    if not parts:
        return pd.DataFrame(columns=['open', 'high', 'low', 'close'], index=pd.Index([], dtype='int64'))
    bars = pd.concat(parts).sort_index()
    if bars.index.duplicated().any():
        raise ValueError('5m K线重复（包括跨文件重复）')
    return bars


def read_metrics(paths, symbol, expected_days):
    parts, bad_days, counts = [], [], {}
    for path in paths:
        day = Path(path).stem[-10:]
        frame = csv_member(path, usecols=['create_time', 'symbol', 'sum_open_interest'])
        stamp = pd.to_datetime(frame['create_time'], utc=True, errors='raise')
        times = stamp.astype('int64').to_numpy() // 1_000_000
        if (stamp.isna().any() or stamp.duplicated().any()
                or not (frame['symbol'] == symbol).all()
                or (times % STEP != 0).any()
                or (stamp.dt.strftime('%Y-%m-%d') != day).any() or len(frame) > 288):
            raise ValueError(f'{path}: metrics时间/币名/行数/重复非法')
        counts[day] = len(frame)
        if len(frame) < 280:
            bad_days.append(day)
            continue
        oi = pd.to_numeric(frame['sum_open_interest'], errors='coerce').to_numpy(float)
        oi[(oi <= 0) | ~np.isfinite(oi)] = np.nan
        parts.append(pd.Series(oi, index=times))
    series = pd.concat(parts).sort_index() if parts else pd.Series(dtype=float)
    if series.index.duplicated().any():
        raise ValueError('跨文件metrics重复')
    valid_hours = sum((series.index.to_numpy() % HOUR == 0) & series.notna().to_numpy())
    return series, dict(expected_days=len(expected_days), present_days=len(counts),
                        absent_days=sorted(set(expected_days) - counts.keys()),
                        day_row_counts=counts, days_below_280=bad_days,
                        non_288_days={d: n for d, n in counts.items() if n != 288},
                        valid_hourly_oi=int(valid_hours),
                        hourly_coverage=valid_hours / (24 * len(expected_days)) if expected_days else None)


def daily_close_check(bars, daily):
    if bars.empty:
        return dict(compared=0, mismatches=0, incomplete_days=0, mismatch_days=[])
    grouped = bars['close'].groupby(bars.index.to_numpy() // DAY)
    aggregates = grouped.agg(['size', 'last'])
    complete = aggregates[aggregates['size'] == 288].copy()
    complete.index = pd.to_datetime(complete.index.to_numpy() * DAY, unit='ms')
    joined = complete.join(daily[['close']], how='inner')
    error = (joined['last'] / joined['close'] - 1).abs()
    bad = error[error > .005]
    return dict(compared=len(joined), mismatches=len(bad),
                incomplete_days=int((aggregates['size'] != 288).sum()),
                mismatch_days=[dict(day=d.strftime('%Y-%m-%d'), relative_error=float(v)) for d, v in bad.items()])


def hourly_features(bars, oi):
    bounds = []
    if not bars.empty:
        bounds.extend((int(bars.index.min()), int(bars.index.max()) + STEP))
    if not oi.empty:
        bounds.extend((int(oi.index.min()), int(oi.index.max())))
    if not bounds:
        return pd.DataFrame(columns=['close', 'r_1h', 'sigma', 'dOI_1h'],
                            index=pd.Index([], dtype='int64'), dtype=float)
    hours = np.arange(min(bounds) // HOUR * HOUR, max(bounds) // HOUR * HOUR + 1, HOUR, dtype=np.int64)
    closes = bars['close'].astype(float).copy()
    closes.index = closes.index + STEP  # mathematical bar END, not Binance end-minus-1ms
    close = closes.reindex(hours)
    returns = np.log(close / close.shift(1))
    sigma = returns.shift(1).rolling(168, min_periods=150).std(ddof=1)
    open_interest = oi.reindex(hours)
    doi = np.log(open_interest / open_interest.shift(1))
    return pd.DataFrame(dict(close=close, r_1h=returns, sigma=sigma, dOI_1h=doi))


def classify(frame, k=MAIN_K, m=MAIN_M):
    eligible = (frame['r_1h'].notna() & frame['sigma'].notna() & frame['dOI_1h'].notna()
                & (frame['r_1h'] <= -k * frame['sigma']))
    result = pd.Series('', index=frame.index, dtype=object)
    result.loc[eligible] = 'M'
    result.loc[eligible & (frame['dOI_1h'] <= math.log(1 - m))] = 'C'
    result.loc[eligible & (frame['dOI_1h'] > -.01)] = 'D'
    return result


def deduplicate(frame):
    last, keep = {}, []
    for t, row in frame.sort_index().iterrows():
        key = (row.get('symbol', ''), row['category'])
        if key not in last or int(t) - last[key] >= DAY:
            keep.append(t)
            last[key] = int(t)
    return frame.loc[keep].sort_index()


def forward_returns(bars, times, horizons=HORIZONS):
    times = np.asarray(times, dtype=np.int64)
    result = pd.DataFrame(index=times)
    stamps = bars.index.to_numpy(np.int64)
    for h in horizons:
        values = np.full(len(times), np.nan)
        starts, ends = times + STEP, times + STEP + h * HOUR
        left, right = np.searchsorted(stamps, starts), np.searchsorted(stamps, ends)
        valid = (right - left == h * HOUR // STEP)
        if len(stamps):
            pos = np.flatnonzero(valid)
            # Aligned, unique timestamps plus exact count imply a gap-free window.
            valid[pos] &= ((stamps[left[pos]] == starts[pos]) & (stamps[right[pos] - 1] == ends[pos] - STEP))
            pos = np.flatnonzero(valid)
            values[pos] = np.log(bars['close'].to_numpy()[right[pos] - 1]
                                 / bars['open'].to_numpy()[left[pos]])
        result[f'R_{h}h'] = values
    return result


def attach_returns(frame, bars, btc_returns=None, symbol=''):
    forwards = forward_returns(bars, frame.index)
    missing = forwards.isna().any(axis=1)
    result = frame.join(forwards).loc[~missing].copy()
    btc_missing = 0
    for h in HORIZONS:
        column = f'R_{h}h'
        if symbol == 'BTCUSDT' or btc_returns is None:
            result[column + '_ex_btc'] = np.nan
        else:
            benchmark = btc_returns[column].reindex(result.index)
            result[column + '_ex_btc'] = result[column] - benchmark
            if h == 24:
                btc_missing = int(benchmark.isna().sum())
    # Match the complete four-window rule for the BTC-adjusted descriptive sample.
    adjusted = [f'R_{h}h_ex_btc' for h in HORIZONS]
    result.loc[result[adjusted].isna().any(axis=1), adjusted] = np.nan
    return result, dict(candidates=len(frame), forward_missing=int(missing.sum()),
                        forward_missing_by_h={str(h): int(forwards[f'R_{h}h'].isna().sum()) for h in HORIZONS},
                        btc_forward_missing=btc_missing)


class CR1Stats:
    """Streaming OLS of R ~ 1 + C, using sufficient statistics by UTC date.

    The C coefficient is mean(C)-mean(D). Each day's influence is its C
    residual sum/N_C minus its D residual sum/N_D, exactly the OLS sandwich.
    """

    def __init__(self):
        self.n = Counter()
        self.sums = Counter()
        self.days = defaultdict(lambda: [0, 0., 0, 0.])

    def add(self, frame, column='R_4h'):
        selected = frame.loc[frame['category'].isin(['C', 'D']) & frame[column].notna(), ['category', column]].copy()
        selected['day'] = selected.index.to_numpy(np.int64) // DAY
        for (day, kind), group in selected.groupby(['day', 'category']):
            n, total = len(group), float(group[column].sum())
            self.n[kind] += n
            self.sums[kind] += total
            offset = 0 if kind == 'C' else 2
            self.days[int(day)][offset] += n
            self.days[int(day)][offset + 1] += total

    def summary(self):
        nc, nd = self.n['C'], self.n['D']
        c = self.sums['C'] / nc if nc else None
        d = self.sums['D'] / nd if nd else None
        delta = c - d if c is not None and d is not None else None
        n, g = nc + nd, len(self.days)
        se = t = None
        if nc and nd and n > 2 and g > 1:
            influence = [(sc - gc * c) / nc - (sd - gd * d) / nd
                         for gc, sc, gd, sd in self.days.values()]
            variance = g / (g - 1) * (n - 1) / (n - 2) * sum(x * x for x in influence)
            se = math.sqrt(max(variance, 0.))
            t = delta / se if se > 0 else None
        return dict(N_C=nc, N_D=nd, mean_C=c, mean_D=d, delta=delta, SE=se, t=t, clusters=g)


def cluster_ols(frame, column='R_4h'):
    stats = CR1Stats()
    stats.add(frame, column)
    return stats.summary()


def decision(main, halves):
    positive = lambda x: x is not None and math.isfinite(x) and x > 0
    checks = dict(sample_size=main['N_C'] >= 100 and main['N_D'] >= 100,
                  positive_significant_delta=positive(main['delta']) and main['t'] is not None
                  and math.isfinite(main['t']) and main['t'] >= 2.,
                  positive_c_mean=positive(main['mean_C']),
                  both_halves_positive=all(positive(halves[h]['delta']) for h in ('H1', 'H2')))
    holds = all(checks.values())
    return dict(mechanism_holds=holds, verdict='holds' if holds else
                ('insufficient' if not checks['sample_size'] else 'not_holds'), checks=checks)


def qualifies_for_s1(mechanism_holds, short_fraction):
    return bool(mechanism_holds and short_fraction is not None and short_fraction >= .10)


def trade_overlap(trades, features_by_symbol):
    sides = {side: dict(total=0, after_cascade=0, missing_oi=0, missing_signal_inputs=0) for side in ('LONG', 'SHORT')}
    for source in trades:
        # Retain the projection boundary even for callers supplying raw mappings.
        trade = project_trade(source)
        symbol, entry = trade['symbol'], trade['entry_time']
        row = sides[trade['side']]
        row['total'] += 1
        features = features_by_symbol.get(symbol)
        first = (entry - 4 * HOUR + HOUR - 1) // HOUR * HOUR
        hours = np.arange(first, (entry - 1) // HOUR * HOUR + 1, HOUR, dtype=np.int64)
        if features is None:
            row['missing_oi'] += 1
            row['missing_signal_inputs'] += 1
            continue
        window = features.reindex(hours)
        row['missing_oi'] += int(window['dOI_1h'].isna().any())
        row['missing_signal_inputs'] += int(window[['r_1h', 'sigma', 'dOI_1h']].isna().any().any())
        row['after_cascade'] += int((window['category'] == 'C').any())
    for row in sides.values():
        row['fraction'] = row['after_cascade'] / row['total'] if row['total'] else None
    return dict(by_side=sides, missing_oi_trades=sum(r['missing_oi'] for r in sides.values()),
                short_fraction=sides['SHORT']['fraction'])


def grouped_files(plan, root):
    grouped = defaultdict(lambda: dict(metrics=[], klines5m=[], expected_days=[]))
    for item in plan['files']:
        row = grouped[item['symbol']]
        path = Path(root) / item['path']
        if path.exists():
            row[item['kind']].append(path)
        if item['kind'] == 'metrics':
            row['expected_days'].append(item['period'])
    return grouped


def inspect_data(root, plan, daily_dir):
    """Preflight pass: no forward returns or IC are computed here."""
    manifest = json.loads((Path(root) / 'manifest.json').read_bytes())
    checks = dict(errors=[], symbols={}, compared=0, mismatches=0, missing=manifest.get('missing', []))
    if manifest.get('plan_sha256') != file_sha256(Path(root) / 'plan.json'):
        checks['errors'].append('manifest与plan SHA256不符')
    missing = {item['path'] for item in checks['missing'] if item.get('status') == 404}
    recorded = manifest.get('files', {})
    for item in plan['files']:
        key, path = item['path'], Path(root) / item['path']
        if not path.exists():
            if key not in missing:
                checks['errors'].append(f'未下载且非404: {key}')
        elif key not in recorded or file_sha256(path) != recorded[key]['sha256']:
            checks['errors'].append(f'原始文件SHA256不符: {key}')
    if set(p.name for p in Path(daily_dir).glob('*.json')) != set(plan['daily_sha256']):
        checks['errors'].append('daily文件清单与plan不符')
    for name, digest in plan['daily_sha256'].items():
        path = Path(daily_dir) / name
        if not path.exists() or file_sha256(path) != digest:
            checks['errors'].append(f'daily SHA256不符: {name}')
    # Integrity/structural failures cannot be made interpretable by the debug flag.
    if checks['errors']:
        checks.update(passed=False, structural_failure=True, mismatch_fraction=None)
        return checks
    for symbol, paths in sorted(grouped_files(plan, root).items()):
        try:
            bars = read_klines(paths['klines5m'])
            _, coverage = read_metrics(paths['metrics'], symbol, paths['expected_days'])
            daily_path = Path(daily_dir) / f'{symbol}.json'
            daily = read_daily(daily_path) if daily_path.exists() else pd.DataFrame(columns=['close'])
            comparison = daily_close_check(bars, daily)
            checks['symbols'][symbol] = dict(metrics=coverage, klines=dict(rows=len(bars)), daily=comparison)
            checks['compared'] += comparison['compared']
            checks['mismatches'] += comparison['mismatches']
        except (OSError, ValueError, KeyError, zipfile.BadZipFile, pd.errors.ParserError) as error:
            checks['errors'].append(f'{symbol}: {error}')
    fraction = checks['mismatches'] / checks['compared'] if checks['compared'] else None
    checks.update(mismatch_fraction=fraction, structural_failure=bool(checks['errors']),
                  passed=not checks['errors'] and fraction is not None and fraction < .01)
    return checks


def event_lines(stream, frame, symbol, segment):
    for t, row in frame.iterrows():
        stream.write(json_bytes(dict(segment=segment, symbol=symbol, t=int(t),
                                     t_utc=utc_text(t), **row.to_dict())))


def analyze(root, plan, trades, stream):
    files = grouped_files(plan, root)
    # Keep only BTC's hourly forwards, release its 5m bars before other symbols.
    btc_bars = read_klines(files['BTCUSDT']['klines5m'])
    a_hours = np.arange(ms(A_START), ms(A_END), HOUR)
    b_hours = np.arange(ms(B_START), ms(B_END), HOUR)
    btc_returns = forward_returns(btc_bars, a_hours)
    del btc_bars
    grid = {(k, m): CR1Stats() for k in KS for m in MS}
    horizons = {(h, adjusted): CR1Stats() for h in HORIZONS for adjusted in (False, True)}
    halves = {name: CR1Stats() for name in ('H1', 'H2')}
    groups = {name: CR1Stats() for name in ('BTCUSDT', 'ETHUSDT', 'BTC_ETH', 'others')}
    counters, b_features = {}, {}
    unconditional_n = sharp_n = 0
    unconditional_sum = sharp_sum = 0.
    for symbol, paths in sorted(files.items()):
        bars = read_klines(paths['klines5m'])
        oi, _ = read_metrics(paths['metrics'], symbol, paths['expected_days'])
        features = hourly_features(bars, oi)
        # Include planned grid hours even when whole boundary archives are absent.
        a = features.reindex(a_hours).copy()
        active = np.zeros(len(a), dtype=bool)
        for month, symbols in plan['pools'].items():
            if symbol in symbols:
                start = ms(month + '-01')
                end = ms(month_after(date.fromisoformat(month + '-01')).isoformat())
                active |= (a.index >= start) & (a.index < end)
        a = a.loc[active]
        per_symbol = dict(pool_hours=len(a), missing_oi_hours=int(a['dOI_1h'].isna().sum()),
                          missing_sigma_hours=int(a['sigma'].isna().sum()), grid={})
        for k in KS:
            for m in MS:
                classified = a.assign(category=classify(a, k, m))
                candidates = classified.loc[classified['category'].isin(['C', 'D'])]
                deduped = deduplicate(candidates)
                events, exclusions = attach_returns(deduped, bars, btc_returns, symbol)
                exclusions.update(before_dedupe=len(candidates), deduplicated=len(candidates) - len(deduped),
                                  intermediate=int((classified['category'] == 'M').sum()))
                per_symbol['grid'][f'{k:g}/{m:g}'] = exclusions
                grid[k, m].add(events)
                if (k, m) == (MAIN_K, MAIN_M):
                    for h in HORIZONS:
                        horizons[h, False].add(events, f'R_{h}h')
                        horizons[h, True].add(events, f'R_{h}h_ex_btc')
                    halves['H1'].add(events.loc[events.index < ms(HALF)])
                    halves['H2'].add(events.loc[events.index >= ms(HALF)])
                    group = symbol if symbol in ('BTCUSDT', 'ETHUSDT') else 'others'
                    groups[group].add(events)
                    if group != 'others':
                        groups['BTC_ETH'].add(events)
                    event_lines(stream, events, symbol, 'A')
        sharp = a.loc[a['r_1h'] <= -MAIN_K * a['sigma']].assign(category='ALL_SHARP')
        sharp = deduplicate(sharp)
        sharp_returns, sharp_missing = attach_returns(sharp, bars, symbol=symbol)
        per_symbol['all_sharp'] = sharp_missing
        sharp_n += len(sharp_returns)
        sharp_sum += float(sharp_returns['R_4h'].sum())
        unconditional = forward_returns(bars, a.index, (4,))['R_4h']
        unconditional_n += int(unconditional.notna().sum())
        unconditional_sum += float(unconditional.sum())
        per_symbol['unconditional_forward_missing'] = int(unconditional.isna().sum())
        counters[symbol] = per_symbol
        if symbol in plan['b_symbols']:
            b = features.reindex(b_hours).copy()
            b['category'] = classify(b)
            b_features[symbol] = b  # hourly inputs only, never any B forward returns
            event_lines(stream, b.loc[b['category'] == 'C'], symbol, 'B')
    main = grid[MAIN_K, MAIN_M].summary()
    halves = {name: stats.summary() for name, stats in halves.items()}
    verdict = decision(main, halves)
    b = trade_overlap(trades, b_features)
    b['c_events'] = {symbol: int((frame['category'] == 'C').sum()) for symbol, frame in b_features.items()}
    eligible = qualifies_for_s1(verdict['mechanism_holds'], b['short_fraction'])
    b['s1_eligible'] = eligible
    mean_c = main['mean_C']
    return dict(**verdict, s1_eligible=eligible, main=main, halves=halves, B=b,
                sensitivity_grid=[dict(k=k, m=m, **stats.summary()) for (k, m), stats in grid.items()],
                horizons={str(h): dict(raw=horizons[h, False].summary(),
                                      ex_btc=horizons[h, True].summary()) for h in HORIZONS},
                groups={name: stats.summary() for name, stats in groups.items()},
                all_sharp=dict(N=sharp_n, mean_R_4h=sharp_sum / sharp_n if sharp_n else None),
                unconditional=dict(N=unconditional_n, mean_R_4h=unconditional_sum / unconditional_n if unconditional_n else None),
                cost=dict(round_trip=.0016, mean_C_log_R_4h=mean_c,
                          mean_C_minus_cost=mean_c - .0016 if mean_c is not None else None,
                          exceeds_cost=mean_c > .0016 if mean_c is not None else None), exclusions=counters)


def markdown_report(result):
    number = lambda value: 'NA' if value is None else (f'{value:.8g}' if isinstance(value, float) else str(value))
    labels = dict(holds='机制成立', not_holds='机制不成立', insufficient='样本不足', invalid='数据检查无效')
    lines = ['# 强平级联 S0', '', f"判定：**{labels[result['verdict']]}**；"
             f"mechanism_holds={result['mechanism_holds']}；S1资格={result['s1_eligible']}。", '',
             f"预注册 SHA256：`{result['prereg_sha256']}`", '', f"代码 HEAD：`{result['git_head']}`", '',
             '本报告为前向对数收益研究，无订单、交易盈亏或账本写入。', '']

    def table(title, columns, rows):
        lines.extend([f'## {title}', '', '| ' + ' | '.join(columns) + ' |',
                      '| ' + ' | '.join(['---'] * len(columns)) + ' |'])
        lines.extend('| ' + ' | '.join(number(v) for v in row) + ' |' for row in rows)
        lines.append('')

    def stats_table(title, rows):
        table(title, ['分组', 'N_C', 'N_D', 'mean C', 'mean D', 'Δ', 'CR1 SE', 't', 'UTC日数'],
              [[label] + [stats[k] for k in ('N_C', 'N_D', 'mean_C', 'mean_D', 'delta', 'SE', 't', 'clusters')]
               for label, stats in rows])

    if 'main' in result:
        stats_table('主指标与两半', [('主参数 k=3 m=5%', result['main']), *result['halves'].items()])
        table('四条判定规则', ['规则', '通过'], result['checks'].items())
        stats_table('敏感性网格（只描述）', [(f"k={r['k']:g}, m={r['m']:.0%}", r) for r in result['sensitivity_grid']])
        stats_table('其他 H 与扣 BTC（只描述）', [(f'{h}h {kind}', stats)
                    for h, pairs in result['horizons'].items() for kind, stats in pairs.items()])
        stats_table('BTC、ETH 与其他币（只描述）', result['groups'].items())
        table('全部急跌与无条件基准（只描述）', ['分组', 'N', 'mean R_4h'],
              [[key, result[key]['N'], result[key]['mean_R_4h']] for key in ('all_sharp', 'unconditional')])
        table('与0.16%往返成本对比（对数收益近似）', ['指标', '值'], result['cost'].items())
        table('B 级联后入场（只计数）', ['side', '总笔数', '级联后笔数', '占比', '缺OI笔数', '缺信号输入笔数'],
              [[side] + [row[k] for k in ('total', 'after_cascade', 'fraction', 'missing_oi', 'missing_signal_inputs')]
               for side, row in result['B']['by_side'].items()])
        table('B 未去重 C 事件数', ['币', 'C事件数'], result['B']['c_events'].items())
        rows = []
        for symbol, counts in result['exclusions'].items():
            for grid, row in counts['grid'].items():
                rows.append([symbol, grid, row['before_dedupe'], row['deduplicated'], row['intermediate'],
                             row['forward_missing'], row['btc_forward_missing'],
                             ', '.join(f'{h}h:{n}' for h, n in row['forward_missing_by_h'].items())])
        table('逐币逐网格剔除计数', ['币', 'k/m', '去重前', '去重剔除', '中间类', '前向缺失', 'BTC缺失', '各H缺失'], rows)
        table('其他输入与描述剔除计数', ['币', '币池整点', '缺OI', '缺σ', '无条件前向缺失', '急跌前向缺失'],
              [[symbol, row['pool_hours'], row['missing_oi_hours'], row['missing_sigma_hours'],
                row['unconditional_forward_missing'], row['all_sharp']['forward_missing']]
               for symbol, row in result['exclusions'].items()])
    check = result['data_check']
    table('数据检查', ['指标', '值'], [(k, check[k]) for k in
          ('passed', 'structural_failure', 'compared', 'mismatches', 'mismatch_fraction')])
    table('每币 metrics 覆盖与日线比对', ['币', '预期日', '有文件日', '有效整点OI', '整点覆盖率', '<280行日', '非288行日', '完整比对日', '偏差日', '不完整K日'],
          [[symbol, row['metrics']['expected_days'], row['metrics']['present_days'],
            row['metrics']['valid_hourly_oi'], row['metrics']['hourly_coverage'],
            len(row['metrics']['days_below_280']), len(row['metrics']['non_288_days']),
            row['daily']['compared'], row['daily']['mismatches'], row['daily']['incomplete_days']]
           for symbol, row in check['symbols'].items()])
    table('metrics异常或缺失日', ['币', '类型', 'UTC日/行数'],
          [[symbol, kind, f'{day}:{n}' if kind == 'non_288_days' else day]
           for symbol, row in check['symbols'].items()
           for kind in ('absent_days', 'days_below_280', 'non_288_days')
           for day, n in (row['metrics'][kind].items() if isinstance(row['metrics'][kind], dict)
                          else ((d, None) for d in row['metrics'][kind]))])
    table('日收盘偏差明细', ['币', 'UTC日', '相对误差'],
          [[symbol, row['day'], row['relative_error']] for symbol, data in check['symbols'].items()
           for row in data['daily']['mismatch_days']])
    table('下载404（不补造）', ['币', '种类', '日期/月', 'URL'],
          [[row['symbol'], row['kind'], row['period'], row['url']] for row in check['missing']])
    if check['errors']:
        lines += ['## 数据错误', ''] + ['- ' + error for error in check['errors']] + ['']
    lines += ['## 实现口径与局限', ''] + ['- ' + note for note in CHOICES]
    lines += ['', 'OI下降仅为被迫平仓代理；成交价插针、下架币覆盖、历史样本已看过与多重检验等局限见冻结预注册。',
              'events.jsonl包含A主参数去重后且前向完整的C/D事件，以及B未去重C事件；敏感性网格不参与判定。', '']
    return '\n'.join(lines)


def run_study(options, trades):
    check_prereg()  # Must precede plan/raw reads and output creation, even for direct callers.
    root = Path(options.data_dir).resolve()
    out = Path(options.out).resolve() if options.out is not None else root / 's0_v1'
    # Refuse even an empty existing output directory. Never overwrite frozen inputs.
    if out.exists():
        raise ValueError(f'输出已存在，拒绝覆盖: {out}；请用--out指定新目录')
    protected = [root / 'raw', Path(options.daily_dir).resolve(), Path(options.trades).resolve(), PREREG.resolve()]
    if out == root or any(out.is_relative_to(p) or p.is_relative_to(out) for p in protected):
        raise ValueError('输出不可覆盖输入或其父目录')
    plan = load_plan(root)
    if plan['b_symbols'] != sorted({t['symbol'] for t in trades} | {'BTCUSDT'}):
        raise ValueError('plan的B币池与冻结交易不符')
    check = inspect_data(root, plan, options.daily_dir)
    result = dict(version=1, prereg_sha256=PREREG_SHA256, trades_sha256=TRADES_SHA256,
                  git_head=subprocess.check_output(['git', '-C', str(REPO), 'rev-parse', 'HEAD'], text=True).strip(),
                  study_sha256=file_sha256(__file__), plan_sha256=file_sha256(root / 'plan.json'),
                  manifest_sha256=file_sha256(root / 'manifest.json'), data_check=check,
                  parameters=dict(k=MAIN_K, m=MAIN_M, horizons=HORIZONS, sigma_hours=168,
                                  sigma_min_valid=150, sigma_ddof=1, dedupe_hours=24, cost=.0016),
                  implementation_choices=CHOICES, allow_data_check_fail=options.allow_data_check_fail,
                  valid=check['passed'], mechanism_holds=False, s1_eligible=False, verdict='invalid')
    out.mkdir(parents=True, exist_ok=False)
    part = out / 'events.jsonl.tmp'
    with part.open('wb') as stream:
        if check['passed'] or (options.allow_data_check_fail and not check['structural_failure']):
            result.update(analyze(root, plan, trades, stream))
            if not check['passed']:
                result['diagnostic_verdict'] = result['verdict']
                result.update(verdict='invalid', mechanism_holds=False, s1_eligible=False)
                result['B']['s1_eligible'] = False
    part.replace(out / 'events.jsonl')
    write_json(out / 'results.json', result)
    report_part = out / 'report.md.tmp'
    report_part.write_text(markdown_report(json_safe(result)), encoding='utf-8')
    report_part.replace(out / 'report.md')
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    for command in ('plan', 'fetch', 'run'):
        sub = commands.add_parser(command)
        sub.add_argument('--data-dir', type=Path, default=DATA / 'liquidation_cascade')
        sub.add_argument('--trades', type=Path, default=DATA / 'llm_trim_offline/export_v1/trades.jsonl')
        if command != 'fetch':
            sub.add_argument('--daily-dir', type=Path, default=DATA / 'factor_mining/crypto_full/daily')
        if command == 'fetch':
            sub.add_argument('--workers', type=int, default=8)
            sub.add_argument('--proxy', default=DEFAULT_PROXY, help='空字符串禁用代理')
        if command == 'run':
            sub.add_argument('--out', type=Path)
            sub.add_argument('--allow-data-check-fail', action='store_true', help='仅日线比对诊断；结果始终invalid')
    args = parser.parse_args(argv)
    try:
        check_prereg()
        trades = load_trades(args.trades)
        if args.command == 'plan':
            plan = make_plan(args.daily_dir, trades)
            write_json(args.data_dir / 'plan.json', plan)
            print(json.dumps(plan['counts'], ensure_ascii=False))
            return 0
        if args.command == 'fetch':
            manifest = fetch_plan(args.data_dir, load_plan(args.data_dir), workers=args.workers, proxy=args.proxy)
            return 1 if manifest['errors'] else 0
        result = run_study(args, trades)
        print(f"VERDICT {result['verdict']}")
        return 0 if result['valid'] else 1
    except (OSError, ValueError, KeyError, TypeError, zipfile.BadZipFile,
            pd.errors.ParserError, subprocess.CalledProcessError) as error:
        parser.exit(1, f'liquidation S0: {error}\n')


if __name__ == '__main__':
    raise SystemExit(main())
