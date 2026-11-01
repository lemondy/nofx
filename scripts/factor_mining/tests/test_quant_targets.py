"""Offline target-rule, persistence, integration and study-alignment checks."""
import contextlib
import copy
import datetime as dt
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import ensemble, live_scores, miner, quant_targets as qt, store
from scripts.factor_mining.panel import Panels
from scripts.factor_mining.tests.synth import SIGNAL_SEED, write_market


def fixture(names=60, rows=120):
    index = pd.date_range('2026-01-01', periods=rows)
    columns = [f'S{i:03}' for i in range(names)]
    t, j = np.arange(rows)[:, None], np.arange(names)[None, :]
    returns = 0.0001 * j + (0.01 + 0.0001 * j) * np.sin(t * 1.3)
    close = pd.DataFrame(100 * np.cumprod(1 + returns, axis=0), index=index, columns=columns)
    fields = dict(close=close, open=close.copy(), high=close * 1.01,
                  low=close * .99, volume=close * 100, vwap=close.copy())
    panels = Panels(fields, close.notna())
    scores = {s: dict(z=float(i), pct=round((i + 1) / names, 4)) for i, s in enumerate(columns)}
    doc = dict(version=1, market='crypto', as_of=index[-1].strftime('%Y-%m-%d'),
               generated_at=(index[-1] + dt.timedelta(days=1)).isoformat(),
               run_id='synthetic-run', universe_size=names,
               horizons={'3': dict(factor_ids=['f_core'], scores=scores)})
    return panels, doc


def build(panels, doc, market='crypto', **kwargs):
    now = kwargs.pop('now', pd.Timestamp(doc['as_of']) + dt.timedelta(days=1))
    return qt.build_targets(market, doc, panels, now=now, **kwargs)


