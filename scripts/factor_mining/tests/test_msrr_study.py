"""Synthetic-only checks for the frozen fq_crypto MSRR study."""
import copy
import hashlib
import itertools
import json

import numpy as np
import pandas as pd
import pytest

from scripts.factor_mining import msrr_study as s
from scripts.factor_mining.tests.synth import planted


def synthetic(rows=270, names=20):
    p = planted(rows=rows, columns=names)
    p.fields = {key: value.rename(columns={'S000': 'BTCUSDT'}) for key, value in p.fields.items()}
    p.mask = p.mask.rename(columns={'S000': 'BTCUSDT'})
    p.fields['funding'] = p.fields['close'] * 0 + .00001
    expressions = ['$close/$open-1', 'Std($ret,5)', '$volume', '$close/Ref($close,3)',
                   'Mean($close,5)/$close', '$high/$low', 'Delta($close,2)']
    factors = [dict(id=f'f{k}', horizon=3, expr=expr, direction=(-1 if k % 2 else 1))
               for k, expr in enumerate(expressions)]
    return p, factors


def test_leg_sums_direction_ranking_and_unmasked_btc_column():
    columns = pd.Index(['BTCUSDT'] + [f'C{i}' for i in range(1, 11)])
    values = np.tile(np.arange(11), (7, 1)).astype(float)
    eligible = np.array([False] + [True] * 10)
    legs = s.leg_weights(values, eligible, columns)
    assert legs.shape == (15, 11)
    np.testing.assert_allclose(legs[:-1:2].sum(axis=1), 1)
    np.testing.assert_allclose(legs[1:-1:2].sum(axis=1), -1)
    np.testing.assert_array_equal(legs[:-1, 0], 0)
    np.testing.assert_array_equal(legs[-1], [1] + [0] * 10)
    np.testing.assert_allclose(legs[0, -2:], .5)
    np.testing.assert_allclose(legs[1, 1:3], -.5)
    reverse = s.leg_weights(-values, eligible, columns)
    np.testing.assert_array_equal(reverse[0], -legs[1])
    np.testing.assert_array_equal(reverse[1], -legs[0])
    assert s.leg_weights(values, eligible, columns, False).shape == (14, 11)
    empty = s.leg_weights(values, np.zeros(11, bool), columns)
    np.testing.assert_array_equal(empty[:-1], 0)
    np.testing.assert_array_equal(empty[-1], legs[-1])
    p, factors = synthetic()
    directed = s.factor_values(p, factors, p.mask.index[-1])
    for f in factors:
        pd.testing.assert_frame_equal(directed[f['id']], s.evaluate_expr(f['expr'], p.fields) * f['direction'])


def test_training_returns_close_to_close_missing_renormalisation_and_train_endpoint():
    p, _ = synthetic(rows=10, names=10)
    close = p.fields['close']
    close.iloc[:] = np.arange(1, 11) * 10.
    close.iloc[3] *= 1.1
    close.iloc[6] *= 1.2
    close.iloc[3, 8] = np.nan
    p.fields['volume'].iloc[:] = np.arange(10)
    factors = [dict(id='v', expr='$volume', direction=1)]
    managed, meta = s.training_returns(p, factors, close.index[0], close.index[7])
    assert list(managed.columns) == ['v/L', 'v/S', 'BTC']
    assert list(managed.index) == list(close.index[[0, 3]])
    assert meta['period_ends'] == [s.study.day(close.index[3]), s.study.day(close.index[6])]
    # One long name is missing at the first endpoint; the other receives +1.
    np.testing.assert_allclose(managed.iloc[0], [.1, -.1, .1])
    np.testing.assert_allclose(managed.iloc[1], [1.2 / 1.1 - 1, -(1.2 / 1.1 - 1), 1.2 / 1.1 - 1])
    assert meta['dropped_coin_leg_observations'] == 2
    close.iloc[3, 9] = np.nan
    empty, meta = s.training_returns(p, factors, close.index[0], close.index[7])
    assert empty.iloc[0, 0] == 0
    assert meta['empty_leg_periods'] == 2


