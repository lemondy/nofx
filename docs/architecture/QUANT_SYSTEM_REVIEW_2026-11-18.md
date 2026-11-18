# NOFX 全链路量化系统深度审查与修复报告（2026-11-18 / 2026-11-19）

审查日期：2026-11-18（审查） / 2026-11-19（逐步修复与回归验证）  
审查基线：`dev` @ `a3ff9a0c8`  
范围：全栈源码（`trader/`、`store/`、`market/`、`kernel/`、`mcp/`、`api/`、`security/`、`fundsledger/`、`web/src/`、`scripts/backtest/`、`scripts/factor_mining/`、`scripts/promotion/`）只读交叉审查 + 代码/契约/统计口径缺陷修复与回归单测。  
运行边界：严格不修改运行时 `data/data.db`、`data/evolution/ledger.jsonl`、冻结预注册 `*-prereg.md`、外部 `launchd` 配置及线上进程，不触发任何真实下单。

---

## 1. 总体结论

NOFX 在双轨架构（LLM 信号内核 + `factor_quant` 程序化纸面交易）与研究治理（预注册、只追加演化账本 $K$ 校正、S1/S2/S3 增量晋级、NautilusTrader + QuantStats 统一回测底座）上具备清晰的工程边界。本轮多模型异构交叉审查共定位 **23 项有精确源码位置与最小反例支撑的问题**（含 2 项 P0、12 项 P1、9 项 P2/性能项），其中 **20 项仓库内代码/契约/统计口径问题已全部完成 TDD 修复并通过全栈回归验证**，其余 **3 项属于运行环境/运维治理边界项**（按约定保持只读不触碰运行资产，保留运维清单）。

---

## 2. 已完成修复清单（#1–#23）

### 2.1 回测引擎与研究方法学（Python）

| 编号 | 级别 | 问题摘要 | 修复方案与位置 | 验证证据 |
|---|---|---|---|---|
| **#1** | **P0** | [`nt_engine.py`](../../scripts/backtest/nt_engine.py) `execution="close"` 模式在下一根日线 `on_bar` 才记前一日净值，导致 close 净值曲线混入下一 bar 涨跌并错标一天；且 close 调仓若同步成交会先于 [`CarryFundingModule.process`](../../scripts/backtest/nt_engine.py#L211-L290) 改写持仓，造成离场日少计费、入场日多计费 | `on_bar(ts)` 将 close 调仓延迟至 `ts + 1ns`（[`_on_close_exec`](../../scripts/backtest/nt_engine.py#L549-L573)），先让 `CarryFundingModule.process(ts)` 结算上一周期持仓，再由 `ts + 2ns` 收盘 `QuoteTick` 推进时钟并以原 bar 时间戳记录收盘净值（[`run_portfolio`](../../scripts/backtest/nt_engine.py#L751-L766)） | [`test_nt_engine.py`](../../scripts/backtest/tests/test_nt_engine.py) 14/14 通过（含 CoC 真值、未来 bar 扰动不变性、`[1,0]`/`[0,1]` 资金费时序校验）及 `nt_parity` 基准验证 |
| **#5** | **P1** | [`alpha_illusion_audit.py`](../../scripts/backtest/alpha_illusion_audit.py) 直接累加毛 `realized_pnl` 未扣 `commission`（把 crypto 实盘净亏 `-\$6.78` 算成毛利 `+\$2.88`），且对混排 ISO 时间戳 `pd.to_datetime` 抛 `ValueError` | 改为净收益 `realized_pnl - COALESCE(commission, 0)`，补充 [`parse_utc_timestamps`](../../scripts/backtest/alpha_illusion_audit.py) 兼容混合 ISO8601 格式，刷新对应诊断报告与研究日志 | [`test_alpha_illusion_audit.py`](../../scripts/backtest/tests/test_alpha_illusion_audit.py) 5/5 通过 |
| **#6** | **P1** | [`dsr_audit.py`](../../scripts/backtest/dsr_audit.py) 把 `scipy.stats.kurtosis(..., fisher=True)` 超额峰度当作原峰度代入 $(\hat{\gamma}_4 - 1)/4$（低估高阶矩方差惩罚），且混用年化 `SR_0` 与非年化标准误导致 100% 假阳性 | 统一在非年化每周期标尺计算 $\widehat{SR}$、$SR_0$ 与 $(\gamma_{4,\text{excess}} + 2)/4 \cdot \widehat{SR}^2$，接入 [`run_nightly.sh`](../../scripts/factor_mining/run_nightly.sh) 第 6 步并刷新诊断报告 | [`test_dsr_audit.py`](../../scripts/backtest/tests/test_dsr_audit.py) 4/4 与 [`test_quant_targets.py`](../../scripts/factor_mining/tests/test_quant_targets.py) 44/44 通过 |
| **#7 / #21** | **P1/P2** | [`ltw_crypto_study.py`](../../scripts/backtest/ltw_crypto_study.py) 自写 `_simulate_ls` 未计永续资金费且绕过 `nt_engine.run_portfolio`；[`paper_pipeline.py`](../../scripts/backtest/paper_pipeline.py) `urllib.urlopen` 无全局超时与熔断 | `ltw_crypto_study.py` 改为路由 `nt_engine.run_portfolio(..., carry=CarryConfig(market="crypto", ...))` 并在文档注明复核口径；`paper_pipeline.py` 增加请求预算、重试退避与连续失败熔断 | [`test_ltw_crypto_study.py`](../../scripts/backtest/tests/test_ltw_crypto_study.py) 6/6、[`test_paper_pipeline.py`](../../scripts/backtest/tests/test_paper_pipeline.py) 2/2 通过 |

