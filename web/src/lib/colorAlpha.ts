/**
 * 给任意 CSS 颜色(hex / var(--x) / rgb)加透明度。
 * 取代旧写法 `${hex}33`(hex 后缀) —— 颜色改用 CSS 变量后字符串拼接不再可用。
 */
export function withAlpha(color: string, percent: number): string {
  const p = Math.max(0, Math.min(100, Math.round(percent)))
  return `color-mix(in srgb, ${color} ${p}%, transparent)`
}
