"""OHLCV features known at each bar's close (see DISCREPANCIES.md).

``compute_features(frame, BrooksParams())`` preserves the time index and never
reads future rows. Geometry, rolling statistics and short-window channels are
vectorized. Only path-dependent counters, confirmations and state machines make
one forward pass. A pivot is emitted on its *confirmation* bar, never backfilled.
All heuristic windows/thresholds live in BrooksParams; score weights alone are
fixed by extracted/08 §5.1. This module performs no I/O or trading.
"""

from dataclasses import dataclass, fields

import numpy as np
import pandas as pd


@dataclass(frozen=True)
class BrooksParams:
    """Book constants plus [inferred] cross-market parameters, all in one place.

    Price tolerances use current, completed ATR, never ticks. ATR is Wilder's
    smoothing seeded by the first period's mean TR. EMA starts at first close
    but is exposed only after ema_period bars. See the audit for interpretations.
    """

    atr_period: int = 14
    ema_period: int = 20
    doji_range_ratio: float = 0.25       # [inferred] small body, relative to bar
    doji_atr_ratio: float = 0.10         # [inferred] replaces absolute 2 ticks
    strong_body_lookback: int = 10       # [inferred] within book's 5--10
    strong_body_ratio: float = 0.60      # [inferred] distinguishes strong entry
    strong_tail_ratio: float = 0.25     # [inferred] each tail, for strong bars
    swing_left: int = 2                 # [inferred] confirmation geometry
    swing_right: int = 2
    swing_sequence: int = 3             # [inferred] highs AND lows required
    breakout_lookback: int = 5          # [inferred] resistance/support proxy
    follow_through_bars: int = 2        # [inferred] subsequent trend bars
    always_in_reversal_bars: int = 5
    always_in_other_side_bars: int = 2
    always_in_max_pullback_atr: float = 3.0  # [inferred] replaces daily ADR
    two_leg_min_bars: int = 10
    two_leg_min_legs: int = 2
    reversal_open_near_atr: float = 0.10     # [inferred] F1 "near"
    reversal_tail_min: float = 1 / 3
    reversal_tail_max: float = 1 / 2
    reversal_opposite_tail_max: float = 0.10  # [inferred] F2 "almost zero"
    reversal_overlap_max: float = 0.50       # [inferred] F3 additional filter
    reversal_close_lookback: int = 8          # [inferred] illustrative horizons
    reversal_extreme_lookback: int = 5
    climax_min_bars: int = 3
    stop_body_ratio: float = 0.50            # [inferred] small directional body
    stop_tail_ratio: float = 1 / 3           # [inferred] long stopping tail
    exhaustion_min_bars: int = 30
    exhaustion_range_atr: float = 2.0        # [inferred]
    ma_gap_bars: int = 20
    micro_min_bars: int = 2
    micro_max_bars: int = 10
    micro_max_pullback_atr: float = 0.15     # [inferred] replaces 1--3 ticks
    micro_max_pullback_bars: int = 2          # [inferred] "one or two"
    micro_touch_atr: float = 0.10            # [inferred] closeness to support
    micro_touch_fraction: float = 0.60       # [inferred] "most bars"
    micro_width_atr: float = 2.0             # [inferred] narrow channel
    micro_climax_bars: int = 10
    flag_min_trend_bars: int = 10
    flag_window: int = 3                     # [inferred] recent consolidation
    flag_width_atr: float = 1.5               # [inferred]
    flag_drift_atr: float = 0.50              # [inferred] roughly horizontal
    flag_overlap_min: float = 0.50           # [inferred]
    flag_failure_bars: int = 3               # [inferred] "one to several"
    strength_window: int = 20                # [inferred] common trailing horizon
    strength_trend_fraction: float = 0.60    # [inferred] rule 3
    strength_overlap_fraction: float = 0.50  # [inferred] rule 4 "many"
    strength_overlap_tolerance_atr: float = 0.05  # [inferred] replaces 1 tick
    strength_tail_ratio: float = 0.20        # [inferred] rule 5
    strength_extreme_range_atr: float = 2.0  # [inferred] rule 10
    strength_pullback_atr: float = 1.0       # [inferred] replaces 0.3 daily ADR
    strength_min_leg_bars: int = 5
    strength_setup_body_ratio: float = 0.60 # [inferred] rule 19
    strength_repeated_setups: int = 2        # [inferred] rule 21 "repeated"

    def __post_init__(self):
        for f in fields(self):
            value = getattr(self, f.name)
            if not np.isfinite(value):
                raise ValueError(f"{f.name} must be finite")
            if f.type is int and (isinstance(value, bool) or not isinstance(value, (int, np.integer)) or value < 1):
                raise ValueError(f"{f.name} must be a positive integer")
            if f.type is float and value < 0:
                raise ValueError(f"{f.name} must be nonnegative")
        for name in ("doji_range_ratio", "strong_body_ratio", "strong_tail_ratio",
                     "reversal_tail_min", "reversal_tail_max", "reversal_opposite_tail_max",
                     "reversal_overlap_max", "stop_body_ratio", "stop_tail_ratio",
                     "micro_touch_fraction", "flag_overlap_min", "strength_trend_fraction",
                     "strength_overlap_fraction", "strength_tail_ratio", "strength_setup_body_ratio"):
            if getattr(self, name) > 1:
                raise ValueError(f"{name} must be in [0, 1]")
        if self.reversal_tail_min > self.reversal_tail_max:
            raise ValueError("reversal tail interval is reversed")
        if self.micro_min_bars < 2 or self.micro_min_bars > self.micro_max_bars:
            raise ValueError("micro window must contain at least two bars")
        if self.flag_window < 2 or self.swing_sequence < 2:
            raise ValueError("flag_window and swing_sequence must be at least two")

    @property
    def warmup_bars(self):
        """All numeric rolling columns are finite from this many rows onward."""
        return max(self.atr_period, self.ema_period, self.strong_body_lookback + 1,
                   self.strength_window, self.breakout_lookback + 1,
                   self.always_in_reversal_bars + 1,
                   self.reversal_close_lookback + 1, self.reversal_extreme_lookback + 1,
                   self.flag_min_trend_bars + self.flag_window,
                   self.swing_left + self.swing_right + 1, self.micro_max_bars)


