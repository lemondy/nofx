import { describe, expect, it } from 'vitest'
import { mergeKlines } from './AdvancedChart'

describe('mergeKlines', () => {
  const bar = (time: number, close: number) => ({ time, close })

  it('replaces the forming candle and appends new bars', () => {
    const history = [bar(1, 10), bar(2, 11), bar(3, 12)]
    const merged = mergeKlines(history, [bar(3, 13), bar(4, 14)])
    expect(merged).toEqual([bar(1, 10), bar(2, 11), bar(3, 13), bar(4, 14)])
  })

  it('keeps history when the tail is empty', () => {
    const history = [bar(1, 10)]
    expect(mergeKlines(history, [])).toBe(history)
  })

  it('handles a tail that overlaps several stored bars', () => {
    const merged = mergeKlines(
      [bar(1, 1), bar(2, 2), bar(3, 3)],
      [bar(2, 20), bar(3, 30)]
    )
    expect(merged).toEqual([bar(1, 1), bar(2, 20), bar(3, 30)])
  })
})
