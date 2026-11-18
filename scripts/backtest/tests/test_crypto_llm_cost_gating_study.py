import hashlib
import pandas as pd
import unittest

from scripts.backtest.crypto_llm_cost_gating_study import (
    PREREG,
    PREREG_SHA256,
    apply_gate_rules,
    build_causal_1h_btc_index,
    evaluate_cohort_gate,
    is_all_candidates_blocked,
    is_btc_quiet_causal,
)


class CryptoLlmCostGatingTests(unittest.TestCase):
    def test_prereg_sha256_matches(self):
        digest = hashlib.sha256(PREREG.read_bytes()).hexdigest()
        self.assertEqual(digest, PREREG_SHA256)

    def test_is_all_candidates_blocked(self):
        self.assertTrue(is_all_candidates_blocked('[{"symbol":"BTCUSDT","status":"blocked"}]'))
        self.assertFalse(is_all_candidates_blocked('[{"symbol":"BTCUSDT","status":"allowed"},{"symbol":"ETHUSDT","status":"blocked"}]'))
        self.assertFalse(is_all_candidates_blocked("[]"))

    def test_strict_causal_1h_btc_lookup(self):
        # Bar 1 closes at t=1000ms (quiet), Bar 2 closes at t=4600_000ms (volatile)
        bars = [
            {"close_time": 1_000_000, "open": 100.0, "high": 100.5, "low": 99.8, "close": 100.2},
            {"close_time": 4_600_000, "open": 100.0, "high": 105.0, "low": 99.0, "close": 104.0},
        ]
        cts, feats = build_causal_1h_btc_index(bars)
        # Exactly at close_time=4_600_000, bar 2 is NOT strictly closed before decision -> uses bar 1 (quiet)
        self.assertTrue(is_btc_quiet_causal(4_600_000, cts, feats))
        # At 4_600_001, bar 2 IS strictly closed -> volatile (False)
        self.assertFalse(is_btc_quiet_causal(4_600_001, cts, feats))

    def test_g2_unblocked_transition_wakes_up_and_evaluate_verdict(self):
        bars = [{"close_time": 900_000, "open": 100.0, "high": 100.2, "low": 99.9, "close": 100.1}]
        cts, feats = build_causal_1h_btc_index(bars)
        cycles = [
            {"id": 1, "ts_ms": 1_000_000, "utc_day": "2026-11-01", "all_blocked": False},
            {"id": 2, "ts_ms": 1_300_000, "utc_day": "2026-11-01", "all_blocked": False},  # 5m gap, quiet -> skip
            {"id": 3, "ts_ms": 1_600_000, "utc_day": "2026-11-02", "all_blocked": True},   # blocked -> skip
            {"id": 4, "ts_ms": 1_900_000, "utc_day": "2026-11-02", "all_blocked": False},  # newly unblocked -> wake!
        ]
        mask_g2 = apply_gate_rules(cycles, cts, feats, mode="G2")
        self.assertEqual(mask_g2, [False, True, True, False])

        # Build synthetic trades showing positive daily true net across 3 weeks
        trades = pd.DataFrame(
            [
                {"id": 1, "dr_id": 1, "entry_iso": "2026-11-01T00:00:00Z", "entry_utc_day": "2026-11-01", "cycle_utc_day": "2026-11-01", "net_pnl": 1.0, "r_mult": 1.0},
                {"id": 2, "dr_id": 4, "entry_iso": "2026-11-02T00:00:00Z", "entry_utc_day": "2026-11-02", "cycle_utc_day": "2026-11-02", "net_pnl": 1.0, "r_mult": 1.0},
                {"id": 3, "dr_id": 2, "entry_iso": "2026-11-01T01:00:00Z", "entry_utc_day": "2026-11-01", "cycle_utc_day": "2026-11-01", "net_pnl": -2.0, "r_mult": -1.0},
            ]
        )
        res = evaluate_cohort_gate({"t1": cycles}, trades, cts, feats, mode="G2", placebo_n=100, seed=7)
        self.assertEqual(res["n_cycles_skipped"], 2)
        self.assertGreater(res["total_true_net_delta_usd"], 0.0)


if __name__ == "__main__":
    unittest.main()
