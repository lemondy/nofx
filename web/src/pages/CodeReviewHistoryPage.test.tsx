import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import CodeReviewHistoryPage, {
  headingSlug,
  resolveDocHref,
} from './CodeReviewHistoryPage'
import type { CodeReviewManifest } from '../lib/api/codeReviews'

const apiMock = vi.hoisted(() => ({
  getCodeReviews: vi.fn(),
  getCodeReviewDoc: vi.fn(),
}))
vi.mock('../lib/api', () => ({ api: apiMock }))

const REPO = 'https://github.com/lemondy/nofx'

const DOC_REPORT_1 = `# 量化系统全面审查

审查覆盖执行层、风控与 LLM 提示词。

| 模块 | 问题 | 严重度 |
| --- | --- | --- |
| 执行层 | 下单重试无上限 | P0 |
| 风控 | 单品种敞口超限未告警 | P1 |

关键代码见 \`trader/paper/paper.go:493\`。
`

/** 构造清单 fixture（每次调用返回全新对象，避免用例间共享引用） */
function buildManifest(): CodeReviewManifest {
  return {
    version: 1,
    updated_at: '2026-10-31',
    repo_url: REPO,
    reviews: [
      {
        id: '2026-10-28-quant-system',
        date: '2026-10-28',
        title: '量化系统全面审查',
        title_en: 'Quant system full review',
        kind: 'quant',
        scope: ['执行层', '风控'],
        baseline_commit: 'c7045ac4',
        summary: '审查了执行层、风控与提示词，确认 28 个问题，21 个已修复。',
        findings: {
          total: 28,
          high: 5,
          medium: 10,
          low: 8,
          info: 5,
          scheme: 'P0–P3',
        },
        resolution: { status: 'fixed', fixed: 21, rejected: 6, open: 1 },
        commits: ['6f1175e6', '2dbefb42'],
        prs: [36, 37],
        docs: [
          {
            path: 'reviews/2026-10-28/quant-system-review.md',
            title: '主报告',
            role: 'report',
          },
          {
            path: 'reviews/2026-10-28/evidence.md',
            title: '证据',
            role: 'evidence',
          },
        ],
      },
      {
        id: '2026-10-20-fullstack-risk',
        date: '2026-10-20',
        title: '全栈风控专项审查',
        title_en: 'Full-stack risk review',
        kind: 'fullstack',
        scope: ['风控', '下单链路'],
        baseline_commit: '',
        summary: '风控专项审查，部分问题已修复。',
        findings: {
          total: 12,
          high: 3,
          medium: 5,
          low: 3,
          info: 1,
          scheme: 'HIGH/MEDIUM',
        },
        resolution: { status: 'partial', fixed: 7, rejected: 2, open: 3 },
        commits: [],
        prs: [38],
        docs: [
          {
            path: 'reviews/2026-10-20/risk-review.md',
            title: '主报告',
            role: 'report',
          },
        ],
      },
      {
        id: '2026-09-13-execution-design',
        date: '2026-09-13',
        title: '执行链模块设计评审',
        title_en: 'Execution module design review',
        kind: 'execution',
        scope: ['执行链'],
        baseline_commit: 'a1b2c3d4',
        summary: '执行链模块设计评审，不涉及修复。',
        findings: {
          total: null,
          high: null,
          medium: null,
          low: null,
          info: 4,
          scheme: '字母编号 A–K',
        },
        resolution: { status: 'na', fixed: null, rejected: null, open: null },
        commits: ['deadbeef'],
        prs: [],
        docs: [
          {
            path: 'reviews/2026-09-13/execution-design.md',
            title: '主报告',
            role: 'report',
          },
        ],
      },
    ],
  }
}

beforeEach(() => {
  apiMock.getCodeReviews.mockImplementation(() =>
    Promise.resolve(buildManifest())
  )
  apiMock.getCodeReviewDoc.mockImplementation((_id: string, path: string) =>
    Promise.resolve({
      path,
      title: '主报告',
      role: 'report',
      content: DOC_REPORT_1,
    })
  )
})

afterEach(() => {
  vi.clearAllMocks()
})

