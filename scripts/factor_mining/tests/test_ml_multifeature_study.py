"""Synthetic-only checks of the preregistered multi-feature ML pipeline."""
import copy
import itertools
import json

import numpy as np
import pandas as pd
import pytest

from scripts.factor_mining import ml_multifeature_study as s
from scripts.factor_mining.tests.synth import planted


def synthetic(rows=280, names=15):
    p = planted(rows=rows, columns=names)
    rng = np.random.default_rng(43)
    p.fields['funding'] = pd.DataFrame(rng.normal(0, .0001, p.mask.shape), index=p.mask.index, columns=p.mask.columns)
    p.fields['premium'] = p.fields['funding'] * .3
    factors = [dict(id=f'f{i}', expr=e, direction=(-1 if i % 2 else 1), horizon=3)
               for i, e in enumerate(('$ret', '$close', '$volume', 'Std($ret, 7)',
                                      'Mean($funding, 7)', '$close / Ref($close, 3)', '$premium'))]
    return p, factors


def test_feature_hash_guard_and_frozen_input_hashes():
    assert len(s.FEATURES) == 38
    assert s.guard_features() == s.FEATURE_HASH
    altered = list(s.FEATURES)
    altered[0] = ('mom_1', '$close')
    with pytest.raises(ValueError, match='feature hash mismatch'):
        s.guard_features(altered)
    with pytest.raises(ValueError):
        s.guard_features(list(reversed(s.FEATURES)))
    assert s.EXPECTED_HASHES['crypto.aggregate'] == 'f60602b5feb8061702afc4a2c53296e3c87ab077669477c9635a0da9826cc4f0'
    assert s.EXPECTED_HASHES['crypto.zoo'] == 'cd9de8374450155e0f575da9e0ae8967722728910b16c064fb8f5eb0a17ad96e'
    assert s.EXPECTED_HASHES['crypto.ensemble'] == '5e3599f89dcaf4b73d4712a674c720004f25cd77eaaf08fb9c0f441a083feeb1'
    with pytest.raises(ValueError):
        s.guard_hashes({'crypto.aggregate': 'tampered'})
    assert s.guard_hashes({'crypto.aggregate': 'tampered'}, True)


def test_rank_map_half_rank_scale_ties_missing_and_minimum_eligible():
    values = pd.DataFrame(np.tile(np.arange(12, dtype=float), (5, 1)))
    mask = pd.DataFrame(True, index=values.index, columns=values.columns)
    values.iloc[1, :3] = [2., 2., 2.]
    values.iloc[2, :3] = [np.nan, np.inf, -np.inf]
    mask.iloc[3, 9:] = False
    mask.iloc[4, 10:] = False
    x = s.rank_map(values, mask)
    expected = ((np.arange(1, 13) - .5) / 12 - .5) * np.sqrt(12)
    np.testing.assert_allclose(x.iloc[0], expected)
    assert x.iloc[0].min() > -np.sqrt(3)
    assert x.iloc[0].max() < np.sqrt(3)
    np.testing.assert_allclose(x.iloc[1, :3], ((2 - .5) / 12 - .5) * np.sqrt(12))
    np.testing.assert_array_equal(x.iloc[2, :3], 0.)
    np.testing.assert_allclose(x.iloc[2, 3:], ((np.arange(1, 10) - .5) / 9 - .5) * np.sqrt(12))
    np.testing.assert_array_equal(x.iloc[3], 0.)
    np.testing.assert_allclose(x.iloc[4, :10], ((np.arange(1, 11) - .5) / 10 - .5) * np.sqrt(12))
    np.testing.assert_array_equal(x.iloc[4, 10:], 0.)
    y = s.rank_map(values, mask, label=True)
    np.testing.assert_allclose(y.iloc[0], expected / np.sqrt(12))
    assert y.iloc[2, :3].isna().all()
    assert y.iloc[3].isna().all()
    assert y.iloc[4, 10:].isna().all()
    cube = x.to_numpy()[:, :, None]
    scores = s.score_panel(cube, mask, np.ones(1))
    assert scores.iloc[3].isna().all()
    assert scores.iloc[4, :10].notna().all() and scores.iloc[4, 10:].isna().all()
    sample_x, sample_y, dates = s.training_sample(cube, y, mask, np.ones(5, bool))
    assert set(dates) == {0, 1, 2, 4}
    assert len(sample_x) == len(sample_y) == 12 + 12 + 9 + 10


