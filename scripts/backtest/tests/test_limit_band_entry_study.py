"""Synthetic C1 fixtures only: no network, real database, data/ or real ledger."""
from contextlib import redirect_stderr, redirect_stdout
import io
import json
import math
from pathlib import Path
import random
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from scripts.backtest import limit_band_entry_study as s

T0 = s.timestamp_ms('2026-10-19T00:00:00Z')


def trade(**changes):
    item = dict(id=1, trader_id=s.LIVE, symbol='BTCUSDT', side='long',
                entry_time=T0 + 1234, exit_time=T0 + 40 * s.MINUTE,
                entry_price=100., exit_price=101., initial_stop_loss=90., ai_managed=1)
    item.update(changes)
    return item


def bars(start=T0, *, low=100., high=100., close=100.):
    return [[start + i * s.MINUTE, 100., high if i > 0 else 100.,
             low if i > 0 else 100., close if i > 0 else 100.]
            for i in range(-1440, 32)]


def log_series(logs, offsets=None):
    offsets = range(len(logs)) if offsets is None else offsets
    return [[T0 + (i - 100) * s.MINUTE, *(100 * math.exp(v) for _ in range(4))]
            for i, v in zip(offsets, logs)]


def write_inputs(root, items, histories, *, metadata=True):
    source = root / 'source'
    source.mkdir(exist_ok=True)
    path = source / 'trades.jsonl'
    path.write_text(''.join(json.dumps(t) + '\n' for t in items))
    klines = root / 'klines'
    klines.mkdir(exist_ok=True)
    manifest = dict(version=1, files={}, trades_sha256=s.sha256(path.read_bytes()))
    for item, history in zip(items, histories):
        name = s.kline_filename(item)
        s.write_json(klines / name, history)
        manifest['files'][name] = s.file_metadata(klines / name, history)
    s.write_json(klines / 'manifest.json', manifest)
    if metadata:
        s.write_json(source / 'export_meta.json', dict(
            n=len(items), sha256=manifest['trades_sha256'], prereg_sha256=s.PREREG_SHA256,
            strategies={trader: dict(limit_entry_enabled=False) for trader in s.TRADERS}))
    return path, klines


class SyntheticTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        network = patch.object(s.market_cache, 'http_json', side_effect=AssertionError('network forbidden'))
        network.start()
        self.addCleanup(network.stop)

    def fixture(self, n=100):
        items, histories = [], []
        for i in range(n):
            start = T0 + (i % 4) * 7 * 24 * 60 * s.MINUTE
            # 4 UTC weeks, with shared files exercising deduplication.
            items.append(trade(id=i, entry_time=start + 1234, exit_time=start + 40 * s.MINUTE))
            histories.append(bars(start, low=99.9))
        return write_inputs(self.root, items, histories)


class RollAndBand(SyntheticTest):
    def test_known_negative_covariance_and_common_mean(self):
        a = .001
        estimate = s.roll_estimate(trade(), log_series([0, a, 0, a]))
        self.assertEqual(estimate['n_returns'], 3)
        self.assertEqual(estimate['n_pairs'], 2)
        self.assertAlmostEqual(estimate['cov1'], -8 * a * a / 9, places=17)
        self.assertAlmostEqual(estimate['s_g'], math.sqrt(8) * a / 3, places=14)

    def test_positive_covariance_clamps_s_g_to_zero(self):
        a = .001
        # Returns -3a,-a,a,3a have zero mean and positive lag covariance.
        estimate = s.roll_estimate(trade(), log_series([0, -3*a, -4*a, -3*a, 0]))
        self.assertAlmostEqual(estimate['cov1'], 5 * a * a / 3, places=17)
        self.assertEqual(estimate['s_g'], 0.)

    def test_gaps_skip_returns_and_lag_pairs(self):
        a = .001
        estimate = s.roll_estimate(trade(), log_series([0, a, 0, 0, a, 0], [0, 1, 2, 4, 5, 6]))
        self.assertEqual(estimate['n_returns'], 4)
        self.assertEqual(estimate['n_pairs'], 2)
        self.assertAlmostEqual(estimate['cov1'], -a*a, places=17)
        self.assertAlmostEqual(estimate['s_g'], a, places=14)
        with self.assertRaisesRegex(ValueError, 'lag pair'):
            s.roll_estimate(trade(), log_series([0, a, 0], [0, 1, 3]))

    def test_roll_uses_only_pre_window(self):
        history = log_series([0, .001, 0, .001])
        original = s.roll_estimate(trade(), history)
        extra = [[T0 - 1441*s.MINUTE, 1., 1., 1., 1.], [T0, 1., 1., 1., 1.],
                 [T0 + s.MINUTE, 1000., 1000., 1000., 1000.]]
        self.assertEqual(s.roll_estimate(trade(), sorted(history + extra)), original)

    def test_theta_equation(self):
        for sigma in (0., .0001, .001, .01):
            theta = s.theta_star(sigma)
            self.assertAlmostEqual(theta, .5 * (s.PHI + math.sqrt(s.PHI**2 + 4*sigma**2)))
            self.assertAlmostEqual(theta * (theta - s.PHI), sigma**2)
        self.assertEqual(s.theta_star(0), .0008)


