"""2026-10-28 审查 A5/A6/A7 的回归测试：入场根止损、限价穿价模式、1m→5m 拼接无空洞。"""
import json
import os
import tempfile
import unittest

from scripts.backtest.backtrader_adapter import run_trade
from scripts.backtest.cli import build_parser
from scripts.backtest.exitsim import build_trade
from scripts.backtest.fetch import Fetcher
from scripts.backtest.klines import MIN_MS, load_bars
from scripts.backtest.params import FALLBACK

BASE = 1_700_000_040_000  # 分钟对齐
MIN = MIN_MS


def P(**kw):
    return dict(FALLBACK, **dict(dict(be_arm=0, lock_r=0, lock_trim=False, trim_r=0,
                                      trail=False, pd_arm=0, tp_cap=0, quick_stop_h=0), **kw))


def T(rows, side="long", kind="mkt", **signal):
    sig = dict(symbol="X", side=side, kind="limit", outcome="unfilled", t0="test", t0_ms=BASE,
               px0=99., sl0=95. if side == "long" else 105., tp0=110. if side == "long" else 90.)
    sig.update(signal)
    bars = [[BASE + i * MIN, *row] for i, row in enumerate(rows)]
    return build_trade(sig, bars, kind)


class EntryBarStopTest(unittest.TestCase):
    """A5：入场成交所在根的区间穿过 SL 时，保守按 SL 出场，不再等到下一根才挂保护单。"""

    def run_trade(self, rows, **kw):
        return run_trade(T(rows, **kw), P(), commission=0, slippage=0, limit_fill="touch")

    def test_market_entry_bar_low_through_sl_exits_at_sl_not_tp(self):
        # 复现例：市价 entry=100、SL=95、入场根 low=90 后收回、后续触达 TP。
        r = self.run_trade([(100, 100, 100, 100), (100, 102, 90, 101), (100, 112, 99, 110)])
        self.assertEqual(r["how"], "sl")
        self.assertAlmostEqual(r["r"], -1.0)
        self.assertAlmostEqual(r["net_pnl"], -5.0)  # 按 SL 价 95 结算，而非盯市 110
        self.assertFalse(r["open"])
        self.assertEqual(r["exit_ms"], BASE + 2 * MIN)  # 入场根收盘时刻

    def test_limit_entry_bar_low_through_sl_exits_at_sl(self):
        # 复现例：限价 99 成交、同根 low=90。
        r = run_trade(T([(100, 100, 100, 100), (100, 101, 90, 100), (100, 112, 99, 110)],
                        kind="lim", px0=99.), P(), commission=0, slippage=0, limit_fill="touch")
        self.assertEqual(r["entry_price"], 99)
        self.assertEqual(r["how"], "sl")
        self.assertAlmostEqual(r["r"], -1.0)

    def test_short_entry_bar_high_through_sl_exits_at_sl(self):
        r = self.run_trade([(100, 100, 100, 100), (100, 106, 99, 99), (95, 98, 89, 91)], side="short")
        self.assertEqual((r["how"], r["r"]), ("sl", -1))  # 修复前为 tp, +2

    def test_entry_bar_touching_sl_exactly_counts_as_stop(self):
        r = self.run_trade([(100, 100, 100, 100), (100, 102, 95, 101), (108, 112, 105, 110)])
        self.assertEqual((r["how"], r["r"]), ("sl", -1))

    def test_entry_bar_not_reaching_sl_keeps_tp(self):
        r = self.run_trade([(100, 100, 100, 100), (100, 102, 99, 101), (108, 112, 105, 110)])
        self.assertEqual((r["how"], r["r"]), ("tp", 2))

    def test_entry_bar_stop_pays_estimated_exit_fee_at_sl(self):
        tr = T([(100, 100, 100, 100), (100, 102, 90, 101), (100, 112, 99, 110)])
        r = run_trade(tr, P(), commission=0.0005, slippage=0, limit_fill="touch")
        self.assertAlmostEqual(r["commission"], (100 + 95) * 0.0005)  # 入场实际费用 + SL 出场估算
        self.assertAlmostEqual(r["net_pnl"], -5 - (100 + 95) * 0.0005)
        self.assertAlmostEqual(r["r"], (-5 - (100 + 95) * 0.0005) / 5)

    def test_gap_entry_beyond_sl_keeps_invalid_fill_path(self):
        # 开盘已越过 SL 走既有 invalid_fill 市价平仓路径，不与 A5 冲突。
        r = self.run_trade([(100, 100, 100, 100), (92, 93, 91, 92), (92, 100, 92, 99)])
        self.assertEqual(r["how"], "invalid_fill")