# Original numbered rules, original integer weights. No duplicated or invented
# points. Rules absent here are explicitly excluded in DISCREPANCIES.md.
STRENGTH_RULES = {
    2: ("trending_swings", 2), 3: ("trend_bars", 2),
    4: ("little_overlap", 2), 5: ("short_tails", 1),
    6: ("body_gaps", 2), 8: ("measuring_gap", 2),
    9: ("micro_gap", 2), 10: ("no_big_climax", 1),
    11: ("largest_counter_bar", 2), 15: ("ema_gap", 2),
    17: ("small_pullback", 2), 18: ("unfilled_close_limit", 1),
    19: ("strong_pullback_setup", 2), 21: ("repeated_two_legs", 1),
    22: ("no_two_other_side", 2), 24: ("failed_counter_spike", 2),
}


def _ratio(numerator, denominator, zero=0.0):
    """A zero range has defined geometry; an unavailable ATR stays NaN."""
    return numerator.div(denominator.where(denominator != 0)).mask(denominator == 0, zero)


def _run(condition):
    b = condition.fillna(False).astype(bool)
    return b.astype("int64").groupby((~b).cumsum()).cumsum()


def _previous(condition):
    return condition.shift(1, fill_value=False)


def _atr(tr, period):
    seed = tr.rolling(period, min_periods=period).mean()
    values = tr.copy()
    values.iloc[:period - 1] = np.nan
    if len(tr) >= period:
        values.iloc[period - 1] = seed.iloc[period - 1]
    return values.ewm(alpha=1 / period, adjust=False, min_periods=1).mean()


def _structure(h, l, p):
    """Pivots at t-right become available at t; equal plateaus use last point."""
    width = p.swing_left + p.swing_right + 1
    ph, pl = h.shift(p.swing_right), l.shift(p.swing_right)
    # Strict right side tie-break chooses the last point of a flat plateau.
    high_event = (ph == h.rolling(width).max()) & (ph > h.rolling(p.swing_right).max())
    low_event = (pl == l.rolling(width).min()) & (pl < l.rolling(p.swing_right).min())
    out = {"swing_high_confirmed": high_event, "swing_low_confirmed": low_event}
    for kind, price, event in (("high", ph, high_event), ("low", pl, low_event)):
        confirmed = price.where(event)
        out[f"has_swing_{kind}"] = confirmed.notna().cummax()
        out[f"last_swing_{kind}"] = confirmed.ffill().fillna(0.0)
        # Work in confirmation-event time, then carry known state forward.
        change = price.loc[event].diff()
        rising, falling = change > 0, change < 0
        states = {
            f"{kind}s_rising": _run(rising) >= p.swing_sequence - 1,
            f"{kind}s_falling": _run(falling) >= p.swing_sequence - 1,
            f"last_{kind}_rising": rising,
            f"last_{kind}_falling": falling,
        }
        for name, value in states.items():
            out[name] = value.astype(float).reindex(h.index).ffill().fillna(0).astype(bool)
    return out


