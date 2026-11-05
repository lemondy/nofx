"""Synthetic-only checks of native execution and descriptive stress overlays."""
import hashlib
import json

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import fq_us_momentum_stress as stress
from scripts.backtest import fq_us_momentum_study as s1


def curve(values, start='2007-10-09'):
    return [dict(date=d.date().isoformat(), equity=float(v))
            for d, v in zip(pd.bdate_range(start, periods=len(values)), values, strict=True)]


def equities(rows):
    return [r['equity'] for r in rows]


def test_equalization_formula_exact_resets_and_initial_fees():
    long = curve([49, 60, 70, 77, 70])
    short = curve([49.5, 40, 35, 35, 42])
    dates = [long[i]['date'] for i in (0, 2, 4)]
    result = stress.equalized_path(long, short, dates, initial_cash=100)
    at_2 = 98.5 * (0.5 * 70 / 49 + 0.5 * 35 / 49.5)
    assert equities(result) == pytest.approx([
        98.5, 98.5 * (0.5 * 60 / 49 + 0.5 * 40 / 49.5), at_2,
        at_2 * 1.05, at_2 * 1.1])
    # Day 3 uses a fresh 50/50 allocation, despite unequal sleeve equity.
    assert result[3]['equity'] / result[2]['equity'] == pytest.approx(1.05)
    assert result[4]['equity'] == pytest.approx(at_2 * 1.1)
    assert long[0]['equity'] == 49
    with pytest.raises(ValueError, match='aligned'):
        stress.equalized_path(long, short[:-1], dates, initial_cash=100)


@pytest.mark.parametrize('values,trigger', [([100, 120, 90, 130, 70], 2),
                                          ([90, 75, 110, 60, 130], 1)])
def test_drawdown_latch_inclusive_with_initial_peak_and_permanent_flat(values, trigger):
    base = curve(values)
    result = stress.kill_switch_overlay(base, [r['date'] for r in base],
                                         drawdown=True, initial_cash=100)
    assert result['triggers'][0]['date'] == base[trigger]['date']
    assert result['triggers'][0]['drawdown'] == pytest.approx(0.25)
    assert len(result['triggers']) == 1
    assert equities(result['equity_curve'][trigger:]) == pytest.approx(
        [values[trigger]] * (len(base) - trigger))
    assert result['flat_sessions'] == len(base) - trigger - 1
    assert result['flat_at_close_sessions'] == len(base) - trigger
    assert result['resume_dates'] == []


def test_daily_loss_inclusive_flat_until_next_rebalance_and_chained_resume():
    base = curve([100, 93, 50, 100, 110, 99])
    rebalances = [base[i]['date'] for i in (0, 4)]
    result = stress.kill_switch_overlay(base, rebalances, daily_loss=True, initial_cash=100)
    assert equities(result['equity_curve']) == pytest.approx([100, 93, 93, 93, 102.3, 92.07])
    assert [t['date'] for t in result['triggers']] == [base[i]['date'] for i in (1, 5)]
    assert result['resume_dates'] == [base[4]['date']]
    assert result['flat_sessions'] == 2
    assert result['flat_at_close_sessions'] == 4
    assert base[2]['equity'] == 50


def test_both_overlays_daily_halt_before_permanent_latch():
    base = curve([100, 92, 46, 46, 36.8, 73.6])
    rebalances = [base[i]['date'] for i in (0, 3, 5)]
    result = stress.kill_switch_overlay(base, rebalances, drawdown=True,
                                         daily_loss=True, initial_cash=100)
    assert equities(result['equity_curve']) == pytest.approx([100, 92, 92, 92, 73.6, 73.6])
    assert [t['kind'] for t in result['triggers']] == ['daily_loss', 'drawdown_latch']
    assert [t['date'] for t in result['triggers']] == [base[1]['date'], base[4]['date']]
    assert result['resume_dates'] == [base[3]['date']]
    assert result['flat_sessions'] == 2
    assert result['flat_at_close_sessions'] == 4


