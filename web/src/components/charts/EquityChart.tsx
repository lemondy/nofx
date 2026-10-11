import { useState, type ReactNode } from 'react'
import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
  ReferenceLine,
} from 'recharts'
import useSWR from 'swr'
import { api } from '../../lib/api'
import { useLanguage } from '../../contexts/LanguageContext'
import { useAuth } from '../../contexts/AuthContext'
import { t } from '../../i18n/translations'
import {
  AlertTriangle,
  BarChart3,
  DollarSign,
  Percent,
  TrendingUp as ArrowUp,
  TrendingDown as ArrowDown,
} from 'lucide-react'
import { cn } from '../../lib/cn'
import {
  Card,
  EmptyState,
  NofxSelect,
  Segmented,
  Skeleton,
  formatSigned,
} from '../ui'

interface EquityPoint {
  timestamp: string
  total_equity: number
  pnl: number
  pnl_pct: number
  cycle_number: number
}

interface EquityChartProps {
  traderId?: string
  embedded?: boolean // 嵌入模式（不显示外层卡片）
}

type DisplayMode = 'dollar' | 'percent'

export function EquityChart({ traderId, embedded = false }: EquityChartProps) {
  const { language } = useLanguage()
  const { user, token } = useAuth()
  const [displayMode, setDisplayMode] = useState<DisplayMode>('dollar')
  // 时间范围选择（周期数切片，数据仍在内存里 — 纯展示层过滤）
  const [range, setRange] = useState<number | 'all'>(2000)

  const {
    data: history,
    error,
    isLoading,
  } = useSWR<EquityPoint[]>(
    user && token && traderId ? `equity-history-${traderId}` : null,
    () => api.getEquityHistory(traderId, true),
    {
      refreshInterval: 30000, // 30秒刷新（历史数据更新频率较低）
      revalidateOnFocus: false,
      dedupingInterval: 20000,
    }
  )

  const { data: account } = useSWR(
    user && token && traderId ? `account-${traderId}` : null,
    () => api.getAccount(traderId, true),
    {
      refreshInterval: 15000, // 15秒刷新（配合后端缓存）
      revalidateOnFocus: false,
      dedupingInterval: 10000,
    }
  )

  // 外层容器: 嵌入模式(ChartTabs 内)不加卡片外观
  const frame = (children: ReactNode, animate = false) =>
    embedded ? (
      <div className="p-3 sm:p-4">{children}</div>
    ) : (
      <Card className={cn('p-3 sm:p-4', animate && 'animate-fade-in')}>
        {children}
      </Card>
    )

  // Loading state - show skeleton
  if (isLoading) {
    return frame(
      <>
        {!embedded && (
          <h3 className="mb-4 text-sm font-semibold text-fg sm:text-base">
            {t('accountEquityCurve', language)}
          </h3>
        )}
        <Skeleton className="h-64 w-full" />
      </>
    )
  }

  if (error) {
    return frame(
      <div className="flex items-center gap-3 rounded-md border border-down/20 bg-down-soft p-3">
        <AlertTriangle className="h-5 w-5 shrink-0 text-down" />
        <div className="min-w-0">
          <div className="text-sm font-semibold text-down">
            {t('loadingError', language)}
          </div>
          <div className="text-xs text-fg-3">{error.message}</div>
        </div>
      </div>
    )
  }

  // 过滤掉无效数据点：total_equity为0或小于1的数据点（API失败导致）
  const validHistory = history?.filter((point) => point.total_equity > 1) || []

  if (!validHistory || validHistory.length === 0) {
    return frame(
      <>
        {!embedded && (
          <h3 className="mb-4 text-sm font-semibold text-fg sm:text-base">
            {t('accountEquityCurve', language)}
          </h3>
        )}
        <EmptyState
          icon={<BarChart3 className="h-10 w-10" />}
          title={t('noHistoricalData', language)}
          description={t('dataWillAppear', language)}
          className="py-12"
        />
      </>
    )
  }

  // 按所选范围切片显示（性能优化）：50 周期（约4小时）到全部，下拉选择
  const RANGE_OPTIONS: Array<{ label: string; value: number | 'all' }> = [
    {
      label: language === 'zh' ? '最近 50 周期' : 'Last 50',
      value: 50,
    },
    {
      label: language === 'zh' ? '最近 100 周期' : 'Last 100',
      value: 100,
    },
    {
      label: language === 'zh' ? '最近 200 周期' : 'Last 200',
      value: 200,
    },
    {
      label: language === 'zh' ? '最近 500 周期' : 'Last 500',
      value: 500,
    },
    {
      label: language === 'zh' ? '最近 1000 周期' : 'Last 1K',
      value: 1000,
    },
    {
      label: language === 'zh' ? '最近 2000 周期' : 'Last 2K',
      value: 2000,
    },
    {
      label: language === 'zh' ? '全部数据' : 'All Data',
      value: 'all',
    },
  ]
  const displayHistory =
    range === 'all' ? validHistory : validHistory.slice(-range)

  // 计算初始余额（优先从 account 获取配置的初始余额，备选从历史数据反推）
  const initialBalance =
    account?.initial_balance || // 从交易员配置读取真实初始余额
    (validHistory[0]
      ? validHistory[0].total_equity - validHistory[0].pnl
      : undefined) || // 备选：淨值 - 盈亏
    1000 // 默认值（与创建交易员时的默认配置一致）

  // 转换数据格式
  const chartData = displayHistory.map((point, index) => {
    const pnl = point.total_equity - initialBalance
    const pnlPct = ((pnl / initialBalance) * 100).toFixed(2)
    return {
      time: new Date(point.timestamp).toLocaleTimeString('zh-CN', {
        hour: '2-digit',
        minute: '2-digit',
      }),
      value: displayMode === 'dollar' ? point.total_equity : parseFloat(pnlPct),
      cycle: point.cycle_number ?? index + 1,
      raw_equity: point.total_equity,
      raw_pnl: pnl,
      raw_pnl_pct: parseFloat(pnlPct),
    }
  })

  const currentValue = chartData[chartData.length - 1]
  const isProfit = currentValue.raw_pnl >= 0

  // 计算Y轴范围
  const calculateYDomain = () => {
    if (displayMode === 'percent') {
      // 百分比模式：找到最大最小值，留20%余量
      const values = chartData.map((d) => d.value)
      const minVal = Math.min(...values)
      const maxVal = Math.max(...values)
      const range = Math.max(Math.abs(maxVal), Math.abs(minVal))
      const padding = Math.max(range * 0.2, 1) // 至少留1%余量
      return [Math.floor(minVal - padding), Math.ceil(maxVal + padding)]
    } else {
      // 美元模式：以初始余额为基准，上下留10%余量
      const values = chartData.map((d) => d.value)
      const minVal = Math.min(...values, initialBalance)
      const maxVal = Math.max(...values, initialBalance)
      const range = maxVal - minVal
      const padding = Math.max(range * 0.15, initialBalance * 0.01) // 至少留1%余量
      return [Math.floor(minVal - padding), Math.ceil(maxVal + padding)]
    }
  }

  // 自定义Tooltip - Binance Style
  const CustomTooltip = ({ active, payload }: any) => {
    if (active && payload && payload.length) {
      const data = payload[0].payload
      return (
        <div className="rounded-md border border-line bg-surface p-2.5 shadow-pop">
          <div className="mb-1 text-xs text-fg-3">
            Cycle #{data.cycle != null ? data.cycle : '—'}
          </div>
          <div className="num font-semibold text-fg">
            {data.raw_equity.toFixed(2)} USDT
          </div>
          <div
            className={cn(
              'num text-sm font-semibold',
              data.raw_pnl >= 0 ? 'text-up' : 'text-down'
            )}
          >
            {formatSigned(data.raw_pnl, { suffix: ' USDT' })} (
            {formatSigned(data.raw_pnl_pct, { suffix: '%' })})
          </div>
        </div>
      )
    }
    return null
  }

  return frame(
    <>
      {/* Header */}
      <div className="mb-3 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
        <div className="min-w-0 flex-1">
          {!embedded && (
            <h3 className="mb-1.5 text-sm font-semibold text-fg sm:text-base">
              {t('accountEquityCurve', language)}
            </h3>
          )}
          <div className="flex flex-col gap-1.5 sm:flex-row sm:items-baseline sm:gap-3">
            <span className="num text-2xl font-semibold text-fg sm:text-[28px]">
              {account?.total_equity.toFixed(2) || '0.00'}
              <span className="ml-1 text-sm font-normal text-fg-3">USDT</span>
            </span>
            <div className="flex flex-wrap items-center gap-2">
              <span
                className={cn(
                  'num inline-flex items-center gap-1 rounded px-2 py-0.5 text-sm font-semibold',
                  isProfit ? 'bg-up-soft text-up' : 'bg-down-soft text-down'
                )}
              >
                {isProfit ? (
                  <ArrowUp className="h-3.5 w-3.5" />
                ) : (
                  <ArrowDown className="h-3.5 w-3.5" />
                )}
                {formatSigned(currentValue.raw_pnl_pct, { suffix: '%' })}
              </span>
              <span className="num text-xs text-fg-3">
                ({formatSigned(currentValue.raw_pnl, { suffix: ' USDT' })})
              </span>
            </div>
          </div>
        </div>

        {/* Display Mode Toggle */}
        <Segmented
          value={displayMode}
          onChange={setDisplayMode}
          className="self-start sm:self-auto"
          items={[
            {
              key: 'dollar',
              label: 'USDT',
              icon: <DollarSign className="h-3.5 w-3.5" />,
            },
            {
              key: 'percent',
              label: <Percent className="h-3.5 w-3.5" />,
            },
          ]}
        />

        {/* Time Range Selector（下拉：支持比预设更短的周期范围） */}
        <NofxSelect
          value={String(range)}
          onChange={(val) => setRange(val === 'all' ? 'all' : Number(val))}
          options={RANGE_OPTIONS.map((opt) => ({
            value: String(opt.value),
            label: opt.label,
          }))}
          className="h-8 min-w-[120px] self-start rounded-md border border-line bg-surface-2 px-2.5 text-xs font-semibold text-fg transition-colors hover:border-line-strong sm:self-auto"
        />
      </div>

      {/* Chart */}
      <div className="relative my-2 overflow-hidden rounded-md">
        {/* NOFX Watermark */}
        <div className="pointer-events-none absolute right-4 top-4 z-10 font-num text-xl font-bold text-brand/15">
          NOFX
        </div>
        <ResponsiveContainer width="100%" height={280}>
          <LineChart
            data={chartData}
            margin={{ top: 10, right: 20, left: 5, bottom: 30 }}
          >
            <defs>
              <linearGradient id="colorGradient" x1="0" y1="0" x2="0" y2="1">
                <stop offset="5%" stopColor="var(--brand)" stopOpacity={0.8} />
                <stop offset="95%" stopColor="var(--brand)" stopOpacity={0.2} />
              </linearGradient>
            </defs>
            <CartesianGrid
              strokeDasharray="3 3"
              stroke="var(--line)"
              strokeOpacity={0.6}
            />
            <XAxis
              dataKey="time"
              stroke="var(--line)"
              tick={{ fill: 'var(--fg-3)', fontSize: 11 }}
              tickLine={{ stroke: 'var(--line)' }}
              interval={Math.floor(chartData.length / 10)}
              angle={-15}
              textAnchor="end"
              height={60}
            />
            <YAxis
              stroke="var(--line)"
              tick={{ fill: 'var(--fg-3)', fontSize: 12 }}
              tickLine={{ stroke: 'var(--line)' }}
              domain={calculateYDomain()}
              tickFormatter={(value) =>
                displayMode === 'dollar' ? `$${value.toFixed(0)}` : `${value}%`
              }
            />
            <Tooltip
              content={<CustomTooltip />}
              cursor={{ stroke: 'var(--line-strong)', strokeDasharray: '3 3' }}
            />
            <ReferenceLine
              y={displayMode === 'dollar' ? initialBalance : 0}
              stroke="var(--line-strong)"
              strokeDasharray="3 3"
              label={{
                value:
                  displayMode === 'dollar'
                    ? t('initialBalance', language).split(' ')[0]
                    : '0%',
                fill: 'var(--fg-3)',
                fontSize: 12,
              }}
            />
            <Line
              type="natural"
              dataKey="value"
              stroke="url(#colorGradient)"
              strokeWidth={3}
              dot={
                chartData.length > 50 ? false : { fill: 'var(--brand)', r: 3 }
              }
              activeDot={{
                r: 6,
                fill: 'var(--brand)',
                stroke: 'var(--brand)',
                strokeWidth: 2,
              }}
              connectNulls={true}
            />
          </LineChart>
        </ResponsiveContainer>
      </div>

      {/* Footer Stats */}
      <div className="mt-3 grid grid-cols-2 gap-2 border-t border-line pt-3 sm:grid-cols-4">
        {[
          {
            label: t('initialBalance', language),
            value: `${initialBalance.toFixed(2)} USDT`,
          },
          {
            label: t('currentEquity', language),
            value: `${currentValue.raw_equity.toFixed(2)} USDT`,
          },
          {
            label: t('historicalCycles', language),
            value: `${validHistory.length} ${t('cycles', language)}`,
          },
          {
            label: t('displayRange', language),
            value:
              range === 'all'
                ? t('allData', language)
                : `${t('recent', language)} ${range}`,
          },
        ].map((item) => (
          <div key={item.label} className="rounded-md bg-surface-2 p-2">
            <div className="text-[11px] uppercase tracking-wider text-fg-3">
              {item.label}
            </div>
            <div className="num mt-0.5 text-xs font-semibold text-fg sm:text-sm">
              {item.value}
            </div>
          </div>
        ))}
      </div>
    </>,
    true
  )
}
