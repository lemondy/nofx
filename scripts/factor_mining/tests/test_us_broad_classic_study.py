"""Frozen classic-factor formulas and study plumbing, using synthetic data only."""
import copy
import hashlib
import json
from pathlib import Path

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import evolution_ledger
from scripts.factor_mining import us_broad_classic_study as s


def minimal_panel():
    # 253 closes are the minimum needed by the registered 252-return OLS.
    dates = pd.bdate_range('2020-01-01', periods=253)
    x = np.tile([-.01, 0., .01], 84)
    epsilon = np.tile([-.001, .002, -.001], 84)
    y = .001 + 2 * x + epsilon
    z = .002 + .5 * x
    close = pd.DataFrame(np.vstack([np.full(2, 100.), 100 * np.cumprod(1 + np.c_[y, z], axis=0)]),
                         index=dates, columns=['A', 'B'])
    spy = pd.Series(np.r_[100., 100 * np.cumprod(1 + x)], index=dates)
    volume = pd.DataFrame(2_000_000., index=dates, columns=close.columns)
    return close, volume, spy, x, y


@pytest.mark.parametrize('factor', list(s.FACTORS))
def test_each_frozen_factor_formula(factor):
    close, volume, spy, x, y = minimal_panel()
    values = s.compute_factors(close, volume, spy)
    c = close['A'].to_numpy()
    # OLS x and epsilon are orthogonal; every 63/252 window has full 3-day cycles.
    residual_std = np.sqrt(21 * (.001**2 + .002**2 + .001**2) / 62)
    lowvol = np.sqrt((4 * 21 * (.01**2 + .01**2) + 21 * (.001**2 + .002**2 + .001**2)) / 62)
    expected = dict(F1=-(c[-1] / c[-22] - 1), F2=-(c[-1] / c[-6] - 1),
                    F3=c[-22] / c[0] - 1, F4=c[-22] / c[-127] - 1,
                    F5=-lowvol, F6=-residual_std,
                    F7=c[-1] / c[-252:].max(), F8=-max(y[-21:]), F9=-2.,
                    F10=np.log(np.mean(np.abs(y[-21:]) / (c[-21:] * 2_000_000))))
    assert values[factor].iloc[-1, 0] == pytest.approx(expected[factor], rel=1e-10, abs=1e-13)
    if factor == 'F6':
        assert values[factor].iloc[-1, 1] == pytest.approx(0., abs=1e-9)
    if factor == 'F9':
        assert values[factor].iloc[-1, 1] == pytest.approx(-.5)
    if factor == 'F10':
        assert values[factor].iloc[-1, 0] < 0
        assert values[factor].iloc[-1, 0] != pytest.approx(np.exp(expected[factor]))


def test_rolling_ols_matches_explicit_intercept_regression_and_date_alignment():
    dates = pd.bdate_range('2018-01-01', periods=340)
    rng = np.random.default_rng(501)
    x = pd.Series(rng.normal(.001, .01, len(dates)), index=dates)
    y = pd.DataFrame({'A': .004 + 1.7 * x + rng.normal(0, .005, len(dates)),
                      'B': -.002 - .8 * x + rng.normal(0, .002, len(dates))})
    y.iloc[70, 0] = np.nan
    for window in (63, 252):
        beta, ivol = s.rolling_ols(y, x.iloc[::-1], window)
        for column in y.columns:
            for end in (251, 322, 339):
                begin = end - window + 1
                xx, yy = x.iloc[begin:end+1].to_numpy(), y[column].iloc[begin:end+1].to_numpy()
                if np.isnan(yy).any():
                    assert np.isnan(beta[column].iloc[end])
                    assert np.isnan(ivol[column].iloc[end])
                else:
                    design = np.c_[np.ones(window), xx]
                    coefficients = np.linalg.lstsq(design, yy, rcond=None)[0]
                    assert beta[column].iloc[end] == pytest.approx(coefficients[1], abs=1e-12)
                    assert ivol[column].iloc[end] == pytest.approx(np.std(yy - design @ coefficients, ddof=1))
    constant = pd.Series(.01, index=dates)
    beta, ivol = s.rolling_ols(y, constant, 63)
    assert beta.isna().all().all()
    assert ivol.isna().all().all()


