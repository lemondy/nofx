"""Frozen Fibonacci/FVG/BOS study; all fills and P&L use ict_bt/Backtrader.

See docs/research/2026-11-01-fib-fvg-bos-prereg.md. No network or input writes.
"""

from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from dataclasses import replace
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import random
import time

import numpy as np

from . import ict, ict_bt, ict_fetch
from .ict_study import control_of, load_universe, make_job, tp_for


REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / "docs/research/2026-11-01-fib-fvg-bos-prereg.md"
DEFAULT_CACHE = "/Users/zhangyun/workspace/nofx/data/backtest_cache"
DEFAULT_FACTOR_ROOT = "/Users/zhangyun/workspace/nofx/data/factor_mining/crypto/daily"
SPLIT_MS = int(datetime(2026, 7, 1, tzinfo=timezone.utc).timestamp() * 1000)
START_MS = int(datetime(2025, 10, 15, tzinfo=timezone.utc).timestamp() * 1000)
END_MS = int(datetime(2026, 10, 18, tzinfo=timezone.utc).timestamp() * 1000)
VARIANTS = ("V1", "V2", "V3", "V4")
CHOICES = [
    "BOS is the first strict close beyond each latest confirmed pivot; each pivot is used once. "
    "The opposite starting pivot must precede P in position and be confirmed at BOS.",
    "H must occur strictly after BOS and confirm no later than BOS+48 (inclusive). "
    "All three FVG bars must lie inside the impulse leg; minimum width uses ATR14 at FVG confirmation; SL uses decision ATR14.",
    "Rank eligible directional FVGs by midpoint distance to the fib-zone center, then earliest formation. "
    "Apply overlap after ranking; V2 retains this ranking and removes only the overlap filter. A boundary touch counts as overlap.",
    "V3 is a subset of the already deduplicated ict full setups, including their original discount and 2R admission gates. "
    "It uses that full setup's displacement FVG and recomputes causal targets after moving entry to the intersection midpoint.",
    "V1/V2 have 48-bar limits. V3/V4 retain full's 24-bar limits; make_job's hardcoded validity is overridden with Setup.valid_bars.",
    "floor=1.0 uses make_job's existing 1h-ATR stop floor for both exits; these eight reference columns are excluded from K=8 and gates.",
    "No additional warmup or sampling. Use all cached bars in the registered date range. "
    "Keep load_universe's existing minimum of 3000 bars and BTC/ETH references.",
    "Controls use control_of with a fresh Random(7) for each cell and the same 6–48h offsets; "
    "control decision time determines its UTC ISO-week cluster and dev/holdout membership. Missing controls are counted, never resampled outside control_of.",
    "Overlap filtering is applied to the full chronological cell before splitting dev/holdout, independently for setups and controls, as in summarize.",
    "Mean cluster SE is CR1 for an intercept-only regression: sqrt(G/(G-1) * sum_g(sum_i(R_i-mean))^2 / n^2). "
    "Fewer than two weeks gives an undefined SE and cannot pass a t gate. Differences use the registered sum-of-variances rule, also for descriptive increments.",
    "G0 fixes the first close, permutes the n-1 close-to-close log returns independently within each symbol "
    "using one default_rng(20261101) stream in sorted-symbol order, and scales each original bar's O/H/L by new_close/old_close. "
    "Timestamps and per-bar O/H/L-to-close ratios remain fixed; reference symbols are shuffled too.",
    "G0 is evaluated before any real-data P&L; real setup counts may be detected beforehand. On G0 failure G1–G3 and increments are NOT_RUN.",
    "Execution inherits run_jobs/run_trade's through-limit fill, same-bar stop convention, finite feed window, "
    "and bar-resolution 72h timeout. End-of-cache trades follow the existing adapter's mark-to-market treatment.",
]


def fib_zone(low, high, sign):
    if sign == 1:
        return high - (high - low) * .786, high - (high - low) * .618
    return low + (high - low) * .618, low + (high - low) * .786


