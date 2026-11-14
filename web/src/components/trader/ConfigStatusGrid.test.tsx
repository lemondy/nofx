import { afterEach, expect, test, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import type { Exchange } from '../../types'
import { ConfigStatusGrid } from './ConfigStatusGrid'

afterEach(() => {
  cleanup()
})

function makeProps(configuredExchanges: Exchange[]) {
  return {
    configuredModels: [],
    configuredExchanges,
    visibleExchangeAddresses: new Set<string>(),
    copiedId: null,
    language: 'zh' as const,
    isModelInUse: () => false,
    getModelUsageInfo: () => ({ runningCount: 0, totalCount: 0 }),
    isExchangeInUse: () => false,
    getExchangeUsageInfo: () => ({ runningCount: 0, totalCount: 0 }),
    onModelClick: vi.fn(),
    onModelDelete: vi.fn(),
    onExchangeClick: vi.fn(),
    onToggleExchangeAddress: vi.fn(),
    onCopyAddress: vi.fn(),
  }
}

test('splits live and paper exchanges into two collapsible groups', () => {
  const exchanges: Exchange[] = [
    {
      id: 'ex-live-1',
      exchange_type: 'binance',
      account_name: 'mac-nofx',
      name: 'Binance',
      type: 'cex',
      enabled: true,
    },
    {
      id: 'ex-paper-1',
      exchange_type: 'paper',
      account_name: 'paper-cand',
      name: 'Paper',
      type: 'paper',
      enabled: true,
    },
    {
      id: 'ex-paper-2',
      exchange_type: 'paper',
      account_name: 'paper-ctrl',
      name: 'Paper',
      type: 'paper',
      enabled: true,
    },
  ]

  render(<ConfigStatusGrid {...makeProps(exchanges)} />)

  const groupButtons = screen.getAllByRole('button', { expanded: false })
  expect(groupButtons).toHaveLength(2)
  expect(groupButtons[0]).toHaveTextContent('实盘')
  expect(groupButtons[0]).toHaveTextContent('1')
  expect(groupButtons[1]).toHaveTextContent('模拟盘')
  expect(groupButtons[1]).toHaveTextContent('2')

  expect(screen.queryByText('mac-nofx')).not.toBeInTheDocument()
  expect(screen.queryByText('paper-cand')).not.toBeInTheDocument()
  expect(screen.queryByText('paper-ctrl')).not.toBeInTheDocument()

  // Expand live group
  fireEvent.click(groupButtons[0])
  expect(groupButtons[0]).toHaveAttribute('aria-expanded', 'true')
  expect(groupButtons[1]).toHaveAttribute('aria-expanded', 'false')
  expect(screen.getByText('mac-nofx')).toBeInTheDocument()
  expect(screen.queryByText('paper-cand')).not.toBeInTheDocument()
  expect(screen.queryByText('paper-ctrl')).not.toBeInTheDocument()

  // Expand paper group
  fireEvent.click(groupButtons[1])
  expect(groupButtons[1]).toHaveAttribute('aria-expanded', 'true')
  expect(screen.getByText('paper-cand')).toBeInTheDocument()
  expect(screen.getByText('paper-ctrl')).toBeInTheDocument()

  // Collapse live group back
  fireEvent.click(groupButtons[0])
  expect(groupButtons[0]).toHaveAttribute('aria-expanded', 'false')
  expect(screen.queryByText('mac-nofx')).not.toBeInTheDocument()
  expect(screen.getByText('paper-cand')).toBeInTheDocument()
})

test('renders empty placeholder inside an exchange group with zero items when expanded', () => {
  const exchanges: Exchange[] = [
    {
      id: 'ex-paper-only',
      exchange_type: 'paper',
      account_name: 'paper-only',
      name: 'Paper',
      type: 'paper',
      enabled: true,
    },
  ]

  render(<ConfigStatusGrid {...makeProps(exchanges)} />)

  const groupButtons = screen.getAllByRole('button', { expanded: false })
  expect(groupButtons[0]).toHaveTextContent('0')
  expect(groupButtons[1]).toHaveTextContent('1')

  fireEvent.click(groupButtons[0])
  fireEvent.click(groupButtons[1])
  expect(screen.getByText('暂无实盘交易所')).toBeInTheDocument()
  expect(screen.getByText('paper-only')).toBeInTheDocument()
})
