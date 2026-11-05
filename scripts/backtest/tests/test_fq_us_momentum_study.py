"""Synthetic-only tests for the preregistered native Backtrader S1 screen."""
import hashlib
import json
import math

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import evolution_ledger
from scripts.backtest import fq_us_momentum_study as study
from scripts.factor_mining.quant_targets import _capped_invvol


def frame(prices, dates=None, volume=5_000_000.0):
    dates = pd.bdate_range('2020-01-01', periods=len(prices)) if dates is None else dates
    return pd.DataFrame(dict(open=prices, high=prices, low=prices, close=prices,
                             volume=volume), index=dates, dtype=float)


def panel(n=20, days=310):
    dates = pd.bdate_range('2020-01-01', periods=days)
    t = np.arange(days)
    frames = {f'S{i:02}': frame(20 * np.exp(0.0002 * (i + 1) * t)
                                  * (1 + 0.01 * np.sin(t)), dates) for i in range(n)}
    return frames, sorted(frames), dates


def test_signal_formula_and_no_lookahead():
    frames, names, dates = panel()
    at = dates[270]
    features = study.build_features(frames, names, dates)
    expected = frames[names[0]].close.iloc[249] / frames[names[0]].close.iloc[18] - 1
    assert features['signal'].loc[at, names[0]] == pytest.approx(expected)
    original = study.decision_at(features, at)
    changed = {s: f.copy() for s, f in frames.items()}
    for f in changed.values():
        f.loc[f.index > at, ['open', 'high', 'low', 'close']] *= 1000
        f.loc[f.index > at, 'volume'] = 0
    altered = study.build_features(changed, names, dates)
    for key in features:
        pd.testing.assert_frame_equal(features[key].loc[:at], altered[key].loc[:at])
    assert study.decision_at(altered, at) == original
    truncated = study.build_features({s: f.loc[:at] for s, f in frames.items()}, names, dates[dates <= at])
    assert study.decision_at(truncated, at) == original
    # The skipped most recent month cannot enter the momentum numerator.
    recent = {s: f.copy() for s, f in frames.items()}
    recent[names[0]].loc[dates[250:271], ['open', 'high', 'low', 'close']] *= 2
    assert study.build_features(recent, names, dates)['signal'].loc[at, names[0]] == expected


def test_eligibility_inclusive_boundaries_and_invalid_bars():
    dates = pd.bdate_range('2020-01-01', periods=254)
    frames = {s: frame(np.full(254, 5.0), dates, volume=2_000_000.0)
              for s in ('good', 'young', 'cheap', 'illiquid', 'invalid', 'missing')}
    frames['young'] = frames['young'].iloc[2:]
    frames['cheap'].loc[:, ['open', 'high', 'low', 'close']] = 4.99
    frames['illiquid']['volume'] = 1_999_999.0
    frames['invalid'].iloc[:2, frames['invalid'].columns.get_loc('high')] = 1.0
    frames['missing'].iloc[-1] = np.nan
    features = study.build_features(frames, sorted(frames), dates)
    assert not features['eligible'].loc[dates[251], 'good']
    assert features['eligible'].loc[dates[252], 'good']
    assert features['eligible'].loc[dates[-1]].to_dict() == {
        s: s == 'good' for s in sorted(frames)}
    assert features['valid_count'].loc[dates[-1], 'invalid'] == 252
    assert features['median_dollar_volume'].loc[dates[-1], 'good'] == 10_000_000


@pytest.mark.parametrize('n,expected', [(14, 0), (15, 3), (19, 3), (20, 4), (90, 18)])
def test_quintile_counts_and_caps(n, expected):
    frames, names, dates = panel(n)
    decision = study.decision_at(study.build_features(frames, names, dates), dates[-1])
    assert decision['eligible_count'] == n
    assert decision['quintile_count'] == expected
    assert len(decision['long']) == len(decision['short']) == expected
    assert not set(decision['long']) & set(decision['short'])
    assert all(0 < w <= 0.20 for w in decision['long'].values())
    assert all(-0.20 <= w < 0 for w in decision['short'].values())
    assert all(0 < w <= 0.10 for w in decision['L'].values())
    if expected:
        assert set(decision['long']) == set(names[-expected:])
        assert set(decision['short']) == set(names[:expected])
        assert sum(decision['long'].values()) == pytest.approx(min(1, expected * 0.20))


