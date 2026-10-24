import { API_BASE, httpClient } from './helpers'

/** 账户类型: live_futures 实盘合约 / live_spot 实盘现货 / paper 模拟盘 */
export type FundsAccountKind = 'live_futures' | 'live_spot' | 'paper'

/** 转账类型: 交易所确认的转账/充值/提现 与 对账推断的转入/转出 */
export type FundsTransferType =
  | 'transfer_in'
  | 'transfer_out'
  | 'deposit'
  | 'withdraw'
  | 'inferred_in'
  | 'inferred_out'

/** 转账来源 */
export type FundsTransferSource = 'exchange' | 'inferred'

/** daily/daily_total 的转账来源: none=当日无转账 */
export type FundsDailyTransferSource =
  | 'exchange'
  | 'inferred'
  | 'mixed'
  | 'none'

/** 账户级金额字段（USDT）; totals 复用同一组字段 */
export interface FundsAccountAmounts {
  initial_balance: number
  current_equity: number
  unrealized_pnl: number
  /** 转入−转出（交易所确认 + 推断） */
  net_transfers: number
  /** 已实现交易盈亏（毛，不含手续费） */
  trading_pnl: number
  /** 手续费（正数表示支出） */
  fees: number
  /** 资金费净额（收入为正） */
  funding: number
  /** 返佣/奖励等其他收入 */
  other_income: number
  /** 对账残差中的小额噪声 */
  unexplained: number
  /** 剔除转账后的真实盈亏 = current_equity − initial_balance − net_transfers */
  adjusted_pnl: number
  /** 剔除转账后收益率（Modified Dietz，百分比） */
  adjusted_return_pct: number
  /** 原始收益率 = current_equity / initial_balance − 1（与看板口径一致，百分比） */
  raw_return_pct: number
}

export interface FundsAccount extends FundsAccountAmounts {
  trader_id: string
  name: string
  kind: FundsAccountKind
  exchange: string
  first_date: string
  last_date: string
  sources: string[]
  warnings: string[]
}

/** 实盘合计（不含 paper; 稳定币按 1:1 折算 USDT） */
export type FundsTotals = FundsAccountAmounts

/** 每账户、每个自然日（按 tz）的对账行 */
export interface FundsDaily {
  date: string
  trader_id: string
  name: string
  open_equity: number
  close_equity: number
  delta_equity: number
  trading_pnl: number
  fees: number
  funding: number
  other_income: number
  unrealized_change: number
  /** 当日转账净额（转入为正） */
  net_transfer: number
  transfer_source: FundsDailyTransferSource
  unexplained: number
}

/** 所有实盘账户按日求和（trader_id/name 省略） */
export interface FundsDailyTotal {
  date: string
  open_equity: number
  close_equity: number
  delta_equity: number
  trading_pnl: number
  fees: number
  funding: number
  other_income: number
  unrealized_change: number
  net_transfer: number
  transfer_source: FundsDailyTransferSource
  unexplained: number
}

/** 转账（充值/提现/内部划转/推断）记录 */
export interface FundsFlow {
  id: string
  /** ISO8601 UTC */
  time: string
  /** 按 tz 的自然日 YYYY-MM-DD */
  date: string
  trader_id: string
  name: string
  /** 转入为正、转出为负 */
  amount: number
  type: FundsTransferType
  source: FundsTransferSource
  confirmed: boolean
  note: string
}

export interface FundsOverview {
  generated_at: string
  tz: string
  days: number
  totals: FundsTotals
  accounts: FundsAccount[]
  /** 按 (date, trader_id) 返回，date 降序、再按 name */
  daily: FundsDaily[]
  /** 按日期降序 */
  daily_total: FundsDailyTotal[]
  flows: FundsFlow[]
}

export interface FundsOverviewParams {
  /** 1~365，默认 90 */
  days?: number
  /** IANA 时区，默认 Asia/Shanghai */
  tz?: string
  /** 默认 false; paper 账户默认不计入合计，仅 include_paper=1 时返回 */
  includePaper?: boolean
}

export const fundsApi = {
  async getFundsOverview(
    params: FundsOverviewParams = {}
  ): Promise<FundsOverview> {
    const qs = new URLSearchParams()
    qs.set('days', String(params.days ?? 90))
    qs.set('tz', params.tz ?? 'Asia/Shanghai')
    qs.set('include_paper', params.includePaper ? '1' : '0')
    // silent: 页面自己渲染错误态，不要全局 toast
    const result = await httpClient.request<FundsOverview>(
      `${API_BASE}/funds/overview?${qs}`,
      { silent: true }
    )
    if (!result.success) {
      throw new Error(result.message || 'Failed to fetch funds overview')
    }
    return result.data!
  },
}
