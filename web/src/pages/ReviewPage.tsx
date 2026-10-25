import { useCallback, useEffect, useMemo, useState } from 'react'
import { Trash2 } from 'lucide-react'
import { api } from '../lib/api'
import { t } from '../i18n/translations'
import type {
  JournalEntry,
  JournalStats,
  TradingRule,
  RuleCheckLog,
  RuleProposal,
  RuleVerification,
  RuleViolation,
  TraderInfo,
  AIReviewResult,
  ReviewPromptConfig,
} from '../types'
import type { Language } from '../i18n/translations'
import {
  Badge,
  Button,
  Card,
  CardHeader,
  CardBody,
  Change,
  PnL,
  Tabs,
  Segmented,
  Stat,
  EmptyState,
} from '../components/ui'

const INPUT_CLS =
  'h-8 w-full rounded-md border border-line bg-surface-2 px-3 text-[13px] text-fg outline-none placeholder:text-fg-3 hover:border-line-strong focus:border-brand focus:ring-1 focus:ring-brand/40'
const TEXTAREA_CLS =
  'w-full rounded-md border border-line bg-surface-2 px-3 py-2 text-[13px] text-fg outline-none placeholder:text-fg-3 hover:border-line-strong focus:border-brand focus:ring-1 focus:ring-brand/40 resize-none'
const LABEL_CLS = 'mb-1 block text-xs font-medium text-fg-3'
const CHIP_BASE =
  'inline-flex h-7 items-center rounded-md border px-3 text-xs font-medium transition-colors'
const chipCls = (
  active: boolean,
  tone: 'brand' | 'info' | 'down' | 'warn' = 'brand'
) =>
  active
    ? tone === 'info'
      ? `${CHIP_BASE} border-info/50 bg-info-soft text-info`
      : tone === 'down'
        ? `${CHIP_BASE} border-down/50 bg-down-soft text-down`
        : tone === 'warn'
          ? `${CHIP_BASE} border-warn/50 bg-warn-soft text-warn`
          : `${CHIP_BASE} border-brand/50 bg-brand-soft text-brand`
    : `${CHIP_BASE} border-line text-fg-3 hover:bg-surface-hover hover:text-fg`
const MODAL_OVERLAY =
  'fixed inset-0 z-50 flex items-center justify-center p-4 bg-[var(--overlay)]'

type ReviewTab = 'journal' | 'stats' | 'rules' | 'ai'

const EMOTION_OPTIONS = [
  'calm',
  'fomo',
  'fear_of_missing',
  'revenge',
  'overconfident',
  'hesitant',
]
const MISTAKE_OPTIONS = [
  'strategy',
  'execution',
  'risk_control',
  'market',
  'none',
]
const STRATEGY_OPTIONS = [
  'breakout',
  'mean_reversion',
  'trend_following',
  'momentum',
  'scalp',
  'news',
  'other',
]
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
  const [selectedTraderId, setSelectedTraderId] = useState<string | undefined>(
    () => {
      return (
        new URLSearchParams(window.location.search).get('trader') || undefined
      )
    }
  )
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
  }, [])

  const tabs: { key: ReviewTab; label: string }[] = [
    { key: 'journal', label: rv('tabJournal') },
    { key: 'stats', label: rv('tabStats') },
    { key: 'rules', label: rv('tabRules') },
    { key: 'ai', label: rv('tabAI') },
  ]

  return (
    <div className="min-h-screen bg-bg text-fg">
      <div className="max-w-[1440px] mx-auto px-4 sm:px-6 py-4">
        {/* Header */}
        <div className="flex items-center justify-between flex-wrap gap-3 mb-3">
          <div>
            <h1 className="text-xl font-semibold">{rv('title')}</h1>
            <p className="text-xs mt-0.5 text-fg-3">{rv('subtitle')}</p>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-xs text-fg-3">{rv('trader')}</span>
            <select
              value={selectedTraderId || ''}
              onChange={(e) => setSelectedTraderId(e.target.value)}
              className="h-8 rounded-md border border-line bg-surface-2 px-2 text-[13px] text-fg outline-none hover:border-line-strong focus:border-brand"
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
        <Tabs
          className="mb-4"
          items={tabs}
          value={tab}
          onChange={(k) => setTab(k)}
        />

        {!selectedTraderId ? (
          <EmptyState title={rv('noTraders')} className="py-16" />
        ) : (
          <>
            {tab === 'journal' && (
              <JournalTab traderId={selectedTraderId} language={language} />
            )}
            {tab === 'stats' && (
              <StatsTab traderId={selectedTraderId} language={language} />
            )}
            {tab === 'rules' && (
              <RulesTab traderId={selectedTraderId} language={language} />
            )}
            {tab === 'ai' && (
              <AITab traderId={selectedTraderId} language={language} />
            )}
          </>
        )}
      </div>
    </div>
  )
}

// ===================== Journal Tab =====================