class Execution(SyntheticTest):
    def assert_pair(self, item, history):
        actual = s.replay_pair(item, history)
        expected = s.arithmetic_reference(item, history)
        for arm in ('candidate', 'baseline'):
            self.assertLessEqual(abs(actual[arm]['r'] - expected[arm]), 1e-9)
        self.assertLessEqual(abs(actual['delta'] - expected['delta']), 1e-9)
        return actual

    def test_long_short_strict_versus_touch_and_fees(self):
        for side, stop in (('long', 90.), ('short', 110.)):
            item = trade(side=side, initial_stop_loss=stop)
            sign = 1 if side == 'long' else -1
            limit = 100*(1-sign*s.PHI)
            history = bars(low=limit if sign > 0 else 100., high=limit if sign < 0 else 100.)
            self.assertEqual(s.entry_plan(item, history)['outcome'], 'fallback')
            touch = s.entry_plan(item, history, touch=True)
            self.assertEqual(touch['outcome'], 'filled')
            self.assertEqual(touch['price'], limit)
            index = 1441
            history[index][3 if sign > 0 else 2] = limit - sign*.00001
            plan = s.entry_plan(item, history)
            self.assertEqual(plan['time_ms'], T0+2*s.MINUTE)
            result = self.assert_pair(item, history)
            self.assertAlmostEqual(result['candidate']['fills'][0]['commission'], limit*.0002)
            self.assertAlmostEqual(result['candidate']['fills'][1]['commission'], 101*.0005)

    def test_entry_minute_and_incomplete_or_late_bars_ignored(self):
        history = bars()
        history[1440][3] = 90.  # entry minute ignored
        history[-1][3] = 90.  # +31m ignored
        self.assertEqual(s.entry_plan(trade(), history)['outcome'], 'fallback')
        history[1442][3] = 90.  # +2m closes at +3m
        item = trade(exit_time=T0+3*s.MINUTE-1)
        self.assertEqual(s.entry_plan(item, history)['outcome'], 'missed')
        item['exit_time'] += 1
        self.assertEqual(s.entry_plan(item, history)['outcome'], 'filled')

    def test_first_fill_and_last_thirtieth_bar(self):
        history = bars()
        history[-2][3] = 99.8  # +30m
        item = trade(exit_time=T0+31*s.MINUTE)
        self.assertEqual(s.entry_plan(item, history)['time_ms'], T0+31*s.MINUTE)
        self.assert_pair(item, history)  # identical entry/exit timestamp
        history[1444][3] = 99.8
        self.assertEqual(s.entry_plan(item, history)['time_ms'], T0+5*s.MINUTE)

    def test_fallback_price_fee_and_exit_boundary(self):
        for side, stop in (('long', 90.), ('short', 110.)):
            item = trade(side=side, initial_stop_loss=stop, exit_time=T0+31*s.MINUTE)
            history = bars()
            plan = s.entry_plan(item, history)
            self.assertEqual(plan['outcome'], 'fallback')
            self.assertAlmostEqual(plan['price'], 100*(1+(.0003 if side == 'long' else -.0003)))
            self.assertEqual(plan['fee'], .0005)
            result = self.assert_pair(item, history)
            self.assertAlmostEqual(result['candidate']['fills'][0]['commission'], plan['price']*.0005)
            self.assertEqual(s.entry_plan(dict(item, exit_time=item['exit_time']-1), history)['outcome'], 'missed')

    def test_missed_has_no_candidate_orders_and_c_zero(self):
        result = self.assert_pair(trade(exit_time=T0+5*s.MINUTE), bars())
        self.assertEqual(result['plan']['outcome'], 'missed')
        self.assertEqual(result['candidate'], dict(r=0., fills=[]))
        self.assertEqual(result['delta'], -result['baseline']['r'])

    def test_stop_guard_at_or_beyond_stop(self):
        for side in ('long', 'short'):
            sign = 1 if side == 'long' else -1
            for stop in (100*(1-sign*s.PHI), 100-sign*.01):
                result = self.assert_pair(trade(side=side, initial_stop_loss=stop), bars())
                self.assertEqual(result['plan']['outcome'], 'stop_guard')
                self.assertEqual(result['delta'], 0.)
                self.assertEqual(result['candidate'], result['baseline'])

    def test_hand_computed_b_c_delta_and_unit_quantity(self):
        item = trade(entry_quantity=999., quantity=.001)
        result = self.assert_pair(item, bars(low=99.8))
        b = (101-100-.0005*100-.0005*101)/10
        p = 99.92
        c = (101-p-.0002*p-.0005*101)/10
        self.assertAlmostEqual(result['baseline']['r'], b)
        self.assertAlmostEqual(result['candidate']['r'], c)
        self.assertAlmostEqual(result['delta'], c-b)
        self.assertEqual(result['candidate']['fills'][0]['size'], 1.)
        short = s.arithmetic_reference(trade(side='short', initial_stop_loss=110.), bars(high=100.2))
        self.assertAlmostEqual(short['baseline'], (-1-.05-.0505)/10)
        self.assertAlmostEqual(short['candidate'], ((100.08-101)-.0002*100.08-.0505)/10)

    def test_no_fallback_and_other_arithmetic_sensitivities(self):
        item, history = trade(), bars()
        no = s.arithmetic_reference(item, history, fallback=False)
        self.assertEqual(no['plan']['outcome'], 'unfilled')
        self.assertEqual(no['candidate'], 0.)
        self.assertEqual(s.entry_plan(item, history, slippage=0.)['price'], 100.)
        self.assertEqual(s.entry_plan(item, bars(low=99.8), maker_fee=.0005)['fee'], .0005)
        for depth in (.0004, .0015, .003):
            self.assertEqual(s.entry_plan(item, history, depth=depth)['limit'], 100*(1-depth))

    def test_missing_bar_unfilled_full_window_is_not_fabricated(self):
        history = bars()
        history.pop(1441)
        with self.assertRaisesRegex(ValueError, '后窗缺口'):
            s.entry_plan(trade(), history)
        history[1441][3] = 99.8
        self.assertEqual(s.entry_plan(trade(), history)['outcome'], 'filled')

    def test_randomized_native_accounting_matches_arithmetic(self):
        rng = random.Random(20261113)
        for i in range(12):
            short = i % 2
            price = rng.uniform(.1, 100000)
            item = trade(side='short' if short else 'long', entry_price=price,
                         exit_price=price*rng.uniform(.97, 1.03),
                         initial_stop_loss=price*(1.02 if short else .98))
            # Flat Roll window, randomly wider future bar.
            history = [[r[0], *(v*price/100 for v in r[1:])] for r in bars(low=99.8, high=100.2)]
            self.assert_pair(item, history)


