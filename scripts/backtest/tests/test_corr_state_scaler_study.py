"""Synthetic data only. No real data/ reads, network calls, or historical runs."""
from contextlib import redirect_stdout
from copy import deepcopy
import io
import json
import math
from pathlib import Path
import re
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest import corr_state_scaler_study as s
from scripts.backtest import dvol_sizing_study as sizing
from scripts.backtest import dvol_vol_forecast_study as forecast
from scripts.backtest import evolution_ledger
from scripts.factor_mining import panel


def synthetic_fields(start='2020-11-01', days=130, symbols=22):
    dates = pd.date_range(start, periods=days, name='date')
    rng = np.random.default_rng(917)
    changes = rng.normal(.001, .012, (days, symbols)) + rng.normal(0, .018, (days, 1))
    close = pd.DataFrame(100 * np.exp(np.cumsum(changes, axis=0)), index=dates,
                         columns=[f'COIN{i:03}USDT' for i in range(symbols)])
    return dict(close=close, volume=close * 0 + 100, vwap=close * 0 + 1)


def write_panel(base, fields):
    daily = base / 'daily'
    daily.mkdir(parents=True, exist_ok=True)
    for symbol in reversed(fields['close'].columns):
        rows = []
        for date in fields['close'].index:
            c = fields['close'].at[date, symbol]
            if pd.isna(c):
                continue
            rows.append([date.strftime('%Y-%m-%d'), c, c * 1.1, c * .9, c,
                         fields['volume'].at[date, symbol], fields['vwap'].at[date, symbol]])
        (daily / f'{symbol}.json').write_text(json.dumps(rows), encoding='utf-8')


def fit_rows(start='2021-01-01', days=100):
    rng = np.random.default_rng(48)
    rows = []
    for i in range(days):
        features = np.exp(rng.normal(-2, .4, 4))
        rows.append(dict(time_ms=s.timestamp_ms(start) + i * s.DAY,
                         rv1=features[0], rv7=features[1], rv30=features[2], D=features[3],
                         rho=.4, n_t=20, y=math.exp(-2 + rng.normal(0, .2))))
    return rows


def passing_stats():
    return dict(carriers={name: dict(
        weekly=dict(comparisons=dict(B_minus_C=dict(mean=.1, t=2., first_half_mean=.1, second_half_mean=.1))),
        models={m: dict(es=v) for m, v in (('U', .3), ('B', .2), ('C', .1))}) for name in s.CARRIERS})


class SyntheticTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='corr-state-synthetic-')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        network = patch('urllib.request.OpenerDirector.open', side_effect=AssertionError('network forbidden'))
        network.start()
        self.addCleanup(network.stop)

    def frozen_inputs(self, fields=None):
        base, trades = self.root / 'panel', self.root / 'export/trades.jsonl'
        (base / 'daily').mkdir(parents=True)
        if fields is not None:
            write_panel(base, fields)
        count = len(list((base / 'daily').glob('*.json')))
        for i in range(s.DAILY_FILE_COUNT - count):
            (base / 'daily' / f'EMPTY{i:03}.json').write_bytes(b'[]')
        (base / 'manifest.json').write_bytes(b'{"synthetic":true}')
        (base / 'validation.json').write_bytes(b'{"synthetic":true}')
        trades.parent.mkdir()
        trades.write_text('', encoding='utf-8')
        hashes = {s.DATA_PREFIX + name: s.sha256((base / name).read_bytes())
                  for name in ('manifest.json', 'validation.json')}
        hashes[s.DATA_PREFIX + 'daily/*.json'] = s.sha256(b''.join(
            p.read_bytes() for p in sorted((base / 'daily').glob('*.json'))))
        hashes[s.TRADE_KEY] = s.sha256(trades.read_bytes())
        return base, trades, hashes


