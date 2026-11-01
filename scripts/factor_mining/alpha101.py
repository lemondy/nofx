"""Predeclared causal Alpha101 translations; independent of the mining DSL.

No source formula is parsed or executed. FORMULAS contains the verbatim local
appendix text (including trailing spaces); TRANSLATIONS is hand translated.
The only unavailable input is cap (#56). Industry neutralization is explicitly
approximated by cross-sectional demeaning, across all observed names.
"""
from __future__ import annotations

import builtins
from functools import wraps
import math
import re

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view


def window(d):
    return builtins.max(1, math.floor(d))


def finite(x):
    return x.replace([np.inf, -np.inf], np.nan) if isinstance(x, pd.DataFrame) else np.where(np.isfinite(x), x, np.nan)


def _like(x, values):
    return pd.DataFrame(values, index=x.index, columns=x.columns, dtype=float)


def rank(x):
    return finite(x).rank(axis=1, method='average', pct=True)


def delay(x, d):
    return finite(x).shift(window(d))


def delta(x, d):
    return finite(x - delay(x, d))


def sum(x, d):
    n = window(d)
    return finite(x).rolling(n, min_periods=n).sum()


def mean(x, d):
    n = window(d)
    return finite(x).rolling(n, min_periods=n).mean()


def stddev(x, d):
    n = window(d)
    return finite(x).rolling(n, min_periods=n).std(ddof=1)


def ts_min(x, d):
    n = window(d)
    return finite(x).rolling(n, min_periods=n).min()


def ts_max(x, d):
    n = window(d)
    return finite(x).rolling(n, min_periods=n).max()


def ts_rank(x, d):
    n = window(d)
    return finite(x).rolling(n, min_periods=n).rank(method='average', pct=True)


def _window_op(x, d, op):
    n = window(d)
    a = finite(x).to_numpy(dtype=float)
    out = np.full(a.shape, np.nan)
    if len(a) >= n:
        w = sliding_window_view(a, n, axis=0)
        values = op(w)
        out[n - 1:] = np.where(np.isfinite(w).all(axis=-1), values, np.nan)
    return finite(_like(x, out))


def product(x, d):
    return _window_op(x, d, lambda w: np.prod(w, axis=-1))


def ts_argmax(x, d):
    # Oldest=1, today=d; np.argmax selects the earliest tie.
    return _window_op(x, d, lambda w: np.argmax(w, axis=-1) + 1.)


def ts_argmin(x, d):
    return _window_op(x, d, lambda w: np.argmin(w, axis=-1) + 1.)


def decay_linear(x, d):
    weights = np.arange(1, window(d) + 1, dtype=float)
    return _window_op(x, d, lambda w: np.einsum('tcn,n->tc', w, weights) / weights.sum())


def _pair_window(x, y, d, corr):
    """Complete, window-local Pearson/sample covariance; bounded memory.

    Center after subtracting the oldest observation: constants have exactly
    zero variance, and future observations cannot change numerical centering.
    """
    n = window(d)
    a, b = finite(x).to_numpy(dtype=float), finite(y).to_numpy(dtype=float)
    out = np.full(a.shape, np.nan)
    if n < 2 or len(a) < n:
        return _like(x, out)
    wa, wb = (sliding_window_view(v, n, axis=0) for v in (a, b))
    for start in range(0, len(wa), 128):
        aa, bb = wa[start:start + 128], wb[start:start + 128]
        valid = np.isfinite(aa).all(axis=-1) & np.isfinite(bb).all(axis=-1)
        ca, cb = aa - aa[..., :1], bb - bb[..., :1]
        ca -= ca.mean(axis=-1, keepdims=True)
        cb -= cb.mean(axis=-1, keepdims=True)
        cross = np.einsum('tcn,tcn->tc', ca, cb)
        if corr:
            den = np.sqrt(np.einsum('tcn,tcn->tc', ca, ca) * np.einsum('tcn,tcn->tc', cb, cb))
            values = np.full(cross.shape, np.nan)
            np.divide(cross, den, out=values, where=valid & (den > 0))
            values = np.clip(values, -1., 1.)
        else:
            values = cross / (n - 1)
        out[start + n - 1:start + n - 1 + len(aa)] = np.where(valid, values, np.nan)
    return finite(_like(x, out))


def correlation(x, y, d):
    return _pair_window(x, y, d, True)


def covariance(x, y, d):
    return _pair_window(x, y, d, False)


def scale(x, a=1):
    # Fix row reduction order regardless of pandas block/memory layout.
    values = np.ascontiguousarray(finite(x).to_numpy(dtype=float))
    denominator = np.nansum(np.abs(values), axis=1)
    out = np.full(values.shape, np.nan)
    np.divide(values * a, denominator[:, None], out=out, where=denominator[:, None] > 0)
    return finite(_like(x, out))


def power(x, a):
    # numpy real power: negative base/noninteger exponent -> NaN.
    with np.errstate(all='ignore'):
        return finite(np.power(finite(x), finite(a)))


def signedpower(x, a):
    return finite(sign(x) * power(abs(x), a))


def abs(x):
    return np.abs(finite(x))


def sign(x):
    return np.sign(finite(x))


def log(x):
    with np.errstate(all='ignore'):
        return finite(np.log(finite(x)))


def indneutralize(x, group=None):
    values = np.ascontiguousarray(finite(x).to_numpy(dtype=float))
    count = np.isfinite(values).sum(axis=1)
    average = np.full(len(values), np.nan)
    np.divide(np.nansum(values, axis=1), count, out=average, where=count > 0)
    return _like(x, values - average[:, None])


def min(x, y):
    return ts_min(x, y) if np.isscalar(y) else finite(np.minimum(finite(x), finite(y)))


def max(x, y):
    return ts_max(x, y) if np.isscalar(y) else finite(np.maximum(finite(x), finite(y)))


def _compare(x, y, op):
    template = x if isinstance(x, pd.DataFrame) else y
    a, b = np.asarray(finite(x)), np.asarray(finite(y))
    return _like(template, np.where(np.isnan(a) | np.isnan(b), np.nan, op(a, b).astype(float)))


def lt(x, y):
    return _compare(x, y, np.less)


