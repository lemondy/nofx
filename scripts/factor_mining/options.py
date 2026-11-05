"""Opt-in miner switches.

FM_GREEDY_ENSEMBLE: incremental train-segment ensemble selection.
FM_FAMILY_WEIGHT: weight mutation parents by family train yield.
FM_LLM_MEMORY: expose previous train-only results to the LLM.
FM_DIVERSE_PARENTS: select train-only, correlation/family-diverse parents.

All are OFF unless the environment variable is exactly '1'; read at call time so
tests (and a shell) can flip them without re-importing modules.
"""
import os

GREEDY_ENSEMBLE = 'FM_GREEDY_ENSEMBLE'   # incremental greedy ensemble on the train segment
FAMILY_WEIGHT = 'FM_FAMILY_WEIGHT'       # pick mutation parents weighted by family train yield
LLM_MEMORY = 'FM_LLM_MEMORY'             # show the LLM its previous nights' train-only results
DIVERSE_PARENTS = 'FM_DIVERSE_PARENTS'   # diversify mutation parents and the LLM top-10


def enabled(name):
    return os.environ.get(name) == '1'
