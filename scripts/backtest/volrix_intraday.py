"""Volrix 日内规则：离线信号 + 原生 Backtrader 订单、OCO、费用及盈亏。

Run: python -m scripts.backtest.volrix_intraday --out ... --json ...
Only indicator calculation, order scheduling and reporting live here. All fills,
positions, commissions and trade P&L are owned by BackBroker. No cheat modes.
"""
from __future__ import annotations

import argparse
from collections import defaultdict
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import sys

import numpy as np
import pandas as pd

from .backtrader_adapter import _backtrader, _through_limit_broker
from .quant_audit import audit_returns

bt = _backtrader()
DATA_ROOT = Path('/Users/zhangyun/workspace/nofx/data')
RULES = ('VWAP_RECLAIM', 'HOLY_GRAIL', 'NR7', 'ICHI_ST', 'RSI3_2C', 'PDH_PDL')
SYMBOLS = ('BTCUSDT', 'ETHUSDT')
TIMEFRAMES = {r: (15 if r in ('HOLY_GRAIL', 'RSI3_2C') else 5) for r in RULES}
TICKS = {'BTCUSDT': 0.1, 'ETHUSDT': 0.01}
COSTS = {'volrix': (0.0, 0.0001, False), 'real': (0.0005, 0.0003, True)}
CAPITAL = 1000.0
BROKER_CASH = 1_000_000.0
DAY_MS = 86_400_000
FUND_MS = 28_800_000
ADMISSION = ('至少一个币全样本 Sharpe ≥ 0.8', '该币前后两半 Sharpe 都 > 0',
             '该币平均每笔净收益 ≥ 5 bps', '另一个币全样本收益 > 0')


def smooth(values, period, *, wilder=False):
    """SMA-seeded EMA / Wilder smoothing; seed only after period observations."""
    a = np.asarray(values, dtype=float)
    out = np.full(len(a), np.nan)
    alpha = 1.0 / period if wilder else 2.0 / (period + 1)
    seed = []
    last = math.nan
    for i, v in enumerate(a):
        if not math.isfinite(v):
            seed.clear()
            last = math.nan
            continue
        if math.isnan(last):
            seed.append(v)
            if len(seed) < period:
                continue
            last = sum(seed) / period
        else:
            last += alpha * (v - last)
        out[i] = last
    return out


def load_bars(path):
    path = Path(path)
    if not path.is_file():
        raise FileNotFoundError(f'缺少本地 5m 数据：{path}；离线 CLI 不下载或生成替代数据')
    raw = json.loads(path.read_text())
    if not raw:
        raise ValueError(f'空 K 线文件：{path}')
    a = np.asarray([row[:6] for row in raw], dtype=float)
    if a.ndim != 2 or a.shape[1] != 6 or not np.isfinite(a).all():
        raise ValueError(f'非法 K 线结构或非有限值：{path}')
    ms, o, h, l, c, v = a.T
    if (np.any(ms % 300_000) or np.any(np.diff(ms) != 300_000)
            or np.any(l <= 0) or np.any(v < 0) or np.any(l > np.minimum(o, c))
            or np.any(h < np.maximum(o, c)) or np.any(l > h)):
        raise ValueError(f'K 线必须是连续、UTC 对齐且 OHLC 合法的 5m：{path}')
    return pd.DataFrame(a[:, 1:], columns=['open', 'high', 'low', 'close', 'volume'],
                        index=pd.to_datetime(ms.astype('int64'), unit='ms', utc=True))


def resample_15m(frame):
    """Only fully observed UTC-aligned bins; no partial tail or missing sub-bars."""
    r = frame.resample('15min', origin='epoch', label='left', closed='left')
    out = r.agg({'open': 'first', 'high': 'max', 'low': 'min', 'close': 'last', 'volume': 'sum'})
    return out.loc[r['close'].count().eq(3)].copy()


