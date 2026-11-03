import hashlib
import io
import json
import math
import sqlite3
from concurrent.futures import ThreadPoolExecutor
import tempfile
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch

from scripts.backtest import replay_score as replay
from scripts.backtest.cli import main
from scripts.backtest.klines import cache_paths


def stats(z=3, n=100):
    return dict(n=n, days=10, mean=z / 10, se=.1, z=z, ci95=[0, 1])


def result():
    return dict(variant_id='v', variant_hash='hash', stats=stats(), noise=stats(0),
                parse_failure_rate=0, auxiliary=dict(open_rate_change=0), warnings=[])


def fixture(root, n=120, noise=True):
    records, rows = [], []
    for i in range(n):
        ts = (datetime(2026, 10, 1, tzinfo=timezone.utc) + timedelta(days=i // 10, minutes=i % 10)).isoformat()
        records.append(dict(record_id=i, timestamp=ts, weight=3 if i % 2 else 1, stratum='other'))
        for arm in (('baseline', 'variant', 'noise') if noise else ('baseline', 'variant')):
            opens = [dict(symbol='BTCUSDT', side='long', order_type='market', entry_price=0,
                          stop_loss=90, take_profit=120, confidence=80)] if i % 2 else []
            rows.append(dict(record_id=i, timestamp=ts, arm=arm, model='m', repeat=0,
                             variant_id='v' if arm == 'variant' else '',
                             variant_hash='hash' if arm == 'variant' else '',
                             status='ok', parse_failure=False, opens=opens))
    (root / 'sample.json').write_text(json.dumps(dict(records=records, seed=7, trader_id='t', requested=n)))
    (root / 'results.jsonl').write_text(''.join(json.dumps(r) + '\n' for r in rows))
    (root / 'variant-hash.json').write_text(json.dumps(dict(id='v', hypothesis='test')))
    return rows


def tiny_sim(row, *_):
    # No-opens have exactly zero R. Open trade performance varies by day.
    day = datetime.fromisoformat(row['timestamp']).day
    delta = .3 + .05 * (-1 if day % 2 else 1)
    r = delta if row['arm'] == 'variant' and row['opens'] else 0
    return dict(r=r, trade_rs=[r] if row['opens'] else [], excluded=False, counts={})


class ReplayStatisticsTest(unittest.TestCase):
    def test_replay_weighted_cluster_se(self):
        ps = [dict(day='a', diff=1, weight=1), dict(day='a', diff=3, weight=2),
              dict(day='b', diff=-1, weight=1), dict(day='b', diff=1, weight=2)]
        got = replay.weighted_cluster(ps)
        self.assertAlmostEqual(got['mean'], 4 / 3)
        self.assertAlmostEqual(got['se'], 1)
        self.assertAlmostEqual(got['ci95'][0], 4 / 3 - 1.959963984540054)
        doubled = [dict(p, weight=p['weight'] / 2) for p in ps for _ in range(2)]
        self.assertAlmostEqual(replay.weighted_cluster(doubled)['se'], 1)
        self.assertIsNone(replay.weighted_cluster(ps[:2])['se'])

    def test_replay_thresholds_and_zk_growth(self):
        judge = lambda k, **kw: replay.verdict(stats(2), stats(0), kw.get('failure', .1), k, kw.get('rate', .5))
        self.assertEqual(judge(1)['verdict'], 'pass')
        self.assertEqual(judge(2)['verdict'], 'fail')
        self.assertGreater(judge(100)['z_K'], judge(2)['z_K'])
        self.assertEqual(judge(1, failure=.10001)['verdict'], 'invalid')
        self.assertEqual(replay.verdict(stats(n=99), None, 0, 1, 0)['verdict'], 'invalid')
        self.assertEqual(replay.verdict(stats(), stats(2), 0, 1, 0)['verdict'], 'invalid')
        self.assertEqual(replay.verdict(stats(), stats(-2), 0, 1, 0)['verdict'], 'invalid')

    def test_replay_open_rate_guard(self):
        for change in (-.50001, .50001, -1, math.inf):
            got = replay.verdict(stats(100), None, 0, 1, change)
            self.assertEqual(got['verdict'], 'fail')
            self.assertTrue(got['warnings'])
        self.assertEqual(replay.verdict(stats(100), None, 0, 1, .7, .8)['verdict'], 'pass')

    def test_replay_zero_variance(self):
        ps = [dict(diff=1, weight=1, day='a'), dict(diff=1, weight=1, day='b')]
        self.assertEqual(replay.weighted_cluster(ps)['z'], math.inf)
        ps[0]['diff'] = ps[1]['diff'] = 0
        self.assertEqual(replay.weighted_cluster(ps)['z'], 0)


class ReplayScoringTest(unittest.TestCase):
    def test_replay_pairs_no_opens_and_optional_noise(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            fixture(root)
            with patch.object(replay.fetch, 'fetch_all') as network:
                r = replay.score(root, 'v', root, simulator=tiny_sim)
                network.assert_not_called()
            self.assertEqual(r['stats']['n'], 120)
            self.assertEqual(r['stats']['days'], 12)
            self.assertAlmostEqual(r['stats']['mean'], .225)
            self.assertEqual(r['noise']['mean'], 0)
            self.assertEqual(r['pairs'][0]['arm_r'], 0)
            self.assertAlmostEqual(r['auxiliary']['open_rates']['baseline'], .75)
            self.assertEqual(r['auxiliary']['open_rate_change'], 0)
            self.assertEqual(r['auxiliary']['noise_decision_agreement'], 1)
            self.assertEqual(replay.append_ledger(root / 'ledger', r)['verdict'], 'pass')
            r = replay.score(root, 'hash', root, arms=('baseline', 'variant'), simulator=tiny_sim)
            self.assertIsNone(r['noise'])
            self.assertTrue(r['warnings'])

    def test_replay_significant_noise_invalid(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            fixture(root)
            def sim(row, *args):
                row = dict(row, arm='variant' if row['arm'] == 'noise' else row['arm'])
                return tiny_sim(row, *args)
            r = replay.score(root, 'v', root, simulator=sim)
            self.assertGreater(r['noise']['z'], 2)
            self.assertEqual(replay.append_ledger(root / 'ledger', r)['verdict'], 'invalid')

    def test_replay_exclude_missing_klines_pair_and_parse_errors(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            rows = fixture(root)
            # More than 10% parse failures in variant must not be diluted by
            # successful baseline/noise rows.
            for row in rows:
                if row['arm'] == 'variant' and row['record_id'] < 13:
                    row.update(status='parse_error', parse_failure=True)
            (root / 'results.jsonl').write_text(''.join(json.dumps(r) + '\n' for r in rows))
            def sim(row, *args):
                r = tiny_sim(row, *args)
                if row['record_id'] == 15 and row['arm'] == 'variant':
                    r.update(excluded=True, counts=dict(missing_klines=1))
                return r
            r = replay.score(root, 'v', root, simulator=sim)
            self.assertEqual(r['stats']['n'], 106)
            self.assertEqual(r['counts']['missing_klines'], 1)
            self.assertGreater(r['parse_failure_rate'], .1)
            self.assertEqual(replay.append_ledger(root / 'ledger', r)['verdict'], 'invalid')

    def test_replay_model_repeat_and_hash_isolation(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            rows = fixture(root, 4)
            with (root / 'results.jsonl').open('a') as f:
                for r in rows:
                    f.write(json.dumps(dict(r, model='another', repeat=2)) + '\n')
            _, selected, _ = replay.load_replay(root, 'v')
            self.assertEqual(len(selected['variant']), 4)
            # Same id with another hash is ambiguous, never silently mix it.
            with (root / 'results.jsonl').open('a') as f:
                f.write(json.dumps(dict(rows[1], variant_hash='other')) + '\n')
            with self.assertRaises(ValueError):
                replay.load_replay(root, 'v')
            _, _, p = replay.load_replay(root, 'hash')
            self.assertEqual(p['hash'], 'hash')

    def test_replay_missing_sl_and_no_download(self):
        row = dict(timestamp='2026-10-01T00:00:00Z', opens=[dict(symbol='BTCUSDT', side='long',
                   order_type='market', stop_loss=0, take_profit=120)])
        with patch.object(replay, 'load_bars') as bars:
            r = replay.simulate_record(row, 'not-used')
            bars.assert_not_called()
        self.assertEqual(r['counts']['missing_sl'], 1)
        self.assertEqual(r['r'], 0)
        row['opens'][0]['stop_loss'] = 90
        with patch.object(replay.fetch, 'fetch_all') as network, patch.object(replay, 'load_bars', return_value=None):
            r = replay.simulate_record(row, 'not-used')
            self.assertTrue(r['excluded'])
            network.assert_not_called()

    def test_replay_tiny_real_backtrader(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            start = int(datetime(2026, 10, 1, tzinfo=timezone.utc).timestamp() * 1000)
            # Entry at 100 on bar 2; TP at 120 reached on bar 3.
            bars = [[start + i * 60000, 100, 101 if i < 2 else 121, 99, 100] for i in range(70)]
            p1, _ = cache_paths(root, 'BTCUSDT', start)
            Path(p1).parent.mkdir()
            Path(p1).write_text(json.dumps(bars))
            row = dict(timestamp='2026-10-01T00:00:00Z', opens=[dict(symbol='BTCUSDT', side='long',
                       order_type='market', entry_price=0, stop_loss=90, take_profit=120)])
            r = replay.simulate_record(row, root, horizon=.05, commission=0, slippage=0)
            self.assertFalse(r['excluded'])
            self.assertAlmostEqual(r['r'], 2)
            row['opens'][0].update(order_type='limit', limit_price=95)
            r = replay.simulate_record(row, root, horizon=.05, commission=0, slippage=0)
            self.assertEqual(r['r'], 0)
            self.assertEqual(r['counts']['unfilled'], 1)

    def test_replay_explicit_fetch_reuses_existing_fetch(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            fixture(root, 4)
            with patch.object(replay.fetch, 'fetch_all') as download:
                replay.score(root, 'v', root, do_fetch=True, simulator=tiny_sim)
            download.assert_called_once()
            self.assertEqual(len(download.call_args.args[0]), 6)

    def test_replay_failure_modes_use_original_nearest_open(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            path = root / 'db'
            with sqlite3.connect(path) as db:
                db.execute('CREATE TABLE trade_journal (trader_id, symbol, side, entry_time, failure_mode, ai_managed)')
                db.execute('CREATE TABLE decision_records (id, trader_id, timestamp, decisions, success)')
                ts = '2026-10-01T00:00:00Z'
                entry = int(datetime(2026, 10, 1, 0, 10, tzinfo=timezone.utc).timestamp() * 1000)
                db.execute('INSERT INTO trade_journal VALUES (?,?,?,?,?,?)', ('t', 'BTCUSDT', 'LONG', entry, 'chased_entry', 1))
                action = json.dumps([dict(symbol='BTCUSDT', action='open_long_limit', success=True)])
                db.execute('INSERT INTO decision_records VALUES (?,?,?,?,?)', (1, 't', ts, action, 1))
                db.commit()
                p = dict(record_id=1, timestamp=ts, day='2026-10-01', weight=1, diff=.3)
                got = replay.failure_mode_changes(dict(trader_id='t'), [p], {}, path)
                self.assertEqual(got['by_failure_mode']['chased_entry']['n'], 1)
                # An unsampled later original decision owns this journal.
                db.execute('INSERT INTO decision_records VALUES (?,?,?,?,?)', (2, 't', '2026-10-01T00:05:00Z', action, 1))
            got = replay.failure_mode_changes(dict(trader_id='t'), [p], {}, path)
            self.assertEqual(got['by_failure_mode'], {})

    def test_replay_cli_uses_scorer(self):
        with tempfile.TemporaryDirectory() as d:
            with patch.object(replay, 'score', return_value=result()) as scorer, redirect_stdout(io.StringIO()):
                # Supply summary fields when mocking only simulation/statistics.
                r = result()
                r.update(counts={}, auxiliary=dict(open_rate_change=0, open_rates={}), invalid_reasons=[])
                with patch.object(replay, 'append_ledger', return_value=dict(r, verdict='pass', K=1, z_K=1.96)):
                    self.assertEqual(main(['replay-score', '--replay-dir', d, '--variant', 'v']), 0)
                self.assertFalse(scorer.call_args.args[7])


class ReplayLedgerTest(unittest.TestCase):
    def test_replay_ledger_append_only_and_k(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'ledger.jsonl'
            first = replay.append_ledger(path, result())
            before = path.read_bytes()
            second = replay.append_ledger(path, result())
            self.assertTrue(path.read_bytes().startswith(before))
            self.assertEqual(len(path.read_text().splitlines()), 2)
            self.assertEqual((first['K'], second['K']), (1, 2))
            self.assertGreater(second['z_K'], first['z_K'])
            path.write_bytes(before + b'{"interrupted":')
            frozen = path.read_bytes()
            with self.assertRaises(ValueError):
                replay.append_ledger(path, result())
            self.assertEqual(path.read_bytes(), frozen)

    def test_replay_ledger_concurrent_appends(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'ledger'
            with ThreadPoolExecutor(max_workers=4) as pool:
                rows = list(pool.map(lambda _: replay.append_ledger(path, result()), range(8)))
            self.assertEqual(sorted(r['K'] for r in rows), list(range(1, 9)))
            stored = [json.loads(line) for line in path.read_text().splitlines()]
            self.assertEqual([r['K'] for r in stored], list(range(1, 9)))

    def test_replay_cache_crash_tail_recovery(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'cache'
            damaged = b'{"partial":'
            marker = json.dumps(dict(cache_tail_recovery=hashlib.sha256(damaged).hexdigest())).encode()
            row = json.dumps(dict(record_id=1)).encode()
            path.write_bytes(damaged + b'\n' + marker + b'\n' + row + b'\n')
            self.assertEqual(replay._cache_rows(path), [dict(record_id=1)])
            path.write_bytes(damaged + b'\n' + row + b'\n')
            with self.assertRaises(ValueError):
                replay._cache_rows(path)


if __name__ == '__main__':
    unittest.main()
