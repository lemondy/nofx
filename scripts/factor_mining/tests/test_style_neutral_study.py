"""Synthetic-only tests for the frozen style-neutralization study."""
import hashlib
import json
from pathlib import Path

import numpy as np
import pandas as pd
import pytest

from scripts.factor_mining import style_neutral_study as s
from scripts.factor_mining.panel import Panels
from scripts.factor_mining.tests.synth import planted, write_market


def panels(market='crypto', rows=240, names=25):
    p = planted(rows=rows, columns=names, market=market)
    if market == 'crypto':
        p.fields = {key: value.rename(columns={'S000': 'BTCUSDT'}) for key, value in p.fields.items()}
        p.mask = p.mask.rename(columns={'S000': 'BTCUSDT'})
        p.fields['funding'] = p.fields['close'] * 0 + .00001
    return p


@pytest.mark.parametrize('market,short,medium', [('crypto', 7, 30), ('us', 21, 126)])
def test_every_style_formula_and_causality(market, short, medium):
    p = panels(market)
    close, volume = p.fields['close'], p.fields['volume']
    result = s.style_variables(p, market)
    t = 180
    rets = close.pct_change(fill_method=None)
    benchmark = rets['BTCUSDT'] if market == 'crypto' else p.benchmark_close.pct_change(fill_method=None)
    np.testing.assert_allclose(result['S1'].iloc[t], np.log((close * volume).iloc[t-29:t+1].mean()))
    np.testing.assert_allclose(result['S3'].iloc[t], rets.iloc[t-29:t+1].std(ddof=1))
    np.testing.assert_allclose(result['S4'].iloc[t], close.iloc[t] / close.iloc[t-short] - 1)
    np.testing.assert_allclose(result['S5'].iloc[t], close.iloc[t] / close.iloc[t-medium] - 1)
    for symbol in close:
        joined = pd.concat([rets[symbol], benchmark], axis=1).iloc[t-59:t+1].dropna().to_numpy()
        beta = np.linalg.lstsq(np.column_stack([np.ones(len(joined)), joined[:, 1]]), joined[:, 0], rcond=None)[0][1]
        assert result['S2'].iloc[t][symbol] == pytest.approx(beta, abs=1e-12)
    assert result['S1'].iloc[:29].isna().all().all()
    assert result['S3'].iloc[:30].isna().all().all()
    assert result['S4'].iloc[:short].isna().all().all()
    assert result['S5'].iloc[:medium].isna().all().all()
    for value in p.fields.values():
        value.iloc[t+1:] *= 100
    if p.benchmark_close is not None:
        p.benchmark_close.iloc[t+1:] *= .01
    after = s.style_variables(p, market)
    for key in s.STYLES:
        pd.testing.assert_frame_equal(result[key].iloc[:t+1], after[key].iloc[:t+1])


def test_beta_pairwise_missing_and_forty_observation_boundary():
    p = panels(rows=100)
    # Missing prices create missing returns on the current and following bar.
    p.fields['close'].iloc[55:60, 1] = np.nan
    result = s.style_variables(p, 'crypto')['S2']
    assert result.iloc[:40].isna().all().all()
    assert result.iloc[40, 0] == pytest.approx(1.)
    rets = p.fields['close'].pct_change(fill_method=None)
    pair = pd.concat([rets.iloc[:, 1], rets['BTCUSDT']], axis=1).iloc[21:81].dropna().to_numpy()
    expected = np.linalg.lstsq(np.column_stack([np.ones(len(pair)), pair[:, 1]]), pair[:, 0], rcond=None)[0][1]
    assert result.iloc[80, 1] == pytest.approx(expected)
    p.fields['close'].iloc[:, 1] = np.nan
    assert s.style_variables(p, 'crypto')['S2'].iloc[:, 1].isna().all()


def regression_fixture(names=30):
    rng = np.random.default_rng(51)
    index = pd.date_range('2026-01-01', periods=5)
    columns = [f'C{i}' for i in range(names)]
    frame = lambda: pd.DataFrame(rng.normal(size=(len(index), names)), index=index, columns=columns)
    styles = {key: frame() for key in s.STYLES}
    mask = pd.DataFrame(True, index=index, columns=columns)
    return styles, mask, frame()


