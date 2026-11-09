"""Frozen early-trim S1 export, incremental fetch and offline Backtrader replay.

python -m scripts.backtest.early_trim_offline_study --help
Only fetch uses the network. Export opens SQLite read-only; run uses frozen files.
"""
from __future__ import annotations

import argparse
from collections import defaultdict
from contextlib import closing
from datetime import datetime, timezone
import hashlib
import json
import math
from pathlib import Path
import re
import sqlite3
import time

from . import evolution_ledger, market_cache
from .backtrader_adapter import _backtrader
from .incremental_filter import weekly_statistics, z_threshold
from scripts.promotion import report as promotion_report

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-09-early-trim-offline-s1-prereg.md'
PREREG_SHA256 = '110305fbf2da9bf72a0bd1badc46f3cbf1cb04741543a232747e29c8aa4c7bba'
MINUTE = 60_000
FEE = .0005
SLIPPAGE = .0003
Z_K = 2.241  # Use the preregistered literal, including its rounding.
LEDGER = REPO / 'data/evolution/ledger.jsonl'
LIVE = 'd7ff3764_f6b845ba-f7b2-4b27-8501-fcf6af94f276_deepseek_1787982996'
ENSEMBLE_CANDIDATE = 'a2dc5705_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_89bba6d6-adab-4eed-ad8b-60cfb1cfac2a'
ENSEMBLE_CONTROL = 'a8eb1bdf_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_273a584a-0c4d-43cf-9ef9-758a7c1d5bea'
TRIM_CANDIDATE = 'ec830bb2_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_bde3f7ab-001a-4dc6-9cfc-1acbb09f287d'
TRIM_CONTROL = '895dc8b3_f6b845ba-f7b2-4b27-8501-fcf6af94f276_glm_82245a32-c08d-4678-b0bd-f5cbb1003b02'
# Full IDs verified against data/promotion/pairs.json, without opening a database.
MECHANISM = {
    ENSEMBLE_CANDIDATE: ('2026-10-17T07:57:18Z', 1.2),
    ENSEMBLE_CONTROL: ('2026-10-17T07:57:18Z', 1.2),
    TRIM_CANDIDATE: ('2026-10-22T01:02:38Z', .8),
    TRIM_CONTROL: ('2026-10-22T01:02:38Z', 1.2),
}
TRADERS = (LIVE, *MECHANISM)
FIELDS = ('id', 'trader_id', 'symbol', 'side', 'entry_time', 'exit_time',
          'entry_price', 'exit_price', 'initial_stop_loss', 'entry_quantity',
          'quantity', 'tp_trim_done', 'ai_managed')
NOTES = [
    '全部时间为 UTC 毫秒。下载区间包含出场分钟之后第 5 分钟的整根 K 线。',
    '入场价校验的分母为全部导出交易，缺少入场分钟计不匹配；仅分钟缺失超限的交易剔除。',
    '机制一致率在通过分钟覆盖校验的模拟盘交易上汇总；严格取入场时间晚于策略更新时间的记录。',
    'export 核对四个模拟盘的策略更新时间及档位；run 缺少 export_meta.json 时使用预注册固定值并披露。',
    'entry_quantity 必须为正，不用已减仓后的 quantity 替代；R=0 或其他无效交易字段使研究 invalid。',
    '净 R 包含开仓一次手续费及各次平仓手续费；滑点仅作用于减仓，粉尘全平同样适用。',
    '触及敏感性按预注册直接以档位价成交并判断粉尘，扣 5bp 手续费。',
    'MFE 使用触发窗口内完整收盘 K 线的有利极值，以零为下限。触发后走势使用其后的完整收盘价及实际出场价。',
    '奇数笔样本前半取 floor(n/2)，后半取其余。周聚类不足三周时 z 缺失，不能通过筛选。',
    'SE=0 时零均值的 z=0，正负均值的 z 为正负无穷；JSON 用 inf/-inf 字符串保存。',
    'strong 使用预注册字面阈值 2.241；z_threshold(2) 的未舍入值仅供核对。',
    '账本由 append_event 原子计算实际 K；有效研究仅在实际 K=2 时允许追加，invalid 不计 K。',
]
LIMITATIONS = [
    '残余仓位实际出场外生，LLM 看到减仓后可能改变决策，本模型未反映。',
    '样本包含已看过线上结果的减仓配对，预注册预查为 132 笔；全部样本预查为 454 笔。',
    '实盘 9 月 29 日至 10 月 22 日配置调整可能影响残余仓位的出场。',
    '约六周的周聚类数较少，SE 不稳定。分钟收盘不能还原标记价及风控轮询。',
    '不模拟强平、资金费及市场冲击。此次实现与合成测试不构成历史有效性证据。',
]


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def verify_prereg(path=None):
    raw = Path(PREREG if path is None else path).read_bytes()
    if sha256(raw) != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run/output')
    return raw


