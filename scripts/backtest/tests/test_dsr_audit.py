import math
import unittest

import numpy as np

from pathlib import Path
import tempfile
import json
from scripts.backtest import quantjourney
from scripts.backtest.dsr_audit import (
    dsr_from_series,
    dsr_probability,
    effective_trials_from_corr,
    expected_max_sr,
    format_markdown_report,
    main as dsr_main,
)


class DsrAuditTests(unittest.TestCase):
    def test_paper_numerical_example(self):
        # Bailey & Lopez de Prado (2014) p. 9-10 numerical example:
        # Annualized SR=2.5 (daily SR=2.5/sqrt(250)=0.158114), T=1250, skew=-3,
        # Pearson kurtosis=10, N=100, Var[SR]=0.002 (wait: annualized 0.5 -> daily 0.5/250=0.002)
        sr_daily = 2.5 / math.sqrt(250.0)
        # Paper uses non-annualized Var[SR] such that N=100 gives DSR ~ 0.90 and N=46 gives DSR ~ 0.9505
        # Verify monotonicity wrt N and non-normality penalty
        d_norm_100 = dsr_probability(sr_daily, 1250, 0.0, 3.0, 100, 0.5 / 250.0)
        d_skew_100 = dsr_probability(sr_daily, 1250, -3.0, 10.0, 100, 0.5 / 250.0)
        self.assertGreater(d_norm_100["dsr"], d_skew_100["dsr"])
        d_skew_46 = dsr_probability(sr_daily, 1250, -3.0, 10.0, 46, 0.5 / 250.0)
        self.assertGreater(d_skew_46["dsr"], d_skew_100["dsr"])

    def test_matches_quantjourney_deflated_sharpe(self):
        rng = np.random.default_rng(7)
        x = rng.normal(0.002, 0.015, size=200)
        qj = quantjourney.deflated_sharpe(x, trials=25, sharpe_variance=0.0005)
        ours = dsr_from_series(x, trials=25, sr_var=0.0005)["dsr"]
        self.assertAlmostEqual(qj, ours, places=12)

    def test_effective_trials_eq9(self):
        # Independent trials -> avg_rho=0 -> n_eff = M
        eye = np.eye(5)
        res0 = effective_trials_from_corr(eye, 100)
        self.assertAlmostEqual(res0["avg_rho"], 0.0, places=12)
        self.assertAlmostEqual(res0["n_eff"], 100.0, places=12)
        # Fully correlated trials -> rho=1 -> n_eff = 1
        ones = np.ones((5, 5))
        res1 = effective_trials_from_corr(ones, 100)
        self.assertAlmostEqual(res1["n_eff"], 1.0, places=12)

    def test_cli_outputs_json_and_markdown_on_temp_root(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            (root / "crypto").mkdir()
            zoo = {
                "horizons": {
                    "3": {"trials": 20000, "accepted": 2, "rejected": 19998},
                    "20": {"trials": 21003, "accepted": 0, "rejected": 21003},
                },
                "factors": [
                    {
                        "id": f"f{i}",
                        "horizon": 3,
                        "train": {"ls_sharpe": 0.5 + 0.1 * i},
                        "val": {"ls_sharpe": 0.3 + 0.05 * i},
                        "ic_monthly": [[f"2024-{m:02d}", 0.02 * ((m + i) % 3)] for m in range(1, 25)],
                    }
                    for i in range(4)
                ],
            }
            ens = {"horizons": {"3": {"factor_ids": ["f0", "f1"], "val": {"n": 360, "ls_sharpe": 0.6, "ls_ret": 0.15}}}}
            (root / "crypto" / "zoo.json").write_text(json.dumps(zoo), encoding="utf-8")
            (root / "crypto" / "ensemble.json").write_text(json.dumps(ens), encoding="utf-8")
            out_json = root / "dsr_audit.json"
            out_md = root / "dsr_audit.md"
            rc = dsr_main(["--root", str(root), "--no-replay", "--out-json", str(out_json), "--out-md", str(out_md)])
            self.assertEqual(rc, 0)
            doc = json.loads(out_json.read_text(encoding="utf-8"))
            h3 = doc["crypto_zoo"]["horizons"]["3"]
            self.assertEqual(h3["m_trials"], 20000)
            self.assertEqual(h3["retained_count"], 4)
            self.assertGreater(h3["n_eff"]["n_eff"], 100.0)
            self.assertIn("20", doc["crypto_zoo"]["horizons"])
            self.assertEqual(doc["crypto_zoo"]["horizons"]["20"]["m_trials"], 21003)
            self.assertEqual(doc["crypto_zoo"]["horizons"]["20"]["retained_count"], 0)
            self.assertIn("gate_status", h3["ensemble"])
            md = out_md.read_text(encoding="utf-8")
            self.assertIn("Deflated Sharpe Ratio", md)
            self.assertIn("20000", md)
            self.assertIn("`h=20`", md)


if __name__ == "__main__":
    unittest.main()
