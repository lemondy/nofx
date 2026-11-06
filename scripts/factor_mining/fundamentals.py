"""Offline SEC fundamentals, versioned by the first session strictly after filing.

No returns, adjusted-close fields, ranks, or factor statistics are read/computed.
``build_panel`` accepts a ticker iterable (resolved using the adjacent manifest)
or a mapping of ticker -> {cik, groups}. Its dict subclass also exposes audit
tables: selected_facts, quarters, sue, events, diagnostics, and metadata.
Daily values expire by period end; stale_suppression counts removed cells.

Shares and prices use today's split basis: each cover-page share count is
multiplied by ALL split ratios strictly after its own end date. The provided
split-adjusted close can then be used directly. This basis is a unit conversion,
not a future fundamental observation; the products cancel in market cap.
"""
from __future__ import annotations

import argparse
from collections import defaultdict
from dataclasses import dataclass
from datetime import date, timedelta
from functools import lru_cache
import gzip
import json
from pathlib import Path
import re

import numpy as np
import pandas as pd


FORMS = frozenset(('10-Q', '10-K', '10-Q/A', '10-K/A', '10-KT'))
MAX_STALENESS_DAYS = 200
PREV_YEAR_TOLERANCE_DAYS = 45
BANK_REVENUE_CONCEPT = 'us-gaap:RevenuesNetOfInterestExpense'
CONCEPTS = {
    'revenue': ('us-gaap:RevenueFromContractWithCustomerExcludingAssessedTax',
                'us-gaap:Revenues', 'us-gaap:SalesRevenueNet',
                'us-gaap:RevenueFromContractWithCustomerIncludingAssessedTax',
                'us-gaap:SalesRevenueGoodsNet', BANK_REVENUE_CONCEPT),
    'cost_of_revenue': ('us-gaap:CostOfRevenue', 'us-gaap:CostOfGoodsAndServicesSold',
                        'us-gaap:CostOfGoodsSold'),
    'gross_profit': ('us-gaap:GrossProfit',),
    'net_income': ('us-gaap:NetIncomeLoss', 'us-gaap:ProfitLoss',
                   'us-gaap:NetIncomeLossAvailableToCommonStockholdersBasic'),
    'cfo': ('us-gaap:NetCashProvidedByUsedInOperatingActivities',
            'us-gaap:NetCashProvidedByUsedInOperatingActivitiesContinuingOperations'),
    'assets': ('us-gaap:Assets',),
    'equity': ('us-gaap:StockholdersEquity',
               'us-gaap:StockholdersEquityIncludingPortionAttributableToNoncontrollingInterest'),
    'eps_diluted': ('us-gaap:EarningsPerShareDiluted',
                     'us-gaap:EarningsPerShareBasicAndDiluted', 'us-gaap:EarningsPerShareBasic'),
    'shares': ('dei:EntityCommonStockSharesOutstanding', 'us-gaap:CommonStockSharesOutstanding'),
}
FLOW = ('revenue', 'gross_profit', 'net_income', 'cfo', 'eps_diluted')
BALANCE = ('assets', 'equity', 'shares')
TTM_NAMES = dict(zip(FLOW, ('ttm_revenue', 'ttm_gross_profit', 'ttm_net_income',
                           'ttm_cfo', 'ttm_eps')))
STALE_ITEMS = (*TTM_NAMES.values(), 'last_eps_q', 'assets', 'assets_prev_year',
               'equity', 'equity_prev_year', 'shares_adj', 'market_cap')
PANEL_NAMES = (*TTM_NAMES.values(), 'assets', 'assets_prev_year', 'equity',
               'equity_prev_year', 'shares_adj', 'market_cap', 'last_eps_q',
               'sue_ready', 'last_ead', 'days_since_ead')
FACT_COLUMNS = ('ticker', 'item', 'start', 'period_end', 'usable_date', 'value',
                'concept', 'filed', 'accn', 'form', 'components')
QUARTER_COLUMNS = ('ticker', 'item', 'start', 'period_end', 'usable_date', 'value',
                   'concept', 'method', 'eps_q4_approx', 'components')
SUE_COLUMNS = ('ticker', 'period_end', 'usable_date', 'eps_q', 'd4',
               'eps_usable_date', 'd4_usable_date', 'eps_q4_approx', 'concept')
EVENT_COLUMNS = ('ticker', 'accession', 'form', 'accepted_utc', 'accepted_et',
                 'effective_date')


class FundamentalPanel(dict):
    """Wide frames with independently persisted provenance and SUE input tables."""


@lru_cache(maxsize=32768)
def _day(value: str) -> date:
    return date.fromisoformat(value)


def duration_months(start: str, end: str) -> int | None:
    days = (_day(end) - _day(start)).days + 1
    for months, lo, hi in ((3, 80, 100), (6, 170, 190), (9, 260, 285), (12, 350, 380)):
        if lo <= days <= hi:
            return months
    return None


