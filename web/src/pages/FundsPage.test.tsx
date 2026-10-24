import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import FundsPage from './FundsPage'
import type { FundsOverview } from '../lib/api/funds'

// recharts 的 ResponsiveContainer 在 jsdom 里没有尺寸，给固定尺寸替身
vi.mock('recharts', async () => {
  const actual = await vi.importActual<typeof import('recharts')>('recharts')
  return {
    ...actual,
    ResponsiveContainer: ({ children }: { children: React.ReactElement }) => (
      <div style={{ width: 600, height: 200 }}>{children}</div>
    ),
  }
})

const apiMock = vi.hoisted(() => ({
  getFundsOverview: vi.fn(),
}))
vi.mock('../lib/api', () => ({ api: apiMock }))

/** 构造 fixture（每次调用返回全新对象, 避免用例间共享引用） */
function buildOverview(opts: { includePaper?: boolean; flows?: boolean } = {}) {
  const withPaper = opts.includePaper ?? false
  const usWarning = '现货充值/划转历史不可用（权限不足），转账按对账残差推断'
  const macWarning =
    '收支流水同步失败（IP 白名单限制），最近 3 天转账按对账残差推断'

  const daily = [
    {
      date: '2026-10-24',
      trader_id: 't-us',
      name: 'us-stock',
      open_equity: 731.4,
      close_equity: 730.76,
      delta_equity: -0.64,
      trading_pnl: 0,
      fees: 0,
      funding: 0,
      other_income: 0,
      unrealized_change: -0.24,
      net_transfer: 0,
      transfer_source: 'none' as const,
      unexplained: -0.4,
    },
    {
      date: '2026-10-23',
      trader_id: 't-mac',
      name: 'mac-nofx',
      open_equity: 1005.0,
      close_equity: 1012.34,
      delta_equity: 7.34,
      trading_pnl: 48.3,
      fees: 8.1,
      funding: -1.86,
      other_income: 0,
      unrealized_change: -30.0,
      net_transfer: 0,
      transfer_source: 'none' as const,
      unexplained: -1.0,
    },
    {
      date: '2026-10-22',
      trader_id: 't-us',
      name: 'us-stock',
      open_equity: 540.3,
      close_equity: 731.4,
      delta_equity: 191.1,
      trading_pnl: 0,
      fees: 0,
      funding: 0,
      other_income: 0,
      unrealized_change: 0.1,
      net_transfer: 191.0,
      transfer_source: 'inferred' as const,
      unexplained: 0,
    },
    {
      date: '2026-10-20',
      trader_id: 't-mac',
      name: 'mac-nofx',
      open_equity: 1000.0,
      close_equity: 1005.0,
      delta_equity: 5.0,
      trading_pnl: 27.2,
      fees: 4.2,
      funding: -1.0,
      other_income: 1.32,
      unrealized_change: 31.68,
      net_transfer: -50.0,
      transfer_source: 'exchange' as const,
      unexplained: 0,
    },
    {
      date: '2026-10-16',
      trader_id: 't-us',
      name: 'us-stock',
      open_equity: 540.3,
      close_equity: 544.3,
      delta_equity: 4.0,
      trading_pnl: 0,
      fees: 0,
      funding: 0,
      other_income: 0,
      unrealized_change: 0,
      net_transfer: 4.0,
      transfer_source: 'inferred' as const,
      unexplained: 0,
    },
    {
      date: '2026-10-14',
      trader_id: 't-us',
      name: 'us-stock',
      open_equity: 104.9,
      close_equity: 540.3,
      delta_equity: 435.4,
      trading_pnl: 5.2,
      fees: 0.9,
      funding: 0,
      other_income: 0,
      unrealized_change: 0.1,
      net_transfer: 435.0,
      transfer_source: 'inferred' as const,
      unexplained: -0.6,
    },
  ]
  if (withPaper) {
    daily.push({
      date: '2026-10-23',
      trader_id: 't-paper',
      name: 'paper-alpaca',
      open_equity: 1000.0,
      close_equity: 1043.2,
      delta_equity: 43.2,
      trading_pnl: 43.2,
      fees: 0,
      funding: 0,
      other_income: 0,
      unrealized_change: 0,
      net_transfer: 0,
      transfer_source: 'none' as const,
      unexplained: 0,
    })
  }

  // daily_total: 只对实盘账户按日求和（不含 paper）
  const dailyTotal = [
    {
      date: '2026-10-24',
      open_equity: 731.4,
      close_equity: 730.76,
      delta_equity: -0.64,
      trading_pnl: 0,
      fees: 0,
      funding: 0,
      other_income: 0,
      unrealized_change: -0.24,
      net_transfer: 0,
      transfer_source: 'none' as const,
      unexplained: -0.4,
    },
    {
      date: '2026-10-23',
      open_equity: 1005.0,
      close_equity: 1012.34,
      delta_equity: 7.34,
      trading_pnl: 48.3,
      fees: 8.1,
      funding: -1.86,
      other_income: 0,
      unrealized_change: -30.0,
      net_transfer: 0,
      transfer_source: 'none' as const,
      unexplained: -1.0,
    },
    {
      date: '2026-10-22',
      open_equity: 540.3,
      close_equity: 731.4,
      delta_equity: 191.1,
      trading_pnl: 0,
      fees: 0,
      funding: 0,
      other_income: 0,
      unrealized_change: 0.1,
      net_transfer: 191.0,
      transfer_source: 'inferred' as const,
      unexplained: 0,
    },
    {
      date: '2026-10-20',
      open_equity: 1000.0,
      close_equity: 1005.0,
      delta_equity: 5.0,
      trading_pnl: 27.2,
      fees: 4.2,
      funding: -1.0,
      other_income: 1.32,
      unrealized_change: 31.68,
      net_transfer: -50.0,
      transfer_source: 'exchange' as const,
      unexplained: 0,
    },
    {
      date: '2026-10-16',
      open_equity: 540.3,
      close_equity: 544.3,
      delta_equity: 4.0,
      trading_pnl: 0,
      fees: 0,
      funding: 0,
      other_income: 0,
      unrealized_change: 0,
      net_transfer: 4.0,
      transfer_source: 'inferred' as const,
      unexplained: 0,
    },
    {
      date: '2026-10-14',
      open_equity: 104.9,
      close_equity: 540.3,
      delta_equity: 435.4,
      trading_pnl: 5.2,
      fees: 0.9,
      funding: 0,
      other_income: 0,
      unrealized_change: 0.1,
      net_transfer: 435.0,
      transfer_source: 'inferred' as const,
      unexplained: -0.6,
    },
  ]

  const accounts = [
    {
      trader_id: 't-us',
      name: 'us-stock',
      kind: 'live_spot' as const,
      exchange: 'Binance',
      initial_balance: 100.08,
      current_equity: 730.76,
      unrealized_pnl: 0,
      net_transfers: 630.0,
      trading_pnl: 5.2,
      fees: 0.9,
      funding: 0,
      other_income: 0,
      unexplained: 0.4,
      adjusted_pnl: 0.5,
      adjusted_return_pct: 0.5,
      raw_return_pct: 630.0,
      first_date: '2026-10-13',
      last_date: '2026-10-24',
      sources: ['equity_snapshots', 'trader_positions'],
      warnings: [usWarning],
    },
    {
      trader_id: 't-mac',
      name: 'mac-nofx',
      kind: 'live_futures' as const,
      exchange: 'Binance',
      initial_balance: 1000.0,
      current_equity: 1012.34,
      unrealized_pnl: 3.21,
      net_transfers: -50.0,
      trading_pnl: 75.5,
      fees: 12.3,
      funding: -2.86,
      other_income: 1.32,
      unexplained: 0.15,
      adjusted_pnl: 62.34,
      adjusted_return_pct: 6.23,
      raw_return_pct: 1.23,
      first_date: '2026-10-20',
      last_date: '2026-10-23',
      sources: ['equity_snapshots', 'trader_positions', 'exchange_income'],
      warnings: [macWarning],
    },
  ]
  if (withPaper) {
    accounts.push({
      trader_id: 't-paper',
      name: 'paper-alpaca',
      kind: 'paper' as const,
      exchange: 'Mock',
      initial_balance: 1000.0,
      current_equity: 1043.2,
      unrealized_pnl: 0,
      net_transfers: 0,
      trading_pnl: 43.2,
      fees: 0,
      funding: 0,
      other_income: 0,
      unexplained: 0,
      adjusted_pnl: 43.2,
      adjusted_return_pct: 4.32,
      raw_return_pct: 4.32,
      first_date: '2026-10-23',
      last_date: '2026-10-23',
      sources: ['equity_snapshots'],
      warnings: [],
    })
  }

  const flows =
    opts.flows === false
      ? []
      : [
          {
            id: 'flow-1',
            time: '2026-10-22T09:31:00Z',
            date: '2026-10-22',
            trader_id: 't-us',
            name: 'us-stock',
            amount: 191.0,
            type: 'inferred_in' as const,
            source: 'inferred' as const,
            confirmed: false,
            note: '对账残差 191.00（阈值 10.81）',
          },
          {
            id: 'flow-2',
            time: '2026-10-20T08:05:00Z',
            date: '2026-10-20',
            trader_id: 't-mac',
            name: 'mac-nofx',
            amount: -50.0,
            type: 'transfer_out' as const,
            source: 'exchange' as const,
            confirmed: true,
            note: '币安 TRANSFER：主账户 → 交易账户',
          },
          {
            id: 'flow-3',
            time: '2026-10-16T02:10:00Z',
            date: '2026-10-16',
            trader_id: 't-us',
            name: 'us-stock',
            amount: 4.0,
            type: 'inferred_in' as const,
            source: 'inferred' as const,
            confirmed: false,
            note: '对账残差 4.00（阈值 2.00）',
          },
          {
            id: 'flow-4',
            time: '2026-10-14T01:23:00Z',
            date: '2026-10-14',
            trader_id: 't-us',
            name: 'us-stock',
            amount: 435.0,
            type: 'inferred_in' as const,
            source: 'inferred' as const,
            confirmed: false,
            note: '对账残差 435.00（阈值 2.10）',
          },
        ]

  const overview: FundsOverview = {
    generated_at: '2026-10-24T01:00:00Z',
    tz: 'Asia/Shanghai',
    days: 90,
    totals: {
      initial_balance: 1100.08,
      current_equity: 1743.1,
      unrealized_pnl: 3.21,
      net_transfers: 580.0,
      trading_pnl: 80.7,
      fees: 13.2,
      funding: -2.86,
      other_income: 1.32,
      unexplained: 0.55,
      adjusted_pnl: 62.84,
      adjusted_return_pct: 5.71,
      raw_return_pct: 58.45,
    },
    accounts,
    daily,
    daily_total: dailyTotal,
    flows,
  }
  return overview
}

