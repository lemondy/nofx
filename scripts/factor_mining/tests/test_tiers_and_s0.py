"""Offline unit tests for size/liquidity tier stratification and S0 Hidden-Sharpe gate."""
from __future__ import annotations

import contextlib
import copy
import datetime as dt
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.factor_mining import (
    evaluate,
    live_scores,
    miner,
    panel,
    quant_targets as qt,
    run_mining,
    s0_diagnostics,
    store,
    tiers,
)
from scripts.factor_mining.panel import Panels
from scripts.factor_mining.tests.synth import SIGNAL_EXPR, SIGNAL_SEED


FROZEN_EVALUATE_SHA256 = '78f7012946777010a3f7a73517bf90225352aea3b5005527843e186bde3a5454'


def _make_tiered_panels(
    market: str = 'crypto',
    rows: int = 120,
    mega_names: list[str] | None = None,
    mid_names: list[str] | None = None,
    etf_names: list[str] | None = None,
    seed: int = 42,
) -> Panels:
    rng = np.random.default_rng(seed)
    index = pd.date_range('2025-01-01', periods=rows, freq='B' if market == 'us' else 'D')
    mega_names = mega_names if mega_names is not None else [f'MEGA{i:02}' for i in range(20)]
    mid_names = mid_names if mid_names is not None else [f'MID{i:02}' for i in range(20)]
    etf_names = etf_names if etf_names is not None else (['SPY', 'SMH'] if market == 'us' else [])
    columns = mega_names + mid_names + etf_names
    n_cols = len(columns)

    signal = rng.normal(size=(rows, n_cols))
    increments = rng.normal(0.0, 0.004, (rows, n_cols))
    increments[2:] += 0.015 * signal[:-2]
    opens = 100.0 * np.exp(np.cumsum(increments, axis=0))
    closes = opens * np.exp(0.02 * signal)
    highs = np.maximum(opens, closes) * 1.005
    lows = np.minimum(opens, closes) * 0.995

    thr = tiers.default_threshold(market)
    volumes = np.zeros((rows, n_cols), dtype=float)
    for j, col in enumerate(columns):
        if col in mega_names or col in etf_names:
            target_dv = thr * 2.5
        else:
            target_dv = thr * 0.15
        volumes[:, j] = target_dv / closes[:, j]

    fields = {
        name: pd.DataFrame(arr, index=index, columns=columns)
        for name, arr in (
            ('open', opens),
            ('high', highs),
            ('low', lows),
            ('close', closes),
            ('volume', volumes),
            ('vwap', closes.copy()),
        )
    }
    mask = pd.DataFrame(True, index=index, columns=columns, dtype=bool)
    bench = pd.Series(100.0 * np.exp(np.cumsum(rng.normal(0.0, 0.001, rows))), index=index)
    return Panels(
        fields,
        mask,
        bench.copy() if market == 'us' else None,
        bench.copy() if market == 'us' else None,
        {'market': market},
    )


def _write_tiered_market(
    root: str | Path,
    market: str = 'crypto',
    rows: int = 320,
    mega_count: int = 16,
    mid_count: int = 16,
    etf_names: list[str] | None = None,
    seed: int = 19,
) -> Panels:
    mega = [f'MEGA{i:02}' for i in range(mega_count)]
    mid = [f'MID{i:02}' for i in range(mid_count)]
    etfs = etf_names if etf_names is not None else (['SMH'] if market == 'us' else [])
    panels = _make_tiered_panels(market=market, rows=rows, mega_names=mega, mid_names=mid, etf_names=etfs, seed=seed)
    base = Path(root) / market / 'daily'
    base.mkdir(parents=True, exist_ok=True)
    for symbol in panels.fields['close'].columns:
        data = [
            [
                d.strftime('%Y-%m-%d'),
                *(float(panels.fields[f].at[d, symbol]) for f in ('open', 'high', 'low', 'close', 'volume', 'vwap')),
            ]
            for d in panels.mask.index
        ]
        (base / f'{symbol}.json').write_text(json.dumps(data), encoding='utf-8')
    if market == 'us':
        spy_rows = [
            [d.strftime('%Y-%m-%d'), float(p), float(p * 1.01), float(p * 0.99), float(p), 5_000_000.0, float(p)]
            for d, p in panels.benchmark_open.items()
        ]
        (base / 'SPY.json').write_text(json.dumps(spy_rows), encoding='utf-8')
    return panels


