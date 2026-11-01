"""live_kelly 的单元测试：内存 sqlite 构造数据，不读真实数据库。

覆盖: R 计算、Kelly 公式、Beta 后验、n_needed、只读打开（写入必须失败）、
以及端到端分组/按月聚合。t/beta 分位数以文献数值校验（环境无 scipy）。

运行:
    python -m unittest scripts.backtest.tests.test_live_kelly -v
"""

from __future__ import annotations

import math
import sqlite3
import tempfile
import unittest
from pathlib import Path

from scripts.backtest import live_kelly as lk

SCHEMA = """
CREATE TABLE trader_positions (
    id integer PRIMARY KEY AUTOINCREMENT,
    trader_id text NOT NULL,
    symbol text NOT NULL,
    side text NOT NULL,
    entry_quantity real DEFAULT 0,
    quantity real NOT NULL,
    entry_price real NOT NULL,
    exit_price real DEFAULT 0,
    entry_time integer,
    exit_time integer,
    realized_pnl real DEFAULT 0,
    fee real DEFAULT 0,
    leverage integer DEFAULT 1,
    status text DEFAULT 'OPEN',
    close_reason text DEFAULT '',
    source text DEFAULT 'system',
    initial_stop_loss real DEFAULT 0,
    ai_managed numeric DEFAULT 0
);
"""

T1 = "aaa11111_acct-uuid_glm_sess-1"
T2 = "bbb22222_acct-uuid_deepseek_s2"

# (trader, source, ai_managed, side, entry_quantity, quantity, entry_price,
#  initial_stop_loss, exit_price, entry_time, exit_time, realized_pnl, fee)
TRADES = [
    # T1 #1: 赢单 risk=|100-90|*1=10, R=(20-1)/10=1.9
    (T1, "system", 1, "LONG", 1.0, 1.0, 100.0, 90.0, 120.0,
     1767225600000, 1767312000000, 20.0, 1.0),
    # T1 #2: 亏单 risk=|100-90|*2=20, R=(-10-1)/20=-0.55
    (T1, "system", 1, "LONG", 2.0, 2.0, 100.0, 90.0, 95.0,
     1767225600000, 1767398400000, -10.0, 1.0),
    # T1 #3: entry_quantity=0 → 用 quantity=4; risk=|50-45|*4=20, R=(-2-2)/20=-0.2
    (T1, "system", 1, "LONG", 0.0, 4.0, 50.0, 45.0, 49.5,
     1767225600000, 1767484800000, -2.0, 2.0),
    # T1 #4: 无止损 → 不计 R
    (T1, "system", 1, "LONG", 1.0, 1.0, 100.0, 0.0, 110.0,
     1767225600000, 1767571200000, 5.0, 0.5),
    # T1 #5: SHORT 亏单 risk=|200-210|*1=10, R=(-15-1)/10=-1.6（方向随 realized_pnl）
    (T1, "system", 1, "SHORT", 1.0, 1.0, 200.0, 210.0, 215.0,
     1767225600000, 1767657600000, -15.0, 1.0),
    # T2 #6: 赢单 risk=|10-9|*10=10, R=(12-2)/10=1.0
    (T2, "system", 1, "LONG", 10.0, 10.0, 10.0, 9.0, 11.2,
     1769980800000, 1770067200000, 12.0, 2.0),
    # T2 #7: 亏单 risk=|10-9.5|*20=10, R=(-6-1)/10=-0.7
    (T2, "system", 1, "LONG", 20.0, 20.0, 10.0, 9.5, 9.7,
     1769980800000, 1770153600000, -6.0, 1.0),
    # T2 #8: 赢单 risk=|50-47.5|*4=10, R=(6-1)/10=0.5
    (T2, "system", 1, "LONG", 4.0, 4.0, 50.0, 47.5, 51.5,
     1772572800000, 1772659200000, 6.0, 1.0),
    # T2 #9: sync 路径的 AI 单，只进参考口径; risk=|5-4|*10=10, R=(8-1)/10=0.7
    (T2, "sync", 1, "LONG", 10.0, 10.0, 5.0, 4.0, 5.8,
     1772572800000, 1772745600000, 8.0, 1.0),
]


