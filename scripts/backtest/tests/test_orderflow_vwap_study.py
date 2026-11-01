"""Synthetic-only checks for the frozen orderflow/VWAP study."""

import hashlib
import json

import numpy as np
import pandas as pd
import pytest

from scripts.backtest import orderflow_vwap_study as study


def panel(n=850, symbols=35, start="2025-10-15 01:00", seed=123):
    rng = np.random.default_rng(seed)
    index = pd.date_range(start, periods=n, freq="h", tz="UTC")
    columns = [f"S{i:03d}" for i in range(symbols)]
    close = 100 * np.exp(np.cumsum(rng.normal(0, .01, (n, symbols)), axis=0))
    opening = np.vstack((close[:1], close[:-1]))
    v = rng.uniform(1, 20, (n, symbols))
    arrays = (opening, np.maximum(opening, close) + 1, np.minimum(opening, close) - 1,
              close, v, v * close, v * rng.beta(5, 5, (n, symbols)))
    return study.Panel({k: pd.DataFrame(a, index=index, columns=columns)
                        for k, a in zip(study.FIELDS, arrays)})


def clone(p):
    return study.Panel({k: v.copy() for k, v in p.frames.items()})


def test_load_align_hash_ignore_state_and_sample(tmp_path):
    p = panel(5, 2)
    for j, symbol in enumerate(p.columns):
        rows = []
        for t, timestamp in enumerate(p.index):
            if j == 1 and t == 2:
                continue
            end = timestamp.value // 1_000_000
            rows.append([end - 3_600_000, *(p[k].iloc[t, j] for k in study.FIELDS), end])
        (tmp_path / f"{symbol}.json").write_text(json.dumps(rows))
    (tmp_path / "BAD.state.json").write_text("not candle JSON")
    loaded = study.load_panel(tmp_path)
    assert loaded.index.tz is not None
    assert loaded.columns.tolist() == p.columns.tolist()
    assert loaded["close"].iloc[2, 1] != loaded["close"].iloc[2, 1]
    assert loaded["close"].iloc[1, 1] == p["close"].iloc[1, 1]
    for path, digest in loaded.input_hashes.items():
        assert hashlib.sha256((tmp_path / path.split("/")[-1]).read_bytes()).hexdigest() == digest
    assert len(study.load_panel(tmp_path, sample=1).columns) == 1


def test_hand_computed_imb_vwaps_and_atr():
    p = panel(190, 1, start="2026-01-01 12:00")
    c = np.arange(190, dtype=float) + 100
    v = np.arange(190, dtype=float) + 1
    p["close"].iloc[:, 0] = c
    p["high"].iloc[:, 0] = c + 2
    p["low"].iloc[:, 0] = c - 2
    p["base_volume"].iloc[:, 0] = v
    p["quote_volume"].iloc[:, 0] = v * (c - .5)
    p["taker_buy_base"].iloc[:, 0] = .75 * v
    p["taker_buy_base"].iloc[:4, 0] = [1, 0, 2, 1]
    indicators = study.compute_indicators(p)
    controls = study.compute_controls(p, indicators)
    f = study.compute_features(p, controls, indicators)
    assert f["imb_4"].iloc[3, 0] == pytest.approx(-2 / 10)
    expected24 = ((-2) + .5 * v[4:24].sum()) / v[:24].sum()
    assert f["imb_24"].iloc[23, 0] == pytest.approx(expected24)
    expected_vwap = sum(v[:24] * (c[:24] - .5)) / sum(v[:24])
    assert indicators["vwap24"].iloc[23, 0] == pytest.approx(expected_vwap)
    assert indicators["atr14"].iloc[:13, 0].isna().all()
    assert indicators["atr14"].iloc[13, 0] == 4
    assert f["vwap24_dist"].iloc[23, 0] == pytest.approx((c[23] - expected_vwap) / 4)
    expected168 = sum(v[:168] * (c[:168] - .5)) / sum(v[:168])
    assert f["vwap168_dist"].iloc[167, 0] == pytest.approx((c[167] - expected168) / 4)
    # Close time, not open time: exactly 00:00 starts a new daily VWAP.
    midnight = p.index.get_loc(pd.Timestamp("2026-01-02", tz="UTC"))
    assert indicators["vwapD"].iloc[midnight, 0] == pytest.approx(c[midnight] - .5)
    expected_day = sum(v[midnight:midnight+2] * (c[midnight:midnight+2] - .5)) / sum(v[midnight:midnight+2])
    assert indicators["vwapD"].iloc[midnight+1, 0] == pytest.approx(expected_day)
    assert f["vwapD_dist"].iloc[midnight+1, 0] == pytest.approx((c[midnight+1] - expected_day) / 4)
    assert controls["rsi14"].iloc[14, 0] == 100
    assert controls["ret_4"].iloc[4, 0] == pytest.approx(np.log(104 / 100))


