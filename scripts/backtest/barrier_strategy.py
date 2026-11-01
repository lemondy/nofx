"""离线 BARRIER_LONG/SHORT 原生 Backtrader 订单回测；预声明门槛不随结果修改。

日线决策相位自样本第0根起算。收盘按权益100%/决策收盘价定数量，
次开盘市价按direction买入/卖空；成交通知后提交原生OCO，次根起生效。
原生逐单撮合的双触发优先级为先提交的止损。不改写任何撮合或盈亏。
超时自实际入场开盘起满30自然日的收盘决策，取消OCO，次开盘平仓。
终点持仓按原生NAV计价，无虚构平仓。LONG的2026段仅描述；
SHORT使用最新完整样本，各币自身长度60/40切分后聚合两窗。
"""
from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
import time

import numpy as np
import pandas as pd

from . import probability_study as probability
from . import quantjourney_orders as orders
from . import volrix

bt = volrix.bt
US10 = probability.US
CAPITAL, BROKER_CASH = 1000., 1.e9
P2_TRIALS = 528
PLATFORM = ((1.5, 1.), (2., 1.), (2.5, 1.), (2., .75), (2., 1.25))
INTERVALS = (3, 5, 10)
SHORT_PLATFORM = ((1., 1.), (1.5, 1.), (2., 1.), (2.5, 1.), (3., 1.), (2., .75), (2., 1.25))
SHORT_THRESHOLDS = """BARRIER_SHORT(k_tp=2,k_sl=1,interval=5,max_days=30)，主检验CRYPTO_BASKET。
名单完全复用P2，数据到最新日；每币自身全样本长度60%/40%切分。
G1：两窗篮子日对数收益Sharpe均高于篮子同敞口空头基准。
G2：DSR≥0.95；N=528；横截面方差用32个币各自日Sharpe的样本方差，SR用篮子日Sharpe。
G3：全样本篮子前后两半（50%/50%）收益均>0。
G4：{(1,1),(1.5,1),(2,1),(2.5,1),(3,1),(2,0.75),(2,1.25)}×interval∈{3,5,10}，21组合中≥70%胜出。
G5：≥60%的币全样本Sharpe高于各自同敞口空头基准。
G1–G5全部满足才PASS；佣金5bp+滑点3bp+真实资金费；缺缓存按0，不剔除。"""
THRESHOLDS = """BARRIER_LONG(k_up=2,k_dn=1,interval=5,max_days=30)，主检验EFA。
门槛样本截止2025-12-31，2026-01-01起仅报告，不进门槛。
G1：前60%/后40%两窗策略对数收益Sharpe均高于同敞口基准。
G2：Deflated Sharpe≥0.95；N=528；横截面方差为US10本规则10个日Sharpe的样本方差。
G3：前后两半（50%/50%）策略收益均>0。
G4：{(1.5,1),(2,1),(2.5,1),(2,0.75),(2,1.25)}×interval∈{3,5,10}的15组合中，≥70%全样本Sharpe高于各自同敞口基准。
G5：US10中≥60%全样本Sharpe高于各自同敞口基准。
G1–G5全部满足才PASS；ETF佣金1bp+滑点2bp，无资金费；BTC/ETH佣金5bp+滑点3bp+真实资金费仅报告。"""


