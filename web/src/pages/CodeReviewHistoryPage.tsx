import {
  isValidElement,
  useCallback,
  useEffect,
  useMemo,
  useState,
} from 'react'
import type { ComponentProps, ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { Components } from 'react-markdown'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Segmented,
  Skeleton,
  Stat,
} from '../components/ui'
import { api } from '../lib/api'
import type {
  CodeReviewDocContent,
  CodeReviewEntry,
  CodeReviewKind,
  CodeReviewManifest,
  CodeReviewResolutionStatus,
} from '../lib/api/codeReviews'

const L = (zh: string, en: string, language: string) =>
  language === 'zh' ? zh : en

// ---------------------------------------------------------------------------
// 文档内链接 / 图片地址改写（纯函数，便于测试）
// ---------------------------------------------------------------------------

/** 本机仓库根的两种形态（git worktree / 主仓库检出） */
const LOCAL_REPO_ROOTS = [
  // worktree：标记后还跟一层 worktree 目录名，需要一并跳过
  { marker: '/.claude/worktrees/', skipNameSegment: true },
  // 主仓库检出：标记本身就是仓库根
  { marker: '/workspace/nofx/', skipNameSegment: false },
]
/** 末尾行号后缀：`:493` 或 `:10-20` */
const LINE_SUFFIX_RE = /:(\d+)(?:-(\d+))?$/

function githubSlug(repoUrl: string): string | null {
  const m = /^https?:\/\/github\.com\/([^/]+)\/([^/]+?)(?:\.git)?\/*$/.exec(
    repoUrl
  )
  return m ? `${m[1]}/${m[2]}` : null
}

function repoBlobUrl(repoUrl: string, repoRelPath: string): string {
  return `${repoUrl}/blob/dev/${repoRelPath}`
}

/** 图片等内容地址走 raw.githubusercontent.com（owner/repo 从 repoUrl 推出） */
function repoRawUrl(repoUrl: string, repoRelPath: string): string | undefined {
  const slug = githubSlug(repoUrl)
  return slug
    ? `https://raw.githubusercontent.com/${slug}/dev/${repoRelPath}`
    : undefined
}

/** 把末尾 `:493` / `:10-20` 从路径上摘下来，转成 GitHub 的 #L 锚点 */
function splitLineSuffix(path: string): { path: string; anchor: string } {
  const m = LINE_SUFFIX_RE.exec(path)
  if (!m) return { path, anchor: '' }
  return {
    path: path.slice(0, m.index),
    anchor: m[2] ? `#L${m[1]}-L${m[2]}` : `#L${m[1]}`,
  }
}

/** 简易 path 归一：处理 `.` / `..`；base 相对仓库根，越出仓库根返回 null */
function normalizeRelative(base: string[], rel: string): string[] | null {
  const parts = [...base]
  for (const seg of rel.split('/')) {
    if (seg === '' || seg === '.') continue
    if (seg === '..') {
      if (parts.length === 0) return null
      parts.pop()
    } else {
      parts.push(seg)
    }
  }
  return parts
}

/**
 * 把文档里的链接/图片地址改写成 GitHub 外链：
 * - `http(s):`、`mailto:`、`#锚点` → 原样
 * - 本机绝对路径（worktree / 主仓库）→ 截取仓库根之后的部分，行号转 #L 锚点
 * - 其他相对路径 → 相对该文档所在目录 docs/<dirname(docPath)> 解析
 * - 解析失败或越出 docs 根 → undefined（调用方渲染成纯文本）
 */
export function resolveDocHref(
  href: string | undefined,
  docPath: string,
  repoUrl: string,
  style: 'blob' | 'raw' = 'blob'
): string | undefined {
  if (!href) return undefined
  if (href.startsWith('#')) return href
  if (/^https?:\/\//i.test(href) || href.startsWith('mailto:')) return href

  const build = (repoRel: string) =>
    style === 'raw'
      ? repoRawUrl(repoUrl, repoRel)
      : repoBlobUrl(repoUrl, repoRel)

  // 1) 本机绝对路径：匹配仓库根标记最后一次出现，取其后剩余部分
  if (href.startsWith('/')) {
    let rootIdx = -1
    let matched: (typeof LOCAL_REPO_ROOTS)[number] | null = null
    for (const root of LOCAL_REPO_ROOTS) {
      const idx = href.lastIndexOf(root.marker)
      if (idx > rootIdx) {
        rootIdx = idx
        matched = root
      }
    }
    if (!matched || rootIdx < 0) return undefined
    let after = href.slice(rootIdx + matched.marker.length)
    if (matched.skipNameSegment) {
      const slash = after.indexOf('/')
      if (slash < 0) return undefined // 只有 worktree 名，没有仓库内路径
      after = after.slice(slash + 1)
    }
    const { path, anchor } = splitLineSuffix(after)
    if (!path) return undefined
    const url = build(path)
    return url ? url + anchor : undefined
  }

  // 2) 相对路径：相对该文档所在目录 docs/<dirname(docPath)> 解析
  const dirEnd = Math.max(docPath.lastIndexOf('/'), 0)
  const dir = docPath.slice(0, dirEnd)
  const base = ['docs', ...(dir ? dir.split('/') : [])]
  const { path, anchor } = splitLineSuffix(href)
  const parts = normalizeRelative(base, path)
  if (!parts || parts.length === 0) return undefined
  const url = build(parts.join('/'))
  return url ? url + anchor : undefined
}

/** 与 GitHub 标题锚点一致的 slug：小写、去标点、空白转 `-` */
export function headingSlug(text: string): string {
  return text
    .trim()
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\s_-]/gu, '')
    .replace(/\s/g, '-')
}