function JournalTab({
  traderId,
  language,
}: {
  traderId: string
  language: Language
}) {
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

  const th = 'h-8 bg-surface-2 px-3 text-xs font-medium text-fg-3 sticky top-0'
  return (
    <div>
      {/* Toolbar */}
      <div className="flex items-center justify-between flex-wrap gap-2 mb-3">
        <div className="flex items-center gap-2">
          <select
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value)}
            className="h-8 rounded-md border border-line bg-surface-2 px-2 text-[13px] text-fg outline-none hover:border-line-strong focus:border-brand"
          >
            <option value="">{rv('filterAll')}</option>
            <option value="pending">{rv('filterPending')}</option>
            <option value="reviewed">{rv('filterReviewed')}</option>
          </select>
          <input
            value={symbolFilter}
            onChange={(e) => setSymbolFilter(e.target.value.toUpperCase())}
            placeholder={rv('filterSymbolPlaceholder')}
            className="h-8 w-32 rounded-md border border-line bg-surface-2 px-3 text-[13px] text-fg outline-none placeholder:text-fg-3 hover:border-line-strong focus:border-brand"
          />
        </div>
        <div className="flex items-center gap-3">
          <span className="num text-xs text-fg-3">
            {rv('totalCount', { count: total })}
          </span>
          <Button
            variant="primary"
            onClick={handleSync}
            disabled={syncing}
            loading={syncing}
          >
            {syncing ? rv('syncing') : rv('syncFromPositions')}
          </Button>
        </div>
      </div>

      {/* Table */}
      <Card className="overflow-x-auto">
        {loading ? (
          <div className="py-12 text-center text-sm text-fg-3">
            {t('loading', language)}
          </div>
        ) : entries.length === 0 ? (
          <EmptyState title={rv('emptyJournal')} className="py-12" />
        ) : (
          <table className="w-full border-collapse text-[13px] whitespace-nowrap">
            <thead>
              <tr className="border-b border-line">
                <th className={`${th} text-left`}>{rv('colSymbol')}</th>
                <th className={`${th} text-left`}>{rv('colSource')}</th>
                <th className={`${th} text-left`}>{rv('colDirection')}</th>
                <th className={`${th} text-right`}>{rv('colEntry')}</th>
                <th className={`${th} text-right`}>{rv('colExit')}</th>
                <th className={`${th} text-right`}>{rv('colLeverage')}</th>
                <th className={`${th} text-right`}>{rv('colPnL')}</th>
                <th className={`${th} text-right`}>{rv('colPnlPct')}</th>
                <th className={`${th} text-left`}>{rv('colTime')}</th>
                <th className={`${th} text-left`}>{rv('colPlan')}</th>
                <th className={`${th} text-left`}>{rv('colAdherence')}</th>
                <th className={`${th} text-left`}>{rv('colEmotion')}</th>
                <th className={`${th} text-left`}>{rv('colStatus')}</th>
                <th className={`${th} text-right`}></th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => (
                <JournalRow
                  key={e.id}
                  entry={e}
                  language={language}
                  onEdit={() => setEditing(e)}
                />
              ))}
            </tbody>
          </table>
        )}
      </Card>

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

  const adherenceLabel = () => {
    switch (entry.executed_as_plan) {
      case 'yes':
        return (
          <Badge variant="up" size="xs">
            {rv('adherenceYes')}
          </Badge>
        )
      case 'partial':
        return (
          <Badge variant="warn" size="xs">
            {rv('adherencePartial')}
          </Badge>
        )
      case 'no':
        return (
          <Badge variant="down" size="xs">
            {rv('adherenceNo')}
          </Badge>
        )
      default:
        return <span className="text-fg-3">--</span>
    }
  }

  const hasPlan = entry.planned_stop_loss > 0 || entry.planned_take_profit > 0
  const td = 'px-3 py-0'

  return (
    <tr className="h-[34px] border-b border-line last:border-b-0 hover:bg-surface-hover">
      <td className={`${td} font-semibold text-fg`}>{entry.symbol}</td>
      <td className={td}>
        {entry.ai_managed === null || entry.ai_managed === undefined ? (
          <span className="text-[11px] text-fg-3">{rv('sourceUnknown')}</span>
        ) : entry.ai_managed ? (
          <Badge variant="brand" size="xs" title={rv('sourceAITitle')}>
            {rv('sourceAI')}
          </Badge>
        ) : (
          <Badge variant="neutral" size="xs" title={rv('sourceManualTitle')}>
            {rv('sourceManual')}
          </Badge>
        )}
      </td>
      <td className={td}>
        <Badge variant={entry.side === 'LONG' ? 'up' : 'down'} size="xs">
          {entry.side === 'LONG' ? rv('long') : rv('short')}
        </Badge>
      </td>
      <td className={`${td} num text-right`}>{fmtPrice(entry.entry_price)}</td>
      <td className={`${td} num text-right`}>{fmtPrice(entry.exit_price)}</td>
      <td className={`${td} num text-right`}>{entry.leverage}x</td>
      <td className={`${td} text-right font-semibold`}>
        <PnL value={entry.realized_pnl} currency="" />
      </td>
      <td className={`${td} text-right`}>
        <Change value={entry.pnl_pct} decimals={1} />
      </td>
      <td className={`${td} num text-fg-3`}>
        {fmtTime(entry.entry_time)} → {fmtTime(entry.exit_time)}
      </td>
      <td className={`${td} num ${hasPlan ? 'text-fg-2' : 'text-fg-3'}`}>
        {hasPlan
          ? `SL ${fmtPrice(entry.planned_stop_loss)} / TP ${fmtPrice(entry.planned_take_profit)}`
          : rv('noPlan')}
      </td>
      <td className={td}>{adherenceLabel()}</td>
      <td className={`${td} ${entry.emotions ? 'text-fg' : 'text-fg-3'}`}>
        {entry.emotions || '--'}
      </td>
      <td className={td}>
        {entry.review_status === 'reviewed' ? (
          <Badge variant="up" size="xs">
            {rv('reviewed')}
          </Badge>
        ) : (
          <Badge variant="brand" size="xs">
            {rv('pendingReview')}
          </Badge>
        )}
      </td>
      <td className={`${td} text-right`}>
        <Button
          size="sm"
          variant="secondary"
          onClick={onEdit}
          className="text-brand"
        >
          {rv('reviewAction')}
        </Button>
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
  const [executedAsPlan, setExecutedAsPlan] = useState(
    entry.executed_as_plan || ''
  )
  const [emotions, setEmotions] = useState<string[]>(
    entry.emotions ? entry.emotions.split(',').filter(Boolean) : []
  )
  const [mistakeCategory, setMistakeCategory] = useState(
    entry.mistake_category || ''
  )
  const [strategyTag, setStrategyTag] = useState(entry.strategy_tag || '')
  const [deviationNote, setDeviationNote] = useState(entry.deviation_note || '')
  const [lesson, setLesson] = useState(entry.lesson || '')
  const [saving, setSaving] = useState(false)

  const toggleEmotion = (emo: string) => {
    setEmotions((prev) =>
      prev.includes(emo) ? prev.filter((x) => x !== emo) : [...prev, emo]
    )
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
    <div className={MODAL_OVERLAY} onClick={onClose}>
      <div
        className="w-full max-w-2xl max-h-[85vh] overflow-y-auto rounded-lg border border-line bg-surface p-5 shadow-[var(--shadow)]"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header: trade facts */}
        <div className="flex items-start justify-between mb-4">
          <div>
            <h3 className="flex items-center gap-2 text-base font-semibold">
              {entry.symbol}
              <Badge variant={entry.side === 'LONG' ? 'up' : 'down'}>
                {entry.side === 'LONG' ? rv('long') : rv('short')}{' '}
                <span className="num">{entry.leverage}x</span>
              </Badge>
            </h3>
            <p className="text-xs mt-1 text-fg-3">
              {fmtTime(entry.entry_time)} → {fmtTime(entry.exit_time)}
              {entry.close_reason ? ` · ${entry.close_reason}` : ''}
            </p>
          </div>
          <Button
            variant="ghost"
            size="sm"
            onClick={onClose}
            aria-label="close"
          >
            ✕
          </Button>
        </div>

        {/* Facts grid */}
        <div className="grid grid-cols-2 md:grid-cols-4 gap-2 mb-4">
          <FactBox label={rv('colEntry')} value={fmtPrice(entry.entry_price)} />
          <FactBox label={rv('colExit')} value={fmtPrice(entry.exit_price)} />
          <FactBox
            label={rv('colPnL')}
            value={`${entry.realized_pnl >= 0 ? '+' : ''}${entry.realized_pnl.toFixed(2)}`}
            color={
              win
                ? 'text-up'
                : entry.realized_pnl < 0
                  ? 'text-down'
                  : 'text-fg-3'
            }
          />
          <FactBox
            label={rv('colPnlPct')}
            value={`${entry.pnl_pct >= 0 ? '+' : ''}${entry.pnl_pct.toFixed(1)}%`}
            color={
              win
                ? 'text-up'
                : entry.realized_pnl < 0
                  ? 'text-down'
                  : 'text-fg-3'
            }
          />
          <FactBox
            label={rv('plannedSL')}
            value={
              entry.planned_stop_loss > 0
                ? fmtPrice(entry.planned_stop_loss)
                : rv('noPlan')
            }
            color={entry.planned_stop_loss > 0 ? undefined : 'text-fg-3'}
          />
          <FactBox
            label={rv('plannedTP')}
            value={
              entry.planned_take_profit > 0
                ? fmtPrice(entry.planned_take_profit)
                : rv('noPlan')
            }
            color={entry.planned_take_profit > 0 ? undefined : 'text-fg-3'}
          />
          <FactBox
            label={rv('colConfidence')}
            value={entry.confidence > 0 ? `${entry.confidence}%` : '--'}
          />
          <FactBox label={rv('colQuantity')} value={String(entry.quantity)} />
        </div>

        {/* Entry reasoning */}
        {entry.entry_reasoning && (
          <div className="mb-4 rounded-md bg-surface-2 p-3 text-xs leading-relaxed text-fg-2">
            <div className="font-semibold mb-1 text-fg">
              {rv('entryReasoning')}
            </div>
            {entry.entry_reasoning}
          </div>
        )}

        {/* Review form */}
        <div className="space-y-4">
          <div>
            <label className={LABEL_CLS}>{rv('executedAsPlan')}</label>
            <div className="flex gap-2 flex-wrap">
              {[
                { v: 'yes', label: rv('adherenceYes') },
                { v: 'partial', label: rv('adherencePartial') },
                { v: 'no', label: rv('adherenceNo') },
              ].map((opt) => (
                <button
                  key={opt.v}
                  type="button"
                  onClick={() => setExecutedAsPlan(opt.v)}
                  className={chipCls(executedAsPlan === opt.v)}
                >
                  {opt.label}
                </button>
              ))}
            </div>
          </div>

          <div>
            <label className={LABEL_CLS}>{rv('emotionState')}</label>
            <div className="flex gap-2 flex-wrap">
              {EMOTION_OPTIONS.map((emo) => (
                <button
                  key={emo}
                  type="button"
                  onClick={() => toggleEmotion(emo)}
                  className={chipCls(emotions.includes(emo), 'info')}
                >
                  {rv(`emotion_${emo}`)}
                </button>
              ))}
            </div>
          </div>

          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            <div>
              <label className={LABEL_CLS}>{rv('mistakeCategory')}</label>
              <select
                value={mistakeCategory}
                onChange={(e) => setMistakeCategory(e.target.value)}
                className={INPUT_CLS}
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
              <label className={LABEL_CLS}>{rv('strategyTag')}</label>
              <select
                value={strategyTag}
                onChange={(e) => setStrategyTag(e.target.value)}
                className={INPUT_CLS}
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
            <label className={LABEL_CLS}>{rv('deviationNote')}</label>
            <textarea
              value={deviationNote}
              onChange={(e) => setDeviationNote(e.target.value)}
              rows={2}
              placeholder={rv('deviationNotePlaceholder')}
              className={TEXTAREA_CLS}
            />
          </div>

          <div>
            <label className={LABEL_CLS}>{rv('lesson')}</label>
            <textarea
              value={lesson}
              onChange={(e) => setLesson(e.target.value)}
              rows={3}
              placeholder={rv('lessonPlaceholder')}
              className={TEXTAREA_CLS}
            />
          </div>
        </div>

        <div className="mt-5 flex justify-end gap-2">
          <Button onClick={onClose}>{t('cancel', language)}</Button>
          <Button variant="primary" onClick={save} disabled={saving}>
            {saving ? t('loading', language) : rv('saveReview')}
          </Button>
        </div>
      </div>
    </div>
  )
}

