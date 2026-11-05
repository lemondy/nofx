"""Tests for the fq_us momentum S2 report (synthetic temp SQLite DBs only)."""

from contextlib import closing, redirect_stdout
from datetime import datetime, timedelta, timezone
import io
import json
import math
import random
import sqlite3
import statistics
import tempfile
import unittest
from pathlib import Path

from scripts.promotion import fq_us_s2_report as report

UTC = timezone.utc
START = datetime(2026, 11, 2, tzinfo=UTC)  # Monday
TRADER = 'fq_us_trader'
LEDGER_PATH = 'ledger.jsonl'

# Mirrors the production schema (sqlite3 -readonly .schema on the real DB).
SCHEMA = """
CREATE TABLE `trader_equity_snapshots` (`id` integer PRIMARY KEY AUTOINCREMENT,`trader_id` text NOT NULL,`timestamp` datetime NOT NULL,`total_equity` real NOT NULL DEFAULT 0,`balance` real NOT NULL DEFAULT 0,`unrealized_pnl` real NOT NULL DEFAULT 0,`position_count` integer DEFAULT 0,`margin_used_pct` real DEFAULT 0,`created_at` datetime);
CREATE TABLE `decision_records` (`id` integer PRIMARY KEY AUTOINCREMENT,`trader_id` text NOT NULL,`cycle_number` integer NOT NULL,`timestamp` datetime NOT NULL,`system_prompt` text DEFAULT "",`input_prompt` text DEFAULT "",`cot_trace` text DEFAULT "",`decision_json` text DEFAULT "",`raw_response` text DEFAULT "",`candidate_coins` text DEFAULT "",`execution_log` text DEFAULT "",`decisions` text DEFAULT "[]",`success` numeric DEFAULT false,`error_message` text DEFAULT "",`ai_request_duration_ms` integer DEFAULT 0,`created_at` datetime, `config_hash` text DEFAULT "", `candidate_verdicts` text DEFAULT "");
CREATE TABLE `factor_quant_state` (`trader_id` text,`last_as_of` text NOT NULL DEFAULT "",`last_run_id` text DEFAULT "",`last_rule` text DEFAULT "",`rebalance_at` integer DEFAULT 0,`last_skip_key` text DEFAULT "",`updated_at` integer DEFAULT 0, `retry_as_of` text NOT NULL DEFAULT "", `retry_attempts` integer NOT NULL DEFAULT 0, `peak_equity` real NOT NULL DEFAULT 0, `day_start_date` text NOT NULL DEFAULT "", `day_start_equity` real NOT NULL DEFAULT 0, `halt_kind` text NOT NULL DEFAULT "", `halt_day` text NOT NULL DEFAULT "", `halt_reason` text NOT NULL DEFAULT "", `halted_at` integer NOT NULL DEFAULT 0, `warn_active` numeric NOT NULL DEFAULT false, `kill_reset_applied` text NOT NULL DEFAULT "",PRIMARY KEY (`trader_id`));
"""


def ts_text(moment: datetime) -> str:
    """GORM-style SQLite datetime text, exactly as the Go store writes it."""
    return moment.astimezone(UTC).strftime('%Y-%m-%d %H:%M:%S.%f') + '+00:00'


def weekdays(count, first=START):
    """The first `count` Mon-Fri UTC dates from `first` (a Monday)."""
    out, day = [], first
    while len(out) < count:
        if day.weekday() < 5:
            out.append(day)
        day += timedelta(days=1)
    return out


def equity_returns_db(directory, returns, rebalances=6, first=START, base_equity=10000.0,
                      kill=None, rebalance_prefix=None, name='data.db'):
    """Baseline snapshot just before start, then 14:00 UTC of each weekday."""
    path = Path(directory) / name
    with closing(sqlite3.connect(path)) as conn:
        conn.executescript(SCHEMA)
        conn.execute('INSERT INTO trader_equity_snapshots (trader_id, timestamp, total_equity)'
                     ' VALUES (?, ?, ?)', (TRADER, ts_text(first - timedelta(hours=1)), base_equity))
        equity = base_equity
        for day, ret in zip(weekdays(len(returns), first), returns):
            equity *= 1.0 + ret
            conn.execute('INSERT INTO trader_equity_snapshots (trader_id, timestamp, total_equity)'
                         ' VALUES (?, ?, ?)', (TRADER, ts_text(day + timedelta(hours=14)), equity))
        prefix = rebalance_prefix or report.REBALANCE_PREFIX
        for i in range(rebalances):
            conn.execute(
                'INSERT INTO decision_records (trader_id, cycle_number, timestamp, cot_trace, success)'
                ' VALUES (?, ?, ?, ?, 1)',
                (TRADER, i + 1, ts_text(weekdays(1, first)[0] + timedelta(hours=10, minutes=i)),
                 f'{prefix}美股 bStock 模拟账本，仅模拟盘，未调用大模型）'))
        if kill:
            conn.execute('INSERT INTO factor_quant_state (trader_id, halt_kind, halted_at, halt_reason)'
                         ' VALUES (?, ?, ?, ?)', (TRADER, kill[0], kill[1], kill[2]))
        conn.commit()
    return str(path)


