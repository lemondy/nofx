import {
 Brain,
 Landmark,
 Eye,
 EyeOff,
 Copy,
 Check,
 Pencil,
 Trash2,
} from 'lucide-react'
import type { AIModel, Exchange, ExchangeAccountState } from '../../types'
import type { Language } from '../../i18n/translations'
import { t } from '../../i18n/translations'
import { getModelIcon } from '../common/ModelIcons'
import { getExchangeIcon } from '../common/ExchangeIcons'
import {
 getShortName,
 AI_PROVIDER_CONFIG,
 truncateAddress,
} from './model-constants'

interface UsageInfo {
 runningCount: number
 totalCount: number
}

interface ConfigStatusGridProps {
 configuredModels: AIModel[]
 configuredExchanges: Exchange[]
 exchangeAccountStates?: Record<string, ExchangeAccountState>
 isExchangeAccountStatesLoading?: boolean
 visibleExchangeAddresses: Set<string>
 copiedId: string | null
 language: Language
 isModelInUse: (modelId: string) => boolean | undefined
 getModelUsageInfo: (modelId: string) => UsageInfo
 isExchangeInUse: (exchangeId: string) => boolean | undefined
 getExchangeUsageInfo: (exchangeId: string) => UsageInfo
 onModelClick: (modelId: string) => void
 onModelDelete: (modelId: string) => void
 onExchangeClick: (exchangeId: string) => void
 onToggleExchangeAddress: (exchangeId: string) => void
 onCopyAddress: (id: string, address: string) => void
}

