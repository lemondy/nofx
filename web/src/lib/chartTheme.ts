// 图表主题: 画布类图表(lightweight-charts 等)不认识 CSS 变量 / color-mix,
// 这里在运行时读取 :root 上已解析的设计变量, 返回可直接传给图表库的颜色字符串。
// 主题切换时 useThemeVersion() 的返回值变化, 图表据此重新 applyOptions。
import { useSyncExternalStore } from 'react'
import { THEME_CHANGE_EVENT } from './theme'

export interface ChartTheme {
  /** 图表背景 (--surface) */
  background: string
  /** 网格线 (--line, 降透明度) */
  grid: string
  /** 坐标轴/刻度边框 (--line-strong) */
  border: string
  /** 坐标轴文字 (--fg-3) */
  text: string
  /** 图例/强调文字 (--fg) */
  textStrong: string
  /** 十字线 (--brand, 半透明) */
  crosshair: string
  /** 十字线标签底色 (--brand) 及其文字色 (--brand-fg) */
  crosshairLabel: string
  crosshairLabelText: string
  /** 涨 / 跌 (--up / --down) */
  up: string
  down: string
  /** 成交量柱 (涨跌色 50% 透明) */
  volumeUp: string
  volumeDown: string
  /** 品牌色 / 警告 / 信息 (--brand / --warn / --info) */
  brand: string
  warn: string
  info: string
}

function readVar(name: string, fallback: string): string {
  if (typeof document === 'undefined') return fallback
  const v = getComputedStyle(document.documentElement)
    .getPropertyValue(name)
    .trim()
  return v || fallback
}

/** 把 #rgb / #rrggbb / rgb()/rgba() 转成带透明度的 rgba(); 无法解析时原样返回。 */
export function toRgba(color: string, alpha: number): string {
  const c = color.trim()
  let m = c.match(/^#([0-9a-f]{3})$/i)
  if (m) {
    const [r, g, b] = m[1].split('').map((x) => parseInt(x + x, 16))
    return `rgba(${r}, ${g}, ${b}, ${alpha})`
  }
  m = c.match(/^#([0-9a-f]{6})([0-9a-f]{2})?$/i)
  if (m) {
    const n = parseInt(m[1], 16)
    return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`
  }
  m = c.match(/^rgba?\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)/)
  if (m) return `rgba(${m[1]}, ${m[2]}, ${m[3]}, ${alpha})`
  return c
}

export function getChartTheme(): ChartTheme {
  const up = readVar('--up', '#0ecb81')
  const down = readVar('--down', '#f6465d')
  const brand = readVar('--brand', '#e0b03a')
  return {
    background: readVar('--surface', '#14181d'),
    grid: toRgba(readVar('--line', '#262c34'), 0.6),
    border: readVar('--line-strong', '#363d47'),
    text: readVar('--fg-3', '#848e9c'),
    textStrong: readVar('--fg', '#eaecef'),
    crosshair: toRgba(brand, 0.5),
    crosshairLabel: readVar('--surface-2', '#1c2127'),
    crosshairLabelText: readVar('--fg', '#eaecef'),
    up,
    down,
    volumeUp: toRgba(up, 0.5),
    volumeDown: toRgba(down, 0.5),
    brand,
    warn: readVar('--warn', '#f0a020'),
    info: readVar('--info', '#3b8beb'),
  }
}

// ---- 主题版本号: 每次切换主题 +1, 用作 effect 依赖以便图表重新应用配色
let version = 0
const listeners = new Set<() => void>()
if (typeof window !== 'undefined') {
  window.addEventListener(THEME_CHANGE_EVENT, () => {
    version += 1
    listeners.forEach((l) => l())
  })
}

function subscribe(cb: () => void) {
  listeners.add(cb)
  return () => {
    listeners.delete(cb)
  }
}

/** 主题版本号; 切换主题时变化。图表在 useEffect 依赖里放它即可重新 applyOptions。 */
export function useThemeVersion(): number {
  return useSyncExternalStore(
    subscribe,
    () => version,
    () => 0
  )
}