def test_missing_stock_date_does_not_forward_fill_returns_or_rolling_windows():
    close, volume, spy, _, _ = minimal_panel()
    close.iloc[200, 0] = np.nan
    values = s.compute_factors(close, volume, spy)
    assert np.isnan(values['F8'].iloc[200, 0])
    assert np.isnan(values['F8'].iloc[220, 0])
    assert np.isnan(values['F8'].iloc[221, 0])
    assert np.isfinite(values['F8'].iloc[222, 0])
    assert np.isnan(values['F6'].iloc[-1, 0])
    assert np.isnan(values['F9'].iloc[-1, 0])
    assert np.isfinite(values['F9'].iloc[-1, 1])


def test_eligibility_exact_price_liquidity_and_history_boundaries():
    dates = pd.bdate_range('2020', periods=280)
    close = pd.DataFrame(5., index=dates, columns=['edge', 'cheap', 'illiquid', 'gap', 'zero', 'negative'])
    volume = close * 0 + 2_000_000.
    close['cheap'] = 4.999
    volume['illiquid'] = 1_999_999.
    close.iloc[10, 3] = np.nan
    volume.iloc[-1, 4] = 0.
    volume.iloc[-1, 5] = -1.
    mask = s.eligibility_mask(close, volume)
    assert not mask.iloc[:252].any().any()
    assert mask.iloc[252]['edge']
    assert not mask['cheap'].any()
    assert not mask['illiquid'].any()
    assert not mask.iloc[252]['gap']
    assert mask.iloc[253]['gap']
    assert mask.iloc[-1]['zero']
    assert not mask.iloc[-1]['negative']
    # Median, not mean: 11 low dollar-volume observations make the stock ineligible.
    volume.loc[dates[-11:], 'edge'] = 1_000_000.
    mask = s.eligibility_mask(close, volume)
    assert not mask.iloc[-1]['edge']
    # Even after sufficient cumulative history, incomplete 20-day liquidity is unavailable.
    close.iloc[265, 3] = np.nan
    assert not s.eligibility_mask(close, volume).iloc[270]['gap']


def test_nw_known_series_and_registered_h_minus_one():
    values = np.tile([-.1, 0., .2, .3], 10)
    centered = values - values.mean()
    # Independently form the Bartlett HAC quadratic form.
    lag = 4
    kernel = np.eye(len(values))
    for i in range(len(values)):
        for j in range(len(values)):
            if 0 < abs(i - j) <= lag:
                kernel[i, j] = 1 - abs(i - j) / (lag + 1)
    expected = values.mean() / np.sqrt(centered @ kernel @ centered / len(values)**2)
    dates = pd.bdate_range('2025', periods=len(values))
    result = s.ic_summary(pd.Series(values, index=dates), 5)
    assert expected == pytest.approx(8.032193289024987)
    assert result['t'] == pytest.approx(expected)
    assert result['nw_lag'] == 4
    assert result['t'] != pytest.approx(s.evaluate.nw_t(values, 5))
    short = values[:19]
    short_centered = short - short.mean()
    short_expected = short.mean() / np.sqrt(short_centered @ kernel[:19, :19] @ short_centered / 19**2)
    assert s.ic_summary(pd.Series(short, index=dates[:19]), 5)['t'] == pytest.approx(short_expected)
    assert s.nw_t([1.], 4) == 1e12
    assert s.nw_t([], 4) == 0


def gate_statistics():
    broad = dict(train=dict(ic=.01, t=3.14, ic_halves=[.001, .02]),
                 val=dict(ic=.02, t=1.65), forward=dict(ic=.001, t=-100.))
    transfer = dict(forward=dict(ic=.001, t=1.))
    return broad, transfer


def test_gates_frozen_boundaries_bonferroni_and_forward_point_estimate():
    broad, transfer = gate_statistics()
    assert all(s.gates(broad, transfer).values())
    broad['train']['t'] = np.nextafter(3.14, -np.inf)
    result = s.gates(broad, transfer)
    assert not result['G1'] and not result['broad_pass'] and not result['transfer_pass']
    assert result['G2'] and result['G3'] and result['G4']