def test_closed_form_ridge_augmented_lstsq_and_spectral_grid():
    rng = np.random.default_rng(71)
    x, y = rng.normal(size=(67, 9)), rng.normal(size=(67, 2))
    lam = .13
    augmented = np.vstack((x, np.sqrt(len(x) * lam) * np.eye(9)))
    target = np.vstack((y, np.zeros((9, 2))))
    expected = np.linalg.lstsq(augmented, target, rcond=None)[0]
    np.testing.assert_allclose(s.ridge(x, y, lam), expected, atol=2e-15)
    g, rhs = s.gram(x, y, chunk=13)
    fit = s.spectral_ridge(g, rhs, len(x), [lam, 2 * lam])
    np.testing.assert_allclose(fit[:, 0], expected, atol=2e-15)
    np.testing.assert_allclose(fit[:, 1], s.ridge(x, y, 2 * lam), atol=2e-15)


def test_rf_seed_formula_chunked_gram_and_prediction():
    rng = np.random.default_rng(19)
    x, y = rng.normal(size=(53, 45)), rng.normal(size=(53, 2))
    omega = s.random_weights()
    assert omega.shape == (45, 2048)
    np.testing.assert_array_equal(omega, np.random.default_rng(20261107).standard_normal((45, 2048)))
    np.testing.assert_array_equal(omega, s.random_weights())
    small = omega[:, :19]
    transformed = s.random_features(x, small)
    phase = x @ small / np.sqrt(45)
    np.testing.assert_allclose(transformed, np.c_[np.sin(phase), np.cos(phase)] / np.sqrt(19))
    np.testing.assert_allclose(np.sum(transformed ** 2, axis=1), 1., atol=1e-15)
    g, rhs = s.gram(x, y, small, chunk=7)
    np.testing.assert_allclose(g, transformed.T @ transformed, atol=2e-15)
    np.testing.assert_allclose(rhs, transformed.T @ y, atol=2e-15)
    beta = rng.normal(size=(38, 6, 2))
    np.testing.assert_allclose(s.predict(x, beta, small, chunk=9),
                               (transformed @ beta.reshape(38, -1)).reshape(53, 6, 2), atol=4e-15)


def test_cv_blocks_embargo_and_tie_selects_larger_absolute_lambda():
    folds = s.cv_indices(53)
    expected = list(np.array_split(np.arange(53), 5))
    for (train, evaluation), block in zip(folds, expected):
        np.testing.assert_array_equal(evaluation, block)
        blocked = np.arange(max(0, block[0] - 5), min(53, block[-1] + 6))
        np.testing.assert_array_equal(train, np.setdiff1d(np.arange(53), blocked))
        assert all(np.min(np.abs(block - i)) > 5 for i in train)
    np.testing.assert_array_equal(np.concatenate([e for _, e in folds]), np.arange(53))
    assert s.choose_lambda([{'lambda': 1., 'ic': .2}, {'lambda': 10., 'ic': .2}])['lambda'] == 10.
    assert s.choose_lambda([{'lambda': 1., 'ic': .20000001}, {'lambda': 10., 'ic': .2}])['lambda'] == 1.


