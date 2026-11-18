"""Unified open-source backtest engine & metrics powered by NautilusTrader + QuantStats.

This module replaces legacy custom broker monkey-patches (``BackBroker._try_exec_*``,
``CarryBroker``, dual-``Cerebro`` stitching) and duplicated performance metric formulas
with native open-source primitives:

- ``nautilus_trader.backtest.engine.BacktestEngine`` (``AccountType.MARGIN``,
  ``OmsType.NETTING``, ``BookType.L1_MBP``)
- ``nautilus_trader.backtest.models.MakerTakerFeeModel`` & ``FillModel``
- ``nautilus_trader.backtest.modules.SimulationModule`` (overnight funding / borrow carry)
- ``nautilus_trader.analysis.PortfolioAnalyzer`` + ``quantstats.stats``
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from decimal import Decimal
from importlib.metadata import PackageNotFoundError, version as pkg_version
import math
from typing import Any, Callable, Iterable

import numpy as np
import pandas as pd

from nautilus_trader.backtest.config import SimulationModuleConfig
from nautilus_trader.backtest.engine import BacktestEngine, BacktestEngineConfig
from nautilus_trader.backtest.models import FillModel, MakerTakerFeeModel
from nautilus_trader.backtest.modules import SimulationModule
from nautilus_trader.config import LoggingConfig
from nautilus_trader.model.book import OrderBook
from nautilus_trader.model.currencies import USD, USDT
from nautilus_trader.model.data import Bar, BarType, BookOrder, QuoteTick
from nautilus_trader.model.enums import (
    AccountType,
    BookType,
    CurrencyType,
    OmsType,
    OrderSide,
    OrderType,
    TimeInForce,
)
from nautilus_trader.model.identifiers import InstrumentId, Symbol, TraderId, Venue
from nautilus_trader.model.instruments import CryptoPerpetual, Equity, Instrument
from nautilus_trader.model.objects import FIXED_PRECISION, Currency, Money, Price, Quantity
from nautilus_trader.persistence.wranglers import BarDataWrangler, QuoteTickDataWrangler
from nautilus_trader.trading.strategy import Strategy

DEFAULT_CAPITAL = 100_000.0
DEFAULT_SIZE_PRECISION = 6
DEFAULT_SIZE_INCREMENT = "0.000001"
LIMIT_FILL_DEFAULT = "through"


def nt_version() -> str:
    """Return installed NautilusTrader package version string."""
    try:
        return pkg_version("nautilus_trader")
    except PackageNotFoundError:
        import nautilus_trader

        return str(getattr(nautilus_trader, "__version__", "unknown"))


def _quantstats_stats():
    # Lazy import keeps QuantStats' matplotlib/seaborn overhead off CLI startup until needed.
    import quantstats.stats as qs_stats

    return qs_stats


def _infer_price_precision(values: Iterable[float]) -> int:
    arr = np.asarray(list(values), dtype=float)
    arr = arr[np.isfinite(arr) & (arr > 0)]
    if len(arr) == 0:
        return 4
    low = float(np.min(arr))
    if low < 0.001:
        return min(FIXED_PRECISION, max(8, int(math.ceil(-math.log10(low))) + 5))
    if low < 0.1:
        return 6
    if low < 10.0:
        return 5
    return 4


def make_instrument(
    symbol: str,
    venue: Venue,
    *,
    market: str = "crypto",
    price_precision: int = 4,
    size_precision: int = DEFAULT_SIZE_PRECISION,
    maker_fee: float = 0.0005,
    taker_fee: float = 0.0005,
    fractional: bool = True,
    price_tick: float | None = None,
) -> Instrument:
    """Create a NautilusTrader Instrument supporting both US equities and crypto perpetuals."""
    settlement = USDT if market == "crypto" else USD
    if price_tick is not None and price_tick > 0:
        tick_str = f"{price_tick:.{price_precision}f}".rstrip("0").rstrip(".")
        if "." not in tick_str:
            price_precision = max(price_precision, 1)
            tick_str = f"{price_tick:.{price_precision}f}"
        else:
            price_precision = max(price_precision, len(tick_str.split(".")[1]))
        price_inc = Price.from_str(f"{price_tick:.{price_precision}f}")
    else:
        step = 10.0 ** (-price_precision)
        price_inc = Price.from_str(f"{step:.{price_precision}f}")

    inst_id = InstrumentId(Symbol(symbol), venue)
    if market == "us" and not fractional:
        return Equity(
            instrument_id=inst_id,
            raw_symbol=Symbol(symbol),
            currency=USD,
            price_precision=price_precision,
            price_increment=price_inc,
            lot_size=Quantity.from_str("1"),
            ts_event=0,
            ts_init=0,
            maker_fee=Decimal(str(maker_fee)),
            taker_fee=Decimal(str(taker_fee)),
        )

    base_code = "".join(ch for ch in symbol.upper() if ch.isalnum())[:12] or "ASSET"
    base_ccy = Currency(
        code=base_code,
        precision=size_precision,
        iso4217=0,
        name=symbol,
        currency_type=CurrencyType.CRYPTO,
    )
    size_step = 10.0 ** (-size_precision)
    size_inc = Quantity.from_str(f"{size_step:.{size_precision}f}")
    return CryptoPerpetual(
        instrument_id=inst_id,
        raw_symbol=Symbol(symbol),
        base_currency=base_ccy,
        quote_currency=settlement,
        settlement_currency=settlement,
        is_inverse=False,
        price_precision=price_precision,
        size_precision=size_precision,
        price_increment=price_inc,
        size_increment=size_inc,
        ts_event=0,
        ts_init=0,
        maker_fee=Decimal(str(maker_fee)),
        taker_fee=Decimal(str(taker_fee)),
    )


class PctSlippageFillModel(FillModel):
    """Official FillModel override adding deterministic percentage slippage on Market/Stop orders
    and configuring Limit fill conservatism (``prob_fill_on_limit=0.0`` => strict through-fill).
    """

    def __init__(self, slippage_pct: float = 0.0, prob_fill_on_limit: float = 0.0) -> None:
        super().__init__(prob_fill_on_limit=prob_fill_on_limit, prob_slippage=0.0)
        self.slippage_pct = float(slippage_pct)

    def get_orderbook_for_fill_simulation(self, instrument, order, best_bid, best_ask):
        if self.slippage_pct <= 0.0:
            return None
        if order.order_type not in (OrderType.MARKET, OrderType.STOP_MARKET):
            return None
        book = OrderBook(instrument_id=instrument.id, book_type=BookType.L2_MBP)
        qty = Quantity(1_000_000, instrument.size_precision)
        bid_px = instrument.make_price(max(best_bid.as_double() * (1.0 - self.slippage_pct), 1e-8))
        ask_px = instrument.make_price(max(best_ask.as_double() * (1.0 + self.slippage_pct), 1e-8))
        book.add(BookOrder(OrderSide.BUY, bid_px, qty, 1), 0, 0)
        book.add(BookOrder(OrderSide.SELL, ask_px, qty, 2), 0, 0)
        return book


@dataclass
class CarryConfig:
    """Daily carry / funding / borrow schedule settled via Nautilus SimulationModule."""

    market: str = "crypto"  # 'crypto' | 'us' | 'none'
    funding_rates: dict[str, dict[Any, float]] = field(default_factory=dict)
    default_funding_rate: float = 0.0  # fallback per-8h funding rate when symbol/date is missing
    funding_multiplier: float = 3.0  # 3 x 8h settlements per UTC day for crypto perps
    us_short_borrow_annual: float = 0.005
    us_margin_finance_annual: float = 0.05


EIGHT_HOURS_NS = 8 * 3_600 * 1_000_000_000
ONE_DAY_NS = 24 * 3_600 * 1_000_000_000


class CarryFundingModule(SimulationModule):
    """Native NautilusTrader SimulationModule for 8h crypto funding and US overnight carry."""

    def __init__(self, carry: CarryConfig, currency: Currency) -> None:
        super().__init__(SimulationModuleConfig())
        self.carry = carry
        self.currency = currency
        self._last_ts_ns: int | None = None
        self.total_carry_paid = 0.0
        self.funding_paid = 0.0
        self.borrow_paid = 0.0
        self.financing_paid = 0.0
        self.observations = 0
        self.covered = 0

    def process(self, ts_now: int) -> None:
        if self.carry.market == "none":
            return
        if self._last_ts_ns is None:
            self._last_ts_ns = ts_now
            return
        if ts_now <= self._last_ts_ns:
            return

        step_ns = EIGHT_HOURS_NS if self.carry.market == "crypto" else ONE_DAY_NS
        first_slot = ((self._last_ts_ns // step_ns) + 1) * step_ns
        if first_slot > ts_now:
            self._last_ts_ns = ts_now
            return
        self._last_ts_ns = ts_now

        positions = self.exchange.cache.positions_open()
        if not positions:
            return

        account = self.exchange.get_account()
        equity = account.balance_total(self.currency).as_double() if account else 0.0
        signed_notionals: dict[str, float] = {}
        for pos in positions:
            sym = pos.instrument_id.symbol.value
            book = self.exchange.get_book(pos.instrument_id)
            px = None
            if book is not None:
                mid = book.midpoint()
                px = float(mid) if mid is not None else None
                if px is None:
                    bid = book.best_bid_price()
                    px = float(bid) if bid is not None else None
            if px is None or px <= 0:
                continue
            signed_qty = pos.signed_qty
            val = float(signed_qty) * px
            signed_notionals[sym] = signed_notionals.get(sym, 0.0) + val
            upnl = pos.unrealized_pnl(Price(px, self.exchange.instruments[pos.instrument_id].price_precision))
            equity += upnl.as_double()

        slots = range(first_slot, ts_now + 1, step_ns)
        cost = 0.0
        if self.carry.market == "crypto":
            per_slot_mult = self.carry.funding_multiplier / 3.0
            for slot_ns in slots:
                slot_ts = pd.Timestamp(slot_ns, tz="UTC")
                slot_day = slot_ts.date()
                for sym, val in signed_notionals.items():
                    if val == 0.0:
                        continue
                    rates = self.carry.funding_rates.get(sym, {})
                    self.observations += 1
                    if slot_ts in rates:
                        rate = float(rates[slot_ts])
                        self.covered += 1
                    elif slot_day in rates:
                        rate = float(rates[slot_day])
                        self.covered += 1
                    else:
                        rate = float(self.carry.default_funding_rate)
                    fee = val * rate * per_slot_mult
                    cost += fee
                    self.funding_paid += fee
        elif self.carry.market == "us":
            elapsed = len(slots)
            long_notional = 0.0
            for sym, val in signed_notionals.items():
                if val < 0.0:
                    borrow = (-val) * self.carry.us_short_borrow_annual * elapsed / 365.0
                    cost += borrow
                    self.borrow_paid += borrow
                elif val > 0.0:
                    long_notional += val
            borrowed_cash = max(long_notional - max(equity, 0.0), 0.0)
            fin = borrowed_cash * self.carry.us_margin_finance_annual * elapsed / 365.0
            cost += fin
            self.financing_paid += fin

        if cost != 0.0 and math.isfinite(cost):
            self.total_carry_paid += cost
            self.exchange.adjust_account(Money(-cost, self.currency))

    def log_diagnostics(self, logger) -> None:
        logger.info(
            f"CarryFundingModule total={self.total_carry_paid:.2f} "
            f"funding={self.funding_paid:.2f} borrow={self.borrow_paid:.2f} "
            f"financing={self.financing_paid:.2f}"
        )

    def reset(self) -> None:
        self._last_ts_ns = None
        self.total_carry_paid = 0.0
        self.funding_paid = 0.0
        self.borrow_paid = 0.0
        self.financing_paid = 0.0
        self.observations = 0
        self.covered = 0


def compute_metrics(
    equity: pd.Series,
    *,
    periods_per_year: int = 252,
    rf: float = 0.0,
    initial_capital: float | None = None,
) -> dict[str, float]:
    """Compute standardized portfolio risk/return statistics using QuantStats."""
    eq = pd.Series(equity, dtype=float).dropna()
    has_cap = initial_capital is not None and math.isfinite(float(initial_capital)) and float(initial_capital) > 0.0
    base_cap = float(initial_capital) if has_cap else (float(eq.iloc[0]) if len(eq) > 0 else 0.0)
    if len(eq) < 2:
        tot = float(eq.iloc[-1] / base_cap - 1.0) if (len(eq) == 1 and base_cap > 0) else 0.0
        return {
            "total_return": tot,
            "cagr": 0.0,
            "sharpe": 0.0,
            "sortino": 0.0,
            "calmar": 0.0,
            "max_drawdown": 0.0,
            "volatility": 0.0,
            "win_rate": 0.0,
            "periods": int(len(eq)),
        }
    if not isinstance(eq.index, pd.DatetimeIndex):
        eq.index = pd.to_datetime(eq.index, utc=True)

    import warnings

    qs = _quantstats_stats()
    eq_stats = eq
    if has_cap and abs(float(eq.iloc[0]) - base_cap) > 1e-9:
        step = (eq.index[1] - eq.index[0]) if len(eq) >= 2 else pd.Timedelta(days=1)
        if step <= pd.Timedelta(0):
            step = pd.Timedelta(days=1)
        anchor = pd.Series([base_cap], index=pd.DatetimeIndex([eq.index[0] - step]), dtype=float)
        eq_stats = pd.concat([anchor, eq])

    returns = eq_stats.pct_change().dropna()
    total_ret = float(eq.iloc[-1] / base_cap - 1.0) if base_cap > 0 else 0.0
    if returns.empty or (returns == 0).all():
        return {
            "total_return": total_ret,
            "cagr": 0.0,
            "sharpe": 0.0,
            "sortino": 0.0,
            "calmar": 0.0,
            "max_drawdown": 0.0,
            "volatility": 0.0,
            "win_rate": 0.0,
            "periods": int(len(eq)),
        }

    def _safe(fn, *args, default=0.0, **kwargs) -> float:
        try:
            with warnings.catch_warnings():
                warnings.simplefilter("ignore", RuntimeWarning)
                val = float(fn(*args, **kwargs))
            return val if math.isfinite(val) else float(default)
        except Exception:
            return float(default)

    mdd = _safe(qs.max_drawdown, eq_stats, default=0.0)
    cagr_val = _safe(qs.cagr, returns, rf=rf, periods=periods_per_year, default=0.0)
    sharpe_val = _safe(qs.sharpe, returns, rf=rf, periods=periods_per_year, default=0.0)
    sortino_val = _safe(qs.sortino, returns, rf=rf, periods=periods_per_year, default=0.0)
    calmar_val = _safe(qs.calmar, returns, periods=periods_per_year, default=0.0)
    vol_val = _safe(qs.volatility, returns, periods=periods_per_year, default=0.0)
    win_val = _safe(qs.win_rate, returns, default=0.0)

    return {
        "total_return": total_ret,
        "cagr": cagr_val,
        "sharpe": sharpe_val,
        "sortino": sortino_val,
        "calmar": calmar_val,
        "max_drawdown": mdd,
        "volatility": vol_val,
        "win_rate": win_val,
        "periods": int(len(eq)),
    }


def _to_utc_index(idx: pd.Index) -> pd.DatetimeIndex:
    dt_idx = pd.DatetimeIndex(pd.to_datetime(idx))
    if dt_idx.tz is None:
        return dt_idx.tz_localize("UTC")
    return dt_idx.tz_convert("UTC")


class _PortfolioTargetStrategy(Strategy):
    """Cross-sectional target-weight portfolio strategy running inside NautilusTrader."""

    def __init__(
        self,
        *,
        venue: Venue,
        currency: Currency,
        instruments: dict[str, Instrument],
        bar_types: dict[str, BarType],
        targets_by_ns: dict[int, dict[str, float]],
        expected_bars_by_ns: dict[int, int],
        expected_quotes_by_ns: dict[int, int],
        close_mark_quotes_ns: set[int],
        execution: str,
        initial_capital: float,
        close_mark_bar_ns: dict[int, int] | None = None,
    ) -> None:
        super().__init__()
        self.venue = venue
        self.currency = currency
        self.instruments = instruments
        self.bar_types = bar_types
        self.targets_by_ns = targets_by_ns
        self.expected_bars_by_ns = expected_bars_by_ns
        self.expected_quotes_by_ns = expected_quotes_by_ns
        self._close_mark_quotes_ns = close_mark_quotes_ns
        self._close_mark_bar_ns = close_mark_bar_ns or {}
        self.execution = execution
        self.initial_capital = float(initial_capital)

        self._bar_counts: dict[int, int] = {}
        self._quote_counts: dict[int, int] = {}
        self._last_close: dict[str, float] = {}
        self._last_open: dict[str, float] = {}
        self._pending_targets: dict[str, float] | None = None
        self._pending_close_targets: dict[str, float] | None = None

        self.equity_curve: list[tuple[pd.Timestamp, float]] = []
        self.fills_log: list[dict[str, Any]] = []
        self.commission_total = 0.0
        self.turnover_total = 0.0
        self.bankrupt = False

    def on_start(self) -> None:
        for sym, bt in self.bar_types.items():
            self.subscribe_bars(bt)
            self.subscribe_quote_ticks(self.instruments[sym].id)

    def _current_equity(self) -> float:
        account = self.portfolio.account(self.venue)
        if account is None:
            return self.initial_capital
        total_bal = account.balance_total(self.currency)
        realized = total_bal.as_double() if total_bal is not None else self.initial_capital
        upnl_money = self.portfolio.unrealized_pnls(self.venue).get(self.currency)
        unrealized = upnl_money.as_double() if upnl_money is not None else 0.0
        return realized + unrealized

    def _record_equity(self, ts_ns: int) -> None:
        eq = self._current_equity()
        if eq <= 0 or not math.isfinite(eq):
            self.bankrupt = True
        self.equity_curve.append((pd.Timestamp(ts_ns, tz="UTC"), float(eq)))

    def _execute_weights(self, target_weights: dict[str, float], use_open_px: bool) -> None:
        if self.bankrupt:
            return
        equity = self._current_equity()
        if equity <= 0 or not math.isfinite(equity):
            self.bankrupt = True
            target_weights = {}

        # Include all currently held symbols so zeroed positions are liquidated.
        held_syms = {
            sym
            for sym, inst in self.instruments.items()
            if abs(float(self.portfolio.net_position(inst.id))) > 0.0
        }
        all_syms = sorted(set(target_weights) | held_syms)
        if not all_syms:
            return

        orders_to_submit: list[tuple[float, OrderSide, Instrument, float]] = []
        for sym in all_syms:
            inst = self.instruments.get(sym)
            if inst is None:
                continue
            px = self._last_open.get(sym) if use_open_px else self._last_close.get(sym)
            if px is None or px <= 0 or not math.isfinite(px):
                continue

            cur_qty = float(self.portfolio.net_position(inst.id))
            tgt_w = float(target_weights.get(sym, 0.0)) if not self.bankrupt else 0.0
            if not math.isfinite(tgt_w):
                tgt_w = 0.0
            tgt_qty = (tgt_w * equity) / px
            delta = tgt_qty - cur_qty

            step = float(inst.size_increment.as_double())
            if abs(delta) < max(step * 0.5, 1e-8):
                continue

            side = OrderSide.BUY if delta > 0 else OrderSide.SELL
            # Reductions & shorts submit before buys so margin/cash frees up cleanly.
            notional_change = delta * px
            orders_to_submit.append((notional_change, side, inst, abs(delta)))

        # Sort ascending by signed notional change -> closes/sells before buys.
        orders_to_submit.sort(key=lambda item: (item[0], item[2].id.symbol.value))
        for _, side, inst, abs_qty in orders_to_submit:
            qty_obj = inst.make_qty(abs_qty)
            if qty_obj.as_double() <= 0:
                continue
            order = self.order_factory.market(
                instrument_id=inst.id,
                order_side=side,
                quantity=qty_obj,
                time_in_force=TimeInForce.IOC,
            )
            self.submit_order(order)

    def on_quote_tick(self, tick: QuoteTick) -> None:
        sym = tick.instrument_id.symbol.value
        ts = tick.ts_init
        cnt = self._quote_counts.get(ts, 0) + 1
        self._quote_counts[ts] = cnt
        expected = self.expected_quotes_by_ns.get(ts)

        # In next_open mode, we have morning QuoteTicks (open_ts, 00:00:00)
        # and evening closing mark QuoteTicks (close_mark_ts, 23:59:59).
        # In close mode, we have post-exec closing mark QuoteTicks at ts + 2ns.
        if ts in self._close_mark_quotes_ns:
            if cnt == expected:
                mark_ns = self._close_mark_bar_ns.get(ts, ts)
                self._record_equity(mark_ns)
        else:
            self._last_open[sym] = 0.5 * (tick.bid_price.as_double() + tick.ask_price.as_double())
            if cnt == expected and self._pending_targets is not None:
                # Delay by 1ns so CarryFundingModule.process(open_ts) settles overnight positions first.
                self.clock.set_time_alert_ns(
                    f"open-exec-{ts}",
                    ts + 1,
                    self._on_open_exec,
                )

    def _on_open_exec(self, event) -> None:
        if self._pending_targets is not None:
            targets = self._pending_targets
            self._pending_targets = None
            self._execute_weights(targets, use_open_px=True)

    def _on_close_exec(self, event) -> None:
        if self._pending_close_targets is not None:
            targets = self._pending_close_targets
            self._pending_close_targets = None
            self._execute_weights(targets, use_open_px=False)

    def on_bar(self, bar: Bar) -> None:
        sym = bar.bar_type.instrument_id.symbol.value
        self._last_close[sym] = bar.close.as_double()
        ts = bar.ts_init
        cnt = self._bar_counts.get(ts, 0) + 1
        self._bar_counts[ts] = cnt
        if cnt != self.expected_bars_by_ns.get(ts):
            return

        if self.execution == "close":
            # Delay close rebalance by 1ns so CarryFundingModule.process(ts) settles
            # existing positions held over (prev_ts, ts] BEFORE close fills mutate holdings;
            # closing QuoteTicks at ts + 2ns then advance the clock and record close equity.
            if ts in self.targets_by_ns:
                self._pending_close_targets = self.targets_by_ns[ts]
                self.clock.set_time_alert_ns(
                    f"close-exec-{ts}",
                    ts + 1,
                    self._on_close_exec,
                )
        else:
            if ts in self.targets_by_ns:
                self._pending_targets = self.targets_by_ns[ts]

    def on_stop(self) -> None:
        pass

    def on_order_filled(self, event) -> None:
        comm = event.commission.as_double() if event.commission is not None else 0.0
        notional = event.last_px.as_double() * event.last_qty.as_double()
        self.commission_total += comm
        self.turnover_total += notional
        self.fills_log.append(
            {
                "symbol": event.instrument_id.symbol.value,
                "side": "BUY" if event.order_side == OrderSide.BUY else "SELL",
                "qty": event.last_qty.as_double(),
                "price": event.last_px.as_double(),
                "notional": notional,
                "commission": comm,
                "ts": pd.Timestamp(event.ts_event, tz="UTC").isoformat(),
            }
        )


def run_portfolio(
    frames: dict[str, pd.DataFrame],
    targets: pd.DataFrame,
    *,
    market: str = "crypto",
    initial_capital: float = DEFAULT_CAPITAL,
    execution: str = "next_open",  # 'next_open' | 'close'
    commission: float = 0.0005,
    slippage: float = 0.0,
    leverage: float = 10.0,
    carry: CarryConfig | None = None,
    fractional: bool = True,
    periods_per_year: int | None = None,
    log_level: str = "ERROR",
) -> dict[str, Any]:
    """Run a unified multi-asset Long/Short portfolio backtest inside NautilusTrader.

    Parameters
    ----------
    frames:
        ``{symbol: DataFrame[open, high, low, close, volume]}`` with UTC/date index.
    targets:
        DataFrame indexed by bar timestamp (matching ``frames`` close dates), columns = symbols,
        values = signed target portfolio weights (fraction of current equity, e.g. ``+0.10`` / ``-0.10``).
        Only rebalance rows (or every date) need to be present; omitted dates keep prior positions.
    execution:
        ``'next_open'`` submits target at bar ``t`` close and fills at bar ``t+1`` open ± slippage;
        ``'close'`` fills at bar ``t`` close ± slippage.
    carry:
        Optional ``CarryConfig`` settled via ``CarryFundingModule`` (Nautilus ``SimulationModule``).
    """
    if execution not in ("next_open", "close"):
        raise ValueError(f"Unsupported execution mode: {execution}")
    if not frames:
        raise ValueError("frames cannot be empty")
    if initial_capital <= 0 or not math.isfinite(initial_capital):
        raise ValueError("initial_capital must be positive and finite")
    if commission < 0 or slippage < 0:
        raise ValueError("commission and slippage must be non-negative")

    currency = USDT if market == "crypto" else USD
    venue_name = "BINANCE" if market == "crypto" else "EQUITY"
    venue = Venue(venue_name)
    ppy = periods_per_year or (365 if market == "crypto" else 252)

    modules: list[SimulationModule] = []
    if carry is not None and carry.market != "none":
        modules.append(CarryFundingModule(carry, currency))

    fill_model = (
        PctSlippageFillModel(slippage_pct=slippage, prob_fill_on_limit=0.0)
        if execution == "close"
        else FillModel(prob_fill_on_limit=0.0, prob_slippage=0.0)
    )

    engine = BacktestEngine(
        BacktestEngineConfig(
            trader_id=TraderId("NOFX-BT-001"),
            logging=LoggingConfig(log_level=log_level),
        )
    )
    engine.add_venue(
        venue=venue,
        oms_type=OmsType.NETTING,
        account_type=AccountType.MARGIN,
        base_currency=currency,
        starting_balances=[Money(initial_capital, currency)],
        default_leverage=Decimal(str(leverage)),
        book_type=BookType.L1_MBP,
        fill_model=fill_model,
        fee_model=MakerTakerFeeModel(),
        modules=modules or None,
        bar_execution=True,
        reject_stop_orders=False,
    )

    # In next_open mode: 00:00:00 open QuoteTick (+1ns carry/exec), 23:59:58 Bar, 23:59:59 close QuoteTick mark.
    bar_offset_ns = (23 * 3600 + 59 * 60 + 58) * 1_000_000_000 if execution == "next_open" else 0
    close_mark_offset_ns = (23 * 3600 + 59 * 60 + 59) * 1_000_000_000 if execution == "next_open" else 0

    instruments: dict[str, Instrument] = {}
    bar_types: dict[str, BarType] = {}
    all_data: list[Any] = []
    close_mark_quotes_ns: set[int] = set()
    close_mark_bar_ns: dict[int, int] = {}

    for sym in sorted(frames):
        raw = frames[sym]
        if raw.empty:
            continue
        df = raw[["open", "high", "low", "close"]].copy()
        df["volume"] = raw["volume"] if "volume" in raw.columns else 1_000_000.0
        df = df.astype(float)
        valid = (
            np.isfinite(df[["open", "high", "low", "close"]]).all(axis=1)
            & (df[["open", "high", "low", "close"]] > 0).all(axis=1)
        )
        df = df.loc[valid].copy()
        if df.empty:
            continue
        df["high"] = np.maximum(df["high"], df[["open", "close"]].max(axis=1))
        df["low"] = np.minimum(df["low"], df[["open", "close"]].min(axis=1))
        df["volume"] = np.where(np.isfinite(df["volume"]) & (df["volume"] > 0), df["volume"], 1_000_000.0)
        df.index = _to_utc_index(df.index)
        df = df.sort_index()

        prec = _infer_price_precision(df[["open", "high", "low", "close"]].to_numpy().ravel())
        inst = make_instrument(
            sym,
            venue,
            market=market,
            price_precision=prec,
            maker_fee=commission,
            taker_fee=commission,
            fractional=fractional,
        )
        engine.add_instrument(inst)
        instruments[sym] = inst

        bt = BarType.from_str(f"{sym}.{venue_name}-1-DAY-LAST-EXTERNAL")
        bar_types[sym] = bt

        bar_df = df.copy()
        if bar_offset_ns:
            bar_df.index = bar_df.index + pd.to_timedelta(bar_offset_ns, unit="ns")
        bars = BarDataWrangler(bt, inst).process(bar_df)
        all_data.extend(bars)

        if execution == "next_open":
            qdf_open = pd.DataFrame(
                {
                    "bid_price": df["open"] * (1.0 - slippage),
                    "ask_price": df["open"] * (1.0 + slippage),
                },
                index=df.index,
            )
            qdf_open.index.name = "timestamp"
            all_data.extend(QuoteTickDataWrangler(inst).process(qdf_open))

            close_idx = df.index + pd.to_timedelta(close_mark_offset_ns, unit="ns")
            close_vals = df["close"].to_numpy()
            qdf_close = pd.DataFrame(
                {
                    "bid_price": close_vals,
                    "ask_price": close_vals,
                },
                index=close_idx,
            )
            qdf_close.index.name = "timestamp"
            all_data.extend(QuoteTickDataWrangler(inst).process(qdf_close))
            close_mark_quotes_ns.update(int(t.value) for t in close_idx)
        else:
            close_idx = df.index + pd.to_timedelta(2, unit="ns")
            close_vals = df["close"].to_numpy()
            qdf_close = pd.DataFrame(
                {
                    "bid_price": close_vals,
                    "ask_price": close_vals,
                },
                index=close_idx,
            )
            qdf_close.index.name = "timestamp"
            all_data.extend(QuoteTickDataWrangler(inst).process(qdf_close))
            for bar_ts, quote_ts in zip(df.index, close_idx):
                q_ns = int(quote_ts.value)
                close_mark_quotes_ns.add(q_ns)
                close_mark_bar_ns[q_ns] = int(bar_ts.value)

    if not instruments:
        engine.dispose()
        raise ValueError("No valid instrument data found")

    # Map rebalance target timestamps to exact close timestamp (in ns).
    tgt_idx = _to_utc_index(targets.index)
    targets_by_ns: dict[int, dict[str, float]] = {}
    for row_ts, (_, row) in zip(tgt_idx, targets.iterrows()):
        close_ns = int(row_ts.value) + bar_offset_ns
        clean = {
            str(col): float(val)
            for col, val in row.items()
            if str(col) in instruments and math.isfinite(float(val))
        }
        targets_by_ns[close_ns] = clean

    expected_bars_by_ns: dict[int, int] = {}
    expected_quotes_by_ns: dict[int, int] = {}
    for d in all_data:
        if isinstance(d, Bar):
            expected_bars_by_ns[d.ts_init] = expected_bars_by_ns.get(d.ts_init, 0) + 1
        else:
            expected_quotes_by_ns[d.ts_init] = expected_quotes_by_ns.get(d.ts_init, 0) + 1

    all_data.sort(key=lambda d: d.ts_init)
    engine.add_data(all_data)

    strat = _PortfolioTargetStrategy(
        venue=venue,
        currency=currency,
        instruments=instruments,
        bar_types=bar_types,
        targets_by_ns=targets_by_ns,
        expected_bars_by_ns=expected_bars_by_ns,
        expected_quotes_by_ns=expected_quotes_by_ns,
        close_mark_quotes_ns=close_mark_quotes_ns,
        execution=execution,
        initial_capital=initial_capital,
        close_mark_bar_ns=close_mark_bar_ns,
    )
    engine.add_strategy(strat)
    engine.run()

    eq_index = pd.DatetimeIndex([t.normalize() if execution == "next_open" else t for t, _ in strat.equity_curve])
    eq_series = pd.Series([v for _, v in strat.equity_curve], index=eq_index, dtype=float)
    eq_series = eq_series[~eq_series.index.duplicated(keep="last")].sort_index()

    metrics = compute_metrics(eq_series, periods_per_year=ppy, initial_capital=initial_capital)
    analyzer = engine.portfolio.analyzer
    nt_stats = {
        "pnls": analyzer.get_performance_stats_pnls(currency=currency),
        "returns": analyzer.get_performance_stats_returns(),
        "general": analyzer.get_performance_stats_general(),
    }

    carry_stats = {}
    if modules:
        cm = modules[0]
        carry_stats = {
            "total_carry_paid": cm.total_carry_paid,
            "funding_paid": cm.funding_paid,
            "borrow_paid": cm.borrow_paid,
            "financing_paid": cm.financing_paid,
            "observations": cm.observations,
            "covered": cm.covered,
        }

    engine.dispose()

    return {
        "engine": "nautilus_trader",
        "equity": eq_series,
        "metrics": metrics,
        "nt_stats": nt_stats,
        "fills": strat.fills_log,
        "fill_count": len(strat.fills_log),
        "commission": strat.commission_total,
        "turnover": strat.turnover_total,
        "carry": carry_stats,
        "bankrupt": strat.bankrupt,
    }


class _SignalBracketStrategy(Strategy):
    """Single-signal entry + bracket OCO + dynamic trailing/breakeven exit in NautilusTrader."""

    def __init__(
        self,
        *,
        instrument: Instrument,
        bar_type: BarType,
        tr: dict[str, Any],
        rules: dict[str, Any],
        slippage: float = 0.0,
        taker_fee: float = 0.0,
        open_ns_by_close_ns: dict[int, int] | None = None,
        bars_by_open_quote_ns: dict[int, tuple[float, float, float]] | None = None,
    ) -> None:
        super().__init__()
        self.inst = instrument
        self.bar_type = bar_type
        self.tr = tr
        self.rules = rules
        self.slippage = float(slippage)
        self.taker_fee = float(taker_fee)
        self.open_ns_by_close_ns = open_ns_by_close_ns or {}
        self.bars_by_open_quote_ns = bars_by_open_quote_ns or {}

        self.sign = 1 if tr["side"] == "long" else -1
        self.mode = rules.get("force_mode") or tr["mode"]
        self.start_ns = int(tr["start_ms"]) * 1_000_000
        self.t0_ns = int(tr["t0_ms"]) * 1_000_000
        self.limit_valid_min = float(tr.get("limit_valid_min", 45.0))

        self.entry_order = None
        self.active_sl_order = None
        self.active_tp_orders: list[Any] = []
        self.done_reasons: set[str] = set()

        self._entry_requested = False
        self._pending_entry_submit = False
        self._pending_time_exit = False
        self._needs_protective_after_bar = False
        self._check_entry_bar_sl = False
        self._forced_sl_pnl: float | None = None

        self.entry_price: float | None = None
        self.entry_ts_ns: int | None = None
        self.entry_open_ns: int | None = None
        self.risk: float | None = None
        self.peak_r = 0.0
        self.stop_r = -1.0
        self.stop_source = "sl"

        self.how = "unfilled"
        self.exit_ts_ns: int | None = None
        self.closing = False
        self.commission_paid = 0.0
        self._prev_bar_ts_ns: int | None = None
        self.last_bar: Bar | None = None

    def _r_price(self, r: float) -> Price:
        assert self.entry_price is not None and self.risk is not None
        floor_px = float(self.inst.price_increment.as_double())
        raw = max(self.entry_price + self.sign * r * self.risk, floor_px)
        return self.inst.make_price(raw)

    def on_start(self) -> None:
        self.subscribe_bars(self.bar_type)
        self.subscribe_quote_ticks(self.inst.id)

    def _cancel_protective(self) -> None:
        for ord_obj in ([self.active_sl_order] + self.active_tp_orders):
            if ord_obj is not None and not ord_obj.is_closed:
                self.cancel_order(ord_obj)
        self.active_sl_order = None
        self.active_tp_orders = []

    def _place_protective(self, qty: float) -> None:
        self._cancel_protective()
        if qty <= 0:
            return
        exit_side = OrderSide.SELL if self.sign > 0 else OrderSide.BUY
        sl_order = self.order_factory.stop_market(
            instrument_id=self.inst.id,
            order_side=exit_side,
            quantity=self.inst.make_qty(qty),
            trigger_price=self._r_price(self.stop_r),
            reduce_only=True,
            tags=[self.stop_source],
        )
        self.active_sl_order = sl_order
        self.submit_order(sl_order)

        assert self.entry_price is not None and self.risk is not None
        tp_r = abs(float(self.tr["tp"]) - self.entry_price) / self.risk
        if self.rules.get("tp_cap", 0) > 0:
            tp_r = min(tp_r, float(self.rules["tp_cap"]))

        levels: list[tuple[str, float, float]] = []
        if self.rules.get("use_tp", True):
            frac = float(self.rules["tp_frac_trend"]) if (self.mode == "trend" and self.rules.get("trail")) else 1.0
            levels.append(("tp", tp_r, frac))
        if self.rules.get("lock_r", 0) > 0 and self.rules.get("lock_trim", False):
            levels.append(("lock", float(self.rules["lock_r"]), 0.5))
        if self.rules.get("trim_r", 0) > 0:
            levels.append(("trim", float(self.rules["trim_r"]), float(self.rules.get("trim_frac", 0.5))))

        rem = qty
        for reason, lvl, frac in sorted(levels, key=lambda x: x[1]):
            if reason in self.done_reasons or frac <= 0 or rem <= 0:
                continue
            part_qty = min(rem, qty * frac)
            tp_ord = self.order_factory.limit(
                instrument_id=self.inst.id,
                order_side=exit_side,
                quantity=self.inst.make_qty(part_qty),
                price=self._r_price(lvl),
                reduce_only=True,
                tags=[reason],
            )
            self.active_tp_orders.append(tp_ord)
            self.submit_order(tp_ord)
            rem -= part_qty
            if rem <= 1e-8:
                break

    def on_quote_tick(self, tick: QuoteTick) -> None:
        if self._pending_entry_submit and self.entry_order is None:
            self._pending_entry_submit = False
            side = OrderSide.BUY if self.sign > 0 else OrderSide.SELL
            qty = self.inst.make_qty(1.0)
            if self.tr["kind"] == "lim":
                exp_dt = datetime.fromtimestamp(self.t0_ns / 1e9, timezone.utc) + timedelta(
                    minutes=self.limit_valid_min
                )
                if tick.ts_init >= int(exp_dt.timestamp() * 1e9):
                    self.how = "unfilled"
                    self.stop()
                    return
                self.entry_order = self.order_factory.limit(
                    instrument_id=self.inst.id,
                    order_side=side,
                    quantity=qty,
                    price=self.inst.make_price(float(self.tr["reference"])),
                    time_in_force=TimeInForce.GTD,
                    expire_time=exp_dt,
                    tags=["entry"],
                )
            else:
                self.entry_order = self.order_factory.market(
                    instrument_id=self.inst.id,
                    order_side=side,
                    quantity=qty,
                    tags=["entry"],
                )
            self.submit_order(self.entry_order)

        if self._pending_time_exit:
            self._pending_time_exit = False
            net_pos = float(self.portfolio.net_position(self.inst.id))
            if abs(net_pos) > 1e-8 and not self.closing:
                self.closing = True
                self._cancel_protective()
                close_ord = self.order_factory.market(
                    instrument_id=self.inst.id,
                    order_side=OrderSide.SELL if net_pos > 0 else OrderSide.BUY,
                    quantity=self.inst.make_qty(abs(net_pos)),
                    reduce_only=True,
                    tags=["time"],
                )
                self.submit_order(close_ord)
                return

        # Enforce conservative SL-first tie-breaking symmetrically for Long and Short when an
        # intrabar range touches active SL (any gap-open TP at bar.open already matched on this
        # opening QuoteTick before on_quote_tick was invoked).
        if (
            not self.closing
            and self.active_sl_order is not None
            and not self.active_sl_order.is_closed
            and self.active_tp_orders
        ):
            ohl = self.bars_by_open_quote_ns.get(tick.ts_init)
            if ohl is not None:
                _, bar_high, bar_low = ohl
                sl_px = self.active_sl_order.trigger_price.as_double()
                sl_touched = (bar_low <= sl_px) if self.sign > 0 else (bar_high >= sl_px)
                if sl_touched:
                    for tp_ord in self.active_tp_orders:
                        if tp_ord is not None and not tp_ord.is_closed:
                            self.cancel_order(tp_ord)
                    self.active_tp_orders = []

    def on_bar(self, bar: Bar) -> None:
        prev_ns = self.last_bar.ts_init if self.last_bar is not None else None
        self._prev_bar_ts_ns = prev_ns
        self.last_bar = bar
        now_ns = bar.ts_init

        if not self._entry_requested:
            if now_ns < self.start_ns:
                return
            self._entry_requested = True
            self._pending_entry_submit = True
            return

        if self.entry_order is None:
            return

        net_pos = float(self.portfolio.net_position(self.inst.id))
        if abs(net_pos) <= 1e-8:
            if self.entry_order.is_closed and self.how == "unfilled":
                self.stop()
            return
        if self.closing or self.entry_price is None or self.risk is None:
            return

        if self.entry_open_ns is None:
            self.entry_open_ns = self.open_ns_by_close_ns.get(now_ns) or prev_ns or self.entry_ts_ns or now_ns

        if self._check_entry_bar_sl:
            self._check_entry_bar_sl = False
            extreme = bar.low.as_double() if self.sign > 0 else bar.high.as_double()
            sl_px = float(self.tr["sl"])
            if self.sign * (extreme - sl_px) <= 0:
                exit_px = sl_px * (1.0 - self.sign * self.slippage)
                qty_val = abs(net_pos)
                self.commission_paid += exit_px * qty_val * self.taker_fee
                self._forced_sl_pnl = self.sign * (exit_px - self.entry_price) * qty_val - self.commission_paid
                self.how = "sl"
                self.exit_ts_ns = now_ns
                self._cancel_protective()
                self.stop()
                return

        if self._needs_protective_after_bar:
            self._needs_protective_after_bar = False
            self._place_protective(abs(net_pos))

        # Favorable excursion update on closed bar
        fav = bar.high.as_double() if self.sign > 0 else bar.low.as_double()
        self.peak_r = max(self.peak_r, self.sign * (fav - self.entry_price) / self.risk)
        close_r = self.sign * (bar.close.as_double() - self.entry_price) / self.risk

        elapsed_h = (now_ns - (self.entry_open_ns or now_ns)) / 3_600_000_000_000.0
        horizon_h = float(self.rules.get("horizon_h", 0.0))
        quick_stop_h = float(self.rules.get("quick_stop_h", 0.0))
        if (horizon_h > 0 and elapsed_h >= horizon_h) or (
            self.mode == "quick" and quick_stop_h > 0 and elapsed_h >= quick_stop_h and close_r <= 0
        ):
            self._pending_time_exit = True
            return

        candidates = [(self.stop_r, self.stop_source)]
        be_arm = float(self.rules.get("be_arm", 0.0))
        lock_r = float(self.rules.get("lock_r", 0.0))
        if (be_arm > 0 and self.peak_r >= be_arm) or (lock_r > 0 and self.peak_r >= lock_r):
            candidates.append((float(self.rules.get("be_off", 0.0)), "be"))
        atr_r = float(self.tr.get("atr_pct", 0.0)) / 100.0 * self.entry_price / self.risk
        if self.rules.get("trail") and self.mode == "trend" and atr_r > 0 and self.peak_r >= float(self.rules.get("trail_arm", 0.0)):
            candidates.append((self.peak_r - float(self.rules.get("trail_atr", 2.0)) * atr_r, "trail"))
        pd_arm = float(self.rules.get("pd_arm", 0.0))
        if pd_arm > 0 and self.peak_r >= pd_arm:
            candidates.append((self.peak_r * (1.0 - float(self.rules.get("pd_give", 0.5))), "pd"))

        new_stop_r, new_source = max(candidates, key=lambda x: x[0])
        if new_stop_r > self.stop_r + 1e-12:
            self.stop_r, self.stop_source = new_stop_r, new_source
            if self.active_sl_order is not None and not self.active_sl_order.is_closed:
                self.modify_order(
                    self.active_sl_order,
                    trigger_price=self._r_price(self.stop_r),
                )
            else:
                self._place_protective(abs(net_pos))

    def on_order_filled(self, event) -> None:
        comm = event.commission.as_double() if event.commission is not None else 0.0
        self.commission_paid += comm
        if self.entry_order is not None and event.client_order_id == self.entry_order.client_order_id:
            self.entry_price = event.last_px.as_double()
            self.entry_ts_ns = event.ts_event
            self.risk = abs(self.entry_price - float(self.tr["sl"]))
            self.how = "open"
            if (
                self.risk <= 0
                or not (
                    self.sign * (self.entry_price - float(self.tr["sl"])) > 0
                    and self.sign * (float(self.tr["tp"]) - self.entry_price) > 0
                )
            ):
                self.closing = True
                close_ord = self.order_factory.market(
                    instrument_id=self.inst.id,
                    order_side=OrderSide.SELL if self.sign > 0 else OrderSide.BUY,
                    quantity=event.last_qty,
                    reduce_only=True,
                    tags=["invalid_fill"],
                )
                self.submit_order(close_ord)
            else:
                # Defer placing protective orders until the entry bar closes (so a single bar
                # cannot simultaneously trigger entry and TP), while enforcing conservative entry_bar_sl.
                self._check_entry_bar_sl = True
                self._needs_protective_after_bar = True
            return

        if self.active_sl_order is not None and event.client_order_id == self.active_sl_order.client_order_id:
            reason = self.stop_source
        else:
            order = self.cache.order(event.client_order_id)
            reason = (order.tags[0] if order and order.tags else None) or "exit"
        if reason in ("tp", "lock", "trim"):
            self.done_reasons.add(reason)
        self.exit_ts_ns = event.ts_event
        rem_pos = abs(float(self.portfolio.net_position(self.inst.id)))
        if rem_pos <= 1e-8:
            self.how = reason
            self._cancel_protective()
            self.stop()
        else:
            self._place_protective(rem_pos)

    def on_order_expired(self, event) -> None:
        if self.entry_order is not None and event.client_order_id == self.entry_order.client_order_id:
            self.how = "unfilled"
            self.stop()


def run_signal_trade(
    bars_df: pd.DataFrame,
    tr: dict[str, Any],
    rules: dict[str, Any],
    *,
    commission: float = 0.0005,
    maker_fee: float | None = None,
    slippage: float = 0.0003,
    limit_fill: str | None = None,
    initial_capital: float | None = None,
    open_ns_by_close_ns: dict[int, int] | None = None,
) -> dict[str, Any]:
    """Simulate a single signal's entry/OCO-bracket/trailing-exit lifecycle natively in NautilusTrader."""
    if not all(math.isfinite(v) and v >= 0 for v in (commission, slippage)):
        raise ValueError("commission and slippage must be non-negative finite floats")
    horizon_h = float(rules.get("horizon_h", 0.0))
    if not math.isfinite(horizon_h) or horizon_h < 0:
        raise ValueError("horizon_h must be non-negative and finite")
    for key in ("trim_frac", "tp_frac_trend"):
        if not 0.0 <= float(rules.get(key, 0.0)) <= 1.0:
            raise ValueError(f"{key} must be within [0, 1]")
    if limit_fill is None:
        limit_fill = LIMIT_FILL_DEFAULT
    if limit_fill not in ("through", "touch"):
        raise ValueError(f"Unsupported limit_fill mode: {limit_fill}")

    df = bars_df[["open", "high", "low", "close"]].astype(float).copy()
    df["volume"] = bars_df["volume"].astype(float) if "volume" in bars_df.columns else 1_000_000.0
    df.index = _to_utc_index(df.index)
    df = df[~df.index.duplicated(keep="first")].sort_index()

    # Scale sub-cent assets (e.g. meme coins / 1e-9 fixtures) into USDT Money range so 8-decimal
    # USDT settlement precision never quantizes small PnLs or tick increments; rescale outputs back.
    ref = float(tr["reference"])
    price_scale = (100.0 / ref) if (ref > 0 and ref < 0.01) else 1.0
    if price_scale != 1.0:
        df[["open", "high", "low", "close"]] = df[["open", "high", "low", "close"]] * price_scale
        tr_scaled = dict(tr)
        for k in ("reference", "sl", "tp", "tick"):
            if tr_scaled.get(k) is not None:
                tr_scaled[k] = float(tr_scaled[k]) * price_scale
    else:
        tr_scaled = tr

    cap = float(initial_capital) if initial_capital is not None else max(float(tr_scaled["reference"]) * 100.0, 100.0)
    prec = _infer_price_precision(df[["open", "high", "low", "close"]].to_numpy().ravel())
    venue = Venue("BINANCE")
    sym = str(tr_scaled.get("symbol", "ASSETUSDT"))
    m_fee = commission if maker_fee is None else float(maker_fee)
    inst = make_instrument(
        sym,
        venue,
        market="crypto",
        price_precision=max(prec, 4),
        maker_fee=m_fee,
        taker_fee=commission,
        price_tick=tr_scaled.get("tick"),
    )
    bt = BarType.from_str(f"{sym}.BINANCE-1-MINUTE-LAST-EXTERNAL")
    bars = BarDataWrangler(bt, inst).process(df)

    # Emit opening QuoteTicks (bid=ask=bar.open) at open_ts + 1ns so orders submitted at bar k
    # execute cleanly at bar k+1 open (with PctSlippageFillModel for Market and zero slip for Limit).
    close_ns_list = [int(t.value) for t in df.index]
    open_quote_ns: list[int] = []
    for i, c_ns in enumerate(close_ns_list):
        if open_ns_by_close_ns and c_ns in open_ns_by_close_ns:
            o_ns = int(open_ns_by_close_ns[c_ns]) + 1
        elif i > 0:
            o_ns = close_ns_list[i - 1] + 1
        else:
            o_ns = c_ns - 59_999_999_999
        open_quote_ns.append(o_ns)
    open_idx = pd.to_datetime(open_quote_ns, unit="ns", utc=True)
    open_vals = df["open"].to_numpy()
    high_vals = df["high"].to_numpy()
    low_vals = df["low"].to_numpy()
    bars_by_open_quote_ns = {
        int(o_ns): (float(open_vals[i]), float(high_vals[i]), float(low_vals[i]))
        for i, o_ns in enumerate(open_quote_ns)
    }
    qdf_open = pd.DataFrame({"bid_price": open_vals, "ask_price": open_vals}, index=open_idx)
    qdf_open.index.name = "timestamp"
    open_quotes = QuoteTickDataWrangler(inst).process(qdf_open)
    all_data = sorted(bars + open_quotes, key=lambda d: d.ts_init)

    prob_limit = 0.0 if limit_fill == "through" else 1.0
    fill_model = PctSlippageFillModel(slippage_pct=slippage, prob_fill_on_limit=prob_limit)

    engine = BacktestEngine(
        BacktestEngineConfig(
            trader_id=TraderId("NOFX-SIG-001"),
            logging=LoggingConfig(log_level="ERROR"),
        )
    )
    engine.add_venue(
        venue=venue,
        oms_type=OmsType.NETTING,
        account_type=AccountType.MARGIN,
        base_currency=USDT,
        starting_balances=[Money(cap, USDT)],
        default_leverage=Decimal("10"),
        book_type=BookType.L1_MBP,
        fill_model=fill_model,
        fee_model=MakerTakerFeeModel(),
        bar_execution=True,
        support_gtd_orders=True,
        support_contingent_orders=True,
        use_reduce_only=True,
    )
    engine.add_instrument(inst)
    engine.add_data(all_data)

    strat = _SignalBracketStrategy(
        instrument=inst,
        bar_type=bt,
        tr=tr_scaled,
        rules=rules,
        slippage=slippage,
        taker_fee=commission,
        open_ns_by_close_ns=open_ns_by_close_ns,
        bars_by_open_quote_ns=bars_by_open_quote_ns,
    )
    engine.add_strategy(strat)
    engine.run()

    acct = engine.portfolio.account(venue)
    end_bal = acct.balance_total(USDT).as_double() if acct else cap
    open_positions = engine.cache.positions_open(instrument_id=inst.id)
    if strat._forced_sl_pnl is not None:
        net_pnl_scaled = strat._forced_sl_pnl
        is_open = False
    elif open_positions:
        last_close = float(df["close"].iloc[-1])
        upnl = sum(float(p.signed_qty) * (last_close - float(p.avg_px_open)) for p in open_positions)
        net_pnl_scaled = (end_bal + upnl) - cap
        is_open = True
    else:
        net_pnl_scaled = end_bal - cap
        is_open = False

    r_mult = (
        (net_pnl_scaled / strat.risk)
        if (strat.risk and strat.risk > 0 and strat.entry_price is not None)
        else None
    )
    entry_ms = (
        int(strat.entry_open_ns // 1_000_000)
        if strat.entry_open_ns is not None
        else (int(strat.entry_ts_ns // 1_000_000) if strat.entry_ts_ns is not None else None)
    )
    exit_ms = int(strat.exit_ts_ns // 1_000_000) if strat.exit_ts_ns is not None else None
    coverage_end_ms = int(df.index[-1].value // 1_000_000) if not df.empty else None
    if is_open and entry_ms is not None and coverage_end_ms is not None:
        exit_ms = max(coverage_end_ms, int(entry_ms + horizon_h * 3_600_000))

    result = {
        "engine": "nautilus_trader",
        "engine_version": nt_version(),
        "how": strat.how,
        "entry_price": (strat.entry_price / price_scale) if strat.entry_price is not None else None,
        "risk": (strat.risk / price_scale) if strat.risk is not None else None,
        "net_pnl": net_pnl_scaled / price_scale,
        "r": r_mult,
        "peak": strat.peak_r,
        "commission": strat.commission_paid / price_scale,
        "commission_paid": strat.commission_paid / price_scale,
        "entry_ms": entry_ms,
        "exit_ms": exit_ms,
        "coverage_end_ms": coverage_end_ms,
        "open": is_open,
    }
    engine.dispose()
    return result


def run_trade_nt(
    tr: dict[str, Any],
    rules: dict[str, Any],
    commission: float = 0.0005,
    slippage: float = 0.0003,
    limit_fill: str | None = None,
    maker_fee: float | None = None,
) -> dict[str, Any]:
    """Adapter accepting ``exitsim.build_trade`` tuple bars ``(close_ms, o, h, l, c, open_ms)``."""
    raw_bars = tr.get("bars") or []
    if len(raw_bars) < 2:
        raise ValueError("tr['bars'] must contain at least 2 OHLC bars")
    idx = pd.to_datetime([int(b[0]) for b in raw_bars], unit="ms", utc=True)
    bars_df = pd.DataFrame(
        {
            "open": [float(b[1]) for b in raw_bars],
            "high": [float(b[2]) for b in raw_bars],
            "low": [float(b[3]) for b in raw_bars],
            "close": [float(b[4]) for b in raw_bars],
            "volume": [1_000_000.0] * len(raw_bars),
        },
        index=idx,
    )
    open_ns_by_close_ns = {int(b[0]) * 1_000_000: int(b[5]) * 1_000_000 for b in raw_bars}
    tr_norm = dict(tr)
    if "symbol" not in tr_norm:
        tr_norm["symbol"] = tr_norm.get("sym", "ASSETUSDT")
    return run_signal_trade(
        bars_df,
        tr_norm,
        rules,
        commission=commission,
        maker_fee=maker_fee,
        slippage=slippage,
        limit_fill=limit_fill,
        open_ns_by_close_ns=open_ns_by_close_ns,
    )