def test_trigger_on_rebalance_does_not_resume_same_day():
    base = curve([100, 90, 99, 108.9])
    result = stress.kill_switch_overlay(base, [base[i]['date'] for i in (0, 1, 3)],
                                         daily_loss=True, initial_cash=100)
    assert equities(result['equity_curve']) == pytest.approx([100, 90, 90, 99])
    assert result['resume_dates'] == [base[3]['date']]
    assert result['flat_sessions'] == 1


def test_window_metrics_known_path_and_nontrading_boundaries():
    base = curve([100, 120, 90, 110, 88], start='2009-03-05')
    result = stress.window_metrics(base, '2009-03-06', '2009-03-11')
    assert result['total_return'] == pytest.approx(88 / 120 - 1)
    assert result['max_drawdown'] == pytest.approx(1 - 88 / 120)
    assert result['drawdown_peak_date'] == '2009-03-06'
    assert result['drawdown_trough_date'] == '2009-03-11'
    assert result['return_sessions'] == 3
    weekend = stress.window_metrics(base, '2009-03-07', '2009-03-11')
    assert weekend['start'] == '2009-03-09'
    assert weekend['total_return'] == pytest.approx(88 / 90 - 1)
    assert not stress.window_metrics(base, '2006-01-01', '2006-12-31')['available']


def test_year_boundaries_worst_day_and_twenty_sessions():
    base = curve([99, 110, 88], start='2010-12-31')
    m = stress.describe_path(base, 100)
    assert m['calendar_year_returns']['2010'] == pytest.approx(-0.01)
    assert m['calendar_year_returns']['2011H1'] == pytest.approx(88 / 99 - 1)
    assert m['worst_single_day']['end'] == base[2]['date']
    assert m['worst_single_day']['total_return'] == pytest.approx(-0.2)
    assert m['worst_20_sessions'] is None
    longer = curve([100] * 20 + [80, 90])
    worst = stress.describe_path(longer, 100)['worst_20_sessions']
    assert worst['total_return'] == pytest.approx(-0.2)
    assert worst['sessions'] == 20
    assert worst['start'] == longer[0]['date'] and worst['end'] == longer[20]['date']


def frame(prices, dates):
    return pd.DataFrame(dict(open=prices, high=prices, low=prices, close=prices,
                             volume=5_000_000.0), index=dates, dtype=float)


def test_tiny_native_end_to_end_panel_with_fees_resets_and_metrics():
    dates = pd.bdate_range('2007-10-09', periods=5)
    frames = {'A': frame([10, 11, 12, 13, 12], dates),
              'B': frame([20, 18, 16, 15, 17], dates)}
    rebalances = [dates[i].date().isoformat() for i in (0, 2, 4)]
    longs = s1.run_sleeve(frames, dates, {d: {'A': 0.8} for d in rebalances},
                          cash=50_000, commission=0.0013)
    shorts = s1.run_sleeve(frames, dates, {d: {'B': -0.8} for d in rebalances},
                           cash=50_000, commission=0.0007, short=True)
    assert longs['engine'] == shorts['engine'] == 'backtrader'
    assert longs['equity_curve'][0]['equity'] == pytest.approx(50_000 - 40_000 * 0.0013)
    assert shorts['equity_curve'][0]['equity'] == pytest.approx(50_000 - 40_000 * 0.0007)
    assert all(f['execution_time'][:10] in rebalances for f in longs['fills'] + shorts['fills'])
    equalized = stress.equalized_path(longs['equity_curve'], shorts['equity_curve'], rebalances)
    assert equalized[0]['equity'] == pytest.approx(100_000 - 40_000 * 0.002)
    reset = 0.5 * (longs['equity_curve'][3]['equity'] / longs['equity_curve'][2]['equity']
                   + shorts['equity_curve'][3]['equity'] / shorts['equity_curve'][2]['equity'])
    assert equalized[3]['equity'] / equalized[2]['equity'] == pytest.approx(reset)
    overlay = stress.kill_switch_overlay(equalized, rebalances, drawdown=True, daily_loss=True)
    metrics = stress.describe_path(equalized, 100_000, post_processing=True)
    assert metrics['total_return'] == pytest.approx(equalized[-1]['equity'] / 100_000 - 1)
    assert metrics['focus_windows']['crash']['total_return'] == pytest.approx(
        equalized[-1]['equity'] / equalized[0]['equity'] - 1)
    assert len(overlay['equity_curve']) == len(dates)
    assert metrics['commission'] is None


