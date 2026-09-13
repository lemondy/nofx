import { useState, useEffect, useMemo } from 'react'
import { api } from '../../lib/api'
import { useLanguage } from '../../contexts/LanguageContext'
import { t, type Language } from '../../i18n/translations'
import { MetricTooltip } from '../common/MetricTooltip'
import { formatPrice, formatQuantity } from '../../utils/format'
import { NofxSelect } from '../ui/select'
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

// Stats Card Component with formula tooltip
function StatCard({
 title,
 value,
 suffix,
 color,
 icon,
 subtitle,
 metricKey,
 language = 'en',
}: {
 title: string
 value: string | number
 suffix?: string
 color?: string
 icon: string
 subtitle?: string
 metricKey?: string
 language?: string
}) {
 return (
 <div
 className="rounded-lg p-4 transition-all duration-200 hover:scale-[1.02]"
 style={{
 background: 'linear-gradient(135deg, #E9E4D6 0%, #EBE7DA 100%)',
 border: '1px solid #C0B9A2',
 boxShadow: '0 4px 12px rgba(0, 0, 0, 0.2)',
 }}
 >
 <div className="flex items-center gap-2 mb-2">
 <span className="text-lg">{icon}</span>
 <span className="text-xs" style={{ color: '#6E6E60' }}>
 {title}
 </span>
 {metricKey && (
 <MetricTooltip metricKey={metricKey} language={language} size={12} />
 )}
 </div>
 <div className="flex items-baseline gap-1">
 <span
 className="text-xl font-bold font-mono"
 style={{ color: color || '#1E1E1A' }}
 >
 {value}
 </span>
 {suffix && (
 <span className="text-sm" style={{ color: '#6E6E60' }}>
 {suffix}
 </span>
 )}
 </div>
 {subtitle && (
 <div className="text-xs mt-1" style={{ color: '#6E6E60' }}>
 {subtitle}
 </div>
 )}
 </div>
 )
}

// Symbol Stats Row
function SymbolStatsRow({ stat }: { stat: SymbolStats }) {
 const totalPnl = stat.total_pnl || 0
 const winRate = stat.win_rate || 0
 const pnlColor = totalPnl >= 0 ? '#2E7D4F' : '#C0392B'
 const winRateColor =
 winRate >= 60 ? '#2E7D4F' : winRate >= 40 ? '#B8912A' : '#C0392B'

 return (
 <div
 className="flex items-center justify-between py-1.5 px-3 rounded-lg transition-all duration-200 hover:bg-[#EDE9DC]"
 style={{ borderBottom: '1px solid #C0B9A2' }}
 >
 <div className="flex items-center gap-2">
 <span className="font-mono font-semibold text-sm" style={{ color: '#1E1E1A' }}>
 {(stat.symbol || '').replace('USDT', '')}
 </span>
 <span className="text-[10px]" style={{ color: '#8A8A7C' }}>
 {stat.total_trades || 0} trades
 </span>
 </div>
 <div className="flex items-center gap-4">
 <div className="text-right">
 <div className="text-[10px]" style={{ color: '#8A8A7C' }}>
 Win Rate
 </div>
 <div className="font-mono font-semibold text-sm" style={{ color: winRateColor }}>
 {winRate.toFixed(1)}%
 </div>
 </div>
 <div className="text-right min-w-[70px]">
 <div className="text-[10px]" style={{ color: '#8A8A7C' }}>
 P&L
 </div>
 <div className="font-mono font-semibold text-sm" style={{ color: pnlColor }}>
 {totalPnl >= 0 ? '+' : ''}
 {formatNumber(totalPnl)}
 </div>
 </div>
 </div>
 </div>
 )
}

