"""Offline Volrix sizing signals; all execution and accounting use Backtrader.

Signals index UTC bar *open dates*: row i's close submits a Market order for
row i+1's open. No fabricated terminal liquidation, stop orders or fill engine.
"""
from __future__ import annotations

import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

from .backtrader_adapter import _backtrader
from .quant_audit import audit_returns, grid_around, sweep_plateau

bt = _backtrader()
DATA_ROOT = Path('/Users/zhangyun/workspace/nofx/data')
COSTS = {'real': dict(commission=.0005, slippage=.0003),
         'volrix': dict(commission=0., slippage=.0005)}


def max20_positions(df, window=20):
    c = df.close
    return (c >= c.rolling(window).max()).to_numpy(dtype=float)


def _ema_sign(df, fast, slow):
    c = df.close
    a = c.ewm(span=fast, adjust=False, min_periods=fast).mean()
    b = c.ewm(span=slow, adjust=False, min_periods=slow).mean()
    return np.sign(a-b).fillna(0).to_numpy()


def ewmac_positions(df, fast=16, slow=64):
    return _ema_sign(df, fast, slow)


def ewmac_vt_positions(df, fast=16, slow=64, rv_window=25, target_vol=.20):
    signs = _ema_sign(df, fast, slow)
    rv = df.close.pct_change(fill_method=None).rolling(rv_window).std(ddof=1).to_numpy()*math.sqrt(365)
    out = np.zeros(len(df))
    size, last_sign = 0., 0.
    for i, sign in enumerate(signs):
        if sign != last_sign and np.isfinite(rv[i]):
            size = sign * min(1., target_vol/rv[i]) if rv[i] > 0 else sign
            last_sign = sign
        out[i] = size
    return out


def golden_cross_positions(df, fast=50, slow=200):
    a, b = df.close.rolling(fast).mean(), df.close.rolling(slow).mean()
    out, pos = np.zeros(len(df)), 0.
    for i, (c, x, y) in enumerate(zip(df.close, a, b)):
        if pos == 0 and x > y and c > x:
            pos = 1.
        elif pos and x < y:
            pos = 0.
        out[i] = pos
    return out


def turtle_positions(df, entry_window=55, exit_window=20):
    hi = df.high.shift(1).rolling(entry_window).max().to_numpy()
    lo = df.low.shift(1).rolling(entry_window).min().to_numpy()
    xhi = df.high.shift(1).rolling(exit_window).max().to_numpy()
    xlo = df.low.shift(1).rolling(exit_window).min().to_numpy()
    out, pos = np.zeros(len(df)), 0.
    for i, c in enumerate(df.close):
        if pos == 0:
            if c > hi[i]:
                pos = 1.
            elif c < lo[i]:
                pos = -1.
        elif (pos > 0 and c < xlo[i]) or (pos < 0 and c > xhi[i]):
            pos = 0.  # an exit cannot also create an entry at this close
        out[i] = pos
    return out


def _monthly_hold(df, values):
    out, pos = np.zeros(len(df)), 0.
    # Calendar month ends are known ex ante; never infer them from next row
    # or label the last (possibly truncated) sample row a month end.
    for i, end in enumerate(df.index.is_month_end):
        if end:
            pos = float(values[i])
        out[i] = pos
    return out


def vol_managed_positions(df, rv_window=21, history_window=252):
    rv = df.close.pct_change(fill_method=None).rolling(rv_window).std(ddof=1)*math.sqrt(365)
    reference = rv.shift(1).rolling(history_window).mean()
    return _monthly_hold(df, (rv < reference).to_numpy())


def high52_positions(df, window=252, entry_ratio=.95, exit_ratio=.90):
    hi = df.close.rolling(window).max().to_numpy()
    out, pos = np.zeros(len(df)), 0.
    for i, c in enumerate(df.close):
        if not pos and c >= entry_ratio*hi[i]:
            pos = 1.
        elif pos and c < exit_ratio*hi[i]:
            pos = 0.
        out[i] = pos
    return out


def sma250_positions(df, window=250):
    return (df.close > df.close.rolling(window).mean()).to_numpy(dtype=float)


def supertrend_positions(df, atr_window=10, multiplier=3.):
    """Wilder ATR seeded by N TRs (first TR=H-L), ratcheting final bands.

    Initial trend is down; first up-flip requires a close above the final
    upper band. Equal closes preserve the previous direction.
    """
    n = len(df)
    out = np.zeros(n)
    if n < atr_window:
        return out
    h, l, c = (df[k].to_numpy(dtype=float) for k in ('high', 'low', 'close'))
    prev = np.r_[c[0], c[:-1]]
    tr = np.maximum(h-l, np.maximum(abs(h-prev), abs(l-prev)))
    atr = float(tr[:atr_window].mean())
    upper = (h[atr_window-1]+l[atr_window-1])/2+multiplier*atr
    lower = (h[atr_window-1]+l[atr_window-1])/2-multiplier*atr
    up = False
    for i in range(atr_window, n):
        atr = (atr*(atr_window-1)+tr[i])/atr_window
        bu = (h[i]+l[i])/2+multiplier*atr
        bl = (h[i]+l[i])/2-multiplier*atr
        upper = bu if bu < upper or c[i-1] > upper else upper
        lower = bl if bl > lower or c[i-1] < lower else lower
        if up and c[i] < lower:
            up = False
        elif not up and c[i] > upper:
            up = True
        out[i] = float(up)
    return out


def _post_down(df, threshold, hold_bars, cooldown):
    r = df.close.pct_change(fill_method=None).to_numpy()
    out, event = np.zeros(len(df)), -10**9
    for i in range(len(df)):
        if i-event > cooldown and i-event >= hold_bars and r[i] <= threshold[i] and threshold[i] < 0:
            event = i
        # Event close e submits entry for e+1; exit signal e+hold_bars
        # closes at e+hold_bars+1, after exactly hold_bars execution bars.
        out[i] = float(i-event < hold_bars)
    return out


def post_down_positions(df, threshold=.02, hold_bars=5, cooldown=10):
    return _post_down(df, np.full(len(df), -threshold), hold_bars, cooldown)


def post_down_vol_positions(df, rv_window=20, sigma=2., hold_bars=5, cooldown=10):
    # Threshold uses the PREVIOUS 20 returns, excluding the event return.
    rv = df.close.pct_change(fill_method=None).shift(1).rolling(rv_window).std(ddof=1)
    return _post_down(df, -sigma*rv.to_numpy(), hold_bars, cooldown)


