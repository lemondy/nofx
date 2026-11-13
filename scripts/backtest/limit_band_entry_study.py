"""Frozen C1 limit-band entry S1: read-only export, incremental fetch, offline run.

python -m scripts.backtest.limit_band_entry_study --help
Only the explicit fetch command uses the network. No trading configuration is written.
"""
from __future__ import annotations

import argparse
from collections import defaultdict
from contextlib import closing
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import sqlite3
import time

from . import evolution_ledger, market_cache
from .backtrader_adapter import _backtrader
from .early_trim_offline_study import (
    REPO, MINUTE, LIVE, ENSEMBLE_CANDIDATE, ENSEMBLE_CONTROL,
    TRIM_CANDIDATE, TRIM_CONTROL, sha256, timestamp_ms, write_json,
    load_trades, kline_filename, validate_klines, file_metadata,
    direction_risk, mean, z_score, comparison,
)
from .incremental_filter import weekly_statistics, z_threshold
from scripts.promotion import report as promotion_report

PREREG = REPO / 'docs/research/2026-11-13-limit-band-entry-s1-prereg.md'
PREREG_SHA256 = 'ff8023b1f02dab2df1cba03f5113c5da0a93e356cbff1582d0e33e9ad9753b5c'
SINCE = timestamp_ms('2026-10-13T00:00:00Z')
UNTIL = timestamp_ms('2026-11-13T00:00:00Z')
TRADERS = (LIVE, ENSEMBLE_CANDIDATE, ENSEMBLE_CONTROL, TRIM_CANDIDATE, TRIM_CONTROL)
FIELDS = ('id', 'trader_id', 'symbol', 'side', 'entry_time', 'exit_time',
          'entry_price', 'exit_price', 'initial_stop_loss', 'ai_managed')
PHI, FEE, MAKER_FEE, SLIPPAGE = .0008, .0005, .0002, .0003
K, Z_K = 6, 2.638  # Frozen literal, not the unrounded z_threshold(6).
LEDGER = REPO / 'data/evolution/ledger.jsonl'
KLINES_DIR = REPO / 'data/limit_band_c1/kl1m'
NOTES = [
    '时间统一为 UTC 毫秒；export 将整值时间归一为整数，Tx>=Te（允许同刻），无效出场排除并计数。下载上下界均含，最后一根开盘为 Te_min+31m（共 1472 根），仅使用 +1 至 +30m 撮合。',
    '策略必须有显式 JSON 布尔 false；缺失、null、数值 0 或缺少任一交易员策略均拒绝。run 必须提供与交易哈希绑定的 export_meta.json。',
    '全部交易（含模拟盘）要求 ai_managed=1；不依赖数据库 quantity，两个账户均使用一单位资产，初始 R=0 或无效字段判 invalid。',
    '价格匹配率及覆盖剔除率的分母均为全部导出交易；缺少入场分钟计不匹配，价格不匹配不另行剔除。结构或哈希错误直接 invalid。',
    '前窗固定 1440 根，后窗固定 +1 至 +30m 共 30 根，覆盖校验不因 Tx 提前而缩短；不插值补缺。',
    'Roll 使用全部有效相邻分钟收益的同一个均值去均值，仅相邻收益端点相差一分钟才构成 lag pair；无 lag pair 判 invalid。',
    '容许的一根后窗缺口若使到期未成交交易无法确认完整 30 根和回退价格，判 invalid，不假设 missed 或补造回退价；已观察到穿价可按同一 L 成交。',
    '穿价成交时间保守记为该根收盘；候选入场与实际出场同刻时，仍按顺序回放两笔订单并扣两次费用。',
    '越过初始止损的 L 归为 stop_guard，两臂均回放实际市价入场；不计为 filled、fallback 或 missed，单独报告数量与占比。',
    '两半按 entry_time、id 排序，奇数笔前半 floor(n/2)、后半其余；不足三周不给 SE/z，不能通过筛选。',
    'SE=0 时均值为零则 z=0，否则 z 为相应正负无穷，JSON 用 inf/-inf 字符串；日聚类同样至少三簇。',
    '分位数使用排序后的线性插值，报告 0/25/50/75/100%；s_G=0 时 gamma=inf，含无穷端点的非零权重插值为 inf。',
    '逆向选择未成交组包括 fallback/missed，排除 stop_guard；不转市价敏感性剔除未成交时仅保留 filled 和不改变入场的 stop_guard。',
    '描述性敏感性全部复用同一有效样本与实际出场，只走算术路径；若仅敏感性遇到无法确定的缺口，该项整组不汇总并列明错误，不影响主判定。不得据此选参数，主检验 trials=1。',
    '冻结导出及已有结果拒绝覆盖；路径保护含符号链接、硬链接及写入临时文件。fetch 只增补缺失分钟，已有 manifest 文件必须先通过哈希核对；run 默认不写账本。',
    '账本按 append_event 原子计算实际 K，有效结果仅实际 K=6 时可追加；invalid 不计 K，仍记录 preregistered_K=6。',
]
LIMITATIONS = [
    '分钟成交价极值不能保证挂单队列成交，触价敏感性仅作乐观上限。',
    '模拟盘 E 可能不含真实滑点，基线可能被高估；实际出场 X/Tx 外生，未重算依赖入场价的 LLM、保本和减仓决策。',
    'Roll 在 1m 收盘价上测到的主要是买卖价反弹，不能直接观测论文中间价 gap。',
    '样本与 early-trim、Kronos 部分重叠，基线已经看过；少量周簇使不确定性估计不稳定。',
    '不模拟资金费、强平及市场冲击；合成测试只证明实现，不证明历史有效或原论文结果复现。',
]


