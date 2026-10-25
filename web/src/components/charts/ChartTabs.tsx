import { useState, useEffect, useRef } from 'react'
import { EquityChart } from './EquityChart'
import { AdvancedChart } from './AdvancedChart'
import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'
import { chartTabs, ts } from '../../i18n/strategy-translations'
import {
  ArrowLeftRight,
  BarChart3,
  Bitcoin,
  CandlestickChart,
  ChevronDown,
  Gem,
  Hexagon,
  Search,
  TrendingUp,
} from 'lucide-react'
import { motion, AnimatePresence } from 'framer-motion'
import { cn } from '../../lib/cn'
import { Button, Card, Input, Segmented, type TabItem } from '../ui'

interface ChartTabsProps {
  traderId: string
  selectedSymbol?: string // Externally selected symbol
  updateKey?: number // Force update key
  exchangeId?: string // Exchange ID
}

type ChartTab = 'equity' | 'kline'
type Interval = '1m' | '5m' | '15m' | '30m' | '1h' | '4h' | '1d'
type MarketType = 'hyperliquid' | 'crypto' | 'stocks' | 'forex' | 'metals'

interface SymbolInfo {
  symbol: string
  name: string
  category: string
}

// Market type configuration
const MARKET_CONFIG = {
  hyperliquid: {
    exchange: 'hyperliquid',
    defaultSymbol: 'BTC',
    icon: Hexagon,
    labelKey: 'hyperliquid' as const,
    hasDropdown: true,
  },
  crypto: {
    exchange: 'binance',
    defaultSymbol: 'BTCUSDT',
    icon: Bitcoin,
    labelKey: 'crypto' as const,
    hasDropdown: false,
  },
  stocks: {
    exchange: 'alpaca',
    defaultSymbol: 'AAPL',
    icon: TrendingUp,
    labelKey: 'stocks' as const,
    hasDropdown: false,
  },
  forex: {
    exchange: 'forex',
    defaultSymbol: 'EUR/USD',
    icon: ArrowLeftRight,
    labelKey: 'forex' as const,
    hasDropdown: false,
  },
  metals: {
    exchange: 'metals',
    defaultSymbol: 'XAU/USD',
    icon: Gem,
    labelKey: 'metals' as const,
    hasDropdown: false,
  },
}

const INTERVALS: { value: Interval; label: string }[] = [
  { value: '1m', label: '1m' },
  { value: '5m', label: '5m' },
  { value: '15m', label: '15m' },
  { value: '30m', label: '30m' },
  { value: '1h', label: '1h' },
  { value: '4h', label: '4h' },
  { value: '1d', label: '1d' },
]

// Infer market type from exchange ID
function getMarketTypeFromExchange(exchangeId: string | undefined): MarketType {
  if (!exchangeId) return 'hyperliquid'
  const lower = exchangeId.toLowerCase()
  if (lower.includes('hyperliquid')) return 'hyperliquid'
  // Other exchanges default to crypto type
  return 'crypto'
}

