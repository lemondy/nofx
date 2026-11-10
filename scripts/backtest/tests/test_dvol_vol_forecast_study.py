"""Synthetic fixtures only. Real HTTP is forbidden in every test."""
from contextlib import redirect_stdout
from copy import deepcopy
import io
import json
import math
from pathlib import Path
import statistics
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlparse

import numpy as np

from scripts.backtest import dvol_vol_forecast_study as s


T0 = s.timestamp_ms('2022-01-01')


def synthetic_bars(start=T0, n=100):
    dvol, perp = [], []
    previous = 100.
    for i in range(n):
        t = start + i * s.DAY
        close = previous * math.exp(.008 * math.sin(i * .73) + .003 * math.cos(i * .17))
        spread = .01 + .004 * (1 + math.sin(i * .31))
        perp.append([t, previous, max(previous, close) * math.exp(spread),
                     min(previous, close) * math.exp(-spread), close, 1000.])
        iv = 60 + 12 * math.sin(i * .13) + 5 * math.cos(i * .37)
        dvol.append([t, iv, iv + 1, iv - 1, iv])
        previous = close
    return dict(dvol=dvol, perp_1d=perp)


def write_inputs(root):
    dvol_dir, carry_dir = root / 'dvol', root / 'carry'
    manifests = {name: dict(version=1, files={}) for name in ('dvol', 'carry')}
    data = synthetic_bars(s.FETCH_START, (s.VALID_END - s.FETCH_START) // s.DAY)
    for currency, symbol in s.SYMBOLS.items():
        for kind, base, name, key in (
            ('dvol', dvol_dir, f'{currency}.json', 'dvol'),
            ('carry', carry_dir, f'perp_1d/{symbol}.json', 'perp_1d'),
        ):
            s.write_json(base / name, data[key])
            manifests[kind]['files'][name] = s.file_metadata(base / name, data[key])
    s.write_json(dvol_dir / 'manifest.json', manifests['dvol'])
    s.write_json(carry_dir / 'manifest.json', manifests['carry'])
    return dvol_dir, carry_dir


def forecast_rows(n=20, start=T0):
    return [dict(time_ms=start + i * s.DAY, log_y=float(i % 3), y=math.exp(i % 3),
                 train_mean_log_y=.2 + .05 * i,
                 predictions=dict(B=float(i % 3) - .5 - .01 * i,
                                  C=float(i % 3) - .2)) for i in range(n)]


class SyntheticTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='dvol-synthetic-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ('http_json', 'fetch_binance_klines'):
            guard = patch.object(s.market_cache, name, side_effect=AssertionError('network forbidden'))
            guard.start()
            self.addCleanup(guard.stop)
        ledger_guard = patch('scripts.backtest.evolution_ledger.append_event',
                             side_effect=AssertionError('ledger writes forbidden'))
        ledger_guard.start()
        self.addCleanup(ledger_guard.stop)


class Variables(SyntheticTest):
    def test_parkinson_formula_and_annualization(self):
        expected = math.log(110 / 90) ** 2 / (4 * math.log(2)) * 365
        self.assertAlmostEqual(s.parkinson_variance(110., 90.), expected, places=13)
        self.assertIsNone(s.parkinson_variance(0., 90.))
        self.assertIsNone(s.parkinson_variance(110., None))
        self.assertEqual(s.parkinson_variance(100., 100.), 0.)

    def test_rv_windows_include_current_day_and_exact_calendar_edges(self):
        data = synthetic_bars()
        day = T0 + 40 * s.DAY
        result = s.build_samples(data['dvol'], data['perp_1d'], day, day + s.DAY)
        row = result['samples'][0]
        p = [s.parkinson_variance(r[2], r[3]) for r in data['perp_1d']]
        self.assertAlmostEqual(row['rv1'], p[40])
        self.assertAlmostEqual(row['rv7'], statistics.mean(p[34:41]))
        self.assertAlmostEqual(row['rv30'], statistics.mean(p[11:41]))
        self.assertAlmostEqual(row['iv'], (data['dvol'][40][4] / 100) ** 2)
        for index in (10, 41):  # Both just outside the past 30 calendar days.
            changed = deepcopy(data)
            changed['perp_1d'][index][2] *= 2
            actual = s.build_samples(changed['dvol'], changed['perp_1d'], day, day + s.DAY)['samples'][0]
            self.assertEqual(actual['rv30'], row['rv30'])
        changed = deepcopy(data)
        changed['perp_1d'][11][2] *= 2
        self.assertNotEqual(s.build_samples(changed['dvol'], changed['perp_1d'], day, day + s.DAY)['samples'][0]['rv30'], row['rv30'])

    def test_target_uses_only_returns_t_plus_one_through_seven(self):
        returns = [.7, .01, -.02, .03, -.04, .05, -.06, .07, .9]
        closes = {T0 - s.DAY: 100.}
        for i, value in enumerate(returns):
            closes[T0 + i * s.DAY] = closes[T0 + (i - 1) * s.DAY] * math.exp(value)
        outcome = s.future_outcomes(closes, T0)
        self.assertAlmostEqual(outcome['y'], 365 / 7 * sum(r * r for r in returns[1:8]))
        self.assertAlmostEqual(outcome['return_7d'], sum(returns[1:8]))
        closes[T0 - s.DAY] *= 100
        closes[T0 + 8 * s.DAY] *= 100
        self.assertEqual(s.future_outcomes(closes, T0), outcome)
        del closes[T0 + 7 * s.DAY]
        self.assertIsNone(s.future_outcomes(closes, T0))

    def test_maximum_drawdown_uses_close_path_and_initial_close(self):
        closes = {T0 + i * s.DAY: p for i, p in enumerate([100., 80., 120., 90., 108., 110., 109., 115.])}
        self.assertAlmostEqual(s.future_outcomes(closes, T0)['max_drawdown_7d'], .25)

    def test_incomplete_and_nonpositive_variables_are_excluded_and_counted(self):
        day = T0 + 40 * s.DAY
        for case, reason in (('rv_missing', 'missing_rv_window'), ('rv_zero', 'nonpositive_rv_component'),
                             ('iv_missing', 'missing_iv'), ('iv_zero', 'nonpositive_iv'),
                             ('target_missing', 'missing_target_window'), ('target_zero', 'nonpositive_target')):
            with self.subTest(case=case):
                data = synthetic_bars()
                if case == 'rv_missing':
                    del data['perp_1d'][20]
                elif case == 'rv_zero':
                    data['perp_1d'][20][2:4] = [100., 100.]
                elif case == 'iv_missing':
                    del data['dvol'][40]
                elif case == 'iv_zero':
                    data['dvol'][40][4] = 0.
                elif case == 'target_missing':
                    del data['perp_1d'][43]
                else:
                    for i in range(40, 48):
                        data['perp_1d'][i][4] = 100.
                result = s.build_samples(data['dvol'], data['perp_1d'], day, day + s.DAY)
                self.assertEqual(result['samples'], [])
                self.assertEqual(result['exclusions'], {reason: 1})
                self.assertEqual(result['excluded_days'], [dict(time_ms=day, reason=reason)])


class Estimation(SyntheticTest):
    def test_ols_recovers_true_parameters(self):
        rng = np.random.default_rng(20261110)
        x = np.column_stack([np.ones(100), rng.normal(size=(100, 4))])
        beta = np.array([.2, -.3, .7, .1, -.4])
        fit = s.fit_ols(x, x @ beta)
        np.testing.assert_allclose(fit['coefficients'], beta, atol=1e-12)
        self.assertEqual(fit['rank'], 5)
        self.assertEqual(fit['n'], 100)

    def test_monthly_cutoff_has_no_future_prices_and_fixed_monthly_coefficients(self):
        month = s.timestamp_ms('2023-01-01')
        data = synthetic_bars(s.TRAIN_START - 30 * s.DAY, 760)
        prepared = s.build_samples(data['dvol'], data['perp_1d'])['samples']
        initial = s.monthly_forecasts(prepared, month, month + 31 * s.DAY)
        fit = initial['fits'][0]
        self.assertEqual(fit['train_last_ms'], month - 8 * s.DAY)
        self.assertEqual(fit['max_target_day_ms'], month - s.DAY)
        self.assertEqual(fit['target_available_at_ms'], month)
        self.assertEqual(len(initial['fits']), 1)
        self.assertEqual({r['fit_month_ms'] for r in initial['forecasts']}, {month})
        self.assertEqual({r['train_mean_log_y'] for r in initial['forecasts']}, {fit['train_mean_log_y']})
        changed = deepcopy(data)
        for row in changed['perp_1d']:
            if row[0] >= month:
                # An m-day price change would alter the illegally included
                # training target at t=m-7. Earlier targets remain unchanged.
                row[1:5] = [v * 3 for v in row[1:5]]
        altered_samples = s.build_samples(changed['dvol'], changed['perp_1d'])['samples']
        altered = s.monthly_forecasts(altered_samples, month, month + 31 * s.DAY)
        self.assertEqual(initial['fits'], altered['fits'])
        by_time = {r['time_ms']: r for r in prepared}
        altered_by_time = {r['time_ms']: r for r in altered_samples}
        illegal_day = month - 7 * s.DAY
        self.assertNotEqual(by_time[illegal_day]['log_y'], altered_by_time[illegal_day]['log_y'])
        # Also perturb only future targets, leaving the prediction features fixed.
        targets_changed = deepcopy(prepared)
        for row in targets_changed:
            if row['time_ms'] > month - 8 * s.DAY:
                row['log_y'] += 100.
        future = s.monthly_forecasts(targets_changed, month, month + 31 * s.DAY)
        self.assertEqual(initial['fits'], future['fits'])
        self.assertEqual([r['predictions'] for r in initial['forecasts']],
                         [r['predictions'] for r in future['forecasts']])
        # The boundary sample itself must be included and affect the fit.
        boundary_changed = deepcopy(prepared)
        for row in boundary_changed:
            if row['time_ms'] == month - 8 * s.DAY:
                row['log_y'] += 100.
        boundary = s.monthly_forecasts(boundary_changed, month, month + 31 * s.DAY)
        self.assertNotEqual(initial['fits'][0]['models']['B']['coefficients'],
                            boundary['fits'][0]['models']['B']['coefficients'])

    def test_missing_first_day_does_not_move_estimation_date_and_next_month_expands(self):
        month = s.timestamp_ms('2023-01-01')
        data = synthetic_bars(s.TRAIN_START - 30 * s.DAY, 760)
        prepared = s.build_samples(data['dvol'], data['perp_1d'])['samples']
        prepared = [r for r in prepared if r['time_ms'] != month]
        result = s.monthly_forecasts(prepared, month, month + 60 * s.DAY)
        self.assertEqual(result['forecasts'][0]['time_ms'], month + s.DAY)
        self.assertEqual(result['fits'][0]['month_ms'], month)
        self.assertEqual(result['fits'][0]['train_last_ms'], month - 8 * s.DAY)
        self.assertEqual(result['fits'][1]['month_ms'], month + 31 * s.DAY)
        self.assertEqual(result['fits'][1]['train_last_ms'], month + 23 * s.DAY)
        self.assertGreater(result['fits'][1]['n'], result['fits'][0]['n'])

    def test_sensitivity_features_and_spread_reparameterization(self):
        data = synthetic_bars(s.TRAIN_START - 30 * s.DAY, 760)
        prepared = s.build_samples(data['dvol'], data['perp_1d'])['samples']
        row = prepared[0]
        self.assertEqual(s.model_features(row, 'IV_only'), [1., math.log(row['iv'])])
        self.assertAlmostEqual(s.model_features(row, 'HAR_log_spread')[-1], math.log(row['iv']) - math.log(row['rv30']))
        result = s.monthly_forecasts(prepared)
        np.testing.assert_allclose([r['predictions']['C'] for r in result['forecasts']],
                                   [r['predictions']['HAR_log_spread'] for r in result['forecasts']], atol=1e-10)


class Metrics(SyntheticTest):
    def test_r_squared_delta_and_individual_training_means(self):
        rows = forecast_rows(4)
        rows[0]['train_mean_log_y'] = 9.
        denominator = sum((r['log_y'] - r['train_mean_log_y']) ** 2 for r in rows)
        b = sum((r['log_y'] - r['predictions']['B']) ** 2 for r in rows)
        c = sum((r['log_y'] - r['predictions']['C']) ** 2 for r in rows)
        result = s.forecast_metrics(rows)
        self.assertAlmostEqual(result['denominator'], denominator)
        self.assertAlmostEqual(result['r2_b'], 1 - b / denominator)
        self.assertAlmostEqual(result['r2_c'], 1 - c / denominator)
        self.assertAlmostEqual(result['delta_r2'], (b - c) / denominator)
        expected = statistics.mean(r['y'] / math.exp(r['predictions']['C']) -
                                   math.log(r['y'] / math.exp(r['predictions']['C'])) - 1 for r in rows)
        self.assertAlmostEqual(result['qlike_c'], expected)
        self.assertAlmostEqual(result['delta_qlike'], result['qlike_c'] - result['qlike_b'])

    def test_nonoverlap_uses_calendar_grid_from_first_effective_day_and_ordinary_t(self):
        rows = forecast_rows(31, T0 + 2 * s.DAY)
        rows = [r for i, r in enumerate(rows) if i not in (2, 7)]
        result = s.nonoverlapping_loss_test(rows)
        selected = [rows[i] for i in (0, 12, 19, 26)]  # Calendar offsets 0,14,21,28.
        self.assertEqual(result['dates_ms'], [r['time_ms'] for r in selected])
        differences = [(r['log_y'] - r['predictions']['B']) ** 2 -
                       (r['log_y'] - r['predictions']['C']) ** 2 for r in selected]
        expected_t = statistics.mean(differences) / (statistics.stdev(differences) / math.sqrt(4))
        self.assertAlmostEqual(result['t'], expected_t)
        self.assertEqual(result['n'], 4)

    def test_t_and_r_squared_degenerate_cases(self):
        self.assertIsNone(s.nonoverlapping_loss_test([])['t'])
        rows = forecast_rows(15)
        for row in rows:
            row['predictions'] = dict(B=row['log_y'] - 1, C=row['log_y'])
            row['train_mean_log_y'] = row['log_y']
        self.assertEqual(s.nonoverlapping_loss_test(rows)['t'], math.inf)
        self.assertIsNone(s.forecast_metrics(rows)['delta_r2'])
        for row in rows:
            row['predictions']['B'] = row['predictions']['C']
        self.assertEqual(s.nonoverlapping_loss_test(rows)['t'], 0.)

    def test_halves_use_effective_day_count_and_own_denominators(self):
        rows = forecast_rows(5)
        rows[-1]['time_ms'] += 100 * s.DAY
        first, second = s.split_halves(rows)
        self.assertEqual((len(first), len(second)), (2, 3))
        result = s.evaluate_forecasts(rows)
        self.assertEqual(result['first_half'], s.forecast_metrics(rows[:2]))
        self.assertEqual(result['second_half'], s.forecast_metrics(rows[2:]))
        self.assertEqual(tuple(map(len, s.split_halves(rows[:4]))), (2, 2))

    def test_four_verdict_branches_and_exact_thresholds(self):
        stats = {currency: dict(delta_r2=.02, nonoverlap=dict(t=2.),
                               first_half=dict(delta_r2=.001), second_half=dict(delta_r2=.001))
                 for currency in s.SYMBOLS}
        self.assertEqual(s.decide_verdict(stats), 'pass')
        self.assertEqual(s.decide_verdict(stats, valid=False), 'invalid')
        for changed in ('delta', 't', 'half'):
            value = deepcopy(stats)
            if changed == 'delta':
                value['ETH']['delta_r2'] = .019999
            elif changed == 't':
                value['ETH']['nonoverlap']['t'] = 1.99999
            else:
                value['ETH']['second_half']['delta_r2'] = 0.
            self.assertEqual(s.decide_verdict(value), 'weak')
        for changed in ('delta_r2', 't'):
            value = deepcopy(stats)
            if changed == 'delta_r2':
                value['ETH']['delta_r2'] = 0.
            else:
                value['ETH']['nonoverlap']['t'] = 0.
            self.assertEqual(s.decide_verdict(value), 'fail')
        self.assertEqual(s.decide_verdict({}), 'fail')

    def test_descriptive_vrp_years_quintiles_and_leverage_correlation(self):
        data = synthetic_bars(T0 - 40 * s.DAY, 100)
        prepared = s.build_samples(data['dvol'], data['perp_1d'], T0, T0 + 40 * s.DAY)['samples']
        forecasts = forecast_rows(len(prepared))
        for row in forecasts:
            row['predictions'].update(IV_only=row['log_y'], HAR_log_spread=row['log_y'])
        result = s.descriptive_statistics(prepared, forecasts, data['dvol'], data['perp_1d'])
        vrp = [r['iv'] - r['y'] for r in prepared]
        self.assertAlmostEqual(result['vrp']['mean'], statistics.mean(vrp))
        self.assertAlmostEqual(result['vrp']['median'], statistics.median(vrp))
        self.assertEqual(result['vrp']['positive_fraction'], sum(v > 0 for v in vrp) / len(vrp))
        self.assertEqual(result['vrp_yearly']['2022']['n'], len(prepared))
        self.assertEqual(set(result['sensitivity']), {'IV_only', 'HAR_log_spread'})
        for groups in result['quintiles'].values():
            self.assertEqual(len(groups['groups']), 5)
            self.assertEqual(sum(g['n'] for g in groups['groups']), len(prepared))
        dvol = {r[0]: r[4] for r in data['dvol']}
        perp = {r[0]: r[4] for r in data['perp_1d']}
        changes = [math.log(dvol[r['time_ms']] / dvol[r['time_ms'] - s.DAY]) for r in prepared]
        returns = [math.log(perp[r['time_ms']] / perp[r['time_ms'] - s.DAY]) for r in prepared]
        self.assertAlmostEqual(result['leverage_effect']['correlation'], np.corrcoef(changes, returns)[0, 1])
        tied = s.quintile_outcomes(prepared, [1.] * len(prepared))
        self.assertEqual([g['n'] for g in tied['groups']], [0, 0, 0, 0, len(prepared)])


class ValidationAndCLI(SyntheticTest):
    def test_verify_prereg_matches_frozen_document_and_rejects_modified_copy(self):
        self.assertEqual(s.sha256(s.verify_prereg()), s.PREREG_SHA256)
        path = self.root / 'changed-prereg.md'
        path.write_bytes(s.verify_prereg() + b'\n')
        with self.assertRaisesRegex(ValueError, 'sha256 mismatch'):
            s.verify_prereg(path)

    def test_coverage_threshold_includes_both_boundaries_and_range_is_open(self):
        data = synthetic_bars(T0, 100)
        for kind in ('dvol', 'perp_1d'):
            changed = deepcopy(data)
            del changed[kind][0]
            self.assertTrue(s.validate_data(changed, T0, T0 + 100 * s.DAY)['valid'])
            del changed[kind][-1]
            validation = s.validate_data(changed, T0, T0 + 100 * s.DAY)
            self.assertFalse(validation['valid'])
            self.assertEqual(validation['details'][kind]['missing_fraction'], .02)
        for value in (5., 400., -1., math.nan):
            changed = deepcopy(data)
            changed['dvol'][1][4] = value
            self.assertFalse(s.validate_data(changed, T0, T0 + 100 * s.DAY)['valid'])

    def test_data_failures_return_invalid_before_any_fitting(self):
        for problem in ('missing_dvol', 'missing_perp', 'range', 'dvol_sha', 'carry_sha', 'metadata', 'bad_manifest'):
            with self.subTest(problem=problem):
                dvol, carry = write_inputs(self.root / problem)
                base = carry if problem in ('missing_perp', 'carry_sha') else dvol
                name = 'perp_1d/BTCUSDT.json' if base == carry else 'BTC.json'
                path = base / name
                rows = json.loads(path.read_bytes())
                if problem.startswith('missing'):
                    rows = rows[30:]
                elif problem == 'range':
                    rows[100][4] = 400.
                elif problem.endswith('sha'):
                    rows[100][4] += .0001
                s.write_json(path, rows)
                manifest = json.loads((base / 'manifest.json').read_bytes())
                if not problem.endswith('sha'):
                    manifest['files'][name] = s.file_metadata(path, rows)
                if problem == 'metadata':
                    manifest['files'][name]['rows'] += 1
                elif problem == 'bad_manifest':
                    manifest = []
                s.write_json(base / 'manifest.json', manifest)
                with patch.object(s, 'fit_ols', side_effect=AssertionError('invalid input was fitted')):
                    result = s.study(dvol, carry)
                self.assertEqual(result['verdict'], 'invalid')
                self.assertEqual(result['trials'], 0)
                self.assertTrue(result['invalid_reasons'])
                self.assertIn('invalid', s.report(result))

    def test_valid_synthetic_run_writes_reports_and_keeps_inputs_unchanged(self):
        dvol, carry = write_inputs(self.root)
        before = {str(p): p.read_bytes() for base in (dvol, carry) for p in base.rglob('*.json')}
        out = self.root / 'out'
        with patch.object(s, 'verify_prereg', wraps=s.verify_prereg) as verify:
            with redirect_stdout(io.StringIO()) as stdout:
                code = s.main(['run', '--dvol-dir', str(dvol), '--carry-dir', str(carry), '--out', str(out)])
            self.assertEqual(verify.call_count, 2)
        self.assertEqual(code, 0)
        self.assertIn('VERDICT ', stdout.getvalue())
        result = json.loads((out / 'results.json').read_bytes())
        self.assertFalse(result['counts_toward_k'])
        self.assertEqual(result['trials'], 1)
        self.assertEqual(set(result['statistics']), {'BTC', 'ETH'})
        self.assertEqual(result['symbols']['BTC']['forecasts'][0]['time_ms'], s.OOS_START)
        self.assertEqual(result['symbols']['BTC']['forecasts'][-1]['time_ms'], s.OOS_END - s.DAY)
        self.assertTrue(result['symbols']['BTC']['fits'])
        text = (out / 'report.md').read_text()
        for phrase in ('主统计与两半', '仅描述', '补充约定', '不计 K', 't+7<m'):
            self.assertIn(phrase, text)
        self.assertEqual(before, {str(p): p.read_bytes() for base in (dvol, carry) for p in base.rglob('*.json')})
        self.assertEqual({p.name for p in out.iterdir()}, {'results.json', 'report.md'})

    def test_invalid_cli_still_writes_report_and_returns_one(self):
        dvol, carry = self.root / 'missing-dvol', self.root / 'missing-carry'
        out = self.root / 'out'
        with redirect_stdout(io.StringIO()):
            code = s.main(['run', '--dvol-dir', str(dvol), '--carry-dir', str(carry), '--out', str(out)])
        self.assertEqual(code, 1)
        self.assertEqual(json.loads((out / 'results.json').read_bytes())['verdict'], 'invalid')
        self.assertIn('无效原因', (out / 'report.md').read_text())

    def test_output_cannot_overwrite_input_directly_or_by_symlink_or_hardlink(self):
        dvol, carry = write_inputs(self.root)
        for out in (dvol, carry, dvol / 'subdir'):
            with self.assertRaisesRegex(ValueError, '不可覆盖'):
                s.validate_output(out, dvol, carry)
        out = self.root / 'out'
        out.mkdir()
        (out / 'results.json').symlink_to(dvol / 'BTC.json')
        with self.assertRaisesRegex(ValueError, '不可覆盖'):
            s.validate_output(out, dvol, carry)
        (out / 'results.json').unlink()
        (out / 'report.md').hardlink_to(carry / 'manifest.json')
        with self.assertRaisesRegex(ValueError, '不可覆盖'):
            s.validate_output(out, dvol, carry)
        (out / 'report.md').unlink()
        (out / 'results.json.tmp').symlink_to(dvol / 'manifest.json')
        with self.assertRaisesRegex(ValueError, '不可覆盖'):
            s.validate_output(out, dvol, carry)
        (out / 'results.json.tmp').unlink()
        s.validate_output(out, dvol, carry)


class Fetch(SyntheticTest):
    def fake_http(self, source, calls, limit=None, overlap=False, continuation_always=False):
        def request(url, proxy):
            query = parse_qs(urlparse(url).query)
            self.assertEqual(proxy, s.market_cache.DEFAULT_PROXY)
            self.assertEqual(query['resolution'], ['1D'])
            self.assertEqual(urlparse(url).path, '/api/v2/public/get_volatility_index_data')
            currency = query['currency'][0]
            left, right = int(query['start_timestamp'][0]), int(query['end_timestamp'][0])
            calls.append((currency, left, right))
            page = [r for r in source[currency] if left <= r[0] <= right]
            truncated = limit is not None and len(page) > limit
            if truncated:
                page = page[-limit:]  # Deliberately emulate backwards pagination.
            if overlap:
                page += [r for r in source[currency] if r[0] == right + 1]
            return dict(result=dict(data=list(reversed(page)),
                                    continuation=left - 1 if truncated or continuation_always else None))
        return request

    def test_fetch_cli_routes_to_offline_fetch_with_requested_sleep(self):
        base = self.root / 'download'
        source = {currency: synthetic_bars(s.FETCH_START, 2)['dvol'] for currency in s.SYMBOLS}
        calls, sleeps = [], []
        fetch = s.fetch_data

        def offline(data_dir, pause):
            return fetch(data_dir, pause, http=self.fake_http(source, calls), sleeper=sleeps.append,
                         now_ms=s.FETCH_START + 2 * s.DAY)

        with patch.object(s, 'fetch_data', side_effect=offline) as invoked:
            self.assertEqual(s.main(['fetch', '--data-dir', str(base), '--sleep', '0.8']), 0)
        invoked.assert_called_once_with(base, .8)
        self.assertEqual(len(calls), 2)
        self.assertEqual(sleeps, [.8])
        self.assertTrue((base / 'manifest.json').exists())

    def test_window_pagination_has_no_omissions_duplicates_or_unfinished_bars(self):
        n = 1803
        source = {currency: synthetic_bars(s.FETCH_START, n + 1)['dvol'] for currency in s.SYMBOLS}
        calls, sleeps = [], []
        base = self.root / 'download'
        result = s.fetch_data(base, http=self.fake_http(source, calls, overlap=True),
                              sleeper=sleeps.append, now_ms=s.FETCH_START + n * s.DAY + s.DAY // 2)
        for currency in s.SYMBOLS:
            rows = json.loads((base / f'{currency}.json').read_bytes())
            self.assertEqual(rows, source[currency][:n])
            self.assertEqual(len({r[0] for r in rows}), n)
            self.assertEqual(result['files'][f'{currency}.json']['sha256'], s.sha256((base / f'{currency}.json').read_bytes()))
            self.assertEqual(result['files'][f'{currency}.json']['rows'], n)
        self.assertEqual(len(calls), 6)
        self.assertEqual(sleeps, [.5] * (len(calls) - 1))
        self.assertTrue(all(right - left < 900 * s.DAY for _, left, right in calls))
        self.assertEqual(calls[0][1], s.FETCH_START)

    def test_continuation_truncation_subdivides_even_with_reverse_pages(self):
        n = 23
        source = {currency: synthetic_bars(s.FETCH_START, n)['dvol'] for currency in s.SYMBOLS}
        calls, sleeps = [], []
        base = self.root / 'download'
        s.fetch_data(base, http=self.fake_http(source, calls, limit=2, continuation_always=True),
                     sleeper=sleeps.append, now_ms=s.FETCH_START + n * s.DAY)
        for currency in s.SYMBOLS:
            self.assertEqual(json.loads((base / f'{currency}.json').read_bytes()), source[currency])
        self.assertEqual(len(sleeps), len(calls) - 1)

    def test_incremental_fetch_fills_prefix_interior_tail_and_preserves_old_rows_and_files(self):
        n = 15
        source = {currency: synthetic_bars(s.FETCH_START, n + 1)['dvol'] for currency in s.SYMBOLS}
        calls, sleeps = [], []
        base = self.root / 'download'
        base.mkdir()
        (base / 'keep.txt').write_text('keep')
        old = deepcopy(source['BTC'][3:7] + source['BTC'][9:12])
        old[0][4] = 99.
        s.write_json(base / 'BTC.json', old)
        s.fetch_data(base, sleep=.7, http=self.fake_http(source, calls, overlap=True),
                     sleeper=sleeps.append, now_ms=s.FETCH_START + n * s.DAY)
        rows = json.loads((base / 'BTC.json').read_bytes())
        expected = deepcopy(source['BTC'][:n])
        for row in old:
            expected[(row[0] - s.FETCH_START) // s.DAY] = row
        self.assertEqual(rows, expected)
        self.assertEqual((base / 'keep.txt').read_text(), 'keep')
        self.assertEqual(sleeps, [.7] * (len(calls) - 1))
        calls.clear()
        s.fetch_data(base, http=self.fake_http(source, calls), sleeper=sleeps.append,
                     now_ms=s.FETCH_START + (n + 1) * s.DAY)
        self.assertEqual(calls, [(c, s.FETCH_START + n * s.DAY, s.FETCH_START + (n + 1) * s.DAY - 1)
                                 for c in s.SYMBOLS])
        self.assertEqual(json.loads((base / 'BTC.json').read_bytes())[:n], expected)
        calls.clear()
        s.fetch_data(base, http=self.fake_http(source, calls), sleeper=sleeps.append,
                     now_ms=s.FETCH_START + (n + 1) * s.DAY)
        self.assertEqual(calls, [])

    def test_fetch_rejects_short_sleep_bad_response_and_unfinished_cache(self):
        for pause in (.499, math.nan, math.inf):
            with self.assertRaisesRegex(ValueError, '0.5'):
                s.fetch_data(self.root / 'download', pause)
        with self.assertRaisesRegex(ValueError, '响应无效'):
            s.fetch_data(self.root / 'download', http=lambda *a, **k: dict(error='bad'),
                         now_ms=s.FETCH_START + s.DAY)
        base = self.root / 'cached'
        s.write_json(base / 'BTC.json', synthetic_bars(s.FETCH_START, 2)['dvol'])
        with self.assertRaisesRegex(ValueError, '未结束日线'):
            s.fetch_data(base, now_ms=s.FETCH_START + s.DAY)


if __name__ == '__main__':
    unittest.main()