describe('CodeReviewHistoryPage', () => {
  it('renders the manifest list in API order and auto-loads the first report', async () => {
    render(<CodeReviewHistoryPage language="zh" />)
    const list = await screen.findByRole('list')
    const titles = within(list)
      .getAllByRole('heading', { level: 2 })
      .map((h) => h.textContent)
    expect(titles).toEqual([
      '量化系统全面审查',
      '全栈风控专项审查',
      '执行链模块设计评审',
    ])
    // 默认选中第一条，并请求它的 report 文档（effect 在列表渲染后才发请求）
    await waitFor(() =>
      expect(apiMock.getCodeReviewDoc).toHaveBeenCalledWith(
        '2026-10-28-quant-system',
        'reviews/2026-10-28/quant-system-review.md'
      )
    )
    // Markdown（含 GFM 表格）渲染出 <table>
    expect(await screen.findByRole('table')).toBeInTheDocument()
    // 统计卡片：确认问题 28+12+null(跳过)=40
    expect(screen.getByText('40')).toBeInTheDocument()
  })

  it('loads the second entry when clicked in the list', async () => {
    render(<CodeReviewHistoryPage language="zh" />)
    const list = await screen.findByRole('list')
    const buttons = within(list).getAllByRole('button')
    expect(buttons).toHaveLength(3)
    fireEvent.click(buttons[1])
    await waitFor(() =>
      expect(apiMock.getCodeReviewDoc).toHaveBeenLastCalledWith(
        '2026-10-20-fullstack-risk',
        'reviews/2026-10-20/risk-review.md'
      )
    )
    // 详情切换到第二条（#38 只属于第二条）
    expect(await screen.findByText('#38')).toBeInTheDocument()
  })

  it('filters the list by kind', async () => {
    render(<CodeReviewHistoryPage language="zh" />)
    await screen.findByRole('list')
    fireEvent.click(screen.getByRole('button', { name: '全栈' }))
    const list = screen.getByRole('list')
    const titles = within(list)
      .getAllByRole('heading', { level: 2 })
      .map((h) => h.textContent)
    expect(titles).toEqual(['全栈风控专项审查'])
    // 原选中条目被过滤掉后，详情跟着切到可见的第一条
    await waitFor(() =>
      expect(apiMock.getCodeReviewDoc).toHaveBeenLastCalledWith(
        '2026-10-20-fullstack-risk',
        'reviews/2026-10-20/risk-review.md'
      )
    )
    // 切回全部
    fireEvent.click(screen.getByRole('button', { name: '全部' }))
    expect(
      within(screen.getByRole('list')).getAllByRole('heading', { level: 2 })
    ).toHaveLength(3)
  })

  it('shows the empty state for an empty manifest', async () => {
    apiMock.getCodeReviews.mockResolvedValueOnce({
      version: 0,
      updated_at: '',
      repo_url: '',
      reviews: [],
    })
    render(<CodeReviewHistoryPage language="zh" />)
    expect(await screen.findByText('暂无代码 Review 记录')).toBeInTheDocument()
    expect(
      screen.getByText(/docs\/reviews\/index\.json 不存在或为空/)
    ).toBeInTheDocument()
  })

  it('shows an error state with retry when the API fails', async () => {
    apiMock.getCodeReviews.mockRejectedValueOnce(new Error('boom'))
    render(<CodeReviewHistoryPage language="zh" />)
    expect(await screen.findByText(/加载失败/)).toBeInTheDocument()
    expect(screen.getByText('boom')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByRole('list')).toBeInTheDocument()
  })
})

describe('resolveDocHref', () => {
  it('keeps external URLs, mailto and in-page anchors as-is', () => {
    expect(resolveDocHref('https://example.com/a.md', 'x/a.md', REPO)).toBe(
      'https://example.com/a.md'
    )
    expect(resolveDocHref('mailto:a@b.com', 'x/a.md', REPO)).toBe(
      'mailto:a@b.com'
    )
    expect(resolveDocHref('#section', 'x/a.md', REPO)).toBe('#section')
  })

  it('rewrites local worktree absolute paths into GitHub blob links with line anchors', () => {
    expect(
      resolveDocHref(
        '/Users/zhangyun/workspace/nofx/.claude/worktrees/hungry-borg-136bf6/trader/paper/paper.go:493',
        'reviews/2026-10-28/x.md',
        REPO
      )
    ).toBe(`${REPO}/blob/dev/trader/paper/paper.go#L493`)
    expect(
      resolveDocHref(
        '/Users/zhangyun/workspace/nofx/kernel/x.go:10-20',
        'reviews/2026-10-28/x.md',
        REPO
      )
    ).toBe(`${REPO}/blob/dev/kernel/x.go#L10-L20`)
    expect(
      resolveDocHref(
        '/Users/zhangyun/workspace/nofx/kernel/x.go',
        'reviews/2026-10-28/x.md',
        REPO
      )
    ).toBe(`${REPO}/blob/dev/kernel/x.go`)
  })

  it('resolves sibling docs relative to the document directory', () => {
    expect(
      resolveDocHref(
        'CODE_REVIEW_2026-10-06_RECHECK.md',
        'architecture/CODE_REVIEW_2026-10-06.md',
        REPO
      )
    ).toBe(
      `${REPO}/blob/dev/docs/architecture/CODE_REVIEW_2026-10-06_RECHECK.md`
    )
    expect(
      resolveDocHref('./evidence/a.png', 'reviews/2026-10-28/x.md', REPO, 'raw')
    ).toBe(
      'https://raw.githubusercontent.com/lemondy/nofx/dev/docs/reviews/2026-10-28/evidence/a.png'
    )
  })

  it('resolves relative paths up to the repo root with line anchors', () => {
    expect(
      resolveDocHref(
        '../../trader/x.go:12',
        'architecture/CODE_REVIEW_2026-10-06.md',
        REPO
      )
    ).toBe(`${REPO}/blob/dev/trader/x.go#L12`)
  })

  it('returns undefined for unresolvable or out-of-root paths', () => {
    expect(
      resolveDocHref(
        '../../../x.md',
        'architecture/CODE_REVIEW_2026-10-06.md',
        REPO
      )
    ).toBeUndefined()
    expect(resolveDocHref('/etc/passwd', 'x/a.md', REPO)).toBeUndefined()
    expect(resolveDocHref(undefined, 'x/a.md', REPO)).toBeUndefined()
  })
})

describe('headingSlug', () => {
  it('matches GitHub heading anchors used by the reports', () => {
    expect(headingSlug('8. 修复记录（2026-09-13）')).toBe(
      '8-修复记录2026-09-13'
    )
    expect(headingSlug('10.4 修复记录（2026-09-20，同日）')).toBe(
      '104-修复记录2026-09-20同日'
    )
  })
})
