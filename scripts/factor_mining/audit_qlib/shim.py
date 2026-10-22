"""Stub qlib's two Cython modules with numpy ports of their OLS math (full-window, NaN-skipping)."""
import sys, types
import numpy as np

def _ols(y, N, kind):
    out = np.full(len(y), np.nan)
    for t in range(len(y)):
        lo = max(0, t - N + 1)
        w = y[lo:t + 1]
        n_pad = N - len(w)                      # qlib pads the left with NaN
        x = np.arange(1 + n_pad, N + 1, dtype=float)
        m = ~np.isnan(w)
        if m.sum() < 1: continue
        xs, ys = x[m], w[m]
        k = len(xs)
        den = k * (xs * xs).sum() - xs.sum() ** 2
        if den == 0: continue
        slope = (k * (xs * ys).sum() - xs.sum() * ys.sum()) / den
        if kind == 'slope': out[t] = slope
        elif kind == 'resi':
            icpt = ys.mean() - slope * xs.mean()
            out[t] = w[-1] - (slope * N + icpt)
        else:
            ss_y = ((ys - ys.mean()) ** 2).sum()
            if ss_y == 0: continue
            out[t] = slope ** 2 * ((xs - xs.mean()) ** 2).sum() / ss_y
    return out

def install():
    r = types.ModuleType('qlib.data._libs.rolling')
    r.rolling_slope = lambda a, n: _ols(a, n, 'slope')
    r.rolling_rsquare = lambda a, n: _ols(a, n, 'rsq')
    r.rolling_resi = lambda a, n: _ols(a, n, 'resi')
    e = types.ModuleType('qlib.data._libs.expanding')
    for nm in ('slope', 'rsquare', 'resi'):
        setattr(e, 'expanding_' + nm, lambda a, nm=nm: _ols(a, len(a), {'slope':'slope','rsquare':'rsq','resi':'resi'}[nm]))
    sys.modules['qlib.data._libs.rolling'] = r
    sys.modules['qlib.data._libs.expanding'] = e
