import { useMemo, useState } from 'react'
import {
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
  ReferenceLine,
  Legend,
  Area,
  ComposedChart,
} from 'recharts'
import useSWR from 'swr'
import { api } from '../../lib/api'
import type { CompetitionTraderData } from '../../types'
import { getTraderColor } from '../../utils/traderColors'
import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'
import { BarChart3, TrendingUp, TrendingDown, Zap } from 'lucide-react'
import { cn } from '../../lib/cn'
import { Change, EmptyState, Segmented, formatSigned } from '../ui'

// Time period options: 1D, 3D, 7D, 30D, All
const TIME_PERIODS = [
  { key: '1d', hours: 24 },
  { key: '3d', hours: 72 },
  { key: '7d', hours: 168 },
  { key: '30d', hours: 720 },
  { key: 'all', hours: 0 },
] as const

interface ComparisonChartProps {
  traders: CompetitionTraderData[]
}

export function ComparisonChart({ traders }: ComparisonChartProps) {
  const { language } = useLanguage()
  const [selectedPeriod, setSelectedPeriod] = useState('7d') // Default to 7 days

  // Get hours for selected period
  const selectedHours =
    TIME_PERIODS.find((p) => p.key === selectedPeriod)?.hours || 0

  // Generate unique key for SWR (include period and hours)
  const tradersKey = traders
    .map((t) => t.trader_id)
    .sort()
    .join(',')

  const { data: allTraderHistories, isLoading } = useSWR(
    traders.length > 0
      ? `equity-histories-${tradersKey}-${selectedHours}`
      : null,
    async () => {
      console.log('Fetching equity history with hours:', selectedHours)
      const traderIds = traders.map((trader) => trader.trader_id)
      const batchData = await api.getEquityHistoryBatch(
        traderIds,
        selectedHours
      )
      console.log(
        'Received data points:',
        Object.values(batchData.histories || {}).map((h: any) => h?.length)
      )
      return traders.map((trader) => {
        const history = batchData.histories?.[trader.trader_id] || []

        // If backend doesn't return total_pnl_pct, calculate it from equity
        if (history.length > 0 && history[0].total_pnl_pct === undefined) {
          const initialEquity = history[0].total_equity
          history.forEach((point: any) => {
            point.total_pnl_pct =
              initialEquity > 0
                ? ((point.total_equity - initialEquity) / initialEquity) * 100
                : 0
          })
        }

        return history
      })
    },
    {
      refreshInterval: 30000,
      revalidateOnFocus: false,
      dedupingInterval: 0, // No deduping for immediate response
      keepPreviousData: false,
    }
  )

  const traderHistories = useMemo(() => {
    if (!allTraderHistories) {
      return traders.map(() => ({ data: undefined }))
    }
    return allTraderHistories.map((data) => ({ data }))
  }, [allTraderHistories, traders.length])

  const combinedData = useMemo(() => {
    const allLoaded = traderHistories.every((h) => h.data)
    if (!allLoaded) return []

    const timestampMap = new Map<
      string,
      {
        timestamp: string
        time: string
        traders: Map<
          string,
          { pnl_pct: number; equity: number; originalTs?: string }
        >
      }
    >()

    // Helper function to normalize timestamp to nearest minute
    const normalizeTimestamp = (ts: string): string => {
      const date = new Date(ts)
      date.setSeconds(0, 0) // Round to minute
      return date.toISOString()
    }

    traderHistories.forEach((history, index) => {
      const trader = traders[index]
      if (!history.data) return

      history.data.forEach((point: any) => {
        // Normalize timestamp to nearest minute so different traders' data aligns
        const normalizedTs = normalizeTimestamp(point.timestamp)

        if (!timestampMap.has(normalizedTs)) {
          const date = new Date(normalizedTs)
          // Format time based on selected period
          let time: string
          if (selectedHours <= 24) {
            // 1 day: show HH:mm
            time = date.toLocaleTimeString('zh-CN', {
              hour: '2-digit',
              minute: '2-digit',
            })
          } else if (selectedHours <= 72) {
            // 3 days: show MM/DD HH:mm
            time = `${date.getMonth() + 1}/${date.getDate()} ${date.getHours().toString().padStart(2, '0')}:${date.getMinutes().toString().padStart(2, '0')}`
          } else {
            // 7+ days: show MM/DD
            time = `${date.getMonth() + 1}/${date.getDate()}`
          }
          timestampMap.set(normalizedTs, {
            timestamp: normalizedTs,
            time,
            traders: new Map(),
          })
        }

        // Use latest value if multiple points fall in same minute
        const existing = timestampMap
          .get(normalizedTs)!
          .traders.get(trader.trader_id)
        if (
          !existing ||
          new Date(point.timestamp) > new Date(existing.originalTs || '')
        ) {
          timestampMap.get(normalizedTs)!.traders.set(trader.trader_id, {
            pnl_pct: point.total_pnl_pct || 0,
            equity: point.total_equity,
            originalTs: point.timestamp,
          })
        }
      })
    })

    const sortedEntries = Array.from(timestampMap.entries()).sort(
      ([tsA], [tsB]) => new Date(tsA).getTime() - new Date(tsB).getTime()
    )

    // Track last known values for each trader to fill gaps
    const lastKnown: Map<string, { pnl_pct: number; equity: number }> =
      new Map()

    const combined = sortedEntries.map(([ts, data], index) => {
      const entry: any = {
        index: index + 1,
        time: data.time,
        timestamp: ts,
      }

      traders.forEach((trader) => {
        const traderData = data.traders.get(trader.trader_id)
        if (traderData) {
          // Update last known value
          lastKnown.set(trader.trader_id, {
            pnl_pct: traderData.pnl_pct,
            equity: traderData.equity,
          })
          entry[`${trader.trader_id}_pnl_pct`] = traderData.pnl_pct
          entry[`${trader.trader_id}_equity`] = traderData.equity
        } else {
          // Use last known value to fill gap
          const last = lastKnown.get(trader.trader_id)
          if (last) {
            entry[`${trader.trader_id}_pnl_pct`] = last.pnl_pct
            entry[`${trader.trader_id}_equity`] = last.equity
          }
        }
      })

      return entry
    })

    return combined
  }, [allTraderHistories, traders, selectedHours])

  // Get trader color
  const traderColor = (traderId: string) => getTraderColor(traders, traderId)

  if (isLoading) {
    return (
      <div className="flex flex-col items-center justify-center py-20">
        <div className="relative">
          <div className="h-16 w-16 animate-spin rounded-full border-4 border-brand border-t-transparent" />
          <TrendingUp className="absolute left-1/2 top-1/2 h-6 w-6 -translate-x-1/2 -translate-y-1/2 text-brand" />
        </div>
        <div className="mt-4 text-sm font-medium text-fg-3">
          {t('loadingChartData', language) || 'Loading chart data...'}
        </div>
      </div>
    )
  }

  if (combinedData.length === 0) {
    return (
      <EmptyState
        icon={
          <span className="flex h-12 w-12 items-center justify-center rounded-lg bg-brand-soft text-brand">
            <BarChart3 className="h-6 w-6" />
          </span>
        }
        title={t('noHistoricalData', language)}
        description={t('dataWillAppear', language)}
        className="py-20"
      />
    )
  }

  const MAX_DISPLAY_POINTS = 500
  const displayData =
    combinedData.length > MAX_DISPLAY_POINTS
      ? combinedData.slice(-MAX_DISPLAY_POINTS)
      : combinedData

  // Calculate Y axis domain with better padding
  const calculateYDomain = () => {
    const allValues: number[] = []
    displayData.forEach((point) => {
      traders.forEach((trader) => {
        const value = point[`${trader.trader_id}_pnl_pct`]
        if (value !== undefined && !isNaN(value)) {
          allValues.push(value)
        }
      })
    })

    if (allValues.length === 0) return [-2, 2]

    const minVal = Math.min(...allValues)
    const maxVal = Math.max(...allValues)
    const range = maxVal - minVal

    // Use actual data range with 20% padding on each side
    // This ensures both lines are clearly visible
    const padding = Math.max(range * 0.2, 2) // At least 2% padding

    return [
      Math.floor((minVal - padding) * 10) / 10,
      Math.ceil((maxVal + padding) * 10) / 10,
    ]
  }

  // Custom Tooltip
  const CustomTooltip = ({ active, payload }: any) => {
    if (active && payload && payload.length) {
      const data = payload[0].payload
      const date = new Date(data.timestamp)
      const dateStr = date.toLocaleDateString('zh-CN', {
        month: 'short',
        day: 'numeric',
      })

      return (
        <div className="min-w-[200px] rounded-md border border-line bg-surface p-3 shadow-pop">
          <div className="mb-2.5 flex items-center gap-1.5 border-b border-line pb-2">
            <Zap className="h-3.5 w-3.5 text-fg-3" />
            <span className="num text-xs font-medium text-fg-2">
              {dateStr} {data.time}
            </span>
          </div>
          <div className="space-y-2">
            {traders.map((trader) => {
              const pnlPct = data[`${trader.trader_id}_pnl_pct`]
              const equity = data[`${trader.trader_id}_equity`]
              if (pnlPct === undefined) return null
              const isPositive = pnlPct >= 0

              return (
                <div
                  key={trader.trader_id}
                  className="flex items-center justify-between gap-4"
                >
                  <div className="flex min-w-0 items-center gap-2">
                    <span
                      className="h-2 w-2 shrink-0 rounded-full"
                      style={{ background: traderColor(trader.trader_id) }}
                    />
                    <span className="max-w-[100px] truncate text-xs font-medium text-fg">
                      {trader.trader_name}
                    </span>
                  </div>
                  <div className="text-right">
                    <div
                      className={cn(
                        'num flex items-center justify-end gap-1 text-sm font-semibold',
                        isPositive ? 'text-up' : 'text-down'
                      )}
                    >
                      {isPositive ? (
                        <TrendingUp className="h-3 w-3" />
                      ) : (
                        <TrendingDown className="h-3 w-3" />
                      )}
                      {formatSigned(pnlPct, { suffix: '%' })}
                    </div>
                    <div className="num text-[11px] text-fg-3">
                      ${equity?.toFixed(2)}
                    </div>
                  </div>
                </div>
              )
            })}
          </div>
        </div>
      )
    }
    return null
  }

  // Calculate stats - find each trader's last available data point
  const traderStats = traders
    .map((trader) => {
      // Find the last data point that has data for this trader
      let currentPnl = 0
      let currentEquity = 0
      for (let i = displayData.length - 1; i >= 0; i--) {
        const pnl = displayData[i]?.[`${trader.trader_id}_pnl_pct`]
        if (pnl !== undefined) {
          currentPnl = pnl
          currentEquity = displayData[i]?.[`${trader.trader_id}_equity`] || 0
          break
        }
      }
      return { ...trader, currentPnl, currentEquity }
    })
    .sort((a, b) => b.currentPnl - a.currentPnl)

  const leader = traderStats[0]
  const gap =
    traderStats.length > 1
      ? Math.abs(traderStats[0].currentPnl - traderStats[1].currentPnl).toFixed(
          2
        )
      : '0.00'

  return (
    <div className="space-y-3">
      {/* Time Period Selector + Mini Stats Bar */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        {/* Time Period Buttons */}
        <Segmented
          size="sm"
          value={selectedPeriod}
          onChange={setSelectedPeriod}
          items={TIME_PERIODS.map((period) => ({
            key: period.key,
            label: t(`comparisonChart.${period.key}`, language),
          }))}
        />

        {/* Mini Stats Bar */}
        <div className="flex flex-wrap items-center gap-2">
          {traderStats.slice(0, 3).map((trader, idx) => (
            <div
              key={trader.trader_id}
              className={cn(
                'flex items-center gap-2 rounded-full border px-2.5 py-1',
                idx === 0
                  ? 'border-brand/30 bg-brand-soft'
                  : 'border-line bg-surface-2'
              )}
            >
              <span
                className="h-2 w-2 shrink-0 rounded-full"
                style={{ background: traderColor(trader.trader_id) }}
              />
              <span className="max-w-[80px] truncate text-xs font-medium text-fg">
                {trader.trader_name}
              </span>
              <span
                className={cn(
                  'num text-xs font-semibold',
                  trader.currentPnl >= 0 ? 'text-up' : 'text-down'
                )}
              >
                {formatSigned(trader.currentPnl, { suffix: '%' })}
              </span>
            </div>
          ))}
        </div>
      </div>

      {/* Chart */}
      <div className="relative overflow-hidden rounded-lg border border-line bg-surface">
        {/* Watermark */}
        <div className="pointer-events-none absolute left-1/2 top-1/2 z-[1] -translate-x-1/2 -translate-y-1/2 font-num text-[80px] font-bold tracking-[0.1em] text-brand/[0.03]">
          NOFX
        </div>

        <ResponsiveContainer width="100%" height={420}>
          <ComposedChart
            data={displayData}
            margin={{ top: 20, right: 20, left: 10, bottom: 20 }}
          >
            <defs>
              {traders.map((trader) => (
                <linearGradient
                  key={`area-gradient-${trader.trader_id}`}
                  id={`area-gradient-${trader.trader_id}`}
                  x1="0"
                  y1="0"
                  x2="0"
                  y2="1"
                >
                  <stop
                    offset="0%"
                    stopColor={traderColor(trader.trader_id)}
                    stopOpacity={0.3}
                  />
                  <stop
                    offset="100%"
                    stopColor={traderColor(trader.trader_id)}
                    stopOpacity={0}
                  />
                </linearGradient>
              ))}
            </defs>

            <CartesianGrid
              strokeDasharray="3 3"
              stroke="var(--line)"
              strokeOpacity={0.6}
              vertical={false}
            />

            <XAxis
              dataKey="time"
              stroke="var(--line)"
              tick={{ fill: 'var(--fg-3)', fontSize: 10 }}
              tickLine={false}
              axisLine={{ stroke: 'var(--line)' }}
              interval={Math.max(Math.floor(displayData.length / 8), 1)}
            />

            <YAxis
              stroke="var(--line)"
              tick={{ fill: 'var(--fg-3)', fontSize: 10 }}
              tickLine={false}
              axisLine={false}
              domain={calculateYDomain()}
              tickFormatter={(value) => `${value.toFixed(1)}%`}
              width={50}
            />

            <Tooltip
              content={<CustomTooltip />}
              cursor={{ stroke: 'var(--line-strong)', strokeDasharray: '3 3' }}
            />

            {/* Zero reference line */}
            <ReferenceLine
              y={0}
              stroke="var(--line-strong)"
              strokeDasharray="8 4"
              strokeWidth={1}
            />

            {/* Area fills for top 2 traders */}
            {traders.slice(0, 2).map((trader) => (
              <Area
                key={`area-${trader.trader_id}`}
                type="monotone"
                dataKey={`${trader.trader_id}_pnl_pct`}
                fill={`url(#area-gradient-${trader.trader_id})`}
                stroke="none"
                connectNulls
              />
            ))}

            {/* Lines for all traders */}
            {traders.map((trader, idx) => (
              <Line
                key={trader.trader_id}
                type="monotone"
                dataKey={`${trader.trader_id}_pnl_pct`}
                stroke={traderColor(trader.trader_id)}
                strokeWidth={idx === 0 ? 3 : 2}
                dot={false}
                activeDot={{
                  r: 5,
                  fill: traderColor(trader.trader_id),
                  stroke: 'var(--surface)',
                  strokeWidth: 2,
                }}
                name={trader.trader_name}
                connectNulls
              />
            ))}

            <Legend
              wrapperStyle={{ paddingTop: '16px' }}
              content={({ payload }) => {
                // Filter out Area entries (they use raw dataKey containing _pnl_pct)
                const filteredPayload =
                  payload?.filter(
                    (entry: any) =>
                      entry.value && !entry.value.includes('_pnl_pct')
                  ) || []

                return (
                  <div className="flex flex-wrap justify-center gap-5">
                    {filteredPayload.map((entry: any, index: number) => {
                      const trader = traders.find(
                        (t) => t.trader_name === entry.value
                      )
                      // Find this trader's last available PnL from traderStats
                      const traderStat = traderStats.find(
                        (t) => t.trader_id === trader?.trader_id
                      )
                      const pnl = traderStat?.currentPnl || 0
                      return (
                        <div
                          key={`legend-${index}`}
                          className="flex items-center gap-1.5"
                        >
                          <span
                            className="h-2 w-2 rounded-full"
                            style={{ backgroundColor: entry.color }}
                          />
                          <span className="text-xs font-medium text-fg">
                            {entry.value}
                            <span
                              className={cn(
                                'num ml-1.5',
                                pnl >= 0 ? 'text-up' : 'text-down'
                              )}
                            >
                              ({formatSigned(pnl, { suffix: '%' })})
                            </span>
                          </span>
                        </div>
                      )
                    })}
                  </div>
                )
              }}
            />
          </ComposedChart>
        </ResponsiveContainer>
      </div>

      {/* Bottom Stats */}
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        <div className="rounded-md border border-line bg-surface-2 p-2.5 text-center">
          <div className="mb-1 text-[10px] uppercase tracking-wider text-fg-3">
            {t('leader', language)}
          </div>
          <div className="truncate text-sm font-semibold text-brand">
            {leader?.trader_name || '-'}
          </div>
        </div>
        <div className="rounded-md border border-line bg-surface-2 p-2.5 text-center">
          <div className="mb-1 text-[10px] uppercase tracking-wider text-fg-3">
            {t('leadPnL', language) || 'Lead PnL'}
          </div>
          <Change
            value={leader?.currentPnl || 0}
            className="text-sm font-semibold"
          />
        </div>
        <div className="rounded-md border border-line bg-surface-2 p-2.5 text-center">
          <div className="mb-1 text-[10px] uppercase tracking-wider text-fg-3">
            {t('currentGap', language)}
          </div>
          <div className="num text-sm font-semibold text-info">{gap}%</div>
        </div>
        <div className="rounded-md border border-line bg-surface-2 p-2.5 text-center">
          <div className="mb-1 text-[10px] uppercase tracking-wider text-fg-3">
            {t('dataPoints', language)}
          </div>
          <div className="num text-sm font-semibold text-fg">
            {displayData.length}
          </div>
        </div>
      </div>
    </div>
  )
}