def make_memory_db() -> sqlite3.Connection:
    con = sqlite3.connect(":memory:")
    con.row_factory = sqlite3.Row
    con.execute(SCHEMA)
    insert_sql = """
        INSERT INTO trader_positions
            (trader_id, symbol, side, entry_quantity, quantity, entry_price,
             exit_price, entry_time, exit_time, realized_pnl, fee, status,
             close_reason, source, initial_stop_loss, ai_managed)
        VALUES (:trader_id, :symbol, :side, :entry_quantity, :quantity,
                :entry_price, :exit_price, :entry_time, :exit_time,
                :realized_pnl, :fee, 'CLOSED', 'ai_close', :source,
                :initial_stop_loss, :ai_managed)
    """
    keys = ("trader_id", "source", "ai_managed", "side", "entry_quantity",
            "quantity", "entry_price", "initial_stop_loss", "exit_price",
            "entry_time", "exit_time", "realized_pnl", "fee")
    for row in TRADES:
        rec = dict(zip(keys, row))
        rec["symbol"] = "SYMUSDT"
        con.execute(insert_sql, rec)
    con.commit()
    return con


def make_file_db(path: Path) -> None:
    con = sqlite3.connect(path)
    con.execute(SCHEMA)
    con.execute(
        "INSERT INTO trader_positions (trader_id, symbol, side, entry_quantity,"
        " quantity, entry_price, exit_price, entry_time, exit_time, "
        "realized_pnl, fee, status, source, initial_stop_loss) "
        "VALUES ('t','SYMUSDT','LONG',1,1,100,110,1,2,10,1,'CLOSED','system',90)")
    con.commit()
    con.close()


class RComputationTest(unittest.TestCase):
    def rec(self, **kw):
        base = dict(
            entry_price=100.0, initial_stop_loss=90.0, entry_quantity=1.0,
            quantity=1.0, realized_pnl=20.0, fee=1.0, side="LONG")
        base.update(kw)
        return base

    def test_long_win(self):
        risk, r, reason = lk.compute_r(self.rec())
        self.assertEqual(reason, "ok")
        self.assertAlmostEqual(risk, 10.0)
        self.assertAlmostEqual(r, 1.9)

    def test_short_loss_direction_follows_pnl(self):
        risk, r, _ = lk.compute_r(self.rec(
            side="SHORT", entry_price=200.0, initial_stop_loss=210.0,
            entry_quantity=1.0, realized_pnl=-15.0, fee=1.0))
        self.assertAlmostEqual(risk, 10.0)
        self.assertAlmostEqual(r, -1.6)

    def test_entry_quantity_zero_falls_back_to_quantity(self):
        risk, r, _ = lk.compute_r(self.rec(
            entry_price=50.0, initial_stop_loss=45.0, entry_quantity=0.0,
            quantity=4.0, realized_pnl=-2.0, fee=2.0))
        self.assertAlmostEqual(risk, 20.0)
        self.assertAlmostEqual(r, -0.2)

    def test_missing_stop_excluded(self):
        risk, r, reason = lk.compute_r(self.rec(initial_stop_loss=0.0))
        self.assertIsNone(risk)
        self.assertIsNone(r)
        self.assertEqual(reason, "stop_missing")

    def test_negative_stop_excluded(self):
        _, _, reason = lk.compute_r(self.rec(initial_stop_loss=-5.0))
        self.assertEqual(reason, "stop_missing")

    def test_zero_risk_invalid(self):
        _, r, reason = lk.compute_r(self.rec(initial_stop_loss=100.0))
        self.assertIsNone(r)
        self.assertEqual(reason, "risk_invalid")

    def test_fee_is_subtracted(self):
        _, r0, _ = lk.compute_r(self.rec(fee=0.0))
        _, r1, _ = lk.compute_r(self.rec(fee=1.0))
        self.assertAlmostEqual(r0 - r1, 0.1)