def faber_positions(df, window=210):
    return _monthly_hold(df, (df.close > df.close.rolling(window).mean()).to_numpy())


def tom_positions(df, entry_days_before_end=2, exit_day=3):
    out, pos = np.zeros(len(df)), 0.
    for i, d in enumerate(df.index):
        if d.day == d.days_in_month-entry_days_before_end+1:
            pos = 1.
        elif d.day == exit_day:
            pos = 0.
        out[i] = pos
    return out


def donchian_sar_positions(df, window=20):
    hi = df.high.shift(1).rolling(window).max().to_numpy()
    lo = df.low.shift(1).rolling(window).min().to_numpy()
    out, pos = np.zeros(len(df)), 0.
    for i, c in enumerate(df.close):
        if c > hi[i]:
            pos = 1.
        elif c < lo[i]:
            pos = -1.
        out[i] = pos
    return out


RULES = {
    'MAX20': (max20_positions, dict(window=20), '1d', 'crypto-btc-20-day-max-trend'),
    'EWMAC_VT': (ewmac_vt_positions, dict(fast=16, slow=64, rv_window=25, target_vol=.20), '1d', 'crypto-eth-ewmac-16-64-vol-target'),
    'EWMAC': (ewmac_positions, dict(fast=16, slow=64), '1d', 'mcx-goldm-ewmac-16-64'),
    'GOLDEN_CROSS': (golden_cross_positions, dict(fast=50, slow=200), '1d', 'mcx-goldm-golden-cross-50-200-long'),
    'TURTLE': (turtle_positions, dict(entry_window=55, exit_window=20), '1d', 'mcx-goldm-turtle-55-20-breakout'),
    'VOL_MANAGED': (vol_managed_positions, dict(rv_window=21, history_window=252), '1d', 'mcx-goldm-volatility-managed-monthly-timing'),
    'HIGH52': (high52_positions, dict(window=252, entry_ratio=.95, exit_ratio=.90), '1d', 'mcx-goldm-52-week-high-proximity-long'),
    'SMA250': (sma250_positions, dict(window=250), '1d', 'mcx-goldm-sma250-trend'),
    'SUPERTREND_D': (supertrend_positions, dict(atr_window=10, multiplier=3.), '1d', 'nifty-daily-supertrend-10-3-long-futures'),
    'POST_DOWN': (post_down_positions, dict(threshold=.02, hold_bars=5, cooldown=10), '1d', 'nifty-post-large-down-day-rebound-futures'),
    'POST_DOWN_VOL': (post_down_vol_positions, dict(rv_window=20, sigma=2., hold_bars=5, cooldown=10), '1d', None),
    'FABER': (faber_positions, dict(window=210), '1d', 'nifty-faber-10-month-sma-timing-futures'),
    'TOM': (tom_positions, dict(entry_days_before_end=2, exit_day=3), '1d', 'nifty-turn-of-month-futures'),
    'SUPERTREND_1H': (supertrend_positions, dict(atr_window=10, multiplier=3.), '1h', 'nifty-1h-supertrend-10-3-long-futures'),
    'DONCHIAN_SAR_1H': (donchian_sar_positions, dict(window=20), '1h', 'donchian20-gold-60m'),
    'DONCHIAN_SAR_5M': (donchian_sar_positions, dict(window=20), '5m', 'donchian20-crudeoil-5m'),
}


class FundingBroker(bt.brokers.BackBroker):
    """One native cash adjustment per UTC day, before open order execution.

    Funding is charged to the position carried into that day's first bar,
    valued at its observable open. No account/P&L logic is replaced.
    """
    params = (('funding_rates', None), ('funding_data', None))

    def __init__(self):
        super().__init__()
        self.funding_paid, self.funding_days, self.last_day = 0., 0, None

    def next(self):
        d = self.p.funding_data
        if d is not None and len(d):
            day = d.datetime.date(0)
            if day != self.last_day:
                self.last_day = day
                rates = self.p.funding_rates
                if rates is not None and day in rates:
                    self.funding_days += 1
                    cost = self.getposition(d).size*float(d.open[0])*rates[day]*3
                    self.add_cash(-cost)
                    self.funding_paid += cost
        super().next()


class TargetStrategy(bt.Strategy):
    """Size at a changed signal's close, then hold that quantity unchanged."""
    params = (('targets', None), ('notional', 1000.), ('sizing', 'compound'),
              ('initial_cash', 1e9))

    def __init__(self):
        self.fills, self.entries, self.held_days = [], [], set()
        self.closed_dates = []
        self.commission_paid, self.turnover = 0., 0.
        self.exposure_sum, self.observations, self.last_target = 0., 0, None
        self.bankrupt = False

    def notify_order(self, order):
        if order.status in (order.Margin, order.Rejected):
            raise ValueError('Backtrader 拒单：'+order.getstatusname())
        if order.status != order.Completed:
            return
        ex = order.executed
        before = self.position.size-ex.size
        day = bt.num2date(ex.dt).date().isoformat()
        self.commission_paid += ex.comm
        self.turnover += abs(ex.size*ex.price)/self.p.notional
        self.fills.append(dict(date=day, time=bt.num2date(ex.dt).isoformat(),
                               signal_time=order.info.signal_time, size=ex.size,
                               price=ex.price, commission=ex.comm))
        if before == 0 or before*self.position.size < 0:
            self.entries.append(day)

    def notify_trade(self, trade):
        if trade.isclosed:
            self.closed_dates.append(bt.num2date(trade.dtclose).date().isoformat())

    def next(self):
        i = len(self.data)-1
        if self.position.size:
            self.held_days.add(self.data.datetime.date(0).isoformat())
        equity = self.broker.getvalue()-self.p.initial_cash+self.p.notional
        # Exposure relative to the capital that sized the position.
        base = self.p.notional if self.p.sizing == 'fixed' else equity
        if base > 0:
            self.exposure_sum += abs(self.position.size*self.data.close[0])/base
            self.observations += 1
        if self.p.sizing == 'compound' and equity <= 0:
            self.bankrupt = True
        target = float(self.p.targets[i])
        if self.last_target != target:
            capital = self.p.notional if self.p.sizing == 'fixed' else equity
            # Bankruptcy is permanent: changed targets may close an existing
            # position, but cannot open/reverse it using the virtual cash.
            size = 0. if self.bankrupt else target*capital/self.data.close[0]
            if not math.isclose(size, self.position.size, rel_tol=1e-12, abs_tol=1e-14):
                order = self.order_target_size(target=size, exectype=bt.Order.Market)
                if order:
                    order.addinfo(signal_time=self.data.datetime.datetime(0).isoformat())
        self.last_target = target