class InputTests(SyntheticTest):
    def test_frozen_hashes_prereg_and_import_identity(self):
        raw = s.verify_prereg()
        recorded = dict(re.findall(r'\| `(data/[^`]+)`[^|]*\| `([0-9a-f]{64})`', raw.decode()))
        self.assertEqual(s.INPUT_SHA256, recorded)
        self.assertEqual(s.DAILY_FILE_COUNT, 685)
        self.assertEqual(s.TRAIN_START, s.timestamp_ms('2021-01-01'))
        for name in ('expected_shortfall', 'paired_test', 'directional_metrics',
                     'carrier_statistics', 'qlike_statistics', 'llm_overlay'):
            self.assertIs(getattr(s, name), getattr(sizing, name))
        self.assertIs(s.fit_ols, forecast.fit_ols)
        self.assertIs(s._read_symbol, panel._read_symbol)
        self.assertIs(s.append_event, evolution_ledger.append_event)
        path = self.root / 'changed.md'
        path.write_bytes(raw + b'\n')
        with self.assertRaisesRegex(ValueError, 'sha256 mismatch'):
            s.verify_prereg(path)

    def test_each_hash_mismatch_refuses_before_panel_and_verifies_at_end(self):
        base, trades, hashes = self.frozen_inputs()
        with patch.object(s, 'INPUT_SHA256', hashes):
            self.assertEqual(s.verify_input_hashes(base, trades), hashes)
        for name in hashes:
            changed = dict(hashes, **{name: '0' * 64})
            with self.subTest(name=name), patch.object(s, 'INPUT_SHA256', changed), \
                    patch.object(s, 'load_inputs') as load, patch.object(s, 'verify_prereg') as verify:
                with self.assertRaisesRegex(ValueError, 'frozen sha256 mismatch'):
                    s.study(base, trades)
                load.assert_not_called()
                self.assertEqual(verify.call_count, 2)

    def test_wrong_file_counts_refuse(self):
        base, trades, hashes = self.frozen_inputs()
        extra = base / 'daily/EXTRA.json'
        extra.write_bytes(b'[]')
        with self.assertRaisesRegex(ValueError, '685'):
            s.verify_input_hashes(base, trades)
        extra.unlink()
        next((base / 'daily').glob('*.json')).unlink()
        with self.assertRaisesRegex(ValueError, '685'):
            s.verify_input_hashes(base, trades)

    def test_all_files_read_then_excluded_and_union_calendar(self):
        fields = synthetic_fields(days=3, symbols=2)
        for name in s.EXCLUDED:
            for field in fields.values():
                field[name] = field.iloc[:, 0]
        fields['close'].iloc[0, 0] = np.nan
        write_panel(self.root, fields)
        bad = self.root / 'daily/INVALID.json'
        bad.write_text('[["2020-11-01",1,1,1,-1,1,1]]')
        with patch.object(s, '_read_symbol', wraps=panel._read_symbol) as read:
            loaded = s.load_inputs(self.root)
        self.assertEqual(read.call_count, len(fields['close'].columns) + 1)
        self.assertFalse(set(loaded['close'].columns) & s.EXCLUDED)
        self.assertEqual(list(loaded['close'].index), list(fields['close'].index))
        self.assertNotIn('INVALID', loaded['close'])
        self.assertTrue(pd.isna(loaded['close'].iloc[0, 0]))


