"""Offline synthetic panels: native Backtrader fills and preregistered gates."""
import hashlib
import json
import numpy as np
import pandas as pd
import pytest

from scripts.backtest import fq_stop_tp_study as study
from scripts.backtest import quantjourney as q
from scripts.factor_mining.store import finite


def frame(rows):
    f = pd.DataFrame(rows, columns=['open', 'high', 'low', 'close'], dtype=float,
                     index=pd.date_range('2024-01-01', periods=len(rows)))
    f['volume'] = 1000.
    return f


FLAT = (100, 100, 100, 100)


def execute(rows, variant='S20', weights=None, **kwargs):
    f = frame(rows)
    targets = pd.DataFrame({'A': weights if weights is not None else [.5] * len(f)}, index=f.index)
    return q.run_weights({'A': f}, targets, market='crypto',
                         policy=q.RebalancePolicy(None, rebalance_on_signal_change=True),
                         commission=kwargs.pop('commission', 0), slippage=kwargs.pop('slippage', 0),
                         keep_fills=True, exit_policy=study.VARIANTS[variant], **kwargs)


def test_fq_stop_long_fills_at_stop_on_entry_bar():
    r = execute([FLAT, (100, 101, 75, 85), FLAT])
    event, = r['exit_events']
    assert event['kind'] == 'stop_loss'
    assert event['price'] == 80
    assert event['date'] == '2024-01-02'
    assert event['quantity_after'] == 0
    assert r['daily_equity']['2024-01-02'] == 900


def test_fq_stop_gap_through_fills_at_open():
    r = execute([FLAT, FLAT, (70, 75, 65, 72), FLAT])
    event, = r['exit_events']
    assert event['trigger'] == 80
    assert event['price'] == 70
    assert r['daily_equity']['2024-01-03'] == 850


@pytest.mark.parametrize('bar,price', [((100, 125, 95, 110), 120), ((130, 135, 125, 132), 130)])
def test_fq_stop_short_stop_and_gap(bar, price):
    r = execute([FLAT, FLAT, bar, FLAT], weights=[-.5] * 4)
    event, = r['exit_events']
    assert event['side'] == 'short'
    assert event['price'] == price
    assert event['size'] == 5
    assert event['quantity_after'] == 0


@pytest.mark.parametrize('weight,bar,expected', [(.5, (100, 125, 70, 100), 80),
                                                (-.5, (100, 130, 75, 100), 120)])
def test_fq_stop_same_bar_stop_precedes_tp(weight, bar, expected):
    r = execute([FLAT, FLAT, bar], variant='T2', weights=[weight] * 3)
    assert [e['kind'] for e in r['exit_events']] == ['stop_loss']
    assert r['exit_events'][0]['price'] == expected
    assert r['metrics']['tp_trigger_count'] == 0


@pytest.mark.parametrize('weight,bar,expected', [(.5, (100, 111, 99, 110), 110),
                                                (-.5, (100, 101, 89, 90), 90)])
def test_fq_stop_partial_tp_halves_qty_and_does_not_repeat(weight, bar, expected):
    r = execute([FLAT, FLAT, bar, bar, bar], variant='T1', weights=[weight] * 5)
    event, = r['exit_events']
    assert event['kind'] == 'take_profit'
    assert event['price'] == pytest.approx(expected)
    assert event['quantity_after'] == event['quantity_before'] / 2
    assert len(r['fills']) == 2


def test_fq_stop_two_tiers_are_thirds_of_original_qty():
    r = execute([FLAT, FLAT, (100, 125, 99, 121), (121, 130, 120, 125)], variant='T2')
    first, second = r['exit_events']
    assert first['size'] == pytest.approx(-5/3)
    assert second['size'] == pytest.approx(-5/3)
    assert second['quantity_after'] == pytest.approx(5/3)


def test_fq_stop_tp_gap_uses_open():
    r = execute([FLAT, FLAT, (120, 125, 119, 121)], variant='T1')
    assert r['exit_events'][0]['price'] == 120


