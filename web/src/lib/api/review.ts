import { API_BASE, httpClient } from './helpers'
import type {
  JournalEntry,
  JournalListResponse,
  JournalStats,
  TradingRule,
  RuleInput,
  RuleCheckLog,
  RuleCheckResult,
  AIReviewResult,
  RuleProposal,
  ReviewPromptConfig,
} from '../../types'

export const reviewApi = {
  // ===== Journal =====
  async getJournal(
    traderId?: string,
    opts?: { limit?: number; offset?: number; symbol?: string; reviewStatus?: string }
  ): Promise<JournalListResponse> {
    const params = new URLSearchParams()
    if (traderId) params.append('trader_id', traderId)
    if (opts?.limit) params.append('limit', String(opts.limit))
    if (opts?.offset) params.append('offset', String(opts.offset))
    if (opts?.symbol) params.append('symbol', opts.symbol)
    if (opts?.reviewStatus) params.append('review_status', opts.reviewStatus)
    const result = await httpClient.request<JournalListResponse>(
      `${API_BASE}/review/journal?${params}`
    )
    if (!result.success) throw new Error('Failed to fetch trade journal')
    return result.data!
  },

  async syncJournal(traderId?: string): Promise<{ created: number }> {
    const url = traderId
      ? `${API_BASE}/review/journal/sync?trader_id=${traderId}`
      : `${API_BASE}/review/journal/sync`
    const result = await httpClient.request<{ created: number }>(url, { method: 'POST' })
    if (!result.success) throw new Error('Failed to sync trade journal')
    return result.data!
  },

  async updateJournalEntry(
    id: number,
    traderId: string | undefined,
    payload: Partial<JournalEntry>
  ): Promise<{ entry: JournalEntry }> {
    const params = new URLSearchParams()
    if (traderId) params.append('trader_id', traderId)
    const result = await httpClient.request<{ entry: JournalEntry }>(
      `${API_BASE}/review/journal/${id}?${params}`,
      { method: 'POST', data: payload }
    )
    if (!result.success) throw new Error('Failed to update journal entry')
    return result.data!
  },

  async getJournalStats(traderId?: string): Promise<JournalStats> {
    const url = traderId
      ? `${API_BASE}/review/journal/stats?trader_id=${traderId}`
      : `${API_BASE}/review/journal/stats`
    const result = await httpClient.request<JournalStats>(url)
    if (!result.success) throw new Error('Failed to fetch journal statistics')
    return result.data!
  },

  // ===== Rules =====
  async getRules(traderId?: string): Promise<{ rules: TradingRule[] }> {
    const url = traderId
      ? `${API_BASE}/review/rules?trader_id=${traderId}`
      : `${API_BASE}/review/rules`
    const result = await httpClient.request<{ rules: TradingRule[] }>(url)
    if (!result.success) throw new Error('Failed to fetch trading rules')
    return result.data!
  },

  async createRule(traderId: string | undefined, input: RuleInput): Promise<{ rule: TradingRule }> {
    const params = new URLSearchParams()
    if (traderId) params.append('trader_id', traderId)
    const result = await httpClient.request<{ rule: TradingRule }>(
      `${API_BASE}/review/rules?${params}`,
      { method: 'POST', data: input }
    )
    if (!result.success) throw new Error('Failed to create trading rule')
    return result.data!
  },

  async updateRule(
    id: number,
    traderId: string | undefined,
    input: Partial<RuleInput>
  ): Promise<{ rule: TradingRule }> {
    const params = new URLSearchParams()
    if (traderId) params.append('trader_id', traderId)
    const result = await httpClient.request<{ rule: TradingRule }>(
      `${API_BASE}/review/rules/${id}?${params}`,
      { method: 'POST', data: input }
    )
    if (!result.success) throw new Error('Failed to update trading rule')
    return result.data!
  },

  async deleteRule(id: number, traderId?: string): Promise<void> {
    const params = new URLSearchParams()
    if (traderId) params.append('trader_id', traderId)
    const result = await httpClient.request<void>(
      `${API_BASE}/review/rules/${id}?${params}`,
      { method: 'DELETE' }
    )
    if (!result.success) throw new Error('Failed to delete trading rule')
  },

  async getRuleLogs(traderId?: string, limit = 50): Promise<{ logs: RuleCheckLog[] }> {
    const url = traderId
      ? `${API_BASE}/review/rules/logs?trader_id=${traderId}&limit=${limit}`
      : `${API_BASE}/review/rules/logs?limit=${limit}`
    const result = await httpClient.request<{ logs: RuleCheckLog[] }>(url)
    if (!result.success) throw new Error('Failed to fetch rule check logs')
    return result.data!
  },

  async checkRule(
    traderId: string | undefined,
    payload: {
      symbol: string
      action: string
      leverage: number
      position_size_usd: number
      stop_loss: number
      take_profit: number
      confidence: number
      price: number
    }
  ): Promise<RuleCheckResult> {
    const params = new URLSearchParams()
    if (traderId) params.append('trader_id', traderId)
    const result = await httpClient.request<RuleCheckResult>(
      `${API_BASE}/review/rules/check?${params}`,
      { method: 'POST', data: payload }
    )
    if (!result.success) throw new Error('Failed to run rule check')
    return result.data!
  },

  // ===== AI review =====
  async aiExtractRules(traderId?: string): Promise<{ proposals: RuleProposal[]; ai_response: string }> {
    const params = new URLSearchParams()
    if (traderId) params.append('trader_id', traderId)
    const result = await httpClient.request<{ proposals: RuleProposal[]; ai_response: string }>(
      `${API_BASE}/review/ai/extract-rules?${params}`,
      { method: 'POST', data: {} }
    )
    if (!result.success) throw new Error('Failed to extract rules with AI')
    return result.data!
  },

  async aiApplyRules(
    traderId: string | undefined,
    rules: RuleInput[]
  ): Promise<{ saved: number }> {
    const params = new URLSearchParams()
    if (traderId) params.append('trader_id', traderId)
    const result = await httpClient.request<{ saved: number }>(
      `${API_BASE}/review/ai/apply-rules?${params}`,
      { method: 'POST', data: { rules } }
    )
    if (!result.success) throw new Error('Failed to apply AI rules')
    return result.data!
  },

  async aiReview(
    traderId?: string,
    period: 'daily' | 'weekly' | 'monthly' = 'weekly'
  ): Promise<AIReviewResult> {
    const params = new URLSearchParams()
    if (traderId) params.append('trader_id', traderId)
    const result = await httpClient.request<AIReviewResult>(
      `${API_BASE}/review/ai/review?${params}`,
      { method: 'POST', data: { period } }
    )
    if (!result.success) throw new Error('Failed to run AI review')
    return result.data!
  },

  async getPromptConfig(): Promise<ReviewPromptConfig> {
    const result = await httpClient.request<ReviewPromptConfig>(
      `${API_BASE}/review/prompt-config`
    )
    if (!result.success) throw new Error('Failed to load review prompt config')
    return result.data!
  },

  async savePromptConfig(
    update: Partial<
      Pick<ReviewPromptConfig, 'review_system_prompt' | 'rule_extract_system_prompt'>
    >
  ): Promise<void> {
    const result = await httpClient.request<{ message: string }>(
      `${API_BASE}/review/prompt-config`,
      { method: 'PUT', data: update }
    )
    if (!result.success) throw new Error('Failed to save review prompt config')
  },
}
