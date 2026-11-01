"""Synthetic, hand-computed cases for the frozen Fibonacci/FVG study."""

from dataclasses import asdict
from datetime import datetime, timezone
import math
import random

import numpy as np
import pytest

from scripts.backtest import fib_fvg_study as study, ict, ict_bt
from scripts.backtest.ict_study import control_of


def long_bars():
    # Low 4=90, P 8=160, low 12=89, BOS 15, H 16=180, decision 18.
    # First impulse FVG 12..14=[103,110]; the other is [140,150].
    rows = [
        (105, 110, 100, 106), (106, 111, 99, 105), (105, 110, 98, 104), (104, 109, 95, 103),
        (103, 108, 90, 104), (104, 110, 96, 105), (105, 115, 98, 106), (106, 125, 100, 107),
        (107, 160, 101, 108), (108, 120, 100, 107), (107, 112, 98, 103), (103, 108, 94, 99),
        (93, 103, 89, 101), (101, 130, 100, 125), (125, 140, 110, 130), (130, 170, 120, 165),
        (165, 180, 150, 164), (164, 175, 151, 163), (163, 173, 152, 162), (162, 169, 153, 161),
    ]
    return [[i * ict.M5, *r] for i, r in enumerate(rows)]


def mirror(bars, axis=400):
    return [[t, axis - o, axis - lo, axis - hi, axis - c] for t, o, hi, lo, c in bars]


def timestamp(date):
    return int(datetime.fromisoformat(date).replace(tzinfo=timezone.utc).timestamp() * 1000)


def test_hand_computed_long_bos_fvg_fib():
    ctx = ict.Ctx(long_bars())
    setups = study.find_fib_setups(ctx)
    assert len(setups) == 1
    s = setups[0]
    # H=180, L=89 -> fib=[108.474,123.762]; intersection=[108.474,110].
    assert s.ref == pytest.approx(109.237)
    # The fourteen true ranges at 5..18 sum to 362.
    assert s.sl == pytest.approx(89 - .1 * (362 / 14))
    assert s.tp_erl == 160
    assert (s.side, s.kind, s.s, s.idx, s.valid_bars) == ("long", "lim", 15, 18, 48)
    assert s.t_close_ms == 19 * ict.M5
    assert s.extreme == 89
    assert study.find_fib_setups(ctx, fib=False)[0].ref == 106.5


def test_fvg_misses_zone_but_no_fib_variant_remains():
    bars = long_bars()
    bars[14][3] = 106  # FVG [103,106], wholly below the registered zone.
    ctx = ict.Ctx(bars)
    assert study.find_fib_setups(ctx) == []
    v2 = study.find_fib_setups(ctx, fib=False)
    assert len(v2) == 1
    assert v2[0].ref == 104.5


def test_short_symmetry():
    long_ctx = ict.Ctx(long_bars())
    short_ctx = ict.Ctx(mirror(long_bars()))
    long = study.find_fib_setups(long_ctx)[0]
    setups = study.find_fib_setups(short_ctx)
    assert len(setups) == 1
    short = setups[0]
    assert short.side == "short"
    assert short.ref == pytest.approx(400 - 109.237)
    assert short.sl == pytest.approx(400 - long.sl)
    assert short.tp_erl == 240
    assert (short.s, short.idx, short.valid_bars) == (15, 18, 48)


@pytest.mark.parametrize("short", [False, True])
@pytest.mark.parametrize("fib", [False, True])
def test_causal_prefix_invariance(short, fib):
    bars = mirror(long_bars()) if short else long_bars()
    # Future crash/rally creates additional pivots and hourly targets.
    rng = np.random.default_rng(82)
    for i in range(len(bars), 150):
        o = bars[-1][-1]
        c = o * math.exp(rng.normal(0, .08))
        bars.append([i * ict.M5, o, max(o, c) + 2, min(o, c) - 2, c])
    complete = study.find_fib_setups(ict.Ctx(bars), fib=fib)
    assert complete
    for end in range(5, len(bars) + 1):
        expected = [asdict(s) for s in complete if s.idx < end]
        actual = [asdict(s) for s in study.find_fib_setups(ict.Ctx(bars[:end]), fib=fib)]
        assert actual == expected
    assert study.find_fib_setups(ict.Ctx(bars[:18]), fib=fib) == []


def test_first_high_must_occur_after_bos_and_confirm_within_48():
    bars = long_bars()
    # Make BOS itself the first high; the first subsequent high is too late.
    bars[15][2] = 190
    bars[16][2] = 180
    for i in range(20, 68):
        bars.append([i * ict.M5, 160, 170, 150, 160])
    bars.extend([[68 * ict.M5, 160, 185, 150, 160],
                 [69 * ict.M5, 160, 170, 150, 160], [70 * ict.M5, 160, 170, 150, 160]])
    assert study.find_fib_setups(ict.Ctx(bars)) == []


def test_v3_uses_full_window_and_original_gap_v4_unchanged():
    bars = long_bars()
    ctx = ict.Ctx(bars)
    full = [s for s in ict.find_setups(ctx) if s.trigger == "full"]
    assert len(full) == 1
    assert [asdict(s) for s in study.find_fib_setups(ctx, require_sweep=True, fib=False)] == [asdict(s) for s in full]
    v3 = study.find_fib_setups(ctx, require_sweep=True)
    assert len(v3) == 1
    # At idx=15, H=170, L=89, not the later 180 at idx=16.
    expected_lo = 170 - (170 - 89) * .786
    assert v3[0].ref == pytest.approx((max(103, expected_lo) + min(110, 170 - 81 * .618)) / 2)
    assert (v3[0].s, v3[0].idx, v3[0].sl, v3[0].valid_bars) == (12, 15, full[0].sl, 24)
    for end in range(5, len(bars) + 1):
        assert [asdict(s) for s in study.find_fib_setups(ict.Ctx(bars[:end]), require_sweep=True)] == [asdict(s) for s in v3 if s.idx < end]