def verify_prereg(path=None):
    raw = Path(PREREG if path is None else path).read_bytes()
    if sha256(raw) != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run/output')
    return raw


def protect_outputs(destinations, protected=(), protected_dirs=(), *, new=False):
    """Check final/temporary paths and existing hardlinks before any mutation."""
    paths = [Path(p) for p in destinations]
    destinations = [p.resolve() for p in paths]
    targets = destinations + [p.with_name(p.name + '.tmp').resolve() for p in paths]
    protected = {PREREG.resolve(), *(Path(p).resolve() for p in protected)}
    directories = [Path(p).resolve() for p in protected_dirs]
    def inode(path):
        stat = path.stat()
        return stat.st_dev, stat.st_ino
    target_inodes = [inode(p) for p in targets if p.exists()]
    protected_inodes = {inode(p) for p in protected if p.exists()}
    if (len(set(targets)) != len(targets) or len(set(target_inodes)) != len(target_inodes)
            or any(i in protected_inodes for i in target_inodes)
            or any(p in protected or any(p.is_relative_to(d) for d in directories) for p in targets)):
        raise ValueError('输出与账本不可覆盖冻结输入或彼此覆盖')
    if new and any(p.exists() for p in destinations):
        raise ValueError('拒绝覆盖已有冻结文件或结果')


def check_strategy_metadata(strategies):
    if not isinstance(strategies, dict) or set(strategies) != set(TRADERS):
        raise ValueError('缺少或重复交易员策略记录')
    for trader, setting in strategies.items():
        if not isinstance(setting, dict) or setting.get('limit_entry_enabled') is not False:
            raise ValueError(f'{trader} limit_entry_enabled 必须为显式 false')


def export_snapshot(connection):
    """Read an existing snapshot; also permits entirely in-memory SQLite tests."""
    connection.row_factory = sqlite3.Row
    strategies = {}
    for row in connection.execute(
            'SELECT t.id AS trader_id,s.id AS strategy_id,s.config '
            'FROM traders t JOIN strategies s ON s.id=t.strategy_id '
            f"WHERE t.id IN ({','.join('?' for _ in TRADERS)})", TRADERS):
        if row['trader_id'] in strategies:
            raise ValueError('重复交易员策略记录')
        config = json.loads(row['config'])
        risk = config.get('risk_control') if isinstance(config, dict) else None
        strategies[row['trader_id']] = dict(
            strategy_id=row['strategy_id'], config_sha256=sha256(row['config'].encode()),
            limit_entry_enabled=risk.get('limit_entry_enabled') if isinstance(risk, dict) else None)
    check_strategy_metadata(strategies)
    filters = ("status='CLOSED' AND initial_stop_loss>0 AND entry_price>0 AND exit_price>0 "
               'AND exit_time>0 AND ai_managed=1 AND entry_time>=? AND entry_time<? '
               f"AND trader_id IN ({','.join('?' for _ in TRADERS)})")
    rows = connection.execute('SELECT ' + ','.join(FIELDS) + ' FROM trader_positions WHERE ' +
                              filters + ' ORDER BY entry_time,id', (SINCE, UNTIL, *TRADERS))
    trades, invalid_exit = [], 0
    for row in rows:
        item = dict(row)
        try:
            entry, exited = timestamp_ms(item['entry_time']), timestamp_ms(item['exit_time'])
            if exited < entry or not math.isfinite(item['exit_price']):
                raise ValueError('无效出场')
        except (ValueError, TypeError, AttributeError, OverflowError):
            invalid_exit += 1
            continue
        item.update(entry_time=entry, exit_time=exited)
        trades.append(item)
    return trades, dict(strategies=strategies, invalid_exit_excluded=invalid_exit,
        selection=dict(sql=filters, since_ms=SINCE, until_ms=UNTIL, traders=list(TRADERS),
                       ai_managed_only=True, valid_exit='integer epoch ms, exit_time>=entry_time'))