@pytest.mark.parametrize('weight,bar,expected', [(.5, (100, 110, 100, 105), 110),
                                                (-.5, (100, 100, 90, 95), 90)])
def test_fq_stop_tp_exact_touch(weight, bar, expected):
    r = execute([FLAT, FLAT, bar], variant='T1', weights=[weight] * 3)
    assert r['exit_events'][0]['price'] == expected


@pytest.mark.parametrize('weight,bars,expected', [
    (.5, [(112, 120, 110, 118), (118, 119, 105, 106)], 108),
    (-.5, [(88, 90, 80, 82), (82, 95, 81, 94)], 88),
])
def test_fq_stop_trailing_stop(weight, bars, expected):
    r = execute([FLAT, FLAT] + bars, variant='T3', weights=[weight] * 4)
    event, = r['exit_events']
    assert event['kind'] == 'trailing_stop'
    assert event['price'] == pytest.approx(expected)


def test_fq_stop_new_trail_cannot_fill_at_pre_activation_open():
    r = execute([FLAT, FLAT, (100, 120, 99, 115)], variant='T3')
    event, = r['exit_events']
    assert event['kind'] == 'trailing_stop'
    assert event['reference_price'] == 108
    assert event['price'] == 108


def test_fq_stop_existing_trail_gaps_at_open():
    r = execute([FLAT, FLAT, (112, 120, 110, 118), (100, 105, 95, 103)], variant='T3')
    assert r['exit_events'][0]['price'] == 100


def test_fq_stop_short_trailing_activation_exact_touch():
    r = execute([FLAT, FLAT, (95, 98, 90, 92), (95, 100, 94, 99)],
                variant='T3', weights=[-.5] * 4)
    assert r['exit_events'][0]['kind'] == 'trailing_stop'
    assert r['exit_events'][0]['price'] == pytest.approx(99)


def test_fq_stop_breakeven_after_partial_tp_same_bar():
    r = execute([FLAT, FLAT, (100, 116, 95, 110)], variant='T4')
    tp, be = r['exit_events']
    assert tp['kind'] == 'take_profit'
    assert tp['quantity_after'] == 2.5
    assert be['kind'] == 'breakeven_stop'
    assert be['price'] == 100
    assert be['quantity_after'] == 0


def test_fq_stop_reentry_only_at_next_target_change():
    rows = [FLAT, (100, 101, 75, 85), FLAT, FLAT, FLAT, FLAT]
    r = execute(rows, weights=[.5, .5, .5, .4, .4, .4])
    assert [(f['execution_bar'], f['reason']) for f in r['fills']] == [
        (1, 'initial'), (1, 'stop_loss'), (4, 'signal')]
    assert r['daily_equity']['2024-01-04'] == 900
    assert r['fills'][-1]['size'] == 3.6


def test_fq_stop_partial_restored_and_tier_rearmed_at_next_target_change():
    r = execute([FLAT, FLAT, (100, 111, 99, 110), FLAT, (100, 111, 99, 110)],
                variant='T1', weights=[.5, .5, .4, .4, .4])
    assert [f['execution_bar'] for f in r['fills'] if f['reason'] == 'signal'] == [3]
    assert [e['date'] for e in r['exit_events']] == ['2024-01-03', '2024-01-05']


def test_fq_stop_unchanged_daily_calendar_cannot_restore_exited_symbol():
    f = frame([FLAT, (100, 101, 70, 80), FLAT, FLAT, FLAT])
    w = pd.DataFrame({'A': [.5, .5, .5, .4, .4]}, index=f.index)
    r = q.run_weights({'A': f}, w, market='crypto', commission=0, slippage=0,
                      keep_fills=True, policy=q.RebalancePolicy('D'),
                      exit_policy=study.VARIANTS['S20'])
    assert [(f['execution_bar'], f['reason']) for f in r['fills']] == [
        (1, 'initial'), (1, 'stop_loss'), (4, 'calendar')]


