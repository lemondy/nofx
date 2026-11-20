"""Unit tests for the NautilusTrader + QuantStats unified backtest engine."""
from __future__ import annotations

import unittest

import numpy as np
import pandas as pd

from scripts.backtest.exitsim import build_trade
from scripts.backtest.nt_engine import (
    CarryConfig,
    compute_metrics,
    run_portfolio,
    run_signal_trade,
    run_trade_nt,
)


def _daily_frame(opens, closes, start="2025-01-01"):
    idx = pd.date_range(start, periods=len(opens), freq="1D", tz="UTC")
    opens = np.asarray(opens, dtype=float)
    closes = np.asarray(closes, dtype=float)
    highs = np.maximum(opens, closes) * 1.01
    lows = np.minimum(opens, closes) * 0.99
    return pd.DataFrame(
        {
            "open": opens,
            "high": highs,
            "low": lows,
            "close": closes,
            "volume": np.full(len(opens), 1_000_000.0),
        },
        index=idx,
    )


class TestNautilusPortfolioEngine(unittest.TestCase):
    def test_next_open_strictly_causal_and_no_final_bar_fill(self):
        # Day 0 close = 100 signal targets 100% AAPL -> fills Day 1 open = 105.
        # Day 1 close = 110, Day 2 close = 121 (gain = 121/105 - 1).
        # Target on Day 2 (final bar) should NOT execute since no Day 3 open exists.
        aapl = _daily_frame([100.0, 105.0, 110.0], [100.0, 110.0, 121.0])
        targets = pd.DataFrame(
            {"AAPL": [1.0, 0.0]},
            index=[aapl.index[0], aapl.index[2]],
        )
        res = run_portfolio(
            {"AAPL": aapl},
            targets,
            market="us",
            initial_capital=10_000.0,
            execution="next_open",
            commission=0.001,
            slippage=0.0,
        )
        # Only 1 fill at Day 1 open (105.0), final-bar 0.0 signal does not execute
        self.assertEqual(res["fill_count"], 1)
        self.assertEqual(len(res["equity"]), 3)
        self.assertAlmostEqual(res["fills"][0]["price"], 105.0, places=6)
        qty = res["fills"][0]["qty"]
        expected_qty = 10_000.0 / 105.0
        self.assertAlmostEqual(qty, expected_qty, places=4)
        comm = expected_qty * 105.0 * 0.001
        self.assertAlmostEqual(res["commission"], comm, places=4)
        expected_final_eq = 10_000.0 - comm + qty * (121.0 - 105.0)
        self.assertAlmostEqual(float(res["equity"].iloc[-1]), expected_final_eq, places=2)

        # Mutating future bar (Day 2 close) must NOT change Day 1 fill price or quantity
        aapl_mut = _daily_frame([100.0, 105.0, 110.0], [100.0, 110.0, 500.0])
        res_mut = run_portfolio(
            {"AAPL": aapl_mut},
            targets,
            market="us",
            initial_capital=10_000.0,
            execution="next_open",
            commission=0.001,
            slippage=0.0,
        )
        self.assertAlmostEqual(res_mut["fills"][0]["price"], res["fills"][0]["price"], places=6)
        self.assertAlmostEqual(res_mut["fills"][0]["qty"], res["fills"][0]["qty"], places=6)

    def test_unified_margin_long_short_and_slippage(self):
        # Day 0: Buy 50% BTC (+100->+120, +20%), Short 50% ETH (-100->-90, +10%) at Day 1 open
        btc = _daily_frame([100.0, 100.0, 100.0], [100.0, 120.0, 120.0])
        eth = _daily_frame([100.0, 100.0, 100.0], [100.0, 90.0, 90.0])
        targets = pd.DataFrame({"BTC": [0.5], "ETH": [-0.5]}, index=[btc.index[0]])

        res = run_portfolio(
            {"BTC": btc, "ETH": eth},
            targets,
            market="crypto",
            initial_capital=10_000.0,
            execution="next_open",
            commission=0.0005,
            slippage=0.001,
        )
        # Buy fills at 100 * 1.001 = 100.1; Sell fills at 100 * 0.999 = 99.9
        fills = {f["symbol"]: f for f in res["fills"]}
        self.assertAlmostEqual(fills["BTC"]["price"], 100.1, places=4)
        self.assertAlmostEqual(fills["ETH"]["price"], 99.9, places=4)
        self.assertGreater(float(res["equity"].iloc[-1]), 11_400.0)
        self.assertFalse(res["bankrupt"])

    def test_close_execution_matches_coc_truth(self):
        aapl = _daily_frame([100.0, 100.0, 110.0], [100.0, 110.0, 121.0])
        targets = pd.DataFrame({"AAPL": [1.0]}, index=[aapl.index[0]])
        res = run_portfolio(
            {"AAPL": aapl},
            targets,
            market="us",
            initial_capital=10_000.0,
            execution="close",
            commission=0.001,
            slippage=0.0,
        )
        self.assertEqual(res["fill_count"], 1)
        self.assertEqual(len(res["equity"]), 3)
        self.assertAlmostEqual(res["fills"][0]["price"], 100.0, places=6)
        # Entered 100 shares at 100.0 close on Day 0, fee = 10.0:
        # Day 0 EOD = 9,990.0; Day 1 EOD = 10,990.0; Day 2 EOD = 12,090.0
        self.assertAlmostEqual(float(res["equity"].iloc[0]), 9_990.0, places=2)
        self.assertAlmostEqual(float(res["equity"].iloc[1]), 10_990.0, places=2)
        self.assertAlmostEqual(float(res["equity"].iloc[2]), 12_090.0, places=2)
        self.assertAlmostEqual(res["metrics"]["total_return"], 0.2090, places=4)

        # Mutating future bar (Day 2 close) must NEVER change Day 0 or Day 1 close-mode equity
        aapl_mut = _daily_frame([100.0, 100.0, 110.0], [100.0, 110.0, 500.0])
        res_mut = run_portfolio(
            {"AAPL": aapl_mut},
            targets,
            market="us",
            initial_capital=10_000.0,
            execution="close",
            commission=0.001,
            slippage=0.0,
        )
        self.assertAlmostEqual(float(res_mut["equity"].iloc[0]), float(res["equity"].iloc[0]), places=6)
        self.assertAlmostEqual(float(res_mut["equity"].iloc[1]), float(res["equity"].iloc[1]), places=6)

    def test_carry_funding_module_crypto_and_us(self):
        btc = _daily_frame([100.0, 100.0, 100.0], [100.0, 100.0, 100.0])
        # Case A: Open Day 1 morning (00:00+1ns), close Day 2 morning (00:00+1ns) = exact 24h hold
        # Crosses d1 08:00, d1 16:00, d2 00:00 -> 3 settlements = $30.0
        targets_24h = pd.DataFrame({"BTC": [1.0, 0.0]}, index=[btc.index[0], btc.index[1]])
        d1 = btc.index[1].date()
        d2 = btc.index[2].date()
        carry = CarryConfig(market="crypto", funding_rates={"BTC": {d1: 0.001, d2: 0.001}}, funding_multiplier=3.0)
        res_24h = run_portfolio(
            {"BTC": btc},
            targets_24h,
            market="crypto",
            initial_capital=10_000.0,
            execution="next_open",
            commission=0.0,
            slippage=0.0,
            carry=carry,
        )
        self.assertAlmostEqual(res_24h["carry"]["funding_paid"], 30.0, places=2)
        self.assertAlmostEqual(float(res_24h["equity"].iloc[-1]), 9970.0, places=2)

        # Case B: Open Day 1 morning (00:00+1ns) and hold through Day 2 EOD (23:59:59) = 5 x 8h settlements
        # (d1 08:00, 16:00 + d2 00:00, 08:00, 16:00) -> $50.0 (and Day 1 alone already settles $20.0, not $0!)
        targets_hold = pd.DataFrame({"BTC": [1.0]}, index=[btc.index[0]])
        res_hold = run_portfolio(
            {"BTC": btc},
            targets_hold,
            market="crypto",
            initial_capital=10_000.0,
            execution="next_open",
            commission=0.0,
            slippage=0.0,
            carry=CarryConfig(market="crypto", funding_rates={"BTC": {d1: 0.001, d2: 0.001}}, funding_multiplier=3.0),
        )
        self.assertAlmostEqual(float(res_hold["equity"].iloc[1]), 9980.0, places=2)
        self.assertAlmostEqual(res_hold["carry"]["funding_paid"], 50.0, places=2)
        self.assertAlmostEqual(float(res_hold["equity"].iloc[-1]), 9950.0, places=2)

        # Case C: Multi-day data gap (Day 1 -> Day 4, skipping 2 days) settles all intermediate 8h slots
        idx_gap = pd.DatetimeIndex(["2025-01-01", "2025-01-02", "2025-01-05"], tz="UTC")
        btc_gap = pd.DataFrame(
            {"open": [100.0, 100.0, 100.0], "high": [101.0, 101.0, 101.0], "low": [99.0, 99.0, 99.0], "close": [100.0, 100.0, 100.0], "volume": [1e6, 1e6, 1e6]},
            index=idx_gap,
        )
        res_gap = run_portfolio(
            {"BTC": btc_gap},
            pd.DataFrame({"BTC": [1.0]}, index=[idx_gap[0]]),
            market="crypto",
            initial_capital=10_000.0,
            execution="next_open",
            commission=0.0,
            slippage=0.0,
            carry=CarryConfig(market="crypto", default_funding_rate=0.001, funding_multiplier=3.0),
        )
        # Jan 2 (08,16) + Jan 3 (00,08,16) + Jan 4 (00,08,16) + Jan 5 (00,08,16) = 11 settlements = $110.0
        self.assertAlmostEqual(res_gap["carry"]["funding_paid"], 110.0, places=2)

    def test_close_mode_funding_settles_before_rebalance_fills(self):
        btc = _daily_frame([100.0, 100.0], [100.0, 100.0])
        carry_cfg = dict(market="crypto", default_funding_rate=0.001, funding_multiplier=3.0)

        # Case 1: Enter 100% long at Day 0 close, exit to 0% at Day 1 close -> held 24h -> 3 slots = $30.0
        res_exit = run_portfolio(
            {"BTC": btc},
            pd.DataFrame({"BTC": [1.0, 0.0]}, index=[btc.index[0], btc.index[1]]),
            market="crypto",
            initial_capital=10_000.0,
            execution="close",
            commission=0.0,
            slippage=0.0,
            carry=CarryConfig(**carry_cfg),
        )
        self.assertAlmostEqual(res_exit["carry"]["funding_paid"], 30.0, places=2)
        self.assertEqual(list(res_exit["equity"].index), list(btc.index))
        self.assertAlmostEqual(float(res_exit["equity"].iloc[0]), 10_000.0, places=2)
        self.assertAlmostEqual(float(res_exit["equity"].iloc[1]), 9_970.0, places=2)

        # Case 2: Flat on Day 0 close, enter 100% long at Day 1 close -> held 0h over (Day 0, Day 1] -> $0.0
        res_enter = run_portfolio(
            {"BTC": btc},
            pd.DataFrame({"BTC": [0.0, 1.0]}, index=[btc.index[0], btc.index[1]]),
            market="crypto",
            initial_capital=10_000.0,
            execution="close",
            commission=0.0,
            slippage=0.0,
            carry=CarryConfig(**carry_cfg),
        )
        self.assertAlmostEqual(res_enter["carry"]["funding_paid"], 0.0, places=2)
        self.assertEqual(list(res_enter["equity"].index), list(btc.index))
        self.assertAlmostEqual(float(res_enter["equity"].iloc[0]), 10_000.0, places=2)
        self.assertAlmostEqual(float(res_enter["equity"].iloc[1]), 10_000.0, places=2)

    def test_prelisting_asset_ignored_without_corruption(self):
        # IPO lists on Day 2; prior NaNs must not trigger phantom orders or NaN equity
        spy = _daily_frame([100.0, 101.0, 102.0, 103.0], [100.0, 101.0, 102.0, 103.0])
        ipo = _daily_frame([np.nan, np.nan, 50.0, 55.0], [np.nan, np.nan, 51.0, 56.0])
        targets = pd.DataFrame(
            {"SPY": [0.5, 0.5], "IPO": [0.5, 0.5]},
            index=[spy.index[0], spy.index[2]],
        )
        res = run_portfolio(
            {"SPY": spy, "IPO": ipo},
            targets,
            market="us",
            initial_capital=10_000.0,
            execution="next_open",
            commission=0.0,
            slippage=0.0,
        )
        self.assertTrue(np.isfinite(res["equity"]).all())
        # Day 0->Day 1 only fills SPY; Day 2->Day 3 fills IPO (and minor drift on SPY if weight changed)
        self.assertIn("SPY", {f["symbol"] for f in res["fills"]})
        self.assertIn("IPO", {f["symbol"] for f in res["fills"]})
        # IPO cannot fill before Day 2 listing
        ipo_fills = [f for f in res["fills"] if f["symbol"] == "IPO"]
        self.assertEqual(len(ipo_fills), 1)
        self.assertEqual(pd.Timestamp(ipo_fills[0]["ts"]).date(), spy.index[3].date())

    def test_us_borrow_and_margin_financing_carry(self):
        # Day 0 close: target 1.5x long AAPL + 0.5x short MSFT -> fills Day 1 open, carries into Day 2.
        # On Day 2 open: short notional = $5,000 -> borrow = 5000 * 0.005 / 365;
        # borrowed cash = max(15000 - 10000, 0) = $5,000 -> financing = 5000 * 0.05 / 365.
        aapl = _daily_frame([100.0, 100.0, 100.0], [100.0, 100.0, 100.0])
        msft = _daily_frame([100.0, 100.0, 100.0], [100.0, 100.0, 100.0])
        targets = pd.DataFrame({"AAPL": [1.5], "MSFT": [-0.5]}, index=[aapl.index[0]])
        carry = CarryConfig(market="us", us_short_borrow_annual=0.005, us_margin_finance_annual=0.05)
        res = run_portfolio(
            {"AAPL": aapl, "MSFT": msft},
            targets,
            market="us",
            initial_capital=10_000.0,
            execution="next_open",
            commission=0.0,
            slippage=0.0,
            leverage=4.0,
            carry=carry,
        )
        expected_borrow = 5000.0 * 0.005 / 365.0
        expected_fin = 5000.0 * 0.05 / 365.0
        self.assertAlmostEqual(res["carry"]["borrow_paid"], expected_borrow, places=4)
        self.assertAlmostEqual(res["carry"]["financing_paid"], expected_fin, places=4)
        self.assertAlmostEqual(float(res["equity"].iloc[-1]), 10_000.0 - expected_borrow - expected_fin, places=2)

    def test_large_universe_over_100_orders_not_throttled(self):
        # 120 assets (60 long + 60 short) submitted in a single rebalance must all fill (no 100/s throttle).
        frames = {f"S{i:03d}": _daily_frame([100.0, 100.0], [100.0, 100.0]) for i in range(120)}
        idx0 = frames["S000"].index[0]
        row = {f"S{i:03d}": (0.5 / 60 if i < 60 else -0.5 / 60) for i in range(120)}
        targets = pd.DataFrame([row], index=[idx0])
        res = run_portfolio(
            frames,
            targets,
            market="us",
            initial_capital=100_000.0,
            execution="close",
            commission=0.0,
            slippage=0.0,
        )
        self.assertEqual(res["fill_count"], 120)
        buys = [f for f in res["fills"] if f["side"] == "BUY"]
        sells = [f for f in res["fills"] if f["side"] == "SELL"]
        self.assertEqual(len(buys), 60)
        self.assertEqual(len(sells), 60)

    def test_portfolio_engine_session_bit_for_bit_parity(self):
        from scripts.backtest.nt_engine import PortfolioEngineSession

        aapl = _daily_frame([100.0, 102.0, 101.0, 105.0], [100.5, 102.5, 101.5, 105.5])
        msft = _daily_frame([200.0, 198.0, 202.0, 199.0], [201.0, 199.0, 203.0, 200.0])
        frames = {"AAPL": aapl, "MSFT": msft}
        t1 = pd.DataFrame({"AAPL": [0.5], "MSFT": [-0.5]}, index=[aapl.index[0]])
        t2 = pd.DataFrame({"AAPL": [-0.5], "MSFT": [0.5]}, index=[aapl.index[0]])
        carry = CarryConfig(market="us", us_short_borrow_annual=0.005, us_margin_finance_annual=0.05)

        ref1 = run_portfolio(frames, t1, market="us", execution="close", commission=0.001, carry=carry)
        ref2 = run_portfolio(frames, t2, market="us", execution="close", commission=0.001, carry=carry)

        with PortfolioEngineSession(frames, market="us", execution="close", commission=0.001, carry=carry) as sess:
            s1 = sess.run(t1)
            s2 = sess.run(t2)
            s1_again = sess.run(t1)

        pd.testing.assert_series_equal(s1["equity"], ref1["equity"])
        pd.testing.assert_series_equal(s2["equity"], ref2["equity"])
        pd.testing.assert_series_equal(s1_again["equity"], ref1["equity"])
        self.assertAlmostEqual(s1_again["carry"]["borrow_paid"], ref1["carry"]["borrow_paid"], places=10)
        self.assertEqual(s1_again["fill_count"], ref1["fill_count"])