// Direction Stats Card
function DirectionStatsCard({ stat, language }: { stat: DirectionStats; language: Language }) {
 const isLong = (stat.side || '').toLowerCase() === 'long'
 const iconColor = isLong ? '#2E7D4F' : '#C0392B'
 const totalPnl = stat.total_pnl || 0
 const winRate = stat.win_rate || 0
 const tradeCount = stat.trade_count || 0
 const avgPnl = stat.avg_pnl || 0
 const pnlColor = totalPnl >= 0 ? '#2E7D4F' : '#C0392B'

 return (
 <div
 className="rounded-lg p-3"
 style={{
 background: 'linear-gradient(135deg, #E9E4D6 0%, #EBE7DA 100%)',
 border: `1px solid ${iconColor}33`,
 }}
 >
 <div className="flex items-center gap-2 mb-3">
 <span className="text-xl">{isLong ? '📈' : '📉'}</span>
 <span
 className="font-bold uppercase"
 style={{ color: iconColor }}
 >
 {stat.side || 'Unknown'}
 </span>
 </div>
 <div className="grid grid-cols-4 gap-4">
 <div>
 <div className="text-xs mb-1" style={{ color: '#6E6E60' }}>
 {t('positionHistory.trades', language)}
 </div>
 <div className="font-mono font-semibold" style={{ color: '#1E1E1A' }}>
 {tradeCount}
 </div>
 </div>
 <div>
 <div className="text-xs mb-1" style={{ color: '#6E6E60' }}>
 {t('positionHistory.winRate', language)}
 </div>
 <div
 className="font-mono font-semibold"
 style={{
 color:
 winRate >= 60
 ? '#2E7D4F'
 : winRate >= 40
 ? '#B8912A'
 : '#C0392B',
 }}
 >
 {winRate.toFixed(1)}%
 </div>
 </div>
 <div>
 <div className="text-xs mb-1" style={{ color: '#6E6E60' }}>
 {t('positionHistory.totalPnL', language)}
 </div>
 <div className="font-mono font-semibold" style={{ color: pnlColor }}>
 {totalPnl >= 0 ? '+' : ''}
 {formatNumber(totalPnl)}
 </div>
 </div>
 <div>
 <div className="text-xs mb-1" style={{ color: '#6E6E60' }}>
 {t('positionHistory.avgPnL', language)}
 </div>
 <div className="font-mono font-semibold" style={{ color: avgPnl >= 0 ? '#2E7D4F' : '#C0392B' }}>
 {avgPnl >= 0 ? '+' : ''}
 {formatNumber(avgPnl)}
 </div>
 </div>
 </div>
 </div>
 )
}

