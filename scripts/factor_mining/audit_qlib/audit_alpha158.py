import sys, re, json
sys.path.insert(0, sys.argv[1])        # repo root (our engine)
import shim; shim.install()
import numpy as np, pandas as pd
import qlib.data.ops as ops
from qlib.data.base import Expression
from qlib.contrib.data.loader import Alpha158DL
from scripts.factor_mining.expr import evaluate_expr
from scripts.factor_mining.tests.test_expr import make_panels

# run qlib expressions without its cache/provider: every node loads directly
Expression.load = lambda self, inst, s, e, *a: self._load_internal(inst, s, e, *a)

panels = make_panels(400, 6, 7)
class F(Expression):
    def __init__(self, name): self.name = name
    def __str__(self): return '$' + self.name
    def _load_internal(self, inst, s, e, *a): return self.series[inst][self.name]
    def get_longest_back_rolling(self): return 0
    def get_extended_window_size(self): return 0, 0
syms = list(panels['close'].columns)
F.series = {c: {k: panels[k][c].astype(float) for k in panels} for c in syms}

ns = {k: getattr(ops, k) for k in dir(ops) if k[0].isupper()}
def qlib_eval(field):
    py = re.sub(r'\$(\w+)', r'F("\1")', field)
    ex = eval(py, {'F': F, **ns})
    return pd.DataFrame({c: ex._load_internal(c, 0, 0) if hasattr(ex, '_load_internal') else ex.load(c, 0, 0) for c in syms})

from scripts.factor_mining.zoo import seed_factors
zoo = {f['name']: f['expr'] for f in seed_factors()}
fields, names = Alpha158DL.get_feature_config()
rep = []
for f, n in zip(fields, names):
    row = dict(name=n, expr=f)
    try:
        theirs = qlib_eval(f).astype(float).replace([np.inf, -np.inf], np.nan)
    except Exception as exc:
        row.update(status='qlib_error', err=f'{type(exc).__name__}: {exc}'[:120]); rep.append(row); continue
    try:
        if n not in zoo:
            row.update(status='MISSING_IN_ZOO'); rep.append(row); continue
        row['zoo_expr'] = zoo[n]
        ours = evaluate_expr(zoo[n], panels)
    except Exception as exc:
        row.update(status='ours_rejects', err=f'{type(exc).__name__}: {exc}'[:120]); rep.append(row); continue
    a, b = ours.iloc[130:].values, theirs.iloc[130:].values
    both = np.isfinite(a) & np.isfinite(b)
    cover = both.sum() / max(np.isfinite(b).sum(), 1)
    if both.sum() < 10:
        row.update(status='no_overlap', cover=round(float(cover), 3)); rep.append(row); continue
    err = np.abs(a[both] - b[both]) / (np.abs(b[both]) + 1e-9)
    corr = float(np.corrcoef(a[both], b[both])[0, 1])
    row.update(status='OK' if err.max() < 1e-6 else 'DIFF', max_rel_err=float(err.max()), corr=round(corr, 6), cover=round(float(cover), 3))
    rep.append(row)
json.dump(rep, open(sys.argv[2], 'w'), ensure_ascii=False, indent=1)
from collections import Counter
print(Counter(r['status'] for r in rep))
for r in rep:
    if r['status'] != 'OK': print(r['name'], r['status'], r.get('err', ''), r.get('max_rel_err', ''), r.get('corr', ''), r.get('cover', ''))
