"""Offline preregistration checks: synthetic prices/snapshots and temporary ledgers."""
import contextlib
import copy
import datetime as dt
import hashlib
import io
import json
from unittest.mock import patch

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import evolution_ledger
from scripts.factor_mining import evaluate, panel, quant_targets, options
from scripts.factor_mining import pit60_hedge_study as s
from scripts.factor_mining.tests.synth import planted, SIGNAL_EXPR
from scripts.factor_mining.tests.test_ensemble_forward_study import synthetic, snapshots
from scripts.factor_mining.tests.test_wide_pool_study import statistics


def factor(id_, expr=SIGNAL_EXPR, t=8., **kwargs):
    return dict(id=id_, expr=expr, horizon=3, direction=1, family='momentum',
                first_seen=1, run_id='synthetic', status='accepted',
                train=dict(t=t, ic=.1, ic_halves=[.1, .1], ls_ret=.2),
                val=dict(t=3., ic=.1, ls_ret=.2), **kwargs)


def snapshot(panels, date, z=None):
    z = np.arange(panels.mask.shape[1]) if z is None else z
    return dict(as_of=str(date.date()), horizons={'3': dict(factor_ids=['a'], scores={
        str(symbol): dict(z=round(float(value), 4), pct=round((i + 1) / len(z), 4))
        for i, (symbol, value) in enumerate(zip(panels.mask.columns, z))})})


@pytest.fixture(scope='module')
def inputs(tmp_path_factory):
    """Actual crypto-layout JSON files, entirely synthetic, with the frozen calendar."""
    root = tmp_path_factory.mktemp('pit60-inputs')
    base, full, frozen = root / 'crypto', root / 'crypto_full', root / 'snapshot'
    dates = pd.date_range('2021-01-01', '2026-11-02')
    p = planted(rows=len(dates), columns=20)
    columns = ['BTCUSDT'] + [f'COIN{i:02}USDT' for i in range(1, 20)]
    for frame in list(p.fields.values()) + [p.mask]:
        frame.index, frame.columns = dates, columns
    (base / 'daily').mkdir(parents=True)
    (base / 'alt_daily/funding').mkdir(parents=True)
    (base / 'alt_daily/premium').mkdir(parents=True)
    for symbol in columns:
        data = [[str(d.date()), *(float(p.fields[f].at[d, symbol]) for f in panel.FIELD_NAMES)] for d in dates]
        (base / 'daily' / f'{symbol}.json').write_text(json.dumps(data))
        for field, value in (('funding', .00001), ('premium', .0001)):
            (base / 'alt_daily' / field / f'{symbol}.json').write_text(
                json.dumps([[str(d.date()), value] for d in dates]))
    (base / 'data_meta.json').write_text(json.dumps(dict(updated_at='synthetic')))
    frozen.mkdir()
    factors = [factor('strong'), factor('duplicate', t=1.), factor('bad', expr='bad(')]
    (frozen / 'zoo.json').write_text(json.dumps(dict(factors=factors, split=dict(
        windows=s.config.MARKETS['crypto']['windows']))))
    (frozen / 'ensemble.json').write_text(json.dumps(dict(horizons={'3': dict(factor_ids=['strong'])})))
    full.mkdir()
    for name in ('daily', 'alt_daily', 'data_meta.json'):
        (full / name).symlink_to(base / name)
    (full / 'validation.json').write_text(json.dumps(dict(synthetic=True)))
    return base, frozen, full


def test_split_range_assertions_are_strict_for_dates_and_windows():
    p, _ = synthetic(n=len(pd.date_range('2021-01-01', '2026-11-02')), names=2)
    dates = pd.date_range('2021-01-01', '2026-11-02')
    for frame in list(p.fields.values()) + [p.mask]:
        frame.index = dates
    p.windows = dict(s.config.MARKETS['crypto']['windows'])
    zoo = dict(split=dict(windows=p.windows))
    split = s.assert_splits(p, zoo)
    assert split['ranges']['train'] == ('2022-05-08', '2025-05-01')
    assert split['ranges']['val'] == ('2025-05-07', '2026-05-01')
    assert split['ranges']['holdout'][0] == '2026-05-07'
    with pytest.raises(AssertionError, match='range drift'):
        s.assert_splits(s.truncate_panels(p), zoo)
    with pytest.raises(AssertionError, match='windows disagree'):
        s.assert_splits(p, dict(split=dict(windows=dict(train=100, val=50, holdout=20))))


