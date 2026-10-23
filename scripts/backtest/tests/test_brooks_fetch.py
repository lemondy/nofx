"""brooks_fetch 单元测试:全部使用假 HTTP,不访问网络。"""

import json
import os
import tempfile
import unittest
from urllib.parse import parse_qs, urlparse

from scripts.backtest import brooks_fetch as bf

H = bf.HOUR_MS


def bar(i, value=1.0):
    t = i * H
    return [t, value, value + 2, value - 1, value + 1, 10.0 + value]


class FakeClient(bf.Client):
    """替换 _raw_get,其余限速/计数/重试逻辑保持真实。"""

    def __init__(self, handler, **kwargs):
        kwargs.setdefault("sleep", lambda _s: None)
        kwargs.setdefault("log", lambda *_a, **_k: None)
        super().__init__(**kwargs)
        self.handler = handler
        self.calls = []

    def _raw_get(self, url):
        self.calls.append(url)
        return self.handler(url), None


class KlineServer:
    def __init__(self, bars_by_symbol, failing=()):
        self.bars_by_symbol = bars_by_symbol
        self.failing = set(failing)

    def __call__(self, url):
        q = parse_qs(urlparse(url).query)
        symbol = q["symbol"][0]
        if symbol in self.failing:
            raise ConnectionError("simulated network failure")
        start = int(q["startTime"][0])
        limit = int(q.get("limit", [str(bf.LIMIT)])[0])
        return [b for b in self.bars_by_symbol[symbol] if b[0] >= start][:limit]


def starts(client):
    return [int(parse_qs(urlparse(u).query)["startTime"][0])
            for u in client.calls]


