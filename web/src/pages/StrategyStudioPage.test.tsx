import React from 'react'
import { afterEach, expect, test, vi } from 'vitest'
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { StrategyStudioPage } from './StrategyStudioPage'

const state = vi.hoisted(() => ({ language: 'zh' }))
vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({ token: 'mock-token' }),
}))
vi.mock('../contexts/LanguageContext', () => ({
  useLanguage: () => ({ language: state.language }),
}))
vi.mock('../i18n/translations', () => ({
  t: (k: string) => k.split('.').at(-1),
}))
vi.mock('../lib/notify', () => ({
  notify: { success: vi.fn(), warning: vi.fn(), error: vi.fn() },
  confirmToast: vi.fn(),
}))
vi.mock('../components/strategy/CoinSourceEditor', () => ({
  CoinSourceEditor: () => null,
}))
vi.mock('../components/strategy/IndicatorEditor', () => ({
  IndicatorEditor: () => null,
}))
vi.mock('../components/strategy/RiskControlEditor', () => ({
  RiskControlEditor: () => null,
}))
vi.mock('../components/strategy/PublishSettingsEditor', () => ({
  PublishSettingsEditor: () => null,
}))
vi.mock('../components/strategy/GridConfigEditor', () => ({
  GridConfigEditor: () => null,
  defaultGridConfig: {},
}))
vi.mock('../components/strategy/StockConfigEditor', () => ({
  StockConfigEditor: ({ config }: any) => (
    <pre data-testid="stock-editor">{JSON.stringify(config)}</pre>
  ),
  defaultStockConfig: {
    symbols: [],
    sessions: { regular: true },
    paper_trading: true,
  },
}))
vi.mock('../components/strategy/TokenEstimateBar', () => ({
  TokenEstimateBar: () => null,
}))
vi.mock('../components/common/DeepVoidBackground', () => ({
  DeepVoidBackground: ({ children }: any) => <div>{children}</div>,
}))
vi.mock('../components/strategy/PromptSectionsEditor', () => ({
  PromptSectionsEditor: ({ config }: any) => (
    <pre data-testid="prompt-sections">{JSON.stringify(config)}</pre>
  ),
}))

const savedConfig = {
  language: 'zh',
  strategy_type: 'ai_trading',
  coin_source: {},
  indicators: {},
  risk_control: {},
  prompt_sections: { role_definition: 'USER_SAVED_CUSTOM' },
  custom_prompt: '',
}
const saved = {
  id: 's1',
  name: 'Saved Custom',
  is_active: true,
  is_default: false,
  updated_at: '2026-10-03T00:00:00Z',
  config: savedConfig,
}
function installFetch(delayDefault = false) {
  let releaseDefault: (value: any) => void = () => {}
  const defaults = new Promise((resolve) => {
    releaseDefault = resolve
  })
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      if (url.includes('default-config')) {
        if (delayDefault) await defaults
        return {
          ok: true,
          json: async () => ({
            ...savedConfig,
            language: 'en',
            prompt_sections: { role_definition: 'ENGLISH_DEFAULT' },
          }),
        }
      }
      if (url.endsWith('/api/strategies'))
        return { ok: true, json: async () => ({ strategies: [saved] }) }
      if (url.endsWith('/api/models')) return { ok: true, json: async () => [] }
      return {
        ok: true,
        json: async () => ({
          system_prompt: 'test',
          prompt_variant: 'balanced',
          config_summary: {},
        }),
      }
    })
  )
  return () => releaseDefault(null)
}
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  state.language = 'zh'
})
test('Review03 switching UI language must preserve saved customized prompt', async () => {
  installFetch()
  const view = render(<StrategyStudioPage />)
  await screen.findByText('Saved Custom')
  fireEvent.click(screen.getByText('promptSections'))
  expect(screen.getByTestId('prompt-sections')).toHaveTextContent(
    'USER_SAVED_CUSTOM'
  )
  state.language = 'en'
  view.rerender(<StrategyStudioPage />)
  // Wait for the fetched template to commit so the safety assertion cannot
  // pass merely because it ran before the async state update.
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
  expect(screen.getByTestId('prompt-sections')).toHaveTextContent(
    'USER_SAVED_CUSTOM'
  )
})

