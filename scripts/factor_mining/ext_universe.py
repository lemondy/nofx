"""Best-effort crypto scoring universe; never feeds the mining data loader.

All writes are confined to <root>/crypto/ext, except the additive live snapshot
published by live_scores.extend_live_scores. HTTP callers can inject get-only
objects; production clients also bound request timeouts/retries to the deadline.
"""
from collections import Counter
import datetime as dt
import json
import os
from pathlib import Path
import sqlite3
import sys
import time

from . import data_fetch, options, store
from .config import MARKETS

FETCH_BUDGET_S = 15 * 60
DEFAULT_DB = data_fetch.DATA / 'data.db'


def _log(message):
    print(f'crypto ext: {message}', file=sys.stderr)


def _utc(now=None):
    now = now if now is not None else dt.datetime.now(dt.timezone.utc)
    return (now.replace(tzinfo=dt.timezone.utc) if now.tzinfo is None
            else now.astimezone(dt.timezone.utc))


def valid_symbol(symbol):
    return (isinstance(symbol, str) and symbol.endswith('USDT') and
            all(c.isalnum() or c == '_' for c in symbol))


def read_extra_symbols(root=data_fetch.ROOT, log=_log):
    try:
        value = store.read_json(Path(root) / 'crypto/ext/extra_symbols.json')
        if isinstance(value, dict) and isinstance(value.get('symbols'), list):
            if all(valid_symbol(s) for s in value['symbols']):
                return value
    except Exception as exc:
        log(f'cannot read cached universe: {type(exc).__name__}: {exc}')
    return {'symbols': []}


def _frequencies(db_path, now, *, deadline=None, clock=time.monotonic):
    """Read only the two narrow columns, using a SQLite read-only URI."""
    uri = Path(db_path).expanduser().resolve().as_uri() + '?mode=ro'
    connection = sqlite3.connect(uri, uri=True, timeout=2)
    try:
        if deadline is not None:
            connection.set_progress_handler(lambda: int(clock() >= deadline), 1000)
        cutoff = now - dt.timedelta(days=14)
        counts = Counter()
        rows = connection.execute(
            'SELECT candidate_coins FROM decision_records '
            # The coarse text bound uses the timestamp index; the exact bound
            # still handles ISO offsets at either end of the 14-day interval.
            'WHERE timestamp >= ? AND julianday(timestamp) >= julianday(?) '
            'AND julianday(timestamp) <= julianday(?)',
            ((cutoff - dt.timedelta(days=1)).date().isoformat(), cutoff.isoformat(), now.isoformat()))
        for (raw,) in rows:
            try:
                coins = json.loads(raw)
            except (ValueError, TypeError):
                continue
            if isinstance(coins, list):
                counts.update(s.strip().upper() for s in coins if isinstance(s, str))
        rows = connection.execute(
            'SELECT symbol, COUNT(*) FROM trader_orders '
            'WHERE created_at >= ? AND created_at <= ? GROUP BY symbol',
            (int(cutoff.timestamp() * 1000), int(now.timestamp() * 1000)))
        for symbol, count in rows:
            if isinstance(symbol, str):
                counts[symbol.strip().upper()] += count
        return counts
    finally:
        connection.close()


def _filter_reason(symbol, info, core, now_ms, min_history):
    if not valid_symbol(symbol):
        return 'invalid_symbol'
    if symbol in core:
        return 'core'
    if symbol in data_fetch.CRYPTO_EXCLUDED:
        return 'excluded'
    if not isinstance(info, dict):
        return 'not_listed'
    if info.get('status') != 'TRADING':
        return 'not_trading'
    if info.get('contractType') != 'PERPETUAL':
        return 'not_perpetual'
    if info.get('quoteAsset') != 'USDT':
        return 'not_usdt'
    # Missing type is not proof of a cryptocurrency; fail closed. A missing
    # subtype is common for COIN contracts and safely means no TRADFI tag.
    if info.get('underlyingType') != 'COIN':
        return 'underlying_type'
    subtype = info.get('underlyingSubType')
    subtype = [] if subtype is None else subtype
    subtype = [subtype] if isinstance(subtype, str) else subtype
    if not isinstance(subtype, (list, tuple)):
        return 'underlying_subtype'
    if any('TRADFI' in str(tag).upper() for tag in subtype):
        return 'tradfi'
    try:
        onboard = int(info['onboardDate'])
        if onboard <= 0:
            return 'onboard_date'
        if now_ms - onboard < min_history * data_fetch.DAY_MS:
            return f'history<{min_history}d'
    except (KeyError, TypeError, ValueError, OverflowError):
        return 'onboard_date'
    return None


