import { useEffect, useState } from 'react'
import { api } from '../../lib/api'
import type { NetTraffic } from '../../lib/api/system'
import { Badge } from '../ui'
import { RefreshCw } from 'lucide-react'

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${i === 0 || v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

function formatUptime(sinceUnix: number): string {
  const ms = Date.now() - sinceUnix * 1000
  if (!Number.isFinite(sinceUnix) || ms < 0) return '—'
  const m = Math.floor(ms / 60000)
  const d = Math.floor(m / 1440)
  const h = Math.floor((m % 1440) / 60)
  if (d > 0) return `${d}d ${h}h ${m % 60}m`
  if (h > 0) return `${h}h ${m % 60}m`
  return `${m}m`
}

// 运行期累计网络流量监控（设置页 Account 标签）：进程启动以来的 HTTP 载荷
// 字节（按主机分列）+ WebSocket 线路字节。进程重启后清零。
export function NetworkTrafficCard({ language }: { language: string }) {
  const zh = language === 'zh'
  const [data, setData] = useState<NetTraffic | null>(null)
  const [error, setError] = useState('')
  const [updatedAt, setUpdatedAt] = useState(0)

  // 轮询失败不降级为永久停止：保留错误提示，下个周期继续重试。
  useEffect(() => {
    let cancelled = false
    const load = () => {
      api
        .getNetTraffic()
        .then((d) => {
          if (cancelled) return
          setData(d)
          setError('')
          setUpdatedAt(Date.now())
        })
        .catch(() => {
          if (!cancelled)
            setError(zh ? '暂无法获取流量数据' : 'Traffic data unavailable')
        })
    }
    load()
    const timer = window.setInterval(load, 5000)
    return () => {
      cancelled = true
      window.clearInterval(timer)
    }
  }, [zh])

  const httpUp = data?.http_up ?? 0
  const httpDown = data?.http_down ?? 0
  const wsBytes = (data?.ws_up ?? 0) + (data?.ws_down ?? 0)
  const hosts = data?.hosts ?? []

  const stats: { label: string; value: string; sub?: string }[] = [
    {
      label: zh ? '上行（HTTP 载荷）' : 'Up (HTTP payload)',
      value: formatBytes(httpUp),
    },
    {
      label: zh ? '下行（HTTP 载荷）' : 'Down (HTTP payload)',
      value: formatBytes(httpDown),
    },
    {
      label: zh ? 'WebSocket（线路字节）' : 'WebSocket (wire)',
      value: formatBytes(wsBytes),
    },
    {
      label: zh ? 'HTTP 请求' : 'HTTP requests',
      value: (data?.requests ?? 0).toLocaleString(),
    },
  ]

  return (
    <div className="border-t border-line pt-4">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h3 className="text-sm font-semibold text-fg">
            {zh ? '网络流量' : 'Network Traffic'}
          </h3>
          <p className="mt-1 text-xs text-fg-3">
            {zh
              ? '进程本次启动以来的累计出网流量，重启后清零。'
              : 'Cumulative outbound traffic since this process started. Resets on restart.'}
          </p>
        </div>
        <Badge variant="neutral" className="shrink-0">
          <RefreshCw size={11} className="mr-1" />
          {data ? formatUptime(data.since_unix) : '—'}
        </Badge>
      </div>

      {error && <p className="mt-2 text-xs text-fg-3">{error}</p>}

      <div className="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-4">
        {stats.map((s) => (
          <div
            key={s.label}
            className="rounded-md border border-line bg-surface-2 px-3 py-2"
          >
            <div className="text-[15px] font-semibold tabular-nums text-fg">
              {s.value}
            </div>
            <div className="mt-0.5 text-[11px] leading-tight text-fg-3">
              {s.label}
            </div>
          </div>
        ))}
      </div>

      {hosts.length > 0 && (
        <div className="mt-3 overflow-hidden rounded-md border border-line">
          <div className="grid grid-cols-[1fr_auto_auto_auto] gap-2 border-b border-line bg-surface-2 px-3 py-1.5 text-[11px] font-medium text-fg-3">
            <span>{zh ? '主机' : 'Host'}</span>
            <span className="w-14 text-right">{zh ? '请求' : 'Reqs'}</span>
            <span className="w-16 text-right">{zh ? '上行' : 'Up'}</span>
            <span className="w-16 text-right">{zh ? '下行' : 'Down'}</span>
          </div>
          {hosts.slice(0, 8).map((h) => (
            <div
              key={h.host}
              className="grid grid-cols-[1fr_auto_auto_auto] items-center gap-2 border-b border-line bg-bg px-3 py-1.5 text-xs last:border-b-0"
            >
              <span className="truncate text-fg" title={h.host}>
                {h.host}
              </span>
              <span className="w-14 text-right tabular-nums text-fg-3">
                {h.requests.toLocaleString()}
              </span>
              <span className="w-16 text-right tabular-nums text-fg-3">
                {formatBytes(h.up)}
              </span>
              <span className="w-16 text-right tabular-nums text-fg-2">
                {formatBytes(h.down)}
              </span>
            </div>
          ))}
        </div>
      )}

      <p className="mt-2 text-[11px] leading-relaxed text-fg-3">
        {zh
          ? '口径：HTTP 上行为请求行+请求体、下行为响应体（不含 TLS 与头部开销，实际代理流量略高）；WebSocket 为含加密帧的线路字节。'
          : 'HTTP up = request line + body, down = response body (TLS & header overhead excluded — actual proxy usage is slightly higher); WebSocket numbers are wire bytes including encryption.'}
        {updatedAt > 0 && ` ${zh ? '每 5 秒刷新' : 'Refreshed every 5s'}.`}
      </p>
    </div>
  )
}
