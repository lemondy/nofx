"""Synthetic scoring-only extension tests; no production data/network access."""
import copy
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import data_fetch, ext_universe as ext, live_scores, run_mining, store
from scripts.factor_mining.expr import evaluate_expr
from scripts.factor_mining.panel import FIELD_NAMES, load_panels
from scripts.factor_mining.tests.synth import planted, write_market


def factor(key, expr='$close', horizon=1, direction=1, **kwargs):
    return dict(id=key, expr=expr, horizon=horizon, direction=direction,
                status='accepted', train={'t': 5.}, family='price_volume') | kwargs


def write_extra(root, symbol, core, *, rows=None, offset=0.):
    dates = core.mask.index if rows is None else core.mask.index[-rows:]
    source = core.mask.columns[0]
    data = [[date.strftime('%Y-%m-%d'), *(float(core.fields[name].at[date, source]) +
              (offset if name != 'volume' else 0.) for name in FIELD_NAMES)] for date in dates]
    store.write_json(Path(root) / 'crypto/ext/daily' / f'{symbol}.json', data)


def legacy_only(snapshot):
    snapshot = copy.deepcopy(snapshot)
    for key in ('factors', 'universe', 'skipped'):
        snapshot.pop(key, None)
    for entry in snapshot['horizons'].values():
        for key in ('detail', 'factor_ids', 'ext_scores'):
            entry.pop(key, None)
    return json.dumps(snapshot, ensure_ascii=False, separators=(',', ':')).encode()