def test_wilder_arithmetic_seed_recursion_and_gap_reset():
    values = pd.DataFrame({"x": [2.] * 14 + [16., 2., np.nan] + [4.] * 14})
    result = study.wilder(values)
    assert result.iloc[13, 0] == 2
    assert result.iloc[14, 0] == 3
    assert result.iloc[15, 0] == pytest.approx(3 + (2 - 3) / 14)
    assert result.iloc[16:30, 0].isna().all()
    assert result.iloc[30, 0] == 4


def test_absorb_is_cross_sectional_residual_on_causal_pool():
    p = panel()
    z = study.compute_indicators(p)
    controls = study.compute_controls(p, z)
    f = study.compute_features(p, controls, z)
    assert f["absorb_24"].iloc[:720].isna().all().all()
    t = 750
    residual = f["absorb_24"].iloc[t].to_numpy()
    ret = controls["ret_24"].iloc[t].to_numpy()
    assert residual.sum() == pytest.approx(0, abs=1e-12)
    assert residual @ ret == pytest.approx(0, abs=1e-12)
    # Changing only future availability must not affect today's F3 universe.
    changed = clone(p)
    changed["close"].iloc[t+1:, :3] = np.nan
    other = study.compute_features(changed)
    pd.testing.assert_series_equal(other["absorb_24"].iloc[t], f["absorb_24"].iloc[t])


def test_feature_control_causality_and_label_window():
    p = panel()
    t = 750
    changed = clone(p)
    for name in study.FIELDS:
        changed[name].iloc[t+1:] *= 1.7
    z1, z2 = study.compute_indicators(p), study.compute_indicators(changed)
    c1, c2 = study.compute_controls(p, z1), study.compute_controls(changed, z2)
    f1, f2 = study.compute_features(p, c1, z1), study.compute_features(changed, c2, z2)
    for name in study.FEATURES:
        pd.testing.assert_series_equal(f1[name].iloc[t], f2[name].iloc[t])
    for name in study.CONTROLS:
        pd.testing.assert_series_equal(c1[name].iloc[t], c2[name].iloc[t])
    for h in study.HORIZONS:
        labels = study.make_labels(p, h)
        expected = np.log(p["close"].iloc[t+h] / p["close"].iloc[t])
        pd.testing.assert_series_equal(labels.raw.iloc[t], expected, check_names=False)
        pd.testing.assert_series_equal(labels.excess.iloc[t], expected - expected.median(), check_names=False)
        late = clone(p)
        late["close"].iloc[t+h+1:] *= 2
        pd.testing.assert_series_equal(study.make_labels(late, h).raw.iloc[t], labels.raw.iloc[t])
        endpoint = clone(p)
        endpoint["close"].iloc[t+h, 0] *= 2
        assert study.make_labels(endpoint, h).raw.iloc[t, 0] == pytest.approx(labels.raw.iloc[t, 0] + np.log(2))