### 2.2 交易执行、适配器契约与 `factor_quant` 风控（Go `trader/`、`store/`）

| 编号 | 级别 | 问题摘要 | 修复方案与位置 | 验证证据 |
|---|---|---|---|---|
| **#2** | **P0** | Bybit / OKX / Gate / Hyperliquid 的 `OpenLong` / `OpenShort` 在开仓前调用全币种撤单，误撤对向仓位保护单或手动 `hands-off` 挂单 | 移除开仓入口无差别 `CancelAllOrders` / `cancelAllTriggerOrders`，保留显式平仓路径撤单语义（[`bybit`](../../trader/bybit/futures_bybit.go)、[`okx`](../../trader/okx/futures_okx.go)、[`gate`](../../trader/gate/futures_gate.go)、[`hyperliquid`](../../trader/hyperliquid/hyperliquid_trader.go)） | 各适配器 `open_protection_test.go` 与 [`auto_trader_open_protection_test.go`](../../trader/auto_trader_open_protection_test.go) 全绿 |
| **#8** | **P1** | `factor_quant` 回撤分档（10%/15%/20%）在两次调仓之间的 `too_soon` 窗口仅更新缩放系数而跳过再平衡，已持仓位不减仓 | 在 [`store/factor_quant_dd.go`](../../store/factor_quant_dd.go) 持久化 `AppliedDDScale`/`PendingDDScale`/`DDTrimTargets`，当 `ddScale < appliedDDScale` 时立即执行 [`fqApplyDrawdownTrim`](../../trader/auto_trader_factor_quant_risk.go) 只减仓缩容 | [`auto_trader_factor_quant_risk_test.go`](../../trader/auto_trader_factor_quant_risk_test.go) 新增分档即时减仓与失败重试单测通过 |
| **#12** | **P1** | OKX 与 Gate 的 `FormatQuantity` 返回合约张数而非标的 base 数量，导致上层复算名义价值/挂保护单出现 `CtVal` 二次缩放错配 | 统一 `FormatQuantity` 返回步进对齐后的 base 币种数量，下单内部使用私有合约张数换算 helper（[`okx/futures_okx.go`](../../trader/okx/futures_okx.go)、[`gate/futures_gate.go`](../../trader/gate/futures_gate.go)） | `okx/quantity_contract_test.go` 与 `gate/quantity_contract_test.go` 通过 |
| **#13** | **P1** | `factor_quant` 2.5$\sigma$ Chandelier 追踪止损在波动率放大时允许止损线反向放宽，且平仓失败仍提前清除 `PeakPrices` | 增加单调收紧 [`fqRatchetStop`](../../trader/auto_trader_factor_quant_risk.go)（多头只升不降、空头只降不升），仅在平仓成功后清理 `PeakPrices` 并定期裁剪无持仓残留键 | [`auto_trader_factor_quant_regime_test.go`](../../trader/auto_trader_factor_quant_regime_test.go) 通过 |
| **#14** | **P1** | 滚动 alpha 衰减换仓在旧仓平仓失败时仍标记已处理并继续开新替补仓，可能突破总敞口上限 | 仅在旧仓平仓确认成功后写入滚动状态，并按当前实际剩余总敞口/单腿预算裁剪替补仓名义额（[`auto_trader_factor_quant_exec.go`](../../trader/auto_trader_factor_quant_exec.go)） | [`auto_trader_factor_quant_regime_test.go`](../../trader/auto_trader_factor_quant_regime_test.go) 通过 |
| **#23** | **P2** | `Regime skip` 零 token 程序合成 `wait` 周期仍调用 `AICharge().Record` 产生幽灵 AI 计费（历史占比约 37%） | [`FullDecision`](../../kernel/engine.go#L587-L596) 新增 `ProgramSynthesized`，[`engine_analysis.go`](../../kernel/engine_analysis.go#L226-L231) 合成时置 `true`，[`auto_trader_loop.go`](../../trader/auto_trader_loop.go#L229-L235) 跳过计费 | [`regime_conflict_test.go`](../../kernel/regime_conflict_test.go) 与 [`auto_trader_loop_c2_test.go`](../../trader/auto_trader_loop_c2_test.go) 通过 |