function nodeText(node: ReactNode): string {
  if (node == null || typeof node === 'boolean') return ''
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(nodeText).join('')
  if (isValidElement<{ children?: ReactNode }>(node))
    return nodeText(node.props.children)
  return ''
}

// ---------------------------------------------------------------------------
// 文案与展示元数据
// ---------------------------------------------------------------------------

const KIND_ORDER: CodeReviewKind[] = [
  'fullstack',
  'quant',
  'execution',
  'module',
  'param',
  'candidates',
  'recheck',
]

const KIND_META: Record<string, { zh: string; en: string }> = {
  fullstack: { zh: '全栈', en: 'Full-stack' },
  quant: { zh: '量化', en: 'Quant' },
  execution: { zh: '执行链', en: 'Execution' },
  module: { zh: '模块', en: 'Module' },
  param: { zh: '参数迭代', en: 'Param iteration' },
  candidates: { zh: '候选链路', en: 'Candidates' },
  recheck: { zh: '复核', en: 'Recheck' },
}

function kindLabel(kind: string, language: string) {
  const meta = KIND_META[kind]
  return meta ? L(meta.zh, meta.en, language) : kind
}

const RESOLUTION_META: Record<
  CodeReviewResolutionStatus,
  { zh: string; en: string; variant: 'up' | 'warn' | 'down' | 'neutral' }
> = {
  fixed: { zh: '已修复', en: 'Fixed', variant: 'up' },
  partial: { zh: '部分修复', en: 'Partial', variant: 'warn' },
  open: { zh: '待处理', en: 'Open', variant: 'down' },
  na: { zh: '不适用', en: 'N/A', variant: 'neutral' },
}

const ROLE_META: Record<string, { zh: string; en: string }> = {
  report: { zh: '主报告', en: 'Report' },
  raw: { zh: '分方向报告', en: 'Direction report' },
  recheck: { zh: '复核', en: 'Recheck' },
  fixes: { zh: '修复记录', en: 'Fix log' },
  verification: { zh: '核实', en: 'Verification' },
  evidence: { zh: '证据', en: 'Evidence' },
}

function roleLabel(role: string, language: string) {
  const meta = ROLE_META[role]
  return meta ? L(meta.zh, meta.en, language) : role
}

function displayTitle(entry: CodeReviewEntry, language: string) {
  return language === 'zh' ? entry.title : entry.title_en || entry.title
}

function fmtCount(v: number | null | undefined) {
  return v == null ? '—' : String(v)
}

// ---------------------------------------------------------------------------
// Markdown 渲染（不渲染原始 HTML，全部走 Tailwind token）
// ---------------------------------------------------------------------------

/** 标题 id 加前缀，避免与页面其他元素的 id 冲突 */
const HEADING_ID_PREFIX = 'cr-'