def test_loader_sorted_hash_yahoo_names_invalid_and_missing_inputs(tmp_path):
    daily = tmp_path / 'daily'
    daily.mkdir()
    raw = {}
    for name, rows in {'Z': [], 'SPY': [['2006-01-03', 10, 11, 9, 10, 1e7, 10]],
                       'BRK-B': [['2006-01-03', 10, 1, 9, 10, 1e7, 10]]}.items():
        raw[name + '.json'] = json.dumps(rows).encode()
        (daily / (name + '.json')).write_bytes(raw[name + '.json'])
    frozen, sp500 = tmp_path / 'frozen.json', tmp_path / 'sp500.json'
    frozen.write_text(json.dumps(dict(symbols=[f'S{i}' for i in range(90)])))
    sp500.write_text(json.dumps(dict(sp500=['BRK.B', 'Z'])))
    result = stress.load_inputs(daily, sp500, frozen)
    assert result['input_hashes']['daily'] == hashlib.sha256(
        b''.join(raw[name] for name in sorted(raw))).hexdigest()
    assert result['requested']['sp500'] == ['BRK-B', 'Z']
    assert set(result['frames']) == {'SPY'}
    assert result['data_quality']['BRK-B']['invalid_rows'] == 1
    assert result['data_quality']['Z']['rows'] == 0


def test_synthetic_cli_full_report_readonly_inputs_and_schedule(tmp_path, monkeypatch):
    daily, out = tmp_path / 'daily', tmp_path / 'out'
    daily.mkdir()
    dates = pd.bdate_range('2005-01-03', periods=295)
    t = np.arange(len(dates))
    names = [f'S{i:02}' for i in range(90)]
    # Only 15 of the frozen 90 have data; eligibility and selection stay unmodified.
    for i, name in enumerate(names[:15] + ['SPY']):
        prices = 20 * np.exp(0.0002 * (i + 1) * t) * (1 + 0.01 * np.sin(t))
        rows = [[d.date().isoformat(), p, p, p, p, 5e6, p]
                for d, p in zip(dates, prices, strict=True)]
        (daily / f'{name}.json').write_text(json.dumps(rows))
    frozen, sp500 = tmp_path / 'frozen.json', tmp_path / 'sp500.json'
    frozen.write_text(json.dumps(dict(symbols=names)))
    sp500.write_text(json.dumps(dict(sp500=names[:15])))
    before = {p: p.read_bytes() for p in tmp_path.rglob('*.json')}
    monkeypatch.setattr(stress, 'FROZEN_UNIVERSE', frozen)
    # The default argument is bound at definition time; use the synthetic frozen input.
    load = stress.load_inputs
    monkeypatch.setattr(stress, 'load_inputs', lambda d, s: load(d, s, frozen))
    assert stress.main(['--daily', str(daily), '--sp500', str(sp500), '--out', str(out)]) == 0
    report = json.loads((out / 'report.json').read_text())
    assert report['descriptive_only'] and not report['counts_toward_K']
    assert 'verdict' not in report and 'K' not in report
    for universe in report['universes'].values():
        assert universe['start'] == dates[252].date().isoformat()
        assert universe['end'] == dates[292].date().isoformat()
        assert universe['trailing_sessions_excluded'] == 2
        assert universe['eligible_counts'][0]['eligible_count'] == 15
        assert universe['eligible_counts'][0]['quintile_count'] == 3
        assert len(universe['paths']) == 9
        for path in universe['paths'].values():
            assert len(path['equity_curve']) == 41
        assert sum(universe['decisions'][0]['long'].values()) == pytest.approx(0.6)
    text = (out / 'report.md').read_text()
    assert '幸存者偏差' in text and '后处理' in text and '不计入任何 K' in text
    assert set(p.name for p in out.iterdir()) == {'report.json', 'report.md'}
    assert {p: p.read_bytes() for p in before} == before
    with pytest.raises(FileExistsError):
        stress.main(['--daily', str(daily), '--sp500', str(sp500), '--out', str(out)])