test('Review03 late save refresh must preserve a newly selected strategy', async () => {
  const other = {
    ...saved,
    id: 's2',
    name: 'Strategy B',
    is_active: false,
    config: {
      ...savedConfig,
      prompt_sections: { role_definition: 'B_CUSTOM' },
    },
  }
  let listCalls = 0
  let release: () => void = () => {}
  const delayed = new Promise<void>((resolve) => {
    release = resolve
  })
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, options?: any) => {
      if (url.endsWith('/api/strategies')) {
        listCalls++
        if (listCalls > 1) await delayed
        return { ok: true, json: async () => ({ strategies: [saved, other] }) }
      }
      if (options?.method === 'PUT')
        return { ok: true, json: async () => ({ message: 'saved' }) }
      if (url.endsWith('/api/models')) return { ok: true, json: async () => [] }
      return {
        ok: true,
        json: async () => ({
          system_prompt: 'test',
          prompt_variant: 'balanced',
          config_summary: {},
        }),
      }
    })
  )
  render(<StrategyStudioPage />)
  await screen.findByDisplayValue('Saved Custom')
  fireEvent.change(screen.getByDisplayValue('Saved Custom'), {
    target: { value: 'Changed A' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'save' }))
  await waitFor(() => expect(listCalls).toBe(2))
  fireEvent.click(screen.getByText('Strategy B'))
  expect(screen.getByDisplayValue('Strategy B')).toBeInTheDocument()
  await act(async () => {
    release()
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
  expect(screen.getByDisplayValue('Strategy B')).toBeInTheDocument()
})

test('save advances only its own version and retains newer drafts on external refresh/conflict', async () => {
  const firstVersion = '2026-10-03T00:00:00.000000001Z'
  const writtenVersion = '2026-10-03T00:00:00.000000002Z'
  const external = {
    ...saved,
    name: 'External edit',
    updated_at: '2026-10-03T00:00:00.000000003Z',
  }
  const writes: any[] = []
  let lists = 0
  let release: () => void = () => {}
  const delayed = new Promise<void>((resolve) => {
    release = resolve
  })
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, options?: any) => {
      if (options?.method === 'PUT') {
        writes.push(JSON.parse(options.body))
        if (writes.length === 1)
          return {
            ok: true,
            json: async () => ({ updated_at: writtenVersion }),
          }
        return { ok: false, status: 409 }
      }
      if (url.endsWith('/api/strategies')) {
        lists++
        if (lists === 2) await delayed
        return {
          ok: true,
          json: async () => ({
            strategies: [
              lists === 1 ? { ...saved, updated_at: firstVersion } : external,
            ],
          }),
        }
      }
      if (url.endsWith('/api/models')) return { ok: true, json: async () => [] }
      return {
        ok: true,
        json: async () => ({ system_prompt: 'test', config_summary: {} }),
      }
    })
  )
  const view = render(<StrategyStudioPage />)
  await screen.findByDisplayValue('Saved Custom')
  fireEvent.change(screen.getByDisplayValue('Saved Custom'), {
    target: { value: 'First save' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'save' }))
  await waitFor(() => expect(lists).toBe(2))
  fireEvent.change(screen.getByDisplayValue('First save'), {
    target: { value: 'Newer local draft' },
  })
  state.language = 'en'
  view.rerender(<StrategyStudioPage />)
  await act(async () => {
    release()
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
  expect(screen.getByDisplayValue('Newer local draft')).toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: 'save' }))
  await waitFor(() => expect(writes).toHaveLength(2))
  expect(writes[1].base_updated_at).toBe(writtenVersion)
  expect(writes[1].config.language).toBe('zh')
  await screen.findByText(/本地草稿已保留/)
  expect(screen.getByDisplayValue('Newer local draft')).toBeInTheDocument()
})

test('selecting US stocks shows StockConfigEditor with paper-on defaults and hides crypto sections', async () => {
  installFetch()
  render(<StrategyStudioPage />)
  await screen.findByText('Saved Custom')
  // ai_trading: crypto sections visible, stock editor absent
  expect(screen.getByText('coinSource')).toBeInTheDocument()
  expect(screen.getByText('riskControl')).toBeInTheDocument()
  expect(screen.queryByTestId('stock-editor')).not.toBeInTheDocument()

  fireEvent.click(screen.getByTestId('strategy-type-us-stock'))

  expect(screen.getByTestId('stock-editor')).toHaveTextContent(
    '"paper_trading":true'
  )
  expect(screen.getByTestId('stock-editor')).toHaveTextContent('"symbols":[]')
  expect(screen.getByText('stockConfig')).toBeInTheDocument()
  for (const hidden of [
    'coinSource',
    'indicators',
    'riskControl',
    'promptSections',
    'customPrompt',
    'gridConfig',
  ]) {
    expect(screen.queryByText(hidden)).not.toBeInTheDocument()
  }
  // publish settings stay available for every strategy type
  expect(screen.getByText('publishSettings')).toBeInTheDocument()
})
