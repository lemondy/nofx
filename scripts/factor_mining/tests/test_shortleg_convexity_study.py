"""Synthetic-only checks of the frozen H3 definitions and replica accounting."""
import copy
from pathlib import Path

import numpy as np
import pandas as pd
import pytest

from scripts.factor_mining import shortleg_convexity_study as s
from scripts.factor_mining.tests.synth import planted


def synthetic(rows=85, names=15):
    panels = planted(rows=rows, columns=names)
    rng = np.random.default_rng(73)
    panels.fields['funding'] = pd.DataFrame(
        rng.normal(.0001, .0002, panels.mask.shape),
        index=panels.mask.index, columns=panels.mask.columns)
    score = pd.DataFrame(rng.normal(size=panels.mask.shape),
                         index=panels.mask.index, columns=panels.mask.columns)
    return panels, score


def test_identity_and_convexity_for_both_legs():
    returns = np.array([-.999, -.4, -.01, 0., .05, .3, 2., 20.])
    log = np.log1p(returns)
    assert np.all(returns - log >= 0)
    for leg in ('long', 'short'):
        parts = s.decomposition(returns, leg, np.zeros(len(returns)))
        assert parts['P'] == pytest.approx(parts['L'] + parts['C'], abs=1e-14)
    assert s.decomposition(returns, 'long', returns * 0)['C'] >= 0
    assert s.decomposition(returns, 'short', returns * 0)['C'] <= 0


def test_long_short_signs_and_fixed_notional_funding():
    long = s.decomposition([.1, -.02], 'long', [.003, .009])
    short = s.decomposition([.1, -.02], 'short', [.003, .009])
    assert long['P'] == pytest.approx(.04)
    assert short['P'] == pytest.approx(-.04)
    assert long['F'] == pytest.approx(-.006)
    assert short['F'] == pytest.approx(.006)
    for key in ('P', 'L', 'C', 'F'):
        assert long[key] == -short[key]
    assert s.decomposition([.2], 'short', [-.01])['F'] == -.01
    with pytest.raises(ValueError):
        s.decomposition([-1.], 'long', [0.])


@pytest.mark.parametrize('p,log,c,rho,label', [
    (-1., 0., -1., 1., '机制成立（强）'),
    (-1., 1., -2., 2., '机制成立（强）'),
    (-3., -2., -1., 1 / 3, '机制部分成立'),
    (-3., -2.000000001, -.999999999, .999999999 / 3, '机制不成立'),
    (-3., -1., -2., 2 / 3, '机制部分成立'),
    (0., 1., -1., None, '机制不成立'),
    (1., 2., -1., None, '机制不成立'),
])
def test_verdict_truth_table_and_one_third_boundary(p, log, c, rho, label):
    result = s.verdict(p, log, c)
    if rho is None:
        assert result['rho'] is None
    else:
        assert result['rho'] == pytest.approx(rho)
    assert result['verdict'] == label


def test_spearman_invariant_with_ties_and_all_comparison_metrics():
    returns = np.array([-.95, -.3, -.1, -.1, 0, .2, .4, 2., 8., 10.])
    score = np.array([2., 2., 6., 5., 4., 3., 1., 7., 8., 9.])
    rows = pd.DataFrame(dict(R=returns, l=np.log1p(returns), score=score,
                             rank_score=pd.Series(score).rank(), eligible=True, included=True,
                             leg=['short'] * 2 + ['neutral'] * 6 + ['long'] * 2))
    result = s.signal_comparison(rows)
    assert result['spearman_R'] == pytest.approx(result['spearman_l'], abs=1e-12)
    assert result['pearson_rank_R'] != pytest.approx(result['pearson_rank_l'])
    assert result['spread_R'] == pytest.approx(9.625)
    assert result['spread_l'] == pytest.approx(rows.l.iloc[-2:].mean() - rows.l.iloc[:2].mean())