@pytest.mark.parametrize('segment,key,value,failed', [
    ('train', 'ic', 0., 'G1'), ('train', 'ic', None, 'G1'),
    ('train', 'ic_halves', [0., .02], 'G1'), ('train', 'ic_halves', [.01, -.01], 'G1'),
    ('val', 'ic', 0., 'G2'), ('val', 't', 1.64999, 'G2'),
    ('forward', 'ic', 0., 'G3'), ('forward', 'ic', np.nan, 'G3'),
    ('transfer', 'ic', 0., 'G4'), ('transfer', 't', .99999, 'G4'),
])
def test_each_gate_failure(segment, key, value, failed):
    broad, transfer = gate_statistics()
    cell = transfer['forward'] if segment == 'transfer' else broad[segment]
    cell[key] = value
    result = s.gates(broad, transfer)
    assert not result[failed]
    assert not result['transfer_pass']
    assert result['broad_pass'] == (failed == 'G4')


def test_halves_and_positive_months_use_valid_daily_ics():
    dates = pd.to_datetime(['2021-01-04', '2021-01-05', '2021-02-01', '2021-02-02',
                            '2021-03-01', '2021-04-01'])
    ic = pd.Series([.2, .2, -.1, np.nan, 0., .2], index=dates)
    summary = s.ic_summary(ic, 20)
    assert summary['ic_halves'] == pytest.approx([.2, 1/30])
    assert summary['half_n_days'] == [2, 3]
    assert summary['positive_month_share'] == .5
    assert summary['n_months'] == 4
    assert summary['n_days'] == 5
    empty = s.ic_summary(ic.iloc[:0], 5)
    assert empty['ic'] is None and empty['ic_halves'] == [None, None]
    assert empty['positive_month_share'] is None


def test_rank_ic_pairwise_ties_and_minimum_30_names():
    dates = pd.bdate_range('2025', periods=3)
    values = pd.DataFrame(np.tile(np.arange(31), (3, 1)), index=dates, dtype=float)
    target = values.copy()
    values.iloc[:, :2] = 1.
    target.iloc[:, :2] = .5
    target.iloc[1, :2] = np.nan
    mask = values.notna()
    mask.iloc[2, 0] = False
    ic = s.evaluate.cs_rank_ic(values, target, mask, s.MIN_NAMES)
    assert ic.iloc[0] == pytest.approx(1.)
    assert np.isnan(ic.iloc[1])
    assert ic.iloc[2] == pytest.approx(1.)


def test_signal_date_segments_and_complete_label_cutoff_without_extra_purge():
    dates = pd.bdate_range('2021-12-20', '2025-02-20')
    for h in s.HORIZONS:
        masks = s.segment_masks(dates, h)
        assert masks['train'].loc['2021-12-31']
        assert masks['val'].loc['2022-01-03']
        assert masks['val'].loc['2024-12-31']
        assert masks['forward'].loc['2025-01-01']
        assert masks['forward'].iloc[-h-1]
        assert not masks['forward'].iloc[-h:].any()
    early = pd.bdate_range('2012-10-25', '2012-11-08')
    assert not s.segment_masks(early, 5)['train'].loc['2012-10-31']
    assert s.segment_masks(early, 5)['train'].loc['2012-11-01']


def test_no_lookahead_all_factors_eligibility_and_exact_t_plus_h_label():
    rng = np.random.default_rng(16)
    dates = pd.bdate_range('2020', periods=330)
    close = pd.DataFrame(100 * np.exp(np.cumsum(rng.normal(.001, .01, (330, 3)), axis=0)),
                         index=dates, columns=list('ABC'))
    volume = pd.DataFrame(2_000_000., index=dates, columns=close.columns)
    spy = pd.Series(100 * np.exp(np.cumsum(rng.normal(0, .01, 330))), index=dates)
    cutoff = 280
    altered_close, altered_volume, altered_spy = close.copy(), volume.copy(), spy.copy()
    altered_close.iloc[cutoff+1:] *= np.arange(1, 330-cutoff)[:, None] + 2
    altered_volume.iloc[cutoff+1:] = 0.
    altered_spy.iloc[cutoff+1:] *= 3
    factors = s.compute_factors(close, volume, spy)
    altered = s.compute_factors(altered_close, altered_volume, altered_spy)
    prefix = s.compute_factors(close.iloc[:cutoff+1], volume.iloc[:cutoff+1], spy.iloc[:cutoff+1])
    for factor in s.FACTORS:
        pd.testing.assert_frame_equal(factors[factor].iloc[:cutoff+1], altered[factor].iloc[:cutoff+1])
        pd.testing.assert_frame_equal(factors[factor].iloc[:cutoff+1], prefix[factor])
    pd.testing.assert_frame_equal(s.eligibility_mask(close, volume).iloc[:cutoff+1],
                                  s.eligibility_mask(altered_close, altered_volume).iloc[:cutoff+1])
    for h in s.HORIZONS:
        labels = s.forward_label(close, h)
        np.testing.assert_allclose(labels.iloc[cutoff], close.iloc[cutoff+h] / close.iloc[cutoff] - 1)
        assert labels.iloc[-h:].isna().all().all()
        endpoint = close.copy()
        endpoint.iloc[cutoff+h] *= 2
        np.testing.assert_allclose(s.forward_label(endpoint, h).iloc[cutoff], 2*(labels.iloc[cutoff]+1)-1)
        other_future = close.copy()
        other_future.iloc[cutoff+1:cutoff+h] *= 5
        other_future.iloc[cutoff+h+1:] *= 7
        pd.testing.assert_series_equal(labels.iloc[cutoff], s.forward_label(other_future, h).iloc[cutoff])


