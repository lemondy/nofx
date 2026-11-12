"""Synthetic-only checks; never invoke the physical-momentum study or CLI."""
import copy
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import evolution_ledger
from scripts.backtest import fq_us_momentum_study as s1
from scripts.backtest import fq_us_physmom_study as study


@pytest.fixture(autouse=True)
def no_study_or_cli(monkeypatch):
    def forbidden(*args, **kwargs):
        pytest.fail('synthetic tests must not invoke run_study, main, or load_dataset')

    monkeypatch.setattr(study, 'run_study', forbidden)
    monkeypatch.setattr(study, 'main', forbidden)
    monkeypatch.setattr(study.volscale, 'load_dataset', forbidden)


def synthetic_panel(days=400, names=20):
    dates = pd.bdate_range('2020-01-01', periods=days)
    price_rng, volume_rng = np.random.default_rng(1949), np.random.default_rng(1950)
    frames = {}
    for i in range(names):
        prices = 30 * np.exp(np.cumsum(price_rng.normal(0.0003 + i * 0.00003, 0.012, days)))
        volume = volume_rng.uniform(2_000_000, 8_000_000, days)
        frames[f'S{i:02}'] = pd.DataFrame(dict(open=prices, high=prices, low=prices,
            close=prices, volume=volume), index=dates)
    features = s1.build_features(frames, sorted(frames), dates)
    volume = pd.DataFrame({s: f.volume for s, f in frames.items()}).reindex(dates)
    return frames, dates, features, volume


@pytest.mark.parametrize('mass', [1.0, 5_000_000.0])
def test_constant_mass_telescopes_and_preserves_rank_decisions_and_weights(mass):
    _, dates, features, volume = synthetic_panel()
    # Feature eligibility still uses the original liquid input panel. Mass=1
    # is a signal-only identity check, not a liquidity-rule modification.
    volume.loc[:, :] = mass
    result = study.physical_momentum(features['close'], volume)
    expected = np.log(features['close'].shift(21) / features['close'].shift(252)) / 231
    np.testing.assert_allclose(result['signal'].iloc[252:], expected.iloc[252:], atol=2e-17, rtol=1e-11)
    assert not result['fallback'].iloc[252:].to_numpy().any()
    sessions_b, baseline = s1.rebalance_schedule(features, dates)
    candidate, sessions_c, decisions = study.candidate_schedule(features, volume, dates, baseline)
    assert candidate is not features and candidate.keys() == features.keys()
    for key in features.keys() - {'signal'}:
        assert candidate[key] is features[key]
    assert sessions_c.equals(sessions_b)
    for b, c in zip(baseline, decisions, strict=True):
        date, names = pd.Timestamp(b['date']), b['eligible']
        assert features['signal'].loc[date, names].rank().to_dict() == candidate['signal'].loc[date, names].rank().to_dict()
        assert b['date'] == c['date'] and b['eligible'] == c['eligible']
        for leg in ('long', 'short', 'L'):
            assert b[leg] == c[leg]
    pd.testing.assert_frame_equal(features['signal'], features['close'].shift(21) / features['close'].shift(252) - 1)


def test_constant_mass_ties_keep_baseline_symbol_order():
    frames, dates, _, volume = synthetic_panel()
    for symbol in ('S00', 'S01', 'S02', 'S03', 'S04'):
        frames[symbol] = frames['S00'].copy()
    features = s1.build_features(frames, sorted(frames), dates)
    volume.loc[:, :] = 1.0
    _, baseline = s1.rebalance_schedule(features, dates)
    _, _, candidate = study.candidate_schedule(features, volume, dates, baseline)
    for b, c in zip(baseline, candidate, strict=True):
        assert b['long'] == c['long'] and b['short'] == c['short']


def tiny_signal_inputs():
    dates = pd.bdate_range('2022-01-03', periods=260)
    returns = 0.001 + np.sin(np.arange(260) * 0.31) * 0.004
    prices = 20 * np.exp(np.cumsum(returns))
    close = pd.DataFrame(dict(A=prices), index=dates)
    volume = pd.DataFrame(dict(A=1 + np.arange(260, dtype=float)), index=dates)
    return close, volume, returns