class _BudgetHttp:
    """Check every pagination request, including injected get-only clients."""
    def __init__(self, http, deadline, clock):
        self.http, self.deadline, self.clock = http, deadline, clock

    def get(self, url, params=None):
        self.check()
        result = self.http.get(url, params)
        self.check()
        return result

    def check(self):
        if self.clock() >= self.deadline:
            raise TimeoutError('fetch deadline exceeded')


def refresh_extra_symbols(root=data_fetch.ROOT, *, http=None, db_path=None, now=None,
                          limit=80, deadline=None, clock=time.monotonic, log=_log):
    """Rank recent candidate/order occurrences; on ANY failure retain the cache.

    filtered counts distinct rejected symbols (rather than their occurrences).
    Ties at K resolve alphabetically for deterministic output.
    """
    previous = read_extra_symbols(root, log)
    try:
        now = _utc(now)
        deadline = deadline if deadline is not None else clock() + FETCH_BUDGET_S
        counts = _frequencies(db_path or os.environ.get('NOFX_DB') or DEFAULT_DB, now,
                              deadline=deadline, clock=clock)
        client = _BudgetHttp(http if http is not None else data_fetch.Http(
            deadline=deadline, clock=clock), deadline, clock)
        info = client.get(data_fetch.FAPI + '/fapi/v1/exchangeInfo') if counts else {'symbols': []}
        if not isinstance(info, dict) or not isinstance(info.get('symbols'), list):
            raise ValueError('invalid exchangeInfo')
        listed = {s['symbol']: s for s in info['symbols']
                  if isinstance(s, dict) and isinstance(s.get('symbol'), str)}
        if counts and not listed:
            raise ValueError('empty exchangeInfo')
        core = {p.stem for p in (Path(root) / 'crypto/daily').glob('*.json')}
        filtered, kept = Counter(), []
        for symbol, _ in sorted(counts.items(), key=lambda item: (-item[1], item[0])):
            reason = _filter_reason(symbol, listed.get(symbol), core, int(now.timestamp() * 1000),
                                    MARKETS['crypto']['min_history'])
            if reason:
                filtered[reason] += 1
            elif len(kept) < limit:
                kept.append(symbol)
            else:
                filtered['limit'] += 1
        output = dict(generated_at=now.isoformat(), lookback_days=14, limit=limit,
                      symbols=kept, frequencies={s: counts[s] for s in kept}, filtered=dict(filtered))
        store.write_json(Path(root) / 'crypto/ext/extra_symbols.json', output)
        log(f'universe: {len(kept)} selected, {sum(filtered.values())} filtered')
        return output
    except Exception as exc:
        log(f'universe refresh skipped, retaining cache: {type(exc).__name__}: {exc}')
        return previous