class TiersTests(unittest.TestCase):
    def test_thresholds_and_validation(self):
        self.assertEqual(tiers.VALID_TIERS, ('all', 'mega_large', 'mid_small'))
        self.assertEqual(tiers.US_MEGA_LARGE_ADV20, 500_000_000.0)
        self.assertEqual(tiers.CRYPTO_MEGA_LARGE_ADV20, 100_000_000.0)
        for etf in ('SPY', 'QQQ', 'SMH', 'EWY', 'DRAM'):
            self.assertIn(etf, tiers.US_ETFS)
        with self.assertRaises(ValueError):
            tiers.validate_tier('micro_cap')
        with self.assertRaises(ValueError):
            tiers.default_threshold('equities')

    def test_classify_tier_frame_and_tier_mask_us_and_crypto(self):
        us_panels = _make_tiered_panels(
            market='us', rows=30, mega_names=['AAPL', 'NVDA'], mid_names=['ACME'], etf_names=['SPY', 'SMH']
        )
        us_panels.mask.iloc[0, :] = False  # unmasked row must yield '' and False

        labels = tiers.classify_tier_frame(us_panels, 'us')
        self.assertTrue((labels.iloc[0] == '').all())
        self.assertEqual(labels.loc[labels.index[-1], 'SPY'], 'etf')
        self.assertEqual(labels.loc[labels.index[-1], 'SMH'], 'etf')
        self.assertEqual(labels.loc[labels.index[-1], 'AAPL'], 'mega_large')
        self.assertEqual(labels.loc[labels.index[-1], 'ACME'], 'mid_small')

        mega_no_etf = tiers.tier_mask(us_panels, 'us', 'mega_large')
        self.assertTrue(mega_no_etf.loc[mega_no_etf.index[-1], 'AAPL'])
        self.assertFalse(mega_no_etf.loc[mega_no_etf.index[-1], 'SMH'])
        self.assertFalse(mega_no_etf.loc[mega_no_etf.index[-1], 'ACME'])
        self.assertFalse(mega_no_etf.iloc[0].any())

        mega_with_etf = tiers.tier_mask(us_panels, 'us', 'mega_large', include_etf_in_mega=True)
        self.assertTrue(mega_with_etf.loc[mega_with_etf.index[-1], 'SMH'])

        mid_mask = tiers.tier_mask(us_panels, 'us', 'mid_small')
        self.assertTrue(mid_mask.loc[mid_mask.index[-1], 'ACME'])
        self.assertFalse(mid_mask.loc[mid_mask.index[-1], 'SPY'])
        self.assertFalse(mid_mask.loc[mid_mask.index[-1], 'AAPL'])

        # Crypto does not apply US ETF isolation.
        cr_panels = _make_tiered_panels(
            market='crypto', rows=30, mega_names=['BTC', 'SMH'], mid_names=['ALT'], etf_names=[]
        )
        cr_labels = tiers.classify_tier_frame(cr_panels, 'crypto')
        self.assertEqual(cr_labels.loc[cr_labels.index[-1], 'SMH'], 'mega_large')
        self.assertEqual(cr_labels.loc[cr_labels.index[-1], 'ALT'], 'mid_small')

    def test_zero_lookahead_adv20_and_classify_as_of(self):
        panels = _make_tiered_panels(
            market='crypto', rows=40, mega_names=['BTC'], mid_names=['SHIFT'], etf_names=[]
        )
        # Keep SHIFT below $100M through day 24, then spike to $5B from day 25 onward.
        cutoff = panels.mask.index[24]
        panels.fields['volume'].loc[:cutoff, 'SHIFT'] = 10_000.0
        panels.fields['close'].loc[:cutoff, 'SHIFT'] = 100.0
        panels.fields['vwap'].loc[:cutoff, 'SHIFT'] = 100.0
        panels.fields['volume'].loc[panels.mask.index[25]:, 'SHIFT'] = 5e9 / 100.0

        as_of_before = tiers.classify_symbols_as_of(panels, 'crypto', cutoff)
        self.assertEqual(as_of_before['SHIFT']['tier'], 'mid_small')
        self.assertLess(as_of_before['SHIFT']['adv20'], tiers.CRYPTO_MEGA_LARGE_ADV20)

        # Corrupting all future bars after cutoff must not alter labels at or before cutoff.
        mutated = copy.deepcopy(panels)
        mutated.fields['volume'].loc[panels.mask.index[25]:, :] = 1e15
        mutated.fields['close'].loc[panels.mask.index[25]:, :] = np.nan
        as_of_mutated = tiers.classify_symbols_as_of(mutated, 'crypto', cutoff)
        self.assertEqual(as_of_mutated, as_of_before)
        pd.testing.assert_frame_equal(
            tiers.classify_tier_frame(panels, 'crypto').loc[:cutoff],
            tiers.classify_tier_frame(mutated, 'crypto').loc[:cutoff],
        )