def _read_json(path: Path):
    if path.suffix == '.gz':
        with gzip.open(path, 'rt', encoding='utf-8') as stream:
            return json.load(stream)
    return json.loads(path.read_text(encoding='utf-8'))


def next_session(calendar: pd.DatetimeIndex, day, *, strict=True) -> str | None:
    pos = calendar.searchsorted(pd.Timestamp(day), side='right' if strict else 'left')
    return calendar[pos].strftime('%Y-%m-%d') if pos < len(calendar) else None


@dataclass(frozen=True)
class Fact:
    item: str
    start: str | None
    end: str
    value: float
    concept: str
    filed: str
    usable: str
    accn: str = ''
    form: str = ''
    components: tuple = ()

    @property
    def key(self):
        return self.start, self.end


@dataclass(frozen=True)
class Quarter:
    start: str
    end: str
    value: float
    usable: str
    concept: str
    method: str
    eps_q4_approx: bool
    components: tuple


def extract_facts(company: dict, calendar: pd.DatetimeIndex) -> list[Fact]:
    """Keep only requested units/forms and dates; do not pre-collapse revisions."""
    result = []
    usable_cache = {}
    for item, concepts in CONCEPTS.items():
        unit = 'shares' if item == 'shares' else 'USD/shares' if item == 'eps_diluted' else 'USD'
        for concept in concepts:
            ns, tag = concept.split(':')
            rows = company.get('facts', {}).get(ns, {}).get(tag, {}).get('units', {}).get(unit, [])
            for row in rows:
                if row.get('form') not in FORMS:
                    continue
                try:
                    end, filed = row['end'], row['filed']
                    _day(end)
                    _day(filed)
                    if end > filed:
                        continue
                    start = row.get('start')
                    if item in BALANCE:
                        if start:
                            continue
                        start = None
                    elif not start or duration_months(start, end) is None:
                        continue
                    value = float(row['val'])
                    if not np.isfinite(value) or isinstance(row['val'], bool):
                        continue
                    if filed not in usable_cache:
                        usable_cache[filed] = next_session(calendar, filed)
                    usable = usable_cache[filed]
                    if usable is None:
                        continue
                    result.append(Fact(item, start, end, value, concept, filed, usable,
                                       row.get('accn', ''), row['form']))
                except (KeyError, TypeError, ValueError, OverflowError):
                    continue
    # For an ambiguous exact same-filing duplicate, the last source row wins.
    # Stable sorting preserves this deterministic companyfacts source order.
    return sorted(result, key=lambda f: (f.usable, f.filed, f.accn))


def _source(f: Fact) -> tuple:
    return f.concept, f.start, f.end, f.filed, f.usable, f.accn, f.components


def _gross_profit(selected: dict) -> dict:
    result = {k: f for k, f in selected.get('gross_profit', {}).items() if not f.components}
    for key, revenue in selected.get('revenue', {}).items():
        cost = selected.get('cost_of_revenue', {}).get(key)
        if (key not in result and cost is not None
                and revenue.concept != BANK_REVENUE_CONCEPT):
            result[key] = Fact('gross_profit', revenue.start, revenue.end,
                               revenue.value - cost.value,
                               f'{revenue.concept} - {cost.concept}',
                               max(revenue.filed, cost.filed), max(revenue.usable, cost.usable),
                               components=(_source(revenue), _source(cost)))
    return result


def _quarter_rank(q: Quarter):
    return q.usable, q.components, q.start


def derive_quarters(facts: dict, item: str) -> dict[str, Quarter]:
    """Derive from PIT-selected durations, matched by fiscal start, never ``fy``.

    Direct quarters take precedence. EPS Q4 uses FY minus the three known
    discrete quarters when possible; FY minus 9m is the fallback. Both EPS Q4
    derivations are flagged because diluted weighted-average denominators vary.
    """
    quarters = {}
    durations = {(f.start, duration_months(f.start, f.end)): f
                 for f in sorted(facts.values(), key=lambda f: (f.usable, f.end, f.accn))}
    for f in facts.values():
        if duration_months(f.start, f.end) == 3:
            q = Quarter(f.start, f.end, f.value, f.usable, f.concept, 'direct', False,
                        (_source(f),))
            if q.end not in quarters or _quarter_rank(q) > _quarter_rank(quarters[q.end]):
                quarters[q.end] = q
    for months, previous in ((6, 3), (9, 6), (12, 9)):
        for (start, length), f in durations.items():
            if length != months or f.end in quarters:
                continue
            prior = durations.get((start, previous))
            if prior is None:
                continue
            qstart = (_day(prior.end) + timedelta(days=1)).isoformat()
            if duration_months(qstart, f.end) != 3:
                continue
            quarters[f.end] = Quarter(qstart, f.end, f.value - prior.value,
                                      max(f.usable, prior.usable),
                                      f'{f.concept} - {prior.concept}',
                                      'fy_minus_9m' if months == 12 else 'ytd_difference',
                                      item == 'eps_diluted' and months == 12,
                                      (_source(f), _source(prior)))
    if item == 'eps_diluted':
        for (start, length), annual in durations.items():
            if length != 12 or (annual.end in quarters and quarters[annual.end].method == 'direct'):
                continue
            first_three = sorted((q for q in quarters.values()
                                  if start <= q.start and q.end < annual.end), key=lambda q: q.end)
            if (len(first_three) != 3 or first_three[0].start != start
                    or not consecutive(first_three)):
                continue
            qstart = (_day(first_three[-1].end) + timedelta(days=1)).isoformat()
            if duration_months(qstart, annual.end) != 3:
                continue
            quarters[annual.end] = Quarter(
                qstart, annual.end, annual.value - sum(q.value for q in first_three),
                max(annual.usable, *(q.usable for q in first_three)),
                annual.concept + ' - sum(Q1:Q3)', 'fy_minus_q1_q2_q3', True,
                (_source(annual), *(s for q in first_three for s in q.components)))
    return dict(sorted(quarters.items()))


