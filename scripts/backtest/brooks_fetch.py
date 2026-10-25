"""拉取币安 U 本位 USDT 永续 1h K 线,供 Brooks 价格行为研究使用。

输出: <out>/{SYMBOL}.json,每行 [openTimeMs, open, high, low, close, volume]。
只保存已收盘 K 线;已有文件时从最后 3 根重叠处增量补齐并去重;写入采用临时文件 + rename。

网络访问复用 fetch.py 的做法:urllib 自动读取 HTTPS_PROXY,请求间隔至少 1 秒,
按 X-MBX-USED-WEIGHT-1M 退避,失败重试后跳过单个币种,并有总请求数硬上限。
"""
import argparse
import json
import math
import os
import tempfile
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from urllib.parse import parse_qs, urlparse

API = "https://fapi.binance.com"
KLINES_URL = API + "/fapi/v1/klines"
TICKER_URL = API + "/fapi/v1/ticker/24hr"

HOUR_MS = 3_600_000
INTERVAL = "1h"
LIMIT = 1500
START_MS = 1_640_995_200_000  # 2022-01-01T00:00:00Z
DEFAULT_OUT = "/private/tmp/claude-501/brooks/kl1h/"
TOP_N = 30
MAX_REQUESTS = 3000
OVERLAP_BARS = 3
WEIGHT_BACKOFF = 1000


class FetchError(RuntimeError):
    """单个请求在重试后仍失败。"""


class RequestLimitReached(RuntimeError):
    """总请求数达到硬上限。"""


class Client:
    """带限速、退避、重试与请求计数上限的 JSON 客户端。"""

    def __init__(self, max_requests=MAX_REQUESTS, min_interval=1.0, retries=4,
                 timeout=20, sleep=time.sleep, log=print):
        self.max_requests = max_requests
        self.min_interval = min_interval
        self.retries = retries
        self.timeout = timeout
        self.sleep = sleep
        self.log = log
        self.requests = 0
        self._last_request = 0.0

    def _raw_get(self, url):
        """返回 (解析后的 JSON, X-MBX-USED-WEIGHT-1M)。测试中可替换此方法。"""
        with urllib.request.urlopen(url, timeout=self.timeout) as r:
            weight = r.headers.get("X-MBX-USED-WEIGHT-1M")
            return json.load(r), weight

    def _wait_for_slot(self):
        if self._last_request:
            delay = self.min_interval - (time.monotonic() - self._last_request)
            if delay > 0:
                self.sleep(delay)

    def get(self, url, label=""):
        last_err = None
        for attempt in range(self.retries + 1):
            if self.requests >= self.max_requests:
                raise RequestLimitReached(
                    f"已达到总请求上限 {self.max_requests}"
                )
            self._wait_for_slot()
            self.requests += 1
            try:
                data, weight = self._raw_get(url)
            except urllib.error.HTTPError as e:
                self._last_request = time.monotonic()
                last_err = e
                if e.code in (400, 404):
                    raise FetchError(f"{label}: HTTP {e.code}") from e
                self.log(f"  err {label or url}: HTTP {e.code}")
                if e.code in (418, 429):
                    self.sleep(60)
            except Exception as e:  # noqa: BLE001 网络错误统一重试
                self._last_request = time.monotonic()
                last_err = e
                self.log(f"  err {label or url}: {e}")
            else:
                self._last_request = time.monotonic()
                try:
                    used = int(weight) if weight else 0
                except (TypeError, ValueError):
                    used = 0
                if used > WEIGHT_BACKOFF:
                    self.log(f"  weight {used} > {WEIGHT_BACKOFF}, 退避 30s")
                    self.sleep(30)
                return data
            if attempt < self.retries:
                backoff = min(5 * (2 ** attempt), 60)
                self.log(f"  retry {label or url} in {backoff}s "
                         f"({attempt + 1}/{self.retries})")
                self.sleep(backoff)
        raise FetchError(f"{label or url}: 重试 {self.retries} 次后失败 ({last_err})")


def klines_url(symbol, start_ms, limit=None):
    limit = LIMIT if limit is None else limit
    return (f"{KLINES_URL}?symbol={symbol}&interval={INTERVAL}"
            f"&startTime={int(start_ms)}&limit={int(limit)}")


