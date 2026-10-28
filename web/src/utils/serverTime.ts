/**
 * 服务端时间戳解析
 *
 * equity-history 等端点历史上输出无时区标记的 UTC 时间（"2006-01-02 15:04:05"）。
 * 直接 `new Date(s)` 会按浏览器本地时区解析，UTC+8 用户的权益曲线时间轴整体错位。
 * 新格式为 RFC3339（带 Z 或 ±hh:mm 偏移）。
 *
 * parseServerTime 对两种格式都按其真实时区解析：
 * - 带时区后缀（Z / ±hh:mm / ±hhmm）→ 原样交给 Date 解析；
 * - 无时区后缀 → 视为 UTC：空格分隔符换成 'T' 并补 'Z'（ISO 形式在各引擎解析一致）。
 */

// 时区后缀：Z，或 ±hh:mm / ±hhmm（RFC3339 偏移形式）
const TZ_SUFFIX = /(?:Z|[+-]\d{2}:?\d{2})$/i

export function parseServerTime(value: string | number | Date): Date {
  if (value instanceof Date) return value
  if (typeof value === 'number') return new Date(value)

  const s = value.trim()
  if (TZ_SUFFIX.test(s)) return new Date(s)

  // 无时区 → 按 UTC 解析
  const normalized = s.replace(' ', 'T')
  const withZone = TZ_SUFFIX.test(normalized) ? normalized : `${normalized}Z`
  const parsed = new Date(withZone)
  // 罕见的非 ISO 字符串：退回原始解析（可能为 Invalid Date，由调用方处理）
  return Number.isNaN(parsed.getTime()) ? new Date(s) : parsed
}

/** 解析失败时返回 null（Invalid Date 归一为 null），便于调用方区分缺失。 */
export function parseServerTimeOrNull(
  value: string | number | Date
): Date | null {
  const d = parseServerTime(value)
  return Number.isNaN(d.getTime()) ? null : d
}
