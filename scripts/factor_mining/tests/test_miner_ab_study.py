"""Preregistration and A/B plumbing tests, using temp synthetic data only."""
import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
from unittest.mock import patch

import pytest

from scripts.backtest import evolution_ledger
from scripts.factor_mining import miner_ab_study as study, options, store
from scripts.factor_mining.tests.synth import SIGNAL_SEED, write_market


def tiny_snapshot(path):
    (path / 'us').mkdir(parents=True)
    (path / 'us/zoo.json').write_text('{"factors":[]}')
    (path / 'us/ledger.jsonl').write_text('')
    return path


def test_snapshot_hash_matches_lc_all_c_content_order(tmp_path):
    snapshot = tiny_snapshot(tmp_path / 'snapshot')
    # Uppercase, lowercase, nested and non-ASCII paths exercise byte sorting.
    files = {'B-upper': b'UPPER', 'b-lower': b'lower', 'Z/sub': b'nested',
             'a-/x': b'dash', 'a_/x': b'underscore', '中': b'utf8'}
    for name, body in files.items():
        path = snapshot / 'us' / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(body)
    (snapshot / 'us/link').symlink_to(snapshot / 'us/B-upper')
    hashes = study.snapshot_hashes(snapshot)
    shell = subprocess.run(
        ['sh', '-c', 'find us -type f | LC_ALL=C sort | xargs cat | shasum -a 256'],
        cwd=snapshot, capture_output=True, text=True, check=True)
    assert hashes['aggregate'] == shell.stdout.split()[0]
    ordered = ['B-upper', 'Z/sub', 'a-/x', 'a_/x', 'b-lower', 'ledger.jsonl', 'zoo.json', '中']
    expected = b''.join((snapshot / 'us' / name).read_bytes() for name in ordered)
    assert hashes['aggregate'] == hashlib.sha256(expected).hexdigest()
    assert hashes['zoo.json'] == hashlib.sha256(b'{"factors":[]}').hexdigest()
    assert hashes['ledger.jsonl'] == hashlib.sha256(b'').hexdigest()


def test_hash_guard_override_and_copy_excludes_llm_work(tmp_path):
    snapshot = tiny_snapshot(tmp_path / 'snapshot')
    (snapshot / 'us/llm_work').mkdir()
    (snapshot / 'us/llm_work/secret').write_text('do not copy')
    work = tmp_path / 'work'
    with pytest.raises(ValueError, match='hash mismatch'):
        study.setup(snapshot, work)
    assert not work.exists()
    manifest = study.setup(snapshot, work, allow_input_drift=True)
    assert len(manifest['input_drift']) == 3
    for arm in ('B', 'D'):
        assert (work / arm / 'us/zoo.json').read_bytes() == (snapshot / 'us/zoo.json').read_bytes()
        assert not (work / arm / 'us/llm_work').exists()
    (work / 'B/us/zoo.json').write_text('changed')
    assert (work / 'D/us/zoo.json').read_text() == '{"factors":[]}'
    assert (snapshot / 'us/zoo.json').read_text() == '{"factors":[]}'


@pytest.mark.parametrize('name', ['aggregate', 'zoo.json', 'ledger.jsonl'])
def test_each_frozen_hash_is_required(tmp_path, name):
    snapshot = tiny_snapshot(tmp_path / 'snapshot')
    expected = study.snapshot_hashes(snapshot)
    expected[name] = '0' * 64
    with patch.object(study, 'EXPECTED_HASHES', expected):
        with pytest.raises(ValueError, match=name):
            study.setup(snapshot, tmp_path / 'work')


@pytest.mark.parametrize('arm', ['B', 'D'])
def test_setup_refuses_either_existing_arm_before_copying(tmp_path, arm):
    snapshot = tiny_snapshot(tmp_path / 'snapshot')
    work = tmp_path / 'work'
    (work / arm).mkdir(parents=True)
    marker = work / arm / 'preserve'
    marker.write_text('unchanged')
    with pytest.raises(FileExistsError):
        study.setup(snapshot, work, allow_input_drift=True)
    assert marker.read_text() == 'unchanged'
    assert not (work / ('D' if arm == 'B' else 'B')).exists()