function buildMarkdownComponents(docPath: string, repoUrl: string): Components {
  const renderLink = (href: string | undefined, children: ReactNode) => {
    const resolved = resolveDocHref(href, docPath, repoUrl)
    if (!resolved) {
      return <span className="break-all text-fg-2">{children}</span>
    }
    if (resolved.startsWith('#')) {
      // 页内锚点：只滚动，不改地址栏 hash（设置页用 hash 记当前 tab）
      return (
        <a
          href={resolved}
          onClick={(e) => {
            e.preventDefault()
            document
              .getElementById(
                `${HEADING_ID_PREFIX}${decodeURIComponent(resolved.slice(1))}`
              )
              ?.scrollIntoView({ block: 'start' })
          }}
          className="text-brand hover:underline"
        >
          {children}
        </a>
      )
    }
    return (
      <a
        href={resolved}
        target="_blank"
        rel="noopener noreferrer"
        className="break-all text-brand hover:underline"
      >
        {children}
      </a>
    )
  }

  const headingId = (children: ReactNode) =>
    `${HEADING_ID_PREFIX}${headingSlug(nodeText(children))}`

  return {
    h1: ({ children }) => (
      <h1
        id={headingId(children)}
        className="mb-3 mt-5 scroll-mt-20 border-b border-line pb-1.5 text-lg font-semibold text-fg first:mt-0"
      >
        {children}
      </h1>
    ),
    h2: ({ children }) => (
      <h2
        id={headingId(children)}
        className="mb-2 mt-5 scroll-mt-20 text-[15px] font-semibold text-fg first:mt-0"
      >
        {children}
      </h2>
    ),
    h3: ({ children }) => (
      <h3
        id={headingId(children)}
        className="mb-2 mt-4 scroll-mt-20 text-[14px] font-semibold text-fg first:mt-0"
      >
        {children}
      </h3>
    ),
    h4: ({ children }) => (
      <h4
        id={headingId(children)}
        className="mb-1.5 mt-3 scroll-mt-20 text-[13px] font-semibold text-fg first:mt-0"
      >
        {children}
      </h4>
    ),
    p: ({ children }) => (
      <p className="my-2 text-[13px] leading-relaxed text-fg-2">{children}</p>
    ),
    ul: ({ children }) => (
      <ul className="my-2 list-disc space-y-1 pl-5 text-[13px] leading-relaxed text-fg-2">
        {children}
      </ul>
    ),
    ol: ({ children }) => (
      <ol className="my-2 list-decimal space-y-1 pl-5 text-[13px] leading-relaxed text-fg-2">
        {children}
      </ol>
    ),
    li: ({ children }) => <li className="leading-relaxed">{children}</li>,
    table: ({ children }) => (
      <div className="my-3 overflow-x-auto">
        <table className="w-full border-collapse text-[12.5px]">
          {children}
        </table>
      </div>
    ),
    thead: ({ children }) => <thead className="bg-surface-2">{children}</thead>,
    th: ({ children }) => (
      <th className="whitespace-nowrap border-b border-line px-2.5 py-1.5 text-left text-xs font-medium text-fg-3">
        {children}
      </th>
    ),
    td: ({ children }) => (
      <td className="border-b border-line px-2.5 py-1.5 align-top text-fg-2">
        {children}
      </td>
    ),
    code: ({ className, children }) => {
      const isBlock =
        typeof className === 'string' && className.includes('language-')
      return isBlock ? (
        <code className={`${className} font-mono text-[12.5px] text-fg`}>
          {children}
        </code>
      ) : (
        <code className="rounded bg-surface-2 px-1 py-0.5 font-mono text-[12px] text-fg">
          {children}
        </code>
      )
    },
    pre: ({ children }) => (
      <pre className="overflow-x-auto rounded-lg border border-line bg-surface-2 p-3 font-mono text-[12.5px] leading-relaxed [&_code]:rounded-none [&_code]:bg-transparent [&_code]:px-0 [&_code]:py-0 [&_code]:text-[12.5px]">
        {children}
      </pre>
    ),
    blockquote: ({ children }) => (
      <blockquote className="my-2 border-l-2 border-line pl-3 text-[13px] leading-relaxed text-fg-3">
        {children}
      </blockquote>
    ),
    a: ({ href, children }: ComponentProps<'a'>) => renderLink(href, children),
    img: ({ src, alt }: ComponentProps<'img'>) => {
      const resolved = resolveDocHref(src, docPath, repoUrl, 'raw')
      if (!resolved) return null
      return (
        <img
          src={resolved}
          alt={alt ?? ''}
          loading="lazy"
          className="my-2 max-w-full rounded-lg border border-line"
        />
      )
    },
    hr: () => <hr className="my-4 border-line" />,
  }
}

