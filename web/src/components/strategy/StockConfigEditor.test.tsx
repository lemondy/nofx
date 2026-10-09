import React, { useState } from 'react'
import { afterEach, expect, test, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import type { StockConfig } from '../../types'
import { StockConfigEditor, defaultStockConfig } from './StockConfigEditor'

const apiMock = vi.hoisted(() => ({ getUSStockSymbols: vi.fn() }))
vi.mock('../../lib/api', () => ({ api: apiMock }))

afterEach(() => {
  cleanup()
  apiMock.getUSStockSymbols.mockReset()
})

function Harness({ onChange }: { onChange: (c: StockConfig) => void }) {
  const [cfg, setCfg] = useState<StockConfig>(defaultStockConfig)
  return (
    <StockConfigEditor
      config={cfg}
      language="zh"
      onChange={(c) => {
        setCfg(c)
        onChange(c)
      }}
    />
  )
}

test('defaults: paper on, regular session on, no symbols, swing preset', () => {
  expect(defaultStockConfig.symbols).toEqual([])
  expect(defaultStockConfig.paper_trading).toBe(true)
  expect(defaultStockConfig.sessions.regular).toBe(true)
  expect(defaultStockConfig.preset).toBe('swing')
})

test('searchable multi-select shows "AAPL · AAPLBUSDT" and selects symbols', async () => {
  apiMock.getUSStockSymbols.mockResolvedValue([
    { symbol: 'AAPLBUSDT', underlying: 'AAPL', base_asset: 'AAPLB' },
    { symbol: 'NVDABUSDT', underlying: 'NVDA', base_asset: 'NVDAB' },
  ])
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)
  expect(await screen.findByText('AAPL · AAPLBUSDT')).toBeInTheDocument()

  fireEvent.change(screen.getByTestId('stock-symbol-search'), {
    target: { value: 'nvd' },
  })
  expect(screen.queryByText('AAPL · AAPLBUSDT')).not.toBeInTheDocument()
  fireEvent.click(screen.getByText('NVDA · NVDABUSDT'))
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ symbols: ['NVDABUSDT'] })
  )
})

test('falls back to free-text comma-separated input when the endpoint fails', async () => {
  apiMock.getUSStockSymbols.mockRejectedValue(new Error('404'))
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)
  const input = await screen.findByTestId('stock-symbol-manual')
  fireEvent.change(input, {
    target: { value: 'aaplbusdt, NVDABUSDT aaplbusdt' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ symbols: ['AAPLBUSDT', 'NVDABUSDT'] })
  )
})

test('sessions, preset and paper toggle update config; live mode warns', async () => {
  apiMock.getUSStockSymbols.mockResolvedValue([])
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)

  fireEvent.click(screen.getByTestId('stock-session-pre'))
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({
      sessions: { regular: true, pre_market: true, after_hours: false },
    })
  )
  fireEvent.click(screen.getByTestId('stock-preset-position'))
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ preset: 'position' })
  )
  // position preset changes the stop ATR placeholder to 2 / 4
  expect(screen.getByTestId('stock-stop_atr_min')).toHaveAttribute(
    'placeholder',
    expect.stringContaining('2')
  )

  expect(screen.queryByTestId('stock-live-warning')).not.toBeInTheDocument()
  fireEvent.click(screen.getByTestId('stock-paper-trading'))
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ paper_trading: false })
  )
  expect(screen.getByTestId('stock-live-warning')).toBeInTheDocument()
})

test('numeric field: empty means 0 (default), value is stored', async () => {
  apiMock.getUSStockSymbols.mockResolvedValue([])
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)
  const field = screen.getByTestId('stock-max_positions')
  expect(field).toHaveAttribute('placeholder', expect.stringContaining('5'))
  fireEvent.change(field, { target: { value: '3' } })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ max_positions: 3 })
  )
  fireEvent.change(field, { target: { value: '' } })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ max_positions: 0 })
  )
})