class BarrierLong(bt.Strategy):
    """Direction-aware native OCO strategy; legacy name/default retain LONG."""
    params = (('k_up', 2.), ('k_dn', 1.), ('interval', 5), ('max_days', 30),
              ('scales', None), ('dates', None), ('exposure_end', None), ('direction', 1))

    def __init__(self):
        self.entry_order = self.exit_order = None
        self.protection = []
        self.entry_signal = None
        self.entry_time = None
        self.funding_mark = 0.
        self.daily, self.exposures = {}, {}
        self.fills, self.closed, self.statuses = [], [], []
        self.exposure_sum = self.observations = self.commission = 0.
        self.bankrupt = False

    def notify_order(self, order):
        self.statuses.append(dict(ref=order.ref, role=order.info.get('role'), status=order.getstatusname()))
        if order.status in (order.Margin, order.Rejected):
            raise RuntimeError(f'原生订单{order.getstatusname()}: {order.info.get("role")}')
        if order.status != order.Completed:
            return
        e = order.executed
        self.commission += e.comm
        self.fills.append(dict(ref=order.ref, role=order.info.get('role'),
                               date=bt.num2date(e.dt).isoformat(), price=e.price,
                               size=e.size, commission=e.comm,
                               signal_date=order.info.get('signal_date')))
        if order.info.get('role') == 'entry':
            self.entry_order = None
            self.entry_time = bt.num2date(e.dt)
            close, s = self.entry_signal
            # Native OCO, stop submitted first: deterministic conservative
            # ordering when BOTH barriers are inside the same later daily bar.
            exit_side = self.sell if self.p.direction == 1 else self.buy
            stop = exit_side(size=abs(e.size), exectype=bt.Order.Stop,
                             price=close*math.exp(-self.p.direction*self.p.k_dn*s))
            take = exit_side(size=abs(e.size), exectype=bt.Order.Limit,
                             price=close*math.exp(self.p.direction*self.p.k_up*s), oco=stop)
            stop.addinfo(role='stop_loss')
            take.addinfo(role='take_profit')
            self.protection = [stop, take]
        else:
            self.exit_order = None
            self.protection = []

    def notify_trade(self, trade):
        if trade.justopened:
            self.funding_mark = self.broker.funding_paid
        if trade.isclosed:
            start, end = bt.num2date(trade.dtopen), bt.num2date(trade.dtclose)
            carry = self.broker.funding_paid-self.funding_mark
            self.closed.append(dict(entry_date=start.isoformat(), exit_date=end.isoformat(),
                                    holding_days=(end-start).total_seconds()/86400,
                                    pnl=trade.pnl, pnlcomm=trade.pnlcomm,
                                    funding=carry, net_pnl=trade.pnlcomm-carry))

    def next(self):
        i = len(self.data)-1
        date = self.p.dates[i]
        nav = self.broker.getvalue()-BROKER_CASH+CAPITAL
        self.daily[date] = nav
        exposure = abs(self.position.size*float(self.data.close[0]))/max(abs(nav), 1e-12)
        self.exposures[date] = exposure
        if self.p.exposure_end is None or date < self.p.exposure_end:
            self.exposure_sum += exposure
            self.observations += 1
        self.bankrupt |= nav <= 0
        if self.position:
            elapsed = (self.data.datetime.datetime(0)-self.entry_time).total_seconds()/86400
            # Newly submitted OCOs cannot yet be canceled by BackBroker.
            # For SHORT, leave predeclared OCO/timeout execution intact even
            # after an entry-bar insolvency; a second close would reverse it.
            emergency = self.bankrupt and self.p.direction == 1
            if (elapsed >= self.p.max_days or emergency) and self.exit_order is None:
                for order in self.protection:
                    if order.alive():
                        self.cancel(order)
                self.exit_order = self.close(exectype=bt.Order.Market)
                self.exit_order.addinfo(role='timeout' if not self.bankrupt else 'bankrupt', signal_date=date)
            return
        if self.bankrupt or self.entry_order is not None or i % self.p.interval:
            return
        s = self.p.scales[i]
        if not math.isfinite(s) or s <= 0:
            return
        close = float(self.data.close[0])
        self.entry_signal = close, s
        entry_side = self.buy if self.p.direction == 1 else self.sell
        self.entry_order = entry_side(size=nav/close, exectype=bt.Order.Market)
        self.entry_order.addinfo(role='entry', signal_date=date)