class Validation(SyntheticTest):
    def test_coverage_thresholds_and_five_percent_drop_boundary(self):
        items = [trade(id=i, symbol=f'S{i}USDT') for i in range(100)]
        histories = [bars() for _ in items]
        for history in histories[:5]:
            del history[:29]  # 29/1440 > 2%
        path, base = write_inputs(self.root, items, histories)
        validation, retained = s.validate_data(items, base)
        self.assertEqual(len(retained), 95)
        self.assertEqual(validation['exclusion_fraction'], .05)
        self.assertNotIn('分钟覆盖剔除比例超过 5%', validation['reasons'])
        del histories[5][:29]
        _, base = write_inputs(self.root, items, histories)
        self.assertIn('分钟覆盖剔除比例超过 5%', s.validate_data(items, base)[0]['reasons'])

    def test_28_missing_pre_and_one_missing_post_pass(self):
        history = bars(low=99.8)
        del history[:28]
        history.pop(-2)
        items = [trade(id=i) for i in range(100)]
        _, base = write_inputs(self.root, items, [history]*100)
        validation, retained = s.validate_data(items, base)
        self.assertTrue(validation['valid'])
        self.assertEqual(len(retained), 100)
        self.assertEqual(validation['details'][0]['pre_missing'], 28)
        self.assertEqual(validation['details'][0]['post_missing'], 1)
        history.pop(-2)
        write_inputs(self.root, items, [history]*100)
        self.assertEqual(s.validate_data(items, base)[0]['excluded'], 100)

    def test_post_window_is_not_shortened_by_exit(self):
        item = trade(exit_time=T0+s.MINUTE)
        _, base = write_inputs(self.root, [item], [bars()[:-3]])
        detail = s.validate_data([item], base)[0]['details'][0]
        self.assertEqual(detail['post_missing'], 2)
        self.assertTrue(detail['excluded'])

    def test_price_match_inclusive_95_percent_and_missing_entry(self):
        items = [trade(id=i, entry_price=100.2 if i < 95 else 100.21) for i in range(100)]
        _, base = write_inputs(self.root, items, [bars()]*100)
        self.assertTrue(s.validate_data(items, base)[0]['valid'])
        items[0]['entry_price'] = 99.79
        self.assertEqual(s.validate_data(items, base)[0]['entry_price_match_fraction'], .94)
        history = bars()
        history.pop(1440)
        write_inputs(self.root, [trade()], [history])
        self.assertEqual(s.validate_data([trade()], base)[0]['entry_price_match_fraction'], 0.)

    def test_hash_missing_manifest_and_malformed_ohlc(self):
        _, base = write_inputs(self.root, [trade()], [bars()])
        path = base / s.kline_filename(trade())
        path.write_text(path.read_text()+'\n')
        self.assertIn('manifest 哈希', ' '.join(s.validate_data([trade()], base)[0]['reasons']))
        (base / 'manifest.json').unlink()
        self.assertIn('manifest 缺失', ' '.join(s.validate_data([trade()], base)[0]['reasons']))
        history = bars()
        history[1][3] = 200
        write_inputs(self.root, [trade()], [history])
        self.assertIn('OHLC', s.validate_data([trade()], base)[0]['details'][0]['error'])

    def test_trade_field_validation(self):
        for changes in (dict(initial_stop_loss=100.), dict(entry_price=0), dict(exit_price=math.inf),
                        dict(ai_managed=0), dict(trader_id='unknown'), dict(side='buy'),
                        dict(entry_time=s.UNTIL), dict(entry_time=s.SINCE-1),
                        dict(exit_time=T0), dict(exit_time=T0+.5)):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                s.validate_trade(trade(**changes))
        s.validate_trade(trade(entry_time=s.SINCE))
        for changes in (dict(side='short', initial_stop_loss=110.), dict(trader_id=s.TRIM_CONTROL)):
            s.validate_trade(trade(**changes))


