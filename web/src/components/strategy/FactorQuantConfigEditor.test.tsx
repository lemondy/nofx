import React, { useState } from 'react'
import { afterEach, expect, test, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import type { FactorQuantConfig } from '../../types'
import {
  FactorQuantConfigEditor,
  defaultFactorQuantConfig,
} from './FactorQuantConfigEditor'

afterEach(cleanup)

function Harness({ onChange }: { onChange: (c: FactorQuantConfig) => void }) {
  const [cfg, setCfg] = useState<FactorQuantConfig>(defaultFactorQuantConfig)
  return (
    <FactorQuantConfigEditor
      config={cfg}
      language="zh"
      onChange={(c) => {
        setCfg(c)
        onChange(c)
      }}
    />
  )
}

test('defaults: crypto market, horizon 3, leverage 3, short stop 0.5', () => {
  expect(defaultFactorQuantConfig).toEqual({
    market: 'crypto',
    horizon: 3,
    leverage: 3,
    short_stop_pct: 0.5,
  })
})

test('renders the intro and the crypto numeric fields', () => {
  render(<Harness onChange={() => {}} />)
  expect(screen.getByText('程序化量化交易员')).toBeInTheDocument()
  expect(screen.getByText(/只能绑定模拟账户/)).toBeInTheDocument()
  for (const id of [
    'factor-horizon',
    'factor-capital_fraction',
    'factor-leverage',
    'factor-rebalance_band_pct',
    'factor-min_order_usdt',
    'factor-short_stop_pct',
    'factor-max_stale_days',
    'factor-targets_path',
  ]) {
    expect(screen.getByTestId(id)).toBeInTheDocument()
  }
  expect(screen.getByTestId('factor-horizon')).toHaveAttribute(
    'placeholder',
    expect.stringContaining('3')
  )
  // default max_stale_days = horizon + 2
  expect(screen.getByTestId('factor-max_stale_days')).toHaveAttribute(
    'placeholder',
    expect.stringContaining('5')
  )
})

test('switching market applies per-market defaults and toggles crypto-only fields', () => {
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)

  // → US stocks: horizon 0 (accept the file's), leverage/short stop hidden
  fireEvent.click(screen.getByTestId('factor-market-us'))
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ market: 'us', horizon: 0 })
  )
  expect(screen.queryByTestId('factor-leverage')).not.toBeInTheDocument()
  expect(screen.queryByTestId('factor-short_stop_pct')).not.toBeInTheDocument()
  // stale default = horizon 0 + 2
  expect(screen.getByTestId('factor-max_stale_days')).toHaveAttribute(
    'placeholder',
    expect.stringContaining('2')
  )

  // → back to crypto: defaults restored
  fireEvent.click(screen.getByTestId('factor-market-crypto'))
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({
      market: 'crypto',
      horizon: 3,
      leverage: 3,
      short_stop_pct: 0.5,
    })
  )
  expect(screen.getByTestId('factor-leverage')).toBeInTheDocument()
  expect(screen.getByTestId('factor-short_stop_pct')).toBeInTheDocument()
})

test('numeric and text fields emit updates', () => {
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)

  fireEvent.change(screen.getByTestId('factor-horizon'), {
    target: { value: '5' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ horizon: 5 })
  )
  // max_stale_days placeholder follows horizon + 2
  expect(screen.getByTestId('factor-max_stale_days')).toHaveAttribute(
    'placeholder',
    expect.stringContaining('7')
  )

  fireEvent.change(screen.getByTestId('factor-capital_fraction'), {
    target: { value: '0.8' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ capital_fraction: 0.8 })
  )

  fireEvent.change(screen.getByTestId('factor-leverage'), {
    target: { value: '5' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ leverage: 5 })
  )

  fireEvent.change(screen.getByTestId('factor-rebalance_band_pct'), {
    target: { value: '0.2' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ rebalance_band_pct: 0.2 })
  )

  fireEvent.change(screen.getByTestId('factor-min_order_usdt'), {
    target: { value: '20' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ min_order_usdt: 20 })
  )

  // 0 on the short stop = disabled
  fireEvent.change(screen.getByTestId('factor-short_stop_pct'), {
    target: { value: '0' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ short_stop_pct: 0 })
  )

  fireEvent.change(screen.getByTestId('factor-max_stale_days'), {
    target: { value: '4' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ max_stale_days: 4 })
  )

  // clearing a numeric field stores 0 = backend default
  fireEvent.change(screen.getByTestId('factor-min_order_usdt'), {
    target: { value: '' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ min_order_usdt: 0 })
  )

  // clearing the path override removes the key entirely
  fireEvent.change(screen.getByTestId('factor-targets_path'), {
    target: { value: '/tmp/qt.json' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ targets_path: '/tmp/qt.json' })
  )
  fireEvent.change(screen.getByTestId('factor-targets_path'), {
    target: { value: '' },
  })
  expect(onChange.mock.lastCall?.[0].targets_path).toBeUndefined()
})