def run_barrier(df, *, k_up=2., k_dn=1., interval=5, max_days=30,
                commission=.0001, slippage=.0002, funding=None, annual_days=252,
                scales=None, cutoff=probability.CUTOFF, direction=1, k_tp=None, k_sl=None):
    """Run native orders on the entire path; all gate metrics use the prefix."""
    volrix.validate_frame(df)
    direction = {'long': 1, 'short': -1}.get(direction, direction)
    if direction not in (-1, 1):
        raise ValueError('direction须为long/short或1/-1')
    k_up = k_up if k_tp is None else k_tp
    k_dn = k_dn if k_sl is None else k_sl
    if k_up <= 0 or k_dn <= 0 or interval < 1 or int(interval) != interval or max_days <= 0:
        raise ValueError('障碍/间隔/最长持有期须正，间隔须整数')
    if not 0 <= commission < 1 or not 0 <= slippage < 1:
        raise ValueError('费用须在[0,1)')
    scales = probability.entry_scales(df, df, '1d') if scales is None else np.asarray(scales, float)
    if len(scales) != len(df):
        raise ValueError('波动数组须与K线等长')
    rates = None if funding is None else {pd.Timestamp(k).date(): float(v) for k, v in funding.items()}
    cerebro = bt.Cerebro(stdstats=False)
    feed = bt.feeds.PandasData(dataname=df, timeframe=bt.TimeFrame.Days)
    cerebro.adddata(feed)
    broker = volrix.FundingBroker(funding_rates=rates, funding_data=feed)
    # Virtual reserve prevents commission/gaps rejecting a 100%-NAV close
    # decision. Strategy capital is always broker NAV minus this reserve.
    broker.setcash(BROKER_CASH)
    broker.setcommission(commission=commission)
    broker.set_slippage_perc(slippage, slip_open=True, slip_match=True)
    broker.set_coc(False)
    broker.set_coo(False)
    cerebro.setbroker(broker)
    exposure_end = pd.Timestamp(cutoff).isoformat() if cutoff is not None else None
    cerebro.addstrategy(BarrierLong, k_up=k_up, k_dn=k_dn, interval=interval, max_days=max_days,
                        scales=scales, dates=[t.isoformat() for t in df.index], exposure_end=exposure_end,
                        direction=direction)
    st = cerebro.run()[0]
    equity = pd.Series(st.daily, dtype=float)
    equity.index = pd.to_datetime(equity.index)
    previous = equity.shift(fill_value=CAPITAL)
    simple_returns = equity/previous-1
    with np.errstate(invalid='ignore', divide='ignore'):
        logs = np.log(equity/previous)
    gate = np.ones(len(df), dtype=bool) if cutoff is None else df.index < pd.Timestamp(cutoff)
    if not gate.any():
        raise ValueError('门槛样本为空')
    s = logs.iloc[np.flatnonzero(gate)].to_numpy()
    average_exposure = st.exposure_sum/max(st.observations, 1)
    benchmark = np.log(df.close/df.close.shift(fill_value=df.close.iloc[0])).to_numpy()[gate]
    benchmark_bankrupt_date = None
    if direction == 1:
        matched = orders.matched_exposure_logs(benchmark, average_exposure)
    else:
        # QJ's actual API expects LONG LOG returns, not simple returns.
        # A short simple return can be <= -1, so construct it directly.
        benchmark = -df.close.pct_change().fillna(0.).to_numpy()[gate]
        matched, bankrupt_at = short_benchmark_logs(benchmark, average_exposure)
        if bankrupt_at is not None:
            benchmark_bankrupt_date = df.index[gate][bankrupt_at].isoformat()
    eq = equity.to_numpy()[gate]
    peaks = np.maximum.accumulate(np.r_[CAPITAL, eq])[1:]
    gate_dates = df.index[gate]
    closed = [x for x in st.closed if cutoff is None or pd.Timestamp(x['exit_date']) < pd.Timestamp(cutoff)]
    oos = logs.to_numpy()[~gate]
    elapsed = (gate_dates[-1]-gate_dates[0]).total_seconds()/86400+1
    valid = bool(np.isfinite(s).all() and (eq > 0).all())
    metrics = dict(trades=len(closed), win_rate=float(np.mean([x['net_pnl'] > 0 for x in closed])) if closed else None,
                   average_holding_days=float(np.mean([x['holding_days'] for x in closed])) if closed else None,
                   average_exposure=average_exposure, sharpe=orders.sharpe(s, annual_days) if valid else None,
                   benchmark_sharpe=(-math.inf if benchmark_bankrupt_date else
                                     orders.sharpe(matched, annual_days) if np.isfinite(matched).all() else None),
                   cagr=math.expm1(float(s.sum())*365/elapsed) if valid else None,
                   maxdd=float(np.max(1-eq/peaks)), total_return=float(eq[-1]/CAPITAL-1),
                   oos_return=math.expm1(float(oos.sum())) if len(oos) and np.isfinite(oos).all() else None,
                   open_at_gate=bool(abs(sum(x['size'] for x in st.fills if cutoff is None or pd.Timestamp(x['date']) < pd.Timestamp(cutoff))) > 1e-10),
                   bankrupt=not valid)
    if direction == -1:
        metrics.update(benchmark_bankrupt=benchmark_bankrupt_date is not None,
                       benchmark_bankrupt_date=benchmark_bankrupt_date, funding_income=-broker.funding_paid)
    gate_fills = [x for x in st.fills if cutoff is None or pd.Timestamp(x['date']) < pd.Timestamp(cutoff)]
    return dict(engine='backtrader', engine_version=bt.__version__, metrics=metrics,
                params=dict(k_up=k_up, k_dn=k_dn, interval=interval, max_days=max_days,
                            **(dict(direction='short', k_tp=k_up, k_sl=k_dn) if direction == -1 else {})),
                commission=commission, slippage=slippage, annual_days=annual_days,
                gate_start=gate_dates[0].isoformat(), gate_end=gate_dates[-1].isoformat(),
                oos_start=df.index[~gate][0].isoformat() if (~gate).any() else None,
                oos_end=df.index[~gate][-1].isoformat() if (~gate).any() else None,
                exposure_observations=st.observations, gate_entry_fills=sum(x['role'] == 'entry' for x in gate_fills),
                daily_equity=st.daily, daily_exposure=st.exposures, gate_logs=s.tolist(),
                **(dict(daily_simple_returns=simple_returns.to_list()) if direction == -1 else {}),
                matched_benchmark_logs=matched.tolist(), full_benchmark_logs=benchmark.tolist(),
                fills=st.fills, closed=st.closed, order_statuses=st.statuses,
                commission_paid=st.commission, funding_paid=broker.funding_paid,
                funding_days=broker.funding_days,
                funding_missing_days=sum(t.date() not in rates for t in df.index) if rates is not None else 0)


def short_benchmark_logs(simple_short_returns, average_exposure):
    """Exposure-matched SHORT simple returns; ruin loses all capital once.

    The ruin day is -inf in log space; later returns are zero because that
    subaccount stays at zero equity. Its Sharpe is explicitly -inf, even when
    inspecting a later window. No funding is included in this benchmark.
    """
    if not math.isfinite(average_exposure) or average_exposure < 0:
        raise ValueError('平均总敞口须有限且非负')
    returns = average_exposure*np.asarray(simple_short_returns, float)
    if not np.isfinite(returns).all():
        raise ValueError('基准简单收益须有限')
    ruined = np.flatnonzero(returns <= -1)
    at = int(ruined[0]) if len(ruined) else None
    if at is not None:
        returns = returns.copy()
        returns[at], returns[at+1:] = -1., 0.
    with np.errstate(divide='ignore'):
        return np.log1p(returns), at


def equal_weight_logs(series, *, simple_returns=False):
    """Daily equal SIMPLE returns over listed/participating subaccounts.

    A missing index means not listed (or outside that coin's own window),
    whereas an invalid observed return must propagate and fail admission.
    """
    logs = pd.concat(series, axis=1)
    active = pd.concat({k: pd.Series(True, index=s.index) for k, s in series.items()}, axis=1)
    invalid = (logs.isna() & active.eq(True)).any(axis=1)
    simple = (logs if simple_returns else np.expm1(logs)).sum(axis=1)/active.count(axis=1)
    simple[invalid] = np.nan
    ruined = np.flatnonzero(simple.to_numpy() <= -1)
    at = int(ruined[0]) if len(ruined) else None
    if at is not None:
        simple.iloc[at], simple.iloc[at+1:] = -1., 0.
    with np.errstate(divide='ignore', invalid='ignore'):
        result = np.log1p(simple)
    return result, None if at is None else result.index[at].isoformat()