def test_hand_checked_231_returns_and_exact_skip_no_lookahead():
    close, volume, returns = tiny_signal_inputs()
    t = 259
    lo, hi = t - 251, t - 21
    assert hi - lo + 1 == 231
    expected = np.dot(volume.A.iloc[lo:hi + 1], returns[lo:hi + 1]) / volume.A.iloc[lo:hi + 1].sum()
    original = study.physical_momentum(close, volume)
    assert not original['fallback'].iloc[t, 0]
    assert original['signal'].iloc[t, 0] == pytest.approx(expected, abs=1e-17)
    recent_close, recent_volume = close.copy(), volume.copy()
    recent_close.iloc[t - 20:] *= np.linspace(2, 30, 21)[:, None]
    recent_volume.iloc[t - 20:] *= 1000
    assert study.physical_momentum(recent_close, recent_volume)['signal'].iloc[t, 0] == original['signal'].iloc[t, 0]
    endpoint = close.copy()
    endpoint.iloc[t - 21, 0] *= 1.5
    assert study.physical_momentum(endpoint, volume)['signal'].iloc[t, 0] != pytest.approx(expected)
    old_close, old_volume = close.copy(), volume.copy()
    old_close.iloc[:t - 252] *= 10
    old_volume.iloc[:t - 251] *= 10000
    assert study.physical_momentum(old_close, old_volume)['signal'].iloc[t, 0] == original['signal'].iloc[t, 0]
    start_price = close.copy()
    start_price.iloc[t - 252, 0] *= 2
    assert study.physical_momentum(start_price, volume)['signal'].iloc[t, 0] != pytest.approx(expected)


@pytest.mark.parametrize('mass_days,uses_fallback', [(0, True), (199, True), (200, False), (231, False)])
def test_fallback_mass_count_inclusive_boundary(mass_days, uses_fallback):
    close, volume, _ = tiny_signal_inputs()
    volume.loc[:, :] = 0
    volume.iloc[8:8 + mass_days, 0] = np.arange(1, mass_days + 1)
    result = study.physical_momentum(close, volume)
    returns = np.log(close.A / close.A.shift(1)).iloc[8:239]
    expected = (returns.mean() if uses_fallback else
                np.dot(returns.iloc[:mass_days], volume.A.iloc[8:8 + mass_days]) / volume.A.iloc[8:8 + mass_days].sum())
    assert bool(result['fallback'].iloc[-1, 0]) == uses_fallback
    assert result['signal'].iloc[-1, 0] == pytest.approx(expected, abs=1e-17)
    assert result['fallback'].dtypes.eq(bool).all()
    if mass_days == 0:
        expected_all = np.log(close / close.shift(1)).rolling(231, min_periods=1).mean().shift(21)
        pd.testing.assert_frame_equal(result['signal'], expected_all)
        assert result['fallback'].to_numpy().all()


def test_nonfinite_returns_and_nonpositive_or_nonfinite_volume_have_zero_mass():
    close, volume, _ = tiny_signal_inputs()
    close.iloc[30, 0] = np.nan  # Removes returns at 30 AND 31, never forward fills.
    for at, value in [(50, 0), (51, -100), (52, np.nan), (53, np.inf), (54, -np.inf)]:
        volume.iloc[at, 0] = value
    result = study.physical_momentum(close, volume)
    r = np.log(close.A / close.A.shift(1)).iloc[8:239]
    v = volume.A.iloc[8:239]
    valid = np.isfinite(r) & np.isfinite(v) & v.gt(0)
    expected = (r[valid] * v[valid]).sum() / v[valid].sum()
    assert result['signal'].iloc[-1, 0] == pytest.approx(expected, abs=1e-17)
    assert not result['fallback'].iloc[-1, 0]
    assert np.isfinite(result['signal'].iloc[252:]).all().all()
    missing = study.physical_momentum(close * np.nan, volume)
    assert missing['signal'].isna().all().all() and missing['fallback'].all().all()
    with pytest.raises(ValueError, match='same calendar'):
        study.physical_momentum(close, volume.iloc[1:])


