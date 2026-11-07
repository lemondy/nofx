"""Synthetic-only checks for the frozen fundamental-factor study."""
import copy
import gzip
import hashlib
import json
import os

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import evolution_ledger
from scripts.factor_mining import evaluate, fundamentals
from scripts.factor_mining import us_fundamental_factor_study as s


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value))


def write_gzip(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    with gzip.open(path, 'wt') as stream:
        json.dump(value, stream)


def empty_panel(dates, columns):
    p = fundamentals.FundamentalPanel()
    defaults = dict(ttm_gross_profit=40., ttm_net_income=12., ttm_revenue=100.,
                    ttm_cfo=16., assets=200., assets_prev_year=160.,
                    equity=80., equity_prev_year=40., market_cap=400.)
    for key, value in defaults.items():
        p[key] = pd.DataFrame(value, index=dates, columns=columns)
    p.sue = pd.DataFrame(columns=fundamentals.SUE_COLUMNS)
    p.events = pd.DataFrame(columns=fundamentals.EVENT_COLUMNS)
    return p


def test_all_nine_formulas_and_fixed_directions():
    dates = pd.bdate_range('2025-01-01', periods=8)
    panel = empty_panel(dates, ['A'])
    adj = pd.DataFrame({'A': [100, 102, 105, 108, 110, 111, 112, 113]}, index=dates, dtype=float)
    spy = pd.Series([100, 101, 102, 103, 104, 105, 106, 107], index=dates, dtype=float)
    quarters = [dict(ticker='A', period_end=f'{year}-03-31', usable_date='2025-01-01',
                     d4=float(value)) for year, value in zip(range(2021, 2025), [1, 2, 4, 8])]
    panel.sue = pd.DataFrame(quarters)
    panel.events = pd.DataFrame([dict(ticker='A', effective_date=str(dates[3].date()))])
    factors = s.compute_factors(panel, adj, spy)
    expected = dict(F1=.2, F2=.2, F3=.2, F4=.03, F5=.25,
                    F6=4 / 180, F7=-.25, F8=8 / np.std([1, 2, 4, 8], ddof=1),
                    F9=110 / 102 - 104 / 101)
    assert set(factors) == set(expected)
    for factor, value in expected.items():
        assert factors[factor].iloc[4, 0] == pytest.approx(value)
    assert factors['F9'].iloc[:4].isna().all().all()


@pytest.mark.parametrize('factor,fields', [
    ('F1', ['assets']), ('F2', ['equity', 'equity_prev_year']),
    ('F3', ['market_cap']), ('F4', ['market_cap']), ('F5', ['market_cap']),
    ('F6', ['assets', 'assets_prev_year']), ('F7', ['assets_prev_year']),
])
@pytest.mark.parametrize('invalid', [0., -1., np.nan, np.inf, -np.inf])
def test_ratios_reject_nonpositive_or_nonfinite_denominators(factor, fields, invalid):
    dates = pd.bdate_range('2025', periods=3)
    panel = empty_panel(dates, ['A'])
    for field in fields:
        panel[field][:] = invalid
    prices = pd.DataFrame(100., index=dates, columns=['A'])
    values = s.compute_factors(panel, prices, prices['A'])
    assert values[factor].isna().all().all()


def test_bm_requires_positive_equity_and_negative_income_is_preserved():
    dates = pd.bdate_range('2025', periods=3)
    panel = empty_panel(dates, ['A'])
    panel['equity'].iloc[:, 0] = [-1, 0, np.nan]
    panel['ttm_net_income'][:] = -12
    prices = pd.DataFrame(100., index=dates, columns=['A'])
    values = s.compute_factors(panel, prices, prices['A'])
    assert values['F3'].isna().all().all()
    assert (values['F4'] == -.03).all().all()
    assert values['F2'].iloc[0, 0] == pytest.approx(-12 / 19.5)


def sue_rows(values):
    rows = []
    for i, value in enumerate(values):
        end = pd.Timestamp('2018-03-31') + pd.offsets.QuarterEnd(i)
        usable = end + pd.DateOffset(days=45)
        rows.append(dict(ticker='A', period_end=str(end.date()),
                         usable_date=str(usable.date()), d4=value, eps_q=999.))
    return pd.DataFrame(rows)


def test_sue_sample_std_minimum_four_last_eight_and_90_calendar_day_boundary():
    rows = sue_rows([1, 2, 3, 4, 5, 6, 7, 8, 100])
    anchor = pd.Timestamp(rows.iloc[-1].usable_date)
    dates = pd.date_range(rows.iloc[0].usable_date, anchor + pd.DateOffset(days=91))
    sue = s.sue_factor(rows, dates, ['A'])['A']
    assert np.isnan(sue.loc[rows.iloc[2].usable_date])
    assert sue.loc[rows.iloc[3].usable_date] == pytest.approx(4 / np.std([1, 2, 3, 4], ddof=1))
    expected = 100 / np.std([2, 3, 4, 5, 6, 7, 8, 100], ddof=1)
    assert sue.loc[anchor] == pytest.approx(expected)
    assert sue.loc[anchor + pd.DateOffset(days=90)] == pytest.approx(expected)
    assert np.isnan(sue.loc[anchor + pd.DateOffset(days=91)])


def test_sue_missing_quarters_zero_std_and_pit_revisions_do_not_fall_back():
    rows = sue_rows([1, np.nan, 2, 3, 4])
    revised = rows.iloc[[0]].copy()
    revised['usable_date'] = str((pd.Timestamp(rows.iloc[-1].usable_date) + pd.DateOffset(days=1)).date())
    revised['d4'] = 20.
    missing_latest = sue_rows([1, 2, 3, 4, 5, np.nan]).iloc[[-1]]
    table = pd.concat([rows, revised, missing_latest], ignore_index=True)
    dates = pd.date_range(rows.iloc[0].usable_date, missing_latest.iloc[0].usable_date)
    sue = s.sue_factor(table, dates, ['A'])['A']
    assert sue.loc[rows.iloc[-1].usable_date] == pytest.approx(4 / np.std([1, 2, 3, 4], ddof=1))
    assert sue.loc[revised.iloc[0].usable_date] == pytest.approx(4 / np.std([20, 2, 3, 4], ddof=1))
    assert np.isnan(sue.iloc[-1])
    constant = sue_rows([1, 1, 1, 1])
    assert s.sue_factor(constant, dates, ['A']).isna().all().all()


def test_ear_three_trading_returns_latest_completed_event_and_90_day_boundary():
    dates = pd.bdate_range('2025-01-01', '2025-07-01')
    stock = pd.DataFrame(100 * 1.02 ** np.arange(len(dates)), index=dates, columns=['A'])
    spy = pd.Series(100 * 1.01 ** np.arange(len(dates)), index=dates)
    e = dates[10]
    events = pd.DataFrame([dict(ticker='A', effective_date=str(e.date()))])
    values = s.ear_factor(events, stock, spy)['A']
    expected = 1.02 ** 3 - 1.01 ** 3
    assert values.loc[:e].isna().all()
    assert values.loc[dates[11]] == pytest.approx(expected)
    assert values.loc[e + pd.DateOffset(days=90)] == pytest.approx(expected)
    assert np.isnan(values.loc[e + pd.DateOffset(days=91)])
    events.loc[1] = ['A', str(dates[20].date())]
    # A newer event supersedes only when its next trading session completes.
    stock.iloc[21] *= 1.1
    changed = s.ear_factor(events, stock, spy)['A']
    assert changed.loc[dates[20]] == pytest.approx(expected)
    assert changed.loc[dates[21]] == pytest.approx(1.1 * 1.02 ** 3 - 1.01 ** 3)
    stock.iloc[19] = np.nan
    missing = s.ear_factor(events, stock, spy)['A']
    assert np.isnan(missing.loc[dates[21]])


def test_eligibility_253_valid_bars_close_price_and_20_day_dollar_median():
    dates = pd.bdate_range('2020', periods=280)
    close = pd.DataFrame(5., index=dates, columns=['A', 'B', 'C', 'D'])
    volume = close * 0 + 2_000_000
    close.loc[:, 'B'] = 4.99
    volume.loc[:, 'C'] = 1_999_999
    close.iloc[0, 3] = np.nan
    mask = s.eligibility_mask(close, volume)
    assert not mask.iloc[:252].any().any()
    assert mask.iloc[252, 0] and not mask.iloc[252, 3] and mask.iloc[253, 3]
    assert not mask[['B', 'C']].any().any()
    volume.iloc[270, 0] = -1
    assert not s.eligibility_mask(close, volume).iloc[270:, 0].any()
    volume.iloc[270, 0] = 0
    assert s.eligibility_mask(close, volume).iloc[270, 0]


def gate_statistics():
    broad = dict(train=dict(ic=.01, t=2.99, ic_halves=[.001, .02]),
                 val=dict(ic=.001, t=1.65), forward=dict(ic=.0001, t=-9))
    return broad, dict(forward=dict(ic=.001, t=1.))


def test_gates_bonferroni_exact_boundaries_and_no_descriptive_conditions():
    broad, transfer = gate_statistics()
    assert s.TRAIN_T == 2.99 and s.HORIZONS == (20, 60)
    assert s.gates(broad, transfer)['transfer_pass']
    for gate, target, key, invalid in [
        ('G1', broad['train'], 't', np.nextafter(2.99, 0)),
        ('G1', broad['train'], 'ic', 0),
        ('G1', broad['train'], 'ic_halves', [.01, 0]),
        ('G1', broad['train'], 'ic_halves', [None, .01]),
        ('G2', broad['val'], 't', np.nextafter(1.65, 0)),
        ('G2', broad['val'], 'ic', 0),
        ('G3', broad['forward'], 'ic', -1e-10),
        ('G4', transfer['forward'], 't', np.nextafter(1., 0)),
        ('G4', transfer['forward'], 'ic', None),
    ]:
        original = target[key]
        target[key] = invalid
        result = s.gates(broad, transfer)
        assert result['failing_gates'] == [gate]
        assert result['first_failing_gate'] == gate
        assert not result['transfer_pass']
        assert result['broad_pass'] == (gate == 'G4')
        target[key] = original


def test_daily_rank_ic_minimum_30_nw_h_minus_one_and_valid_day_halves(monkeypatch):
    dates = pd.bdate_range('2020', periods=9)
    values = pd.DataFrame(np.tile(np.arange(31), (9, 1)), index=dates, dtype=float)
    target = values.copy()
    target.iloc[0, :2] = np.nan
    target.iloc[1, :1] = np.nan
    ic = evaluate.cs_rank_ic(values, target, values.notna(), s.MIN_NAMES)
    assert np.isnan(ic.iloc[0]) and ic.iloc[1] == pytest.approx(1.)
    sample = pd.Series([.01, np.nan, -.02, .03, .02, np.nan, .01, .04, np.nan], index=dates)
    calls = []
    monkeypatch.setattr(evaluate, 'nw_t', lambda x, lag: calls.append(lag) or 123.)
    long_sample = pd.Series(np.arange(40) / 100, index=pd.bdate_range('2020', periods=40))
    assert s.ic_summary(long_sample, 60)['t'] == 123.
    assert calls == [59]
    summary = s.ic_summary(sample, 20)
    assert summary['half_n_days'] == [3, 3]
    assert summary['ic_halves'] == pytest.approx([(.01 - .02 + .03) / 3, (.02 + .01 + .04) / 3])
    assert summary['nw_lag'] == 19
    masks = s.segment_masks(pd.bdate_range('2024-12-30', periods=80), 20)
    assert masks['val'].sum() == 2
    assert not masks['forward'].iloc[-20:].any()


def test_labels_and_momentum_use_adjclose_and_exact_offsets():
    dates = pd.bdate_range('2024-01-01', periods=340)
    adj = pd.DataFrame({'A': 100 * 1.01 ** np.arange(len(dates))}, index=dates)
    for h in s.HORIZONS:
        assert s.forward_label(adj, h).iloc[270, 0] == pytest.approx(1.01 ** h - 1)
        assert s.forward_label(adj, h).iloc[-h:].isna().all().all()
    dates = pd.bdate_range('2024-01-01', periods=400)
    rng = np.random.default_rng(1)
    adj = pd.DataFrame(np.exp(rng.normal(0, .1, (400, 32)).cumsum(axis=0)), index=dates)
    mom = adj.shift(21) / adj.shift(252) - 1
    rows = [dict(factor='F1', gates=dict(broad_pass=True)), dict(factor='F1', gates=dict(broad_pass=True))]
    corr = s.passing_correlations(dict(F1=mom), adj, adj.notna(), rows)
    assert corr['factor_ids'] == ['F1']
    assert corr['momentum_12_1']['F1']['correlation'] == pytest.approx(1.)
    assert corr['momentum_12_1']['F1']['n_days'] > 30
    assert s.passing_correlations({}, adj, adj.notna(), [])['matrix'] == {}


@pytest.fixture
def synthetic_inputs(tmp_path):
    root = tmp_path / 'fundamentals'
    dates = pd.bdate_range('2012-01-02', periods=350).append(
        pd.bdate_range('2022-01-03', periods=40)).append(pd.bdate_range('2025-01-02', periods=90))
    symbols = [f'S{i:02d}' for i in range(32)]
    entries = {t: dict(cik=str(i + 1).zfill(10), groups=['sp500', 'bstock90'])
               for i, t in enumerate(symbols)}
    concepts = dict(GrossProfit=40, NetIncomeLoss=12, Revenues=100,
                    NetCashProvidedByUsedInOperatingActivities=16,
                    Assets=200, StockholdersEquity=80, CommonStockSharesOutstanding=10,
                    EarningsPerShareDiluted=1)
    for i, (ticker, entry) in enumerate(entries.items()):
        facts = {}
        for concept, base in concepts.items():
            rows = []
            for q in range(64):
                end = pd.Timestamp('2010-03-31') + pd.offsets.QuarterEnd(q)
                row = dict(end=str(end.date()), filed=str((end + pd.DateOffset(days=40)).date()),
                           form='10-Q', val=float(base + i + q / 10))
                if concept not in ('Assets', 'StockholdersEquity', 'CommonStockSharesOutstanding'):
                    row['start'] = str((end - pd.offsets.QuarterEnd() + pd.DateOffset(days=1)).date())
                rows.append(row)
            unit = 'shares' if concept == 'CommonStockSharesOutstanding' else 'USD/shares' if concept == 'EarningsPerShareDiluted' else 'USD'
            facts[concept] = dict(units={unit: rows})
        write_gzip(root / 'raw/companyfacts' / f'CIK{entry["cik"]}.json.gz', dict(facts={'us-gaap': facts}))
        write_gzip(root / 'raw/submissions' / f'CIK{entry["cik"]}.json.gz',
                   dict(form=['8-K'], items=['2.02'], acceptanceDateTime=['2025-01-15T20:00:00Z'],
                        accessionNumber=['event']))
        close = 100 + np.arange(len(dates)) * (.01 + i / 1000)
        rows = [[str(d.date()), float(c), float(c * (.8 + j / 2000)), 2_000_000.]
                for j, (d, c) in enumerate(zip(dates, close))]
        write_json(root / 'prices' / f'{ticker}.json', dict(rows=rows, splits=[]))
    write_json(root / 'prices/SPY.json', dict(rows=[[str(d.date()), 100., 100., 2e6] for d in dates]))
    raw = sorted(os.path.join(dp, f) for dp, _, fs in os.walk(root / 'raw') for f in fs if f.endswith('.gz'))
    write_json(root / 'manifest.json', dict(tickers=entries, raw_sha256=s.concatenated_sha256(raw)))
    write_json(root / 'prices_meta.json', dict(sha256=s.concatenated_sha256(sorted((root / 'prices').glob('*.json')))))
    write_json(s.coverage_path(root), dict(bstock={t: dict(kind='10-Q') for t in symbols}))
    return root


def test_hash_guard_full_path_sort_manifest_and_prereg_prices_meta_and_override(synthetic_inputs, monkeypatch):
    root = synthetic_inputs
    actual, drift = s.guard_inputs(root, True)
    assert set(drift) == {'raw_prereg', 'prices_prereg'}
    with pytest.raises(ValueError, match='sha256 mismatch'):
        s.guard_inputs(root)
    # Same basenames in different directories make basename-only sorting wrong.
    write_gzip(root / 'raw/aaa/z.json.gz', {'first': 1})
    write_gzip(root / 'raw/zzz/a.json.gz', {'last': 2})
    paths = sorted(os.path.join(dp, f) for dp, _, fs in os.walk(root / 'raw') for f in fs if f.endswith('.gz'))
    raw_hash = hashlib.sha256(b''.join(open(p, 'rb').read() for p in paths)).hexdigest()
    manifest = s.read_json(root / 'manifest.json')
    manifest['raw_sha256'] = raw_hash
    write_json(root / 'manifest.json', manifest)
    monkeypatch.setattr(s, 'EXPECTED_HASHES', dict(raw=raw_hash, prices=actual['prices']))
    assert s.guard_inputs(root)[1] == {}
    for path, expected_drift in [
        (root / 'raw/aaa/z.json.gz', {'raw_prereg', 'raw_manifest'}),
        (root / 'prices/SPY.json', {'prices_prereg', 'prices_meta'}),
    ]:
        original = path.read_bytes()
        path.write_bytes(original + b'\n')
        with pytest.raises(ValueError, match='sha256 mismatch'):
            s.guard_inputs(root)
        assert set(s.guard_inputs(root, True)[1]) == expected_drift
        path.write_bytes(original)
    manifest['raw_sha256'] = 'wrong'
    write_json(root / 'manifest.json', manifest)
    assert set(s.guard_inputs(root, True)[1]) == {'raw_manifest'}
    manifest['raw_sha256'] = raw_hash
    write_json(root / 'manifest.json', manifest)
    write_json(root / 'prices_meta.json', dict(sha256='wrong'))
    assert set(s.guard_inputs(root, True)[1]) == {'prices_meta'}


def test_universes_manifest_not_price_names_and_raw_probe_cross_check(synthetic_inputs):
    root = synthetic_inputs
    entries = s.read_json(root / 'manifest.json')['tickers']
    for i, ticker in enumerate(sorted(s.FOREIGN)):
        entries[ticker] = dict(cik=str(100 + i).zfill(10), groups=['bstock90'])
    entries['MISSING'] = dict(cik='0000000999', groups=['sp500'])
    write_json(root / 'manifest.json', dict(tickers=entries))
    probe = {t: dict(kind='20-F' if t in s.FOREIGN else '10-Q') for t in entries if t != 'MISSING'}
    write_json(s.coverage_path(root), dict(bstock=probe))
    symbols, prices, spy, composition = s.load_universes(root, frozen=False)
    assert len(symbols['U']) == len(symbols['T']) == 32
    assert composition['excluded_foreign'] == sorted(s.FOREIGN)
    assert composition['missing_broad_prices'] == ['MISSING']
    assert prices['close'].index.equals(spy.index)
    assert not prices['adjclose'].equals(prices['close'])
    probe['S00']['kind'] = '20-F'
    write_json(s.coverage_path(root), dict(bstock=probe))
    with pytest.raises(ValueError, match='T mismatch'):
        s.load_universes(root, frozen=False)


def test_no_lookahead_future_prices_and_future_usable_quarter_versions():
    dates = pd.bdate_range('2020-01-01', periods=400)
    panel = empty_panel(dates, ['A'])
    panel.sue = sue_rows([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12])
    panel.events = pd.DataFrame([dict(ticker='A', effective_date=str(dates[i].date())) for i in (40, 200, 300)])
    adj = pd.DataFrame({'A': 100 * 1.001 ** np.arange(len(dates))}, index=dates)
    spy = adj['A'] * 1.1
    cutoff = 230
    baseline = s.compute_factors(panel, adj, spy)
    changed = copy.deepcopy(panel)
    for frame in changed.values():
        frame.iloc[cutoff + 1:] *= 1000
    future = pd.to_datetime(changed.sue.usable_date) > dates[cutoff]
    changed.sue.loc[future, 'd4'] = 99999
    old_revision = changed.sue.iloc[[0]].copy()
    old_revision['usable_date'] = str(dates[cutoff + 5].date())
    old_revision['d4'] = -999999
    changed.sue = pd.concat([changed.sue, old_revision], ignore_index=True)
    future_adj, future_spy = adj.copy(), spy.copy()
    future_adj.iloc[cutoff + 1:] *= 10
    future_spy.iloc[cutoff + 1:] *= 100
    altered = s.compute_factors(changed, future_adj, future_spy)
    for factor in s.FACTORS:
        pd.testing.assert_frame_equal(baseline[factor].iloc[:cutoff + 1], altered[factor].iloc[:cutoff + 1])


def test_rebuilt_panel_no_lookahead_from_future_raw_filings_and_price_rows(synthetic_inputs):
    root = synthetic_inputs
    frame = s.read_prices(root / 'prices/S00.json')
    spy = s.read_prices(root / 'prices/SPY.json')['adjclose']
    kwargs = dict(raw_root=root / 'raw', prices_root=root / 'prices', tickers=['S00'],
                  start=str(frame.index[0].date()), end=str(frame.index[-1].date()))
    panel = fundamentals.build_panel(**kwargs)
    before = s.compute_factors(panel, frame[['adjclose']].rename(columns={'adjclose': 'S00'}), spy)
    cutoff = frame.index[320]
    path = root / 'raw/companyfacts/CIK0000000001.json.gz'
    with gzip.open(path, 'rt') as source:
        company = json.load(source)
    for concept in company['facts']['us-gaap'].values():
        for rows in concept['units'].values():
            for row in rows:
                if pd.Timestamp(row['filed']) >= cutoff:
                    row['val'] *= 100
            revision = copy.deepcopy(rows[0])
            revision.update(filed=str(frame.index[325].date()), val=999999., form='10-Q/A')
            rows.append(revision)
    write_gzip(path, company)
    prices = s.read_json(root / 'prices/S00.json')
    for row in prices['rows']:
        if pd.Timestamp(row[0]) > cutoff:
            row[1], row[2], row[3] = row[1] * 10, row[2] * 20, 0.
    write_json(root / 'prices/S00.json', prices)
    altered = s.read_prices(root / 'prices/S00.json')
    after = s.compute_factors(fundamentals.build_panel(**kwargs),
                              altered[['adjclose']].rename(columns={'adjclose': 'S00'}), spy)
    for factor in s.FACTORS:
        pd.testing.assert_frame_equal(before[factor].loc[:cutoff], after[factor].loc[:cutoff])
    pd.testing.assert_frame_equal(
        s.eligibility_mask(frame[['close']], frame[['volume']].rename(columns={'volume': 'close'})).loc[:cutoff],
        s.eligibility_mask(altered[['close']], altered[['volume']].rename(columns={'volume': 'close'})).loc[:cutoff])


def test_ledger_exact_fields_pass_fail_and_no_ledger(tmp_path):
    broad, transfer = gate_statistics()
    result = dict(factor='F1', h=20, gates=s.gates(broad, transfer))
    path = tmp_path / 'ledger.jsonl'
    row = s.ledger_result([result], {'raw': 'synthetic'}, path, {}, False)
    assert {key: row[key] for key in evolution_ledger.REQUIRED} == dict(
        track='fq_us', stage='S1', change_type='E', idea_id='us-fundamental-factors-v1',
        baseline_version='fq_us:mom_ls_invvol_v1', verdict='screen_pass')
    assert row['K'] == 1 and row['passing_transfer'] == [dict(factor='F1', h=20)]
    assert s.ledger_result([], {}, path, {}, False)['verdict'] == 'fail'
    assert len(evolution_ledger.read_rows(path)) == 2
    before = path.read_bytes()
    assert s.ledger_result([], {}, None, {}, False)['verdict'] == 'fail'
    assert path.read_bytes() == before


def test_output_guard_resolves_symlinks_and_ledger_collisions(tmp_path):
    root, out = tmp_path / 'fundamentals', tmp_path / 'out'
    root.mkdir()
    out.mkdir()
    for target, ledger in [(root / 'out', None), (out, root / 'ledger.jsonl'),
                            (out, s.coverage_path(root))]:
        with pytest.raises(ValueError, match='read-only'):
            s.validate_outputs(root, target, ledger)
    (out / 'report.json').symlink_to(root / 'manifest.json')
    with pytest.raises(ValueError, match='read-only'):
        s.validate_outputs(root, out, None)
    (out / 'report.json').unlink()
    with pytest.raises(ValueError, match='must differ'):
        s.validate_outputs(root, out, out / 'report.md')


def test_synthetic_cli_rebuilds_panel_reports_descriptives_and_one_ledger(synthetic_inputs, tmp_path):
    root, out = synthetic_inputs, tmp_path / 'out'
    hashes = s.guard_inputs(root, True)[0]
    with pytest.raises(SystemExit):
        s.main(['--fund-root', str(root), '--out', str(out)])
    assert not out.exists()
    assert s.main(['--fund-root', str(root), '--out', str(out), '--allow-input-drift']) == 0
    report = s.read_json(out / 'report.json')
    assert len(report['results']) == 18
    assert report['parameters']['train_t'] == 2.99
    assert report['panel_metadata']['tickers'] == 32
    assert len(evolution_ledger.read_rows(out / 'ledger.jsonl')) == 1
    assert report['verdict'] == ('screen_pass' if report['n_pass_transfer'] else 'fail')
    for row in report['results']:
        for universe in ('broad', 'transfer'):
            assert set(row[universe]) == set(s.PERIODS)
            for summary in row[universe].values():
                assert summary['nw_lag'] == row['h'] - 1
                assert 'median_available_names' in summary and 'positive_month_share' in summary
        assert set(row['broad_ls']) == set(s.PERIODS)
        assert 'failing_gates' in row['gates']
    markdown = (out / 'report.md').read_text()
    assert f'结论：**{report["verdict"]}**' in markdown
    assert '失败门槛' in markdown and 'deviations' in markdown and '12-1' in markdown
    assert s.guard_inputs(root, True)[0] == hashes


def test_quintile_equal_weight_h_spacing_and_transaction_costs():
    dates = pd.bdate_range('2025', periods=61)
    values = pd.DataFrame(np.tile(np.arange(30), (61, 1)), index=dates, dtype=float)
    labels = values * 0 + .02
    labels.iloc[:, -6:] = .1
    segment = pd.Series(False, index=dates)
    segment.iloc[:21] = True
    result = s.quintile_returns(values, labels, values.notna(), segment, 20)
    assert [row['date'] for row in result['periods']] == [str(dates[i].date()) for i in (0, 20)]
    assert result['gross_mean'] == pytest.approx(.08)
    assert result['net_mean'] < result['gross_mean']
    assert result['periods'][0]['entry_cost'] == pytest.approx(.002)
    assert result['periods'][-1]['exit_cost'] > 0
    assert result['periods'][0]['n_long'] == result['periods'][0]['n_short'] == 6
