# `crypto_llm` AI 调用降频与事件门控省费机制（`G1 / G2`）— S1 预注册（2026-11-18）

本文件写于编写研究主脚本及计算任何 `G1`/`G2` 盈亏、胜率、$R$ 或净收益差之前。一经锁定 `PREREG_SHA256`，下述样本池、门控规则、成本口径与判定阈值一律冻结，运行后不得修改。

## 1. 研究起源、机制与诚实声明

- **文献与审计出处**：Ye 等（2026）*AlphaIllusion*（`arXiv:2605.16895`，Section 2.4 & 3.2 Eq. 1 `P5`），以及 Pilot 1 审计 [`docs/research/2026-11-18-alpha-illusion-assessment.md`](file:///Users/zhangyun/workspace/nofx/docs/research/2026-11-18-alpha-illusion-assessment.md)。
- **已观测到的系统结构事实（非收益预览）**：
  1. 在 `data/data.db` 的 `mac-nofx` `13,670` 个周期中，`5,807` 个周期（`42.5%`）`ai_request_duration_ms == 0`（主要由 `Regime skip` 合成 `wait`，未发起 LLM 请求），但 [`trader/auto_trader_loop.go`](file:///Users/zhangyun/workspace/nofx/trader/auto_trader_loop.go#L223-L227) 仍无条件记入 `ai_charges`；而在**真正发起了外部 API 调用**的周期（`ai_request_duration_ms > 0`）中，仍有 `~83%` 只返回 `wait`/`hold`，且 6 位纯自动模拟盘上 **`47%–53%` 的真实 LLM 调用发生在“所有候选币已被 Go 硬门控 `blocked`、仅因存在开放持仓而呼叫 LLM 巡检”的周期**；同时实际真实 LLM 周期中位间隔仅 **`5.1–8.6` 分钟**。
  2. Go 交易内核的止损、移动止损、止盈、分层减仓与回撤保护均独立运行，不依赖高频轮询 LLM 才能触发出场。
- **诚实声明与已知偏差**：
  1. **样本窗口与重合度**：主队列由 `data/data.db` 中 6 个全自动纯模拟 `crypto_llm` 交易员组成（`2026-10-20 .. 2026-11-18`，覆盖 `20,000+` 决策周期、`8,140` 次真实 LLM 调用与 `405` 笔已平仓交易，`entry_order_id` 精确匹配率 `> 99%`），与 `mac-nofx` 10–11 月实盘同源但无人工平仓干扰。
  2. **非同步配对限制**：多交易员并行运行于同一加密市场窗口，主统计量对**全队列共享每日自然日（UTC）总真净收益差**作周聚类 `SE` 推断（将同日同池多交易员视作单一联合账户日序列），避免把同一天多个交易员算作独立样本而虚高 $t$ 值。
  3. **账本 `K` 预算**：`data/evolution/ledger.jsonl` 中 `crypto_llm` 当前累计 `K = 9`；本预注册新增 **2 个候选变体（`K = 10` 与 `K = 11`）**。强显著门槛：`z_K(11) = Φ⁻¹(1 - 0.05 / (2 * 11)) ≈ 2.8376`。
  4. **被跳过周期的 `ai_close` 反事实影响**：若某周期原本未开新仓但触发了 `ai_close` 主动平仓，跳过该周期会推迟该平仓到下一未跳过周期或 Go 硬止损；本实验在主检验（按开仓 `entry_order_id` 匹配掉单 + 省下 `c_AI` 费）之外，必须单独计算并报告受影响 `ai_close` 订单在下一个可用周期的 1h K 线敏感性影响。

## 2. 冻结样本、真实 AI 计价与外部行情来源

- **主评估人群（`pooled-6-paper`）**：
  - `因子 ensemble 参考字段-对照`（`a8eb1bdf_...`）
  - `因子 ensemble 参考字段-候选`（`a2dc5705_...`）
  - `提前减仓 0.8R（1/3）-候选`（`ec830bb2_...`）
  - `提前减仓 0.8R（1/3）-对照`（`895dc8b3_...`）
  - `C1 限价带宽入场（执行层）-候选`（`d211acdd_...`）
  - `C1 限价带宽入场（执行层）-对照`（`00a1dd1f_...`）
- **实盘审计补充组（`mac-nofx-matched`，非主 `K` 决定项，独立披露）**：
  - `mac-nofx`（`d7ff3764_...`）自 `2026-10-10` 起启用 `candidate_verdicts` 后的全部真 LLM 周期及 OID 匹配平仓交易，并单独导出全周期 `5,807` 次零时长 `Regime skip` 误计 `ai_charges` 对账单。
- **真实单次 LLM 成本口径**：`c_AI = $0.01 / 次`（严格仅对 `ai_request_duration_ms > 0` 的周期计费，对标 `data.db` `ai_charges` 对该批模型 `glm` / `deepseek` 的 `$0.01/次` 默认记价）。
- **外部行情（仅用于 `G2` 因果波动判断与 `ai_close` 敏感性审计）**：
  - NAS MariaDB `nofx.crypto_klines`（`market='binance_um'`, `interval='1h'`, `fmt='dict'` 保留毫秒级 `close_time`），严格要求 `close_time < decision_ts_ms`（零未来函数）。

## 3. 冻结候选门控变体（均属 `change_type = B` 过滤/触发门控）

对每个交易员 $u$ 的真实 LLM 调用序列（按时间戳升序，$t_k$ 为第 $k$ 次真实 LLM 周期，`ai_request_duration_ms > 0`）：

### 变体 `G1`（`K = 10`, `idea_id = crypto-llm-all-blocked-skip-v1`）
- **因果规则**：若周期 $t_k$ 存在非空 `candidate_verdicts` 且**所有**候选币种的 `status == "blocked"`，则**跳过该周期的 LLM 调用**（省下 `\$0.01`；不发起 `open_*`；持仓由 Go 确定性 SL/TP 继续保护）。

### 变体 `G2`（`K = 11`, `idea_id = crypto-llm-quiet-cadence-15m-skip-v1`）
- **因果递推状态**：维护每个交易员上一次**实际放行**调用 LLM 的时间戳 $t_{\text{last}}$ 与上一周期是否全拦截状态。
- **因果规则**：在周期 $t_k$ 若满足以下**任意一条**，则跳过本次 LLM 调用（不更新 $t_{\text{last}}$）；否则放行（付费 `\$0.01`，更新 $t_{\text{last}} \leftarrow t_k$）：
  1. 满足 `G1`（`candidate_verdicts` 非空且全部 `status == "blocked"`）；**或**
  2. 距离上一次实际放行 LLM 调用不足 **`15` 分钟**（$t_k - t_{\text{last}} < 15\text{ min}$），**且**当前周期不存在候选由上一周期 `blocked` 新转为可开仓的事件突破，**且**最近 1 根严格已收盘 `BTCUSDT 1h` 棒线处于低波动静默状态（$|r_{1h}| < 0.008$ 且 $(\text{high}-\text{low})/\text{open} < 0.015$）。

## 4. 主评价指标、随机安慰剂对照与判定规则

1. **全队列交易-周期精确匹配**：
   - 仅当平仓交易的 `entry_order_id` 唯一对应 `decision_records` 中 `ai_request_duration_ms > 0` 的 `open_*` 记录时计入匹配；**匹配覆盖缺失率 `> 5%` 则该实验臂直接判 `invalid`**。
2. **主序列（扣 AI 费真实日净收益增量 $\Delta_d$）**：
   - 将所有匹配交易按其**入场触发周期所在 UTC 日期**归属至日 $d$；将所有真实 LLM 周期亦按 UTC 日期归属至日 $d$。
   - 基线日净收益：$\text{TrueNet}_{B,d} = \sum_{i \in d} (\text{realized\_pnl}_i - \text{fee}_i) - N_{\text{calls}, B, d} \cdot c_{\text{AI}}$
   - 候选日净收益：$\text{TrueNet}_{C,d} = \sum_{i \in d, \text{cycle}(i) \notin \text{skip}} (\text{realized\_pnl}_i - \text{fee}_i) - N_{\text{calls}, C, d} \cdot c_{\text{AI}}$
   - 日增量序列：$\Delta_d = \text{TrueNet}_{C,d} - \text{TrueNet}_{B,d} = (\text{AI 节省金额}_d) - (\text{被跳过周期对应入场交易的净盈亏}_d)$。
   - 对 $\{\Delta_d\}$ 计算 `incremental_filter.weekly_statistics`（UTC 周一聚类 `SE`）与 $z = \overline{\Delta}_d / \text{SE}$。
3. **2,000 次同跳过率随机安慰剂对照**：
   - 在每个交易员内维持与门控规则完全相同的跳过周期数 $S_u$，从其真实 LLM 周期中均匀无放回随机抽取 $S_u$ 个跳过周期（`seed = 20261118`），重算全队列平均每日/总增量 $\Delta_{\text{placebo}}$，计算 `placebo_pct = mean(Delta_placebo < Delta_C)`。
   - **同时披露**：纯交易侧过滤 `R` 均值差与保留交易数比例 `1 - p_trades`。
4. **切分检验**：将观测日按时间顺序平分为前半（`dev`）与后半（`holdout`），检验 `dev_delta > 0` 且 `holdout_delta > 0`。
5. **严格判定顺序（先匹配即止）**：
   - `invalid`：交易匹配缺失 `> 5%`，或真实周期跳过率 `q in {0, 1}`；不计入 `K`。
   - `fewer_trades`：若开仓交易被丢弃比例 `p_trades > 0.50`（跳过超过一半开仓交易量）或 `placebo_pct < 0.90`。
   - `fail`：`z < 1.0`，或 `dev_delta <= 0`，或 `holdout_delta <= 0`。
   - `strong`：`p_trades <= 0.50`、`placebo_pct >= 0.90`、`dev_delta > 0`、`holdout_delta > 0` 且 `z >= z_K(11) = 2.8376`（推荐建 S2 模拟配对）。
   - `screen_pass`：`p_trades <= 0.50`、`placebo_pct >= 0.90`、`dev_delta > 0`、`holdout_delta > 0` 且 `1.0 <= z < 2.8376`（列为 S2 候选）。

## 5. 执行与交付边界

- 仅撰写研究脚本 [`scripts/backtest/crypto_llm_cost_gating_study.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/crypto_llm_cost_gating_study.py)、单元测试、研究报告与研究日志；
- **严禁**修改 `data/data.db`、交易内核、实盘/模拟盘策略配置，**严禁**启动 S2 配对或下单；
- 完成后通过 `evolution_ledger.append_event` 将 `K = 10`（`G1`）与 `K = 11`（`G2`）记入 `data/evolution/ledger.jsonl`。
