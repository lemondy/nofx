#!/bin/sh
# Sync scripts/factor_mining into the stable runtime dir used by launchd (same dir as autoresearch).
# Safe to run any time: nothing here is hash-frozen (the evaluator's sha256 is only recorded per run).
set -eu
SRC=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DST=${AR_RUNTIME:-/Users/zhangyun/workspace/nofx-autoresearch}
mkdir -p "$DST/scripts"
touch "$DST/scripts/__init__.py"
rsync -a --delete --exclude '__pycache__' --exclude '*.pyc' --exclude 'tests' "$SRC/scripts/factor_mining/" "$DST/scripts/factor_mining/"
rsync -a --delete --exclude '__pycache__' --exclude '*.pyc' --exclude 'tests' "$SRC/scripts/promotion/" "$DST/scripts/promotion/"
# promotion/report.py optionally imports scripts.backtest.quant_audit (stdlib-only); sync just that file.
mkdir -p "$DST/scripts/backtest"
[ -f "$DST/scripts/backtest/__init__.py" ] || touch "$DST/scripts/backtest/__init__.py"
rsync -a "$SRC/scripts/backtest/quant_audit.py" "$DST/scripts/backtest/quant_audit.py"
echo "synced to $DST/scripts/factor_mining"
