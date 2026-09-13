import { useState, useEffect, useCallback } from 'react'
import { Shield, TrendingUp, AlertTriangle, Activity, Box, ChevronDown, ChevronUp } from 'lucide-react'
import type { GridRiskInfo } from '../../types'
import { gridRisk, ts } from '../../i18n/strategy-translations'

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
 case 'narrow': return '#2E7D4F'
 case 'standard': return '#B8912A'
 case 'wide': return '#F7931A'
 case 'volatile': return '#C0392B'
 case 'trending': return '#8B5CF6'
 default: return '#6E6E60'
 }
 }

 const getBreakoutColor = (level: string) => {
 switch (level) {
 case 'none': return '#2E7D4F'
 case 'short': return '#B8912A'
 case 'mid': return '#F7931A'
 case 'long': return '#C0392B'
 default: return '#6E6E60'
 }
 }

 const getPositionColor = (percent: number) => {
 if (percent < 50) return '#2E7D4F'
 if (percent < 80) return '#B8912A'
 return '#C0392B'
 }

 const formatPrice = (price: number) => {
 if (price === 0) return '-'
 if (price >= 1000) return price.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
 if (price >= 1) return price.toFixed(4)
 return price.toFixed(6)
 }

 const formatUSD = (value: number) => {
 return `$${value.toLocaleString('en-US', { minimumFractionDigits: 0, maximumFractionDigits: 0 })}`
 }

 const cardStyle = {
 background: '#F2EFE6',
 border: '1px solid #C0B9A2',
 }

 if (loading) {
 return (
 <div className="p-3 text-center text-xs" style={{ color: '#6E6E60' }}>
 {ts(gridRisk.loading, language)}
 </div>
 )
 }

 if (error) {
 return (
 <div className="p-3 text-center text-xs" style={{ color: '#C0392B' }}>
 {ts(gridRisk.error, language)}: {error}
 </div>
 )
 }

 if (!riskInfo) {
 return (
 <div className="p-3 text-center text-xs" style={{ color: '#6E6E60' }}>
 {ts(gridRisk.noData, language)}
 </div>
 )
 }

 return (
 <div className="rounded-lg" style={cardStyle}>
 {/* Collapsible Header */}
 <div
 className="flex items-center justify-between p-3 cursor-pointer hover:bg-[#E9E4D6] transition-colors"
 onClick={() => setExpanded(!expanded)}
 >
 <div className="flex items-center gap-2">
 <Shield className="w-4 h-4" style={{ color: '#B8912A' }} />
 <span className="font-medium text-sm" style={{ color: '#1E1E1A' }}>
 {ts(gridRisk.gridRisk, language)}
 </span>
 </div>
 <div className="flex items-center gap-3">
 {/* Summary badges when collapsed */}
 <div className="flex items-center gap-2 text-xs">
 <span
 className="px-2 py-0.5 rounded"
 style={{ background: getRegimeColor(riskInfo.regime_level) + '20', color: getRegimeColor(riskInfo.regime_level) }}
 >
 {ts(gridRisk[(riskInfo.regime_level || 'standard') as keyof typeof gridRisk], language)}
 </span>
 <span className="font-mono" style={{ color: '#1E1E1A' }}>
 {riskInfo.effective_leverage.toFixed(1)}x
 </span>
 <span
 className="font-mono"
 style={{ color: getPositionColor(riskInfo.position_percent) }}
 >
 {riskInfo.position_percent.toFixed(0)}%
 </span>
 </div>
 {expanded ? (
 <ChevronUp className="w-4 h-4" style={{ color: '#6E6E60' }} />
 ) : (
 <ChevronDown className="w-4 h-4" style={{ color: '#6E6E60' }} />
 )}
 </div>
 </div>

 {/* Expanded Content */}
 {expanded && (
 <div className="px-3 pb-3 space-y-3">
 {/* Row 1: Leverage & Position */}
 <div className="grid grid-cols-2 gap-3">
 {/* Leverage */}
 <div className="p-2 rounded" style={{ background: '#E9E4D6' }}>
 <div className="flex items-center gap-1 mb-2">
 <TrendingUp className="w-3 h-3" style={{ color: '#B8912A' }} />
 <span className="text-xs font-medium" style={{ color: '#6E6E60' }}>{ts(gridRisk.leverageInfo, language)}</span>
 </div>
 <div className="grid grid-cols-3 gap-1 text-xs">
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.currentLeverage, language)}</div>
 <div className="font-mono" style={{ color: '#1E1E1A' }}>{riskInfo.current_leverage}x</div>
 </div>
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.effectiveLeverage, language)}</div>
 <div className="font-mono" style={{ color: '#B8912A' }}>{riskInfo.effective_leverage.toFixed(2)}x</div>
 </div>
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.recommendedLeverage, language)}</div>
 <div
 className="font-mono"
 style={{ color: riskInfo.current_leverage > riskInfo.recommended_leverage ? '#C0392B' : '#2E7D4F' }}
 >
 {riskInfo.recommended_leverage}x
 </div>
 </div>
 </div>
 </div>

 {/* Position */}
 <div className="p-2 rounded" style={{ background: '#E9E4D6' }}>
 <div className="flex items-center gap-1 mb-2">
 <Activity className="w-3 h-3" style={{ color: '#B8912A' }} />
 <span className="text-xs font-medium" style={{ color: '#6E6E60' }}>{ts(gridRisk.positionInfo, language)}</span>
 </div>
 <div className="grid grid-cols-3 gap-1 text-xs">
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.currentPosition, language)}</div>
 <div className="font-mono" style={{ color: '#1E1E1A' }}>{formatUSD(riskInfo.current_position)}</div>
 </div>
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.maxPosition, language)}</div>
 <div className="font-mono" style={{ color: '#1E1E1A' }}>{formatUSD(riskInfo.max_position)}</div>
 </div>
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.positionPercent, language)}</div>
 <div className="font-mono" style={{ color: getPositionColor(riskInfo.position_percent) }}>
 {riskInfo.position_percent.toFixed(1)}%
 </div>
 </div>
 </div>
 {/* Mini progress bar */}
 <div className="h-1 mt-2 rounded-full overflow-hidden" style={{ background: '#C0B9A2' }}>
 <div
 className="h-full rounded-full"
 style={{ width: `${Math.min(riskInfo.position_percent, 100)}%`, background: getPositionColor(riskInfo.position_percent) }}
 />
 </div>
 </div>
 </div>

 {/* Row 2: Market State & Liquidation */}
 <div className="grid grid-cols-2 gap-3">
 {/* Market State */}
 <div className="p-2 rounded" style={{ background: '#E9E4D6' }}>
 <div className="flex items-center gap-1 mb-2">
 <Shield className="w-3 h-3" style={{ color: '#B8912A' }} />
 <span className="text-xs font-medium" style={{ color: '#6E6E60' }}>{ts(gridRisk.marketState, language)}</span>
 </div>
 <div className="grid grid-cols-2 gap-2 text-xs">
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.regimeLevel, language)}</div>
 <div className="font-medium" style={{ color: getRegimeColor(riskInfo.regime_level) }}>
 {ts(gridRisk[(riskInfo.regime_level || 'standard') as keyof typeof gridRisk], language)}
 </div>
 </div>
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.currentPrice, language)}</div>
 <div className="font-mono" style={{ color: '#1E1E1A' }}>{formatPrice(riskInfo.current_price)}</div>
 </div>
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.breakoutLevel, language)}</div>
 <div className="font-medium" style={{ color: getBreakoutColor(riskInfo.breakout_level) }}>
 {ts(gridRisk[(riskInfo.breakout_level || 'none') as keyof typeof gridRisk], language)}
 </div>
 </div>
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.breakoutDirection, language)}</div>
 <div
 className="font-medium"
 style={{ color: riskInfo.breakout_direction === 'up' ? '#2E7D4F' : riskInfo.breakout_direction === 'down' ? '#C0392B' : '#6E6E60' }}
 >
 {riskInfo.breakout_direction ? ts(gridRisk[riskInfo.breakout_direction as keyof typeof gridRisk], language) : '-'}
 </div>
 </div>
 </div>
 </div>

 {/* Liquidation */}
 <div className="p-2 rounded" style={{ background: '#E9E4D6' }}>
 <div className="flex items-center gap-1 mb-2">
 <AlertTriangle className="w-3 h-3" style={{ color: '#C0392B' }} />
 <span className="text-xs font-medium" style={{ color: '#6E6E60' }}>{ts(gridRisk.liquidationInfo, language)}</span>
 </div>
 <div className="grid grid-cols-2 gap-2 text-xs">
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.liquidationPrice, language)}</div>
 <div className="font-mono" style={{ color: '#C0392B' }}>
 {riskInfo.liquidation_price > 0 ? formatPrice(riskInfo.liquidation_price) : '-'}
 </div>
 </div>
 <div>
 <div style={{ color: '#8A8A7C' }}>{ts(gridRisk.liquidationDistance, language)}</div>
 <div className="font-mono" style={{ color: '#C0392B' }}>
 {riskInfo.liquidation_distance > 0 ? `${riskInfo.liquidation_distance.toFixed(1)}%` : '-'}
 </div>
 </div>
 </div>
 </div>
 </div>

 {/* Row 3: Box State */}
 <div className="p-2 rounded" style={{ background: '#E9E4D6' }}>
 <div className="flex items-center gap-1 mb-2">
 <Box className="w-3 h-3" style={{ color: '#B8912A' }} />
 <span className="text-xs font-medium" style={{ color: '#6E6E60' }}>{ts(gridRisk.boxState, language)}</span>
 </div>
 <div className="grid grid-cols-3 gap-2 text-xs">
 <div className="flex justify-between">
 <span style={{ color: '#8A8A7C' }}>{ts(gridRisk.shortBox, language)}</span>
 <span className="font-mono" style={{ color: '#1E1E1A' }}>
 {formatPrice(riskInfo.short_box_lower)} - {formatPrice(riskInfo.short_box_upper)}
 </span>
 </div>
 <div className="flex justify-between">
 <span style={{ color: '#8A8A7C' }}>{ts(gridRisk.midBox, language)}</span>
 <span className="font-mono" style={{ color: '#1E1E1A' }}>
 {formatPrice(riskInfo.mid_box_lower)} - {formatPrice(riskInfo.mid_box_upper)}
 </span>
 </div>
 <div className="flex justify-between">
 <span style={{ color: '#8A8A7C' }}>{ts(gridRisk.longBox, language)}</span>
 <span className="font-mono" style={{ color: '#1E1E1A' }}>
 {formatPrice(riskInfo.long_box_lower)} - {formatPrice(riskInfo.long_box_upper)}
 </span>
 </div>
 </div>
 </div>
 </div>
 )}
 </div>
 )
}
