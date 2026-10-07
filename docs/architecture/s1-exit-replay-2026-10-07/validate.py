import csv, json, statistics as st
from sim import COST_BPS
rows=list(csv.DictReader(open('trades.csv'))); res=json.load(open('results.json'))
A=res['A 现行']
prog={'stop_loss','trailing_stop','drawdown_protect','tp_trim','trend_runner'}
pairs=[]
for r,(simR,why) in zip(rows,A):
    if r['close_reason'] not in prog or int(r['entry_time'])<1791043200000-3*86400000: continue  # since 09-30
    e,x,sl=float(r['entry_price']),float(r['exit_price']),float(r['sl'])
    d=1 if r['side']=='LONG' else -1
    act=(x-e)*d/abs(e-sl) - (COST_BPS/1e4)*e/abs(e-sl)
    pairs.append((r['id'],r['symbol'],r['close_reason'],round(act,2),why,round(simR,2)))
for p in pairs: print(p)
a=[p[3] for p in pairs]; s=[p[5] for p in pairs]
print('n',len(pairs),'actual mean %.3f sim mean %.3f'%(st.mean(a),st.mean(s)))
print('MAE %.3f  corr %.3f'%(st.mean(abs(x-y) for x,y in zip(a,s)), st.correlation(a,s)))
print('|diff|<=0.25R:', sum(abs(x-y)<=0.25 for x,y in zip(a,s)),'/',len(a))
