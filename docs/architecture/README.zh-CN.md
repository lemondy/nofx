# NOFX 架构文档

**语言:** [English](README.md) | **中文**

> 最后更新：2026-11-19（基于 [2026-11-18/19 全链路深度审查与修复](QUANT_SYSTEM_REVIEW_2026-11-18.md)，基线 `dev`）。与英文版保持同步。

为希望了解 NOFX 内部实现的开发者提供的技术文档。

---

## 概述

NOFX 是一个**单用户自托管**的全栈 AI 交易平台，覆盖加密货币永续合约与美股（Binance bStock 代币 + 纸面账本）。系统有三条主线：

1. **LLM 交易路径**（本仓库语境下不加限定的"量化"默认指这条）：kernel 决策内核把行情、指标、扫描器证据预计算成结构化信号注入提示词，由 LLM 产出开平仓决策，执行层程序化校验与下单。
2. **factor_quant 程序化路径**：因子挖掘 → 冻结回测（S1）→ 模拟盘前向检验（S2）→ 纯程序化调仓，全程不经 LLM，代码层强制 paper。
3. **研究方法学**：预注册、演化账本、K 累计多重检验校正、增量评估三阶段（S1/S2/S3）。

**技术栈：**

- **后端：** Go 1.25、Gin、GORM（生产驱动 `gorm.io/driver/sqlite` → mattn/go-sqlite3 CGO；Postgres 为一等可选）
- **前端：** React 18 + TypeScript 5.8 + Vite 6、TailwindCSS、SWR、axios、zustand
- **AI 厂商：** DeepSeek、OpenAI、Claude、Qwen、Gemini、Grok、Kimi、MiniMax、GLM + custom 自定义端点
- **交易所：** Binance（合约主力）、Bybit、OKX、Bitget、Gate、KuCoin、Hyperliquid、Aster、Lighter、Indodax（现货骨架）、Binance bStock（美股）、paper 模拟盘

---

## 系统总览

```
                         ┌──────────────────────────────────────────┐
                         │              web/ (React SPA)            │
                         │   nginx 静态托管 + /api 反代（Go 不服务静态）│
                         └────────────────────┬─────────────────────┘
                                              │ /api (JWT)
┌─────────────────────────────────────────────▼──────────────────────────────────────────┐
│  Go 后端 (main.go)                                                                     │
│                                                                                        │
│  manager/trader_manager ── 构造/启动/恢复 AutoTrader（按 users × traders 全量加载）        │
│        │                                                                               │
│        ▼                                                                               │
│  trader.AutoTrader ── 四种策略形态分叉：ai_trading / grid_trading / us_stock / factor_quant│
│    │            │                                                                      │
│    │            ▼                                                                      │
│    │      kernel（LLM 决策内核）── mcp（9+1 AI 厂商客户端）                                │
│    │            ▲                                                                      │
│    ▼            │                                                                       │
│  trader/ 执行层（入场/出场/保护单/对账/风控）                                                  │
│    │                                                                                   │
│    ▼                                                                                   │
│  交易所适配器（binance/bybit/okx/.../paper）                                               │
│                                                                                        │
│  支撑层：market（K线/指标/扫描器） provider（外部数据源） store（SQLite/Postgres）              │
│          security（SSRF） netmeter（流量计量） fundsledger telegram cmd/* 工具              │
└─────────────────────────────────────────────┬──────────────────────────────────────────┘
                                              │
              外部：Binance fapi / CoinAnk / vergex(headless relay) / Hyperliquid /
                    Yahoo·bStock / Alpaca / TwelveData / OpenBB sidecar / NAS MariaDB(只读)
```

---

## 项目结构

