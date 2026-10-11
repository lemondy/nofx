import { cn } from '../../lib/cn'
import { Button, Badge, PnL, Change, Card } from '../ui'
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
    className: 'bg-brand-soft text-brand',
  },
  duplicate: {
    labelKey: 'sourceDuplicate',
    className: 'bg-info-soft text-info',
  },
  baseline: {
    labelKey: 'sourceBaseline',
    className: 'bg-surface-2 text-fg-3',
  },
  save: {
    labelKey: 'sourceSave',
    className: 'bg-up-soft text-up',
  },
  external: {
    labelKey: 'sourceExternal',
    className: 'bg-down-soft text-down',
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
      <div className="flex flex-col items-center justify-center py-12 text-fg-3">
        <Loader2 className="w-6 h-6 mb-2 animate-spin" />
        <p className="text-sm">{tv('loading', language)}</p>
      </div>
    )
  }

  if (error) {
    return (
      <div className="p-3 space-y-3">
        <div className="p-3 rounded-lg bg-down-soft border border-down/30 text-sm text-down">
          {tv('loadFailed', language)}: {error}
        </div>
        <Button
          variant="primary"
          size="sm"
          onClick={fetchVersions}
          className="flex items-center gap-1.5 text-brand-fg"
        >
          <RefreshCw className="w-3 h-3" />
          {tv('refresh', language)}
        </Button>
      </div>
    )
  }

  if (!data || data.versions.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-12 text-fg-3">
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
        <p className="text-xs text-fg-3">{tv('subtitle', language)}</p>
        <Button
          variant="ghost"
          size="sm"
          onClick={fetchVersions}
          disabled={isLoading}
          className="flex items-center gap-1 text-fg-3"
        >
          {isLoading ? (
            <Loader2 className="w-3 h-3 animate-spin" />
          ) : (
            <RefreshCw className="w-3 h-3" />
          )}
          {tv('refresh', language)}
        </Button>
      </div>

      <div className="num relative">
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
                className="absolute left-[5px] top-2 bottom-0 w-px bg-line"
                aria-hidden
              />
              <div
                className={cn(
                  'absolute left-0 top-1.5 w-[11px] h-[11px] rounded-full border-2',
                  isCurrent ? 'border-brand' : 'border-brand/35',
                  isCurrent ? 'bg-brand' : 'bg-transparent'
                )}
                aria-hidden
              />
              <Card dense className="overflow-hidden">
                {/* header */}
                <div className="flex min-h-9 items-center justify-between border-b border-line bg-surface-2 px-3 py-2 gap-2 flex-wrap">
                  <div className="flex items-center gap-1.5 flex-wrap">
                    <GitCommitVertical className="w-3 h-3 text-brand" />
                    <span className="num text-xs font-semibold text-fg">
                      v{versionNo}
                    </span>
                    <Badge className={source.className} size="xs">
                      {tv(source.labelKey, language)}
                    </Badge>
                    {isCurrent && (
                      <Badge variant="brand" size="xs">
                        {tv('current', language)}
                      </Badge>
                    )}
                  </div>
                  <span className="num text-xs text-fg-3">
                    {fmtTime(v.changed_at, language)}
                  </span>
                </div>
                {/* hashes */}
                <div className="px-3 pb-2 flex flex-wrap items-center gap-2 break-all text-xs text-fg-3 num">
                  <span title={tv('configHash', language)}>
                    {tv('configHash', language)}#{v.config_hash}
                  </span>
                  <span title={tv('riskHash', language)}>
                    {tv('riskHash', language)}#{v.risk_hash}
                  </span>
                </div>
                {/* realized performance */}
                <div className="mx-2.5 mb-2 p-2 rounded bg-bg border border-line">
                  <div className="flex items-center justify-between mb-1">
                    <span className="text-xs text-fg-3">
                      {tv('statsTitle', language)}
                    </span>
                    {stats.low_sample && stats.trades > 0 && (
                      <Badge variant="warn" size="xs">
                        {tv('lowSample', language)}
                      </Badge>
                    )}
                  </div>
                  {stats.trades === 0 ? (
                    <p className="text-xs text-fg-3 opacity-70">
                      {tv('noTrades', language)}
                    </p>
                  ) : (
                    <div className="grid grid-cols-2 gap-3 text-right sm:grid-cols-3">
                      <div>
                        <div className="text-xs text-fg-3">
                          {tv('statTrades', language)}
                        </div>
                        <div className="num text-xs font-medium text-fg">
                          {stats.trades}
                          <span className="text-[11px] text-fg-3">
                            {' '}
                            ({stats.wins}W/{stats.losses}L)
                          </span>
                        </div>
                      </div>
                      <div>
                        <div className="text-xs text-fg-3">
                          {tv('statWinRate', language)}
                        </div>
                        <div className="num text-xs font-medium text-fg">
                          {stats.win_rate.toFixed(1)}%
                          {/* review 2026-10-09 I: Wilson 95% interval */}
                          {stats.win_rate_hi != null && (
                            <span className="num text-[11px] text-fg-3">
                              {' '}
                              ({(stats.win_rate_lo ?? 0).toFixed(0)}–
                              {stats.win_rate_hi.toFixed(0)}%)
                            </span>
                          )}
                        </div>
                      </div>
                      <div>
                        <div className="text-xs text-fg-3">
                          {tv('statNetPnl', language)}
                        </div>
                        <div
                          className={`text-xs font-medium ${stats.net_pnl >= 0 ? 'text-up' : 'text-down'}`}
                        >
                          <PnL value={stats.net_pnl} currency="" />
                        </div>
                      </div>
                      <div>
                        <div className="text-xs text-fg-3">
                          {tv('statPF', language)}
                        </div>
                        <div className="num text-xs font-medium text-fg">
                          {stats.profit_factor.toFixed(2)}
                        </div>
                      </div>
                      <div>
                        <div className="text-xs text-fg-3">
                          {tv('statAvgR', language)}
                        </div>
                        <div
                          className={`text-xs font-medium ${stats.avg_r >= 0 ? 'text-up' : 'text-down'}`}
                        >
                          <Change
                            value={stats.r_count > 0 ? stats.avg_r : null}
                            suffix=""
                          />
                        </div>
                      </div>
                    </div>
                  )}
                  {/* review 2026-10-09 I: attribution / exclusion notes */}
                  {((stats.crossed_version ?? 0) > 0 ||
                    (stats.excluded_manual ?? 0) > 0 ||
                    (stats.excluded_unattributed ?? 0) > 0) && (
                    <p className="mt-1 text-[11px] text-fg-3 opacity-80">
                      {(stats.crossed_version ?? 0) > 0 &&
                        `${tv('crossedVersion', language)} ${stats.crossed_version}`}
                      {(stats.crossed_version ?? 0) > 0 &&
                        ((stats.excluded_manual ?? 0) > 0 ||
                          (stats.excluded_unattributed ?? 0) > 0) &&
                        ' · '}
                      {((stats.excluded_manual ?? 0) > 0 ||
                        (stats.excluded_unattributed ?? 0) > 0) &&
                        `${tv('excluded', language)} ${tv('excludedManual', language)} ${stats.excluded_manual ?? 0} / ${tv('excludedUnattributed', language)} ${stats.excluded_unattributed ?? 0}`}
                    </p>
                  )}
                </div>
                {/* diff */}
                {v.summary.length === 0 ? (
                  <p className="px-2.5 pb-2 text-xs text-fg-3 opacity-70">
                    {tv('diffNone', language)}
                  </p>
                ) : (
                  <div className="px-2.5 pb-2">
                    <Button
                      variant="ghost"
                      onClick={() => setExpandedDiff(diffOpen ? null : v.id)}
                      className="text-brand mb-1"
                    >
                      {tv('diffTitle', language)} · {v.summary.length}
                      {diffOpen ? ' ▲' : ' ▼'}
                    </Button>
                    <div className="space-y-0.5 num">
                      {shownDiff.map(
                        (d: StrategyConfigDiffEntry, i: number) => (
                          <div
                            key={`${d.path}-${i}`}
                            className="text-xs leading-4 break-all"
                          >
                            <span className="text-fg">{d.path}</span>
                            <span className="text-fg-3"> : </span>
                            <span className="text-down line-through opacity-80">
                              {renderDiffValue(d.old)}
                            </span>
                            <span className="text-fg-3"> → </span>
                            <span className="text-up">
                              {renderDiffValue(d.new)}
                            </span>
                          </div>
                        )
                      )}
                      {!diffOpen && v.summary.length > 6 && (
                        <Button
                          variant="ghost"
                          onClick={() => setExpandedDiff(v.id)}
                          className="text-fg-3"
                        >
                          …{v.summary.length - 6}{' '}
                          {tv('diffTruncated', language)}
                        </Button>
                      )}
                    </div>
                  </div>
                )}
              </Card>
            </div>
          )
        })}
      </div>
    </div>
  )
}