def test_ridge_closed_form_matches_augmented_lstsq_and_beta_rescaling():
    rng = np.random.default_rng(710)
    x = rng.normal(.003, .04, (61, 15)) * np.linspace(.1, 3., 15)
    z = .17
    fit = s.ridge_msrr(x, z)
    sigma = x.std(axis=0, ddof=1)
    scaled = x / sigma
    augmented = np.vstack([scaled, np.sqrt(z * len(x)) * np.eye(15)])
    target = np.r_[np.ones(len(x)), np.zeros(15)]
    expected = np.linalg.lstsq(augmented, target, rcond=None)[0]
    np.testing.assert_allclose(fit['beta_tilde'], expected, rtol=1e-12, atol=1e-14)
    np.testing.assert_array_equal(fit['sigma'], sigma)
    np.testing.assert_allclose(fit['beta'], expected / sigma, rtol=1e-12, atol=1e-14)
    np.testing.assert_allclose(x @ fit['beta'], scaled @ fit['beta_tilde'], atol=1e-14)
    with pytest.raises(ValueError, match='standard deviations'):
        s.ridge_msrr(np.zeros((10, 15)), z)


@pytest.mark.parametrize('dimensions', [15, 14])
def test_z_grid_uncentered_second_moment_and_literal_divisor(dimensions):
    rng = np.random.default_rng(98)
    x = rng.normal(.01, .02, (53, dimensions))
    scaled = x / x.std(axis=0, ddof=1)
    expected = np.trace(scaled.T @ scaled / len(x)) / 15
    lambda_bar, grid = s.z_grid(x)
    assert lambda_bar == pytest.approx(expected)
    np.testing.assert_allclose(grid, expected * np.array([.001, .01, .1, 1, 10, 100]))


def test_cv_contiguous_blocks_one_period_embargo_and_boundary_indices():
    folds = s.cv_indices(23)
    expected_blocks = [np.arange(0, 5), np.arange(5, 10), np.arange(10, 15),
                       np.arange(15, 19), np.arange(19, 23)]
    np.testing.assert_array_equal(folds[0][0], np.arange(6, 23))
    np.testing.assert_array_equal(folds[-1][0], np.arange(18))
    np.testing.assert_array_equal(folds[2][0], np.r_[np.arange(9), np.arange(16, 23)])
    for (train, evaluation), block in zip(folds, expected_blocks):
        np.testing.assert_array_equal(evaluation, block)
        assert not set(train) & set(evaluation)
        assert all(np.min(np.abs(evaluation - i)) > 1 for i in train)
    np.testing.assert_array_equal(np.concatenate([e for _, e in folds]), np.arange(23))


def test_cv_training_fold_std_oof_concatenation_refit_and_larger_z_tie():
    rng = np.random.default_rng(912)
    x = rng.normal(.003, .025, (53, 15))
    x[:11] *= 10
    returns = pd.DataFrame(x, index=pd.date_range('2020-01-01', periods=53))
    result = s.fit_model(returns)
    for row in result['cv']:
        expected = np.zeros(len(x))
        for train, evaluation in s.cv_indices(len(x)):
            train_x = x[train]
            sigma = train_x.std(axis=0, ddof=1)
            scaled = train_x / sigma
            tilde = np.linalg.solve(scaled.T @ scaled / len(train) + row['z'] * np.eye(15), scaled.mean(axis=0))
            expected[evaluation] = x[evaluation] @ (tilde / sigma)
        np.testing.assert_allclose(list(row['oof_returns'].values()), expected, atol=1e-14)
        assert row['sharpe'] == pytest.approx(expected.mean() / expected.std(ddof=1) * np.sqrt(365 / 3))
    chosen = max(result['cv'], key=lambda row: (row['sharpe'], row['z']))
    assert result['chosen_z'] == chosen['z']
    np.testing.assert_allclose(result['beta'], s.ridge_msrr(x, chosen['z'])['beta'])
    assert s.choose_z([dict(z=1., sharpe=.4), dict(z=100., sharpe=.4), dict(z=10., sharpe=.4)]) == 100.
    assert s.choose_z([dict(z=1., sharpe=.400000000001), dict(z=100., sharpe=.4)]) == 1.


def test_coin_weight_normalisation_cap_excess_cash_and_all_zero():
    np.testing.assert_array_equal(s.normalise_cap(np.zeros(20)), 0)
    weights = s.normalise_cap([8., -1., 1.])
    np.testing.assert_allclose(weights, [.1, -.1, .1])
    assert np.abs(weights).sum() == pytest.approx(.3)
    assert 1 - np.abs(weights).sum() == pytest.approx(.7)
    diversified = np.r_[np.ones(10), -np.ones(10)]
    np.testing.assert_allclose(s.normalise_cap(diversified), diversified / 20)
    assert np.abs(s.normalise_cap(diversified)).sum() == pytest.approx(1)