def consecutive(quarters: list[Quarter]) -> bool:
    return all(_day(b.start) == _day(a.end) + timedelta(days=1)
               for a, b in zip(quarters, quarters[1:]))


def ttm(quarters: dict[str, Quarter]) -> float:
    last = sorted(quarters.values(), key=lambda q: q.end)[-4:]
    return sum(q.value for q in last) if len(last) == 4 and consecutive(last) else np.nan


def _seasonal(quarter: Quarter, quarters: dict) -> Quarter | None:
    candidates = [q for q in quarters.values()
                  if 350 <= (_day(quarter.end) - _day(q.end)).days <= 380]
    return min(candidates, key=lambda q: abs((_day(quarter.end) - _day(q.end)).days - 365),
               default=None)


def earnings_effective_day(accepted, calendar: pd.DatetimeIndex) -> str | None:
    stamp = pd.Timestamp(accepted)
    if pd.isna(stamp):
        return None
    # SEC acceptanceDateTime is UTC, including legacy strings without a suffix.
    stamp = stamp.tz_localize('UTC') if stamp.tzinfo is None else stamp.tz_convert('UTC')
    et = stamp.tz_convert('America/New_York')
    return next_session(calendar, et.date(), strict=et.hour >= 16)


def load_submissions(raw_root: Path, cik: str) -> list[dict]:
    folder = Path(raw_root) / 'submissions'
    records, seen = [], set()
    main_name = f'CIK{cik}.json.gz'
    paths = sorted(folder.glob(f'CIK{cik}*.json.gz'), key=lambda p: (p.name != main_name, p.name))
    columns = ('form', 'filingDate', 'acceptanceDateTime', 'items', 'reportDate', 'accessionNumber')
    for path in paths:
        # Do not match a different CIK prefix or an unrelated file.
        if not re.fullmatch(rf'CIK{cik}(?:-submissions-\d+)?\.json\.gz', path.name):
            continue
        data = _read_json(path)
        data = data.get('filings', {}).get('recent', data)
        for i, form in enumerate(data.get('form', [])):
            row = {k: data[k][i] for k in columns
                   if isinstance(data.get(k), list) and i < len(data[k])}
            accession = row.get('accessionNumber')
            key = accession or (form, row.get('acceptanceDateTime'), row.get('items'))
            if key not in seen:
                records.append(row)
                seen.add(key)
    return records


def earnings_events(records: list[dict], calendar: pd.DatetimeIndex, ticker='') -> list[dict]:
    events = []
    for row in records:
        if row.get('form') not in ('8-K', '8-K/A'):
            continue
        if not re.search(r'(?<![\d.])2\.02(?!\d)', str(row.get('items', ''))):
            continue
        try:
            accepted = pd.Timestamp(row['acceptanceDateTime'])
            accepted = (accepted.tz_localize('UTC') if accepted.tzinfo is None
                        else accepted.tz_convert('UTC'))
            effective = earnings_effective_day(accepted, calendar)
            if effective:
                events.append(dict(ticker=ticker, accession=row.get('accessionNumber', ''),
                                   form=row['form'], accepted_utc=accepted.isoformat(),
                                   accepted_et=accepted.tz_convert('America/New_York').isoformat(),
                                   effective_date=effective))
        except (KeyError, TypeError, ValueError):
            continue
    return sorted(events, key=lambda e: (e['effective_date'], e['accepted_utc'], e['accession']))


def _prices(path: Path) -> tuple[pd.Series, list]:
    if not path.exists():
        return pd.Series(dtype=float), []
    data = _read_json(path)
    # Only date and split-adjusted close are read. adjclose/volume are unused.
    values = {pd.Timestamp(r[0]): float(r[1]) for r in data.get('rows', [])
              if len(r) >= 2 and r[1] is not None and np.isfinite(float(r[1])) and float(r[1]) > 0}
    splits = []
    for day, numerator, denominator in data.get('splits', []):
        ratio = float(numerator) / float(denominator)
        if not np.isfinite(ratio) or ratio <= 0:
            raise ValueError(f'invalid split in {path}: {day}')
        splits.append((day, ratio))
    return pd.Series(values, dtype=float).sort_index(), sorted(splits)