def test_reacceptance_deterministic_fork_and_fixed_selection_threshold():
    p = planted(rows=360, columns=20)
    candidates = [factor('a'), factor('b', expr='-($close/$open-1)'),
                  factor('missing', expr='$funding'), factor('constant', expr='$close*0')]
    one, selected_one = s.reaccept(p, candidates, workers=1)
    two, selected_two = s.reaccept(p, candidates, workers=2)
    assert one == two
    assert selected_one == selected_two
    assert one[0]['direction'] == 1 and one[1]['direction'] == -1
    assert one[2]['status'] == 'invalid_expr'
    assert one[3]['status'] == 'no_coverage'
    assert selected_one
    original = evaluate.classify
    with patch.object(s.evaluate, 'classify', wraps=original) as classify:
        s.reaccept(p, candidates, workers=1)
    assert classify.call_count == 2
    for call in classify.call_args_list:
        assert call.args[2:] == (16502, evaluate.t_required(16502))
    # Force a t that would pass K=1 but fails the preregistered K_SEL.
    metrics = dict(direction=1, train=dict(t=3., ic=.1, ic_halves=[.1, .1], ls_ret=.1),
                   val=dict(t=3., ic=.1, ls_ret=.1))
    with patch.object(s.evaluate, 'evaluate_factor', return_value=metrics):
        rows, selected = s.reaccept(p, candidates[:1], workers=1)
    assert rows[0]['status'] == 'rejected' and selected == []
    with patch.dict('os.environ', {options.GREEDY_ENSEMBLE: '1'}):
        with pytest.raises(AssertionError, match='must be off'):
            s.reaccept(p, candidates, workers=1)
    with pytest.raises(ValueError, match='positive'):
        s.reaccept(p, candidates, workers=0)


def test_redundancy_order_signed_correlation_and_candidate_exclusion():
    p = planted(rows=100, columns=20)
    split = panel.make_splits(p.mask.index, 3)
    rows = [factor('low', t=5.), factor('high', t=9.), factor('candidate', t=10.)]
    rows[-1]['status'] = 'candidate'
    s.remove_redundancy(rows, p, split)
    assert rows[0]['status'] == 'redundant'
    assert rows[0]['corr_with'] == 'high'
    assert rows[0]['corr_max'] == pytest.approx(1.)
    assert rows[1]['status'] == 'accepted'
    assert rows[2]['status'] == 'candidate'
    # Stable tie breakers match miner, and exactly 0.8 is redundant.
    rows = [factor('z'), factor('a'), factor('early')]
    rows[-1]['first_seen'] = 0
    expected_dates = p.mask.index[split['train'] | split['val']][::5]
    def corr(a, b, dates):
        pd.testing.assert_index_equal(dates, expected_dates)
        pd.testing.assert_index_equal(a.index, expected_dates)
        return s.config.CORR_REDUNDANT
    with patch.object(s.evaluate, 'cross_corr', side_effect=corr):
        s.remove_redundancy(rows, p, split)
    assert rows[-1]['status'] == 'accepted'
    assert all(f['corr_with'] == 'early' for f in rows[:2])
    # Miner compares signed correlations, not their absolute values.
    rows = [factor('a'), factor('inverse', expr='-($close/$open-1)', t=7.)]
    s.remove_redundancy(rows, p, split)
    assert all(f['status'] == 'accepted' for f in rows)


def test_long_sleeve_matches_build_targets_positive_part_and_is_causal():
    p, score = synthetic(n=140, names=40)
    date = p.mask.index[110]
    for values in (np.arange(40), np.zeros(40), score.loc[date].to_numpy()):
        doc = snapshot(p, date, values)
        expected = quant_targets.build_targets('crypto', doc, p, now=date + dt.timedelta(days=1))
        assert expected['status'] == 'ok'
        actual = s.long_sleeve(p, doc, now=date + dt.timedelta(days=1))
        assert actual['status'] == 'ok'
        assert actual['n_eligible'] == expected['universe_size']
        assert actual['long_weights'] == {k: v for k, v in expected['weights'].items() if v > 0}
    before = s.long_sleeve(p, doc, now=date)
    p.fields['close'].loc[p.mask.index > date] = np.nan
    assert s.long_sleeve(p, doc, now=date) == before
    # The long sleeve remains available when the protected short gate is empty.
    p2, _ = synthetic(n=70, names=20)
    doc2 = snapshot(p2, p2.mask.index[-1])
    assert quant_targets.build_targets('crypto', doc2, p2, now=p2.mask.index[-1])['status'] != 'ok'
    assert s.long_sleeve(p2, doc2, now=p2.mask.index[-1])['status'] == 'ok'


