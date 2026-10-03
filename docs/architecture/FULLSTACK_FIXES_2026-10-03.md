# NOFX 复审问题修复记录 · 2026-10-03

对应审查：[FULLSTACK_REVIEW_2026-10-03.md](/Users/zhangyun/workspace/nofx/docs/architecture/FULLSTACK_REVIEW_2026-10-03.md)，原基线 `e68ed1fd9110`。本记录针对当前工作区实现；尚未提交、部署或重启服务，未操作实盘账户或生产数据库。

审查的 R01–R20 均已加入代码修复或明确的安全缓解。R18 采用禁止应用无法重放验证的评分中心变化；没有实现完整原始特征重放。R11 的共享执行锁覆盖同一进程内引用同一用户/交易所配置 ID 的交易器，不应扩展为分布式或跨配置的真实账户全局锁。

## 逐项处理

| 编号 | 当前行为与主要实现 | 验证 |
|---|---|---|
| R01 | 移动止损先保存旧 ID，挂新单并核验，再仅撤旧 ID；不会批量撤掉新止损或误撤 TP。Binance 单止损限制下替换失败会补回旧保护并核验恢复。 | 原成功路径回归；Binance 本地 HTTP 模拟的新挂单失败、旧单恢复可见/不可见测试。 |
| R02 | SL/TP 实际数量和价格核验完成才推进 ProtectedQty；失败撤销旧水位，保留恢复任务。 | 订单不可见、水位撤销后终态清理回归。 |
| R03 | 每次按累计实际成交量检查覆盖，补挂数量差额；区分 TP/SL 和 closePosition。Bybit 规范条件单方向，OKX 合约张数转基础数量，Bitget 规范计划单。 | 多次部分成交；Bybit 四种止损/止盈触发方向测试。 |
| R04 | 撤销、过期、拒绝及离线恢复仅在最终有效回执和残量保护确认后清理；查询失败、数量倒退、保护失败均保留任务。 | 取消后部分成交、最终查询未知、非法/倒退回执测试。 |
| R05 | Hyperliquid 使用订单历史与逐订单成交记录，不从 open orders 消失猜成交。标准与 XYZ 下单返回真实 ID/成交数量/均价，拒绝伪造 FILLED 或 ID=0。 | 零数量终态回归；真实 SDK 回执形状的拒绝/挂单/成交测试。 |
| R06 | 订单状态与保护使用账户执行互斥；AI 等待释放执行权，panic 也重新获取。停止取消 AI 请求，先撤入场与处理残量再等待主循环；所有交易器先接收停止信号。退出不再因固定 10 秒截断关键清理。 | 慢 AI 期间保护、停止早于 AI join、panic 锁恢复、普通/流式 HTTP 取消及 race 检查。 |
| R07 | 网格停止、日损失、突破暂停使用统一撤单/残量保护协议，只撤本交易器持有 ID。紧急退出只处理账本所属格点，实际最终平仓未知时返回未完成并保留 ExitOrderID，禁止重复发单。 | 停止撤单、撤单拒绝、无关订单保留、紧急退出未知回执及后续确认测试。 |
| R08 | 网格读取订单累计成交数量与真实均价，部分成交后撤单仍保留仓位；不再用账户总仓位猜具体订单全成交。入场与退出水位、ID、暂停状态保存在 grid checkpoint，重启先恢复对账。调格不得丢弃已有仓位。 | 部分撤单、晚到成交、未知退出不重发、持久化恢复测试。 |
| R09 | 网格复用持久化账户日初权益，峰值单调更新；重载保留损失基线。正常停机与风控/未知暂停分开，重启不会清除风险暂停。 | 同日重载保持日损失；不同暂停原因重启测试。 |
| R10 | 仓位、权益、预留信息未知时拒绝新增风险；同标的双向仓位按 gross exposure 累加，非有限权益不用于预算。 | 仓位查询失败拒绝入场的原回归及全量风险测试。 |
| R11 | 同账户配置的执行、风险核查串行化；纳入其他交易器普通挂单、网格挂单及数据库中的未恢复计划。AI 返回后清除适配器余额/仓位缓存并重新读取，再计算准入额度。 | 其他交易器挂单超限、实例注销后持久化预留仍占额度测试。 |
| R12 | 运行意图附带单调 RunVersion；重载、自动恢复按版本启动，旧循环不能覆盖新停止意图。停止即使内存实例不存在也能落库。模型/交易所缺失或禁用时移除旧实例并停止恢复。 | 重载旧版本拒绝启动、无内存实例停止 API 测试。 |
| R13 | 连亏熔断改用 RealizedPnL − Fee，与净盈亏统计保持一致。 | 毛盈利但扣费后连续亏损触发熔断。 |
| R14 | AI 入场来源按 order ID 持久化，live mark 清理后仍可 stamp 同步仓位；晚到登记可修复已关闭的匹配行。已知 AI 订单标记不再认领同币同向手动新单。旧无订单来源标记保留兼容。 | 开仓同步→登记→清理→平仓同步；清理后开仓同步；晚到来源、手动订单隔离测试。 |
| R15 | UI 语言切换不覆盖策略语言和自定义 Prompt；替换模板仍需要用户显式操作。 | 原真实页面语言切换回归。 |
| R16 | 异步响应检查当前策略 ID、草稿及版本；保存返回自身提交的 updated_at，后台刷新不推进脏草稿版本。409 保留草稿供后续处理，纳秒版本比较不退回旧响应。 | 保存 A 后选择 B；保存后继续编辑、外部版本推进后 409 保留草稿；服务端提交版本测试。 |
| R17 | 已存在 PostgreSQL journal 表也执行 ensureColumns 增量迁移，不再直接跳过。 | 在临时 SQLite 重建旧 schema，验证新增列、旧行保留和幂等；未连接 PostgreSQL 实例。 |
| R18 | 不应用无法用当前历史信号重放的新 price_atr_center/vol_center，不再将旧参数保留集收益作为这些变化已验证的证据；阈值调整仍保留时间切分、purge 与独立保留集检查。 | 正保留集收益可以验证阈值，但评分中心保持不变的回归。 |
| R19 | 流式 HTTP 复制完整 client，保留重定向策略；直连只拨已解析且校验过的 IP，避免二次 DNS。代理请求的目的 URL 和重定向继续校验。 | 流式重定向钩子；代理目的地私网/元数据/IPv6 地址与重定向拦截测试。 |
| R20 | Docker 上下文排除 .env 变体、secrets/keys、私钥文件、data/、数据库及 WAL 等副文件，保留 .env.example/.env.sample。 | 路径规则静态核对；未上传构建上下文。 |

