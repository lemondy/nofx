import { useState, useEffect, useMemo } from 'react'
import useSWR from 'swr'
import { api } from '../../lib/api'
import { useLanguage } from '../../contexts/LanguageContext'
import { t, type Language } from '../../i18n/translations'
import { MetricTooltip } from '../common/MetricTooltip'
import { formatPrice, formatQuantity } from '../../utils/format'
import { NofxSelect } from '../ui/select'
import { Loader2 } from 'lucide-react'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Segmented } from '../ui/tabs'
import { EmptyState } from '../ui/empty-state'
import { PnL, Change } from '../ui/pnl'
import type {
  HistoricalPosition,
  TraderStats,
  SymbolStats,
  DirectionStats,
} from '../../types'

interface PositionHistoryProps {
  traderId: string
}

// Format number with proper decimals (for large numbers)
function formatNumber(value: number, decimals: number = 2): string {
  if (Math.abs(value) >= 1000000) {
    return (value / 1000000).toFixed(2) + 'M'
  }
  if (Math.abs(value) >= 1000) {
    return (value / 1000).toFixed(2) + 'K'
  }
  return value.toFixed(decimals)
}

// Format duration from minutes
function formatDuration(minutes: number): string {
  if (!minutes || minutes <= 0) return '-'
  if (minutes < 60) return `${minutes.toFixed(0)}m`
  if (minutes < 1440) return `${(minutes / 60).toFixed(1)}h`
  return `${(minutes / 1440).toFixed(1)}d`
}

