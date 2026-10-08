# 参数自动化迭代机制审查 · 2026-10-08

范围：系统中所有"根据结果自动调整参数 / 产出参数提案 / 为调参采集数据"的机制。只读代码与实盘库（`data/data.db`，只读查询），不改实盘配置。

**结论：真正按实盘表现自动改变交易行为的只有 NEGATIVE_EDGE 健康门，且有实锤 bug（A）。两个调参器被锁在"只出研究提案"，安全锁正确；影子拦截、质量分桶等校准数据集存在会误导调参的口径偏差。**

## 1. 机制清单

| # | 机制 | 调整对象 | 是否自动生效 | 结论 |
| --- | --- | --- | --- | --- |
| 1 | NEGATIVE_EDGE 健康门（`kernel/engine_prompt.go` `ResolveStrategyEdge`） | 滚动 PF<0.9 时开仓追加硬门 | **是，每周期** | A 已修；B 维持现状 |
| 2 | 锚点偏移 ATR 缩放（`kernel/anchor_offset.go`） | 限价偏移 / 留白阈值随 ATR(1h) | 是（按当时波动率，不从结果学习） | 正确 |
| 3 | Breakout 回测调参（`market/breakout/backtest.go` `TuneFromBacktest`） | strong/medium 阈值，每 7 天 | 否：只写 `data/breakout_backtest.json`，`ApplyParams` 无生产调用 | 安全；统计偏弱（J） |
| 4 | 做空权重调参器（`market/breakout/shorttuner.go`） | 9 个做空因子权重 | 否：只写提案；代码不会设置 `ShortWeightsValidated` | 方法缺陷（G） |
| 5 | Gate 影子拦截（`trader/auto_trader_shadow.go`, `api/gate_shadow.go`） | min_rr / 共识 / 偏离等阈值的校准数据 | 否，仅观测 | C 已修；D/E/F 待修 |
| 6 | AI 复盘提规则（`api/handler_review.go`） | 硬/软规则 | 人工批准后生效，加载失败 fail-closed | 流程正确（K） |
| 7 | 入场质量分桶（`store/entry_assessment.go` `BucketStats`） | 自评分→胜率校准 | 否，仅观测 | 关联口径错（H） |
| 8 | 策略版本效果（`store/strategy_version.go`） | 各配置版本实盘表现 | 否，仅展示 | 口径混杂（I） |

网格的自动调整 / 方向切换为规则状态机，不属参数迭代，未深审。

## 2. 已修复

### A. 全胜窗口被判 NEGATIVE_EDGE

`store/position_query.go` 仅在 `totalLoss > 0` 时计算 PF，无亏损单时 PF=0；`StrategyHealthEdge(0)` → `NEGATIVE_EDGE`，开仓硬门被打开，提示词同时写"表现: 需改进"。

修复（kernel）：新增 `ResolveStrategyEdge(*TradingStats)` 作为门与提示词的唯一判定入口——无数据 → POSITIVE；无亏损单（`PF==0 && AvgLoss==0`）→ 按 TotalPnL 判 POSITIVE / NO_EDGE；否则按 PF。提示词中 PF 显示为 `n/a(窗口内无亏损单)`，不再给出"需改进"。store 层 PF=0 语义保持不变（API / 前端仍在使用）。

### 最小样本（用户决定 2026-10-08）

窗口内平仓少于 `MinNegativeEdgeTrades = 20` 笔时不判 NEGATIVE_EDGE（降为 NO_EDGE），提示词标注样本不足。POSITIVE / NO_EDGE 判定不受样本数影响。

### C. 影子统计被 no_data 行污染

评估失败的行以 `outcome=no_data, exit=0` 落库，统计接口仍按 `(0-entry)/risk` 计 R：多单贡献数十 R 负值、空单同量级正值，`sum_r / avg_r` 失真。修复后仅 `tp_first / sl_first / timeout` 且 exit>0、risk>0 的行计入 R，`avg_r` 分母为有效 R 行数（新增 `r_samples` 字段）；48h R 只用 48h 出场价，不再回落到 8h。

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

- **D 影子默认立即成交**：`EntryPrice` 常为限价锚点（`kernel/signal_layer.go` `limit_anchor`），评估却假设挂单即成交。价格不回踩直接到 TP 的情形被记为 tp_first，系统性高估被拦交易胜率，偏向"放松门槛"。应先判定锚点是否被触及，未触及记 `unfilled`。
- **E 影子时间窗**：拦截前已开盘的 1h K 线被计入（`barEnd.Before(start)` 只排除已收盘的）；无 8h/48h 上限，一直走到评估当下；停机超过 32h 后，拉到的 K 线已不含拦截时刻。
- **F 影子归因**：多码行同时计入每个码；同一 key 约每 8h 新建一行，48h 窗口重叠约 6 倍，样本数虚高。
- **G 做空调参器**：先按天求均值再相关，测的是大盘择时而非截面选币；仅 top-10 采样带来筛选偏差；标签为 24h 收盘而非 SL/TP 路径。应改为日内截面 rank IC 序列再检验。z>3 + ≥30 隔日块使其几乎不触发，且只出提案，风险有限。
- **H 质量分桶**：开仓评估认领其后第一笔同币同向成交，无时间上限；限价未成交或中间有手动单都会错配。应改用 `entry_attributions` 精确关联。
- **I 版本效果**：未区分 AI / 手动单，无样本量或置信区间提示；按开仓时间归属版本，中途改出场参数时结果算在旧版本上。
- **J Breakout 回测**：注释"每 UTC 天重建关键位"实为每小时（无害）；stride 3 根 15m 配 24h 标签，样本高度重叠，40/15 样本门槛并非独立样本；验证只比均值无显著性。只出提案，风险低。
- **K AI 规则**：">=3 笔支撑"只在提示词中，代码不校验；规则无过期、无事后效果追踪；输入仅取已复盘单，有选择偏差。

## 5. 正确之处

两个调参器的 `LIVE PUBLICATION BLOCKED` 锁；回测关键位按时点重建、只用已收盘 K 线；训练集剔除标签跨入测试期的样本，候选值须在测试集上胜过现行值；回测样本不足的退避与原子写入；规则加载失败 fail-closed；盈利锁 1R 锚定初始止损。