## 工程检查与可复现证据

恢复完整 ESLint/Prettier 门槛并统一前端历史格式，修正少量无效表达式、未使用规则及渲染期重定向。此次 diff 含大范围格式清理，应与交易逻辑变更分别阅读。

App 页面按需加载，大型第三方库拆分。生产主 JS 从审查的 2,188.82 kB（gzip 628.68 kB）降至 469.10 kB（gzip 160.26 kB），各 JS chunk 均小于默认 500 kB 警告阈值；这是拆包后的单文件大小变化，不表示全部依赖传输量减少同等比例。

最终检查日志存放在 [fixes](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes)。原审查失败日志保留在 evidence 目录，避免将审查基线与修复结果混合。复现测试已纳入正常 Go/React 测试套件；原隔离 mock 补上逐订单撤销行为及禁用行情网络访问，仍断言原审查安全契约。

| 命令 | 结果与证据 |
|---|---|
| `go test ./... -count=1` | 全量通过：[go-test.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/go-test.txt) |
| `go test -race ./trader ./store ./api ./manager ./kernel ./mcp ./security ./market/breakout ./trader/binance ./trader/bybit ./trader/hyperliquid -count=1` | 通过；最后网格退出修改后再补跑 trader race：[go-race.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/go-race.txt)、[trader-race.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/trader-race.txt) |
| `go vet ./...` | 通过，无输出：[go-vet.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/go-vet.txt) |
| `npm test`（web） | 7 个文件、113 项通过：[web-test.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/web-test.txt) |
| `npm run lint`、`npm run format:check`（web） | 均通过：[web-lint.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/web-lint.txt)、[web-format.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/web-format.txt) |
| `npm run build`（web） | TypeScript 与 Vite 构建通过，无大 chunk 警告：[web-build.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/web-build.txt) |
| 原 `reproduce.py` 与 `reproduce_web.py` | 原 14 个 Go 与 2 个真实页面场景转绿：[review-go.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/review-go.txt)、[review-web.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/fixes/review-web.txt) |
| `git diff --check` | 通过。 |

macOS race 构建有 LC_DYSYMTAB 链接器警告，未出现测试失败或检测到的数据竞态。未据此保证所有可能的调度交错均已验证。

## 部署与验证边界

- 数据库初始化将新增 RunVersion、ExecutedQty、AI 入场来源和网格 checkpoint 等结构，并补齐 journal 列。已在临时 SQLite 验证，真实 PostgreSQL 旧库升级仍需独立验收；本次没有运行生产迁移。
- 未向真实交易所发送订单。交易所条件单可见延迟、查询范围、账户模式与 SDK 返回兼容性需要沙盒/小额受控验收。Binance 单止损限制下无法实现绝对无间隙替换；失败补偿也依赖交易所可用性。
- 交易所查询或撤单/保护失败仍可能留下风险；代码保留任务、返回/记录失败并暂停新增风险，不能保证断网期间交易所最终完成撤单或保护。Stop 完成表示交易器停止；不代表账户平仓或所有交易所操作已确认。未知网格退出 ID 保留为 unknown，不能自动重发，需真实订单对账后处理。
- 共享锁的部署边界是单后端进程、同一 userID + exchangeID。多个后端副本、不同配置 ID/API key 实际指向同一账户时，需要真实账户身份映射和数据库原子预留方案，不能使用本实现宣称跨进程账户上限安全。
- 日损失使用现有 UTC 交易日和账户权益口径；现金转入转出及未入账资金费用的专项调整不在本次实现范围内。
- 不具备原始特征重放时自动调参不会改评分中心；完整参数组的保留集交易重放仍是后续能力，而非已验证结果。
- 报告中 LONG 按 AI 来源/方向/策略版本独立健康度是后续策略设计项，本次不根据 177 笔汇总数据擅自改实盘门槛。代码修复不等于证明这些缺陷造成了 -8.39 盈亏，也不保证策略盈利。
