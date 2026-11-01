import { API_BASE, httpClient } from './helpers'

/** 审查类型: fullstack 全栈 / quant 量化 / execution 执行链 / module 模块 / param 参数迭代 / candidates 候选链路 / recheck 复核 */
export type CodeReviewKind =
  | 'fullstack'
  | 'quant'
  | 'execution'
  | 'module'
  | 'param'
  | 'candidates'
  | 'recheck'

/** 修复状态: fixed 全部已修 / partial 部分已修 / open 基本未修(仅记录) / na 不适用 */
export type CodeReviewResolutionStatus = 'fixed' | 'partial' | 'open' | 'na'

/** 文档角色: report 主报告 / raw 分方向原始报告 / recheck 复核 / fixes 修复记录 / verification 核实 / evidence 证据 */
export type CodeReviewDocRole =
  | 'report'
  | 'raw'
  | 'recheck'
  | 'fixes'
  | 'verification'
  | 'evidence'

/** 清单里的一条文档（path 相对 docs/ 目录，只允许 .md） */
export interface CodeReviewDoc {
  path: string
  title: string
  role: CodeReviewDocRole
}

/** 严重度分布（统一归一后，未知为 null）；scheme 保留原报告的分级口径 */
export interface CodeReviewFindings {
  total: number | null
  high: number | null
  medium: number | null
  low: number | null
  info: number | null
  scheme: string
}

/** 修复情况统计（未知为 null） */
export interface CodeReviewResolution {
  status: CodeReviewResolutionStatus
  fixed: number | null
  rejected: number | null
  open: number | null
}

/** 清单里的一条审查记录 */
export interface CodeReviewEntry {
  id: string
  date: string
  title: string
  title_en: string
  kind: CodeReviewKind
  scope: string[]
  baseline_commit: string
  summary: string
  findings: CodeReviewFindings
  resolution: CodeReviewResolution
  commits: string[]
  prs: number[]
  docs: CodeReviewDoc[]
}

/** 清单（docs/reviews/index.json 的只读视图） */
export interface CodeReviewManifest {
  version: number
  updated_at: string
  repo_url: string
  reviews: CodeReviewEntry[]
}

/** 文档全文（markdown 原文） */
export interface CodeReviewDocContent {
  path: string
  title: string
  role: string
  content: string
}

export const codeReviewApi = {
  async getCodeReviews(): Promise<CodeReviewManifest> {
    // silent: 页面自己渲染错误态，不要全局 toast
    const result = await httpClient.request<CodeReviewManifest>(
      `${API_BASE}/code-reviews`,
      { silent: true }
    )
    if (!result.success) {
      throw new Error(result.message || 'Failed to fetch code reviews')
    }
    return result.data!
  },

  async getCodeReview(id: string): Promise<CodeReviewEntry> {
    const result = await httpClient.request<CodeReviewEntry>(
      `${API_BASE}/code-reviews/${encodeURIComponent(id)}`,
      { silent: true }
    )
    if (!result.success) {
      throw new Error(result.message || 'Failed to fetch code review')
    }
    return result.data!
  },

  async getCodeReviewDoc(
    id: string,
    path: string
  ): Promise<CodeReviewDocContent> {
    const result = await httpClient.request<CodeReviewDocContent>(
      `${API_BASE}/code-reviews/${encodeURIComponent(id)}/doc?path=${encodeURIComponent(path)}`,
      { silent: true }
    )
    if (!result.success) {
      throw new Error(result.message || 'Failed to fetch code review doc')
    }
    return result.data!
  },
}