def export_trades(db, out):
    verify_prereg()
    out = Path(out)
    paths = [out / 'trades.jsonl', out / 'export_meta.json']
    protect_outputs(paths, [db], new=True)
    with closing(sqlite3.connect(Path(db).resolve().as_uri() + '?mode=ro', uri=True)) as connection:
        connection.execute('PRAGMA query_only=ON')
        connection.execute('BEGIN')
        trades, metadata = export_snapshot(connection)
    raw = ''.join(json.dumps(t, ensure_ascii=False, allow_nan=False, separators=(',', ':')) + '\n'
                  for t in trades).encode()
    meta = dict(metadata, version=1, n=len(trades), sha256=sha256(raw), prereg_sha256=PREREG_SHA256,
                counts_by_trader={t: sum(row['trader_id'] == t for row in trades) for t in TRADERS})
    out.mkdir(parents=True, exist_ok=True)
    with paths[0].open('xb') as handle:
        handle.write(raw)
    write_json(paths[1], meta)
    return meta


def kline_window(trade):
    entry = timestamp_ms(trade['entry_time']) // MINUTE * MINUTE
    return entry - 1440 * MINUTE, entry + 32 * MINUTE  # inclusive +31m, exclusive end


def fetch_klines(trades_path, klines_dir=KLINES_DIR, sleep=.3, *, fetch=None, sleeper=time.sleep):
    """Preserve cached rows; market_cache paces every HTTP page at >=0.3 seconds."""
    verify_prereg()
    if not math.isfinite(sleep) or sleep < .3:
        raise ValueError('--sleep 必须至少为 0.3 秒')
    fetch = market_cache.fetch_binance_klines if fetch is None else fetch
    trades, digest = load_trades(trades_path)
    base = Path(klines_dir)
    manifest_path = base / 'manifest.json'
    tasks = {kline_filename(t): (t['symbol'], *kline_window(t)) for t in trades}
    protect_outputs([manifest_path, *(base / name for name in tasks)],
                    [trades_path, Path(trades_path).with_name('export_meta.json')])
    manifest = json.loads(manifest_path.read_bytes()) if manifest_path.exists() else dict(version=1, files={})
    if not isinstance(manifest, dict) or not isinstance(manifest.get('files'), dict):
        raise ValueError('manifest 必须包含 files 对象')
    if manifest.get('trades_sha256', digest) != digest:
        raise ValueError('manifest 与冻结交易不一致')
    # Preflight every file before changing any cache bytes.
    existing = {}
    for name in tasks:
        path = base / name
        rows = json.loads(path.read_bytes()) if path.exists() else []
        validate_klines(rows)
        if name in manifest['files']:
            recorded = manifest['files'][name]
            if (not path.exists() or not isinstance(recorded, dict)
                    or any(recorded.get(k) != v for k, v in file_metadata(path, rows).items())):
                raise ValueError(f'{name} manifest 与缓存不一致')
        existing[name] = rows
    base.mkdir(parents=True, exist_ok=True)
    requested = False
    for name, (symbol, start, end) in sorted(tasks.items()):
        path = base / name
        cached = {row[0]: row for row in existing[name]}
        missing = set(range(start, end, MINUTE)) - cached.keys()
        if missing:
            if requested:
                sleeper(sleep)
            downloaded = fetch(symbol, '1m', min(missing), max(missing) + MINUTE)
            requested = True
            validate_klines(downloaded)
            for row in downloaded:
                if start <= row[0] < end:
                    cached.setdefault(row[0], row)
            write_json(path, [cached[t] for t in sorted(cached)])
        rows = [cached[t] for t in sorted(cached)]
        manifest['files'][name] = dict(file_metadata(path, rows), requested_start_ms=start,
            requested_end_ms=end, complete=not (set(range(start, end, MINUTE)) - cached.keys()))
        manifest['trades_sha256'] = digest
        write_json(manifest_path, manifest)
    if not tasks:
        manifest['trades_sha256'] = digest
        write_json(manifest_path, manifest)
    return manifest


def validate_trade(trade):
    if any(field not in trade for field in FIELDS):
        raise ValueError('交易字段缺失')
    if trade['trader_id'] not in TRADERS or trade['ai_managed'] != 1:
        raise ValueError('交易员或 ai_managed 不符合筛选')
    entry, exited = timestamp_ms(trade['entry_time']), timestamp_ms(trade['exit_time'])
    if not SINCE <= entry < UNTIL or exited < entry:
        raise ValueError('入场区间或出场时间无效')
    for key in ('entry_price', 'exit_price', 'initial_stop_loss'):
        if not math.isfinite(trade[key]) or trade[key] <= 0:
            raise ValueError(f'{key} 必须为有限正数')
    if trade['entry_price'] == trade['initial_stop_loss']:
        raise ValueError('初始 R 为零')
    if not isinstance(trade['side'], str) or trade['side'].lower() not in ('long', 'short'):
        raise ValueError('方向无效')


