#!/usr/bin/env python3
"""Run review invariants through Go overlays without editing production packages.

Exit 1 is expected on the reviewed revision: the assertions describe the safe
behavior that is currently missing. All account data and credentials are fake.
"""
import json
import os
from pathlib import Path
import subprocess
import tempfile

evidence = Path(__file__).resolve().parent
root = evidence.parents[3]
sources = {
    "trader/review_20261001_test.go": "trader_regressions.go.txt",
    "api/review_20261001_test.go": "api_regressions.go.txt",
    "market/breakout/review_20261001_test.go": "tuning_regressions.go.txt",
}
with tempfile.TemporaryDirectory(prefix="nofx-review-") as scratch:
    overlay = Path(scratch) / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {
        str(root / virtual): str(evidence / source)
        for virtual, source in sources.items()
    }}))
    env = dict(os.environ)
    env.setdefault("GOCACHE", str(Path(scratch) / "go-cache"))
    result = subprocess.run([
        "go", "test", "-overlay", str(overlay),
        "./trader", "./api", "./market/breakout",
        "-run", "^TestReview20261001", "-count=1", "-v",
    ], cwd=root, env=env)
    raise SystemExit(result.returncode)
