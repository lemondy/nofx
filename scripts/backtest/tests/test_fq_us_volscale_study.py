"""Synthetic-only checks of the frozen volatility scaling S1 specification."""
import copy
import hashlib
import json
import math

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import evolution_ledger
from scripts.backtest import fq_us_volscale_study as study


def frame(prices, dates):
    return pd.DataFrame(dict(open=prices, high=prices, low=prices, close=prices,
                             volume=5_000_000.0), index=dates, dtype=float)


def decision(date, long=None, short=None):
    return dict(date=str(date.date()), long=long or {}, short=short or {})


def test_unscaled_holdings_drift_by_hand_and_rebalance_after_close():
    dates = pd.bdate_range('2020-01-01', periods=5)
    close = pd.DataFrame(dict(A=[10, 12, 15, 15, 18], B=[20, 18, 24, 12, 12]), index=dates)
    decisions = [decision(dates[0], {'A': 1}, {'B': -1}),
                 decision(dates[2], {'B': 1}, {'A': -1})]
    result = study.unscaled_returns(close, decisions)
    # Initial cash is 1, plus 0.05 A and minus 0.025 B. Day 2 still uses
    # these drifting holdings, not freshly balanced half-weight daily returns.
    assert math.isnan(result.iloc[0])
    assert result.iloc[1] == pytest.approx(1.15 - 1)
    assert result.iloc[2] == pytest.approx(1.15 / 1.15 - 1)
    # Reset at day 2: 0.575 B long and 0.575 A short, on equity 1.15.
    assert result.iloc[3] == pytest.approx(0.8625 / 1.15 - 1)
    assert result.iloc[4] == pytest.approx(0.7475 / 0.8625 - 1)
    assert result.iloc[2] != pytest.approx(0.5 * (15 / 12 - 1) - 0.5 * (24 / 18 - 1))


def test_unscaled_sparse_targets_cash_missing_marks_and_flat_period():
    dates = pd.bdate_range('2020-01-01', periods=4)
    close = pd.DataFrame(dict(A=[10, np.nan, 12, 100], B=[20, 18, 16, 1]), index=dates)
    result = study.unscaled_returns(close, [decision(dates[0], {'A': 0.4}, {'B': -0.4}),
                                            decision(dates[2])])
    assert result.iloc[1] == pytest.approx(0.02)
    assert result.iloc[2] == pytest.approx(1.08 / 1.02 - 1)
    assert result.iloc[3] == pytest.approx(0)


def test_sigma_126_inclusive_sample_window_and_reference_252_boundary():
    dates = pd.bdate_range('2020-01-01', periods=420)
    returns = pd.Series(np.sin(np.arange(420) * 0.4) * 0.02, index=dates)
    returns.iloc[0] = np.nan
    signal = study.volatility_signal(returns)
    assert signal.sigma_hat.iloc[:126].isna().all()
    assert signal.k.iloc[:126].eq(1).all()
    assert signal.sigma_hat.iloc[126] == pytest.approx(returns.iloc[1:127].std(ddof=1))
    assert signal.sigma_hat.iloc[127] == pytest.approx(returns.iloc[2:128].std(ddof=1))
    assert signal.sigma_ref.iloc[:377].isna().all()
    assert signal.k.iloc[:377].eq(1).all()
    assert signal.sigma_observations.iloc[376] == 251
    assert signal.sigma_observations.iloc[377] == 252
    assert signal.sigma_ref.iloc[377] == pytest.approx(signal.sigma_hat.iloc[126:378].median())


def test_expanding_daily_median_is_causal_and_k_is_capped():
    dates = pd.bdate_range('2020-01-01', periods=750)
    t = np.arange(len(dates))
    returns = pd.Series(np.sin(t) * np.where(t < 450, 0.01, 0.08), index=dates)
    original = study.volatility_signal(returns)
    for i in (376, 450, 600, 749):
        expected = original.sigma_hat.iloc[:i + 1].dropna().median()
        assert original.sigma_ref.iloc[i] == pytest.approx(expected)
        assert original.k.iloc[i] == pytest.approx(min(1, expected / original.sigma_hat.iloc[i]))
    assert original.k.between(0, 1).all()
    assert original.k.iloc[600] < 0.5
    changed = returns.copy()
    changed.iloc[601:] = np.cos(t[601:]) * 50
    pd.testing.assert_frame_equal(original.iloc[:601], study.volatility_signal(changed).iloc[:601])
    pd.testing.assert_frame_equal(original.iloc[:601], study.volatility_signal(returns.iloc[:601]))
    falling = pd.Series(np.sin(t) * np.where(t < 450, 0.1, 0.001), index=dates)
    capped = study.volatility_signal(falling)
    assert capped.sigma_ref.iloc[-1] > capped.sigma_hat.iloc[-1]
    assert capped.k.iloc[-1] == 1
    assert study.volatility_signal(pd.Series(0.0, index=dates)).k.eq(1).all()