class CapitalCurve(bt.Analyzer):
    params = (('notional', 1000.), ('initial_cash', 1e9))

    def start(self):
        self.days = {}

    def next(self):
        s = self.strategy
        day = s.data.datetime.date(0).isoformat()
        self.days[day] = s.broker.getvalue()-self.p.initial_cash+self.p.notional

    def get_analysis(self):
        return self.days


def _nested(obj, *keys, default=0):
    for k in keys:
        if k not in obj:
            return default
        obj = obj[k]
    return obj


def equity_statistics(equity, initial=1000.):
    """Statistics of observed native broker equity, never simulated returns."""
    s = pd.Series(equity, dtype=float)
    if s.empty:
        raise ValueError('权益序列为空')
    previous = np.r_[initial, s.to_numpy()[:-1]]
    values = s.to_numpy()
    valid = (previous > 0) & (values > 0)
    r = np.divide(values-previous, previous, out=np.full(len(s), np.nan), where=previous > 0)
    log_r = np.full(len(s), np.nan)
    log_r[valid] = np.log(values[valid]/previous[valid])
    std = float(np.std(r, ddof=1)) if len(s) > 1 and np.isfinite(r).all() else None
    vol = std*math.sqrt(365) if std is not None else None
    sharpe = float(np.mean(r)/std*math.sqrt(365)) if std and std > 1e-14 else (0. if std is not None else None)
    dates = pd.to_datetime(s.index)
    days = (dates[-1]-dates[0]).days+1
    cagr = (math.expm1(math.log(values[-1]/initial)*365/days) if values[-1] > 0 else -1.) if initial > 0 else None
    peaks = np.maximum.accumulate(np.r_[initial, values])
    dd = float(np.max((peaks[1:]-values)/peaks[1:])) if initial > 0 else None
    return dict(return_pct=float((values[-1]/initial-1)*100) if initial > 0 else None,
                cagr_pct=cagr*100 if cagr is not None else None,
                annual_vol_pct=vol*100 if vol is not None else None, sharpe=sharpe,
                maxdd_pct=dd*100 if dd is not None else None, calmar=cagr/dd if dd else None,
                bankrupt=bool((values <= 0).any()),
                daily_returns=dict(zip(s.index, r.tolist())),
                daily_log_returns=dict(zip(s.index, log_r.tolist())))


def run_rule(df, positions, *, notional=1000, commission, slippage, funding=None,
             sizing='compound'):
    """Independent Cerebro/BackBroker run, sized only when the target changes.

    New quantity = target * capital / signal close; fill at the next open.
    Capital is current strategy equity in compound mode, initial notional in
    fixed mode (Volrix reproduction only). Nonpositive equity halts new entries.
    A constant target (including buy-and-hold) never resizes the position.
    """
    validate_frame(df)
    targets = np.asarray(positions, dtype=float)
    if targets.shape != (len(df),) or not np.isfinite(targets).all() or (abs(targets) > 1+1e-12).any():
        raise ValueError('positions 必须与 K 线等长、有限且位于 [-1,1]')
    if not all(math.isfinite(x) for x in (notional, commission, slippage)) or notional <= 0 or not 0 <= commission < 1 or not 0 <= slippage < 1:
        raise ValueError('notional/commission/slippage 参数不合法')
    if sizing not in ('fixed', 'compound'):
        raise ValueError("sizing 须为 'fixed' 或 'compound'")
    rates = None
    if funding is not None:
        items = funding.items() if hasattr(funding, 'items') else funding
        rates = {pd.Timestamp(k).date(): float(v) for k, v in items}
        if not all(math.isfinite(x) for x in rates.values()):
            raise ValueError('资金费率必须有限')
    cerebro = bt.Cerebro(stdstats=False)
    step = df.index[1]-df.index[0] if len(df) > 1 else pd.Timedelta(days=1)
    is_daily = step.total_seconds() >= 86400
    feed = bt.feeds.PandasData(dataname=df, timeframe=bt.TimeFrame.Days if is_daily else bt.TimeFrame.Minutes,
                              compression=1 if is_daily else max(1, int(step.total_seconds()/60)))
    cerebro.adddata(feed)
    initial_cash = 1e9
    broker = FundingBroker(funding_rates=rates, funding_data=feed)
    broker.setcash(initial_cash)
    broker.setcommission(commission=commission)
    broker.set_slippage_perc(slippage, slip_open=True, slip_match=True)
    broker.set_coc(False)
    broker.set_coo(False)
    cerebro.setbroker(broker)
    cerebro.addstrategy(TargetStrategy, targets=targets, notional=notional,
                        sizing=sizing, initial_cash=initial_cash)
    cerebro.addanalyzer(CapitalCurve, _name='capital', notional=notional, initial_cash=initial_cash)
    cerebro.addanalyzer(bt.analyzers.TimeReturn, _name='timereturn', timeframe=bt.TimeFrame.Days, fund=False)
    cerebro.addanalyzer(bt.analyzers.DrawDown, _name='drawdown', fund=False)
    cerebro.addanalyzer(bt.analyzers.TradeAnalyzer, _name='trades')
    s = cerebro.run()[0]
    equity = s.analyzers.capital.get_analysis()
    stats = equity_statistics(equity, notional)
    stats['bankrupt'] = stats['bankrupt'] or s.bankrupt
    trades = s.analyzers.trades.get_analysis()
    count = _nested(trades, 'total', 'closed')
    won = _nested(trades, 'won', 'total')
    profit = _nested(trades, 'won', 'pnl', 'total')
    loss = abs(_nested(trades, 'lost', 'pnl', 'total'))
    stats.update(trades=int(count), win_rate_pct=100*won/count if count else None,
                 profit_factor=profit/loss if loss else None,
                 average_exposure=s.exposure_sum/s.observations if s.observations else None,
                 commission_paid=s.commission_paid, funding_paid=broker.funding_paid,
                 turnover=s.turnover, funding_days=broker.funding_days)
    return dict(engine='backtrader', engine_version=bt.__version__, notional=notional,
                commission=commission, slippage=slippage, sizing=sizing,
                start=min(equity), end=max(equity),
                metrics=stats, daily_equity=equity, fills=s.fills, entry_dates=s.entries,
                held_dates=sorted(s.held_days),
                closed_trades_by_date=s.closed_dates,
                native=dict(trade_analyzer=trades, drawdown=s.analyzers.drawdown.get_analysis(),
                            time_return={k.isoformat(): v for k, v in s.analyzers.timereturn.get_analysis().items()}))


