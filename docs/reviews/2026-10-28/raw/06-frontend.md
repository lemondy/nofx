# 前端（web/src）及对应后端 API 口径审查报告

说明：`web/node_modules` 不存在，`npx tsc --noEmit` 与 `npx vitest run` 无法执行（跳过）；本报告全部结论来自逐行读代码，关键结论均给出双方（前端+后端）证据。

## A. 确认的 Bug

### A1 | P1 | 持仓历史页「净盈亏」卡片双重扣手续费
- **严重度**：P1（结果错误，交易员看到的净利偏低）
- **位置**：`web/src/components/trader/PositionHistory.tsx:604-618`；后端口径 `store/position_query.go:117-124`
- **触发场景**：任何有手续费的 trader 打开「持仓历史」标签页，第二行统计卡的「净盈亏」= `stats.total_pnl − stats.total_fee`。而后端 `total_pnl` 已经是 `Σ(realized_pnl − fee)`（净额），于是显示值比真实净盈亏**再少扣一遍总手续费**。按仓库注释里自己的例子（177 笔、fees=10.14），净 −18.53 会显示成 −28.67。
- **证据**：
  ```go
  // store/position_query.go:122  TotalPnL 已是净额
  net := pos.RealizedPnL - pos.Fee
  stats.TotalPnL += net
  ```
  ```tsx
  {/* PositionHistory.tsx:606-609  再减一次 fee */}
  value={((stats.total_pnl || 0) - (stats.total_fee || 0) >= 0 ? '+' : '') +
    formatNumber((stats.total_pnl || 0) - (stats.total_fee || 0))}
  ```
  同文件 zh 文案也自证口径：`translations.ts:2696 totalPnL: '总盈亏(净)'`——上面「总盈亏」卡已按净值标注。
- **修复建议**：净盈亏卡直接显示 `stats.total_pnl`；或后端明确拆出 `gross_pnl / net_pnl / total_fee` 三字段，前端只做展示不做算术。

### A2 | P1 | 小价格币（<1e-6）在决策卡/网格风控/K线头部被截断为 0
- **严重度**：P1（交易员看到「0.000000」的入场价/止损价，无法核对 AI 挂单）
- **位置**：
  - `web/src/components/trader/DecisionCard.tsx:40-45`（本地 `formatPrice`，最低 6 位小数）
  - `web/src/components/strategy/GridRiskPanel.tsx:99-108`（同样的本地 `formatPrice`）
  - `web/src/components/charts/AdvancedChart.tsx:1211-1235`（头部现价 `maximumFractionDigits: 2`、H/L `toFixed(2)`）
- **触发场景**：系统自述支持 meme 币（`web/src/utils/format.ts:9` 以 CHEEMS 0.000000166 为例）。该价格在 DecisionCard 的入场/止损/止盈格显示 `0.000000`（toFixed(6)），在 K 线头部现价显示 `0.00`。而仓库专门写了自适应精度的 `utils/format.ts`（15 位小数），这几处没有复用、各自重写且档位不足。
- **证据**：
  ```ts
  // DecisionCard.tsx:40-45
  function formatPrice(price: number | undefined): string {
    if (!price || price === 0) return '-'
    if (price >= 1000) return price.toFixed(2)
    if (price >= 1) return price.toFixed(4)
    return price.toFixed(6)          // 1e-7 → "0.000000"
  }
  ```
  ```tsx
  // AdvancedChart.tsx:1211-1215
  {marketStats.price.toLocaleString(undefined, {
    minimumFractionDigits: 2,
    maximumFractionDigits: exchange === 'forex' || exchange === 'metals' ? 4 : 2,
  })}                                // 1e-7 → "0.00"
  ```
- **修复建议**：三处全部改用 `utils/format.ts` 的 `formatPrice`；`AdvancedChart` 图例已有的 `formatLegendPrice` 也应把 `<0.01 → 6 位` 一档扩展为按数量级取有效数字（对齐 utils 版本）。