def test_fallback_share_counts_only_eligible_decision_stock_dates_including_terminal():
    dates = pd.bdate_range('2020-01-01', periods=4)
    eligible = pd.DataFrame([[1, 1, 0], [1, 1, 1], [1, 0, 1], [0, 1, 1]],
                            columns=list('ABC'), index=dates, dtype=bool)
    fallback = pd.DataFrame([[1, 0, 1], [1, 1, 1], [0, 1, 1], [1, 1, 0]],
                            columns=list('ABC'), index=dates, dtype=bool)
    decisions = [dict(date=str(d.date())) for d in dates[[0, 2, 3]]]
    assert study.fallback_usage(eligible, fallback, decisions) == dict(
        eligible_stock_dates=6, fallback_stock_dates=3, share=0.5)
    assert study.fallback_usage(eligible & False, fallback, decisions)['share'] is None


def test_candidate_schedule_asserts_baseline_dates():
    _, dates, features, volume = synthetic_panel()
    _, baseline = s1.rebalance_schedule(features, dates)
    bad = copy.deepcopy(baseline)
    bad[-1]['date'] = str(dates[-1].date())
    with pytest.raises(AssertionError, match='decision dates'):
        study.candidate_schedule(features, volume, dates, bad)


def test_placebo_seed_order_bounds_multisets_missing_rows_and_short_history():
    dates = pd.bdate_range('2020-01-01', periods=220)
    close = pd.DataFrame(20.0, columns=['Z', 'SHORT', 'A', 'EMPTY'], index=dates)
    close.loc[dates[::7], 'Z'] = np.nan
    close.loc[dates[126:], 'SHORT'] = np.nan
    close['EMPTY'] = np.nan
    volume = pd.DataFrame({s: 1000 * i + np.arange(220, dtype=float)
                           for i, s in enumerate(close.columns)}, index=dates)
    volume.loc[dates[10], 'Z'] = np.nan
    before = volume.copy()
    first, repeat, oracle = [np.random.default_rng(study.PLACEBO_SEED) for _ in range(3)]
    for _ in range(20):
        result = study.shift_volume(close, volume, first)
        repeated = study.shift_volume(close, volume, repeat)
        pd.testing.assert_frame_equal(result['volume'], repeated['volume'])
        assert result['offsets'] == repeated['offsets']
        assert result['short_histories'] == dict(EMPTY=0, SHORT=126)
        for symbol in sorted(close.columns):
            valid = close[symbol].notna()
            n = int(valid.sum())
            offset = int(oracle.integers(63, n - 63 + 1)) if n >= 127 else 0
            assert result['offsets'][symbol] == offset
            assert n < 127 or 63 <= offset <= n - 63
            np.testing.assert_equal(np.sort(result['volume'].loc[valid, symbol]), np.sort(volume.loc[valid, symbol]))
            np.testing.assert_equal(result['volume'].loc[valid, symbol], np.roll(volume.loc[valid, symbol], offset))
            pd.testing.assert_series_equal(result['volume'].loc[~valid, symbol], volume.loc[~valid, symbol])
    pd.testing.assert_frame_equal(volume, before)


@pytest.mark.parametrize('n', [127, 200])
@pytest.mark.parametrize('upper', [False, True])
def test_placebo_inclusive_offset_endpoints(n, upper):
    class EndpointRng:
        def integers(self, low, high):
            assert low == 63 and high == n - 63 + 1
            return high - 1 if upper else low

    close = pd.DataFrame(dict(A=np.ones(n)))
    volume = pd.DataFrame(dict(A=np.arange(n, dtype=float)))
    result = study.shift_volume(close, volume, EndpointRng())
    assert result['offsets']['A'] == (n - 63 if upper else 63)


