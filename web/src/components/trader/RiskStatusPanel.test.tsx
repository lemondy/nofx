import { afterEach, expect, test, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import useSWR from 'swr'
import type { RiskStatus } from '../../types'
import { RiskStatusPanel } from './RiskStatusPanel'

vi.mock('swr', () => ({ default: vi.fn() }))
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

function mockStatus(
  basis?: string,
  dailyCap = 7,
  overrides: Partial<RiskStatus> = {}
) {
  const data: RiskStatus = {
    generated_at: '2026-11-02T00:00:00Z',
    equity: 10000,
    day_start_equity: 10000,
    daily_loss_pct: 0,
    daily_loss_cap_pct: dailyCap,
    daily_halted: false,
    initial_balance: 10000,
    account_drawdown_pct: 20,
    account_drawdown_cap_pct: 25,
    account_drawdown_basis: basis,
    account_breaker_active: false,
    reduce_only: false,
    loss_streak_enabled: false,
    loss_streak_max: 0,
    loss_streak_bans: [],
    pending_entries: [],
    positions: [],
    ...overrides,
  }
  vi.mocked(useSWR).mockReturnValue({
    data,
    error: undefined,
    isLoading: false,
    isValidating: false,
    mutate: vi.fn(),
  })
}

test.each([
  ['zh', '账户回撤（相对权益峰值）'],
  ['en', 'Account drawdown (from peak)'],
  ['id', 'Drawdown akun (dari puncak)'],
] as const)('peak drawdown meter is labelled in %s', (language, label) => {
  mockStatus('peak')
  render(<RiskStatusPanel traderId="fq" language={language} />)
  expect(screen.getByText(label)).toBeInTheDocument()
})

test('existing paths retain their drawdown label without a peak basis', () => {
  mockStatus()
  render(<RiskStatusPanel traderId="ai" language="en" />)
  expect(screen.getByText('Account drawdown')).toBeInTheDocument()
  expect(screen.queryByText(/from peak/)).not.toBeInTheDocument()
})

test('float caps render without binary rounding noise', () => {
  mockStatus('peak', 0.07 * 100)
  render(<RiskStatusPanel traderId="fq" language="en" />)
  expect(screen.getByText(/0\.00% \/ 7% /)).toBeInTheDocument()
  expect(screen.queryByText(/7\.0000/)).not.toBeInTheDocument()
})

function mockFactorQuant(ageSeconds = 0, code = 'too_soon') {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-03T00:00:00Z'))
  const tick = {
    at: new Date(Date.now() - ageSeconds * 1000).toISOString(),
    code,
    message: '目标日期未到调仓期',
    equity: 10000.125,
  }
  const factorQuant: NonNullable<RiskStatus['factor_quant']> = {
    tick_interval_sec: 300,
    last_tick: tick,
    recent_ticks: [tick],
    targets_as_of: '2026-11-02',
    targets_status: 'ok',
    targets_rule: 'ls_invvol_protected_v1',
    horizon: 3,
    last_rebalance_as_of: '2026-10-31',
    last_rebalance_at: '2026-11-01T00:00:00Z',
    next_eligible_as_of: '2026-11-03',
  }
  mockStatus('peak', 7, { factor_quant: factorQuant })
  return factorQuant
}

test('factor quant shows a fresh tick, metadata and recent equity', () => {
  mockFactorQuant(120)
  render(<RiskStatusPanel traderId="fq" language="zh" />)
  expect(
    screen.getByRole('heading', { name: '量化运行状态' })
  ).toBeInTheDocument()
  expect(screen.getByText('运行中')).toHaveClass('text-up')
  expect(screen.getByText(/2 分钟前/)).toBeInTheDocument()
  expect(
    screen.getByText('2026-11-02 · ok · ls_invvol_protected_v1')
  ).toBeInTheDocument()
  expect(screen.getByText('目标 as_of ≥ 2026-11-03')).toBeInTheDocument()
  expect(screen.getByText('3 天')).toBeInTheDocument()
  expect(screen.getByText('10000.13')).toBeInTheDocument()
  expect(screen.getByTitle('目标日期未到调仓期')).toHaveClass('truncate')
  expect(screen.queryByText('重试')).not.toBeInTheDocument()
})

test.each([
  [600, '运行中', 'text-up'],
  [601, '可能已停止', 'text-down'],
])('factor quant liveness at %s seconds', (age, badge, tone) => {
  mockFactorQuant(age)
  render(<RiskStatusPanel traderId="fq" language="zh" />)
  expect(screen.getByText(badge)).toHaveClass(tone)
})

test.each([
  ['zh', '未到调仓期'],
  ['en', 'Not yet due'],
  ['id', 'Belum waktunya rebalance'],
] as const)(
  'factor quant result code is translated in %s',
  (language, label) => {
    mockFactorQuant()
    render(<RiskStatusPanel traderId="fq" language={language} />)
    expect(screen.getByText(label)).toBeInTheDocument()
    expect(
      screen.getByText(`${label} · 目标日期未到调仓期`)
    ).toBeInTheDocument()
  }
)

test('unknown factor quant codes retain their raw value', () => {
  mockFactorQuant(0, 'future_code')
  render(<RiskStatusPanel traderId="fq" language="zh" />)
  expect(screen.getByText('future_code')).toBeInTheDocument()
})

test('factor quant waits for its first check and shows retry only when active', () => {
  const status = mockFactorQuant()
  mockStatus('peak', 7, {
    factor_quant: {
      ...status,
      last_tick: undefined,
      recent_ticks: [],
      next_eligible_as_of: undefined,
      retry_as_of: '2026-11-02',
      retry_attempts: 2,
    },
  })
  render(<RiskStatusPanel traderId="fq" language="zh" />)
  expect(screen.getByText('等待首次检查')).toHaveClass('text-fg-3')
  expect(screen.getByText('首次调仓')).toBeInTheDocument()
  expect(screen.getByText('重试')).toBeInTheDocument()
  expect(screen.getByText('2026-11-02 · 2')).toBeInTheDocument()
})

test('LLM traders do not show the factor quant section', () => {
  mockStatus()
  render(<RiskStatusPanel traderId="ai" language="zh" />)
  expect(
    screen.queryByRole('heading', { name: '量化运行状态' })
  ).not.toBeInTheDocument()
})
