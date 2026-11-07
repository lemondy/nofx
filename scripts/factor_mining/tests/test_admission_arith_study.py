"""Synthetic-only regression checks for the frozen arithmetic admission study."""
import copy
import json
import math

import numpy as np
import pandas as pd
import pytest

from scripts.factor_mining import admission_arith_study as s
from scripts.factor_mining.miner import Miner
from scripts.factor_mining.tests.synth import planted


def fixture(rows=61, names=23):
    p = planted(rows=rows, columns=names)
    rng = np.random.default_rng(19)
    values = pd.DataFrame(rng.integers(0, 7, p.mask.shape).astype(float),
                          index=p.mask.index, columns=p.mask.columns)
    raw = pd.DataFrame(rng.normal(.001, .03, p.mask.shape), index=values.index, columns=values.columns)
    raw.iloc[-8:] = np.nan
    values.iloc[0] = np.nan
    values.iloc[4, :5] = np.nan
    p.mask.iloc[7, :11] = False
    segment = pd.Series(False, index=values.index)
    segment.iloc[:len(values) - 8] = True
    return p, values, raw, segment


@pytest.mark.parametrize('h', [1, 3, 7])
def test_zero_funding_exact_selection_turnover_ties_and_skipped_dates(h):
    p, values, raw, segment = fixture()
    funding = values.fillna(0.) * 0.
    result = s.funding_long_short(values, raw, p.mask, segment, h, s.config.MARKETS['crypto'], funding)
    expected, turnover = s.evaluate.long_short(values, raw, p.mask, segment, h, s.config.MARKETS['crypto'])
    pd.testing.assert_series_equal(result['base'], expected, check_exact=True)
    pd.testing.assert_series_equal(result['net'], expected, check_exact=True)
    np.testing.assert_array_equal(result['turnover'], turnover)
    assert result['coverage']['fraction'] == 1.


def test_funding_sign_window_and_missing_zero():
    p = planted(rows=12, columns=16)
    values = pd.DataFrame(np.tile(np.arange(16), (12, 1)), index=p.mask.index, columns=p.mask.columns)
    raw = values * 0.
    segment = pd.Series(False, index=p.mask.index)
    segment.iloc[0] = True
    rates = raw.copy()
    # 16 names split as 4/3/3/3/3: longs are 13,14,15 and shorts are 0,1,2,3.
    rates.iloc[1:4, 13:] = [.01, .02, .03]
    rates.iloc[1:4, :4] = .004
    rates.iloc[4] = 1000.
    rates.iloc[2, 13] = np.nan
    run = s.funding_long_short(values, raw, p.mask, segment, 3, s.config.MARKETS['crypto'], rates)
    long_paid = ((2 * .01) + (3 * .02) + (3 * .03)) / 3 * 3
    short_received = 3 * .004 * 3
    assert run['net'].iloc[0] - run['base'].iloc[0] == pytest.approx(-long_paid + short_received)
    assert run['coverage'] == dict(covered=20, selected=21, fraction=20 / 21)
    changed = rates.copy()
    changed.iloc[4] = -1000.
    other = s.funding_long_short(values, raw, p.mask, segment, 3, s.config.MARKETS['crypto'], changed)
    pd.testing.assert_series_equal(run['net'], other['net'], check_exact=True)
    assert run['legs'].iloc[0].long_funding < 0 < run['legs'].iloc[0].short_funding


def test_ls_t_sample_standard_deviation_and_undefined_cases():
    x = np.array([.01, .03, -.02, .04, -.01])
    assert s.ls_t(x) == pytest.approx(x.mean() / (x.std(ddof=1) / math.sqrt(5)))
    assert math.isnan(s.ls_t([1.]))
    assert math.isnan(s.ls_t([0., 0.]))
    assert math.isnan(s.ls_t([1., np.nan]))
    assert s.ls_t([1., 1.]) == math.inf
    assert s.ls_t([-1., -1.]) == -math.inf


def factor(id_='a', t=6., first_seen='2020-01-01', run_id='r', status='accepted'):
    train = dict(ic=.1, t=t, ic_halves=[.1, .1], ls_ret=.1)
    val = dict(ic=.1, t=2., ls_ret=.1)
    return dict(id=id_, expr='$close', horizon=3, direction=1, train=train, val=val,
                first_seen=first_seen, run_id=run_id, status=status, family='other')


def metrics(train_t=2., val_t=2., train_f=2., val_f=2., train_mean=.01, val_mean=.01):
    return dict(train=dict(unfunded=dict(ls_t=train_t), funded=dict(ls_t=train_f, mean=train_mean)),
                val=dict(unfunded=dict(ls_t=val_t), funded=dict(ls_t=val_f, mean=val_mean)))


