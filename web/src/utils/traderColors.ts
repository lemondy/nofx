// Trader颜色配置 - 统一的颜色分配逻辑
// 用于 ComparisonChart 和 Leaderboard，确保颜色一致性
// 深色主题使用亮色，浅色主题使用同色相的深色变体(白底对比度 >= 4.5:1)

export const TRADER_COLORS_DARK = [
  '#8fb08f', // sage (lightened for dark-theme contrast)
  '#c084fc', // purple-400
  '#34d399', // emerald-400
  '#fb923c', // orange-400
  '#f472b6', // pink-400
  '#fbbf24', // amber-400
  '#38bdf8', // sky-400
  '#a78bfa', // violet-400
  '#4ade80', // green-400
  '#fb7185', // rose-400
]

export const TRADER_COLORS_LIGHT = [
  '#4a634a', // sage
  '#7e22ce', // purple-700
  '#047857', // emerald-700
  '#c2410c', // orange-700
  '#be185d', // pink-700
  '#92400e', // amber-800
  '#0369a1', // sky-700
  '#6d28d9', // violet-700
  '#15803d', // green-700
  '#be123c', // rose-700
]

/** 兼容旧用法: 深色主题调色板 */
export const TRADER_COLORS = TRADER_COLORS_DARK

export type TraderColorTheme = 'dark' | 'light'

function activeTheme(): TraderColorTheme {
  if (typeof document === 'undefined') return 'dark'
  return document.documentElement.dataset.theme === 'light' ? 'light' : 'dark'
}

/** 当前主题的调色板 */
export function getTraderPalette(theme?: TraderColorTheme): string[] {
  return (theme ?? activeTheme()) === 'light'
    ? TRADER_COLORS_LIGHT
    : TRADER_COLORS_DARK
}

/**
 * 根据trader的索引位置获取颜色(随当前主题变化, 渲染时调用即可)
 * @param traders - trader列表
 * @param traderId - 当前trader的ID
 * @param theme - 可选, 显式指定主题
 * @returns 对应的颜色值
 */
export function getTraderColor(
  traders: Array<{ trader_id: string }>,
  traderId: string,
  theme?: TraderColorTheme
): string {
  const palette = getTraderPalette(theme)
  const traderIndex = traders.findIndex((t) => t.trader_id === traderId)
  if (traderIndex === -1) return palette[0] // 默认返回第一个颜色
  // 如果超出颜色池大小，循环使用
  return palette[traderIndex % palette.length]
}