class KellyFormulaTest(unittest.TestCase):
    def test_classic_half_double(self):
        self.assertAlmostEqual(lk.kelly_fraction(0.5, 2.0), 0.25)

    def test_even_odds(self):
        self.assertAlmostEqual(lk.kelly_fraction(0.6, 1.0), 0.2)

    def test_negative_edge_is_negative(self):
        self.assertAlmostEqual(lk.kelly_fraction(0.3, 2.0), 0.3 - 0.7 / 2.0)

    def test_invalid_odds(self):
        self.assertIsNone(lk.kelly_fraction(0.5, 0.0))
        self.assertIsNone(lk.kelly_fraction(0.5, None))
        self.assertIsNone(lk.kelly_fraction(None, 2.0))

    def test_continuous_expectancy_over_second_moment(self):
        # R ∈ {+1, −1} 等概率: E=0 → f*=0
        self.assertAlmostEqual(lk.kelly_continuous([1.0, -1.0]), 0.0)
        # R ∈ {2, −1} 等概率: E=0.5, E[R²]=2.5 → 0.2
        self.assertAlmostEqual(lk.kelly_continuous([2.0, -1.0]), 0.2)


class QuantileImplementationTest(unittest.TestCase):
    """无 scipy，用文献数值校验自实现分位数。"""

    def test_t_ppf_known_values(self):
        # df=1 即柯西分布: t_0.975 = tan(0.475π) ≈ 12.7062；t_0.95 ≈ 6.3138
        self.assertAlmostEqual(lk.t_ppf(0.975, 1), 12.70620473617471, places=4)
        self.assertAlmostEqual(lk.t_ppf(0.95, 1), 6.313751514800096, places=4)
        self.assertAlmostEqual(lk.t_ppf(0.975, 4), 2.7764451051977987, places=4)
        self.assertAlmostEqual(lk.t_ppf(0.975, 10), 2.2281388519649385, places=4)
        self.assertAlmostEqual(lk.t_ppf(0.5, 7), 0.0)
        self.assertAlmostEqual(lk.t_ppf(0.025, 4), -2.7764451051977987, places=4)

    def test_beta_ppf_uniform(self):
        self.assertAlmostEqual(lk.beta_ppf(0.025, 1, 1), 0.025, places=6)
        self.assertAlmostEqual(lk.beta_ppf(0.975, 1, 1), 0.975, places=6)

    def test_beta_ppf_symmetric(self):
        lo = lk.beta_ppf(0.025, 2, 2)
        hi = lk.beta_ppf(0.975, 2, 2)
        self.assertAlmostEqual(lo, 0.0943, places=3)
        self.assertAlmostEqual(lo + hi, 1.0, places=6)

    def test_betainc_reg_matches_beta_cdf(self):
        # Beta(2,5) 的整数公式: I_0.3(2,5) = 1 − 0.7^6 − 6·0.3·0.7^5 = 0.579825
        self.assertAlmostEqual(lk.betainc_reg(2, 5, 0.3), 0.579825, places=5)
        # 对称分布 Beta(5,5) 在 0.5 处恰为 0.5（覆盖连分式反射分支）
        self.assertAlmostEqual(lk.betainc_reg(5, 5, 0.5), 0.5, places=9)


class BetaPosteriorTest(unittest.TestCase):
    def test_mean_is_a_over_a_plus_b(self):
        post = lk.beta_posterior(1, 4)  # Beta(2, 5)
        self.assertEqual(post["alpha"], 2)
        self.assertEqual(post["beta"], 5)
        self.assertAlmostEqual(post["mean"], 2.0 / 7.0)

    def test_credible_interval_brackets_mean(self):
        post = lk.beta_posterior(1, 4)
        lo, hi = post["ci95"]
        self.assertLess(lo, post["mean"])
        self.assertGreater(hi, post["mean"])
        self.assertTrue(0.0 < lo < hi < 1.0)

    def test_more_data_narrows_interval(self):
        wide = lk.beta_posterior(1, 4)["ci95"]
        narrow = lk.beta_posterior(100, 400)["ci95"]
        self.assertGreater(narrow[1] - narrow[0], 0)
        self.assertLess(narrow[1] - narrow[0], wide[1] - wide[0])


class SampleSizeTest(unittest.TestCase):
    def test_known_values(self):
        # (1.96 * 1 / 0.2)^2 = 96.04
        self.assertAlmostEqual(lk.n_needed(0.2, 1.0), 96.04, places=6)

    def test_nonpositive_expectancy_not_applicable(self):
        self.assertIsNone(lk.n_needed(0.0, 1.0))
        self.assertIsNone(lk.n_needed(-0.1, 1.0))

    def test_missing_std_not_applicable(self):
        self.assertIsNone(lk.n_needed(0.2, None))

    def test_scales_with_variance(self):
        self.assertAlmostEqual(
            lk.n_needed(0.2, 2.0), 4 * lk.n_needed(0.2, 1.0), places=6)