class MembershipAndReturnsTests(SyntheticTest):
    def test_mask_matches_load_panels_history_amount_ties_and_missing_bars(self):
        fields = synthetic_fields(days=95, symbols=4)
        # A newly observed coin needs 60 observed bars, including today's bar.
        fields['close'].iloc[:10, 3] = np.nan
        # A gap interrupts rolling amount for 20 calendar rows.
        fields['close'].iloc[68, 0] = np.nan
        # A large amount on the current day enters the trailing 20-day mean.
        fields['volume'].iloc[80, 2] = 10000
        base = self.root / 'synthetic'
        write_panel(base, fields)
        config = dict(benchmark=None, min_history=60, universe_n=2, windows={})
        with patch.dict(panel.MARKETS, synthetic=config):
            reference = panel.load_panels('synthetic', self.root)
        loaded = s.load_inputs(base)
        actual = s.membership_mask(loaded, 2)
        pd.testing.assert_frame_equal(actual, reference.mask)
        self.assertFalse(actual.iloc[:59].to_numpy().any())
        self.assertEqual(list(actual.columns[actual.iloc[59]]), ['COIN000USDT', 'COIN001USDT'])
        self.assertFalse(actual.iloc[69, 0])
        self.assertTrue(actual.iloc[80, 2])
        # Independent N=4 check catches a 59/60-bar off-by-one for the late listing.
        with patch.dict(panel.MARKETS, synthetic=dict(config, universe_n=4)):
            reference = panel.load_panels('synthetic', self.root)
        actual = s.membership_mask(loaded, 4)
        pd.testing.assert_frame_equal(actual, reference.mask)
        self.assertFalse(actual.iloc[68, 3])
        self.assertTrue(actual.iloc[69, 3])

    def test_exclusions_never_enter_mask(self):
        fields = synthetic_fields(days=65, symbols=3)
        for name in s.EXCLUDED:
            for field in fields.values():
                field[name] = field.iloc[:, 0] * 100
        mask = s.membership_mask(fields, 50)
        self.assertFalse(set(mask.columns) & s.EXCLUDED)
        self.assertEqual(int(mask.iloc[-1].sum()), 3)

    def test_returns_lag_membership_missing_close_count_and_start(self):
        dates = pd.date_range('2020-12-31', periods=4)
        close = pd.DataFrame(dict(A=[100, 100, 110, 121], B=[100, 100, np.nan, 90]), index=dates)
        mask = pd.DataFrame(dict(A=[False, True, False, True], B=[True, True, True, False]), index=dates)
        result = s.basket_returns(close, mask)
        self.assertNotIn(s.TRAIN_START, result['returns'])
        self.assertAlmostEqual(result['returns'][s.TRAIN_START + s.DAY], .1)
        first = result['daily'][0]
        self.assertEqual(first['member_count'], 2)
        self.assertEqual(first['missing_next_close'], 1)
        self.assertEqual(first['missing_symbols'], ['B'])
        self.assertEqual(first['members'], ['A', 'B'])
        # S on the return day selects only B. It cannot alter the preceding day's basket.
        self.assertNotEqual(first['members'], result['daily'][1]['members'])

    def test_calendar_and_membership_invalid_and_missing_ratio_boundary(self):
        fields = synthetic_fields(start='2021-01-01', days=2, symbols=20)
        mask = fields['close'].notna()
        basket = dict(daily=[dict(member_time_ms=s.TRAIN_START, member_count=200, missing_next_close=1)])
        with patch.object(s, 'OOS_END', s.TRAIN_START + s.DAY):
            checked = s.validate_carrier(mask, basket, 20)
            self.assertTrue(checked['valid'])  # exactly 0.005 is allowed
            mask.iloc[0, 0] = False
            checked = s.validate_carrier(mask, basket, 20)
            self.assertFalse(checked['valid'])
            self.assertEqual(checked['first_bad_membership_days'][0]['count'], 19)
            self.assertEqual(s.decide_verdict(passing_stats(), checked['valid']), 'invalid')
            mask.iloc[0, 0] = True
            basket['daily'][0]['missing_next_close'] = 2
            checked = s.validate_carrier(mask, basket, 20)
            self.assertFalse(checked['valid'])
            self.assertEqual(s.decide_verdict(passing_stats(), checked['valid']), 'invalid')
        with patch.object(s, 'VALID_START', s.TRAIN_START), patch.object(s, 'VALID_END', s.TRAIN_START + s.DAY):
            self.assertTrue(s.validate_calendar(fields['close'].index)['valid'])
            missing = s.validate_calendar(fields['close'].index[1:])
            self.assertFalse(missing['valid'])
            self.assertEqual(missing['first_missing_days'], ['2021-01-01'])