def ls_panel(n=30):
    dates = pd.bdate_range('2025', periods=11)
    values = pd.DataFrame(np.tile(np.arange(n), (11, 1)), index=dates,
                          columns=[f'S{i:02d}' for i in range(n)], dtype=float)
    labels = values * 0 + .02
    labels.iloc[:, -n//5:] = .1
    mask = values.notna()
    return values, labels, mask


def test_quintile_equal_weights_and_10bp_actual_entry_exit_notional_cost():
    values, labels, mask = ls_panel()
    segment = pd.Series(False, index=values.index)
    segment.iloc[0] = True
    result = s.quintile_returns(values, labels, mask, segment, 5)
    assert result['n_periods'] == 1
    assert result['gross_mean'] == pytest.approx(.08)
    assert result['net_mean'] == pytest.approx(.08 - .002 - .001 * (1.1 + 1.02))
    assert result['periods'][0]['n_long'] == result['periods'][0]['n_short'] == 6
    assert result['periods'][0]['entry_turnover'] == pytest.approx(2.)


def test_quintile_h_day_phase_turnover_with_price_drift_and_terminal_cost():
    values, labels, mask = ls_panel()
    segment = pd.Series(False, index=values.index)
    segment.iloc[:6] = True
    result = s.quintile_returns(values, labels, mask, segment, 5)
    assert [row['date'] for row in result['periods']] == [str(values.index[i].date()) for i in (0, 5)]
    drift = np.array([1.1, -1.02]) / (1 + .08 - .002)
    expected_turnover = np.abs(np.array([1., -1.]) - drift).sum()
    assert result['periods'][1]['entry_turnover'] == pytest.approx(expected_turnover)
    assert result['periods'][0]['exit_cost'] == 0
    assert result['periods'][1]['exit_cost'] > 0
    assert result['net_cumulative'] == pytest.approx(np.prod([1+row['net_return'] for row in result['periods']])-1)
    mask.iloc[0] = False
    result = s.quintile_returns(values, labels, mask, segment, 5)
    assert [row['date'] for row in result['periods']] == [str(values.index[5].date())]
    assert result['periods'][0]['entry_turnover'] == pytest.approx(2.)


def test_quintiles_never_use_future_label_availability_to_select_names():
    values, labels, mask = ls_panel(32)
    segment = pd.Series(False, index=values.index)
    segment.iloc[0] = True
    labels.iloc[0, -1] = np.nan
    result = s.quintile_returns(values, labels, mask, segment, 5)
    assert result['n_periods'] == 0
    assert result['missing_endpoint_dates'] == [str(values.index[0].date())]
    labels.iloc[0, -1] = 100.
    positive = s.quintile_returns(values, labels, mask, segment, 5)
    labels.iloc[0, -1] = -.99
    negative = s.quintile_returns(values, labels, mask, segment, 5)
    assert positive['periods'][0]['long_symbols'] == negative['periods'][0]['long_symbols']


def write_daily(path, dates, close, volume=2_000_000.):
    rows = [[str(d.date()), float(c), float(c), float(c), float(c), volume, float(c)]
            for d, c in zip(dates, close)]
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(rows))


