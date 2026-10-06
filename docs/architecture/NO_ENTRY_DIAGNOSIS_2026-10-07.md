# 最近几小时未成交开仓：运行诊断 · 2026-10-07

统计窗口：**2026-10-07 00:00:00–06:15:00，Asia/Shanghai**。只读查看运行日志、数据库和二进制元数据；复现使用生产版本源码 overlay 与虚构行情，不访问实际交易账户。

**主因：程序确实提交了入场限价单，但挂单监控随后将它们全部秒撤。新挂单复核读取约 20 根聚合 15m K 线，却以至少 60 根校验；生产版本中这会触发 DATA_INSUFFICIENT 等禁开码，取消未成交订单。这是一处之前审查漏掉的执行链路缺陷。**

## 1. 运行事实

- 当前 PID 28632，启动时间 10-06 21:42:54；二进制元数据为 `74a5daae0c5a8c74029e46eeeb3237713a492c16`、`vcs.modified=false`，磁盘构建时间为 21:42。
- 窗口内完成并保存 54 个决策周期；后台猪头和做空榜持续更新，日志没有 panicked 或 protection fault 匹配。活动 trader 为 mac-nofx；空军一号处于停止状态。
- 29 笔成功提交的入场限价单，29 笔全部匹配到程序发出的撤单；挂出到撤单间隔 **1–31 秒，中位数 3 秒**。
- 29 笔按符号分布：BZUSDT 12、SNXXUSDT 5、MUUSDT 5、BTWUSDT 2，其余 SNDKUSDT/PROMUSDT/CRWVUSDT/QNTUSDT/DOGEUSDT 各 1。反复挂撤不等于 29 个独立交易机会。
- 窗口内数据库没有新增成交持仓；当前持仓数 0，挂单状态表为空。最近账户成交记录为 10-06 22:23:56 的 CAPUSDT，已在 22:25:06 平仓；该记录由 sync 导入，不能仅凭 ai_managed=0 判定实际操作者。

日志例子（[当天运行日志](/Users/zhangyun/workspace/nofx/data/nofx_2026-10-07.log)）：

| 符号 | 入场挂单时间 | 程序撤单时间 | 存活 |
| --- | --- | --- | --- |
| SNXXUSDT short | 00:18:03 | 00:18:07 | 4 秒 |
| BZUSDT long | 02:49:25 | 02:49:29 | 4 秒 |
| BZUSDT long | 05:25:29 | 05:25:30 | 1 秒 |
| DOGEUSDT short | 05:50:50 | 05:51:00 | 10 秒 |

## 2. 已复现的数据口径冲突

1. AI 分析端按策略获取 15m/1h/4h/1d，保存的 SNXXUSDT（cycle 5843）、BZUSDT（5864）、DOGEUSDT（5891）信号均为每周期 60 根、data_quality.sufficient=true、开仓方向 failed=[]。
2. 挂单复核的 [pendingDirectionBlocked](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending_risk.go:148) 调用通用 getMarketData，而不是按策略周期获取行情。
3. [getMarketData](/Users/zhangyun/workspace/nofx/trader/auto_trader_marketdata.go:13) 进入 [GetWithExchangeAndPrice](/Users/zhangyun/workspace/nofx/market/data.go:104)，只请求 **100 根 3m**（第 128 行）以及 1h/4h/1d。
4. [executionTimeframeData](/Users/zhangyun/workspace/nofx/market/data.go:1080) 从 3m 本地聚合 15m，连续 100 根仅产生约 20–21 根；形成中尾根进一步由指标函数排除。
5. 复核却传入 PrimaryTF=15m 和策略的 selected_timeframes，信号数据质量要求 15m 至少 60 根。源码及隔离复现均得到 **15m: 20 bars < 60 minimum**，多/空硬门都包含 DATA_INSUFFICIENT。20 根的短历史也可能改变微趋势等其他信号。
6. [processPendingEntries](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:484) 遇到非空复核原因便取消 NEW/PARTIALLY_FILLED 订单。保护监控每 30 秒运行，并与决策周期共享执行锁；挂单在 AI 执行完成后的下一次监控获得锁时即可撤掉。