### 2.3 行情数据层、限流与 LLM 内核（Go `market/`、`kernel/`、`mcp/`）

| 编号 | 级别 | 问题摘要 | 修复方案与位置 | 验证证据 |
|---|---|---|---|---|
| **#9** | **P1** | [`mcp/client.go`](../../mcp/client.go) SSE 流式读取吞掉中游 `error` 事件及异常断流 EOF，残缺文本流入 `salvageTruncatedDecisionArray` 存在漏掉风控调整决策风险 | 识别 SSE JSON `error` chunk、跟踪 `[DONE]`/`finish_reason` 正常结束标志（含无空格 `data:{...}`），对未完整收尾流直接报错重试而非静默截断抢救 | [`mcp/stream_integrity_test.go`](../../mcp/stream_integrity_test.go) 通过 |
| **#10** | **P1** | Hyperliquid `executionTimeframe` 归一化为 `5m` 后未传给 `getKlinesWithFallback`，仍拉 `3m` K 线并把 12 根 `5m`（60m）硬编码按 20 根算 1h 涨跌幅 | 透传真实 `execTf` 并按 `barsPerHour(execTf)` 计算 1h 涨跌幅（[`market/data.go`](../../market/data.go)、[`market/data_klines.go`](../../market/data_klines.go)） | [`market/execution_timeframe_integrity_test.go`](../../market/execution_timeframe_integrity_test.go) 通过 |
| **#11** | **P1** | [`market/api_client.go`](../../market/api_client.go) `parseKline` 对上游 JSON 数组直接裸类型断言与下标访问，异常响应可触发 panic | 改为长度检查 + 安全解析 helper，非法根 K 线返回结构化 `error` | [`market/api_client_integrity_test.go`](../../market/api_client_integrity_test.go) 通过 |
| **#17** | **P2** | `ClosedKlines` 单次剔除未闭合尾部、短序列 ATR 回退用未闭合 K 线、BTC 1h/4h 涨跌幅按切片倒数第 2 根而非闭合时间戳对齐 | [`ClosedKlines`](../../kernel/signal_layer.go) 循环剥离全部 `CloseTime > now` K 线；短序列 ATR 保持只用闭合 bar；BTC 基准收益按精确闭合周期开盘时间匹配 | [`kernel/regression_integrity_test.go`](../../kernel/regression_integrity_test.go)、[`market/closed_atr_test.go`](../../market/closed_atr_test.go) 通过 |
| **#18** | **P2** | `resolveBatchConflicts` 仅拦截反向对开，放行同币同向 `open_long` + `open_long_limit` 双重开仓；`stockengine/prompts_short.go` 多空阶段提示词混用 | 同币同向重复开仓在批内消解（保留限价或首条，其余降级 `wait`）；修正美股做空提示词阶段约束（[`kernel/engine_analysis.go`](../../kernel/engine_analysis.go)、[`kernel/stockengine/prompts_short.go`](../../kernel/stockengine/prompts_short.go)） | [`kernel/batch_same_side_test.go`](../../kernel/batch_same_side_test.go) 通过 |
| **#19** | **P2** | [`market/bnweight/meter.go`](../../market/bnweight/meter.go) `ReconcileIP` 用较早响应头覆盖并发在途请求已预扣的权重 | 跟踪每个请求生命周期内的 `inflight` 预留量，对账时取 `max(used, reported + inflightExcludingSelf)` | [`market/bnweight/meter_test.go`](../../market/bnweight/meter_test.go) 并发对账回归通过 |
| **T1-10** | **P3/Perf** | `market/data_klines.go` 展示序列递推每步重算全前缀，复杂度 $O(N^2)$ | 新增线性时间 `emaSeries` / `rsiSeries` 一次扫描计算（[`market/data_indicators.go`](../../market/data_indicators.go)） | [`market/indicator_series_equivalence_test.go`](../../market/indicator_series_equivalence_test.go) 校验误差 $< 10^{-9}$ |

