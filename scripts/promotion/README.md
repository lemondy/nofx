# 模拟盘配对与增量评估

从仓库根目录以 `python -m scripts.promotion.<命令>` 运行。`PROMOTION_DIR`
指定配对目录（默认当前 checkout 的 `data/promotion`），`NOFX_DB` 指定数据库。
建对和 `promote` 默认数据库为主仓库的 `data/data.db`；`report` 保持原有默认值：
当前 checkout 的 `data/data.db`。在 worktree 中运行报告时请显式设置 `NOFX_DB`。
主仓库通过 `.git` / `commondir` 解析，不依赖当前工作目录或调用 git。

## 建对与预登记

建对默认只读预览；检查后追加 `--apply` 才会按原有流程备份数据库、创建两个
停止状态的模拟交易员并写 `pairs.json`。在界面手动启动后用 `start` 记录首次启动时间。

```sh
python -m scripts.promotion.create_pair \
  --source-trader SOURCE_ID --feature-path indicators.enable_factor_ensemble \
  --feature-value true --pair-id ensemble-v2 --title 因子增量验证 \
  --mode incremental --track crypto_llm --baseline-version CONFIG_HASH \
  --idea-id ensemble-input-v2 --change-type A
python -m scripts.promotion.create_pair start ensemble-v2
```

增量模式要求 `track`、`baseline-version`、`idea-id`、`change-type`。
交易线为 `crypto_llm / us_llm / fq_crypto / fq_us`，改动类型为 `A–E`。

| 参数 | 增量默认值 |
| --- | --- |
| `--margin-r` | 0.03 R |
| `--min-days` | 30 天 |
| `--min-trades` | 每侧 50 笔有效 R |
| `--harm-check-days` | 14 天 |
| `--extend-days` | 14 天 |
| `--extend-se` | 0.02 R |
| `--metric` | `avg_r`（可选 `daily_return`，见下） |
| `--margin-daily` | 0.0002（每日收益差的非劣效边际，仅 `daily_return` 使用） |
| `--trade-ratio-max` | 1.5；`none`/`null` 表示不设上限 |
| `--min-short-trades` | 不限；设置后要求候选组 SHORT 平仓 ≥ 该笔数 |
| `--min-trades-scope` | `both`（可选 `candidate`，`min-trades` 只约束候选组） |
| `--extend-se-daily` | 0.0001（`daily_return` 的 extend SE 门槛） |

`prereg` 包含上述参数、模式、交易线、基线版本、思路及改动类型、UTC `registered_at`，
以及 `prereg_hash`。哈希为其余全部 prereg 字段的 SHA-256：JSON 按键排序、使用紧凑
分隔符 `(',', ':')`、保留 UTF-8 中文、禁止 NaN/Infinity。没有修改登记的命令。
未指定增量模式时，不产生 prereg，原有 7 天 / 20 笔配置默认值及报告行为保持不变。

2026-11-04 之前登记的 prereg 没有 metric 等新字段：校验对缺失字段取默认值
（`avg_r`、0.0002、1.5、不限、`both`、0.0001），判定行为与当时完全一致；哈希仍只
覆盖实际存在的字段。

历史配对可以且只能补登记一次，参数与新建时相同。例如：

```sh
python -m scripts.promotion.create_pair prereg factor-ensemble \
  --mode incremental --track crypto_llm --baseline-version CONFIG_HASH \
  --idea-id factor-ensemble --change-type A
python -m scripts.promotion.create_pair prereg early-trim-0p8r \
  --mode incremental --track crypto_llm --baseline-version CONFIG_HASH \
  --idea-id early-trim-0p8r --change-type C
```

补登记在 prereg 内写入 `retroactive: true` 和说明，并打印：
「补登记发生在已看到部分结果之后，结论需打折」。说明和标记也纳入哈希。
已有 prereg（包括损坏的登记）一律拒绝覆盖。补登记保留原有外层配置，旧口径判定仍可参照。
以上命令为操作示例，本次实现不自动补登记现有配对。

## us_stock 配对（做空模拟）

源交易员是 us_stock（`strategy_type=us_stock`）时，`create_pair` 走专用路径：

- **不新建 paper 交易所**：us_stock 模拟盘依赖 binance 行情，两组克隆保留源交易员的
  `exchange_id`；源交易所不是真实 binance 交易所时直接拒绝。