def _entry(gap, zone, fib):
    lo, hi = max(gap.lo, zone[0]), min(gap.hi, zone[1])
    return ((lo + hi) / 2 if lo <= hi else None) if fib else gap.ce


def _retarget(ctx, setup, ref, books=None):
    books = books or (ict._books(ctx.swings5), ict._books(ctx.swings1h))
    sign = 1 if setup.side == "long" else -1
    erl, p1, p2, p3, bias, inside, k = ict._targets(
        ctx, *books, setup.idx, ref, sign, setup.erl_level is not None)
    hourly = ctx.a1h
    atr_pct = float(ctx.atr1h[k] / hourly["c"][k] * 100) if k >= 0 and hourly["c"][k] else None
    result = replace(setup, ref=float(ref), tp_erl=erl, tp_p1=p1, tp_p2=p2, tp_p3=p3,
                     atr1h_pct=atr_pct,
                     flags=dict(setup.flags, htf_bias=bias, in_htf_fvg=inside,
                                p1=p1 is not None, p2=p2 is not None, p3=p3 is not None))
    return result if sign * (ref - result.sl) > 0 and tp_for(result, "erl") is not None else None


def _full_gaps(ctx):
    """The exact displacement predicate and ordering in ict.find_setups."""
    body = np.abs(ctx.a5["c"] - ctx.a5["o"])
    out = {"long": [], "short": []}
    for gap in ctx.fvgs5:
        m = gap.mid
        previous = body[max(0, m - 20):m]
        previous = previous[previous > 0]
        sign = 1 if gap.direction == "bull" else -1
        if (len(previous) and sign * (ctx.a5["c"][m] - ctx.a5["o"][m]) > 0
                and body[m] >= ict.DEFAULTS["body_mult"] * np.median(previous)
                and ctx.a5["h"][m] - ctx.a5["l"][m] >= ict.DEFAULTS["atr_mult"] * ctx.atr5[m]):
            out["long" if sign == 1 else "short"].append(gap)
    return out


def _fib_full(ctx, full):
    gaps = _full_gaps(ctx)
    ends = {side: np.array([g.end for g in gs]) for side, gs in gaps.items()}
    books = ict._books(ctx.swings5), ict._books(ctx.swings1h)
    out = []
    for setup in full:
        pos = int(np.searchsorted(ends[setup.side], setup.s + 1))
        if pos == len(gaps[setup.side]):
            raise RuntimeError("full setup has no source displacement FVG")
        gap = gaps[setup.side][pos]
        if gap.end > min(setup.idx, setup.s + ict.DEFAULTS["window"]) or gap.ce != setup.ref:
            raise RuntimeError("full setup's FVG cannot be reproduced exactly")
        low = float(np.min(ctx.a5["l"][setup.s:setup.idx + 1]))
        high = float(np.max(ctx.a5["h"][setup.s:setup.idx + 1]))
        sign = 1 if setup.side == "long" else -1
        ref = _entry(gap, fib_zone(low, high, sign), True)
        if ref is not None:
            result = _retarget(ctx, setup, ref, books)
            if result is not None:
                out.append(result)
    return out


