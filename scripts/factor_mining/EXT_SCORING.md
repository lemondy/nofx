# 扩展打分宇宙

`run_mining` 在 crypto 挖掘、forward、ensemble 和原有 live_scores 导出全部结束后，
调用 `ext_universe.run_extension`。`FM_EXT_UNIVERSE=0` 完全关闭该步骤，输出仍为原 schema。
`--no-fetch` 只使用已有扩展清单和本地数据，不访问 DB 或 HTTP。

扩展清单从 `NOFX_DB`（默认 `data/data.db`）以 SQLite `mode=ro` 读取最近 14 天
candidate_coins 和订单 symbol，按出现次数排序，平频按 symbol 排序，最多 80 个。
exchangeInfo 必须明确满足 TRADING / PERPETUAL / USDT / COIN，排除含 TRADFI 的
subtype、CRYPTO_EXCLUDED、核心 daily 文件中的全部币以及上市不足 min_history 的币。
缺少 underlyingType 或有效 onboardDate 时不入选；缺少 subtype 视为空数组。
`extra_symbols.json` 的 filtered 统计每种原因被排除的不同币数，frequencies 保存入选币频次。
DB、HTTP、原子写入出错时保留原清单。

所有扩展数据和抓取元数据只写入 `<root>/crypto/ext/`：

- `extra_symbols.json`
- `daily/{SYM}.json`
- `alt_daily/{funding,premium}/{SYM}.json`
- `data_meta.json`

抓取复用 data_fetch 的日线、funding、premium 函数以及三天增量重叠。
先抓所有日线，再抓 alt；生产日线 spacing=0.4 秒，alt spacing=0.8 秒。
清单更新与抓取共用 900 秒预算，分页、HTTP timeout、重试等待都检查剩余时间；
达到期限后不再请求，已有磁盘数据仍可打分。测试仅注入 get-only 假对象。
所有异常只写日志，扩展文件和 live_scores 都用原子替换，失败保留旧文件。

核心 `export_live_scores` 函数保持原实现，继续用基础面板和挖掘表达式缓存生成旧字段。
扩展函数读取这个快照，复制所有旧字段，然后一次性追加新字段。
夜间 hook 还检查快照生成时间，核心导出失败留下的旧快照不会被混入当晚的新元数据。

扩展打分包括两类币：ext 目录加载的币，以及已经在核心 daily/基础面板中、
但 as_of 当日 mask 为 False 的数据币（例如未进入流动性前 60 的币）。
后一类自动从基础面板识别，直接使用已有列的日线、alt 和因子值，不读取 ext 目录，
不需要进入 extra_symbols.json 或重复抓取；其数据仍由原有 nightly daily 更新。
两类币共用 z 标准化、pct、`factors<half`、ext_scores/detail 和 universe.extended 规则。

独立的打分面板复制基础日历和 mask，追加 ext 目录的列，其 mask 永远为 False；
基础面板的数据币保留原 mask，as_of 当日仍为 False，不加入核心 scores 或 universe.core。
扩展币只统计 as_of 及以前的已观测日线（基础列按 close 非空天数计），少于 min_history 为 `history<60d`，
空数据为 `no_data`，缺少 as_of 或 as_of 已早于最新 UTC 完结日为 `stale`。
alt 严格按日期对齐，不补值。核心 daily/alt_daily 的读取和 load_panels 均未改变。

每个因子在打分面板重新求值，乘 direction 后，用核心 mask 内均值、样本标准差
（ddof=1）标准化。组合使用 fillna(0) 加总 / 有效因子数；不足半数因子有限的扩展币
在该 horizon 不出分。只有所有 horizon 都没出分的币才进入全局 `skipped: factors<half`。
ext pct 为未舍入的打分面板核心组合 z 的经验 CDF（`核心 z <= 扩展 z` 的比例），
不含扩展币自身；z/pct 保留 4 位，detail 保留 2 位，仅保留有限值。
universe.extended 统计至少一个 horizon 成功的不同扩展币。

CSRank / CSZ / CSDemean 会看到打分面板的全部列，所以加入扩展币可能改变
ext_scores 和 detail（含核心 detail）；旧 scores 始终直接保留基础面板结果。
这也意味着 detail 的均值不要求逐项重现旧 scores。

zoo 的 id 标识表达式，同一 id 可在不同 horizon 有不同的 direction、val、forward。
顶层 factors[id] 保留第一个 horizon 的约定字段；只有发生重复 id 时增量增加
`by_horizon: {"1": {...}, "3": {...}}`，完整保留各 horizon 元数据。
每个 horizon 的 factor_ids 仍使用原始 id，且剔除 forward failed。

扩展步骤不写 zoo、ledger、ensemble、forward、runs 或 status，也不改变挖掘预算。
evaluate.py、expr.py、panel.py、miner.py 和 run_nightly.sh 均不需要改动。
