# 薄盘口 / 异常波动币风控

本轮（2026-10-25）将价格/OI 入场闸改为短窗口，并按用户提供的 Binance 60 天回放摘要调整默认值。不改既有持仓快速保护、OI 仓位折扣、止损类型或盘口风控。未 commit/push。

## 默认值和开关

所有字段位于 `risk_control`，由 `Effective*` 访问器解析。阈值字段遵循 `0/未设 = 默认，负数 = 关闭该窗口`，NaN/Inf 回到安全默认值。**hold-off 例外：未设/null = 30 分钟，显式 0 或负数 = 关闭**；后端使用 `*float64` 保留未设与零的区别，前端留空恢复默认。已有配置中显式保存的正阈值保持原值，本轮不迁移策略数据。

| 字段 | 默认值 | 依据 / 作用 |
| --- | --- | --- |
| `min_quote_volume_24h_millions` | **5**（原 30，百万美元） | 只过滤死盘，不能防操控。30M 会长期挡掉真实候选 MYX/PIEVERSE/SAGA；pump 当日成交额暴增反而会放行。5M 仍会过滤一部分低成交额候选，不保证保留所有原候选。 |
| `vol_spike_5m_mult` | **8** | 5m 滚动振幅达到历史同窗中位数的 8 倍禁入。 |
| `vol_spike_15m_mult` | **6** | 15m 滚动振幅达到 6 倍禁入。 |
| `vol_spike_mult` | **5**（原 4） | 沿用旧字段，现用于 60m 滚动振幅；缺少短周期历史时才使用旧 1h 形成中 K 线算法。机器码仍叫 `1H`。 |
| `vol_spike_holdoff_minutes` | **30 分钟** | 任一价格/OI 异常后继续禁入，新的异常刷新起点；回放显示异常消退后的半小时仍有较高后续摆动。成交额不足本身不启动冷却。 |
| `oi_surge_15m_pct` | **8%** | 采样合约 OI 在 15m 内上涨或下降超过 8% 禁入，对操控币较有选择性。 |
| `oi_surge_1h_pct` | **15%**（原 30%） | 60m OI 绝对变化超过 15% 禁入，补充短窗口；旧 30% 对 IRYS 过迟钝。 |
| `thin_oi_full_size_millions` | `5 × EffectiveMinOIMillions()`，通常 75 | 原行为不变，足够 OI 才使用满仓尺寸。 |
| `thin_oi_min_size_factor` | 0.3 | 原行为不变，折扣 `clamp(OI / 满仓门槛, 0.3, 1)`。 |
| `fast_window_minutes` | 5 分钟，最高 120 | 原持仓快速保护窗口，负数关闭。 |
| `fast_adverse_atr_mult` / `fast_favorable_atr_mult` | 2 / 2 | 原逆向减仓、顺向保本阈值，使用闭合 1h ATR%。 |
| `fast_adverse_reduce_frac` | 0.5，最高 0.75 | 原减仓比例和累计上限不变。 |
| `fast_adverse_cooldown_minutes` | 30 分钟 | 原成功减仓冷却不变，与入场 hold-off 相互独立。 |
| `fast_fallback_pct` | 3% 价格变动 | 原 ATR 缺失降级阈值，不是 ROE。 |
| `thin_oi_mark_stop_millions` | `3 × EffectiveMinOIMillions()`，通常 45 | 原薄币 Binance 止损 MARK_PRICE 选择不变。 |
| `spread_gate_fail_closed_thin` | true | 原薄币/未知 OI 山寨币缺盘口拒单策略不变；显式 false 关闭。 |

BTCUSDT、ETHUSDT、bstock 豁免成交额、价格异常、OI 异常和入场 hold-off。按完整规范化符号匹配，ETHFI、BTCDOM 等不享受 BTC/ETH 豁免；bstock 原有可测宽价差闸仍然保留。

## 价格滚动窗口与数据降级