@pytest.mark.parametrize('rf', [False, True])
def test_cv_gram_subtraction_matches_explicit_fits_fixed_full_scale(rf):
    rng = np.random.default_rng(76)
    dates = np.repeat(np.arange(31), 11)
    x = rng.normal(size=(len(dates), 5))
    y = x[:, 0] * .03 + rng.normal(size=len(dates)) * .2
    omega = s.random_weights(5, 9) if rf else None
    models, sufficient = s.fit_pair(x, y, dates, omega, lambda _: None)
    transformed = s.design(x, omega)
    expected_scale = np.trace(transformed.T @ transformed / len(x)) / transformed.shape[1]
    targets = [y, s.permute_labels(y, dates)]
    for a, model in enumerate(models):
        assert model['scale'] == pytest.approx(expected_scale)
        for row in model['cv']:
            assert row['lambda'] == pytest.approx(row['multiplier'] * expected_scale)
            ics = []
            for train, evaluation in s.cv_indices(31):
                use, evaluate = np.isin(dates, train), np.isin(dates, evaluation)
                beta = s.ridge(transformed[use], targets[a][use], row['lambda'])
                ics.append(s.daily_ic_rows(transformed[evaluate] @ beta, targets[a][evaluate], dates[evaluate]))
            assert row['ic'] == pytest.approx(pd.concat(ics).mean(), abs=1e-12)
        beta = s.ridge(transformed, targets[a], model['chosen_lambda'])
        np.testing.assert_allclose(model['beta'], beta, atol=2e-10)
    np.testing.assert_allclose(sufficient[0], transformed.T @ transformed, atol=2e-12)
    np.testing.assert_allclose(sufficient[1], transformed.T @ y, atol=2e-12)


def test_permutation_within_dates_preserves_multisets_and_single_rng_order():
    dates = np.array([5, 1, 5, 3, 1, 3, 3, 5, 1, 5])
    y = np.arange(len(dates), dtype=float)
    result = s.permute_labels(y, dates)
    rng = np.random.default_rng(20261108)
    for day in sorted(set(dates)):
        np.testing.assert_array_equal(np.sort(result[dates == day]), np.sort(y[dates == day]))
        np.testing.assert_array_equal(result[dates == day], rng.permutation(y[dates == day]))
    np.testing.assert_array_equal(result, s.permute_labels(y, dates))
    assert not np.array_equal(result, y)


@pytest.mark.parametrize('z,noise,val,fwd', list(itertools.product([.99, 1., 2.5758293035489, 3.], [.99, 1.], [0., .01], [0., .01])))
def test_verdict_truth_table(z, noise, val, fwd):
    z_k = s.z_threshold(5)
    expected = ('strong' if z >= z_k else 'screen_pass' if z >= 1 else 'fail') if noise >= 1 and val > 0 and fwd > 0 else 'fail'
    assert s.verdict(100, {'a': .5}, False, z, noise, val, fwd, z_k) == expected


def test_invalid_precedes_all_other_verdicts_and_z_threshold():
    good = dict(n=100, shares={'a': .5}, bad_prediction=False, z=3., z_noise=1., val_delta=.1, forward_delta=.1, z_k=s.z_threshold(5))
    assert s.verdict(**good) == 'strong'
    for bad in [dict(n=99), dict(shares={'a': .50001}), dict(bad_prediction=True)]:
        assert s.verdict(**(good | bad)) == 'invalid'
    for bad in [dict(z=None), dict(z=np.nan), dict(z_noise=np.nan), dict(val_delta=np.nan)]:
        assert s.verdict(**(good | bad)) == 'fail'
    assert s.z_threshold(5) == pytest.approx(2.5758, abs=.00005)


def minimal_result(verdict='fail'):
    stats = dict(verdict=verdict, z=.8, z_noise=.3, z_K=s.z_threshold(5), segment_delta=dict(val=.0001, forward=.0002))
    model = dict(chosen_lambda=.01, chosen_multiplier=.1)
    return dict(crypto=dict(paired=dict(R=stats, RF=stats.copy()), models=dict(R=model, RF=model.copy())),
                hashes={'crypto.aggregate': 'synthetic'}, deviations=['合成测试'])


