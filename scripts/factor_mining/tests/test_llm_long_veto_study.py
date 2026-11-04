"""Offline S1 veto checks; all databases, frozen inputs and ledgers are synthetic."""
from copy import deepcopy
from datetime import timedelta
import json
import sqlite3

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import evolution_ledger, incremental_filter as filters
from scripts.factor_mining import panel
from scripts.factor_mining import llm_long_veto_study as s


def trade(symbol='C00', entered='2026-10-10T00:00:01Z', side='LONG', trade_id=1):
    return dict(trade_id=trade_id, trader_id=s.TRADERS[0], symbol=symbol, side=side,
                entry_time=entered, r=-1.)


def decision_connection(tmp_path, decision, record_timestamp='2026-10-10T00:00:01Z'):
    conn = sqlite3.connect(tmp_path / 'attribution.db')
    conn.row_factory = sqlite3.Row
    conn.execute('CREATE TABLE decision_records (trader_id TEXT, timestamp TEXT, decisions TEXT)')
    conn.execute('INSERT INTO decision_records VALUES (?, ?, ?)',
                 (s.TRADERS[0], record_timestamp, json.dumps([decision])))
    return conn


@pytest.mark.parametrize('offset_ms,action,symbol,success,expected', [
    (0, 'open_long', 'C00', True, True),
    (s.ATTRIB_WINDOW_MS, 'open_long', 'C00', True, True),
    (s.ATTRIB_WINDOW_MS + 1, 'open_long', 'C00', True, False),
    (-1, 'open_long', 'C00', True, False),
    (0, 'open_long', 'OTHER', True, False),
    (0, 'open_long', 'C00', False, False),
    (0, 'open_long', 'C00', 1, False),
    (0, 'close_long', 'C00', True, False),
    (0, 'open_short', 'C00', True, False),
    (0, 'open_long_limit', 'C00', True, True),
])
def test_attribution_window(tmp_path, offset_ms, action, symbol, success, expected):
    row = trade()
    stamp = pd.Timestamp(row['entry_time']) - timedelta(milliseconds=offset_ms)
    with decision_connection(tmp_path, dict(action=action, symbol=symbol, success=success,
                                            timestamp=stamp.isoformat())) as conn:
        index, counts = s.read_decisions(conn, s.TRADERS[0])
        assert s.ai_attributed(row, index) is expected
        assert counts['skipped_json'] == 0


def test_nanosecond_iso_fallback_and_malformed_json(tmp_path):
    row = trade()
    with decision_connection(tmp_path, dict(action='open_long', symbol='C00', success=True,
                                            timestamp='2026-10-10T00:00:00.999999999Z')) as conn:
        index, _ = s.read_decisions(conn, s.TRADERS[0])
        assert s.ai_attributed(row, index)
        assert s.timestamp_ns('2026-10-10T00:00:00.999999999Z') == s.timestamp_ns(row['entry_time']) - 1
        conn.execute('DELETE FROM decision_records')
        conn.executemany('INSERT INTO decision_records VALUES (?, ?, ?)', [
            (s.TRADERS[0], row['entry_time'], '{bad json'),
            (s.TRADERS[0], row['entry_time'], '{}'),
            (s.TRADERS[0], row['entry_time'], json.dumps([
                dict(action='open_long_limit', symbol='C00', success=True),
                dict(action='open_long', symbol='C00', success=True, timestamp='invalid'),
                'invalid decision'])),
        ])
        index, counts = s.read_decisions(conn, s.TRADERS[0])
        assert s.ai_attributed(row, index)
        assert counts['skipped_json'] == 2
        assert counts['invalid_timestamps'] == counts['invalid_decisions'] == 1
        index = {('long', 'C00'): [s.timestamp_ns(row['entry_time']) + 1]}
        assert not s.ai_attributed(row, index)


@pytest.mark.parametrize('action', ['open_short', 'open_short_limit'])
def test_short_attribution(tmp_path, action):
    with decision_connection(tmp_path, dict(action=action, symbol='C00', success=True)) as conn:
        index, _ = s.read_decisions(conn, s.TRADERS[0])
        assert s.ai_attributed(trade(side='sHoRt'), index)
        assert not s.ai_attributed(trade(), index)


