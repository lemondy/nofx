import { afterEach, expect, test, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import useSWR from 'swr'
import type { RiskStatus } from '../../types'
import { RiskStatusPanel } from './RiskStatusPanel'

vi.mock('swr', () => ({ default: vi.fn() }))
afterEach(cleanup)

function mockStatus(basis?: string, dailyCap = 7) {
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
