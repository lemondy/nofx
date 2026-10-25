import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, within } from '@testing-library/react'
import { api } from '../../lib/api'
import type { NetTraffic } from '../../lib/api/system'
import { NetworkTrafficCard } from './NetworkTrafficCard'

vi.mock('../../lib/api', () => ({
  api: { getNetTraffic: vi.fn() },
}))

const traffic: NetTraffic = {
  since: '2026-10-25T00:00:00Z',
  since_unix: 1792886400,
  requests: 4,
  http_up: 128,
  http_down: 8192,
  ws_up: 0,
  ws_down: 0,
  hosts: [{ host: 'fapi.binance.com', requests: 4, up: 128, down: 8192 }],
}

describe('NetworkTrafficCard Top paths', () => {
  beforeEach(() => vi.clearAllMocks())
  afterEach(cleanup)

  it('renders host + path, counts, bytes and average download in Chinese', async () => {
    const path = '/fapi/v1/klines'
    vi.mocked(api.getNetTraffic).mockResolvedValue({
      ...traffic,
      paths: [{ ...traffic.hosts[0], path }],
    })
    render(<NetworkTrafficCard language="zh" />)
    const table = await screen.findByRole('table', { name: 'Top 路径' })
    expect(
      within(table)
        .getAllByRole('columnheader')
        .map((h) => h.textContent)
    ).toEqual(['主机+路径', '请求数', '上行', '下行', '平均每次下行'])
    const cells = within(table).getAllByRole('cell')
    expect(cells.map((cell) => cell.textContent)).toEqual([
      `fapi.binance.com${path}`,
      '4',
      '128 B',
      '8.0 KB',
      '2.0 KB',
    ])
    expect(within(table).getByTitle(`fapi.binance.com${path}`)).toHaveClass(
      'truncate'
    )
    expect(table.parentElement).toHaveClass('overflow-x-auto')
    expect(screen.getByText('主机')).toBeInTheDocument()
    expect(
      screen.getByText(/路径已归一化.*超出 200 个路径的并入 \(other\)/)
    ).toBeInTheDocument()
  })

  it('renders only the first 15 paths in English and handles zero requests', async () => {
    vi.mocked(api.getNetTraffic).mockResolvedValue({
      ...traffic,
      paths: Array.from({ length: 16 }, (_, i) => ({
        host: 'fapi.binance.com',
        path: `/endpoint${i}`,
        requests: 0,
        up: 0,
        down: 1024,
      })),
    })
    render(<NetworkTrafficCard language="en" />)
    const table = await screen.findByRole('table', { name: 'Top Paths' })
    expect(
      within(table).getByRole('columnheader', { name: 'Avg down / req' })
    ).toBeInTheDocument()
    expect(within(table).getAllByRole('row')).toHaveLength(16) // header + 15 paths
    expect(
      within(table).getByTitle('fapi.binance.com/endpoint14')
    ).toBeInTheDocument()
    expect(
      within(table).queryByTitle('fapi.binance.com/endpoint15')
    ).not.toBeInTheDocument()
    expect(
      within(table).getAllByRole('row')[1].lastElementChild
    ).toHaveTextContent('0 B')
  })

  it.each([undefined, []])(
    'omits the table when paths is %j',
    async (paths) => {
      vi.mocked(api.getNetTraffic).mockResolvedValue({ ...traffic, paths })
      render(<NetworkTrafficCard language="en" />)
      await screen.findByTitle('fapi.binance.com')
      expect(
        screen.queryByRole('table', { name: 'Top Paths' })
      ).not.toBeInTheDocument()
    }
  )
})
