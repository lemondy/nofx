"""Tests for the fq_us momentum S2 report (synthetic temp SQLite DBs only)."""

from contextlib import closing, redirect_stdout
from datetime import datetime, timedelta, timezone
import io
import json
import math
import random
import sqlite3
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


def run_report(db_path, ledger=None, out=None, start=START):
    argv = ['--db', db_path, '--trader-id', TRADER, '--start', report.iso(start),
            '--now', '2026-11-05T12:00:00Z']
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


if __name__ == '__main__':
    unittest.main()
