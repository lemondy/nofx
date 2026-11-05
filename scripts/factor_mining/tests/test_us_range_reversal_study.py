"""Frozen methodology tests: synthetic bars and worktree-local ledgers only."""
import hashlib
import json
from pathlib import Path
from statistics import NormalDist
import tempfile
from unittest.mock import patch

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import evolution_ledger
from scripts.factor_mining import evaluate, us_range_reversal_study as s


@pytest.fixture
def local_root():
    with tempfile.TemporaryDirectory(dir=s.REPO, prefix='.rangerev_test_') as directory:
        yield Path(directory)


def bars(close, dates=None):
    close = np.asarray(close, dtype=float)
    dates = pd.bdate_range('2023-01-02', periods=len(close)) if dates is None else dates
    return pd.DataFrame(dict(close=close, high=close * 20, low=close / 20), index=dates)


def statistics(z=2.1, **changes):
    return dict(dict(delta=.0021, se=.001, z=z, dev_delta=.001, holdout_delta=.003,
                     n_days=140, trade_ratio=1.1, placebo_pct=75.), **changes)


def synthetic_report():
    return dict(prereg_path=s.PREREG, data_path=str(s.REPO / '.synthetic_read_only'),
                input_hashes=dict(daily_json='synthetic-hash'),
                primary={arm: statistics() for arm in s.VARIANTS})


def synthetic_inputs(root, n=400, names=12):
    data = root / 'daily'
    data.mkdir()
    dates = pd.bdate_range('2023-01-02', periods=n)
    for j, ticker in enumerate(['SPY'] + [f'S{i:02d}' for i in range(names)]):
        close = 50 + np.arange(n) * (.05 + j * .004) + np.sin(np.arange(n) / (9 + j))
        rows = [[str(d.date()), float(c), float(c * 2), float(c / 2), float(c), 100., float(c)]
                for d, c in zip(dates, close)]
        (data / f'{ticker}.json').write_text(json.dumps(rows), encoding='utf-8')
    return data


def test_signal_formulas_and_close_only_range():
    close = 50 + np.arange(280, dtype=float) / 5
    close[0], close[30], close[200], close[252] = 10000, 2, 120, 80
    frame = s.ticker_features(bars(close))
    assert frame['mom12_1'].iloc[:252].isna().all()
    assert frame['mom12_1'].iloc[252] == pytest.approx(close[231] / close[0] - 1)
    assert frame['mom12_1'].iloc[270] == pytest.approx(close[249] / close[18] - 1)
    assert frame['range_pos'].iloc[:251].isna().all()
    window = close[1:253]
    assert frame['range_pos'].iloc[252] == pytest.approx((80 - window.min()) / (window.max() - window.min()))
    assert frame['mom6'].iloc[:126].isna().all()
    assert frame['mom6'].iloc[126] == pytest.approx(close[126] / close[0] - 1)
    assert frame['mom6'].iloc[252] == pytest.approx(close[252] / close[126] - 1)
    assert frame['sma200'].iloc[:199].isna().all()
    assert frame['sma200'].iloc[252] == pytest.approx(close[53:253].mean())
    changed = bars(close)
    changed['high'], changed['low'] = 1e20, -1e20
    pd.testing.assert_frame_equal(s.ticker_features(changed), frame)


def test_flat_range_has_no_value_and_sma_comparison_is_strict():
    dates = pd.bdate_range('2023-01-02', periods=300)
    panel = s.align_panels(dict(SPY=bars(np.arange(300) + 10, dates),
                               FLAT=bars(np.full(300, 10.), dates),
                               UP=bars(np.arange(300) + 10, dates),
                               DOWN=bars(400 - np.arange(300), dates)))
    assert panel['range_pos']['FLAT'].isna().all()
    eligible = s.eligible_pool(panel)
    assert not eligible['FLAT'].any()
    assert not eligible['DOWN'].any()
    assert not eligible['UP'].iloc[:252].any()
    assert eligible['UP'].iloc[252:280].all()
    assert not eligible['UP'].iloc[280:].any()
    assert s.eligible_pool(panel, use_sma=False)['DOWN'].iloc[252:280].all()