```
nofx/
├── main.go                    # 进程入口：装配、启动、优雅关停
├── manager/                   # 多交易员管理器（加载/启动/恢复/RunVersion 重载）
├── api/                       # Gin 路由 + 中间件 + 全部 handler（路由注册即文档）
├── kernel/                    # LLM 决策内核：信号层/prompt/校验/程序派生
│   └── stockengine/           #   美股独立引擎（Kell 周期相位、默认现货多头 + 可选 paper 空头、市值分层）
├── mcp/                       # AI 模型客户端抽象 + 9 厂商 provider + 流式/限流/重试
├── trader/                    # 交易执行层（最大包，140+ 源文件）
│   ├── types/                 #   Trader/GridTrader 接口、TradeRecord
│   ├── binance/  bybit/  okx/  bitget/  gate/  kucoin/          # 交易所适配器
│   ├── hyperliquid/  aster/  lighter/  indodax/                 #
│   ├── binance_bstock/  binance_stocks/                         # 美股执行器
│   └── paper/                 #   加密模拟盘（撮合+资金费）
├── market/                    # 行情数据：K线聚合/指标/扫描器家族
│   ├── breakout/              #   突破/做空扫描器、在线调参器、BTC regime
│   ├── pubcache/              #   HTTP 公共缓存 + single-flight
│   ├── bnweight/              #   Binance fapi 全局限流（权重桶+优先级+在途预留对账）
│   └── usstock/               #   bStock/Yahoo 美股数据、NYSE 日历
├── provider/                  # 外部数据源：coinank/vergex/hyperliquid/alpaca/
│                              #   twelvedata/openbb/nofxos(已废弃,仅复用类型)
├── store/                     # GORM 持久层：40+ 表、迁移、busy 重试
├── fundsledger/               # 资金账本纯函数聚合（/api/funds/overview）
├── security/                  # SSRF 防护：ValidateURL + SafeHTTPClient + LAN 白名单防重绑定
├── crypto/                    # RSA-OAEP + AES-GCM 信封加密（交易所密钥）
├── auth/                      # JWT(HS256) + bcrypt + 内存黑名单
├── config/                    # .env 进程级配置（无热更新）
├── hook/                      # 全局字符串键 hook 注册表（扩展点，当前无生产注册方）
├── logger/                    # logrus，按日轮转 data/nofx_YYYY-MM-DD.log
├── netmeter/                  # RoundTripper 字节计量 → /api/net-traffic
├── telegram/                  # 通知 + LLM 代理 bot（agent/）+ 会话记忆（session/）
├── telemetry/  wallet/        # 匿名遥测；Base 链 USDC 余额校验
├── cmd/                       # 运维工具：placetp/protectposition/cancelorphan/
│                              #   listalgos/llmreplay/cycleic/repair20261007
├── web/                       # React 前端（见前端架构）
├── scripts/                   # 研究基础设施：backtest/factor_mining/autoresearch/promotion
├── docs/                      # architecture / research(含 37 份预注册) / api / design
├── data/                      # 运行数据：data.db、evolution/ledger.jsonl、
│                              #   factor_mining/、quant_targets.json（不提交 git）
├── docker/  nginx/  railway/  # 部署形态
└── AGENTS.md                  # AI 协作与研究流程约定
```

---

## 后端架构

### 进程启动与生命周期

`main.go:32-204` 顺序：

1. `godotenv.Load()` 读 CWD `.env` → `logger.Init` → `config.Init`
2. `crypto` 初始化（失败即 Fatal）→ `store` 初始化（GORM AutoMigrate 全表）
3. `openbb.EnsureSidecar`（fail-open，可选本地数据面）
4. `manager.LoadTradersFromStore`：遍历所有用户，trader 必须同时有 strategy、AI model、enabled exchange 才加载；`IsRunning=1` 的经 `StartIfDesired(RunVersion)` 原地恢复
5. `api.NewServer`（内含 72h 自动复盘循环）→ telegram 监督 goroutine → `notify.Init` → breakout 扫描引擎 → vergex headless relay

关停：`server.Shutdown`(5s) → 第二次 Ctrl+C 强退 → `StopAll`（`SignalStop` + 并行 `StopForRestart`，us_stock 保留挂单）。**单用户系统**：首个用户注册后注册接口永久关闭（`api/handler_user.go:51-63`）。

### 多交易员管理（manager）

- 内存注册表 + 锁序 `loadMu → tm.mu`；`StartTrader` 查找+启动同临界区防孤儿循环
- 重载用 **RunVersion**：`StopForRestart`（保留挂单）区分于真停（撤单）
- **授权不走 manager**：所有数据接口经 `getTraderFromQuery` → `GetForUser`（WHERE user_id）归属校验；公开接口以 `ShowInCompetition` 为可见性门槛

### HTTP API 与中间件

请求链：nginx → `gin.Logger/Recovery` → `SetTrustedProxies(nil)` → `requestLimits`（body ≤2MB；固定窗口限流：默认 600/min、行情 120/min、login 10/min）→ CORS `*` → 路由 → 受保护路由过 `authMiddleware`（Bearer JWT HS256 + DB token 撤销 + TokenVersion，`api/server.go:718-764`）。

路由分组（完整清单见 `api/server.go:88-487` 与 `docs/api/API_REFERENCE.md`）：认证与系统面、交易员 CRUD/start/stop/prompt/sync-balance、策略 CRUD/preview-prompt/test-run、行情（公开 klines/symbols/trending/breakout）、交易数据（status/account/positions/decisions/statistics/usstock）、资金（funds/overview、exchanges）、模型与 AI 成本、研究与复盘（review/factor-mining/promotion/code-reviews/system-quality）、系统（net-traffic/telegram）。

**约定：路由注册即文档** —— `api/route_registry.go` 的 schema 同时注入 telegram agent 提示词，文档与实现同源。

### 四种策略形态

`trader/auto_trader.go:666-810` 的 `run()` 按策略类型分叉：

| 形态 | 循环 | 说明 |
|---|---|---|
| **ai_trading**（LLM 交易） | `runCycle`，5 分钟整点对齐 | kernel 决策 + 执行层程序化把关，见下两节 |
| **grid_trading** | `RunGridCycle` | 网格策略，与 ai_trading 共用合约监视器 |
| **us_stock** | `runStockLoop` | ET 时区预设决策时刻表 + 60s 保护 tick；stockengine 五阶段 cycle（对账→行情+周回撤熔断→regime 缩仓→LLM→锁内审查执行）；bStock 纸面账本 |
| **factor_quant** | `runFactorQuantLoop`，独立 5 分钟 tick | 纯程序化调仓，**代码级强制 paper**（`factorQuantGuard` 拒绝非 paper），见下文 |