class SummarizeTest(unittest.TestCase):
    RS = [1.9, -0.55, -0.2, -1.6, 1.0, -0.7, 0.5]

    def test_basic_stats(self):
        s = lk.summarize(self.RS)
        self.assertEqual(s["n"], 7)
        self.assertEqual(s["wins"], 3)
        self.assertEqual(s["losses"], 4)
        self.assertAlmostEqual(s["p"], 3.0 / 7.0)
        self.assertAlmostEqual(s["expectancy"], 0.05, places=12)
        # 样本标准差（ddof=1）
        self.assertAlmostEqual(s["std"], math.sqrt(8.235 / 6.0), places=12)
        self.assertAlmostEqual(s["r_win"], 3.4 / 3.0)
        self.assertAlmostEqual(s["r_loss"], -3.05 / 4.0)
        self.assertAlmostEqual(s["b"], (3.4 / 3.0) / (3.05 / 4.0))
        self.assertAlmostEqual(
            s["kelly"], 3.0 / 7.0 - (4.0 / 7.0) / s["b"], places=12)
        self.assertAlmostEqual(s["kelly_cont"], 0.05 / (8.2525 / 7.0))
        self.assertAlmostEqual(s["n_needed"], (1.96 * s["std"] / 0.05) ** 2)

    def test_bootstrap_reproducible_and_sane(self):
        s1 = lk.summarize(self.RS)
        s2 = lk.summarize(self.RS)
        self.assertEqual(s1["boot_ci95"], s2["boot_ci95"])
        self.assertEqual(s1["p_positive"], s2["p_positive"])
        lo, hi = s1["boot_ci95"]
        self.assertLessEqual(lo, s1["expectancy"])
        self.assertLessEqual(s1["expectancy"], hi)
        self.assertTrue(0.0 <= s1["p_positive"] <= 1.0)

    def test_seed_changes_bootstrap(self):
        s1 = lk.summarize(self.RS, seed=1)
        s2 = lk.summarize(self.RS, seed=2)
        self.assertNotEqual(s1["boot_ci95"], s2["boot_ci95"])

    def test_empty(self):
        s = lk.summarize([])
        self.assertEqual(s["n"], 0)
        self.assertIsNone(s["expectancy"])
        self.assertIsNone(s["kelly"])
        self.assertIsNone(s["n_needed"])

    def test_all_losses_kelly_none(self):
        s = lk.summarize([-1.0, -0.5])
        self.assertEqual(s["wins"], 0)
        self.assertIsNone(s["b"])
        self.assertIsNone(s["kelly"])
        self.assertIsNone(s["n_needed"])