def test_setup_accepts_matching_hashes(tmp_path):
    snapshot = tiny_snapshot(tmp_path / 'snapshot')
    with patch.object(study, 'EXPECTED_HASHES', study.snapshot_hashes(snapshot)):
        manifest = study.setup(snapshot, tmp_path / 'work')
    assert manifest['input_drift'] == {}


@pytest.mark.parametrize('b,d,expected', [(0, 1, 'screen_pass'), (1, 2, 'screen_pass'),
                                         (0, 0, 'fail'), (1, 1, 'fail'), (2, 1, 'fail')])
def test_verdict_screen_and_fail(b, d, expected):
    arms = {arm: dict(M1=m1, completed_nights=4, budget_exhausted=False, errors=[])
            for arm, m1 in [('B', b), ('D', d)]}
    assert study.verdict(arms) == expected


@pytest.mark.parametrize('arm', ['B', 'D'])
@pytest.mark.parametrize('field,value', [('completed_nights', 3), ('budget_exhausted', True),
                                        ('errors', ['failure'])])
def test_verdict_invalid_takes_precedence(arm, field, value):
    arms = {a: dict(M1=m1, completed_nights=4, budget_exhausted=False, errors=[])
            for a, m1 in [('B', 0), ('D', 10)]}
    arms[arm][field] = value
    assert study.verdict(arms) == 'invalid'


def test_new_filter_is_horizon_id_and_ledger_run_id_only():
    factors = [dict(horizon=h, id=fid, run_id='misleading-zoo-run', first_seen='2026-11-06')
               for h, fid in [(5, 'new'), (10, 'new'), (5, 'old'), (5, 'foreign')]]
    ledger = [dict(horizon=h, id=fid, run_id=rid) for h, fid, rid in [
        (5, 'new', 'ab'), (10, 'new', 'old'), (5, 'old', 'old'), (5, 'foreign', 'unrelated')]]
    assert study.new_factors(factors, ledger, {'ab'}) == factors[:1]
    runs = [dict(run_id=study.night_identity(0)[2]), dict(run_id='unrelated')]
    ids = study.ab_run_ids([dict(run_id='progress-only')], runs)
    assert ids == {study.night_identity(0)[2], 'progress-only'}


def report_work(tmp_path):
    snapshot = tmp_path / 'snapshot'
    write_market(snapshot, market='us', rows=240, columns=20)
    store.write_zoo('us', dict(factors=[]), snapshot)
    (snapshot / 'us/ledger.jsonl').write_text('')
    work = tmp_path / 'work'
    study.setup(snapshot, work, allow_input_drift=True)
    for arm in ('B', 'D'):
        root = work / arm
        new_run = study.night_identity(0)[2]
        factors = []
        for h, fid, status, rid, confirmed in [
                (5, 'old', 'accepted', 'old', True),
                (5, 'fresh', 'accepted', new_run, arm == 'D'),
                (10, 'fresh', 'accepted', new_run, False),
                (10, 'candidate', 'candidate', new_run, False),
                (5, 'redundant', 'redundant', new_run, False),
                (20, 'rejected', 'rejected', new_run, False),
                (5, 'foreign', 'accepted', 'unrelated', True)]:
            factors.append(dict(horizon=h, id=fid, expr='$volume', direction=1, status=status,
                                train=dict(t=3.), val=dict(ic=.02),
                                forward=dict(verdict='confirmed' if confirmed else 'failed')))
            store.append_ledger('us', [dict(horizon=h, id=fid, run_id=rid)], root)
        store.write_zoo('us', dict(factors=factors), root)
        for i in range(4):
            now, seed, run_id = study.night_identity(i)
            row = dict(run_id=run_id, seconds=10. + i, error=None, budget_exhausted=False,
                       started_at=now.isoformat(), market='us', seed=seed)
            if arm == 'D':
                row['diverse_parents'] = dict(per_horizon={'5': dict(
                    n_scanned=20, n_selected=4, n_rejected_corr=10, n_rejected_family=6, n_families=1)},
                    llm=None)
            store.append_run(row, root)
            store.append_jsonl(root / 'ab_progress.jsonl', [dict(night=i + 1, **row)])
    return work, snapshot