def gt(x, y):
    return _compare(x, y, np.greater)


def eq(x, y):
    return _compare(x, y, np.equal)


def logical_or(x, y):
    return _compare(x, y, lambda a, b: (a != 0) | (b != 0))


def where(c, a, b):
    condition = np.asarray(finite(c))
    return finite(_like(c, np.where(np.isnan(condition), np.nan, np.where(condition != 0, a, b))))


class Inputs:
    """Lazy derived inputs, local to one evaluation (no mutable global cache)."""
    def __init__(self, fields):
        self.fields = fields
        self.cache = {}

    def __getattr__(self, key):
        if key == 'returns':
            if key not in self.cache:
                self.cache[key] = finite(self.close / delay(self.close, 1) - 1)
            return self.cache[key]
        return self.fields[key]

    def adv(self, d):
        key = ('adv', d)
        if key not in self.cache:
            self.cache[key] = mean(self.volume * self.vwap, d)
        return self.cache[key]


# Each expression below is a manual translation of the matching appendix row.
# Keep nested industry calls and the original constants, even if redundant.
TRANSLATIONS = {
    1: lambda p: rank(ts_argmax(signedpower(where(lt(p.returns, 0), stddev(p.returns, 20), p.close), 2.), 5)) - 0.5,
    2: lambda p: -correlation(rank(delta(log(p.volume), 2)), rank((p.close - p.open) / p.open), 6),
    3: lambda p: -correlation(rank(p.open), rank(p.volume), 10),
    4: lambda p: -ts_rank(rank(p.low), 9),
    5: lambda p: rank(p.open - sum(p.vwap, 10) / 10) * -abs(rank(p.close - p.vwap)),
    6: lambda p: -correlation(p.open, p.volume, 10),
    7: lambda p: where(lt(p.adv(20), p.volume), -ts_rank(abs(delta(p.close, 7)), 60) * sign(delta(p.close, 7)), -1),
    8: lambda p: -rank(sum(p.open, 5) * sum(p.returns, 5) - delay(sum(p.open, 5) * sum(p.returns, 5), 10)),
    9: lambda p: where(lt(0, ts_min(delta(p.close, 1), 5)), delta(p.close, 1), where(lt(ts_max(delta(p.close, 1), 5), 0), delta(p.close, 1), -delta(p.close, 1))),
    10: lambda p: rank(where(lt(0, ts_min(delta(p.close, 1), 4)), delta(p.close, 1), where(lt(ts_max(delta(p.close, 1), 4), 0), delta(p.close, 1), -delta(p.close, 1)))),
    11: lambda p: (rank(ts_max(p.vwap - p.close, 3)) + rank(ts_min(p.vwap - p.close, 3))) * rank(delta(p.volume, 3)),
    12: lambda p: sign(delta(p.volume, 1)) * -delta(p.close, 1),
    13: lambda p: -rank(covariance(rank(p.close), rank(p.volume), 5)),
    14: lambda p: -rank(delta(p.returns, 3)) * correlation(p.open, p.volume, 10),
    15: lambda p: -sum(rank(correlation(rank(p.high), rank(p.volume), 3)), 3),
    16: lambda p: -rank(covariance(rank(p.high), rank(p.volume), 5)),
    17: lambda p: -rank(ts_rank(p.close, 10)) * rank(delta(delta(p.close, 1), 1)) * rank(ts_rank(p.volume / p.adv(20), 5)),
    18: lambda p: -rank(stddev(abs(p.close - p.open), 5) + (p.close - p.open) + correlation(p.close, p.open, 10)),
    19: lambda p: -sign((p.close - delay(p.close, 7)) + delta(p.close, 7)) * (1 + rank(1 + sum(p.returns, 250))),
    20: lambda p: -rank(p.open - delay(p.high, 1)) * rank(p.open - delay(p.close, 1)) * rank(p.open - delay(p.low, 1)),
    21: lambda p: where(lt(sum(p.close, 8) / 8 + stddev(p.close, 8), sum(p.close, 2) / 2), -1, where(lt(sum(p.close, 2) / 2, sum(p.close, 8) / 8 - stddev(p.close, 8)), 1, where(logical_or(lt(1, p.volume / p.adv(20)), eq(p.volume / p.adv(20), 1)), 1, -1))),
    22: lambda p: -(delta(correlation(p.high, p.volume, 5), 5) * rank(stddev(p.close, 20))),
    23: lambda p: where(lt(sum(p.high, 20) / 20, p.high), -delta(p.high, 2), 0),
    24: lambda p: where(logical_or(lt(delta(sum(p.close, 100) / 100, 100) / delay(p.close, 100), 0.05), eq(delta(sum(p.close, 100) / 100, 100) / delay(p.close, 100), 0.05)), -(p.close - ts_min(p.close, 100)), -delta(p.close, 3)),
    25: lambda p: rank(-p.returns * p.adv(20) * p.vwap * (p.high - p.close)),
    26: lambda p: -ts_max(correlation(ts_rank(p.volume, 5), ts_rank(p.high, 5), 5), 3),
    27: lambda p: where(lt(0.5, rank(sum(correlation(rank(p.volume), rank(p.vwap), 6), 2) / 2.0)), -1, 1),
    28: lambda p: scale(correlation(p.adv(20), p.low, 5) + (p.high + p.low) / 2 - p.close),
    29: lambda p: min(product(rank(rank(scale(log(sum(ts_min(rank(rank(-rank(delta(p.close - 1, 5)))), 2), 1))))), 1), 5) + ts_rank(delay(-p.returns, 6), 5),
    30: lambda p: (1.0 - rank(sign(p.close - delay(p.close, 1)) + sign(delay(p.close, 1) - delay(p.close, 2)) + sign(delay(p.close, 2) - delay(p.close, 3)))) * sum(p.volume, 5) / sum(p.volume, 20),
    31: lambda p: rank(rank(rank(decay_linear(-rank(rank(delta(p.close, 10))), 10)))) + rank(-delta(p.close, 3)) + sign(scale(correlation(p.adv(20), p.low, 12))),
    32: lambda p: scale(sum(p.close, 7) / 7 - p.close) + 20 * scale(correlation(p.vwap, delay(p.close, 5), 230)),
    33: lambda p: rank(-power(1 - p.open / p.close, 1)),
    34: lambda p: rank(1 - rank(stddev(p.returns, 2) / stddev(p.returns, 5)) + (1 - rank(delta(p.close, 1)))),
    35: lambda p: ts_rank(p.volume, 32) * (1 - ts_rank(p.close + p.high - p.low, 16)) * (1 - ts_rank(p.returns, 32)),
    36: lambda p: 2.21 * rank(correlation(p.close - p.open, delay(p.volume, 1), 15)) + 0.7 * rank(p.open - p.close) + 0.73 * rank(ts_rank(delay(-p.returns, 6), 5)) + rank(abs(correlation(p.vwap, p.adv(20), 6))) + 0.6 * rank((sum(p.close, 200) / 200 - p.open) * (p.close - p.open)),
    37: lambda p: rank(correlation(delay(p.open - p.close, 1), p.close, 200)) + rank(p.open - p.close),
    38: lambda p: -rank(ts_rank(p.close, 10)) * rank(p.close / p.open),
    39: lambda p: -rank(delta(p.close, 7) * (1 - rank(decay_linear(p.volume / p.adv(20), 9)))) * (1 + rank(sum(p.returns, 250))),
    40: lambda p: -rank(stddev(p.high, 10)) * correlation(p.high, p.volume, 10),
    41: lambda p: power(p.high * p.low, 0.5) - p.vwap,
    42: lambda p: rank(p.vwap - p.close) / rank(p.vwap + p.close),
    43: lambda p: ts_rank(p.volume / p.adv(20), 20) * ts_rank(-delta(p.close, 7), 8),
    44: lambda p: -correlation(p.high, rank(p.volume), 5),
    45: lambda p: -(rank(sum(delay(p.close, 5), 20) / 20) * correlation(p.close, p.volume, 2) * rank(correlation(sum(p.close, 5), sum(p.close, 20), 2))),
    46: lambda p: where(lt(0.25, (delay(p.close, 20) - delay(p.close, 10)) / 10 - (delay(p.close, 10) - p.close) / 10), -1, where(lt((delay(p.close, 20) - delay(p.close, 10)) / 10 - (delay(p.close, 10) - p.close) / 10, 0), 1, -(p.close - delay(p.close, 1)))),
    47: lambda p: rank(1 / p.close) * p.volume / p.adv(20) * (p.high * rank(p.high - p.close) / (sum(p.high, 5) / 5)) - rank(p.vwap - delay(p.vwap, 5)),
    48: lambda p: indneutralize(correlation(delta(p.close, 1), delta(delay(p.close, 1), 1), 250) * delta(p.close, 1) / p.close, 'subindustry') / sum(power(delta(p.close, 1) / delay(p.close, 1), 2), 250),
    49: lambda p: where(lt((delay(p.close, 20) - delay(p.close, 10)) / 10 - (delay(p.close, 10) - p.close) / 10, -0.1), 1, -(p.close - delay(p.close, 1))),
    50: lambda p: -ts_max(rank(correlation(rank(p.volume), rank(p.vwap), 5)), 5),
    51: lambda p: where(lt((delay(p.close, 20) - delay(p.close, 10)) / 10 - (delay(p.close, 10) - p.close) / 10, -0.05), 1, -(p.close - delay(p.close, 1))),
    52: lambda p: (-ts_min(p.low, 5) + delay(ts_min(p.low, 5), 5)) * rank((sum(p.returns, 240) - sum(p.returns, 20)) / 220) * ts_rank(p.volume, 5),
    53: lambda p: -delta(((p.close - p.low) - (p.high - p.close)) / (p.close - p.low), 9),
    54: lambda p: -((p.low - p.close) * power(p.open, 5)) / ((p.low - p.high) * power(p.close, 5)),
    55: lambda p: -correlation(rank((p.close - ts_min(p.low, 12)) / (ts_max(p.high, 12) - ts_min(p.low, 12))), rank(p.volume), 6),
    56: lambda p: 0 - (1 * (rank(sum(p.returns, 10) / sum(sum(p.returns, 2), 3)) * rank(p.returns * p.cap))),
    57: lambda p: 0 - (1 * ((p.close - p.vwap) / decay_linear(rank(ts_argmax(p.close, 30)), 2))),
    58: lambda p: -ts_rank(decay_linear(correlation(indneutralize(p.vwap, 'sector'), p.volume, 3.92795), 7.89291), 5.50322),
    59: lambda p: -ts_rank(decay_linear(correlation(indneutralize(p.vwap * 0.728317 + p.vwap * (1 - 0.728317), 'industry'), p.volume, 4.25197), 16.2289), 8.19648),
    60: lambda p: 0 - (1 * (2 * scale(rank(((p.close - p.low) - (p.high - p.close)) / (p.high - p.low) * p.volume)) - scale(rank(ts_argmax(p.close, 10))))),
    61: lambda p: lt(rank(p.vwap - ts_min(p.vwap, 16.1219)), rank(correlation(p.vwap, p.adv(180), 17.9282))),
    62: lambda p: lt(rank(correlation(p.vwap, sum(p.adv(20), 22.4101), 9.91009)), rank(lt(rank(p.open) + rank(p.open), rank((p.high + p.low) / 2) + rank(p.high)))) * -1,
    63: lambda p: (rank(decay_linear(delta(indneutralize(p.close, 'industry'), 2.25164), 8.22237)) - rank(decay_linear(correlation(p.vwap * 0.318108 + p.open * (1 - 0.318108), sum(p.adv(180), 37.2467), 13.557), 12.2883))) * -1,
    64: lambda p: lt(rank(correlation(sum(p.open * 0.178404 + p.low * (1 - 0.178404), 12.7054), sum(p.adv(120), 12.7054), 16.6208)), rank(delta((p.high + p.low) / 2 * 0.178404 + p.vwap * (1 - 0.178404), 3.69741))) * -1,
    65: lambda p: lt(rank(correlation(p.open * 0.00817205 + p.vwap * (1 - 0.00817205), sum(p.adv(60), 8.6911), 6.40374)), rank(p.open - ts_min(p.open, 13.635))) * -1,
    66: lambda p: (rank(decay_linear(delta(p.vwap, 3.51013), 7.23052)) + ts_rank(decay_linear(((p.low * 0.96633 + p.low * (1 - 0.96633)) - p.vwap) / (p.open - (p.high + p.low) / 2), 11.4157), 6.72611)) * -1,
    67: lambda p: power(rank(p.high - ts_min(p.high, 2.14593)), rank(correlation(indneutralize(p.vwap, 'sector'), indneutralize(p.adv(20), 'subindustry'), 6.02936))) * -1,
    68: lambda p: lt(ts_rank(correlation(rank(p.high), rank(p.adv(15)), 8.91644), 13.9333), rank(delta(p.close * 0.518371 + p.low * (1 - 0.518371), 1.06157))) * -1,
    69: lambda p: power(rank(ts_max(delta(indneutralize(p.vwap, 'industry'), 2.72412), 4.79344)), ts_rank(correlation(p.close * 0.490655 + p.vwap * (1 - 0.490655), p.adv(20), 4.92416), 9.0615)) * -1,
    70: lambda p: power(rank(delta(p.vwap, 1.29456)), ts_rank(correlation(indneutralize(p.close, 'industry'), p.adv(50), 17.8256), 17.9171)) * -1,
    71: lambda p: max(ts_rank(decay_linear(correlation(ts_rank(p.close, 3.43976), ts_rank(p.adv(180), 12.0647), 18.0175), 4.20501), 15.6948), ts_rank(decay_linear(power(rank((p.low + p.open) - (p.vwap + p.vwap)), 2), 16.4662), 4.4388)),
    72: lambda p: rank(decay_linear(correlation((p.high + p.low) / 2, p.adv(40), 8.93345), 10.1519)) / rank(decay_linear(correlation(ts_rank(p.vwap, 3.72469), ts_rank(p.volume, 18.5188), 6.86671), 2.95011)),
    73: lambda p: max(rank(decay_linear(delta(p.vwap, 4.72775), 2.91864)), ts_rank(decay_linear(delta(p.open * 0.147155 + p.low * (1 - 0.147155), 2.03608) / (p.open * 0.147155 + p.low * (1 - 0.147155)) * -1, 3.33829), 16.7411)) * -1,
    74: lambda p: lt(rank(correlation(p.close, sum(p.adv(30), 37.4843), 15.1365)), rank(correlation(rank(p.high * 0.0261661 + p.vwap * (1 - 0.0261661)), rank(p.volume), 11.4791))) * -1,
    75: lambda p: lt(rank(correlation(p.vwap, p.volume, 4.24304)), rank(correlation(rank(p.low), rank(p.adv(50)), 12.4413))),
    76: lambda p: max(rank(decay_linear(delta(p.vwap, 1.24383), 11.8259)), ts_rank(decay_linear(ts_rank(correlation(indneutralize(p.low, 'sector'), p.adv(81), 8.14941), 19.569), 17.1543), 19.383)) * -1,
    77: lambda p: min(rank(decay_linear(((p.high + p.low) / 2 + p.high) - (p.vwap + p.high), 20.0451)), rank(decay_linear(correlation((p.high + p.low) / 2, p.adv(40), 3.1614), 5.64125))),
    78: lambda p: power(rank(correlation(sum(p.low * 0.352233 + p.vwap * (1 - 0.352233), 19.7428), sum(p.adv(40), 19.7428), 6.83313)), rank(correlation(rank(p.vwap), rank(p.volume), 5.77492))),
    79: lambda p: lt(rank(delta(indneutralize(p.close * 0.60733 + p.open * (1 - 0.60733), 'sector'), 1.23438)), rank(correlation(ts_rank(p.vwap, 3.60973), ts_rank(p.adv(150), 9.18637), 14.6644))),
    80: lambda p: power(rank(sign(delta(indneutralize(p.open * 0.868128 + p.high * (1 - 0.868128), 'industry'), 4.04545))), ts_rank(correlation(p.high, p.adv(10), 5.11456), 5.53756)) * -1,
    81: lambda p: lt(rank(log(product(rank(power(rank(correlation(p.vwap, sum(p.adv(10), 49.6054), 8.47743)), 4)), 14.9655))), rank(correlation(rank(p.vwap), rank(p.volume), 5.07914))) * -1,
    82: lambda p: min(rank(decay_linear(delta(p.open, 1.46063), 14.8717)), ts_rank(decay_linear(correlation(indneutralize(p.volume, 'sector'), p.open * 0.634196 + p.open * (1 - 0.634196), 17.4842), 6.92131), 13.4283)) * -1,
    83: lambda p: rank(delay((p.high - p.low) / (sum(p.close, 5) / 5), 2)) * rank(rank(p.volume)) / (((p.high - p.low) / (sum(p.close, 5) / 5)) / (p.vwap - p.close)),
    84: lambda p: signedpower(ts_rank(p.vwap - ts_max(p.vwap, 15.3217), 20.7127), delta(p.close, 4.96796)),
    85: lambda p: power(rank(correlation(p.high * 0.876703 + p.close * (1 - 0.876703), p.adv(30), 9.61331)), rank(correlation(ts_rank((p.high + p.low) / 2, 3.70596), ts_rank(p.volume, 10.1595), 7.11408))),
    86: lambda p: lt(ts_rank(correlation(p.close, sum(p.adv(20), 14.7444), 6.00049), 20.4195), rank((p.open + p.close) - (p.vwap + p.open))) * -1,
    87: lambda p: max(rank(decay_linear(delta(p.close * 0.369701 + p.vwap * (1 - 0.369701), 1.91233), 2.65461)), ts_rank(decay_linear(abs(correlation(indneutralize(p.adv(81), 'industry'), p.close, 13.4132)), 4.89768), 14.4535)) * -1,
    88: lambda p: min(rank(decay_linear((rank(p.open) + rank(p.low)) - (rank(p.high) + rank(p.close)), 8.06882)), ts_rank(decay_linear(correlation(ts_rank(p.close, 8.44728), ts_rank(p.adv(60), 20.6966), 8.01266), 6.65053), 2.61957)),
    89: lambda p: ts_rank(decay_linear(correlation(p.low * 0.967285 + p.low * (1 - 0.967285), p.adv(10), 6.94279), 5.51607), 3.79744) - ts_rank(decay_linear(delta(indneutralize(p.vwap, 'industry'), 3.48158), 10.1466), 15.3012),
    90: lambda p: power(rank(p.close - ts_max(p.close, 4.66719)), ts_rank(correlation(indneutralize(p.adv(40), 'subindustry'), p.low, 5.38375), 3.21856)) * -1,
    91: lambda p: (ts_rank(decay_linear(decay_linear(correlation(indneutralize(p.close, 'industry'), p.volume, 9.74928), 16.398), 3.83219), 4.8667) - rank(decay_linear(correlation(p.vwap, p.adv(30), 4.01303), 2.6809))) * -1,
    92: lambda p: min(ts_rank(decay_linear(lt((p.high + p.low) / 2 + p.close, p.low + p.open), 14.7221), 18.8683), ts_rank(decay_linear(correlation(rank(p.low), rank(p.adv(30)), 7.58555), 6.94024), 6.80584)),
    93: lambda p: ts_rank(decay_linear(correlation(indneutralize(p.vwap, 'industry'), p.adv(81), 17.4193), 19.848), 7.54455) / rank(decay_linear(delta(p.close * 0.524434 + p.vwap * (1 - 0.524434), 2.77377), 16.2664)),
    94: lambda p: power(rank(p.vwap - ts_min(p.vwap, 11.5783)), ts_rank(correlation(ts_rank(p.vwap, 19.6462), ts_rank(p.adv(60), 4.02992), 18.0926), 2.70756)) * -1,
    95: lambda p: lt(rank(p.open - ts_min(p.open, 12.4105)), ts_rank(power(rank(correlation(sum((p.high + p.low) / 2, 19.1351), sum(p.adv(40), 19.1351), 12.8742)), 5), 11.7584)),
    96: lambda p: max(ts_rank(decay_linear(correlation(rank(p.vwap), rank(p.volume), 3.83878), 4.16783), 8.38151), ts_rank(decay_linear(ts_argmax(correlation(ts_rank(p.close, 7.45404), ts_rank(p.adv(60), 4.13242), 3.65459), 12.6556), 14.0365), 13.4143)) * -1,
    97: lambda p: (rank(decay_linear(delta(indneutralize(p.low * 0.721001 + p.vwap * (1 - 0.721001), 'industry'), 3.3705), 20.4523)) - ts_rank(decay_linear(ts_rank(correlation(ts_rank(p.low, 7.87871), ts_rank(p.adv(60), 17.255), 4.97547), 18.5925), 15.7152), 6.71659)) * -1,
    98: lambda p: rank(decay_linear(correlation(p.vwap, sum(p.adv(5), 26.4719), 4.58418), 7.18088)) - rank(decay_linear(ts_rank(ts_argmin(correlation(rank(p.open), rank(p.adv(15)), 20.8187), 8.62571), 6.95668), 8.07206)),
    99: lambda p: lt(rank(correlation(sum((p.high + p.low) / 2, 19.8975), sum(p.adv(60), 19.8975), 8.8136)), rank(correlation(p.low, p.volume, 6.28259))) * -1,
    100: lambda p: 0 - (1 * ((1.5 * scale(indneutralize(indneutralize(rank(((p.close - p.low) - (p.high - p.close)) / (p.high - p.low) * p.volume), 'subindustry'), 'subindustry')) - scale(indneutralize(correlation(p.close, rank(p.adv(20)), 5) - rank(ts_argmin(p.close, 30)), 'subindustry'))) * (p.volume / p.adv(20)))),
    101: lambda p: (p.close - p.open) / ((p.high - p.low) + .001),
}