def aggregate_short(runs, part='full'):
    strategy, benchmark = {}, {}
    for asset, run in runs.items():
        dates = pd.DatetimeIndex(pd.to_datetime(list(run['daily_equity'])))
        cut = int(len(dates)*.6)
        sl = {'full': slice(None), 'front': slice(0, cut), 'back': slice(cut, None)}[part]
        # Average actual SIMPLE subaccount returns before taking a log. One
        # coin losing >100% need not bankrupt the whole equal-weight basket.
        simple = run['daily_simple_returns'] if 'daily_simple_returns' in run else np.expm1(run['gate_logs'])
        strategy[asset] = pd.Series(simple, index=dates).iloc[sl]
        benchmark[asset] = pd.Series(run['matched_benchmark_logs'], index=dates).iloc[sl]
    s, strategy_ruin = equal_weight_logs(strategy, simple_returns=True)
    b, benchmark_ruin = equal_weight_logs(benchmark)
    finite = bool(np.isfinite(s).all())
    elapsed = (s.index[-1]-s.index[0]).total_seconds()/86400+1
    equity = CAPITAL*np.exp(s.cumsum().to_numpy())
    peaks = np.maximum.accumulate(np.r_[CAPITAL, equity])[1:]
    return dict(metrics=dict(sharpe=orders.sharpe(s, 365) if finite else None,
                             benchmark_sharpe=-math.inf if benchmark_ruin else orders.sharpe(b, 365),
                             cagr=math.expm1(float(s.sum())*365/elapsed) if finite else None,
                             maxdd=float(np.max(1-equity/peaks)),
                             total_return=math.expm1(float(s.sum())) if finite else None,
                             bankrupt=strategy_ruin is not None or not finite,
                             benchmark_bankrupt=benchmark_ruin is not None,
                             benchmark_bankrupt_date=benchmark_ruin),
                dates=[t.isoformat() for t in s.index], gate_logs=s.tolist(),
                matched_benchmark_logs=b.tolist())


def evaluate_short_gates(runs, basket, platform):
    windows = []
    for label, part in [('前60%', 'front'), ('后40%', 'back')]:
        m = aggregate_short(runs, part)['metrics']
        windows.append(dict(window=label, sharpe=m['sharpe'], benchmark_sharpe=m['benchmark_sharpe']))
    s = np.asarray(basket['gate_logs'], float)
    # Shared DSR computes the daily basket SR internally; cross variance must
    # likewise use unannualized individual-coin SRs, never neighbor SRs.
    daily_srs = {asset: r['metrics']['sharpe']/math.sqrt(365)
                 if r['metrics']['sharpe'] is not None else math.nan for asset, r in runs.items()}
    variance = float(np.var(list(daily_srs.values()), ddof=1))
    dsr = orders.deflated_sharpe(s, P2_TRIALS, variance) if math.isfinite(variance) else None
    invalid_coins = [asset for asset, sr in daily_srs.items() if not math.isfinite(sr)]
    half = len(s)//2
    valid = bool(np.isfinite(s).all())
    halves = [math.expm1(float(x.sum())) if np.isfinite(x).all() else None for x in (s[:half], s[half:])]
    plateau_wins, cross_wins = sum(_beats(x) for x in platform), sum(_beats(x) for x in runs.values())
    gates = dict(G1=bool(valid and all(w['sharpe'] is not None and w['sharpe'] > w['benchmark_sharpe'] for w in windows)),
                 G2=bool(dsr is not None and dsr >= .95), G3=bool(valid and all(x > 0 for x in halves)),
                 G4=plateau_wins/len(platform) >= .70, G5=cross_wins/len(runs) >= .60)
    return dict(gates=gates, verdict='PASS' if all(gates.values()) else 'FAIL',
                failed=[g for g, passed in gates.items() if not passed], windows=windows,
                dsr=dsr, trials=P2_TRIALS, cross_daily_sharpe_variance=variance,
                dsr_invalid_coins=invalid_coins,
                dsr_unavailable_reason='子账户权益非正，32币日对数Sharpe方差无法定义；不剔除、不追加资金，G2失败' if invalid_coins else None,
                cross_daily_sharpes=daily_srs, halves=halves,
                platform_wins=plateau_wins, platform_total=len(platform),
                cross_wins=cross_wins, cross_total=len(runs))


