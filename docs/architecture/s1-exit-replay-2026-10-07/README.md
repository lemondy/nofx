# S1 出场参数回放（2026-10-07）

用实盘 145 笔交易（有开仓止损记录、止损距离 <50%）的真实入场，回放币安 1 分钟 K 线，比较不同出场规则。只读公开行情，不访问账户。

复跑：

```bash
# 1. 导出交易（在仓库根目录执行），写入本目录 trades.csv（需带表头）
(echo "id,symbol,side,entry_price,exit_price,sl,tp,entry_time,exit_time,close_reason,exit_mode,mfe_r,mae_r";
 sqlite3 -readonly -csv data/data.db "select p.id,p.symbol,p.side,p.entry_price,p.exit_price,p.initial_stop_loss,coalesce(j.planned_take_profit,0),p.entry_time,p.exit_time,p.close_reason,coalesce(p.exit_mode,''),p.mfe_r,p.mae_r from trader_positions p left join trade_journal j on j.position_id=p.id where p.status='CLOSED' and p.initial_stop_loss>0 and abs(p.entry_price-p.initial_stop_loss)/p.entry_price<0.5 order by p.id;") > docs/architecture/s1-exit-replay-2026-10-07/trades.csv
cd docs/architecture/s1-exit-replay-2026-10-07
python3 fetch.py      # 拉 K 线到 k/（已存在的跳过），需要能访问 fapi.binance.com
python3 sim.py        # 回放全部方案 → results.json
python3 validate.py   # 模拟器校准：与实盘程序出场对比
python3 compare.py    # 方案对比（成对 bootstrap 95% CI、前后半样本）
python3 aiclose.py    # AI 平仓 vs 规则出场；09-29 后子样本
python3 entryq.py     # 与出场无关的入场质量：+xR 先于 -1R 的概率
```

`trades.csv`、`k/`、`results.json` 是数据产物，未纳入版本库。
