import { Shield, AlertTriangle } from 'lucide-react'
import type { RiskControlConfig } from '../../types'
import { riskControl, ts } from '../../i18n/strategy-translations'

interface RiskControlEditorProps {
 config: RiskControlConfig
 onChange: (config: RiskControlConfig) => void
 disabled?: boolean
 language: string
}

export function RiskControlEditor({
 config,
 onChange,
 disabled,
 language,
}: RiskControlEditorProps) {
 const updateField = <K extends keyof RiskControlConfig>(
 key: K,
 value: RiskControlConfig[K]
 ) => {
 if (!disabled) {
 onChange({ ...config, [key]: value })
 }
 }

 return (
 <div className="space-y-6">
 {/* Position Limits */}
 <div>
 <div className="flex items-center gap-2 mb-4">
 <Shield className="w-5 h-5" style={{ color: '#B8912A' }} />
 <h3 className="font-medium" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.positionLimits, language)}
 </h3>
 </div>

 <div className="grid grid-cols-1 gap-4 mb-4">
 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.maxPositions, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.maxPositionsDesc, language)}
 </p>
 <input
 type="number"
 value={config.max_positions ?? 3}
 onChange={(e) =>
 updateField('max_positions', parseInt(e.target.value) || 3)
 }
 disabled={disabled}
 min={1}
 max={10}
 className="w-32 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.earlyCloseMinHours, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.earlyCloseMinHoursDesc, language)}
 </p>
 <input
 type="number"
 value={config.early_close_min_hours ?? 0}
 onChange={(e) =>
 updateField(
 'early_close_min_hours',
 e.target.value === '' ? 0 : parseInt(e.target.value)
 )
 }
 disabled={disabled}
 min={-1}
 max={72}
 className="w-32 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 <p className="text-xs mt-2 font-medium" style={{ color: '#2E7D4F' }}>
 {`当前生效: ${(config.early_close_min_hours ?? 0) === 0 ? 4 : (config.early_close_min_hours ?? 0) <= -1 ? '禁用' : config.early_close_min_hours}h`}
 </p>
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.pumpGuard4hPct, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.pumpGuard4hPctDesc, language)}
 </p>
 <input
 type="number"
 value={config.pump_guard_4h_pct ?? 0}
 onChange={(e) =>
 updateField(
 'pump_guard_4h_pct',
 e.target.value === '' ? 0 : parseFloat(e.target.value)
 )
 }
 disabled={disabled}
 min={-1}
 max={200}
 className="w-32 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 <p className="text-xs mt-2 font-medium" style={{ color: '#2E7D4F' }}>
 {`当前生效: ${(config.pump_guard_4h_pct ?? 0) === 0 ? 20 : (config.pump_guard_4h_pct ?? 0) < 0 ? '禁用' : config.pump_guard_4h_pct}%`}
 </p>
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.tpTrimProfitPct, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.tpTrimProfitPctDesc, language)}
 </p>
 <input
 type="number"
 value={config.tp_trim_profit_pct ?? 0}
 onChange={(e) =>
 updateField(
 'tp_trim_profit_pct',
 e.target.value === '' ? 0 : parseFloat(e.target.value)
 )
 }
 disabled={disabled}
 className="w-32 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 {(() => {
 const lockR = config.profit_lock_at_r ?? 0
 if (lockR >= 0) {
 return (
 <p className="text-xs mt-2 font-medium" style={{ color: '#B8912A' }}>
 {ts(riskControl.profitLockTrimHint, language)}
 </p>
 )
 }
 return null
 })()}
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.tpFullProfitPct, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.tpFullProfitPctDesc, language)}
 </p>
 <input
 type="number"
 value={config.tp_full_profit_pct ?? 0}
 onChange={(e) =>
 updateField(
 'tp_full_profit_pct',
 e.target.value === '' ? 0 : parseFloat(e.target.value)
 )
 }
 disabled={disabled}
 className="w-32 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.profitLockAtR, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.profitLockAtRDesc, language)}
 </p>
 <input
 type="number"
 step={0.1}
 value={config.profit_lock_at_r ?? 0}
 onChange={(e) =>
 updateField(
 'profit_lock_at_r',
 e.target.value === '' ? 0 : parseFloat(e.target.value)
 )
 }
 disabled={disabled}
 className="w-32 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 <p className="text-xs mt-2 font-medium" style={{ color: '#2E7D4F' }}>
 {`当前生效: ${(config.profit_lock_at_r ?? 0) < 0 ? '禁用(ROE 减仓档接管)' : `${(config.profit_lock_at_r ?? 0) === 0 ? 1 : config.profit_lock_at_r}R 时保本+减半 50%`}`}
 </p>
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.profitLockBEOffsetR, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.profitLockBEOffsetRDesc, language)}
 </p>
 <input
 type="number"
 step={0.05}
 value={config.profit_lock_be_offset_r ?? 0}
 onChange={(e) =>
 updateField(
 'profit_lock_be_offset_r',
 e.target.value === '' ? 0 : parseFloat(e.target.value)
 )
 }
 disabled={disabled}
 className="w-32 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 <p className="text-xs mt-2 font-medium" style={{ color: '#2E7D4F' }}>
 {`当前生效: ${(config.profit_lock_be_offset_r ?? 0) < 0 ? '纯保本(开仓价)' : `开仓价+${(config.profit_lock_be_offset_r ?? 0) === 0 ? 0.2 : config.profit_lock_be_offset_r}R`}
 `}
 </p>
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.tpCloseFraction, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.tpCloseFractionDesc, language)}
 </p>
 <input
 type="number"
 step={0.05}
 min={-1}
 max={1}
 value={config.tp_close_fraction ?? 0}
 onChange={(e) =>
 updateField(
 'tp_close_fraction',
 e.target.value === '' ? 0 : parseFloat(e.target.value)
 )
 }
 disabled={disabled}
 className="w-32 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 <p className="text-xs mt-2 font-medium" style={{ color: '#2E7D4F' }}>
 {(() => {
 const raw = config.tp_close_fraction ?? 0
 // Backend bool: absent/false = trailing OFF → full close (no runner).
 const trailingOn = config.trailing_stop_enabled === true
 if (!trailingOn) return '当前生效: 全平(移动止损关闭,趋势跑单不可用)'
 const eff = raw < 0 ? 1 : raw === 0 ? 0.5 : raw
 return `当前生效: 止盈触发平 ${(eff * 100).toFixed(0)}%,剩余 ${100 - eff * 100}% 由移动止损接管`
 })()}
 </p>
 </div>
 </div>

 {/* Trading Leverage (Exchange) */}
 <div className="mb-2">
 <p className="text-xs font-medium mb-2" style={{ color: '#B8912A' }}>
 {ts(riskControl.tradingLeverage, language)}
 </p>
 </div>
 <div className="grid grid-cols-2 gap-4 mb-4">
 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.btcEthLeverage, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.btcEthLeverageDesc, language)}
 </p>
 <div className="flex items-center gap-2">
 <input
 type="range"
 value={config.btc_eth_max_leverage ?? 5}
 onChange={(e) =>
 updateField('btc_eth_max_leverage', parseInt(e.target.value))
 }
 disabled={disabled}
 min={1}
 max={20}
 className="flex-1 accent-yellow-500"
 />
 <span
 className="w-12 text-center font-mono"
 style={{ color: '#B8912A' }}
 >
 {config.btc_eth_max_leverage ?? 5}x
 </span>
 </div>
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.altcoinLeverage, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.altcoinLeverageDesc, language)}
 </p>
 <div className="flex items-center gap-2">
 <input
 type="range"
 value={config.altcoin_max_leverage ?? 5}
 onChange={(e) =>
 updateField('altcoin_max_leverage', parseInt(e.target.value))
 }
 disabled={disabled}
 min={1}
 max={20}
 className="flex-1 accent-yellow-500"
 />
 <span
 className="w-12 text-center font-mono"
 style={{ color: '#B8912A' }}
 >
 {config.altcoin_max_leverage ?? 5}x
 </span>
 </div>
 </div>
 </div>

 {/* Position Value Ratio (Risk Control - CODE ENFORCED) */}
 <div className="mb-2">
 <p className="text-xs font-medium" style={{ color: '#2E7D4F' }}>
 {ts(riskControl.positionValueRatio, language)}
 </p>
 <p className="text-xs mt-1" style={{ color: '#6E6E60' }}>
 {ts(riskControl.positionValueRatioDesc, language)}
 </p>
 </div>
 <div className="grid grid-cols-2 gap-4">
 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #2E7D4F' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.btcEthPositionValueRatio, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.btcEthPositionValueRatioDesc, language)}
 </p>
 <div className="flex items-center gap-2">
 <input
 type="range"
 value={config.btc_eth_max_position_value_ratio ?? 5}
 onChange={(e) =>
 updateField('btc_eth_max_position_value_ratio', parseFloat(e.target.value))
 }
 disabled={disabled}
 min={0.5}
 max={10}
 step={0.5}
 className="flex-1 accent-green-500"
 />
 <span
 className="w-12 text-center font-mono"
 style={{ color: '#2E7D4F' }}
 >
 {config.btc_eth_max_position_value_ratio ?? 5}x
 </span>
 </div>
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #2E7D4F' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.altcoinPositionValueRatio, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.altcoinPositionValueRatioDesc, language)}
 </p>
 <div className="flex items-center gap-2">
 <input
 type="range"
 value={config.altcoin_max_position_value_ratio ?? 1}
 onChange={(e) =>
 updateField('altcoin_max_position_value_ratio', parseFloat(e.target.value))
 }
 disabled={disabled}
 min={0.5}
 max={10}
 step={0.5}
 className="flex-1 accent-green-500"
 />
 <span
 className="w-12 text-center font-mono"
 style={{ color: '#2E7D4F' }}
 >
 {config.altcoin_max_position_value_ratio ?? 1}x
 </span>
 </div>
 </div>
 </div>
 </div>

 {/* Risk Parameters */}
 <div>
 <div className="flex items-center gap-2 mb-4">
 <AlertTriangle className="w-5 h-5" style={{ color: '#C0392B' }} />
 <h3 className="font-medium" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.riskParameters, language)}
 </h3>
 </div>

 <div className="grid grid-cols-2 gap-4">
 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.minRiskReward, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.minRiskRewardDesc, language)}
 </p>
 <div className="flex items-center">
 <span style={{ color: '#6E6E60' }}>1:</span>
 <input
 type="number"
 value={config.min_risk_reward_ratio ?? 3}
 onChange={(e) => {
 // 0 is a legal value (disables the RR gate) — a bare
 // `|| 3` made it unreachable from the UI.
 const v = parseFloat(e.target.value)
 updateField('min_risk_reward_ratio', Number.isFinite(v) ? v : 3)
 }}
 disabled={disabled}
 min={0}
 max={10}
 step={0.5}
 className="w-20 px-3 py-2 rounded ml-2"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 </div>
 {(config.min_risk_reward_ratio ?? 3) === 0 && (
 <p className="text-xs mt-2 font-medium" style={{ color: '#B8912A' }}>
 {ts(riskControl.minRRZeroHint, language)}
 </p>
 )}
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #2E7D4F' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.maxMarginUsage, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.maxMarginUsageDesc, language)}
 </p>
 <div className="flex items-center gap-2">
 <input
 type="range"
 value={(config.max_margin_usage ?? 0.9) * 100}
 onChange={(e) =>
 updateField('max_margin_usage', parseInt(e.target.value) / 100)
 }
 disabled={disabled}
 min={10}
 max={100}
 className="flex-1 accent-green-500"
 />
 <span className="w-12 text-center font-mono" style={{ color: '#2E7D4F' }}>
 {Math.round((config.max_margin_usage ?? 0.9) * 100)}%
 </span>
 </div>
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #2E7D4F' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.riskPerTradePct, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.riskPerTradePctDesc, language)}
 </p>
 <div className="flex items-center">
 <input
 type="number"
 value={config.risk_per_trade_pct ?? 0}
 onChange={(e) => {
 const v = parseFloat(e.target.value)
 updateField('risk_per_trade_pct', e.target.value === '' || isNaN(v) ? 0 : v)
 }}
 disabled={disabled}
 min={0}
 max={10}
 step={0.1}
 className="w-24 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 <span className="ml-2" style={{ color: '#6E6E60' }}>
 %
 </span>
 </div>
 <p className="text-xs mt-2 font-medium" style={{ color: '#2E7D4F' }}>
 {`当前生效: ${(config.risk_per_trade_pct ?? 0) <= 0 ? '默认 1.5' : config.risk_per_trade_pct}%`}
 </p>
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #2E7D4F' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.vendorDivergence, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.vendorDivergenceDesc, language)}
 </p>
 <div className="flex items-center">
 <input
 type="number"
 value={config.max_vendor_divergence_pct ?? 0}
 onChange={(e) => {
 const v = parseFloat(e.target.value)
 updateField('max_vendor_divergence_pct', e.target.value === '' || isNaN(v) ? 0 : v)
 }}
 disabled={disabled}
 min={-5}
 max={10}
 step={0.5}
 className="w-24 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 <span className="ml-2" style={{ color: '#6E6E60' }}>
 %
 </span>
 </div>
 <p className="text-xs mt-2 font-medium" style={{ color: '#2E7D4F' }}>
 {`当前生效: ${(config.max_vendor_divergence_pct ?? 0) < 0 ? '禁用' : (config.max_vendor_divergence_pct ?? 0) === 0 ? '默认 1' : config.max_vendor_divergence_pct}%`}
 </p>
 </div>
 </div>
 </div>

 {/* Entry Requirements */}
 <div>
 <div className="flex items-center gap-2 mb-4">
 <Shield className="w-5 h-5" style={{ color: '#2E7D4F' }} />
 <h3 className="font-medium" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.entryRequirements, language)}
 </h3>
 </div>

 <div className="grid grid-cols-2 gap-4">
 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.minPositionSize, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.minPositionSizeDesc, language)}
 </p>
 <div className="flex items-center">
 <input
 type="number"
 value={config.min_position_size ?? 12}
 onChange={(e) =>
 updateField('min_position_size', parseFloat(e.target.value) || 12)
 }
 disabled={disabled}
 min={10}
 max={1000}
 className="w-24 px-3 py-2 rounded"
 style={{
 background: '#E9E4D6',
 border: '1px solid #C0B9A2',
 color: '#1E1E1A',
 }}
 />
 <span className="ml-2" style={{ color: '#6E6E60' }}>
 USDT
 </span>
 </div>
 </div>

 <div
 className="p-4 rounded-lg"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 {ts(riskControl.minConfidence, language)}
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 {ts(riskControl.minConfidenceDesc, language)}
 </p>
 <div className="flex items-center gap-2">
 <input
 type="range"
 value={config.min_confidence ?? 75}
 onChange={(e) =>
 updateField('min_confidence', parseInt(e.target.value))
 }
 disabled={disabled}
 min={50}
 max={100}
 className="flex-1 accent-green-500"
 />
 <span className="w-12 text-center font-mono" style={{ color: '#2E7D4F' }}>
 {config.min_confidence ?? 75}
 </span>
 </div>
 </div>
 </div>

 <div
 className="p-4 rounded-lg mt-4"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 限价挂单偏移 limit_entry_offset_pct
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 默认限价开仓:做多挂现价下方、做空挂现价上方该百分比(0.1–5%,默认0.5)。偏移越大成交越慢、价位越优
 </p>
 <div className="flex items-center gap-2">
 <input
 type="range"
 value={config.limit_entry_offset_pct ?? 0.5}
 onChange={(e) =>
 updateField('limit_entry_offset_pct', parseFloat(e.target.value))
 }
 disabled={disabled}
 min={0.1}
 max={5}
 step={0.1}
 className="flex-1 accent-green-500"
 />
 <span className="w-14 text-center font-mono" style={{ color: '#2E7D4F' }}>
 {(config.limit_entry_offset_pct ?? 0.5).toFixed(1)}%
 </span>
 </div>
 </div>

 <div
 className="p-4 rounded-lg mt-4"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="flex items-center justify-between block text-sm mb-1 cursor-pointer" style={{ color: '#1E1E1A' }}>
 <span>锚点穿越转市价 limit_entry_market_fallback</span>
 <input
 type="checkbox"
 checked={config.limit_entry_market_fallback !== false}
 onChange={(e) =>
 updateField('limit_entry_market_fallback', e.target.checked)
 }
 disabled={disabled}
 className="w-3.5 h-3.5 rounded accent-green-500"
 />
 </label>
 <p className="text-xs" style={{ color: '#6E6E60' }}>
 AI 推理期间行情穿过限价锚点(多单锚点高于现价/空单低于现价)时,视为回踩/反弹已到位,自动转为市价单入场(全部风险闸门按现价复检);价格已越过止损则拒单。关闭后维持旧行为:穿越即拒单
 </p>
 </div>

 <div
 className="p-4 rounded-lg mt-4"
 style={{ background: '#F2EFE6', border: '1px solid #C0B9A2' }}
 >
 <label className="block text-sm mb-1" style={{ color: '#1E1E1A' }}>
 回撤保护平仓 peak_drawdown
 </label>
 <p className="text-xs mb-2" style={{ color: '#6E6E60' }}>
 浮盈峰值达到「起征浮盈」后,从峰值回撤超过「最大回撤」即程序自动平仓锁定利润
 </p>
 <div className="grid grid-cols-2 gap-3">
  <div>
   <label className="block text-xs mb-1" style={{ color: '#6E6E60' }}>
    起征浮盈 %
   </label>
   <input
    type="number"
    value={config.peak_drawdown_min_profit_pct ?? 5}
    onChange={(e) =>
     updateField('peak_drawdown_min_profit_pct', parseFloat(e.target.value) || 5)
    }
    disabled={disabled}
    min={1}
    max={50}
    step={0.5}
    className="w-24 px-3 py-2 rounded"
    style={{
     background: '#E9E4D6',
     border: '1px solid #C0B9A2',
     color: '#1E1E1A',
    }}
   />
  </div>
  <div>
   <label className="block text-xs mb-1" style={{ color: '#6E6E60' }}>
    最大回撤 %
   </label>
   <input
    type="number"
    value={config.peak_drawdown_max_dd_pct ?? 55}
    onChange={(e) =>
     updateField('peak_drawdown_max_dd_pct', parseFloat(e.target.value) || 55)
    }
    disabled={disabled}
    min={10}
    max={95}
    step={1}
    className="w-24 px-3 py-2 rounded"
    style={{
     background: '#E9E4D6',
     border: '1px solid #C0B9A2',
     color: '#1E1E1A',
    }}
   />
  </div>
 </div>
 </div>
 </div>
 </div>
 )
}