def study_short(root, progress=print):
    # P2 owns membership. Its pre-2026 price slices are deliberately NOT used:
    # SHORT's predeclared sample includes the latest available daily bar.
    _, _, meta = probability.load_data(root, (), progress)
    basket = meta['basket']
    frames, scales, funding, missing = {}, {}, {}, []
    runs = {}
    for asset in basket:
        frames[asset] = volrix.load_frame(root/'factor_mining/crypto/daily'/f'{asset}.json', '1d')
        scales[asset] = probability.entry_scales(frames[asset], frames[asset], '1d')
        path = root/'factor_mining/crypto/alt_daily/funding'/f'{asset}.json'
        if path.is_file():
            funding[asset] = volrix.load_funding(root, asset)
        else:
            funding[asset] = {}
            missing.append(asset)

    def run_asset(asset, tp=2., sl=1., interval=5):
        return run_barrier(frames[asset], direction=-1, k_tp=tp, k_sl=sl, interval=interval,
                           cutoff=None, commission=.0005, slippage=.0003, annual_days=365,
                           funding=funding[asset], scales=scales[asset])

    for asset in basket:
        progress(f'BARRIER_SHORT {asset}')
        runs[asset] = run_asset(asset)
        dates = frames[asset].index
        cut = int(len(dates)*.6)
        runs[asset]['sample_split'] = dict(bars=len(dates), front_bars=cut, back_bars=len(dates)-cut,
                                          front_end=dates[cut-1].isoformat(), back_start=dates[cut].isoformat())
    combined = aggregate_short(runs)
    platform = []
    for tp, sl in SHORT_PLATFORM:
        for interval in INTERVALS:
            progress(f'CRYPTO_BASKET platform ({tp},{sl}) interval={interval}')
            neighbors = runs if (tp, sl, interval) == (2., 1., 5) else {
                asset: run_asset(asset, tp, sl, interval) for asset in basket}
            combo = combined if neighbors is runs else aggregate_short(neighbors)
            # Preserve auditable scores/exposures without serializing 672 full
            # native order paths. Main-rule per-coin paths remain in JSON.
            platform.append(dict(params=dict(k_tp=tp, k_sl=sl, interval=interval), metrics=combo['metrics'],
                                 coins={asset: r['metrics'] for asset, r in neighbors.items()}))
    admission = evaluate_short_gates(runs, combined, platform)
    return dict(rule='BARRIER_SHORT', thresholds=SHORT_THRESHOLDS, basket=basket, latest=meta['latest'],
                runs=runs, portfolio=combined, platform=platform, admission=admission,
                missing_funding=missing, benchmark_funding_included=False)


def _beats(run):
    m = run['metrics']
    return bool(m['sharpe'] is not None and m['benchmark_sharpe'] is not None and m['sharpe'] > m['benchmark_sharpe'])


def evaluate_gates(etfs, platform):
    main = etfs['EFA']
    s, b = (np.asarray(main[k], float) for k in ('gate_logs', 'matched_benchmark_logs'))
    cut, half = int(len(s)*.6), len(s)//2
    windows = [dict(window=name, sharpe=orders.sharpe(s[sl], 252), benchmark_sharpe=orders.sharpe(b[sl], 252))
               for name, sl in [('前60%', slice(0, cut)), ('后40%', slice(cut, None))]]
    # DSR implementation expects UNANNUALIZED daily SR variance, as in QJ.
    daily_srs = [x['metrics']['sharpe']/math.sqrt(252) for x in etfs.values()]
    variance = float(np.var(daily_srs, ddof=1))
    dsr = orders.deflated_sharpe(s, P2_TRIALS, variance)
    halves = [math.expm1(float(x.sum())) for x in (s[:half], s[half:])]
    plateau_wins = sum(_beats(x) for x in platform)
    cross_wins = sum(_beats(x) for x in etfs.values())
    valid = bool(np.isfinite(s).all() and np.isfinite(b).all())
    gates = dict(G1=bool(valid and all(x['sharpe'] > x['benchmark_sharpe'] for x in windows)),
                 G2=bool(dsr >= .95), G3=bool(valid and all(x > 0 for x in halves)),
                 G4=plateau_wins/len(platform) >= .70, G5=cross_wins/len(US10) >= .60)
    return dict(gates=gates, verdict='PASS' if all(gates.values()) else 'FAIL',
                failed=[g for g, passed in gates.items() if not passed], windows=windows,
                dsr=dsr, trials=P2_TRIALS, cross_daily_sharpe_variance=variance,
                cross_daily_sharpes=dict(zip(etfs, daily_srs)), halves=halves,
                platform_wins=plateau_wins, platform_total=len(platform),
                cross_wins=cross_wins, cross_total=len(US10))


def study(root=volrix.DATA_ROOT, progress=print):
    root = Path(root)
    frames, etfs = {}, {}
    for asset in US10:
        progress(f'BARRIER_LONG {asset}')
        frames[asset] = probability.load_us_frame(root/'quantjourney/prices'/f'{asset}.json')
        etfs[asset] = run_barrier(frames[asset])
    platform = []
    for ku, kd in PLATFORM:
        for interval in INTERVALS:
            progress(f'EFA platform ({ku},{kd}) interval={interval}')
            # Truncate future bars before running each neighbor. No endpoint
            # liquidation; prefix equals the gate prefix of a continuous run.
            run = etfs['EFA'] if (ku, kd, interval) == (2., 1., 5) else run_barrier(
                frames['EFA'].loc[lambda d: d.index < probability.CUTOFF], k_up=ku, k_dn=kd, interval=interval)
            platform.append(run)
    crypto = {}
    for asset in probability.CRYPTO_MAIN:
        progress(f'BARRIER_LONG {asset} (仅报告)')
        df = volrix.load_frame(root/'factor_mining/crypto/daily'/f'{asset}.json', '1d')
        crypto[asset] = run_barrier(df, commission=.0005, slippage=.0003,
                                    funding=volrix.load_funding(root, asset), annual_days=365)
    return dict(rule='BARRIER_LONG', thresholds=THRESHOLDS, etfs=etfs, platform=platform,
                crypto=crypto, admission=evaluate_gates(etfs, platform), barrier_short=study_short(root, progress))