def validate_frame(df):
    if not isinstance(df.index, pd.DatetimeIndex) or df.empty or not df.index.is_monotonic_increasing or not df.index.is_unique:
        raise ValueError('K 线须非空、按唯一 UTC 时间递增')
    if df.index.tz is not None and str(df.index.tz) != 'UTC':
        raise ValueError('K 线时区须 UTC 或无时区的 UTC')
    a = df[['open', 'high', 'low', 'close', 'volume']].to_numpy(dtype=float)
    if not np.isfinite(a).all() or (a[:, :4] <= 0).any() or (a[:, 4] < 0).any():
        raise ValueError('K 线价格/成交量不合法')
    if (a[:, 1] < np.maximum(a[:, 0], a[:, 3])).any() or (a[:, 2] > np.minimum(a[:, 0], a[:, 3])).any():
        raise ValueError('K 线 OHLC 范围不合法')


def load_frame(path, timeframe='1d'):
    path = Path(path)
    if not path.is_file():
        raise ValueError(f'缺少本地 {timeframe} K 线：{path}；命令不会联网下载，请用 --rules 排除相关规则')
    rows = json.loads(path.read_text())
    df = pd.DataFrame([r[:6] for r in rows], columns=['time', 'open', 'high', 'low', 'close', 'volume'])
    df.index = pd.DatetimeIndex(pd.to_datetime(df.pop('time'), unit='ms' if timeframe != '1d' else None, utc=True)).tz_localize(None)
    df = df.astype(float)
    validate_frame(df)
    expected = pd.Timedelta(seconds={'1d': 86400, '1h': 3600, '5m': 300}[timeframe])
    if len(df) > 1 and not (df.index.to_series().diff().iloc[1:] == expected).all():
        raise ValueError(f'K 线不连续（不能把缺失根当作下一根成交）：{path}')
    return df


def load_funding(root, symbol):
    path = Path(root)/'factor_mining/crypto/alt_daily/funding'/f'{symbol}.json'
    if not path.is_file():
        raise ValueError(f'缺少资金费缓存：{path}')
    return {pd.Timestamp(k).date(): float(v) for k, v in json.loads(path.read_text())}


def _halves(run):
    equity = pd.Series(run['daily_equity'])
    dates = pd.to_datetime(equity.index)
    mid = dates[0]+(dates[-1]-dates[0])/2
    result = {}
    for label, mask in [('first', dates <= mid), ('second', dates > mid)]:
        idx = np.flatnonzero(mask)
        if not len(idx):
            result[label] = None
            continue
        initial = float(equity.iloc[idx[0]-1]) if idx[0] else run['notional']
        part = equity.iloc[idx].to_dict()
        stats = equity_statistics(part, initial)
        stats['trades'] = sum(min(part) <= d <= max(part) for d in run['closed_trades_by_date'])
        result[label] = stats
    return result


def _audit(run, benchmark):
    a = pd.Series(run['metrics']['daily_log_returns'])
    b = pd.Series(benchmark['metrics']['daily_log_returns'])
    pair = pd.concat([a, b], axis=1, join='inner')
    if run['metrics']['bankrupt'] or benchmark['metrics']['bankrupt']:
        return dict(verdict='NO_EDGE', reason='策略资本非正，无法定义全窗对数收益')
    result = audit_returns(pair.iloc[:, 0].tolist(), pair.iloc[:, 1].tolist(), 365)
    result['verdict'] = {'DATA_VALIDATED': 'VALIDATED'}.get(result['verdict'], result['verdict'])
    return result


def _date_overlap(ours, source):
    a, b = set(ours), set(source)
    exact = len(a & b)
    # One-to-one matching: never let a single source date match many entries.
    remaining = set(pd.Timestamp(x) for x in b)
    matches = 0
    for x in sorted(pd.Timestamp(x) for x in a):
        candidates = [y for y in remaining if abs((x-y).days) <= 1]
        if candidates:
            y = min(candidates, key=lambda y: (abs((x-y).days), y))
            remaining.remove(y)
            matches += 1
    return dict(ours=len(a), volrix=len(b), jaccard=exact/len(a | b) if a | b else 1.,
                exact_match_rate=exact/len(b) if b else None,
                tolerant_match_rate=matches/len(b) if b else None,
                tolerant_jaccard=matches/(len(a)+len(b)-matches) if a or b else 1.)


def reproduction(root, frames):
    rows = []
    for name, symbol in [('MAX20', 'BTCUSDT'), ('EWMAC_VT', 'ETHUSDT')]:
        func, params, _, slug = RULES[name]
        perf_path = Path(root)/'volrix/detail'/f'{slug}.json'
        trade_path = Path(root)/'volrix/trades'/f'{slug}.json'
        if not perf_path.is_file() or not trade_path.is_file():
            raise ValueError(f'缺少 Volrix 复现数据：{perf_path} / {trade_path}')
        perf = json.loads(perf_path.read_text())['data']['performance']
        trades = json.loads(trade_path.read_text())['data']['items']
        df = frames[symbol]
        # Indicators warm up on earlier real history; execution starts flat at
        # windowStart (no outside-window synthetic position or P&L).
        targets = func(df, **params)
        mask = (df.index >= pd.Timestamp(perf['windowStart'])) & (df.index <= pd.Timestamp(perf['windowEnd']))
        run = run_rule(df.loc[mask], targets[mask], sizing='fixed', **COSTS['volrix'])
        start, end = perf['windowStart'], perf['windowEnd']
        source_entries = sorted({t['entryDate'] for t in trades if start <= t['entryDate'] <= end})
        # Source exits at 00:59:59 on exitDate, so that date is not a held day.
        held = set()
        for t in trades:
            a, b = max(start, t['entryDate']), min(end, t['exitDate'])
            if a <= b:
                held.update(d.date().isoformat() for d in pd.date_range(a, b, inclusive='left'))
        rows.append(dict(rule=name, symbol=symbol, start=start, end=end,
                         sizing=run['sizing'], cost_model='volrix',
                         ours={k: v for k, v in run['metrics'].items() if not k.startswith('daily_')},
                         volrix=perf, entry_overlap=_date_overlap(run['entry_dates'], source_entries),
                         held_overlap=_date_overlap(run['held_dates'], held),
                         comparison_basis='entry_dates',
                         pre_window_source_entries=sum(t['entryDate'] < start for t in trades)))
    return rows