// Format date
function formatDate(dateStr: string): string {
  if (!dateStr) return '-'
  const date = new Date(dateStr)
  if (isNaN(date.getTime())) return '-'
  return date.toLocaleDateString('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

// Stats tile with formula tooltip
function StatCard({
  title,
  value,
  suffix,
  color,
  subtitle,
  metricKey,
  language = 'en',
}: {
  title: string
  value: string | number
  suffix?: string
  color?: string
  icon?: string
  subtitle?: string
  metricKey?: string
  language?: string
}) {
  return (
    <div className="min-w-0 rounded-md border border-line bg-surface-2 px-3 py-2">
      <div className="flex items-center gap-1.5">
        <span className="truncate text-xs text-fg-3">{title}</span>
        {metricKey && (
          <MetricTooltip metricKey={metricKey} language={language} size={12} />
        )}
      </div>
      <div className="mt-0.5 flex items-baseline gap-1">
        <span
          className="num truncate text-lg font-semibold"
          style={{ color: color || 'var(--fg)' }}
        >
          {value}
        </span>
        {suffix && <span className="text-xs text-fg-3">{suffix}</span>}
      </div>
      {subtitle && (
        <div className="mt-0.5 truncate text-[11px] text-fg-3">{subtitle}</div>
      )}
    </div>
  )
}

// Symbol Stats Row
function SymbolStatsRow({ stat }: { stat: SymbolStats }) {
  const totalPnl = stat.total_pnl || 0
  const winRate = stat.win_rate || 0
  const winRateColor =
    winRate >= 60 ? 'text-up' : winRate >= 40 ? 'text-brand' : 'text-down'

  return (
    <div className="flex h-[34px] items-center justify-between border-b border-line px-3 transition-colors last:border-b-0 hover:bg-surface-hover">
      <div className="flex items-center gap-2">
        <span className="num text-[13px] font-semibold text-fg">
          {(stat.symbol || '').replace('USDT', '')}
        </span>
        <span className="num text-[11px] text-fg-3">
          {stat.total_trades || 0} trades
        </span>
      </div>
      <div className="flex items-center gap-4">
        <span className={`num text-[13px] font-semibold ${winRateColor}`}>
          {winRate.toFixed(1)}%
        </span>
        <PnL
          value={totalPnl}
          currency=""
          className="min-w-[70px] text-right text-[13px] font-semibold"
        />
      </div>
    </div>
  )
}

// Direction Stats Card
function DirectionStatsCard({
  stat,
  language,
}: {
  stat: DirectionStats
  language: Language
}) {
  const isLong = (stat.side || '').toLowerCase() === 'long'
  const totalPnl = stat.total_pnl || 0
  const winRate = stat.win_rate || 0
  const tradeCount = stat.trade_count || 0
  const avgPnl = stat.avg_pnl || 0
  const winRateColor =
    winRate >= 60 ? 'text-up' : winRate >= 40 ? 'text-brand' : 'text-down'

  const label = (key: string) => (
    <div className="mb-0.5 text-[11px] text-fg-3">{t(key, language)}</div>
  )

  return (
    <div className="rounded-md border border-line bg-surface-2 px-3 py-2">
      <div className="mb-2">
        <Badge variant={isLong ? 'up' : 'down'}>
          {(stat.side || 'Unknown').toUpperCase()}
        </Badge>
      </div>
      <div className="grid grid-cols-4 gap-3">
        <div>
          {label('positionHistory.trades')}
          <div className="num text-[13px] font-semibold text-fg">
            {tradeCount}
          </div>
        </div>
        <div>
          {label('positionHistory.winRate')}
          <div className={`num text-[13px] font-semibold ${winRateColor}`}>
            {winRate.toFixed(1)}%
          </div>
        </div>
        <div>
          {label('positionHistory.totalPnL')}
          <PnL
            value={totalPnl}
            currency=""
            className="text-[13px] font-semibold"
          />
        </div>
        <div>
          {label('positionHistory.avgPnL')}
          <PnL
            value={avgPnl}
            currency=""
            className="text-[13px] font-semibold"
          />
        </div>
      </div>
    </div>
  )
}

// Position Row Component
// realizedR is the exit's R multiple against the OPENING stop; null when the
// row has no usable planned stop (manual / legacy rows).
export function realizedR(p: HistoricalPosition): number | null {
  const entry = p.entry_price || 0
  const exit = p.exit_price || 0
  const sl = p.initial_stop_loss || 0
  if (entry <= 0 || exit <= 0 || sl <= 0) return null
  const risk = Math.abs(entry - sl)
  if (risk <= 0 || risk / entry >= 0.5) return null
  const isLong = (p.side || '').toUpperCase() === 'LONG'
  return (isLong ? exit - entry : entry - exit) / risk
}

function PositionRow({
  position,
  language,
}: {
  position: HistoricalPosition
  language: Language
}) {
  const side = position.side || ''
  const isLong = side.toUpperCase() === 'LONG'
  const grossPnl = position.realized_pnl || 0
  // Rows show NET PnL (after fees) — the same caliber as the stats cards;
  // gross stays in the tooltip (2026-10-07 review).
  const realizedPnl = grossPnl - (position.fee || 0)
  const rMult = realizedR(position)

  // Calculate holding time
  const entryTime = position.entry_time
    ? new Date(position.entry_time).getTime()
    : 0
  const exitTime = position.exit_time
    ? new Date(position.exit_time).getTime()
    : 0
  const holdingMinutes =
    entryTime && exitTime && exitTime > entryTime
      ? (exitTime - entryTime) / 60000
      : 0

  // Calculate PnL percentage based on entry price
  const entryPrice = position.entry_price || 0
  const exitPrice = position.exit_price || 0
  let pnlPct = 0
  if (entryPrice > 0) {
    if (isLong) {
      pnlPct = ((exitPrice - entryPrice) / entryPrice) * 100
    } else {
      pnlPct = ((entryPrice - exitPrice) / entryPrice) * 100
    }
  }

  // Use entry_quantity for display (original position size)
  const displayQty = position.entry_quantity || position.quantity || 0
  const fee = position.fee || 0

  return (
    <tr className="h-[34px] border-b border-line transition-colors last:border-b-0 hover:bg-surface-hover">
      {/* Symbol + side */}
      <td className="px-3">
        <div className="flex items-center gap-2">
          <span className="num font-semibold text-fg">
            {(position.symbol || '').replace('USDT', '')}
          </span>
          <Badge variant={isLong ? 'up' : 'down'} size="xs">
            {side.toUpperCase()}
          </Badge>
        </div>
      </td>

      <td className="num px-3 text-right text-fg">{formatPrice(entryPrice)}</td>
      <td className="num px-3 text-right text-fg">{formatPrice(exitPrice)}</td>
      <td className="num px-3 text-right text-fg-2">
        {formatQuantity(displayQty)}
      </td>
      <td className="num px-3 text-right text-fg">
        {formatNumber(entryPrice * displayQty)}
      </td>

      {/* P&L (net) */}
      <td
        className="px-3 text-right"
        title={`${t('positionHistory.grossPnl', language)}: ${grossPnl >= 0 ? '+' : ''}${formatNumber(grossPnl)}`}
      >
        <div className="flex items-baseline justify-end gap-2">
          <PnL value={realizedPnl} currency="" className="font-semibold" />
          <Change value={pnlPct} className="text-xs" />
        </div>
      </td>

      {/* R multiple vs the opening stop; excursions in the tooltip */}
      <td
        className={`num px-3 text-right ${
          rMult === null ? 'text-fg-3' : rMult >= 0 ? 'text-up' : 'text-down'
        }`}
        title={
          rMult === null
            ? t('positionHistory.rUnavailable', language)
            : `MFE ${(position.mfe_r || 0).toFixed(2)}R · MAE ${(position.mae_r || 0).toFixed(2)}R${position.exit_mode ? ` · ${position.exit_mode}` : ''}${position.close_reason ? ` · ${position.close_reason}` : ''}`
        }
      >
        {rMult === null ? '—' : `${rMult >= 0 ? '+' : ''}${rMult.toFixed(2)}R`}
      </td>

      {/* Fee - show more precision for small fees */}
      <td className="num px-3 text-right text-xs text-fg-3">
        -{fee < 0.01 && fee > 0 ? fee.toFixed(4) : fee.toFixed(2)}
      </td>

      <td className="num px-3 text-right text-xs text-fg-3">
        {formatDuration(holdingMinutes)}
      </td>

      <td className="num px-3 text-right text-xs text-fg-3">
        {formatDate(position.exit_time)}
      </td>
    </tr>
  )
}

export function PositionHistory({ traderId }: PositionHistoryProps) {
  const { language } = useLanguage()

  // Pagination state
  const [pageSize, setPageSize] = useState<number>(20)
  const [currentPage, setCurrentPage] = useState<number>(1)

  // Filter state
  const [filterSymbol, setFilterSymbol] = useState<string>('all')
  const [filterSide, setFilterSide] = useState<string>('all')
  const [sortBy, setSortBy] = useState<'time' | 'pnl' | 'pnl_pct'>('time')
  const [sortOrder, setSortOrder] = useState<'asc' | 'desc'>('desc')

  // Poll like the other dashboard panels (SWR, 30s) — the old fetch-once
  // useEffect froze the list at mount: trades closed hours ago never showed
  // up until a manual reload (user report 09-30).
  const historyLimit = Math.max(200, pageSize * 5)
  const {
    data: historyData,
    error: historyError,
    isLoading: historyLoading,
  } = useSWR(
    traderId ? `position-history-${traderId}-${historyLimit}` : null,
    () => api.getPositionHistory(traderId!, historyLimit, true),
    {
      refreshInterval: 30000,
      revalidateOnFocus: false,
      dedupingInterval: 15000,
    }
  )
  const positions: HistoricalPosition[] = historyData?.positions || []
  const stats: TraderStats | null = historyData?.stats ?? null
  const symbolStats: SymbolStats[] = historyData?.symbol_stats || []
  const directionStats: DirectionStats[] = historyData?.direction_stats || []
  const loading = historyLoading
  const error = historyError
    ? historyError instanceof Error
      ? historyError.message
      : 'Failed to load history'
    : null

  // Get unique symbols for filter
  const uniqueSymbols = useMemo(() => {
    const symbols = new Set(positions.map((p) => p.symbol))
    return Array.from(symbols).sort()
  }, [positions])

  // Filtered and sorted positions (before pagination)
  const filteredAndSortedPositions = useMemo(() => {
    let result = [...positions]

    // Apply filters
    if (filterSymbol !== 'all') {
      result = result.filter((p) => p.symbol === filterSymbol)
    }
    if (filterSide !== 'all') {
      result = result.filter(
        (p) => (p.side || '').toUpperCase() === filterSide.toUpperCase()
      )
    }

    // Apply sorting
    result.sort((a, b) => {
      let comparison = 0
      switch (sortBy) {
        case 'time':
          comparison =
            new Date(a.exit_time || 0).getTime() -
            new Date(b.exit_time || 0).getTime()
          break
        case 'pnl':
          comparison = (a.realized_pnl || 0) - (b.realized_pnl || 0)
          break
        case 'pnl_pct': {
          const aPrice = a.entry_price || 1
          const bPrice = b.entry_price || 1
          const aPct = (((a.exit_price || 0) - aPrice) / aPrice) * 100
          const bPct = (((b.exit_price || 0) - bPrice) / bPrice) * 100
          comparison = aPct - bPct
          break
        }
      }
      return sortOrder === 'desc' ? -comparison : comparison
    })

    return result
  }, [positions, filterSymbol, filterSide, sortBy, sortOrder])

  // Pagination calculations
  const totalFilteredCount = filteredAndSortedPositions.length
  const totalPages = Math.ceil(totalFilteredCount / pageSize)

  // Reset to page 1 when filters change
  useEffect(() => {
    setCurrentPage(1)
  }, [filterSymbol, filterSide, sortBy, sortOrder, pageSize])

  // Paginated positions (for display)
  const paginatedPositions = useMemo(() => {
    const startIndex = (currentPage - 1) * pageSize
    return filteredAndSortedPositions.slice(startIndex, startIndex + pageSize)
  }, [filteredAndSortedPositions, currentPage, pageSize])

  // For backwards compatibility, keep filteredPositions as the paginated result
  const filteredPositions = paginatedPositions

  // Calculate profit/loss ratio (avg win / avg loss)
  const profitLossRatio = useMemo(() => {
    if (!stats) return 0
    const avgWin = stats.avg_win || 0
    const avgLoss = stats.avg_loss || 0
    if (avgLoss === 0) return avgWin > 0 ? Infinity : 0
    return avgWin / avgLoss
  }, [stats])

  if (loading) {
    return (
      <div className="flex items-center justify-center gap-2 p-12 text-fg-3">
        <Loader2 className="h-5 w-5 animate-spin" />
        {t('positionHistory.loading', language)}
      </div>
    )
  }

  if (error) {
    return (
      <div className="m-4 rounded-md border border-down/30 bg-down-soft p-4 text-center text-down">
        {error}
      </div>
    )
  }

  if (positions.length === 0) {
    return (
      <EmptyState
        title={t('positionHistory.noHistory', language)}
        description={t('positionHistory.noHistoryDesc', language)}
      />
    )
  }

  const totalFilteredPnl = filteredAndSortedPositions.reduce(
    (sum, p) => sum + (p.realized_pnl || 0),
    0
  )

  const thBase =
    'sticky top-0 z-10 h-8 whitespace-nowrap bg-surface-2 px-3 text-xs font-medium text-fg-3'

  const pageBtn = (label: string, onClick: () => void, disabled: boolean) => (
    <Button
      variant="ghost"
      size="sm"
      className="h-6 px-2"
      onClick={onClick}
      disabled={disabled}
    >
      {label}
    </Button>
  )

  return (
    <div className="space-y-3 p-3">
      {/* Overall Stats - Row 1: Core Metrics */}
      {stats && (
        <div className="grid grid-cols-2 gap-2 md:grid-cols-3 lg:grid-cols-6">
          <StatCard
            title={t('positionHistory.totalTrades', language)}
            value={stats.total_trades || 0}
            subtitle={t('positionHistory.winLoss', language, {
              win: stats.win_trades || 0,
              loss: stats.loss_trades || 0,
            })}
            language={language}
          />
          <StatCard
            title={t('positionHistory.winRate', language)}
            value={(stats.win_rate || 0).toFixed(1)}
            suffix="%"
            color={
              (stats.win_rate || 0) >= 60
                ? 'var(--up)'
                : (stats.win_rate || 0) >= 40
                  ? 'var(--brand)'
                  : 'var(--down)'
            }
            metricKey="win_rate"
            language={language}
          />
          <StatCard
            title={t('positionHistory.totalPnL', language)}
            value={
              ((stats.total_pnl || 0) >= 0 ? '+' : '') +
              formatNumber(stats.total_pnl || 0)
            }
            color={(stats.total_pnl || 0) >= 0 ? 'var(--up)' : 'var(--down)'}
            subtitle={`${t('positionHistory.fee', language)}: -${formatNumber(stats.total_fee || 0)}`}
            metricKey="total_return"
            language={language}
          />
          <StatCard
            title={t('positionHistory.totalWin', language)}
            value={'+' + formatNumber(stats.total_win || 0)}
            color="var(--up)"
            language={language}
          />
          <StatCard
            title={t('positionHistory.totalLoss', language)}
            value={'-' + formatNumber(stats.total_loss || 0)}
            color="var(--down)"
            language={language}
          />
          <StatCard
            title={t('positionHistory.profitFactor', language)}
            value={(stats.profit_factor || 0).toFixed(2)}
            color={
              (stats.profit_factor || 0) >= 1.5
                ? 'var(--up)'
                : (stats.profit_factor || 0) >= 1
                  ? 'var(--brand)'
                  : 'var(--down)'
            }
            subtitle={t('positionHistory.profitFactorDesc', language)}
            metricKey="profit_factor"
            language={language}
          />
        </div>
      )}

      {/* Overall Stats - Row 2: Advanced Metrics */}
      {stats && (
        <div className="grid grid-cols-2 gap-2 md:grid-cols-3 lg:grid-cols-6">
          <StatCard
            title={t('positionHistory.plRatio', language)}
            value={
              profitLossRatio === Infinity ? '∞' : profitLossRatio.toFixed(2)
            }
            color={
              profitLossRatio >= 1.5
                ? 'var(--up)'
                : profitLossRatio >= 1
                  ? 'var(--brand)'
                  : 'var(--down)'
            }
            subtitle={t('positionHistory.plRatioDesc', language)}
            metricKey="expectancy"
            language={language}
          />
          <StatCard
            title={t('positionHistory.sharpeRatio', language)}
            value={(stats.sharpe_ratio || 0).toFixed(2)}
            color={
              (stats.sharpe_ratio || 0) >= 1
                ? 'var(--up)'
                : (stats.sharpe_ratio || 0) >= 0
                  ? 'var(--brand)'
                  : 'var(--down)'
            }
            subtitle={t('positionHistory.sharpeRatioDesc', language)}
            metricKey="sharpe_ratio"
            language={language}
          />
          <StatCard
            title={t('positionHistory.maxDrawdown', language)}
            value={(stats.max_drawdown_pct || 0).toFixed(1)}
            suffix="%"
            color={
              (stats.max_drawdown_pct || 0) <= 10
                ? 'var(--up)'
                : (stats.max_drawdown_pct || 0) <= 20
                  ? 'var(--brand)'
                  : 'var(--down)'
            }
            metricKey="max_drawdown"
            language={language}
          />
          <StatCard
            title={t('positionHistory.avgWin', language)}
            value={'+' + formatNumber(stats.avg_win || 0)}
            color="var(--up)"
            metricKey="avg_trade_pnl"
            language={language}
          />
          <StatCard
            title={t('positionHistory.avgLoss', language)}
            value={'-' + formatNumber(stats.avg_loss || 0)}
            color="var(--down)"
            language={language}
          />
          <StatCard
            title={t('positionHistory.netPnL', language)}
            value={
              ((stats.total_pnl || 0) - (stats.total_fee || 0) >= 0
                ? '+'
                : '') +
              formatNumber((stats.total_pnl || 0) - (stats.total_fee || 0))
            }
            color={
              (stats.total_pnl || 0) - (stats.total_fee || 0) >= 0
                ? 'var(--up)'
                : 'var(--down)'
            }
            subtitle={t('positionHistory.netPnLDesc', language)}
            language={language}
          />
        </div>
      )}

      {/* Direction Stats */}
      {directionStats.length > 0 && (
        <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
          {directionStats.map((stat) => (
            <DirectionStatsCard
              key={stat.side}
              stat={stat}
              language={language}
            />
          ))}
        </div>
      )}

      {/* Symbol Performance */}
      {symbolStats.length > 0 && (
        <div className="rounded-md border border-line bg-surface-2">
          <div className="border-b border-line px-3 py-1.5 text-[13px] font-semibold text-fg">
            {t('positionHistory.symbolPerformance', language)}
          </div>
          <div>
            {symbolStats.slice(0, 10).map((stat) => (
              <SymbolStatsRow key={stat.symbol} stat={stat} />
            ))}
          </div>
        </div>
      )}

      {/* Position List */}
      <div className="overflow-hidden rounded-md border border-line">
        {/* Filters */}
        <div className="flex flex-wrap items-center gap-3 border-b border-line px-3 py-2">
          <div className="flex items-center gap-2">
            <span className="text-xs text-fg-3">
              {t('positionHistory.symbol', language)}:
            </span>
            <NofxSelect
              value={filterSymbol}
              onChange={(val) => setFilterSymbol(val)}
              options={[
                {
                  value: 'all',
                  label: t('positionHistory.allSymbols', language),
                },
                ...uniqueSymbols.map((s) => ({
                  value: s,
                  label: (s || '').replace('USDT', ''),
                })),
              ]}
              className="h-8 rounded-md border border-line bg-surface-2 px-2 text-[13px] text-fg"
            />
          </div>

          <div className="flex items-center gap-2">
            <span className="text-xs text-fg-3">
              {t('positionHistory.side', language)}:
            </span>
            <Segmented
              size="sm"
              value={filterSide}
              onChange={setFilterSide}
              items={['all', 'LONG', 'SHORT'].map((side) => ({
                key: side,
                label:
                  side === 'all' ? t('positionHistory.all', language) : side,
              }))}
            />
          </div>

          <div className="ml-auto flex items-center gap-2">
            <span className="text-xs text-fg-3">
              {t('positionHistory.sort', language)}:
            </span>
            <NofxSelect
              value={`${sortBy}-${sortOrder}`}
              onChange={(val) => {
                const [by, order] = val.split('-') as [
                  'time' | 'pnl' | 'pnl_pct',
                  'asc' | 'desc',
                ]
                setSortBy(by)
                setSortOrder(order)
              }}
              options={[
                {
                  value: 'time-desc',
                  label: t('positionHistory.latestFirst', language),
                },
                {
                  value: 'time-asc',
                  label: t('positionHistory.oldestFirst', language),
                },
                {
                  value: 'pnl-desc',
                  label: t('positionHistory.highestPnL', language),
                },
                {
                  value: 'pnl-asc',
                  label: t('positionHistory.lowestPnL', language),
                },
              ]}
              className="h-8 rounded-md border border-line bg-surface-2 px-2 text-[13px] text-fg"
            />
          </div>
        </div>

        {/* Table */}
        <div className="max-h-[560px] overflow-auto">
          <table className="w-full border-collapse text-[13px]">
            <thead>
              <tr className="border-b border-line">
                <th className={`${thBase} text-left`}>
                  {t('positionHistory.symbol', language)}
                </th>
                <th className={`${thBase} text-right`}>
                  {t('positionHistory.entry', language)}
                </th>
                <th className={`${thBase} text-right`}>
                  {t('positionHistory.exit', language)}
                </th>
                <th className={`${thBase} text-right`}>
                  {t('positionHistory.qty', language)}
                </th>
                <th className={`${thBase} text-right`}>
                  {t('positionHistory.value', language)}
                </th>
                <th className={`${thBase} text-right`}>
                  {t('positionHistory.pnl', language)}
                </th>
                <th
                  className={`${thBase} text-right`}
                  title={t('positionHistory.rMultipleHint', language)}
                >
                  R
                </th>
                <th className={`${thBase} text-right`}>
                  {t('positionHistory.fee', language)}
                </th>
                <th className={`${thBase} text-right`}>
                  {t('positionHistory.duration', language)}
                </th>
                <th className={`${thBase} text-right`}>
                  {t('positionHistory.closedAt', language)}
                </th>
              </tr>
            </thead>
            <tbody>
              {filteredPositions.map((position) => (
                <PositionRow
                  key={position.id}
                  position={position}
                  language={language}
                />
              ))}
            </tbody>
          </table>
        </div>

        {/* Footer with Pagination */}
        <div className="flex flex-wrap items-center justify-between gap-3 border-t border-line px-3 py-2 text-xs text-fg-3">
          <div className="flex items-center gap-4">
            <span>
              {t('positionHistory.showingPositions', language, {
                count: totalFilteredCount,
                total: positions.length,
              })}
            </span>
            {totalFilteredCount > 0 && (
              <span className="flex items-center gap-1">
                {t('positionHistory.totalPnL', language)}:{' '}
                <PnL value={totalFilteredPnl} currency="" />
              </span>
            )}
          </div>

          <div className="flex items-center gap-3">
            <div className="flex items-center gap-2">
              <span>{language === 'zh' ? '每页' : 'Per page'}:</span>
              <NofxSelect
                value={pageSize}
                onChange={(val) => setPageSize(Number(val))}
                options={[
                  { value: 20, label: '20' },
                  { value: 50, label: '50' },
                  { value: 100, label: '100' },
                ]}
                className="h-7 rounded-md border border-line bg-surface-2 px-2 text-xs text-fg"
              />
            </div>

            {totalPages > 1 && (
              <div className="flex items-center gap-1">
                {pageBtn('«', () => setCurrentPage(1), currentPage === 1)}
                {pageBtn(
                  '‹',
                  () => setCurrentPage((p) => Math.max(1, p - 1)),
                  currentPage === 1
                )}
                <span className="num px-2 text-fg">
                  {currentPage} / {totalPages}
                </span>
                {pageBtn(
                  '›',
                  () => setCurrentPage((p) => Math.min(totalPages, p + 1)),
                  currentPage === totalPages
                )}
                {pageBtn(
                  '»',
                  () => setCurrentPage(totalPages),
                  currentPage === totalPages
                )}
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