def main_table(runs):
    rows = []
    for asset, run in runs.items():
        m = run['metrics']
        rows.append([asset, m['trades'], None if m['win_rate'] is None else m['win_rate']*100,
                     m['average_holding_days'], m['average_exposure']*100, m['sharpe'],
                     m['benchmark_sharpe'], None if m['cagr'] is None else m['cagr']*100,
                     m['maxdd']*100, None if m['oos_return'] is None else m['oos_return']*100])
    return probability.table(['资产', '笔数', '胜率%', '平均持有天数', '平均敞口%', 'Sharpe',
                              '同敞口基准Sharpe', 'CAGR%', 'MaxDD%', '2026样本外收益%'], rows)


def short_coin_table(runs):
    rows = []
    for asset, run in runs.items():
        m = run['metrics']
        rows.append([asset, m['trades'], None if m['win_rate'] is None else 100*m['win_rate'],
                     m['average_holding_days'], 100*m['average_exposure'], m['sharpe'],
                     m['benchmark_sharpe'], m['funding_income'], '是' if m['benchmark_bankrupt'] else '否'])
    return probability.table(['资产', '笔数', '胜率%', '平均持有天数', '平均敞口%', 'Sharpe',
                              '同敞口空头基准Sharpe', '资金费收入', '基准破产'], rows)