def passing_stats():
    return dict(n=100, mean=0.001, sd=0.01, z=study.Z_K,
                halves=dict(first=dict(n=50, mean=0.001), second=dict(n=50, mean=0.001)))


@pytest.mark.parametrize('field,value,expected', [
    ('z', study.Z_K, 'strong'), ('z', study.Z_K - 1e-9, 'screen_pass'),
    ('z', 1.0, 'screen_pass'), ('z', 1.0 - 1e-9, 'fail'), ('z', None, 'fail'),
    ('z', math.inf, 'strong'), ('z', math.nan, 'fail'),
    ('first', 0.0, 'fail'), ('second', 0.0, 'fail'), ('first', None, 'fail'),
    ('n', 99, 'invalid'), ('fallback_share', 0.05, 'strong'),
    ('fallback_share', 0.05000001, 'invalid'), ('fallback_share', None, 'invalid'),
    ('baseline_reproduced', False, 'invalid'),
    ('placebo_pct', 0.90, 'strong'), ('placebo_pct', 0.89999999, 'fail'),
    ('placebo_pct', None, 'fail'), ('d2_mean_delta', 0.0, 'strong'),
    ('d2_mean_delta', -1e-12, 'fail'), ('d2_mean_delta', None, 'fail'),
])
def test_verdict_frozen_boundaries(field, value, expected):
    stats = passing_stats()
    gates = dict(fallback_share=0.0, baseline_reproduced=True, placebo_pct=0.90, d2_mean_delta=0.0)
    if field in ('first', 'second'):
        stats['halves'][field]['mean'] = value
    elif field in stats:
        stats[field] = value
    else:
        gates[field] = value
    assert study.decide_verdict(stats, **gates) == expected


def test_invalid_precedes_other_gates_and_reproduction_uses_relative_tolerance():
    stats = passing_stats()
    stats.update(n=99, z=-10)
    assert study.decide_verdict(stats, fallback_share=0, baseline_reproduced=True,
                               placebo_pct=0, d2_mean_delta=-1) == 'invalid'
    expected = study.D3_B_EXPECTED_TOTAL
    assert study.baseline_reproduction(expected, expected)['passed']
    assert study.baseline_reproduction(expected * (1 + 0.999e-6), expected)['passed']
    assert not study.baseline_reproduction(expected * (1 + 1.001e-6), expected)['passed']
    # isclose uses max(abs(actual), abs(expected)); the preregistered
    # reproduction error instead always divides by abs(expected).
    near = expected * (1 + 1.0000005e-6)
    assert math.isclose(near, expected, rel_tol=1e-6, abs_tol=0)
    assert not study.baseline_reproduction(near, expected)['passed']
    assert not study.baseline_reproduction(float('nan'), expected)['passed']


def test_placebo_percentile_strict_comparison_and_distribution():
    # Eighteen below C, one tied, one above: ties never count as wins.
    means = [-0.001] * 18 + [0.0, 0.001]
    result = study.placebo_summary(0.0, means)
    assert result['placebo_pct'] == 0.90
    assert result['distribution']['n'] == 20
    assert result['distribution']['mean'] == pytest.approx(np.mean(means))
    assert study.placebo_summary(0.0, [0.0] * 20)['placebo_pct'] == 0
    with pytest.raises(ValueError, match='20 finite'):
        study.placebo_summary(0.0, means[:-1])
    with pytest.raises(ValueError, match='20 finite'):
        study.placebo_summary(0.0, means[:-1] + [np.nan])


def period_rows():
    return [dict(k=k, start=f'2020-0{k}-01', end=f'2020-0{k + 1}-01', trading_days=20,
                 net_return=r) for k, r in enumerate([0.01, -0.03, 0.02], 1)]


@pytest.mark.parametrize('key,value', [('k', 9), ('start', '2020-01-02'), ('end', '2020-03-02'), ('trading_days', 19)])
def test_period_pairing_asserts_mismatched_keys_and_dates(key, value):
    baseline, candidate = period_rows(), period_rows()
    candidate[0][key] = value
    with pytest.raises(AssertionError, match='k and dates'):
        study.paired_periods(baseline, candidate)


