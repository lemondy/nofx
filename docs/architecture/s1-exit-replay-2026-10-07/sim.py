"""Exit-rule replay for S1 (2026-10-07 review).

Mirrors trader/auto_trader_vol.go + auto_trader_risk.go exit semantics:
  - exchange SL / structural TP legs: intrabar on 1m high/low, stop first when
    both trade inside one bar (conservative), gap fills at the open
  - drawdown monitor every 1m on the close (mark proxy): TP ladder trim 1/3 at
    tp_trim_at_r (breakeven secured first), R-mode drawdown protect
  - decision cycle every 5m: breakeven arm, 1R lock (BE stop only, because
    tp_trim_yields_to_lock=false), 2xATR(1h) trailing armed at 1.5R (trend),
    TP-runner conversion
AI discretionary closes are not modelled. Horizon 72h, then close at market.
"""
import csv, json, math, os, random, statistics as st

S = os.path.dirname(os.path.abspath(__file__))
COST_BPS = 12.0  # round trip: 2x taker 5bps + 2bps slippage
HORIZON_MIN = 72 * 60


def load():
    rows = list(csv.DictReader(open(f"{S}/trades.csv")))
    out = []
    for r in rows:
        k = json.load(open(f"{S}/k/{r['id']}.json"))
        out.append((r, k))
    return out


def hourly_atr_fn(bars):
    """ATR(14, Wilder) of CLOSED 1h bars, as a function of time."""
    hours = {}
    for t, o, h, l, c in bars:
        hk = t // 3_600_000
        if hk not in hours:
            hours[hk] = [o, h, l, c]
        else:
            x = hours[hk]
            x[1] = max(x[1], h); x[2] = min(x[2], l); x[3] = c
    keys = sorted(hours)
    atr_at = {}
    atr, prev_c = None, None
    trs = []
    for hk in keys:
        o, h, l, c = hours[hk]
        tr = h - l if prev_c is None else max(h - l, abs(h - prev_c), abs(l - prev_c))
        trs.append(tr)
        if len(trs) == 14:
            atr = sum(trs) / 14
        elif len(trs) > 14:
            atr = (atr * 13 + tr) / 14
        prev_c = c
        atr_at[hk + 1] = atr  # usable once the hour closed
    def f(t):
        hk = t // 3_600_000
        while hk not in atr_at and hk > keys[0]:
            hk -= 1
        return atr_at.get(hk)
    return f


