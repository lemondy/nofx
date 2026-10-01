#!/usr/bin/env python3
"""多头归因复查(2026-10-01 审计的定时复查,10-10 执行一次)。
只读 data/data.db + 币安公共K线(走本机代理),不改任何规则/数据。
输出:五维归因 + 09-26 修复批次后队列的三项复查判定(见 10-01 归因结论)。
"""
import sqlite3, json, re, os, time
from collections import defaultdict
from datetime import datetime

TID = "d7ff3764_f6b845ba-f7b2-4b27-8501-fcf6af94f276_deepseek_1787982996"
DB = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'data', 'data.db')
os.environ.setdefault('HTTPS_PROXY', 'http://127.0.0.1:7890')
os.environ.setdefault('HTTP_PROXY', 'http://127.0.0.1:7890')
import urllib.request

conn = sqlite3.connect(DB); conn.row_factory = sqlite3.Row
def ts_ms(s):
    try: return int(datetime.fromisoformat(s).timestamp()*1000)
    except Exception: return 0

longs = conn.execute("""SELECT id,symbol,entry_price,exit_price,entry_time,exit_time,
 realized_pnl,fee,initial_stop_loss FROM trader_positions
 WHERE status='CLOSED' AND side='LONG' AND trader_id=? ORDER BY entry_time""",(TID,)).fetchall()
dec_index = defaultdict(list)
for r in conn.execute("SELECT rowid AS rid, timestamp, decision_json, input_prompt FROM decision_records WHERE trader_id=? ORDER BY timestamp",(TID,)):
    ts = ts_ms(r['timestamp'])
    try: arr = json.loads(r['decision_json'] or '[]')
    except Exception: continue
    for d in arr if isinstance(arr,list) else []:
        if isinstance(d,dict) and str(d.get('action','')).startswith('open_long'):
            dec_index[d.get('symbol','')].append((ts, d.get('action',''), d.get('take_profit',0) or 0, r['rid']))

def tag_of(rid, sym):
    ip = conn.execute("SELECT input_prompt FROM decision_records WHERE rowid=?",(rid,)).fetchone()
    if ip and ip['input_prompt']:
        m = re.search(re.escape(sym)+r'\s*\(([^)]+)\)', ip['input_prompt'])
        if m: return re.sub(r'\s+',' ',m.group(1))[:22]
    return '(无tag)'

_kc = {}
def klines(sym):
    if sym in _kc: return _kc[sym]
    url = f"https://fapi.binance.com/fapi/v1/klines?symbol={sym}&interval=1h&limit=500"
    try:
        with urllib.request.urlopen(url, timeout=12) as resp:
            data = json.loads(resp.read())
        _kc[sym] = [(int(b[0]), int(b[6]), float(b[2]), float(b[4])) for b in data]
    except Exception:
        _kc[sym] = []
    time.sleep(0.12)
    return _kc[sym]

def ema_series(closes, n=20):
    out=[]; k=2/(n+1); e=closes[0]
    for c in closes:
        e = c if not out else c*k+e*(1-k)
        out.append(e)
    return out

def ctx_at(sym, ts):
    bars = klines(sym)
    if not bars: return None, None
    ema = ema_series([b[3] for b in bars])
    idx = None
    for i,b in enumerate(bars):
        if b[1] <= ts: idx = i
    if idx is None: return None, None
    hi7 = max((b[2] for b in bars if b[0] <= ts and b[0] > ts-7*86400_000), default=bars[idx][3])
    return ema[idx], hi7

CUTOFF = ts_ms('2026-09-26T00:00:00+00:00')
def bucket_hold(h): return '<30min' if h<30 else ('30~120m' if h<120 else ('2~8h' if h<480 else '>8h'))

rows=[]
for L in longs:
    et=L['entry_time']; net=(L['realized_pnl'] or 0)-(L['fee'] or 0)
    cand=[d for d in dec_index.get(L['symbol'],[]) if et-40*60_000<=d[0]<=et+2*60_000]
    ai=cand[-1] if cand else None
    ema,hi7 = ctx_at(L['symbol'], et)
    rows.append(dict(sym=L['symbol'], et=et, net=net, win=net>0,
        isl=L['initial_stop_loss'] or 0, entry=L['entry_price'], exit=L['exit_price'],
        hold=(L['exit_time']-et)/60000.0, manual=(ai is None),
        mkt=(ai[1]=='open_long') if ai else None, tp=(ai[2] if ai else 0),
        tag=tag_of(ai[3],L['symbol']) if ai else '',
        ema=(L['entry_price']-ema)/ema*100 if ema else None,
        high=(hi7-L['entry_price'])/hi7*100 if hi7 else None))

