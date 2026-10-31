"""Offline QuantJourney order examples, independently ported to Backtrader.

Source: QuantJourneyOrg/quantjourney-bt (Apache-2.0), examples O01--O20.
Source files are reference text only; this module never imports/executes them.
Every fill, position and trading P&L is accounted for by native BackBroker.
"""
from __future__ import annotations

import argparse
from collections import Counter, deque
from functools import lru_cache
import json
import math
from pathlib import Path
from statistics import NormalDist

import numpy as np
import pandas as pd

from .backtrader_adapter import _backtrader
from .quant_audit import audit_returns, grid_around, sharpe, sweep_plateau
from .volrix import load_frame, load_funding
from .volrix_intraday import ArrayFeed, load_bars, smooth

bt = _backtrader()
ROOT = Path('/Users/zhangyun/workspace/nofx/data')
CAPITAL, BROKER_CASH = 1000., 1.e9
COINS = ('BTC', 'ETH', 'BNB', 'XRP', 'ADA', 'DOGE', 'LTC', 'LINK', 'TRX', 'BCH')
ETFS = ('SPY', 'EFA', 'EEM', 'TLT', 'IEF', 'GLD', 'DBC', 'VNQ')
DAILY = tuple(f'O{i:02}' for i in (*range(1, 15), 17, 18, 20))
RULES = tuple(f'O{i:02}' for i in (*range(1, 19), 20))
NAMES = ('Market SMA', 'Market RSI', 'Limit RSI dip', 'Limit trend pullback',
         'Stop breakout', 'Protective stop', 'StopLimit breakout', 'StopLimit protection',
         'Trailing trend', 'Trailing RSI', 'Trailing StopLimit', 'Bracket trend',
         'Bracket RSI', 'OCO dip/breakout', '5m bracket RSI', '30m stop breakout',
         'Monthly rotation', 'Signal-change rotation', 'FX lots', 'Donchian ATR logic')
# One predeclared main parameter per rule: five points, including the default.
DEFAULTS = {r: {'slow': 50} for r in ('O01', 'O06', 'O08', 'O09', 'O11', 'O12', 'O18')}
DEFAULTS.update({r: {'rsi_period': 14} for r in ('O02', 'O03', 'O10', 'O13', 'O15')})
DEFAULTS.update(O04={'slow': 50}, O05={'lookback': 20}, O07={'lookback': 20},
                O14={'hold': 10}, O16={'lookback': 12}, O17={'lookback': 126}, O20={'lookback': 55})
FRACTIONS = dict(zip(RULES, (.18, .12, .15, .12, .10, .15, .10, .15, .15,
                                    .15, .15, .15, .15, .10, .20, .20, 1., .18, .25)))
SLIPPAGE = dict(zip(RULES, (5, 5, 3, 3, 5, 5, 4, 5, 5, 5, 5, 5, 5, 5, 2, 2, 3, 3, 1)))
PER_SHARE = {'O01', 'O05', 'O06', 'O08', 'O09', 'O12', 'O14', 'O15', 'O16', 'O17', 'O18'}


def edge_source(rule):
    return '行为（短期均值回归）' if rule in ('O02', 'O03', 'O10', 'O13', 'O15') else '行为（趋势/动量/反转）'


@lru_cache(maxsize=64)
def read_daily(symbol, market):
    if market == 'crypto':
        return load_frame(ROOT / 'factor_mining/crypto/daily' / f'{symbol}USDT.json')
    raw = json.loads((ROOT / 'quantjourney/prices' / f'{symbol}.json').read_text())
    f = pd.DataFrame(raw, columns=['date', 'open', 'high', 'low', 'close', 'adjclose', 'volume'])
    f.index = pd.to_datetime(f.pop('date'), utc=True).dt.tz_localize(None)
    f = f.astype(float)
    factor = f.adjclose / f.close
    f.loc[:, ['open', 'high', 'low', 'close']] = f[['open', 'high', 'low', 'close']].mul(factor, axis=0)
    return f.drop(columns='adjclose')


def features(frame, params):
    """Causal indicators. Rolling highs exclude t; RSI uses Wilder smoothing."""
    f = frame.copy()
    c, h, l = f.close, f.high, f.low
    f['fast'] = c.rolling(20).mean()
    f['slow'] = c.rolling(params.get('slow', 50)).mean()
    delta = c.diff().to_numpy()
    gain = smooth(np.maximum(delta, 0), params.get('rsi_period', 14), wilder=True)
    loss = smooth(np.maximum(-delta, 0), params.get('rsi_period', 14), wilder=True)
    f['rsi'] = np.divide(100*gain, gain+loss, out=np.full(len(f), 50.), where=(gain+loss) != 0)
    n = params.get('lookback', 20)
    f['prior_close_high'] = c.shift().rolling(n).max()
    f['prior_high'] = h.shift().rolling(n).max()
    f['prior_low'] = l.shift().rolling(n).min()
    f['momentum'] = c / c.shift(params.get('lookback', 126)) - 1
    tr = pd.concat([h-l, (h-c.shift()).abs(), (l-c.shift()).abs()], axis=1).max(axis=1)
    f['atr'] = tr.rolling(20).mean()
    return f


def funding_cost(size, price, rate):
    return size * price * rate * 3


class OriginalCommission(bt.CommInfoBase):
    params = (('stocklike', True), ('commtype', bt.CommInfoBase.COMM_PERC),
              ('percabs', True), ('per_share', False), ('scale', 100.))

    def _getcommission(self, size, price, pseudoexec):
        if self.p.per_share:
            return max(min(abs(size)*.005, abs(size*price)*.005), 1/self.p.scale)
        return abs(size*price)*.0001


