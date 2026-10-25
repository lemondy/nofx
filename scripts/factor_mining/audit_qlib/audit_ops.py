import sys, re, json
sys.path.insert(0, sys.argv[1])        # repo root (our engine)
import shim; shim.install()
import numpy as np, pandas as pd
import qlib.data.ops as ops
from qlib.data.base import Expression
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


cases=[]
for n in (5,20):
    for op in ['Mean','Sum','Std','Var','Skew','Kurt','Max','Min','Med','Mad','Rank','IdxMax','IdxMin','Delta','Slope','Rsquare','Resi','WMA','EMA']:
        ours_op = {'Med':'Median'}.get(op, op)
        cases.append((f'{op}($close,{n})', f'{ours_op}($close,{n})'))
    cases += [(f'Corr($close,$volume,{n})',)*2, (f'Cov($close,$volume,{n})',)*2, (f'Quantile($close,{n},0.3)',)*2, (f'Ref($close,{n})',)*2]
cases += [('Abs($close-$open)',)*2,('Sign($close-$open)',)*2,('Log($volume)',)*2,('Power($close,2)',)*2,('Greater($open,$close)',)*2,('Less($open,$close)',)*2,
          ('If($close>$open,$high,$low)', 'If($close>$open,$high,$low)')]
rep=[]
for q,o in cases:
    try: theirs = qlib_eval(q).astype(float).replace([np.inf,-np.inf],np.nan)
    except Exception as e: rep.append((q,'qlib_error',str(e)[:80])); continue
    try: ours = evaluate_expr(o, panels)
    except Exception as e: rep.append((q,'ours_error',str(e)[:80])); continue
    a,b = ours.iloc[130:].values, theirs.iloc[130:].values
    both = np.isfinite(a)&np.isfinite(b)
    err = np.abs(a[both]-b[both])/(np.abs(b[both])+1e-9)
    rep.append((q, 'OK' if err.max()<1e-6 else 'DIFF', f'max_rel_err={err.max():.3g} cover={both.sum()/max(np.isfinite(b).sum(),1):.3f}'))
for r in rep:
    if r[1]!='OK': print(r)
print(len(rep),'cases;', sum(r[1]=='OK' for r in rep),'OK')