def run_report(db_path, ledger=None, out=None, start=START, trader=TRADER):
    argv = ['--db', db_path, '--trader-id', trader, '--start', report.iso(start),
            '--now', '2026-11-05T12:00:00Z']
    if out:
        argv += ['--out', str(out)]
    if ledger:
        argv += ['--ledger', str(ledger)]
    buffer = io.StringIO()
    with redirect_stdout(buffer):
        code = report.main(argv)
    return code, buffer.getvalue()


CONTROL = 'fq_us_control'
CANDIDATE = 'fq_us_candidate'
STAGE_EVENTS = [(i, f'2026-11-{i + 2:02d}') for i in range(6)]  # 6 shared as_of rebalances


def pair_equity_db(directory, control_returns, candidate_returns, control_drop=(),
                   candidate_drop=(), control_events=None, candidate_events=None,
                   control_kill=None, candidate_kill=None, base_equity=10000.0,
                   name='pair.db'):
    """Two-arm fixture mirroring equity_returns_db, once per trader.

    Each arm gets a baseline snapshot before start plus a 14:00 UTC snapshot
    per weekday return. `*_drop` are weekday indexes without snapshots for
    that arm (the common window shrinks to the intersection). `*_events` are
    [(weekday_index, as_of_or_None)] rebalance records; as_of None omits the
    `as_of=` line so the UTC-date fallback applies. `*_kill` are
    (halt_kind, halted_at_ms, halt_reason) factor_quant_state latches.
    """
    path = Path(directory) / name
    span = max(len(control_returns), len(candidate_returns))
    for events in (control_events, candidate_events):
        for day_index, _ in events or []:
            span = max(span, day_index + 1)
    days = weekdays(span, START)
    with closing(sqlite3.connect(path)) as conn:
        conn.executescript(SCHEMA)
        for trader, returns, drop, events, kill in (
                (CONTROL, control_returns, control_drop, control_events, control_kill),
                (CANDIDATE, candidate_returns, candidate_drop, candidate_events,
                 candidate_kill)):
            conn.execute('INSERT INTO trader_equity_snapshots (trader_id, timestamp,'
                         ' total_equity) VALUES (?, ?, ?)',
                         (trader, ts_text(START - timedelta(hours=1)), base_equity))
            equity = base_equity
            for i, ret in enumerate(returns):
                if i in drop:
                    continue
                equity *= 1.0 + ret
                conn.execute('INSERT INTO trader_equity_snapshots (trader_id, timestamp,'
                             ' total_equity) VALUES (?, ?, ?)',
                             (trader, ts_text(days[i] + timedelta(hours=14)), equity))
            for i, (day_index, as_of) in enumerate(events or []):
                trace = f'{report.REBALANCE_PREFIX}美股 bStock 模拟账本，仅模拟盘，未调用大模型）'
                if as_of is not None:
                    trace += f'\nas_of={as_of} 规则=mom_ls_invvol_v1 horizon=28 天 run_id='
                conn.execute('INSERT INTO decision_records (trader_id, cycle_number,'
                             ' timestamp, cot_trace, success) VALUES (?, ?, ?, ?, 1)',
                             (trader, i + 1,
                              ts_text(days[day_index] + timedelta(hours=10, minutes=i)),
                              trace))
            if kill:
                conn.execute('INSERT INTO factor_quant_state (trader_id, halt_kind,'
                             ' halted_at, halt_reason) VALUES (?, ?, ?, ?)',
                             (trader, kill[0], kill[1], kill[2]))
        conn.commit()
    return str(path)


def run_pair(db_path, ledger=None, out=None, start=START, control=CONTROL,
             candidate=CANDIDATE):
    argv = ['--db', db_path, '--control', control, '--candidate', candidate,
            '--start', report.iso(start), '--now', '2026-11-05T12:00:00Z']
    if out:
        argv += ['--out', str(out)]
    if ledger:
        argv += ['--ledger', str(ledger)]
    buffer = io.StringIO()
    with redirect_stdout(buffer):
        code = report.main(argv)
    return code, buffer.getvalue()