def test_eligibility_720_prior_bars_minimum_and_complete_future():
    p = panel(770, 32)
    good = study.eligibility_mask(p, 4)
    assert not good.iloc[719].any()
    assert good.iloc[720].all()
    assert not good.iloc[-4:].any().any()
    missing = clone(p)
    missing["close"].iloc[722, 0] = np.nan
    mask = study.eligibility_mask(missing, 4)
    assert not mask.iloc[720, 0]  # Endpoint exists but an interior label bar is absent.
    assert mask.iloc[720, 1:].all()
    assert mask.iloc[723, 0]  # Warmup counts observed bars; a historical gap does not restart it.
    early_gap = clone(p)
    early_gap["close"].iloc[0, 0] = np.nan
    assert not study.eligibility_mask(early_gap, 4).iloc[720, 0]
    assert study.eligibility_mask(early_gap, 4).iloc[721, 0]
    thin = clone(p)
    thin["quote_volume"].iloc[730, :3] = 0
    assert not study.eligibility_mask(thin, 4).iloc[730].any()  # 29 < 30 drops whole timestamp.
    thin["quote_volume"].iloc[730, 2] = 1
    assert study.eligibility_mask(thin, 4).iloc[730].sum() == 30


def planted(n=160, symbols=60, seed=987):
    rng = np.random.default_rng(seed)
    idx = pd.date_range("2026-01-01", periods=n, freq="h", tz="UTC")
    x = pd.DataFrame(rng.normal(size=(n, symbols)), index=idx)
    y = x * .8 + pd.DataFrame(rng.normal(size=x.shape), index=idx) * .3
    controls = {f"c{i}": pd.DataFrame(rng.normal(size=x.shape), index=idx) for i in range(7)}
    return x, y, controls


def test_spearman_partial_signal_and_exact_control_zero():
    x, y, controls = planted()
    assert study.cross_sectional_ic(x, y).mean() > .85
    assert study.partial_ic(x, controls, y).mean() > .75
    assert abs(study.partial_ic(x, {**controls, "exact": x}, y).mean()) < 1e-12
    # Midranks, including ties, not ordinal ranks.
    tx = pd.DataFrame([[1., 1., 3., 4.]])
    ty = pd.DataFrame([[4., 3., 2., 1.]])
    expected = np.corrcoef([1.5, 1.5, 3, 4], [4, 3, 2, 1])[0, 1]
    assert study.cross_sectional_ic(tx, ty, min_symbols=4).iloc[0] == pytest.approx(expected)
    rg = study.rank_gauss(tx.to_numpy())
    assert rg[0, 0] == rg[0, 1]
    assert np.isfinite(rg).all()


def test_prepared_matches_direct_partial_even_with_missing_controls():
    x, y, controls = planted(50)
    controls["c0"].iloc[5:10, :4] = np.nan
    x.iloc[12, :2] = np.nan
    eligible = y.notna()
    prepared = study.prepare_horizon({"x": x}, controls, study.Labels(y, y, eligible))
    a, b = study.evaluate_prepared(prepared)["x"]
    pd.testing.assert_series_equal(a, study.cross_sectional_ic(x, y, eligible))
    pd.testing.assert_series_equal(b, study.partial_ic(x, controls, y, eligible))


def test_newey_west_iid_distribution_and_ar_lag():
    rng = np.random.default_rng(12)
    ts = np.array([study.newey_west_t(a, 4) for a in rng.normal(size=(400, 2000))])
    assert abs(ts.mean()) < .2
    assert .8 < ts.std() < 1.2
    noise = rng.normal(size=5000)
    ar = np.zeros(5000)
    for i in range(1, len(ar)):
        ar[i] = .85 * ar[i-1] + noise[i]
    ar += .4
    assert study.newey_west_t(ar, 24) < study.newey_west_t(ar, 1)
    # Direct Bartlett reference, including the n divisor.
    a = np.array([.1, -.2, .4, .8, -.3, .1])
    u = a - a.mean()
    variance = (u @ u + 2 * (1 - 1 / 3) * (u[1:] @ u[:-1]) + 2 * (1 - 2 / 3) * (u[2:] @ u[:-2])) / len(a)
    assert study.newey_west_t(a, 2) == pytest.approx(a.mean() / np.sqrt(variance / len(a)))


