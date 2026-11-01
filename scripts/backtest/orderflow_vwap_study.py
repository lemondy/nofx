"""Offline implementation of docs/research/2026-11-01-orderflow-vwap-prereg.md.

No downloads, trading, fitting of thresholds, or writes to the input directory.
All cross-sectional operations use average ranks. Functions accept synthetic
panels independently of the CLI; numpy and pandas are the only dependencies.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass, field
import hashlib
import json
import math
from pathlib import Path
from statistics import NormalDist
import subprocess
import time

import numpy as np
import pandas as pd


SEED = 20261101
HORIZONS = (4, 24)
WARMUP = 720
MIN_SYMBOLS = 30
COST = 2 * (0.0005 + 0.0003)
BOOTSTRAPS = 2000
DEV_START = pd.Timestamp("2025-11-15 00:00", tz="UTC")
DEV_END = pd.Timestamp("2026-06-30 23:00", tz="UTC")
HOLDOUT_START = pd.Timestamp("2026-07-01 00:00", tz="UTC")
REPO = Path(__file__).resolve().parents[2]
DEFAULT_CACHE = Path("/Users/zhangyun/workspace/nofx/data/backtest_cache/ict1h")
FIELDS = ("open", "high", "low", "close", "base_volume", "quote_volume", "taker_buy_base")
FEATURES = ("imb_4", "imb_24", "absorb_24", "vwap24_dist", "vwapD_dist", "vwap168_dist")
CONTROLS = ("rsi14", "macd_atr", "ema20_dist", "bollinger_b", "volume_ratio", "ret_4", "ret_24")
CHOICES = [
    "以输入 close_ms 为精确 UTC 决策时间，补齐等间隔 1 小时索引，不填充价格或成交量。",
    "dev 两端按文档时间字面值包含（截至 2026-06-30 23:00）；标签终点不得进入 holdout。",
    "720 根预热计 t 之前实际存在的 K 线（不要求此前连续）；标签完整性要求 t..t+h 每小时都有 K 线。",
    "日 VWAP 按决策收盘时间的 UTC 日期累计，00:00 收盘的当前 K 线属于新日并触发重置。",
    "Wilder 用首 14 个有效连续观测的算术均值起始，随后 alpha=1/14；首根 TR=high-low，缺失后重新预热。",
    "RSI 用 14 个价格变化；全平为 50、无损失且有上涨为 100。EMA adjust=False，首观测起递推，至少满周期才输出。",
    "MACD 柱为 DIF-DEA（不乘 2）；布林标准差 ddof=0；零分母视为缺失。",
    "absorb_24 仅在因果合格池（有当根、成交额>0、已预热）内回归，不以未来标签可用性选择拟合币池。",
    "IC 按特征/标签成对有限值，偏 IC 另要求全部控制有限；各自不足 30 币时丢弃该时点。标签中位数先按完整合格池计算。",
    "秩正态化用平均秩，概率=(rank-0.5)/n；OLS 含截距。特征完全被控制解释时，数值零残差的偏 IC 定义为 0。",
    "月度一致率为有效月份均值与该样本段整体均值同号的百分比；零均值不算同号。NW 对去缺失后的 IC 序列计算，自协方差分母 n。",
    "G3 固定网格锚定首个不早于 holdout 起点的面板时间，不因缺失顺延；每端 floor(0.2*n) 币，按符号名稳定打破并列。",
    "G3 按标签定义使用对数超额收益的等权多空差，扣 0.0016；循环移动块自助法，拼接后截到原期数，严格计算均值>0 的频率。",
    "置换在每个 h 的完整合格池内逐时独立进行；特征/控制/偏 IC 残差固定，标签秩可复用，两个 h 的置换独立。",
    "G0 只有恰好 100 次置换且两项通过才确认；--sample 是子池诊断，非 100 次或 --skip-null 均不解读真实数据。",
    "合成面板保留实际缺失位置、价格起始量级；每币真实小时对数收益样本标准差 ddof=1，收益均值为 0。",
    "合成量在每币有效时点内独立打乱；open=前收、high/low=开收极值、quote_volume=volume*close，买量为独立 Beta(50,50)*volume。",
    "BTC 4h K 线按 UTC 右闭右标记聚合，必须有 4 根；EMA20/50 完成后前向对齐至小时，等值不归多空。",
    "描述相关矩阵用因果合格池且不依赖标签；时序合并 IC 在各样本段/币内部仅将特征去均值，标签保留原始未来对数收益，再合并算 Spearman。",
    "每项随机程序独立从固定种子 20261101 起始；各 G3 格子采用相同种子，未调整任何门槛。",
]


@dataclass
class Panel:
    frames: dict[str, pd.DataFrame]
    input_hashes: dict[str, str] = field(default_factory=dict)

    @property
    def index(self):
        return self.frames["close"].index

    @property
    def columns(self):
        return self.frames["close"].columns

    @property
    def present(self):
        return self.frames["close"].notna()

    def __getitem__(self, key):
        return self.frames[key]


@dataclass
class Labels:
    raw: pd.DataFrame
    excess: pd.DataFrame
    eligible: pd.DataFrame


def load_panel(cache: str | Path, sample: int | None = None) -> Panel:
    """Read/hash selected files, align exact close times on a complete hour grid."""
    paths = sorted(p for p in Path(cache).glob("*.json") if not p.name.endswith(".state.json"))
    if sample is not None:
        if sample < 1:
            raise ValueError("sample must be positive")
        paths = paths[:sample]
    if not paths:
        raise ValueError(f"no candle JSON files in {cache}")
    series = {name: {} for name in FIELDS}
    hashes = {}
    for path in paths:
        content = path.read_bytes()
        hashes[str(path.resolve())] = hashlib.sha256(content).hexdigest()
        rows = np.asarray(json.loads(content), dtype=float)
        if rows.ndim != 2 or rows.shape[1] != 9 or not len(rows):
            raise ValueError(f"{path}: expected nonempty 9-column candles")
        idx = pd.to_datetime(rows[:, 8].astype(np.int64), unit="ms", utc=True)
        if idx.has_duplicates or not idx.is_monotonic_increasing:
            raise ValueError(f"{path}: close times must be unique and sorted")
        for j, name in enumerate(FIELDS, start=1):
            series[name][path.stem] = pd.Series(rows[:, j], index=idx)
    union = pd.DatetimeIndex(sorted(set().union(*(s.index for s in series["close"].values()))))
    grid = pd.date_range(union[0], union[-1], freq="h")
    if not union.isin(grid).all():
        raise ValueError("close times do not share one hourly grid")
    return Panel({k: pd.DataFrame(v).reindex(grid).rename_axis("close_time_utc")
                  for k, v in series.items()}, hashes)


def causal_eligibility(panel: Panel) -> pd.DataFrame:
    present = panel.present
    return present & panel["quote_volume"].gt(0) & (present.cumsum() - present.astype(int)).ge(WARMUP)


def eligibility_mask(panel: Panel, h: int, min_symbols: int = MIN_SYMBOLS) -> pd.DataFrame:
    complete = panel.present.astype(int).rolling(h + 1, min_periods=h + 1).sum().shift(-h).eq(h + 1)
    mask = causal_eligibility(panel) & complete
    return mask & mask.sum(axis=1).ge(min_symbols).to_numpy()[:, None]


def make_labels(panel: Panel, h: int, min_symbols: int = MIN_SYMBOLS) -> Labels:
    mask = eligibility_mask(panel, h, min_symbols)
    raw = np.log(panel["close"].shift(-h) / panel["close"]).where(mask)
    return Labels(raw, raw.sub(raw.median(axis=1), axis=0), mask)


def wilder(values: pd.DataFrame, period: int = 14) -> pd.DataFrame:
    """Arithmetic seed, then Wilder recursion; a missing value resets the seed."""
    a = values.to_numpy(float)
    out = np.full_like(a, np.nan)
    count = np.zeros(a.shape[1], dtype=int)
    total = np.zeros(a.shape[1])
    previous = np.zeros(a.shape[1])
    for t, row in enumerate(a):
        valid = np.isfinite(row)
        count[~valid], total[~valid], previous[~valid] = 0, 0, 0
        count[valid] += 1
        seeding = valid & (count <= period)
        total[seeding] += row[seeding]
        start = valid & (count == period)
        previous[start] = total[start] / period
        running = valid & (count > period)
        previous[running] += (row[running] - previous[running]) / period
        ready = valid & (count >= period)
        out[t, ready] = previous[ready]
    return pd.DataFrame(out, index=values.index, columns=values.columns)


def ema(values: pd.DataFrame, period: int) -> pd.DataFrame:
    return values.ewm(span=period, adjust=False, min_periods=period, ignore_na=False).mean().where(values.notna())


def compute_indicators(panel: Panel) -> dict[str, pd.DataFrame]:
    c, v, q = panel["close"], panel["base_volume"], panel["quote_volume"]
    previous = c.shift(1)
    tr = pd.DataFrame(np.maximum.reduce([
        (panel["high"] - panel["low"]).to_numpy(),
        (panel["high"] - previous).abs().fillna(panel["high"] - panel["low"]).to_numpy(),
        (panel["low"] - previous).abs().fillna(panel["high"] - panel["low"]).to_numpy(),
    ]), index=c.index, columns=c.columns)
    result = {"atr14": wilder(tr), "ema20": ema(c, 20)}
    for n in (24, 168):
        result[f"vwap{n}"] = q.rolling(n).sum() / v.rolling(n).sum().replace(0, np.nan)
    day = c.index.floor("D")
    result["vwapD"] = q.groupby(day).cumsum() / v.groupby(day).cumsum().replace(0, np.nan)
    return result


def compute_controls(panel: Panel, indicators: dict | None = None) -> dict[str, pd.DataFrame]:
    z = compute_indicators(panel) if indicators is None else indicators
    c, v = panel["close"], panel["base_volume"]
    atr = z["atr14"].replace(0, np.nan)
    changes = c.diff()
    gains, losses = wilder(changes.clip(lower=0)), wilder(-changes.clip(upper=0))
    rsi = 100 - 100 / (1 + gains / losses.replace(0, np.nan))
    rsi = rsi.mask((losses == 0) & (gains > 0), 100).mask((losses == 0) & (gains == 0), 50)
    macd = ema(c, 12) - ema(c, 26)
    mean, std = c.rolling(20).mean(), c.rolling(20).std(ddof=0)
    return {"rsi14": rsi, "macd_atr": (macd - ema(macd, 9)) / atr,
            "ema20_dist": (c - z["ema20"]) / atr,
            "bollinger_b": (c - (mean - 2 * std)) / (4 * std).replace(0, np.nan),
            "volume_ratio": v / v.rolling(20).mean().replace(0, np.nan),
            "ret_4": np.log(c / c.shift(4)), "ret_24": np.log(c / c.shift(24))}


def ols_residuals(feature: pd.DataFrame, controls: dict[str, pd.DataFrame],
                  mask: pd.DataFrame | np.ndarray | None = None, *, gaussian: bool = False,
                  min_symbols: int = 2) -> pd.DataFrame:
    """Cross-sectional OLS with intercept. No labels or future availability used."""
    x = feature.to_numpy(float)
    cs = [c.to_numpy(float) for c in controls.values()]
    valid = np.isfinite(x)
    for c in cs:
        valid &= np.isfinite(c)
    if mask is not None:
        valid &= np.asarray(mask, dtype=bool)
    target = rank_gauss(x, valid) if gaussian else np.where(valid, x, np.nan)
    design = np.stack([rank_gauss(c, valid) if gaussian else c for c in cs], axis=-1)
    result = np.full_like(x, np.nan)
    for t in np.flatnonzero(valid.sum(axis=1) >= min_symbols):
        keep = valid[t]
        a = np.column_stack((np.ones(keep.sum()), design[t, keep]))
        y = target[t, keep]
        residual = y - a @ np.linalg.lstsq(a, y, rcond=None)[0]
        if gaussian and np.linalg.norm(residual) <= 1e-10 * max(1.0, np.linalg.norm(y)):
            residual[:] = 0.0
        result[t, keep] = residual
    return pd.DataFrame(result, index=feature.index, columns=feature.columns)


def compute_features(panel: Panel, controls: dict | None = None,
                     indicators: dict | None = None) -> dict[str, pd.DataFrame]:
    z = compute_indicators(panel) if indicators is None else indicators
    controls = compute_controls(panel, z) if controls is None else controls
    v, d = panel["base_volume"], 2 * panel["taker_buy_base"] - panel["base_volume"]
    result = {f"imb_{n}": d.rolling(n).sum() / v.rolling(n).sum().replace(0, np.nan) for n in (4, 24)}
    result["absorb_24"] = ols_residuals(result["imb_24"], {"ret_24": controls["ret_24"]}, causal_eligibility(panel))
    for n in ("24", "D", "168"):
        result[f"vwap{n}_dist"] = (panel["close"] - z[f"vwap{n}"]) / z["atr14"].replace(0, np.nan)
    return {k: result[k] for k in FEATURES}


def ranks(values: np.ndarray, mask: np.ndarray | None = None) -> np.ndarray:
    a = np.asarray(values, dtype=float)
    if mask is not None:
        a = np.where(mask, a, np.nan)
    return pd.DataFrame(a).rank(axis=1, method="average").to_numpy()


def rank_gauss(values: np.ndarray, mask: np.ndarray | None = None) -> np.ndarray:
    r = ranks(values, mask)
    n = np.isfinite(r).sum(axis=1)
    # Half-integer average ranks permit a small exact NormalDist lookup table.
    table = np.full((r.shape[1] + 1, 2 * r.shape[1] + 1), np.nan)
    normal = NormalDist()
    for count in np.unique(n[n > 0]):
        table[count, 2:2 * count + 1] = [normal.inv_cdf((j / 2 - .5) / count)
                                       for j in range(2, 2 * count + 1)]
    return table[n[:, None], np.nan_to_num(2 * r, nan=0).astype(int)]


def rank_unit(values: np.ndarray, mask: np.ndarray | None = None) -> np.ndarray:
    r = ranks(values, mask)
    valid = np.isfinite(r)
    count = valid.sum(axis=1, keepdims=True)
    mean = np.divide(np.nansum(r, axis=1, keepdims=True), count,
                     out=np.zeros((len(r), 1)), where=count > 0)
    centered = np.where(valid, r - mean, 0.0)
    norm = np.sqrt(np.sum(centered ** 2, axis=1, keepdims=True))
    unit = np.divide(centered, norm, out=np.zeros_like(centered), where=norm > 0)
    return np.where(valid, unit, np.nan)


def unit_ic(x: np.ndarray, y: np.ndarray, min_symbols: int = MIN_SYMBOLS,
            constant_zero: bool = False) -> np.ndarray:
    valid = np.isfinite(x) & np.isfinite(y)
    out = np.sum(np.where(valid, x * y, 0.0), axis=1)
    bad = (valid.sum(axis=1) < min_symbols) | (np.nansum(y * y, axis=1) == 0)
    if not constant_zero:
        bad |= np.nansum(x * x, axis=1) == 0
    out[bad] = np.nan
    return np.clip(out, -1, 1)


def cross_sectional_ic(feature: pd.DataFrame, label: pd.DataFrame,
                       eligible: pd.DataFrame | None = None, min_symbols: int = MIN_SYMBOLS) -> pd.Series:
    x, y = feature.to_numpy(float), label.to_numpy(float)
    mask = np.isfinite(x) & np.isfinite(y)
    if eligible is not None:
        mask &= np.asarray(eligible)
    return pd.Series(unit_ic(rank_unit(x, mask), rank_unit(y, mask), min_symbols), index=feature.index)


def partial_ic(feature: pd.DataFrame, controls: dict[str, pd.DataFrame], label: pd.DataFrame,
               eligible: pd.DataFrame | None = None, min_symbols: int = MIN_SYMBOLS) -> pd.Series:
    mask = label.notna() if eligible is None else eligible & label.notna()
    residual = ols_residuals(feature, controls, mask, gaussian=True, min_symbols=min_symbols)
    valid = residual.notna().to_numpy()
    result = unit_ic(rank_unit(residual.to_numpy(), valid), rank_unit(label.to_numpy(), valid),
                     min_symbols, constant_zero=True)
    return pd.Series(result, index=feature.index)


def newey_west_t(series, lag: int) -> float:
    a = np.asarray(series, dtype=float)
    a = a[np.isfinite(a)]
    n = len(a)
    if n < 2:
        return float("nan")
    u = a - a.mean()
    # Bartlett lag L retains the preregistered denominator L+1 even for short samples.
    # Avoid thousands of small threaded BLAS calls in the permutation null.
    long_variance = float(np.einsum("i,i->", u, u) / n)
    for k in range(1, min(lag, n - 1) + 1):
        long_variance += 2 * (1 - k / (lag + 1)) * float(np.einsum("i,i->", u[k:], u[:-k]) / n)
    if long_variance <= 0:
        return float("nan")
    return float(a.mean() / np.sqrt(long_variance / n))


def monthly_sign_consistency(series: pd.Series) -> dict:
    clean = series.dropna()
    # Numeric calendar groups avoid formatting every timestamp in every null run.
    monthly = clean.groupby([clean.index.year, clean.index.month]).mean()
    mean = clean.mean()
    pct = float((monthly * mean > 0).mean() * 100) if len(monthly) else float("nan")
    return {"monthly_sign_pct": pct,
            "monthly_means": {f"{year:04d}-{month:02d}": float(value)
                              for (year, month), value in monthly.items()}}


def summarize(series: pd.Series, h: int) -> dict:
    return {"n": int(series.notna().sum()), "mean": float(series.mean()),
            "nw_t": newey_west_t(series, h), **monthly_sign_consistency(series)}


def split_masks(index: pd.DatetimeIndex, h: int) -> dict[str, np.ndarray]:
    endpoint = index + pd.Timedelta(h, unit="h")
    return {"dev": np.asarray((index >= DEV_START) & (index <= DEV_END) & (endpoint < HOLDOUT_START)),
            "holdout": np.asarray((index >= HOLDOUT_START) & (endpoint <= index[-1]))}


def g1(ic: dict, partial: dict) -> bool:
    return bool(abs(ic["nw_t"]) >= 3.0 and ic["mean"] * partial["mean"] > 0
                and abs(partial["nw_t"]) >= 2.0)


def g2(dev: dict, holdout: dict, partial: dict) -> bool:
    direction = np.sign(dev["mean"])
    return bool(direction * holdout["mean"] > 0 and direction * holdout["nw_t"] >= 2.0
                and direction * partial["mean"] > 0 and direction * partial["nw_t"] >= 1.5)


@dataclass
class PreparedCell:
    x: np.ndarray
    residual: np.ndarray
    raw_mask: np.ndarray
    partial_mask: np.ndarray
    raw_full: bool
    partial_full: bool


@dataclass
class PreparedHorizon:
    index: pd.DatetimeIndex
    eligible: np.ndarray
    y: np.ndarray
    cells: dict[str, PreparedCell]


def prepare_horizon(features: dict, controls: dict, labels: Labels) -> PreparedHorizon:
    eligible = labels.eligible.to_numpy()
    cells = {}
    for name, f in features.items():
        raw_mask = eligible & np.isfinite(f.to_numpy())
        residual = ols_residuals(f, controls, eligible, gaussian=True, min_symbols=MIN_SYMBOLS)
        part_mask = np.isfinite(residual.to_numpy())
        cells[name] = PreparedCell(rank_unit(f.to_numpy(), raw_mask), rank_unit(residual.to_numpy(), part_mask),
                                   raw_mask, part_mask, np.array_equal(raw_mask, eligible),
                                   np.array_equal(part_mask, eligible))
    return PreparedHorizon(labels.raw.index, eligible, rank_unit(labels.excess.to_numpy(), eligible), cells)


def evaluate_prepared(prepared: PreparedHorizon, y: np.ndarray | None = None) -> dict:
    y = prepared.y if y is None else y
    result = {}
    for name, cell in prepared.cells.items():
        raw_y = y if cell.raw_full else rank_unit(y, cell.raw_mask)
        part_y = y if cell.partial_full else rank_unit(y, cell.partial_mask)
        result[name] = (pd.Series(unit_ic(cell.x, raw_y), index=prepared.index),
                        pd.Series(unit_ic(cell.residual, part_y, constant_zero=True), index=prepared.index))
    return result


def permute_labels(values: np.ndarray, eligible: np.ndarray, rng: np.random.Generator) -> np.ndarray:
    """Independent row permutations, with missing positions preserved, no row loop."""
    order = np.argsort(np.where(eligible, rng.random(values.shape), 2.0), axis=1)
    donors = np.take_along_axis(values, order, axis=1)
    first_n = np.arange(values.shape[1])[None, :] < eligible.sum(axis=1)[:, None]
    out = np.full_like(values, np.nan)
    out[eligible] = donors[first_n]
    return out


def permutation_null(prepared: dict[int, PreparedHorizon], runs: int = 100,
                     seed: int = SEED, progress=None) -> dict:
    rng = np.random.default_rng(seed)
    devs = {h: split_masks(p.index, h)["dev"] for h, p in prepared.items()}
    # Only development rows can affect G1; keep null work proportional to that sample.
    subsets = {h: PreparedHorizon(p.index[devs[h]], p.eligible[devs[h]], p.y[devs[h]],
                {n: PreparedCell(c.x[devs[h]], c.residual[devs[h]], c.raw_mask[devs[h]],
                                  c.partial_mask[devs[h]], c.raw_full, c.partial_full)
                 for n, c in p.cells.items()}) for h, p in prepared.items()}
    results = []
    for run in range(runs):
        cells = {}
        for h, p in subsets.items():
            y = permute_labels(p.y, p.eligible, rng)
            for name, (ic, partial) in evaluate_prepared(p, y).items():
                a, b = summarize(ic, h), summarize(partial, h)
                cells[f"{name}/h{h}"] = {"ic": a, "partial_ic": b, "g1": g1(a, b)}
        results.append({"run": run + 1, "any_g1": any(c["g1"] for c in cells.values()), "cells": cells})
        if progress is not None and ((run + 1) % 5 == 0 or run + 1 == runs):
            progress(f"标签置换 {run + 1}/{runs}")
    count = sum(r["any_g1"] for r in results)
    return {"runs": runs, "seed": seed, "any_g1_count": count,
            "family_error_rate": count / runs if runs else None,
            "max_allowed_count": 10, "passed": count <= 10 if runs == 100 else None,
            "run_results": results}


def synthetic_random_walk(panel: Panel, seed: int = SEED) -> Panel:
    rng = np.random.default_rng(seed)
    c = panel["close"].to_numpy()
    mask = panel.present.to_numpy()
    returns = np.log(panel["close"] / panel["close"].shift(1))
    sigma = returns.std(ddof=1).to_numpy()
    if not np.isfinite(sigma).all():
        raise ValueError("synthetic null requires each symbol to have a measurable hourly volatility")
    initial = panel["close"].bfill().iloc[0].to_numpy()
    increments = rng.normal(0.0, sigma, c.shape)
    log_close = np.log(initial)[None, :] + np.cumsum(increments, axis=0)
    close = np.exp(log_close)
    open_ = np.exp(log_close - increments)
    volume = panel["base_volume"].to_numpy().copy()
    for j in range(volume.shape[1]):
        volume[mask[:, j], j] = rng.permutation(volume[mask[:, j], j])
    buy = rng.beta(50, 50, c.shape) * volume
    arrays = (open_, np.maximum(open_, close), np.minimum(open_, close), close,
              volume, volume * close, buy)
    return Panel({name: pd.DataFrame(np.where(mask, a, np.nan), index=panel.index, columns=panel.columns)
                  for name, a in zip(FIELDS, arrays)})


def g0(prepared: dict[int, PreparedHorizon], panel: Panel, runs: int = 100,
       skip: bool = False, progress=None) -> dict:
    if skip:
        return {"passed": None, "status": "skipped", "permutation": None, "synthetic": None}
    perm = permutation_null(prepared, runs, progress=progress)
    if progress:
        progress("合成随机游走面板")
    synthetic = synthetic_random_walk(panel)
    indicators = compute_indicators(synthetic)
    controls = compute_controls(synthetic, indicators)
    features = compute_features(synthetic, controls, indicators)
    cells = {}
    for h in HORIZONS:
        p = prepare_horizon(features, controls, make_labels(synthetic, h))
        dev = split_masks(p.index, h)["dev"]
        for name, (ic, part) in evaluate_prepared(p).items():
            a, b = summarize(ic[dev], h), summarize(part[dev], h)
            cells[f"{name}/h{h}"] = {"ic": a, "partial_ic": b, "g1": g1(a, b)}
    synth_pass = not any(c["g1"] for c in cells.values())
    passed = False if not synth_pass or perm["passed"] is False else (True if perm["passed"] is True else None)
    return {"passed": passed, "status": "passed" if passed is True else "failed" if passed is False else "incomplete",
            "permutation": perm, "synthetic": {"passed": synth_pass, "cells": cells,
                "hourly_log_return_std": np.log(panel["close"] / panel["close"].shift(1)).std(ddof=1).to_dict()}}


def block_bootstrap_probability(net: np.ndarray, h: int, seed: int = SEED,
                                 runs: int = BOOTSTRAPS) -> float:
    a = np.asarray(net, float)
    a = a[np.isfinite(a)]
    if not len(a):
        return float("nan")
    length = max(1, 24 // h)
    rng = np.random.default_rng(seed)
    starts = rng.integers(0, len(a), size=(runs, math.ceil(len(a) / length)))
    indices = ((starts[:, :, None] + np.arange(length)) % len(a)).reshape(runs, -1)[:, :len(a)]
    return float((a[indices].mean(axis=1) > 0).mean())


def g3(feature: pd.DataFrame, labels: Labels, h: int, direction: float,
       seed: int = SEED) -> dict:
    index = feature.index
    starts = index[index >= HOLDOUT_START]
    grid = np.zeros(len(index), dtype=bool)
    if len(starts):
        grid = np.asarray((index >= starts[0]) & ((index - starts[0]) % pd.Timedelta(h, unit="h") == pd.Timedelta(0)))
    selected = grid & split_masks(index, h)["holdout"]
    times, gross, sizes = [], [], []
    for t in np.flatnonzero(selected):
        valid = labels.eligible.iloc[t].to_numpy() & np.isfinite(feature.iloc[t].to_numpy())
        valid &= np.isfinite(labels.excess.iloc[t].to_numpy())
        if valid.sum() < MIN_SYMBOLS or not np.isfinite(direction) or direction == 0:
            continue
        x = feature.iloc[t].to_numpy()[valid] * direction
        y = labels.excess.iloc[t].to_numpy()[valid]
        order = np.argsort(x, kind="stable")
        n = int(valid.sum() * .2)
        gross.append(float(y[order[-n:]].mean() - y[order[:n]].mean()))
        times.append(index[t].isoformat())
        sizes.append(n)
    net = np.asarray(gross) - COST
    mean = float(net.mean()) if len(net) else float("nan")
    probability = block_bootstrap_probability(net, h, seed)
    return {"passed": bool(mean > 0 and probability >= .90), "n": len(net), "direction": float(direction),
            "gross_mean": float(np.mean(gross)) if gross else float("nan"), "net_mean": mean,
            "cost_per_period": COST, "bootstrap_p_mean_gt_zero": probability,
            "bootstrap_runs": BOOTSTRAPS, "block_length_periods": max(1, 24 // h),
            "times": times, "gross": gross, "net": net.tolist(), "symbols_per_leg": sizes}


def btc_regime(panel: Panel) -> pd.Series:
    if "BTCUSDT" not in panel.columns:
        return pd.Series("unavailable", index=panel.index)
    btc = panel["close"][["BTCUSDT"]]
    bars = btc.resample("4h", closed="right", label="right", origin="start_day")
    close = bars.last().where(bars.count() == 4)
    fast, slow = ema(close, 20), ema(close, 50)
    value = pd.Series(np.where(fast.iloc[:, 0] > slow.iloc[:, 0], "bull",
                               np.where(fast.iloc[:, 0] < slow.iloc[:, 0], "bear", "unavailable")), index=close.index)
    return value.reindex(panel.index, method="ffill").fillna("unavailable")


def rank_correlation_means(features: dict, controls: dict, mask: pd.DataFrame) -> dict:
    all_values = {**features, **controls}
    arrays = {k: v.to_numpy() for k, v in all_values.items()}
    masks = {k: np.asarray(mask) & np.isfinite(v) for k, v in arrays.items()}
    units = {k: rank_unit(v, masks[k]) for k, v in arrays.items()}
    result = {}
    for name in features:
        result[name] = {}
        for other in all_values:
            common = masks[name] & masks[other]
            x = units[name] if np.array_equal(common, masks[name]) else rank_unit(arrays[name], common)
            y = units[other] if np.array_equal(common, masks[other]) else rank_unit(arrays[other], common)
            series = unit_ic(x, y)
            finite = series[np.isfinite(series)]
            result[name][other] = {"mean": float(finite.mean()) if len(finite) else float("nan"), "n": len(finite)}
    return result


def time_series_pooled_ic(feature: pd.DataFrame, raw_labels: pd.DataFrame,
                          row_mask: np.ndarray | None = None) -> dict:
    valid = feature.notna() & raw_labels.notna()
    if row_mask is not None:
        valid &= np.asarray(row_mask)[:, None]
    demeaned = feature.where(valid).sub(feature.where(valid).mean(axis=0), axis=1)
    keep = valid.to_numpy()
    x, y = demeaned.to_numpy()[keep], raw_labels.to_numpy()[keep]
    if len(x) < 2:
        return {"ic": float("nan"), "n": len(x)}
    rx = pd.Series(x).rank().to_numpy()
    ry = pd.Series(y).rank().to_numpy()
    rx, ry = rx - rx.mean(), ry - ry.mean()
    denom = np.linalg.norm(rx) * np.linalg.norm(ry)
    return {"ic": float(rx @ ry / denom) if denom else float("nan"), "n": len(x)}


def verdict(cells: dict, calibration: dict) -> dict:
    if calibration["passed"] is not True:
        return {"text": "不解读", "candidates": [], "reason": f"G0 {calibration['status']}"}
    candidates = [key for key, c in cells.items() if c["g1"] and c["g2"] and c["g3"]["passed"]]
    return {"text": "可做参考字段 A/B 的候选" if candidates else "delta/CVD、VWAP 偏离相对已有指标无增量",
            "candidates": candidates}


def analyze_real(panel: Panel, features: dict, controls: dict, labels: dict,
                 prepared: dict) -> dict:
    cells, series_data = {}, {}
    regime = btc_regime(panel)
    for h in HORIZONS:
        splits = split_masks(panel.index, h)
        for name, (ic, part) in evaluate_prepared(prepared[h]).items():
            stats = {s: {"ic": summarize(ic[m], h), "partial_ic": summarize(part[m], h)} for s, m in splits.items()}
            direction = float(np.sign(stats["dev"]["ic"]["mean"]))
            key = f"{name}/h{h}"
            cells[key] = {**stats, "direction_from_dev": direction,
                          "g1": g1(stats["dev"]["ic"], stats["dev"]["partial_ic"]),
                          "g2": g2(stats["dev"]["ic"], stats["holdout"]["ic"], stats["holdout"]["partial_ic"]),
                          "g3": g3(features[name], labels[h], h, direction),
                          "btc_regime": {s: {r: {"ic": summarize(ic[m & regime.eq(r).to_numpy()], h),
                                                   "partial_ic": summarize(part[m & regime.eq(r).to_numpy()], h)}
                                              for r in ("bull", "bear")} for s, m in splits.items()},
                          "time_series_pooled_ic": {s: time_series_pooled_ic(features[name], labels[h].raw, m)
                                                     for s, m in {"all": np.ones(len(panel.index), bool), **splits}.items()}}
            series_data[key] = {"ic": ic.tolist(), "partial_ic": part.tolist()}
    return {"cells": cells, "ic_series": {"index": [t.isoformat() for t in panel.index], "cells": series_data},
            "feature_control_rank_correlations": rank_correlation_means(features, controls, causal_eligibility(panel)),
            "btc_regime_counts": regime.value_counts().to_dict(),
            "eligible_counts": {str(h): labels[h].eligible.sum(axis=1).tolist() for h in HORIZONS}}


def parameters() -> dict:
    return {"seed": SEED, "horizons_hours": HORIZONS, "warmup_prior_bars": WARMUP, "min_symbols": MIN_SYMBOLS,
            "features": FEATURES, "controls": CONTROLS, "imb_windows": [4, 24], "vwap_windows": [24, 168],
            "daily_vwap_timezone": "UTC", "atr_period": 14, "rsi_period": 14, "macd_periods": [12, 26, 9],
            "ema_period": 20, "bollinger_period": 20, "bollinger_std_multiplier": 2, "bollinger_ddof": 0,
            "volume_ratio_period": 20, "dev_start": DEV_START.isoformat(), "dev_end": DEV_END.isoformat(),
            "holdout_start": HOLDOUT_START.isoformat(), "purge": "label endpoint < holdout_start",
            "nw_kernel": "Bartlett", "nw_lag": "h", "g0_null_runs": 100, "g0_max_any_g1_runs": 10,
            "synthetic_beta": [50, 50], "synthetic_return_mean": 0, "synthetic_volatility_ddof": 1,
            "g1_ic_abs_t": 3.0, "g1_partial_abs_t": 2.0, "g2_directional_ic_t": 2.0,
            "g2_directional_partial_t": 1.5, "g3_tail_fraction": .2, "fee": .0005, "slippage": .0003,
            "round_trip_cost": COST, "g3_bootstrap_runs": BOOTSTRAPS, "g3_block_hours": 24,
            "g3_probability": .90, "btc_regime_hours": 4, "btc_regime_ema_periods": [20, 50]}


def json_safe(value):
    if isinstance(value, dict):
        return {str(k): json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple, np.ndarray)):
        return [json_safe(v) for v in value]
    if isinstance(value, (float, np.floating)):
        return float(value) if np.isfinite(value) else None
    if isinstance(value, (np.integer,)):
        return int(value)
    if isinstance(value, np.bool_):
        return bool(value)
    return value


def markdown_report(report: dict) -> str:
    real = report.get("real_data", report.get("real_data_uninterpreted"))
    calibration = report["g0"]
    lines = ["# delta/CVD 与 VWAP 离线研究", "", f"Git HEAD：`{report['git_head']}`。种子：{SEED}。",
             f"样本：{report['data']['symbols']} 币，{report['data']['timestamps']} 个小时；仅为该幸存者币池的结果。",
             "", "## G0 零假设校准", ""]
    states = {"passed": "通过", "failed": "失败", "incomplete": "未完成 100 次，未确认", "skipped": "跳过，未确认"}
    lines.append(f"**G0：{states[calibration['status']]}。**")
    if calibration["permutation"] is not None:
        p = calibration["permutation"]
        lines += [f"标签置换：{p['runs']} 次中 {p['any_g1_count']} 次有任一格通过 G1；预注册要求 100 次中至多 10 次。",
                  f"合成随机游走：{'通过' if calibration['synthetic']['passed'] else '失败'}（12 格均不得通过 G1）。",
                  "", "| 合成格子 | IC 均值 | NW t | 偏 IC 均值 | NW t | G1 |", "|---|---:|---:|---:|---:|---|"]
        for key, cell in calibration["synthetic"]["cells"].items():
            lines.append(f"| {key} | {fmt(cell['ic']['mean'])} | {fmt(cell['ic']['nw_t'])} | {fmt(cell['partial_ic']['mean'])} | {fmt(cell['partial_ic']['nw_t'])} | {yes(cell['g1'])} |")
    lines += ["", "## 真实数据数值", ""]
    if calibration["passed"] is not True:
        lines += ["**不解读。** G0 未通过完整校准；以下数值仅供审计，JSON 位于 `real_data_uninterpreted`。", ""]
    for split, title in (("dev", "开发集"), ("holdout", "留出集")):
        lines += [f"### {title}", "", "| 格子 | IC 均值 | NW t | 偏 IC 均值 | NW t | IC 月同号 % | 偏 IC 月同号 % | 时点数 |",
                  "|---|---:|---:|---:|---:|---:|---:|---:|"]
        for key, cell in real["cells"].items():
            a, b = cell[split]["ic"], cell[split]["partial_ic"]
            lines.append(f"| {key} | {fmt(a['mean'])} | {fmt(a['nw_t'])} | {fmt(b['mean'])} | {fmt(b['nw_t'])} | {fmt(a['monthly_sign_pct'], 1)} | {fmt(b['monthly_sign_pct'], 1)} | {a['n']} |")
    lines += ["", "### 门槛与经济显著性", "", "| 格子 | dev 方向 | G1 | G2 | G3 | 期数 | 净均值 | P(均值>0) |", "|---|---:|---|---|---|---:|---:|---:|"]
    for key, c in real["cells"].items():
        g = c["g3"]
        lines.append(f"| {key} | {fmt(c['direction_from_dev'], 0)} | {yes(c['g1'])} | {yes(c['g2'])} | {yes(g['passed'])} | {g['n']} | {fmt(g['net_mean'])} | {fmt(g['bootstrap_p_mean_gt_zero'])} |")
    lines += ["", "## 结论", "", report["verdict"]["text"]]
    if report["verdict"]["candidates"]:
        lines.append("候选：" + "、".join(report["verdict"]["candidates"]))
    lines += ["", "## 仅描述：BTC 4h EMA20/50 状态与时序合并 IC", "",
              "| 格子 | 样本 | BTC 多头 IC | BTC 空头 IC | 时序合并 IC |", "|---|---|---:|---:|---:|"]
    for key, c in real["cells"].items():
        for split in ("dev", "holdout"):
            r = c["btc_regime"][split]
            lines.append(f"| {key} | {split} | {fmt(r['bull']['ic']['mean'])} | {fmt(r['bear']['ic']['mean'])} | {fmt(c['time_series_pooled_ic'][split]['ic'])} |")
    lines += ["", "## 仅描述：横截面秩相关均值", "",
              "| 特征 | " + " | ".join((*FEATURES, *CONTROLS)) + " |", "|---|" + "---:|" * (len(FEATURES) + len(CONTROLS))]
    for name, row in real["feature_control_rank_correlations"].items():
        lines.append("| " + name + " | " + " | ".join(fmt(row[k]["mean"]) for k in (*FEATURES, *CONTROLS)) + " |")
    lines += ["", "## 固定参数与歧义处理", "", "```json", json.dumps(report["params"], ensure_ascii=False, indent=2), "```", ""]
    lines.extend(f"- {choice}" for choice in CHOICES)
    lines += ["", "输入文件 SHA256、完整 IC 序列、月均值、自助法收益序列和所有置换格子数值见 report.json。", ""]
    return "\n".join(lines)


def fmt(value, precision=4):
    return f"{value:.{precision}f}" if value is not None and np.isfinite(value) else "—"


def yes(value):
    return "通过" if value else "未通过"


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cache", type=Path, default=DEFAULT_CACHE)
    parser.add_argument("--out", type=Path, default=REPO / ".orderflow_out")
    parser.add_argument("--sample", type=int)
    parser.add_argument("--null-runs", type=int, default=100)
    parser.add_argument("--skip-null", action="store_true")
    args = parser.parse_args(argv)
    if args.null_runs < 1:
        parser.error("--null-runs must be positive")
    if args.sample is not None and args.sample < 1:
        parser.error("--sample must be positive")
    out = args.out.resolve()
    if out.is_relative_to(args.cache.resolve()) or out.is_relative_to(DEFAULT_CACHE.parents[1].resolve()):
        parser.error("output must be outside the input cache and data directory")
    start = time.monotonic()
    def progress(message):
        print(f"[{time.monotonic() - start:.1f}s] {message}", flush=True)
    progress("读取并校验输入文件")
    panel = load_panel(args.cache, args.sample)
    progress(f"{len(panel.index)} 小时 × {len(panel.columns)} 币；计算因果特征和控制")
    indicators = compute_indicators(panel)
    controls = compute_controls(panel, indicators)
    features = compute_features(panel, controls, indicators)
    labels = {h: make_labels(panel, h) for h in HORIZONS}
    prepared = {h: prepare_horizon(features, controls, labels[h]) for h in HORIZONS}
    calibration = g0(prepared, panel, args.null_runs, args.skip_null, progress)
    progress(f"G0 {calibration['status']}；计算审计数值和描述项")
    real = analyze_real(panel, features, controls, labels, prepared)
    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO, text=True, capture_output=True, check=True).stdout.strip()
    report = {"preregistration": "docs/research/2026-11-01-orderflow-vwap-prereg.md", "git_head": head,
              "params": parameters(), "implementation_choices": CHOICES,
              "invocation": {"cache": str(args.cache.resolve()), "out": str(out), "sample": args.sample,
                             "null_runs": args.null_runs, "skip_null": args.skip_null},
              "data": {"symbols": len(panel.columns), "symbol_names": panel.columns.tolist(),
                       "timestamps": len(panel.index), "start": panel.index[0].isoformat(), "end": panel.index[-1].isoformat(),
                       "input_sha256": panel.input_hashes, "survivorship_bias": True},
              "g0": calibration, "verdict": verdict(real["cells"], calibration),
              "real_data" if calibration["passed"] is True else "real_data_uninterpreted": real}
    out.mkdir(parents=True, exist_ok=True)
    (out / "report.json").write_text(json.dumps(json_safe(report), ensure_ascii=False, indent=2, allow_nan=False) + "\n", encoding="utf-8")
    (out / "report.md").write_text(markdown_report(report), encoding="utf-8")
    progress(f"完成：{out / 'report.json'}；{out / 'report.md'}；结论：{report['verdict']['text']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