def timestamp_ms(value):
    """Integers mean UTC milliseconds; ISO/GORM naive strategy times mean UTC."""
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        if not math.isfinite(value) or int(value) != value:
            raise ValueError('时间必须为整数毫秒')
        return int(value)
    parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return int(parsed.timestamp() * 1000)


SINCE = timestamp_ms('2026-09-29T00:00:00Z')
UNTIL = timestamp_ms('2026-11-09T00:00:00Z')


def write_json(path, value):
    """Atomically replace output bytes; incremental callers retain old rows."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.tmp')
    temporary.write_text(json.dumps(evolution_ledger.json_safe(value), ensure_ascii=False,
                                    indent=2, allow_nan=False) + '\n', encoding='utf-8')
    temporary.replace(path)


def check_strategy_metadata(strategies):
    if not isinstance(strategies, dict):
        raise ValueError('策略元数据必须为对象')
    for trader, (updated, level) in MECHANISM.items():
        actual = strategies.get(trader, {})
        # The DB keeps microseconds; the prereg literal is whole seconds.
        if (not isinstance(actual, dict) or not isinstance(actual.get('updated_at'), int)
                or actual['updated_at'] // 1000 != timestamp_ms(updated) // 1000
                or actual.get('tp_trim_at_r') != level):
            raise ValueError(f'{trader} 策略更新时间或 tp_trim_at_r 与预注册不一致')


def export_trades(db, out):
    """One SQLite snapshot, mode=ro and query_only, with fully specified IDs."""
    verify_prereg()
    uri = Path(db).resolve().as_uri() + '?mode=ro'
    with closing(sqlite3.connect(uri, uri=True)) as connection:
        connection.row_factory = sqlite3.Row
        connection.execute('PRAGMA query_only=ON')
        connection.execute('BEGIN')
        filters = ("status='CLOSED' AND initial_stop_loss>0 AND exit_time>0 AND exit_price>0 "
                   f"AND entry_time>=? AND entry_time<? AND trader_id IN ({','.join('?' for _ in TRADERS)}) "
                   'AND (trader_id<>? OR ai_managed=1)')
        rows = connection.execute('SELECT ' + ','.join(FIELDS) + ' FROM trader_positions WHERE ' +
                                  filters + ' ORDER BY entry_time,id', (SINCE, UNTIL, *TRADERS, LIVE))
        trades = []
        invalid_exit = 0
        for row in rows:
            trade = dict(row)
            if (not math.isfinite(trade['exit_price']) or not math.isfinite(trade['exit_time'])
                    or trade['exit_time'] < trade['entry_time']):
                invalid_exit += 1
                continue
            trades.append(trade)
        strategies = {}
        for row in connection.execute(
                'SELECT t.id AS trader_id,s.id AS strategy_id,s.updated_at,s.config '
                'FROM traders t JOIN strategies s ON s.id=t.strategy_id '
                f"WHERE t.id IN ({','.join('?' for _ in TRADERS)})", TRADERS):
            config = json.loads(row['config'])
            strategies[row['trader_id']] = dict(
                strategy_id=row['strategy_id'], updated_at=timestamp_ms(row['updated_at']),
                tp_trim_at_r=config.get('risk_control', {}).get('tp_trim_at_r'),
                config_sha256=sha256(row['config'].encode()))
        if set(strategies) != set(TRADERS):
            raise ValueError('缺少交易员策略记录')
        check_strategy_metadata(strategies)
    raw = ''.join(json.dumps(row, ensure_ascii=False, allow_nan=False, separators=(',', ':')) + '\n'
                  for row in trades).encode()
    meta = dict(version=1, sha256=sha256(raw), n=len(trades),
                counts_by_trader={trader: sum(t['trader_id'] == trader for t in trades) for trader in TRADERS},
                selection=dict(sql=filters, since_ms=SINCE, until_ms=UNTIL, traders=list(TRADERS),
                               live_ai_managed_only=True, valid_exit='finite positive price, exit_time>=entry_time'),
                invalid_exit_excluded=invalid_exit, strategies=strategies,
                prereg_sha256=PREREG_SHA256)
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    (out / 'trades.jsonl').write_bytes(raw)
    write_json(out / 'export_meta.json', meta)
    return meta


def load_trades(path):
    raw = Path(path).read_bytes()
    trades = [json.loads(line) for line in raw.splitlines() if line.strip()]
    ids = [row['id'] for row in trades]
    if len(set(ids)) != len(ids):
        raise ValueError('重复交易 id')
    return sorted(trades, key=lambda t: (t['entry_time'], t['id'])), sha256(raw)


def kline_window(trade):
    entry = trade['entry_time'] // MINUTE * MINUTE
    exit_minute = trade['exit_time'] // MINUTE * MINUTE
    return entry - 5 * MINUTE, exit_minute + 6 * MINUTE  # exclusive end


def kline_filename(trade):
    if not re.fullmatch(r'[A-Z0-9]+', trade['symbol']):
        raise ValueError('symbol 必须为 Binance 大写字母数字代码')
    return f"{trade['symbol']}_{trade['entry_time'] // MINUTE * MINUTE}.json"


def validate_klines(rows):
    """Reject malformed, duplicate, nonascending or non-minute OHLC rows."""
    previous = -1
    if not isinstance(rows, list):
        raise ValueError('K 线必须为行数组')
    for row in rows:
        if not isinstance(row, list) or len(row) < 5:
            raise ValueError('K 线行必须含 openTimeMs,o,h,l,c')
        opened = timestamp_ms(row[0])
        o, h, l, c = map(float, row[1:5])
        if (opened <= previous or opened % MINUTE
                or not all(math.isfinite(p) and p > 0 for p in (o, h, l, c))
                or not l <= min(o, c) <= max(o, c) <= h):
            raise ValueError('K 线时间顺序、分钟对齐或 OHLC 无效')
        previous = opened


def file_metadata(path, rows):
    return dict(sha256=sha256(Path(path).read_bytes()), rows=len(rows),
                start_ms=rows[0][0] if rows else None, end_ms=rows[-1][0] + MINUTE if rows else None)


def fetch_klines(trades_path, klines_dir, sleep=.3, *, fetch=None, sleeper=time.sleep):
    """Merge missing minutes, keeping every cached row and unrelated manifest entry.

    market_cache.http_json paces individual pages at >=0.3s. The outer pause
    also separates per-file fetches; --sleep may increase this minimum.
    """
    if not math.isfinite(sleep) or sleep < .3:
        raise ValueError('--sleep 必须至少为 0.3 秒')
    fetch = market_cache.fetch_binance_klines if fetch is None else fetch
    trades, trades_sha = load_trades(trades_path)
    base = Path(klines_dir)
    base.mkdir(parents=True, exist_ok=True)
    manifest_path = base / 'manifest.json'
    manifest = json.loads(manifest_path.read_bytes()) if manifest_path.exists() else dict(version=1, files={})
    tasks = {}
    for trade in trades:
        name = kline_filename(trade)
        start, end = kline_window(trade)
        prior = tasks.get(name, (trade['symbol'], start, end))
        tasks[name] = (trade['symbol'], min(start, prior[1]), max(end, prior[2]))
    requested = False
    for name, (symbol, start, end) in sorted(tasks.items()):
        path = base / name
        rows = json.loads(path.read_bytes()) if path.exists() else []
        validate_klines(rows)
        cached = {row[0]: row for row in rows}
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
            rows = [cached[t] for t in sorted(cached)]
            write_json(path, rows)
        manifest['files'][name] = dict(file_metadata(path, rows),
                                     requested_start_ms=start, requested_end_ms=end,
                                     complete=not (set(range(start, end, MINUTE)) - cached.keys()))
        manifest['trades_sha256'] = trades_sha
        write_json(manifest_path, manifest)  # keep successful progress on interruption
    if not tasks:
        manifest['trades_sha256'] = trades_sha
        write_json(manifest_path, manifest)
    return manifest


def validate_trade(trade):
    if any(field not in trade for field in FIELDS):
        raise ValueError('交易字段缺失')
    if trade['trader_id'] not in TRADERS or (trade['trader_id'] == LIVE and trade['ai_managed'] != 1):
        raise ValueError('交易员或实盘 ai_managed 不符合筛选')
    entry, exit_time = timestamp_ms(trade['entry_time']), timestamp_ms(trade['exit_time'])
    if not SINCE <= entry < UNTIL or exit_time < entry:
        raise ValueError('入场区间或出场时间无效')
    for key in ('entry_price', 'exit_price', 'initial_stop_loss', 'entry_quantity'):
        if not math.isfinite(trade[key]) or trade[key] <= 0:
            raise ValueError(f'{key} 必须为有限正数')
    if trade['entry_price'] == trade['initial_stop_loss']:
        raise ValueError('初始 R 为零')
    if (not isinstance(trade['side'], str) or trade['side'].lower() not in ('long', 'short')
            or trade['tp_trim_done'] not in (0, 1)):
        raise ValueError('方向或 tp_trim_done 无效')


def validate_data(trades, klines_dir):
    base = Path(klines_dir)
    manifest_path = base / 'manifest.json'
    reasons, details, retained = [], [], []
    try:
        manifest = json.loads(manifest_path.read_bytes())
        if not isinstance(manifest, dict) or not isinstance(manifest.get('files'), dict):
            raise ValueError('manifest 必须包含 files 对象')
    except (OSError, ValueError) as exc:
        manifest = {'files': {}}
        reasons.append(f'manifest 缺失或无效 {exc}')
    for trade in trades:
        detail = dict(id=trade['id'], entry_price_match=False, excluded=True)
        try:
            validate_trade(trade)
            path = base / kline_filename(trade)
            detail['file'] = path.name
            if path.exists():
                raw = path.read_bytes()
                rows = json.loads(raw)
                validate_klines(rows)
                actual = file_metadata(path, rows)
                detail['sha256'] = actual['sha256']
                recorded = manifest.get('files', {}).get(path.name, {})
                if not isinstance(recorded, dict) or any(recorded.get(k) != v for k, v in actual.items()):
                    reasons.append(f'{path.name} manifest 哈希、行数或区间不一致')
            else:
                rows = []
            start = trade['entry_time'] // MINUTE * MINUTE
            stop = trade['exit_time'] // MINUTE * MINUTE
            by_time = {row[0]: row for row in rows}
            expected = (stop - start) // MINUTE + 1
            missing = sum(t not in by_time for t in range(start, stop + MINUTE, MINUTE))
            entry_bar = by_time.get(start)
            detail.update(expected_minutes=expected, missing_minutes=missing,
                          missing_fraction=missing / expected, excluded=missing / expected > .01,
                          entry_price_match=entry_bar is not None and
                          float(entry_bar[3]) * .998 <= trade['entry_price'] <= float(entry_bar[2]) * 1.002)
            if not detail['excluded']:
                retained.append((trade, rows))
        except (OSError, ValueError, TypeError, KeyError, OverflowError) as exc:
            detail['error'] = str(exc)
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
    return dict(valid=not reasons, n=n, excluded=excluded, exclusion_fraction=exclusion_fraction,
                entry_price_match_fraction=price_fraction, reasons=reasons, details=details), retained


def trigger_window(trade, rows):
    entry_minute = trade['entry_time'] // MINUTE * MINUTE
    return [row for row in rows if row[0] > entry_minute and row[0] + MINUTE <= trade['exit_time']]


def direction_risk(trade):
    return (1 if trade['side'].lower() == 'long' else -1), abs(trade['entry_price'] - trade['initial_stop_loss'])


def find_trigger(trade, rows, level, *, touch=False):
    if level is None:
        return None
    sign, risk = direction_risk(trade)
    entry = trade['entry_price']
    for row in trigger_window(trade, rows):
        observed = float(row[2 if sign > 0 else 3] if touch else row[4])
        if sign * (observed - entry) / risk >= level:
            price = entry + sign * level * risk if touch else float(row[4])
            return dict(time_ms=row[0] + MINUTE, price=price,
                        fraction=1. if trade['entry_quantity'] * price * (2 / 3) < 5 else 1 / 3)
    return None


def validate_mechanism(retained):
    groups, details = {}, []
    for trader in MECHANISM:
        groups[trader] = dict(tp=0, tn=0, fp=0, fn=0)
    for trade, rows in retained:
        setting = MECHANISM.get(trade['trader_id'])
        if setting is None or trade['entry_time'] <= timestamp_ms(setting[0]):
            continue
        predicted = find_trigger(trade, rows, setting[1]) is not None
        actual = bool(trade['tp_trim_done'])
        cell = ('tp' if actual else 'fp') if predicted else ('fn' if actual else 'tn')
        groups[trade['trader_id']][cell] += 1
        details.append(dict(id=trade['id'], trader_id=trade['trader_id'], predicted=predicted, actual=actual))
    matrix = {cell: sum(g[cell] for g in groups.values()) for cell in ('tp', 'tn', 'fp', 'fn')}
    n = sum(matrix.values())
    accuracy = (matrix['tp'] + matrix['tn']) / n if n else None
    return dict(valid=accuracy is not None and accuracy >= .85, n=n, accuracy=accuracy,
                confusion_matrix=matrix, by_trader=groups, details=details)


def arithmetic_reference(trade, rows, level, *, touch=False, slippage=SLIPPAGE):
    """Independent fee/PnL oracle only; production statistics use native broker."""
    sign, risk = direction_risk(trade)
    entry, exit_price = trade['entry_price'], trade['exit_price']
    x_r = (sign * (exit_price - entry) - FEE * exit_price) / risk
    trigger = find_trigger(trade, rows, level, touch=touch)
    net = x_r
    if trigger is not None:
        price = trigger['price'] if touch else trigger['price'] * (1 - sign * slippage)
        trim_r = (sign * (price - entry) - FEE * price) / risk
        net = trigger['fraction'] * trim_r + (1 - trigger['fraction']) * x_r
    return net - FEE * entry / risk


def simulate_arm(trade, rows, level, *, touch=False, slippage=SLIPPAGE):
    """Native Backtrader Market orders, normalized to one original whole unit.

    This is a replay of prescribed execution events, not an OHLC stop simulator.
    A point-price feed and cheat-on-close execute E, the slipped trim price,
    and X exactly. Slippage is embedded only in the trim event's feed price so
    native broker slippage cannot also alter recorded entry/final exit prices.
    A final flush tick lets the last COC order settle. Equal trim/exit times
    remain separate ordered events. All accounting and fees use BackBroker.
    """
    bt = _backtrader()
    sign, risk = direction_risk(trade)
    trigger = find_trigger(trade, rows, level, touch=touch)
    events = [dict(time_ms=trade['entry_time'], price=trade['entry_price'], kind='entry', fraction=1.)]
    if trigger is not None:
        price = trigger['price'] if touch else trigger['price'] * (1 - sign * slippage)
        events.append(dict(trigger, price=price, kind='trim'))
    if trigger is None or trigger['fraction'] < 1:
        events.append(dict(time_ms=trade['exit_time'], price=trade['exit_price'], kind='exit'))
    events.append(dict(time_ms=max(trade['exit_time'], events[-1]['time_ms']) + 1,
                       price=events[-1]['price'], kind='flush'))

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

    class Replay(bt.Strategy):
        def __init__(self):
            self.fills = []
            self.net_pnl = None

        def next(self):
            event = events[len(self.data) - 1]
            if event['kind'] == 'flush':
                return
            opening = event['kind'] == 'entry'
            size = event['fraction'] if opening or event['kind'] == 'trim' else abs(self.position.size)
            submit = self.buy if (sign > 0) == opening else self.sell
            order = submit(size=size, exectype=bt.Order.Market)
            order.addinfo(event=event)

        def notify_order(self, order):
            if order.status == bt.Order.Completed:
                event = order.info['event']
                # Backtrader divides size*price by size for its average fill.
                # Allow only that floating-point roundoff, never a price tick.
                if abs(order.executed.price - event['price']) > 4 * math.ulp(event['price']):
                    raise RuntimeError('Backtrader 未按指定价格成交')
                self.fills.append(dict(kind=event['kind'], time_ms=event['time_ms'],
                                       price=order.executed.price, size=order.executed.size,
                                       commission=order.executed.comm))
            elif order.status in (bt.Order.Margin, bt.Order.Rejected, bt.Order.Canceled, bt.Order.Expired):
                raise RuntimeError(f'Backtrader 订单失败 {order.getstatusname()}')

        def notify_trade(self, closed):
            if closed.isclosed:
                self.net_pnl = closed.pnlcomm

    cerebro = bt.Cerebro(stdstats=False)
    cerebro.adddata(EventFeed())
    cerebro.addstrategy(Replay)
    cerebro.broker.setcash(max(trade['entry_price'], trade['exit_price'], *(e['price'] for e in events)) * 10)
    cerebro.broker.setcommission(commission=FEE, stocklike=True)
    cerebro.broker.set_coc(True)
    replay = cerebro.run(runonce=False)[0]
    if replay.position.size != 0 or replay.net_pnl is None or len(replay.fills) != len(events) - 1:
        raise RuntimeError('Backtrader 未完整平仓')
    value = replay.net_pnl / risk
    if abs(value - arithmetic_reference(trade, rows, level, touch=touch, slippage=slippage)) > 1e-9:
        raise RuntimeError('Backtrader 与算术参考差超过 1e-9 R')
    return dict(r=value, trigger=trigger, fills=replay.fills)


def mean(values):
    values = list(values)
    return math.fsum(values) / len(values) if values else None


def z_score(average, se):
    if average is None or se is None:
        return None
    return average / se if se else (math.copysign(math.inf, average) if average else 0.)


def comparison(rows, key='delta'):
    ordered = sorted(rows, key=lambda row: (row['entry_time'], row['id']))
    samples = [(datetime.fromtimestamp(row['entry_time'] / 1000, timezone.utc), row[key]) for row in ordered]
    stats = weekly_statistics(samples)
    half = len(ordered) // 2
    return dict(stats, mean_delta=stats['avg_r'], z=z_score(stats['avg_r'], stats['se']),
                dev_delta=mean(row[key] for row in ordered[:half]),
                holdout_delta=mean(row[key] for row in ordered[half:]),
                half_sizes=[half, len(ordered) - half])


def decide_verdict(stats, valid=True):
    if not valid or stats['n'] < 100:
        return 'invalid'
    if all(stats[k] is not None and stats[k] > 0 for k in ('dev_delta', 'holdout_delta')):
        if stats['z'] is not None and stats['z'] >= Z_K:
            return 'strong'
        if stats['z'] is not None and stats['z'] >= 1:
            return 'screen_pass'
    return 'fail'


def price_path(trade, rows, trigger):
    if trigger is None:
        return None
    sign, risk = direction_risk(trade)
    relative = [sign * (float(row[4]) - trade['entry_price']) / risk - .8
                for row in trigger_window(trade, rows) if row[0] + MINUTE > trigger['time_ms']]
    return dict(n_closes=len(relative), min_relative_r=min(relative) if relative else None,
                max_relative_r=max(relative) if relative else None,
                last_relative_r=relative[-1] if relative else None,
                closes_below=sum(v < 0 for v in relative), closes_at_or_above=sum(v >= 0 for v in relative),
                exit_relative_r=sign * (trade['exit_price'] - trade['entry_price']) / risk - .8)


def describe(rows):
    daily = defaultdict(list)
    for row in rows:
        day = datetime.fromtimestamp(row['entry_time'] / 1000, timezone.utc).date().isoformat()
        daily[day].append(row['delta'])
    avg = mean(row['delta'] for row in rows)
    se = promotion_report.cluster_se([math.fsum((v - avg) / len(rows) for v in group) for group in daily.values()])
    groups = {'<0.8R': [], '[0.8,1.2)R': [], '>=1.2R': []}
    traders = defaultdict(list)
    paths = [row['post_trigger_path'] for row in rows if row['post_trigger_path'] is not None]
    for row in rows:
        label = '<0.8R' if row['mfe_r'] < .8 else '[0.8,1.2)R' if row['mfe_r'] < 1.2 else '>=1.2R'
        groups[label].append(row['delta'])
        traders[row['trader_id']].append(row['delta'])
    def summary(group):
        return dict(n=len(group), mean_delta=mean(group))
    return dict(daily=dict(n_days=len(daily), mean_delta=avg, se=se, z=z_score(avg, se)),
                mfe_groups={k: summary(v) for k, v in groups.items()},
                by_trader={k: summary(v) for k, v in traders.items()},
                post_trigger=dict(n=len(paths), mean_exit_relative_r=mean(p['exit_relative_r'] for p in paths),
                                  exits_below=sum(p['exit_relative_r'] < 0 for p in paths),
                                  exits_at_or_above=sum(p['exit_relative_r'] >= 0 for p in paths)),
                touch=comparison(rows, 'touch_delta'), slippage_10bp=comparison(rows, 'slippage_10bp_delta'),
                baseline_vs_no_trim=comparison(rows, 'baseline_vs_no_trim_delta'))


def study(trades_path, klines_dir):
    before = verify_prereg()
    trades, trades_sha = load_trades(trades_path)
    validation, retained = validate_data(trades, klines_dir)
    meta_path = Path(trades_path).with_name('export_meta.json')
    meta = None
    if meta_path.exists():
        try:
            meta = json.loads(meta_path.read_bytes())
            if not isinstance(meta, dict):
                raise ValueError('export_meta 必须为对象')
            if (meta['sha256'] != trades_sha or meta['n'] != len(trades)
                    or meta['prereg_sha256'] != PREREG_SHA256):
                raise ValueError('export_meta 与冻结交易或预注册不一致')
            check_strategy_metadata(meta['strategies'])
        except (ValueError, KeyError, TypeError) as exc:
            validation['valid'] = False
            validation['reasons'].append(str(exc))
    # Mechanism checks happen before any arm PnL or paired delta is computed.
    mechanism = validate_mechanism(retained)
    reasons = list(validation['reasons'])
    if not mechanism['valid']:
        reasons.append('机制一致率低于 85% 或没有可校验样本')
    if len(retained) < 100:
        reasons.append('有效交易不足 100')
    manifest_path = Path(klines_dir) / 'manifest.json'
    result = dict(version=1, engine='backtrader', prereg_sha256=PREREG_SHA256,
                  inputs=dict(trades_sha256=trades_sha,
                              manifest_sha256=sha256(manifest_path.read_bytes()) if manifest_path.exists() else None,
                              export_meta_sha256=sha256(meta_path.read_bytes()) if meta_path.exists() else None),
                  strategy_metadata_source='export_meta' if meta is not None else 'prereg_fallback',
                  strategies=meta.get('strategies') if isinstance(meta, dict) else {
                      trader: dict(updated_at=timestamp_ms(updated), tp_trim_at_r=level)
                      for trader, (updated, level) in MECHANISM.items()},
                  validation=validation, mechanism=mechanism, invalid_reasons=reasons,
                  n_total=len(trades), n=len(retained), verdict='invalid', statistics=None,
                  descriptions=None, trades=[], commission=FEE, trim_slippage=SLIPPAGE,
                  K=2, z_K=Z_K, z_K_unrounded=z_threshold(2), trials=1, notes=NOTES, limitations=LIMITATIONS)
    if not reasons:
        result['engine_version'] = _backtrader().__version__
        for trade, bars in retained:
            candidate = simulate_arm(trade, bars, .8)
            baseline = simulate_arm(trade, bars, 1.2)
            touch = {str(level): simulate_arm(trade, bars, level, touch=True) for level in (.8, 1.2)}
            slippery = {str(level): simulate_arm(trade, bars, level, slippage=.001) for level in (.8, 1.2)}
            plain = simulate_arm(trade, bars, None)
            sign, risk = direction_risk(trade)
            extremes = [sign * (float(row[2 if sign > 0 else 3]) - trade['entry_price']) / risk
                        for row in trigger_window(trade, bars)]
            result['trades'].append(dict(id=trade['id'], trader_id=trade['trader_id'], entry_time=trade['entry_time'],
                candidate=candidate, baseline=baseline, delta=candidate['r'] - baseline['r'],
                sensitivity_touch=touch, touch_delta=touch['0.8']['r'] - touch['1.2']['r'],
                sensitivity_10bp=slippery, slippage_10bp_delta=slippery['0.8']['r'] - slippery['1.2']['r'],
                no_trim=plain, baseline_vs_no_trim_delta=baseline['r'] - plain['r'],
                mfe_r=max([0., *extremes]), post_trigger_path=price_path(trade, bars, candidate['trigger'])))
        result['statistics'] = comparison(result['trades'])
        result['arm_means'] = {arm: mean(t[arm]['r'] for t in result['trades']) for arm in ('candidate', 'baseline', 'no_trim')}
        result['verdict'] = decide_verdict(result['statistics'])
        result['descriptions'] = describe(result['trades'])
    if verify_prereg() != before:
        raise ValueError('Preregistration changed during study; refusing output')
    return result


def report(result):
    lines = ['# 提前减仓 0.8R 离线复核 S1', '',
             f'判定 `{result["verdict"]}`。有效交易 {result["n"]} / {result["n_total"]}。', '',
             f'预注册 SHA256 `{PREREG_SHA256}`。track `crypto_llm`，类型 C，K=2，阈值 2.241。', '',
             f'机制元数据来源 `{result["strategy_metadata_source"]}`。']
    if result['invalid_reasons']:
        lines += ['', '## 无效原因', ''] + ['- ' + reason for reason in result['invalid_reasons']]
    for title, data in (('输入校验', result['validation']), ('机制校验与混淆矩阵', result['mechanism']),
                        ('两臂净 R 均值', result.get('arm_means')), ('主统计与时间两半', result['statistics']),
                        ('仅描述项', result['descriptions']), ('冻结输入', result['inputs'])):
        if data is not None:
            data = {k: v for k, v in data.items() if k != 'details'}
            lines += ['', f'## {title}', '', '```json',
                      json.dumps(evolution_ledger.json_safe(data), ensure_ascii=False, indent=2), '```']
    action = {'fail': '建议提前结束线上 early-trim-0p8r 配对，执行须经用户同意。',
              'screen_pass': '线上配对照预注册继续运行至期满。',
              'strong': '线上配对照预注册继续运行至期满。',
              'invalid': '校验未通过或样本不足，本次无法给出有效的增量筛选结论。'}[result['verdict']]
    lines += ['', '## 后续', '', action, '', '## 执行口径与补充约定', '']
    lines += ['- ' + note for note in NOTES]
    lines += ['', '## 预先承认的局限', ''] + ['- ' + note for note in LIMITATIONS]
    lines += ['', '逐笔两臂、成交明细、描述项和失败样本明细见 results.json。', '']
    return '\n'.join(lines)


def write_ledger(result, path):
    def build(k, previous):
        if result['verdict'] != 'invalid' and k != 2:
            raise ValueError(f'实际账本 K={k} 与预注册 K=2 不一致，拒绝追加')
        stats = result['statistics'] or {}
        return dict(track='crypto_llm', stage='S1', change_type='C', idea_id='early-trim-0p8r-offline',
                    baseline_version='tp_trim_at_r=1.2@' + PREREG_SHA256,
                    verdict=result['verdict'], delta=stats.get('mean_delta'), se=stats.get('se'),
                    z=stats.get('z'), dev_delta=stats.get('dev_delta'), holdout_delta=stats.get('holdout_delta'),
                    n=result['n'], preregistered_K=2, z_K=Z_K, prereg_sha256=PREREG_SHA256,
                    input_hashes=result['inputs'], trials=1)
    return evolution_ledger.append_event(path, 'crypto_llm', build)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    export = sub.add_parser('export')
    export.add_argument('--db', type=Path, required=True)
    export.add_argument('--out', type=Path, required=True)
    fetch = sub.add_parser('fetch')
    fetch.add_argument('--trades', type=Path, required=True)
    fetch.add_argument('--klines-dir', type=Path, default=REPO / 'data/llm_trim_offline/kl1m')
    fetch.add_argument('--sleep', type=float, default=.3)
    run = sub.add_parser('run')
    run.add_argument('--trades', type=Path, required=True)
    run.add_argument('--klines-dir', type=Path, required=True)
    run.add_argument('--out', type=Path, required=True)
    run.add_argument('--write-ledger', type=Path, nargs='?', const=LEDGER)
    args = parser.parse_args(argv)
    try:
        if args.command == 'export':
            if args.db.resolve() in ((args.out / name).resolve() for name in ('trades.jsonl', 'export_meta.json')):
                raise ValueError('导出文件不可覆盖数据库')
            meta = export_trades(args.db, args.out)
            print(f'导出 {meta["n"]} 笔，SHA256 {meta["sha256"]}')
        elif args.command == 'fetch':
            fetch_klines(args.trades, args.klines_dir, args.sleep)
        else:
            protected = [args.trades.resolve(), args.trades.with_name('export_meta.json').resolve(), PREREG.resolve()]
            destinations = [(args.out / name).resolve() for name in ('results.json', 'report.md')]
            if args.write_ledger is not None:
                destinations.append(args.write_ledger.resolve())
            if (any(dest in protected or dest.is_relative_to(args.klines_dir.resolve()) for dest in destinations)
                    or len(set(destinations)) != len(destinations)):
                raise ValueError('输出与账本不可覆盖冻结输入或彼此覆盖')
            result = study(args.trades, args.klines_dir)
            write_json(args.out / 'results.json', result)
            (args.out / 'report.md').write_text(report(result), encoding='utf-8')
            if args.write_ledger is not None:
                write_ledger(result, args.write_ledger)
            print(f'VERDICT {result["verdict"]}')
            return 1 if result['verdict'] == 'invalid' else 0
    except (ValueError, OSError, sqlite3.Error) as exc:
        parser.error(str(exc))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