class OrdersBroker(bt.brokers.BackBroker):
    """Small semantic adapters around BackBroker's native matching methods.

    QJ ratchets trails with this bar's high/low before testing the stop. QJ
    StopLimit uses trigger-bounded open as the native limit price reference.
    Orders expire AFTER their Nth attempted bar, never before it. Reproduction
    brackets activate next bar (QJ's sorted snapshot); evaluation also tests
    the protective stop on the entry bar, conservatively ignoring entry-bar TP.
    """
    params = (('rates', None), ('market', 'us'), ('evaluation', True))

    def __init__(self):
        super().__init__()
        self.strategy = None
        self.last_day = None
        self.funding_paid = self.borrow_paid = self.financing_paid = 0.
        self.funding_missing = self.funding_observations = 0
        self._limit_bound = None
        self.carry = Counter()

    def next(self):
        st = self.strategy
        if st is not None and len(st.data):
            day = st.data.datetime.date(0)
            if day != self.last_day:
                elapsed = (day-self.last_day).days if self.last_day else 1
                self.last_day = day
                long = short = 0.
                for d in st.datas:
                    pos = self.getposition(d).size
                    nominal = pos * float(d.open[0])
                    long += max(nominal, 0.)
                    short += max(-nominal, 0.)
                    if self.p.evaluation and self.p.market == 'crypto' and pos:
                        rates = (self.p.rates or {}).get(d._name, {})
                        self.funding_observations += 1
                        self.funding_missing += day not in rates
                        cost = funding_cost(pos, float(d.open[0]), rates.get(day, 0.))
                        self.add_cash(-cost)
                        self.funding_paid += cost
                        self.carry[d._name] += cost
                if self.p.evaluation and self.p.market == 'us':
                    equity = self.getcash()-BROKER_CASH+CAPITAL+long-short
                    borrow = short*.005*elapsed/365
                    finance = max(0., long-max(equity, 0.))*.05*elapsed/365
                    self.add_cash(-borrow-finance)
                    self.borrow_paid += borrow
                    self.financing_paid += finance
                    for d in st.datas:
                        nominal = self.getposition(d).size*float(d.open[0])
                        self.carry[d._name] += (max(-nominal, 0.)/short*borrow if short else 0.)
                        self.carry[d._name] += (max(nominal, 0.)/long*finance if long else 0.)
        # Stable priority, including QJ's two same-priority OCO entry legs.
        if self.pending:
            self.pending = deque(sorted(self.pending, key=lambda o: o.info.get('priority',
                0 if o.exectype in (bt.Order.Stop, bt.Order.StopLimit, bt.Order.StopTrail,
                                    bt.Order.StopTrailLimit) else 5 if o.exectype == bt.Order.Limit else 6)))
        super().next()

    def _slip_up(self, pmax, price, doslip=True, lim=False):
        p = price*(1+self.p.slip_perc) if doslip else price
        return min(p, self._limit_bound) if self._limit_bound is not None else p

    def _slip_down(self, pmin, price, doslip=True, lim=False):
        p = price*(1-self.p.slip_perc) if doslip else price
        return max(p, self._limit_bound) if self._limit_bound is not None else p

    def _try_exec_stoplimit(self, order, popen, phigh, plow, pclose, pcreated, plimit):
        # Price discovery stays in native _try_exec_limit / _execute; no custom
        # fills or account mutations. QJ tests full OHLC after stop activation.
        hit = phigh >= pcreated if order.isbuy() else plow <= pcreated
        if hit:
            order.triggered = True
            reference = max(popen, pcreated) if order.isbuy() else min(popen, pcreated)
            self._try_exec_limit(order, reference, phigh, plow, plimit)

    def _try_exec(self, order):
        st = self.strategy
        if st is not None:
            i = len(order.data)-1
            if not st.available[order.data._name][i]:
                return
        trailing = order.exectype in (bt.Order.StopTrail, bt.Order.StopTrailLimit)
        if trailing and not order.triggered:
            highlow = float(order.data.high[0] if order.issell() else order.data.low[0])
            if not order.info.get('trail_started', False):
                order.created.price = -math.inf if order.issell() else math.inf
                order.info['trail_started'] = True
            order.trailadjust(highlow)
        bound = None
        if order.exectype == bt.Order.Limit:
            bound = order.created.price
        elif order.exectype in (bt.Order.StopLimit, bt.Order.StopTrailLimit):
            bound = order.created.pricelimit
        self._limit_bound = bound
        try:
            super()._try_exec(order)
        finally:
            self._limit_bound = None
        if order.alive() and order.info.get('ttl'):
            order.info['live_bars'] = order.info.get('live_bars', 0)+1
            if order.info['live_bars'] >= order.info['ttl']:
                order.status = bt.Order.Expired
                self.notify(order)
                # QJ expiry affects this leg only. In particular, on the last
                # live bar an expired dip Limit must not cancel the still-
                # untested breakout Stop. Native OCO cancellation stays active
                # for actual fills and explicit cancellations.
                self._bracketize(order, cancel=True)

    def _bracketize(self, order, cancel=False):
        children = list(self._pchildren.get(order.ref, ()))[1:]
        super()._bracketize(order, cancel=cancel)
        if cancel or not self.p.evaluation or order.status != bt.Order.Completed:
            return
        for child in children:
            if child.exectype == bt.Order.Stop:
                child.activate()
                if child in self._toactivate:
                    self._toactivate.remove(child)
                if child in self.pending:
                    self.pending.remove(child)
                self._try_exec(child)
                if child.alive():
                    self.pending.appendleft(child)
                elif child.status == bt.Order.Completed:
                    super()._bracketize(child)