def roll_estimate(trade, rows):
    entry = trade['entry_time'] // MINUTE * MINUTE
    pre = [r for r in rows if entry - 1440 * MINUTE <= r[0] < entry]
    returns = {b[0]: math.log(float(b[4]) / float(a[4]))
               for a, b in zip(pre, pre[1:]) if b[0] - a[0] == MINUTE}
    average = mean(returns.values())
    pairs = [(returns[t - MINUTE], r) for t, r in returns.items() if t - MINUTE in returns]
    if not pairs:
        raise ValueError('Roll 无有效 lag pair')
    cov1 = math.fsum((a - average) * (b - average) for a, b in pairs) / len(pairs)
    return dict(s_g=math.sqrt(max(0., -cov1)), cov1=cov1, n_returns=len(returns), n_pairs=len(pairs))


def theta_star(s_g):
    return .5 * (PHI + math.sqrt(PHI * PHI + 4 * s_g * s_g))


def validate_data(trades, klines_dir):
    base = Path(klines_dir)
    reasons, details, retained = [], [], []
    try:
        manifest = json.loads((base / 'manifest.json').read_bytes())
        if not isinstance(manifest, dict) or not isinstance(manifest.get('files'), dict):
            raise ValueError('manifest 必须包含 files 对象')
    except (OSError, ValueError) as exc:
        manifest = dict(files={})
        reasons.append(f'manifest 缺失或无效 {exc}')
    for trade in trades:
        detail = dict(id=trade['id'], entry_price_match=False, excluded=True)
        try:
            validate_trade(trade)
            path = base / kline_filename(trade)
            rows = json.loads(path.read_bytes()) if path.exists() else []
            validate_klines(rows)
            detail['file'] = path.name
            if path.exists():
                actual = file_metadata(path, rows)
                detail['sha256'] = actual['sha256']
                recorded = manifest['files'].get(path.name)
                if not isinstance(recorded, dict) or any(recorded.get(k) != v for k, v in actual.items()):
                    reasons.append(f'{path.name} manifest 哈希、行数或区间不一致')
            entry = trade['entry_time'] // MINUTE * MINUTE
            indexed = {r[0]: r for r in rows}
            missing_pre = sum(t not in indexed for t in range(entry - 1440 * MINUTE, entry, MINUTE))
            missing_post = sum(entry + i * MINUTE not in indexed for i in range(1, 31))
            bar = indexed.get(entry)
            excluded = missing_pre / 1440 > .02 or missing_post > 1
            detail.update(pre_missing=missing_pre, pre_missing_fraction=missing_pre / 1440,
                post_missing=missing_post, excluded=excluded,
                entry_price_match=bar is not None and
                    float(bar[3]) * .998 <= trade['entry_price'] <= float(bar[2]) * 1.002)
            if not excluded:
                roll_estimate(trade, rows)  # mechanism availability before any returns
                retained.append((trade, rows))
        except (OSError, ValueError, TypeError, KeyError, AttributeError, OverflowError) as exc:
            detail.update(error=str(exc), excluded=True)
            reasons.append(f"交易 {trade['id']} 输入无效 {exc}")
        details.append(detail)
    n = len(trades)
    excluded = n - len(retained)
    exclusion_fraction = excluded / n if n else 1.
    price_fraction = sum(d['entry_price_match'] for d in details) / n if n else 0.
    if exclusion_fraction > .05:
        reasons.append('分钟覆盖剔除比例超过 5%')
    if price_fraction < .95:
        reasons.append('入场价匹配比例低于 95%')
    if len(retained) < 100:
        reasons.append('有效交易不足 100')
    return dict(valid=not reasons, n=n, excluded=excluded, exclusion_fraction=exclusion_fraction,
                entry_price_match_fraction=price_fraction, reasons=reasons, details=details), retained