def _is_leveraged(symbol, universe):
    """识别 BTCUPUSDT / BTCDOWNUSDT 这类杠杆代币。

    不能简单用 "UP" in symbol:JUP/PUMP/SUPER 等正常币种也会被误伤。
    杠杆代币的底层资产通常也有对应的永续合约,因此用“去掉 UP/DOWN 后底层存在”判断。
    """
    base = symbol[:-4] if symbol.endswith("USDT") else symbol
    for suffix in ("UP", "DOWN"):
        if base.endswith(suffix):
            underlying = base[:-len(suffix)]
            if underlying and f"{underlying}USDT" in universe:
                return True
    return False


def select_symbols(tickers, top_n=TOP_N):
    """按 24h quoteVolume 选出前 top_n 个 USDT 永续合约。"""
    valid = []
    for t in tickers:
        symbol = str(t.get("symbol", ""))
        if not symbol.endswith("USDT") or len(symbol) <= 4:
            continue
        # 排除 BUSD/USDC 等非 USDT 计价、交割合约(_PERP/日期后缀)及异常名称。
        if "_" in symbol or not symbol.isalnum() or not symbol.isupper():
            continue
        try:
            quote_volume = float(t.get("quoteVolume") or 0)
        except (TypeError, ValueError):
            continue
        if quote_volume <= 0:
            continue
        valid.append((symbol, quote_volume))

    universe = {s for s, _ in valid}
    valid = [(s, q) for s, q in valid if not _is_leveraged(s, universe)]
    valid.sort(key=lambda x: (-x[1], x[0]))
    return [s for s, _ in valid[:top_n]]


