"""量化审计规则 — 从逆向工程方法论文章提炼的可执行版本.

来源: "How Quants Reverse Engineer Any Trading System With AI"
(https://x.com/RitOnchain/status/2110336556221481297). 文章的量化内核:

  1. 三态裁决: 系统必须在与基准的对比中同时赢 CAGR 和 Sharpe,
     且训练窗和保留窗都要赢, 才算 DATA_VALIDATED; 只赢其一 = TRADE_OFF;
     都不赢 = NO_EDGE. 切分只能按时间切, 禁止随机打乱 (时序打乱会把
     未来泄漏进过去).
  2. 200 日趋势规则 (文中给出的基准系统): 收盘 ≥ SMA200×1.01 进,
     收盘 ≤ SMA200×0.97 出, 空仓期吃无风险收益.
  3. 扫参平台判定: 参数在默认值 0.5x~1.5x 邻域内平移, 默认值赚钱
     但邻域大面积亏钱 = 孤峰 (lone peak) = 过拟合红旗.
  4. 红旗检查 (可自动化的子集): 前视 (信号滞后一根就失效),
     马丁格尔 (亏损后加仓), 单一 regime (利润集中在一两年).

纯标准库, 无第三方依赖. 演示:

    python3 -m scripts.backtest.quant_audit          # BTC 日线 vs 买入持有
    python3 -m scripts.backtest.quant_audit --symbol ETHUSDT

数据来自本机 nofx API (/api/klines); 后端不在时自动退化为合成随机游走
并明确标注. 所有收益率为对数收益; 成本按换仓次数的 round-trip 费率扣除.
"""

from __future__ import annotations

import argparse
import json
import math
import statistics
import urllib.request
from dataclasses import dataclass, field

# ---------------------------------------------------------------------------
# 收益统计
# ---------------------------------------------------------------------------


def log_returns(prices: list[float]) -> list[float]:
    return [math.log(prices[i] / prices[i - 1]) for i in range(1, len(prices)) if prices[i - 1] > 0]


def cagr(rets: list[float], periods_per_year: int) -> float:
    """对数收益序列的年化复合增长率."""
    if not rets:
        return 0.0
    total = math.exp(sum(rets)) - 1.0
    years = len(rets) / periods_per_year
    if years <= 0 or total <= -1.0:
        return 0.0
    return (1.0 + total) ** (1.0 / years) - 1.0


def sharpe(rets: list[float], periods_per_year: int, rf_per_period: float = 0.0) -> float:
    if len(rets) < 2:
        return 0.0
    excess = [r - rf_per_period for r in rets]
    sd = statistics.stdev(excess)
    if sd == 0:
        return 0.0
    return (statistics.mean(excess) / sd) * math.sqrt(periods_per_year)


def max_drawdown(rets: list[float]) -> float:
    """最大回撤 (正数, 0.35 = -35%)."""
    equity, peak, worst = 1.0, 1.0, 0.0
    for r in rets:
        equity *= math.exp(r)
        peak = max(peak, equity)
        worst = max(worst, 1.0 - equity / peak)
    return worst


# ---------------------------------------------------------------------------
# 1. 三态裁决 (时间切分, 永不随机)
# ---------------------------------------------------------------------------

VERDICT_VALIDATED = "DATA_VALIDATED"
VERDICT_TRADEOFF = "TRADE_OFF"
VERDICT_NO_EDGE = "NO_EDGE"


@dataclass
class SplitVerdict:
    verdict: str
    detail: dict = field(default_factory=dict)

    def __str__(self) -> str:  # 演示输出用
        d = self.detail
        rows = []
        for win in ("train", "holdout"):
            for m in ("cagr", "sharpe"):
                k = f"{win}_{m}"
                rows.append(f"{k}={d[k]:+.3f} (bench {d[f'{k}_bench']:+.3f})")
        return f"{self.verdict:14s} " + " ".join(rows)


def time_split(rets: list[float], train_frac: float = 0.6) -> tuple[list[float], list[float]]:
    """唯一允许的切分方式: 按时间, 前段训练后段保留."""
    cut = int(len(rets) * train_frac)
    return rets[:cut], rets[cut:]