def test_invvol_cap_equivalence():
    for values in ([0.1, 0.2, 0.3], [0.001, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6],
                   np.geomspace(1e-20, 0.9, 18)):
        vol = pd.Series(values)
        original = _capped_invvol(vol, 0.5, 0.1) * 2
        sleeve = _capped_invvol(vol, 1.0, 0.2)
        np.testing.assert_allclose(sleeve, original, rtol=0, atol=1e-14)
        assert sleeve.max() <= 0.2
        assert sleeve.sum() == pytest.approx(min(1.0, len(vol) * 0.2))


def test_rebalance_clock_flat_when_insufficient_and_complete_periods():
    frames, names, dates = panel(days=310)
    features = study.build_features(frames, names, dates)
    features['eligible'].loc[dates[272], :] = False
    sessions, decisions = study.rebalance_schedule(features, dates)
    assert sessions[0] == dates[252]
    assert sessions[-1] == dates[292]
    assert [r['date'] for r in decisions] == [dates[i].date().isoformat() for i in (252, 272, 292)]
    assert decisions[1]['long'] == decisions[1]['short'] == {}
    curve = [dict(date=d.date().isoformat(), equity=100_000 + i) for i, d in enumerate(sessions)]
    periods = study.period_table(curve, decisions)
    assert len(periods) == 2
    assert periods[0]['net_return'] == pytest.approx(100_020 / 100_000 - 1)
    assert periods[1]['net_return'] == pytest.approx(100_040 / 100_020 - 1)


def test_native_long_sleeve_close_fills_and_hand_pnl():
    dates = pd.bdate_range('2024-01-02', periods=3)
    frames = {'A': frame([10, 11, 12], dates), 'B': frame([20, 19.5, 19], dates)}
    # Open prices are deliberately unrelated to close prices.
    for f in frames.values():
        f['open'] = f.close * 3
        f['high'] = f.open
    targets = {dates[0].date().isoformat(): {'A': 0.4, 'B': 0.4}, dates[-1].date().isoformat(): {}}
    run = study.run_sleeve(frames, dates, targets, cash=1000, commission=0.0013)
    expected = 1000 + 40 * (12 - 10) + 20 * (19 - 20) - (800 + 860) * 0.0013
    assert run['engine'] == 'backtrader'
    assert run['equity_curve'][0]['equity'] == pytest.approx(1000 - 800 * 0.0013, abs=1e-6)
    assert run['equity_curve'][1]['equity'] == pytest.approx(1000 + 30 - 800 * 0.0013, abs=1e-6)
    assert run['metrics']['final_equity'] == pytest.approx(expected, abs=1e-6)
    assert run['metrics']['commission'] == pytest.approx(1660 * 0.0013, abs=1e-6)
    assert len(run['fills']) == 4
    assert run['fills'][0]['execution_time'] == dates[0].isoformat()
    assert run['fills'][-1]['execution_time'] == dates[-1].isoformat()
    assert run['equity_curve'][-1]['positions'] == {}


def test_native_shorts_profit_when_prices_fall_including_cover_cost():
    dates = pd.bdate_range('2024-01-02', periods=3)
    frames = {'A': frame([10, 9, 8], dates), 'B': frame([20, 18, 15], dates)}
    targets = {dates[0].date().isoformat(): {'A': -0.4, 'B': -0.4}, dates[-1].date().isoformat(): {}}
    run = study.run_sleeve(frames, dates, targets, cash=1000, commission=0.0007, short=True)
    expected = 1000 + 40 * (10 - 8) + 20 * (20 - 15) - (800 + 620) * 0.0007
    assert run['metrics']['final_equity'] == pytest.approx(expected, abs=1e-6)
    assert run['equity_curve'][1]['equity'] > run['equity_curve'][0]['equity']
    assert run['equity_curve'][0]['short_notional'] == pytest.approx(800)
    assert run['equity_curve'][1]['short_notional'] == pytest.approx(720)
    assert run['equity_curve'][-1]['positions'] == {}


