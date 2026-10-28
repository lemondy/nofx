import { describe, expect, it } from 'vitest'
import { drawdownBasis, positionNetPnl, statsNetPnl, sumNetPnl } from './netPnl'

describe('positionNetPnl', () => {
  it('prefers backend net_pnl (includes funding)', () => {
    expect(positionNetPnl({ realized_pnl: 10, fee: 1, net_pnl: 7.5 })).toBe(7.5)
  })
  it('keeps an explicit zero net_pnl', () => {
    expect(positionNetPnl({ realized_pnl: 10, fee: 1, net_pnl: 0 })).toBe(0)
  })
  it('falls back to realized_pnl − fee when net_pnl is absent', () => {
    expect(positionNetPnl({ realized_pnl: 10, fee: 1 })).toBe(9)
    expect(
      positionNetPnl({ realized_pnl: 10, fee: 1, net_pnl: undefined })
    ).toBe(9)
  })
  it('treats missing numbers as zero', () => {
    expect(
      positionNetPnl({} as unknown as Parameters<typeof positionNetPnl>[0])
    ).toBe(0)
  })
})

describe('sumNetPnl', () => {
  it('mixes rows with and without net_pnl', () => {
    expect(
      sumNetPnl([
        { realized_pnl: 10, fee: 1, net_pnl: 8 },
        { realized_pnl: -5, fee: 0.5 },
      ])
    ).toBeCloseTo(2.5)
  })
  it('is 0 for an empty list', () => {
    expect(sumNetPnl([])).toBe(0)
  })
})

describe('statsNetPnl', () => {
  it('prefers total_net_pnl', () => {
    expect(statsNetPnl({ total_pnl: 100, total_net_pnl: 96 })).toBe(96)
  })
  it('falls back to total_pnl', () => {
    expect(statsNetPnl({ total_pnl: 100 })).toBe(100)
  })
})

describe('drawdownBasis', () => {
  it('passes known values through', () => {
    expect(drawdownBasis({ max_drawdown_basis: 'equity_twr' })).toBe(
      'equity_twr'
    )
    expect(drawdownBasis({ max_drawdown_basis: 'closed_trades' })).toBe(
      'closed_trades'
    )
    expect(drawdownBasis({ max_drawdown_basis: 'unavailable' })).toBe(
      'unavailable'
    )
  })
  it('is undefined when absent or unknown', () => {
    expect(drawdownBasis({})).toBeUndefined()
    expect(
      drawdownBasis({
        max_drawdown_basis: 'x' as unknown as 'equity_twr',
      })
    ).toBeUndefined()
  })
})