class TestNautilusSignalBracket(unittest.TestCase):
    def _bars(self, ohlc):
        idx = pd.date_range("2025-01-01 00:01", periods=len(ohlc), freq="1min", tz="UTC")
        return pd.DataFrame(ohlc, columns=["open", "high", "low", "close"], index=idx)

    def _base_rules(self):
        return {
            "horizon_h": 24,
            "quick_stop_h": 0,
            "be_arm": 0.0,
            "be_off": 0.0,
            "lock_r": 0.0,
            "lock_trim": False,
            "trim_r": 0.0,
            "trim_frac": 0.0,
            "tp_cap": 0.0,
            "tp_frac_trend": 1.0,
            "trail": False,
            "trail_arm": 0.0,
            "trail_atr": 2.0,
            "pd_arm": 0.0,
            "pd_give": 0.5,
            "use_tp": True,
        }

    def test_limit_through_vs_touch_conservatism(self):
        # Bar 0 submits Limit BUY @ 99.50. Bar 1 has exact low = 99.50 (touches, doesn't cross through!).
        bars = self._bars(
            [
                [100.0, 100.2, 99.8, 100.0],
                [100.0, 100.5, 99.50, 100.4],
                [100.4, 103.5, 100.3, 103.0],
            ]
        )
        t0 = int(bars.index[0].timestamp() * 1000)
        tr = {
            "symbol": "BTCUSDT",
            "side": "long",
            "kind": "lim",
            "mode": "quick",
            "t0_ms": t0,
            "start_ms": t0,
            "reference": 99.50,
            "limit_valid_min": 5,
            "sl": 98.50,
            "tp": 103.00,
            "atr_pct": 1.0,
            "tick": 0.01,
        }
        r_through = run_signal_trade(
            bars, tr, self._base_rules(), commission=0.0005, maker_fee=0.0002, slippage=0.0, limit_fill="through"
        )
        self.assertEqual(r_through["how"], "unfilled")

        r_touch = run_signal_trade(
            bars, tr, self._base_rules(), commission=0.0005, maker_fee=0.0002, slippage=0.0, limit_fill="touch"
        )
        self.assertEqual(r_touch["how"], "tp")
        self.assertAlmostEqual(r_touch["entry_price"], 99.50, places=4)
        # Entry & TP are both Limit orders -> maker_fee (0.0002 each leg)
        expected_comm = 99.50 * 0.0002 + 103.00 * 0.0002
        self.assertAlmostEqual(r_touch["commission_paid"], expected_comm, places=5)
        self.assertGreater(r_touch["r"], 3.0)

    def test_market_entry_hits_stop_loss_with_slippage(self):
        bars = self._bars(
            [
                [100.0, 100.2, 99.8, 100.0],
                [100.0, 100.1, 97.5, 97.8],
            ]
        )
        t0 = int(bars.index[0].timestamp() * 1000)
        tr = {
            "symbol": "ETHUSDT",
            "side": "long",
            "kind": "mkt",
            "mode": "quick",
            "t0_ms": t0,
            "start_ms": t0,
            "reference": 100.0,
            "limit_valid_min": 45,
            "sl": 98.0,
            "tp": 104.0,
            "atr_pct": 1.0,
        }
        res = run_signal_trade(
            bars, tr, self._base_rules(), commission=0.0005, slippage=0.001, limit_fill="through"
        )
        self.assertEqual(res["how"], "sl")
        self.assertAlmostEqual(res["entry_price"], 100.1, places=3)
        self.assertLess(res["r"], -0.9)

    def test_dual_touch_bar_enforces_sl_first_symmetrically_long_and_short(self):
        # Bar 0 signal, Bar 1 enters cleanly @ 100.0 (no SL/TP touch), Bar 2 touches BOTH SL and TP!
        bars_dual = self._bars(
            [
                [100.0, 100.0, 100.0, 100.0],
                [100.0, 100.5, 99.5, 100.0],
                [100.0, 110.0, 90.0, 100.0],
            ]
        )
        t0 = int(bars_dual.index[0].timestamp() * 1000)
        tr_long = {
            "symbol": "BTCUSDT",
            "side": "long",
            "kind": "mkt",
            "mode": "quick",
            "t0_ms": t0,
            "start_ms": t0,
            "reference": 100.0,
            "limit_valid_min": 45,
            "sl": 95.0,
            "tp": 105.0,
            "atr_pct": 1.0,
        }
        tr_short = {
            "symbol": "BTCUSDT",
            "side": "short",
            "kind": "mkt",
            "mode": "quick",
            "t0_ms": t0,
            "start_ms": t0,
            "reference": 100.0,
            "limit_valid_min": 45,
            "sl": 105.0,
            "tp": 95.0,
            "atr_pct": 1.0,
        }
        r_long = run_signal_trade(bars_dual, tr_long, self._base_rules(), commission=0.0, slippage=0.0)
        r_short = run_signal_trade(bars_dual, tr_short, self._base_rules(), commission=0.0, slippage=0.0)
        self.assertEqual(r_long["how"], "sl")
        self.assertEqual(r_short["how"], "sl")
        self.assertAlmostEqual(r_long["r"], -1.0, places=6)
        self.assertAlmostEqual(r_short["r"], -1.0, places=6)

    def test_breakeven_trailing_and_time_horizon(self):
        # Bar 0 signal, Bar 1 entry=100 & rallies to 104 (+0.8R vs sl=95 -> risk=5), arming be_off=0.1 (100.5).
        # Bar 2 pulls back low=99 -> hits modified BE StopMarket @ 100.5!
        bars_be = self._bars(
            [
                [100.0, 100.0, 100.0, 100.0],
                [100.0, 104.0, 99.0, 103.0],
                [101.0, 102.0, 99.0, 100.0],
            ]
        )
        t0 = int(bars_be.index[0].timestamp() * 1000)
        tr = {
            "symbol": "BTCUSDT",
            "side": "long",
            "kind": "mkt",
            "mode": "trend",
            "t0_ms": t0,
            "start_ms": t0,
            "reference": 100.0,
            "limit_valid_min": 45,
            "sl": 95.0,
            "tp": 115.0,
            "atr_pct": 1.0,
        }
        rules_be = dict(self._base_rules(), be_arm=0.75, be_off=0.1)
        r_be = run_signal_trade(bars_be, tr, rules_be, commission=0.0, slippage=0.0)
        self.assertEqual(r_be["how"], "be")
        self.assertAlmostEqual(r_be["r"], 0.1, places=3)

        # ATR trailing stop: atr_pct=2.0 -> atr_r=0.4, trail_atr=2.0 -> distance 0.8R.
        # Bar 1 spikes to 110 (+2.0R) -> trail moves stop to 2.0 - 0.8 = +1.2R (106.0).
        # Bar 2 drops low=105 -> hits trail StopMarket @ 106.0!
        bars_tr = self._bars(
            [
                [100.0, 100.0, 100.0, 100.0],
                [100.0, 110.0, 99.0, 108.0],
                [108.0, 108.5, 105.0, 105.5],
            ]
        )
        tr2 = dict(tr, atr_pct=2.0)
        rules_tr = dict(self._base_rules(), trail=True, trail_arm=1.5, trail_atr=2.0, use_tp=False)
        r_tr = run_signal_trade(bars_tr, tr2, rules_tr, commission=0.0, slippage=0.0)
        self.assertEqual(r_tr["how"], "trail")
        self.assertAlmostEqual(r_tr["r"], 1.2, places=3)

    def test_run_trade_nt_tuple_adapter_parity(self):
        base = 1_700_000_040_000
        min_ms = 60_000
        sig = dict(symbol="SOLUSDT", side="long", kind="market", outcome="filled", t0="t", t0_ms=base, sl0=95.0, tp0=110.0)
        # Market entry @ 102, hit TP @ 110
        tr = build_trade(sig, [[base + i * min_ms, *row] for i, row in enumerate([(100, 100, 100, 100), (102, 104, 101, 103), (103, 111, 102, 110)])], "mkt")
        r = run_trade_nt(tr, self._base_rules(), commission=0.0, slippage=0.0)
        self.assertEqual(r["engine"], "nautilus_trader")
        self.assertEqual((r["entry_price"], r["how"], r["net_pnl"]), (102.0, "tp", 8.0))
        self.assertAlmostEqual(r["r"], 8.0 / 7.0, places=4)

        # Time horizon exit (1m)
        tr_time = build_trade(sig, [[base + i * min_ms, *row] for i, row in enumerate([(100, 100, 100, 100), (100, 101, 99, 100), (102, 103, 101, 102)])], "mkt")
        r_time = run_trade_nt(tr_time, dict(self._base_rules(), horizon_h=1 / 60), commission=0.0, slippage=0.0)
        self.assertEqual((r_time["how"], r_time["net_pnl"]), ("time", 2.0))

        # Ultra-low-price asset keeps full R precision
        scale = 1e-9
        tr_low = build_trade(sig, [[base + i * min_ms, *row] for i, row in enumerate([(100, 100, 100, 100), (100, 101, 99, 100), (100, 111, 99, 110)])], "mkt")
        tr_low["bars"] = [(t, o * scale, h * scale, l * scale, c * scale, ot) for t, o, h, l, c, ot in tr_low["bars"]]
        for k in ("reference", "sl", "tp"):
            tr_low[k] *= scale
        r_low = run_trade_nt(tr_low, self._base_rules(), commission=0.0, slippage=0.0)
        self.assertAlmostEqual(r_low["r"], 2.0, places=6)