def render_short_report(result):
    f = probability.fmt
    a, m = result['admission'], result['portfolio']['metrics']
    status = lambda key: 'PASS' if a['gates'][key] else 'FAIL'
    windows = a['windows']
    main = probability.table(['组合', 'Sharpe', '同敞口空头基准Sharpe', 'CAGR%', 'MaxDD%',
                              '前60%策略/基准Sharpe', '后40%策略/基准Sharpe'],
                             [['CRYPTO_BASKET', m['sharpe'], m['benchmark_sharpe'],
                               None if m['cagr'] is None else 100*m['cagr'], 100*m['maxdd'],
                               *[f(w['sharpe'])+'/'+f(w['benchmark_sharpe']) for w in windows]]])
    coins = short_coin_table(result['runs'])
    gates = [
        f"- G1：{status('G1')}；"+'；'.join(f"{w['window']}策略/同敞口空头基准Sharpe={f(w['sharpe'])}/{f(w['benchmark_sharpe'])}" for w in windows)+'。',
        f"- G2：{status('G2')}；DSR={format(a['dsr'], '.6f') if a['dsr'] is not None else 'NA'}，要求≥0.95；N={a['trials']}，{a['cross_total']}币日Sharpe样本方差={format(a['cross_daily_sharpe_variance'], '.8f') if math.isfinite(a['cross_daily_sharpe_variance']) else 'NA'}。"+
        (f"{a['dsr_unavailable_reason']}（{'、'.join(a['dsr_invalid_coins'])}）。" if a['dsr_invalid_coins'] else ''),
        f"- G3：{status('G3')}；前/后两半收益={f(100*a['halves'][0]) if a['halves'][0] is not None else 'NA'}%/{f(100*a['halves'][1]) if a['halves'][1] is not None else 'NA'}%，均须>0。",
        f"- G4：{status('G4')}；参数平台胜出={a['platform_wins']}/{a['platform_total']}（{f(100*a['platform_wins']/a['platform_total'])}%），要求≥70%。",
        f"- G5：{status('G5')}；CRYPTO_BASKET胜出={a['cross_wins']}/{a['cross_total']}（{f(100*a['cross_wins']/a['cross_total'])}%），要求≥60%。",
        f"- 汇总：{a['verdict']}；未通过：{','.join(a['failed']) if a['failed'] else '无'}。"]
    result['report_sections'] = dict(main_table=main, coin_table=coins, gate_summary='\n'.join(gates))
    lines = ['## BARRIER_SHORT 预声明门槛检验', '', '```text', SHORT_THRESHOLDS, '```', '',
             f"CRYPTO_BASKET：{len(result['basket'])}币；最新日 {result['latest']}。名单完全复用probability_study.load_data的P2规则：2022-01-01前上市且至今仍存活。幸存者偏差对做空不利（偏保守）；但2022年后山寨币整体下跌的漂移会同时进入策略与基准。", '',
             '### 篮子主表', '', main, '',
             '每币独立Cerebro、初始1000；当日所有已上市币子账户简单收益等权平均，再取log1p；未上市前不计入分母，预热与空仓日计入。G1先按每币自身长度floor(0.6×n)切分连续回测收益，再各自对齐日历聚合两窗；两窗均继承真实持仓，不重启、不强制平仓。G3按完整篮子日历序列前后两半（floor(n/2)）计算。Sharpe按365年化、日对数收益、无风险利率0；CAGR按实际自然日。', '',
             '空仓时从该币第一根起每interval根收盘决策，按决策权益/收盘价确定100%名义数量，次开盘市价卖空。s完全复用P2 entry_scales；止盈买入Limit=决策收盘价×exp(−k_tp·s)，止损买入Stop=决策收盘价×exp(k_sl·s)。收到入场成交通知后提交原生OCO，次根生效（入场K线尚无保护）；止损先提交，双触发先止损；止损跳空按原生开盘价与3bp滑点撮合。成交起满30自然日仍未触发则收盘撤OCO，次开盘平仓。期末未平仓按原生权益计价。佣金5bp/边。', '',
             '空头基准每币用全门槛样本平均总敞口e，计算log1p(e×(−r_t))，首日0；G1两窗共用全样本e，平台每组合使用各币自身e。基准不计交易成本、不计资金费（策略计真实资金费）。quantjourney_orders.matched_exposure_logs实际接收多头对数收益，因此这里直接计算空头简单收益，避免错误转换。', '',
             '若e×(−r_t)≤−1，该币基准当日归零并永久破产，Sharpe=−∞；归零日子账户简单收益记−100%，后续为0，不复活。篮子仍按相同已上市币分母等权聚合；某币破产不等同于整个篮子破产，篮子仅在自身日收益≤−100%时归零。JSON中负无穷序列化为字符串"-inf"。', '',
             '### 逐币表', '', coins, '',
             '笔数、胜率、平均持有天数只含已平仓交易；胜率按扣佣金及资金费后的净盈亏。平均敞口含预热和空仓日，按收盘总名义/当前权益计算。资金费收入=−FundingBroker.funding_paid，可为负；正费率时size<0令cost<0，broker扣除负cost即增加权益。每日按隔夜仓位×开盘价×缓存平均费率×3结算，包含平仓当天、不含入场当天。', '',
             '策略权益非正的币：'+('、'.join(a['dsr_invalid_coins']) or '无')+'。不剔除、不追加资金；停止新入场，已有OCO/超时按原生规则退出。该币对数Sharpe不可定义，G2不以剩余币替代32币方差。篮子严格先聚合原生NAV的日简单收益（NAV_t/NAV_{t−1}−1）再取log1p；负权益后的比值仍保留，故含破产子账户的篮子统计仅作描述，不能解读为可持续可投资收益。原生NAV与资金费、成交明细均保存在JSON中。', '',
             '### G1–G5 / PASS 汇总', '', *gates, '',
             '### 参数平台（预声明21组合）', '',
             probability.table(['k_tp', 'k_sl', 'interval', '篮子Sharpe', '篮子基准Sharpe', '胜出'],
                               [[r['params']['k_tp'], r['params']['k_sl'], r['params']['interval'],
                                 r['metrics']['sharpe'], r['metrics']['benchmark_sharpe'],
                                 '是' if _beats(r) else '否'] for r in result['platform']]), '',
             '### BNB、DOGE、XLM（事后选出、存在选择偏差；仅单列描述，不增设门槛）', '',
             short_coin_table({s: result['runs'][s+'USDT'] for s in ('BNB', 'DOGE', 'XLM')}), '',
             '### BTC、ETH（事后选出、存在选择偏差；仅单列描述，不增设门槛）', '',
             short_coin_table({s: result['runs'][s+'USDT'] for s in ('BTC', 'ETH')}), '',
             '以上币仍按原篮子成员身份进入G2/G5；单列表不构成额外检验或筛选。', '',
             '### 资金费缓存与样本覆盖', '',
             '缺资金费缓存币（全程按0资金费，未剔除）：'+('、'.join(result['missing_funding']) or '无')+'。', '',
             probability.table(['资产', '开始', '最新', '前60%根数', '后40%根数', '后40%首日', '缺费率日', '基准破产日'],
                               [[asset, r['gate_start'][:10], r['gate_end'][:10], r['sample_split']['front_bars'],
                                 r['sample_split']['back_bars'], r['sample_split']['back_start'][:10],
                                 r['funding_missing_days'], r['metrics']['benchmark_bankrupt_date']]
                                for asset, r in result['runs'].items()]), '']
    return '\n'.join(lines)+'\n'


