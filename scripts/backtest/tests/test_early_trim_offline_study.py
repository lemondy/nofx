"""Synthetic transactions and minute bars only; no network or real database."""
from contextlib import redirect_stdout
import io
import json
import math
from pathlib import Path
import random
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from scripts.backtest import early_trim_offline_study as s


T0 = s.timestamp_ms('2026-10-26T00:00:00Z')


def trade(**changes):
    result = dict(id=1, trader_id=s.TRIM_CANDIDATE, symbol='BTCUSDT', side='long',
                  entry_time=T0 + 1234, exit_time=T0 + 4 * s.MINUTE,
                  entry_price=100., exit_price=101., initial_stop_loss=90.,
                  entry_quantity=1., quantity=.1, tp_trim_done=1, ai_managed=1)
    result.update(changes)
    return result


def bars(closes=(100., 108., 107., 101., 101.), start=T0):
    return [[start + i * s.MINUTE, price, price, price, price] for i, price in enumerate(closes)]


def write_inputs(root, trades, histories):
    source = root / 'source'
    source.mkdir(exist_ok=True)
    path = source / 'trades.jsonl'
    path.write_text(''.join(json.dumps(t) + '\n' for t in trades))
    klines = root / 'klines'
    klines.mkdir(exist_ok=True)
    manifest = dict(version=1, files={})
    for item, history in zip(trades, histories):
        name = s.kline_filename(item)
        s.write_json(klines / name, history)
        manifest['files'][name] = s.file_metadata(klines / name, history)
    s.write_json(klines / 'manifest.json', manifest)
    return path, klines


class SyntheticTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        network = patch.object(s.market_cache, 'fetch_binance_klines', side_effect=AssertionError('network forbidden'))
        network.start()
        self.addCleanup(network.stop)


