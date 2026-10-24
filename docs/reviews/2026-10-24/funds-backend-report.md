# 资金总览后端实现与离线核对

工作目录：`funds-be`；接口契约：`docs/design/funds-overview.md`。未修改 `web/`，未 commit/push，未调用真实交易、下单或资金划转操作。主库仅通过 `file:.../data.db?mode=ro&_query_only=on` 读取，未执行迁移或写入；报告写在当前 worktree。

**已实现**

- 独立纯函数包 `fundsledger`：账户汇总、每日明细、实盘每日合计、转账流、Modified Dietz 和原始收益率。
- `account_income` 自动迁移、四字段唯一去重、原始 income_type 保留、分类与读写方法。额外 `account_kind` 区分同一 exchange_id 的现货/合约钱包：真实数据里的 us-stock 和 mac-nofx 正好共用 exchange_id，不能混用流水。
- 可选 `types.IncomeHistoryReader`，没有修改共享 Trader 接口。合约读取全部类型；现货尽力读取充提、跨 MAIN 钱包的双向 universal transfer 历史。
- 登录保护的 `GET /api/funds/overview`，days/tz/include_paper 参数校验、交易员与交易所双重用户隔离、无数据账户 warning、请求触发同步。每个用户的交易所账户串行同步两种钱包，每 15 分钟最多一个同步周期。

**口径与取舍**

- 日期按请求 IANA 时区，默认 Asia/Shanghai。首日用当天第一条快照为开盘；以后用上一日收盘。缺快照日沿用上次权益，不生成推断转账；恢复快照后扣除缺失期间已知流水再判断残差，避免重复推断。跨缺快照期的实际转账日期不确定，flow.note 会说明。
- 先扣已确认转账，再对剩余残差执行严格 `abs(residual) > max(2, 2% × open_equity)`。小额残差保留为 unexplained；确认与推断同日并存时 source=mixed。
- 合约 trading_pnl / fees 按日优先采用 REALIZED_PNL / COMMISSION；对应类型当日没有流水时回退持仓行，避免重复相加。
- 美股实盘 `trader_positions.realized_pnl` 是毛盈亏，fee 单列，不能再把毛盈亏当净额加回手续费。未平仓行可能已经部分卖出，累计已实现盈亏及手续费也纳入；由于缺少逐笔时间，使用 updated_at 近似归日，并返回 warning。模拟美股只读取 stock_paper_positions，不混入实盘持仓；部分卖出同样存在时间近似。
- account/totals 使用全部可用历史的累计金额；days 只限制 daily/daily_total/flows 的展示窗口，避免切换 7 天后漏扣更早转入。收益率字段单位为百分数。Modified Dietz 的区间为首末快照，确认流水使用实际时间，推断流水用当日末快照时间近似；合计用各账户加权本金之和计算，不对收益率取算术平均。
- 模拟盘即使 include_paper=1 也不计入 totals/daily_total。没有快照的账户保留 warning，暂不计入合计；过期快照的权益沿用并明确提示。
- USD/USDC 等稳定币按 1:1；非稳定币流水保留原币资产和数量，但不冒充 USDT 加入金额对账，返回 warning。提现金额按钱包减少额（amount + 网络费）记负转账。
- 首条快照以前的资金变动无法重建；与配置 initial_balance 的差额只告警，不凭空补一笔转账。

**真实数据离线冒烟**

生成时点固定为 2026-10-24 02:00:00 UTC（北京时间 10:00），tz=Asia/Shanghai，days=90。未请求真实交易所账户历史，主库尚无 account_income 表，因此以下全部转账均为 inferred，非交易所确认。完整 JSON 见 `reports/funds-smoke-2026-10-24.json`。

| 账户 | 初始资金 | 最新权益 | 净转账 | 毛交易盈亏 | 手续费 | 未解释差额 | 剔转账盈亏 | 原始收益率 | Dietz 收益率 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| mac-nofx | 135.18413468 | 263.42573007 | 86.69679934 | 32.26375526 | 23.37526869 | 3.08744646 | 41.54479605 | 94.86438308% | 42.44706633% |
| us-stock | 100.07600000 | 730.75903044 | 629.78940566 | 1.74140500 | 0.28827799 | -1.14792623 | 0.89362478 | 630.20407534% | 0.17199241% |

| 日期（北京时间） | 账户 | 转账金额 | 来源 |
|---|---|---:|---|
| 2026-10-22 | us-stock | +190.55768050 | inferred |
| 2026-10-22 | mac-nofx | +100.53922620 | inferred |
| 2026-10-14 | us-stock | +439.23172516 | inferred |
| 2026-10-14 | mac-nofx | +7.97082729 | inferred |
| 2026-10-01 | mac-nofx | +40.03136254 | inferred |
| 2026-09-21 | mac-nofx | -2.99165493 | inferred |
| 2026-09-19 | mac-nofx | +3.03650427 | inferred |
| 2026-09-16 | mac-nofx | +20.01540000 | inferred |
| 2026-09-03 | mac-nofx | -6.71806923 | inferred |
| 2026-09-02 | mac-nofx | -157.76172725 | inferred |
| 2026-08-30 | mac-nofx | +82.57493045 | inferred |