@pytest.fixture
def synthetic_inputs(tmp_path):
    broad, bstock = tmp_path / 'broad', tmp_path / 'bstock'
    dates = pd.bdate_range('2019-01-01', periods=280).append(
        pd.bdate_range('2022-01-03', periods=35)).append(pd.bdate_range('2025-01-02', periods=45))
    rng = np.random.default_rng(103)
    symbols = [f'S{i:02d}' for i in range(32)]
    for symbol in symbols:
        prices = 100 * np.exp(np.cumsum(rng.normal(.0005, .01, len(dates))))
        write_daily(broad / 'daily' / f'{symbol}.json', dates, prices)
        write_daily(bstock / 'daily' / f'{symbol}.json', dates, prices)
    write_daily(bstock / 'daily/ETF.json', dates, prices)
    write_daily(broad / 'benchmark/SPY.json', dates, 100*np.exp(np.cumsum(rng.normal(0, .01, len(dates)))))
    (broad / 'constituents_20261105.json').write_text(json.dumps(dict(sp500=symbols + ['MISSING'], ndx100=[])))
    (broad / 'bstock_instrument_types_20261105.json').write_text(json.dumps(dict.fromkeys(symbols, 'EQUITY') | {'ETF': 'ETF'}))
    return broad, bstock


def test_hash_guard_raw_sorted_concatenation_all_five_inputs_and_explicit_override(synthetic_inputs, monkeypatch):
    broad, bstock = synthetic_inputs
    hashes, drift = s.guard_inputs(broad, bstock, True)
    assert set(drift) == set(s.EXPECTED_HASHES)
    expected = hashlib.sha256(b''.join(p.read_bytes() for p in sorted((broad / 'daily').glob('*.json')))).hexdigest()
    assert hashes['broad_daily'] == expected
    expected_t = hashlib.sha256(b''.join(p.read_bytes() for p in sorted((bstock / 'daily').glob('*.json')))).hexdigest()
    assert hashes['bstock_daily'] == expected_t
    with pytest.raises(ValueError, match='sha256 mismatch'):
        s.guard_inputs(broad, bstock)
    monkeypatch.setattr(s, 'EXPECTED_HASHES', hashes.copy())
    assert s.guard_inputs(broad, bstock)[1] == {}
    paths = {'broad_constituents': broad / 'constituents_20261105.json',
             'broad_daily': broad / 'daily/S00.json', 'spy': broad / 'benchmark/SPY.json',
             'bstock_daily': bstock / 'daily/S00.json',
             'bstock_instrument_types': broad / 'bstock_instrument_types_20261105.json'}
    for key, path in paths.items():
        original = path.read_bytes()
        path.write_bytes(original + b'\n')
        with pytest.raises(ValueError, match=key):
            s.guard_inputs(broad, bstock)
        actual, drift = s.guard_inputs(broad, bstock, True)
        assert set(drift) == {key}
        assert actual[key] != hashes[key]
        path.write_bytes(original)


def test_loader_uses_membership_equity_types_spy_calendar_and_valid_bars(synthetic_inputs):
    broad, bstock = synthetic_inputs
    path = broad / 'daily/S00.json'
    rows = json.loads(path.read_text())
    missing_date = rows.pop(200)[0]
    invalid_date = rows[10][0]
    rows[10][2] = -1
    path.write_text(json.dumps(rows))
    universes, spy, composition = s.load_universes(broad, bstock, frozen=False)
    assert 'ETF' not in universes['T']['close']
    assert composition['excluded_bstock_symbols'] == ['ETF']
    assert composition['missing_constituents'] == ['MISSING']
    assert universes['U']['close'].index.equals(spy.index)
    assert np.isnan(universes['U']['close'].loc[missing_date, 'S00'])
    assert np.isnan(universes['U']['close'].loc[invalid_date, 'S00'])
    assert np.isnan(universes['U']['volume'].loc[invalid_date, 'S00'])
    with pytest.raises(ValueError, match='frozen universe counts'):
        s.load_universes(broad, bstock, frozen=True)


def test_output_guard_including_symlinks_and_ledger_collisions(synthetic_inputs, tmp_path):
    broad, bstock = synthetic_inputs
    for out, ledger in [(broad / 'out', None), (bstock, None), (tmp_path / 'out', broad / 'ledger')]:
        with pytest.raises(ValueError, match='read-only'):
            s.validate_outputs(broad, bstock, out, ledger)
    alias = tmp_path / 'alias'
    alias.symlink_to(broad, target_is_directory=True)
    with pytest.raises(ValueError, match='read-only'):
        s.validate_outputs(broad, bstock, alias / 'out', None)
    out = tmp_path / 'out'
    out.mkdir()
    (out / 'report.json').symlink_to(broad / 'constituents_20261105.json')
    with pytest.raises(ValueError, match='read-only'):
        s.validate_outputs(broad, bstock, out, None)
    (out / 'report.json').unlink()
    with pytest.raises(ValueError, match='must differ'):
        s.validate_outputs(broad, bstock, out, out / 'report.md')


