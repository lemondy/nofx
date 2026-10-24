"""预注册的三推信号研究；无订单/成交模拟，缓存只写工作树 out。

python -m scripts.backtest.threepush_study --help
"""
from __future__ import annotations

import argparse
import ast
from dataclasses import dataclass, asdict
import hashlib
import json
import math
from pathlib import Path
import time

import numpy as np
import pandas as pd

from scripts.backtest import regime_study as rs
from scripts.backtest.quant_audit import grid_around, sweep_plateau, time_split

VERSION = "2026-10-24-v2"
ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / "artifacts/threepush/out_v2"
PREREG = ROOT / "docs/research/threepush-preregistration.md"
SUMMARY = ROOT / "docs/research/threepush-summary"
HORIZONS = {"1h": (6, 24, 72), "1d": (3, 7, 14)}
GAPS = {"1h": (6, 72), "1d": (5, 40)}
CUTS = ((1/3, 2/3), (.3, .7), (.4, .6))
CLASSES = ("steep", "medium", "low", "flat")
CYCLES = ("all", "pre2023", "2023_2024", "2025_2026")
DIRECTIONS = ("pooled", "up", "down")
KEY = ["timeframe", "param", "h", "direction", "cycle", "test"]
SEED = 20261024
# name, hypothesis, kind, category A, category B, outcome, directional sign
SPECS = (
    ("H1_med_steep", "H1", "contrast", "medium", "steep", "diff", 1),
    ("H2_edge", "H2", "mean", "medium", "", "diff", 1),
    ("H2_med_steep", "H2", "contrast", "medium", "steep", "diff", 1),
    ("H2_med_low", "H2", "contrast", "medium", "low", "diff", 1),
    ("H3_no_edge", "H3", "mean", "low", "", "diff", -1),
    ("H4_no_edge", "H4", "mean", "flat", "", "diff", -1),
    ("H4_med_flat", "H4", "contrast", "medium", "flat", "diff", 1),
    *((f"H5_{name}", "H5", "regression", "medium", "", name, 1)
      for name in ("decay_flag", "location_flag", "channel_break", "rr_flag")),
)


@dataclass(frozen=True)
class Detector:
    k: int = 3
    pullback: float = .5


def parameter_grid():
    # grid_around retains the shared audit API; explicit k avoids integer rounding ambiguity.
    depths = [v["pullback"] for v in grid_around({"pullback": .5}, (.5, 1., 1.5)).values()]
    return {f"k{k}_p{p:g}_q{q}": (Detector(k, p), CUTS[q])
            for k in (2, 3, 4) for p in depths for q in range(3)}


DEFAULT = "k3_p0.5_q0"


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def atomic(path, obj):
    rs.atomic_json(path, obj)


def fingerprints():
    return {str(p.relative_to(ROOT)): digest(p) for p in
            (Path(__file__).resolve(), Path(rs.__file__),
             ROOT / "scripts/backtest/quant_audit.py", PREREG)}


def seal(out):
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    lock = {"version": VERSION, "sealed_at_utc": pd.Timestamp.now(tz="UTC").isoformat(),
            "files": fingerprints(), "tests_crypto": 2*27*3*3*4*len(SPECS), "out": str(out.resolve()),
            "control_files": {name:digest(out/name) for name in ("null.json", "planted.json", "volatility-scales.json") if (out/name).exists()}}
    path = out / "preregistration-lock.json"
    if path.exists():
        check_lock(out)
        return json.loads(path.read_text())
    atomic(path, lock)
    if out.resolve() == OUT.resolve():
        SUMMARY.mkdir(parents=True, exist_ok=True)
        atomic(SUMMARY / path.name, lock)
    return lock


def check_lock(out):
    lock = json.loads((Path(out) / "preregistration-lock.json").read_text())
    if any(not (Path(out)/name).exists() or digest(Path(out)/name) != value for name, value in lock.get("control_files", {}).items()):
        raise ValueError("Sealed controls changed")
    if lock["files"] != fingerprints():
        raise ValueError("Sealed code/protocol changed; use a newly preregistered experiment")


def atr14(bars):
    c = bars.close
    tr = pd.concat([bars.high-bars.low, (bars.high-c.shift()).abs(),
                    (bars.low-c.shift()).abs()], axis=1).max(axis=1)
    return tr.where(bars[["open", "high", "low", "close"]].notna().all(axis=1)).rolling(14).mean()


def confirmed_pivots(bars, k=3):
    """(pivot index, sign, extreme, confirmation index); strict ties, causal delay."""
    if k < 1:
        raise ValueError("k must be positive")
    n = len(bars)
    if n < 2*k+1:
        return []
    high, low = bars.high.to_numpy(), bars.low.to_numpy()
    hv = np.lib.stride_tricks.sliding_window_view(high, 2*k+1)
    lv = np.lib.stride_tricks.sliding_window_view(low, 2*k+1)
    center = np.arange(k, n-k)
    hp = (hv[:, k] == np.max(hv, axis=1)) & (np.sum(hv == hv[:, k, None], axis=1) == 1)
    lp = (lv[:, k] == np.min(lv, axis=1)) & (np.sum(lv == lv[:, k, None], axis=1) == 1)
    return [(int(i), 1 if hp[j] else -1, float(high[i] if hp[j] else low[i]), int(i+k))
            for j, i in enumerate(center) if hp[j] != lp[j]]


FEATURE_COLUMNS = ["t", "d", "l0_i", "h1_i", "l1_i", "h2_i", "l2_i", "h3_i", "span",
                   "extreme", "l2", "atr", "entry", "stop", "risk", "target_distance",
                   "slope_atr", "near_flat", "pullback_depth", "overlap", "momentum_decay",
                   "p3_p2", "location", "channel_break", "post_h3_bars", "reverse_body_share",
                   "rr_space", "rr_flag", "decay_flag", "timestamp", "day"]


