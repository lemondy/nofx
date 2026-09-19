import { useState } from 'react'
import {
 Plus,
 X,
 Database,
 TrendingUp,
 TrendingDown,
 List,
 Ban,
 Zap,
 Shuffle,
 ArrowDownRight,
} from 'lucide-react'
import type { CoinSourceConfig } from '../../types'
import { coinSource, ts } from '../../i18n/strategy-translations'
import { NofxSelect } from '../ui/select'

interface CoinSourceEditorProps {
 config: CoinSourceConfig
 onChange: (config: CoinSourceConfig) => void
 disabled?: boolean
 language: string
}

export function CoinSourceEditor({
 config,
 onChange,
 disabled,
 language,
}: CoinSourceEditorProps) {
 const [newCoin, setNewCoin] = useState('')
 const [newExcludedCoin, setNewExcludedCoin] = useState('')

 const sourceTypes = [
 { value: 'static', icon: List, color: '#6E6E60' },
 { value: 'ai500', icon: Database, color: '#B8912A' },
 { value: 'oi_top', icon: TrendingUp, color: '#2E7D4F' },
 { value: 'oi_low', icon: TrendingDown, color: '#C0392B' },
 { value: 'piggy_dash', icon: Zap, color: '#EC4899' },
 { value: 'short_scan', icon: ArrowDownRight, color: '#EF4444' },
 ] as const

 type SourceKey = (typeof sourceTypes)[number]['value']

 // The set of sources currently selected, derived from the config. A single
 // source maps to its own source_type; 2+ map to mixed + use_* flags.
 const getSelectedSources = (): SourceKey[] => {
 if (config.source_type === 'mixed') {
 const s: SourceKey[] = []
 if (config.use_ai500) s.push('ai500')
 if (config.use_oi_top) s.push('oi_top')
 if (config.use_oi_low) s.push('oi_low')
 if (config.use_piggy_dash) s.push('piggy_dash')
 if (config.use_short_scan) s.push('short_scan')
 // Legacy mixed configs (no use_static flag) always included static coins
 if (config.use_static !== false) s.push('static')
 return s.length > 0 ? s : ['static']
 }
 if (sourceTypes.some((t) => t.value === config.source_type)) {
 return [config.source_type as SourceKey]
 }
 return []
 }

 // Map a source selection back to the persisted config shape: one source
 // keeps its dedicated source_type, several combine into mixed mode.
 const buildConfig = (sources: SourceKey[]): CoinSourceConfig => {
 const base = { ...config }
 if (sources.length === 1) {
 const t = sources[0]
 return {
 ...base,
 source_type: t as CoinSourceConfig['source_type'],
 use_static: undefined,
 use_ai500: t === 'ai500',
 use_oi_top: t === 'oi_top',
 use_oi_low: t === 'oi_low',
 use_piggy_dash: t === 'piggy_dash',
 use_short_scan: t === 'short_scan',
 }
 }
 return {
 ...base,
 source_type: 'mixed',
 use_static: sources.includes('static'),
 use_ai500: sources.includes('ai500'),
 use_oi_top: sources.includes('oi_top'),
 use_oi_low: sources.includes('oi_low'),
 use_piggy_dash: sources.includes('piggy_dash'),
 use_short_scan: sources.includes('short_scan'),
 }
 }

 const selectedSources = getSelectedSources()
 const isMixed = config.source_type === 'mixed'

 const toggleSource = (value: SourceKey) => {
 if (disabled) return
 let next: SourceKey[]
 if (selectedSources.includes(value)) {
 // Keep at least one source selected
 if (selectedSources.length === 1) return
 next = selectedSources.filter((s) => s !== value)
 } else {
 next = [...selectedSources, value]
 }
 onChange(buildConfig(next))
 }

 // Calculate mixed mode summary
 const getMixedSummary = () => {
 const sources: string[] = []
 let totalLimit = 0

 if (config.use_ai500) {
 sources.push(`AI500(${config.ai500_limit || 3})`)
 totalLimit += config.ai500_limit || 3
 }
 if (config.use_oi_top) {
 sources.push(
 `${ts(coinSource.oiIncreaseShort, language)}(${config.oi_top_limit || 3})`
 )
 totalLimit += config.oi_top_limit || 3
 }
 if (config.use_oi_low) {
 sources.push(
 `${ts(coinSource.oiDecreaseShort, language)}(${config.oi_low_limit || 3})`
 )
 totalLimit += config.oi_low_limit || 3
 }
 if (config.use_piggy_dash) {
 sources.push(
 `${ts(coinSource.piggyDash, language)}(${config.piggy_dash_limit || 5})`
 )
 totalLimit += config.piggy_dash_limit || 5
 }
 if (config.use_short_scan) {
 sources.push(
 `${ts(coinSource.shortScan, language)}(${config.short_scan_limit || 5})`
 )
 totalLimit += config.short_scan_limit || 5
 }
 if (config.use_static !== false && (config.static_coins || []).length > 0) {
 sources.push(
 `${ts(coinSource.custom, language)}(${config.static_coins?.length || 0})`
 )
 totalLimit += config.static_coins?.length || 0
 }

 return { sources, totalLimit }
 }

 // xyz dex assets (stocks, forex, commodities) - should NOT get USDT suffix
 const xyzDexAssets = new Set([
 // Stocks
 'TSLA',
 'NVDA',
 'AAPL',
 'MSFT',
 'META',
 'AMZN',
 'GOOGL',
 'AMD',
 'COIN',
 'NFLX',
 'PLTR',
 'HOOD',
 'INTC',
 'MSTR',
 'TSM',
 'ORCL',
 'MU',
 'RIVN',
 'COST',
 'LLY',
 'CRCL',
 'SKHX',
 'SNDK',
 // Forex
 'EUR',
 'JPY',
 // Commodities
 'GOLD',
 'SILVER',
 // Index
 'XYZ100',
 ])

 const isXyzDexAsset = (symbol: string): boolean => {
 const base = symbol
 .toUpperCase()
 .replace(/^XYZ:/, '')
 .replace(/USDT$|USD$|-USDC$/, '')
 return xyzDexAssets.has(base)
 }

 const MAX_STATIC_COINS = 10

 const showToast = (msg: string) => {
 const toast = document.createElement('div')
 toast.textContent = msg
 toast.className =
 'fixed top-4 left-1/2 -translate-x-1/2 px-4 py-2 rounded-lg text-sm z-50 shadow-lg'
 toast.style.cssText = 'background:#C0392B;color:#fff;'
 document.body.appendChild(toast)
 setTimeout(() => toast.remove(), 2000)
 }

 const handleAddCoin = () => {
 if (!newCoin.trim()) return

 const currentCoins = config.static_coins || []
 if (currentCoins.length >= MAX_STATIC_COINS) {
 showToast(
 language === 'zh'
 ? `最多添加 ${MAX_STATIC_COINS} 个币种`
 : `Maximum ${MAX_STATIC_COINS} coins allowed`
 )
 return
 }

 const symbol = newCoin.toUpperCase().trim()

 // For xyz dex assets (stocks, forex, commodities), use xyz: prefix without USDT
 let formattedSymbol: string
 if (isXyzDexAsset(symbol)) {
 // Remove xyz: prefix (case-insensitive) and any USD suffixes
 const base = symbol
 .replace(/^xyz:/i, '')
 .replace(/USDT$|USD$|-USDC$/i, '')
 formattedSymbol = `xyz:${base}`
 } else {
 formattedSymbol = symbol.endsWith('USDT') ? symbol : `${symbol}USDT`
 }

 if (!currentCoins.includes(formattedSymbol)) {
 onChange({
 ...config,
 static_coins: [...currentCoins, formattedSymbol],
 })
 }
 setNewCoin('')
 }

 const handleRemoveCoin = (coin: string) => {
 onChange({
 ...config,
 static_coins: (config.static_coins || []).filter((c) => c !== coin),
 })
 }

 const handleAddExcludedCoin = () => {
 if (!newExcludedCoin.trim()) return
 const symbol = newExcludedCoin.toUpperCase().trim()

 // For xyz dex assets, use xyz: prefix without USDT
 let formattedSymbol: string
 if (isXyzDexAsset(symbol)) {
 const base = symbol
 .replace(/^xyz:/i, '')
 .replace(/USDT$|USD$|-USDC$/i, '')
 formattedSymbol = `xyz:${base}`
 } else {
 formattedSymbol = symbol.endsWith('USDT') ? symbol : `${symbol}USDT`
 }

 const currentExcluded = config.excluded_coins || []
 if (!currentExcluded.includes(formattedSymbol)) {
 onChange({
 ...config,
 excluded_coins: [...currentExcluded, formattedSymbol],
 })
 }
 setNewExcludedCoin('')
 }

 const handleRemoveExcludedCoin = (coin: string) => {
 onChange({
 ...config,
 excluded_coins: (config.excluded_coins || []).filter((c) => c !== coin),
 })
 }

 // NofxOS badge component
 const NofxOSBadge = () => (
 <span className="text-[9px] px-1.5 py-0.5 rounded font-medium bg-purple-500/20 text-purple-400 border border-purple-500/30">
 NofxOS
 </span>
 )

 return (
 <div className="space-y-6">
 {/* Source Type Multi-Selector */}
 <div>
 <label className="block text-sm font-medium mb-1 text-nofx-text">
 {ts(coinSource.sourceType, language)}
 </label>
 <p className="text-xs mb-3 text-nofx-text-muted">
 {ts(coinSource.multiSelectHint, language)}
 </p>
 <div className="grid grid-cols-3 md:grid-cols-6 gap-2">
 {sourceTypes.map(({ value, icon: Icon, color }) => {
 const selected = selectedSources.includes(value)
 return (
 <button
 key={value}
 onClick={() => toggleSource(value)}
 disabled={disabled}
 className={`relative p-4 rounded-lg border transition-all ${
 selected
 ? 'ring-2 ring-nofx-gold bg-nofx-gold/10'
 : 'hover:bg-[#1E1E1A]/5 bg-nofx-bg'
 } border-nofx-gold/20`}
 >
 {selected && (
 <span className="absolute top-1.5 right-1.5 w-4 h-4 rounded-full bg-nofx-gold text-black text-[10px] font-bold flex items-center justify-center">
 ✓
 </span>
 )}
 <Icon className="w-6 h-6 mx-auto mb-2" style={{ color }} />
 <div className="text-sm font-medium text-nofx-text">
 {ts(coinSource[value as keyof typeof coinSource], language)}
 </div>
 <div className="text-xs mt-1 text-nofx-text-muted">
 {ts(
 coinSource[`${value}Desc` as keyof typeof coinSource],
 language
 )}
 </div>
 </button>
 )
 })}
 </div>
 {isMixed && (
 <div className="mt-2 text-xs font-medium text-blue-400">
 {ts(coinSource.mixedMode, language).replace(
 '{n}',
 String(selectedSources.length)
 )}
 </div>
 )}
 </div>

 {/* Static Coins - when the static source is selected */}
 {selectedSources.includes('static') && (
 <div>
 <label className="block text-sm font-medium mb-3 text-nofx-text">
 {ts(coinSource.staticCoins, language)}
 </label>
 <div className="flex flex-wrap gap-2 mb-3">
 {(config.static_coins || []).map((coin) => (
 <span
 key={coin}
 className="flex items-center gap-1 px-3 py-1.5 rounded-full text-sm bg-nofx-bg-lighter text-nofx-text"
 >
 {coin}
 {!disabled && (
 <button
 onClick={() => handleRemoveCoin(coin)}
 className="ml-1 hover:text-red-400 transition-colors"
 >
 <X className="w-3 h-3" />
 </button>
 )}
 </span>
 ))}
 </div>
 {!disabled && (
 <div className="flex gap-2">
 <input
 type="text"
 value={newCoin}
 onChange={(e) => setNewCoin(e.target.value)}
 onKeyDown={(e) => e.key === 'Enter' && handleAddCoin()}
 placeholder="BTC, ETH, SOL..."
 className="flex-1 px-4 py-2 rounded-lg bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 <button
 onClick={handleAddCoin}
 className="px-4 py-2 rounded-lg flex items-center gap-2 transition-colors bg-nofx-gold text-black hover:bg-yellow-500"
 >
 <Plus className="w-4 h-4" />
 {ts(coinSource.addCoin, language)}
 </button>
 </div>
 )}
 </div>
 )}

 {/* Excluded Coins */}
 <div>
 <div className="flex items-center gap-2 mb-3">
 <Ban className="w-4 h-4 text-nofx-danger" />
 <label className="text-sm font-medium text-nofx-text">
 {ts(coinSource.excludedCoins, language)}
 </label>
 </div>
 <p className="text-xs mb-3 text-nofx-text-muted">
 {ts(coinSource.excludedCoinsDesc, language)}
 </p>
 <div className="flex flex-wrap gap-2 mb-3">
 {(config.excluded_coins || []).map((coin) => (
 <span
 key={coin}
 className="flex items-center gap-1 px-3 py-1.5 rounded-full text-sm bg-nofx-danger/15 text-nofx-danger"
 >
 {coin}
 {!disabled && (
 <button
 onClick={() => handleRemoveExcludedCoin(coin)}
 className="ml-1 hover:text-[#1E1E1A] transition-colors"
 >
 <X className="w-3 h-3" />
 </button>
 )}
 </span>
 ))}
 {(config.excluded_coins || []).length === 0 && (
 <span className="text-xs italic text-nofx-text-muted">
 {ts(coinSource.excludedNone, language)}
 </span>
 )}
 </div>
 {!disabled && (
 <div className="flex gap-2">
 <input
 type="text"
 value={newExcludedCoin}
 onChange={(e) => setNewExcludedCoin(e.target.value)}
 onKeyDown={(e) => e.key === 'Enter' && handleAddExcludedCoin()}
 placeholder="BTC, ETH, DOGE..."
 className="flex-1 px-4 py-2 rounded-lg text-sm bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 <button
 onClick={handleAddExcludedCoin}
 className="px-4 py-2 rounded-lg flex items-center gap-2 transition-colors text-sm bg-nofx-danger text-[#1E1E1A] hover:bg-red-600"
 >
 <Ban className="w-4 h-4" />
 {ts(coinSource.addExcludedCoin, language)}
 </button>
 </div>
 )}
 </div>

 {/* Min OI Value Filter — applies to all sources */}
 <div>
 <label className="text-sm font-medium text-nofx-text">
 {ts(coinSource.minOILabel, language)}
 </label>
 <p className="text-xs mb-2 text-nofx-text-muted">
 {ts(coinSource.minOIDesc, language)}
 </p>
 <div className="flex items-center gap-2">
 <input
 type="number"
 min={0}
 max={1000}
 step={0.5}
 value={config.min_oi_value_millions ?? 15}
 onChange={(e) =>
 onChange({
 ...config,
 min_oi_value_millions: Math.max(
 0,
 Math.min(1000, Number(e.target.value) || 0)
 ),
 })
 }
 disabled={disabled}
 className="w-28 px-3 py-2 rounded-lg text-sm bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 <span className="text-xs text-nofx-text-muted">M USD</span>
 {(config.min_oi_value_millions ?? 15) === 0 && (
 <span className="text-xs text-nofx-text-muted">
 ({ts(coinSource.minOIDefault, language)})
 </span>
 )}
 </div>
 </div>

 {/* AI500 Options - when the ai500 source is selected */}
 {selectedSources.includes('ai500') && (
 <div className="p-4 rounded-lg bg-nofx-gold/5 border border-nofx-gold/20">
 <div className="flex items-center justify-between mb-3">
 <div className="flex items-center gap-2">
 <Zap className="w-4 h-4 text-nofx-gold" />
 <span className="text-sm font-medium text-nofx-text">
 AI500 {ts(coinSource.dataSourceConfig, language)}
 </span>
 <NofxOSBadge />
 </div>
 </div>

 <div className="space-y-3">
 <div className="flex items-center gap-3 pl-2">
 <span className="text-sm text-nofx-text-muted">
 {ts(coinSource.ai500Limit, language)}:
 </span>
 <NofxSelect
 value={config.ai500_limit || 3}
 onChange={(val) =>
 !disabled &&
 onChange({ ...config, ai500_limit: parseInt(val) || 3 })
 }
 disabled={disabled}
 options={[1, 2, 3, 4, 5, 6, 7, 8, 9, 10].map((n) => ({
 value: n,
 label: String(n),
 }))}
 className="px-3 py-1.5 rounded bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 </div>

 <p className="text-xs pl-2 text-nofx-text-muted">
 {ts(coinSource.nofxosNote, language)}
 </p>
 </div>
 </div>
 )}

 {/* Piggy Dash Options - when the piggy_dash source is selected */}
 {selectedSources.includes('piggy_dash') && (
 <div className="space-y-4 p-4 rounded-lg bg-nofx-bg border border-nofx-gold/20">
 <div className="flex items-center gap-2">
 <Zap className="w-4 h-4" style={{ color: '#EC4899' }} />
 <span className="text-sm font-medium text-nofx-text">
 {ts(coinSource.piggyDashTitle, language)}
 </span>
 </div>

 <div>
 <label className="block text-sm text-nofx-text mb-2">
 {ts(coinSource.piggyDashLimit, language)}
 </label>
 <input
 type="number"
 min={1}
 max={30}
 value={config.piggy_dash_limit || 5}
 onChange={(e) =>
 onChange({
 ...config,
 piggy_dash_limit: Math.max(
 1,
 Math.min(30, Number(e.target.value) || 5)
 ),
 })
 }
 disabled={disabled}
 className="w-24 px-3 py-2 rounded-lg bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 </div>

 <div>
 <label className="block text-sm text-nofx-text mb-2">
 {ts(coinSource.piggyDashDirection, language)}
 </label>
 <div className="flex gap-2">
 {[
 { value: '', label: coinSource.piggyDashAll },
 { value: 'breakout', label: coinSource.piggyDashOnlyBreakout },
 {
 value: 'breakdown',
 label: coinSource.piggyDashOnlyBreakdown,
 },
 ].map(({ value, label }) => (
 <button
 key={value || 'all'}
 onClick={() =>
 !disabled &&
 onChange({ ...config, piggy_dash_direction: value })
 }
 disabled={disabled}
 className={`px-3 py-1.5 rounded-lg text-sm transition-all ${
 (config.piggy_dash_direction || '') === value
 ? 'bg-nofx-gold/15 text-nofx-gold border border-nofx-gold/40'
 : 'bg-nofx-bg text-nofx-text-muted border border-nofx-gold/10 hover:bg-[#1E1E1A]/5'
 }`}
 >
 {ts(label, language)}
 </button>
 ))}
 </div>
 </div>

 <div className="text-xs text-nofx-text-muted flex items-start gap-1.5">
 <span>🐷</span>
 <span>{ts(coinSource.piggyDashNote, language)}</span>
 </div>
 </div>
 )}

 {/* OI Top Options - when the oi_top source is selected */}
 {selectedSources.includes('oi_top') && (
 <div className="p-4 rounded-lg bg-nofx-success/5 border border-nofx-success/20">
 <div className="flex items-center justify-between mb-3">
 <div className="flex items-center gap-2">
 <TrendingUp className="w-4 h-4 text-nofx-success" />
 <span className="text-sm font-medium text-nofx-text">
 {ts(coinSource.oiIncreaseTitle, language)}{' '}
 {ts(coinSource.dataSourceConfig, language)}
 </span>
 <NofxOSBadge />
 </div>
 </div>

 <div className="space-y-3">
 <div className="flex items-center gap-3 pl-2">
 <span className="text-sm text-nofx-text-muted">
 {ts(coinSource.oiTopLimit, language)}:
 </span>
 <NofxSelect
 value={config.oi_top_limit || 3}
 onChange={(val) =>
 !disabled &&
 onChange({ ...config, oi_top_limit: parseInt(val) || 3 })
 }
 disabled={disabled}
 options={[1, 2, 3, 4, 5, 6, 7, 8, 9, 10].map((n) => ({
 value: n,
 label: String(n),
 }))}
 className="px-3 py-1.5 rounded bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 </div>

 <p className="text-xs pl-2 text-nofx-text-muted">
 {ts(coinSource.nofxosNote, language)}
 </p>
 </div>
 </div>
 )}

 {/* Short Scan Options - when the short_scan source is selected */}
 {selectedSources.includes('short_scan') && (
 <div className="space-y-4 p-4 rounded-lg bg-nofx-bg border border-nofx-gold/20">
 <div className="flex items-center gap-2">
 <ArrowDownRight className="w-4 h-4" style={{ color: '#EF4444' }} />
 <span className="text-sm font-medium text-nofx-text">
 {ts(coinSource.shortScanTitle, language)}
 </span>
 </div>

 <div>
 <label className="block text-sm text-nofx-text mb-2">
 {ts(coinSource.shortScanLimit, language)}
 </label>
 <input
 type="number"
 min={1}
 max={30}
 value={config.short_scan_limit || 5}
 onChange={(e) =>
 onChange({
 ...config,
 short_scan_limit: Math.max(
 1,
 Math.min(30, Number(e.target.value) || 5)
 ),
 })
 }
 disabled={disabled}
 className="w-24 px-3 py-2 rounded-lg bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 </div>

 <div>
 <label className="block text-sm text-nofx-text mb-2">
 {ts(coinSource.shortScanFunding, language)}
 </label>
 <div className="flex items-center gap-2">
 <input
 type="number"
 min={0}
 max={1}
 step={0.01}
 value={config.short_scan_funding_rate_pct ?? 0.03}
 onChange={(e) =>
 onChange({
 ...config,
 short_scan_funding_rate_pct: Math.max(
 0,
 Math.min(1, Number(e.target.value) || 0)
 ),
 })
 }
 disabled={disabled}
 className="w-28 px-3 py-2 rounded-lg text-sm bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 <span className="text-xs text-nofx-text-muted">%</span>
 {(config.short_scan_funding_rate_pct ?? 0.03) === 0 && (
 <span className="text-xs text-nofx-text-muted">
 ({ts(coinSource.shortScanFundingDefault, language)})
 </span>
 )}
 </div>
 <p className="text-xs pl-2 mt-1 text-nofx-text-muted">
 {ts(coinSource.shortScanFundingDesc, language)}
 </p>
 </div>

 <div>
 <label className="block text-sm text-nofx-text mb-2">
 {ts(coinSource.shortScanHistoryDays, language)}
 </label>
 <div className="flex items-center gap-2">
 <input
 type="number"
 min={-1}
 max={30}
 value={config.short_scan_history_days ?? 0}
 onChange={(e) =>
 onChange({
 ...config,
 short_scan_history_days: Math.max(
 -1,
 Math.min(30, Number(e.target.value) || 0)
 ),
 })
 }
 disabled={disabled}
 className="w-24 px-3 py-2 rounded-lg text-sm bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 <span className="text-xs text-nofx-text-muted">
 {ts(coinSource.shortScanHistoryDaysUnit, language)}
 </span>
 {(config.short_scan_history_days ?? 0) === 0 && (
 <span className="text-xs text-nofx-text-muted">
 ({ts(coinSource.shortScanHistoryDaysDefault, language)})
 </span>
 )}
 </div>
 <p className="text-xs pl-2 mt-1 text-nofx-text-muted">
 {ts(coinSource.shortScanHistoryDaysDesc, language)}
 </p>
 </div>

 <div>
 <label className="block text-sm text-nofx-text mb-2">
 {ts(coinSource.shortScanHistoryMax, language)}
 </label>
 <div className="flex items-center gap-2">
 <input
 type="number"
 min={1}
 max={100}
 value={config.short_scan_history_max ?? 0}
 onChange={(e) =>
 onChange({
 ...config,
 short_scan_history_max: Math.max(
 1,
 Math.min(100, Number(e.target.value) || 0)
 ),
 })
 }
 disabled={disabled}
 className="w-24 px-3 py-2 rounded-lg text-sm bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 <span className="text-xs text-nofx-text-muted">
 {ts(coinSource.shortScanHistoryMaxUnit, language)}
 </span>
 {(config.short_scan_history_max ?? 0) === 0 && (
 <span className="text-xs text-nofx-text-muted">
 ({ts(coinSource.shortScanHistoryMaxDefault, language)})
 </span>
 )}
 </div>
 <p className="text-xs pl-2 mt-1 text-nofx-text-muted">
 {ts(coinSource.shortScanHistoryMaxDesc, language)}
 </p>
 </div>

 <p className="text-xs pl-2 text-nofx-text-muted">
 {ts(coinSource.shortScanNote, language)}
 </p>
 </div>
 )}

 {/* OI Low Options - when the oi_low source is selected */}
 {selectedSources.includes('oi_low') && (
 <div className="p-4 rounded-lg bg-nofx-danger/5 border border-nofx-danger/20">
 <div className="flex items-center justify-between mb-3">
 <div className="flex items-center gap-2">
 <TrendingDown className="w-4 h-4 text-nofx-danger" />
 <span className="text-sm font-medium text-nofx-text">
 {ts(coinSource.oiDecreaseTitle, language)}{' '}
 {ts(coinSource.dataSourceConfig, language)}
 </span>
 <NofxOSBadge />
 </div>
 </div>

 <div className="space-y-3">
 <div className="flex items-center gap-3 pl-2">
 <span className="text-sm text-nofx-text-muted">
 {ts(coinSource.oiLowLimit, language)}:
 </span>
 <NofxSelect
 value={config.oi_low_limit || 3}
 onChange={(val) =>
 !disabled &&
 onChange({ ...config, oi_low_limit: parseInt(val) || 3 })
 }
 disabled={disabled}
 options={[1, 2, 3, 4, 5, 6, 7, 8, 9, 10].map((n) => ({
 value: n,
 label: String(n),
 }))}
 className="px-3 py-1.5 rounded bg-nofx-bg border border-nofx-gold/20 text-nofx-text"
 />
 </div>

 <p className="text-xs pl-2 text-nofx-text-muted">
 {ts(coinSource.nofxosNote, language)}
 </p>
 </div>
 </div>
 )}

 {/* Mixed Mode Summary */}
 {isMixed &&
 (() => {
 const { sources, totalLimit } = getMixedSummary()
 if (sources.length === 0) return null
 return (
 <div className="p-3 rounded-lg bg-blue-500/5 border border-blue-500/20">
 <div className="flex items-center gap-2 mb-2">
 <Shuffle className="w-4 h-4 text-blue-400" />
 <span className="text-sm font-medium text-nofx-text">
 {ts(coinSource.mixedSummary, language)}
 </span>
 </div>
 <div className="flex items-center justify-between text-xs">
 <span className="text-nofx-text font-medium">
 {sources.join(' + ')}
 </span>
 <span className="text-nofx-text-muted">
 {ts(coinSource.maxCoins, language)} {totalLimit}{' '}
 {ts(coinSource.coins, language)}
 </span>
 </div>
 </div>
 )
 })()}
 </div>
 )
}
