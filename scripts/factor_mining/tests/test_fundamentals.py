"""Synthetic-only tests of SEC availability, derivation, and unit conversions."""
import copy
from datetime import date, timedelta
import gzip
import json

import numpy as np
import pandas as pd
import pytest

from scripts.factor_mining.fundamentals import (
    BANK_REVENUE_CONCEPT, CONCEPTS, MAX_STALENESS_DAYS,
    PREV_YEAR_TOLERANCE_DAYS, STALE_ITEMS, Fact, build_panel, coverage_report, derive_quarters,
    duration_months, earnings_effective_day, extract_facts, load_submissions,
    save_panel, ttm,
)


CIK = '0000000001'
ENTRIES = {'TEST': {'cik': CIK, 'groups': ['sp500', 'bstock90']}}


def fact(value, end, filed, start=None, form='10-Q', accn=None):
    row = dict(val=value, end=end, filed=filed, form=form,
               accn=accn or f'{filed}-{end}', fy=2099, fp='FY')
    if start:
        row['start'] = start
    return row


def company(items):
    data = {'cik': 1, 'facts': {}}
    for concept, rows in items.items():
        ns, tag = concept.split(':')
        unit = 'shares' if 'SharesOutstanding' in tag else 'USD/shares' if 'EarningsPerShare' in tag else 'USD'
        data['facts'].setdefault(ns, {})[tag] = {'units': {unit: rows}}
    return data