class TestQuantStatsMetrics(unittest.TestCase):
    def test_compute_metrics_analytical_curve(self):
        # Smooth upward equity with a single clean drawdown
        idx = pd.date_range("2025-01-01", periods=5, freq="1D", tz="UTC")
        eq = pd.Series([100.0, 110.0, 99.0, 108.9, 119.79], index=idx)
        m = compute_metrics(eq, periods_per_year=252)
        self.assertAlmostEqual(m["total_return"], 0.1979, places=4)
        self.assertAlmostEqual(m["max_drawdown"], -0.10, places=4)
        self.assertGreater(m["sharpe"], 0.0)


class TestNautilusCliAndAdapters(unittest.TestCase):
    def test_exitsim_and_entry_and_cli_nautilus_routing(self):
        import csv
        import json
        import os
        import tempfile
        from scripts.backtest.cli import build_parser, main
        from scripts.backtest.entry import build_rows
        from scripts.backtest.exitsim import run_variants
        from scripts.backtest.nt_engine import nt_version
        from scripts.backtest.params import FALLBACK

        base = 1_700_000_040_000
        min_ms = 60_000
        sig = dict(symbol="BTCUSDT", side="long", kind="market", outcome="filled",
                   t0="t", t0_ms=base, sl0=95.0, tp0=110.0)
        bars = [[base + i * min_ms, *row] for i, row in enumerate(
            [(100, 100, 100, 100), (100, 101, 99, 100), (100, 111, 99, 110)]
        )]
        tr = build_trade(sig, bars, "mkt")
        p = dict(FALLBACK, be_arm=0, lock_r=0, lock_trim=False, trim_r=0,
                 trail=False, pd_arm=0, tp_cap=0, quick_stop_h=0)

        with tempfile.TemporaryDirectory() as d:
            csv_path = os.path.join(d, "nt_variants.csv")
            rows_out = run_variants(
                [tr], p, [["baseline", {}]], csv_path=csv_path,
                out=lambda _: None, commission=0.0, slippage=0.0, engine="nautilus",
            )
            self.assertAlmostEqual(rows_out[0][1]["sum"], 2.0, places=4)
            with open(csv_path) as f:
                csv_rows = list(csv.DictReader(f))
            self.assertEqual(csv_rows[0]["engine"], "nautilus_trader")
            self.assertEqual(csv_rows[0]["engine_version"], nt_version())

            os.makedirs(os.path.join(d, "kl1m"))
            long_bars = [[base + i * min_ms, 100.0, 111.0 if i == 2 else 101.0, 99.0, 100.0] for i in range(12)]
            with open(os.path.join(d, "kl1m", f"BTCUSDT_{base}.json"), "w") as f:
                json.dump(long_bars, f)
            res, _ = build_rows([sig], d, [8], commission=0.0, slippage=0.0, engine="nautilus")
            self.assertEqual(res[8][0]["mkt_how"], "tp")
            self.assertAlmostEqual(res[8][0]["mkt_r"], 2.0, places=4)

        ap = build_parser()
        parsed_default = ap.parse_args(["exit"])
        self.assertEqual(parsed_default.engine, "nautilus")
        parsed = ap.parse_args(["exit", "--engine", "nautilus"])
        self.assertEqual(parsed.engine, "nautilus")
        parsed_parity = ap.parse_args(["nt-parity", "--benchmark", "us_long"])
        self.assertEqual(parsed_parity.benchmark, "us_long")