// Position Row Component
function PositionRow({ position }: { position: HistoricalPosition }) {
 const side = position.side || ''
 const isLong = side.toUpperCase() === 'LONG'
 const realizedPnl = position.realized_pnl || 0
 const isProfitable = realizedPnl >= 0
 const sideColor = isLong ? '#2E7D4F' : '#C0392B'
 const pnlColor = isProfitable ? '#2E7D4F' : '#C0392B'

 // Calculate holding time
 const entryTime = position.entry_time ? new Date(position.entry_time).getTime() : 0
 const exitTime = position.exit_time ? new Date(position.exit_time).getTime() : 0
 const holdingMinutes = entryTime && exitTime && exitTime > entryTime ? (exitTime - entryTime) / 60000 : 0

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

 return (
 <tr
 className="transition-all duration-200 hover:bg-[#1E1E1A]/5"
 style={{ borderBottom: '1px solid #C0B9A2' }}
 >
 {/* Symbol */}
 <td className="py-1.5 px-3">
 <div className="flex items-center gap-2">
 <span className="font-mono font-semibold" style={{ color: '#1E1E1A' }}>
 {(position.symbol || '').replace('USDT', '')}
 </span>
 <span
 className="px-2 py-0.5 rounded text-xs font-semibold uppercase"
 style={{
 background: `${sideColor}22`,
 color: sideColor,
 border: `1px solid ${sideColor}44`,
 }}
 >
 {side}
 </span>
 </div>
 </td>

 {/* Entry Price */}
 <td className="py-1.5 px-3 text-right font-mono text-sm" style={{ color: '#1E1E1A' }}>
 {formatPrice(entryPrice)}
 </td>

 {/* Exit Price */}
 <td className="py-1.5 px-3 text-right font-mono text-sm" style={{ color: '#1E1E1A' }}>
 {formatPrice(exitPrice)}
 </td>

 {/* Quantity */}
 <td className="py-1.5 px-3 text-right font-mono text-sm" style={{ color: '#6E6E60' }}>
 {formatQuantity(displayQty)}
 </td>

 {/* Position Value (Entry Price * Quantity) */}
 <td className="py-1.5 px-3 text-right font-mono text-sm" style={{ color: '#1E1E1A' }}>
 {formatNumber(entryPrice * displayQty)}
 </td>

 {/* P&L */}
 <td className="py-1.5 px-3 text-right text-sm">
 <div className="font-mono font-semibold" style={{ color: pnlColor }}>
 {isProfitable ? '+' : ''}
 {formatNumber(realizedPnl)}
 </div>
 <div className="text-xs" style={{ color: pnlColor }}>
 {pnlPct >= 0 ? '+' : ''}
 {pnlPct.toFixed(2)}%
 </div>
 </td>

 {/* Fee - show more precision for small fees */}
 <td className="py-1.5 px-3 text-right font-mono text-xs" style={{ color: '#6E6E60' }}>
 -{((position.fee || 0) < 0.01 && (position.fee || 0) > 0)
 ? (position.fee || 0).toFixed(4)
 : (position.fee || 0).toFixed(2)}
 </td>

 {/* Duration */}
 <td className="py-1.5 px-3 text-center text-xs" style={{ color: '#6E6E60' }}>
 {formatDuration(holdingMinutes)}
 </td>

 {/* Exit Time */}
 <td className="py-1.5 px-3 text-right text-xs" style={{ color: '#6E6E60' }}>
 {formatDate(position.exit_time)}
 </td>
 </tr>
 )
}

