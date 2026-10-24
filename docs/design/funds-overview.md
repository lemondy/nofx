# 资金总览页面（/funds）设计与接口契约

目标：汇总所有账户的资金情况，展示每日盈亏，并把「转账（充值/提现/内部划转）」与「真实交易产生的盈亏」分开。动机：看板的“总盈亏 / 收益率”= 当前权益 − 初始资金，会把转入当成盈利（例：us-stock 初始 100，权益 731，显示 +730%；10-14 单日 +435、10-22 +191 基本是转入）。

## 账户范围与数据来源
账户 = 一个交易员（trader）。类型 `kind`：`live_futures`（实盘合约，如 mac-nofx）、`live_spot`（实盘美股 bStock 现货，如 us-stock）、`paper`（模拟盘，默认不计入合计，`include_paper=1` 才返回）。
每个账户每日对账用到：
1. 权益快照 `trader_equity_snapshots`（total_equity、balance、unrealized_pnl，约每周期一条）→ 每日开盘/收盘权益、浮动盈亏变化。
2. 真实交易盈亏：`trader_positions`（已平仓行，realized_pnl、fee、exit_time），期货优先用交易所收支流水中的 REALIZED_PNL。
3. 交易所收支流水（仅实盘；币安 U 本位合约 `/fapi/v1/income`，不带 incomeType 过滤取全部类型；只保留最近约 3 个月）：落库表 `account_income`，按类型分类：
   - 转账：`TRANSFER`、`INTERNAL_TRANSFER`、`CROSS_COLLATERAL_TRANSFER` 及其他含 TRANSFER 的类型；充值/提现（若接口可得）。
   - 交易：`REALIZED_PNL`；手续费：`COMMISSION`；资金费：`FUNDING_FEE`；其他收入：`COMMISSION_REBATE`、`API_REBATE`、`REFERRAL_KICKBACK`、`WELCOME_BONUS`、`CONTEST_REWARD` 等。
   - 现货（美股 bStock 账户）：尽力读取充值/提现/划转历史（如 go-binance 已有对应 service），权限不足或接口不可用则在 `data_quality` 里写明并回退到推断。
4. 推断：没有交易所流水的日子（早于 3 个月、现货、读取失败），用对账残差识别“疑似转账”。

## 每日对账口径（每账户、每个自然日，时区由 `tz` 指定，默认 Asia/Shanghai）
```
open_equity(d)  = d-1 日最后一条快照权益（首日用当日第一条）
close_equity(d) = d 日最后一条快照权益
delta_equity    = close - open
unrealized_change(d) = unrealized(close d) - unrealized(close d-1)        # 来自快照的 unrealized_pnl
trading_pnl(d)  = 当日已实现盈亏（毛，不含手续费）
fees(d)         = 当日手续费（正数表示支出）
funding(d)      = 当日资金费净额（收入为正）
other_income(d) = 当日其他收入（返佣、奖励等）
confirmed_transfer(d) = 当日交易所确认的转账净额（转入为正，转出为负）
explained = trading_pnl - fees + funding + other_income + unrealized_change
residual  = delta_equity - explained - confirmed_transfer
若 |residual| > max(2 USDT, 2% × open_equity) 且当日没有已确认转账覆盖它 → 记一条 inferred 转账，amount = residual，type = inferred_in/inferred_out；
否则 unexplained = residual（快照时点误差、缺失成交等小额噪声）。
```
累计口径：`net_transfers = Σ(confirmed + inferred 转账)`；`adjusted_pnl = current_equity - initial_balance - net_transfers`（剔除转账后的真实盈亏）；`adjusted_return_pct` 用 Modified Dietz：`adjusted_pnl / (initial_balance + Σ flow_i × (T - t_i)/T)`，T 为区间总时长；`raw_return_pct = current_equity / initial_balance - 1`（与看板现有口径一致，用于对比）。
合计（totals）只汇总实盘账户（`kind != paper`）；各币种统一按 USDT（USDC 等稳定币按 1:1，注明）。