class FeatureTests(SyntheticTest):
    def test_rv_and_target_windows_annualization_first_rv30_and_targetless_tail(self):
        fields = synthetic_fields(days=140, symbols=20)
        mask = s.membership_mask(fields, 20)
        returns = {s.TRAIN_START + i * s.DAY: .0001 * i for i in range(1, 80)}
        prepared = s.build_features(fields['close'], mask, returns)
        first = prepared['rows'][0]
        self.assertEqual(first['time_ms'], s.timestamp_ms('2021-01-31'))
        self.assertAlmostEqual(first['rv1'], 365 * returns[first['time_ms']] ** 2)
        for window in (7, 30):
            expected = np.mean([365 * returns[first['time_ms'] - i * s.DAY] ** 2 for i in range(window)])
            self.assertAlmostEqual(first[f'rv{window}'], expected)
        samples = s.build_samples(prepared['rows'], returns)
        row = samples['rows'][0]
        expected = 365 / 7 * sum(returns[first['time_ms'] + i * s.DAY] ** 2 for i in range(1, 8))
        self.assertAlmostEqual(row['y'], expected)
        self.assertNotIn('y', first)
        self.assertGreater(prepared['rows'][-1]['time_ms'], samples['rows'][-1]['time_ms'])
        returns.pop(first['time_ms'] + 3 * s.DAY)
        self.assertIsNone(s.future_outcomes(returns, first['time_ms']))

    def test_rho_D_numpy_and_complete_prices_and_zero_variance(self):
        fields = synthetic_fields(days=31, symbols=20)
        close = fields['close']
        t = close.index[-1]
        state = s.correlation_state(close, list(close.columns), t)
        matrix = np.corrcoef(np.diff(np.log(close.to_numpy()), axis=0), rowvar=False)
        rho = matrix[np.triu_indices(20, k=1)].mean()
        self.assertAlmostEqual(state['rho'], rho)
        self.assertAlmostEqual(state['D'], (1 + 19 * rho) / 20)
        self.assertEqual(state['n_t'], 20)
        close.iloc[0, 0] = np.nan  # t-30 must also exist
        close.iloc[12, 1] = np.nan
        close.iloc[:, 2] = 100  # constant log returns are unusable
        state = s.correlation_state(close, list(close.columns), t)
        matrix = np.corrcoef(np.diff(np.log(close.iloc[:, 3:].to_numpy()), axis=0), rowvar=False)
        self.assertEqual(state['n_t'], 17)
        self.assertEqual(state['unavailable_members'], 3)
        self.assertAlmostEqual(state['rho'], matrix[np.triu_indices(17, k=1)].mean())
        close.iloc[-1, 3] *= 2
        self.assertNotEqual(s.correlation_state(close, list(close.columns), t)['rho'], state['rho'])

    def test_n_below_15_excluded_including_top10(self):
        fields = synthetic_fields(days=130, symbols=20)
        returns = {s.TRAIN_START + i * s.DAY: .01 for i in range(1, 70)}
        for n in (10, 14):
            with self.subTest(n=n):
                prepared = s.build_features(fields['close'], s.membership_mask(fields, n), returns)
                self.assertFalse(prepared['rows'])
                self.assertGreater(prepared['excluded']['insufficient_correlation_members'], 0)
        prepared = s.build_features(fields['close'], s.membership_mask(fields, 15), returns)
        self.assertTrue(prepared['rows'])

    def test_erratum_member_threshold(self):
        self.assertEqual([s.min_correlation_members(n) for n in (10, 20, 50)], [8, 15, 15])
        fields = synthetic_fields(days=130, symbols=20)
        returns = {s.TRAIN_START + i * s.DAY: .01 for i in range(1, 70)}
        mask = s.membership_mask(fields, 10)
        self.assertTrue(s.build_features(fields['close'], mask, returns, 8)['rows'])
        mask = s.membership_mask(fields, 7)
        self.assertFalse(s.build_features(fields['close'], mask, returns, 8)['rows'])

    def test_nonpositive_features_and_targets_counted(self):
        fields = synthetic_fields(days=130, symbols=20)
        mask = s.membership_mask(fields, 20)
        t = s.timestamp_ms('2021-02-01')
        returns = {s.TRAIN_START + i * s.DAY: .01 for i in range(1, 70)}
        returns[t] = 0
        prepared = s.build_features(fields['close'], mask, returns)
        self.assertEqual(prepared['excluded']['nonpositive_rv1'], 1)
        self.assertNotIn(t, [r['time_ms'] for r in prepared['rows']])
        for key in ('rv7', 'rv30'):
            zero = dict(returns)
            for i in range(int(key[2:])):
                zero[t - i * s.DAY] = 0
            self.assertNotIn(t, [r['time_ms'] for r in s.build_features(fields['close'], mask, zero)['rows']])
        with patch.object(s, 'correlation_state', return_value=dict(rho=0., n_t=20, D=0., unavailable_members=0)):
            self.assertGreater(s.build_features(fields['close'], mask, returns)['excluded']['nonpositive_D'], 0)
        targets = s.build_samples([dict(time_ms=t)], {t + i * s.DAY: 0. for i in range(1, 8)})
        self.assertEqual(targets['excluded']['nonpositive_y'], 1)


