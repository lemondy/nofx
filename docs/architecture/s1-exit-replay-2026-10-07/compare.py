import csv, json, statistics as st, collections
from sim import boot_ci, maxdd
rows=list(csv.DictReader(open('trades.csv'))); res=json.load(open('results.json'))
order=sorted(range(len(rows)), key=lambda i:int(rows[i]['entry_time']))
A=[res['A 现行'][i][0] for i in range(len(rows))]
half=len(order)//2
first=set(order[:half]); second=set(order[half:])
print(f"{'方案':28s} {'期望R':>7} {'中位':>6} {'胜率':>5} {'均盈':>6} {'均亏':>6} {'总R':>7} {'最大回撤R':>8} | {'vs A 差值 [95%CI]':>22} | 前半 后半")
for name,v in res.items():
    r=[x[0] for x in v]
    w=[x for x in r if x>0]; l=[x for x in r if x<=0]
    seq=[r[i] for i in order]
    d=[r[i]-A[i] for i in range(len(r))]
    lo,hi=boot_ci(d)
    f=st.mean(r[i]-A[i] for i in first); s=st.mean(r[i]-A[i] for i in second)
    print(f"{name:28s} {st.mean(r):+7.3f} {st.median(r):+6.2f} {len(w)/len(r):5.0%} {st.mean(w):6.2f} {st.mean(l):6.2f} {sum(r):+7.1f} {maxdd(seq):8.1f} | {st.mean(d):+6.3f} [{lo:+.3f},{hi:+.3f}] | {f:+.3f} {s:+.3f}")
print()
for name in ['A 现行','B 关闭0.5R保本(1R仍锁保本)','C B+回撤保护1.5R/60%','D 只留SL/TP+1R锁+移动止损']:
    c=collections.Counter(x[1] for x in res[name]); print(name, dict(c))