def _callable(number):
    translation = TRANSLATIONS[number]

    @wraps(translation)
    def compute(fields):
        if number == 56:
            raise ValueError('needs cap')
        with np.errstate(all='ignore'):
            result = translation(Inputs(fields))
        base = fields['close']
        if (not isinstance(result, pd.DataFrame) or not result.index.equals(base.index)
                or not result.columns.equals(base.columns)):
            raise ValueError(f'Alpha#{number}: result shape/axes mismatch')
        return finite(result.astype(float))

    compute.__name__ = f'alpha{number:03}'
    return compute


def _uses(formula):
    words = set(re.findall(r'\b[a-z]+\d*\b', formula.lower()))
    fields = words & {'open', 'high', 'low', 'close', 'volume', 'vwap', 'cap'}
    if 'returns' in words:
        fields.add('close')
    if any(re.fullmatch(r'adv\d+', word) for word in words):
        fields.update(('volume', 'vwap'))
    return fields


# Verbatim expression text after "Alpha#N: " in page1.md, appendix I.
FORMULAS = {
    1: '(rank(Ts_ArgMax(SignedPower(((returns < 0) ? stddev(returns, 20) : close), 2.), 5)) - 0.5) ',
    2: '(-1 * correlation(rank(delta(log(volume), 2)), rank(((close - open) / open)), 6)) ',
    3: '(-1 * correlation(rank(open), rank(volume), 10)) ',
    4: '(-1 * Ts_Rank(rank(low), 9)) ',
    5: '(rank((open - (sum(vwap, 10) / 10))) * (-1 * abs(rank((close - vwap))))) ',
    6: '(-1 * correlation(open, volume, 10)) ',
    7: '((adv20 < volume) ? ((-1 * ts_rank(abs(delta(close, 7)), 60)) * sign(delta(close, 7))) : (-1 * 1)) ',
    8: '(-1 * rank(((sum(open, 5) * sum(returns, 5)) - delay((sum(open, 5) * sum(returns, 5)), 10)))) ',
    9: '((0 < ts_min(delta(close, 1), 5)) ? delta(close, 1) : ((ts_max(delta(close, 1), 5) < 0) ? delta(close, 1) : (-1 * delta(close, 1)))) ',
    10: 'rank(((0 < ts_min(delta(close, 1), 4)) ? delta(close, 1) : ((ts_max(delta(close, 1), 4) < 0) ? delta(close, 1) : (-1 * delta(close, 1))))) ',
    11: '((rank(ts_max((vwap - close), 3)) + rank(ts_min((vwap - close), 3))) * rank(delta(volume, 3))) ',
    12: '(sign(delta(volume, 1)) * (-1 * delta(close, 1))) ',
    13: '(-1 * rank(covariance(rank(close), rank(volume), 5))) ',
    14: '((-1 * rank(delta(returns, 3))) * correlation(open, volume, 10)) ',
    15: '(-1 * sum(rank(correlation(rank(high), rank(volume), 3)), 3)) ',
    16: '(-1 * rank(covariance(rank(high), rank(volume), 5))) ',
    17: '(((-1 * rank(ts_rank(close, 10))) * rank(delta(delta(close, 1), 1))) * rank(ts_rank((volume / adv20), 5))) ',
    18: '(-1 * rank(((stddev(abs((close - open)), 5) + (close - open)) + correlation(close, open, 10)))) ',
    19: '((-1 * sign(((close - delay(close, 7)) + delta(close, 7)))) * (1 + rank((1 + sum(returns, 250))))) ',
    20: '(((-1 * rank((open - delay(high, 1)))) * rank((open - delay(close, 1)))) * rank((open - delay(low, 1)))) ',
    21: '((((sum(close, 8) / 8) + stddev(close, 8)) < (sum(close, 2) / 2)) ? (-1 * 1) : (((sum(close, 2) / 2) < ((sum(close, 8) / 8) - stddev(close, 8))) ? 1 : (((1 < (volume / adv20)) || ((volume / adv20) == 1)) ? 1 : (-1 * 1)))) ',
    22: '(-1 * (delta(correlation(high, volume, 5), 5) * rank(stddev(close, 20)))) ',
    23: '(((sum(high, 20) / 20) < high) ? (-1 * delta(high, 2)) : 0) ',
    24: '((((delta((sum(close, 100) / 100), 100) / delay(close, 100)) < 0.05) || ((delta((sum(close, 100) / 100), 100) / delay(close, 100)) == 0.05)) ? (-1 * (close - ts_min(close, 100))) : (-1 * delta(close, 3))) ',
    25: 'rank(((((-1 * returns) * adv20) * vwap) * (high - close))) ',
    26: '(-1 * ts_max(correlation(ts_rank(volume, 5), ts_rank(high, 5), 5), 3)) ',
    27: '((0.5 < rank((sum(correlation(rank(volume), rank(vwap), 6), 2) / 2.0))) ? (-1 * 1) : 1) ',
    28: 'scale(((correlation(adv20, low, 5) + ((high + low) / 2)) - close)) ',
    29: '(min(product(rank(rank(scale(log(sum(ts_min(rank(rank((-1 * rank(delta((close - 1), 5))))), 2), 1))))), 1), 5) + ts_rank(delay((-1 * returns), 6), 5)) ',
    30: '(((1.0 - rank(((sign((close - delay(close, 1))) + sign((delay(close, 1) - delay(close, 2)))) + sign((delay(close, 2) - delay(close, 3)))))) * sum(volume, 5)) / sum(volume, 20)) ',
    31: '((rank(rank(rank(decay_linear((-1 * rank(rank(delta(close, 10)))), 10)))) + rank((-1 * delta(close, 3)))) + sign(scale(correlation(adv20, low, 12)))) ',
    32: '(scale(((sum(close, 7) / 7) - close)) + (20 * scale(correlation(vwap, delay(close, 5), 230)))) ',
    33: 'rank((-1 * ((1 - (open / close))^1))) ',
    34: 'rank(((1 - rank((stddev(returns, 2) / stddev(returns, 5)))) + (1 - rank(delta(close, 1)))))',
    35: '((Ts_Rank(volume, 32) * (1 - Ts_Rank(((close + high) - low), 16))) * (1 - Ts_Rank(returns, 32))) ',
    36: '(((((2.21 * rank(correlation((close - open), delay(volume, 1), 15))) + (0.7 * rank((open - close)))) + (0.73 * rank(Ts_Rank(delay((-1 * returns), 6), 5)))) + rank(abs(correlation(vwap, adv20, 6)))) + (0.6 * rank((((sum(close, 200) / 200) - open) * (close - open))))) ',
    37: '(rank(correlation(delay((open - close), 1), close, 200)) + rank((open - close))) ',
    38: '((-1 * rank(Ts_Rank(close, 10))) * rank((close / open))) ',
    39: '((-1 * rank((delta(close, 7) * (1 - rank(decay_linear((volume / adv20), 9)))))) * (1 + rank(sum(returns, 250)))) ',
    40: '((-1 * rank(stddev(high, 10))) * correlation(high, volume, 10)) ',
    41: '(((high * low)^0.5) - vwap) ',
    42: '(rank((vwap - close)) / rank((vwap + close))) ',
    43: '(ts_rank((volume / adv20), 20) * ts_rank((-1 * delta(close, 7)), 8))',
    44: '(-1 * correlation(high, rank(volume), 5)) ',
    45: '(-1 * ((rank((sum(delay(close, 5), 20) / 20)) * correlation(close, volume, 2)) * rank(correlation(sum(close, 5), sum(close, 20), 2)))) ',
    46: '((0.25 < (((delay(close, 20) - delay(close, 10)) / 10) - ((delay(close, 10) - close) / 10))) ? (-1 * 1) : (((((delay(close, 20) - delay(close, 10)) / 10) - ((delay(close, 10) - close) / 10)) < 0) ? 1 : ((-1 * 1) * (close - delay(close, 1))))) ',
    47: '((((rank((1 / close)) * volume) / adv20) * ((high * rank((high - close))) / (sum(high, 5) / 5))) - rank((vwap - delay(vwap, 5)))) ',
    48: '(indneutralize(((correlation(delta(close, 1), delta(delay(close, 1), 1), 250) * delta(close, 1)) / close), IndClass.subindustry) / sum(((delta(close, 1) / delay(close, 1))^2), 250)) ',
    49: '(((((delay(close, 20) - delay(close, 10)) / 10) - ((delay(close, 10) - close) / 10)) < (-1 * 0.1)) ? 1 : ((-1 * 1) * (close - delay(close, 1)))) ',
    50: '(-1 * ts_max(rank(correlation(rank(volume), rank(vwap), 5)), 5)) ',
    51: '(((((delay(close, 20) - delay(close, 10)) / 10) - ((delay(close, 10) - close) / 10)) < (-1 * 0.05)) ? 1 : ((-1 * 1) * (close - delay(close, 1)))) ',
    52: '((((-1 * ts_min(low, 5)) + delay(ts_min(low, 5), 5)) * rank(((sum(returns, 240) - sum(returns, 20)) / 220))) * ts_rank(volume, 5)) ',
    53: '(-1 * delta((((close - low) - (high - close)) / (close - low)), 9)) ',
    54: '((-1 * ((low - close) * (open^5))) / ((low - high) * (close^5))) ',
    55: '(-1 * correlation(rank(((close - ts_min(low, 12)) / (ts_max(high, 12) - ts_min(low, 12)))), rank(volume), 6)) ',
    56: '(0 - (1 * (rank((sum(returns, 10) / sum(sum(returns, 2), 3))) * rank((returns * cap))))) ',
    57: '(0 - (1 * ((close - vwap) / decay_linear(rank(ts_argmax(close, 30)), 2)))) ',
    58: '(-1 * Ts_Rank(decay_linear(correlation(IndNeutralize(vwap, IndClass.sector), volume, 3.92795), 7.89291), 5.50322)) ',
    59: '(-1 * Ts_Rank(decay_linear(correlation(IndNeutralize(((vwap * 0.728317) + (vwap * (1 - 0.728317))), IndClass.industry), volume, 4.25197), 16.2289), 8.19648)) ',
    60: '(0 - (1 * ((2 * scale(rank(((((close - low) - (high - close)) / (high - low)) * volume)))) - scale(rank(ts_argmax(close, 10))))))',
    61: '(rank((vwap - ts_min(vwap, 16.1219))) < rank(correlation(vwap, adv180, 17.9282))) ',
    62: '((rank(correlation(vwap, sum(adv20, 22.4101), 9.91009)) < rank(((rank(open) + rank(open)) < (rank(((high + low) / 2)) + rank(high))))) * -1) ',
    63: '((rank(decay_linear(delta(IndNeutralize(close, IndClass.industry), 2.25164), 8.22237)) - rank(decay_linear(correlation(((vwap * 0.318108) + (open * (1 - 0.318108))), sum(adv180, 37.2467), 13.557), 12.2883))) * -1) ',
    64: '((rank(correlation(sum(((open * 0.178404) + (low * (1 - 0.178404))), 12.7054), sum(adv120, 12.7054), 16.6208)) < rank(delta(((((high + low) / 2) * 0.178404) + (vwap * (1 - 0.178404))), 3.69741))) * -1) ',
    65: '((rank(correlation(((open * 0.00817205) + (vwap * (1 - 0.00817205))), sum(adv60, 8.6911), 6.40374)) < rank((open - ts_min(open, 13.635)))) * -1) ',
    66: '((rank(decay_linear(delta(vwap, 3.51013), 7.23052)) + Ts_Rank(decay_linear(((((low * 0.96633) + (low * (1 - 0.96633))) - vwap) / (open - ((high + low) / 2))), 11.4157), 6.72611)) * -1) ',
    67: '((rank((high - ts_min(high, 2.14593)))^rank(correlation(IndNeutralize(vwap, IndClass.sector), IndNeutralize(adv20, IndClass.subindustry), 6.02936))) * -1) ',
    68: '((Ts_Rank(correlation(rank(high), rank(adv15), 8.91644), 13.9333) < rank(delta(((close * 0.518371) + (low * (1 - 0.518371))), 1.06157))) * -1) ',
    69: '((rank(ts_max(delta(IndNeutralize(vwap, IndClass.industry), 2.72412), 4.79344))^Ts_Rank(correlation(((close * 0.490655) + (vwap * (1 - 0.490655))), adv20, 4.92416), 9.0615)) * -1) ',
    70: '((rank(delta(vwap, 1.29456))^Ts_Rank(correlation(IndNeutralize(close, IndClass.industry), adv50, 17.8256), 17.9171)) * -1) ',
    71: 'max(Ts_Rank(decay_linear(correlation(Ts_Rank(close, 3.43976), Ts_Rank(adv180, 12.0647), 18.0175), 4.20501), 15.6948), Ts_Rank(decay_linear((rank(((low + open) - (vwap + vwap)))^2), 16.4662), 4.4388)) ',
    72: '(rank(decay_linear(correlation(((high + low) / 2), adv40, 8.93345), 10.1519)) / rank(decay_linear(correlation(Ts_Rank(vwap, 3.72469), Ts_Rank(volume, 18.5188), 6.86671), 2.95011))) ',
    73: '(max(rank(decay_linear(delta(vwap, 4.72775), 2.91864)), Ts_Rank(decay_linear(((delta(((open * 0.147155) + (low * (1 - 0.147155))), 2.03608) / ((open * 0.147155) + (low * (1 - 0.147155)))) * -1), 3.33829), 16.7411)) * -1) ',
    74: '((rank(correlation(close, sum(adv30, 37.4843), 15.1365)) < rank(correlation(rank(((high * 0.0261661) + (vwap * (1 - 0.0261661)))), rank(volume), 11.4791))) * -1) ',
    75: '(rank(correlation(vwap, volume, 4.24304)) < rank(correlation(rank(low), rank(adv50), 12.4413))) ',
    76: '(max(rank(decay_linear(delta(vwap, 1.24383), 11.8259)), Ts_Rank(decay_linear(Ts_Rank(correlation(IndNeutralize(low, IndClass.sector), adv81, 8.14941), 19.569), 17.1543), 19.383)) * -1) ',
    77: 'min(rank(decay_linear(((((high + low) / 2) + high) - (vwap + high)), 20.0451)), rank(decay_linear(correlation(((high + low) / 2), adv40, 3.1614), 5.64125))) ',
    78: '(rank(correlation(sum(((low * 0.352233) + (vwap * (1 - 0.352233))), 19.7428), sum(adv40, 19.7428), 6.83313))^rank(correlation(rank(vwap), rank(volume), 5.77492))) ',
    79: '(rank(delta(IndNeutralize(((close * 0.60733) + (open * (1 - 0.60733))), IndClass.sector), 1.23438)) < rank(correlation(Ts_Rank(vwap, 3.60973), Ts_Rank(adv150, 9.18637), 14.6644))) ',
    80: '((rank(Sign(delta(IndNeutralize(((open * 0.868128) + (high * (1 - 0.868128))), IndClass.industry), 4.04545)))^Ts_Rank(correlation(high, adv10, 5.11456), 5.53756)) * -1) ',
    81: '((rank(Log(product(rank((rank(correlation(vwap, sum(adv10, 49.6054), 8.47743))^4)), 14.9655))) < rank(correlation(rank(vwap), rank(volume), 5.07914))) * -1) ',
    82: '(min(rank(decay_linear(delta(open, 1.46063), 14.8717)), Ts_Rank(decay_linear(correlation(IndNeutralize(volume, IndClass.sector), ((open * 0.634196) + (open * (1 - 0.634196))), 17.4842), 6.92131), 13.4283)) * -1) ',
    83: '((rank(delay(((high - low) / (sum(close, 5) / 5)), 2)) * rank(rank(volume))) / (((high - low) / (sum(close, 5) / 5)) / (vwap - close))) ',
    84: 'SignedPower(Ts_Rank((vwap - ts_max(vwap, 15.3217)), 20.7127), delta(close, 4.96796)) ',
    85: '(rank(correlation(((high * 0.876703) + (close * (1 - 0.876703))), adv30, 9.61331))^rank(correlation(Ts_Rank(((high + low) / 2), 3.70596), Ts_Rank(volume, 10.1595), 7.11408))) ',
    86: '((Ts_Rank(correlation(close, sum(adv20, 14.7444), 6.00049), 20.4195) < rank(((open + close) - (vwap + open)))) * -1) ',
    87: '(max(rank(decay_linear(delta(((close * 0.369701) + (vwap * (1 - 0.369701))), 1.91233), 2.65461)), Ts_Rank(decay_linear(abs(correlation(IndNeutralize(adv81, IndClass.industry), close, 13.4132)), 4.89768), 14.4535)) * -1) ',
    88: 'min(rank(decay_linear(((rank(open) + rank(low)) - (rank(high) + rank(close))), 8.06882)), Ts_Rank(decay_linear(correlation(Ts_Rank(close, 8.44728), Ts_Rank(adv60, 20.6966), 8.01266), 6.65053), 2.61957)) ',
    89: '(Ts_Rank(decay_linear(correlation(((low * 0.967285) + (low * (1 - 0.967285))), adv10, 6.94279), 5.51607), 3.79744) - Ts_Rank(decay_linear(delta(IndNeutralize(vwap, IndClass.industry), 3.48158), 10.1466), 15.3012)) ',
    90: '((rank((close - ts_max(close, 4.66719)))^Ts_Rank(correlation(IndNeutralize(adv40, IndClass.subindustry), low, 5.38375), 3.21856)) * -1) ',
    91: '((Ts_Rank(decay_linear(decay_linear(correlation(IndNeutralize(close, IndClass.industry), volume, 9.74928), 16.398), 3.83219), 4.8667) - rank(decay_linear(correlation(vwap, adv30, 4.01303), 2.6809))) * -1) ',
    92: 'min(Ts_Rank(decay_linear(((((high + low) / 2) + close) < (low + open)), 14.7221), 18.8683), Ts_Rank(decay_linear(correlation(rank(low), rank(adv30), 7.58555), 6.94024), 6.80584)) ',
    93: '(Ts_Rank(decay_linear(correlation(IndNeutralize(vwap, IndClass.industry), adv81, 17.4193), 19.848), 7.54455) / rank(decay_linear(delta(((close * 0.524434) + (vwap * (1 - 0.524434))), 2.77377), 16.2664))) ',
    94: '((rank((vwap - ts_min(vwap, 11.5783)))^Ts_Rank(correlation(Ts_Rank(vwap, 19.6462), Ts_Rank(adv60, 4.02992), 18.0926), 2.70756)) * -1) ',
    95: '(rank((open - ts_min(open, 12.4105))) < Ts_Rank((rank(correlation(sum(((high + low) / 2), 19.1351), sum(adv40, 19.1351), 12.8742))^5), 11.7584)) ',
    96: '(max(Ts_Rank(decay_linear(correlation(rank(vwap), rank(volume), 3.83878), 4.16783), 8.38151), Ts_Rank(decay_linear(Ts_ArgMax(correlation(Ts_Rank(close, 7.45404), Ts_Rank(adv60, 4.13242), 3.65459), 12.6556), 14.0365), 13.4143)) * -1) ',
    97: '((rank(decay_linear(delta(IndNeutralize(((low * 0.721001) + (vwap * (1 - 0.721001))), IndClass.industry), 3.3705), 20.4523)) - Ts_Rank(decay_linear(Ts_Rank(correlation(Ts_Rank(low, 7.87871), Ts_Rank(adv60, 17.255), 4.97547), 18.5925), 15.7152), 6.71659)) * -1) ',
    98: '(rank(decay_linear(correlation(vwap, sum(adv5, 26.4719), 4.58418), 7.18088)) - rank(decay_linear(Ts_Rank(Ts_ArgMin(correlation(rank(open), rank(adv15), 20.8187), 8.62571), 6.95668), 8.07206))) ',
    99: '((rank(correlation(sum(((high + low) / 2), 19.8975), sum(adv60, 19.8975), 8.8136)) < rank(correlation(low, volume, 6.28259))) * -1) ',
    100: '(0 - (1 * (((1.5 * scale(indneutralize(indneutralize(rank(((((close - low) - (high - close)) / (high - low)) * volume)), IndClass.subindustry), IndClass.subindustry))) - scale(indneutralize((correlation(close, rank(adv20), 5) - rank(ts_argmin(close, 30))), IndClass.subindustry))) * (volume / adv20)))) ',
    101: '((close - open) / ((high - low) + .001)) ',
}

ALPHAS: dict[int, dict] = {
    n: dict(formula=formula, fn=_callable(n), uses=_uses(formula),
            indneutral="indneutralize" in formula.lower(),
            skipped="needs cap" if n == 56 else None)
    for n, formula in FORMULAS.items()
}
