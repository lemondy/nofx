"""Synthetic-only unittest coverage of the frozen macro-event S0 definitions."""

from contextlib import redirect_stderr
import csv
from datetime import datetime, timezone
import hashlib
import io
import json
import math
from pathlib import Path
import random
import sqlite3
import tempfile
import unittest
from unittest.mock import Mock, patch

import numpy as np

from scripts.backtest import macro_event_study as study


HOUR = study.HOUR_MS
WEEK = study.WEEK_MS
BASE = int(datetime(2024, 1, 1, tzinfo=timezone.utc).timestamp() * 1000)
HEADER = ",".join(study.EVENT_HEADER) + "\n"


def candles(n=70, start=BASE):
    return [[start + i * HOUR, 100., 101., 99., 100., 1., 100.] for i in range(n)]


def lookup_cache(observations, *, ranges=None, returns=None):
    """Build small already-precomputed tables to isolate pairing/aggregation."""
    times = sorted(observations)
    values = np.array([observations[t] for t in times], dtype=float)
    range_values = values.copy() if ranges is None else np.array([ranges[t] for t in times])
    return_values = values.copy() if returns is None else np.array([returns[t] for t in times])
    return study.SymbolCache({t: i for i, t in enumerate(times)}, np.ones(len(times)),
                             {key: values.copy() for key in ((k, h) for k in study.KS for h in study.HORIZONS)},
                             {h: range_values.copy() for h in study.HORIZONS}, return_values, {})


class EventInputTests(unittest.TestCase):
    def test_et_conversion_across_spring_dst(self):
        before = study.et_to_utc_ms("2024-03-08", "08:30")
        after = study.et_to_utc_ms("2024-03-12", "08:30")
        self.assertEqual(study.utc_text(before), "2024-03-08T13:30:00+00:00")
        self.assertEqual(study.utc_text(after), "2024-03-12T12:30:00+00:00")
        self.assertEqual(after, study.et_to_utc_ms("2024-03-12", "08:30:00"))

    def test_csv_preserves_metadata_and_sorts_events(self):
        events = study.parse_events(HEADER +
                                   'CPI,2024-03-12,08:30,2024-02,BLS,"a,b"\n' +
                                   'NFP,2024-03-08,08:30,2024-02,BLS,\n')
        self.assertEqual([e.event_type for e in events], ["NFP", "CPI"])
        self.assertEqual(events[1].note, "a,b")
        self.assertEqual(events[1].source, "BLS")
        self.assertEqual(study.event_inventory(events), dict(
            total=2, by_type=dict(FOMC=0, CPI=1, NFP=1),
            by_year={"2024": 2}, by_year_type={"2024": dict(FOMC=0, CPI=1, NFP=1)}))

    def test_csv_rejects_invalid_headers_types_dates_times_and_duplicates(self):
        valid = "CPI,2024-03-12,08:30,2024-02,BLS,\n"
        invalid = ["", HEADER, "bad,header\n" + valid,
                   HEADER.replace("event_type", "type") + valid,
                   HEADER + valid.replace("CPI", "GDP"),
                   HEADER + valid.replace("2024-03-12", "2024-02-30"),
                   HEADER + valid.replace("2024-03-12", "2024-3-12"),
                   HEADER + valid.replace("08:30", "8:30"),
                   HEADER + valid.replace("08:30", "25:30"),
                   HEADER + valid.replace("08:30", "08:60"),
                   HEADER + valid.replace("08:30", "08:30:60"),
                   HEADER + valid.replace("08:30", "08:30Z"),
                   HEADER + "CPI,2024-03-12,08:30\n",
                   HEADER + valid.rstrip("\n") + ",extra\n",
                   HEADER + valid + valid.replace("08:30", "09:00")]
        for content in invalid:
            with self.subTest(content=content), self.assertRaises(ValueError):
                study.parse_events(content)
        with self.assertRaises(csv.Error):
            study.parse_events(HEADER + 'CPI,2024-03-12,08:30,2024-02,BLS,"unclosed\n')

    def test_nonexistent_and_ambiguous_et_times_are_rejected(self):
        for date, time in (("2024-03-10", "02:30"), ("2024-11-03", "01:30")):
            with self.subTest(date=date), self.assertRaises(ValueError):
                study.et_to_utc_ms(date, time)

    def test_processing_windows_have_six_hours_for_both_release_times(self):
        for date, time, utc_hours, leads in (
                ("2024-03-12", "08:30", [7, 8, 9, 10, 11, 12], [.5, 1.5, 2.5, 3.5, 4.5, 5.5]),
                ("2024-03-12", "14:00", [12, 13, 14, 15, 16, 17], [1, 2, 3, 4, 5, 6])):
            with self.subTest(time=time):
                release = study.et_to_utc_ms(date, time)
                entries = study.treatment_times(release)
                self.assertEqual(len(entries), 6)
                self.assertEqual([datetime.fromtimestamp(t / 1000, timezone.utc).hour for t in entries], utc_hours)
                self.assertEqual(sorted((release - t) / HOUR for t in entries), leads)
                self.assertTrue(all(release - 6 * HOUR <= t < release for t in entries))


