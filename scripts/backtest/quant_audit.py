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
  4. 红旗检查 (可自动化的子集): 前视 (因果性截断检验与额外滞后启发式),
     马丁格尔 (亏损后加仓), 单一 regime (利润集中在一两年).

纯标准库, 无第三方依赖. 演示:

    python3 -m scripts.backtest.quant_audit          # BTC 日线 vs 买入持有
    python3 -m scripts.backtest.quant_audit --symbol ETHUSDT

数据来自本机 nofx API (/api/klines); 后端不在时自动退化为合成随机游走
并明确标注. 所有收益率为对数收益; 成本按换仓次数的 round-trip 费率扣除.
"""

from __future__ import annotations

import argparse
from collections.abc import Callable, Mapping
from itertools import product
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
VERDICT_INSUFFICIENT = "INSUFFICIENT_SAMPLE"


@dataclass
class SplitVerdict:
    verdict: str
    detail: dict = field(default_factory=dict)

    def __str__(self) -> str:  # 演示输出用
        d = self.detail
        if self.verdict == VERDICT_INSUFFICIENT:
            return (f"{self.verdict} train={d['train_n']}, holdout={d['holdout_n']}, "
                    f"每窗要求 ≥{d['min_window_periods']}")
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
    min_window_periods: int = 20,
) -> SplitVerdict:
    """按时间切分，两窗都赢 CAGR/Sharpe 才通过；不足时只返回样本信息。

    两序列必须逐期对齐、等长。任一侧 NaN/inf 都成对删除，计数后再保序切分，
    避免独立过滤使系统与基准错位。此处只验证已有样本，不证明样本外表现。
    """
    if len(sys_rets) != len(bench_rets):
        raise ValueError("system and benchmark returns must be aligned and equal length")
    if not 0 < train_frac < 1:
        raise ValueError("train_frac must be between 0 and 1")
    if type(min_window_periods) is not int or min_window_periods < 1:
        raise ValueError("min_window_periods must be a positive integer")
    if periods_per_year <= 0:
        raise ValueError("periods_per_year must be positive")
    pairs = [(s, b) for s, b in zip(sys_rets, bench_rets) if math.isfinite(s) and math.isfinite(b)]
    filtered = {"system": sum(not math.isfinite(r) for r in sys_rets),
                "benchmark": sum(not math.isfinite(r) for r in bench_rets),
                "pairs": len(sys_rets) - len(pairs)}
    sys_rets = [s for s, _ in pairs]
    bench_rets = [b for _, b in pairs]
    s_tr, s_ho = time_split(sys_rets, train_frac)
    b_tr, b_ho = time_split(bench_rets, train_frac)
    # Exact minimum under floor(n * train_frac), including integer rounding.
    required = max(math.ceil(min_window_periods / train_frac),
                   math.floor((min_window_periods - 1) / (1 - train_frac)) + 1)
    while min(int(required * train_frac), required - int(required * train_frac)) < min_window_periods:
        required += 1
    sufficient = min(len(s_tr), len(s_ho)) >= min_window_periods
    d: dict = {"n_periods": len(pairs), "required": required, "train_frac": train_frac,
               "min_window_periods": min_window_periods, "sample_sufficient": sufficient,
               "filtered": filtered, "train_n": len(s_tr), "holdout_n": len(s_ho),
               "train_n_bench": len(b_tr), "holdout_n_bench": len(b_ho)}
    if not sufficient:
        d["reason"] = (f"训练窗 {len(s_tr)}、保留窗 {len(s_ho)}；"
                       f"每窗至少需要 {min_window_periods} 期")
        return SplitVerdict(VERDICT_INSUFFICIENT, d)
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


def audit_returns(
    sys_rets: list[float],
    bench_rets: list[float],
    periods_per_year: int,
    *,
    train_frac: float = 0.6,
    min_window_periods: int = 20,
) -> dict:
    """可复用的对齐对数收益审计；不足时不输出 CAGR/Sharpe，结果可 JSON 序列化。"""
    result = verdict(sys_rets, bench_rets, periods_per_year, train_frac, min_window_periods)
    return {"verdict": result.verdict, **result.detail}


# ---------------------------------------------------------------------------
# 2. 200 日滞回趋势规则 (文中基准系统)
# ---------------------------------------------------------------------------


def trend_rule_positions(
    closes: list[float],
    ma_window: int = 200,
    enter_above: float = 0.01,
    exit_below: float = -0.03,
) -> list[int]:
    """返回第 i 根收盘决定、从第 i+1 根生效的目标敞口 (0/1)。"""
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
    """信号层收益近似（非订单模拟）：第 i 根收益用 positions[i-1]。

    positions[i] 是第 i 根收盘才决定、从第 i+1 根起生效的目标敞口 (0/1)。
    生效敞口与前一期不同就在该收益 bar 扣 half×cost_rt；初始敞口视为 0，
    故 positions[0]=1 在首个收益 bar 扣费。末根信号无后续 bar，不成交也不扣费。
    持有期吃 close-to-close 对数收益，空仓期吃 rf_annual/periods_per_year。
    """
    if len(positions) != len(closes):
        raise ValueError("positions and closes must have equal length")
    rets: list[float] = []
    rf = rf_annual / periods_per_year
    for i in range(1, len(closes)):
        r = math.log(closes[i] / closes[i - 1]) if closes[i - 1] > 0 else 0.0
        exp = positions[i - 1]
        gross = r if exp == 1 else rf
        if exp != (positions[i - 2] if i >= 2 else 0):
            gross -= cost_rt / 2.0
        rets.append(gross)
    return rets


# ---------------------------------------------------------------------------
# 3. 扫参平台 vs 孤峰
# ---------------------------------------------------------------------------


@dataclass
class SweepResult:
    grid: dict  # {parameter key: score}; trend_rule uses (ma, enter, exit): sharpe
    default_key: object
    plateau_ratio: float
    lone_peak: bool

    def __str__(self) -> str:
        return (
            f"default {self.default_key} sharpe={self.grid[self.default_key]:+.2f}, "
            f"邻域通过率 {self.plateau_ratio:.0%} → "
            + ("LONE PEAK (过拟合红旗)" if self.lone_peak else "PLATEAU (稳健)")
        )


def sweep_plateau(
    eval_fn: Callable,
    grid: Mapping,
    default_key,
    *,
    min_pass_ratio: float = 0.5,
    score_threshold: float = 0.0,
) -> SweepResult:
    """评估任意参数网格；通过率含默认点，默认点缺席时不判孤峰。"""
    if not 0 <= min_pass_ratio <= 1:
        raise ValueError("min_pass_ratio must be between 0 and 1")
    scores = {key: float(eval_fn(params)) for key, params in grid.items()}
    ratio = sum(score > score_threshold for score in scores.values()) / len(scores) if scores else 0.0
    lone = scores.get(default_key, score_threshold) > score_threshold and ratio < min_pass_ratio
    return SweepResult(scores, default_key, ratio, lone)


def grid_around(defaults, factors=(0.5, 0.75, 1.0, 1.25, 1.5)) -> dict:
    """数值默认参数的笛卡尔邻域：返回 {值元组: 参数字典/元组}。

    支持命名字典或元组；整数先 round(6) 再 int，浮点 round(6)，乘正系数保留符号。
    """
    named = isinstance(defaults, Mapping)
    values = defaults.values() if named else defaults
    factors = tuple(factors)
    axes = [sorted({(int if type(center) is int else float)(round(center * f, 6))
                    for f in factors}) for center in values]
    return {key: dict(zip(defaults, key)) if named else key for key in product(*axes)}


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
    neighborhood = grid_around(defaults, (0.5, 0.75, 1.0, 1.25, 1.5)[:steps])
    grid = {(ma, round(en, 4), round(ex, 4)): params
            for (ma, en, ex), params in neighborhood.items() if en > 0 and ex < 0}
    default_key = (ma0, round(en0, 4), round(ex0, 4))

    def evaluate(params):
        pos = trend_rule_positions(closes, *params)
        rets = apply_positions(closes, pos, periods_per_year=periods_per_year)
        return sharpe(rets, periods_per_year)

    return sweep_plateau(evaluate, grid, default_key)


def _grid(center: float, steps: int = 5, cast=float):
    span = [0.5, 0.75, 1.0, 1.25, 1.5][:steps]
    return sorted({cast(round(center * s, 6)) for s in span})


# ---------------------------------------------------------------------------
# 4. 红旗检查 (可自动化子集)
# ---------------------------------------------------------------------------


def flag_lookahead(positions: list[int], closes: list[float], min_retention: float = 0.2) -> bool:
    """事后启发式：在次 bar 生效基线上，再额外滞后一根 bar 后夏普坍塌。

    判据: base > 0 且 lag1 夏普 < base × min_retention。可能误报，也可能漏报；
    仅凭持仓/收益无法证明因果性，确定使用未来数据需 flag_lookahead_signal 截断检验。
    """
    base = sharpe(apply_positions(closes, positions), 365)
    lagged = [0] + positions[:-1]
    after = sharpe(apply_positions(closes, lagged), 365)
    return base > 0 and after < base * min_retention


def flag_lookahead_signal(signal_fn: Callable, closes: list[float], cuts=None) -> bool:
    """定论性截断检验：任一前缀末信号与全序列相同时点不同，就使用了未来数据。

    signal_fn 必须确定性、返回等长敞口序列。默认检查多个早/中/后期点（含倒数点）；
    cuts 是前缀长度 k，范围 1 <= k < len(closes)。False 只表示所选截断点未发现泄漏。
    """
    n = len(closes)
    if n < 2:
        return False
    full = signal_fn(closes)
    if len(full) != n:
        raise ValueError("signal_fn must return one position per close")
    if cuts is None:
        points = {1, 2, n - 2, n - 1, *(int(n * f) for f in (0.1, 0.25, 0.5, 0.6, 0.75, 0.9))}
        cuts = sorted(k for k in points if 1 <= k < n)
    for k in cuts:
        if not 1 <= k < n:
            raise ValueError("cuts must be prefix lengths between 1 and len(closes)-1")
        prefix = signal_fn(closes[:k])
        if len(prefix) != k:
            raise ValueError("signal_fn must return one position per close")
        if prefix[-1] != full[k - 1]:
            return True
    return False


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
    print("成交口径：次日生效（收盘决定目标敞口，下一根收益起应用）")

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
    if flag_lookahead_signal(trend_rule_positions, closes):
        flags.append("LOOKAHEAD_SIGNAL 截断检验发现未来数据")
    if flag_lookahead(pos, closes):
        flags.append("LOOKAHEAD 前视嫌疑")
    if flag_one_regime(sys_rets, 365):
        flags.append("ONE_REGIME 单一regime")
    print(f"红旗: {', '.join(flags) if flags else '无 (可自动化子集)'}")
    print("注: 马丁格尔检查需要真实下单序列 sizes, 演示略; 文中 8 红旗的 repaint/survivorship/"
          "fake-AI/secrets 四项需要数据血缘与合规信息, 无法从收益序列自动判定.")


if __name__ == "__main__":
    main()