调度对齐：`nextAlignedWait` 把扫描对齐整点网格（5m → :00/:05/…），越界跳过不补跑；启动即先跑一轮。

### LLM 决策内核（kernel + mcp）

**kernel/ 按主题分组：**

| 主题 | 关键文件 | 职责 |
|---|---|---|
| 入口/编排 | `engine_analysis.go` | `GetFullDecisionWithStrategy` 主流程、regime-skip（`ProgramSynthesized` 零计费）、JSON 修复解析、批内同向/反向冲突消解 |
| 类型/Schema | `engine.go` | `Context`/`Decision`、wait_state/stage 派生、候选池来源（static/ai500/oi_top/oi_low/piggy_dash/short_scan/hyper_*/mixed） |
| 校验/吸附 | `engine_position.go` | `validateDecision`、锚点/SL/TP 吸附（0.05% 容差）、注解回填 |
| 信号层 | `signal_layer.go`（3650+ 行） | `ComputeSymbolSignals` 每币信号块、`computeHardEntryGate` 双向门、`classifyTrend` 四象限（up/pullback/down/rally）、`methodStopPlan`、RR 扫描、TP 菜单、`SHORT_SCAN_ONLY_NO_LONG` |
| 门辅助 | `regime_line.go` `volatile_gate.go` `market_regime.go` `bb_ride.go` | EMA50 线守卫、薄量/急涨/OI 异动绝对禁入、闭合时间对齐 1h/4h regime、布林骑行证据 |
| Prompt | `engine_prompt.go` + `schema.go` | 系统/用户提示词分段、双语数据字典、硬约束按 live config 渲染 |
| 美股 | `stockengine/` | Kell 周期相位分类、默认现货多头 + 可选纸面空头（`us_short_mode: off|paper`、`prompts_short.go`）、市值分层（`mega_large`/`mid_small`）、confidence<80 拒单 |

