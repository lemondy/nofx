# 审查报告：大模型 Prompt、决策 schema 与解析、AI 与程序的职责边界

范围：`kernel/engine_prompt.go, engine.go, engine_analysis.go, engine_position.go, signal_layer.go, schema.go, candidate_verdicts.go, rule_engine.go, rule_verify.go`、`mcp/client.go + provider/`、`trader/auto_trader_loop.go, auto_trader_decision.go, auto_trader_cot.go`，以及为核对执行侧而读的 `auto_trader_orders.go / auto_trader_risk.go / auto_trader_rules.go / auto_trader_shadow.go`。

方法说明：
- 只读审查，仓库文件未改动。临时测试用 `go test -overlay` 注入，放在 scratchpad。
- 我中途误用 `GOFLAGS=-mod=mod`，go.mod 被自动改写，已 `git checkout -- go.mod` 还原，`git status` 为空。
- `go vet ./kernel ./mcp/...` 无告警，`go test ./kernel ./mcp/...` 全绿。
- 真实 prompt 渲染：没有另写渲染程序，直接取生产库 `~/workspace/nofx/data/data.db`（`immutable=1` 只读打开）里 `decision_records` 保存的完整 system/user/response。取 id=17195（2026-10-27 23:27 UTC，deepseek trader），并对 2026-10-20 以来约 3100 条 LLM 调用记录做了统计。

结论先行：
- 本系统的"AI 与程序边界"设计得相当扎实。执行侧对模型输出有多层服务端校验：SL/TP 吸附到程序计划、仓位下压、候选池校验、硬门、风险预算、手动仓免疫。
- 2026-10-01 以来 867 条开仓决策中，没有一条违反 `allowed=false`，说明模型实际上只是在程序预筛过的选择题里挑选。
- 主要问题集中在四处：
  1. 解析层脆弱，且有一处会把草稿当最终决策。
  2. 429 不重试，导致单日 24% 周期失败。
  3. 喂给模型的两个数值口径有偏差（EMA50 未收敛、"4h 涨幅"窗口随相位漂移）。
  4. 缺少能回答"LLM 到底有没有增量价值"的离线对照。

---

## A. 确认的 Bug

### A1 | P1 | 429 / 500 不在可重试列表，限流直接丢整个周期
- 位置：`mcp/client.go:25-39`（`retryableErrors`）、`mcp/client.go:436-447`（`IsRetryableError`）、`mcp/client.go:163-214`（重试循环）
- 证据：
```go
retryableErrors = []string{"EOF","timeout",...,"status 502","status 503","status 520","status 524"}
```
  没有 `status 429`、`status 500`。
- 触发场景：多个 trader 共用同一 GLM key。生产库 2026-10-27 全天 1326 个周期里有 323 个失败，错误是 `API returned error (status 429): Too many pending requests`，没有 "still failed after N retries" 前缀，即首次就放弃。
- 后果：连续 3 次失败触发 `safeMode`（`auto_trader_loop.go:222`），禁止新开仓。
- 另外 `waitForRetry` 是线性 `2s×attempt`，没有抖动，也不读 `Retry-After`。
- 修复：429/500/529 纳入重试；有 `Retry-After` 就遵守，否则指数退避加抖动。多 trader 共享同一 key 时在进程内加一个 provider 级信号量，限制并发在途请求数。

### A2 | P1 | 决策 JSON 解析是"全有或全无"，一个类型偏差丢掉整个周期
- 位置：`kernel/engine_analysis.go:550-650`（`extractDecisions`）、`:658-680`（`fixMissingQuotes`）、`kernel/engine.go:498-`（`Decision` 里 `Leverage int / Confidence int`）
- 我用 overlay 测试复现（`json.Unmarshal` 一次失败，整个数组作废）：

| 输入 | 结果 |
|---|---|
| `"leverage":3.0` | `cannot unmarshal number 3.0 into ... int`，n=0 |
| `"confidence":85.5` | 同上 |
| `"take_profit_pct":+1.1` | `invalid character '+'` |
| `"leverage":"3"` | `cannot unmarshal string` |
| `"NaN"` | 失败 |
| reasoning 字符串里含中文引号 `突破“关键位”失败` | **失败**（`fixMissingQuotes` 对全文无差别把 `“”` 换成 `"`，字符串内部被截断） |

