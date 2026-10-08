# 参数自动化迭代机制审查 · 2026-10-08

范围：系统中所有"根据结果自动调整参数 / 产出参数提案 / 为调参采集数据"的机制。只读代码与实盘库（`data/data.db`，只读查询），不改实盘配置。

**结论：真正按实盘表现自动改变交易行为的只有 NEGATIVE_EDGE 健康门，且有实锤 bug（A）。两个调参器被锁在"只出研究提案"，安全锁正确；影子拦截、质量分桶等校准数据集存在会误导调参的口径偏差。**

## 1. 机制清单

| # | 机制 | 调整对象 | 是否自动生效 | 结论 |
| --- | --- | --- | --- | --- |
| 1 | NEGATIVE_EDGE 健康门（`kernel/engine_prompt.go` `ResolveStrategyEdge`） | 滚动 PF<0.9 时开仓追加硬门 | **是，每周期** | A 已修；B 维持现状 |
| 2 | 锚点偏移 ATR 缩放（`kernel/anchor_offset.go`） | 限价偏移 / 留白阈值随 ATR(1h) | 是（按当时波动率，不从结果学习） | 正确 |
| 3 | Breakout 回测调参（`market/breakout/backtest.go` `TuneFromBacktest`） | strong/medium 阈值，每 7 天 | 否：只写 `data/breakout_backtest.json`，`ApplyParams` 无生产调用 | 安全；统计偏弱（J） |
| 4 | 做空权重调参器（`market/breakout/shorttuner.go`） | 9 个做空因子权重 | 否：只写提案；代码不会设置 `ShortWeightsValidated` | G 已修 |
| 5 | Gate 影子拦截（`trader/auto_trader_shadow.go`, `api/gate_shadow.go`） | min_rr / 共识 / 偏离等阈值的校准数据 | 否，仅观测 | C/D/E/F 已修 |
| 6 | AI 复盘提规则（`api/handler_review.go`） | 硬/软规则 | 人工批准后生效，加载失败 fail-closed | 流程正确（K） |
| 7 | 入场质量分桶（`store/entry_assessment.go` `BucketStats`） | 自评分→胜率校准 | 否，仅观测 | H 已修 |
| 8 | 策略版本效果（`store/strategy_version.go`） | 各配置版本实盘表现 | 否，仅展示 | I 已修 |

网格的自动调整 / 方向切换为规则状态机，不属参数迭代，未深审。

## 2. 已修复

### A. 全胜窗口被判 NEGATIVE_EDGE

`store/position_query.go` 仅在 `totalLoss > 0` 时计算 PF，无亏损单时 PF=0；`StrategyHealthEdge(0)` → `NEGATIVE_EDGE`，开仓硬门被打开，提示词同时写"表现: 需改进"。

修复（kernel）：新增 `ResolveStrategyEdge(*TradingStats)` 作为门与提示词的唯一判定入口——无数据 → POSITIVE；无亏损单（`PF==0 && AvgLoss==0`）→ 按 TotalPnL 判 POSITIVE / NO_EDGE；否则按 PF。提示词中 PF 显示为 `n/a(窗口内无亏损单)`，不再给出"需改进"。store 层 PF=0 语义保持不变（API / 前端仍在使用）。

### 最小样本（用户决定 2026-10-08）

窗口内平仓少于 `MinNegativeEdgeTrades = 20` 笔时不判 NEGATIVE_EDGE（降为 NO_EDGE），提示词标注样本不足。POSITIVE / NO_EDGE 判定不受样本数影响。

### C. 影子统计被 no_data 行污染

评估失败的行以 `outcome=no_data, exit=0` 落库，统计接口仍按 `(0-entry)/risk` 计 R：多单贡献数十 R 负值、空单同量级正值，`sum_r / avg_r` 失真。修复后仅 `tp_first / sl_first / timeout` 且 exit>0、risk>0 的行计入 R，`avg_r` 分母为有效 R 行数（新增 `r_samples` 字段）；48h R 只用 48h 出场价，不再回落到 8h。

### D/E. 影子评估：限价锚点须成交 + 严格时间窗

D：入场价常为限价锚点，旧评估默认拦截时刻即成交，价格不回踩直接到 TP 也记 tp_first，高估被拦交易胜率。E：纳入拦截前已开盘的 K 线、无 8h/48h 上限（走到评估当下含未收盘 K 线）、停机后固定根数拉不到拦截时刻。