def test_forward_alignment_uses_own_bars_and_missing_master_day_is_excluded():
    dates = pd.bdate_range('2023-01-02', periods=350)
    own_dates = dates.delete([270, 310])
    own_close = np.arange(len(own_dates), dtype=float) + 10
    panel = s.align_panels(dict(SPY=bars(np.arange(350) + 10, dates),
                               XYZ=bars(own_close, own_dates)))
    for h in (5, 10, 20):
        assert panel[f'fwd{h}'].at[dates[260], 'XYZ'] == pytest.approx(own_close[260 + h] / own_close[260] - 1)
    assert panel['fwd20'].at[dates[300], 'XYZ'] == pytest.approx(own_close[299 + 20] / own_close[299] - 1)
    assert panel['mom12_1'].at[dates[300], 'XYZ'] == pytest.approx(own_close[299 - 21] / own_close[299 - 252] - 1)
    assert panel['close'].loc[dates[[270, 310]], 'XYZ'].isna().all()
    assert not s.eligible_pool(panel).loc[dates[[270, 310]], 'XYZ'].any()
    assert pd.isna(panel['fwd20'].at[own_dates[-20], 'XYZ'])
    assert panel['close'].index.equals(dates)
    assert list(panel['close'].columns) == ['XYZ']


def test_top_five_ties_are_alphabetical_in_both_directions():
    values = pd.Series(2., index=list('LKJIHGFEDCBA'))
    eligible = pd.Series(True, index=values.index)
    for ascending in (True, False):
        assert s.top_five(values, eligible, ascending) == list('ABCDE')
    eligible['A'] = False
    values['B'] = np.nan
    assert s.top_five(values, eligible, True) == list('CDEFG')
    values = pd.Series([1., 1., 2., 2., 3., 3., 4.], index=list('GFEDCBA'))
    assert s.top_five(values, pd.Series(True, index=values.index), False) == list('ABCDEFG')[:5]


def selection_panel():
    dates = pd.bdate_range('2024-01-01', periods=3)
    columns = list('LKJIHGFEDCBA')
    frame = pd.DataFrame(1., index=dates, columns=columns)
    panel = dict(mom12_1=frame.copy(), mom6=frame.copy(), range_pos=frame.copy(),
                 close=frame * 20, sma200=frame * 10, fwd20=frame * .01)
    panel['mom12_1'].iloc[1, :3] = np.nan
    panel['mom12_1'].iloc[2, :2] = np.nan
    return panel


def test_skip_fewer_than_ten_eligible_but_keep_exactly_ten():
    panel = selection_panel()
    returns, selections, eligible = s.select_groups(panel)
    dates = panel['close'].index
    assert eligible.sum(axis=1).tolist() == [12, 9, 10]
    for arm in s.SIGNALS:
        assert list(selections[arm]) == [dates[0], dates[2]]
        assert selections[arm][dates[0]] == list('ABCDE')
        assert returns[arm].tolist() == pytest.approx([.01, .01])
    stats = s.paired_diff(returns['V1'], returns['B'])
    assert stats['n_days'] == 2 and stats['delta'] == 0


def test_paired_difference_halves_use_ceil_and_date_order():
    dates = pd.bdate_range('2024-01-01', periods=5)
    baseline = pd.Series(.01, index=dates)
    variant = baseline + pd.Series([.01, .02, .03, -.01, .01], index=dates)
    stats = s.paired_diff(variant.iloc[::-1], baseline)
    assert stats['delta'] == pytest.approx(.012)
    assert stats['dev_delta'] == pytest.approx(.02)
    assert stats['holdout_delta'] == pytest.approx(0.)
    assert stats['halves']['dev']['n_days'] == 3
    assert stats['halves']['holdout']['date_range'] == [str(dates[3].date()), str(dates[4].date())]


@pytest.mark.parametrize('lag', [0, 4, 9, 19])
def test_nw_se_matches_existing_nw_t(lag):
    rng = np.random.default_rng(14)
    noise = rng.normal(size=220)
    x = .03 + noise + np.roll(noise, 1) * .7
    x[17] = np.nan
    clean = x[np.isfinite(x)]
    assert float(clean.mean()) / s.nw_se(x, lag) == pytest.approx(evaluate.nw_t(x, lag), rel=1e-13)
    # A centered sample still has a well-defined positive standard error.
    centered = clean - clean.mean()
    assert s.nw_se(centered, lag) > 0
    assert float(centered.mean()) / s.nw_se(centered, lag) == pytest.approx(evaluate.nw_t(centered, lag), abs=1e-14)


def test_nw_short_and_constant_series_conventions():
    assert s.nw_se([1.] * 19, 19) is None
    for value in (1., -1.):
        series = np.full(100, value)
        assert value / s.nw_se(series, 19) == evaluate.nw_t(series, 19)
    assert s.nw_se(np.zeros(100), 19) == 0


