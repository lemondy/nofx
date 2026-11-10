"""Synthetic data only. Every network entry point is forbidden by default."""
from contextlib import redirect_stdout
from copy import deepcopy
import io
import json
import math
from pathlib import Path
import random
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlparse

from scripts.backtest import crypto_carry_study as s


T0 = s.timestamp_ms('2020-01-01T00:00:00Z')


def bars(n=30, start=T0 - 7 * s.DAY, price=100.):
    return [[start + i * s.DAY, price, price * 1.01, price * .99, price, 1000.] for i in range(n + 7)]


def funding(n=30, start=T0 - 7 * s.DAY, rate=.0003, period=8 * s.HOUR):
    return [[t, rate, 100.] for t in range(start, T0 + (n + 1) * s.DAY + 1, period)]


def fixture(n=30):
    daily = bars(n)
    return dict(spot_1d=daily, perp_1d=deepcopy(daily), funding=funding(n),
                panel_close={r[0]: r[4] for r in daily})


def write_inputs(root, n=30, symbols=s.SYMBOLS):
    base, panel = root / 'carry', root / 'panel'
    manifest = dict(version=1, files={})
    for symbol in symbols:
        data = fixture(n)
        for kind in ('funding', 'spot_1d', 'perp_1d'):
            name = f'{kind}/{symbol}.json'
            s.write_json(base / name, data[kind])
            manifest['files'][name] = s.file_metadata(base / name, data[kind])
        s.write_json(panel / f'{symbol}.json', [[s.datetime.fromtimestamp(t / 1000, s.timezone.utc).date().isoformat(),
                                               *row[1:], row[4]] for t, row in ((r[0], r) for r in data['perp_1d'])])
    s.write_json(base / 'manifest.json', manifest)
    return base, panel


class SyntheticTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='crypto-carry-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ('http_json', 'fetch_binance_klines'):
            guard = patch.object(s.market_cache, name, side_effect=AssertionError('network forbidden'))
            guard.start()
            self.addCleanup(guard.stop)

    def run_sleeve(self, spot=None, perp=None, rates=None, n=10, **kwargs):
        spot = bars(n) if spot is None else spot
        perp = deepcopy(spot) if perp is None else perp
        rates = funding(n) if rates is None else rates
        return s.simulate_sleeve(spot, perp, rates, T0, T0 + n * s.DAY, **kwargs)


