import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { Badge, Change, PnL, formatSigned, signTone } from './index'

describe('formatSigned', () => {
  it('adds + for positive and U+2212 minus for negative', () => {
    expect(formatSigned(1.234, { suffix: '%' })).toBe('+1.23%')
    expect(formatSigned(-1.234, { suffix: '%' })).toBe('−1.23%')
  })

  it('does not sign zero (incl. values rounding to zero)', () => {
    expect(formatSigned(0, { suffix: '%' })).toBe('0.00%')
    expect(formatSigned(-0.001)).toBe('0.00')
  })

  it('puts sign before currency and groups thousands', () => {
    expect(formatSigned(1234.5, { prefix: '$' })).toBe('+$1,234.50')
    expect(formatSigned(-80, { prefix: '$' })).toBe('−$80.00')
  })

  it('signTone follows rounded value', () => {
    expect(signTone(0.004)).toBe('zero')
    expect(signTone(0.006)).toBe('up')
    expect(signTone(-2)).toBe('down')
  })
})

describe('Change / PnL', () => {
  it('colors by sign and uses .num', () => {
    render(
      <>
        <Change value={2.5} />
        <Change value={-2.5} />
        <Change value={0} />
        <PnL value={10} />
      </>
    )
    const up = screen.getByText('+2.50%')
    expect(up).toHaveClass('text-up', 'num')
    expect(up).toHaveAttribute('data-tone', 'up')
    expect(screen.getByText('−2.50%')).toHaveClass('text-down')
    expect(screen.getByText('0.00%')).toHaveClass('text-fg-2')
    expect(screen.getByText('+$10.00')).toHaveClass('text-up')
  })

  it('renders a dash for missing values', () => {
    render(<PnL value={null} />)
    expect(screen.getByText('—')).toHaveClass('text-fg-3')
  })
})

describe('Badge', () => {
  it.each([
    ['neutral', 'text-fg-2'],
    ['brand', 'text-brand'],
    ['up', 'text-up'],
    ['down', 'text-down'],
    ['warn', 'text-warn'],
    ['info', 'text-info'],
    ['ai', 'text-ai'],
  ] as const)('variant %s uses %s', (variant, cls) => {
    render(<Badge variant={variant}>{variant}</Badge>)
    expect(screen.getByText(variant)).toHaveClass(cls)
  })

  it('supports xs and sm sizes', () => {
    render(
      <>
        <Badge size="xs">xs</Badge>
        <Badge size="sm">sm</Badge>
      </>
    )
    expect(screen.getByText('xs')).toHaveClass('h-[18px]')
    expect(screen.getByText('sm')).toHaveClass('h-5')
  })
})
