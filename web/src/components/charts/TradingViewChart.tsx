import { useEffect, useRef, useState, memo } from 'react'
import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'
import { ChevronDown, Maximize, TrendingUp, X } from 'lucide-react'
import { useTheme } from '../../lib/theme'
import { getChartTheme } from '../../lib/chartTheme'
import { cn } from '../../lib/cn'
import { Button, Card, Input, Segmented } from '../ui'

// 支持的交易所列表 (合约格式)
const EXCHANGES = [
  { id: 'BINANCE', name: 'Binance', prefix: 'BINANCE:', suffix: '.P' },
  { id: 'BYBIT', name: 'Bybit', prefix: 'BYBIT:', suffix: '.P' },
  { id: 'OKX', name: 'OKX', prefix: 'OKX:', suffix: '.P' },
  { id: 'BITGET', name: 'Bitget', prefix: 'BITGET:', suffix: '.P' },
  { id: 'MEXC', name: 'MEXC', prefix: 'MEXC:', suffix: '.P' },
  { id: 'GATEIO', name: 'Gate.io', prefix: 'GATEIO:', suffix: '.P' },
] as const

// 热门交易对
const POPULAR_SYMBOLS = [
  'BTCUSDT',
  'ETHUSDT',
  'SOLUSDT',
  'BNBUSDT',
  'XRPUSDT',
  'DOGEUSDT',
  'ADAUSDT',
  'AVAXUSDT',
  'DOTUSDT',
  'LINKUSDT',
  'MATICUSDT',
  'LTCUSDT',
]

// 时间周期选项
const INTERVALS = [
  { id: '1', label: '1m' },
  { id: '5', label: '5m' },
  { id: '15', label: '15m' },
  { id: '30', label: '30m' },
  { id: '60', label: '1H' },
  { id: '240', label: '4H' },
  { id: 'D', label: '1D' },
  { id: 'W', label: '1W' },
]

interface TradingViewChartProps {
  defaultSymbol?: string
  defaultExchange?: string
  height?: number
  showToolbar?: boolean
  embedded?: boolean // 嵌入模式（不显示外层卡片）
}

