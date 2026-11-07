"""Synthetic-only checks for the frozen US multi-feature ML S1 study."""
import copy
import itertools
import json
import math

import numpy as np
import pandas as pd
import pytest

from scripts.factor_mining import us_ml_multifeature_study as s


def synthetic(rows=600, names=32):
    calendar = pd.bdate_range('2020-01-01', periods=rows)
    rng = np.random.default_rng(197)
    market = rng.normal(.0003, .009, rows)
    returns = market[:, None] * np.linspace(.5, 1.6, names) + rng.normal(.0002, .012, (rows, names))
    close = 50 * np.exp(np.cumsum(returns, axis=0))
    frames = {}
    for j in range(names):
        prices = close[:, j]
        frames[f'S{j:02d}'] = pd.DataFrame(dict(open=prices * .999, high=prices * 1.015,
            low=prices * .985, close=prices, volume=rng.uniform(2e6, 4e6, rows),
            typical=prices * 1.001), index=calendar)
    spy = pd.Series(200 * np.exp(np.cumsum(market)), index=calendar)
    features = s.momentum.build_features(frames, sorted(frames), calendar)
    pool = s.make_pool(frames, sorted(frames), calendar, spy, features['eligible'])
    return pool, frames, features, calendar


def test_feature_hash_guard():
    assert len(s.FEATURES) == 30
    assert s.guard_features() == '987f53daa50c6f828f568b2794ef8a6470608a4a4ed084f406870d7233bf9e4c'
    assert [n for n, _ in s.FEATURES][-2:] == ['body', 'body_7']
    for changed in [s.FEATURES[::-1], [('mom_1', '$close'), *s.FEATURES[1:]], s.FEATURES[:-1]]:
        with pytest.raises(ValueError, match='feature hash mismatch'):
            s.guard_features(changed)


def test_custom_features_hand_computed_and_complete_windows():
    dates = pd.bdate_range('2020-01-01', periods=300)
    increments = np.array([.01, -.02, .03, .005, -.004] * 60)
    market = np.r_[100., 100 * np.cumprod(1 + increments[1:])]
    values = np.r_[20., 20 * np.cumprod(1 + 1.7 * increments[1:] + np.sin(np.arange(1, 300)) * .003)]
    close, spy = pd.DataFrame({'A': values}, index=dates), pd.Series(market, index=dates)
    features = s.custom_features(close, spy)
    r, m = values[1:] / values[:-1] - 1, market[1:] / market[:-1] - 1
    at = 280
    assert tuple(features) == s.CUSTOM
    assert features['beta_60_spy'].iloc[at, 0] == pytest.approx(np.cov(r[at-60:at], m[at-60:at], ddof=1)[0, 1] / np.var(m[at-60:at], ddof=1))
    assert features['ivol_30_spy'].iloc[at, 0] == pytest.approx(np.std(r[at-30:at] - m[at-30:at], ddof=1))
    assert features['mom12_1'].iloc[at, 0] == values[at-21] / values[at-252] - 1
    assert features['mom6_1'].iloc[at, 0] == values[at-21] / values[at-126] - 1
    assert features['high52'].iloc[at, 0] == values[at] / max(values[at-251:at+1])
    assert features['beta_60_spy'].iloc[:60].isna().all().all()
    assert features['ivol_30_spy'].iloc[:30].isna().all().all()
    assert features['high52'].iloc[:251].isna().all().all()
    broken = close.copy()
    broken.iloc[260] = np.nan
    result = s.custom_features(broken, spy)
    for name in ('beta_60_spy', 'ivol_30_spy', 'high52'):
        assert np.isnan(result[name].iloc[at, 0])


