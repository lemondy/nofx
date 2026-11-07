# 2026-11-07 第七轮全链审查报告

**审查基线**：`HEAD 90ab394b`（dev）。**实盘进程 PID 73160 = f2ccd213**（PR #83 tip，11-06 周五 0 点启动，vcs 元数据验证）——落后 HEAD 6 个 PR，均为研究/文档类提交（fq_us volscale 配对、风格中性化、not_listed 修复），无执行代码缺口。

**范围**：自上轮全审（c8c59876，10-25）以来的 194 个提交，重点是五块新执行代码：factor_quant 程序化引擎（fdb5928c 系）、美股做空模拟（9d632bc1 系）、paper 引擎资金费/并发（6c785e38/0fd2d904/aedf9328）、crypto AI 周期锁收窄 + STATE_CHANGED（aedb30e3）、限价入场先落库（36c86136）。

**方法**：factor_quant 执行链由子代理独立深审（通读 5 文件+6 测试、go vet、只读查库核对 3 个线上 fq 交易员）；其余由主审人工完成。全程只读，未修改源码、未调用交易接口。

**结论**：本轮新代码质量延续高位——无实盘资金风险的新缺陷（factor_quant 为 paper-only 硬约束，美股做空 live 路径双重拒绝）。**1 项运营级 P1（decision_records 12 天翻倍至 1449MB）+ 2 项代码 P2（均在 factor_quant，paper 环境）+ 1 项研究-部署一致性 P2 + 6 项 P3**。上轮 P1（pubcache init 捕获 DefaultTransport）确认关闭。

---

## 发现清单

### 1. [P1-运营] decision_records 1449MB：12 天翻倍（+63MB/天），仍无任何保留机制

实测 dbstat：`decision_records` 1449.0MB（data.db 1.5G 的 ~96%），10-25 时为 686.7MB。治理建议（上轮已给）未实施：全仓仍无该表的 prune/retention；读取方仅 API 决策面板，统计走 journal/entry_assessments。磁盘剩余 31Gi（按当前速率 ~1.5 年耗尽），但更早到来的是 API 决策查询变慢、备份窗口拉长、WAL checkpoint 压力。**建议本周实施**：保留 30–60 天全量、更早行删 `input_prompt`/decisions 大列或整行，配合 `PRAGMA incremental_vacuum`；这是两轮审查连续点名的唯一未决项。

### 2. [P2] factor_quant：重试耗尽后常规调仓对"同 symbol 双侧持仓"只看到一侧，泄漏一条腿

`trader/auto_trader_factor_quant_plan.go:285-290`（`held` map 按 symbol 键控）+ `auto_trader_factor_quant_exec.go:658-661`（仅 Retry 态走 `fqPlanRetryOrders`）。crypto 方向翻转时旧侧 close 连败 7 轮（首发+6 重试）后重试态清除，账户同 symbol 持有多空两侧；下一个 as_of 的常规 `fqPlanOrders` 中 `held[sym]` 只保留一侧（排序后为 short），另一侧规划器不可见——组合中性度失真，直到该腿 20% 止损或目标过期清仓。缓解：泄漏腿带止损、两条 flatten 路径双侧全平。建议：账户存在同 symbol 双侧持仓时始终走 retry 规划器（或保留 Retry 态直到干净通过）并告警。

### 3. [P2] factor_quant：权益峰值种子继承历史快照，复用 trader id 切策略可能一启动即回撤熔断锁死

`trader/auto_trader_factor_quant_risk.go:112-122`：首次评估 `peak = max(initialBalance, equity, Equity().MaxEquity(at.id))`，历史快照不分策略来源。trader id 先跑过 AI/us_stock 且权益更高时，切换成 fq（新账户 10k）首个 tick 相对旧峰值回撤可能 ≥25% → 直接 drawdown 清仓+锁存（需 KillResetID），日志不提示根因。建议：峰值只采信 fq 首启后的快照，或仅"无持仓且无调仓历史"时采信历史。