### A3 | P2 | equity-history 时间戳不带时区，前端按本地时区解析，时间轴整体错位
- **严重度**：P2（展示错误，且是竞赛/对比页的数据源）
- **位置**：后端 `api/handler_competition.go:178`（`handleEquityHistory`，batch 端点同构）；前端 `web/src/components/charts/EquityChart.tsx:187`、`ComparisonChart.tsx:115-119`
- **触发场景**：快照以 UTC 存库（`store/equity.go:52-55` 强制 UTC），序列化为 `"2006-01-02 15:04:05"`（无 Z/偏移）。前端 `new Date("2026-10-28 08:39:00")` 按浏览器本地时区解析——UTC+8 用户的权益曲线 X 轴全部慢 8 小时，跨日切点错误。
- **证据**：
  ```go
  // store/equity.go:53  UTC
  snapshot.Timestamp = time.Now().UTC()
  // handler_competition.go:178  无时区格式化
  Timestamp: snap.Timestamp.Format("2006-01-02 15:04:05"),
  ```
  ```ts
  // EquityChart.tsx:187  按本地解析
  time: new Date(point.timestamp).toLocaleTimeString('zh-CN', {...})
  ```
  对比：决策记录时间戳走 Go 默认 RFC3339（带时区），无此问题——仅 equity 端点中招。
- **修复建议**：后端输出 `snap.Timestamp.UTC().Format(time.RFC3339)`（一次性改两个端点，向后兼容解析）。

### A4 | P2 | 持仓历史：底部「总盈亏」用毛额，与行内净额口径不一致
- **严重度**：P2
- **位置**：`web/src/components/trader/PositionHistory.tsx:455-458`（合计）、`:216-219`（行显示 `grossPnl - fee`）
- **触发场景**：筛选后底部显示的「合计 Total PnL」是 `Σ realized_pnl`（毛），而每行 PnL 列是净额——两者对不上，差额恰为筛选集手续费。
- **证据**：`const totalFilteredPnl = filteredAndSortedPositions.reduce((sum, p) => sum + (p.realized_pnl || 0), 0)` vs 行内 `const realizedPnl = grossPnl - (position.fee || 0)`。
- **修复建议**：合计改为 `Σ(realized_pnl − fee)`，与行、与 stats 卡同口径。

### A5 | P2 | 排序比较器 `pnl_pct` 忽略 SHORT 方向（当前为不可达死代码）
- **严重度**：P2（潜在）
- **位置**：`web/src/components/trader/PositionHistory.tsx:387-393`
- **触发场景**：`sortBy='pnl_pct'` 时对空头用 `(exit−entry)/entry` 计算——空头盈利单得到负百分比，排序完全颠倒。当前 UI 下拉只有 time/pnl 两个维度（`:704-721`），该分支不可达；但 `sortBy` 状态类型含 `'pnl_pct'`，未来一加选项即触发。
- **证据**：`const aPct = (((a.exit_price || 0) - aPrice) / aPrice) * 100`（未按 side 反向；行渲染处 `:239-243` 是正确处理了的）。
- **修复建议**：按 side 取反，或直接删掉死分支。

### A6 | P2 | App 每 30s 轮询 `/api/statistics`，但数据从未被渲染
- **严重度**：P2（无意义请求负载）
- **位置**：`web/src/App.tsx:367-377`；`web/src/pages/TraderDashboardPage.tsx:142`（仅出现在 props 类型里，解构与渲染均未使用）
- **触发场景**：每个打开 dashboard 的用户每 30s 请求一次 statistics，响应丢弃。
- **修复建议**：删除该 useSWR 或真正把周期统计渲染出来。

### A7 | P2 | `tradersConfigStore` 用 `e.id === 'aster'` 判断交易所类型——永假死分支
- **严重度**：P2
- **位置**：`web/src/stores/tradersConfigStore.ts:50-63`
- **触发场景**：`allExchanges` 来自 `GET /api/exchanges`（SafeExchangeConfig），`id` 是 UUID、且响应不含 `apiKey`（见 `api/handler_exchange.go:106-132`）。因此 `'aster'/'hyperliquid'` 分支永不命中，`configuredExchanges` 实际退化为「enabled 的账户」：已填好 Aster/Hyperliquid 凭证但未启用的账户在「已配置」列表中消失。
- **证据**：`if (e.id === 'aster') { return e.asterUser && ... }` —— 应为 `e.exchange_type`。
- **修复建议**：改用 `exchange_type` 判断；顺带确认 `e.apiKey` 条件可删（后端永不返回）。