export function ConfigStatusGrid({
 configuredModels,
 configuredExchanges,
 exchangeAccountStates,
 isExchangeAccountStatesLoading,
 visibleExchangeAddresses,
 copiedId,
 language,
 isModelInUse,
 getModelUsageInfo,
 isExchangeInUse,
 getExchangeUsageInfo,
 onModelClick,
 onModelDelete,
 onExchangeClick,
 onToggleExchangeAddress,
 onCopyAddress,
}: ConfigStatusGridProps) {
 const getExchangeStateMeta = (state: ExchangeAccountState | undefined) => {
 if (!state) {
 return {
 label: language === 'zh' ? '未检查' : 'NOT CHECKED',
 className: 'text-[#6E6E60] border-[#B3AB92]/80 bg-[#ECE8DB]/40',
 }
 }

 switch (state.status) {
 case 'ok':
 return {
 label: state.display_balance || '0',
 className: 'text-emerald-300 border-emerald-500/20 bg-emerald-500/10',
 }
 case 'disabled':
 return {
 label: language === 'zh' ? '已禁用' : 'DISABLED',
 className: 'text-[#6E6E60] border-[#B3AB92]/80 bg-[#ECE8DB]/40',
 }
 case 'missing_credentials':
 return {
 label: language === 'zh' ? '配置不完整' : 'INCOMPLETE',
 className: 'text-amber-300 border-amber-500/20 bg-amber-500/10',
 }
 case 'invalid_credentials':
 return {
 label: language === 'zh' ? '密钥无效' : 'INVALID KEYS',
 className: 'text-rose-300 border-rose-500/20 bg-rose-500/10',
 }
 case 'permission_denied':
 return {
 label: language === 'zh' ? '无余额权限' : 'NO PERMISSION',
 className: 'text-orange-300 border-orange-500/20 bg-orange-500/10',
 }
 default:
 return {
 label: language === 'zh' ? '暂时无法获取' : 'UNAVAILABLE',
 className: 'text-[#3A3A32] border-[#A69E86]/60 bg-[#E4E0D0]/50',
 }
 }
 }

 return (
 <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
 {/* AI Models Card */}
 <div className="nofx-glass rounded-lg border border-nofx-line/50 overflow-hidden">
 <div className="px-4 py-3 border-b border-nofx-line/50 bg-[#F2EFE6] flex items-center gap-2 ">
 <Brain className="w-4 h-4 text-nofx-gold" />
 <h3 className="text-sm font-mono tracking-widest text-[#3A3A32] uppercase">
 {t('aiModels', language)}
 </h3>
 </div>

 <div className="p-4 space-y-3">
 {configuredModels.map((model) => {
 const inUse = isModelInUse(model.id)
 const usageInfo = getModelUsageInfo(model.id)
 return (
 <div
 key={model.id}
 className={`group relative flex items-center justify-between p-3 rounded-md transition-all border border-transparent ${
 inUse
 ? 'opacity-80'
 : 'hover:bg-[#1E1E1A]/5 hover:border-nofx-line'
 } bg-[#F2EFE6]`}
 >
 <div className="flex items-center gap-4">
 <div className="relative">
 <div className="absolute inset-0 bg-indigo-500/20 rounded-full blur-sm group-hover:bg-indigo-500/30 transition-all"></div>
 <div className="w-10 h-10 rounded-full flex items-center justify-center bg-[#F2EFE6] border border-nofx-line relative z-10">
 {getModelIcon(model.provider || model.id, {
 width: 20,
 height: 20,
 }) || (
 <span className="text-xs font-bold text-indigo-400">
 {getShortName(model.name)[0]}
 </span>
 )}
 </div>
 </div>

 <div className="min-w-0">
 <div className="font-mono text-sm text-[#2A2A24] group-hover:text-nofx-gold transition-colors">
 {getShortName(model.name)}
 </div>
 <div className="text-[10px] text-[#6E6E60] font-mono flex items-center gap-2">
 {model.customModelName ||
 AI_PROVIDER_CONFIG[model.provider]?.defaultModel ||
 ''}
 </div>
 </div>
 </div>

 <div className="flex items-center gap-1.5">
 {/* Edit / delete controls */}
 <button
 type="button"
 title={language === 'zh' ? '编辑' : 'Edit'}
 onClick={(e) => {
 e.stopPropagation()
 onModelClick(model.id)
 }}
 className="p-1.5 rounded-md text-[#6E6E60] hover:text-nofx-gold hover:bg-[#1E1E1A]/5 transition-all"
 >
 <Pencil className="w-3.5 h-3.5" />
 </button>
 <button
 type="button"
 title={language === 'zh' ? '删除' : 'Delete'}
 disabled={inUse}
 onClick={(e) => {
 e.stopPropagation()
 if (!inUse) onModelDelete(model.id)
 }}
 className={`p-1.5 rounded-md transition-all ${
 inUse
 ? 'text-[#8A8A7C] cursor-not-allowed'
 : 'text-[#6E6E60] hover:text-red-400 hover:bg-red-500/10'
 }`}
 >
 <Trash2 className="w-3.5 h-3.5" />
 </button>
 <div className="text-right">
 {usageInfo.totalCount > 0 ? (
 <span
 className={`text-[10px] font-mono px-2 py-1 rounded border ${
 usageInfo.runningCount > 0
 ? 'bg-green-500/10 border-green-500/30 text-green-400'
 : 'bg-yellow-500/10 border-yellow-500/30 text-yellow-400'
 }`}
 >
 {usageInfo.runningCount}/{usageInfo.totalCount} ACTIVE
 </span>
 ) : (
 <span className="text-[10px] font-mono text-[#8A8A7C] uppercase tracking-wider">
 {language === 'zh' ? '就绪' : 'STANDBY'}
 </span>
 )}
 </div>
 </div>
 </div>
 )
 })}

 {configuredModels.length === 0 && (
 <div className="text-center py-10 border border-dashed border-[#C0B9A2] rounded-lg bg-[#F2EFE6]">
 <Brain className="w-8 h-8 mx-auto mb-3 text-[#8A8A7C]" />
 <div className="text-xs font-mono text-[#6E6E60] uppercase tracking-widest">
 {t('noModelsConfigured', language)}
 </div>
 </div>
 )}
 </div>
 </div>

 {/* Exchanges Card */}
 <div className="nofx-glass rounded-lg border border-nofx-line/50 overflow-hidden">
 <div className="px-4 py-3 border-b border-nofx-line/50 bg-[#F2EFE6] flex items-center gap-2 ">
 <Landmark className="w-4 h-4 text-nofx-gold" />
 <h3 className="text-sm font-mono tracking-widest text-[#3A3A32] uppercase">
 {t('exchanges', language)}
 </h3>
 </div>

 <div className="p-4 space-y-3">
 {configuredExchanges.map((exchange) => {
 const inUse = isExchangeInUse(exchange.id)
 const usageInfo = getExchangeUsageInfo(exchange.id)
 const state = exchangeAccountStates?.[exchange.id]
 const stateMeta = getExchangeStateMeta(state)
 return (
 <div
 key={exchange.id}
 className={`group relative flex items-center justify-between p-3 rounded-md transition-all border border-transparent ${
 inUse
 ? 'opacity-80'
 : 'hover:bg-[#1E1E1A]/5 hover:border-nofx-line cursor-pointer'
 } bg-[#F2EFE6]`}
 onClick={() => onExchangeClick(exchange.id)}
 >
 <div className="flex items-center gap-4 min-w-0">
 <div className="relative">
 <div className="absolute inset-0 bg-yellow-500/20 rounded-full blur-sm group-hover:bg-yellow-500/30 transition-all"></div>
 <div className="w-10 h-10 rounded-full flex items-center justify-center bg-[#F2EFE6] border border-nofx-line relative z-10">
 {getExchangeIcon(exchange.exchange_type || exchange.id, {
 width: 20,
 height: 20,
 })}
 </div>
 </div>

 <div className="min-w-0">
 <div className="font-mono text-sm text-[#2A2A24] group-hover:text-nofx-gold transition-colors truncate">
 {exchange.exchange_type?.toUpperCase() ||
 getShortName(exchange.name)}
 <span className="text-[10px] text-[#6E6E60] ml-2 border border-[#C0B9A2] px-1 rounded">
 {exchange.account_name || 'DEFAULT'}
 </span>
 </div>
 <div className="text-[10px] text-[#6E6E60] font-mono flex items-center gap-2">
 {exchange.type?.toUpperCase() || 'CEX'}
 </div>
 <div className="mt-1 flex flex-wrap items-center gap-2 text-[10px] font-mono">
 <span
 className={`rounded border px-1.5 py-0.5 ${stateMeta.className}`}
 >
 {isExchangeAccountStatesLoading && !state
 ? language === 'zh'
 ? '检查中...'
 : 'CHECKING...'
 : stateMeta.label}
 </span>
 {state?.status !== 'ok' && state?.error_message ? (
 <span className="text-[#6E6E60] truncate max-w-[220px]">
 {state.error_message}
 </span>
 ) : null}
 </div>
 </div>
 </div>

 <div className="flex flex-col items-end gap-1">
 {/* Wallet Address Display Logic */}
 {(() => {
 const walletAddr =
 exchange.hyperliquidWalletAddr ||
 exchange.asterUser ||
 exchange.lighterWalletAddr
 if (exchange.type !== 'dex' || !walletAddr) return null
 const isVisible = visibleExchangeAddresses.has(exchange.id)
 const isCopied = copiedId === `exchange-${exchange.id}`

 return (
 <div
 className="flex items-center gap-1"
 onClick={(e) => e.stopPropagation()}
 >
 <span className="text-[10px] font-mono text-[#6E6E60] bg-[#E9E4D6] px-1.5 py-0.5 rounded border border-[#C0B9A2]">
 {isVisible ? walletAddr : truncateAddress(walletAddr)}
 </span>
 <button
 onClick={(e) => {
 e.stopPropagation()
 onToggleExchangeAddress(exchange.id)
 }}
 className="text-[#8A8A7C] hover:text-[#3A3A32]"
 >
 {isVisible ? <EyeOff size={10} /> : <Eye size={10} />}
 </button>
 <button
 onClick={(e) => {
 e.stopPropagation()
 onCopyAddress(`exchange-${exchange.id}`, walletAddr)
 }}
 className="text-[#8A8A7C] hover:text-nofx-gold"
 >
 {isCopied ? (
 <Check size={10} className="text-green-500" />
 ) : (
 <Copy size={10} />
 )}
 </button>
 </div>
 )
 })()}

 {usageInfo.totalCount > 0 ? (
 <span
 className={`text-[10px] font-mono px-2 py-1 rounded border ${
 usageInfo.runningCount > 0
 ? 'bg-green-500/10 border-green-500/30 text-green-400'
 : 'bg-yellow-500/10 border-yellow-500/30 text-yellow-400'
 }`}
 >
 {usageInfo.runningCount}/{usageInfo.totalCount} ACTIVE
 </span>
 ) : (
 <span className="text-[10px] font-mono text-[#8A8A7C] uppercase tracking-wider">
 {language === 'zh' ? '就绪' : 'STANDBY'}
 </span>
 )}
 </div>
 </div>
 )
 })}
 {configuredExchanges.length === 0 && (
 <div className="text-center py-10 border border-dashed border-[#C0B9A2] rounded-lg bg-[#F2EFE6]">
 <Landmark className="w-8 h-8 mx-auto mb-3 text-[#8A8A7C]" />
 <div className="text-xs font-mono text-[#6E6E60] uppercase tracking-widest">
 {t('noExchangesConfigured', language)}
 </div>
 </div>
 )}
 </div>
 </div>
 </div>
 )
}
