import { useState } from 'react'
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

export function EquityChart({ traderId, embedded = false }: EquityChartProps) {
  const { language } = useLanguage()
  const { user, token } = useAuth()
  const [displayMode, setDisplayMode] = useState<'dollar' | 'percent'>('dollar')
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

  // Loading state - show skeleton
  if (isLoading) {
    return (
      <div className={embedded ? 'p-6' : 'binance-card p-6'}>
        {!embedded && (
          <h3
            className="text-lg font-semibold mb-6"
            style={{ color: 'var(--fg)' }}
          >
            {t('accountEquityCurve', language)}
          </h3>
        )}
        <div className="animate-pulse">
          <div className="skeleton h-64 w-full rounded"></div>
        </div>
      </div>
    )
  }

  if (error) {
    return (
      <div className={embedded ? 'p-6' : 'binance-card p-6'}>
        <div
          className="flex items-center gap-3 p-4 rounded"
          style={{
            background: 'var(--down-soft)',
            border:
              '1px solid color-mix(in srgb, var(--down) 20%, transparent)',
          }}
        >
          <AlertTriangle className="w-6 h-6" style={{ color: 'var(--down)' }} />
          <div>
            <div className="font-semibold" style={{ color: 'var(--down)' }}>
              {t('loadingError', language)}
            </div>
            <div className="text-sm" style={{ color: 'var(--fg-3)' }}>
              {error.message}
            </div>
          </div>
        </div>
      </div>
    )
  }

  // 过滤掉无效数据：total_equity为0或小于1的数据点（API失败导致）
  const validHistory = history?.filter((point) => point.total_equity > 1) || []

  if (!validHistory || validHistory.length === 0) {
    return (
      <div className={embedded ? 'p-6' : 'binance-card p-6'}>
        {!embedded && (
          <h3
            className="text-lg font-semibold mb-6"
            style={{ color: 'var(--fg)' }}
          >
            {t('accountEquityCurve', language)}
          </h3>
        )}
        <div className="text-center py-16" style={{ color: 'var(--fg-3)' }}>
          <div className="mb-4 flex justify-center opacity-50">
            <BarChart3 className="w-16 h-16" />
          </div>
          <div className="text-lg font-semibold mb-2">
            {t('noHistoricalData', language)}
          </div>
          <div className="text-sm">{t('dataWillAppear', language)}</div>
        </div>
      </div>
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
        <div
          className="rounded p-3 shadow-xl"
          style={{
            background: 'var(--surface-hover)',
            border: '1px solid var(--line)',
          }}
        >
          <div className="text-xs mb-1" style={{ color: 'var(--fg-3)' }}>
            Cycle #{data.cycle != null ? data.cycle : '—'}
          </div>
          <div className="font-bold mono" style={{ color: 'var(--fg)' }}>
            {data.raw_equity.toFixed(2)} USDT
          </div>
          <div
            className="text-sm mono font-bold"
            style={{ color: data.raw_pnl >= 0 ? 'var(--up)' : 'var(--down)' }}
          >
            {data.raw_pnl >= 0 ? '+' : ''}
            {data.raw_pnl.toFixed(2)} USDT ({data.raw_pnl_pct >= 0 ? '+' : ''}
            {data.raw_pnl_pct}%)
          </div>
        </div>
      )
    }
    return null
  }

  return (
    <div
      className={
        embedded ? 'p-3 sm:p-5' : 'binance-card p-3 sm:p-5 animate-fade-in'
      }
    >
      {/* Header */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between mb-4">
        <div className="flex-1">
          {!embedded && (
            <h3
              className="text-base sm:text-lg font-bold mb-2"
              style={{ color: 'var(--fg)' }}
            >
              {t('accountEquityCurve', language)}
            </h3>
          )}
          <div className="flex flex-col sm:flex-row sm:items-baseline gap-2 sm:gap-4">
            <span
              className="text-2xl sm:text-3xl font-bold mono"
              style={{ color: 'var(--fg)' }}
            >
              {account?.total_equity.toFixed(2) || '0.00'}
              <span
                className="text-base sm:text-lg ml-1"
                style={{ color: 'var(--fg-3)' }}
              >
                USDT
              </span>
            </span>
            <div className="flex items-center gap-2 flex-wrap">
              <span
                className="text-sm sm:text-lg font-bold mono px-2 sm:px-3 py-1 rounded flex items-center gap-1"
                style={{
                  color: isProfit ? 'var(--up)' : 'var(--down)',
                  background: isProfit ? 'var(--up-soft)' : 'var(--down-soft)',
                  border: `1px solid ${
                    isProfit
                      ? 'color-mix(in srgb, var(--up) 20%, transparent)'
                      : 'color-mix(in srgb, var(--down) 20%, transparent)'
                  }`,
                }}
              >
                {isProfit ? (
                  <ArrowUp className="w-4 h-4" />
                ) : (
                  <ArrowDown className="w-4 h-4" />
                )}
                {isProfit ? '+' : ''}
                {currentValue.raw_pnl_pct}%
              </span>
              <span
                className="text-xs sm:text-sm mono"
                style={{ color: 'var(--fg-3)' }}
              >
                ({isProfit ? '+' : ''}
                {currentValue.raw_pnl.toFixed(2)} USDT)
              </span>
            </div>
          </div>
        </div>

        {/* Display Mode Toggle */}
        <div
          className="flex gap-0.5 sm:gap-1 rounded p-0.5 sm:p-1 self-start sm:self-auto"
          style={{
            background: 'var(--surface-2)',
            border: '1px solid var(--line)',
          }}
        >
          <button
            onClick={() => setDisplayMode('dollar')}
            className="px-3 sm:px-4 py-1.5 sm:py-2 rounded text-xs sm:text-sm font-bold transition-all flex items-center gap-1"
            style={
              displayMode === 'dollar'
                ? {
                    background: 'var(--brand)',
                    color: 'var(--brand-fg)',
                    boxShadow:
                      '0 2px 8px color-mix(in srgb, var(--brand) 40%, transparent)',
                  }
                : { background: 'transparent', color: 'var(--fg-3)' }
            }
          >
            <DollarSign className="w-4 h-4" /> USDT
          </button>
          <button
            onClick={() => setDisplayMode('percent')}
            className="px-3 sm:px-4 py-1.5 sm:py-2 rounded text-xs sm:text-sm font-bold transition-all flex items-center gap-1"
            style={
              displayMode === 'percent'
                ? {
                    background: 'var(--brand)',
                    color: 'var(--brand-fg)',
                    boxShadow:
                      '0 2px 8px color-mix(in srgb, var(--brand) 40%, transparent)',
                  }
                : { background: 'transparent', color: 'var(--fg-3)' }
            }
          >
            <Percent className="w-4 h-4" />
          </button>
        </div>

        {/* Time Range Selector（下拉：支持比预设更短的周期范围） */}
        <select
          value={String(range)}
          onChange={(e) =>
            setRange(e.target.value === 'all' ? 'all' : Number(e.target.value))
          }
          className="px-3 py-2 rounded text-xs sm:text-sm font-bold self-start sm:self-auto"
          style={{
            background: 'var(--surface-2)',
            border: '1px solid var(--line)',
            color: 'var(--fg)',
          }}
          title={
            language === 'zh' ? '显示范围（周期数）' : 'Display range (cycles)'
          }
        >
          {RANGE_OPTIONS.map((opt) => (
            <option key={String(opt.value)} value={String(opt.value)}>
              {opt.label}
            </option>
          ))}
        </select>
      </div>

      {/* Chart */}
      <div
        className="my-2"
        style={{
          borderRadius: '8px',
          overflow: 'hidden',
          position: 'relative',
        }}
      >
        {/* NOFX Watermark */}
        <div
          style={{
            position: 'absolute',
            top: '15px',
            right: '15px',
            fontSize: '20px',
            fontWeight: 'bold',
            color: 'color-mix(in srgb, var(--brand) 15%, transparent)',
            zIndex: 10,
            pointerEvents: 'none',
            fontFamily: 'monospace',
          }}
        >
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
            <CartesianGrid strokeDasharray="3 3" stroke="var(--line)" />
            <XAxis
              dataKey="time"
              stroke="var(--fg-3)"
              tick={{ fill: 'var(--fg-3)', fontSize: 11 }}
              tickLine={{ stroke: 'var(--line)' }}
              interval={Math.floor(chartData.length / 10)}
              angle={-15}
              textAnchor="end"
              height={60}
            />
            <YAxis
              stroke="var(--fg-3)"
              tick={{ fill: 'var(--fg-3)', fontSize: 12 }}
              tickLine={{ stroke: 'var(--line)' }}
              domain={calculateYDomain()}
              tickFormatter={(value) =>
                displayMode === 'dollar' ? `$${value.toFixed(0)}` : `${value}%`
              }
            />
            <Tooltip content={<CustomTooltip />} />
            <ReferenceLine
              y={displayMode === 'dollar' ? initialBalance : 0}
              stroke="var(--line)"
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
      <div
        className="mt-3 grid grid-cols-2 sm:grid-cols-4 gap-2 sm:gap-3 pt-3"
        style={{ borderTop: '1px solid var(--line)' }}
      >
        <div
          className="p-2 rounded transition-all hover:bg-opacity-50"
          style={{
            background: 'color-mix(in srgb, var(--brand) 5%, transparent)',
          }}
        >
          <div
            className="text-xs mb-1 uppercase tracking-wider"
            style={{ color: 'var(--fg-3)' }}
          >
            {t('initialBalance', language)}
          </div>
          <div
            className="text-xs sm:text-sm font-bold mono"
            style={{ color: 'var(--fg)' }}
          >
            {initialBalance.toFixed(2)} USDT
          </div>
        </div>
        <div
          className="p-2 rounded transition-all hover:bg-opacity-50"
          style={{
            background: 'color-mix(in srgb, var(--brand) 5%, transparent)',
          }}
        >
          <div
            className="text-xs mb-1 uppercase tracking-wider"
            style={{ color: 'var(--fg-3)' }}
          >
            {t('currentEquity', language)}
          </div>
          <div
            className="text-xs sm:text-sm font-bold mono"
            style={{ color: 'var(--fg)' }}
          >
            {currentValue.raw_equity.toFixed(2)} USDT
          </div>
        </div>
        <div
          className="p-2 rounded transition-all hover:bg-opacity-50"
          style={{
            background: 'color-mix(in srgb, var(--brand) 5%, transparent)',
          }}
        >
          <div
            className="text-xs mb-1 uppercase tracking-wider"
            style={{ color: 'var(--fg-3)' }}
          >
            {t('historicalCycles', language)}
          </div>
          <div
            className="text-xs sm:text-sm font-bold mono"
            style={{ color: 'var(--fg)' }}
          >
            {validHistory.length} {t('cycles', language)}
          </div>
        </div>
        <div
          className="p-2 rounded transition-all hover:bg-opacity-50"
          style={{
            background: 'color-mix(in srgb, var(--brand) 5%, transparent)',
          }}
        >
          <div
            className="text-xs mb-1 uppercase tracking-wider"
            style={{ color: 'var(--fg-3)' }}
          >
            {t('displayRange', language)}
          </div>
          <div
            className="text-xs sm:text-sm font-bold mono"
            style={{ color: 'var(--fg)' }}
          >
            {range === 'all'
              ? t('allData', language)
              : `${t('recent', language)} ${range}`}
          </div>
        </div>
      </div>
    </div>
  )
}