def test_residual_zero_mean_and_orthogonal_with_intercept():
    styles, mask, values = regression_fixture()
    fit = s.Neutralizer(styles, mask)
    residual, r2 = fit.fit(values)
    np.testing.assert_allclose(residual.mean(axis=1), 0, atol=2e-15)
    for i in range(len(values)):
        np.testing.assert_allclose(fit.x[i].T @ residual.iloc[i].to_numpy(), 0, atol=2e-14)
    assert r2.between(0, 1).all()


def test_exact_linear_style_rank_factor_is_removed():
    styles, mask, _ = regression_fixture()
    values = s.percentile_ranks(styles['S1'], mask) * 3 + 2
    residual, r2 = s.Neutralizer(styles, mask).fit(values)
    np.testing.assert_allclose(residual, 0, atol=1e-13)
    np.testing.assert_allclose(r2, 1, atol=2e-15)


def test_missing_styles_factor_mask_and_rank_before_listwise_deletion():
    styles, mask, values = regression_fixture()
    styles['S2'].iloc[0, 0] = np.nan
    styles['S3'].iloc[0, 1] = np.inf
    values.iloc[1, 0] = np.nan
    mask.iloc[2, 0] = False
    fit = s.Neutralizer(styles, mask)
    residual, _ = fit.fit(values)
    assert residual.iloc[0, :2].isna().all()
    assert pd.isna(residual.iloc[1, 0])
    assert pd.isna(residual.iloc[2, 0])
    assert residual.iloc[0, 2:].notna().all()
    ranked = s.percentile_ranks(values, mask).iloc[0].to_numpy()
    use = np.isfinite(fit.x[0]).all(axis=1) & np.isfinite(ranked)
    x = np.column_stack([np.ones(use.sum()), fit.x[0, use]])
    expected = ranked[use] - x @ np.linalg.lstsq(x, ranked[use], rcond=None)[0]
    np.testing.assert_allclose(residual.iloc[0, use], expected)


def test_fewer_than_fifteen_complete_names_means_all_nan():
    styles, mask, values = regression_fixture(names=15)
    styles['S1'].iloc[0, 0] = np.nan
    mask.iloc[1, 0] = False
    values.iloc[2, 0] = np.nan
    residual, r2 = s.Neutralizer(styles, mask).fit(values)
    assert residual.iloc[:3].isna().all().all()
    assert r2.iloc[:3].isna().all()
    assert residual.iloc[3:].notna().all().all()
    styles, mask, values = regression_fixture(names=14)
    assert s.Neutralizer(styles, mask).fit(values)[0].isna().all().all()


def test_percentile_rank_average_ties_and_mask():
    values = pd.DataFrame([[1., 1., 3., 100., np.inf]])
    mask = pd.DataFrame([[True, True, True, False, True]])
    np.testing.assert_allclose(s.percentile_ranks(values, mask), [[0, 0, .5, np.nan, np.nan]], equal_nan=True)


def test_week_cluster_se_hand_example_iso_year_boundary():
    # Sunday belongs to the prior Monday, even across the ISO year boundary.
    dates = pd.to_datetime(['2025-12-28', '2025-12-29', '2026-01-04', '2026-01-05'])
    result = s.weekly_statistics(pd.Series([1., 2., 4., 5.], index=dates))
    # mean=3; cluster sums=-2, 0, 2; no G/(G-1) correction.
    assert result['mean_delta'] == 3
    assert result['se'] == pytest.approx(np.sqrt(8) / 4)
    assert result['z'] == pytest.approx(3 / (np.sqrt(8) / 4))
    assert result['n_weeks'] == 3
    assert result['cluster_sums'] == {'2025-12-22': -2., '2025-12-29': 0., '2026-01-05': 2.}
    assert s.weekly_statistics(pd.Series([0., 0.], index=dates[:2]))['z'] == 0
    assert math_is_positive_infinity(s.weekly_statistics(pd.Series([1., 1.], index=dates[:2]))['z'])
    with pytest.raises(ValueError):
        s.weekly_statistics(pd.Series([np.nan], index=dates[:1]))
    with pytest.raises(ValueError):
        s.weekly_statistics(pd.Series([1., 2.], index=[dates[0], dates[0]]))