def build_features(frame):
    """All columns at t depend only on bars through t (visible cloud shifts +26)."""
    f = frame.copy()
    h, l, c, v = (f[k] for k in ('high', 'low', 'close', 'volume'))
    for n in (20, 50, 200):
        f[f'ema{n}'] = smooth(c, n)
    f['sma20'] = c.rolling(20).mean()
    f['vol_sma20'] = v.rolling(20).mean()
    tr = pd.concat([h-l, (h-c.shift()).abs(), (l-c.shift()).abs()], axis=1).max(axis=1)
    f['atr14'] = smooth(tr, 14, wilder=True)
    f['atr7'] = smooth(tr, 7, wilder=True)
    up, down = h.diff(), -l.diff()
    plus = np.where((up > down) & (up > 0), up, 0.)
    minus = np.where((down > up) & (down > 0), down, 0.)
    ps, ns = smooth(plus, 14, wilder=True), smooth(minus, 14, wilder=True)
    den = ps + ns
    dx = np.divide(100 * np.abs(ps-ns), den, out=np.zeros(len(f)), where=den != 0)
    dx[~np.isfinite(den)] = np.nan
    f['adx14'] = smooth(dx, 14, wilder=True)
    delta = c.diff().to_numpy()
    gain = smooth(np.maximum(delta, 0), 3, wilder=True)
    loss = smooth(np.maximum(-delta, 0), 3, wilder=True)
    total = gain + loss
    f['rsi3'] = np.divide(100 * gain, total, out=np.full(len(f), 50.), where=total != 0)
    date = f.index.floor('D')
    pv = ((h+l+c)/3*v).groupby(date).cumsum()
    vv = v.groupby(date).cumsum()
    f['vwap'] = pv / vv.replace(0, np.nan)
    tenkan = (h.rolling(9).max()+l.rolling(9).min())/2
    kijun = (h.rolling(26).max()+l.rolling(26).min())/2
    f['cloud_a'] = ((tenkan+kijun)/2).shift(26)
    f['cloud_b'] = ((h.rolling(52).max()+l.rolling(52).min())/2).shift(26)
    basic_u = ((h+l)/2+3*f.atr7).to_numpy()
    basic_l = ((h+l)/2-3*f.atr7).to_numpy()
    close = c.to_numpy()
    direction = np.zeros(len(f), dtype=int)
    line = np.full(len(f), np.nan)
    upper = lower = math.nan
    for i in range(len(f)):
        if not math.isfinite(basic_u[i]):
            continue
        if math.isnan(upper):
            upper, lower, d = basic_u[i], basic_l[i], 1
        else:
            upper = basic_u[i] if basic_u[i] < upper or close[i-1] > upper else upper
            lower = basic_l[i] if basic_l[i] > lower or close[i-1] < lower else lower
            d = direction[i-1]
            if d < 0 and close[i] > upper:
                d = 1
            elif d > 0 and close[i] < lower:
                d = -1
        direction[i], line[i] = d, lower if d > 0 else upper
    f['st_direction'], f['supertrend'] = direction, line
    # Previous UTC calendar day, not the last available day across a data gap.
    daily = f.groupby(date).agg(high=('high', 'max'), low=('low', 'min'), count=('close', 'size'))
    minutes = int((f.index[1]-f.index[0]).total_seconds()/60) if len(f) > 1 else 5
    complete = daily['count'].eq(1440//minutes)
    prev = daily[['high', 'low']].where(complete, np.nan)
    prev.index = prev.index.shift(1, freq='D')
    f['pdh'] = date.map(prev.high)
    f['pdl'] = date.map(prev.low)
    return f


def build_setups(f, rule, tick):
    """Causal signal proposals. Broker state separately enforces entry/session caps."""
    if rule not in RULES:
        raise ValueError(f'未知规则：{rule}')
    s = pd.DataFrame(index=f.index, data={'side': 0, 'trigger': np.nan,
                                         'stop': np.nan, 'atr': f.atr14})
    c, h, l, o = (f[k] for k in ('close', 'high', 'low', 'open'))
    long = short = pd.Series(False, index=f.index)
    if rule == 'VWAP_RECLAIM':
        long = (f.ema50 > f.ema200) & (l < f.vwap) & (c > f.vwap) & (f.volume > f.vol_sma20)
        short = (f.ema50 < f.ema200) & (h > f.vwap) & (c < f.vwap) & (f.volume > f.vol_sma20)
        long &= f.atr14.gt(0)
        short &= f.atr14.gt(0)
    elif rule == 'HOLY_GRAIL':
        a, m = f.adx14.to_numpy(), f.sma20.to_numpy()
        highs, lows = h.to_numpy(), l.to_numpy()
        day = f.index.floor('D')
        armed = 0
        armed_at = -100
        for i in range(1, len(f)):
            if day[i] != day[i-1]:
                armed = 0
            if armed and i-armed_at > 10:
                armed = 0
            slope = int(m[i] > m[i-1])-int(m[i] < m[i-1])
            if armed and i > armed_at and slope == armed:
                touch = lows[i] <= m[i] if armed > 0 else highs[i] >= m[i]
                if touch:
                    s.iloc[i, s.columns.get_loc('side')] = armed
                    s.iloc[i, s.columns.get_loc('trigger')] = highs[i]+tick if armed > 0 else lows[i]-tick
                    s.iloc[i, s.columns.get_loc('stop')] = lows[i] if armed > 0 else highs[i]
                    armed = 0
                    continue
            if not armed and a[i] > 30 and a[i] > a[i-1] and slope:
                armed, armed_at = slope, i
        return s
    elif rule == 'NR7':
        width = h-l
        nr = width.lt(width.shift().rolling(6).min())
        long = nr & (l > f.ema20).rolling(7).sum().eq(7)
        short = nr & (h < f.ema20).rolling(7).sum().eq(7)
        s.loc[long, ['trigger', 'stop']] = np.column_stack((h[long], l[long]))
        s.loc[short, ['trigger', 'stop']] = np.column_stack((l[short], h[short]))
    elif rule == 'ICHI_ST':
        above = c.gt(f.cloud_a) & c.gt(f.cloud_b)
        below = c.lt(f.cloud_a) & c.lt(f.cloud_b)
        long = f.st_direction.eq(1) & f.st_direction.shift().eq(-1) & above
        short = f.st_direction.eq(-1) & f.st_direction.shift().eq(1) & below
    elif rule == 'RSI3_2C':
        same_day = f.index.floor('D') == f.index.floor('D').to_series(index=f.index).shift()
        long = (c.shift() > o.shift()) & (c > o) & (c > h.shift()) & (l > l.shift())
        long &= (l > f.ema50) & (l.shift() > f.ema50.shift()) & (f.rsi3 > 80) & same_day
        short = (c.shift() < o.shift()) & (c < o) & (c < l.shift()) & (h < h.shift())
        short &= (h < f.ema50) & (h.shift() < f.ema50.shift()) & (f.rsi3 < 20) & same_day
        s.loc[long, ['trigger', 'stop']] = np.column_stack((h[long]+tick, l.shift()[long]-tick))
        s.loc[short, ['trigger', 'stop']] = np.column_stack((l[short]-tick, h.shift()[short]+tick))
    elif rule == 'PDH_PDL':
        long = (c > f.pdh) & (c.shift() <= f.pdh)
        short = (c < f.pdl) & (c.shift() >= f.pdl)
        s.loc[long, 'stop'] = f.pdl[long]
        s.loc[short, 'stop'] = f.pdh[short]
    s.loc[long, 'side'] = 1
    s.loc[short, 'side'] = -1
    return s


class ArrayFeed(bt.feed.DataBase):
    def start(self):
        super().start()
        self.rows = iter(self.p.dataname)

    def _load(self):
        row = next(self.rows, None)
        if row is None:
            return False
        dt, o, h, l, c, v = row
        self.lines.datetime[0] = dt
        self.lines.open[0], self.lines.high[0], self.lines.low[0] = o, h, l
        self.lines.close[0], self.lines.volume[0] = c, v
        self.lines.openinterest[0] = 0
        return True


# Reuse the existing one-tick through-limit adapter; all execution prices and
# commissions are still determined by BackBroker's native methods.
class IntradayBroker(type(_through_limit_broker(bt.brokers.BackBroker, None))):
    def __init__(self):
        super().__init__()
        self.strategy = None

    def next(self):
        # Settlement is BEFORE orders at that open: an old position closing at
        # 00:00 pays; an entry filled exactly at settlement does not pay.
        st = self.strategy
        i = len(st.data)-1
        ms = int(st.ms[i])
        if st.p.funding_enabled and ms % FUND_MS == 0:
            position = self.getposition(st.data)
            if position.size:
                date = st.dates[i]
                rate = st.p.funding.get(date)
                if rate is None:
                    raise ValueError(f'持仓结算缺少 {date} 日平均资金费率；不以零替代')
                notional = abs(position.size)*float(st.data.open[0])
                flow = -math.copysign(1, position.size)*rate*notional
                self.add_cash(flow)
                st.record_funding(ms, rate, notional, flow)
        super().next()

    def _bracketize(self, order, cancel=False):
        super()._bracketize(order, cancel=cancel)
        if cancel or order.status != bt.Order.Completed or order.info.get('role') != 'entry':
            return
        stop = order.owner.broker_entry_filled(order)
        if stop is None:
            return
        # Schedule only the new SL on the entry bar. Use native Stop matching,
        # gap/slippage/commission/accounting/OCO, never a synthetic P&L patch.
        # Entry-bar TP is ignored (conservative, as in backtrader_adapter).
        # Keep SL ahead of TP in the queue on every later bar as well.
        at = self.pending.index(stop)
        self.pending.remove(stop)
        self._try_exec(stop)
        if stop.alive():
            self.pending.insert(at, stop)
        elif stop.status == bt.Order.Completed:
            super()._bracketize(stop)


class DailyEquity(bt.Analyzer):
    def start(self):
        self.daily = {}

    def next(self):
        st = self.strategy
        i = len(st.data)-1
        self.daily[st.dates[i]] = {'broker_value': self.strategy.broker.getvalue(),
                                   'close': float(st.data.close[0]), 'last_open_ms': int(st.ms[i])}

    def get_analysis(self):
        return self.daily


def trailing_stop(entry, initial_stop, old_stop, peak, side):
    """Fixed entry-price 0.1% steps; only improves the previous stop."""
    step = entry*0.001
    steps = max(0, math.floor((side*(peak-entry)+step*1e-10)/step))
    candidate = initial_stop+side*steps*step
    return max(old_stop, candidate) if side > 0 else min(old_stop, candidate)


class IntradayStrategy(bt.Strategy):
    params = (('frame', None), ('setups', None), ('rule', None), ('minutes', 5),
              ('funding', None), ('funding_enabled', False), ('signal_end_ms', None),
              ('trace', False))

    def __init__(self):
        self.f = {k: self.p.frame[k].to_numpy() for k in self.p.frame.columns}
        self.s = {k: self.p.setups[k].to_numpy() for k in self.p.setups.columns}
        self.ms = self.p.frame.index.as_unit('ms').asi8
        self.dates = np.asarray(self.p.frame.index.strftime('%Y-%m-%d'))
        self.entry_order = self.close_order = None
        self.exits = []
        self.entry_i = None
        self.active = None
        self.trades = []
        self.by_id = {}
        self.orders = []
        self.funding_events = []
        self.stop_updates = []
        self.entries_by_day = defaultdict(int)
        self.directions_by_day = defaultdict(set)
        self.commission_paid = 0.0
        self.funding_paid = 0.0  # positive = cost, negative = receipt
        self.sequence = 0
        self.broker.strategy = self

    def _cancel_exits(self):
        for order in self.exits:
            if order.alive():
                self.cancel(order)
        self.exits = []

    def _place_exits(self, row):
        self._cancel_exits()
        submit = self.sell if row['side'] > 0 else self.buy
        kw = dict(size=row['size'], tradeid=row['id'], _checksubmit=False)
        stop = submit(exectype=bt.Order.Stop, price=row['stop'], role='exit', reason='sl', **kw)
        self.exits = [stop]
        if row.get('target') is not None:
            target = submit(exectype=bt.Order.Limit, price=row['target'], oco=stop,
                            role='exit', reason='tp', **kw)
            self.exits.append(target)
        return stop

    def broker_entry_filled(self, order):
        """Called by the scheduling adapter after native entry accounting."""
        i = len(self.data)-1
        meta = order.info['setup']
        price = order.executed.price
        side = meta['side']
        stop = meta['stop']
        target = None
        if self.p.rule == 'VWAP_RECLAIM':
            stop = price-side*1.5*meta['atr']
            target = price+side*2*meta['atr']
        elif self.p.rule in ('HOLY_GRAIL', 'NR7', 'RSI3_2C'):
            risk = side*(price-stop)
            if risk > 0:
                target = price+side*2*risk
        row = {'id': order.tradeid, 'side': side, 'signal_ms': meta['signal_ms'],
               'entry_ms': int(self.ms[i]), 'entry_price': price,
               'entry_reference': meta['reference'], 'size': abs(order.executed.size),
               'actual_entry_notional': abs(order.executed.size)*price,
               'stop': stop, 'initial_stop': stop, 'target': target,
               'signal_atr': meta['atr'], 'funding_cashflow': 0.0,
               'entry_commission': order.executed.comm, 'open': True}
        self.trades.append(row)
        self.by_id[row['id']] = row
        self.active = row
        self.entry_order = None
        self.entries_by_day[self.dates[i]] += 1
        self.directions_by_day[self.dates[i]].add(side)
        self.peak = price
        if self.p.rule == 'ICHI_ST':
            return None
        return self._place_exits(row)

    def record_funding(self, ms, rate, notional, flow):
        self.funding_paid -= flow
        self.funding_events.append({'time_ms': ms, 'rate': rate, 'notional': notional,
                                    'cashflow': flow, 'trade_id': self.active['id']})
        self.active['funding_cashflow'] += flow

    def notify_order(self, order):
        if order.status == bt.Order.Completed:
            self.commission_paid += order.executed.comm
            if order.info.get('role') == 'exit':
                row = self.by_id[order.tradeid]
                row.update(exit_ms=int(self.ms[len(self.data)-1]), exit_price=order.executed.price,
                           exit_reason=order.info.get('reason'), exit_commission=order.executed.comm)
                if not self.position:
                    self._cancel_exits()
                    self.close_order = None
        elif order.status in (bt.Order.Margin, bt.Order.Rejected):
            raise RuntimeError(f'原生 broker 拒绝订单 {order.ref}: {order.getstatusname()}')
        if order.status in (bt.Order.Completed, bt.Order.Canceled, bt.Order.Expired):
            if order.info.get('role') == 'entry' and self.entry_order is not None and order.ref == self.entry_order.ref:
                self.entry_order = None
            self.orders.append({'ref': order.ref, 'trade_id': order.tradeid,
                                'role': order.info.get('role'), 'reason': order.info.get('reason'),
                                'type': order.getordername(), 'status': order.getstatusname(),
                                'bar_open_ms': int(round(bt.num2date(order.executed.dt, tz=timezone.utc, naive=False).timestamp()*1000)),
                                'created_price': order.created.price,
                                'fill_price': order.executed.price if order.status == bt.Order.Completed else None,
                                'size': order.executed.size, 'commission': order.executed.comm,
                                'created_bar': order.info.get('created_bar')})

    def notify_trade(self, trade):
        if trade.isclosed:
            row = self.by_id[trade.tradeid]
            row.update(open=False, gross_pnl=trade.pnl, pnl_after_commission=trade.pnlcomm,
                       commission=trade.commission,
                       net_pnl=trade.pnlcomm+row['funding_cashflow'],
                       net_bps=(trade.pnlcomm+row['funding_cashflow'])/CAPITAL*10000,
                       bars_held=trade.barlen)
            if self.active and self.active['id'] == trade.tradeid:
                self.active = None

    def _flatten(self, reason):
        if self.close_order is None and self.position:
            self._cancel_exits()
            self.close_order = self.close(tradeid=self.active['id'], role='exit', reason=reason)

    def next(self):
        i = len(self.data)-1
        ms = int(self.ms[i])
        last = ms % DAY_MS == DAY_MS-self.p.minutes*60_000
        if self.entry_order is not None and self.entry_order.alive():
            if last or i-self.entry_i >= 3:
                self.cancel(self.entry_order)
                self.entry_order = None
        if last:
            self._flatten('session')
            return
        if self.p.signal_end_ms is not None and ms >= self.p.signal_end_ms:
            return
        if self.position:
            if self.close_order is not None:
                return
            row = self.active
            if self.p.rule == 'ICHI_ST':
                if row['side']*(self.f['close'][i]-self.f['supertrend'][i]) < 0:
                    self._flatten('supertrend')
            elif self.p.rule == 'PDH_PDL':
                self.peak = (max(self.peak, self.f['high'][i]) if row['side'] > 0
                             else min(self.peak, self.f['low'][i]))
                new = trailing_stop(row['entry_price'], row['initial_stop'], row['stop'], self.peak, row['side'])
                if row['side']*(new-row['stop']) > 1e-10:
                    row['stop'] = new
                    self._place_exits(row)  # close-time update; native execution starts next bar
                    if self.p.trace:
                        self.stop_updates.append({'bar_open_ms': ms, 'stop': new, 'trade_id': row['id']})
            return
        if self.entry_order is not None:
            return
        side = int(self.s['side'][i])
        day = self.dates[i]
        if not side:
            return
        if self.p.rule == 'RSI3_2C' and self.entries_by_day[day] >= 2:
            return
        if self.p.rule == 'PDH_PDL' and side in self.directions_by_day[day]:
            return
        trigger = self.s['trigger'][i]
        reference = trigger if math.isfinite(trigger) else self.f['close'][i]
        self.sequence += 1
        meta = {'side': side, 'reference': float(reference), 'stop': float(self.s['stop'][i]),
                'atr': float(self.s['atr'][i]), 'signal_ms': ms+self.p.minutes*60_000}
        submit = self.buy if side > 0 else self.sell
        kw = {'exectype': bt.Order.Stop, 'price': trigger} if math.isfinite(trigger) else {'exectype': bt.Order.Market}
        self.entry_order = submit(size=CAPITAL/reference, tradeid=self.sequence,
                                  role='entry', setup=meta, created_bar=i, **kw)
        self.entry_i = i


def _sharpe(a):
    a = np.asarray(a, dtype=float)
    return float(np.mean(a)/np.std(a, ddof=1)*math.sqrt(365)) if len(a) > 1 and np.std(a, ddof=1) > 0 else 0.0


def metrics(daily, trades, max_money_dd, commission, funding):
    pnl = np.asarray([d['pnl'] for d in daily])
    total = float(pnl.sum())
    factor = 1+total/CAPITAL
    cagr = (factor**(365/len(daily))-1)*100 if len(daily) and factor > 0 else None
    closed = [t for t in trades if not t['open']]
    wins = sum(t['net_pnl'] > 0 for t in closed)
    win_pnl = sum(max(0., t['net_pnl']) for t in closed)
    loss_pnl = -sum(min(0., t['net_pnl']) for t in closed)
    start, end = pd.Timestamp(daily[0]['date']), pd.Timestamp(daily[-1]['date'])
    midpoint = start+(end-start)/2
    halves = []
    for first in (True, False):
        selected = [d for d in daily if (pd.Timestamp(d['date']) <= midpoint) == first]
        a = [d['pnl']/CAPITAL for d in selected]
        halves.append({'start': selected[0]['date'] if selected else None,
                       'end': selected[-1]['date'] if selected else None,
                       'return_pct': sum(a)*100, 'sharpe': _sharpe(a), 'days': len(a)})
    return {'return_pct': total/CAPITAL*100, 'cagr_pct': cagr,
            'sharpe': _sharpe(pnl/CAPITAL), 'max_drawdown_pct': max_money_dd/CAPITAL*100,
            'trades': len(closed), 'entries': len(trades), 'open_trades': len(trades)-len(closed),
            'win_rate_pct': wins/len(closed)*100 if closed else 0.,
            'profit_factor': win_pnl/loss_pnl if loss_pnl else None,
            'profit_factor_infinite': bool(win_pnl and not loss_pnl),
            'avg_net_bps': sum(t['net_bps'] for t in closed)/len(closed) if closed else 0.,
            'commission_total': commission, 'funding_total': funding,
            'first_half': halves[0], 'second_half': halves[1],
            'midpoint': midpoint.isoformat(), 'net_pnl': total}


def run_backtest(frame, rule, symbol='BTCUSDT', cost='real', funding=None,
                 start=None, end=None, setups=None, trace=False, features=None):
    """end is inclusive UTC signal date; one extra next-day bar executes flatten."""
    f = features if features is not None else build_features(frame)
    proposals = setups if setups is not None else build_setups(f, rule, TICKS[symbol])
    lower = pd.Timestamp(start, tz='UTC') if start else f.index[0]
    upper = pd.Timestamp(end, tz='UTC')+pd.DateOffset(days=1) if end else None
    mask = f.index >= lower
    if upper is not None:
        mask &= f.index <= upper
    f, proposals = f.loc[mask], proposals.loc[mask]
    if f.empty:
        raise ValueError('回测窗口没有 K 线')
    commission, slippage, use_funding = COSTS[cost]
    rows = np.column_stack((np.asarray([bt.date2num(d) for d in f.index.to_pydatetime()]),
                            f[['open', 'high', 'low', 'close', 'volume']].to_numpy()))
    cerebro = bt.Cerebro(stdstats=False)
    broker = IntradayBroker()
    broker.limit_tick = TICKS[symbol]
    cerebro.setbroker(broker)
    broker.setcash(BROKER_CASH)
    broker.setcommission(commission=commission, commtype=bt.CommInfoBase.COMM_PERC,
                         stocklike=True, percabs=True)
    broker.set_slippage_perc(slippage, slip_open=True, slip_limit=True, slip_match=True, slip_out=False)
    cerebro.adddata(ArrayFeed(dataname=rows, timeframe=bt.TimeFrame.Minutes, compression=TIMEFRAMES[rule]))
    cerebro.addstrategy(IntradayStrategy, frame=f, setups=proposals, rule=rule,
                        minutes=TIMEFRAMES[rule], funding=funding or {}, funding_enabled=use_funding,
                        signal_end_ms=int(upper.timestamp()*1000) if upper is not None else None, trace=trace)
    cerebro.addanalyzer(bt.analyzers.TradeAnalyzer, _name='native_trades')
    cerebro.addanalyzer(bt.analyzers.DrawDown, _name='native_drawdown', fund=False)
    cerebro.addanalyzer(bt.analyzers.TimeReturn, _name='native_time_return', timeframe=bt.TimeFrame.Days, fund=False)
    cerebro.addanalyzer(DailyEquity, _name='daily_equity')
    strategy = cerebro.run(runonce=True)[0]
    daily = []
    previous_value = BROKER_CASH
    # Aligned buy-and-hold: first evaluation day's open, then previous day's close.
    previous_close = float(f.open.iloc[0])
    for date, row in strategy.analyzers.daily_equity.get_analysis().items():
        pnl = row['broker_value']-previous_value
        r = pnl/CAPITAL
        daily.append({'date': date, 'broker_value': row['broker_value'],
                      'reporting_equity': CAPITAL+row['broker_value']-BROKER_CASH,
                      'pnl': pnl, 'return': r, 'log_return': math.log1p(r) if r > -1 else None,
                      'buy_hold_log_return': math.log(row['close']/previous_close),
                      'last_open_ms': row['last_open_ms']})
        previous_value, previous_close = row['broker_value'], row['close']
    native_trades = strategy.analyzers.native_trades.get_analysis()
    dd = strategy.analyzers.native_drawdown.get_analysis()
    result = {'rule': rule, 'symbol': symbol, 'cost': cost, 'minutes': TIMEFRAMES[rule],
              'start': f.index[0].isoformat(), 'last_bar_open': f.index[-1].isoformat(),
              'signal_end': upper.isoformat() if upper is not None else None,
              'bars': len(f), 'metrics': metrics(daily, strategy.trades, dd.max.moneydown,
                                               strategy.commission_paid, strategy.funding_paid),
              'daily': daily, 'trades': strategy.trades, 'orders': strategy.orders,
              'funding_events': strategy.funding_events,
              'native': {'TradeAnalyzer': native_trades, 'DrawDown': dd,
                         'TimeReturn': {k.isoformat(): v for k, v in strategy.analyzers.native_time_return.get_analysis().items()}},
              'pending_entry': bool(strategy.entry_order and strategy.entry_order.alive()),
              'pending_close': bool(strategy.close_order and strategy.close_order.alive())}
    # Exact broker reconciliation includes open MTM and actual entry commissions.
    closed_net = sum(t['net_pnl'] for t in strategy.trades if not t['open'])
    result['reconciliation'] = {'broker_net_pnl': broker.getvalue()-BROKER_CASH,
                                'closed_trade_net_pnl': closed_net,
                                'open_mark_to_market_and_cost': broker.getvalue()-BROKER_CASH-closed_net,
                                'native_closed_trades': native_trades.get('total', {}).get('closed', 0)}
    if all(d['log_return'] is not None for d in daily):
        result['audit'] = audit_returns([d['log_return'] for d in daily],
                                        [d['buy_hold_log_return'] for d in daily], 365)
    else:
        result['audit'] = {'verdict': 'UNDEFINED_DAILY_LOG_RETURN', 'reason': '存在日亏损 ≥ USD1000'}
    if trace:
        result['stop_updates'] = strategy.stop_updates
    return result


def infer_source_timezone(items, frame, sample_size=20):
    """Infer string UTC offset from FIRST source entry prices, never tune returns.

    Offset means source local clock = UTC + offset. Compare unslipped entry
    price to the Binance bar open over all quarter-hour offsets [-14h,+14h].
    """
    sample = items[:sample_size]
    scores = []
    for offset in range(-14*60, 14*60+1, 15):
        errors = []
        for t in sample:
            stamp = pd.Timestamp(t['entryTime'], tz='UTC').value-offset*60_000_000_000
            dt = pd.Timestamp(stamp, tz='UTC')
            price = frame.open.get(dt)
            if price is not None:
                reference = t.get('entryPriceWithoutSlippage')
                if reference is None:
                    side = 1 if t['transactionType'].lower() == 'buy' else -1
                    reference = t['entryPrice']/(1+side*0.0001)
                errors.append(abs(float(price)-reference)/reference*10000)
        scores.append({'offset_minutes': offset, 'observations': len(errors),
                       'exact_within_tick': sum(e <= .1/float(frame.open.iloc[0])*10000+1e-8 for e in errors),
                       'median_abs_error_bps': float(np.median(errors)) if errors else math.inf})
    ranked = sorted(scores, key=lambda x: (x['observations'] != len(sample), x['median_abs_error_bps']))
    best = ranked[0]
    offset = best['offset_minutes']
    examples = []
    for t in sample[:5]:
        stamp = pd.Timestamp(t['entryTime'], tz='UTC').value-offset*60_000_000_000
        dt = pd.Timestamp(stamp, tz='UTC')
        examples.append({'source_entry_time': t['entryTime'], 'inferred_utc': dt.isoformat(),
                         'source_unslipped_entry': t.get('entryPriceWithoutSlippage'),
                         'local_5m_open': frame.open.get(dt)})
    return {'offset_minutes': offset, 'sample_size': len(sample), 'best': best,
            'runner_up': ranked[1], 'examples': examples, 'all_candidates': scores,
            'status': 'PRICE_SUPPORTED_ASSUMPTION' if best['median_abs_error_bps'] < .1 else 'UNCERTAIN'}


def match_entries(local, source, offset_minutes=0, tolerance_ms=300_000):
    ours = sorted((t['entry_ms'], t['side']) for t in local)
    theirs = sorted((pd.Timestamp(t['entryTime'], tz='UTC').value//1_000_000-offset_minutes*60_000,
                      1 if t['transactionType'].lower() == 'buy' else -1) for t in source)
    i = j = matched = same_side = exact = 0
    examples = []
    while i < len(ours) and j < len(theirs):
        a, b = ours[i], theirs[j]
        if a[0] < b[0]-tolerance_ms:
            i += 1
        elif b[0] < a[0]-tolerance_ms:
            j += 1
        else:
            matched += 1
            same_side += int(a[1] == b[1])
            exact += int(a[0] == b[0])
            if len(examples) < 10:
                examples.append({'local_entry_ms': a[0], 'source_entry_ms_utc': int(b[0]),
                                 'difference_minutes': (a[0]-b[0])/60000, 'same_side': a[1] == b[1]})
            i += 1
            j += 1
    return {'tolerance_minutes': tolerance_ms/60000, 'one_to_one': True,
            'matched': matched, 'exact_time_matches_in_matching': exact,
            'same_side_in_matching': same_side, 'local_entries': len(ours), 'source_entries': len(theirs),
            'local_match_pct': matched/len(ours)*100 if ours else 0.,
            'source_match_pct': matched/len(theirs)*100 if theirs else 0., 'examples': examples}


def admission(results, rules=RULES):
    """Frozen gate, evaluated in the declared order with the SAME candidate coin."""
    output = {}
    for rule in rules:
        coins = {r['symbol']: r['metrics'] for r in results if r['rule'] == rule and r['cost'] == 'real'}
        if not all(s in coins for s in SYMBOLS):
            output[rule] = {'status': 'FAIL', 'first_failed_condition': '缺少 BTC/ETH 两币完整结果，无法满足双币准入条件',
                            'qualified_symbol': None}
            continue
        candidates = list(SYMBOLS)
        history = []
        for k, text in enumerate(ADMISSION):
            kept = []
            for symbol in candidates:
                m = coins[symbol]
                other = coins[SYMBOLS[1] if symbol == SYMBOLS[0] else SYMBOLS[0]]
                checks = (m['sharpe'] >= .8,
                          m['first_half']['sharpe'] > 0 and m['second_half']['sharpe'] > 0,
                          m['avg_net_bps'] >= 5, other['return_pct'] > 0)
                if checks[k]:
                    kept.append(symbol)
            history.append({'condition': text, 'remaining_candidates': kept})
            candidates = kept
            if not candidates:
                break
        output[rule] = {'status': 'PASS' if candidates else 'FAIL',
                        'first_failed_condition': None if candidates else history[-1]['condition'],
                        'qualified_symbol': candidates[0] if candidates else None,
                        'condition_checks': history}
    return output


def _number(value, precision=2):
    return f'{value:.{precision}f}' if value is not None and math.isfinite(value) else 'N/A'


def reproduction_text(rep):
    m, p, match = rep['backtest']['metrics'], rep['source_performance'], rep['entry_matching']
    tz = rep['timezone']
    return (f"VWAP_RECLAIM / BTCUSDT / volrix / 2023-09-08..2026-10-27："
            f"本地交易数 {m['trades']}、胜率 {_number(m['win_rate_pct'])}%、PF {_number(m['profit_factor'], 3)}、"
            f"收益 {_number(m['return_pct'])}%；Volrix 交易数 {p['totalTrades']}、胜率 {_number(p['winRate'])}%、"
            f"PF {_number(p['profitFactor'], 3)}、收益 {_number(p['returnPercent'])}%。"
            f"入场时间 ±5 分钟一对一匹配 {match['matched']}/{match['local_entries']}"
            f"（本地覆盖 {_number(match['local_match_pct'])}%，Volrix 覆盖 {_number(match['source_match_pct'])}%）。"
            f"前 {tz['sample_size']} 笔价格比对推断源时间为 UTC{tz['offset_minutes']/60:+g}；"
            f"该偏移的开盘价绝对误差中位数 {_number(tz['best']['median_abs_error_bps'], 4)} bps。"
            '本次结果不构成精确复现。')


def admission_text(gates):
    return '\n'.join(f"- {rule}: {v['status']}；"+
                     (f"达标币种 {v['qualified_symbol']}。" if v['status'] == 'PASS'
                      else f"首个不满足条件：{v['first_failed_condition']}。")
                     for rule, v in gates.items())


def render_report(payload):
    meta, rep, results = payload['metadata'], payload['reproduction'], payload['results']
    lines = ['# Volrix 日内价格规则 Backtrader 回测', '',
             '## 预先声明的准入条件', '',
             'PASS 当且仅当 real 口径下：BTC 或 ETH 至少一个全样本 Sharpe ≥ 0.8，'
             '同一币种前后两半 Sharpe 都 > 0、平均每笔净收益 ≥ 5 bps，且另一个币全样本收益 > 0。'
             '依此顺序报告首个不满足条件；阈值在运行前固定，未按结果调整。', '',
             '## 执行与数据口径', '',
             f"引擎：Backtrader {bt.__version__}；每币每规则独立 Cerebro / BackBroker，"
             '初始 broker 现金 USD1,000,000。每笔数量为 USD1000 / 信号已知的入场参考价；'
             '实际名义金额随跳空及滑点偏离 USD1000，不加仓。', '',
             f"主表信号窗口：{meta['main_start']}..{meta['main_end']}（UTC）；"
             f"额外读取 {meta['execution_date']} 首根，用于最后一日强平。{meta['coverage_note']}", '',
             'volrix：双边手续费 0、Market/Stop 滑点 0.01%、无资金费。'
             'real：每次成交手续费 0.05%、Market/Stop 滑点 0.03%、计资金费。'
             'TP 限价按现有适配器要求穿过 1 tick；盘中按限价成交，'
             '跳空优价成交按原生开盘滑点处理，成交价仍受限价约束。', '',
             '信号只用已收盘 bar；Market/Stop 入场最早下一根生效。已挂 SL 排在 TP 前，'
             '同根同时触及 SL/TP 先止损，原生 OCO 取消另一单。'
             '入场成交后按实际成交价创建保护单：调度适配器只在入场根调用原生 Stop 检查 SL，'
             '成交、跳空、滑点、手续费、持仓和盈亏均由 BackBroker 执行；入场根 TP 不回溯。', '',
             'UTC 日最后一根（5m 23:55 / 15m 23:45）收盘提交 Market 强平，'
             '下一日首根开盘成交，存在跨日执行偏差；最后一根不再开仓并撤销待触发入场。'
             'Stop 入场在后续第 1、2、3 根可触发，第 3 根收盘仍未成交则撤单。', '',
             '收益、平均每笔 bps、回撤均使用 USD1000 固定资本分母；回撤为原生 DrawDown.max.moneydown / 1000，'
             '含盘中各 bar 收盘权益。日收益为原生 broker 日权益变化 / 1000，Sharpe 使用这些日收益（样本标准差，√365）。'
             'CAGR = (1+累计净盈亏/1000)^(365/日数)−1；终值非正则 N/A。'
             'audit_returns 输入 ln(1+固定资本日收益)，与同日期买入持有日对数收益比较，默认按时间 60/40；'
             '它不是负累计净值的对数，也不将固定名义交易改成复投。前后两半按日历日期中点切分，'
             '各半收益仍除以1000，交易不重启。', '',
             'Wilder ATR/ADX 的 TR 首根以 high−low 种子参与，DM 首根为0；EMA 用首 n 根 SMA 种子；'
             'RSI 用3个收盘差 Wilder 平滑（全平为50）。Ichimoku 云向后取26根前计算值；'
             'Supertrend 首个 ATR7 有效值初始化为多头，不将初始化算翻转。'
             'Holy Grail 先按 SMA20 斜率武装方向，再在之后1..10根内接受同斜率触及，触发价在触及根极值外1 tick；'
             'NR7 的7根分别与各自 EMA20 比较，按任务表以信号极值挂 Stop；'
             'RSI 两根限定同一 UTC 日，最多2次实际成交入场。'
             'PDH/PDL 仅取前一完整 UTC 日，0.1%步长固定为实际成交价的0.1%，'
             '从已收盘 bar 的有利极值累计整数步，只往有利方向移动，下一根生效。', '',
             '资金费在 UTC 00:00 / 08:00 / 16:00 撮合前，以当时开盘价×持仓数量为名义金额，'
             '使用结算日平均费率，通过 broker.add_cash 记账；多头付正费率、空头收正费率。'
             '结算时已有、随后在同一开盘强平的持仓仍计费；恰在结算开盘新入场的持仓不计当次费。'
             '表中资金费为净支出（负数表示净收入）；缺少日费率不会默认为零。', '',
             '## 复现核对', '', reproduction_text(rep), '',
             '| 指标 | 本地 Backtrader | Volrix performance |',
             '|---|---:|---:|']
    m, p = rep['backtest']['metrics'], rep['source_performance']
    for label, left, right in [('交易数', m['trades'], p['totalTrades']),
                                ('胜率 %', m['win_rate_pct'], p['winRate']),
                                ('PF', m['profit_factor'], p['profitFactor']),
                                ('收益 %', m['return_pct'], p['returnPercent'])]:
        lines.append(f'| {label} | {int(left)} | {int(right)} |' if label == '交易数'
                     else f'| {label} | {_number(left, 3)} | {_number(right, 3)} |')
    lines += ['', f"源成交两页共 {rep['source_trade_count']} 笔；时间匹配不重复使用同一笔，"
              f"匹配中同方向 {rep['entry_matching']['same_side_in_matching']} 笔。"
              '先仅按前20笔未经滑点的源入场价与本地5m开盘价，检验 UTC−14..UTC+14 的15分钟偏移，'
              '再固定最佳偏移做时间核对；没有用策略收益选择时区。', '',
              '| 源时间字符串 | 推断 UTC | 源未滑点价 | 本地5m开盘价 |',
              '|---|---|---:|---:|']
    for ex in rep['timezone']['examples']:
        lines.append(f"| {ex['source_entry_time']} | {ex['inferred_utc']} | {_number(ex['source_unslipped_entry'])} | {_number(ex['local_5m_open'])} |")
    lines += ['', '差异包括：Volrix 明细的退出可在分钟内（如 04:43:59），本任务只有5m OHLC；'
              '本地盘中 TP 按限价成交，源明细目标价则显示退出滑点；入场根保守止损、'
              '强平延迟、指标种子和平台未公开的实现细节也会改变后续交易序列。'
              '价格核对支持 UTC 假设，不能证明源平台所有内部时钟与成交路径一致。', '',
              '## 主表（real）', '',
              '| 规则 | 币 | 收益% | CAGR% | Sharpe | MaxDD% | 交易数 | 胜率% | PF | 平均净bps | 手续费USD | 资金费USD |',
              '|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|']
    for r in results:
        m = r['metrics']
        values = [str(m[k]) if k == 'trades' else _number(m[k], 3 if k in ('sharpe', 'profit_factor') else 2) for k in
                  ('return_pct', 'cagr_pct', 'sharpe', 'max_drawdown_pct', 'trades', 'win_rate_pct',
                   'profit_factor', 'avg_net_bps', 'commission_total', 'funding_total')]
        lines.append(f"| {r['rule']} | {r['symbol']} | "+' | '.join(values)+' |')
    lines += ['', '## 前后两半（real，按日期中点）', '',
              '| 规则 | 币 | 前半区间 | 前半收益% | 前半Sharpe | 后半区间 | 后半收益% | 后半Sharpe |',
              '|---|---|---|---:|---:|---|---:|---:|']
    for r in results:
        a, b = r['metrics']['first_half'], r['metrics']['second_half']
        lines.append(f"| {r['rule']} | {r['symbol']} | {a['start']}..{a['end']} | {_number(a['return_pct'])} | {_number(a['sharpe'],3)} | {b['start']}..{b['end']} | {_number(b['return_pct'])} | {_number(b['sharpe'],3)} |")
    lines += ['', '## audit_returns 裁决', '',
              '按相同日期对齐买入持有，不扣基准手续费和资金费；策略日权益包含实际成本。'
              '下表 CAGR 来自审计用日对数收益的复合，与主表固定资本累计收益的 CAGR 口径不同。'
              'C/S = CAGR% / Sharpe。', '',
              '| 规则 | 币 | 裁决 | 前60%策略 C/S | 前60%持有 C/S | 后40%策略 C/S | 后40%持有 C/S |',
              '|---|---|---|---:|---:|---:|---:|']
    for r in results:
        a = r['audit']
        fields = []
        for prefix in ('train', 'holdout'):
            for suffix in ('', '_bench'):
                c, s = a.get(prefix+'_cagr'+suffix), a.get(prefix+'_sharpe'+suffix)
                fields.append(f'{_number(c*100 if c is not None else None)} / {_number(s,3)}')
        lines.append(f"| {r['rule']} | {r['symbol']} | {a['verdict']} | "+' | '.join(fields)+' |')
    lines += ['', '## PASS/FAIL 汇总', '', admission_text(payload['admission']), '',
              '## 局限', '',
              '- 5m 替代 NR7、Ichimoku 原3m和 PDH/PDL 原1m；15m 只用3根完整5m重采样。OHLC 无法确定根内路径，'
              'Stop/Limit 退出时间仅能定位所在 bar，采用先止损及入场根忽略TP的保守约定。',
              '- 加密会话定义为 UTC 日；跨日强平延迟至下一日开盘，会增加一次00:00资金费敞口。'
              '时区是价格核对支持的假设；不宣称精确复现 Volrix。',
              '- 日平均资金费替代三次实际历史结算费率，以边界开盘价近似结算标记价；'
              '数据末端缺口及任何前推处理均在数据口径注明。',
              '- 未建模保证金、杠杆约束、强平、盘口深度、部分成交、交易所最小数量/数量精度和动态费用。'
              '固定1000资本只是报告分母，累计亏损可超过100%；大现金 broker 不会因此自动停止交易。',
              '- 同一历史样本上的门槛检查与时间切分不能替代独立样本外验证；6规则×2币存在多重比较，'
              '未做参数择优、网格寻优或显著性修正。', '',
              '## 账务核对', '',
              '| 规则 | 币 | broker净盈亏 | 平仓交易净盈亏含资金费 | 未平仓盯市及成本残差 | 原生平仓笔数 |',
              '|---|---|---:|---:|---:|---:|']
    for r in results:
        c = r['reconciliation']
        lines.append(f"| {r['rule']} | {r['symbol']} | {_number(c['broker_net_pnl'],6)} | {_number(c['closed_trade_net_pnl'],6)} | {_number(c['open_mark_to_market_and_cost'],6)} | {c['native_closed_trades']} |")
    return '\n'.join(lines)+'\n'


def clean_json(value):
    if isinstance(value, dict):
        return {str(k): clean_json(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [clean_json(v) for v in value]
    if isinstance(value, (float, np.floating)):
        return float(value) if math.isfinite(value) else None
    if isinstance(value, np.integer):
        return int(value)
    if isinstance(value, np.bool_):
        return bool(value)
    return value


def _choices(values, allowed, label):
    result = [item.upper() for value in values for item in value.split(',') if item]
    unknown = set(result)-set(allowed)
    if unknown or not result:
        raise ValueError(f'{label} 参数无效：{sorted(unknown)}；允许 {",".join(allowed)}')
    return list(dict.fromkeys(result))


def load_funding(path):
    path = Path(path)
    if not path.is_file():
        raise FileNotFoundError(f'缺少本地资金费文件：{path}')
    values = {str(day): float(rate) for day, rate in json.loads(path.read_text())}
    if not values or not all(math.isfinite(v) for v in values.values()):
        raise ValueError(f'资金费文件为空或含非有限费率：{path}')
    return values


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--out', type=Path)
    parser.add_argument('--json', type=Path)
    parser.add_argument('--rules', nargs='+', default=list(RULES))
    parser.add_argument('--symbols', nargs='+', default=list(SYMBOLS))
    parser.add_argument('--data-root', type=Path, default=DATA_ROOT)
    parser.add_argument('--start', help='UTC 首个信号日（默认数据起点；指标保留此前预热）')
    parser.add_argument('--end', help='UTC 最后信号日（包含；另读次日首根执行强平）')
    parser.add_argument('--funding-tail', choices=('complete', 'last'), default='complete',
                        help='complete: 共同完整费率窗口；last: 尾部缺失费率沿用最近日均值（明确标记）')
    args = parser.parse_args(argv)
    try:
        rules = _choices(args.rules, RULES, 'rules')
        symbols = _choices(args.symbols, SYMBOLS, 'symbols')
        root = args.data_root
        frames = {s: load_bars(root/'volrix/klines'/f'{s}_5m.json') for s in dict.fromkeys([*symbols, 'BTCUSDT'])}
        funding = {s: load_funding(root/'factor_mining/crypto/alt_daily/funding'/f'{s}.json') for s in symbols}
        # Last signal day needs a next-day opening bar AND that day's 00:00 rate.
        price_end = min(frames[s].index[-1].floor('D') for s in symbols)-pd.DateOffset(days=1)
        rate_end = min(pd.Timestamp(max(funding[s]), tz='UTC') for s in symbols)-pd.DateOffset(days=1)
        end = pd.Timestamp(args.end, tz='UTC') if args.end else min(price_end, rate_end) if args.funding_tail == 'complete' else price_end
        start = pd.Timestamp(args.start, tz='UTC') if args.start else max(frames[s].index[0].ceil('D') for s in symbols)
        if end < start or end > price_end or (args.funding_tail == 'complete' and end > rate_end):
            raise ValueError(f'窗口无效或缺少次日价格/资金费；价格允许至 {price_end.date()}，完整费率允许至 {rate_end.date()}')
        execution_date = (end+pd.DateOffset(days=1)).date().isoformat()
        tails = {}
        for symbol in symbols:
            keys = pd.date_range(start, end+pd.DateOffset(days=1), freq='D').strftime('%Y-%m-%d')
            missing = [k for k in keys if k not in funding[symbol]]
            last_date = max(funding[symbol])
            if missing and (args.funding_tail != 'last' or any(k <= last_date for k in missing)):
                raise ValueError(f'{symbol} 缺少资金费日期 {missing[:5]}，不以零替代')
            for key in missing:
                funding[symbol][key] = funding[symbol][last_date]
            tails[symbol] = {'last_observed_funding_date': last_date, 'forward_filled_dates': missing,
                             'raw_price_last_open': frames[symbol].index[-1].isoformat()}
        coverage_note = ('默认取价格与资金费均完整的共同窗口；下一日00:00强平仍需该日费率，'
                         '因此末尾信号日早于资金费文件末日一天。未使用末尾缺失费率的价格日期。'
                         if args.funding_tail == 'complete' else
                         '使用全部完整UTC日价格样本；尾部缺失资金费沿用最近已知日均值，前推日期见下列覆盖信息。')
        coverage_note += ' 覆盖信息：'+json.dumps(tails, ensure_ascii=False)
        feature_cache = {}
        for symbol, frame in frames.items():
            feature_cache[(symbol, 5)] = build_features(frame)
            if symbol in symbols and any(TIMEFRAMES[r] == 15 for r in rules):
                feature_cache[(symbol, 15)] = build_features(resample_15m(frame))
        print('运行复现核对 VWAP_RECLAIM BTCUSDT volrix 2023-09-08..2026-10-27', file=sys.stderr, flush=True)
        rep_bt = run_backtest(frames['BTCUSDT'], 'VWAP_RECLAIM', 'BTCUSDT', 'volrix',
                             start='2023-09-08', end='2026-10-27', features=feature_cache[('BTCUSDT', 5)])
        source = []
        for filename in ('research-crypto-vwap-reclaim.json', 'research-crypto-vwap-reclaim.p2.json'):
            source.extend(json.loads((root/'volrix/trades'/filename).read_text())['data']['items'])
        perf = json.loads((root/'volrix/detail/research-crypto-vwap-reclaim.json').read_text())['data']['performance']
        tz = infer_source_timezone(source, frames['BTCUSDT'])
        matching = match_entries(rep_bt['trades'], source, tz['offset_minutes'])
        rep = {'backtest': rep_bt, 'source_performance': perf, 'source_trade_count': len(source),
               'timezone': tz, 'entry_matching': matching}
        print(reproduction_text(rep), file=sys.stderr, flush=True)
        results = []
        for rule in rules:
            for symbol in symbols:
                print(f'运行主表 {rule} {symbol} real {start.date()}..{end.date()}', file=sys.stderr, flush=True)
                f = feature_cache[(symbol, TIMEFRAMES[rule])]
                r = run_backtest(f, rule, symbol, 'real', funding[symbol],
                                 start=start.date().isoformat(), end=end.date().isoformat(), features=f)
                results.append(r)
                print(f"  收益 {r['metrics']['return_pct']:.2f}% Sharpe {r['metrics']['sharpe']:.3f} 交易 {r['metrics']['trades']}", file=sys.stderr, flush=True)
        payload = {'metadata': {'engine': 'backtrader', 'engine_version': bt.__version__,
                                'main_start': start.date().isoformat(), 'main_end': end.date().isoformat(),
                                'execution_date': execution_date, 'funding_tail': args.funding_tail,
                                'funding_coverage': tails, 'coverage_note': coverage_note,
                                'capital_denominator': CAPITAL, 'broker_cash': BROKER_CASH,
                                'costs': COSTS, 'admission_conditions_predeclared': ADMISSION,
                                'generated_utc': datetime.now(timezone.utc).isoformat()},
                   'reproduction': rep, 'results': results, 'admission': admission(results, rules)}
        payload = clean_json(payload)
        report = render_report(payload)
        if args.out:
            args.out.parent.mkdir(parents=True, exist_ok=True)
            args.out.write_text(report)
        else:
            print(report)
        if args.json:
            args.json.parent.mkdir(parents=True, exist_ok=True)
            args.json.write_text(json.dumps(payload, ensure_ascii=False, allow_nan=False, indent=2)+'\n')
        print(admission_text(payload['admission']), file=sys.stderr, flush=True)
        return 0
    except (FileNotFoundError, ValueError, RuntimeError, KeyError) as exc:
        print(f'错误：{exc}', file=sys.stderr)
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