class Execution(SyntheticTest):
    def assert_matches(self, item, history, level, **options):
        actual = s.simulate_arm(item, history, level, **options)
        self.assertLessEqual(abs(actual['r'] - s.arithmetic_reference(item, history, level, **options)), 1e-9)
        return actual

    def test_no_trigger_includes_entry_and_exit_fees(self):
        item = trade(tp_trim_done=0)
        result = self.assert_matches(item, bars((100, 103, 104, 105, 106)), .8)
        self.assertIsNone(result['trigger'])
        self.assertEqual([fill['kind'] for fill in result['fills']], ['entry', 'exit'])
        self.assertAlmostEqual(result['r'], (1 - .0005 * (100 + 101)) / 10)

    def test_only_candidate_triggers(self):
        candidate = self.assert_matches(trade(), bars(), .8)
        baseline = self.assert_matches(trade(), bars(), 1.2)
        self.assertIsNotNone(candidate['trigger'])
        self.assertIsNone(baseline['trigger'])
        self.assertAlmostEqual(candidate['fills'][1]['price'], 108 * (1 - .0003))
        self.assertAlmostEqual(candidate['fills'][1]['size'], -1 / 3)
        self.assertAlmostEqual(candidate['fills'][2]['size'], -2 / 3)
        self.assertEqual(candidate['fills'][0]['price'], 100)
        self.assertEqual(candidate['fills'][2]['price'], 101)

    def test_both_thresholds_candidate_trims_only_once(self):
        history = bars((100, 108, 112, 114, 115))
        for level, price in ((.8, 108), (1.2, 112)):
            result = self.assert_matches(trade(), history, level)
            self.assertEqual(len(result['fills']), 3)
            self.assertEqual(result['trigger']['price'], price)
            self.assertEqual(sum(fill['kind'] == 'trim' for fill in result['fills']), 1)

    def test_short_direction_and_adverse_slippage(self):
        item = trade(side='SHORT', initial_stop_loss=110., exit_price=95.)
        history = bars((100, 92, 88, 91, 90))
        for level in (.8, 1.2):
            result = self.assert_matches(item, history, level)
            self.assertGreater(result['fills'][1]['price'], result['trigger']['price'])
            self.assertGreater(result['fills'][1]['size'], 0)

    def test_entry_minute_and_bars_closing_after_exit_are_excluded(self):
        item = trade(exit_time=T0 + 3 * s.MINUTE + 30000)
        history = bars((115, 101, 102, 115, 120))
        self.assertIsNone(self.assert_matches(item, history, .8)['trigger'])
        item['exit_time'] = T0 + 4 * s.MINUTE
        self.assertEqual(self.assert_matches(item, history, .8)['trigger']['time_ms'], item['exit_time'])

    def test_trim_at_exact_exit_close_has_two_ordered_fills(self):
        item = trade(exit_time=T0 + 2 * s.MINUTE)
        result = self.assert_matches(item, bars(), .8)
        self.assertEqual(result['fills'][1]['time_ms'], result['fills'][2]['time_ms'])
        self.assertEqual(result['fills'][2]['price'], item['exit_price'])

    def test_dust_uses_original_quantity_and_unslipped_trigger_price(self):
        item = trade(entry_quantity=.01, quantity=100.)
        result = self.assert_matches(item, bars(), .8)
        self.assertEqual(result['trigger']['fraction'], 1.)
        self.assertEqual([fill['kind'] for fill in result['fills']], ['entry', 'trim'])
        item['entry_quantity'] = 5 / (108 * (2 / 3))
        self.assertEqual(self.assert_matches(item, bars(), .8)['trigger']['fraction'], 1 / 3)
        item['entry_quantity'] = 1.
        item['quantity'] = .001
        self.assertEqual(self.assert_matches(item, bars(), .8)['trigger']['fraction'], 1 / 3)

    def test_touch_sensitivity_and_10bp(self):
        history = bars((100, 101, 102, 103, 104))
        history[1][2] = 113
        self.assertIsNone(s.find_trigger(trade(), history, .8))
        for level in (.8, 1.2):
            result = self.assert_matches(trade(), history, level, touch=True)
            self.assertAlmostEqual(result['fills'][1]['price'], 100 + 10 * level)
        ten = self.assert_matches(trade(), bars(), .8, slippage=.001)
        self.assertAlmostEqual(ten['fills'][1]['price'], 108 * .999)

    def test_randomized_native_accounting_matches_reference(self):
        rng = random.Random(121)
        for number in range(24):
            sign = 1 if number % 2 else -1
            entry = rng.uniform(.1, 50000)
            risk = entry * rng.uniform(.001, .1)
            prices = [entry] + [entry + sign * risk * rng.uniform(-1, 3) for _ in range(4)]
            item = trade(entry_price=entry, initial_stop_loss=entry - sign * risk,
                         exit_price=entry + sign * risk * rng.uniform(-1, 3),
                         side='long' if sign > 0 else 'short', entry_quantity=10 / entry)
            for level in (None, .8, 1.2):
                self.assert_matches(item, bars(prices), level, touch=number % 3 == 0,
                                    slippage=.001 if number % 5 == 0 else .0003)