### 2.4 安全边界、API 与前端交互契约（Go `api/`、`security/` + React `web/`）

| 编号 | 级别 | 问题摘要 | 修复方案与位置 | 验证证据 |
|---|---|---|---|---|
| **#15** | **P1** | [`FactorQuantConfigEditor.tsx`](../../web/src/components/strategy/FactorQuantConfigEditor.tsx) 调仓阈值标签写 `%` 却直接读写小数（差 100 倍）；美股切回加密时残留 `us_short_mode="paper"` 触发后端 400 | 前端百分比控件做 $\times 100 / \div 100$ 双向换算，切换 `crypto` 时清空 `us_short_mode: ""` 与 `us_short_fee_bps: null`，补齐市值分层与美股纸面空头控件 | [`FactorQuantConfigEditor.test.tsx`](../../web/src/components/strategy/FactorQuantConfigEditor.test.tsx) 与 [`strategy_factor_quant_review_test.go`](../../api/strategy_factor_quant_review_test.go) 通过 |
| **#16** | **P1** | 账户/持仓查询失败时 `GetRiskStatus` 仍返回 0 回撤/0 持仓，前端 [`RiskStatusPanel.tsx`](../../web/src/components/trader/RiskStatusPanel.tsx) 渲染绿色 `OK` | 后端输出 `equity_unknown` / `positions_unknown` 布尔标记（[`trader/risk_status.go`](../../trader/risk_status.go)），前端对未知状态展示红色告警态而非绿色 `OK` | [`risk_status_review_test.go`](../../trader/risk_status_review_test.go) 与 [`RiskStatusPanel.test.tsx`](../../web/src/components/trader/RiskStatusPanel.test.tsx) 通过 |
| **#20** | **P2** | `ed41115ad` 放宽私网模型端点后，公网域名 DNS 重绑定到私网 IP 或公网 302 跳转到内网地址未被拦截 | [`security/url_validator.go`](../../security/url_validator.go) 仅允许字面量私网 IP 或 `NOFX_LAN_ALLOWLIST` 显式白名单主机访问 RFC1918/ULA，拒绝普通域名解析到私网及公网向私网重定向 | [`url_validator_review_test.go`](../../security/url_validator_review_test.go) 与既有 LAN 单测全部通过 |
| **#22** | **P2** | 晋级/因子报告接口静默吞错、复盘查询缺少单用户归属与条数上限、前端轮询双定时器并发竞态 | 修复 [`handler_promotion.go`](../../api/handler_promotion.go)、[`handler_factor_mining.go`](../../api/handler_factor_mining.go)、[`handler_review.go`](../../api/handler_review.go)、[`handler_system_quality.go`](../../api/handler_system_quality.go)；[`FactorMiningPage.tsx`](../../web/src/pages/FactorMiningPage.tsx) 与 [`PaperPromotionPage.tsx`](../../web/src/pages/PaperPromotionPage.tsx) 改为单定时器 + `reqSeqRef` 防乱序覆盖 | [`handler_review_regression_test.go`](../../api/handler_review_regression_test.go) 与 [`polling.review.test.tsx`](../../web/src/pages/polling.review.test.tsx)（及全量 30 文件 / 289 项 Vitest）通过 |

