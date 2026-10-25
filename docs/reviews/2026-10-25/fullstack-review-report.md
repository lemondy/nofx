# 2026-10-25 第六轮全链审查报告（前后端 / 加密 + 美股 / prompt 数据 / 风控）

基于 `HEAD c8c59876`（已验证实盘进程 PID 9874 运行即该构建，含当日全部提交：pubcache、fapi v3、rules_cache、netmeter wire-bytes、SQLite busy 修复）。全程只读审查，未修改源码、未调用交易接口。

**范围**：今日合入批次全量精读（pubcache / fapi v3 / exchangeInfo 缓存 / -1013 自愈 / netmeter）+ 近两周修复批验证（exitmeta 重试、-1121 隔离、早平锁盈利豁免、auth 自动恢复、SQLite busy）+ 风控/执行/prompt 数据/前端抽查。kernel 候选池与扫描器等既往多轮深审区域做抽查复核（见文末"已验证无问题"）。

**方法说明**：子代理通道因账户并发配额全部失败，本轮为人工逐模块审查。

**结论**：近期批次工程质量普遍很高，未发现会直接导致错单或资金损失的执行错误。确认 **1 项 P1（今日 pubcache 提交引入的回归，已把 trader 包测试打红）、2 项 P2（值得尽快处理）、1 项 P2 口径、4 项 P3**。

---

## 发现清单

### 1. [P1] pubcache 提交破坏测试封闭性；netmeter 覆盖 CoinAnk 流量依赖未指明的包初始化顺序

位置：`provider/coinank/coinank_api/kline.go:68`

```go
var client = &http.Client{
	Timeout:   30 * time.Second,
	Transport: pubcache.Wrap(nil),
}
```

`pubcache.Wrap(nil)` 在**包初始化时**捕获 `http.DefaultTransport` 当时的值。三重影响：

- **`go test ./trader/` 失败**：`TestB1BreakevenTrailingSameCycle`（`trader/review_batch1_20261007_test.go:230`）5 个子测试全挂，报 `cycle did not read the mocked candles: requests=0`。该测试在运行时热替换 `http.DefaultTransport` 注入内存 K 线，而 CoinAnk 客户端已持有初始化时捕获的真实 transport，mock 不再被拦截。测试套件红会掩盖未来真实回归。
- **测试可能真发网络请求**：mock 失效后，测试内的 K 线拉取走真实 transport 直连 api.coinank.com，封闭性破坏。
- **生产上 netmeter 对 CoinAnk 的计数变脆弱**：是否被计数取决于 `netmeter.init()`（换 DefaultTransport）与 coinank_api 包变量初始化谁先执行；Go 规范对独立包初始化顺序未指明，当前靠包路径字典序侥幸成立。CoinAnk 是流量大户（pubcache 提交信息实测 25 分钟 14.9MB），顺序一旦变化，设置页流量监控会静默漏计。

修复（几行，晚绑定）：

```go
type lateBindTransport struct{}
func (lateBindTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(r) // 请求时解析：生产=netmeter 包装后，测试=mock
}
var client = &http.Client{Timeout: 30 * time.Second, Transport: pubcache.Wrap(lateBindTransport{})}
```

### 2. [P2] fapi v3 对账：6h 复核窗口内 v2 基线持续失败时，好的 v3 数据整体报错

位置：`trader/binance/account_v3.go:393-396`（持仓）、`:503-506`（账户）。

check 路径上 v3 成功、v2 基线失败 → 直接返回错误（fail-closed）。v2 是被 v3 取代的旧接口；若 Binance 将来对 v2 限流或下线，这里成为每 6 小时一次的硬失败点（非 check 的稳态路径不受影响，v3 仍可用直到自身出错）。

建议：v2 基线失败时跳过本轮对账、仍采用 v3 并告警——对账是防 v3 出错的保险，不是数据必要来源。

### 3. [P2] decision_records 686.7MB（占 data.db 734MB 的 93%），无任何保留/清理机制

实测（只读 dbstat）：decision_records 686.7MB，其余全部表合计 <45MB。每周期每 trader 落全量 `input_prompt`（约 150KB/行），全仓 `prune|retention|purge` 对该表零命中。

读取方只有 API 决策面板/候选回看；统计类（NEGATIVE_EDGE/胜率/R 分布）全部走 journal 与 entry_assessments——**修剪旧 input_prompt 不影响任何交易逻辑**。

