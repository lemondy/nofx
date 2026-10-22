"""Finer expression families used for allocation (miner.family() stays untouched:
its five labels are already persisted in the zoo, UI and tests).

family_fine is a pure function of the expression text, so it never depends on
results and can be applied to historical ledger rows.

Key = <operator group>:<window band>:<flavor>
  operator group  first match in priority order anywhere in the expression:
                  corr (Corr/Cov), trend (Slope/Rsquare/Resi), disp (Std/Var/Mad/Skew/Kurt),
                  extreme (Max/Min/Quantile/Median), mean (Mean/EMA/WMA/Sum),
                  misc (IdxMax/IdxMin/Rank/Delta/Ref or no windowed operator at all)
  window band     longest literal window: s (<=10), m (<=30), l (>30); none -> s
  flavor          v = uses $volume; x = no volume but cross-sectional (CSRank/CSZ/CSDemean);
                  p = plain price/return time-series expression
"""
import ast
import re

from .expr import OPERATORS

_OP_GROUPS = (
    ('corr', ('Corr', 'Cov')),
    ('trend', ('Slope', 'Rsquare', 'Resi')),
    ('disp', ('Std', 'Var', 'Mad', 'Skew', 'Kurt')),
    ('extreme', ('Max', 'Min', 'Quantile', 'Median')),
    ('mean', ('Mean', 'EMA', 'WMA', 'Sum')),
)
_CS = frozenset(('CSRank', 'CSZ', 'CSDemean'))
_WINDOW_POS = {name: (2 if name in ('Corr', 'Cov') else 1)
               for name, spec in OPERATORS.items() if spec['window_arg']}


def features(expr):
    """(field names, operator names, literal windows) of one expression."""
    fields = set(re.findall(r'\$([A-Za-z_]+)', expr))
    tree = ast.parse(re.sub(r'\$([A-Za-z_]+)', r'FM_\1', expr), mode='eval')
    ops, windows = set(), []
    for node in ast.walk(tree):
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
            ops.add(node.func.id)
            pos = _WINDOW_POS.get(node.func.id)
            if pos is not None and len(node.args) > pos:
                arg = node.args[pos]
                if isinstance(arg, ast.Constant) and isinstance(arg.value, (int, float)):
                    windows.append(arg.value)
    return fields, ops, windows


def family_fine(expr):
    try:
        fields, ops, windows = features(expr)
    except (SyntaxError, ValueError):
        return 'unparsed'
    group = next((name for name, members in _OP_GROUPS if ops & set(members)), 'misc')
    longest = max(windows) if windows else 0
    band = 's' if longest <= 10 else 'm' if longest <= 30 else 'l'
    flavor = 'v' if 'volume' in fields else 'x' if ops & _CS else 'p'
    return f'{group}:{band}:{flavor}'