优先读取 `market.Data.TimeframeData["5m"].Klines`，含最后一根形成中的 K 线，不等待收盘。分别使用 1/3/12 根形成 5m/15m/60m 窗口：

- 振幅 = `(窗口 max(high) − min(low)) / 窗口首根 open × 100`。
- 基线取此前最多 288 个同窗口滚动振幅的中位数，步长一根 5m，最低 144 个样本（12h）。基线窗口结束位置必须早于当前窗口首根，完全排除与当前窗口共享 K 线的样本。
- N 个基线样本、每窗 w 根，需要 `N + 2w − 1` 根原始 K 线。所以 60m 窗口完整 288 点基线需 **311 根**，最低 144 点需 **167 根**；不能仅按 `288+12` 计算。
- 已检查真实取数路径：`getWithTimeframes` 每周期拉取 `max(200, count)`（上限 1500），但旧转换只保留策略配置的 60–120 根。本轮让 5m/15m 保留至少 200 根已拉取历史，不增加 REST 调用。常见 200 根 5m 下，三个窗口可用基线分别为 **199/195/177** 点（约 16.6/16.3/14.8h），全部不少于 12h。
- 5m 缺失或历史过短时降级到 15m，仅计算 15m（1 根）和 60m（4 根），用最多 96、最低 48 个基线点，保持 24h/12h 的基线跨度。15m 同样不足才退回旧 1h：形成中小时振幅 / 此前 24 根小时振幅中位数。
- 各窗口独立要求最低基线样本数；已选 5m/15m 数据若只能支撑较短窗口，较长窗口 fail-open。已选且长度足够的序列出现缺口、陈旧或无效价格时 fail-open，不用粗周期掩盖数据问题。
- 使用的 K 线必须有有效时间戳、正且有限的 open/high/low、high ≥ low、严格连续；最后一根开盘时间距评估 now 不超过两个周期且不能在未来。基线为零、算术非有限也 fail-open。评估使用调用方时钟，既有闭合指标继续使用快照时钟。

5m/15m/60m **任一比值达到阈值（≥）**便禁入，代码形如 `VOL_SPIKE_5M_x_GT_y`、`VOL_SPIKE_15M_x_GT_y`、`VOL_SPIKE_1H_x_GT_y`；为兼容保留 `_GT_` 命名。旧 1h 退路仍有整点翻页局限，只有缺少足够短周期历史时才使用。旧 `GetWithExchangeAndPrice` 路径只有 100 根 3m、约 20 根聚合 15m，通常走 1h 退路；按策略取数的 prompt/pending recheck 路径能使用保留的 5m/15m 历史。

## OI 双窗口与冷却

- 沿用 `getOpenInterestData` 成功读取时的采样缓冲，缓存 TTL 3 分钟；缓存命中不伪造样本。各扫描周期才可能有新观测，不额外请求历史 OI。
- `market/oi_risk.go` 的互斥锁保护每符号 721 点环形缓冲，15 秒内合并，在最高采样速率下仍保留至少三小时，满足 ≥2h 要求。
- 对 W=15m/60m：`abs(latest / reference − 1) × 100`，reference 是 **时间 ≤ now−W 的最近一个样本**，不是以 latest 时间作为窗口起点。内部 API 保留带符号变化供已有信号展示，闸门取绝对值。
- 最新样本年龄须在 `[0, 6m]`；参考样本最多比目标早 `max(0.35×W, 5m)`，即 15m 窗口 5m15s、60m 窗口 21m。参考 OI 必须正且有限，最新 OI 真零按 100% 崩塌处理。
- 代码为 `OI_SURGE_15M_x_GT_y`、`OI_SURGE_1H_x_GT_y`，变化严格超过阈值才触发（沿用旧 OI 比较边界）。15m/60m 缺样本各自 fail-open，冷启动需相应窗口的真实观测。
- prompt 与 pending recheck 都使用上述带时间校验的采样数据。原 quant 1h OI 仍作为已有衍生品展示及方向信号输入，不能替代新异常闸的采样鲜度检查。
- 价格/OI 异常被观测后，按规范化符号记录线程安全的进程内时间。之后即使当前异常消失，hold-off 未到期仍发 `VOL_HOLDOFF_30m`（配置决定数字）；每次新的异常刷新，恰好到期释放。状态在进程重启后丢失，允许如此，不写数据库。

