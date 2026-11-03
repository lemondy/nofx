"""Baseline registry CLI with temporary databases and atomic JSON publication."""

from contextlib import closing, redirect_stderr, redirect_stdout
import io
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from scripts.backtest.evolution_ledger import TRACKS
from scripts.promotion import baseline
from scripts.promotion.create_pair import open_database


SCHEMA = """
CREATE TABLE strategy_config_versions (
    id INTEGER PRIMARY KEY, strategy_id TEXT, config_hash TEXT, risk_hash TEXT,
    source TEXT, summary TEXT, config TEXT, changed_at INTEGER, created_at INTEGER
);
INSERT INTO strategy_config_versions VALUES (1,'strategy','old-hash','risk','','','{}',100,500);
INSERT INTO strategy_config_versions VALUES (2,'strategy','latest-hash','risk','','','{}',200,300);
INSERT INTO strategy_config_versions VALUES (3,'strategy','stale-hash','risk','','','{}',150,900);
INSERT INTO strategy_config_versions VALUES (4,'strategy-2','other-hash','risk','','','{}',300,1000);
"""


class BaselineTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(dir=Path(__file__).parent, prefix=".test-baseline-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.db = self.directory / "versions #1?.db"
        self.path = self.directory / "data/evolution/baselines.json"
        with sqlite3.connect(self.db) as connection:
            connection.executescript(SCHEMA)
        root = patch.object(baseline, "main_repo_root", return_value=self.directory)
        root.start()
        self.addCleanup(root.stop)
        env = patch.dict(os.environ, {"NOFX_DB": str(self.db)})
        env.start()
        self.addCleanup(env.stop)

    def run_cli(self, arguments):
        output, errors = io.StringIO(), io.StringIO()
        with redirect_stdout(output), redirect_stderr(errors):
            code = baseline.main(arguments)
        return code, output.getvalue(), errors.getvalue()

    def set_args(self, strategy="strategy", track="crypto_llm"):
        return ["set", "--track", track, "--strategy-id", strategy, "--note", "人工批准的基线"]

    def test_show_every_track_without_creating_registry_or_database(self):
        self.db.unlink()
        code, output, _ = self.run_cli(["show"])
        self.assertEqual(code, 0)
        self.assertEqual(output.splitlines(), [f"{track}: null" for track in TRACKS])
        self.assertFalse(self.path.parent.exists())
        self.assertFalse(self.db.exists())

    def test_set_dry_run_then_yes_reads_latest_hash_and_leaves_db_unchanged(self):
        original = self.db.read_bytes()
        code, output, _ = self.run_cli(self.set_args())
        self.assertEqual(code, 0)
        self.assertIn("DRY-RUN", output)
        self.assertIn("latest-hash", output)
        self.assertFalse(self.path.parent.exists())
        code, output, _ = self.run_cli([*self.set_args(), "--yes"])
        self.assertEqual(code, 0)
        self.assertIn("已写入", output)
        registry = json.loads(self.path.read_text())
        self.assertEqual(registry["crypto_llm"]["current"]["config_hash"], "latest-hash")
        self.assertEqual(registry["crypto_llm"]["current"]["strategy_id"], "strategy")
        self.assertEqual(registry["crypto_llm"]["current"]["note"], "人工批准的基线")
        self.assertIsNone(registry["crypto_llm"]["previous"])
        self.assertEqual(self.db.read_bytes(), original)
        self.assertEqual([p.name for p in self.path.parent.iterdir() if p.name != ".pairs.lock"], ["baselines.json"])
        code, output, _ = self.run_cli(["show"])
        self.assertEqual(code, 0)
        self.assertIn('"config_hash": "latest-hash"', output)

    def test_second_set_preserves_other_tracks_and_rollback_swaps(self):
        for arguments in (self.set_args(), self.set_args(track="us_llm"), self.set_args("strategy-2")):
            self.assertEqual(self.run_cli([*arguments, "--yes"])[0], 0)
        registry = json.loads(self.path.read_text())
        before = self.path.read_bytes()
        self.assertEqual(registry["crypto_llm"]["previous"]["config_hash"], "latest-hash")
        self.assertEqual(registry["crypto_llm"]["current"]["config_hash"], "other-hash")
        arguments = ["rollback", "--track", "crypto_llm"]
        self.assertEqual(self.run_cli(arguments)[0], 0)
        self.assertEqual(before, self.path.read_bytes())
        self.assertEqual(self.run_cli([*arguments, "--yes"])[0], 0)
        rolled = json.loads(self.path.read_text())
        self.assertEqual(rolled["crypto_llm"]["current"], registry["crypto_llm"]["previous"])
        self.assertEqual(rolled["crypto_llm"]["previous"], registry["crypto_llm"]["current"])
        self.assertEqual(rolled["us_llm"], registry["us_llm"])
        self.assertEqual(self.run_cli([*arguments, "--yes"])[0], 0)
        self.assertEqual(json.loads(self.path.read_text()), registry)

    def test_newest_changed_at_then_id_and_readonly_access(self):
        with sqlite3.connect(self.db) as connection:
            connection.execute("INSERT INTO strategy_config_versions VALUES (5,'strategy','tie-hash','','','','{}',200,0)")
            connection.execute("PRAGMA journal_mode=WAL")
        original = self.db.read_bytes()
        with sqlite3.connect(self.db) as writer:
            writer.execute("BEGIN IMMEDIATE")
            writer.execute("UPDATE strategy_config_versions SET config_hash='uncommitted' WHERE id=5")
            self.assertEqual(baseline.latest_config_hash(self.db, "strategy"), "tie-hash")
            writer.rollback()
        self.assertEqual(self.db.read_bytes(), original)
        with closing(open_database(self.db)) as connection:
            connection.execute("PRAGMA query_only=OFF")
            with self.assertRaises(sqlite3.OperationalError):
                connection.execute("DELETE FROM strategy_config_versions")

    def test_missing_version_or_schema_and_no_previous_rollback_refused(self):
        self.assertEqual(self.run_cli([*self.set_args("missing"), "--yes"])[0], 1)
        self.assertEqual(self.run_cli(["rollback", "--track", "crypto_llm", "--yes"])[0], 1)
        self.assertFalse(self.path.exists())
        self.assertEqual(self.run_cli([*self.set_args(), "--yes"])[0], 0)
        original = self.path.read_bytes()
        self.assertEqual(self.run_cli(["rollback", "--track", "crypto_llm", "--yes"])[0], 1)
        self.assertEqual(original, self.path.read_bytes())
        with sqlite3.connect(self.db) as connection:
            connection.execute("DROP TABLE strategy_config_versions")
        self.assertEqual(self.run_cli([*self.set_args(), "--yes"])[0], 1)
        self.assertEqual(original, self.path.read_bytes())

    def test_atomic_replace_failure_preserves_registry_and_cleans_temp(self):
        self.assertEqual(self.run_cli([*self.set_args(), "--yes"])[0], 0)
        original = self.path.read_bytes()
        with patch("scripts.promotion.create_pair.os.replace", side_effect=OSError("替换失败")):
            code, _, errors = self.run_cli([*self.set_args("strategy-2"), "--yes"])
        self.assertEqual(code, 1)
        self.assertIn("替换失败", errors)
        self.assertEqual(self.path.read_bytes(), original)
        self.assertEqual([p.name for p in self.path.parent.iterdir() if p.name != ".pairs.lock"], ["baselines.json"])

    def test_explicit_paths_and_invalid_registry_never_overwritten(self):
        alternate = self.directory / "alternate.json"
        self.assertEqual(self.run_cli([*self.set_args(), "--yes", "--db", str(self.db),
                                       "--registry", str(alternate)])[0], 0)
        self.assertTrue(alternate.exists())
        self.assertFalse(self.path.exists())
        for content in ("[]", '{"crypto_llm": {}}', '{"crypto_llm":{"current":42,"previous":null}}'):
            with self.subTest(content=content):
                alternate.write_text(content)
                self.assertEqual(self.run_cli([*self.set_args(), "--yes", "--registry", str(alternate)])[0], 1)
                self.assertEqual(alternate.read_text(), content)


if __name__ == "__main__":
    unittest.main()