- 生产证据：
  - 2026-08 至 09，29 个周期死于 `take_profit_pct: +1.1`。
  - 10-26 一例死于重复 `<decision>` 块。
  - 另有 11 例是 `decision_stage "WAIT"` 非法枚举，整批作废。
  - 10-20 以来解析失败只有 1/3100，说明现行 prompt 下概率低，但一旦发生，代价是整个周期的持仓管理（adjust/close/partial）全丢。
- 后果：解析失败按"AI 失败"计数，3 次进安全模式。
- 修复，按收益排序：
  1. 把数组拆成 `[]json.RawMessage`，逐对象宽松解码（数值字段用自定义 `flexInt/flexFloat`，接受 `3.0`、`"3"`、`+1.1`）；单个对象失败只降级该对象为 wait/hold。
  2. 只在 JSON 结构位置（字符串字面量之外）做全角标点修复，字符串内部不动。
  3. 解析失败时用错误信息再请求一次"仅重发 `<decision>`"，比丢周期便宜得多。
  4. 中期改用结构化输出（OpenAI/GLM/DeepSeek 的 `response_format=json_schema` 或 tool call，Claude 的 tool_use），可去掉大部分正则修补。

### A3 | P1 | 信号块里的 `ema_fast/ema_slow/trend` 在 59 根 K 线上算 EMA50，未收敛，且与同块 `regime_line.ema50` 不一致
- 位置：`kernel/signal_layer.go:2573-2582`（`ExportCalculateEMA(kb, fastP/slowP)`）、`market/data_indicators.go:6-23`（SMA 种子 EMA）、`store/strategy.go:27`（`MinKlineCount = 60`）、`kernel/regime_line.go:23-38`（用数据层更长序列的 EMA50）
- 证据（真实数据）：
  - 样例块 `closed_bars_used:59`，EMA50 由前 50 根 SMA 作种子，只递推 9 步。
  - 在 246 个真实 Structured Signal 块里比较同一 TF 的 `regime_line.ema50` 与 `timeframes[tf].ema_slow`：中位差 0.24%，p90 0.63%，最大 1.0%；"收盘价在线上/下"判断相反的有 10/246（4.1%）。
  - 合成 GBM 模拟（1h 波动 0.6%）：59 根种子的 EMA50 相对长历史版本，中位误差 0.29%，p90 0.66%，约 0.6 个小时 ATR；收盘价相对 EMA50 的符号约 5% 的 bar 判错；`EMA20>EMA50` 排列判错 7.5%。
- 触发场景：`trend`、`ema_slow`、`NEG_EDGE_TREND_MISALIGNED`（trend_tf+regime_tf 的 EMA 方向）、`market_regime` 的 EMA 排列（`kernel/market_regime.go:249-251`）都吃这个值。同一块里出现两个不同的 EMA50，模型和硬门读到的结论可能互相矛盾。
- 修复：指标计算窗口与展示窗口解耦——拉取 ≥200 根（EMA50 需约 150 根以上收敛）计算，只展示最近 N 根；`signal_layer` 直接复用数据层已有的 `EMA50Values`，避免两套口径。

### A4 | P1 | `CHASE_MOVE4H` 与 BTC 行 "4h" 的窗口随相位在 0–4h 间漂移
- 位置：`market/data.go:234-241`、`kernel/signal_layer.go:154-157,1769-1778`、`kernel/engine_prompt.go:526-529`
- 证据：
```go
// 4-hour price change = price from 1 4-hour K-line ago
price4hAgo := klines4h[len(klines4h)-2].Close   // 最后一根是形成中的 4h K，[-2] = 上一个 4h 边界的收盘
priceChange4h = (currentPrice - price4hAgo) / price4hAgo * 100
```
- 触发场景：币在 03:00–04:00 UTC 暴涨 +12%，04:05 时 `[-2]` 是 04:00 收盘，`PriceChange4h≈0.4%`，`CHASE_MOVE4H_x_GT_6` 不触发，而真实 4h 涨幅是 12%。同一个值还以 "BTC (1h:…, 4h:…)" 的名义直接喂给模型，prompt 里 `pump_guard` 特意澄清了自己"不是最近 4 小时"，这里却没澄清。
- 严重度：其余追高门（`CHASE_EMA20_1H`、`EMA20_STRETCH`）部分兜底，所以是 P1 不是 P0。
- 修复：用 3m/5m 序列取 `now-4h` 的价格（与 1h 的 `klines3m[len-21]` 同法），或把字段重命名为 "since_4h_boundary" 并另算滚动 4h。

