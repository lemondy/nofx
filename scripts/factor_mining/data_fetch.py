"""Daily OHLCV fetcher for the factor-mining pipeline (the only networked module).

crypto: Binance USDT-M perpetual 1d klines (fapi), full history per symbol.
us:     Yahoo chart API 1d bars of the underlyings of Binance bStock pairs,
        OHLC rescaled by adjclose/close so splits and dividends do not create
        fake returns (volume is left as reported).

Each symbol is stored as ``<root>/<market>/daily/<SYMBOL>.json``: a list of
``[date, open, high, low, close, volume, vwap]`` rows (UTC date, ascending).
crypto vwap = quoteVolume/volume; us vwap = (high+low+close)/3 because Yahoo
has no true vwap. Incomplete (current-day) bars are never stored.

crypto only, side files under ``<root>/crypto/alt_daily/<field>/<SYMBOL>.json``
(rows ``[date, value]``, UTC date, completed days only, best effort):
  funding  mean of the funding-rate settlements whose fundingTime falls inside
           that UTC day (00:00/08:00/16:00 on 8h symbols). The 00:00 settlement
           of day t+1 belongs to t+1, never to t.
  premium  close of that day's 1d premiumIndexKlines bar (bar closes 23:59:59.999).
Both are fully known by the close of their own day, so a day-t value is usable
for a decision taken after close t. History: funding from listing (2019-09),
premium from ~2019-12. Open interest, long/short ratios and taker buy/sell
ratio only expose ~30 days upstream, so they are accumulated by ``alt_accum``
(opt-in) rather than exposed as fields.
"""
import argparse
import datetime as dt
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

DATA = Path('/Users/zhangyun/workspace/nofx/data')
ROOT = Path(os.environ.get('FACTOR_MINING_DIR') or DATA / 'factor_mining')
PROXY = os.environ.get('FM_PROXY', 'http://127.0.0.1:7890')
CRYPTO_EXCLUDED = {'APRUSDT', 'MANAUSDT', 'STXUSDT', 'USELESSUSDT'}
# Leveraged / inverse products are not stocks for cross-sectional purposes; SPY is the benchmark.
US_EXCLUDED = {'TQQQ', 'SQQQ', 'SOXL', 'SOXS', 'UVXY', 'QQQ', 'MUU', 'KORU'}
BENCHMARK = 'SPY'
UA = 'Mozilla/5.0 (nofx-factor-mining)'
DAY_MS = 86400000
CRYPTO_START = dt.datetime(2019, 9, 1, tzinfo=dt.timezone.utc)
FAPI = 'https://fapi.binance.com'
ALT_FIELDS = ('funding', 'premium')
FUNDING_PAGE = 1000


class Http:
    """Proxy-aware JSON GET with spacing and bounded retries; ``get`` is the test seam."""
    def __init__(self, spacing=0.4, proxy=PROXY, sleep=time.sleep):
        handlers = [urllib.request.ProxyHandler({'http': proxy, 'https': proxy})] if proxy else []
        self.opener = urllib.request.build_opener(*handlers)
        self.spacing, self.sleep, self.last = spacing, sleep, 0.0

    def get(self, url, params=None):
        if params:
            url += '?' + urllib.parse.urlencode(params)
        err = None
        for attempt in range(5):
            self.sleep(max(0.0, self.spacing - (time.monotonic() - self.last)))
            self.last = time.monotonic()
            try:
                req = urllib.request.Request(url, headers={'User-Agent': UA})
                with self.opener.open(req, timeout=30) as resp:
                    return json.load(resp)
            except urllib.error.HTTPError as exc:
                err = exc
                if exc.code in (400, 401, 403, 404):
                    break
                self.sleep(min(120, 8 * 2 ** attempt) if exc.code in (418, 429) else 2 ** (attempt + 1))
            except (OSError, ValueError) as exc:
                err = exc
                self.sleep(2 ** (attempt + 1))
        raise RuntimeError(f'{url}: {err}')


def _num(x):
    try:
        v = float(x)
    except (TypeError, ValueError):
        return None
    return v if v == v and abs(v) != float('inf') else None


def _valid(row):
    _, o, h, l, c, v, w = row
    return all(x is not None for x in (o, h, l, c, v, w)) and min(o, l, c) > 0 and h >= max(o, c) and l <= min(o, c) and v >= 0


def date_of(ms):
    return dt.datetime.fromtimestamp(ms / 1000, dt.timezone.utc).strftime('%Y-%m-%d')


def read_symbol(path):
    try:
        return json.loads(Path(path).read_text(encoding='utf-8'))
    except (FileNotFoundError, ValueError):
        return []


def write_symbol(path, rows):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix('.tmp')
    tmp.write_text(json.dumps(rows, separators=(',', ':')), encoding='utf-8')
    os.replace(tmp, path)


def merge(old, new):
    by_date = {r[0]: r for r in old}
    by_date.update((r[0], r) for r in new)
    return [by_date[d] for d in sorted(by_date)]


