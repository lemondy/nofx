import { afterEach, describe, expect, it } from 'vitest'
import { getCandleChartTheme } from './candleTheme'
import { getChartTheme } from '../../lib/chartTheme'

describe('getCandleChartTheme', () => {
  afterEach(() => {
    document.documentElement.style.removeProperty('--surface-2')
  })

  it('takes the crosshair label background from the surface-2 token', () => {
    document.documentElement.style.setProperty('--surface-2', '#f3f4f6')
    expect(getCandleChartTheme().crosshairLabel).toBe('#f3f4f6')
  })

  it('falls back to the dark surface-2 colour when the token is missing', () => {
    expect(getCandleChartTheme().crosshairLabel).toBe('#1c2127')
  })

  it('keeps every other colour from getChartTheme', () => {
    const base = getChartTheme()
    const candle = getCandleChartTheme()
    expect(candle).toEqual({ ...base, crosshairLabel: candle.crosshairLabel })
  })
})