def find_fib_setups(ctx, require_sweep=False, fib=True):
    """Causal V1/V2, or V3/full when require_sweep=True.

    Precomputed future pivots are searched only to locate the first post-BOS H;
    a candidate is emitted at H.avail, never at H.idx or the BOS close.
    """
    if require_sweep:
        full = [s for s in ict.find_setups(ctx) if s.trigger == "full"]
        return _fib_full(ctx, full) if fib else full
    a = ctx.a5
    books = ict._books(ctx.swings5), ict._books(ctx.swings1h)
    n = len(a["c"])
    directional_gaps = {d: [g for g in ctx.fvgs5 if g.direction == d] for d in ("bull", "bear")}
    gap_starts = {d: np.array([g.start for g in gs]) for d, gs in directional_gaps.items()}
    candidates = []
    for side, kind, other, sign, direction in (("long", "high", "low", 1, "bull"),
                                               ("short", "low", "high", -1, "bear")):
        points = [s for s in ctx.swings5 if s.kind == kind]
        indices = np.array([s.idx for s in points])
        for pos, p in enumerate(points):
            end = points[pos + 1].avail if pos + 1 < len(points) else n
            hits = np.flatnonzero(sign * (a["c"][p.avail:end] - p.price) > 0)
            if not len(hits):
                continue
            bos = int(p.avail + hits[0])
            hpos = int(np.searchsorted(indices, bos, side="right"))
            if hpos >= len(points):
                continue
            h = points[hpos]
            if h.avail > bos + 48:
                continue
            starts, _ = ict._known(books[0], other, bos)
            starts = starts[starts < p.idx]
            if not len(starts):
                continue
            start = int(starts[-1])
            key = "l" if sign == 1 else "h"
            leg_start = int(start + np.argmin(sign * a[key][start:bos + 1]))
            extreme = float(a[key][leg_start])
            low, high = (extreme, h.price) if sign == 1 else (h.price, extreme)
            if high <= low:
                continue
            zone = fib_zone(low, high, sign)
            gs = directional_gaps[direction]
            first = int(np.searchsorted(gap_starts[direction], leg_start))
            last = int(np.searchsorted(gap_starts[direction], h.idx - 2, side="right"))
            valid = [g for g in gs[first:last] if g.hi - g.lo >= .1 * ctx.atr5[g.end]]
            if not valid:
                continue
            gap = min(valid, key=lambda g: (abs(g.ce - sum(zone) / 2), g.end))
            ref = _entry(gap, zone, fib)
            if ref is None:
                continue
            idx = h.avail
            now = int(a["t_close"][idx])
            setup = ict.Setup(
                sym="", side=side, trigger="fib_bos" if fib else "bos_fvg", s=bos, idx=idx,
                kind="lim", ref=ref, sl=float(extreme - sign * .1 * ctx.atr5[idx]),
                extreme=extreme, tp_erl=None, tp_p1=None, tp_p2=None, tp_p3=None,
                flags={}, ny_hour=datetime.fromtimestamp(now / 1000, timezone.utc).astimezone(ict.NY).hour,
                atr5=float(ctx.atr5[idx]), atr1h_pct=None, t_close_ms=now, valid_bars=48)
            setup = _retarget(ctx, setup, ref, books)
            if setup is not None:
                candidates.append(setup)
    out, last = [], {}
    for setup in sorted(candidates, key=lambda s: (s.idx, s.s, s.side)):
        if setup.t_close_ms - last.get(setup.side, -ict.HOUR) >= ict.HOUR:
            out.append(setup)
            last[setup.side] = setup.t_close_ms
    return out


def iso_week(t_ms):
    d = datetime.fromtimestamp(t_ms / 1000, timezone.utc).isocalendar()
    return d.year, d.week


def clustered_se(values, decision_ms):
    """CR1 standard error of an equally weighted trade mean, UTC ISO weeks."""
    if len(values) != len(decision_ms):
        raise ValueError("R values and decision timestamps must have equal lengths")
    if not values:
        return None
    avg = sum(values) / len(values)
    residuals = defaultdict(float)
    for value, timestamp in zip(values, decision_ms):
        residuals[iso_week(timestamp)] += value - avg
    g = len(residuals)
    return math.sqrt(g / (g - 1) * sum(v * v for v in residuals.values())) / len(values) if g > 1 else None


def _t(value, se):
    if se is None:
        return None
    return value / se if se > 0 else (0.0 if value == 0 else math.copysign(math.inf, value))


def _summary(jobs, results, kept):
    values = [results[i]["r"] for i in kept]
    times = [jobs[i]["t_ms"] for i in kept]
    s = ict_bt.stats(values)
    s["se_iid"] = s.pop("se")
    s["se_cluster"] = clustered_se(values, times)
    s["weeks"] = len({iso_week(t) for t in times})
    s["t_cluster"] = _t(s["avg"], s["se_cluster"]) if s["n"] else None
    return s