def test_monthly_sign_consistency():
    idx = pd.to_datetime(["2026-01-01", "2026-01-02", "2026-02-01", "2026-03-01"], utc=True)
    result = study.monthly_sign_consistency(pd.Series([.4, .6, -.1, .3], index=idx))
    assert result["monthly_sign_pct"] == pytest.approx(200 / 3)
    assert result["monthly_means"]["2026-01"] == .5


@pytest.mark.parametrize("h", study.HORIZONS)
def test_dev_holdout_and_purge(h):
    idx = pd.date_range("2025-11-14", "2026-07-04", freq="h", tz="UTC")
    masks = study.split_masks(idx, h)
    assert not masks["dev"][idx.get_loc(study.DEV_START - pd.Timedelta(1, unit="h"))]
    assert masks["dev"][idx.get_loc(study.DEV_START)]
    last_dev = study.HOLDOUT_START - pd.Timedelta(h+1, unit="h")
    assert masks["dev"][idx.get_loc(last_dev)]
    assert not masks["dev"][idx.get_loc(last_dev + pd.Timedelta(1, unit="h"))]
    assert masks["holdout"][idx.get_loc(study.HOLDOUT_START)]
    assert masks["holdout"][-h-1]
    assert not masks["holdout"][-h:].any()
    assert not (masks["dev"] & masks["holdout"]).any()


@pytest.mark.parametrize("h", study.HORIZONS)
def test_g3_fixed_nonoverlapping_grid_and_cost(h):
    idx = pd.date_range(study.HOLDOUT_START, periods=100, freq="h")
    x = pd.DataFrame(np.tile(np.arange(31.), (100, 1)), index=idx)
    y = (x - 15) * .001
    eligible = x.notna()
    eligible.iloc[0] = False  # Must not shift the sampling grid to 01:00.
    labels = study.Labels(y.where(eligible), y.where(eligible), eligible)
    result = study.g3(x, labels, h, 1)
    times = pd.DatetimeIndex(result["times"])
    assert times[0] == study.HOLDOUT_START + pd.Timedelta(h, unit="h")
    assert ((times - study.HOLDOUT_START) % pd.Timedelta(h, unit="h") == pd.Timedelta(0)).all()
    assert ((times[1:] - times[:-1]) >= pd.Timedelta(h, unit="h")).all()
    assert (times[-1] + pd.Timedelta(h, unit="h")) <= idx[-1]
    expected_gross = np.mean(np.arange(25, 31) * .001) - np.mean(np.arange(6) * .001)
    assert result["gross_mean"] == pytest.approx(expected_gross)
    assert result["net_mean"] == pytest.approx(expected_gross - .0016)
    assert result["symbols_per_leg"] == [6] * result["n"]
    assert result["block_length_periods"] == 24 // h
    assert result["bootstrap_runs"] == 2000
    assert result["bootstrap_p_mean_gt_zero"] == 1
    assert result["passed"]
    assert not study.g3(x, labels, h, -1)["passed"]
    assert study.block_bootstrap_probability(np.full(20, -.001), h) == 0


def test_permutation_null_near_zero_reproducible_and_preserves_missing():
    x, y, controls = planted(220)
    eligible = y.notna()
    eligible.iloc[:10, :4] = False
    y = y.where(eligible)
    prepared = study.prepare_horizon({"x": x}, controls, study.Labels(y, y, eligible))
    result = study.permutation_null({4: prepared}, runs=8)
    means = [r["cells"]["x/h4"]["ic"]["mean"] for r in result["run_results"]]
    assert max(abs(np.asarray(means))) < .04
    assert abs(np.mean(means)) < .015
    assert result["passed"] is None  # Eight runs cannot confirm the frozen 100-run G0.
    again = study.permutation_null({4: prepared}, runs=8)
    assert result == again
    rng = np.random.default_rng(9)
    shuffled = study.permute_labels(y.to_numpy(), eligible.to_numpy(), rng)
    assert np.array_equal(np.isnan(shuffled), ~eligible.to_numpy())
    for row in (0, 12):
        np.testing.assert_array_equal(np.sort(shuffled[row, eligible.iloc[row]]), np.sort(y.iloc[row].dropna()))