def adjusted_shares(shares: Fact, splits: list) -> float:
    return shares.value * float(np.prod([ratio for day, ratio in splits if day > shares.end]))


def _ticker_entries(raw_root: Path, tickers) -> dict:
    if isinstance(tickers, dict):
        return {t: (v if isinstance(v, dict) else {'cik': v}) for t, v in tickers.items()}
    tickers = list(tickers)
    path = raw_root.parent / 'manifest.json'
    manifest = _read_json(path) if path.exists() else {}
    entries = manifest.get('tickers', manifest)
    missing = set(tickers) - entries.keys()
    if missing:
        for path in sorted((raw_root / 'submissions').glob('CIK??????????.json.gz')):
            data = _read_json(path)
            for ticker in data.get('tickers', []):
                if ticker in missing:
                    entries[ticker] = {'cik': data.get('cik', path.name[3:13])}
    return {t: entries.get(t, {}) for t in tickers}


def _fact_record(ticker, f):
    return dict(zip(FACT_COLUMNS, (ticker, f.item, f.start, f.end, f.usable, f.value,
                                  f.concept, f.filed, f.accn, f.form, f.components)))


def _quarter_record(ticker, item, q):
    return dict(zip(QUARTER_COLUMNS, (ticker, item, q.start, q.end, q.usable, q.value,
                                     q.concept, q.method, q.eps_q4_approx, q.components)))


def _instant_facts(selected, item):
    instants = selected.get(item, {})
    if not instants:
        return None, None
    latest = max(instants.values(), key=lambda f: f.end)
    target = (pd.Timestamp(latest.end) - pd.DateOffset(years=1)).date()
    prior = [f for f in instants.values()
             if abs((_day(f.end) - target).days) <= PREV_YEAR_TOLERANCE_DAYS]
    previous = min(prior, key=lambda f: (abs((_day(f.end) - target).days), f.end), default=None)
    return latest, previous


def _ticker_history(ticker, facts, splits):
    """Process filing batches chronologically; derive only from the current state."""
    state = defaultdict(dict)
    selected = defaultdict(dict)
    quarter_state = {item: {} for item in FLOW}
    snapshots, fact_log, quarter_log, sue_log = [], [], [], []
    last_sue = {}
    batches = defaultdict(list)
    for f in facts:
        batches[f.usable].append(f)
    for usable, batch in sorted(batches.items()):
        changed = set()
        for f in batch:
            state[f.item].setdefault(f.key, {})[f.concept] = f
            changed.add((f.item, f.key))
        for item, key in sorted(changed):
            options = state[item][key]
            chosen = next(options[c] for c in CONCEPTS[item] if c in options)
            if selected[item].get(key) != chosen:
                selected[item][key] = chosen
                fact_log.append(_fact_record(ticker, chosen))
        gross = _gross_profit(selected)
        # Derived gross profit is logged separately from directly selected tags.
        for key, f in gross.items():
            if f.components and selected['gross_profit'].get(key) != f:
                fact_log.append(_fact_record(ticker, f))
        selected['gross_profit'] = gross
        for item in FLOW:
            current = derive_quarters(selected[item], item)
            for end, q in current.items():
                if quarter_state[item].get(end) != q:
                    quarter_log.append(_quarter_record(ticker, item, q))
            quarter_state[item] = current
        eps = quarter_state['eps_diluted']
        for end, q in eps.items():
            prior = _seasonal(q, eps)
            d4 = q.value - prior.value if prior else None
            d4_usable = max(q.usable, prior.usable) if prior else None
            signature = (q, prior)
            if last_sue.get(end) != signature:
                sue_log.append(dict(zip(SUE_COLUMNS, (
                    ticker, end, max(q.usable, d4_usable or q.usable), q.value, d4,
                    q.usable, d4_usable, q.eps_q4_approx, q.concept))))
                last_sue[end] = signature
        values = {TTM_NAMES[item]: ttm(quarter_state[item]) for item in FLOW}
        periods = {TTM_NAMES[item]: max(quarter_state[item], default=None) for item in FLOW}
        for item in ('assets', 'equity'):
            latest, previous = _instant_facts(selected, item)
            values[item] = latest.value if latest else np.nan
            values[item + '_prev_year'] = previous.value if previous else np.nan
            # The historical comparator expires with its latest-period anchor.
            periods[item] = periods[item + '_prev_year'] = latest.end if latest else None
        shares = max(selected['shares'].values(), key=lambda f: f.end, default=None)
        values['shares_adj'] = adjusted_shares(shares, splits) if shares else np.nan
        periods['shares_adj'] = shares.end if shares else None
        latest_eps = max(eps.values(), key=lambda q: q.end, default=None)
        values['last_eps_q'] = latest_eps.value if latest_eps else np.nan
        periods['last_eps_q'] = latest_eps.end if latest_eps else None
        values['sue_ready'] = bool(latest_eps and _seasonal(latest_eps, eps))
        snapshots.append((pd.Timestamp(usable), values, periods))
    return snapshots, fact_log, quarter_log, sue_log