def difference(a, b):
    se = math.hypot(a["se_cluster"], b["se_cluster"]) if a["se_cluster"] is not None and b["se_cluster"] is not None else None
    avg = a["avg"] - b["avg"]
    return dict(avg=avg, se_cluster=se, t_cluster=_t(avg, se) if a["n"] and b["n"] else None)


def shuffle_bars(bars, rng):
    """Permute close-to-close log returns, preserving every bar's OHLC ratios."""
    rows = np.asarray(bars, dtype=float)
    if rows.size == 0:
        return []
    if np.any(rows[:, 1:] <= 0):
        raise ValueError("G0 requires positive OHLC prices")
    shuffled = rows.copy()
    logs = np.log(rows[:, 4])
    new_close = np.exp(logs[0] + np.r_[0.0, np.cumsum(rng.permutation(np.diff(logs)))])
    shuffled[:, 1:5] *= (new_close / rows[:, 4])[:, None]
    shuffled[:, 4] = new_close
    return [[int(r[0]), *map(float, r[1:])] for r in shuffled]


def gap_frac(bars):
    t = [r[0] for r in bars]
    return sum((b - a) // ict.M5 - 1 for a, b in zip(t, t[1:]) if b - a > ict.M5) / max(1, len(t))


def _rows(a):
    return np.column_stack([a[k] for k in ("t_open", "o", "h", "l", "c")])


def _jobs(setups, tpkey, floor):
    jobs = []
    for setup in setups:
        j = make_job(setup.sym, setup, tpkey, floor)
        if j is not None:
            j["valid_bars"] = setup.valid_bars
            close = float(ict_bt.DATA[setup.sym]["c"][setup.idx])
            j["pull"] = abs(close - setup.ref) / close if setup.kind == "lim" else 0.0
            jobs.append(j)
    return sorted(jobs, key=lambda j: (j["sym"], j["idx"], j["side"]))


def _run(jobs, procs, label):
    results = []
    rules = ict_bt.plain_rules(horizon_h=72)
    for start in range(0, len(jobs), 256):
        batch = jobs[start:start + 256]
        if procs == 1:
            result = [ict_bt._work((j, rules, 72, .0005, .0003)) for j in batch]
        else:
            result = ict_bt.run_jobs(batch, rules, horizon_h=72, comm=.0005, slip=.0003, procs=procs)
        results.extend(result)
        print(f"{label} Backtrader {len(results)}/{len(jobs)}", flush=True)
    return results


def run_cell(setups, tpkey, floor, procs, label, out):
    jobs = _jobs(setups, tpkey, floor)
    rng = random.Random(7)
    controls = [c for c in (control_of(j, ict_bt.DATA[j["sym"]], rng) for j in jobs) if c]
    print(f"{label}: jobs={len(jobs)} controls={len(controls)} floor={floor}", flush=True)
    results = _run(jobs, procs, label + " setup")
    cresults = _run(controls, procs, label + " control")
    _, kept, blocked = ict_bt.summarize(jobs, results)
    _, ckept, cblocked = ict_bt.summarize(controls, cresults)
    groups = {}
    for name, predicate in (("all", lambda t: True), ("dev", lambda t: t < SPLIT_MS),
                             ("holdout", lambda t: t >= SPLIT_MS)):
        s = _summary(jobs, results, [i for i in kept if predicate(jobs[i]["t_ms"])])
        c = _summary(controls, cresults, [i for i in ckept if predicate(controls[i]["t_ms"])])
        groups[name] = dict(setup=s, control=c, difference=difference(s, c))
    result = dict(jobs=len(jobs), controls=len(controls), missing_controls=len(jobs) - len(controls),
                  blocked=blocked, control_blocked=cblocked,
                  outcomes=dict(Counter(r["how"] for r in results)),
                  control_outcomes=dict(Counter(r["how"] for r in cresults)), groups=groups)
    # Raw, auditable Backtrader outputs; one file per registered/reference cell.
    write_json(out / (label + ".json"), dict(jobs=jobs, controls=controls, results=results,
                                            control_results=cresults, kept=kept, control_kept=ckept))
    return result


def gate_verdicts(null, primary=None):
    s, d = null["groups"]["all"]["setup"], null["groups"]["all"]["difference"]
    g0 = bool(s["n"] and d["t_cluster"] is not None and abs(d["t_cluster"]) < 2 and s["avg"] <= 0)
    gates = {"G0": dict(pass_=g0, avg=s["avg"], n=s["n"], difference=d["avg"], t=d["t_cluster"])}
    if not g0 or primary is None:
        for name in ("G1", "G2", "G3"):
            gates[name] = dict(pass_=None, reason="G0 failed" if not g0 else "real P&L not yet run")
        return gates
    s, d = primary["groups"]["all"]["setup"], primary["groups"]["all"]["difference"]
    dev, holdout = (primary["groups"][p]["setup"] for p in ("dev", "holdout"))
    gates["G1"] = dict(pass_=bool(s["n"] >= 300 and s["avg"] > 0 and s["t_cluster"] is not None and s["t_cluster"] >= 2),
                       avg=s["avg"], n=s["n"], t=s["t_cluster"])
    gates["G2"] = dict(pass_=bool(d["avg"] > 0 and d["t_cluster"] is not None and d["t_cluster"] >= 2),
                       difference=d["avg"], t=d["t_cluster"])
    gates["G3"] = dict(pass_=bool(dev["n"] and holdout["n"] and dev["avg"] > 0 and holdout["avg"] > 0),
                       dev_avg=dev["avg"], dev_n=dev["n"], holdout_avg=holdout["avg"], holdout_n=holdout["n"])
    return gates


def _number(value):
    return "NA" if value is None else f"{value:.6f}"


def verdict_lines(gates):
    lines = []
    for name, g in gates.items():
        status = "NOT_RUN" if g["pass_"] is None else "PASS" if g["pass_"] else "FAIL"
        detail = " ".join(f"{k}={_number(v) if isinstance(v, float) else v}" for k, v in g.items() if k != "pass_")
        lines.append(f"{name} {status} | {detail}")
    conclusion = ("方法有偏，无法判定" if not gates["G0"]["pass_"] else
                  "满足接入条件：仅考虑独立模拟盘" if all(g["pass_"] for g in gates.values()) else "无可交易 edge，不接入")
    return lines + [f"DECISION | {conclusion}"]


def _json_safe(obj):
    if isinstance(obj, dict):
        return {str(k): _json_safe(v) for k, v in obj.items()}
    if isinstance(obj, (tuple, list)):
        return [_json_safe(v) for v in obj]
    if isinstance(obj, float) and not math.isfinite(obj):
        return str(obj)
    return obj


def write_json(path, value):
    path.write_text(json.dumps(_json_safe(value), ensure_ascii=False, allow_nan=False), encoding="utf-8")


def write_report(out, result):
    lines = ["# Fibonacci + FVG + BOS preregistered study", "", f"Registered cells K=8. Status: {result['status']}.",
             "Backtrader; commission=0.0005/side; slippage=0.0003; plain_rules(horizon_h=72).", "",
             "## Deviations", "", "None. Execution conventions and inherited adapter limitations are listed below.", "",
             "## Implementation conventions", "", *[f"- {s}" for s in CHOICES], "",
             "## Universe and setup counts", "", f"Included symbols: {len(result['universe'])}; dropped for gaps: {result['dropped_gaps']}; "
             f"too short: {result['too_short']}.", "", "| Variant | Setups | Long | Short |", "|---|---:|---:|---:|"]
    for name, count in result["setup_counts"].items():
        lines.append(f"| {name} | {count['total']} | {count.get('long', 0)} | {count.get('short', 0)} |")
    lines += ["", "## Results", "", "Statistics are filled trades after the existing same-symbol/direction overlap filter. "
              "SEs cluster across symbols by UTC ISO week of decision. Split: 2026-07-01 00:00 UTC.", "",
              "| Cell / period | n | avgR | Win | Payoff | PF | iid SE | Week SE | t | Control n | Control avgR | Delta | Delta SE | Delta t |",
              "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|"]
    cells = {"G0_V1_erl_floor0": result["null"], **result["cells"]}
    for name, cell in cells.items():
        for period, group in cell["groups"].items():
            s, c, d = group["setup"], group["control"], group["difference"]
            lines.append(f"| {name} / {period} | {s['n']} | {_number(s['avg'])} | {s['win']:.2%} | "
                         f"{_number(s['payoff'])} | {_number(s['pf'])} | {_number(s['se_iid'])} | {_number(s['se_cluster'])} | "
                         f"{_number(s['t_cluster'])} | {c['n']} | {_number(c['avg'])} | {_number(d['avg'])} | {_number(d['se_cluster'])} | {_number(d['t_cluster'])} |")
    if not result["cells"]:
        lines += ["", "Real-data ERL, 2R and floor=1.0 reference results were NOT_RUN because G0 failed, as preregistered."]
    lines += ["", "## Descriptive Fibonacci increments", ""]
    lines += [f"- {name}: delta={_number(d['avg'])}, week SE={_number(d['se_cluster'])}, t={_number(d['t_cluster'])}"
              for name, d in result["increments"].items()] or ["NOT_RUN (G0 failed)."]
    lines += ["", "## Mechanical gate verdicts", "", "```", *result["verdict_lines"], "```", "",
              "## Limitations", "", "5m OHLC execution; survivor-selected universe; about 12 months, primarily bearish. "
              "No parameter tuning. Raw jobs, results, kept indices and input hashes accompany results.json.", ""]
    (out / "report.md").write_text("\n".join(lines), encoding="utf-8")
    write_json(out / "results.json", result)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--cache", default=DEFAULT_CACHE, type=Path)
    ap.add_argument("--factor-root", default=DEFAULT_FACTOR_ROOT, type=Path)
    ap.add_argument("--procs", default=min(9, os.cpu_count() or 1), type=int)
    a = ap.parse_args(argv)
    if a.procs < 1:
        ap.error("--procs must be positive")
    if not a.factor_root.is_dir() or not (a.cache / "ict5m").is_dir():
        ap.error("factor-root and cache/ict5m must exist (offline only)")
    if a.out.resolve().is_relative_to(a.cache.resolve()) or a.out.resolve().is_relative_to(a.factor_root.resolve()):
        ap.error("output must not be under an input directory")
    a.out.mkdir(parents=True, exist_ok=True)
    started = time.time()
    syms = sorted({p.stem for p in a.factor_root.glob("*.json")} & {p.stem for p in (a.cache / "ict5m").glob("*.json")})
    dropped, short, included, hashes = [], [], [], {}
    for sym in syms:
        bars = ict_fetch.load(a.cache, sym)
        if gap_frac(bars) > .01:
            dropped.append(sym)
        elif len(bars) < 3000:
            short.append(sym)
        else:
            included.append(sym)
    if not included:
        raise RuntimeError("empty eligible universe")
    for sym in sorted(set(included) | {"BTCUSDT", "ETHUSDT"}):
        path = Path(ict_fetch.path(a.cache, sym))
        hashes[str(path)] = hashlib.sha256(path.read_bytes()).hexdigest()
        bars = ict_fetch.load(a.cache, sym)
        if not bars or bars[0][0] < START_MS or bars[-1][0] >= END_MS:
            raise RuntimeError(f"{sym}: cache falls outside registered dates or is missing; stop rather than change input range")
    for path in (PREREG, Path(__file__), REPO / "scripts/backtest/ict.py", REPO / "scripts/backtest/ict_bt.py",
                 REPO / "scripts/backtest/ict_study.py", REPO / "scripts/backtest/backtrader_adapter.py"):
        hashes[str(path)] = hashlib.sha256(path.read_bytes()).hexdigest()
    print(f"universe={len(included)} dropped(gaps>1%)={dropped} too_short={short}; procs={a.procs}", flush=True)
    setups = {v: [] for v in VARIANTS}
    # Load/detect one symbol at a time, retaining only compact arrays for execution.
    for sym in included:
        ctx = load_universe(a.cache, [sym])[sym]
        own = {"V1": find_fib_setups(ctx), "V2": find_fib_setups(ctx, fib=False)}
        own["V4"] = [s for s in ict.find_setups(ctx) if s.trigger == "full"]
        own["V3"] = _fib_full(ctx, own["V4"])
        for variant, found in own.items():
            for setup in found:
                setup.sym = sym
            setups[variant].extend(found)
        print(f"detect {sym}: " + " ".join(f"{v}={len(own[v])}" for v in VARIANTS), flush=True)
    del ctx, own
    counts = {v: dict(total=len(ss), **Counter(s.side for s in ss)) for v, ss in setups.items()}
    # Generate all shuffled symbols first, so BTC/ETH references use the same paths.
    rng = np.random.default_rng(20261101)
    shuffled = {}
    for sym in sorted(set(included) | {"BTCUSDT", "ETHUSDT"}):
        bars = _rows(ict_bt.DATA[sym]) if sym in ict_bt.DATA else ict_fetch.load(a.cache, sym)
        shuffled[sym] = ict.load_arrays(shuffle_bars(bars, rng))
    ict_bt.DATA.clear()
    ict_bt.DATA.update({sym: shuffled[sym] for sym in included})
    null_setups = []
    for sym in included:
        ref = "ETHUSDT" if sym == "BTCUSDT" else "BTCUSDT"
        ctx = ict.Ctx(_rows(shuffled[sym]), _rows(shuffled[ref]))
        found = find_fib_setups(ctx)
        for setup in found:
            setup.sym = sym
        null_setups.extend(found)
        print(f"G0 detect {sym}: V1={len(found)}", flush=True)
    del ctx, shuffled
    counts["G0_V1"] = dict(total=len(null_setups), **Counter(s.side for s in null_setups))
    null = run_cell(null_setups, "erl", 0.0, a.procs, "G0_V1_erl_floor0", a.out)
    gates = gate_verdicts(null)
    cells, increments = {}, {}
    if gates["G0"]["pass_"]:
        print("G0 PASS; running the eight registered cells and floor=1.0 reference columns", flush=True)
        for sym in included:
            ict_bt.DATA[sym] = ict.load_arrays(ict_fetch.load(a.cache, sym))
        for floor in (0.0, 1.0):
            for variant in VARIANTS:
                for exit_ in ("erl", "2r"):
                    name = f"{variant}_{exit_}_floor{floor:g}"
                    cells[name] = run_cell(setups[variant], exit_, floor, a.procs, name, a.out)
        gates = gate_verdicts(null, cells["V1_erl_floor0"])
        for exit_ in ("erl", "2r"):
            for v1, v2 in (("V1", "V2"), ("V3", "V4")):
                increments[f"{v1}-{v2}/{exit_}"] = difference(
                    cells[f"{v1}_{exit_}_floor0"]["groups"]["all"]["setup"],
                    cells[f"{v2}_{exit_}_floor0"]["groups"]["all"]["setup"])
    result = dict(status="complete" if gates["G0"]["pass_"] else "stopped_at_G0", K=8,
                  cache=str(a.cache), factor_root=str(a.factor_root), universe=included,
                  dropped_gaps=dropped, too_short=short, setup_counts=counts, input_sha256=hashes,
                  seeds=dict(controls=7, null=20261101), conventions=CHOICES, deviations=[],
                  null=null, cells=cells, increments=increments, gates=gates,
                  verdict_lines=verdict_lines(gates), elapsed_seconds=time.time() - started)
    write_report(a.out, result)
    print("setup_counts " + json.dumps(counts, sort_keys=True), flush=True)
    for line in result["verdict_lines"]:
        print(line, flush=True)
    print(f"report={a.out / 'report.md'} results={a.out / 'results.json'} elapsed={result['elapsed_seconds']:.1f}s", flush=True)


if __name__ == "__main__":
    main()