def test_verdict_tiers_thresholds_and_both_halves():
    assert s.verdict(statistics(n_days=99), 1)['verdict'] == 'invalid'
    assert s.verdict(statistics(), 1)['verdict'] == 'strong'
    assert s.verdict(statistics(), 2)['verdict'] == 'screen_pass'
    assert s.verdict(statistics(z=1.), 1)['verdict'] == 'screen_pass'
    assert s.verdict(statistics(z=.999), 1)['verdict'] == 'fail'
    for change in (dict(dev_delta=0.), dict(holdout_delta=-.001), dict(dev_delta=None), dict(z=None)):
        assert s.verdict(statistics(z=9., **change) if 'z' not in change else statistics(**change), 1)['verdict'] == 'fail'
    boundary = NormalDist().inv_cdf(1 - .05 / 2)
    assert s.verdict(statistics(z=boundary, n_days=100), 1)['verdict'] == 'strong'
    assert s.verdict(statistics(), 2)['z_K'] == NormalDist().inv_cdf(1 - .05 / 4)
    with pytest.raises(ValueError):
        s.verdict(statistics(), 0)


def test_input_hash_concatenates_bytes_in_filename_order(local_root):
    (local_root / 'Z.json').write_bytes(b'last')
    (local_root / 'A.json').write_bytes(b'first')
    (local_root / 'ignore.txt').write_bytes(b'ignored')
    actual = hashlib.sha256(b'firstlast').hexdigest()
    with pytest.raises(ValueError, match='sha256 mismatch'):
        s.guard_inputs(local_root)
    assert s.guard_inputs(local_root, True)[0] == actual
    with patch.object(s, 'EXPECTED_HASH', actual):
        assert s.guard_inputs(local_root) == (actual, {})


def test_cli_hash_refusal_override_and_reports_without_ledger_writes(local_root, capsys):
    data = synthetic_inputs(local_root)
    out, ledger = local_root / 'out', local_root / 'never_created.jsonl'
    before = {p.name: p.read_bytes() for p in data.glob('*.json')}
    with patch.object(s, 'DEFAULT_LEDGER', ledger):
        with pytest.raises(SystemExit) as error:
            s.main(['run', '--data', str(data), '--out', str(out)])
        assert error.value.code == 2
        assert not out.exists()
        assert s.main(['run', '--data', str(data), '--out', str(out), '--allow-input-drift']) == 0
    assert not ledger.exists()
    report = json.loads((out / 'report.json').read_text())
    assert report['allow_input_drift'] and report['input_drift']
    assert report['universe']['size'] == 12
    assert report['primary']['V1']['expected']['K'] == 1
    assert report['primary']['V2']['expected']['K'] == 2
    assert report['secondary'].keys() == {'rank_ic', 'no_sma200', 'placebo', 'turnover', 'absolute_returns'}
    assert len(report['secondary']['placebo']['mean_deltas']) == 200
    for signal, horizons in report['secondary']['rank_ic'].items():
        for h, stats in horizons.items():
            assert stats['lag'] == int(h) - 1
    text = (out / 'report.md').read_text()
    assert '幸存者' in text and '预期' in text and 'deviations' in text
    assert {p.name: p.read_bytes() for p in data.glob('*.json')} == before
    assert 'expected_K=2' in capsys.readouterr().out


def test_outputs_cannot_write_data_or_outside_worktree(local_root):
    data = local_root / 'data'
    for out, message in ((data / 'out', 'read-only'), (Path('/tmp/rangerev_out'), 'inside this worktree')):
        with pytest.raises(ValueError, match=message):
            s.validate_outputs(data, out)


def test_load_excludes_all_eleven_and_truncates_at_frozen_end(local_root):
    data = synthetic_inputs(local_root, n=300, names=1)
    rows = json.loads((data / 'S00.json').read_text())
    rows.append(['2026-11-03', 1., 2., .5, 999999., 100., 1.])
    (data / 'S00.json').write_text(json.dumps(rows))
    for ticker in s.EXCLUDED:
        (data / f'{ticker}.json').write_text(json.dumps(rows[:-1]))
    panels = s.load_panels(data, frozen=False)
    assert list(panels['close'].columns) == ['S00']
    assert panels['close'].index.max() < pd.Timestamp('2026-11-03')
    with pytest.raises(ValueError, match='101 files'):
        s.load_panels(data)