def test_long_sleeve_stale_missing_and_insufficient_handling():
    p, _ = synthetic(n=100, names=20)
    date = p.mask.index[-1]
    doc = snapshot(p, date)
    for mutation in ({'as_of': None}, {'as_of': '2026-01-01'}, {'as_of': '2027-01-01'}):
        changed = dict(doc, **mutation)
        expected = quant_targets.build_targets('crypto', changed, p, now=date)
        actual = s.long_sleeve(p, changed, now=date)
        assert (actual['status'], actual['reason']) == (expected['status'], expected['reason'])
    assert s.long_sleeve(p, {}, now=date)['status'] == 'no_validated_factors'
    p.mask.loc[date, p.mask.columns[:6]] = False
    assert s.long_sleeve(p, doc, now=date)['reason'] == 'fewer_than_15_eligible_symbols'
    p.mask.loc[date] = True
    p.fields['close'].iloc[-1, :6] = np.nan
    assert s.long_sleeve(p, doc, now=date)['n_eligible'] == 14


def test_beta_ols_pairwise_finite_intercept_fallback_and_clip():
    dates = pd.date_range('2026-01-01', periods=30)
    x = pd.Series(np.linspace(-.02, .02, 30), index=dates)
    assert s.beta_ols(2 * x + .003, x) == pytest.approx(2.)
    assert s.beta_ols(-2 * x, x) == 0.
    assert s.beta_ols(4 * x, x) == 3.
    assert s.beta_ols(x.iloc[:19] * 2, x) == 1.
    assert s.beta_ols(x.iloc[:20] * 2, x) == pytest.approx(2.)
    poisoned = 2 * x + .003
    poisoned.iloc[:5] = np.inf
    assert s.beta_ols(poisoned, x) == pytest.approx(2.)
    assert s.beta_ols(x, x * 0) == 1.


def test_hedge_uses_last_30_returns_caps_h_and_nets_btc_long():
    p, _ = synthetic(n=120, names=40)
    date = p.mask.index[100]
    # Put BTC at the top, retaining ordinary finite vol for other symbols.
    doc = snapshot(p, date, [100] + list(range(39)))
    seen = []
    def beta(y, x):
        assert len(y) == len(x) == 30
        assert y.index[-1] == date
        assert y.index[0] == date - dt.timedelta(days=29)
        seen.append(y.name)
        return 3.
    target = s.hedge_targets(p, doc, now=date, beta_fn=beta)
    assert target['status'] == 'ok'
    assert target['h'] == .5
    assert target['mean_beta'] == pytest.approx(3.)
    assert target['weights']['BTCUSDT'] == pytest.approx(target['long_weights']['BTCUSDT'] - .5)
    assert all(w >= 0 for symbol, w in target['weights'].items() if symbol != 'BTCUSDT')
    assert set(seen) == set(target['long_weights'])
    assert sum(abs(w) for w in target['weights'].values()) <= 1.00001
    low = s.hedge_targets(p, doc, now=date, beta_fn=lambda y, x: .2)
    assert low['h'] == pytest.approx(sum(low['long_weights'].values()) * .2)
    assert low['weights']['BTCUSDT'] == pytest.approx(low['long_weights']['BTCUSDT'] - low['h'])
    real = s.hedge_targets(p, doc, now=date)
    returns = p.fields['close'].loc[:date].tail(31).pct_change(fill_method=None).tail(30)
    assert real['betas'] == {name: s.beta_ols(returns[name], returns['BTCUSDT']) for name in real['long_weights']}


def test_hedge_skips_missing_btc_and_retains_previous_target():
    p, score = synthetic(n=130, names=20)
    dates = p.mask.index[100:112]
    p.fields['close'].loc[dates[3], 'BTCUSDT'] = np.nan
    targets, signals = s.hedge_historical_targets(p, score, [factor('a')], dates)
    assert signals[0]['status'] == 'ok'
    assert signals[1]['reason'] == 'missing_btc_close_at_as_of'
    np.testing.assert_array_equal(targets.loc[dates[3]], targets.loc[dates[2]])
    assert [signal['as_of'] for signal in signals] == [str(d.date()) for d in dates[::3]]
    qpanel = copy.deepcopy(p)
    qpanel.fields['close'] = qpanel.fields['close'].drop(columns='BTCUSDT')
    doc = snapshot(qpanel, dates[0])
    assert s.hedge_targets(qpanel, doc, now=dates[0])['reason'] == 'missing_btc_close_at_as_of'