function FactBox({
  label,
  value,
  color,
}: {
  label: string
  value: string
  color?: string
}) {
  return (
    <div className="rounded-md bg-surface-2 p-2">
      <div className="mb-0.5 text-[11px] text-fg-3">{label}</div>
      <div className={`num text-[13px] font-semibold ${color || 'text-fg'}`}>
        {value}
      </div>
    </div>
  )
}

// ===================== Stats Tab =====================

function StatsTab({
  traderId,
  language,
}: {
  traderId: string
  language: Language
}) {
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
      <div className="py-12 text-center text-sm text-fg-3">
        {t('loading', language)}
      </div>
    )
  }
  if (!stats || stats.total_entries === 0) {
    return <EmptyState title={rv('emptyStats')} className="py-12" />
  }

  const plRatio = stats.avg_loss > 0 ? stats.avg_win / stats.avg_loss : 0

  return (
    <div className="space-y-3">
      {/* Core metrics */}
      <Card className="grid grid-cols-2 gap-4 p-4 md:grid-cols-3 lg:grid-cols-6">
        <MetricCard
          label={rv('totalTrades')}
          value={String(stats.total_entries)}
          sub={`${rv('reviewedCount')} ${stats.reviewed_count}`}
        />
        <MetricCard
          label={rv('winRate')}
          value={`${stats.win_rate.toFixed(1)}%`}
          sub={`${stats.win_trades}W / ${stats.loss_trades}L`}
          color={stats.win_rate >= 50 ? 'up' : 'down'}
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
          color={stats.expectancy >= 0 ? 'up' : 'down'}
        />
        <MetricCard
          label={rv('profitFactor')}
          value={
            stats.profit_factor > 0 ? stats.profit_factor.toFixed(2) : '--'
          }
          color={stats.profit_factor >= 1 ? 'up' : 'down'}
        />
        <MetricCard
          label={rv('totalPnL')}
          value={`${stats.total_pnl >= 0 ? '+' : ''}${stats.total_pnl.toFixed(2)}`}
          color={stats.total_pnl >= 0 ? 'up' : 'down'}
        />
      </Card>

      {/* Execution layer: adherence + plan coverage */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
        <div className="rounded-lg border border-line bg-surface p-4">
          <h3 className="text-sm font-semibold mb-2">
            {rv('adherenceSection')}
          </h3>
          <GroupStatsTable
            groups={stats.adherence_plan}
            language={language}
            labelMap={(k) => rv(`adherence_${k}`)}
          />
          <div className="mt-3 flex items-center justify-between border-t border-line pt-3 text-xs text-fg-3">
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

        <div className="rounded-lg border border-line bg-surface p-4">
          <h3 className="text-sm font-semibold mb-2">{rv('emotionSection')}</h3>
          <GroupStatsTable
            groups={stats.emotion_stats}
            language={language}
            labelMap={(k) => rv(`emotion_${k}`)}
          />
        </div>

        <div className="rounded-lg border border-line bg-surface p-4">
          <h3 className="text-sm font-semibold mb-2">{rv('mistakeSection')}</h3>
          <GroupStatsTable
            groups={stats.mistake_stats}
            language={language}
            labelMap={(k) => rv(`mistake_${k}`)}
          />
        </div>

        <div className="rounded-lg border border-line bg-surface p-4">
          <h3 className="text-sm font-semibold mb-2">
            {rv('strategySection')}
          </h3>
          <GroupStatsTable
            groups={stats.strategy_stats}
            language={language}
            labelMap={(k) => rv(`strategy_${k}`)}
          />
        </div>
      </div>
    </div>
  )
}