def verdict(
    sys_rets: list[float],
    bench_rets: list[float],
    periods_per_year: int,
    train_frac: float = 0.6,
) -> SplitVerdict:
    """QuantGPT 裁决: 训练窗与保留窗都要在 CAGR 与 Sharpe 上同时胜过基准."""
    s_tr, s_ho = time_split(sys_rets, train_frac)
    b_tr, b_ho = time_split(bench_rets, train_frac)
    d: dict = {}
    wins = []
    for name, s, b in (("train", s_tr, b_tr), ("holdout", s_ho, b_ho)):
        d[f"{name}_cagr"] = cagr(s, periods_per_year)
        d[f"{name}_cagr_bench"] = cagr(b, periods_per_year)
        d[f"{name}_sharpe"] = sharpe(s, periods_per_year)
        d[f"{name}_sharpe_bench"] = sharpe(b, periods_per_year)
        wins.append(d[f"{name}_cagr"] > d[f"{name}_cagr_bench"])
        wins.append(d[f"{name}_sharpe"] > d[f"{name}_sharpe_bench"])
    if all(wins):
        v = VERDICT_VALIDATED
    elif any(wins):
        v = VERDICT_TRADEOFF
    else:
        v = VERDICT_NO_EDGE
    return SplitVerdict(v, d)


# ---------------------------------------------------------------------------
# 2. 200 日滞回趋势规则 (文中基准系统)
# ---------------------------------------------------------------------------


def trend_rule_positions(
    closes: list[float],
    ma_window: int = 200,
    enter_above: float = 0.01,
    exit_below: float = -0.03,
) -> list[int]:
    """返回每根 bar 收盘时点的目标敞口 (0/1), 成本与无风险收益在 apply 中处理."""
    pos = [0] * len(closes)
    exposure = 0
    for i in range(len(closes)):
        if i >= ma_window - 1:
            ma = sum(closes[i - ma_window + 1 : i + 1]) / ma_window
            c = closes[i]
            if exposure == 0 and c >= ma * (1.0 + enter_above):
                exposure = 1
            elif exposure == 1 and c <= ma * (1.0 + exit_below):
                exposure = 0
        pos[i] = exposure
    return pos


def apply_positions(
    closes: list[float],
    positions: list[int],
    rf_annual: float = 0.04,
    periods_per_year: int = 365,
    cost_rt: float = 0.001,
) -> list[float]:
    """按持仓序列合成净收益: 持有期吃价格收益, 空仓期吃 rf, 每次换向扣 half×cost_rt."""
    rets: list[float] = []
    rf = rf_annual / periods_per_year
    for i in range(1, len(closes)):
        r = math.log(closes[i] / closes[i - 1]) if closes[i - 1] > 0 else 0.0
        exp = positions[i]
        gross = r if exp == 1 else rf
        if positions[i] != positions[i - 1]:
            gross -= cost_rt / 2.0
        rets.append(gross)
    return rets


# ---------------------------------------------------------------------------
# 3. 扫参平台 vs 孤峰
# ---------------------------------------------------------------------------


@dataclass
class SweepResult:
    grid: dict  # {(ma, enter, exit): sharpe}
    default_key: tuple
    plateau_ratio: float
    lone_peak: bool

    def __str__(self) -> str:
        return (
            f"default {self.default_key} sharpe={self.grid[self.default_key]:+.2f}, "
            f"邻域通过率 {self.plateau_ratio:.0%} → "
            + ("LONE PEAK (过拟合红旗)" if self.lone_peak else "PLATEAU (稳健)")
        )


def sweep_trend_rule(
    closes: list[float],
    periods_per_year: int,
    defaults: tuple = (200, 0.01, -0.03),
    steps: int = 5,
) -> SweepResult:
    """默认参数 0.5x~1.5x 等距五档三围网格; 判定孤峰.

    孤峰定义: 默认值 Sharpe > 0 且 邻域(全部网格点)中 Sharpe > 0 的比例 < 0.5.
    默认值本身就是岭上一点时, 周围一半以上的点也该赚钱 — 否则利润是噪声.
    """
    ma0, en0, ex0 = defaults
    mas = _grid(ma0, steps, cast=int)
    ens = _grid(en0, steps)
    exs = _grid(ex0, steps)  # 负数乘系数仍保持符号

    grid: dict = {}
    for ma in mas:
        for en in ens:
            for ex in exs:
                if en <= 0 or ex >= 0:
                    continue
                pos = trend_rule_positions(closes, ma, en, ex)
                rets = apply_positions(closes, pos)
                grid[(ma, round(en, 4), round(ex, 4))] = sharpe(rets, periods_per_year)

    default_key = (ma0, round(en0, 4), round(ex0, 4))
    passing = sum(1 for v in grid.values() if v > 0)
    ratio = passing / len(grid) if grid else 0.0
    lone = grid.get(default_key, 0.0) > 0 and ratio < 0.5
    return SweepResult(grid, default_key, ratio, lone)


def _grid(center: float, steps: int = 5, cast=float):
    span = [0.5, 0.75, 1.0, 1.25, 1.5][:steps]
    return sorted({cast(round(center * s, 6)) for s in span})


# ---------------------------------------------------------------------------
# 4. 红旗检查 (可自动化子集)
# ---------------------------------------------------------------------------


