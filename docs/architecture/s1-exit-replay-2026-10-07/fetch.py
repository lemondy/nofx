import csv, json, os, time, urllib.request, sys
S=os.path.dirname(os.path.abspath(__file__))
os.makedirs(f"{S}/k", exist_ok=True)
proxy=urllib.request.ProxyHandler({'https': os.environ.get('HTTPS_PROXY','')})
op=urllib.request.build_opener(proxy)
def get(sym, start, end):
    out=[]; t=start
    while t<end:
        url=f"https://fapi.binance.com/fapi/v1/klines?symbol={sym}&interval=1m&startTime={t}&endTime={end}&limit=1500"
        for attempt in range(4):
            try:
                d=json.loads(op.open(url, timeout=20).read()); break
            except Exception as e:
                if attempt==3: raise
                time.sleep(2)
        if not d: break
        out+= [[r[0], float(r[1]), float(r[2]), float(r[3]), float(r[4])] for r in d]
        t=d[-1][0]+60000
        time.sleep(0.12)
    return out
rows=list(csv.DictReader(open(f"{S}/trades.csv")))
fail=[]
for r in rows:
    fn=f"{S}/k/{r['id']}.json"
    if os.path.exists(fn): continue
    et=int(r['entry_time']); start=et-16*3600_000; end=et+72*3600_000
    try:
        k=get(r['symbol'], start, min(end, int(time.time()*1000)))
        json.dump(k, open(fn,'w'))
    except Exception as e:
        fail.append((r['id'], r['symbol'], str(e)[:80]))
print("done", len(rows), "fail", fail)
