import { useState } from 'react'
import useSWR from 'swr'
import {
  TrendingUp,
  Shield,
  Zap,
  Eye,
  EyeOff,
  Copy,
  Check,
  Layers,
  Target,
  Activity,
  Search,
  type LucideIcon,
} from 'lucide-react'
import { useLanguage } from '../contexts/LanguageContext'
import { useAuth } from '../contexts/AuthContext'
import { toast } from 'sonner'
import { t } from '../i18n/translations'
import {
  Badge,
  Button,
  Card,
  CardHeader,
  CardBody,
  EmptyState,
  Input,
  Segmented,
  Skeleton,
  Stat,
} from '../components/ui'

interface PublicStrategy {
  id: string
  name: string
  description: string
  author_email?: string
  is_public: boolean
  config_visible: boolean
  config?: any
  stats?: {
    used_by: number
    rating: number
  }
  created_at: string
  updated_at: string
}

const strategyStyles: Record<string, { color: string; icon: LucideIcon }> = {
  scalper: { color: 'text-brand', icon: Zap },
  swing: { color: 'text-info', icon: TrendingUp },
  arbitrage: { color: 'text-ai', icon: Layers },
  conservative: { color: 'text-up', icon: Shield },
  aggressive: { color: 'text-down', icon: Target },
  default: { color: 'text-fg-3', icon: Activity },
}

function getStrategyStyle(name: string) {
  const lowerName = name.toLowerCase()
  if (lowerName.includes('scalp')) return strategyStyles.scalper
  if (lowerName.includes('swing')) return strategyStyles.swing
  if (lowerName.includes('arb')) return strategyStyles.arbitrage
  if (lowerName.includes('safe') || lowerName.includes('conserv'))
    return strategyStyles.conservative
  if (lowerName.includes('aggress') || lowerName.includes('high'))
    return strategyStyles.aggressive
  return strategyStyles.default
}