def build_panel(raw_root, prices_root, tickers, start='2009-06-01', end=None) -> dict[str, pd.DataFrame]:
    """Build daily SPY-session frames and PIT audit tables without network access.

    Concept priority is evaluated for each exact (start,end) at each information
    date. Within each concept, the latest filing wins. A preferred concept that
    first appears later cannot replace the fallback before its own usable date.
    ``sue_ready`` means the latest known EPS quarter has a known seasonal d4;
    the separate study must determine its standardization history requirement.
    ``days_since_ead`` counts calendar days. Flows and instants expire when the
    latest period end is more than MAX_STALENESS_DAYS before each signal date.
    Prior-year balances are nearest to latest end minus one calendar year within
    PREV_YEAR_TOLERANCE_DAYS, and expire with the latest-period anchor.
    """
    raw_root, prices_root = Path(raw_root), Path(prices_root)
    entries = _ticker_entries(raw_root, tickers)
    benchmark, _ = _prices(prices_root / 'SPY.json')
    calendar = pd.DatetimeIndex(benchmark.index, name='date')
    if calendar.empty:
        raise ValueError('SPY.json must contain a nonempty trading calendar')
    stop = min(pd.Timestamp(end), calendar[-1]) if end else calendar[-1]
    calendar = calendar[calendar <= stop]
    index = calendar[calendar >= pd.Timestamp(start)]
    if index.empty:
        raise ValueError('requested date range has no SPY sessions')
    panel = FundamentalPanel()
    for name in PANEL_NAMES:
        dtype = 'datetime64[ns]' if name == 'last_ead' else bool if name == 'sue_ready' else float
        fill = pd.NaT if name == 'last_ead' else False if name == 'sue_ready' else np.nan
        panel[name] = pd.DataFrame(fill, index=index, columns=list(entries), dtype=dtype)
    fact_tables, quarter_tables, sue_tables, event_tables, diagnostics = [], [], [], [], []
    stale_suppression = {str(year): dict.fromkeys(STALE_ITEMS, 0)
                         for year in sorted(set(index.year))}
    for ticker, entry in entries.items():
        cik = str(entry.get('cik', '')).removeprefix('CIK').zfill(10)
        path = raw_root / 'companyfacts' / f'CIK{cik}.json.gz'
        company = _read_json(path) if path.exists() else {}
        submissions = load_submissions(raw_root, cik)
        facts = extract_facts(company, calendar)
        forms = {r.get('form') for r in submissions}
        fact_forms = {r.get('form') for ns in company.get('facts', {}).values()
                      for concept in ns.values() for rows in concept.get('units', {}).values()
                      for r in rows}
        foreign = not (forms | fact_forms) & FORMS and bool(
            (forms | fact_forms) & {'20-F', '20-F/A', '40-F', '40-F/A', '6-K', '6-K/A'})
        # Foreign-only filers produce no fundamental or earnings-event panel data.
        if foreign:
            facts = []
        close, splits = _prices(prices_root / f'{ticker}.json')
        snapshots, fact_log, quarter_log, sue_log = _ticker_history(ticker, facts, splits)
        if snapshots:
            snapshot_dates = [d for d, _, _ in snapshots]
            frame = pd.DataFrame([v for _, v, _ in snapshots], index=snapshot_dates)
            periods = pd.DataFrame([p for _, _, p in snapshots], index=snapshot_dates)
            # Reindex by the previous snapshot, preserving explicit NaNs in it.
            # Cellwise ffill would hide a newly revealed gap in the latest TTM.
            frame = frame.reindex(index, method='ffill')
            periods = periods.reindex(index, method='ffill')
            frame['market_cap'] = frame['shares_adj'] * close.reindex(index)
            periods['market_cap'] = periods['shares_adj']
            for name in STALE_ITEMS:
                ages = (index.to_series() - pd.to_datetime(periods[name])).dt.days
                stale = ages > MAX_STALENESS_DAYS
                suppressed = frame[name].notna() & stale
                for year, count in suppressed.groupby(index.year).sum().items():
                    stale_suppression[str(year)][name] += int(count)
                frame[name] = frame[name].mask(stale)
            for name in frame:
                panel[name][ticker] = (frame[name].eq(True) if name == 'sue_ready'
                                       else frame[name])
        events = [] if foreign else earnings_events(submissions, calendar, ticker)
        if events:
            effective = pd.DatetimeIndex(sorted({e['effective_date'] for e in events}))
            series = pd.Series(effective, index=effective)
            last = series.reindex(series.index.union(index)).sort_index().ffill().reindex(index)
            panel['last_ead'][ticker] = last
            panel['days_since_ead'][ticker] = (index.to_series() - last).dt.days.astype(float)
        available = {f.item for f in facts}
        if any(row['item'] == 'gross_profit' for row in fact_log):
            available.add('gross_profit')
        missing = [i for i in (*FLOW, *BALANCE) if i not in available]
        missing_latest = [i for i in (*TTM_NAMES.values(), 'assets', 'equity', 'shares_adj', 'market_cap')
                          if pd.isna(panel[i][ticker].iloc[-1])]
        latest_periods = {item: period for item, period in snapshots[-1][2].items() if period} if snapshots else {}
        if 'shares_adj' in latest_periods:
            latest_periods['market_cap'] = latest_periods['shares_adj']
        stale_items = {item: period for item, period in latest_periods.items()
                       if (_day(index[-1].strftime('%Y-%m-%d')) - _day(period)).days > MAX_STALENESS_DAYS}
        first_revenue_period = min((f.end for f in facts if f.item == 'revenue'), default=None)
        reasons = []
        if foreign:
            reasons.append('foreign filer')
        if not path.exists():
            reasons.append('no companyfacts')
        if (missing or missing_latest) and not foreign:
            reasons.append('missing items')
        if close.empty:
            reasons.append('no prices')
        if first_revenue_period and first_revenue_period > '2015-12-31':
            reasons.append('recent-only revenue concept')
        diagnostics.append(dict(ticker=ticker, cik=cik, groups=entry.get('groups', []),
                                reasons=reasons, missing_items=missing,
                                missing_latest=missing_latest, latest_periods=latest_periods,
                                stale_items=stale_items, first_revenue_period=first_revenue_period,
                                forms=sorted(f for f in forms | fact_forms if f),
                                price_observations=int(close.reindex(index).notna().sum())))
        fact_tables.append(pd.DataFrame(fact_log, columns=FACT_COLUMNS))
        quarter_tables.append(pd.DataFrame(quarter_log, columns=QUARTER_COLUMNS))
        sue_tables.append(pd.DataFrame(sue_log, columns=SUE_COLUMNS).astype({'eps_q': float, 'd4': float}))
        event_tables.append(pd.DataFrame(events, columns=EVENT_COLUMNS))
    for attr, tables, columns in (('selected_facts', fact_tables, FACT_COLUMNS),
                                  ('quarters', quarter_tables, QUARTER_COLUMNS),
                                  ('sue', sue_tables, SUE_COLUMNS),
                                  ('events', event_tables, EVENT_COLUMNS)):
        setattr(panel, attr, pd.concat([t for t in tables if not t.empty], ignore_index=True)
                if any(not t.empty for t in tables) else pd.DataFrame(columns=columns))
    panel.diagnostics = diagnostics
    panel.stale_suppression = stale_suppression
    panel.metadata = dict(start=index[0].strftime('%Y-%m-%d'), end=index[-1].strftime('%Y-%m-%d'),
                          sessions=len(index), tickers=len(entries),
                          split_basis='today', availability='first SPY session strictly after filed',
                          sue_ready='latest known EPS quarter has a known seasonal d4',
                          days_since_ead='calendar days',
                          max_staleness_days=MAX_STALENESS_DAYS,
                          prev_year_tolerance_days=PREV_YEAR_TOLERANCE_DAYS,
                          prev_year='nearest latest end minus one calendar year; expires with latest end',
                          eps_q4='FY minus sum(Q1:Q3), else FY minus 9m; approximation flagged',
                          concept_counts='selected duration/instant filing versions, not daily fills',
                          ambiguous_duplicates='same concept/period/filed/accession: last raw row wins')
    return panel


