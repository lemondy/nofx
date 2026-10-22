"""Opt-in switches for optimizations that backtests showed no benefit for.

All are OFF unless the environment variable is exactly '1'; read at call time so
tests (and a shell) can flip them without re-importing modules.
"""
import os

GREEDY_ENSEMBLE = 'FM_GREEDY_ENSEMBLE'   # incremental greedy ensemble on the train segment
FAMILY_WEIGHT = 'FM_FAMILY_WEIGHT'       # pick mutation parents weighted by family train yield
LLM_MEMORY = 'FM_LLM_MEMORY'             # show the LLM its previous nights' train-only results


def enabled(name):
    return os.environ.get(name) == '1'
