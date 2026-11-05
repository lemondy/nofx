# fq_us 动量多空接入与 S2 在线模拟（2026-11-05）

依据：
- `docs/research/2026-11-05-fq-us-momentum-prereg.md`，规则 `mom_ls_invvol_v1`；
- S1 结果：`docs/research/2026-11-05-fq-us-momentum-study.md`。

用户已批准按多空规则接入并启动 S2。全程只用模拟盘，不接触任何真实下单接口。

## 目标

现有交易员“因子量化-美股（程序化模拟）”（id 1025e717…）从空仓改为执行 `mom_ls_invvol_v1`：
- **多头**：bStock 现货，记入美股模拟账本；
- **空头**：永续合约，记入同一账本的 SHORT 行，与 us-stock 做空模拟（PR #76）共用一套记账：手续费、资金费、按标的方向计算市值。

## T1：目标文件（Python，`scripts/factor_mining/quant_targets.py`）

- us 规则由 `long_invvol_v1` 改为 `mom_ls_invvol_v1`。原规则因为一直没有因子，从未产生过权重。crypto 不变。
- **信号与选股和 S1 逐字一致。** 复用 `scripts/backtest/fq_us_momentum_study.py` 的 `build_features` 和 `decision_at`，数据源换成线上的 `data/factor_mining/us/daily`。规则如下：
  - 可用条件：至少 253 根有效日线，收盘价 ≥ 5，20 日成交额中位数 ≥ 1000 万美元；
  - 信号：mom12_1；
  - 多空各取 20%，按 1/σ 加权，总敞口各 0.5，单只上限 0.10。
- **股票池冻结为预注册的 90 只 EQUITY**，存成仓库内的小配置文件 `scripts/factor_mining/us_momentum_universe.json`。之后新上架的 bStock 不进入。
- **只做空有永续的股票。** 读 Go 端缓存 `data/cache/usstock_symbols.json`（version 2）。
  - 动量最低的 20% 里没有 perp_symbol 的股票，记入 `excluded`，原因 `no_perp`；剩下的空头重新按 1/σ 分配 0.5。这是接入时的偏离，S1 回测时没有剔除。
  - 映射文件缺失或版本不对时，状态写 `insufficient_universe`，原因 `perp_map_unavailable`，不产生任何权重。
- **只做多已上架的股票（2026-11-05 上线后补充）。** 动量最高的 20% 里，不在映射文件中的股票（即没有上架的 bStock，例如 STX）记入 `excluded`，原因 `not_listed`；剩下的多头重新按 1/σ 分配 0.5。首次调仓时 STX 因此被跳过，多头只有 0.458。
- **as_of** 取日线面板最后一个可用日期。权重键统一用 `<代码>BUSDT`，空头为负数。
- **horizon 写 28。** Go 端按 as_of 的自然日间隔判断是否调仓，28 个自然日约等于 20 个交易日。这是接入偏离，写进文档。
- **夜间任务。** `run_nightly.sh` 在 us 挖掘之后单独执行一次 `python -m scripts.factor_mining.quant_targets --market us --root "$FM_ROOT" --write`，挖掘失败也不影响目标文件更新。
- **验收：** 在 S1 快照数据上，S1 每个调仓日的多头集合和权重（S1 每条腿内的权重 × 0.5）完全一致。空头在不涉及 `no_perp` 的日期上完全一致；涉及的日期按剔除规则核对。

## T2：执行（Go，factor_quant 交易员）

- **`store.FactorQuantConfig` 新增两项：**
  - `us_short_mode`：取值 `""`、`off` 或 `paper`，默认关闭；填 `live` 时拒绝；
  - `us_short_fee_bps`：默认 4，范围 0～100。
- **空头开关关闭时，**只要目标文件里有负权重，就整次跳过调仓（code 为 `short_disabled`），不做半套。空头开关为 `paper` 时，规划器设 `LongOnly=false`。
- **`fqStockBroker`：**
  - `Account()`：按方向读取持仓。空头的标记价用永续报价（`stockGetPerpQuote`），失败时退回缓存价，再退回开仓均价。权益 = 现金 + Σ 按方向计算的市值，未实现盈亏也按方向计算。
  - **资金费：**
    - 读账户之前，对每个未平空头调用 `at.stockApplyFunding` 结算资金费；
    - 每个持仓 30 分钟内最多结算一次（内存节流），避免重演 PR #56 那次资金费接口请求风暴；
    - 减仓或平空之前，强制先结算一次；
    - 结算失败只记日志，不阻断。
  - `Place()`：
    - **开空（open_short）：** 品种必须有 PerpSymbol，否则报错 `NOT_SHORTABLE`。用新的永续报价成交，成交价 = 报价 × (1 − 滑点)，调用 `OpenOrAddSideWithFee(SHORT, us_short_fee_bps)`。
    - **平空（close_short）：** 成交价 = 永续报价 × (1 + 滑点)，调用 `ReduceSideWithFee(SHORT)`。
    - **多头：** 不变。
  - `FormatQty()`：空头优先用永续合约的数量精度；拿不到时用 bStock 的精度，在注释里说明。
- **其余同步改为按方向计算：**
  - 熔断，即回撤和单日亏损；
  - API 读取：账户信息和持仓列表都要显示 SHORT 方向，市值为负数。
- **不改：** crypto 路径、us-stock 引擎、真实下单。

## T3：S2 报告（Python，`scripts/promotion/fq_us_s2_report.py`）

- 只读打开数据库，读 `trader_equity_snapshots`。以纽约证交所工作日（周一至周五）为样本，每天取 UTC 当日最后一个快照，从 S2 开始时间算起。
- **统计口径与预注册 S2 相同：**
  - 统计量：每日净值收益，Newey-West 标准误，滞后 20；
  - 样本：至少 126 个交易日，且至少 6 次调仓（调仓次数从 factor_quant 的调仓记录里数）；
  - 判定：
    - `pass`：z ≥ 1.65，累计收益 > 0，未触发 25% 熔断；
    - `fail`：累计收益 ≤ 0，或触发熔断；
    - 其他为 `inconclusive`，只允许延长一次到 252 天；
  - 天数或调仓次数不够时报 `pending`。
- 输出 JSON 和中文 Markdown，可选用 `evolution_ledger` 记一行 S2。

## 上线步骤（主 agent，在用户批准合并之后）

1. 修改交易员 1025e717 的配置：
   - `us_short_mode=paper`；
   - `max_stale_days=4`，覆盖周末，因为 as_of 是最后一个交易日；
   - 修改前先备份数据库。
2. 生成目标文件，确认状态为 ok，空头不含 `no_perp`。
3. 按标准流程重启后端，检查首次调仓的订单、账本行和资金费结算。
4. 在主账本记一行 S2 开始（`fq-us-momentum-ls-v1`，stage S2），写明开始时间。
