import { describe, expect, it } from 'vitest'
import { realizedR } from './PositionHistory'
import type { HistoricalPosition } from '../../types'

const pos = (o: Partial<HistoricalPosition>) => o as HistoricalPosition

describe('realizedR', () => {
  it('long and short R against the opening stop', () => {
    expect(
      realizedR(
        pos({
          side: 'LONG',
          entry_price: 100,
          exit_price: 104,
          initial_stop_loss: 98,
        })
      )
    ).toBeCloseTo(2)
    expect(
      realizedR(
        pos({
          side: 'SHORT',
          entry_price: 100,
          exit_price: 101,
          initial_stop_loss: 102,
        })
      )
    ).toBeCloseTo(-0.5)
  })
  it('null without a usable planned stop', () => {
    expect(
      realizedR(pos({ side: 'LONG', entry_price: 100, exit_price: 104 }))
    ).toBeNull()
    expect(
      realizedR(
        pos({
          side: 'LONG',
          entry_price: 100,
          exit_price: 104,
          initial_stop_loss: 1e-9,
        })
      )
    ).toBeNull()
  })
})