export function PositionHistory({ traderId }: PositionHistoryProps) {
 const { language } = useLanguage()
 const [loading, setLoading] = useState(true)
 const [error, setError] = useState<string | null>(null)
 const [positions, setPositions] = useState<HistoricalPosition[]>([])
 const [stats, setStats] = useState<TraderStats | null>(null)
 const [symbolStats, setSymbolStats] = useState<SymbolStats[]>([])
 const [directionStats, setDirectionStats] = useState<DirectionStats[]>([])

 // Pagination state
 const [pageSize, setPageSize] = useState<number>(20)
 const [currentPage, setCurrentPage] = useState<number>(1)

 // Filter state
 const [filterSymbol, setFilterSymbol] = useState<string>('all')
 const [filterSide, setFilterSide] = useState<string>('all')
 const [sortBy, setSortBy] = useState<'time' | 'pnl' | 'pnl_pct'>('time')
 const [sortOrder, setSortOrder] = useState<'asc' | 'desc'>('desc')

 useEffect(() => {
 const fetchData = async () => {
 try {
 setLoading(true)
 setError(null)
 // Fetch more data than needed to support filtering, but respect pageSize for initial load
 const data = await api.getPositionHistory(
 traderId,
 Math.max(200, pageSize * 5),
 true
 )
 setPositions(data.positions || [])
 setStats(data.stats)
 setSymbolStats(data.symbol_stats || [])
 setDirectionStats(data.direction_stats || [])
 } catch (err) {
 setError(err instanceof Error ? err.message : 'Failed to load history')
 } finally {
 setLoading(false)
 }
 }

 if (traderId) {
 fetchData()
 }
 }, [traderId, pageSize])

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
 new Date(a.exit_time || 0).getTime() - new Date(b.exit_time || 0).getTime()
 break
 case 'pnl':
 comparison = (a.realized_pnl || 0) - (b.realized_pnl || 0)
 break
 case 'pnl_pct': {
 const aPrice = a.entry_price || 1
 const bPrice = b.entry_price || 1
 const aPct = ((a.exit_price || 0) - aPrice) / aPrice * 100
 const bPct = ((b.exit_price || 0) - bPrice) / bPrice * 100
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
 <div
 className="flex items-center justify-center p-12"
 style={{ color: '#6E6E60' }}
 >
 <div className="animate-spin mr-3">
 <svg className="w-6 h-6" fill="none" viewBox="0 0 24 24">
 <circle
 className="opacity-25"
 cx="12"
 cy="12"
 r="10"
 stroke="currentColor"
 strokeWidth="4"
 />
 <path
 className="opacity-75"
 fill="currentColor"
 d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"
 />
 </svg>
 </div>
 {t('positionHistory.loading', language)}
 </div>
 )
 }

 if (error) {
 return (
 <div
 className="rounded-lg p-6 text-center"
 style={{
 background: 'rgba(192, 57, 43, 0.1)',
 border: '1px solid rgba(192, 57, 43, 0.3)',
 color: '#C0392B',
 }}
 >
 {error}
 </div>
 )
 }

 if (positions.length === 0) {
 return (
 <div
 className="rounded-lg p-12 text-center"
 style={{
 background: 'linear-gradient(135deg, #E9E4D6 0%, #EBE7DA 100%)',
 border: '1px solid #C0B9A2',
 }}
 >
 <div className="text-4xl mb-4">📊</div>
 <div className="text-lg font-semibold mb-2" style={{ color: '#1E1E1A' }}>
 {t('positionHistory.noHistory', language)}
 </div>
 <div style={{ color: '#6E6E60' }}>
 {t('positionHistory.noHistoryDesc', language)}
 </div>
 </div>
 )
 }

 return (
 <div className="space-y-6">
 {/* Overall Stats - Row 1: Core Metrics */}
 {stats && (
 <div className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-5 gap-4">
 <StatCard
 icon="📊"
 title={t('positionHistory.totalTrades', language)}
 value={stats.total_trades || 0}
 subtitle={t('positionHistory.winLoss', language, { win: stats.win_trades || 0, loss: stats.loss_trades || 0 })}
 language={language}
 />
 <StatCard
 icon="🎯"
 title={t('positionHistory.winRate', language)}
 value={(stats.win_rate || 0).toFixed(1)}
 suffix="%"
 color={
 (stats.win_rate || 0) >= 60
 ? '#2E7D4F'
 : (stats.win_rate || 0) >= 40
 ? '#B8912A'
 : '#C0392B'
 }
 metricKey="win_rate"
 language={language}
 />
 <StatCard
 icon="💰"
 title={t('positionHistory.totalPnL', language)}
 value={((stats.total_pnl || 0) >= 0 ? '+' : '') + formatNumber(stats.total_pnl || 0)}
 color={(stats.total_pnl || 0) >= 0 ? '#2E7D4F' : '#C0392B'}
 subtitle={`${t('positionHistory.fee', language)}: -${formatNumber(stats.total_fee || 0)}`}
 metricKey="total_return"
 language={language}
 />
 <StatCard
 icon="📈"
 title={t('positionHistory.profitFactor', language)}
 value={(stats.profit_factor || 0).toFixed(2)}
 color={(stats.profit_factor || 0) >= 1.5 ? '#2E7D4F' : (stats.profit_factor || 0) >= 1 ? '#B8912A' : '#C0392B'}
 subtitle={t('positionHistory.profitFactorDesc', language)}
 metricKey="profit_factor"
 language={language}
 />
 <StatCard
 icon="⚖️"
 title={t('positionHistory.plRatio', language)}
 value={profitLossRatio === Infinity ? '∞' : profitLossRatio.toFixed(2)}
 color={profitLossRatio >= 1.5 ? '#2E7D4F' : profitLossRatio >= 1 ? '#B8912A' : '#C0392B'}
 subtitle={t('positionHistory.plRatioDesc', language)}
 metricKey="expectancy"
 language={language}
 />
 </div>
 )}

 {/* Overall Stats - Row 2: Advanced Metrics */}
 {stats && (
 <div className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-5 gap-4">
 <StatCard
 icon="📉"
 title={t('positionHistory.sharpeRatio', language)}
 value={(stats.sharpe_ratio || 0).toFixed(2)}
 color={(stats.sharpe_ratio || 0) >= 1 ? '#2E7D4F' : (stats.sharpe_ratio || 0) >= 0 ? '#B8912A' : '#C0392B'}
 subtitle={t('positionHistory.sharpeRatioDesc', language)}
 metricKey="sharpe_ratio"
 language={language}
 />
 <StatCard
 icon="🔻"
 title={t('positionHistory.maxDrawdown', language)}
 value={(stats.max_drawdown_pct || 0).toFixed(1)}
 suffix="%"
 color={(stats.max_drawdown_pct || 0) <= 10 ? '#2E7D4F' : (stats.max_drawdown_pct || 0) <= 20 ? '#B8912A' : '#C0392B'}
 metricKey="max_drawdown"
 language={language}
 />
 <StatCard
 icon="🏆"
 title={t('positionHistory.avgWin', language)}
 value={'+' + formatNumber(stats.avg_win || 0)}
 color="#2E7D4F"
 metricKey="avg_trade_pnl"
 language={language}
 />
 <StatCard
 icon="💸"
 title={t('positionHistory.avgLoss', language)}
 value={'-' + formatNumber(stats.avg_loss || 0)}
 color="#C0392B"
 language={language}
 />
 <StatCard
 icon="💵"
 title={t('positionHistory.netPnL', language)}
 value={((stats.total_pnl || 0) - (stats.total_fee || 0) >= 0 ? '+' : '') + formatNumber((stats.total_pnl || 0) - (stats.total_fee || 0))}
 color={(stats.total_pnl || 0) - (stats.total_fee || 0) >= 0 ? '#2E7D4F' : '#C0392B'}
 subtitle={t('positionHistory.netPnLDesc', language)}
 language={language}
 />
 </div>
 )}

 {/* Direction Stats */}
 {directionStats.length > 0 && (
 <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
 {directionStats.map((stat) => (
 <DirectionStatsCard key={stat.side} stat={stat} language={language} />
 ))}
 </div>
 )}

 {/* Symbol Performance */}
 {symbolStats.length > 0 && (
 <div
 className="rounded-lg p-3"
 style={{
 background: 'linear-gradient(135deg, #E9E4D6 0%, #EBE7DA 100%)',
 border: '1px solid #C0B9A2',
 }}
 >
 <div className="flex items-center gap-2 mb-4">
 <span className="text-lg">🏅</span>
 <span className="font-semibold" style={{ color: '#1E1E1A' }}>
 {t('positionHistory.symbolPerformance', language)}
 </span>
 </div>
 <div className="space-y-1">
 {symbolStats.slice(0, 10).map((stat) => (
 <SymbolStatsRow key={stat.symbol} stat={stat} />
 ))}
 </div>
 </div>
 )}

 {/* Position List */}
 <div
 className="rounded-lg overflow-hidden"
 style={{
 background: 'linear-gradient(135deg, #E9E4D6 0%, #EBE7DA 100%)',
 border: '1px solid #C0B9A2',
 }}
 >
 {/* Filters */}
 <div
 className="flex flex-wrap items-center gap-4 p-4"
 style={{ borderBottom: '1px solid #C0B9A2' }}
 >
 <div className="flex items-center gap-2">
 <span className="text-sm" style={{ color: '#6E6E60' }}>
 {t('positionHistory.symbol', language)}:
 </span>
 <NofxSelect
 value={filterSymbol}
 onChange={(val) => setFilterSymbol(val)}
 options={[
 { value: 'all', label: t('positionHistory.allSymbols', language) },
 ...uniqueSymbols.map(s => ({ value: s, label: (s || '').replace('USDT', '') }))
 ]}
 className="rounded px-3 py-1.5 text-sm"
 style={{
 background: '#F2EFE6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 </div>

 <div className="flex items-center gap-2">
 <span className="text-sm" style={{ color: '#6E6E60' }}>
 {t('positionHistory.side', language)}:
 </span>
 <div className="flex rounded overflow-hidden" style={{ border: '1px solid #C0B9A2' }}>
 {['all', 'LONG', 'SHORT'].map((side) => (
 <button
 key={side}
 onClick={() => setFilterSide(side)}
 className="px-3 py-1.5 text-sm capitalize transition-colors"
 style={{
 background: filterSide === side ? '#C0B9A2' : 'transparent',
 color: filterSide === side ? '#1E1E1A' : '#6E6E60',
 }}
 >
 {side === 'all' ? t('positionHistory.all', language) : side}
 </button>
 ))}
 </div>
 </div>

 <div className="flex items-center gap-2 ml-auto">
 <span className="text-sm" style={{ color: '#6E6E60' }}>
 {t('positionHistory.sort', language)}:
 </span>
 <NofxSelect
 value={`${sortBy}-${sortOrder}`}
 onChange={(val) => {
 const [by, order] = val.split('-') as ['time' | 'pnl' | 'pnl_pct', 'asc' | 'desc']
 setSortBy(by)
 setSortOrder(order)
 }}
 options={[
 { value: 'time-desc', label: t('positionHistory.latestFirst', language) },
 { value: 'time-asc', label: t('positionHistory.oldestFirst', language) },
 { value: 'pnl-desc', label: t('positionHistory.highestPnL', language) },
 { value: 'pnl-asc', label: t('positionHistory.lowestPnL', language) },
 ]}
 className="rounded px-3 py-1.5 text-sm"
 style={{
 background: '#F2EFE6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 </div>
 </div>

 {/* Table */}
 <div className="overflow-x-auto">
 <table className="w-full">
 <thead>
 <tr style={{ background: '#F2EFE6' }}>
 <th
 className="py-3 px-4 text-left text-xs font-semibold uppercase tracking-wider"
 style={{ color: '#6E6E60' }}
 >
 {t('positionHistory.symbol', language)}
 </th>
 <th
 className="py-3 px-4 text-right text-xs font-semibold uppercase tracking-wider"
 style={{ color: '#6E6E60' }}
 >
 {t('positionHistory.entry', language)}
 </th>
 <th
 className="py-3 px-4 text-right text-xs font-semibold uppercase tracking-wider"
 style={{ color: '#6E6E60' }}
 >
 {t('positionHistory.exit', language)}
 </th>
 <th
 className="py-3 px-4 text-right text-xs font-semibold uppercase tracking-wider"
 style={{ color: '#6E6E60' }}
 >
 {t('positionHistory.qty', language)}
 </th>
 <th
 className="py-3 px-4 text-right text-xs font-semibold uppercase tracking-wider"
 style={{ color: '#6E6E60' }}
 >
 {t('positionHistory.value', language)}
 </th>
 <th
 className="py-3 px-4 text-right text-xs font-semibold uppercase tracking-wider"
 style={{ color: '#6E6E60' }}
 >
 {t('positionHistory.pnl', language)}
 </th>
 <th
 className="py-3 px-4 text-right text-xs font-semibold uppercase tracking-wider"
 style={{ color: '#6E6E60' }}
 >
 {t('positionHistory.fee', language)}
 </th>
 <th
 className="py-3 px-4 text-center text-xs font-semibold uppercase tracking-wider"
 style={{ color: '#6E6E60' }}
 >
 {t('positionHistory.duration', language)}
 </th>
 <th
 className="py-3 px-4 text-right text-xs font-semibold uppercase tracking-wider"
 style={{ color: '#6E6E60' }}
 >
 {t('positionHistory.closedAt', language)}
 </th>
 </tr>
 </thead>
 <tbody>
 {filteredPositions.map((position) => (
 <PositionRow key={position.id} position={position} />
 ))}
 </tbody>
 </table>
 </div>

 {/* Footer with Pagination */}
 <div
 className="flex flex-wrap items-center justify-between gap-4 p-4 text-sm"
 style={{ borderTop: '1px solid #C0B9A2', color: '#6E6E60' }}
 >
 {/* Left: Count info */}
 <div className="flex items-center gap-4">
 <span>
 {t('positionHistory.showingPositions', language, { count: totalFilteredCount, total: positions.length })}
 </span>
 {totalFilteredCount > 0 && (
 <span>
 {t('positionHistory.totalPnL', language)}:{' '}
 <span
 style={{
 color:
 filteredAndSortedPositions.reduce((sum, p) => sum + (p.realized_pnl || 0), 0) >= 0
 ? '#2E7D4F'
 : '#C0392B',
 }}
 >
 {filteredAndSortedPositions.reduce((sum, p) => sum + (p.realized_pnl || 0), 0) >= 0
 ? '+'
 : ''}
 {formatNumber(
 filteredAndSortedPositions.reduce((sum, p) => sum + (p.realized_pnl || 0), 0)
 )}
 </span>
 </span>
 )}
 </div>

 {/* Right: Pagination controls */}
 <div className="flex items-center gap-3">
 {/* Page size selector */}
 <div className="flex items-center gap-2">
 <span className="text-xs" style={{ color: '#6E6E60' }}>
 {language === 'zh' ? '每页' : 'Per page'}:
 </span>
 <NofxSelect
 value={pageSize}
 onChange={(val) => setPageSize(Number(val))}
 options={[
 { value: 20, label: '20' },
 { value: 50, label: '50' },
 { value: 100, label: '100' },
 ]}
 className="rounded px-2 py-1 text-sm"
 style={{
 background: '#F2EFE6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 </div>

 {/* Page navigation */}
 {totalPages > 1 && (
 <div className="flex items-center gap-1">
 <button
 onClick={() => setCurrentPage(1)}
 disabled={currentPage === 1}
 className="px-2 py-1 rounded text-xs transition-colors disabled:opacity-30"
 style={{
 background: currentPage === 1 ? 'transparent' : '#C0B9A2',
 color: '#1E1E1A',
 }}
 >
 «
 </button>
 <button
 onClick={() => setCurrentPage((p) => Math.max(1, p - 1))}
 disabled={currentPage === 1}
 className="px-2 py-1 rounded text-xs transition-colors disabled:opacity-30"
 style={{
 background: currentPage === 1 ? 'transparent' : '#C0B9A2',
 color: '#1E1E1A',
 }}
 >
 ‹
 </button>
 <span className="px-3 text-xs" style={{ color: '#1E1E1A' }}>
 {currentPage} / {totalPages}
 </span>
 <button
 onClick={() => setCurrentPage((p) => Math.min(totalPages, p + 1))}
 disabled={currentPage === totalPages}
 className="px-2 py-1 rounded text-xs transition-colors disabled:opacity-30"
 style={{
 background: currentPage === totalPages ? 'transparent' : '#C0B9A2',
 color: '#1E1E1A',
 }}
 >
 ›
 </button>
 <button
 onClick={() => setCurrentPage(totalPages)}
 disabled={currentPage === totalPages}
 className="px-2 py-1 rounded text-xs transition-colors disabled:opacity-30"
 style={{
 background: currentPage === totalPages ? 'transparent' : '#C0B9A2',
 color: '#1E1E1A',
 }}
 >
 »
 </button>
 </div>
 )}
 </div>
 </div>
 </div>
 </div>
 )
}
