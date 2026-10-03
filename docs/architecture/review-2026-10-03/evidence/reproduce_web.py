#!/usr/bin/env python3
"""Test the actual page in jsdom; temporary test is removed in finally.

No server, browser session, strategy writes, or production modifications.
Expected assertion failure on reviewed HEAD e68ed1fd.
"""
from pathlib import Path
import subprocess

evidence = Path(__file__).resolve().parent
root = evidence.parents[3]
target = root / 'web/src/pages/__review_20261003.test.tsx'
if target.exists():
    raise SystemExit('Refusing to replace an existing test')
try:
    target.write_text((evidence / 'studio_regressions.tsx.txt').read_text())
    result = subprocess.run(['./node_modules/.bin/vitest', 'run', 'src/pages/__review_20261003.test.tsx'], cwd=root / 'web')
finally:
    target.unlink(missing_ok=True)
raise SystemExit(result.returncode)