class OrderStrategy(bt.Strategy):
    params = (('rule', 'O01'), ('settings', None), ('arrays', None), ('available', None),
              ('dates', None), ('monthends', None), ('reproduction', False), ('scale', 100.),
              ('benchmark', False), ('exposure_end', None))

    def __init__(self):
        self.broker.strategy = self
        self.available = self.p.available
        self.orders = {d._name: [] for d in self.datas}
        self.prev, self.entry_bar, self.counter = {}, {}, Counter()
        self.protected = {}
        self.last_month = None
        self.daily, self.entries, self.closed = {}, [], []
        self.fills = []
        self.exposure_sum = self.turnover = self.commission = 0.
        self.observations = 0
        self.bankrupt = False
        self.cost_marks = {}

    def alive(self, d):
        self.orders[d._name] = [o for o in self.orders[d._name] if o.alive()]
        return bool(self.orders[d._name])

    def cancel_all(self, d):
        for o in self.orders[d._name]:
            if o.alive():
                self.cancel(o)

    def order_price(self, price):
        """QJ cent ticks for US stocks; preserve raw crypto prices/distances."""
        return round(price, 2) if self.broker.p.market == 'us' else float(price)

    def submit(self, d, size, exectype=bt.Order.Market, ttl=None, **kw):
        if abs(size) < 1e-12:
            return None
        o = (self.buy if size > 0 else self.sell)(data=d, size=abs(size), exectype=exectype, **kw)
        # Backtrader treats price=0 as an omitted reference price. Retain an
        # explicit zero, so an unreachable zero Limit cannot fill at close.
        if kw.get('price') == 0:
            o.created.price = 0.
        if kw.get('plimit') == 0:
            o.created.pricelimit = 0.
        o.addinfo(ttl=ttl, signal_time=d.datetime.datetime(0).isoformat())
        self.orders[d._name].append(o)
        return o

    def bracket(self, d, size, close, tp, sl):
        if size <= 0:
            return
        limit, stop = self.order_price(close*(1+tp)), self.order_price(close*(1-sl))
        group = self.buy_bracket(data=d, size=size, exectype=bt.Order.Market,
                                 limitprice=limit, stopprice=stop)
        group[1].created.price = stop
        group[2].created.price = limit
        for o in group:
            o.addinfo(signal_time=d.datetime.datetime(0).isoformat())
        self.orders[d._name].extend(group)

    def notify_order(self, order):
        if order.status in (order.Margin, order.Rejected):
            raise RuntimeError(f'Backtrader {self.p.rule} {order.data._name} {order.data.datetime.date(0)} '
                               f'{order.getstatusname()} size={order.size} cash={self.broker.getcash()}')
        if order.status != order.Completed:
            return
        e, s = order.executed, order.data._name
        date = bt.num2date(e.dt).date().isoformat()
        self.fills.append(dict(symbol=s, date=date, price=e.price, size=e.size,
                               signal_time=order.info.get('signal_time'), commission=e.comm))
        self.turnover += abs(e.size*e.price)/CAPITAL
        self.commission += e.comm

    def notify_trade(self, trade):
        s = trade.data._name
        if trade.justopened:
            date = bt.num2date(trade.dtopen).date().isoformat()
            self.entries.append((s, date))
        if trade.isclosed:
            direction = math.copysign(1., trade.history[0].event.size)
            nominal = sum(abs(h.event.size*h.event.price) for h in trade.history
                          if h.event.size*direction > 0)
            # Costs allocated by the broker to each instrument, including
            # funding/borrow. Portfolio financing is allocated by long notional.
            paid = self.carry_by_symbol(s)-self.cost_marks.pop(trade.ref, self.carry_by_symbol(s))
            self.closed.append(dict(date=bt.num2date(trade.dtclose).date().isoformat(),
                                    pnl=trade.pnlcomm-paid, nominal=nominal,
                                    net_bps=(trade.pnlcomm-paid)/nominal*10000 if nominal else 0.))
        elif trade.justopened:
            self.cost_marks[trade.ref] = self.carry_by_symbol(s)

    def carry_by_symbol(self, s):
        return self.broker.carry.get(s, 0.)

    def size(self, nav, fraction, price):
        v = max(0., nav)*fraction/price
        return math.floor(v*self.p.scale)/self.p.scale if self.p.reproduction else v

    def prenext(self):
        self.next()

    def next(self):
        i = len(self.data)-1
        nav = self.broker.getvalue()-BROKER_CASH+CAPITAL
        self.bankrupt |= nav <= 0
        date = self.p.dates[i]
        self.daily[date] = nav
        exposure = sum(abs(self.getposition(d).size*float(d.close[0])) for d in self.datas)
        if self.p.exposure_end is None or date <= self.p.exposure_end:
            self.exposure_sum += exposure/max(abs(nav), 1e-12)
            self.observations += 1
        rule, p = self.p.rule, self.p.settings
        if self.bankrupt:
            for d in self.datas:
                self.cancel_all(d)
                if self.getposition(d).size:
                    self.close(d)
            return
        if self.p.benchmark:
            # Known calendar month-end; first allocation on first tradable bar.
            monthend = i in self.p.monthends
            active = [d for d in self.datas if self.available[d._name][i]]
            if i == 0 or (len(self.datas) > 1 and monthend):
                for d in active:
                    target = nav/(len(active) or 1)/float(d.close[0])
                    # Reserve the known one-side costs at initial allocation.
                    target *= 1/(1+self.broker.p.slip_perc+self.broker.getcommissioninfo(d).p.commission)
                    self.submit(d, target-self.getposition(d).size)
            return
        if rule == 'O17':
            month = date[:7]
            if month == self.last_month:
                return
            self.last_month = month
            candidates = [(float(self.p.arrays[d._name]['momentum'][i]), d) for d in self.datas
                          if self.available[d._name][i] and math.isfinite(self.p.arrays[d._name]['momentum'][i])]
            target = {d._name for _, d in sorted(candidates, key=lambda x: (-x[0], x[1]._name))[:3]}
            for d in self.datas:
                pos = self.getposition(d).size
                if pos > 0 and d._name not in target:
                    self.submit(d, -pos)
            for d in self.datas:
                if d._name in target and self.getposition(d).size == 0:
                    self.submit(d, self.size(nav, 1/max(1, len(target)), float(d.close[0])))
            return
        for d in self.datas:
            s = d._name
            if not self.available[s][i]:
                continue
            a = self.p.arrays[s]
            c, pos = float(d.close[0]), self.getposition(d).size
            fast, slow, rsi = (a[k][i] for k in ('fast', 'slow', 'rsi'))
            signal = int(fast > slow)
            prev = self.prev.get(s, 0)
            pending = self.alive(d)
            q = self.size(nav, FRACTIONS[rule], c)
            if pos == 0:
                self.protected[s] = False
                if rule != 'O16':
                    self.entry_bar.pop(s, None)
            if rule in ('O01', 'O06', 'O08', 'O09', 'O11', 'O12', 'O18'):
                if not math.isfinite(slow) or not math.isfinite(fast):
                    continue
                if signal and not prev and pos == 0 and (not pending or rule != 'O01'):
                    if rule == 'O12':
                        self.bracket(d, q, c, .06, .03)
                    else:
                        self.submit(d, q)
                elif pos > 0 and rule in ('O06', 'O08', 'O09', 'O11') and not self.protected.get(s):
                    entry = self.getposition(d).price
                    if rule == 'O06':
                        self.submit(d, -pos, bt.Order.Stop, price=self.order_price(entry*.95))
                    elif rule == 'O08':
                        self.submit(d, -pos, bt.Order.StopLimit, price=self.order_price(entry*.96), plimit=self.order_price(entry*.955))
                    elif rule == 'O09':
                        self.submit(d, -pos, bt.Order.StopTrail, trailpercent=.04)
                    else:
                        amount, offset = self.order_price(c*.04), self.order_price(c*.005)
                        self.submit(d, -pos, bt.Order.StopTrailLimit, price=c, plimit=c-offset, trailamount=amount)
                    self.protected[s] = True
                elif not signal and prev and pos > 0:
                    self.cancel_all(d)
                    self.submit(d, -pos)
                self.prev[s] = signal
            elif rule in ('O02', 'O03', 'O10', 'O13', 'O15'):
                if not math.isfinite(rsi):
                    continue
                threshold = {'O02': 35, 'O03': 40, 'O10': 38, 'O13': 35, 'O15': 30}[rule]
                if pos == 0 and not pending and rsi < threshold:
                    if rule in ('O13', 'O15'):
                        tp, sl = (.04, .02) if rule == 'O13' else (.006, .004)
                        self.bracket(d, q, c, tp, sl)
                    elif rule == 'O03':
                        self.submit(d, q, bt.Order.Limit, ttl=3, price=self.order_price(c*.985))
                    else:
                        self.submit(d, q)
                elif pos > 0:
                    if rule == 'O03' and not pending:
                        self.submit(d, -pos, bt.Order.Limit, ttl=3, price=self.order_price(self.getposition(d).price*1.03))
                    elif rule == 'O10' and not self.protected.get(s):
                        self.submit(d, -pos, bt.Order.StopTrail, trailpercent=.05)
                        self.protected[s] = True
                    elif rsi > {'O02': 60, 'O03': math.inf, 'O10': 65, 'O13': 70, 'O15': math.inf}[rule]:
                        self.cancel_all(d)
                        self.submit(d, -pos)
            elif rule == 'O04':
                if not math.isfinite(slow):
                    continue
                if pos == 0 and not pending and c > slow:
                    self.submit(d, q, bt.Order.Limit, ttl=2, price=self.order_price(c*.99))
                elif pos > 0 and c < slow:
                    self.cancel_all(d)
                    self.submit(d, -pos)
                elif pos > 0 and not pending:
                    self.submit(d, -pos, bt.Order.Limit, ttl=5, price=self.order_price(self.getposition(d).price*1.04))
            elif rule in ('O05', 'O07', 'O14', 'O16'):
                n = p.get('lookback', 20 if rule != 'O16' else 12)
                if rule in ('O05', 'O07') and i < n+1:
                    continue
                if rule == 'O16' and not math.isfinite(a['prior_high'][i]):
                    continue
                self.counter[s] += 1
                count = self.counter[s]
                if pos > 0 and s not in self.entry_bar:
                    self.entry_bar[s] = count
                hold = p.get('hold', {'O05': 10, 'O07': 12, 'O14': 10, 'O16': 8}[rule])
                if pos > 0 and count-self.entry_bar.get(s, 0) >= hold:
                    self.cancel_all(d)
                    self.submit(d, -pos)
                    self.entry_bar.pop(s, None)
                    continue
                if pos == 0 and (not pending or rule == 'O16'):
                    if rule == 'O14':
                        if count % 5:
                            continue
                        first = self.submit(d, q, bt.Order.Limit, ttl=5, price=self.order_price(c*.98))
                        second = self.submit(d, q, bt.Order.Stop, ttl=5, price=self.order_price(c*1.02), oco=first)
                        first.addinfo(priority=4)
                        second.addinfo(priority=4)
                    else:
                        high = a['prior_high' if rule == 'O16' else 'prior_close_high'][i]
                        if not math.isfinite(high):
                            continue
                        stop = self.order_price(high*(1.001 if rule == 'O16' else 1.002))
                        if rule == 'O07':
                            self.submit(d, q, bt.Order.StopLimit, ttl=5, price=stop, plimit=self.order_price(stop*1.004))
                        else:
                            self.submit(d, q, bt.Order.Stop, ttl=3 if rule == 'O16' else 5, price=stop)
                        if rule == 'O16':
                            # Faithful: original resets holding clock at each
                            # submission and allows overlapping pending entries.
                            self.entry_bar[s] = count
            elif rule == 'O20':
                high, low, atr = (a[k][i] for k in ('prior_high', 'prior_low', 'atr'))
                if not all(math.isfinite(x) for x in (high, low, atr)) or pending:
                    continue
                direction = 1 if c > high else -1 if c < low else int(np.sign(pos))
                target = direction*min(nav*.005/atr, nav*.25/c)
                self.submit(d, target-pos)