class Statistics(SyntheticTest):
    def test_every_verdict_branch_and_fewer_trades_precedence(self):
        stats = dict(n=100, z=2.638, dev_delta=.1, holdout_delta=.1, mean_delta=.1,
                     missed_fraction=0., delta_excluding_missed=.1)
        cases = [({}, 'strong'), ({'z': 2.63799}, 'screen_pass'), ({'z': 1.}, 'screen_pass'),
                 ({'z': .999}, 'fail'), ({'z': None}, 'fail'), ({'n': 99}, 'invalid'),
                 ({'dev_delta': 0.}, 'fail'), ({'holdout_delta': -.1}, 'fail'),
                 ({'missed_fraction': .06, 'delta_excluding_missed': 0.}, 'fewer_trades'),
                 ({'missed_fraction': .05, 'delta_excluding_missed': 0.}, 'strong'),
                 ({'missed_fraction': .06, 'mean_delta': 0., 'delta_excluding_missed': -.1, 'z': 0}, 'fail'),
                 ({'missed_fraction': .06, 'delta_excluding_missed': .001}, 'strong')]
        for changes, expected in cases:
            with self.subTest(changes=changes):
                self.assertEqual(s.decide_verdict(dict(stats, **changes)), expected)
        self.assertEqual(s.decide_verdict(stats, valid=False), 'invalid')

    def test_utc_week_weighting_time_halves_and_missed(self):
        dates = ['2026-10-18T23:59:59Z', '2026-10-19T00:00:00Z', '2026-10-25T23:59:59Z',
                 '2026-10-26T00:00:00Z', '2026-11-02T00:00:00Z']
        rows = [dict(id=i, entry_time=s.timestamp_ms(date), delta=float(i),
                     plan=dict(outcome='missed' if i == 4 else 'filled')) for i, date in enumerate(dates)]
        stats = s.primary_statistics(list(reversed(rows)))
        expected = s.weekly_statistics([(s.datetime.fromtimestamp(r['entry_time']/1000, s.timezone.utc), r['delta']) for r in rows])
        self.assertEqual(stats['n_weeks'], 4)
        self.assertEqual(stats['se'], expected['se'])
        self.assertEqual(stats['mean_delta'], 2.)
        self.assertEqual(stats['dev_delta'], .5)
        self.assertEqual(stats['holdout_delta'], 3.)
        self.assertEqual(stats['half_sizes'], [2, 3])
        self.assertEqual(stats['missed_fraction'], .2)
        self.assertEqual(stats['delta_excluding_missed'], 1.5)
        self.assertIsNone(s.comparison(rows[:2])['z'])
        self.assertEqual(s.z_score(0, 0), 0)
        self.assertEqual(s.z_score(1, 0), math.inf)

    def test_descriptions_and_sensitivities_are_separate(self):
        retained = [(trade(id=1), bars(low=99.8)), (trade(id=2), bars()),
                    (trade(id=3, exit_time=T0+3*s.MINUTE), bars()),
                    (trade(id=4, initial_stop_loss=99.99), bars())]
        rows = [s.replay_pair(t, h) for t, h in retained]
        result = s.describe(rows, retained)
        self.assertEqual(set(result['outcomes']), {'filled', 'fallback', 'missed', 'stop_guard'})
        self.assertTrue(all(v['rate'] == .25 for v in result['outcomes'].values()))
        self.assertEqual(result['cov1_nonnegative_fraction'], 1.)
        self.assertEqual(result['s_g_median_bp'], 0.)
        self.assertEqual(result['gamma_quantiles']['0.5'], math.inf)
        self.assertEqual(len(result['sensitivities']), 7)
        self.assertEqual(result['sensitivities']['no_fallback']['unfilled_n'], 2)
        self.assertAlmostEqual(result['sensitivities']['no_fallback']['delta_excluding_unfilled'], (rows[0]['delta']+rows[3]['delta'])/2)
        self.assertAlmostEqual(result['adverse_selection']['difference'], rows[0]['baseline']['r'] -
                               (rows[1]['baseline']['r']+rows[2]['baseline']['r'])/2)
        self.assertEqual(result['by_side']['long']['n'], 4)
        self.assertIsNone(result['daily']['z'])
        self.assertEqual(s.quantiles([0, 1, 2, 3])['0.25'], .75)