class LimitFillModeTest(unittest.TestCase):
    """A6：默认 through 要求价格穿过限价至少 1 tick；touch 保留旧行为。"""

    def test_touch_fills_on_exact_touch_but_through_does_not(self):
        rows = [(100, 100, 100, 100), (100, 101, 99, 100), (100, 104, 100, 103)]
        tr = T(rows, kind="lim", px0=99.)
        touch = run_trade(tr, P(), commission=0, slippage=0, limit_fill="touch")
        self.assertEqual(touch["entry_price"], 99)  # low 恰好触价即成交（旧行为）
        through = run_trade(tr, P(), commission=0, slippage=0, limit_fill="through")
        self.assertEqual(through["how"], "unfilled")  # 只触未穿（差 1 tick）不成交
        self.assertIsNone(through["r"])
        self.assertEqual(through["commission"], 0)

    def test_through_fills_when_crossed_and_same_native_fill_price(self):
        rows = [(100, 100, 100, 100), (100, 101, 98.5, 100), (100, 111, 99, 110)]
        tr = T(rows, kind="lim", px0=99.)
        touch = run_trade(tr, P(), commission=0, slippage=0, limit_fill="touch")
        through = run_trade(tr, P(), commission=0, slippage=0, limit_fill="through")
        self.assertEqual(through["entry_price"], 99)  # 成交价仍与原生一致
        self.assertEqual((touch["how"], through["how"]), ("tp", "tp"))
        self.assertAlmostEqual(touch["r"], through["r"])

    def test_through_also_applies_to_tp_exit(self):
        rows = [(100, 100, 100, 100), (100, 104, 99, 103), (100, 110, 99, 100)]
        touch = run_trade(T(rows), P(), commission=0, slippage=0, limit_fill="touch")
        through = run_trade(T(rows), P(), commission=0, slippage=0, limit_fill="through")
        self.assertEqual(touch["how"], "tp")  # high=110 恰好触价
        self.assertEqual(through["how"], "open")  # 未穿价不成交，持仓到期末
        self.assertTrue(through["open"])

    def test_default_is_through_and_bad_mode_rejected(self):
        rows = [(100, 100, 100, 100), (100, 101, 99, 100), (100, 104, 100, 103)]
        default = run_trade(T(rows, kind="lim", px0=99.), P(), commission=0, slippage=0)
        self.assertEqual(default["how"], "unfilled")  # 模块默认 through
        with self.assertRaises(ValueError):
            run_trade(T(rows), P(), limit_fill="nope")

    def test_cli_limit_fill_option(self):
        self.assertEqual(build_parser().parse_args(["exit"]).limit_fill, "through")
        self.assertEqual(build_parser().parse_args(["exit", "--limit-fill", "touch"]).limit_fill, "touch")
        with self.assertRaises(SystemExit):
            build_parser().parse_args(["exit", "--limit-fill", "bogus"])


