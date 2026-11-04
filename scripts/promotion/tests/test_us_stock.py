"""us_stock paper pair (long-only control vs long+short candidate), temp DBs only."""

from contextlib import closing, redirect_stderr, redirect_stdout
from dataclasses import replace
from datetime import timedelta
import io
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from scripts.promotion import create_pair as pair_cli
from scripts.promotion import report as promotion
from scripts.promotion.tests import _golden_legacy_fixture as golden
from scripts.promotion.tests.test_report import START


US_CONFIG = (' {\n  "strategy_type" : "us_stock",\n'
             ' "stock_config" : { "paper_trading" : false, "short_mode" : "off", "max_positions" : 5 },\n'
             ' "indicators" : { "x" : 1 }\n}\n')
US_SOURCE = "binance01_user-model_12940599-650a-4e4b-a5fc-986208fb912e"
CREATE_SCHEMA = """
CREATE TABLE `traders` (`id` text,`user_id` text NOT NULL DEFAULT "default",`name` text NOT NULL,`ai_model_id` text NOT NULL,`exchange_id` text NOT NULL,`strategy_id` text DEFAULT "",`initial_balance` real NOT NULL,`is_running` numeric DEFAULT false,`show_in_competition` numeric DEFAULT true,`run_version` integer NOT NULL DEFAULT 0,PRIMARY KEY (`id`));
CREATE TABLE `strategies` (`id` text,`user_id` text NOT NULL DEFAULT "",`name` text NOT NULL,`is_active` numeric DEFAULT false,`is_default` numeric DEFAULT false,`is_public` numeric DEFAULT false,`config` text NOT NULL DEFAULT "{}",PRIMARY KEY (`id`));
CREATE TABLE `exchanges` (`id` text,`exchange_type` text NOT NULL DEFAULT "",`account_name` text NOT NULL DEFAULT "",`user_id` text NOT NULL DEFAULT "default",`name` text NOT NULL,`type` text NOT NULL,`enabled` numeric DEFAULT false,`api_key` text DEFAULT "",`secret_key` text DEFAULT "",`created_at` datetime,`updated_at` datetime,PRIMARY KEY (`id`));
"""

US_OPTIONS = pair_cli.PairOptions(US_SOURCE, "stock_config.short_mode", "paper", "us-short-v1", "美股做空")


class UsStockPlanTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix=".test-us-stock-", dir=Path(__file__).parent)
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.db = self.directory / "paper #1?.db"
        self.promotion = self.directory / "promotion"
        self.backups = self.directory / "backups"
        with sqlite3.connect(self.db) as connection:
            connection.executescript(CREATE_SCHEMA)
            connection.execute("ALTER TABLE exchanges ADD COLUMN paper_config TEXT NOT NULL DEFAULT ''")
            connection.execute("INSERT INTO exchanges(id,user_id,exchange_type,name,type,enabled) "
                               "VALUES ('binance-1','user','binance','Binance','cex',1)")
            connection.execute("INSERT INTO strategies(id,user_id,name,config) VALUES ('source-strategy','user','美股实盘',?)",
                               (US_CONFIG,))
            connection.execute("INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,strategy_id,"
                               "initial_balance,is_running) VALUES (?,'user','美股交易员','user-model','binance-1',"
                               "'source-strategy',4200,1)", (US_SOURCE,))

    def rows(self, table, database=None):
        with closing(pair_cli.open_database(database or self.db)) as connection:
            return [dict(row) for row in connection.execute(f'SELECT * FROM "{table}" ORDER BY id')]

    def test_plan_reuses_binance_exchange_forces_paper_and_patches_candidate_only(self):
        options = replace(US_OPTIONS, paper_cash=777, prereg=pair_cli.PreregOptions(
            "us_llm", "config-v1", "us-short", "E", metric="daily_return", trade_ratio_max=None,
            min_trades=30, min_short_trades=15, min_trades_scope="candidate", min_days=63, harm_check_days=20))
        plan, _ = pair_cli.create_pair(self.db, self.promotion, options, apply=True, backup_dir=self.backups)
        self.assertEqual([len(self.rows(table)) for table in ("exchanges", "strategies", "traders")], [1, 3, 3])
        self.assertEqual(self.rows("exchanges")[0]["exchange_type"], "binance")
        configs = {}
        for role in ("candidate", "control"):
            ids = plan["ids"][role]
            trader = next(row for row in self.rows("traders") if row["id"] == ids["trader_id"])
            strategy = next(row for row in self.rows("strategies") if row["id"] == ids["strategy_id"])
            self.assertEqual(trader["exchange_id"], "binance-1")  # never a new paper exchange
            self.assertEqual(trader["initial_balance"], 777)  # paper-cash on both clones
            self.assertFalse(trader["is_running"])
            self.assertEqual(strategy["is_active"], 0)
            configs[role] = json.loads(strategy["config"])
            self.assertEqual(ids["exchange_id"], "binance-1")
        self.assertEqual(configs["control"], {"strategy_type": "us_stock",
                                              "stock_config": {"paper_trading": True, "short_mode": "off",
                                                               "max_positions": 5},
                                              "indicators": {"x": 1}})
        self.assertEqual(configs["candidate"]["stock_config"],
                         {"paper_trading": True, "short_mode": "paper", "max_positions": 5})
        pair = json.loads((self.promotion / "pairs.json").read_text())["pairs"][0]
        self.assertEqual(pair["paper_cash"], 777)
        self.assertEqual(pair["prereg"]["metric"], "daily_return")
        self.assertIsNone(pair["prereg"]["trade_ratio_max"])
        self.assertTrue(pair_cli.verify_prereg(pair["prereg"]))

    def test_dry_run_is_readonly_and_prints_us_stock_specifics(self):
        before = self.db.read_bytes()
        with patch.dict(os.environ, {"PROMOTION_DIR": str(self.promotion), "NOFX_DB": str(self.db)}):
            with redirect_stdout(io.StringIO()) as output:
                self.assertEqual(pair_cli.main([
                    "--source-trader", US_SOURCE, "--feature-path", US_OPTIONS.feature_path,
                    "--feature-value", '"paper"', "--pair-id", US_OPTIONS.pair_id, "--title", US_OPTIONS.title,
                    "--backup-dir", str(self.backups)]), 0)
        self.assertEqual(before, self.db.read_bytes())
        self.assertFalse(self.promotion.exists())
        text = output.getvalue()
        self.assertEqual(text.count('INSERT INTO "'), 4)  # two strategies, two traders, no exchange
        self.assertIn("不新建 paper 交易所，两组复用源 binance 交易所 binance-1", text)
        self.assertIn("stock_config.paper_trading=true", text)
        self.assertIn("initial_balance=paper-cash=1000 USDT", text)
        self.assertIn('stock_config.short_mode="paper" 仅作用于候选', text)
        self.assertIn("exchange=binance-1", text)

    def test_refusals(self):
        snapshot = self.directory / "invalid.db"

        def isolated(sql):
            with sqlite3.connect(self.db) as connection:
                connection.execute("BEGIN")
                connection.execute(sql)
                snapshot.write_bytes(connection.serialize())
                connection.rollback()

        cases = [
            ("UPDATE exchanges SET exchange_type='okx', type='cex'", "binance"),
            ("UPDATE exchanges SET exchange_type='cex', type='paper'", "不能是 paper"),
            ("UPDATE strategies SET config='{\"strategy_type\":\"us_stock\",\"stock_config\":{\"paper_trading\":true}}'",
             "paper_trading"),
        ]
        for sql, message in cases:
            with self.subTest(sql=sql):
                isolated(sql)
                for apply in (False, True):
                    with self.assertRaisesRegex(ValueError, message):
                        pair_cli.create_pair(snapshot, self.promotion, US_OPTIONS, apply=apply,
                                             backup_dir=self.backups)
                self.assertEqual([len(self.rows(table)) for table in ("exchanges", "strategies", "traders")],
                                 [1, 1, 1])
                self.assertFalse((self.promotion / "pairs.json").exists())
        with self.assertRaisesRegex(ValueError, "paper_trading"):
            pair_cli.create_pair(self.db, self.promotion,
                                 replace(US_OPTIONS, feature_path="stock_config.paper_trading",
                                         feature_value=False))
        with self.assertRaisesRegex(ValueError, "paper-cash"):
            replace(US_OPTIONS, paper_cash=0).validate()

    def test_non_us_stock_ignores_paper_cash_and_keeps_paper_exchange(self):
        document = {"pairs": []}
        self.promotion.mkdir()
        (self.promotion / "pairs.json").write_text(json.dumps(document), encoding="utf-8")
        with sqlite3.connect(self.db) as connection:
            connection.execute("UPDATE strategies SET config='{\"indicators\":{\"enable_x\":false}}'")
            connection.execute("UPDATE exchanges SET exchange_type='binance' WHERE id='binance-1'")
        plan, _ = pair_cli.create_pair(self.db, self.promotion,
                                       replace(pair_cli.PairOptions(US_SOURCE, "indicators.enable_x", True,
                                                                    "crypto-v1", "加密"),
                                               initial_balance=2500, paper_cash=999),
                                       apply=True, backup_dir=self.backups)
        self.assertNotIn("paper_cash", plan["pair"])
        self.assertEqual(len(self.rows("exchanges")), 3)
        self.assertEqual(sorted(row["initial_balance"] for row in self.rows("traders")), [2500, 2500, 4200])


class PreregFieldTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix=".test-us-prereg-", dir=Path(__file__).parent)
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.db = self.directory / "paper #1?.db"
        with sqlite3.connect(self.db) as connection:
            connection.executescript(CREATE_SCHEMA)
            connection.execute("ALTER TABLE exchanges ADD COLUMN paper_config TEXT NOT NULL DEFAULT ''")
            connection.execute("INSERT INTO exchanges(id,user_id,exchange_type,name,type) "
                               "VALUES ('binance-1','user','binance','Binance','cex')")
            connection.execute("INSERT INTO strategies(id,user_id,name,config) VALUES ('st','user','美股',?)",
                               (US_CONFIG,))
            connection.execute("INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,strategy_id,"
                               "initial_balance) VALUES (?,'user','美股交易员','user-model','binance-1','st',100)",
                               (US_SOURCE,))

    def registered(self, **kwargs):
        return pair_cli.make_prereg(replace(pair_cli.PreregOptions("us_llm", "config-v1", "做空", "E"), **kwargs),
                                    promotion.iso(START))

    def test_new_fields_are_registered_hashed_and_verified(self):
        prereg = self.registered(metric="daily_return", margin_daily=0.0003, trade_ratio_max=None,
                                 min_short_trades=15, min_trades_scope="candidate", extend_se_daily=0.0002)
        body = {key: value for key, value in prereg.items() if key != "prereg_hash"}
        self.assertEqual(prereg["prereg_hash"], pair_cli.prereg_hash(prereg))
        for key, value in (("metric", "daily_return"), ("margin_daily", 0.0003), ("trade_ratio_max", None),
                           ("min_short_trades", 15), ("min_trades_scope", "candidate"), ("extend_se_daily", 0.0002)):
            self.assertEqual(prereg[key], value, key)
        # The hash covers the new fields: altering any of them breaks verification.
        mutations = {"metric": "avg_r", "margin_daily": 0.0002, "trade_ratio_max": 1.5,
                     "min_short_trades": 16, "min_trades_scope": "both", "extend_se_daily": 0.0001}
        for key, value in mutations.items():
            with self.subTest(key=key):
                self.assertNotEqual(prereg[key], value)
                self.assertFalse(pair_cli.verify_prereg({**prereg, key: value}))
        self.assertTrue(pair_cli.verify_prereg(dict(reversed(list(prereg.items())))))
        for kwargs in ({"metric": "sharpe"}, {"min_trades_scope": "candidate "}, {"trade_ratio_max": -0.1},
                       {"min_short_trades": 0}, {"min_short_trades": 1.5}, {"margin_daily": -1},
                       {"extend_se_daily": -1e-9}, {"extend_se_daily": float("nan")}):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                self.registered(**kwargs)

    def test_defaults_match_documented_values(self):
        prereg = self.registered()
        self.assertEqual((prereg["metric"], prereg["margin_daily"], prereg["trade_ratio_max"],
                          prereg["min_short_trades"], prereg["min_trades_scope"], prereg["extend_se_daily"]),
                         ("avg_r", 0.0002, 1.5, None, "both", 0.0001))

    def test_old_prereg_without_new_keys_still_verifies(self):
        prereg = {**vars(pair_cli.PreregOptions("crypto_llm", "config-v1", "ensemble", "A")),
                  "registered_at": promotion.iso(START)}
        for key in ("metric", "margin_daily", "trade_ratio_max", "min_short_trades",
                    "min_trades_scope", "extend_se_daily"):
            del prereg[key]
        prereg["prereg_hash"] = pair_cli.prereg_hash(prereg)
        self.assertTrue(pair_cli.verify_prereg(prereg))
        # Any later edit to the sealed body is still tampering.
        self.assertFalse(pair_cli.verify_prereg({**prereg, "retroactive": True, "note": "补登记"}))

    def test_cli_flags_defaults_none_and_mode_guard(self):
        arguments = ["--source-trader", US_SOURCE, "--feature-path", "stock_config.short_mode",
                     "--feature-value", '"paper"', "--pair-id", "p1", "--title", "标题",
                     "--mode", "incremental", "--track", "us_llm", "--baseline-version", "cfg",
                     "--idea-id", "做空", "--change-type", "E", "--metric", "daily_return",
                     "--trade-ratio-max", "none", "--min-trades", "30", "--min-trades-scope", "candidate",
                     "--min-short-trades", "15"]
        with patch.dict(os.environ, {"PROMOTION_DIR": str(self.directory), "NOFX_DB": str(self.db)}):
            with patch.object(pair_cli, "print_plan") as printed:
                self.assertEqual(pair_cli.main(arguments), 0)
            prereg = printed.call_args.args[0]["pair"]["prereg"]
            self.assertEqual((prereg["metric"], prereg["margin_daily"], prereg["trade_ratio_max"],
                              prereg["min_trades_scope"], prereg["min_short_trades"], prereg["extend_se_daily"]),
                             ("daily_return", 0.0002, None, "candidate", 15, 0.0001))
            self.assertTrue(pair_cli.verify_prereg(prereg))
            with patch.object(pair_cli, "print_plan") as printed:
                without_optional = [item for item in arguments
                                    if item not in ("--trade-ratio-max", "none", "--min-short-trades", "15")]
                self.assertEqual(pair_cli.main(without_optional), 0)  # both flags absent
            prereg = printed.call_args.args[0]["pair"]["prereg"]
            self.assertIsNone(prereg["min_short_trades"])
            self.assertEqual(prereg["trade_ratio_max"], 1.5)  # default, not none
            base = ["--source-trader", US_SOURCE, "--feature-path", "x", "--feature-value", "1",
                    "--pair-id", "p", "--title", "t"]
            for extra in (["--metric", "daily_return"], ["--min-short-trades", "15"],
                          ["--margin-daily", "0.001"], ["--extend-se-daily", "0.001"],
                          ["--min-trades-scope", "candidate"], ["--trade-ratio-max", "none"],
                          ["--trade-ratio-max", "2"]):
                with redirect_stderr(io.StringIO()) as errors:
                    self.assertEqual(pair_cli.main([*base, *extra]), 1)
                self.assertIn("预登记字段需要 --mode incremental", errors.getvalue())