class TargetRulesTests(unittest.TestCase):
    def test_floor_quantiles_disjoint_legs_and_exact_schema(self):
        panels, doc = fixture(names=59)
        before_panels, before_doc = copy.deepcopy(panels), copy.deepcopy(doc)
        result = build(panels, doc)
        longs = {s for s, w in result['weights'].items() if w > 0}
        shorts = {s for s, w in result['weights'].items() if w < 0}
        self.assertEqual(result['status'], 'ok')
        self.assertEqual(longs, set(panels.mask.columns[-11:]))
        self.assertEqual(shorts, set(panels.mask.columns[:11]))
        self.assertFalse(longs & shorts)
        self.assertAlmostEqual(sum(result['weights'][s] for s in longs), .5, places=5)
        self.assertAlmostEqual(sum(result['weights'][s] for s in shorts), -.5, places=5)
        self.assertEqual(set(result), {'version', 'market', 'status', 'reason', 'generated_at',
                                     'as_of', 'run_id', 'horizon', 'rule', 'factor_ids',
                                     'universe_size', 'weights', 'excluded', 'pct'})
        self.assertEqual(result['rule'], dict(name='ls_invvol_protected_v1', quantile=.2,
            long_gross=.5, short_gross=.5, max_name_weight=.1, vol_window=30,
            short_exclude_ret7_quantile=.9, min_listing_days=90, long_only=False))
        self.assertEqual(result['factor_ids'], ['f_core'])
        self.assertEqual(result['run_id'], doc['run_id'])
        self.assertEqual(result['pct']['S058'], 1.)
        self.assertTrue(all(w == round(w, 6) and w != 0 for w in result['weights'].values()))
        self.assertEqual(doc, before_doc)
        pd.testing.assert_frame_equal(panels.mask, before_panels.mask)
        pd.testing.assert_frame_equal(panels.fields['close'], before_panels.fields['close'])

    def test_ties_are_stable_and_never_overlap(self):
        panels, doc = fixture()
        for v in doc['horizons']['3']['scores'].values():
            v['z'] = 0.
        result = build(panels, doc)
        self.assertEqual({s for s,w in result['weights'].items() if w < 0}, set(panels.mask.columns[:12]))
        self.assertEqual({s for s,w in result['weights'].items() if w > 0}, set(panels.mask.columns[-12:]))

    def test_short_exclusions_use_whole_eligible_ret7_and_never_refill(self):
        panels, doc = fixture()
        panels.fields['close'].loc[panels.mask.index[-7:], 'S000'] *= 2.
        panels.fields['close'].loc[panels.mask.index[:40], 'S001'] = np.nan
        result = build(panels, doc)
        self.assertEqual(result['excluded'], {'S000': 'short_ret7_top_decile',
                                               'S001': 'short_listing_lt_90d'})
        shorts = {s for s,w in result['weights'].items() if w < 0}
        self.assertEqual(shorts, set(panels.mask.columns[2:12]))
        self.assertNotIn('S012', result['weights'])
        self.assertEqual(len([w for w in result['weights'].values() if w > 0]), 12)

    def test_ret7_threshold_is_inclusive_and_history_boundary_is_90(self):
        panels, doc = fixture()
        # With ten finite score-bearing names above the 90th percentile, an
        # exact tie at the quantile must also be removed (>=, not >).
        close = panels.fields['close']
        reference = close.iloc[-8]
        gains = np.arange(60, dtype=float) / 100
        gains[0] = gains[54]
        close.iloc[-1] = reference * (1 + gains)
        close.loc[close.index[:30], 'S001'] = np.nan  # exactly 90 observations
        result = build(panels, doc)
        self.assertIn('short_ret7_top_decile', result['excluded']['S000'])
        self.assertNotIn('S001', result['excluded'])
        self.assertIn('S001', result['weights'])

    def test_both_short_reasons_are_retained_and_masked_pump_is_irrelevant(self):
        panels, doc = fixture()
        close = panels.fields['close']
        close.loc[close.index[:40], 'S000'] = np.nan
        close.loc[close.index[-7:], 'S000'] *= 2.
        close.loc[close.index[-7:], 'S059'] *= 100.
        panels.mask.iloc[-1, -1] = False
        result = build(panels, doc)
        self.assertEqual(result['universe_size'], 59)
        self.assertEqual(result['excluded']['S000'], 'short_ret7_top_decile,short_listing_lt_90d')
        self.assertNotIn('S059', result['pct'])

    def test_inverse_vol_uses_30_returns_sample_std(self):
        panels, doc = fixture()
        result = build(panels, doc)
        close = panels.fields['close']
        vol = close.iloc[-31:].pct_change(fill_method=None).iloc[1:].std(ddof=1)
        for symbols, sign in ((close.columns[-12:], 1), (close.columns[:12], -1)):
            inverse = 1 / vol.loc[symbols]
            expected = sign * .5 * inverse / inverse.sum()
            for s in symbols:
                self.assertAlmostEqual(result['weights'][s], expected[s], delta=5.01e-7)

    def test_iterative_cap_reallocates_again_after_first_cap_and_all_capped(self):
        vol = pd.Series([1., 4., 10., 10., 10., 10., 10.], index=list('abcdefg'))
        weights = qt._capped_invvol(vol, .5, .1)
        # a caps first; that redistribution makes b cap next.
        expected = pd.Series([.1, .1, .06, .06, .06, .06, .06], index=vol.index)
        np.testing.assert_allclose(weights, expected, atol=1e-12)
        np.testing.assert_allclose(qt._capped_invvol(vol.iloc[:3], .5, .1), [.1, .1, .1])
        np.testing.assert_allclose(qt._capped_invvol(vol, 1., .1), [.1] * 7)

    def test_sleeve_with_too_few_names_accepts_lower_gross(self):
        panels, doc = fixture(names=15)
        result = build(panels, doc)
        self.assertEqual(result['status'], 'ok')
        self.assertAlmostEqual(sum(w for w in result['weights'].values() if w > 0), .3)
        self.assertAlmostEqual(sum(w for w in result['weights'].values() if w < 0), -.3)

    def test_empty_short_sleeve_and_thin_universe_are_empty(self):
        panels, doc = fixture(names=15)
        panels.fields['close'].iloc[:40, :3] = np.nan
        result = build(panels, doc)
        self.assertEqual(result['status'], 'insufficient_universe')
        self.assertEqual(result['reason'], 'empty_short_leg_after_exclusions')
        self.assertEqual(result['weights'], {})
        self.assertEqual(len(result['excluded']), 3)
        panels.mask.iloc[-1, 0] = False
        thin = build(panels, doc)
        self.assertEqual(thin['status'], 'insufficient_universe')
        self.assertEqual(thin['weights'], {})
        self.assertEqual(thin['universe_size'], 14)

    def test_eligibility_needs_finite_score_positive_full_window_vol_and_mask(self):
        panels, doc = fixture()
        close = panels.fields['close']
        panels.mask.iloc[-1, 0] = False
        doc['horizons']['3']['scores']['S001']['z'] = np.inf
        doc['horizons']['3']['scores']['S002']['z'] = np.nan
        close['S003'] = 100.
        close.iloc[-15, 4] = np.nan
        close.iloc[-32, 5] = np.nan  # outside the necessary 31 close observations
        doc['horizons']['3']['ext_scores'] = {'EXTUSDT': dict(z=-1e6, pct=0.)}
        result = build(panels, doc)
        self.assertEqual(result['universe_size'], 55)
        for s in ('S000', 'S001', 'S002', 'S003', 'S004', 'EXTUSDT'):
            self.assertNotIn(s, result['weights'])
            self.assertNotIn(s, result['pct'])
        self.assertIn('S005', result['weights'])

    def test_us_first_available_horizon_long_only_key_mapping_and_budget(self):
        panels, doc = fixture()
        entry = doc['horizons'].pop('3')
        doc['horizons'] = {'5': {'scores': {}}, '10': entry, '20': copy.deepcopy(entry)}
        doc['horizons']['20']['scores']['S000']['z'] = 1e6
        result = build(panels, doc, market='us')
        self.assertEqual(result['horizon'], 10)
        self.assertEqual(result['rule']['name'], 'long_invvol_v1')
        self.assertTrue(result['rule']['long_only'])
        self.assertEqual(result['excluded'], {})
        self.assertTrue(all(0 < w <= .1 for w in result['weights'].values()))
        self.assertAlmostEqual(sum(result['weights'].values()), 1., places=5)
        self.assertEqual(set(result['weights']), {s + 'BUSDT' for s in panels.mask.columns[-12:]})
        self.assertIn('S059BUSDT', result['pct'])
        self.assertNotIn('S059', result['pct'])
        doc['horizons']['5'] = entry
        self.assertEqual(build(panels, doc, market='us')['horizon'], 5)

    def test_no_future_panel_rows_can_change_targets_or_history_age(self):
        panels, doc = fixture()
        panels.fields['close'].iloc[:40, 0] = np.nan
        expected = build(panels, doc)
        future = pd.date_range(panels.mask.index[-1] + dt.timedelta(days=1), periods=200)
        for name, frame in panels.fields.items():
            panels.fields[name] = pd.concat([frame, pd.DataFrame(1e100, index=future, columns=frame.columns)])
        panels.mask = pd.concat([panels.mask, pd.DataFrame(False, index=future, columns=panels.mask.columns)])
        self.assertEqual(build(panels, doc), expected)
        self.assertEqual(expected['excluded']['S000'], 'short_listing_lt_90d')

    def test_missing_scores_no_factors_ignores_other_horizons_and_extensions(self):
        for market in ('crypto', 'us'):
            for doc in (None, {}, {'horizons': {}}, {'horizons': {'3': {'ext_scores': {'EXT': {'z': 1}}}}}):
                with self.subTest(market=market, doc=doc):
                    result = qt.build_targets(market, doc, None, now='2026-11-01')
                    self.assertEqual(result['status'], 'no_validated_factors')
                    self.assertEqual(result['weights'], {})
                    self.assertEqual(result['horizon'], 3 if market == 'crypto' else None)
        panels, doc = fixture()
        doc['horizons']['1'] = doc['horizons'].pop('3')
        self.assertEqual(build(panels, doc)['status'], 'no_validated_factors')

    def test_staleness_boundary_uses_utc_dates_and_empty_weights(self):
        panels, doc = fixture()
        day = pd.Timestamp(doc['as_of'])
        self.assertEqual(build(panels, doc, now=day + dt.timedelta(days=5, hours=23))['status'], 'ok')
        stale = build(panels, doc, now=day + dt.timedelta(days=6))
        self.assertEqual(stale['status'], 'stale_scores')
        self.assertEqual(stale['weights'], {})
        local = (day + dt.timedelta(days=6, hours=1)).tz_localize('Asia/Shanghai')
        self.assertEqual(build(panels, doc, now=local)['status'], 'ok')
        doc['horizons']['5'] = doc['horizons'].pop('3')
        self.assertEqual(build(panels, doc, market='us', now=day + dt.timedelta(days=7))['status'], 'ok')
        self.assertEqual(build(panels, doc, market='us', now=day + dt.timedelta(days=8))['status'], 'stale_scores')

    def test_missing_asof_or_missing_panel_date_has_no_weights(self):
        panels, doc = fixture()
        panels.mask = panels.mask.iloc[:-1]
        result = build(panels, doc)
        self.assertEqual(result['status'], 'insufficient_universe')
        self.assertEqual(result['weights'], {})
        doc['as_of'] = None
        result = qt.build_targets('crypto', doc, panels, now='2026-11-01')
        self.assertEqual(result['status'], 'stale_scores')
        self.assertEqual(result['weights'], {})

    def test_horizon_cannot_override_fixed_selection(self):
        panels, doc = fixture()
        self.assertEqual(build(panels, doc, horizon=3)['horizon'], 3)
        with self.assertRaises(ValueError):
            build(panels, doc, horizon=1)


