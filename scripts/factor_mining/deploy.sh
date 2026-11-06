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
# promotion/report.py imports scripts.backtest.quant_audit, and the fq_us target steps import
# fq_us_momentum_study / fq_us_volscale_study (plus their backtest imports): sync the Python
# modules of scripts/backtest. No --delete here: the runtime copy may hold older extra files.
mkdir -p "$DST/scripts/backtest"
rsync -a --exclude '__pycache__' --exclude '*.pyc' --exclude 'tests' \
    --include '*/' --include '*.py' --include '*.json' --exclude '*' \
    "$SRC/scripts/backtest/" "$DST/scripts/backtest/"
[ -f "$DST/scripts/backtest/__init__.py" ] || touch "$DST/scripts/backtest/__init__.py"
echo "synced to $DST/scripts/factor_mining"
