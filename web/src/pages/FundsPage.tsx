import { useEffect, useMemo, useState } from 'react'
import {
  Bar,
  BarChart,
  CartesianGrid,
  Line,
  LineChart,
  ReferenceLine,
  ResponsiveContainer,
  Tooltip as ChartTooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { AlertTriangle, Wallet } from 'lucide-react'
import { api } from '../lib/api'
import type {
  FundsAccount,
  FundsAccountKind,
  FundsDaily,
  FundsDailyTotal,
  FundsOverview,
  FundsTransferSource,
  FundsTransferType,
} from '../lib/api/funds'
import {
  Badge,
  Button,
  Card,
  CardHeader,
  EmptyState,
  Segmented,
  Stat,
  Tabs,
  Tooltip,
  formatSigned,
  signTone,
} from '../components/ui'
import { getChartTheme, toRgba, useThemeVersion } from '../lib/chartTheme'

const DEFAULT_TZ = 'Asia/Shanghai'

const SELECT_CLS =
  'h-8 rounded-md border border-line bg-surface-2 px-2 text-[13px] text-fg outline-none hover:border-line-strong focus:border-brand'
const TH_CLS =
  'h-8 bg-surface-2 px-3 py-0 text-xs font-medium text-fg-3 whitespace-nowrap'
const TR_CLS =
  'h-[34px] border-b border-line last:border-b-0 hover:bg-surface-hover'

const L = (zh: string, en: string, language: string) =>
  language === 'zh' ? zh : en

/** 金额（无符号, 千分位, 2 位小数） */
const fmtMoney = (v: number) =>
  v.toLocaleString('en-US', {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })

/** 带符号金额（正 + / 负 −，零无符号） */
const fmtSignedMoney = (v: number) => formatSigned(v)

/** 带符号百分比 */
const fmtPct = (v: number) => formatSigned(v, { suffix: '%' })

const toneCls = (v: number) => {
  const tone = signTone(v)
  return tone === 'up' ? 'text-up' : tone === 'down' ? 'text-down' : 'text-fg-2'
}

/** Stat 组件的 tone（无 zero 变体, 零值用 default） */
const statTone = (v: number): 'default' | 'up' | 'down' => {
  const tone = signTone(v)
  return tone === 'up' ? 'up' : tone === 'down' ? 'down' : 'default'
}

/** ISO8601 UTC → 按 tz 显示 "YYYY-MM-DD HH:mm" */
function fmtTimeTz(iso: string, tz: string): string {
  try {
    // en-CA 的日期部分是 YYYY-MM-DD; 某些 ICU 版本会把日期与时间用 ", " 连接
    return new Date(iso)
      .toLocaleString('en-CA', {
        timeZone: tz,
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        hour12: false,
      })
      .replace(', ', ' ')
  } catch {
    return iso
  }
}

const KIND_LABELS: Record<FundsAccountKind, { zh: string; en: string }> = {
  live_futures: { zh: '实盘合约', en: 'Live futures' },
  live_spot: { zh: '实盘现货', en: 'Live spot' },
  paper: { zh: '模拟盘', en: 'Paper' },
}

const TRANSFER_TYPE_LABELS: Record<
  FundsTransferType,
  { zh: string; en: string }
> = {
  transfer_in: { zh: '转入', en: 'Transfer in' },
  transfer_out: { zh: '转出', en: 'Transfer out' },
  deposit: { zh: '充值', en: 'Deposit' },
  withdraw: { zh: '提现', en: 'Withdraw' },
  inferred_in: { zh: '推断转入', en: 'Inferred in' },
  inferred_out: { zh: '推断转出', en: 'Inferred out' },
}

function kindLabel(kind: FundsAccountKind, language: string) {
  return L(KIND_LABELS[kind].zh, KIND_LABELS[kind].en, language)
}

function typeLabel(type: FundsTransferType, language: string) {
  return L(
    TRANSFER_TYPE_LABELS[type].zh,
    TRANSFER_TYPE_LABELS[type].en,
    language
  )
}

/** 来源徽标: 交易所确认 / 推断 */
function SourceBadge({
  source,
  language,
}: {
  source: FundsTransferSource
  language: string
}) {
  return source === 'exchange' ? (
    <Badge variant="info" size="xs">
      {L('交易所确认', 'Exchange-confirmed', language)}
    </Badge>
  ) : (
    <Badge variant="warn" size="xs">
      {L('推断', 'Inferred', language)}
    </Badge>
  )
}

/** 每日明细「转账」列的徽标（exchange / inferred / mixed） */
function DailySourceBadge({
  source,
  language,
}: {
  source: FundsDaily['transfer_source']
  language: string
}) {
  if (source === 'exchange') {
    return (
      <Badge variant="info" size="xs">
        {L('交易所确认', 'Exchange-confirmed', language)}
      </Badge>
    )
  }
  if (source === 'inferred') {
    return (
      <Badge variant="warn" size="xs">
        {L('推断', 'Inferred', language)}
      </Badge>
    )
  }
  if (source === 'mixed') {
    return (
      <Badge variant="neutral" size="xs">
        {L('混合', 'Mixed', language)}
      </Badge>
    )
  }
  return null
}

const fmtAxis = (v: number) =>
  Math.abs(v) >= 1000 ? `${(v / 1000).toFixed(1)}k` : v.toFixed(0)

type ChartRow = {
  date: string
  fullDate: string
  close_equity: number
  trading_pnl: number
  feesNeg: number
  funding: number
  unrealized_change: number
  net_transfer: number
  /** 截至当日的累计净转账 */
  cumTransfer: number
  adjusted_equity: number
}

/** 图表 tooltip: 列出各组成部分（按当日盈亏口径） */
function ChartTooltipContent({
  active,
  payload,
  language,
}: {
  active?: boolean
  payload?: any[]
  language: string
}) {
  if (!active || !payload || payload.length === 0) return null
  const row = payload[0].payload as ChartRow
  const items = [
    { label: L('交易盈亏', 'Trading PnL', language), value: row.trading_pnl },
    { label: L('手续费', 'Fees', language), value: -row.feesNeg },
    { label: L('资金费', 'Funding', language), value: row.funding },
    {
      label: L('浮盈变化', 'Unrealized Δ', language),
      value: row.unrealized_change,
    },
  ]
  return (
    <div className="rounded-md border border-line bg-surface p-2.5 shadow-pop">
      <div className="mb-1 text-xs font-semibold text-fg">{row.fullDate}</div>
      {items.map((it) => (
        <div key={it.label} className="flex justify-between gap-4 text-xs">
          <span className="text-fg-3">{it.label}</span>
          <span className={`num ${toneCls(it.value)}`}>
            {fmtSignedMoney(it.value)}
          </span>
        </div>
      ))}
      <div className="mt-1 flex justify-between gap-4 border-t border-line pt-1 text-xs">
        <span className="text-fg-3">
          {L('转账（不计入盈亏）', 'Transfer (excluded)', language)}
        </span>
        <span className={`num ${toneCls(row.net_transfer)}`}>
          {fmtSignedMoney(row.net_transfer)}
        </span>
      </div>
      <div className="flex justify-between gap-4 text-xs">
        <span className="text-fg-3">
          {L('收盘权益', 'Close equity', language)}
        </span>
        <span className="num text-fg">{fmtMoney(row.close_equity)}</span>
      </div>
      <div className="flex justify-between gap-4 text-xs">
        <span className="text-fg-3">
          {L('剔除转账后权益', 'Equity ex-transfers', language)}
        </span>
        <span className="num text-fg">{fmtMoney(row.adjusted_equity)}</span>
      </div>
    </div>
  )
}

function Legend({ items }: { items: { color: string; label: string }[] }) {
  return (
    <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1">
      {items.map((it) => (
        <span
          key={it.label}
          className="inline-flex items-center gap-1.5 text-xs text-fg-3"
        >
          <span
            className="h-2 w-2 rounded-full"
            style={{ background: it.color }}
          />
          {it.label}
        </span>
      ))}
    </div>
  )
}

export default function FundsPage({ language }: { language: string }) {
  const [days, setDays] = useState(90)
  const [includePaper, setIncludePaper] = useState(false)
  const [accountFilter, setAccountFilter] = useState('all')
  const [chartTab, setChartTab] = useState<'pnl' | 'equity'>('pnl')
  const [data, setData] = useState<FundsOverview | null>(null)
  const [errorMsg, setErrorMsg] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [refreshKey, setRefreshKey] = useState(0)

  useEffect(() => {
    let alive = true
    setLoading(true)
    api
      .getFundsOverview({ days, tz: DEFAULT_TZ, includePaper })
      .then((overview) => {
        if (!alive) return
        setData(overview)
        setErrorMsg(null)
      })
      .catch((err: unknown) => {
        if (!alive) return
        setData(null)
        setErrorMsg(err instanceof Error ? err.message : 'load failed')
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [days, includePaper, refreshKey])

  const themeVersion = useThemeVersion()
  const theme = useMemo(() => getChartTheme(), [themeVersion])

  const accounts = data?.accounts ?? []
  const effectiveFilter =
    accountFilter !== 'all' &&
    accounts.some((a) => a.trader_id === accountFilter)
      ? accountFilter
      : 'all'
  const tz = data?.tz || DEFAULT_TZ

  /** 每日明细/图表使用的数据: 全部(实盘合计) 或 指定账户 */
  const dailyRows: FundsDailyTotal[] | FundsDaily[] = useMemo(() => {
    if (!data) return []
    if (effectiveFilter === 'all') return data.daily_total
    return data.daily.filter((d) => d.trader_id === effectiveFilter)
  }, [data, effectiveFilter])

  /** 图表数据: 按日期升序, 并计算累计净转账与「剔除转账后权益」 */
  const chartRows: ChartRow[] = useMemo(() => {
    const asc = [...dailyRows].sort((a, b) => a.date.localeCompare(b.date))
    const rows: ChartRow[] = []
    asc.forEach((r, i) => {
      const cumTransfer = (i > 0 ? rows[i - 1].cumTransfer : 0) + r.net_transfer
      rows.push({
        date: r.date.slice(5),
        fullDate: r.date,
        close_equity: r.close_equity,
        trading_pnl: r.trading_pnl,
        feesNeg: -r.fees,
        funding: r.funding,
        unrealized_change: r.unrealized_change,
        net_transfer: r.net_transfer,
        cumTransfer,
        adjusted_equity: r.close_equity - cumTransfer,
      })
    })
    return rows
  }, [dailyRows])

  /** 每日明细合计行（对展示的行求和; 收盘权益取最新一天） */
  const dailySums = useMemo(() => {
    const sums = {
      delta_equity: 0,
      net_transfer: 0,
      trading_pnl: 0,
      fees: 0,
      funding: 0,
      unrealized_change: 0,
      unexplained: 0,
    }
    dailyRows.forEach((r) => {
      sums.delta_equity += r.delta_equity
      sums.net_transfer += r.net_transfer
      sums.trading_pnl += r.trading_pnl
      sums.fees += r.fees
      sums.funding += r.funding
      sums.unrealized_change += r.unrealized_change
      sums.unexplained += r.unexplained
    })
    return sums
  }, [dailyRows])
  const latestClose = dailyRows[0]?.close_equity // 行按日期降序

  const totals = data?.totals
  const realPnl =
    totals != null
      ? totals.trading_pnl - totals.fees + totals.funding + totals.other_income
      : 0
  const feesFunding = totals != null ? totals.fees + totals.funding : 0

  return (
    <div className="min-h-screen bg-bg text-fg">
      <div className="mx-auto max-w-[1440px] px-4 py-4 sm:px-6">
        {/* Header */}
        <div className="mb-3 flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-xl font-semibold">
              {L('资金总览', 'Funds Overview', language)}
            </h1>
            <p className="mt-0.5 text-xs text-fg-3">
              {L(
                '把转账（充值/提现/划转）与真实交易盈亏分开——原始收益率会把转入当盈利',
                'Transfers (deposits/withdrawals/moves) separated from real trading PnL — raw return counts deposits as profit',
                language
              )}
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-xs text-fg-3">
              {L('账户', 'Account', language)}
            </span>
            <select
              value={effectiveFilter}
              onChange={(e) => setAccountFilter(e.target.value)}
              className={SELECT_CLS}
              aria-label={L('账户筛选', 'Account filter', language)}
            >
              <option value="all">
                {L('全部（实盘合计）', 'All (live total)', language)}
              </option>
              {accounts.map((a) => (
                <option key={a.trader_id} value={a.trader_id}>
                  {a.name}
                </option>
              ))}
            </select>
            <Segmented
              value={includePaper ? 'paper' : 'live'}
              onChange={(key) => setIncludePaper(key === 'paper')}
              size="sm"
              items={[
                { key: 'live', label: L('仅实盘', 'Live only', language) },
                {
                  key: 'paper',
                  label: L('含模拟盘', 'Include paper', language),
                },
              ]}
            />
            <Segmented
              value={String(days)}
              onChange={(key) => setDays(Number(key))}
              size="sm"
              items={[
                { key: '7', label: L('近 7 天', '7d', language) },
                { key: '30', label: L('近 30 天', '30d', language) },
                { key: '90', label: L('近 90 天', '90d', language) },
              ]}
            />
          </div>
        </div>

        {loading && !data ? (
          <div className="py-16 text-center text-sm text-fg-3">
            {L('加载中', 'Loading', language)}...
          </div>
        ) : errorMsg && !data ? (
          <EmptyState
            className="py-16"
            icon={<AlertTriangle className="h-8 w-8" />}
            title={L('加载失败', 'Failed to load', language)}
            description={errorMsg}
            action={
              <Button
                variant="secondary"
                size="sm"
                onClick={() => setRefreshKey((k) => k + 1)}
              >
                {L('重试', 'Retry', language)}
              </Button>
            }
          />
        ) : !data || accounts.length === 0 ? (
          <EmptyState
            className="py-16"
            icon={<Wallet className="h-8 w-8" />}
            title={L('暂无账户数据', 'No account data', language)}
            description={L(
              '还没有可展示的交易员账户',
              'No trader accounts to show yet',
              language
            )}
          />
        ) : (
          <>
            {/* 1. 总览卡片（实盘合计） */}
            <Card className="mb-3 grid grid-cols-2 gap-4 p-4 lg:grid-cols-6">
              <Stat
                label={L('总权益', 'Total equity', language)}
                value={fmtMoney(totals?.current_equity ?? 0)}
                hint="USDT"
              />
              <Stat
                label={L('净转入', 'Net transfers', language)}
                value={fmtSignedMoney(totals?.net_transfers ?? 0)}
                hint={L('转入 − 转出', 'transfers in − out', language)}
                tone={statTone(totals?.net_transfers ?? 0)}
              />
              <Stat
                label={L('真实交易盈亏', 'Real trading PnL', language)}
                value={fmtSignedMoney(realPnl)}
                hint={L(
                  '交易 − 手续费 + 资金费 + 其他',
                  'trading − fees + funding + other',
                  language
                )}
                tone={statTone(realPnl)}
              />
              <Stat
                label={L('剔除转账后收益率', 'Return ex-transfers', language)}
                value={fmtPct(totals?.adjusted_return_pct ?? 0)}
                hint={L('Modified Dietz', 'Modified Dietz', language)}
                tone={statTone(totals?.adjusted_return_pct ?? 0)}
              />
              <Stat
                label={L('手续费+资金费', 'Fees + funding', language)}
                value={fmtMoney(feesFunding)}
                hint={L(
                  '手续费（支出为正）+ 资金费（收入为正）',
                  'fees (expense+) + funding (income+)',
                  language
                )}
              />
              <Stat
                label={L('未解释差额', 'Unexplained', language)}
                value={fmtSignedMoney(totals?.unexplained ?? 0)}
                hint={L('对账残差中的噪声', 'reconciliation noise', language)}
                tone={statTone(totals?.unexplained ?? 0)}
              />
            </Card>

            {/* 2. 账户表 */}
            <Card dense className="mb-3">
              <CardHeader
                title={L('账户', 'Accounts', language)}
                subtitle={L(
                  '原始收益率 vs 剔除转账后收益率并排对比',
                  'Raw return vs return excluding transfers, side by side',
                  language
                )}
              />
              <div className="overflow-x-auto">
                <table
                  className="w-full border-collapse text-[13px]"
                  aria-label={L('账户表', 'Accounts table', language)}
                >
                  <thead>
                    <tr>
                      <th className={`${TH_CLS} text-left`}>
                        {L('账户', 'Account', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('初始资金', 'Initial', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('当前权益', 'Equity', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('浮动盈亏', 'Unrealized', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('净转入', 'Net transfers', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('交易盈亏', 'Trading PnL', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('手续费', 'Fees', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('资金费', 'Funding', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('剔除转账后盈亏', 'PnL ex-transfers', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('原始收益率', 'Raw return', language)}
                      </th>
                      <th className={`${TH_CLS} text-right`}>
                        {L('剔除转账后收益率', 'Return ex-transfers', language)}
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {accounts.map((a) => (
                      <AccountRow
                        key={a.trader_id}
                        account={a}
                        language={language}
                      />
                    ))}
                  </tbody>
                </table>
              </div>
            </Card>

            {/* 3. 图表 */}
            <Card dense className="mb-3">
              <CardHeader
                title={L('图表', 'Charts', language)}
                subtitle={L(
                  '转账不计入盈亏，单独标记',
                  'transfers are excluded from PnL and marked separately',
                  language
                )}
              />
              <div className="px-3 pb-3 pt-2">
                <Tabs
                  value={chartTab}
                  onChange={setChartTab}
                  items={[
                    { key: 'pnl', label: L('每日盈亏', 'Daily PnL', language) },
                    {
                      key: 'equity',
                      label: L('权益曲线', 'Equity curve', language),
                    },
                  ]}
                />
                {chartRows.length === 0 ? (
                  <EmptyState
                    className="py-10"
                    title={L(
                      '该时间范围内没有对账数据',
                      'No reconciliation data in this range',
                      language
                    )}
                  />
                ) : chartTab === 'pnl' ? (
                  <div
                    aria-label={L('每日盈亏图', 'Daily PnL chart', language)}
                  >
                    <div className="mt-3 h-[260px] w-full">
                      <ResponsiveContainer width="100%" height="100%">
                        <BarChart
                          data={chartRows}
                          stackOffset="sign"
                          margin={{ top: 8, right: 12, bottom: 0, left: 0 }}
                        >
                          <CartesianGrid stroke={theme.grid} vertical={false} />
                          <XAxis
                            dataKey="date"
                            tick={{ fill: theme.text, fontSize: 11 }}
                            stroke={theme.border}
                            minTickGap={16}
                          />
                          <YAxis
                            tick={{ fill: theme.text, fontSize: 11 }}
                            stroke={theme.border}
                            width={52}
                            tickFormatter={fmtAxis}
                          />
                          <ChartTooltip
                            cursor={{ fill: theme.grid }}
                            content={
                              <ChartTooltipContent language={language} />
                            }
                          />
                          <ReferenceLine y={0} stroke={theme.border} />
                          <Bar
                            dataKey="trading_pnl"
                            stackId="pnl"
                            fill={theme.up}
                            isAnimationActive={false}
                          />
                          <Bar
                            dataKey="feesNeg"
                            stackId="pnl"
                            fill={theme.down}
                            isAnimationActive={false}
                          />
                          <Bar
                            dataKey="funding"
                            stackId="pnl"
                            fill={theme.info}
                            isAnimationActive={false}
                          />
                          <Bar
                            dataKey="unrealized_change"
                            stackId="pnl"
                            fill={theme.brand}
                            isAnimationActive={false}
                          />
                          <Bar
                            dataKey="net_transfer"
                            fill={toRgba(theme.text, 0.45)}
                            isAnimationActive={false}
                          />
                        </BarChart>
                      </ResponsiveContainer>
                    </div>
                    <Legend
                      items={[
                        {
                          color: theme.up,
                          label: L('交易盈亏', 'Trading PnL', language),
                        },
                        {
                          color: theme.down,
                          label: L('手续费', 'Fees', language),
                        },
                        {
                          color: theme.info,
                          label: L('资金费', 'Funding', language),
                        },
                        {
                          color: theme.brand,
                          label: L('浮盈变化', 'Unrealized Δ', language),
                        },
                        {
                          color: toRgba(theme.text, 0.45),
                          label: L(
                            '转账（不计入盈亏）',
                            'Transfers (excluded)',
                            language
                          ),
                        },
                      ]}
                    />
                  </div>
                ) : (
                  <div
                    aria-label={L('权益曲线图', 'Equity curve chart', language)}
                  >
                    <div className="mt-3 h-[260px] w-full">
                      <ResponsiveContainer width="100%" height="100%">
                        <LineChart
                          data={chartRows}
                          margin={{ top: 8, right: 12, bottom: 0, left: 0 }}
                        >
                          <CartesianGrid stroke={theme.grid} vertical={false} />
                          <XAxis
                            dataKey="date"
                            tick={{ fill: theme.text, fontSize: 11 }}
                            stroke={theme.border}
                            minTickGap={16}
                          />
                          <YAxis
                            tick={{ fill: theme.text, fontSize: 11 }}
                            stroke={theme.border}
                            width={52}
                            tickFormatter={fmtAxis}
                          />
                          <ChartTooltip
                            cursor={{ stroke: theme.crosshair }}
                            content={
                              <ChartTooltipContent language={language} />
                            }
                          />
                          {chartRows
                            .filter((r) => Math.abs(r.net_transfer) > 0.005)
                            .map((r) => (
                              <ReferenceLine
                                key={r.fullDate}
                                x={r.date}
                                stroke={toRgba(theme.info, 0.7)}
                                strokeDasharray="3 3"
                              />
                            ))}
                          <ReferenceLine y={0} stroke={theme.border} />
                          <Line
                            dataKey="close_equity"
                            stroke={theme.brand}
                            strokeWidth={2}
                            dot={false}
                            isAnimationActive={false}
                          />
                          <Line
                            dataKey="adjusted_equity"
                            stroke={theme.up}
                            strokeWidth={2}
                            strokeDasharray="5 3"
                            dot={false}
                            isAnimationActive={false}
                          />
                        </LineChart>
                      </ResponsiveContainer>
                    </div>
                    <Legend
                      items={[
                        {
                          color: theme.brand,
                          label: L('权益', 'Equity', language),
                        },
                        {
                          color: theme.up,
                          label: L(
                            '剔除转账后权益',
                            'Equity ex-transfers',
                            language
                          ),
                        },
                        {
                          color: toRgba(theme.info, 0.7),
                          label: L('转账日期', 'Transfer date', language),
                        },
                      ]}
                    />
                  </div>
                )}
              </div>
            </Card>

            {/* 4. 每日明细表 */}
            <Card dense className="mb-3">
              <CardHeader
                title={L('每日明细', 'Daily detail', language)}
                subtitle={L(
                  '按日对账: 权益变化 = 盈亏 + 转账 + 未解释',
                  'Daily reconciliation: Δ equity = PnL + transfers + unexplained',
                  language
                )}
              />
              {dailyRows.length === 0 ? (
                <EmptyState
                  className="py-10"
                  title={L(
                    '该时间范围内没有对账数据',
                    'No reconciliation data in this range',
                    language
                  )}
                />
              ) : (
                <div className="overflow-x-auto">
                  <table
                    className="w-full border-collapse text-[13px]"
                    aria-label={L('每日明细表', 'Daily detail table', language)}
                  >
                    <thead>
                      <tr>
                        <th className={`${TH_CLS} text-left`}>
                          {L('日期', 'Date', language)}
                        </th>
                        <th className={`${TH_CLS} text-right`}>
                          {L('收盘权益', 'Close equity', language)}
                        </th>
                        <th className={`${TH_CLS} text-right`}>
                          {L('权益变化', 'Δ equity', language)}
                        </th>
                        <th className={`${TH_CLS} text-right`}>
                          {L('转账', 'Transfer', language)}
                        </th>
                        <th className={`${TH_CLS} text-right`}>
                          {L('交易盈亏', 'Trading PnL', language)}
                        </th>
                        <th className={`${TH_CLS} text-right`}>
                          {L('手续费', 'Fees', language)}
                        </th>
                        <th className={`${TH_CLS} text-right`}>
                          {L('资金费', 'Funding', language)}
                        </th>
                        <th className={`${TH_CLS} text-right`}>
                          {L('浮盈变化', 'Unrealized Δ', language)}
                        </th>
                        <th className={`${TH_CLS} text-right`}>
                          {L('未解释', 'Unexplained', language)}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {dailyRows.map((r) => (
                        <tr key={r.date} className={TR_CLS}>
                          <td className="num px-3 py-0">{r.date}</td>
                          <td className="num px-3 py-0 text-right">
                            {fmtMoney(r.close_equity)}
                          </td>
                          <td
                            className={`num px-3 py-0 text-right ${toneCls(r.delta_equity)}`}
                          >
                            {fmtSignedMoney(r.delta_equity)}
                          </td>
                          <td className="px-3 py-0 text-right">
                            <span className="inline-flex items-center gap-1.5">
                              <span
                                className={`num ${toneCls(r.net_transfer)}`}
                              >
                                {fmtSignedMoney(r.net_transfer)}
                              </span>
                              <DailySourceBadge
                                source={r.transfer_source}
                                language={language}
                              />
                            </span>
                          </td>
                          <td
                            className={`num px-3 py-0 text-right ${toneCls(r.trading_pnl)}`}
                          >
                            {fmtSignedMoney(r.trading_pnl)}
                          </td>
                          <td className="num px-3 py-0 text-right">
                            {fmtMoney(r.fees)}
                          </td>
                          <td
                            className={`num px-3 py-0 text-right ${toneCls(r.funding)}`}
                          >
                            {fmtSignedMoney(r.funding)}
                          </td>
                          <td
                            className={`num px-3 py-0 text-right ${toneCls(r.unrealized_change)}`}
                          >
                            {fmtSignedMoney(r.unrealized_change)}
                          </td>
                          <td
                            className={`num px-3 py-0 text-right ${toneCls(r.unexplained)}`}
                          >
                            {fmtSignedMoney(r.unexplained)}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                    <tfoot>
                      <tr className="h-[34px] border-t border-line-strong bg-surface-2 font-semibold">
                        <td className="px-3 py-0">
                          {L('合计', 'Total', language)}
                        </td>
                        <td className="num px-3 py-0 text-right">
                          {latestClose != null ? fmtMoney(latestClose) : '--'}
                        </td>
                        <td
                          className={`num px-3 py-0 text-right ${toneCls(dailySums.delta_equity)}`}
                        >
                          {fmtSignedMoney(dailySums.delta_equity)}
                        </td>
                        <td
                          className={`num px-3 py-0 text-right ${toneCls(dailySums.net_transfer)}`}
                        >
                          {fmtSignedMoney(dailySums.net_transfer)}
                        </td>
                        <td
                          className={`num px-3 py-0 text-right ${toneCls(dailySums.trading_pnl)}`}
                        >
                          {fmtSignedMoney(dailySums.trading_pnl)}
                        </td>
                        <td className="num px-3 py-0 text-right">
                          {fmtMoney(dailySums.fees)}
                        </td>
                        <td
                          className={`num px-3 py-0 text-right ${toneCls(dailySums.funding)}`}
                        >
                          {fmtSignedMoney(dailySums.funding)}
                        </td>
                        <td
                          className={`num px-3 py-0 text-right ${toneCls(dailySums.unrealized_change)}`}
                        >
                          {fmtSignedMoney(dailySums.unrealized_change)}
                        </td>
                        <td
                          className={`num px-3 py-0 text-right ${toneCls(dailySums.unexplained)}`}
                        >
                          {fmtSignedMoney(dailySums.unexplained)}
                        </td>
                      </tr>
                    </tfoot>
                  </table>
                </div>
              )}
            </Card>

            {/* 5. 转账列表 */}
            <Card dense className="mb-3">
              <CardHeader
                title={L('转账列表', 'Transfers', language)}
                subtitle={L(
                  '转入为绿、转出为红; 来源分「交易所确认」与「推断」',
                  'transfers in green / out red; source is exchange-confirmed or inferred',
                  language
                )}
              />
              {data.flows.length === 0 ? (
                <EmptyState
                  className="py-10"
                  title={L('暂无转账记录', 'No transfers', language)}
                />
              ) : (
                <div className="overflow-x-auto">
                  <table
                    className="w-full border-collapse text-[13px]"
                    aria-label={L('转账列表', 'Transfers table', language)}
                  >
                    <thead>
                      <tr>
                        <th className={`${TH_CLS} text-left`}>
                          {L('时间', 'Time', language)} ({tz})
                        </th>
                        <th className={`${TH_CLS} text-left`}>
                          {L('账户', 'Account', language)}
                        </th>
                        <th className={`${TH_CLS} text-right`}>
                          {L('金额', 'Amount', language)}
                        </th>
                        <th className={`${TH_CLS} text-left`}>
                          {L('类型', 'Type', language)}
                        </th>
                        <th className={`${TH_CLS} text-left`}>
                          {L('来源', 'Source', language)}
                        </th>
                        <th className={`${TH_CLS} text-left`}>
                          {L('备注', 'Note', language)}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {data.flows.map((f) => (
                        <tr key={f.id} className={TR_CLS}>
                          <td className="num whitespace-nowrap px-3 py-0 text-fg-2">
                            {fmtTimeTz(f.time, tz)}
                          </td>
                          <td className="px-3 py-0">{f.name}</td>
                          <td
                            className={`num px-3 py-0 text-right ${toneCls(f.amount)}`}
                          >
                            {fmtSignedMoney(f.amount)}
                          </td>
                          <td className="px-3 py-0">
                            {typeLabel(f.type, language)}
                          </td>
                          <td className="px-3 py-0">
                            <SourceBadge
                              source={f.source}
                              language={language}
                            />
                          </td>
                          <td
                            className="max-w-[320px] truncate px-3 py-0 text-xs text-fg-3"
                            title={f.note}
                          >
                            {f.note || '--'}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </Card>

            <p className="mt-3 max-w-[100ch] text-xs text-fg-3">
              {L(
                `口径：金额均为 USDT（USDC 等稳定币按 1:1 折算）；合计仅含实盘账户，模拟盘需勾选「含模拟盘」；「剔除转账后」= 权益 − 初始资金 − 净转账，收益率用 Modified Dietz。生成时间 ${fmtTimeTz(data.generated_at, tz)}（${tz}）。`,
                `Scope: amounts in USDT (stablecoins at 1:1); totals cover live accounts only — tick "Include paper" for paper accounts; "ex-transfers" = equity − initial − net transfers, return via Modified Dietz. Generated at ${fmtTimeTz(data.generated_at, tz)} (${tz}).`,
                language
              )}
            </p>
          </>
        )}
      </div>
    </div>
  )
}

/** 原始收益率 vs 剔除转账后收益率并排; 相差 >10 个百分点时高亮并提示「包含转账影响」 */
function AccountRow({
  account,
  language,
}: {
  account: FundsAccount
  language: string
}) {
  const distorted =
    Math.abs(account.raw_return_pct - account.adjusted_return_pct) > 10
  return (
    <tr className={TR_CLS}>
      <td className="px-3 py-0">
        <span className="flex items-center gap-1.5 whitespace-nowrap">
          <span className="font-medium">{account.name}</span>
          <Badge
            variant={account.kind === 'paper' ? 'neutral' : 'brand'}
            size="xs"
          >
            {kindLabel(account.kind, language)}
          </Badge>
          {account.warnings.length > 0 && (
            <Tooltip
              content={
                <div className="max-w-xs space-y-1">
                  {account.warnings.map((w) => (
                    <div key={w}>{w}</div>
                  ))}
                </div>
              }
            >
              <button
                type="button"
                aria-label={L(
                  `${account.name} 的警告`,
                  `warnings for ${account.name}`,
                  language
                )}
                className="inline-flex h-4 w-4 items-center justify-center"
              >
                <AlertTriangle className="h-3.5 w-3.5 text-warn" />
              </button>
            </Tooltip>
          )}
        </span>
      </td>
      <td className="num px-3 py-0 text-right">
        {fmtMoney(account.initial_balance)}
      </td>
      <td className="num px-3 py-0 text-right">
        {fmtMoney(account.current_equity)}
      </td>
      <td
        className={`num px-3 py-0 text-right ${toneCls(account.unrealized_pnl)}`}
      >
        {fmtSignedMoney(account.unrealized_pnl)}
      </td>
      <td
        className={`num px-3 py-0 text-right ${toneCls(account.net_transfers)}`}
      >
        {fmtSignedMoney(account.net_transfers)}
      </td>
      <td
        className={`num px-3 py-0 text-right ${toneCls(account.trading_pnl)}`}
      >
        {fmtSignedMoney(account.trading_pnl)}
      </td>
      <td className="num px-3 py-0 text-right">{fmtMoney(account.fees)}</td>
      <td className={`num px-3 py-0 text-right ${toneCls(account.funding)}`}>
        {fmtSignedMoney(account.funding)}
      </td>
      <td
        className={`num px-3 py-0 text-right ${toneCls(account.adjusted_pnl)}`}
      >
        {fmtSignedMoney(account.adjusted_pnl)}
      </td>
      <td
        className={`num whitespace-nowrap px-3 py-0 text-right ${
          distorted
            ? 'rounded bg-warn-soft px-1.5 text-warn'
            : toneCls(account.raw_return_pct)
        }`}
      >
        {fmtPct(account.raw_return_pct)}
        {distorted && (
          <Badge variant="warn" size="xs" className="ml-1.5">
            {L('包含转账影响', 'Includes transfers', language)}
          </Badge>
        )}
      </td>
      <td
        className={`num whitespace-nowrap px-3 py-0 text-right ${
          distorted
            ? 'rounded bg-warn-soft px-1.5 text-warn'
            : toneCls(account.adjusted_return_pct)
        }`}
      >
        {fmtPct(account.adjusted_return_pct)}
      </td>
    </tr>
  )
}