def fetch_extra_data(symbols, root=data_fetch.ROOT, *, http=None, alt_http=None, now_ms=None,
                     deadline=None, clock=time.monotonic, log=_log):
    """Incremental OHLCV then funding/premium, all exclusively under crypto/ext.

    Every symbol/field and metadata publication is best effort. Daily data is
    fetched first, so slow alt history cannot starve remaining daily symbols.
    """
    deadline = deadline if deadline is not None else clock() + FETCH_BUDGET_S
    now_ms = int(time.time() * 1000) if now_ms is None else now_ms
    daily = _BudgetHttp(http if http is not None else data_fetch.Http(
        deadline=deadline, clock=clock), deadline, clock)
    alt = _BudgetHttp(alt_http if alt_http is not None else (
        http if http is not None else data_fetch.Http(spacing=0.8, deadline=deadline, clock=clock)),
        deadline, clock)
    base = Path(root) / 'crypto/ext'
    symbols = list(dict.fromkeys(symbols))
    result = dict(updated_at=_utc().isoformat(), ok=0, failed={},
                  alt=dict(ok=0, failed={}), timed_out=False)

    def refresh(path, fetcher, sym, client):
        client.check()
        old = data_fetch.read_symbol(path)
        start = (data_fetch._day_ms(old[-1][0]) - 3 * data_fetch.DAY_MS if old else
                 int(data_fetch.CRYPTO_START.timestamp() * 1000))
        new = fetcher(client, sym, start, now_ms)
        if not new and not old:
            raise ValueError('no data')
        if new:
            store.write_json(path, data_fetch.merge(old, new))

    for sym in symbols:
        try:
            if not valid_symbol(sym) or sym in data_fetch.CRYPTO_EXCLUDED:
                raise ValueError('excluded or invalid symbol')
            if (Path(root) / 'crypto/daily' / f'{sym}.json').exists():
                raise ValueError('core symbol')
            refresh(base / 'daily' / f'{sym}.json', data_fetch.crypto_rows, sym, daily)
            result['ok'] += 1
        except Exception as exc:
            result['failed'][sym] = f'{type(exc).__name__}: {exc}'[:160]
            result['timed_out'] |= clock() >= deadline
    for sym in symbols:
        if sym in result['failed']:
            continue
        for field, fetcher in (('funding', data_fetch.funding_rows), ('premium', data_fetch.premium_rows)):
            try:
                refresh(base / 'alt_daily' / field / f'{sym}.json', fetcher, sym, alt)
                result['alt']['ok'] += 1
            except Exception as exc:
                result['alt']['failed'][f'{field}/{sym}'] = f'{type(exc).__name__}: {exc}'[:160]
                result['timed_out'] |= clock() >= deadline
    try:
        store.write_json(base / 'data_meta.json', result)
    except Exception as exc:
        log(f'data metadata write skipped: {type(exc).__name__}: {exc}')
    log(f'data: {result}')
    return result


def run_extension(root=data_fetch.ROOT, *, fetch=True, http=None, alt_http=None, db_path=None,
                  now=None, not_before=None, clock=time.monotonic, log=_log):
    """Post-mining hook. A disabled flag avoids even reading extension artifacts."""
    if os.environ.get('FM_EXT_UNIVERSE', '1') == '0':
        return None
    try:
        from .live_scores import extend_live_scores
        from .panel import load_panels

        now = _utc(now)
        deadline = clock() + FETCH_BUDGET_S
        universe = read_extra_symbols(root, log)
        if fetch:
            universe = refresh_extra_symbols(root, http=http, db_path=db_path, now=now,
                                             deadline=deadline, clock=clock, log=log)
            try:
                fetch_extra_data(universe['symbols'], root, http=http, alt_http=alt_http,
                                 now_ms=int(now.timestamp() * 1000), deadline=deadline, clock=clock, log=log)
            except Exception as exc:
                log(f'data refresh skipped: {type(exc).__name__}: {exc}')
        zoo = store.read_json(Path(root) / 'crypto/zoo.json')
        selected_ids = None
        if options.enabled(options.GREEDY_ENSEMBLE):
            ensemble = store.read_json(Path(root) / 'crypto/ensemble.json')
            selected_ids = {int(h): v['factor_ids'] for h, v in ensemble['horizons'].items() if v}
        return extend_live_scores(zoo['factors'], load_panels('crypto', root), universe['symbols'],
                                  root=root, selected_ids=selected_ids, now=now,
                                  not_before=not_before, log=log)
    except Exception as exc:
        log(f'extension skipped: {type(exc).__name__}: {exc}')
        return None
