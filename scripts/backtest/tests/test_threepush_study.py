"""Causal detector, first passage, matching and pre-registered statistical pipeline."""
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest import threepush_study as s


def hand_bars(gain=4., d=1, spacing=12, repeat=1):
    """Known L0,H1,L1,H2,L2,H3 with strict unique OHLC extrema."""
    n = 540+repeat*7*spacing
    c = np.full(n, 100.)
    for j in range(repeat):
        start = 520+j*7*spacing
        knots = np.array([0, spacing, 2*spacing, 3*spacing, 4*spacing, 5*spacing, 6*spacing])
        values = 100+d*np.array([-5, 5, -2, 5+gain/2, 0, 5+gain, 1.])
        path = np.interp(np.arange(6*spacing+1), knots, values)
        c[start:start+len(path)] = path
        c[start+len(path):] = path[-1]
    return pd.DataFrame(dict(open=c-d*.02, high=c+.1, low=c-.1, close=c, volume=1.),
                        index=pd.date_range('2018-01-01', periods=n, freq='h', tz='UTC'))


class DetectorTests(unittest.TestCase):
    def test_known_features_and_mirror(self):
        for d in (1, -1):
            b = hand_bars(d=d)
            ev = s.detect_patterns(b, '1h')
            # May include opposite patterns later; first registered motif is exact.
            self.assertGreaterEqual(len(ev), 1)
            e = ev.iloc[0]
            self.assertEqual(e.d, d)
            self.assertEqual(e.h3_i, 580)
            self.assertEqual(e.t, 583)
            self.assertEqual(e.post_h3_bars, 3)
            a = s.atr14(b).iloc[583]
            self.assertAlmostEqual(e.slope_atr, 4/a/48)
            self.assertAlmostEqual(e.momentum_decay, 9.2/10.2)
            self.assertAlmostEqual(e.p3_p2, 9.2/9.2)
            self.assertAlmostEqual(e.pullback_depth, ((7.2/10.2)+(7.2/9.2))/2)
            self.assertGreaterEqual(e.overlap, 0)
            self.assertLessEqual(e.overlap, 1)
            self.assertGreater(e.location, .8)
            self.assertAlmostEqual(e.risk, d*(e.stop-e.entry))
            self.assertAlmostEqual(e.rr_space, d*(e.entry-e.l2)/e.risk)

    def test_steep_slow_flat_and_thresholds(self):
        steep = s.detect_patterns(hand_bars(gain=12), '1h').iloc[0]
        slow = s.detect_patterns(hand_bars(gain=1), '1h').iloc[0]
        flat = s.detect_patterns(hand_bars(gain=.05), '1h').iloc[0]
        self.assertGreater(steep.slope_atr, slow.slope_atr)
        self.assertTrue(flat.near_flat)
        self.assertFalse(steep.near_flat)
        self.assertFalse(slow.near_flat)
        ev = pd.DataFrame([steep, slow, flat])
        th = s.fit_thresholds(ev)
        self.assertEqual(list(s.classify(ev, th)), ['steep', 'low', 'flat'])
        # Classification uses the passed frozen cuts, no quantiles on holdout.
        self.assertEqual(s.classify(pd.DataFrame([steep]), th)[0], 'steep')
        self.assertTrue(s.detect_patterns(hand_bars(gain=0), '1h').empty)
        self.assertTrue(s.detect_patterns(hand_bars(spacing=2), '1h').empty)

    def test_confirmation_delay_and_every_feature_truncation(self):
        b = hand_bars(repeat=3)
        for k in (2, 3, 4):
            full = s.detect_patterns(b, '1h', s.Detector(k, .5))
            self.assertGreater(len(full), 0)
            for e in full.itertuples():
                self.assertEqual(e.t, e.h3_i+k)
                before = s.detect_patterns(b.iloc[:e.t], '1h', s.Detector(k, .5))
                self.assertFalse((before.t == e.t).any())
                prefix = s.detect_patterns(b.iloc[:e.t+1], '1h', s.Detector(k, .5))
                pd.testing.assert_frame_equal(prefix.reset_index(drop=True), full[full.t <= e.t].reset_index(drop=True))
            for cut in (570, 582, 583, 584, len(b)-2):
                prefix = s.detect_patterns(b.iloc[:cut], '1h', s.Detector(k, .5))
                pd.testing.assert_frame_equal(prefix.reset_index(drop=True), full[full.t < cut].reset_index(drop=True), check_dtype=False)

    def test_no_reused_extrema_or_gap_bridge(self):
        b = hand_bars(repeat=4)
        ev = s.detect_patterns(b, '1h')
        self.assertGreaterEqual(len(ev), 3)
        seen = set()
        for r in ev.itertuples():
            points = {r.h1_i, r.h2_i, r.h3_i}
            self.assertFalse(points & seen)
            seen |= points
        for prev, nxt in zip(ev.itertuples(), list(ev.itertuples())[1:]):
            self.assertGreater(nxt.l0_i, prev.h3_i)
        b.iloc[565] = np.nan
        gapped = s.detect_patterns(b, '1h')
        self.assertFalse(((gapped.l0_i <= 565) & (gapped.t >= 565)).any())

    def test_tied_pivots_not_confirmed_and_no_future_revision(self):
        b = hand_bars()
        piv = s.confirmed_pivots(b, 3)
        self.assertTrue(all(t == i+3 for i, _, _, t in piv))
        altered = b.copy()
        altered.iloc[600:, altered.columns.get_loc('high')] *= 2
        pd.testing.assert_frame_equal(s.detect_patterns(b.iloc[:600], '1h'), s.detect_patterns(altered, '1h').query('t<600'))