建议：保留 30–60 天全量，更早的行删除 `input_prompt`/decisions 大列或整行；配合增量 VACUUM 回收空间（734MB 库做全量 VACUUM 需停机窗口）。

### 4. [P2-口径] pubcache 资金费 TTL 10min 超过调用方缓存 5min

位置：`market/pubcache/cache.go:35`（`FundingTTL = 10 * time.Minute`）。提交说明"新鲜度语义与改造前完全一致"不成立：调用方解析后缓存 5 分钟，过期后可能从 pubcache 拿到最多 10 分钟旧的 funding（改造前上限 5 分钟）。实际影响可忽略（资金费 8h 结算周期），但要么更正声明，要么把 FundingTTL 降到 ≤5min 对齐语义。

### 5. [P3] futures PlaceLimitOrder 的 ReduceOnly=true 死分支

位置：`trader/binance/futures_orders.go:987-991`。注释称"1R profit-lock trims"，但全仓已无调用方设 `ReduceOnly: true`（1R 减仓现走市价平仓门；hyperliquid/paper 的 true 用法是各自场所的正确语义）。且该分支 side→positionSide 映射（SELL→SHORT）在 Hedge Mode 下是**开仓**组合，若被未来代码调用必重演 -1106（2026-09-22 事故语义）。建议删除死分支或改为显式平仓语义构造，并更正注释。

### 6. [P3] pubcache K 线收盘边界响应永不缓存（纯性能）

位置：`market/pubcache/cache.go:395`：`if !closeAt.Before(started) && closeAt.Before(expires)`。恰在 fetch 开始时刻收盘的 bar（closeAt == started）使 expires=started → 整个响应不入缓存；整点对齐调度下该边界并不罕见。修正为 `closeAt.After(started)` 即可（该 bar 已是终值，可正常缓存满 TTL）。

### 7. [P3] rules_cache 磁盘文件按 baseURL 互覆

`trader/binance/rules_cache.go:30` 与 `trader/binance_bstock/rules_cache.go:35`：单文件只存一个 baseURL，testnet 与 mainnet 并存时每次写互相覆盖、对方读取即弃——退化为无磁盘缓存（无正确性影响）。bstock 的 `writeRulesFile` 还缺 `f.Sync()`（futures 版有），崩溃后仅损失缓存需重拉。

---

## 已验证无问题（按模块）