def test_fq_stop_addition_reprices_stop_from_native_average():
    r = execute([FLAT, FLAT, (120, 122, 119, 120), (120, 122, 85, 100)],
                weights=[.2, .4, .4, .4])
    # 2 shares at 100, then 3.466666... at 120: native weighted average.
    add = r['fills'][1]
    avg = (2 * 100 + add['size'] * 120) / (2 + add['size'])
    assert r['exit_events'][0]['trigger'] == pytest.approx(avg * .8)


def test_fq_stop_missing_bar_cannot_trigger_on_forward_filled_extrema():
    f = frame([FLAT, (100, 111, 90, 100), FLAT, FLAT]).drop(pd.Timestamp('2024-01-03'))
    targets = pd.DataFrame({'A': [.5, .5, .5, .5]}, index=pd.date_range('2024-01-01', periods=4))
    r = q.run_weights({'A': f}, targets, market='crypto', commission=0, slippage=0,
                      policy=q.RebalancePolicy(None, rebalance_on_signal_change=True),
                      exit_policy=study.VARIANTS['T1'])
    assert len(r['exit_events']) == 1
    assert r['exit_events'][0]['date'] == '2024-01-02'


def test_fq_stop_b0_has_only_short_50pct_stop():
    rows = [FLAT, FLAT, (100, 155, 40, 100)]
    assert execute(rows, variant='B0')['exit_events'] == []
    assert execute(rows, variant='B0', weights=[-.5]*3)['exit_events'][0]['price'] == 150
    assert execute(rows, variant='S0')['exit_events'] == []


def test_fq_stop_sigma_uses_prior_bar_and_is_frozen_at_entry():
    idx = pd.date_range('2024-01-01', periods=36)
    closes = 100 * np.cumprod(1 + np.resize([.02, -.02, .01, -.01], len(idx)))
    f = pd.DataFrame(dict(open=closes, high=closes * 1.01, low=closes * .99,
                          close=closes, volume=1000.), index=idx)
    dates = idx[30:]
    # First signal at row30, entry row31. An extreme entry-day close must
    # never enter the entry sigma, nor update the frozen percentage later.
    prior_sigma = f.close.pct_change(fill_method=None).iloc[1:31].std(ddof=1)
    pct = np.clip(2.5 * prior_sigma * np.sqrt(3), .05, .5)
    entry = f.loc[idx[31], 'open']
    f.loc[idx[31], ['high', 'close']] = entry * 1.3
    f.loc[idx[32], ['open', 'high', 'low', 'close']] = [entry, entry * 1.01, entry * .7, entry * .8]
    w = pd.DataFrame({'A': .5}, index=dates)
    r = q.run_weights({'A': f}, w, market='crypto', commission=0, slippage=0,
                      policy=q.RebalancePolicy(None, rebalance_on_signal_change=True),
                      exit_policy=study.VARIANTS['SV'])
    assert r['exit_events'][0]['trigger'] == pytest.approx(entry * (1-pct))


@pytest.mark.parametrize('sigma,pct', [(0, .05), (1, .5)])
def test_fq_stop_sigma_clamps(sigma, pct):
    dates = pd.date_range('2024-01-01', periods=3)
    r = execute([FLAT, FLAT, (100, 101, 40, 60)], variant='SV',
                exit_volatility=pd.DataFrame({'A': sigma}, index=dates))
    assert r['exit_events'][0]['trigger'] == 100 * (1-pct)


def test_fq_stop_costs_and_funding_are_native():
    rows = [FLAT, (100, 101, 90, 100), (100, 101, 70, 80), FLAT]
    rates = {'A': {d.date(): .001 for d in frame(rows).index}}
    r = execute(rows, commission=.0005, slippage=.0003, funding=rates)
    buy, stop = r['fills']
    trigger = buy['price'] * .8
    assert stop['price'] == pytest.approx(trigger * (1-.0003))
    assert r['metrics']['commission_paid'] == pytest.approx(sum(f['commission'] for f in r['fills']))
    assert r['metrics']['funding_paid'] == 1.5
    assert r['metrics']['slippage_paid'] == pytest.approx(5*.03 + 5*trigger*.0003)