---

## 3. 运行时与治理遗留清单（只读边界内未自动触碰项）

以下 3 项涉及仓库外系统目录或线上运行态数据库，按本轮安全边界仅做只读核实与运维建议，不在代码提交中自动篡改：

1. **#3 · S2 在跑交易员与新默认开关隔离（运维确认项）**：`store/strategy_factor_quant.go` 中 4 项新机制（`regime_tilt`、`rolling_exit`、`vol_adaptive_stop`、`drawdown_scale`）在指针为 `nil` 时默认开启。若重启线上进程使 `fq-us-momentum-ls-v1`（S2 观察期内）加载新二进制前仍希望保持原始 S2 规则冻结，需在策略配置 JSON 显式写入对应 `false`，或开启新版本交易员 ID 对照。
2. **#4 · `launchd` 夜间因子挖掘指向仓库外副本（运维迁移项）**：`~/Library/LaunchAgents/com.nofx.factor-mining.plist` 当前仍指向 `~/workspace/nofx-autoresearch/scripts/factor_mining/run_nightly.sh`（无 `.git`）。建议运维窗口内将其 `ProgramArguments` 与 `WorkingDirectory` 统一切回主仓库 `/Users/zhangyun/workspace/nofx`，使 `dsr_audit` 第 6 步及最新修复在夜间任务生效。
3. **本机磁盘快照归档（可选运维项）**：`data/` 下存在历史 `data.db.bak-*` 冷备份文件；按 `AGENTS.md` 磁盘约定，可在停机维护窗口迁移至 NAS `/Volumes/homes/liz/nofx_data`（热库 `data/data.db` 与 `data/evolution/ledger.jsonl` 保持在本地盘）。

---

## 4. 经复核明确不建议修改的项

延续 [2026-11-14 审查报告](QUANT_SYSTEM_REVIEW_2026-11-14.md) 的证据原则，以下设计选择经复核确认合理、保持不动：

- **LLM 账户回撤基准默认 `initial`**：兼容历史配置且有单测保护，`factor_quant` 已独立使用 `peak`。
- **美股 LLM 空头对冲不随多头 regime 缩仓同步削减**：`auto_trader_stock_regime.go` 明确设计为保留空头对冲腿。
- **`quant_targets.json` 核心阶段 `factor_ids` 为空数组**：受字节冻结单测保护，仅扩展阶段追加，不影响交易执行。
- **冻结预注册脚本与 `scripts/factor_mining/evaluate.py`**：受 `PREREG_SHA256` 完整性约束，保持只读不改。