def test_long_cash_bound_and_native_sell_before_buy():
    dates = pd.bdate_range('2024-01-02', periods=3)
    frames = {s: frame([10, 10, 10], dates) for s in ('A', 'B', 'C')}
    targets = {dates[0].date().isoformat(): {'C': 1.0},
               dates[1].date().isoformat(): {'A': 0.5, 'B': 0.5}}
    run = study.run_sleeve(frames, dates, targets, cash=1000, commission=0.0013)
    assert all(row['cash'] >= -1e-7 for row in run['equity_curve'])
    assert run['cash_size_adjustments']
    assert run['equity_curve'][1]['positions'].keys() == {'A', 'B'}
    fills = [f for f in run['fills'] if f['date'] == dates[1].date().isoformat()]
    assert fills[0]['symbol'] == 'C' and fills[0]['size'] < 0
    assert all(f['size'] > 0 for f in fills[1:])
    # Canceled cash-constrained remainders cannot fill on a later day.
    assert not [f for f in run['fills'] if f['date'] == dates[2].date().isoformat()]


def test_short_rebalance_uses_own_equity_not_initial_cash():
    dates = pd.bdate_range('2024-01-02', periods=3)
    frames = {'A': frame([10, 8, 8], dates), 'B': frame([10, 10, 10], dates)}
    targets = {dates[0].date().isoformat(): {'A': -0.5},
               dates[1].date().isoformat(): {'B': -0.5}}
    run = study.run_sleeve(frames, dates, targets, cash=1000, commission=0.0007, short=True)
    second = run['equity_curve'][1]['positions']
    assert set(second) == {'B'}
    assert second['B']['value'] == pytest.approx(-0.5 * (1100 - 500 * 0.0007), abs=1e-6)


def test_z_halves_and_verdict():
    values = [0.01, 0.02, 0.03, 0.04, 0.05]
    result = study.period_statistics(values)
    assert result['z'] == pytest.approx(np.mean(values) / (np.std(values, ddof=1) / math.sqrt(5)))
    assert result['halves'] == dict(first=dict(n=2, mean=0.015), second=dict(n=3, mean=0.04))
    assert result['verdict'] == 'screen_pass'
    assert study.period_statistics([-0.01, -0.01, 0.3, 0.3])['verdict'] == 'fail'
    assert study.period_statistics([-0.1, 0.11, -0.1, 0.11])['verdict'] == 'fail'
    assert study.period_statistics([0.0] * 4)['verdict'] == 'fail'
    assert study.period_statistics([])['verdict'] == 'fail'
    assert study.period_statistics([0.1])['verdict'] == 'fail'
    assert study.period_statistics([0.1] * 4)['verdict'] == 'screen_pass'
    assert study.period_statistics([-0.1] * 4)['verdict'] == 'fail'
    # For two periods, [0, x] gives z=1 but the first half is not positive.
    assert study.period_statistics([0, 0.02])['z'] == pytest.approx(1)
    assert study.period_statistics([0, 0.02])['verdict'] == 'fail'
    with pytest.raises(ValueError, match='finite'):
        study.period_statistics([np.nan])


