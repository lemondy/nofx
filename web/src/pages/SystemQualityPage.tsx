import { useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { SystemQuality } from '../lib/api/system'
import type { TraderInfo } from '../types'
import { Badge, Card, CardHeader, EmptyState, Stat } from '../components/ui'

const SELECT_CLS =
  'h-8 rounded-md border border-line bg-surface-2 px-2 text-[13px] text-fg outline-none hover:border-line-strong focus:border-brand'
const TH_CLS =
  'h-8 bg-surface-2 px-3 py-0 text-xs font-medium text-fg-3 whitespace-nowrap'
const TR_CLS =
  'h-[34px] border-b border-line last:border-b-0 hover:bg-surface-hover'

const L = (zh: string, en: string, language: string) =>
  language === 'zh' ? zh : en

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
  const [selectedTraderId, setSelectedTraderId] = useState<string | undefined>(
    () => {
      return (
        new URLSearchParams(window.location.search).get('trader') || undefined
      )
    }
  )
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
    <div className="min-h-screen bg-bg text-fg">
      <div className="mx-auto max-w-[1440px] px-4 py-4 sm:px-6">
        {/* Header */}
        <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
          <div>
            <h1 className="text-xl font-semibold">
              {L('系统质量', 'System Quality', language)}
            </h1>
            <p className="mt-0.5 text-xs text-fg-3">
              {L(
                '每轮 AI 决策的耗时、成功率与服务自身失败分布——衡量系统本身的健康度与可改进空间',
                'Per-cycle AI-call latency, success rate and failure mix — how healthy the system itself is',
                language
              )}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-xs text-fg-3">
              {L('交易员', 'Trader', language)}
            </span>
            <select
              value={selectedTraderId || ''}
              onChange={(e) => setSelectedTraderId(e.target.value)}
              className={SELECT_CLS}
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
              className={SELECT_CLS}
            >
              <option value={24}>
                {L('近 24 小时', 'Last 24h', language)}
              </option>
              <option value={72}>
                {L('近 3 天', 'Last 3 days', language)}
              </option>
              <option value={168}>
                {L('近 7 天', 'Last 7 days', language)}
              </option>
            </select>
          </div>
        </div>

        {loading && !quality ? (
          <div className="py-16 text-center text-sm text-fg-3">
            {L('加载中', 'Loading', language)}...
          </div>
        ) : failed && !quality ? (
          <EmptyState
            className="py-16"
            title={L(
              '数据加载失败，正在自动重试…',
              'Load failed — retrying automatically…',
              language
            )}
          />
        ) : !quality || quality.total_cycles === 0 ? (
          <EmptyState
            className="py-16"
            title={L(
              '该时间窗口内没有决策周期',
              'No decision cycles in this window',
              language
            )}
          />
        ) : (
          <>
            {/* Stat strip */}
            <Card className="mb-3 grid grid-cols-2 gap-4 p-4 lg:grid-cols-5">
              <Stat
                label={L('周期成功率', 'Cycle success rate', language)}
                value={`${quality.success_rate_pct.toFixed(1)}%`}
                hint={`${quality.successful_cycles}/${quality.total_cycles} ${L('个周期', 'cycles', language)}`}
                tone={
                  quality.success_rate_pct >= 95
                    ? 'up'
                    : quality.success_rate_pct < 85
                      ? 'down'
                      : 'default'
                }
              />
              <Stat
                label={L('AI 平均耗时', 'AI avg latency', language)}
                value={fmtMs(ai?.avg_ms)}
                hint={L('每次大模型调用', 'per model call', language)}
              />
              <Stat
                label={L('AI P50 / P95', 'AI P50 / P95', language)}
                value={`${fmtMs(ai?.p50_ms)} / ${fmtMs(ai?.p95_ms)}`}
                hint={L('中位数与长尾', 'median vs tail', language)}
              />
              <Stat
                label={L('AI 最长耗时', 'AI max latency', language)}
                value={fmtMs(ai?.max_ms)}
                hint={L('窗口内最慢一次', 'slowest call in window', language)}
              />
              <Stat
                label={L('失败周期', 'Failed cycles', language)}
                value={String(quality.failed_cycles)}
                hint={L(
                  '含执行与 AI 调用失败',
                  'execution + AI failures',
                  language
                )}
                tone={quality.failed_cycles === 0 ? 'up' : 'down'}
              />
            </Card>

            <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
              {/* Failure breakdown */}
              <Card dense>
                <CardHeader
                  title={L('失败分类', 'Failure breakdown', language)}
                />
                {quality.failures.length === 0 ? (
                  <div className="py-8 text-center text-xs text-fg-3">
                    {L(
                      '窗口内无失败周期',
                      'No failed cycles in this window',
                      language
                    )}
                  </div>
                ) : (
                  <div className="overflow-x-auto">
                    <table className="w-full border-collapse text-[13px]">
                      <thead>
                        <tr>
                          <th className={`${TH_CLS} text-left`}>
                            {L('类别', 'Category', language)}
                          </th>
                          <th className={`${TH_CLS} text-right`}>
                            {L('次数', 'Count', language)}
                          </th>
                          <th className={`${TH_CLS} text-left`}>
                            {L('最近示例', 'Latest sample', language)}
                          </th>
                        </tr>
                      </thead>
                      <tbody>
                        {quality.failures.map((f) => (
                          <tr key={f.category} className={TR_CLS}>
                            <td className="px-3 py-0">
                              <Badge variant="down">
                                {failureLabel(f.category, language)}
                              </Badge>
                            </td>
                            <td className="num px-3 py-0 text-right">
                              {f.count}
                            </td>
                            <td
                              className="num max-w-[280px] truncate px-3 py-0 text-fg-3"
                              title={f.sample}
                            >
                              {f.sample || '--'}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </Card>

              {/* Hourly table */}
              <Card dense>
                <CardHeader
                  title={L('逐小时概览', 'Hourly overview', language)}
                />
                <div className="custom-scrollbar max-h-[360px] overflow-auto">
                  <table className="w-full border-collapse text-[13px]">
                    <thead>
                      <tr>
                        <th className={`${TH_CLS} sticky top-0 text-left`}>
                          {L('小时', 'Hour', language)}
                        </th>
                        <th className={`${TH_CLS} sticky top-0 text-right`}>
                          {L('周期', 'Cycles', language)}
                        </th>
                        <th className={`${TH_CLS} sticky top-0 text-right`}>
                          {L('失败', 'Fail', language)}
                        </th>
                        <th className={`${TH_CLS} sticky top-0 text-right`}>
                          {L('平均耗时', 'Avg latency', language)}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {[...quality.hourly].reverse().map((h) => (
                        <tr key={h.hour} className={TR_CLS}>
                          <td className="num px-3 py-0">{h.hour}</td>
                          <td className="num px-3 py-0 text-right">
                            {h.cycles}
                          </td>
                          <td
                            className={`num px-3 py-0 text-right ${h.failures > 0 ? 'text-down' : 'text-fg-3'}`}
                          >
                            {h.failures}
                          </td>
                          <td className="num px-3 py-0 text-right">
                            {fmtMs(h.avg_duration_ms)}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </Card>
            </div>

            <p className="mt-3 max-w-[80ch] text-xs text-fg-3">
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