def test_scaled_sleeve_targets_sum_to_k_and_caps_scale_without_renormalizing():
    dates = pd.bdate_range('2020-01-01', periods=2)
    decisions = [decision(dates[0], {f'L{i}': 0.2 for i in range(5)},
                          {f'S{i}': -0.2 for i in range(5)}),
                 decision(dates[1], {'L0': 0.2}, {'S0': -0.2})]
    scales = {str(d.date()): k for d, k in zip(dates, (0.35, 0.6), strict=True)}
    for leg, sign in [('long', 1), ('short', -1)]:
        targets = study.scaled_targets(decisions, scales, leg)
        assert sum(targets[str(dates[0].date())].values()) == pytest.approx(sign * 0.35)
        assert max(abs(w) for w in targets[str(dates[0].date())].values()) == pytest.approx(0.2 * 0.35)
        assert sum(abs(w) for w in targets[str(dates[1].date())].values()) == pytest.approx(0.2 * 0.6)
    assert study.scaled_targets(decisions, dict.fromkeys(scales, 0), 'long')[str(dates[0].date())] == dict.fromkeys(decisions[0]['long'], 0)


def passing_datasets():
    return {name: dict(arms=dict(
        B=dict(metrics=dict(sharpe=0.5, max_drawdown=0.3)),
        V=dict(metrics=dict(sharpe=0.5, max_drawdown=0.2), kill_switch=dict(triggered=False))))
        for name in ('D1', 'D2', 'D3')}


@pytest.mark.parametrize('name', ['D1', 'D2', 'D3'])
@pytest.mark.parametrize('metric,value', [('sharpe', 0.49), ('max_drawdown', 0.3),
                                          ('sharpe', None), ('sharpe', float('nan'))])
def test_verdict_requires_each_dataset_and_strict_drawdown(name, metric, value):
    datasets = passing_datasets()
    assert study.paired_verdict(datasets)['verdict'] == 'screen_pass'
    datasets[name]['arms']['V']['metrics'][metric] = value
    assert study.paired_verdict(datasets)['verdict'] == 'fail'


def test_verdict_requires_d1_kill_switch_clear_and_all_three_datasets():
    datasets = passing_datasets()
    datasets['D1']['arms']['V']['kill_switch']['triggered'] = True
    assert study.paired_verdict(datasets)['verdict'] == 'fail'
    datasets['D1']['arms']['V']['kill_switch']['triggered'] = False
    # Only D1 V is gated; B and other datasets' overlays are not criteria.
    datasets['D1']['arms']['B']['kill_switch'] = dict(triggered=True)
    datasets['D2']['arms']['V']['kill_switch']['triggered'] = True
    assert study.paired_verdict(datasets)['verdict'] == 'screen_pass'
    del datasets['D3']
    result = study.paired_verdict(datasets)
    assert result['verdict'] == 'fail' and not result['complete']


def test_hash_guard_sorted_raw_contents_and_failure_before_output(tmp_path, monkeypatch):
    daily = tmp_path / 'daily'
    daily.mkdir()
    (daily / 'Z.json').write_bytes(b'z')
    (daily / 'A.json').write_bytes(b'a')
    (daily / 'ignored.txt').write_bytes(b'ignored')
    monkeypatch.setitem(study.DATASETS, 'D1', dict(daily=daily, spy=daily / 'SPY.json'))
    expected = hashlib.sha256(b'az').hexdigest()
    monkeypatch.setitem(study.EXPECTED_HASHES, 'D1', expected)
    hashes, files, mismatches = study.guard_hashes(['D1'])
    assert hashes == dict(D1=expected) and files == dict(D1=['A.json', 'Z.json'])
    assert not mismatches
    (daily / 'A.json').write_bytes(b'a changed')
    out = tmp_path / 'out'
    with pytest.raises(ValueError, match='hash mismatch'):
        study.main(['--datasets', 'D1', '--out', str(out), '--no-ledger'])
    assert not out.exists()
    actual, _, drift = study.guard_hashes(['D1'], True)
    assert drift == dict(D1=dict(expected=expected, actual=actual['D1']))
    with pytest.raises(ValueError, match='empty'):
        study.daily_hash(tmp_path / 'absent')