def math_is_positive_infinity(value):
    return np.isinf(value) and value > 0


@pytest.mark.parametrize('z,val,forward,expected', [
    (1., .1, .2, 'screen_pass'), (2., .1, .2, 'screen_pass'),
    (np.inf, .1, .2, 'screen_pass'), (.99999, .1, .2, 'fail'),
    (-2., .1, .2, 'fail'), (1., 0., .2, 'fail'), (1., -.1, .2, 'fail'),
    (1., .1, 0., 'fail'), (1., .1, -.2, 'fail'),
    (None, .1, .2, 'fail'), (np.nan, .1, .2, 'fail'),
    (1., None, .2, 'fail'), (1., .1, None, 'fail'), (1., np.nan, .2, 'fail'),
])
def test_all_verdict_branches(z, val, forward, expected):
    assert s.verdict(z, val, forward) == expected


def test_C_sorted_content_hash_and_guard(tmp_path):
    base = tmp_path / 'crypto'
    (base / 'daily').mkdir(parents=True)
    contents = {'zoo.json': b'{"factors":[]}', 'ensemble.json': b'{}',
                'daily/z.json': b'last', 'daily/B.json': b'first', 'daily/a.json': b'middle'}
    for name, data in reversed(list(contents.items())):
        (base / name).write_bytes(data)
    actual, count = s.input_hashes(tmp_path, 'crypto')
    expected_bytes = b''.join(contents[key] for key in sorted(contents, key=lambda x: x.encode()))
    assert count == len(contents)
    assert actual['crypto.aggregate'] == hashlib.sha256(expected_bytes).hexdigest()
    assert actual['crypto.zoo'] == hashlib.sha256(contents['zoo.json']).hexdigest()
    assert s.guard_hashes(actual, expected=actual) == {}
    with pytest.raises(ValueError, match='refusing'):
        s.guard_hashes(actual)
    assert s.guard_hashes(actual, True)
    (base / 'daily/a.json').write_bytes(b'drift')
    drifted = s.input_hashes(tmp_path, 'crypto')[0]
    with pytest.raises(ValueError):
        s.guard_hashes(drifted, expected=actual)
    assert s.guard_hashes(drifted, True, expected=actual)['crypto.aggregate']['actual'] == drifted['crypto.aggregate']


def test_output_paths_must_not_overlap_inputs_or_each_other(tmp_path):
    root = tmp_path / 'input'
    with pytest.raises(ValueError):
        s.validate_outputs([root], [root / 'result.json'])
    with pytest.raises(ValueError):
        s.validate_outputs([root], [tmp_path / 'result', tmp_path / 'result'])


def minimal_result():
    return dict(crypto=dict(paired=dict(verdict='fail', z=-.3),
                            segments={key: dict(paired=dict(mean_delta=-.01),
                                                arms={arm: dict(ic=dict(ic=.02)) for arm in ('B', 'N')})
                                      for key in ('val', 'forward')}),
                hashes={'crypto.aggregate': 'abc'}, deviations=[])


def test_temp_ledger_row_fields_and_K(tmp_path):
    ledger = tmp_path / 'ledger.jsonl'
    for _ in range(3):
        row = s.append_ledger(ledger, minimal_result())
    rows = [json.loads(line) for line in ledger.read_text().splitlines()]
    assert len(rows) == 3
    assert row['K'] == row['preregistered_K'] == 3
    assert rows[0]['K'] == 1
    for key, expected in dict(track='fq_crypto', stage='S1', change_type='E',
                              idea_id='fq-crypto-style-neutral-v1', baseline_version='ensemble@5e3599f8',
                              verdict='fail', z=-.3).items():
        assert row[key] == expected
    assert row['input_hashes']['crypto.aggregate'] == 'abc'
    assert set(row['segment_delta']) == {'val', 'forward'}