def synthetic_hash_snapshot(tmp_path):
    (tmp_path / 'daily').mkdir()
    (tmp_path / 'daily' / 'Z.json').write_bytes(b'z')
    (tmp_path / 'daily' / 'A.json').write_bytes(b'a')
    (tmp_path / study.TYPE_FILE).write_bytes(b'types')
    (tmp_path / study.MAP_FILE).write_bytes(b'mapping')
    return {'daily': hashlib.sha256(b'az').hexdigest(),
            study.TYPE_FILE: hashlib.sha256(b'types').hexdigest(),
            study.MAP_FILE: hashlib.sha256(b'mapping').hexdigest()}


@pytest.mark.parametrize('which', ['daily', study.TYPE_FILE, study.MAP_FILE])
def test_hash_guard_sorted_content_and_all_three_inputs(tmp_path, monkeypatch, which):
    expected = synthetic_hash_snapshot(tmp_path)
    monkeypatch.setattr(study, 'EXPECTED_HASHES', expected)
    assert study.guard_hashes(tmp_path) == (expected, {})
    path = tmp_path / ('daily/A.json' if which == 'daily' else which)
    path.write_bytes(path.read_bytes() + b'changed')
    with pytest.raises(ValueError, match='hash mismatch'):
        study.guard_hashes(tmp_path)
    actual, mismatch = study.guard_hashes(tmp_path, True)
    assert set(mismatch) == {which}
    assert actual[which] != expected[which]


def test_ledger_row_append_only(tmp_path):
    path = tmp_path / 'ledger.jsonl'
    base = dict(stage='S1', change_type='E', idea_id='older',
                baseline_version='fq_us:no_validated_factors', verdict='fail')
    for _ in range(2):
        evolution_ledger.append_event(path, 'fq_us', lambda k, previous: base)
    original = path.read_bytes()
    report = dict(z=1.2, n=5, verdict='screen_pass',
                  halves=dict(first=dict(n=2, mean=0.01), second=dict(n=3, mean=0.02)),
                  legs={s: dict(metrics=dict(total_return=0.1)) for s in ('long', 'short')},
                  input_hashes={'daily': 'synthetic'}, deviations=[])
    row = study.append_ledger(path, report)
    assert path.read_bytes().startswith(original)
    assert row['K'] == row['preregistered_K'] == 3
    assert row['track'] == 'fq_us'
    assert row['stage'] == 'S1' and row['change_type'] == 'E'
    assert row['idea_id'] == 'fq-us-momentum-ls-v1'
    assert row['baseline_version'] == 'fq_us:no_validated_factors'
    assert row['verdict'] == 'screen_pass' and row['z'] == 1.2
    assert row['halves'] == report['halves']
    assert row['legs']['short']['total_return'] == 0.1
    assert row['input_hashes'] == report['input_hashes']
    assert row['prereg_path'] == study.PREREG_PATH
    assert len(path.read_text().splitlines()) == 3


def test_funding_stress_calendar_days_and_no_feedback():
    longs = [dict(date=d, equity=50000) for d in ('2024-01-05', '2024-01-08')]
    shorts = [dict(date=d, equity=50000, short_notional=40000) for d in ('2024-01-05', '2024-01-08')]
    result = study.funding_stress(longs, shorts)
    assert result['total_funding'] == pytest.approx(40000 * 0.1 * 3 / 365)
    assert result['equity_curve'][-1]['equity'] == pytest.approx(100000 - result['total_funding'])
    assert shorts[-1]['equity'] == 50000
    assert result['descriptive_only']


def test_metrics_initial_cost_year_boundaries_drawdown_and_turnover():
    curve = [dict(date='2023-12-29', equity=99), dict(date='2024-01-02', equity=110),
             dict(date='2024-01-03', equity=88)]
    fills = [dict(date='2023-12-29', notional=50, commission=1),
             dict(date='2024-01-02', notional=99, commission=0.1)]
    result = study.path_metrics(curve, 100, fills)
    assert result['yearly_returns'] == pytest.approx({'2023': -0.01, '2024': 88 / 99 - 1})
    assert result['max_drawdown'] == pytest.approx(0.2)
    assert result['turnover'] == pytest.approx(1.5)
    assert result['commission'] == 1.1