def test_beta_zero_equals_long_only_targets_and_native_equity():
    p, score = synthetic(n=125, names=20)
    dates = p.mask.index[95:]
    targets, signals = s.hedge_historical_targets(p, score, [factor('a')], dates, beta_fn=lambda y, x: 0.)
    long_only = pd.DataFrame(np.nan, index=dates, columns=score.columns)
    for date in dates[::3]:
        row = score.loc[date]
        target = s.long_sleeve(p, snapshot(p, date, row.to_numpy()), now=date + dt.timedelta(days=1))
        long_only.loc[date] = pd.Series(target['long_weights']).reindex(score.columns, fill_value=0.)
    long_only = long_only.ffill().fillna(0.)
    pd.testing.assert_frame_equal(targets, long_only)
    assert all(signal['h'] == 0. for signal in signals)
    def backtest(weights):
        frames, rates, weights = quant_targets._frames_rates(p, weights)
        return s.q.run_weights(frames, weights, market='crypto', commission=.0005, slippage=.0003,
                               funding=rates, keep_fills=True,
                               policy=s.q.RebalancePolicy(frequency=None, rebalance_on_signal_change=True))
    assert backtest(targets)['daily_equity'] == backtest(long_only)['daily_equity']


def test_odd_halves_put_middle_day_in_first_half_and_reuse_cluster_statistic():
    dates = pd.date_range('2026-05-07', periods=101, tz='UTC')
    new = pd.Series(np.arange(101) * .0001, index=dates)
    base = new * 0
    actual = s.paired_statistics(new, base)
    expected = s.paired_diff(new, base)
    for key in ('delta', 'se', 'z', 'n_days', 'n_weeks', 'daily_diff'):
        assert actual[key] == expected[key]
    assert actual['dev_delta'] == pytest.approx(new.iloc[:51].mean())
    assert actual['holdout_delta'] == pytest.approx(new.iloc[51:].mean())
    assert actual['half_ranges'] == [[str(dates[0].date()), str(dates[50].date())],
                                     [str(dates[51].date()), str(dates[-1].date())]]
    assert s.paired_statistics(new.iloc[:0], base.iloc[:0])['half_ranges'] == [[], []]
    assert s.paired_statistics(new.iloc[:1], base.iloc[:1])['holdout_delta'] is None


def test_verdict_tiers_boundaries_and_all_invalid_conditions():
    assert s.verdict(statistics(3.), 2, 5)['verdict'] == 'strong'
    boundary = s.verdict(statistics(), 2, 5)['z_K']
    assert boundary == pytest.approx(2.2414027276)
    assert s.verdict(statistics(boundary), 2, 5)['verdict'] == 'strong'
    assert s.verdict(statistics(), 2, 5)['verdict'] == 'screen_pass'
    assert s.verdict(statistics(1.), 2, 5)['verdict'] == 'screen_pass'
    assert s.verdict(statistics(.999), 2, 5)['verdict'] == 'fail'
    for delta in ('dev_delta', 'holdout_delta'):
        assert s.verdict(dict(statistics(9.), **{delta: 0.}), 2, 5)['verdict'] == 'fail'
    cases = [(dict(statistics(), n_days=99), 5, True, True),
             (statistics(), 4, True, True), (statistics(), 5, False, True), (statistics(), 5, True, False)]
    for stats, accepted, ok, s0 in cases:
        result = s.verdict(stats, 2, accepted, ok, s0)
        assert result['verdict'] == 'invalid' and result['K'] is None
        assert result['invalid_reasons']
    result = s.verdict(dict(statistics(), n_days=99), 1, 4, False, False)
    assert len(result['invalid_reasons']) == 4