`THIN_VOLUME_*`、所有 `VOL_SPIKE_*`、所有 `OI_SURGE_*` 和 `VOL_HOLDOFF_*` 均进入双向 `hard_entry_gate.failed`，属于市价/限价、多/空的绝对禁入。market exception、限价锚点和挂单复核都不能绕过。

## 回放依据（2026-10-25）

以下为本任务提供的 Binance **60 天 5m K 线 / OI 历史回放摘要**，本次未重新下载或独立重跑。价格使用上述滚动振幅、此前 288 个 5m 采样点的同窗中位数；考察触发后 forward 1h 最大摆动，对比无触发基线。

- 价格联合条件 `[5m ≥8× 或 15m ≥6× 或 60m ≥5×]`：普通山寨 SOL/SUI/FIL/DOGE/SAGA/MYX/PONS 触发占比约 0.4–1.3%、约 0.3–0.5 次/天，触发后 1h 最大摆动约基线 2 倍。IRYS 触发占比 4.2%，后续摆动中位 7.05%（基线 0.80%）；US 为 10.55%（基线 3.12%）。
- 当前已不触发、但之前 30 分钟内触发过的 hold-off 区域，多数币后续 1h 摆动仍为基线 1.5–3 倍，支持保留 30 分钟禁入。
- 15m OI 8%：BTC/ETH/SOL/DOGE/SUI/MYX/PIEVERSE 几乎不触发；IRYS 0.93 次/天，后续 1h 摆动中位 13.55%（基线 0.79%）；US 1.24 次/天、11.97%（基线 3.14%）；SAGA 0.17 次/天。
- 60m OI 15%：IRYS 0.64 次/天、后续摆动 11.5%；US 0.74 次/天，其余约零。旧 60m 30% 对 IRYS 仅 0.27 次/天，选择性之外还存在迟钝问题。
- 日成交额：IRYS 常态约 1.1M，pump 日可达 2.7B；SAGA 7.8M、MYX 2.9M、PIEVERSE 3.2M、PONS 48M、US 59M。成交额大不等于不被操控，因此将该闸定位为死盘过滤。

局限：样本仅 60 天、币种和行情阶段有限；forward 摆动衡量风险而非方向收益，**未计 PnL、费用、滑点、持仓管理或机会成本**。实现常见 200 根历史使用 12–24h 的缩短基线，与回放完整 24h 基线不完全相同；15m 和旧 1h 退路也不是同一采样口径。15 秒最小合并间隔和实际扫描频率不能保证捕捉瞬时往返；稀疏 OI 采样会 fail-open。没有实盘/测试网下单验证。

## 数据缺失行为

| 情况 | 行为 |
| --- | --- |
| 24h 成交额未知/无效 | 成交额闸 fail-open，记录日志。 |
| 价格短历史缺失/过短 | 记录降级日志，按 5m→15m→1h 尝试；各启用窗口无可靠结果时 fail-open 并 log。 |
| 已选 K 线序列缺口、陈旧、无效，或零基线 | 受影响的价格窗口 fail-open 并 log。 |
| OI 冷启动、参考点偏差超限、最新样本过期/缺失 | 受影响 OI 窗口 fail-open 并 log。 |
| 当前异常数据缺失，但已有观测事件仍在 hold-off 内 | 数据窗口 fail-open 并 log；已知冷却仍禁入，到期自动释放。 |
| 未启用窗口、BTC/ETH/bstock | 对应新闸不适用，不生成异常或冷却。 |

