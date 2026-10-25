# 薄盘口 / 异常波动币风控

本次改动只涉及五项风控、配置入口及对应测试。未提交 git、未 push、未生成仓库内可执行文件。

## 默认值和开关

数值字段遵循 `0/未设 = 默认值，负数 = 关闭`；所有字段位于 `risk_control`，有对应 `Effective*` 访问器。显式 `false` 关闭薄币缺盘口拒单。

| 字段 | 默认值 | 依据 / 作用 |
| --- | --- | --- |
| `min_quote_volume_24h_millions` | 30（3000 万美元） | 现有候选池默认 OI 下限为 1500 万美元；要求日成交额至少覆盖其两倍，是保守的工程初值。BTC/ETH、bstock 豁免。 |
| `thin_oi_full_size_millions` | `5 × EffectiveMinOIMillions()`，通常 75 | 在候选 OI 底线之上留出流动性余量才使用满仓尺寸。 |
| `thin_oi_min_size_factor` | 0.3 | 折扣为 `clamp(OI / 满仓门槛, 0.3, 1)`；系数最多为 1，负数关闭折扣。BTC/ETH、bstock 豁免。 |
| `vol_spike_mult` | 3 | 最新 1h 的 `(high-low)/open` 相对之前 24 根小时振幅中位数超过 3 倍时禁入；包括形成中的最新小时，不等收盘才响应。bstock 豁免。 |
| `oi_surge_1h_pct` | 30% | OI 上涨或下降超过 30% 都禁入；使用合约数量变化，避免币价本身上涨导致名义金额假性增长。bstock 豁免。 |
| `fast_window_minutes` | 5 分钟 | 短窗口识别突发异常；最高 120 分钟，负数关闭整项快速保护。 |
| `fast_adverse_atr_mult` | 2 | 五分钟内逆向移动至少 2 倍闭合 1h ATR%，高于常规短周期噪声。负数仅关闭逆向分支。 |
| `fast_favorable_atr_mult` | 2 | 顺向暴冲达到同等量级时先锁保本。负数仅关闭顺向分支。 |
| `fast_adverse_reduce_frac` | 0.5 | 每次减当前仓位 50%；最多 0.75，仍受原始仓位累计 75% 和最小剩余仓位限制。负数关闭快速减仓。 |
| `fast_adverse_cooldown_minutes` | 30 分钟 | 防止异常行情下连续减仓；成功才消耗冷却，平均开仓价变化不会重置冷却。负数关闭快速减仓。 |
| `fast_fallback_pct` | 3% 价格变动 | ATR 缺失时避免对小幅噪声反应；负数关闭 ATR 缺失时的固定阈值分支。 |
| `thin_oi_mark_stop_millions` | `3 × EffectiveMinOIMillions()`，通常 45 | 小于该缓存 OI 门槛时，Binance 期货止损采用 MARK_PRICE，负数关闭该项。 |
| `spread_gate_fail_closed_thin` | true | OI 低于满仓门槛，或 OI 未知且非 BTC/ETH 时，盘口缺失拒绝开仓。没有可靠的其他主流币静态名单，因此不额外豁免；ETHFI、BTCDOM 不按前缀豁免。 |

## 路径和约束

- `THIN_VOLUME_*`、`VOL_SPIKE_1H_*`、`OI_SURGE_*` 均进入 `hard_entry_gate.failed`。它们是市价和限价、多空双向的绝对禁入码，market exception 不能绕过；新限价与挂单复核共用这些语义。异常事件和薄流动性不会因使用限价锚点而消失。
- OI 折扣接在共用的 `clampSizeToRisk` 返回处，风险上限裁剪完成后再折扣。市价多、市价空和 pending 限价走同一条尺寸链，折扣后继续检查最小仓位，不把不足最小值的仓位重新放大。
- market 层按符号使用互斥锁保护的 721 点循环缓冲，只在 OI 上游读取成功时采样；15 秒内合并，最高采样速率下保留至少三小时。缓存命中不会伪造新观测。1h 参考点允许向前最多 10 分钟偏差，当前样本最多 3 分钟旧。
- 快速保护优先使用现有 Binance 5 秒 mark WebSocket tick，并在已有一分钟 drawdown tick 中兜底；不增加 REST 行情或持仓轮询。ATR 由原有行情读取缓存，10 分钟过期；保护单提交与确认仍沿用现有交易接口。
- 只处理 `isAIManaged` 仓位。逆向分支先调用 `ensureBreakevenBeforeAutomatedTrim`；亏损且尚无保本止损时跳过减仓，避免违反旧约束。顺向分支目标覆盖配置的往返成本（默认 20bps，至少 10bps），只调用 `moveStopExchange` 收紧止损，已锁住更多利润的止损保持原值。
- 已有 R 止盈刚减仓的同一 tick 不叠加快速减仓，并更新快照数量；累计减仓检查也读取成功的 R/ROE 标志，避免 fill sync 尚未更新数据库时低估已减比例。
- Binance 的 STOP_MARKET 创建、重试和恢复使用同一次判定的 working type。MARK_PRICE 抗单笔成交插针，但跳空时可能触发更晚。止盈仍使用原来的触发类型。保护单识别继续按方向、订单类型、价格及覆盖量工作，不按 working type 判缺失或重复，因此无需增加 OpenOrder 字段。
- bstock 豁免新流动性禁入和缺盘口 fail-closed；原本可测的宽价差闸仍保留，未放宽旧检查。

## 数据缺失行为