def test_synthetic_panel_deterministic_volumes_shape_and_missing():
    p = panel(800, 3)
    for key in study.FIELDS:
        p[key].iloc[20, 0] = np.nan
    a, b = study.synthetic_random_walk(p), study.synthetic_random_walk(p)
    for key in study.FIELDS:
        pd.testing.assert_frame_equal(a[key], b[key])
        assert a[key].shape == p[key].shape
        assert np.array_equal(a[key].isna(), p[key].isna())
    for j in range(3):
        np.testing.assert_array_equal(np.sort(a["base_volume"].iloc[:, j].dropna()),
                                      np.sort(p["base_volume"].iloc[:, j].dropna()))
    ratio = a["taker_buy_base"] / a["base_volume"]
    assert ((ratio >= 0) & (ratio <= 1) | ratio.isna()).all().all()
    assert a["close"].equals(b["close"])
    assert not a["close"].equals(p["close"])


def test_gates_direction_and_g0_verdict_block():
    dev = {"mean": -.02, "nw_t": -3.1}
    part = {"mean": -.01, "nw_t": -2.1}
    assert study.g1(dev, part)
    assert not study.g1(dev, {"mean": .01, "nw_t": 4})
    assert study.g2(dev, {"mean": -.02, "nw_t": -2.1}, {"mean": -.01, "nw_t": -1.5})
    assert not study.g2(dev, {"mean": .02, "nw_t": 4}, {"mean": .01, "nw_t": 3})
    cells = {"x/h4": {"g1": True, "g2": True, "g3": {"passed": True}}}
    assert study.verdict(cells, {"passed": False, "status": "failed"})["text"] == "不解读"
    assert study.verdict(cells, {"passed": None, "status": "skipped"})["candidates"] == []
    assert study.verdict(cells, {"passed": True, "status": "passed"})["candidates"] == ["x/h4"]
    cells["x/h4"]["g2"] = False
    assert study.verdict(cells, {"passed": True})["text"] == "delta/CVD、VWAP 偏离相对已有指标无增量"


def test_btc_regime_is_causal():
    p = panel(400, 1, start="2026-01-01 01:00")
    for key in study.FIELDS:
        p.frames[key].columns = ["BTCUSDT"]
    p["close"].iloc[:, 0] = np.arange(400.) + 100
    regime = study.btc_regime(p)
    assert regime.iloc[210] == "bull"
    changed = clone(p)
    changed["close"].iloc[211:] *= .1
    pd.testing.assert_series_equal(regime.iloc[:211], study.btc_regime(changed).iloc[:211])
    # A completed 4h value is available at 04:00, not at 01:00.
    assert (regime.iloc[:199] == "unavailable").all()


def test_time_series_pooled_uses_raw_returns_and_coin_demeaning():
    index = pd.date_range("2026-01-01", periods=3, freq="h", tz="UTC")
    x = pd.DataFrame([[1., 11.], [2., 12.], [3., 13.]], index=index)
    raw = pd.DataFrame([[-1., -1.], [0., 0.], [1., 1.]], index=index)
    result = study.time_series_pooled_ic(x, raw)
    assert result["ic"] == pytest.approx(1)
    assert result["n"] == 6


def test_json_safe_is_strict():
    result = study.json_safe({"a": np.nan, "b": np.int64(5), "c": np.array([1., np.inf])})
    assert json.loads(json.dumps(result, allow_nan=False)) == {"a": None, "b": 5, "c": [1., None]}