**mcp/**：接口 `AIClient`（`mcp/interface.go:14`）；`Client` 模板基类 + 9 个 provider 子包经 `RegisterProvider` 注册；流式 SSE（90s 空闲看门狗，分档硬顶首试 300s/重试 600s；上游 `error` chunk 与未收尾异常断流直接拒收重试，不进入截断 JSON 抢救）；每 host+key 并发限流；上下文守卫从不删 system 消息。

**一轮决策生命周期**（crypto 周期）：

1. **周期准备**（`trader/auto_trader_loop.go:27`，全程持账户执行锁）：pending 限价单结算、波动率调仓/移动止损、保护单看门狗、孤儿持仓对账、auth 熔断检查、配置漂移自检
2. **构建上下文**：账户/持仓快照（含交易所 SL/TP 实查）、loss-streak 禁买映射、近 5 笔平仓 + 滚动统计；权益快照落库、日内熔断基线锚定
3. **数据获取**：按执行 venue 取多周期 K 线（动态透传 `executionTimeframe`，如 Hyperliquid `5m`，强制补 1h/4h），OI 低于阈值的候选剔除
4. **Prompt 组装**：system prompt 字节稳定（命中 provider 前缀缓存）；每候选 `ComputeSymbolSignals` 产出约 30 开关驱动的信号 JSON，同时捕获 `GateStates`（后续吸附与派生的真值源）；token 超预算先剥 ohlcv，仍超则整周期报错
5. **regime-skip**：全部候选被硬拦且全部 AI 持仓平仓锁定 ⇒ 不调 LLM，程序合成 wait（`ALL_CANDIDATES_HARD_BLOCKED`，`ProgramSynthesized=true`），零 token 且不写 `ai_charges`
6. **模型调用** → 解析（CoT 提取、JSON 修复/正常完结流截断抢救）
7. **程序吸附与校验**：`correctLimitAnchors`（锚被抑制的限价开仓降级 wait）→ `correctStopLossToPlan`/`correctTakeProfitToPlan`（吸附回程序预计算的 stop_plan/TP 菜单）→ `validateDecision`（动作枚举/必填/杠杆钳制/仓位上限）→ **程序派生注解覆盖模型声明**（wait_state/stage/blocking_factors）→ 批内冲突消解（反向对开降级 wait；同币同向重复开仓去重收敛）
8. **trader 执行**：先平后开排序、STATE_CHANGED 数量漂移拒绝、影子拦截记录、preTrade 规则、`applyHardRiskGates`、逐条执行（保证金预留/释放）
9. **落库**：`decision_records`（全量 prompt/CoT/决策 JSON，system prompt 哈希去重）、`entry_assessments`（每决策一行质量自评数据集）、`gate_shadow_blocks`（被硬门拦截但三元组完整的反事实，8h/48h 到期后用 15m K 线裁决 tp_first/sl_first/timeout，用于校准阈值）、`ai_charges`（仅真实上游 LLM 调用计费）

### 执行层（trader/）

**入场路径**：限价为默认（`limit_entry_enabled`），锚点 = 快照价 ± clamp(0.5×ATR(1h))；锚点穿越回退市价；市价两类例外（breakout 六条件、bb_ride 骑行）经 `marketExceptionGate` 执行端复核（`trader/auto_trader_orders.go:150`）；可选入场价格带守卫 `entry_band`（`auto_trader_entry_band.go`，`off`/`shadow`/`live`）；点差门、OI 门槛、保证金预算硬门、max_positions 槽位核算三路径强制。

**出场路径**：SL/TP 保护单全链路校验（`placeProtectiveOrders` → `verifyProtectiveLegs` 两次回查 → 缺腿 Telegram 告警；保护失败=开仓失败，F01 契约；各交易所适配器开仓入口保留对向仓位保护单与手动挂单）；30s 保护 watchdog 补挂/重臂/撤孤儿腿；1R 锁盈（`initial_stop_loss` write-once 锚，达 1R 减半+SL 移开仓价）；TP 阶梯（tp_trim/tp_full）；TP 菜单 exit_mode（trend/range/quick）；adjust_stop_loss 仅收紧且保本-or-better；early-close 门（不足 4h 需 1h ≥2 根逆势收盘）。

**风控子系统**：账户级熔断（日亏/回撤）、loss-streak 连亏禁开、drawdown-protect、margin usage 预算、funding 拥挤年化门、`risk_status` 显式暴露账户/持仓读取失败（`equity_unknown`/`positions_unknown` 防假绿）；账户执行锁按 exchangeID 跨 trader 共享（`account_execution.go:225`）。

**对账与恢复**：启动对账 + `lim-<hash4>-` client-ID 归属标签重 claim；`reconcileOrphanedPositionRows` 每周期收口 DB/交易所漂移（只撤本 trader 标签单，手动单 hands-off 永不取消）；重启后 pending 影子行恢复；配置漂移自检（DB vs 内存 config hash）。

**交易所适配器状态**：

| 适配器 | 成熟度 | 备注 |
|---|---|---|
| binance | 完整（主力） | order_sync/user_stream/规则缓存/薄量阈值 |
| paper | 完整 | 加密模拟盘，factor_quant 的 broker |
| binance_bstock / binance_stocks | 完整 | us_stock 执行器；支持 `StockPaperShort` 纸面空头账本；binance_stocks 无止损 API |
| bybit/okx/bitget/hyperliquid/aster/lighter | 完整 | Trader+order_sync+entry band；`FormatQuantity` 统一返回标的 base 数量 |
| gate/kucoin | 可用 | 无 PlaceLimitOrder（限价入场 fail-closed）；Gate 数量契约已对齐 base 单位 |
| indodax | 骨架 | 现货映射，OpenShort/SetStopLoss 报错（见已知问题 P1-2） |

### factor_quant 程序化交易员

`trader/auto_trader_factor_quant*.go`：独立 5 分钟 tick → 组合级风控（回撤分档降仓 10%→0.70x / 15%→0.40x / 20%→0.25x，先于 25% 熔断；**非调仓日 `too_soon` 窗口内触发新回撤档位时由 `fqApplyDrawdownTrim` 立即执行只减仓缩容**）→ 读 `data/quant_targets.json`（factor_mining 流水线产出）→ 调仓门（horizon/as_of/stale）→ 分 phase 调仓（平仓→减仓→加仓→建仓）→ 止损补挂/Chandelier 移动止损 → 权益快照。已支持：牛熊 regime tilt（基准日线 EMA20/50，牛市 20 多/10 空非对称）、滚动 alpha 衰减出场（旧仓平仓失败跨 tick 重试，替补仓受剩余总敞口预算约束）、2.5σ 自适应单调棘轮止损（`fqRatchetStop`，峰值仅在平仓成功后清理）、1/σ² 空头定权、市值分层敞口约束（`size_tier`、`max_mid_small_exposure_pct`）及美股纸面空头（`us_short_mode`、`us_short_fee_bps`）。核心四开关默认开启（`store/strategy_factor_quant.go:66-81`）。

全链路（挖掘→S1→S2→实盘）见「研究与回测基础设施」。

### 数据层

**K 线三层链**（`market/data_klines.go`）：CoinAnk（主）→ Binance fapi 直连回退（仅同所）→ OpenBB/yfinance tier-3（仅主流币）；离线研究与因子更新另支持局域网只读 **NAS MariaDB K 线库**（`scripts/backtest/nas_klines.py`，`crypto_klines` + `us_daily`，`factor_mining/data_fetch --source nas-mysql|hybrid`）。多周期：15m 由 3m 本地聚合（零额外厂商调用）；forming candle 用 live 价回补（±80% sanity band）；指标全部手写（EMA/Wilder RSI/ATR/MACD/BOLL，`market/data_indicators.go`，展示序列 $O(N)$ 线性递推），**全路径严格只用 closed bars**；staleness 检测 MaxStaleBars=3。

**HTTP 底座（所有出站流量）**：`netmeter.Wrap` 字节计量 → `pubcache`（公共 GET 缓存 + single-flight，精确路径白名单）→ `bnweight`（Binance fapi 权重桶：IP 2400/min + funding 500/5min + 数据 1000/5min，Critical/Normal/Low 优先级，保留并发在途预留量对账，429/418 全局冷却）→ `SafeHTTPClient`（SSRF：URL 校验 + 字面量私网 IP / `NOFX_LAN_ALLOWLIST` 显式放行 + 阻断公网域名 DNS 重绑定至私网及公网向内网重定向 + DNS fail-closed，`security/url_validator.go`）。代理统一走 `HTTPS_PROXY` 环境变量。

**扫描器家族**（`market/breakout/`）：5 分钟主循环，主宇宙=成交量 Top20 + 二宇宙（热门/涨/跌榜各 20）；做空宇宙 = 24h 涨幅 Top50 + hist_gainer 历史涨幅池 + near_high 慢顶（距 90 日高点 <5% + 4h 看跌背离）；九维评分，分级线 70/55/40 → strong/medium/weak；**在线调参器**按隔日 UTC 块截面秩 IC（Spearman）评估，≥30 块且 |t|>3 才显著，产出仅是研究提案不直接发布（`shorttuner.go:75-97` 双开关）。

**provider/ 状态**：vergex（go-rod headless 过 Cloudflare，AI500/OI/NetFlow **唯一来源无 fallback**）、coinank K 线（活跃；其 OpenAPI 模块死代码）、hyperliquid xyz 资产（活跃）、alpaca/twelvedata（前端图表）、openbb sidecar（可选）、nofxos（已废弃，仅复用类型）。

**store/**：40+ 表（GORM AutoMigrate，无版本号 schema；Postgres 走 ensureColumns 只加列）。核心表：`traders`/`trader_positions`/`decision_records`（+system prompt 去重表）/`entry_assessments`/`gate_shadow_blocks`/`trader_equity_snapshots`/`trade_journal`/`ai_charges`/`factor_quant_state`/美股纸面家族（`stock_paper_*` 等）。SQLite 生产 DSN：`WAL + synchronous=NORMAL + busy_timeout=15000 + _txlock=immediate`，池 4/4，busy 一次性 300ms 重试。

**关键数据口径（改动前必读）：**

- `funding_rate` 是 Binance 原始小数（0.0001=0.01%/结算期）；年化用该币真实结算间隔（fundingInfo 表），非固定 ×3/日；拥挤度用 `funding_annualized_pct`
- `premiumIndex.lastFundingRate` 是漂移的前瞻值；rollover 判定用已结算序列（`funding_history.go`）
- OI USD delta = base 增量 × 最新价（不是名义值直接相减）；OI 短窗 build-up 用合约数量；失败 ≠ 0（`FundingRateOK`/`OpenInterestOK` 区分）
- 美股 bStock 以 1d bar 为波动标尺，缺 1d ATR fail-closed；freshness 按 NYSE 开市判定

---

## 前端架构（web/）

**骨架**：`main.tsx` 挂 `BrowserRouter` + sonner；但 **`App.tsx` 手写路由**（`useState(pathname)` + popstate/hashchange + `history.pushState`，`App.tsx:182-227`），react-router 仅个别组件的 `useNavigate` 使用；页面全部 lazy import。未匹配路径回退 LandingPage。认证：JWT 存 localStorage；401 由 axios 拦截器统一清 token → `unauthorized` 事件 → returnUrl → `/login`。

**页面与数据源：**

| 页面 | 职责 / API |
|---|---|
| TraderDashboardPage | 交易员仪表盘（默认运行中 trader 里 cycles 最大者）；App 顶层 SWR 注入 status/account/positions/decisions（15s）；RiskStatusPanel 30s（未知状态红灯告警） |
| StrategyStudioPage | 策略编辑器（含 `FactorQuantConfigEditor` 百分比双向归一化与跨市场清理）；裸 fetch `/api/strategies`；保存 15s AbortController + 409 乐观锁 `base_updated_at` |
| DataPage | 行情/扫描数据 tabs（crypto/hl/breakout/shortscan 🩸）；trending 浏览器直连 vergex 并 relay 回写后端缓存 |
| FundsPage | 资金总览 `/api/funds/overview`（fundsledger）；每日明细分页 |
| ReviewPage | 交易日志/规则引擎/AI 复盘 `/api/review/*`（带用户归属校验与条数上限） |
| ResearchHubPage | hash 三 tab：factor-mining / paper-promotion / research-log（各自独立页面文件，单定时器 + 请求序号防并发竞态） |
| SettingsPage | 模型/交易所配置、netmeter 流量卡（5s）、系统质量与代码审查嵌入 tab |
| 其余 | FactorMiningPage、PaperPromotionPage、ResearchLogPage（纯静态）、CodeReviewHistoryPage、SystemQualityPage、CompetitionPage、AITradersPage、LandingPage |

**数据获取约定**：SWR 为主，**弹性退避而非永久 latch**——App 顶层 account/positions 连续失败 2 次进入 60s 心跳，任一次成功恢复 15s（`App.tsx:304-345`）；decisions 线性退避。axios `httpClient` 单例：请求注入 Bearer，响应拦截器系统错误（401/5xx）toast 后吞掉、仅 4xx 业务错误返回调用方（`web/src/lib/httpClient.ts:109-255`）。例外：StrategyStudio/AuthContext 部分裸 fetch 绕过统一处理（见已知问题）。

**其他**：zustand 仅 2 个 store（配置聚合/弹窗），其余 SWR+props；K 线图 lightweight-charts、曲线图 recharts；i18n 三语（en/zh/id，`translations.ts` 5347 行）但策略编辑器另有独立字典；设计体系见 `docs/design/TRADING_UI_DESIGN_SYSTEM.md`（CSS 变量 token，`index.css:13-98`）；测试 vitest + testing-library（30 文件 289 用例）；类型 `types/` 全手写镜像 Go 结构体（无 codegen）。

**构建部署**：dev vite proxy `/api → localhost:8080`（`web/vite.config.ts:33`）；生产 nginx 容器静态托管 dist + `/api` 反代（`docker/frontend/Dockerfile.frontend`、`railway/start.sh` 动态生成 nginx conf）。**Go 后端不服务静态文件。**

---

## 研究与回测基础设施

| 目录 | 职责 | 入口 |
|---|---|---|
| `scripts/backtest/` | **NautilusTrader 1.222.0 + QuantStats 0.0.86** 统一回测（`nt_engine.py`，默认 `--engine nautilus` 与 `nt-parity`，`next_open` 与 `close` 均严格因果且先结算持有期资金费/借券费；`--engine backtrader` 仅保留只读兼容历史冻结研究）；另含 `nas_klines.py`、`dsr_audit.py`、`alpha_illusion_audit.py`、`paper_pipeline.py`；**只读 `data/data.db` 的 decision_records，出场参数从 strategies.config 现读** | `.venv/bin/python -m scripts.backtest <sub>`；README.md 为权威口径 |
| `scripts/factor_mining/`（50+ 文件） | 每晚因子挖掘：遗传变异 + LLM 提案 + `s0_diagnostics` / `tiers` → 冻结评估（K 门限 $t_{\text{req}}=\max(2, \Phi^{-1}(1-0.025/K))$）→ `quant_targets.json` 交易权重 → 第 6 步 `dsr_audit` | `run_nightly.sh`（launchd 02:30，`FM_US_MINING` 控制美股线，`--source nas-mysql\|hybrid` 支持 NAS K 线） |
| `scripts/promotion/` | 模拟盘晋升：基线登记、候选/对照对、日报、增量判定（`harm_stop`/`extend`）与 S3 留出监控（`rollback_suggested`）、fq_us S2 前向检验报告（126 交易日，NW lag20，$z\ge 1.65$） | `python -m scripts.promotion.report` |
| `scripts/autoresearch/` | 自进化打分研究（采集→冻结数据集→K 门限→一次性 holdout） | `run_round.sh` |
| `cmd/llmreplay/` | 离线 LLM 重放（固定样本、三臂 baseline/noise/variant） | `go run ./cmd/llmreplay` |

**增量评估三阶段**（`INCREMENTAL_EVAL_DESIGN_2026-11-03.md`）现状：

- **S1 离线筛选**：工具链完整（dev/holdout 二分、噪声组、随机剔除安慰剂、影子闸门、DSR 审计）；账本现状 S1=40
- **S2 预注册在线配对**：两条路径并存（promotion 候选/对照对；fq_us 单交易员前向检验）；账本 S2=4；`report.py` 已接入 `harm_stop` 与 `extend` 增量判定
- **S3 留出检验**：`report.py` 已支持留出期回撤/退化触发 `rollback_suggested` 监控，当前账本尚无正式晋升的 S3 行

**账本**：`data/evolution/ledger.jsonl` 只追加（当前共 44 行），`evolution_ledger.append_event` 用 flock 保证 K 计算与写入原子；K 按 track 累计（`fq_crypto` 15、`fq_us` 11、`crypto_llm` 11、`us_llm` 2、`crypto_carry` 1 等）。

**方法学约定**：预注册文档 `docs/research/YYYY-MM-DD-<topic>-prereg.md`（37 份），**先提交后计算收益/IC，跑完不得改参数**；四类风险检查（前视/幸存者偏差/标签重叠/多重检验）分别有对应工具与教训记录（如 `scripts/backtest/README.md:121` 的同 bar 前视 CAGR +73.5%→+41.1%）；研究记录三处手工同步：`docs/research/*.md` + `web/src/constants/researchLog.ts` + `docs/research/quant-research-log.html`。

---

## 关键设计原则

1. **程序派生而非模型声明**：wait_state/stage/blocking_factors/硬门结论全部程序推导，模型声明一律被覆盖——杜绝数据集自相矛盾
2. **precompute-derived-data-for-llm**：门、RR、TP 菜单、regime、微趋势全部代码预算进信号 JSON；prompt 明令禁止模型从原始 K 线重算
3. **"计划的门就是成交的门"**：stop_plan/rr_scan/TP 菜单/执行端同口径；模型输出经 0.05% 容差吸附回程序价
4. **fail-closed 为默认，fail-open 逐点记录**：缺证据 ≠ 通过（BTC_REGIME_UNKNOWN、VENDOR_DIVERGENCE_UNKNOWN 一律拦截）；流动性类门缺数放行但写入词表；对账/保护单链路的 fail-open 点均有注释与告警
5. **零 token 合成**：确定性结论（全候选硬拦、平仓门锁定、双向冲突）压缩渲染或跳过 LLM（且不记虚拟计费）
6. **一处真值、两处渲染**：GateStates 在 prompt 构建期捕获一次，供 wait 派生、影子记录、执行端豁免复用
7. **单用户 + 归属校验**：manager 内存表不是授权源；一切走 `GetForUser`
8. **回测口径对齐实盘**：backtest 只读生产 DB、出场参数从配置现读、限价穿价 1 tick 保守口径

---

## 部署形态

| 形态 | 说明 |
|---|---|
| **docker-compose** | 前端 nginx 容器（静态 + /api 反代 nofx:8080）+ 后端容器；`.env` 挂载 + env_file；`start.sh` 生成的 DATA_ENCRYPTION_KEY 写回 .env 持久 |
| **Railway** | all-in-one 容器：内启 nginx 反代 127.0.0.1:8081；`railway/start.sh` 运行时生成密钥（**见已知问题 P1-1**） |
| **本机 nohup**（当前 live） | kill / build / nohup 三步分开执行；**必须显式 `env HTTPS_PROXY=http://127.0.0.1:7890`**（.env 无代理变量）；`go version -m` 验证构建；data.db 用 `mode=ro` 查询 |

---

## 核心依赖

### 后端 (Go)

| 包 | 用途 |
|---|---|
| `gin-gonic/gin` | HTTP 框架 |
| `gorm.io/gorm` + `gorm.io/driver/sqlite`（mattn CGO） | ORM 与生产 SQLite 驱动（Postgres 可选） |
| `adshao/go-binance/v2` | Binance 合约客户端 |
| `go-hyperliquid` / `bybit.go.api` / `gateapi-go` / `lighter-go` | 交易所 SDK |
| `golang-jwt/jwt/v5` | JWT 认证 |
| `rs/zerolog`（go.mod 依赖）但实际使用 `sirupsen/logrus` | 日志（按日轮转） |
| `joho/godotenv` | .env 加载 |
| `go-telegram-bot-api/v5` | Telegram |
| `go-rod/rod` | vergex headless relay |
| `ethereum/go-ethereum` | 钱包校验 |

### 前端 (React)

| 包 | 用途 |
|---|---|
| `react` 18 + `react-router-dom`（仅局部使用） | UI 框架 |
| `swr` | 数据获取/轮询 |
| `axios` | HTTP 客户端（httpClient 单例） |
| `zustand` | 轻量状态（2 个 store） |
| `lightweight-charts` | K 线图 |
| `recharts` | 权益/对比/因子图 |
| `tailwindcss` + 自建 ui 基件 | 设计体系 |
| `vitest` + `@testing-library/react` | 测试 |

---

## 已知问题与技术债（2026-11-19 更新）

> 2026-11-18/19 深度审查发现的 20 项仓库内代码/契约/统计缺陷（含 `nt_engine` close 模式净值与资金费时序、多所开仓误撤对向保护单、`factor_quant` 非调仓期回撤即时减仓与棘轮止损、SSE 断流误抢救、SSRF DNS 重绑定、风控未知态假绿、regime-skip 幽灵计费等）已全部修复并合入 `dev`，详见 [QUANT_SYSTEM_REVIEW_2026-11-18.md](QUANT_SYSTEM_REVIEW_2026-11-18.md)。以下仅保留当前仍未关闭的架构债与运维边界项。

### P1（正确性 / 安全 / 治理）

1. **Railway 密钥不持久**（`railway/start.sh:10-15`）：`RSA_PRIVATE_KEY`/`DATA_ENCRYPTION_KEY` 未在平台 env 设置时每次启动随机生成且不持久 → 重启后所有加密的交易所 API key 无法解密，trader 静默加载失败。对比根目录 `start.sh:142-146` 是写回 .env 的。
2. **indodax 适配器可被选为 AI 合约策略**（`trader/auto_trader.go:476`）：SetStopLoss/SetTakeProfit/OpenShort 硬报错 → 开仓后"裸奔 → 保护失败 → 紧急平仓"循环。应在构造期仿照 `factorQuantGuard` 拒绝无止损 API 的适配器。
3. **S2 在跑交易员与新默认开关的运行态隔离**（运维确认项）：`factor_quant` 四项增强开关（`regime_tilt`/`rolling_exit`/`vol_adaptive_stop`/`drawdown_scale`）在配置未显式赋值时默认开启；重启加载新二进制前，若需冻结 `fq-us-momentum-ls-v1` 的原始 S2 规则，应在 DB 策略配置中显式写入 `false` 或分立新交易员 ID 对照。
4. **launchd 夜间任务运行仓库外代码副本**（`~/Library/LaunchAgents/com.nofx.factor-mining.plist` 指向 `~/workspace/nofx-autoresearch`，无 .git）：主仓库修复（含第 6 步 `dsr_audit`）需将 launchd plist 切回主仓库 `/Users/zhangyun/workspace/nofx` 后方可在夜间任务生效。
5. **前端路由三轨制**（`web/src/App.tsx:182-227`）：BrowserRouter 空挂 + 手写路由 + 局部 `useNavigate` 并存，路由映射表 4 处重复；`ChartWithOrders*.tsx`（836 行）、`PageNotFound.tsx` 为无引用死代码。

### P2（设计缺陷，摘选）

- 限流粒度失效：官方部署后端均在 nginx 后，`SetTrustedProxies(nil)` 使全站共享单桶（`api/server.go:47`）
- 每个带鉴权的数据请求都执行 `LoadUserTradersFromStore`（持 loadMu + 3 次 DB 查询），慢重载会串行阻塞数据接口（`api/server.go:695`）
- `decision_records` 清理函数存在但无调用方，表持续膨胀（`store/decision.go:338`）
- prompt 阻断码词表与 gate 实际输出仍有部分滞后（`DATA_STALE_*`、NEG_EDGE 系列等，`kernel/schema.go:312`）
- `buildAccountContext` 对持仓 map 直接类型断言，单条畸形快照可 panic 停机 trader（`trader/auto_trader_loop.go:669`）
- 限价入场路径缺市价路径的 fresh 账户重准入/降尺寸复验（`trader/auto_trader_pending.go:230`）
- JWT 存 localStorage（XSS 可窃取，`web/src/lib/…AuthContext.tsx:109`）；i18n 三套机制并存；部分页面裸 fetch 绕过 httpClient 统一处理
- 增量评估基线登记 `baselines.json` 尚未在生产环境执行首次 `baseline set`；S3 尚未产生正式账本行
- 短调参器 journal 全量重写 O(N)/30min（`market/breakout/shorttuner.go:624`）

### P3（摘要）

死代码若干（store/driver.go、provider/coinank apikey、nofxos 网络客户端、hook 未注册扩展点）；`crypto.go` 注释声称的 replay 防护实际只是 5 分钟时间窗；两套 Binance HTTP 栈并存（`market/api_client.go` 独立于 `SafeHTTPClient`）；`docs/architecture/STRATEGY_MODULE.md` 严重过时（引用已不存在的 `decision/` 包），仅留作历史参考。

---

## 相关文档

| 文档 | 内容 | 状态 |
|---|---|---|
| [QUANT_SYSTEM_REVIEW_2026-11-18.md](QUANT_SYSTEM_REVIEW_2026-11-18.md) | 2026-11-18/19 全链路量化深度审查与修复报告 | **现行（最新）** |
| [QUANT_SYSTEM_REVIEW_2026-11-14.md](QUANT_SYSTEM_REVIEW_2026-11-14.md) | 2026-11-14 全链路量化 review（修订版） | 历史基线参考 |
| [INCREMENTAL_EVAL_DESIGN_2026-11-03.md](INCREMENTAL_EVAL_DESIGN_2026-11-03.md) | 增量评估三阶段设计 | S1/S2 已落地，S3 监控已接入 |
| [FACTOR_QUANT_TRADER_DESIGN_2026-11-01.md](FACTOR_QUANT_TRADER_DESIGN_2026-11-01.md) | factor_quant 交易员设计 | 已实现并扩展 |
| [FQ_US_MOMENTUM_S2_DESIGN_2026-11-05.md](FQ_US_MOMENTUM_S2_DESIGN_2026-11-05.md) | fq_us 动量 S2 设计 | 在跑 |
| [LLM_EVOLUTION_LOOP_DESIGN_2026-11-02.md](LLM_EVOLUTION_LOOP_DESIGN_2026-11-02.md) | LLM 演化循环 | 设计稿 |
| [STRATEGY_MODULE.zh-CN.md](STRATEGY_MODULE.zh-CN.md) | 策略模块（旧版决策引擎描述） | **严重过时**，仅作历史参考 |
| [../api/API_REFERENCE.md](../api/API_REFERENCE.md) | HTTP API 参考 | 随 route_registry 维护 |
| [../design/TRADING_UI_DESIGN_SYSTEM.md](../design/TRADING_UI_DESIGN_SYSTEM.md) | 前端设计体系 | 现行 |
| [../research/paper-promotion.md](../research/paper-promotion.md) | 论文/策略晋级流程 | 现行 |
| `scripts/backtest/README.md` | 回测权威口径 | 现行 |

## 快速链接

- [最新量化审查报告](QUANT_SYSTEM_REVIEW_2026-11-18.md) / [增量评估](INCREMENTAL_EVAL_DESIGN_2026-11-03.md) / [快速开始](../getting-started/README.zh-CN.md) / [FAQ](../faq/README.md)

---

[← 返回文档首页](../README.md)
