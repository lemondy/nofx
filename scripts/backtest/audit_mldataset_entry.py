"""ml-dataset 标签入场因果性审计(可复跑).

用法(需 venv, 仓库根执行):

    .venv/bin/python -m scripts.backtest.audit_mldataset_entry

背景: 913980f5 修了 quant_audit 的同 bar 前视(positions[i] 用第 i 根收盘决定第 i
根敞口)。ml-dataset 的标签天然看未来(它们是结局), 唯一需要审计的是**入场价**:
必须来自信号之后可成交的价格, 不允许用决策棒收盘直接当成交价。经验检验:

  - 市价信号: 成交价 == 次棒开盘 × (1 ± 滑点)(因果); == 决策棒收盘 即泄漏。
  - 限价信号: 挂价 == 决策时价 px0; 开盘跳空更有利时按开盘价成交(改善, 合法);
    其余小离群(触及棒区间内插值)按 bp 量级报告。

特征侧不在此脚本范围: mlfeatures 从 decision_records.input_prompt(t0 快照)提取,
构造上 point-in-time。"""
import json
import sys

from . import exitsim
from .backtrader_adapter import run_trade
from .klines import load_bars
from .params import FALLBACK

SIGNALS = "data/backtest_cache/signals.json"
CACHE = "data/backtest_cache"


def main() -> int:
    sigs = json.load(open(SIGNALS))["signals"]
    m = dict(mkt_checked=0, mkt_next_open=0, mkt_same_close=0,
             lim_checked=0, lim_at_px0=0, lim_gap_open=0)
    outliers, skipped = [], 0
    for sig in sigs:
        got = load_bars(CACHE, sig["symbol"], sig["t0_ms"])
        if got is None:
            continue
        bars, n1m = got
        kind = "mkt" if sig["kind"] == "market" else "lim"
        tr = exitsim.build_trade(sig, bars, kind, n1m)
        if tr is None or len(tr["bars"]) < 2:
            skipped += 1
            continue
        try:
            out = run_trade(tr, dict(FALLBACK), exitsim.TAKER, exitsim.SLIP)
        except Exception:
            skipped += 1
            continue
        ep = out.get("entry_price")
        if not ep:
            skipped += 1
            continue
        feed = tr["bars"]
        if kind == "mkt":
            close0, open1 = feed[0][4], feed[1][1]
            sign = 1 if sig["side"] == "long" else -1
            m["mkt_checked"] += 1
            if abs(ep - open1 * (1 + sign * exitsim.SLIP)) / open1 < 1e-6:
                m["mkt_next_open"] += 1
            elif abs(ep - close0) / close0 < 1e-6:
                m["mkt_same_close"] += 1
            else:
                outliers.append((sig["symbol"], sig["t0"][:16], "mkt", ep, close0, open1))
        else:
            px0, open1 = sig["px0"], feed[1][1]
            m["lim_checked"] += 1
            if abs(ep - px0) / px0 < 1e-6:
                m["lim_at_px0"] += 1
            elif (sig["side"] == "long" and open1 < px0) or (sig["side"] == "short" and open1 > px0):
                m["lim_gap_open"] += 1  # 开盘比挂价更有利: 按开盘成交(合法改善)
            else:
                outliers.append((sig["symbol"], sig["t0"][:16], "lim", ep, px0, open1))

    print(f"市价: 检验 {m['mkt_checked']}, 次棒开盘±滑点(因果)={m['mkt_next_open']}, "
          f"决策棒收盘(泄漏)={m['mkt_same_close']}")
    print(f"限价: 检验 {m['lim_checked']}, 决策时挂价={m['lim_at_px0']}, 有利跳空按开盘={m['lim_gap_open']}")
    print(f"跳过(无缓存/未成交/无效): {skipped}")
    for sym, t0, kind, ep, ref, open1 in outliers:
        bp = abs(ep - ref) / ref * 1e4
        print(f"  离群 {kind} {sym} {t0}: entry={ep:.6g} ref={ref:.6g} 次棒开盘={open1:.6g} ({bp:.1f}bp)")
    leak = m["mkt_same_close"] > 0
    print("结论: " + ("发现同棒泄漏 —— 标签不可用!" if leak else
                      "入场价全部因果(次棒开盘/决策时挂价/有利跳空改善), 标签无同棒前视"))
    return 1 if leak else 0


if __name__ == "__main__":
    sys.exit(main())
