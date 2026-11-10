"""Synthetic inputs only. Never read real data/ or make HTTP requests."""
from contextlib import redirect_stdout
from copy import deepcopy
import io
import json
import math
from pathlib import Path
import re
import statistics
import tempfile
import unittest
from unittest.mock import patch

import numpy as np

from scripts.backtest import dvol_sizing_study as s
from scripts.backtest import evolution_ledger, market_cache
from scripts.backtest.tests.test_dvol_vol_forecast_study import synthetic_bars


T0 = s.timestamp_ms('2022-11-01')
MONTH = s.timestamp_ms('2023-01-01')


def write_inputs(root, n=100):
    dvol_dir, carry_dir, trades = root / 'dvol', root / 'carry', root / 'export/trades.jsonl'
    manifests = {k: dict(files={}) for k in ('dvol', 'carry')}
    bars = synthetic_bars(T0, n)
    for coin in ('BTC', 'ETH'):
        name = coin + '.json'
        s.write_json(dvol_dir / name, bars['dvol'])
        manifests['dvol']['files'][name] = s.file_metadata(dvol_dir / name, bars['dvol'])
    for symbol in s.SYMBOLS.values():
        name = f'perp_1d/{symbol}.json'
        s.write_json(carry_dir / name, bars['perp_1d'])
        manifests['carry']['files'][name] = s.file_metadata(carry_dir / name, bars['perp_1d'])
    for kind, base in (('dvol', dvol_dir), ('carry', carry_dir)):
        s.write_json(base / 'manifest.json', manifests[kind])
    trades.parent.mkdir()
    trade = dict(id=1, side='LONG', entry_price=100., exit_price=110., initial_stop_loss=95.,
                 entry_time=MONTH + 10 * s.DAY)
    trades.write_text(json.dumps(trade) + '\n', encoding='utf-8')
    paths = s.input_paths(dvol_dir, carry_dir, trades)
    hashes = {name: s.sha256(path.read_bytes()) for name, path in paths.items()}
    return dvol_dir, carry_dir, trades, hashes


def risk_rows(n, start=MONTH):
    rows = []
    for i in range(n):
        models = {}
        for j, model in enumerate(s.MODELS):
            k = 1. if model == 'U' else .35 + .1 * ((i + j) % 5)
            r = (.005 + .001 * i) * (-1 if i % 3 == 0 else 1)
            models[model] = dict(k=k, x=k * r)
        rows.append(dict(time_ms=start + i * s.DAY, models=models))
    return rows


def passing_stats():
    return dict(carriers={c: dict(
        weekly=dict(comparisons=dict(B_minus_C=dict(t=2., first_half_mean=.1, second_half_mean=.2))),
        models={m: dict(es=v) for m, v in (('U', .3), ('B', .2), ('C', .1))},
    ) for c in ('BTC', 'ETH', 'ALT4')}, qlike={c: dict(B=1., C=.9) for c in s.SYMBOLS})


class SyntheticTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='dvol-sizing-synthetic-')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        for name in ('http_json', 'fetch_binance_klines'):
            guard = patch.object(market_cache, name, side_effect=AssertionError('network forbidden'))
            guard.start()
            self.addCleanup(guard.stop)

    def small_calendar(self):
        for name, value in dict(TRAIN_START=T0 + 30 * s.DAY, VALID_START=T0, VALID_END=T0 + 100 * s.DAY,
                                OOS_START=MONTH, OOS_END=MONTH + 31 * s.DAY).items():
            guard = patch.object(s, name, value)
            guard.start()
            self.addCleanup(guard.stop)