class CandleTests(unittest.TestCase):
    def test_atr_uses_exactly_fourteen_prior_true_ranges_and_prior_close(self):
        rows = candles()
        rows[0][3:5] = [89., 90.]
        cache = study.precompute(rows)
        self.assertTrue(np.isnan(cache.atr[:15]).all())
        self.assertAlmostEqual(cache.atr[15], (11 + 13 * 2) / 14)
        self.assertEqual(cache.atr[16], 2.)
        for row in rows[15:]:
            row[1:5] = [100., 1000., 1., 500.]
        changed = study.precompute(rows)
        self.assertEqual(changed.atr[15], cache.atr[15])

    def test_long_short_touch_and_inclusive_last_bar_exclude_bar_h_plus_one(self):
        entry = 20
        for horizon in study.HORIZONS:
            for high, low, expected in ((101, 99, 0.), (103, 99, .5),
                                        (101, 97, .5), (103, 97, 1.)):
                with self.subTest(horizon=horizon, high=high, low=low):
                    rows = candles()
                    rows[entry + horizon - 1][2:4] = [high, low]
                    rows[entry + horizon][2:4] = [1000, 1]
                    cache = study.precompute(rows)
                    self.assertEqual(cache.hits[1.5, horizon][entry], expected)
        rows = candles()
        rows[entry][2:4] = [103, 97]
        self.assertEqual(study.precompute(rows).hits[study.MAIN][entry], 1.)

    def test_all_grid_values_match_an_independent_scalar_calculation(self):
        rows = candles(100)
        for i, row in enumerate(rows):
            row[1:5] = [100 + i % 5, 107 + i % 9, 94 - i % 4, 100 + i % 7]
        del rows[60]
        cache = study.precompute(rows)
        by_time = {row[0]: row for row in rows}
        for (k, horizon), values in cache.hits.items():
            for row in rows:
                t = row[0]
                needed = [by_time.get(t + j * HOUR) for j in range(-15, horizon)]
                actual = values[cache.index[t]]
                if any(bar is None for bar in needed):
                    self.assertTrue(math.isnan(actual))
                    continue
                prior = needed[:15]
                atr = sum(max(prior[j][2], prior[j - 1][4]) -
                          min(prior[j][3], prior[j - 1][4]) for j in range(1, 15)) / 14
                future = needed[15:]
                expected = (int(min(bar[3] for bar in future) <= row[1] - k * atr) +
                            int(max(bar[2] for bar in future) >= row[1] + k * atr)) / 2
                self.assertEqual(actual, expected)

    def test_missing_atr_or_observation_candle_discards_entry(self):
        entry = BASE + 25 * HOUR
        for missing in (10, 15, 24, 25, 30, 36):
            with self.subTest(missing=missing):
                rows = candles()
                del rows[missing]
                cache = study.precompute(rows)
                position = cache.index.get(entry)
                self.assertTrue(position is None or math.isnan(cache.hits[study.MAIN][position]))
                self.assertEqual(cache.metadata["missing_hours"], 1)
                self.assertEqual(cache.metadata["gap_intervals"], 1)
        rows = candles()
        del rows[9]  # Outside t-15h .. t+H-1h.
        cache = study.precompute(rows)
        self.assertEqual(cache.hits[study.MAIN][cache.index[entry]], 0.)
        self.assertTrue(math.isnan(cache.hits[study.MAIN][-1]))

    def test_missing_hours_are_not_filled_and_returns_need_previous_close(self):
        rows = candles()
        del rows[20:23]
        cache = study.precompute(rows)
        self.assertNotIn(BASE + 20 * HOUR, cache.index)
        self.assertEqual(cache.metadata["missing_hours"], 3)
        self.assertEqual(cache.metadata["gap_intervals"], 1)
        self.assertTrue(math.isnan(cache.abs_returns[cache.index[BASE + 23 * HOUR]]))
        self.assertEqual(cache.abs_returns[cache.index[BASE + 24 * HOUR]], 0.)
        self.assertTrue(math.isnan(cache.atr[cache.index[BASE + 37 * HOUR]]))
        self.assertEqual(cache.atr[cache.index[BASE + 38 * HOUR]], 2.)

    def test_start_date_and_zero_atr(self):
        rows = candles(start=study.START_MS - 20 * HOUR)
        cache = study.precompute(rows)
        self.assertTrue(np.isnan(cache.hits[study.MAIN][:20]).all())
        self.assertEqual(cache.hits[study.MAIN][20], 0.)
        for row in rows:
            row[1:5] = [100.] * 4
        cache = study.precompute(rows)
        self.assertEqual(cache.atr[20], 0.)
        self.assertEqual(cache.hits[study.MAIN][20], 1.)
        self.assertTrue(math.isnan(cache.ranges[12][20]))

    def test_candle_validation(self):
        cases = [[], [[0, 1]], candles(2)[::-1]]
        for column, value in ((0, BASE + 1), (1, 0), (2, 98), (3, 102), (4, float("nan")), (5, -1)):
            rows = candles(2)
            rows[0][column] = value
            cases.append(rows)
        cases.append([candles(1)[0], candles(1)[0]])
        for rows in cases:
            with self.subTest(rows=rows), self.assertRaises(ValueError):
                study.precompute(rows)