修复：影子行记录 `entry_basis`；改用 15m K 线，只用拦截后开盘、已收盘且不超出 horizon 的连续 K 线，覆盖不全记 `no_data`；限价锚点须在实盘挂单有效期（`limitEntryLifetime`）内被触及，否则 `unfilled`（不计 R，统计单列）；成交那根只认止损；timeout 用窗口内最后收盘价；拉取根数按行龄计算（上限 1500 根 ≈15.6 天）。首版用 1h K 线会让前半小时内的拦截一律 unfilled，已改 15m 并加回归。

### H. 质量分桶按订单号精确关联

开仓评估新增 `order_id / order_tracked`：新数据按订单号 → 已平仓持仓 → 复盘日志精确关联（拆单合并、未成交无结果）；历史行保留近似匹配但限 2h 内、仅 AI 单、不得认领已精确关联的行。统计新增 `matched_exact / matched_legacy`。

### F. 影子统计归因与 48h 去重（2026-10-09）

新增 `by_code_sole(_48h)`：只统计被单一拦截码族拦下的行，回答"只放宽这一个门会放进来什么"；`by_code` 保留为任一拦截码视角（同族重复码只计一次）。48h 统计按币种+方向（跨交易员）只保留间隔 ≥48h 的行，`rows_dropped_overlap_48h` 显示剔除数。

### G. 做空调参器改为截面 rank IC（2026-10-09）

采样扩到前 30 名并记录排名；隔日块内同一币先合并为一个点，按币种算 Spearman 秩相关，跨块 IC 序列 ≥30 块且 |t|>3 才显著。提案附每因子诊断，注明标签为 24h 收盘价。仍只出研究提案。

### I. 版本效果口径（2026-10-09）

只统计 AI 单（`excluded_manual / excluded_unattributed` 显示排除数）；<20 笔标 `low_sample`；胜率附 Wilson 95% 区间；`crossed_version` 标出平仓已在下一版本生效后的单数。前端版本面板同步展示。

## 3. B：PF 混算手动单——维持现状（用户决定 2026-10-08）

`PositionStore.getStats`（NEGATIVE_EDGE 的 PF 来源）不按归属过滤，订单同步会导入整个账户，手动单一并计入。连亏熔断已按 `ai_managed` 过滤，PF 这条路没有。

实盘近 30 天已平仓（按复盘日志 `trade_journal.ai_managed` 归属，2026-10-08 查询）：

| 口径 | 笔数 | 盈利 | 亏损 | PF | 判定 |
| --- | --- | --- | --- | --- | --- |
| 现状（AI + 手动） | 240 | 174.8 | 177.8 | 0.98 | NO_EDGE，门不触发 |
| 仅 AI | 168 | 88.2 | 123.2 | 0.72 | NEGATIVE_EDGE |
| 仅手动 | 72 | 86.6 | 54.6 | 1.59 | — |

手动单盈利掩盖了 AI 的负期望；改为仅 AI 口径会立即触发负期望门、显著收紧开仓。用户决定暂维持混算。

若日后切换：归属口径采用**复盘日志归属**（按开仓决策匹配，已回填历史，窗口完整）。持仓表 `trader_positions.ai_managed` 在 09-29 前全部为 0，用它过滤会把早期 AI 单当成手动单剔除；两套归属在 09-29 后有 13 笔不一致，切换前应先核对。

## 4. 待修（会误导调参的方法偏差）

- **J Breakout 回测**：注释"每 UTC 天重建关键位"实为每小时（无害）；stride 3 根 15m 配 24h 标签，样本高度重叠，40/15 样本门槛并非独立样本；验证只比均值无显著性。只出提案，风险低。
- **K AI 规则**：">=3 笔支撑"只在提示词中，代码不校验；规则无过期、无事后效果追踪；输入仅取已复盘单，有选择偏差。

## 5. 正确之处

两个调参器的 `LIVE PUBLICATION BLOCKED` 锁；回测关键位按时点重建、只用已收盘 K 线；训练集剔除标签跨入测试期的样本，候选值须在测试集上胜过现行值；回测样本不足的退避与原子写入；规则加载失败 fail-closed；盈利锁 1R 锚定初始止损。