class DataValidation(SyntheticTest):
    def test_missing_minutes_excluded_and_exclusion_limit(self):
        trades = [trade(id=i, symbol=f'S{i}USDT') for i in range(100)]
        histories = [bars() for _ in trades]
        for i in range(5):
            histories[i].pop(2)
        _, klines = write_inputs(self.root, trades, histories)
        validation, retained = s.validate_data(trades, klines)
        self.assertTrue(validation['valid'])
        self.assertEqual(len(retained), 95)
        self.assertEqual(validation['exclusion_fraction'], .05)
        histories[5].pop(2)
        _, klines = write_inputs(self.root, trades, histories)
        validation, _ = s.validate_data(trades, klines)
        self.assertFalse(validation['valid'])
        self.assertIn('分钟覆盖剔除比例超过 5%', validation['reasons'])

    def test_exact_one_percent_missing_is_retained(self):
        item = trade(exit_time=T0 + 99 * s.MINUTE)
        history = bars([100] * 100)
        history.pop(42)
        _, klines = write_inputs(self.root, [item], [history])
        validation, retained = s.validate_data([item], klines)
        self.assertTrue(validation['valid'])
        self.assertEqual(len(retained), 1)
        self.assertEqual(validation['details'][0]['missing_fraction'], .01)

    def test_entry_price_boundary_and_95_percent_denominator(self):
        trades = [trade(id=i, symbol=f'S{i}USDT', entry_price=100.2 if i < 95 else 100.21) for i in range(100)]
        _, klines = write_inputs(self.root, trades, [bars() for _ in trades])
        validation, retained = s.validate_data(trades, klines)
        self.assertTrue(validation['valid'])
        self.assertEqual(validation['entry_price_match_fraction'], .95)
        self.assertEqual(len(retained), 100)
        trades[0]['entry_price'] = 99.79
        validation, _ = s.validate_data(trades, klines)
        self.assertFalse(validation['valid'])
        self.assertEqual(validation['entry_price_match_fraction'], .94)

    def test_missing_entry_counts_as_price_mismatch(self):
        _, klines = write_inputs(self.root, [trade()], [bars()[1:]])
        validation, _ = s.validate_data([trade()], klines)
        self.assertEqual(validation['entry_price_match_fraction'], 0)
        self.assertFalse(validation['valid'])

    def test_hash_mismatch_and_malformed_ohlc_are_invalid(self):
        _, klines = write_inputs(self.root, [trade()], [bars()])
        path = klines / s.kline_filename(trade())
        path.write_text(path.read_text() + '\n')
        self.assertFalse(s.validate_data([trade()], klines)[0]['valid'])
        history = bars()
        history[1][3] = 200
        _, klines = write_inputs(self.root, [trade()], [history])
        self.assertIn('OHLC', s.validate_data([trade()], klines)[0]['details'][0]['error'])

    def test_zero_r_and_missing_original_quantity_are_invalid(self):
        for changes in (dict(initial_stop_loss=100.), dict(entry_quantity=0.)):
            item = trade(**changes)
            _, klines = write_inputs(self.root, [item], [bars()])
            self.assertFalse(s.validate_data([item], klines)[0]['valid'])