def test_volatility_is_trailing_ddof_one_and_minimum_twenty_observations():
    panels, _ = synthetic()
    close = panels.fields['close']
    date = close.index[45]
    actual = s.ex_ante_vol(close)
    expected = np.log(close.iloc[16:46].to_numpy() / close.iloc[15:45].to_numpy()).std(axis=0, ddof=1)
    np.testing.assert_allclose(actual.loc[date], expected, atol=1e-14)
    assert actual.iloc[:20].isna().all().all()
    assert actual.iloc[20].notna().all()
    changed = close.copy()
    changed.loc[changed.index > date] *= np.linspace(2, 100, (changed.index > date).sum())[:, None]
    pd.testing.assert_frame_equal(s.ex_ante_vol(changed).loc[:date], actual.loc[:date])
    sparse = close.copy()
    sparse.iloc[16:23, 0] = np.nan
    # Seven missing prices invalidate eight returns, leaving 22 observations.
    assert np.isfinite(s.ex_ante_vol(sparse).loc[date].iloc[0])
    sparse.iloc[16:29, 0] = np.nan
    assert np.isnan(s.ex_ante_vol(sparse).loc[date].iloc[0])


def test_quintiles_stable_ties_eligibility_missing_and_remainder():
    values = np.array([3., 1., 1., 5., 2., 6., 7., 8., 9., 10., 11., 12., np.nan, -100.])
    eligible = np.ones(len(values), bool)
    eligible[-1] = False
    groups = s.quintiles(values, eligible)
    np.testing.assert_array_equal(groups, [2, 1, 1, 2, 1, 2, 3, 3, 4, 4, 5, 5, 0, 0])
    np.testing.assert_array_equal(s.quintiles(np.ones(10), np.ones(10, bool)), np.repeat(np.arange(1, 6), 2))
    np.testing.assert_array_equal(s.quintiles([1, 2], [False, False]), [0, 0])


def test_tail_share_uses_period_equal_weights_and_strict_threshold():
    returns = np.array([.5, -.1, .3, .8, -.2, -.1])
    rows = pd.DataFrame(dict(segment='val', period=['p1', 'p1', 'p2', 'p2', 'p2', 'p2'],
                             leg='short', included=True, R=returns,
                             c=returns - np.log1p(returns)))
    result = s.tail_share(rows)
    weights = np.array([.5, .5, .25, .25, .25, .25])
    tail = returns > .3
    assert result['P_share'] == pytest.approx((weights[tail] * returns[tail]).sum() / (weights * returns).sum())
    assert result['C_share'] == pytest.approx((weights[tail] * rows.c.to_numpy()[tail]).sum() / (weights * rows.c).sum())
    assert result['tail_n'] == 2
    rows['included'] = False
    assert s.tail_share(rows)['C_share'] is None


@pytest.mark.parametrize('mode', ['normal', 'unchanged', 'missing'])
def test_exact_reconciliation_against_existing_replica(mode):
    panels, score = synthetic()
    start, end = score.index[35], score.index[-1]
    if mode == 'unchanged':
        score.iloc[:] = np.arange(score.shape[1])
    if mode == 'missing':
        # Missing held opens, a missing exit, and a missing rate exercise stale
        # positions, feasible-budget scaling, zero funding, and stale marks.
        score.iloc[:] = np.arange(score.shape[1])
        score.iloc[38:, :] = np.arange(score.shape[1])[::-1]
        panels.fields['open'].iloc[39, [0, 14]] = np.nan
        panels.fields['open'].iloc[42:46, 14] = np.nan
        panels.fields['close'].iloc[44, 14] = np.nan
        panels.fields['funding'].iloc[37, 0] = np.nan
    rows, periods, counts = s.build_periods(panels, score, start, end, 'val', 'B')
    result = s.reconcile(panels, score, start, end, rows, periods, counts)
    assert result['max_abs_daily_return_error'] <= 1e-12
    assert result['total_return_error'] <= 1e-12
    assert result['compounded_period_return'] == pytest.approx(result['main_total_return'], abs=1e-12)
    assert len(rows) == len(periods) * score.shape[1]
    assert rows.final_close.any()
    assert np.all(rows.c >= 0)
    if mode == 'missing':
        assert counts['missing_execution_path_events'] > 0
        assert counts['stale_position_periods'] > 0
        assert counts['excluded_selected_coin_periods'] > 0
        kept = rows.loc[rows.period.eq('2020-02-08') & rows.coin.eq('S014')].iloc[0]
        assert kept.quantity == pytest.approx(kept.previous_quantity)
    if mode == 'unchanged':
        assert counts['unchanged_target_periods'] == len(periods) - 1
        held = rows.loc[rows.coin.eq('S000')]
        np.testing.assert_allclose(held.quantity, held.quantity.iloc[0], atol=0)
        np.testing.assert_allclose(held.funding_basis, held.funding_basis.iloc[0], atol=0)