### A5 | P1（低频）| 多个 `<decision>` 块时取第一个完整块，草稿会被当最终决策执行
- 位置：`kernel/engine_analysis.go:557-566`（`reDecisionTag` 懒匹配取第一个）、`:586-` 兜底路径取第一个数组
- 复现：
```
<reasoning>draft: <decision>[{"symbol":"ETHUSDT","action":"close_long"}]</decision> hmm no...</reasoning>
<decision>[{"symbol":"ETHUSDT","action":"hold"...}]</decision>
```
  → 解析出 `ETHUSDT close_long`（最终块被忽略）。无标签兜底同理，会取推理里示例数组的第一个（测试得到 `X close_long`）。
- 生产频率：2253 个含 `<decision>` 的记录里 15 个出现多块，1 个因此解析失败；未见执行错误的证据，所以标"低频"。
- 与 `mcp/client.go:324` 及 `ParseSSEStream:845` 的 `reasoning_content` 兜底叠加：content 为空时，把思考链（可能含草稿块）当作答案解析。2026-10-20 的补丁只处理了"未闭合"。
- 修复：取**最后**一个完整块；兜底路径禁止从 reasoning 文本里解析可执行动作（只允许 `content`）。

### A6 | P1（潜伏，未触发）| 市价默认策略下 `EXTENDED_PUMP_UNCONFIRMED` 在执行侧没有任何拦截
- 位置：`trader/auto_trader_risk.go:2552-2568`（`absoluteBanCode` 不含该码），`:2746-2760`（市价分支只拦 absolute + anti-chase），`trader/auto_trader_orders.go:136`（`marketExceptionGate` 在 `!LimitEntryEnabled` 时 `return d, nil`），`trader/auto_trader_pending_risk.go:269-285`
- 证据：`absoluteBanCode` 的注释写"EXTENDED_PUMP_UNCONFIRMED → marketExceptionGate"，但这条路径只在限价默认策略下才有效。生产 system prompt 含"默认市价入场"（`marketExceptionGate` 提前返回），同时含暴涨延伸门文案。
- 触发场景：模型对 `long.failed=["EXTENDED_PUMP_UNCONFIRMED"]` 的币输出 `open_long`，我沿 `applyHardRiskGates → executeOpenLongWithRecord → validateOpenRisk` 都没找到对该码的镜像检查。目前只靠 prompt 的"allowed=false 必须 wait"。
- 现状：2026-10-01 以来 867 条开仓决策中 0 条违反 `allowed=false`，所以是纵深防御缺口，不是已发生事故。
- 修复：市价开仓统一规则——`gs` 的方向 `allowed=false` 且 `failed` 含任何未在显式白名单中的码，一律拒绝；白名单只放确有镜像检查的 `MICRO_TREND_* / RR_MAX_* / MIN_SIZE_DEAD_ZONE / STOP_PLAN_*`。

### A7 | P2 | 模型 contract 删掉了 `reasoning`，但下游仍消费 `Decision.Reasoning`，导致复盘与告警为空
- 位置：`kernel/engine_prompt.go:~150-160`（contract 只要求 decision_summary，且"不要重复 JSON 中的理由"）、`kernel/engine.go:518`、`trader/auto_trader_loop.go:400`、`trader/auto_trader_risk.go:2682,2836,2858,2907,2924,2955`、`trader/auto_trader_orders.go:170`
- 证据：2026-10-20 以来 3108 条回复的 31770 个决策对象字段统计里没有 `reasoning`；`trade_journal.entry_reasoning` 近 12 天（10-16 至 10-27）每天 0 条非空；所有 Telegram 拦截告警里的 `<i>%s</i>` 为空。
- 后果：复盘闭环（rule 抽取、journal）缺少"为什么开仓"的数据。
- 修复：开仓决策要求输出一句 `entry_rationale`（≤120 字）；或把 `<reasoning>` 的对应行按 symbol 切出来回填。