def entry_plan(trade, rows, *, touch=False, depth=None, fallback=True,
               slippage=SLIPPAGE, maker_fee=MAKER_FEE):
    """Determine prescribed execution events; never infer an intrabar path."""
    roll = roll_estimate(trade, rows)
    theta = theta_star(roll['s_g']) if depth is None else depth
    sign, _ = direction_risk(trade)
    limit = trade['entry_price'] * (1 - sign * theta)
    plan = dict(roll, theta=theta, gamma=PHI / roll['s_g'] if roll['s_g'] else math.inf, limit=limit)
    if sign * (limit - trade['initial_stop_loss']) <= 0:
        return dict(plan, outcome='stop_guard', price=trade['entry_price'], fee=FEE, time_ms=trade['entry_time'])
    entry = trade['entry_time'] // MINUTE * MINUTE
    eligible = [r for r in rows if entry < r[0] <= entry + 30 * MINUTE
                and r[0] + MINUTE <= trade['exit_time']]
    for row in eligible:
        extreme = float(row[3 if sign > 0 else 2])
        through = sign * (limit - extreme)
        if through > 0 or (touch and through == 0):
            return dict(plan, outcome='filled', price=limit, fee=maker_fee, time_ms=row[0] + MINUTE)
    if not fallback:
        return dict(plan, outcome='unfilled', price=None, fee=None, time_ms=None)
    if trade['exit_time'] < entry + 31 * MINUTE:
        return dict(plan, outcome='missed', price=None, fee=None, time_ms=None)
    if len(eligible) != 30:
        raise ValueError('未成交且后窗缺口，无法确认完整 30 根及回退价格')
    return dict(plan, outcome='fallback', price=float(eligible[-1][4]) * (1 + sign * slippage),
                fee=FEE, time_ms=entry + 31 * MINUTE)


def arithmetic_reference(trade, rows, **options):
    plan = entry_plan(trade, rows, **options)
    sign, risk = direction_risk(trade)
    e, x = trade['entry_price'], trade['exit_price']
    b = (sign * (x - e) - FEE * e - FEE * x) / risk
    p = plan['price']
    c = 0. if p is None else (sign * (x - p) - plan['fee'] * p - FEE * x) / risk
    return dict(baseline=b, candidate=c, delta=c - b, plan=plan)


def simulate_arm(trade, plan=None):
    """Replay entry and X through native BackBroker, as in the early-trim template.

    Point-price COC events preserve the frozen limit price (no gap improvement).
    The native commission model distinguishes opening maker from closing taker.
    A flush tick settles the final order; same-time entry/exit remain separate.
    """
    bt = _backtrader()
    sign, risk = direction_risk(trade)
    if plan is not None and plan['price'] is None:
        return dict(r=0., fills=[])
    price = trade['entry_price'] if plan is None else plan['price']
    fee = FEE if plan is None else plan['fee']
    entered = trade['entry_time'] if plan is None else plan['time_ms']
    events = [dict(time_ms=entered, price=price, kind='entry'),
              dict(time_ms=trade['exit_time'], price=trade['exit_price'], kind='exit'),
              dict(time_ms=trade['exit_time'] + 1, price=trade['exit_price'], kind='flush')]

    class EventFeed(bt.feed.DataBase):
        def start(self):
            super().start()
            self.events = iter(events)

        def _load(self):
            event = next(self.events, None)
            if event is None:
                return False
            self.lines.datetime[0] = bt.date2num(datetime.fromtimestamp(event['time_ms'] / 1000, timezone.utc))
            for line in (self.lines.open, self.lines.high, self.lines.low, self.lines.close):
                line[0] = event['price']
            self.lines.volume[0] = self.lines.openinterest[0] = 0
            return True

    class Fees(bt.CommInfoBase):
        params = (('stocklike', True), ('commtype', bt.CommInfoBase.COMM_PERC), ('percabs', True))

        def _getcommission(self, size, price, pseudoexec):
            return abs(size) * price * (fee if size * sign > 0 else FEE)

    class Replay(bt.Strategy):
        def __init__(self):
            self.fills, self.net_pnl = [], None

        def next(self):
            event = events[len(self.data) - 1]
            if event['kind'] == 'flush':
                return
            opening = event['kind'] == 'entry'
            submit = self.buy if (sign > 0) == opening else self.sell
            order = submit(size=1., exectype=bt.Order.Market)
            order.addinfo(event=event)

        def notify_order(self, order):
            if order.status == bt.Order.Completed:
                event = order.info['event']
                if abs(order.executed.price - event['price']) > 4 * math.ulp(event['price']):
                    raise RuntimeError('Backtrader 未按指定价格成交')
                self.fills.append(dict(kind=event['kind'], time_ms=event['time_ms'],
                    price=order.executed.price, size=order.executed.size, commission=order.executed.comm))
            elif order.status in (bt.Order.Margin, bt.Order.Rejected, bt.Order.Canceled, bt.Order.Expired):
                raise RuntimeError(f'Backtrader 订单失败 {order.getstatusname()}')

        def notify_trade(self, closed):
            if closed.isclosed:
                self.net_pnl = closed.pnlcomm

    cerebro = bt.Cerebro(stdstats=False)
    cerebro.adddata(EventFeed())
    cerebro.addstrategy(Replay)
    cerebro.broker.setcash(max(e['price'] for e in events) * 10)
    cerebro.broker.addcommissioninfo(Fees())
    cerebro.broker.set_coc(True)
    replay = cerebro.run(runonce=False)[0]
    if replay.position.size != 0 or replay.net_pnl is None or len(replay.fills) != 2:
        raise RuntimeError('Backtrader 未完整平仓')
    return dict(r=replay.net_pnl / risk, fills=replay.fills)


