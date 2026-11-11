"""Synthetic files and forecasts only. The optional CPU smoke test uses GBM."""
from contextlib import redirect_stdout
import io
import json
import math
import os
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

import numpy as np

from scripts.backtest import kronos_entry_veto_study as s

try:
    import torch
except ImportError:
    torch = None


T0 = 1_800_000_000_000
SCRATCH = Path('/private/tmp/claude-501/-Users-zhangyun-workspace-nofx--claude-worktrees-gallant-elgamal-16f421/'
               '8980eec2-7f3e-4502-b323-0cd88242875b/scratchpad')


def trade(number=1, **changes):
    row = dict(id=number, trader_id='synthetic', symbol='BTCUSDT', side='LONG', entry_time=T0 + 1234)
    row.update(changes)
    return row


def bars(entry_ms=T0 + 1234, n=512, post=False):
    start, end = s.kline_window(entry_ms, post)
    opens = list(range(start, end, s.STEP))[-n:]
    return [[opened, 100 + i / 10, 101 + i / 10, 99 + i / 10, 100 + i / 10, 10., 1000.]
            for i, opened in enumerate(opens)]


def paths_for(context, adverse=12, side=1):
    paths = np.tile(np.asarray(context[-1], dtype=np.float32), (16, 60, 1))
    p0 = context[-1][3]
    paths[:adverse, :, 3] = p0 * (1 - side * .01)
    paths[adverse:, :, 3] = p0 * (1 + side * .01)
    return paths


class SyntheticTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.start_patch(s.market_cache, 'fetch_binance_klines', side_effect=AssertionError('network forbidden'))

    def start_patch(self, target, name, *args, **kwargs):
        mock = patch.object(target, name, *args, **kwargs)
        self.addCleanup(mock.stop)
        return mock.start()

    def jsonl(self, name, rows):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(b''.join(s.json_bytes(row) for row in rows))
        return path

    def options(self):
        return SimpleNamespace(trades=self.root / 'source/trades.jsonl', results=self.root / 'source/results.json',
            klines_dir=self.root / 'klines', kronos_repo=self.root / 'kronos', weights=self.root / 'weights',
            out=self.root / 'out', write_ledger=None, device='cpu')

    def frozen_inputs(self, n=3):
        options = self.options()
        self.jsonl('source/trades.jsonl', [trade(i) for i in range(n)])
        s.write_json(options.results, dict(trades=[dict(id=i, no_trim=dict(r=-1. if i < n // 4 else .5),
                                                        baseline=dict(r=i / 10)) for i in range(n)]))
        prereg = self.root / 'prereg.md'
        prereg.write_text('synthetic frozen spec\n')
        self.start_patch(s, 'PREREG', prereg)
        for constant, path in (('PREREG_SHA256', prereg), ('TRADES_SHA256', options.trades),
                               ('RESULTS_SHA256', options.results)):
            self.start_patch(s, constant, s.file_sha256(path))
        hashes = {}
        for name in s.WEIGHT_HASHES:
            path = options.weights / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(name.encode())
            hashes[name] = s.file_sha256(path)
            s.write_json(path.parent / 'config.json', {})
        self.start_patch(s, 'WEIGHT_HASHES', hashes)
        self.start_patch(s, 'N_TRADES', n)
        options.kronos_repo.mkdir()
        self.start_patch(s.subprocess, 'check_output', return_value=s.KRONOS_HEAD + '\n')
        return options


class WindowsAndFetch(SyntheticTest):
    def test_last_closed_bar_and_exclusive_windows(self):
        for entered in (T0, T0 + 1, T0 + s.STEP - 1):
            start, end = s.kline_window(entered)
            self.assertEqual(end - s.STEP, T0 - s.STEP)
            self.assertEqual(end - start, 512 * s.STEP)
            self.assertLessEqual(end, entered)
            self.assertEqual(s.kline_window(entered, True), (T0, T0 + 60 * s.STEP))
        self.assertEqual(s.kline_window(T0 + s.STEP)[1], T0 + s.STEP)
        self.assertEqual(s.kline_filename(trade()), f'BTCUSDT_{T0 + 1234}.json')

    def test_fetch_dedupe_sort_clip_manifest_and_resume(self):
        source = self.jsonl('trades.jsonl', [trade()])
        target = self.root / 'klines'
        calls, sleeps = [], []
        def fetch(symbol, interval, start, end):
            calls.append((symbol, interval, start, end))
            rows = bars(post=len(calls) == 2)
            return [rows[-1], *reversed(rows), rows[0], [start - s.STEP, 1, 1, 1, 1, 1, 1],
                    [end, 1, 1, 1, 1, 1, 1]]
        manifest = s.fetch_klines(source, target, fetch=fetch, sleeper=sleeps.append)
        self.assertEqual(len(calls), 2)
        self.assertEqual(sleeps, [.3])
        self.assertEqual(manifest['trades_sha256'], s.file_sha256(source))
        for post, count in ((False, 512), (True, 60)):
            name = ('post/' if post else '') + s.kline_filename(trade())
            meta = manifest['files'][name]
            self.assertEqual(meta['rows'], count)
            self.assertTrue(meta['complete'])
            self.assertEqual(meta['sha256'], s.file_sha256(target / name))
            self.assertEqual((meta['requested_start_ms'], meta['requested_end_ms']), s.kline_window(T0 + 1234, post))
            self.assertEqual(json.loads((target / name).read_bytes()), bars(post=post))
        before = {p: (p.read_bytes(), p.stat().st_mtime_ns) for p in target.rglob('*.json') if p.name != 'manifest.json'}
        s.fetch_klines(source, target, fetch=lambda *a: self.fail('complete files refetched'), sleeper=self.fail)
        self.assertEqual(before, {p: (p.read_bytes(), p.stat().st_mtime_ns) for p in before})

    def test_incomplete_fetch_is_preserved_and_resumed(self):
        source = self.jsonl('trades.jsonl', [trade()])
        target = self.root / 'klines'
        def interrupted(symbol, interval, start, end):
            if end - start == 60 * s.STEP:
                raise OSError('interrupted')
            return bars()[:-1]
        with self.assertRaises(OSError):
            s.fetch_klines(source, target, fetch=interrupted, sleeper=lambda _: None)
        manifest = json.loads((target / 'manifest.json').read_bytes())
        self.assertFalse(manifest['files'][s.kline_filename(trade())]['complete'])
        self.assertEqual(manifest['files'][s.kline_filename(trade())]['rows'], 511)
        calls = []
        def resume(symbol, interval, start, end):
            calls.append(start)
            return bars(post=end - start == 60 * s.STEP)
        result = s.fetch_klines(source, target, fetch=resume, sleeper=lambda _: None)
        self.assertEqual(len(calls), 2)
        self.assertTrue(all(meta['complete'] for meta in result['files'].values()))
        for delay in (.299, -1, math.nan, math.inf):
            with self.assertRaises(ValueError):
                s.fetch_klines(source, target, delay, fetch=resume)


class DataAndRules(SyntheticTest):
    def test_context_validation_gap_short_last_open_and_bad_prices(self):
        for n in (480, 512):
            self.assertEqual(len(s.validate_context(bars(n=n), T0 + 1234)), n)
        invalid = [bars(n=479), bars()[:-1], bars()[:200] + bars()[201:]]
        for bad in (0, -1, math.nan, math.inf):
            rows = bars()
            rows[200][4] = bad
            invalid.append(rows)
        rows = bars()
        rows[0][0] += 1
        invalid.append(rows)
        for rows in invalid:
            with self.subTest(rows=len(rows)), self.assertRaises(ValueError):
                s.validate_context(rows, T0 + 1234)
        extended = [[bars()[0][0] - s.STEP, 1, 1, 1, 1, 1, 1], *bars()]
        self.assertEqual(s.validate_context(extended, T0 + 1234), bars())

    def test_frozen_join_iso_r_and_id_validation(self):
        options = self.frozen_inputs()
        rows = s.load_trades(options.trades, options.results)
        self.assertEqual(rows[0]['trade_id'], '0')
        self.assertEqual(rows[0]['side_sign'], 1)
        self.assertEqual(rows[0]['r'], .5)
        self.assertEqual(rows[2]['r_baseline'], .2)
        self.assertRegex(rows[0]['entry_time'], r'^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.234Z$')
        result = json.loads(options.results.read_bytes())
        result['trades'][0]['id'] = 999
        s.write_json(options.results, result)
        with self.assertRaises(ValueError):
            s.load_trades(options.trades, options.results)
        self.jsonl('duplicates.jsonl', [trade(), trade()])
        with self.assertRaises(ValueError):
            s.load_export(self.root / 'duplicates.jsonl')

    def test_adverse_long_short_threshold_and_equal_terminal(self):
        context = [r[1:7] for r in bars()]
        for side in (1, -1):
            for count in (0, 11, 12, 13, 16):
                result = s.forecast_summary(paths_for(context, count, side), context, side)
                self.assertEqual(result['p_adverse'], count / 16)
                self.assertEqual(result['drop'], count >= 12)
                self.assertEqual(result['n_context'], 512)
                self.assertAlmostEqual(result['past5h'], math.log(context[-1][3] / context[-61][3]))
            paths = paths_for(context, 12, side)
            paths[0, -1, 3] = context[-1][3]
            self.assertEqual(s.forecast_summary(paths, context, side)['p_adverse'], 11 / 16)
        closes = np.asarray(context)[:, 3]
        self.assertAlmostEqual(result['z_last'], (closes[-1] - closes.mean()) / closes.std(ddof=0))
        flat = np.full((480, 6), 100.)
        self.assertIsNone(s.forecast_summary(paths_for(flat), flat, 1)['z_last'])

    def test_proxy_numeric_ties_sides_and_no_forecast(self):
        trades = [dict(trade_id=key, side_sign=side) for key, side in [('10', 1), ('2', -1), ('3', 1), ('1', 1)]]
        records = [dict(trade_id=key, z_last=z, no_forecast=missing, drop=drop)
                   for key, z, missing, drop in [('10', 2., False, True), ('2', -2., False, True),
                                                ('3', 1., False, False), ('1', 100., True, False)]]
        self.assertEqual(s.proxy_mask(trades, records), {'10': True, '2': True, '3': False, '1': False})
        records[0]['drop'] = False
        self.assertEqual([key for key, drop in s.proxy_mask(trades, records).items() if drop], ['2'])

    def test_extra_invalid_gate_boundaries(self):
        records = [dict(no_forecast=i >= 95, drop=i < 20) for i in range(100)]
        gates = s.s0_gates(records, dict(valid=True), dict(valid=True))
        self.assertTrue(all(g['valid'] for g in gates.values()))
        records[19]['drop'] = False
        self.assertFalse(s.s0_gates(records, dict(valid=True), dict(valid=True))['G4']['valid'])
        records[94]['no_forecast'] = True
        self.assertFalse(s.s0_gates(records, dict(valid=True), dict(valid=True))['G2']['valid'])

    def test_spearman_ties_missing_and_constant(self):
        self.assertEqual(s.spearman([(1, 2), (2, 3), (2, 3), (3, 4)]), dict(n=4, rho=1.))
        self.assertEqual(s.spearman([(1, 3), (2, 2), (3, 1)])['rho'], -1.)
        self.assertIsNone(s.spearman([(1, 1), (1, 2)])['rho'])
        self.assertEqual(s.spearman([(None, 1), (math.nan, 2), (1, 1)]), dict(n=1, rho=None))


class Forecasting(SyntheticTest):
    def test_synthetic_gate_exact_seeds_context_and_mean_boundaries(self):
        calls = []
        def forecast(context, opens, seed):
            index = len(calls)
            self.assertEqual(seed, 11 + index)
            np.testing.assert_array_equal(context, s.gbm(572, 5000 + index).values[:512])
            self.assertEqual(context.shape, (512, 6))
            self.assertEqual(opens[0], 1_893_456_000_000)
            self.assertEqual(opens[-1] - opens[0], 511 * s.STEP)
            calls.append(seed)
            return paths_for(context, 8)
        with redirect_stdout(io.StringIO()):
            gate = s.synthetic_gate(forecast)
        self.assertTrue(gate['valid'])
        self.assertEqual(gate['mean_p_up'], .5)
        self.assertEqual(len(calls), 50)
        with redirect_stdout(io.StringIO()):
            failed = s.synthetic_gate(lambda context, opens, seed: paths_for(context, 16))
        self.assertFalse(failed['valid'])

    def test_checkpoints_reuse_input_change_and_torn_tail(self):
        source = self.jsonl('trades.jsonl', [trade()])
        result_path = self.root / 'r.json'
        s.write_json(result_path, dict(trades=[dict(id=1, no_trim=dict(r=.1), baseline=dict(r=.2))]))
        self.start_patch(s, 'N_TRADES', 1)
        trades = s.load_trades(source, result_path)
        klines = self.root / 'klines'
        s.write_json(klines / s.kline_filename(trade()), bars())
        inputs = dict(frozen='a')
        calls = []
        def forecast(context, opens, seed):
            calls.append(seed)
            return paths_for(context)
        with redirect_stdout(io.StringIO()):
            first, _ = s.collect_forecasts(trades, s.validate_data(trades, klines), self.root / 'out', inputs, forecast)
            second, _ = s.collect_forecasts(trades, s.validate_data(trades, klines), self.root / 'out', inputs, forecast)
        self.assertEqual(first, second)
        self.assertEqual(calls, [20261112])
        checkpoint = self.root / 'out/forecasts.jsonl'
        original = checkpoint.read_bytes()
        with checkpoint.open('ab') as stream:
            stream.write(b'{"trade_id":')
        s.load_checkpoints(checkpoint)
        self.assertEqual(checkpoint.read_bytes(), original)
        changed = bars()
        changed[10][5] = 20
        s.write_json(klines / s.kline_filename(trade()), changed)
        with redirect_stdout(io.StringIO()):
            s.collect_forecasts(trades, s.validate_data(trades, klines), self.root / 'out', inputs, forecast)
            s.collect_forecasts(trades, s.validate_data(trades, klines), self.root / 'out', dict(frozen='b'), forecast)
        self.assertEqual(len(calls), 3)
        self.assertEqual(len(checkpoint.read_bytes().splitlines()), 3)

    def test_reproducibility_compares_probability_and_all_paths(self):
        context = [r[1:7] for r in bars()]
        opens = [r[0] for r in bars()]
        paths = paths_for(context)
        records = [dict(trade_id=str(i), no_forecast=False, side_sign=1, paths=paths.tolist(),
                        **s.forecast_summary(paths, context, 1)) for i in range(10)]
        contexts = {str(i): (context, opens) for i in range(10)}
        forecast = lambda context, opens, seed: paths.copy()
        gate = s.reproducibility_gate(records, contexts, forecast)
        self.assertTrue(gate['valid'])
        records[0]['p_adverse'] = .5
        self.assertIn('0', s.reproducibility_gate(records, contexts, forecast)['mismatches'])
        records[0]['p_adverse'] = .75
        records[0]['paths'][0][0][0] += 1
        self.assertIn('0', s.reproducibility_gate(records, contexts, forecast)['mismatches'])
        self.assertFalse(s.reproducibility_gate(records[:9], contexts, forecast)['valid'])

    @unittest.skipUnless(torch is not None, 'torch is not importable')
    def test_torch_numpy_shim_and_optional_local_gbm_determinism(self):
        import pandas as pd
        calls = []
        module = SimpleNamespace(np=np)
        def stamps(series):
            self.assertIsNone(series.dt.tz)
            return pd.DataFrame({name: getattr(series.dt, name) for name in ('minute', 'hour', 'weekday', 'day', 'month')})
        module.calc_time_stamps = stamps
        def inference(tok, model, x, xs, ys, **kwargs):
            self.assertEqual(tuple(x.shape), (1, 512, 6))
            self.assertEqual(tuple(xs.shape), (1, 512, 5))
            self.assertEqual(tuple(ys.shape), (1, 60, 5))
            self.assertEqual(x.dtype, torch.float32)
            self.assertEqual(x.device.type, 'cpu')
            self.assertLessEqual(float(x.abs().max()), 5.)
            self.assertEqual(kwargs, dict(max_context=512, pred_len=60, clip=5, T=1., top_k=0,
                                          top_p=.9, sample_count=16, verbose=False))
            calls.append(True)
            paths = torch.rand((1, 16, 512, 6)).numpy()
            return module.np.mean(paths, axis=1)
        module.auto_regressive_inference = inference
        context = s.gbm(572, 5000).values[:512]
        opens = (pd.date_range('2030-01-01', periods=572, freq='5min').asi8 // 1_000_000).tolist()[:512]
        bundle = (None, None, module)
        a = s.forecast_paths(bundle, context, opens, 11)
        b = s.forecast_paths(bundle, context, opens, 11)
        np.testing.assert_array_equal(a, b)
        self.assertEqual(a.shape, (16, 60, 6))
        self.assertIs(module.np, np)
        self.assertEqual(len(calls), 2)
        def wrong_mean(*args, **kwargs):
            return module.np.mean(np.ones((1, 16, 60, 6)), axis=1) + 1
        with patch.object(module, 'auto_regressive_inference', side_effect=wrong_mean):
            with self.assertRaises(AssertionError):
                s.capture_inference(module)
        self.assertIs(module.np, np)
        with patch.object(module, 'auto_regressive_inference', side_effect=RuntimeError('failure')):
            with self.assertRaises(RuntimeError):
                s.capture_inference(module)
        self.assertIs(module.np, np)
        weights, repo = SCRATCH / 'weights', SCRATCH / 'ext/Kronos'
        if weights.is_dir() and repo.is_dir():
            with patch.dict(os.environ, HF_HUB_OFFLINE='1'):
                real = s.load_models(repo, weights)
                a = s.forecast_paths(real, context, opens, 11)
                b = s.forecast_paths(real, context, opens, 11)
            self.assertEqual(a.shape, (16, 60, 6))
            np.testing.assert_array_equal(a, b)


class IntegrityAndLedger(SyntheticTest):
    def test_every_frozen_hash_and_head_refuses_before_output_or_ledger(self):
        options = self.frozen_inputs()
        options.write_ledger = self.root / 'ledger.jsonl'
        inputs = s.verify_inputs(options)
        self.assertEqual(inputs['prereg_sha256'], s.PREREG_SHA256)
        models = self.start_patch(s, 'load_models', side_effect=AssertionError('models must not load'))
        for path in (s.PREREG, options.trades, options.results,
                     *(options.weights / name for name in s.WEIGHT_HASHES)):
            original = path.read_bytes()
            path.write_bytes(original + b'changed')
            with self.subTest(path=path), self.assertRaisesRegex(ValueError, 'mismatch'):
                s.study(options)
            path.write_bytes(original)
        with patch.object(s.subprocess, 'check_output', return_value='bad-head'):
            with self.assertRaisesRegex(ValueError, 'HEAD mismatch'):
                s.study(options)
        models.assert_not_called()
        self.assertFalse(options.out.exists())
        self.assertFalse(options.write_ledger.exists())

    def test_output_overlap_including_symlinks_and_ledger(self):
        options = self.options()
        s.guard_outputs(options)
        for out in (options.klines_dir, options.kronos_repo / 'nested', options.weights,
                    options.trades.parent, options.results.parent):
            with self.subTest(out=out), self.assertRaises(ValueError):
                s.guard_outputs(SimpleNamespace(**dict(vars(options), out=out)))
        for ledger in (options.trades, options.results, options.klines_dir / 'ledger', options.out / 'mask.jsonl'):
            with self.subTest(ledger=ledger), self.assertRaises(ValueError):
                s.guard_outputs(SimpleNamespace(**dict(vars(options), write_ledger=ledger)))
        options.klines_dir.mkdir()
        alias = self.root / 'alias'
        alias.symlink_to(options.klines_dir, target_is_directory=True)
        with self.assertRaises(ValueError):
            s.guard_outputs(SimpleNamespace(**dict(vars(options), out=alias)))

    def test_device_refusal_precedes_models_and_output(self):
        options = self.options()
        options.device = 'mps'
        with self.assertRaisesRegex(ValueError, 'cpu'):
            s.study(options)
        self.assertFalse(options.out.exists())

    def test_ledger_k_guard_and_invalid_k_none(self):
        ledger = self.root / 'ledger.jsonl'
        result = dict(verdict='fail', stats=dict(n=439, dropped_n=100, delta=.1),
                      input_hashes=dict(synthetic='hash'), proxy=dict(delta=.05), s2_eligible=False)
        with self.assertRaisesRegex(ValueError, 'K=1'):
            s.write_ledger(result, ledger)
        self.assertEqual(ledger.read_bytes(), b'')
        invalid = s.write_ledger(dict(result, verdict='invalid'), ledger)
        self.assertIsNone(invalid['K'])
        self.assertFalse(invalid['counts_toward_k'])
        for i in range(4):
            s.evolution_ledger.append_event(ledger, 'crypto_llm', lambda k, prev: dict(track='crypto_llm',
                stage='S1', change_type='B', baseline_version='synthetic', idea_id=str(k), verdict='fail'))
        before = len(s.evolution_ledger.read_rows(ledger))
        row = s.write_ledger(result, ledger)
        self.assertEqual(row['K'], 5)
        self.assertEqual(row['preregistered_K'], 5)
        self.assertEqual(row['z_K'], s.incremental_filter.z_threshold(5))
        self.assertTrue(row['counts_toward_k'])
        self.assertEqual(row['idea_id'], 'kronos-entry-veto-v1')
        self.assertEqual(len(s.evolution_ledger.read_rows(ledger)), before + 1)
        with self.assertRaisesRegex(ValueError, 'K=6'):
            s.write_ledger(result, ledger)
        self.assertEqual(len(s.evolution_ledger.read_rows(ledger)), before + 1)

    def test_synthetic_study_order_invalid_outputs_and_evaluate_compatibility(self):
        options = self.frozen_inputs(100)
        raw_trades = [trade(i, symbol=f'S{i}USDT', side='LONG' if i % 2 else 'SHORT') for i in range(100)]
        self.jsonl('source/trades.jsonl', raw_trades)
        self.start_patch(s, 'TRADES_SHA256', s.file_sha256(options.trades))
        for raw in raw_trades[:-1]:
            s.write_json(options.klines_dir / s.kline_filename(raw), bars())
            s.write_json(options.klines_dir / 'post' / s.kline_filename(raw), bars(post=True))
        events = []
        validate = s.validate_data
        def validation(*args):
            events.append('validation')
            return validate(*args)
        def synthetic(forecast):
            events.append('G3')
            return dict(valid=False, mean_p_up=.7)
        def forecast(bundle, context, opens, seed):
            self.assertIn('G3', events)
            key = seed - 20261111
            return paths_for(context, 12 if key < 25 else 4, 1 if key % 2 else -1)
        reproduction = s.reproducibility_gate
        def g1(*args):
            result = reproduction(*args)
            events.append('G1')
            return result
        statistics = s.incremental_filter.filter_statistics
        def stats(*args, **kwargs):
            self.assertEqual(events, ['validation', 'G3', 'G1'])
            self.assertEqual(kwargs, dict(seed=20261103, placebo_n=2000))
            return statistics(*args, **kwargs)
        with patch.object(s, 'validate_data', side_effect=validation), patch.object(s, 'synthetic_gate', side_effect=synthetic), \
                patch.object(s, 'load_models', return_value=None), patch.object(s, 'forecast_paths', side_effect=forecast), \
                patch.object(s, 'reproducibility_gate', side_effect=g1), \
                patch.object(s.incremental_filter, 'filter_statistics', side_effect=stats), redirect_stdout(io.StringIO()):
            result = s.study(options)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertFalse(result['s2_eligible'])
        self.assertTrue(result['gates']['G1']['valid'])
        self.assertEqual(result['gates']['G2']['no_forecast_n'], 1)
        self.assertEqual(result['stats']['dropped_n'], 25)
        self.assertEqual(len(result['trades']), 100)
        missing = next(row for row in result['trades'] if row['trade_id'] == '99')
        self.assertTrue(missing['no_forecast'])
        self.assertFalse(missing['drop'])
        self.assertTrue(missing['reason'])
        self.assertEqual(result['descriptives']['directional_hit_rate']['n'], 99)
        s.write_outputs(result, options.out)
        for name in s.OUTPUTS:
            self.assertTrue((options.out / name).is_file())
        for name, digest in result['output_hashes'].items():
            self.assertEqual(s.file_sha256(options.out / name), digest)
        saved = json.loads((options.out / 'results.json').read_bytes())
        self.assertEqual(saved['verdict'], 'invalid')
        evaluated = s.incremental_filter.evaluate_files(SimpleNamespace(trades=options.out / 'trades.jsonl',
            mask=options.out / 'mask.jsonl', seed=20261103, placebo_n=2000, track='crypto_llm', no_ledger=True,
            ledger=self.root / 'absent-ledger.jsonl', baseline_version='test', idea_id='test', hypothesis='test'))
        self.assertEqual(evaluated['delta'], result['stats']['delta'])
        self.assertEqual(evaluated['dropped_n'], 25)
        output = io.StringIO()
        argv = ['run', *[value for key in ('trades', 'results', 'klines-dir', 'kronos-repo', 'weights', 'out')
                         for value in ('--' + key, str(getattr(options, key.replace('-', '_'))))]]
        with patch.object(s, 'study', return_value=result), redirect_stdout(output):
            self.assertEqual(s.main(argv), 1)
        self.assertIn('VERDICT invalid', output.getvalue())


if __name__ == '__main__':
    unittest.main()