def parameter_grid(name):
    defaults = RULES[name][1]
    grid = grid_around(defaults, (.5, .75, 1., 1.25, 1.5))
    scanned, fixed = list(defaults), {}
    if len(grid) > 125:
        scanned = [k for k in defaults if k in ('fast', 'slow', 'hold_bars', 'cooldown') or 'window' in k][:3]
        if not scanned:
            scanned = list(defaults)[:3]
        fixed = {k: v for k, v in defaults.items() if k not in scanned}
        grid = {k: {**fixed, **v} for k, v in grid_around({k: defaults[k] for k in scanned}, (.5, .75, 1., 1.25, 1.5)).items()}
    # All generated positive integer periods are usable. Semantic inequalities
    # define the admissible neighbourhood, before any outcomes are observed.
    grid = {key: p for key, p in grid.items()
            if all(not isinstance(v, int) or v >= 1 for v in p.values())
            and ('slow' not in p or p['fast'] < p['slow'])
            and ('entry_window' not in p or p['entry_window'] > p['exit_window'])
            and ('entry_ratio' not in p or p['exit_ratio'] < p['entry_ratio'] <= 1)
            and ('cooldown' not in p or p['cooldown'] >= p['hold_bars'])}
    return grid, tuple(defaults[k] for k in scanned), scanned, fixed


def _scalar_metrics(metrics):
    return {k: v for k, v in metrics.items() if not k.startswith('daily_')}


def _summarize_run(run):
    """Only BTC/ETH retain time series, fills and native analyzer details."""
    keys = ('engine', 'engine_version', 'notional', 'commission', 'slippage', 'sizing', 'start', 'end')
    return {**{k: run[k] for k in keys}, 'metrics': _scalar_metrics(run['metrics'])}


def cross_section(runs, benchmarks):
    rows = [r['metrics'] for r in runs.values()]
    sharpes = [x['sharpe'] for x in rows]
    valid = [x for x in sharpes if x is not None]
    outperform = sum(r['metrics']['sharpe'] is not None and benchmarks[s]['metrics']['sharpe'] is not None
                     and r['metrics']['sharpe'] > benchmarks[s]['metrics']['sharpe'] for s, r in runs.items())
    if not rows:
        return dict(n=0, median_sharpe=None, median_cagr_pct=None, positive_sharpe_ratio=None,
                    beat_bh_ratio=None, median_maxdd_pct=None, portfolio=None, benchmark_portfolio=None)

    def portfolio(collection):
        # Inner calendar: constant constituent set with all symbols available;
        # start each sleeve equally at 1000 on the first common date, then
        # hold its equity units unchanged (no cross-symbol rebalancing).
        equities = pd.concat([pd.Series(r['daily_equity'], name=s) for s, r in collection.items()], axis=1, join='inner').sort_index()
        if equities.empty or (equities.iloc[0] <= 0).any():
            return None
        initial = 1000.
        eq = equities.div(equities.iloc[0]).mul(initial).mean(axis=1)
        return dict(metrics=_scalar_metrics(equity_statistics(eq.to_dict(), initial)),
                    start=eq.index[0], end=eq.index[-1], initial_capital=initial,
                    n_symbols=len(collection))

    return dict(n=len(rows), median_sharpe=float(np.median(valid)) if valid else None,
                median_cagr_pct=float(np.median([x['cagr_pct'] for x in rows])),
                positive_sharpe_ratio=sum(x is not None and x > 0 for x in sharpes)/len(rows),
                beat_bh_ratio=outperform/len(rows), median_maxdd_pct=float(np.median([x['maxdd_pct'] for x in rows])),
                portfolio=portfolio(runs), benchmark_portfolio=portfolio({s: benchmarks[s] for s in runs}))


def admission(main, cross, sweep):
    candidates = []
    for symbol in ('BTCUSDT', 'ETHUSDT'):
        if symbol not in main:
            continue
        r = main[symbol]
        m = r['run']['metrics']
        halves = r['halves']
        if (m['sharpe'] is not None and m['sharpe'] >= .8 and
                all(halves[k] is not None and halves[k]['sharpe'] is not None and halves[k]['sharpe'] > 0 for k in ('first', 'second'))):
            candidates.append(symbol)
    if not candidates:
        return dict(verdict='FAIL', reason='条件1：无 BTC/ETH 全样本 Sharpe≥0.8 且前后半 Sharpe均>0', symbol=None)
    rejected = []
    eligible = []
    for s in candidates:
        verdict = main[s]['audit']['verdict']
        dd = main[s]['run']['metrics']['maxdd_pct']
        bhdd = main[s]['benchmark']['metrics']['maxdd_pct']
        if verdict not in ('VALIDATED', 'TRADE_OFF'):
            rejected.append(f'{s} audit={verdict}')
        elif dd >= bhdd:
            rejected.append(f'{s} MaxDD={dd:.2f}%≥BH {bhdd:.2f}%')
        else:
            eligible.append(s)
    candidates = eligible
    if not candidates:
        return dict(verdict='FAIL', reason='条件2：'+'；'.join(rejected), symbol=None)
    if cross is None or cross['positive_sharpe_ratio'] is None or cross['positive_sharpe_ratio'] < .6:
        return dict(verdict='FAIL', reason='条件3：截面 Sharpe>0 比例不足60%或未评估', symbol=candidates[0])
    if sweep is None or sweep['verdict'] != 'PLATEAU':
        return dict(verdict='FAIL', reason='条件4：参数邻域未达到 PLATEAU 或未评估', symbol=candidates[0])
    return dict(verdict='PASS', reason='全部预声明条件满足', symbol=candidates[0])