def test_ledger_validates_and_counts_only_noninvalid_rows(tmp_path):
    ledger = tmp_path / 'ledger.jsonl'
    def append(**kwargs):
        return s.ledger_result(statistics(), kwargs.get('accepted', 5), True, kwargs.get('s0', True),
                               s.EXPECTED_HASHES, 1.1, .9, ledger, {}, False)
    first = append()
    invalid = append(s0=False)
    second = append()
    assert (first['K'], invalid['K'], second['K']) == (1, None, 2)
    assert first['verdict'] == 'strong' and second['verdict'] == 'screen_pass'
    assert second['z_K'] > first['z_K']
    assert second['change_type'] == 'E' and second['change_types'] == ['E', 'C']
    assert second['baseline_version'] == s.EXPECTED_HASHES['ensemble.json'][:16] + ':ls_invvol_protected_v1'
    assert second['prereg_path'] == s.PREREG
    assert second['gross_exposure_ratio'] == .9
    assert len(evolution_ledger.read_rows(ledger)) == 3
    for row in evolution_ledger.read_rows(ledger):
        evolution_ledger.validate(row)


def test_snapshot_symlinks_truncated_copies_source_unchanged_and_s0(inputs):
    base, frozen, _ = inputs
    before_base, before_frozen = snapshots(base), snapshots(frozen)
    with s.snapshot_root(base, frozen) as root, s.snapshot_root(base, frozen, truncated=True) as copied:
        assert (root / 'crypto/daily').is_symlink()
        assert (root / 'crypto/alt_daily').is_symlink()
        for name in ('zoo.json', 'ensemble.json', 'data_meta.json'):
            assert (root / 'crypto' / name).is_symlink()
            assert (copied / 'crypto' / name).is_symlink()
        selection = panel.load_panels('crypto', root)
        bt = s.truncate_panels(selection)
        loaded_copy = panel.load_panels('crypto', copied)
        assert s.panel_digest(bt) == s.panel_digest(loaded_copy)
        assert str(selection.mask.index[-1].date()) == '2026-11-02'
        assert str(bt.mask.index[-1].date()) == s.END_BT
        for path in (copied / 'crypto/daily').glob('*.json'):
            assert not path.is_symlink()
            assert json.loads(path.read_text())[-1][0] == s.END_BT
        for path in (copied / 'crypto/alt_daily').glob('*/*.json'):
            assert not path.is_symlink()
            assert json.loads(path.read_text())[-1][0] == s.END_BT
        factors, _ = quant_targets._load_frozen(root)
        reproduced = s.reproduce_baseline(copied, bt, factors)
        assert reproduced['matched'], reproduced
        assert reproduced['max_relative_error'] == 0.
        assert reproduced['n_days'] == 181
        assert reproduced['start'] == '2026-05-04'
    assert snapshots(base) == before_base and snapshots(frozen) == before_frozen


def test_s0_detects_interior_mismatch_missing_dates_and_failure_without_crashing():
    p, _ = synthetic(n=10, names=2)
    expected = dict(daily_equity={'2026-05-04': 1000., '2026-05-05': 1001., '2026-05-06': 1002.})
    reference = dict(intervals=dict(forward=dict(start='2026-05-04', end='2026-05-06',
        strategies=dict(ls_invvol_protected_v1=expected))))
    actual = dict(expected['daily_equity'])
    actual['2026-05-05'] = 1001.01
    for changed in (actual, {'2026-05-06': 1002.}):
        with patch.object(s.quant_targets, 'backtest', return_value=reference), \
             patch.object(s, 'run_arm', return_value=dict(ok=True, daily_equity=changed)):
            assert not s.reproduce_baseline('unused', p, [])['matched']
    with patch.object(s.quant_targets, 'backtest', side_effect=RuntimeError('synthetic failure')):
        result = s.reproduce_baseline('unused', p, [])
    assert not result['matched'] and 'synthetic failure' in result['error']


def test_short_tail_weights_actual_rebalance_opens_and_missing():
    p, _ = synthetic(n=8, names=2)
    opens = p.fields['open']
    opens.iloc[:] = 100.
    opens.iloc[4] = [150., 110.]
    targets = pd.DataFrame(0., index=p.mask.index[:4], columns=p.mask.columns)
    targets.iloc[:] = [-.1, -.3]
    tail = s.short_tail(p, targets, targets.index[:1])
    assert tail['n'] == 2 and tail['share_gt_30'] == .5
    assert tail['loss_share_from_gt_30'] == pytest.approx(.05 / (.05 + .03))
    opens.iloc[4, 0] = np.nan
    assert s.short_tail(p, targets, targets.index[:1])['n'] == 1
    assert s.short_tail(p, targets, targets.index[:1])['loss_share_from_gt_30'] == 0.