def test_report_metrics_diagnostics_and_temp_ledger(tmp_path):
    work, snapshot = report_work(tmp_path)
    ledger = tmp_path / 'ledger.jsonl'
    result = study.report(work, tmp_path / 'out', ledger=ledger, snapshot=snapshot)
    assert result['verdict'] == 'screen_pass'
    b, d = result['arms']['B'], result['arms']['D']
    assert (b['M1'], b['M2'], d['M1'], d['M2']) == (0, 2, 1, 2)
    assert d['diagnostics']['new_non_rejected'] == 4
    assert d['diagnostics']['clusters']['5'] == dict(n_top=2, n_clusters=1)
    assert d['diagnostics']['n_families'] == 1
    assert d['diagnostics']['median_val_ic'] == .02
    assert len(d['diagnostics']['per_night_seconds']) == 4
    assert all(r['stats'] is None for r in b['diagnostics']['parent_pool_stats'])
    assert d['diagnostics']['parent_pool_stats'][0]['stats']['per_horizon']['5']['n_selected'] == 4
    rows = evolution_ledger.read_rows(ledger)
    assert len(rows) == 1
    row = rows[0]
    assert (row['stage'], row['change_type'], row['track']) == ('S1', 'E', 'fq_us')
    assert row['baseline_version'] == 'miner@37898817c6af'
    assert row['idea_id'] == 'us-miner-diverse-parents-v1'
    assert row['K'] == 1 and row['counts_toward_k']
    assert row['arms'] == result['arms']
    assert row['snapshot_hashes'] == study.snapshot_hashes(snapshot)
    assert row['prereg'] == study.PREREG
    assert json.loads((tmp_path / 'out/report.json').read_text())['verdict'] == 'screen_pass'
    md = (tmp_path / 'out/report.md').read_text()
    assert '结论：**screen_pass**' in md and '| D | 4 | 1 | 2 |' in md
    assert study.main(['report', '--work', str(work), '--out', str(tmp_path / 'no-ledger'),
                       '--no-ledger']) == 0
    assert len(evolution_ledger.read_rows(ledger)) == 1


def test_report_invalid_ledger_does_not_count_toward_k(tmp_path):
    work, _ = report_work(tmp_path)
    store.append_jsonl(work / 'B/ab_progress.jsonl', [dict(
        night=4, run_id=study.night_identity(3)[2], seconds=1, budget_exhausted=True, error=None)])
    result = study.report(work, tmp_path / 'out', ledger=tmp_path / 'ledger.jsonl')
    assert result['verdict'] == 'invalid'
    assert result['ledger_row']['K'] is None and not result['ledger_row']['counts_toward_k']


@pytest.mark.parametrize('arm', ['B', 'D'])
def test_run_fixed_calls_clean_environment_resume_and_restore(tmp_path, arm):
    (tmp_path / arm / 'us').mkdir(parents=True)
    names = {v for v in vars(options).values() if isinstance(v, str) and v.startswith('FM_')}
    calls = []

    def fake(self, market, **kwargs):
        calls.append((market, copy.deepcopy(kwargs)))
        assert os.environ.get(options.DIVERSE_PARENTS) == ('1' if arm == 'D' else None)
        assert all(name == options.DIVERSE_PARENTS or name not in os.environ for name in names)
        now, seed, run_id = study.night_identity(len(calls) - 1)
        assert kwargs == dict(n_mutations=1500, n_llm=0, budget_s=10800,
                              seed=seed, use_llm=False, now=now)
        store.append_run(dict(run_id=run_id, seconds=.1, error=None, budget_exhausted=False), self.root)

    with patch.dict(os.environ, {name: 'inherited' for name in names}), \
         patch.object(study.Miner, 'run_market', fake):
        progress = study.run(tmp_path, arm)
        assert all(os.environ[name] == 'inherited' for name in names)
        assert study.run(tmp_path, arm) == progress
    assert len(calls) == len(progress) == 4
    assert [r['seed'] for r in progress] == list(range(20261106, 20261110))
    assert all(not r['error'] and not r['budget_exhausted'] for r in progress)