def agg(keyfn, title, only_ai=True, min_show=1):
    g=defaultdict(lambda: dict(n=0,net=0.0,w=0))
    for r in rows:
        if only_ai and r['manual']: continue
        k=keyfn(r)
        if k is None: continue
        g[k]['n']+=1; g[k]['net']+=r['net']; g[k]['w']+=1 if r['win'] else 0
    print(f"\n== {title} ==  (* = n<30,参考意义弱)")
    for k,v in sorted(g.items(), key=lambda kv: kv[1]['net']):
        if v['n']<min_show: continue
        mark=' ' if v['n']>=30 else '*'
        print(f"  {mark} {k:<46} n={v['n']:>4} net={v['net']:>+8.2f} win={100*v['w']/v['n']:>5.1f}%")

print(f"平仓多头总数: {len(rows)} (AI {sum(1 for r in rows if not r['manual'])} / 手动 {sum(1 for r in rows if r['manual'])})")
print(f"全历史 AI 多头: net={sum(r['net'] for r in rows if not r['manual']):+.2f}")
post = [r for r in rows if not r['manual'] and r['et']>=CUTOFF]
print(f"09-26 后 AI 多头: n={len(post)} net={sum(r['net'] for r in post):+.2f} win={100*sum(1 for r in post if r['win'])/max(1,len(post)):.1f}%")

agg(lambda r: f"{r['mkt'] and '市价' or '限价'}|{r['tag']}", "A) 来源 × 方式(全历史 AI)", min_show=5)
agg(lambda r: (r['mkt'] and '市价' or '限价')+'|'+r['tag'] if r['et']>=CUTOFF else None, "B) 来源 × 方式(09-26 后队列)", min_show=3)
def b_high(r):
    if r['high'] is None: return None
    return 'a)距7日高<2%(贴顶)' if r['high']<2 else ('b)2~5%' if r['high']<5 else ('c)5~10%(回踩带)' if r['high']<10 else 'd)>10%(深回撤)'))
agg(b_high, "C) 距7日高点(全历史 AI)")
agg(lambda r: b_high(r) if r['et']>=CUTOFF else None, "D) 距7日高点(09-26 后队列)", min_show=3)
agg(lambda r: bucket_hold(r["hold"]), "E) 持仓时长(全历史 AI)")
agg(lambda r: bucket_hold(r['hold']) if r['et']>=CUTOFF else None, "F) 持仓时长(09-26 后队列)", min_show=3)

print("\n== G) 出场归因(ISL>0 干净子集, AI) ==")
g=defaultdict(lambda: dict(n=0,net=0.0,w=0))
for r in rows:
    if r['manual'] or r['isl']<=0: continue
    if abs(r['exit']-r['isl'])/r['isl']<0.015: er='SL触发(价≈锚)'
    elif r['tp']>0 and abs(r['exit']-r['tp'])/r['tp']<0.015: er='TP触发(价≈计划)'
    elif r['net']>0: er='浮盈主动平'
    else: er='浮亏主动平(未到SL)'
    g[er]['n']+=1; g[er]['net']+=r['net']; g[er]['w']+=1 if r['win'] else 0
loss_n = sum(v['n'] for k,v in g.items() if '亏损' in k or 'SL' in k)
pre_sl = g['浮亏主动平(未到SL)']['n']
loss_tot = pre_sl + g['SL触发(价≈锚)']['n']
for k,v in sorted(g.items(), key=lambda kv: kv[1]['net']):
    print(f"   {k:<24} n={v['n']:>3} net={v['net']:>+7.2f} avg={v['net']/max(1,v['n']):>+6.2f} win={100*v['w']/max(1,v['n']):>5.1f}%")
share = 100.0*pre_sl/max(1,loss_tot)
print(f"   → 浮亏主动平占亏损出场 {share:.0f}%({pre_sl}/{loss_tot})")

print("\n===== 三项复查判定(对照 10-01 结论)=====")
def verdict(name, cond, detail):
    print(f"  [{'触发' if cond else '未触发'}] {name}: {detail}")
def hi_deep_post():
    gg=defaultdict(lambda: dict(n=0,net=0.0))
    for r in post:
        k=b_high(r)
        if k in ('d)>10%(深回撤)','a)距7日高<2%(贴顶)'):
            gg[k]['n']+=1; gg[k]['net']+=r['net']
    return gg
gg = hi_deep_post()
for k,v in gg.items():
    verdict(f"{k} 仍为负", v['n']>=15 and v['net']<0, f"n={v['n']} net={v['net']:+.2f}(09-26 后队列;n≥15 才判定)")
h_fast = [r for r in post if r['hold']<30]
v_fast = sum(r['net'] for r in h_fast)
verdict("<30min 快进快出仍为负", len(h_fast)>=15 and v_fast<0, f"n={len(h_fast)} net={v_fast:+.2f}")
verdict("浮亏主动平占比>35%", loss_tot>=15 and share>35, f"{share:.0f}%({pre_sl}/{loss_tot};亏损出场 ≥15 笔才判定)")
print("\n判定为'触发'的项 → 按当日数据起草规则建议,交用户拍板;不自动改任何配置。")