class PersistenceIntegrationTests(unittest.TestCase):
    def test_atomic_write_only_target_changes_and_no_temp_remains(self):
        panels, doc = fixture()
        with tempfile.TemporaryDirectory() as root:
            base = Path(root) / 'crypto'
            store.write_json(base / 'live_scores.json', doc)
            (base / 'sentinel').write_bytes(b'unchanged')
            before = {p: p.read_bytes() for p in Path(root).rglob('*') if p.is_file()}
            replace = store.os.replace
            def atomic(source, destination):
                self.assertEqual(Path(source).parent, Path(destination).parent)
                self.assertEqual(json.loads(Path(source).read_text())['version'], 1)
                replace(source, destination)
            with patch.object(qt.panel, 'load_panels', side_effect=AssertionError('must reuse panels')), \
                 patch.object(store.os, 'replace', side_effect=atomic) as swaps:
                path = qt.write_targets('crypto', root, panels=panels, now=pd.Timestamp(doc['as_of']))
            swaps.assert_called_once()
            self.assertEqual(path, base / 'quant_targets.json')
            self.assertEqual(json.loads(path.read_text())['status'], 'ok')
            self.assertEqual({p for p in Path(root).rglob('*') if p.is_file()}, set(before) | {path})
            self.assertTrue(all(p.read_bytes() == value for p,value in before.items()))
            self.assertEqual(list(base.glob('.quant_targets.json.*')), [])
            snapshot = path.read_bytes()
            with patch.object(store.os, 'replace', side_effect=OSError('test atomic failure')):
                with self.assertRaises(OSError):
                    qt.write_targets('crypto', root, panels=panels, now=pd.Timestamp(doc['as_of']))
            self.assertEqual(path.read_bytes(), snapshot)
            self.assertEqual(list(base.glob('.quant_targets.json.*')), [])

    def test_missing_file_writes_no_factors_without_loading_panels(self):
        with tempfile.TemporaryDirectory() as root, \
             patch.object(qt.panel, 'load_panels', side_effect=AssertionError('no panels needed')):
            path = qt.write_targets('us', root, now='2026-11-01')
            doc = json.loads(path.read_text())
            self.assertEqual(doc['status'], 'no_validated_factors')
            self.assertIsNone(doc['horizon'])
            self.assertEqual(doc['weights'], {})
            self.assertEqual(list(Path(root).rglob('*.json')), [path])

    def test_load_panels_when_needed_and_print_cli_never_writes(self):
        panels, doc = fixture()
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'crypto/live_scores.json'
            store.write_json(path, doc)
            before = path.read_bytes()
            with patch.object(qt.panel, 'load_panels', return_value=panels) as load, \
                 patch.object(qt, '_utc', return_value=pd.Timestamp(doc['as_of'], tz='UTC')):
                qt.write_targets('crypto', root)
            load.assert_called_once_with('crypto', root)
            (path.parent / 'quant_targets.json').unlink()
            for flag in ([], ['--print']):
                with patch.object(qt.panel, 'load_panels', return_value=panels), \
                     patch.object(store, 'write_json', side_effect=AssertionError('read-only CLI')), \
                     contextlib.redirect_stdout(io.StringIO()) as output:
                    qt.main(['--market', 'crypto', '--root', root] + flag)
                self.assertEqual(json.loads(output.getvalue())['market'], 'crypto')
                self.assertEqual(path.read_bytes(), before)
                self.assertEqual(list(path.parent.iterdir()), [path])

    def test_real_exporter_schema_is_consumed_without_factor_ids_or_extensions(self):
        panels, _ = fixture()
        factor = dict(id='f_close', expr='$close', direction=1, horizon=3,
                      status='accepted', train={'t': 5.})
        now = panels.mask.index[-1] + dt.timedelta(days=1)
        with tempfile.TemporaryDirectory() as root:
            snapshot = live_scores.export_live_scores('crypto', [factor], panels, root=root, now=now)
            self.assertNotIn('factor_ids', snapshot['horizons']['3'])
            result = qt.build_targets('crypto', snapshot, panels, now=now)
        self.assertEqual(result['status'], 'ok')
        self.assertEqual(result['run_id'], snapshot['run_id'])
        self.assertEqual(result['factor_ids'], [])  # do not guess from zoo or another run

    def test_miner_quant_failure_does_not_affect_live_export_or_main_run(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root)
            stderr = io.StringIO()
            now = pd.Timestamp('2021-10-12', tz='UTC').to_pydatetime()
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]), \
                 patch.object(miner.time, 'monotonic', return_value=0.), \
                 patch.object(live_scores, 'export_live_scores', wraps=live_scores.export_live_scores) as export, \
                 patch.object(qt, 'write_targets', side_effect=RuntimeError('quant failed')) as write, \
                 contextlib.redirect_stderr(stderr):
                output = miner.Miner(root).run_market('crypto', n_mutations=0, n_llm=0,
                    budget_s=120, seed=1, use_llm=False, now=now)
            export.assert_called_once()
            write.assert_called_once_with('crypto', Path(root), panels=export.call_args.args[2])
            self.assertEqual(output['market'], 'crypto')
            self.assertTrue((Path(root) / 'crypto/live_scores.json').is_file())
            self.assertIsNone(store.read_jsonl(Path(root) / 'runs.jsonl')[-1]['error'])
            self.assertIn('quant_targets export failed for crypto: RuntimeError: quant failed', stderr.getvalue())