def test_two_ledger_rows_and_invalid_null_k(tmp_path):
    path = tmp_path / 'ledger.jsonl'
    rows = s.append_ledger(path, minimal_result())
    assert [r['K'] for r in rows] == [1, 2]
    assert [r['preregistered_K'] for r in rows] == [5, 6]
    assert [r['idea_id'] for r in rows] == ['fq-crypto-ml-ridge-v1', 'fq-crypto-ml-rff-v1']
    for row in rows:
        assert row['track'] == 'fq_crypto'
        assert row['stage'] == 'S1'
        assert row['change_type'] == 'E'
        assert row['baseline_version'] == 'ensemble@5e3599f8'
        assert row['feature_hash'] == s.FEATURE_HASH
        assert row['prereg_path'] == s.PREREG
        assert row['input_hashes']['crypto.aggregate'] == 'synthetic'
    invalid = s.append_ledger(path, minimal_result('invalid'))
    assert all(r['K'] is None and not r['counts_toward_k'] for r in invalid)
    assert len([json.loads(line) for line in path.read_text().splitlines()]) == 4


def test_outputs_refused_in_snapshot_and_symlink(tmp_path):
    root = tmp_path / 'snapshot'
    root.mkdir()
    alias = tmp_path / 'alias'
    alias.symlink_to(root, target_is_directory=True)
    for out in [root, root / 'nested', alias / 'nested']:
        with pytest.raises(ValueError, match='read-only'):
            s.main(['--root', str(root), '--out', str(out), '--no-ledger'])
    with pytest.raises(ValueError, match='read-only'):
        s.main(['--root', str(root), '--out', str(tmp_path / 'out'), '--ledger', str(alias / 'ledger.jsonl')])


def test_feature_pipeline_nonfinite_shares_direction_and_causal_score():
    p, factors = synthetic()
    cube, names, shares, _ = s.feature_panels(p, factors)
    train = s.panel.make_splits(p.mask.index, 3, p.windows)['train'].to_numpy()
    directed = s.factor_values(p, factors, p.mask.index[-1])
    for j, f in enumerate(factors):
        np.testing.assert_array_equal(cube[:, :, 38 + j], s.rank_map(directed[f['id']], p.mask))
    raw = s.evaluate_expr(dict(s.FEATURES)['mom_120'], p.fields).to_numpy()
    use = p.mask.to_numpy() & train[:, None]
    assert shares['mom_120'] == pytest.approx((~np.isfinite(raw) & use).sum() / use.sum())
    t = 210
    beta = np.linspace(-.1, .1, 45)
    omega = s.random_weights(45, 13)
    before_r = s.score_panel(cube, p.mask, beta)
    before_rf = s.score_panel(cube, p.mask, np.linspace(-.1, .1, 26), omega)
    future = copy.deepcopy(p)
    rng = np.random.default_rng(990)
    for frame in future.fields.values():
        frame.iloc[t + 1:] *= rng.uniform(.1, 10, frame.iloc[t + 1:].shape)
    future.mask.iloc[t + 1:] = False
    changed_cube, _, _, _ = s.feature_panels(future, factors)
    np.testing.assert_array_equal(cube[:t + 1], changed_cube[:t + 1])
    pd.testing.assert_frame_equal(before_r.iloc[:t + 1], s.score_panel(changed_cube, future.mask, beta).iloc[:t + 1])
    pd.testing.assert_frame_equal(before_rf.iloc[:t + 1], s.score_panel(changed_cube, future.mask, np.linspace(-.1, .1, 26), omega).iloc[:t + 1])
    assert len(names) == 45


