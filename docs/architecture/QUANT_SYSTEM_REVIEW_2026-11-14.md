# NOFX 量化系统审查报告（修订版）

审查日期：2026-11-14（同日复核修订）  
代码基线：`dev`（`81e5c6987121d259817dfca6e08eac1e3e181b9e`）  
范围：只读审查，未修改任何交易逻辑或配置。

> **修订说明**：初版把若干已知取舍、已披露缺口和未经验证的设想写成了 P0 缺陷，并含无依据的量化估计。本版逐项对照代码复核后重写：只保留有证据的结论，其余明确标为"不建议改"或"未经检验的设想"。

---

## 1. 总体评价

系统是双轨（`LLM Kernel` + `factor_quant`）、带严格统计治理的混合量化系统。治理与执行安全明显强于一般个人量化项目；短板主要在**已知并已披露**的数据偏差与 Alpha 层，而非工程缺陷。本报告**不做 A/B 评分**（无量化口径，主观打分价值低）。

## 2. 站得住的优点（有代码/文档证据）

1. **研究治理**：预注册、只追加账本 [ledger.jsonl](/Users/zhangyun/workspace/nofx/data/evolution/ledger.jsonl) 按赛道记 $K$、S1→S2→S3 三阶晋升、安慰剂/噪声对照；重叠标签用非重叠抽样并做 Student-$t$→$z$ 校准（[evaluate.py#L118-L144](/Users/zhangyun/workspace/nofx/scripts/factor_mining/evaluate.py#L118-L144)）。
2. **LLM 路径分工**：确定性事实由 [signal_layer.go](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go) 计算，模型只做判断；system prompt 字节稳定以命中缓存（[engine_prompt.go#L27-L30](/Users/zhangyun/workspace/nofx/kernel/engine_prompt.go#L27-L30)）。
3. **执行安全**：慢 I/O 期间释放执行锁并在返回后刷新账户（[auto_trader_loop.go#L137-L195](/Users/zhangyun/workspace/nofx/trader/auto_trader_loop.go#L137-L195)）、30s 保护看门狗、`ai_managed` 所有权隔离、`factor_quant` 代码级仅限 paper。
4. **模拟撮合保真度**：[paper.go](/Users/zhangyun/workspace/nofx/trader/paper/paper.go) 含分钟内 SL→TP→限价顺序、跳空按开盘价成交、限价穿价代理、真实资金费结算。

## 3. 经复核的发现

### 3.1 确认存在的小缺陷

#### R1 · 有意设计，不修 · `quant_targets.json` 的 `factor_ids` 为空
> **复核结论（2026-11-14 晚）**：尝试修复后发现这是两条有测试保护的设计选择叠加的结果，已撤回改动：① 核心导出字节冻结，`factor_ids` 只允许由扩展阶段追加（[test_ext_scores.py](/Users/zhangyun/workspace/nofx/scripts/factor_mining/tests/test_ext_scores.py#L74)）；② 目标文件不从 zoo/ensemble 猜测因子来源（[test_quant_targets.py#L865-L876](/Users/zhangyun/workspace/nofx/scripts/factor_mining/tests/test_quant_targets.py#L865-L876)），因 CLI 单独运行时 ensemble 可能与 live_scores 不是同一次 run。字段仅用于审计，不影响交易。下文为原始观察，保留备查。

- 位置：[live_scores.py#L67-L72](/Users/zhangyun/workspace/nofx/scripts/factor_mining/live_scores.py#L67-L72)；`factor_ids` 仅在 [扩展阶段 L216](/Users/zhangyun/workspace/nofx/scripts/factor_mining/live_scores.py#L216) 补写。
- 现象：[quant_targets.json](/Users/zhangyun/workspace/nofx/data/factor_mining/crypto/quant_targets.json) 中 `factor_ids` 为 `[]`，而 `live_scores.json` 中有值。`miner.py` 先 `write_targets`（[L643](/Users/zhangyun/workspace/nofx/scripts/factor_mining/miner.py#L643)），扩展在独立流程（`ext_universe.py`）中执行，时序差由代码推断，**未实际复现**。
- 影响：**仅影响审计溯源**。Go 侧 [auto_trader_factor_quant_targets.go#L55](/Users/zhangyun/workspace/nofx/trader/auto_trader_factor_quant_targets.go#L55) 只声明该字段，不用于校验或决策。
- 建议：一行修复（核心导出时写入 `factor_ids`），低优先级。

#### R2 · 运维 · 基线尚未登记
- 登记工具已存在：[baseline.py](/Users/zhangyun/workspace/nofx/scripts/promotion/baseline.py)，用法见 [README「基线登记」](/Users/zhangyun/workspace/nofx/scripts/promotion/README.md)。`data/evolution/baselines.json` 不存在，仅说明尚未执行 `python -m scripts.promotion.baseline set`；`show` 无登记时显示 null，属预期行为。
- 账本中 `baseline_version` 写法不一与此是否相关，**无证据**，不下因果结论。
- 建议：下次基线晋升时用现有工具登记即可，无需新开发。

### 3.2 属实但属于"已披露/已知缺口"，非新发现

#### R3 · 美股纸面账本无单标止损
- [FACTOR_QUANT_TRADER_DESIGN#L146](/Users/zhangyun/workspace/nofx/docs/architecture/FACTOR_QUANT_TRADER_DESIGN_2026-11-01.md#L146) 已列为"已知缺口"；US 目标含空头（[quant_targets.py#L145-L169](/Users/zhangyun/workspace/nofx/scripts/factor_mining/quant_targets.py#L145-L169)）。
- 仅纸面、无真实资金；若 S2 要观察空头尾部风险，可考虑补，是否做由使用者决定。**不属于 P0。**

#### R4 · 样本池偏差（美股 bStock、加密存活币）
- 研究日志与 [quant_targets.py 报告口径](/Users/zhangyun/workspace/nofx/scripts/factor_mining/quant_targets.py#L463) 已明确披露"存活币宇宙有幸存者偏差"。属已知限制，不是新问题；解决需新数据源，成本高，仅在需要更强结论时再做。

#### R5 · `fq_crypto` 对数 RankIC 与算术收益的错配
- 来自既有研究（shortleg-convexity、admission-arith）结论的复述，**不是本次新证据**。
- 按 `AGENTS.md`，该方向已有大量研究；"波动率残差目标""BTC 对冲空头腿"等改法**未核对是否已被验证过**，在查重之前不应作为推荐项，仅作"若重启该方向的待查假设"。

### 3.3 复核后不建议修改

| 项 | 理由 |
| :--- | :--- |
| SQLite 改 WAL | [driver.go#L188](/Users/zhangyun/workspace/nofx/store/driver.go#L188) 注释说明 DELETE 是为 Docker 兼容有意选择；外部读者（[report.py](/Users/zhangyun/workspace/nofx/scripts/promotion/report.py#L617)、[llmreplay](/Users/zhangyun/workspace/nofx/cmd/llmreplay/main.go#L232)）均 `mode=ro` 且带超时；**无锁争用实测**。WAL 在容器卷/bind mount 上有风险。除非出现实测锁等待，否则不动。`paper-promotion.md#L48` 仅为泛指措辞，可顺手润色。 |
| LLM 回撤基准默认改 `peak` | `initial` 是为兼容旧配置保留的默认（[account_drawdown_test.go](/Users/zhangyun/workspace/nofx/store/account_drawdown_test.go)），`factor_quant` 已用 `peak`（[auto_trader_factor_quant.go#L864](/Users/zhangyun/workspace/nofx/trader/auto_trader_factor_quant.go#L864)）。属策略配置决策，不是缺陷。 |
| 美股 LLM 空头逆向 Regime 缩减 | [auto_trader_stock_regime.go#L110](/Users/zhangyun/workspace/nofx/trader/auto_trader_stock_regime.go#L110) 注释明示"short hedges are left intact"，是有意取舍；对称缩减为无研究支撑的新功能。 |

### 3.4 未经检验的设想（仅供参考，无证据、不排期）

- **单笔内配对推广**：C1（[auto_trader_entry_band.go](/Users/zhangyun/workspace/nofx/trader/auto_trader_entry_band.go)、[S2 计划](/Users/zhangyun/workspace/nofx/docs/research/2026-11-13-limit-band-entry-s2-plan.md)）的反事实配对确实比双交易员 A/B 噪声小，可在后续**不改变 prompt 上下文**的执行/出场实验中考虑复用。降噪幅度需以实测方差为准，**不给预估数字**。
- **规则引擎接入结构特征**、**llmreplay 状态联动**、**跨交易所原生喂价/冲击成本**：未核对适配器与样本量，无问题证据，暂不立项。
- **架构 README 同步**：[README.zh-CN.md](/Users/zhangyun/workspace/nofx/docs/architecture/README.zh-CN.md) 与目录现状有出入，属文档整理，低优先级。

## 4. 建议清单（精简）

| 优先级 | 事项 | 性质 |
| :---: | :--- | :--- |
| — | `factor_ids` 为空（R1） | 有意设计，已验证不修 |
| 运维 | 下次基线晋升时执行 `baseline set`（R2） | 无需开发 |
| 视需要 | 美股纸面空头加硬止损（R3） | 若 S2 要看尾部风险再做 |
| 观察 | 后续出场/执行类 S2 实验评估是否复用 C1 配对（3.4） | 方法选择 |

其余均**不建议改动**或**待证据**。结论："当前系统没有需要紧急修复的缺陷，主要风险是已披露的数据偏差与 `fq_crypto` 的 Alpha 不足"。