def test_ledger_fields_counts_fixed_gates_and_locked_k(tmp_path):
    broad, transfer = gate_statistics()
    passing = dict(factor='F1', h=5, gates=s.gates(broad, transfer))
    broad_only = copy.deepcopy(passing)
    broad_only.update(factor='F2', h=10)
    broad_only['gates']['G4'] = broad_only['gates']['transfer_pass'] = False
    ledger = tmp_path / 'ledger.jsonl'
    hashes = {'synthetic': 'abc'}
    first = s.ledger_result([passing, broad_only], hashes, ledger, {}, False)
    second = s.ledger_result([], hashes, ledger, {}, False)
    assert first['K'] == 1 and second['K'] == 2
    assert first['track'] == 'fq_us' and first['stage'] == 'S1'
    assert first['change_type'] == 'E' and first['idea_id'] == 'us-broad-classic-v1'
    assert first['verdict'] == 'screen_pass' and second['verdict'] == 'fail'
    assert first['n_pass_broad'] == 2 and first['n_pass_transfer'] == 1
    assert first['passing_transfer'] == [dict(factor='F1', h=5)]
    assert first['input_hashes'] == hashes and first['prereg_path'] == s.PREREG
    assert evolution_ledger.read_rows(ledger) == [first, second]
    original = ledger.read_bytes()
    assert s.ledger_result([passing], hashes, None, {}, False)['verdict'] == 'screen_pass'
    assert ledger.read_bytes() == original


def test_passing_factor_correlation_deduplicates_h_and_uses_forward_mask():
    dates = pd.bdate_range('2024-12-30', periods=10)
    frame = pd.DataFrame(np.tile(np.arange(32), (len(dates), 1)), index=dates, dtype=float)
    factors = {'F1': frame, 'F2': -frame, 'F3': frame * 0}
    rows = [dict(factor=f, h=h, gates=dict(broad_pass=passed)) for f, h, passed in
            [('F1', 5, True), ('F1', 10, True), ('F2', 5, True), ('F3', 5, False)]]
    mask = frame.notna()
    mask.iloc[-1, :3] = False
    corr = s.passing_correlations(factors, mask, rows)
    assert corr['factor_ids'] == ['F1', 'F2']
    assert corr['matrix']['F1']['F2'] == pytest.approx(-1.)
    assert corr['matrix']['F1']['F1'] == pytest.approx(1.)
    assert corr['n_days']['F1']['F2'] == 7
    assert s.passing_correlations(factors, mask, [])['matrix'] == {}


def test_synthetic_cli_end_to_end_reports_ledger_and_read_only_inputs(synthetic_inputs, tmp_path):
    broad, bstock = synthetic_inputs
    before = s.guard_inputs(broad, bstock, True)[0]
    out = tmp_path / 'out'
    ledger = out / 'ledger.jsonl'
    args = ['--broad-root', str(broad), '--bstock-root', str(bstock), '--out', str(out), '--ledger', str(ledger)]
    with pytest.raises(SystemExit):
        s.main(args)
    assert not out.exists()
    assert s.main(args + ['--allow-input-drift']) == 0
    report = json.loads((out / 'report.json').read_text())
    assert len(report['results']) == 30
    assert {(row['factor'], row['h']) for row in report['results']} == {(f, h) for f in s.FACTORS for h in s.HORIZONS}
    assert report['allow_input_drift'] and report['input_drift']
    assert len(evolution_ledger.read_rows(ledger)) == 1
    assert report['verdict'] == ('screen_pass' if report['n_pass_transfer'] else 'fail')
    for row in report['results']:
        for universe in ('broad', 'transfer'):
            assert set(row[universe]) == set(s.PERIODS)
            for summary in row[universe].values():
                assert summary['nw_lag'] == row['h'] - 1
                assert len(summary['ic_halves']) == 2
                assert 'positive_month_share' in summary
        assert set(row['broad_ls']) == set(s.PERIODS)
        assert set(row['transfer_ls']) == {'forward'}
    assert '结论：**' + report['verdict'] + '**' in (out / 'report.md').read_text()
    assert s.guard_inputs(broad, bstock, True)[0] == before