### A8 | P2 | 手动平仓兜底 `exitPrice = quantity * 100` 会把假价格写成 FILLED 成交
- **严重度**：P2（特定交易所下污染历史盈亏）
- **位置**：`api/handler_trader_status.go:317-323`（`recordClosePositionOrder`）
- **触发场景**：OrderSync 白名单（binance/lighter/hyperliquid/bybit/okx/bitget/aster/gate）之外的交易所（paper、kucoin、indodax、binance_stocks）走此路径。用户点「平仓」时若 `GetPositions()` 失败（`:221-224` 错误仅日志、继续执行）→ `entryPrice=0` → `exitPrice = posQty*100` 被当作成交价写入 order+fill（`Status: "FILLED"`, `RealizedPnL: 0`，手续费按"Lighter 0.04%"估算套用到 kucoin/paper）。持仓历史/图表标记由此得到完全虚构的平仓价。
- **证据**：
  ```go
  if exitPrice == 0 {
      exitPrice = quantity * 100 // Rough estimate if we don't have price
  }
  fee := exitPrice * quantity * 0.0004
  ```
- **修复建议**：拿不到真实成交价时应查询订单状态或等待 OrderSync，而不是写假数据；至少跳过记录并提示。

### A9 | P2 | EquityChart 初始余额回退链引用不存在的字段 `point.pnl`，最终落到虚构的 1000
- **严重度**：P2
- **位置**：`web/src/components/charts/EquityChart.tsx:35-41, 174-180`
- **触发场景**：`/api/account` 的 `initial_balance` 为 0 时（`handleUpdateTrader` 换绑交易所会把 initial_balance 重置为 0，`api/handler_trader.go:744-752`；或创建时交易所余额探测失败、用户填 0），回退到 `validHistory[0].total_equity - validHistory[0].pnl`——但后端 equity 端点根本没有 `pnl` 字段（只有 `total_pnl`，见 `handler_competition.go:148-156`），表达式为 `NaN`（falsy）→ 静默落到 `1000` 兜底 → 整条曲线的 PnL/PnL% 以虚构 1000 USDT 为基线。
- **证据**：`const pnlPct = ((pnl / initialBalance) * 100).toFixed(2)`，`pnl = point.total_equity - initialBalance`。
- **修复建议**：改用 `total_pnl` 字段；且 1000 兜底应去掉（显示「无基线」而不是编数字）。

### A10 | P2 | Sharpe 指标数值与 Tooltip 公式口径不符（per-trade 金额 SNR 冒充 Sharpe）
- **严重度**：P2（指标定义误导）
- **位置**：计算 `store/position_query.go:234-256`；展示 `web/src/components/trader/PositionHistory.tsx:562-575` + `web/src/components/common/MetricTooltip.tsx:45-54`
- **触发场景**：后端「Sharpe」= 每笔平仓净盈亏（USDT 金额，非收益率）的 mean/std，无风险利率为 0、未年化、量纲是金额。Tooltip 却向用户展示 `SR = (r̄ − r_f)/σ`（收益率口径）。用户拿它和外部年化 Sharpe 对比必然得出错误结论（per-trade SNR≈1.2 看起来像很差的年化 Sharpe，或反之）。
- **证据**：`return mean / stdDev`（pnls 为 `pos.RealizedPnL - pos.Fee` 金额序列）。
- **修复建议**：改名「每笔期望/波动比（per-trade expectancy ratio）」，或真正基于 equity 曲线算日收益年化 Sharpe；Tooltip 公式同步修正。

### A11 | P2 | 持仓浮盈百分比失败/缺失时显示 0.00%，且未说明是「按保证金的 ROE」
- **严重度**：P2
- **位置**：`web/src/pages/TraderDashboardPage.tsx:515-517`；后端 `trader/auto_trader.go:948-953`、`trader/auto_trader_decision.go:238-239`
- **触发场景**：① `pos.unrealized_pnl_pct ?? 0`——字段缺失（类型上非可选却防御性 `?? 0`）时绿色/红色 0.00% 混入表格，与「无数据」不可区分。② 该值是 `unrealizedPnl / marginUsed × 100`（ROE），10x 杠杆 1% 的价格波动显示 10%，但表头 tooltip 只写「未实现盈亏」，与持仓历史页的价格收益率（`PositionHistory.tsx:237-244`）两套口径同名不同值。
- **修复建议**：缺失渲染 `—`；表头标注「ROE%（按保证金）」，与历史页「价格涨幅%」区分命名。

## B. 疑似问题（需进一步验证）