def test_btc_share_after_cap_includes_cash_dates_and_date_weighted_pooling():
    raw = [[8., 1., 1.], [1., 9., 0.], [0., 0., 0.]]
    weights = pd.DataFrame([s.normalise_cap(row) for row in raw], columns=['BTCUSDT', 'A', 'B'])
    assert s.btc_weight_share(weights) == pytest.approx((1 / 3 + .5 + 0) / 3)
    only_btc = weights * 0
    only_btc['BTCUSDT'] = .1
    assert s.btc_weight_share(only_btc) == 1
    pooled = pd.concat([weights, only_btc.iloc[:1]])
    assert s.btc_weight_share(pooled) == pytest.approx((1 / 3 + .5 + 0 + 1) / 4)


@pytest.mark.parametrize('z,val,forward,share', list(itertools.product([1.64, 1.65], [0., .01], [0., .01], [.5, .50001])))
def test_verdict_truth_table(z, val, forward, share):
    expected = 'screen_pass' if z >= 1.65 and val > 0 and forward > 0 and share <= .5 else 'fail'
    assert s.verdict(z, val, forward, share) == expected


@pytest.mark.parametrize('z,val,forward,share', [
    (None, .1, .1, .1), (np.nan, .1, .1, .1), (2., None, .1, .1),
    (2., np.nan, .1, .1), (2., .1, None, .1), (2., .1, np.nan, .1),
    (2., .1, .1, None), (2., .1, .1, np.nan), (2., .1, .1, .9),
])
def test_verdict_missing_or_btc_dominated_fails(z, val, forward, share):
    assert s.verdict(z, val, forward, share) == 'fail'


def test_hash_guard_same_frozen_crypto_values_and_content_hash(tmp_path):
    base = tmp_path / 'crypto'
    (base / 'daily').mkdir(parents=True)
    contents = {'zoo.json': b'{"factors":[]}', 'ensemble.json': b'{}',
                'daily/a.json': b'lowercase', 'daily/B.json': b'uppercase'}
    for name, value in contents.items():
        (base / name).write_bytes(value)
    actual, count = s.input_hashes(tmp_path, 'crypto')
    assert count == 4
    expected = b''.join(contents[key] for key in sorted(contents, key=lambda key: key.encode()))
    assert actual['crypto.aggregate'] == hashlib.sha256(expected).hexdigest()
    assert s.EXPECTED_HASHES['crypto.aggregate'] == 'f60602b5feb8061702afc4a2c53296e3c87ab077669477c9635a0da9826cc4f0'
    assert s.EXPECTED_HASHES['crypto.zoo'] == 'cd9de8374450155e0f575da9e0ae8967722728910b16c064fb8f5eb0a17ad96e'
    assert s.EXPECTED_HASHES['crypto.ensemble'] == '5e3599f89dcaf4b73d4712a674c720004f25cd77eaaf08fb9c0f441a083feeb1'
    assert s.guard_hashes(actual, expected=actual) == {}
    with pytest.raises(ValueError, match='refusing'):
        s.guard_hashes(actual)
    assert s.guard_hashes(actual, True)
    (base / 'daily/a.json').write_bytes(b'changed')
    drift = s.input_hashes(tmp_path, 'crypto')[0]
    with pytest.raises(ValueError):
        s.guard_hashes(drift, expected=actual)
    assert s.guard_hashes(drift, True, expected=actual)


def test_output_and_ledger_refused_inside_snapshot_including_symlink(tmp_path):
    root = tmp_path / 'snapshot'
    root.mkdir()
    alias = tmp_path / 'alias'
    alias.symlink_to(root, target_is_directory=True)
    for out in [root, root / 'nested', alias / 'nested']:
        with pytest.raises(ValueError, match='read-only'):
            s.main(['--root', str(root), '--out', str(out), '--no-ledger'])
    with pytest.raises(ValueError, match='read-only'):
        s.main(['--root', str(root), '--out', str(tmp_path / 'out'), '--ledger', str(root / 'ledger.jsonl')])
    with pytest.raises(ValueError):
        s.validate_outputs(root, [tmp_path / 'same', tmp_path / 'same'])


def minimal_result():
    return dict(crypto=dict(paired=dict(verdict='fail', z=.8), btc_weight_share=.6,
                            models=dict(M=dict(chosen_z=.01)),
                            segments={name: dict(paired=dict(mean_delta=delta))
                                      for name, delta in [('val', .001), ('forward', -.002)]}),
                hashes={'crypto.aggregate': 'synthetic'}, deviations=['合成测试'])