def test_run_recovers_progress_after_completed_run(tmp_path):
    (tmp_path / 'B/us').mkdir(parents=True)
    run_id = study.night_identity(0)[2]
    store.append_run(dict(run_id=run_id, seconds=7., error=None, budget_exhausted=False), tmp_path / 'B')
    with patch.object(study.Miner, 'run_market', side_effect=AssertionError('night reran')):
        rows = study.run(tmp_path, 'B', nights=1, mutations=2, budget_min=1)
    assert rows[0]['seconds'] == 7.
    assert rows[0]['run_id'] == run_id


def test_partial_trials_without_final_run_cannot_silently_repeat(tmp_path):
    (tmp_path / 'B/us').mkdir(parents=True)
    store.append_ledger('us', [dict(run_id=study.night_identity(0)[2], id='partial', horizon=5)],
                        tmp_path / 'B')
    with patch.object(study.Miner, 'run_market', side_effect=AssertionError('night reran')):
        with pytest.raises(RuntimeError, match='interrupted night'):
            study.run(tmp_path, 'B')
    progress = store.read_jsonl(tmp_path / 'B/ab_progress.jsonl')
    assert len(progress) == 1 and 'interrupted night' in progress[0]['error']


@pytest.mark.parametrize('error,budget', [(None, True), ('RuntimeError: boom', False)])
def test_run_persists_failures_and_refuses_unsafe_retry(tmp_path, error, budget):
    (tmp_path / 'B/us').mkdir(parents=True)

    def fake(self, market, **kwargs):
        assert not os.environ.get(options.DIVERSE_PARENTS)
        store.append_run(dict(run_id=study.night_identity(0)[2], seconds=.1, error=error,
                              budget_exhausted=budget), self.root)
        if error:
            raise RuntimeError('boom')

    with patch.object(study.Miner, 'run_market', fake):
        if error:
            with pytest.raises(RuntimeError, match='boom'):
                study.run(tmp_path, 'B')
        else:
            study.run(tmp_path, 'B')
    rows = store.read_jsonl(tmp_path / 'B/ab_progress.jsonl')
    assert len(rows) == 1 and rows[0]['error'] == error and rows[0]['budget_exhausted'] == budget
    with pytest.raises(ValueError, match='previously failed'):
        study.run(tmp_path, 'B')


def test_tiny_synthetic_both_arms_cli_end_to_end(tmp_path):
    snapshot = tmp_path / 'snapshot'
    write_market(snapshot, market='us', rows=240, columns=20)
    store.write_zoo('us', dict(factors=[]), snapshot)
    (snapshot / 'us/ledger.jsonl').write_text('')
    before = study.snapshot_hashes(snapshot)
    work = tmp_path / 'work'
    assert study.main(['setup', '--snapshot', str(snapshot), '--work', str(work),
                       '--allow-input-drift']) == 0
    with patch('scripts.factor_mining.miner.zoo.seed_factors', return_value=[SIGNAL_SEED]):
        for arm in ('B', 'D'):
            args = ['run', '--work', str(work), '--arm', arm, '--nights', '1',
                    '--mutations', '2', '--budget-min', '2']
            assert study.main(args) == 0
            before_resume = (work / arm / 'us/ledger.jsonl').read_bytes()
            assert study.main(args) == 0
            assert (work / arm / 'us/ledger.jsonl').read_bytes() == before_resume
            progress = store.read_jsonl(work / arm / 'ab_progress.jsonl')
            assert len(progress) == 1 and study.completed(progress[0])
            row = store.read_jsonl(work / arm / 'runs.jsonl')[0]
            assert row['mutation_evaluated'] > 0
            assert ('diverse_parents' in row) == (arm == 'D')
            assert row['started_at'] == '2026-11-06T19:00:00+00:00'
    ledger = tmp_path / 'evolution.jsonl'
    assert study.main(['report', '--work', str(work), '--out', str(tmp_path / 'out'),
                       '--ledger', str(ledger)]) == 0
    result = json.loads((tmp_path / 'out/report.json').read_text())
    assert result['verdict'] == 'invalid'
    assert all(a['completed_nights'] == 1 for a in result['arms'].values())
    assert len(evolution_ledger.read_rows(ledger)) == 1
    assert before == study.snapshot_hashes(snapshot)