def run_study(names, symbols=('BTCUSDT', 'ETHUSDT'), universe_min_years=3., root=DATA_ROOT, progress=print):
    if not names or any(n not in RULES for n in names) or len(set(names)) != len(names):
        raise ValueError('--rules 须为 all 或 RULES 中不重复的名称')
    if not symbols or len(set(symbols)) != len(symbols) or any(not s.isalnum() for s in symbols):
        raise ValueError('--symbols 不合法')
    if not math.isfinite(universe_min_years) or universe_min_years <= 0:
        raise ValueError('--universe-min-years 须为正数')
    root = Path(root)
    # Preflight every requested intraday file, fail before starting expensive runs.
    frames = {}
    for tf in sorted({RULES[n][2] for n in names} | {'1d'}):
        for s in sorted(set(symbols) | {'BTCUSDT', 'ETHUSDT'}):
            if tf != '1d' and s not in ('BTCUSDT', 'ETHUSDT'):
                continue
            path = root/'factor_mining/crypto/daily'/f'{s}.json' if tf == '1d' else root/'volrix/klines'/f'{s}_{tf}.json'
            frames[tf, s] = load_frame(path, tf)
    funding = {s: load_funding(root, s) for s in sorted(set(symbols) | {'BTCUSDT', 'ETHUSDT'})}
    result = dict(engine='backtrader', engine_version=bt.__version__, cost_models=COSTS, notional=1000,
                  data_root=str(root), symbols=list(symbols), universe_min_years=universe_min_years,
                  reproduction=reproduction(root, {s: frames['1d', s] for s in ('BTCUSDT', 'ETHUSDT')}),
                  rules={}, universe=[], universe_excluded=[], metadata={})
    progress('fixed / volrix 复现核对完成；开始 compound / real 主表与截面（每次运行独立 Cerebro）')
    daily_names = [n for n in names if RULES[n][2] == '1d']
    universe_frames = {}
    for path in sorted((root/'factor_mining/crypto/daily').glob('*.json')) if daily_names else []:
        s = path.stem
        df = frames.get(('1d', s))
        if df is None:
            df = load_frame(path)
        years = ((df.index[-1]-df.index[0]).days+1)/365.25
        if years >= universe_min_years:
            universe_frames[s] = df
            if s not in funding:
                funding[s] = load_funding(root, s)
            result['universe'].append(dict(symbol=s, years=years, start=str(df.index[0].date()), end=str(df.index[-1].date())))
        else:
            result['universe_excluded'].append(dict(symbol=s, years=years))
    baseline = {}

    def benchmark(tf, s, df):
        if (tf, s) not in baseline:
            # Spot-equivalent hold: a multi-year perp long pays more cumulative
            # funding than its capital (BTC 2019-2022), leaving log returns undefined.
            baseline[tf, s] = run_rule(df, np.ones(len(df)), sizing='compound', funding=None, **COSTS['real'])
        return baseline[tf, s]

    for name in names:
        func, defaults, tf, slug = RULES[name]
        study = dict(timeframe=tf, source_slug=slug, params=defaults, main={}, cross_section=None, sweep=None)
        # Both required coins are always evaluated for admission and sweep;
        # --symbols adds main rows without restricting the cross-sectional pool.
        selected = sorted(set(symbols) | {'BTCUSDT', 'ETHUSDT'}) if tf == '1d' else ['BTCUSDT', 'ETHUSDT']
        base_runs = {}
        for s in selected:
            df = frames[tf, s]
            r = run_rule(df, func(df, **defaults), sizing='compound', funding=funding[s], **COSTS['real'])
            b = benchmark(tf, s, df)
            base_runs[s] = r
            halves, benchmark_halves = _halves(r), _halves(b)
            detailed = s in ('BTCUSDT', 'ETHUSDT')
            if not detailed:
                halves = {k: _scalar_metrics(v) if v else None for k, v in halves.items()}
                benchmark_halves = {k: _scalar_metrics(v) if v else None for k, v in benchmark_halves.items()}
            study['main'][s] = dict(run=r if detailed else _summarize_run(r),
                                    benchmark=b if detailed else _summarize_run(b),
                                    halves=halves, benchmark_halves=benchmark_halves, audit=_audit(r, b))
        if tf == '1d':
            cross_runs, cross_bh = {}, {}
            for s, df in universe_frames.items():
                r = base_runs.get(s)
                if r is None:
                    r = run_rule(df, func(df, **defaults), sizing='compound', funding=funding[s], **COSTS['real'])
                cross_runs[s] = r
                cross_bh[s] = benchmark(tf, s, df)
            study['cross_section'] = cross_section(cross_runs, cross_bh)
            study['cross_section']['per_symbol'] = {s: _scalar_metrics(r['metrics']) for s, r in cross_runs.items()}
            study['cross_section']['benchmark_per_symbol'] = {s: _scalar_metrics(r['metrics']) for s, r in cross_bh.items()}
            progress(f'{name}: 主表/截面 {len(cross_runs)} 币完成；开始参数邻域')
            grid, key, scanned, fixed = parameter_grid(name)

            def score(p):
                if p == defaults:
                    values = [base_runs[s]['metrics']['sharpe'] for s in ('BTCUSDT', 'ETHUSDT')]
                else:
                    values = [run_rule(frames['1d', s], func(frames['1d', s], **p), sizing='compound', funding=funding[s], **COSTS['real'])['metrics']['sharpe'] for s in ('BTCUSDT', 'ETHUSDT')]
                return float(np.mean(values)) if all(v is not None for v in values) else -math.inf

            sweep = sweep_plateau(score, grid, key)
            study['sweep'] = dict(verdict='LONE PEAK' if sweep.lone_peak else 'PLATEAU',
                                  plateau_ratio=sweep.plateau_ratio, default_score=sweep.grid[key],
                                  scanned=scanned, fixed=fixed,
                                  scores=[dict(params=grid[k], score=v) for k, v in sweep.grid.items()])
        study['admission'] = admission(study['main'], study['cross_section'], study['sweep'])
        result['rules'][name] = study
        progress(f"{name}: {study['admission']['verdict']}；"+study['admission']['reason'])
    result['metadata'] = dict(sizing='主评估 compound / real：仅目标变化时按新目标×当前策略权益/信号根收盘价定数量，次根开盘成交；变化之间数量固定，不加杠杆（多头满仓名义≈权益）；权益曾≤0则停止新开仓',
                              reproduction_sizing='复现核对 fixed / volrix：目标变化时按新目标×1000/信号根收盘价定数量，变化之间数量固定',
                              benchmark='现货等价买入持有：首根收盘按1000/close定买入数量，次根开盘成交后一直持有；计手续费滑点、不计资金费',
                              funding='每日首根开盘前的隔夜仓位 × 当根open × 当日平均费率 × 3',
                              half_split='日期中点；后半以分界前一日实际资本为锚，不重启策略；交易数按平仓日',
                              audit_split='quant_audit 默认60/40，非主表50/50',
                              universe_portfolio='所有合格币共同日期交集；各币权益除以共同首日权益×1000后取算术平均，初始资本1000；初始等权、之后不再平衡；等权BH同法；首日任一币权益≤0则组合不可定义',
                              json_detail='仅BTCUSDT/ETHUSDT保存逐日权益、收益、成交和原生分析器；截面仅保存汇总metrics',
                              admission='1h/5m仅步骤2/4，无截面与扫参不能获PASS')
    return result


