# Volrix 公开策略移植回测（2026-10-31）

来源：<https://app.volrix.ai/strategies?sort=newest>，通过公开 API `api.volrix.ai/strategy-library/strategies`（列表、详情、report、trades）抓取全部 83 个策略。原始数据在 `data/volrix/`（gitignored）：`summary.csv` 汇总，`portable_specs.md` 为可移植规则原文，`trades/` 为三个币安策略的公开成交。策略代码需登录才可见，本次只用公开的文字规则与成交记录。

## 结论

**没有可以直接融合进加密交易系统的策略。** 22 条可移植规则在币安永续（含手续费 0.05%、滑点 0.03%、资金费）上按预声明门槛回测，只有 `SUPERTREND_D` 在 ETH 上机械通过，且相对现货买入持有没有实质优势（见下）。

| 类别 | 数量 | 处理 |
|---|---:|---|
| 印度期权（NIFTY/SENSEX/BANKNIFTY/MCX 期权卖方、跨式、BTST 等） | 62 | 不可移植：需印度期权链/IV 数据，nofx 也不交易期权 |
| 标的价格规则（日线/1h/5m 目标仓位） | 16 | `scripts.backtest volrix`，全部 FAIL 或无实质优势 |
| 日内止损止盈规则（5m/15m） | 6 | `scripts.backtest.volrix_intraday`，全部 FAIL |
| 三个原生币安策略 | 3 | 包含在上两类中（MAX20、EWMAC_VT、VWAP_RECLAIM） |

## 复现核对

| 策略 | 本地 vs Volrix | 对齐程度 |
|---|---|---|
| BTC 20 日新高（MAX20） | 收益 119.4% vs 133.2%，交易 186 vs 187 | 入场日 100% 一致 |
| ETH EWMAC 16/64 波动率目标 | 收益 228.6% vs 223.9%，交易 36 vs 38 | 入场日 97.3%（±1 日 100%） |
| BTC VWAP reclaim（5m） | 交易 6142 vs 6191，PF 0.93 vs 0.90 | 入场时间 ±5 分钟 98.4% |

规则移植正确。Volrix 的 Sharpe（2.25、3.48）明显高于我们按日权益计算的值（0.83、0.87），其算法未公开，不可直接比较。Volrix 加密回测**不计手续费和资金费**，这是其指标偏乐观的主要原因。

## 主要发现

- **资金费是永续长持的硬成本。** BTC 永续从 2019-09 满仓持有，累计资金费约 3,553 美元（本金 1,000），2022-11 权益一度为负；同期不计资金费终值 7,831。因此基准改用现货等价买入持有（计手续费、不计资金费）。
- **慢速趋势规则（金叉、SMA250、Faber、Supertrend 日线）本质上是"降回撤、降收益"。** BTC 上 Sharpe 0.53–0.59，均低于现货持有 0.79；ETH 前半段强、后半段弱。
- `SUPERTREND_D` / ETH 通过门槛：全样本 Sharpe 0.91 vs 持有 0.90、MaxDD 48% vs 79%；但后半段 0.25 vs 0.44，60/40 留出段 0.01 vs −0.03。22 条规则 × 2 币的多重比较下，这一勉强通过与随机结果相符。
- 高换手规则（1h/5m Donchian、1h Supertrend、6 条日内规则、POST_DOWN）被成本吃光：日内平均每笔净 −8 至 −18 bps，毛收益本身接近 0。
- **唯一值得留意的线索（事后观察，未预声明）：** MAX20 与 HIGH52 在 49 币等权组合（共同窗口 2023-08..2026-10）上 Sharpe 1.48 / 0.91、MaxDD 13.8% / 13.5%，等权持有为 0.76 / 72.9%。单币中位 Sharpe 只有 0.34 / 0.12，收益来自低敞口突破在山寨篮子上的分散。窗口短、存在幸存者偏差，若要跟进应作为截面因子交给 `scripts/factor_mining` 的冻结评估流程，而不是直接上线。
- 历史 AI 信号按日线趋势分组（2026-09-12..10-12，可匹配 187 笔）：期间 BTC 日线一直向上，逆势组每组只有 4–16 笔，无法判断日线趋势过滤对现有 AI 交易是否有帮助。

## 复现命令

```bash
env -u PYTHONPATH PYTHONDONTWRITEBYTECODE=1 /Users/zhangyun/workspace/nofx/.venv/bin/python -m scripts.backtest volrix \
  --rules all --out .volrix_out/volrix_report.md --json .volrix_out/volrix_report.json
env -u PYTHONPATH PYTHONDONTWRITEBYTECODE=1 /Users/zhangyun/workspace/nofx/.venv/bin/python -m scripts.backtest.volrix_intraday \
  --out .volrix_out/intraday.md --json .volrix_out/intraday.json
```

1h/5m K 线（BTC/ETH）在 `data/volrix/klines/`，日线与资金费复用 `data/factor_mining/crypto/`。口径与局限见 `scripts/backtest/README.md` 的 Volrix 一节。