def test_ledger_row_written_to_temp_file_with_automatic_K(tmp_path):
    path = tmp_path / 'ledger.jsonl'
    result = minimal_result()
    for _ in range(2):
        row = s.append_ledger(path, result)
    rows = [json.loads(line) for line in path.read_text().splitlines()]
    assert [r['K'] for r in rows] == [1, 2]
    for key, expected in dict(track='fq_crypto', stage='S1', change_type='E',
                              idea_id='fq-crypto-msrr-ridge-v1', baseline_version='ensemble@5e3599f8',
                              verdict='fail', preregistered_K=4, z=.8, btc_weight_share=.6, chosen_z=.01).items():
        assert row[key] == expected
    assert row['segment_delta'] == {'val': .001, 'forward': -.002}
    assert row['input_hashes'] == result['hashes']


def test_no_lookahead_beta_and_selected_z_unchanged_after_training_end():
    p, factors = synthetic()
    splits = s.panel.make_splits(p.mask.index, 3, p.windows)
    start, end = splits['ranges']['train']
    managed, _ = s.training_returns(p, factors, start, end)
    before = {arm: s.fit_model(x) for arm, x in [('M', managed), ('M0', managed.iloc[:, :-1])]}
    after_panel = copy.deepcopy(p)
    later = after_panel.mask.index > pd.Timestamp(end)
    for value in after_panel.fields.values():
        value.loc[later] *= np.arange(value.shape[1]) + 100
    after_panel.mask.loc[later] = False
    changed, _ = s.training_returns(after_panel, factors, start, end)
    pd.testing.assert_frame_equal(managed, changed, check_exact=True)
    for arm, x in [('M', changed), ('M0', changed.iloc[:, :-1])]:
        after = s.fit_model(x)
        np.testing.assert_array_equal(before[arm]['beta'], after['beta'])
        assert before[arm]['chosen_z'] == after['chosen_z']


def test_no_lookahead_val_targets_schedule_btc_mask_and_zero_beta_cash():
    p, factors = synthetic()
    start, end = p.mask.index[180], p.mask.index[217]
    t = p.mask.index[192]
    p.mask.loc[t, 'BTCUSDT'] = False
    beta = np.r_[np.linspace(-.03, .04, 14), .2]
    before = s.build_targets(p, factors, beta, start, end)
    after_panel = copy.deepcopy(p)
    for value in after_panel.fields.values():
        value.loc[value.index > t] *= np.arange(value.shape[1], 0, -1) + 20
    after_panel.mask.loc[after_panel.mask.index > t] = False
    after = s.build_targets(after_panel, factors, beta, start, end)
    pd.testing.assert_frame_equal(before.loc[:t], after.loc[:t], check_exact=True)
    held_stop = before.index.get_loc(t) + 3
    pd.testing.assert_frame_equal(before.iloc[:held_stop], after.iloc[:held_stop], check_exact=True)
    assert before.index.equals(pd.date_range(start, end))
    assert not before.equals(after)
    assert before.abs().max().max() <= .1
    assert (before.abs().sum(axis=1) <= 1 + 1e-14).all()
    for i in range(0, len(before), 3):
        np.testing.assert_array_equal(before.iloc[i:min(i + 3, len(before))],
                                      np.tile(before.iloc[i], (min(3, len(before) - i), 1)))
    cash = s.build_targets(p, factors, np.zeros(15), start, end)
    np.testing.assert_array_equal(cash, 0)
    btc_only = s.build_targets(p, factors, np.r_[np.zeros(14), 1.], start, end)
    assert btc_only.at[t, 'BTCUSDT'] == .1
    assert s.btc_weight_share(btc_only) == 1


def test_decomposition_reconstructs_capped_coin_weights_without_rescaling():
    targets = pd.DataFrame([[.1, .02, -.03], [-.05, -.04, .07]], columns=['BTCUSDT', 'A', 'B'])
    parts = s.split_targets(targets)
    pd.testing.assert_frame_equal(sum(parts.values()), targets)
    assert (parts['long'] >= 0).all().all()
    assert (parts['short'] <= 0).all().all()
    assert (parts['long']['BTCUSDT'] == 0).all()
    assert (parts['short']['BTCUSDT'] == 0).all()
    pd.testing.assert_series_equal(parts['BTC']['BTCUSDT'], targets['BTCUSDT'])