def save_panel(panel: dict[str, pd.DataFrame], out) -> None:
    """Standard-library/pandas pickle+gzip only; no parquet engine required."""
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    for name, frame in panel.items():
        frame.to_pickle(out / f'{name}.pkl.gz', compression='gzip')
    for attr in ('selected_facts', 'quarters', 'sue', 'events'):
        if hasattr(panel, attr):
            getattr(panel, attr).to_pickle(out / f'{attr}.pkl.gz', compression='gzip')
    (out / 'metadata.json').write_text(json.dumps(getattr(panel, 'metadata', {}), indent=2) + '\n')


def coverage_report(panel, entries, out) -> dict:
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    metrics = ('ttm_revenue', 'ttm_net_income', 'assets', 'equity', 'market_cap')
    groups = {}
    for group in ('sp500', 'bstock90'):
        members = [t for t, v in entries.items() if group in v.get('groups', [])]
        years = []
        for year in range(2010, 2027):
            dates = panel['assets'].index[panel['assets'].index.year == year]
            fractions, any_day = {}, {}
            mean_daily = {}
            for metric in metrics:
                valid = panel[metric].loc[dates, members].notna()
                fractions[metric] = float(valid.iloc[-1].mean()) if len(dates) and members else None
                any_day[metric] = float(valid.any(axis=0).mean()) if len(dates) and members else None
                mean_daily[metric] = float(valid.to_numpy().mean()) if len(dates) and members else None
            events = panel.events
            count = int(((events.ticker.isin(members)) &
                         events.effective_date.str.startswith(str(year))).sum())
            years.append(dict(year=year, tickers=len(members), sessions=len(dates),
                              coverage=fractions, any_day_coverage=any_day,
                              mean_daily_coverage=mean_daily, earnings_events=count))
        groups[group] = years
    counts = {}
    for item, frame in panel.selected_facts.groupby('item'):
        counts[item] = {str(k): int(v) for k, v in frame.concept.value_counts().items()}
    spots = []
    for ticker in ('AAPL', 'MSFT', 'JPM', 'XOM', 'NVDA'):
        if ticker not in panel['assets']:
            continue
        row = dict(ticker=ticker, date=panel.metadata['end'])
        for item in ('ttm_revenue', 'ttm_net_income', 'equity', 'shares_adj', 'market_cap', 'last_ead'):
            value = panel[item][ticker].iloc[-1]
            row[item] = (None if pd.isna(value) else value.strftime('%Y-%m-%d')
                         if isinstance(value, pd.Timestamp) else float(value))
        spots.append(row)
    report = dict(metadata=panel.metadata, groups=groups, concept_usage=counts,
                  stale_suppression=panel.stale_suppression,
                  uncovered=[d for d in panel.diagnostics if d['reasons']],
                  stale=[d for d in panel.diagnostics if d['stale_items']],
                  spot_checks=spots)
    (out / 'coverage.json').write_text(json.dumps(report, ensure_ascii=False, indent=2, allow_nan=False) + '\n')
    lines = ['# 美国基本面 PIT 数据覆盖报告', '',
             f"面板区间：{panel.metadata['start']} 至 {panel.metadata['end']}；"
             f"{panel.metadata['sessions']} 个 SPY 交易日，{panel.metadata['tickers']} 只股票。", '',
             '仅构建数据层；未读取或计算远期收益、IC、因子排名或表现。', '',
             '年度覆盖率定义：固定组内当年最后一个已有交易日该项非空的股票占比；'
             'JSON 另存日均及当年至少一日非空的覆盖率。2026 年仅统计已有交易日。事件数按生效交易日计数，'
             '8-K/A 单独计事件，同一申报文件跨分页去重。当前名单存在幸存者偏差。', '',
             '可用日：提交日之后第一个 SPY 交易日；重述从自身可用日起替换。'
             '份额及价格均为今日拆股口径；份额按封面日期之后全部拆股换算。'
             'EPS Q4 优先年度减前三个离散季度，其次年度减九个月累计，均标记近似。', '',
             'sue_ready 仅表示最新 EPS 季度有已知同比季度差，不执行标准化；'
             f'days_since_ead 为自然日。TTM、最新季度 EPS 及资产/权益/份额的最新期间距信号日超过 '
             f'{MAX_STALENESS_DAYS} 个自然日即置空，市值随份额置空。'
             f'同比资产/权益选择距“最新期间减一年”最近且在 ±{PREV_YEAR_TOLERANCE_DAYS} 天内的时点，'
             '并随最新期间过期。银行净利息收入后营收科目不用于派生毛利。', '']
    for group, years in groups.items():
        lines += [f'## {group}（固定 {years[0]["tickers"]} 只）', '',
                  '| 年份 | 营收 TTM | 净利 TTM | 资产 | 权益 | 市值 | 财报事件数 |',
                  '|---|---:|---:|---:|---:|---:|---:|']
        for row in years:
            values = ['—' if row['coverage'][m] is None else f'{row["coverage"][m]:.1%}' for m in metrics]
            lines.append(f'| {row["year"]} | ' + ' | '.join(values) + f' | {row["earnings_events"]} |')
        lines.append('')
    lines += ['## 最新日期核查（金额为美元，份额为股）', '',
              '| 股票 | 营收 TTM | 净利 TTM | 权益 | 拆股调整份额 | 市值 | 最新财报事件 |',
              '|---|---:|---:|---:|---:|---:|---|']
    for row in spots:
        values = ['—' if row[k] is None else f'{row[k]:,.2f}' for k in
                  ('ttm_revenue', 'ttm_net_income', 'equity', 'shares_adj', 'market_cap')]
        lines.append(f'| {row["ticker"]} | ' + ' | '.join(values) + f' | {row["last_ead"] or "—"} |')
    lines += ['', f'上述核查值的最新已知季度截止日（超过 {MAX_STALENESS_DAYS} 个自然日已置空）：']
    for row in spots:
        diagnostic = next(d for d in panel.diagnostics if d['ticker'] == row['ticker'])
        periods = diagnostic['latest_periods']
        lines.append(f'- {row["ticker"]}：营收 {periods.get("ttm_revenue", "无")}；'
                     f'净利 {periods.get("ttm_net_income", "无")}。')
    lines += ['', '## 科目使用次数', '',
              '按可用日实际选中的期间/时点申报版本计数，包含比较期间及重述；'
              '不是每日前填次数。派生毛利单独记录其组成科目。', '', '| 项目 | 科目 | 次数 |', '|---|---|---:|']
    for item, concepts in counts.items():
        for concept, count in concepts.items():
            lines.append(f'| {item} | {concept} | {count} |')
    lines += ['', '## 未覆盖或部分缺项的股票', '',
              '同时列出首个可用收入事实的期间截止日晚于 2015 年的股票（recent-only concept）；'
              '近期有季度不代表已有连续四季或可推导的年度值。', '',
              '| 股票 | 原因 | 缺失项目 | 首个营收期间 |', '|---|---|---|---|']
    translations = {'foreign filer': '外国发行人', 'no companyfacts': '无 companyfacts 文件',
                    'missing items': '缺少科目', 'no prices': '无价格文件',
                    'recent-only revenue concept': '收入科目仅近期存在（recent-only concept）'}
    for row in report['uncovered']:
        lines.append(f'| {row["ticker"]} | ' + '、'.join(translations[r] for r in row['reasons']) +
                     ' | ' + (', '.join(sorted(set(row['missing_items'] + row['missing_latest']))) or '—') +
                     f' | {row["first_revenue_period"] or "—"} |')
    lines += ['', '## 陈旧值置空单元格数', '',
              '按全部股票去重、按信号日年份和面板项目计数；只计原本非空、因超过 '
              f'{MAX_STALENESS_DAYS} 天被置空的日度单元格。原本缺失或无法构成 TTM 的单元格不计入；'
              '市值仅在有价格时计数，同比资产/权益按最新期间的年龄计数。', '',
              '| 年份 | ' + ' | '.join(STALE_ITEMS) + ' |',
              '|---|' + '---:|' * len(STALE_ITEMS)]
    for year, items in panel.stale_suppression.items():
        lines.append(f'| {year} | ' + ' | '.join(str(items[item]) for item in STALE_ITEMS) + ' |')
    lines += ['', '## 陈旧期间提示', '',
              f'以下项目的最新已知期间距面板末日超过 {MAX_STALENESS_DAYS} 个自然日，'
              '对应面板值已置空；季度及申报来源审计表保留历史记录。同比项列出其最新期间锚点。', '',
              '| 股票 | 项目及最新期间 |', '|---|---|']
    for row in report['stale']:
        lines.append(f'| {row["ticker"]} | ' + '；'.join(f'{k}: {v}' for k, v in row['stale_items'].items()) + ' |')
    lines += ['', '## 文件与审计', '',
              '各宽表保存为 `<项目>.pkl.gz`，通过 `pandas.read_pickle` 读取。'
              '`selected_facts.pkl.gz` 保存科目选择版本和来源；`quarters.pkl.gz` 保存季度版本、'
              '推导方法与 EPS Q4 近似标记；`sue.pkl.gz` 保存季度 EPS 和同比差的 PIT 版本；'
              '`events.pkl.gz` 保存合并分页后的 UTC/美东接受时间及生效日。', '',
              'SUE 表同一股票/季度可以有多个可用日版本；截至 t 时按 usable_date ≤ t 选择最后一行。'
              'EPS 本身的可用日另存 eps_usable_date，同比差的可用日为两季组成项的最大值。', '',
              '同一科目、期间、提交日和文件号出现冲突值时按原文件最后一行确定性选择。'
              '不插值、不补财报；陈旧值按上述规则置空，精确日期不连续的季度不组成 TTM。', '']
    (out / 'coverage.md').write_text('\n'.join(lines), encoding='utf-8')
    return report


def main():
    parser = argparse.ArgumentParser(description='离线构建 SEC PIT 基本面数据层')
    parser.add_argument('--raw', type=Path, required=True)
    parser.add_argument('--prices', type=Path, required=True)
    parser.add_argument('--manifest', type=Path, required=True)
    parser.add_argument('--out', type=Path, default=Path('.fund_out'))
    parser.add_argument('--start', default='2009-06-01')
    parser.add_argument('--end')
    args = parser.parse_args()
    manifest = _read_json(args.manifest)
    entries = manifest.get('tickers', manifest)
    panel = build_panel(args.raw, args.prices, entries, args.start, args.end)
    save_panel(panel, args.out)
    report = coverage_report(panel, entries, args.out)
    print(f'已保存 {len(panel)} 项宽表；{panel.metadata["tickers"]} 只股票，'
          f'{panel.metadata["sessions"]} 个交易日；目录 {args.out}')
    print(pd.DataFrame(report['spot_checks']).to_string(index=False))


if __name__ == '__main__':
    main()