def render_report(result):
    f = probability.fmt
    a = result['admission']
    g = a['gates']
    status = lambda key: 'PASS' if g[key] else 'FAIL'
    gate_lines = [
        f"- G1：{status('G1')}；"+'；'.join(f"{w['window']}策略/同敞口基准Sharpe={f(w['sharpe'])}/{f(w['benchmark_sharpe'])}" for w in a['windows'])+'。',
        f"- G2：{status('G2')}；DSR={a['dsr']:.6f}，要求≥0.95；N={a['trials']}，US10日Sharpe样本方差={a['cross_daily_sharpe_variance']:.8f}。",
        f"- G3：{status('G3')}；前/后两半收益={f(100*a['halves'][0])}%/{f(100*a['halves'][1])}%，均须>0。",
        f"- G4：{status('G4')}；参数平台胜出={a['platform_wins']}/{a['platform_total']}（{f(100*a['platform_wins']/a['platform_total'])}%），要求≥70%。",
        f"- G5：{status('G5')}；US10胜出={a['cross_wins']}/{a['cross_total']}（{f(100*a['cross_wins']/a['cross_total'])}%），要求≥60%。",
        f"- 汇总：{a['verdict']}；未通过：{','.join(a['failed']) if a['failed'] else '无'}。"]
    main = main_table(result['etfs'])
    result['report_sections'] = dict(main_table=main, gate_summary='\n'.join(gate_lines))
    lines = ['# BARRIER_LONG 预声明门槛回测', '', '```text', THRESHOLDS, '```', '',
             '## ETF 主表', '', main, '',
             '笔数、胜率、平均持有天数仅统计门槛期内已平仓交易；门槛末未平仓按原生权益计价进入收益。平均敞口为门槛样本各交易日收盘总名义持仓/当前策略NAV的算术均值，包含空仓和预热日；100%仓位按决策收盘权益/收盘价确定数量，开盘跳空后实际敞口可超过100%。', '',
             '同敞口基准复用quantjourney_orders.matched_exposure_logs：log1p(门槛期平均总敞口×同标的每日简单收益)，首日为0。G1两窗共用该门槛期平均敞口；G4每个组合各用其自身敞口。Sharpe用日对数收益，ETF按252、加密按365年化，无风险利率0；CAGR按实际自然日年化。', '',
             '入场前30自然日日对数收益标准差完全复用P2 entry_scales。收盘决策、下一根开盘市价买入；收到入场成交通知后挂原生Stop和Limit OCO，次根开始撮合，因此入场K线内尚无保护单。双触发按原生提交顺序先止损。满30自然日收盘仍持仓则撤OCO、次开盘市价平仓。止损跳空由原生broker按开盘加配置滑点撮合，限价价格改善/滑点上限也沿用原生规则。', '',
             '## EFA G1–G5', '', *gate_lines, '',
             '## EFA 参数平台（固定15组合）', '',
             probability.table(['k_up', 'k_dn', 'interval', 'Sharpe', '同敞口基准Sharpe', '胜出'],
                               [[r['params']['k_up'], r['params']['k_dn'], r['params']['interval'],
                                 r['metrics']['sharpe'], r['metrics']['benchmark_sharpe'], '是' if _beats(r) else '否'] for r in result['platform']]), '',
             '## BTC/ETH（仅报告，不进任何门槛）', '', main_table(result['crypto']), '',
             '资金费沿用volrix.FundingBroker：每日首根开盘前的隔夜仓位×开盘价×当日缓存费率×3；原生现金扣款。胜率的已平仓净盈亏包括对应持仓期资金费。以下费用总额含2026连续运行段。', '']
    for asset, run in result['crypto'].items():
        lines.append(f"- {asset}：佣金={f(run['commission_paid'])}，资金费={f(run['funding_paid'])}，资金费缓存缺失日={run['funding_missing_days']}。")
    lines += ['', '## 样本覆盖', '',
              '2026收益为连续运行中从门槛末权益到最新可用权益的简单收益，保留跨年持仓与历史入场相位；该段所有收益、敞口和成交均不进入门槛。此日历留出段仅作描述。虚拟大额备用现金只防止100%仓位被费用或跳空拒单，策略NAV始终剔除备用现金。', '']
    for asset, run in {**result['etfs'], **result['crypto']}.items():
        lines.append(f"- {asset}：门槛 {run['gate_start'][:10]} 至 {run['gate_end'][:10]}；2026起 {str(run['oos_start'])[:10]} 至 {str(run['oos_end'])[:10]}；入场成交/已平仓={run['gate_entry_fills']}/{run['metrics']['trades']}，门槛末持仓={'是' if run['metrics']['open_at_gate'] else '否'}。")
    report = '\n'.join(lines)+'\n'
    if 'barrier_short' in result:
        report += '\n'+render_short_report(result['barrier_short'])
    return report


def short_json_safe(value):
    """Standards-compliant JSON with explicit, auditable bankrupt Sharpes."""
    if isinstance(value, dict):
        return {str(k): short_json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [short_json_safe(v) for v in value]
    if isinstance(value, (float, np.floating)) and value == -math.inf:
        return '-inf'
    return probability.json_safe(value)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', default='.prob_out/barrier.md')
    parser.add_argument('--json', default='.prob_out/barrier.json')
    parser.add_argument('--data-root', type=Path, default=volrix.DATA_ROOT)
    args = parser.parse_args(argv)
    out, json_path = (probability._output_path(x, args.data_root) for x in (args.out, args.json))
    if out == json_path:
        parser.error('--out和--json须不同')
    start = time.perf_counter()
    result = study(args.data_root, lambda x: print(x, flush=True))
    result['elapsed_seconds'] = time.perf_counter()-start
    report = render_report(result)
    for path in (out, json_path):
        path.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(report, encoding='utf-8')
    safe = probability.json_safe(result)
    safe['barrier_short'] = short_json_safe(result['barrier_short'])
    json_path.write_text(json.dumps(safe, ensure_ascii=False, indent=2, allow_nan=False)+'\n', encoding='utf-8')
    print(f'完成 elapsed={result["elapsed_seconds"]:.3f}s', flush=True)
    print(result['report_sections']['gate_summary'], flush=True)
    print('BARRIER_SHORT CRYPTO_BASKET', flush=True)
    print(result['barrier_short']['report_sections']['main_table'], flush=True)
    print(result['barrier_short']['report_sections']['gate_summary'], flush=True)
    print('缺资金费缓存币（按0）：'+('、'.join(result['barrier_short']['missing_funding']) or '无'), flush=True)
    print(f'Markdown: {out}\nJSON: {json_path}', flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
