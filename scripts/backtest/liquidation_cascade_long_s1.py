"""Frozen H2b offline S1. Only the explicit ``fetch`` command uses the network.

Actual trades and the sparse shared-capital portfolio use Backtrader. Arithmetic
code is an independent reconciliation oracle and the preregistered placebo.
"""
from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from datetime import date, datetime, timedelta, timezone
import json
import math
from pathlib import Path
import re
import subprocess
import zipfile

import numpy as np
import pandas as pd

from . import evolution_ledger
from .backtrader_adapter import _backtrader, BacktraderUnavailable
from .incremental_filter import weekly_statistics, z_threshold
from .liquidation_cascade_s0 import (
    select_pools, read_klines, read_metrics, read_daily, hourly_features,
    classify, deduplicate, forward_returns, inspect_data, grouped_files,
    file_sha256, json_safe, json_bytes, write_json, fetch_plan, http_zip,
    ms, utc_text, month_after, dates, STEP, HOUR, DAY, MAIN_K, MAIN_M,
    BASE_URL, CR1Stats, csv_member, load_plan as load_s0_plan,
)
from .market_cache import DEFAULT_PROXY

REPO = Path(__file__).resolve().parents[2]
DATA = Path('/Users/zhangyun/workspace/nofx/data')
PREREG = REPO / 'docs/research/2026-11-12-liquidation-cascade-long-s1-prereg.md'
PREREG_SHA256 = '7774def40d6747bc358c3bde08d0066519b30ba32708a90874d517f8c386118e'
S0_MODULE = Path(__file__).with_name('liquidation_cascade_s0.py')
S0_SHA256 = '85d8240725a3e7179ed6b50c05cedd8d047975125b1cc72b0d2710473285c3b8'
P_START, P_END = '2022-01-01', '2023-01-01'
A_START, A_END = '2023-01-01', '2026-05-01'
MONTHS = list(pd.period_range('2022-01', '2022-12', freq='M').astype(str))
FEE, SLIP, STRESS_SLIP, CAPITAL, TOL = .0005, .0008, .0020, 10000., 1e-9
CASH_BUFFER = 1e6
Z_K = z_threshold(9)
assert abs(Z_K - 2.773) < .001

CHOICES = [
    'P数据使用独立p2022根；S0 plan/manifest只读，不含B段下载或S3检验。预热按S0清单：前月5m与月前8日metrics。',
    '事件完全复用冻结S0函数，先限当月top60再按C/D分别24h去重；不因前向缺失删除或递补C事件。',
    '原始5m以根开始时刻索引、结束时刻驱动Backtrader：t根next下单，t+5m开盘入场，t+4h根next平仓，t+5m+4h开盘出场；另存exit_bar_time，A5/data_gap的exit_time为该根结束时刻，原生盘中Stop用触发根开盘时刻代理。',
    '止损基于含滑点的实际入场价；时间出场单提交时撤销止损，目标出场根只使用开盘，不用该根盘中低点触发已撤止损。',
    'A5入场根触及止损及data_gap最后收盘，作为两种明确的离线结算例外：创建卖单并调用BackBroker._execute完成现金、仓位和手续费记账；不启用cheat。其余成交为原生Market/Stop。',
    'data_gap只查看下一网格是否存在，在中断前最后完整根收盘结算；入场根缺失或信号下单根缺失均unfilled，分别计数。',
    '零成本与不利成交仅用算术；各自按其入场价重新确定20%止损，不利入场取该根最高价（不再叠加入场滑点）。12/24h同规则只改持有期。',
    '稀疏组合每币一个feed，prenext调用next；broker只对当前全局时刻有新根且晚于订单创建时刻的feed撮合，防止旧根重复触发Stop。',
    '运行前、未看任何收益时由审查确定：组合以1倍、不加杠杆为硬约束；broker初始现金=10000+隐藏零息缓冲CASH_BUFFER=1e6，set_checksubmit(False)；所有权益、sizing、日末曲线、收益和MaxDD均扣除缓冲，真实现金=broker现金−缓冲。逐笔成交后记录真实现金最小值、负值次数和最大透支额（含同根结算前的跳空透支）；原生Margin/Rejected仍判实现无效。逐笔非组合引擎保持1e9现金、size=1。',
    '运行前、未看任何收益时由审查确定：组合free=真实现金−Σ未成交买单预留额，预留额=size×下单根收盘价×(1+slip)×(1+fee)；目标名义=下单时扣除缓冲后的权益×10%。free≤权益×0.1%则不下单并计cash_skipped；否则若目标名义×(1+slip)×(1+fee)≤free，size=目标名义/下单根收盘价，否则size=free/[下单根收盘价×(1+slip)×(1+fee)]并计cash_capped。挂单占用10笔上限，满10笔优先计skipped_full，与cash_skipped分开；同刻按币名字母序，每笔重读权益和free。',
    '日末取UTC日最后一个feed时刻的broker权益，无事件日沿用；末日信号跨段持有继续结算，单列段末后的日末权益，组合指标和核对覆盖结算延伸日；边界数据只结算登记段事件，不识别S3事件。',
    '持仓时间占比为至少一笔已成交仓位的UTC时间并集/完整日曲线跨度；同根A5按一根5m保守占用，未成交挂单不算持仓时间；盘中Stop缺少逐笔时刻，用触发根开盘计时。',
    'P覆盖率按入选且可下载的币月有效整点OI加总/这些币月整点数，排除预热、BTC额外下载与unfetchable；不要求每个币月都达80%。',
    'S0对账除子集外还要求新增事件至少缺S0四个前向窗口之一；逐窗口原因可重复计数。P的C-D描述沿用S0四窗口完整样本与信号UTC日聚类。',
    '安慰剂仅对主成本R_net_main非None的P段已成交事件抽样，每次随机均值除以已成交笔数；candidate_counts和missing也仅覆盖这些事件，报告单列unfilled数量。均匀有放回独立抽取同币同月top60整点；要求信号根及入场到4h时间出场根全在，即使提前止损也要求完整窗口；不去重随机时点。',
    '扣BTC用实际主成本交易入场开盘至实际出场的同窗口BTC毛对数收益：时间/跳空/盘中止损用对应根开盘，A5/data_gap用对应根收盘；BTC窗口缺根则只缺该描述项。',
    'data_gap记-100%版本仅汇总简单收益（其余交易expm1(R_net)），不将-100%变成有限对数混入主均值。盈亏平衡往返成本取零成本均值的对数近似，含止损路径变化局限。',
    '组合相关性和beta使用共同有效UTC日简单收益；top60按当月池子等权，仅完整相邻日收盘成员参与，另报每日有效成员数，不前填缺失价格。',
    '入场成交额读取原始K线第8列quote_asset_volume（含表头/微秒兼容）；缺值单列，不以volume或估算成交额替代。',
    '本脚本固定一套参数、主成本及2000次安慰剂；合成调试不算市场试验，真实run内部trials=1；默认不写账本、不更新研究记录或正式配置。',
]


class InvalidStudy(ValueError):
    """A failed implementation/data gate; never a market-performance verdict."""


def check_frozen(prereg=None, s0_module=None):
    if file_sha256(PREREG if prereg is None else prereg) != PREREG_SHA256:
        raise ValueError('H2b prereg sha256 mismatch，拒绝运行')
    if file_sha256(S0_MODULE if s0_module is None else s0_module) != S0_SHA256:
        raise ValueError('S0 module sha256 mismatch，拒绝运行')


def make_plan(daily_dir):
    pools, hashes = select_pools(daily_dir, months=MONTHS)
    return plan_for_pools(pools,hashes)