class TargetsAndMatchingTests(unittest.TestCase):
    def test_first_passage_tie_stop_and_forward_close(self):
        b = hand_bars()
        e = s.detect_patterns(b, '1h').iloc[0].to_dict()
        t = e['t']
        b.loc[b.index[t+1], 'high'] = e['stop']+1
        b.loc[b.index[t+1], 'low'] = e['entry']-e['target_distance']-1
        y = s.forward_outcome(b, e, 6)
        self.assertEqual(y['outcome'], -1)
        self.assertEqual(y['r_score'], -1)
        self.assertAlmostEqual(y['rev'], -e['d']*np.log(b.close.iloc[t+6]/b.close.iloc[t]))
        self.assertEqual(s.forward_outcome(b, {**e, 't': len(b)-3}, 6), {})
        invalid = s.forward_outcome(b, {**e, 'risk': -1}, 6)
        self.assertTrue(np.isnan(invalid['r_score']))

    def test_matching_same_bin_distance_exclusions_and_repeatability(self):
        b = s.synthetic_bars(42, 5000, '1h')
        ev = s.detect_patterns(b, '1h')
        allowed = np.ones(len(b), bool)
        train = np.arange(len(b)) < 3000
        excluded = np.zeros(len(b), bool)
        for t in ev.t:
            excluded[t:t+4] = True
        matched = s.match_controls(b, ev, excluded, train, allowed)
        self.assertGreater(len(matched), 5)
        self.assertEqual(matched, s.match_controls(b, ev, excluded, train, allowed))
        for t, result in matched.items():
            indices = result['indices']
            self.assertEqual(len(set(indices)), 5)
            self.assertFalse(excluded[indices].any())
            self.assertLessEqual(max(abs(np.array(indices)-t)), 30*24)
            self.assertEqual(np.searchsorted(result['edges'], result['move'], side='left'), result['bin'])
            self.assertTrue((np.searchsorted(result['edges'], result['control_moves'], side='left') == result['bin']).all())
        restricted = np.arange(len(b)) >= 3000
        hold_events = ev[ev.t >= 3000]
        ho = s.match_controls(b, hold_events, excluded, train, restricted)
        self.assertTrue(all(min(v['indices']) >= 3000 for v in ho.values()))

    def test_exclusion_is_causal_and_never_reaches_before_detection(self):
        detectors = {"k2_p0.5": pd.DataFrame({"t": [8, 18]}),
                     "k4_p0.5": pd.DataFrame({"t": [12, 28]})}
        mask = s.causal_exclusions(30, detectors)
        expected = set(range(8,11)) | set(range(12,17)) | set(range(18,21)) | {28,29}
        self.assertEqual(set(np.flatnonzero(mask)), expected)
        self.assertFalse(mask[7])
        self.assertFalse(mask[11])
        for u in range(30):
            emitted = {k: v[v.t <= u] for k,v in detectors.items()}
            prefix = s.causal_exclusions(30, emitted)
            np.testing.assert_array_equal(prefix[:u+1], mask[:u+1])
            for j in np.flatnonzero(prefix):
                self.assertTrue(any(t <= j <= t+int(k[1]) for k,v in emitted.items() for t in v.t))
        # Integration: prepare_events must use the causal mask, not rebuild ±k.
        b = s.synthetic_bars(12, 1200, "1h")
        seen = []
        def inspect(bars, events, excluded, *args):
            all_detectors = {f"k{k}_p{depth:g}": s.detect_patterns(bars, "1h", s.Detector(k,depth))
                             for k in (2,3,4) for depth in (.25,.5,.75)}
            np.testing.assert_array_equal(excluded, s.causal_exclusions(len(bars), all_detectors))
            seen.append(True)
            return {}
        with patch.object(s, "match_controls", side_effect=inspect):
            s.prepare_events([("S",b)], "1h", s.study_split(b.index,"1h"), "train")
        self.assertEqual(len(seen), 9)

    def test_geometric_targets_never_enter_inference_family(self):
        excluded = {"H1_stop_first", "H1_extension", "H2_R", "H3_space", "H4_breakout"}
        self.assertTrue(excluded.isdisjoint({sp[0] for sp in s.SPECS}))
        self.assertEqual(len(s.SPECS), 11)
        self.assertTrue(all(sp[5] == "diff" if sp[2] != "regression" else sp[0].startswith("H5_") for sp in s.SPECS))
        # Regression consumes matched Y1, never the geometrically biased raw target.
        b = s.planted_bars(9, 2500, "1h")
        events, _ = s.prepare_events([("S",b)], "1h", s.study_split(b.index,"1h"), "train")
        ev = events["k3_p0.5"].copy()
        if len(ev):
            ev["class"] = "medium"
            ev["location_flag"] = 0
            ev["diff"] = np.nan
            ev["rev_net"] = 100.
            fit = s.test_cell(ev, s.SPECS[-1], 6,"1h","pooled")
            self.assertEqual(fit["n"], 0)
            self.assertEqual(fit["p"], 1.)

    def test_split_purge_and_cost_cancellation(self):
        b = s.synthetic_bars(2, 2000)
        split = s.study_split(b.index, '1d')
        self.assertEqual(split['purge_bars'], 120)
        mask = s.phase_mask(b, split, 'train', '1d')
        last = np.flatnonzero(mask)[-1]
        self.assertLess(b.index[last+14], pd.Timestamp(split['train_end_exclusive']))
        signal, control = .003, -.002
        self.assertAlmostEqual((signal-s.rs.CRYPTO_COST)-(control-s.rs.CRYPTO_COST), signal-control)

    def test_full_family_and_insufficient_p_one(self):
        ev = {name.rsplit('_q', 1)[0]: pd.DataFrame(columns=s.FEATURE_COLUMNS) for name in s.parameter_grid()}
        th = {name: {'edges':[0.,1.], 'location_cut':.8} for name in s.parameter_grid()}
        tr = s.run_tests(ev, th, '1d')
        self.assertEqual(len(tr), 10692)
        self.assertTrue((tr.p == 1).all())
        self.assertTrue((tr.status == 'insufficient').all())


