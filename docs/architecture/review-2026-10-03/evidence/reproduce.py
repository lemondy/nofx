#!/usr/bin/env python3
"""Isolated regression evidence. No exchange calls or production changes.

Tests assert safe behavior. Expected nonzero on reviewed HEAD e68ed1fd.
"""
import json
from pathlib import Path
import subprocess
import tempfile

evidence = Path(__file__).resolve().parent
root = evidence.parents[3]
with tempfile.TemporaryDirectory(prefix="nofx-review-03-") as scratch:
    overlay = Path(scratch) / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {
        str(root / "trader/review_20261003_test.go"): str(evidence / "trader_regressions.go.txt"),
        str(root / "store/review_20261003_test.go"): str(evidence / "store_regressions.go.txt"),
    }}))
    result = subprocess.run([
        "go", "test", "-overlay", str(overlay), "./trader", "./store",
        "-run", "^TestReview03", "-count=1", "-v",
    ], cwd=root)
    raise SystemExit(result.returncode)
