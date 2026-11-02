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

test('defaults: crypto market, horizon 3, leverage 3, both stops 0.2', () => {
  expect(defaultFactorQuantConfig).toEqual({
    market: 'crypto',
    horizon: 3,
    leverage: 3,
    long_stop_pct: 0.2,
    short_stop_pct: 0.2,
    drawdown_warn_pct: 0.15,
    max_drawdown_pct: 0.25,
    daily_loss_pct: 0.07,
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
    'factor-long_stop_pct',
    'factor-short_stop_pct',
    'factor-drawdown_warn_pct',
    'factor-max_drawdown_pct',
    'factor-daily_loss_pct',
    'factor-kill-reset',
    'factor-max_stale_days',
    'factor-flatten_after_days',
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

  // → US stocks: horizon 0 (accept the file's), leverage/both stops hidden
  fireEvent.click(screen.getByTestId('factor-market-us'))
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ market: 'us', horizon: 0 })
  )
  expect(screen.queryByTestId('factor-leverage')).not.toBeInTheDocument()
  expect(screen.queryByTestId('factor-long_stop_pct')).not.toBeInTheDocument()
  expect(screen.queryByTestId('factor-short_stop_pct')).not.toBeInTheDocument()
  // the US-only cost inputs appear
  expect(screen.getByTestId('factor-us_fee_bps')).toHaveAttribute(
    'placeholder',
    expect.stringContaining('10')
  )
  expect(screen.getByTestId('factor-us_slippage_bps')).toHaveAttribute(
    'placeholder',
    expect.stringContaining('3')
  )
  // stale default = horizon 0 + 2
  expect(screen.getByTestId('factor-max_stale_days')).toHaveAttribute(
    'placeholder',
    expect.stringContaining('2')
  )

  expect(screen.getByTestId('factor-flatten_after_days')).toHaveAttribute(
    'placeholder',
    '默认 5'
  )

  // → back to crypto: defaults restored
  fireEvent.click(screen.getByTestId('factor-market-crypto'))
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({
      market: 'crypto',
      horizon: 3,
      leverage: 3,
      long_stop_pct: 0.2,
      short_stop_pct: 0.2,
    })
  )
  expect(screen.getByTestId('factor-leverage')).toBeInTheDocument()
  expect(screen.getByTestId('factor-short_stop_pct')).toBeInTheDocument()
  expect(screen.queryByTestId('factor-us_fee_bps')).not.toBeInTheDocument()
  expect(screen.queryByTestId('factor-us_slippage_bps')).not.toBeInTheDocument()
})

test('US cost inputs: 0 is a real value, empty clears to null (backend default)', () => {
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)
  fireEvent.click(screen.getByTestId('factor-market-us'))

  fireEvent.change(screen.getByTestId('factor-us_fee_bps'), {
    target: { value: '0' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ us_fee_bps: 0 })
  )
  fireEvent.change(screen.getByTestId('factor-us_slippage_bps'), {
    target: { value: '7.5' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ us_slippage_bps: 7.5 })
  )
  fireEvent.change(screen.getByTestId('factor-us_fee_bps'), {
    target: { value: '' },
  })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ us_fee_bps: null })
  )
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

test('flatten field follows horizon and stale defaults, and accepts explicit days, disabled and default', () => {
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)
  const input = screen.getByTestId('factor-flatten_after_days')
  expect(screen.getByText('目标过期自动清仓（天）')).toBeInTheDocument()
  expect(
    screen.getByText(/0=默认（horizon\+max_stale_days），-1=关闭/)
  ).toBeInTheDocument()
  expect(input).toHaveAttribute('placeholder', '默认 8')
  expect(input).toHaveAttribute('min', '-1')
  expect(input).toHaveAttribute('max', '60')
  expect(input).toHaveAttribute('step', '1')
  fireEvent.change(screen.getByTestId('factor-horizon'), {
    target: { value: '5' },
  })
  expect(input).toHaveAttribute('placeholder', '默认 12')
  fireEvent.change(screen.getByTestId('factor-max_stale_days'), {
    target: { value: '10' },
  })
  expect(input).toHaveAttribute('placeholder', '默认 15')
  for (const value of ['1', '60', '-1']) {
    fireEvent.change(input, { target: { value } })
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ flatten_after_days: Number(value) })
    )
    expect(input).toHaveValue(Number(value))
  }
  fireEvent.change(input, { target: { value: '' } })
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ flatten_after_days: 0 })
  )
})