- **pubcache（除发现 1/4/6）**：凭据头（x-mbx-apikey/authorization/signature 等）/签名查询参数/POST/非白名单路径全部绕过，跨 trader 只共享公开行情，无私有数据可串；只缓存 200+合法 JSON（HTTP 200 错误信封、坏 JSON 不入缓存）；forming K 线不跨收盘被当终值；TTL 从 fetch 起算不被命中延长；singleflight 无泄漏、字节记账一致；CoinAnk interval 为 `"15m"` 后缀格式与 `candleEnd` 解析匹配；15s 游标对齐（`time.Now().Truncate`）让多 trader 命中同 key。
- **netmeter**：gzip 计量=压缩后字节+透明解压，与 net/http 语义一致（仅当 Accept-Encoding 归我方所有时接管）；上行含请求行 path+query；路径聚合基数封顶 200+溢出桶、Telegram bot token 剥离在截断之前；`Wrap` 自动解套防双计。
- **fapi v3（除发现 2）**：v2/v3 并发对账逐字段容差合理（强平价 0.2% 容全仓联动、PnL 容差按实际 mark 移动推导，防大额 PnL 错误被固定容差掩盖）；冷却分类（瞬时/网络/限流 5min，语义/对账不一致 6h）正确；v2 回退成功时抑制 v3 认证错误，避免代理双出口 IP 误触 authBlocked；symbolConfig 取不到 leverage 整体回退 v2、不发半快照；leverage 与 v2 基线精确比对；锁顺序（positionsCacheMutex→positionMu→configMu）有注释防护；GetBalance/GetPositions 消费字段与 v3 合成对象完全匹配，无数据形状分叉。
- **rules_cache（futures+bstock）**：tmp+rename 原子写盘、7 天 stale grace、失败 5min 退避；-1013 按符号 10min 节流失效+本单拒绝下次自愈（不会重复下单）；bstock 单符号 lookup 避免每次全量 17.8MB、负缓存 10min；深拷贝防共享 map 污染。
- **SQLite busy 三根因修复已全部落地**（2026-10-25 事故）：`_txlock=immediate` + `_busy_timeout=15000` 走每连接 DSN + WAL；`retryOnceOnBusy` 覆盖 stock_protection 4 处 + stock_paper 1 处（幂等增量记账、重放安全）；`store/gorm.go` 注释记录"所有 db.Transaction 均为短纯 SQL 块"的审计结论。
- **-1121 隔离**（`trader/binance/order_sync.go`）：24h 隔离+到期单次重探（relist 场景）、内存态重启后单查询重学，终结每 30 秒死循环烧权重；配合 exitmeta 指数退避+哨兵（亚分钟持仓/-1121/超龄不再无限重试）。
- **早平锁盈利豁免**（363ff5fd）：R 锚 ≥0.5R、无锚时价格浮盈 ≥1.5% 兜底；执行侧用实时 mark 复核；min-hold 锁独立保留（`prompt_compression_test.go` 钉死）；亏损/微利行为不变。
- **auth 自动恢复**（1e274abe）：generation 计数防陈旧成功清锁、只读 GetBalance 探测（无交易所变更）、1/2/5/10min 退避+30min 告警节流。
- **风控核心数学**：sizing 从止损反推（equity×risk%÷止损距离%，含往返成本 bps）+ notional 硬顶（BTC/ETH 5x、山寨 1x 默认）双保险；保证金预算含挂单预留、pending 状态未知时 fail-closed（Inf）；max_positions `>` 语义配合 post-open 计数（PROMUSDT 修复仍生效）；点差门 fail-open 带日志；回撤保护 5/55 的 R 模式与 ROE 模式统一。
- **factor_ensemble**（7bbb7d4a/ebbd6dfe）：纯参考层、无任何 gate 消费；逐字段容错解码、NaN/Inf/范围检查、8MiB 上限、3 天过期、确定性排序——不会污染决策数据。
- **指标**：RSI/ATR 标准 Wilder 实现；ATR14 closed-bars-only 与执行侧口径一致（09-28 修复仍生效）。
- **前端**：轮询无 latch 停摆模式（失败仅设标志、interval 永续重试，全 web/src 无"N 次失败后停轮询"模式）；策略保存 15s AbortController + 乐观锁 409（09-14 sl_min 静默回滚路径已闭合）；httpClient 30s 超时。
- **执行链路**：Hedge Mode 下全仓无任何 ReduceOnly+quantity 组合的实际调用（-1106 教训保持）；限价锚点/网格单均为正确开仓组合；`formatQuantityWithRules` 恒向下取整（不会放大风险预算）；-1013 失效挂钩 futures 8 处 + bstock 5 处调用点。

## 风控覆盖评估（缺口供拍板，非 bug）

已覆盖：止损带双尺（ATR1h floor / max(2×ATR4h,8%) cap）、RR 门、sizing 双保险、保证金预算（含挂单预留）、max_positions、连亏熔断、回撤保护 5/55、点差门、pump guard、市价例外证据复核、早平门+盈利豁免、保护单看门狗+缺腿告警、孤儿对账收口、auth 阻断自动恢复。

仍缺（按风险排序）：

1. **同方向相关性聚集无门**：max_positions 各仓独立计数，极端同向（如多个 AI 币同时多）时组合回撤=单仓×N，保证金预算是唯一总量约束。建议至少在 prompt 侧标注组合方向敞口供模型自察。
2. API 长时间失败窗口内，持仓只剩交易所侧 algo 单保护（watchdog 同样依赖 API）——现有兜底=SL/TP 常驻交易所，属可接受设计，知悉即可。

## 测试状态

`go build ./...` 通过；netmeter / pubcache / binance / kernel / store 全部 ok；trader 包仅 `TestB1BreakevenTrailingSameCycle` 失败（即发现 1）。

## 建议动作（优先级）

1. 修发现 1（晚绑定 transport，几行改动 + 重跑 `go test ./trader/`）——唯一需要改代码的项；
2. 拍板 decision_records 保留策略（建议 30–60 天）并实施；
3. 拍板 fapi v3 的 v2 基线失败降级策略；
4. P3 四项可顺手修（死分支、边界缓存条件、缓存文件名带 baseURL、bstock 补 fsync）。