class MechanismAndStatistics(SyntheticTest):
    def test_accuracy_threshold_and_confusion_matrix(self):
        samples = [(trade(id=i, tp_trim_done=int(i < 17)), bars()) for i in range(20)]
        result = s.validate_mechanism(samples)
        self.assertTrue(result['valid'])
        self.assertEqual(result['accuracy'], .85)
        self.assertEqual(result['confusion_matrix'], dict(tp=17, tn=0, fp=3, fn=0))
        samples[-4][0]['tp_trim_done'] = 0
        self.assertFalse(s.validate_mechanism(samples)['valid'])
        self.assertFalse(s.validate_mechanism([])['valid'])
        both = [(trade(id=22, tp_trim_done=0), bars([100] * 5)),
                (trade(id=23, tp_trim_done=1), bars([100] * 5))]
        self.assertEqual(s.validate_mechanism(both)['confusion_matrix'], dict(tp=0, tn=1, fp=0, fn=1))

    def test_four_thresholds_and_strict_updated_time(self):
        samples = []
        for i, (trader_id, (updated, level)) in enumerate(s.MECHANISM.items()):
            start = s.timestamp_ms(updated) // s.MINUTE * s.MINUTE
            item = trade(id=i, trader_id=trader_id, entry_time=s.timestamp_ms(updated) + 1,
                         exit_time=start + 4 * s.MINUTE, tp_trim_done=int(level == .8))
            samples.append((item, bars(start=start)))
            samples.append((dict(item, id=i + 10, entry_time=s.timestamp_ms(updated), tp_trim_done=1), bars(start=start)))
        result = s.validate_mechanism(samples)
        self.assertTrue(result['valid'])
        self.assertEqual(result['n'], 4)
        self.assertEqual(result['confusion_matrix'], dict(tp=1, tn=3, fp=0, fn=0))

    def test_all_verdict_branches_and_fixed_threshold(self):
        stats = dict(n=100, z=2.241, dev_delta=.1, holdout_delta=.2)
        for changes, expected in (({}, 'strong'), ({'z': 2.24099}, 'screen_pass'),
                                  ({'z': 1.}, 'screen_pass'), ({'z': .999}, 'fail'),
                                  ({'z': None}, 'fail'), ({'dev_delta': 0.}, 'fail'),
                                  ({'holdout_delta': -.1}, 'fail'), ({'n': 99}, 'invalid')):
            with self.subTest(changes=changes):
                self.assertEqual(s.decide_verdict(dict(stats, **changes)), expected)
        self.assertEqual(s.decide_verdict(stats, valid=False), 'invalid')

    def test_utc_entry_week_weighting_and_odd_halves(self):
        dates = ['2026-10-04T23:59:59Z', '2026-10-05T00:00:00Z', '2026-10-11T23:59:59Z',
                 '2026-10-12T00:00:00Z', '2026-10-19T00:00:00Z']
        rows = [dict(id=i, entry_time=s.timestamp_ms(date), delta=float(i)) for i, date in enumerate(dates)]
        stats = s.comparison(list(reversed(rows)))
        expected = s.weekly_statistics([(s.datetime.fromtimestamp(r['entry_time'] / 1000, s.timezone.utc), r['delta']) for r in rows])
        self.assertEqual(stats['n_weeks'], 4)
        self.assertEqual(stats['se'], expected['se'])
        self.assertEqual(stats['mean_delta'], 2.)
        self.assertEqual(stats['dev_delta'], .5)
        self.assertEqual(stats['holdout_delta'], 3.)
        self.assertEqual(stats['half_sizes'], [2, 3])
        self.assertEqual(s.z_score(0, 0), 0)
        self.assertEqual(s.z_score(1, 0), math.inf)
        self.assertEqual(s.z_score(-1, 0), -math.inf)


