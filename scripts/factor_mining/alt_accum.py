"""Accumulate short-history Binance futures statistics, one daily row per day.

openInterestHist, globalLongShortAccountRatio, topLongShortPositionRatio,
topLongShortAccountRatio and takerlongshortRatio only serve ~30 days upstream
(measured 2026-10: a request whose startTime reaches back >30 days is rejected
with an HTTP error, and an unbounded request returns the latest 30 daily rows),
far short of the evaluator's 1095/365/180-day windows and its MIN_COVERAGE=0.3.
They are therefore NOT DSL fields; this module just keeps a growing local copy.

Layout (``accum_dir`` is a parameter; data_fetch defaults it to ``<root>/alt``):
``<accum_dir>/<kind>/<SYMBOL>.json`` = list of dict rows ascending by ``date``:
``{'date': 'YYYY-MM-DD', 'timestamp': <source ms>, <numeric fields as float>}``.
``date`` is the UTC date of the 1d bucket's source timestamp; the current,
possibly unfinished day is never stored. A bucket is only fully known at the end
of its date, so any future field built from these rows must be shifted/aligned
as "known after close of ``date``" (the same rule as funding/premium).
"""
import datetime as dt
from pathlib import Path

from .data_fetch import FAPI, _num, date_of, read_symbol, write_symbol

KINDS = {
    'oi': '/futures/data/openInterestHist',
    'glsr': '/futures/data/globalLongShortAccountRatio',
    'toplsp': '/futures/data/topLongShortPositionRatio',
    'topls_acct': '/futures/data/topLongShortAccountRatio',
    'taker': '/futures/data/takerlongshortRatio',
}
UPSTREAM_DAYS = 30


def fetch_rows(http, kind, symbol, now_ms):
    """Latest ``UPSTREAM_DAYS`` 1d buckets, finished days only, as dict rows."""
    page = http.get(FAPI + KINDS[kind], {'symbol': symbol, 'period': '1d', 'limit': UPSTREAM_DAYS})
    if not isinstance(page, list):
        raise ValueError(f'{kind}/{symbol}: expected array')
    today = date_of(now_ms)
    rows = []
    for raw in page:
        try:
            ts = int(raw['timestamp'])
        except (KeyError, TypeError, ValueError):
            continue
        row = {'date': date_of(ts), 'timestamp': ts}
        for key, value in raw.items():
            if key not in ('symbol', 'timestamp', 'pair', 'contractType'):
                number = _num(value)
                if number is not None:
                    row[key] = number
        if row['date'] < today and len(row) > 2:
            rows.append(row)
    return rows


def merge_rows(old, new):
    by_date = {r['date']: r for r in old}
    by_date.update((r['date'], r) for r in new)
    return [by_date[d] for d in sorted(by_date)]


def accumulate(symbols, http, now_ms, accum_dir, kinds=None, log=print):
    """Append the latest buckets for each kind/symbol; failures are isolated."""
    ok, failed = 0, {}
    for kind in kinds or KINDS:
        for sym in symbols:
            path = Path(accum_dir) / kind / f'{sym}.json'
            try:
                new = fetch_rows(http, kind, sym, now_ms)
                if new:
                    write_symbol(path, merge_rows(read_symbol(path), new))
                ok += 1
            except Exception as exc:
                failed[f'{kind}/{sym}'] = f'{type(exc).__name__}: {exc}'[:160]
    log(f'crypto accum: updated {ok}/{len(symbols) * len(kinds or KINDS)}, failed {len(failed)}')
    return {'ok': ok, 'failed': failed}


def read_accum(accum_dir, kind, symbol):
    """Stored rows for one kind/symbol (empty list if none)."""
    rows = read_symbol(Path(accum_dir) / kind / f'{symbol}.json')
    return [r for r in rows if isinstance(r, dict) and 'date' in r]


def days_accumulated(accum_dir, kind, symbol):
    rows = read_accum(accum_dir, kind, symbol)
    if not rows:
        return 0
    first = dt.date.fromisoformat(rows[0]['date'])
    last = dt.date.fromisoformat(rows[-1]['date'])
    return (last - first).days + 1