def replay_pair(trade, rows):
    reference = arithmetic_reference(trade, rows)
    baseline = simulate_arm(trade)
    candidate = simulate_arm(trade, reference['plan'])
    delta = candidate['r'] - baseline['r']
    if not all(math.isfinite(v) for v in (baseline['r'], candidate['r'], delta,
                                         reference['baseline'], reference['candidate'], reference['delta'])):
        raise RuntimeError('Backtrader 或算术参考产生非有限 R')
    error = max(abs(baseline['r'] - reference['baseline']),
                abs(candidate['r'] - reference['candidate']), abs(delta - reference['delta']))
    if not math.isfinite(error) or error > 1e-9:
        raise RuntimeError('Backtrader 与算术参考差超过 1e-9 R')
    return dict(id=trade['id'], trader_id=trade['trader_id'], side=trade['side'].lower(),
                entry_time=trade['entry_time'], baseline=baseline, candidate=candidate,
                delta=delta, plan=reference['plan'], reference=reference, max_error_r=error)


def primary_statistics(rows):
    stats = comparison(rows)
    nonmissed = [r['delta'] for r in rows if r['plan']['outcome'] != 'missed']
    return dict(stats, missed_n=len(rows) - len(nonmissed),
                missed_fraction=(len(rows) - len(nonmissed)) / len(rows) if rows else 0.,
                delta_excluding_missed=mean(nonmissed))


def decide_verdict(stats, valid=True):
    if not valid or stats['n'] < 100:
        return 'invalid'
    if (stats['missed_fraction'] > .05 and stats['mean_delta'] > 0
            and stats['delta_excluding_missed'] is not None and stats['delta_excluding_missed'] <= 0):
        return 'fewer_trades'
    if all(stats[k] is not None and stats[k] > 0 for k in ('dev_delta', 'holdout_delta')):
        if stats['z'] is not None and stats['z'] >= Z_K:
            return 'strong'
        if stats['z'] is not None and stats['z'] >= 1:
            return 'screen_pass'
    return 'fail'


def quantiles(values):
    ordered = sorted(values)
    result = {}
    for q in (0., .25, .5, .75, 1.):
        if not ordered:
            value = None
        else:
            index = q * (len(ordered) - 1)
            left, right = ordered[math.floor(index)], ordered[math.ceil(index)]
            fraction = index - math.floor(index)
            value = left if fraction == 0 or left == right else (
                math.inf if math.isinf(right) else left + fraction * (right - left))
        result[str(q)] = value
    return result


def describe(rows, retained):
    daily, traders, sides = defaultdict(list), defaultdict(list), defaultdict(list)
    outcomes = {k: [] for k in ('filled', 'fallback', 'missed', 'stop_guard')}
    for row in rows:
        daily[datetime.fromtimestamp(row['entry_time'] / 1000, timezone.utc).date().isoformat()].append(row['delta'])
        traders[row['trader_id']].append(row['delta'])
        sides[row['side']].append(row['delta'])
        outcomes[row['plan']['outcome']].append(row)
    avg = mean(row['delta'] for row in rows)
    se = promotion_report.cluster_se([
        math.fsum((v - avg) / len(rows) for v in group) for group in daily.values()]) if len(daily) >= 3 else None
    def summary(values):
        return dict(n=len(values), mean_delta=mean(values))
    filled_b = mean(row['baseline']['r'] for row in outcomes['filled'])
    notfilled_b = mean(row['baseline']['r'] for k in ('fallback', 'missed') for row in outcomes[k])
    sensitivities = {}
    variants = [('touch', dict(touch=True)), ('fixed_4bp', dict(depth=.0004)),
                ('fixed_15bp', dict(depth=.0015)), ('fixed_30bp', dict(depth=.003)),
                ('no_fallback', dict(fallback=False)), ('fallback_slippage_0', dict(slippage=0.)),
                ('maker_fee_5bp', dict(maker_fee=FEE))]
    for name, options in variants:
        samples, unfilled, errors = [], [], []
        for trade, bars in retained:
            try:
                ref = arithmetic_reference(trade, bars, **options)
            except (ValueError, ArithmeticError) as exc:
                errors.append(dict(id=trade['id'], error=str(exc)))
                continue
            samples.append(dict(id=trade['id'], entry_time=trade['entry_time'], delta=ref['delta']))
            unfilled.append(ref['plan']['outcome'] == 'unfilled')
        if errors:
            sensitivities[name] = dict(available=False, n=len(retained), errors=errors, statistics=None)
            continue
        sensitivities[name] = dict(comparison(samples), available=True)
        if name == 'no_fallback':
            sensitivities[name].update(unfilled_n=sum(unfilled),
                delta_excluding_unfilled=mean(r['delta'] for r, drop in zip(samples, unfilled) if not drop))
    return dict(
        outcomes={k: dict(summary([r['delta'] for r in group]), rate=len(group) / len(rows)) for k, group in outcomes.items()},
        adverse_selection=dict(filled_mean_b=filled_b, not_filled_mean_b=notfilled_b,
            difference=filled_b - notfilled_b if filled_b is not None and notfilled_b is not None else None),
        by_trader={k: summary(v) for k, v in traders.items()}, by_side={k: summary(v) for k, v in sides.items()},
        daily=dict(n_days=len(daily), mean_delta=avg, se=se, z=z_score(avg, se)),
        s_g_median_bp=quantiles([r['plan']['s_g'] * 10000 for r in rows])['0.5'],
        cov1_nonnegative_fraction=sum(r['plan']['cov1'] >= 0 for r in rows) / len(rows),
        theta_quantiles=quantiles([r['plan']['theta'] for r in rows]),
        gamma_quantiles=quantiles([r['plan']['gamma'] for r in rows]), sensitivities=sensitivities)