class FetchAndExport(SyntheticTest):
    @staticmethod
    def database():
        connection = sqlite3.connect(':memory:')
        connection.execute('CREATE TABLE trader_positions (' + ','.join(
            key + (' TEXT' if key in ('trader_id', 'symbol', 'side') else ' REAL' if key in (
                'entry_price', 'exit_price', 'initial_stop_loss') else ' INTEGER') for key in s.FIELDS) + ',status TEXT)')
        connection.execute('CREATE TABLE traders (id TEXT,strategy_id TEXT)')
        connection.execute('CREATE TABLE strategies (id TEXT,config TEXT)')
        for i, trader_id in enumerate(s.TRADERS):
            connection.execute('INSERT INTO traders VALUES (?,?)', (trader_id, str(i)))
            connection.execute('INSERT INTO strategies VALUES (?,?)',
                               (str(i), json.dumps(dict(risk_control=dict(limit_entry_enabled=False)))))
            FetchAndExport.insert(connection, trade(id=i+1, trader_id=trader_id))
        return connection

    @staticmethod
    def insert(connection, item):
        connection.execute('INSERT INTO trader_positions VALUES (' + ','.join('?' for _ in range(len(s.FIELDS)+1)) + ')',
                           [item[key] for key in s.FIELDS]+[item.get('status', 'CLOSED')])

    def test_in_memory_export_filter_full_ids_all_ai_and_boundaries(self):
        with self.database() as connection:
            excluded = [dict(ai_managed=0, trader_id=t) for t in s.TRADERS]
            excluded += [dict(trader_id='unknown'), dict(initial_stop_loss=0.), dict(entry_price=0.),
                         dict(exit_price=0.), dict(exit_time=0), dict(exit_time=T0),
                         dict(exit_time=T0+1500.5), dict(entry_time=s.SINCE-1),
                         dict(entry_time=s.UNTIL), dict(status='OPEN')]
            for i, changes in enumerate(excluded, 100):
                self.insert(connection, trade(id=i, **changes))
            self.insert(connection, trade(id=50, entry_time=s.SINCE))
            items, meta = s.export_snapshot(connection)
        self.assertEqual([r['id'] for r in items], [50, 1, 2, 3, 4, 5])
        self.assertEqual(meta['invalid_exit_excluded'], 2)
        self.assertEqual(set(meta['strategies']), set(s.TRADERS))

    def test_every_strategy_requires_explicit_false(self):
        for trader_index in range(5):
            for value in (True, 0, None, 'false'):
                with self.subTest(trader_index=trader_index, value=value), self.database() as connection:
                    connection.execute('UPDATE strategies SET config=? WHERE id=?',
                        (json.dumps(dict(risk_control=dict(limit_entry_enabled=value))), str(trader_index)))
                    with self.assertRaisesRegex(ValueError, 'limit_entry_enabled'):
                        s.export_snapshot(connection)
        for config in ({}, {'risk_control': {}}, {'risk_control': None}, []):
            with self.database() as connection:
                connection.execute('UPDATE strategies SET config=? WHERE id=?', (json.dumps(config), '0'))
                with self.assertRaises(ValueError):
                    s.export_snapshot(connection)
        with self.database() as connection:
            connection.execute('DELETE FROM traders WHERE id=?', (s.TRIM_CONTROL,))
            with self.assertRaisesRegex(ValueError, '策略记录'):
                s.export_snapshot(connection)

    def test_export_readonly_frozen_sha_and_refuses_overwrite(self):
        db = self.root / 'fixture.db'
        with self.database() as source, sqlite3.connect(db) as target:
            source.commit()
            source.backup(target)
        original = db.read_bytes()
        real_connect, calls = sqlite3.connect, []
        def read_only(database_uri, **options):
            self.assertTrue(database_uri.endswith('?mode=ro'))
            self.assertTrue(options['uri'])
            connection = real_connect(database_uri, **options)
            with self.assertRaises(sqlite3.OperationalError):
                connection.execute('CREATE TABLE forbidden (id INTEGER)')
            calls.append(database_uri)
            return connection
        out = self.root / 'export'
        with patch.object(s.sqlite3, 'connect', side_effect=read_only):
            meta = s.export_trades(db, out)
        self.assertEqual(len(calls), 1)
        self.assertEqual(db.read_bytes(), original)
        items, digest = s.load_trades(out / 'trades.jsonl')
        self.assertEqual(len(items), 5)
        self.assertEqual(meta['sha256'], digest)
        before = (out / 'trades.jsonl').read_bytes()
        with self.assertRaisesRegex(ValueError, '拒绝覆盖'):
            s.export_trades(db, out)
        self.assertEqual((out / 'trades.jsonl').read_bytes(), before)

    def test_fetch_bounds_dedup_manifest_pacing_and_complete_cache(self):
        items = [trade(), trade(id=2), trade(id=3, symbol='ETHUSDT')]
        path = self.root / 'trades.jsonl'
        path.write_text(''.join(json.dumps(t)+'\n' for t in items))
        base = self.root / 'klines'
        base.mkdir()
        extra = [[T0-1441*s.MINUTE, 100., 100., 100., 100.]]
        s.write_json(base / s.kline_filename(items[0]), extra)
        calls, sleeps = [], []
        def fake(symbol, interval, start, end):
            calls.append((symbol, interval, start, end))
            return bars() + [[end, 100., 100., 100., 100.]]
        manifest = s.fetch_klines(path, base, fetch=fake, sleeper=sleeps.append)
        self.assertEqual(len(calls), 2)
        self.assertEqual(sleeps, [.3])
        self.assertEqual(calls[0][1:], ('1m', T0-1440*s.MINUTE, T0+32*s.MINUTE))
        self.assertEqual((calls[0][3]-calls[0][2])//s.MINUTE, 1472)
        name = s.kline_filename(items[0])
        saved = (base / name).read_bytes()
        self.assertEqual(json.loads(saved)[0], extra[0])
        self.assertEqual(json.loads(saved)[-1][0], T0+31*s.MINUTE)
        self.assertTrue(manifest['files'][name]['complete'])
        self.assertEqual(manifest['files'][name]['sha256'], s.sha256(saved))
        s.fetch_klines(path, base, fetch=lambda *args: self.fail('complete cache fetched'))
        self.assertEqual((base / name).read_bytes(), saved)
        with self.assertRaises(ValueError):
            s.fetch_klines(path, base, sleep=.299, fetch=fake)

    def test_incomplete_fetch_retries_without_replacing_old_rows_and_checks_sha(self):
        path = self.root / 'trades.jsonl'
        path.write_text(json.dumps(trade())+'\n')
        base = self.root / 'klines'
        history = bars()
        history.pop(100)
        manifest = s.fetch_klines(path, base, fetch=lambda *a: history, sleeper=lambda _: None)
        name = s.kline_filename(trade())
        self.assertFalse(manifest['files'][name]['complete'])
        calls = []
        def repair(*args):
            calls.append(args)
            return [bars()[100]]
        manifest = s.fetch_klines(path, base, fetch=repair)
        self.assertEqual(calls[0][2:], (T0-1340*s.MINUTE, T0-1339*s.MINUTE))
        self.assertTrue(manifest['files'][name]['complete'])
        (base / name).write_text((base / name).read_text()+'\n')
        with self.assertRaisesRegex(ValueError, 'manifest'):
            s.fetch_klines(path, base, fetch=lambda *args: self.fail('hash corruption fetched'))


class StudyAndCLI(SyntheticTest):
    def test_prereg_sha_checked_before_inputs_in_all_commands(self):
        self.assertEqual(s.sha256(s.verify_prereg()), s.PREREG_SHA256)
        bad = self.root / 'prereg.md'
        bad.write_text('modified')
        with self.assertRaisesRegex(ValueError, 'sha256 mismatch'):
            s.verify_prereg(bad)
        with patch.object(s, 'PREREG', bad), patch.object(s, 'load_trades') as load:
            for operation in (lambda: s.study(self.root/'absent', self.root),
                              lambda: s.fetch_klines(self.root/'absent', self.root),
                              lambda: s.export_trades(self.root/'absent', self.root)):
                with self.assertRaisesRegex(ValueError, 'sha256 mismatch'):
                    operation()
            load.assert_not_called()

    def test_invalid_sample_and_metadata_prevent_pnl_but_write_report(self):
        path, base = write_inputs(self.root, [trade()], [bars()])
        out = self.root / 'results'
        with patch.object(s, 'simulate_arm', side_effect=AssertionError('PnL before validation')), redirect_stdout(io.StringIO()):
            self.assertEqual(s.main(['run', '--trades', str(path), '--klines-dir', str(base), '--out', str(out)]), 1)
        result = json.loads((out/'results.json').read_bytes())
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIsNone(result['statistics'])
        self.assertIn('有效交易不足 100', (out/'report.md').read_text())
        self.assertIn('NOTES', (out/'report.md').read_text())

    def test_metadata_and_manifest_binding_fail_closed(self):
        path, base = self.fixture()
        meta_path = path.with_name('export_meta.json')
        original = meta_path.read_bytes()
        with patch.object(s, 'simulate_arm', side_effect=AssertionError('PnL before metadata')):
            for key, value in (('sha256', 'bad'), ('prereg_sha256', 'bad'), ('n', 99),
                               ('strategies', {t: dict(limit_entry_enabled=True) for t in s.TRADERS})):
                metadata = json.loads(original)
                metadata[key] = value
                s.write_json(meta_path, metadata)
                self.assertEqual(s.study(path, base)['verdict'], 'invalid')
            meta_path.unlink()
            self.assertEqual(s.study(path, base)['verdict'], 'invalid')
            meta_path.write_bytes(original)
            manifest = json.loads((base/'manifest.json').read_bytes())
            manifest['trades_sha256'] = 'bad'
            s.write_json(base/'manifest.json', manifest)
            self.assertIn('manifest 与冻结交易哈希不一致', s.study(path, base)['invalid_reasons'])

    def test_runtime_backtrader_disagreement_is_invalid(self):
        path, base = self.fixture()
        actual = s.simulate_arm
        def disagree(item, plan=None):
            result = actual(item, plan)
            if plan is not None:
                result['r'] += 1e-7
            return result
        with patch.object(s, 'simulate_arm', side_effect=disagree):
            result = s.study(path, base)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIn('1e-9 R', ' '.join(result['invalid_reasons']))
        self.assertIsNone(result['statistics'])

    def test_nonfinite_broker_result_is_rejected(self):
        actual = s.simulate_arm
        def nonfinite(item, plan=None):
            return dict(r=math.nan, fills=[]) if plan is not None else actual(item)
        with patch.object(s, 'simulate_arm', side_effect=nonfinite):
            with self.assertRaisesRegex(RuntimeError, '非有限 R'):
                s.replay_pair(trade(), bars(low=99.8))

    def test_sensitive_only_gap_cannot_change_primary_verdict(self):
        items = [trade(id=i, entry_time=T0+(i%4)*7*24*60*s.MINUTE+1234,
                       exit_time=T0+(i%4)*7*24*60*s.MINUTE+40*s.MINUTE) for i in range(100)]
        histories = []
        for item in items:
            history = bars(item['entry_time']//s.MINUTE*s.MINUTE, low=99.9)
            history.pop(1443)
            histories.append(history)
        path, base = write_inputs(self.root, items, histories)
        result = s.study(path, base)
        self.assertEqual(result['verdict'], 'strong')
        self.assertTrue(result['descriptions']['sensitivities']['touch']['available'])
        deeper = result['descriptions']['sensitivities']['fixed_30bp']
        self.assertFalse(deeper['available'])
        self.assertIsNone(deeper['statistics'])
        self.assertEqual(len(deeper['errors']), 100)

    def test_over_five_percent_dropped_aborts_even_with_101_retained(self):
        items = [trade(id=i, symbol=f'S{i}USDT') for i in range(107)]
        histories = [bars() for _ in items]
        for history in histories[:6]:
            del history[:29]
        path, base = write_inputs(self.root, items, histories)
        with patch.object(s, 'simulate_arm', side_effect=AssertionError('invalid coverage PnL')):
            result = s.study(path, base)
        self.assertEqual(result['n'], 101)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIn('分钟覆盖剔除比例超过 5%', result['invalid_reasons'])

    def test_no_pnl_before_ambiguous_window_check(self):
        items = [trade(id=i) for i in range(100)]
        history = bars()
        history.pop(1441)
        path, base = write_inputs(self.root, items, [history]*100)
        with patch.object(s, 'simulate_arm', side_effect=AssertionError('ambiguous window PnL')):
            result = s.study(path, base)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIn('后窗缺口', ' '.join(result['invalid_reasons']))

    def test_full_run_chinese_report_optional_ledger_and_k_guard(self):
        path, base = self.fixture()
        out = self.root / 'results'
        ledger = self.root / 'ledger.jsonl'
        with patch.object(s, 'write_ledger', wraps=s.write_ledger) as writer, redirect_stdout(io.StringIO()):
            self.assertEqual(s.main(['run', '--trades', str(path), '--klines-dir', str(base), '--out', str(out)]), 0)
            writer.assert_not_called()
        result = json.loads((out/'results.json').read_bytes())
        self.assertEqual(result['verdict'], 'strong')
        self.assertEqual(result['K'], 6)
        self.assertEqual(result['z_K'], 2.638)
        self.assertEqual(result['statistics']['half_sizes'], [50, 50])
        self.assertEqual(result['descriptions']['outcomes']['filled']['rate'], 1.)
        self.assertLessEqual(result['mechanism']['max_error_r'], 1e-9)
        self.assertIn('由用户决定是否实施', (out/'report.md').read_text())
        for i in range(5):
            s.evolution_ledger.append_event(ledger, 'crypto_llm', lambda k, previous: dict(
                stage='S1', change_type='C', idea_id='synthetic-previous', baseline_version='fixture', verdict='fail'))
        # write_ledger takes numeric in-memory statistics, before JSON conversion.
        numeric = dict(result, statistics=dict(result['statistics'], z=3.))
        row = s.write_ledger(numeric, ledger)
        self.assertEqual(row['K'], 6)
        self.assertEqual(row['preregistered_K'], 6)
        self.assertEqual(row['idea_id'], 'limit-band-entry-c1-v1')
        self.assertEqual(row['baseline_version'], 'market-entry')
        self.assertEqual(row['prereg_sha256'], s.PREREG_SHA256)
        self.assertEqual(row['input_hashes'], result['inputs'])
        before = ledger.read_bytes()
        with self.assertRaisesRegex(ValueError, 'K=7'):
            s.write_ledger(numeric, ledger)
        self.assertEqual(ledger.read_bytes(), before)

    def test_cli_invalid_ledger_optional_path_and_no_k_count(self):
        path, base = write_inputs(self.root, [trade()], [bars()])
        ledger = self.root / 'ledger.jsonl'
        for i, ledger_args in enumerate((['--write-ledger', str(ledger)], ['--write-ledger'])):
            with patch.object(s, 'LEDGER', ledger), redirect_stdout(io.StringIO()):
                self.assertEqual(s.main(['run', '--trades', str(path), '--klines-dir', str(base),
                    '--out', str(self.root/f'out{i}'), *ledger_args]), 1)
        rows = s.evolution_ledger.read_rows(ledger)
        self.assertEqual(len(rows), 2)
        self.assertTrue(all(r['verdict'] == 'invalid' and r['K'] is None and r['preregistered_K'] == 6 for r in rows))

    def test_cli_refuses_input_aliases_and_existing_results(self):
        path, base = write_inputs(self.root, [trade()], [bars()])
        out = self.root / 'out'
        out.mkdir()
        (out/'results.json').symlink_to(path)
        before = path.read_bytes()
        bad_args = [
            ['run', '--trades', str(path), '--klines-dir', str(base), '--out', str(out)],
            ['run', '--trades', str(path), '--klines-dir', str(base), '--out', str(base)],
            ['run', '--trades', str(path), '--klines-dir', str(base), '--out', str(self.root/'other'), '--write-ledger', str(path)],
            ['run', '--trades', str(path), '--klines-dir', str(base), '--out', str(self.root/'other'), '--write-ledger', str(self.root/'other/results.json')],
            ['export', '--db', str(path), '--out', str(path.parent)],
        ]
        with redirect_stderr(io.StringIO()), patch.object(s, 'study') as study:
            for argv in bad_args:
                with self.assertRaises(SystemExit) as raised:
                    s.main(argv)
                self.assertEqual(raised.exception.code, 2)
            study.assert_not_called()
        self.assertEqual(path.read_bytes(), before)
        (out/'results.json').unlink()
        (out/'results.json').write_text('frozen result')
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            s.main(bad_args[0])
        self.assertEqual((out/'results.json').read_text(), 'frozen result')

    def test_fetch_manifest_cannot_overwrite_trade_input(self):
        base = self.root / 'cache'
        base.mkdir()
        path = base/'manifest.json'
        path.write_text(json.dumps(trade())+'\n')
        before = path.read_bytes()
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            s.main(['fetch', '--trades', str(path), '--klines-dir', str(base)])
        self.assertEqual(path.read_bytes(), before)

    def test_cli_protects_atomic_temporary_paths_and_hardlinks(self):
        path, base = write_inputs(self.root, [trade()], [bars()])
        out = self.root / 'output'
        out.mkdir()
        before = path.read_bytes()
        (out/'results.json.tmp').symlink_to(path)
        argv = ['run', '--trades', str(path), '--klines-dir', str(base), '--out', str(out)]
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            s.main(argv)
        (out/'results.json.tmp').unlink()
        ledger_alias = self.root / 'ledger-alias.jsonl'
        ledger_alias.hardlink_to(path)
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            s.main([*argv, '--write-ledger', str(ledger_alias)])
        self.assertEqual(path.read_bytes(), before)


if __name__ == '__main__':
    unittest.main()