def test_period_pairing_sign_statistics_and_count_guard():
    baseline, candidate = period_rows(), period_rows()
    for row, delta in zip(candidate, [0.001, -0.002, 0.004], strict=True):
        row['net_return'] += delta
    stats = study.paired_statistics(baseline, candidate)
    expected = s1.period_statistics([c['net_return'] - b['net_return']
                                     for b, c in zip(baseline, candidate, strict=True)])
    for key in ('n', 'mean', 'sd', 'z', 'halves'):
        assert stats[key] == expected[key]
    assert 'verdict' not in stats
    assert stats['period_table'][0]['delta'] == pytest.approx(0.001)
    with pytest.raises(AssertionError, match='period counts'):
        study.paired_periods(baseline, candidate[:-1])


def test_selection_diagnostics_spearman_ties_overlap_and_empty_period():
    dates = pd.bdate_range('2020-01-01', periods=3)
    eligible = pd.DataFrame(True, columns=list('ABCD'), index=dates)
    eligible.iloc[1] = False
    baseline_signal = pd.DataFrame([[1, 2, 2, 4]] * 3, columns=list('ABCD'), index=dates)
    candidate_signal = pd.DataFrame([[4, 2, 2, 1]] * 3, columns=list('ABCD'), index=dates)
    baseline = [dict(date=str(d.date()), long={'A': 0.2, 'B': 0.2}, short={'C': -0.2, 'D': -0.2})
                for d in dates]
    candidate = [dict(date=str(d.date()), long={'B': 0.2, 'C': 0.2}, short={'A': -0.2, 'D': -0.2})
                 for d in dates]
    for decisions in (baseline, candidate):
        decisions[1].update(long={}, short={})
    result = study.selection_diagnostics(dict(eligible=eligible, signal=baseline_signal),
        dict(signal=candidate_signal), baseline, candidate)
    assert len(result['period_table']) == 2  # Terminal decision has no period.
    assert result['median_spearman'] == pytest.approx(-1.0)
    assert result['median_long_overlap'] == result['median_short_overlap'] == 0.5
    empty = result['period_table'][1]
    assert empty['rank_count'] == 0 and empty['spearman'] is None
    assert empty['long_overlap'] is None and empty['short_overlap'] is None


@pytest.fixture(scope='module')
def native_synthetic_result():
    frames, dates, features, volume = synthetic_panel()
    sessions, baseline = s1.rebalance_schedule(features, dates)
    candidate, sessions_c, decisions = study.candidate_schedule(features, volume, dates, baseline)
    assert sessions.equals(sessions_c)
    arms = dict(B=study.run_arm_unscaled(frames, sessions, baseline, label='synthetic B'),
                C=study.run_arm_unscaled(frames, sessions, decisions, label='synthetic C'))
    comparison = study.compare_arms(arms['B'], arms['C'], baseline)
    selection = study.selection_diagnostics(features, candidate, baseline, decisions)
    fallback = study.fallback_usage(features['eligible'],
        study.physical_momentum(features['close'], volume)['fallback'], decisions)
    return dict(arms=arms, comparison=comparison, selection=selection, fallback=fallback)


def test_native_synthetic_end_to_end_independent_volume_finite_delta(native_synthetic_result):
    result = native_synthetic_result
    assert result['comparison']['n'] >= 2
    assert math.isfinite(result['comparison']['mean'])
    assert all(math.isfinite(r['delta']) for r in result['comparison']['period_table'])
    for arm in result['arms'].values():
        assert arm['post_processing'] and arm['metrics']['turnover'] is None
        assert all(row['k'] == 1 and row['sigma_observations'] == 0 for row in arm['signal_rebalances'])
        for leg in ('long', 'short'):
            assert arm['legs'][leg]['engine'] == 'backtrader'
            assert arm['legs'][leg]['fills']
            assert math.isfinite(result['comparison']['legs'][leg]['mean'])
    assert -1 <= result['selection']['median_spearman'] <= 1
    assert len(result['selection']['period_table']) == result['comparison']['n']
    assert result['fallback']['share'] == 0
    for year, stats in result['comparison']['per_year'].items():
        values = [r['delta'] for r in result['comparison']['period_table'] if r['start'].startswith(year)]
        assert stats['n'] == len(values) and stats['mean'] == pytest.approx(np.mean(values))


