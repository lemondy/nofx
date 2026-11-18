# `crypto_llm` AI 调用降频与事件门控省费机制（`G1 / G2`）— S1 研究报告（2026-11-18）

- **预注册文档**：[`docs/research/2026-11-18-crypto-llm-cost-gating-s1-prereg.md`](file:///Users/zhangyun/workspace/nofx/docs/research/2026-11-18-crypto-llm-cost-gating-s1-prereg.md)
- **冻结校验**：`PREREG_SHA256 = 6bed6419559e76e7b80f97cf377c2aaa18c98febafdb1e24d4123ed63656c0aa`
- **研究脚本**：[`scripts/backtest/crypto_llm_cost_gating_study.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/crypto_llm_cost_gating_study.py)（单元测试 [`test_crypto_llm_cost_gating_study.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/tests/test_crypto_llm_cost_gating_study.py)）
- **账本登记**：[`data/evolution/ledger.jsonl`](file:///Users/zhangyun/workspace/nofx/data/evolution/ledger.jsonl) `crypto_llm` `K = 10`（`G1`）与 `K = 11`（`G2`）

---

## 1. 总结结论（按分级标准）

- **实证诊断与工程漏洞定位成立**；按预注册 S1 统计判定规则，**`G1`（`K=10`）与 `G2`（`K=11`）均判 `fewer_trades`**（未通过“显著优于同跳过率随机对照组 `placebo_pct >= 0.90`”门槛），**均不直接修改实盘或上线新 S2 门控**。
- **可落地的独立工程修复（Bug 定界）**：[`trader/auto_trader_loop.go`](file:///Users/zhangyun/workspace/nofx/trader/auto_trader_loop.go#L223-L227) 在每次 `aiDecision != nil` 时无条件写入 `ai_charges`，把 [`kernel/engine_analysis.go`](file:///Users/zhangyun/workspace/nofx/kernel/engine_analysis.go#L204-L230) 触发的零耗时程序合成 `wait`（`Regime skip`，`ai_request_duration_ms == 0`）也按 `$0.01/次` 计入了 AI 账单。在 `mac-nofx` 上累计 **`13,670` 周期中实际仅 `7,863` 次发起真实 LLM 调用，却被记账 `12,592` 笔（`$121.524`），虚增约 `4,729` 次（`~$47.3`）伪 AI 成本**。
- **`G1` 的结构性发现与敏感性边界**：
  1. 在 `pooled-6-paper`（`8,670` 次真实 LLM 调用、`410` 笔 `entry_order_id` 匹配交易）上，**`G1`（当 `candidate_verdicts` 非空且全为 `blocked` 时跳过 LLM）跳过 `4,216 / 8,670`（`48.63%`）真实调用、直接节省 `$42.16` AI 费用，且 `0 / 410` 笔开仓交易被误伤（`p = 0.0%`，`$0.00` 开仓损益损失）**，总真实净收益从 `-$70.59` 升至 `-$28.43`（`+$1.6864/天 ± $0.3513`，`z = 4.8009 > z_K(11) = 2.8376`，`dev = +1.1708/d`、`holdout = +2.1623/d`）。
  2. 在 `mac-nofx` 启用 `candidate_verdicts` 之后（2026-10-10 起 `3,451` 次真实 LLM 调用、`96` 笔匹配交易），`G1` 同样跳过 `676` 次（`19.6%`）真实调用省下 `$6.76` 且 **`0 / 96` 开仓交易丢失**。
  3. **为何按冻结规则仍判 `fewer_trades`？** 随机跳过任意 `48.63%` 周期（2,000 次对照）同样省下 `$42.16` AI 费，而这 410 笔基线交易平均每笔费后净利接近零（`+$16.11 / 410 ≈ +$0.039/笔`），随机砍掉约一半低期望开仓反而得到接近相等的日增净利（随机对照均值 `$1.6863/天`，故 `G1` 分位为 `placebo_pct = 0.507 < 0.90`）。同时，**`G1` 跳过的周期内包含 `213 / 380` 次 `ai_close` 主动平仓事件**——若停用这些周期的 LLM 巡检，该批持仓的离场将顺延至 Go 确定性 SL/TP 或下一个非全阻断周期，该部分反事实价格路径需先消除主动平仓依赖或通过小额 S2 才能闭合。
  4. **`G2`（`15m` 静默降频）淘汰**：`70.29%` 跳过率虽省 `$60.94` AI 费，但错失 `110 / 410`（`26.83%`）开仓（损失 `-$17.02` 交易净利），周聚类 `SE` 骤增至 `$4.5594/天`（`z = 0.3853`），且 `holdout = -$5.8397/天` 负向翻转，`placebo_pct = 0.516`，明确不可用。

---

## 2. 主队列 `pooled-6-paper` S1 定量结果

样本：`data/data.db` 中 6 个全自动纯模拟 `crypto_llm` 交易员（25 个 UTC 自然日 / 5 个聚类周；`8,670` 次真实 LLM 周期；`410` 笔已平仓交易匹配 `entry_order_id`，另有 `2` 笔未匹配，`coverage_error = 0.0049 << 0.05` 校验通过）。NAS MariaDB `crypto_klines` `BTCUSDT 1h` 严格因果提取 `1,896` 根棒线（`close_time < decision_ts_ms`）。

- **基线表现（无门控）**：交易扣交易所手续费净收益 `+$16.11`，真实 LLM 调用成本 `-$86.70`（`8,670 × $0.01`），**扣 AI 费真实净收益 `-$70.59`**。

| 指标 | `G1` (`K=10`, 全候选 `blocked` 跳过) | `G2` (`K=11`, `G1` + `15m` 低波静默) | 判定阈值 |
|---|---:|---:|---|
| 跳过真实 LLM 周期 | **`4,216 / 8,670` (`48.63%`)** | `6,094 / 8,670` (`70.29%`) | `q ∉ {0, 1}` |
| AI 成本节省 | **`+$42.16`** | `+$60.94` | — |
| 丢失 `open_*` 交易数 / 比例 | **`0 / 410` (`0.0%`)** | `110 / 410` (`26.83%`) | `p_trades ≤ 0.50` |
| 丢失交易净损益影响 | **`$0.00`** | `-$17.02` | — |
| 候选扣 AI 费真实净收益 | **`-$28.43`** | `-$26.67` | — |
| 真实日均增量 `Δ` ± 周聚类 `SE` | **`+$1.6864 ± $0.3513 / 天`** | `+$1.7569 ± $4.5594 / 天` | — |
| 检验统计量 `z` | **`4.8009`** | `0.3853` | `z_K(11) = 2.8376` |
| `dev` / `holdout` 半期拆分（$/天） | **`+1.1708` / `+2.1623`** | `+9.9865` / **`-5.8397`** | 均 `> 0` |
| 2000 次随机同跳过率安慰剂均值 / 分位 | `$1.6863/天` / **`50.7%`** | `$1.5450/天` / `51.6%` | `≥ 90.0%` |
| 交易侧 `Δ avgR` (`z_r`) | `0.00000` (`N/A`) | `-0.00162` (`-0.075`) | — |
| 跳过周期内原 `ai_close` 事件数 | `213 / 380` | `279 / 380` | 敏感性边界 |
| **最终 S1 Verdict** | **`fewer_trades`** | **`fewer_trades`** | — |

---

## 3. `mac-nofx` 实盘审计与 `ai_charges` 账务 Bug 证据

1. **`ai_charges` 虚计实证（非交易策略 S1，纯系统审计）**：
   - [`kernel/engine_analysis.go`](file:///Users/zhangyun/workspace/nofx/kernel/engine_analysis.go#L215-L230) 在所有候选币均被硬门控拦截且无可用模型持仓时返回程序合成 `FullDecision`（`RawResponse = "program-synthesized wait (regime skip)"`，`ai_request_duration_ms == 0`）。
   - [`trader/auto_trader_loop.go`](file:///Users/zhangyun/workspace/nofx/trader/auto_trader_loop.go#L223-L227) 仅判断 `if aiDecision != nil && at.store != nil` 即调用 `AICharge().Record(...)`。
   - 对 `mac-nofx` 全量 `13,670` 条 `decision_records` 与 `ai_charges` 对账：`5,807` 条为 `ai_request_duration_ms == 0` 的零耗时周期（其中 `4,675` 条为 `Regime skip`），实际向 API 发起请求的仅有 `7,863` 次；但 `ai_charges` 却写入了 **`12,592` 笔（`$121.524`）**，即存在约 **`4,729` 次（`~$47.29`）从未发出却被计费的幽灵 AI 成本**。
2. **`mac-nofx` post-2026-10-10（启用 `candidate_verdicts` 后）门控核查**：
   - 在 `3,451` 次真实 LLM 调用、`96` 笔匹配已平仓交易上：
   - **`G1`**：跳过 `676 / 3,451`（`19.6%`）真实调用（省 `$6.76`），**丢失 `0 / 96` 笔开仓（`$0.00` 净损益影响）**；
   - **`G2`**：跳过 `2,167 / 3,451`（`62.8%`）真实调用（省 `$21.67`），但误删 `30 / 96` 笔盈利合计 `+$2.89` 的开仓。

---

## 4. 下一步建议（仅提供可验证路径，未改动实盘）

1. **高确定性无风险工程修复（可随时单独实施）**：在 [`trader/auto_trader_loop.go`](file:///Users/zhangyun/workspace/nofx/trader/auto_trader_loop.go#L223-L227) 为 `AICharge().Record` 增加 `aiDecision.AIRequestDurationMs > 0` 条件，立即消除统计/看板上 ~37.6% 的 `Regime skip` 幽灵账单失真。
2. **若后续要继续挖掘 `G1` 的省费价值**：仅当**无活跃持仓或仅靠 Go SL/TP 管理持仓**（避免延迟 `213 / 380` 次 `ai_close` 主动平仓）时才跳过全 `blocked` 周期，并另立新预注册方案（`K=12`）或走 S2 模拟配对验证对持仓退出质量的影响。

---

## 5. 复现命令与单元测试

```bash
python3 -m unittest scripts.backtest.tests.test_crypto_llm_cost_gating_study
python3 -m scripts.backtest.crypto_llm_cost_gating_study --json-out /tmp/opt2_crypto_llm_gating.json
```