def test_cluster_se_hand_computed_and_iso_year_boundary():
    # Week sums of residuals are -4,+4; CR1 variance = 2*(16+16)/16=4.
    times = [timestamp(d) for d in ("2026-01-05", "2026-01-06", "2026-01-12", "2026-01-13")]
    assert study.clustered_se([1., 3., 5., 7.], times) == pytest.approx(2.)
    assert study.clustered_se([1., 3.], [times[0], times[2]]) == pytest.approx(1.)
    assert study.clustered_se([], []) is None
    assert study.clustered_se([1., 3.], times[:2]) is None
    assert study.iso_week(timestamp("2025-12-29")) == study.iso_week(timestamp("2026-01-01")) == (2026, 1)
    with pytest.raises(ValueError):
        study.clustered_se([1.], [])


def test_g0_shuffle_preserves_each_symbols_return_multiset_and_offsets():
    rng = np.random.default_rng(20261101)
    symbols = {"A": long_bars(), "B": mirror(long_bars())}
    before = {sym: np.array(bars) for sym, bars in symbols.items()}
    for sym in sorted(symbols):
        original = before[sym]
        shuffled = np.array(study.shuffle_bars(symbols[sym], rng))
        np.testing.assert_allclose(np.sort(np.diff(np.log(shuffled[:, 4]))),
                                   np.sort(np.diff(np.log(original[:, 4]))), atol=2e-15)
        np.testing.assert_allclose(shuffled[:, 1:4] / shuffled[:, 4, None],
                                   original[:, 1:4] / original[:, 4, None])
        np.testing.assert_array_equal(shuffled[:, 0], original[:, 0])
        np.testing.assert_array_equal(symbols[sym], original)
        assert shuffled[0, 4] == pytest.approx(original[0, 4])
        assert not np.allclose(shuffled[:, 4], original[:, 4])
    first = study.shuffle_bars(long_bars(), np.random.default_rng(20261101))
    assert first == study.shuffle_bars(long_bars(), np.random.default_rng(20261101))


def test_job_validity_and_controls_keep_limit_geometry():
    s = study.find_fib_setups(ict.Ctx(long_bars()))[0]
    s.sym = "SYNTH"
    bars = [[i * ict.M5, 160., 170., 150., 160.] for i in range(2200)]
    ict_bt.DATA[s.sym] = ict.load_arrays(bars)
    try:
        job = study._jobs([s], "erl", 0)[0]
        assert job["valid_bars"] == 48
        job["idx"] = 1000
        control = control_of(job, ict_bt.DATA[s.sym], random.Random(7))
        assert control["valid_bars"] == 48
        assert abs(control["ref"] - control["sl"]) / control["ref"] == pytest.approx(abs(job["ref"] - job["sl"]) / job["ref"])
        assert abs(control["ref"] - control["tp"]) / control["ref"] == pytest.approx(abs(job["ref"] - job["tp"]) / job["ref"])
    finally:
        ict_bt.DATA.pop(s.sym)


def test_backtrader_limit_can_fill_after_24_bars_but_before_48():
    bars = long_bars()
    s = study.find_fib_setups(ict.Ctx(bars))[0]
    s.sym = "SYNTH"
    bars.extend([[i * ict.M5, 160., 170., 150., 160.] for i in range(20, 50)])
    bars.extend([[50 * ict.M5, 160., 165., 108., 115.],
                 [51 * ict.M5, 115., 161., 114., 160.],
                 [52 * ict.M5, 160., 162., 150., 161.]])
    ict_bt.DATA[s.sym] = ict.load_arrays(bars)
    try:
        job = study._jobs([s], "erl", 0)[0]
        result = ict_bt.run_jobs([job, dict(job, valid_bars=24)], ict_bt.plain_rules(), procs=2)
        assert result[0]["entry_ms"] == 50 * ict.M5
        assert result[0]["how"] == "tp"
        assert result[0]["r"] > 0
        assert result[1]["r"] is None
        assert result[1]["how"] == "unfilled"
    finally:
        ict_bt.DATA.pop(s.sym)


def test_gate_zero_failure_stops_other_verdicts():
    null = {"groups": {"all": {"setup": dict(n=400, avg=.01),
                               "difference": dict(avg=0., t_cluster=0.)}}}
    gates = study.gate_verdicts(null)
    assert gates["G0"]["pass_"] is False
    assert all(gates[g]["pass_"] is None for g in ("G1", "G2", "G3"))
    assert "方法有偏，无法判定" in study.verdict_lines(gates)[-1]


def test_no_trade_when_erl_fails_1point2r_guard():
    bars = long_bars()
    # A high close enough to entry removes trade space; BOS still breaks it.
    bars[8][2] = 125.1
    bars[13][4] = 124
    bars[14][4] = 125
    # P8 is nearest ERL and below the required 1.2R from the intersection entry.
    assert study.find_fib_setups(ict.Ctx(bars)) == []
