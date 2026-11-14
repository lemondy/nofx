import { afterEach, expect, test, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import type { Exchange, TraderInfo } from '../../types'
import { TradersList } from './TradersList'

afterEach(() => {
  cleanup()
})

const exchanges: Exchange[] = [
  {
    id: 'ex-1',
    name: 'mac-nofx',
    exchange_type: 'binance',
    enabled: true,
  },
]

function makeProps(traders: TraderInfo[]) {
  return {
    traders,
    isLoading: false,
    allExchanges: exchanges,
    configuredModelsCount: 1,
    configuredExchangesCount: 1,
    visibleTraderAddresses: new Set<string>(),
    copiedId: null,
    language: 'zh' as const,
    onNavigate: vi.fn(),
    onEditTrader: vi.fn(),
    onToggleTrader: vi.fn(),
    onToggleCompetition: vi.fn(),
    onDeleteTrader: vi.fn(),
    onToggleTraderAddress: vi.fn(),
    onCopyAddress: vi.fn(),
  }
}

test('splits running and stopped traders into two collapsible groups', () => {
  const traders: TraderInfo[] = [
    {
      trader_id: 't-run-1',
      trader_name: '运行交易员A',
      ai_model: 'glm',
      exchange_id: 'ex-1',
      is_running: true,
    },
    {
      trader_id: 't-stop-1',
      trader_name: '停止交易员B',
      ai_model: 'glm',
      exchange_id: 'ex-1',
      is_running: false,
    },
    {
      trader_id: 't-run-2',
      trader_name: '运行交易员C',
      ai_model: 'deepseek',
      exchange_id: 'ex-1',
      is_running: true,
    },
  ]

  render(<TradersList {...makeProps(traders)} />)

  const groupButtons = screen.getAllByRole('button', { expanded: false })
  expect(groupButtons).toHaveLength(2)
  expect(groupButtons[0]).toHaveTextContent('运行中')
  expect(groupButtons[0]).toHaveTextContent('2')
  expect(groupButtons[1]).toHaveTextContent('已停止')
  expect(groupButtons[1]).toHaveTextContent('1')

  expect(screen.queryByText('运行交易员A')).not.toBeInTheDocument()
  expect(screen.queryByText('运行交易员C')).not.toBeInTheDocument()
  expect(screen.queryByText('停止交易员B')).not.toBeInTheDocument()

  // Expand running group
  fireEvent.click(groupButtons[0])
  expect(groupButtons[0]).toHaveAttribute('aria-expanded', 'true')
  expect(groupButtons[1]).toHaveAttribute('aria-expanded', 'false')
  expect(screen.getByText('运行交易员A')).toBeInTheDocument()
  expect(screen.getByText('运行交易员C')).toBeInTheDocument()
  expect(screen.queryByText('停止交易员B')).not.toBeInTheDocument()

  // Expand stopped group
  fireEvent.click(groupButtons[1])
  expect(groupButtons[1]).toHaveAttribute('aria-expanded', 'true')
  expect(screen.getByText('停止交易员B')).toBeInTheDocument()

  // Collapse running group back
  fireEvent.click(groupButtons[0])
  expect(groupButtons[0]).toHaveAttribute('aria-expanded', 'false')
  expect(screen.queryByText('运行交易员A')).not.toBeInTheDocument()
  expect(screen.queryByText('运行交易员C')).not.toBeInTheDocument()
  expect(screen.getByText('停止交易员B')).toBeInTheDocument()
})

test('renders empty placeholder inside a group with zero traders when expanded', () => {
  const traders: TraderInfo[] = [
    {
      trader_id: 't-run-only',
      trader_name: '仅运行交易员',
      ai_model: 'glm',
      exchange_id: 'ex-1',
      is_running: true,
    },
  ]

  render(<TradersList {...makeProps(traders)} />)

  const groupButtons = screen.getAllByRole('button', { expanded: false })
  expect(groupButtons[0]).toHaveTextContent('1')
  expect(groupButtons[1]).toHaveTextContent('0')

  fireEvent.click(groupButtons[0])
  fireEvent.click(groupButtons[1])
  expect(screen.getByText('仅运行交易员')).toBeInTheDocument()
  expect(screen.getByText('暂无已停止的交易员')).toBeInTheDocument()
})