def test_fq_stop_default_none_identical_to_pre_change_engine():
    f = frame([(100, 101, 99, 100), (100, 105, 98, 103), (103, 104, 98, 99),
               (99, 103, 97, 101), (101, 103, 99, 102), (102, 104, 100, 103)])
    w = pd.DataFrame({'A': [.5, .5, -.4, -.4, 0, 0]}, index=f.index)
    kwargs = dict(policy=q.RebalancePolicy(None, rebalance_on_signal_change=True), market='crypto',
                  funding={'A': {d.date(): .001 for d in f.index}}, keep_fills=True)
    implicit = finite(q.run_weights({'A': f}, w, **kwargs))
    explicit = finite(q.run_weights({'A': f}, w, exit_policy=None, **kwargs))
    assert explicit == implicit
    # Captured from unmodified quantjourney.py before implementing the policy:
    # whole result includes native equity, metrics, funding, costs and fills.
    assert hashlib.sha256(json.dumps(explicit, sort_keys=True).encode()).hexdigest() == (
        '399e41f5b91c0638928dca2eae42add2c90b139c32311f51b63a001597ee916b')
    no_exits = finite(q.run_weights({'A': f}, w, exit_policy=study.VARIANTS['S0'], **kwargs))
    assert no_exits['daily_equity'] == implicit['daily_equity']
    assert no_exits['fills'] == implicit['fills']
    assert all(no_exits['metrics'][key] == value for key, value in implicit['metrics'].items())


def test_fq_stop_bootstrap_is_paired_deterministic_and_uses_sharpe_difference():
    base = np.tile([-.01, .02, -.02, .03, .01], 4)
    returns = pd.DataFrame({'B0': base, 'S20': base, 'T1': base + .001})
    a, b = study.bootstrap_diffs(returns), study.bootstrap_diffs(returns)
    assert a == b
    assert a['B0']['S20']['p_diff_gt_zero'] == 0
    assert a['B0']['S20']['p_diff_lt_zero'] == 0
    assert a['S20']['T1']['p_diff_gt_zero'] == 1
    assert a['S20']['T1']['sharpe_diff'] == pytest.approx(study.sharpe(base+.001) - study.sharpe(base))


def test_fq_stop_decision_rules_enforce_all_gates_and_inclusive_90pct():
    def interval():
        return dict(variants={v: dict(metrics=dict(maxdd_pct=10., return_pct=20.)) for v in study.VARIANTS},
                    bootstrap={ref: {v: dict(sharpe_diff=.1, p_diff_gt_zero=.90, p_diff_lt_zero=.10)
                                     for v in study.VARIANTS} for ref in ('B0', 'S20')})
    val, forward = interval(), interval()
    forward['bootstrap']['B0']['S20'].update(sharpe_diff=-.1, p_diff_lt_zero=.9)
    val['bootstrap']['B0']['S20']['sharpe_diff'] = -.1
    forward['variants']['S30']['metrics']['maxdd_pct'] = 10.01
    val['bootstrap']['S20']['S15']['sharpe_diff'] = 0
    forward['bootstrap']['S20']['S10']['p_diff_gt_zero'] = .8999
    forward['variants']['T1']['metrics']['return_pct'] = 19.99
    forward['bootstrap']['S20']['T2']['sharpe_diff'] = 0
    decisions = study.decision_rules(dict(val=val, forward=forward))
    by_variant = {d['variant']: d for d in decisions}
    assert len(decisions) == 11
    assert by_variant['S20']['passed']
    assert all(not by_variant[v]['passed'] for v in ('S30', 'S15', 'S10', 'T1', 'T2'))
    assert all(by_variant[v]['passed'] for v in ('S0', 'B0', 'SV', 'T3', 'T4'))


def test_fq_stop_preregistered_variant_table_is_fixed():
    assert tuple(study.VARIANTS) == ('S0', 'B0', 'S30', 'S20', 'S15', 'S10', 'SV', 'T1', 'T2', 'T3', 'T4')
    assert (study.BLOCK, study.BOOTSTRAPS, study.SEED) == (5, 2000, 20261101)