| 缺失数据 | 行为 |
| --- | --- |
| 24h 成交额 | fail-open，记录日志。 |
| 仓位折扣 OI / 估值价格缓存缺失或过期 | 不折扣，记录日志。 |
| 1h 波动比较历史不足、不连续或过期 | 波动异常禁入闸 fail-open，记录日志；其他既有数据质量闸仍可拒绝。 |
| 1h OI 历史不足或过期 | OI 剧变闸 fail-open，记录日志；优先沿用原有 quant 1h 变化，缺失才读采样缓冲。 |
| 快速保护的 1h ATR | 回退默认 3% 固定价格变动，记录日志；缺少有效持仓、mark 或 AI 管理标记时不采取新动作。 |
| MARK_PRICE 判定所需 OI | 保持 CONTRACT_PRICE，不新增 REST 查询。 |
| 盘口接口不存在 / 请求失败 / 无法计算有效非零 spread | 薄币和 OI 未知的山寨币 fail-closed，并给出具体原因；非薄币、未知 OI 的 BTC/ETH 和 bstock 保持原 fail-open。 |

## 验证

- 新增测试覆盖配置默认值、负数关闭、JSON 显式 false、动态 OI 门槛、成交额/OI/波动边界、多空和所有开仓样式、pending 复核、缓存过期、OI 真零、循环缓冲和并发读取、滑窗方向和过期、冷却、手动仓、只收紧、亏损前保本约束、减仓失败重试、R 止盈交互、快速路径无行情/持仓 REST 读取，以及止损创建/重试/恢复。
- 中英文 schema 已添加机器码说明；更新对应提示词哈希快照，并保持原 26000 字节预算测试通过。
- `GOCACHE=$PWD/.gocache go test ./trader/... ./kernel/... ./store/... ./market/...`：与任务相关的根包及 Binance 测试通过；仍有六个未改动包的旧 `httptest.NewServer` 用例因沙箱禁止监听本地 TCP 端口失败（`bind: operation not permitted`）：`trader/aster`、`trader/binance_stocks`、`trader/gate`、`trader/lighter`、`market/bnweight`、`market/breakout`。没有为通过测试修改这些无关文件。
- `GOCACHE=$PWD/.gocache go test -race ./trader ./trader/binance ./market ./store ./kernel -run 'TestThin|TestVolatile|TestFast|TestOIRisk|TestHourlyRange|TestPendingFreshVolatility|TestRejectedStopReplacement' -count=1`：通过。
- `GOCACHE=$PWD/.gocache go build ./... && GOCACHE=$PWD/.gocache go vet ./trader/... ./kernel/... ./store/... ./market/...`：通过。

## 未验证及限制

- 没有实盘或交易所测试网下单验证。阈值是可配置的保守工程初值，未对 IRYS 做历史回测校准。
- OI 缓冲冷启动需至少一小时的观测，之前按要求 fail-open；有原 quant 1h 数据时可提前启用该闸。
- mark 滑窗和冷却保存在进程内，重启会重建；完成平仓会清理窗口/冷却。没有新增数据库迁移。
- 无 WebSocket 或流失效时使用已有一分钟 tick；5 秒内的瞬时往返不保证被采样命中。
- 前端已补配置类型及中英文编辑控件；本工作树没有 node_modules/tsc，未运行前端构建或界面测试。

## 改动文件清单

- [`docs/volatile-coin-risk-control.md`](../docs/volatile-coin-risk-control.md)
- [`kernel/engine_data_binance.go`](../kernel/engine_data_binance.go)
- [`kernel/engine_prompt.go`](../kernel/engine_prompt.go)
- [`kernel/liquidity_sweep_test.go`](../kernel/liquidity_sweep_test.go)
- [`kernel/schema.go`](../kernel/schema.go)
- [`kernel/signal_layer.go`](../kernel/signal_layer.go)
- [`kernel/volatile_gate.go`](../kernel/volatile_gate.go)
- [`kernel/volatile_gate_test.go`](../kernel/volatile_gate_test.go)
- [`market/data.go`](../market/data.go)
- [`market/oi_risk.go`](../market/oi_risk.go)
- [`market/oi_risk_test.go`](../market/oi_risk_test.go)
- [`store/strategy.go`](../store/strategy.go)
- [`store/strategy_volatile.go`](../store/strategy_volatile.go)
- [`store/strategy_volatile_test.go`](../store/strategy_volatile_test.go)
- [`trader/auto_trader.go`](../trader/auto_trader.go)
- [`trader/auto_trader_fast.go`](../trader/auto_trader_fast.go)
- [`trader/auto_trader_fast_test.go`](../trader/auto_trader_fast_test.go)
- [`trader/auto_trader_freshness_test.go`](../trader/auto_trader_freshness_test.go)
- [`trader/auto_trader_marketdata.go`](../trader/auto_trader_marketdata.go)
- [`trader/auto_trader_pending_risk.go`](../trader/auto_trader_pending_risk.go)
- [`trader/auto_trader_risk.go`](../trader/auto_trader_risk.go)
- [`trader/auto_trader_stream.go`](../trader/auto_trader_stream.go)
- [`trader/auto_trader_thin.go`](../trader/auto_trader_thin.go)
- [`trader/auto_trader_thin_test.go`](../trader/auto_trader_thin_test.go)
- [`trader/binance/fix_20261003_test.go`](../trader/binance/fix_20261003_test.go)
- [`trader/binance/futures.go`](../trader/binance/futures.go)
- [`trader/binance/futures_orders.go`](../trader/binance/futures_orders.go)
- [`trader/binance/volatile_stop_test.go`](../trader/binance/volatile_stop_test.go)
- [`web/src/components/strategy/RiskControlEditor.tsx`](../web/src/components/strategy/RiskControlEditor.tsx)
- [`web/src/types/strategy.ts`](../web/src/types/strategy.ts)