def test_period_return_and_funding_manually_include_entry_exclude_exit():
    panels, score = synthetic(rows=40, names=5)
    score.iloc[:] = np.arange(5)
    panels.fields['funding'].iloc[:] = .001
    start, end = score.index[31], score.index[35]
    rows, periods, _ = s.build_periods(panels, score, start, end, 'val', 'B')
    first = rows.loc[rows.period.eq(s.study.day(start))]
    assert np.allclose(first.funding, .009)
    expected = panels.fields['open'].iloc[35] / panels.fields['open'].iloc[32] - 1
    np.testing.assert_allclose(first.R, expected)
    short, long = first.loc[first.leg.eq('short')].iloc[0], first.loc[first.leg.eq('long')].iloc[0]
    assert short.F == pytest.approx(.009)
    assert long.F == pytest.approx(-.009)
    assert short.funding_pnl == pytest.approx(-short.funding_basis * .009)
    assert periods[0]['cost'] == pytest.approx(s.study.q.CAPITAL * .0008)
    last = rows.loc[rows.period.ne(s.study.day(start))]
    assert last.final_close.all()
    assert last.holding_days.eq(0).all()
    assert np.allclose(last.funding, .003)


def test_vol_quintile_does_not_use_future_prices_or_future_availability():
    panels, score = synthetic()
    start, end = score.index[35], score.index[47]
    rows, _, _ = s.build_periods(panels, score, start, end, 'val', 'B')
    changed = copy.deepcopy(panels)
    changed.fields['close'].iloc[36:] *= 30
    changed.fields['open'].iloc[36, 0] = np.nan
    other, _, _ = s.build_periods(changed, score, start, end, 'val', 'B')
    first = rows.loc[rows.period.eq(s.study.day(start))]
    first_other = other.loc[other.period.eq(s.study.day(start))]
    np.testing.assert_array_equal(first.vol_quintile, first_other.vol_quintile)
    np.testing.assert_array_equal(first.sigma_hat, first_other.sigma_hat)


def test_combined_components_ic_statistics_and_sign_check():
    panels, score = synthetic()
    val, _, _ = s.build_periods(panels, score, score.index[35], score.index[55], 'val', 'B')
    forward, _, _ = s.build_periods(panels, score, score.index[60], score.index[-1], 'forward', 'B')
    a, b, combined = s.describe(val), s.describe(forward), s.describe(pd.concat([val, forward]))
    for leg in ('long', 'short'):
        for key in ('P', 'L', 'C', 'F'):
            assert combined['legs'][leg][key] == pytest.approx(a['legs'][leg][key] + b['legs'][leg][key])
        assert combined['legs'][leg]['compounded_price'] == pytest.approx(
            (1 + a['legs'][leg]['compounded_price']) * (1 + b['legs'][leg]['compounded_price']) - 1)
    assert combined['net_convexity'] == pytest.approx(.5 * (combined['legs']['long']['C'] + combined['legs']['short']['C']))
    assert combined['ic']['spearman_R'] == combined['ic']['spearman_l']
    values = pd.Series([v['spread_R'] for v in combined['signal_statistics']])
    assert combined['ic']['spread_R']['t'] == pytest.approx(values.mean() / (values.std(ddof=1) / np.sqrt(len(values))))
    assert combined['sign_check']['arithmetic_ls'] == pytest.approx(values.sum())
    assert combined['ols']['n'] == sum(v['n'] for v in combined['vol_quintiles'])


@pytest.mark.parametrize('root,out', [
    ('/synthetic/snapshot/crypto', '/synthetic/snapshot/crypto/out'),
    ('/synthetic/snapshot/crypto', '/synthetic/snapshot/crypto'),
    ('/synthetic/snapshot', '/synthetic/snapshot/crypto/out'),
    ('/synthetic/snapshot/crypto', '/synthetic/snapshot/crypto/../crypto/out'),
])
def test_out_refusal_before_loading_inputs(root, out):
    with pytest.raises(ValueError, match='read-only --root'):
        s.main(['--root', root, '--ml-report', '/does-not-exist.json', '--out', out])


def test_output_outside_snapshot_is_accepted_without_writes():
    root, out = s.validate_out('/synthetic/snapshot/crypto', '/synthetic/output')
    assert root == Path('/synthetic/snapshot')
    assert out == Path('/synthetic/output')