class FittingAndSizingTests(SyntheticTest):
    def test_model_features_and_n_minus_p_calibration(self):
        rows = fit_rows(days=30)
        row = rows[0]
        b = [1., math.log(row['rv1']), math.log(row['rv7']), math.log(row['rv30'])]
        self.assertEqual(s.model_features(row, 'B'), b)
        self.assertEqual(s.model_features(row, 'C'), b + [math.log(row['D'])])
        for model, p in (('B', 4), ('C', 5)):
            fit = s.calibrated_fit(rows, model)
            x = np.asarray([s.model_features(r, model) for r in rows])
            errors = np.log([r['y'] for r in rows]) - x @ fit['coefficients']
            self.assertEqual(fit['residual_df'], len(rows) - p)
            self.assertAlmostEqual(fit['s2'], float(errors @ errors) / (len(rows) - p))
            with self.assertRaisesRegex(ValueError, 'n>p'):
                s.calibrated_fit(rows[:p], model)

    def test_monthly_no_lookahead_training_start_strict_target_cutoff_and_tail(self):
        samples = fit_rows(start='2020-12-15', days=100)
        features = deepcopy(samples)
        for row in features:
            row.pop('y')
        month = s.timestamp_ms('2021-03-01')
        result = s.calibrated_monthly_forecasts(samples, features, start=month)
        fit = result['fits'][0]
        expected = [r for r in samples if s.TRAIN_START <= r['time_ms'] and r['time_ms'] + 7 * s.DAY < month]
        self.assertEqual(fit['n'], len(expected))
        self.assertEqual(fit['train_start_ms'], s.TRAIN_START)
        self.assertEqual(fit['train_last_ms'], month - 8 * s.DAY)
        self.assertEqual(fit['max_target_day_ms'], month - s.DAY)
        changed = deepcopy(samples)
        for row in changed:
            if row['time_ms'] >= month - 7 * s.DAY:
                row.update(y=9999., rv1=9999., D=9999.)
        altered = s.calibrated_monthly_forecasts(changed, features, start=month)
        self.assertEqual(fit, altered['fits'][0])
        # A genuinely targetless feature row after the last sample still gets a prediction.
        tail = dict(features[-1], time_ms=features[-1]['time_ms'] + s.DAY)
        result = s.calibrated_monthly_forecasts(samples, features + [tail], start=month)
        self.assertEqual(result['forecasts'][-1]['time_ms'], tail['time_ms'])
        self.assertIsNone(result['forecasts'][-1]['y'])
        for prediction in result['forecasts']:
            for model, values in prediction['models'].items():
                self.assertAlmostEqual(values['F'], values['F0'] * math.exp(fit['models'][model]['s2'] / 2))

    def test_synthetic_price_changes_in_month_do_not_change_month_coefficients(self):
        fields = synthetic_fields(start='2020-11-01', days=220, symbols=20)
        month = s.timestamp_ms('2021-04-01')

        def estimates(data):
            mask = s.membership_mask(data, 20)
            returns = s.basket_returns(data['close'], mask)['returns']
            features = s.build_features(data['close'], mask, returns)['rows']
            samples = s.build_samples(features, returns)['rows']
            return s.calibrated_monthly_forecasts(samples, features, start=month)['fits'][0]

        before = estimates(fields)
        changed = deepcopy(fields)
        date = pd.Timestamp(month, unit='ms')
        changed['close'].loc[date:] *= np.linspace(2, 3, len(changed['close'].loc[date:]))[:, None]
        self.assertEqual(before, estimates(changed))

    def test_insufficient_training_and_B_only_fit(self):
        rows = fit_rows(days=5)
        feature = dict(rows[0], time_ms=s.OOS_START)
        result = s.calibrated_monthly_forecasts(rows, [feature])
        self.assertEqual(set(result['fits'][0]['models']), {'B'})
        self.assertFalse(result['forecasts'])
        self.assertEqual(len(result['skipped']), 1)

    def test_reference_expanding_median_targetless_and_no_future(self):
        start = s.TRAIN_START
        features = [dict(time_ms=start + i * s.DAY, rv30=v * v) for i, v in enumerate((1, 9, 3, 5))]
        dates = [start - s.DAY] + [r['time_ms'] for r in features]
        references = s.expanding_references(features, list(reversed(dates)))
        self.assertEqual(list(references.values()), [None, 1., 5., 3., 4.])
        changed = deepcopy(features)
        changed[-1]['rv30'] = 1e12
        self.assertEqual(s.expanding_references(changed, dates)[start + 2 * s.DAY], 3.)
        features.insert(0, dict(time_ms=start - s.DAY, rv30=1e9))
        self.assertEqual(s.expanding_references(features, dates)[start], 1.)

    def test_k_cap_return_next_and_missing_x(self):
        t = s.OOS_START
        features = [dict(time_ms=t, rv30=4.)]
        forecasts = [dict(features[0], y=1., models=dict(B=dict(sigma_hat=4., F=16.), C=dict(sigma_hat=1., F=1.)))]
        result = s.sizing_rows(features, forecasts, {t: 9., t + s.DAY: -.2})[0]
        self.assertEqual({m: v['k'] for m, v in result['models'].items()}, dict(U=1., T=1., B=.5, C=1.))
        self.assertAlmostEqual(result['models']['B']['x'], -.1)
        self.assertEqual(result['models']['U']['x'], -.2)
        missing = s.sizing_rows(features, forecasts, {})[0]
        self.assertTrue(all(v['x'] is None for v in missing['models'].values()))

    def test_evaluation_inclusive_bounds_and_common_validity(self):
        rows = [dict(time_ms=t, y=1., return_next=.01, models=dict(B=dict(F=1.), C=dict(F=1.)))
                for t in (s.OOS_START - s.DAY, s.OOS_START, s.OOS_END, s.OOS_END + s.DAY)]
        self.assertEqual([r['time_ms'] for r in s.evaluation_rows(rows)], [s.OOS_START, s.OOS_END])
        for key, value in (('y', None), ('y', 0.), ('return_next', None), ('return_next', math.nan)):
            bad = dict(rows[1], **{key: value})
            self.assertFalse(s.evaluation_rows([bad]))
        bad = deepcopy(rows[1])
        del bad['models']['C']
        self.assertFalse(s.evaluation_rows([bad]))