class FetchAndExport(SyntheticTest):
    def test_incremental_fetch_bounds_merge_manifest_and_shared_filename(self):
        items = [trade(), trade(id=2, exit_time=T0 + 7 * s.MINUTE), trade(id=3, symbol='ETHUSDT')]
        source = self.root / 'trades.jsonl'
        source.write_text(''.join(json.dumps(t) + '\n' for t in items))
        base = self.root / 'klines'
        base.mkdir()
        extra = bars([100], start=T0 - 10 * s.MINUTE)
        s.write_json(base / s.kline_filename(items[0]), extra)
        unrelated = base / 'keep.txt'
        unrelated.write_text('preserved')
        calls, sleeps = [], []

        def fake(symbol, interval, start, end):
            calls.append((symbol, interval, start, end))
            return bars([100] * ((end - start) // s.MINUTE + 3), start=start)

        manifest = s.fetch_klines(source, base, fetch=fake, sleeper=sleeps.append)
        self.assertEqual(len(calls), 2)
        self.assertEqual(sleeps, [.3])
        self.assertIn(('BTCUSDT', '1m', T0 - 5 * s.MINUTE, T0 + 13 * s.MINUTE), calls)
        name = s.kline_filename(items[0])
        rows = json.loads((base / name).read_bytes())
        self.assertEqual(rows[0], extra[0])
        self.assertEqual(rows[-1][0], T0 + 12 * s.MINUTE)
        self.assertTrue(manifest['files'][name]['complete'])
        self.assertEqual(manifest['files'][name]['sha256'], s.sha256((base / name).read_bytes()))
        before = (base / name).read_bytes()
        s.fetch_klines(source, base, fetch=lambda *a: self.fail('complete cache fetched'), sleeper=sleeps.append)
        self.assertEqual((base / name).read_bytes(), before)
        self.assertEqual(unrelated.read_text(), 'preserved')
        # Repair a single missing cached minute, preserving every other row.
        rows.pop(4)
        s.write_json(base / name, rows)
        s.fetch_klines(source, base, fetch=fake, sleeper=sleeps.append)
        self.assertEqual(len(calls), 3)
        self.assertEqual(json.loads((base / name).read_bytes())[0], extra[0])
        with self.assertRaises(ValueError):
            s.fetch_klines(source, base, sleep=.299, fetch=fake)

    def test_incomplete_download_remains_and_is_retried(self):
        source = self.root / 'trades.jsonl'
        source.write_text(json.dumps(trade()) + '\n')
        base = self.root / 'klines'
        manifest = s.fetch_klines(source, base, fetch=lambda *a: bars(), sleeper=lambda _: None)
        self.assertFalse(manifest['files'][s.kline_filename(trade())]['complete'])
        self.assertTrue((base / s.kline_filename(trade())).exists())

    def make_database(self):
        db = self.root / 'fixture.db'
        with sqlite3.connect(db) as connection:
            connection.execute('CREATE TABLE trader_positions (' + ','.join(
                name + (' TEXT' if name in ('trader_id', 'symbol', 'side') else ' REAL' if name in (
                    'entry_price', 'exit_price', 'initial_stop_loss', 'quantity', 'entry_quantity') else ' INTEGER')
                for name in s.FIELDS) + ',status TEXT)')
            connection.execute('CREATE TABLE traders (id TEXT,strategy_id TEXT)')
            connection.execute('CREATE TABLE strategies (id TEXT,updated_at TEXT,config TEXT)')
            for i, trader_id in enumerate(s.TRADERS):
                updated, level = s.MECHANISM.get(trader_id, ('2026-11-01T00:00:00Z', 1.2))
                connection.execute('INSERT INTO traders VALUES (?,?)', (trader_id, str(i)))
                connection.execute('INSERT INTO strategies VALUES (?,?,?)',
                                   (str(i), updated, json.dumps(dict(risk_control=dict(tp_trim_at_r=level)))))
                item = trade(id=i + 1, trader_id=trader_id)
                self.insert(connection, item)
            rejected = [dict(trader_id=s.LIVE, ai_managed=0), dict(trader_id='unknown'),
                        dict(initial_stop_loss=0.), dict(exit_time=0), dict(exit_price=0.),
                        dict(entry_time=s.SINCE - 1), dict(entry_time=s.UNTIL),
                        dict(exit_time=T0), dict(status='OPEN')]
            for i, changes in enumerate(rejected, 100):
                self.insert(connection, trade(id=i, **changes))
            self.insert(connection, trade(id=20, entry_time=s.SINCE))
        return db

    @staticmethod
    def insert(connection, item):
        connection.execute('INSERT INTO trader_positions VALUES (' + ','.join('?' for _ in range(len(s.FIELDS) + 1)) + ')',
                           [item[key] for key in s.FIELDS] + [item.get('status', 'CLOSED')])

    def test_export_opens_read_only_selects_full_ids_and_freezes_bytes(self):
        db = self.make_database()
        original_bytes = db.read_bytes()
        real_connect = sqlite3.connect
        opened = []

        def readonly_connect(database_uri, **options):
            self.assertTrue(database_uri.startswith('file:'))
            self.assertTrue(database_uri.endswith('?mode=ro'))
            self.assertTrue(options['uri'])
            connection = real_connect(database_uri, **options)
            with self.assertRaises(sqlite3.OperationalError):
                connection.execute('CREATE TABLE forbidden (id INTEGER)')
            opened.append(database_uri)
            return connection

        out = self.root / 'export'
        with patch.object(s.sqlite3, 'connect', side_effect=readonly_connect):
            meta = s.export_trades(db, out)
        self.assertEqual(len(opened), 1)
        self.assertEqual(db.read_bytes(), original_bytes)
        trades, digest = s.load_trades(out / 'trades.jsonl')
        self.assertEqual([t['id'] for t in trades], [20, 1, 2, 3, 4, 5])
        self.assertEqual(meta['n'], 6)
        self.assertEqual(meta['sha256'], digest)
        self.assertEqual(meta['counts_by_trader'][s.TRIM_CANDIDATE], 2)
        self.assertEqual(set(meta['strategies']), set(s.TRADERS))
        self.assertEqual(meta['strategies'][s.TRIM_CANDIDATE]['tp_trim_at_r'], .8)

    def test_export_rejects_strategy_time_or_threshold_mismatch(self):
        db = self.make_database()
        for column, value in (('updated_at', '2026-10-22T01:02:39Z'),
                              ('config', json.dumps(dict(risk_control=dict(tp_trim_at_r=1.2))))):
            with sqlite3.connect(db) as connection:
                connection.execute(f'UPDATE strategies SET {column}=? WHERE id=?', (value, '3'))
            with self.assertRaisesRegex(ValueError, '不一致'):
                s.export_trades(db, self.root / 'bad-export')
            self.assertFalse((self.root / 'bad-export').exists())
            with sqlite3.connect(db) as connection:
                updated, level = s.MECHANISM[s.TRIM_CANDIDATE]
                connection.execute('UPDATE strategies SET updated_at=?,config=? WHERE id=?',
                                   (updated, json.dumps(dict(risk_control=dict(tp_trim_at_r=level))), '3'))


class StudyAndCLI(SyntheticTest):
    def test_excess_exclusions_invalidate_even_with_100_retained(self):
        items = [trade(id=i, symbol=f'S{i}USDT') for i in range(107)]
        histories = [bars() for _ in items]
        for history in histories[:6]:
            history.pop(2)
        path, klines = write_inputs(self.root, items, histories)
        with patch.object(s, 'simulate_arm', side_effect=AssertionError('invalid data calculated')):
            result = s.study(path, klines)
        self.assertEqual(result['n'], 101)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIn('分钟覆盖剔除比例超过 5%', result['invalid_reasons'])

    def test_strategy_metadata_mismatch_prevents_pnl(self):
        path, klines = self.full_fixture()
        items, digest = s.load_trades(path)
        settings = {trader: dict(updated_at=s.timestamp_ms(updated), tp_trim_at_r=level)
                    for trader, (updated, level) in s.MECHANISM.items()}
        # The DB stores microseconds: a sub-second offset must still match.
        settings[s.TRIM_CONTROL]['updated_at'] += 107
        s.check_strategy_metadata({k: dict(v) for k, v in settings.items()})
        settings[s.TRIM_CANDIDATE]['updated_at'] += 1000
        s.write_json(path.with_name('export_meta.json'), dict(
            sha256=digest, n=len(items), prereg_sha256=s.PREREG_SHA256, strategies=settings))
        with patch.object(s, 'simulate_arm', side_effect=AssertionError('invalid metadata calculated')):
            result = s.study(path, klines)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIn('策略更新时间', ' '.join(result['invalid_reasons']))

    def test_prereg_mismatch_raises_before_calculation(self):
        self.assertEqual(s.sha256(s.verify_prereg()), s.PREREG_SHA256)
        bad = self.root / 'prereg.md'
        bad.write_text('modified')
        with self.assertRaisesRegex(ValueError, 'sha256 mismatch'):
            s.verify_prereg(bad)
        with patch.object(s, 'PREREG', bad), patch.object(s, 'load_trades') as load:
            with self.assertRaises(ValueError):
                s.study(self.root / 'absent', self.root)
            load.assert_not_called()

    def test_invalid_run_still_writes_chinese_report_before_any_pnl(self):
        path, klines = write_inputs(self.root, [trade(tp_trim_done=0)], [bars()])
        out = self.root / 'output'
        with patch.object(s, 'simulate_arm', side_effect=AssertionError('PnL before validation')):
            with redirect_stdout(io.StringIO()):
                code = s.main(['run', '--trades', str(path), '--klines-dir', str(klines), '--out', str(out)])
        self.assertEqual(code, 1)
        result = json.loads((out / 'results.json').read_bytes())
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIsNone(result['statistics'])
        self.assertIn('机制', (out / 'report.md').read_text())
        self.assertEqual(result['strategy_metadata_source'], 'prereg_fallback')

    def test_export_meta_mismatch_is_invalid_and_reportable(self):
        path, klines = write_inputs(self.root, [trade()], [bars()])
        s.write_json(path.with_name('export_meta.json'), dict(sha256='bad', n=1, prereg_sha256=s.PREREG_SHA256))
        result = s.study(path, klines)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIn('冻结交易', ' '.join(result['invalid_reasons']))

    def full_fixture(self):
        items, histories = [], []
        start = s.timestamp_ms('2026-10-20T00:00:00Z')
        for i in range(100):
            at = start + i * 4 * 3600000
            items.append(trade(id=i, entry_time=at + 1000, exit_time=at + 4 * s.MINUTE, exit_price=100.))
            histories.append(bars((100, 108, 107, 100, 100), start=at))
        return write_inputs(self.root, items, histories)

    def test_full_run_descriptions_and_optional_ledger(self):
        path, klines = self.full_fixture()
        out, ledger = self.root / 'output', self.root / 'ledger.jsonl'
        s.evolution_ledger.append_event(ledger, 'crypto_llm', lambda k, previous: dict(
            stage='S1', change_type='B', idea_id='previous', baseline_version='fixture', verdict='fail'))
        with redirect_stdout(io.StringIO()), patch.object(s, 'write_ledger', wraps=s.write_ledger) as writer:
            self.assertEqual(s.main(['run', '--trades', str(path), '--klines-dir', str(klines), '--out', str(out)]), 0)
            writer.assert_not_called()
        result = json.loads((out / 'results.json').read_bytes())
        self.assertEqual(result['verdict'], 'strong')
        self.assertEqual(result['statistics']['half_sizes'], [50, 50])
        self.assertEqual(result['descriptions']['mfe_groups']['[0.8,1.2)R']['n'], 100)
        self.assertEqual(result['descriptions']['post_trigger']['n'], 100)
        self.assertIn('daily', result['descriptions'])
        self.assertEqual(result['descriptions']['baseline_vs_no_trim']['mean_delta'], 0.)
        self.assertEqual(result['trades'][0]['post_trigger_path']['exit_relative_r'], -.8)
        self.assertEqual(len(result['trades'][0]['candidate']['fills']), 3)
        self.assertIn('线上配对照预注册继续运行', (out / 'report.md').read_text())
        # Ledger receives the in-memory numeric z, including infinity if SE=0.
        numeric_result = dict(result, statistics=dict(result['statistics'], z=2.5))
        row = s.write_ledger(numeric_result, ledger)
        self.assertEqual(row['K'], 2)
        self.assertEqual(row['idea_id'], 'early-trim-0p8r-offline')
        self.assertEqual(row['change_type'], 'C')
        self.assertEqual(row['prereg_sha256'], s.PREREG_SHA256)
        self.assertEqual(row['n'], 100)
        before = ledger.read_bytes()
        with self.assertRaisesRegex(ValueError, 'K=3'):
            s.write_ledger(numeric_result, ledger)
        self.assertEqual(ledger.read_bytes(), before)

    def test_cli_ledger_optional_path_and_invalid_does_not_count(self):
        path, klines = write_inputs(self.root, [trade()], [bars()])
        ledger = self.root / 'ledger.jsonl'
        for ledger_args in (['--write-ledger', str(ledger)], ['--write-ledger']):
            with patch.object(s, 'LEDGER', ledger), redirect_stdout(io.StringIO()):
                self.assertEqual(s.main(['run', '--trades', str(path), '--klines-dir', str(klines),
                                         '--out', str(self.root / 'output'), *ledger_args]), 1)
        rows = s.evolution_ledger.read_rows(ledger)
        self.assertEqual(len(rows), 2)
        self.assertTrue(all(row['verdict'] == 'invalid' and row['K'] is None for row in rows))
        self.assertTrue(all(row['preregistered_K'] == 2 for row in rows))


if __name__ == '__main__':
    unittest.main()