def test_us_rank_mapping_minimum_names_and_finite_labels():
    values = pd.DataFrame(np.tile(np.arange(35.), (4, 1)))
    mask = values.notna()
    mask.iloc[0, 29:] = False
    values.iloc[1, :6] = np.nan
    values.iloc[2, :5] = np.nan
    features = s.rank_map(values, mask)
    assert features.iloc[0].eq(0).all()
    labels = s.rank_map(values, mask, label=True)
    assert labels.iloc[:2].isna().all().all()
    assert labels.iloc[2].notna().sum() == 30
    np.testing.assert_allclose(labels.iloc[2, 5:], (np.arange(1, 31) - .5) / 30 - .5)
    np.testing.assert_allclose(features.iloc[3], ((np.arange(1, 36) - .5) / 35 - .5) * np.sqrt(12))
    tmask = mask.copy()
    tmask.iloc[:, 15:] = False
    assert s.rank_map(values, tmask, min_names=15).iloc[3, :15].ne(0).sum() == 14


def test_cv_embargo_is_22_full_trading_positions_not_sample_distance():
    positions = np.arange(0, 500, 5)
    folds = s.cv_indices(positions)
    np.testing.assert_array_equal(np.concatenate([e for _, e in folds]), np.arange(100))
    for tr, ev in folds:
        expected = np.flatnonzero((positions < positions[ev[0]] - 22) | (positions > positions[ev[-1]] + 22))
        np.testing.assert_array_equal(tr, expected)
        for i in tr:
            assert np.min(np.abs(positions[ev] - positions[i])) > 22
    # At the second fold, four sampled dates on each side are removed; the fifth remains.
    tr, ev = folds[1]
    assert ev[0] - 4 not in tr and ev[0] - 5 in tr
    irregular = np.array([0, 8, 17, 22, 23, 45, 67, 68, 90, 113, 135, 158, 181, 204, 227])
    for tr, ev in s.cv_indices(irregular):
        assert all(np.min(np.abs(irregular[ev] - irregular[i])) >= 23 for i in tr)


def test_every_fifth_trading_date_and_label_end_purge():
    dates = pd.bdate_range('2021-01-01', '2022-02-28').delete([5, 20, 100])
    mask = pd.DataFrame(True, index=dates, columns=range(30))
    labels = mask.astype(float)
    mask.iloc[:3] = False
    mask.iloc[13] = False
    labels.iloc[18, 0] = np.nan
    selected = s.sampled_dates(dates, mask, labels)
    last_end = dates.searchsorted(pd.Timestamp('2021-12-31'), side='right') - 1
    expected = np.arange(3, last_end - 20 + 1, 5)
    expected = expected[~np.isin(expected, [13, 18])]
    np.testing.assert_array_equal(np.flatnonzero(selected), expected)
    assert np.all(dates[np.flatnonzero(selected) + 20] <= pd.Timestamp('2021-12-31'))
    # The phase does not restart after an unusable sampled date.
    assert selected[23] and not selected[24]
    window = s.train_window(dates)
    assert window[last_end - 20] and not window[last_end - 19]


def test_cluster_cr0_hand_computation():
    delta = pd.Series([1., 3., -2., 4., 9.])
    clusters = pd.Series(['a', 'a', 'b', 'b', 'b'])
    mean = 3.
    expected_se = math.sqrt((1 + 3 - 2 * mean) ** 2 + (-2 + 4 + 9 - 3 * mean) ** 2) / 5
    result = s.cluster_statistics(delta, clusters)
    assert result == dict(n_days=5, n_periods=2, mean_delta=mean, se=expected_se, z=mean / expected_se)
    assert s.cluster_statistics(pd.Series(dtype=float), pd.Series(dtype=float))['z'] is None


def test_segments_use_period_start_and_daily_boundaries():
    assert [s.segment_of(x) for x in ['2021-12-31', '2022-01-01', '2024-12-31', '2025-01-01']] == ['excluded', 'val', 'val', 'forward']
    calendar = pd.bdate_range('2024-12-27', '2025-01-08')
    periods = [dict(k=1, start='2024-12-27', end='2025-01-02'), dict(k=2, start='2025-01-02', end='2025-01-08')]
    members = s.period_days(calendar, periods)
    assert calendar[0] not in members.index
    assert members.loc['2025-01-02'].to_dict() == dict(period=1, segment='val')
    assert members.loc['2025-01-03'].to_dict() == dict(period=2, segment='forward')
    assert members.index.is_unique and len(members) == len(calendar) - 1