def metrics(equity, periods=365, initial=CAPITAL):
    e = np.asarray(equity, float)
    valid = len(e) > 1 and np.all(e > 0) and np.isfinite(e).all()
    total = e[-1]/initial-1 if len(e) else 0.
    rets = np.log(e/np.r_[initial, e[:-1]]) if valid else np.full(len(e), np.nan)
    simple = np.expm1(rets)
    half = len(e)//2
    peak = np.maximum.accumulate(np.r_[initial, e])[1:]
    return dict(total_return=total, cagr=(1+total)**(periods/max(len(e)-1, 1))-1 if valid else None,
                sharpe=sharpe(rets.tolist(), periods) if valid else None,
                sharpe_rf2=sharpe(simple[1:].tolist(), periods, .02/periods) if valid else None,
                maxdd=float(np.max(1-e/peak)) if len(e) else 0.,
                halves=[float(np.expm1(np.sum(rets[:half]))), float(np.expm1(np.sum(rets[half:])))] if valid else [None, None],
                bankrupt=not valid)


def run_rule(frames, rule='O01', *, market='crypto', reproduction=False, params=None,
             benchmark=False, rates=None, keep_fills=False, exposure_end=None):
    """Run all bars; optionally end only the average gross exposure at a cutoff."""
    p = {**DEFAULTS.get(rule, {}), **(params or {})}
    index = pd.DatetimeIndex(sorted(set().union(*(set(f.index) for f in frames.values()))))
    available, arrays = {}, {}
    cerebro = bt.Cerebro(stdstats=False, quicknotify=False)
    broker = OrdersBroker(rates=rates, market=market, evaluation=not reproduction and not benchmark)
    # Benchmarks pay trading costs but carry no funding/borrow/financing.
    broker.setcash(BROKER_CASH)
    slip = SLIPPAGE[rule]/10000 if reproduction else .0002 if market == 'us' else .0003
    broker.set_slippage_perc(slip, slip_open=True, slip_out=True)
    if reproduction:
        broker.addcommissioninfo(OriginalCommission(per_share=rule in PER_SHARE))
    else:
        broker.setcommission(commission=.0001 if market == 'us' else .0005)
    broker.carry = Counter()
    cerebro.setbroker(broker)
    for s, original in frames.items():
        featured = features(original, p).reindex(index)
        available[s] = featured.close.notna().to_numpy()
        arrays[s] = {k: featured[k].to_numpy() for k in ('fast', 'slow', 'rsi', 'prior_close_high',
                                                       'prior_high', 'prior_low', 'momentum', 'atr')}
        # Flat marks on unavailable dates; availability blocks orders/fills.
        f = original.reindex(index)
        close = f.close.ffill().fillna(1.)
        for k in ('open', 'high', 'low', 'close'):
            f[k] = f[k].fillna(close)
        f['volume'] = f.volume.fillna(0.)
        dt = index.asi8/86400e9+719163
        rows = np.column_stack([dt, f[['open', 'high', 'low', 'close', 'volume']].to_numpy()])
        cerebro.adddata(ArrayFeed(dataname=rows), name=s)
    # Calendar membership only, no future prices: observed exchange sessions
    # include month-end holidays. An incomplete tail is not a month-end event.
    monthends = {index[j].strftime('%Y-%m-%d') for j in range(len(index)-1)
                 if index[j].month != index[j+1].month}
    tail = index[-1]
    if tail.is_month_end or (market == 'us' and tail+pd.offsets.BMonthEnd(0) == tail):
        monthends.add(tail.strftime('%Y-%m-%d'))
    # Intraday feeds rebalance once at the last completed bar of month-end,
    # rather than rebalancing on every bar carrying that calendar date.
    monthend_indices = {j for j in range(len(index)) if index[j].strftime('%Y-%m-%d') in monthends
                        and (j == len(index)-1 or index[j].date() != index[j+1].date())}
    cerebro.addstrategy(OrderStrategy, rule=rule, settings=p, arrays=arrays, available=available, monthends=monthend_indices,
                        dates=index.strftime('%Y-%m-%d').tolist(), reproduction=reproduction,
                        benchmark=benchmark, exposure_end=exposure_end)
    st = cerebro.run(runonce=False, tradehistory=True)[0]
    periods = 252 if market == 'us' else 365
    m = metrics(list(st.daily.values()), periods)
    m.update(trades=len(st.closed), average_net_bps=float(np.mean([x['net_bps'] for x in st.closed])) if st.closed else None,
             turnover=st.turnover, average_exposure=st.exposure_sum/max(st.observations, 1),
             commission=st.commission, funding=broker.funding_paid, borrow=broker.borrow_paid,
             financing=broker.financing_paid, missing_funding=broker.funding_missing,
             funding_observations=broker.funding_observations)
    result = dict(metrics=m, daily_equity=st.daily, entries=st.entries, closed=st.closed)
    if keep_fills:
        result['fills'] = st.fills
    return result


def daily_logs(run, end=None):
    e = pd.Series(run['daily_equity'], dtype=float)
    if end:
        e = e[e.index <= end]
    if (e <= 0).any():
        return pd.Series(np.nan, index=e.index)
    return np.log(e/e.shift(fill_value=CAPITAL))