- **两组强制 `stock_config.paper_trading=true`**（对照组也是）；候选组再打
  `--feature-path/--feature-value` 补丁（只作用于候选）。源策略本身已开启
  `paper_trading` 时拒绝建对。
- **`--paper-cash`**（默认 1000）同时设定两个克隆的 `initial_balance`，并记入 pair 元数据。
- 交易从 `stock_paper_positions` 读取（`closed_at` 为退出时间，
  R = realized_pnl / (|avg_price − initial_stop| × entry_quantity)，side/funding 列缺失时
  按全 LONG、资金费 0 处理）；权益快照仍读 `trader_equity_snapshots`。

做空模拟的建对命令（≈45 个美股交易日，预登记写死后再不改）：

```sh
python -m scripts.promotion.create_pair \
  --source-trader SOURCE_US_STOCK_ID \
  --feature-path stock_config.short_mode --feature-value '"paper"' \
  --pair-id us-short-paper-v1 --title 美股做空模拟 \
  --mode incremental --track us_llm --baseline-version CONFIG_HASH \
  --idea-id us-short-paper-v1 --change-type E \
  --metric daily_return --margin-daily 0.0002 --trade-ratio-max none \
  --min-trades 30 --min-trades-scope candidate --min-short-trades 15 \
  --min-days 63 --harm-check-days 20 --extend-se-daily 0.0001 \
  --paper-cash 1000
python -m scripts.promotion.create_pair start us-short-paper-v1
```

## 报告与判定

```sh
python -m scripts.promotion.report
python -m scripts.promotion.report --ledger
python -m scripts.promotion.report --ledger /path/to/ledger.jsonl
```

报告只读数据库，原子更新 `PROMOTION_DIR/report.json`。没有 prereg 的配对保持旧字段、
`ready / hold / insufficient` 判定和文本输出。登记哈希不符时，该配对仅输出 `id` 和
`verdict: prereg_tampered`，不计算统计量或记账。

增量模式新增 `delta`（Δ）、`se`、`lower_bound`（Δ − 1.645·SE）、`trade_ratio`、
完整 prereg 和 `retroactive`。`trade_ratio` 使用候选/对照的平仓总笔数；正式样本门槛
使用每侧有效 R 笔数。对照没有平仓时，比值为 null。

按以下顺序判定，天数为从 `started_at` 起经过的 UTC 天数：

| 判定 | 条件 |
| --- | --- |
| `insufficient` | 未到伤害检查期 |
| `harm_stop` | 达到伤害检查期，SE 可计算且 Δ < −2·SE |
| `insufficient` | 未到最短天数，或任一侧有效 R 少于最少笔数 |
| `promote` | Δ > 0、下界 > −margin_r、0.67 ≤ 平仓数比 ≤ 1.5 |
| `fewer_trades` | Δ > 0、平仓数比 < 0.67 |
| `extend` | SE > extend_se，且运行天数 < min_days + extend_days |
| `reject` | 其余情况，包括正式判定期内 SE 缺失且未命中前面的条件 |

`weekly_avg_r`、其他旧统计字段和旧判定逻辑保留，旧判定放在 `legacy_verdict`，原因放在
`legacy_reasons`。增量计算复用 `weekly_r_comparison`，但将统计计算门槛设为每侧 1 笔，
独立应用登记中的正式样本门槛，使 `harm_stop` 可以早于 min_days/min_trades 触发。
仍需联合至少 3 个退出周才能给出 SE。该计算保存在 `incremental_weekly_avg_r`。

### metric=daily_return

`metric=daily_return` 时（`incremental_metric` 字段标明，比较保存在
`incremental_daily_return`）：

- 主统计量改为每日权益收益率差：每臂取 UTC 日末最后快照之比（缺日向前填充，与旧口径
  相同），配对两组都有的已完成 UTC 日；Δ = mean(候选 − 对照)。
- SE 用 `cluster_se` 按 UTC 周一所在周聚类（交易日为簇内样本）；不足两个聚类周时
  不给 SE/CI。