### A8 | P2 | 流式路径忽略 `finish_reason`，被 `max_tokens` 截断时静默通过
- 位置：`mcp/client.go:783-857`（`ParseSSEStream` 解析了 `FinishReason` 却不用）、`mcp/provider/claude.go:237` 起（无 `stop_reason` 检查）；非流式 `client.go:333-338` 有日志
- 默认 `AI_STREAM_DECISIONS=1`，所以生产走流式。叠加 `salvageTruncatedDecisionArray`，截断后只保留已完成的对象，尾部的持仓管理决策静默丢失。
- 修复：流式路径也记录 `finish_reason=length` 并计数告警；截断时若有持仓，补一次只针对持仓的短调用。

### A9 | P2 | Prompt 与代码对"0"的语义相反
- 位置：`kernel/engine_prompt.go:243`（"price/stop_loss/take_profit 为0表示不可交易或被抑制，绝不是占位符"）vs `kernel/engine_position.go:51-58,488-496,538-545`（0 被当占位符，用程序计划值填充）；`engine_analysis.go:31`（`?` 被替换成 null→0）
- 后果：模型用 0 表达"不可交易"的意图会被程序改写成可执行价格；后续是否还会被拦截取决于硬门，所以是 P2。
- 修复：prompt 只保留一种语义——"未知就省略字段"；代码把"0 且 action 为 open_*"视为模型表达了弃权，转 wait，而不是填充。

### A10 | P2 | Telegram CoT 在限价→市价改写后误报"被闸门过滤"
- 位置：`trader/auto_trader_cot.go:44-60`（以 `Symbol|Action` 为键匹配）、`trader/auto_trader_loop.go:303`（`forceMarketEntries` 改写 action）、`trader/auto_trader_orders.go:24-46`
- `proposed` 里是原始 `open_*_limit`，`executed` 里是改写后的 `open_*`，键不匹配，已成交的开仓也显示"被闸门过滤，未执行"。当前生产策略是市价默认，模型不输出 limit，暂不触发。
- 修复：改写时保留 `OrigAction` 并用它做键。

---

## B. 疑似问题（需进一步验证）

- **B1 温度参数**：`mcp/client.go:252,898` 对所有 OpenAI 兼容 provider 固定发 `temperature=0.5`，Claude 分支不发（默认 1.0），各 provider 不一致。需验证 OpenAI `gpt-5.x` 推理模型是否拒绝非默认 temperature（会 400，然后流式 4xx 回退非流式仍 400）。当前生产模型是 GLM/DeepSeek，未复现。另外决策调用没有 seed、没有记录模型版本，无法精确复现。
- **B2 共享状态被提前改写**：`resolveTakeProfitOptions`（`engine_position.go:557-590`）直接修改共享的 `GateState.LongTakeProfit/ShortTakeProfit`，且发生在 `validateDecision` 之前。注释里"validateDecision already resets those"与实际顺序相反。决策随后被拒时，污染的状态仍被执行侧（`cycleGateStates`）读取。需验证是否影响 `checkRR` 的"plan-parity 豁免"。
- **B3 BTC 头行含形成中的 K 线**：`market/data.go:213-224` 先用实时价修补形成中的 3m K，再算 `CurrentMACD / CurrentRSI7`，而 prompt 声称"closed candles only"。BTC 头行指标因此含未收盘数据。仅是背景信息，影响小。
- **B4 批内无一致性校验**：`validateDecision` 逐个独立校验，同一 symbol 重复、`open_long`+`close_long` 同时出现都能通过。执行侧靠"已有同向仓位"和排序（先平后开）兜底，没有显式的批内去重。
- **B5 symbol 严格相等**：`engine_position.go:264,297` 里用 `d.Symbol == "BTCUSDT" || "ETHUSDT"` 选杠杆/仓位上限，小写或无后缀的 symbol 会落入山寨档（更保守）。`validateDecision` 也不规范化 symbol 大小写。
- **B6 复盘规则的统计方法**：`kernel/rule_verify.go:24-33,129-163`，"supported"仅要求 matched≥3 且净亏损低于基准。该规则本来就是从这些亏损单里抽出来的，验证集与生成集重合（样本内），且无显著性检验与多重比较校正。`trading_rules` 表目前为空，属休眠风险。同文件相关：`rule_engine.go:355-372`（`isSymbolLikeTag`）排除词表只有 11 个，`breakout/rsi/reversal` 等普通标签会被当作 symbol 标签，导致该 soft rule 对所有币都不匹配。
- **B7 注入面**：外部新闻标题已在 `renderSignalBlock` 剥离（`engine_prompt.go:2171-2185`），做法正确。剩余来源：`RulesText`（LLM 复盘生成并以 "MUST obey" 注入，`rule_engine.go:374-`）、第三方字符串（情绪标签、CFTC `ReportDate`、AI500 symbol 名）。风险低，但建议对进入 prompt 的外部字符串统一做字符集/长度白名单。另外 `store/strategy.go:1189` 与 `provider/nofxos/client.go:19` 硬编码了 NofxOS key（`cm_…`），虽注释称已弃用，仍建议移除。
- **B8 潜在矛盾**：`VolTargetEnabled` 文案（`engine_prompt.go:~420`）邀请模型"评估加仓"，但执行侧对同向已有仓位一律拒绝（`auto_trader_orders.go:213-217`）。当前生产配置未启用该段，所以只是潜伏矛盾。`schema.go` 里 "MarginUsage 安全值<30% 危险值>70%" 与策略上限 90% 也不一致。
- **B9 `macd_hist` 命名**：实为 MACD line/price（`signal_layer.go:2587-2594`），图例已声明（`engine_prompt.go:2129`），属命名问题，建议改名 `macd_line_pct`。