def test_output_paths_separate_ledger_and_input_protection(tmp_path, monkeypatch):
    daily = tmp_path / 'daily'
    spy, universe = tmp_path / 'SPY.json', tmp_path / 'universe.json'
    monkeypatch.setattr(study, 'SP500', universe)
    monkeypatch.setitem(study.DATASETS, 'D3', dict(daily=daily, spy=spy))
    out, ledger = study.output_paths(tmp_path / 'study', tmp_path / 'evolution/ledger.jsonl')
    assert out == tmp_path / 'study' and ledger == tmp_path / 'evolution/ledger.jsonl'
    assert study.output_paths(out)[1] == out / 'ledger.jsonl'
    for target in (daily / 'out', daily):
        with pytest.raises(ValueError, match='input daily'):
            study.output_paths(target)
    for target in (daily / 'ledger.jsonl', spy, universe):
        with pytest.raises(ValueError, match='input'):
            study.output_paths(out, target)


def test_ledger_append_only_and_frozen_identity(tmp_path):
    path = tmp_path / 'ledger.jsonl'
    base = dict(stage='S1', change_type='E', idea_id='previous',
                baseline_version='baseline', verdict='fail')
    for _ in range(3):
        evolution_ledger.append_event(path, 'fq_us', lambda k, previous: base)
    before = path.read_bytes()
    datasets = passing_datasets()
    report = dict(datasets=datasets, input_hashes={d: 'synthetic' for d in datasets},
                  input_drift={}, deviations=[], **study.paired_verdict(datasets))
    row = study.append_ledger(path, report)
    assert path.read_bytes().startswith(before)
    assert row['track'] == 'fq_us' and row['stage'] == 'S1' and row['change_type'] == 'C'
    assert row['idea_id'] == 'fq-us-momentum-volscale-v1'
    assert row['baseline_version'] == 'mom_ls_invvol_v1'
    assert row['K'] == row['preregistered_K'] == 4
    assert row['datasets']['D1']['V'] == dict(sharpe=0.5, max_drawdown=0.2, kill_switch_triggered=False)
    assert row['input_hashes'] == report['input_hashes']
    assert json.loads(path.read_text().splitlines()[-1])['verdict'] == 'screen_pass'
    isolated = study.append_ledger(tmp_path / 'isolated.jsonl', copy.deepcopy(report))
    assert isolated['K'] == 1 and isolated['preregistered_K'] == 4
    assert any('K=1' in d for d in isolated['deviations'])


def test_tiny_native_end_to_end_scaling_costs_cash_and_d1_latch():
    dates = pd.bdate_range('2009-03-09', periods=4)
    frames = {'A': frame([10, 11, 12, 12], dates),
              'B': frame([10, 12, 11, 10], dates)}
    decisions = [decision(dates[0], {'A': 1}, {'B': -1}),
                 decision(dates[-1], {'A': 1}, {'B': -1})]
    scales = {d['date']: 0.4 for d in decisions}
    close = pd.DataFrame({s: f.close for s, f in frames.items()})
    signal = study.volatility_signal(study.unscaled_returns(close, decisions))
    arm = study.run_arm(frames, dates, decisions, scales, signal, d1=True)
    assert arm['legs']['long']['engine'] == arm['legs']['short']['engine'] == 'backtrader'
    assert arm['equity_curve'][0]['equity'] == pytest.approx(100000 - 20000 * 0.002)
    assert arm['legs']['long']['equity_curve'][0]['cash'] == pytest.approx(30000 - 26)
    assert arm['legs']['short']['equity_curve'][0]['short_notional'] == pytest.approx(20000)
    assert arm['legs']['long']['equity_curve'][1]['positions']['A']['size'] == pytest.approx(2000)
    assert arm['signal_rebalances'][0]['target_gross_exposure'] == pytest.approx(0.4)
    initial_gross = 0.5 * (20000 / 49974 + 20000 / 49986)
    assert arm['gross_exposure_curve'][0]['gross_exposure'] == pytest.approx(initial_gross)
    assert not arm['kill_switch']['triggered']
    levels = np.array([100000] + [r['equity'] for r in arm['equity_curve']])
    returns = levels[1:] / levels[:-1] - 1
    assert arm['metrics']['sharpe'] == pytest.approx(returns.mean() / returns.std(ddof=1) * np.sqrt(252))
    # A rising short name makes the scaled native path breach the inclusive latch.
    frames['B'] = frame([10, 30, 30, 30], dates)
    failed = study.run_arm(frames, dates, decisions, scales, signal, d1=True)
    assert failed['kill_switch']['triggered']
    assert failed['kill_switch']['triggers'][0]['date'] == str(dates[1].date())