- 判定表与 avg_r 相同，但非劣效边际用 `margin_daily`，extend 的 SE 门槛用
  `extend_se_daily`（独立登记字段，默认 0.0001/日；不按 margin_daily/margin_r 折算，
  避免复用一个对该指标无意义的参数）；正式样本按平仓笔数（`n`）计，
  `min_trades_scope=candidate` 只约束候选组，`min_short_trades` 只约束候选组
  SHORT 平仓。`trade_ratio_max=null` 时 promote 不设平仓数比上限（0.67 下限不变）。
- 报告额外给出仅参考的辅助字段 `us_stock_extra`：候选组空头 avgR 与笔数、
  候选组多头 avgR − 对照组多头 avgR、两组 MaxDD、候选组空头资金费合计
  （平仓 SHORT 行的 funding 求和）。这些字段不参与任何判定。

### avg_r 与新字段

`metric=avg_r`（默认）的判定与之前完全一致；若 prereg 里显式登记了
`trade_ratio_max`、`min_trades_scope` 或 `min_short_trades`，同样按其生效：
缺失或为默认值时，行为与旧版硬编码的 1.5 上限、两侧门槛、不限空头完全相同。

## 晋升与保留对照

```sh
python -m scripts.promotion.create_pair promote ensemble-v2 --holdback-days 28
```

`promote` 在内存中重算该配对的最新报告，数据库以 `mode=ro` 打开；仅接受最新判定
`promote`。选择重算可以避免使用过期 `report.json`，且不写报告、不记账。它只原子更新
`pairs.json` 的 `promoted_at` 和 `holdback_until`，拒绝重复晋升或重置对照期。
目录锁协调元数据写入，不创建持久锁文件。

正式交易员配置须手动修改并经用户批准；原对照模拟盘应继续运行。
报告在保留期内仅用严格晚于 `promoted_at` 平仓的交易计算增量和交易数比：
Δ < −2·SE 时为 `rollback_suggested`，否则为 `holdback_ok`。
缺少 SE 时仍为 `holdback_ok`，原因会注明无法排除伤害。
到达 `holdback_until`（包含等于时刻）后为 `holdback_done`。
`evaluation_started_at` 与 `evaluation_closed_counts` 标明增量窗口及平仓笔数；旧统计仍是全期。
回滚提示不会修改任何交易员配置。

## 账本

默认不记账；`--ledger` 不带路径时使用主仓库 `data/evolution/ledger.jsonl`。
`harm_stop / promote / fewer_trades / reject` 记 S2，`rollback_suggested` 记 S3，其他状态不追加。
每行包含思路、改动类型、基线、Δ、SE、z、平仓数比、pair_id、登记哈希与补登记标记。
SE 为零或未知时 z 为 null。S2/S3 不增加只统计 S1 的 K，沿用共享账本约定。
通过 `evolution_ledger.append_event` 写入，在其排他锁内按 `(pair_id, stage, verdict)`
去重，重复执行或并发运行都不会重复记同一结论。

## 基线登记

```sh
python -m scripts.promotion.baseline show
python -m scripts.promotion.baseline set --track crypto_llm --strategy-id STRATEGY_ID --note "人工批准的晋升"
python -m scripts.promotion.baseline set --track crypto_llm --strategy-id STRATEGY_ID --note "人工批准的晋升" --yes
python -m scripts.promotion.baseline rollback --track crypto_llm --yes
```

默认路径为主仓库 `data/evolution/baselines.json`。每个子命令可用 `--registry PATH`
指定临时登记文件；`set` 可用 `--db PATH` 指定数据库（否则用 `NOFX_DB` 或主仓库 DB）。
`show` 列出全部交易线的当前基线，没有登记时显示 null。

`set` 只读 `strategy_config_versions`，按 `changed_at DESC, id DESC` 取该策略的最新
`config_hash`。默认只打印拟变更，`--yes` 才原子写 JSON：

```json
{"crypto_llm":{"current":{"strategy_id":"...","config_hash":"...","set_at":"UTC","note":"..."},"previous":null}}
```

再次 set 会把旧 current 留作 previous；rollback 交换 current/previous，保留原登记时间和说明。
rollback 省略 `--yes` 也只预览。所有登记与回滚操作都只写 JSON，不写数据库或交易员配置。

## 本地验证

测试只使用临时目录和数据库：

```sh
python -m pytest scripts/promotion -q
```

检查实时 promotion 目录时只运行 `report --help`；需要运行报告则使用复制到临时目录的输入。