class PanelTierTests(unittest.TestCase):
    def test_load_panels_tier_all_unchanged_and_filtered_tiers(self):
        with tempfile.TemporaryDirectory() as root:
            _write_tiered_market(root, market='us', rows=90, mega_count=4, mid_count=4, etf_names=['SMH'])
            default_p = panel.load_panels('us', root)
            all_p = panel.load_panels('us', root, tier='all')
            mega_p = panel.load_panels('us', root, tier='mega_large')
            mid_p = panel.load_panels('us', root, tier='mid_small')

        pd.testing.assert_frame_equal(default_p.mask, all_p.mask)
        self.assertEqual(default_p.meta, all_p.meta)
        self.assertNotIn('tier', all_p.meta)

        self.assertEqual(mega_p.meta['tier'], 'mega_large')
        self.assertEqual(mid_p.meta['tier'], 'mid_small')
        self.assertEqual(mega_p.meta['universe'], 4)
        self.assertEqual(mid_p.meta['universe'], 4)
        self.assertFalse(mega_p.mask['SMH'].any())
        self.assertFalse(mid_p.mask['SMH'].any())
        self.assertTrue(mega_p.mask['MEGA00'].iloc[-1])
        self.assertFalse(mega_p.mask['MID00'].iloc[-1])
        self.assertTrue(mid_p.mask['MID00'].iloc[-1])
        self.assertFalse(mid_p.mask['MEGA00'].iloc[-1])
        with self.assertRaises(ValueError):
            panel.load_panels('us', '.', tier='invalid')