def test_momentum_signal_override_reproduces_B_native_paths_exactly():
    _, frames, features, calendar = synthetic(rows=300, names=20)
    sessions, baseline = s.momentum.rebalance_schedule(features, calendar)
    override = s.signal_decisions(features, [pd.Timestamp(r['date']) for r in baseline], features['signal'])
    assert override == baseline
    a = s.run_arm(frames, sessions, baseline, lambda _: None, 'baseline')
    b = s.run_arm(frames, sessions, override, lambda _: None, 'override')
    assert a['period_table'] == b['period_table']
    assert a['equity_curve'] == b['equity_curve']
    assert a['fills'] == b['fills']


@pytest.mark.parametrize('z,noise,val,fwd', list(itertools.product([.99, 1., s.z_threshold(6), 3.], [.99, 1.], [0., .01], [0., .01])))
def test_verdict_truth_table(z, noise, val, fwd):
    expected = ('strong' if z >= s.z_threshold(6) else 'screen_pass' if z >= 1 else 'fail') if noise >= 1 and val > 0 and fwd > 0 else 'fail'
    assert s.verdict(30, {'f': .5}, False, z, noise, val, fwd, s.z_threshold(6)) == expected


def test_invalid_precedes_every_other_verdict():
    good = dict(n=30, shares={'f': .5}, bad_prediction=False, z=3., z_noise=1., val_delta=.01, forward_delta=.01, z_k=s.z_threshold(6))
    for bad in [dict(n=29), dict(shares={'f': .50001}), dict(bad_prediction=True)]:
        assert s.verdict(**(good | bad)) == 'invalid'
    for bad in [dict(z=None), dict(z=np.nan), dict(z_noise=np.nan), dict(val_delta=np.nan)]:
        assert s.verdict(**(good | bad)) == 'fail'
    assert s.z_threshold(6) == pytest.approx(2.638, abs=.0005)
    assert s.z_threshold(7) == pytest.approx(2.690, abs=.0005)


def test_ledger_rows(tmp_path):
    result = dict(paired={a: dict(verdict='fail', z=.5, z_noise=.3, z_K=s.z_threshold(k), preregistered_K=k,
                      segment_delta=dict(val=.001, forward=-.002)) for a, k in [('R', 6), ('RF', 7)]},
                  models={a: dict(chosen_lambda=.01) for a in ('R', 'RF')},
                  hashes={'broad_daily': 'synthetic'}, deviations=[])
    path = tmp_path / 'ledger.jsonl'
    events = s.append_ledger(path, result)
    assert [r['K'] for r in events] == [1, 2]
    assert [r['preregistered_K'] for r in events] == [6, 7]
    assert [r['idea_id'] for r in events] == ['fq-us-ml-ridge-v1', 'fq-us-ml-rff-v1']
    for r in events:
        assert r['track'] == 'fq_us' and r['stage'] == 'S1' and r['change_type'] == 'E'
        assert r['baseline_version'] == 'fq_us:mom_ls_invvol_v1'
        assert r['feature_hash'] == s.FEATURE_HASH and r['prereg_path'] == s.PREREG
        assert r['input_hashes'] == result['hashes'] and r['chosen_lambda'] == .01
    assert len([json.loads(line) for line in path.read_text().splitlines()]) == 2
    for p in result['paired'].values():
        p['verdict'] = 'invalid'
    assert all(r['K'] is None and not r['counts_toward_k'] for r in s.append_ledger(path, result))


def test_output_paths_refuse_inputs_and_symlinks(tmp_path):
    broad, snapshot = tmp_path / 'U', tmp_path / 'T'
    broad.mkdir()
    snapshot.mkdir()
    alias = tmp_path / 'alias'
    alias.symlink_to(broad, target_is_directory=True)
    for root in (broad, snapshot, alias):
        with pytest.raises(ValueError, match='read-only'):
            s.validate_outputs(broad, snapshot, root / 'out', None)
    with pytest.raises(ValueError):
        s.validate_outputs(broad, snapshot, tmp_path / 'out', snapshot / 'ledger.jsonl')
    out = tmp_path / 'out'
    out.mkdir()
    (out / 'daily_returns.csv').symlink_to(snapshot / 'prices.csv')
    with pytest.raises(ValueError, match='read-only'):
        s.validate_outputs(broad, snapshot, out, None)