---

## C. 可复用的成熟方案

| 现有自写 | 建议 | 判断 |
|---|---|---|
| `extractDecisions` + 6 个正则 + `fixMissingQuotes` + `salvage…`（`engine_analysis.go`，约 300 行） | 各 provider 的结构化输出（json_schema / tool_use）+ 逐元素宽松解码；仍需保留一个很薄的兜底 | **值得换**。收益是消灭 A2/A5 这类整类问题；成本是各 provider 能力不一，推理模型的 tool call 支持要实测 |
| EMA/RSI/ATR/BOLL/MACD（`market/data_indicators.go`）、StochRSI | 不必引入 `go-talib`/`cinar/indicator` | **自写合理**：公式正确，核心问题是预热长度（A3）。建议用 TA-Lib 的固定样例数据做黄金值单测，成本很低 |
| 重试/退避（`mcp/client.go`） | `cenkalti/backoff` 之类 | 收益有限，直接补全可重试集合与抖动即可 |
| 回测 | 仓库约定已用 Backtrader（`scripts/backtest`） | **保持**，LLM vs 规则基线的离线对照也应走这条路径，不要另写模拟器 |
| 因子/IC 评估 | `alphalens`/`quantstats`（Python 研究侧） | 只用于研究脚本，不进 Go 热路径 |
| 提示缓存 | system prompt 已做到字节稳定（`engine_prompt.go:~29`，实盘权益放在 user prompt） | **做法正确**，OpenAI 自动前缀缓存与 Anthropic `cache_control` 均可利用 |
| 持久化 prompt | `decision_records` 每行存完整 system+input | 建议 system prompt 按哈希去重存一份（见 D8） |

自写合理的地方：signal layer 的收盘 K 线结算（`computeTFSignal:2452-2480` 丢弃形成中的 bar，`ohlcv_last_closed_utc` 与实际对得上）；`stop_plan`/`rr_scan`/`tp_options` 预计算——这是整个设计的亮点，把算术和价格全部程序化，模型只做选择。

---

## D. 优化建议（按预期收益排序）

**D1 建立"LLM vs 规则基线"的反事实账本（最高收益）**
- 现状：
  - `gate_shadow_blocks` 只记录被拦截的方向（`auto_trader_shadow.go:64` `if d.allowed … continue`），无法回答 LLM 在"放行集合"里挑选有没有价值。
  - 已有数据：1882 个"至少一个方向被放行"的候选实例里，LLM 只开了 171 个（9.1%），其余 90.9% 被 LLM 否决，但这些被否决的放行信号没有后验结果。
  - 被拦截方向的 8h 影子结果：tp_first 498、sl_first 1437、timeout 3478，计划 RR 约 1.6–2.0。按"已判定"口径，止盈优先只占 25.7%，对应负期望，旁证门控有价值。
- 做法：
  1. 对 `allowed=true` 的方向同样写影子记录，带 `llm_action` 字段。
  2. 基线：放行集合里按 `directional_score` 取前 N 个、全部采用程序的 stop_plan/tp_option①，用 Backtrader 重放，与实盘 LLM 子集做配对比较；用 bootstrap 给出置信区间，注意同一时段多币相关性。
  3. 同时落盘所有放行方向的 entry/SL/TP，现在只存在于 `input_prompt` 文本里。