class Execution(SyntheticTest):
    def test_signal_window_excludes_decision_time_and_includes_seven_days_left(self):
        rates = [[T0 - 7 * s.DAY - 1, 10., 100.], [T0 - 7 * s.DAY, .01, 100.],
                 [T0 - 1, .02, 100.], [T0, 20., 100.]]
        self.assertAlmostEqual(s.signal_apr(rates, T0), .03 / 7 * 365)

    def test_signal_uses_summed_settlements_for_mixed_cycles(self):
        rates = [[t, .001, 100.] for t in range(T0 - 7 * s.DAY, T0 - 3 * s.DAY, 8 * s.HOUR)]
        rates += [[t, .001, 100.] for t in range(T0 - 3 * s.DAY, T0, 4 * s.HOUR)]
        self.assertAlmostEqual(s.signal_apr(rates, T0), len(rates) * .001 / 7 * 365)

    def test_hysteresis_exact_thresholds_and_flat_capital(self):
        aprs = [.099, .10, .05, .02, .019, .09, .10]
        with patch.object(s, 'signal_apr', side_effect=lambda _, t: aprs[(t - T0) // s.DAY]):
            result = self.run_sleeve(n=len(aprs))
        entries = [f for f in result['fills'] if f['kind'] == 'entry' and f['leg'] == 'spot']
        exits = [f for f in result['fills'] if f['kind'] == 'exit' and f['leg'] == 'spot']
        self.assertEqual([f['time_ms'] for f in entries], [T0 + s.DAY, T0 + 6 * s.DAY])
        self.assertEqual([f['time_ms'] for f in exits], [T0 + 4 * s.DAY])
        self.assertEqual(result['daily'][0]['equity'], 1.)
        self.assertEqual(result['daily'][4]['equity'], result['daily'][5]['equity'])
        # Second entry uses current sleeve equity, not initial capital or aggregate NAV.
        self.assertAlmostEqual(entries[1]['size'], .75 * result['daily'][5]['equity'] / 100)

    def test_next_open_uses_each_leg_price_and_equal_unrebalanced_quantity(self):
        spot, perp = bars(3), bars(3, price=110.)
        result = self.run_sleeve(spot, perp, n=3, always=True)
        first, second = result['fills']
        self.assertAlmostEqual(first['size'], .75 / 100)
        self.assertAlmostEqual(second['size'], -first['size'])
        self.assertAlmostEqual(first['price'], 100 * (1 + s.SLIPPAGE))
        self.assertAlmostEqual(second['price'], 110 * (1 - s.SLIPPAGE))
        self.assertTrue(all(r['quantity'] == first['size'] for r in result['daily']))
        self.assertEqual(result['exits'], 0)  # Open inventory remains marked at the end.

    def test_funding_entry_excluded_exit_included_once(self):
        rates = [[T0, .5, 100.], [T0 + 8 * s.HOUR, .001, 100.],
                 [T0 + s.DAY, .002, 100.], [T0 + s.DAY + 1, 1., 100.]]
        with patch.object(s, 'signal_apr', side_effect=lambda _, t: .11 if t == T0 else -.01):
            result = self.run_sleeve(rates=rates, n=2, zero_cost=True)
        self.assertEqual([e['time_ms'] for e in result['funding_events']], [T0 + 8 * s.HOUR, T0 + s.DAY])
        self.assertAlmostEqual(result['daily'][-1]['funding'], .75 * .003)

    def test_negative_funding_is_paid_and_mark_fallback_uses_own_day(self):
        perp = bars(2)
        perp[8] = [T0 + s.DAY, 100., 112., 99., 110., 1000.]
        rates = [[T0, -.8, None], [T0 + 8 * s.HOUR, -.01, None],
                 [T0 + s.DAY, -.02, None]]
        result = self.run_sleeve(perp=perp, rates=rates, n=2, always=True, zero_cost=True)
        self.assertEqual(result['mark_fallbacks'], 2)
        self.assertAlmostEqual(result['daily'][-1]['funding'], -.0075 * (100 * .01 + 110 * .02))
        self.assertTrue(all(e['cashflow'] < 0 for e in result['funding_events']))

    def test_missing_fallback_day_raises(self):
        rates = [[T0 + 2 * s.DAY, .01, None]]
        with self.assertRaisesRegex(ValueError, '结算当日无永续收盘价'):
            self.run_sleeve(rates=rates, n=2, always=True)

    def test_margin_breach_same_day_and_reentry_next_day(self):
        spot, perp = bars(3), bars(3)
        threshold = 100 * (1 - s.SLIPPAGE) * s.MARGIN_FACTOR
        spot[7][2] = 120.
        perp[7][2] = threshold
        result = self.run_sleeve(spot, perp, n=3, always=True)
        forced = [f for f in result['fills'] if f['reason'] == 'margin']
        self.assertEqual(result['margin_breaches'], 1)
        self.assertEqual(result['entries'], 2)
        self.assertEqual(result['exits'], 1)
        self.assertEqual(forced[0]['time_ms'], T0 + s.DAY)
        self.assertEqual(forced[0]['raw_price'], 120.)
        self.assertAlmostEqual(forced[1]['raw_price'], threshold)
        self.assertLess(forced[0]['price'], forced[0]['raw_price'])
        self.assertGreater(forced[1]['price'], forced[1]['raw_price'])
        self.assertEqual(result['daily'][0]['quantity'], 0.)
        self.assertGreater(result['daily'][1]['quantity'], 0.)

    def test_all_four_costs_and_adverse_slippage(self):
        with patch.object(s, 'signal_apr', side_effect=lambda _, t: .11 if t == T0 else 0.):
            result = self.run_sleeve(n=2)
        fills = result['fills']
        self.assertEqual(len(fills), 4)
        for f in fills:
            if f['size'] > 0:
                self.assertGreater(f['price'], f['raw_price'])
            else:
                self.assertLess(f['price'], f['raw_price'])
            fee = s.SPOT_FEE if f['leg'] == 'spot' else s.PERP_FEE
            self.assertAlmostEqual(f['commission'], abs(f['size']) * f['price'] * fee)
        expected = sum(f['commission'] + abs(f['size']) * abs(f['price'] - f['raw_price']) for f in fills)
        self.assertAlmostEqual(result['daily'][-1]['costs'], expected)
        self.assertAlmostEqual(expected, .75 * .0042)
        zero = self.run_sleeve(n=2, always=True, zero_cost=True)
        self.assertEqual(zero['daily'][-1]['costs'], 0.)

    def test_pnl_decomposition_at_every_close_and_combination(self):
        spot, perp = bars(8), bars(8, price=102.)
        for i in range(7, len(spot)):
            spot[i][4] = 100. + (i - 7) * .1
            perp[i][4] = 102. - (i - 7) * .1
        a = self.run_sleeve(spot, perp, n=8, always=True)
        b = self.run_sleeve(n=8, always=True)
        combined = s.combine_sleeves([a, b])
        for curve, capital in ((a['daily'], 1.), (combined['daily'], 2.)):
            for r in curve:
                self.assertAlmostEqual(r['equity'] - capital, r['funding'] - r['costs'] + r['basis_pnl'], places=12)
        self.assertEqual(combined['daily'][-1]['equity'], a['daily'][-1]['equity'] + b['daily'][-1]['equity'])
        self.assertGreater(a['daily'][-1]['basis_pnl'], 0)

    def test_randomized_native_oracle_equivalence(self):
        for seed in range(20):
            with self.subTest(seed=seed):
                rng = random.Random(seed)
                n = 35
                spot, perp = bars(n), bars(n)
                for i in range(7, len(spot)):
                    o = 100 * rng.uniform(.8, 1.2)
                    for leg in (spot, perp):
                        opened, close = o * rng.uniform(.995, 1.005), o * rng.uniform(.98, 1.02)
                        high = max(opened, close) * rng.uniform(1., 1.12)
                        leg[i][1:5] = [opened, high, min(opened, close) * .99, close]
                rates = funding(n, period=4 * s.HOUR)
                for row in rates:
                    row[1] = rng.uniform(-.0005, .0008)
                    row[2] = None if rng.random() < .2 and row[0] < T0 + n * s.DAY else rng.uniform(80., 120.)
                native = self.run_sleeve(spot, perp, rates, n=n, always=seed % 3 == 0, zero_cost=seed % 4 == 0)
                self.assertTrue(native['reconciliation']['valid'])
                self.assertLess(native['reconciliation']['max_scaled_error'], 1e-12)

    def test_reconciliation_rejects_dates_length_equity_and_components(self):
        reference = s.arithmetic_reference(bars(2), bars(2), funding(2), T0, T0 + 2 * s.DAY)
        self.assertFalse(s.reconcile_curves([], reference, 1.)['valid'])
        for key in ('time_ms', 'equity', 'funding', 'costs', 'basis_pnl'):
            changed = deepcopy(reference)
            changed[0][key] += 1
            self.assertFalse(s.reconcile_curves(changed, reference, 1.)['valid'])

    def test_native_oracle_mismatch_is_not_silently_accepted(self):
        ref = s.arithmetic_reference(bars(2), bars(2), funding(2), T0, T0 + 2 * s.DAY)
        ref[0]['equity'] += .001
        with patch.object(s, 'arithmetic_reference', return_value=ref):
            with self.assertRaisesRegex(ValueError, '算术参考'):
                self.run_sleeve(n=2)

    def test_allowed_missing_day_marks_forward_without_trading(self):
        spot, perp = bars(4), bars(4)
        del spot[8]  # No spot bar on day 2. Signal would exit that morning.
        with patch.object(s, 'signal_apr', side_effect=lambda _, t: .11 if t == T0 else -.01):
            result = self.run_sleeve(spot, perp, n=4)
        exits = [f for f in result['fills'] if f['kind'] == 'exit']
        self.assertEqual(exits[0]['time_ms'], T0 + 2 * s.DAY)
        self.assertEqual(len(result['daily']), 4)


class Validation(SyntheticTest):
    def test_valid_fixture(self):
        self.assertTrue(s.validate_data(fixture(), T0, T0 + 30 * s.DAY)['valid'])

    def test_missing_each_daily_source_fails(self):
        for kind in ('spot_1d', 'perp_1d'):
            data = fixture()
            del data[kind][9]
            result = s.validate_data(data, T0, T0 + 30 * s.DAY)
            self.assertFalse(result['valid'])
            self.assertTrue(any(kind in r and '缺失' in r for r in result['reasons']))

    def test_daily_missing_at_point_five_percent_is_allowed(self):
        data = fixture(200)
        del data['spot_1d'][12]
        self.assertTrue(s.validate_data(data, T0, T0 + 200 * s.DAY)['valid'])

    def test_missing_settlements_interior_and_boundaries(self):
        for location in ('middle', 'beginning', 'end'):
            with self.subTest(location=location):
                data = fixture(10)
                if location == 'middle':
                    data['funding'] = [r for r in data['funding'] if r[0] != T0 + 4 * s.DAY + 8 * s.HOUR]
                elif location == 'beginning':
                    data['funding'] = [r for r in data['funding'] if r[0] > T0]
                else:
                    data['funding'] = [r for r in data['funding'] if r[0] < T0 + 9 * s.DAY]
                result = s.validate_data(data, T0, T0 + 10 * s.DAY)
                self.assertFalse(result['valid'])
                self.assertTrue(any('结算缺失' in r for r in result['reasons']))

    def test_funding_missing_at_point_five_percent_is_allowed(self):
        rows = funding(200)
        remove = {T0 + k * s.DAY + 8 * s.HOUR for k in (5, 50, 150)}
        rows = [r for r in rows if r[0] not in remove]
        coverage = s.funding_coverage(rows, T0, T0 + 200 * s.DAY)
        self.assertEqual(coverage['missing_fraction'], .005)
        self.assertTrue(coverage['valid'])

    def test_panel_mismatch_and_absence_are_counted(self):
        for missing in (False, True):
            data = fixture()
            if missing:
                del data['panel_close'][T0 + 2 * s.DAY]
            else:
                data['panel_close'][T0 + 2 * s.DAY] = 101.
            result = s.validate_data(data, T0, T0 + 30 * s.DAY)
            self.assertFalse(result['valid'])
            self.assertEqual(result['details']['panel']['matched'], 29)

    def test_panel_one_percent_and_price_tolerance_boundaries(self):
        data = fixture(200)
        for t in range(T0, T0 + 200 * s.DAY, s.DAY):
            data['panel_close'][t] = 100 / 1.001
        data['panel_close'][T0] = 101.
        data['panel_close'][T0 + s.DAY] = 101.
        result = s.validate_data(data, T0, T0 + 200 * s.DAY)
        self.assertTrue(result['valid'])
        self.assertEqual(result['details']['panel']['matched_fraction'], .99)

    def test_cycle_switch_eight_to_four_hours_and_back(self):
        switch, switch_back, end = T0 + 10 * s.DAY, T0 + 20 * s.DAY, T0 + 30 * s.DAY
        rows = [[t, .001, None] for t in range(T0 - 7 * s.DAY, switch, 8 * s.HOUR)]
        rows += [[t, .001, None] for t in range(switch, switch_back, 4 * s.HOUR)]
        rows += [[t, .001, None] for t in range(switch_back, end + s.DAY, 8 * s.HOUR)]
        coverage = s.funding_coverage(rows, T0, end)
        self.assertTrue(coverage['valid'])
        self.assertEqual(coverage['missing'], 0)
        self.assertEqual([p['period_hours'] for p in coverage['segments']], [8, 4, 8])
        rows = [r for r in rows if r[0] != switch + 4 * s.DAY + 4 * s.HOUR]
        self.assertEqual(s.funding_coverage(rows, T0, end)['missing'], 1)

    def test_cycle_irregular_and_millisecond_jitter(self):
        rates = funding(10)
        rates[25][0] += 1
        self.assertTrue(s.funding_coverage(rates, T0, T0 + 10 * s.DAY)['valid'])
        rates[25][0] += s.HOUR
        with self.assertRaisesRegex(ValueError, '网格'):
            s.funding_coverage(rates, T0, T0 + 10 * s.DAY)
        with self.assertRaisesRegex(ValueError, '样本不足'):
            s.funding_coverage([], T0, T0 + s.DAY)
        with self.assertRaisesRegex(ValueError, '众数'):
            s.funding_coverage([[T0, .01, 100.], [T0 + s.DAY, .01, 100.]], T0, T0 + s.DAY)

    def test_short_four_hour_regime_is_not_rejected_by_surrounding_eight_hours(self):
        rows = funding(10)
        rows.append([T0 + 2 * s.DAY + 4 * s.HOUR, .01, 100.])
        rows.sort()
        coverage = s.funding_coverage(rows, T0, T0 + 10 * s.DAY)
        self.assertTrue(coverage['valid'])
        self.assertEqual(coverage['missing'], 0)
        self.assertEqual([p['period_hours'] for p in coverage['segments']], [8, 4, 8])

    def test_malformed_rows_and_inadequate_signal_history(self):
        for rows, kind in (([[T0, 1., None], [T0, 1., None]], 'funding'),
                           ([[T0, float('nan'), None]], 'funding'), ([[T0, .01, -1.]], 'funding'),
                           ([[T0 + 1, 100., 101., 99., 100., 1.]], 'spot_1d'),
                           ([[T0, 100., 99., 98., 100., 1.]], 'perp_1d')):
            with self.assertRaises(ValueError):
                s.validate_rows(rows, kind)
        data = fixture()
        data['funding'] = [r for r in data['funding'] if r[0] >= T0]
        self.assertIn('七日前', ' '.join(s.validate_data(data, T0, T0 + 30 * s.DAY)['reasons']))


class Statistics(SyntheticTest):
    def test_all_verdict_branches_and_each_pass_condition(self):
        stats = dict(annual_return=.05, z=1.96, first_half_annual=.001,
                     second_half_annual=.001, recent_annual=.001, max_dd=.10)
        self.assertEqual(s.decide_verdict(stats), 'pass')
        self.assertEqual(s.decide_verdict(stats, valid=False), 'invalid')
        self.assertEqual(s.decide_verdict(None), 'invalid')
        for key, value in (('annual_return', .049), ('z', 1.959), ('z', None),
                           ('recent_annual', 0.), ('max_dd', .100001)):
            self.assertEqual(s.decide_verdict(dict(stats, **{key: value})), 'weak')
        for key in ('annual_return', 'first_half_annual', 'second_half_annual'):
            self.assertEqual(s.decide_verdict(dict(stats, **{key: 0.})), 'fail')

    def test_daily_stats_odd_halves_utc_weeks_and_recent_dates(self):
        first = s.RECENT_START - 2 * s.DAY
        returns = [.001 * (i + 1) for i in range(5)]
        curve, value = [], 1.
        for i, r in enumerate(returns):
            value *= 1 + r
            curve.append(dict(time_ms=first + i * s.DAY, equity=value))
        result = s.daily_metrics(curve, 1.)
        self.assertAlmostEqual(result['annual_return'], .003 * 365)
        self.assertAlmostEqual(result['first_half_annual'], .0015 * 365)
        self.assertAlmostEqual(result['second_half_annual'], .004 * 365)
        self.assertAlmostEqual(result['recent_annual'], .004 * 365)
        expected = s.weekly_statistics([(s.datetime.fromtimestamp(r['time_ms'] / 1000, s.timezone.utc), v)
                                        for r, v in zip(curve, returns)])
        self.assertEqual(result['n_weeks'], expected['n_weeks'])
        self.assertIsNone(result['z'])
        self.assertAlmostEqual(result['annual_volatility'], s.statistics.stdev(returns) * math.sqrt(365))

    def test_recent_year_includes_both_frozen_endpoints(self):
        times = [s.RECENT_START - s.DAY, s.RECENT_START, s.UNTIL - s.DAY, s.UNTIL]
        curve, value = [], 1.
        for t, r in zip(times, (.5, .01, .03, -.5)):
            value *= 1 + r
            curve.append(dict(time_ms=t, equity=value))
        self.assertAlmostEqual(s.daily_metrics(curve, 1.)['recent_annual'], .02 * 365)

    def test_z_uses_utc_monday_clustered_daily_samples(self):
        curve, values, equity = [], [], 1.
        for i in range(31):
            value = (i % 5 - 1) * .001
            equity *= 1 + value
            t = T0 + i * s.DAY
            curve.append(dict(time_ms=t, equity=equity))
            values.append((s.datetime.fromtimestamp(t / 1000, s.timezone.utc), value))
        expected = s.weekly_statistics(values)
        stats = s.daily_metrics(curve, 1.)
        self.assertAlmostEqual(stats['se'], expected['se'])
        self.assertAlmostEqual(stats['z'], expected['avg_r'] / expected['se'])

    def test_zero_standard_error_and_max_drawdown(self):
        curve = [dict(time_ms=T0 + i * s.DAY, equity=1.) for i in range(22)]
        stats = s.daily_metrics(curve, 1.)
        self.assertEqual(stats['z'], 0.)
        self.assertEqual(stats['max_dd'], 0.)
        curve[-1]['equity'] = .8
        self.assertAlmostEqual(s.daily_metrics(curve, 1.)['max_dd'], .2)
        with patch.object(s, 'weekly_statistics', return_value=dict(avg_r=.01, se=0., n_weeks=4)):
            self.assertEqual(s.daily_metrics(curve, 1.)['z'], math.inf)

    def test_descriptive_years_and_negative_funding_losses(self):
        sleeve = dict(initial_capital=1., daily=[dict(time_ms=T0 + i * s.DAY, equity=v)
                                               for i, v in enumerate((1., .98, .97, .99))],
                      funding_events=[dict(cashflow=-.01), dict(cashflow=.005)])
        rates = [[T0, .01, 100.], [T0 + s.DAY, -.01, 100.], [T0 + 2 * s.DAY, -.01, 100.],
                 [T0 + 3 * s.DAY, .01, 100.]]
        loss = s.negative_funding_loss(sleeve, rates, T0 + 4 * s.DAY)
        self.assertAlmostEqual(loss['max_loss'], .03)
        self.assertEqual(loss['negative_funding_paid'], .01)
        self.assertEqual(list(s.yearly_returns(sleeve['daily'], 1.)), ['2020'])


class Fetch(SyntheticTest):
    def fake_fetch(self, histories):
        calls = []

        def http(url, proxy):
            self.assertEqual(proxy, s.market_cache.DEFAULT_PROXY)
            q = parse_qs(urlparse(url).query)
            symbol, start, end, limit = q['symbol'][0], int(q['startTime'][0]), int(q['endTime'][0]), int(q['limit'][0])
            kind = 'funding' if 'fundingRate' in url else 'spot_1d'
            calls.append((kind, start, end, limit))
            rows = [r for r in histories[kind][symbol] if start <= r[0] <= end][:limit]
            if kind == 'funding':
                return [dict(fundingTime=r[0], fundingRate=str(r[1]), markPrice=r[2]) for r in rows]
            return rows

        def perp(symbol, interval, start, end, proxy, page_limit):
            self.assertEqual((interval, proxy, page_limit), ('1d', s.market_cache.DEFAULT_PROXY, 1500))
            calls.append(('perp_1d', start, end, page_limit))
            # Same semantics as market_cache, which may return rows beyond end.
            return [r + [999.] for r in histories['perp_1d'][symbol] if r[0] >= start][:page_limit]
        return http, perp, calls

    def test_incremental_merge_holes_and_prefix_no_network(self):
        histories = {kind: {'BTCUSDT': fixture(10)[kind]} for kind in ('funding', 'spot_1d', 'perp_1d')}
        http, perp, calls = self.fake_fetch(histories)
        sleeps = []
        base = self.root / 'carry'
        # Seed a partial legacy cache with an interior hole and a historical value
        # different from upstream. Cached values must win, even on overlapping pages.
        cached = deepcopy(histories['spot_1d']['BTCUSDT'][3:9])
        cached[0][5] = 123456.
        del cached[2]
        s.write_json(base / 'spot_1d/BTCUSDT.json', cached)
        unrelated = base / 'keep.txt'
        unrelated.write_text('keep')
        with patch.object(s, 'SYMBOLS', ('BTCUSDT',)):
            manifest = s.fetch_data(base, http=http, fetch_perp=perp, sleeper=sleeps.append, now_ms=T0 + 10 * s.DAY)
        rows = json.loads((base / 'spot_1d/BTCUSDT.json').read_bytes())
        self.assertEqual(len(rows), 17)
        self.assertEqual(rows[3][5], 123456.)
        self.assertEqual(unrelated.read_text(), 'keep')
        self.assertEqual(len(manifest['files']), 3)
        self.assertEqual(len(sleeps), len(calls) - 1)
        self.assertTrue(all(pause >= .3 for pause in sleeps))
        self.assertEqual(manifest['files']['spot_1d/BTCUSDT.json']['sha256'], s.sha256((base / 'spot_1d/BTCUSDT.json').read_bytes()))
        before = (base / 'spot_1d/BTCUSDT.json').read_bytes()
        calls.clear()
        with patch.object(s, 'SYMBOLS', ('BTCUSDT',)):
            s.fetch_data(base, http=http, fetch_perp=perp, sleeper=lambda _: None, now_ms=T0 + 11 * s.DAY)
        self.assertEqual((base / 'spot_1d/BTCUSDT.json').read_bytes(), before)
        self.assertTrue(all(call[1] != 0 for call in calls))

    def test_funding_paginates_start_time_and_four_hour_hole(self):
        n = 350
        histories = {kind: {'BTCUSDT': fixture(n)[kind]} for kind in ('funding', 'spot_1d', 'perp_1d')}
        histories['funding']['BTCUSDT'] = funding(n, period=4 * s.HOUR)
        http, perp, calls = self.fake_fetch(histories)
        base = self.root / 'carry'
        with patch.object(s, 'SYMBOLS', ('BTCUSDT',)):
            s.fetch_data(base, http=http, fetch_perp=perp, sleeper=lambda _: None, now_ms=T0 + n * s.DAY)
        pages = [call for call in calls if call[0] == 'funding']
        self.assertGreaterEqual(len(pages), 3)
        expected = histories['funding']['BTCUSDT'][999][0] + 1
        self.assertEqual(pages[1][1], expected)
        self.assertTrue(all(call[3] == 1000 for call in pages))
        path = base / 'funding/BTCUSDT.json'
        rows = json.loads(path.read_bytes())
        del rows[100]
        s.write_json(path, rows)
        with patch.object(s, 'SYMBOLS', ('BTCUSDT',)):
            s.fetch_data(base, http=http, fetch_perp=perp, sleeper=lambda _: None, now_ms=T0 + n * s.DAY)
        self.assertIn(histories['funding']['BTCUSDT'][100], json.loads(path.read_bytes()))

    def test_funding_never_sends_start_zero_and_refills_cached_tail_prefix(self):
        # Binance treats startTime=0 as absent and returns only the latest 500 rows.
        n = 400
        histories = {kind: {'BTCUSDT': fixture(n)[kind]} for kind in ('funding', 'spot_1d', 'perp_1d')}
        http, perp, calls = self.fake_fetch(histories)

        def binance_like(url, proxy):
            if 'fundingRate' in url and parse_qs(urlparse(url).query)['startTime'][0] == '0':
                return [dict(fundingTime=r[0], fundingRate=str(r[1]), markPrice=r[2])
                        for r in histories['funding']['BTCUSDT'][-500:]]
            return http(url, proxy)

        base = self.root / 'carry'
        path = base / 'funding/BTCUSDT.json'
        s.write_json(path, histories['funding']['BTCUSDT'][-500:])  # the truncated tail seen live
        with patch.object(s, 'SYMBOLS', ('BTCUSDT',)):
            s.fetch_data(base, http=binance_like, fetch_perp=perp, sleeper=lambda _: None, now_ms=T0 + n * s.DAY)
        funding_calls = [call for call in calls if call[0] == 'funding']
        self.assertTrue(funding_calls and all(call[1] >= s.FUNDING_START for call in funding_calls))
        self.assertEqual(json.loads(path.read_bytes()), histories['funding']['BTCUSDT'])

    def test_invalid_sleep_and_nonadvancing_page(self):
        with self.assertRaises(ValueError):
            s.fetch_data(self.root, sleep=.29)
        def stuck(url, **kwargs):
            return [dict(fundingTime=0, fundingRate='.01', markPrice=None)] * 1000
        with patch.object(s, 'SYMBOLS', ('BTCUSDT',)):
            with self.assertRaises(ValueError):
                s.fetch_data(self.root, http=stuck, sleeper=lambda _: None, now_ms=T0)

    def test_first_perp_fetch_covers_listing_with_strict_end_fetcher(self):
        histories = {kind: {'BTCUSDT': fixture(10)[kind]} for kind in ('funding', 'spot_1d', 'perp_1d')}
        http, _, _ = self.fake_fetch(histories)
        def strict_perp(symbol, interval, start, end, **kwargs):
            self.assertEqual((start, end), (0, T0 + 10 * s.DAY))
            return [r for r in histories['perp_1d'][symbol] if start <= r[0] < end]
        with patch.object(s, 'SYMBOLS', ('BTCUSDT',)):
            manifest = s.fetch_data(self.root, http=http, fetch_perp=strict_perp,
                                    sleeper=lambda _: None, now_ms=T0 + 10 * s.DAY)
        self.assertEqual(manifest['files']['perp_1d/BTCUSDT.json']['rows'], 17)


class Integration(SyntheticTest):
    def test_verify_prereg_real_hash_and_modified_file(self):
        self.assertEqual(s.sha256(s.verify_prereg()), s.PREREG_SHA256)
        path = self.root / 'prereg.md'
        path.write_bytes(s.verify_prereg() + b'changed')
        with self.assertRaisesRegex(ValueError, 'sha256 mismatch'):
            s.verify_prereg(path)

    def test_cli_writes_results_report_all_descriptions_and_ledger(self):
        n = 30
        base, panel = write_inputs(self.root, n)
        out, ledger = self.root / 'out', self.root / 'ledger.jsonl'
        with patch.object(s, 'SINCE', T0), patch.object(s, 'UNTIL', T0 + n * s.DAY), \
                patch.object(s, 'RECENT_START', T0), redirect_stdout(io.StringIO()):
            status = s.main(['run', '--data-dir', str(base), '--panel-daily', str(panel),
                             '--out', str(out), '--write-ledger', str(ledger)])
        self.assertEqual(status, 0)
        result = json.loads((out / 'results.json').read_bytes())
        self.assertNotEqual(result['verdict'], 'invalid')
        self.assertEqual(len(result['descriptions']['sensitivity']), 5)
        self.assertEqual(set(result['descriptions']['secondary']), set(s.SECONDARY))
        self.assertIn('always_hold', result['descriptions'])
        self.assertIn('zero_cost', result['descriptions'])
        self.assertIn('资金费', (out / 'report.md').read_text())
        self.assertEqual(result['ledger']['K'], 1)
        self.assertEqual(result['sleeve_runs_attempted'], 18)
        self.assertTrue(all(d['verdict'] == 'descriptive_only' for d in result['descriptions']['secondary'].values()))
        for key in ('annual_return', 'z', 'first_half_annual', 'second_half_annual', 'recent_annual', 'max_dd'):
            self.assertIn(key, result['ledger'])

    def test_invalid_missing_inputs_still_write_report(self):
        out = self.root / 'out'
        with redirect_stdout(io.StringIO()):
            code = s.main(['run', '--data-dir', str(self.root / 'absent'), '--panel-daily', str(self.root / 'panel'), '--out', str(out)])
        self.assertEqual(code, 1)
        self.assertEqual(json.loads((out / 'results.json').read_bytes())['verdict'], 'invalid')
        self.assertIn('manifest', (out / 'report.md').read_text())

    def test_manifest_tampering_and_bad_prereg_stop_before_profit(self):
        base, panel = write_inputs(self.root)
        path = base / 'perp_1d/BTCUSDT.json'
        path.write_bytes(path.read_bytes() + b' ')
        with patch.object(s, 'simulate_sleeve', side_effect=AssertionError('profit forbidden')):
            result = s.study(base, panel)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIn('manifest', ' '.join(result['invalid_reasons']))
        with patch.object(s, 'PREREG_SHA256', 'wrong'):
            with self.assertRaises(ValueError):
                s.study(base, panel)

    def test_reference_disagreement_yields_invalid_report(self):
        base, panel = write_inputs(self.root)
        with patch.object(s, 'SINCE', T0), patch.object(s, 'UNTIL', T0 + 30 * s.DAY), \
                patch.object(s, 'simulate_sleeve', side_effect=ValueError('Backtrader 与算术参考不一致')):
            result = s.study(base, panel)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertIn('算术参考', s.report(result))

    def test_output_protection(self):
        with self.assertRaisesRegex(ValueError, '不可覆盖'):
            s.validate_output(self.root / 'carry', self.root / 'carry', self.root / 'panel', None)
        with self.assertRaises(ValueError):
            s.validate_output(self.root / 'out', self.root / 'carry', self.root / 'panel', self.root / 'out/results.json')

    def test_ledger_k_is_atomic_per_track_and_invalid_does_not_count(self):
        ledger = self.root / 'ledger.jsonl'
        result = dict(verdict='invalid', statistics=None, inputs={}, trials=0)
        invalid = s.write_ledger(result, ledger)
        self.assertIsNone(invalid['K'])
        result['verdict'] = 'weak'
        event = s.write_ledger(result, ledger)
        self.assertEqual(event['K'], 1)
        self.assertEqual((event['track'], event['stage'], event['change_type'], event['idea_id']),
                         ('crypto_carry', 'S1', 'E', 'crypto-funding-carry-v1'))
        before = ledger.read_bytes()
        with self.assertRaisesRegex(ValueError, '实际账本 K=2'):
            s.write_ledger(result, ledger)
        self.assertEqual(before, ledger.read_bytes())


if __name__ == '__main__':
    unittest.main()