function MetricCard({
  label,
  value,
  sub,
  color,
}: {
  label: string
  value: string
  sub?: string
  color?: 'up' | 'down'
}) {
  return <Stat label={label} value={value} hint={sub} tone={color} />
}

function GroupStatsTable({
  groups,
  language,
  labelMap,
}: {
  groups: {
    key: string
    count: number
    win_rate: number
    total_pnl: number
    avg_pnl: number
  }[]
  language: Language
  labelMap: (key: string) => string
}) {
  const rv = (key: string) => t(`reviewPage.${key}`, language)
  if (!groups || groups.length === 0) {
    return (
      <div className="py-6 text-center text-xs text-fg-3">{rv('noData')}</div>
    )
  }
  const th = 'h-8 bg-surface-2 px-3 text-xs font-medium text-fg-3'
  return (
    <div className="-mx-4 -mb-4 overflow-x-auto">
      <table className="w-full border-collapse text-[13px]">
        <thead>
          <tr className="border-y border-line">
            <th className={`${th} text-left`}>{rv('groupCol')}</th>
            <th className={`${th} text-right`}>{rv('countCol')}</th>
            <th className={`${th} text-right`}>{rv('winRateCol')}</th>
            <th className={`${th} text-right`}>{rv('totalPnlCol')}</th>
            <th className={`${th} text-right`}>{rv('avgPnlCol')}</th>
          </tr>
        </thead>
        <tbody>
          {groups.map((g) => (
            <tr
              key={g.key}
              className="h-[34px] border-b border-line last:border-b-0 hover:bg-surface-hover"
            >
              <td className="px-3 py-0">{labelMap(g.key)}</td>
              <td className="num px-3 py-0 text-right">{g.count}</td>
              <td
                className={`num px-3 py-0 text-right ${g.win_rate >= 50 ? 'text-up' : 'text-down'}`}
              >
                {g.win_rate.toFixed(0)}%
              </td>
              <td className="px-3 py-0 text-right">
                <PnL value={g.total_pnl} decimals={1} currency="" />
              </td>
              <td className="px-3 py-0 text-right">
                <PnL value={g.avg_pnl} decimals={1} currency="" />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// ===================== Rules Tab =====================

function RulesTab({
  traderId,
  language,
}: {
  traderId: string
  language: Language
}) {
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
      const [ruleRes, logRes] = await Promise.all([
        api.getRules(traderId),
        api.getRuleLogs(traderId, 30),
      ])
      setRules(ruleRes.rules || [])
      setLogs(logRes.logs || [])
    } catch {
      setRules([])
      setLogs([])
    } finally {
      setLoading(false)
    }
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

  const reverifyRule = async (rule: TradingRule) => {
    try {
      await api.reverifyRule(rule.id, traderId)
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
        proposals
          .filter((p) => !proposalBlocked(p))
          .map((p) => ({
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
      const skipped = res.rejected?.length || 0
      setApplyResult(
        rv('rulesApplied', { count: res.saved }) +
          (skipped
            ? rv('rulesSkipped', { count: skipped }) +
              ': ' +
              res.rejected!.map((r) => r.reason).join('; ')
            : '')
      )
      setProposals(null)
      load()
    } catch (err) {
      setApplyResult(err instanceof Error ? err.message : String(err))
    }
  }

  const hardRules = rules.filter((r) => r.rule_type === 'hard')
  const softRules = rules.filter((r) => r.rule_type === 'soft')

  return (
    <div className="space-y-3">
      {/* Toolbar */}
      <div className="flex items-center justify-between flex-wrap gap-3">
        <p className="text-xs text-fg-3">{rv('rulesIntro')}</p>
        <div className="flex gap-2">
          <Button
            onClick={handleExtract}
            disabled={extracting}
            loading={extracting}
            className="border-ai/40 bg-ai-soft text-ai hover:bg-ai-soft hover:border-ai"
          >
            {extracting ? rv('extracting') : rv('aiExtractRules')}
          </Button>
          <Button variant="primary" onClick={() => setShowCreate(true)}>
            {rv('addRule')}
          </Button>
        </div>
      </div>

      {applyResult && (
        <div className="rounded-lg border border-line bg-surface px-3 py-2 text-xs text-fg-3">
          {applyResult}
        </div>
      )}

      {/* AI proposals */}
      {proposals && (
        <div className="rounded-lg border border-line bg-surface p-4">
          <div className="flex items-center justify-between mb-3">
            <h3 className="text-sm font-bold">{rv('aiProposals')}</h3>
            <div className="flex gap-2">
              {proposals.length > 0 && (
                <Button
                  variant="primary"
                  size="sm"
                  onClick={handleApplyProposals}
                >
                  {rv('applyAll')}
                </Button>
              )}
              <Button size="sm" onClick={() => setProposals(null)}>
                ✕
              </Button>
            </div>
          </div>
          {proposals.length === 0 ? (
            <p className="text-xs py-4 text-center text-fg-3">
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
      <div className="rounded-lg border border-line bg-surface p-4">
        <h3 className="text-sm font-semibold mb-2">
          {rv('hardRules')}{' '}
          <span className="text-xs font-normal text-fg-3">
            ({hardRules.length})
          </span>
        </h3>
        {loading ? (
          <div className="py-6 text-center text-xs text-fg-3">
            {t('loading', language)}
          </div>
        ) : hardRules.length === 0 ? (
          <p className="py-6 text-center text-xs text-fg-3">
            {rv('noHardRules')}
          </p>
        ) : (
          <div className="space-y-2">
            {hardRules.map((r) => (
              <RuleRow
                key={r.id}
                rule={r}
                language={language}
                onToggle={() => toggleEnabled(r)}
                onDelete={() => deleteRule(r)}
                onReverify={() => reverifyRule(r)}
              />
            ))}
          </div>
        )}
      </div>

      {/* Soft lessons */}
      <div className="rounded-lg border border-line bg-surface p-4">
        <h3 className="text-sm font-semibold mb-2">
          {rv('softRules')}{' '}
          <span className="text-xs font-normal text-fg-3">
            ({softRules.length})
          </span>
        </h3>
        {!loading && softRules.length === 0 ? (
          <p className="py-6 text-center text-xs text-fg-3">
            {rv('noSoftRules')}
          </p>
        ) : (
          <div className="space-y-2">
            {softRules.map((r) => (
              <RuleRow
                key={r.id}
                rule={r}
                language={language}
                onToggle={() => toggleEnabled(r)}
                onDelete={() => deleteRule(r)}
              />
            ))}
          </div>
        )}
      </div>

      {/* Check logs */}
      <div className="rounded-lg border border-line bg-surface p-4">
        <h3 className="text-sm font-semibold mb-2">{rv('checkLogs')}</h3>
        {logs.length === 0 ? (
          <p className="py-6 text-center text-xs text-fg-3">
            {rv('noCheckLogs')}
          </p>
        ) : (
          <div className="-mx-4 -mb-4 overflow-x-auto">
            <table className="w-full border-collapse text-[13px]">
              <thead>
                <tr className="border-y border-line">
                  {[
                    rv('colTime'),
                    rv('colSymbol'),
                    rv('ruleName'),
                    rv('checkResult'),
                  ].map((h) => (
                    <th
                      key={h}
                      className="h-8 bg-surface-2 px-3 text-left text-xs font-medium text-fg-3"
                    >
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {logs.map((log) => (
                  <tr
                    key={log.id}
                    className="h-[34px] border-b border-line last:border-b-0 hover:bg-surface-hover"
                  >
                    <td className="num px-3 py-0 text-fg-3">
                      {fmtTime(log.created_at)}
                    </td>
                    <td className="px-3 py-0 font-semibold">
                      {log.symbol}{' '}
                      <span className="font-normal text-fg-3">
                        {log.action}
                      </span>
                    </td>
                    <td className="px-3 py-0">{log.rule_name}</td>
                    <td className="px-3 py-0">
                      {log.blocked ? (
                        <Badge variant="down" size="xs">
                          {rv('resultBlocked')}
                        </Badge>
                      ) : (
                        <Badge variant="warn" size="xs">
                          {rv('resultWarned')}
                        </Badge>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
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
  onReverify,
}: {
  rule: TradingRule
  language: Language
  onToggle: () => void
  onDelete: () => void
  onReverify?: () => void
}) {
  const rv = (key: string, params?: Record<string, string | number>) =>
    t(`reviewPage.${key}`, language, params)
  return (
    <div className="flex items-start justify-between gap-3 rounded-md border border-line bg-surface-2 px-3 py-2">
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[13px] font-semibold">{rule.name}</span>
          <Badge
            variant={rule.rule_type === 'hard' ? 'info' : 'brand'}
            size="xs"
          >
            {rule.rule_type === 'hard' ? rv('typeHard') : rv('typeSoft')}
          </Badge>
          {rule.on_violation === 'block' ? (
            <Badge variant="down" size="xs">
              {rv('violationBlock')}
            </Badge>
          ) : (
            <Badge variant="warn" size="xs">
              {rv('violationWarn')}
            </Badge>
          )}
          {rule.source === 'ai_review' && (
            <Badge variant="ai" size="xs">
              AI
            </Badge>
          )}
          <span className="num text-[11px] text-fg-3">
            {rv('ruleHits', { hits: rule.hit_count, blocks: rule.block_count })}
          </span>
          {rule.triggers_30d !== undefined && (
            <span className="num text-[11px] text-fg-3">
              {rv('triggers30d', { n: rule.triggers_30d })}
            </span>
          )}
          {rule.review_due && (
            <Badge variant="warn" size="xs">
              {rv('reviewDue')}
            </Badge>
          )}
        </div>
        {rule.rule_type === 'hard' && (
          <div className="num mt-1 text-[11px] text-info">
            {rule.condition_json}
          </div>
        )}
        <div className="mt-1 text-xs text-fg-2">
          {rule.rule_type === 'hard' ? rule.description : rule.lesson_text}
        </div>
      </div>
      <div className="flex items-center gap-2 shrink-0">
        {onReverify &&
          rule.rule_type === 'hard' &&
          rule.source === 'ai_review' && (
            <Button size="sm" onClick={onReverify}>
              {rv('reverify')}
            </Button>
          )}
        <button
          type="button"
          onClick={onToggle}
          className={`relative h-5 w-9 rounded-full transition-colors ${rule.enabled ? 'bg-up' : 'bg-line-strong'}`}
          title={rule.enabled ? rv('enabled') : rv('disabled')}
        >
          <span
            className={`absolute top-0.5 h-4 w-4 rounded-full bg-white transition-all ${rule.enabled ? 'left-[18px]' : 'left-0.5'}`}
          />
        </button>
        <Button
          variant="ghost"
          size="sm"
          onClick={onDelete}
          aria-label="delete"
        >
          <Trash2 className="h-3.5 w-3.5" />
        </Button>
      </div>
    </div>
  )
}

// review 2026-10-09 K: hard proposals the server cannot back with history
// are not applicable (the apply endpoint re-checks and rejects them too).
function proposalBlocked(p: RuleProposal): boolean {
  const st = p.verification?.status
  return p.rule_type === 'hard' && (st === 'weak' || st === 'contradicted')
}

function VerificationBadge({
  v,
  language,
}: {
  v: RuleVerification
  language: Language
}) {
  const rv = (key: string, params?: Record<string, string | number>) =>
    t(`reviewPage.${key}`, language, params)
  const label: Record<string, string> = {
    supported: rv('verifSupported'),
    weak: rv('verifWeak'),
    contradicted: rv('verifContradicted'),
    unverifiable: rv('verifUnverifiable'),
    soft: rv('verifSoft'),
  }
  const variant =
    v.status === 'supported'
      ? 'up'
      : v.status === 'weak'
        ? 'warn'
        : v.status === 'contradicted'
          ? 'down'
          : v.status === 'soft'
            ? 'info'
            : 'neutral'
  const detail = rv('verifDetail', {
    matched: v.matched,
    pop: v.population,
    wins: v.wins,
    net: v.net_pnl.toFixed(2),
  })
  return (
    <Badge variant={variant} size="xs" title={v.reason || detail}>
      {label[v.status] || v.status}
    </Badge>
  )
}

function ProposalRow({
  proposal,
  language,
}: {
  proposal: RuleProposal
  language: Language
}) {
  const rv = (key: string, params?: Record<string, string | number>) =>
    t(`reviewPage.${key}`, language, params)
  return (
    <div className="rounded-md border border-line bg-surface-2 px-3 py-2 text-xs">
      <div className="flex items-center gap-2 flex-wrap">
        <Badge
          variant={proposal.rule_type === 'hard' ? 'info' : 'brand'}
          size="xs"
        >
          {proposal.rule_type === 'hard' ? rv('typeHard') : rv('typeSoft')}
        </Badge>
        <span className="font-semibold">{proposal.name}</span>
        {proposal.verification && (
          <VerificationBadge v={proposal.verification} language={language} />
        )}
      </div>
      {proposal.verification && proposal.verification.status !== 'soft' && (
        <div className="num mt-1 text-[11px] text-fg-3">
          {proposal.verification.reason ||
            rv('verifDetail', {
              matched: proposal.verification.matched,
              pop: proposal.verification.population,
              wins: proposal.verification.wins,
              net: proposal.verification.net_pnl.toFixed(2),
            })}
        </div>
      )}
      {proposalBlocked(proposal) && (
        <div className="text-[10px] mt-1 text-down">{rv('verifBlocked')}</div>
      )}
      {proposal.rule_type === 'hard' && proposal.condition && (
        <div className="num mt-1 text-[11px] text-info">
          {proposal.condition}
        </div>
      )}
      <div className="mt-1 text-xs text-fg-2">
        {proposal.rule_type === 'hard'
          ? proposal.description
          : proposal.lesson_text}
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

  return (
    <div className={MODAL_OVERLAY} onClick={onClose}>
      <div
        className="w-full max-w-lg rounded-lg border border-line bg-surface p-5 shadow-[var(--shadow)]"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between mb-4">
          <h3 className="text-base font-bold">{rv('addRule')}</h3>
          <Button
            variant="ghost"
            size="sm"
            onClick={onClose}
            aria-label="close"
          >
            ✕
          </Button>
        </div>

        <div className="space-y-4">
          <div className="flex gap-2">
            {(['hard', 'soft'] as const).map((tp) => (
              <button
                key={tp}
                type="button"
                onClick={() => setRuleType(tp)}
                className={`${chipCls(ruleType === tp)} h-8 flex-1 justify-center`}
              >
                {tp === 'hard' ? rv('typeHard') : rv('typeSoft')}
              </button>
            ))}
          </div>

          <div>
            <label className={LABEL_CLS}>{rv('ruleName')}</label>
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              className={INPUT_CLS}
            />
          </div>

          {ruleType === 'hard' ? (
            <>
              <div>
                <label className={LABEL_CLS}>{rv('ruleCondition')}</label>
                <div className="flex gap-2">
                  <select
                    value={field}
                    onChange={(e) => setField(e.target.value)}
                    className={`${INPUT_CLS} flex-1`}
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
                    className={`${INPUT_CLS} !w-20 shrink-0`}
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
                    className={`${INPUT_CLS} flex-1`}
                  />
                </div>
                <div className="num text-[11px] mt-1.5 text-fg-3">
                  {conditionJSON}
                </div>
              </div>
              <div>
                <label className={LABEL_CLS}>{rv('violationAction')}</label>
                <div className="flex gap-2">
                  {(['block', 'warn'] as const).map((act) => (
                    <button
                      key={act}
                      type="button"
                      onClick={() => setOnViolation(act)}
                      className={`${chipCls(onViolation === act, act === 'block' ? 'down' : 'warn')} h-8 flex-1 justify-center`}
                    >
                      {act === 'block'
                        ? rv('violationBlock')
                        : rv('violationWarn')}
                    </button>
                  ))}
                </div>
              </div>
              <div>
                <label className={LABEL_CLS}>{rv('ruleDescription')}</label>
                <input
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                  className={INPUT_CLS}
                />
              </div>
            </>
          ) : (
            <>
              <div>
                <label className={LABEL_CLS}>{rv('lessonText')}</label>
                <textarea
                  value={lessonText}
                  onChange={(e) => setLessonText(e.target.value)}
                  rows={3}
                  className={TEXTAREA_CLS}
                />
              </div>
              <div>
                <label className={LABEL_CLS}>{rv('ruleTags')}</label>
                <input
                  value={tags}
                  onChange={(e) => setTags(e.target.value)}
                  placeholder="fomo, high_leverage"
                  className={INPUT_CLS}
                />
              </div>
            </>
          )}

          {error && <p className="text-xs text-down">{error}</p>}
        </div>

        <div className="mt-5 flex justify-end gap-2">
          <Button onClick={onClose}>{t('cancel', language)}</Button>
          <Button variant="primary" onClick={save} disabled={saving}>
            {saving ? t('loading', language) : rv('saveRule')}
          </Button>
        </div>
      </div>
    </div>
  )
}

// ===================== AI Review Tab =====================

function AITab({
  traderId,
  language,
}: {
  traderId: string
  language: Language
}) {
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
    <div className="space-y-3">
      <div className="rounded-lg border border-line bg-surface p-4">
        <p className="mb-3 max-w-[80ch] text-xs text-fg-3">
          {rv('aiReviewIntro')}
        </p>
        <div className="flex items-center gap-3 flex-wrap">
          <Segmented
            items={(['daily', 'weekly', 'monthly'] as const).map((p) => ({
              key: p,
              label: rv(`period_${p}`),
            }))}
            value={period}
            onChange={(k) => setPeriod(k)}
          />
          <Button
            variant="primary"
            onClick={run}
            disabled={running}
            loading={running}
          >
            {running ? rv('aiRunning') : rv('aiRunReview')}
          </Button>
          <Button onClick={() => setShowPrompts((v) => !v)}>
            {rv('promptSettings')}
            {promptCfg &&
              (promptCfg.review_custom || promptCfg.rule_extract_custom) && (
                <Badge variant="brand" size="xs">
                  {rv('promptCustomActive')}
                </Badge>
              )}
          </Button>
        </div>
        {error && <p className="text-xs mt-3 text-down">{error}</p>}

        {showPrompts && (
          <div className="mt-4 space-y-3 border-t border-line pt-4">
            <div>
              <div className="flex items-center justify-between mb-1">
                <label className="text-xs font-medium text-fg">
                  {rv('promptReview')}
                </label>
                {promptCfg?.review_custom && (
                  <span className="text-[10px] text-brand">
                    {rv('promptCustomActive')}
                  </span>
                )}
              </div>
              <textarea
                value={reviewPrompt}
                onChange={(e) => setReviewPrompt(e.target.value)}
                rows={10}
                className={`${TEXTAREA_CLS} num resize-y`}
              />
            </div>
            <div>
              <div className="flex items-center justify-between mb-1">
                <label className="text-xs font-medium text-fg">
                  {rv('promptRuleExtract')}
                </label>
                {promptCfg?.rule_extract_custom && (
                  <span className="text-[10px] text-brand">
                    {rv('promptCustomActive')}
                  </span>
                )}
              </div>
              <textarea
                value={rulePrompt}
                onChange={(e) => setRulePrompt(e.target.value)}
                rows={8}
                className={`${TEXTAREA_CLS} num resize-y`}
              />
            </div>
            <p className="text-[10px] text-fg-3">{rv('promptResetHint')}</p>
            <div className="flex items-center gap-2">
              <Button
                variant="primary"
                onClick={savePrompts}
                disabled={savingPrompts}
              >
                {savingPrompts ? t('loading', language) : rv('promptSave')}
              </Button>
              <Button
                onClick={() => {
                  setReviewPrompt('')
                  setRulePrompt('')
                }}
              >
                {rv('promptReset')}
              </Button>
              {promptMsg && (
                <span className="text-xs text-up">{promptMsg}</span>
              )}
            </div>
            {!promptCfg?.review_custom && !promptCfg?.rule_extract_custom && (
              <p className="text-[10px] text-fg-3">
                {rv('promptUsingDefault')}
              </p>
            )}
          </div>
        )}
      </div>

      {result && (
        <Card>
          <CardHeader
            title={rv('aiReviewResult')}
            actions={
              <>
                <Badge variant="ai" size="xs">
                  AI
                </Badge>
                <span className="num text-xs text-fg-3">
                  {fmtTime(result.generated_at)}
                </span>
              </>
            }
          />
          <CardBody>
            <pre className="max-w-[80ch] whitespace-pre-wrap font-sans text-[13px] leading-relaxed text-fg-2">
              {result.ai_response}
            </pre>
          </CardBody>
        </Card>
      )}
    </div>
  )
}

// Expose violations renderer for possible reuse (kept minimal)
export type { RuleViolation }