def synthetic_inputs(tmp_path, monkeypatch):
    dates = pd.bdate_range('2005-01-03', periods=295)
    t = np.arange(len(dates))
    names = [f'S{i:02}' for i in range(90)]
    daily = tmp_path / 'daily'
    daily.mkdir()
    for i, name in enumerate(names[:20] + ['SPY']):
        prices = 20 * np.exp(0.0002 * (i + 1) * t) * (1 + 0.01 * np.sin(t))
        rows = [[str(d.date()), p, p, p, p, 5e6, p]
                for d, p in zip(dates, prices, strict=True)]
        (daily / f'{name}.json').write_text(json.dumps(rows))
    frozen, sp500 = tmp_path / 'frozen.json', tmp_path / 'sp500.json'
    frozen.write_text(json.dumps(dict(symbols=names)))
    sp500.write_text(json.dumps(dict(sp500=names[:20] + ['BRK.B'])))
    monkeypatch.setattr(study, 'SP500', sp500)
    monkeypatch.setattr(study.stress, 'FROZEN_UNIVERSE', frozen)
    for name in study.DATASETS:
        monkeypatch.setitem(study.DATASETS, name, dict(daily=daily,
            universe='bstock90' if name == 'D2' else 'sp500', spy=daily / 'SPY.json'))
        monkeypatch.setitem(study.EXPECTED_HASHES, name, study.daily_hash(daily)[0])
    return dates, daily


def test_loaders_universes_yahoo_conversion_and_d3_external_calendar(tmp_path, monkeypatch):
    _, daily = synthetic_inputs(tmp_path, monkeypatch)
    for name in ('D1', 'D2', 'D3'):
        loaded = study.load_dataset(name)
        assert len(loaded['calendar']) == 295
        assert 'SPY' not in loaded['requested']
        assert len(loaded['requested']) == (90 if name == 'D2' else 21)
        if name != 'D2':
            assert 'BRK-B' in loaded['requested']
    external = tmp_path / 'SPY.json'
    external.write_bytes((daily / 'SPY.json').read_bytes())
    (daily / 'SPY.json').unlink()
    monkeypatch.setitem(study.DATASETS, 'D3', dict(daily=daily, universe='sp500', spy=external))
    assert len(study.load_dataset('D3')['calendar']) == 295


def test_synthetic_cli_report_ledger_and_input_readonly(tmp_path, monkeypatch):
    dates, _ = synthetic_inputs(tmp_path, monkeypatch)
    before = {p: p.read_bytes() for p in tmp_path.rglob('*.json')}
    out = tmp_path / 'out'
    assert study.main(['--out', str(out), '--datasets', 'D1',
                       '--ledger', str(out / 'ledger.jsonl')]) == 0
    report = json.loads((out / 'report.json').read_text())
    dataset = report['datasets']['D1']
    assert dataset['start'] == str(dates[252].date())
    assert dataset['end'] == str(dates[292].date())
    assert dataset['n'] == 2 and dataset['trailing_sessions_excluded'] == 2
    assert report['verdict'] == 'fail' and not report['complete']
    assert report['ledger_row']['K'] == 1 and report['preregistered_K'] == 4
    assert len((out / 'ledger.jsonl').read_text().splitlines()) == 1
    for arm in dataset['arms'].values():
        assert len(arm['signal_rebalances']) == 3
        assert len(arm['equity_curve']) == 41
        assert all(r['k'] == 1 for r in arm['signal_rebalances'])
    assert dataset['signal_daily'][0]['u_d'] is None
    text = (out / 'report.md').read_text()
    assert '判定：**fail**' in text and '幸存者偏差' in text and '后处理' in text
    assert {p: p.read_bytes() for p in before} == before
    with pytest.raises(FileExistsError):
        study.main(['--out', str(out), '--datasets', 'D1', '--no-ledger'])
    assert study.main(['--out', str(tmp_path / 'no_ledger'), '--datasets', 'D1', '--no-ledger']) == 0
    assert not (tmp_path / 'no_ledger/ledger.jsonl').exists()