def _entry_counts(extreme, pivot_event, pivot_price):
    """H counts; negating lows gives the exact L mirror.

    A lower high arms the next attempt. Consecutive higher highs cannot count
    again. A recovered pullback origin (or a newly confirmed higher swing high)
    starts a fresh leg. Lower bounce pivots inside an ABC do not reset it. The
    breakout bar can both emit Hn and reset the stored count for the next bar.
    """
    n, armed, anchor, previous_pivot = 0, False, None, None
    counts, entries, resets = [], [], []
    x = extreme.to_numpy()
    for i, price in enumerate(x):
        entry, reset = 0, False
        if anchor is None:
            anchor = price
        if pivot_event.iloc[i]:
            pivot = pivot_price.iloc[i]
            if previous_pivot is not None and pivot > previous_pivot and pivot != anchor:
                n, armed, anchor, reset = 0, False, max(price, pivot), True
            previous_pivot = pivot
        if i:
            if price < x[i - 1]:
                armed = True
            elif price > x[i - 1] and armed:
                n += 1
                entry, armed = n, False
        if price > anchor:
            n, armed, anchor, reset = 0, False, price, True
        counts.append(n)
        entries.append(entry)
        resets.append(reset)
    return counts, entries, resets


def _measuring_gap(breakout, pullback, near, far, bull=True):
    """Confirm on a later pullback that stays beyond the breakout bar's edge."""
    ref = None
    result = []
    for start, test, a, b in zip(breakout, pullback, near, far):
        event = False
        if ref is not None:
            intact = a > ref if bull else a < ref
            if not intact:
                ref = None
            elif test:
                event, ref = True, None
        if start and ref is None and not event:
            ref = b
        result.append(event)
    return pd.Series(result, index=breakout.index, dtype=bool)


def _second_entry_failure(entry, reset, stop, extreme, bull):
    """First later violation of H2/L2's t-1 signal extreme, within its leg."""
    level, events = None, []
    for i in range(len(entry)):
        fail = level is not None and (extreme.iloc[i] < level if bull else extreme.iloc[i] > level)
        if fail or reset.iloc[i]:
            level = None
        if entry.iloc[i] == 2:
            level = stop.iloc[i - 1]
        events.append(fail)
    return pd.Series(events, index=entry.index, dtype=bool)


def _micro_channels(h, l, bull_bar, bear_bar, atr, p):
    """Enumerate short endpoint lines in parallel over all timestamps.

    Support joins endpoint lows (bull) / highs (bear); parallel resistance is
    the maximum distance to the opposite extreme. Also require most touches,
    narrow width, only a few tiny pullbacks, and no consecutive counter bars.
    These are declared geometric proxies, not a subjective optimal line fit.
    """
    out = {}
    for side, near, far, counter in (("bull", l, h, bear_bar), ("bear", -h, -l, bull_bar)):
        length = pd.Series(0, index=h.index, dtype="int64")
        width_out = pd.Series(0.0, index=h.index)
        for window in range(p.micro_min_bars, p.micro_max_bars + 1):
            start = near.shift(window - 1)
            slope = (near - start) / (window - 1)
            legal = slope > 0
            touches = pd.Series(0, index=h.index)
            width = pd.Series(0.0, index=h.index)
            pullbacks = pd.Series(0, index=h.index)
            for lag in range(window):
                line = start + slope * (window - 1 - lag)
                distance = near.shift(lag) - line
                legal &= distance >= -p.micro_touch_atr * atr
                touches += (distance.abs() <= p.micro_touch_atr * atr).astype(int)
                width = np.maximum(width, far.shift(lag) - line)
                if lag < window - 1:
                    retrace = near.shift(lag + 1) - near.shift(lag)
                    pullbacks += (retrace > 0).astype(int)
                    legal &= retrace <= p.micro_max_pullback_atr * atr
                    legal &= ~(counter.shift(lag, fill_value=False) &
                               counter.shift(lag + 1, fill_value=False))
            legal &= ((touches >= window * p.micro_touch_fraction) &
                      (width <= p.micro_width_atr * atr) &
                      (pullbacks <= p.micro_max_pullback_bars))
            length = length.mask(legal, window)
            width_out = width_out.mask(legal, _ratio(width, atr))
        out[f"micro_channel_{side}_length"] = length
        out[f"micro_channel_{side}"] = length >= p.micro_min_bars
        out[f"micro_channel_{side}_width_atr"] = width_out
        monotone = _run(near >= near.shift(1)) + 1
        out[f"micro_{side}_no_pullback_length"] = monotone
        out[f"micro_{side}_climactic"] = (out[f"micro_channel_{side}"] & (monotone >= p.micro_climax_bars) &
            (near > near.shift(p.micro_climax_bars - 1)))
    return out