class TestPortfolioRiskOverlay(unittest.TestCase):
    def _make_df(self, rows: list[tuple[str, float, float, float, float]]) -> pd.DataFrame:
        idx = pd.DatetimeIndex([pd.Timestamp(r[0], tz="UTC") for r in rows])
        return pd.DataFrame(
            {"open": [r[1] for r in rows], "high": [r[2] for r in rows],
             "low": [r[3] for r in rows], "close": [r[4] for r in rows],
             "volume": [10_000.0] * len(rows)},
            index=idx,
        )

    def test_short_stop_loss_and_cooldown(self):
        from scripts.backtest.nt_engine import PortfolioRiskOverlay, run_portfolio

        # Day 0: signal short A (-0.5) & long B (+0.5)
        # Day 1: opens @ 100, then A spikes to high=130 -> 20% short stop triggers @ 120!
        # Day 3: rebalance signals short A (-0.5) again, but 10d cooldown blocks A!
        rows_a = [
            ("2025-01-01", 100, 101, 99, 100),
            ("2025-01-02", 100, 130, 99, 128),
            ("2025-01-03", 128, 130, 125, 128),
            ("2025-01-04", 128, 130, 125, 128),
            ("2025-01-05", 128, 160, 125, 155),
            ("2025-01-06", 155, 156, 154, 155),
        ]
        rows_b = [
            ("2025-01-01", 100, 101, 99, 100),
            ("2025-01-02", 100, 101, 99, 100),
            ("2025-01-03", 100, 101, 99, 100),
            ("2025-01-04", 100, 101, 99, 100),
            ("2025-01-05", 100, 101, 99, 100),
            ("2025-01-06", 100, 101, 99, 100),
        ]
        frames = {"A": self._make_df(rows_a), "B": self._make_df(rows_b)}
        tgt = pd.DataFrame(
            {"A": [-0.5, -0.5], "B": [0.5, 0.5]},
            index=pd.DatetimeIndex([pd.Timestamp("2025-01-01", tz="UTC"), pd.Timestamp("2025-01-04", tz="UTC")]),
        )
        overlay = PortfolioRiskOverlay(
            long_stop_pct=0.20,
            short_stop_pct=0.20,
            stop_cooldown_days=10,
            cooldown_redistribute=False,
        )
        res = run_portfolio(frames, tgt, market="crypto", commission=0.0, slippage=0.0, risk_overlay=overlay)
        self.assertGreaterEqual(res["risk_stats"]["stop_loss_count"], 1)
        self.assertGreaterEqual(res["risk_stats"]["cooldown_block_count"], 1)
        # Because A was blocked on 2025-01-05, the spike from 128 -> 155 must NOT cause another stop-loss.
        self.assertEqual(res["risk_stats"]["stop_loss_count"], 1)

    def test_drawdown_scale_and_rolling_exit(self):
        from scripts.backtest.nt_engine import PortfolioRiskOverlay, run_portfolio

        rows_a = [
            ("2025-01-01", 100, 101, 99, 100),
            ("2025-01-02", 100, 101, 99, 100),
            ("2025-01-03", 100, 101, 99, 100),
            ("2025-01-04", 100, 101, 99, 100),
            ("2025-01-05", 100, 101, 99, 100),
        ]
        rows_b = [
            ("2025-01-01", 100, 101, 99, 100),
            ("2025-01-02", 100, 101, 99, 100),
            ("2025-01-03", 100, 105, 99, 104),
            ("2025-01-04", 104, 110, 103, 108),
            ("2025-01-05", 108, 110, 107, 109),
        ]
        frames = {"A": self._make_df(rows_a), "B": self._make_df(rows_b)}
        # Scheduled rebalance only on Jan 1 (hold A long +0.5)
        tgt = pd.DataFrame({"A": [0.5], "B": [0.0]}, index=pd.DatetimeIndex([pd.Timestamp("2025-01-01", tz="UTC")]))
        # On Jan 2 (intermediate day), A decays to pct=0.20 (<0.50), B is new top (weight=0.60).
        d2 = pd.Timestamp("2025-01-02").date()
        overlay = PortfolioRiskOverlay(
            horizon=3,
            rolling_exit=True,
            daily_pct={d2: {"A": 0.20, "B": 0.95}},
            daily_targets={d2: {"B": 0.60}},
        )
        res = run_portfolio(frames, tgt, market="crypto", commission=0.0, slippage=0.0, risk_overlay=overlay)
        self.assertEqual(res["risk_stats"]["rolling_exit_count"], 1)
        # B rallied from 100 -> 109 after rolling replacement on Jan 3 open!
        self.assertGreater(res["equity"].iloc[-1], 100_000.0)


if __name__ == "__main__":
    unittest.main()