class DecisionAndDescriptionTests(SyntheticTest):
    def test_four_verdicts_and_R3_boundaries(self):
        stats = passing_stats()
        self.assertEqual(s.decide_verdict(stats), 'pass')
        self.assertEqual(s.decide_verdict(stats, False), 'invalid')
        for name in ('TOP10', 'TOP50'):
            changed = deepcopy(stats)
            changed['carriers'][name]['weekly']['comparisons']['B_minus_C']['mean'] = 0.
            self.assertEqual(s.decide_verdict(changed), 'weak')
            changed = deepcopy(stats)
            changed['carriers'][name]['models']['C']['es'] = .2
            self.assertEqual(s.decide_verdict(changed), 'weak')
        for key, value in (('t', 1.999), ('first_half_mean', 0.), ('second_half_mean', 0.)):
            changed = deepcopy(stats)
            changed['carriers']['TOP20']['weekly']['comparisons']['B_minus_C'][key] = value
            self.assertEqual(s.decide_verdict(changed), 'fail')
        changed = deepcopy(stats)
        changed['carriers']['TOP20']['models']['C']['es'] = .2
        self.assertEqual(s.decide_verdict(changed), 'fail')

    def test_None_and_nonfinite_never_pass_conditions(self):
        self.assertEqual(s.decide_verdict({}), 'fail')
        for bad in (None, math.nan, math.inf, -math.inf):
            for carrier in s.CARRIERS:
                for field in ('mean', 't', 'first_half_mean', 'second_half_mean'):
                    changed = passing_stats()
                    changed['carriers'][carrier]['weekly']['comparisons']['B_minus_C'][field] = bad
                    if carrier == 'TOP20' and field != 'mean':
                        self.assertFalse(s.decision_conditions(changed)['R1']['passed'])
                    if carrier != 'TOP20' and field == 'mean':
                        self.assertFalse(s.decision_conditions(changed)['R3']['passed'])
                for model in ('U', 'B', 'C'):
                    changed = passing_stats()
                    changed['carriers'][carrier]['models'][model]['es'] = bad
                    if carrier == 'TOP20':
                        self.assertFalse(s.decision_conditions(changed)['R2']['passed'])

    def test_future_compounding_and_drawdown_initial_peak(self):
        t = s.OOS_START
        values = [-.2, .1, .3, -.5, .1, .2, .1]
        returns = {t + (i + 1) * s.DAY: r for i, r in enumerate(values)}
        result = s.future_outcomes(returns, t)
        path = np.r_[1., np.cumprod(1 + np.asarray(values))]
        self.assertAlmostEqual(result['return_7d'], path[-1] - 1)
        self.assertAlmostEqual(result['max_drawdown_7d'], max(1 - path / np.maximum.accumulate(path)))
        losses = {t + i * s.DAY: -.01 for i in range(1, 8)}
        self.assertAlmostEqual(s.future_outcomes(losses, t)['max_drawdown_7d'], 1 - .99 ** 7)

    def test_quintiles_boundaries_groups_ties_and_outcomes(self):
        t = s.OOS_START
        rows = [dict(time_ms=t + i * s.DAY, rho=i / 20) for i in range(10)]
        returns = {t + i * s.DAY: .001 * (i + 1) for i in range(1, 18)}
        result = s.rho_quintiles(rows, returns)
        np.testing.assert_allclose(result['boundaries'], np.quantile([r['rho'] for r in rows], np.linspace(0, 1, 6)))
        self.assertEqual([g['days'] for g in result['groups']], [2] * 5)
        for i, group in enumerate(result['groups']):
            outcomes = [s.future_outcomes(returns, rows[j]['time_ms']) for j in (2 * i, 2 * i + 1)]
            for key in ('return_7d', 'max_drawdown_7d', 'y'):
                self.assertAlmostEqual(group[key + '_mean'], np.mean([o[key] for o in outcomes]))
        tied = s.rho_quintiles([dict(r, rho=.4) for r in rows], returns)
        self.assertEqual([g['days'] for g in tied['groups']], [0, 0, 0, 0, 10])
        self.assertEqual(s.rho_quintiles([], {})['boundaries'], [None] * 6)

    def test_distributions_correlations_years_and_Neff(self):
        rows = [dict(time_ms=s.timestamp_ms(f'{year}-01-01') + i * s.DAY,
                     rho=.1 + i * .1, D=.2 + i * .05, rv30=.3 + i * .08)
                for year in (2022, 2023) for i in range(5)]
        returns = {r['time_ms'] + i * s.DAY: .01 for r in rows for i in range(1, 8)}
        result = s.descriptive_statistics(rows, rows[5:], returns)
        all_rows = result['all_features']
        self.assertEqual(set(all_rows['by_year']), {'2022', '2023'})
        self.assertEqual(result['evaluation']['rho']['n'], 5)
        self.assertEqual(set(all_rows['rho']['quantiles']), set(map(str, s.QUANTILES)))
        self.assertAlmostEqual(all_rows['n_eff']['mean'], np.mean([5 / (1 + 4 * r['rho']) for r in rows]))
        self.assertAlmostEqual(all_rows['rho_rv30_pearson'], 1.)
        self.assertAlmostEqual(all_rows['log_D_log_rv30_pearson'],
                               np.corrcoef(np.log([r['D'] for r in rows]), np.log([r['rv30'] for r in rows]))[0, 1])
        self.assertIsNone(s.pearson([1, 1], [1, 2]))
        self.assertEqual(s.correlation_descriptives([dict(rows[0], rho=-.25)])['n_eff_undefined'], 1)

    def test_overlay_uses_targetless_post_evaluation_top20_rows(self):
        t = s.OOS_END + 3 * s.DAY
        rows = [dict(time_ms=t, y=None, models={m: dict(k=k) for m, k in (('U', 1.), ('B', .7), ('C', .4))})]
        trade = dict(id=1, entry_time=t + s.DAY + 123, side='LONG', entry_price=100., exit_price=110., initial_stop_loss=95.)
        result = s.llm_overlay([trade], rows)
        self.assertEqual(result['retained'], 1)
        self.assertEqual(result['trades'][0]['k_date_ms'], t)
        self.assertAlmostEqual(result['models']['C']['sum_R'], .8)