### 4. [P2-流程] crypto fq 目标是多空中性账本，但项目自己的前向校验刚结论空头腿未通过

只读核对线上 `data/factor_mining/crypto/quant_targets.json`（as_of 11-05，status ok）：23 权重中 11 个为负（Σ≈0，dollar-neutral）。而 b913c8b5（11-01）冻结 ensemble 前向校验结论：h=1/3/7 前向 IC 0.065-0.075 显著，但**五分位多空扣费后为负，原因是空头档混入暴涨币**。fq crypto paper 循环当前正按含未验证空头腿的目标运行——paper 无资金风险，且 fq_us 已有同类先例修复（12ee9d86 not_listed 剔除）。建议：空头腿修复（shortleg_convexity 方向）落地前，crypto 目标产出生成器剔除负权重（long-only/长偏）或把 fq crypto 目标显式标注 partial 状态，保持"研究未通过的不部署"纪律一致。

### 5. [P3] factor_quant 四项小瑕疵（子代理详审结论，细节见其报告）

- **horizon 退化**（`auto_trader_factor_quant.go:506-511` + `store/strategy_factor_quant.go:229-241`）：US 目标文件损坏时过期清仓阈值从 horizon+stale 退化为 3+2=5 天——方向 fail-safe 但随文件可用性漂移；建议读不到文件时用 runtime 缓存的 `fq.targetsHorizon`。
- **daily_pnl 恒 0**（`auto_trader_factor_quant.go:815`）：fq 路径从不写 `at.dailyPnL`（仅 AI 循环更新），账户卡当日盈亏永远 0；建议从 DayStartEquity 差推导或置 null。
- **US flip 重开腿 qty 不随刷新价重算**（`auto_trader_factor_quant_exec.go:674-684`）：仅 us_short_mode=paper 翻转场景，名义偏离由现金守卫兜底。
- **status=ok + 零权重 = 合法清仓**（`quant_targets.py:315` volscale `round(w*k,6)` 无 k 下限）：语义可辩护，建议 python 给 k 下限或改非 ok 状态。

### 6. [P3] factor_quant 两条信息级

整 tick 持有 executionMutex 且 US 路径内含逐符号报价网络 I/O（几十持仓时分钟级；API 读路径已绕开，暂无受害者）；前端 `FactorQuantConfig` 类型缺 `us_short_mode`/`us_short_fee_bps` 字段（GET→PUT spread 透传不丢配置，但 UI 无法管理做空开关——线上 `us_short_mode=paper` 是绕过 UI 设置的）。

### 7. [P3] 上轮 P1 残留收窄为已文档化 footgun

`provider/coinank/coinank_api/kline.go:68` 仍是 init 时 `pubcache.Wrap(nil)`。生产侧顾虑**结构性关闭**：`nofx/security` 依赖 `nofx/netmeter`（go list 验证依赖边），Go 初始化顺序保证 netmeter.init 先于 coinank_api 变量初始化——CoinAnk 流量必然被 netmeter 计量。测试侧 10-28 已改 `marketDataFn` 注入（36c86136，测试注释已记录原因）。残留：未来测试不得对 CoinAnk 路径做 transport 热替换，需走 marketDataFn——已是文档化约定，不再计为缺陷。

---

## 已验证无问题

