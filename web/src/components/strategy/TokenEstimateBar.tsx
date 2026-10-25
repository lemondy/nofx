import { useState, useEffect, useRef } from 'react'
import { Tooltip } from '../ui'
import { Loader2, Info } from 'lucide-react'
import type { StrategyConfig } from '../../types'
import { t, type Language } from '../../i18n/translations'

const API_BASE = import.meta.env.VITE_API_BASE || ''

interface ModelLimit {
  name: string
  context_limit: number
  usage_pct: number
  level: string
}

interface TokenEstimateResult {
  total: number
  model_limits: ModelLimit[]
  suggestions: string[]
}

interface TokenEstimateBarProps {
  config: StrategyConfig | null
  language: Language
  onTokenCountChange?: (total: number) => void
}

export function TokenEstimateBar({
  config,
  language,
  onTokenCountChange,
}: TokenEstimateBarProps) {
  const [estimate, setEstimate] = useState<TokenEstimateResult | null>(null)
  const [isLoading, setIsLoading] = useState(false)
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const tr = (key: string) => t(`strategyStudio.${key}`, language)

  useEffect(() => {
    if (!config) {
      setEstimate(null)
      return
    }

    if (debounceRef.current) {
      clearTimeout(debounceRef.current)
    }

    debounceRef.current = setTimeout(async () => {
      setIsLoading(true)
      try {
        const response = await fetch(
          `${API_BASE}/api/strategies/estimate-tokens`,
          {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ config }),
          }
        )
        if (response.ok) {
          const data = await response.json()
          setEstimate(data)
          onTokenCountChange?.(data.total)
        }
      } catch {
        // silently ignore — non-critical UI element
      } finally {
        setIsLoading(false)
      }
    }, 800)

    return () => {
      if (debounceRef.current) {
        clearTimeout(debounceRef.current)
      }
    }
  }, [config])

  if (!config) return null

  if (isLoading && !estimate) {
    return (
      <div className="flex items-center gap-1.5 text-xs text-fg-3">
        <Loader2 className="w-3 h-3 animate-spin" />
        <span>{tr('tokenEstimating')}</span>
      </div>
    )
  }

  if (!estimate) return null

  // Display based on 200K reference
  const pct = Math.round((estimate.total * 100) / 200000)
  const barWidth = Math.min(pct, 100)

  const barClass = pct >= 100 ? 'bg-down' : pct >= 80 ? 'bg-brand' : 'bg-up'
  const textClass =
    pct >= 100 ? 'text-down' : pct >= 80 ? 'text-brand' : 'text-fg-3'

  return (
    <div className="space-y-1">
      <div className="flex items-center gap-2">
        <div className="flex-1 h-1.5 rounded-full overflow-hidden bg-surface-2">
          <div
            className={`h-full rounded-full transition-[width] duration-500 ${barClass}`}
            style={{ width: `${barWidth}%` }}
          />
        </div>
        <span className={`text-xs num whitespace-nowrap ${textClass}`}>
          {isLoading ? (
            <Loader2 className="w-3 h-3 animate-spin inline" />
          ) : (
            `${pct}%`
          )}
        </span>
        <Tooltip
          className="[&>[role=tooltip]]:left-auto [&>[role=tooltip]]:right-0 [&>[role=tooltip]]:translate-x-0"
          content={
            <>
              {tr('tokenTooltip')} (
              <span className="num">
                ~{estimate.total.toLocaleString()} / 200K
              </span>
              )
            </>
          }
        >
          <button
            type="button"
            aria-label={tr('tokenTooltip')}
            className="flex h-7 w-7 items-center justify-center rounded-md text-fg-3 hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50"
          >
            <Info className="h-3.5 w-3.5" />
          </button>
        </Tooltip>
      </div>
    </div>
  )
}
