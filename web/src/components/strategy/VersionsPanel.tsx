import { useCallback, useEffect, useState } from 'react'
import { GitCommitVertical, RefreshCw, Loader2, History } from 'lucide-react'
import type { Language } from '../../i18n/translations'
import { t } from '../../i18n/translations'
import type {
  StrategyConfigDiffEntry,
  StrategyVersionsResponse,
} from '../../types'

const API_BASE = import.meta.env.VITE_API_BASE || ''

interface VersionsPanelProps {
  strategyId: string
  token: string | null
  language: Language
  // Bump to refetch (e.g. after a successful save — updated_at advances).
  refreshKey?: string
}

const tv = (key: string, language: Language) =>
  t(`strategyVersions.${key}`, language)

// Diff values arrive JSON-encoded; scalars render bare, containers compact.
function renderDiffValue(encoded: string): string {
  if (encoded === '') return '∅'
  try {
    const v = JSON.parse(encoded)
    if (typeof v === 'string') return v
    if (typeof v === 'number' || typeof v === 'boolean') return String(v)
    return JSON.stringify(v)
  } catch {
    return encoded
  }
}

const SOURCE_STYLE: Record<string, { labelKey: string; className: string }> = {
  create: {
    labelKey: 'sourceCreate',
    className: 'bg-nofx-gold/15 text-nofx-gold',
  },
  duplicate: {
    labelKey: 'sourceDuplicate',
    className: 'bg-blue-400/15 text-blue-400',
  },
  baseline: {
    labelKey: 'sourceBaseline',
    className: 'bg-nofx-text-muted/15 text-nofx-text-muted',
  },
  save: {
    labelKey: 'sourceSave',
    className: 'bg-nofx-success/15 text-nofx-success',
  },
  external: {
    labelKey: 'sourceExternal',
    className: 'bg-nofx-danger/15 text-nofx-danger',
  },
}