class ReadOnlyTest(unittest.TestCase):
    def test_open_ro_blocks_writes(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / "t.db"
            make_file_db(path)
            con = lk.open_ro(str(path))
            try:
                # 只读查询正常
                rows = lk.load_closed(con)
                self.assertEqual(len(rows), 1)
                # 尝试写入必须失败
                with self.assertRaises(sqlite3.OperationalError):
                    con.execute(
                        "INSERT INTO trader_positions (trader_id, symbol, side,"
                        " quantity, entry_price) VALUES ('x','Y','LONG',1,1)")
                with self.assertRaises(sqlite3.OperationalError):
                    con.execute("UPDATE trader_positions SET fee = 0")
                with self.assertRaises(sqlite3.OperationalError):
                    con.execute("DELETE FROM trader_positions")
            finally:
                con.close()

    def test_open_ro_blocks_ddl_and_vacuum(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / "t.db"
            make_file_db(path)
            con = lk.open_ro(str(path))
            try:
                with self.assertRaises(sqlite3.OperationalError):
                    con.execute("CREATE TABLE sneaky (x integer)")
                with self.assertRaises(sqlite3.OperationalError):
                    con.execute("DROP TABLE trader_positions")
                with self.assertRaises(sqlite3.OperationalError):
                    con.execute("VACUUM")
                with self.assertRaises(sqlite3.OperationalError):
                    con.execute("PRAGMA journal_mode=WAL")
            finally:
                con.close()


class EndToEndTest(unittest.TestCase):
    def setUp(self):
        self.con = make_memory_db()

    def tearDown(self):
        self.con.close()

    def test_labels(self):
        self.assertEqual(lk.trader_label(T1), "aaa11111_glm")
        self.assertEqual(lk.trader_label(T2), "bbb22222_deepseek")
        labels = lk.build_labels([T1, T2])
        self.assertEqual(labels[T1], "aaa11111_glm")
        # 同账户同模型重名 → 追加尾段消歧
        dup = "aaa11111_acct-uuid_glm_sess-2"
        labels2 = lk.build_labels([T1, dup])
        self.assertNotEqual(labels2[T1], labels2[dup])
        self.assertTrue(labels2[T1].startswith("aaa11111_glm"))

    def test_main_scope_groups(self):
        rows = lk.load_closed(self.con, "source='system'")
        labels = lk.build_labels(sorted({r["trader_id"] for r in rows}))
        scope = lk.analyze_scope(lk.enrich(rows, labels), "test")
        self.assertEqual(scope["closed"], 8)
        self.assertEqual(scope["with_r"], 7)
        self.assertEqual(scope["without_r"], 1)
        self.assertEqual(scope["without_r_reasons"], {"stop_missing": 1})
        keys = [g["key"] for g in scope["groups"]]
        self.assertEqual(keys[0], "全部合并")
        self.assertEqual(sorted(keys[1:]), ["aaa11111_glm", "bbb22222_deepseek"])
        all_stats = scope["groups"][0]["stats"]
        self.assertEqual(all_stats["n"], 7)
        self.assertAlmostEqual(all_stats["expectancy"], 0.05, places=12)
        # T1: R=[1.9,-0.55,-0.2,-1.6] → E=-0.1125, 有 R 4 笔、无 R 1 笔
        t1 = next(g for g in scope["groups"] if g["key"] == "aaa11111_glm")
        self.assertEqual(t1["closed"], 5)
        self.assertAlmostEqual(t1["stats"]["expectancy"], -0.1125, places=12)

    def test_appendix_scope_includes_sync(self):
        rows = lk.load_closed(self.con, "ai_managed=1")
        labels = lk.build_labels(sorted({r["trader_id"] for r in rows}))
        scope = lk.analyze_scope(lk.enrich(rows, labels), "test")
        self.assertEqual(scope["closed"], 9)
        self.assertEqual(scope["with_r"], 8)

    def test_monthly_expectancy(self):
        rows = lk.load_closed(self.con, "source='system'")
        labels = lk.build_labels(sorted({r["trader_id"] for r in rows}))
        scope = lk.analyze_scope(lk.enrich(rows, labels), "test")
        months = {m["month"]: m for m in scope["monthly"]}
        # 2026-01: R=[1.9,-0.55,-0.2,-1.6] → E=-0.1125（第 5 笔 T1 无 R 不计）
        self.assertEqual(months["2026-01"]["n"], 4)
        self.assertAlmostEqual(months["2026-01"]["expectancy"], -0.1125, places=6)
        # 2026-02: R=[1.0,-0.7] → E=0.15
        self.assertEqual(months["2026-02"]["n"], 2)
        self.assertAlmostEqual(months["2026-02"]["expectancy"], 0.15, places=6)
        # 2026-03: R=[0.5]
        self.assertEqual(months["2026-03"]["n"], 1)
        self.assertAlmostEqual(months["2026-03"]["expectancy"], 0.5)

    def test_report_builds_without_db_writes(self):
        report = lk.build_report(self.con, ":memory:")
        md = lk.render_markdown(report)
        for needle in ("# nofx 实盘已平仓交易：Kelly 与样本量分析",
                       "source='system'", "realized_pnl", "Kelly f*", "n_needed",
                       "按月 E[R]"):
            self.assertIn(needle, md)
        self.assertEqual(report["scopes"]["main"]["closed"], 8)
        self.assertEqual(report["scopes"]["ai_managed"]["closed"], 9)


if __name__ == "__main__":
    unittest.main()
