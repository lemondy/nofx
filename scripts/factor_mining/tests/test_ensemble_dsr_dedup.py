"""Unit tests for train-only monthly-IC correlation deduplication and DSR(N_eff) gating in ensemble.py."""
from __future__ import annotations

import math
import os
import unittest
from unittest.mock import patch

import numpy as np
import pandas as pd

from scripts.backtest import dsr_audit
from scripts.factor_mining import ensemble, options
from scripts.factor_mining.config import MARKETS
from scripts.factor_mining.panel import forward_target, make_splits


def _synth_ic_months(vals: list[float], start_year: int = 2022) -> list[list]:
    out = []
    y, m = start_year, 1
    for v in vals:
        out.append([f"{y:04d}-{m:02d}", float(v)])
        m += 1
        if m > 12:
            m = 1
            y += 1
    return out


class TestEnsembleICDedupAndDSRGate(unittest.TestCase):
    def test_deduplicate_by_ic_corr_drops_clones_and_keeps_orthogonal(self):
        rng = np.random.default_rng(42)
        base = rng.normal(0.02, 0.05, size=24)
        clone = base * 0.95 + rng.normal(0.0, 0.005, size=24)  # corr ~ 0.99
        anti_clone = -base * 0.9 + rng.normal(0.0, 0.005, size=24)  # corr ~ -0.99
        orth = rng.normal(0.01, 0.05, size=24)
        # Project out base to guarantee |corr| < 0.05
        orth = orth - (np.dot(orth, base) / np.dot(base, base)) * base

        cands = [
            {"id": "f1", "train": {"t": 6.0}, "ic_monthly": _synth_ic_months(base.tolist())},
            {"id": "f2_clone", "train": {"t": 5.5}, "ic_monthly": _synth_ic_months(clone.tolist())},
            {"id": "f3_anti", "train": {"t": 5.2}, "ic_monthly": _synth_ic_months(anti_clone.tolist())},
            {"id": "f4_orth", "train": {"t": 5.0}, "ic_monthly": _synth_ic_months(orth.tolist())},
        ]
        kept, dropped = ensemble.deduplicate_by_ic_corr(cands, max_corr=0.50)
        self.assertEqual([f["id"] for f in kept], ["f1", "f4_orth"])
        self.assertEqual([d["id"] for d in dropped], ["f2_clone", "f3_anti"])
        self.assertTrue(all(abs(d["max_corr"]) >= 0.50 for d in dropped))

    def test_deduplicate_train_only_no_val_leakage(self):
        # First 18 months (2022-01..2023-06, train) are orthogonal;
        # Next 18 months (2023-07..2024-12, val) are 100% identical clones.
        rng = np.random.default_rng(7)
        a_tr = rng.normal(0.0, 0.05, size=18)
        b_tr = rng.normal(0.0, 0.05, size=18)
        b_tr = b_tr - (np.dot(b_tr, a_tr) / np.dot(a_tr, a_tr)) * a_tr
        val_clone = rng.normal(0.0, 0.5, size=18)  # huge variance so full-sample corr > 0.9

        cands = [
            {"id": "f1", "ic_monthly": _synth_ic_months(np.concatenate([a_tr, val_clone]).tolist())},
            {"id": "f2", "ic_monthly": _synth_ic_months(np.concatenate([b_tr, val_clone]).tolist())},
        ]
        kept_train_only, dropped_train_only = ensemble.deduplicate_by_ic_corr(
            cands, max_corr=0.50, train_end_ym="2023-06"
        )
        self.assertEqual([f["id"] for f in kept_train_only], ["f1", "f2"])
        self.assertEqual(dropped_train_only, [])

        # Without train_end_ym cutoff, val clone dominates correlation -> f2 dropped
        kept_full, dropped_full = ensemble.deduplicate_by_ic_corr(cands, max_corr=0.50)
        self.assertEqual([f["id"] for f in kept_full], ["f1"])
        self.assertEqual(len(dropped_full), 1)

    def test_compute_ensemble_dsr_matches_dsr_audit_reference(self):
        rng = np.random.default_rng(123)
        pool = []
        for i in range(40):
            ic_m = _synth_ic_months((0.5 * np.sin(np.arange(30) / 3.0) + rng.normal(0, 0.2, 30)).tolist())
            pool.append({
                "id": f"f{i}",
                "horizon": 3,
                "val": {"ls_sharpe": float(rng.normal(0.1, 0.8))},
                "ic_monthly": ic_m,
            })

        ens_metrics = {"val": {"ls_sharpe": 0.40, "n": 360}}
        res = ensemble.compute_ensemble_dsr(ens_metrics, pool, h=3, ppy=365, m_trials=10000, min_dsr=0.95)

        # Cross-check against dsr_audit reference formulas
        ann = 365.0 / 3.0
        srs = [f["val"]["ls_sharpe"] / math.sqrt(ann) for f in pool]
        sr_var = float(np.var(srs, ddof=1))
        months = sorted({m for m, _ in pool[0]["ic_monthly"]})
        mat = pd.DataFrame([{m: dict(f["ic_monthly"]).get(m, np.nan) for m in months} for f in pool]).T
        neff_ref = dsr_audit.effective_trials_from_corr(mat.corr(min_periods=12).to_numpy(), 10000)
        dsr_ref = dsr_audit.dsr_probability(0.40 / math.sqrt(ann), 120, 0.0, 3.0, neff_ref["n_eff"], sr_var)

        self.assertAlmostEqual(res["n_eff"], round(neff_ref["n_eff"], 1), places=1)
        self.assertAlmostEqual(res["sr0"], round(dsr_ref["sr0"], 4), places=4)
        self.assertAlmostEqual(res["dsr"], round(dsr_ref["dsr"], 4), places=4)
        self.assertFalse(res["pass"])
        self.assertEqual(res["status"], "OVERFIT_WARNING")

        # Very strong ensemble SR with few trials must pass >= 0.95
        strong = ensemble.compute_ensemble_dsr(
            {"val": {"ls_sharpe": 5.0, "n": 360}}, pool[:5], h=3, ppy=365, m_trials=5, min_dsr=0.95
        )
        self.assertTrue(strong["pass"])
        self.assertEqual(strong["status"], "PASS")
        self.assertGreaterEqual(strong["dsr"], 0.95)

    def test_build_ensemble_dsr_gate_and_ic_dedup_opt_in(self):
        # Construct minimal synthetic panel & splits using real build_ensemble pipeline
        from types import SimpleNamespace

        dates = pd.date_range("2022-01-01", periods=900, freq="D")
        rng = np.random.default_rng(99)
        syms = [f"C{i}" for i in range(15)]
        close = pd.DataFrame(100.0 * np.exp(np.cumsum(rng.normal(0.0, 0.02, (900, 15)), axis=0)), index=dates, columns=syms)
        mask = pd.DataFrame(True, index=dates, columns=syms)
        panels = SimpleNamespace(fields={"open": close, "close": close}, mask=mask, windows=None, benchmark_open=None)
        cfg = dict(MARKETS["crypto"], horizons=[3], min_names=6)
        splits = {3: make_splits(dates, h=3, windows=None)}
        targets = {3: forward_target(panels, 3)}

        base_ic = rng.normal(0.02, 0.05, 36)
        clone_ic = base_ic * 0.98 + rng.normal(0.0, 0.002, 36)
        v1 = pd.DataFrame(rng.normal(0, 1, (900, 15)), index=dates, columns=syms)
        v2 = v1 * 0.95 + pd.DataFrame(rng.normal(0, 0.1, (900, 15)), index=dates, columns=syms)
        values_cache = {"f1": v1, "f2": v2}
        accepted = [
            {"id": "f1", "expr": "close", "horizon": 3, "status": "accepted", "direction": 1,
             "train": {"t": 5.0}, "val": {"ls_sharpe": 0.1}, "ic_monthly": _synth_ic_months(base_ic.tolist())},
            {"id": "f2", "expr": "close", "horizon": 3, "status": "accepted", "direction": 1,
             "train": {"t": 4.8}, "val": {"ls_sharpe": 0.2}, "ic_monthly": _synth_ic_months(clone_ic.tolist())},
        ]
        # Extra dummy pool so sr_var > 0 and M=10000 drives DSR << 0.95
        all_factors = accepted + [
            {"id": f"d{i}", "horizon": 3, "status": "rejected", "val": {"ls_sharpe": float(rng.normal(0.0, 1.0))},
             "ic_monthly": _synth_ic_months(rng.normal(0, 0.05, 36).tolist())}
            for i in range(30)
        ]
        hmeta = {"3": {"trials": 10000}}

        # 1) Default (flags OFF): both factors kept, exact legacy 4-key schema preserved
        with patch.dict(os.environ, {options.IC_DEDUP: "0", options.DSR_GATE: "0"}, clear=False):
            ens_default = ensemble.build_ensemble(
                "crypto", accepted, panels, targets, splits, cfg,
                values_cache=values_cache, all_factors=all_factors, horizons_meta=hmeta,
            )
        self.assertEqual(ens_default["horizons"]["3"]["factor_ids"], ["f1", "f2"])
        self.assertEqual(set(ens_default["horizons"]["3"]), {"factor_ids", "train", "val", "curve"})

        # 2) FM_IC_DEDUP=1: clone f2 dropped
        with patch.dict(os.environ, {options.IC_DEDUP: "1", options.DSR_GATE: "0"}, clear=False):
            ens_dedup = ensemble.build_ensemble(
                "crypto", accepted, panels, targets, splits, cfg,
                values_cache=values_cache, all_factors=all_factors, horizons_meta=hmeta,
            )
        self.assertEqual(ens_dedup["horizons"]["3"]["factor_ids"], ["f1"])
        self.assertEqual(len(ens_dedup["horizons"]["3"]["ic_dedup"]["dropped"]), 1)

        # 3) FM_DSR_GATE=1: failing DSR ensemble gated out to dsr_rejected
        with patch.dict(os.environ, {options.IC_DEDUP: "0", options.DSR_GATE: "1"}, clear=False):
            ens_gated = ensemble.build_ensemble(
                "crypto", accepted, panels, targets, splits, cfg,
                values_cache=values_cache, all_factors=all_factors, horizons_meta=hmeta,
            )
        self.assertIsNone(ens_gated["horizons"]["3"])
        self.assertIn("3", ens_gated["dsr_rejected"])
        self.assertEqual(ens_gated["dsr_rejected"]["3"]["dsr"]["status"], "OVERFIT_WARNING")


if __name__ == "__main__":
    unittest.main()
