// K 线画布图表的配色: 基于 lib/chartTheme 的 getChartTheme(), 只把十字线标签底色
// 换成设计规范要求的 --surface-2 (chartTheme 默认是品牌色)。
// lightweight-charts 不认识 var(), 这里读取的都是解析后的颜色值。
import { getChartTheme, type ChartTheme } from '../../lib/chartTheme'

function readVar(name: string, fallback: string): string {
  if (typeof document === 'undefined') return fallback
  const v = getComputedStyle(document.documentElement)
    .getPropertyValue(name)
    .trim()
  return v || fallback
}

export function getCandleChartTheme(): ChartTheme {
  return {
    ...getChartTheme(),
    crosshairLabel: readVar('--surface-2', '#1c2127'),
  }
}