class InputsAndCLI(SyntheticTest):
    def test_frozen_constants_and_prereg_hash(self):
        raw = s.verify_prereg()
        recorded = dict(re.findall(r'\| `(data/[^`]+)`[^|]*\| `([0-9a-f]{64})`', raw.decode()))
        self.assertEqual(s.INPUT_SHA256, recorded)
        self.assertEqual(len(recorded), 9)
        path = self.root / 'prereg.md'
        path.write_bytes(raw + b'\n')
        with self.assertRaisesRegex(ValueError, 'sha256 mismatch'):
            s.verify_prereg(path)

    def test_each_frozen_input_hash_mismatch_refuses_run(self):
        dvol, carry, trades, hashes = write_inputs(self.root)
        paths = s.input_paths(dvol, carry, trades)
        with patch.object(s, 'INPUT_SHA256', hashes):
            self.assertEqual(s.verify_input_hashes(paths), hashes)
            for name in hashes:
                with self.subTest(name=name):
                    changed = dict(hashes, **{name: '0' * 64})
                    with patch.object(s, 'INPUT_SHA256', changed), patch.object(s, 'build_samples') as build:
                        with self.assertRaisesRegex(ValueError, 'frozen sha256 mismatch'):
                            s.study(dvol, carry, trades)
                        build.assert_not_called()

    def test_manifest_sha_rows_and_intervals_for_all_eight_files(self):
        dvol, carry, trades, hashes = write_inputs(self.root)
        for base, names in ((dvol, ['BTC.json', 'ETH.json']),
                            (carry, [f'perp_1d/{v}.json' for v in s.SYMBOLS.values()])):
            manifest_path = base / 'manifest.json'
            original = json.loads(manifest_path.read_bytes())
            for name in names:
                for key in ('sha256', 'rows', 'start_ms', 'end_ms'):
                    with self.subTest(name=name, key=key):
                        changed = deepcopy(original)
                        changed['files'][name][key] = 'bad'
                        s.write_json(manifest_path, changed)
                        with self.assertRaisesRegex(ValueError, 'manifest'):
                            s.load_inputs(dvol, carry)
            s.write_json(manifest_path, original)
        data, manifest_hashes = s.load_inputs(dvol, carry)
        self.assertEqual(set(data['perp']), set(s.SYMBOLS))
        self.assertEqual(len(manifest_hashes), 2)

    def test_bad_coverage_invalid_and_every_currency_checked(self):
        self.small_calendar()
        dvol, carry, trades, hashes = write_inputs(self.root)
        with patch.object(s, 'INPUT_SHA256', hashes), patch.object(s, 'VALID_END', T0 + 102 * s.DAY):
            result = s.study(dvol, carry, trades)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertEqual(set(result['validation']['by_symbol']), set(s.SYMBOLS))
        self.assertEqual(result['symbols'], {})
        self.assertIn('缺失超过 1%', s.report(result))

    def test_dvol_range_and_malformed_perp_invalid(self):
        self.small_calendar()
        for case in ('dvol', 'perp', 'manifest'):
            with self.subTest(case=case), tempfile.TemporaryDirectory(dir=self.root) as directory:
                dvol, carry, trades, hashes = write_inputs(Path(directory))
                if case == 'manifest':
                    (carry / 'manifest.json').write_text('{}')
                else:
                    path = dvol / 'ETH.json' if case == 'dvol' else carry / 'perp_1d/DOGEUSDT.json'
                    rows = json.loads(path.read_bytes())
                    rows[40][4] = 400 if case == 'dvol' else -1
                    s.write_json(path, rows)
                    base = dvol if case == 'dvol' else carry
                    manifest = json.loads((base / 'manifest.json').read_bytes())
                    manifest['files'][str(path.relative_to(base))] = s.file_metadata(path, rows)
                    s.write_json(base / 'manifest.json', manifest)
                    hashes = {k: s.sha256(p.read_bytes()) for k, p in s.input_paths(dvol, carry, trades).items()}
                with patch.object(s, 'INPUT_SHA256', hashes):
                    result = s.study(dvol, carry, trades)
                self.assertEqual(result['verdict'], 'invalid')
                self.assertTrue(result['validation']['reasons'])

    def test_coverage_boundary_exactly_one_percent_and_dvol_strict_bounds(self):
        bars = synthetic_bars(T0, 100)
        data = deepcopy(bars)
        del data['perp_1d'][30]
        self.assertTrue(s.validate_data(data, T0, T0 + 100 * s.DAY)['valid'])
        del data['perp_1d'][30]
        self.assertFalse(s.validate_data(data, T0, T0 + 100 * s.DAY)['valid'])
        for bound in (5., 400.):
            data = deepcopy(bars)
            data['dvol'][0][4] = bound
            self.assertFalse(s.validate_data(data, T0, T0 + 100 * s.DAY)['valid'])

    def test_end_to_end_synthetic_outputs_and_prereg_checked_twice(self):
        self.small_calendar()
        dvol, carry, trades, hashes = write_inputs(self.root)
        out = self.root / 'out'
        with patch.object(s, 'INPUT_SHA256', hashes), patch.object(s, 'verify_prereg', wraps=s.verify_prereg) as verify:
            with redirect_stdout(io.StringIO()), patch.object(s, 'write_ledger') as ledger:
                self.assertEqual(s.main(['run', '--dvol-dir', str(dvol), '--carry-dir', str(carry),
                                         '--trades', str(trades), '--out', str(out)]), 0)
                ledger.assert_not_called()
            self.assertEqual(verify.call_count, 2)
        result = json.loads((out / 'results.json').read_bytes())
        self.assertEqual(len(result['symbols']), 6)
        self.assertEqual(result['symbols']['ETH']['dvol_source'], 'ETH')
        self.assertEqual(result['symbols']['SOL']['dvol_source'], 'BTC')
        self.assertEqual(result['llm_overlay']['retained'], 1)
        self.assertEqual(len(result['btc_worst_five']), 5)
        self.assertEqual(result['statistics']['carriers']['BTC']['n'], 31)
        self.assertEqual(result['trials']['historical_runs_this_invocation'], 1)
        for symbol in result['symbols'].values():
            self.assertIn('s2', symbol['fits'][0]['models']['C'])
            self.assertIn('F0', symbol['daily'][0]['models']['B'])
        text = (out / 'report.md').read_text()
        for expected in ('未校准 QLIKE', '分位数', 'LLM 交易叠加', 'sd=0', 'σ_ref'):
            self.assertIn(expected, text)

    def test_invalid_cli_writes_report_and_returns_one(self):
        self.small_calendar()
        dvol, carry, trades, hashes = write_inputs(self.root)
        with patch.object(s, 'INPUT_SHA256', hashes), patch.object(s, 'VALID_END', T0 + 102 * s.DAY):
            with redirect_stdout(io.StringIO()):
                code = s.main(['run', '--dvol-dir', str(dvol), '--carry-dir', str(carry),
                               '--trades', str(trades), '--out', str(self.root / 'out')])
        self.assertEqual(code, 1)
        self.assertEqual(json.loads((self.root / 'out/results.json').read_bytes())['verdict'], 'invalid')

    def test_end_prereg_mismatch_refuses_any_output(self):
        self.small_calendar()
        dvol, carry, trades, hashes = write_inputs(self.root)
        out = self.root / 'out'
        with patch.object(s, 'INPUT_SHA256', hashes), patch.object(s, 'verify_prereg', side_effect=[b'ok', ValueError('changed')]):
            with redirect_stdout(io.StringIO()), patch('sys.stderr', new=io.StringIO()):
                with self.assertRaises(SystemExit):
                    s.main(['run', '--dvol-dir', str(dvol), '--carry-dir', str(carry),
                            '--trades', str(trades), '--out', str(out)])
        self.assertFalse(out.exists())

    def test_cli_defaults_and_optional_ledger_path(self):
        dummy = dict(verdict='fail')
        for flag, expected in (([], None), (['--write-ledger'], s.LEDGER),
                               (['--write-ledger', str(self.root / 'ledger.jsonl')], self.root / 'ledger.jsonl')):
            with patch.object(s, 'validate_output') as check, patch.object(s, 'study', return_value=dummy.copy()) as study:
                with patch.object(s, 'write_json'), patch.object(s, 'report', return_value='报告'), \
                        patch.object(s, 'write_ledger', return_value={}) as ledger, redirect_stdout(io.StringIO()):
                    self.root.joinpath('out').mkdir(exist_ok=True)
                    self.assertEqual(s.main(['run', '--out', str(self.root / 'out'), *flag]), 0)
                study.assert_called_once_with(s.REPO / 'data/dvol', s.REPO / 'data/carry',
                                              s.REPO / 'data/llm_trim_offline/export_v1/trades.jsonl')
                self.assertEqual(check.call_args.args[-1], expected)
                self.assertEqual(ledger.call_count, int(expected is not None))


