import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useLanguage } from '../contexts/LanguageContext'
import { t } from '../i18n/translations'
import { Loader2 } from 'lucide-react'
import {
  Badge,
  Button,
  Card,
  CardHeader,
  CardBody,
  Change,
  Segmented,
  Tabs,
} from '../components/ui'

// Vergex trending data (https://vergex.trade/trending), proxied through
// the NOFX backend (/api/trending/*) with a short server-side cache.

// Row of vergex /data-intelligence/flow/markets (the official net inflow card).
// Upstream sends netFlow/latestPrice as numeric strings; priceChangePct is a
// number already expressed in percent.
interface FlowRow {
  key: string
  marketType: string
  symbol: string
  netFlow: number
  latestPrice: number
  priceChangePct: number
  trades?: number
}

interface FlowResponse {
  data?: {
    by: string
    window: string
    inflow?: Record<string, any>[]
    outflow?: Record<string, any>[]
  }
}

interface OIRow {
  current_oi: number
  net_long: number
  net_short: number
  oi_delta: number
  oi_delta_percent: number
  oi_delta_value: number
  price: number
  price_delta_percent: number
  rank: number
  symbol: string
}

interface DepthRow {
  rank: number
  symbol: string
  bid_volume: number
  ask_volume: number
  delta: number
  price: number
  price_delta_percent: number
}

interface RatesRow {
  rank: number
  symbol: string
  funding_rate: number
  mark_price: number
  index_price: number
  next_funding_time: number
  price_delta_percent: number
}

interface PriceRow {
  pair: string
  symbol: string
  price_delta: number
  price: number
  future_flow: number
  spot_flow: number
  oi: number
  oi_delta: number
  oi_delta_value: number
}

type GenericRow = Record<string, any>

interface CryptoResponse {
  top?: GenericRow[]
  low?: GenericRow[]
  future?: DepthRow[]
  spot?: DepthRow[]
}

interface HLRow {
  symbol: string
  base: string
  quote: string
  dex: string
  maxLeverage: number
  lastPrice: number
  change24hAbs: number
  change24hPct: number
  funding8h: number
  volume24h: number
  openInterest: number
}

interface HLResponse {
  generatedAt?: string
  rows?: HLRow[]
}

interface CategoryAsset {
  symbol: string
  name?: string
  /** Vergex sends pre-formatted strings for AI500 assets and raw numbers elsewhere. */
  price?: number | string
  change?: string
  change_pct?: number
  score?: number
  signal?: string
}

interface PredictionRow {
  symbol?: string
  title?: string
  name?: string
  price?: string
  change?: string
  volume?: string
  signal?: string
  icon?: string
}

interface CategoryResponse {
  marketSession?: {
    session?: string
    labelEn?: string
    labelZh?: string
  }
  category?: {
    key: string
    label: string
    title: string
    accent?: string
    assets?: CategoryAsset[]
  }
  tableRows?: PredictionRow[]
}

type CryptoTab = 'net_flow' | 'oi' | 'depth' | 'rates' | 'price'
type Duration = '5m' | '15m' | '30m' | '1h' | '4h' | '8h' | '12h' | '24h'

const DURATIONS: Duration[] = [
  '5m',
  '15m',
  '30m',
  '1h',
  '4h',
  '8h',
  '12h',
  '24h',
]
const PRICE_DURATIONS: Duration[] = [
  '15m',
  '30m',
  '1h',
  '4h',
  '8h',
  '12h',
  '24h',
]
// /flow/markets supports every duration except 30m.
const FLOW_DURATIONS: Duration[] = ['5m', '15m', '1h', '4h', '8h', '12h', '24h']
const HL_CATEGORIES = [
  { key: 'crypto', labelKey: 'dataPage.hlCrypto' },
  { key: 'stocks', labelKey: 'dataPage.hlStocks' },
  { key: 'preipo', labelKey: 'dataPage.hlPreIPO' },
  { key: 'indices', labelKey: 'dataPage.hlIndices' },
  { key: 'commodities', labelKey: 'dataPage.hlCommodities' },
  { key: 'fx', labelKey: 'dataPage.hlFx' },
] as const
const PAGE_SIZES = [10, 20, 50]
const REFRESH_MS = 60_000

function fmtUsd(v: number | undefined, digits = 2): string {
  if (v === undefined || v === null || !Number.isFinite(v)) return '-'
  const abs = Math.abs(v)
  if (abs >= 1e9) return `$${(v / 1e9).toFixed(2)}B`
  if (abs >= 1e6) return `$${(v / 1e6).toFixed(2)}M`
  if (abs >= 1e3) return `$${(v / 1e3).toFixed(1)}K`
  return `$${v.toFixed(digits)}`
}

function fmtPrice(v: number | undefined): string {
  if (v === undefined || v === null || !Number.isFinite(v)) return '-'
  if (v >= 1000)
    return `$${v.toLocaleString('en-US', { maximumFractionDigits: 2 })}`
  if (v >= 1) return `$${v.toFixed(3)}`
  return `$${v.toFixed(6)}`
}

function fmtPct(v: number | undefined): string {
  if (v === undefined || v === null || !Number.isFinite(v)) return '-'
  return `${v >= 0 ? '+' : ''}${(v * 100).toFixed(2)}%`
}

// For fields vergex already expresses in percent units
// (e.g. price_delta_percent, oi_delta_percent, change24hPct).
function fmtPctDirect(v: number | undefined): string {
  if (v === undefined || v === null || !Number.isFinite(v)) return '-'
  return `${v >= 0 ? '+' : ''}${v.toFixed(2)}%`
}

// Funding rates arrive in percent units with 4-decimal precision upstream.
function fmtRate4(v: number | undefined): string {
  if (v === undefined || v === null || !Number.isFinite(v)) return '-'
  return `${v.toFixed(4)}%`
}

function pctColorCls(v: number | undefined): string {
  if (v === undefined || v === null || !Number.isFinite(v) || v === 0)
    return 'text-fg-3'
  return v > 0 ? 'text-up' : 'text-down'
}

const VERGEX_BASE = 'https://vergex.trade'

// Map a backend proxy path (/api/trending/*) to the vergex upstream path it
// proxies, so the browser can fetch upstream directly and relay the payload.
function vergexUpstreamPath(backendPath: string): string | null {
  const m = backendPath.match(
    /^\/api\/trending\/(category|hl|crypto|flow)\?(.+)$/
  )
  if (!m) return null
  const [, endpoint, query] = m
  if (endpoint === 'flow')
    return `/api/v1/data-intelligence/flow/markets?${query}`
  return `/trending-${endpoint}?${query}`
}

