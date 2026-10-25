import { cn } from '../../lib/cn'
import { useState, useEffect, useCallback } from 'react'
import {
  Shield,
  TrendingUp,
  AlertTriangle,
  Activity,
  Box,
  ChevronDown,
  ChevronUp,
} from 'lucide-react'
import type { GridRiskInfo } from '../../types'
import { gridRisk, ts } from '../../i18n/strategy-translations'
import { withAlpha } from '../../lib/colorAlpha'

interface GridRiskPanelProps {
  traderId: string
  language?: string
  refreshInterval?: number // ms, default 5000
}

export function GridRiskPanel({
  traderId,
  language = 'en',
  refreshInterval = 5000,
}: GridRiskPanelProps) {
  const [riskInfo, setRiskInfo] = useState<GridRiskInfo | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [expanded, setExpanded] = useState(false)

  const fetchRiskInfo = useCallback(async () => {
    try {
      const token = localStorage.getItem('auth_token')
      const response = await fetch(`/api/traders/${traderId}/grid-risk`, {
        headers: {
          Authorization: `Bearer ${token}`,
        },
      })

      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`)
      }

      const data = await response.json()
      setRiskInfo(data)
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error')
    } finally {
      setLoading(false)
    }
  }, [traderId])

  useEffect(() => {
    fetchRiskInfo()
    const interval = setInterval(fetchRiskInfo, refreshInterval)
    return () => clearInterval(interval)
  }, [fetchRiskInfo, refreshInterval])

  const getRegimeColor = (regime: string) => {
    switch (regime) {
      case 'narrow':
        return 'var(--up)'
      case 'standard':
        return 'var(--brand)'
      case 'wide':
        return 'var(--warn)'
      case 'volatile':
        return 'var(--down)'
      case 'trending':
        return 'var(--ai)'
      default:
        return 'var(--fg-3)'
    }
  }

  const getBreakoutColor = (level: string) => {
    switch (level) {
      case 'none':
        return 'var(--up)'
      case 'short':
        return 'var(--brand)'
      case 'mid':
        return 'var(--warn)'
      case 'long':
        return 'var(--down)'
      default:
        return 'var(--fg-3)'
    }
  }

  const getPositionColor = (percent: number) => {
    if (percent < 50) return 'var(--up)'
    if (percent < 80) return 'var(--brand)'
    return 'var(--down)'
  }

  const formatPrice = (price: number) => {
    if (price === 0) return '-'
    if (price >= 1000)
      return price.toLocaleString('en-US', {
        minimumFractionDigits: 2,
        maximumFractionDigits: 2,
      })
    if (price >= 1) return price.toFixed(4)
    return price.toFixed(6)
  }

  const formatUSD = (value: number) => {
    return `$${value.toLocaleString('en-US', { minimumFractionDigits: 0, maximumFractionDigits: 0 })}`
  }

  if (loading) {
    return (
      <div className="p-3 text-center text-xs text-fg-3">
        {ts(gridRisk.loading, language)}
      </div>
    )
  }

  if (error) {
    return (
      <div className="p-3 text-center text-xs text-down">
        {ts(gridRisk.error, language)}: {error}
      </div>
    )
  }

  if (!riskInfo) {
    return (
      <div className="p-3 text-center text-xs text-fg-3">
        {ts(gridRisk.noData, language)}
      </div>
    )
  }

  return (
    <div className="rounded-lg bg-surface-2 border border-line">
      {/* Collapsible Header */}
      <div
        className="flex items-center justify-between p-3 cursor-pointer hover:bg-surface-hover transition-colors"
        onClick={() => setExpanded(!expanded)}
      >
        <div className="flex items-center gap-2">
          <Shield className="w-4 h-4 text-brand" />
          <span className="font-medium text-sm text-fg">
            {ts(gridRisk.gridRisk, language)}
          </span>
        </div>
        <div className="flex items-center gap-3">
          {/* Summary badges when collapsed */}
          <div className="flex items-center gap-2 text-xs">
            <span
              className="px-2 py-0.5 rounded"
              style={{
                background: withAlpha(
                  getRegimeColor(riskInfo.regime_level),
                  13
                ),
                color: getRegimeColor(riskInfo.regime_level),
              }}
            >
              {ts(
                gridRisk[
                  (riskInfo.regime_level || 'standard') as keyof typeof gridRisk
                ],
                language
              )}
            </span>
            <span className="num text-fg">
              {riskInfo.effective_leverage.toFixed(1)}x
            </span>
            <span
              className="num"
              style={{ color: getPositionColor(riskInfo.position_percent) }}
            >
              {riskInfo.position_percent.toFixed(0)}%
            </span>
          </div>
          {expanded ? (
            <ChevronUp className="w-4 h-4 text-fg-3" />
          ) : (
            <ChevronDown className="w-4 h-4 text-fg-3" />
          )}
        </div>
      </div>

      {/* Expanded Content */}
      {expanded && (
        <div className="px-3 pb-3 space-y-3">
          {/* Row 1: Leverage & Position */}
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
            {/* Leverage */}
            <div className="p-2 rounded bg-surface-2">
              <div className="flex items-center gap-1 mb-2">
                <TrendingUp className="w-3 h-3 text-brand" />
                <span className="text-xs font-medium text-fg-3">
                  {ts(gridRisk.leverageInfo, language)}
                </span>
              </div>
              <div className="grid grid-cols-3 gap-1 text-xs">
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.currentLeverage, language)}
                  </div>
                  <div className="num text-fg">
                    {riskInfo.current_leverage}x
                  </div>
                </div>
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.effectiveLeverage, language)}
                  </div>
                  <div className="num text-brand">
                    {riskInfo.effective_leverage.toFixed(2)}x
                  </div>
                </div>
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.recommendedLeverage, language)}
                  </div>
                  <div
                    className={cn(
                      'num',
                      riskInfo.current_leverage > riskInfo.recommended_leverage
                        ? 'text-down'
                        : 'text-up'
                    )}
                  >
                    {riskInfo.recommended_leverage}x
                  </div>
                </div>
              </div>
            </div>

            {/* Position */}
            <div className="p-2 rounded bg-surface-2">
              <div className="flex items-center gap-1 mb-2">
                <Activity className="w-3 h-3 text-brand" />
                <span className="text-xs font-medium text-fg-3">
                  {ts(gridRisk.positionInfo, language)}
                </span>
              </div>
              <div className="grid grid-cols-3 gap-1 text-xs">
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.currentPosition, language)}
                  </div>
                  <div className="num text-fg">
                    {formatUSD(riskInfo.current_position)}
                  </div>
                </div>
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.maxPosition, language)}
                  </div>
                  <div className="num text-fg">
                    {formatUSD(riskInfo.max_position)}
                  </div>
                </div>
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.positionPercent, language)}
                  </div>
                  <div
                    className="num"
                    style={{
                      color: getPositionColor(riskInfo.position_percent),
                    }}
                  >
                    {riskInfo.position_percent.toFixed(1)}%
                  </div>
                </div>
              </div>
              {/* Mini progress bar */}
              <div className="h-1 mt-2 rounded-full overflow-hidden bg-line">
                <div
                  className="h-full rounded-full"
                  style={{
                    width: `${Math.min(riskInfo.position_percent, 100)}%`,
                    background: getPositionColor(riskInfo.position_percent),
                  }}
                />
              </div>
            </div>
          </div>

          {/* Row 2: Market State & Liquidation */}
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
            {/* Market State */}
            <div className="p-2 rounded bg-surface-2">
              <div className="flex items-center gap-1 mb-2">
                <Shield className="w-3 h-3 text-brand" />
                <span className="text-xs font-medium text-fg-3">
                  {ts(gridRisk.marketState, language)}
                </span>
              </div>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-2 text-xs">
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.regimeLevel, language)}
                  </div>
                  <div
                    className="font-medium"
                    style={{ color: getRegimeColor(riskInfo.regime_level) }}
                  >
                    {ts(
                      gridRisk[
                        (riskInfo.regime_level ||
                          'standard') as keyof typeof gridRisk
                      ],
                      language
                    )}
                  </div>
                </div>
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.currentPrice, language)}
                  </div>
                  <div className="num text-fg">
                    {formatPrice(riskInfo.current_price)}
                  </div>
                </div>
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.breakoutLevel, language)}
                  </div>
                  <div
                    className="font-medium"
                    style={{ color: getBreakoutColor(riskInfo.breakout_level) }}
                  >
                    {ts(
                      gridRisk[
                        (riskInfo.breakout_level ||
                          'none') as keyof typeof gridRisk
                      ],
                      language
                    )}
                  </div>
                </div>
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.breakoutDirection, language)}
                  </div>
                  <div
                    className={cn(
                      'font-medium',
                      riskInfo.breakout_direction === 'up'
                        ? 'text-up'
                        : riskInfo.breakout_direction === 'down'
                          ? 'text-down'
                          : 'text-fg-3'
                    )}
                  >
                    {riskInfo.breakout_direction
                      ? ts(
                          gridRisk[
                            riskInfo.breakout_direction as keyof typeof gridRisk
                          ],
                          language
                        )
                      : '-'}
                  </div>
                </div>
              </div>
            </div>

            {/* Liquidation */}
            <div className="p-2 rounded bg-surface-2">
              <div className="flex items-center gap-1 mb-2">
                <AlertTriangle className="w-3 h-3 text-down" />
                <span className="text-xs font-medium text-fg-3">
                  {ts(gridRisk.liquidationInfo, language)}
                </span>
              </div>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-2 text-xs">
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.liquidationPrice, language)}
                  </div>
                  <div className="num text-down">
                    {riskInfo.liquidation_price > 0
                      ? formatPrice(riskInfo.liquidation_price)
                      : '-'}
                  </div>
                </div>
                <div>
                  <div className="text-fg-3">
                    {ts(gridRisk.liquidationDistance, language)}
                  </div>
                  <div className="num text-down">
                    {riskInfo.liquidation_distance > 0
                      ? `${riskInfo.liquidation_distance.toFixed(1)}%`
                      : '-'}
                  </div>
                </div>
              </div>
            </div>
          </div>

          {/* Row 3: Box State */}
          <div className="p-2 rounded bg-surface-2">
            <div className="flex items-center gap-1 mb-2">
              <Box className="w-3 h-3 text-brand" />
              <span className="text-xs font-medium text-fg-3">
                {ts(gridRisk.boxState, language)}
              </span>
            </div>
            <div className="grid grid-cols-3 gap-2 text-xs">
              <div className="flex justify-between">
                <span className="text-fg-3">
                  {ts(gridRisk.shortBox, language)}
                </span>
                <span className="num text-fg">
                  {formatPrice(riskInfo.short_box_lower)} -{' '}
                  {formatPrice(riskInfo.short_box_upper)}
                </span>
              </div>
              <div className="flex justify-between">
                <span className="text-fg-3">
                  {ts(gridRisk.midBox, language)}
                </span>
                <span className="num text-fg">
                  {formatPrice(riskInfo.mid_box_lower)} -{' '}
                  {formatPrice(riskInfo.mid_box_upper)}
                </span>
              </div>
              <div className="flex justify-between">
                <span className="text-fg-3">
                  {ts(gridRisk.longBox, language)}
                </span>
                <span className="num text-fg">
                  {formatPrice(riskInfo.long_box_lower)} -{' '}
                  {formatPrice(riskInfo.long_box_upper)}
                </span>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