function TradingViewChartComponent({
  defaultSymbol = 'BTCUSDT',
  defaultExchange = 'BINANCE',
  height = 400,
  showToolbar = true,
  embedded = false,
}: TradingViewChartProps) {
  const { language } = useLanguage()
  const appTheme = useTheme()
  const containerRef = useRef<HTMLDivElement>(null)
  const [exchange, setExchange] = useState(defaultExchange)
  const [symbol, setSymbol] = useState(defaultSymbol)
  const [timeInterval, setTimeInterval] = useState('60')
  const [customSymbol, setCustomSymbol] = useState('')
  const [showExchangeDropdown, setShowExchangeDropdown] = useState(false)
  const [showSymbolDropdown, setShowSymbolDropdown] = useState(false)
  const [isFullscreen, setIsFullscreen] = useState(false)

  // 当外部传入的 defaultSymbol 变化时，更新内部 symbol
  useEffect(() => {
    if (defaultSymbol && defaultSymbol !== symbol) {
      // console.log('[TradingViewChart] 更新币种:', defaultSymbol)
      setSymbol(defaultSymbol)
    }
  }, [defaultSymbol])

  // 当外部传入的 defaultExchange 变化时，更新内部 exchange
  useEffect(() => {
    if (defaultExchange && defaultExchange !== exchange) {
      const normalizedExchange = defaultExchange.toUpperCase()
      // console.log('[TradingViewChart] 更新交易所:', normalizedExchange)
      if (EXCHANGES.some((e) => e.id === normalizedExchange)) {
        setExchange(normalizedExchange)
      }
    }
  }, [defaultExchange])

  // 获取完整的交易对符号 (合约格式: BINANCE:BTCUSDT.P)
  const getFullSymbol = () => {
    const exchangeInfo = EXCHANGES.find((e) => e.id === exchange)
    const prefix = exchangeInfo?.prefix || 'BINANCE:'
    const suffix = exchangeInfo?.suffix || '.P'
    return `${prefix}${symbol}${suffix}`
  }

  // 加载 TradingView Widget
  useEffect(() => {
    if (!containerRef.current) return

    // 清空容器
    containerRef.current.innerHTML = ''

    // 创建 widget 容器
    const widgetContainer = document.createElement('div')
    widgetContainer.className = 'tradingview-widget-container'
    widgetContainer.style.height = '100%'
    widgetContainer.style.width = '100%'

    const widgetDiv = document.createElement('div')
    widgetDiv.className = 'tradingview-widget-container__widget'
    widgetDiv.style.height = '100%'
    widgetDiv.style.width = '100%'

    widgetContainer.appendChild(widgetDiv)
    containerRef.current.appendChild(widgetContainer)

    // 加载 TradingView 脚本
    const script = document.createElement('script')
    script.src =
      'https://s3.tradingview.com/external-embedding/embed-widget-advanced-chart.js'
    script.type = 'text/javascript'
    script.async = true
    script.innerHTML = JSON.stringify({
      width: '100%',
      height: '100%',
      symbol: getFullSymbol(),
      interval: timeInterval,
      timezone:
        Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Shanghai',
      theme: appTheme,
      style: '1',
      locale: language === 'zh' ? 'zh_CN' : 'en',
      enable_publishing: false,
      // 外部 TradingView 组件不认识 CSS 变量, 传解析后的实际颜色
      backgroundColor: getChartTheme().background,
      gridColor: getChartTheme().grid,
      hide_top_toolbar: !showToolbar,
      hide_legend: false,
      save_image: false,
      calendar: false,
      hide_volume: false,
      support_host: 'https://www.tradingview.com',
    })

    widgetContainer.appendChild(script)

    return () => {
      if (containerRef.current) {
        containerRef.current.innerHTML = ''
      }
    }
  }, [exchange, symbol, timeInterval, language, showToolbar, appTheme])

  // 处理自定义交易对输入
  const handleCustomSymbolSubmit = () => {
    if (customSymbol.trim()) {
      let sym = customSymbol.trim().toUpperCase()
      // 如果没有 USDT 后缀，自动加上
      if (!sym.endsWith('USDT')) {
        sym = sym + 'USDT'
      }
      setSymbol(sym)
      setCustomSymbol('')
      setShowSymbolDropdown(false)
    }
  }

  const body = (
    <>
      {/* Header */}
      <div
        className={cn(
          'flex flex-wrap items-center gap-2 p-3 sm:p-4',
          !embedded && 'border-b border-line'
        )}
      >
        {!embedded && (
          <div className="flex items-center gap-2">
            <TrendingUp className="h-4 w-4 text-fg-3" />
            <h3 className="text-sm font-semibold text-fg sm:text-base">
              {t('marketChart', language)}
            </h3>
          </div>
        )}

        {/* Controls */}
        <div
          className={cn(
            'flex flex-wrap items-center gap-2',
            !embedded && 'ml-auto'
          )}
        >
          {/* Exchange Selector */}
          <div className="relative">
            <Button
              variant="secondary"
              size="sm"
              onClick={() => {
                setShowExchangeDropdown(!showExchangeDropdown)
                setShowSymbolDropdown(false)
              }}
            >
              {EXCHANGES.find((e) => e.id === exchange)?.name || exchange}
              <ChevronDown className="h-3.5 w-3.5 text-fg-3" />
            </Button>

            {showExchangeDropdown && (
              <div className="absolute left-0 top-full z-20 mt-1 min-w-[120px] rounded-md border border-line bg-surface py-1 shadow-pop">
                {EXCHANGES.map((ex) => (
                  <button
                    key={ex.id}
                    type="button"
                    onClick={() => {
                      setExchange(ex.id)
                      setShowExchangeDropdown(false)
                    }}
                    className={cn(
                      'block w-full px-3 py-1.5 text-left text-xs font-medium transition-colors',
                      exchange === ex.id
                        ? 'bg-brand-soft text-brand'
                        : 'text-fg hover:bg-surface-hover'
                    )}
                  >
                    {ex.name}
                  </button>
                ))}
              </div>
            )}
          </div>

          {/* Symbol Selector */}
          <div className="relative">
            <Button
              variant="secondary"
              size="sm"
              onClick={() => {
                setShowSymbolDropdown(!showSymbolDropdown)
                setShowExchangeDropdown(false)
              }}
              className="border-brand/30 bg-brand-soft font-bold text-brand hover:bg-brand-soft"
            >
              {symbol}
              <ChevronDown className="h-3.5 w-3.5" />
            </Button>

            {showSymbolDropdown && (
              <div className="absolute left-0 top-full z-20 mt-1 w-[280px] rounded-md border border-line bg-surface py-2 shadow-pop">
                {/* Custom Input */}
                <div className="border-b border-line px-3 pb-2">
                  <div className="flex gap-2">
                    <Input
                      type="text"
                      value={customSymbol}
                      onChange={(e) =>
                        setCustomSymbol(e.target.value.toUpperCase())
                      }
                      onKeyDown={(e) =>
                        e.key === 'Enter' && handleCustomSymbolSubmit()
                      }
                      placeholder={t('enterSymbol', language)}
                      className="h-7 flex-1 text-xs"
                    />
                    <Button
                      variant="primary"
                      size="sm"
                      onClick={handleCustomSymbolSubmit}
                    >
                      OK
                    </Button>
                  </div>
                </div>

                {/* Popular Symbols */}
                <div className="px-2 pt-2">
                  <div className="mb-1 px-2 py-1 text-xs text-fg-3">
                    {t('popularSymbols', language)}
                  </div>
                  <div className="grid grid-cols-3 gap-1">
                    {POPULAR_SYMBOLS.map((sym) => (
                      <button
                        key={sym}
                        type="button"
                        onClick={() => {
                          setSymbol(sym)
                          setShowSymbolDropdown(false)
                        }}
                        className={cn(
                          'rounded-md px-2 py-1.5 text-xs font-medium transition-colors',
                          symbol === sym
                            ? 'bg-brand-soft text-brand'
                            : 'bg-surface-2 text-fg hover:bg-surface-hover'
                        )}
                      >
                        {sym.replace('USDT', '')}
                      </button>
                    ))}
                  </div>
                </div>
              </div>
            )}
          </div>

          {/* Interval Selector */}
          <Segmented
            size="sm"
            value={timeInterval}
            onChange={setTimeInterval}
            items={INTERVALS.map((int) => ({ key: int.id, label: int.label }))}
          />

          {/* Fullscreen Toggle */}
          <Button
            variant={isFullscreen ? 'primary' : 'secondary'}
            size="sm"
            className="w-7 px-0"
            onClick={() => setIsFullscreen(!isFullscreen)}
            title={
              isFullscreen
                ? t('exitFullscreen', language)
                : t('fullscreen', language)
            }
          >
            {isFullscreen ? (
              <X className="h-4 w-4" />
            ) : (
              <Maximize className="h-4 w-4" />
            )}
          </Button>
        </div>
      </div>

      {/* Chart Container */}
      <div
        ref={containerRef}
        className="overflow-hidden bg-surface"
        style={{ height: isFullscreen ? 'calc(100vh - 65px)' : height }}
      />

      {/* Click outside to close dropdowns */}
      {(showExchangeDropdown || showSymbolDropdown) && (
        <div
          className="fixed inset-0 z-10"
          onClick={() => {
            setShowExchangeDropdown(false)
            setShowSymbolDropdown(false)
          }}
        />
      )}
    </>
  )

  const rootClass = cn(
    'overflow-hidden',
    isFullscreen && 'fixed inset-0 z-50 flex flex-col rounded-none bg-surface'
  )

  return embedded ? (
    <div className={rootClass}>{body}</div>
  ) : (
    <Card className={cn(rootClass, !isFullscreen && 'animate-fade-in')}>
      {body}
    </Card>
  )
}

// 使用 memo 避免不必要的重渲染
export const TradingViewChart = memo(TradingViewChartComponent)