def simulate(r, bars, p):
    side = 1 if r["side"] == "LONG" else -1
    entry = float(r["entry_price"]); sl0 = float(r["sl"]); tp = float(r["tp"])
    et = int(r["entry_time"])
    risk = abs(entry - sl0)
    mode = r["exit_mode"] or "trend"
    if p.get("force_mode"):
        mode = p["force_mode"]
    atr = hourly_atr_fn(bars)
    R = lambda px: (px - entry) * side / risk
    be_price = entry + side * risk * p["be_offset_r"]

    qty = 1.0
    realized = 0.0
    stop = sl0
    tp_frac = 1.0 if (mode in ("range", "quick") or not p["trail"]) else p["tp_frac"]
    tp_live = tp > 0 and p["use_tp"] and ((tp - entry) * side > 0)
    tp_qty = tp_frac  # fraction of the ORIGINAL size resting at TP
    trim_done = False
    peak = None
    reason = "horizon"
    path = [b for b in bars if b[0] + 60_000 > et]  # bars from the entry bar on
    if not path:
        return None
    for i, (t, o, h, l, c) in enumerate(path):
        if t - et > HORIZON_MIN * 60_000:
            break
        if i > 0:  # entry bar: fill time inside the bar unknown -> no intrabar exits
            # 1) exchange legs, stop first
            hit_stop = (l <= stop) if side == 1 else (h >= stop)
            if hit_stop:
                fill = min(stop, o) if side == 1 else max(stop, o)
                realized += qty * R(fill); qty = 0
                reason = "stop" if stop == sl0 else ("be_stop" if abs(stop - be_price) < 1e-12 else "trail_stop")
                break
            if tp_live and tp_qty > 0:
                hit_tp = (h >= tp) if side == 1 else (l <= tp)
                if hit_tp:
                    q = min(qty, tp_qty)
                    realized += q * R(tp); qty -= q; tp_qty = 0
                    if qty <= 1e-9:
                        reason = "tp"; break
        # 2) 1-minute monitor at the close
        rc = R(c)
        prev_peak = rc if peak is None else peak
        peak = rc if peak is None else max(peak, rc)
        acted = False
        if p["trim_r"] > 0 and not trim_done and rc >= p["trim_r"]:
            if (stop - be_price) * side < 0:
                stop = be_price
            q = qty / 3
            realized += q * rc; qty -= q; trim_done = True; acted = True
        if not acted and p["dd_arm"] > 0 and prev_peak >= p["dd_arm"] and prev_peak > 0:
            if (prev_peak - rc) / prev_peak >= p["dd_gb"]:
                realized += qty * rc; qty = 0; reason = "dd_protect"; break
        if p.get("tp_full_r", 0) > 0 and rc >= p["tp_full_r"]:
            realized += qty * rc; qty = 0; reason = "tp_full"; break
        # 3) decision cycle every 5 minutes (wall clock)
        if (t // 60_000) % 5 == 0:
            if p["be_arm"] > 0 and rc >= p["be_arm"] and (stop - be_price) * side < 0:
                stop = be_price
            if p["lock_r"] > 0 and rc >= p["lock_r"] and (stop - be_price) * side < 0:
                stop = be_price
            if p["trail"] and mode == "trend" and rc >= p["trail_arm"]:
                a = atr(t)
                if a:
                    cand = c - side * p["trail_mult"] * a
                    if (side == 1 and cand > stop * 1.001) or (side == -1 and cand < stop * 0.999):
                        stop = cand
                if tp_live and tp_qty > 0 and (c - tp) * side >= 0:
                    tp_qty = 0  # runner conversion
        last_c = c
    if qty > 0:
        realized += qty * R(path[min(i, len(path) - 1)][4])
    cost_r = (COST_BPS / 10_000) * entry / risk
    return realized - cost_r, reason


CURRENT = dict(be_arm=0.5, be_offset_r=0.2, lock_r=1.0, trim_r=1.2, dd_arm=1.0, dd_gb=0.5,
               trail=True, trail_arm=1.5, trail_mult=2.0, tp_frac=0.5, use_tp=True)

VARIANTS = {
    "A 现行": CURRENT,
    "B 关闭0.5R保本(1R仍锁保本)": {**CURRENT, "be_arm": 0},
    "C B+回撤保护1.5R/60%": {**CURRENT, "be_arm": 0, "dd_arm": 1.5, "dd_gb": 0.6},
    "D 只留SL/TP+1R锁+移动止损": {**CURRENT, "be_arm": 0, "dd_arm": 0, "trim_r": 0},
    "E 纯SL/TP(无管理)": {**CURRENT, "be_arm": 0, "lock_r": 0, "dd_arm": 0, "trim_r": 0, "trail": False},
    "F 保本1R/减仓1.5R/回撤2R": {**CURRENT, "be_arm": 1.0, "trim_r": 1.5, "dd_arm": 2.0, "dd_gb": 0.5},
    "G 现行但保本0.8R": {**CURRENT, "be_arm": 0.8},
}


def maxdd(xs):
    peak = cum = dd = 0
    for x in xs:
        cum += x; peak = max(peak, cum); dd = min(dd, cum - peak)
    return dd


def boot_ci(diffs, n=5000, seed=7):
    rnd = random.Random(seed); m = len(diffs)
    means = sorted(sum(diffs[rnd.randrange(m)] for _ in range(m)) / m for _ in range(n))
    return means[int(0.025 * n)], means[int(0.975 * n)]


if __name__ == "__main__":
    data = load()
    res = {}
    for name, p in VARIANTS.items():
        res[name] = [simulate(r, k, p) for r, k in data]
    json.dump({k: v for k, v in res.items()}, open(f"{S}/results.json", "w"))
    print("trades", len(data))