class DescriptiveStudyTests(unittest.TestCase):
    def test_frozen_loader_pins_horizon_direction_and_ignores_status(self):
        with tempfile.TemporaryDirectory() as root:
            base = Path(root) / 'crypto'
            store.write_json(base / 'ensemble.json', {'horizons': {'3': {'factor_ids': ['a', 'b']}}})
            factors = [dict(id='a', horizon=3, expr='$close', direction=-1, status='rejected'),
                       dict(id='a', horizon=1, expr='$volume', direction=1),
                       dict(id='b', horizon=3, expr='$open', direction=1),
                       dict(id='extra', horizon=3, expr='$high', direction=1)]
            store.write_json(base / 'zoo.json', {'factors': factors})
            frozen, hashes = qt._load_frozen(root)
            self.assertEqual([f['id'] for f in frozen], ['a', 'b'])
            self.assertEqual(frozen[0]['direction'], -1)
            self.assertEqual(frozen[0]['expr'], '$close')
            self.assertEqual(len(hashes['ensemble.json']['sha256']), 64)
            self.assertIsNone(hashes['ensemble.json']['generated_at'])

    def test_target_stairs_and_scores_are_causal_and_engine_does_not_drift_rebalance(self):
        from scripts.backtest.quantjourney import RebalancePolicy, run_weights
        panels, doc = fixture()
        close = panels.fields['close']
        panels.fields.update(open=close.copy(), high=close * 1.01, low=close * .99,
                             volume=close * 100, vwap=close.copy(), funding=close * 0 + .0001)
        factors = [dict(id='f', expr='$close', direction=1)]
        dates = close.index[-8:]
        scores = ensemble.ensemble_scores(factors, panels)
        protected, naive, signals = qt._historical_targets(panels, scores, factors, dates)
        pd.testing.assert_series_equal(protected.iloc[0], protected.iloc[1], check_names=False)
        pd.testing.assert_series_equal(protected.iloc[0], protected.iloc[2], check_names=False)
        self.assertEqual([s['as_of'] for s in signals], [d.strftime('%Y-%m-%d') for d in dates[::3]])
        prefix = copy.deepcopy(panels)
        prefix.fields = {k:v.loc[:dates[0]] for k,v in panels.fields.items()}
        prefix.mask = prefix.mask.loc[:dates[0]]
        pd.testing.assert_series_equal(ensemble.ensemble_scores(factors, prefix).iloc[-1], scores.loc[dates[0]])
        changed = copy.deepcopy(panels)
        changed.fields['close'].loc[dates[1]:] *= 1e6
        later_scores = ensemble.ensemble_scores(factors, changed)
        changed_targets = qt._historical_targets(changed, later_scores, factors, dates)[0]
        pd.testing.assert_frame_equal(changed_targets.iloc[:3], protected.iloc[:3])
        # An unchanged target must not generate daily resize orders when prices
        # and NAV drift. Orders on the first next-open only, with day-keyed rates.
        fixed = naive.copy()
        fixed.loc[:] = naive.iloc[0].to_numpy()
        frames, rates, fixed = qt._frames_rates(panels, fixed)
        self.assertTrue(all(hasattr(d, 'year') and not isinstance(d, pd.Timestamp)
                            for values in rates.values() for d in values))
        run = run_weights(frames, fixed, market='crypto', commission=.0005, slippage=.0003,
                          funding=rates, keep_fills=True,
                          policy=RebalancePolicy(frequency=None, rebalance_on_signal_change=True))
        self.assertEqual({f['execution_bar'] for f in run['fills']}, {1})
        self.assertEqual(run['metrics']['rebalance_stats'], {'initial': 1})
        self.assertGreater(run['metrics']['funding_covered'], 0)
        self.assertEqual(run['metrics']['funding_covered'], run['metrics']['funding_observations'])


if __name__ == '__main__':
    unittest.main()