1. **「同步余额」会重置 PnL 基线**：`api/handler_trader_status.go:139`（`UpdateInitialBalance(actualBalance)`）→ `trader/auto_trader_decision.go:174`（`total_pnl = equity − initialBalance`）与 EquityChart 基线、`GetFullStats` 的 MaxDD 分母全部重置。点一次「同步余额」，仪表盘历史盈亏清零、历史回撤占比突变；同步前的入金会被计为盈利。资金页（FundsPage/Modified Dietz）口径正确，但 dashboard 没有。**需验证**：产品是否接受「initial_balance 即当前基线」的语义；若是，至少应在同步时提示影响。
2. **平仓净盈亏可能不含资金费**：持仓历史 `net = realized_pnl − fee`，`fee` 来自 fills 的 commission。若 OrderSync 未把 Binance 资金费结算写进 `realized_pnl/fee`，长期持仓的每笔净盈亏会系统性偏差。**需验证**：`store` 的 OrderSync 写入逻辑中 funding 结算行的归类（本子任务未覆盖 OrderSync）。
3. **MaxDrawdown 只看已平仓序列**（`store/position_query.go:260-286`）：浮亏峰值（扛单期间的权益低点）不计入，回撤被低估；且 `initialEquity<=0` 时回退 100 USDT 基准。**需验证**：equity 快照是否足以改成基于权益曲线的 DD（数据已有，10000 点）。
4. **复盘日志行显示毛盈亏**：`web/src/pages/ReviewPage.tsx:416,550` 直接渲染 `entry.realized_pnl`（毛），而同页 win_rate/expectancy 用净（`store/journal.go:494-498`）。与持仓历史页（净）跨页不一致。
5. **token 存 localStorage**（`web/src/lib/httpClient.ts:81`）：XSS 下可被窃取；self-hosted 单机部署风险中等，公网部署（Railway 配置存在）风险升高。备选：httpOnly cookie 或至少加短过期。
6. **`handlePositionHistory` 吞掉统计错误**（`api/handler_order.go:298-304` 三个 `_`）：stats 查询失败时前端统计卡整体静默消失，无任何错误态。同理 `EquityChart` 用 `total_equity > 1` 静默丢点（`:120`）。
7. **`GetFullStats`/`GetSymbolStats`/`GetDirectionStats` 每次全表加载**：30s 轮询 × 每个打开历史页的用户 × 全量平仓单（无 LIMIT、Go 内聚合）。千笔以上历史后是可感知的 DB/CPU 负载。**需验证**：实际数据量级。

## C. 可复用的成熟方案（替代自写轮子）

| 现状 | 建议 | 收益 / 成本 / 风险 |
|---|---|---|
| 策略配置表单 40+ 数字字段全是裸 `<Input type="number">`，仅靠 HTML `min/max` + 后端 400 英文报错（`RiskControlEditor.tsx`） | **zod + react-hook-form**（或仅 zod 做提交前校验），schema 与后端 `validateBoundsAndLadder` 镜像 | 收益：单位错误（% vs 小数、风险 30）提交前拦截，可做跨字段约束（如 `tp_full_at_r > tp_trim_at_r` 目前仅后端拦）；成本低（新增 2 依赖）；风险低。注意：**后端校验本身已较完整**（`store/strategy_validation.go:89-120`，`risk_per_trade_pct≤10`），这是前端体验补强而非填补空门 |
| 4 份价格格式化实现（`utils/format.ts` 正确版、DecisionCard、GridRiskPanel、AdvancedChart 头部） | 统一 `utils/format.ts`；不足处引入 `Intl.NumberFormat` + 有效数字逻辑 | 直接消灭 A2；零新增依赖 |
| `GridRiskPanel`（裸 fetch）、`PaperPromotionPage`（裸 fetch）、`ChartTabs` 的 symbols 请求绕过 `httpClient` | 全部走 `httpClient` | 统一 401 处理/silent 错误/toast；否则这些页面 401 时行为不一致 |
| `date-fns` 已在依赖中，但 `PositionHistory` 手写 `formatDuration/formatDate`、AdvancedChart 手写时间格式 | 用 date-fns `format`/`formatDistanceToNow` + `Intl` 时区 | 顺带修复 A3 的解析侧；成本极低 |
| 前端指标 `utils/indicators.ts`（SMA/EMA/BB 三函数，仅图表展示） | **自写在此合理**（量小、纯展示、有测试场景）；若扩 RSI/MACD/ATR 再上 `technicalindicators` | 不为换而换 |
| 图表选型：K 线用 `lightweight-charts`、统计图用 `recharts` | **选型正确，无需替换**；`ChartWithOrders.tsx`/`ChartWithOrdersSimple.tsx`（约 840 行）无引用，属死代码应删除 | 减维护面 |
| 绩效指标（Sharpe/PF/ expectancy/MaxDD）后端自写 | 前端展示层维持现状；指标层建议方法论对齐 quantstats/empyrical 的命名与年化口径（见 D1），Go 侧可参考 `github.com/shopspring/decimal` 处理资金精度、不必引入整套库 | 低成本高可信度 |