@pytest.mark.parametrize('gate,kwargs,expected', [
    ('A', {}, 'accepted'), ('A', dict(train_f=1.65, val_f=1.65), 'accepted'),
    ('A', dict(train_f=1.649), 'candidate'), ('A', dict(val_f=1.), 'candidate'),
    ('A0', dict(train_f=-4., val_f=-4.), 'accepted'),
    ('A0', dict(train_t=1.64), 'candidate'), ('A0', dict(val_t=1.64), 'candidate'),
    ('S_f', dict(train_f=.1, val_f=.1), 'accepted'),
    ('S_f', dict(train_mean=0.), 'candidate'), ('S_f', dict(val_mean=-.01), 'candidate'),
])
def test_gate_truth_table(gate, kwargs, expected):
    f = factor()
    # The old LS signs are replaced, including negative old means.
    f['train']['ls_ret'] = f['val']['ls_ret'] = -.2
    assert s.gate_status(f, 1, 2., gate, metrics(**kwargs)) == expected


@pytest.mark.parametrize('section,key,value,expected', [
    ('train', 't', 1.9, 'rejected'), ('train', 'ic', 0., 'rejected'),
    ('train', 'ic_halves', [.1, -.1], 'rejected'),
    ('val', 'ic', 0., 'candidate'), ('val', 't', 1.64, 'candidate'),
])
@pytest.mark.parametrize('gate', s.GATES)
def test_unchanged_ic_gate(section, key, value, expected, gate):
    f = factor()
    f[section][key] = value
    assert s.gate_status(f, 1, 2., gate, metrics()) == expected


def toy_zoo():
    p, values, _, _ = fixture(rows=80, names=20)
    factors = [factor('later', first_seen='2020-01-02'), factor('candidate', t=7.),
               factor('first', run_id='a'), factor('second', run_id='b'), factor('negative', t=5.)]
    factors[1]['val']['t'] = 1.
    cache = {f['id']: values.copy() for f in factors}
    cache['negative'] = -values
    splits = {3: s.panel.make_splits(p.mask.index, 3, p.windows)}
    cfg = dict(s.config.MARKETS['crypto'], horizons=(3,))
    return p, factors, cache, splits, cfg


def test_redundancy_ordering_matches_miner_and_candidate_never_enters_kept():
    p, factors, cache, splits, cfg = toy_zoo()
    expected = {(f['horizon'], f['id']): copy.deepcopy(f) for f in factors}
    Miner._classify_and_correlate(expected, {3: 1}, p, splits, cfg, cache)
    actual = s.reclassify(factors, {3: 1}, p, splits, cfg, cache)
    for key in actual:
        for name in ('status', 'corr_with', 'corr_max'):
            assert actual[key][name] == expected[key][name]
    assert actual[3, 'candidate']['status'] == 'candidate'
    assert actual[3, 'first']['status'] == 'accepted'
    assert actual[3, 'second']['status'] == actual[3, 'later']['status'] == 'redundant'
    assert actual[3, 'negative']['status'] == 'accepted'


def test_s0_reproduction_and_detecting_status_and_ls_failures():
    p = planted(rows=180, columns=20)
    cfg = dict(s.config.MARKETS['crypto'], horizons=(3,))
    splits = {3: s.panel.make_splits(p.mask.index, 3, p.windows)}
    values = s.expr.evaluate_expr('$close/$open-1', p.fields)
    excess, raw = s.panel.forward_target(p, 3)
    f = factor()
    f.update(s.evaluate.evaluate_factor(values, excess, raw, p.mask, splits[3], 3, cfg))
    cache = {'a': values}
    classified = s.reclassify([f], {3: 1}, p, splits, cfg, cache)
    f['status'] = classified[3, 'a']['status']
    assert f['status'] == 'accepted'
    result = s.reproduce_s0([f], {3: 1}, p, splits, cfg, cache)
    assert result['passed'] and result['max_ls_error'] <= 1e-9
    assert result['accepted_checked'] == 1 and result['ls_comparisons'] == 2
    broken = copy.deepcopy(f)
    broken['train']['ls_ret'] += 1e-6
    assert not s.reproduce_s0([broken], {3: 1}, p, splits, cfg, cache)['passed']
    broken['status'] = 'candidate'
    result = s.reproduce_s0([broken], {3: 1}, p, splits, cfg, cache)
    assert not result['passed'] and len(result['status_mismatches']) == 1