def _fmt(x, digits=2):
    return '—' if x is None or not math.isfinite(float(x)) else f'{x:.{digits}f}'


def _table(headers, rows):
    return '\n'.join(['| '+' | '.join(headers)+' |', '| '+' | '.join(['---']*len(headers))+' |']+
                     ['| '+' | '.join(map(str, row))+' |' for row in rows])


def reproduction_table(result):
    rows = []
    for r in result['reproduction']:
        a, b = r['ours'], r['volrix']
        pair = lambda key, source: _fmt(a[key])+' / '+_fmt(b[source])
        overlap = r['held_overlap'] if r['comparison_basis'] == 'held_dates' else r['entry_overlap']
        rows.append([r['rule'], f"{r['start']}..{r['end']}", pair('return_pct', 'returnPercent'),
                     pair('cagr_pct', 'cagrPct'), pair('maxdd_pct', 'maxDrawdownPct'),
                     pair('sharpe', 'sharpeRatio'), f"{a['trades']} / {b['totalTrades']}",
                     ('持仓日' if r['comparison_basis'] == 'held_dates' else '入场日')+
                     f"：{overlap['jaccard']:.1%} / {overlap['exact_match_rate']:.1%} / {overlap['tolerant_match_rate']:.1%}"])
    return _table(['规则', '对齐窗口', '收益% 我们/Volrix', 'CAGR% 我们/Volrix', 'MaxDD% 我们/Volrix',
                   'Sharpe 我们/Volrix', '平仓交易数 我们/Volrix', 'Jaccard / 精确召回 / ±1日召回'], rows)


def admission_table(result):
    return _table(['规则', 'PASS/FAIL', '首个不满足条件 / 通过依据', '合格币'],
                  [[n, r['admission']['verdict'], r['admission']['reason'], r['admission']['symbol'] or '—'] for n, r in result['rules'].items()])