def study(trades_path, klines_dir):
    before = verify_prereg()
    trades, digest = load_trades(trades_path)
    validation, retained = validate_data(trades, klines_dir)
    reasons = list(validation['reasons'])
    meta_path, manifest_path = Path(trades_path).with_name('export_meta.json'), Path(klines_dir) / 'manifest.json'
    meta = None
    try:
        meta = json.loads(meta_path.read_bytes())
        if (not isinstance(meta, dict) or meta.get('sha256') != digest or meta.get('n') != len(trades)
                or meta.get('prereg_sha256') != PREREG_SHA256):
            raise ValueError('export_meta 与冻结交易或预注册不一致')
        check_strategy_metadata(meta.get('strategies'))
    except (OSError, ValueError, TypeError) as exc:
        reasons.append(f'策略元数据校验失败 {exc}')
    try:
        manifest = json.loads(manifest_path.read_bytes())
        if manifest.get('trades_sha256') != digest:
            reasons.append('manifest 与冻结交易哈希不一致')
    except (OSError, ValueError, AttributeError):
        pass  # validate_data already records malformed/missing manifests
    result = dict(version=1, engine='backtrader', prereg_sha256=PREREG_SHA256,
        inputs=dict(trades_sha256=digest, manifest_sha256=sha256(manifest_path.read_bytes()) if manifest_path.exists() else None,
                    export_meta_sha256=sha256(meta_path.read_bytes()) if meta_path.exists() else None,
                    klines={d['file']: d['sha256'] for d in validation['details'] if 'sha256' in d}),
        strategies=meta.get('strategies') if isinstance(meta, dict) else None,
        validation=validation, invalid_reasons=reasons, n_total=len(trades), n=len(retained),
        verdict='invalid', statistics=None, descriptions=None, trades=[],
        K=K, z_K=Z_K, z_K_unrounded=z_threshold(K), trials=1,
        phi=PHI, taker_fee=FEE, maker_fee=MAKER_FEE, fallback_slippage=SLIPPAGE,
        notes=NOTES, limitations=LIMITATIONS)
    if not reasons:
        try:
            result['engine_version'] = _backtrader().__version__
            # Check execution availability for all trades before any PnL.
            for trade, bars in retained:
                entry_plan(trade, bars)
            replayed = []
            for trade, bars in retained:
                replayed.append(replay_pair(trade, bars))
            result['mechanism'] = dict(n=len(replayed), max_error_r=max(r['max_error_r'] for r in replayed), valid=True)
            result['trades'] = replayed
            # Aggregate only after every pair passed the arithmetic check.
            result['statistics'] = primary_statistics(replayed)
            result['arm_means'] = {arm: mean(r[arm]['r'] for r in replayed) for arm in ('baseline', 'candidate')}
            result['descriptions'] = describe(replayed, retained)
            result['verdict'] = decide_verdict(result['statistics'])
        except (RuntimeError, ValueError, ArithmeticError) as exc:
            reasons.append(f'机制或回放校验失败 {exc}')
            result.update(verdict='invalid', statistics=None, descriptions=None)
            result['mechanism'] = dict(valid=False, error=str(exc))
    if verify_prereg() != before:
        raise ValueError('Preregistration changed during study; refusing output')
    return result


