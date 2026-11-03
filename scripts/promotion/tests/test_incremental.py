"""Pre-registration, ordered online decisions, holdback and append-only ledger."""

from contextlib import redirect_stderr, redirect_stdout
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from datetime import timedelta
import hashlib
import io
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from scripts.backtest import evolution_ledger
from scripts.promotion import create_pair as create
from scripts.promotion import report
from scripts.promotion.tests.test_report import START, SCHEMA, pair


OPTIONS = create.PreregOptions("crypto_llm", "config-v1", "新字段", "A")


def registered(**kwargs):
    return create.make_prereg(replace(OPTIONS, **kwargs), report.iso(START))


class DecisionTests(unittest.TestCase):
    def decide(self, *, days=30, delta=0.02, se=0.01, cn=100, bn=100, cr=100, br=100, **prereg):
        return report.decide_incremental(days, registered(**prereg), {"n": cn, "r_n": cr},
                                         {"n": bn, "r_n": br}, {"mean_diff": delta, "cluster_se": se})[0]

    def test_all_ordered_paths_and_boundaries(self):
        cases = [
            ("insufficient", {"days": 13.999, "delta": -1}),
            ("harm_stop", {"days": 14, "delta": -0.03, "se": 0.01, "cr": 2, "br": 2}),
            ("insufficient", {"days": 14, "delta": -0.02, "se": 0.01}),
            ("insufficient", {"days": 29.999}),
            ("insufficient", {"cr": 49}), ("insufficient", {"br": 49}),
            ("promote", {}), ("promote", {"cr": 50, "br": 50}),
            ("promote", {"cn": 67}), ("promote", {"cn": 150}),
            ("fewer_trades", {"cn": 66}),
            ("fewer_trades", {"cn": 66, "se": 0.1}),
            ("reject", {"cn": 151}),
            ("extend", {"delta": 0, "se": 0.021}),
            ("extend", {"days": 43.999, "delta": 0, "se": 0.021}),
            ("reject", {"days": 44, "delta": 0, "se": 0.021}),
            ("reject", {"delta": 0, "se": 0.02}),
            ("reject", {"delta": -0.01}),
            ("reject", {"se": None}), ("reject", {"delta": None, "se": None}),
            ("promote", {"se": 0}), ("reject", {"delta": 0, "se": 0}),
        ]
        for expected, kwargs in cases:
            with self.subTest(expected=expected, **kwargs):
                self.assertEqual(self.decide(**kwargs), expected)

    def test_lower_bound_is_strict_and_uses_preregistered_margin(self):
        margin = 1.645 * 0.02 - 0.001
        self.assertEqual(self.decide(delta=0.001, se=0.02, margin_r=margin), "reject")
        self.assertEqual(self.decide(delta=0.001, se=0.02, margin_r=margin + 1e-6), "promote")

    def test_custom_windows_and_no_control_trades(self):
        self.assertEqual(self.decide(days=10, harm_check_days=7, delta=-0.1), "harm_stop")
        self.assertEqual(self.decide(days=20, min_days=20, min_trades=10, cr=10, br=10), "promote")
        self.assertEqual(self.decide(days=35, delta=0, se=0.03, extend_days=5), "reject")
        self.assertEqual(self.decide(bn=0, br=0), "insufficient")

    def test_canonical_hash_and_invalid_prereg(self):
        prereg = registered()
        body = {key: value for key, value in prereg.items() if key != "prereg_hash"}
        canonical = json.dumps(body, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False)
        self.assertEqual(prereg["prereg_hash"], hashlib.sha256(canonical.encode()).hexdigest())
        self.assertTrue(create.verify_prereg(dict(reversed(list(prereg.items())))))
        for field, value in (("margin_r", 0.5), ("min_days", 3), ("retroactive", True), ("note", "改写")):
            with self.subTest(field=field):
                self.assertFalse(create.verify_prereg({**prereg, field: value}))
        for kwargs in ({"track": "bad"}, {"change_type": "F"}, {"baseline_version": ""},
                       {"idea_id": " "}, {"margin_r": -1}, {"extend_se": float("nan")},
                       {"min_days": 6}, {"min_trades": 1.5}, {"harm_check_days": -1}, {"extend_days": -1}):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                registered(**kwargs)
        malformed = {**prereg, "registered_at": 123}
        malformed["prereg_hash"] = create.prereg_hash(malformed)
        self.assertFalse(create.verify_prereg(malformed))


class IncrementalIOTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(dir=Path(__file__).parent, prefix=".test-incremental-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.db = self.directory / "paper #1?.db"
        self.path = self.directory / "pairs.json"
        with sqlite3.connect(self.db) as connection:
            connection.executescript(SCHEMA)
        env = patch.dict(os.environ, {"PROMOTION_DIR": str(self.directory), "NOFX_DB": str(self.db),
                                     "FACTOR_MINING_DIR": str(self.directory / "no-factors")})
        env.start()
        self.addCleanup(env.stop)

    def save(self, configured=None):
        configured = configured if configured is not None else pair(prereg=registered())
        document = {"custom": "保留", "pairs": [configured]}
        self.path.write_bytes(create.document_bytes(document))
        return configured

    def trades(self, role, *, n=50, r=0.1, start=START, days=29, invalid=False):
        with sqlite3.connect(self.db) as connection:
            for i in range(n):
                # Spread exits across weeks; equal constant R gives an exact zero SE.
                exit_at = start + timedelta(days=days * i / max(1, n - 1))
                connection.execute(
                    "INSERT INTO trader_positions (trader_id,symbol,status,entry_time,exit_time,realized_pnl,fee,"
                    "initial_stop_loss,entry_price,quantity) VALUES (?,?,?,?,?,?,?,?,?,?)",
                    (role, "BTCUSDT", "CLOSED", int(START.timestamp() * 1000), int(exit_at.timestamp() * 1000),
                     r, 0, 0 if invalid else 99, 100, 1))

    def item(self, configured=None, days=30):
        configured = configured if configured is not None else pair(prereg=registered())
        return report.build_report([configured], self.db, START + timedelta(days=days))["pairs"][0]

    def test_retro_registration_once_cli_warning_and_sealed_note(self):
        original = self.save(pair())
        arguments = ["prereg", original["id"], "--mode", "incremental", "--track", "crypto_llm",
                     "--baseline-version", "old-config", "--idea-id", "ensemble", "--change-type", "A"]
        with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(create.main(arguments), 0)
        self.assertIn(create.RETRO_WARNING, errors.getvalue())
        saved = json.loads(self.path.read_text())
        p = saved["pairs"][0]["prereg"]
        self.assertEqual(p["note"], create.RETRO_WARNING)
        self.assertTrue(p["retroactive"])
        self.assertTrue(create.verify_prereg(p))
        self.assertEqual({key: value for key, value in saved["pairs"][0].items() if key != "prereg"}, original)
        before = self.path.read_bytes()
        with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(create.main(arguments), 1)
        self.assertIn("已有 prereg", errors.getvalue())
        self.assertEqual(before, self.path.read_bytes())
        p["note"] = "删除后见偏差说明"
        self.assertFalse(create.verify_prereg(p))

    def test_existing_null_prereg_and_missing_pair_refused(self):
        self.save(pair(prereg=None))
        for pair_id in ("ensemble", "missing"):
            with self.subTest(pair_id=pair_id), self.assertRaises(ValueError):
                create.register_pair(self.directory, pair_id, OPTIONS)

    def test_tampered_verdict_only_without_database_or_metrics(self):
        for bad in ({**registered(), "margin_r": 9}, None, {}):
            with self.subTest(bad=bad):
                configured = pair(started_at=None, prereg=bad)
                result = report.build_report([configured], self.directory / "missing.db", START)
                self.assertEqual(result["pairs"], [{"id": "ensemble", "verdict": "prereg_tampered"}])
                self.assertIn("prereg_tampered", report.summary(result))
                report.record_ledger(result, self.directory / "absent-ledger.jsonl")
                self.assertFalse((self.directory / "absent-ledger.jsonl").exists())

    def test_integrated_promote_fields_legacy_information_and_db_readonly(self):
        self.trades("candidate", r=0.2)
        self.trades("control", r=0.1)
        before = self.db.read_bytes()
        configured = pair(prereg=registered())
        item = self.item(configured)
        legacy = self.item(pair())
        self.assertEqual(item["verdict"], "promote")
        self.assertEqual(item["legacy_verdict"], legacy["verdict"])
        self.assertEqual(item["legacy_reasons"], legacy["reasons"])
        for field in ("candidate", "control", "paired_daily", "weekly_avg_r", "warnings", "audit"):
            self.assertEqual(item[field], legacy[field])
        self.assertAlmostEqual(item["delta"], 0.1)
        self.assertAlmostEqual(item["lower_bound"], item["delta"] - 1.645 * item["se"])
        self.assertEqual(item["trade_ratio"], 1)
        self.assertFalse(item["retroactive"])
        self.assertEqual(before, self.db.read_bytes())
        self.assertNotIn("legacy_verdict", legacy)
        text = report.summary({"generated_at": "test", "pairs": [item], "caveats": "test"})
        self.assertIn("Δ=", text)
        self.assertIn("prereg_hash=", text)

    def test_harm_stop_before_min_days_and_before_min_trades(self):
        self.trades("candidate", n=3, r=-0.2, days=13)
        self.trades("control", n=3, r=0.1, days=13)
        self.assertEqual(self.item(days=13.99)["verdict"], "insufficient")
        item = self.item(days=14)
        self.assertEqual(item["verdict"], "harm_stop")
        self.assertEqual(item["incremental_weekly_avg_r"]["candidate_r_n"], 3)
        self.assertIsNone(item["weekly_avg_r"]["mean_diff"])

    def test_closed_counts_for_ratio_but_valid_r_counts_for_minimum(self):
        self.trades("candidate", n=50, r=0.2)
        self.trades("control", n=100, r=0.1)
        self.assertEqual(self.item()["verdict"], "fewer_trades")
        self.trades("candidate", n=20, r=0.2, invalid=True)
        item = self.item()
        self.assertEqual(item["verdict"], "promote")
        self.assertEqual(item["trade_ratio"], 0.7)
        self.assertEqual(item["candidate"]["r_n"], 50)
        strict = pair(prereg=registered(min_trades=51))
        self.assertEqual(self.item(strict)["verdict"], "insufficient")

    def test_missing_se_rejected_at_min_days(self):
        self.trades("candidate", r=0.2, days=1)
        self.trades("control", r=0.1, days=1)
        self.assertEqual(self.item(days=29)["verdict"], "insufficient")
        self.assertEqual(self.item()["verdict"], "reject")

    def test_mixed_legacy_and_tampered_pair_order(self):
        configured = [pair(id="bad", prereg={}), pair(id="legacy")]
        result = report.build_report(configured, self.db, START + timedelta(days=30))
        self.assertEqual([p["id"] for p in result["pairs"]], ["bad", "legacy"])
        self.assertEqual(result["pairs"][1]["verdict"], "insufficient")
        self.assertNotIn("delta", result["pairs"][1])

    def test_holdback_only_uses_exits_strictly_after_promotion(self):
        promoted = START + timedelta(days=30)
        configured = pair(prereg=registered(), promoted_at=report.iso(promoted),
                          holdback_until=report.iso(promoted + timedelta(days=28)))
        self.trades("candidate", r=10)
        self.trades("control", r=-10)
        self.trades("candidate", n=1, r=1000, start=promoted, days=0)  # Exact boundary excluded.
        self.trades("control", n=1, r=-1000, start=promoted, days=0)
        self.trades("candidate", n=3, r=-0.2, start=promoted + timedelta(milliseconds=1), days=20)
        self.trades("control", n=3, r=0.1, start=promoted + timedelta(milliseconds=1), days=20)
        item = self.item(configured, days=51)
        self.assertEqual(item["verdict"], "rollback_suggested")
        self.assertAlmostEqual(item["delta"], -0.3)
        self.assertEqual(item["incremental_weekly_avg_r"]["candidate_r_n"], 3)
        self.assertGreater(item["weekly_avg_r"]["mean_diff"], 0)
        self.assertEqual(item["evaluation_started_at"], report.iso(promoted))
        self.assertEqual(self.item(configured, days=30)["verdict"], "holdback_ok")  # No post-promotion data.
        self.assertIsNone(self.item(configured, days=30)["delta"])
        self.assertEqual(self.item(configured, days=57.999)["verdict"], "rollback_suggested")
        self.assertEqual(self.item(configured, days=58)["verdict"], "holdback_done")
        with sqlite3.connect(self.db) as connection:
            connection.execute("UPDATE trader_positions SET realized_pnl=0.2 WHERE trader_id='candidate'")
        self.assertEqual(self.item(configured, days=51)["verdict"], "holdback_ok")

    def test_promote_recomputes_ignores_stale_report_and_writes_only_pairs(self):
        self.save()
        self.trades("candidate", r=0.2)
        self.trades("control", r=0.1)
        stale = self.directory / "report.json"
        stale.write_text('{"pairs":[{"id":"ensemble","verdict":"reject"}]}')
        before = {p.name: p.read_bytes() for p in self.directory.iterdir() if p.is_file() and p.name != ".pairs.lock"}
        now = START + timedelta(days=30)
        saved = create.promote_pair(self.directory, self.db, "ensemble", now=now)
        self.assertEqual(saved["promoted_at"], report.iso(now))
        self.assertEqual(saved["holdback_until"], report.iso(now + timedelta(days=28)))
        self.assertEqual(set(before), {p.name for p in self.directory.iterdir() if p.name != ".pairs.lock"})
        for name, content in before.items():
            if name != "pairs.json":
                self.assertEqual((self.directory / name).read_bytes(), content)
        with self.assertRaises(ValueError):
            create.promote_pair(self.directory, self.db, "ensemble", now=now + timedelta(days=1))

    def test_promote_refused_unless_fresh_verdict_promote(self):
        for configured in (pair(), pair(prereg=registered()), pair(prereg={})):
            with self.subTest(configured=configured):
                self.save(configured)
                before = self.path.read_bytes()
                (self.directory / "report.json").write_text('{"pairs":[{"id":"ensemble","verdict":"promote"}]}')
                with self.assertRaises(ValueError):
                    create.promote_pair(self.directory, self.db, "ensemble", now=START + timedelta(days=30))
                self.assertEqual(before, self.path.read_bytes())
        with self.assertRaises(ValueError):
            create.promote_pair(self.directory, self.db, "absent")
        with self.assertRaises(ValueError):
            create.promote_pair(self.directory, self.db, "ensemble", holdback_days=-1)

    def test_promote_cli_reminder_and_custom_holdback(self):
        self.save()
        self.trades("candidate", r=0.2)
        self.trades("control", r=0.1)
        original = report.build_report
        fixed_report = lambda pairs, db, as_of: original(pairs, db, START + timedelta(days=30))
        with patch.object(report, "build_report", side_effect=fixed_report), redirect_stdout(io.StringIO()) as output:
            self.assertEqual(create.main(["promote", "ensemble", "--holdback-days", "21"]), 0)
        self.assertIn("手动修改并经用户批准", output.getvalue())
        saved = json.loads(self.path.read_text())["pairs"][0]
        self.assertEqual(report.as_utc(saved["holdback_until"]) - report.as_utc(saved["promoted_at"]), timedelta(days=21))

    def test_ledger_deduplication_fields_stages_and_nonfinal_skip(self):
        ledger = self.directory / "ledger.jsonl"
        item = {**pair(prereg=registered()), "verdict": "promote", "delta": 0.1, "se": 0.02,
                "trade_ratio": 1, "retroactive": True}
        document = {"pairs": [item]}
        report.record_ledger(document, ledger)
        original = ledger.read_bytes()
        report.record_ledger(document, ledger)
        self.assertEqual(ledger.read_bytes(), original)
        for verdict in ("harm_stop", "fewer_trades", "reject", "rollback_suggested", "extend", "insufficient",
                        "holdback_ok", "holdback_done", "prereg_tampered"):
            report.record_ledger({"pairs": [{**item, "verdict": verdict}]}, ledger)
        rows = evolution_ledger.read_rows(ledger)
        self.assertEqual(len(rows), 5)
        self.assertEqual([r["stage"] for r in rows], ["S2"] * 4 + ["S3"])
        self.assertEqual(rows[0]["z"], 5)
        for key in ("idea_id", "change_type", "baseline_version", "prereg_hash", "track"):
            self.assertEqual(rows[0][key], item["prereg"][key])
        self.assertEqual(rows[0]["pair_id"], item["id"])
        self.assertTrue(rows[0]["retroactive"])
        self.assertIsNone(rows[0]["K"])
        self.assertFalse(rows[0]["counts_toward_k"])

    def test_concurrent_ledger_writers_deduplicate_inside_append_lock(self):
        ledger = self.directory / "ledger.jsonl"
        item = {**pair(prereg=registered()), "verdict": "promote", "delta": 0.1, "se": 0.02,
                "trade_ratio": 1, "retroactive": False}
        with ThreadPoolExecutor(max_workers=4) as workers:
            futures = [workers.submit(report.record_ledger, {"pairs": [item]}, ledger) for _ in range(8)]
            for future in futures:
                future.result(timeout=5)
        self.assertEqual(len(evolution_ledger.read_rows(ledger)), 1)

    def test_ledger_cli_default_off_optional_path_and_main_root_default(self):
        self.save()
        self.trades("candidate", r=0.2)
        self.trades("control", r=0.1)
        default = self.directory / "data/evolution/ledger.jsonl"
        original = report.build_report
        fixed_report = lambda pairs, db, as_of: original(pairs, db, START + timedelta(days=30))
        with (patch.object(report, "main_repo_root", return_value=self.directory),
              patch.object(report, "build_report", side_effect=fixed_report), redirect_stdout(io.StringIO())):
            self.assertEqual(report.main([]), 0)
            self.assertFalse(default.exists())
            self.assertEqual(report.main(["--ledger"]), 0)
            self.assertEqual(report.main(["--ledger"]), 0)
            self.assertEqual(len(evolution_ledger.read_rows(default)), 1)
            explicit = self.directory / "explicit.jsonl"
            self.assertEqual(report.main(["--ledger", str(explicit)]), 0)
            self.assertEqual(len(evolution_ledger.read_rows(explicit)), 1)


if __name__ == "__main__":
    unittest.main()