def deflated_sharpe(returns, trials, sr_variance):
    """Bailey & Lopez de Prado (2014), daily log returns, rf=0, no annualization.

    SR0 = sqrt(Var_cross(SR)) * [(1-gamma) Phi^-1(1-1/N)
                              + gamma Phi^-1(1-1/(N*e))].
    DSR = Phi[(SR-SR0)*sqrt(T-1) /
              sqrt(1-skew*SR+(Pearson_kurtosis-1)*SR^2/4)].
    G2 uses strategy returns and their cross-sectional sample SR variance.
    Excess returns are evaluated separately for the original-spec diagnostic.
    For N=1 there is no selection hurdle, SR0=0. Constant/invalid series fail.
    """
    x = np.asarray(returns, float)
    if trials < 1 or sr_variance < 0:
        raise ValueError('trials >= 1 and variance >= 0 required')
    if len(x) < 3 or not np.isfinite(x).all() or np.std(x, ddof=1) == 0:
        return 0.
    sr = float(np.mean(x)/np.std(x, ddof=1))
    central = x-np.mean(x)
    sigma = np.sqrt(np.mean(central**2))
    skew, kurt = np.mean(central**3)/sigma**3, np.mean(central**4)/sigma**4
    norm, gamma = NormalDist(), .5772156649015329
    hurdle = math.sqrt(sr_variance)*((1-gamma)*norm.inv_cdf(1-1/trials)
                    + gamma*norm.inv_cdf(1-1/(trials*math.e))) if trials > 1 else 0.
    den = math.sqrt(max(1e-15, 1-skew*sr+(kurt-1)*sr*sr/4))
    return norm.cdf((sr-hurdle)*math.sqrt(len(x)-1)/den)


def apply_family_dsr(family, trials, diagnostic=()):
    """Set strategy G2 and excess diagnostics with separate SR variances.

    Each entry is (row, aligned strategy logs, aligned benchmark logs, periods).
    The family includes daily and intraday rule/market portfolios, never coins
    or parameter neighbors; intraday admission remains its separate four gates.
    """
    for row, strategy, benchmark, _ in (*family, *diagnostic):
        s, b = np.asarray(strategy, float), np.asarray(benchmark, float)
        for key, x in (('daily_sharpe', s), ('excess_daily_sharpe', s-b)):
            row[key] = float(np.mean(x)/np.std(x, ddof=1)) if len(x) > 1 and np.isfinite(x).all() and np.std(x, ddof=1) > 0 else 0.
    variances = [float(np.var([row[key] for row, *_ in family], ddof=1)) if len(family) > 1 else 0.
                 for key in ('daily_sharpe', 'excess_daily_sharpe')]
    for row, strategy, benchmark, _ in (*family, *diagnostic):
        row['dsr'] = deflated_sharpe(strategy, trials, variances[0])
        row['dsr_excess'] = deflated_sharpe(np.asarray(strategy)-np.asarray(benchmark), trials, variances[1])
    return tuple(variances)


def matched_exposure_logs(benchmark, average_exposure):
    """Scale the original benchmark's SIMPLE returns, then return daily logs.

    Exposure is the strategy's mean gross notional/NAV over the gate period.
    Leverage above one is retained without financing costs; a zero benchmark
    stays zero for market-neutral rules. Nonpositive NAV factors invalidate G1.
    """
    if not math.isfinite(average_exposure) or average_exposure < 0:
        raise ValueError('average gross exposure must be finite and nonnegative')
    with np.errstate(invalid='ignore', divide='ignore', over='ignore'):
        return np.log1p(average_exposure*np.expm1(np.asarray(benchmark, float)))


def gate_decision(strategy, benchmark, periods, dsr, plateau, cross_ratio=None, *, average_exposure=1.):
    s, full_b = list(strategy), list(benchmark)
    b = matched_exposure_logs(full_b, average_exposure).tolist()
    valid = len(s) == len(full_b) and len(s) > 1 and np.isfinite(s).all() and np.isfinite(full_b).all()
    matched_valid = valid and np.isfinite(b).all()
    audit = audit_returns(s, b, periods) if matched_valid else {}
    full_audit = audit_returns(s, full_b, periods) if valid else {}
    g1 = audit.get('sample_sufficient', False) and all(audit[f'{w}_sharpe'] > audit[f'{w}_sharpe_bench'] for w in ('train', 'holdout'))
    h = len(s)//2
    halves = [math.expm1(sum(s[:h])), math.expm1(sum(s[h:]))] if valid else [None, None]
    gates = dict(G1=bool(g1), G2=bool(dsr >= .95),
                 G3=bool(valid and all(x > 0 for x in halves)), G4=plateau == 'PLATEAU')
    if cross_ratio is not None:
        gates['G5'] = cross_ratio >= .60
    failed = [g for g, passed in gates.items() if not passed]
    return dict(verdict='FAIL' if failed else 'PASS', failed=failed, gates=gates, audit=audit,
                full_exposure_audit=full_audit, average_exposure=average_exposure,
                matched_benchmark_sharpe=sharpe(b, periods) if matched_valid else None, halves=halves)


def intraday_decision(rows):
    eligible = []
    for s, r in rows.items():
        m, other = r['metrics'], rows['ETH' if s == 'BTC' else 'BTC']['metrics']
        if (m['sharpe'] is not None and m['sharpe'] >= .8 and all(x is not None and x > 0 for x in m['halves'])
                and m['average_net_bps'] is not None and m['average_net_bps'] >= 5 and other['total_return'] > 0):
            eligible.append(s)
    return dict(verdict='PASS' if eligible else 'FAIL', eligible=eligible)


def official(rule):
    path = next((ROOT / 'quantjourney/reports').glob(f'example-orders-{int(rule[1:]):02}-*.json'))
    return json.loads(path.read_text())['sections']


def compare_reproduction(rule, run, sections):
    summary = {r['metric']: r['all'] for r in sections['performance_summary']['rows']}
    ratios = {r['metric']: r['value'] for r in sections['performance_ratios']['rows']}
    off = dict(total_return=summary['Return on Initial Capital'], cagr=ratios['CAGR'],
               sharpe_rf2=ratios['Sharpe Ratio'], trades=summary['Round Trips'])
    e = pd.Series(run['daily_equity'], dtype=float)
    e.index = pd.to_datetime(e.index)
    monthly = e.groupby(e.index.to_period('M')).last()
    monthly = monthly/monthly.shift(fill_value=CAPITAL)-1
    target = sections['periodical_analysis']['monthly']
    target = pd.Series({pd.Period(year=x['year'], month=x['month'], freq='M'): x['return'] for x in target})
    both = pd.concat([monthly, target], axis=1).dropna()
    corr = float(both.iloc[:, 0].corr(both.iloc[:, 1]))
    ours = dict(run['metrics'])
    # QJ compute_annualized_return uses calendar years, unlike audit_returns'
    # observation-count CAGR. Keep the reproduction comparison in QJ units.
    years = (e.index[-1]-e.index[0]).days/365.25
    ours['cagr'] = (1+ours['total_return'])**(1/years)-1 if years > 0 and ours['total_return'] > -1 else None
    delta = abs(ours['total_return']-off['total_return'])
    passed = corr >= .90 and (delta <= .10 or delta <= .30*abs(off['total_return']))
    source_entries = Counter((x['Instrument'], x['Timestamp'][:10])
                             for x in sections.get('trade_list', {}).get('rows', []) if x['Side'] == 'buy')
    local_entries = Counter(map(tuple, run['entries']))
    matches = sum((source_entries & local_entries).values())
    agreement = matches/sum(source_entries.values()) if source_entries else None
    reason = '' if passed else ('月收益相关不足' if corr < .90 else '总收益误差超限')
    return dict(rule=rule, ours=ours, official=off, monthly_correlation=corr,
                absolute_return_error=delta, relative_return_error=delta/abs(off['total_return']) if off['total_return'] else None,
                entry_agreement=agreement, matched_entries=matches, official_entries=sum(source_entries.values()),
                verdict='PASS' if passed else 'FAIL', reason=reason)