- us-stock：10-14 收盘权益从 100.18000000 到 539.15440516（+438.97440516），浮盈变化 -0.25732000，残差 +439.23172516；对应题述约 +435 的大额跳变。10-22 权益增加 191.18608505，浮盈增加 0.62840455，残差 +190.55768050，符合题述约 +191。剔转账后收益率为 +0.17199241%，没有再把充值计成数百个百分点盈利。
- mac-nofx：10-22 权益增加 118.31640482，毛已实现盈亏 5.78365999，手续费 0.21238932，浮盈增加 12.20590795，因此推断转账为 100.53922620。题述约 +120 是权益跳变，按契约扣除可解释盈亏后不能全部当作转入。
- mac-nofx：09-01 无快照，恢复快照的北京时间 09-02 得到 -157.76172725 的累计残差；金额对应题述约 -157，但无法根据缺失快照断言实际转出发生在 09-01 还是 09-02。
- mac-nofx 首条快照 165.31303361，比配置初始资金 135.18413468 高 30.12889893；此历史缺口会影响累计 adjusted_pnl 和收益率，已返回 warning。其余小额 inferred 也可能包含缺成交、缺资金费或估值时点误差，不能当作已确认充值。

**币安接口与权限发现**

- 已引用 go-binance v2.8.9，包含 deposit / withdraw / universal transfer history 服务，不需要引入依赖。
- SDK 的充值、提现结构遗漏 walletType；直接使用其解码结果会把 funding 钱包历史误算为现货。实现保留 SDK 的 Deposit/Withdraw 字段，复用现有签名 transport 发只读 GET，并补充 walletType 解码；walletType 缺失时跳过并返回可识别错误。只确认现货 walletType=0、已成功状态；待处理或其他钱包记录不计入。
- universal transfer 历史使用 SDK 服务，按方向及页码查询（100/页），只统计跨 MAIN 的已确认记录。充值/提现按 offset 分页（1000/页），窗口严格小于 90 天。子账户、经纪商或其他未覆盖的划转通道仍依赖残差，不能承诺历史完整。
- 合约 income 不传 incomeType，1000/页，使用与 historyCtx 相同的 Normal 权重优先级及现有 metered transport。SDK 没有分页页码字段；满页时间窗口递归拆分并按时间推进，保住同毫秒记录；单个毫秒仍满 1000 条时返回明确不完整错误，不静默跳过。
- 官方说明：[合约 income 仅保留近三个月](https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Get-Income-History)，[universal transfer history 保留近六个月](https://developers.binance.com/docs/wallet/asset/query-user-universal-transfer)。
- 没有用真实 API Key 发起历史查询，真实账户权限尚未验证，现货确认转账实际取得数为 0。权限不足、超时、接口不可用或部分结果均只产生 warning；可读到的部分仍落库，缺口回退推断。
- 资金接口只使用 traderManager 中已加载的实例。未加载时告警；不会调用 LoadUserTradersFromStore，因为现有合约构造函数会设置持仓模式，超出本任务只读约束。
- 同步部分失败后保留本进程重试起点，避免已落库的新记录跳过失败窗口；该重试起点和 15 分钟限频状态在进程内，重启后使用已落库最新时间 -1 小时。首次返回空历史则之后仍回填 90 天，直到有可用游标。

**验证**

所有命令均设置 `GOCACHE=$PWD/.gocache GOPROXY=off`；测试另外设置 `BINANCE_LIVE_TEST=0` 关闭既有实盘集成测试。

- `go build ./...`
- `go vet ./fundsledger ./store ./api ./trader/binance ./trader/binance_bstock ./trader/types`
- `go test ./fundsledger ./store ./api ./trader/binance ./trader/binance_bstock ./trader/types -count=1`
- `go test -race ./api -run ^TestFunds -count=1`

新增测试覆盖纯交易日、确认转账、正负阈值边界、确认后的残差、缺快照与跨缺失日已知流水、首日、北京时间日界线、Dietz 手算（100 初始、中途 +500、2 盈亏，2/350 ×100）、模拟盘排除、真实美股持仓形态、分类去重、共用 exchange_id 钱包隔离、用户及外部交易所引用隔离、90 天回填/增量重叠/失败重试、并发限频、HTTP 历史分页及权限失败。

第一次全套运行触发已有 `httptest.NewServer` 监听端口 panic（bind: operation not permitted）。已将 Binance 相关 3 个旧测试文件中的 5 处服务改用内存 HTTP transport，并在构造函数测试调用前注入 mock 客户端；不监听端口，不触达真实币安。最终验证结果以下方最终执行记录为准。

最终执行记录：`go build ./...` 与上述 `go vet` 均退出 0；6 个相关包的 `go test -count=1` 全部通过；新增 Funds API / 同步测试的 `-race` 通过。race 链接时出现 macOS LC_DYSYMTAB 工具链警告，测试仍退出 0。`git diff --check` 通过，工作区改动未包含 web/。