## D. 优化建议（按预期收益排序）

1. **PnL 口径单一事实来源**（对应 A1/A4/B4）：后端在所有聚合/明细响应中直接给 `net` 字段，前端一律不许自己 `gross − fee`。本次确认的两处口径 bug 都源于「前端自己算」。
2. **修时区链路**（A3）：equity 端点输出 RFC3339 UTC；前端统一走 FundsPage 已有的 `fmtTimeTz` 模式（那是全仓库唯一正确处理时区的页面，值得抽成公共 util）。
3. **统计查询下推 SQL + 缓存**（B7）：`GetFullStats/GetSymbol/GetDirection` 一次全表三连查，改聚合 SQL 或 60s 进程内缓存，配合前端 30s 轮询足够。
4. **指标命名与文档对齐**（A10/B3）：区分 per-trade expectancy、价格收益率、ROE 三套口径并在 UI 命名上显式化（现在三处都叫「盈亏%」）。
5. **dashboard 权益曲线接入转账标记**：fundsledger 数据已存在（FundsPage 用了），dashboard 的 EquityChart 至少画出转账日参考线，避免入金被读成盈利（与 B1 同根）。
6. **错误态与 0 值区分**（A11/B6）：所有 `?? 0` 渲染数值的地方改为 `—`；被吞的 stats 错误向前端返回部分错误标记。
7. **死代码/死请求清理**（A6/A7/C 表）：`ChartWithOrders*`、`stats` 轮询、store 死分支。
8. **表单体验**（C 表第一行）：zod 镜像后端校验 + 中文单位提示，危险参数（杠杆/风险%）保存前二次确认弹窗。

## E. 覆盖文件清单与未覆盖部分

**逐行读过（前端）**：`utils/format.ts`、`utils/indicators.ts`(引用检查)、`lib/httpClient.ts`、`lib/api/{helpers,data,traders,config,review(部分)}.ts`、`App.tsx`、`pages/TraderDashboardPage.tsx`、`pages/FundsPage.tsx`、`pages/PaperPromotionPage.tsx`、`components/trader/{PositionHistory,DecisionCard,RiskStatusPanel,utils,TradersList(部分)}.tsx`、`components/charts/{EquityChart,AdvancedChart,ChartTabs,ComparisonChart(部分),mergeKlines.test}.ts*`、`components/strategy/{RiskControlEditor,GridRiskPanel,GridConfigEditor(单位相关)}.tsx`、`components/ui/pnl.tsx`、`components/factorMining/format.ts`、`stores/tradersConfigStore.ts`、`types/{trading,config(部分)}.ts`、`components/common/MetricTooltip.tsx`。
**逐行读过（后端）**：`api/handler_trader.go`、`api/handler_trader_status.go`、`api/handler_order.go`、`api/server.go`(路由段)、`api/handler_competition.go`(equity/statistics 段)、`api/handler_factor_mining.go`、`api/handler_exchange.go`(安全段)、`api/open_orders_cache.go`、`api/request_limits.go`、`api/handler_klines.go`、`store/position_query.go`、`store/strategy_validation.go`、`store/journal.go`(口径段)、`store/equity.go`(引用检查)、`trader/auto_trader_decision.go`(账户/持仓段)。

**未覆盖/浅覆盖**：`pages/ReviewPage.tsx`（1855 行，只核查了盈亏口径行）、`pages/{DataPage,ResearchLogPage,ResearchHubPage,StrategyStudioPage,SettingsPage,SystemQualityPage,LandingPage}.tsx`、`components/trader/{AITradersPage,CompetitionPage,ExchangeConfigModal,TraderConfigModal,TraderConfigViewModal,ModelConfigModal,PositionHistory 之外的编辑器}.tsx`、`components/strategy/{StockConfigEditor,IndicatorEditor,CoinSourceEditor,VersionsPanel,PromptSectionsEditor,GridConfigEditor 其余部分}.tsx`、`components/factorMining/{FactorTable,FactorDetail,EnsembleCard,RunsTable}.tsx`（其指标由 Python 侧生成，本子任务未审 Python）、`api/handler_review.go` 的 AI 提示词段、`api/handler_funds.go`/fundsledger、OrderSync 与 fills 写入链路（B2 结论依赖它）、mcp/store 其余模块。前端类型检查与单测因缺 `node_modules` 未运行。