def compact(run):
    return dict(metrics=run['metrics'], daily_equity=run['daily_equity'])


def run_study(rules=RULES, market='all', progress=print):
    result = dict(engine=f'Backtrader {bt.__version__}', reproduction=[], us={}, crypto={}, intraday={},
                  excluded={'O19': '原市场 FX，nofx 不交易', 'O20_original': '原市场期货，nofx 不交易；仅评估加密 Donchian+ATR 逻辑移植'},
                  parameters={r: DEFAULTS[r] for r in rules}, reference={}, oos={})
    markets = ('us', 'crypto') if market == 'all' else (market,)
    # Daily rule × market combinations form the DSR trial pool. Intraday O15/O16
    # are judged by their own frozen gate; their SR near -8 in the pool would lift
    # the expected-max hurdle far above any daily rule (diagnostic DSR only).
    trials = sum(1 for r in rules for m in markets if r not in ('O15', 'O16') and not (m == 'us' and r == 'O20'))
    result['dsr_trials'] = trials
    daily_family = []
    benchmarks = {}
    for m in markets:
        for r in rules:
            if r not in DAILY or (m == 'us' and r == 'O20'):
                continue
            symbols = ETFS if r == 'O17' and m == 'us' else COINS if r == 'O17' else ('SPY', 'QQQ', 'IWM') if m == 'us' else ('BTC', 'ETH')
            start = '2007-01-03' if r == 'O17' else '2001-01-03'
            frames = {s: read_daily(s, m).loc[start:'2026-10-30'] if m == 'us' else read_daily(s, m) for s in symbols}
            rates = {s: load_funding(ROOT, s+'USDT') for s in symbols} if m == 'crypto' else None
            if m == 'us':
                sections = official(r)
                settings = sections['settings']['values']
                rep_frames = {s: f.loc[settings['start_date']:settings['end_date']] for s, f in frames.items()}
                rep = run_rule(rep_frames, r, market=m, reproduction=True)
                result['reproduction'].append(compare_reproduction(r, rep, sections))
            # Neither the US G1 returns nor its mean exposure use the OOS tail.
            gate_end = '2025-12-31' if m == 'us' else None
            run = run_rule(frames, r, market=m, rates=rates, exposure_end=gate_end)
            key = (m, tuple(symbols), start)
            if key not in benchmarks:
                benchmarks[key] = run_rule(frames, r, market=m, benchmark=True)
            bench = benchmarks[key]
            # Publication sample is not used for ANY US gate or neighborhood.
            logs, blog = daily_logs(run, gate_end), daily_logs(bench, gate_end)
            aligned = pd.concat([logs, blog], axis=1)
            logs, blog = aligned.iloc[:, 0], aligned.iloc[:, 1]
            gate_frames = {s: f.loc[:gate_end] if gate_end else f for s, f in frames.items()}
            scores = {}
            for values, params in grid_around(DEFAULTS[r]).items():
                if params == DEFAULTS[r]:
                    score = sharpe(logs.tolist(), 252 if m == 'us' else 365) if np.isfinite(logs).all() else -math.inf
                else:
                    sweep_run = run_rule(gate_frames, r, market=m, params=params, rates=rates)
                    score = sweep_run['metrics']['sharpe']
                    score = score if score is not None else -math.inf
                scores[values] = score
            sweep = sweep_plateau(lambda p: scores[tuple(p.values())], grid_around(DEFAULTS[r]), tuple(DEFAULTS[r].values()))
            plateau = 'LONE PEAK' if sweep.lone_peak else 'PLATEAU'
            cross = None
            if m == 'crypto' and r != 'O17':
                cross = {}
                for s in COINS:
                    single = run_rule({s: read_daily(s, m)}, r, market=m,
                                      rates={s: load_funding(ROOT, s+'USDT')})
                    cross[s] = single['metrics']
            row = dict(rule=r, name=NAMES[int(r[1:])-1], edge_source=edge_source(r),
                       run=compact(run), benchmark=compact(bench), cross_section=cross,
                       plateau=plateau, plateau_ratio=sweep.plateau_ratio,
                       sweep=[dict(params=p, sharpe=scores[k]) for k, p in grid_around(DEFAULTS[r]).items()])
            result[m][r] = row
            daily_family.append((row, logs, blog, 252 if m == 'us' else 365))
            if m == 'us':
                e = pd.Series(run['daily_equity'])
                b = pd.Series(bench['daily_equity'])
                tail = e[e.index >= '2026-01-01']
                btail = b[b.index >= '2026-01-01']
                initial = float(e[e.index < '2026-01-01'].iloc[-1])
                binitial = float(b[b.index < '2026-01-01'].iloc[-1])
                result['oos'][r] = dict(strategy=metrics(tail, 252, initial), benchmark=metrics(btail, 252, binitial))
            progress(f'{m} {r}: 组合、基准、参数邻域'+('、10币截面完成' if cross else '完成'), flush=True)
        ref = 'SPY' if m == 'us' else 'BTC'
        f = read_daily(ref, m)
        if m == 'us':
            f = f.loc['2001-01-03':'2026-10-30']
        result['reference'][m] = compact(run_rule({ref: f}, 'O01', market=m, benchmark=True))
    intraday_family = []
    if 'crypto' in markets:
        for r in rules:
            if r not in ('O15', 'O16'):
                continue
            rows = {}
            intra_frames = {}
            for s in ('BTC', 'ETH'):
                f = load_bars(ROOT / 'volrix/klines' / f'{s}USDT_5m.json').tz_localize(None)
                if r == 'O16':
                    bins = f.resample('30min', origin='epoch', label='left', closed='left')
                    f = bins.agg(dict(open='first', high='max', low='min', close='last', volume='sum')).loc[bins.close.count().eq(6)]
                rates = {s: load_funding(ROOT, s+'USDT')}
                rows[s] = compact(run_rule({s: f}, r, market='crypto', rates=rates))
                intra_frames[s] = f
                progress(f'crypto {r} {s}: 日内全样本完成', flush=True)
            combo = run_rule(intra_frames, r, market='crypto', rates={s: load_funding(ROOT, s+'USDT') for s in ('BTC', 'ETH')})
            progress(f'crypto {r}: 日内组合完成；开始月末等权基准', flush=True)
            bench = run_rule(intra_frames, r, market='crypto', benchmark=True)
            progress(f'crypto {r}: 日内基准完成', flush=True)
            aligned = pd.concat([daily_logs(combo), daily_logs(bench)], axis=1)
            row = dict(rows=rows, admission=intraday_decision(rows),
                       portfolio=compact(combo), benchmark=compact(bench))
            result['intraday'][r] = row
            intraday_family.append((row, aligned.iloc[:, 0], aligned.iloc[:, 1], 365))
    result['dsr_sr_variance'], result['dsr_excess_sr_variance'] = apply_family_dsr(daily_family, trials, intraday_family)
    for row, s, b, periods in daily_family:
        cross = row['cross_section']
        ratio = sum(x['sharpe'] is not None and x['sharpe'] > 0 for x in cross.values())/len(cross) if cross else None
        row['cross_positive_ratio'] = ratio
        row['admission'] = gate_decision(s, b, periods, row['dsr'], row['plateau'], ratio,
                                        average_exposure=row['run']['metrics']['average_exposure'])
    return result


