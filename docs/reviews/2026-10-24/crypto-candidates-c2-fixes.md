# Crypto candidates C2 修复记录

范围：`crypto-candidates.md` 第 **1、2、3、11** 项。四项均先读代码复核，确认成立后修复；未修改审查报告原文。

## 逐项结果

### 1. 已修复：空候选不再跳过持仓管理

- 原代码在调用模型前仅按候选数量返回，确实会丢掉现有持仓的模型管理周期。
- 现在仅在候选、实际持仓、待处理 pending 均为空时跳过；候选源失败仍保留原日志，继续使用空候选。
- 持仓或 pending 尚在时继续模型周期。额外明确过滤四种开仓动作，包括现仓加仓；原因是既有 `cycleEntryPool` 会把 AI 管理的持仓加入入场集合，单靠空候选不能保证禁止新增风险。平仓、hold、保护调整仍走原执行路径。
- 抽出带决策回调的周期内部入口，生产入口仍调用原来的 `GetFullDecisionWithStrategy`，回归测试无需模型或行情网络请求。

### 2. 已修复：排除名单覆盖存量入场挂单

- 原 pending 路径确实没有排除名单检查。
- 对名单项和 pending 符号先 `TrimSpace` 再 `market.Normalize`，覆盖大小写、分隔符、首尾空白。
- 恢复/登记统一入口 `setPendingEntry`、周期和快速保护生命周期、`pendingDirectionBlocked` 均检查排除名单。排除检查先于方向行情查询；已知订单即使终态回执暂不可读，也先尝试取消该入场订单。
- 复用既有 `cancelPending`：仅按入场订单 ID 取消剩余量，复查终态、核验累计成交量保护后才释放所有权；回执、终态或保护无法确认时保留 pending 重试。命中排除名单本身不会触发清仓，也不会清除已成交量的管理标记或保护。
- UNKNOWN 提交仍须先解析出真实订单 ID，未解析时保留跟踪，沿用原有保守逻辑。

### 3. 已修复：CoinAnk 异常行返回 error 而非 panic

- 免费 REST 原代码对每行直接索引 `[0]` 至 `[8]`，`data:[[]]` 可复现越界 panic。
- grep 同类索引后，另确认付费 REST 和 WebSocket 的 K 线解析存在相同短行风险，一并修复。
- 两个 REST 入口共用 `ParseKlineRows`：检查至少九列、数值有限、时间非负且可安全转换、开始时间不晚于结束时间、行间开始/结束时间严格递增；异常时不返回部分有效序列。
- 数据错误统一可通过 `errors.Is(err, coinank.ErrInvalidKlineData)` 识别，JSON 错误同时保留原错误链。现有 `getKlinesWithFallback` 已会对 error/空结果执行 Binance 回退，因此无需修改 `market/`。
- WebSocket 跳过不足九列的消息，正常消息继续处理；未对同一根蜡烛的重复更新施加 REST 的时间递增约束。

### 11. 已修复：入场和 pending 共用 breakdown 顶确认豁免

- 原入场路径对 breakdown 不传顶部确认限制，pending 却对所有未确认 short 撤单，规则确实矛盾。
- `ShortScanNeedsTopConfirmation` 统一规则；pending 使用当前 short-scan 快照中该币的 `Universe`。breakdown 不因 `Confirmed=false` 被撤，gainer、near_high 和未知家族继续沿用原顶确认要求。
- 没有新增数据库字段、表或索引，未改变 AutoMigrate；无需迁移现存 pending 行。
- 方向、数据质量、账户风险等其他检查保持原逻辑，breakdown 豁免仅作用于顶确认。

## 改动文件

生产代码：

- `trader/auto_trader_loop.go`
- `trader/auto_trader_pending.go`
- `trader/auto_trader_pending_risk.go`
- `kernel/engine_prompt.go`（仅共享 breakdown 顶确认规则）
- `provider/coinank/kline.go`
- `provider/coinank/coinank_api/kline.go`
- `provider/coinank/coinank_api/kline_ws.go`

新增测试（12 个命名测试，另含表驱动场景）：

- `trader/auto_trader_loop_c2_test.go`：空源/失败源/全排除时仍管理持仓；空闲周期跳过、pending 周期继续；市价/限价模式下禁止四种开仓动作及现仓加仓。
- `trader/auto_trader_pending_c2_test.go`：排除检查优先于方向/行情；周期和快速检查撤单；未成交/部分成交；终态未确认、保护未确认、回执不可读时保留所有权；从 SQLite shadow row 恢复；breakdown 与其他家族的顶确认差异。
- `provider/coinank/kline_test.go`：行长、各列 NaN/正负 Inf、时间范围和顺序、有效行字段映射、付费 REST 短行。
- `provider/coinank/coinank_api/kline_validation_test.go`：免费 REST 短行/时间异常/数值溢出及可识别错误；WebSocket 略过短行后仍输出有效消息。

## 回归与验收

所有 Go 命令均在本 worktree 运行，使用 `GOCACHE=$PWD/.gocache GOPROXY=off`。

回归红灯验证：

- 保留测试入口，仅恢复第 1、2、11 项原缺陷逻辑后，空候选持仓/挂单管理、排除名单直接检查/撤单/恢复、breakdown 用例均失败；随后恢复修复代码。
- 免费 REST 在原解析器上运行短行用例，复现 `index out of range [0] with length 0`。
- 单独移除“空候选禁止开仓”过滤时，管理周期用例进入开仓行情查询并失败，确认测试不会被其他风险门掩盖。

验收结果：

| 检查 | 结果 |
| --- | --- |
| `go build ./...` | 通过 |
| `go vet ./trader ./kernel ./provider/coinank/...` | 通过 |
| `go test ./trader ./kernel ./provider/coinank/... -count=1` | trader 通过（6.899s）、kernel 通过（85.395s）、coinank 根包通过；coinank_api 的 7 个既有联网测试受环境限制失败 |
| 排除这 7 个既有联网测试后，`go test ./trader ./provider/coinank/... -count=1` | 通过；最后一次 trader 11.467s，coinank 0.452s，coinank_api 0.392s |
| `git diff --check` | 通过 |

最后一次离线测试命令：

```sh
GOCACHE=$PWD/.gocache GOPROXY=off go test ./trader ./provider/coinank/... \
  -skip '^(TestBaseCoinSymbolsNoArgs|TestBaseCoinSymbolsBTC|TestBaseCoinSymbolsBTCUSDT|TestDepthWs|TestKline|TestKlineDaily|TestKlineWs)$' \
  -count=1
```

## 局限与操作边界

- 完整测试命令并非全绿：上述 7 个旧用例直接访问外部 CoinAnk，HTTP 请求被沙箱拒绝连接本机代理 `127.0.0.1:7890`，WebSocket 请求无法解析 `ws.coinank.com`。本次未发生 httptest 监听端口 panic，也未改动或永久跳过这些旧用例。
- 新增测试仅用 mock 交易适配器、内存 HTTP transport、消息通道和临时 SQLite；未调用真实下单、撤单或持仓接口。Binance 实际网络回退和交易所撤单/保护行为未做实盘验证。
- 第 11 项按当前扫描快照判断信号家族，不持久化历史家族；快照没有该币时保留原先“未匹配则不追加顶确认拦截”的行为，其他方向和保护检查仍执行。
- 未改 `kernel/engine.go`、`market/`、Vergex、Hyperliquid；未 git commit/push，未写 `/Users/zhangyun/workspace/nofx/data`。