def write_gzip(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    with gzip.open(path, 'wt') as stream:
        json.dump(data, stream)


def inputs(tmp_path, data, start='2018-01-01', end='2022-12-30', splits=(), close=25):
    raw, prices = tmp_path / 'raw', tmp_path / 'prices'
    prices.mkdir(exist_ok=True)
    calendar = pd.bdate_range(start, end)
    rows = [[d.strftime('%Y-%m-%d'), close, 999999, 0] for d in calendar]
    for ticker in ('SPY', 'TEST'):
        (prices / f'{ticker}.json').write_text(json.dumps({'rows': rows, 'splits': list(splits)}))
    write_gzip(raw / 'companyfacts' / f'CIK{CIK}.json.gz', data)
    return raw, prices, calendar


def build(tmp_path, data, start='2018-01-01', end='2022-12-30', **kwargs):
    raw, prices, _ = inputs(tmp_path, data, start, end, **kwargs)
    return build_panel(raw, prices, ENTRIES, start, end)


def value(panel, item, day):
    return panel[item].loc[day, 'TEST']


def test_filed_day_is_unavailable_and_weekend_waits_for_next_session(tmp_path):
    data = company({'us-gaap:Assets': [fact(100, '2020-03-31', '2020-05-01'),
                                     fact(150, '2020-06-30', '2020-08-01')]})
    p = build(tmp_path, data, '2020-04-30', '2020-08-04')
    assert np.isnan(value(p, 'assets', '2020-05-01'))
    assert value(p, 'assets', '2020-05-04') == 100
    assert value(p, 'assets', '2020-07-31') == 100
    assert value(p, 'assets', '2020-08-03') == 150


def test_restatement_only_replaces_from_its_own_usable_date(tmp_path):
    p = build(tmp_path, company({'us-gaap:Assets': [
        fact(100, '2020-03-31', '2020-05-01'),
        fact(120, '2020-03-31', '2020-05-08', form='10-Q/A')]}), '2020-05-01', '2020-05-12')
    assert value(p, 'assets', '2020-05-08') == 100
    assert value(p, 'assets', '2020-05-11') == 120
    assert list(p.selected_facts.value) == [100, 120]


def test_concept_fallback_is_period_specific_and_pit_recorded(tmp_path):
    first, second, third = CONCEPTS['revenue'][:3]
    p = build(tmp_path, company({
        third: [fact(10, '2020-03-31', '2020-04-30', '2020-01-01')],
        second: [fact(11, '2020-03-31', '2020-05-01', '2020-01-01'),
                 fact(20, '2020-06-30', '2020-07-30', '2020-04-01')],
        first: [fact(12, '2020-03-31', '2020-05-04', '2020-01-01')],
    }), '2020-04-29', '2020-08-03')
    logs = p.quarters[p.quarters.period_end == '2020-03-31']
    assert list(logs.concept) == [third, second, first]
    assert list(logs.usable_date) == ['2020-05-01', '2020-05-04', '2020-05-05']
    assert list(logs.value) == [10, 11, 12]
    assert p.quarters.iloc[-1].concept == second


def test_latest_filing_within_preferred_concept_and_same_day_tie(tmp_path):
    first, second = CONCEPTS['equity']
    p = build(tmp_path, company({first: [fact(10, '2020-03-31', '2020-05-01'),
                                       fact(12, '2020-03-31', '2020-05-05')],
                                second: [fact(99, '2020-03-31', '2020-05-06')]}),
              '2020-05-01', '2020-05-08')
    assert value(p, 'equity', '2020-05-06') == 12
    assert value(p, 'equity', '2020-05-08') == 12
    assert set(p.selected_facts.concept) == {first}


def test_ytd_quarters_q4_and_component_max_with_noncalendar_fiscal_year():
    calendar = pd.bdate_range('2019-01-01', '2021-12-31')
    data = company({'us-gaap:Revenues': [
        fact(10, '2019-06-30', '2019-08-01', '2019-04-01'),
        fact(30, '2019-09-30', '2019-11-01', '2019-04-01'),
        # Delayed 9m observation must also delay the Q4 derived using it.
        fact(60, '2019-12-31', '2020-06-01', '2019-04-01'),
        fact(100, '2020-03-31', '2020-05-01', '2019-04-01'),
    ]})
    facts = extract_facts(data, calendar)
    q = derive_quarters({f.key: f for f in facts}, 'revenue')
    assert [v.value for v in q.values()] == [10, 20, 30, 40]
    assert q['2020-03-31'].usable == '2020-06-02'
    assert q['2019-12-31'].usable == '2020-06-02'
    assert q['2020-03-31'].method == 'fy_minus_9m'
    assert not q['2020-03-31'].eps_q4_approx
    assert ttm(q) == 100
    # A different actual start must not be matched by the deliberately wrong fy.
    wrong = Fact('revenue', '2018-04-01', '2018-12-31', 50, 'x', '2020-02-01', '2020-02-03')
    q = derive_quarters({f.key: f for f in facts if f.end != '2019-12-31'} | {wrong.key: wrong}, 'revenue')
    assert '2020-03-31' not in q


def test_direct_quarter_precedes_ytd_and_gross_profit_fallback_updates(tmp_path):
    p = build(tmp_path, company({
        'us-gaap:Revenues': [fact(50, '2020-03-31', '2020-04-30', '2020-01-01'),
                              fact(60, '2020-03-31', '2020-05-05', '2020-01-01')],
        'us-gaap:CostOfRevenue': [fact(20, '2020-03-31', '2020-05-01', '2020-01-01')],
        'us-gaap:GrossProfit': [fact(45, '2020-03-31', '2020-05-07', '2020-01-01')],
    }), '2020-04-30', '2020-05-12')
    gp = p.quarters[p.quarters.item == 'gross_profit']
    assert list(gp.value) == [30, 40, 45]
    assert list(gp.usable_date) == ['2020-05-04', '2020-05-06', '2020-05-08']
    assert gp.iloc[0].concept == 'us-gaap:Revenues - us-gaap:CostOfRevenue'
    assert gp.iloc[-1].concept == 'us-gaap:GrossProfit'
    assert len(p.selected_facts.query("item == 'gross_profit'")) == 3


def test_direct_quarter_priority_and_eps_fy_minus_9m_fallback():
    calendar = pd.bdate_range('2019-01-01', '2021-01-01')
    facts = extract_facts(company({'us-gaap:EarningsPerShareDiluted': [
        fact(1, '2019-03-31', '2019-05-01', '2019-01-01'),
        fact(4, '2019-06-30', '2019-08-01', '2019-01-01'),
        fact(2, '2019-06-30', '2019-08-01', '2019-04-01'),
        fact(6, '2019-09-30', '2019-11-01', '2019-01-01'),
        fact(10, '2019-12-31', '2020-02-03', '2019-01-01'),
    ]}), calendar)
    q = derive_quarters({f.key: f for f in facts}, 'eps_diluted')
    assert q['2019-06-30'].value == 2  # Direct, not the 4 - 1 YTD difference.
    assert q['2019-06-30'].method == 'direct'
    q = derive_quarters({f.key: f for f in facts if f.end >= '2019-09-30'}, 'eps_diluted')
    assert q['2019-12-31'].value == 4
    assert q['2019-12-31'].method == 'fy_minus_9m'
    assert q['2019-12-31'].eps_q4_approx


def four_quarters(concept='us-gaap:Revenues'):
    return {concept: [fact(10, '2019-03-31', '2019-05-01', '2019-01-01'),
                      fact(20, '2019-06-30', '2019-08-01', '2019-04-01'),
                      fact(30, '2019-09-30', '2019-11-01', '2019-07-01'),
                      fact(40, '2019-12-31', '2020-02-03', '2019-10-01')]}


def test_bank_revenue_fallback_forms_ttm_without_derived_gross_profit(tmp_path):
    assert CONCEPTS['revenue'][-2:] == ('us-gaap:SalesRevenueGoodsNet', BANK_REVENUE_CONCEPT)
    rows = four_quarters(BANK_REVENUE_CONCEPT)
    rows.update(four_quarters('us-gaap:CostOfRevenue'))
    p = build(tmp_path, company(rows), '2019-04-01', '2020-02-05')
    assert value(p, 'ttm_revenue', '2020-02-04') == 100
    assert set(p.quarters.concept) == {BANK_REVENUE_CONCEPT}
    assert p['ttm_gross_profit'].isna().all().all()
    assert p.selected_facts.query("item == 'gross_profit'").empty
    assert 'gross_profit' in p.diagnostics[0]['missing_items']


def test_bank_fallback_and_gross_profit_exclusion_are_period_specific(tmp_path):
    goods = 'us-gaap:SalesRevenueGoodsNet'
    p = build(tmp_path, company({
        BANK_REVENUE_CONCEPT: [fact(10, '2020-03-31', '2020-04-30', '2020-01-01'),
                               fact(20, '2020-06-30', '2020-07-30', '2020-04-01')],
        goods: [fact(12, '2020-03-31', '2020-05-04', '2020-01-01')],
        'us-gaap:CostOfRevenue': [fact(3, '2020-03-31', '2020-04-30', '2020-01-01'),
                                fact(5, '2020-06-30', '2020-07-30', '2020-04-01')],
    }), '2020-04-30', '2020-08-03')
    revenue = p.quarters.query("item == 'revenue'")
    assert list(revenue.concept) == [BANK_REVENUE_CONCEPT, goods, BANK_REVENUE_CONCEPT]
    gross = p.quarters.query("item == 'gross_profit'")
    assert list(gross.value) == [9]
    assert list(gross.usable_date) == ['2020-05-05']
    assert list(gross.period_end) == ['2020-03-31']


def test_daily_staleness_keeps_200_days_suppresses_201_and_reports_cells(tmp_path):
    assert MAX_STALENESS_DAYS == 200
    rows = {}
    for tag in ('us-gaap:Revenues', 'us-gaap:GrossProfit', 'us-gaap:NetIncomeLoss',
                'us-gaap:NetCashProvidedByUsedInOperatingActivities',
                'us-gaap:EarningsPerShareDiluted'):
        rows[tag] = [fact((i + 1) * 10, end, filed, start)
                     for i, (start, end, filed) in enumerate([
                         ('2020-01-01', '2020-03-31', '2020-05-01'),
                         ('2020-04-01', '2020-06-30', '2020-08-03'),
                         ('2020-07-01', '2020-09-30', '2020-11-02'),
                         ('2020-10-01', '2020-12-31', '2021-02-03')])]
    for tag, current, previous in (('us-gaap:Assets', 250, 100),
                                   ('us-gaap:StockholdersEquity', 150, 80)):
        rows[tag] = [fact(previous, '2019-12-31', '2020-02-03'),
                     fact(current, '2020-12-31', '2021-02-03')]
    rows['dei:EntityCommonStockSharesOutstanding'] = [fact(10, '2020-12-31', '2021-02-03')]
    p = build(tmp_path, company(rows), '2021-07-19', '2021-07-20')
    expected = dict.fromkeys(('ttm_revenue', 'ttm_gross_profit', 'ttm_net_income', 'ttm_cfo', 'ttm_eps'), 100)
    expected.update(last_eps_q=40, assets=250, assets_prev_year=100, equity=150,
                    equity_prev_year=80, shares_adj=10, market_cap=250)
    for item in STALE_ITEMS:
        assert value(p, item, '2021-07-19') == expected[item], item
        assert np.isnan(value(p, item, '2021-07-20')), item
    report = coverage_report(p, ENTRIES, tmp_path / 'out')
    assert report['stale_suppression'] == {'2021': dict.fromkeys(STALE_ITEMS, 1)}
    saved = json.loads((tmp_path / 'out' / 'coverage.json').read_text())
    assert saved['stale_suppression'] == report['stale_suppression']
    assert '陈旧值置空单元格数' in (tmp_path / 'out' / 'coverage.md').read_text()


def test_fresh_filing_of_an_old_period_does_not_reset_staleness(tmp_path):
    p = build(tmp_path, company({'us-gaap:Assets': [
        fact(100, '2020-12-31', '2021-02-03'),
        fact(120, '2020-12-31', '2021-07-19', form='10-K/A')]}),
        '2021-07-19', '2021-07-20')
    assert value(p, 'assets', '2021-07-19') == 100
    assert np.isnan(value(p, 'assets', '2021-07-20'))
    assert p.selected_facts.iloc[-1].value == 120
    assert p.stale_suppression['2021']['assets'] == 1


def test_stale_suppression_counts_by_year_and_excludes_original_missing_cells(tmp_path):
    data = company({'us-gaap:Assets': [fact(100, '2020-03-31', '2020-05-01')],
                    'dei:EntityCommonStockSharesOutstanding': [fact(10, '2020-03-31', '2020-05-01')]})
    raw, prices, _ = inputs(tmp_path, data, '2020-12-30', '2021-01-04')
    # Market cap was already unavailable on this date because price is missing.
    path = prices / 'TEST.json'
    price_data = json.loads(path.read_text())
    price_data['rows'] = [r for r in price_data['rows'] if r[0] != '2021-01-01']
    path.write_text(json.dumps(price_data))
    p = build_panel(raw, prices, ENTRIES, '2020-12-30', '2021-01-04')
    for year in ('2020', '2021'):
        counts = p.stale_suppression[year]
        assert counts['assets'] == counts['shares_adj'] == 2
        assert counts['ttm_revenue'] == counts['assets_prev_year'] == 0
    assert p.stale_suppression['2020']['market_cap'] == 2
    assert p.stale_suppression['2021']['market_cap'] == 1


def test_ttm_requires_four_consecutive_known_quarters_and_gap_resets_to_nan(tmp_path):
    rows = four_quarters()
    rows['us-gaap:Revenues'].append(fact(60, '2020-06-30', '2020-08-03', '2020-04-01'))
    p = build(tmp_path, company(rows), '2019-04-01', '2020-08-05')
    assert np.isnan(value(p, 'ttm_revenue', '2020-02-03'))
    assert value(p, 'ttm_revenue', '2020-02-04') == 100
    assert np.isnan(value(p, 'ttm_revenue', '2020-08-04'))
    assert np.isnan(value(p, 'ttm_revenue', '2020-08-05'))


def test_missing_intermediate_quarter_does_not_form_ttm(tmp_path):
    rows = four_quarters()
    rows['us-gaap:Revenues'].pop(1)
    p = build(tmp_path, company(rows), '2020-02-03', '2020-02-06')
    assert p['ttm_revenue'].isna().all().all()


def test_eps_q4_approximation_and_sue_revisions_component_availability(tmp_path):
    tag = 'us-gaap:EarningsPerShareDiluted'
    rows = four_quarters(tag)
    rows[tag][-1] = fact(100, '2019-12-31', '2020-02-03', '2019-01-01', '10-K')
    rows[tag] += [fact(15, '2020-03-31', '2020-05-01', '2020-01-01'),
                  fact(12, '2019-03-31', '2020-05-05', '2019-01-01', '10-Q/A')]
    p = build(tmp_path, company(rows), '2019-04-01', '2020-05-08')
    q4 = p.quarters[p.quarters.period_end == '2019-12-31']
    assert q4.iloc[0].value == 40
    assert q4.eps_q4_approx.all()
    assert set(q4.method) == {'fy_minus_q1_q2_q3'}
    s = p.sue[p.sue.period_end == '2020-03-31']
    assert list(s.d4) == [5, 3]
    assert list(s.usable_date) == ['2020-05-04', '2020-05-06']
    assert list(s.eps_usable_date) == ['2020-05-04', '2020-05-04']
    assert not value(p, 'sue_ready', '2020-05-01')
    assert value(p, 'sue_ready', '2020-05-04')
    assert value(p, 'last_eps_q', '2020-05-04') == 15


@pytest.mark.parametrize(('accepted', 'expected'), [
    ('2020-03-06T20:59:59Z', '2020-03-06'),  # 15:59 EST, before DST starts
    ('2020-03-06T21:00:00Z', '2020-03-09'),  # 16:00 EST
    ('2020-03-07T15:00:00Z', '2020-03-09'),  # weekend
    ('2020-03-09T19:59:59Z', '2020-03-09'),  # 15:59 EDT, after DST starts
    ('2020-03-09T20:00:00Z', '2020-03-10'),  # 16:00 EDT
    ('2020-10-30T20:00:00Z', '2020-11-02'),  # 16:00 EDT, before DST ends
    ('2020-11-02T20:59:59Z', '2020-11-02'),  # 15:59 EST
    ('2020-11-02T21:00:00Z', '2020-11-03'),  # 16:00 EST
])
def test_earnings_effective_day_dst_boundary(accepted, expected):
    assert earnings_effective_day(accepted, pd.bdate_range('2020-01-01', '2020-12-31')) == expected


def test_submission_pages_dedup_items_and_daily_events(tmp_path):
    raw, prices, _ = inputs(tmp_path, company({}), '2020-03-05', '2020-03-12')
    records = dict(form=['8-K', '8-K/A', '8-K', '8-K', '10-Q'],
                   accessionNumber=['a', 'b', 'c', 'd', 'e'],
                   items=['2.02,9.01', '2.02', '12.02', '2.020', '2.02'],
                   acceptanceDateTime=['2020-03-06T20:59:00Z', '2020-03-09T20:00:00Z',
                                       '2020-03-10T13:00:00Z', '2020-03-10T13:00:00Z',
                                       '2020-03-10T13:00:00Z'])
    write_gzip(raw / 'submissions' / f'CIK{CIK}.json.gz', {'filings': {'recent': records}})
    older = copy.deepcopy(records)
    older['items'][0] = ''  # Recent metadata wins if paged files overlap.
    write_gzip(raw / 'submissions' / f'CIK{CIK}-submissions-001.json.gz', older)
    assert len(load_submissions(raw, CIK)) == 5
    p = build_panel(raw, prices, ENTRIES, '2020-03-05', '2020-03-12')
    assert list(p.events.effective_date) == ['2020-03-06', '2020-03-10']
    assert pd.isna(value(p, 'last_ead', '2020-03-05'))
    assert value(p, 'last_ead', '2020-03-09') == pd.Timestamp('2020-03-06')
    assert value(p, 'days_since_ead', '2020-03-09') == 3
    assert value(p, 'days_since_ead', '2020-03-10') == 0


def test_synthetic_four_for_one_split_consistent_market_cap(tmp_path):
    p = build(tmp_path, company({'dei:EntityCommonStockSharesOutstanding': [
        fact(100, '2020-08-20', '2020-08-21'),
        fact(400, '2020-08-31', '2020-08-31')],
        'us-gaap:CommonStockSharesOutstanding': [fact(999, '2020-08-20', '2020-08-21')]}),
        '2020-08-20', '2020-09-03', splits=[['2020-08-31', 4, 1]])
    for day in ('2020-08-24', '2020-08-28', '2020-08-31', '2020-09-01'):
        assert value(p, 'shares_adj', day) == 400
        assert value(p, 'market_cap', day) == 10000
    assert p.selected_facts.concept.str.startswith('dei:').all()
    # Even an end-truncated run must convert shares to the price's today basis.
    raw, prices = tmp_path / 'raw', tmp_path / 'prices'
    earlier = build_panel(raw, prices, ENTRIES, '2020-08-20', '2020-08-28')
    assert value(earlier, 'market_cap', '2020-08-28') == 10000


def test_prior_year_balances_use_latest_known_period_and_comparative_restatement(tmp_path):
    p = build(tmp_path, company({'us-gaap:Assets': [
        fact(80, '2019-03-30', '2019-05-01'),
        fact(100, '2020-03-28', '2020-05-01'),
        fact(85, '2019-03-30', '2020-05-05')]}), '2020-05-01', '2020-05-08')
    assert value(p, 'assets', '2020-05-06') == 100
    assert value(p, 'assets_prev_year', '2020-05-04') == 80
    assert value(p, 'assets_prev_year', '2020-05-06') == 85


@pytest.mark.parametrize('offset', [-46, -45, 45, 46])
def test_prior_year_instant_tolerance_is_inclusive_in_both_directions(tmp_path, offset):
    assert PREV_YEAR_TOLERANCE_DAYS == 45
    prior_end = (date(2019, 12, 31) + timedelta(days=offset)).isoformat()
    data = company({tag: [fact(80, prior_end, '2021-02-03'),
                          fact(100, '2020-12-31', '2021-02-03')]
                    for tag in ('us-gaap:Assets', 'us-gaap:StockholdersEquity')})
    p = build(tmp_path, data, '2021-02-03', '2021-02-05')
    for item in ('assets', 'equity'):
        actual = value(p, item + '_prev_year', '2021-02-04')
        assert value(p, item, '2021-02-04') == 100
        if abs(offset) <= 45:
            assert actual == 80
        else:
            assert np.isnan(actual)


def test_prior_year_instant_is_closest_to_calendar_year_including_leap_day(tmp_path):
    p = build(tmp_path, company({'us-gaap:Assets': [
        fact(70, '2019-02-20', '2020-03-02'),
        fact(80, '2019-02-28', '2020-03-02'),
        fact(90, '2019-03-01', '2020-03-02'),
        fact(100, '2020-02-29', '2020-03-02')]}), '2020-03-02', '2020-03-04')
    assert value(p, 'assets_prev_year', '2020-03-03') == 80


@pytest.mark.parametrize('year, recent', [(2015, False), (2016, True)])
def test_recent_only_revenue_report_includes_tickers_even_with_complete_ttm(tmp_path, year, recent):
    rows = four_quarters()
    for row in rows['us-gaap:Revenues']:
        for key in ('start', 'end', 'filed'):
            row[key] = (pd.Timestamp(row[key]) + pd.DateOffset(years=year - 2019)).strftime('%Y-%m-%d')
    p = build(tmp_path, company(rows), f'{year}-01-01', f'{year + 1}-03-01')
    assert p['ttm_revenue']['TEST'].iloc[-1] == 100
    report = coverage_report(p, ENTRIES, tmp_path / 'out')
    diagnostic = next(row for row in report['uncovered'] if row['ticker'] == 'TEST')
    assert ('recent-only revenue concept' in diagnostic['reasons']) == recent
    assert diagnostic['first_revenue_period'] == f'{year}-03-31'
    if recent:
        assert '收入科目仅近期存在（recent-only concept）' in (tmp_path / 'out' / 'coverage.md').read_text()


def test_units_forms_and_duration_boundaries():
    data = company({'us-gaap:Assets': [fact(100, '2020-03-31', '2020-05-01', form='20-F'),
                                     fact(120, '2020-03-31', '2020-05-01', form='10-KT')]})
    data['facts']['us-gaap']['Assets']['units']['EUR'] = [fact(999, '2020-03-31', '2020-05-01')]
    facts = extract_facts(data, pd.bdate_range('2020-01-01', '2020-06-01'))
    assert [f.value for f in facts] == [120]
    for days, months in [(79, None), (80, 3), (100, 3), (101, None), (170, 6),
                         (190, 6), (260, 9), (285, 9), (350, 12), (380, 12), (381, None)]:
        end = (date(2020, 1, 1) + timedelta(days=days-1)).isoformat()
        assert duration_months('2020-01-01', end) == months


def test_no_lookahead_any_future_fact_changes_leave_row_t_unchanged(tmp_path):
    rows = four_quarters()
    for tag in ('us-gaap:GrossProfit', 'us-gaap:NetIncomeLoss',
                'us-gaap:NetCashProvidedByUsedInOperatingActivities',
                'us-gaap:EarningsPerShareDiluted'):
        rows.update(four_quarters(tag))
    rows.update({'us-gaap:Assets': [fact(100, '2019-12-31', '2020-02-03')],
                 'us-gaap:StockholdersEquity': [fact(60, '2019-12-31', '2020-02-03')],
                 'dei:EntityCommonStockSharesOutstanding': [fact(40, '2020-01-31', '2020-02-03')]})
    # Include future restatements of old periods as well as future period ends.
    for tag in list(rows):
        future = copy.deepcopy(rows[tag][-1])
        future.update(filed='2020-03-02', accn='future', val=999)
        rows[tag].append(future)
    data = company(rows)
    raw, prices, _ = inputs(tmp_path, data, '2019-01-01', '2020-04-01')
    baseline = build_panel(raw, prices, ENTRIES, '2020-02-01', '2020-04-01')
    cutoff = build_panel(raw, prices, ENTRIES, '2020-02-01', '2020-02-28')
    for name in baseline:
        pd.testing.assert_series_equal(baseline[name].loc['2020-02-28'], cutoff[name].loc['2020-02-28'])
    for ns in data['facts'].values():
        for concept in ns.values():
            for facts in concept['units'].values():
                for row in facts:
                    if row['filed'] > '2020-02-28':
                        row['val'] *= -1000000
    write_gzip(raw / 'companyfacts' / f'CIK{CIK}.json.gz', data)
    changed = build_panel(raw, prices, ENTRIES, '2020-02-01', '2020-04-01')
    for name in baseline:
        pd.testing.assert_frame_equal(baseline[name].loc[:'2020-02-28'], changed[name].loc[:'2020-02-28'])
    assert value(baseline, 'assets', '2020-03-03') != value(changed, 'assets', '2020-03-03')


def test_foreign_filer_missing_file_reporting_and_pickle_roundtrip(tmp_path):
    data = company({'us-gaap:Assets': [fact(100, '2019-12-31', '2020-02-03', form='20-F')]})
    raw, prices, _ = inputs(tmp_path, data, '2020-01-01', '2020-03-01')
    entries = dict(ENTRIES, ABSENT={'cik': 2, 'groups': ['sp500']})
    p = build_panel(raw, prices, entries, '2020-01-01', '2020-03-01')
    assert p['assets'].isna().all().all()
    assert p.diagnostics[0]['reasons'] == ['foreign filer']
    assert 'no companyfacts' in p.diagnostics[1]['reasons']
    out = tmp_path / 'out'
    save_panel(p, out)
    for name, frame in p.items():
        pd.testing.assert_frame_equal(frame, pd.read_pickle(out / f'{name}.pkl.gz'))
    report = coverage_report(p, entries, out)
    assert report['groups']['sp500'][10]['coverage']['assets'] == 0
    assert report['groups']['sp500'][10]['earnings_events'] == 0
    assert (out / 'coverage.md').read_text().startswith('# 美国基本面')


def test_list_tickers_resolves_adjacent_manifest(tmp_path):
    raw, prices, _ = inputs(tmp_path, company({}), '2020-01-01', '2020-01-03')
    (tmp_path / 'manifest.json').write_text(json.dumps({'tickers': ENTRIES}))
    p = build_panel(raw, prices, ['TEST'], '2020-01-01', '2020-01-03')
    assert p.metadata['tickers'] == 1
    assert p.diagnostics[0]['cik'] == CIK