class DailySampling(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db = Path(self.tmp.name) / 'data.db'
        with closing(sqlite3.connect(self.db)) as conn:
            conn.executescript(SCHEMA)
            rows = [
                # Sunday before start: two snapshots, the later one anchors.
                (datetime(2026, 11, 1, 20), 90.0),
                (datetime(2026, 11, 1, 21), 95.0),
                # Monday: three intraday snapshots, last one must win.
                (START + timedelta(hours=1), 100.0),
                (START + timedelta(hours=9), 110.0),
                (START + timedelta(hours=15), 105.0),
                # Tuesday.
                (START + timedelta(days=1, hours=8), 200.0),
                (START + timedelta(days=1, hours=15), 210.0),
                # Weekend: must be dropped entirely.
                (START + timedelta(days=5, hours=8), 300.0),
                (START + timedelta(days=5, hours=15), 301.0),
                (START + timedelta(days=6, hours=12), 400.0),
                # Next Monday.
                (START + timedelta(days=7, hours=15), 500.0),
            ]
            for moment, equity in rows:
                conn.execute('INSERT INTO trader_equity_snapshots (trader_id, timestamp, total_equity)'
                             ' VALUES (?, ?, ?)', (TRADER, ts_text(moment), equity))
            conn.commit()

    def test_baseline_weekends_and_last_of_day(self):
        with closing(report.open_db(self.db)) as conn:
            baseline, points = report.load_snapshots(conn, TRADER, START)
        self.assertEqual(baseline[1], 95.0)
        self.assertEqual([equity for _, equity in points], [105.0, 210.0, 500.0])
        self.assertEqual([moment.weekday() for moment, _ in points], [0, 1, 0])
        returns = report.returns_over(baseline, points)
        self.assertEqual(returns, [105.0 / 95.0 - 1, 210.0 / 105.0 - 1, 500.0 / 210.0 - 1])

    def test_baseline_inside_start_date(self):
        """A mid-day start anchors on the earlier snapshot, not the later one."""
        start = START + timedelta(hours=12)
        with closing(report.open_db(self.db)) as conn:
            baseline, points = report.load_snapshots(conn, TRADER, start)
        self.assertEqual(baseline[0], START + timedelta(hours=9))
        self.assertEqual([equity for _, equity in points], [105.0, 210.0, 500.0])

    def test_baseline_falls_back_to_first_after_start(self):
        start = datetime(2026, 11, 8, 12, tzinfo=UTC)  # Sunday, nothing at/before it
        with closing(report.open_db(self.db)) as conn:
            baseline, points = report.load_snapshots(conn, TRADER, start)
        self.assertEqual(baseline[1], 400.0)  # the Sunday snapshot anchors…
        # …but a weekend anchor is never reused as a daily sample point.
        self.assertEqual([equity for _, equity in points], [500.0])

    def test_no_snapshots(self):
        with closing(sqlite3.connect(self.db)) as conn, closing(report.open_db(self.db)) as ro:
            conn.execute('DELETE FROM trader_equity_snapshots')
            conn.commit()
            baseline, points = report.load_snapshots(ro, TRADER, START)
        self.assertIsNone(baseline)
        self.assertEqual(points, [])
        self.assertEqual(report.returns_over(baseline, points), [])


class NeweyWest(unittest.TestCase):
    def _reference(self, series, lag):
        """Independent pure-python transcription of scripts/factor_mining/evaluate.nw_t."""
        x = [float(v) for v in series]
        n = len(x)
        mean = sum(x) / n
        centered = [v - mean for v in x]
        variance = sum(c * c for c in centered) / n
        for k in range(1, min(lag, n - 1) + 1):
            covariance = sum(a * b for a, b in zip(centered[k:], centered)) / n
            variance += 2 * (1 - k / (lag + 1)) * covariance
        return mean / math.sqrt(variance / n)

    def test_known_series_matches_reference(self):
        sine = [0.001 + 0.02 * math.sin(2 * math.pi * i / 60) for i in range(126)]
        rnd = random.Random(11)
        noise = [rnd.gauss(0.001, 0.002) for _ in range(130)]
        for series in (sine, noise):
            self.assertAlmostEqual(report.hac_z(series, 20), self._reference(series, 20), places=8)
            self.assertAlmostEqual(report.nw_t_fallback(series, 20),
                                   report.nw_t(series, 20) if report.nw_t else 0.0, places=6)

    def test_edge_cases_match_frozen_evaluator(self):
        self.assertEqual(report.hac_z([0.01] * 10, 20), 0.0)  # n < 20 -> 0.0
        self.assertEqual(report.hac_z([0.01] * 40, 20), 1e12)  # zero variance, positive mean
        self.assertEqual(report.hac_z([-0.01] * 40, 20), -1e12)
        self.assertEqual(report.hac_z([0.0] * 40, 20), 0.0)
        self.assertEqual(report.hac_z([0.01, float('nan')] * 20, 20), 1e12)  # non-finite dropped

    def test_max_drawdown(self):
        baseline = (START, 100.0)
        points = [(START + timedelta(days=i), equity)
                  for i, equity in enumerate([130.0, 91.0, 120.0], start=1)]
        self.assertAlmostEqual(report.max_drawdown(baseline, points), 0.30)


class Verdicts(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def verdict_for(self, returns, rebalances=6, kill=None):
        self.db_counter = getattr(self, 'db_counter', 0) + 1
        db = equity_returns_db(self.tmp.name, returns, rebalances,
                               name=f'data-{self.db_counter}.db', kill=kill)
        with closing(report.open_db(db)) as conn:
            return report.build_report(conn, TRADER, START, START + timedelta(days=1))

    def test_pending_when_days_missing(self):
        good = [0.001] * 100
        result = self.verdict_for(good, rebalances=6)
        self.assertEqual(result['verdict'], 'pending')
        self.assertEqual(result['sample_stage'], 'stage_1')
        self.assertIn('样本不足', ''.join(result['reasons']))

    def test_pending_when_rebalances_missing(self):
        result = self.verdict_for([0.001] * 130, rebalances=3)
        self.assertEqual(result['verdict'], 'pending')
        self.assertIn('调仓不足', ''.join(result['reasons']))

    def test_fail_on_negative_cumulative(self):
        result = self.verdict_for([-0.001] * 130)
        self.assertEqual(result['verdict'], 'fail')
        self.assertLess(result['cum_return'], 0)
        self.assertFalse(result['kill_fired'])

    def test_fail_on_drawdown_latch(self):
        fired = int((START + timedelta(hours=1)).timestamp() * 1000)
        result = self.verdict_for([0.001] * 130, kill=('drawdown', fired, '回撤熔断'))
        self.assertEqual(result['verdict'], 'fail')
        self.assertEqual(result['kill_sources'], ['state_latch'])
        self.assertTrue(result['kill_fired'])

    def test_latch_before_start_is_not_a_window_kill(self):
        before = int((START - timedelta(days=1)).timestamp() * 1000)
        result = self.verdict_for([0.001] * 130, kill=('drawdown', before, '回撤熔断'))
        self.assertNotEqual(result['verdict'], 'fail')
        self.assertFalse(result['kill_fired'])

    def test_fail_on_computed_drawdown_fallback(self):
        # 40% single-day loss with recovery: dd >= 25% fires even without a latch row.
        returns = [-0.4] + [0.006] * 129
        result = self.verdict_for(returns)
        self.assertGreaterEqual(result['max_drawdown'], 0.25)
        self.assertEqual(result['kill_sources'], ['computed_drawdown'])
        self.assertEqual(result['verdict'], 'fail')

    def test_pass(self):
        rnd = random.Random(11)
        result = self.verdict_for([rnd.gauss(0.001, 0.002) for _ in range(130)])
        self.assertGreaterEqual(result['nw_z'], 1.65)
        self.assertGreater(result['cum_return'], 0)
        self.assertEqual(result['verdict'], 'pass')
        self.assertEqual(result['sample_stage'], 'extension')

    def test_inconclusive_then_extension_final(self):
        def sine(n):
            return [0.001 + 0.01 * math.sin(2 * math.pi * i / 60) for i in range(n)]

        first = self.verdict_for(sine(126))
        self.assertEqual(first['verdict'], 'inconclusive')
        self.assertEqual(first['sample_stage'], 'extension')
        final = self.verdict_for(sine(254))
        self.assertEqual(final['verdict'], 'fail_extended_inconclusive')
        self.assertEqual(final['sample_stage'], 'extension_final')
        self.assertIn('fail_extended_inconclusive', ''.join(final['reasons']))


class RebalanceCounting(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db = Path(self.tmp.name) / 'data.db'
        prefix = report.REBALANCE_PREFIX
        rows = [  # (timestamp, success, cot_trace)
            (START + timedelta(hours=12), 1, prefix + '美股…'),          # count
            (START + timedelta(hours=13), 1, prefix + '美股…'),          # count
            (START - timedelta(days=1), 1, prefix + '美股…'),            # before start
            (START + timedelta(hours=14), 1, '因子量化 · 未调仓（…'),     # skip tick
            (START + timedelta(hours=15), 0, prefix + '美股…'),          # aborted
            (START + timedelta(hours=16), 1, '无关的 AI 决策'),           # other trader noise
        ]
        with closing(sqlite3.connect(self.db)) as conn:
            conn.executescript(SCHEMA)
            for i, (moment, success, trace) in enumerate(rows):
                conn.execute('INSERT INTO decision_records (trader_id, cycle_number, timestamp,'
                             ' cot_trace, success) VALUES (?, ?, ?, ?, ?)',
                             (TRADER, i + 1, ts_text(moment), trace, success))
            conn.commit()

    def test_counts_only_successful_rebalances_since_start(self):
        with closing(report.open_db(self.db)) as conn:
            self.assertEqual(report.count_rebalances(conn, TRADER, START), 2)
            self.assertEqual(report.count_rebalances(conn, 'other_trader', START), 0)


class ReadOnlyOpen(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def test_write_is_refused(self):
        db = equity_returns_db(self.tmp.name, [0.001] * 21)
        with closing(report.open_db(db)) as conn:
            with self.assertRaises(sqlite3.OperationalError):
                conn.execute('INSERT INTO trader_equity_snapshots (trader_id, timestamp,'
                             ' total_equity) VALUES (?, ?, ?)', ('x', '2026-01-01', 1.0))

    def test_missing_file_is_refused(self):
        with self.assertRaises(sqlite3.OperationalError):
            report.open_db(Path(self.tmp.name) / 'absent.db')


class LedgerAndOutput(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.ledger = Path(self.tmp.name) / LEDGER_PATH

    def test_pass_appends_expected_row(self):
        rnd = random.Random(11)
        db = equity_returns_db(self.tmp.name, [rnd.gauss(0.001, 0.002) for _ in range(130)])
        code, printed = run_report(db, ledger=self.ledger, out=Path(self.tmp.name) / 'out')
        self.assertEqual(code, 0)
        self.assertIn('判定：pass', printed)
        self.assertTrue((Path(self.tmp.name) / 'out' / 'report.json').exists())
        self.assertTrue((Path(self.tmp.name) / 'out' / 'report.md').exists())
        markdown = (Path(self.tmp.name) / 'out' / 'report.md').read_text(encoding='utf-8')
        self.assertIn('预注册', markdown)
        rows = report.read_rows(self.ledger)
        self.assertEqual(len(rows), 1)
        row = rows[0]
        self.assertEqual(row['track'], 'fq_us')
        self.assertEqual(row['stage'], 'S2')
        self.assertEqual(row['change_type'], 'E')
        self.assertEqual(row['idea_id'], 'fq-us-momentum-ls-v1')
        self.assertEqual(row['baseline_version'], 'fq_us:no_validated_factors')
        self.assertEqual(row['verdict'], 'pass')
        self.assertIsNone(row['K'])  # S2 never counts toward K
        self.assertFalse(row['counts_toward_k'])
        self.assertIn('n_days', row)
        payload = json.loads((Path(self.tmp.name) / 'out' / 'report.json').read_text())
        self.assertEqual(payload['verdict'], 'pass')
        self.assertIn('decision_records', payload['rebalance_query'])

    def test_pending_skips_ledger(self):
        db = equity_returns_db(self.tmp.name, [0.001] * 100)
        code, printed = run_report(db, ledger=self.ledger)
        self.assertEqual(code, 0)
        self.assertIn('判定：pending', printed)
        self.assertIn('不写 evolution_ledger', printed)
        self.assertFalse(self.ledger.exists())

    def test_bad_start_is_a_clean_error(self):
        db = equity_returns_db(self.tmp.name, [0.001] * 21)
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = report.main(['--db', db, '--trader-id', TRADER, '--start', 'not-a-date'])
        self.assertEqual(code, 1)


class PairMode(unittest.TestCase):
    """Pair mode (--control/--candidate) per the volscale pair prereg."""

    maxDiff = None

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def build(self, **kwargs):
        db = pair_equity_db(self.tmp.name, name=f'pair-{self.id()}.db', **kwargs)
        with closing(report.open_db(db)) as conn:
            return report.build_pair_report(conn, CONTROL, CANDIDATE, START,
                                            START + timedelta(days=1))

    # -- fixtures ---------------------------------------------------------
    def test_common_window_is_the_intersection(self):
        """Dates present in both series; returns computed on those dates only."""
        data = self.build(control_returns=[0.0, 0.0, 0.1, -0.1, 0.05],
                          candidate_returns=[0.02] * 5, candidate_drop={1, 3})
        self.assertEqual(data['window_dates'], ['2026-11-02', '2026-11-04', '2026-11-06'])
        self.assertEqual(data['n_days'], 3)
        self.assertEqual(data['control']['n_days'], 3)
        self.assertEqual(data['candidate']['n_days'], 3)
        # Control compounds all five returns but is only sampled on common dates.
        for got, want in zip(data['control']['daily_returns'],
                             [0.0, 0.1, 10395.0 / 11000.0 - 1.0]):
            self.assertAlmostEqual(got, want, places=12)
        for got, want in zip(data['candidate']['daily_returns'], [0.02, 0.02, 0.02]):
            self.assertAlmostEqual(got, want, places=12)

    def test_per_arm_sharpe_and_max_drawdown(self):
        data = self.build(control_returns=[0.01, -0.03, 0.02, -0.01, 0.005],
                          candidate_returns=[0.02, -0.01, 0.015, 0.0, -0.02])
        for arm in ('control', 'candidate'):
            returns = data[arm]['daily_returns']
            expected = (sum(returns) / len(returns)) / statistics.stdev(returns) * math.sqrt(252)
            self.assertAlmostEqual(data[arm]['sharpe'], expected, places=12)
        # equity paths: control 10000→10100→9797→… (trough 9797), candidate dd exactly 2%
        self.assertAlmostEqual(data['control']['max_drawdown'], 0.03)
        self.assertAlmostEqual(data['candidate']['max_drawdown'], 0.02)
        cum = 1.0
        for ret in (0.02, -0.01, 0.015, 0.0, -0.02):
            cum *= 1.0 + ret
        self.assertAlmostEqual(data['candidate']['cum_return'], cum - 1.0, places=12)

    def test_candidate_without_snapshots_is_pending(self):
        data = self.build(control_returns=[0.001] * 10, candidate_returns=[])
        self.assertEqual(data['window_dates'], [])
        self.assertEqual(data['n_days'], 0)
        self.assertEqual(data['verdict'], 'pending')

    # -- verdict branches -------------------------------------------------
    def test_early_pass_when_only_control_kills(self):
        fired = int((START + timedelta(hours=2)).timestamp() * 1000)
        data = self.build(control_returns=[0.01] * 5, candidate_returns=[0.01] * 5,
                          control_kill=('drawdown', fired, '回撤熔断'))
        self.assertEqual(data['verdict'], 'pass')
        self.assertEqual(data['control']['kill_sources'], ['state_latch'])
        self.assertFalse(data['candidate']['kill_fired'])
        self.assertIn('提前判 pass', ''.join(data['reasons']))

    def test_early_fail_when_candidate_kills(self):
        fired = int((START + timedelta(hours=2)).timestamp() * 1000)
        data = self.build(control_returns=[0.01] * 5, candidate_returns=[0.01] * 5,
                          candidate_kill=('drawdown', fired, '回撤熔断'))
        self.assertEqual(data['verdict'], 'fail')
        self.assertEqual(data['candidate']['kill_sources'], ['state_latch'])

    def test_both_killed_fails_on_candidate_rule(self):
        fired = int((START + timedelta(hours=2)).timestamp() * 1000)
        kill = ('drawdown', fired, '回撤熔断')
        data = self.build(control_returns=[0.01] * 5, candidate_returns=[0.01] * 5,
                          control_kill=kill, candidate_kill=kill)
        self.assertEqual(data['verdict'], 'fail')

    def test_pending_when_common_days_missing(self):
        data = self.build(control_returns=[0.001] * 100, candidate_returns=[0.001] * 100,
                          control_events=STAGE_EVENTS, candidate_events=STAGE_EVENTS)
        self.assertEqual(data['verdict'], 'pending')
        self.assertEqual(data['common_rebalances'], 6)
        self.assertEqual(data['sample_stage'], 'stage_1')
        self.assertIn('样本不足', ''.join(data['reasons']))

    def test_pending_when_common_rebalances_missing(self):
        events = [(i, f'2026-11-{i + 2:02d}') for i in range(3)]
        data = self.build(control_returns=[0.001] * 130, candidate_returns=[0.001] * 130,
                          control_events=events, candidate_events=events)
        self.assertEqual(data['verdict'], 'pending')
        self.assertEqual(data['common_rebalances'], 3)
        self.assertIn('共同调仓不足', ''.join(data['reasons']))

    def test_stage_one_pass(self):
        control = [0.001] * 130
        control[10] = -0.10  # 10% drawdown, weak Sharpe
        data = self.build(control_returns=control,
                          candidate_returns=[0.001, 0.003] * 65,  # dd 0, Sharpe ~31.7
                          control_events=STAGE_EVENTS, candidate_events=STAGE_EVENTS)
        self.assertEqual(data['n_days'], 130)
        self.assertEqual(data['sample_stage'], 'extension')
        self.assertGreater(data['candidate']['sharpe'], data['control']['sharpe'])
        self.assertLess(data['candidate']['max_drawdown'], data['control']['max_drawdown'])
        self.assertEqual(data['verdict'], 'pass')

    def test_stage_one_fail(self):
        candidate = [0.001] * 130
        candidate[10] = -0.10
        data = self.build(control_returns=[0.001, 0.003] * 65,
                          candidate_returns=candidate,
                          control_events=STAGE_EVENTS, candidate_events=STAGE_EVENTS)
        self.assertLess(data['candidate']['sharpe'], data['control']['sharpe'])
        self.assertGreaterEqual(data['candidate']['max_drawdown'],
                                data['control']['max_drawdown'])
        self.assertEqual(data['verdict'], 'fail')

    def test_stage_one_inconclusive(self):
        # Candidate Sharpe is fine but its MaxDD is larger: conditions disagree.
        data = self.build(control_returns=[0.0005, -0.0005] * 65,
                          candidate_returns=[0.003] * 129 + [-0.03],
                          control_events=STAGE_EVENTS, candidate_events=STAGE_EVENTS)
        self.assertEqual(data['n_days'], 130)
        self.assertGreaterEqual(data['candidate']['sharpe'], data['control']['sharpe'])
        self.assertGreater(data['candidate']['max_drawdown'], data['control']['max_drawdown'])
        self.assertEqual(data['verdict'], 'inconclusive')
        self.assertIn('互有胜负', ''.join(data['reasons']))

    def test_extension_pass(self):
        control = [0.001] * 260
        control[10] = -0.10
        data = self.build(control_returns=control,
                          candidate_returns=[0.001, 0.003] * 130,
                          control_events=STAGE_EVENTS, candidate_events=STAGE_EVENTS)
        self.assertEqual(data['n_days'], 260)
        self.assertEqual(data['sample_stage'], 'extension_final')
        self.assertEqual(data['verdict'], 'pass')

    def test_extension_fail_even_when_dd_is_smaller(self):
        # n >= 252 needs BOTH conditions: weak candidate Sharpe fails outright.
        control = [0.004] * 259 + [-0.20]  # dd 20% < 25%, no kill; Sharpe ~4
        data = self.build(control_returns=control,
                          candidate_returns=[-0.0005, -0.0007] * 130,  # dd ~8%, Sharpe << 0
                          control_events=STAGE_EVENTS, candidate_events=STAGE_EVENTS)
        self.assertEqual(data['n_days'], 260)
        self.assertLess(data['candidate']['sharpe'], data['control']['sharpe'])
        self.assertLess(data['candidate']['max_drawdown'], data['control']['max_drawdown'])
        self.assertFalse(data['control']['kill_fired'])
        self.assertFalse(data['candidate']['kill_fired'])
        self.assertEqual(data['verdict'], 'fail')

    # -- common rebalance counting ---------------------------------------
    def test_common_rebalances_intersect_by_as_of(self):
        control = [(0, '2026-11-02'), (1, '2026-11-03'), (2, '2026-11-04')]
        candidate = [(0, '2026-11-03'), (1, '2026-11-04'), (2, '2026-11-06')]
        data = self.build(control_returns=[0.0], candidate_returns=[0.0],
                          control_events=control, candidate_events=candidate)
        self.assertEqual(data['common_rebalance_asofs'], ['2026-11-03', '2026-11-04'])
        self.assertEqual(data['common_rebalances'], 2)
        self.assertEqual(data['control']['rebalances'], 3)  # own counts kept for reference
        self.assertEqual(data['candidate']['rebalances'], 3)

    def test_rebalance_fallback_matches_utc_date(self):
        """No as_of line in cot_trace → the record's UTC date is the key."""
        data = self.build(control_returns=[0.0], candidate_returns=[0.0],
                          control_events=[(0, None), (1, None)],
                          candidate_events=[(0, None)])
        self.assertEqual(data['common_rebalance_asofs'], ['2026-11-02'])
        self.assertEqual(data['common_rebalances'], 1)

    def test_rebalance_asofs_do_not_cross_arms_when_dates_differ(self):
        data = self.build(control_returns=[0.0], candidate_returns=[0.0],
                          control_events=[(0, '2026-11-05')],
                          candidate_events=[(0, '2026-11-02')])
        self.assertEqual(data['common_rebalances'], 0)
        self.assertEqual(data['verdict'], 'pending')

    # -- output: stdout / files / ledger ----------------------------------
    def test_pass_appends_pair_ledger_row_and_writes_reports(self):
        control = [0.001] * 130
        control[10] = -0.10
        db = pair_equity_db(self.tmp.name, control, [0.001, 0.003] * 65,
                            control_events=STAGE_EVENTS, candidate_events=STAGE_EVENTS,
                            name='pair-ledger.db')
        ledger = Path(self.tmp.name) / LEDGER_PATH
        out = Path(self.tmp.name) / 'out'
        code, printed = run_pair(db, ledger=ledger, out=out)
        self.assertEqual(code, 0)
        self.assertIn('fq_us S2 配对报告', printed)
        self.assertIn('fq-us-volscale-pair-v1', printed)
        self.assertIn('判定：pass', printed)
        self.assertTrue((out / 'report.json').exists())
        self.assertTrue((out / 'report.md').exists())
        payload = json.loads((out / 'report.json').read_text())
        self.assertEqual(payload['verdict'], 'pass')
        self.assertEqual(payload['common_rebalances'], 6)
        self.assertIn('as_of', payload['rebalance_key_method'])
        markdown = (out / 'report.md').read_text(encoding='utf-8')
        self.assertIn('波动率缩放在线配对', markdown)
        self.assertIn('mom_ls_invvol_volscale_v1', markdown)
        rows = report.read_rows(ledger)
        self.assertEqual(len(rows), 1)
        row = rows[0]
        self.assertEqual(row['track'], 'fq_us')
        self.assertEqual(row['stage'], 'S2')
        self.assertEqual(row['change_type'], 'C')
        self.assertEqual(row['idea_id'], 'fq-us-volscale-pair-v1')
        self.assertEqual(row['baseline_version'], 'mom_ls_invvol_v1')
        self.assertEqual(row['verdict'], 'pass')
        self.assertIsNone(row['K'])  # S2 never counts toward K
        self.assertFalse(row['counts_toward_k'])
        self.assertEqual(row['n_days'], 130)
        self.assertEqual(row['common_rebalances'], 6)
        self.assertEqual(row['control']['trader_id'], CONTROL)
        self.assertEqual(row['candidate']['trader_id'], CANDIDATE)
        self.assertIn('sharpe', row['control'])
        self.assertIn('max_drawdown', row['candidate'])

    def test_pending_skips_ledger(self):
        db = pair_equity_db(self.tmp.name, [0.001] * 100, [0.001] * 100,
                            name='pair-pending.db')
        ledger = Path(self.tmp.name) / LEDGER_PATH
        code, printed = run_pair(db, ledger=ledger)
        self.assertEqual(code, 0)
        self.assertIn('判定：pending', printed)
        self.assertIn('不写 evolution_ledger', printed)
        self.assertFalse(ledger.exists())

    def test_missing_trader_ids_are_a_clean_error(self):
        db = pair_equity_db(self.tmp.name, [0.001], [0.001], name='pair-args.db')
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = report.main(['--db', db, '--control', CONTROL,
                                '--start', report.iso(START)])
        self.assertEqual(code, 1)


class SingleModeUnchanged(unittest.TestCase):
    """The pair mode must not alter single-trader behavior."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def test_single_mode_report_and_cli_on_pair_fixture(self):
        control = [0.001] * 130
        control[10] = -0.10  # dd 10%, no kill; single rules decide on z
        db = pair_equity_db(self.tmp.name, control, [0.001, 0.003] * 65,
                            control_events=STAGE_EVENTS, candidate_events=STAGE_EVENTS,
                            name='single.db')
        with closing(report.open_db(db)) as conn:
            data = report.build_report(conn, CONTROL, START, START + timedelta(days=1))
        self.assertEqual(data['idea_id'], 'fq-us-momentum-ls-v1')
        self.assertEqual(data['n_days'], 130)
        self.assertEqual(data['rebalances'], 6)
        self.assertAlmostEqual(data['max_drawdown'], 0.10, places=12)
        self.assertFalse(data['kill_fired'])
        self.assertNotIn(data['verdict'], ('pending', 'fail'))  # cum > 0, no kill
        ledger = Path(self.tmp.name) / LEDGER_PATH
        code, printed = run_report(db, ledger=ledger, start=START, trader=CONTROL)
        self.assertEqual(code, 0)
        self.assertIn(f"fq_us S2 报告（{report.IDEA_ID}，预注册口径）", printed)
        self.assertIn(f"交易员 {CONTROL}", printed)
        rows = report.read_rows(ledger)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]['change_type'], 'E')

    def test_conflicting_trader_and_pair_args_are_refused(self):
        db = pair_equity_db(self.tmp.name, [0.001], [0.001], name='mix.db')
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = report.main(['--db', db, '--trader-id', CONTROL, '--control', CONTROL,
                                '--candidate', CANDIDATE, '--start', report.iso(START)])
        self.assertEqual(code, 1)

    def test_no_trader_args_are_refused(self):
        db = pair_equity_db(self.tmp.name, [0.001], [0.001], name='none.db')
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = report.main(['--db', db, '--start', report.iso(START)])
        self.assertEqual(code, 1)


if __name__ == '__main__':
    unittest.main()