def render_report(result):
    lines = ['# Volrix 目标仓位规则回测', '',
             '原生 Backtrader '+result['engine_version']+'；复现核对=fixed 固定名义+volrix 成本；主评估=compound 按权益复利+real 成本。仅目标变化时定数量，变化之间数量固定；t 收盘信号、t+1 开盘 Market。', '',
             '## 复现核对（fixed 定仓 / volrix 成本）', '', reproduction_table(result), '',
             'Jaccard 为入场日集合交并比；精确/±1日匹配率以来源日期数为分母，±1日使用一对一匹配。JSON 另存持仓日比较：来源持仓日取 [entryDate, exitDate)（来源在出场日 00:59:59 平仓）。来源在 01:00 执行，本地在 00:00 开盘执行。', '',
             '复现固定使用 fixed：仅目标变化时按新目标×1000/信号根close 定数量。指标在 windowStart..windowEnd 内从空仓开始，指标用此前历史预热；来源有跨窗口持仓，不能精确复原。EWMAC_VT 仅在翻向时按 sign×min(1,0.2/rv25)×1000/close 定数量，此后保持数量不变。Volrix Sharpe 与回撤的原始算法未公开，不把数值差异强行解释为相同口径。', '',
             '## 主表（compound 定仓 / real 成本）', '',
             '每格为 CAGR% / Sharpe / MaxDD% / 原生完整平仓交易数。主表、截面、裁决、参数邻域与准入均用 compound+real：目标变化时数量=新目标×当前策略权益/信号根close；策略权益=broker value−初始虚拟现金+1000，含已实现/未实现盈亏、手续费与资金费。变化之间数量固定，不加杠杆（多头满仓名义≈权益）；EWMAC_VT翻向时用sign×min(1,0.2/rv25)×当前权益/close。权益曾≤0则永久停止新开仓，目标变化时仍可平仓，记录bankrupt=True。按日权益计算，UTC 日期中点前后半；后半保留分界收益与策略状态。BH为现货等价买入持有：首根收盘按1000/close定数量，下一根开盘买入后一直持有，计手续费滑点、不计资金费（永续多头多年持有的累计资金费曾超过本金，BTC 2022-11 权益为负，对数收益无定义）。', '',
             'BH在fixed与compound下买入数量相同；按名义累计扣除资金费仍可能使策略权益≤0，故BH的MaxDD仍可能超过100%、Sharpe不可定义。这种情况不删日期、不截断权益，audit按NO_EDGE处理。', '']
    rows = []
    compact = lambda m: ' / '.join([_fmt(m['cagr_pct']), _fmt(m['sharpe']), _fmt(m['maxdd_pct']), str(m['trades'])]) if m else '—'
    for n, rule in result['rules'].items():
        for s, r in rule['main'].items():
            run = r['run']
            rows.append([n, s, f"{run['start']}..{run['end']}", compact(run['metrics']),
                         compact(r['halves']['first']), compact(r['halves']['second']), compact(r['benchmark']['metrics']),
                         compact(r['benchmark_halves']['first']), compact(r['benchmark_halves']['second'])])
    lines += [_table(['规则', '币', '实际窗口', '全样本', '前半', '后半', 'BH全样本', 'BH前半', 'BH后半'], rows), '',
              '## 成本、敞口与交易质量', '']
    rows = []
    for n, rule in result['rules'].items():
        for s, r in rule['main'].items():
            for label, run in [(n, r['run']), ('BH', r['benchmark'])]:
                m = run['metrics']
                rows.append([label, s, _fmt(m['average_exposure']), _fmt(m['commission_paid']), _fmt(m['funding_paid']),
                             _fmt(m['turnover']), _fmt(m['annual_vol_pct']), _fmt(m['calmar']), _fmt(m['win_rate_pct']), _fmt(m['profit_factor'])])
    lines += [_table(['规则', '币', '平均名义/1000', '手续费USD', '资金费USD(负=收入)', '换手/1000', '年化波动%', 'Calmar', '胜率%', 'PF'], rows), '',
              '## 截面稳健性', '', f"上市缓存跨度≥{result['universe_min_years']}年，共 {len(result['universe'])} 币；--symbols 不缩小此池。compound+real 等权组合在所有合格币共同日期交集上，将各币策略权益除以共同首日权益×1000后取算术平均，以1000为初始权益，用equity_statistics计算；等权BH同法。这是共同首日等权、之后不再平衡的组合。共同首日任一币权益≤0时无法归一；组合自身权益曾≤0时组合指标记—。", '']
    rows = []
    for n, rule in result['rules'].items():
        c = rule['cross_section']
        if c is None:
            continue
        p, b = c['portfolio'], c['benchmark_portfolio']
        triplet = lambda x: ' / '.join(_fmt(x['metrics'][k]) for k in ('sharpe', 'cagr_pct', 'maxdd_pct')) if x and not x['metrics']['bankrupt'] else '—'
        rows.append([n, c['n'], _fmt(c['median_sharpe']), _fmt(c['median_cagr_pct']),
                     _fmt(c['positive_sharpe_ratio']*100) if c['positive_sharpe_ratio'] is not None else '—',
                     _fmt(c['beat_bh_ratio']*100) if c['beat_bh_ratio'] is not None else '—', _fmt(c['median_maxdd_pct']),
                     triplet(p), triplet(b), f"{p['start']}..{p['end']}" if p else '—'])
    lines += [_table(['规则', '币数', '中位Sharpe', '中位CAGR%', 'Sharpe>0占比%', '胜BH Sharpe占比%', '中位MaxDD%', '组合Sharpe/CAGR%/MaxDD%', '等权BH同指标', '组合窗口'], rows), '',
              '## 时间切分裁决', '', 'compound+real；audit_returns 使用日对数权益收益、365、默认60/40时间切分；与主表日期中点切分分别列示。', '']
    rows = []
    for n, rule in result['rules'].items():
        for s, r in rule['main'].items():
            a = r['audit']
            values = [_fmt(a.get(k)) for k in ('train_cagr', 'train_cagr_bench', 'holdout_cagr', 'holdout_cagr_bench', 'train_sharpe', 'train_sharpe_bench', 'holdout_sharpe', 'holdout_sharpe_bench')]
            rows.append([n, s, a['verdict'], ' / '.join(values[:4]), ' / '.join(values[4:])])
    lines += [_table(['规则', '币', '裁决', '训练CAGR/BH/保留CAGR/BH（比例）', '训练Sharpe/BH/保留Sharpe/BH'], rows), '', '## 参数邻域', '',
              '所有数值默认值取0.5/0.75/1/1.25/1.5倍（整数截断去重）；>125点仅扫主要窗口（包括持仓/冷却根数），其他参数固定。剔除非正窗口、fast≥slow、海龟入场窗≤退出窗、HIGH52的退出比≥入场比或入场比>1、冷却期<持仓期。打分为BTC/ETH compound+real全样本平均Sharpe；任一侧不可定义时该点计未通过，分数记JSON null。沿用 sweep_plateau：默认分数>0且邻域正分比例<50%才标 LONE PEAK；其余 PLATEAU（不等于证明盈利）。', '']
    lines += [_table(['规则', '点数', '默认平均Sharpe', '正分占比%', '判定', '扫描参数', '固定参数'],
                     [[n, len(r['sweep']['scores']), _fmt(r['sweep']['default_score']), _fmt(100*r['sweep']['plateau_ratio']), r['sweep']['verdict'], ','.join(r['sweep']['scanned']), str(r['sweep']['fixed'])] for n, r in result['rules'].items() if r['sweep']]), '',
              '## PASS/FAIL 汇总', '',
              '预声明（compound+real）：同一BTC/ETH币全样本Sharpe≥0.8且前后半均>0；该币audit为VALIDATED或TRADE_OFF且MaxDD低于BH；截面Sharpe>0占比≥60%；参数邻域PLATEAU。按此顺序报告首个失败条件。1h/5m按任务仅做主表和audit，无截面/邻域，不能获得PASS。', '', admission_table(result), '',
              '## 局限与约定', '',
              '- 资金费按每日 3 次结算近似：日均费率×3×UTC日首根开盘前的隔夜仓位名义；多头正费扣钱、空头正费收钱，通过 broker.add_cash 记账；缺失费率按0，JSON记录覆盖天数。日均费率为事后结算成本数据，只用于扣费，不参与信号。',
              '- 幸存者偏差：只含当前仍在缓存里的币；缓存起点未必是交易所真实上市日，上市年数按可用缓存跨度估计。',
              '- 日线收盘 UTC 00:00（行日期为开盘日）与 Volrix 时区/01:00检查有差异；印度交易日改为加密自然日。月末使用已知自然月末；要求连续K线，截断样本末根不会被误判为月末。',
              '- 未建模保证金/强平/最小下单量；无限充足现金用于消除现金约束，不代表可用杠杆。末端仅盯市，未强制平仓。股票式原生broker管理线性合约价格盈亏，不模拟实际合约保证金。',
              '- CAGR、波动、Sharpe、MaxDD按1000初始策略资本的日权益计算（回撤分母为历次策略资本峰值），绝不用1e9虚拟现金计算报告收益。原生TimeReturn/DrawDown的大现金口径只作JSON审计，TradeAnalyzer的PF与胜率含成交手续费、不含现金调整型资金费。',
              '- EWMAC EMA采用adjust=False、完整慢窗预热；rv用简单收益样本标准差ddof=1；VOL_MANAGED比较当前rv与不含当前的前252个rv均值；POST_DOWN_VOL阈值不含事件日收益。Supertrend以首N个TR均值seed Wilder ATR，初始方向向下。',
              '- DONCHIAN SAR在首次突破前空仓，首次突破后一直±1；不人为指定没有规则支持的初始多空。POST_DOWN事件后10根内忽略新事件；第五持仓根收盘下平仓单，第六根开盘成交。',
              '- 参数邻域与时间切分均使用同一历史样本，非独立样本外验证；多规则比较未做多重检验校正；本报告不能等同可执行实盘收益。', '']
    lines.insert(len(lines)-1, '- 策略或BH资本曾≤0时不能定义完整对数收益，audit记NO_EDGE；无法计算的Sharpe/半段CAGR记—。截面共同首日权益非正则无法归一；组合自身权益曾≤0时组合指标记—，不删除这些日期后继续计算。JSON仅为BTCUSDT/ETHUSDT保存逐日权益和成交，截面仅保存汇总metrics。')
    return '\n'.join(lines)


def json_safe(value):
    if isinstance(value, dict):
        return {str(k): json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(v) for v in value]
    if isinstance(value, (float, np.floating)):
        return float(value) if math.isfinite(float(value)) else None
    if isinstance(value, np.integer):
        return int(value)
    return value