export function ChartTabs({
  traderId,
  selectedSymbol,
  updateKey,
  exchangeId,
}: ChartTabsProps) {
  const { language } = useLanguage()
  const [activeTab, setActiveTab] = useState<ChartTab>('equity')
  const [chartSymbol, setChartSymbol] = useState<string>('BTC')
  const [interval, setInterval] = useState<Interval>('5m')
  const [symbolInput, setSymbolInput] = useState('')
  const [marketType, setMarketType] = useState<MarketType>(() =>
    getMarketTypeFromExchange(exchangeId)
  )
  const [availableSymbols, setAvailableSymbols] = useState<SymbolInfo[]>([])
  const [showDropdown, setShowDropdown] = useState(false)
  const [searchFilter, setSearchFilter] = useState('')
  const dropdownRef = useRef<HTMLDivElement>(null)

  // Auto-switch market type when exchange ID changes
  useEffect(() => {
    const newMarketType = getMarketTypeFromExchange(exchangeId)
    setMarketType(newMarketType)
  }, [exchangeId])

  // Determine exchange from market type
  const marketConfig = MARKET_CONFIG[marketType]
  // Prefer passed-in exchangeId (when not hyperliquid)
  const currentExchange =
    marketType === 'hyperliquid'
      ? 'hyperliquid'
      : exchangeId || marketConfig.exchange

  // Fetch available symbol list
  useEffect(() => {
    if (marketConfig.hasDropdown) {
      fetch(`/api/symbols?exchange=${marketConfig.exchange}`)
        .then((res) => res.json())
        .then((data) => {
          if (data.symbols) {
            // Sort by category: crypto > stock > forex > commodity > index
            const categoryOrder: Record<string, number> = {
              crypto: 0,
              stock: 1,
              forex: 2,
              commodity: 3,
              index: 4,
            }
            const sorted = [...data.symbols].sort(
              (a: SymbolInfo, b: SymbolInfo) => {
                const orderA = categoryOrder[a.category] ?? 5
                const orderB = categoryOrder[b.category] ?? 5
                if (orderA !== orderB) return orderA - orderB
                return a.symbol.localeCompare(b.symbol)
              }
            )
            setAvailableSymbols(sorted)
          }
        })
        .catch((err) => console.error('Failed to fetch symbols:', err))
    }
  }, [marketType, marketConfig.exchange, marketConfig.hasDropdown])

  // Close dropdown on outside click
  useEffect(() => {
    const handleClickOutside = (event: MouseEvent) => {
      if (
        dropdownRef.current &&
        !dropdownRef.current.contains(event.target as Node)
      ) {
        setShowDropdown(false)
      }
    }
    document.addEventListener('mousedown', handleClickOutside)
    return () => document.removeEventListener('mousedown', handleClickOutside)
  }, [])

  // Update default symbol when switching market type
  const handleMarketTypeChange = (type: MarketType) => {
    setMarketType(type)
    setChartSymbol(MARKET_CONFIG[type].defaultSymbol)
    setShowDropdown(false)
  }

  // Filtered symbol list
  const filteredSymbols = availableSymbols.filter((s) =>
    s.symbol.toLowerCase().includes(searchFilter.toLowerCase())
  )

  // Auto-switch to kline chart when symbol selected externally
  useEffect(() => {
    if (selectedSymbol) {
      setChartSymbol(selectedSymbol)
      setActiveTab('kline')
    }
  }, [selectedSymbol, updateKey])

  // Handle manual symbol input
  const handleSymbolSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (symbolInput.trim()) {
      let symbol = symbolInput.trim().toUpperCase()
      // Auto-append USDT suffix for crypto
      if (marketType === 'crypto' && !symbol.endsWith('USDT')) {
        symbol = symbol + 'USDT'
      }
      setChartSymbol(symbol)
      setSymbolInput('')
    }
  }

  // View switch: equity curve / kline chart (labels shorten on mobile)
  const viewItems: TabItem<ChartTab>[] = [
    {
      key: 'equity',
      icon: <BarChart3 className="h-3.5 w-3.5" />,
      label: (
        <>
          <span className="hidden md:inline">
            {t('accountEquityCurve', language)}
          </span>
          <span className="md:hidden">Eq</span>
        </>
      ),
    },
    {
      key: 'kline',
      icon: <CandlestickChart className="h-3.5 w-3.5" />,
      label: (
        <>
          <span className="hidden md:inline">{t('marketChart', language)}</span>
          <span className="md:hidden">Kline</span>
        </>
      ),
    },
  ]

  return (
    <Card
      className={cn(
        'relative z-10 flex w-full flex-col transition-all duration-300',
        typeof window !== 'undefined' && window.innerWidth < 768
          ? 'h-[500px]'
          : 'h-[600px]'
      )}
    >
      {/* Toolbar: mobile scrolls horizontally; desktop uses flex layout. */}
      <div className="relative z-20 flex shrink-0 flex-wrap items-center justify-between gap-y-2 rounded-t-lg border-b border-line bg-surface-2 px-3 py-2 md:flex-nowrap">
        {/* Left: Tab Switcher */}
        <div className="flex flex-wrap items-center gap-2">
          <Segmented
            value={activeTab}
            onChange={setActiveTab}
            items={viewItems}
          />

          {/* Market Type Pills - Only when kline active, HIDDEN on mobile to save space */}
          {activeTab === 'kline' && (
            <div className="ml-1 hidden items-center border-l border-line pl-2 md:flex">
              <Segmented
                size="sm"
                value={marketType}
                onChange={handleMarketTypeChange}
                items={(Object.keys(MARKET_CONFIG) as MarketType[]).map(
                  (type) => {
                    const config = MARKET_CONFIG[type]
                    const Icon = config.icon
                    return {
                      key: type,
                      icon: <Icon className="h-3 w-3" />,
                      label: ts(chartTabs[config.labelKey], language),
                    }
                  }
                )}
              />
            </div>
          )}
        </div>

        {/* Right: Symbol + Interval */}
        {activeTab === 'kline' && (
          <div className="flex w-full min-w-0 items-center gap-2 md:w-auto md:gap-3">
            {/* Symbol Dropdown */}
            <div className="relative shrink-0" ref={dropdownRef}>
              {marketConfig.hasDropdown ? (
                <>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => setShowDropdown(!showDropdown)}
                    className="font-semibold text-fg"
                  >
                    <span className="num">{chartSymbol}</span>
                    <ChevronDown
                      className={cn(
                        'h-3 w-3 text-fg-3 transition-transform',
                        showDropdown && 'rotate-180'
                      )}
                    />
                  </Button>
                  {showDropdown && (
                    <div className="absolute right-0 top-full z-50 mt-2 w-64 overflow-hidden rounded-lg border border-line bg-surface shadow-pop">
                      <div className="border-b border-line p-2">
                        <div className="flex items-center gap-2 rounded-md border border-line bg-surface-2 px-2 py-1.5 transition-colors focus-within:border-brand">
                          <Search className="h-3.5 w-3.5 shrink-0 text-fg-3" />
                          <input
                            type="text"
                            value={searchFilter}
                            onChange={(e) => setSearchFilter(e.target.value)}
                            placeholder="Search symbol..."
                            className="num flex-1 bg-transparent text-[11px] text-fg placeholder:text-fg-3 focus:outline-none"
                            autoFocus
                          />
                        </div>
                      </div>
                      <div className="custom-scrollbar max-h-60 overflow-y-auto">
                        {['crypto', 'stock', 'forex', 'commodity', 'index'].map(
                          (category) => {
                            const categorySymbols = filteredSymbols.filter(
                              (s) => s.category === category
                            )
                            if (categorySymbols.length === 0) return null
                            const labels: Record<string, string> = {
                              crypto: 'Crypto',
                              stock: 'Stocks',
                              forex: 'Forex',
                              commodity: 'Commodities',
                              index: 'Index',
                            }
                            return (
                              <div key={category}>
                                <div className="bg-surface-2 px-3 py-1.5 text-[10px] font-bold uppercase tracking-wider text-fg-3">
                                  {labels[category]}
                                </div>
                                {categorySymbols.map((s) => (
                                  <button
                                    key={s.symbol}
                                    onClick={() => {
                                      setChartSymbol(s.symbol)
                                      setShowDropdown(false)
                                      setSearchFilter('')
                                    }}
                                    className={cn(
                                      'flex w-full items-center justify-between px-3 py-2 text-left text-[11px] transition-colors hover:bg-surface-hover',
                                      chartSymbol === s.symbol
                                        ? 'bg-brand-soft text-brand'
                                        : 'text-fg-2'
                                    )}
                                  >
                                    <span className="num">{s.symbol}</span>
                                    <span className="text-[10px] text-fg-3">
                                      {s.name}
                                    </span>
                                  </button>
                                ))}
                              </div>
                            )
                          }
                        )}
                      </div>
                    </div>
                  )}
                </>
              ) : (
                <span className="num inline-flex h-7 items-center rounded-md border border-line bg-surface-2 px-2.5 text-xs font-semibold text-fg">
                  {chartSymbol}
                </span>
              )}
            </div>

            {/* Interval Selector - Allow scrolling if needed */}
            <div className="no-scrollbar max-w-[200px] overflow-x-auto md:max-w-none">
              <Segmented
                size="sm"
                value={interval}
                onChange={setInterval}
                items={INTERVALS.map((int) => ({
                  key: int.value,
                  label: int.label,
                }))}
              />
            </div>

            {/* Quick Input - Hidden on mobile, dropdown search is enough */}
            <form
              onSubmit={handleSymbolSubmit}
              className="hidden shrink-0 items-center md:flex"
            >
              <Input
                type="text"
                value={symbolInput}
                onChange={(e) => setSymbolInput(e.target.value)}
                placeholder="Sym"
                className="num h-7 w-16 rounded-r-none px-2 text-[11px]"
              />
              <Button
                type="submit"
                variant="secondary"
                size="sm"
                className="h-7 rounded-l-none border-l-0 px-2 text-[11px] text-fg-2"
              >
                Go
              </Button>
            </form>
          </div>
        )}
      </div>

      {/* Tab Content - Chart autosizes to this container */}
      <div className="relative h-full min-h-0 flex-1 overflow-hidden rounded-b-lg bg-surface">
        <AnimatePresence mode="wait">
          {activeTab === 'equity' ? (
            <motion.div
              key="equity"
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              exit={{ opacity: 0 }}
              transition={{ duration: 0.2 }}
              className="h-full w-full absolute inset-0"
            >
              <EquityChart traderId={traderId} embedded />
            </motion.div>
          ) : (
            <motion.div
              key={`kline-${chartSymbol}-${interval}-${currentExchange}`}
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              exit={{ opacity: 0 }}
              transition={{ duration: 0.2 }}
              className="h-full w-full absolute inset-0"
            >
              <AdvancedChart
                symbol={chartSymbol}
                interval={interval}
                traderID={traderId}
                // Dynamic auto-sizing via ResizeObserver
                exchange={currentExchange}
                onSymbolChange={setChartSymbol}
              />
            </motion.div>
          )}
        </AnimatePresence>
      </div>
    </Card>
  )
}