def write_crypto_snapshot(root):
    write_market(root, rows=240, columns=20)
    base = root / 'crypto'
    (base / 'daily/S000.json').rename(base / 'daily/BTCUSDT.json')
    index = pd.date_range('2020-01-01', periods=240)
    for symbol in ['BTCUSDT'] + [f'S{i:03}' for i in range(1, 20)]:
        path = base / 'alt_daily/funding' / f'{symbol}.json'
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps([[s.study.day(d), .00001] for d in index]))
    factors = [dict(id=f'f{h}', horizon=h, expr='$close/$open-1', direction=1,
                    status='accepted', train=dict(t=3.)) for h in (1, 3, 7)]
    (base / 'zoo.json').write_text(json.dumps(dict(factors=factors)))
    (base / 'ensemble.json').write_text(json.dumps(dict(horizons={str(h): dict(factor_ids=[f'f{h}'])
                                                               for h in (1, 3, 7)})))
    return index


def test_tiny_end_to_end_cli_real_backtrader_and_us_description(tmp_path, monkeypatch):
    crypto_root, us_root, out = tmp_path / 'c', tmp_path / 'u', tmp_path / 'out'
    index = write_crypto_snapshot(crypto_root)
    write_market(us_root, market='us', rows=240, columns=20)
    us_factor = dict(id='u1', horizon=10, expr='$close/$open-1', direction=1,
                     status='candidate', train=dict(t=3.))
    (us_root / 'us/zoo.json').write_text(json.dumps(dict(factors=[us_factor])))
    (us_root / 'us/ensemble.json').write_text('{}')
    monkeypatch.setattr(s, 'VAL_RANGE', (s.study.day(index[145]), s.study.day(index[175])))
    monkeypatch.setattr(s.study, 'VAL_RANGE', (s.study.day(index[141]), s.study.day(index[175])))
    monkeypatch.setattr(s.study, 'FORWARD_START', s.study.day(index[192]))
    before = {str(p): p.read_bytes() for root in (crypto_root, us_root) for p in root.rglob('*') if p.is_file()}
    result = s.main(['--crypto-root', str(crypto_root), '--us-root', str(us_root),
                     '--out', str(out), '--ledger', str(out / 'ledger.jsonl'), '--allow-input-drift'])
    assert (out / 'report.md').exists()
    assert json.loads((out / 'report.json').read_text())['hashes'] == result['hashes']
    assert result['crypto']['reproduction']['score_exact_match']
    assert result['crypto']['paired']['n'] == 31 + 48
    for segment in result['crypto']['segments'].values():
        for arm in ('B', 'N'):
            a = segment['arms'][arm]
            assert a['backtest']['engine_version']
            assert a['backtest']['metrics']['funding_observations'] > 0
            assert set(a['legs']) == {'long', 'short'}
            assert a['ic']['n'] > 0
        b, n = (pd.Series(segment['arms'][arm]['daily_returns']) for arm in ('B', 'N'))
        np.testing.assert_allclose(pd.Series(segment['paired']['daily_delta']), n - b)
    assert result['us']['n'] == 1
    assert result['us']['neutral_ic']['n'] == 1
    assert len((out / 'ledger.jsonl').read_text().splitlines()) == 1
    assert all(Path(path).read_bytes() == data for path, data in before.items())
    report = (out / 'report.md').read_text()
    assert all(text in report for text in ('主检验', '美股描述', '偏离', '输入哈希', 'Accepted zoo'))


def test_us_selection_candidate_only_frozen_direction_and_training_order():
    p = panels('us')
    factors = [dict(id=f'u{i:03}', horizon=10, expr='$close/$open-1', direction=-1,
                    status='candidate', train=dict(t=float(i))) for i in range(3)]
    factors += [dict(factors[0], id='accepted', status='accepted', train=dict(t=999.)),
                dict(factors[0], id='rejected', status='rejected', train=dict(t=999.)),
                dict(factors[0], id='h20', horizon=20, train=dict(t=999.))]
    result = s.us_study(p, dict(factors=factors), progress=lambda msg: None)
    assert [f['id'] for f in result['rows']] == ['u002', 'u001', 'u000']
    assert all(f['direction'] == -1 for f in result['rows'])
    assert result['nw_lag'] == 10