class DailyReturnDecisionTests(unittest.TestCase):
    def decide(self, *, days=63, delta=0.0005, se=0.0001, cn=30, bn=30, short=30, **prereg):
        options = replace(pair_cli.PreregOptions("us_llm", "config-v1", "做空", "E"),
                          min_trades=prereg.pop("min_trades", 30), min_days=63, harm_check_days=20, **prereg)
        return promotion.decide_daily_return(
            days, vars(options), {"n": cn, "short_n": short}, {"n": bn},
            {"mean_diff": delta, "cluster_se": se})[0]

    def test_all_ordered_paths_and_boundaries(self):
        cases = [
            ("insufficient", {"days": 19.999}),
            ("harm_stop", {"days": 20, "delta": -0.001, "se": 0.0001}),
            ("insufficient", {"days": 20, "delta": -0.0002, "se": 0.0001}),
            ("insufficient", {"days": 62.999}),
            ("insufficient", {"cn": 29}), ("insufficient", {"bn": 29}),
            ("promote", {}), ("promote", {"cn": 45}),  # trade_ratio_max default 1.5 admits a 1.5 ratio
            ("reject", {"cn": 60}),  # 2.0 ratio above the default upper bound
            ("fewer_trades", {"cn": 20, "min_trades": 20}),
            ("extend", {"delta": 0.00001, "se": 0.0002}),
            ("extend", {"days": 76.999, "delta": 0, "se": 0.0002}),
            ("reject", {"days": 77, "delta": 0, "se": 0.0002}),
            ("reject", {"delta": 0, "se": 0.0001}), ("reject", {"delta": -0.0001}),
            ("reject", {"se": None}), ("reject", {"delta": None, "se": None}),
            ("promote", {"se": 0}), ("reject", {"delta": 0, "se": 0}),
        ]
        for expected, kwargs in cases:
            with self.subTest(expected=expected, **kwargs):
                self.assertEqual(self.decide(**kwargs), expected)

    def test_ratio_two_promotes_only_without_upper_bound(self):
        self.assertEqual(self.decide(cn=60, trade_ratio_max=None), "promote")
        self.assertEqual(self.decide(cn=60), "reject")  # default 1.5 blocks a 2.0 ratio
        self.assertEqual(self.decide(cn=60, trade_ratio_max=2.0), "promote")
        self.assertEqual(self.decide(cn=60, trade_ratio_max=1.9, days=76.9, se=0.0002), "extend")

    def test_lower_bound_uses_margin_daily_strictly(self):
        margin = 1.645 * 0.0001 - 1e-6  # lower == -margin exactly: strict > must reject
        self.assertEqual(self.decide(delta=1e-6, se=0.0001, margin_daily=margin), "reject")
        self.assertEqual(self.decide(delta=1e-6, se=0.0001, margin_daily=margin + 1e-12), "promote")

    def test_min_trades_scope_and_min_short_trades_gating(self):
        self.assertEqual(self.decide(bn=1, min_trades_scope="candidate", trade_ratio_max=None), "promote")
        self.assertEqual(self.decide(bn=1, min_trades_scope="candidate", short=29, min_short_trades=30),
                         "insufficient")
        self.assertEqual(self.decide(short=29, min_short_trades=30), "insufficient")
        self.assertEqual(self.decide(short=0, min_short_trades=1), "insufficient")
        self.assertEqual(self.decide(short=0), "promote")  # min_short_trades=None never gates
        options = vars(replace(pair_cli.PreregOptions("us_llm", "c", "i", "E"),
                               min_trades=30, min_trades_scope="candidate", min_short_trades=30))
        verdict, reasons = promotion.decide_daily_return(63, options, {"n": 30, "short_n": 29}, {"n": 0},
                                                         {"mean_diff": 0.001, "cluster_se": 0.00001})
        self.assertEqual(verdict, "insufficient")
        self.assertEqual(reasons, ["候选 SHORT 平仓 29 笔，少于要求的 30 笔"])


class UsStockReportTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix=".test-us-report-", dir=Path(__file__).parent)
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.db = self.directory / "paper #1?.db"
        self.schema_full = """
        CREATE TABLE traders (id TEXT PRIMARY KEY, strategy_id TEXT);
        CREATE TABLE strategies (id TEXT PRIMARY KEY, config TEXT);
        CREATE TABLE trader_equity_snapshots (
            id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL,
            timestamp DATETIME NOT NULL, total_equity REAL NOT NULL DEFAULT 0);
        CREATE TABLE stock_paper_positions (
            id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL, symbol TEXT NOT NULL,
            quantity REAL DEFAULT 0, entry_quantity REAL DEFAULT 0, avg_price REAL DEFAULT 0,
            initial_stop REAL DEFAULT 0, stop REAL DEFAULT 0, take_profit REAL DEFAULT 0,
            opened_at INTEGER DEFAULT 0, updated_at INTEGER DEFAULT 0, status TEXT DEFAULT 'OPEN',
            exited_qty REAL DEFAULT 0, exit_value REAL DEFAULT 0, exit_price REAL DEFAULT 0,
            realized_pnl REAL DEFAULT 0, closed_at INTEGER DEFAULT 0, close_reason TEXT DEFAULT '',
            fee REAL DEFAULT 0, side TEXT DEFAULT 'LONG', funding REAL DEFAULT 0);
        """
        self.schema_legacy = """
        CREATE TABLE traders (id TEXT PRIMARY KEY, strategy_id TEXT);
        CREATE TABLE strategies (id TEXT PRIMARY KEY, config TEXT);
        CREATE TABLE trader_equity_snapshots (
            id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL,
            timestamp DATETIME NOT NULL, total_equity REAL NOT NULL DEFAULT 0);
        CREATE TABLE stock_paper_positions (
            id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL, symbol TEXT NOT NULL,
            quantity REAL DEFAULT 0, entry_quantity REAL DEFAULT 0, avg_price REAL DEFAULT 0,
            initial_stop REAL DEFAULT 0, stop REAL DEFAULT 0, take_profit REAL DEFAULT 0,
            opened_at INTEGER DEFAULT 0, updated_at INTEGER DEFAULT 0, status TEXT DEFAULT 'OPEN',
            exited_qty REAL DEFAULT 0, exit_value REAL DEFAULT 0, exit_price REAL DEFAULT 0,
            realized_pnl REAL DEFAULT 0, closed_at INTEGER DEFAULT 0, close_reason TEXT DEFAULT '',
            fee REAL DEFAULT 0);
        """

    def populate(self, *, with_side=True, gain=1.001):
        with sqlite3.connect(self.db) as connection:
            connection.executescript(self.schema_full if with_side else self.schema_legacy)
            connection.execute("INSERT INTO traders VALUES ('candidate','st-c'),('control','st-b')")
            connection.execute("INSERT INTO strategies VALUES "
                               "('st-c','{\"strategy_type\":\"us_stock\",\"stock_config\":{\"paper_trading\":true,\"short_mode\":\"paper\"}}'),"
                               "('st-b','{\"strategy_type\":\"us_stock\",\"stock_config\":{\"paper_trading\":true,\"short_mode\":\"off\"}}')")
            for day in range(11):  # 10 completed UTC days; candidate moves gain-1 per day, control flat
                moment = promotion.iso(START + timedelta(days=day, hours=23))
                connection.execute("INSERT INTO trader_equity_snapshots (trader_id,timestamp,total_equity) "
                                   "VALUES ('candidate',?,?)", (moment, 1000 * (gain ** day)))
                connection.execute("INSERT INTO trader_equity_snapshots (trader_id,timestamp,total_equity) "
                                   "VALUES ('control',?,?)", (moment, 1000.0))
            # risk = |50-49| * entry_quantity(2) = 2; R = realized_pnl / 2 (gross, fee ignored)
            rows = [("candidate", "SHORT", 3.0, 0.25, 1.0), ("candidate", "SHORT", 1.0, 0.25, 2.0),
                    ("candidate", "LONG", 2.0, 0.0, -0.5), ("candidate", "LONG", -2.0, 0.0, 0.0),
                    ("control", "LONG", 2.0, 0.1, 0.0)]
            for index, (trader, side, pnl, fee, funding) in enumerate(rows, start=1):
                closed = int((START + timedelta(days=index, hours=4)).timestamp() * 1000)
                if with_side:
                    connection.execute(
                        "INSERT INTO stock_paper_positions (trader_id,symbol,quantity,entry_quantity,avg_price,"
                        "initial_stop,status,opened_at,closed_at,realized_pnl,fee,side,funding) "
                        "VALUES (?,?,?,?,?,?,'CLOSED',?,?,?,?,?,?)",
                        (trader, "AAPLUSDT", 0, 2, 50, 49, int(START.timestamp() * 1000), closed, pnl, fee,
                         side, funding))
                else:  # the migrated columns do not exist yet
                    connection.execute(
                        "INSERT INTO stock_paper_positions (trader_id,symbol,quantity,entry_quantity,avg_price,"
                        "initial_stop,status,opened_at,closed_at,realized_pnl,fee) "
                        "VALUES (?,?,?,?,?,?,'CLOSED',?,?,?,?)",
                        (trader, "AAPLUSDT", 0, 2, 50, 49, int(START.timestamp() * 1000), closed, pnl, fee))
            if with_side:
                connection.execute("INSERT INTO stock_paper_positions (trader_id,symbol,quantity,entry_quantity,"
                                   "avg_price,initial_stop,status,opened_at,realized_pnl,fee,side,funding) "
                                   "VALUES ('candidate','TSLAUSDT',1,1,200,190,'OPEN',?,0,0,'SHORT',0.75)",
                                   (int(START.timestamp() * 1000),))
            else:
                connection.execute("INSERT INTO stock_paper_positions (trader_id,symbol,quantity,entry_quantity,"
                                   "avg_price,initial_stop,status,opened_at,realized_pnl,fee) "
                                   "VALUES ('candidate','TSLAUSDT',1,1,200,190,'OPEN',?,0,0)",
                                   (int(START.timestamp() * 1000),))

    def configured_pair(self, **overrides):
        options = pair_cli.PreregOptions("us_llm", "config-v1", "做空", "E", metric="daily_return",
                                         trade_ratio_max=None, min_trades=4, min_trades_scope="candidate",
                                         min_short_trades=2, min_days=7, harm_check_days=2)
        prereg = pair_cli.make_prereg(options, promotion.iso(START))
        return {"id": "us-short", "title": "美股做空", "feature": "stock_config.short_mode",
                "candidate_trader_id": "candidate", "control_trader_id": "control",
                "live_trader_id": None, "started_at": promotion.iso(START),
                "min_days": 7, "min_trades": 4, "paper_cash": 1000, "prereg": prereg, **overrides}

    def test_stock_paper_trade_source_sides_funding_and_daily_promote(self):
        self.populate()
        item = promotion.build_report([self.configured_pair()], self.db, START + timedelta(days=10, hours=12))
        item = item["pairs"][0]
        candidate, control = item["candidate"], item["control"]
        self.assertEqual(candidate["n"], 4)  # closed stock_paper_positions only
        self.assertEqual(candidate["short_n"], 2)
        self.assertEqual(candidate["long_n"], 2)
        self.assertEqual(candidate["open_positions"], 1)  # counted in stock_paper_positions
        self.assertAlmostEqual(candidate["avg_r"], (1.5 + 0.5 + 1.0 - 1.0) / 4)  # gross R on realized_pnl
        self.assertAlmostEqual(candidate["short_avg_r"], (1.5 + 0.5) / 2)
        self.assertAlmostEqual(candidate["long_avg_r"], (1.0 - 1.0) / 2)
        self.assertAlmostEqual(candidate["short_funding_usdt"], 3.0)  # closed SHORT rows only
        self.assertAlmostEqual(candidate["net_pnl_usdt"], 3.0 + 1.0 + 2.0 - 2.0 - 0.5)
        self.assertEqual(control["n"], 1)  # control is us_stock too: side fields exist, all LONG
        self.assertEqual((control["short_n"], control["long_n"]), (0, 1))
        self.assertIsNone(control["short_avg_r"])
        self.assertEqual(control["short_funding_usdt"], 0.0)
        self.assertEqual(item["verdict"], "promote")
        self.assertEqual(item["incremental_metric"], "daily_return")
        self.assertAlmostEqual(item["delta"], 0.001)
        self.assertAlmostEqual(item["se"], 0.0)  # constant daily diff, weekly clusters
        self.assertEqual(item["trade_ratio"], 4.0)  # no upper bound
        daily = item["incremental_daily_return"]
        self.assertEqual(daily["n_days"], 9)  # Oct 2–10; Oct 1 has no previous day, Oct 11 incomplete
        self.assertEqual(daily["n_weeks"], 2)
        self.assertAlmostEqual(candidate["max_drawdown"], 0.0)
        extra = item["us_stock_extra"]
        self.assertEqual(extra["candidate_short_n"], 2)
        self.assertAlmostEqual(extra["long_avg_r_diff"], 0.0 - 1.0)
        self.assertEqual(extra["candidate_short_funding_usdt"], 3.0)
        self.assertEqual(extra["max_drawdown"]["control"], 0.0)
        text = promotion.summary({"generated_at": "t", "pairs": [item], "caveats": "c"})
        self.assertIn("做空辅助(仅报告)", text)
        self.assertIn("日收益配对: 配对日=9", text)
        self.assertIn("trade_ratio_max=不设上限", text)

    def test_min_short_trades_gates_insufficient(self):
        self.populate()
        strict = self.configured_pair()
        strict["prereg"].update(min_short_trades=3, min_trades=4)
        strict["prereg"]["prereg_hash"] = pair_cli.prereg_hash(strict["prereg"])
        item = promotion.build_report([strict], self.db, START + timedelta(days=10, hours=12))["pairs"][0]
        self.assertEqual(item["verdict"], "insufficient")
        self.assertEqual(item["reasons"], ["候选 SHORT 平仓 2 笔，少于要求的 3 笔"])
        # scope=candidate: the control's single close does not gate the verdict.
        lax = self.configured_pair()
        lax["prereg"].update(min_short_trades=2, min_trades=4)
        lax["prereg"]["prereg_hash"] = pair_cli.prereg_hash(lax["prereg"])
        self.assertEqual(promotion.build_report([lax], self.db,
                                                START + timedelta(days=10, hours=12))["pairs"][0]["verdict"],
                         "promote")

    def test_harm_stop_is_checked_before_the_sample_floors(self):
        self.populate(gain=0.995)  # constant −0.5%/day: SE collapses to zero, harm is unambiguous
        item = promotion.build_report([self.configured_pair()], self.db, START + timedelta(days=8))["pairs"][0]
        self.assertEqual(item["verdict"], "harm_stop")  # despite candidate n=4 < min_trades
        self.assertAlmostEqual(item["delta"], -0.005)
        self.assertAlmostEqual(item["se"], 0.0)

    def test_missing_side_and_funding_columns_mean_long_and_zero(self):
        self.populate(with_side=False)
        item = promotion.build_report([self.configured_pair()], self.db, START + timedelta(days=10, hours=12))
        item = item["pairs"][0]
        self.assertEqual(item["candidate"]["n"], 4)
        self.assertEqual(item["candidate"]["short_n"], 0)  # every row is a LONG
        self.assertEqual(item["candidate"]["short_funding_usdt"], 0.0)
        self.assertIsNone(item["candidate"]["short_avg_r"])
        self.assertAlmostEqual(item["candidate"]["avg_r"], (1.5 + 0.5 + 1.0 - 1.0) / 4)
        self.assertEqual(item["verdict"], "insufficient")  # min_short_trades=3 can never pass

    def test_exit_window_and_gross_r_formula(self):
        self.populate()
        with sqlite3.connect(self.db) as connection:
            inside = int((START + timedelta(days=9)).timestamp() * 1000)
            connection.execute("INSERT INTO stock_paper_positions (trader_id,symbol,quantity,entry_quantity,"
                               "avg_price,initial_stop,status,opened_at,closed_at,realized_pnl,fee,side,funding) "
                               "VALUES ('candidate','NVDAUSDT',0,2,50,49,'CLOSED',?,?,3,0,'SHORT',0)",
                               (inside - 1, inside))
            early = int((START - timedelta(days=1)).timestamp() * 1000)
            connection.execute("INSERT INTO stock_paper_positions (trader_id,symbol,quantity,entry_quantity,"
                               "avg_price,initial_stop,status,opened_at,closed_at,realized_pnl,fee,side,funding) "
                               "VALUES ('candidate','OLDUSDT',0,2,50,49,'CLOSED',?,?,99,0,'SHORT',0)",
                               (early, early))
            late = int((START + timedelta(days=11)).timestamp() * 1000)
            connection.execute("INSERT INTO stock_paper_positions (trader_id,symbol,quantity,entry_quantity,"
                               "avg_price,initial_stop,status,opened_at,closed_at,realized_pnl,fee,side,funding) "
                               "VALUES ('candidate','NEWUSDT',0,2,50,49,'CLOSED',?,?,99,0,'SHORT',0)",
                               (late, late))
        item = promotion.build_report([self.configured_pair()], self.db, START + timedelta(days=10, hours=12))
        item = item["pairs"][0]
        # Window bounds are exact: rows strictly outside [started_at, as_of] never count.
        self.assertEqual(item["candidate"]["n"], 5)
        self.assertAlmostEqual(item["candidate"]["short_avg_r"], (1.5 + 0.5 + 1.5) / 3)

    def test_legacy_report_identity_with_golden(self):
        reference = json.loads(golden.Path(__file__).with_name("_golden_legacy_report.json").read_text(encoding="utf-8"))
        with golden.tempfile.TemporaryDirectory(prefix="golden-check-") as temporary:
            db_path = golden.Path(temporary) / "fixture.db"
            golden.populate(db_path)
            current = golden.build(db_path)
        self.assertEqual(current, reference)


if __name__ == "__main__":
    unittest.main()