class Forecasting(SyntheticTest):
    def test_calibration_uses_n_minus_p_and_correct_lognormal_level(self):
        rng = np.random.default_rng(1110)
        training = []
        for i in range(40):
            a, b, c, d = rng.normal(size=4)
            y = .2 + .1 * a + .2 * b - .3 * c + rng.normal(scale=.4)
            training.append(dict(time_ms=MONTH - (60 - i) * s.DAY, rv1=math.exp(a), rv7=math.exp(b),
                                 rv30=math.exp(c), iv=math.exp(d), log_y=y, y=math.exp(y)))
        row = dict(training[-1], time_ms=MONTH)
        result = s.calibrated_monthly_forecasts(training, [row], MONTH, MONTH + s.DAY)
        for model, p in (('B', 4), ('C', 5)):
            fit = result['fits'][0]['models'][model]
            x = np.array([s.model_features(r, model) for r in training])
            residual = np.array([r['log_y'] for r in training]) - x @ np.array(fit['coefficients'])
            expected = float(residual @ residual) / (40 - p)
            self.assertEqual(fit['residual_df'], 40 - p)
            self.assertAlmostEqual(fit['s2'], expected, places=14)
            prediction = result['forecasts'][0]['models'][model]
            self.assertAlmostEqual(prediction['F'], math.exp(prediction['log_prediction'] + expected / 2))
            self.assertAlmostEqual(prediction['F0'], math.exp(prediction['log_prediction']))
            self.assertAlmostEqual(prediction['sigma_hat'], math.sqrt(prediction['F']))
        with self.assertRaisesRegex(ValueError, 'n>p'):
            s.calibrated_fit(training[:5], 'C')
        self.assertFalse(s.calibrated_monthly_forecasts(training[:5], [row], MONTH, MONTH + s.DAY)['forecasts'])

    def test_B_C_predictions_equal_original_every_day_and_no_lookahead(self):
        data = synthetic_bars(s.TRAIN_START - 30 * s.DAY, 760)
        samples = s.build_samples(data['dvol'], data['perp_1d'])['samples']
        end = MONTH + 60 * s.DAY
        original = s.monthly_forecasts(samples, MONTH, end)
        actual = s.calibrated_monthly_forecasts(samples, start=MONTH, end=end)
        self.assertEqual([r['time_ms'] for r in actual['forecasts']], [r['time_ms'] for r in original['forecasts']])
        for ours, prior in zip(actual['forecasts'], original['forecasts']):
            for model in ('B', 'C'):
                self.assertEqual(ours['models'][model]['log_prediction'], prior['predictions'][model])
        self.assertEqual(actual['fits'][0]['train_last_ms'], MONTH - 8 * s.DAY)
        self.assertEqual(actual['fits'][0]['target_available_at_ms'], MONTH)
        changed = deepcopy(data)
        for row in changed['perp_1d']:
            if row[0] >= MONTH:
                row[1:5] = [v * 3 for v in row[1:5]]
        new_samples = s.build_samples(changed['dvol'], changed['perp_1d'])['samples']
        altered = s.calibrated_monthly_forecasts(new_samples, start=MONTH, end=end)
        self.assertEqual(actual['fits'][0], altered['fits'][0])
        # Perturbing the last allowed target must affect the fit.
        boundary = deepcopy(samples)
        next(r for r in boundary if r['time_ms'] == MONTH - 8 * s.DAY)['log_y'] += 10
        self.assertNotEqual(actual['fits'][0], s.calibrated_monthly_forecasts(boundary, start=MONTH, end=end)['fits'][0])

    def test_targetless_features_match_full_samples_and_ignore_future(self):
        data = synthetic_bars(s.TRAIN_START - 30 * s.DAY, 160)
        samples = s.build_samples(data['dvol'], data['perp_1d'])['samples']
        features = s.targetless_features(data['dvol'], data['perp_1d'])
        indexed = {r['time_ms']: r for r in features}
        for row in samples:
            self.assertEqual(indexed[row['time_ms']], {k: row[k] for k in ('time_ms', 'rv1', 'rv7', 'rv30', 'iv')})
            for model in ('B', 'C'):
                self.assertEqual(s.model_features(indexed[row['time_ms']], model), s.model_features(row, model))
        day = samples[20]['time_ms']
        truncated = {k: [r for r in v if r[0] <= day] for k, v in data.items()}
        self.assertEqual(s.targetless_features(truncated['dvol'], truncated['perp_1d'])[-1], indexed[day])
        self.assertGreater(features[-1]['time_ms'], samples[-1]['time_ms'])

    def test_november_targetless_forecasts_use_november_fit(self):
        end = s.timestamp_ms('2026-11-08')
        start = s.TRAIN_START - 30 * s.DAY
        data = synthetic_bars(start, (end - start) // s.DAY)
        samples = s.build_samples(data['dvol'], data['perp_1d'], s.TRAIN_START, end)['samples']
        tail = s.targetless_features(data['dvol'], data['perp_1d'], samples[-1]['time_ms'] + s.DAY, end)
        result = s.calibrated_monthly_forecasts(samples, samples + tail, s.OOS_END, end)
        self.assertEqual(len(result['forecasts']), 7)
        self.assertTrue(all(r['y'] is None for r in result['forecasts']))
        self.assertEqual(result['fits'][0]['month_ms'], s.OOS_END)
        self.assertEqual(result['fits'][0]['train_last_ms'], s.OOS_END - 8 * s.DAY)
        direct = s.calibrated_monthly_forecasts(samples, tail[:1], s.OOS_END, end)
        self.assertEqual(result['fits'], direct['fits'])
        self.assertEqual(result['forecasts'][0], direct['forecasts'][0])
        daily = s.sizing_rows(samples, result['forecasts'], data['perp_1d'])
        expected_ref = statistics.median(math.sqrt(r['rv30']) for r in samples)
        self.assertTrue(all(r['sigma_ref'] == expected_ref for r in daily))


class ScalingAndMetrics(SyntheticTest):
    def test_reference_is_exact_expanding_median_includes_training_and_today(self):
        samples = [dict(time_ms=s.TRAIN_START + i * s.DAY, rv30=v * v) for i, v in enumerate((1., 5., 3., 100.))]
        samples.insert(0, dict(time_ms=s.TRAIN_START - s.DAY, rv30=1e8))
        dates = [s.TRAIN_START + i * s.DAY for i in range(5)]
        refs = s.expanding_references(samples, dates)
        self.assertEqual([refs[t] for t in dates], [1., 3., 3., 4., 4.])
        changed = deepcopy(samples)
        changed[-1]['rv30'] = 1e10
        self.assertEqual([s.expanding_references(changed, dates)[t] for t in dates[:3]], [1., 3., 3.])

    def test_k_cap_and_next_day_return_timing(self):
        day = s.OOS_START
        samples = [dict(time_ms=day, rv30=4.)]
        forecasts = [dict(time_ms=day, rv30=4., y=1., fit_month_ms=day,
                          models=dict(B=dict(sigma_hat=4., F=16.), C=dict(sigma_hat=1., F=1.)))]
        perp = [[day - s.DAY, 0, 0, 0, 50], [day, 0, 0, 0, 100], [day + s.DAY, 0, 0, 0, 80]]
        row = s.sizing_rows(samples, forecasts, perp)[0]
        self.assertAlmostEqual(row['return_next'], -.2)
        self.assertEqual(row['models']['B']['k'], .5)
        self.assertAlmostEqual(row['models']['B']['x'], -.1)
        self.assertEqual(row['models']['C']['k'], 1.)
        self.assertEqual(row['models']['T']['k'], 1.)
        self.assertEqual(row['models']['U']['k'], 1.)
        self.assertEqual(s.evaluation_rows([row]), [row])
        missing = s.sizing_rows(samples, forecasts, perp[:-1])
        self.assertIsNone(missing[0]['models']['B']['x'])
        self.assertEqual(s.evaluation_rows(missing), [])
        self.assertNotIn('k', forecasts[0]['models']['B'])

    def test_ALT4_intersection_exposure_and_constituent_turnover(self):
        by_coin = {c: risk_rows(3) for c in s.ALT4}
        for c, k in zip(s.ALT4, (.2, .4, .6, .8)):
            for row in by_coin[c]:
                row['models']['B'].update(k=k, x=k * .1)
        by_coin['DOGE'] = by_coin['DOGE'][1:]
        rows = s.alt4_rows(by_coin)
        self.assertEqual([r['time_ms'] for r in rows], [MONTH + s.DAY, MONTH + 2 * s.DAY])
        self.assertAlmostEqual(rows[0]['models']['B']['k'], .5)
        self.assertAlmostEqual(rows[0]['models']['B']['x'], .05)
        # Equal and opposite changes leave average exposure fixed but incur turnover.
        rows[1]['models']['B']['constituent_k'] = [.4, .2, .6, .8]
        stats = s.carrier_statistics(rows)['models']['B']
        self.assertAlmostEqual(stats['mean_exposure'], .5)
        self.assertEqual(stats['normalized_returns'], [.1, .1])
        self.assertAlmostEqual(stats['turnover'], .1)
        self.assertAlmostEqual(stats['cost'], .00005)
        self.assertEqual(stats['k_quantiles']['0.5'], .5)
        self.assertEqual(stats['scaled_fraction'], 1.)

    def test_ES_ceil_and_absolute_tail(self):
        values = [-float(i) for i in range(1, 42)]
        self.assertEqual(s.expected_shortfall(values), 40.5)
        self.assertEqual(s.expected_shortfall(values[:40]), 40.)
        self.assertEqual(s.expected_shortfall([-.3, .1]), .3)
        self.assertIsNone(s.expected_shortfall([]))

    def test_weekly_blocks_threshold_losses_t_and_odd_halves(self):
        rows = risk_rows(36)
        # Missing calendar dates do not move seven-index block boundaries.
        rows[10:12] = [dict(r, time_ms=r['time_ms'] + 100 * s.DAY) for r in rows[10:12]]
        rows = sorted(rows, key=lambda r: r['time_ms'])
        stats = s.carrier_statistics(rows)
        weekly = stats['weekly']
        self.assertEqual(weekly['n_blocks'], 5)
        self.assertEqual(weekly['discarded_days'], 1)
        u = [r['models']['U']['x'] for r in rows]
        u_v = [365 / 7 * math.fsum(v * v for v in u[i:i + 7]) for i in range(0, 35, 7)]
        self.assertEqual(weekly['v_star'], statistics.median(u_v))
        losses = {}
        for m in s.MODELS:
            exposure = statistics.mean(r['models'][m]['k'] for r in rows)
            xs = [r['models'][m]['x'] / exposure for r in rows]
            vs = [365 / 7 * math.fsum(v * v for v in xs[i:i + 7]) for i in range(0, 35, 7)]
            ls = [max(0, math.log(v / weekly['v_star'])) ** 2 for v in vs]
            self.assertEqual(stats['models'][m]['weekly_variances'], vs)
            self.assertEqual(stats['models'][m]['weekly_losses'], ls)
            self.assertEqual(stats['models'][m]['mean_weekly_loss'], statistics.mean(ls))
            losses[m] = ls
        for left, right in (('B', 'C'), ('T', 'B'), ('T', 'U')):
            d = [a - b for a, b in zip(losses[left], losses[right])]
            paired = weekly['comparisons'][f'{left}_minus_{right}']
            self.assertEqual(paired['differences'], d)
            self.assertEqual(paired['first_half_mean'], statistics.mean(d[:2]))
            self.assertEqual(paired['second_half_mean'], statistics.mean(d[2:]))
            self.assertAlmostEqual(paired['t'], statistics.mean(d) / (statistics.stdev(d) / math.sqrt(5)))

    def test_sd_zero_empty_and_zero_threshold_handling(self):
        paired = s.paired_test([2., 2., 2.])
        self.assertEqual(paired['t'], 0.)
        self.assertIn('sd=0', paired['notes'][0])
        self.assertIsNone(s.paired_test([1.])['t'])
        self.assertIsNone(s.paired_test([])['t'])
        rows = risk_rows(14)
        for row in rows:
            for values in row['models'].values():
                values['x'] = 0.
        stats = s.carrier_statistics(rows)
        self.assertEqual(stats['weekly']['v_star'], 0.)
        self.assertIsNone(stats['models']['C']['mean_weekly_loss'])
        self.assertIsNone(stats['weekly']['comparisons']['B_minus_C']['t'])
        self.assertIsNone(s.carrier_statistics([])['models']['U']['es'])

    def test_qlike_calibrated_and_uncalibrated_daily_means(self):
        rows = [dict(y=y, models={m: dict(F=f, F0=f / 2) for m, f in (('B', 2.), ('C', 3.))})
                for y in (1., 4., 9.)]
        result = s.qlike_statistics(rows)
        for model, f in (('B', 2.), ('C', 3.)):
            self.assertAlmostEqual(result[model], statistics.mean(y / f - math.log(y / f) - 1 for y in (1., 4., 9.)))
            self.assertAlmostEqual(result['uncalibrated'][model], statistics.mean(y / (f / 2) - math.log(y / (f / 2)) - 1 for y in (1., 4., 9.)))

    def test_long_short_sharpe_and_compounded_drawdown(self):
        xs = [.1, -.2, .1]
        result = s.directional_metrics(xs)
        self.assertAlmostEqual(result['sharpe'], statistics.mean(xs) / statistics.stdev(xs) * math.sqrt(365))
        self.assertAlmostEqual(result['max_drawdown'], .2)
        short = s.directional_metrics([-x for x in xs])
        self.assertAlmostEqual(short['max_drawdown'], .1)
        self.assertIsNone(s.directional_metrics([.1, .1])['sharpe'])
        self.assertAlmostEqual(s.directional_metrics([-1.1])['max_drawdown'], 1.1)


class DecisionsAndOverlay(SyntheticTest):
    def test_all_verdicts_and_L_alt_three_of_four_boundary(self):
        stats = passing_stats()
        self.assertEqual(s.decide_verdict(stats, False), 'invalid')
        self.assertEqual(s.decide_verdict(stats), 'pass')
        stats['qlike']['SOL']['C'] = 1.
        self.assertEqual(s.decide_verdict(stats), 'pass')
        self.assertEqual(s.decision_conditions(stats)['L']['alt_better_count'], 3)
        stats['qlike']['BNB']['C'] = 1.
        self.assertEqual(s.decide_verdict(stats), 'weak')
        stats['qlike']['BTC']['C'] = 1.
        self.assertEqual(s.decide_verdict(stats), 'fail')
        self.assertEqual(s.decide_verdict({}), 'fail')

    def test_strict_R1_halves_R2_and_main_boundaries(self):
        for carrier, expected in (('BTC', 'fail'), ('ETH', 'fail'), ('ALT4', 'weak')):
            for field, value in (('t', 1.999), ('first_half_mean', 0.), ('second_half_mean', 0.)):
                stats = passing_stats()
                stats['carriers'][carrier]['weekly']['comparisons']['B_minus_C'][field] = value
                self.assertEqual(s.decide_verdict(stats), expected)
            for model in ('U', 'B'):
                stats = passing_stats()
                stats['carriers'][carrier]['models']['C']['es'] = stats['carriers'][carrier]['models'][model]['es']
                self.assertEqual(s.decide_verdict(stats), expected)
        stats = passing_stats()
        stats['qlike']['ETH']['C'] = stats['qlike']['ETH']['B']
        self.assertEqual(s.decide_verdict(stats), 'fail')

    def test_overlay_R_direction_date_boundary_normalization_and_exclusions(self):
        day = s.timestamp_ms('2026-11-03')
        daily = risk_rows(2, day)
        daily[0]['models']['B']['k'] = .5
        daily[1]['models']['B']['k'] = .25
        base = dict(entry_price=100., exit_price=110., initial_stop_loss=95., side='LONG', entry_time=day + s.DAY)
        trades = [dict(base, id=1), dict(base, id=2, side='short', entry_time=day + 2 * s.DAY + 123)]
        # Both exactly midnight and just before it use the last fully closed day.
        trades += [dict(base, id=3, entry_time=day + 2 * s.DAY - 1)]
        for bad in (dict(initial_stop_loss=None), dict(initial_stop_loss=100.), dict(entry_price=0.),
                    dict(exit_price=-1.), dict(exit_price=float('nan'))):
            trades.append(dict(base, **bad))
        trades += [dict(base, side='invalid'), dict(base, entry_time=None), dict(base, entry_time=day + 4 * s.DAY)]
        result = s.llm_overlay(trades, daily)
        self.assertEqual([r['R'] for r in result['trades']], [2., -2., 2.])
        self.assertEqual([r['k_date_ms'] for r in result['trades']], [day, day + s.DAY, day])
        self.assertEqual(result['excluded'], dict(invalid_prices_or_stop=5, invalid_side=1,
                                                 invalid_entry_time=1, missing_btc_prediction=1))
        self.assertEqual(result['total'], result['retained'] + result['excluded_total'])
        self.assertAlmostEqual(result['models']['U']['sum_R'], 2.)
        self.assertAlmostEqual(result['models']['B']['sum_R'], 1.5)
        self.assertAlmostEqual(result['models']['B']['mean_R'], .5)
        self.assertAlmostEqual(result['models']['B']['normalized_sum_R'], 1.5 / (1.25 / 3))
        self.assertAlmostEqual(result['models']['B']['normalized_mean_R'], .5 / (1.25 / 3))


class LedgerAndPaths(SyntheticTest):
    def test_K_must_be_three_noninvalid_and_invalid_does_not_count(self):
        path = self.root / 'ledger.jsonl'
        result = dict(verdict='pass', statistics=passing_stats(), input_hashes={'synthetic': 'hash'}, trials={})
        with self.assertRaisesRegex(ValueError, 'K=1'):
            s.write_ledger(result, path)
        self.assertEqual(path.read_bytes(), b'')
        invalid = s.write_ledger(dict(result, verdict='invalid'), path)
        self.assertIsNone(invalid['K'])
        self.assertFalse(invalid['counts_toward_k'])
        for i in range(2):
            evolution_ledger.append_event(path, 'crypto_llm', lambda k, previous: dict(
                track='crypto_llm', stage='S1', change_type='C', idea_id=f'prior-{i}',
                baseline_version='test', verdict='fail'))
        row = s.write_ledger(result, path)
        self.assertEqual(row['K'], 3)
        self.assertEqual(row['preregistered_K'], 3)
        self.assertEqual(row['idea_id'], 'dvol-sizing-scaler-v1')
        self.assertEqual(row['baseline_version'], 'no-market-vol-scaler')
        self.assertEqual(row['change_type'], 'C')
        self.assertEqual(row['prereg_sha256'], s.PREREG_SHA256)
        self.assertEqual(row['carriers']['BTC']['R1']['t'], 2.)
        self.assertEqual(row['L']['alt_better_count'], 4)
        before = path.read_bytes()
        with self.assertRaisesRegex(ValueError, 'K=4'):
            s.write_ledger(result, path)
        self.assertEqual(path.read_bytes(), before)

    def test_output_cannot_cover_inputs_or_ledger_or_prereg(self):
        dvol, carry, trades, hashes = write_inputs(self.root)
        out = self.root / 'out'
        s.validate_output(out, dvol, carry, trades, self.root / 'ledger.jsonl')
        for bad_out, ledger in ((dvol, None), (carry / 'child', None), (trades.parent, None),
                                (out, trades), (out, dvol / 'ledger.jsonl'), (out, out / 'results.json'),
                                (out, out / 'results.json.tmp'), (out, s.PREREG), (out, out),
                                (out, out / 'results.json/ledger.jsonl')):
            with self.subTest(out=bad_out, ledger=ledger), self.assertRaises(ValueError):
                s.validate_output(bad_out, dvol, carry, trades, ledger)
        out.mkdir()
        (out / 'results.json').symlink_to(dvol / 'BTC.json')
        with self.assertRaises(ValueError):
            s.validate_output(out, dvol, carry, trades)
        (out / 'results.json').unlink()
        (out / 'report.md').hardlink_to(trades)
        with self.assertRaises(ValueError):
            s.validate_output(out, dvol, carry, trades)


if __name__ == '__main__':
    unittest.main()