def test_record_locked_k_track_isolation_verdict_recomputed_and_duplicates(local_root):
    ledger, report_path = local_root / 'ledger.jsonl', local_root / 'report.json'
    evolution_ledger.append_event(ledger, 'crypto_llm', lambda *_: dict(
        track='crypto_llm', stage='S1', change_type='E', baseline_version='other',
        idea_id='other-track', verdict='strong'))
    report = synthetic_report()
    # Stale dry-run verdicts must never determine the recorded verdict or K.
    report['primary']['V1']['expected'] = dict(K=99, verdict='fail', z_K=99)
    report_path.write_text(json.dumps(report))
    recorded = s.record_report(report_path, ledger)
    assert [row['K'] for row in recorded] == [1, 2]
    assert [row['track'] for row in recorded] == ['us_llm', 'us_llm']
    assert [row['idea_id'] for row in recorded] == ['us-range-pos-v1', 'us-mom6-reversal-v1']
    assert [row['verdict'] for row in recorded] == ['strong', 'screen_pass']
    assert recorded[0]['z_K'] == NormalDist().inv_cdf(1 - .05 / 2)
    assert recorded[1]['z_K'] == NormalDist().inv_cdf(1 - .05 / 4)
    for row in recorded:
        assert row['stage'] == 'S1' and row['change_type'] == 'E'
        assert row['prereg_path'] == s.PREREG and row['baseline_version'] == s.BASELINE_VERSION
        for field in ('delta', 'se', 'z', 'z_K', 'dev_delta', 'holdout_delta', 'n_days',
                      'trade_ratio', 'placebo_pct', 'input_hashes'):
            assert field in row
    before = ledger.read_bytes()
    with pytest.raises(ValueError, match='duplicate'):
        s.record_report(report_path, ledger)
    assert ledger.read_bytes() == before
    assert len(evolution_ledger.read_rows(ledger)) == 3


def test_dry_k_matches_record_and_invalid_does_not_consume_k(local_root):
    report = synthetic_report()
    report['primary']['V1']['n_days'] = 99
    s.dry_verdicts(report, [])
    assert report['primary']['V1']['expected']['K'] is None
    assert report['primary']['V2']['expected']['K'] == 1
    path = local_root / 'report.json'
    path.write_text(json.dumps(report))
    rows = s.record_report(path, local_root / 'ledger.jsonl')
    assert [row['K'] for row in rows] == [None, 1]
    for arm, row in zip(s.VARIANTS, rows):
        assert report['primary'][arm]['expected']['verdict'] == row['verdict']
        assert report['primary'][arm]['expected']['z_K'] == row['z_K']


def test_record_duplicate_guard_also_runs_inside_append_lock(local_root):
    report_path, ledger = local_root / 'report.json', local_root / 'ledger.jsonl'
    report = synthetic_report()
    report_path.write_text(json.dumps(report))
    original = evolution_ledger.append_event

    def competing_append(path, track, build):
        original(path, track, lambda k, _: s.ledger_row(report, 'V1', k))
        return original(path, track, build)

    with patch.object(evolution_ledger, 'append_event', side_effect=competing_append):
        with pytest.raises(ValueError, match='duplicate'):
            s.record_report(report_path, ledger)
    assert len(evolution_ledger.read_rows(ledger)) == 1


def test_turnover_and_absolute_pool_convention():
    dates = pd.bdate_range('2024-01-01', periods=3)
    selections = {dates[0]: list('ABCDE'), dates[1]: list('ABCDE'), dates[2]: list('ABCFG')}
    result = s.turnover(selections)
    assert result['n_transitions'] == 2
    assert result['mean'] == pytest.approx(.2)
    assert str(dates[0].date()) not in result['daily']


def test_placebo_deterministic_and_rank_ic_unrestricted():
    panel = selection_panel()
    for j, ticker in enumerate(panel['fwd20'].columns):
        panel['fwd20'][ticker] = j / 100
        panel['range_pos'][ticker] = j
    panel['fwd5'] = panel['fwd20'].copy()
    panel['fwd10'] = panel['fwd20'].copy()
    panel['mom6'] = panel['range_pos'].copy()
    returns, _, eligible = s.select_groups(panel)
    variants = {arm: s.paired_diff(returns[arm], returns['B']) for arm in s.VARIANTS}
    first = s.placebo_study(panel, eligible, returns['B'], variants, lambda _: None)
    second = s.placebo_study(panel, eligible, returns['B'], variants, lambda _: None)
    assert first == second
    assert len(first['mean_deltas']) == 200
    stats = s.rank_ic_study(panel)['range_pos']['20']
    assert stats['n_days'] == 3  # Includes the date with only nine primary-eligible names.
    assert stats['mean'] == pytest.approx(1.)