- 参照：`confidence` 对结果没有可用信息。`trade_journal` 里 `ai_managed=1` 共 388 单，其中 369 单 confidence 落在 70–79，80+ 仅 15 单。各档平均净盈亏：70–74 为 −0.64，75–79 为 −0.66，80+ 为 −0.12（n=15）。最小阈值 70 基本是空操作。不是显著性检验，但至少说明不该依据 confidence 做仓位或例外判定（当前 `MarketExceptionMinScore=80` 就用了它）。

**D2 让 LLM 只回答它必须回答的问题**
- 样例（id 17195）：8 个候选里 7 个双向被拦，5 个手动仓，模型输出 11 个对象、约 3k 字符、耗时 81s，其中绝大多数是复制阻断码。
- 做法：程序对"双向硬拦截"候选和"手动仓 hold"直接合成 wait/hold，LLM 只处理有放行方向的候选和 `Managed` 持仓。预期输出 token 与时延明显下降，也消除"模型把阻断码抄错"的风险。
- 账户级预门：目前 regime-skip 只看候选方向和平仓锁（`engine_analysis.go:150-240`），不看账户级阻断。统计显示 2026-10-20 以来 3113 次 LLM 调用中，有 112 次（3.6%）发生在 `MarginUsage≥90%` 时，全部 0 开仓，共消耗约 15,105 秒 AI 时间。应把保证金、日内熔断、账户回撤、最大持仓、风险敞口上限做成程序字段，并纳入 regime-skip 判定。

**D3 仓位交给程序**
- 现状：prompt 让模型按 `equity×3%÷(止损%+0.2%)` 自己算 `position_size_usd`，执行侧 `clampSizeToRisk`（`auto_trader_risk.go:954-973`）只向下限制。
- 核对：对 171 个开仓决策，用 `equity×3.0÷(|entry−stop_plan|/entry×100+0.20)` 对比模型给出的值——131 个（77%）与公式值相差 2% 以内，40 个（23%）低于公式值，p10 为公式值的 42%，最小仅 1%，没有超出。
- 做法：信号块新增 `size_plan_usd`（已综合风险预算、仓位上限、保证金余量、净方向风险上限），模型只选"满/半/放弃"。
- 注意：低于公式的部分我无法区分是合理（其他上限）还是模型出错，需要用 `size_plan_usd` 复算后再看。

**D4 修指标预热和窗口口径**：见 A3、A4。成本低，且直接影响硬门输入。

**D5 时延与"陈旧快照"**
- AI 调用平均约 90 秒（10-20 以来逐日均值 24–144 秒），加上串行抓取（`fetchMarketDataWithStrategy` 逐币串行，注释自述约 1 分钟），决策基于 2–3 分钟前的快照。
- 实证：开仓失败里 "net RR below … at entry" 12 次 + "invalid fill (actual fill net RR below)" 9 次，均为入场时 RR 相对快照漂移。
- 做法：并发抓取（有界并发）；开仓前用实时价按同一 stop_plan/tp 重算净 RR，不足则降级为 wait，而不是下单后再平仓。

**D6 Token 与信息密度**（实测，样例 id 17195）
- system 16,884 字符，user 18,716 字符，response 3,066 字符。
- 估算：仓库估算器（非 ASCII 按 2 token）给 15.7k + 8.1k ≈ 23.8k，偏保守；我按 CJK≈0.9 token/字、ASCII≈3.3 字符/token 估算约 15k 输入 + 约 1.1k 输出，量级为每次调用 1.5 万 token。10-20 以来 3113 次 LLM 调用合计约 4.7 千万输入 token（约 600 万/天，其中 system 部分可走前缀缓存）。
- 密度：user prompt 中一个完整候选的 JSON 占 10,716 字符（57%），其中 `ohlcv` 3,407（32%）、`timeframes` 2,832、`hard_entry_gate` 1,602。图例已声明 ohlcv "只用于软证据，不得覆盖程序结论"；样例响应没有引用任何原始 K 线。`EnableRawKlines` 做一次 A/B 关闭几乎是零成本的降本项。
- 另外：每个手动仓重复一段约 430 字符的"只能 hold"样板，5 个就是 2.1k 字符，约占 user 的 11%；system prompt 的 Strategy Parameters 区约 7k 字符，大部分是模型无法改变的"程序强制"说明，可裁剪为"只列当前生效且影响选择的项"。
- ohlcv 数组的列序（o,h,l,c,v）和时间序（旧→新）图例没有写明，建议补一行。