def fmt(x, percent=False):
    if x is None or not math.isfinite(float(x)):
        return '—'
    return f'{x*100:.2f}%' if percent else f'{x:.3f}'


def table(headers, rows):
    return '\n'.join(['| '+' | '.join(headers)+' |', '| '+' | '.join(['---']*len(headers))+' |']+
                     ['| '+' | '.join(map(str, row))+' |' for row in rows])


def reproduction_table(result):
    rows = []
    for r in result['reproduction']:
        a, b = r['ours'], r['official']
        pair = lambda k, pct=False: fmt(a[k], pct)+' / '+fmt(b[k], pct)
        rows.append([r['rule'], fmt(r['monthly_correlation']), pair('total_return', True), pair('cagr', True),
                     pair('sharpe_rf2'), f"{a['trades']} / {b['trades']}", fmt(r['entry_agreement'], True),
                     r['verdict']+('：'+r['reason'] if r['reason'] else '')])
    return table(['规则', '月相关', '总收益 本地/官方', 'CAGR 本地/官方', 'Sharpe rf2% 本地/官方',
                  '交易数 本地/官方', '入场日期一致率', '复现'], rows)


def main_table(result, market):
    rows = []
    for r, row in result[market].items():
        a, b, g = row['run']['metrics'], row['benchmark']['metrics'], row['admission']
        audit = g['audit']
        full_audit = g['full_exposure_audit']
        split = lambda w: fmt(full_audit.get(w+'_sharpe'))+'/'+fmt(full_audit.get(w+'_sharpe_bench'))
        matched_split = ' / '.join(fmt(audit.get(w+'_sharpe'))+':'+fmt(audit.get(w+'_sharpe_bench'))
                                   for w in ('train', 'holdout'))
        rows.append([r, row['edge_source'], fmt(a['sharpe']), fmt(b['sharpe']), split('train'), split('holdout'),
                     fmt(g['matched_benchmark_sharpe']), matched_split,
                     fmt(row['dsr']), fmt(row['dsr_excess']), '/'.join(fmt(x, True) for x in g['halves']),
                     row['plateau']+f"({row['plateau_ratio']:.0%})", fmt(a['maxdd'], True)+'/'+fmt(b['maxdd'], True),
                     a['trades'], fmt(a['average_net_bps']), fmt(a['turnover']), fmt(a['average_exposure']),
                     fmt(row['cross_positive_ratio'], True), g['verdict']+(' '+','.join(g['failed']) if g['failed'] else '')])
    return table(['规则', 'edge_source', 'Sharpe', '基准Sharpe', '60%窗 S/B', '40%窗 S/B',
                  '同敞口基准SR', '60/40 SR 策略:同敞口基准', 'DSR(策略)', 'DSR(超额,原规格)',
                  '两半收益', '平台(正分比)', 'MaxDD S/B', '交易数', '平均净bps/笔', '换手', '平均敞口', 'G5正Sharpe比', '准入'], rows)


def intraday_table(result):
    rows = []
    for r, row in result['intraday'].items():
        for s, run in row['rows'].items():
            a = run['metrics']
            rows.append([r, s, edge_source(r), '5m' if r == 'O15' else '30m', fmt(a['total_return'], True),
                         fmt(a['sharpe']), '/'.join(fmt(x, True) for x in a['halves']),
                         a['trades'], fmt(a['average_net_bps']), row['admission']['verdict']])
    return table(['规则', '币', 'edge_source', '周期', '净收益', '日Sharpe', '两半净收益', '交易数', '平均净bps/笔', '规则准入'], rows)


def pass_table(result):
    rows = [[m, r, row['admission']['verdict'], ','.join(row['admission']['failed']) or 'G1–G5全部满足']
            for m in ('us', 'crypto') for r, row in result[m].items()]
    rows += [['crypto日内', r, row['admission']['verdict'], ','.join(row['admission']['eligible']) or '无币同时满足日内四门槛']
             for r, row in result['intraday'].items()]
    return table(['市场', '规则', 'PASS/FAIL', '未过门槛/通过币'], rows)