// ---------------------------------------------------------------------------
// 页面
// ---------------------------------------------------------------------------

type KindFilter = 'all' | CodeReviewKind

function DetailRow({
  label,
  children,
}: {
  label: string
  children: ReactNode
}) {
  return (
    <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-[13px]">
      <span className="shrink-0 text-fg-3">{label}</span>
      <span className="min-w-0 text-fg-2">{children}</span>
    </div>
  )
}

export default function CodeReviewHistoryPage({
  language,
}: {
  language: string
}) {
  const [manifest, setManifest] = useState<CodeReviewManifest | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [kindFilter, setKindFilter] = useState<KindFilter>('all')
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [activeDocPath, setActiveDocPath] = useState<string | null>(null)
  const [doc, setDoc] = useState<CodeReviewDocContent | null>(null)
  const [docLoading, setDocLoading] = useState(false)
  const [docError, setDocError] = useState<string | null>(null)
  const [docRetry, setDocRetry] = useState(0)

  const load = useCallback(() => {
    setLoading(true)
    setError(null)
    api
      .getCodeReviews()
      .then((m) => {
        setManifest(m)
        setSelectedId(
          (prev) =>
            (prev && m.reviews.some((r) => r.id === prev) ? prev : null) ??
            m.reviews[0]?.id ??
            null
        )
        setLoading(false)
      })
      .catch((e: unknown) => {
        setError(e instanceof Error ? e.message : String(e))
        setLoading(false)
      })
  }, [])

  useEffect(() => {
    load()
  }, [load])

  const reviews = manifest?.reviews ?? []
  const selected = useMemo(
    () => reviews.find((r) => r.id === selectedId) ?? null,
    [reviews, selectedId]
  )
  const repoUrl = manifest?.repo_url || 'https://github.com/lemondy/nofx'

  // 当前要展示的文档：用户点选的，否则第一个 report（没有就第一个文档）
  const target = useMemo(
    () =>
      selected
        ? ((activeDocPath
            ? selected.docs.find((d) => d.path === activeDocPath)
            : undefined) ??
          selected.docs.find((d) => d.role === 'report') ??
          selected.docs[0] ??
          null)
        : null,
    [selected, activeDocPath]
  )

  useEffect(() => {
    if (!selected || !target) {
      setDoc(null)
      setDocError(null)
      setDocLoading(false)
      return
    }
    let alive = true
    setDocLoading(true)
    setDocError(null)
    api
      .getCodeReviewDoc(selected.id, target.path)
      .then((content) => {
        if (!alive) return
        setDoc(content)
        setDocLoading(false)
      })
      .catch((e: unknown) => {
        if (!alive) return
        setDoc(null)
        setDocError(e instanceof Error ? e.message : String(e))
        setDocLoading(false)
      })
    return () => {
      alive = false
    }
  }, [selected, target, docRetry])

  const stats = useMemo(() => {
    const sum = (get: (r: CodeReviewEntry) => number | null) =>
      reviews.reduce<number>((acc, r) => {
        const v = get(r)
        return v == null ? acc : acc + v
      }, 0)
    return {
      rounds: reviews.length,
      findingsTotal: sum((r) => r.findings.total),
      high: sum((r) => r.findings.high),
      fixed: sum((r) => r.resolution.fixed),
      open: sum((r) => r.resolution.open),
    }
  }, [reviews])

  const kindItems = useMemo(() => {
    const present = new Set(reviews.map((r) => r.kind as string))
    const ordered = KIND_ORDER.filter((k) => present.has(k))
    const extra = Array.from(present).filter(
      (k) => !KIND_ORDER.includes(k as CodeReviewKind)
    )
    return [...ordered, ...extra]
  }, [reviews])

  const visible = useMemo(
    () =>
      kindFilter === 'all'
        ? reviews
        : reviews.filter((r) => r.kind === kindFilter),
    [reviews, kindFilter]
  )

  const mdComponents = useMemo(
    () => buildMarkdownComponents(doc?.path ?? '', repoUrl),
    [doc?.path, repoUrl]
  )

  const selectEntry = (id: string) => {
    setSelectedId(id)
    setActiveDocPath(null)
  }

  // 过滤后当前选中条目不可见时，改选可见列表的第一条
  const changeKindFilter = (k: KindFilter) => {
    setKindFilter(k)
    const next = k === 'all' ? reviews : reviews.filter((r) => r.kind === k)
    if (!next.some((r) => r.id === selectedId) && next[0]) {
      selectEntry(next[0].id)
    }
  }

  return (
    <div>
      {/* 页头 */}
      <div className="mb-4 flex flex-wrap items-end justify-between gap-x-4 gap-y-1">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold text-fg">
            {L('代码 Review 历史', 'Code Review History', language)}
          </h1>
          <p className="mt-1 text-[13px] text-fg-3">
            {L(
              '历次代码审查的报告、复核与修复记录，按时间倒序。笔记内容为中文。',
              'Past code review reports, rechecks and fix records, newest first. Notes are written in Chinese.',
              language
            )}
          </p>
        </div>
        {manifest?.updated_at && (
          <div className="text-xs text-fg-3">
            {L('清单更新', 'Manifest updated', language)}
            {': '}
            <span className="num text-fg-2">{manifest.updated_at}</span>
          </div>
        )}
      </div>

      {/* 加载中 */}
      {loading && (
        <div className="space-y-3">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
            {[0, 1, 2, 3, 4].map((i) => (
              <Skeleton key={i} className="h-16 rounded-lg" />
            ))}
          </div>
          <div className="grid items-start gap-4 lg:grid-cols-[380px_1fr]">
            <div className="space-y-2">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-24 rounded-lg" />
              ))}
            </div>
            <Skeleton className="h-72 rounded-lg" />
          </div>
        </div>
      )}

      {/* 加载失败 */}
      {!loading && error && (
        <EmptyState
          title={L('加载失败', 'Failed to load', language)}
          description={error}
          action={
            <Button size="sm" onClick={load}>
              {L('重试', 'Retry', language)}
            </Button>
          }
        />
      )}

      {/* 空清单 */}
      {!loading && !error && reviews.length === 0 && (
        <EmptyState
          title={L('暂无代码 Review 记录', 'No code reviews yet', language)}
          description={L(
            '清单 docs/reviews/index.json 不存在或为空。',
            'The manifest docs/reviews/index.json does not exist or is empty.',
            language
          )}
        />
      )}

      {/* 主体 */}
      {!loading && !error && reviews.length > 0 && (
        <>
          {/* 统计卡片 */}
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
            <Card className="p-3">
              <Stat
                label={L('审查轮次', 'Reviews', language)}
                value={stats.rounds}
              />
            </Card>
            <Card className="p-3">
              <Stat
                label={L('确认问题总数', 'Confirmed findings', language)}
                value={stats.findingsTotal}
              />
            </Card>
            <Card className="p-3">
              <Stat
                label={L('高严重度总数', 'High severity', language)}
                value={stats.high}
              />
            </Card>
            <Card className="p-3">
              <Stat
                label={L('已修复总数', 'Fixed', language)}
                value={stats.fixed}
              />
            </Card>
            <Card className="p-3">
              <Stat
                label={L('待处理总数', 'Open', language)}
                value={stats.open}
              />
            </Card>
          </div>

          {/* kind 过滤 */}
          <Segmented<KindFilter>
            className="mb-2 mt-4"
            value={kindFilter}
            onChange={changeKindFilter}
            items={[
              { key: 'all', label: L('全部', 'All', language) },
              ...kindItems.map((k) => ({
                key: k as KindFilter,
                label: kindLabel(k, language),
              })),
            ]}
          />

          <div className="grid items-start gap-4 lg:grid-cols-[380px_1fr]">
            {/* 左栏列表（接口已按日期倒序，前端不再重排） */}
            <ul className="space-y-2">
              {visible.map((r) => {
                const active = r.id === selectedId
                const res = RESOLUTION_META[r.resolution.status]
                return (
                  <li key={r.id}>
                    <button
                      type="button"
                      onClick={() => selectEntry(r.id)}
                      aria-pressed={active}
                      className={`w-full rounded-lg border p-3 text-left transition-colors ${
                        active
                          ? 'border-brand bg-brand-soft'
                          : 'border-line bg-surface hover:border-line-strong hover:bg-surface-hover'
                      }`}
                    >
                      <div className="flex flex-wrap items-center gap-1.5 text-xs text-fg-3">
                        <span className="num">{r.date}</span>
                        <Badge variant="neutral" size="xs">
                          {kindLabel(r.kind, language)}
                        </Badge>
                        <Badge variant={res.variant} size="xs">
                          {L(res.zh, res.en, language)}
                        </Badge>
                      </div>
                      <h2 className="mt-1 text-[14px] font-semibold text-fg">
                        {displayTitle(r, language)}
                      </h2>
                      <div className="mt-1.5 flex flex-wrap gap-x-3 gap-y-0.5 text-[11px] text-fg-3">
                        <span>
                          {`${L('高', 'High', language)} ${fmtCount(r.findings.high)}`}
                        </span>
                        <span>
                          {`${L('中', 'Med', language)} ${fmtCount(r.findings.medium)}`}
                        </span>
                        <span>
                          {`${L('低', 'Low', language)} ${fmtCount(r.findings.low)}`}
                        </span>
                        <span>
                          {`${L('信息', 'Info', language)} ${fmtCount(r.findings.info)}`}
                        </span>
                      </div>
                    </button>
                  </li>
                )
              })}
            </ul>

            {/* 右栏详情 */}
            <div className="min-w-0">
              {selected && (
                <>
                  <Card className="p-4">
                    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                      <h2 className="text-base font-semibold text-fg">
                        {displayTitle(selected, language)}
                      </h2>
                      <span className="num text-xs text-fg-3">
                        {selected.date}
                      </span>
                      <Badge
                        variant={
                          RESOLUTION_META[selected.resolution.status].variant
                        }
                        size="xs"
                      >
                        {L(
                          RESOLUTION_META[selected.resolution.status].zh,
                          RESOLUTION_META[selected.resolution.status].en,
                          language
                        )}
                      </Badge>
                      <Badge variant="neutral" size="xs">
                        {kindLabel(selected.kind, language)}
                      </Badge>
                    </div>

                    {selected.scope.length > 0 && (
                      <div className="mt-2 flex flex-wrap gap-1.5">
                        {selected.scope.map((s) => (
                          <Badge key={s} variant="neutral" size="xs">
                            {s}
                          </Badge>
                        ))}
                      </div>
                    )}

                    {selected.baseline_commit && (
                      <p className="mt-2 text-[13px] text-fg-3">
                        {L('基线 commit', 'Baseline', language)}
                        {': '}
                        <a
                          href={`${repoUrl}/commit/${selected.baseline_commit}`}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="num text-brand hover:underline"
                        >
                          {selected.baseline_commit}
                        </a>
                      </p>
                    )}

                    <p className="mt-2 text-[13px] leading-relaxed text-fg-2">
                      {selected.summary}
                    </p>

                    <div className="mt-3 space-y-1.5 border-t border-line pt-3">
                      <DetailRow label={L('严重度', 'Severity', language)}>
                        <span>
                          {`${L('高', 'High', language)} ${fmtCount(selected.findings.high)}`}{' '}
                          ·{' '}
                          {`${L('中', 'Med', language)} ${fmtCount(selected.findings.medium)}`}{' '}
                          ·{' '}
                          {`${L('低', 'Low', language)} ${fmtCount(selected.findings.low)}`}{' '}
                          ·{' '}
                          {`${L('信息', 'Info', language)} ${fmtCount(selected.findings.info)}`}
                        </span>
                        {selected.findings.scheme && (
                          <span className="text-fg-3">
                            ({L('口径', 'scheme', language)}
                            {': '}
                            {selected.findings.scheme})
                          </span>
                        )}
                      </DetailRow>
                      <DetailRow label={L('修复状态', 'Resolution', language)}>
                        <span>
                          {`${L('已修复', 'Fixed', language)} ${fmtCount(selected.resolution.fixed)}`}{' '}
                          ·{' '}
                          {`${L('复核否定', 'Rejected', language)} ${fmtCount(selected.resolution.rejected)}`}{' '}
                          ·{' '}
                          {`${L('待处理', 'Open', language)} ${fmtCount(selected.resolution.open)}`}
                        </span>
                      </DetailRow>
                      {selected.commits.length > 0 && (
                        <DetailRow label={L('关联提交', 'Commits', language)}>
                          <span className="flex flex-wrap gap-1.5">
                            {selected.commits.map((sha) => (
                              <a
                                key={sha}
                                href={`${repoUrl}/commit/${sha}`}
                                target="_blank"
                                rel="noopener noreferrer"
                                className="num rounded bg-surface-2 px-1.5 py-0.5 text-[12px] text-brand transition-colors hover:bg-surface-hover"
                              >
                                {sha}
                              </a>
                            ))}
                          </span>
                        </DetailRow>
                      )}
                      {selected.prs.length > 0 && (
                        <DetailRow
                          label={L('关联 PR', 'Pull requests', language)}
                        >
                          <span className="flex flex-wrap gap-2">
                            {selected.prs.map((n) => (
                              <a
                                key={n}
                                href={`${repoUrl}/pull/${n}`}
                                target="_blank"
                                rel="noopener noreferrer"
                                className="text-brand hover:underline"
                              >
                                #{n}
                              </a>
                            ))}
                          </span>
                        </DetailRow>
                      )}
                    </div>
                  </Card>

                  <Card className="mt-4">
                    {selected.docs.length > 0 && (
                      <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2.5">
                        <span className="text-xs font-semibold text-fg-3">
                          {L('文档', 'Documents', language)}
                        </span>
                        {selected.docs.map((d) => {
                          const active = d.path === target?.path
                          return (
                            <button
                              key={d.path}
                              type="button"
                              onClick={() => setActiveDocPath(d.path)}
                              aria-pressed={active}
                              className={`inline-flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs transition-colors ${
                                active
                                  ? 'border-brand bg-brand-soft text-brand'
                                  : 'border-line text-fg-2 hover:border-line-strong hover:text-fg'
                              }`}
                            >
                              <Badge variant="neutral" size="xs">
                                {roleLabel(d.role, language)}
                              </Badge>
                              {d.title}
                            </button>
                          )
                        })}
                      </div>
                    )}
                    <div className="px-4 py-3">
                      {docLoading ? (
                        <div className="space-y-2 py-2">
                          <Skeleton className="h-4 w-1/3" />
                          <Skeleton className="h-3 w-full" />
                          <Skeleton className="h-3 w-11/12" />
                          <Skeleton className="h-3 w-4/5" />
                          <Skeleton className="h-24 w-full" />
                        </div>
                      ) : docError ? (
                        <div className="py-6 text-center">
                          <p className="text-[13px] text-down">
                            {L(
                              '文档加载失败',
                              'Failed to load document',
                              language
                            )}
                            {': '}
                            {docError}
                          </p>
                          <Button
                            size="sm"
                            className="mt-2"
                            onClick={() => setDocRetry((n) => n + 1)}
                          >
                            {L('重试', 'Retry', language)}
                          </Button>
                        </div>
                      ) : doc ? (
                        <div>
                          <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
                            <h3 className="text-[13px] font-semibold text-fg-2">
                              {doc.title}
                            </h3>
                            <a
                              href={`${repoUrl}/blob/dev/docs/${doc.path}`}
                              target="_blank"
                              rel="noopener noreferrer"
                              className="text-xs text-brand hover:underline"
                            >
                              {L('在 GitHub 查看', 'View on GitHub', language)}↗
                            </a>
                          </div>
                          <ReactMarkdown
                            remarkPlugins={[remarkGfm]}
                            components={mdComponents}
                          >
                            {doc.content}
                          </ReactMarkdown>
                        </div>
                      ) : (
                        <p className="py-6 text-center text-[13px] text-fg-3">
                          {L(
                            '该条审查没有可展示的文档',
                            'No documents for this review',
                            language
                          )}
                        </p>
                      )}
                    </div>
                  </Card>
                </>
              )}
            </div>
          </div>
        </>
      )}
    </div>
  )
}