def plan_for_pools(pools, hashes):
    """S0 A-list construction, parameterized only by the frozen 2022 pools."""
    metrics, monthly, daily_k = (defaultdict(set) for _ in range(3))
    unfetchable = {m: [s for s in ss if not re.fullmatch(r'[A-Z0-9]+USDT', s)]
                   for m, ss in pools.items()}
    for month, symbols in pools.items():
        start = date.fromisoformat(month + '-01')
        end = month_after(start)
        for symbol in (set(symbols) - set(unfetchable[month])) | {'BTCUSDT'}:
            metrics[symbol].update(d.isoformat() for d in dates(start - timedelta(days=8), end))
            monthly[symbol].update((month, month_after(start, -1).strftime('%Y-%m')))
            daily_k[symbol].update(d.isoformat() for d in dates(end, end + timedelta(days=2)))
    files = []
    for symbol in sorted(metrics):
        for day in sorted(metrics[symbol]):
            name = f'{symbol}-metrics-{day}.zip'
            files.append(dict(kind='metrics', symbol=symbol, period=day,
                              path=f'raw/metrics/{symbol}/{name}',
                              url=f'{BASE_URL}/daily/metrics/{symbol}/{name}'))
        for period in sorted(monthly[symbol] | daily_k[symbol]):
            daily = len(period) == 10
            if daily and period[:7] in monthly[symbol]:
                continue
            name = f'{symbol}-5m-{period}.zip'
            files.append(dict(kind='klines5m', symbol=symbol, period=period,
                              path=f'raw/klines5m/{symbol}/{name}',
                              url=f'{BASE_URL}/{"daily" if daily else "monthly"}/klines/{symbol}/5m/{name}'))
    counts = dict(Counter(r['kind'] for r in files))
    counts.update(total=len(files), symbols=len(metrics), pool_months=len(pools),
                  pool_symbol_months=sum(map(len, pools.values())),
                  unfetchable_symbol_months=sum(map(len, unfetchable.values())))
    return dict(version=1, prereg_sha256=PREREG_SHA256, s0_module_sha256=S0_SHA256,
                p_range=[P_START, P_END], pools=pools,
                unfetchable={m: ss for m, ss in unfetchable.items() if ss},
                daily_sha256=hashes, files=files, counts=counts)


def load_p_plan(root):
    plan = json.loads((Path(root) / 'plan.json').read_bytes())
    if (plan['version'] != 1 or plan['prereg_sha256'] != PREREG_SHA256
            or plan['s0_module_sha256'] != S0_SHA256 or plan['p_range'] != [P_START, P_END]
            or sorted(plan['pools']) != MONTHS):
        raise ValueError('P plan冻结输入/时间范围不符')
    for name, digest in plan['daily_sha256'].items():
        if not re.fullmatch(r'\w+USDT\.json', name) or not re.fullmatch(r'[a-f0-9]{64}', digest):
            raise ValueError('P plan日线路径/SHA非法')
    for month, symbols in plan['pools'].items():
        if (len(symbols) > 60 or len(set(symbols)) != len(symbols)
                or any(not re.fullmatch(r'\w+USDT', s) for s in symbols)
                or plan['unfetchable'].get(month, []) !=
                [s for s in symbols if not re.fullmatch(r'[A-Z0-9]+USDT', s)]):
            raise ValueError('P plan币池/unfetchable非法')
    seen = set()
    for item in plan['files']:
        symbol, period, kind = item['symbol'], item['period'], item['kind']
        if not re.fullmatch(r'[A-Z0-9]+USDT', symbol) or kind not in ('metrics', 'klines5m'):
            raise ValueError('非法P plan币名/种类')
        daily = len(period) == 10
        if not re.fullmatch(r'\d{4}-\d{2}' + (r'-\d{2}' if daily else ''), period):
            raise ValueError('非法P plan日期')
        datetime.strptime(period, '%Y-%m-%d' if daily else '%Y-%m')
        if kind == 'metrics':
            if not daily:
                raise ValueError('metrics必须为日文件')
            name = f'{symbol}-metrics-{period}.zip'
            url = f'{BASE_URL}/daily/metrics/{symbol}/{name}'
        else:
            name = f'{symbol}-5m-{period}.zip'
            url = f'{BASE_URL}/{"daily" if daily else "monthly"}/klines/{symbol}/5m/{name}'
        if item['path'] != f'raw/{kind}/{symbol}/{name}' or item['url'] != url or item['path'] in seen:
            raise ValueError('P plan路径/URL非法或重复')
        seen.add(item['path'])
    canonical = plan_for_pools(plan['pools'],plan['daily_sha256'])
    if (sorted(plan['files'],key=lambda r:r['path']) != sorted(canonical['files'],key=lambda r:r['path'])
            or plan['counts'] != canonical['counts']):
        raise ValueError('P plan清单/计数不符合冻结币池（缺文件或含额外时间段）')
    return plan


def event_frame(bars, oi, symbol, pools, start, end):
    frame = hourly_features(bars, oi).reindex(np.arange(ms(start), ms(end), HOUR))
    months = pd.to_datetime(frame.index, unit='ms').strftime('%Y-%m')
    active = np.array([symbol in pools.get(m, []) for m in months])
    frame = frame.loc[active].copy()
    frame['category'] = classify(frame, k=MAIN_K, m=MAIN_M)
    return deduplicate(frame.loc[frame['category'].isin(['C', 'D'])])


def coverage_rows(oi, symbol, pools):
    rows = []
    for month, symbols in pools.items():
        if symbol not in symbols or not re.fullmatch(r'[A-Z0-9]+USDT', symbol):
            continue
        start = ms(month + '-01')
        end = ms(month_after(date.fromisoformat(month + '-01')).isoformat())
        times = np.arange(start, end, HOUR)
        valid = int(oi.reindex(times).notna().sum())
        rows.append(dict(symbol=symbol, month=month, valid=valid, expected=len(times), coverage=valid/len(times)))
    return rows


def metrics_coverage(rows):
    valid, expected = sum(r['valid'] for r in rows), sum(r['expected'] for r in rows)
    ratio = valid / expected if expected else None
    return dict(valid=valid, expected=expected, coverage=ratio,
                passed=ratio is not None and ratio >= .80, symbol_months=rows)


def arithmetic_trade(bars, t, fee=FEE, slip=SLIP, horizon=4, adverse=False):
    """Independent OHLC oracle; input index denotes bar OPEN, inclusive exit bar."""
    t = int(t)
    entry, target = t + STEP, t + STEP + horizon * HOUR
    result = dict(t=t, entry_time=None, exit_time=None, entry_price=None,
                  exit_price=None, exit_reason='unfilled', R_net=None)
    if t not in bars.index or entry not in bars.index:
        result['unfilled_reason'] = 'entry_bar_missing' if entry not in bars.index else 'signal_bar_missing'
        return result
    window = bars.loc[entry:target]
    stamps = window.index.to_numpy(np.int64)
    values = window[['open', 'high', 'low', 'close']].to_numpy(float)
    entry_px = float(values[0, 1] if adverse else values[0, 0] * (1 + slip))
    stop = entry_px * .8
    raw, exit_t, reason, endpoint = None, None, None, 'open'
    for i, stamp in enumerate(stamps):
        o, h, l, c = values[i]
        if stamp == target:
            raw, exit_t, reason = o, int(stamp), 'time'
            break
        if l <= stop:
            raw = stop if i == 0 else min(o, stop)
            exit_t, reason = int(stamp), 'a5_stop' if i == 0 else 'stop'
            endpoint = 'close' if i == 0 else 'open'
            break
        if i + 1 == len(stamps) or stamps[i + 1] != stamp + STEP:
            raw, exit_t, reason, endpoint = c, int(stamp), 'data_gap', 'close'
            break
    exit_px = float(raw * (1 - slip))
    actual_exit = exit_t + (STEP if reason in ('a5_stop','data_gap') else 0)
    result.update(entry_time=entry, exit_time=actual_exit, exit_bar_time=exit_t, entry_price=entry_px,
                  exit_price=exit_px, exit_reason=reason, exit_endpoint=endpoint,
                  R_net=math.log(exit_px * (1-fee) / (entry_px * (1+fee))))
    return result


def complete_window(bars, t, horizon=4):
    needed = np.arange(t, t + STEP + horizon * HOUR + STEP, STEP)
    return bool(np.isin(needed, bars.index.to_numpy()).all())


def sparse_windows(bars, times):
    stamps = bars.index.to_numpy(np.int64)
    mask = np.zeros(len(stamps), dtype=bool)
    for t in times:
        left, right = np.searchsorted(stamps, [t, t + STEP + 4*HOUR], side='left')
        mask[left:right + int(right < len(stamps) and stamps[right] == t+STEP+4*HOUR)] = True
    return bars.loc[mask, ['open', 'high', 'low', 'close']].copy()