def render_report(result):
    lines = ['# QuantJourney 订单型策略移植评估', '',
             result['engine']+'；初始策略资本1000；所有订单、成交、仓位、交易盈亏通过 Backtrader。', '',
             '## 口径说明', '',
             '- 美股使用 adjclose/close 复权全部 OHLC；复现按原区间从空仓开始、原整数股按100倍账户映射到1000本金，原每股佣金及每单最低1美元也同比缩放。原 FixedBpsCommission 为1bp，settings 的 null/摘要0并非零佣金；滑点逐规则3/4/5bp。',
             '- 复现与评估均为 t 收盘信号、t+1 开盘 Market；未使用 cheat-on-close。Limit 触价才成交，跳空改善；Stop 跳空按更差开盘价；StopLimit 触发后保留限价；trailing 在本根高/低更新后测试，StopTrailLimit 固定金额距离和限价偏移。滑点可超出OHLC但不得违反限价。',
             '- 订单有效期按可交易bar计数、在第N次尝试后到期；停止类先于Limit再Market。O14两条原始OCO订单同优先级、按源码先Limit后Stop。复现bracket下一根激活；评估额外测试入场根SL、忽略该根TP（日内路径未知时保守）。',
             '- 价格精度：美股（含复现）保留源码round(price,2)的美分价位；加密限价、止损价、止损限价、bracket价及跟踪距离/限价偏移使用原始浮点价，保留相对参考价的方向关系，不按小数位取整。未按各币实盘tick重新优化规则。',
             '- 评估按当前策略权益与信号close复利定数量，允许碎股/碎币以保留1000本金下的原名义仓位比例。虚拟充足broker现金仅消除股票式现金拒单；所有收益、融资阈值与敞口均以实际1000策略权益计。O01–O14/O18美股为SPY/QQQ/IWM一个组合，加密为BTC+ETH一个组合；逐币另跑G5。O17为原8ETF/固定10币月度动量（新月首根收盘排名，下一根开盘下单），保留现有入选仓位数量。',
             '- 美股评估1bp佣金+2bp滑点/边，空头每日0.5%/年借券、超过权益的多头名义5%/年融资，周末按日历天计；加密5bp+3bp/边，隔夜带入仓位在UTC日首根开盘扣日均资金费×3，正费率多付空收。缓存缺失资金费按0并计数；现金无息。平均每笔净bps按已平仓原生交易净盈亏（含持仓分摊资金/融资费）/入场名义求均值。',
             '- 单标的BH计一次入场成本、不计资金费；组合基准为同篮子等权BH、月末再平衡并计成本。不可交易币权重0、不参与排名；固定存活10币有幸存者偏差。另列SPY/BTC现货等价BH参照。',
             '- 主表完整日权益；美股所有G1–G4、DSR横截面及扫参仅用截至2025-12-31数据，2026-01..10保持原策略状态单列为发布后样本外，不参与准入。G3按收益序列对半，两半净收益均>0。',
             '- G1 更正为同敞口基准，属于收紧，避免对数收益方差拖累带来的机械优势。同敞口基准m_t=log(1+e·r_t)，r_t为原组合基准当日简单收益，e为该组合准入评估期的平均总敞口（美股截至2025-12-31，加密全期）；e>1保留杠杆倍数、不另计融资费，市场中性基准0不变。O01–O14/O18使用三ETF或BTC+ETH等权基准，O17使用其篮子等权基准。调用audit_returns(策略日对数收益,m,periods)，60/40两窗Sharpe均严格高于同敞口基准才通过。主表同敞口基准SR、平均敞口及60/40比较均用准入评估期；原满仓基准Sharpe及60%/40%窗S/B保留参照，Sharpe/基准Sharpe仍展示完整日权益。',
             '- 预声明 G2 作用于超额收益，对低敞口策略结构性不可通过（全部为 0）；审查时更正为策略自身收益的 DSR。该更正在看到全部 DSR=0 之后作出，属于事后放宽，已披露。',
             f"- G2：策略自身日对数收益（rf=0）的DSR≥0.95；SR、偏度、非超额峰度均来自策略自身收益，SR不年化。试验数N={result['dsr_trials']}（日线规则×市场组合数；O15/O16 日内规则按日内门槛裁决，不进入试验池与方差，否则其 SR≈−8 会把期望最大 SR 门槛抬到远高于任何日线规则；该排除在审查时作出，已披露），策略自身日SR横截面样本方差={result['dsr_sr_variance']:.8g}；不把逐币、参数点重复计为规则。主表DSR(超额,原规格)单独使用超额日SR横截面样本方差={result['dsr_excess_sr_variance']:.8g}，仅诊断、不参与准入。G4预声明一个主参数，grid_around×0.5/0.75/1/1.25/1.5，原样sweep_plateau判定（默认正分且正分比<50%为孤峰，其余PLATEAU）。G5仅加密单标的日线逻辑：10币正Sharpe占比≥60%，O17无G5。",
             '- 日内O15 BTC/ETH 5m，O16由完整6根5m重采样30m；全缓存期。Sharpe按UTC日末权益、365年化；同一币Sharpe≥0.8、两半净收益>0、平均每笔≥5bps，另一币全样本收益>0。O16忠实保留源码允许多条未成交Stop并存、提交时重设持有时钟的行为。',
             '- O19：原市场 FX/期货，nofx 不交易（FX手数未移植）。O20：原市场 FX/期货，nofx 不交易（期货合约未移植）；另评估加密Donchian55+简单ATR20逻辑、每币风险0.5%NAV且名义≤25%NAV，无期货整数合约/保证金约束。O15/O16不在美股运行。', '',
             '## 复现表', '', reproduction_table(result), '',
             '复现合格：月度相关≥0.90，且总收益相对误差≤30%或绝对误差≤10个百分点；交易数与入场一致率用于诊断，不替代该合格线。入场一致率为(标的,日期)多重集合一对一匹配/官方买入数。复现CAGR按首末日期差/365.25日历年；rf2% Sharpe按简单日收益减0.02/252、样本标准差，与官方口径对齐；评估/G1使用日对数收益及rf=0。', '',
             '## 美股评估主表', '', main_table(result, 'us'), '',
             '## 加密评估主表', '', main_table(result, 'crypto'), '',
             '## 日内表', '', intraday_table(result), '',
             '## 2026-01..10 发布后样本外（美股，仅展示）', '',
             table(['规则', '净收益 S/B', 'Sharpe S/B', 'MaxDD S/B'],
                   [[r, fmt(x['strategy']['total_return'], True)+'/'+fmt(x['benchmark']['total_return'], True),
                     fmt(x['strategy']['sharpe'])+'/'+fmt(x['benchmark']['sharpe']),
                     fmt(x['strategy']['maxdd'], True)+'/'+fmt(x['benchmark']['maxdd'], True)] for r, x in result['oos'].items()]), '',
             '## 主参数', '', table(['规则', '主参数默认值'], [[r, str(p)] for r, p in result['parameters'].items()]), '',
             '## SPY/BTC 参照', '', table(['市场', '收益', 'Sharpe', 'MaxDD'],
                  [[m, fmt(x['metrics']['total_return'], True), fmt(x['metrics']['sharpe']), fmt(x['metrics']['maxdd'], True)] for m, x in result['reference'].items()]), '',
             '## 复现差异排查', '']
    for row in result['reproduction']:
        if row['verdict'] == 'FAIL':
            lines.append(f"- {row['rule']}：{row['reason']}；月相关{fmt(row['monthly_correlation'])}，收益绝对误差{fmt(row['absolute_return_error'], True)}，入场一致率{fmt(row['entry_agreement'], True)}。本地OHLC为指定缓存复权行情，官方数据hash对应的逐日价格与云端quantjourney_ti RSI实现未随报告发布，无法证明完全相同；trailing及bracket已按本地只读引擎核对，不以调参追平官方。")
    if not any(x['verdict'] == 'FAIL' for x in result['reproduction']):
        lines.append('全部复现达到预声明合格线。')
    lines += ['', '## 资金费缓存覆盖', '', table(['规则', '缺失持仓结算日/总持仓结算日'],
                [[r, f"{x['run']['metrics']['missing_funding']}/{x['run']['metrics']['funding_observations']}"] for r, x in result['crypto'].items()]), '',
              '## PASS 汇总', '', pass_table(result), '',
              '未建模强平、最小数量、交易所阶梯保证金；尾部仅盯市不强平。参数平台与多窗均来自同一历史，不能作为独立样本外证据。JSON仅汇总与UTC日权益，不保存逐bar日内权益、全部成交或扫参权益。', '']
    return '\n'.join(lines)


def json_safe(x):
    if isinstance(x, dict):
        return {str(k): json_safe(v) for k, v in x.items()}
    if isinstance(x, (list, tuple)):
        return [json_safe(v) for v in x]
    if isinstance(x, (float, np.floating)):
        return float(x) if math.isfinite(x) else None
    if isinstance(x, np.integer):
        return int(x)
    return x


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--rules', default='all', help='all or comma-separated O01..O18,O20')
    parser.add_argument('--market', choices=('us', 'crypto', 'all'), default='all')
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--json', type=Path, required=True)
    args = parser.parse_args(argv)
    rules = list(RULES) if args.rules == 'all' else [r.strip().upper() for r in args.rules.split(',')]
    if not rules or len(set(rules)) != len(rules) or any(r not in RULES for r in rules):
        parser.error('--rules must contain unique O01..O18,O20')
    result = run_study(rules, args.market)
    serialized = json.dumps(json_safe(result), ensure_ascii=False, separators=(',', ':'), allow_nan=False)
    if len(serialized.encode()) >= 30*1024*1024:
        raise ValueError('JSON exceeds 30 MB')
    for path in (args.out, args.json):
        path.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(render_report(result))
    args.json.write_text(serialized)
    print(f'报告：{args.out}；JSON：{args.json} ({len(serialized.encode())/1024/1024:.2f} MiB)')
    print(pass_table(result))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