git blame 确认该新增挂单复核函数来自 `abba5978`（10-06 19:26:46），随后部署在 74a5daae 中。已修复的空头数量及 scanner panic 路径不是本轮定位点。

**隔离动态验证：**恢复 74a5daae 的业务源码，将行情读取边界注入实际 executionTimeframeData 生成的虚构行情，其余 pendingDirectionBlocked/processPendingEntries/cancelPending 保持原实现。原周期 gate allowed=true、挂单年龄不足一秒：

| 复现条件 | 多仓挂单 | 空仓挂单 |
| --- | --- | --- |
| 关闭 EntryTimingGate，仅隔离数据量缺口 | DATA_INSUFFICIENT，撤单并清除 pending | DATA_INSUFFICIENT，撤单并清除 pending |
| 开启 EntryTimingGate，与当前配置一致 | fixture 的首个原因为 MICRO_TREND_NOT_LONG，撤单 | fixture 的首个原因为 MICRO_TREND_NOT_SHORT，撤单 |

因此降低微趋势门也不能消除确定的历史长度缺口。上述 fixture 的微趋势结果不是对每笔真实订单历史行情的回放。

**证据边界：**线上成功撤单路径没有输出 reason；[这里](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:485) 仅在撤单失败时记录原因。因此“29 笔都被程序秒撤”是直接日志事实，“20/60 数据缺口会导致挂单复核撤单”是已复现的确定性缺陷；无法从现有日志证明每笔真实撤单的第一个触发码都恰好为 DATA_INSUFFICIENT。它可能先遇到微趋势、BTC 数据可用性或其他复核禁开条件。

## 3. 次要因素

- 大部分符号决策为 wait：窗口内保存 575 条 wait，常见记录因素包括 TIMING_GATE、STRUCTURE_CONFLICT、CONFLICT_UNRESOLVED、RR_LOW、CONSENSUS_OPPOSED。这些是逐符号、逐周期重复标签，不能相加当独立候选数量。
- 03:11 的 CRWVUSDT 多头入场被 supply-zone 风控拒绝；05:35 的 CRVUSDT 空头在执行前被 15m regime line 拦截。它们是具体局部拒绝，无法解释其余 29 笔成功提交后秒撤。
- 00:08 扫描上游 DNS/代理验证失败一次，后续正常；AI 请求有一次超时重试。05:23 的八条 ERRO 是前端 token 过期，非交易 API 凭据失效。
- 当前账户权益约 129.65，初始约 135.18，账户回撤约 4.09%，低于配置 20% 熔断；daily_max_loss_pct=0。未见账户整体资金或回撤熔断解释这批秒撤。

## 4. 修复方向与证据

优先修复挂单复核的数据获取契约：按配置读取执行交易所的真实周期，保证每个必需周期至少 60 根闭合 K 线，再运行同口径的复核。补真实配置下“挂出→监控→继续保留→成交”的测试，并保留方向失效时的撤单能力。成功撤单也应记录 reason、各周期数量和来源，使逐单归因可追踪。

本轮只诊断，没有修改业务代码、策略配置或重启进程。诊断期间另有批次 3 修复提交 `6cc2d726`；末次核对时磁盘二进制仍为 74a5daae，pendingDirectionBlocked 的上述数据读取路径也仍在。本轮使用历史源码 overlay 隔离，没有覆盖其他修复工作。

复现：[测试源码及重跑说明](/Users/zhangyun/workspace/nofx/docs/architecture/incident-2026-10-07/README.md)、[测试输出](/Users/zhangyun/workspace/nofx/docs/architecture/incident-2026-10-07/reproduction-output.txt)、[挂撤配对](/Users/zhangyun/workspace/nofx/docs/architecture/incident-2026-10-07/cancellation-pairs.tsv)。
