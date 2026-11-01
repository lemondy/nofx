import { describe, expect, it } from 'vitest'
import { parseServerTime, parseServerTimeOrNull } from './serverTime'

describe('parseServerTime', () => {
  it('parses legacy "YYYY-MM-DD HH:mm:ss" as UTC (no timezone suffix)', () => {
    // 后端旧格式：UTC 存储、无时区输出。必须按 UTC 解析而不是本地时区。
    const d = parseServerTime('2026-10-28 08:39:00')
    expect(d.toISOString()).toBe('2026-10-28T08:39:00.000Z')
  })

  it('parses RFC3339 with Z suffix', () => {
    const d = parseServerTime('2026-10-28T08:39:00Z')
    expect(d.toISOString()).toBe('2026-10-28T08:39:00.000Z')
  })

  it('parses RFC3339 with numeric offset', () => {
    const d = parseServerTime('2026-10-28T16:39:00+08:00')
    expect(d.toISOString()).toBe('2026-10-28T08:39:00.000Z')
  })

  it('parses RFC3339 without zone suffix as UTC', () => {
    const d = parseServerTime('2026-10-28T08:39:00')
    expect(d.toISOString()).toBe('2026-10-28T08:39:00.000Z')
  })

  it('keeps both formats representing the same instant equal', () => {
    const legacy = parseServerTime('2026-10-28 08:39:00')
    const rfc3339 = parseServerTime('2026-10-28T08:39:00Z')
    expect(legacy.getTime()).toBe(rfc3339.getTime())
  })

  it('passes through Date and unix-ms inputs', () => {
    const now = new Date('2026-10-28T08:39:00Z')
    expect(parseServerTime(now).getTime()).toBe(now.getTime())
    expect(parseServerTime(now.getTime()).getTime()).toBe(now.getTime())
  })

  it('timezone is parsed independently of the browser local zone', () => {
    // 本地时区无论是 UTC+8 还是 UTC-5，同一字符串解析出的 UTC 时刻一致
    const d = parseServerTime('2000-01-01 00:00:00')
    expect(d.getTime()).toBe(Date.UTC(2000, 0, 1, 0, 0, 0))
  })
})

describe('parseServerTimeOrNull', () => {
  it('returns null for invalid input', () => {
    expect(parseServerTimeOrNull('not-a-date')).toBeNull()
  })

  it('returns a Date for valid input', () => {
    const d = parseServerTimeOrNull('2026-10-28 08:39:00')
    expect(d).not.toBeNull()
    expect(d!.toISOString()).toBe('2026-10-28T08:39:00.000Z')
  })
})