def report(result):
    lines = ['# C1 带宽限价入场离线 S1', '',
        f'判定 `{result["verdict"]}`。有效交易 {result["n"]} / {result["n_total"]}。', '',
        f'预注册 SHA256 `{PREREG_SHA256}`。track `crypto_llm`，类型 C，K=6，阈值 2.638。']
    if result['invalid_reasons']:
        lines += ['', '## 无效原因', ''] + ['- ' + reason for reason in result['invalid_reasons']]
    for title, data in (('输入校验', result['validation']), ('Backtrader 与算术机制核对', result.get('mechanism')),
                       ('两臂净 R 均值', result.get('arm_means')), ('主统计与时间两半', result['statistics']),
                       ('仅描述项（不参与判定）', result['descriptions']), ('冻结输入', result['inputs'])):
        if data is not None:
            data = {k: v for k, v in data.items() if k != 'details'}
            lines += ['', '## ' + title, '', '```json',
                      json.dumps(evolution_ledger.json_safe(data), ensure_ascii=False, indent=2), '```']
    action = {'fail': '关闭 C1，没有新证据不再提议接入。',
              'fewer_trades': '改善由少做交易驱动，关闭 C1，没有新证据不再提议接入。',
              'screen_pass': '增量筛选通过，建议向用户提出 S2 线上配对，由用户决定是否实施。',
              'strong': '增量筛选通过，建议向用户提出 S2 线上配对，由用户决定是否实施。',
              'invalid': '证据不足：校验未通过，本次无法给出有效的增量筛选结论。'}[result['verdict']]
    lines += ['', '## 后续', '', action, '', '## NOTES：执行口径与补充约定', '']
    lines += ['- ' + note for note in NOTES]
    lines += ['', '## 预先承认的局限', ''] + ['- ' + note for note in LIMITATIONS]
    lines += ['', '逐笔成交、参考值、剔除明细见 results.json。主检验一次，敏感性仅作描述。', '']
    return '\n'.join(lines)


def write_ledger(result, path):
    def build(k, previous):
        if result['verdict'] != 'invalid' and k != K:
            raise ValueError(f'实际账本 K={k} 与预注册 K=6 不一致，拒绝追加')
        stats = result['statistics'] or {}
        return dict(track='crypto_llm', stage='S1', change_type='C', idea_id='limit-band-entry-c1-v1',
            baseline_version='market-entry', verdict=result['verdict'], n=result['n'],
            delta=stats.get('mean_delta'), se=stats.get('se'), z=stats.get('z'),
            dev_delta=stats.get('dev_delta'), holdout_delta=stats.get('holdout_delta'),
            preregistered_K=K, z_K=Z_K, prereg_sha256=PREREG_SHA256, input_hashes=result['inputs'], trials=1)
    return evolution_ledger.append_event(path, 'crypto_llm', build)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    export = sub.add_parser('export')
    export.add_argument('--db', type=Path, required=True)
    export.add_argument('--out', type=Path, required=True)
    fetch = sub.add_parser('fetch')
    fetch.add_argument('--trades', type=Path, required=True)
    fetch.add_argument('--klines-dir', type=Path, default=KLINES_DIR)
    fetch.add_argument('--sleep', type=float, default=.3)
    run = sub.add_parser('run')
    run.add_argument('--trades', type=Path, required=True)
    run.add_argument('--klines-dir', type=Path, default=KLINES_DIR)
    run.add_argument('--out', type=Path, required=True)
    run.add_argument('--write-ledger', type=Path, nargs='?', const=LEDGER)
    args = parser.parse_args(argv)
    try:
        if args.command == 'export':
            meta = export_trades(args.db, args.out)
            print(f'导出 {meta["n"]} 笔，SHA256 {meta["sha256"]}')
        elif args.command == 'fetch':
            fetch_klines(args.trades, args.klines_dir, args.sleep)
        else:
            destinations = [args.out / 'results.json', args.out / 'report.md']
            protect_outputs(destinations, [args.trades, args.trades.with_name('export_meta.json')],
                            [args.klines_dir], new=True)
            if args.write_ledger is not None:
                protect_outputs([*destinations, args.write_ledger],
                    [args.trades, args.trades.with_name('export_meta.json')], [args.klines_dir])
            result = study(args.trades, args.klines_dir)
            write_json(destinations[0], result)
            destinations[1].write_text(report(result), encoding='utf-8')
            if args.write_ledger is not None:
                write_ledger(result, args.write_ledger)
            print(f'VERDICT {result["verdict"]}')
            return 1 if result['verdict'] == 'invalid' else 0
    except (ValueError, OSError, sqlite3.Error) as exc:
        parser.error(str(exc))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