def small_panel():
    index = pd.date_range('2026-10-08', '2026-10-10')
    frame = pd.DataFrame([[1., 2., 3., 4., 5.], [1., 2., 3., 4., 5.], [5., 4., 3., 2., 1.]],
                         index=index, columns=['C00', 'C01', 'C02', 'C03', 'C04'])
    return panel.Panels({'close': frame}, frame.notna())


def test_score_date_and_pct_boundaries():
    p = small_panel()
    pct = s.percentiles(p.fields['close'], p)
    rows = s.score_trades([trade()], pct, p)
    assert rows[0]['score_date'] == '2026-10-09'
    assert rows[0]['pct'] == .20  # Entry on D is one second after midnight; D has pct=1.
    assert s.veto_mask(rows) == {'1': True}
    pct.at[pd.Timestamp('2026-10-09'), 'C00'] = np.nextafter(.20, 1.)
    rows = s.score_trades([trade()], pct, p)
    assert s.veto_mask(rows) == {'1': False}
    # An offset timezone is converted to its UTC entry date first.
    assert s.score_trades([trade(entered='2026-10-10T08:00:01+08:00')], pct, p)[0]['score_date'] == '2026-10-09'


@pytest.mark.parametrize('kind,reason', [('missing_symbol', 'not_in_panel'), ('missing_date', 'no_row'),
                                       ('masked', 'not_in_mask'), ('nonfinite', 'nonfinite')])
def test_unscored_kept(kind, reason):
    p = small_panel()
    row = trade()
    if kind == 'missing_symbol':
        row['symbol'] = 'OUTSIDE'
    elif kind == 'missing_date':
        row['entry_time'] = '2026-10-08T00:00:01Z'
    elif kind == 'masked':
        p.mask.at[pd.Timestamp('2026-10-09'), 'C00'] = False
    elif kind == 'nonfinite':
        p.fields['close'].at[pd.Timestamp('2026-10-09'), 'C00'] = np.inf
    rows = s.score_trades([row], s.percentiles(p.fields['close'], p), p)
    assert rows[0]['unscored_reason'] == reason
    assert rows[0]['pct'] is None and rows[0]['scored'] is False
    assert s.veto_mask(rows) == {'1': False}


def test_percentile_masks_and_nonfinite_do_not_enter_denominator():
    p = small_panel()
    p.mask['C04'] = False
    p.fields['close']['C03'] = -np.inf
    pct = s.percentiles(p.fields['close'], p)
    assert pct.iloc[1]['C00'] == 1 / 3
    assert pd.isna(pct.iloc[1]['C03']) and pd.isna(pct.iloc[1]['C04'])


@pytest.mark.parametrize('difference,missing,matched', [(0., False, True), (1e-8, False, False),
                                                       (0., True, False), (1e-10, False, True)])
def test_rank_ic_comparator(difference, missing, matched):
    expected = dict(mean=.19, daily={'2026-05-07': .1, '2026-05-08': .28})
    actual = deepcopy(expected)
    actual['daily']['2026-05-07'] += difference
    if missing:
        actual['daily'].pop('2026-05-08')
    result = s.compare_rank_ic(actual, expected)
    assert result['matched'] is matched
    assert result['missing_days'] == (['2026-05-08'] if missing else [])
    if difference:
        assert result['max_abs_error'] == pytest.approx(difference)
    actual = deepcopy(expected)
    actual['mean'] += 1e-8
    assert not s.compare_rank_ic(actual, expected)['matched']