class FetchTests(unittest.TestCase):
    def test_pagination_starts_at_last_and_drops_unclosed_bar(self):
        # 0..3199 已收盘;3200 是当前尚未收盘的一根。
        now = 3200 * H + 30 * 60_000
        bars = [bar(i) for i in range(3201)]
        client = FakeClient(KlineServer({"BTCUSDT": bars}))
        with tempfile.TemporaryDirectory() as d:
            result = bf.fetch_symbol(client, "BTCUSDT", d, now, start_ms=0)
            self.assertEqual(result["bars"], 3200)
            self.assertEqual(result["pages"], 3)
            self.assertEqual(starts(client), [0, 1500 * H, 3000 * H])

            with open(os.path.join(d, "BTCUSDT.json")) as f:
                rows = json.load(f)
            self.assertEqual(len(rows), 3200)
            self.assertEqual(rows[0][0], 0)
            self.assertEqual(rows[-1][0], 3199 * H)
            self.assertLessEqual(rows[-1][0] + H, now)
            self.assertIsInstance(rows[0][0], int)
            self.assertTrue(all(isinstance(v, float) for v in rows[0][1:]))
            self.assertFalse([f for f in os.listdir(d) if f.endswith(".tmp")])

    def test_parse_rows_drops_only_unclosed(self):
        now = 10 * H + 1
        payload = [bar(i) for i in range(11)]
        rows = bf.parse_rows(payload, now)
        self.assertEqual([r[0] for r in rows], [i * H for i in range(10)])

    def test_incremental_merge_uses_three_bar_overlap_and_dedupes(self):
        # 已有 0..999(含一条重复),新数据 1000..1002,1003 未收盘。
        now = 1003 * H + 30 * 60_000
        initial = [bar(i, i) for i in range(1000)] + [bar(999, 999)]
        server = KlineServer({"ETHUSDT": [bar(i, i) for i in range(1003)]})
        client = FakeClient(server)
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "ETHUSDT.json")
            bf.save_rows(path, initial)
            result = bf.fetch_symbol(client, "ETHUSDT", d, now, start_ms=0)

            # 从最后一根(999)回退 2 根 → 997 开始,3 根重叠。
            self.assertEqual(starts(client), [997 * H])
            self.assertEqual(result["added"], 3)  # 1000,1001,1002
            self.assertEqual(result["bars"], 1003)

            with open(path) as f:
                rows = json.load(f)
            times = [r[0] for r in rows]
            self.assertEqual(len(times), len(set(times)))
            self.assertEqual(times, sorted(times))
            self.assertEqual(times[0], 0)
            self.assertEqual(times[-1], 1002 * H)
            # 重叠区间的值仍正确(没有被旧文件覆盖或打乱)。
            self.assertEqual(rows[997][1], 997.0)

    def test_failed_symbol_skipped_after_retries(self):
        now = 10 * H + 30 * 60_000
        data = {s: [bar(i) for i in range(10)]
                for s in ("AAAUSDT", "BBBUSDT", "CCCUSDT")}
        client = FakeClient(KlineServer(data, failing={"BBBUSDT"}), retries=4)
        with tempfile.TemporaryDirectory() as d:
            summary = bf.fetch_all(client, list(data), d, now, start_ms=0,
                                   log=lambda *_a, **_k: None)
            self.assertEqual([x["symbol"] for x in summary["success"]],
                             ["AAAUSDT", "CCCUSDT"])
            self.assertEqual([x["symbol"] for x in summary["failed"]],
                             ["BBBUSDT"])
            self.assertFalse(summary["aborted"])
            self.assertEqual(
                len([u for u in client.calls if "BBBUSDT" in u]), 5)
            self.assertTrue(os.path.exists(os.path.join(d, "AAAUSDT.json")))
            self.assertTrue(os.path.exists(os.path.join(d, "CCCUSDT.json")))
            self.assertFalse(os.path.exists(os.path.join(d, "BBBUSDT.json")))

    def test_request_cap_stops_and_keeps_completed_symbols(self):
        now = 10 * H + 30 * 60_000
        symbols = ["AAAUSDT", "BBBUSDT", "CCCUSDT"]
        data = {s: [bar(i) for i in range(10)] for s in symbols}
        client = FakeClient(KlineServer(data), max_requests=2)
        with tempfile.TemporaryDirectory() as d:
            summary = bf.fetch_all(client, symbols, d, now, start_ms=0,
                                   log=lambda *_a, **_k: None)
            self.assertEqual([x["symbol"] for x in summary["success"]],
                             ["AAAUSDT", "BBBUSDT"])
            self.assertEqual([x["symbol"] for x in summary["failed"]],
                             ["CCCUSDT"])
            self.assertTrue(summary["aborted"])
            self.assertEqual(summary["requests"], 2)
            self.assertEqual(client.requests, 2)
            self.assertTrue(os.path.exists(os.path.join(d, "AAAUSDT.json")))
            self.assertTrue(os.path.exists(os.path.join(d, "BBBUSDT.json")))
            self.assertFalse(os.path.exists(os.path.join(d, "CCCUSDT.json")))

    def test_estimate_pages_and_incremental_start(self):
        now = 1501 * H + 30 * 60_000
        self.assertEqual(bf.estimate_pages(0, now), 2)
        self.assertEqual(bf.estimate_pages(1500 * H, now), 1)
        self.assertEqual(bf.estimate_pages(1502 * H, now), 0)
        self.assertEqual(bf.fetch_start([], 0), 0)
        self.assertEqual(bf.fetch_start([bar(5)], 0), 3 * H)
        self.assertEqual(bf.fetch_start([bar(5)]), bf.START_MS)


class SelectSymbolsTests(unittest.TestCase):
    def test_filters_quotes_perps_and_leveraged_tokens(self):
        tickers = [
            {"symbol": "BTCUSDT", "quoteVolume": "100"},
            {"symbol": "ETHUSDT", "quoteVolume": "90"},
            {"symbol": "JUPUSDT", "quoteVolume": "80"},    # 正常币,不能因含 UP 被误删
            {"symbol": "PUMPUSDT", "quoteVolume": "70"},
            {"symbol": "BTCUPUSDT", "quoteVolume": "60"},  # 杠杆代币
            {"symbol": "BTCDOWNUSDT", "quoteVolume": "50"},
            {"symbol": "BTCBUSD", "quoteVolume": "40"},
            {"symbol": "ETHUSDC", "quoteVolume": "30"},
            {"symbol": "BTCUSDT_250627", "quoteVolume": "20"},
            {"symbol": "BTCUSDT_PERP", "quoteVolume": "10"},
            {"symbol": "ZEROUSDT", "quoteVolume": "0"},
        ]
        self.assertEqual(bf.select_symbols(tickers),
                         ["BTCUSDT", "ETHUSDT", "JUPUSDT", "PUMPUSDT"])
        self.assertEqual(bf.select_symbols(tickers, 2), ["BTCUSDT", "ETHUSDT"])


if __name__ == "__main__":
    unittest.main()