def _always_in(f, h, l, c, p):
    """Three-state, close-only approximation; confirmations precede degradation."""
    state, age, peak, trough, pb_age, pb_legs, counter_before = 0, 0, None, None, 0, 0, False
    result = {k: [] for k in ("always_in", "always_in_age", "always_in_changed",
                              "always_in_pullback_bars", "always_in_pullback_legs")}
    columns = ("always_in_bull_confirm", "always_in_bear_confirm", "bull_other_side_run",
               "bear_other_side_run", "last_low_rising", "last_high_falling",
               "has_swing_low", "has_swing_high", "last_swing_low", "last_swing_high",
               "highs_falling", "lows_falling", "highs_rising", "lows_rising", "atr",
               "h2_failed", "l2_failed", "strong_bear", "strong_bull", "ema20")
    arrays = {k: f[k].to_numpy() for k in columns}
    hv, lv, cv = h.to_numpy(), l.to_numpy(), c.to_numpy()
    for i in range(len(c)):
        old = state
        bc, sc = arrays["always_in_bull_confirm"][i], arrays["always_in_bear_confirm"][i]
        confirmation = bc != sc and (bc or sc)
        if confirmation:
            state = 1 if bc else -1
        if state and state == old:
            new_extreme = hv[i] > peak if state > 0 else lv[i] < trough
            if new_extreme:
                pb_age, pb_legs, counter_before = 0, 0, False
            else:
                pb_age += 1
                counter = i > 0 and state * (cv[i] - cv[i - 1]) < 0
                if counter and not counter_before:
                    pb_legs += 1
                counter_before = counter
            peak, trough = max(peak, hv[i]), min(trough, lv[i])
            depth = peak - lv[i] if state > 0 else hv[i] - trough
            opposite = arrays["bull_other_side_run" if state > 0 else "bear_other_side_run"][i]
            structure_fail = ((arrays["highs_falling"][i] and arrays["lows_falling"][i]) if state > 0
                              else (arrays["highs_rising"][i] and arrays["lows_rising"][i]))
            support_fail = ((arrays["has_swing_low"][i] and arrays["last_low_rising"][i] and
                             lv[i] < arrays["last_swing_low"][i]) if state > 0 else
                            (arrays["has_swing_high"][i] and arrays["last_high_falling"][i] and
                             hv[i] > arrays["last_swing_high"][i]))
            second_failed = ((arrays["h2_failed"][i] and arrays["strong_bear"][i] and cv[i] < arrays["ema20"][i]) if state > 0
                             else (arrays["l2_failed"][i] and arrays["strong_bull"][i] and cv[i] > arrays["ema20"][i]))
            if not confirmation and (opposite >= p.always_in_other_side_bars or support_fail or structure_fail or
                second_failed or
                (pb_age >= p.two_leg_min_bars and pb_legs >= p.two_leg_min_legs) or
                depth > p.always_in_max_pullback_atr * arrays["atr"][i]):
                state = 0
        if state != old:
            age, pb_age, pb_legs, counter_before = 0, 0, 0, False
            peak, trough = hv[i], lv[i]
        elif state:
            age += 1
        else:
            age = 0
        for name, value in zip(result, (state, age, state != old, pb_age, pb_legs)):
            result[name].append(value)
    return result