@pytest.fixture
def inputs(tmp_path, monkeypatch):
    """Use the real ensemble/RankIC/statistics/ledger paths with synthetic panels."""
    full, base = tmp_path / 'full', tmp_path / 'base'
    snapshot = full / 'prereg_20261104'
    snapshot.mkdir(parents=True)
    base.mkdir()
    (base / 'data_meta.json').write_text('{}')
    factors = [dict(id='frozen', expr='$close', direction=1, horizon=3)]
    (snapshot / 'zoo.json').write_text(json.dumps(dict(factors=factors)))
    (snapshot / 'ensemble.json').write_text(json.dumps(dict(horizons={'3': dict(factor_ids=['frozen'])})))
    (full / 'prereg_20261104b').mkdir()
    (full / 'prereg_20261104b/f1_selected.json').write_text(json.dumps(dict(factors=factors)))
    (full / 'validation.json').write_text('{"synthetic":true}')
    index = pd.date_range('2026-05-01', '2026-11-02')
    t, j = np.arange(len(index))[:, None], np.arange(20)[None, :]
    close = pd.DataFrame(100 + j * 10 + t * (j + 1) * .02 + np.sin(t / 3 + j),
                         index=index, columns=[f'C{i:02}' for i in range(20)])
    fields = dict(close=close, open=close * 1.001, high=close * 1.01, low=close * .99,
                  volume=close * 100., vwap=close.copy())
    p = panel.Panels(fields, close.notna(), meta=dict(synthetic=True))
    # Source report uses the prior study's exact path, independently of the new helper.
    score = s.ensemble.ensemble_scores(factors, p)
    truncated = s.truncate_panels(p)
    excess, _ = panel.forward_labels(truncated, 3, s.FORWARD_START)
    dates = truncated.mask.index[truncated.mask.index >= pd.Timestamp(s.FORWARD_START)]
    expected_ic = s.ic_summary(s.rank_ic(score.loc[dates], excess.loc[dates], truncated.mask.loc[dates]))
    source_report = full / 'study/pit60_btchedge/report.json'
    source_report.parent.mkdir(parents=True)
    source_report.write_text(json.dumps(dict(rank_ic=dict(UH=expected_ic))))
    monkeypatch.setattr(s, 'FROZEN', {name: s.sha256(full / name) for name in s.FROZEN})
    panel_calls = []

    def load_full(root_dir, n):
        assert root_dir == full
        assert s.config.MARKETS['crypto']['min_history'] == 60
        panel_calls.append(n)
        return p

    def load_base(market, root):
        assert market == 'crypto'
        assert (root / 'crypto/ensemble.json').is_symlink()
        assert s.config.MARKETS['crypto']['universe_n'] == 60
        return p

    monkeypatch.setattr(s.universe_expansion_study, '_panels', load_full)
    monkeypatch.setattr(s.panel, 'load_panels', load_base)
    db = tmp_path / 'synthetic.db'
    with sqlite3.connect(db) as conn:
        conn.executescript('''
            CREATE TABLE trader_positions (
                id INTEGER PRIMARY KEY, trader_id TEXT, symbol TEXT, side TEXT, status TEXT,
                entry_time INTEGER, exit_time INTEGER, realized_pnl REAL, fee REAL,
                initial_stop_loss REAL, entry_price REAL, quantity REAL);
            CREATE TABLE decision_records (trader_id TEXT, timestamp TEXT, decisions TEXT);
        ''')
        for i in range(132):
            side = 'LoNg' if i < 120 else 'SHORT'
            symbol = f'C{i % 20:02}'
            entry = pd.Timestamp('2026-09-17T00:00:01Z') + timedelta(days=i % 45)
            trader_id = s.TRADERS[i % 4]
            r = -1. if i % 20 < 4 else .5 + i / 200.
            conn.execute('INSERT INTO trader_positions VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
                         (i + 1, trader_id, symbol, side, 'CLOSED', entry.value // 1_000_000,
                          (entry + timedelta(hours=1)).value // 1_000_000, r * 10, 0., 90., 100., 1.))
            conn.execute('INSERT INTO decision_records VALUES (?, ?, ?)',
                         (trader_id, entry.isoformat(), json.dumps([dict(
                             action='open_long' if i < 120 else 'open_short_limit',
                             symbol=symbol, success=True, timestamp=entry.isoformat())])))
        # Manual long is excluded by attribution. November entry is excluded by UNTIL.
        for i, entry, symbol in [(133, '2026-10-01T00:00:00Z', 'MANUAL'),
                                  (134, '2026-11-01T00:00:00Z', 'C00')]:
            stamp = pd.Timestamp(entry).value // 1_000_000
            conn.execute('INSERT INTO trader_positions VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
                         (i, s.TRADERS[0], symbol, 'LONG', 'CLOSED', stamp, stamp + 3600000,
                          -10., 0., 90., 100., 1.))
    return dict(db=db, full=full, base=base, snapshot=snapshot, p=p, calls=panel_calls,
                out=tmp_path / 'out', ledger=tmp_path / 'ledger.jsonl')


def run(inputs, **kwargs):
    return s.run_study(inputs['db'], inputs['full'], inputs['base'], inputs['snapshot'],
                       inputs['out'], inputs['ledger'], **kwargs)


def test_frozen_hash_mismatch_precedes_panel_load(inputs):
    (inputs['full'] / 'validation.json').write_text('{"changed":true}')
    with pytest.raises(ValueError, match='frozen input sha256 mismatch'):
        run(inputs)
    assert inputs['calls'] == []
    assert not inputs['out'].exists() and not inputs['ledger'].exists()


def test_long_only_primary_and_short_descriptive(inputs, monkeypatch):
    original = filters.filter_statistics
    original_connect = s.sqlite3.connect
    calls = []

    def readonly_connect(database, **options):
        assert database == inputs['db'].resolve().as_uri() + '?mode=ro'
        assert options == dict(uri=True)
        return original_connect(database, **options)

    def statistics(trades, mask, **kwargs):
        sides = {row['side'].lower() for row in trades}
        assert len(sides) <= 1  # No mixed-side placebo, including descriptive alternatives.
        calls.append((sides, len(trades), kwargs))
        return original(trades, mask, **kwargs)

    monkeypatch.setattr(filters, 'filter_statistics', statistics)
    monkeypatch.setattr(s.sqlite3, 'connect', readonly_connect)
    result = run(inputs, no_ledger=True)
    trades, digest = filters.read_jsonl(inputs['out'] / 'trades.jsonl')
    masks, _ = filters.read_jsonl(inputs['out'] / 'mask.jsonl')
    assert len(trades) == len(masks) == result['primary']['n'] == 120
    assert all(row['side'].lower() == 'long' for row in trades)
    assert result['primary']['trades_sha256'] == digest
    assert result['primary']['dropped_n'] == 24
    assert result['primary']['mask_coverage_error'] == 0
    assert result['descriptives']['short_veto']['n'] == 12
    assert result['descriptives']['short_veto']['verdict'] == 'descriptive_only'
    assert calls[-1][0] == {'long'} and calls[-1][1] == 120
    assert any(sides == {'short'} for sides, _, _ in calls)
    assert all(options == dict(seed=s.SEED, placebo_n=s.PLACEBO_N) for _, _, options in calls)
    assert result['attribution'][s.TRADERS[0][:8]]['long']['excluded_symbols'] == {'MANUAL': 1}
    assert result['s0']['ok']
    assert len(result['panel_digests']) == 6
    assert inputs['calls'] == [None, 60]
    assert not inputs['ledger'].exists()
    s.validate_outputs(inputs['out'], inputs['ledger'], False)


@pytest.mark.parametrize('failure', [None, 'rank_ic', 'coverage', 'export'])
def test_s0_and_ledger(inputs, monkeypatch, failure):
    if failure == 'rank_ic':
        path = inputs['full'] / 'study/pit60_btchedge/report.json'
        doc = json.loads(path.read_text())
        doc['rank_ic']['UH']['mean'] += 1e-8
        path.write_text(json.dumps(doc))
        monkeypatch.setitem(s.FROZEN, 'study/pit60_btchedge/report.json', s.sha256(path))
    elif failure == 'coverage':
        with sqlite3.connect(inputs['db']) as conn:
            conn.execute("UPDATE trader_positions SET symbol='OUTSIDE' WHERE side='LoNg' AND symbol>='C15'")
            for trader_id in s.TRADERS:
                conn.execute('INSERT INTO decision_records VALUES (?, ?, ?)',
                             (trader_id, '2026-09-16T00:00:00Z', json.dumps([])))
            records = conn.execute('SELECT rowid, decisions FROM decision_records').fetchall()
            for rowid, payload in records:
                decisions = json.loads(payload)
                for decision in decisions:
                    if decision['action'] == 'open_long' and decision['symbol'] >= 'C15':
                        decision['symbol'] = 'OUTSIDE'
                conn.execute('UPDATE decision_records SET decisions=? WHERE rowid=?', (json.dumps(decisions), rowid))
    elif failure == 'export':
        original = filters.read_closed_trades

        def different_export(conn, traders, **kwargs):
            rows, counts = original(conn, traders, **kwargs)
            if not kwargs and rows:
                rows[0]['r'] += .01
            return rows, counts

        monkeypatch.setattr(filters, 'read_closed_trades', different_export)
    result = run(inputs)
    rows = evolution_ledger.read_rows(inputs['ledger'])
    assert len(rows) == 1
    row = rows[0]
    evolution_ledger.validate(row)
    assert (row['track'], row['change_type'], row['stage']) == ('crypto_llm', 'B', 'S1')
    assert row['s0_ok'] is (failure is None)
    if failure:
        assert row['verdict'] == 'invalid' and row['K'] is None
        assert not row['counts_toward_k'] and len(row['s0_reasons']) == 1
    else:
        assert row['verdict'] != 'invalid' and row['K'] == 1 and row['counts_toward_k']
    assert result['primary']['verdict'] == row['verdict']
    s.validate_outputs(inputs['out'], inputs['ledger'], True)


def test_dry_run_then_official_is_identical_and_k_comes_from_track(inputs):
    prior = dict(track=s.TRACK, stage='S1', change_type='B', baseline_version='old',
                 idea_id='old', verdict='fail')
    evolution_ledger.append_event(inputs['ledger'], s.TRACK, lambda *_: prior)
    before = inputs['ledger'].read_bytes()
    dry = run(inputs, no_ledger=True)
    assert inputs['ledger'].read_bytes() == before
    assert dry['primary']['K'] == 2
    assert any('K=2' in text for text in dry['deviations'])
    official = run(inputs)
    comparison = official['run_comparison']
    assert comparison['previous_wrote_ledger'] is False
    assert comparison['trades_identical'] and comparison['stats_identical']
    assert comparison['added_trade_ids'] == comparison['removed_trade_ids'] == comparison['changed_trade_ids'] == []
    for key in ('trades_sha256', 'mask_sha256', 'delta', 'se', 'z', 'verdict', 'K'):
        assert official['primary'][key] == dry['primary'][key]


def test_run_comparison_lists_changed_sample(inputs):
    run(inputs, no_ledger=True)
    with sqlite3.connect(inputs['db']) as conn:
        conn.execute('UPDATE trader_positions SET realized_pnl=realized_pnl+1 WHERE id=1')
    result = run(inputs, no_ledger=True)
    assert not result['run_comparison']['trades_identical']
    assert not result['run_comparison']['stats_identical']
    assert result['run_comparison']['changed_trade_ids'] == ['1']


def test_vetoed_features_use_only_history_and_listing_age(inputs):
    p = inputs['p']
    row = s.score_trades([trade()], s.percentiles(p.fields['close'], p), p)[0]
    result = s.vetoed_features([row], {'1': True}, p)[0]
    date = pd.Timestamp(row['score_date'])
    symbol = row['symbol']
    history = p.fields['close'].loc[:date, symbol]
    assert result['median_amount_20d'] == pytest.approx(
        (p.fields['volume'][symbol] * p.fields['vwap'][symbol]).loc[:date].tail(20).median())
    assert result['log_return_std_30d'] == pytest.approx(np.log(history).diff().tail(30).std())
    assert result['listing_age_days'] == (date - history.first_valid_index()).days
    assert result['listing_under_90d'] is False
    p.fields['close'].loc[date + timedelta(days=1):, symbol] = 1e9
    assert s.vetoed_features([row], {'1': True}, p)[0] == result


def test_validate_outputs_checks_ledger_and_files(inputs):
    run(inputs)
    with inputs['ledger'].open('a') as stream:
        stream.write(json.dumps(dict(track=s.TRACK, idea_id='wrong', trades_sha256='wrong')) + '\n')
    with pytest.raises(ValueError, match='last crypto_llm'):
        s.validate_outputs(inputs['out'], inputs['ledger'], True)
    (inputs['out'] / 'mask.jsonl').write_text('{}\n')
    with pytest.raises(ValueError, match='sha256 mismatch'):
        s.validate_outputs(inputs['out'], inputs['ledger'], False)


def test_cli_help(capsys):
    with pytest.raises(SystemExit) as error:
        s.main(['--help'])
    assert error.value.code == 0
    assert '--no-ledger' in capsys.readouterr().out