- **crypto 周期锁收窄 + STATE_CHANGED（aedb30e3，实盘已跑 8 天）**：拆分与设计文档 `docs/design/crypto-cycle-lock-scope.md` 完全一致——buildAccountContext 锁内 / enrichMarketContext 锁外（~2 分钟行情 I/O 不再阻塞 us_stock 保护 tick，修复 21h 38 次平均 73s 排队）；重新加锁后 stopped 复查；STATE_CHANGED 动作表实现精确（open 查同向数量、partial 双非零、adjust 双向、close/hold 豁免=纯降风险不拦；容差 max(1e-9, rel 1e-6)、NaN/Inf 拒绝；拒绝发生在风险预留之前）。
- **美股做空模拟（9d632bc1 系）**：`ApplyShortFunding` 幂等（CAS `last_funding_time` 乐观锁 + 结算守卫 + busy 重试，重放 no-op）；资金费读取失败仅记日志**不跳过止损/止盈检查**（设计承诺兑现）；空头止损方向与不利成交价（`max(stop, price)`）正确；live 空头双重拒绝（配置层 `short_mode=live` 校验报错 + 执行层 SHORT_DISABLED，`short_mode=paper` 强制 paper_trading=true）；perp 映射磁盘缓存升 v2（e2de2ab3，防 v1 缓存致全员不可做空 24h）；空头报价时效归位到下单前重取价（a0fc6747，修复 LLM 耗时致全部 open_short 被 PERP_PRICE_STALE 拒）；short_mode=off 时提示词逐字节不变（golden 测试）。
- **paper 引擎（crypto）**：资金费实现纪律良好——fetch 在状态锁外、charge 在 mutateLocked 内原子提交；grace 10min + 2min 轮询间隔 + 失败退避 1→15min + 每符号缓存 500 条封顶 + 每 Advance 8 笔限流；读账户并发同步行情 20s 缓存（0fd2d904）。
- **限价入场先落库再下单（36c86136）**：submitting 态 + 稳定 client ID 持久化完整计划，写库失败不下单；重启按 client ID 查回（挂单恢复 pending / 已成交补 SL/TP）；孤儿撤单必查最终成交量、无计划残量只告警不接管；账户预留计入交易所未登记的外部开仓挂单（快照失败只拒新开仓）。
- **store 409 修复（574b977a）**：事务内读当前 updated_at 以 time.Equal 比较，脚本手改的时间戳格式不再永久 409；真实并发写仍拒（测试双向钉死）。
- **mcp 密钥日志脱敏（3843b7bb）**：9 家 provider 启动日志不再打印 key 首尾片段，仅"configured"。
- **evolution 自动复盘（c530570c/9c8f3892）**：`auto_review_enabled` 默认 false（opt-in），每用户 3 天节流；llmreplay 离线重放 + input_inject 为纯研究工具。
- **factor_quant 十项核心验证（子代理）**：再平衡去重（持久化+内存双保险，重启/连发/写库失败均不重复下单）、失败重试不重复已成功订单、目标过期清仓条件完备、熔断状态机（清仓后不可能被买回、daily 次日自动解除、drawdown 仅 KillResetID 幂等解锁）、回撤/日亏计算保护、止损迁移幂等、与 AI 交易员完全隔离（构造期硬拒非 paper + 编译期断言 + ledger 一对一绑定 + 查库证实零交集）、quant_targets.json 全谱系坏输入拒收、风控面板口径正确、清仓/停机记录跨重启去重。
- **fq_us not_listed 修复（12ee9d86）**：未上架多头剔除+重新加权，带专测。

## 测试状态

`go build ./...` 通过；store / kernel/stockengine / market/usstock / trader/paper / trader 全部 ok（含 lockscope、stock short、factor_quant 套件，`-count=1`）；`go vet ./trader ./store ./api` 干净（子代理验证）。

## 建议动作（优先级）

1. **本周**：实施 decision_records 保留策略（P1，连续两轮点名，增速 63MB/天）；
2. fq 双边持仓泄漏修复（P2 #2）——paper 环境但会污染研究数据的中性度假设；
3. fq 峰值种子修正（P2 #3）——一行级改动，防复用 id 踩坑；
4. **拍板**：crypto fq 空头腿处理（P2 #4）——研究结论与部署目标的对齐方式（剔除负权重 vs 标注 partial）；
5. P3 五项可顺手修（horizon 缓存、daily_pnl、flip qty、k 下限、前端 short_mode 字段）。