def test_T_score_has_no_price_lookahead():
    pool, frames, _, calendar = synthetic(rows=410)
    cube, names, _, _ = s.feature_panels(pool, s.MIN_T)
    at = 350
    altered = copy.deepcopy(frames)
    for j, frame in enumerate(altered.values()):
        frame.loc[calendar[at+1:], ['open', 'high', 'low', 'close', 'typical']] *= .2 + j / 10
        frame.loc[calendar[at+1:], 'volume'] *= 3
    features = s.momentum.build_features(altered, sorted(altered), calendar)
    spy = pool.spy.copy()
    spy.iloc[at+1:] *= 5
    changed = s.make_pool(altered, sorted(altered), calendar, spy, features['eligible'])
    changed_cube = s.feature_panels(changed, s.MIN_T)[0]
    np.testing.assert_array_equal(cube[:at+1], changed_cube[:at+1])
    assert names == [n for n, _ in s.FEATURES] + list(s.CUSTOM)
    for omega, p in [(None, 35), (s.core.random_weights(35, 7), 14)]:
        beta = np.linspace(-.03, .04, p)
        original = s.score_panels(cube, pool.mask, [beta], omega)[0]
        perturbed = s.score_panels(changed_cube, changed.mask, [beta], omega)[0]
        pd.testing.assert_frame_equal(original.iloc[:at+1], perturbed.iloc[:at+1], check_exact=True)


@pytest.mark.parametrize('rf', [False, True])
def test_U_after_train_end_cannot_change_samples_or_coefficients(rf):
    pool, frames, _, calendar = synthetic(rows=620)
    cube = s.feature_panels(pool, s.MIN_U)[0]
    original = s.training_sample(cube, pool)
    changed_frames = copy.deepcopy(frames)
    for j, frame in enumerate(changed_frames.values()):
        frame.loc[frame.index > s.TRAIN_END, ['open', 'high', 'low', 'close', 'typical']] *= .5 + j
        frame.loc[frame.index > s.TRAIN_END, 'volume'] = 0
    features = s.momentum.build_features(changed_frames, sorted(changed_frames), calendar)
    spy = pool.spy.copy()
    spy.loc[spy.index > s.TRAIN_END] *= 10
    changed = s.make_pool(changed_frames, sorted(changed_frames), calendar, spy, features['eligible'])
    changed_cube = s.feature_panels(changed, s.MIN_U)[0]
    perturbed = s.training_sample(changed_cube, changed)
    for a, b in zip(original, perturbed):
        np.testing.assert_array_equal(a, b)
    x, y, dates = original
    omega = s.core.random_weights(35, 7) if rf else None
    fits = s.fit_pair(x, y, dates, omega, lambda _: None)
    changed_fits = s.fit_pair(*perturbed, omega, lambda _: None)
    for a, b in zip(fits, changed_fits):
        np.testing.assert_array_equal(a['beta'], b['beta'])
        assert a['chosen_lambda'] == b['chosen_lambda']
    # Check the US wrapper against direct fold fits with the full-training scale.
    transformed = s.core.design(x, omega)
    unique, codes = np.unique(dates, return_inverse=True)
    noise = s.core.permute_labels(y, dates)
    for arm, model in enumerate(fits):
        target = y if arm == 0 else noise
        for row in model['cv']:
            daily = []
            for train, evaluation in s.cv_indices(unique):
                tr, ev = np.isin(codes, train), np.isin(codes, evaluation)
                beta = s.core.ridge(transformed[tr], target[tr], row['lambda'])
                daily.append(s.core.daily_ic_rows(transformed[ev] @ beta, target[ev], dates[ev]))
            assert row['ic'] == pytest.approx(pd.concat(daily).mean(), abs=1e-10)