class S0DiagnosticsTests(unittest.TestCase):
    def test_build_managed_factor_returns_is_train_only(self):
        panels = _make_tiered_panels(market='crypto', rows=650, mega_names=[f'M{i:02}' for i in range(15)],
                                     mid_names=[f'S{i:02}' for i in range(15)])
        horizon = 3
        splits = panel.make_splits(panels.mask.index, horizon, panels.windows)
        factors = [
            {'id': 'f_sig', 'expr': SIGNAL_EXPR, 'horizon': horizon, 'direction': 1},
            {'id': 'f_mom', 'expr': 'Mean($close,5)/Mean($close,20)-1', 'horizon': horizon, 'direction': 1},
        ]
        rets_clean = s0_diagnostics.build_managed_factor_returns(factors, panels, horizon, market='crypto', tier='all')
        self.assertGreaterEqual(len(rets_clean), 50)
        self.assertTrue((rets_clean.index <= panels.mask.index[splits['train']][-1]).all())

        # Poison every bar in val and holdout; train-only managed returns must stay identical.
        poisoned = copy.deepcopy(panels)
        val_or_holdout = splits['val'] | splits['holdout']
        for name in poisoned.fields:
            poisoned.fields[name].loc[val_or_holdout] = 1e9 if name != 'close' else np.nan
        rets_poisoned = s0_diagnostics.build_managed_factor_returns(
            factors, poisoned, horizon, market='crypto', tier='all'
        )
        pd.testing.assert_frame_equal(rets_clean, rets_poisoned)

        # Verify tier filtering isolates mega_large and mid_small.
        rets_mega = s0_diagnostics.build_managed_factor_returns(
            factors, panels, horizon, market='crypto', tier='mega_large'
        )
        rets_mid = s0_diagnostics.build_managed_factor_returns(
            factors, panels, horizon, market='crypto', tier='mid_small'
        )
        self.assertFalse(rets_mega.empty)
        self.assertFalse(rets_mid.empty)
        self.assertFalse(np.allclose(rets_mega['f_sig'].to_numpy(), rets_mid['f_sig'].to_numpy()))

    def test_evaluate_s0_gate_insufficient_noise_linear_and_hidden(self):
        # 1. Insufficient data
        small = pd.DataFrame(np.ones((5, 3)))
        res_small = s0_diagnostics.evaluate_s0_gate(small, periods_per_year=121)
        self.assertEqual(res_small['status'], 'insufficient_data')
        self.assertEqual(res_small['verdict'], 'insufficient_data')

        rng = np.random.default_rng(7)
        # 2. Pure zero-mean noise -> fail_noise_overfit
        noise = rng.normal(0.0, 0.02, size=(240, 40))
        res_noise = s0_diagnostics.evaluate_s0_gate(noise, periods_per_year=121)
        self.assertEqual(res_noise['status'], 'ok')
        self.assertEqual(res_noise['verdict'], 'fail_noise_overfit')

        # 3. Planted hidden alpha in small-variance eigenmodes alongside a zero-mean dominant PC
        T, P = 300, 40
        v1 = np.ones((1, P)) / np.sqrt(P)
        pc1 = rng.normal(0.0, 0.60, size=(T, 1))
        pc1 = pc1 - pc1.mean()
        # Hidden alpha orthogonal to v1 in small-variance modes
        v2 = np.tile([1.0, -1.0], P // 2)[None, :] / np.sqrt(P)
        hidden_signal = 0.006 * v2 + rng.normal(0.0, 0.003, size=(T, P))
        planted = pc1 @ v1 + hidden_signal
        res_planted = s0_diagnostics.evaluate_s0_gate(planted, periods_per_year=121)
        self.assertEqual(res_planted['status'], 'ok')
        self.assertEqual(res_planted['verdict'], 'pass_hidden_alpha_detected')
        self.assertGreater(res_planted['theta0_z'], 1.65)
        self.assertGreater(res_planted['theta1'], 0.0)

        # 4. Pure rank-1 dominant linear factor (orthogonal small residual) -> linear_dominant_no_hidden_alpha
        # Construct matrix where top eigenvector carries all the mean/variance and residual eigenvalues ~ 0.
        u = rng.normal(0.0, 1.0, size=(T, 1))
        u = u - u.mean() + 0.5
        v = np.ones((1, P)) / np.sqrt(P)
        rank1 = 0.03 * (u @ v) + rng.normal(0.0, 1e-4, size=(T, P))
        res_lin = s0_diagnostics.evaluate_s0_gate(rank1, periods_per_year=121)
        self.assertEqual(res_lin['status'], 'ok')
        self.assertEqual(res_lin['verdict'], 'linear_dominant_no_hidden_alpha')

    def test_diagnose_market_tiers_seed_fallback_zoo_and_cli(self):
        with tempfile.TemporaryDirectory() as root:
            _write_tiered_market(root, market='crypto', rows=650, mega_count=15, mid_count=15, seed=21)
            report = s0_diagnostics.diagnose_market_tiers('crypto', root, horizon=3, max_factors=15)
            self.assertEqual(report['market'], 'crypto')
            self.assertEqual(report['horizon'], 3)
            self.assertEqual(report['factor_source'], 'seed_fallback')
            self.assertEqual(set(report['tiers']), {'all', 'mega_large', 'mid_small'})
            for tname in ('all', 'mega_large', 'mid_small'):
                self.assertIn('verdict', report['tiers'][tname])
                self.assertIn('universe_as_of', report['tiers'][tname])

            # Test CLI --json
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                rc = s0_diagnostics.main(['--market', 'crypto', '--horizon', '3', '--root', root, '--json'])
            self.assertEqual(rc, 0)
            parsed = json.loads(buf.getvalue())
            self.assertEqual(parsed['market'], 'crypto')
            self.assertEqual(set(parsed['tiers']), {'all', 'mega_large', 'mid_small'})

            # Test 'zoo' source when >= 5 qualified factors exist
            zoo_factors = [
                {'id': f'f{i}', 'horizon': 3, 'status': 'accepted', 'expr': f'Mean($close,{i+2})', 'direction': 1, 'train': {'t': 3.0 + i}}
                for i in range(6)
            ]
            store.write_json(Path(root) / 'crypto/zoo.json', {'factors': zoo_factors})
            report_zoo = s0_diagnostics.diagnose_market_tiers('crypto', root, horizon=3, max_factors=10)
            self.assertEqual(report_zoo['factor_source'], 'zoo')
            self.assertEqual(report_zoo['factor_count'], 6)


class MinerAndCliTierTests(unittest.TestCase):
    def test_llm_propose_tier_prompts_chinese_and_no_val(self):
        prompts = {}

        def fake_runner(workdir, prompt_path, out_path):
            prompts[workdir.name] = Path(prompt_path).read_text(encoding='utf-8')
            out_path.write_text('["$close/$open-1"]', encoding='utf-8')

        with tempfile.TemporaryDirectory() as root:
            for market in ('us', 'crypto'):
                for tier in tiers.VALID_TIERS:
                    key = f'{market}_{tier}'
                    valid, stats = miner.llm_propose(
                        market, '[]', '[]', 5, fake_runner, Path(root) / key, tier=tier
                    )
                    self.assertEqual(valid, ['(($close/$open)-1)'])
                    self.assertIsNone(stats['llm_error'])
                    text = prompts[key]
                    # Critical invariant: test_miner.py forbids any substring 'val'
                    self.assertNotIn('val', text)
                    if tier == 'mid_small':
                        self.assertIn('中小盘/次主流流动性分层', text)
                    elif market == 'us' or tier == 'mega_large':
                        self.assertIn('高效率大盘/超大盘分层', text)

    def test_miner_run_market_tier_isolation_and_cli(self):
        with tempfile.TemporaryDirectory() as root:
            _write_tiered_market(root, market='crypto', rows=650, mega_count=15, mid_count=15, seed=17)
            search = miner.Miner(root)
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                search.run_market(
                    'crypto', n_mutations=2, n_llm=0, budget_s=60, seed=1, use_llm=False, tier='mega_large'
                )
            # Top-level store must stay completely untouched / isolated
            self.assertFalse((Path(root) / 'crypto/zoo.json').exists())
            self.assertFalse((Path(root) / 'crypto/ledger.jsonl').exists())
            tier_dir = Path(root) / 'crypto/tiers/mega_large'
            self.assertTrue((tier_dir / 'zoo.json').exists())
            self.assertTrue((tier_dir / 'ledger.jsonl').exists())
            runs = store.read_jsonl(Path(root) / 'runs.jsonl')
            self.assertEqual(len(runs), 1)
            self.assertEqual(runs[0]['tier'], 'mega_large')

            # CLI --tier mid_small
            with patch.object(miner.zoo, 'seed_factors', return_value=[SIGNAL_SEED]):
                rc = run_mining.main([
                    '--market', 'crypto', '--tier', 'mid_small',
                    '--no-fetch', '--no-llm', '--mutations', '1', '--budget-min', '0.5', '--root', root,
                ])
            self.assertEqual(rc, 0)
            self.assertFalse((Path(root) / 'crypto/zoo.json').exists())
            self.assertTrue((Path(root) / 'crypto/tiers/mid_small/zoo.json').exists())
            all_runs = store.read_jsonl(Path(root) / 'runs.jsonl')
            self.assertEqual([r['tier'] for r in all_runs], ['mega_large', 'mid_small'])


class ConsumptionLayerTierTests(unittest.TestCase):
    def test_quant_targets_crypto_tier_all_legacy_and_tier_filtering(self):
        mega = [f'MEGA{i:02}' for i in range(25)]
        mid = [f'MID{i:02}' for i in range(25)]
        panels = _make_tiered_panels(market='crypto', rows=120, mega_names=mega, mid_names=mid)
        all_cols = list(panels.mask.columns)
        scores = {s: dict(z=float(i), pct=round((i + 1) / len(all_cols), 4)) for i, s in enumerate(all_cols)}
        as_of = panels.mask.index[-1]
        doc = dict(
            version=1,
            market='crypto',
            as_of=as_of.strftime('%Y-%m-%d'),
            generated_at=(as_of + dt.timedelta(days=1)).isoformat(),
            run_id='synth-tier',
            universe_size=len(all_cols),
            horizons={'3': dict(factor_ids=['f_core'], scores=scores)},
        )
        now = as_of + dt.timedelta(days=1)

        res_all = qt.build_targets('crypto', doc, panels, now=now, tier='all')
        self.assertEqual(set(res_all), {
            'version', 'market', 'status', 'reason', 'generated_at', 'as_of',
            'run_id', 'horizon', 'rule', 'factor_ids', 'universe_size',
            'weights', 'excluded', 'pct',
        })
        self.assertEqual(res_all['status'], 'ok')

        res_mega = qt.build_targets('crypto', doc, panels, now=now, tier='mega_large')
        self.assertEqual(res_mega['status'], 'ok')
        self.assertEqual(res_mega['tier'], 'mega_large')
        self.assertEqual(res_mega['universe_size'], 25)
        self.assertTrue(set(res_mega['weights']).issubset(set(mega)))

        res_mid = qt.build_targets('crypto', doc, panels, now=now, tier='mid_small')
        self.assertEqual(res_mid['status'], 'ok')
        self.assertEqual(res_mid['tier'], 'mid_small')
        self.assertEqual(res_mid['universe_size'], 25)
        self.assertTrue(set(res_mid['weights']).issubset(set(mid)))

        with tempfile.TemporaryDirectory() as root:
            summary = qt.summarize_target_tiers(res_all, root=root, panels=panels)
        self.assertEqual(summary['etf']['count'], 0)
        self.assertEqual(summary['unknown']['count'], 0)
        # In res_all, bottom 10 scores are in mega_names (0..9), top 10 in mid_names (40..49)
        self.assertGreater(summary['mega_large']['count'], 0)
        self.assertGreater(summary['mid_small']['count'], 0)
        self.assertAlmostEqual(
            summary['mega_large']['long_gross'] + summary['mid_small']['long_gross'], 0.5, places=4
        )
        self.assertAlmostEqual(
            summary['mega_large']['short_gross'] + summary['mid_small']['short_gross'], 0.5, places=4
        )

    def test_quant_targets_us_tier_filtering_and_summary(self):
        with tempfile.TemporaryDirectory() as root:
            universe = json.loads(qt.US_UNIVERSE.read_text(encoding='utf-8'))['symbols'][:40]
            self.assertIn('SMH', tiers.US_ETFS)
            # Ensure SMH is included if present, or force one ETF name from universe
            etfs = [s for s in universe if s in tiers.US_ETFS]
            non_etfs = [s for s in universe if s not in tiers.US_ETFS]
            mega_names = set(non_etfs[:18])
            mid_names = set(non_etfs[18:])
            # Fill every US_UNIVERSE symbol so _load_us_features reads cleanly without missing warnings
            all_universe = json.loads(qt.US_UNIVERSE.read_text(encoding='utf-8'))['symbols']
            calendar = pd.bdate_range('2025-01-01', periods=290)
            t = np.arange(len(calendar))
            base = Path(root) / 'us/daily'
            base.mkdir(parents=True)
            for idx_s, sym in enumerate(all_universe + ['SPY']):
                if sym in universe or sym == 'SPY':
                    prices = 50.0 * np.exp(0.0003 * (idx_s + 1) * t) * (1.0 + 0.01 * np.sin(t))
                    vol = (tiers.US_MEGA_LARGE_ADV20 * 2.0) / prices if (sym in mega_names or sym in etfs or sym == 'SPY') else (tiers.US_MEGA_LARGE_ADV20 * 0.1) / prices
                    rows = [[str(d.date()), p, p * 1.005, p * 0.995, p, v, p] for d, p, v in zip(calendar, prices, vol)]
                else:
                    rows = []
                (base / f'{sym}.json').write_text(json.dumps(rows), encoding='utf-8')
            mapping = Path(root) / 'perps.json'
            store.write_json(mapping, dict(version=2, symbols=[
                dict(underlying=s, perp_symbol=s + 'USDT') for s in universe
            ]))

            res_default = qt.build_targets('us', root=root, perp_map=mapping, now=calendar[-1])
            self.assertEqual(set(res_default), {
                'version', 'market', 'status', 'reason', 'generated_at', 'as_of',
                'run_id', 'horizon', 'rule', 'factor_ids', 'universe_size',
                'weights', 'excluded', 'pct',
            })
            self.assertEqual(res_default['status'], 'ok')

            res_mega = qt.build_targets('us', root=root, perp_map=mapping, now=calendar[-1], tier='mega_large')
            self.assertEqual(res_mega['status'], 'ok')
            self.assertEqual(res_mega['tier'], 'mega_large')
            held_mega = {k[:-5] for k in res_mega['weights']}
            self.assertTrue(held_mega.issubset(mega_names))
            self.assertFalse(held_mega & tiers.US_ETFS)

            res_mid = qt.build_targets('us', root=root, perp_map=mapping, now=calendar[-1], tier='mid_small')
            self.assertEqual(res_mid['status'], 'ok')
            self.assertEqual(res_mid['tier'], 'mid_small')
            held_mid = {k[:-5] for k in res_mid['weights']}
            self.assertTrue(held_mid.issubset(mid_names))
            self.assertFalse(held_mid & tiers.US_ETFS)

            tier_summary = qt.summarize_target_tiers(res_default, root=root)
            self.assertEqual(
                tier_summary['mega_large']['count'] + tier_summary['mid_small']['count'] + tier_summary['etf']['count'],
                len(res_default['weights']),
            )

            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                qt.main(['--market', 'us', '--root', root, '--perp-map', str(mapping), '--tier', 'mega_large', '--tier-summary'])
            cli_out = json.loads(buf.getvalue())
            self.assertEqual(cli_out['tier'], 'mega_large')
            self.assertIn('tier_summary', cli_out)

    def test_live_scores_export_unchanged_and_tier_summary(self):
        mega = [f'MEGA{i:02}' for i in range(15)]
        mid = [f'MID{i:02}' for i in range(15)]
        panels = _make_tiered_panels(market='crypto', rows=120, mega_names=mega, mid_names=mid)
        factors = [{'id': 'f1', 'horizon': 3, 'status': 'accepted', 'expr': SIGNAL_EXPR, 'direction': 1, 'train': {'t': 4.0}}]
        now = panels.mask.index[-1] + dt.timedelta(days=1)
        with tempfile.TemporaryDirectory() as root:
            snap = live_scores.export_live_scores('crypto', factors, panels, root=root, now=now)
            self.assertIsNotNone(snap)
            self.assertNotIn('factors', snap)
            summary = live_scores.summarize_live_score_tiers('crypto', panels, root=root)
        self.assertEqual(summary['status'], 'ok')
        self.assertEqual(summary['tiers']['mega_large'], 15)
        self.assertEqual(summary['tiers']['mid_small'], 15)
        h3 = summary['horizons']['3']
        self.assertEqual(h3['core']['count'], 30)
        self.assertEqual(h3['extended']['count'], 0)
        self.assertEqual(h3['by_tier']['mega_large']['count'], 15)
        self.assertEqual(h3['by_tier']['mid_small']['count'], 15)


class EvaluateFrozenGuardTest(unittest.TestCase):
    def test_evaluate_py_sha256_is_frozen(self):
        actual = hashlib.sha256(Path(evaluate.__file__).read_bytes()).hexdigest()
        self.assertEqual(actual, FROZEN_EVALUATE_SHA256)