def test_output_paths_stay_in_out(tmp_path):
    assert study.output_paths(tmp_path)[1] == tmp_path / 'ledger.jsonl'
    with pytest.raises(ValueError, match='inside --out'):
        study.output_paths(tmp_path, tmp_path.parent / 'outside.jsonl')
    with pytest.raises(ValueError, match='read-only'):
        study.output_paths('/Users/zhangyun/workspace/nofx/data/forbidden')


def test_cli_hash_failure_writes_nothing(tmp_path):
    snapshot = tmp_path / 'input'
    snapshot.mkdir()
    synthetic_hash_snapshot(snapshot)
    out = tmp_path / 'out'
    with pytest.raises(ValueError, match='hash mismatch'):
        study.main(['--snapshot', str(snapshot), '--out', str(out)])
    assert not out.exists()


def test_synthetic_cli_reports_benchmarks_ledger_and_readonly_inputs(tmp_path, monkeypatch):
    snapshot, out = tmp_path / 'input', tmp_path / 'out'
    (snapshot / 'daily').mkdir(parents=True)
    frames, names, dates = panel(n=90, days=295)
    # A recent listing remains in the fixed universe, but not in the first
    # rebalance's eligible set or the initial buy-and-hold purchases.
    frames[names[-1]] = frames[names[-1]].iloc[280:]
    frames['SPY'] = frame(100 + np.arange(len(dates)) * 0.1, dates)
    types = {s: 'EQUITY' for s in names}
    types['SPY'] = 'ETF'
    (snapshot / study.TYPE_FILE).write_text(json.dumps(types))
    mapping = dict(symbols=[dict(underlying=s, perp_symbol=s + 'USDT') for s in names[:-2] + names[-1:]])
    (snapshot / study.MAP_FILE).write_text(json.dumps(mapping))
    for s, f in frames.items():
        rows = [[d.date().isoformat(), *map(float, row), float(row[3])]
                for d, row in zip(f.index, f.to_numpy(), strict=True)]
        (snapshot / 'daily' / f'{s}.json').write_text(json.dumps(rows))
    before = {p: p.read_bytes() for p in snapshot.rglob('*.json')}
    monkeypatch.setattr(study, 'EXPECTED_HASHES', study.input_hashes(snapshot))
    assert study.main(['--snapshot', str(snapshot), '--out', str(out)]) == 0
    report = json.loads((out / 'report.json').read_text())
    text = (out / 'report.md').read_text()
    assert report['n'] == 2
    assert report['start'] == dates[252].date().isoformat()
    assert report['end'] == dates[292].date().isoformat()
    assert report['trailing_sessions_excluded'] == 2
    assert 'SPY' not in report['universe'] and len(report['universe']) == 90
    assert report['perp_mapping'][names[-2]] is None
    assert any('没有 perp_symbol' in d for d in report['deviations'])
    assert report['benchmarks']['EW90']['unpurchasable_at_start'] == [names[-1]]
    assert report['benchmarks']['EW90']['reserved_cash_weight'] == pytest.approx(1 / 90)
    assert len(report['benchmarks']['EW90']['fills']) == 89
    assert len(report['benchmarks']['SPY']['fills']) == 1
    assert report['L']['descriptive_only']
    for i, row in enumerate(report['equity_curve']):
        assert row['equity'] == pytest.approx(sum(report['legs'][s]['equity_curve'][i]['equity']
                                                for s in ('long', 'short')))
    stats = study.period_statistics([r['net_return'] for r in report['period_table']])
    assert report['verdict'] == stats['verdict'] and report['z'] == stats['z']
    assert '判定：**' + report['verdict'] + '**' in text
    assert report['ledger_row']['K'] == 1 and report['ledger_row']['preregistered_K'] == 3
    assert report['ledger_row']['deviations'] == report['deviations']
    assert len((out / 'ledger.jsonl').read_text().splitlines()) == 1
    assert {p: p.read_bytes() for p in before} == before
