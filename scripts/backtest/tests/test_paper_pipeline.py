import json
from pathlib import Path
import tempfile
import unittest

from scripts.backtest.paper_pipeline import (
    build_index,
    next_candidates,
    record_status,
    score_paper_text,
)


class PaperPipelineTests(unittest.TestCase):
    def test_scoring_prioritizes_crypto_and_llm_over_unrelated(self):
        s_crypto, t_crypto, _ = score_paper_text("Perpetual Futures Funding Rate Arbitrage in Cryptocurrency", "06-加密市场", tier="curated")
        s_bond, _, _ = score_paper_text("Corporate Governance and Bankruptcy Law in Mortgage Markets", "other", tier="q-fin")
        self.assertGreater(s_crypto, s_bond)
        self.assertIn("crypto_perps_factors", t_crypto)

    def test_index_dedup_and_circuit_breaker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            papers = root / "papers"
            (papers / "01-因子挖掘").mkdir(parents=True)
            (papers / "01-因子挖掘/AlphaForge-2024-arXiv-2406.18394.pdf").write_bytes(b"%PDF-1.4 dummy")
            (papers / "05-研究可靠性").mkdir(parents=True)
            (papers / "05-研究可靠性/DeflatedSharpeRatio-2014-Bailey-LopezDePrado.pdf").write_bytes(b"%PDF-1.4 dummy")

            # Fake researchLog.ts that already mentions 2406.18394
            rlog = root / "researchLog.ts"
            rlog.write_text("export const RESEARCH_LOG = [{id:'x', source:'2406.18394'}];", encoding="utf-8")
            docs = root / "docs"
            docs.mkdir()
            qdir = root / "queue"

            doc = build_index(papers, qdir, research_log_ts=rlog, docs_dir=docs)
            self.assertEqual(doc["counts"]["curated_pdfs"], 2)
            by_id = {c["paper_id"]: c for c in doc["candidates"]}
            self.assertEqual(by_id["arxiv:2406.18394"]["status"], "done")
            self.assertEqual(by_id["curated:DeflatedSharpeRatio-2014-Bailey-LopezDePrado"]["status"], "pending")

            top = next_candidates(qdir, limit=5)
            self.assertEqual(len(top), 1)
            self.assertEqual(top[0]["paper_id"], "curated:DeflatedSharpeRatio-2014-Bailey-LopezDePrado")

            # Recording status in ledger must remove completed/in_progress papers from next_candidates
            record_status("curated:DeflatedSharpeRatio-2014-Bailey-LopezDePrado", "done", queue_dir=qdir)
            self.assertEqual(next_candidates(qdir, limit=5), [])

            # Test 3 consecutive same errors trigger escalation AND halt subsequent next_candidates
            r1 = record_status("p1", "error", error_sig="nas_timeout", queue_dir=qdir)
            r2 = record_status("p2", "error", error_sig="nas_timeout", queue_dir=qdir)
            r3 = record_status("p3", "error", error_sig="nas_timeout", queue_dir=qdir)
            self.assertFalse(r1["escalated_human"])
            self.assertFalse(r2["escalated_human"])
            self.assertTrue(r3["escalated_human"])
            self.assertTrue(r3["breaker_tripped"])
            self.assertEqual(r3["same_error_streak"], 3)
            self.assertIn("Circuit breaker", (qdir / "NEEDS-HUMAN.md").read_text(encoding="utf-8"))
            with self.assertRaises(RuntimeError):
                next_candidates(qdir, limit=5)


if __name__ == "__main__":
    unittest.main()
