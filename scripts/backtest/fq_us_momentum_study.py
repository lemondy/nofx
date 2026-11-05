"""Frozen fq_us S1 screen. All execution and accounting use Backtrader.

No fetching, parameter search, or writes to the input snapshot are performed.
The additional same-close feed callback only lets native CoC orders settle;
features and the rebalance clock always use the original daily observations.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

from . import evolution_ledger
from .backtrader_adapter import _backtrader
from scripts.factor_mining.quant_targets import _capped_invvol


PREREG_PATH = 'docs/research/2026-11-05-fq-us-momentum-prereg.md'
TYPE_FILE = 'bstock_instrument_types_20261105.json'
MAP_FILE = 'usstock_symbols.json'
EXPECTED_HASHES = {
    'daily': '5bd711af9dbeef6d2131005aea89a5fc65897e3d47485145bd3868448d3e8042',
    TYPE_FILE: '44a9695513d538f8a46bc73d1def61afc218723d5b706fd76f62ab06386a57be',
    MAP_FILE: 'aef048519a33e13d8243b4f74b8714256b1522cb67422d29be6304e2d557e286',
}
PARAMS = dict(
    rule='mom_ls_invvol_v1', universe='snapshot instrumentType == EQUITY',
    expected_universe_size=90, min_valid_bars=253, min_close=5.0,
    liquidity_window=20, min_median_dollar_volume=10_000_000.0,
    signal='close[t-21] / close[t-252] - 1', rebalance_trading_days=20,
    min_eligible=15, quantile=0.2, volatility_window=30, volatility_ddof=1,
    long_gross=0.5, short_gross=0.5, cap=0.10, sleeve_cap=0.20,
    initial_cash=100_000.0, long_initial_cash=50_000.0, short_initial_cash=50_000.0,
    long_cost_per_fill=0.0013, short_cost_per_fill=0.0007,
    funding_rate=0.0, funding_stress_annual_rate=0.10,
    z_threshold=1.0, period_sd_ddof=1, baseline_period_return=0.0,
    variant_L=dict(rule='long_invvol_v1', gross=1.0, cap=0.10, cost=0.0013),
)


def input_hashes(snapshot):
    snapshot = Path(snapshot)
    files = sorted((snapshot / 'daily').glob('*.json'), key=lambda p: p.name)
    if not files:
        raise ValueError('daily/*.json is empty')
    digest = hashlib.sha256()
    for path in files:
        digest.update(path.read_bytes())
    return {'daily': digest.hexdigest(), **{
        name: hashlib.sha256((snapshot / name).read_bytes()).hexdigest()
        for name in (TYPE_FILE, MAP_FILE)}}


def guard_hashes(snapshot, allow_input_drift=False):
    actual = input_hashes(snapshot)
    mismatches = {k: dict(expected=v, actual=actual[k])
                  for k, v in EXPECTED_HASHES.items() if actual[k] != v}
    if mismatches and not allow_input_drift:
        raise ValueError('preregistered input hash mismatch: ' + json.dumps(mismatches))
    return actual, mismatches


def valid_rows(frame):
    values = frame[['open', 'high', 'low', 'close', 'volume']]
    prices = values[['open', 'high', 'low', 'close']]
    return (np.isfinite(values).all(axis=1) & prices.gt(0).all(axis=1)
            & values.volume.ge(0)
            & values.high.ge(prices[['open', 'low', 'close']].max(axis=1) - 1e-8)
            & values.low.le(prices[['open', 'high', 'close']].min(axis=1) + 1e-8))


def load_snapshot(snapshot):
    snapshot = Path(snapshot)
    types = json.loads((snapshot / TYPE_FILE).read_text())
    universe = sorted(s for s, kind in types.items() if kind == 'EQUITY')
    if len(universe) != PARAMS['expected_universe_size']:
        raise ValueError(f'expected 90 EQUITY instruments, got {len(universe)}')
    mapping = json.loads((snapshot / MAP_FILE).read_text())
    perps = {r['underlying']: r.get('perp_symbol') for r in mapping['symbols']}
    frames, invalid = {}, {}
    for symbol in universe + ['SPY']:
        rows = json.loads((snapshot / 'daily' / f'{symbol}.json').read_text())
        frame = pd.DataFrame([r[:6] for r in rows],
                             columns=['date', 'open', 'high', 'low', 'close', 'volume'])
        frame.index = pd.DatetimeIndex(pd.to_datetime(frame.pop('date')))
        if not frame.index.is_monotonic_increasing or frame.index.has_duplicates:
            raise ValueError(f'{symbol}: dates must be unique and ascending')
        frame = frame.astype(float)
        valid = valid_rows(frame)
        invalid[symbol] = int((~valid).sum())
        frames[symbol] = frame.where(valid, np.nan)
    calendar = frames['SPY'].dropna().index
    if calendar.empty:
        raise ValueError('SPY has no valid trading calendar')
    quality = dict(invalid_rows=invalid, off_calendar_rows={
        s: int((~f.index.isin(calendar)).sum()) for s, f in frames.items()})
    return universe, {s: perps.get(s) for s in universe}, frames, calendar, quality


def build_features(frames, universe, calendar):
    """Causal daily features; missing rows never count as valid history."""
    close = pd.DataFrame({s: frames[s].close.where(valid_rows(frames[s]))
                          for s in universe}).reindex(calendar)
    volume = pd.DataFrame({s: frames[s].volume for s in universe}).reindex(calendar)
    dollars = close * volume
    median = dollars.rolling(PARAMS['liquidity_window'], min_periods=20).median()
    count = close.notna().cumsum()
    eligible = (count.ge(PARAMS['min_valid_bars']) & close.ge(PARAMS['min_close'])
                & median.ge(PARAMS['min_median_dollar_volume']))
    return dict(close=close, valid_count=count, median_dollar_volume=median,
                eligible=eligible, signal=close.shift(21) / close.shift(252) - 1,
                volatility=close.pct_change(fill_method=None).rolling(30, min_periods=30)
                .std(ddof=1))


def decision_at(features, date):
    eligible = features['eligible'].loc[date]
    symbols = sorted(eligible.index[eligible])
    result = dict(date=str(pd.Timestamp(date).date()), eligible=symbols,
                  eligible_count=len(symbols), quintile_count=0,
                  signals={}, volatility={}, long={}, short={}, L={})
    if len(symbols) < PARAMS['min_eligible']:
        return result
    signals = features['signal'].loc[date, symbols]
    if not np.isfinite(signals).all():
        raise ValueError(f'{date}: eligible instrument has undefined momentum')
    ordered = signals.sort_values(kind='stable')
    n = max(1, math.floor(0.2 * len(symbols)))
    longs, shorts = list(ordered.index[-n:]), list(ordered.index[:n])
    vol = features['volatility'].loc[date, sorted(set(longs + shorts))]
    if not (np.isfinite(vol) & vol.gt(0)).all():
        raise ValueError(f'{date}: selected instrument has undefined inverse volatility')
    result.update(quintile_count=n, signals=signals.to_dict(), volatility=vol.to_dict(),
                  long=_capped_invvol(vol.loc[longs], 1.0, 0.20).to_dict(),
                  short=(-_capped_invvol(vol.loc[shorts], 1.0, 0.20)).to_dict(),
                  L=_capped_invvol(vol.loc[longs], 1.0, 0.10).to_dict())
    return result


def rebalance_schedule(features, calendar):
    counts = features['eligible'].sum(axis=1)
    possible = counts.index[counts.ge(PARAMS['min_eligible'])]
    if possible.empty:
        raise ValueError('no date has at least 15 eligible instruments')
    start = calendar.get_loc(possible[0])
    dates = calendar[start::PARAMS['rebalance_trading_days']]
    if len(dates) < 3:
        raise ValueError('at least two complete periods are required')
    sessions = calendar[start:calendar.get_loc(dates[-1]) + 1]
    return sessions, [decision_at(features, d) for d in dates]


def run_sleeve(frames, sessions, targets, *, cash, commission, short=False):
    """Execute target percentages using native BackBroker, including fees.

    A second callback at close+1 second has identical prices and no new data.
    BackBroker settles CoC orders on that callback at their creation close.
    The audit reads the settled native account, including on the final date.
    """
    bt = _backtrader()

    class FractionalCommission(bt.CommInfoBase):
        def getsize(self, price, cash):
            return self.p.leverage * cash / price

    class CashBoundFiller:
        def __init__(self, broker):
            self.broker, self.adjustments = broker, []

        def __call__(self, order, price, ago):
            requested = abs(order.executed.remsize)
            size = requested
            if not short and order.isbuy():
                ci = self.broker.getcommissioninfo(order.data)
                affordable = max(0.0, self.broker.getcash()) / (price + ci.getcommission(1, price))
                size = min(size, affordable * (1 - 1e-14))
            if size < requested * (1 - 1e-10):
                self.adjustments.append(dict(date=bt.num2date(order.created.dt).date().isoformat(),
                    symbol=order.data._name, requested_size=requested, filled_size=size))
            return size

    class Audit(bt.Analyzer):
        def start(self):
            self.curve = []

        def next(self):
            if self.strategy.datetime.datetime().second != 1:
                return
            broker = self.strategy.broker
            positions = {d._name: dict(size=p.size, price=float(d.close[0]),
                                      value=broker.getvalue(datas=[d]))
                         for d, p in broker.positions.items() if p.size}
            if any(p['size'] > 0 if short else p['size'] < 0 for p in positions.values()):
                raise RuntimeError('sleeve position has the wrong sign')
            if not short and broker.getcash() < -1e-7:
                raise RuntimeError('long sleeve exceeded native cash')
            self.curve.append(dict(date=self.strategy.datetime.date().isoformat(),
                equity=broker.getvalue(), cash=broker.getcash(), positions=positions,
                short_notional=-sum(p['value'] for p in positions.values()) if short else 0.0))

        def get_analysis(self):
            return self.curve

    class Sleeve(bt.Strategy):
        def __init__(self):
            self.feeds = {d._name: d for d in self.datas}
            self.orders, self.fills, self.order_events = [], [], []

        def next(self):
            date = self.datetime.date().isoformat()
            if self.datetime.datetime().second == 1:
                for order in self.orders:
                    if order.alive():
                        self.cancel(order)
                self.orders = []
                return
            if date not in targets:
                return
            weights = targets[date]
            if any(not math.isfinite(w) or (w > 0 if short else w < 0)
                   for w in weights.values()) or sum(abs(w) for w in weights.values()) > 1 + 1e-10:
                raise ValueError('invalid sleeve target percentages')
            equity = self.broker.getvalue()
            if equity <= 0:
                raise RuntimeError('sleeve equity is nonpositive')
            names = set(weights) | {s for s, d in self.feeds.items() if self.getposition(d).size}
            # Ascending signed changes submit every sell before every buy.
            changes = {s: weights.get(s, 0.0) * equity
                       - self.broker.getvalue(datas=[self.feeds[s]]) for s in names}
            for symbol in sorted(names, key=lambda s: (changes[s], s)):
                data = self.feeds[symbol]
                if not math.isfinite(data.close[0]) or data.close[0] <= 0:
                    raise ValueError(f'{date}: cannot trade unpriced {symbol}')
                order = self.order_target_percent(data=data, target=weights.get(symbol, 0.0))
                if order is not None:
                    self.orders.append(order)

        def notify_order(self, order):
            self.order_events.append(dict(symbol=order.data._name, status=order.getstatusname(),
                                         date=self.datetime.date().isoformat()))
            if order.status in (order.Margin, order.Rejected, order.Expired):
                raise RuntimeError(f'native order failed: {order.data._name} {order.getstatusname()}')
            if order.status in (order.Completed, order.Partial) and order.executed.size:
                if order.status == order.Partial and order.executed.remsize == 0:
                    raise RuntimeError('unexpected zero remainder')
                self.fills.append(dict(symbol=order.data._name,
                    date=bt.num2date(order.executed.dt).date().isoformat(),
                    execution_time=bt.num2date(order.executed.dt).isoformat(),
                    size=order.executed.size, price=order.executed.price,
                    notional=abs(order.executed.size * order.executed.price),
                    commission=order.executed.comm, status=order.getstatusname()))

    cerebro = bt.Cerebro(stdstats=False)
    cerebro.broker.setcash(cash)
    cerebro.broker.set_coc(True)
    cerebro.broker.set_shortcash(True)
    cerebro.broker.set_checksubmit(False)
    cerebro.broker.addcommissioninfo(FractionalCommission(
        commission=commission, stocklike=True, commtype=bt.CommInfoBase.COMM_PERC,
        percabs=True, interest=0.0, leverage=1.0))
    filler = CashBoundFiller(cerebro.broker)
    cerebro.broker.set_filler(filler)
    # Unlisted names get a finite inert mark, but are never orderable. Later
    # missing marks only carry a previously observed close, never a future row.
    for symbol in sorted(frames):
        frame = frames[symbol].reindex(sessions).copy()
        known = frames[symbol].close.reindex(frames[symbol].index.union(sessions)).ffill()
        mark = known.reindex(sessions).fillna(1.0)
        for column in ('open', 'high', 'low', 'close'):
            frame[column] = frame[column].fillna(mark)
        frame['volume'] = frame.volume.fillna(0.0)
        settlement = frame.copy()
        for column in ('open', 'high', 'low'):
            settlement[column] = settlement.close
        settlement['volume'] = 0.0
        settlement.index = pd.DatetimeIndex(
            settlement.index.to_numpy(dtype='datetime64[ns]') + np.timedelta64(1, 's'))
        feed = pd.concat([frame, settlement]).sort_index()
        cerebro.adddata(bt.feeds.PandasData(dataname=feed, openinterest=None,
                        timeframe=bt.TimeFrame.Seconds), name=symbol)
    cerebro.addstrategy(Sleeve)
    cerebro.addanalyzer(Audit, _name='audit')
    cerebro.addanalyzer(bt.analyzers.DrawDown, _name='drawdown')
    strategy = cerebro.run(runonce=False)[0]
    curve = strategy.analyzers.audit.get_analysis()
    if len(curve) != len(sessions):
        raise RuntimeError('native audit does not cover the evaluation calendar')
    return dict(engine='backtrader', engine_version=bt.__version__, initial_cash=cash,
        commission_rate=commission, short=short, equity_curve=curve,
        fills=strategy.fills, order_events=strategy.order_events,
        cash_size_adjustments=filler.adjustments,
        native_drawdown=dict(strategy.analyzers.drawdown.get_analysis()),
        metrics=path_metrics(curve, cash, strategy.fills))


def path_metrics(curve, initial_cash, fills=()):
    """Describe already observed broker equity and fills, without simulation."""
    previous, peak, drawdown, yearly = initial_cash, initial_cash, 0.0, {}
    traded = {}
    for fill in fills:
        traded[fill['date']] = traded.get(fill['date'], 0.0) + fill['notional']
    turnover = 0.0
    year_start, last_year = initial_cash, None
    for row in curve:
        equity, year = row['equity'], row['date'][:4]
        if year != last_year:
            year_start, last_year = previous, year
        yearly[year] = equity / year_start - 1
        peak = max(peak, equity)
        drawdown = max(drawdown, 1 - equity / peak)
        turnover += traded.get(row['date'], 0.0) / previous
        previous = equity
    return dict(total_return=previous / initial_cash - 1, final_equity=previous,
                yearly_returns=yearly, max_drawdown=drawdown,
                traded_notional=sum(traded.values()), turnover=turnover,
                annualized_turnover=turnover * 252 / len(curve) if curve else 0.0,
                commission=sum(f['commission'] for f in fills))


def period_statistics(returns):
    values = np.asarray(returns, dtype=float)
    if not np.isfinite(values).all():
        raise ValueError('period returns must be finite')
    n = len(values)
    mean = float(values.mean()) if n else None
    sd = float(values.std(ddof=1)) if n >= 2 else None
    z = (mean / (sd / math.sqrt(n)) if sd and sd > 0 else
         math.copysign(math.inf, mean) if n >= 2 and mean else None)
    split = n // 2
    halves = dict(first=dict(n=split, mean=float(values[:split].mean()) if split else None),
                  second=dict(n=n-split, mean=float(values[split:].mean()) if n-split else None))
    passed = (z is not None and z >= 1.0 and halves['first']['mean'] is not None
              and halves['first']['mean'] > 0 and halves['second']['mean'] > 0)
    return dict(n=n, mean=mean, sd=sd, z=z, halves=halves,
                verdict='screen_pass' if passed else 'fail')


def period_table(curve, decisions):
    by_date = {r['date']: r for r in curve}
    return [dict(k=k, start=a['date'], end=b['date'], trading_days=20,
                 start_equity=by_date[a['date']]['equity'], end_equity=by_date[b['date']]['equity'],
                 net_return=by_date[b['date']]['equity'] / by_date[a['date']]['equity'] - 1)
            for k, (a, b) in enumerate(zip(decisions, decisions[1:]), 1)]


def funding_stress(long_curve, short_curve):
    """Post-process native paths; no execution, financing feedback or resizing."""
    accrued, curve, previous = 0.0, [], None
    for long_row, short_row in zip(long_curve, short_curve, strict=True):
        if long_row['date'] != short_row['date']:
            raise ValueError('funding paths must align')
        days = (pd.Timestamp(short_row['date']) - pd.Timestamp(previous['date'])).days if previous else 0
        fee = (previous['short_notional'] * 0.10 * days / 365) if previous else 0.0
        accrued += fee
        curve.append(dict(date=short_row['date'], daily_funding=fee, cumulative_funding=accrued,
            short_equity=short_row['equity'] - accrued,
            equity=long_row['equity'] + short_row['equity'] - accrued))
        previous = short_row
    return dict(descriptive_only=True, method='post_processing_ACT365_prior_close_notional',
                annual_rate=0.10, total_funding=accrued, equity_curve=curve,
                metrics=path_metrics(curve, 100_000.0))


def append_ledger(path, report):
    def build(k, previous_rows):
        if k != 3:
            report['deviations'].append(
                f'账本口径：预注册计划 K=3；指定的隔离输出账本按已有记录计算 K={k}，不补造历史行。')
        return dict(stage='S1', change_type='E', idea_id='fq-us-momentum-ls-v1',
                    baseline_version='fq_us:no_validated_factors', verdict=report['verdict'],
                    z=report['z'], halves=report['halves'], n=report['n'],
                    legs={s: report['legs'][s]['metrics'] for s in ('long', 'short')},
                    input_hashes=report['input_hashes'], prereg_path=PREREG_PATH,
                    preregistered_K=3, deviations=report['deviations'],
                    report_path=report.get('report_path'))
    return evolution_ledger.append_event(path, 'fq_us', build)


def run_study(snapshot, allow_input_drift=False):
    hashes, mismatches = guard_hashes(snapshot, allow_input_drift)
    universe, perps, frames, calendar, quality = load_snapshot(snapshot)
    features = build_features(frames, universe, calendar)
    sessions, decisions = rebalance_schedule(features, calendar)
    stock_frames = {s: frames[s] for s in universe}
    long = run_sleeve(stock_frames, sessions, {r['date']: r['long'] for r in decisions},
                      cash=50_000.0, commission=0.0013)
    short = run_sleeve(stock_frames, sessions, {r['date']: r['short'] for r in decisions},
                       cash=50_000.0, commission=0.0007, short=True)
    variant = run_sleeve(stock_frames, sessions, {r['date']: r['L'] for r in decisions},
                         cash=100_000.0, commission=0.0013)
    first = sessions[0]
    purchasable = [s for s in universe if first in frames[s].index
                   and valid_rows(frames[s]).loc[first]]
    missing = sorted(set(universe) - set(purchasable))
    ew = run_sleeve(stock_frames, sessions,
                    {str(first.date()): {s: 1 / len(universe) for s in purchasable}},
                    cash=100_000.0, commission=0.0013)
    ew.update(descriptive_only=True, initial_weights={s: 1 / len(universe) for s in purchasable},
              unpurchasable_at_start=missing, reserved_cash_weight=len(missing) / len(universe))
    spy = run_sleeve({'SPY': frames['SPY']}, sessions, {str(first.date()): {'SPY': 1.0}},
                     cash=100_000.0, commission=0.0013)
    spy['descriptive_only'] = True
    curve = [dict(date=a['date'], equity=a['equity'] + b['equity'])
             for a, b in zip(long['equity_curve'], short['equity_curve'], strict=True)]
    periods = period_table(curve, decisions)
    stats = period_statistics([r['net_return'] for r in periods])
    deviations = [
        '歧义口径：交易日及 t−21/t−252 使用 SPY 有效日线日历；有效日线要求 OHLC 正数、有限且价格范围合法，成交量非负。',
        '歧义口径：波动率用截至 t 的 30 个简单日收益、样本标准差（ddof=1）；信号并列按股票代码稳定升序，最高组取末尾。',
        '歧义口径：周期边界采用收盘成交并扣费后的权益；终点调仓照常执行，不强制平仓。首次建仓费计入累计收益，但边界收益比不再计入首次费用。',
        '歧义口径：基准沿用多头每次成交 0.0013 成本；固定 90 只各分配 1/90，首日无有效价格者保留现金，之后不补买。',
        '歧义口径：资金费压力采用 ACT/365，前一收盘空头名义金额乘间隔自然日数；周末计提在下一交易日展示，不反馈至下单。',
        '执行口径：允许分数股以实现目标权重；买单由原生 broker filler 按含费可用现金裁量并取消余量，先卖后买，不借款补费用。',
    ]
    if missing:
        deviations.append('90 只等权基准首日无法买入并保留现金的股票：' + '、'.join(missing))
    unmapped = [s for s in universe if not perps[s]]
    if unmapped:
        deviations.append(
            '冻结输入与预注册陈述不一致：以下 EQUITY 代码在映射中没有 perp_symbol：'
            + '、'.join(unmapped)
            + '。按最字面的 instrumentType=EQUITY 保留 90 只股票池，不添加映射过滤、不臆造合约；相关空头回放不证明实际可交易。')
    if mismatches:
        deviations.append('已显式使用 --allow-input-drift：输入哈希偏离预注册，本次不构成冻结输入复现。')
    if any(quality['invalid_rows'].values()) or any(quality['off_calendar_rows'].values()):
        deviations.append('输入存在无效或 SPY 日历外日线，详见 data_quality；不计入有效历史和信号。')
    return dict(engine='backtrader', engine_version=long['engine_version'], params=PARAMS,
        prereg_path=PREREG_PATH, preregistered_K=3, snapshot=str(Path(snapshot).resolve()),
        input_hashes=hashes, expected_input_hashes=EXPECTED_HASHES, input_drift=mismatches,
        deviations=deviations, universe=universe, perp_mapping=perps, data_quality=quality,
        start=str(sessions[0].date()), end=str(sessions[-1].date()), trading_sessions=len(sessions),
        trailing_sessions_excluded=int((calendar > sessions[-1]).sum()),
        execution=dict(cheat_on_close=True, settlement_callback='same_close_plus_1_second',
                       fractional_shares=True, period_boundary='after_close_fills_and_costs',
                       turnover='sum(abs(fill_notional)/previous_session_equity); both sides, no /2'),
        period_table=periods, **stats, legs=dict(long=long, short=short),
        L=dict(descriptive_only=True, **variant), benchmarks=dict(EW90=ew, SPY=spy),
        equity_curve=curve, metrics=path_metrics(curve, 100_000.0, long['fills'] + short['fills']),
        funding_stress=funding_stress(long['equity_curve'], short['equity_curve']),
        weights=[dict(**r, nominal_total_long={s: w * 0.5 for s, w in r['long'].items()},
                      nominal_total_short={s: w * 0.5 for s, w in r['short'].items()}) for r in decisions])


def render_markdown(report):
    def pct(value):
        return f'{value:.4%}' if value is not None else '无'

    def number(value):
        return f'{value:.6f}' if value is not None else '无'

    halves = report['halves']
    lines = [
        '# fq_us 动量多空 S1 冻结规则报告', '',
        f"判定：**{report['verdict']}**；z = {number(report['z'])}；完整周期 n = {report['n']}。", '',
        '本检验使用已看过的数据，不构成样本外证据；股票名单有事后选择偏差。S1 只筛查可行性，S2 须另行批准。', '',
        f"区间：{report['start']} 至 {report['end']}，共 {report['trading_sessions']} 个交易日；尾部不足 20 日的 {report['trailing_sessions_excluded']} 个交易日不纳入。",
        f"前半段：n={halves['first']['n']}，均值={pct(halves['first']['mean'])}；后半段：n={halves['second']['n']}，均值={pct(halves['second']['mean'])}。",
        f"周期均值={pct(report['mean'])}，样本标准差={pct(report['sd'])}；z=均值/(样本标准差/√n)。",
        '通过条件固定为 z≥1 且两半均值均>0；描述性结果不参与判定。', '',
        '## 固定参数与成交口径', '',
        '股票池为快照的 90 只 EQUITY，ETF 不入选；冻结永续映射的缺失详见偏离。至少 253 根有效日线、收盘≥5 美元、过去 20 日成交额中位数≥1000 万美元；少于 15 只时两腿空仓。',
        '信号 c[t−21]/c[t−252]−1；每 20 个交易日调仓，最高/最低各 floor(20%×可用数)。30 日波动率倒数加权，多空名义敞口各 0.5，单只上限 0.10。',
        '多空独立 Cerebro，各 50,000 美元，以自身权益用 order_target_percent 调仓（袖内敞口 1、上限 0.20）；权益相加。两袖随收益漂移，不强制恢复等额资金。',
        'Backtrader 原生 broker，set_coc(True)。每日收盘后同价 +1 秒回调只用于结算，不引入下一日信息；原生 fill 时间仍为决策日收盘。',
        '手续费与滑点合并：多头每次成交 0.0013，空头 0.0007；基础资金费为零。先卖后买，买入按含费可用现金裁量，余量取消。',
        '分年收益以此前年末权益为基数，首年含初始建仓费用；最大回撤含初始现金基准。换手为成交绝对名义金额/前一交易日权益之和（双边，不除以二），年化乘 252/交易日数。', '',
        '## 描述性结果', '',
        '| 账户 | 累计净收益 | 最大回撤 | 双边换手 | 成交费用（美元） |',
        '|---|---:|---:|---:|---:|',
    ]
    accounts = [('多空组合', report), ('多头腿（相对 50,000）', report['legs']['long']),
                ('空头腿（相对 50,000）', report['legs']['short']), ('L 只做多', report['L']),
                ('90 只固定等权买持', report['benchmarks']['EW90']), ('SPY 买持', report['benchmarks']['SPY'])]
    for name, account in accounts:
        m = account['metrics']
        lines.append(f"| {name} | {pct(m['total_return'])} | {pct(m['max_drawdown'])} | {m['turnover']:.4f} | {m['commission']:.4f} |")
    stress = report['funding_stress']
    lines += ['', f"资金费压力（描述性后处理）：按空头名义金额年费 10% 日计提，累计扣除 {stress['total_funding']:.4f} 美元；组合累计收益 {pct(stress['metrics']['total_return'])}，最大回撤 {pct(stress['metrics']['max_drawdown'])}。",
              '该调整只从原生空头权益路径减去累计资金费，不重新模拟交易、仓位或复利反馈。', '',
              '## 分年收益', '', '| 年份 | 多空组合 | 多头腿 | 空头腿 | L | 等权90 | SPY |',
              '|---|---:|---:|---:|---:|---:|---:|']
    for year in report['metrics']['yearly_returns']:
        lines.append('| ' + year + ' | ' + ' | '.join(pct(a['metrics']['yearly_returns'].get(year))
                                                      for _, a in accounts) + ' |')
    lines += ['', '## 偏离与歧义处理', ''] + [f'- {d}' for d in report['deviations']]
    lines += ['', '## 输入与账本', '', f"预注册：`{PREREG_PATH}`。"]
    for name, digest in report['input_hashes'].items():
        lines.append(f'- {name}: `{digest}`')
    row = report.get('ledger_row')
    lines += ['', f"预注册记 K=3；本次输出账本按实际已有行计 K={row['K']}。" if row else '本次未写账本；预注册记 K=3。',
              '完整周期表、逐日原生权益、成交、各次目标权重、两腿、基准与压力路径见 report.json。', '',
              '## 完整周期', '', '| 周期 | 起点 | 终点 | 净收益 |', '|---:|---|---|---:|']
    lines += [f"| {r['k']} | {r['start']} | {r['end']} | {pct(r['net_return'])} |" for r in report['period_table']]
    return '\n'.join(lines) + '\n'


def output_paths(out, ledger=None):
    out = Path(out).resolve()
    readonly = Path('/Users/zhangyun/workspace/nofx/data').resolve()
    if out.is_relative_to(readonly):
        raise ValueError('--out must not be inside the read-only data directory')
    ledger = Path(ledger).resolve() if ledger else out / 'ledger.jsonl'
    if not ledger.is_relative_to(out):
        raise ValueError('--ledger must be inside --out')
    return out, ledger


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--snapshot', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    group = parser.add_mutually_exclusive_group()
    group.add_argument('--ledger', type=Path)
    group.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--allow-input-drift', action='store_true')
    args = parser.parse_args(argv)
    out, ledger = output_paths(args.out, args.ledger)
    if out.is_relative_to(args.snapshot.resolve()):
        parser.error('--out must not be inside --snapshot')
    report = run_study(args.snapshot, args.allow_input_drift)
    out.mkdir(parents=True, exist_ok=True)
    report['report_path'] = str(out / 'report.json')
    if not args.no_ledger:
        report['ledger_row'] = append_ledger(ledger, report)
    safe = evolution_ledger.json_safe(report)
    (out / 'report.json').write_text(json.dumps(safe, ensure_ascii=False, indent=2, allow_nan=False) + '\n', encoding='utf-8')
    (out / 'report.md').write_text(render_markdown(report), encoding='utf-8')
    print(f"verdict={report['verdict']} z={report['z']} n={report['n']} "
          f"halves={report['halves']['first']['mean']:.8f},{report['halves']['second']['mean']:.8f}")
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