@pytest.mark.parametrize('rf', [False, True])
def test_fit_ignores_prices_and_labels_beyond_training_information_end(rf):
    p, factors = synthetic(rows=310, names=12)
    splits = s.panel.make_splits(p.mask.index, 3, p.windows)
    last_train = np.flatnonzero(splits['train'])[-1]
    cube = s.feature_panels(p, factors)[0]
    labels = s.rank_map(s.panel.forward_target(p, 3)[0], p.mask, label=True)
    x, y, dates = s.training_sample(cube, labels, p.mask, splits['train'])
    # The last train-signal label requires open[t+4]; preserve those four bars.
    later = copy.deepcopy(p)
    for frame in later.fields.values():
        frame.iloc[last_train + 5:] *= np.linspace(.2, 2., 12)
    changed_cube = s.feature_panels(later, factors)[0]
    changed_labels = s.rank_map(s.panel.forward_target(later, 3)[0], later.mask, label=True)
    changed_labels.iloc[last_train + 1:] = 999.
    xx, yy, dd = s.training_sample(changed_cube, changed_labels, later.mask, splits['train'])
    np.testing.assert_array_equal(x, xx)
    np.testing.assert_array_equal(y, yy)
    np.testing.assert_array_equal(dates, dd)
    omega = s.random_weights(45, 7) if rf else None
    original, _ = s.fit_pair(x, y, dates, omega, lambda _: None)
    changed, _ = s.fit_pair(xx, yy, dd, omega, lambda _: None)
    for a, b in zip(original, changed):
        np.testing.assert_array_equal(a['beta'], b['beta'])
        assert a['chosen_lambda'] == b['chosen_lambda']
    # Post-signal closes never enter the train label, which uses opens only.
    close_only = copy.deepcopy(p)
    close_only.fields['close'].iloc[last_train + 1:] *= .01
    close_cube = s.feature_panels(close_only, factors)[0]
    close_labels = s.rank_map(s.panel.forward_target(close_only, 3)[0], close_only.mask, label=True)
    xxx, yyy, _ = s.training_sample(close_cube, close_labels, close_only.mask, splits['train'])
    np.testing.assert_array_equal(x, xxx)
    np.testing.assert_array_equal(y, yyy)


def test_synthetic_full_backtest_report_and_reproduction(monkeypatch):
    p, factors = synthetic(rows=400, names=12)
    p.fields = {key: value.rename(columns={'S000': 'BTCUSDT'}) for key, value in p.fields.items()}
    p.mask = p.mask.rename(columns={'S000': 'BTCUSDT'})
    splits = s.panel.make_splits(p.mask.index, 3, p.windows)
    monkeypatch.setattr(s, 'VAL_RANGE', splits['ranges']['val'])
    monkeypatch.setattr(s.study, 'FORWARD_START', splits['ranges']['holdout'][0])
    original_weights = s.random_weights
    monkeypatch.setattr(s, 'random_weights', lambda: original_weights(45, 7))
    original_permutation = s.permute_labels
    permutations = []

    def shared_permutation(y, dates):
        permutations.append(1)
        return original_permutation(y, dates)

    monkeypatch.setattr(s, 'permute_labels', shared_permutation)
    result = s.crypto_study(p, factors, lambda _: None)
    assert len(permutations) == 1
    assert result['reproduction']['forward_daily_returns_exact_match']
    assert set(result['models']) == {'R', 'RF', 'N_R', 'N_RF'}
    assert set(result['descriptive_refits']) == {'R_prime', 'RF_prime'}
    for arm, stats in result['paired'].items():
        assert stats['n'] > 100
        assert stats['preregistered_K'] == (5 if arm == 'R' else 6)
    for segment in result['segments'].values():
        assert set(segment['arms']) == {'B', 'R', 'RF', 'N_R', 'N_RF'}
        for arm in ('R', 'RF'):
            assert set(segment['arms'][arm]['legs']) == {'long', 'short'}
    report = s.render_report(dict(crypto=result, hashes={}, file_counts={'crypto': 0}, deviations=[],
                                  attempt=1, elapsed_seconds=0., command='合成测试'))
    assert '六组' in report and '45 个系数' in report
    assert 'z_noise=' in report and '训练加验证' in report
    assert '重训的信息终点晚于前向起点' in report
