"""为每个信号缓存币安 U 本位合约 K 线(1m 1500 根 + 补满到 5m 边界的少量 1m + 之后的 5m 1000 根)。urllib 自动遵循环境变量 HTTPS_PROXY。"""
import json
import os
import time
import urllib.error
import urllib.request

from .klines import FIVE_M_LIMIT, MIN_MS, ONE_M_LIMIT, cache_paths, ceil_5m, floor_min

URL = "https://fapi.binance.com/fapi/v1/klines"
WEIGHT_BACKOFF = 1000  # X-MBX-USED-WEIGHT-1M 超过则退避 30s


def _is_complete(path, limit, interval_ms):
    """缓存是否完整。不足 limit 根时:若文件写入时刻紧贴最后一根 K 线(当时数据还没出完)→ 视为不完整需补抓;
    否则(如已下架币种,数据本来就短)视为完整。"""
    try:
        with open(path) as f:
            rows = json.load(f)
        mtime = os.path.getmtime(path)
    except (OSError, ValueError):
        return False
    if len(rows) >= limit:
        return True
    if not rows:
        return False
    return mtime * 1000 - rows[-1][0] > 3 * interval_ms


class Fetcher:
    def __init__(self, sleep=1.0, retries=3, log=print):
        self.sleep, self.retries, self.log = sleep, retries, log
        self.requests = 0
        self.failed = []

    def _get(self, symbol, interval, start, limit):
        url = f"{URL}?symbol={symbol}&interval={interval}&startTime={start}&limit={limit}"
        for attempt in range(self.retries):
            try:
                with urllib.request.urlopen(url, timeout=20) as r:
                    weight = r.headers.get("X-MBX-USED-WEIGHT-1M")
                    data = json.load(r)
                self.requests += 1
                time.sleep(self.sleep)
                if weight and int(weight) > WEIGHT_BACKOFF:
                    self.log(f"  weight {weight} > {WEIGHT_BACKOFF}, 退避 30s")
                    time.sleep(30)
                return [[k[0], float(k[1]), float(k[2]), float(k[3]), float(k[4])] for k in data]
            except urllib.error.HTTPError as e:
                if e.code == 400:  # 币种不存在/参数错误,重试无意义
                    self.log(f"  err {symbol} {interval}: HTTP 400")
                    return None
                self.log(f"  err {symbol} {interval}: {e}")
            except Exception as e:  # noqa: BLE001 网络类错误统一重试
                self.log(f"  err {symbol} {interval}: {e}")
            time.sleep(5)
        return None

    def _save(self, path, rows):
        os.makedirs(os.path.dirname(path), exist_ok=True)
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(rows, f)
        os.replace(tmp, path)

    def _seal_1m(self, symbol, b1, start):
        """A7：最后一根 1m 的收盘时刻不足 5m 起点时，补抓缺口 1m，保证 1m→5m 拼接无时间空洞。

        补抓失败(网络/已下架)时原样返回，之后 load_bars 的空洞检查会拦截该缓存。
        """
        gap = b1[-1][0] + MIN_MS
        missing = (start - gap) // MIN_MS
        if missing <= 0:
            return b1
        rows = self._get(symbol, "1m", gap, missing)
        if not rows:
            return b1
        have = {b[0] for b in b1}
        extra = [b for b in rows if b[0] not in have and gap <= b[0] < start]
        return b1 + extra if extra else b1

    def fetch_signal(self, cache, symbol, t0_ms, dry_run=False):
        """返回 (n_1m_fetched, n_5m_fetched)。dry_run 只统计需要补抓的文件数。"""
        t0 = floor_min(t0_ms)
        p1, p5 = cache_paths(cache, symbol, t0)
        f1 = f5 = 0
        have1 = _is_complete(p1, ONE_M_LIMIT, MIN_MS)
        if not have1:
            f1 = 1
            if not dry_run:
                rows = self._get(symbol, "1m", t0, ONE_M_LIMIT)
                if rows is None:
                    self.failed.append((symbol, t0, "1m"))
                    return 0, 0
                self._save(p1, rows)
        need5 = f1 or not _is_complete(p5, FIVE_M_LIMIT, 5 * MIN_MS)  # 1m 刷新后 5m 起点变了,需重抓
        if need5 and (dry_run or os.path.exists(p1)):
            f5 = 1
        if not dry_run and os.path.exists(p1):
            with open(p1) as f:
                b1 = json.load(f)
            if need5 and not b1:
                return f1, 0
            if b1:
                # A7：5m 起点取最后一根 1m 的收盘时刻(open+1m)向上取整到 5m 边界，
                # 并用 1m 补满到该边界，保证拼接后无时间空洞。
                start = ceil_5m(b1[-1][0] + MIN_MS)
                sealed = self._seal_1m(symbol, b1, start)
                if len(sealed) > len(b1):
                    self._save(p1, sealed)
                if need5:
                    rows = self._get(symbol, "5m", start, FIVE_M_LIMIT)
                    if rows is None:
                        self.failed.append((symbol, t0, "5m"))
                        return f1, 0
                    self._save(p5, rows)
        return f1, f5


def fetch_all(signals, cache, sleep=1.0, dry_run=False, log=print):
    fx = Fetcher(sleep=sleep, log=log)
    seen, n1, n5 = set(), 0, 0
    for i, s in enumerate(signals):
        key = (s["symbol"], floor_min(s["t0_ms"]))
        if key in seen:
            continue
        seen.add(key)
        a, b = fx.fetch_signal(cache, s["symbol"], s["t0_ms"], dry_run=dry_run)
        n1, n5 = n1 + a, n5 + b
        if (a or b) and not dry_run and (n1 + n5) % 25 == 0:
            log(f"  [{i + 1}/{len(signals)}] fetched 1m={n1} 5m={n5}")
    return dict(signals=len(seen), fetched_1m=n1, fetched_5m=n5, failed=len(fx.failed),
                requests=fx.requests, failed_list=fx.failed, dry_run=dry_run)
