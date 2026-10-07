import csv, json, statistics as st, collections
from sim import COST_BPS, boot_ci
rows=list(csv.DictReader(open('trades.csv'))); res=json.load(open('results.json'))
A=res['A 现行']
def actual(r):
    e,x,sl=float(r['entry_price']),float(r['exit_price']),float(r['sl']); d=1 if r['side']=='LONG' else -1
    return (x-e)*d/abs(e-sl)-(COST_BPS/1e4)*e/abs(e-sl)
since=1790611200000  # 09-29 00:00 +08
grp=collections.defaultdict(list)
for i,r in enumerate(rows):
    if int(r['entry_time'])<since: continue
    grp[r['close_reason']].append((actual(r), A[i][0]))
print(f"{'出场原因':18s} {'笔数':>4} {'实际R':>7} {'规则A回放R':>10} {'差(实际-回放)':>12}")
allp=[]
for k,v in sorted(grp.items(), key=lambda kv:-len(kv[1])):
    a=[x for x,_ in v]; s=[y for _,y in v]; allp+=v
    print(f"{k:18s} {len(v):4d} {st.mean(a):+7.3f} {st.mean(s):+10.3f} {st.mean(a)-st.mean(s):+12.3f}")
a=[x for x,_ in allp]; s=[y for _,y in allp]
print(f"{'合计(09-29后)':18s} {len(a):4d} {st.mean(a):+7.3f} {st.mean(s):+10.3f} {st.mean(a)-st.mean(s):+12.3f}")
ai=grp['ai_close']; d=[x-y for x,y in ai]; lo,hi=boot_ci(d)
print(f"ai_close 差值 95%CI [{lo:+.3f},{hi:+.3f}]  AI平仓优于规则的笔数 {sum(x>y for x,y in ai)}/{len(ai)}")
# variants within since-0929 subset
print()
idx=[i for i,r in enumerate(rows) if int(r['entry_time'])>=since]
for name,v in res.items():
    r=[v[i][0] for i in idx]; d=[v[i][0]-A[i][0] for i in idx]; lo,hi=boot_ci(d)
    print(f"09-29后 {name:28s} n={len(r)} 期望 {st.mean(r):+.3f}  vsA {st.mean(d):+.3f} [{lo:+.3f},{hi:+.3f}]")