def test_arm_failure_is_recorded_and_does_not_raise():
    p, score = synthetic(n=100, names=20)
    with patch.object(s.q, 'run_weights', side_effect=RuntimeError('synthetic execution failure')):
        run = s.run_arm(p, score, [], p.mask.index[90:])
    assert not run['ok'] and 'synthetic execution failure' in run['error']


def test_hash_guard_output_guard_and_cli_drift_refusal(inputs, tmp_path):
    base, frozen, _ = inputs
    with pytest.raises(ValueError, match='sha256 mismatch'):
        s.guard_inputs(frozen)
    actual, drift = s.guard_inputs(frozen, True)
    assert set(drift) == set(s.EXPECTED_HASHES)
    assert actual['zoo.json'] == hashlib.sha256((frozen / 'zoo.json').read_bytes()).hexdigest()
    with patch.object(s, 'EXPECTED_HASHES', actual):
        assert s.guard_inputs(frozen) == (actual, {})
    for out in (base, base / 'out', frozen):
        with pytest.raises(ValueError):
            s.validate_outputs(base, frozen, out, None)
    with pytest.raises(ValueError, match='must differ'):
        s.validate_outputs(base, frozen, tmp_path, tmp_path / 'report.json')
    with pytest.raises(ValueError, match='overwrite'):
        s.validate_outputs(base, frozen, tmp_path, frozen / 'ensemble.json')
    with patch.object(s, 'run_study') as run, contextlib.redirect_stderr(io.StringIO()):
        with pytest.raises(SystemExit):
            s.main(['--base-dir', str(base), '--snapshot-dir', str(frozen), '--out', str(tmp_path), '--no-ledger'])
        run.assert_not_called()


def test_cli_end_to_end_synthetic_reports_and_exactly_one_ledger_row(inputs, tmp_path):
    base, frozen, full = inputs
    before = {str(p): snapshots(p) for p in inputs}
    out, ledger = tmp_path / 'out', tmp_path / 'ledger.jsonl'
    with contextlib.redirect_stderr(io.StringIO()) as stderr, contextlib.redirect_stdout(io.StringIO()):
        assert s.main(['--base-dir', str(base), '--snapshot-dir', str(frozen), '--full-dir', str(full),
                       '--out', str(out), '--ledger', str(ledger), '--workers', '1', '--allow-input-drift']) == 0
    report = json.loads((out / 'report.json').read_text())
    assert (out / 'report.md').is_file()
    assert 'deviations' in (out / 'report.md').read_text()
    assert report['baseline_reproduction']['matched']
    assert report['baseline_reproduction']['max_relative_error'] == 0.
    assert report['primary']['n_days'] == 178
    assert report['primary']['verdict'] == 'invalid'  # only one independent accepted factor
    assert report['ledger']['written']
    assert len(evolution_ledger.read_rows(ledger)) == 1
    assert report['reacceptance']['counts']['accepted'] == 1
    assert report['reacceptance']['counts']['redundant'] == 1
    assert report['reacceptance']['counts']['invalid_expr'] == 1
    assert report['rank_ic']['B']['n_days'] == 174
    assert set(report['decomposition']) == {'U-B', 'H-B', 'UH-U', 'UH-H'}
    assert report['universe_composition']['mean_daily_overlap'] == 20.
    assert report['input_hashes']['validation.json'] == s.sha256(full / 'validation.json')
    for interval in report['intervals'].values():
        assert all(run['ok'] for run in interval['arms'].values())
        for arm in ('UH', 'H'):
            assert interval['arms'][arm]['hedge_summary']['h'] is not None
    assert 'S0:' in stderr.getvalue()
    assert before == {str(p): snapshots(p) for p in inputs}


def test_no_ledger_uses_next_k_readonly_in_temporary_reference(tmp_path):
    ledger = tmp_path / 'reference.jsonl'
    previous = dict(track='fq_crypto', stage='S1', verdict='fail')
    ledger.write_text(json.dumps(previous) + '\n')
    before = ledger.read_bytes()
    with patch.object(s, 'DEFAULT_LEDGER', ledger):
        row = s.ledger_result(statistics(), 5, True, True, s.EXPECTED_HASHES, 1., 1., None, {}, False)
    assert row['K'] == 2
    assert ledger.read_bytes() == before