class ControlsTests(unittest.TestCase):
    def test_large_panel_pipeline_ci_scaled_down(self):
        # Same two-frequency pipeline; tiny panel and one grid point are CI only.
        # Formal 10-seed large-panel runs are saved externally, never replaced here.
        grid = {s.DEFAULT: (s.Detector(), s.CUTS[0])}
        with patch.object(s, "parameter_grid", return_value=grid), contextlib.redirect_stdout(io.StringIO()):
            result, train, hold, desc = s.synthetic_experiment(100, panel={"1h": (2, 1500), "1d": (2, 900)})
        self.assertEqual(result["tests"], 2*3*3*4*len(s.SPECS))
        self.assertGreater(result["pattern_rows"], 0)
        self.assertTrue((desc.matched_n > 0).any())
        self.assertEqual(set(train.test), {v[0] for v in s.SPECS})
        self.assertTrue(train.q.between(0, 1).all())
        self.assertFalse(s.calibration_status([result], [result])["passed"])

    def test_calibration_requires_large_panels_and_same_key_medium_power(self):
        panel = {tf: {"assets": a, "bars": b} for tf, (a,b) in s.LARGE_PANEL.items()}
        null = [dict(seed=i, panel=panel, train_fdr=0, confirmed=0, sufficient_edge=1) for i in range(100,110)]
        planted = [dict(seed=7, panel=panel, train_fdr=1, confirmed=1, planted_edge_keys=[["same-key"]])]
        self.assertTrue(s.calibration_status(null, planted)["passed"])
        self.assertFalse(s.calibration_status(null[:-1], planted)["passed"])
        self.assertFalse(s.calibration_status(null, [{**planted[0], "planted_edge_keys": []}])["passed"])
        bad = [{**v, "train_fdr": int(v["seed"] < 104)} for v in null]
        self.assertFalse(s.calibration_status(bad, planted)["passed"])


class LifecycleTests(unittest.TestCase):
    def test_freeze_hashes_and_single_use_holdout(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
            out = Path(directory)
            s.seal(out)
            for tf in s.HORIZONS:
                frames = [('S0', s.synthetic_bars(91, 700, tf))]
                s.run_training(out, tf, frames)
            train = s.freeze(out)
            self.assertEqual(len(train), 21384)
            s.verify_frozen(out)
            frames = [('S0', s.synthetic_bars(91, 700, '1d'))]
            s.evaluate(out, '1d', frames=frames)
            with self.assertRaisesRegex(ValueError, 'single-use'):
                s.evaluate(out, '1d', frames=frames)
            with self.assertRaisesRegex(ValueError, 'Frozen experiment'):
                s.run_training(out, '1d', frames)
            threshold = out / 'crypto_1d.thresholds.json'
            threshold.write_text('{}')
            with self.assertRaisesRegex(ValueError, 'Frozen input changed'):
                s.verify_frozen(out)


if __name__ == '__main__':
    unittest.main()
