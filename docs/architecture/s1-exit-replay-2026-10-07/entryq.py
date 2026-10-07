import csv, json, statistics as st, math
rows=list(csv.DictReader(open('trades.csv')))
since=1790611200000
def race(r,k,target):
    d=1 if r['side']=='LONG' else -1; e=float(r['entry_price']); risk=abs(e-float(r['sl'])); et=int(r['entry_time'])
    for t,o,h,l,c in k:
        if t<et+60000: continue
        if t-et>72*3600000: break
        lo=(l-e)*d/risk if d==1 else (e-h)/risk
        hi=(h-e)*d/risk if d==1 else (e-l)/risk
        if lo<=-1: return 0   # conservative: stop first
        if hi>=target: return 1
    return None
for label,cond in [('09-29前',lambda r:int(r['entry_time'])<since),('09-29后',lambda r:int(r['entry_time'])>=since)]:
    out=[]
    for target in (0.5,1.0,2.0):
        xs=[race(r,json.load(open(f"k/{r['id']}.json")),target) for r in rows if cond(r)]
        xs=[x for x in xs if x is not None]
        p=sum(xs)/len(xs); se=math.sqrt(p*(1-p)/len(xs))
        out.append(f"+{target}R先于-1R: {p:.0%}±{1.96*se:.0%} (n={len(xs)})")
    sub=[r for r in rows if cond(r)]
    slp=st.mean(abs(float(r['entry_price'])-float(r['sl']))/float(r['entry_price'])*100 for r in sub)
    print(label, ' | '.join(out), f"| 平均止损距离 {slp:.2f}%")
print("盈亏平衡参考: 到+1R概率需>50%, 到+2R需>33%(不计成本)")