def test_synthetic_end_to_end_real_backtrader_reports_and_exact_baseline(tmp_path, monkeypatch):
    p, factors = synthetic(rows=180)
    root, out = tmp_path / 'snapshot', tmp_path / 'out'
    (root / 'crypto').mkdir(parents=True)
    all_factors = factors + [dict(factors[0], id=f'f_h{h}', horizon=h) for h in (1, 7)]
    (root / 'crypto/zoo.json').write_text(json.dumps(dict(factors=all_factors)))
    (root / 'crypto/ensemble.json').write_text(json.dumps(dict(horizons={
        str(h): dict(factor_ids=[f['id'] for f in all_factors if f['horizon'] == h]) for h in (1, 3, 7)})))
    before = {path: path.read_bytes() for path in root.rglob('*') if path.is_file()}
    index = p.mask.index
    monkeypatch.setattr(s.panel, 'load_panels', lambda market, snapshot: copy.deepcopy(p))
    monkeypatch.setattr(s, 'VAL_RANGE', (s.study.day(index[110]), s.study.day(index[130])))
    monkeypatch.setattr(s.study, 'FORWARD_START', s.study.day(index[140]))
    result = s.main(['--root', str(root), '--out', str(out), '--allow-input-drift',
                     '--ledger', str(out / 'ledger.jsonl')])
    c = result['crypto']
    assert c['reproduction']['score_exact_match']
    assert c['reproduction']['forward_daily_returns_exact_match']
    assert c['paired']['n'] == 21 + 40
    assert len(c['models']['M']['beta']) == 15
    assert len(c['models']['M0']['beta']) == 14
    for segment in c['segments'].values():
        assert set(segment['arms']) == {'B', 'M', 'M0'}
        assert set(segment['arms']['M']['legs']) == {'long', 'short', 'BTC'}
        baseline = pd.Series(segment['arms']['B']['daily_returns'])
        for arm, paired_key in [('M', 'paired'), ('M0', 'paired_M0_descriptive')]:
            candidate = segment['arms'][arm]
            np.testing.assert_allclose(pd.Series(segment[paired_key]['daily_delta']),
                                       pd.Series(candidate['daily_returns']) - baseline)
            assert candidate['backtest']['engine_version']
            assert candidate['backtest']['execution_audit']['unexpected_fills'] == 0
        metrics = segment['arms']['M']['backtest']['metrics']
        assert metrics['funding_observations'] > 0
    assert all(path.read_bytes() == data for path, data in before.items())
    assert json.loads((out / 'report.json').read_text()) == result
    report = (out / 'report.md').read_text()
    assert all(word in report for word in ('β̃', '偏离', '只描述', '输入哈希', 'M₀', '主检验'))
    row = json.loads((out / 'ledger.jsonl').read_text())
    assert row['verdict'] == c['paired']['verdict']
    assert row['chosen_z'] == c['models']['M']['chosen_z']
    assert row['btc_weight_share'] == c['btc_weight_share']


def test_target_backtest_exact_execution_arguments_and_calendar(monkeypatch):
    p, factors = synthetic(rows=60)
    start, end = p.mask.index[15], p.mask.index[42]
    targets = s.build_targets(p, factors, np.linspace(-.1, .1, 15), start, end)
    calls = []
    original = s.study.q.run_weights

    def capture(frames, weights, **kwargs):
        calls.append((frames, weights.copy(), kwargs))
        return original(frames, weights, **kwargs)

    monkeypatch.setattr(s.study.q, 'run_weights', capture)
    run, _ = s.target_backtest(p, targets, start, end)
    assert run['execution_audit']['unexpected_fills'] == 0
    _, weights, kwargs = calls[0]
    pd.testing.assert_frame_equal(weights, targets)
    assert set(kwargs) == {'market', 'commission', 'slippage', 'funding', 'policy', 'keep_fills'}
    assert kwargs['market'] == 'crypto'
    assert kwargs['commission'] == s.study.COMMISSION
    assert kwargs['slippage'] == s.study.SLIPPAGE
    assert kwargs['funding'] == s.study.funding_rates(p.fields['funding'], targets.index)
    assert kwargs['policy'] == s.study.q.RebalancePolicy(None, rebalance_on_signal_change=True)
    assert kwargs['keep_fills'] is True
    with pytest.raises(ValueError, match='calendar'):
        s.target_backtest(p, targets.iloc[:-1], start, end)