// Relay a browser-fetched vergex payload to the backend so the trending proxy
// endpoints and the strategy coin sources can serve it. Fire-and-forget.
function relayVergexPayload(upstreamPath: string, body: string): void {
  // The relay endpoint is auth-protected (cache poisoning fix 09-25): the
  // dashboard is logged in, so attach the same bearer the http client uses.
  const token = localStorage.getItem('auth_token')
  fetch(`/api/trending/relay?path=${encodeURIComponent(upstreamPath)}`, {
    method: 'POST',
    headers: token ? { Authorization: `Bearer ${token}` } : undefined,
    body,
  }).catch(() => {})
}

// Fetch a trending endpoint: try the browser's direct vergex connection first
// (passes Cloudflare, unlike backend requests), relaying the payload for the
// backend; fall back to the backend proxy on any direct-fetch failure.
async function fetchTrendingJSON(backendPath: string): Promise<unknown> {
  const upstream = vergexUpstreamPath(backendPath)
  if (upstream) {
    try {
      const res = await fetch(VERGEX_BASE + upstream, { cache: 'no-store' })
      if (res.ok) {
        const text = await res.text()
        relayVergexPayload(upstream, text)
        return JSON.parse(text)
      }
    } catch {
      // Fall through to the backend proxy.
    }
  }
  const res = await fetch(backendPath)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

// Keep the backend's vergex caches fed with the upstream paths consumed by
// strategy coin sources (provider/vergex), independent of which cards the
// user currently has on screen. Paths not covered here fall back to NofxOS
// in the kernel when the relay entry goes stale.
function useVergexRelaySync(): void {
  useEffect(() => {
    const paths = [
      '/trending-category?key=ai500&lang=en',
      '/trending-crypto?tab=oi&duration=1h&limit=100',
      '/trending-crypto?tab=price&duration=1h&limit=100',
      '/api/v1/data-intelligence/flow/markets?window=1h&limit=25',
    ]
    const sync = () => {
      for (const p of paths) {
        fetch(VERGEX_BASE + p, { cache: 'no-store' })
          .then((r) => (r.ok ? r.text() : null))
          .then((text) => {
            if (text) relayVergexPayload(p, text)
          })
          .catch(() => {})
      }
    }
    sync()
    const timer = setInterval(sync, REFRESH_MS)
    return () => clearInterval(timer)
  }, [])
}

function useTrendingData<T>(
  path: string | null,
  refreshMs = REFRESH_MS
): { data: T | null; failed: boolean } {
  const [data, setData] = useState<T | null>(null)
  const [failed, setFailed] = useState(false)
  const [lastPath, setLastPath] = useState<string | null>(path)

  // Render-time reset: a new path invalidates the previous response immediately
  // (no stale first frame from another tab's rows).
  if (lastPath !== path) {
    setLastPath(path)
    setData(null)
    setFailed(false)
  }

  useEffect(() => {
    if (!path) {
      return
    }
    let cancelled = false
    const load = () => {
      fetchTrendingJSON(path)
        .then((json) => {
          if (!cancelled) {
            setData(json as T)
            setFailed(false)
          }
        })
        .catch(() => {
          // Keep previous data on transient failures of the SAME endpoint
          if (!cancelled) setFailed(true)
        })
    }
    load()
    const timer = setInterval(load, refreshMs)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [path, refreshMs])

  return { data, failed }
}

// Generic client-side pagination hook.
function usePaged<T>(rows: T[], defaultSize: number) {
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(defaultSize)
  const totalPages = Math.max(1, Math.ceil(rows.length / pageSize))
  const safePage = Math.min(page, totalPages)
  const paged = rows.slice((safePage - 1) * pageSize, safePage * pageSize)
  return {
    paged,
    page: safePage,
    totalPages,
    pageSize,
    setPage: (p: number) => setPage(Math.min(Math.max(1, p), totalPages)),
    setPageSize: (s: number) => {
      setPageSize(s)
      setPage(1)
    },
  }
}

function Pagination({
  page,
  totalPages,
  pageSize,
  total,
  onPage,
  onPageSize,
  language,
}: {
  page: number
  totalPages: number
  pageSize: number
  total: number
  onPage: (p: number) => void
  onPageSize: (s: number) => void
  language: 'en' | 'zh' | 'id'
}) {
  return (
    <div className="flex items-center justify-between pt-3 flex-wrap gap-2">
      <div className="num text-xs text-fg-3">
        {t('dataPage.totalRows', language, { count: total })}
      </div>
      <div className="flex items-center gap-2">
        <select
          value={pageSize}
          onChange={(e) => onPageSize(Number(e.target.value))}
          className="h-7 rounded-md border border-line bg-surface-2 px-2 text-xs text-fg outline-none hover:border-line-strong focus:border-brand"
        >
          {PAGE_SIZES.map((s) => (
            <option key={s} value={s}>
              {s} / page
            </option>
          ))}
        </select>
        <Button
          size="sm"
          disabled={page <= 1}
          onClick={() => onPage(page - 1)}
          aria-label="previous page"
        >
          ‹
        </Button>
        <span className="num px-1 text-xs text-fg-3">
          {page} / {totalPages}
        </span>
        <Button
          size="sm"
          disabled={page >= totalPages}
          onClick={() => onPage(page + 1)}
          aria-label="next page"
        >
          ›
        </Button>
      </div>
    </div>
  )
}

function RankBadge({ rank }: { rank?: number }) {
  if (!rank) return null
  return (
    <span className="num inline-block min-w-5 text-xs font-medium text-fg-3">
      {rank}
    </span>
  )
}

function LoadingRow({ cols }: { cols: number }) {
  return (
    <tr>
      <td colSpan={cols} className="py-8 text-center text-sm text-fg-3">
        <Loader2 className="mr-2 inline-block h-4 w-4 animate-spin align-[-3px]" />
        Loading...
      </td>
    </tr>
  )
}

// --- Featured picks (AI500 / Prediction) ---

function PredictionTable({
  rows,
  language,
}: {
  rows: PredictionRow[]
  language: 'en' | 'zh' | 'id'
}) {
  const paged = usePaged(rows, 10)
  return (
    <>
      <table className="w-full border-collapse text-[13px]">
        <tbody>
          {paged.paged.map((row, i) => (
            <TableRow key={`${row.symbol ?? row.title}-${i}`}>
              <Td>
                <span className="line-clamp-1 whitespace-normal font-semibold text-fg">
                  {row.title ?? row.symbol}
                </span>
                {row.signal && (
                  <span className="ml-2 text-[11px] text-fg-3">
                    {row.signal}
                  </span>
                )}
              </Td>
              <Td right mono>
                {row.price ?? '-'}
              </Td>
              <Td right>{row.change ?? '-'}</Td>
              <Td right>{row.volume ?? '-'}</Td>
            </TableRow>
          ))}
        </tbody>
      </table>
      <Pagination
        page={paged.page}
        totalPages={paged.totalPages}
        pageSize={paged.pageSize}
        total={rows.length}
        onPage={paged.setPage}
        onPageSize={paged.setPageSize}
        language={language}
      />
    </>
  )
}

function FeaturedCard({
  cacheKey,
  titleKey,
  accent,
  language,
}: {
  cacheKey: 'ai500' | 'prediction'
  titleKey: string
  accent: string
  language: 'en' | 'zh' | 'id'
}) {
  const { data } = useTrendingData<CategoryResponse>(
    `/api/trending/category?key=${cacheKey}&lang=${language === 'zh' ? 'zh' : 'en'}`
  )
  const assets = data?.category?.assets || []
  const tableRows = data?.tableRows || []
  const session = data?.marketSession
  const sessionLabel = session?.labelEn
    ? language === 'zh'
      ? session.labelZh
      : session.labelEn
    : null

  return (
    <Card dense>
      <CardHeader
        title={
          <span className="inline-flex items-center gap-2">
            <span
              aria-hidden
              className="h-1.5 w-1.5 rounded-full"
              style={{ background: accent }}
            />
            {t(titleKey, language)}
          </span>
        }
        actions={
          sessionLabel ? (
            <Badge
              variant={session?.session === 'open' ? 'up' : 'neutral'}
              size="xs"
            >
              {sessionLabel}
            </Badge>
          ) : undefined
        }
      />
      <CardBody>
        {!data ? (
          <div className="py-4 text-center text-xs text-fg-3">Loading...</div>
        ) : tableRows.length > 0 ? (
          <PredictionTable rows={tableRows} language={language} />
        ) : assets.length === 0 ? (
          <div className="py-4 text-center text-xs text-fg-3">
            {t('dataPage.noData', language)}
          </div>
        ) : (
          <div className="max-h-72 overflow-y-auto">
            <table className="w-full border-collapse text-[13px]">
              <tbody>
                {assets.map((a, i) => {
                  const pct = a.change
                    ? parseFloat(a.change.replace(/[^0-9.+-]/g, ''))
                    : a.change_pct
                  return (
                    <TableRow key={`${a.symbol}-${i}`}>
                      <Td>
                        <RankBadge rank={i + 1} />
                      </Td>
                      <Td>
                        <span className="font-semibold text-fg">
                          {a.symbol}
                        </span>
                        {a.signal && (
                          <span className="ml-2 text-[11px] text-fg-3">
                            {a.signal}
                          </span>
                        )}
                      </Td>
                      <Td right>
                        {a.price !== undefined
                          ? typeof a.price === 'string'
                            ? a.price
                            : fmtPrice(a.price)
                          : ''}
                      </Td>
                      <Td right>
                        {a.change && Number.isFinite(pct) ? (
                          <span className={pctColorCls(pct)}>{a.change}</span>
                        ) : a.change ? (
                          <span className="text-fg-3">{a.change}</span>
                        ) : (
                          <Change value={a.change_pct} />
                        )}
                      </Td>
                    </TableRow>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </CardBody>
    </Card>
  )
}

// --- Two-column top/low table (net_flow, oi, rates, price) ---

function TwoSidedTable<T extends GenericRow>({
  top,
  low,
  language,
  cols,
  renderRow,
  renderHeader,
}: {
  top: T[] | undefined
  low: T[] | undefined
  language: 'en' | 'zh' | 'id'
  cols: number
  renderRow: (row: T, isTop: boolean, globalRank: number) => ReactNode
  renderHeader: (isTop: boolean) => ReactNode
}) {
  // Drop upstream placeholder rows (missing symbol/pair) and paginate the two
  // lists in lockstep — they have equal length.
  const topRows = (top || []).filter((r) => !!(r.symbol || r.pair))
  const lowRows = (low || []).filter((r) => !!(r.symbol || r.pair))
  const paged = usePaged<T>(topRows, 10)
  const lowSlice = lowRows.slice(
    (paged.page - 1) * paged.pageSize,
    paged.page * paged.pageSize
  )

  if (!top && !low) {
    return (
      <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3">
        <table className="w-full border-collapse text-[13px]">
          <tbody>
            <LoadingRow cols={cols} />
          </tbody>
        </table>
      </div>
    )
  }

  const rankAt = (i: number) => (paged.page - 1) * paged.pageSize + i + 1

  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3">
      {/* Inflow side */}
      <div className="mb-2 flex items-center gap-2">
        <Badge variant="up">▲ {t('dataPage.topInflow', language)}</Badge>
      </div>
      <table className="w-full border-collapse text-[13px]">
        <thead>{renderHeader(true)}</thead>
        <tbody>
          {paged.paged.map((row, i) => (
            <TableRow key={i}>{renderRow(row, true, rankAt(i))}</TableRow>
          ))}
        </tbody>
      </table>
      {/* Outflow side */}
      <div className="mb-2 mt-4 flex items-center gap-2">
        <Badge variant="down">▼ {t('dataPage.bottomOutflow', language)}</Badge>
      </div>
      <table className="w-full border-collapse text-[13px]">
        <thead>{renderHeader(false)}</thead>
        <tbody>
          {lowSlice.map((row, i) => (
            <TableRow key={i}>{renderRow(row, false, rankAt(i))}</TableRow>
          ))}
        </tbody>
      </table>
      <Pagination
        page={paged.page}
        totalPages={paged.totalPages}
        pageSize={paged.pageSize}
        total={topRows.length + lowRows.length}
        onPage={paged.setPage}
        onPageSize={paged.setPageSize}
        language={language}
      />
    </div>
  )
}

function TableRow({ children }: { children: ReactNode }) {
  return (
    <tr className="h-[34px] border-b border-line transition-colors last:border-b-0 hover:bg-surface-hover">
      {children}
    </tr>
  )
}

function Th({ children, right }: { children: ReactNode; right?: boolean }) {
  return (
    <th
      className={`sticky top-0 h-8 whitespace-nowrap bg-surface-2 px-3 py-0 text-xs font-medium normal-case tracking-normal text-fg-3 ${right ? 'text-right' : 'text-left'}`}
    >
      {children}
    </th>
  )
}

function Td({
  children,
  right,
  mono,
}: {
  children: ReactNode
  right?: boolean
  mono?: boolean
}) {
  return (
    <td
      className={`whitespace-nowrap px-3 py-0 text-[13px] text-fg ${right ? 'text-right' : 'text-left'} ${mono || right ? 'num' : ''}`}
    >
      {children}
    </td>
  )
}

// --- Depth table (future / spot lists) ---

function DepthListTable({
  rows,
  language,
  defaultPageSize,
}: {
  rows: DepthRow[]
  language: 'en' | 'zh' | 'id'
  defaultPageSize: number
}) {
  const paged = usePaged(rows, defaultPageSize)
  return (
    <>
      <table className="w-full border-collapse text-[13px]">
        <thead>
          <tr>
            <Th>#</Th>
            <Th>{t('dataPage.symbol', language)}</Th>
            <Th right>{t('dataPage.bidVol', language)}</Th>
            <Th right>{t('dataPage.askVol', language)}</Th>
            <Th right>{t('dataPage.delta', language)}</Th>
            <Th right>{t('dataPage.price', language)}</Th>
            <Th right>24h</Th>
          </tr>
        </thead>
        <tbody>
          {paged.paged.map((row, i) => (
            <TableRow key={`${row.symbol}-${i}`}>
              <Td>
                <RankBadge rank={(paged.page - 1) * paged.pageSize + i + 1} />
              </Td>
              <Td mono>{row.symbol}</Td>
              <Td right mono>
                {fmtUsd(row.bid_volume)}
              </Td>
              <Td right mono>
                {fmtUsd(row.ask_volume)}
              </Td>
              <Td right mono>
                <span className={pctColorCls(row.delta)}>
                  {fmtUsd(row.delta)}
                </span>
              </Td>
              <Td right mono>
                {fmtPrice(row.price)}
              </Td>
              <Td right>
                <span className={pctColorCls(row.price_delta_percent)}>
                  {fmtPctDirect(row.price_delta_percent)}
                </span>
              </Td>
            </TableRow>
          ))}
        </tbody>
      </table>
      <Pagination
        page={paged.page}
        totalPages={paged.totalPages}
        pageSize={paged.pageSize}
        total={rows.length}
        onPage={paged.setPage}
        onPageSize={paged.setPageSize}
        language={language}
      />
    </>
  )
}

function DepthTable({
  data,
  language,
}: {
  data: CryptoResponse | null
  language: 'en' | 'zh' | 'id'
}) {
  const future = data?.future || []
  const spot = data?.spot || []

  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3">
      {!data ? (
        <table className="w-full border-collapse text-[13px]">
          <tbody>
            <LoadingRow cols={7} />
          </tbody>
        </table>
      ) : (
        <>
          <div className="mb-2 flex items-center gap-2">
            <Badge variant="info">{t('dataPage.futuresDepth', language)}</Badge>
          </div>
          <DepthListTable
            rows={future}
            language={language}
            defaultPageSize={10}
          />
          <div className="mb-2 mt-4 flex items-center gap-2">
            <Badge variant="ai">{t('dataPage.spotDepth', language)}</Badge>
          </div>
          <DepthListTable
            rows={spot}
            language={language}
            defaultPageSize={10}
          />
        </>
      )}
    </div>
  )
}

// --- Hyperliquid universe table ---

function HLTable({
  category,
  language,
}: {
  category: string
  language: 'en' | 'zh' | 'id'
}) {
  const path =
    category === 'preipo'
      ? '/api/trending/hl?category=other&sub=preipo'
      : `/api/trending/hl?category=${category}`
  const { data } = useTrendingData<HLResponse>(path)
  const rows = data?.rows || []
  const [search, setSearch] = useState('')
  const filtered = useMemo(
    () =>
      rows.filter((r) =>
        search ? r.base.toLowerCase().includes(search.toLowerCase()) : true
      ),
    [rows, search]
  )
  const sorted = useMemo(
    () => [...filtered].sort((a, b) => (b.volume24h || 0) - (a.volume24h || 0)),
    [filtered]
  )
  const paged = usePaged(sorted, 20)

  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3">
      <div className="flex items-center justify-between mb-3 flex-wrap gap-2">
        <input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder={t('dataPage.searchSymbol', language)}
          className="h-8 w-48 rounded-md border border-line bg-surface-2 px-3 text-[13px] text-fg outline-none placeholder:text-fg-3 hover:border-line-strong focus:border-brand focus:ring-1 focus:ring-brand/40"
        />
        {data?.generatedAt && (
          <span className="text-[11px] text-fg-3">
            {new Date(data.generatedAt).toLocaleTimeString()}
          </span>
        )}
      </div>
      {!data ? (
        <table className="w-full border-collapse text-[13px]">
          <tbody>
            <LoadingRow cols={8} />
          </tbody>
        </table>
      ) : sorted.length === 0 ? (
        <div className="text-xs py-6 text-center text-fg-3">
          {t('dataPage.noData', language)}
        </div>
      ) : (
        <>
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-[13px]">
              <thead>
                <tr>
                  <Th>#</Th>
                  <Th>{t('dataPage.symbol', language)}</Th>
                  <Th right>{t('dataPage.price', language)}</Th>
                  <Th right>24h</Th>
                  <Th right>{t('dataPage.funding', language)}</Th>
                  <Th right>{t('dataPage.volume24h', language)}</Th>
                  <Th right>{t('dataPage.openInterest', language)}</Th>
                  <Th right>{t('dataPage.maxLeverage', language)}</Th>
                </tr>
              </thead>
              <tbody>
                {paged.paged.map((row, i) => (
                  <TableRow key={row.symbol}>
                    <Td>
                      <RankBadge
                        rank={(paged.page - 1) * paged.pageSize + i + 1}
                      />
                    </Td>
                    <Td>
                      <span className="font-semibold text-fg">{row.base}</span>
                      <span className="ml-1 text-[11px] text-fg-3">
                        /{row.quote}
                      </span>
                    </Td>
                    <Td right mono>
                      {fmtPrice(row.lastPrice)}
                    </Td>
                    <Td right>
                      <span className={pctColorCls(row.change24hPct)}>
                        {fmtPctDirect(row.change24hPct)}
                      </span>
                    </Td>
                    <Td right>
                      <span className={pctColorCls(row.funding8h)}>
                        {(row.funding8h * 100).toFixed(4)}%
                      </span>
                    </Td>
                    <Td right mono>
                      {fmtUsd(row.volume24h)}
                    </Td>
                    <Td right mono>
                      {fmtUsd(row.openInterest)}
                    </Td>
                    <Td right mono>
                      {row.maxLeverage}x
                    </Td>
                  </TableRow>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination
            page={paged.page}
            totalPages={paged.totalPages}
            pageSize={paged.pageSize}
            total={sorted.length}
            onPage={paged.setPage}
            onPageSize={paged.setPageSize}
            language={language}
          />
        </>
      )}
    </div>
  )
}

const CRYPTO_TABS: {
  key: CryptoTab
  labelKey: string
  hasDuration: boolean
}[] = [
  { key: 'net_flow', labelKey: 'dataPage.tabNetFlow', hasDuration: true },
  { key: 'oi', labelKey: 'dataPage.tabOI', hasDuration: true },
  { key: 'depth', labelKey: 'dataPage.tabDepth', hasDuration: false },
  { key: 'rates', labelKey: 'dataPage.tabRates', hasDuration: false },
  { key: 'price', labelKey: 'dataPage.tabPrice', hasDuration: true },
]

// --- Net flow table (vergex /flow/markets — same source as the official card) ---

function normalizeFlowRow(r: Record<string, any>): FlowRow {
  return {
    key: String(r.key ?? ''),
    marketType: String(r.marketType ?? ''),
    symbol: String(r.symbol ?? ''),
    netFlow: Number(r.netFlow),
    latestPrice: Number(r.latestPrice),
    priceChangePct: Number(r.priceChangePct),
    trades: r.trades,
  }
}

function FlowBar({ value, max }: { value: number; max: number }) {
  const pct = max > 0 ? Math.min(100, (Math.abs(value) / max) * 100) : 0
  return (
    <span
      className={`absolute right-0 top-1/2 h-4 -translate-y-1/2 rounded-sm ${value >= 0 ? 'bg-up-soft' : 'bg-down-soft'}`}
      style={{ width: `${Math.max(2, pct)}%` }}
    />
  )
}

function FlowTable({
  flowData,
  failed,
  duration,
  language,
}: {
  flowData: FlowResponse | null
  failed: boolean
  duration: Duration
  language: 'en' | 'zh' | 'id'
}) {
  const inflow = useMemo(
    () =>
      (flowData?.data?.inflow ?? [])
        .map(normalizeFlowRow)
        .filter((r) => !!r.symbol),
    [flowData]
  )
  const outflow = useMemo(
    () =>
      (flowData?.data?.outflow ?? [])
        .map(normalizeFlowRow)
        .filter((r) => !!r.symbol),
    [flowData]
  )
  const maxFlow = useMemo(
    () =>
      Math.max(
        1,
        ...inflow.map((r) => Math.abs(r.netFlow)),
        ...outflow.map((r) => Math.abs(r.netFlow))
      ),
    [inflow, outflow]
  )
  // Paginate both lists in lockstep.
  const paged = usePaged(inflow, 10)
  const lowSlice = outflow.slice(
    (paged.page - 1) * paged.pageSize,
    paged.page * paged.pageSize
  )
  const rankAt = (i: number) => (paged.page - 1) * paged.pageSize + i + 1
  const changeHeader = `${duration} ${t('dataPage.priceChange', language)}`
  const flowHeader = `${duration} ${t('dataPage.netFlow', language)}`

  const renderRow = (r: FlowRow, globalRank: number) => (
    <>
      <Td>
        <RankBadge rank={globalRank} />
      </Td>
      <Td>
        <span className="font-semibold text-fg">{r.symbol}</span>
        {r.marketType === 'hip3_perp' && (
          <span className="ml-1.5 text-[11px] text-fg-3">HIP-3</span>
        )}
      </Td>
      <Td right mono>
        {fmtPrice(r.latestPrice)}
      </Td>
      <Td right>
        <span className={pctColorCls(r.priceChangePct)}>
          {fmtPctDirect(r.priceChangePct)}
        </span>
      </Td>
      <Td right mono>
        <span className="relative inline-block min-w-24 px-1 py-0.5">
          <FlowBar value={r.netFlow} max={maxFlow} />
          <span className={`relative ${pctColorCls(r.netFlow)}`}>
            {fmtUsd(r.netFlow)}
          </span>
        </span>
      </Td>
    </>
  )

  const header = (
    <tr>
      <Th>#</Th>
      <Th>{t('dataPage.symbol', language)}</Th>
      <Th right>{t('dataPage.price', language)}</Th>
      <Th right>{changeHeader}</Th>
      <Th right>{flowHeader}</Th>
    </tr>
  )

  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3">
      {!flowData ? (
        failed ? (
          <div className="py-10 text-center text-sm text-fg-3">
            {t('dataPage.flowUnavailable', language)}
          </div>
        ) : (
          <table className="w-full border-collapse text-[13px]">
            <tbody>
              <LoadingRow cols={5} />
            </tbody>
          </table>
        )
      ) : (
        <>
          <div className="mb-2 flex items-center gap-2">
            <Badge variant="up">▲ {t('dataPage.topInflow', language)}</Badge>
          </div>
          <table className="w-full border-collapse text-[13px]">
            <thead>{header}</thead>
            <tbody>
              {paged.paged.map((row, i) => (
                <TableRow key={row.key || i}>
                  {renderRow(row, rankAt(i))}
                </TableRow>
              ))}
            </tbody>
          </table>
          <div className="mb-2 mt-4 flex items-center gap-2">
            <Badge variant="down">
              ▼ {t('dataPage.bottomOutflow', language)}
            </Badge>
          </div>
          <table className="w-full border-collapse text-[13px]">
            <thead>{header}</thead>
            <tbody>
              {lowSlice.map((row, i) => (
                <TableRow key={row.key || i}>
                  {renderRow(row, rankAt(i))}
                </TableRow>
              ))}
            </tbody>
          </table>
          <Pagination
            page={paged.page}
            totalPages={paged.totalPages}
            pageSize={paged.pageSize}
            total={inflow.length + outflow.length}
            onPage={paged.setPage}
            onPageSize={paged.setPageSize}
            language={language}
          />
        </>
      )}
    </div>
  )
}

function CryptoSection({ language }: { language: 'en' | 'zh' | 'id' }) {
  const [tab, setTab] = useState<CryptoTab>('net_flow')
  const [duration, setDuration] = useState<Duration>('1h')
  // Net flow uses vergex's newer /flow endpoint which has no 30m window.
  const durations =
    tab === 'price'
      ? PRICE_DURATIONS
      : tab === 'net_flow'
        ? FLOW_DURATIONS
        : DURATIONS
  useEffect(() => {
    if (!durations.includes(duration)) setDuration('1h')
  }, [tab])

  const tabDef = CRYPTO_TABS.find((t) => t.key === tab)!
  const isFlowTab = tab === 'net_flow'
  const cryptoQuery = isFlowTab
    ? null
    : tabDef.hasDuration
      ? `/api/trending/crypto?tab=${tab}&duration=${duration}&limit=100`
      : `/api/trending/crypto?tab=${tab}&limit=100`
  const flowQuery = isFlowTab
    ? `/api/trending/flow?window=${duration}&limit=25`
    : null
  const { data } = useTrendingData<CryptoResponse>(cryptoQuery)
  const { data: flowData, failed: flowFailed } =
    useTrendingData<FlowResponse>(flowQuery)

  // Upstream quirk: with large limits the rates "low" list is front-loaded with
  // empty placeholder rows. Drop them and sort by funding rate like vergex does
  // (top: most positive first, low: most negative first).
  const ratesTop = useMemo(
    () =>
      ((data?.top ?? []) as RatesRow[])
        .filter((r) => !!r.symbol)
        .sort((a, b) => b.funding_rate - a.funding_rate),
    [data]
  )
  const ratesLow = useMemo(
    () =>
      ((data?.low ?? []) as RatesRow[])
        .filter((r) => !!r.symbol)
        .sort((a, b) => a.funding_rate - b.funding_rate),
    [data]
  )

  return (
    <div className="space-y-3">
      {/* Tab bar + duration selector */}
      <div className="flex items-center justify-between flex-wrap gap-2 border-b border-line">
        <Tabs
          className="border-b-0"
          items={CRYPTO_TABS.map((ct) => ({
            key: ct.key,
            label: t(ct.labelKey, language),
          }))}
          value={tab}
          onChange={(k) => setTab(k)}
        />
        {tabDef.hasDuration && (
          <Segmented
            size="sm"
            className="mb-1"
            items={durations.map((d) => ({ key: d, label: d }))}
            value={duration}
            onChange={(k) => setDuration(k)}
          />
        )}
      </div>

      {tab === 'depth' ? (
        <DepthTable data={data} language={language} />
      ) : tab === 'net_flow' ? (
        <FlowTable
          flowData={flowData}
          failed={flowFailed}
          duration={duration}
          language={language}
        />
      ) : tab === 'oi' ? (
        <TwoSidedTable
          language={language}
          cols={6}
          top={data?.top as OIRow[] | undefined}
          low={data?.low as OIRow[] | undefined}
          renderHeader={() => (
            <tr>
              <Th>#</Th>
              <Th>{t('dataPage.symbol', language)}</Th>
              <Th right>{t('dataPage.currentOI', language)}</Th>
              <Th right>{t('dataPage.oiChange', language)}</Th>
              <Th right>{t('dataPage.price', language)}</Th>
              <Th right>24h</Th>
            </tr>
          )}
          renderRow={(row, _isTop, globalRank) => {
            const r = row as OIRow
            return (
              <>
                <Td>
                  <RankBadge rank={globalRank} />
                </Td>
                <Td mono>{r.symbol}</Td>
                <Td right mono>
                  {fmtUsd(r.current_oi)}
                </Td>
                <Td right mono>
                  <span className={pctColorCls(r.oi_delta_percent)}>
                    {fmtPctDirect(r.oi_delta_percent)}
                  </span>
                  <span className="ml-1 text-fg-3">
                    ({fmtUsd(r.oi_delta_value)})
                  </span>
                </Td>
                <Td right mono>
                  {fmtPrice(r.price)}
                </Td>
                <Td right>
                  <span className={pctColorCls(r.price_delta_percent)}>
                    {fmtPctDirect(r.price_delta_percent)}
                  </span>
                </Td>
              </>
            )
          }}
        />
      ) : tab === 'rates' ? (
        <TwoSidedTable
          language={language}
          cols={6}
          top={ratesTop}
          low={ratesLow}
          renderHeader={() => (
            <tr>
              <Th>#</Th>
              <Th>{t('dataPage.symbol', language)}</Th>
              <Th right>{t('dataPage.fundingRate', language)}</Th>
              <Th right>{t('dataPage.markPrice', language)}</Th>
              <Th right>{t('dataPage.indexPrice', language)}</Th>
              <Th right>24h</Th>
            </tr>
          )}
          renderRow={(row, _isTop, globalRank) => {
            const r = row as RatesRow
            return (
              <>
                <Td>
                  <RankBadge rank={globalRank} />
                </Td>
                <Td mono>{r.symbol}</Td>
                <Td right mono>
                  <span className={pctColorCls(r.funding_rate)}>
                    {/* funding_rate is already in percent units upstream */}
                    {fmtRate4(r.funding_rate)}
                  </span>
                </Td>
                <Td right mono>
                  {fmtPrice(r.mark_price)}
                </Td>
                <Td right mono>
                  {fmtPrice(r.index_price)}
                </Td>
                <Td right>
                  <span className={pctColorCls(r.price_delta_percent)}>
                    {fmtPctDirect(r.price_delta_percent)}
                  </span>
                </Td>
              </>
            )
          }}
        />
      ) : (
        <TwoSidedTable
          language={language}
          cols={6}
          top={data?.top as PriceRow[] | undefined}
          low={data?.low as PriceRow[] | undefined}
          renderHeader={() => (
            <tr>
              <Th>#</Th>
              <Th>{t('dataPage.symbol', language)}</Th>
              <Th right>{t('dataPage.price', language)}</Th>
              <Th right>{t('dataPage.priceChange', language)}</Th>
              <Th right>{t('dataPage.futureFlow', language)}</Th>
              <Th right>{t('dataPage.oiChange', language)}</Th>
            </tr>
          )}
          renderRow={(_row, _isTop, globalRank) => {
            const r = _row as PriceRow
            return (
              <>
                <Td>
                  <RankBadge rank={globalRank} />
                </Td>
                <Td mono>{r.pair}</Td>
                <Td right mono>
                  {fmtPrice(r.price)}
                </Td>
                <Td right>
                  <span className={pctColorCls(r.price_delta)}>
                    {fmtPct(r.price_delta)}
                  </span>
                </Td>
                <Td right mono>
                  <span className={pctColorCls(r.future_flow)}>
                    {fmtUsd(r.future_flow)}
                  </span>
                </Td>
                <Td right mono>
                  <span className={pctColorCls(r.oi_delta_value)}>
                    {fmtUsd(r.oi_delta_value)}
                  </span>
                </Td>
              </>
            )
          }}
        />
      )}
    </div>
  )
}

function HLSection({ language }: { language: 'en' | 'zh' | 'id' }) {
  const [category, setCategory] = useState<string>('crypto')
  return (
    <div className="space-y-3">
      <Segmented
        items={HL_CATEGORIES.map((hc) => ({
          key: hc.key,
          label: t(hc.labelKey, language),
        }))}
        value={category}
        onChange={(k) => setCategory(k)}
      />
      <HLTable category={category} language={language} />
    </div>
  )
}

// --- Breakout/breakdown engine ranking (猪猪冲刺 engine, 5-min refresh) ---

interface BreakoutRow {
  symbol: string
  price: number
  direction: string
  score: number
  grade: string
  resonance: boolean
  level: number
  level_source: string
  pattern?: string
  confluence?: number
  room_atr?: number
  percentile?: number
  regime?: string
  spread_pct?: number
}

function gradeColor(grade: string): string {
  switch (grade) {
    case 'strong':
      return 'text-up'
    case 'medium':
      return 'text-brand'
    default:
      return 'text-fg-3'
  }
}

type BadgeVariant = 'up' | 'down' | 'brand' | 'info' | 'neutral'

function directionBadge(
  dir: string,
  language: 'en' | 'zh' | 'id'
): { label: string; variant: BadgeVariant } {
  if (dir === 'breakout') {
    return {
      label: language === 'zh' ? '突破' : 'Breakout',
      variant: 'up',
    }
  }
  return {
    label: language === 'zh' ? '跌破' : 'Breakdown',
    variant: 'down',
  }
}

function BreakoutSection({ language }: { language: 'en' | 'zh' | 'id' }) {
  const { data } = useTrendingData<{
    generated_at?: string
    count: number
    results: BreakoutRow[]
    warming_up?: boolean
  }>('/api/breakout/snapshot', 60_000)
  const rows = data?.results || []
  const sorted = useMemo(
    () => [...rows].sort((a, b) => b.score - a.score),
    [rows]
  )
  const paged = usePaged(sorted, 10)

  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="text-sm font-semibold text-fg">
            {t('dataPage.breakoutRanking', language)}
          </span>
          {data?.generated_at && (
            <span className="num text-xs text-fg-3">
              {new Date(data.generated_at).toLocaleTimeString()}
            </span>
          )}
        </div>
        <div className="flex items-center gap-2">
          {rows.length > 0 && rows[0].regime && (
            <Badge
              size="xs"
              variant={
                rows[0].regime === 'btc_bull'
                  ? 'up'
                  : rows[0].regime === 'btc_bear'
                    ? 'down'
                    : 'neutral'
              }
            >
              {t(`dataPage.regime_${rows[0].regime}`, language)}
            </Badge>
          )}
          <span className="text-xs text-fg-3">
            {t('dataPage.breakoutNote', language)}
          </span>
        </div>
      </div>

      {!data ? (
        <table className="w-full border-collapse text-[13px]">
          <tbody>
            <LoadingRow cols={9} />
          </tbody>
        </table>
      ) : data.warming_up || sorted.length === 0 ? (
        <div className="text-xs py-6 text-center text-fg-3">
          {t('dataPage.noData', language)}
        </div>
      ) : (
        <>
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-[13px]">
              <thead>
                <tr>
                  <Th>#</Th>
                  <Th>{t('dataPage.symbol', language)}</Th>
                  <Th>{t('dataPage.direction', language)}</Th>
                  <Th right>{t('dataPage.score', language)}</Th>
                  <Th right>{t('dataPage.grade', language)}</Th>
                  <Th right>{t('dataPage.pattern', language)}</Th>
                  <Th right>{t('dataPage.percentile', language)}</Th>
                  <Th right>{t('dataPage.keyLevel', language)}</Th>
                  <Th right>{t('dataPage.price', language)}</Th>
                </tr>
              </thead>
              <tbody>
                {paged.paged.map((row, i) => {
                  const badge = directionBadge(row.direction, language)
                  return (
                    <TableRow key={row.symbol}>
                      <Td>
                        <RankBadge
                          rank={(paged.page - 1) * paged.pageSize + i + 1}
                        />
                      </Td>
                      <Td mono>{row.symbol}</Td>
                      <Td>
                        <Badge variant={badge.variant} size="xs">
                          {badge.label}
                        </Badge>
                        {row.resonance && (
                          <Badge
                            variant="brand"
                            size="xs"
                            className="ml-1.5"
                            title={t('dataPage.resonance', language)}
                          >
                            ⚡
                          </Badge>
                        )}
                      </Td>
                      <Td right mono>
                        <span
                          className={`font-semibold ${gradeColor(row.grade)}`}
                        >
                          {row.score.toFixed(1)}
                        </span>
                      </Td>
                      <Td right>
                        <span className={gradeColor(row.grade)}>
                          {t(`dataPage.grade_${row.grade}`, language)}
                        </span>
                      </Td>
                      <Td right>
                        <Badge
                          size="xs"
                          variant={
                            row.pattern === 'retest_hold'
                              ? 'up'
                              : row.pattern === 'breakout'
                                ? 'info'
                                : 'neutral'
                          }
                        >
                          {t(
                            `dataPage.pattern_${row.pattern || 'approach'}`,
                            language
                          )}
                        </Badge>
                      </Td>
                      <Td right mono>
                        {row.percentile?.toFixed(0) ?? '-'}
                      </Td>
                      <Td right mono>
                        {row.level > 0 ? row.level.toFixed(4) : '-'}
                        <span className="ml-1 text-[9px] text-fg-3">
                          {row.level_source}
                        </span>
                      </Td>
                      <Td right mono>
                        {fmtPrice(row.price)}
                      </Td>
                    </TableRow>
                  )
                })}
              </tbody>
            </table>
          </div>
          <Pagination
            page={paged.page}
            totalPages={paged.totalPages}
            pageSize={paged.pageSize}
            total={sorted.length}
            onPage={paged.setPage}
            onPageSize={paged.setPageSize}
            language={language}
          />
        </>
      )}
    </div>
  )
}

interface ShortScanRow {
  symbol: string
  price: number
  change_24h_pct: number
  funding_annualized_pct: number
  long_short_ratio?: number
  oi_value_millions: number
  bearish_divergence_4h: boolean
  fake_breakout: boolean
  ma20_break: boolean
  funding_rollover: boolean
  confirmed: boolean
  score: number
  percentile?: number
  grade: string
  universe?: string
  btc_regime?: string
}

function shortUniverseBadge(
  universe: string | undefined,
  language: 'en' | 'zh' | 'id'
): { label: string; variant: BadgeVariant } {
  if (universe === 'near_high') {
    return {
      label: t('dataPage.universe_near_high', language),
      variant: 'down',
    }
  }
  if (universe === 'hist_gainer') {
    return {
      label: t('dataPage.universe_hist_gainer', language),
      variant: 'brand',
    }
  }
  return { label: t('dataPage.universe_gainer', language), variant: 'up' }
}

function ShortScanSection({ language }: { language: 'en' | 'zh' | 'id' }) {
  const { data } = useTrendingData<{
    generated_at?: string
    count: number
    results: ShortScanRow[]
  }>('/api/breakout/short-scan?limit=25', 60_000)
  const rows = data?.results || []
  const sorted = useMemo(
    () => [...rows].sort((a, b) => b.score - a.score),
    [rows]
  )
  const paged = usePaged(sorted, 10)

  const confirmChip = (label: string, on: boolean) =>
    on ? (
      <Badge key={label} variant="down" size="xs" className="mr-1">
        {label}
      </Badge>
    ) : null

  return (
    <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3">
      <div className="flex items-center justify-between mb-3 flex-wrap gap-2">
        <div className="flex items-center gap-2">
          <span className="text-sm font-semibold text-fg">
            {t('dataPage.shortScanRanking', language)}
          </span>
          {data?.generated_at && (
            <span className="text-[11px] text-fg-3">
              {new Date(data.generated_at).toLocaleTimeString()}
            </span>
          )}
        </div>
        <span className="text-[11px] text-fg-3">
          {t('dataPage.shortScanNote', language)}
        </span>
      </div>

      {!data ? (
        <table className="w-full border-collapse text-[13px]">
          <tbody>
            <LoadingRow cols={9} />
          </tbody>
        </table>
      ) : sorted.length === 0 ? (
        <div className="text-xs py-6 text-center text-fg-3">
          {t('dataPage.noData', language)}
        </div>
      ) : (
        <>
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-[13px]">
              <thead>
                <tr>
                  <Th>#</Th>
                  <Th>{t('dataPage.symbol', language)}</Th>
                  <Th>{t('dataPage.universe', language)}</Th>
                  <Th right>{t('dataPage.score', language)}</Th>
                  <Th right>{t('dataPage.grade', language)}</Th>
                  <Th>{t('dataPage.confirmed', language)}</Th>
                  <Th right>{t('dataPage.fundingAnn', language)}</Th>
                  <Th right>{t('dataPage.lsRatio', language)}</Th>
                  <Th right>{t('dataPage.oiValue', language)}</Th>
                  <Th right>{t('dataPage.price', language)}</Th>
                </tr>
              </thead>
              <tbody>
                {paged.paged.map((row, i) => {
                  const u = shortUniverseBadge(row.universe, language)
                  const chips = [
                    confirmChip(
                      language === 'zh' ? '顶背离' : 'DIV',
                      row.bearish_divergence_4h
                    ),
                    confirmChip(
                      language === 'zh' ? '假突破' : 'Fake',
                      row.fake_breakout
                    ),
                    confirmChip(
                      language === 'zh' ? '破EMA20' : 'EMA20',
                      row.ma20_break
                    ),
                    confirmChip(
                      language === 'zh' ? '费率回落' : 'Fund',
                      row.funding_rollover
                    ),
                  ].filter(Boolean)
                  return (
                    <TableRow key={`${row.symbol}-${i}`}>
                      <Td>
                        <RankBadge
                          rank={(paged.page - 1) * paged.pageSize + i + 1}
                        />
                      </Td>
                      <Td mono>{row.symbol}</Td>
                      <Td>
                        <Badge variant={u.variant} size="xs">
                          {u.label}
                        </Badge>
                      </Td>
                      <Td right mono>
                        <span
                          className={`font-semibold ${gradeColor(row.grade)}`}
                        >
                          {row.score.toFixed(1)}
                        </span>
                      </Td>
                      <Td right>
                        <span className={gradeColor(row.grade)}>
                          {t(`dataPage.grade_${row.grade}`, language)}
                        </span>
                      </Td>
                      <Td>
                        {chips.length > 0 ? (
                          chips
                        ) : (
                          <span className="text-[11px] text-fg-3">-</span>
                        )}
                      </Td>
                      <Td right mono>
                        <span
                          className={
                            row.funding_annualized_pct >= 30
                              ? 'text-down'
                              : 'text-fg'
                          }
                        >
                          {row.funding_annualized_pct.toFixed(1)}%
                        </span>
                      </Td>
                      <Td right mono>
                        {row.long_short_ratio
                          ? row.long_short_ratio.toFixed(2)
                          : '-'}
                      </Td>
                      <Td right mono>
                        {row.oi_value_millions > 0
                          ? `$${row.oi_value_millions.toFixed(1)}M`
                          : '-'}
                      </Td>
                      <Td right mono>
                        {fmtPrice(row.price)}
                      </Td>
                    </TableRow>
                  )
                })}
              </tbody>
            </table>
          </div>
          <Pagination
            page={paged.page}
            totalPages={paged.totalPages}
            pageSize={paged.pageSize}
            total={sorted.length}
            onPage={paged.setPage}
            onPageSize={paged.setPageSize}
            language={language}
          />
        </>
      )}
    </div>
  )
}

type TopSection = 'crypto' | 'hl' | 'breakout' | 'shortscan'

export function DataPage() {
  const { language } = useLanguage()
  const [section, setSection] = useState<TopSection>('crypto')
  useVergexRelaySync()

  return (
    <div className="w-full min-h-[calc(100vh-52px)] px-4 md:px-6 py-4">
      <div className="max-w-7xl mx-auto space-y-4">
        {/* Page title + top-level section switch */}
        <div className="flex items-center justify-between flex-wrap gap-3">
          <h2 className="text-xl font-semibold text-fg">
            {t('dataCenter', language)}
          </h2>
        </div>
        {/* Featured picks */}
        <div className="grid grid-cols-1 lg:grid-cols-2 items-start gap-3">
          <FeaturedCard
            cacheKey="ai500"
            titleKey="dataPage.ai500"
            accent="var(--up)"
            language={language}
          />
          <FeaturedCard
            cacheKey="prediction"
            titleKey="dataPage.prediction"
            accent="var(--info)"
            language={language}
          />
        </div>

        <Tabs
          items={[
            { key: 'crypto', label: t('dataPage.cryptoTrending', language) },
            { key: 'hl', label: t('dataPage.hlUniverse', language) },
            { key: 'breakout', label: t('dataPage.breakoutRanking', language) },
            {
              key: 'shortscan',
              label: t('dataPage.shortScanRanking', language),
            },
          ]}
          value={section}
          onChange={(k) => setSection(k)}
        />

        {/* Data sections */}
        {section === 'crypto' ? (
          <CryptoSection language={language} />
        ) : section === 'hl' ? (
          <HLSection language={language} />
        ) : section === 'shortscan' ? (
          <ShortScanSection language={language} />
        ) : (
          <BreakoutSection language={language} />
        )}

        <div className="text-center text-[11px] pb-4 text-fg-3">
          {t('dataPage.sourceNote', language)}
        </div>
      </div>
    </div>
  )
}
