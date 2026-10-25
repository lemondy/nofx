import { useEffect, useState } from 'react'
import { AlertTriangle, CandlestickChart, CircleCheck } from 'lucide-react'
import { httpClient } from '../../lib/httpClient'
import { Card, CardHeader, Stat } from '../ui'

interface ChartWithOrdersSimpleProps {
  symbol: string
  interval?: string
  traderID?: string
  height?: number
}

export function ChartWithOrdersSimple({
  symbol = 'BTCUSDT',
  interval = '5m',
  traderID,
  height = 500,
}: ChartWithOrdersSimpleProps) {
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [klineCount, setKlineCount] = useState(0)
  const [orderCount, setOrderCount] = useState(0)

  useEffect(() => {
    const loadData = async () => {
      console.log(
        '[ChartSimple] Loading data for',
        symbol,
        interval,
        'trader:',
        traderID
      )
      setLoading(true)
      setError(null)

      try {
        // 从我们自己的服务获取K线数据
        const limit = 100
        const klineUrl = `/api/klines?symbol=${symbol}&interval=${interval}&limit=${limit}`

        console.log('[ChartSimple] Fetching klines from our service:', klineUrl)
        const klineResult = await httpClient.request(klineUrl, { silent: true })

        if (!klineResult.success || !klineResult.data) {
          throw new Error('Failed to fetch klines from our service')
        }

        console.log('[ChartSimple] Received klines:', klineResult.data.length)
        setKlineCount(klineResult.data.length)

        // 测试获取订单数据
        if (traderID) {
          const tradesUrl = `/api/trades?trader_id=${traderID}&symbol=${symbol}&limit=100`
          console.log('[ChartSimple] Fetching trades from:', tradesUrl)
          const tradesResult = await httpClient.request(tradesUrl, {
            silent: true,
          })

          if (tradesResult.success && tradesResult.data) {
            console.log(
              '[ChartSimple] Received trades:',
              tradesResult.data.length
            )
            setOrderCount(tradesResult.data.length)
          } else {
            console.warn(
              '[ChartSimple] Failed to fetch trades:',
              tradesResult.message || 'Unknown error',
              tradesResult
            )
          }
        }

        setLoading(false)
      } catch (err: any) {
        console.error('[ChartSimple] Error:', err)
        setError(err.message || 'Failed to load data')
        setLoading(false)
      }
    }

    loadData()
  }, [symbol, interval, traderID])

  return (
    <Card className="relative overflow-hidden" style={{ minHeight: height }}>
      {/* 标题栏 */}
      <CardHeader
        title={
          <span className="flex items-center gap-2">
            <CandlestickChart className="h-4 w-4 shrink-0 text-fg-3" />
            <span className="num">
              {symbol} {interval} (测试模式)
            </span>
          </span>
        }
        actions={
          loading ? (
            <span className="text-xs text-fg-3">加载中...</span>
          ) : undefined
        }
      />

      {/* 测试信息 */}
      <div className="space-y-3 p-4">
        {error ? (
          <div className="flex flex-col items-center py-6 text-center">
            <AlertTriangle className="mb-2 h-6 w-6 text-down" />
            <div className="text-sm text-down">{error}</div>
          </div>
        ) : (
          <>
            <div className="rounded-md border border-line bg-surface-2 p-3">
              <Stat
                label="币安K线数据"
                value={`${klineCount} 根K线`}
                tone="up"
                size="lg"
              />
            </div>

            {traderID && (
              <div className="rounded-md border border-line bg-surface-2 p-3">
                <Stat
                  label="历史订单数据"
                  value={`${orderCount} 笔订单`}
                  size="lg"
                />
              </div>
            )}

            <div className="rounded-md border border-line bg-surface-2 p-3">
              <div className="text-xs text-fg-3">状态</div>
              <div className="mt-0.5 flex items-center gap-2 text-sm text-fg">
                <CircleCheck className="h-4 w-4 shrink-0 text-up" />
                数据获取正常，图表组件开发中
              </div>
            </div>
          </>
        )}
      </div>
    </Card>
  )
}