class LedgerAndOutputTests(SyntheticTest):
    def test_ledger_K_guard_and_invalid_exemption(self):
        result = dict(verdict='pass', input_hashes={'synthetic': 'hash'}, statistics=passing_stats())
        path = self.root / 'ledger.jsonl'
        for i in range(3):
            evolution_ledger.append_event(path, 'crypto_llm', lambda k, previous: dict(
                track='crypto_llm', stage='S1', change_type='C', idea_id=f'synthetic-{k}',
                baseline_version='synthetic', verdict='fail'))
        row = s.write_ledger(result, path)
        self.assertEqual(row['K'], 4)
        self.assertEqual(row['preregistered_K'], 4)
        self.assertEqual(row['idea_id'], 'corr-state-scaler-v1')
        self.assertEqual(row['baseline_version'], 'har-basket-scaler')
        self.assertTrue(all(row[r]['passed'] for r in ('R1', 'R2', 'R3')))
        before = path.read_bytes()
        with self.assertRaisesRegex(ValueError, 'K=5'):
            s.write_ledger(result, path)
        self.assertEqual(path.read_bytes(), before)
        row = s.write_ledger(dict(result, verdict='invalid'), path)
        self.assertIsNone(row['K'])
        self.assertFalse(row['counts_toward_k'])

    def test_output_rejects_input_directories_files_ledger_overlap_symlink_hardlink(self):
        base, trades, hashes = self.frozen_inputs()
        out = self.root / 'out'
        ledger = self.root / 'ledger.jsonl'
        s.validate_output(out, base, trades, ledger)
        for bad_out in (base, base / 'nested', trades.parent, base / 'daily'):
            with self.subTest(out=bad_out), self.assertRaises(ValueError):
                s.validate_output(bad_out, base, trades)
        for bad_ledger in (base / 'manifest.json', base / 'daily/new.json', trades, s.PREREG,
                           out / 'results.json', out / 'results.json.tmp', out / 'report.md', out / 'report.md/nested'):
            with self.subTest(ledger=bad_ledger), self.assertRaises(ValueError):
                s.validate_output(out, base, trades, bad_ledger)
        out.mkdir()
        (out / 'report.md').symlink_to(trades)
        with self.assertRaises(ValueError):
            s.validate_output(out, base, trades)
        (out / 'report.md').unlink()
        (out / 'report.md').hardlink_to(base / 'daily/EMPTY000.json')
        with self.assertRaises(ValueError):
            s.validate_output(out, base, trades)

    def test_cli_defaults_and_optional_ledger_path(self):
        # Parser defaults can be checked without opening the default inputs or ledger.
        with patch.object(s, 'validate_output') as validate, patch.object(s, 'study', return_value={}), \
                patch.object(s, 'write_json'), patch.object(s, 'report', return_value='合成报告'), \
                patch.object(s, 'write_ledger', return_value={}) as ledger, \
                patch('pathlib.Path.write_text'), redirect_stdout(io.StringIO()):
            with patch.object(s, 'study', return_value=dict(verdict='fail')) as study:
                self.assertEqual(s.main(['run', '--out', str(self.root / 'out'), '--write-ledger']), 0)
                study.assert_called_once_with(s.REPO / 'data/factor_mining/crypto_full', s.REPO / s.TRADE_KEY)
                self.assertEqual(validate.call_args.args[-1], s.LEDGER)
                self.assertEqual(ledger.call_args.args[-1], s.LEDGER)

    def test_end_to_end_synthetic_outputs_diagnostics_overlay_and_prereg_checks(self):
        fields = synthetic_fields(start='2022-08-01', days=191, symbols=52)
        base, trades, hashes = self.frozen_inputs(fields)
        trade = dict(id=1, side='LONG', entry_price=100., exit_price=110., initial_stop_loss=95.,
                     entry_time=s.timestamp_ms('2023-02-06T01:00:00Z'))
        trades.write_text(json.dumps(trade) + '\n', encoding='utf-8')
        hashes[s.TRADE_KEY] = s.sha256(trades.read_bytes())
        out = self.root / 'out'
        with patch.object(s, 'INPUT_SHA256', hashes), \
                patch.multiple(s, TRAIN_START=s.timestamp_ms('2022-10-01'), OOS_END=s.timestamp_ms('2023-01-20'),
                               VALID_START=s.timestamp_ms('2022-08-01'), VALID_END=s.timestamp_ms('2023-02-07')), \
                patch.object(s, 'verify_prereg', wraps=s.verify_prereg) as verify, redirect_stdout(io.StringIO()):
            code = s.main(['run', '--data-dir', str(base), '--trades', str(trades), '--out', str(out)])
        self.assertEqual(code, 0)
        self.assertEqual(verify.call_count, 2)
        result = json.loads((out / 'results.json').read_text())
        self.assertTrue(result['validation']['valid'])
        self.assertIn(result['verdict'], ('pass', 'weak', 'fail'))
        # Prereg section 11 erratum: TOP10 uses an 8-member correlation threshold.
        self.assertGreater(result['statistics']['carriers']['TOP10']['n'], 0)
        self.assertEqual(result['input_hashes'], hashes)
        for name in ('TOP20', 'TOP50'):
            data = result['carriers'][name]
            self.assertTrue(data['fits'])
            self.assertTrue(data['daily'])
            self.assertIsNone(data['daily'][-1]['y'])
            self.assertIsNone(data['daily'][-1]['models']['C']['x'])
            self.assertIn('D', data['features'][0])
            self.assertIn('n_t', data['features'][0])
            self.assertGreater(result['statistics']['carriers'][name]['n'], 0)
            self.assertIn('uncalibrated', result['statistics']['qlike'][name])
        self.assertEqual(result['llm_overlay']['retained'], 1)
        self.assertEqual(len(result['top20_worst_five']), 5)
        text = (out / 'report.md').read_text()
        self.assertIn('第 11 节运行前勘误', text)
        self.assertIn('R1、R2、R3', text)
        json.dumps(result, allow_nan=False)

    def test_invalid_calendar_still_produces_diagnostics_and_report(self):
        fields = synthetic_fields(start='2022-10-01', days=130, symbols=20)
        fields = {name: frame.drop(frame.index[15]) for name, frame in fields.items()}
        base, trades, hashes = self.frozen_inputs(fields)
        with patch.object(s, 'INPUT_SHA256', hashes):
            result = s.study(base, trades)
        self.assertEqual(result['verdict'], 'invalid')
        self.assertGreater(result['validation']['calendar']['missing_days'], 0)
        self.assertGreater(result['validation']['carriers']['TOP50']['bad_membership_days'], 0)
        self.assertIn('描述项', s.report(result))
        self.assertTrue(result['carriers']['TOP20']['features'])


if __name__ == '__main__':
    unittest.main()