## API
`GET /api/funds/overview?days=90&tz=Asia/Shanghai&include_paper=0`（需登录，仅返回当前用户的交易员；days 1~365，默认 90）。响应（金额均为 USDT，浮点；时间 ISO8601 UTC；日期 YYYY-MM-DD 按 tz）：
```json
{
  "generated_at": "2026-10-24T01:00:00Z",
  "tz": "Asia/Shanghai",
  "days": 90,
  "totals": { "...同 account 的金额字段，trader_id/name 省略..." },
  "accounts": [{
    "trader_id": "…", "name": "us-stock", "kind": "live_spot", "exchange": "Binance",
    "initial_balance": 100.08, "current_equity": 730.76, "unrealized_pnl": 0.0,
    "net_transfers": 630.0, "trading_pnl": 5.2, "fees": 0.9, "funding": 0.0, "other_income": 0.0, "unexplained": 0.4,
    "adjusted_pnl": 0.5, "adjusted_return_pct": 0.5, "raw_return_pct": 630.0,
    "first_date": "2026-10-13", "last_date": "2026-10-24",
    "sources": ["equity_snapshots", "trader_positions", "exchange_income"],
    "warnings": ["现货充值/划转历史不可用（权限不足），转账按对账残差推断"]
  }],
  "daily": [{
    "date": "2026-10-22", "trader_id": "…", "name": "us-stock",
    "open_equity": 540.3, "close_equity": 731.4, "delta_equity": 191.1,
    "trading_pnl": 0.0, "fees": 0.0, "funding": 0.0, "other_income": 0.0, "unrealized_change": 0.1,
    "net_transfer": 191.0, "transfer_source": "inferred", "unexplained": 0.0
  }],
  "daily_total": [ { "date": "…", 同 daily 字段（对所有实盘账户按日求和，trader_id/name 省略，transfer_source 取 mixed/exchange/inferred/none） } ],
  "flows": [{
    "id": "…", "time": "2026-10-22T…Z", "date": "2026-10-22", "trader_id": "…", "name": "us-stock",
    "amount": 191.0, "type": "transfer_in|transfer_out|deposit|withdraw|inferred_in|inferred_out",
    "source": "exchange|inferred", "confirmed": false, "note": "对账残差 191.0（阈值 10.8）"
  }]
}
```
`daily` 按 (date, trader_id) 全部返回（已按 date 降序、再按 name），`daily_total` 按日期降序。无数据的账户 `accounts[].warnings` 说明原因，不抛 500。
收支流水同步：首次请求回填 90 天，之后增量；请求触发同步（每个交易所账户最多每 15 分钟一次、加互斥锁、失败不影响响应，只在 warnings 里报告）。

## 前端 /funds
顶部导航「资金」入口；页面分区：
1. 总览卡片（实盘合计）：总权益、净转入（转入−转出）、真实交易盈亏（交易−手续费+资金费+其他）、剔除转账后收益率、手续费+资金费合计、未解释差额。
2. 账户表：每个账户一行，**原始收益率 vs 剔除转账收益率并排**（二者相差 >10 个百分点时高亮并提示“包含转账影响”），警告图标悬停显示 warnings。
3. 图表：每日盈亏堆叠柱（交易盈亏/手续费/资金费/浮盈变化，转账不计入，用单独标记）；权益曲线 vs “剔除转账后的权益”（权益 − 累计净转账）两条线；转账日期用竖线/标记。
4. 每日明细表（可按账户筛选、7/30/90 天切换、含合计行）：日期、收盘权益、权益变化、转账（带“交易所确认/推断”徽标）、交易盈亏、手续费、资金费、浮盈变化、未解释。
5. 转账列表：时间、账户、金额（转入绿/转出红）、类型、来源徽标（交易所确认 / 推断）、备注。
中英文（沿用各页的 language 属性），空数据/加载/错误态齐全；移动端宽度可用（表格横向滚动）。