def crypto_rows(http, symbol, start_ms, now_ms):
    rows = []
    while start_ms < now_ms:
        page = http.get('https://fapi.binance.com/fapi/v1/klines',
                        {'symbol': symbol, 'interval': '1d', 'startTime': start_ms, 'limit': 1500})
        if not isinstance(page, list) or not page:
            break
        for k in page:
            open_ms = int(k[0])
            if open_ms + DAY_MS > now_ms:        # current day still forming
                continue
            vol, qv = _num(k[5]), _num(k[7])
            vwap = qv / vol if vol and qv else _num(k[4])
            rows.append([date_of(open_ms), _num(k[1]), _num(k[2]), _num(k[3]), _num(k[4]), vol, vwap])
        nxt = int(page[-1][0]) + DAY_MS
        if len(page) < 1500 or nxt <= start_ms:
            break
        start_ms = nxt
    return [r for r in rows if _valid(r)]


def us_rows(http, ticker, years=6):
    res = http.get(f'https://query1.finance.yahoo.com/v8/finance/chart/{urllib.parse.quote(ticker)}',
                   {'range': f'{years}y', 'interval': '1d', 'includeAdjustedClose': 'true'})
    result = (res.get('chart') or {}).get('result') or []
    if not result:
        return []
    r = result[0]
    ts = r.get('timestamp') or []
    q = ((r.get('indicators') or {}).get('quote') or [{}])[0]
    adj = (((r.get('indicators') or {}).get('adjclose') or [{}])[0]).get('adjclose') or []
    rows = []
    for i, t in enumerate(ts):
        o, h, l, c, v = (_num((q.get(k) or [None] * len(ts))[i]) for k in ('open', 'high', 'low', 'close', 'volume'))
        a = _num(adj[i]) if i < len(adj) else None
        if None in (o, h, l, c, v) or not c:
            continue
        f = a / c if a else 1.0
        o, h, l, c = o * f, h * f, l * f, c * f
        rows.append([date_of(int(t) * 1000), o, h, l, c, v, (h + l + c) / 3])
    # Yahoo's last row can be the live session; keep only dates strictly before today (UTC).
    today = dt.datetime.now(dt.timezone.utc).strftime('%Y-%m-%d')
    return [r for r in rows if r[0] < today and _valid(r)]


def funding_daily(settlements, today):
    """Daily mean funding rate per UTC date from ``(fundingTime_ms, rate)`` pairs.

    A settlement is assigned to the UTC date of its own timestamp, so the 00:00
    settlement of t+1 is never part of day t. Days >= ``today`` (YYYY-MM-DD)
    are dropped as possibly incomplete. Row: ``[date, mean_rate]``.
    """
    by_day = {}
    for ms, rate in settlements:
        if rate is not None:
            by_day.setdefault(date_of(int(ms)), []).append(rate)
    return [[d, sum(v) / len(v)] for d, v in sorted(by_day.items()) if d < today]


def funding_rows(http, symbol, start_ms, now_ms):
    settlements = []
    while start_ms < now_ms:
        page = http.get(FAPI + '/fapi/v1/fundingRate',
                        {'symbol': symbol, 'startTime': start_ms, 'endTime': now_ms, 'limit': FUNDING_PAGE})
        if not isinstance(page, list) or not page:
            break
        settlements.extend((int(r['fundingTime']), _num(r.get('fundingRate'))) for r in page)
        nxt = int(page[-1]['fundingTime']) + 1
        if len(page) < FUNDING_PAGE or nxt <= start_ms:
            break
        start_ms = nxt
    return funding_daily(settlements, date_of(now_ms))


def premium_rows(http, symbol, start_ms, now_ms):
    rows = []
    while start_ms < now_ms:
        page = http.get(FAPI + '/fapi/v1/premiumIndexKlines',
                        {'symbol': symbol, 'interval': '1d', 'startTime': start_ms, 'limit': 1500})
        if not isinstance(page, list) or not page:
            break
        for k in page:
            open_ms = int(k[0])
            value = _num(k[4])
            if open_ms + DAY_MS > now_ms or value is None:   # current day still forming
                continue
            rows.append([date_of(open_ms), value])
        nxt = int(page[-1][0]) + DAY_MS
        if len(page) < 1500 or nxt <= start_ms:
            break
        start_ms = nxt
    return rows


def _day_ms(date):
    return int(dt.datetime.strptime(date, '%Y-%m-%d').replace(tzinfo=dt.timezone.utc).timestamp() * 1000)


def update_alt(root, symbols, http, now_ms, log=print):
    """Incrementally refresh the crypto funding/premium side files; never raises.

    Returns {'ok': n_symbol_fields, 'failed': {'field/SYMBOL': reason}}.
    """
    fetchers = {'funding': funding_rows, 'premium': premium_rows}
    ok, failed = 0, {}
    for sym in symbols:
        for field in ALT_FIELDS:
            path = Path(root) / 'crypto' / 'alt_daily' / field / f'{sym}.json'
            try:
                old = read_symbol(path)
                start = _day_ms(old[-1][0]) - 3 * DAY_MS if old else int(CRYPTO_START.timestamp() * 1000)
                new = fetchers[field](http, sym, start, now_ms)
                if new:
                    write_symbol(path, merge(old, new))
                ok += 1
            except Exception as exc:    # alt data must never break the OHLCV update
                failed[f'{field}/{sym}'] = f'{type(exc).__name__}: {exc}'[:160]
    log(f'crypto alt: updated {ok}/{len(symbols) * len(ALT_FIELDS)}, failed {len(failed)}')
    return {'ok': ok, 'failed': failed}