async function renderPage(language = 'zh') {
  render(<FundsPage language={language} />)
  // 等待数据渲染完成（账户表出现）
  await screen.findByRole('table', {
    name: language === 'zh' ? '账户表' : 'Accounts table',
  })
}

beforeEach(() => {
  apiMock.getFundsOverview.mockImplementation(
    (params: { includePaper?: boolean }) =>
      Promise.resolve(buildOverview({ includePaper: params?.includePaper }))
  )
})

afterEach(() => {
  vi.clearAllMocks()
})

describe('FundsPage', () => {
  it('requests the overview with default params (90d, Asia/Shanghai, no paper)', () => {
    render(<FundsPage language="zh" />)
    expect(apiMock.getFundsOverview).toHaveBeenCalledWith({
      days: 90,
      tz: 'Asia/Shanghai',
      includePaper: false,
    })
  })

  it('renders the totals stat strip from totals', async () => {
    await renderPage()
    // 总权益 1743.10; 真实交易盈亏 80.70−13.20−2.86+1.32 = 65.96
    // 剔除转账后收益率 5.71%; 净成本 手续费 13.20 − 资金费(−2.86) = 16.06
    expect(screen.getByText('1,743.10')).toBeInTheDocument()
    expect(screen.getByText('+65.96')).toBeInTheDocument()
    expect(screen.getByText('+5.71%')).toBeInTheDocument()
    expect(screen.getByText('16.06')).toBeInTheDocument()
    expect(screen.getByText('+0.55')).toBeInTheDocument()
    // 净转入 +580.00 出现在总览卡片与每日明细合计行
    expect(screen.getAllByText('+580.00').length).toBeGreaterThan(0)
  })

  it('shows raw vs adjusted returns side by side and flags >10pp distortion', async () => {
    await renderPage()
    const table = screen.getByRole('table', { name: '账户表' })
    // us-stock: 原始 +630.00% vs 剔除转账后 +0.50%, 相差 >10 个百分点 → 高亮
    const raw = within(table).getByText('+630.00%')
    expect(within(table).getByText('+0.50%')).toBeInTheDocument()
    expect(raw.closest('td')).toHaveClass('bg-warn-soft')
    expect(screen.getAllByText('包含转账影响')).toHaveLength(1)
    // mac-nofx: +1.23% vs +6.23%, 相差 5pp → 不提示
    const macRaw = within(table).getByText('+1.23%')
    expect(within(table).getByText('+6.23%')).toBeInTheDocument()
    expect(macRaw.closest('td')).not.toHaveClass('bg-warn-soft')
  })

  it('exposes account warnings through a hover tooltip', async () => {
    await renderPage()
    const usWarning = screen.getByText(
      '现货充值/划转历史不可用（权限不足），转账按对账残差推断'
    )
    expect(usWarning.closest('[role="tooltip"]')).toBeInTheDocument()
    expect(
      screen.getByText(
        '收支流水同步失败（IP 白名单限制），最近 3 天转账按对账残差推断'
      )
    ).toBeInTheDocument()
  })

  it('labels confirmed vs inferred transfers in the daily table and flow list', async () => {
    await renderPage()
    const daily = screen.getByRole('table', { name: '每日明细表' })
    expect(within(daily).getAllByText('交易所确认').length).toBeGreaterThan(0)
    expect(within(daily).getAllByText('推断').length).toBeGreaterThan(0)
    const flows = screen.getByRole('table', { name: '转账列表' })
    expect(within(flows).getAllByText('交易所确认').length).toBeGreaterThan(0)
    expect(within(flows).getAllByText('推断').length).toBeGreaterThan(0)
    expect(within(flows).getAllByText('推断转入').length).toBe(3)
    expect(within(flows).getByText('转出')).toBeInTheDocument()
  })

  it('colors transfer-in green and transfer-out red', async () => {
    await renderPage()
    const flows = screen.getByRole('table', { name: '转账列表' })
    // 转入 +435.00 → text-up; 转出 −50.00 → text-down（− 为 U+2212）
    const inflow = within(flows).getByText('+435.00')
    expect(inflow).toHaveClass('text-up')
    const outflow = within(flows).getByText('−50.00')
    expect(outflow).toHaveClass('text-down')
  })

  it('formats flow times in the response tz', async () => {
    await renderPage()
    // 2026-10-22T09:31:00Z → Asia/Shanghai (UTC+8) → 2026-10-22 17:31
    expect(screen.getByText('2026-10-22 17:31')).toBeInTheDocument()
  })

  it('sums the daily detail table into a totals row', async () => {
    await renderPage()
    const daily = screen.getByRole('table', { name: '每日明细表' })
    expect(within(daily).getByText('合计')).toBeInTheDocument()
    // 展示的是全部实盘账户的 daily_total: 最新收盘 730.76（明细行 + 合计行）
    expect(within(daily).getAllByText('730.76')).toHaveLength(2)
    expect(within(daily).getByText('+642.20')).toBeInTheDocument() // 权益变化合计
    expect(within(daily).getByText('+580.00')).toBeInTheDocument() // 转账合计
    expect(within(daily).getByText('+80.70')).toBeInTheDocument() // 交易盈亏合计
    expect(within(daily).getByText('13.20')).toBeInTheDocument() // 手续费合计
    expect(within(daily).getByText('−2.86')).toBeInTheDocument() // 资金费合计
    expect(within(daily).getByText('−2.00')).toBeInTheDocument() // 未解释合计
  })

  it('filters the daily detail table by account', async () => {
    await renderPage()
    fireEvent.change(screen.getByLabelText('账户筛选'), {
      target: { value: 't-us' },
    })
    const daily = screen.getByRole('table', { name: '每日明细表' })
    // 只剩 us-stock 的日子; mac 的 10-23 不再出现
    expect(within(daily).queryByText('2026-10-23')).toBeNull()
    expect(within(daily).getByText('2026-10-16')).toBeInTheDocument()
    // 合计行重算: 净转账 = 191 + 4 + 435 = +630.00
    expect(within(daily).getByText('+630.00')).toBeInTheDocument()
  })

  it('refetches with the right params when switching 7/30/90 days', async () => {
    await renderPage()
    expect(apiMock.getFundsOverview).toHaveBeenLastCalledWith({
      days: 90,
      tz: 'Asia/Shanghai',
      includePaper: false,
    })
    fireEvent.click(screen.getByRole('button', { name: '近 30 天' }))
    await waitFor(() =>
      expect(apiMock.getFundsOverview).toHaveBeenLastCalledWith({
        days: 30,
        tz: 'Asia/Shanghai',
        includePaper: false,
      })
    )
    fireEvent.click(screen.getByRole('button', { name: '近 7 天' }))
    await waitFor(() =>
      expect(apiMock.getFundsOverview).toHaveBeenLastCalledWith({
        days: 7,
        tz: 'Asia/Shanghai',
        includePaper: false,
      })
    )
  })

  it('refetches with include_paper when toggling 含模拟盘', async () => {
    await renderPage()
    fireEvent.click(screen.getByRole('button', { name: '含模拟盘' }))
    await waitFor(() =>
      expect(apiMock.getFundsOverview).toHaveBeenLastCalledWith({
        days: 90,
        tz: 'Asia/Shanghai',
        includePaper: true,
      })
    )
    // paper 账户出现（选项 + 表格行）, 且带「模拟盘」徽标
    await screen.findAllByText('paper-alpaca')
    expect(screen.getByText('模拟盘')).toBeInTheDocument()
  })

  it('switches charts between daily PnL and equity curve', async () => {
    await renderPage()
    expect(screen.getByLabelText('每日盈亏图')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('tab', { name: '权益曲线' }))
    expect(screen.getByLabelText('权益曲线图')).toBeInTheDocument()
    expect(screen.queryByLabelText('每日盈亏图')).toBeNull()
  })

  it('shows loading and error states', async () => {
    apiMock.getFundsOverview.mockImplementationOnce(
      () => new Promise(() => {}) // 一直 pending → 停在加载态
    )
    const { unmount } = render(<FundsPage language="zh" />)
    expect(screen.getByText('加载中...')).toBeInTheDocument()
    unmount()

    apiMock.getFundsOverview.mockImplementationOnce(() =>
      Promise.reject(new Error('boom'))
    )
    render(<FundsPage language="en" />)
    expect(await screen.findByText('Failed to load')).toBeInTheDocument()
    expect(screen.getByText('boom')).toBeInTheDocument()
    // 重试按钮存在
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument()
  })

  it('shows the empty state when there is no account data', async () => {
    apiMock.getFundsOverview.mockImplementationOnce(() =>
      Promise.resolve({
        ...buildOverview(),
        accounts: [],
        daily: [],
        daily_total: [],
        flows: [],
      })
    )
    render(<FundsPage language="zh" />)
    expect(await screen.findByText('暂无账户数据')).toBeInTheDocument()
  })

  it('shows the empty state for an empty transfer list', async () => {
    apiMock.getFundsOverview.mockImplementationOnce(() =>
      Promise.resolve(buildOverview({ flows: false }))
    )
    await renderPage()
    expect(screen.getByText('暂无转账记录')).toBeInTheDocument()
  })

  it('renders in English', async () => {
    await renderPage('en')
    expect(
      screen.getByRole('heading', { name: 'Funds Overview' })
    ).toBeInTheDocument()
    expect(screen.getByText('Total equity')).toBeInTheDocument()
    // 「Net transfers」同时出现在总览卡片与账户表表头
    expect(screen.getAllByText('Net transfers').length).toBeGreaterThan(1)
    expect(screen.getByText('Real trading PnL')).toBeInTheDocument()
    expect(screen.getAllByText('Exchange-confirmed').length).toBeGreaterThan(0)
    expect(screen.getAllByText('Inferred').length).toBeGreaterThan(0)
    expect(screen.queryByText('资金总览')).toBeNull()
    // 英文下 7 天切换控件
    fireEvent.click(screen.getByRole('button', { name: '7d' }))
    await waitFor(() =>
      expect(apiMock.getFundsOverview).toHaveBeenLastCalledWith({
        days: 7,
        tz: 'Asia/Shanghai',
        includePaper: false,
      })
    )
  })
})