既有风控数据缺失策略保持不变，例如薄币缺盘口的 fail-closed、快速保护 ATR 缺失回退 3%、OI 估值缺失不折扣。本任务的 fail-open 不会绕过其他既有数据质量或交易风控。

## 验证

单测涵盖形成中 K 线、整点翻页回归、基线排除重叠、200 根历史与最低样本边界、时间缺口/陈旧/无效值、三窗口任一触发、数据降级、OI 两窗口样本容差/真实零/冷启动/缓存采样/缓冲保留、hold-off 注入时钟/刷新/到期/并发/豁免、默认值/负数关闭/JSON 显式零，以及所有开仓样式和 pending 复核的绝对禁入。

本轮最终执行结果：

```sh
GOCACHE=$PWD/.gocache go test -count=1 ./trader/... ./kernel/... ./store/... ./market/...
GOCACHE=$PWD/.gocache go build ./... && GOCACHE=$PWD/.gocache go vet ./trader/... ./kernel/... ./store/... ./market/...
GOCACHE=$PWD/.gocache go test -race -count=1 ./kernel ./market ./store ./trader -run 'TestVolatile|TestRollingRange|TestVolatilityData|TestOIRisk|TestFetchedShort|TestPendingFreshVolatility'
```

- 完整测试：`trader`、`trader/binance`、`kernel`、`store`、`market` 及其余可运行子包通过。六个未修改子包的既有 `httptest` 用例因本沙箱禁止监听本地 TCP 而失败，均为 `bind: operation not permitted`：`trader/aster`、`trader/binance_stocks`、`trader/gate`、`trader/lighter`、`market/bnweight`、`market/breakout`。因此整条测试命令退出码为 1，不能声称全绿。
- 全仓库 build、相关包 vet：通过（退出码 0）。
- 上述定向 race 检查：四个根包全部通过，无数据竞争；macOS 链接器有 `LC_DYSYMTAB` 警告，不影响退出码 0。
- 前端 `tsc --noEmit -p web/tsconfig.json`、两个修改文件的 Prettier 检查：通过。临时复用主仓库已有 `node_modules`，链接已删除，没有安装依赖或保留构建产物。
- `git diff --check`：通过；原 26000 字节提示词预算和更新后的哈希快照通过。

## 本轮改动文件

- 核心计算与配置接入：[`kernel/volatile_gate.go`](../kernel/volatile_gate.go)、[`kernel/signal_layer.go`](../kernel/signal_layer.go)、[`kernel/engine_prompt.go`](../kernel/engine_prompt.go)、[`kernel/schema.go`](../kernel/schema.go)。
- 行情历史与 OI：[`market/data.go`](../market/data.go)、[`market/data_klines.go`](../market/data_klines.go)、[`market/oi_risk.go`](../market/oi_risk.go)。
- 配置结构和默认值：[`store/strategy.go`](../store/strategy.go)、[`store/strategy_volatile.go`](../store/strategy_volatile.go)。
- 执行与挂单复核：[`trader/auto_trader_risk.go`](../trader/auto_trader_risk.go)、[`trader/auto_trader_pending_risk.go`](../trader/auto_trader_pending_risk.go)。
- 测试：[`kernel/volatile_gate_test.go`](../kernel/volatile_gate_test.go)、[`kernel/liquidity_sweep_test.go`](../kernel/liquidity_sweep_test.go)（schema 更新后的提示词哈希）、[`market/data_test.go`](../market/data_test.go)、[`market/oi_risk_test.go`](../market/oi_risk_test.go)、[`store/strategy_volatile_test.go`](../store/strategy_volatile_test.go)、[`trader/auto_trader_thin_test.go`](../trader/auto_trader_thin_test.go)。
- 前端：[`web/src/components/strategy/RiskControlEditor.tsx`](../web/src/components/strategy/RiskControlEditor.tsx)、[`web/src/types/strategy.ts`](../web/src/types/strategy.ts)。
- 本文档：[`docs/volatile-coin-risk-control.md`](../docs/volatile-coin-risk-control.md)。
