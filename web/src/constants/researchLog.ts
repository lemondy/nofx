// 量化研究记录：已尝试 / 回测 / 验证过的思路。新增研究时在数组最前面追加一条即可。
// 来源与原始报告见各条 source；同步的静态版本：docs/research/quant-research-log.html。

export type ResearchVerdict = 'none' | 'partial' | 'adopted' | 'running'

export interface ResearchTable {
  head: string[]
  rows: string[][]
}

export interface ResearchEntry {
  id: string
  date: string
  title: string
  domain: string
  verdict: ResearchVerdict
  verdictText: string
  /** 思路 / 起因 */
  idea: string
  /** 方法与要点 */
  paragraphs?: string[]
  bullets?: string[]
  table?: ResearchTable
  conclusion?: string
  source: string
}

export const RESEARCH_METHOD_NOTE =
  '所有带订单/盈亏的回测统一走 scripts/backtest（Backtrader）。加密货币费用 0.05%/边 + 0.03% 滑点，同币同向单仓（重叠过滤），并配时间错位的随机对照；统计推断按周聚类 SE。R = 以初始止损距离为 1 的盈亏倍数。“未发现优势”≠“已证伪”：多数样本只有 4–12 个月，且处于偏空行情，结论仅对该样本有效。'

export const RESEARCH_LOG: ResearchEntry[] = [
  {
    id: 'kronos-kit-assessment',
    date: '2026-11-10',
    title: 'Kronos 与 KiT（K 线基础模型）评估',
    domain: '美股 + 加密 · 外部资料 · 信号层（K 线预测）',
    verdict: 'none',
    verdictText: '证据不足 · 不接入；H1 开仓否决已预注册，待用户同意',
    idea: '用户要求深入分析两个开源 K 线基础模型：Kronos（arXiv 2508.02739，AAAI 2026；分层离散 token + 自回归 Transformer，45 个交易所的 120 亿根 K 线，预训练截止 2024-06，MIT 许可，safetensors 权重）和 KiT（arXiv 2609.34507；flow matching 扩散 Transformer，五维对数比 K 线编码，A 股 3914 只 / 美股 888 只 / 加密 28 个，AGPL 许可）。核对宣称指标、切分、偏差和成本，判断能否接入 nofx 的两个市场和两条路径。',
    bullets: [
      'Kronos：时间切分干净（测试从 2024-07 开始），推理代码无前视。收益 RankIC 主要来自 A 股（XSHG 0.064、XNAS 0.060、加密 0.033），线性模型 DLinear 已接近 Kronos。A 股回测超额 0.21 中，很多非预训练模型也为正，有规模暴露混入。',
      'KiT：论文回测 2025.05–2026.04 年化 +65%，但其中 8 个月落在验证期，按发布权重口径甚至在训练期；训练截止日在论文、权重说明、配置三处不一致。标的看起来只含当前仍在交易的，测试窗只有 3.3 个月（日线 3–5 个锚点），没有标准误。美股 RankIC 约为 0，加密只有 28 个币。波动率基线 GARCH 未拟合。权重需 torch.load(weights_only=False) 加载 pickle，有安全风险，未下载。',
      '对照 nofx：两者评价的都是截面排序，LLM 交易员需要的是单币方向和路径风险；fq 截面打分已有 ML 多特征失败的教训（RankIC 高而 P&L 差）；DVOL 研究已表明预测更准不等于决策更好。美股不提假设。',
      '唯一的低成本检验 H1：开仓前用 Kronos-small 零样本从 512 根 5m K 线生成 16 条 5 小时路径，不利路径 ≥ 12 条时否决开仓（B 类，crypto_llm K=5，439 笔冻结交易）。先验偏低，预注册 docs/research/2026-11-11-kronos-entry-veto-s1-prereg.md，未运行。',
      '合成数据冒烟测试（不计 K）：CPU 每次预测约 30 秒，同种子可复现。但在 50 条无漂移随机游走上，82% 的序列 16 条路径几乎同向，预测偏移中位 2.5σ（应约为 0），与窗口 z 分数相关 −0.78，12% 的 K 线不合法。模型读数近似“区间位置反转”，所以预注册加了同剔除笔数的窗口 z 分数代理对照，Kronos 必须胜过代理才进入 S2。',
    ],
    conclusion:
      '两个模型都不能直接接入 nofx（美股、加密，LLM 与 factor_quant 均是）。KiT 的收益宣称不可信，不推荐接入；Kronos 只值得做一次 H1 S1，是否运行由用户决定。本次未改任何交易配置，未用真实价格计算任何收益或 IC。',
    source: 'docs/research/2026-11-10-kronos-kit-assessment.md',
  },
  {
    id: 'quantbot-h2-h3-risk-report',
    date: '2026-11-10',
    title: '峰值回撤熔断选项与蒙特卡洛回撤分位（quantbot H2、H3 落地）',
    domain: '加密 + 美股 · 风险层 · 熔断与报告口径',
    verdict: 'partial',
    verdictText: '工程落地 · 默认关闭 / 仅描述，不计 K',
    idea: 'quantbot 评估留下的两项低成本改动。H2：LLM 交易员的账户熔断只按初始资金计算，账户先盈利后回吐时很晚才触发；新增可选口径 account_drawdown_basis=peak（账户级持久化高水位，默认关闭）。H3：配对报告只给单条路径的最大回撤，新增日收益重排 1000 次的 MaxDD 分位，只作描述。',
    bullets: [
      '历史权益描述性重放（只读）：因子 ensemble 候选初始口径最大回撤 15.6%、峰值口径 20.5%，阈值 20% 时峰值口径会在 11-08 停止开仓，初始口径不触发；对照为 11.0% 与 18.5%。',
      '实盘账户的峰值回撤被入金和手动交易放大（us-stock 初始口径 0.08%、峰值口径 12.5%），所以峰值口径默认关闭，只建议用于模拟盘或独立账户。',
      '重排分位示例：因子 ensemble 候选实际 MaxDD 17.3%，重排中位数 21.8%、第 95 百分位 29.2%，实际路径只排在第 15 百分位，说明单条路径回撤偏乐观。',
    ],
    conclusion:
      '两项都不改变现有交易行为：峰值口径需在策略里显式设为 peak 才启用，并需重新部署后端；回撤分位不参与配对判定。是否对某个模拟盘启用峰值口径由用户决定。',
    source: 'docs/research/2026-11-09-quantbot-math-assessment.md',
  },
  {
    id: 'corr-state-scaler-s1',
    date: '2026-11-10',
    title: '币种间相关性状态用于山寨币组合仓位缩放 S1',
    domain: '加密 · 风险与仓位层 · 仓位缩放',
    verdict: 'none',
    verdictText: 'S1 fail · 相关性不能提前预示风险，不接入',
    idea: '用户问题：很多币和 BTC、ETH 走势趋同，相关性是否有价值？现行止损风险上限已按相关性为 1 相加，所以只检验"相关性高时提前减仓"。每天按 20 日成交额取前 20 个山寨币（时点口径，含下架币）组成等权组合，比较 HAR（B）与 HAR + ln 分散化因子 D（C，D 由平均两两相关 ρ̄ 算出）驱动的只减不加缩放；TOP10、TOP50 为稳健性载体。训练 2021–2022，评估 2023-01 至 2026-10（1395 天）。crypto_llm K=4。',
    bullets: [
      '判定：TOP20 周超额风险损失配对 t = −0.18（前半为负），R1 不成立；ES C 0.13444 vs B 0.13452，几乎相同；TOP50 上 C 的 ES 还高于 B。判 fail。运行前修正了一处预注册矛盾：TOP10 成员不足 15，相关性门槛改为 min(15, ⌈0.75N⌉)。',
      '方向与直觉相反：47 次月度拟合中 ln D 的系数在三个载体上全部为负。也就是说，已知历史波动后，相关性越高，之后波动反而越低（暴跌后相关性升高、波动回落）。按 ρ̄ 五分位看，之后 7 日回撤和波动都不单调；2025-10-10 跌 −22.8% 的前一天 ρ̄ 为 0.50，接近中位数。',
      '描述：ρ̄ 均值 0.44，5 个同向山寨币仓位约等于 1.8 个独立仓位（N_eff 中位数），组合波动约为相关性为 1 时的 74%，所以现行按相关性为 1 计算的上限并不过度保守。HAR 缩放 ES 比不缩放低 0.7%–1.7%，与 DVOL 仓位 S1 一致。',
    ],
    conclusion:
      '币种间相关性确实很高，但现行风控已按最坏情况计算，相关性状态也不能提前预示风险，风险层没有增量；收益层此前的 BTC 对冲 S1 和风格中性化 S1 也已失败。按预注册不接入，不调参重跑。本次未改任何交易配置。',
    source: 'docs/research/2026-11-10-corr-state-scaler-s1-study.md',
  },
  {
    id: 'lob-imbalance-paper',
    date: '2026-11-10',
    title: '订单簿不平衡预测下一次中间价变动（VisualHFT 论文）评估',
    domain: '美股 + 加密 · 外部资料 · 执行与微观结构',
    verdict: 'none',
    verdictText: '证据不足 · 毫秒级信号，与 nofx 时间尺度不匹配',
    idea: '用户给出 VisualHFT Research 工作论文（2026-11-09 第 2 版，作者为被测软件厂商）：Nasdaq TotalView-ITCH 逐笔数据，每天最活跃的 100 只股票，按距最优价加权的前 10 档不平衡，预测下一次中间价变动的方向；回放前提交预分析计划，4 个留出日。按外部资料流程对照 nofx 的盘口数据、执行层和历史研究。',
    bullets: [
      '论文结果：留出日命中率 55.1%，基准 50.0%（z 16.9）；1 tick 价差时斜率 0.413，宽价差时 0.134；控制上一次变动和最近 1 秒主动成交量后仍保留 89%。但中位预测时长只有 2.46 毫秒，按方向计的平均变动 0.186 tick，平均半价差 5.73 tick（比值 0.032）。作者自评中档 MECHANISM：机制成立，跨价差交易不划算。',
      '对照 nofx：LLM 交易员是分钟级周期，factor_quant 为日频，与论文差 4–6 个数量级；系统只在点差门里取前 5 档 REST 快照，不存盘口历史；美股 LLM 交易员走币安的股票代币和永续，不是 Nasdaq 簿。订单流/VWAP 研究（PR #59）已显示主动成交量在分钟和小时级没有增量。',
      '唯一可借鉴：限价入场的执行层（Balanced、Aggressive、做空三个策略启用了 limit_entry）。H1：下单时和成交时只记录盘口不平衡，攒满 200 笔成交后检验它能否预测成交后 1、5、15 分钟的逆向选择。需要改实盘进程，先验偏低，由用户决定是否做。',
    ],
    conclusion:
      '论文的机制结论可信，但时间尺度、数据和成本都与 nofx 不匹配，没有可直接接入的内容（美股、加密都一样）。不建议把盘口不平衡作为方向信号写进 LLM 提示词。本次只读资料并只读查询配置，未改任何交易配置。',
    source: 'docs/research/2026-11-10-lob-imbalance-assessment.md',
  },
  {
    id: 'dvol-sizing-s1',
    date: '2026-11-10',
    title: '含 DVOL 的波动预测用于仓位缩放 S1',
    domain: '加密 · 风险与仓位层 · 仓位缩放',
    verdict: 'none',
    verdictText: 'S1 fail · 校准成功但缩放不如 HAR，不接入',
    idea: 'DVOL 波动预测 S1 通过后的仓位检验：校准后（加 s²/2）的 HAR + DVOL 与 HAR 各自驱动只减不加的缩放 k = min(1, σ_ref/σ̂)，在 BTC、ETH（各用自身 DVOL）与 SOL/BNB/XRP/DOGE 等权（借用 BTC DVOL）上，按平均敞口归一化后比较与方向无关的尾部风险（2023-01 至 2026-10，1400 天）。crypto_llm K=3。',
    bullets: [
      '校准成功：校准后 C 的 QLIKE 在 BTC 0.436→0.401、ETH 0.482→0.437，且 4 个山寨币中 3 个更好（XRP 例外）。',
      '风险结果反向：周超额风险损失配对 t（正为 C 更好）BTC −1.67、ETH −1.84、ALT4 −2.61；最差 2.5% 交易日 ES，C 都高于 B（BTC 0.0820 vs 0.0812、ETH 0.1143 vs 0.1101、ALT4 0.1063 vs 0.1046），判 fail。',
      '原因：DVOL 平滑、反应慢，C 触发减仓的天数少（BTC 6% vs B 17%），BTC 最大 5 次单日跌幅前有 4 次 C 是满仓。描述：只看历史波动的 HAR 缩放 B 比不缩放 ES 低约 3%–6%，非检验假设；LLM 近期交易期间 k 全为 1，叠加无效果。',
    ],
    conclusion:
      '预测更准不等于缩放更好：含 DVOL 的波动预测不接入仓位层，按预注册记录在案，不调参重跑。HAR 缩放的轻微改善只是描述，接入需另行预注册且优先级低。本次未改任何交易配置。',
    source: 'docs/research/2026-11-10-dvol-sizing-s1-study.md',
  },
  {
    id: 'dvol-vol-forecast',
    date: '2026-11-10',
    title: 'DVOL 对永续未来 7 日波动的增量预测 S1',
    domain: '加密 · 风险与仓位层 · 波动预测',
    verdict: 'partial',
    verdictText: 'S1 pass（风险层）· 水平需校准，未接入',
    idea: '来自《A Practical Guide to Quantitative Volatility Trading》评估的 H1：隐含波动率是未来已实现波动的前瞻性定价。BTC、ETH 上比较 HAR（Parkinson 日/周/月方差）与 HAR + Deribit DVOL 对未来 7 日已实现方差的样本外预测（2023-01 至 2026-10，扩展窗口每月重估，1400 天），不计 K。',
    bullets: [
      '对数口径样本外 R²：BTC 0.234 → 0.294（+6.0 个百分点），ETH 0.236 → 0.311（+7.5）；不重叠 7 日样本配对 t 2.46 / 2.68；前后两半都为正，判 pass。只用 DVOL 一个变量也有 0.300 / 0.332，单独就好于 HAR。',
      '反向信号：按方差水平计算的 QLIKE 加入 DVOL 后变差（BTC +0.034、ETH +0.007）。对数模型未做偏差修正会低估方差水平，所以不能直接用于仓位缩放，必须先校准。',
      '描述：隐含方差高于之后 7 日实际方差的时间 BTC 74%、ETH 69%（波动率风险溢价为正）；能事先算出的 DVOL − RV30 五分位与未来 7 日回撤、收益都没有单调关系，没有择时信号。',
    ],
    conclusion:
      'DVOL 能改善永续未来波动的预测（风险层增量筛选通过），但水平校准变差。接入前需另做预注册：用校准后、含 DVOL 的波动预测给永续仓位做缩放（LLM 的 vol_target 或 factor_quant 的 1/σ），与现行做法配对比较。本次未改任何交易配置。',
    source: 'docs/research/2026-11-10-dvol-vol-forecast-s1-study.md',
  },
  {
    id: 'vol-trading-book',
    date: '2026-11-10',
    title:
      '《A Practical Guide to Quantitative Volatility Trading》评估（Bloch 2016，327 页）',
    domain: '加密 + 美股 · 外部资料 · 期权与波动率',
    verdict: 'none',
    verdictText: '证据不足 · 期权教材，无可直接接入的策略',
    idea: '用户给出 SSRN 2715517（Daniel Bloch，2016）：期权定价、隐含波动率曲面、多重分形市场、MixVol 参数化模型、方差互换与离散度交易。按外部资料流程通读全书，并按“能否借鉴到永续交易”逐项对照 nofx。',
    bullets: [
      '全书没有任何回测或收益统计；唯一实证是 MixVol 在 BMW 期权上的校准。5.4 的策略（波动率套利、偏斜、日历价差、离散度交易）全部是期权组合，nofx 无法执行。',
      '可迁移到永续的两条：期权市场的前瞻信息（隐含波动率、波动率风险溢价）用于风险与仓位，即 H1，已预注册；截面平均相关性作为组合风险状态，即 H2，先验偏低，暂不做。',
      '已覆盖或无效：波动聚集与厚尾（regime-statistics 中波动特征有小幅增量），局部 Hurst / 多重分形择时（regime-statistics 无效）。',
    ],
    conclusion:
      '期权教材，没有可直接接入的策略；可借鉴的是把期权市场信息用在永续的风控与仓位上，先检验 DVOL 能否改进波动预测。',
    source:
      'docs/research/2026-11-10-quant-volatility-trading-book-assessment.md',
  },
  {
    id: 'crypto-funding-carry',
    date: '2026-11-09',
    title: '资金费 carry（现货多 + 永续空）离线 S1',
    domain: '加密 · 市场中性 · 新策略',
    verdict: 'partial',
    verdictText: 'S1 weak · 机制成立但近一年无收益，不接入',
    idea: '来自《The Math Inside a Trading Bot》评估的 H1。BTC、ETH 各半资本，过去 7 天年化资金费 ≥10% 时在次日开盘开仓（买现货、等量做空永续，3 倍保证金），<2% 时平仓；现货 0.10%、永续 0.05% taker 费加 0.03% 滑点。2020-01-01 至 2026-11-06，新 track crypto_carry，K=1；Backtrader 双数据源，与算术实现逐日核对误差 6e-15。',
    bullets: [
      '结果：年化 3.8%、z 0.99、前后两半 +4.2% / +3.4%、近一年 −0.06%、MaxDD 25.8%，判 weak（只满足两半为正）。',
      'MaxDD 几乎全部来自 2020-03-13 ETH 永续成交价插针（最高 323，而现货 139.7、标记价格 139.3）：预注册按成交价最高价判保证金击穿，交易所实际按标记价格强平，不会触发。事后只修正这一天（仅描述）：年化 7.7%、MaxDD 0.6%，但近一年仍为 −0.08%，判定不变。',
      '收益集中在牛市：2021 年约 +25%、2024 年约 +8%；2022 年以来除 2024 外接近 0。近一年接近 0 是因为资金费长期低于门槛、多数时间空仓。始终持有、零成本、门槛敏感性及 XRP、DOGE、BNB 单币版本的近一年也都接近 0。',
      '数据：Binance fundingRate 把 startTime=0 当作未传，只返回最近 500 条；已改为从 2019-09-01 分页并加回归测试，正式运行用完整历史。',
    ],
    conclusion:
      '资金费 carry 的机制真实，牛市阶段有可观收益，但近两年资金费被压缩，扣成本后接近 0，不值得为它给加密模拟盘补现货腿。等资金费重新长期处于高位时，再用新的样本外数据另行预注册。方法教训：涉及强平的回测应使用标记价格。',
    source: 'docs/research/2026-11-09-crypto-funding-carry-s1-study.md',
  },
  {
    id: 'quantbot-math-sheet',
    date: '2026-11-09',
    title: '《The Math Inside a Trading Bot》网页评估（48 张公式卡）',
    domain: '加密 + 美股 · 外部资料 · 知识速查',
    verdict: 'none',
    verdictText: '证据不足 · 无可直接接入的策略',
    idea: '用户给出网页 quantbot-math.netlify.app（@qwinsi0x）：交易机器人的 8 个阶段（数据、信号、过滤、优势、仓位、执行、风险、验证）各 6 张公式卡，每张有用途和常见致命误用。按外部资料流程逐张对照 nofx 代码和历史研究；与 11-07 评估的《一页纸量化理论》大量重叠。',
    bullets: [
      '48 张中 38 张 nofx 已实现、已检验或不适用（Z 分数回归、Hurst、波动状态切换、RSI、VWAP、凯利等都已检验无效），8 张部分覆盖，2 张未覆盖：资金费 carry（2.6）与蒙特卡洛回撤（8.6）。',
      '描述性核对（"亏钱的机器人先查执行"）：9-29 起实盘 AI 单 131 笔，未扣费 E[R] −0.017、手续费 0.027R；LLM 模拟盘 323 笔，未扣费 +0.023、手续费 0.039R。费用与优势同量级，但未扣费优势与 0 无法区分，问题仍在入场。',
      '假设：H1 资金费 carry（现货多 + 永续空，按资金费门槛持有）作为独立市场中性策略做 S1，数据已在因子面板 alt_daily；H2 LLM 交易员账户熔断改为按峰值计算（现按初始资金）；H3 模拟盘报告加蒙特卡洛回撤分位。',
      '不提：贝叶斯胜率替换 POOR_HISTORY 闸门方向合理，但影子样本只有 27 笔，无法检验；盘口不平衡、微观价格缺 L2 历史。',
    ],
    conclusion:
      '入门速查表，没有可直接接入的策略。唯一的新收益来源是资金费 carry，值得预注册离线 S1；峰值熔断和蒙特卡洛回撤是低成本的风险与报告改进。均需用户同意后再做，本次未改任何配置。',
    source: 'docs/research/2026-11-09-quantbot-math-assessment.md',
  },
  {
    id: 'early-trim-offline',
    date: '2026-11-09',
    title: '提前减仓 0.8R（vs 1.2R）离线复核 S1',
    domain: '加密 · LLM 交易 · 出场规则',
    verdict: 'none',
    verdictText: 'S1 fail · 两档差值≈0，建议结束线上配对',
    idea: '线上配对 early-trim-0p8r 跑了 16 天，avgR 差 −0.033R，但 SE 0.060R，按推演到 44 天也几乎不可能通过。减仓只改出场、不改入场，所以预注册离线复核：取 9-29 至 11-08 的 439 笔真实交易（实盘 AI 单 + 4 个 LLM 模拟盘），按 1m K 线收盘价模拟 0.8R 与 1.2R 两档减 1/3，残余仓位两臂都按实际出场价平仓，Backtrader 撮合并与算术实现逐笔核对。crypto_llm K=2。',
    bullets: [
      'Δ（0.8R − 1.2R）= −0.0004R/笔，周聚类 SE 0.0063、z −0.06，95% CI [−0.013, +0.012]R；前后两半 +0.011 / −0.012，判 fail。离线 SE 比线上配对小一个数量级，已排除 0.03R 量级的差别。',
      '抵消结构：MFE 在 0.8–1.2R 的 69 笔，0.8R 档多锁利 +0.123R；MFE ≥1.2R 的 78 笔，0.8R 档少赚 −0.110R；其余 292 笔两档相同。0.8R 触发后残余仓位平均再回落 0.08R，冲高与回落约各半。',
      '校验：439 笔全部有完整 1m K 线，入场价匹配 99.8%；用模型预测触发与 4 个模拟盘的 tp_trim_done 对比，一致率 93.5%。触及即成交、10bp 滑点两个敏感性版本 Δ 也都约为 0。',
      '描述项（未预注册、不作结论）：1.2R 减仓相对完全不减仓 +0.014R，z 2.66。',
    ],
    conclusion:
      '0.8R 和 1.2R 两个减仓档位的差别可以忽略，线上配对继续跑也给不出更多信息。建议提前结束 early-trim-0p8r 配对（需用户同意，尚未执行）；线上配置不变，仍为 1.2R。',
    source: 'docs/research/2026-11-09-early-trim-offline-s1-study.md',
  },
  {
    id: 'fq-shortexit-funding',
    date: '2026-11-08',
    title: 'fq_crypto 空头腿：浮盈回吐出场（G）与负资金费率排除（F）S1',
    domain: '加密 · 因子量化 · 出场规则 / 过滤器',
    verdict: 'none',
    verdictText: 'S1 两条均 fail · 线上不改',
    idea: '因子量化-加密模拟盘从峰值 +7.6% 回落到 +0.7%。按小时重算发现主因是 AKE、ERA、ICNT 三个空头被逼空（合计 −649 USDT），加上大盘下跌拖累多头腿（−272）和资金费（−49），并不是缺少止盈。据此预注册两条只针对空头腿的改动，在 val（2025-05 至 2026-04）和 forward（2026-05 至 2026-11）上用 Backtrader 加小时路径，与现行规则（f_0ce8b2ee 因子，20% 止损）配对比较。',
    bullets: [
      'G 空头浮盈 20% 后回吐一半即平仓（K=7）：z 0.31，val +0.55 bp/日、forward −0.40 bp/日，判 fail；次要因子组 z −1.53。79 次回吐出场锁住约 12% 浮盈，但离场后价格走向的中位数约为 0，回吐并不预示被逼空。',
      'F 排除 7 日资金费率均值低于 −0.01%/8h 的空头（K=8）：z −1.07，两段都为负，安慰剂分位 0.69，判 fail。val 段空头资金费从 75.7 降到 12.5，但净收益从 +26.7% 掉到 +13.0%：负资金费率的币往往还在下跌，排除它们削弱了因子本身“资金费率越低越做空”的部分。',
      '小时线校验：11935 个被持有的（币, 日）全部有完整小时线，与日线收盘价 100% 吻合。固定 20% 止损按小时路径与按日线路径结果逐日相同，所以 PR #61 关于固定止损的结论不受日线口径影响。',
      '基线本身 forward −5.7%（Sharpe −0.47）：模拟盘回吐的根源仍是入场没有优势加空头凸性，不是出场规则。fq_crypto 累计 8 个 S1，全部 fail。',
    ],
    conclusion:
      '两条空头腿改动都没有通过 S1，线上不做任何改动。在 val、forward 两段上，止盈类出场（含 11-01 的日线研究）和按资金费率筛空头都没有增益，不再沿这个方向调参。',
    source: 'docs/research/2026-11-08-fq-crypto-shortexit-funding-study.md',
  },
  {
    id: 'ic-overlap-h1',
    date: '2026-11-08',
    title: 'H1：zoo 准入 t 改用不重叠口径（预注册备选 B）',
    domain: '加密 + 美股 · 研究方法 · 因子评估',
    verdict: 'partial',
    verdictText: '评估器已改为不重叠口径 · 已部署 11-08',
    idea: '接续重叠样本 IC 审计。计算前登记：主口径为均匀核 HAC（滞后 h）+ Student t(df=⌊n/h⌋−1) 换算到正态 z；判据是 12 组空模型的 |z|>1.96 在 2%–8% 且 z 标准差在 0.85–1.15；不通过只允许退到备选 B（偏移 0 每 h 日取样的普通 t，df=k−1 换算 z）。然后用只读干跑脚本，在 11-07 的 zoo 上按旧/新口径各重分类一遍。',
    bullets: [
      '主口径误判率 2.5%–8.0%，但加密 AR h=1 组 z 标准差 1.15002 超出上限，判为不通过（h=1 时各方法都偏高，原因是收益的一阶横截面自相关）；备选 B 的 12 组误判率 2.5%–7.0%、标准差 0.873–1.103，通过，评估器改为 B（evaluate.py sha256 78f70129…），与审计脚本的 B 数值逐个相同。',
      '干跑（旧口径重现原状态 100%）：加密 h=7 accepted 12→0，美股 1→0、候选 11084→200；加密 h=3 accepted 2→1（f_eda12cac 训练 z 4.64，门槛 4.65），h=1 4→3（f_a241597b 4.57）。',
      '模拟盘影响：加密因子量化 h=3 入选因子会从 {f_0ce8b2ee, f_eda12cac} 变成只剩 f_0ce8b2ee；夜间任务会把多出的 rejected 裁出 zoo，所以部署前要备份。',
    ],
    conclusion:
      '评估口径修正已实现并通过校准，旧口径确实放进了一批证据被高估的慢因子。用户确认后于 2026-11-08 合并（PR #104）并部署到夜间运行副本，部署前已备份 data/factor_mining/backup-pre-ic-overlap-h1-20261108-200839；从 11-09 02:30 那次夜间任务起按新口径重分类，加密因子量化模拟盘的 h=3 组合随之只剩 f_0ce8b2ee。交易配置、提示词和实盘未改。',
    source: 'docs/research/2026-11-08-ic-overlap-h1-study.md',
  },
  {
    id: 'ic-overlap-audit',
    date: '2026-11-08',
    title: '重叠样本 IC 显著性审计：挖矿评估器的 NW lag=h 仍然偏松',
    domain: '加密 + 美股 · 研究方法 · 因子评估',
    verdict: 'none',
    verdictText: '诊断 · 慢因子误判率约 2 倍，评估器已按 H1 修正',
    idea: '用户贴来的帖子：预测周期 h 大于 IC 计算间隔、因子又变得慢时，重叠样本 IC 的普通 t 会严重虚高，NW（滞后 h−1）也只修到一半。nofx 冻结评估器 factor_mining/evaluate.py 每天算 RankIC、用 Bartlett NW lag=h 算 t，正是帖子里“NW 滞后 19 天”那一档。用与收益独立的随机因子（iid 与 AR(1) 0.98）配真实训练段标签（2022-05 至 2025-05）做空模型校准，每个市场 200 个种子，并重算 zoo 已准入因子的不重叠 t。诊断性质，不计 K。',
    bullets: [
      '理论：t 放大倍数在 NW lag=h 时，h=3/7/20 分别为 1.13/1.18/1.21，lag=4h 时约 1.04；在 K≈15000 的门槛 4.65 处，单次误判概率是名义值的 6–8 倍。',
      '空模型 |t|>1.96 比例（慢因子）：普通 t 29%–57%，现行 NW lag=h 为 9%–11%（加密 h=3、h=7 都是 11%），NW lag=4h 为 8%–9.5%，每 h 天取一次样 2.5%–5.5%；iid 随机因子在各口径下都是 2.5%–6.5%，说明虚高来自“重叠 + 持续性”。',
      '准入因子：加密 h=7 的 12 个 accepted 换成不重叠 t 后训练 t 为 2.79–4.15，全部低于 4.65（验证段仍 ≥2.3）；美股唯一的 h=20 accepted 训练 2.53、验证 1.16，两道门槛都不过。加密 h=1 的 4 个、h=3 的 2 个仍然通过。',
      '影响面：实盘因子量化（加密）用 h=3 的目标文件，美股用动量规则，都不受影响；历史 S1 中失败的结论方向不变，判“通过”的动量与波动缩放 S1 本来就用不重叠 20 日周期；cycleic 已有不重叠 t；全部使用 RankIC，帖子里 Pearson 被极端值拽偏的问题已规避。',
    ],
    conclusion:
      '现行评估器偏松但不是普通 t 那种量级；建议（需用户同意，因为是冻结文件）把 zoo 准入 t 改为不重叠或 Hansen-Hodrick 口径并重分类，预计清空加密 h=7 与美股的 accepted，h=3 实盘不变。本轮未改评估器、zoo、交易配置、提示词与模拟盘。',
    source: 'docs/research/2026-11-08-ic-overlap-audit.md',
  },
  {
    id: 'macro-event-window-s0',
    date: '2026-11-08',
    title: '宏观事件窗口 S0：FOMC/CPI/非农前入场的止损触发率',
    domain: '加密 · 风险机制 · 事件窗口',
    verdict: 'none',
    verdictText: '描述 · 机制成立，但实盘窗口内入场仅 2%，不做 S1',
    idea: 'dao-quant-research 评估的 H1：事件发布时波动跳升，事件前几小时按 ATR 设定的止损容易被直接打掉。补下 BTC/ETH/SOL/BNB/XRP/DOGE 永续 2020-01 至 2026-11 的 1h K 线（0 缺口），从美联储新闻稿和 BLS 档案原文整理 220 次实际发布时刻（FOMC 57、CPI 81、非农 82，含 2020 年紧急会议与 2025 年停摆推迟）。在事件前 0–6 小时的每个整点放多空各一笔 1.5×ATR14 止损的假想入场，看 12 小时内是否触及，对照为同币、同 UTC 整点、±1–4 周且离任何事件 ≥48 小时的普通时段；按事件聚类，配 1000 次时间错位 placebo。登记先于计算，只描述、不计 K。',
    bullets: [
      '主结果 D=+0.094（触发率约 62% 对 52%），95% CI [+0.072, +0.117]，placebo 分位 1.000（假事件均值 +0.002、标准差 0.011），判定机制成立；220 个事件中 159 个为正。',
      'FOMC +0.159、CPI +0.083、非农 +0.060；k∈{1,1.5,2.5}×H∈{6,12,24} 全部显著；六个币都显著；2021 与 2026 年 CI 含 0 或贴边，其余年份显著，没有系统性减弱。',
      '机制形态：发布前 6 小时 |收益| 比普通时段低约 10%（ATR 被低估），发布那一小时 1.76 倍、下一小时 1.24 倍，两小时后恢复；观察窗区间/ATR 1.20 倍。',
      'S1 潜在样本（只计笔数）：加密已平仓 759 笔中只有 16 笔（2.1%）在事件前 0–6 小时入场，主交易员 436 笔中 7 笔（1.6%）。',
      '偏离披露：ZCode 触发速率限制无产出、ALFRED 超时，事件时刻改由主 agent 直接核对官方原文（文件名日期与禁发行一致）；定义和判定未改。',
    ],
    conclusion:
      '机制真实而稳健，但只说明止损更易被触及，不等于亏损；nofx 实盘在窗口内入场极少，按登记设计做 crypto_llm B 类过滤 S1 几乎必然证据不足，建议现在不做 S1，作为风险常识记录。交易配置、提示词与模拟盘未改。',
    source: 'docs/research/2026-11-08-macro-event-window-s0-study.md',
  },
  {
    id: 'dao-quant-research',
    date: '2026-11-08',
    title: 'laozdao/dao-quant-research「双引擎四层融合模型」知识库评估',
    domain: '加密 + 美股 · 方法论 · 外部资料',
    verdict: 'none',
    verdictText: '证据不足 · 无可直接接入内容，H1/H2 待定',
    idea: '用户给出 GitHub 仓库 laozdao/dao-quant-research（main 1ba7b89，MIT），要求分析其量化方案能否融合借鉴。按 AGENTS.md 外部资料流程完成阅读、对照与实验方案。仓库 349 个文件中 345 个是 Markdown，没有代码、数据或回测产物；核心是面向 A 股的“双引擎四层融合模型”（基本面 60% + 量价 25% + 风控 15%，0–100 分、五档评级），其余是行业/个股分析、120 多篇宏观事件随笔、开源项目介绍和证书指南。',
    bullets: [
      '回测数字不可复现且前后矛盾：M07-01 称 v1.0 年化 19.5%、基准 8.2%，按同表六个年度收益复利只有 18.6% 和 4.6%；M07-02 对同一模型给出另一套年度收益（2019 年 32.5% vs 38.5%）；v0.1 于 2024-06 才开始，却有 2019 年起的收益。M03-08 的 FOMC 事件表把 t=2.31、2.12 标成 p<0.01。',
      '对照 nofx：基本面引擎（美股 SEC 基本面 S1 0/18）、IC/IR 与状态权重（MSRR、多特征 ML、regime 研究）、Alpha101、订单流、相关集中度与波动缩放都已实现或已检验；“筹码分布”需要 A 股持仓数据，不适用。ai-hedge-fund 的波动分档 + 相关性乘数仓位规则已被现有单笔 R 定额与同向集中度限制覆盖。',
      '未覆盖的只有两处，证据都来自资料引用的外部工作：H1 宏观事件窗口（FOMC/CPI/非农前入场被事件波动打止损；先做 S0 机制描述，现有 1h 缓存只有约 32 个事件，需补 5 年主流币 1h 数据，再用 incremental_filter 做 crypto_llm B 类过滤 S1，K=2）；H2 开仓前反方复核（TradingAgents 多空辩论的最小版本，用 llmreplay 离线重放配噪声组，成本高，且因子否决过滤的前例是交易变少、方向相反）。H3 GTJA191 与 Alpha101 同族，不单独立项。',
    ],
    conclusion:
      '证据不足，没有可直接接入的内容。H1 值得先做 S0 机制描述，H2 留作备选，二者都需另写预注册并经用户同意。交易配置、挖掘器与模拟盘未改。',
    source: 'docs/research/2026-11-08-dao-quant-research-assessment.md',
  },
  {
    id: 'fq-crypto-admission-arith',
    date: '2026-11-07',
    title: 'fq_crypto 挖掘器准入修正：算术多空（含资金费）显著性门槛的影响',
    domain: '加密 · 因子量化 · 挖掘器准入',
    verdict: 'none',
    verdictText: 'no_factors · 新门槛下 0 个因子通过',
    idea: '凸性分解显示秩 IC 看不到空头凸性，而挖掘器准入主门槛是秩 IC（t≥4.63），算术多空只要求 ls_ret>0（只看符号、不计资金费）。在冻结快照 zoo（13,157 个因子）上保持秩 IC 部分不变，把符号条件换成“训练与验证的算术五分位多空净收益（扣成本、计资金费）ls_t 均 ≥1.65”（A），并描述不计资金费的 A0 和只加资金费仍看符号的 S_f；按挖掘器顺序重做冗余判定，B′ 非空才做前向配对 S1（fq_crypto K=7）。预注册先于计算；S0 复现 13,157 个状态零差异、40 个 accepted 的 ls_ret 误差 1e-16。',
    bullets: [
      '判定 no_factors（不计 K）：h=1/3/7 秩 IC 部分通过 5,268/3,626/2,711 个，原规则 accepted 7/7/26，S_f 5/2/22，A0 与 A 全部为 0。不计资金费也是 0，问题在算术多空本身。',
      '秩 IC 合格因子的训练段算术多空 ls_t 中位数为负（h=1 −2.06，h=3 −1.52，h=7 −0.78）；训练 ls_t 最高的 h=1 量价相关因子验证段约 −2.4。事后放宽到两段 ≥1.0 仍为 0。',
      '线上 h=3 的 7 个因子算术多空 t 在训练段 0.003–0.13、验证段 0.03–0.42（不计资金费），计资金费后多为负；当初靠秩 IC 加“ls_ret>0”通过。',
    ],
    conclusion:
      '候选池在算术意义上没有可兑现的多空边际，不是门槛太严。若把准入改成 A，fq_crypto 组合为空、等于停掉这条模拟盘。用户 2026-11-07 决定维持现状只观察：挖掘器准入、每晚任务与模拟盘不变，fq_crypto 结果不作为扩大或接入实盘的依据。',
    source:
      'docs/research/2026-11-07-fq-crypto-admission-arith-prereg.md · docs/research/2026-11-07-fq-crypto-admission-arith-study.md · scripts/factor_mining/admission_arith_study.py · data/factor_mining/admission_arith/s1_v1/report.md',
  },
  {
    id: 'fq-crypto-shortleg-convexity',
    date: '2026-11-07',
    title: 'fq_crypto 空头腿凸性分解：秩 IC 为什么高而多空亏钱',
    domain: '加密 · 因子量化 · 机制诊断',
    verdict: 'none',
    verdictText: '描述 · 机制成立：空头凸性吃掉排序收益',
    idea: '来自《一页纸量化理论》评估的 H3（图中 4.5 凸性/Jensen）。恒等式：空头持有期价格盈亏 −R = −l − (R−l)，l=ln(1+R)，凸性项 R−l≥0 永远是空头成本，期望约 σ²T/2；秩 IC 对单调变换不变，看不到这一项。对线上 7 因子合成 B 与 ML 线性岭 R，按 fq_crypto 原口径（每 3 日、前后 20%、开盘进出、资金费）逐币逐期分解，核对与 vectorized_replica 逐日一致（误差 ≤7e-16），R 复现 ML 报告 IC 与分腿收益。登记先于计算，只描述、不计 K。',
    bullets: [
      '验证+前向 180 期：B 空头价格 P_S −0.43 = 对数部分 L_S +2.42 + 凸性 C_S −2.84（资金费另 −0.15），ρ=6.7；R 为 −0.91 = +2.00 − 2.92，ρ=3.2。两者都判“机制成立（强）”：对数意义上做空排序是对的，亏损全部来自凸性。',
      'B 秩 IC 0.105（t 5.2）；五分位多空对数收益 +1.18%/期（t 2.6），算术收益 −0.20%/期（t −0.4），训练段同样（算术 t 0.37 vs 对数 2.06）。最高波动组对数 −1.20%/期、算术 +0.42%/期、凸性 1.62%，空头腿 74% 在这一组；凸性可由事前 σ̂²T/2 部分预测（斜率 0.54，R² 0.07）。',
      '尾部：涨幅 >30% 的空头币期占 3.9%，价格亏损合计 −4.19，其余 96% 空头币期合计 +3.76。挖掘器准入主门槛是秩 IC 的 t≥t_req，算术多空只要求 ls_ret>0（不看显著性、不计资金费），B 训练段算术价差 t 0.37 仍通过。',
    ],
    conclusion:
      '解释了 fq_crypto “IC 高、多空亏”：因子识别的是“高波动币的中位数更低”，但高波动币算术均值比中位数高约 σ²T/2，空头腿正好付这一项。研究口径应改为看算术多空净收益（含资金费）或按波动调整的标签；挖掘器准入的修正会改变每晚结果，需另立方案经用户同意。交易配置未改。',
    source:
      'docs/research/2026-11-07-fq-crypto-shortleg-convexity-prereg.md · docs/research/2026-11-07-fq-crypto-shortleg-convexity-study.md · scripts/factor_mining/shortleg_convexity_study.py · data/factor_mining/shortleg_convexity/s1_v1/report.md',
  },
  {
    id: 'quant-theory-sheet',
    date: '2026-11-07',
    title: '《the entire quant theory on one sheet》量化速查表评估',
    domain: '加密 + 美股 · 方法论 · 外部资料',
    verdict: 'none',
    verdictText: '无可直接接入内容 · H3 机制成立，H1/H2 待定',
    idea: '用户给出一张《the entire quant theory on one sheet》长图（@RitOnchain，10 层 50 个公式：概率、统计、线性代数、优化、时间序列、随机微积分、衍生品、组合与风险、微观结构、研究与 ML），要求识别方案并分析能否融入 nofx。按 AGENTS.md 外部资料流程完成阅读、对照与实验方案。资料是面试/入门速查表，没有数据、代码、回测或可执行规则；图中数值例子全部复算正确（贝叶斯 50%、BS 看涨 10.45、DSR 门槛 1.14 等）。',
    bullets: [
      '对照 50 张卡片：第 2、3、8、10 层（标准误、多重检验、PCA/风格、收缩、均值方差、凯利、回撤熔断、滚动验证、DSR、精确率）nofx 已实现或已检验并否定；第 6、7 层（随机微积分、期权）与第 9 层做市不适用；盘口不平衡已在 breakout 模块计算，但无历史 L2 无法检验。',
      '未检验的只有：4.5 凸性/波动拖累、5.5 GARCH、5.3–5.4 系统化协整配对、8.5 ES。协整配对因加密短周期反转被换手吃掉（alpha101）、单币空头被右尾和资金费吃掉（pit60-btchedge）而不提出。',
      '假设：H1 fq_us 熊市状态停空头腿（Daniel-Moskowitz，叠加波动率缩放；但 SPY 24 个月收益为负在 2012–2026 年只有 40 天，效力几乎全靠已看过的 2008–10）；H2 用 GARCH/EWMA 替换 126 日波动做缩放（先验偏低）；H3 描述性分解 fq_crypto 空头腿亏损中的凸性项 σ²T/2（日波动 6–8% 时 3 日约 0.54–0.96%，与 ML 研究空头腿每期约 −0.65% 同量级）。',
    ],
    conclusion:
      '证据不足，没有可直接接入的内容。H1 针对已知动量崩溃风险但统计效力弱，H2 先验低，二者都需另写预注册并经用户同意后做 S1（fq_us K 8/9）；H3 已完成（fq-crypto-shortleg-convexity）：空头腿亏损确实来自凸性，对数意义下排序正确。交易配置未改。',
    source: 'docs/research/2026-11-07-quant-theory-sheet-assessment.md',
  },
  {
    id: 'fq-us-ml-multifeature',
    date: '2026-11-07',
    title:
      'fq_us 信号层：多特征机器学习打分（线性岭 / 随机特征岭）vs 12-1 动量',
    domain: '美股 · 因子量化 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fail · 显著输给动量',
    idea: '用户要求把加密货币上的多特征机器学习检验也用到美股。35 个特征（30 个与加密版同名的通用价量特征 + SPY 贝塔/特质波动 + 12-1、6-1 动量与 52 周高点），在标普 500 宽池（498 只，2012–2021，每 5 日抽样）上预测 20 日收益截面秩；两个判定臂线性岭 R 与随机傅里叶特征岭 RF，各配置换标签噪声组。模型分数替换 fq_us 线上 mom_ls_invvol_v1（90 只 bStock、每 20 日、前后 20% 多空、波动率倒数加权、Backtrader）的动量排序，其余不变；预注册早于加密结果。数据已看过，两池有幸存者偏差，只作筛选。',
    bullets: [
      'S1 两臂 fail（fq_us K=6/7，已记账）：60 个周期，按周期聚类；R z=−3.25（验证 −0.117%/日，前向 −0.176%/日），RF z=−2.96，都显著差于动量 B；只做多变体同样落后（z −2.55、−2.22）。',
      'B 验证 +80%、前向 +192%（收益来自多头腿）；R −21.9%、+53.2%，RF +0.3%、+42.7%。宽池样本外 IC 很弱：R 训练 0.053 → 验证 0.014、前向 0.026，RF 0.066 → 0.011、0.016；RF 不优于 R。',
      '模型偏好高波动、低流动性、短期反转（对 vol_30 相关 +0.65、对 dvol_30 −0.49），在 bStock 上验证段 IC 为负（R −0.035，t −4.4），与动量多头组只重合 25–34%。空头腿各臂两段都亏。',
    ],
    conclusion:
      '美股上多特征模型样本外信号弱、迁移到 bStock 后 IC 为负，显著输给 12-1 动量（基线被 bStock 幸存者偏差抬高，但宽池 IC 本身也弱）。结合加密结果，“多特征 + 收缩”在两个市场都关闭；线上不变。',
    source:
      'docs/research/2026-11-07-fq-us-ml-multifeature-prereg.md · docs/research/2026-11-07-fq-us-ml-multifeature-study.md · scripts/factor_mining/us_ml_multifeature_study.py · data/factor_mining/ml_multifeature/us_study/s1_v1/report.md',
  },
  {
    id: 'fq-crypto-ml-multifeature',
    date: '2026-11-07',
    title:
      'fq_crypto 信号层：多特征机器学习打分（线性岭 / 随机特征岭）vs 等权因子合成',
    domain: '加密 · 因子量化 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fail · IC 很高但多空更亏',
    idea: '来自 Kelly & Xiu 的“复杂度的美德”。当前挖掘逐条搜索公式、等权合成，没有模型同时学习多个特征。用 45 个特征（38 个事先固定的通用价量/资金费特征 + 7 个线上因子），每日截面秩变换，预测 3 日超额收益的截面秩；两个判定臂：线性岭 R（45 维）与随机傅里叶特征岭 RF（4096 维），训练段 5 折分块交叉验证选 λ 后冻结，各配一个日内置换标签的噪声组。模型分数直接替换 B 的打分，组合与执行完全相同（每 3 日、五分位多空，Backtrader 扣成本与资金费）。数据已被多项研究看过，只作筛选。',
    bullets: [
      'S1 两臂 fail（fq_crypto K=5/6，已记账）：R 合并 z=−2.25（验证 −0.077%/日，前向 −0.073%/日），显著差于 B；RF z=−0.57；两臂都不如各自噪声组（z_noise −0.53、−1.00）。',
      '截面秩 IC 很高：R 验证 0.113（t 7.2）、前向 0.081（t 4.6）；RF 0.104、0.070。但多空扣成本后验证 −36.6%/−14.2%、前向 −30.7%/−31.0%（B −16.4%、−20.8%）。亏损集中在空头腿（R 验证空头 −29.9%、资金费 6.7%；前向空头 −32.9%，多头 +0.3%）。',
      '模型学到的就是低波动：分数对 30 日波动率截面相关 R −0.92、RF −0.76，对 B 0.94、0.83；交叉验证两臂都选最强收缩，RF 样本外 IC 与 R 相同，没有非线性增量。噪声组前向反而赚（+0.3%、+7.3%）。',
    ],
    conclusion:
      '多特征、复杂模型只会把低波动风格学得更纯：IC 更高、多空更亏。fq_crypto 的瓶颈是秩 IC 与盈亏错位（空头腿撞上少数暴涨高波动币并付资金费），不是预测精度。线上不变；该方向关闭，不再换树模型重试。若继续改进，应在空头腿风险约束或以收益幅度为目标上另立假设。',
    source:
      'docs/research/2026-11-07-fq-crypto-ml-multifeature-prereg.md · docs/research/2026-11-07-fq-crypto-ml-multifeature-study.md · scripts/factor_mining/ml_multifeature_study.py · data/factor_mining/ml_multifeature/study/s1_v1/report.md',
  },
  {
    id: 'fq-crypto-msrr',
    date: '2026-11-07',
    title: 'fq_crypto 组合权重：岭收缩 MSRR vs 等权五分位多空（H1）',
    domain: '加密 · 因子量化 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fail · 回撤降低但收益无改善',
    idea: '来自 Kelly & Xiu 综述评估的 H1。fq_crypto 线上组合是 7 个因子 z 分数等权、前后 20% 多空；IC 为正但多空亏钱。改为 MSRR：把每个因子的多头腿、空头腿各作一个管理组合（14 列）再加 BTC 列，训练段（2022-05→2025-05，363 期）用岭收缩求夏普最优权重，5 折分块交叉验证选收缩参数，β 冻结；币级权重归一到总杠杆 1、单币 ≤10%。与基线 B（等权五分位，style-neutral 研究同口径）在验证段与前向段做配对 Δ（周聚类 SE），Backtrader 扣成本与资金费。数据已被多项研究看过，只作筛选。',
    bullets: [
      'S1 判 fail（fq_crypto K=4，已记账）：合并日均 Δ +0.025%，z=0.24；验证段 +0.027%（z 0.27），前向段 +0.020%（z 0.09）；BTC 权重占比 10.2%。',
      '回撤明显变小（验证 41.5%→21.6%，前向 38.3%→16.1%），但 M 仍亏（−4.0%、−14.6%），换手为 B 的 1.5–1.8 倍；回撤变小来自 15 条腿互相抵消、方向性风险降低。',
      'β 过拟合：交叉验证选中网格最小收缩，训练样本内夏普 3.0 → 交叉验证 0.83 → 验证/前向亏损；多个因子的多头腿系数为负、空头腿为正。分解后验证段空头赚、多头亏，前向段反过来，没有稳定的腿。去掉 BTC 列（M₀，只描述）z=0.30。',
    ],
    conclusion:
      '组合权重这一层救不回来：问题在信号（7 个因子本质是低波动风格），不在权重分配。线上不变；H3（联合收缩准入）不再推进；H2（部分调仓）只省成本，B 扣掉全部交易成本后仍亏，不建议单独做。',
    source:
      'docs/research/2026-11-07-fq-crypto-msrr-prereg.md · docs/research/2026-11-07-fq-crypto-msrr-study.md · scripts/factor_mining/msrr_study.py · data/factor_mining/msrr/study/s1_v1/report.md',
  },
  {
    id: 'kelly-xiu-financial-ml',
    date: '2026-11-07',
    title: 'Kelly & Xiu《Financial Machine Learning》综述评估',
    domain: '加密 + 美股 · 方法论 · 外部资料',
    verdict: 'none',
    verdictText: '无可接入内容 · H1 已检验 fail',
    idea: '用户给出 Kelly 与 Xiu 的综述《Financial Machine Learning》（SSRN 4501707，159 页），问能否融入 nofx。按 AGENTS.md 外部资料流程完成阅读、对照与实验方案。正文 133 页全部读过（其中 3 段为补读），参考文献没读。综述没有代码、数据和可直接交易的规则，数字均来自所引论文的美股月频样本。',
    bullets: [
      '可借鉴：①多重检验当选择器会过于保守，应对候选池做联合收缩（p.102–104），对应挖掘器逐因子 Bonferroni 准入（K≈1.5 万、t≥4.65）；②MSRR：对因子管理组合做带岭收缩的夏普最优权重（p.112–118）；③交易成本下的部分调整 w_t = m·w_{t−1} + (1−m)·A_t（p.126–130）。',
      '已覆盖或不适用：经济指标优先、时间切分（增量评估框架已有）；信号层 ML 过滤已失败（meta-labeling AUC 0.43–0.54）；复杂模型需要信号分散且样本大，加密 60 币 × 4 年、因子主要是低波动风格，不适用；ML 收益主要来自小盘低流动性股（p.126），与美股挖掘暂停一致；LLM 新闻嵌入缺历史新闻数据。',
      '校准预期：Da 等（2022）测得美股可实现夏普约 0.5（扣成本前），只有不可实现值的约 1/4（p.101）。',
    ],
    conclusion:
      '证据不足，没有可以直接接入的内容。提出 3 个待检验假设：H1 用 MSRR 岭收缩组合权重替换 fq_crypto 的等权打分加五分位多空（针对“IC 为正但多空亏钱”，优先）；H2 部分调整降换手；H3 联合收缩准入（先验低，H1 通过后再做）。需另写预注册并经用户同意后做 S1。',
    source:
      'docs/research/2026-11-07-kelly-xiu-financial-ml-assessment.md · ~/Downloads/ssrn-4501707.pdf',
  },
  {
    id: 'us-fundamental-factors',
    date: '2026-11-06',
    title: '美股基本面因子（SEC EDGAR 按时点）检验',
    domain: '美股 · 因子量化 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fail · 0/18，SUE 只卡训练段',
    idea: '量价因子三步检验后，用 SEC EDGAR 按时点构建的基本面数据（PR #90 调研，本分支数据层：提交后下一交易日可用，重述按自身可用日生效，超过 200 天置空，拆股一致市值，8-K 2.02 财报生效日）检验 9 个经典基本面因子：GP/A、ROE、B/M、E/P、S/P、应计项、资产增长、SUE、财报日超额收益。持有 20/60 日，共 18 个检验。宽池为标普 500 现成分股，训练 2012–21、验证 2022–24、前向 2025–26，Bonferroni 训练 t ≥ 2.99，再迁移到 78 只有季度财报的 bStock。',
    bullets: [
      'S1 判 fail（fq_us K=5，已记账）：宽池通过 0/18，可迁移通过 0/18。',
      '价值类（B/M、E/P）训练段反向、2025–26 转正；GP/A、ROE 前向为负；应计项只在训练段显著（t 3.3/3.5），之后失效。',
      'SUE 是唯一近期方向一致为正的信号：验证 t 2.4/2.5，宽池前向为正，bStock 前向 t 2.1；但 2012–21 年 IC 约为 0，只卡在 G1，与 12-1 动量同样是“近几年才有效”。',
    ],
    conclusion:
      '补上基本面数据也没有改变结论：2012–2026 年美股大盘股的单因子溢价随时期翻转，宽池样本量与按时点取数都足够，问题不在数据缺口。若要用 SUE，只能另立预注册（K=6），作为动量规则的附加项直接进在线配对；否则可结束美股因子挖掘，维持动量加风控。',
    source:
      'docs/research/2026-11-06-us-fundamental-factors-prereg.md · docs/research/2026-11-06-us-fundamental-factors-study.md · scripts/factor_mining/fundamentals.py · scripts/factor_mining/us_fundamental_factor_study.py · data/factor_mining/fundamentals_study/s1_v1/report.md',
  },
  {
    id: 'us-fundamental-data-sources',
    date: '2026-11-06',
    title: '美股基本面数据源调研',
    domain: '美股 · 因子量化 · 数据',
    verdict: 'partial',
    verdictText: '调研 · SEC EDGAR 可用',
    idea: '美股量价三步检验无因子，诊断认为缺基本面信息。实测调研免费、可编程、能按时点回放的基本面数据源：SEC EDGAR companyfacts / submissions / frames、Nasdaq 盈利意外、Yahoo 报表序列，并核对标普 500 与 bStock 90 的覆盖率。',
    bullets: [
      'SEC EDGAR companyfacts 每个数值带 filed/accn，可以按“截至 t 首次披露”回放，申报从 2009 年起；submissions 中 8-K Item 2.02 的 acceptanceDateTime 给出精确的财报发布时点。',
      'Nasdaq 盈利意外只有最近 4 季、Yahoo 报表只有约 4–5 年且无提交日，都不能用于回测；分析师预期没有免费历史。',
      '覆盖率：标普 500 共 503 只全部映射到 CIK；bStock 90 只中 78 只有季度 us-gaap XBRL，另有 12 只外国发行人（ARM、ASML、TSM、BABA、PDD 等）没有。',
    ],
    conclusion:
      '可以用 EDGAR 加 Yahoo 价格构建 GP/A、ROE、B/M、E/P、S/P、应计项、资产增长、SUE、财报日超额收益。正式抓取需要用户提供 SEC User-Agent 联系方式；下一步是数据管道加预注册 S1（fq_us K 加 1）。文献中这些因子在大盘股里同样偏弱，不保证有结果。',
    source:
      'docs/research/2026-11-06-us-fundamental-data-sources.md · data/factor_mining/fundamentals_probe/coverage_20261106.json',
  },
  {
    id: 'style-neutral-factors',
    date: '2026-11-05',
    title: '因子风格中性化：加密 ensemble 的信息有多少是风格',
    domain: '加密 · 因子量化 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fail · 因子主要是低波动/低贝塔',
    idea: '来源 Quant Bible（MIT）的遗漏变量偏差 / 回归解剖。对线上 fq_crypto 的 h=3 ensemble（7 个因子）做风格中性化：每日把因子百分位秩对 5 个风格秩（30 日成交额、60 日 BTC 贝塔、30 日波动率、7 日与 30 日动量）做 OLS，取残差再合成。B 原始 vs N 中性化，Backtrader 每 3 日调仓、五分位多空、扣成本与资金费，在验证段与前向段做配对 Δ（周聚类 SE）。另描述加密 zoo 全部 40 个 accepted 因子和美股 top300 候选。数据已看过，只作筛选。',
    bullets: [
      'S1 判 fail（fq_crypto K=3，已记账）：合并日均 Δ +0.011%，z=0.15；验证段 −0.038%，前向段 +0.107%，中性化没有稳定改善多空。',
      '7 个因子与 30 日波动率截面相关 −0.48～−0.84，风格 R² 0.54～0.77；ensemble 秩 IC 验证 0.115→0.029、前向 0.077→−0.005。只用 5 个风格合成的分数 IC 验证 0.094、前向 0.070，与 ensemble 几乎相同，多空同样亏。',
      '40 个 accepted 因子风格 R² 中位数 0.54，验证 IC 中位数 0.082→0.017；美股 top300 候选风格 R² 仅 0.09，中性化前后都没有 t ≥ 1.65。',
    ],
    conclusion:
      '加密挖掘出的因子本质上是低波动、低贝塔异象的不同写法，秩 IC 好看但多空亏钱（空头腿是高波动币），去掉风格后几乎没有独立信息。线上 fq_crypto 实际在做低波动减高波动。改进方向是挖掘器准入加风格中性门槛（另需预注册），按本次结果现有因子大概率无一能留下。',
    source:
      'docs/research/2026-11-05-style-neutral-factors-prereg.md · docs/research/2026-11-05-style-neutral-factors-study.md · scripts/factor_mining/style_neutral_study.py · data/factor_mining/style_neutral/study/s1_v1/report.md',
  },
  {
    id: 'fq-us-volscale',
    date: '2026-11-05',
    title: 'fq_us 动量多空 + 波动率缩放',
    domain: '美股 · 因子量化 · 增量评估',
    verdict: 'partial',
    verdictText: 'S1 screen_pass · 降回撤，宽池仍不赚钱',
    idea: '2008 压力测试显示 mom_ls_invvol_v1 在 2009 年反弹中亏损 29%。按文献（Barroso 和 Santa-Clara、Daniel 和 Moskowitz）加波动率缩放：每次调仓按未缩放组合 126 日波动率与其扩展中位数之比，取 k=min(1, σ_ref/σ̂)，两腿敞口乘以 k，只缩不放。在 D1（标普 500，2006–2011H1）、D2（bStock，2021–2026）、D3（标普 500，2012–2026）三组上与基础规则配对，要求三组夏普都不低、最大回撤都更低，且 D1 不触发 25% 熔断。数据都已看过，只作筛选。',
    bullets: [
      'S1 判 screen_pass（fq_us K=4，已记账）：夏普 D1 −0.65→−0.48、D2 0.90→0.98、D3 0.01→0.17；最大回撤 35.4%→20.7%、26.0%→20.1%、29.1%→14.6%；D1 缩放版没有触发熔断。',
      '2009 年反弹窗从 −29.1% 降到 −12.0%；平均缩放系数 D1 0.83、D2 0.91、D3 0.78，最小约 0.3。',
      '重要提醒：基础规则在 D3 宽池 14 年累计 −3.4%（夏普 0.01），在 bStock 上却是 +134%，说明 S1 的好结果主要来自名单的选择偏差。缩放能降风险，不能制造收益。',
    ],
    conclusion:
      '波动率缩放作为风控层有效。按预注册，下一步需另写在线配对设计（缩放版新交易员对现行 S2），由用户决定；现行 S2 不改。宽池结果提示：对动量规则本身的预期应放低。',
    source:
      'docs/research/2026-11-05-fq-us-volscale-prereg.md · docs/research/2026-11-05-fq-us-volscale-study.md · scripts/backtest/fq_us_volscale_study.py · data/factor_mining/us_volscale/study/s1_v1/report.md',
  },
  {
    id: 'fq-us-momentum-stress-2008',
    date: '2026-11-05',
    title: 'fq_us 动量多空规则：2008 压力测试（描述性）',
    domain: '美股 · 因子量化 · 压力测试',
    verdict: 'none',
    verdictText: '描述性 · 2009 反弹亏 29%，25% 熔断触发',
    idea: '用户问美股量化是否考虑 2008 这类极端行情。线上 S2 规则 mom_ls_invvol_v1（12-1 动量、20 日调仓、多空各 0.5、1/σ）原样放到 2005-01→2011-06 的 Yahoo 复权日线上，复用 S1 的选股与 Backtrader 两腿：bstock90（冻结名单中当年有数据的 40 只）与 sp500（当前成分 432 只），均有幸存者偏差。线上做法（每次调仓多空重设各 0.5）与 25% 回撤 / 7% 单日熔断按日收盘后处理。只作描述，不计 K，不改 S2。',
    bullets: [
      'sp500 等权重置多空 2006–2011H1 −31.1%（SPY +12.8%），最大回撤 35.4%；崩盘窗 +3.9%（空头腿 +101% 抵消多头腿 −53%），反弹窗 −29.1%（空头腿 −68%），2009 年 −27.1%：典型动量崩溃。',
      '25% 回撤熔断按收盘在 2009-05-04 触发（回撤 26%），之后空仓，总收益 −23.6%；7% 单日熔断从未触发（最差单日 −4.6%）。',
      'bstock90 每条腿只有 3–5 只：等权重置多空 +7.6%，回撤 22.1%，无熔断；样本太窄，参考价值有限。',
    ],
    conclusion:
      '多空不对冲熊市后的急速反弹，规则收益高度依赖市场阶段（2021–26 大赚，2006–11 宽池亏 31%）。25% 熔断能把损失限制在约四分之一，但是一次性锁定、需人工重置；单日熔断对连续数周的动量崩溃无效。缓解办法（按波动率缩放、熊市状态下停空头腿）需另立预注册，K 加 1；正在运行的 S2 不改。',
    source:
      'docs/research/2026-11-05-fq-us-momentum-stress-2008.md · scripts/backtest/fq_us_momentum_stress.py · data/factor_mining/us_stress/study/v1/report.md',
  },
  {
    id: 'us-range-reversal',
    date: '2026-11-05',
    title: '美股候选排序：52 周区间位置 / 6 个月反转 vs 12-1 动量',
    domain: '美股 · LLM 交易路径 · 候选排序 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fail · 反转排序明显更差',
    idea: '来自 Yartseva (2025) 10 倍股论文：6 个月动量系数为负、越接近 12 个月高点下一年收益越低。论文按结果选样（只看已涨 10 倍的股票），不能直接当选股规则，只检验其技术面部分。us_stock 扫描现按 TopMomentum 选候选：动量有效且高于 SMA200 的个股中按 mom12_1 取前 5。E 类改动：同一合格池内改按 52 周区间位置（V1）或 6 个月动量（V2）从低到高取前 5，比较之后 20 个交易日收益。数据为 90 只 bStock 个股的 Yahoo 复权日线（排除 11 个 ETF/杠杆 ETF），2021-11 至 2026-10 共 1166 天；Newey-West lag 19，us_llm 线 K=1、2。',
    bullets: [
      'S1 两个变体都判 fail（已记账）：V1 每日 Δ = −4.72%/20 日，z = −3.36（z_K 1.96），两半 −2.6%/−6.8%；V2 Δ = −4.25%，z = −2.77（z_K 2.24），两半 −2.8%/−5.7%。',
      '随机前 5 安慰剂（200 次）：随机挑选平均比动量前 5 少 3.2%/20 日，两个变体比全部随机结果都差（分位 0%）。',
      '全币池 RankIC 三个信号都为正：range_pos 20 日 +0.073（t 3.4），mom6 +0.046，mom12_1 +0.057。越接近 52 周高点之后越强，是动量延续，不是反转。去掉 SMA200 条件仍为负；变体换手为动量排序的 3.4–3.7 倍。',
    ],
    conclusion:
      '不进入线上配对，线上排序不变。论文的反转是“已知赢家”内部的选样均值回归。现行 12-1 动量 + SMA200 排序在现有币池内跑赢随机和等权，但币池是 2026 年仍上架的 bStock（多为近年走强后上架），动量优势会被幸存者偏差高估。',
    source:
      'docs/research/2026-11-05-us-range-reversal-prereg.md · docs/research/2026-11-05-us-range-reversal-study.md · scripts/factor_mining/us_range_reversal_study.py',
  },
  {
    id: 'fq-us-momentum-ls',
    date: '2026-11-05',
    title: 'fq_us 动量多空单一信号（mom_ls_invvol_v1）',
    domain: '美股 · 因子量化 · 增量评估',
    verdict: 'partial',
    verdictText: 'S1 screen_pass · 非样本外，空头腿为负',
    idea: 'fq_us 无因子（第 1 步标普 500 经典因子 0/30，第 2 步挖掘器多样化父代 fail），只有 12-1 动量在近期与 bStock 上方向一直为正；用户要求 fq_us 能多能空。预注册规则 mom_ls_invvol_v1：90 只 bStock 个股，12-1 动量每 20 个交易日调仓，前 20% 现货做多、后 20% 永续做空，各 0.5 总敞口，1/σ 加权、单只上限 0.1，成本多 13bp/空 7bp。S1 用 Backtrader 在 2021-11→2026-10 共 62 个周期上检验，判定 z ≥ 1 且两半均值 > 0。声明：数据已看过、名单有幸存者偏差，S1 只筛可行性，最终判定靠 S2 在线模拟。',
    bullets: [
      'S1 判 screen_pass（fq_us K=3，已记账）：每周期均值 +3.02%，z = 2.44，两半 +0.62%/+5.43%。两腿每期重置为各 0.5 的复核 z = 2.11，结论不变。',
      '收益全部来自多头腿：多头腿 +840%（相对 5 万），空头腿 −64%（两半均为负，仅 2022 年盈利）；只做多 +920%，等权持有 +157%，SPY +77%。',
      '多头 +840% 远超等权，主要反映 bStock 名单的事后选择偏差；另有 9 只股票没有永续映射，实际不能做空。',
    ],
    conclusion:
      '通过的是可行性筛查，不是样本外证据。S2 需用户批准后接入（quant_targets 新规则 + fq_us 永续空头腿）才启动。空头腿回测为负贡献：要么照预注册跑多空 S2，要么另立只做多预注册（K=4），不能事后改规则。',
    source:
      'docs/research/2026-11-05-fq-us-momentum-prereg.md · docs/research/2026-11-05-fq-us-momentum-study.md · scripts/backtest/fq_us_momentum_study.py · data/factor_mining/us_momentum/study/s1_v1/report.md',
  },
  {
    id: 'us-miner-diverse-parents',
    date: '2026-11-05',
    title: '美股因子挖掘器：多样化变异父代（解决因子库塌缩）',
    domain: '美股 · 因子量化 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fail · 两组均 0 个通过验证',
    idea: '诊断：us 因子库 h=10 前 300 个候选按 0.8 秩相关聚类只有 1 簇，偏向大盘高流动性（与 −log 成交额秩相关 −0.33；该代理训练段 IC −0.063、验证段 ≈0）。成因是变异父代 = 训练 t 前 20，全在同一簇。改动（开关 FM_DIVERSE_PARENTS，默认关）：父代池与 LLM top-10 在训练 t 前 300 中按训练段秩相关 < 0.8、每个 family_fine 至多 4 个贪心挑选。A/B：从同一冻结快照出发，B 现状 vs D 改动各跑 4 晚（1500 变异、无 LLM、同种子），主指标为新增 accepted 且保留段前向 confirmed 的因子数。“K 按簇计”未做：卡住的是验证段而非 t_req，且会放松多重检验。',
    bullets: [
      'S1 判 fail（fq_us K=2，已记账）：B、D 两组新增 accepted 都是 0，前向 confirmed 都是 0。',
      '前 300 名里按规则只能选出 1～4 个父代（不是 20 个），D 组父代池基本只剩随机种子：塌缩比诊断时更彻底。',
      'D 组训练通过的新因子 104 个（B 组 513），新增簇数相当，验证 IC 中位数仍为负（−0.010 对 −0.013）。B 组新增因子已分布在 4～8 个簇，同样无一通过验证。',
    ],
    conclusion:
      '第 2 步无增量，线上夜间任务不打开该开关，代码保留为默认关闭。结合第 1 步（标普 500 经典因子 0/30），问题不在搜索方式或池子宽度：2022–25 训练段学到的美股截面方向在 2025–26 验证段不成立。剩下的选项是为 12-1 动量另立预注册（K+1），或暂停 fq_us。',
    source:
      'docs/research/2026-11-05-us-miner-diverse-parents-prereg.md · docs/research/2026-11-05-us-miner-diverse-parents-study.md · scripts/factor_mining/diversity.py · scripts/factor_mining/miner_ab_study.py · data/factor_mining/us_miner_ab/report_v1/report.md',
  },
  {
    id: 'us-broad-classic-factors',
    date: '2026-11-05',
    title: '美股宽池（标普 500）经典因子检验：能否给 fq_us 提供因子',
    domain: '美股 · 因子量化 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fail · 0/30 通过',
    idea: 'fq_us 从未调仓（no_validated_factors）。诊断发现：us 因子库 1.1 万多个通过训练的因子 99.9% 属于成交量族（变异自我强化），验证期 IC 中位数为负；每日可用标的只有 76–86 只。假设：在宽池上，预先登记的 10 个经典价量因子（短期反转、周反转、12-1/6-1 动量、低波动、特质波动、52 周高点、彩票效应、低贝塔、非流动性）× 5/10/20 日，按 Bonferroni t ≥ 3.14 + 验证 t ≥ 1.65 + 前向 IC > 0 能通过，并能迁移到 90 只 bStock 个股。数据：标普 500 当前成分 498 只，15 年 Yahoo 复权日线（有幸存者偏差）。',
    bullets: [
      'S1 判 fail（fq_us K=1，已记账）：宽池通过 0/30，可迁移通过 0/30。',
      '低波动、特质低波动、低贝塔、彩票效应四个防御类因子在 2012–26 三段 IC 全为负：高波动成长股系统性跑赢。',
      '12-1 动量训练段（2012–21）≈ 0，2022 年后转正，bStock 前向 IC +0.04～0.06；只卡在 G1。非流动性过 G1 但验证衰减。反转因子在 bStock 上为负。',
    ],
    conclusion:
      '“宽池 + 经典因子”不能解决美股无因子：问题不只是样本小，而是美股因子溢价本身不稳定。12-1 动量是唯一在近期与 bStock 上方向稳定为正的信号（us-stock 候选排序已在用），若要作为 fq_us 单一信号需另立预注册、K+1，不能事后放行。',
    source:
      'docs/research/2026-11-05-us-broad-classic-factors-prereg.md · docs/research/2026-11-05-us-broad-classic-factors-study.md · scripts/factor_mining/us_broad_classic_study.py · data/factor_mining/us_broad/study/classic_v1/report.md',
  },
  {
    id: 'llm-long-veto',
    date: '2026-11-04',
    title: 'LLM 加密交易员：因子弱币做多否决（B 类过滤）',
    domain: '加密 · LLM 交易路径 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fewer_trades · 否决集反而更好',
    idea: 'fq_crypto 的 S1 显示：在全量时点 top 60 上重新验收的 20 个因子（F1）前向 RankIC 0.19，但信息集中在“谁会跌”，单币空头无法兑现。改为 B 类过滤器：LLM 交易员准备做多时，若该币 F1 分数在开仓前一日处于全币池（不设 N 上限，覆盖约 91% 的 AI 做多）最低 20%，则否决这次做多；做空、出场、仓位不变。样本是 4 个不含因子字段的加密 LLM 交易员（实盘 deepseek + 3 个 glm 模拟盘）在 2026-10-31 之前的 AI 归因做多平仓，约 138 笔（排除疑似手动单）；用 incremental_filter 做多头内随机剔除安慰剂（2000 次），crypto_llm 线 K=1，z_K 1.96。',
    bullets: [
      'S1 判 fewer_trades（crypto_llm K=1，已记账）：138 笔 AI 归因做多中 71 笔被否决（p = 51%，命中 p > 0.5 规则）。S0 通过：F1 RankIC 逐日复现，覆盖率 91%。',
      '方向与假设相反：被否决集 avgR −0.046，保留集 −0.234；Δ = −0.097R，z = −1.47，安慰剂分位 7%，后半段 −0.16。总 R 变好（+3.2）只是因为基线本身亏损。',
      '描述：阈值 0.10/0.30 也都为负（z −1.97/−1.07）；F2 现有 ensemble 为负（z −0.88/−0.93）；对称做空否决无信息（z 0.21）。实盘 117 笔单独看方向相同。被否决的主要是 BTW、MUBARAK、AKE、CAP、PHA 等高波动小币。',
    ],
    conclusion:
      '不进入 S2，按预声明不调参重试。LLM 做多的币有一半落在因子判为弱的一侧，但 LLM 带止损的短线做多在这些币上并不更差：因子预测的是 3 日截面相对收益，与 LLM 的交易对象不同。至此，截面因子在加密市场的三种用法（独立多空、对冲多头、LLM 否决器）都没有通过，只剩 A 类参考字段的线上配对在跑。',
    source:
      'docs/research/2026-11-04-llm-long-factor-veto-prereg.md · data/factor_mining/crypto_full/prereg_20261104b/f1_selected.json · docs/research/2026-11-04-llm-long-factor-veto-study.md · scripts/factor_mining/llm_long_veto_study.py · data/factor_mining/crypto_full/study/llm_long_veto/report.md',
  },
  {
    id: 'pit60-btchedge',
    date: '2026-11-04',
    title: '因子量化-加密：时点 top 60 全量币池 + BTC 对冲空头腿',
    domain: '加密 · 因子量化程序化交易员 · 增量评估',
    verdict: 'none',
    verdictText: 'S1 fail · 不进入线上',
    idea: '用户选择了“时点 top 60 + 空头腿改造”作为下一步。冻结 11-03 nightly 的 zoo/ensemble 快照（h=3 共 11 个因子），比较四组：B 为现状；UH 为主变体，用全量时点 top 60（含已下架币），把 3866 个 h=3 表达式在新币池上按 K=16502（t_req 4.669）重新验收并组合，空头腿改为按多头篮子 beta 做空 BTC；U 和 H 分别只换币池、只换空头腿，仅作拆解。主检验是前向 2026-05-07 至 2026-10-31 的 UH − B 每日收益配对差（按周聚类，K=2，z_K 2.24）；S0 要求 B 组复现 quant_targets 回测。',
    bullets: [
      'S1 判 fail：前向 2026-05-07～10-31 共 178 天，UH − B 每日 Δ = +0.064%（SE 0.145%），z = 0.44（z_K 2.24），前半段为负。已记账，fq_crypto K=2。S0 复现与 quant_targets 回测完全一致。',
      '四组前向收益：现状 B −14.5%，UH +BTC 对冲 −1.4%，U 全量池 + 单币空头 −35.2%，H 现有因子 + BTC 对冲 +1.2%。改善几乎全部来自“不做单币空头”；换币池本身无增量（UH − H 的 z 为 −0.93）。',
      '在新币池上重新验收：94 个因子通过 t ≥ 4.67，前 20 个与快照组合重合为 0，几乎全是负方向的波动率/振幅因子（做多低波、做空高波）。前向 RankIC 达 0.19（t 9.0），而基线为 0.08。',
      '但排序信息集中在“谁会跌”，无法兑现：U 组单币空头在 val 段赚 +59%，前向亏 −32%。3 日涨幅 >30% 的空头持仓贡献了 63% 的空头亏损，还要倒贴负资金费（每 1000 初始资金付出 76）。多头腿在所有组前向都为负（−1.3% 到 −3.2%）。',
    ],
    conclusion:
      '不进入 S2，按预声明不调参重试。加密截面因子的信息主要是识别弱币：单币空头因右尾拉盘和负资金费无法兑现，多头一侧跑不赢 BTC。这不是币池宽度或因子数量的问题。唯一未试的方向是把因子作为 B 类过滤器，在 LLM 交易路径上否决做多弱币，需另写预声明并让 K 加 1。',
    source:
      'docs/research/2026-11-04-pit60-btchedge-prereg.md · docs/research/2026-11-04-pit60-btchedge-study.md · scripts/factor_mining/pit60_hedge_study.py · data/factor_mining/crypto_full/study/pit60_btchedge/report.md',
  },
  {
    id: 'crypto-universe-expansion',
    date: '2026-11-03',
    title: '加密因子挖掘股票池扩展：含已下架币的全量 USDT-M 永续',
    domain: '加密 · 因子挖掘数据',
    verdict: 'partial',
    verdictText: '建议时点 top60 含已下架币 · 不扩到 150',
    idea: '用户问为什么加密因子挖掘池只有 60 个币，而美股有几千只。现有挖掘池是固定的 104 币名单（实际 100 个），只包含今天还在交易的币：训练窗口 2022-05～2025-05 每天只有 35–83 个币有数据，LUNA、FTT 等 166 个已下架币完全缺席。这次从 data.binance.vision 拉取了全部 685 个 USDT-M 币本位永续（519 个在交易、135 个 SETTLING、31 个已从 exchangeInfo 删除）的 1d K 线、资金费和 premium 月度归档，写入新目录 data/factor_mining/crypto_full。然后做时点普查，计算有效宽度、成本和拉盘尾部，并用冻结评估器把 zoo 的 40 个 accepted 因子和冻结组合在 top 60/100/150 上重算（只用 train/val，holdout 仍封存）。',
    bullets: [
      '数据校验：6.4 万个归档文件全部 sha256 一致；OHLC 异常 0 条，重复日期 0 条。2026-10-28 是 Binance 归档的全市场缺口，已用 fapi 为 518 个在交易的币补齐。59,309 根下架后的零成交量平盘 K 线已剔除。100 个存量币与现有 fapi 数据逐日一致（只有 TRUMP 1 天差 0.018%）。',
      '普查：训练窗口内全市场平均 223 个币在交易，其中 133 个日成交额 ≥$20M；现有名单平均只有 52 个。时点 top 60 与现有名单平均只重合 30 个，约 15% 的成员后来被下架；top 150 中这一比例约 21%。',
      '有效宽度：去掉截面均值后，特征值参与比在 top 60/100/150 下分别为 17.8 / 22.3 / 25.3（训练段）。名数扩大 2.5 倍，独立下注数只增长 1.4–1.75 倍，IR 理论上限提高约 20%–30%。',
      '成本与风险：盘口快照下 $10k 单边成本，1–60 名 3 bp，61–100 名 10 bp，101–150 名 12 bp，151–250 名 14 bp（配置是 8 bp）。7 日涨幅 >50% 的概率各档都在 1.2%–1.4%，与排名无关；下架率则随排名上升，151–250 名是前 60 名的 2.6 倍。',
      '重算：换成全量时点池后，因子中位 train t 从约 5.6 升到约 8。加回已下架币后，三个周期的 val 多空都比仅现存币 top 100 好 14–51 个百分点。但从 60 扩到 100、150 时，三个周期的多空都下降。h=3 组合（fq_crypto 所用）的 val 多空在 base60 / 全量 60 / 100 / 150 上分别为 +12% / −1% / −18% / −26%；空头腿亏损中来自 3 日涨幅 >30% 持仓的份额从 27% 升到 50%–57%。',
      '重新挖掘的影响：现有 40 个 accepted 因子在全量 top 60/100/150 下分别保留 30/19/14 个。K 应与原账本合并计算：K≈24,402 时 t_req 从 4.61 升到 4.75。fq_crypto 的基线变更需要按增量框架做配对评估。',
    ],
    conclusion:
      '建议重新挖掘时改用含已下架币的时点 top 60（N 不变）。这一步能消除幸存者偏差，并让 IC 的 t 提高约 2.5，但它本身不能让 fq_crypto 的 h=3 组合赚钱，瓶颈仍是空头腿的拉盘尾部。所以应与空头腿改造一起预注册、作为配对变更评估。N=100 等空头腿改造后再考虑；不建议 N=150（成本 12–36 bp，下架风险更高，多空为负）。切换线上挖掘池需用户另行决定，本研究没有改动 config、zoo、ensemble 或交易员。',
    source:
      'scripts/factor_mining/full_universe.py · scripts/factor_mining/universe_expansion_study.py · docs/research/2026-11-03-crypto-universe-expansion.md · data/factor_mining/crypto_full/study/result.json',
  },
  {
    id: 'wide-pool-ensemble',
    date: '2026-11-03',
    title: '加密宽因子池组合（40 个低相关因子 vs 现有 14 因子）',
    domain: '加密 · 因子挖掘 · 增量评估',
    verdict: 'none',
    verdictText: '无增量（S1 fail）',
    idea: '成熟多因子策略用几十个低相关因子一起打分，而现有因子量化-加密只用 14 个 accepted 因子等权。按增量评估框架做第一个用例：从 zoo 里 h=3 训练期达标的 3650 个因子中，按训练 ICIR 贪心选出 40 个两两秩相关 <0.5 的因子，权重按训练 IC 再向等权收缩一半；持仓规则、费用、资金费都不变，只把打分换掉，在 2026-05-04 之后的前向区间与现有方案做每日收益配对差。预声明在运行前提交。',
    bullets: [
      '主检验：Δ = −0.125%/日，周聚类 SE 0.085%，z = −1.47（z_K 1.96），前后两半都为负，判 fail，账本 fq_crypto K=1。验证段配对差同样为负（z −2.33，对基线有利）。',
      '信号层几乎相同：前向 RankIC 基线 0.073（t 2.99），宽池 0.076（t 3.08），差 0.0025（t 0.38）；随机 40 因子安慰剂中排第 82.5 分位。为凑够 40 个低相关因子要考察 3294 个，池子高度同质。',
      '亏损来自空头腿：前向空头腿净损益基线 −140、宽池 −327（初始 1000），多头腿 +8 / +3；宽池偏向波动率类因子（13/40），把高波动币排到末档，多吃了暴涨右尾，换手多 45%。',
      '基线本身前向也亏：当前因子量化-加密规则 2026-05-04 之后 −13.2%（年化约 −25%），同样主要亏在空头腿。',
    ],
    conclusion:
      '因子数量不是瓶颈，组合构建（空头腿）才是。不按本次结果调参重试；后续优先做空头腿改造（只做多、剔除暴涨/新币并按波动率缩仓、截尾收益挖掘目标），以及扩充含已下架币的币池后再看多因子组合，均需新的预声明并计入 K。',
    source:
      'scripts/factor_mining/wide_pool_study.py · docs/research/2026-11-03-wide-pool-ensemble-prereg.md · docs/research/2026-11-03-wide-pool-ensemble-study.md',
  },
  {
    id: 'fib-fvg-bos',
    date: '2026-11-01',
    title: '斐波那契 + FVG + BOS 入场模型',
    domain: '加密 · 程序化入场模型',
    verdict: 'none',
    verdictText: '无可交易 edge',
    idea: '用户问能否在自动量化交易员中融合：等结构突破（BOS），回撤进入 FVG，FVG 与斐波那契 0.618–0.786 重叠处挂限价，止损放在结构外，目标为下一个流动性池。10-18 的 ICT 研究已测过其中 5 步，这次只补斐波那契过滤和“不要求先扫流动性”两点。预注册 4 个变体 × 2 种出场：V1 用户版、V2 去掉斐波那契、V3 10-18 “full” 加斐波那契、V4 “full” 原样。100 币 5m K 线（2025-10-15～2026-10-17），Backtrader，0.05%+0.03% 成本，同几何随机错位对照。',
    bullets: [
      'G0 随机游走校准通过（与对照差 t −1.1）；G1/G2/G3 全部不通过：V1/ERL 净 avgR −1.13（周聚类 t −7.6），比随机对照还差 0.50R（t −3.2），开发段和留出段都为负。',
      '5m 结构止损很窄（中位 0.36%），R 口径被成本放大。换成每笔收益率：V1 毛收益约 −0.05%、净 −0.22%，扣费前就亏。',
      '斐波那契过滤没有增量：V1 与 V2 每笔净收益 −0.216% 对 −0.214%；V3 − V4 t −0.15。',
      '止损至少 1×1h ATR（参考列）时，毛收益约 +0.05%/笔，略好于随机对照，但覆盖不了 0.16% 成本。“先扫流动性”的版本只有 34/67 笔成交，无法判断。',
    ],
    conclusion:
      '无可交易 edge，不并入因子量化交易员，也不单独做程序化交易员。10-18 的 ICT 结论在加入斐波那契共振、去掉扫荡前提后仍然成立。',
    source:
      'scripts/backtest/fib_fvg_study.py · docs/research/2026-11-01-fib-fvg-bos-prereg.md · docs/research/2026-11-01-fib-fvg-bos-study.md',
  },
  {
    id: 'fq-stop-tp',
    date: '2026-11-01',
    title: '因子量化-加密：单币止损阈值与分批止盈',
    domain: '加密 · 因子量化程序化交易员',
    verdict: 'partial',
    verdictText: '保留 20% 止损 · 止盈无增量',
    idea: '用户要求把因子量化-加密的空单止损从 +50% 降到 20%、多单加 −20% 止损，并问止损阈值是否合理、能否分批止盈。预注册 11 个变体，用 Backtrader 重放 ls_invvol_protected_v1（冻结 h=3 ensemble，5bp+3bp 加真实资金费，日线，同根 K 线先止损）：无止损、现行空 50%、多空 30/20/15/10%、波动缩放 2.5σ√3，以及在 20% 之上叠加的 4 种止盈：10% 止盈一半、10%/20% 各 1/3、移动止损、止盈一半后保本。判定以 forward（2026-05 起）为主，要求块自助法 P≥0.90 且 val 同向。',
    bullets: [
      'S0 重放复现此前描述性回测（val +27.1%，forward −12.7%），撮合与记账未被扩展改变。',
      '20% 优于现行空 50%：forward −8.3% 对 −17.3%，Sharpe −0.56 对 −1.20，回撤 25.8% 对 31.3%；val +38.4% 对 +25.3%。两段同向，但 P(差<0)=0.14，不显著。forward 中约 11% 的空单持仓触发 20% 止损。',
      '其他止损无一达标：10% 在 forward 最好（−2.8%），但 val 变差；15% 与波动缩放都不稳定。',
      '分批止盈 T1/T2 在 val 与 20% 持平，在 forward 更差，净收益更低；保本止损和移动止损两段都更差。但移动止损受日线“同根先高后低”的悲观假设影响很大，不能据此断定无效。',
    ],
    conclusion:
      '保留用户选择的多空 20% 止损（PR #60），不接入止盈。所有变体 forward 都为负，问题在入场信号而非出场。移动止损若要公平评估，需用 1h K 线重做并另行预注册。',
    source:
      'scripts/backtest/fq_stop_tp_study.py · scripts/backtest/quantjourney.py · docs/research/2026-11-01-fq-stop-tp-prereg.md · docs/research/2026-11-01-fq-stop-tp-study.md',
  },
  {
    id: 'orderflow-vwap',
    date: '2026-11-01',
    title: '主动买卖差（delta/CVD）与 VWAP 偏离：相对已有指标的增量',
    domain: '加密 · 大模型提示词指标',
    verdict: 'none',
    verdictText: '无增量',
    idea: '用户问能否给大模型交易链路加均线、VWAP、布林带、RSI、MACD、成交量、delta、CVD、成交量分布和大单。前五项提示词已有；成交量分布 10-26 研究已关闭；大单需要逐笔数据、成本高。剩下的 delta/CVD 和 VWAP 能从已拉取的 K 线零成本算出，于是先做预注册离线验证：100 币 1h K 线（2025-10-15～2026-10-16），6 个特征（4h/24h 主动买卖失衡、价格未解释的资金流、距 24h/当日/7 日 VWAP 的 ATR 倍数）× 未来 4h/24h 截面超额收益，门槛为 G0 零假设校准、G1 开发集（|t|≥3 且扣除 RSI/MACD/EMA/布林/量比/近期涨跌后的偏 IC 同号 |t|≥2）、G2 留出集（2026-07 起同向）、G3 五分位多空扣 0.16% 往返成本仍为正。',
    bullets: [
      'G0 通过：标签置换 100 次 0 次误报（上限 10），合成随机游走面板 12 格全不通过。',
      '原始方向是短期均值回归：价格在 VWAP 之上、主动买入偏多的币随后 4–24h 跑输，dev 与 holdout 都显著（vwap24_dist/h4 IC −0.033 t −10.1 → −0.023 t −5.2）。',
      '但信息已被已有指标覆盖：vwap24_dist 与 (close−EMA20)/ATR 秩相关 0.93、与 RSI14 0.89；扣除后偏 IC 反号或≈0，G1 全部不通过。主动买卖失衡与已有指标相关仅 0.1–0.4，但偏 IC 在 holdout 不显著且方向反转。',
      '经济性：|IC|≈0.02–0.03，4h 多空毛收益每期 0.02%–0.05%，远低于 0.16% 往返成本；vwap24_dist/h24 净 +0.12%/期但 P(均值>0)=0.67<0.90。',
    ],
    conclusion:
      '0/12 通过，不加入提示词、不开 A/B。线索（未预注册，不作结论）：vwap24_dist 控制已有指标后偏 IC 在 dev/holdout 均为正（t 2.8/2.6），若要验证需新预注册并计入 K。',
    source:
      'scripts/backtest/orderflow_vwap_study.py · docs/research/2026-11-01-orderflow-vwap-prereg.md · docs/research/2026-11-01-orderflow-vwap-study.md',
  },
  {
    id: 'factor-quant-paper',
    date: '2026-11-01',
    title: '因子量化交易员（程序化模拟盘，不经过大模型）',
    domain: '加密 + 美股 · 因子挖掘第二路输出',
    verdict: 'running',
    verdictText: '模拟盘前向检验中',
    idea: '因子挖掘的输出分成两路：一路照旧作为 factor_ensemble 提示词参考；另一路由每晚挖掘生成 quant_targets.json 目标权重，新策略类型 factor_quant 的交易员按调仓周期在模拟账户自动调仓，不调用大模型，代码层只允许模拟账户。加密规则 ls_invvol_protected_v1：h=3 ensemble 做多前 20%、做空后 20%，1/σ 加权，空头排除近 7 日涨幅前 10% 与上市不足 90 天的币，单币上限 10%，空头 50% 灾难止损，每 3 天调仓；美股 long_invvol_v1 只做多，目前没有通过验证的因子，保持空仓等待。',
    bullets: [
      '描述性回测（Backtrader，5bp+3bp 与真实资金费，当前 h=3 成分历史重放）：val 2025-05～2026-04 保护规则 +27.1%（Sharpe 0.94，回撤 16.5%），朴素等权 −1.6%；forward 2026-05～10 保护规则 −12.7%（Sharpe −0.65，回撤 29.3%），朴素等权 −19.2%。val 有选择偏差，forward 是当前成分的历史重放，模拟盘才是真正的前向检验。',
      '已知风险来自 ensemble 前向研究：因子排最后的一档会混入暴涨币，空头保护只能减轻、不能消除；空头灾难止损未计入回测。',
    ],
    conclusion:
      '运行中：两个模拟交易员（加密、美股）上线后按模拟盘结果判断；不接入实盘。',
    source:
      'scripts/factor_mining/quant_targets.py · trader/auto_trader_factor_quant*.go · store/strategy_factor_quant.go · docs/architecture/FACTOR_QUANT_TRADER_DESIGN_2026-11-01.md',
  },
  {
    id: 'ensemble-forward',
    date: '2026-11-01',
    title: '加密因子 ensemble 前向校验（市场中性组合可行性）',
    domain: '加密 · 因子挖掘',
    verdict: 'none',
    verdictText: '前向未通过',
    idea: '因子分数目前只作为 factor_ensemble 写进 AI 提示词当参考：预测目标是截面相对收益，而交易员做的是少数币的方向性交易，信号宽度被浪费，A/B 模拟盘也测不出差异（10 个配对日，日差 −0.45%±1.09%）。替代方案是把 ensemble 做成独立的市场中性组合直接交易。前提是 ensemble 样本外有效，因此冻结当前成分（h=1/3/7 分别为 7/14/14 个因子），只读 2026-05-04 之后约 180 天从未参与选择的数据做预声明检验。',
    bullets: [
      'A1 信号层：前向 RankIC 均为正且显著，h=1/3/7 分别为 0.065（t 4.02）、0.070（t 2.86）、0.075（t 2.42），前后两半同号，IC 水平与 train/val 相当。',
      'A2 五分位多空扣手续费和资金费：每期均值年化 −129%/−85%/−196%，全部未通过；按预声明，B 部分（Backtrader 市场中性组合、6 道门槛）没有运行。',
      '诊断：两腿中位数之差为正（h=3 +1.26%/期，h=7 +1.61%/期），排序方向对；但因子排最后的一档混入暴涨币（AKEUSDT 一周 +196%/+264%，QUSDT +343%，BRUSDT +108%），h=7 五分位均值从 Q1 到 Q5 为 4.1%、2.3%、1.3%、1.4%、0.5%，单调递减。秩相关不受右尾影响，等权做空却正好吃下右尾。',
    ],
    conclusion:
      '不做独立组合，维持仅作参考。风险提示：factor_ensemble 低 pct（因子看空）集中了暴涨币，据此做空等于承担右尾，提示词是否追加说明由用户决定。后续方向（只做多腿、空头剔除暴涨/新币或按波动率缩仓、挖掘目标改为截尾收益）需要新的预声明实验，并计入 K。',
    source:
      'scripts/factor_mining/ensemble_forward_study.py · docs/research/2026-11-01-ensemble-forward-study.md',
  },
  {
    id: 'alpha101',
    date: '2026-11-01',
    title: 'WorldQuant Alpha101 因子（BigQuant 复现文章）',
    domain: '加密 + 美股 · 截面因子',
    verdict: 'none',
    verdictText: '无可融合因子',
    idea: '用户给出 BigQuant 三篇文章（Alpha101 复现与解析、附录三 SQL、附录四 Alpha#100 因子分析），问能否融合进本系统。系统已有 qlib 风格因子挖掘（Alpha158 种子 + 每晚变异/LLM 搜索 + 前向校验，分数仅作 factor_ensemble 提示参考），因此融合路径是把 Alpha101 作为种子进入 zoo，门槛与 zoo 完全一致：冻结评估器、K=现有 ledger 试验数+本批（t_req≈4.6）、val 达标、与已接受因子相关<0.8、加密扣资金费后仍达标。',
    bullets: [
      '按论文原式手工翻译 100 个公式（#56 缺市值跳过；小数窗口 floor、scale 保留符号、行业中性化无行业数据改为截面去均值），测试覆盖算子手算、逐个因果性、原文逐字一致；附录三 SQL 由用户登录后只读核对，发现 scale 丢符号、#54/#60/#29/#64 笔误、#96 残留调试过滤，不能当标准答案。',
      '加密（60 币池，h=1/3/7）：截面 IC 存在，h=1 有 9 个过多重检验门槛（#5/#38/#33/#49/#44/#51/#35/#84/#90，IC 0.02–0.04），但都是高换手短周期因子（每次调仓约 40% 换手、单边 0.08%），五分位多空扣费后 train −18%～−70%/年，全部停在 candidate；最接近的 #20 在 h=7 val t=5.3，但 train t 4.31 < 4.59。',
      '美股（100 只 bStock 正股，h=5/10/20）：train t 最高 3.74（#36），val 全不显著；Alpha101 原设计持有期 0.6–6.4 天，本系统美股持有期≥5 天，且 vwap 只是 (H+L+C)/3 近似。',
      '与现有因子重叠：#33=rank(open/close) 与 Alpha158 的 OPEN0 相关 1.000，#38/OPEN0 0.88，#54/KSFT2 0.94，#40/CORR10 0.82；有效部分大多已被 zoo 覆盖。',
    ],
    conclusion:
      '0 个可融合（accepted 0、redundant 0），不改挖掘流程与交易代码。若要再试，需先降低换手（decay 平滑或延长调仓）作为新的预声明实验，并计入 K。',
    source:
      'scripts/factor_mining/alpha101.py · scripts/factor_mining/alpha101_study.py · docs/research/2026-11-01-alpha101-study.md · data/alpha101/',
  },
  {
    id: 'probability-martingale',
    date: '2026-11-01',
    title: '数学家视角：鞅、赌徒破产与 Kelly 检验',
    domain: '加密 + 美股 ETF · 概率论',
    verdict: 'none',
    verdictText: '无可融合策略',
    idea: '用户给出 CMU 教材 bws_book.pdf，要求从数学家角度、基于概率论尝试交易。核对后该书是证明写作入门（逻辑、归纳、集合、函数、模算术、组合计数），不讲期望、方差、随机游走、鞅或 Kelly；因此借用书中“精确命题 + 找反例 + 鸽笼式多重检验”的方法，概率工具来自标准概率论：可选停止定理说明价格为鞅时任何止盈止损/加减仓都无正期望，优势只能来自对鞅的偏离。',
    bullets: [
      'P1 方差比（Lo–MacKinlay 稳健 z*，222 个检验，Holm 校正，前 60%/后 40% 同号才算稳定）：显著且稳定 0 个；BTC/ETH 日线与小时线 VR 0.93–1.16 均不显著，美股日线仅有轻微均值回归倾向。',
      'P2 双障碍（先碰止盈 2s 还是止损 1s 等 6 组，528 个检验）：命中率只有 EFA 多头 (2,1) 显著（0.402 对鞅 0.333），但 10 只 ETF 普遍偏高、约一半是权益漂移，另有向下跳空造成的偏差；按实际跳空成交价的期望检验 z=0.89。预声明 Backtrader 策略 BARRIER_LONG FAIL（后段 Sharpe 0.47 对同敞口持有 0.55，DSR 0.13，平台 3/15，ETF 1/10）。',
      '期望检验在加密篮子上找到 BNB/DOGE/XLM 带止损空头的 6 个稳定偏离；预声明 BARRIER_SHORT（32 币等权）FAIL：年化 −16%、回撤 85%、两半 −59%/−31%。原因是空头凸性：跌 2s 只赚 1−e^(−2s)、涨 1s 亏 e^(s)−1，按对数价格取反计量把 σ²/2 的方差拖累算成了优势（DOGE 约 0.07s，与观测 0.08s 吻合）。',
      'P3 Kelly：前段估的 f* 95% 区间大多跨 0（BTC −0.26～2.64），TLT 估出 4.5 倍、后段回撤 98%；ETH 半 Kelly 增长高于全 Kelly。公式没错，错在 μ 估不准，应使用半 Kelly 或更小。',
      'P4 nofx 实盘（ai_managed=1，293 笔可算 R，R 已扣手续费）：胜率 48.1%、赔率 0.78、E[R]=−0.077R（95% 区间 −0.147～−0.007，P(E[R]>0)=1.6%），6 个 trader 全部为负，Kelly f*=−0.18：最优仓位为 0，问题在入场优势而非仓位。',
    ],
    conclusion:
      '没有可融合的策略，不改交易代码。留下四条检查清单：止盈止损比例本身不创造期望；评估止盈止损规则看实际成交价的期望而不是命中率；高波动币做空每天承担约 σ²/2 的凸性拖累；先证明 E[R]>0 再谈 Kelly，且只用半 Kelly 或更小。',
    source:
      'scripts/backtest/probability_study.py · scripts/backtest/barrier_strategy.py · scripts/backtest/live_kelly.py · docs/research/2026-11-01-probability-martingale-study.md · data/math_book/',
  },
  {
    id: 'quantjourney-bt',
    date: '2026-10-31',
    title: 'QuantJourney 开源回测库 50 个示例策略移植验证',
    domain: '美股 ETF + 加密 · 外部策略',
    verdict: 'none',
    verdictText: '无可融合策略（79 个组合全部未通过）',
    idea: '用户给出 GitHub 仓库 QuantJourneyOrg/quantjourney-bt（Apache-2.0，宣传为“50 个 Python 交易策略免费公开”：动量、突破、配对、风险平价、RSI 回归、走前优化）和一篇 X 文章《How to Become a Quant》，要求全部回测验证能否融合进 nofx。仓库实际是回测引擎的功能示例集：25 个权重型（演示再平衡/风控叠加）、20 个订单型（演示限价/止损/跟踪止损/括号单/OCO）、5 个走前优化流程，README 自述为教学用例、不构成投资建议。',
    bullets: [
      '官方自报结果（45 份公开 performance_report，含月度收益与成交明细）与同期买入持有比较（同 rf=2%、同标的同区间）：可比的 35 个中 29 个 Sharpe 低于买入持有；订单型 O01–O14 为 −0.43~0.21（SPY 0.42）；只有月度动量轮动 O17（0.53 对 0.51）、波动率目标动量 W18（0.57 对 0.51）及两个 0.45 对 0.45 的打平跑赢。日内样本只有约一个月，FX/期货无可比基准。',
      '数据核对（数据保留在 data/quantjourney/，不删除）：Yahoo 美股 ETF/个股 23 只（2000-01 至 2026-10-30）交易日历与 SPY 完全一致，近 10 年与纳斯达克官方历史逐日比对收盘价偏差 >0.5% 仅 1 天（纳斯达克的占位行），与 QJ 自己的成交价比值逐标的为常数（仅差其后的分红复权）；加密 10 币日线与币安 fapi 全历史逐日 OHLC 完全一致，资金费抽样逐结算一致，BTC/ETH 1h、5m 无缺口。',
      '复现（证明移植正确）：订单型 16/16 合格（月度收益相关 0.999–1.000，入场日期一致率 98.9%–100%）；权重型 18/19 合格，W19 风险平价不合格是 QJ 原 ERC 迭代数值不稳定（协方差差一个浮点 ULP 单资产权重变 78 个百分点），未为追官方数字改算法。',
      '评估：t 收盘决策 t+1 开盘成交；美股佣金 1bp+滑点 2bp+借券 0.5%/年+融资 5%/年（门槛样本截至 2025-12-31，2026 单列）；加密 5bp+3bp+真实资金费。门槛：G1 两窗 Sharpe 都胜同敞口基准、G2 Deflated Sharpe ≥0.95、G3 两半都赚钱、G4 参数平台、G5 加密 10 币横截面 ≥60%；按文章“谁在付钱”标注收益来源。',
      '审查时三处更正（已披露）：G2 原作用于超额收益导致全部为 0，改为策略自身收益 DSR（事后放宽）；日内规则 SR≈−10 被放进 DSR 试验池把门槛抬到年化约 3.6，改为只用日线组合；对数收益方差拖累让低敞口策略机械胜过满仓基准，G1 改为同敞口基准（收紧），原先通过 G1 的 14 个组合全部变为不通过。',
      '结果：没有择时能力超过同仓位持有（如 W22 MACD 在 BTC 上两窗 0.78/0.44 对同敞口持有 0.80/0.64）。唯一过 G1/G3/G4/G5 的是 O07 止损限价突破，但 DSR 仅 0.09（美股）/0.31（加密）；权重型 DSR 最高 W05 逆波动率 0.35、W06 0.29、W17 0.29。配对、截面动量/反转为负或接近零，日内规则每笔净 −15~−17 bps，短期均值回归在加密上为负。',
    ],
    conclusion:
      '这些都是公开多年的教科书规则，按真实成本与同敞口基准比较后没有一个能证明有超额，与文章“无付钱方即噪声、多重检验出假赢家、发表后衰减”的观点一致；不改任何交易代码。移植模块与报告保留，供以后复查。',
    source:
      'scripts/backtest/quantjourney.py · scripts/backtest/quantjourney_orders.py · scripts/backtest/market_cache.py · docs/research/2026-10-31-quantjourney-strategies.md · data/quantjourney/',
  },
  {
    id: 'volrix-strategies',
    date: '2026-10-31',
    title: 'Volrix.ai 公开策略库（83 个）移植回测',
    domain: '加密 · 外部策略',
    verdict: 'none',
    verdictText: '无可融合策略',
    idea: '用户要求把 app.volrix.ai 策略库里所有公开策略抓下来，本地回测验证能否融合进 nofx。',
    bullets: [
      '抓取：通过公开 API（无需登录）拿到全部 83 个策略的规则描述、指标与成交明细；策略代码需登录（未注册），只用公开文字规则与成交。62 个为印度期权（NIFTY/SENSEX/MCX 卖方、跨式等），需印度期权链数据且 nofx 不交易期权，不可移植；22 条价格规则移植到币安永续。',
      '复现：三个原生币安策略与 Volrix 成交对齐——BTC 20 日新高入场日 100% 一致（119.4% 对 133.2%）、ETH EWMAC 波动率目标入场日 97.3%（228.6% 对 223.9%）、BTC VWAP reclaim 入场时间 ±5 分钟 98.4%。Volrix 的加密回测不计手续费与资金费，Sharpe 算法未公开、不可比。',
      '口径：佣金 0.05%+滑点 0.03%+真实资金费，按当前权益复利定仓，基准为现货等价买入持有。教训：BTC 永续从 2019-09 满仓长持累计资金费约 3,553 美元（本金 1,000），2022-11 权益一度为负，长持基准必须用现货等价。',
      '结果：16 条日线/小时规则只有 SUPERTREND_D 在 ETH 上机械通过（全样本 Sharpe 0.91 对持有 0.90、回撤 48% 对 79%，但后半段 0.25 对 0.44、60/40 留出段 0.01 对 −0.03），22 条规则×2 币的多重比较下与随机相符；BTC 上慢速趋势规则 Sharpe 0.53–0.59 低于持有 0.79，本质是降回撤也降收益。6 条日内止损止盈规则每笔净 −8~−18 bps，扣成本前也接近 0。',
      '事后线索（未预声明）：MAX20、HIGH52 放到 49 币等权篮子（2023-08..2026-10）Sharpe 1.48/0.91、回撤约 14%，等权持有 0.76/73%；单币中位 Sharpe 仅 0.34/0.12，窗口短且有幸存者偏差，用户 10-31 决定暂缓。AI 历史信号按日线趋势分组：样本期 BTC 日线一直向上，逆势组每组仅 4–16 笔，无法判断。',
    ],
    conclusion:
      '没有可以直接融合进加密交易系统的 Volrix 策略；不改任何交易代码。若跟进篮子线索，应作为截面因子交给 scripts/factor_mining 冻结评估，而不是直接上线。',
    source:
      'scripts/backtest/volrix.py · scripts/backtest/volrix_intraday.py · docs/research/2026-10-31-volrix-strategies.md · data/volrix/',
  },
  {
    id: 'factor-mining-sensitivity',
    date: '2026-10-28',
    title: '因子挖掘已准入因子的币池（幸存者偏差）/ 资金费敏感性',
    domain: '加密 · 因子挖掘',
    verdict: 'partial',
    verdictText: '多数不稳：子池 + 资金费下仅 11/41 仍满足准入',
    idea: '因子挖掘的 100 币池来自当前存活快照，且多空收益未计资金费。检查 zoo 中 41 个 accepted 因子×持有期在两种口径下是否仍满足准入：只用 2022-01-01 前已有数据的 32 币子池（减轻幸存者偏差）；多空腿计入真实资金费。',
    bullets: [
      '方法：全部 IC、HAC t、分层收益调用冻结的 evaluate.py（SHA256 记录在报告），不读 holdout 标签、不重搜表达式、不翻转方向；子池重新计算截面算子与成交额排名；资金费按每日 3 次结算估算、以每腿入场名义计费，缺失记 NA 不补零（实际缺失 0/606,928）。',
      '结果：只换子池仍满足 12/41，只加资金费 22/41，两者同时 11/41。三种口径都稳的有 7 个（如 f_709d7407、f_8dd9bbca、f_a601391e、f_ac795953 的 7 日期，f_86c39d5d、f_c0b3235b 的 1 日期，f_cba0f8c6 的 3 日期）；不少因子在子池的验证段多空收益由正转为 −30%~−77%。',
      '局限：32 币子池仍来自 10-18 存活快照，无法消除退市合约缺失造成的偏差，完整修复需 data.binance.vision 历史合约数据。',
    ],
    conclusion:
      '已准入因子中相当一部分依赖幸存者偏差或忽略资金费，实盘参考需谨慎；本报告只作为是否另开评估器实验的证据，未修改冻结评估器、zoo 或 ledger。',
    source:
      'scripts/factor_mining/sensitivity_20261028.py · docs/research/factor-mining-sensitivity-20261028.md（提交 142b19ff）',
  },
  {
    id: 'us-factor-mining-v2',
    date: '2026-10-27',
    title: '美股因子挖掘 v2（us2：S&P 500 ∪ bstock 544 只）',
    domain: '美股 · 因子挖掘',
    verdict: 'none',
    verdictText: 'G0 零效应校准未通过，中止，无因子',
    idea: '现行美股挖掘（96 只 bstock 池）9,031 次试验只得 2 个 accepted（20 日成交量族），前向 105 天均失败。v2 先预注册，把面板扩到 S&P 500 ∪ bstock 正股 544 只（2020-10-27..2026-10-26，逐日成交额前 300、min_history=60），加入 SPY 日收益与 VIX 作为市场字段，并要求先通过时间错位 null 校准（G0）才可运行真实挖掘。',
    bullets: [
      '数据审计：547 个文件逐个 SHA256 核对一致；13 只因历史过短在下载阶段排除；幸存者偏差按预注册披露。',
      'G0：3 个种子 × h=1/3/5 × 每格 5,000 次试验（t_req=4.42），在时间错位的零效应面板上仍有 200–566 个因子被判 accepted、179–890 个 candidate，全部 9 格失败（应接近 0）。',
      '按预注册第 4 节停止：未运行真实挖掘、未打开 holdout、未调整门槛或种子；耗时 G0 约 183 分钟。',
    ],
    conclusion:
      '该评估设计在美股截面上无法控制误报，不能据此提出任何因子；“无证据”不等于证明效应为零。代码与报告在未合入 dev 的分支 claude/wyckoff-trading-review-7954ae（提交 2d604427）。',
    source:
      'scripts/factor_mining/us2_study.py · docs/research/2026-10-27-us-factor-mining-v2-study.md · docs/research/us-factor-mining-v2-summary/（分支 claude/wyckoff-trading-review-7954ae）',
  },
  {
    id: 'wyckoff-vp',
    date: '2026-10-26',
    title: '威科夫 / 成交量分布（VP）事件信号层研究',
    domain: '加密 · 价格行为 / 成交量',
    verdict: 'none',
    verdictText: 'null 校准未通过，中止，不接入',
    idea: '检验威科夫/VP 事件（日线成交量分布、UT/Spring、努力与结果背离等 E1/E2h/E2l/B0/E3 五类事件 × 方向 × 24/72 根，共 45 格）能否作为加密 1h 信号层证据，先预注册再看结果。',
    bullets: [
      'v1：对照规则自相矛盾（错位 ±[6,48] 又要求不落在任一事件 ±72 根内），可用对照恒为 0，45 格配对 n=0，无任何推断。',
      'v2：对照改为排除同类事件 ±6 根；随机游走 null 的 3 个面板各有 11、11、12/45 格 BH 通过——向更早错位的对照覆盖了事件的铺垫行情（对照 Y1 被压低约 50–300 bps），零效应下也显著。v2 真实结果已被看到，一律作废。',
      'v3：对照只取事件之后 +[7,48] 根，新增前置门槛 G0（每个 null 面板 BH 通过 ≤1）。51 币×8,700 根随机游走，种子 100 有 2 格通过，G0 未通过；植入 80 bps 已知效应时 44–45 格可检出（检出力足够）。',
    ],
    conclusion:
      '真实数据从未得到裁决；用户 10-26 决定关闭，不再改规格，不并入系统。若重开需新的 v4 预注册，先修正 null 面板 E3 事件数不匹配（null 约 6,700 对真实 128）。代码与报告在未合入 dev 的分支 claude/wyckoff-trading-review-7954ae（提交 4d1a2e1e）。',
    source:
      'scripts/backtest/wyckoff_study.py · docs/research/2026-10-25-wyckoff-vp-study.md · 预注册 v1–v3 · docs/research/wyckoff-summary/（分支 claude/wyckoff-trading-review-7954ae）',
  },
  {
    id: 'threepush-wedge',
    date: '2026-10-24',
    title: '三推 / 楔形反转按斜率分类（陡峭·中等·低斜率·近水平）信号层研究',
    domain: '加密 + 美股 · 价格行为',
    verdict: 'none',
    verdictText: '无证据支持，不接入',
    idea: '用户提供一张“不同斜率的三推/楔形反转”价格行为图与文字：陡峭三推第三推后常继续延伸、不宜逆势；中等斜率三推反转机会较好；低斜率三推更像区间、盈亏比差；近水平为三重顶/底；强调位置+力量+空间，三推是背景而非入场信号。系统里没有真正的三推识别（cycle.go 的 wedge_pop/drop 只是 EMA 事件），Brooks 研究也没测过。',
    bullets: [
      '因果识别器：分形摆动点右侧 k 根确认后才可见，检出时点=第三推确认收盘；三推不重叠复用；斜率按 ATR/根数归一，训练期三分位冻结分 steep/medium/low，近水平单列。对照：同币同方向、±30 日、过去同幅度同向五分位匹配的 5 个随机时点（回答“三推是否超出‘刚涨过一段’的信息”）。数据：加密 1h（100 币约 2~3 年）、加密日线、美股日线（泛化）。',
      'v1 校准失败：标准 10 小种子零通过，但大面板随机游走出现 605 个训练通过/436 个保留确认。复核定位两个设计偏差：对照抽样把检出时点之前的 k 根也排除（对未来的条件化）；Y2 首次穿越 R 期望/rr_space/延续幅度由形态几何机械决定，类别间天然不同。v1 的“有效”全部被否决（真实数据本身也是 0 通过）。',
      'v2 修正（预注册先于重跑，并披露 v1 结果已被看过）：对照只排除 [t,t+k]；推断仅保留几何中性的 Y1（形态−匹配对照反转收益，成本后）及类别差、H5 要素增量；Y2/rr_space/Y3 降为描述性统计。校准：10 个种子、每个 1h 50×15,000 + 日线 100×2,500（按真实逐币波动缩放）训练通过全为 0；植入 medium 反转效应训练通过 504、保留确认 474。本人换种子 777/778 独立复跑：每个约 2.5 万形态，0 通过/0 确认；植入版 1702/1450。',
      '真实 v2：检验 21,384 项（可估计 6,630），训练 FDR 通过 0，保留确认 0，默认参数“有效”清单为空。形态数（训练/保留）：1h steep 2286/4314、medium 2285/4572、low 2286/4920、flat 134/260；日线 steep 163/431、medium 163/345、low 164/314、flat 11/17。形态−对照的 Y1 点估计在训练与保留窗符号常相反（如 1h medium 中期 −26.0/+15.6 bps；日线 medium 短期 +179/−151 bps），没有稳定的类别差。',
      '三个原问题：陡峭三推逆势是否更差——不支持（无证据表明 steep 比 medium 差）；中等斜率是否最好——不支持；低斜率反转空间是否有限——不可推断（空间/盈亏比由几何决定，不是预测证据）。日线与 flat 类样本偏少，“不支持”不等于“原文错误”。',
      '最脆弱：v1 保留窗已被看过，v2 并非全新独立验证；日线/flat 匹配样本少；1h 仅 2~3 年且大半处于同一大行情；检出器参数（k、回调下限、分位切点）的选择自由度虽有邻域检验，仍是一族。',
    ],
    conclusion:
      '没有证据支持把“三推按斜率分类”做成可交易规则或参考字段；不改任何提示词与风控。三推更适合作为人工阅读图形时的背景判断，而非系统信号。研究脚本与预注册、校准框架保留（scripts/backtest/threepush_study.py）。',
    source:
      'scripts/backtest/threepush_study.py · docs/research/threepush-study.md · docs/research/threepush-preregistration.md · docs/research/threepush-summary/',
  },
  {
    id: 'regime-statistics',
    date: '2026-10-24',
    title: '市场形态统计特征（15 种形态）信号层研究',
    domain: '加密 + 美股 · 市场状态',
    verdict: 'partial',
    verdictText:
      '部分有效：仅波动/回撤风险类特征有效，收益择时与 exit_mode 无证据',
    idea: '一张“15 种市场形态”图（趋势/均值回归/随机游走/暴涨/波动聚集/挤压/厚尾/跳跃/闪崩/区间/V 反转/状态切换…）能否转成可检验的统计特征，补充现有仅 EMA+ADX×BB/ATR 的 market_regime。先预注册标准再看结果，不接入任何交易逻辑。',
    bullets: [
      '数据：加密 1h（100 币，2023-10 起约 3 年）、加密日线（2019 起）、美股日线（96 只，仅泛化）。特征 28 个（效率比、方差比、自相关、Hurst-lite、凸性、波动比、波动的波动、峰度、跳跃、影线、区间穿越、回撤反弹…）× 5 个窗口邻域 × 3 个预测期 × 目标：T1 顺势/逆势收益差（扣成本）、T2 未来波动（HAR 基线控制）、T3 尾部/回撤概率（当前波动 logit 基线）、T4 状态可用性。每个周期 2,955 项检验，两个加密周期合并 5,910 项做一次 BH FDR=10%。',
      '统计纪律：预注册文档在真实数据检验前写入（v1→v3 的修订只依据合成数据负对照，未看真实目标）；60/40 时间切分并 purge；日期聚类 + HAC 取较大 SE；候选清单在保留窗打开前冻结并存哈希；保留窗只评一次；14 个随机游走面板的负对照训练拒绝为 0；植入的高 ER 动量效应可被检出；完整性检查全部通过。',
      '结果：训练 FDR 通过 708 项，保留窗同向确认 594 项（含所有五点窗口，不是独立发现数）；默认窗口且同时满足平台与经济门槛的“有效”只有 4 个特征、7 个组合：日线 ewma_ratio / volvol / tail_frequency → 未来 3/7 日波动（HAR 控制后增量 R² 0.55~1.65 个百分点，美股同向）；last_jump（单根 |r|/σ>4）→ 未来回撤风险（1h h=4：保留窗 9.9% 对波动基线 2.7%，3.7×；日线 h=7 为 3.4~3.9×，美股仅同向 t≈1.1）。',
      '无效：T1（顺势/逆势收益择时）全部在保留窗未确认；T4（用 ER/VR 状态选 trend/range/quick）训练窗无候选；ER、VR、ρ1、Hurst、凸性、加速度、stretch、squeeze、BB 分位、峰度、区间穿越、回撤/反弹等均无效。盘口深度/买卖价反弹在 1h 与日线频率不可检验。',
      '独立复算（本人自己的实现，不用研究脚本）：1h last_jump → 未来 4 小时回撤>3×ATR，波动匹配基线下训练 1.87×、保留 3.67×——方向与量级复现，但训练明显弱于保留（训练 q=0.075 勉强通过），且“跳跃”不分方向（向上暴涨后的回落也算）。',
      '最脆弱：volvol 增量 R² 仅比门槛高 0.047pp；跳跃样本稀少（保留窗 1h 约 7,000 条，日线仅数百）且集中在共同冲击日，不是独立事件；美股回撤只同向；状态持续性有机械持续性；加密与美股共享全球风险周期。',
    ],
    conclusion:
      '可考虑的落地范围很窄：把“近期出现跳跃”作为短期回撤风险的参考证据、把日线波动特征作为未来波动的参考估计，先做影子记录。本批不支持任何开仓门控、收益择时，也不支持用状态自动选 exit_mode；未改 market_regime 或其它交易代码。“无效”表示未达预注册标准，不等于证明为零。',
    source:
      'scripts/backtest/regime_study.py · docs/research/regime-statistics-study.md · docs/research/regime-statistics-preregistration.md · docs/research/regime-study-summary/',
  },
  {
    id: 'us-candidate-filter',
    date: '2026-10-23',
    title: '美股扩大候选数 / 趋势过滤能否改善（候选池质量检验）',
    domain: '美股 · 候选筛选',
    verdict: 'none',
    verdictText: '无证据支持，维持现状',
    idea: 'us-stock 交易员近一周无新开仓：扫描器按 12-1 动量取前 5 名，而领涨股集体回撤，前 5 名几乎都在下降趋势，AI 按规则全部 wait。检验：扩大候选数（前 10/15），或先剔除下降趋势标的再取前 N，是否改善结果。AI 逐笔决策无法回测，只验证候选池质量。',
    bullets: [
      '数据：95 个美股正股/ETF（SPY 外）日线，有效回测 2021-10 ~ 2026-10；宇宙是“当前已上线 bStock”，存活者/选择偏差严重（绝对 CAGR 被高估，如基线 79.8%），只有变体间的相对比较有意义，且该偏差也可能影响相对比较。',
      '信号层（不含订单，按日期聚类 + 整日自助法）：动量前 15 名按趋势分类，下行（close<EMA20<EMA50）vs 上升（close>EMA20>EMA50）的前瞻收益，样本外 10 日 2.86% vs 3.15%（差 −0.29pp，区间 [−2.19, 1.61]），大盘回撤期 10 日 4.35% vs 3.14%（差 +1.21pp，区间 [−2.60, 4.87]）；训练/验证期下行组 10 日反而比上升组高 2.2~2.7pp，样本外未确认。即没有证据表明“下降趋势的动量标的更差”。',
      'Backtrader 机械组合（MOM_TOP，每 10 个交易日调仓、等权、佣金 0.1%/边，预注册 7 个变体）：V0 前 5 无过滤 Sharpe 1.29 / CAGR 79.8% / MaxDD 53.0%；扩大到 10/15 名 Sharpe 1.15/1.17、CAGR 56.1%/53.3%；T1(close>EMA50) 过滤前 5/前 10 Sharpe 0.91/0.96；T2(close>EMA20>EMA50) 0.75/0.80。所有变体相对 V0 的日收益差为负（全期 −7.6 ~ −17.1 bp/日），大盘回撤期差异区间全部低于 0（−47 ~ −75 bp/日）。波动与回撤略降，但收益与 Sharpe 都更差。',
      '训练+验证按预定规则选中 T1，但它只是两个定义里落后较少的；样本外仍落后。因此不支持“回撤期加趋势过滤会改善”。',
    ],
    conclusion:
      '维持现状：不扩大候选数，不加趋势过滤。近期无开仓是规则（downtrend / wedge_drop 禁开）与行情（领涨股集体回撤）的共同结果，符合设计，不是故障。注意：同一份数据显示下降趋势的动量标的前瞻收益并不更差（样本内更高），但因宇宙存活者偏差（今天上线的 bStock 本身就是赢家集合，回调后“反弹”有构造性偏差），不能据此放宽“downtrend 绝不开仓”的规则。局限：6 年单一行情、日期聚类对重叠标签区间偏窄、未做多重检验校正、EMA 分类不等同线上完整结构判断。',
    source:
      'scripts/backtest/us_candidate_study.py · scripts/backtest/stocks_cache.py · scripts/backtest stocks（MOM_TOP 增加候选过滤参数）',
  },
  {
    id: 'brooks-price-action',
    date: '2026-10-23',
    title:
      'Al Brooks 价格行为体系（always-in / H2·L2 / 高潮 / 均线缺口）信号层检验',
    domain: '加密 + 美股 · 价格行为',
    verdict: 'none',
    verdictText: '无证据支持，未进入订单回测',
    idea: '用户提供从《高级趋势技术分析》提取的量化规则（trend_trade/）。先验证核心断言在我们数据上是否成立：always-in 状态是否真有条件区分力、“80% 反转/突破失败”、强趋势分层、H2/L2 顺势回调加过滤器、高潮与 20 根均线缺口后的调整、禁做清单的价值。',
    bullets: [
      '特征库（Codex）：183 个因果向量化特征 + 与提取代码 brooks_pa 的 46 项规则差异审计（H/L 计数未保留尝试间隔与重置；参考强趋势评分最高仅 20 分却以 30 分为门槛；二次入场比较符号、下影线公式、量度目标单位有误）。强趋势表 8 项因缺会话开盘/明确分段定义等无法程序化。',
      '数据：加密日线（约 100 币，2019 起）、加密 1h（7 个满 2 年的主流币，2022 起）、美股日线（SPY 外 95 个文件，含 ETF）；训练 ~2023、验证 2024、样本外 2025-01 起只看一次。信号层标签（前瞻收益，不含止损与盈亏），日期聚类稳健 SE + 2000 次整日自助法 + 500 次时间错位随机对照；预注册 7 组检验，共 3168 项，BH 校正后 123 项显著（其中样本外 37 项，含反方向结果）。',
      '“80% 失败”公理被否定：区间向上突破后收回区间的比例样本外为 70.5%（币日）、70.2%（币 1h，相对 80% q=5.8e-10）、57.4%（美日，q=2.8e-10）；强趋势里的逆势反转分支样本为 0，无法检验。50/50 基线也不成立：样本外币日线 43.7% 上涨（bearish 阶段）。',
      'always-in 条件化：主周期 H2/L2 顺势减逆势在三个市场均不显著（q≥0.47）；仅美股“反转最低要求”在 10/20 根上顺势比不确定高 +118/+160 bps（q=.005/.014），但训练与验证阶段没有提名、单一市场，属探索性发现。H2/L2 加顺势过滤、再加反转K线质量过滤，增量均不显著（q≥.27）。禁做清单三条（十字星、过度重叠、微型通道逆势）样本外无一通过校正。',
      '均线缺口：首次触线后回到 EMA20 附近的比例为 89.0%（币 1h）/ 83.7%（美日），比基线高约 10~12 个百分点（q<2e-4），只是“回归均线”的统计规律，并非可交易边际；高潮停止后回均线的概率反而低于基线（69.0% / 63.7%），与书中“高潮后调整”的说法相反。',
      '按事先冻结的晋级规则（训练与验证共同提名才进入订单回测）：没有任何规则被提名。原书语境是 Emini 5 分钟图，规则在日线与 1h 上的迁移本身可能就不成立；没有测 5 分钟级别。',
    ],
    conclusion:
      '没有可落地的交易规则，不进入 Backtrader 订单回测，不改任何提示词或风控。特征库与检验脚本保留为研究工具（scripts/backtest/brooks*）。若要继续：先补充 5 分钟数据与会话/开盘类特征再测日内剧本；否则不建议在这套规则上继续投入。“证据不足”不等于证明效应为零，样本含存活者偏差与收益重叠，周聚类可能低估不确定性。',
    source:
      'scripts/backtest/brooks/ · scripts/backtest/brooks_study.py · scripts/backtest/brooks_fetch.py · scripts/backtest/brooks/DISCREPANCIES.md',
  },
  {
    id: 'factor-ensemble-vs-trades',
    date: '2026-10-22',
    title: '因子集成分数能否区分真实交易的好坏（206 笔）',
    domain: '加密 · 因子 × 实盘交易',
    verdict: 'none',
    verdictText: '无证据支持提高因子比重',
    idea: '因子集成在验证集上 IC 为正（0.086/0.118/0.148，t≈5.6~5.9），但扣费后多空收益不稳定（1 日 −13%、3 日 +70%、7 日 −3.6%），前向校验 34 个入选因子里 31 个未定论、3 个失败、0 个确认。检验这个分数对实际成交的入场点有没有区分力：若有，才谈提高它在大模型开平仓里的比重。',
    bullets: [
      '方法：206 笔已平仓、币在因子宇宙内的交易（2026-08-29~10-21，其余约 248 笔为代币化股票/小币种不在宇宙内）；取入场前一个已收盘日的集成百分位，按多空方向对齐（越高 = 因子越同意该方向），与该笔实际收益做秩相关。',
      '结果（对齐分数 与 交易收益 的秩相关，95% 自助法区间）：1 日 −0.124 [−0.27, +0.03]；3 日 −0.077 [−0.24, +0.08]；7 日 −0.083 [−0.25, +0.08]。因子最同意的 1/3：胜率 39~41%、平均收益 −0.56%~−0.83%；最不同意的 1/3：胜率 46~56%、平均收益 −0.21%~+0.26%。整体基准胜率 46%、平均收益 −0.16%。',
      '解读：区间都跨过 0，只能说“没看到任何帮助”，不能说有害。检验对因子偏有利（因子用截至 10 月的数据挑选，这些交易多数落在其训练/验证窗口内），仍无正向。分数是约 60 个币内的截面排序，AI 入场本身已是突破候选里挑出来的，持有期与止盈止损也与 1/3/7 日因子周期不对应。样本 137 多 / 41 空，偏小。',
      '同时改了一处：前向校验失败的因子不再进入给 AI 看的实盘参考分数（live_scores.json 新增 forward_failed_excluded 字段）；ensemble.json 与研究侧保持对前向结果“只展示、不参与选择”，否则前向不再是干净的样本外证据。',
    ],
    conclusion:
      '不提高因子在大模型开仓/平仓中的比重，仍仅作参考。若要升级，按顺序：前向出现 confirmed（扣费后多空转正）→ 候选/对照模拟盘跑满 ≥7 天且 ≥20 笔 → 先做确定性规则（因子强烈反对时否决开仓、仓位小幅缩放），最后才考虑提示词比重。',
    source:
      'scripts/factor_mining/live_scores.py · 检验脚本：对 data.db trader_positions 与 factor_mining 面板做点时分数对齐（一次性分析）',
  },
  {
    id: 'factor-mining-rdagent-followups',
    date: '2026-10-22',
    title: '因子挖掘后续优化（参考 RD-Agent）：四项均无可测量的质量收益',
    domain: '加密 · 因子挖掘流程',
    verdict: 'none',
    verdictText: '无法证明有价值',
    idea: '对照 microsoft/RD-Agent 的因子循环，检验我们流程还能怎么优化：增量选择集成因子、按家族历史产出分配变异预算、LLM 跨夜记忆。另对夜内闭环（每批评估后把训练集结果反馈给下一批，PR #6）做了有无反馈对照。',
    bullets: [
      '增量贪心选集成（按训练集 IC t 值逐个加入）：不论增益阈值取 0~2%，集成都被压到 2~3 个因子；训练集 t 升（h1 8.3→11.2）、验证集 t 两升一降，但验证集 IC 三个周期都下降（0.085→0.073、0.117→0.092、0.142→0.105）。未采用。',
      '变异按家族产出加权（仅用训练集 t 统计，保留 30% 均匀探索）：同数据同随机种子各 600 个变异，训练集 t≥3 的比例 49.4% 对 50.0%、t≥5 为 34.6% 对 35.5%。现有 family() 把大多数表达式归为 other，粒度太粗。未采用。',
      'LLM 跨夜记忆（前 3 晚训练集最优表达式与失败原因统计）：真实 GLM 各连跑两晚（每晚 40 条）；第二晚训练集初筛通过 64% 对 54%，新颖表达式 29 对 29；样本约 87，差距在噪声内。未采用。',
      '夜内闭环有无反馈对照（同数据副本、各 3 次、每次 60 条真实 GLM、不带变异）：第 1 批两臂条件相同，初筛通过率（训练集 t≥3）却是 94% 对 83%，这就是噪声量级；第 2/3 批 76%/59% 对 67%/72%，差距落在噪声内且方向不一致，top10 平均 t 也无差别（11.6/11.4/10.6 对 11.5/10.7/10.3）。唯一一致的信号：有效表达式数 41/44/46 对 39/40/37（带反馈约多 13%，3 次中最低值高于对照最高值），推测反馈里的合法样例减少了语法错误。结论：闭环不提升质量，只略增有效产出；保留（无副作用、无验证集泄漏），但不要期待它抬高入选率。',
      '实现过程中发现：evaluate.py 是冻结评估器，其 SHA256 会写入每次运行，不能往里加辅助函数；train_screen / reject_reason 因此放在 feedback.py。',
    ],
    conclusion:
      '三项优化没有证据支持，已回退；夜内闭环对质量也没有可测量提升，只略增有效表达式数，作为低成本改进保留。需要更大样本或更细的家族划分才可能看出差别；在此之前，把精力放在数据与前向校验上更有价值。',
    source:
      'scripts/factor_mining/ · PR #6（夜内闭环）· 实验：每臂 3 次重复的有无反馈对照',
  },
  {
    id: 'factor-ensemble-live',
    date: '2026-10-20',
    title: '因子挖掘 ensemble 接入实盘提示词（仅参考字段）',
    domain: '加密 · 提示词参考',
    verdict: 'partial',
    verdictText: '已接入 · 默认关闭 · 无 edge 证据',
    idea: '把夜间因子挖掘里已 accepted 因子的等权方向调整 z 分 ensemble，取最新已收盘日的截面百分位，导出为 data/factor_mining/crypto/live_scores.json；实盘引擎在 enable_factor_ensemble 开启时，把该币 1/3/7 日 horizon 的 pct/z 作为 factor_ensemble 参考字段交给 AI。',
    bullets: [
      '研究记录里已采用的结论（basket_vol 仓位缩放、12-1 动量排序）早已在线上，本次只补上因子挖掘这一路。',
      '数据过期保护：as_of 距今超过 3 个日历日、或币不在因子宇宙（约 60 个币）时不输出该字段；文件缺失/损坏一律静默降级。',
      '任何 hard gate、评分、候选筛选、下单逻辑都不读取该字段；提示词明确写「不能绕过 hard_entry_gate / rr_scan」。',
      '当前 crypto 入选因子的前向校验（约 176~179 天）绝大多数为 inconclusive（1 个 failed）：IC 为正（0.02~0.06），但多空净收益为负，属于「排序有信息、扣费后不可交易」。',
    ],
    conclusion:
      '只作为 AI 的参考证据上线，不提升为评分或入选条件；等前向校验出现 confirmed（多空净收益转正）且实盘决策样本里 pct 与结果有可测关系，再考虑升级。',
    source:
      'scripts/factor_mining/live_scores.py · kernel/factor_ensemble.go · docs/research/factor-mining-design.md「实盘参考字段导出」',
  },
  {
    id: 'breaker-state',
    date: '2026-10-19',
    title: '账户级连亏 / 日亏熔断：状态之后入场是否更差',
    domain: '加密 · 风控',
    verdict: 'none',
    verdictText: '无法证明有价值',
    idea: '问题：账户级、跨币种的“连亏 k 笔 / 当日累计亏损”之后再入场的交易，期望是否显著更差？现有熔断：日亏 10%（有效默认值）、连亏禁入仅按单币种、净方向止损风险 20%。10-19 日内约 −8% 未触发任何熔断。',
    bullets: [
      '数据两套：实盘成交 200 笔有初始止损的 R（6 周，入场时只用已平仓交易的状态）；历史 NEW≥50 信号流（同币同向单仓，每 UTC 日下采样 ≤8 笔近似实盘，20 个种子）。共 1650 个探索性比较。',
      '历史（20 种子均值）：24h 连亏≥4 之后入场 avgR −0.073R，与其余的差 −0.126±0.064R（p=0.050）；日亏≤−3R 之后 −0.110R，差 −0.157±0.077R（p=0.043）；24h 连亏≥3 差 −0.069±0.062R 不显著。名义显著未通过多重校正，后半段未复现。',
      '实盘：样本只有 6 个日历周，周聚类 SE 不可靠；其中 BNCUSDT 一笔 +34.44R 是止损距离几乎为 0 的手动同步仓造成的数据假象，主导了总体结果，因此实盘数字不能作为部署依据。静态剔除这些状态后，历史样本 sumR 与最大回撤改善，实盘样本几乎无变化或略差；未重放被剔除交易对后续状态的反馈。',
    ],
    conclusion:
      '无法证明账户级连亏冷却或更紧的日亏线有正期望价值，因此未改任何风控参数。若出于控制尾部风险想收紧，应作为风险偏好决定（如日亏线 10%→6%），而不是已被回测证明的优势。',
    source:
      'scripts/backtest/breaker_state_study.py（报告 docs/research/breaker-state-report.txt）',
  },
  {
    id: 'range-breakdown-short',
    date: '2026-10-19',
    title: '区间震荡期的破位做空：降级 / 回抽确认',
    domain: '加密 · 做空',
    verdict: 'none',
    verdictText: '无法证明有差异',
    idea: '10-19 实盘连续止损：BTC 三天在 80.8k–82.7k 区间震荡，凌晨砸到下沿后 V 型反弹 +2%，TIA / SEI / WLD / PROM 的破位做空在下沿附近入场被反弹打掉。问：区间期的破位做空是否该降级？改成等反抽再空是否更好？',
    bullets: [
      '沿用 breakdown_study 的 22569 个历史信号（100 币，2025-10 ~ 2026-10-13，下一根 5m 开盘市价空，SL=2×ATR，TP=4×ATR，72h，扣费滑点，同币同向单仓）。新增特征：BTC 最近 18 根 4h 振幅 btc_range72（三分位 + 先验阈值 <3%），入场价离 24h 低点的 ATR 距离 chase_low_atr。',
      '区间期 / 追下沿均无显著更差：range<3% 的 BASELINE 为 −0.037±0.064R（n=475），NEW≥50 为 +0.030±0.073R；振幅最低三分位反而是 +0.067±0.030R，且前半段 +0.39R、后半段 −0.09R，不稳定。',
      '回抽确认（限价空 ref+0.5/1.0×ATR，未成交按 0R 计）：成交率 53% / 38%，按信号计 sumR 相对市价入场的配对差 +0.003±0.032 / −0.014±0.040R；range<3% 子集 +0.053±0.091R，均不显著。',
      '更大的因素是市场方向：该破位空策略逐月 avgR 在 −0.33R ~ +0.51R 之间摆动（2026-03/04/05/07/08/09 为负，其余多为正），是净空头 beta，行情反弹的月份必然成串止损。共 66 项检验，未校正多重比较。',
    ],
    conclusion:
      '不建议为区间期加降级或回抽确认门：样本里没有证据。10-19 的连续止损更像反弹日的市场方向风险。若要治理，应试验组合层面的相关性 / 日亏熔断，而不是区间过滤。',
    source:
      'scripts/backtest/breakdown_range_study.py（报告 docs/research/range-breakdown-short-report.txt）',
  },
  {
    id: 'factor-mining-qlib',
    date: '2026-10-18',
    title: 'qlib 式自动因子挖掘（美股 + 虚拟币）',
    domain: '美股 + 加密 · 截面因子',
    verdict: 'running',
    verdictText: '进行中',
    idea: '参考 microsoft/qlib 的 Alpha158 表达式因子库与 RD-Agent 的 LLM 提案闭环，在日线截面上自动挖掘“因子值越高，未来 h 日超额收益越高”的因子，页面「因子挖掘」可实时查看。',
    bullets: [
      '表达式 DSL：白名单 AST，字段 $open/$high/$low/$close/$volume/$vwap/$ret，约 40 个算子（Ref/Mean/Std/Rank/Corr/Slope/Rsquare/Resi/EMA/CSRank…），窗口 ≤120，禁止任意代码；通用无前视测试。',
      '候选三路：195 个 Alpha158 风格种子 → 对 top 因子随机变异 → GLM-5.3-flash 提案（提示词只含 train 指标，不含 val / holdout）。',
      '评估：日 RankIC、Newey-West HAC t（lag=h）、五分位多空净收益（币 0.08% / 股 0.05% 单边）、换手；train/val/holdout 为定长滚动窗口（币 1095/365/180 天、股 756/252/126 交易日，每晚整体前移，purge h+2，旧因子每晚重评估），holdout 仅 judge.py 人工开启。',
      '前向校验：因子首次 accepted 时登记并冻结方向，仅用入选前选择从未读过的新数据统计 IC / 多空，只展示、不参与任何筛选，实盘参考以此为准。',
      '多重检验：同一 (市场, 周期) 的有效试验数 K 持续累加，t_req = max(2, Φ⁻¹(1−0.025/K))，旧因子会随 K 增大被降级。',
      'accepted 需 train 达标 + val IC 同号 t≥1.65 + train 与 val 扣费后多空收益均 > 0；与已接受因子截面相关 ≥0.8 标为冗余。',
      '首次运行：币 100 个标的（2019~今，点时刻成交额前 60），美股 91 个 bStock 正股 + SPY 基准（Yahoo 复权日线，约 6 年）；币圈大量 IC 显著但多空净收益为负（均值被高波动币的偏态主导），因此几乎没有 accepted。',
    ],
    conclusion:
      '这是研究层筛选，不接入任何下单；accepted ≠ 可交易（未建模资金费、冲击成本与容量，美股存在幸存者偏差）。每晚增量运行，结果以页面为准。',
    source:
      'scripts/factor_mining/ · docs/research/factor-mining-design.md · data/factor_mining/',
  },
  {
    id: 'autoresearch-glm',
    date: '2026-10-18',
    title: 'GLM 自进化评分挖掘框架',
    domain: '加密 · 多空评分',
    verdict: 'running',
    verdictText: '进行中',
    idea: '用订阅的 GLM-5.3-flash 夜间自主提出候选评分函数 score(X)，目标是“分数越高，对应方向收益 / 胜率越高”。',
    bullets: [
      '冻结评估器；候选评分在 AST 沙箱内运行，特征 ≤12 个。',
      '时间切分 train / val / holdout = 60/20/20，间隔 76h purge；holdout 放在独立私有目录，验证只返回通过/不通过。',
      '通过门槛 t_req = max(2, Φ⁻¹(1−0.025/K))，K 为有效试验次数（多重比较校正）。',
      '只晋升为“候选”，需人工 + Backtrader 复核；永不自动上线。',
      '新增数据源：OI 历史、全局 / 大户多空比、主动买卖比、premiumIndex K 线、恐慌贪婪指数、资金费历史。爆仓流因 allForceOrders 已下线（404）且无 websockets 库，暂未接入。',
    ],
    source: 'scripts/autoresearch/（构建中，定时任务待创建）',
  },
  {
    id: 'short-features-audit',
    date: '2026-10-18',
    title: '做空前置筛选审计 + 特征 IC + 样本外评分',
    domain: '加密 · 做空',
    verdict: 'none',
    verdictText: '无稳定优势',
    idea: '当前“前置筛选”本身是否把更适合做空的币选了出来？有没有“分数越高收益越高”的更好评分？',
    paragraphs: [
      '方法：100 币、2100 个决策时点（2025-10-29 ~ 2026-10-13，4h 步长），入场 = 下一根 5m 开盘做空，SL = ref + 2×ATR14(1h)，TP = ref − 4×ATR，72h；价格、成交量、主动买卖比、OI、资金费等特征，Fama-MacBeth 式截面 IC（周聚类 SE），五分位价差，50/50 与 4 折扩展窗口 walk-forward。',
    ],
    bullets: [
      '前置筛选（涨幅榜 / 历史涨幅池 / 破位池等）选出的币，扣费后收益不优于随机 12 币。',
      '单特征 IC 量级只有 ±0.02 ~ 0.03，且前后半段、牛 / 熊 / 震荡状态下符号会翻；ret_5d、ema_gap 对 netR 的 IC 为正但集中在后半段 / btc_bull（等于“趋势延续而非回落”，与做空直觉相反），并受多重比较影响。',
      '样本外综合评分（价格 + 量 + 主动买卖 + 资金费）没有找到稳定跨折有效的组合。',
      '市场状态开关（仅 btc_bear 时做空）样本外不成立：btc_bull 下做空约 −0.14 ~ −0.19R，btc_bear 略正但不显著。',
    ],
    conclusion:
      '当前数据下无法靠价量 / 资金费特征构造出“分数越高收益越高”的做空评分；“候选少”主要是供给结构问题，而不是漏掉了好机会。',
    source:
      'scripts/backtest/short_features_study.py · 提交 179ca29a · 报告 data/short_features_full_v3/report.txt',
  },
  {
    id: 'breakdown-score',
    date: '2026-10-18',
    title: '破位池延续型结构评分 + 豁免顶部确认门',
    domain: '加密 · 做空',
    verdict: 'partial',
    verdictText: '已上线代码 · 无 edge 证据',
    idea: '市场偏空但做空候选达标者少。根因：破位池沿用 pump-fade 的结构 / 背离维度，对已下跌币恒为 ~0（45% 权重失效），分数上限约 26，永远过不了 short_scan_min_score；破位候选还会被 SHORT_TOP_CONFIRM_MISSING 顶部确认门拦下。',
    paragraphs: [
      '改动：新分数 = 0.35 延续（1h EMA20 反弹失败 .45 + 24 根新低 .30 + 4h 更低高点 .25）+ 0.25 下行空间 + 0.15 拒绝 + 0.10 相对 BTC 弱势 + 0.15 资金费健康；universe=breakdown 豁免顶部确认门。',
    ],
    table: {
      head: ['方案', 'n', 'avgR（扣费）', '毛 avgR', '每决策候选数'],
      rows: [
        ['NEW ≥ 50', '4713', '+0.017 ± 0.020', '+0.075', '4.83'],
        ['随机对照（匹配 NEW）', '7484', '+0.021 ± 0.016', '+0.081', '—'],
        ['OLD ≥ 50', '620', '+0.064 ± 0.056', '+0.128', '0.31'],
        ['基线（全部破位候选）', '6946', '−0.003 ± 0.016', '+0.051', '—'],
      ],
    },
    bullets: [
      'NEW vs 随机 Δ = −0.004（95% CI [−0.065, +0.057]），NEW vs OLD Δ = −0.047（CI [−0.162, +0.068]）：均不显著。',
      '分数分层也不单调（NEW 30–40 反而 +0.086R，≥60 为 +0.036R）。',
    ],
    conclusion:
      '修复了供给（达标候选 0.31 → 4.83 个/决策），但没有证明收益更好；评分只是“把候选交给模型”，不是 edge 来源。',
    source:
      '提交 54074535（研究脚本）、c5fd7226（market/breakout/breakdownscan.go、kernel/engine_prompt.go）· scripts/backtest/breakdown_study.py',
  },
  {
    id: 'ict-standalone',
    date: '2026-10-18',
    title: 'ICT 概念作为独立入场信号',
    domain: '加密 / 股票永续',
    verdict: 'none',
    verdictText: '≈随机择时，扣费后为负',
    idea: '流动性扫荡（sweep）、MSS、CISD、FVG 回补、溢价 / 折价区（p1–p3）、ERL 被扫、SMT。每个 setup 配“同币同向同几何、错位 6–48h”的随机对照。',
    table: {
      head: ['样本', '信号', 'n', 'avgR', '对照 avgR', '差'],
      rows: [
        [
          '加密 30 币 5m ~120 天',
          'sweep/any',
          '18134',
          '−0.149',
          '−0.181',
          '+0.03',
        ],
        [
          '加密 100 币（剔除缺口币）',
          'sweep/any',
          '82950',
          '−0.112',
          '−0.122',
          '+0.010 ± 0.007',
        ],
        [
          '同上',
          'sweep/erl_swept',
          '10632',
          '−0.141',
          '−0.197',
          '+0.056 ± 0.019',
        ],
        [
          '同上',
          'cisd/any、mss/any',
          '35.8k',
          '−0.12 ~ −0.13',
          '−0.11',
          '−0.02 ~ −0.03',
        ],
        [
          '股票永续',
          'sweep/any',
          '23941',
          '−0.211',
          '−0.260',
          '+0.049 ± 0.013',
        ],
      ],
    },
    bullets: [
      '个别子条件（ERL 被扫、SMT）相对对照有 +0.05 左右的小幅差，但绝对 avgR 仍为负，扣费后亏损；p1 区间相对对照为 −0.15。',
      '111k 个 setup 的绝对结果一致为负：胜率 ~33%、盈亏比 ~1.6、PF 0.75–0.85。',
    ],
    conclusion: 'ICT 单独使用没有可交易 edge。',
    source:
      'scripts/backtest/{ict,ict_bt,ict_fetch,ict_study}.py · 提交 20d7083f、d024f15b、fdf49994',
  },
  {
    id: 'ict-on-ai',
    date: '2026-10-18',
    title: 'ICT 叠加到 nofx 的 AI 开仓信号上',
    domain: '加密 · AI 信号',
    verdict: 'none',
    verdictText: '没有帮助',
    idea: '用 2026-09-12 ~ 10-12 的 420 个 AI 开仓信号（可回测 317 个），按“近 4h 有 sweep / MSS / CISD / FVG 回补 / 位置是否折价做多”分组。',
    table: {
      head: ['分组', 'n', 'avgR', '胜率'],
      rows: [
        ['全部 AI 信号', '317', '−0.031 ± 0.056', '55%'],
        ['近 4h 有 sweep', '239', '−0.057 ± 0.064', '54%'],
        ['近 4h 有 MSS', '164', '−0.096 ± 0.077', '53%'],
        ['无 ICT 触发', '62', '+0.104 ± 0.127', '61%'],
        ['ICT 触发 & 位置合理', '32', '−0.083 ± 0.179', '53%'],
      ],
    },
    paragraphs: ['有 ICT 触发的信号反而略差，且样本小、SE 大于差距。'],
    conclusion:
      'ICT 不能提升现有 AI 信号质量。作为提示词参考字段保留 liquidity_sweep（enable_liquidity_sweep，默认关闭，仅作参考证据）。',
    source: '提交 d63cca06（kernel 可选字段）',
  },
  {
    id: 'regime-overlay',
    date: '2026-10-17',
    title: '美股市场状态仓位缩放（regime overlay）',
    domain: '美股 · 组合',
    verdict: 'adopted',
    verdictText: '已采用 basket_vol',
    idea: '在动量 Top-10 组合上叠加：已实现波动分位（RV_PCT / RV_RATIO）、广度（BREADTH）、VIX 期限结构、动量篮子波动（BASKET_VOL）、组合（COMBO），以及硬开关版本。Backtrader 日线，佣金 0.10%/边，无滑点。',
    table: {
      head: ['策略', 'CAGR', 'Sharpe', 'MaxDD', '曝险'],
      rows: [
        ['MOM_TOP + NONE', '94.7%', '1.510', '51.4%', '98%'],
        ['MOM_TOP + BASKET_VOL', '77.3%', '1.690', '29.5%', '80%'],
        ['MOM_TOP + RV_PCT', '76.1%', '1.484', '37.0%', '83%'],
        ['MOM_TOP + VIX_TS', '78.4%', '1.431', '50.0%', '90%'],
        ['MOM_TOP + BREADTH', '61.8%', '1.318', '36.3%', '86%'],
        [
          'HARD_RV / HARD_VIX / HARD_BREADTH',
          '62–68%',
          '1.27–1.35',
          '38–51%',
          '81–93%',
        ],
      ],
    },
    conclusion:
      '只有 BASKET_VOL 同时提高 Sharpe 并把回撤从 51% 压到 30%；硬开关因换手高、成本重，均不如连续缩放。绝对收益含幸存者偏差，应看相对改善。',
    source:
      '提交 a1a5f37c（回测）、0ddbad23（us_stock 上线 basket_vol）· docs/research/2026-10-17-regime-overlay-backtrader-1d.md',
  },
  {
    id: 'ma2060',
    date: '2026-10-16',
    title: 'MA20/60 双均线规则 vs 动量选股（美股组合）',
    domain: '美股 · 组合',
    verdict: 'partial',
    verdictText: '均线规则不占优',
    idea: '把 20/60 双均线规则与现行动量选股做组合级对比（1d 与 1h 两个周期）。',
    table: {
      head: ['策略', 'CAGR', 'Sharpe', 'MaxDD', '交易数', '胜率'],
      rows: [
        ['MOM_TOP', '94.7%', '1.510', '51.4%', '175', '54%'],
        [
          'MOM_X_MA（动量 × 均线过滤）',
          '59.6%',
          '1.391',
          '34.8%',
          '183',
          '51%',
        ],
        ['MA2060_RULES', '51.1%', '1.223', '40.9%', '688', '35%'],
        ['MA_CROSS', '43.1%', '1.090', '46.3%', '163', '41%'],
        ['等权买入持有 EW_BH', '55.6%', '1.520', '32.6%', '0', '—'],
        ['SPY 买入持有', '17.3%', '1.078', '18.8%', '0', '—'],
      ],
    },
    conclusion:
      '20/60 规则交易 688 次、胜率 35%，Sharpe 低于等权持有；宇宙是当前上市的 bStock 名单，存在幸存者偏差，等权持有本身就跑出 55%。均线只适合作过滤，不适合作为主信号。',
    source:
      '提交 0cd412af、28de52a3 · docs/research/2026-10-16-ma2060-backtrader-{1d,1h}.md',
  },
  {
    id: 'kell-ic',
    date: '2026-10-15',
    title: 'Kell 价格周期扫描分（美股）',
    domain: '美股 · 排序',
    verdict: 'none',
    verdictText: '无正向 edge',
    idea: '对生产用 stockengine.ClassifyCycle 评分做 IC 检验：77 个 bStock 标的、2022-08 ~ 2026-09、1020 个日期，目标为 β×SPY 残差收益，信号滞后 1 日。',
    bullets: [
      '10 日残差 Spearman IC = −0.014（NW t −1.91）；5 / 20 日同为负（t −2.3 / −2.8）。',
      'LongSetup 标的比其余低 0.84% / 10 日（t −3.31）；Top-10 超额 −21%/年，扣 10bp 成本后 −25%。',
      '相位事件：“买点”相位最差（wedge_pop ≈ 0%，ema_crossback +0.11%）；uptrend +1.07%、exhaustion_extension +1.71% 最好——在该宇宙里是动量有效，不是回踩入场。',
    ],
    conclusion: '当前扫描排名未经证实，任何改造都要先过 cmd/cycleic。',
    source: 'cmd/cycleic · docs/research/2026-10-15-kell-cycle-ic.md',
  },
  {
    id: 'momentum-ic',
    date: '2026-10-15',
    title: '动量类信号 IC（mom12_1 / rs63 / trend_rs / ext10 / sma200_mom）',
    domain: '美股 · 排序',
    verdict: 'adopted',
    verdictText: '动量有支持',
    idea: '检验可替代 Kell 评分的排序信号（10 日残差收益 IC）。',
    table: {
      head: ['信号', '10d IC', 'NW t', '备注'],
      rows: [
        ['mom12_1', '+0.045', '2.36', '非重叠 t 2.13，5 年全为正，换手 15%'],
        [
          'sma200_mom',
          '+0.049',
          '2.59',
          '先站上 SMA200 再按 12-1 排序；20d IC +0.066；换手 19%',
        ],
        ['trend_mom', '+0.045', '2.69', '2022 年为负，换手 43%'],
        ['trend_rs', '+0.030', '1.82', '不显著'],
        ['rs63', '+0.025', '1.43', '与 ret63 截面等价'],
        ['ext10', '+0.016', '—', '不显著'],
      ],
    },
    conclusion:
      '12-1 动量是唯一显著、跨年稳定的信号；趋势过滤版略好但与朴素版差异在噪声内。Top-N 组合 +35 ~ 40%/年的超额含幸存者偏差，只信 IC 符号与一致性，不信收益水平。',
    source: 'docs/research/2026-10-15-{momentum-signals,trend-momentum}-ic.md',
  },
  {
    id: 'entry-exit-replay',
    date: '2026-10-15',
    title: '加密开仓 / 平仓规则回测（决策记录重放）',
    domain: '加密 · 执行规则',
    verdict: 'partial',
    verdictText: '差异多在噪声内',
    idea: '信号来源：decision_records，约 420 个（2026-09-12 ~ 10-12，60 分钟去重）；每笔 σ ≈ 1R，sumR 标准误约 20R。',
    bullets: [
      '重叠约束是关键：不限制同币同向单仓时，市价入场与“纯 SL/TP”出场看起来好很多（+17R / +57R）；加上现实约束后差异缩回噪声，之前的结论被推翻。现为默认。',
      '市价 vs 限价入场：约持平；因此 10-12 修复了 limit_entry_enabled=false 时仍按限价的问题（cd81a9b1），上线 1–2 周后需用该工具复盘。',
      '出场：最一致（仍弱）的信号是过早保本档（breakeven_arm_r 0.75）在市价入场的两个半段都有害。',
      'quick 模式时间止损（4h）：全部强制 quick 时 −1.1R vs −9.5R，回撤减半；实际只有约 7 个信号用到，影响很小，保留。',
      'AI 平仓 vs 程序规则：25 笔历史 ai_close 从真实入场回放——实际 −9.1R，按程序规则持有 −12.4R，纯 SL/TP −14.9R；19/25 笔 AI 平仓更好。se≈3.5R，偏弱，但没有理由限制 AI 平仓。',
    ],
    conclusion:
      '真正的问题是入场 edge（strategy_health NO_EDGE，PF≈1.0），不是出场。',
    source: '提交 41382354（scripts/backtest）',
  },
  {
    id: 'meta-labeling',
    date: '2026-10-13',
    title: '信号过滤器（meta-labeling）',
    domain: '加密 · AI 信号',
    verdict: 'none',
    verdictText: '无可部署过滤器',
    idea: '420 个信号 × 67 个入场时特征（Structured Signal JSON），标签 = 模拟 R。walk-forward logit / ridge / HGB 样本外 AUC 仅 0.43–0.54。',
    paragraphs: [
      '一个 logit 前 50% 切分看似显著（p=0.007），但在多重检验后失效，换标签后翻转；严格嵌套 walk-forward（特征选择只用过去数据）p≈0.5。btc_beta（低 beta 更好）只在事后挑选时有效。',
    ],
    conclusion:
      '此样本量下没有可部署的统计过滤。累计 ~1000+ 信号后用嵌套流程重测。',
    source: 'scripts/backtest',
  },
  {
    id: 'quant-vault',
    date: '2026-10-07',
    title: 'Quant Trading Vault 策略族（每 5 分钟跑规则、AI 开仓）',
    domain: '加密 + 股票永续',
    verdict: 'none',
    verdictText: '0/6 族通过',
    idea: '6 个策略族代表 × 加密 1h/4h + Binance TradFi 股票永续 15m/1h，计入成本与资金费。',
    paragraphs: [
      '无一通过；唯一零成本下通过的是股票 15m RSI 均值回归，每单位换手仅约 1.5bp edge，无法交易。股票永续 2026-01-28 才上线，样本短。',
    ],
    conclusion:
      '不接入 nofx 信号层；至多把低换手的加密趋势特征作为 shadow-only 输入。',
    source:
      '~/workspace/quant-vault-research/（nofx 仓库外；REPORT.md、wf_harness.py）',
  },
]