def daily_curve(observations, start, end):
    by_day = {int(t)//DAY: float(v) for t, v in sorted(observations)}
    final_day = max([ms(end)//DAY - 1] + list(by_day))
    previous, rows = CAPITAL, []
    for day in range(ms(start)//DAY, final_day + 1):
        previous = by_day.get(day, previous)
        rows.append(dict(time_ms=day*DAY, equity=previous, settlement_extension=day*DAY >= ms(end)))
    return rows


def run_engine(events, bars_by_symbol, *, fee=FEE, slip=SLIP, portfolio=False,
               start=P_START, end=P_END):
    """Native sparse multi-feed engine. A5/gap exceptions still use broker accounting."""
    bt = _backtrader()
    events = sorted(events, key=lambda e: (e['t'], e['symbol']))
    schedule = defaultdict(list)
    for e in events:
        schedule[int(e['t'])].append(e)
    usable = {s: f for s, f in bars_by_symbol.items() if not f.empty}
    if not usable:
        return dict(trades=[], observations=[], daily=daily_curve([], start, end),
                    accepted=[], skipped=0, cash_capped=0, cash_skipped=0,
                    real_cash_min=CAPITAL, real_cash_negative_count=0, real_cash_max_overdraft=0.,
                    max_concurrent=0, errors=[], engine='backtrader')

    class SparseFeed(bt.feed.DataBase):
        def start(self):
            super().start()
            self.rows = iter(self.p.dataname.itertuples(name=None))

        def _load(self):
            row = next(self.rows, None)
            if row is None:
                return False
            stamp, o, h, l, c = row
            self.lines.datetime[0] = bt.date2num(datetime.fromtimestamp((stamp+STEP)/1000, timezone.utc))
            for line, value in zip((self.lines.open, self.lines.high, self.lines.low, self.lines.close), (o,h,l,c)):
                line[0] = value
            self.lines.volume[0] = self.lines.openinterest[0] = 0
            return True

    class SparseBroker(bt.brokers.BackBroker):
        def __init__(self):
            super().__init__()
            self.cash_stats = dict(real_cash_min=CAPITAL, real_cash_negative_count=0,
                                   real_cash_max_overdraft=0.)

        def _execute(self, order, ago=None, price=None, cash=None, position=None, dtcoc=None):
            previous_size = order.executed.size
            result = super()._execute(order, ago, price, cash, position, dtcoc)
            if portfolio and ago is not None and order.executed.size != previous_size:
                # Count actual settlements, including A5/gap; never pseudo-executions.
                real_cash = self.getcash() - CASH_BUFFER
                self.cash_stats['real_cash_min'] = min(self.cash_stats['real_cash_min'], real_cash)
                self.cash_stats['real_cash_negative_count'] += int(real_cash < 0)
                self.cash_stats['real_cash_max_overdraft'] = max(
                    self.cash_stats['real_cash_max_overdraft'], -real_cash)
            return result

        def _try_exec(self, order):
            now = bt.date2num(cerebro._dtmaster)
            if (not len(order.data) or abs(order.data.datetime[0] - now) > 1e-8
                    or order.data.datetime[0] <= order.created.dt):
                return
            super()._try_exec(order)

        def settle(self, order, price):
            # Explicit A5/gap exception, no synthetic bars and no direct cash mutation.
            self.pending.remove(order)
            self._execute(order, ago=0, price=price)
            self._get_value()

    class Replay(bt.Strategy):
        def __init__(self):
            self.feeds = {d._name: d for d in self.datas}
            self.states, self.done, self.accepted, self.errors = {}, [], [], []
            self.observations = []
            self.skipped = self.maximum = 0
            self.cash_capped = self.cash_skipped = 0

        def prenext(self):
            self.next()

        def notify_order(self, order):
            if order.status in (bt.Order.Margin, bt.Order.Rejected, bt.Order.Expired):
                self.errors.append(f'{order.data._name}: {order.getstatusname()}')
                return
            if order.status != bt.Order.Completed:
                return
            key = order.info['key']
            state = self.states.get(key)
            if state is None:
                return  # exceptional settlement already recorded synchronously
            stamp = int(round(bt.num2date(order.executed.dt).replace(tzinfo=timezone.utc).timestamp()*1000))-STEP
            if order.isbuy():
                state.pop('reserved_cash', None)
                state.update(entry_time=stamp, entry_price=order.executed.price,
                             entry_fee=order.executed.comm, size=order.executed.size)
                stop = self.sell(data=order.data, size=order.executed.size,
                                 exectype=bt.Order.Stop, price=order.executed.price*.8)
                stop.addinfo(key=key, reason='stop')
                state['stop'] = stop
            else:
                self.finish(key, stamp, order.executed.price, order.executed.comm, order.info['reason'])

        def finish(self, key, stamp, price, commission, reason):
            state = self.states.pop(key)
            if 'stop' in state:
                self.cancel(state['stop'])
            cost = state['entry_price']*state['size'] + state['entry_fee']
            proceeds = price*state['size'] - commission
            actual_exit = stamp + (STEP if reason in ('a5_stop','data_gap') else 0)
            self.done.append(dict(symbol=key[0], t=key[1], entry_time=state['entry_time'],
                                  exit_time=actual_exit, exit_bar_time=stamp,
                                  entry_price=state['entry_price'], exit_price=price,
                                  entry_fee=state['entry_fee'], exit_fee=commission, size=state['size'],
                                  exit_reason=reason, R_net=math.log(proceeds/cost)))

        def next(self):
            now = int(round(cerebro._dtmaster.replace(tzinfo=timezone.utc).timestamp()*1000))-STEP
            current = {d._name: d for d in self.datas if len(d) and
                       abs(d.datetime[0]-bt.date2num(cerebro._dtmaster)) < 1e-8}
            for key, state in list(self.states.items()):
                if key[0] not in current or 'entry_price' not in state or state.get('closing'):
                    continue
                d = current[key[0]]
                reason = None
                if now == state['entry_time'] and d.low[0] <= state['entry_price']*.8:
                    raw, reason = state['entry_price']*.8, 'a5_stop'
                elif now + STEP not in usable[key[0]].index:
                    raw, reason = d.close[0], 'data_gap'
                if reason:
                    self.cancel(state['stop'])
                    order = self.sell(data=d, size=state['size'])
                    order.addinfo(key=key, reason=reason)
                    self.broker.settle(order, raw*(1-slip))
                    self.finish(key, now, order.executed.price, order.executed.comm, reason)
                elif now == key[1] + 4*HOUR:
                    self.cancel(state['stop'])
                    order = self.sell(data=d, size=state['size'])
                    order.addinfo(key=key, reason='time')
                    state['closing'] = True
            for event in schedule.get(now, []):
                s, t = event['symbol'], int(event['t'])
                if s not in current or t+STEP not in usable[s].index:
                    continue
                if portfolio and len(self.states) >= 10:
                    self.skipped += 1
                    continue
                key, d = (s,t), current[s]
                if any(k[0] == s for k in self.states):
                    raise InvalidStudy('同币持仓重叠')
                equity = self.broker.getvalue() - (CASH_BUFFER if portfolio else 0.)
                if equity <= 0:
                    raise InvalidStudy('组合权益非正')
                sizing = {}
                if portfolio:
                    free = self.broker.getcash() - CASH_BUFFER - math.fsum(
                        state.get('reserved_cash', 0.) for state in self.states.values())
                    if free <= equity*.001:
                        self.cash_skipped += 1
                        continue
                    target, cost_factor = equity*.1, (1+slip)*(1+fee)
                    capped = target*cost_factor > free
                    size = free/(d.close[0]*cost_factor) if capped else target/d.close[0]
                    self.cash_capped += int(capped)
                    sizing = dict(order_free_cash=free, reserved_cash=size*d.close[0]*cost_factor,
                                  cash_capped=capped)
                else:
                    size = 1.
                self.states[key] = dict(size=size, reserved_cash=sizing.get('reserved_cash', 0.))
                order = self.buy(data=d, size=size)
                order.addinfo(key=key)
                self.accepted.append(dict(symbol=s,t=t,size=size,order_equity=equity,
                                          order_close=d.close[0],**sizing))
                self.maximum = max(self.maximum, len(self.states))
            self.observations.append((now+STEP, self.broker.getvalue() - (CASH_BUFFER if portfolio else 0.)))

    cerebro = bt.Cerebro(stdstats=False)
    cerebro.setbroker(SparseBroker())
    for symbol, frame in sorted(usable.items()):
        cerebro.adddata(SparseFeed(dataname=frame, timeframe=bt.TimeFrame.Minutes, compression=5), name=symbol)
    cerebro.broker.setcash(CAPITAL + CASH_BUFFER if portfolio else 1e9)
    cerebro.broker.set_checksubmit(False)
    cerebro.broker.setcommission(commission=fee)
    cerebro.broker.set_slippage_perc(slip, slip_open=True, slip_match=True, slip_out=True)
    cerebro.addstrategy(Replay)
    replay = cerebro.run(runonce=False, preload=False)[0]
    if replay.states:
        replay.errors.append(f'结束仍有{len(replay.states)}笔未结算')
    return dict(trades=sorted(replay.done,key=lambda r:(r['t'],r['symbol'])),
                observations=replay.observations, daily=daily_curve(replay.observations,start,end),
                accepted=replay.accepted, skipped=replay.skipped, max_concurrent=replay.maximum,
                cash_capped=replay.cash_capped, cash_skipped=replay.cash_skipped,
                **(cerebro.broker.cash_stats if portfolio else {}),
                errors=replay.errors, engine='backtrader', engine_version=bt.__version__)


def backtrader_trade(bars, t, fee=FEE, slip=SLIP):
    reference = arithmetic_trade(bars,t,fee,slip)
    if reference['R_net'] is None:
        return reference
    frame = sparse_windows(bars,[t])
    actual = run_engine([dict(symbol='SYNTHUSDT',t=t)], {'SYNTHUSDT':frame},fee=fee,slip=slip)
    if actual['errors'] or len(actual['trades']) != 1:
        raise InvalidStudy(f'逐笔Backtrader失败: {actual["errors"]}')
    trade = actual['trades'][0]
    for key in ('entry_time','exit_time','exit_reason'):
        if trade[key] != reference[key]:
            raise InvalidStudy(f'逐笔{key}不一致: {trade[key]} != {reference[key]}')
    error = abs(trade['R_net']-reference['R_net'])
    if not math.isfinite(error) or error > TOL:
        raise InvalidStudy(f'逐笔净对数收益不一致: {error}')
    return dict(trade, reference_error=error)


def arithmetic_portfolio(events, bars_by_symbol, start=P_START, end=P_END):
    """Independent shared-cash oracle, never used in place of actual portfolio."""
    schedule, paths = defaultdict(list), {}
    for e in sorted(events,key=lambda r:(r['t'],r['symbol'])):
        key = e['symbol'], int(e['t'])
        paths[key] = arithmetic_trade(bars_by_symbol[key[0]],key[1])
        if paths[key]['R_net'] is not None:
            schedule[key[1]].append(key)
    timeline = sorted({int(t) for f in bars_by_symbol.values() for t in f.index})
    current_prices, states, observations, accepted = {}, {}, [], []
    cash, skipped, maximum = CAPITAL + CASH_BUFFER, 0, 0
    cash_capped = cash_skipped = 0
    cash_stats = dict(real_cash_min=CAPITAL, real_cash_negative_count=0, real_cash_max_overdraft=0.)
    def equity():
        return cash + math.fsum(state['size']*current_prices[key[0]]
                                for key,state in states.items() if state['filled']) - CASH_BUFFER
    def fill(key, entering):
        nonlocal cash
        state, p = states[key], paths[key]
        if entering:
            notional = state['size']*p['entry_price']
            cash -= notional
            cash -= notional*FEE
            if cash < -1e-7:
                raise InvalidStudy('算术组合异常跳空耗尽隐藏缓冲现金')
            state['filled'] = True
            state.pop('reserved_cash')
        else:
            cash += state['size']*p['entry_price'] + state['size']*(p['exit_price']-p['entry_price'])
            cash -= state['size']*p['exit_price']*FEE
            del states[key]
        real_cash = cash - CASH_BUFFER
        cash_stats['real_cash_min'] = min(cash_stats['real_cash_min'], real_cash)
        cash_stats['real_cash_negative_count'] += int(real_cash < 0)
        cash_stats['real_cash_max_overdraft'] = max(cash_stats['real_cash_max_overdraft'], -real_cash)
    for now in timeline:
        for symbol, frame in bars_by_symbol.items():
            if now in frame.index:
                current_prices[symbol] = float(frame.at[now,'close'])
        # Native broker queue is FIFO by order submission, followed by the two
        # next() exceptions in state insertion order. Preserve intermediate cash.
        # Submission phases: notifications (0), position management (1), signals (2).
        native = []
        for key,state in list(states.items()):
            p = paths[key]
            if now == p['entry_time']:
                native.append((key[1], 2, key[1], key[0], True))
            if now == p['exit_bar_time'] and p['exit_reason'] in ('stop','time'):
                submitted = p['entry_time'] if p['exit_reason']=='stop' else key[1]+4*HOUR
                phase = 0 if p['exit_reason']=='stop' else 1
                native.append((submitted, phase, key[1], key[0], False))
        for _,_,t,symbol,entering in sorted(native):
            fill((symbol,t), entering)
        for key in list(states):
            p = paths[key]
            if now == p['exit_bar_time'] and p['exit_reason'] in ('a5_stop','data_gap'):
                fill(key, False)
        for key in schedule.get(now,[]):
            if len(states) >= 10:
                skipped += 1
                continue
            value = equity()
            if value <= 0:
                raise InvalidStudy('组合权益非正')
            free = cash - CASH_BUFFER - math.fsum(state.get('reserved_cash', 0.) for state in states.values())
            if free <= value*.001:
                cash_skipped += 1
                continue
            close = current_prices[key[0]]
            target, cost_factor = value*.1, (1+SLIP)*(1+FEE)
            capped = target*cost_factor > free
            size = free/(close*cost_factor) if capped else target/close
            cash_capped += int(capped)
            reserved = size*close*cost_factor
            states[key] = dict(size=size,filled=False,reserved_cash=reserved)
            accepted.append(dict(symbol=key[0],t=key[1],size=size,order_equity=value,order_close=close,
                                 order_free_cash=free,reserved_cash=reserved,cash_capped=capped))
            maximum = max(maximum,len(states))
        observations.append((now+STEP,equity()))
    if states:
        raise InvalidStudy('算术组合结束仍有持仓')
    return dict(daily=daily_curve(observations,start,end),accepted=accepted,
                skipped=skipped,max_concurrent=maximum,cash_capped=cash_capped,cash_skipped=cash_skipped,
                **cash_stats)


def reconcile_portfolio(actual, reference):
    if actual['errors']:
        raise InvalidStudy(f'组合Backtrader订单错误: {actual["errors"]}')
    a, b = actual['daily'], reference['daily']
    if len(a) != len(b) or any(x['time_ms'] != y['time_ms'] for x,y in zip(a,b)):
        raise InvalidStudy('组合日末曲线日期不一致')
    if any(not math.isfinite(x['equity']) or not math.isfinite(y['equity']) or y['equity']<=0 for x,y in zip(a,b)):
        raise InvalidStudy('组合权益非有限数或参考权益非正')
    worst = max((abs(x['equity']-y['equity'])/abs(y['equity']) for x,y in zip(a,b)), default=0.)
    if worst > TOL:
        raise InvalidStudy(f'组合逐日相对误差{worst} > {TOL}')
    counts = ('skipped','max_concurrent','cash_capped','cash_skipped','real_cash_negative_count')
    if (any(actual[k] != reference[k] for k in counts)
            or len(actual['accepted']) != len(reference['accepted'])):
        raise InvalidStudy('组合开仓/跳过/并发计数不一致')
    cash_errors = {}
    for key in ('real_cash_min','real_cash_max_overdraft'):
        x,y = actual[key],reference[key]
        error = abs(x-y)/(abs(y) if y else 1.)
        if not math.isfinite(x) or not math.isfinite(y) or error > TOL:
            raise InvalidStudy(f'组合{key}相对误差{error} > {TOL}')
        cash_errors[key] = error
    for x,y in zip(actual['accepted'],reference['accepted']):
        if (x['symbol'],x['t']) != (y['symbol'],y['t']) or abs(x['size']-y['size']) > TOL*max(1.,abs(y['size'])):
            raise InvalidStudy('组合字母序或size不一致')
    return dict(valid=True,max_daily_relative_error=worst,cash_relative_errors=cash_errors,tolerance=TOL)


def cr1_mean(values, entry_times):
    values = np.asarray(values,dtype=float)
    n = len(values)
    if n != len(entry_times) or not np.isfinite(values).all():
        raise InvalidStudy('CR1输入长度/有限性非法')
    avg = float(values.mean()) if n else None
    groups = defaultdict(float)
    for r,t in zip(values,entry_times):
        groups[int(t)//DAY] += float(r)-avg
    g, se, t_stat = len(groups), None, None
    if g >= 2:
        se = math.sqrt(g/(g-1)*math.fsum(x*x for x in groups.values())/(n*n))
        t_stat = avg/se if se > 0 else None
    return dict(N=n,mean=avg,SE=se,t=t_stat,clusters=g)


def summary(rows, column='R_net_main'):
    valid = [r for r in rows if r.get(column) is not None]
    return cr1_mean([r[column] for r in valid],[r['entry_time'] for r in valid])


def placebo_candidates(bars, symbol, pools):
    """Vectorized arithmetic oracle for complete hourly windows, by symbol/month."""
    out = {}
    stamps = bars.index.to_numpy(np.int64)
    values = bars[['open','high','low','close']].to_numpy(float)
    for month,symbols in sorted(pools.items()):
        if symbol not in symbols:
            continue
        start, end = ms(month+'-01'), ms(month_after(date.fromisoformat(month+'-01')).isoformat())
        times = np.arange(start,end,HOUR)
        left, right = np.searchsorted(stamps,times), np.searchsorted(stamps,times+STEP+4*HOUR)
        eligible = np.flatnonzero((right-left == 49) & (right < len(stamps)))
        if len(eligible):
            idx = left[eligible,None]+np.arange(50)
            contiguous = (stamps[idx] == times[eligible,None]+np.arange(50)*STEP).all(axis=1)
            eligible, idx = eligible[contiguous], idx[contiguous]
            entry = values[idx[:,1],0]*(1+SLIP)
            stop = entry*.8
            touch = values[idx[:,1:49],2] <= stop[:,None]
            hit, first = touch.any(axis=1), touch.argmax(axis=1)
            raw = values[idx[:,49],0].copy()
            at = np.flatnonzero(hit)
            raw[at] = np.where(first[at] == 0,stop[at],
                               np.minimum(values[idx[at,first[at]+1],0],stop[at]))
            returns = np.log(raw*(1-SLIP)*(1-FEE)/(entry*(1+FEE)))
        else:
            returns = np.array([],dtype=float)
        out[(symbol,month)] = dict(times=times[eligible].tolist(),returns=returns.tolist())
    return out


def placebo(rows, candidates, actual_mean, repeats=2000, seed=20261112):
    # Match summary(rows): every draw and its denominator refer to filled trades.
    filled = [r for r in rows if r.get('R_net_main') is not None]
    metadata = dict(seed=seed,repeats=repeats,filled_count=len(filled),unfilled_count=len(rows)-len(filled))
    rng = np.random.default_rng(seed)
    means = np.zeros(repeats)
    missing, sizes = [], {}
    for e in sorted(filled,key=lambda r:(r['t'],r['symbol'])):
        key = e['symbol'],utc_text(e['t'])[:7]
        values = np.asarray(candidates.get(key,{}).get('returns',[]),dtype=float)
        sizes['/'.join(key)] = len(values)
        if not len(values):
            missing.append(dict(symbol=key[0],month=key[1],t=e['t']))
            continue
        means += values[rng.integers(0,len(values),size=repeats)]
    if missing:
        return dict(valid=False,missing=missing,percentile=None,candidate_counts=sizes,**metadata)
    if not filled or actual_mean is None:
        return dict(valid=True,missing=[],percentile=None,candidate_counts=sizes,means=[],**metadata)
    means /= len(filled)
    pct = float(np.mean(means<actual_mean)+.5*np.mean(means==actual_mean))
    return dict(valid=True,missing=[],percentile=pct,candidate_counts=sizes,
                means=means.tolist(),**metadata)


def decide(stats, valid=True):
    positive = lambda x: x is not None and math.isfinite(x) and x > 0
    p = stats.get('P',{})
    t = p.get('t')
    checks = dict(
        p_positive_significant=positive(p.get('mean')) and t is not None and math.isfinite(t) and t >= 2.,
        both_halves_positive=positive(stats.get('H1')) and positive(stats.get('H2')),
        stress_positive=positive(stats.get('stress')),
        placebo_95=stats.get('placebo_pct') is not None and stats['placebo_pct'] >= .95,
        a_positive=positive(stats.get('A')),
        portfolio_positive=positive(stats.get('portfolio_return')),
    )
    verdict = ('invalid' if not valid else 'insufficient' if p.get('N',0)<100 else
               'strong' if all(checks.values()) and t >= Z_K else
               'screen_pass' if all(checks.values()) else 'fail')
    return dict(verdict=verdict,checks=checks,sample_size_pass=p.get('N',0)>=100,
                strong_t_pass=t is not None and math.isfinite(t) and t >= Z_K)


def reconcile_s0(expected, actual, missing_reasons):
    expected, actual = set(expected), set(actual)
    lost, added = sorted(expected-actual), sorted(actual-expected)
    unexplained = [key for key in added if not missing_reasons.get(key)]
    reasons = Counter(h for key in added for h in missing_reasons.get(key,[]))
    return dict(passed=not lost and not unexplained,s0_events=len(expected),s1_events=len(actual),
                lost=[dict(symbol=s,t=t) for s,t in lost],added=len(added),
                added_events=[dict(symbol=s,t=t,missing_horizons=missing_reasons.get((s,t),[])) for s,t in added],
                missing_reason_counts=dict(reasons),unexplained=[dict(symbol=s,t=t) for s,t in unexplained])


def portfolio_metrics(engine, start, end):
    curve = engine['daily']
    previous, peak, dd, samples, daily = CAPITAL,CAPITAL,0.,[],[]
    for row in curve:
        value = row['equity']
        if previous <= 0 or not math.isfinite(value):
            raise InvalidStudy('组合权益非正/非法')
        r = value/previous-1
        previous, peak = value,max(peak,value)
        dd = max(dd,1-value/peak)
        samples.append((datetime.fromtimestamp(row['time_ms']/1000,timezone.utc),r))
        daily.append(dict(row,return_=r))
    returns = np.array([r for _,r in samples])
    mean = float(returns.mean())
    sd = float(returns.std(ddof=1)) if len(returns)>1 else None
    weekly = weekly_statistics(samples)
    se = weekly['se']
    weekly['z'] = weekly['avg_r']/se if se is not None and se>0 else None
    intervals = sorted((r['entry_time'],r['exit_time'])
                       for r in engine['trades'])
    occupied, left, right = 0,None,None
    for a,b in intervals:
        if left is None:
            left,right = a,b
        elif a>right:
            occupied += right-left
            left,right = a,b
        else:
            right = max(right,b)
    if left is not None:
        occupied += right-left
    return dict(total_return=previous/CAPITAL-1,annual_return=mean*365,
                annual_volatility=sd*math.sqrt(365) if sd is not None else None,
                sharpe=mean/sd*math.sqrt(365) if sd else None,max_dd=dd,weekly=weekly,
                holding_fraction=occupied/(len(curve)*DAY),skipped_full=engine['skipped'],
                cash_capped=engine['cash_capped'],cash_skipped=engine['cash_skipped'],
                cash_buffer=CASH_BUFFER,real_cash_min=engine['real_cash_min'],
                real_cash_negative_count=engine['real_cash_negative_count'],
                real_cash_max_overdraft=engine['real_cash_max_overdraft'],
                max_concurrent=engine['max_concurrent'],accepted=engine['accepted'],daily=daily,
                trades=engine['trades'],reconciliation=engine['reconciliation'])


def read_quote_volume(paths):
    parts = []
    for path in paths:
        frame = csv_member(path,header=None,usecols=[0,7],dtype=str)
        if len(frame) and frame.iloc[0,0].lstrip('\ufeff').strip().lower() == 'open_time':
            frame = frame.iloc[1:]
        stamp = pd.to_numeric(frame.iloc[:,0],errors='raise').to_numpy(float)
        stamp = np.where(stamp>=10**14,stamp/1000,stamp).astype(np.int64)
        volume = pd.to_numeric(frame.iloc[:,1],errors='coerce').to_numpy(float)
        volume[(~np.isfinite(volume)) | (volume<0)] = np.nan
        parts.append(pd.Series(volume,index=stamp))
    return pd.concat(parts).sort_index() if parts else pd.Series(dtype=float)


def btc_adjustment(bars, trade):
    a,b = trade['entry_time'],trade.get('exit_bar_time',trade['exit_time'])
    if a is None or b is None or not np.isin(np.arange(a,b+STEP,STEP),bars.index).all():
        return None
    endpoint = 'close' if trade['exit_reason'] in ('a5_stop','data_gap') else 'open'
    return trade['R_net_main']-math.log(float(bars.at[b,endpoint])/float(bars.at[a,'open']))


def correlations(daily, daily_dir, pools):
    prices = {}
    for symbol in sorted({'BTCUSDT'} | {s for ss in pools.values() for s in ss}):
        path = Path(daily_dir)/f'{symbol}.json'
        if path.exists():
            prices[symbol] = read_daily(path)['close']
    benchmarks = {'BTC':[], 'top60_equal_weight':[]}
    member_counts = []
    for row in daily:
        day = pd.Timestamp(row['time_ms'],unit='ms')
        prev = pd.Timestamp(day.to_pydatetime()-timedelta(days=1))
        def change(symbol):
            series = prices.get(symbol)
            if series is None or day not in series.index or prev not in series.index:
                return None
            return float(series.loc[day]/series.loc[prev]-1)
        btc = change('BTCUSDT')
        if btc is not None:
            benchmarks['BTC'].append((row['return_'],btc))
        values = [v for s in pools.get(day.strftime('%Y-%m'),[]) if (v:=change(s)) is not None]
        member_counts.append(dict(day=day.strftime('%Y-%m-%d'),members=len(values)))
        if values:
            benchmarks['top60_equal_weight'].append((row['return_'],float(np.mean(values))))
    result = {}
    for name,pairs in benchmarks.items():
        corr = beta = None
        if len(pairs)>1:
            a,b = np.asarray(pairs).T
            if b.std(ddof=1)>0:
                beta = float(np.cov(a,b,ddof=1)[0,1]/b.var(ddof=1))
                if a.std(ddof=1)>0:
                    corr = float(np.corrcoef(a,b)[0,1])
        result[name] = dict(N=len(pairs),correlation=corr,beta=beta)
    result['daily_top60_member_counts'] = member_counts
    return result


def segment_files(plan, root, segment):
    # S0 plan contains B archives too. Do not load them into S1 feature/return code.
    low, high = ('2021-12','2023-01-02') if segment == 'P' else ('2022-12','2026-05-02')
    filtered = dict(plan,files=[r for r in plan['files'] if low <= r['period'] <= high])
    return grouped_files(filtered,root)


def describe(rows):
    filled = [r for r in rows if r['R_net_main'] is not None]
    groups = defaultdict(list)
    for row in filled:
        kind = row['symbol'] if row['symbol'] in ('BTCUSDT','ETHUSDT') else 'others'
        dt = datetime.fromtimestamp(row['entry_time']/1000,timezone.utc)
        for name in (f"{row['segment']}/asset/{kind}",f"{row['segment']}/year/{dt.year}",
                     f"{row['segment']}/quarter/{dt.year}Q{(dt.month-1)//3+1}"):
            groups[name].append(row)
    result = dict(groups={name:summary(data) for name,data in sorted(groups.items())})
    for segment in ('P','A'):
        data = [r for r in filled if r['segment']==segment]
        vol = [r['entry_quote_volume'] for r in data if r.get('entry_quote_volume') is not None]
        simple = [-1. if r['exit_reason']=='data_gap' else math.expm1(r['R_net_main']) for r in data]
        zero = summary(data,'R_net_zero')
        result[segment] = dict(
            counts=dict(events=sum(r['segment']==segment for r in rows),filled=len(data),
                        unfilled=sum(r['segment']==segment and r['exit_reason']=='unfilled' for r in rows),
                        unfilled_reasons=dict(Counter(r.get('unfilled_reason') for r in rows
                            if r['segment']==segment and r['exit_reason']=='unfilled')),
                        exit_reasons=dict(Counter(r['exit_reason'] for r in data))),
            horizons={str(h):summary(data,f'R_{h}h') for h in (12,24)},
            ex_btc=summary(data,'ex_btc'),zero_cost=zero,adverse=summary(data,'R_net_adverse'),
            break_even_round_trip_log_approx=zero['mean'],
            data_gap_minus100_simple=dict(N=len(simple),mean=float(np.mean(simple)) if simple else None,
                                        data_gap=sum(r['exit_reason']=='data_gap' for r in data)),
            capacity=dict(N=len(vol),missing=len(data)-len(vol),
                          median=float(np.median(vol)) if vol else None,
                          p10=float(np.quantile(vol,.1)) if vol else None,
                          below_1m_fraction=float(np.mean(np.array(vol)<1e6)) if vol else None),
            worst_10=sorted(data,key=lambda r:r['R_net_main'])[:10])
    return result


def analyze_segment(root, plan, segment, stream):
    start,end = (P_START,P_END) if segment=='P' else (A_START,A_END)
    pools = {m:ss for m,ss in plan['pools'].items() if start[:7] <= m < end[:7]}
    files = segment_files(plan,root,segment)
    btc = read_klines(files['BTCUSDT']['klines5m'])
    rows, events, sparse, candidates, c_set, missing_reasons = [],[],{}, {},set(),{}
    delta = CR1Stats()
    worst_error = 0.
    symbols = sorted({s for ss in pools.values() for s in ss if re.fullmatch(r'[A-Z0-9]+USDT',s)})
    for symbol in symbols:
        paths = files[symbol]
        bars = read_klines(paths['klines5m'])
        oi,_ = read_metrics(paths['metrics'],symbol,paths['expected_days'])
        frame = event_frame(bars,oi,symbol,pools,start,end)
        forwards = forward_returns(bars,frame.index)
        raw = frame.join(forwards)
        delta.add(raw.loc[forwards.notna().all(axis=1)])
        c_times = frame.index[frame['category']=='C']
        quote = read_quote_volume(paths['klines5m']) if len(c_times) else pd.Series(dtype=float)
        for t in c_times:
            t = int(t)
            c_set.add((symbol,t))
            missing_reasons[(symbol,t)] = [str(h) for h in (1,4,12,24) if pd.isna(forwards.at[t,f'R_{h}h'])]
            event = dict(symbol=symbol,t=t)
            events.append(event)
            main = backtrader_trade(bars,t)
            stress = backtrader_trade(bars,t,slip=STRESS_SLIP)
            zero = arithmetic_trade(bars,t,fee=0.,slip=0.)
            adverse = arithmetic_trade(bars,t,adverse=True)
            main_return = main.pop('R_net')
            row = dict(main,segment=segment,symbol=symbol,R_net_main=main_return,
                       R_net_stress=stress['R_net'],R_net_zero=zero['R_net'],R_net_adverse=adverse['R_net'],
                       R_12h=arithmetic_trade(bars,t,horizon=12)['R_net'],
                       R_24h=arithmetic_trade(bars,t,horizon=24)['R_net'],ex_btc=None,
                       entry_quote_volume=None,stress_execution=stress)
            if row['entry_time'] is not None:
                q = quote.get(row['entry_time'],np.nan)
                row['entry_quote_volume'] = float(q) if pd.notna(q) else None
                if symbol != 'BTCUSDT':
                    row['ex_btc'] = btc_adjustment(btc,row)
            worst_error = max(worst_error,main.get('reference_error',0.),stress.get('reference_error',0.))
            rows.append(row)
            stream.write(json_bytes(row))
        if len(c_times):
            sparse[symbol] = sparse_windows(bars,c_times)
        if segment=='P':
            candidates.update(placebo_candidates(bars,symbol,pools))
    actual = run_engine(events,sparse,portfolio=True,start=start,end=end)
    if actual['errors']:
        raise InvalidStudy(f'组合Backtrader订单错误: {actual["errors"]}')
    reference = arithmetic_portfolio(events,sparse,start,end)
    actual['reconciliation'] = reconcile_portfolio(actual,reference)
    metrics = portfolio_metrics(actual,start,end)
    return dict(rows=rows,events=events,c_set=c_set,missing_reasons=missing_reasons,
                delta=delta.summary(),portfolio=metrics,candidates=candidates,
                max_trade_reference_error=worst_error)


def guard_output(out, s0_root, p_root, daily_dir, ledger=None):
    out,s0_root,p_root,daily_dir = map(lambda p:Path(p).resolve(),(out,s0_root,p_root,daily_dir))
    if out.exists():
        raise ValueError(f'输出已存在，拒绝覆盖: {out}')
    protected = [s0_root/'raw',p_root/'raw',daily_dir,PREREG.resolve(),S0_MODULE.resolve(),
                 s0_root/'plan.json',s0_root/'manifest.json',p_root/'plan.json',p_root/'manifest.json',
                 s0_root/'s0_v1_corrected']
    if out in (s0_root,p_root) or any(out.is_relative_to(p) or p.is_relative_to(out) for p in protected):
        raise ValueError('输出不可覆盖输入或其父目录')
    if ledger is not None:
        path = Path(ledger).resolve()
        if path.is_relative_to(out) or any(path==p or path.is_relative_to(p) for p in protected):
            raise ValueError('账本路径不可覆盖输入或研究输出')


def markdown_report(result):
    lines = ['# H2b 强平级联后做多 S1', '',f"判定：**{result['verdict']}**；有效性：{result['valid']}。",
             '', 'P 段是决定性检验；A 段已被 S0 看过，只作一致性检查。主持有期4h，K=9。', '']
    def table(title, headers, rows):
        lines.extend(['## '+title,'','| '+' | '.join(headers)+' |','|'+'|'.join(['---']*len(headers))+'|'])
        for row in rows:
            lines.append('| '+' | '.join(str(json_safe(v)).replace('|','\\|').replace('\n',' ') for v in row)+' |')
        lines.append('')
    table('判定检查',['项目','通过'],list(result.get('checks',{}).items())+
          [(name,result.get(name,False)) for name in ('sample_size_pass','strong_t_pass')])
    table('实现与数据门槛',['项目','通过'],result.get('validation_checks',{}).items())
    table('输入证据',['文件','SHA256'],result['input_hashes'].items())
    table('数据检查',['段','passed','结构失败','完整日比对','偏差比例'],
          [[s,c.get('passed'),c.get('structural_failure'),c.get('compared'),c.get('mismatch_fraction')]
           for s,c in result.get('data_check',{}).items()])
    table('逐币数据检查明细',['段','币','metrics覆盖与异常日','K线','日收盘核对'],
          [[segment,symbol,row.get('metrics'),row.get('klines'),row.get('daily')]
           for segment,check in result.get('data_check',{}).items() for symbol,row in check.get('symbols',{}).items()])
    table('数据缺失与错误',['段','类型','内容'],
          [[segment,kind,item] for segment,check in result.get('data_check',{}).items()
           for kind in ('missing','errors') for item in check.get(kind,[])])
    cov = result.get('metrics_coverage',{})
    lines += [f"P 整点 OI 加权覆盖：{cov.get('valid')}/{cov.get('expected')} = {cov.get('coverage')}。",'']
    table('P逐币月覆盖',['币','月','有效整点','预期整点','比例'],
          [[r[k] for k in ('symbol','month','valid','expected','coverage')] for r in cov.get('symbol_months',[])])
    if 's0_reconciliation' in result:
        rec = result['s0_reconciliation']
        table('S0事件对账',['项目','值'],[(k,v) for k,v in rec.items() if k not in ('added_events','lost','unexplained')])
        table('S0差集',['币','时刻','缺失窗口h'],
              [[r['symbol'],utc_text(r['t']),r['missing_horizons']] for r in rec['added_events']])
        table('S0漏事件与无法解释差集',['类型','币','时刻'],
              [[k,r['symbol'],utc_text(r['t'])] for k in ('lost','unexplained') for r in rec[k]])
    if 'stats' in result:
        table('主统计',['项目','值'],result['stats'].items())
        table('安慰剂',['项目','值'],[(k,v) for k,v in result['placebo'].items() if k not in ('means','candidate_counts','missing')])
        lines += [f"安慰剂与主均值均仅含 P 段已成交交易：{result['placebo']['filled_count']} 笔；"
                  f"排除 unfilled：{result['placebo']['unfilled_count']} 笔。",'']
        table('安慰剂合格时点数',['币/月','数量'],result['placebo']['candidate_counts'].items())
        table('安慰剂无合格时点',['币','月','事件t'],
              [[r['symbol'],r['month'],utc_text(r['t'])] for r in result['placebo']['missing']])
        table('P段C-D原始4h描述',['指标','值'],result['p_c_minus_d'].items())
        for segment,data in result['portfolio'].items():
            lines += [f'{segment} 组合隐藏零息缓冲现金 {CASH_BUFFER}；权益、sizing、日末曲线、收益和 MaxDD 均已扣除。'
                      '真实现金最小值包含初始现金及逐笔成交后现金；负值次数按逐笔成交后计数，最大透支额按同一口径记录，'
                      '包括同根结算前的跳空透支。','']
            table(segment+'组合',['指标','值'],[(k,v) for k,v in data.items() if k not in ('daily','trades','accepted','correlations')])
            table(segment+'组合相关性',['基准','N','相关系数','beta'],
                  [[k,v['N'],v['correlation'],v['beta']] for k,v in data['correlations'].items() if isinstance(v,dict)])
            table(segment+'逐日组合权益',['日','权益','简单日收益','段末结算延伸'],
                  [[utc_text(r['time_ms'])[:10],r['equity'],r['return_'],r['settlement_extension']] for r in data['daily']])
            table(segment+'top60日基准成员数',['日','有效成员'],
                  [[r['day'],r['members']] for r in data['correlations']['daily_top60_member_counts']])
        desc = result['descriptive']
        table('分组描述',['组','N','均值','CR1 SE','t'],
              [[k]+[v[x] for x in ('N','mean','SE','t')] for k,v in desc['groups'].items()])
        for segment in ('P','A'):
            d = desc[segment]
            table(segment+'成本、容量和中断描述',['指标','值'],[(k,v) for k,v in d.items() if k!='worst_10'])
            table(segment+'最差10笔',['币','信号','入场','出场','原因','主成本R_net'],
                  [[r['symbol'],utc_text(r['t']),utc_text(r['entry_time']),utc_text(r['exit_time']),r['exit_reason'],r['R_net_main']]
                   for r in d['worst_10']])
    if result.get('errors'):
        lines += ['## 无效原因','']+['- '+e for e in result['errors']]+['']
    lines += ['## CHOICES（口径全文）','']+['- '+c for c in CHOICES]+['',
              '## 局限','', 'OI下降是强平代理，无法确认被迫卖出；K线不能核实点差、冲击和插针路径。资金费不计入。',
              'A段有样本污染；缺档仍可能让事件无法识别。−100%描述使用简单收益，不进入对数均值。',
              '未作factor_quant配对增量检验；S1不授权S3或模拟盘。全部表仅为冻结规则下的离线研究。', '']
    return '\n'.join(lines)


def write_ledger(result, path):
    def build(k, previous):
        invalid = result['verdict']=='invalid'
        if not invalid and k!=9:
            raise ValueError(f'实际账本K={k}与预注册K=9不一致，拒绝追加')
        stats = result.get('stats',{})
        p = stats.get('P',{})
        return dict(track='fq_crypto',stage='S1',change_type='E',idea_id='liquidation-cascade-long',
                    baseline_version='none-standalone',verdict=result['verdict'],
                    preregistered_K=9,z_K=Z_K,delta=p.get('mean'),se=p.get('SE'),z=p.get('t'),
                    dev_delta=stats.get('H1'),holdout_delta=stats.get('H2'),placebo_pct=stats.get('placebo_pct'),
                    prereg_sha256=PREREG_SHA256,input_hashes=result['input_hashes'],trials=1,
                    errors=result.get('errors',[]),counts_toward_k=not invalid)
    return evolution_ledger.append_event(path,'fq_crypto',build)


def run_study(options):
    check_frozen()
    s0_root,p_root,daily = map(Path,(options.s0_dir,options.p_dir,options.daily_dir))
    out = Path(options.out) if options.out is not None else s0_root/'s1_long_v1'
    ledger = getattr(options,'write_ledger',None)
    guard_output(out,s0_root,p_root,daily,ledger)
    result = dict(version=1,verdict='invalid',valid=False,checks=decide({},False)['checks'],
                  sample_size_pass=False,strong_t_pass=False,errors=[],CHOICES=CHOICES,
                  validation_checks=dict(frozen_hashes=True,p_data=False,a_data=False,p_coverage=False,
                                         s0_events=False,trade_reference=False,portfolio_reference=False,placebo_candidates=False),
                  prereg_sha256=PREREG_SHA256,s0_module_sha256=S0_SHA256,
                  git_head=subprocess.check_output(['git','-C',str(REPO),'rev-parse','HEAD'],text=True).strip(),
                  input_hashes={str(PREREG):file_sha256(PREREG),str(S0_MODULE):file_sha256(S0_MODULE),
                                str(Path(__file__)):file_sha256(__file__)},
                  parameters=dict(k=MAIN_K,m=MAIN_M,fee=FEE,slip=SLIP,stress_slip=STRESS_SLIP,
                                  horizon=4,stop=.20,capital=CAPITAL,cash_buffer=CASH_BUFFER,weight=.1,max_positions=10,
                                  placebo_repeats=2000,seed=20261112,preregistered_K=9,z_K=Z_K,trials=1),
                  allow_data_check_fail=options.allow_data_check_fail)
    out.mkdir(parents=True,exist_ok=False)
    part = out/'trades.jsonl.tmp'
    with part.open('wb') as stream:
        try:
            p_plan,s0_plan = load_p_plan(p_root),load_s0_plan(s0_root)
            s0_events = s0_root/'s0_v1_corrected/events.jsonl'
            for path in (p_root/'plan.json',p_root/'manifest.json',s0_root/'plan.json',s0_root/'manifest.json',s0_events):
                result['input_hashes'][str(path)] = file_sha256(path)
            # manifests recursively bind all raw hashes; plans bind all daily hashes.
            result['daily_sha256'] = p_plan['daily_sha256']
            result['data_check'] = dict(P=inspect_data(p_root,p_plan,daily),A=inspect_data(s0_root,s0_plan,daily))
            result['validation_checks'].update(p_data=result['data_check']['P']['passed'],a_data=result['data_check']['A']['passed'])
            files = grouped_files(p_plan,p_root)
            coverage = []
            for symbol in sorted({s for ss in p_plan['pools'].values() for s in ss if re.fullmatch(r'[A-Z0-9]+USDT',s)}):
                paths = files[symbol]
                oi,_ = read_metrics(paths['metrics'],symbol,paths['expected_days'])
                coverage.extend(coverage_rows(oi,symbol,p_plan['pools']))
            result['metrics_coverage'] = metrics_coverage(coverage)
            result['validation_checks']['p_coverage'] = result['metrics_coverage']['passed']
            data_pass = all(c['passed'] for c in result['data_check'].values()) and result['metrics_coverage']['passed']
            if not data_pass:
                result['errors'].append('P/A数据检查或P metrics覆盖率未通过')
                if not options.allow_data_check_fail or any(c['structural_failure'] for c in result['data_check'].values()):
                    raise InvalidStudy(result['errors'][-1])
            expected = set()
            with s0_events.open(encoding='utf-8') as source:
                for line in source:
                    row = json.loads(line)
                    if row['segment']=='A' and row['category']=='C':
                        expected.add((row['symbol'],int(row['t'])))
            segments = {}
            for segment,root,plan in (('P',p_root,p_plan),('A',s0_root,s0_plan)):
                print(f'{segment}: offline Backtrader trades and portfolio',flush=True)
                segments[segment] = analyze_segment(root,plan,segment,stream)
            p,a = segments['P'],segments['A']
            result['s0_reconciliation'] = reconcile_s0(expected,a['c_set'],a['missing_reasons'])
            result['validation_checks'].update(s0_events=result['s0_reconciliation']['passed'],
                                               trade_reference=True,portfolio_reference=True)
            if not result['s0_reconciliation']['passed']:
                result['errors'].append('A段事件识别未复现S0')
            rows = p['rows']+a['rows']
            primary = summary(p['rows'])
            control = placebo(p['rows'],p['candidates'],primary['mean'])
            result['placebo'] = control
            result['validation_checks']['placebo_candidates'] = control['valid']
            if not control['valid']:
                result['errors'].append('安慰剂存在无合格整点的币月')
            stats = dict(P=primary,H1=summary([r for r in p['rows'] if r['t']<ms('2022-07-01')])['mean'],
                         H2=summary([r for r in p['rows'] if r['t']>=ms('2022-07-01')])['mean'],
                         stress=summary(p['rows'],'R_net_stress')['mean'],A=summary(a['rows'])['mean'],
                         placebo_pct=control['percentile'],portfolio_return=p['portfolio']['total_return'])
            result.update(stats=stats,p_c_minus_d=p['delta'],descriptive=describe(rows),
                          portfolio={s:v['portfolio'] for s,v in segments.items()},
                          max_trade_reference_error={s:v['max_trade_reference_error'] for s,v in segments.items()})
            for s,plan in (('P',p_plan),('A',s0_plan)):
                port = result['portfolio'][s]
                port['correlations'] = correlations(port['daily'],daily,plan['pools'])
            result['valid'] = data_pass and not result['errors']
            result.update(decide(stats,result['valid']))
        except (OSError,ValueError,KeyError,TypeError,zipfile.BadZipFile,pd.errors.ParserError,BacktraderUnavailable) as error:
            result['valid'],result['verdict'] = False,'invalid'
            if str(error) not in result['errors']:
                result['errors'].append(str(error))
    part.replace(out/'trades.jsonl')
    # Persist a concrete result even if an explicitly requested ledger append fails.
    write_json(out/'results.json',result)
    (out/'report.md').write_text(markdown_report(json_safe(result)),encoding='utf-8')
    if ledger is not None:
        result['ledger'] = write_ledger(result,ledger)
        write_json(out/'results.json',result)
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command',required=True)
    for command in ('plan','fetch','run'):
        sub = commands.add_parser(command)
        sub.add_argument('--s0-dir',type=Path,default=DATA/'liquidation_cascade')
        sub.add_argument('--p-dir',type=Path,default=DATA/'liquidation_cascade/p2022')
        if command!='fetch':
            sub.add_argument('--daily-dir',type=Path,default=DATA/'factor_mining/crypto_full/daily')
        if command=='fetch':
            sub.add_argument('--workers',type=int,default=8)
            sub.add_argument('--proxy',default=DEFAULT_PROXY)
        if command=='run':
            sub.add_argument('--out',type=Path)
            sub.add_argument('--allow-data-check-fail',action='store_true',help='仅调试，检查失败仍invalid')
            sub.add_argument('--write-ledger',nargs='?',const=str(DATA/'evolution/ledger.jsonl'))
    args = parser.parse_args(argv)
    try:
        check_frozen()
        # P must not alias the S0 root, raw archives, or frozen S0 result directory.
        if args.command in ('plan','fetch'):
            p,s = args.p_dir.resolve(),args.s0_dir.resolve()
            if p==s or s.is_relative_to(p) or p.is_relative_to(s/'raw') or p.is_relative_to(s/'s0_v1_corrected'):
                raise ValueError('P根不可覆盖S0输入')
        if args.command=='plan':
            if (args.p_dir/'plan.json').exists() or (args.p_dir/'manifest.json').exists():
                raise ValueError('P plan/manifest已存在，拒绝覆盖')
            plan = make_plan(args.daily_dir)
            write_json(args.p_dir/'plan.json',plan)
            print(json.dumps(plan['counts'],ensure_ascii=False))
            return 0
        if args.command=='fetch':
            manifest = fetch_plan(args.p_dir,load_p_plan(args.p_dir),workers=args.workers,proxy=args.proxy)
            return 1 if manifest['errors'] else 0
        result = run_study(args)
        print(f"VERDICT {result['verdict']}")
        return 0 if result['valid'] else 1
    except (OSError,ValueError,KeyError,TypeError,zipfile.BadZipFile,pd.errors.ParserError,
            subprocess.CalledProcessError) as error:
        parser.exit(1,f'H2b S1: {error}\n')


if __name__=='__main__':
    raise SystemExit(main())