class ExtendedScoresTests(unittest.TestCase):
    def setUp(self):
        self.core = planted(rows=100, columns=15)
        self.now = (self.core.mask.index[-1] + pd.Timedelta(days=1)).tz_localize('UTC').to_pydatetime()
        self.factors = [factor('f_close', val={'t': 5.06, 'ic': .0449},
                               forward={'verdict': 'inconclusive'}),
                        factor('f_rank', 'CSRank($close)', direction=-1),
                        factor('f_failed', '$volume', forward={'verdict': 'failed'}),
                        factor('f_h3', '$volume', horizon=3)]

    def test_core_scores_bitwise_identical_and_incremental_schema_correct(self):
        core = self.core
        core.mask.iloc[:, -1] = False
        before_fields = copy.deepcopy(core.fields)
        before_mask = core.mask.copy()
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'crypto/live_scores.json'
            baseline = live_scores.export_live_scores('crypto', self.factors, core, root=root, now=self.now)
            before = path.read_bytes()
            write_extra(root, 'XUSDT', core, offset=15.)
            write_extra(root, 'SHORTUSDT', core, rows=59)
            write_extra(root, 'STALEUSDT', core)
            stale = Path(root) / 'crypto/ext/daily/STALEUSDT.json'
            store.write_json(stale, store.read_json(stale)[:-1])
            symbols = ['XUSDT', 'SHORTUSDT', 'STALEUSDT', 'NONEUSDT']
            scoring, names, skipped = live_scores.load_scoring_panels(
                core, symbols, root=root, as_of=baseline['as_of'], now=self.now)
            pd.testing.assert_frame_equal(scoring.mask[core.mask.columns], before_mask)
            self.assertFalse(scoring.mask['XUSDT'].any())
            output = live_scores.extend_live_scores(self.factors, core, symbols, root=root, now=self.now)
            self.assertEqual(legacy_only(output), before)
            # A new export has a new identity, with all other fields unchanged.
            repeated = live_scores.export_live_scores('crypto', self.factors, core, root=root, now=self.now)
            self.assertEqual(json.loads(path.read_bytes()), repeated)
            self.assertNotEqual(repeated['run_id'], baseline['run_id'])
            self.assertEqual({k: v for k, v in repeated.items() if k != 'run_id'},
                             {k: v for k, v in baseline.items() if k != 'run_id'})
        outside_mask = core.mask.columns[-1]
        self.assertEqual(names, [outside_mask, 'XUSDT'])
        self.assertEqual(skipped, {'SHORTUSDT': 'history<60d', 'STALEUSDT': 'stale', 'NONEUSDT': 'no_data'})
        self.assertEqual(output['skipped'], skipped)
        self.assertEqual(output['version'], 1)
        self.assertEqual(output['universe'], {'core': 14, 'extended': 2})
        entry = output['horizons']['1']
        self.assertEqual(entry['factor_ids'], ['f_close', 'f_rank'])
        self.assertEqual(entry['forward_failed_excluded'], 1)
        self.assertNotIn('f_failed', output['factors'])
        self.assertEqual(output['factors']['f_close'], dict(family='price_volume', horizon=1, direction=1,
            expr='$close', expr_sha256=hashlib.sha256(b'$close').hexdigest()[:12],
            val_t=5.06, val_ic=.0449, forward='inconclusive'))
        self.assertIsNone(output['factors']['f_rank']['val_t'])
        self.assertIsNone(output['factors']['f_rank']['val_ic'])
        self.assertEqual(output['factors']['f_rank']['forward'], 'none')
        at = pd.Timestamp(output['as_of'])
        mask = core.mask.loc[at]
        zs = []
        for f in self.factors[:2]:
            values = evaluate_expr(f['expr'], scoring.fields).loc[at] * f['direction']
            reference = values[mask.index[mask]]
            z = (values - reference.mean()) / reference.std()
            zs.append(z)
            self.assertEqual(entry['detail']['XUSDT'][f['id']], round(z['XUSDT'], 2))
            self.assertEqual(entry['detail'][mask.index[0]][f['id']], round(z[mask.index[0]], 2))
        combined = (zs[0] + zs[1]) / 2
        expected_z = combined['XUSDT']
        expected_pct = (combined[mask.index[mask]] <= expected_z).mean()
        self.assertEqual(entry['ext_scores']['XUSDT'], dict(z=round(expected_z, 4), pct=round(expected_pct, 4)))
        self.assertEqual(set(entry['detail']), set(mask.index[mask]) | {outside_mask, 'XUSDT'})
        self.assertEqual(set(entry['ext_scores']), {outside_mask, 'XUSDT'})
        self.assertEqual(entry['ext_scores'][outside_mask], dict(
            z=round(combined[outside_mask], 4),
            pct=round((combined[mask.index[mask]] <= combined[outside_mask]).mean(), 4)))
        for name in core.fields:
            pd.testing.assert_frame_equal(core.fields[name], before_fields[name])
        pd.testing.assert_frame_equal(core.mask, before_mask)

    def test_existing_non_mask_column_scores_without_ext_files_or_changing_core_scores(self):
        core = self.core
        symbol = core.mask.columns[-1]
        # Historically eligible, but outside the liquidity mask on as_of.
        core.mask.loc[core.mask.index[-1], symbol] = False
        without = copy.deepcopy(core)
        without.mask = without.mask.drop(columns=symbol)
        without.fields = {name: frame.drop(columns=symbol) for name, frame in without.fields.items()}
        fs = [factor('f_close'), factor('f_volume', '$volume'), factor('f_h3', '$volume', horizon=3)]
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'crypto/live_scores.json'
            live_scores.export_live_scores('crypto', fs, without, root=root, now=self.now)
            before = store.read_json(path)
            live_scores.export_live_scores('crypto', fs, core, root=root, now=self.now)
            after = store.read_json(path)
            self.assertNotEqual(after.pop('run_id'), before.pop('run_id'))
            self.assertEqual(after, before)
            before = path.read_bytes()
            # A repeated core symbol in the extra list must neither read an
            # ext file nor duplicate scores. Mask-eligible core names stay core.
            symbols = [symbol, symbol, core.mask.columns[0]]
            with patch.object(live_scores, '_read_symbol', side_effect=AssertionError('ext daily I/O')), \
                 patch.object(live_scores, '_read_alt', side_effect=AssertionError('ext alt I/O')):
                scoring, names, skipped = live_scores.load_scoring_panels(
                    core, symbols, root=root, as_of=core.mask.index[-1], now=self.now)
                output = live_scores.extend_live_scores(fs, core, symbols, root=root, now=self.now)
            self.assertFalse((Path(root) / 'crypto/ext').exists())
        self.assertEqual(names, [symbol])
        self.assertEqual(skipped, {})
        self.assertEqual(legacy_only(output), before)
        self.assertEqual(output['universe'], {'core': 14, 'extended': 1})
        for entry in output['horizons'].values():
            self.assertEqual(set(entry['ext_scores']), {symbol})
            self.assertIn(symbol, entry['detail'])
            self.assertNotIn(symbol, entry['scores'])
        pd.testing.assert_frame_equal(scoring.mask, core.mask)
        for name in core.fields:
            pd.testing.assert_frame_equal(scoring.fields[name], core.fields[name])

    def test_existing_non_mask_history_counts_observations_and_requires_as_of_close(self):
        core = self.core
        short, stale, empty, boundary = core.mask.columns[-4:]
        core.mask.loc[:, [short, stale, empty, boundary]] = False
        close = core.fields['close']
        # Missing days do not count as history; exactly 60 observations pass.
        close.loc[close.index[:-59], short] = np.nan
        close.loc[close.index[-1], stale] = np.nan
        close.loc[:, empty] = np.nan
        close.loc[close.index[:-60], boundary] = np.nan
        # A future close cannot turn 59 observations into 60 at as_of.
        close.loc[pd.Timestamp(self.now).tz_localize(None), short] = 100.
        core.fields = {name: frame.reindex(close.index) for name, frame in core.fields.items()}
        core.mask = core.mask.reindex(close.index, fill_value=False)
        fs = [factor('f_close')]
        with tempfile.TemporaryDirectory() as root:
            live_scores.export_live_scores('crypto', fs, core, root=root, now=self.now)
            output = live_scores.extend_live_scores(fs, core, [], root=root, now=self.now)
        self.assertEqual(output['universe'], {'core': 11, 'extended': 1})
        self.assertEqual(output['skipped'], {short: 'history<60d', stale: 'stale', empty: 'no_data'})
        entry = output['horizons']['1']
        self.assertEqual(set(entry['ext_scores']), {boundary})
        self.assertIn(boundary, entry['detail'])
        for symbol in (short, stale, empty, boundary):
            self.assertNotIn(symbol, entry['scores'])
        for symbol in (short, stale, empty):
            self.assertNotIn(symbol, entry['detail'])

    def test_existing_non_mask_column_on_stale_calendar_is_skipped(self):
        core = self.core
        symbol = core.mask.columns[-1]
        core.mask.iloc[-1, -1] = False
        with tempfile.TemporaryDirectory() as root:
            live_scores.export_live_scores('crypto', self.factors, core, root=root, now=self.now)
            output = live_scores.extend_live_scores(self.factors, core, [], root=root,
                                                    now=self.now + dt.timedelta(days=1))
        self.assertEqual(output['universe'], {'core': 14, 'extended': 0})
        self.assertEqual(output['skipped'], {symbol: 'stale'})

    def test_existing_non_mask_columns_share_ext_finite_factor_threshold(self):
        core = self.core
        good, thin = core.mask.columns[-2:]
        core.mask.iloc[-1, -2:] = False
        core.fields['funding'] = core.fields['close'] * .00001
        core.fields['premium'] = core.fields['volume'] * .000001
        core.fields['premium'].loc[core.mask.index[-1], [good, thin]] = np.nan
        core.fields['funding'].loc[core.mask.index[-1], thin] = np.nan
        fs = [factor('f_close'), factor('f_fund', '$funding'), factor('f_prem', '$premium')]
        with tempfile.TemporaryDirectory() as root:
            live_scores.export_live_scores('crypto', fs, core, root=root, now=self.now)
            output = live_scores.extend_live_scores(fs, core, [], root=root, now=self.now)
            self.assertEqual(output['universe'], {'core': 13, 'extended': 1})
            self.assertEqual(output['skipped'], {thin: 'factors<half'})
            self.assertEqual(set(output['horizons']['1']['ext_scores']), {good})
            self.assertEqual(set(output['horizons']['1']['detail'][good]), {'f_close', 'f_fund'})
            live_scores.export_live_scores('crypto', fs[:2], core, root=root, now=self.now)
            half = live_scores.extend_live_scores(fs[:2], core, [], root=root, now=self.now)
            self.assertEqual(set(half['horizons']['1']['ext_scores']), {good, thin})
            self.assertEqual(half['skipped'], {})

    def test_alt_alignment_finite_factor_counts_half_threshold_and_no_self_in_percentile(self):
        core = self.core
        core.fields['funding'] = core.fields['close'] * .00001
        core.fields['premium'] = core.fields['volume'] * .000001
        fs = [factor('f_close'), factor('f_fund', '$funding'), factor('f_prem', '$premium')]
        with tempfile.TemporaryDirectory() as root:
            write_extra(root, 'GOODUSDT', core)
            write_extra(root, 'THINUSDT', core)
            # 60 valid rows is enough, and alt values stay on their actual day.
            write_extra(root, 'BOUNDARYUSDT', core, rows=60)
            date = core.mask.index[-1].strftime('%Y-%m-%d')
            previous = core.mask.index[-2].strftime('%Y-%m-%d')
            for symbol in ('GOODUSDT', 'BOUNDARYUSDT'):
                store.write_json(Path(root) / 'crypto/ext/alt_daily/funding' / f'{symbol}.json',
                                 [[previous, .2], [date, float(core.fields['funding'].iloc[-1, 0])]])
            live_scores.export_live_scores('crypto', fs, core, root=root, now=self.now)
            output = live_scores.extend_live_scores(fs, core, ['GOODUSDT', 'THINUSDT', 'BOUNDARYUSDT'],
                                                    root=root, now=self.now)
            self.assertEqual(output['universe']['extended'], 2)
            self.assertEqual(output['skipped'], {'THINUSDT': 'factors<half'})
            entry = output['horizons']['1']
            self.assertEqual(set(entry['ext_scores']), {'GOODUSDT', 'BOUNDARYUSDT'})
            self.assertEqual(set(entry['detail']['GOODUSDT']), {'f_close', 'f_fund'})
            # Exactly half (one of two) is allowed; <= core CDF handles ties and
            # excludes the extension itself. One factor equals a core value.
            fs = fs[:1]
            live_scores.export_live_scores('crypto', fs, core, root=root, now=self.now)
            one = live_scores.extend_live_scores(fs, core, ['THINUSDT'], root=root, now=self.now)
            values = core.fields['close'].iloc[-1]
            pct = (values <= values.iloc[0]).mean()
            self.assertEqual(one['horizons']['1']['ext_scores']['THINUSDT']['pct'], round(pct, 4))
            fs.append(factor('f_fund', '$funding'))
            live_scores.export_live_scores('crypto', fs, core, root=root, now=self.now)
            half = live_scores.extend_live_scores(fs, core, ['THINUSDT'], root=root, now=self.now)
            self.assertIn('THINUSDT', half['horizons']['1']['ext_scores'])

    def test_stale_base_calendar_and_future_rows_cannot_make_ext_fresh_or_long_enough(self):
        with tempfile.TemporaryDirectory() as root:
            write_extra(root, 'STALEUSDT', self.core)
            live_scores.export_live_scores('crypto', self.factors, self.core, root=root, now=self.now)
            output = live_scores.extend_live_scores(self.factors, self.core, ['STALEUSDT'], root=root,
                                                    now=self.now + dt.timedelta(days=1))
            self.assertEqual(output['skipped'], {'STALEUSDT': 'stale'})
            self.assertEqual(output['universe']['extended'], 0)
            write_extra(root, 'FUTUREUSDT', self.core, rows=59)
            path = Path(root) / 'crypto/ext/daily/FUTUREUSDT.json'
            rows = store.read_json(path)
            future = rows[-1].copy()
            future[0] = self.now.strftime('%Y-%m-%d')
            store.write_json(path, rows + [future])
            output = live_scores.extend_live_scores(self.factors, self.core, ['FUTUREUSDT'], root=root,
                                                    now=self.now)
            self.assertEqual(output['skipped'], {'FUTUREUSDT': 'history<60d'})

    def test_selected_ids_failed_guard_metadata_truncation_and_finite_json(self):
        fs = [factor('f1', '($close)' + ' ' * 140),
              factor('f2', forward={'verdict': 'failed'}), factor('f3', '$volume')]
        ids = {1: ['f1', 'f2', 'missing']}
        with tempfile.TemporaryDirectory() as root:
            write_extra(root, 'XUSDT', self.core)
            live_scores.export_live_scores('crypto', fs, self.core, root=root, now=self.now, selected_ids=ids)
            output = live_scores.extend_live_scores(fs, self.core, ['XUSDT'], root=root,
                                                    now=self.now, selected_ids=ids)
            self.assertEqual(output['horizons']['1']['factor_ids'], ['f1'])
            self.assertEqual(len(output['factors']['f1']['expr']), 120)
            self.assertEqual(output['factors']['f1']['expr_sha256'],
                             hashlib.sha256(fs[0]['expr'].encode('utf-8')).hexdigest()[:12])
            self.assertNotEqual(output['factors']['f1']['expr_sha256'],
                                hashlib.sha256(output['factors']['f1']['expr'].encode('utf-8')).hexdigest()[:12])
            serialized = (Path(root) / 'crypto/live_scores.json').read_text()
            self.assertNotIn('NaN', serialized)
            self.assertNotIn('Infinity', serialized)

    def test_any_extension_failure_preserves_complete_previous_snapshot(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'crypto/live_scores.json'
            live_scores.export_live_scores('crypto', self.factors, self.core, root=root, now=self.now)
            before = path.read_bytes()
            for patcher in (patch.object(live_scores, 'evaluate_expr', side_effect=RuntimeError('bad factor')),
                            patch.object(store.os, 'replace', side_effect=OSError('disk full'))):
                with patcher:
                    self.assertIsNone(live_scores.extend_live_scores(self.factors, self.core, [], root=root,
                                                                    now=self.now, log=lambda *_: None))
                self.assertEqual(path.read_bytes(), before)
                self.assertEqual(list(path.parent.glob('.live_scores.json.*')), [])
            self.assertIsNone(live_scores.extend_live_scores(self.factors, self.core, [], root=root,
                now=self.now, not_before=self.now + dt.timedelta(seconds=1), log=lambda *_: None))
            self.assertEqual(path.read_bytes(), before)

    def test_same_id_across_horizons_keeps_each_set_of_metadata(self):
        fs = [factor('f_same', val={'t': 1., 'ic': .01}),
              factor('f_same', horizon=3, direction=-1, val={'t': 3., 'ic': .03}),
              factor('f_same', horizon=7, val={'t': 7., 'ic': .07})]
        with tempfile.TemporaryDirectory() as root:
            live_scores.export_live_scores('crypto', fs, self.core, root=root, now=self.now)
            output = live_scores.extend_live_scores(fs, self.core, [], root=root, now=self.now)
        meta = output['factors']['f_same']
        self.assertEqual(meta['horizon'], 1)
        self.assertEqual(meta['val_t'], 1.)
        self.assertEqual(meta['by_horizon']['3']['direction'], -1)
        self.assertEqual(meta['by_horizon']['7']['val_t'], 7.)
        self.assertNotIn('by_horizon', meta['by_horizon']['1'])

    def test_disabled_is_original_bytes_and_no_extension_io(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / 'crypto/live_scores.json'
            live_scores.export_live_scores('crypto', self.factors, self.core, root=root, now=self.now)
            before = path.read_bytes()
            with patch.dict(os.environ, FM_EXT_UNIVERSE='0'), \
                 patch.object(ext, 'read_extra_symbols', side_effect=AssertionError('extension I/O')), \
                 patch.object(live_scores.store, 'read_json', side_effect=AssertionError('extension I/O')):
                self.assertIsNone(ext.run_extension(root))
                self.assertIsNone(live_scores.extend_live_scores(self.factors, self.core, [], root=root))
            self.assertEqual(path.read_bytes(), before)
            self.assertEqual(legacy_only(json.loads(before)), before)
            self.assertFalse((Path(root) / 'crypto/ext').exists())


class ExtensionHookTests(unittest.TestCase):
    def test_post_mining_hook_offline_and_failure_never_fails_nightly(self):
        for extension_error in (False, True):
            with self.subTest(error=extension_error), tempfile.TemporaryDirectory() as root:
                with patch.object(run_mining.Miner, 'run_market', return_value={}) as mine, \
                     patch.object(ext, 'run_extension', side_effect=OSError('extension unavailable')
                                  if extension_error else None) as hook:
                    result = run_mining.main(['--market', 'crypto', '--root', root, '--no-fetch', '--no-llm'])
                self.assertEqual(result, 0)
                mine.assert_called_once()
                hook.assert_called_once()
                self.assertFalse(hook.call_args.kwargs['fetch'])
        with tempfile.TemporaryDirectory() as root, \
             patch.object(run_mining.Miner, 'run_market', return_value={}), \
             patch.object(ext, 'run_extension') as hook:
            self.assertEqual(run_mining.main(['--market', 'us', '--root', root, '--no-fetch', '--no-llm']), 0)
            hook.assert_not_called()
        with tempfile.TemporaryDirectory() as root, \
             patch.dict(os.environ, FM_EXT_UNIVERSE='0'), \
             patch.object(run_mining.Miner, 'run_market', return_value={}), \
             patch.object(ext, 'run_extension') as hook:
            self.assertEqual(run_mining.main(['--market', 'crypto', '--root', root, '--no-fetch', '--no-llm']), 0)
            hook.assert_not_called()

    def test_run_extension_no_fetch_uses_cache_and_never_constructs_http(self):
        with tempfile.TemporaryDirectory() as root:
            write_market(root, rows=100, columns=70)
            core = load_panels('crypto', root)
            now = (core.mask.index[-1] + pd.Timedelta(days=1)).tz_localize('UTC').to_pydatetime()
            fs = [factor('f1')]
            store.write_json(Path(root) / 'crypto/zoo.json', {'factors': fs})
            store.write_json(Path(root) / 'crypto/ext/extra_symbols.json', {'symbols': ['XUSDT']})
            write_extra(root, 'XUSDT', core)
            live_scores.export_live_scores('crypto', fs, core, root=root, now=now)
            with patch.object(data_fetch, 'Http', side_effect=AssertionError('network forbidden')), \
                 patch.object(ext, '_frequencies', side_effect=AssertionError('DB forbidden')):
                output = ext.run_extension(root, fetch=False, now=now)
            self.assertEqual(output['universe'], {'core': 60, 'extended': 11})

    def test_universe_and_fetch_share_budget_and_timeout_still_scores_cached_data(self):
        elapsed, calls = [0.], []
        with tempfile.TemporaryDirectory() as root:
            write_market(root, rows=100, columns=15)
            core = load_panels('crypto', root)
            now = (core.mask.index[-1] + dt.timedelta(days=1)).tz_localize('UTC').to_pydatetime()
            fs = [factor('f1')]
            store.write_json(Path(root) / 'crypto/zoo.json', {'factors': fs})
            write_extra(root, 'XUSDT', core)
            live_scores.export_live_scores('crypto', fs, core, root=root, now=now)
            before = (Path(root) / 'crypto/ext/daily/XUSDT.json').read_bytes()
            untouched = {p: p.read_bytes() for p in Path(root).rglob('*.json')
                         if 'ext' not in p.relative_to(root).parts and p.name != 'live_scores.json'}

            class SlowHttp:
                def get(self, url, params=None):
                    calls.append(url)
                    if url.endswith('/exchangeInfo'):
                        elapsed[0] = 899
                        return {'symbols': [dict(symbol='XUSDT', status='TRADING', contractType='PERPETUAL',
                            quoteAsset='USDT', underlyingType='COIN',
                            onboardDate=int((now - dt.timedelta(days=100)).timestamp() * 1000))]}
                    elapsed[0] += 2
                    return []

            with patch.object(ext, '_frequencies', return_value={'XUSDT': 3}):
                output = ext.run_extension(root, http=SlowHttp(), now=now,
                                           clock=lambda: elapsed[0], log=lambda *_: None)
            self.assertEqual(len(calls), 2)
            self.assertEqual(output['universe']['extended'], 1)
            self.assertTrue(store.read_json(Path(root) / 'crypto/ext/data_meta.json')['timed_out'])
            self.assertEqual((Path(root) / 'crypto/ext/daily/XUSDT.json').read_bytes(), before)
            for path, contents in untouched.items():
                self.assertEqual(path.read_bytes(), contents)


if __name__ == '__main__':
    unittest.main()
