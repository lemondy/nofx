"""Research-only full USDT-M perp universe, including delisted coins (never feeds live mining).

Source: data.binance.vision monthly archives (1d klines and fundingRate) for every
USDT-quoted coin perpetual that ever had an archive: live (exchangeInfo TRADING),
SETTLING (delisted, still in exchangeInfo) and ABSENT (removed from exchangeInfo,
e.g. LUNA, FTT). TradFi perps (equity/commodity/FX/pre-market) and index perps
(BTCDOM, DEFI, ALL) are excluded. Days missing inside a monthly archive (Binance
dropped 2026-10-28 from every October file) and, for live symbols, the current
not-yet-archived month are filled from the daily 1d archives when they exist.
Zero-volume bars (a delisted contract keeps printing flat bars while settling)
are not tradable and are dropped from ``daily/``.

Everything is written under ``<out>`` (default ``data/factor_mining/crypto_full``),
never under ``data/factor_mining/crypto``:

  raw/klines_1d/<SYM>/<SYM>-1d-YYYY-MM.zip[.CHECKSUM]   downloads, kept forever
  raw/klines_1d_daily/<SYM>/<SYM>-1d-YYYY-MM-DD.zip[.CHECKSUM]  gap fill + current month
  raw/funding/<SYM>/<SYM>-fundingRate-YYYY-MM.zip[.CHECKSUM]
  raw/premium_1d/<SYM>/<SYM>-1d-YYYY-MM.zip[.CHECKSUM]   premiumIndexKlines
  daily/<SYM>.json            rows [date, o, h, l, c, volume, vwap] (data_fetch format)
  alt_daily/funding/<SYM>.json rows [date, mean funding rate] (data_fetch.funding_daily)
  alt_daily/premium/<SYM>.json rows [date, 1d premium close] (data_fetch.premium_rows), traded days only
  manifest.json               symbol -> status, onboard/delivery dates, archive keys
  validation.json             per-symbol checksum, gap and OHLC checks

A zip is accepted only when its sha256 matches the published CHECKSUM; a verified
zip is never downloaded again or overwritten, and a mismatching download is kept
as ``*.zip.mismatch`` for inspection. Commands: ``fetch`` (list + download),
``build`` (zips -> json + validation), ``all``.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import csv
import datetime as dt
import hashlib
import io
import json
import os
from pathlib import Path
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile

from . import data_fetch

OUT = data_fetch.ROOT / 'crypto_full'
LIVE_DAILY = data_fetch.ROOT / 'crypto' / 'daily'
VISION = 'https://data.binance.vision/'
S3_LIST = 'https://s3-ap-northeast-1.amazonaws.com/data.binance.vision'
KLINE_PREFIX = 'data/futures/um/monthly/klines/'
DAILY_KLINE_PREFIX = 'data/futures/um/daily/klines/'
FUNDING_PREFIX = 'data/futures/um/monthly/fundingRate/'
PREMIUM_PREFIX = 'data/futures/um/monthly/premiumIndexKlines/'
EXCLUDED_UNDERLYING = {'INDEX', 'PREMARKET'}
WORKERS = 32
# data.binance.vision / S3 are reachable directly; the local proxy drops connections
# under 32 parallel downloads, so archives default to no proxy (FU_VISION_PROXY overrides).
VISION_PROXY = os.environ.get('FU_VISION_PROXY', '')
RETRIES = 5


def _opener(proxy=data_fetch.PROXY):
    proxies = data_fetch._proxy_list(proxy)
    if not proxies:
        return urllib.request.build_opener()
    return urllib.request.build_opener(urllib.request.ProxyHandler({'http': proxies[0], 'https': proxies[0]}))


def _get(opener, url, sleep=time.sleep):
    err = None
    for attempt in range(RETRIES):
        try:
            req = urllib.request.Request(url, headers={'User-Agent': data_fetch.UA})
            with opener.open(req, timeout=60) as resp:
                return resp.read()
        except urllib.error.HTTPError as exc:
            err = exc
            if exc.code in (403, 404):
                break
        except OSError as exc:
            err = exc
        if attempt < RETRIES - 1:
            sleep(2 ** (attempt + 1))
    raise RuntimeError(f'{url}: {err}')


def list_keys(opener, prefix, delimiter=None):
    """All keys (or, with ``delimiter``, common prefixes) under an S3 prefix."""
    out, marker = [], ''
    while True:
        params = {'prefix': prefix, 'marker': marker}
        if delimiter:
            params['delimiter'] = delimiter
        xml = _get(opener, S3_LIST + '?' + urllib.parse.urlencode(params)).decode()
        tag = 'Prefix' if delimiter else 'Key'
        found = re.findall(rf'<{tag}>({re.escape(prefix)}[^<]+)</{tag}>', xml)
        out += found
        if '<IsTruncated>true</IsTruncated>' not in xml or not found:
            return out
        nxt = re.search(r'<NextMarker>([^<]+)</NextMarker>', xml)
        marker = nxt.group(1) if nxt else found[-1]


def classify(exchange_info, vision_symbols):
    """{symbol: {status, onboard, delivery}} for USDT coin perps with an archive."""
    info = {s['symbol']: s for s in exchange_info.get('symbols', [])}
    out = {}
    for sym in sorted(set(vision_symbols)):
        if not sym.endswith('USDT'):
            continue
        s = info.get(sym)
        if s is None:
            out[sym] = dict(status='ABSENT', onboard=None, delivery=None)
            continue
        if (s.get('contractType') != 'PERPETUAL' or s.get('quoteAsset') != 'USDT' or
                s.get('underlyingType') in EXCLUDED_UNDERLYING):
            continue
        out[sym] = dict(status=s.get('status'), onboard=_date(s.get('onboardDate')),
                        delivery=_date(s.get('deliveryDate')))
    return out


def _date(ms):
    # deliveryDate of a live perp is the 2100-12-25 sentinel.
    return data_fetch.date_of(int(ms)) if ms and int(ms) < 4e12 else None


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def fetch_file(opener, key, dest):
    """Download one archive + CHECKSUM into ``dest``; returns 'cached'|'ok'|'mismatch'|error str."""
    dest = Path(dest)
    check = dest.with_name(dest.name + '.CHECKSUM')
    if dest.exists() and check.exists():
        if check.read_text().split()[0] == sha256(dest.read_bytes()):
            return 'cached'
    try:
        url = VISION + urllib.parse.quote(key)      # some symbols are CJK, e.g. 币安人生USDT
        expected_text = _get(opener, url + '.CHECKSUM').decode()
        data = _get(opener, url)
    except (RuntimeError, ValueError) as exc:
        return f'error: {exc}'[:200]
    expected = expected_text.split()[0] if expected_text.split() else ''
    dest.parent.mkdir(parents=True, exist_ok=True)
    if sha256(data) != expected:
        dest.with_name(dest.name + '.mismatch').write_bytes(data)
        return 'mismatch'
    if dest.exists():       # an unverified older copy is kept, never overwritten
        os.replace(dest, dest.with_name(dest.name + f'.prev{time.time_ns()}'))
    tmp = dest.with_name(dest.name + '.tmp')
    tmp.write_bytes(data)
    os.replace(tmp, dest)
    check.write_text(expected_text)
    return 'ok'


def fetch(out=OUT, opener=None, symbols=None, log=print, workers=WORKERS, api_opener=None):
    """``opener`` serves the archives (default: VISION_PROXY), ``api_opener`` fapi (default: FM_PROXY)."""
    opener = opener or _opener(VISION_PROXY)
    api_opener = api_opener or _opener()
    out = Path(out)
    exchange_info = json.loads(_get(api_opener, data_fetch.FAPI + '/fapi/v1/exchangeInfo'))
    vision = [p[len(KLINE_PREFIX):-1] for p in list_keys(opener, KLINE_PREFIX, '/')]
    universe = classify(exchange_info, vision)
    if symbols:
        universe = {s: v for s, v in universe.items() if s in set(symbols)}
    log(f'full universe: {len(universe)} symbols '
        f'({sum(v["status"] == "TRADING" for v in universe.values())} trading)')

    def keys_of(sym):
        k = [x for x in list_keys(opener, f'{KLINE_PREFIX}{sym}/1d/') if x.endswith('.zip')]
        f = [x for x in list_keys(opener, f'{FUNDING_PREFIX}{sym}/') if x.endswith('.zip')]
        p = [x for x in list_keys(opener, f'{PREMIUM_PREFIX}{sym}/1d/') if x.endswith('.zip')]
        return sym, k, f, p

    with ThreadPoolExecutor(workers) as pool:
        for sym, k, f, p in pool.map(keys_of, universe):
            universe[sym].update(kline_keys=k, funding_keys=f, premium_keys=p)
    jobs = [(key, out / 'raw' / 'klines_1d' / sym / key.rsplit('/', 1)[1])
            for sym, v in universe.items() for key in v['kline_keys']]
    jobs += [(key, out / 'raw' / 'funding' / sym / key.rsplit('/', 1)[1])
             for sym, v in universe.items() for key in v['funding_keys']]
    jobs += [(key, out / 'raw' / 'premium_1d' / sym / key.rsplit('/', 1)[1])
             for sym, v in universe.items() for key in v['premium_keys']]
    log(f'archives: {len(jobs)} files')
    results = {}
    with ThreadPoolExecutor(workers) as pool:
        for (key, _), res in zip(jobs, pool.map(lambda j: fetch_file(opener, *j), jobs)):
            results[key] = res
    results.update(fill_days(out, universe, opener, workers, log=log))
    counts = {}
    for res in results.values():
        bucket = res if res in ('cached', 'ok', 'mismatch', 'absent', 'fapi') else 'error'
        counts[bucket] = counts.get(bucket, 0) + 1
    manifest = dict(fetched_at=dt.datetime.now(dt.timezone.utc).isoformat(timespec='seconds'),
                    counts=counts, symbols=universe,
                    failed={k: v for k, v in results.items() if v not in ('cached', 'ok', 'absent', 'fapi')})
    out.mkdir(parents=True, exist_ok=True)
    data_fetch.write_symbol(out / 'manifest.json', manifest)
    log(f'archives: {counts}')
    return manifest


def missing_days(rows, until=None):
    """Dates absent between the first and last row (and up to ``until``, inclusive)."""
    have = {r[0] for r in rows}
    if not have:
        return []
    day, end = dt.date.fromisoformat(min(have)), dt.date.fromisoformat(max([max(have)] + ([until] if until else [])))
    out = []
    while day <= end:
        if day.isoformat() not in have:
            out.append(day.isoformat())
        day += dt.timedelta(days=1)
    return out


def fill_days(out, universe, opener, workers=WORKERS, now=None, log=print, http=None):
    """Download daily 1d archives for days the monthly files lack; 404 means a real gap.

    Live symbols whose day is in neither archive get it from fapi klines instead
    (``raw/fapi_fill/<SYM>.json``, no checksum; counted as ``fapi_filled``).
    """
    now = now or dt.datetime.now(dt.timezone.utc)
    yesterday = (now.date() - dt.timedelta(days=1)).isoformat()
    jobs = []
    for sym, meta in universe.items():
        rows = [r for p in sorted((Path(out) / 'raw' / 'klines_1d' / sym).glob('*.zip')) if _verified(p)
                for r in kline_rows(p)]
        until = yesterday if meta['status'] == 'TRADING' else None
        for day in missing_days(rows, until):
            name = f'{sym}-1d-{day}.zip'
            jobs.append((f'{DAILY_KLINE_PREFIX}{sym}/1d/{name}', Path(out) / 'raw' / 'klines_1d_daily' / sym / name))
    log(f'daily archives: {len(jobs)} candidate days')
    with ThreadPoolExecutor(workers) as pool:
        res = dict(zip((j[0] for j in jobs), pool.map(lambda j: fetch_file(opener, *j), jobs)))
    # A 404 on a daily file is an expected, genuine gap (halt or not yet archived), not a failure.
    res = {k: ('absent' if 'HTTP Error 404' in v else v) for k, v in res.items()}
    absent = {}
    for key, v in res.items():
        sym, day = key.rsplit('/', 1)[1][:-4].split('-1d-')
        if v == 'absent' and universe[sym]['status'] == 'TRADING':
            absent.setdefault(sym, []).append(day)
    http = http or (data_fetch.Http() if absent else None)
    for sym, days in sorted(absent.items()):
        path = Path(out) / 'raw' / 'fapi_fill' / f'{sym}.json'
        try:
            rows = data_fetch.crypto_rows(http, sym, data_fetch._day_ms(min(days)), int(now.timestamp() * 1000))
        except RuntimeError as exc:
            log(f'fapi fill {sym}: {exc}')
            continue
        got = [r for r in rows if r[0] in set(days)]
        if got:
            data_fetch.write_symbol(path, data_fetch.merge(data_fetch.read_symbol(path), got))
            for r in got:
                res[f'{DAILY_KLINE_PREFIX}{sym}/1d/{sym}-1d-{r[0]}.zip'] = 'fapi'
    return res



def _csv_rows(path):
    """Data rows of the single CSV in a zip; the header row (present since ~2022) is skipped."""
    with zipfile.ZipFile(path) as zf:
        names = [n for n in zf.namelist() if n.endswith('.csv')]
        if len(names) != 1:
            raise ValueError(f'{path.name}: expected one csv, got {names}')
        text = zf.read(names[0]).decode()
    rows = list(csv.reader(io.StringIO(text)))
    return [r for r in rows if r and r[0].strip().lstrip('-').isdigit()]


def kline_rows(path):
    out = []
    for k in _csv_rows(path):
        open_ms = int(k[0])
        vol, qv = data_fetch._num(k[5]), data_fetch._num(k[7])
        vwap = qv / vol if vol and qv else data_fetch._num(k[4])
        out.append([data_fetch.date_of(open_ms), data_fetch._num(k[1]), data_fetch._num(k[2]),
                    data_fetch._num(k[3]), data_fetch._num(k[4]), vol, vwap])
    return out


def funding_settlements(path):
    # columns: calc_time, funding_interval_hours, last_funding_rate
    return [(int(r[0]), data_fetch._num(r[2])) for r in _csv_rows(path)]


def _verified(path):
    check = path.with_name(path.name + '.CHECKSUM')
    return check.exists() and check.read_text().split()[0] == sha256(path.read_bytes())


def _days(a, b):
    return (dt.date.fromisoformat(b) - dt.date.fromisoformat(a)).days


def check_rows(rows):
    """Gap / duplicate / OHLC report for one symbol's raw kline rows (ascending or not)."""
    dates = [r[0] for r in rows]
    unique = sorted(set(dates))
    invalid = [r[0] for r in rows if not data_fetch._valid(r)]
    gaps = [(a, b, _days(a, b) - 1) for a, b in zip(unique, unique[1:]) if _days(a, b) > 1]
    zero_vol = sum(1 for r in rows if r[5] == 0)
    return dict(rows=len(unique), first=unique[0] if unique else None, last=unique[-1] if unique else None,
                duplicates=len(dates) - len(unique), invalid_ohlc=len(invalid), invalid_dates=invalid[:5],
                gaps=len(gaps), missing_days=sum(g[2] for g in gaps),
                longest_gap=max(gaps, key=lambda g: g[2]) if gaps else None, zero_volume_days=zero_vol)


