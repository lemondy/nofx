import { useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { SystemQuality } from '../lib/api/system'
import type { TraderInfo } from '../types'

const C = {
  bg: '#F2EFE6',
  card: '#ECE8DB',
  border: '#C0B9A2',
  inset: '#F2EFE6',
  text: '#1E1E1A',
  muted: '#6E6E60',
  up: '#2E7D4F',
  down: '#C0392B',
  gold: '#B8912A',
}

const L = (zh: string, en: string, language: string) => (language === 'zh' ? zh : en)

const FAILURE_LABELS: Record<string, { zh: string; en: string }> = {
  AI_CALL: { zh: 'AI 调用失败', en: 'AI call' },
  NETWORK: { zh: '网络/超时', en: 'Network/timeout' },
  EXCHANGE: { zh: '交易所接口', en: 'Exchange API' },
  AUTH: { zh: '鉴权/IP 白名单', en: 'Auth/IP whitelist' },
  DATA: { zh: '行情数据', en: 'Market data' },
  OTHER: { zh: '其他', en: 'Other' },
}

function failureLabel(cat: string, language: string) {
  const def = FAILURE_LABELS[cat]
  if (!def) return cat
  return language === 'zh' ? def.zh : def.en
}

function fmtMs(ms?: number) {
  if (ms === undefined || ms <= 0) return '--'
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const m = Math.floor(ms / 60_000)
  const s = Math.round((ms % 60_000) / 1000)
  return `${m}m${s.toString().padStart(2, '0')}s`
}

export default function SystemQualityPage({ language }: { language: string }) {
  const [traders, setTraders] = useState<TraderInfo[]>([])
  const [selectedTraderId, setSelectedTraderId] = useState<string | undefined>(() => {
    return new URLSearchParams(window.location.search).get('trader') || undefined
  })
  const [hours, setHours] = useState(24)
  const [quality, setQuality] = useState<SystemQuality | null>(null)
  const [failed, setFailed] = useState(false)
  const [loading, setLoading] = useState(true)
  // Polling resilience: back off on failure, never latch a permanent poll-off
  // (the decisions-panel freeze lesson). Consecutive failures stretch the
  // interval; any success resets it.
  const backoffRef = useRef(0)

  useEffect(() => {
    api
      .getTraders(true)
      .then((list) => {
        setTraders(list)
        if (!selectedTraderId && list.length > 0) {
          setSelectedTraderId(list[0].trader_id)
        }
      })
      .catch(() => setTraders([]))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    if (!selectedTraderId) return
    let alive = true
    let timer: number | undefined

    const tick = async () => {
      try {
        const data = await api.getSystemQuality(selectedTraderId, hours)
        if (!alive) return
        setQuality(data)
        setFailed(false)
        setLoading(false)
        backoffRef.current = 0
      } catch {
        if (!alive) return
        setFailed(true)
        setLoading(false)
        backoffRef.current = Math.min(backoffRef.current + 1, 4)
      } finally {
        if (alive) {
          const interval = 15_000 + backoffRef.current * 20_000
          timer = window.setTimeout(tick, interval)
        }
      }
    }
    tick()
    return () => {
      alive = false
      if (timer) window.clearTimeout(timer)
    }
  }, [selectedTraderId, hours])

  const ai = quality?.ai_calls

  return (
    <div className="min-h-screen" style={{ background: C.bg, color: C.text }}>
      <div className="max-w-[1440px] mx-auto px-6 py-6">
        {/* Header */}
        <div className="flex items-center justify-between flex-wrap gap-4 mb-5">
          <div>
            <h1 className="text-xl font-bold">{L('系统质量', 'System Quality', language)}</h1>
            <p className="text-xs mt-1" style={{ color: C.muted }}>
              {L(
                '每轮 AI 决策的耗时、成功率与服务自身失败分布——衡量系统本身的健康度与可改进空间',
                'Per-cycle AI-call latency, success rate and failure mix — how healthy the system itself is',
                language
              )}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-xs" style={{ color: C.muted }}>
              {L('交易员', 'Trader', language)}
            </span>
            <select
              value={selectedTraderId || ''}
              onChange={(e) => setSelectedTraderId(e.target.value)}
              className="text-xs rounded px-3 py-2 outline-none"
              style={{ background: C.card, border: `1px solid ${C.border}`, color: C.text }}
            >
              {traders.length === 0 && <option value="">--</option>}
              {traders.map((tr) => (
                <option key={tr.trader_id} value={tr.trader_id}>
                  {tr.trader_name}
                </option>
              ))}
            </select>
            <select
              value={hours}
              onChange={(e) => setHours(Number(e.target.value))}
              className="text-xs rounded px-3 py-2 outline-none"
              style={{ background: C.card, border: `1px solid ${C.border}`, color: C.text }}
            >
              <option value={24}>{L('近 24 小时', 'Last 24h', language)}</option>
              <option value={72}>{L('近 3 天', 'Last 3 days', language)}</option>
              <option value={168}>{L('近 7 天', 'Last 7 days', language)}</option>
            </select>
          </div>
        </div>

        {loading && !quality ? (
          <div className="py-24 text-center text-sm" style={{ color: C.muted }}>
            {L('加载中', 'Loading', language)}...
          </div>
        ) : failed && !quality ? (
          <div className="py-24 text-center text-sm" style={{ color: C.muted }}>
            {L('数据加载失败，正在自动重试…', 'Load failed — retrying automatically…', language)}
          </div>
        ) : !quality || quality.total_cycles === 0 ? (
          <div className="py-24 text-center text-sm" style={{ color: C.muted }}>
            {L('该时间窗口内没有决策周期', 'No decision cycles in this window', language)}
          </div>
        ) : (
          <>
            {/* Stat cards */}
            <div className="grid grid-cols-2 lg:grid-cols-5 gap-4 mb-6">
              <StatCard
                label={L('周期成功率', 'Cycle success rate', language)}
                value={`${quality.success_rate_pct.toFixed(1)}%`}
                sub={`${quality.successful_cycles}/${quality.total_cycles} ${L('个周期', 'cycles', language)}`}
                color={quality.success_rate_pct >= 95 ? C.up : quality.success_rate_pct >= 85 ? C.gold : C.down}
                language={language}
              />
              <StatCard
                label={L('AI 平均耗时', 'AI avg latency', language)}
                value={fmtMs(ai?.avg_ms)}
                sub={L('每次大模型调用', 'per model call', language)}
                language={language}
              />
              <StatCard
                label={L('AI P50 / P95', 'AI P50 / P95', language)}
                value={`${fmtMs(ai?.p50_ms)} / ${fmtMs(ai?.p95_ms)}`}
                sub={L('中位数与长尾', 'median vs tail', language)}
                language={language}
              />
              <StatCard
                label={L('AI 最长耗时', 'AI max latency', language)}
                value={fmtMs(ai?.max_ms)}
                sub={L('窗口内最慢一次', 'slowest call in window', language)}
                color={C.gold}
                language={language}
              />
              <StatCard
                label={L('失败周期', 'Failed cycles', language)}
                value={String(quality.failed_cycles)}
                sub={L('含执行与 AI 调用失败', 'execution + AI failures', language)}
                color={quality.failed_cycles === 0 ? C.up : C.down}
                language={language}
              />
            </div>

            <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
              {/* Failure breakdown */}
              <div className="rounded-lg p-5" style={{ background: C.card, border: `1px solid ${C.border}` }}>
                <h2 className="text-sm font-bold mb-4 uppercase tracking-wide">
                  {L('失败分类', 'Failure breakdown', language)}
                </h2>
                {quality.failures.length === 0 ? (
                  <div className="py-8 text-center text-xs" style={{ color: C.muted }}>
                    ✅ {L('窗口内无失败周期', 'No failed cycles in this window', language)}
                  </div>
                ) : (
                  <table className="w-full rank-table text-xs">
                    <thead>
                      <tr>
                        <th className="text-left pb-2 font-semibold" style={{ color: C.muted }}>
                          {L('类别', 'Category', language)}
                        </th>
                        <th className="text-right pb-2 font-semibold" style={{ color: C.muted }}>
                          {L('次数', 'Count', language)}
                        </th>
                        <th className="text-left pb-2 pl-4 font-semibold" style={{ color: C.muted }}>
                          {L('最近示例', 'Latest sample', language)}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {quality.failures.map((f) => (
                        <tr key={f.category}>
                          <td className="py-2.5 font-semibold" style={{ color: C.down }}>
                            {failureLabel(f.category, language)}
                          </td>
                          <td className="py-2.5 text-right font-mono">{f.count}</td>
                          <td
                            className="py-2.5 pl-4 font-mono max-w-[280px] truncate"
                            title={f.sample}
                            style={{ color: C.muted }}
                          >
                            {f.sample || '--'}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </div>

              {/* Hourly table */}
              <div className="rounded-lg p-5" style={{ background: C.card, border: `1px solid ${C.border}` }}>
                <h2 className="text-sm font-bold mb-4 uppercase tracking-wide">
                  {L('逐小时概览', 'Hourly overview', language)}
                </h2>
                <div className="max-h-[360px] overflow-y-auto custom-scrollbar">
                  <table className="w-full rank-table text-xs">
                    <thead>
                      <tr>
                        <th className="text-left pb-2 font-semibold" style={{ color: C.muted }}>
                          {L('小时', 'Hour', language)}
                        </th>
                        <th className="text-right pb-2 font-semibold" style={{ color: C.muted }}>
                          {L('周期', 'Cycles', language)}
                        </th>
                        <th className="text-right pb-2 font-semibold" style={{ color: C.muted }}>
                          {L('失败', 'Fail', language)}
                        </th>
                        <th className="text-right pb-2 font-semibold" style={{ color: C.muted }}>
                          {L('平均耗时', 'Avg latency', language)}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {[...quality.hourly].reverse().map((h) => (
                        <tr key={h.hour}>
                          <td className="py-2.5 font-mono">{h.hour}</td>
                          <td className="py-2.5 text-right font-mono">{h.cycles}</td>
                          <td
                            className="py-2.5 text-right font-mono"
                            style={{ color: h.failures > 0 ? C.down : C.muted }}
                          >
                            {h.failures}
                          </td>
                          <td className="py-2.5 text-right font-mono">{fmtMs(h.avg_duration_ms)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            </div>

            <p className="text-xs mt-4" style={{ color: C.muted }}>
              {L(
                '口径：耗时只统计成功发出并返回的大模型调用（ai_request_duration_ms）；成功率按决策周期计，一个周期内任一动作执行失败即计为失败。',
                'Scope: latency counts completed model calls only (ai_request_duration_ms); success rate is per decision cycle — any failed action marks the cycle failed.',
                language
              )}
            </p>
          </>
        )}
      </div>
    </div>
  )
}

function StatCard({
  label,
  value,
  sub,
  color,
  language,
}: {
  label: string
  value: string
  sub?: string
  color?: string
  language: string
}) {
  return (
    <div className="rounded-lg p-4" style={{ background: C.card, border: `1px solid ${C.border}` }}>
      <div className="text-[10px] font-mono uppercase tracking-wider mb-2" style={{ color: C.muted }}>
        {label}
      </div>
      <div className="text-xl font-bold font-mono tracking-tight" style={{ color: color || C.text }}>
        {value}
      </div>
      {sub && (
        <div className="text-[10px] mt-1.5 font-mono" style={{ color: C.muted }}>
          {language === 'zh' ? sub : sub}
        </div>
      )}
    </div>
  )
}
