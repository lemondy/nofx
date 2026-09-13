import { useCallback, useEffect, useMemo, useState } from 'react'
import { api } from '../lib/api'
import { t } from '../i18n/translations'
import type {
 JournalEntry,
 JournalStats,
 TradingRule,
 RuleCheckLog,
 RuleProposal,
 RuleViolation,
 TraderInfo,
 AIReviewResult,
 ReviewPromptConfig,
} from '../types'
import type { Language } from '../i18n/translations'

// Binance-style dark palette (matches DataPage conventions)
const C = {
 bg: '#F2EFE6',
 card: '#ECE8DB',
 cardHover: '#EBE7DA',
 border: '#C0B9A2',
 rowBorder: '#E9E4D6',
 text: '#1E1E1A',
 muted: '#6E6E60',
 faint: '#8A8A7C',
 up: '#2E7D4F',
 down: '#C0392B',
 gold: '#B8912A',
 blue: '#4B9EFF',
 warn: '#B8912A',
}

const CARD_STYLE: React.CSSProperties = {
 background: C.card,
 border: `1px solid ${C.border}`,
 borderRadius: 12,
}

type ReviewTab = 'journal' | 'stats' | 'rules' | 'ai'

const EMOTION_OPTIONS = ['calm', 'fomo', 'fear_of_missing', 'revenge', 'overconfident', 'hesitant']
const MISTAKE_OPTIONS = ['strategy', 'execution', 'risk_control', 'market', 'none']
const STRATEGY_OPTIONS = ['breakout', 'mean_reversion', 'trend_following', 'momentum', 'scalp', 'news', 'other']
const RULE_FIELDS = [
 'leverage',
 'position_size_usd',
 'position_value_pct',
 'stop_loss_pct',
 'take_profit_pct',
 'risk_reward',
 'confidence',
 'symbol',
 'has_stop_loss',
 'has_take_profit',
]

