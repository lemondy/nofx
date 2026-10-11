import { describe, expect, it } from 'vitest'
import { gateCodeLabel } from './gateCodeLabels'

// 2026-10-10 per-candidate block reasons
describe('gateCodeLabel', () => {
  it('keeps numeric suffixes visible (zh)', () => {
    expect(gateCodeLabel('RR_MAX_0.24', 'zh').label).toBe('盈亏比 0.24')
    expect(gateCodeLabel('VENDOR_DIVERGENCE_-1.22', 'zh').label).toBe(
      '行情源偏离 -1.22%'
    )
    expect(gateCodeLabel('EMA20_STRETCH_58.4_GT_10', 'zh').label).toBe(
      '追高 58.4%'
    )
    expect(gateCodeLabel('RR_MAX_0.24', 'zh').raw).toBe('RR_MAX_0.24')
  })
  it('translates plain and en codes', () => {
    expect(gateCodeLabel('BTC_4H_DOWNTREND', 'zh').label).toBe('BTC 4H 下跌')
    expect(gateCodeLabel('MICRO_TREND_NOT_SHORT', 'en').label).toBe(
      'micro-trend not short'
    )
  })
  it('renders unknown codes raw', () => {
    expect(gateCodeLabel('SOMETHING_NEW_3', 'zh').label).toBe('SOMETHING_NEW_3')
  })
})