def cross_check(rows, reference, tol=1e-6):
    """Max relative close difference vs the existing fapi daily file on shared dates."""
    ref = {r[0]: r[4] for r in reference}
    diffs = [abs(r[4] / ref[r[0]] - 1) for r in rows if r[0] in ref and ref[r[0]] and r[4]]
    return dict(shared=len(diffs), max_rel_close_diff=max(diffs) if diffs else None,
                mismatched=sum(d > tol for d in diffs))


def build(out=OUT, live_daily=LIVE_DAILY, now_ms=None, log=print):
    out = Path(out)
    manifest = json.loads((out / 'manifest.json').read_text())
    now_ms = now_ms or int(time.time() * 1000)
    today = data_fetch.date_of(now_ms)
    report = {}
    for sym, meta in sorted(manifest['symbols'].items()):
        rep = dict(status=meta['status'], checksum_failed=[])
        rows = []
        for sub in ('klines_1d', 'klines_1d_daily'):
            for path in sorted((out / 'raw' / sub / sym).glob('*.zip')):
                if not _verified(path):
                    rep['checksum_failed'].append(path.name)
                    continue
                rows += kline_rows(path)
        have = {r[0] for r in rows}
        fapi = [r for r in data_fetch.read_symbol(out / 'raw' / 'fapi_fill' / f'{sym}.json') if r[0] not in have]
        rep['fapi_filled'] = len(fapi)
        rows = [r for r in rows + fapi if r[0] < today]
        traded = [r for r in rows if r[5]]
        rep.update(check_rows(traded))        # gaps are judged on tradable days only
        rep['zero_volume_days'] = len(rows) - len(traded)
        rep['last_bar'] = max((r[0] for r in rows), default=None)
        clean = data_fetch.merge([], [r for r in traded if data_fetch._valid(r)])
        if clean:
            data_fetch.write_symbol(out / 'daily' / f'{sym}.json', clean)
        reference = data_fetch.read_symbol(Path(live_daily) / f'{sym}.json')
        if reference:
            rep['vs_live'] = cross_check(clean, reference)
        settlements = []
        for path in sorted((out / 'raw' / 'funding' / sym).glob('*.zip')):
            if not _verified(path):
                rep['checksum_failed'].append(path.name)
                continue
            settlements += funding_settlements(path)
        funding = data_fetch.funding_daily(settlements, today)
        if funding:
            data_fetch.write_symbol(out / 'alt_daily' / 'funding' / f'{sym}.json', funding)
        rep['funding_days'] = len(funding)
        traded_days = {r[0] for r in clean}
        premium = []
        for path in sorted((out / 'raw' / 'premium_1d' / sym).glob('*.zip')):
            if not _verified(path):
                rep['checksum_failed'].append(path.name)
                continue
            premium += [[r[0], r[4]] for r in kline_rows(path) if r[0] in traded_days and r[4] is not None]
        premium = data_fetch.merge([], premium)
        if premium:
            data_fetch.write_symbol(out / 'alt_daily' / 'premium' / f'{sym}.json', premium)
        rep['premium_days'] = len(premium)
        report[sym] = rep
    summary = dict(
        built_at=dt.datetime.now(dt.timezone.utc).isoformat(timespec='seconds'),
        symbols=len(report), with_klines=sum(r['rows'] > 0 for r in report.values()),
        checksum_failed=sum(len(r['checksum_failed']) for r in report.values()),
        invalid_ohlc=sum(r['invalid_ohlc'] for r in report.values()),
        duplicates=sum(r['duplicates'] for r in report.values()),
        symbols_with_gaps=sum(r['gaps'] > 0 for r in report.values()),
        missing_days=sum(r['missing_days'] for r in report.values()),
        fapi_filled_days=sum(r['fapi_filled'] for r in report.values()),
        vs_live_symbols=sum('vs_live' in r for r in report.values()),
        vs_live_mismatched=sum(r.get('vs_live', {}).get('mismatched', 0) for r in report.values()),
        by_status={s: sum(r['status'] == s for r in report.values())
                   for s in sorted({r['status'] for r in report.values()})})
    data_fetch.write_symbol(out / 'validation.json', dict(summary=summary, symbols=report))
    log(f'build: {summary}')
    return summary


def main(argv=None):
    sys.stdout.reconfigure(line_buffering=True)
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument('command', choices=('fetch', 'build', 'all'))
    p.add_argument('--out', default=str(OUT))
    p.add_argument('--symbols', nargs='*')
    p.add_argument('--workers', type=int, default=WORKERS)
    a = p.parse_args(argv)
    if Path(a.out).resolve() == (data_fetch.ROOT / 'crypto').resolve():
        p.error('refusing to write into the live mining directory')
    if a.command in ('fetch', 'all'):
        fetch(a.out, symbols=a.symbols, workers=a.workers)
    if a.command in ('build', 'all'):
        build(a.out)
    return 0


if __name__ == '__main__':
    sys.exit(main())
