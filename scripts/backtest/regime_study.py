"""离线市场形态研究：预注册、训练筛选、单次保留窗评估；不模拟交易。

python -m scripts.backtest.regime_study --help
See docs/research/regime-statistics-preregistration.md before running real data.
Only numpy/pandas and the standard library are required.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
from pathlib import Path
import time

import numpy as np
import pandas as pd

from scripts.backtest.quant_audit import grid_around, sweep_plateau, time_split

# PRE-REGISTERED, before any real-data target evaluation. Do not tune on holdout.
PROTOCOL_VERSION = "2026-10-24-v3"  # null calibration completed before real-data tests
TRAIN_FRACTION = 0.60
FDR = 0.10                         # one BH family across BOTH crypto frequencies
HOLDOUT_T = 2.0
NEIGHBOR_T = 1.5
PLATEAU_RATIO = 0.60
WINDOWS = {"1h": 96, "1d": 40}
FACTORS = (0.5, 0.75, 1.0, 1.25, 1.5)
HORIZONS = {"1h": (4, 24, 72), "1d": (1, 3, 7)}
MIN_HISTORY_DAYS = 60
MIN_EXPANDING = 60
MIN_CROSS_SECTION = 10
MIN_OBSERVATIONS = 200
MIN_CLUSTERS = 40
MIN_BUCKET = 30
CRYPTO_COST = 0.0015               # round trip, deducted per signal payoff, not orders
US_COST = 0.0005
MIN_INCREMENTAL_R2 = 0.005         # 0.5 percentage points
MIN_TAIL_LIFT = 1.30
MIN_TAIL_INCREMENT = 0.01
TAIL_SIGMA = 3.0
DRAWDOWN_ATR = 3.0
JUMP_SIGMA = 4.0
SQUEEZE_PERCENTILE = 0.20
VOLATILITY_BINS = 20              # frozen training quantiles; categorical logit baseline
MIN_HAC_DAYS = 22                  # monthly bandwidth, calibrated on IID controls only
HAC_OVERLAP_MULTIPLIER = 4         # Bartlett bandwidth >=4 overlapping horizons
WINSOR_QUANTILES = (0.005, 0.995)  # fit on train only, only continuous T2 regressors
ER_TREND, ER_RANGE = 0.35, 0.20
DATA_ROOT = Path("/Users/zhangyun/workspace/nofx/data")
DEFAULT_OUT = DATA_ROOT / "regime_study/out"
FEATURES = (
    "er", "vr4", "vr8", "rho1", "hurst", "convexity_t", "acceleration", "stretch",
    "ewma_ratio", "arch1", "volvol", "bb_percentile", "squeeze_duration", "breakout",
    "kurtosis", "tail_frequency", "jump_share", "last_jump", "bars_since_jump",
    "wick_lower", "wick_upper", "median_cross", "upper_touches", "lower_touches",
    "donchian_atr", "drawdown", "rebound", "recovery_speed",
)
UNTESTABLE = ("BID-ASK BOUNCE", "ORDER BOOK")
KEY = ["market", "timeframe", "feature", "window", "horizon", "target", "method"]


def window_grid(timeframe):
    return [p["n"] for p in grid_around({"n": WINDOWS[timeframe]}, FACTORS).values()]


def efficiency_ratio(close: pd.Series, n: int) -> pd.Series:
    return close.diff(n).abs() / close.diff().abs().rolling(n).sum().replace(0, np.nan)


def variance_ratio(returns: pd.Series, n: int, q: int) -> pd.DataFrame:
    """Overlapping Lo–MacKinlay VR and its standard homoscedastic asymptotic z.

    q-sums all lie within the SAME n one-bar returns. Finite-sample denominator
    m=q*(n-q+1)*(1-q/n); sample variance of one-bar returns uses n-1.
    """
    if n <= q:
        raise ValueError("n must exceed q")
    mu = returns.rolling(n).mean()
    qsum = returns.rolling(q).sum()
    k = n - q + 1
    numerator = (qsum.pow(2).rolling(k).sum()
                 - 2*q*mu*qsum.rolling(k).sum() + k*(q*mu).pow(2))
    denom = q*k*(1-q/n)*returns.rolling(n).var()
    vr = numerator.clip(lower=0) / denom.replace(0, np.nan)
    theta = 2*(2*q-1)*(q-1)/(3*q*n)
    return pd.DataFrame({"vr": vr, "z": (vr-1)/math.sqrt(theta)})


def jump_share(returns: pd.Series, n: int) -> pd.Series:
    rv = returns.pow(2).rolling(n).sum()
    bv = (math.pi/2)*(n/(n-1))*(returns.abs()*returns.shift().abs()).rolling(n-1).sum()
    return ((rv-bv)/rv.replace(0, np.nan)).clip(0, 1)


def _rolling_dot(values, weights):
    result = np.full(len(values), np.nan)
    if len(values) >= len(weights):
        result[len(weights)-1:] = np.convolve(np.asarray(values), weights[::-1], "valid")
    return pd.Series(result, index=values.index)


def _duration(flag: pd.Series, valid: pd.Series) -> pd.Series:
    # Run-length computation, no Python loop over bars. Unknown history stays NaN.
    values = flag.fillna(False).to_numpy(dtype=bool)
    positions = np.arange(len(values))
    last_false = np.maximum.accumulate(np.where(~values, positions, -1))
    return pd.Series(np.where(values, positions-last_false, 0), index=flag.index).where(valid)


def compute_features(bars: pd.DataFrame, n: int) -> pd.DataFrame:
    """Pure causal functions of OHLCV through the current CLOSED bar.

    vr*_z and state are diagnostics; all FEATURES are independently truncation tested.
    q variances use a common n-return support. No future breakout is a feature.
    """
    c, hi, lo, op = (bars[k].astype(float) for k in ("close", "high", "low", "open"))
    lp = np.log(c)
    r = lp.diff()
    sigma = r.ewm(span=n, adjust=False, min_periods=n).std()
    short = r.ewm(span=max(4, n//4), adjust=False, min_periods=max(4, n//4)).std()
    tr = pd.concat([hi-lo, (hi-c.shift()).abs(), (lo-c.shift()).abs()], axis=1).max(axis=1)
    atr = tr.rolling(14).mean().replace(0, np.nan)
    f = pd.DataFrame(index=c.index)
    f["er"] = efficiency_ratio(c, n)
    for q in (4, 8):
        vr = variance_ratio(r, n, q)
        f[f"vr{q}"], f[f"vr{q}_z"] = vr["vr"], vr["z"]
    f["rho1"] = r.rolling(n-1).corr(r.shift())
    logq = np.log(np.array([1, 2, 4, 8], dtype=float))
    logq -= logq.mean()
    logvars = np.array([np.log(r.rolling(q).sum().rolling(n-q+1).var().clip(lower=1e-30))
                        for q in (1, 2, 4, 8)])
    f["hurst"] = (logq[:, None]*logvars).sum(axis=0)/(2*(logq@logq))
    x = np.linspace(-1, 1, n)
    quad = x*x - (x*x).mean()  # orthogonal to both intercept and linear component
    sy = lp.rolling(n).sum()
    sx, sq = _rolling_dot(lp, x), _rolling_dot(lp, quad)
    sse = (lp.pow(2).rolling(n).sum() - sy*sy/n - sx*sx/(x@x) - sq*sq/(quad@quad))
    f["convexity_t"] = sq/np.sqrt(sse.clip(lower=1e-20)*(quad@quad)/(n-3))
    half = n//2
    f["acceleration"] = (lp-lp.shift(half)) - (lp.shift(half)-lp.shift(2*half))
    ema = lp.ewm(span=n, adjust=False, min_periods=n).mean()
    f["stretch"] = (lp-ema)/(sigma*math.sqrt(n)).replace(0, np.nan)
    f["ewma_ratio"] = short/sigma.replace(0, np.nan)
    f["arch1"] = r.pow(2).rolling(n-1).corr(r.shift().pow(2))
    f["volvol"] = sigma.rolling(n).std()/sigma.rolling(n).mean().replace(0, np.nan)
    width = 4*c.rolling(n).std()/c.rolling(n).mean()
    # Rolling percentile rank is strictly point-in-time; not a full-sample percentile.
    f["bb_percentile"] = width.rolling(n).rank(pct=True)
    squeezed = f["bb_percentile"].le(SQUEEZE_PERCENTILE)
    f["squeeze_duration"] = _duration(squeezed, f["bb_percentile"].notna())
    f["breakout"] = (r.abs()/sigma.shift().replace(0, np.nan))*squeezed.shift(fill_value=False).astype(float)
    f["kurtosis"] = r.rolling(n).kurt()
    robust = r.abs().rolling(n).median()/0.6744897501960817
    tail = r.abs().gt(TAIL_SIGMA*robust).astype(float).where(robust.notna())
    f["tail_frequency"] = tail.rolling(n).mean()
    f["jump_share"] = jump_share(r, n)
    jump = r.abs().gt(JUMP_SIGMA*sigma.shift())
    f["last_jump"] = jump.astype(float).where(sigma.shift().notna())
    idx = np.arange(len(r))
    last = np.maximum.accumulate(np.where(jump, idx, -1))
    f["bars_since_jump"] = pd.Series(np.where(last >= 0, idx-last, np.nan), index=r.index)
    f["wick_lower"] = (pd.concat([op, c], axis=1).min(axis=1)-lo)/atr
    f["wick_upper"] = (hi-pd.concat([op, c], axis=1).max(axis=1))/atr
    upper, lower = hi.rolling(n).max(), lo.rolling(n).min()
    crossings, uptouches, lotouches = (np.full(len(c), np.nan) for _ in range(3))
    if len(c) >= n:
        price_windows = np.lib.stride_tricks.sliding_window_view(c.to_numpy(), n)
        side = np.sign(price_windows-c.rolling(n).median().to_numpy()[n-1:, None])
        crossings[n-1:] = np.sum(side[:, 1:]*side[:, :-1] < 0, axis=1)
        high_windows = np.lib.stride_tricks.sliding_window_view(hi.to_numpy(), n)
        low_windows = np.lib.stride_tricks.sliding_window_view(lo.to_numpy(), n)
        uptouches[n-1:] = np.sum(high_windows >= (upper-.25*atr).to_numpy()[n-1:, None], axis=1)
        lotouches[n-1:] = np.sum(low_windows <= (lower+.25*atr).to_numpy()[n-1:, None], axis=1)
    f["median_cross"], f["upper_touches"], f["lower_touches"] = crossings, uptouches, lotouches
    f["donchian_atr"] = (upper-lower)/atr
    drawdowns = np.full(len(c), np.nan)
    if len(c) >= n:
        price_windows = np.lib.stride_tricks.sliding_window_view(c.to_numpy(), n)
        peaks = np.maximum.accumulate(price_windows, axis=1)
        drawdowns[n-1:] = np.max(1-price_windows/peaks, axis=1)
    f["drawdown"] = drawdowns
    f["rebound"] = (c-lower)/(upper-lower).replace(0, np.nan)
    # Age of most recent low within n bars (argmax on reversed view; <=t only).
    lows = lo.to_numpy()
    age = np.full(len(c), np.nan)
    if len(c) >= n:
        view = np.lib.stride_tricks.sliding_window_view(lows, n)
        age[n-1:] = np.argmin(view[:, ::-1], axis=1)
    f["recovery_speed"] = np.log(c/lower)/(1+age)
    f["state"] = np.select([(f.er >= ER_TREND) & (f.vr4 > 1),
                             (f.er <= ER_RANGE) & (f.vr4 < 1)], [1., -1.], 0.)
    f["state"] = f["state"].where(f.er.notna() & f.vr4.notna())
    # No feature may bridge an OHLC gap, even one computed by EWM or boolean logic.
    complete = bars[["open", "high", "low", "close"]].notna().all(axis=1).rolling(2*n+1).sum().eq(2*n+1)
    return f.where(complete, np.nan).replace([np.inf, -np.inf], np.nan)


def expanding_terciles(values: pd.Series) -> pd.Series:
    past = values.shift().expanding(min_periods=MIN_EXPANDING)
    low, high = past.quantile(1/3), past.quantile(2/3)
    out = pd.Series(np.select([values <= low, values > high], [0, 2], 1), index=values.index)
    return out.where(values.notna() & low.notna(), -1).astype(np.int8)


def forward_targets(bars: pd.DataFrame, timeframe: str) -> dict:
    """Labels begin at t+1, with complete horizons; sigma/ATR thresholds freeze at t."""
    c = bars.close
    r = np.log(c).diff()
    bpd = 24 if timeframe == "1h" else 1
    spans = (24, 120, 528) if bpd == 24 else (1, 5, 22)
    controls = np.column_stack([np.log(r.pow(2).rolling(w).mean().clip(lower=1e-20)) for w in spans])
    sigma = r.ewm(span=24 if bpd == 24 else 22, adjust=False, min_periods=22).std()
    tr = pd.concat([bars.high-bars.low, (bars.high-c.shift()).abs(), (bars.low-c.shift()).abs()], axis=1).max(axis=1)
    atr = tr.rolling(14).mean()
    out = {"controls": controls, "sigma": sigma.to_numpy(), "atr_pct": (atr/c).to_numpy()}
    for h in HORIZONS[timeframe]:
        valid = r.notna().rolling(h).sum().shift(-h).eq(h)
        rv = r.pow(2).rolling(h).sum().shift(-h)
        maxr = r.abs().rolling(h).max().shift(-h)
        peak = c.to_numpy().copy()
        dd = np.zeros(len(c))
        for k in range(1, h+1):
            future = c.shift(-k).to_numpy()
            dd = np.maximum(dd, peak-future)
            peak = np.maximum(peak, future)
        out[h] = {
            "return": np.log(c.shift(-h)/c).where(valid).to_numpy(),
            "rv": np.log(rv.clip(lower=1e-20)).where(valid).to_numpy(),
            "tail": maxr.gt(TAIL_SIGMA*sigma).astype(float).where(valid & sigma.notna()).to_numpy(),
            "drawdown": pd.Series(dd, index=c.index).gt(DRAWDOWN_ATR*atr).astype(float).where(valid & atr.notna()).to_numpy(),
        }
    return out


def _beta_fraction(a, b, x):
    # Lentz continued fraction for regularized incomplete beta (no scipy dependency).
    tiny = 1e-300
    c, d = 1., 1-(a+b)*x/(a+1)
    d = 1/max(abs(d), tiny) * (1 if d >= 0 else -1)
    value = d
    for m in range(1, 201):
        for aa in (m*(b-m)*x/((a+2*m-1)*(a+2*m)),
                   -(a+m)*(a+b+m)*x/((a+2*m)*(a+2*m+1))):
            d = 1+aa*d
            if abs(d) < tiny:
                d = tiny
            c = 1+aa/c
            if abs(c) < tiny:
                c = tiny
            d = 1/d
            delta = d*c
            value *= delta
        if abs(delta-1) < 3e-14:
            break
    return value


def student_p(t, df):
    if not math.isfinite(t) or df <= 0:
        return 1.
    x, a, b = df/(df+t*t), df/2, .5
    if x >= 1:
        return 1.
    front = math.exp(math.lgamma(a+b)-math.lgamma(a)-math.lgamma(b)+a*math.log(x)+b*math.log1p(-x))
    if x < (a+1)/(a+b+2):
        return min(1., front*_beta_fraction(a, b, x)/a)
    return min(1., max(0., 1-front*_beta_fraction(b, a, 1-x)/b))


def bh_adjust(pvalues):
    p = np.nan_to_num(np.asarray(pvalues, float), nan=1., posinf=1.)
    order = np.argsort(p)
    adjusted = np.minimum.accumulate((p[order]*len(p)/np.arange(1, len(p)+1))[::-1])[::-1]
    result = np.empty(len(p))
    result[order] = np.clip(adjusted, 0, 1)
    return result


def clustered_inference(bread, scores, days, h, bpd, coefficient=-1, nobs=None):
    """Date-cluster sandwich plus HAC on date score sums; use the larger SE.

    days are actual integer UTC calendar days: weekends/gaps are NOT compressed.
    scores can be individual or grouped-binomial score rows, with identical result.
    """
    groups, inverse = np.unique(days, return_inverse=True)
    g, p = len(groups), scores.shape[1]
    if g < MIN_CLUSTERS:
        return None
    sums = np.zeros((int(groups[-1]-groups[0])+1, p))
    for j in range(p):
        sums[groups-groups[0], j] = np.bincount(inverse, weights=scores[:, j], minlength=g)
    meat = sums.T@sums
    lag = max(MIN_HAC_DAYS, HAC_OVERLAP_MULTIPLIER*math.ceil(h/bpd))
    hac = meat.copy()
    for k in range(1, min(lag, len(sums)-1)+1):
        ac = sums[k:].T@sums[:-k]
        hac += (1-k/(lag+1))*(ac+ac.T)
    correction = g/(g-1)
    if nobs and nobs > p:
        correction *= (nobs-1)/(nobs-p)
    v0 = max(0., float((bread@meat@bread)[coefficient, coefficient])*correction)
    v1 = max(0., float((bread@hac@bread)[coefficient, coefficient])*correction)
    # Conservative reference df: count independent bandwidth-sized date blocks,
    # rather than pretending that HAC-correlated daily clusters are independent.
    return {"se": math.sqrt(max(v0, v1)), "se_date": math.sqrt(v0), "clusters": g,
            "hac_lag": lag, "p_df": max(1, g//(lag+1)-1)}


def ols(x, y, days, h, bpd):
    x, y = np.asarray(x, float), np.asarray(y, float)
    if len(y) < MIN_OBSERVATIONS or np.linalg.matrix_rank(x.T@x) < x.shape[1]:
        return None
    bread = np.linalg.inv(x.T@x)
    beta = bread@(x.T@y)
    residual = y-x@beta
    inference = clustered_inference(bread, x*residual[:, None], days, h, bpd, nobs=len(y))
    if inference is None or inference["se"] <= 0:
        return None
    t = float(beta[-1])/inference["se"]
    return {"coef": float(beta[-1]), "t": t,
            "t_date": float(beta[-1])/max(inference["se_date"], 1e-30),
            "p": student_p(t, inference["p_df"]), "n": len(y),
            "sse": float(residual@residual), "beta": beta, **inference}


def _public(fit):
    return {k: v for k, v in fit.items() if k not in ("beta", "sse", "se", "se_date")} if fit else {"p": 1., "status": "样本不足"}


def contrast_test(x, y, days, h, bpd, cost=0.):
    valid = np.isfinite(y) & ((x == 0) | (x == 2))
    a, target, dates = x[valid], y[valid], days[valid]
    if min(np.sum(a == 0), np.sum(a == 2)) < MIN_BUCKET:
        return {"p": 1., "status": "样本不足"}, None
    fit = ols(np.column_stack([np.ones(len(a)), a == 2]), target, dates, h, bpd)
    out = _public(fit)
    if fit:
        out.update(top=float(target[a == 2].mean()), bottom=float(target[a == 0].mean()),
                   top_net=float(target[a == 2].mean()-cost), bottom_net=float(target[a == 0].mean()-cost),
                   net_difference=float(fit["coef"]))
    return out, None


def rv_test(feature, target, controls, days, h, bpd, frozen=None):
    valid = np.isfinite(feature) & np.isfinite(target) & np.isfinite(controls).all(axis=1)
    z, y, c, dates = feature[valid].astype(float), target[valid], controls[valid], days[valid]
    if len(y) < MIN_OBSERVATIONS:
        return {"p": 1., "status": "样本不足"}, None
    if frozen is None:
        low, high = np.quantile(z, WINSOR_QUANTILES)
        clipped = np.clip(z, low, high)
        mean, scale = float(clipped.mean()), float(clipped.std())
        cmean, cscale = c.mean(axis=0), c.std(axis=0)
    else:
        low, high, mean, scale, cmean, cscale = (frozen[k] for k in ("low", "high", "mean", "scale", "cmean", "cscale"))
    if scale < 1e-12 or np.min(cscale) < 1e-12:
        return {"p": 1., "status": "样本不足"}, None
    z = (np.clip(z, low, high)-mean)/scale
    base = np.column_stack([np.ones(len(y)), (c-cmean)/cscale])
    full = np.column_stack([base, z])
    fit = ols(full, y, dates, h, bpd)
    if not fit:
        return {"p": 1., "status": "样本不足"}, None
    base_beta = np.linalg.lstsq(base.T@base, base.T@y, rcond=None)[0]
    if frozen is None:
        model = dict(low=float(low), high=float(high), mean=mean, scale=scale,
                     cmean=cmean.tolist(), cscale=cscale.tolist(), base_beta=base_beta.tolist(), beta=fit["beta"].tolist())
    else:
        model = frozen
    residual_base = y-base@np.asarray(model["base_beta"])
    residual_full = y-full@np.asarray(model["beta"])
    sst = np.sum((y-y.mean())**2)
    out = _public(fit)
    out.update(delta_r2=float((residual_base@residual_base-residual_full@residual_full)/sst),
               baseline_r2=float(1-residual_base@residual_base/sst))
    return out, model


def _logistic_fit(x, counts, successes):
    beta = np.zeros(x.shape[1])
    beta[0] = math.log((successes.sum()+.5)/(counts.sum()-successes.sum()+.5))
    for _ in range(60):
        eta = np.clip(x@beta, -30, 30)
        prob = 1/(1+np.exp(-eta))
        weight = counts*prob*(1-prob)
        hessian = x.T@(weight[:, None]*x)
        gradient = x.T@(successes-counts*prob)
        step = np.linalg.lstsq(hessian, gradient, rcond=1e-12)[0]
        # Prevent separation from overflowing; a separated fit is not evidence.
        if np.max(np.abs(step)) > 5:
            step *= 5/np.max(np.abs(step))
        beta += step
        if np.max(np.abs(step)) < 1e-8:
            break
    return beta


def tail_test(bucket, y, sigma, days, h, bpd, frozen=None):
    valid = np.isfinite(y) & np.isfinite(sigma) & (sigma > 0) & ((bucket == 0) | (bucket == 2))
    a, target, sig, dates = bucket[valid], y[valid], sigma[valid], days[valid]
    if len(target) < MIN_OBSERVATIONS or min(np.sum(a == 0), np.sum(a == 2)) < MIN_BUCKET:
        return {"p": 1., "status": "样本不足"}, None
    edges = np.asarray(frozen["edges"]) if frozen else np.unique(np.quantile(sig, np.arange(1, VOLATILITY_BINS)/VOLATILITY_BINS))
    v = np.searchsorted(edges, sig)
    nv = len(edges)+1
    group = 2*v+(a == 2)
    # Group by volatility-category and bucket, exactly sufficient for binomial MLE.
    counts = np.bincount(group, minlength=2*nv).astype(float)
    successes = np.bincount(group, weights=target, minlength=2*nv)
    cat = np.arange(2*nv)//2
    base = np.column_stack([np.ones(2*nv), (cat[:, None] == np.arange(1, nv)).astype(float)])
    x = np.column_stack([base, np.arange(2*nv) % 2])
    if min(successes.sum(), counts.sum()-successes.sum()) < MIN_BUCKET:
        return {"p": 1., "status": "样本不足"}, None
    beta = _logistic_fit(x, counts, successes)
    if np.max(np.abs(beta)) > 25:
        return {"p": 1., "status": "样本不足"}, None
    pred = 1/(1+np.exp(-np.clip(x@beta, -30, 30)))
    information = x.T@((counts*pred*(1-pred))[:, None]*x)
    if np.linalg.matrix_rank(information) < x.shape[1]:
        return {"p": 1., "status": "样本不足"}, None
    bread = np.linalg.inv(information)
    # Date/category/bucket aggregation preserves the cluster sandwich exactly.
    unique, inv = np.unique(dates, return_inverse=True)
    combined = inv*(2*nv)+group
    cn = np.bincount(combined, minlength=len(unique)*2*nv).reshape(-1, 2*nv)
    cy = np.bincount(combined, weights=target, minlength=len(unique)*2*nv).reshape(-1, 2*nv)
    score = (cy-cn*pred)@x
    inf = clustered_inference(bread, score, unique, h, bpd, nobs=len(target))
    if inf is None or inf["se"] <= 0:
        return {"p": 1., "status": "样本不足"}, None
    if frozen is None:
        model = {"edges": edges.tolist(), "base_beta": _logistic_fit(base, counts, successes).tolist(),
                 "beta": beta.tolist(), "risk_top": bool(beta[-1] > 0)}
    else:
        model = frozen
    pb = 1/(1+np.exp(-np.clip(base@np.asarray(model["base_beta"]), -30, 30)))
    pf = 1/(1+np.exp(-np.clip(x@np.asarray(model["beta"]), -30, 30)))
    risk_group = (np.arange(2*nv) % 2) == int(model["risk_top"])
    risk_n = counts[risk_group].sum()
    risk_p = successes[risk_group].sum()/risk_n
    base_p = np.sum(counts[risk_group]*pb[risk_group])/risk_n
    # Unconditional probability uses all available feature buckets, including middle.
    uncond_valid = np.isfinite(y) & np.isfinite(sigma) & (bucket >= 0)
    unconditional = float(y[uncond_valid].mean())
    logloss = lambda p: float(-np.sum(successes*np.log(np.clip(p, 1e-15, 1))+(counts-successes)*np.log(np.clip(1-p, 1e-15, 1)))/counts.sum())
    t = float(beta[-1]/inf["se"])
    out = {"coef": float(beta[-1]), "t": t, "t_date": float(beta[-1]/max(inf["se_date"], 1e-30)),
           "p": student_p(t, inf["p_df"]), "n": len(target), "clusters": inf["clusters"],
           "hac_lag": inf["hac_lag"], "p_df": inf["p_df"], "risk_probability": float(risk_p), "vol_baseline_probability": float(base_p),
           "unconditional_probability": unconditional, "lift": float(risk_p/max(base_p, 1e-15)),
           "probability_increment": float(risk_p-base_p), "unconditional_lift": float(risk_p/max(unconditional, 1e-15)),
           "delta_logloss": logloss(pb)-logloss(pf), "risk_top": model["risk_top"]}
    out.update(top_probability=float(target[a == 2].mean()), bottom_probability=float(target[a == 0].mean()),
               top_n=int(np.sum(a == 2)), bottom_n=int(np.sum(a == 0)))
    return out, model


def atomic_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix+".tmp")
    tmp.write_text(json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False), encoding="utf-8")
    tmp.replace(path)


def code_hash():
    return hashlib.sha256(Path(__file__).read_bytes()).hexdigest()


def load_bars(path, timeframe, market):
    rows = json.loads(Path(path).read_text())
    if not rows:
        return pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    frame = pd.DataFrame([r[:6] for r in rows], columns=["time", "open", "high", "low", "close", "volume"])
    if timeframe == "1h":
        stamps = pd.to_datetime(frame.pop("time"), unit="ms", utc=True)+pd.Timedelta(hours=1)
    else:
        stamps = pd.to_datetime(frame.pop("time"), utc=True)+pd.Timedelta(hours=24 if market == "crypto" else 21)
    frame.index = pd.DatetimeIndex(stamps)
    frame = frame.apply(pd.to_numeric, errors="coerce").sort_index()
    frame = frame[~frame.index.duplicated(keep="last")]
    # Fixed as-of date; crypto daily closes shift to next UTC midnight.
    frame = frame.loc[frame.index < pd.Timestamp("2026-10-25", tz="UTC")]
    valid = (frame[["open", "high", "low", "close"]] > 0).all(axis=1)
    valid &= np.isfinite(frame[["open", "high", "low", "close"]]).all(axis=1)
    valid &= frame.high.ge(frame[["open", "close", "low"]].max(axis=1))
    valid &= frame.low.le(frame[["open", "close", "high"]].min(axis=1))
    frame.loc[~valid, :] = np.nan
    if timeframe == "1h" and len(frame):
        frame = frame.reindex(pd.date_range(frame.index.min(), frame.index.max(), freq="h"))
    return frame


def split_masks(timestamps, timeframe):
    """Calendar 60/40 split via audit.time_split; purge common market bars."""
    ts = pd.DatetimeIndex(timestamps)
    calendar = pd.date_range(ts.min().floor("D"), ts.max().floor("D"), freq="D")
    _, retained = time_split(list(calendar), TRAIN_FRACTION)
    cutoff = retained[0]
    common = ts.unique().sort_values()
    before = common[common < cutoff]
    purge = max(HORIZONS[timeframe])
    train_end = before[-purge] if len(before) >= purge else common[0]
    return (np.asarray(ts < train_end), np.asarray(ts >= cutoff),
            {"calendar_start": str(calendar[0]), "calendar_end": str(calendar[-1]),
             "cutoff": str(cutoff), "train_end_exclusive": str(train_end), "purge_bars": purge})


def data_paths(timeframe, market):
    if timeframe == "1h" and market != "crypto":
        raise ValueError("No US hourly data: use --timeframe 1d --market us")
    root = DATA_ROOT / ("regime_study/klines_1h" if timeframe == "1h" else f"factor_mining/{market}/daily")
    return sorted(root.glob("*.json"))


def prepare_base(frames, timeframe):
    bpd = 24 if timeframe == "1h" else 1
    times = pd.DatetimeIndex(np.concatenate([f.index.to_numpy() for _, f in frames]))
    # Recreate UTC explicitly, including the otherwise-naive numpy representation.
    times = pd.to_datetime(times, utc=True)
    tr, ho, split = split_masks(times, timeframe)
    eligible, controls, sigmas, atrs = [], [], [], []
    symbols, snapshots = [], []
    labels = {h: {k: [] for k in ("return", "rv", "tail", "drawdown")} for h in HORIZONS[timeframe]}
    inventories = []
    for sid, (name, f) in enumerate(frames):
        observed = f.close.notna()
        history = f.index.asi8 >= int(f.index[0].value)+MIN_HISTORY_DAYS*86_400_000_000_000
        active = history & (observed.cumsum() >= MIN_HISTORY_DAYS*bpd) & observed
        eligible.append(np.asarray(active))
        t = forward_targets(f, timeframe)
        controls.append(t["controls"])
        sigmas.append(t["sigma"])
        atrs.append(t["atr_pct"])
        symbols.append(np.full(len(f), sid, dtype=np.int16))
        snapshots.append(np.asarray(f.index.hour == 0) if bpd == 24 else np.ones(len(f), bool))
        for h in labels:
            for k in labels[h]:
                arr = t[h][k].copy()
                # Individual stocks may have halts/gaps: common-session purge alone
                # cannot guarantee that the h-th observed bar precedes the cutoff.
                cutoff = pd.Timestamp(split["cutoff"])
                endpoint = pd.Series(f.index, index=f.index).shift(-h)
                crosses = np.asarray((f.index < cutoff) & (endpoint >= cutoff))
                arr[crosses] = np.nan
                labels[h][k].append(arr)
        inventories.append({"symbol": name, "bars": int(observed.sum()), "missing": int((~observed).sum()),
                            "first": str(f.index[0]), "last": str(f.index[-1]), "eligible": int(np.sum(active))})
    eligible = np.concatenate(eligible)
    result = {"days": times.values.astype("datetime64[D]").astype(np.int64), "symbol": np.concatenate(symbols),
              "snapshot": np.concatenate(snapshots), "eligible": eligible, "train": tr & eligible,
              "holdout": ho & eligible, "controls": np.concatenate(controls), "sigma": np.concatenate(sigmas),
              "atr_pct": np.concatenate(atrs)}
    for h in labels:
        for k in labels[h]:
            result[f"{h}_{k}"] = np.concatenate(labels[h][k])
    return result, {"split": split, "inventory": inventories, "bars": len(times), "eligible_bars": int(eligible.sum())}


def build_features(frames, n):
    raw, bins, directions, states = [], [], [], []
    for _, bars in frames:
        f = compute_features(bars, n)
        raw.append(f[list(FEATURES)].to_numpy(dtype=np.float32))
        bins.append(np.column_stack([expanding_terciles(f[name]).to_numpy() for name in FEATURES]))
        directions.append(np.sign(np.log(bars.close/bars.close.shift(n))).to_numpy(dtype=np.float32))
        states.append(f.state.to_numpy(dtype=np.float32))
    return {"raw": np.concatenate(raw), "ts": np.concatenate(bins),
            "direction": np.concatenate(directions), "state": np.concatenate(states)}


def cross_section_buckets(feature, base):
    """Synchronous daily ranks; midnight snapshots persist forward within that day."""
    valid = base["snapshot"] & base["eligible"] & np.isfinite(feature)
    out = np.full(len(feature), -1, dtype=np.int8)
    if not valid.any():
        return out
    x = pd.DataFrame({"x": feature[valid], "day": base["days"][valid]})
    grouped = x.groupby("day").x
    low = x.day.map(grouped.quantile(1/3))
    high = x.day.map(grouped.quantile(2/3))
    count = grouped.transform("count")
    bucket = np.select([x.x <= low, x.x > high], [0, 2], 1).astype(np.int8)
    bucket[count < MIN_CROSS_SECTION] = -1
    stride = int(base["symbol"].max())+1
    keys = base["days"]*stride+base["symbol"]
    lookup = pd.Series(bucket, index=keys[valid])
    if lookup.index.has_duplicates:
        raise ValueError("More than one daily snapshot per symbol")
    out[:] = lookup.reindex(keys, fill_value=-1).to_numpy(dtype=np.int8)
    out[~base["eligible"] | ~np.isfinite(feature)] = -1
    return out


def test_specifications():
    for feature in FEATURES:
        yield feature, "T2", "continuous"
        for method in ("ts", "cs"):
            for target in ("T1", "T3_tail", "T3_drawdown"):
                yield feature, target, method
    yield "state", "T4", "state"


def dispatch_test(spec, arrays, base, mask, h, timeframe, market, frozen=None, bucket_cache=None):
    feature, target, method = spec
    bpd = 24 if timeframe == "1h" else 1
    cost = CRYPTO_COST if market == "crypto" else US_COST
    days = base["days"][mask]
    if target == "T4":
        state = arrays["state"]
        buckets = np.where(state == -1, 0, np.where(state == 1, 2, -1))
        y = 2*arrays["direction"][mask]*base[f"{h}_return"][mask]
        return contrast_test(buckets[mask], y, days, h, bpd)
    j = FEATURES.index(feature)
    raw = arrays["raw"][:, j]
    if target == "T2":
        return rv_test(raw[mask], base[f"{h}_rv"][mask], base["controls"][mask], days, h, bpd, frozen)
    if method == "ts":
        buckets = arrays["ts"][:, j]
    else:
        if bucket_cache is not None and feature in bucket_cache:
            buckets = bucket_cache[feature]
        else:
            buckets = cross_section_buckets(raw, base)
            if bucket_cache is not None:
                bucket_cache[feature] = buckets
    if target == "T1":
        y = arrays["direction"][mask]*base[f"{h}_return"][mask]
        return contrast_test(buckets[mask], y, days, h, bpd, cost)
    endpoint = target.split("_", 1)[1]
    # Match the baseline volatility to the known event threshold. Otherwise a wick
    # divided by ATR can merely recover that threshold on an IID random walk.
    current_vol = base["sigma" if endpoint == "tail" else "atr_pct"]
    return tail_test(buckets[mask], base[f"{h}_{endpoint}"][mask], current_vol[mask], days, h, bpd, frozen)


def state_transitions(states, frames, base, mask, h):
    shifted, start = [], 0
    for _, frame in frames:
        stop = start+len(frame)
        arr = np.full(len(frame), np.nan)
        arr[:-h] = states[start+h:stop]
        shifted.append(arr)
        start = stop
    future = np.concatenate(shifted)
    # Both ends must lie in the same admitted split, not just the starting point.
    shifted_mask, start = [], 0
    for _, frame in frames:
        stop = start+len(frame)
        a = np.zeros(len(frame), bool)
        a[:-h] = mask[start+h:stop]
        shifted_mask.append(a)
        start = stop
    valid = mask & np.concatenate(shifted_mask) & np.isfinite(states) & np.isfinite(future)
    count = np.zeros((3, 3), dtype=np.int64)
    np.add.at(count, (states[valid].astype(int)+1, future[valid].astype(int)+1), 1)
    probs = count/np.maximum(count.sum(axis=1, keepdims=True), 1)
    marginal = count.sum(axis=0)/max(1, count.sum())
    return {"order": ["range", "other", "trend"], "count": count.tolist(), "probability": probs.tolist(),
            "next_marginal": marginal.tolist(), "diagonal_minus_marginal": (np.diag(probs)-marginal).tolist()}


def study_prefix(out, timeframe, market):
    return Path(out)/f"{market}_{timeframe}"


def run_training(out, timeframe, market, frames=None):
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    prefix = study_prefix(out, timeframe, market)
    if prefix.with_suffix(".holdout.csv").exists() or (out/"candidates.json").exists():
        raise ValueError("Frozen experiment exists; training cannot be rerun into this directory")
    input_hashes = {}
    if frames is None:
        paths = data_paths(timeframe, market)
        input_hashes = {str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in paths}
        frames = [(p.stem, load_bars(p, timeframe, market)) for p in paths]
    frames = [(name, f) for name, f in frames if len(f)]
    if not frames:
        raise ValueError("No input data")
    cache = out/"cache"/f"{market}_{timeframe}"
    cache.mkdir(parents=True, exist_ok=True)
    base, metadata = prepare_base(frames, timeframe)
    np.savez(cache/"base.npz", **base)
    metadata.update(market=market, timeframe=timeframe, protocol=PROTOCOL_VERSION, code_sha256=code_hash(),
                    input_sha256=input_hashes,
                    windows=window_grid(timeframe), features=list(FEATURES), horizons=list(HORIZONS[timeframe]),
                    planned_tests=len(list(test_specifications()))*len(window_grid(timeframe))*len(HORIZONS[timeframe]))
    atomic_json(prefix.with_suffix(".metadata.json"), metadata)
    rows, transitions = [], {}
    for n in window_grid(timeframe):
        start = time.monotonic()
        arrays = build_features(frames, n)
        np.savez(cache/f"features_{n}.npz", **arrays)
        buckets = {}
        for spec in test_specifications():
            for h in HORIZONS[timeframe]:
                fit, model = dispatch_test(spec, arrays, base, base["train"], h, timeframe, market, bucket_cache=buckets)
                row = dict(zip(KEY, [market, timeframe, spec[0], n, h, spec[1], spec[2]]))
                row.update(fit)
                row["model"] = json.dumps(model, ensure_ascii=False, allow_nan=False)
                rows.append(row)
        for h in HORIZONS[timeframe]:
            transitions[f"{n}_{h}"] = state_transitions(arrays["state"], frames, base, base["train"], h)
        print(f"TRAIN {market} {timeframe} n={n}: {len(rows)} tests cumulative, {time.monotonic()-start:.1f}s", flush=True)
    pd.DataFrame(rows).to_csv(prefix.with_suffix(".train.csv"), index=False)
    atomic_json(prefix.with_suffix(".transitions.train.json"), transitions)
    return pd.DataFrame(rows)


def add_selection(rows):
    """Global BH, then a TRAIN-ONLY plateau. Never choose a neighbor as default."""
    result = rows.copy()
    result["q"] = bh_adjust(result.p.fillna(1).to_numpy())
    result["candidate"] = result.q <= FDR
    result["plateau"] = 0.
    groups = [k for k in KEY if k != "window"]
    for _, group in result.groupby(groups, sort=False):
        default = WINDOWS[group.iloc[0].timeframe]
        central = group[group.window == default]
        if central.empty or not np.isfinite(central.iloc[0].get("coef", np.nan)):
            continue
        direction = np.sign(central.iloc[0].coef)
        grid = {int(row.window): {"t": row.get("t", np.nan)} for _, row in group.iterrows()}
        plateau = sweep_plateau(lambda params: float(np.isfinite(params["t"]) and direction*params["t"] >= NEIGHBOR_T),
                                grid, default, min_pass_ratio=PLATEAU_RATIO, score_threshold=.5)
        result.loc[group.index, "plateau"] = plateau.plateau_ratio
    return result


def freeze_candidates(out, timeframes=("1h", "1d")):
    out = Path(out)
    destination = out/"candidates.json"
    if destination.exists():
        raise ValueError("Candidate manifest already frozen")
    sources = [study_prefix(out, tf, "crypto").with_suffix(".train.csv") for tf in timeframes]
    for p in sources:
        if not p.exists():
            raise ValueError(f"Both crypto training tables are required: {p}")
    rows = add_selection(pd.concat([pd.read_csv(p) for p in sources], ignore_index=True))
    metadata = [json.loads(study_prefix(out, tf, "crypto").with_suffix(".metadata.json").read_text()) for tf in timeframes]
    if any(m["code_sha256"] != code_hash() for m in metadata):
        raise ValueError("Code changed after training; cannot freeze an inconsistent experiment")
    rows.to_csv(out/"crypto_training_fdr.csv", index=False)
    candidates = json.loads(rows[rows.candidate].to_json(orient="records"))
    manifest = {"protocol": PROTOCOL_VERSION, "code_sha256": code_hash(), "family_tests": len(rows),
                "fdr": FDR, "candidate_count": len(candidates), "candidates": candidates,
                "training_sha256": {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in sources},
                "preregistration_sha256": hashlib.sha256((Path(__file__).resolve().parents[2]/"docs/research/regime-statistics-preregistration.md").read_bytes()).hexdigest()}
    atomic_json(destination, manifest)
    print(f"FROZEN tests={len(rows)} FDR_candidates={len(candidates)}", flush=True)
    return rows


def economics_pass(target, fit):
    if target == "T1":
        return fit.get("net_difference", -np.inf) > 0
    if target == "T2":
        return fit.get("delta_r2", -np.inf) >= MIN_INCREMENTAL_R2
    if target.startswith("T3"):
        return fit.get("lift", 0) >= MIN_TAIL_LIFT and fit.get("probability_increment", -np.inf) >= MIN_TAIL_INCREMENT
    return True  # no T4 economic threshold specified in the task


def evaluate_holdout(out, timeframe, market):
    out = Path(out)
    manifest = json.loads((out/"candidates.json").read_text())
    if manifest["code_sha256"] != code_hash():
        raise ValueError("Code changed since candidate freeze")
    for name, digest in manifest["training_sha256"].items():
        if hashlib.sha256((out/name).read_bytes()).hexdigest() != digest:
            raise ValueError("Frozen training table changed")
    prefix = study_prefix(out, timeframe, market)
    result_path = prefix.with_suffix(".holdout.csv")
    marker = prefix.with_suffix(".holdout.started.json")
    if result_path.exists() or marker.exists():
        raise ValueError("Holdout evaluation is single-use; an attempt already exists")
    meta = json.loads(prefix.with_suffix(".metadata.json").read_text())
    if meta["code_sha256"] != code_hash():
        raise ValueError("Cache code differs from the frozen protocol")
    candidates = [row for row in manifest["candidates"] if row["timeframe"] == timeframe]
    # US only checks the matching crypto DAILY candidates, never makes its own choices.
    if market == "us" and timeframe != "1d":
        raise ValueError("US generalization is daily only")
    atomic_json(marker, {"protocol": PROTOCOL_VERSION, "candidate_count": len(candidates), "note": "single use, written before opening holdout cache"})
    cache = out/"cache"/f"{market}_{timeframe}"
    with np.load(cache/"base.npz") as z:
        base = {k: z[k] for k in z.files}
    rows = []
    for n in window_grid(timeframe):
        selected = [r for r in candidates if r["window"] == n]
        if not selected:
            continue
        with np.load(cache/f"features_{n}.npz") as z:
            arrays = {k: z[k] for k in z.files}
        buckets = {}
        # US has its own training fit; it never borrows price/volatility scale from crypto.
        us_train = pd.read_csv(prefix.with_suffix(".train.csv")) if market == "us" else None
        for train in selected:
            spec = (train["feature"], train["target"], train["method"])
            model = json.loads(train["model"]) if isinstance(train["model"], str) else None
            if us_train is not None:
                match = us_train[(us_train.feature == spec[0]) & (us_train.target == spec[1]) & (us_train.method == spec[2]) & (us_train.window == n) & (us_train.horizon == train["horizon"])]
                model = json.loads(match.iloc[0].model) if isinstance(match.iloc[0].model, str) else None
            if spec[1] in ("T2", "T3_tail", "T3_drawdown") and model is None:
                fit = {"p": 1., "status": "样本不足"}
            else:
                fit, _ = dispatch_test(spec, arrays, base, base["holdout"], train["horizon"], timeframe, market, model, buckets)
            row = dict(zip(KEY, [market, timeframe, spec[0], n, train["horizon"], spec[1], spec[2]]))
            row.update(fit)
            direction_ok = fit.get("coef", 0)*train.get("coef", 0) > 0
            row["direction_consistent"] = direction_ok
            row["confirmed"] = bool(direction_ok and abs(fit.get("t", 0)) >= HOLDOUT_T)
            row["economic_pass"] = economics_pass(spec[1], fit)
            row["plateau"] = train["plateau"]
            row["effective"] = bool(market == "crypto" and row["confirmed"] and row["economic_pass"]
                                    and train["plateau"] >= PLATEAU_RATIO and n == WINDOWS[timeframe])
            rows.append(row)
        print(f"HOLDOUT {market} {timeframe} n={n}: {len(rows)} candidates evaluated", flush=True)
    result = pd.DataFrame(rows) if rows else pd.DataFrame(columns=KEY+["confirmed", "effective", "direction_consistent"])
    result.to_csv(result_path, index=False)
    return result


def synthetic_panel(seed=7, bars=1200, assets=12, timeframe="1d", planted=False):
    """Matched length and volatility IID Gaussian walks; optional causal planted effect.

    Planting: a predictable high-efficiency, persistent-drift state alternates with
    zero-drift random walk. All prices are generated chronologically, no labels fed
    back into features. The same dated regimes continue across the time split.
    """
    rng = np.random.default_rng(seed)
    index = pd.date_range("2020-01-01", periods=bars, freq="h" if timeframe == "1h" else "D", tz="UTC")
    result = []
    for sid in range(assets):
        innovations = rng.normal(0, .01, bars)
        if planted:
            period = (np.arange(bars)+sid*7)//100
            drift = np.where(period % 2 == 0, np.where(period % 4 == 0, .018, -.018), 0.)
            innovations += drift
        close = 100*np.exp(np.cumsum(innovations))
        op = np.r_[close[0], close[:-1]]
        high = np.maximum(op, close)*1.002
        low = np.minimum(op, close)*.998
        result.append((f"S{sid:03d}", pd.DataFrame({"open": op, "high": high, "low": low,
                                                   "close": close, "volume": 1.}, index=index)))
    return result


def run_synthetic_control(out, seed=7, planted=False, bars=1200, assets=12):
    """Run the very same 2,955-hypothesis pipeline and single-use gate on a null panel."""
    train = run_training(out, "1d", "crypto", synthetic_panel(seed, bars, assets, planted=planted))
    selected = freeze_candidates(out, timeframes=("1d",))
    hold = evaluate_holdout(out, "1d", "crypto")
    summary = {"seed": seed, "bars_per_asset": bars, "assets": assets, "planted": planted,
               "tests": len(train), "train_fdr": int(selected.candidate.sum()),
               "holdout_confirmed": int(hold.confirmed.sum()), "effective": int(hold.effective.sum())}
    atomic_json(Path(out)/"control_summary.json", summary)
    return summary, selected, hold


def _number(row, column, digits=2, multiplier=1.):
    value = row.get(column, np.nan)
    return f"{value*multiplier:.{digits}f}" if pd.notna(value) and np.isfinite(value) else "—"


def make_report(out, report_path):
    out = Path(out)
    manifest = json.loads((out/"candidates.json").read_text())
    crypto = pd.read_csv(out/"crypto_training_fdr.csv")
    tables, summaries = [], []
    effective_rows = []
    for market, timeframe in (("crypto", "1h"), ("crypto", "1d"), ("us", "1d")):
        prefix = study_prefix(out, timeframe, market)
        train = crypto[(crypto.market == market) & (crypto.timeframe == timeframe)].copy() if market == "crypto" else pd.read_csv(prefix.with_suffix(".train.csv"))
        hold = pd.read_csv(prefix.with_suffix(".holdout.csv"))
        meta = json.loads(prefix.with_suffix(".metadata.json").read_text())
        if market == "us":
            train["q"] = np.nan
            train["candidate"] = False
            train["plateau"] = np.nan
        train = train.rename(columns={k: f"train_{k}" for k in train.columns if k not in KEY})
        hold = hold.rename(columns={k: f"hold_{k}" for k in hold.columns if k not in KEY})
        joined = train.merge(hold, on=KEY, how="left", validate="one_to_one")
        joined["verdict"] = "无效"
        insufficient = joined.train_status.eq("样本不足") if "train_status" in joined else pd.Series(False, index=joined.index)
        joined.loc[insufficient, "verdict"] = "样本不足"
        if market == "crypto":
            joined.loc[joined.get("hold_effective", False).fillna(False).astype(bool), "verdict"] = "有效"
            effective_rows.extend(json.loads(joined[joined.verdict == "有效"][KEY].to_json(orient="records")))
        else:
            joined["verdict"] = "未入泛化评估"
            admitted = joined.hold_direction_consistent.notna()
            joined.loc[admitted, "verdict"] = np.where(joined.loc[admitted, "hold_direction_consistent"], "泛化同向", "泛化反向/不足")
        joined.to_csv(prefix.with_suffix(".results.csv"), index=False)
        summary = {"market": market, "timeframe": timeframe, "tests": len(train),
                   "train_fdr": int(train.train_candidate.sum()) if market == "crypto" else None,
                   "holdout_evaluated": len(hold), "holdout_confirmed": int(hold.hold_confirmed.sum()),
                   "effective_default": int((joined.verdict == "有效").sum()),
                   "generalization_same_direction": int(hold.hold_direction_consistent.sum()),
                   "symbols": len(meta["inventory"]), "bars": meta["bars"], "split": meta["split"]}
        summaries.append(summary)
        tables.append((market, timeframe, joined, meta))
    summary = {"protocol": PROTOCOL_VERSION, "code_sha256": manifest["code_sha256"],
               "crypto_family_tests": manifest["family_tests"], "crypto_fdr_candidates": manifest["candidate_count"],
               "studies": summaries, "effective": effective_rows, "untestable": list(UNTESTABLE)}
    atomic_json(out/"summary.json", summary)
    artifact_link = str(out.resolve())
    lines = ["# 市场形态统计特征离线研究", "", "本研究只评价信号的前瞻统计，不模拟订单、仓位或盈亏。",
             "", f"预注册版本 `{PROTOCOL_VERSION}`；代码 SHA256 `{manifest['code_sha256']}`。",
             "[预注册全文](regime-statistics-preregistration.md)在真实数据检验前写入；候选清单在保留窗打开前冻结并保存哈希。",
             "", f"实际 JSON/CSV 与缓存位置：`{artifact_link}`。原指定绝对路径因沙箱拒绝创建，未写入；未改其它数据或数据库。",
             "", "## 核心结果", "", f"加密合并检验 **{manifest['family_tests']}** 项，训练 BH FDR≤10% 通过 **{manifest['candidate_count']}** 项。",
             "", "|市场/周期|全部预注册检验|训练 FDR 通过|保留评估|保留同向且 abs(t)≥2|默认窗口有效|",
             "|---|---:|---:|---:|---:|---:|"]
    for s in summaries:
        lines.append(f"|{s['market']}/{s['timeframe']}|{s['tests']}|{s['train_fdr'] if s['train_fdr'] is not None else '不筛选'}|{s['holdout_evaluated']}|{s['holdout_confirmed']}|{s['effective_default']}|")
    lines += ["", "“保留确认数”包含所有五点窗口；“有效”仅默认窗口，且同时通过平台与经济门槛。不同分档法、预测期及目标分别计数，不能当成独立发现数。",
              "", "有效清单：" + ("；".join(f"{r['timeframe']} {r['feature']} {r['target']} {r['method']} h={r['horizon']}" for r in effective_rows) if effective_rows else "**无特征达标**。"),
              "", "## 方法与门槛", "",
              "默认 n=96 小时/40 日；五点 0.5/0.75/1/1.25/1.5 倍均计入 BH。每个周期 28 个特征×5 窗口×3 预测期×7 个目标/分档组合，加上 15 个 T4 状态检验，共 2,955 项。两个加密周期合并 5,910 项；美股 2,955 项只用于描述与匹配泛化。",
              "时间按全市场日历跨度 60/40 切分，训练末端 purge 最大 h 个公共 bar；资产只在当时已有 60 天且足够观测后进入。分档为截至上一根的扩展三分位、同步每日截面三分位；零值/二元特征的分位相等时低值归低档，避免按资产名字随机拆散同值。",
              "所有表的 t 为日期聚类和日期得分 HAC 中较大 SE 计算；HAC lag=max(22,4×ceil(h/每日bar数))，日期空档补零。p 采用保守的带宽日期块自由度 max(1,floor(G/(lag+1))−1)。CSV 同时保存原始日期聚类 t。跨资产及前瞻重叠均处理，但长记忆与结构突变仍可能使近似不足。",
              "训练 BH 显著后才进入一次保留评估；同向且 |t|≥2、训练邻域同向 |t|≥1.5 比例≥60%，并满足经济阈值才能有效。T1 要求原始高−低成本后差>0；T2 冻结训练系数的样本外增量 R²≥0.005；T3 对当前波动 logit 的条件概率提升≥1.3 且≥1个百分点。T4 比较 trend/range 的顺势减逆势差，无另设经济阈值。",
              "T1 的 15/5bps 成本分别扣在两档均值，相减完全抵消，不能把档差称作可实现净收益。T2 基线为 log RV 的 HAR(1/5/22日或24/120/528小时)。T3 使用当前波动训练分位 20 档的类别 logit（单根尾部用 EWMA σ，路径回撤用 ATR/价格，与事件阈值一致），按日期/波动档/特征档聚合二项充分统计量，保留窗预测系数冻结；其基线是分段常数近似。",
              "VR 使用重叠收益和有限样本修正，附标准同方差 z；最终特征推断使用聚类 t，未以 z 直接做额外筛选。Hurst-lite 是 q 方差缩放斜率的一半。drawdown 为 n 窗内收盘路径精确最大回撤，rebound 为当前位置占 n 窗价格振幅，速度为距最近低点的对数反弹/低点年龄。稳健 σ 使用零中心 MAD 近似。",
              "", "## 各特征结论", "", "以下按默认窗口汇总：任一目标/分档/预测期达标才记该特征有效；完整逐检验表保留失败行。", "", "|特征|加密1h|加密1d|", "|---|---|---|"]
    for name in (*FEATURES, "state"):
        statuses = []
        for _, _, df, _ in tables[:2]:
            selected = df[(df.feature == name) & (df.window == WINDOWS[df.iloc[0].timeframe])]
            statuses.append("有效" if (selected.verdict == "有效").any() else ("样本不足" if (selected.verdict == "样本不足").all() else "无效"))
        lines.append(f"|{name}|{statuses[0]}|{statuses[1]}|")
    lines += ["|BID-ASK BOUNCE / ORDER BOOK|不可检验|不可检验|", "", "ρ1 只是小时/日频噪声代理，不能据此推断盘口或买卖价跳动。", "", "## 数据与切分", ""]
    for market, tf, _, meta in tables:
        s = meta["split"]
        lines.append(f"- {market}/{tf}：{len(meta['inventory'])} 标的，{meta['bars']:,} 根（含补空缺），当时满足历史条件 {meta['eligible_bars']:,} 根；{s['calendar_start']} 至 {s['calendar_end']}；训练 t<{s['train_end_exclusive']}，保留 t≥{s['cutoff']}，purge {s['purge_bars']} bar。")
    lines += ["", "## 默认窗口逐特征×目标结果", "", "所有 8,865 行（包括五点邻域、失败项与未入保留项）均在各市场 `*.results.csv`，原始训练表和冻结模型参数也一并保留。下表为全部默认窗口；“—”表示未获准评估或该目标不适用，绝非零效应。ΔR² 为百分点，档差为 bps，Δp 为百分点；斜线依次为训练/保留。", ""]
    for market, tf, df, _ in tables:
        lines += [f"### {market} / {tf}", "", f"[全部窗口 CSV]({artifact_link}/{market}_{tf}.results.csv)", "",
                  "|特征|目标/分档|h|训练t|q|保留t|ΔR² 训/留(pp)|成本后档差 训/留(bps)|保留lift/Δp(pp)|平台|结论|",
                  "|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|"]
        for _, row in df[df.window == WINDOWS[tf]].sort_values(["feature", "target", "method", "horizon"]).iterrows():
            r2 = _number(row, "train_delta_r2", multiplier=100)+"/"+_number(row, "hold_delta_r2", multiplier=100)
            net = _number(row, "train_net_difference", multiplier=10000)+"/"+_number(row, "hold_net_difference", multiplier=10000)
            tail = _number(row, "hold_lift")+"/"+_number(row, "hold_probability_increment", multiplier=100)
            lines.append(f"|{row.feature}|{row.target}/{row.method}|{int(row.horizon)}|{_number(row,'train_t')}|{_number(row,'train_q',3)}|{_number(row,'hold_t')}|{r2}|{net}|{tail}|{_number(row,'train_plateau',0,100)}%|{row.verdict}|")
    lines += ["", "## 状态持续性与泛化", "",
              "训练状态转移矩阵（range/other/trend，含对角概率减下一状态边际概率）见各市场 `*.transitions.train.json`。滚动窗口重叠会机械地产生持续性；它不是预测收益有效的证据。T4 必须独立通过收益差检验。",
              "美股只匹配加密日线训练候选，不独立筛选；方向一致不代表在美股显著。US 行的 q 为不适用。加密小时特征的日线迁移不是同一频率复现。SPY 随本地美股池一同纳入；T1 是绝对顺逆势 payoff，未声称相对 SPY 的 alpha。",
              "", "## 最脆弱的结论与局限", "",
              "1. 数据池是当前提供的 100 币/96 股，缺乏历史退市与完整当时可投标的清单。逐时历史资格避免用未来存续长度筛选，仍无法消除预选池及幸存者偏差。",
              "2. 小时数据仅约三年，多个币共享同一大行情；日期聚类减少横截面伪独立，却无法创造独立市场周期。长预测期、少数尾部日期驱动的结论最脆弱。",
              "3. T1 的重叠标签、T4 的重叠状态和慢特征都可能有长于 HAC 截断的相关。保留窗显著性未再做第二次 FDR，是预注册的确认门槛，不能解释为所有最终发现整体 FDR≤10%。",
              "4. T3 的 20 档当前波动基线是近似；同档内部的波动差异、阈值本身使用估计 σ、稀少事件或分离，都可能影响增量解释。最接近 1.3×/1pp 边界的尾部结论尤其脆弱。",
              "5. T2 若仅训练 R² 提升而保留下降，属于不稳定关系；默认 n 的有效性不能推广到其它周期。0.5% 门槛附近的结论对基线估计及异常行情敏感。",
              "6. 日线美股为复权数据，调整版本不是历史点时版本；除权、交易时段、节假日与加密 24/7 市场不同。当前来源未提供复权版本链，泛化仅作辅助。",
              "7. 统计特征是形态代理：Hurst-lite 不是长期 Hurst 一致估计，RV/BV 跳跃份额在低频有限样本中有噪声，OHLC 影线不能识别闪崩交易机制。无盘口的形态不可检验。",
              "", "## 落地建议", ""]
    if effective_rows:
        lines.append("仅将已达标的特征×目标×周期作为 market_regime 的参考证据，先做独立滚动样本的影子记录。T2/T3 只解释风险环境；只有达标的 T4 才支持探索 exit_mode=trend/range/quick 的辅助选择。美股方向反转或单一预测期有效的项应继续离线验证。不得用本结果直接做开仓门控；未评估订单、滑点、资金费率或执行。")
    else:
        lines.append("本批无特征达标，维持现有交易逻辑；不增加过滤、熔断、开仓门控或 exit_mode 自动切换。后续如积累独立周期或更完整退市数据，应重新预注册新的研究。")
    lines += ["", "## 验证与可复现", "",
              "执行 `python -m unittest discover -s scripts/backtest/tests -t .`；特征已知序列、逐特征截断、标签对齐、BH 校准、整套随机游走负对照和植入效应均由固定种子测试覆盖。控制面板与真实面板使用同一统计、筛选和保留评估代码；单测控制的资产数/长度按测试规模匹配，不声称其为真实 100 币面板的精确重采样。",
              "复现需新 out 目录：先分别对 crypto/1h、crypto/1d、us/1d 运行 `--phase train`；再 `--phase freeze`；分别 `--phase evaluate`；最后 `--phase report`。或一次 `--phase all`。已有候选清单禁止重训；保留评估开始前落盘单次标记，禁止重复打开。缓存不可用时不会回退合成数据。",
              "", f"[汇总 JSON]({artifact_link}/summary.json) · [训练全族 FDR 表]({artifact_link}/crypto_training_fdr.csv) · [冻结候选]({artifact_link}/candidates.json)", ""]
    Path(report_path).parent.mkdir(parents=True, exist_ok=True)
    Path(report_path).write_text("\n".join(lines), encoding="utf-8")
    print(json.dumps(summary, ensure_ascii=False, indent=2), flush=True)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--timeframe", choices=("1h", "1d"), default="1h")
    parser.add_argument("--market", choices=("crypto", "us"), default="crypto")
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    parser.add_argument("--phase", choices=("train", "freeze", "evaluate", "report", "all", "null", "planted"), default="train",
                        help="all runs all three studies in strict train→freeze→holdout order; default train")
    parser.add_argument("--report", type=Path, default=Path("docs/research/regime-statistics-study.md"))
    args = parser.parse_args()
    if args.phase == "train":
        run_training(args.out, args.timeframe, args.market)
    elif args.phase == "freeze":
        freeze_candidates(args.out)
    elif args.phase == "evaluate":
        evaluate_holdout(args.out, args.timeframe, args.market)
    elif args.phase == "report":
        make_report(args.out, args.report)
    elif args.phase in ("null", "planted"):
        run_synthetic_control(args.out, planted=args.phase == "planted")
    else:
        for market, tf in (("crypto", "1h"), ("crypto", "1d"), ("us", "1d")):
            run_training(args.out, tf, market)
        freeze_candidates(args.out)
        for market, tf in (("crypto", "1h"), ("crypto", "1d"), ("us", "1d")):
            evaluate_holdout(args.out, tf, market)
        make_report(args.out, args.report)


if __name__ == "__main__":
    main()