def ledger_report():
    return dict(**passing_stats(), verdict='strong', prereg_sha256=study.prereg_sha256(),
                placebo_pct=0.90, d2_mean_delta=0.0, fallback_share=0.0, baseline_reproduced=True,
                input_hashes=dict(D3='synthetic-d3', D2='synthetic-d2'), invalid_reasons=[], deviations=[])


def test_ledger_identity_k_deviation_append_only_and_invalid_does_not_count(tmp_path):
    path = tmp_path / 'ledger.jsonl'
    base = dict(stage='S1', change_type='E', idea_id='synthetic-previous',
                baseline_version='synthetic', verdict='fail')
    for _ in range(7):
        evolution_ledger.append_event(path, 'fq_us', lambda k, previous: base)
    report = ledger_report()
    invalid = dict(report, verdict='invalid', invalid_reasons=['enough_periods'], deviations=[])
    bad = study.append_ledger(path, invalid)
    assert bad['K'] is None and not bad['counts_toward_k']
    assert not evolution_ledger.counts_toward_k(bad)
    before = path.read_bytes()
    row = study.append_ledger(path, report)
    assert path.read_bytes().startswith(before)
    assert row['K'] == row['preregistered_K'] == 8 and row['counts_toward_k']
    assert row['track'] == 'fq_us' and row['stage'] == 'S1' and row['change_type'] == 'E'
    assert row['idea_id'] == 'fq-us-physical-momentum-v1'
    assert row['baseline_version'] == 'fq_us:mom_ls_invvol_v1'
    assert row['prereg_path'] == study.PREREG_PATH
    prereg = Path(__file__).resolve().parents[3] / study.PREREG_PATH
    assert row['prereg_sha256'] == hashlib.sha256(prereg.read_bytes()).hexdigest()
    assert row['input_hashes'] == report['input_hashes']
    assert row['z'] == study.Z_K and row['halves'] == report['halves']
    assert json.loads(path.read_text().splitlines()[-1])['verdict'] == 'strong'
    isolated = study.append_ledger(tmp_path / 'isolated.jsonl', ledger_report())
    assert isolated['K'] == 1 and isolated['preregistered_K'] == 8
    assert any('K=1' in note for note in isolated['deviations'])


def test_hash_guard_uses_only_d3_d2_and_rejects_drift(tmp_path, monkeypatch):
    daily = tmp_path / 'daily'
    daily.mkdir()
    (daily / 'Z.json').write_bytes(b'z')
    (daily / 'A.json').write_bytes(b'a')
    expected = hashlib.sha256(b'az').hexdigest()
    for name in ('D3', 'D2'):
        monkeypatch.setitem(study.volscale.DATASETS, name, dict(daily=daily, spy=daily / 'SPY.json'))
        monkeypatch.setitem(study.volscale.EXPECTED_HASHES, name, expected)
    assert tuple(study.DATASETS) == ('D3', 'D2')
    hashes, files, drift = study.guard_hashes()
    assert hashes == dict(D3=expected, D2=expected) and not drift
    assert files == dict(D3=['A.json', 'Z.json'], D2=['A.json', 'Z.json'])
    (daily / 'A.json').write_bytes(b'changed synthetic input')
    with pytest.raises(ValueError, match='hash mismatch'):
        study.guard_hashes()
    with pytest.raises(TypeError):
        study.guard_hashes(True)


