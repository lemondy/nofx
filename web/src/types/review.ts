// ===== Trade Review System (复盘) types =====

// TradeJournalEntry one reviewed trade (auto-synced from closed positions)
export interface JournalEntry {
  id: number
  trader_id: string
  position_id: number
  // Trade facts
  symbol: string
  side: 'LONG' | 'SHORT'
  entry_price: number
  exit_price: number
  quantity: number
  leverage: number
  entry_time: number // Unix ms
  exit_time: number // Unix ms
  realized_pnl: number
  fee: number
  pnl_pct: number
  close_reason: string
  // Decision basis
  planned_stop_loss: number
  planned_take_profit: number
  entry_reasoning: string
  confidence: number
  // Review fields
  executed_as_plan: '' | 'yes' | 'partial' | 'no' | string
  deviation_note: string
  emotions: string // comma-separated tags
  mistake_category: '' | 'strategy' | 'execution' | 'risk_control' | 'market' | 'none' | string
  strategy_tag: string
  lesson: string
  review_status: 'pending' | 'reviewed' | string
  reviewed_at: number
  created_at: number
  updated_at: number
}

export interface JournalListResponse {
  entries: JournalEntry[]
  total: number
}

export interface JournalGroupStats {
  key: string
  count: number
  wins: number
  win_rate: number
  total_pnl: number
  avg_pnl: number
}

export interface JournalStats {
  total_entries: number
  reviewed_count: number
  pending_count: number
  win_trades: number
  loss_trades: number
  win_rate: number
  total_pnl: number
  avg_win: number
  avg_loss: number
  expectancy: number
  profit_factor: number
  adherence_plan: JournalGroupStats[]
  emotion_stats: JournalGroupStats[]
  mistake_stats: JournalGroupStats[]
  strategy_stats: JournalGroupStats[]
  with_plan_count: number
  with_plan_win_rate: number
  no_plan_count: number
  no_plan_win_rate: number
}

// ===== Rules =====
export interface TradingRule {
  id: number
  trader_id: string
  rule_type: 'hard' | 'soft'
  name: string
  description: string
  condition_json: string
  on_violation: 'block' | 'warn'
  lesson_text: string
  tags: string
  source: 'manual' | 'ai_review' | string
  source_stats: string
  enabled: boolean
  hit_count: number
  block_count: number
  created_at: number
  updated_at: number
}

export interface RuleCondition {
  field: string
  op: string
  value: number | string | boolean
}

export interface RuleInput {
  rule_type: 'hard' | 'soft'
  name: string
  description?: string
  condition?: string // JSON string for hard rules
  on_violation?: 'block' | 'warn'
  lesson_text?: string
  tags?: string
  source?: string
  source_stats?: string
  enabled?: boolean
}

export interface RuleCheckLog {
  id: number
  trader_id: string
  rule_id: number
  rule_name: string
  action: string
  symbol: string
  violated: boolean
  blocked: boolean
  message: string
  created_at: number
}

export interface RuleViolation {
  rule_id: number
  rule_name: string
  rule_type: 'hard' | 'soft'
  action: 'block' | 'warn'
  message: string
  symbol: string
  decision: string
  detail: string
}

export interface RuleCheckResult {
  violations: RuleViolation[]
  warnings: RuleViolation[]
  lessons: RuleViolation[]
  blocked: boolean
}

export interface RuleProposal extends RuleInput {
  name: string
  rule_type: 'hard' | 'soft'
}

// ===== AI review =====
export interface AIReviewResult {
  ai_response: string
  period: 'daily' | 'weekly' | 'monthly' | string
  generated_at: number
}

// ===== Review prompt config =====
// Templates in effect (custom when *_custom is true, else built-in default).
export interface ReviewPromptConfig {
  review_system_prompt: string
  rule_extract_system_prompt: string
  review_custom: boolean
  rule_extract_custom: boolean
}
