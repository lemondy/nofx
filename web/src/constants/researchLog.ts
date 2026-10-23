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
