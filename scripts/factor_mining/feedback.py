"""Diagnostics shown to the LLM / logs. Deliberately NOT in evaluate.py: that file is
frozen (its SHA256 is recorded per run), and these helpers only read its outputs."""
import numpy as np

from .config import VAL_MIN_T


def train_screen(train, t_req):
    """Train-only verdict that is safe to show the LLM: never reflects validation or holdout."""
    halves = train['ic_halves']
    if train['t'] < t_req:
        return 'fail:train_t_below_threshold'
    if np.sign(halves[0]) != np.sign(halves[1]):
        return 'fail:ic_sign_flips_between_train_halves'
    if train['ic'] <= 0:
        return 'fail:train_ic_not_positive'
    return 'pass'


def reject_reason(train, val, t_req):
    """Why evaluate.classify did not accept; mirrors its checks in order. 'ok' = accepted.
    Internal diagnostics only — it exposes validation results, never send it to the LLM."""
    halves = train['ic_halves']
    if train['t'] < t_req:
        return 'train_t_below_threshold'
    if np.sign(halves[0]) != np.sign(halves[1]):
        return 'ic_sign_flips_between_train_halves'
    if train['ic'] <= 0:
        return 'train_ic_not_positive'
    if val['ic'] <= 0:
        return 'val_ic_not_positive'
    if val['t'] < VAL_MIN_T:
        return 'val_t_below_threshold'
    if val['ls_ret'] <= 0 or train['ls_ret'] <= 0:
        return 'long_short_not_positive'
    return 'ok'