def last_closed_open(now_ms):
    """当前时刻最后一根已收盘 1h K 线的 openTime。"""
    return ((int(now_ms) - HOUR_MS) // HOUR_MS) * HOUR_MS


def parse_rows(payload, now_ms):
    """把原始 K 线转为数值行,并丢弃尚未收盘的最后一根。"""
    rows = []
    for k in payload:
        open_ms = int(k[0])
        if open_ms + HOUR_MS > now_ms:
            continue
        rows.append([open_ms, float(k[1]), float(k[2]), float(k[3]),
                     float(k[4]), float(k[5])])
    return rows


def load_rows(path):
    try:
        with open(path) as f:
            data = json.load(f)
    except (OSError, ValueError):
        return []
    rows = []
    for r in data:
        if isinstance(r, list) and len(r) >= 6:
            try:
                rows.append([int(r[0]), float(r[1]), float(r[2]),
                             float(r[3]), float(r[4]), float(r[5])])
            except (TypeError, ValueError):
                continue
    rows.sort(key=lambda x: x[0])
    return rows


def save_rows(path, rows):
    """原子写入:同目录临时文件写完后 os.replace。"""
    directory = os.path.dirname(path) or "."
    os.makedirs(directory, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=os.path.basename(path) + ".",
                               suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(rows, f, separators=(",", ":"))
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def fetch_start(existing, start_ms=START_MS):
    """增量起点:已有文件则回退 2 小时,形成 3 根重叠。"""
    if not existing:
        return start_ms
    return max(start_ms, existing[-1][0] - (OVERLAP_BARS - 1) * HOUR_MS)


def estimate_pages(start_ms, now_ms):
    last = last_closed_open(now_ms)
    if start_ms > last:
        return 0
    bars = (last - start_ms) // HOUR_MS + 1
    return max(1, math.ceil(bars / LIMIT))


def fetch_symbol(client, symbol, out_dir, now_ms, start_ms=START_MS, log=print):
    """拉取并合并单个币种,返回统计信息。失败抛 FetchError。"""
    path = os.path.join(out_dir, f"{symbol}.json")
    existing = load_rows(path)
    cursor = fetch_start(existing, start_ms)

    rows = {r[0]: r for r in existing if r[0] + HOUR_MS <= now_ms}
    added = 0
    pages = 0

    while cursor + HOUR_MS <= now_ms:
        payload = client.get(klines_url(symbol, cursor),
                             label=f"{symbol}@{cursor}")
        pages += 1
        if not isinstance(payload, list):
            raise FetchError(f"{symbol}: 非预期的 K 线响应")

        batch = parse_rows(payload, now_ms)
        for r in batch:
            if r[0] not in rows:
                added += 1
            rows[r[0]] = r

        if len(payload) < LIMIT:
            break
        next_cursor = int(payload[-1][0]) + HOUR_MS
        if next_cursor <= cursor:
            raise FetchError(f"{symbol}: 分页游标未前进")
        cursor = next_cursor

    out = [rows[t] for t in sorted(rows) if t + HOUR_MS <= now_ms]
    if not out:
        raise FetchError(f"{symbol}: 没有已收盘 K 线")
    # 已有内容未变化时也重写一次,保证格式统一;文件很小,代价可忽略。
    save_rows(path, out)
    return {
        "symbol": symbol,
        "bars": len(out),
        "added": added,
        "pages": pages,
        "first": out[0][0],
        "last": out[-1][0],
    }


def fetch_all(client, symbols, out_dir, now_ms, start_ms=START_MS, log=print):
    success, failed = [], []
    aborted = False
    for i, symbol in enumerate(symbols, 1):
        before = client.requests
        try:
            result = fetch_symbol(client, symbol, out_dir, now_ms,
                                  start_ms=start_ms, log=log)
        except RequestLimitReached as e:
            failed.append({"symbol": symbol, "error": str(e)})
            log(f"  {e},停止;已完成 {len(success)} 个币")
            aborted = True
            break
        except Exception as e:  # noqa: BLE001 单币失败不终止整体
            failed.append({"symbol": symbol, "error": str(e)})
            log(f"  skip {symbol}: {e}")
            continue
        success.append(result)
        log(f"  [{i}/{len(symbols)}] {symbol}: {result['bars']} 根 "
            f"(+{result['added']}), {client.requests - before} 请求")
    return {
        "success": success,
        "failed": failed,
        "aborted": aborted,
        "requests": client.requests,
    }


def _fmt_ms(ms):
    return datetime.fromtimestamp(ms / 1000, tz=timezone.utc).strftime(
        "%Y-%m-%d %H:%M UTC")


def print_summary(summary, log=print):
    success, failed = summary["success"], summary["failed"]
    total_bars = sum(x["bars"] for x in success)
    log("")
    log(f"成功 {len(success)} 个币,失败 {len(failed)} 个,"
        f"共 {total_bars} 根 K 线,请求 {summary['requests']} 次")
    for x in success:
        log(f"  {x['symbol']:<14} {x['bars']:>6} 根  "
            f"{_fmt_ms(x['first'])} ~ {_fmt_ms(x['last'])}")
    if failed:
        log("失败:")
        for x in failed:
            log(f"  {x['symbol']}: {x['error']}")
    if summary["aborted"]:
        log("注意:触发请求上限,后续币种未拉取。")


def build_parser():
    p = argparse.ArgumentParser(
        description="拉取币安 USDT 永续 1h K 线 (2022-01-01 起,仅已收盘)")
    p.add_argument("--out", default=DEFAULT_OUT, help="输出目录")
    p.add_argument("--dry-run", action="store_true",
                   help="只打印币种和预计请求数,不拉取 K 线")
    p.add_argument("--top", type=int, default=TOP_N,
                   help=f"按 24h 成交额选取的币种数 (默认 {TOP_N})")
    p.add_argument("--max-requests", type=int, default=MAX_REQUESTS,
                   help=f"总请求数硬上限 (默认 {MAX_REQUESTS})")
    p.add_argument("--retries", type=int, default=4,
                   help="单个请求失败后的重试次数 (默认 4)")
    p.add_argument("--sleep", type=float, default=1.0,
                   help="请求间隔秒数,不得小于 1 (默认 1.0)")
    return p


def main(argv=None):
    args = build_parser().parse_args(argv)
    sleep = max(1.0, args.sleep)
    client = Client(max_requests=args.max_requests, min_interval=sleep,
                    retries=args.retries)
    try:
        tickers = client.get(TICKER_URL, label="ticker/24hr")
    except (FetchError, RequestLimitReached) as e:
        print(f"获取 24h 行情失败: {e}")
        return 1
    if not isinstance(tickers, list):
        print("24h 行情响应格式异常")
        return 1

    symbols = select_symbols(tickers, args.top)
    if not symbols:
        print("没有找到符合条件的 USDT 永续合约")
        return 1

    now_ms = int(time.time() * 1000)
    plans = []
    for symbol in symbols:
        existing = load_rows(os.path.join(args.out, f"{symbol}.json"))
        start = fetch_start(existing, START_MS)
        plans.append((symbol, start, estimate_pages(start, now_ms)))

    if args.dry_run:
        print(f"将拉取 {len(symbols)} 个币 (24h 成交额前 {args.top}):")
        for symbol, start, pages in plans:
            print(f"  {symbol:<14} 从 {_fmt_ms(start)} 起,预计 {pages} 请求")
        total = 1 + sum(p for _, _, p in plans)
        print(f"预计总请求 {total} 次 (含行情 1 次),上限 {args.max_requests}")
        return 0

    print(f"开始拉取 {len(symbols)} 个币,输出 {args.out}")
    summary = fetch_all(client, symbols, args.out, now_ms, log=print)
    print_summary(summary)
    return 0 if not summary["failed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