**D7 输出字段瘦身**：`entry_quality`/`management_quality`/`management_flags`/`next_trigger` 对 wait/hold 每个对象都要生成，当前 31770 个对象里 23423 个是 wait，只服务于数据集标注，没有证据说明它们预测结果。要么证明有用（用 D1 的账本做校准），要么由程序基于硬门状态回填。

**D8 存储与可观测**：`decision_records` 每行平均约 12.8k + 18.8k 字符的 prompt，17002 行约 540MB，占 926MB 数据库的大半。system prompt 按 `config_hash` 去重存一份即可。再给每次调用记录模型名、温度、token 用量、`finish_reason`、解析路径（tag/fence/salvage/fallback），A2/A5/A8 这类问题才能被指标发现。

**D9 A/B 与影子评估**：因为 prompt 和回复都已落库，可以做**离线回放**——把历史 `system_prompt + input_prompt` 喂给候选模型/新 prompt，对比动作一致率与"放行集合内的选择"，不需要实盘。结果好坏仍要靠 D1 的账本，一致率不能代替收益。

---

## E. 覆盖与未覆盖

已读（全文或关键段）：
- `kernel/engine_prompt.go`（system 全文、user 全文、`strategyParamsText`、`signalBlockLegend`、`renderSignalBlock`）
- `kernel/engine_analysis.go`（全部）、`kernel/engine_position.go`（全部）、`kernel/engine.go`（Context/GateState/Decision 类型及 `GetFullDecisionWithStrategy` 链路）
- `kernel/signal_layer.go`（EMA/MACD/RSI 计算段、CHASE_MOVE4H 段、字段定义）、`kernel/regime_line.go`、`kernel/market_regime.go` 的 EMA 段
- `kernel/candidate_verdicts.go`、`kernel/rule_engine.go`、`kernel/rule_verify.go`
- `mcp/client.go`、`mcp/config.go`、`mcp/context_guard.go`、`mcp/provider/openai.go / claude.go` 的关键段
- `trader/auto_trader_loop.go`（`runCycleWithDecision` 主体、`sortDecisionsByPriority`）、`auto_trader_cot.go`、`auto_trader_decision.go` 函数清单
- 为核对执行侧而读：`auto_trader_orders.go`（`executeDecisionWithRecord`、`marketExceptionGate`、`executeOpenLong/Short` 前半）、`auto_trader_risk.go`（`clampSizeToRisk`、`marginBudgetBlocksOpen`、`executeAdjustStopLossWithRecord`、`applyHardRiskGates` 前半、`absoluteBanCode`）、`auto_trader_rules.go`、`auto_trader_shadow.go`

数据与实测：
- 生产库 `decision_records / trade_journal / entry_assessments / gate_shadow_blocks / trading_rules` 的只读查询。
- overlay 解析测试：14 个输入。
- EMA 预热的蒙特卡洛：5000 根合成序列。
- `go vet ./kernel ./mcp/...`、`go test ./kernel ./mcp/...` 通过。

未覆盖或仅浅读：
- `kernel/schema.go` 仅看了结构与引用处。
- `docs/prompt-guide.zh-CN.md` 未逐段核对，只作背景。
- `kernel/signal_layer.go` 其余约 2500 行（硬门各码的具体阈值逻辑、`rr_scan`、`tp_options` 生成）。
- `kernel/engine_data_binance.go`、`engine.go` 的候选源扫描部分、`kernel/stockengine`（美股引擎，响应格式与加密不同，样本里出现的 `{"reasoning":…}` 包装属于它）。
- `trader/auto_trader_loop.go` 的 `buildTradingContext` 后半、`auto_trader_pending*.go`、`auto_trader_risk.go` 后半的各类闸门。
- `mcp/provider/` 中 deepseek/qwen/glm/kimi/minimax/grok/gemini 的差异细节（仅确认它们没有覆盖 `BuildMCPRequestBody`，即温度等参数同构）。
- 所有数字均来自 `immutable=1` 读取的快照（不含 WAL 最新数据，约少于 1 天），统计性结论（confidence 与盈亏、被放行信号的接受率）均为描述性统计，没有做显著性检验。