test('flatten field renders English and respects disabled', () => {
  const onChange = vi.fn()
  render(
    <FactorQuantConfigEditor
      config={{ market: 'crypto', horizon: 0, flatten_after_days: -1 }}
      language="en"
      disabled
      onChange={onChange}
    />
  )
  expect(screen.getByText('Flatten expired targets (days)')).toBeInTheDocument()
  expect(screen.getByText(/-1=disabled/)).toBeInTheDocument()
  const input = screen.getByTestId('factor-flatten_after_days')
  expect(input).toBeDisabled()
  expect(input).toHaveValue(-1)
  expect(input).toHaveAttribute('placeholder', 'Default 8')
  fireEvent.change(input, { target: { value: '12' } })
  expect(onChange).not.toHaveBeenCalled()
})

test('both stop inputs preserve zero, show 20% defaults and restore defaults when cleared', () => {
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)
  for (const key of ['long_stop_pct', 'short_stop_pct'] as const) {
    const input = screen.getByTestId(`factor-${key}`)
    expect(input).toHaveValue(0.2)
    expect(input).toHaveAttribute('placeholder', '默认 0.2')
    fireEvent.change(input, { target: { value: '0' } })
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ [key]: 0 })
    )
    expect(input).toHaveValue(0)
    fireEvent.change(input, { target: { value: '' } })
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ [key]: 0.2 })
    )
    expect(input).toHaveValue(0.2)
    fireEvent.change(input, { target: { value: '0.15' } })
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ [key]: 0.15 })
    )
  }
  expect(screen.getAllByText(/默认 0.2（20%）/)).toHaveLength(2)
})

test('stop inputs show English defaults and respect disabled', () => {
  const onChange = vi.fn()
  render(
    <FactorQuantConfigEditor
      config={{ market: 'crypto' }}
      language="en"
      disabled
      onChange={onChange}
    />
  )
  expect(screen.getAllByText(/default 0.2 \(20%\)/)).toHaveLength(2)
  for (const key of ['long_stop_pct', 'short_stop_pct']) {
    const input = screen.getByTestId(`factor-${key}`)
    expect(input).toBeDisabled()
    expect(input).toHaveAttribute('placeholder', 'Default 0.2')
    fireEvent.change(input, { target: { value: '0' } })
  }
  expect(onChange).not.toHaveBeenCalled()
})

test('portfolio risk percentages show defaults in both markets and preserve zero', () => {
  const onChange = vi.fn()
  render(<Harness onChange={onChange} />)
  for (const market of ['crypto', 'us'] as const) {
    fireEvent.click(screen.getByTestId(`factor-market-${market}`))
    for (const [key, defaultPct] of [
      ['drawdown_warn_pct', 15],
      ['max_drawdown_pct', 25],
      ['daily_loss_pct', 7],
    ] as const) {
      const input = screen.getByTestId(`factor-${key}`)
      expect(input).toHaveValue(defaultPct)
      expect(input).toHaveAttribute('placeholder', `默认 ${defaultPct}%`)
      fireEvent.change(input, { target: { value: '0' } })
      expect(input).toHaveValue(0)
      expect(onChange).toHaveBeenLastCalledWith(
        expect.objectContaining({ [key]: 0 })
      )
      fireEvent.change(input, { target: { value: '12.5' } })
      expect(onChange).toHaveBeenLastCalledWith(
        expect.objectContaining({ [key]: 0.125 })
      )
      fireEvent.change(input, { target: { value: '' } })
      expect(input).toHaveValue(defaultPct)
      expect(onChange).toHaveBeenLastCalledWith(
        expect.objectContaining({ [key]: defaultPct / 100 })
      )
    }
  }
})

test('reset emits a new ISO ID and explains that saving is required', () => {
  const onChange = vi.fn()
  const now = new Date('2026-11-02T03:04:05.000Z')
  const clock = vi
    .spyOn(Date.prototype, 'toISOString')
    .mockReturnValue(now.toISOString())
  try {
    render(<Harness onChange={onChange} />)
    expect(screen.getByText(/保存后生效/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '复位回撤熔断' }))
    expect(onChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ kill_reset_id: '2026-11-02T03:04:05.000Z' })
    )
  } finally {
    clock.mockRestore()
  }
})

test('portfolio risk fields show English defaults and respect disabled', () => {
  const onChange = vi.fn()
  render(
    <FactorQuantConfigEditor
      config={{ market: 'us' }}
      language="en"
      disabled
      onChange={onChange}
    />
  )
  for (const [key, pct] of [
    ['drawdown_warn_pct', 15],
    ['max_drawdown_pct', 25],
    ['daily_loss_pct', 7],
  ] as const) {
    const input = screen.getByTestId(`factor-${key}`)
    expect(input).toBeDisabled()
    expect(input).toHaveValue(pct)
    expect(input).toHaveAttribute('placeholder', `Default ${pct}%`)
    fireEvent.change(input, { target: { value: '0' } })
  }
  const reset = screen.getByRole('button', {
    name: 'Reset drawdown kill switch',
  })
  expect(reset).toBeDisabled()
  fireEvent.click(reset)
  expect(onChange).not.toHaveBeenCalled()
})
