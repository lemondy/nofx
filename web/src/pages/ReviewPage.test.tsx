import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../lib/api'
import { t, type Language } from '../i18n/translations'
import type { JournalEntry, JournalStats, TraderInfo } from '../types'
import { ReviewPage } from './ReviewPage'

vi.mock('../lib/api', () => ({
  api: {
    getTraders: vi.fn(),
    getJournal: vi.fn(),
    updateJournalEntry: vi.fn(),
    getPromptConfig: vi.fn(),
    savePromptConfig: vi.fn(),
    getJournalStats: vi.fn(),
  },
}))

const entry: JournalEntry = {
  id: 1,
  trader_id: 'T',
  position_id: 1,
  symbol: 'BTCUSDT',
  side: 'LONG',
  entry_price: 100,
  exit_price: 101,
  quantity: 1,
  leverage: 2,
  entry_time: 1000,
  exit_time: 2000,
  realized_pnl: 1,
  fee: 0.1,
  pnl_pct: 1.8,
  close_reason: '',
  planned_stop_loss: 90,
  planned_take_profit: 120,
  entry_reasoning: '',
  confidence: 80,
  executed_as_plan: 'yes',
  deviation_note: '',
  emotions: '',
  mistake_category: 'none',
  strategy_tag: 'other',
  lesson: '',
  failure_mode: 'no_mistake',
  ai_review_attempts: 0,
  review_status: 'ai_reviewed',
  reviewed_at: 3000,
  ai_managed: true,
  created_at: 1000,
  updated_at: 3000,
}

beforeEach(() => {
  vi.resetAllMocks()
  window.history.replaceState({}, '', '/')
  vi.mocked(api.getTraders).mockResolvedValue([
    { trader_id: 'T', trader_name: 'Test' } as TraderInfo,
  ])
  vi.mocked(api.getJournal).mockResolvedValue({ entries: [entry], total: 1 })
  vi.mocked(api.updateJournalEntry).mockResolvedValue({ entry })
  vi.mocked(api.getPromptConfig).mockResolvedValue({
    review_system_prompt: 'review',
    rule_extract_system_prompt: 'rules',
    review_custom: false,
    rule_extract_custom: false,
    auto_review_enabled: false,
  })
  vi.mocked(api.savePromptConfig).mockResolvedValue(undefined)
})

describe('ReviewPage automatic review', () => {
  it.each<Language>(['zh', 'en', 'id'])(
    'filters and edits failure modes in %s',
    async (language) => {
      const rv = (key: string) => t(`reviewPage.${key}`, language)
      render(<ReviewPage language={language} />)
      const row = (await screen.findByText('BTCUSDT')).closest('tr')!
      expect(within(row).getByText(rv('aiReviewed'))).toBeInTheDocument()
      expect(
        within(row).getByText(rv('failure_no_mistake'))
      ).toBeInTheDocument()

      const filter = screen.getByRole('combobox', { name: rv('colStatus') })
      fireEvent.change(filter, { target: { value: 'ai_reviewed' } })
      await waitFor(() =>
        expect(api.getJournal).toHaveBeenLastCalledWith(
          'T',
          expect.objectContaining({ reviewStatus: 'ai_reviewed' })
        )
      )

      await screen.findByText('BTCUSDT')
      fireEvent.click(screen.getByRole('button', { name: rv('reviewAction') }))
      const select = screen.getByRole('combobox', { name: rv('failureMode') })
      expect(select).toHaveValue('no_mistake')
      expect(within(select).getAllByRole('option')).toHaveLength(12)
      fireEvent.change(select, { target: { value: 'weak_setup' } })
      fireEvent.click(screen.getByRole('button', { name: rv('saveReview') }))
      await waitFor(() =>
        expect(api.updateJournalEntry).toHaveBeenCalledWith(
          1,
          'T',
          expect.objectContaining({ failure_mode: 'weak_setup' })
        )
      )
    }
  )

  it('loads a default-off toggle and saves explicit enable and disable', async () => {
    render(<ReviewPage language="zh" />)
    await screen.findByText('BTCUSDT')
    fireEvent.click(screen.getByRole('tab', { name: 'AI 复盘' }))
    await waitFor(() => expect(api.getPromptConfig).toHaveBeenCalledTimes(1))
    fireEvent.click(screen.getByRole('button', { name: 'Prompt 设置' }))
    const toggle = await screen.findByRole('checkbox', {
      name: '启用 AI 自动复盘',
    })
    expect(toggle).not.toBeChecked()
    fireEvent.click(toggle)
    fireEvent.click(
      screen.getByRole('button', { name: t('reviewPage.promptSave', 'zh') })
    )
    await waitFor(() =>
      expect(api.savePromptConfig).toHaveBeenLastCalledWith(
        expect.objectContaining({ auto_review_enabled: true })
      )
    )
    await waitFor(() => expect(toggle).not.toBeChecked())
    fireEvent.click(
      screen.getByRole('button', { name: t('reviewPage.promptSave', 'zh') })
    )
    await waitFor(() =>
      expect(api.savePromptConfig).toHaveBeenLastCalledWith(
        expect.objectContaining({ auto_review_enabled: false })
      )
    )
  })

  it('renders failure statistics with missing R shown as unavailable', async () => {
    vi.mocked(api.getJournalStats).mockResolvedValue({
      total_entries: 3,
      reviewed_count: 3,
      pending_count: 0,
      win_trades: 1,
      loss_trades: 2,
      win_rate: 33.3,
      total_pnl: -1,
      avg_win: 1,
      avg_loss: 1,
      expectancy: -0.33,
      profit_factor: 0.5,
      adherence_plan: [],
      emotion_stats: [],
      mistake_stats: [],
      strategy_stats: [],
      with_plan_count: 2,
      with_plan_win_rate: 50,
      no_plan_count: 1,
      no_plan_win_rate: 0,
      by_failure_mode: [
        {
          key: 'weak_setup',
          count: 2,
          total_pnl: -2,
          avg_r: -0.25,
          r_samples: 2,
        },
        {
          key: 'no_mistake',
          count: 1,
          total_pnl: 1,
          avg_r: null,
          r_samples: 0,
        },
      ],
    } satisfies JournalStats)
    render(<ReviewPage language="zh" />)
    await screen.findByText('BTCUSDT')
    fireEvent.click(screen.getByRole('tab', { name: '统计分析' }))
    const row = (await screen.findByText('信号不足')).closest('tr')!
    expect(within(row).getByText('-0.25R (2)')).toBeInTheDocument()
    const unknown = screen.getByText('过程无明显错误').closest('tr')!
    expect(within(unknown).getByText('--')).toBeInTheDocument()
  })
})