def test_output_paths_protect_inputs_and_support_separate_ledger(tmp_path, monkeypatch):
    daily = tmp_path / 'daily'
    spy, universe, frozen = [tmp_path / name for name in ('SPY.json', 'universe.json', 'frozen.json')]
    monkeypatch.setattr(study.volscale, 'SP500', universe)
    monkeypatch.setattr(study.volscale.stress, 'FROZEN_UNIVERSE', frozen)
    monkeypatch.setitem(study.volscale.DATASETS, 'D3', dict(daily=daily, spy=spy))
    out, ledger = study.output_paths(tmp_path / 'out', tmp_path / 'evolution/ledger.jsonl')
    assert out == tmp_path / 'out' and ledger == tmp_path / 'evolution/ledger.jsonl'
    assert study.output_paths(out)[1] == out / 'ledger.jsonl'
    for target in (daily, daily / 'out'):
        with pytest.raises(ValueError, match='input daily'):
            study.output_paths(target)
    for target in (daily / 'ledger.jsonl', spy, universe, frozen):
        with pytest.raises(ValueError, match='input'):
            study.output_paths(out, target)
    for target in (out / 'report.json', out / 'report.md', Path(study.PREREG_PATH)):
        with pytest.raises(ValueError, match='report or preregistration'):
            study.output_paths(out, target)
    # Path.resolve also protects a symlink into a synthetic input directory.
    daily.mkdir()
    alias = tmp_path / 'alias'
    alias.symlink_to(daily, target_is_directory=True)
    with pytest.raises(ValueError, match='input daily'):
        study.output_paths(alias / 'out')


def test_markdown_and_strict_json_on_synthetic_results(native_synthetic_result):
    report = ledger_report()
    report.update(verdict='invalid', notes=study.NOTES,
                  deviations=[n for n in study.NOTES if n.startswith('歧义口径')],
                  validity_checks=dict(enough_periods=False, fallback_within_limit=True,
                                       baseline_reproduced=False), invalid_reasons=['enough_periods'])
    report['datasets'] = {}
    for name in ('D3', 'D2'):
        expected = study.D3_B_EXPECTED_TOTAL if name == 'D3' else study.D2_B_EXPECTED_TOTAL
        report['datasets'][name] = dict(native_synthetic_result,
            baseline_reproduction=dict(study.baseline_reproduction(
                native_synthetic_result['arms']['B']['metrics']['total_return'], expected),
                descriptive_only=name == 'D2'))
    draws = [dict(draw=i + 1, mean=(i - 10) * 0.0001, z=0.0, short_histories={}) for i in range(20)]
    report['placebo'] = dict(seed=study.PLACEBO_SEED, draws=draws,
        **study.placebo_summary(native_synthetic_result['comparison']['mean'], [d['mean'] for d in draws]))
    report['placebo_pct'] = report['placebo']['placebo_pct']
    text = study.render_markdown(report)
    for phrase in ('判定：**invalid**', '安慰剂分布', '分年 Δ', '多头腿', 'D2 mean Δ', '回退比例',
                   'B 复现', '夏普', '最大回撤', '累计收益', '幸存者偏差', '歧义口径'):
        assert phrase in text
    safe = evolution_ledger.json_safe(report)
    decoded = json.loads(json.dumps(safe, ensure_ascii=False, allow_nan=False))
    assert decoded['datasets']['D3']['arms']['B']['signal_rebalances'][0]['sigma_hat'] is None


def test_investable_features_drop_undefined_volatility_for_all_arms():
    dates = pd.bdate_range('2024-01-01', periods=3)
    eligible = pd.DataFrame(True, index=dates, columns=['A', 'B', 'C'])
    vol = pd.DataFrame([[0.1, np.nan, 0.2], [0.1, 0.0, 0.2], [0.1, 0.3, -1.0]],
                       index=dates, columns=eligible.columns)
    features = dict(eligible=eligible, volatility=vol, signal=vol)
    out, dropped = study.investable_features(features)
    expected = pd.DataFrame([[True, False, True], [True, False, True], [True, True, False]],
                            index=dates, columns=eligible.columns)
    pd.testing.assert_frame_equal(out['eligible'], expected)
    pd.testing.assert_frame_equal(dropped, ~expected)
    assert out['signal'] is features['signal'] and features['eligible'].all().all()