class MatchingAndStatisticsTests(unittest.TestCase):
    def test_controls_have_eight_weekly_candidates_and_exact_48h_is_allowed(self):
        entry = BASE + 100 * HOUR
        events = [study.Event("CPI", entry + HOUR),
                  study.Event("NFP", entry - WEEK + study.EXCLUSION_MS - 1),
                  study.Event("FOMC", entry + WEEK + study.EXCLUSION_MS)]
        calendar = study.Calendar(events)
        controls = calendar.controls(entry)
        expected = {entry + sign * m * WEEK for m in (1, 2, 3, 4) for sign in (-1, 1)}
        self.assertEqual(set(controls), expected - {entry - WEEK})
        self.assertIn(entry + WEEK, controls)
        self.assertIs(calendar.controls(entry), controls)

    def test_less_than_two_valid_controls_discards_the_treatment(self):
        release = BASE + 60 * 24 * HOUR
        entry = release - HOUR
        calendar = study.Calendar([study.Event("CPI", release)])
        observations = {entry: 1., entry - WEEK: 0.}
        cache = lookup_cache(observations)
        self.assertEqual(study.event_pairs(release, {"A": cache}, calendar), [])
        observations[entry + WEEK] = .5
        cache = lookup_cache(observations)
        pair, = study.event_pairs(release, {"A": cache}, calendar)
        self.assertEqual(pair.control_count, 2)
        self.assertEqual(pair.control, .25)
        self.assertEqual(pair.difference, .75)
        blocked = study.Calendar([study.Event("CPI", release), study.Event("NFP", entry + WEEK)])
        self.assertEqual(study.event_pairs(release, {"A": cache}, blocked), [])

    def test_a_gap_in_one_control_can_leave_fewer_than_two(self):
        rows = candles(24 * 60)
        entry = BASE + 30 * 24 * HOUR
        calendar = study.Calendar([study.Event("CPI", entry + HOUR)])
        # Keep only two available weekly control windows and the treated window.
        starts = (entry, entry - WEEK, entry + WEEK)
        rows = [row for row in rows if any(t - 15 * HOUR <= row[0] < t + 12 * HOUR for t in starts)]
        cache = study.precompute(rows)
        pair = study.match_pair("A", cache, cache.hits[study.MAIN], entry, calendar.controls(entry))
        self.assertIsNotNone(pair)
        rows = [row for row in rows if row[0] != entry + WEEK + 5 * HOUR]
        cache = study.precompute(rows)
        self.assertIsNone(study.match_pair("A", cache, cache.hits[study.MAIN], entry, calendar.controls(entry)))

    def test_event_equal_weighting_sample_se_and_ci(self):
        groups = [[study.Pair("A", 0, 1., 0., 2), study.Pair("B", 0, 0., 0., 2)],
                  [study.Pair("A", 1, 0., 1., 2)], []]
        result = study.summarize_events(groups)
        self.assertEqual(result["D"], -.25)
        self.assertAlmostEqual(result["SE"], .75)
        np.testing.assert_allclose(result["CI95"], [-1.72, 1.22])
        self.assertEqual(result["N_e"], 2)
        self.assertEqual(result["matched_pairs"], 3)
        self.assertEqual(result["excluded_events"], 1)
        self.assertIsNone(study.cluster_statistics([])["D"])
        self.assertEqual(study.cluster_statistics([.5])["CI95"], [None, None])

    def test_descriptive_groups_lead_boundaries_ranges_and_symbols(self):
        release = BASE + 60 * 24 * HOUR
        event = study.Event("FOMC", release)
        calendar = study.Calendar([event])
        observations = {}
        for t in study.treatment_times(release):
            observations.update({control: .25 for control in calendar.controls(t)})
            observations[t] = .75
        a = lookup_cache(observations, ranges={t: 4 * v for t, v in observations.items()})
        b = lookup_cache({t: 0. for t in observations}, ranges={t: 1. for t in observations})
        result = study.analyze([event], {"A": a, "B": b}, runs=0)
        self.assertEqual(result["main"]["D"], .25)
        self.assertEqual(result["by_event_type"]["FOMC"]["D"], .25)
        self.assertEqual(result["by_event_type"]["CPI"]["N_e"], 0)
        self.assertEqual(result["by_year"]["2024"]["D"], .25)
        self.assertEqual(result["by_symbol"]["A"]["D"], .5)
        self.assertEqual(result["by_symbol"]["B"]["D"], 0.)
        self.assertEqual(len(result["sensitivity_grid"]), 9)
        self.assertTrue(all(row["matched_pairs"] == 2 for row in result["by_lead_hours"]))
        self.assertTrue(all(row["treatment_mean"] == .375 for row in result["by_lead_hours"]))
        self.assertEqual(result["range_atr"]["treatment_mean"], 2.)
        self.assertEqual(result["range_atr"]["control_mean"], 1.)
        self.assertEqual(result["range_atr"]["ratio"], 2.)

    def test_profile_uses_containing_candle_and_same_relative_control_positions(self):
        anchor = BASE + 60 * 24 * HOUR
        event = study.Event("CPI", anchor + HOUR // 2)
        # +1 week's anchor is exactly 48h from NFP and is eligible. Its +1h
        # position is closer than 48h but remains in the matched control profile.
        blocker = study.Event("NFP", anchor + WEEK + study.EXCLUSION_MS)
        calendar = study.Calendar([event, blocker])
        observations = {}
        for offset in range(-6, 7):
            observations[anchor + offset * HOUR] = .02 + offset * .001
            for control in calendar.controls(anchor):
                observations[control + offset * HOUR] = .01
        profile = study.return_profile([event], {"A": lookup_cache(observations)}, calendar)
        self.assertEqual([row["offset_hours"] for row in profile], list(range(-6, 7)))
        self.assertEqual(profile[6]["treatment_mean"], .02)
        self.assertEqual(profile[7]["treatment_mean"], .021)
        self.assertEqual(profile[7]["control_mean"], .01)
        self.assertEqual(profile[7]["control_observations"], 8)

    def test_abs_log_return_and_realized_range_formulas(self):
        rows = candles()
        rows[20][2:5] = [104., 98., 102.]
        cache = study.precompute(rows)
        self.assertAlmostEqual(cache.abs_returns[20], abs(math.log(102 / 100)))
        self.assertAlmostEqual(cache.abs_returns[21], abs(math.log(100 / 102)))
        self.assertEqual(cache.ranges[12][20], (104 - 98) / 2)

    def test_event_bar_shocks_flow_through_full_study(self):
        events = [study.Event(kind, BASE + day * 24 * HOUR + 12 * HOUR + half_hour)
                  for kind, day, half_hour in (("CPI", 60, 0), ("NFP", 100, HOUR // 2), ("FOMC", 140, 0))]
        rows = candles(24 * 200)
        for event in events:
            index = (event.release_ms - BASE) // HOUR
            rows[index][2:5] = [104., 96., 103.]
        result = study.analyze(events, {"A": study.precompute(rows)}, runs=20)
        self.assertEqual(result["main"]["D"], 1.)
        self.assertEqual(result["main"]["N_e"], 3)
        self.assertEqual(result["main"]["CI95"], [1., 1.])
        self.assertTrue(result["verdict"]["mechanism_established"])
        self.assertEqual(result["placebo"]["percentile_lt"], 1.)
        self.assertEqual(result["range_atr"]["treatment_mean"], 4.)
        self.assertEqual(result["range_atr"]["control_mean"], 1.)
        self.assertAlmostEqual(result["absolute_return_profile"][6]["treatment_mean"], math.log(1.03))
        grid = {(row["k"], row["H"]): row for row in result["sensitivity_grid"]}
        self.assertEqual(grid[2.5, 12]["D"], 0.)
        self.assertAlmostEqual(grid[1.5, 6]["D"], 8 / 9)


class PlaceboTests(unittest.TestCase):
    def test_seed_reproducibility_exclusions_and_event_cache(self):
        events = [study.Event("CPI", BASE + (60 + i * 14) * 24 * HOUR) for i in range(6)]
        rows = candles(24 * 220)
        for i, row in enumerate(rows):
            row[2:4] = [101 + i % 7, 99 - i % 5]
        caches = {"A": study.precompute(rows)}
        calendar = study.Calendar(events)
        rng = random.Random(study.SEED)
        draws = study.draw_placebo_times(events, calendar, rng)
        self.assertEqual(draws, study.draw_placebo_times(events, calendar, random.Random(study.SEED)))
        for event, shifted in zip(events, draws):
            self.assertIsNotNone(shifted)
            self.assertIn(abs(shifted - event.release_ms), [m * WEEK for m in (1, 2, 3, 4)])
            self.assertTrue(all(abs(shifted - real.release_ms) >= study.EXCLUSION_MS for real in events))
        with patch.object(study, "event_pairs", wraps=study.event_pairs) as pairs:
            first = study.placebo(events, caches, calendar, .2, runs=30)
        second = study.placebo(events, caches, calendar, .2, runs=30)
        self.assertEqual(first, second)
        self.assertEqual(first["valid_runs"], 30)
        self.assertLessEqual(pairs.call_count, 8 * len(events))
        self.assertEqual(pairs.call_count, first["cached_shifted_events"])

    def test_twenty_failed_draws_drop_only_that_fake_event(self):
        event = study.Event("CPI", BASE + 50 * 24 * HOUR)
        calendar = study.Calendar([event, study.Event("FOMC", event.release_ms + WEEK)])
        rng = Mock()
        rng.choice.return_value = 1
        self.assertEqual(study.draw_placebo_times([event], calendar, rng), [None])
        self.assertEqual(rng.choice.call_count, 40)

    def test_placebo_reports_strict_and_inclusive_percentiles(self):
        events = [study.Event("CPI", BASE + 60 * 24 * HOUR)]
        with patch.object(study, "event_pairs", return_value=[study.Pair("A", BASE, 0., 0., 2)]):
            result = study.placebo(events, {}, study.Calendar(events), 0., runs=10)
        self.assertEqual(result["percentile_lt"], 0.)
        self.assertEqual(result["percentile_le"], 1.)
        empty = study.placebo(events, {}, study.Calendar(events), .1, runs=3)
        self.assertEqual(empty["D"], [None] * 3)
        self.assertEqual(empty["valid_runs"], 0)
        self.assertIsNone(empty["percentile_lt"])

    def test_decision_uses_all_three_frozen_gates_and_strict_percentile(self):
        main = dict(D=.1, CI95=[.001, .2])
        null = dict(percentile_lt=.90, percentile_le=1.)
        self.assertTrue(study.decision(main, null)["mechanism_established"])
        for changed_main, changed_null in ((dict(D=0., CI95=[.001, .2]), null),
                                           (dict(D=.1, CI95=[0., .2]), null),
                                           (main, dict(percentile_lt=.899, percentile_le=1.)),
                                           (main, dict(percentile_lt=None)),
                                           (dict(D=.1, CI95=[None, None]), null)):
            with self.subTest(main=changed_main, null=changed_null):
                self.assertFalse(study.decision(changed_main, changed_null)["mechanism_established"])


class LiveCountTests(unittest.TestCase):
    def test_read_only_connection_allowed_columns_and_union_boundaries(self):
        release = BASE + 60 * 24 * HOUR
        events = [study.Event("CPI", release), study.Event("FOMC", release + 2 * HOUR)]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "counts ?# 中文.db"
            original_connect = sqlite3.connect
            with original_connect(path) as writer:
                writer.execute("CREATE TABLE trader_positions (trader_id TEXT, symbol TEXT, status TEXT, "
                               "entry_time INTEGER, exchange_type TEXT, realized_pnl REAL, fee REAL, exit_price REAL)")
                rows = [("live", "BTCUSDT", "CLOSED", entry, "binance", 999., 999., 999.)
                        for entry in (release - 6 * HOUR, release - 6 * HOUR - 1,
                                      release - 1, release, release + 2 * HOUR)]
                rows += [("live", "ETHUSDT", "OPEN", release - 1, "binance", 0, 0, 0),
                         ("live", "BTCUSDT_PERP", "CLOSED", release - 1, "binance", 0, 0, 0),
                         ("live", "AAPL", "CLOSED", release - 1, "alpaca", 0, 0, 0),
                         ("paper", "ETHUSDT", "CLOSED", release - 1, "paper", 0, 0, 0),
                         ("paper", "ETHUSDT", "CLOSED", None, "paper", 0, 0, 0)]
                writer.executemany("INSERT INTO trader_positions VALUES (?, ?, ?, ?, ?, ?, ?, ?)", rows)
            digest_before = study.sha256_file(path)
            queries = []
            accessed_columns = set()
            connections = []

            def authorize(action, table, column, database, trigger):
                if action == sqlite3.SQLITE_READ and table == "trader_positions":
                    accessed_columns.add(column)
                    if column not in {"trader_id", "symbol", "status", "entry_time"}:
                        return sqlite3.SQLITE_DENY
                return sqlite3.SQLITE_OK

            def connect(database_url, **kwargs):
                self.assertTrue(database_url.startswith("file:"))
                self.assertTrue(database_url.endswith("?mode=ro"))
                self.assertEqual(kwargs, {"uri": True})
                connection = original_connect(database_url, **kwargs)
                with self.assertRaises(sqlite3.OperationalError):
                    connection.execute("CREATE TABLE forbidden_write (n INTEGER)")
                connection.set_authorizer(authorize)
                connection.set_trace_callback(queries.append)
                connections.append(connection)
                return connection

            with patch.object(study.sqlite3, "connect", side_effect=connect) as mocked:
                result = study.live_counts(path, events)
            mocked.assert_called_once()
            self.assertEqual(result["traders"]["live"], dict(
                total_closed=5, in_event_window=3, invalid_entry_time=0, fraction=.6))
            self.assertEqual(result["traders"]["paper"], dict(
                total_closed=2, in_event_window=1, invalid_entry_time=1, fraction=.5))
            self.assertEqual(accessed_columns, {"trader_id", "symbol", "status", "entry_time"})
            self.assertIn("PRAGMA query_only = ON", queries)
            sql = " ".join(queries).lower()
            for forbidden in ("realized_pnl", "fee", "exit_price", "select *"):
                self.assertNotIn(forbidden, sql)
                self.assertNotIn(forbidden, json.dumps(result))
            self.assertEqual(study.sha256_file(path), digest_before)
            self.assertEqual(result["file_sha256"], digest_before)
            self.assertEqual(len(result["selected_rows_sha256"]), 64)
            with self.assertRaises(sqlite3.ProgrammingError):
                connections[0].execute("SELECT 1")

    def test_read_only_query_sees_committed_wal_rows(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "wal.db"
            writer = sqlite3.connect(path)
            try:
                writer.execute("PRAGMA journal_mode = WAL")
                writer.execute("PRAGMA wal_autocheckpoint = 0")
                writer.execute("CREATE TABLE trader_positions (trader_id TEXT, symbol TEXT, status TEXT, entry_time INTEGER)")
                writer.execute("INSERT INTO trader_positions VALUES ('paper', 'BTCUSDT', 'CLOSED', ?)", (BASE,))
                writer.commit()
                result = study.live_counts(path, [study.Event("CPI", BASE + HOUR)])
                self.assertEqual(result["traders"]["paper"]["in_event_window"], 1)
            finally:
                writer.close()


class CommandLineTests(unittest.TestCase):
    def test_synthetic_cli_emits_complete_numeric_reports_and_skips_database(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "inputs"
            out = Path(directory) / "output"
            (root / "macro_events").mkdir(parents=True)
            (root / "volrix" / "klines").mkdir(parents=True)
            event_content = (HEADER + "CPI,2024-01-30,08:30,2023-12,BLS,\n" +
                             "NFP,2024-02-09,08:30,2024-01,BLS,\n")
            event_path = root / "macro_events" / "macro_events.csv"
            event_path.write_text(event_content, encoding="utf-8")
            rows = candles(24 * 90)
            del rows[100]
            price_path = root / "volrix" / "klines" / "BTCUSDT_1h.json"
            price_content = json.dumps(rows).encode("utf-8")
            price_path.write_bytes(price_content)
            with patch.object(study.sqlite3, "connect", side_effect=AssertionError("unexpected DB read")), patch("builtins.print"):
                code = study.main(["--data-root", str(root), "--out", str(out),
                                   "--symbols", "BTCUSDT", "--placebo", "5", "--no-live"])
            self.assertEqual(code, 0)
            report_text = (out / "report.json").read_text(encoding="utf-8")
            result = json.loads(report_text)
            self.assertNotIn("NaN", report_text)
            self.assertNotIn("Infinity", report_text)
            self.assertEqual(result["input_sha256"][str(price_path.resolve())], hashlib.sha256(price_content).hexdigest())
            self.assertEqual(result["input_sha256"][str(event_path.resolve())], hashlib.sha256(event_content.encode()).hexdigest())
            self.assertEqual(result["symbols"]["BTCUSDT"]["missing_hours"], 1)
            self.assertEqual(result["events"]["total"], 2)
            self.assertEqual(result["main"]["N_e"], 2)
            self.assertEqual(result["main"]["D"], 0.)
            self.assertEqual(result["main"]["CI95"], [0., 0.])
            self.assertEqual(result["placebo"]["requested_runs"], 5)
            self.assertTrue(result["diagnostic_run"])
            self.assertTrue(result["live_counts"]["skipped"])
            self.assertEqual(len(result["sensitivity_grid"]), 9)
            self.assertEqual(len(result["absolute_return_profile"]), 13)
            self.assertEqual(len(result["by_lead_hours"]), 6)
            summary = (out / "summary.md").read_text(encoding="utf-8")
            for heading in ("主统计与事件类型", "敏感性网格", "按年份", "按币种", "提前时长触发率",
                            "观察窗区间/ATR", "事件 K 线前后绝对对数收益", "已平仓 USDT 交易笔数", "诊断运行"):
                self.assertIn(heading, summary)

    def test_invalid_event_csv_exits_before_outputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "inputs"
            (root / "macro_events").mkdir(parents=True)
            (root / "macro_events" / "macro_events.csv").write_text("invalid\n")
            out = Path(directory) / "output"
            with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
                study.main(["--data-root", str(root), "--out", str(out), "--no-live"])
            self.assertEqual(caught.exception.code, 1)
            self.assertFalse(out.exists())

    def test_cli_rejects_writing_into_inputs_and_invalid_arguments(self):
        cases = [["--out", "/tmp/inputs/output"],
                 ["--out", "/tmp/output", "--placebo", "-1"],
                 ["--out", "/tmp/output", "--symbols", "BTCUSDT", "BTCUSDT"],
                 ["--out", "/tmp/output", "--symbols", "../BTCUSDT"]]
        for extra in cases:
            with self.subTest(extra=extra), redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
                study.main(["--data-root", "/tmp/inputs", *extra])
            self.assertEqual(caught.exception.code, 2)


if __name__ == "__main__":
    unittest.main()