def crypto_universe(path=DATA / 'alt_data' / 'universe_src.json'):
    syms = json.loads(Path(path).read_text(encoding='utf-8')).get('crypto', [])
    return sorted(s for s in set(syms) if s.endswith('USDT') and s not in CRYPTO_EXCLUDED)


def us_universe(http):
    info = http.get('https://api.binance.com/api/v3/exchangeInfo')
    out = []
    for s in info.get('symbols', []):
        name = s.get('symbol', '')
        if s.get('status') == 'TRADING' and name.endswith('BUSDT'):
            out.append(name[:-5])
    keep = sorted({u for u in out if u not in US_EXCLUDED and u.isalpha()} | {BENCHMARK})
    return keep


def update(market, root=ROOT, http=None, symbols=None, now_ms=None, log=print,
           alt=True, alt_http=None, accumulate=None, accum_dir=None):
    """Refresh daily OHLCV (and, for crypto, funding/premium side files).

    ``accumulate`` (default: env FM_ALT_ACCUM == '1') additionally appends the
    short-history snapshots (OI, long/short ratios, taker ratio) to ``accum_dir``
    (default ``<root>/alt``); see ``alt_accum``. Alt steps are best effort.
    """
    alt_http = alt_http or (http if http is not None else Http(spacing=0.8))
    http = http or Http()
    now_ms = now_ms or int(time.time() * 1000)
    base = Path(root) / market / 'daily'
    if symbols is None:
        symbols = crypto_universe() if market == 'crypto' else us_universe(http)
    ok, failed = 0, {}
    for sym in symbols:
        path = base / f'{sym}.json'
        old = read_symbol(path)
        try:
            if market == 'crypto':
                if old:
                    start = int(dt.datetime.strptime(old[-1][0], '%Y-%m-%d').replace(tzinfo=dt.timezone.utc).timestamp() * 1000) - 3 * DAY_MS
                else:
                    start = int(CRYPTO_START.timestamp() * 1000)
                new = crypto_rows(http, sym, start, now_ms)
            else:
                new = us_rows(http, sym)
        except RuntimeError as exc:
            failed[sym] = str(exc)[:160]
            log(f'{market} {sym}: FAIL {failed[sym]}')
            continue
        if not new and not old:
            failed[sym] = 'no data'
            continue
        # US adjusted history changes after dividends/splits, so replace it wholesale.
        rows = new if market == 'us' and new else merge(old, new)
        write_symbol(path, rows)
        ok += 1
    extra = {}
    if market == 'crypto' and alt:
        alt_symbols = [s for s in symbols if s not in failed]
        try:
            extra['alt'] = update_alt(root, alt_symbols, alt_http, now_ms, log)
        except Exception as exc:
            extra['alt'] = {'ok': 0, 'failed': {'*': f'{type(exc).__name__}: {exc}'[:160]}}
        if accumulate is None:
            accumulate = os.environ.get('FM_ALT_ACCUM') == '1'
        if accumulate:
            try:
                from . import alt_accum
                extra['accum'] = alt_accum.accumulate(alt_symbols, alt_http, now_ms,
                                                      accum_dir or Path(root) / 'alt', log=log)
            except Exception as exc:
                extra['accum'] = {'ok': 0, 'failed': {'*': f'{type(exc).__name__}: {exc}'[:160]}}
    stamp = dt.datetime.now(dt.timezone.utc).isoformat(timespec='seconds')
    meta = Path(root) / market / 'data_meta.json'
    meta.parent.mkdir(parents=True, exist_ok=True)
    meta.write_text(json.dumps({'updated_at': stamp, 'ok': ok, 'failed': failed, **extra}, ensure_ascii=False),
                    encoding='utf-8')
    log(f'{market}: updated {ok}/{len(symbols)}, failed {len(failed)}')
    return {'ok': ok, 'failed': failed}


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument('--market', choices=('crypto', 'us', 'all'), default='all')
    p.add_argument('--symbols', nargs='*')
    p.add_argument('--root', default=str(ROOT))
    p.add_argument('--no-alt', action='store_true', help='skip crypto funding/premium side files')
    p.add_argument('--accumulate', action='store_true', help='also append OI/long-short/taker daily snapshots')
    p.add_argument('--accum-dir', default=None, help='default: <root>/alt')
    a = p.parse_args(argv)
    for m in (('crypto', 'us') if a.market == 'all' else (a.market,)):
        update(m, root=a.root, symbols=a.symbols or None, alt=not a.no_alt,
               accumulate=a.accumulate or None, accum_dir=a.accum_dir)
    return 0


if __name__ == '__main__':
    sys.exit(main())