def flag_lookahead(positions: list[int], closes: list[float], min_retention: float = 0.2) -> bool:
    """信号整体滞后一根 bar 后夏普坍塌 → 前视嫌疑.

    真实的动量/趋势信号滞后一根只是衰减 (日频趋势规则通常保留 >30% 夏普);
    用当根收盘决定当根敞口的信号滞后一根后夏普保留率通常 <5%.
    判据: base > 0 且 lag1 夏普 < base × min_retention.
    """
    base = sharpe(apply_positions(closes, positions), 365)
    lagged = [0] + positions[:-1]
    after = sharpe(apply_positions(closes, lagged), 365)
    return base > 0 and after < base * min_retention


def flag_martingale(sizes: list[float], rets: list[float]) -> bool:
    """仓位与连续亏损深度正相关 (Spearman 简化版) → 马丁格尔."""
    if len(sizes) != len(rets) or len(sizes) < 10:
        return False
    streak = 0
    streaks = []
    for r in rets[:-1]:
        streak = streak + 1 if r < 0 else 0
        streaks.append(float(streak))
    rho = _spearman(streaks, sizes[1:])
    return rho > 0.5


def flag_one_regime(rets: list[float], periods_per_year: int) -> bool:
    """按自然年切收益: 只有少数年份贡献利润 → 单一 regime."""
    years: dict = {}
    n = len(rets)
    for i, r in enumerate(rets):
        y = i // periods_per_year
        years[y] = years.get(y, 0.0) + r
    if len(years) < 2:
        return False
    yearly = sorted(years.values())
    positive = sum(1 for v in yearly if v > 0)
    return positive < 2 or (yearly[-1] > 0 and sum(yearly[:-1]) <= 0)


def _spearman(a: list[float], b: list[float]) -> float:
    ra = _ranks(a)
    rb = _ranks(b)
    n = len(a)
    d2 = sum((x - y) ** 2 for x, y in zip(ra, rb))
    return 1 - 6 * d2 / (n * (n * n - 1)) if n > 1 else 0.0


def _ranks(xs: list[float]) -> list[float]:
    order = sorted(range(len(xs)), key=lambda i: xs[i])
    r = [0.0] * len(xs)
    for rank, i in enumerate(order):
        r[i] = float(rank)
    return r


# ---------------------------------------------------------------------------
# 演示: BTC 日线 200 日规则 vs 买入持有
# ---------------------------------------------------------------------------


def _fetch_closes(symbol: str, limit: int = 1000) -> tuple[list[float], bool]:
    url = (
        f"http://127.0.0.1:8080/api/klines?symbol={symbol}"
        f"&interval=1d&limit={limit}&exchange=binance"
    )
    try:
        with urllib.request.urlopen(url, timeout=20) as resp:
            ks = json.load(resp)
        return [float(k["close"]) for k in ks], False
    except Exception:
        import random

        random.seed(7)
        px, out = 100.0, []
        for _ in range(limit):
            px *= math.exp(random.gauss(0.0003, 0.03))
            out.append(px)
        return out, True


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--symbol", default="BTCUSDT")
    ap.add_argument("--train-frac", type=float, default=0.6)
    args = ap.parse_args()

    closes, synthetic = _fetch_closes(args.symbol)
    src = "合成随机游走 (本机 API 不可达)" if synthetic else f"本机 API {args.symbol} 日线"
    print(f"数据: {src}, {len(closes)} 根")

    pos = trend_rule_positions(closes)
    sys_rets = apply_positions(closes, pos)
    bench_rets = log_returns(closes)

    v = verdict(sys_rets, bench_rets, periods_per_year=365, train_frac=args.train_frac)
    print(f"裁决: {v}")
    print(
        f"  全期: 系统 CAGR {cagr(sys_rets,365):+.1%} / MDD {max_drawdown(sys_rets):.1%}"
        f" | 基准 CAGR {cagr(bench_rets,365):+.1%} / MDD {max_drawdown(bench_rets):.1%}"
    )

    sw = sweep_trend_rule(closes, 365)
    print(f"扫参: {sw}")

    flags = []
    if flag_lookahead(pos, closes):
        flags.append("LOOKAHEAD 前视嫌疑")
    if flag_one_regime(sys_rets, 365):
        flags.append("ONE_REGIME 单一regime")
    print(f"红旗: {', '.join(flags) if flags else '无 (可自动化子集)'}")
    print("注: 马丁格尔检查需要真实下单序列 sizes, 演示略; 文中 8 红旗的 repaint/survivorship/"
          "fake-AI/secrets 四项需要数据血缘与合规信息, 无法从收益序列自动判定.")


if __name__ == "__main__":
    main()