function fmtTime(ms: number): string {
 if (!ms) return '--'
 const d = new Date(ms)
 const pad = (n: number) => String(n).padStart(2, '0')
 return `${d.getMonth() + 1}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function fmtPrice(v: number): string {
 if (!v) return '--'
 if (Math.abs(v) >= 1000) return v.toFixed(1)
 if (Math.abs(v) >= 1) return v.toFixed(4)
 return v.toPrecision(4)
}

export function ReviewPage({ language }: { language: Language }) {
 const rv = useCallback(
 (key: string, params?: Record<string, string | number>) =>
 t(`reviewPage.${key}`, language, params),
 [language]
 )

 const [traders, setTraders] = useState<TraderInfo[]>([])
 const [selectedTraderId, setSelectedTraderId] = useState<string | undefined>(() => {
 return new URLSearchParams(window.location.search).get('trader') || undefined
 })
 const [tab, setTab] = useState<ReviewTab>('journal')

 useEffect(() => {
 api
 .getTraders(true)
 .then((list) => {
 setTraders(list)
 if (!selectedTraderId && list.length > 0) {
 setSelectedTraderId(list[0].trader_id)
 }
 })
 .catch(() => setTraders([]))
 // eslint-disable-next-line react-hooks/exhaustive-deps
 }, [])

 const tabs: { key: ReviewTab; label: string }[] = [
 { key: 'journal', label: rv('tabJournal') },
 { key: 'stats', label: rv('tabStats') },
 { key: 'rules', label: rv('tabRules') },
 { key: 'ai', label: rv('tabAI') },
 ]

 return (
 <div className="min-h-screen" style={{ background: C.bg, color: C.text }}>
 <div className="max-w-[1440px] mx-auto px-6 py-6">
 {/* Header */}
 <div className="flex items-center justify-between flex-wrap gap-4 mb-5">
 <div>
 <h1 className="text-xl font-bold">{rv('title')}</h1>
 <p className="text-xs mt-1" style={{ color: C.muted }}>
 {rv('subtitle')}
 </p>
 </div>
 <div className="flex items-center gap-2">
 <span className="text-xs" style={{ color: C.muted }}>
 {rv('trader')}
 </span>
 <select
 value={selectedTraderId || ''}
 onChange={(e) => setSelectedTraderId(e.target.value)}
 className="text-xs rounded px-3 py-2 outline-none"
 style={{ background: C.card, border: `1px solid ${C.border}`, color: C.text }}
 >
 {traders.length === 0 && <option value="">--</option>}
 {traders.map((tr) => (
 <option key={tr.trader_id} value={tr.trader_id}>
 {tr.trader_name}
 </option>
 ))}
 </select>
 </div>
 </div>

 {/* Tabs */}
 <div className="flex gap-1 mb-5 border-b" style={{ borderColor: C.border }}>
 {tabs.map((item) => (
 <button
 key={item.key}
 onClick={() => setTab(item.key)}
 className="px-4 py-2.5 text-sm font-medium transition-colors rounded-t"
 style={{
 color: tab === item.key ? C.gold : C.muted,
 borderBottom: tab === item.key ? `2px solid ${C.gold}` : '2px solid transparent',
 background: tab === item.key ? 'rgba(184,145,42,0.06)' : 'transparent',
 }}
 >
 {item.label}
 </button>
 ))}
 </div>

 {!selectedTraderId ? (
 <div className="py-20 text-center text-sm" style={{ color: C.muted }}>
 {rv('noTraders')}
 </div>
 ) : (
 <>
 {tab === 'journal' && <JournalTab traderId={selectedTraderId} language={language} />}
 {tab === 'stats' && <StatsTab traderId={selectedTraderId} language={language} />}
 {tab === 'rules' && <RulesTab traderId={selectedTraderId} language={language} />}
 {tab === 'ai' && <AITab traderId={selectedTraderId} language={language} />}
 </>
 )}
 </div>
 </div>
 )
}

// ===================== Journal Tab =====================

function JournalTab({ traderId, language }: { traderId: string; language: Language }) {
 const rv = useCallback(
 (key: string, params?: Record<string, string | number>) =>
 t(`reviewPage.${key}`, language, params),
 [language]
 )
 const [entries, setEntries] = useState<JournalEntry[]>([])
 const [total, setTotal] = useState(0)
 const [loading, setLoading] = useState(true)
 const [syncing, setSyncing] = useState(false)
 const [statusFilter, setStatusFilter] = useState('')
 const [symbolFilter, setSymbolFilter] = useState('')
 const [editing, setEditing] = useState<JournalEntry | null>(null)

 const load = useCallback(async () => {
 setLoading(true)
 try {
 const data = await api.getJournal(traderId, {
 limit: 100,
 symbol: symbolFilter || undefined,
 reviewStatus: statusFilter || undefined,
 })
 setEntries(data.entries || [])
 setTotal(data.total || 0)
 } catch {
 setEntries([])
 } finally {
 setLoading(false)
 }
 // eslint-disable-next-line react-hooks/exhaustive-deps
 }, [traderId, statusFilter, symbolFilter])

 useEffect(() => {
 if (traderId) load()
 }, [traderId, load])

 const handleSync = async () => {
 setSyncing(true)
 try {
 const res = await api.syncJournal(traderId)
 if (res.created > 0) {
 await load()
 }
 } catch {
 /* ignore */
 } finally {
 setSyncing(false)
 }
 }

 return (
 <div>
 {/* Toolbar */}
 <div className="flex items-center justify-between flex-wrap gap-3 mb-4">
 <div className="flex items-center gap-2">
 <select
 value={statusFilter}
 onChange={(e) => setStatusFilter(e.target.value)}
 className="text-xs rounded px-3 py-2 outline-none"
 style={{ background: C.card, border: `1px solid ${C.border}`, color: C.text }}
 >
 <option value="">{rv('filterAll')}</option>
 <option value="pending">{rv('filterPending')}</option>
 <option value="reviewed">{rv('filterReviewed')}</option>
 </select>
 <input
 value={symbolFilter}
 onChange={(e) => setSymbolFilter(e.target.value.toUpperCase())}
 placeholder={rv('filterSymbolPlaceholder')}
 className="text-xs rounded px-3 py-2 outline-none w-32"
 style={{ background: C.card, border: `1px solid ${C.border}`, color: C.text }}
 />
 </div>
 <div className="flex items-center gap-3">
 <span className="text-xs" style={{ color: C.muted }}>
 {rv('totalCount', { count: total })}
 </span>
 <button
 onClick={handleSync}
 disabled={syncing}
 className="text-xs font-semibold rounded px-4 py-2 transition-opacity hover:opacity-80"
 style={{ background: C.gold, color: '#111' }}
 >
 {syncing ? rv('syncing') : rv('syncFromPositions')}
 </button>
 </div>
 </div>

 {/* Table */}
 <div style={CARD_STYLE} className="overflow-x-auto">
 {loading ? (
 <div className="py-16 text-center text-sm" style={{ color: C.muted }}>
 {t('loading', language)}
 </div>
 ) : entries.length === 0 ? (
 <div className="py-16 text-center text-sm" style={{ color: C.muted }}>
 {rv('emptyJournal')}
 </div>
 ) : (
 <table className="w-full text-xs">
 <thead>
 <tr style={{ color: C.muted, borderBottom: `1px solid ${C.border}` }}>
 <th className="text-left font-medium px-4 py-3">{rv('colSymbol')}</th>
 <th className="text-left font-medium px-4 py-3">{rv('colDirection')}</th>
 <th className="text-right font-medium px-4 py-3">{rv('colEntry')}</th>
 <th className="text-right font-medium px-4 py-3">{rv('colExit')}</th>
 <th className="text-right font-medium px-4 py-3">{rv('colLeverage')}</th>
 <th className="text-right font-medium px-4 py-3">{rv('colPnL')}</th>
 <th className="text-right font-medium px-4 py-3">{rv('colPnlPct')}</th>
 <th className="text-left font-medium px-4 py-3">{rv('colTime')}</th>
 <th className="text-left font-medium px-4 py-3">{rv('colPlan')}</th>
 <th className="text-left font-medium px-4 py-3">{rv('colAdherence')}</th>
 <th className="text-left font-medium px-4 py-3">{rv('colEmotion')}</th>
 <th className="text-left font-medium px-4 py-3">{rv('colStatus')}</th>
 <th className="text-right font-medium px-4 py-3"></th>
 </tr>
 </thead>
 <tbody>
 {entries.map((e) => (
 <JournalRow key={e.id} entry={e} language={language} onEdit={() => setEditing(e)} />
 ))}
 </tbody>
 </table>
 )}
 </div>

 {editing && (
 <ReviewEditModal
 entry={editing}
 language={language}
 onClose={() => setEditing(null)}
 onSaved={() => {
 setEditing(null)
 load()
 }}
 />
 )}
 </div>
 )
}

function JournalRow({
 entry,
 language,
 onEdit,
}: {
 entry: JournalEntry
 language: Language
 onEdit: () => void
}) {
 const rv = (key: string) => t(`reviewPage.${key}`, language)
 const win = entry.realized_pnl > 0
 const pnlColor = win ? C.up : entry.realized_pnl < 0 ? C.down : C.muted

 const adherenceLabel = () => {
 switch (entry.executed_as_plan) {
 case 'yes':
 return <span style={{ color: C.up }}>{rv('adherenceYes')}</span>
 case 'partial':
 return <span style={{ color: C.warn }}>{rv('adherencePartial')}</span>
 case 'no':
 return <span style={{ color: C.down }}>{rv('adherenceNo')}</span>
 default:
 return <span style={{ color: C.faint }}>--</span>
 }
 }

 const hasPlan = entry.planned_stop_loss > 0 || entry.planned_take_profit > 0

 return (
 <tr style={{ borderBottom: `1px solid ${C.rowBorder}` }} className="hover:bg-[#1E1E1A]/[0.03]">
 <td className="px-4 py-3 font-semibold">{entry.symbol}</td>
 <td className="px-4 py-3">
 <span style={{ color: entry.side === 'LONG' ? C.up : C.down }}>
 {entry.side === 'LONG' ? rv('long') : rv('short')}
 </span>
 </td>
 <td className="px-4 py-3 text-right">{fmtPrice(entry.entry_price)}</td>
 <td className="px-4 py-3 text-right">{fmtPrice(entry.exit_price)}</td>
 <td className="px-4 py-3 text-right">{entry.leverage}x</td>
 <td className="px-4 py-3 text-right font-semibold" style={{ color: pnlColor }}>
 {entry.realized_pnl >= 0 ? '+' : ''}
 {entry.realized_pnl.toFixed(2)}
 </td>
 <td className="px-4 py-3 text-right" style={{ color: pnlColor }}>
 {entry.pnl_pct >= 0 ? '+' : ''}
 {entry.pnl_pct.toFixed(1)}%
 </td>
 <td className="px-4 py-3" style={{ color: C.muted }}>
 {fmtTime(entry.entry_time)} → {fmtTime(entry.exit_time)}
 </td>
 <td className="px-4 py-3" style={{ color: hasPlan ? C.muted : C.faint }}>
 {hasPlan ? `SL ${fmtPrice(entry.planned_stop_loss)} / TP ${fmtPrice(entry.planned_take_profit)}` : rv('noPlan')}
 </td>
 <td className="px-4 py-3">{adherenceLabel()}</td>
 <td className="px-4 py-3" style={{ color: entry.emotions ? C.text : C.faint }}>
 {entry.emotions || '--'}
 </td>
 <td className="px-4 py-3">
 {entry.review_status === 'reviewed' ? (
 <span
 className="px-2 py-0.5 rounded text-[10px]"
 style={{ background: 'rgba(46,125,79,0.12)', color: C.up }}
 >
 {rv('reviewed')}
 </span>
 ) : (
 <span
 className="px-2 py-0.5 rounded text-[10px]"
 style={{ background: 'rgba(184,145,42,0.12)', color: C.gold }}
 >
 {rv('pendingReview')}
 </span>
 )}
 </td>
 <td className="px-4 py-3 text-right">
 <button
 onClick={onEdit}
 className="text-xs rounded px-3 py-1.5 transition-colors hover:bg-[#1E1E1A]/5"
 style={{ border: `1px solid ${C.border}`, color: C.gold }}
 >
 {rv('reviewAction')}
 </button>
 </td>
 </tr>
 )
}

// ===================== Review Edit Modal =====================

function ReviewEditModal({
 entry,
 language,
 onClose,
 onSaved,
}: {
 entry: JournalEntry
 language: Language
 onClose: () => void
 onSaved: () => void
}) {
 const rv = (key: string) => t(`reviewPage.${key}`, language)
 const [executedAsPlan, setExecutedAsPlan] = useState(entry.executed_as_plan || '')
 const [emotions, setEmotions] = useState<string[]>(
 entry.emotions ? entry.emotions.split(',').filter(Boolean) : []
 )
 const [mistakeCategory, setMistakeCategory] = useState(entry.mistake_category || '')
 const [strategyTag, setStrategyTag] = useState(entry.strategy_tag || '')
 const [deviationNote, setDeviationNote] = useState(entry.deviation_note || '')
 const [lesson, setLesson] = useState(entry.lesson || '')
 const [saving, setSaving] = useState(false)

 const toggleEmotion = (emo: string) => {
 setEmotions((prev) => (prev.includes(emo) ? prev.filter((x) => x !== emo) : [...prev, emo]))
 }

 const save = async () => {
 setSaving(true)
 try {
 await api.updateJournalEntry(entry.id, entry.trader_id, {
 executed_as_plan: executedAsPlan,
 emotions: emotions.join(','),
 mistake_category: mistakeCategory,
 strategy_tag: strategyTag,
 deviation_note: deviationNote,
 lesson,
 })
 onSaved()
 } catch {
 /* toast handled by httpClient */
 } finally {
 setSaving(false)
 }
 }

 const win = entry.realized_pnl > 0

 return (
 <div
 className="fixed inset-0 z-50 flex items-center justify-center p-4"
 style={{ background: 'rgba(0,0,0,0.65)' }}
 onClick={onClose}
 >
 <div
 className="w-full max-w-2xl max-h-[85vh] overflow-y-auto rounded-xl p-6"
 style={{ background: C.card, border: `1px solid ${C.border}` }}
 onClick={(e) => e.stopPropagation()}
 >
 {/* Header: trade facts */}
 <div className="flex items-start justify-between mb-4">
 <div>
 <h3 className="text-base font-bold">
 {entry.symbol}{' '}
 <span style={{ color: entry.side === 'LONG' ? C.up : C.down }}>
 {entry.side === 'LONG' ? rv('long') : rv('short')} {entry.leverage}x
 </span>
 </h3>
 <p className="text-xs mt-1" style={{ color: C.muted }}>
 {fmtTime(entry.entry_time)} → {fmtTime(entry.exit_time)}
 {entry.close_reason ? ` · ${entry.close_reason}` : ''}
 </p>
 </div>
 <button onClick={onClose} className="text-lg px-2" style={{ color: C.muted }}>
 ✕
 </button>
 </div>

 {/* Facts grid */}
 <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-5">
 <FactBox label={rv('colEntry')} value={fmtPrice(entry.entry_price)} />
 <FactBox label={rv('colExit')} value={fmtPrice(entry.exit_price)} />
 <FactBox
 label={rv('colPnL')}
 value={`${entry.realized_pnl >= 0 ? '+' : ''}${entry.realized_pnl.toFixed(2)}`}
 color={win ? C.up : entry.realized_pnl < 0 ? C.down : C.muted}
 />
 <FactBox
 label={rv('colPnlPct')}
 value={`${entry.pnl_pct >= 0 ? '+' : ''}${entry.pnl_pct.toFixed(1)}%`}
 color={win ? C.up : entry.realized_pnl < 0 ? C.down : C.muted}
 />
 <FactBox
 label={rv('plannedSL')}
 value={entry.planned_stop_loss > 0 ? fmtPrice(entry.planned_stop_loss) : rv('noPlan')}
 color={entry.planned_stop_loss > 0 ? undefined : C.faint}
 />
 <FactBox
 label={rv('plannedTP')}
 value={entry.planned_take_profit > 0 ? fmtPrice(entry.planned_take_profit) : rv('noPlan')}
 color={entry.planned_take_profit > 0 ? undefined : C.faint}
 />
 <FactBox
 label={rv('colConfidence')}
 value={entry.confidence > 0 ? `${entry.confidence}%` : '--'}
 />
 <FactBox label={rv('colQuantity')} value={String(entry.quantity)} />
 </div>

 {/* Entry reasoning */}
 {entry.entry_reasoning && (
 <div className="mb-5 p-3 rounded-lg text-xs leading-relaxed" style={{ background: C.bg, color: C.muted }}>
 <div className="font-semibold mb-1" style={{ color: C.text }}>
 {rv('entryReasoning')}
 </div>
 {entry.entry_reasoning}
 </div>
 )}

 {/* Review form */}
 <div className="space-y-4">
 <div>
 <label className="text-xs font-medium block mb-2" style={{ color: C.muted }}>
 {rv('executedAsPlan')}
 </label>
 <div className="flex gap-2 flex-wrap">
 {[
 { v: 'yes', label: rv('adherenceYes') },
 { v: 'partial', label: rv('adherencePartial') },
 { v: 'no', label: rv('adherenceNo') },
 ].map((opt) => (
 <button
 key={opt.v}
 onClick={() => setExecutedAsPlan(opt.v)}
 className="px-4 py-1.5 rounded text-xs transition-colors"
 style={{
 border: `1px solid ${executedAsPlan === opt.v ? C.gold : C.border}`,
 color: executedAsPlan === opt.v ? C.gold : C.muted,
 background: executedAsPlan === opt.v ? 'rgba(184,145,42,0.08)' : 'transparent',
 }}
 >
 {opt.label}
 </button>
 ))}
 </div>
 </div>

 <div>
 <label className="text-xs font-medium block mb-2" style={{ color: C.muted }}>
 {rv('emotionState')}
 </label>
 <div className="flex gap-2 flex-wrap">
 {EMOTION_OPTIONS.map((emo) => (
 <button
 key={emo}
 onClick={() => toggleEmotion(emo)}
 className="px-3 py-1.5 rounded text-xs transition-colors"
 style={{
 border: `1px solid ${emotions.includes(emo) ? C.blue : C.border}`,
 color: emotions.includes(emo) ? C.blue : C.muted,
 background: emotions.includes(emo) ? 'rgba(75,158,255,0.08)' : 'transparent',
 }}
 >
 {rv(`emotion_${emo}`)}
 </button>
 ))}
 </div>
 </div>

 <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
 <div>
 <label className="text-xs font-medium block mb-2" style={{ color: C.muted }}>
 {rv('mistakeCategory')}
 </label>
 <select
 value={mistakeCategory}
 onChange={(e) => setMistakeCategory(e.target.value)}
 className="w-full text-xs rounded px-3 py-2 outline-none"
 style={{ background: C.bg, border: `1px solid ${C.border}`, color: C.text }}
 >
 <option value="">--</option>
 {MISTAKE_OPTIONS.map((opt) => (
 <option key={opt} value={opt}>
 {rv(`mistake_${opt}`)}
 </option>
 ))}
 </select>
 </div>
 <div>
 <label className="text-xs font-medium block mb-2" style={{ color: C.muted }}>
 {rv('strategyTag')}
 </label>
 <select
 value={strategyTag}
 onChange={(e) => setStrategyTag(e.target.value)}
 className="w-full text-xs rounded px-3 py-2 outline-none"
 style={{ background: C.bg, border: `1px solid ${C.border}`, color: C.text }}
 >
 <option value="">--</option>
 {STRATEGY_OPTIONS.map((opt) => (
 <option key={opt} value={opt}>
 {rv(`strategy_${opt}`)}
 </option>
 ))}
 </select>
 </div>
 </div>

 <div>
 <label className="text-xs font-medium block mb-2" style={{ color: C.muted }}>
 {rv('deviationNote')}
 </label>
 <textarea
 value={deviationNote}
 onChange={(e) => setDeviationNote(e.target.value)}
 rows={2}
 placeholder={rv('deviationNotePlaceholder')}
 className="w-full text-xs rounded px-3 py-2 outline-none resize-none"
 style={{ background: C.bg, border: `1px solid ${C.border}`, color: C.text }}
 />
 </div>

 <div>
 <label className="text-xs font-medium block mb-2" style={{ color: C.muted }}>
 {rv('lesson')}
 </label>
 <textarea
 value={lesson}
 onChange={(e) => setLesson(e.target.value)}
 rows={3}
 placeholder={rv('lessonPlaceholder')}
 className="w-full text-xs rounded px-3 py-2 outline-none resize-none"
 style={{ background: C.bg, border: `1px solid ${C.border}`, color: C.text }}
 />
 </div>
 </div>

 <div className="flex justify-end gap-3 mt-6">
 <button
 onClick={onClose}
 className="px-4 py-2 rounded text-xs"
 style={{ border: `1px solid ${C.border}`, color: C.muted }}
 >
 {t('cancel', language)}
 </button>
 <button
 onClick={save}
 disabled={saving}
 className="px-5 py-2 rounded text-xs font-semibold transition-opacity hover:opacity-80"
 style={{ background: C.gold, color: '#111' }}
 >
 {saving ? t('loading', language) : rv('saveReview')}
 </button>
 </div>
 </div>
 </div>
 )
}

function FactBox({ label, value, color }: { label: string; value: string; color?: string }) {
 return (
 <div className="p-2.5 rounded-lg" style={{ background: C.bg }}>
 <div className="text-[10px] mb-1" style={{ color: C.faint }}>
 {label}
 </div>
 <div className="text-xs font-semibold" style={{ color: color || C.text }}>
 {value}
 </div>
 </div>
 )
}

// ===================== Stats Tab =====================

function StatsTab({ traderId, language }: { traderId: string; language: Language }) {
 const rv = useCallback(
 (key: string, params?: Record<string, string | number>) =>
 t(`reviewPage.${key}`, language, params),
 [language]
 )
 const [stats, setStats] = useState<JournalStats | null>(null)
 const [loading, setLoading] = useState(true)

 useEffect(() => {
 setLoading(true)
 api
 .getJournalStats(traderId)
 .then(setStats)
 .catch(() => setStats(null))
 .finally(() => setLoading(false))
 }, [traderId])

 if (loading) {
 return (
 <div className="py-16 text-center text-sm" style={{ color: C.muted }}>
 {t('loading', language)}
 </div>
 )
 }
 if (!stats || stats.total_entries === 0) {
 return (
 <div className="py-16 text-center text-sm" style={{ color: C.muted }}>
 {rv('emptyStats')}
 </div>
 )
 }

 const plRatio = stats.avg_loss > 0 ? stats.avg_win / stats.avg_loss : 0

 return (
 <div className="space-y-5">
 {/* Core metrics */}
 <div className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-6 gap-3">
 <MetricCard label={rv('totalTrades')} value={String(stats.total_entries)} sub={`${rv('reviewedCount')} ${stats.reviewed_count}`} />
 <MetricCard
 label={rv('winRate')}
 value={`${stats.win_rate.toFixed(1)}%`}
 sub={`${stats.win_trades}W / ${stats.loss_trades}L`}
 color={stats.win_rate >= 50 ? C.up : C.down}
 />
 <MetricCard
 label={rv('plRatio')}
 value={plRatio > 0 ? plRatio.toFixed(2) : '--'}
 sub={`+${stats.avg_win.toFixed(1)} / -${stats.avg_loss.toFixed(1)}`}
 />
 <MetricCard
 label={rv('expectancy')}
 value={`${stats.expectancy >= 0 ? '+' : ''}${stats.expectancy.toFixed(2)}`}
 sub={rv('expectancyDesc')}
 color={stats.expectancy >= 0 ? C.up : C.down}
 />
 <MetricCard
 label={rv('profitFactor')}
 value={stats.profit_factor > 0 ? stats.profit_factor.toFixed(2) : '--'}
 color={stats.profit_factor >= 1 ? C.up : C.down}
 />
 <MetricCard
 label={rv('totalPnL')}
 value={`${stats.total_pnl >= 0 ? '+' : ''}${stats.total_pnl.toFixed(2)}`}
 color={stats.total_pnl >= 0 ? C.up : C.down}
 />
 </div>

 {/* Execution layer: adherence + plan coverage */}
 <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
 <div style={CARD_STYLE} className="p-4">
 <h3 className="text-sm font-bold mb-3">{rv('adherenceSection')}</h3>
 <GroupStatsTable groups={stats.adherence_plan} language={language} labelMap={(k) => rv(`adherence_${k}`)} />
 <div className="mt-3 pt-3 text-xs flex items-center justify-between" style={{ borderTop: `1px solid ${C.rowBorder}`, color: C.muted }}>
 <span>{rv('planCoverage')}</span>
 <span>
 {rv('withPlanWinRate', {
 count: stats.with_plan_count,
 rate: stats.with_plan_win_rate.toFixed(1),
 })}
 {' · '}
 {rv('noPlanWinRate', {
 count: stats.no_plan_count,
 rate: stats.no_plan_win_rate.toFixed(1),
 })}
 </span>
 </div>
 </div>

 <div style={CARD_STYLE} className="p-4">
 <h3 className="text-sm font-bold mb-3">{rv('emotionSection')}</h3>
 <GroupStatsTable groups={stats.emotion_stats} language={language} labelMap={(k) => rv(`emotion_${k}`)} />
 </div>

 <div style={CARD_STYLE} className="p-4">
 <h3 className="text-sm font-bold mb-3">{rv('mistakeSection')}</h3>
 <GroupStatsTable groups={stats.mistake_stats} language={language} labelMap={(k) => rv(`mistake_${k}`)} />
 </div>

 <div style={CARD_STYLE} className="p-4">
 <h3 className="text-sm font-bold mb-3">{rv('strategySection')}</h3>
 <GroupStatsTable groups={stats.strategy_stats} language={language} labelMap={(k) => rv(`strategy_${k}`)} />
 </div>
 </div>
 </div>
 )
}

function MetricCard({ label, value, sub, color }: { label: string; value: string; sub?: string; color?: string }) {
 return (
 <div style={CARD_STYLE} className="p-4">
 <div className="text-[11px] mb-1.5" style={{ color: C.muted }}>
 {label}
 </div>
 <div className="text-lg font-bold" style={{ color: color || C.text }}>
 {value}
 </div>
 {sub && (
 <div className="text-[10px] mt-1" style={{ color: C.faint }}>
 {sub}
 </div>
 )}
 </div>
 )
}

function GroupStatsTable({
 groups,
 language,
 labelMap,
}: {
 groups: { key: string; count: number; win_rate: number; total_pnl: number; avg_pnl: number }[]
 language: Language
 labelMap: (key: string) => string
}) {
 const rv = (key: string) => t(`reviewPage.${key}`, language)
 if (!groups || groups.length === 0) {
 return (
 <div className="py-6 text-center text-xs" style={{ color: C.faint }}>
 {rv('noData')}
 </div>
 )
 }
 return (
 <table className="w-full text-xs">
 <thead>
 <tr style={{ color: C.faint }}>
 <th className="text-left font-medium pb-2">{rv('groupCol')}</th>
 <th className="text-right font-medium pb-2">{rv('countCol')}</th>
 <th className="text-right font-medium pb-2">{rv('winRateCol')}</th>
 <th className="text-right font-medium pb-2">{rv('totalPnlCol')}</th>
 <th className="text-right font-medium pb-2">{rv('avgPnlCol')}</th>
 </tr>
 </thead>
 <tbody>
 {groups.map((g) => (
 <tr key={g.key} style={{ borderTop: `1px solid ${C.rowBorder}` }}>
 <td className="py-2">{labelMap(g.key)}</td>
 <td className="py-2 text-right">{g.count}</td>
 <td className="py-2 text-right" style={{ color: g.win_rate >= 50 ? C.up : C.down }}>
 {g.win_rate.toFixed(0)}%
 </td>
 <td
 className="py-2 text-right"
 style={{ color: g.total_pnl >= 0 ? C.up : C.down }}
 >
 {g.total_pnl >= 0 ? '+' : ''}
 {g.total_pnl.toFixed(1)}
 </td>
 <td
 className="py-2 text-right"
 style={{ color: g.avg_pnl >= 0 ? C.up : C.down }}
 >
 {g.avg_pnl >= 0 ? '+' : ''}
 {g.avg_pnl.toFixed(1)}
 </td>
 </tr>
 ))}
 </tbody>
 </table>
 )
}

// ===================== Rules Tab =====================

function RulesTab({ traderId, language }: { traderId: string; language: Language }) {
 const rv = useCallback(
 (key: string, params?: Record<string, string | number>) =>
 t(`reviewPage.${key}`, language, params),
 [language]
 )
 const [rules, setRules] = useState<TradingRule[]>([])
 const [logs, setLogs] = useState<RuleCheckLog[]>([])
 const [loading, setLoading] = useState(true)
 const [showCreate, setShowCreate] = useState(false)
 const [proposals, setProposals] = useState<RuleProposal[] | null>(null)
 const [extracting, setExtracting] = useState(false)
 const [applyResult, setApplyResult] = useState('')

 const load = useCallback(async () => {
 setLoading(true)
 try {
 const [ruleRes, logRes] = await Promise.all([api.getRules(traderId), api.getRuleLogs(traderId, 30)])
 setRules(ruleRes.rules || [])
 setLogs(logRes.logs || [])
 } catch {
 setRules([])
 setLogs([])
 } finally {
 setLoading(false)
 }
 // eslint-disable-next-line react-hooks/exhaustive-deps
 }, [traderId])

 useEffect(() => {
 if (traderId) load()
 }, [traderId, load])

 const toggleEnabled = async (rule: TradingRule) => {
 try {
 await api.updateRule(rule.id, traderId, { enabled: !rule.enabled })
 load()
 } catch {
 /* ignore */
 }
 }

 const deleteRule = async (rule: TradingRule) => {
 try {
 await api.deleteRule(rule.id, traderId)
 load()
 } catch {
 /* ignore */
 }
 }

 const handleExtract = async () => {
 setExtracting(true)
 setApplyResult('')
 try {
 const res = await api.aiExtractRules(traderId)
 setProposals(res.proposals || [])
 } catch (err) {
 setProposals([])
 setApplyResult(err instanceof Error ? err.message : String(err))
 } finally {
 setExtracting(false)
 }
 }

 const handleApplyProposals = async () => {
 if (!proposals || proposals.length === 0) return
 try {
 const res = await api.aiApplyRules(
 traderId,
 proposals.map((p) => ({
 rule_type: p.rule_type,
 name: p.name,
 description: p.description,
 condition: p.condition,
 on_violation: p.on_violation,
 lesson_text: p.lesson_text,
 tags: p.tags,
 source_stats: p.source_stats,
 }))
 )
 setApplyResult(rv('rulesApplied', { count: res.saved }))
 setProposals(null)
 load()
 } catch (err) {
 setApplyResult(err instanceof Error ? err.message : String(err))
 }
 }

 const hardRules = rules.filter((r) => r.rule_type === 'hard')
 const softRules = rules.filter((r) => r.rule_type === 'soft')

 return (
 <div className="space-y-5">
 {/* Toolbar */}
 <div className="flex items-center justify-between flex-wrap gap-3">
 <p className="text-xs" style={{ color: C.muted }}>
 {rv('rulesIntro')}
 </p>
 <div className="flex gap-2">
 <button
 onClick={handleExtract}
 disabled={extracting}
 className="text-xs font-semibold rounded px-4 py-2 transition-opacity hover:opacity-80"
 style={{ border: `1px solid ${C.gold}`, color: C.gold, background: 'rgba(184,145,42,0.06)' }}
 >
 {extracting ? rv('extracting') : rv('aiExtractRules')}
 </button>
 <button
 onClick={() => setShowCreate(true)}
 className="text-xs font-semibold rounded px-4 py-2 transition-opacity hover:opacity-80"
 style={{ background: C.gold, color: '#111' }}
 >
 {rv('addRule')}
 </button>
 </div>
 </div>

 {applyResult && (
 <div className="text-xs px-4 py-3 rounded-lg" style={{ background: C.card, border: `1px solid ${C.border}`, color: C.muted }}>
 {applyResult}
 </div>
 )}

 {/* AI proposals */}
 {proposals && (
 <div style={CARD_STYLE} className="p-4">
 <div className="flex items-center justify-between mb-3">
 <h3 className="text-sm font-bold">{rv('aiProposals')}</h3>
 <div className="flex gap-2">
 {proposals.length > 0 && (
 <button
 onClick={handleApplyProposals}
 className="text-xs px-3 py-1.5 rounded"
 style={{ background: C.gold, color: '#111' }}
 >
 {rv('applyAll')}
 </button>
 )}
 <button
 onClick={() => setProposals(null)}
 className="text-xs px-3 py-1.5 rounded"
 style={{ border: `1px solid ${C.border}`, color: C.muted }}
 >
 ✕
 </button>
 </div>
 </div>
 {proposals.length === 0 ? (
 <p className="text-xs py-4 text-center" style={{ color: C.faint }}>
 {rv('noProposals')}
 </p>
 ) : (
 <div className="space-y-2">
 {proposals.map((p, i) => (
 <ProposalRow key={i} proposal={p} language={language} />
 ))}
 </div>
 )}
 </div>
 )}

 {/* Hard rules */}
 <div style={CARD_STYLE} className="p-4">
 <h3 className="text-sm font-bold mb-3">
 {rv('hardRules')} <span className="text-xs font-normal" style={{ color: C.muted }}>({hardRules.length})</span>
 </h3>
 {loading ? (
 <div className="py-6 text-center text-xs" style={{ color: C.muted }}>
 {t('loading', language)}
 </div>
 ) : hardRules.length === 0 ? (
 <p className="py-6 text-center text-xs" style={{ color: C.faint }}>
 {rv('noHardRules')}
 </p>
 ) : (
 <div className="space-y-2">
 {hardRules.map((r) => (
 <RuleRow key={r.id} rule={r} language={language} onToggle={() => toggleEnabled(r)} onDelete={() => deleteRule(r)} />
 ))}
 </div>
 )}
 </div>

 {/* Soft lessons */}
 <div style={CARD_STYLE} className="p-4">
 <h3 className="text-sm font-bold mb-3">
 {rv('softRules')} <span className="text-xs font-normal" style={{ color: C.muted }}>({softRules.length})</span>
 </h3>
 {!loading && softRules.length === 0 ? (
 <p className="py-6 text-center text-xs" style={{ color: C.faint }}>
 {rv('noSoftRules')}
 </p>
 ) : (
 <div className="space-y-2">
 {softRules.map((r) => (
 <RuleRow key={r.id} rule={r} language={language} onToggle={() => toggleEnabled(r)} onDelete={() => deleteRule(r)} />
 ))}
 </div>
 )}
 </div>

 {/* Check logs */}
 <div style={CARD_STYLE} className="p-4">
 <h3 className="text-sm font-bold mb-3">{rv('checkLogs')}</h3>
 {logs.length === 0 ? (
 <p className="py-6 text-center text-xs" style={{ color: C.faint }}>
 {rv('noCheckLogs')}
 </p>
 ) : (
 <table className="w-full text-xs">
 <thead>
 <tr style={{ color: C.faint }}>
 <th className="text-left font-medium pb-2">{rv('colTime')}</th>
 <th className="text-left font-medium pb-2">{rv('colSymbol')}</th>
 <th className="text-left font-medium pb-2">{rv('ruleName')}</th>
 <th className="text-left font-medium pb-2">{rv('checkResult')}</th>
 </tr>
 </thead>
 <tbody>
 {logs.map((log) => (
 <tr key={log.id} style={{ borderTop: `1px solid ${C.rowBorder}` }}>
 <td className="py-2" style={{ color: C.muted }}>
 {fmtTime(log.created_at)}
 </td>
 <td className="py-2 font-semibold">
 {log.symbol} <span style={{ color: C.faint }}>{log.action}</span>
 </td>
 <td className="py-2">{log.rule_name}</td>
 <td className="py-2">
 {log.blocked ? (
 <span style={{ color: C.down }}>{rv('resultBlocked')}</span>
 ) : (
 <span style={{ color: C.warn }}>{rv('resultWarned')}</span>
 )}
 </td>
 </tr>
 ))}
 </tbody>
 </table>
 )}
 </div>

 {showCreate && (
 <CreateRuleModal
 traderId={traderId}
 language={language}
 onClose={() => setShowCreate(false)}
 onSaved={() => {
 setShowCreate(false)
 load()
 }}
 />
 )}
 </div>
 )
}

function RuleRow({
 rule,
 language,
 onToggle,
 onDelete,
}: {
 rule: TradingRule
 language: Language
 onToggle: () => void
 onDelete: () => void
}) {
 const rv = (key: string, params?: Record<string, string | number>) =>
 t(`reviewPage.${key}`, language, params)
 return (
 <div
 className="flex items-start justify-between gap-3 p-3 rounded-lg"
 style={{ background: C.bg, border: `1px solid ${C.rowBorder}` }}
 >
 <div className="flex-1 min-w-0">
 <div className="flex items-center gap-2 flex-wrap">
 <span className="text-xs font-semibold">{rule.name}</span>
 {rule.on_violation === 'block' ? (
 <span
 className="px-1.5 py-0.5 rounded text-[10px]"
 style={{ background: 'rgba(192,57,43,0.12)', color: C.down }}
 >
 {rv('violationBlock')}
 </span>
 ) : (
 <span
 className="px-1.5 py-0.5 rounded text-[10px]"
 style={{ background: 'rgba(184,145,42,0.12)', color: C.warn }}
 >
 {rv('violationWarn')}
 </span>
 )}
 {rule.source === 'ai_review' && (
 <span
 className="px-1.5 py-0.5 rounded text-[10px]"
 style={{ background: 'rgba(75,158,255,0.12)', color: C.blue }}
 >
 AI
 </span>
 )}
 <span className="text-[10px]" style={{ color: C.faint }}>
 {rv('ruleHits', { hits: rule.hit_count, blocks: rule.block_count })}
 </span>
 </div>
 {rule.rule_type === 'hard' && (
 <div className="text-[11px] mt-1 font-mono" style={{ color: C.blue }}>
 {rule.condition_json}
 </div>
 )}
 <div className="text-[11px] mt-1" style={{ color: C.muted }}>
 {rule.rule_type === 'hard' ? rule.description : rule.lesson_text}
 </div>
 </div>
 <div className="flex items-center gap-2 shrink-0">
 <button
 onClick={onToggle}
 className="w-9 h-5 rounded-full relative transition-colors"
 style={{ background: rule.enabled ? C.up : C.border }}
 title={rule.enabled ? rv('enabled') : rv('disabled')}
 >
 <span
 className="absolute top-0.5 w-4 h-4 rounded-full bg-white transition-all"
 style={{ left: rule.enabled ? 18 : 2 }}
 />
 </button>
 <button onClick={onDelete} className="text-xs px-2 py-1" style={{ color: C.faint }}>
 🗑
 </button>
 </div>
 </div>
 )
}

function ProposalRow({ proposal, language }: { proposal: RuleProposal; language: Language }) {
 const rv = (key: string) => t(`reviewPage.${key}`, language)
 return (
 <div className="p-3 rounded-lg text-xs" style={{ background: C.bg, border: `1px solid ${C.rowBorder}` }}>
 <div className="flex items-center gap-2 flex-wrap">
 <span
 className="px-1.5 py-0.5 rounded text-[10px]"
 style={{
 background: proposal.rule_type === 'hard' ? 'rgba(75,158,255,0.12)' : 'rgba(184,145,42,0.12)',
 color: proposal.rule_type === 'hard' ? C.blue : C.gold,
 }}
 >
 {proposal.rule_type === 'hard' ? rv('typeHard') : rv('typeSoft')}
 </span>
 <span className="font-semibold">{proposal.name}</span>
 </div>
 {proposal.rule_type === 'hard' && proposal.condition && (
 <div className="text-[11px] mt-1 font-mono" style={{ color: C.blue }}>
 {proposal.condition}
 </div>
 )}
 <div className="text-[11px] mt-1" style={{ color: C.muted }}>
 {proposal.rule_type === 'hard' ? proposal.description : proposal.lesson_text}
 </div>
 </div>
 )
}

function CreateRuleModal({
 traderId,
 language,
 onClose,
 onSaved,
}: {
 traderId: string
 language: Language
 onClose: () => void
 onSaved: () => void
}) {
 const rv = (key: string) => t(`reviewPage.${key}`, language)
 const [ruleType, setRuleType] = useState<'hard' | 'soft'>('hard')
 const [name, setName] = useState('')
 const [description, setDescription] = useState('')
 const [field, setField] = useState('leverage')
 const [op, setOp] = useState('<=')
 const [value, setValue] = useState('10')
 const [onViolation, setOnViolation] = useState<'block' | 'warn'>('block')
 const [lessonText, setLessonText] = useState('')
 const [tags, setTags] = useState('')
 const [saving, setSaving] = useState(false)
 const [error, setError] = useState('')

 const conditionJSON = useMemo(() => {
 if (ruleType !== 'hard') return ''
 let parsed: number | string | boolean = value
 if (field === 'symbol') {
 parsed = value
 } else if (field === 'has_stop_loss' || field === 'has_take_profit') {
 parsed = value === 'true'
 } else {
 const n = parseFloat(value)
 parsed = isNaN(n) ? value : n
 }
 return JSON.stringify({ field, op, value: parsed })
 }, [ruleType, field, op, value])

 const save = async () => {
 setError('')
 if (!name.trim()) {
 setError(rv('ruleNameRequired'))
 return
 }
 setSaving(true)
 try {
 await api.createRule(traderId, {
 rule_type: ruleType,
 name: name.trim(),
 description: description.trim(),
 condition: conditionJSON,
 on_violation: onViolation,
 lesson_text: lessonText.trim(),
 tags: tags.trim(),
 })
 onSaved()
 } catch (err) {
 setError(err instanceof Error ? err.message : String(err))
 } finally {
 setSaving(false)
 }
 }

 const inputStyle: React.CSSProperties = {
 background: C.bg,
 border: `1px solid ${C.border}`,
 color: C.text,
 }

 return (
 <div
 className="fixed inset-0 z-50 flex items-center justify-center p-4"
 style={{ background: 'rgba(0,0,0,0.65)' }}
 onClick={onClose}
 >
 <div
 className="w-full max-w-lg rounded-xl p-6"
 style={{ background: C.card, border: `1px solid ${C.border}` }}
 onClick={(e) => e.stopPropagation()}
 >
 <div className="flex items-center justify-between mb-4">
 <h3 className="text-base font-bold">{rv('addRule')}</h3>
 <button onClick={onClose} className="text-lg px-2" style={{ color: C.muted }}>
 ✕
 </button>
 </div>

 <div className="space-y-4">
 <div className="flex gap-2">
 {(['hard', 'soft'] as const).map((tp) => (
 <button
 key={tp}
 onClick={() => setRuleType(tp)}
 className="flex-1 px-4 py-2 rounded text-xs transition-colors"
 style={{
 border: `1px solid ${ruleType === tp ? C.gold : C.border}`,
 color: ruleType === tp ? C.gold : C.muted,
 background: ruleType === tp ? 'rgba(184,145,42,0.08)' : 'transparent',
 }}
 >
 {tp === 'hard' ? rv('typeHard') : rv('typeSoft')}
 </button>
 ))}
 </div>

 <div>
 <label className="text-xs block mb-1.5" style={{ color: C.muted }}>
 {rv('ruleName')}
 </label>
 <input
 value={name}
 onChange={(e) => setName(e.target.value)}
 className="w-full text-xs rounded px-3 py-2 outline-none"
 style={inputStyle}
 />
 </div>

 {ruleType === 'hard' ? (
 <>
 <div>
 <label className="text-xs block mb-1.5" style={{ color: C.muted }}>
 {rv('ruleCondition')}
 </label>
 <div className="flex gap-2">
 <select
 value={field}
 onChange={(e) => setField(e.target.value)}
 className="flex-1 text-xs rounded px-2 py-2 outline-none"
 style={inputStyle}
 >
 {RULE_FIELDS.map((f) => (
 <option key={f} value={f}>
 {f}
 </option>
 ))}
 </select>
 <select
 value={op}
 onChange={(e) => setOp(e.target.value)}
 className="w-20 text-xs rounded px-2 py-2 outline-none"
 style={inputStyle}
 >
 {['>', '>=', '<', '<=', '==', '!=', 'in'].map((o) => (
 <option key={o} value={o}>
 {o}
 </option>
 ))}
 </select>
 <input
 value={value}
 onChange={(e) => setValue(e.target.value)}
 className="flex-1 text-xs rounded px-3 py-2 outline-none"
 style={inputStyle}
 />
 </div>
 <div className="text-[10px] mt-1.5 font-mono" style={{ color: C.faint }}>
 {conditionJSON}
 </div>
 </div>
 <div>
 <label className="text-xs block mb-1.5" style={{ color: C.muted }}>
 {rv('violationAction')}
 </label>
 <div className="flex gap-2">
 {(['block', 'warn'] as const).map((act) => (
 <button
 key={act}
 onClick={() => setOnViolation(act)}
 className="flex-1 px-4 py-2 rounded text-xs transition-colors"
 style={{
 border: `1px solid ${onViolation === act ? (act === 'block' ? C.down : C.warn) : C.border}`,
 color: onViolation === act ? (act === 'block' ? C.down : C.warn) : C.muted,
 background: 'transparent',
 }}
 >
 {act === 'block' ? rv('violationBlock') : rv('violationWarn')}
 </button>
 ))}
 </div>
 </div>
 <div>
 <label className="text-xs block mb-1.5" style={{ color: C.muted }}>
 {rv('ruleDescription')}
 </label>
 <input
 value={description}
 onChange={(e) => setDescription(e.target.value)}
 className="w-full text-xs rounded px-3 py-2 outline-none"
 style={inputStyle}
 />
 </div>
 </>
 ) : (
 <>
 <div>
 <label className="text-xs block mb-1.5" style={{ color: C.muted }}>
 {rv('lessonText')}
 </label>
 <textarea
 value={lessonText}
 onChange={(e) => setLessonText(e.target.value)}
 rows={3}
 className="w-full text-xs rounded px-3 py-2 outline-none resize-none"
 style={inputStyle}
 />
 </div>
 <div>
 <label className="text-xs block mb-1.5" style={{ color: C.muted }}>
 {rv('ruleTags')}
 </label>
 <input
 value={tags}
 onChange={(e) => setTags(e.target.value)}
 placeholder="fomo, high_leverage"
 className="w-full text-xs rounded px-3 py-2 outline-none"
 style={inputStyle}
 />
 </div>
 </>
 )}

 {error && (
 <p className="text-xs" style={{ color: C.down }}>
 {error}
 </p>
 )}
 </div>

 <div className="flex justify-end gap-3 mt-6">
 <button
 onClick={onClose}
 className="px-4 py-2 rounded text-xs"
 style={{ border: `1px solid ${C.border}`, color: C.muted }}
 >
 {t('cancel', language)}
 </button>
 <button
 onClick={save}
 disabled={saving}
 className="px-5 py-2 rounded text-xs font-semibold transition-opacity hover:opacity-80"
 style={{ background: C.gold, color: '#111' }}
 >
 {saving ? t('loading', language) : rv('saveRule')}
 </button>
 </div>
 </div>
 </div>
 )
}

// ===================== AI Review Tab =====================

function AITab({ traderId, language }: { traderId: string; language: Language }) {
 const rv = useCallback(
 (key: string) => t(`reviewPage.${key}`, language),
 [language]
 )
 const [period, setPeriod] = useState<'daily' | 'weekly' | 'monthly'>('weekly')
 const [running, setRunning] = useState(false)
 const [result, setResult] = useState<AIReviewResult | null>(null)
 const [error, setError] = useState('')

 // Prompt settings state
 const [promptCfg, setPromptCfg] = useState<ReviewPromptConfig | null>(null)
 const [reviewPrompt, setReviewPrompt] = useState('')
 const [rulePrompt, setRulePrompt] = useState('')
 const [showPrompts, setShowPrompts] = useState(false)
 const [savingPrompts, setSavingPrompts] = useState(false)
 const [promptMsg, setPromptMsg] = useState('')

 useEffect(() => {
 api
 .getPromptConfig()
 .then((cfg) => {
 setPromptCfg(cfg)
 setReviewPrompt(cfg.review_system_prompt)
 setRulePrompt(cfg.rule_extract_system_prompt)
 })
 .catch(() => {})
 }, [])

 const savePrompts = async () => {
 setSavingPrompts(true)
 setPromptMsg('')
 try {
 await api.savePromptConfig({
 review_system_prompt: reviewPrompt,
 rule_extract_system_prompt: rulePrompt,
 })
 const cfg = await api.getPromptConfig()
 setPromptCfg(cfg)
 setReviewPrompt(cfg.review_system_prompt)
 setRulePrompt(cfg.rule_extract_system_prompt)
 setPromptMsg(rv('promptSaved'))
 } catch {
 setPromptMsg(rv('promptLoadFail'))
 } finally {
 setSavingPrompts(false)
 }
 }

 const run = async () => {
 setRunning(true)
 setError('')
 try {
 const res = await api.aiReview(traderId, period)
 setResult(res)
 } catch (err) {
 setError(err instanceof Error ? err.message : String(err))
 } finally {
 setRunning(false)
 }
 }

 return (
 <div className="space-y-5">
 <div style={CARD_STYLE} className="p-4">
 <p className="text-xs mb-4" style={{ color: C.muted }}>
 {rv('aiReviewIntro')}
 </p>
 <div className="flex items-center gap-3 flex-wrap">
 <div className="flex gap-1 p-1 rounded-lg" style={{ background: C.bg }}>
 {(['daily', 'weekly', 'monthly'] as const).map((p) => (
 <button
 key={p}
 onClick={() => setPeriod(p)}
 className="px-4 py-1.5 rounded text-xs transition-colors"
 style={{
 background: period === p ? C.gold : 'transparent',
 color: period === p ? '#111' : C.muted,
 fontWeight: period === p ? 600 : 400,
 }}
 >
 {rv(`period_${p}`)}
 </button>
 ))}
 </div>
 <button
 onClick={run}
 disabled={running}
 className="px-5 py-2 rounded text-xs font-semibold transition-opacity hover:opacity-80"
 style={{ background: C.gold, color: '#111' }}
 >
 {running ? rv('aiRunning') : rv('aiRunReview')}
 </button>
 <button
 onClick={() => setShowPrompts((v) => !v)}
 className="px-3 py-2 rounded text-xs transition-opacity hover:opacity-80"
 style={{ color: C.muted, border: `1px solid ${C.border}` }}
 >
 {rv('promptSettings')}
 {promptCfg && (promptCfg.review_custom || promptCfg.rule_extract_custom) && (
 <span
 className="ml-1.5 px-1.5 py-0.5 rounded text-[9px]"
 style={{ background: 'rgba(184,145,42,0.15)', color: C.gold }}
 >
 {rv('promptCustomActive')}
 </span>
 )}
 </button>
 </div>
 {error && (
 <p className="text-xs mt-3" style={{ color: C.down }}>
 {error}
 </p>
 )}

 {showPrompts && (
 <div
 className="mt-4 pt-4 space-y-4"
 style={{ borderTop: `1px solid ${C.border}` }}
 >
 <div>
 <div className="flex items-center justify-between mb-1">
 <label className="text-xs font-medium" style={{ color: C.text }}>
 {rv('promptReview')}
 </label>
 {promptCfg?.review_custom && (
 <span className="text-[10px]" style={{ color: C.gold }}>
 {rv('promptCustomActive')}
 </span>
 )}
 </div>
 <textarea
 value={reviewPrompt}
 onChange={(e) => setReviewPrompt(e.target.value)}
 rows={10}
 className="w-full p-2 rounded text-xs font-mono resize-y"
 style={{ background: C.bg, color: C.text, border: `1px solid ${C.border}` }}
 />
 </div>
 <div>
 <div className="flex items-center justify-between mb-1">
 <label className="text-xs font-medium" style={{ color: C.text }}>
 {rv('promptRuleExtract')}
 </label>
 {promptCfg?.rule_extract_custom && (
 <span className="text-[10px]" style={{ color: C.gold }}>
 {rv('promptCustomActive')}
 </span>
 )}
 </div>
 <textarea
 value={rulePrompt}
 onChange={(e) => setRulePrompt(e.target.value)}
 rows={8}
 className="w-full p-2 rounded text-xs font-mono resize-y"
 style={{ background: C.bg, color: C.text, border: `1px solid ${C.border}` }}
 />
 </div>
 <p className="text-[10px]" style={{ color: C.faint }}>
 {rv('promptResetHint')}
 </p>
 <div className="flex items-center gap-2">
 <button
 onClick={savePrompts}
 disabled={savingPrompts}
 className="px-4 py-1.5 rounded text-xs font-semibold transition-opacity hover:opacity-80"
 style={{ background: C.gold, color: '#111' }}
 >
 {savingPrompts ? t('loading', language) : rv('promptSave')}
 </button>
 <button
 onClick={() => {
 setReviewPrompt('')
 setRulePrompt('')
 }}
 className="px-3 py-1.5 rounded text-xs"
 style={{ color: C.muted, border: `1px solid ${C.border}` }}
 >
 {rv('promptReset')}
 </button>
 {promptMsg && (
 <span className="text-xs" style={{ color: C.up }}>
 {promptMsg}
 </span>
 )}
 </div>
 {!promptCfg?.review_custom && !promptCfg?.rule_extract_custom && (
 <p className="text-[10px]" style={{ color: C.faint }}>
 {rv('promptUsingDefault')}
 </p>
 )}
 </div>
 )}
 </div>

 {result && (
 <div style={CARD_STYLE} className="p-5">
 <div className="flex items-center justify-between mb-3">
 <h3 className="text-sm font-bold">{rv('aiReviewResult')}</h3>
 <span className="text-[10px]" style={{ color: C.faint }}>
 {fmtTime(result.generated_at)}
 </span>
 </div>
 <pre
 className="text-xs leading-relaxed whitespace-pre-wrap font-sans"
 style={{ color: C.text }}
 >
 {result.ai_response}
 </pre>
 </div>
 )}
 </div>
 )
}

// Expose violations renderer for possible reuse (kept minimal)
export type { RuleViolation }