def pattern_features(bars, pivots, t, timeframe, atr=None, location_range=None):
    """All feature values use only bars through detection t; six alternating pivots."""
    (l0, h1, l1, h2, l2, h3) = pivots
    d = h3[1]
    a = float(atr14(bars.iloc[:t+1]).iloc[-1]) if atr is None else float(atr)
    i0, i1, j1, i2, j2, i3 = [p[0] for p in pivots]
    v0, v1, w1, v2, w2, v3 = [d*p[2] for p in pivots]
    p1, p2, p3 = v1-v0, v2-w1, v3-w2
    part = bars.iloc[i0:i3+1]
    hi, lo = part.high.to_numpy(), part.low.to_numpy()
    intersection = np.maximum(0, np.minimum(hi[1:], hi[:-1])-np.maximum(lo[1:], lo[:-1]))
    denominator = np.minimum(hi[1:]-lo[1:], hi[:-1]-lo[:-1])
    overlap = np.divide(intersection, denominator, out=np.zeros_like(intersection), where=denominator > 0).mean()
    if location_range is None:
        recent = bars.iloc[max(0, t-(480 if timeframe == "1h" else 120)+1):t+1]
        top, bottom = recent.high.max(), recent.low.min()
    else:
        top, bottom = location_range
    location = (h3[2]-bottom)/(top-bottom) if d == 1 else (top-h3[2])/(top-bottom)
    line = l1[2]+(l2[2]-l1[2])*(t-j1)/(j2-j1)
    entry = float(bars.close.iloc[t])
    stop = h3[2]+d*.25*a
    risk, distance = d*(stop-entry), d*(entry-l2[2])
    rr = distance/risk if risk > .05*a and distance > 0 else 0.
    body = (bars.close.iloc[i3+1:t+1]-bars.open.iloc[i3+1:t+1]).to_numpy()
    total = np.abs(body).sum()
    ts = bars.index[t]
    return dict(t=t, d=d, l0_i=i0, h1_i=i1, l1_i=j1, h2_i=i2, l2_i=j2, h3_i=i3,
                span=i3-i0, extreme=h3[2], l2=l2[2], atr=a, entry=entry, stop=stop, risk=risk,
                target_distance=min(distance, 2*risk) if risk > .05*a and distance > 0 else np.nan,
                slope_atr=(v3-v1)/a/(i3-i1), near_flat=(v3-v1 < .5*a),
                pullback_depth=((v1-w1)/p1+(v2-w2)/p2)/2, overlap=float(overlap),
                momentum_decay=p3/p1, p3_p2=p3/p2, location=float(location),
                channel_break=int(d*(entry-line) < 0), post_h3_bars=t-i3,
                reverse_body_share=float(np.abs(body[d*body < 0]).sum()/total) if total else 0.,
                rr_space=float(rr), rr_flag=int(rr >= 2), decay_flag=int(p3/p1 < 1),
                timestamp=ts, day=int(ts.value//86_400_000_000_000))


def detect_patterns(bars, timeframe="1h", config=Detector()):
    """Pure online-equivalent state machine. Emitted records are never revised."""
    a = atr14(bars).to_numpy()
    nloc = 480 if timeframe == "1h" else 120
    top = bars.high.rolling(nloc, min_periods=nloc).max().to_numpy()
    bottom = bars.low.rolling(nloc, min_periods=nloc).min().to_numpy()
    complete = bars[["open", "high", "low", "close"]].notna().all(axis=1).to_numpy()
    invalid = np.r_[0, np.cumsum(~complete)]
    state, result = [], []
    floor = -1
    mingap, maxgap = GAPS[timeframe]
    for p in confirmed_pivots(bars, config.k):
        i, direction, value, t = p
        if i <= floor:
            continue
        if state and invalid[i+1]-invalid[state[-1][0]]:
            state = []
        if state and state[-1][1] == direction:
            if direction*value > direction*state[-1][2]:
                state[-1] = p
            else:
                continue
        else:
            state.append(p)
        state = state[-6:]
        if len(state) < 6 or not np.isfinite(a[t]) or not np.isfinite(top[t]):
            continue
        l0, h1, l1, h2, l2, h3 = state
        d = direction
        if not (mingap <= h2[0]-h1[0] <= maxgap and mingap <= h3[0]-h2[0] <= maxgap
                and 1 <= h1[0]-l0[0] <= maxgap):
            continue
        vals = [d*q[2] for q in state]
        v0, v1, w1, v2, w2, v3 = vals
        if not (v1 < v2 < v3 and min(v1-v0, v2-w1, v3-w2) > 0
                and min(v1-w1, v2-w2) >= config.pullback*a[t]):
            continue
        if invalid[t+1]-invalid[l0[0]]:
            continue
        result.append(pattern_features(bars, state, t, timeframe, a[t], (top[t], bottom[t])))
        floor = h3[0]
        state = []
    return pd.DataFrame(result, columns=FEATURE_COLUMNS)


def study_split(timestamps, timeframe):
    """Minimal wrapper increases regime purge to the registered full pattern span."""
    _, _, original = rs.split_masks(timestamps, timeframe)
    ts = pd.DatetimeIndex(timestamps)
    cutoff = pd.Timestamp(original["cutoff"])
    common = ts.unique().sort_values()
    before = common[common < cutoff]
    purge = max(max(HORIZONS[timeframe]), 3*GAPS[timeframe][1])
    end = before[-purge] if len(before) >= purge else common[0]
    return {**original, "train_end_exclusive": str(end), "purge_bars": purge}


def phase_mask(bars, split, phase, timeframe):
    ts = bars.index
    endpoint = pd.Series(ts, index=ts).shift(-max(HORIZONS[timeframe]))
    if phase == "train":
        return np.asarray((ts < pd.Timestamp(split["train_end_exclusive"])) &
                          (endpoint < pd.Timestamp(split["train_end_exclusive"])))
    return np.asarray((ts >= pd.Timestamp(split["cutoff"])) & endpoint.notna())


def fit_thresholds(events, cuts=CUTS[0]):
    nonflat = events.loc[~events.near_flat.astype(bool), "slope_atr"].dropna()
    edges = np.quantile(nonflat, cuts).tolist() if len(nonflat) else [0., 0.]
    classes = classify(events, {"edges": edges})
    loc = events.loc[classes == "medium", "location"].dropna()
    return {"edges": edges, "location_cut": float(loc.quantile(.8)) if len(loc) else 1.}


def classify(events, thresholds):
    return np.where(events.near_flat.astype(bool), "flat",
                    np.where(events.slope_atr <= thresholds["edges"][0], "low",
                             np.where(events.slope_atr <= thresholds["edges"][1], "medium", "steep")))


def target_arrays(bars):
    values = bars[["open", "high", "low", "close"]].to_numpy(float)
    missing = np.r_[0, np.cumsum(~np.isfinite(values).all(axis=1))]
    return values, missing


def forward_outcome(bars, event, h, t=None, control=False):
    """Only targets read future bars; missing/gapped full horizons remain unavailable."""
    t = int(event["t"] if t is None else t)
    if t+h >= len(bars):
        return {}
    values, missing = bars.attrs.get("target_arrays", (None, None))
    if values is None:
        values, missing = target_arrays(bars)
    if missing[t+h+1]-missing[t+1]:
        return {}
    future = values[t+1:t+h+1]
    d, a, entry = int(event["d"]), float(event["atr"]), float(values[t, 3])
    if control:
        ca = float(bars.attrs["atr"][t])
        scale = ca/a
        extreme = entry+(event["extreme"]-event["entry"])*scale
        risk, distance = event["risk"]*scale, event["target_distance"]*scale
        stop = entry+d*risk
        a = ca
    else:
        extreme, stop, risk, distance = (event[k] for k in ("extreme", "stop", "risk", "target_distance"))
    rev = -d*math.log(float(values[t+h, 3])/entry)
    extension = max(0., (float(future[:, 1].max())-extreme) if d == 1 else (extreme-float(future[:, 2].min())))/a
    outcome, score = np.nan, np.nan
    if risk > 0 and np.isfinite(distance) and distance > 0:
        target = entry-d*distance
        hits_stop = future[:, 1] >= stop if d == 1 else future[:, 2] <= stop
        hits_target = future[:, 2] <= target if d == 1 else future[:, 1] >= target
        stop_idx = np.flatnonzero(hits_stop)
        target_idx = np.flatnonzero(hits_target)
        si = stop_idx[0] if len(stop_idx) else h+1
        ti = target_idx[0] if len(target_idx) else h+1
        outcome = -1 if si <= ti and si <= h else (1 if ti < si and ti <= h else 0)
        score = -1. if outcome == -1 else (distance/risk if outcome == 1 else 0.)
    return dict(rev=rev, extension=extension, outcome=outcome, r_score=score)


def breakout_outcome(bars, event, h, maxwait, allowed):
    t, d = int(event["t"]), int(event["d"])
    values, missing = bars.attrs.get("target_arrays", (None, None))
    if values is None:
        values, missing = target_arrays(bars)
    for b in range(t+1, min(len(bars), t+maxwait+1)):
        if not np.isfinite(values[b, 3]):
            return np.nan
        if d*(values[b, 3]-event["stop"]) > 0:
            if b+h >= len(bars) or not allowed[b] or not allowed[b+h]:
                return np.nan
            if missing[b+h+1]-missing[b]:
                return np.nan
            return d*math.log(float(values[b+h, 3]/values[b, 3]))
    return np.nan


def match_controls(bars, events, excluded, train_mask, allowed, seed=SEED):
    """5 without-replacement controls/event; frozen per-symbol/span train quintiles."""
    a = atr14(bars).to_numpy()
    c = bars.close.to_numpy()
    days = bars.index.asi8/86_400_000_000_000
    rng = np.random.default_rng(seed)
    lookup, result = {}, {}
    valid_ohlc = bars[["open", "high", "low", "close"]].notna().all(axis=1).to_numpy()
    missing = np.r_[0, np.cumsum(~valid_ohlc)]
    for e in events.to_dict("records"):
        t, span, d = int(e["t"]), int(e["span"]), int(e["d"])
        key = (span, d)
        if key not in lookup:
            move = np.full(len(c), np.nan)
            move[span:] = d*(c[span:]-c[:-span])/a[span:]
            ii = np.arange(span, len(c))
            move[ii[missing[ii+1]-missing[ii-span] > 0]] = np.nan
            past = move[train_mask & np.isfinite(move) & (move > 0)]
            edges = np.quantile(past, [.2, .4, .6, .8]) if len(past) >= 30 else None
            bins = np.searchsorted(edges, move, side="left") if edges is not None else None
            lookup[key] = move, bins, edges
        move, bins, edges = lookup[key]
        if edges is None or not np.isfinite(move[t]) or move[t] <= 0:
            continue
        lo, hi = np.searchsorted(days, [days[t]-30, days[t]+30], side="left")
        hi = min(hi+1, len(c))
        candidates = np.arange(lo, hi)
        good = (allowed[candidates] & ~excluded[candidates] & np.isfinite(move[candidates]) &
                (move[candidates] > 0) & (bins[candidates] == bins[t]) & (np.abs(days[candidates]-days[t]) <= 30))
        pool = candidates[good]
        if len(pool) >= 5:
            chosen = rng.choice(pool, 5, replace=False)
            result[t] = {"indices": chosen.tolist(), "bin": int(bins[t]),
                         "move": float(move[t]), "control_moves": move[chosen].tolist(), "edges": edges.tolist()}
    return result


def stable_seed(*parts):
    return (SEED+int(hashlib.sha256("|".join(map(str, parts)).encode()).hexdigest()[:8], 16)) % (2**32)


def causal_exclusions(length, detectors):
    """At candidate u, only detections already emitted by u can exclude u."""
    excluded = np.zeros(length, bool)
    for name, events in detectors.items():
        k = int(name[1])
        for t in events.t:
            excluded[int(t):min(length, int(t)+k+1)] = True
    return excluded


def prepare_events(frames, timeframe, split, phase):
    """Features cached by detector, not by slope cuts. Never accesses target values."""
    collected = {f"k{k}_p{p:g}": [] for k in (2, 3, 4) for p in (.25, .5, .75)}
    matches_meta = []
    for sid, (symbol, bars) in enumerate(frames):
        bars.attrs.clear()
        allowed = phase_mask(bars, split, phase, timeframe)
        train_mask = phase_mask(bars, split, "train", timeframe)
        detectors = {name: detect_patterns(bars, timeframe, Detector(int(name[1]), float(name.split("p")[1])))
                     for name in collected}
        excluded = causal_exclusions(len(bars), detectors)
        bars.attrs["atr"] = atr14(bars).to_numpy()
        bars.attrs["target_arrays"] = target_arrays(bars)
        for name, ev in detectors.items():
            raw_n = len(ev)
            if len(ev):
                ev = ev.loc[allowed[ev.t.to_numpy(dtype=int)]].copy()
                if phase == "holdout":
                    ev = ev.loc[bars.index[ev.l0_i.to_numpy(dtype=int)] >= pd.Timestamp(split["cutoff"])].copy()
            matched = match_controls(bars, ev, excluded, train_mask, allowed, stable_seed(symbol, name, phase))
            records = []
            for event in ev.to_dict("records"):
                t = event["t"]
                for h in HORIZONS[timeframe]:
                    target = forward_outcome(bars, event, h)
                    if not target:
                        continue
                    cost = rs.US_COST if symbol.startswith("US:") else rs.CRYPTO_COST
                    row = {**event, **target, "h": h, "symbol": symbol, "rev_net": target["rev"]-cost,
                           "matched": t in matched, "control_mean": np.nan, "diff": np.nan,
                           "control_days": [], "control_values": [], "control_R": np.nan, "control_extension": np.nan,
                           "breakout_net": (breakout_outcome(bars, event, h, max(HORIZONS[timeframe]), allowed)-cost) if event["near_flat"] else np.nan}
                    row["stop_minus_target"] = -target["outcome"]
                    if t in matched:
                        controls = matched[t]["indices"]
                        labels = [forward_outcome(bars, event, h, u, True) for u in controls]
                        if all(labels):
                            vals = [v["rev"] for v in labels]
                            row.update(control_mean=float(np.mean(vals)), diff=target["rev"]-np.mean(vals),
                                       control_days=[int(bars.index[u].value//86_400_000_000_000) for u in controls],
                                       control_values=vals,
                                       control_R=float(np.mean([v["r_score"] for v in labels])),
                                       control_extension=float(np.mean([v["extension"] for v in labels])))
                    records.append(row)
            collected[name].extend(records)
            matches_meta.append(dict(symbol=symbol, detector=name, raw_all=raw_n, phase_n=len(ev), matched_n=len(matched)))
        if (sid+1) % 10 == 0:
            print(f"PREPARE {timeframe}/{phase} {sid+1}/{len(frames)}", flush=True)
    return {name: pd.DataFrame(rows) for name, rows in collected.items()}, matches_meta


def public_fit(fit):
    if not fit:
        return {"status": "insufficient", "p": 1., "coef": np.nan, "t": np.nan}
    return {k: v for k, v in fit.items() if k not in ("beta", "sse", "se", "se_date")}


def matched_inference(frame, h, bpd):
    """Paired/date and actual-control-date score aggregation, conservative larger SE."""
    y = frame["diff"].to_numpy(float)
    paired = rs.ols(np.ones((len(y), 1)), y, frame.day.to_numpy(int), h, bpd)
    if not paired:
        return None
    signal = frame.rev.to_numpy(float)
    controls = np.array(frame.control_values.tolist(), float)
    days = np.r_[frame.day.to_numpy(int), np.array(frame.control_days.tolist(), int).ravel()]
    scores = np.r_[signal-signal.mean(), (-(controls-controls.mean())/5).ravel()][:, None]
    actual = rs.clustered_inference(np.array([[1/len(y)]]), scores, days, h, bpd, nobs=len(y))
    if actual and actual["se"] > paired["se"]:
        paired.update(actual)
        paired["t"] = paired["coef"]/actual["se"]
        paired["t_date"] = paired["coef"]/max(actual["se_date"], 1e-30)
        paired["p"] = rs.student_p(paired["t"], actual["p_df"])
    return paired


def refine_matched_contrast(fit, frame, category, h, bpd):
    """Preserve actual dates for control influence in a difference-of-differences."""
    event_scores, control_scores = np.zeros(len(frame)), np.zeros((len(frame), 5))
    signal = frame.rev.to_numpy(float)
    controls = np.array(frame.control_values.tolist(), float)
    for label, sign in ((category, 1), (None, -1)):
        mask = (frame["class"].to_numpy() == category) if label else (frame["class"].to_numpy() != category)
        n = mask.sum()
        event_scores[mask] = sign*(signal[mask]-signal[mask].mean())/n
        control_scores[mask] = -sign*(controls[mask]-controls[mask].mean())/(5*n)
    days = np.r_[frame.day.to_numpy(int), np.array(frame.control_days.tolist(), int).ravel()]
    scores = np.r_[event_scores, control_scores.ravel()][:, None]
    inf = rs.clustered_inference(np.ones((1, 1)), scores, days, h, bpd, nobs=len(frame))
    implied_se = abs(fit["coef"]/fit["t"]) if fit["t"] else 0.
    if inf and inf["se"] > implied_se:
        fit.update(inf)
        fit["t"] = fit["coef"]/inf["se"]
        fit["t_date"] = fit["coef"]/max(inf["se_date"], 1e-30)
        fit["p"] = rs.student_p(fit["t"], inf["p_df"])
    return fit


def matched_regression(frame, x, h, bpd):
    """Same OLS/HAC; additionally preserve actual control-date influence scores."""
    fit = rs.ols(x, frame["diff"].to_numpy(float), frame.day.to_numpy(int), h, bpd)
    if not fit:
        return None
    controls = np.asarray(frame.control_values.tolist(), float)
    # Linear projection is additive: signal residual minus control residual.
    bread = np.linalg.inv(x.T@x)
    signal = frame.rev.to_numpy(float)
    beta_signal = bread@(x.T@signal)
    beta_controls = bread@(x.T@controls.mean(axis=1))
    signal_scores = x*(signal-x@beta_signal)[:, None]
    control_scores = -x[:, None, :]*(controls-(x@beta_controls)[:, None])[:, :, None]/5
    days = np.r_[frame.day.to_numpy(int), np.asarray(frame.control_days.tolist(), int).ravel()]
    scores = np.concatenate([signal_scores, control_scores.reshape(-1, x.shape[1])])
    actual = rs.clustered_inference(bread, scores, days, h, bpd, nobs=len(frame))
    if actual and actual["se"] > fit["se"]:
        fit.update(actual)
        fit["t"] = fit["coef"]/actual["se"]
        fit["t_date"] = fit["coef"]/max(actual["se_date"], 1e-30)
        fit["p"] = rs.student_p(fit["t"], actual["p_df"])
    return fit


def sufficient(frame, direction):
    if len(frame) < rs.MIN_OBSERVATIONS or frame.day.nunique() < rs.MIN_CLUSTERS:
        return False
    return direction != "pooled" or min((frame.d == 1).sum(), (frame.d == -1).sum()) >= 30


def test_cell(frame, spec, h, timeframe, direction):
    name, hyp, kind, a, b, outcome, sign = spec
    subset = frame.loc[frame["class"].isin([a, b] if b else [a])].copy()
    columns = [outcome] if kind != "regression" else ["diff", "decay_flag", "location_flag", "channel_break", "rr_flag"]
    valid = np.isfinite(subset[columns].to_numpy(float)).all(axis=1)
    subset = subset.loc[valid]
    base = {"n": len(subset), "clusters": subset.day.nunique(), "hypothesis": hyp, "sign": sign,
            "outcome": outcome, "status": "insufficient", "p": 1., "coef": np.nan, "t": np.nan}
    if not sufficient(subset, direction):
        return base
    bpd = 24 if timeframe == "1h" else 1
    if kind == "contrast":
        if min((subset["class"] == a).sum(), (subset["class"] == b).sum()) < 30:
            return base
        # Shared contrast helper handles exact same clustered OLS and small-bucket rule.
        bucket = np.where(subset["class"] == a, 2, 0)
        fit, _ = rs.contrast_test(bucket, subset[outcome].to_numpy(float), subset.day.to_numpy(int), h, bpd)
        if "coef" not in fit:
            return base
        if outcome == "diff":
            fit = refine_matched_contrast(fit, subset, a, h, bpd)
    elif kind == "regression":
        flags = ["decay_flag", "location_flag", "channel_break", "rr_flag"]
        if any(min(subset[f].sum(), len(subset)-subset[f].sum()) < 30 for f in flags):
            return base
        order = [f for f in flags if f != outcome]+[outcome]
        x = np.column_stack([np.ones(len(subset)), subset[order].to_numpy(float)])
        fit = matched_regression(subset, x, h, bpd)
    else:
        if outcome in ("r_score", "stop_minus_target") and min((subset.outcome == 1).sum(), (subset.outcome == -1).sum()) < 30:
            return base
        fit = matched_inference(subset, h, bpd) if outcome == "diff" else rs.ols(
            np.ones((len(subset), 1)), subset[outcome].to_numpy(float), subset.day.to_numpy(int), h, bpd)
    if not fit or not np.isfinite(fit.get("t", np.nan)):
        return base
    p_two = fit["p"]
    p_one = p_two/2 if sign*fit["t"] >= 0 else 1-p_two/2
    result = {**base, **public_fit(fit), "p_two": p_two, "p": p_one, "status": "ok"}
    if kind == "regression":
        result["model_order"] = ["intercept"]+order
        result["model_beta"] = fit["beta"].tolist()
    return result


def partition(frame, direction, cycle):
    selected = frame
    if direction != "pooled":
        selected = selected.loc[selected.d == (1 if direction == "up" else -1)]
    if cycle != "all":
        year = pd.to_datetime(selected.timestamp, utc=True).dt.year
        mask = year < 2023 if cycle == "pre2023" else (year.between(2023, 2024) if cycle == "2023_2024" else year.between(2025, 2026))
        selected = selected.loc[mask]
    return selected


def run_tests(events, thresholds, timeframe, candidates=None):
    rows = []
    for param, (_, cuts) in parameter_grid().items():
        frame = events[param.rsplit("_q", 1)[0]].copy()
        if frame.empty:
            # Keep the full family even for an empty panel.
            frame = pd.DataFrame(columns=FEATURE_COLUMNS+["h", "rev", "rev_net", "diff", "r_score", "extension", "outcome", "breakout_net", "stop_minus_target"])
        th = thresholds[param]
        frame["class"] = classify(frame, th)
        frame["location_flag"] = (frame.location >= th["location_cut"]).astype(int)
        for h in HORIZONS[timeframe]:
            horizon = frame.loc[frame.h == h]
            for direction in DIRECTIONS:
                for cycle in CYCLES:
                    cell = partition(horizon, direction, cycle)
                    for spec in SPECS:
                        key = (timeframe, param, h, direction, cycle, spec[0])
                        if candidates is not None and key not in candidates:
                            continue
                        fit = test_cell(cell, spec, h, timeframe, direction)
                        rows.append({**dict(zip(KEY, key)), **fit})
    return pd.DataFrame(rows)


def describe(events, thresholds, timeframe, phase, market):
    rows = []
    frame = events[DEFAULT.rsplit("_q", 1)[0]].copy()
    if frame.empty:
        return pd.DataFrame()
    frame["class"] = classify(frame, thresholds[DEFAULT])
    for h in HORIZONS[timeframe]:
        for cls in CLASSES:
            cell = frame[(frame.h == h) & (frame["class"] == cls)]
            daycounts = cell.groupby("day").size()
            rows.append(dict(market=market, timeframe=timeframe, phase=phase, h=h, category=cls,
                             n=len(cell), share=len(cell)/max(1, int((frame.h == h).sum())),
                             matched_n=int(cell["diff"].notna().sum()), dates=cell.day.nunique(),
                             max_date_share=float(daycounts.max()/len(cell)) if len(cell) else np.nan,
                             up=int((cell.d == 1).sum()), down=int((cell.d == -1).sum()),
                             rev=cell.rev.mean(), rev_net=cell.rev_net.mean(), difference=cell["diff"].mean(),
                             control_rev=cell.control_mean.mean(), r_score=cell.r_score.mean(), y2_n=int(cell.r_score.notna().sum()),
                             extension_n=int(cell.extension.notna().sum()), rr_n=int(cell.rr_space.notna().sum()),
                             breakout_net=cell.breakout_net.mean(), breakout_n=int(cell.breakout_net.notna().sum()),
                             target_first=(cell.outcome.dropna() == 1).mean(), stop_first=(cell.outcome.dropna() == -1).mean(),
                             neither=(cell.outcome.dropna() == 0).mean(), invalid_y2=int(cell.outcome.isna().sum()),
                             extension=cell.extension.mean(), control_R=cell.control_R.mean(),
                             control_extension=cell.control_extension.mean(), rr_space=cell.rr_space.mean(),
                             overlap=cell.overlap.mean(), decay=cell.momentum_decay.mean()))
    return pd.DataFrame(rows)


def load_panel(timeframe, market):
    frames, inventory = [], []
    for path in rs.data_paths(timeframe, market):
        f = rs.load_bars(path, timeframe, market)
        if len(f):
            symbol = ("US:" if market == "us" else "")+path.stem
            frames.append((symbol, f))
            inventory.append({"symbol": symbol, "rows": len(f), "start": str(f.index[0]), "end": str(f.index[-1]),
                              "bytes": path.stat().st_size, "sha256": digest(path)})
    if not frames:
        raise ValueError(f"No local data: {market}/{timeframe}")
    return frames, inventory


def save_events(path, events):
    # Trusted local pickle only; never read user/downloaded pickles.
    pd.to_pickle(events, path)


def run_training(out, timeframe, frames=None):
    out = Path(out)
    check_lock(out)
    if (out / "candidates.json").exists() or (out / f"crypto_{timeframe}.train.csv").exists():
        raise ValueError("Frozen experiment or existing training; use fresh out")
    if frames is None:
        require_calibration(out)
    frames, inventory = load_panel(timeframe, "crypto") if frames is None else (frames, [])
    times = pd.DatetimeIndex(np.concatenate([f.index.to_numpy() for _, f in frames]))
    split = study_split(pd.to_datetime(times, utc=True), timeframe)
    events, matchmeta = prepare_events(frames, timeframe, split, "train")
    thresholds = {}
    for param, (_, cuts) in parameter_grid().items():
        ev = events[param.rsplit("_q", 1)[0]]
        if ev.empty:
            ev = pd.DataFrame(columns=FEATURE_COLUMNS)
        else:
            ev = ev[ev.h == HORIZONS[timeframe][0]]
        thresholds[param] = fit_thresholds(ev, cuts)
    tests = run_tests(events, thresholds, timeframe)
    tests.to_csv(out / f"crypto_{timeframe}.train.csv", index=False)
    atomic(out / f"crypto_{timeframe}.thresholds.json", thresholds)
    atomic(out / f"crypto_{timeframe}.meta.json", {"split": split, "inventory": inventory, "matches": matchmeta})
    save_events(out / f"crypto_{timeframe}.train.events.pkl", events)
    describe(events, thresholds, timeframe, "train", "crypto").to_csv(out / f"crypto_{timeframe}.train.description.csv", index=False)
    print(f"TRAIN crypto/{timeframe}: {len(tests)} tests", flush=True)
    return tests


def select_training(train):
    result = train.copy()
    result["q"] = rs.bh_adjust(result.p.fillna(1).to_numpy())
    result["candidate"] = (result.q <= .1) & (result.status == "ok")
    plateau = {}
    for key, group in result.groupby(["timeframe", "h", "direction", "cycle", "test"], sort=False):
        grid = {r.param: r.sign*r.t if np.isfinite(r.t) else -1e30 for r in group.itertuples()}
        sweep = sweep_plateau(lambda value: value, grid, DEFAULT, min_pass_ratio=.6, score_threshold=1.5-1e-12)
        plateau[key] = sweep.plateau_ratio
    result["plateau"] = [plateau[tuple(row[c] for c in ("timeframe", "h", "direction", "cycle", "test"))] for _, row in result.iterrows()]
    return result


def freeze(out):
    out = Path(out)
    check_lock(out)
    if (out / "candidates.json").exists():
        raise ValueError("Already frozen")
    paths = [out / f"crypto_{tf}.train.csv" for tf in HORIZONS]
    train = select_training(pd.concat([pd.read_csv(p) for p in paths], ignore_index=True))
    train.to_csv(out / "crypto_training_fdr.csv", index=False)
    files = paths+[out / "crypto_training_fdr.csv"]+[out / f"crypto_{tf}.{suffix}" for tf in HORIZONS for suffix in ("thresholds.json", "meta.json", "train.events.pkl")]
    evaluation = train.loc[train.candidate, KEY].copy()
    h5 = train[(train.hypothesis == "H5") & train.candidate]
    companion_columns = ["timeframe", "param", "h", "direction", "cycle"]
    if len(h5):
        companions = train[train.hypothesis == "H5"].merge(h5[companion_columns].drop_duplicates(), on=companion_columns)
        evaluation = pd.concat([evaluation, companions[KEY]], ignore_index=True).drop_duplicates()
    manifest = {"version": VERSION, "frozen_at_utc": pd.Timestamp.now(tz="UTC").isoformat(),
                "tests": len(train), "train_fdr": int(train.candidate.sum()),
                "candidate_keys": train.loc[train.candidate, KEY].values.tolist(),
                "evaluation_keys": evaluation.values.tolist(),
                "files": {p.name: digest(p) for p in files}, "protocol_sha256": digest(out / "preregistration-lock.json")}
    atomic(out / "candidates.json", manifest)
    (out / "candidates.sha256").write_text(digest(out / "candidates.json")+"\n")
    print(f"FREEZE {len(train)} tests, {manifest['train_fdr']} FDR candidates", flush=True)
    return train


def verify_frozen(out):
    out = Path(out)
    check_lock(out)
    if digest(out / "candidates.json") != (out / "candidates.sha256").read_text().strip():
        raise ValueError("Candidate hash changed")
    frozen = json.loads((out / "candidates.json").read_text())
    if frozen["protocol_sha256"] != digest(out / "preregistration-lock.json"):
        raise ValueError("Protocol lock changed")
    if any(digest(out / name) != value for name, value in frozen["files"].items()):
        raise ValueError("Frozen input changed")
    return frozen


def economic_gate(events, thresholds, row):
    frame = events[row.param.rsplit("_q", 1)[0]].copy()
    if frame.empty:
        return False
    frame["class"] = classify(frame, thresholds[row.param])
    cell = partition(frame[frame.h == row.h], row.direction, row.cycle)
    medium = cell[cell["class"] == "medium"]
    if row.hypothesis == "H2":
        return bool(medium["diff"].mean() > 0)
    if row.test in ("H1_med_steep", "H3_no_edge", "H3_space", "H4_no_edge", "H4_med_flat"):
        category = "steep" if row.hypothesis == "H1" else ("low" if row.hypothesis == "H3" else "flat")
        return bool(cell.loc[cell["class"] == category, "diff"].mean() <= 0)
    return True


def frozen_prediction_mse(events, thresholds, row, training):
    if row.hypothesis != "H5":
        return np.nan
    model = training.loc[tuple(getattr(row, c) for c in KEY)]
    if not isinstance(model.get("model_beta"), str):
        return np.nan
    beta, order = ast.literal_eval(model.model_beta), ast.literal_eval(model.model_order)
    frame = events[row.param.rsplit("_q", 1)[0]].copy()
    if frame.empty:
        return np.nan
    frame["class"] = classify(frame, thresholds[row.param])
    frame["location_flag"] = (frame.location >= thresholds[row.param]["location_cut"]).astype(int)
    frame = partition(frame[(frame.h == row.h) & (frame["class"] == "medium")], row.direction, row.cycle)
    frame = frame.loc[frame["diff"].notna()]
    if frame.empty:
        return np.nan
    x = np.column_stack([np.ones(len(frame)), frame[order[1:]].to_numpy(float)])
    return float(np.mean((frame["diff"].to_numpy(float)-x@np.asarray(beta))**2))


def evaluate(out, timeframe, market="crypto", frames=None):
    out = Path(out)
    frozen = verify_frozen(out)
    marker = out / f"{market}_{timeframe}.holdout.started.json"
    try:
        with marker.open("x") as f:
            json.dump({"started_utc": pd.Timestamp.now(tz="UTC").isoformat(), "candidate_sha256": digest(out / "candidates.json")}, f)
    except FileExistsError:
        raise ValueError("Holdout is single-use; already started") from None
    frames, inventory = load_panel(timeframe, market) if frames is None else (frames, [])
    if market == "crypto":
        meta = json.loads((out / f"crypto_{timeframe}.meta.json").read_text())
        if inventory and meta["inventory"] != inventory:
            raise ValueError("Source data changed since training")
        split = meta["split"]
    else:
        times = pd.to_datetime(np.concatenate([f.index.to_numpy() for _, f in frames]), utc=True)
        split = study_split(times, timeframe)
        atomic(out / f"us_{timeframe}.meta.json", {"split": split, "inventory": inventory})
    thresholds = json.loads((out / f"crypto_{timeframe}.thresholds.json").read_text())
    candidates = {tuple(v) for v in frozen["evaluation_keys"] if v[0] == timeframe}
    training = pd.read_csv(out / "crypto_training_fdr.csv").set_index(KEY)
    results, descriptions = [], []
    for phase in (("train", "holdout") if market == "us" else ("holdout",)):
        events, matchmeta = prepare_events(frames, timeframe, split, phase)
        save_events(out / f"{market}_{timeframe}.{phase}.events.pkl", events)
        atomic(out / f"{market}_{timeframe}.{phase}.matches.json", matchmeta)
        tests = run_tests(events, thresholds, timeframe, candidates)
        if len(tests):
            tests["phase"] = phase
            tests["frozen_prediction_mse"] = [frozen_prediction_mse(events, thresholds, row, training) for row in tests.itertuples()]
            tests["economic"] = [economic_gate(events, thresholds, row) for row in tests.itertuples()]
            tests["confirmed"] = (tests.sign*tests.t >= 2) & (tests.status == "ok")
            results.append(tests)
        descriptions.append(describe(events, thresholds, timeframe, phase, market))
    result = pd.concat(results, ignore_index=True) if results else pd.DataFrame(columns=KEY+["phase", "coef", "t", "p", "confirmed", "economic"])
    result.to_csv(out / f"{market}_{timeframe}.holdout.csv", index=False)
    pd.concat(descriptions, ignore_index=True).to_csv(out / f"{market}_{timeframe}.description.csv", index=False)
    print(f"EVALUATE {market}/{timeframe}: {len(result)} candidate rows", flush=True)
    return result


def synthetic_bars(seed, bars=4200, timeframe="1d", volatility=.008):
    rng = np.random.default_rng(seed)
    returns = rng.normal(0, volatility, bars)
    c = 100*np.exp(np.cumsum(returns))
    o = np.r_[c[0], c[:-1]]
    # Independent sub-bar wicks, no predictability of future close returns.
    w = rng.uniform(.125, .75, (2, bars))*volatility
    return pd.DataFrame(dict(open=o, high=np.maximum(o, c)*(1+w[0]),
                             low=np.minimum(o, c)*(1-w[1]), close=c, volume=1.),
                        index=pd.date_range("2014-01-01", periods=bars, freq="h" if timeframe == "1h" else "D", tz="UTC"))


def planted_bars(seed, bars=4200, timeframe="1d", volatility=.008):
    """Fixed artificial motif repeated with independent scale/noise; calibration only."""
    rng = np.random.default_rng(seed)
    frame = synthetic_bars(seed, bars, timeframe, volatility)
    close = frame.close.to_numpy().copy()
    # Three slopes mixed ex ante; medium motif is followed by a large downward drift.
    # 140-bar blocks permit complete independent pattern formation and matching.
    for start in range((500 if timeframe == "1h" else 140)+int(rng.integers(0, 100)), bars-140, 140):
        level = close[start]
        kind = rng.integers(0, 3)
        gain = (.012, .04, .12)[kind]
        knots = [0, 8, 18, 28, 38, 48, 58, 62, 86, 112, 139]
        values = np.array([0, -.03, .04, -.01, .04+gain/2, .005, .04+gain, .035+gain,
                           -.12 if kind == 1 else .03+gain, -.14 if kind == 1 else .02, 0])
        values *= rng.choice([-1, 1])*volatility/.008
        path = level*np.exp(np.interp(np.arange(140), knots, values))
        path *= np.exp(rng.normal(0, .0001*volatility/.008, 140))
        close[start:start+140] = path
    op = close*np.exp(rng.normal(0, .00005*volatility/.008, bars))
    frame.loc[:, "open"] = op
    frame.loc[:, "close"] = close
    frame.loc[:, "high"] = np.maximum(op, close)*(1+.0005*volatility/.008)
    frame.loc[:, "low"] = np.minimum(op, close)*(1-.0005*volatility/.008)
    return frame


def synthetic_experiment(seed=100, planted=False, bars=6000, assets=20, panel=None, scales=None):
    """Same feature/matching/BH/candidate-only confirmation pipeline, both frequencies."""
    train_rows, storage, desc = [], {}, []
    maker = planted_bars if planted else synthetic_bars
    for tf in HORIZONS:
        n_assets, n_bars = panel[tf] if panel else (assets, bars)
        sigmas = [v["volatility"] for v in scales[tf]] if scales else [.008]
        frames = [(f"S{i}", maker(seed*1000+i, n_bars, tf, sigmas[i % len(sigmas)])) for i in range(n_assets)]
        split = study_split(frames[0][1].index, tf)
        ev, _ = prepare_events(frames, tf, split, "train")
        th = {param: fit_thresholds(ev[param.rsplit("_q", 1)[0]].drop_duplicates(["symbol", "t"]) if len(ev[param.rsplit("_q", 1)[0]]) else pd.DataFrame(columns=FEATURE_COLUMNS), cuts)
              for param, (_, cuts) in parameter_grid().items()}
        train_rows.append(run_tests(ev, th, tf))
        storage[tf] = frames, split, th
        desc.append(describe(ev, th, tf, "train", "synthetic"))
    train = select_training(pd.concat(train_rows, ignore_index=True))
    keys = {tuple(v) for v in train.loc[train.candidate, KEY].values.tolist()}
    for _, row in train[(train.hypothesis == "H5") & train.candidate].iterrows():
        for spec in SPECS[-4:]:
            keys.add(tuple(row[c] if c != "test" else spec[0] for c in KEY))
    holds = []
    for tf, (frames, split, th) in storage.items():
        ev, _ = prepare_events(frames, tf, split, "holdout")
        holds.append(run_tests(ev, th, tf, keys))
        desc.append(describe(ev, th, tf, "holdout", "synthetic"))
    hold = pd.concat(holds, ignore_index=True)
    if len(hold):
        selected_keys = {tuple(v) for v in train.loc[train.candidate, KEY].values.tolist()}
        is_candidate = np.array([tuple(v) in selected_keys for v in hold[KEY].values.tolist()])
        confirmed = int(((hold.sign*hold.t >= 2) & (hold.status == "ok") & is_candidate).sum())
    else:
        confirmed = 0
    summary = dict(seed=seed, planted=planted, tests=len(train), train_fdr=int(train.candidate.sum()), confirmed=confirmed,
                   panel={tf: {"assets": panel[tf][0] if panel else assets, "bars": panel[tf][1] if panel else bars} for tf in HORIZONS},
                   pattern_rows=int(pd.concat(desc).query("phase=='train'").n.sum()) if any(len(d) for d in desc) else 0,
                   sufficient=int((train.status == "ok").sum()),
                   sufficient_edge=int(((train.status == "ok") & train.test.isin(["H2_edge", "H3_no_edge", "H4_no_edge"])).sum()))
    summary["predictive_fdr"] = int(train.candidate.sum())
    summary["sufficient_by_test"] = {k: int(v) for k, v in train[train.status == "ok"].groupby("test").size().items()}
    summary["fdr_by_test"] = {k: int(v) for k, v in train[train.candidate].groupby("test").size().items()}
    confirmed_rows = hold[(hold.sign*hold.t >= 2) & (hold.status == "ok")] if len(hold) else hold
    if len(confirmed_rows):
        confirmed_rows = confirmed_rows.merge(train.loc[train.candidate, KEY], on=KEY)
    summary["confirmed_by_test"] = {k: int(v) for k, v in confirmed_rows.groupby("test").size().items()} if len(confirmed_rows) else {}
    summary["planted_edge_keys"] = confirmed_rows.loc[confirmed_rows.test == "H2_edge", KEY].values.tolist() if len(confirmed_rows) else []
    return summary, train, hold, pd.concat(desc, ignore_index=True)


LARGE_PANEL = {"1h": (50, 15000), "1d": (100, 2500)}


def volatility_scales(out):
    """Training-only scales; no detector or new real forward-target evaluation."""
    path = Path(out)/"volatility-scales.json"
    if path.exists():
        return json.loads(path.read_text())["scales"]
    scales, inventories = {}, {}
    for tf in HORIZONS:
        frames, inventories[tf] = load_panel(tf, "crypto")
        times = pd.to_datetime(np.concatenate([f.index.to_numpy() for _, f in frames]), utc=True)
        split = study_split(times, tf)
        scales[tf] = []
        for symbol, frame in sorted(frames, key=lambda pair: pair[0]):
            returns = np.log(frame.close).diff().where(phase_mask(frame, split, "train", tf))
            source = "market_training_window"
            if returns.notna().sum() < 30:
                # Newly listed assets may have no bars before the pooled cutoff.
                returns = np.log(frame.close.iloc[:max(31, int(len(frame)*.6))]).diff()
                source = "symbol_first_60pct_volatility_only"
            sigma = float(returns.std())
            if not np.isfinite(sigma) or sigma <= 0:
                raise ValueError(f"Invalid training volatility: {tf}/{symbol}")
            scales[tf].append({"symbol": symbol, "volatility": sigma, "n": int(returns.notna().sum()), "source": source})
    atomic(path, {"scales": scales, "inventory": inventories, "estimated_utc": pd.Timestamp.now(tz="UTC").isoformat()})
    return scales


def run_controls(out, planted=False):
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    scales = volatility_scales(out)
    name = "planted" if planted else "null"
    summaries, descriptions = [], []
    for seed in ([7] if planted else range(100, 110)):
        start = time.monotonic()
        summary, train, hold, description = synthetic_experiment(seed, planted, panel=LARGE_PANEL, scales=scales)
        summary.update(code_sha256=digest(__file__), protocol_sha256=digest(PREREG),
                       scale_sha256=digest(out/"volatility-scales.json"), completed_utc=pd.Timestamp.now(tz="UTC").isoformat())
        summaries.append(summary)
        description["seed"] = seed
        descriptions.append(description)
        train.to_csv(out/f"{name}.{seed}.train.csv", index=False)
        hold.to_csv(out/f"{name}.{seed}.holdout.csv", index=False)
        atomic(out/f"{name}.json", summaries)
        pd.concat(descriptions).to_csv(out/f"{name}.descriptions.csv", index=False)
        print(json.dumps(summary)+f" elapsed={time.monotonic()-start:.1f}s", flush=True)
    return summaries


def calibration_status(null_summaries, planted_summaries, stress_summaries=None):
    """Global-null family rejection probability, assessed at real-scale panel size."""
    def large(v):
        return all(v.get("panel", {}).get(tf, {}).get("assets", 0) >= a and
                   v.get("panel", {}).get(tf, {}).get("bars", 0) >= b for tf, (a, b) in LARGE_PANEL.items())
    complete = sorted(v["seed"] for v in null_summaries) == list(range(100,110)) and all(large(v) for v in null_summaries)
    rejects = sum(v["train_fdr"] > 0 for v in null_summaries)
    train_n = sum(v["train_fdr"] for v in null_summaries)
    hold_n = sum(v["confirmed"] for v in null_summaries)
    power = any(v.get("seed") == 7 and large(v) and v.get("planted_edge_keys") for v in planted_summaries)
    passed = complete and rejects <= 3 and hold_n <= max(1, train_n*.25) and power and all(v.get("sufficient_edge", 0) > 0 for v in null_summaries)
    return {"passed": bool(passed), "complete_large_panels": complete, "null_reject_families": rejects,
            "null_train_rejections": train_n, "null_hold_confirmations": hold_n, "planted_medium_detected": bool(power),
            "criterion": "10 fixed large panels; <=3 with any train BH rejection; hold<=max(1,train*.25); matched edge estimable; planted H2_edge same-key train+hold",
            "reason": "passed" if passed else "large-panel random-walk calibration failed or incomplete"}


def require_calibration(out):
    out = Path(out)
    null = json.loads((out/"null.json").read_text()) if (out/"null.json").exists() else []
    planted = json.loads((out/"planted.json").read_text()) if (out/"planted.json").exists() else []
    if not calibration_status(null, planted)["passed"]:
        raise ValueError("Real data inference requires passing large-panel calibration")
    for item in null+planted:
        if item["code_sha256"] != digest(__file__) or item["protocol_sha256"] != digest(PREREG) or item["scale_sha256"] != digest(out/"volatility-scales.json"):
            raise ValueError("Calibration code/protocol/scales changed; recalibrate before real inference")


def make_report(out, report):
    out = Path(out)
    verify_frozen(out)
    train = pd.read_csv(out / "crypto_training_fdr.csv")
    hold = pd.concat([pd.read_csv(out / f"crypto_{tf}.holdout.csv") for tf in HORIZONS], ignore_index=True)
    if len(hold):
        result = train.merge(hold.rename(columns={c: "hold_"+c for c in hold.columns if c not in KEY}), on=KEY, how="left")
    else:
        result = train.copy()
        for c in ("coef", "t", "p", "confirmed", "economic", "status"):
            result["hold_"+c] = np.nan
    result["confirmed"] = result.candidate & result.hold_confirmed.fillna(False).astype(bool)
    result["effective"] = result.confirmed & (result.plateau >= .6) & result.hold_economic.fillna(False).astype(bool)
    contrasts = result.set_index(KEY)
    for index, row in result[result.test.isin(["H3_no_edge", "H4_no_edge"])].iterrows():
        required = "H2_med_low" if row.hypothesis == "H3" else "H4_med_flat"
        comp = contrasts.loc[tuple(row[c] if c != "test" else required for c in KEY)]
        if not (comp.candidate and comp.confirmed and comp.plateau >= .6):
            result.loc[index, "effective"] = False
    for _, group in result[result.hypothesis == "H5"].groupby(["timeframe", "param", "h", "direction", "cycle"]):
        if not (len(group) == 4 and (group.coef > 0).all() and (group.hold_coef > 0).all()):
            result.loc[group.index, "effective"] = False
    null = json.loads((out / "null.json").read_text())
    planted = json.loads((out / "planted.json").read_text())
    calibration = calibration_status(null, planted)
    result["verdict"] = np.where(result.effective, "支持", np.where(result.status == "insufficient", "样本不足", "不支持"))
    if not calibration["passed"]:
        result["effective"] = False
        result["verdict"] = "不可推断（校准未通过）"
    result.to_csv(out / "crypto.results.csv", index=False)
    default = result[result.param == DEFAULT]
    effective = default[default.effective]
    summary = dict(version=VERSION, calibration=calibration, tests=len(train), train_fdr=int(train.candidate.sum()),
                   holdout_confirmed=int(result.confirmed.sum()), effective_default=len(effective),
                   effective_keys=effective[KEY].values.tolist(), sufficient_tests=int((train.status == "ok").sum()),
                   default_train_fdr=int(default.candidate.sum()), default_holdout_confirmed=int(default.confirmed.sum()),
                   artifact_directory=str(out.resolve()), candidate_sha256=digest(out / "candidates.json"),
                   holdout_disclosure="v1保留窗已被看过；v2按修正定义评估一次，不是全新独立保留样本")
    descriptions = pd.concat([pd.read_csv(out / f"crypto_{tf}.train.description.csv") for tf in HORIZONS]+
                             [pd.read_csv(out / f"{market}_{tf}.description.csv") for market, tf in
                              (("crypto", "1h"), ("crypto", "1d"), ("us", "1d"))], ignore_index=True)
    geometry = []
    for r in descriptions.itertuples():
        base = {c: getattr(r, c) for c in ("market", "timeframe", "phase", "h", "category", "n")}
        for metric, ncol in (("r_score", "y2_n"), ("target_first", "y2_n"), ("stop_first", "y2_n"),
                             ("neither", "y2_n"), ("extension", "extension_n"), ("rr_space", "rr_n"), ("breakout_net", "breakout_n")):
            geometry.append({**base, "metric": metric, "estimate": getattr(r, metric), "valid_n": getattr(r, ncol),
                             "label": "由形态几何机械决定，不构成预测证据" if metric != "breakout_net" else "未匹配突破收益，仅描述"})
    for _, group in descriptions.groupby(["market", "timeframe", "phase", "h"]):
        rows = group.set_index("category")
        for test, a, b, metric in (("H1_extension", "steep", "medium", "extension"), ("H3_space", "medium", "low", "rr_space")):
            geometry.append({**{c: group.iloc[0][c] for c in ("market", "timeframe", "phase", "h")},
                             "category": a+"-"+b, "metric": test, "estimate": rows.loc[a, metric]-rows.loc[b, metric],
                             "n": rows.loc[a, "n"]+rows.loc[b, "n"], "valid_n": rows.loc[a, "n"]+rows.loc[b, "n"],
                             "label": "由形态几何机械决定，不构成预测证据"})
        steep = rows.loc["steep"]
        geometry.append({**{c: group.iloc[0][c] for c in ("market", "timeframe", "phase", "h")},
                         "category": "steep", "metric": "H1_stop_first", "estimate": steep.stop_first-steep.target_first,
                         "n": steep.n, "valid_n": steep.y2_n, "label": "由形态几何机械决定，不构成预测证据"})
    geometry = pd.DataFrame(geometry)
    previous = json.loads((OUT.parent/"out_v1/summary.json").read_text()) if (OUT.parent/"out_v1/summary.json").exists() else {}
    comparison = pd.DataFrame([{"version": v.get("version"), "calibration_passed": v.get("calibration", {}).get("passed"),
                                **{c: v.get(c) for c in ("tests", "train_fdr", "holdout_confirmed", "effective_default")}}
                               for v in (previous, summary)])
    atomic(out / "summary.json", summary)
    SUMMARY.mkdir(parents=True, exist_ok=True)
    atomic(SUMMARY / "summary.json", summary)
    descriptions.to_csv(SUMMARY / "class-estimates.csv", index=False)
    geometry.to_csv(SUMMARY / "geometry-descriptive.csv", index=False)
    default.to_csv(SUMMARY / "default-tests.csv", index=False)
    comparison.to_csv(SUMMARY / "v1-v2.csv", index=False)
    for name in ("null.json", "planted.json", "preregistration-lock.json"):
        (SUMMARY / name).write_bytes((out / name).read_bytes())
    # Old small-panel/stress summaries belong to the immutable v1 archive.
    for name in ("null-stress.json", "verification.json"):
        (SUMMARY / name).unlink(missing_ok=True)
    def fmt(x, scale=1):
        return "—" if pd.isna(x) else f"{float(x)*scale:.3f}"
    lines = ["# 三推 / 楔形信号研究 v2", "", "## 校准结果", "",
             f"大面板校准：**{'通过' if calibration['passed'] else '未通过，全部不可推断'}**。固定10种子100–109，每个种子同时含1h 50资产×15,000根和1d 100资产×2,500根；按真实逐币训练波动缩放，使用相同检测、分档、匹配、BH和候选保留流程。",
             f"训练出现任意拒绝的面板 {calibration['null_reject_families']}/10；总训练拒绝 {calibration['null_train_rejections']}，保留确认 {calibration['null_hold_confirmations']}。预注册允许≤3个面板任意拒绝且确认≤max(1,训练拒绝×25%)，并要求匹配Y1可估计及植入medium同键训练/保留检出。全局零假设下检查任意拒绝概率，未用行拒绝比例代替FDR。十种子仅为粗粒度校准。",
             "", "|种子|1h资产×根数|1d资产×根数|可估计项|训练BH通过|保留确认|", "|---:|---|---|---:|---:|---:|"]
    for v in null:
        lines.append(f"|{v['seed']}|50×15000|100×2500|{v['sufficient']}|{v['train_fdr']}|{v['confirmed']}|")
    lines += ["", "|植入种子|训练BH通过（全族）|保留确认（全族）|H2_edge训练通过|H2_edge同键保留确认|", "|---:|---:|---:|---:|---:|"]
    for v in planted:
        lines.append(f"|{v['seed']}|{v['train_fdr']}|{v['confirmed']}|{v['fdr_by_test'].get('H2_edge',0)}|{v['confirmed_by_test'].get('H2_edge',0)}|")
    lines += ["", "逐种子、逐检验族可估计及拒绝数见 [null.json](threepush-summary/null.json)，植入键见 [planted.json](threepush-summary/planted.json)。大面板全部训练表、候选保留表和波动来源清单在工作树 `artifacts/threepush/out_v2/`。校准未删除任何保留的Y1检验族。",
              "", "## v1 → v2 差异与保留窗披露", "",
              "v1真实训练与保留结果已经见过，全部无通过。v2修正方向仅来自合成诊断和两点逻辑，并非根据真实结果挑选。**v1保留窗已被看过；v2是修正定义后的一次评估，不是全新独立保留样本。**",
              "", "|版本|校准通过|训练检验数|训练BH通过|保留确认|默认有效项|", "|---|---|---:|---:|---:|---:|"]
    for r in comparison.itertuples():
        lines.append(f"|{r.version}|{r.calibration_passed}|{r.tests}|{r.train_fdr}|{r.holdout_confirmed}|{r.effective_default}|")
    lines += ["", "对照排除由[t−k,t+k]改为[t,t+k]，未来检出不再改变之前候选资格。推断仅保留匹配Y1、类别间差和H5在匹配Y1上的四项增量；H5原先回归成本后原始收益，现统一用形态−对照差。Y2、rr_space、Y3、先损占比退出BH和有效性门槛；未匹配突破收益也仅描述。",
              "检出器、pivot确认延迟k、非重叠、特征、训练三分位冻结、60/40时间切分、purge、聚类+HAC与邻域平台均保持。原文及哈希见[预注册v2](threepush-preregistration.md)，原代码、产物和报告存 `artifacts/threepush/out_v1/`，逐文件哈希清单可核验。",
              "", "## v2真实数据与逐项结果", "",
              f"加密推断共 **{len(train):,}** 项（含不足p=1），训练BH10%通过 **{summary['train_fdr']}**，保留确认 **{summary['holdout_confirmed']}**。可估计 {summary['sufficient_tests']} 项。默认训练通过 {summary['default_train_fdr']}，保留确认 {summary['default_holdout_confirmed']}；默认有效清单：**{'无' if effective.empty else '见下方支持项'}**。",
              "有效需训练q≤.1、保留同向t≥2、27格训练邻域同向t≥1.5比例≥60%及预注册Y1条件；H5还需四系数两阶段同向。未冻结候选不计算保留t。成本在形态与对照差中抵消，Y1差不是交易净利润。",
              ""]
    for market, tf in (("crypto", "1h"), ("crypto", "1d"), ("us", "1d")):
        meta = json.loads((out/f"{market}_{tf}.meta.json").read_text())
        sp = meta["split"]
        lines.append(f"- {market}/{tf}：{len(meta['inventory'])}资产；训练t<{sp['train_end_exclusive']}，保留t≥{sp['cutoff']}，purge={sp['purge_bars']}。")
    lines += ["", "### 三个原问题", ""]
    questions = [("陡峭三推逆势是否更差（Y1）", ("H1_med_steep",)),
                 ("中等斜率是否最好（有Y1优势且优于steep/low）", ("H2_edge", "H2_med_steep", "H2_med_low"))]
    for question, tests in questions:
        target = default[(default.direction == "pooled") & (default.cycle == "all") & default.test.isin(tests)]
        supported = any(set(tests).issubset(set(g.loc[g.effective, "test"])) for _, g in target.groupby(["timeframe", "h"]))
        verdict = "不可推断" if not calibration["passed"] else ("支持" if supported else ("样本不足" if (target.status == "insufficient").all() else "不支持"))
        lines.append(f"- **{question}：{verdict}。** 必须在同一频率/h满足全部子条件；不显著不等于无效或等效。")
    low = default[(default.direction == "pooled") & (default.cycle == "all") & (default.test == "H3_no_edge")]
    lowverdict = "支持" if low.effective.any() else ("样本不足" if (low.status == "insufficient").all() else "不支持")
    lines += [f"- **低斜率反转空间是否有限：不可推断。** rr_space是几何描述，不能据此推断预测能力；另列low匹配Y1负向主张：{lowverdict if calibration['passed'] else '不可推断'}。",
              "", "### 默认参数：各类形态数与匹配Y1点估计", "",
              "形态数按h完整标签；匹配数不足5个时不进Y1推断。阶段仅为train/holdout，小时/日线类别阈值各自只用训练拟合；美股沿用加密日线阈值。完整表含日期集中度、方向及原始/成本后收益，见[class-estimates.csv](threepush-summary/class-estimates.csv)。",
              "", "|市场/频率|h|类别|训练形态/匹配数|保留形态/匹配数|训练形态−对照Y1 bps|保留形态−对照Y1 bps|", "|---|---:|---|---:|---:|---:|---:|"]
    for (market, tf, h, category), group in descriptions.groupby(["market", "timeframe", "h", "category"], sort=False):
        tr = group[group.phase == "train"].iloc[0]
        ho = group[group.phase == "holdout"].iloc[0]
        lines.append(f"|{market}/{tf}|{h}|{category}|{tr.n}/{tr.matched_n}|{ho.n}/{ho.matched_n}|{fmt(tr.difference,10000)}|{fmt(ho.difference,10000)}|")
    lines += ["", "### 默认参数逐项推断（双向池化、全周期）", "",
              "完整分方向/周期表见[default-tests.csv](threepush-summary/default-tests.csv)，全部21,384项见out_v2/crypto.results.csv。效应单位bps；H5为匹配Y1的回归系数。",
              "", "|频率|h|子项|训练n|训练t|q|保留t|训练/保留效应bps|邻域%|判定|", "|---|---:|---|---:|---:|---:|---:|---|---:|---|"]
    for r in default[(default.direction == "pooled") & (default.cycle == "all")].itertuples():
        lines.append(f"|{r.timeframe}|{r.h}|{r.test}|{r.n}|{fmt(r.t)}|{fmt(r.q)}|{fmt(r.hold_t)}|{fmt(r.coef,10000)}/{fmt(r.hold_coef,10000)}|{fmt(r.plateau,100)}|{r.verdict}|")
    lines += ["", "### 几何及突破描述", "",
              "**Y2、rr_space、Y3、H1止损先到占比：由形态几何机械决定，不构成预测证据。** 仅报告点估计与有效样本数，不进入BH，不判有效/不支持。H3空间差仅是medium与low在当前定义下的几何事实；未匹配突破收益亦仅描述。完整原始点估计和非缺失n见[geometry-descriptive.csv](threepush-summary/geometry-descriptive.csv)。",
              "", "|频率/阶段|h|类别|Y2有效n|R均值|目标/止损/未到%|Y3 ATR(n)|rr_space(n)|", "|---|---:|---|---:|---:|---|---|---|"]
    for r in descriptions[descriptions.market == "crypto"].itertuples():
        lines.append(f"|{r.timeframe}/{r.phase}|{r.h}|{r.category}|{r.y2_n}|{fmt(r.r_score)}|{fmt(r.target_first,100)}/{fmt(r.stop_first,100)}/{fmt(r.neither,100)}|{fmt(r.extension)}({r.extension_n})|{fmt(r.rr_space)}({r.rr_n})|")
    lines += ["", "## 美股泛化", "", "使用crypto/1d冻结阈值，仅对加密训练冻结候选推断，不独立筛选；各类描述已列上表。"]
    us = pd.read_csv(out/"us_1d.holdout.csv")
    lines.append(f"美股候选推断行数：{len(us)}（训练/保留合计）；无日线候选时只描述。")
    lines += ["", "## 复现与验证", "",
              "指定Python：`/Users/zhangyun/workspace/nofx/.venv/bin/python`。模块 `scripts.backtest.threepush_study`；先 `--phase null`、`--phase planted`，通过后 `--phase all`。正式null/planted固定大面板，无缩小命令行覆盖；小面板仅函数级CI。全部产物在out_v2，代码/协议/校准先封存，训练候选/阈值冻结后保留single-use；已有结果不可覆盖重跑。",
              "新增专项测试覆盖排除前缀因果性、几何目标不入族、缩小面板的检测/匹配/BH/冻结确认流程；原pivot延迟、特征截断、匹配五分位、首次穿越和生命周期测试保留。测试及哈希核验结果见[verification.json](threepush-summary/verification.json)。",
              "", "## 最脆弱项", "",
              "- v1保留窗已被看过，v2不是全新独立验证；下一次实质修订需新的独立历史。十个合成种子仅能排查明显校准偏差，不能保证真实市场FDR或检验功效。",
              "- flat及日线匹配样本稀疏，200形态/40日期簇门槛与分方向/周期拆分导致大量不足；不支持不能解释为不存在效应。",
              "- 小时仅约三年，多币共处同一行情；日历周期不等于独立牛熊，日期聚类/HAC仍是近似，未充分刻画长记忆、匹配不确定性或所有跨币依赖。",
              "- 幸存者标的池、现存复权版本、27格参数自由度和严格操作定义限制泛化；Y1匹配只消除部分历史走势差异，未覆盖全部波动/趋势混杂。",
              "- 无订单成交、滑点、资金费率或容量模型；即使未来发现有效项也只能作为研究参考字段，不直接门控交易。本轮未改交易代码、未commit/push。", ""]
    Path(report).write_text("\n".join(lines), encoding="utf-8")
    print(json.dumps(summary, ensure_ascii=False, indent=2), flush=True)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--phase", choices=("seal", "train", "freeze", "evaluate", "report", "all", "null", "planted"), default="train")
    parser.add_argument("--timeframe", choices=("1h", "1d"), default="1h")
    parser.add_argument("--market", choices=("crypto", "us"), default="crypto")
    parser.add_argument("--out", type=Path, default=OUT)
    parser.add_argument("--report", type=Path, default=ROOT / "docs/research/threepush-study.md")
    args = parser.parse_args()
    if args.phase == "seal":
        seal(args.out)
    elif args.phase == "train":
        if args.market != "crypto":
            parser.error("US is generalization only; run evaluate after crypto freeze")
        run_training(args.out, args.timeframe)
    elif args.phase == "freeze":
        freeze(args.out)
    elif args.phase == "evaluate":
        evaluate(args.out, args.timeframe, args.market)
    elif args.phase == "report":
        make_report(args.out, args.report)
    elif args.phase in ("null", "planted"):
        run_controls(args.out, args.phase == "planted")
    else:
        seal(args.out)
        for tf in HORIZONS:
            run_training(args.out, tf)
        freeze(args.out)
        for tf in HORIZONS:
            evaluate(args.out, tf)
        evaluate(args.out, "1d", "us")
        make_report(args.out, args.report)


if __name__ == "__main__":
    main()