class SpliceSealTest(unittest.TestCase):
    """A7：5m 起点取最后一根 1m 收盘时刻向上取整，fetch 用 1m 补满边界，load 拒绝空洞。"""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.cache = tmp.name
        os.makedirs(os.path.join(self.cache, "kl1m"))
        os.makedirs(os.path.join(self.cache, "kl5m"))

    def write(self, zone, rows):
        path = os.path.join(self.cache, zone, f"X_{BASE}.json")
        with open(path, "w") as f:
            json.dump(rows, f)
        return path

    def read(self, zone):
        with open(os.path.join(self.cache, zone, f"X_{BASE}.json")) as f:
            return json.load(f)

    def fetch(self, b1, b5):
        """写入 1m 缓存并用假 _get 执行 fetch_signal，返回 (结果, 调用列表)。

        BASE 距 5m 边界偏移 40s：BASE+6m 是边界，BASE+7m 的收盘后最近边界是 BASE+11m。
        """
        self.write("kl1m", b1)
        if b5 is not None:
            self.write("kl5m", b5)
        calls = []
        fx = Fetcher(sleep=0)

        def fake_get(symbol, interval, start, limit):
            calls.append((interval, start, limit))
            if interval == "1m" and start == BASE + 7 * MIN:
                return [[BASE + (7 + i) * MIN, 2., 2., 2., 2.] for i in range(4)]
            if interval == "5m" and start == BASE + 11 * MIN:
                return [[BASE + 11 * MIN, 3., 3., 3., 3.]]
            return []
        fx._get = fake_get
        return fx.fetch_signal(self.cache, "X", BASE), calls

    def bar(self, open_ms, px=1.):
        return [open_ms, px, px, px, px]

    def test_fetch_seals_gap_with_1m_and_starts_5m_at_boundary(self):
        b1 = [self.bar(BASE + i * MIN) for i in range(7)]  # 最后一根收盘 BASE+7m,距 5m 边界差 4m
        (f1, f5), calls = self.fetch(b1, b5=None)
        self.assertEqual((f1, f5), (0, 1))
        self.assertIn(("1m", BASE + 7 * MIN, 4), calls)       # 补抓缺口 1m
        self.assertIn(("5m", BASE + 11 * MIN, 1000), calls)   # 5m 从收盘时刻的边界开始
        sealed = self.read("kl1m")
        self.assertEqual([b[0] for b in sealed[7:]], [BASE + (7 + i) * MIN for i in range(4)])
        bars, n1m = load_bars(self.cache, "X", BASE)          # 拼接无空洞
        self.assertEqual((n1m, bars[-1][0]), (11, BASE + 11 * MIN))

    def test_fetch_skips_seal_when_1m_already_reaches_boundary(self):
        b1 = [self.bar(BASE + i * MIN) for i in range(11)]    # 收盘恰在 BASE+11m 边界
        (_, _), calls = self.fetch(b1, b5=None)
        self.assertEqual([c for c in calls if c[0] == "1m"], [])
        self.assertIn(("5m", BASE + 11 * MIN, 1000), calls)

    def test_load_bars_rejects_splice_gap(self):
        self.write("kl1m", [self.bar(BASE + i * MIN) for i in range(7)])
        self.write("kl5m", [self.bar(BASE + 11 * MIN, 3.)])   # 12:57 类场景:中间缺 4 分钟
        with self.assertRaises(ValueError) as ctx:
            load_bars(self.cache, "X", BASE)
        self.assertIn("拼接空洞", str(ctx.exception))

    def test_load_bars_accepts_boundary_aligned_splice(self):
        self.write("kl1m", [self.bar(BASE + i * MIN) for i in range(11)])
        self.write("kl5m", [self.bar(BASE + 11 * MIN, 3.), self.bar(BASE + 16 * MIN, 3.)])
        bars, n1m = load_bars(self.cache, "X", BASE)
        self.assertEqual((n1m, [b[0] for b in bars[11:]]), (11, [BASE + 11 * MIN, BASE + 16 * MIN]))

    def test_load_bars_without_5m_cache_is_unchanged(self):
        self.write("kl1m", [self.bar(BASE)])
        bars, n1m = load_bars(self.cache, "X", BASE)
        self.assertEqual((n1m, len(bars)), (1, 1))


if __name__ == "__main__":
    unittest.main()