function fmtTime(ms: number, language: Language): string {
  return new Date(ms).toLocaleString(language === 'zh' ? 'zh-CN' : 'en-US', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

function fmtSigned(v: number, digits = 2): string {
  const s = v.toFixed(digits)
  return v > 0 ? `+${s}` : s
}

export function VersionsPanel({
  strategyId,
  token,
  language,
  refreshKey,
}: VersionsPanelProps) {
  const [data, setData] = useState<StrategyVersionsResponse | null>(null)
  const [isLoading, setIsLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [expandedDiff, setExpandedDiff] = useState<number | null>(null)

  const fetchVersions = useCallback(async () => {
    if (!token || !strategyId) return
    setIsLoading(true)
    setError(null)
    try {
      const response = await fetch(
        `${API_BASE}/api/strategies/${strategyId}/versions`,
        { headers: { Authorization: `Bearer ${token}` } }
      )
      if (!response.ok) throw new Error(`HTTP ${response.status}`)
      setData(await response.json())
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error')
    } finally {
      setIsLoading(false)
    }
  }, [token, strategyId])

  useEffect(() => {
    setData(null)
    setExpandedDiff(null)
    fetchVersions()
  }, [fetchVersions, refreshKey])

  if (isLoading && !data) {
    return (
      <div className="flex flex-col items-center justify-center py-12 text-nofx-text-muted">
        <Loader2 className="w-6 h-6 mb-2 animate-spin" />
        <p className="text-sm">{tv('loading', language)}</p>
      </div>
    )
  }

  if (error) {
    return (
      <div className="p-3 space-y-3">
        <div className="p-3 rounded-lg bg-nofx-danger/10 border border-nofx-danger/30 text-sm text-nofx-danger">
          {tv('loadFailed', language)}: {error}
        </div>
        <button
          onClick={fetchVersions}
          className="flex items-center gap-1.5 px-3 py-1.5 rounded text-xs bg-nofx-gold text-black hover:bg-yellow-500"
        >
          <RefreshCw className="w-3 h-3" />
          {tv('refresh', language)}
        </button>
      </div>
    )
  }

  if (!data || data.versions.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-12 text-nofx-text-muted">
        <History className="w-10 h-10 mb-2 opacity-30" />
        <p className="text-sm">{tv('empty', language)}</p>
        <p className="text-xs mt-1 opacity-70">{tv('emptyHint', language)}</p>
      </div>
    )
  }

  // Newest first for display; version numbers count from the oldest (v1).
  const ordered = [...data.versions].reverse()
  // The version currently live in the DB = newest row whose hash matches.
  const currentId = ordered.find((v) => v.config_hash === data.current_hash)?.id

  return (
    <div className="p-3 space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-xs text-nofx-text-muted">
          {tv('subtitle', language)}
        </p>
        <button
          onClick={fetchVersions}
          disabled={isLoading}
          className="flex items-center gap-1 px-2 py-1 rounded text-xs text-nofx-text-muted hover:text-nofx-gold disabled:opacity-50"
        >
          {isLoading ? (
            <Loader2 className="w-3 h-3 animate-spin" />
          ) : (
            <RefreshCw className="w-3 h-3" />
          )}
          {tv('refresh', language)}
        </button>
      </div>

      <div className="relative">
        {ordered.map((v, idx) => {
          const versionNo = ordered.length - idx
          const source = SOURCE_STYLE[v.source] ?? SOURCE_STYLE.baseline
          const stats = v.stats
          const isCurrent = v.id === currentId
          const diffOpen = expandedDiff === v.id
          const shownDiff = diffOpen ? v.summary : v.summary.slice(0, 6)
          return (
            <div key={v.id} className="relative pl-5 pb-3">
              {/* timeline rail */}
              <div
                className="absolute left-[5px] top-2 bottom-0 w-px bg-nofx-line"
                aria-hidden
              />
              <div
                className="absolute left-0 top-1.5 w-[11px] h-[11px] rounded-full border-2"
                style={{
                  borderColor: isCurrent ? '#B8912A' : 'rgba(184,145,42,0.35)',
                  background: isCurrent ? '#B8912A' : 'transparent',
                }}
                aria-hidden
              />
              <div className="rounded-lg bg-nofx-bg-lighter border border-nofx-gold/20 overflow-hidden">
                {/* header */}
                <div className="flex items-center justify-between px-2.5 py-2 gap-2 flex-wrap">
                  <div className="flex items-center gap-1.5 flex-wrap">
                    <GitCommitVertical className="w-3 h-3 text-nofx-gold" />
                    <span className="text-xs font-bold text-nofx-text">
                      v{versionNo}
                    </span>
                    <span
                      className={`px-1.5 py-0.5 text-[10px] rounded ${source.className}`}
                    >
                      {tv(source.labelKey, language)}
                    </span>
                    {isCurrent && (
                      <span className="px-1.5 py-0.5 text-[10px] rounded bg-nofx-gold text-black font-medium">
                        {tv('current', language)}
                      </span>
                    )}
                  </div>
                  <span className="text-[10px] text-nofx-text-muted">
                    {fmtTime(v.changed_at, language)}
                  </span>
                </div>
                {/* hashes */}
                <div className="px-2.5 pb-1.5 flex items-center gap-3 text-[10px] text-nofx-text-muted font-mono">
                  <span title={tv('configHash', language)}>
                    {tv('configHash', language)}#{v.config_hash}
                  </span>
                  <span title={tv('riskHash', language)}>
                    {tv('riskHash', language)}#{v.risk_hash}
                  </span>
                </div>
                {/* realized performance */}
                <div className="mx-2.5 mb-2 p-2 rounded bg-nofx-bg border border-nofx-line">
                  <div className="flex items-center justify-between mb-1">
                    <span className="text-[10px] text-nofx-text-muted">
                      {tv('statsTitle', language)}
                    </span>
                  </div>
                  {stats.trades === 0 ? (
                    <p className="text-xs text-nofx-text-muted opacity-70">
                      {tv('noTrades', language)}
                    </p>
                  ) : (
                    <div className="grid grid-cols-5 gap-1 text-center">
                      <div>
                        <div className="text-[10px] text-nofx-text-muted">
                          {tv('statTrades', language)}
                        </div>
                        <div className="text-xs font-medium text-nofx-text">
                          {stats.trades}
                          <span className="text-[9px] text-nofx-text-muted">
                            {' '}
                            ({stats.wins}W/{stats.losses}L)
                          </span>
                        </div>
                      </div>
                      <div>
                        <div className="text-[10px] text-nofx-text-muted">
                          {tv('statWinRate', language)}
                        </div>
                        <div className="text-xs font-medium text-nofx-text">
                          {stats.win_rate.toFixed(1)}%
                        </div>
                      </div>
                      <div>
                        <div className="text-[10px] text-nofx-text-muted">
                          {tv('statNetPnl', language)}
                        </div>
                        <div
                          className={`text-xs font-medium ${stats.net_pnl >= 0 ? 'text-nofx-success' : 'text-nofx-danger'}`}
                        >
                          {fmtSigned(stats.net_pnl)}
                        </div>
                      </div>
                      <div>
                        <div className="text-[10px] text-nofx-text-muted">
                          {tv('statPF', language)}
                        </div>
                        <div className="text-xs font-medium text-nofx-text">
                          {stats.profit_factor.toFixed(2)}
                        </div>
                      </div>
                      <div>
                        <div className="text-[10px] text-nofx-text-muted">
                          {tv('statAvgR', language)}
                        </div>
                        <div
                          className={`text-xs font-medium ${stats.avg_r >= 0 ? 'text-nofx-success' : 'text-nofx-danger'}`}
                        >
                          {stats.r_count > 0 ? fmtSigned(stats.avg_r) : '—'}
                        </div>
                      </div>
                    </div>
                  )}
                </div>
                {/* diff */}
                {v.summary.length === 0 ? (
                  <p className="px-2.5 pb-2 text-[10px] text-nofx-text-muted opacity-70">
                    {tv('diffNone', language)}
                  </p>
                ) : (
                  <div className="px-2.5 pb-2">
                    <button
                      onClick={() => setExpandedDiff(diffOpen ? null : v.id)}
                      className="text-[10px] text-nofx-gold hover:underline mb-1"
                    >
                      {tv('diffTitle', language)} · {v.summary.length}
                      {diffOpen ? ' ▲' : ' ▼'}
                    </button>
                    <div className="space-y-0.5 font-mono">
                      {shownDiff.map(
                        (d: StrategyConfigDiffEntry, i: number) => (
                          <div
                            key={`${d.path}-${i}`}
                            className="text-[10px] leading-4 break-all"
                          >
                            <span className="text-nofx-text">{d.path}</span>
                            <span className="text-nofx-text-muted"> : </span>
                            <span className="text-nofx-danger line-through opacity-80">
                              {renderDiffValue(d.old)}
                            </span>
                            <span className="text-nofx-text-muted"> → </span>
                            <span className="text-nofx-success">
                              {renderDiffValue(d.new)}
                            </span>
                          </div>
                        )
                      )}
                      {!diffOpen && v.summary.length > 6 && (
                        <button
                          onClick={() => setExpandedDiff(v.id)}
                          className="text-[10px] text-nofx-text-muted hover:text-nofx-gold"
                        >
                          …{v.summary.length - 6}{' '}
                          {tv('diffTruncated', language)}
                        </button>
                      )}
                    </div>
                  </div>
                )}
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}