@pytest.mark.parametrize('n,bad,z,halves,empty,same,expected', [
    (119, False, 9., [1., 1.], False, False, 'invalid'),
    (120, True, 9., [1., 1.], False, False, 'invalid'),
    (120, False, s.Z_K, [-1., -1.], False, False, 'strong'),
    (120, False, 1., [1., 1.], False, False, 'screen_pass'),
    (120, False, 1., [0., 1.], False, False, 'fail'),
    (120, False, .99, [1., 1.], False, False, 'fail'),
    (120, False, math.nan, [1., 1.], False, False, 'fail'),
    (0, True, math.nan, [], True, False, 'no_factors'),
    (0, True, math.nan, [], False, True, 'unchanged'),
])
def test_verdict_truth_table(n, bad, z, halves, empty, same, expected):
    assert s.verdict(n, bad, z, halves, empty=empty, unchanged=same) == expected


def history(count=6):
    rows = [dict(track='fq_crypto', stage='S1', verdict='fail', idea_id=f'history-{i}') for i in range(count)]
    return rows + [dict(track='fq_crypto', stage='S1', verdict='invalid'),
                   dict(track='fq_us', stage='S1', verdict='fail'),
                   dict(track='fq_crypto', stage='S2', verdict='fail')]


def ledger_result(verdict='fail'):
    return dict(verdict=verdict, paired=dict(forward=dict(z=-1.)), hashes={}, deviations=[])


def test_ledger_guard_requires_k7_and_no_duplicate_or_special_rows(tmp_path):
    path = tmp_path / 'ledger.jsonl'
    for verdict in ('no_factors', 'unchanged'):
        assert s.append_ledger(path, ledger_result(verdict)) is None
        assert not path.exists()
    for n in (0, 5, 7):
        path.write_text(''.join(json.dumps(r) + '\n' for r in history(n)))
        before = path.read_bytes()
        with pytest.raises(ValueError, match='exactly 6'):
            s.append_ledger(path, ledger_result())
        assert path.read_bytes() == before
    path.write_text(''.join(json.dumps(r) + '\n' for r in history()))
    event = s.append_ledger(path, ledger_result())
    assert event['K'] == 7 and event['counts_toward_k']
    assert event['idea_id'] == s.IDEA
    before = path.read_bytes()
    with pytest.raises(ValueError):
        s.append_ledger(path, ledger_result())
    assert path.read_bytes() == before


def test_invalid_ledger_row_checks_k_but_is_not_counted(tmp_path):
    path = tmp_path / 'ledger.jsonl'
    path.write_text(''.join(json.dumps(r) + '\n' for r in history()))
    event = s.append_ledger(path, ledger_result('invalid'))
    assert event['K'] is None and not event['counts_toward_k']
    assert s.next_k(s.read_rows(path), 'fq_crypto') == 7


def test_out_path_refusal_collisions_readonly_and_symlinks(tmp_path):
    root = tmp_path / 'input'
    root.mkdir()
    out = tmp_path / 'out'
    for invalid in (root, root / 'child'):
        with pytest.raises(ValueError):
            s.validate_outputs(root, invalid)
    alias = tmp_path / 'alias'
    alias.symlink_to(root, target_is_directory=True)
    with pytest.raises(ValueError):
        s.validate_outputs(root, alias / 'child')
    for name in ('report.md', 'report.json', 'factors.csv', 'attempts.jsonl', 'run.log', 'cache/anything.npy'):
        with pytest.raises(ValueError):
            s.validate_outputs(root, out, out / name)
    with pytest.raises(ValueError):
        s.validate_outputs(root, out, root / 'ledger.jsonl')
    # The main agent can explicitly choose its real ledger on an authorized rerun.
    assert s.validate_outputs(root, out, s.HISTORY_LEDGER) == out.resolve()
    assert s.validate_outputs(root, out, out / 'ledger.jsonl') == out.resolve()


def test_snapshot_k_cutoff_and_metadata_check(tmp_path):
    path = tmp_path / 'miner.jsonl'
    rows = [dict(ts='2020-01-01T00:00:00Z', horizon=h) for h in s.HORIZONS]
    rows += [dict(ts='2020-01-03T00:00:00Z', horizon=3)]
    path.write_text(''.join(json.dumps(r) + '\n' for r in rows))
    zoo = dict(horizons={str(h): dict(trials=1, t_req=s.evaluate.t_required(1)) for h in s.HORIZONS})
    trials, total = s.snapshot_trials(path, '2020-01-02T00:00:00Z', zoo)
    assert total == 3 and all(t['K'] == 1 for t in trials.values())
    zoo['horizons']['3']['trials'] = 2
    with pytest.raises(ValueError):
        s.snapshot_trials(path, '2020-01-02T00:00:00Z', zoo)
