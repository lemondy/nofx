import hashlib
import io
import json
import math
import sqlite3
from concurrent.futures import ThreadPoolExecutor
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch

from scripts.backtest import evolution_ledger, replay_score as replay
from scripts.backtest.cli import main
from scripts.backtest.klines import cache_paths


def stats(z=3, n=100):
    return dict(n=n, days=10, mean=z / 10 if z is not None else .3,
                se=.1 if z is not None else None, z=z, ci95=[0, 1])


def halves():
    return dict(dev=stats(2), holdout=stats(4))


def result():
    return dict(variant_id='v', variant_hash='hash', stats=stats(), noise=stats(0),
                halves=halves(), patch_hit_rate=1, parse_failure_rate=0,
                auxiliary=dict(open_rate_change=0, open_rates=dict(baseline=.5, variant=.5)),
                counts={}, warnings=[])


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
        judge = lambda k, **kw: replay.screen_verdict(
            stats(2), halves(), stats(0), kw.get('failure', .1), .8, k, kw.get('rate', .5))
        self.assertEqual(judge(1)['verdict'], 'strong')
        self.assertEqual(judge(2)['verdict'], 'screen_pass')
        self.assertGreater(judge(100)['z_K'], judge(2)['z_K'])
        self.assertEqual(judge(1, failure=.10001)['verdict'], 'invalid')
        for main, noise, hit, reason in (
                (stats(n=99), None, 1, '有效配对点 < 100'),
                (stats(), stats(2), 1, '噪声组 |z| ≥ 2'),
                (stats(), stats(-2), 1, '噪声组 |z| ≥ 2'),
                (stats(), stats(0), .79999, '补丁命中率 < 80%')):
            with self.subTest(reason=reason, noise=noise):
                got = replay.screen_verdict(main, halves(), noise, 0, hit, 1, 1)
                self.assertEqual(got['verdict'], 'invalid')
                self.assertEqual(got['invalid_reasons'], [reason])

    def test_replay_screen_pass_fail_and_missing_evidence(self):
        for z, expected in ((1, 'screen_pass'), (.99999, 'fail'), (0, 'fail'),
                            (-3, 'fail'), (None, 'fail')):
            with self.subTest(z=z):
                got = replay.screen_verdict(stats(z), halves(), None, 0, 1, 1, 0)
                self.assertEqual(got['verdict'], expected)
                self.assertIn('未提供噪声组', got['warnings'][0])
                if z is None:
                    self.assertTrue(any('无法估计 SE' in w for w in got['warnings']))
        for half in ('dev', 'holdout'):
            for mean in (None, 0, -.1):
                with self.subTest(half=half, mean=mean):
                    split = halves()
                    split[half]['mean'] = mean
                    got = replay.screen_verdict(stats(100), split, stats(0), 0, 1, 1, 0)
                    self.assertEqual(got['verdict'], 'fail')
        got = replay.screen_verdict(stats(), halves(), stats(None), 0, 1, 1, 0)
        self.assertTrue(any('无法估计 SE' in w for w in got['warnings']))

    def test_replay_open_rate_guard(self):
        for change in (-.50001, .50001, -1, math.inf):
            got = replay.screen_verdict(stats(100), halves(), None, 0, 1, 1, change)
            self.assertEqual(got['verdict'], 'fewer_trades')
            self.assertFalse(got['open_rate_guard'])
            self.assertTrue(got['warnings'])
        self.assertEqual(replay.screen_verdict(stats(100), halves(), None, 0, 1, 1, .7, .8)['verdict'], 'strong')
        self.assertEqual(replay.screen_verdict(stats(None), halves(), None, 0, 1, 1, -1)['verdict'], 'fewer_trades')

    def test_replay_split_halves_by_distinct_days(self):
        ps = [dict(day=f'2026-10-{day:02}', diff=diff, weight=weight)
              for day, diff, weight in ((5, 10, 1), (1, 1, 1), (4, 7, 1),
                                        (2, 2, 2), (3, 5, 1), (1, 3, 3))]
        got = replay.split_halves(ps)
        self.assertEqual((got['dev']['n'], got['dev']['days']), (4, 3))
        self.assertAlmostEqual(got['dev']['mean'], 19 / 7)
        self.assertIsNotNone(got['dev']['se'])
        self.assertEqual((got['holdout']['n'], got['holdout']['days']), (2, 2))
        self.assertAlmostEqual(got['holdout']['mean'], 8.5)
        self.assertAlmostEqual(got['holdout']['se'], 1.5)

    def test_replay_split_halves_small_samples(self):
        ps = [dict(day='2026-10-01', diff=1, weight=1),
              dict(day='2026-10-02', diff=2, weight=1)]
        got = replay.split_halves(ps)
        for half, mean in (('dev', 1), ('holdout', 2)):
            self.assertEqual(got[half]['mean'], mean)
            self.assertIsNone(got[half]['se'])
        self.assertEqual(replay.screen_verdict(stats(), got, stats(0), 0, 1, 1, 0)['verdict'], 'strong')
        got = replay.split_halves(ps[:1])
        self.assertEqual(got['dev']['mean'], 1)
        self.assertIsNone(got['holdout']['mean'])
        self.assertIsNone(got['holdout']['se'])
        self.assertEqual(replay.split_halves([]), dict(dev=replay.weighted_cluster([]),
                                                      holdout=replay.weighted_cluster([])))

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
            self.assertEqual(r['halves']['dev']['days'], 6)
            self.assertEqual(r['halves']['holdout']['days'], 6)
            self.assertAlmostEqual(r['halves']['dev']['mean'], .225)
            self.assertAlmostEqual(r['halves']['holdout']['mean'], .225)
            self.assertEqual(r['patch_hit_rate'], 1)
            self.assertEqual(r['noise']['mean'], 0)
            self.assertEqual(r['pairs'][0]['arm_r'], 0)
            self.assertAlmostEqual(r['auxiliary']['open_rates']['baseline'], .75)
            self.assertEqual(r['auxiliary']['open_rate_change'], 0)
            self.assertEqual(r['auxiliary']['noise_decision_agreement'], 1)
            self.assertEqual(replay.append_ledger(root / 'ledger', r, 'crypto_llm', 'h0')['verdict'], 'strong')
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
            self.assertEqual(replay.append_ledger(root / 'ledger', r, 'crypto_llm', 'h0')['verdict'], 'invalid')

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
            self.assertEqual(replay.append_ledger(root / 'ledger', r, 'crypto_llm', 'h0')['verdict'], 'invalid')

    def test_replay_patch_hit_rate_before_pair_exclusion(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            rows = fixture(root, n=150)
            for row in rows:
                if row['arm'] == 'variant' and row['record_id'] < 31:
                    row.update(status='patch_miss', opens=[])
            # Missing calls do not shrink the manifest denominator; other
            # models/repeats/hashes must not contribute patch misses.
            rows = [r for r in rows if not (r['arm'] == 'variant' and r['record_id'] == 149)]
            rows += [dict(rows[1], model='other'), dict(rows[1], repeat=1),
                     dict(rows[1], variant_id='other', variant_hash='other')]
            (root / 'results.jsonl').write_text(''.join(json.dumps(r) + '\n' for r in rows))
            _, selected, _ = replay.load_replay(root, 'hash', model='m')
            self.assertEqual(sum(r['status'] == 'patch_miss' for r in selected['variant'].values()), 31)
            r = replay.score(root, 'hash', root, model='m', simulator=tiny_sim)
            self.assertEqual(r['stats']['n'], 118)
            self.assertEqual(r['counts']['patch_miss'], 31)
            self.assertEqual(r['parse_failure_rate'], 0)
            self.assertAlmostEqual(r['patch_hit_rate'], 1 - 31 / 150)
            row = replay.append_ledger(root / 'ledger', r, 'crypto_llm', 'h0')
            self.assertEqual(row['verdict'], 'invalid')
            self.assertEqual(row['invalid_reasons'], ['补丁命中率 < 80%'])

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
            path = Path(d) / 'ledger.jsonl'
            for extra, change_type, idea_id in (([], 'D', 'v'),
                    (['--change-type', 'A', '--idea-id', 'factor-input'], 'A', 'factor-input')):
                out = io.StringIO()
                with self.subTest(change_type=change_type), redirect_stdout(out), \
                        patch.object(replay, 'score', return_value=result()) as scorer:
                    self.assertEqual(main(['replay-score', '--replay-dir', d, '--variant', 'v',
                                           '--track', 'us_llm', '--baseline-version', 'config-hash',
                                           '--ledger', str(path)] + extra), 0)
                self.assertFalse(scorer.call_args.args[7])
                row = evolution_ledger.read_rows(path)[-1]
                self.assertEqual((row['track'], row['baseline_version'], row['change_type'], row['idea_id']),
                                 ('us_llm', 'config-hash', change_type, idea_id))
                for text in ('重放结论：strong', 'dev', 'holdout', '补丁命中率 100.0%', 'K=', 'z_K='):
                    self.assertIn(text, out.getvalue())
                self.assertEqual(out.getvalue().splitlines()[-1],
                                 'screen_pass/strong 仅进入线上配对队列，不直接上线；fewer_trades 归入基线交易频率问题')

    def test_replay_cli_requires_track_and_baseline_version(self):
        base = ['replay-score', '--replay-dir', 'unused', '--variant', 'v']
        for extra, missing in ((['--baseline-version', 'h0'], '--track'),
                               (['--track', 'crypto_llm'], '--baseline-version')):
            err = io.StringIO()
            with self.subTest(missing=missing), redirect_stderr(err), \
                    patch.object(replay, 'score') as scorer, self.assertRaises(SystemExit) as exc:
                main(base + extra)
            self.assertEqual(exc.exception.code, 2)
            self.assertIn(missing, err.getvalue())
            scorer.assert_not_called()

    def test_replay_cli_rejects_invalid_track_or_change_type(self):
        base = ['replay-score', '--replay-dir', 'unused', '--variant', 'v', '--baseline-version', 'h0']
        for extra in (['--track', 'other'], ['--track', 'crypto_llm', '--change-type', 'B']):
            with self.subTest(extra=extra), redirect_stderr(io.StringIO()), \
                    self.assertRaises(SystemExit) as exc:
                main(base + extra)
            self.assertEqual(exc.exception.code, 2)


class ReplayLedgerTest(unittest.TestCase):
    def test_replay_ledger_append_only_and_k(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'ledger.jsonl'
            first = replay.append_ledger(path, result(), 'crypto_llm', 'h0')
            before = path.read_bytes()
            second = replay.append_ledger(path, result(), 'crypto_llm', 'h0')
            self.assertTrue(path.read_bytes().startswith(before))
            self.assertEqual(len(path.read_text().splitlines()), 2)
            self.assertEqual((first['K'], second['K']), (1, 2))
            self.assertGreater(second['z_K'], first['z_K'])
            path.write_bytes(before + b'{"interrupted":')
            frozen = path.read_bytes()
            with self.assertRaises(ValueError):
                replay.append_ledger(path, result(), 'crypto_llm', 'h0')
            self.assertEqual(path.read_bytes(), frozen)

    def test_replay_ledger_k_and_zk_are_per_track_non_invalid_s1(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'ledger.jsonl'
            r = result()
            r['stats'] = stats(2)
            first = replay.append_ledger(path, r, 'crypto_llm', 'h0')
            other = replay.append_ledger(path, r, 'us_llm', 'h0')
            self.assertEqual((first['K'], other['K']), (1, 1))
            self.assertEqual((first['verdict'], other['verdict']), ('strong', 'strong'))
            self.assertEqual(first['z_K'], other['z_K'])
            evolution_ledger.append_event(path, 'crypto_llm', lambda k, _: dict(
                baseline_version='h0', change_type='D', stage='S2', idea_id='v', verdict='promote'))
            invalid = replay.append_ledger(path, dict(r, patch_hit_rate=.79), 'crypto_llm', 'h0')
            second = replay.append_ledger(path, r, 'crypto_llm', 'h1')
            self.assertEqual(invalid['verdict'], 'invalid')
            self.assertIsNone(invalid['K'])
            self.assertFalse(invalid['counts_toward_k'])
            self.assertEqual(second['K'], 2)
            self.assertEqual(second['verdict'], 'screen_pass')
            self.assertEqual(invalid['z_K'], second['z_K'])
            self.assertGreater(second['z_K'], first['z_K'])

    def test_replay_ledger_shared_fields_and_preserved_summary(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            fixture(root)
            r = replay.score(root, 'v', root, simulator=tiny_sim)
            row = replay.append_ledger(root / 'ledger', r, 'crypto_llm', 'config-hash',
                                       change_type='A', idea_id='factor-input')
            self.assertEqual((row['stage'], row['track'], row['baseline_version'], row['change_type'], row['idea_id']),
                             ('S1', 'crypto_llm', 'config-hash', 'A', 'factor-input'))
            self.assertEqual(row['verdict'], 'strong')
            self.assertTrue(row['counts_toward_k'])
            for field, expected in dict(delta=r['stats']['mean'], se=r['stats']['se'], z=r['stats']['z'],
                    trade_ratio=1, placebo_pct=None, dev_delta=r['halves']['dev']['mean'],
                    holdout_delta=r['halves']['holdout']['mean'], patch_hit_rate=1).items():
                self.assertEqual(row[field], expected)
            self.assertGreater(row['z_K'], 1.9)
            self.assertIn('scored_at', row)
            self.assertIn('recorded_at', row)
            stored = evolution_ledger.read_rows(root / 'ledger')[0]
            for key in r:
                if key not in ('pairs', 'noise_pairs'):
                    self.assertEqual(row[key], r[key])
                    self.assertEqual(stored[key], r[key])
            for key in ('pairs', 'noise_pairs'):
                self.assertNotIn(key, row)
                self.assertNotIn(key, stored)
                self.assertIn(key, r)
            self.assertEqual(replay.append_ledger(root / 'ledger', r, 'crypto_llm', 'h0')['idea_id'], 'v')

    def test_replay_ledger_trade_ratio_and_change_type_validation(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'ledger'
            for baseline, variant, expected in ((.4, .6, 1.5), (0, 0, None), (0, .5, None)):
                with self.subTest(baseline=baseline, variant=variant):
                    r = result()
                    change = variant / baseline - 1 if baseline else math.inf if variant else 0
                    r['auxiliary'] = dict(open_rates=dict(baseline=baseline, variant=variant), open_rate_change=change)
                    row = replay.append_ledger(path, r, 'crypto_llm', 'h0')
                    if expected is None:
                        self.assertIsNone(row['trade_ratio'])
                    else:
                        self.assertAlmostEqual(row['trade_ratio'], expected)
                    if math.isinf(change):
                        self.assertEqual(row['verdict'], 'fewer_trades')
                        self.assertEqual(evolution_ledger.read_rows(path)[-1]['auxiliary']['open_rate_change'], 'inf')
            before = path.read_bytes()
            for change_type in ('B', 'C', 'E', 'Z'):
                with self.subTest(change_type=change_type), self.assertRaises(ValueError):
                    replay.append_ledger(path, result(), 'crypto_llm', 'h0', change_type=change_type)
            self.assertEqual(path.read_bytes(), before)

    def test_replay_ledger_concurrent_appends(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'ledger'
            with ThreadPoolExecutor(max_workers=4) as pool:
                rows = list(pool.map(lambda _: replay.append_ledger(path, result(), 'crypto_llm', 'h0'), range(8)))
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
