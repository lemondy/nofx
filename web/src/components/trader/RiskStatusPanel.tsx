import useSWR from 'swr'
import { api } from '../../lib/api'
import { t, type Language } from '../../i18n/translations'
import type { RiskStatus } from '../../types'

// RiskStatusPanel surfaces the program-enforced risk state the backend
// already computes (2026-10-07 review F1): daily-loss halt, account drawdown
// breaker, loss-streak bans, resting limit entries and open positions in R.

const GOOD = '#2E7D4F'
const BAD = '#C0392B'
const WARN = '#B8912A'
const MUTED = '#6E6E60'

function meterColor(value: number, cap: number) {
  if (cap <= 0) return MUTED
  const ratio = value / cap
  if (ratio >= 1) return BAD
  if (ratio >= 0.6) return WARN
  return GOOD
}

function fmtR(r: number) {
  return `${r >= 0 ? '+' : ''}${r.toFixed(2)}R`
}

function fmtTime(iso: string) {
  const d = new Date(iso)
  return Number.isNaN(d.getTime())
    ? '--'
    : d.toLocaleString(undefined, {
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
      })
}

function Meter({
  label,
  value,
  cap,
  language,
}: {
  label: string
  value: number
  cap: number
  language: Language
}) {
  const shown = Math.max(0, value)
  const color = meterColor(shown, cap)
  const pct = cap > 0 ? Math.min(100, (shown / cap) * 100) : 0
  return (
    <div>
      <div className="flex justify-between text-xs mb-1">
        <span style={{ color: MUTED }}>{label}</span>
        <span className="font-mono" style={{ color }}>
          {shown.toFixed(2)}% /{' '}
          {cap > 0
            ? `${cap}% ${t('riskStatus.cap', language)}`
            : t('riskStatus.off', language)}
        </span>
      </div>
      <div className="h-1.5 rounded bg-nofx-line/40 overflow-hidden">
        <div
          className="h-full rounded"
          style={{ width: `${pct}%`, background: color }}
        />
      </div>
    </div>
  )
}

export function RiskStatusPanel({
  traderId,
  language,
}: {
  traderId: string
  language: Language
}) {
  const { data, error } = useSWR<RiskStatus>(
    traderId ? `risk-status-${traderId}` : null,
    () => api.getRiskStatus(traderId, true),
    { refreshInterval: 30000, dedupingInterval: 10000 }
  )

  if (error && !data) {
    return (
      <div className="nofx-glass p-4 text-xs" style={{ color: MUTED }}>
        {t('riskStatus.unavailable', language)}
      </div>
    )
  }
  if (!data) return null

  const alerts: { text: string; color: string }[] = []
  if (data.daily_halted)
    alerts.push({
      text: `${t('riskStatus.dailyLoss', language)}: ${t('riskStatus.halted', language)}`,
      color: BAD,
    })
  if (data.account_breaker_active)
    alerts.push({
      text: `${t('riskStatus.accountDrawdown', language)}: ${t('riskStatus.halted', language)}`,
      color: BAD,
    })
  if (data.reduce_only)
    alerts.push({ text: t('riskStatus.reduceOnly', language), color: WARN })
  if (data.protection_fault)
    alerts.push({
      text: `${t('riskStatus.protectionFault', language)}: ${data.protection_fault}`,
      color: BAD,
    })

  return (
    <div className="nofx-glass p-5 space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-bold uppercase tracking-wide text-nofx-text-main">
          {t('riskStatus.title', language)}
        </h2>
        <span
          className="text-xs font-semibold"
          style={{ color: alerts.length ? alerts[0].color : GOOD }}
        >
          {alerts.length ? alerts[0].text : t('riskStatus.ok', language)}
        </span>
      </div>
      {alerts.length > 1 && (
        <ul className="text-xs space-y-1">
          {alerts.slice(1).map((a) => (
            <li key={a.text} style={{ color: a.color }}>
              {a.text}
            </li>
          ))}
        </ul>
      )}

      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        <Meter
          label={t('riskStatus.dailyLoss', language)}
          value={data.daily_loss_pct}
          cap={data.daily_loss_cap_pct}
          language={language}
        />
        <Meter
          label={t('riskStatus.accountDrawdown', language)}
          value={data.account_drawdown_pct}
          cap={data.account_drawdown_cap_pct}
          language={language}
        />
      </div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-4 text-xs">
        <div>
          <div className="mb-1" style={{ color: MUTED }}>
            {t('riskStatus.lossStreak', language)}
          </div>
          {!data.loss_streak_enabled ? (
            <div style={{ color: MUTED }}>
              {t('riskStatus.lossStreakOff', language)}
            </div>
          ) : data.loss_streak_bans.length === 0 ? (
            <div style={{ color: GOOD }}>{t('riskStatus.none', language)}</div>
          ) : (
            data.loss_streak_bans.map((b) => (
              <div key={b.symbol} className="font-mono" style={{ color: BAD }}>
                {b.symbol} · {t('riskStatus.until', language)}{' '}
                {fmtTime(b.until)}
              </div>
            ))
          )}
        </div>

        <div>
          <div className="mb-1" style={{ color: MUTED }}>
            {t('riskStatus.pending', language)}
          </div>
          {data.pending_entries.length === 0 ? (
            <div style={{ color: MUTED }}>{t('riskStatus.none', language)}</div>
          ) : (
            data.pending_entries.map((p) => (
              <div
                key={`${p.symbol}-${p.side}`}
                className="font-mono"
                title={`SL ${p.stop_loss} · TP ${p.take_profit} · ${fmtTime(p.placed_at)}`}
              >
                <span style={{ color: p.side === 'long' ? GOOD : BAD }}>
                  {p.side.toUpperCase()}
                </span>{' '}
                {p.symbol} @ {p.price}
              </div>
            ))
          )}
        </div>

        <div>
          <div className="mb-1" style={{ color: MUTED }}>
            {t('riskStatus.positionsR', language)}
          </div>
          {data.positions.length === 0 ? (
            <div style={{ color: MUTED }}>{t('riskStatus.none', language)}</div>
          ) : (
            data.positions.map((p) => (
              <div
                key={`${p.symbol}-${p.side}`}
                className="font-mono"
                title={
                  p.has_initial_stop
                    ? `SL0 ${p.initial_stop} · SL ${p.current_stop} · ${p.exit_mode}`
                    : t('riskStatus.noInitialStop', language)
                }
              >
                {p.symbol}{' '}
                {p.has_initial_stop ? (
                  <>
                    <span style={{ color: p.current_r >= 0 ? GOOD : BAD }}>
                      {t('riskStatus.currentR', language)} {fmtR(p.current_r)}
                    </span>
                    {p.current_stop > 0 && (
                      <span
                        style={{
                          color: p.stop_locked_r >= 0 ? GOOD : MUTED,
                        }}
                      >
                        {' '}
                        · {t('riskStatus.lockedR', language)}{' '}
                        {fmtR(p.stop_locked_r)}
                      </span>
                    )}
                  </>
                ) : (
                  <span style={{ color: MUTED }}>
                    {t('riskStatus.noInitialStop', language)}
                  </span>
                )}
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  )
}