def _flag_failures(candidate, upper, lower, close, p, bull):
    pending, born = None, None
    events, failures = [], []
    for i in range(len(close)):
        fail = False
        if pending is not None:
            if i - born > p.flag_failure_bars:
                pending = None
            elif (close.iloc[i] < pending if bull else close.iloc[i] > pending):
                fail, pending = True, None
        boundary = (upper.iloc[i - 1] if bull else lower.iloc[i - 1]) if i > 0 else None
        event = i > 0 and candidate.iloc[i - 1] and (
            close.iloc[i] > boundary if bull else close.iloc[i] < boundary)
        if event:
            pending, born = boundary, i
        events.append(event)
        failures.append(fail)
    return events, failures


def compute_features(frame: pd.DataFrame, params: BrooksParams | None = None) -> pd.DataFrame:
    """Return numeric/bool features for completed bars, with no input mutation.

    Requires a unique, increasing DatetimeIndex, finite valid OHLCV and
    nonnegative volume. Irregular spacing/timezones are accepted, with no
    resampling or session assumptions. Zero-range bars have close_pos=0.5 and
    other ratios zero. Missing-history events are False/0, accompanied by
    warmup_ready/has_swing_*; only rolling numeric columns have warmup NaNs.

    Hn/Ln mark the *trigger* bar; their signal bar is t-1. reversal_*_f4 confirms
    the PREVIOUS bar at the current entry bar's close. No value is backdated.
    Strength columns use original rule numbers; bull/bear scores are independent
    and normalized by the supported maximum (28), NOT the original 24-rule sum.
    """
    p = params or BrooksParams()
    required = ["open", "high", "low", "close", "volume"]
    if not isinstance(frame, pd.DataFrame) or not set(required).issubset(frame.columns):
        raise ValueError("frame requires open/high/low/close/volume columns")
    if not isinstance(frame.index, pd.DatetimeIndex) or frame.index.hasnans or not frame.index.is_unique or not frame.index.is_monotonic_increasing:
        raise ValueError("index must be a unique increasing DatetimeIndex without NaT")
    if not frame.columns.is_unique:
        raise ValueError("columns must be unique")
    try:
        x = frame[required].astype(float)
    except (ValueError, TypeError) as exc:
        raise ValueError("OHLCV must be numeric") from exc
    if not np.isfinite(x.to_numpy()).all():
        raise ValueError("OHLCV must be finite; fill missing bars explicitly")
    o, h, l, c, v = (x[k] for k in required)
    if ((h < np.maximum(o, c)) | (l > np.minimum(o, c)) | (h < l) | (v < 0)).any():
        raise ValueError("invalid OHLC geometry or negative volume")

    f = {}
    rng, body = h - l, (c - o).abs()
    upper, lower = h - np.maximum(o, c), np.minimum(o, c) - l
    tr = pd.concat([rng, (h - c.shift(1)).abs(), (l - c.shift(1)).abs()], axis=1).max(axis=1)
    atr = _atr(tr, p.atr_period)
    ema = c.ewm(span=p.ema_period, adjust=False, min_periods=p.ema_period).mean()
    f.update(body=body, range=rng, upper_tail=upper, lower_tail=lower,
             body_ratio=_ratio(body, rng), upper_tail_ratio=_ratio(upper, rng),
             lower_tail_ratio=_ratio(lower, rng), close_pos=_ratio(c - l, rng, zero=0.5),
             true_range=tr, atr=atr, ema20=ema,
             mean_prior_body=body.shift(1).rolling(p.strong_body_lookback).mean(),
             warmup_ready=pd.Series(np.arange(len(x)) >= p.warmup_bars - 1, index=x.index))
    for name, value in (("body", body), ("range", rng), ("upper_tail", upper), ("lower_tail", lower), ("close_ema", c - ema)):
        f[name + "_atr"] = _ratio(value, atr)
    threshold = np.maximum(p.doji_range_ratio * rng, p.doji_atr_ratio * atr.fillna(0))
    doji = body <= threshold
    bull, bear = ~doji & (c > o), ~doji & (c < o)
    strong = ((body >= f["mean_prior_body"]) & (f["body_ratio"] >= p.strong_body_ratio) &
              (f["upper_tail_ratio"] <= p.strong_tail_ratio) & (f["lower_tail_ratio"] <= p.strong_tail_ratio))
    sb, ss = strong & bull, strong & bear
    inside, outside = (h <= h.shift(1)) & (l >= l.shift(1)), (h > h.shift(1)) & (l < l.shift(1))
    f.update(is_doji=doji, is_trend=~doji, trend_bull=bull, trend_bear=bear,
             strong_bull=sb, strong_bear=ss, inside=inside, outside=outside,
             ii=inside & _previous(inside), iii=inside & _previous(inside) & inside.shift(2, fill_value=False),
             ioi=inside & _previous(outside) & inside.shift(2, fill_value=False),
             bar_pullback_bull=l < l.shift(1), bar_pullback_bear=h > h.shift(1))
    overlap = (np.minimum(h, h.shift(1)) - np.maximum(l, l.shift(1))).clip(lower=0).fillna(0)
    f["overlap_ratio"] = _ratio(overlap, np.minimum(rng, rng.shift(1))).fillna(0)
    f["body_overlap_ratio"] = _ratio((np.minimum(np.maximum(o, c), np.maximum(o, c).shift(1)) -
        np.maximum(np.minimum(o, c), np.minimum(o, c).shift(1))).clip(lower=0),
        np.minimum(body, body.shift(1))).fillna(0)
    f.update(_structure(h, l, p))
    for side, extreme, pivot_price in (("h", h, h.shift(p.swing_right)), ("l", -l, -l.shift(p.swing_right))):
        pivot_event = f["swing_high_confirmed" if side == "h" else "swing_low_confirmed"]
        count, entry, reset = _entry_counts(extreme, pivot_event, pivot_price)
        f[side + "_count"] = pd.Series(count, index=x.index, dtype="int64")
        f[side + "_entry"] = pd.Series(entry, index=x.index, dtype="int64")
        f[side + "_reset"] = pd.Series(reset, index=x.index, dtype=bool)
        for n in range(1, 5):
            f[f"{side}{n}"] = f[side + "_entry"] == n
    f["h2_failed"] = _second_entry_failure(f["h_entry"], f["h_reset"], l, l, True)
    f["l2_failed"] = _second_entry_failure(f["l_entry"], f["l_reset"], h, h, False)

    for side, sign, trend, strong_bar, tail, opposite_tail in (
        ("bull", 1, bull, sb, lower, upper), ("bear", -1, bear, ss, upper, lower)):
        pre = "reversal_" + side + "_"
        min_ok = (sign * (c - o) > 0) | (sign * (c - (h + l) / 2) > 0)
        excessive = ((h + l) / 2 > l.shift(1)) if sign > 0 else ((h + l) / 2 < h.shift(1))
        f[pre + "minimum"] = min_ok
        f[pre + "tail_ratio"] = _ratio(tail, rng)
        f[pre + "opposite_tail_ratio"] = _ratio(opposite_tail, rng)
        f[pre + "close_strength"] = f["close_pos"] if sign > 0 else 1 - f["close_pos"]
        f[pre + "excessive_overlap"] = excessive
        f[pre + "f1"] = ((sign * (o - c.shift(1)) <= p.reversal_open_near_atr * atr) &
                           (sign * (c - c.shift(1)) > 0) & (sign * (c - o) > 0))
        f[pre + "f2"] = ((f[pre + "tail_ratio"] >= p.reversal_tail_min) &
            (f[pre + "tail_ratio"] <= p.reversal_tail_max) &
            (f[pre + "opposite_tail_ratio"] <= p.reversal_opposite_tail_max) & (rng > 0))
        f[pre + "f3"] = ((~excessive) & (f["overlap_ratio"] < p.reversal_overlap_max) & (c.shift(1).notna()))
        # Shift the signal, not the entry: F4 at t refers to signal t-1.
        valid = min_ok & ~excessive & c.shift(1).notna()
        f[pre + "valid"] = valid
        f[pre + "f4"] = _previous(valid) & strong_bar & (h > h.shift(1) if sign > 0 else l < l.shift(1))
        f[pre + "closes_reversed"] = sum((sign * (c - c.shift(k)) > 0).astype("int64") for k in range(1, p.reversal_close_lookback + 1))
        extremes = h if sign > 0 else l
        f[pre + "extremes_reversed"] = sum((sign * (c - extremes.shift(k)) > 0).astype("int64") for k in range(1, p.reversal_extreme_lookback + 1))
        f[pre + "f5"] = (f[pre + "closes_reversed"] > 0) & (f[pre + "extremes_reversed"] > 0)
        # F4 has a different reference bar, so never mix it into today's score.
        f[pre + "quality_score"] = sum(f[pre + k].astype("int64") for k in ("f1", "f2", "f3", "f5"))

    for side, trend, opposite, tail in (("bull", bull, bear, upper), ("bear", bear, bull, lower)):
        stop = (doji | inside | opposite | ((f["body_ratio"] <= p.stop_body_ratio) &
                 (_ratio(tail, rng) >= p.stop_tail_ratio)))
        active = trend & ~stop
        run = _run(active)
        f["climax_" + side + "_run"] = run
        f["climax_" + side + "_active"] = run >= p.climax_min_bars
        f["climax_" + side + "_end"] = stop & (run.shift(1, fill_value=0) >= p.climax_min_bars)
        f["exhaustion_" + side] = (_run(trend) >= p.exhaustion_min_bars) & (rng >= p.exhaustion_range_atr * atr)

    f["ema_touch"] = (l <= ema) & (h >= ema)
    for side, gap in (("bull", l > ema), ("bear", h < ema)):
        run = _run(gap)
        previous_long_gap = run.shift(1, fill_value=0) >= p.ma_gap_bars
        reclaim = c > ema if side == "bull" else c < ema
        f["ema_gap_" + side] = gap
        f["ema_gap_" + side + "_run"] = run
        f["ema_gap_" + side + "_20"] = run >= p.ma_gap_bars
        f["ema_gap_" + side + "_first_touch"] = previous_long_gap & f["ema_touch"]
        f["ema_gap_" + side + "_touch_reclaim"] = previous_long_gap & f["ema_touch"] & reclaim

    prior_high, prior_low = h.shift(1).rolling(p.breakout_lookback).max(), l.shift(1).rolling(p.breakout_lookback).min()
    bb, bs = sb & (c > prior_high), ss & (c < prior_low)
    f["strong_breakout_bull"], f["strong_breakout_bear"] = bb, bs
    f["measuring_gap_bull"] = _measuring_gap(bb, f["bar_pullback_bull"] | inside, l, h)
    f["measuring_gap_bear"] = _measuring_gap(bs, f["bar_pullback_bear"] | inside, h, l, bull=False)
    f["micro_gap_bull"] = _previous(sb) & (l >= h.shift(2))
    f["micro_gap_bear"] = _previous(ss) & (h <= l.shift(2))
    for side, sign, breakout, trend, strong_bar, other_breakout, breached in (
        ("bull", 1, bb, bull, sb, bs, prior_low), ("bear", -1, bs, bear, ss, bb, prior_high)):
        follow = breakout.shift(p.follow_through_bars, fill_value=False)
        for lag in range(p.follow_through_bars):
            follow &= trend.shift(lag, fill_value=False) & (sign * (c.shift(lag) - c.shift(p.follow_through_bars)) > 0)
        f["breakout_" + side + "_follow_confirm"] = follow
        failed_opposite = _previous(other_breakout) & strong_bar & (sign * (c - breached.shift(1)) > 0)
        f["failed_breakout_" + ("bear" if sign > 0 else "bull")] = failed_opposite
        ref = (h.shift(1).rolling(p.always_in_reversal_bars).max() if sign > 0
               else l.shift(1).rolling(p.always_in_reversal_bars).min())
        reversal = strong_bar & (sign * (c - ref) > 0)
        f["always_in_" + side + "_confirm"] = follow | failed_opposite | reversal
        other_side = (~doji) & (sign * (c - ema) < 0)
        f[side + "_other_side_run"] = _run(other_side)
    f.update(_always_in(f, h, l, c, p))
    f.update(_micro_channels(h, l, bull, bear, atr, p))

    fw = p.flag_window
    flag_hi, flag_lo = h.rolling(fw).max(), l.rolling(fw).min()
    narrow = ((flag_hi - flag_lo <= p.flag_width_atr * atr) &
              ((c - c.shift(fw - 1)).abs() <= p.flag_drift_atr * atr) &
              (f["overlap_ratio"].rolling(fw - 1).mean() >= p.flag_overlap_min))
    for side, sign, trend in (("bull", 1, bull), ("bear", -1, bear)):
        prior_trend = ((trend.rolling(p.flag_min_trend_bars).mean().shift(fw) >= p.strength_trend_fraction) &
                      (sign * (c.shift(fw) - c.shift(fw + p.flag_min_trend_bars - 1)) > 0))
        candidate = narrow & prior_trend
        f["final_flag_" + side + "_candidate"] = candidate
        event, failure = _flag_failures(candidate, flag_hi, flag_lo, c, p, side == "bull")
        f["final_flag_" + side + "_breakout"] = pd.Series(event, index=x.index, dtype=bool)
        f["final_flag_" + side + "_failure"] = pd.Series(failure, index=x.index, dtype=bool)

    w = p.strength_window
    # Require an entire scoring window of warmed features: unavailable EMA/ATR
    # must not be interpreted as evidence for rules about the *absence* of risk.
    ready = pd.Series(np.arange(len(x)) >= p.warmup_bars + w - 2, index=x.index)
    any_recent = lambda b: b.rolling(w, min_periods=w).max().fillna(0).astype(bool)
    total = sum(weight for _, weight in STRENGTH_RULES.values())
    for side, sign, trend, counter, tail, count_side in (
        ("bull", 1, bull, bear, upper, "h"), ("bear", -1, bear, bull, lower, "l")):
        trend_count = trend.rolling(w).sum()
        near_close = l >= c.shift(1) - p.strength_overlap_tolerance_atr * atr if sign > 0 else h <= c.shift(1) + p.strength_overlap_tolerance_atr * atr
        tail_average = _ratio((tail.where(trend, 0)).rolling(w).sum(), rng.where(trend, 0).rolling(w).sum())
        body_gap = trend & (sign * (o - c.shift(1)) > 0)
        extreme = trend & (rng > p.strength_extreme_range_atr * atr)
        largest_counter = (rng.where(counter, -np.inf).rolling(w).max() == rng.rolling(w).max())
        run = _run(trend & (sign * c.diff() > 0))
        # Rule 17 is an observed first pullback after a >=5-bar directional leg.
        origin = h.shift(1) if sign > 0 else l.shift(1)
        depth = origin - l if sign > 0 else h - origin
        pullback = f["bar_pullback_" + side] | inside
        small_pb = ((run.shift(1, fill_value=0) >= p.strength_min_leg_bars) &
                    pullback & ~_previous(pullback) & (depth > 0) &
                    (depth < p.strength_pullback_atr * atr))
        unfilled = l > c.shift(1) if sign > 0 else h < c.shift(1)
        strong_setup = ((f[count_side + "1"] | f[count_side + "2"]) &
                        _previous(f["reversal_" + side + "_valid"]) &
                        (f["body_ratio"].shift(1) >= p.strength_setup_body_ratio))
        repeated = f[count_side + "2"].rolling(w).sum() >= p.strength_repeated_setups
        other_side_pair = f[side + "_other_side_run"] >= p.always_in_other_side_bars
        # Counter spike failed on THIS bar; no label on the earlier spike.
        counter_spike = _run(counter) >= p.climax_min_bars
        failed_spike = (_previous(counter_spike) & trend &
                         (c > h.shift(1) if sign > 0 else c < l.shift(1)))
        rules = {
            2: f["highs_rising"] & f["lows_rising"] if sign > 0 else f["highs_falling"] & f["lows_falling"],
            3: trend.rolling(w).mean() >= p.strength_trend_fraction,
            4: _ratio((trend & near_close).rolling(w).sum(), trend_count) >= p.strength_overlap_fraction,
            5: (tail_average < p.strength_tail_ratio) & (trend_count > 0),
            6: any_recent(body_gap), 8: any_recent(f["measuring_gap_" + side]),
            9: any_recent(f["micro_gap_" + side]), 10: ~any_recent(extreme),
            11: largest_counter, 15: f["ema_gap_" + side + "_20"],
            17: any_recent(small_pb), 18: any_recent(unfilled),
            19: any_recent(strong_setup), 21: repeated,
            22: ~any_recent(other_side_pair), 24: any_recent(failed_spike),
        }
        score = pd.Series(0, index=x.index, dtype="int64")
        for number, (_, weight) in STRENGTH_RULES.items():
            value = (rules[number] & ready).fillna(False)
            f[f"strength_{side}_{number:02d}"] = value
            score += value.astype("int64") * weight
        f[f"strength_{side}_score"] = score
        f[f"strength_{side}_normalized"] = score / total
    f["strength_ready"] = ready
    f["strength_max_score"] = pd.Series(total, index=x.index, dtype="int64")
    return pd.DataFrame(f, index=x.index)