export function StrategyMarketPage() {
  const { language } = useLanguage()
  const { token, user } = useAuth()
  const [searchQuery, setSearchQuery] = useState('')
  const [selectedCategory, setSelectedCategory] = useState<string>('all')
  const [copiedId, setCopiedId] = useState<string | null>(null)

  const tr = (key: string) => t(`strategyMarket.${key}`, language)

  // Fetch public strategies
  const { data: strategies, isLoading } = useSWR<PublicStrategy[]>(
    'public-strategies',
    async () => {
      const response = await fetch('/api/strategies/public')
      if (!response.ok) throw new Error('Failed to fetch strategies')
      const data = await response.json()
      return data.strategies || []
    },
    {
      refreshInterval: 60000,
      revalidateOnFocus: false,
    }
  )

  const filteredStrategies =
    strategies?.filter((s) => {
      if (searchQuery) {
        const query = searchQuery.toLowerCase()
        return (
          s.name.toLowerCase().includes(query) ||
          s.description?.toLowerCase().includes(query)
        )
      }
      return true
    }) || []

  const handleCopyConfig = async (strategy: PublicStrategy) => {
    if (!strategy.config) return
    try {
      await navigator.clipboard.writeText(
        JSON.stringify(strategy.config, null, 2)
      )
      setCopiedId(strategy.id)
      toast.success(tr('copied'))
      setTimeout(() => setCopiedId(null), 2000)
    } catch (err) {
      console.error('Failed to copy:', err)
    }
  }

  const formatDate = (dateStr: string) => {
    const date = new Date(dateStr)
    return date
      .toLocaleDateString('en-US', {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        hour12: false,
      })
      .replace(',', '')
  }

  const getIndicatorList = (config: any) => {
    if (!config?.indicators) return []
    const indicators = []
    if (config.indicators.enable_ema) indicators.push('EMA')
    if (config.indicators.enable_macd) indicators.push('MACD')
    if (config.indicators.enable_rsi) indicators.push('RSI')
    if (config.indicators.enable_atr) indicators.push('ATR')
    if (config.indicators.enable_boll) indicators.push('BOLL')
    if (config.indicators.enable_volume) indicators.push('VOL')
    if (config.indicators.enable_oi) indicators.push('OI')
    if (config.indicators.enable_funding_rate) indicators.push('FR')
    return indicators
  }

  return (
    <main className="min-h-screen bg-bg px-3 py-4 text-fg sm:px-6">
      <div className="mx-auto max-w-[1600px] space-y-4">
        <header className="border-b border-line pb-4">
          <div className="mb-2 flex items-center gap-2">
            <Layers className="h-5 w-5 text-brand" />
            <h1 className="text-xl font-semibold">{tr('title')}</h1>
          </div>
          <p className="text-xs text-fg-2">{tr('subtitle')}</p>
          <p className="mt-1 text-sm text-fg-3">{tr('description')}</p>
        </header>

        <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
          <div className="relative min-w-0 flex-1">
            <Search className="pointer-events-none absolute left-3 top-2 h-4 w-4 text-fg-3" />
            <Input
              aria-label={tr('search')}
              placeholder={tr('search')}
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              className="pl-9"
            />
          </div>
          <Segmented
            items={['all', 'popular', 'recent'].map((key) => ({
              key,
              label: tr(key),
            }))}
            value={selectedCategory}
            onChange={setSelectedCategory}
            className="self-start sm:self-auto"
          />
        </div>

        {isLoading && (
          <div
            role="status"
            aria-label={tr('loading')}
            className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4"
          >
            {[0, 1, 2, 3].map((key) => (
              <Card key={key} dense>
                <CardBody className="space-y-3">
                  <Skeleton className="h-5 w-2/3" />
                  <Skeleton className="h-10 w-full" />
                  <Skeleton className="h-20 w-full" />
                  <Skeleton className="h-8 w-full" />
                </CardBody>
              </Card>
            ))}
          </div>
        )}

        {!isLoading && filteredStrategies.length === 0 && (
          <Card dense>
            <EmptyState
              icon={<Activity className="h-8 w-8" />}
              title={tr('noStrategies')}
              description={tr('noStrategiesDesc')}
            />
          </Card>
        )}

        {!isLoading && filteredStrategies.length > 0 && (
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
            {filteredStrategies.map((strategy) => {
              const style = getStrategyStyle(strategy.name)
              const Icon = style.icon
              const indicators = getIndicatorList(strategy.config)
              return (
                <Card
                  key={strategy.id}
                  dense
                  className="flex min-w-0 flex-col transition-colors hover:border-line-strong"
                >
                  <CardHeader
                    title={
                      <span className="flex min-w-0 items-center gap-2">
                        <Icon className={`h-4 w-4 shrink-0 ${style.color}`} />
                        <span className="truncate" title={strategy.name}>
                          {strategy.name}
                        </span>
                      </span>
                    }
                    actions={
                      <Badge
                        variant={strategy.config_visible ? 'up' : 'neutral'}
                        size="xs"
                      >
                        {strategy.config_visible ? (
                          <Eye size={12} />
                        ) : (
                          <EyeOff size={12} />
                        )}
                        {strategy.config_visible
                          ? 'PUBLIC_ACCESS'
                          : tr('configHidden')}
                      </Badge>
                    }
                  />
                  <CardBody className="flex flex-1 flex-col gap-3">
                    <p className="min-h-9 text-xs text-fg-3 line-clamp-2">
                      {strategy.description || 'NO_DESCRIPTION_AVAILABLE'}
                    </p>
                    <div className="grid grid-cols-2 gap-2 text-xs">
                      <div className="min-w-0">
                        <div className="text-fg-3">{tr('author')}</div>
                        <div className="truncate text-fg-2">
                          @{strategy.author_email?.split('@')[0] || 'UNKNOWN'}
                        </div>
                      </div>
                      <div className="text-right">
                        <div className="text-fg-3">{tr('createdAt')}</div>
                        <div className="num text-[11px] text-fg-2">
                          {formatDate(strategy.created_at)}
                        </div>
                      </div>
                    </div>
                    <div className="flex-1 rounded-md border border-line bg-surface-2 p-3">
                      {strategy.config_visible && strategy.config ? (
                        <div className="space-y-3">
                          <div className="flex flex-wrap gap-1">
                            {indicators.length > 0 ? (
                              indicators.map((ind) => (
                                <Badge key={ind} size="xs">
                                  {ind}
                                </Badge>
                              ))
                            ) : (
                              <span className="text-xs text-fg-3">
                                NO_INDICATORS
                              </span>
                            )}
                          </div>
                          {strategy.config.risk_control && (
                            <div className="grid grid-cols-2 gap-3">
                              <Stat
                                label={tr('maxLeverage')}
                                value={`${strategy.config.risk_control.btc_eth_max_leverage || '-'}x`}
                              />
                              <Stat
                                label={tr('maxPositions')}
                                value={
                                  strategy.config.risk_control.max_positions ||
                                  '-'
                                }
                                className="text-right [&>div]:justify-end"
                              />
                            </div>
                          )}
                        </div>
                      ) : (
                        <div className="flex min-h-20 items-center justify-center gap-2 text-xs text-fg-3">
                          <EyeOff size={16} className="shrink-0" />
                          {tr('configHiddenDesc')}
                        </div>
                      )}
                    </div>
                    {strategy.config_visible && strategy.config ? (
                      <Button
                        onClick={() => handleCopyConfig(strategy)}
                        className="w-full"
                      >
                        {copiedId === strategy.id ? (
                          <>
                            <Check className="h-3.5 w-3.5 text-up" />
                            <span className="text-up">{tr('copied')}</span>
                          </>
                        ) : (
                          <>
                            <Copy className="h-3.5 w-3.5" />
                            {tr('copyConfig')}
                          </>
                        )}
                      </Button>
                    ) : (
                      <Button disabled className="w-full">
                        <Shield size={14} />
                        {tr('hideConfig')}
                      </Button>
                    )}
                  </CardBody>
                </Card>
              )
            })}
          </div>
        )}

        {user && token && (
          <div className="flex justify-center border-t border-line pt-4">
            <Button
              variant="primary"
              onClick={() => (window.location.href = '/strategy')}
            >
              <Layers size={16} />
              {tr('shareYours')}
            </Button>
          </div>
        )}
      </div>
    </main>
  )
}
