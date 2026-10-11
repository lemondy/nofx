import { cn } from '../lib/cn'
import {
  Input,
  Button,
  Badge,
  Card,
  CardHeader,
  CardBody,
  Tabs,
  EmptyState,
} from '../components/ui'
import { useState, useEffect, useCallback, useRef } from 'react'
import { useAuth } from '../contexts/AuthContext'
import { useLanguage } from '../contexts/LanguageContext'
import {
  Plus,
  Copy,
  Trash2,
  Check,
  ChevronDown,
  ChevronRight,
  Settings,
  BarChart3,
  Target,
  Shield,
  Zap,
  Activity,
  Save,
  Sparkles,
  Eye,
  Play,
  FileText,
  Loader2,
  RefreshCw,
  Clock,
  Bot,
  Terminal,
  Code,
  Send,
  Download,
  Upload,
  Globe,
  History,
} from 'lucide-react'
import type { Strategy, StrategyConfig, AIModel } from '../types'
import { confirmToast, notify } from '../lib/notify'
import { CoinSourceEditor } from '../components/strategy/CoinSourceEditor'
import { IndicatorEditor } from '../components/strategy/IndicatorEditor'
import { RiskControlEditor } from '../components/strategy/RiskControlEditor'
import { PromptSectionsEditor } from '../components/strategy/PromptSectionsEditor'
import { PublishSettingsEditor } from '../components/strategy/PublishSettingsEditor'
import {
  GridConfigEditor,
  defaultGridConfig,
} from '../components/strategy/GridConfigEditor'
import {
  StockConfigEditor,
  defaultStockConfig,
} from '../components/strategy/StockConfigEditor'
import { TokenEstimateBar } from '../components/strategy/TokenEstimateBar'
import { VersionsPanel } from '../components/strategy/VersionsPanel'
import { t } from '../i18n/translations'
import { riskControl, ts } from '../i18n/strategy-translations'

const API_BASE = import.meta.env.VITE_API_BASE || ''

function versionAtLeast(candidate?: string, known?: string) {
  if (!known) return true
  if (!candidate) return false
  const candidateMs = Date.parse(candidate)
  const knownMs = Date.parse(known)
  if (!Number.isFinite(candidateMs) || !Number.isFinite(knownMs))
    return candidate === known
  if (candidateMs !== knownMs) return candidateMs > knownMs
  const fraction = (value: string) =>
    (value.match(/\.(\d+)(?:Z|[+-])/)?.[1] || '').padEnd(9, '0')
  return fraction(candidate) >= fraction(known)
}

export function StrategyStudioPage() {
  const { token } = useAuth()
  const { language } = useLanguage()

  const [strategies, setStrategies] = useState<Strategy[]>([])
  const [selectedStrategy, setSelectedStrategy] = useState<Strategy | null>(
    null
  )
  const [editingConfig, setEditingConfig] = useState<StrategyConfig | null>(
    null
  )
  const [isLoading, setIsLoading] = useState(true)
  const [isSaving, setIsSaving] = useState(false)
  const [estimatedTokens, setEstimatedTokens] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [hasChanges, setHasChanges] = useState(false)

  // AI Models for test run
  const [aiModels, setAiModels] = useState<AIModel[]>([])
  const [selectedModelId, setSelectedModelId] = useState<string>('')

  // Accordion states for left panel
  const [expandedSections, setExpandedSections] = useState({
    gridConfig: true,
    stockConfig: true,
    coinSource: true,
    indicators: false,
    riskControl: false,
    promptSections: false,
    customPrompt: false,
    publishSettings: false,
  })

  // Right panel states
  const [activeRightTab, setActiveRightTab] = useState<
    'prompt' | 'test' | 'versions'
  >('prompt')
  const [promptPreview, setPromptPreview] = useState<{
    system_prompt: string
    user_prompt?: string
    prompt_variant: string
    config_summary: Record<string, unknown>
  } | null>(null)
  const [isLoadingPrompt, setIsLoadingPrompt] = useState(false)
  const [selectedVariant, setSelectedVariant] = useState('balanced')

  // AI Test Run states
  const [aiTestResult, setAiTestResult] = useState<{
    system_prompt?: string
    user_prompt?: string
    ai_response?: string
    reasoning?: string
    decisions?: unknown[]
    error?: string
    duration_ms?: number
  } | null>(null)
  const [isRunningAiTest, setIsRunningAiTest] = useState(false)

  const toggleSection = (section: keyof typeof expandedSections) => {
    setExpandedSections((prev) => ({
      ...prev,
      [section]: !prev[section],
    }))
  }

  // Fetch AI Models
  const fetchAiModels = useCallback(async () => {
    if (!token) return
    try {
      const response = await fetch(`${API_BASE}/api/models`, {
        headers: { Authorization: `Bearer ${token}` },
      })
      if (response.ok) {
        const data = await response.json()
        // Backend returns an array, not { models: [] }
        const allModels = Array.isArray(data) ? data : data.models || []
        const enabledModels = allModels.filter((m: AIModel) => m.enabled)
        setAiModels(enabledModels)
        // F16b (2026-10-01 review): functional auto-select + stable deps —
        // depending on selectedModelId re-created this callback on every model
        // switch, which re-ran the mount effect and re-fetched strategies,
        // clobbering unsaved drafts.
        if (enabledModels.length > 0) {
          setSelectedModelId((prev) => prev || enabledModels[0].id)
        }
      }
    } catch (err) {
      console.error('Failed to fetch AI models:', err)
    }
  }, [token])

  // Fetch strategies. F16b: by default this only refreshes the LIST and
  // auto-selects when nothing is selected yet — a background refresh must
  // never yank the selection back to the active/first strategy and overwrite
  // editingConfig (that discarded unsaved drafts on every model switch).
  // reselect=true (mount) restores the old active/first selection;
  // strategyId refreshes ONE strategy's server copy into the editor.
  const fetchStrategies = useCallback(
    async (opts?: { reselect?: boolean; strategyId?: string }) => {
      if (!token) return
      try {
        const response = await fetch(`${API_BASE}/api/strategies`, {
          headers: { Authorization: `Bearer ${token}` },
        })
        if (!response.ok) throw new Error('Failed to fetch strategies')
        const data = await response.json()
        setStrategies(data.strategies || [])

        if (opts?.strategyId) {
          const fresh = (data.strategies || []).find(
            (s: Strategy) => s.id === opts.strategyId
          )
          if (fresh && selectedStrategyRef.current?.id === opts.strategyId) {
            // Dirty drafts keep their original edit base, so another writer's
            // new version still causes a conflict instead of authorizing overwrite.
            if (
              !hasChangesRef.current &&
              versionAtLeast(
                fresh.updated_at,
                selectedStrategyRef.current.updated_at
              )
            ) {
              setSelectedStrategy((prev) =>
                prev &&
                prev.id === fresh.id &&
                versionAtLeast(fresh.updated_at, prev.updated_at)
                  ? { ...prev, ...fresh }
                  : prev
              )
              setEditingConfig(fresh.config)
            }
          }
          return
        }
        if (opts?.reselect) {
          const active = data.strategies?.find((s: Strategy) => s.is_active)
          if (active) {
            setSelectedStrategy(active)
            setEditingConfig(active.config)
          } else if (data.strategies?.length > 0) {
            setSelectedStrategy(data.strategies[0])
            setEditingConfig(data.strategies[0].config)
          }
          return
        }
        // Default: keep whatever the user has selected/edited.
        setSelectedStrategy((prev) => {
          if (prev) return prev
          const active = data.strategies?.find((s: Strategy) => s.is_active)
          return active || data.strategies?.[0] || null
        })
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Unknown error')
      } finally {
        setIsLoading(false)
      }
    },
    [token]
  )

  useEffect(() => {
    fetchStrategies({ reselect: true })
    fetchAiModels()
  }, [])

  // Mirrors of dirty/selection state for effects and callbacks that must not
  // depend on them (F16: the language switch and background refreshes need to
  // READ dirtiness without re-firing).
  const hasChangesRef = useRef(hasChanges)
  useEffect(() => {
    hasChangesRef.current = hasChanges
  }, [hasChanges])

  // Interface language does not mutate persisted strategy prompts.
  const selectedStrategyRef = useRef(selectedStrategy)
  const editingConfigRef = useRef(editingConfig)
  selectedStrategyRef.current = selectedStrategy
  editingConfigRef.current = editingConfig

  // Create new strategy
  const handleCreateStrategy = async () => {
    if (!token) return
    try {
      const configResponse = await fetch(
        `${API_BASE}/api/strategies/default-config?lang=${language}`,
        { headers: { Authorization: `Bearer ${token}` } }
      )
      if (!configResponse.ok) throw new Error('Failed to fetch default config')
      const defaultConfig = await configResponse.json()

      const response = await fetch(`${API_BASE}/api/strategies`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({
          name: tr('newStrategyName'),
          description: '',
          config: defaultConfig,
        }),
      })
      if (!response.ok) throw new Error('Failed to create strategy')
      const result = await response.json()
      if (result.id) {
        // F16c (2026-10-01 review): fetch the SERVER-created strategy instead of
        // fabricating one with a client-side updated_at — the fabricated
        // timestamp could never match the server's (RFC3339Nano) and the first
        // save reliably died on the optimistic-lock 409.
        const createdResp = await fetch(
          `${API_BASE}/api/strategies/${result.id}`,
          {
            headers: { Authorization: `Bearer ${token}` },
          }
        )
        if (createdResp.ok) {
          const created = await createdResp.json()
          setSelectedStrategy((prev) =>
            prev ? { ...prev, ...created } : created
          )
          setEditingConfig(created.config)
          setHasChanges(false)
        }
        await fetchStrategies()
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error')
    }
  }

  // Delete strategy
  const handleDeleteStrategy = async (id: string) => {
    if (!token) return

    // Check if strategy is in use by any trader before showing dialog
    try {
      const tradersResp = await fetch(`${API_BASE}/api/my-traders`, {
        headers: { Authorization: `Bearer ${token}` },
      })
      if (tradersResp.ok) {
        const traderList = await tradersResp.json()
        const using = traderList.filter((t: any) => t.strategy_id === id)
        if (using.length > 0) {
          const names = using.map((t: any) => t.trader_name).join(', ')
          notify.error(`Strategy is in use by: ${names}`)
          return
        }
      }
    } catch {
      // fetch failed — proceed, backend will guard
    }

    const confirmed = await confirmToast(tr('confirmDeleteStrategy'), {
      title: tr('confirmDelete'),
      okText: tr('delete'),
      cancelText: tr('cancel'),
    })
    if (!confirmed) return

    try {
      const response = await fetch(`${API_BASE}/api/strategies/${id}`, {
        method: 'DELETE',
        headers: { Authorization: `Bearer ${token}` },
      })
      if (!response.ok) {
        const data = await response.json().catch(() => ({}))
        notify.error(data.error || 'Failed to delete strategy')
        return
      }
      notify.success(tr('strategyDeleted'))
      if (selectedStrategy?.id === id) {
        setSelectedStrategy(null)
        setEditingConfig(null)
        setHasChanges(false)
      }
      await fetchStrategies()
    } catch (err) {
      notify.error(err instanceof Error ? err.message : 'Unknown error')
    }
  }

  // Duplicate strategy
  const handleDuplicateStrategy = async (id: string) => {
    if (!token) return
    try {
      const response = await fetch(
        `${API_BASE}/api/strategies/${id}/duplicate`,
        {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            Authorization: `Bearer ${token}`,
          },
          body: JSON.stringify({
            name: tr('strategyCopy'),
          }),
        }
      )
      if (!response.ok) throw new Error('Failed to duplicate strategy')
      await fetchStrategies()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error')
    }
  }

  // Activate strategy
  const handleActivateStrategy = async (id: string) => {
    if (!token) return
    try {
      const response = await fetch(
        `${API_BASE}/api/strategies/${id}/activate`,
        {
          method: 'POST',
          headers: { Authorization: `Bearer ${token}` },
        }
      )
      if (!response.ok) throw new Error('Failed to activate strategy')
      await fetchStrategies()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error')
    }
  }

  // Export strategy as JSON file
  const handleExportStrategy = (strategy: Strategy) => {
    const exportData = {
      name: strategy.name,
      description: strategy.description,
      config: strategy.config,
      exported_at: new Date().toISOString(),
      version: '1.0',
    }
    const blob = new Blob([JSON.stringify(exportData, null, 2)], {
      type: 'application/json',
    })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `strategy_${strategy.name.replace(/\s+/g, '_')}_${new Date().toISOString().split('T')[0]}.json`
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    URL.revokeObjectURL(url)
    notify.success(tr('strategyExported'))
  }

  // Import strategy from JSON file
  const handleImportStrategy = async (
    event: React.ChangeEvent<HTMLInputElement>
  ) => {
    const file = event.target.files?.[0]
    if (!file || !token) return

    try {
      const text = await file.text()
      const importData = JSON.parse(text)

      // Validate imported data
      if (!importData.config || !importData.name) {
        throw new Error(tr('invalidStrategyFile'))
      }

      // Create new strategy with imported config
      const response = await fetch(`${API_BASE}/api/strategies`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({
          name: `${importData.name} (${tr('imported')})`,
          description: importData.description || '',
          config: importData.config,
        }),
      })
      if (!response.ok) throw new Error('Failed to import strategy')

      notify.success(tr('strategyImported'))
      await fetchStrategies()
    } catch (err) {
      const errorMsg = err instanceof Error ? err.message : 'Unknown error'
      notify.error(errorMsg)
    } finally {
      // Reset file input
      event.target.value = ''
    }
  }

  // Save strategy
  const handleSaveStrategy = async () => {
    if (!token || !selectedStrategy || !editingConfig) return
    if (estimatedTokens >= 128000 && currentStrategyType === 'ai_trading') {
      notify.warning(tr('tokenExceedWarning'))
      // continue with save
    }
    const savingID = selectedStrategy.id
    const savingDraft = JSON.stringify({
      name: selectedStrategy.name,
      description: selectedStrategy.description,
      config: editingConfig,
    })
    setIsSaving(true)
    // Guard against zombie saves: a lost connection (backend restart, proxy
    // hiccup) must never leave the button latched in 保存中 forever.
    const controller = new AbortController()
    const timeoutId = setTimeout(() => controller.abort(), 15_000)
    try {
      // Preserve strategy prompt language independently of UI language
      const configWithLanguage = {
        ...editingConfig,
        language: editingConfig.language,
      }
      const response = await fetch(
        `${API_BASE}/api/strategies/${selectedStrategy.id}`,
        {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json',
            Authorization: `Bearer ${token}`,
          },
          body: JSON.stringify({
            name: selectedStrategy.name,
            description: selectedStrategy.description,
            config: configWithLanguage,
            is_public: selectedStrategy.is_public,
            config_visible: selectedStrategy.config_visible,
            // Optimistic lock (round-4 review R4-5): the backend refuses the save
            // (409) when the strategy row changed after THIS page loaded its copy —
            // a stale snapshot silently overwriting newer risk_control values was
            // the 09-14 sl_min_atr_mult rollback path.
            base_updated_at: selectedStrategy.updated_at,
          }),
          signal: controller.signal,
        }
      )
      if (!response.ok) {
        if (response.status === 409) {
          const msg =
            '保存被拒绝：服务端已有更新。本地草稿已保留，请比较最新配置后再保存。'
          setError(msg)
          notify.error(msg)
          // Refresh the list while retaining the conflicting draft and its
          // edit-base token for an explicit comparison/reload.
          await fetchStrategies()
          return
        }
        const body = await response.text().catch(() => '')
        throw new Error(
          `保存失败 (HTTP ${response.status})${
            response.status === 401
              ? ' — 登录已过期，请重新登录'
              : response.status === 403
                ? ' — 系统默认策略不可修改'
                : body
                  ? `: ${body.slice(0, 120)}`
                  : ''
          }`
        )
      }
      const result = await response.json()
      const current = selectedStrategyRef.current
      if (current?.id === savingID) {
        if (result.updated_at)
          setSelectedStrategy((prev) =>
            prev && prev.id === savingID
              ? { ...prev, updated_at: result.updated_at }
              : prev
          )
        const nowDraft = JSON.stringify({
          name: current.name,
          description: current.description,
          config: editingConfigRef.current,
        })
        if (nowDraft === savingDraft) {
          hasChangesRef.current = false
          setHasChanges(false)
        }
      }
      notify.success(tr('strategySaved'))
    } catch (err) {
      const msg =
        err instanceof DOMException && err.name === 'AbortError'
          ? '保存超时（15 秒无响应）— 请检查后端是否正常后重试'
          : err instanceof Error
            ? err.message
            : 'Unknown error'
      setError(msg)
      notify.error(msg)
    } finally {
      clearTimeout(timeoutId)
      setIsSaving(false)
    }
    // Refresh in the background — never block the saving state on it.
    // strategyId: refresh THIS strategy's server copy (updated_at advances
    // after a save; the next save's optimistic lock needs it) without yanking
    // the selection or clobbering a draft the user may already be typing.
    fetchStrategies({ strategyId: savingID })
  }

  // Update config section
  const updateConfig = <K extends keyof StrategyConfig>(
    section: K,
    value: StrategyConfig[K]
  ) => {
    if (!editingConfig) return
    // 2026-10-03 review P2: functional update — the old closure spread let
    // two rapid calls (the strategy-type switch fires TWO setEditingConfig)
    // overwrite each other's change, leaving a half-switched config.
    setEditingConfig((prev) => (prev ? { ...prev, [section]: value } : prev))
    setHasChanges(true)
  }

  // Fetch prompt preview
  const fetchPromptPreview = async () => {
    if (!token || !editingConfig) return
    setIsLoadingPrompt(true)
    try {
      const response = await fetch(
        `${API_BASE}/api/strategies/preview-prompt`,
        {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            Authorization: `Bearer ${token}`,
          },
          body: JSON.stringify({
            config: editingConfig,
            account_equity: 1000,
            prompt_variant: selectedVariant,
          }),
        }
      )
      if (!response.ok) throw new Error('Failed to fetch prompt preview')
      const data = await response.json()
      setPromptPreview(data)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error')
    } finally {
      setIsLoadingPrompt(false)
    }
  }

  // Run AI test with real AI model
  const runAiTest = async () => {
    if (!token || !editingConfig || !selectedModelId) return
    setIsRunningAiTest(true)
    setAiTestResult(null)
    try {
      const response = await fetch(`${API_BASE}/api/strategies/test-run`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({
          config: editingConfig,
          prompt_variant: selectedVariant,
          ai_model_id: selectedModelId,
          run_real_ai: true,
        }),
      })
      if (!response.ok) throw new Error('Failed to run AI test')
      const data = await response.json()
      setAiTestResult(data)
    } catch (err) {
      setAiTestResult({
        error: err instanceof Error ? err.message : 'Unknown error',
      })
    } finally {
      setIsRunningAiTest(false)
    }
  }

  const tr = (key: string) => t(`strategyStudio.${key}`, language)

  if (isLoading) {
    return (
      <div className="flex items-center justify-center min-h-[70vh]">
        <div className="text-center">
          <div className="relative">
            <div className="w-16 h-16 rounded-full border-4 border-line border-t-brand animate-spin" />
            <Zap className="w-6 h-6 text-brand absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2" />
          </div>
        </div>
      </div>
    )
  }

  // Get current strategy type (default to ai_trading if not set)
  const currentStrategyType = editingConfig?.strategy_type || 'ai_trading'
  // us_stock prompts are built at run time by the program, so the crypto
  // prompt preview / AI test (which call the crypto engine) are not offered.
  const isStockStrategy = currentStrategyType === 'us_stock'
  const effectiveRightTab =
    isStockStrategy && activeRightTab !== 'versions'
      ? 'versions'
      : activeRightTab

  const configSections = [
    // Grid Config - only for grid_trading
    {
      key: 'gridConfig' as const,
      icon: Activity,
      title: tr('gridConfig'),
      forStrategyType: 'grid_trading' as const,
      content: editingConfig?.grid_config && (
        <GridConfigEditor
          config={editingConfig.grid_config}
          onChange={(gridConfig) => updateConfig('grid_config', gridConfig)}
          disabled={selectedStrategy?.is_default}
          language={language}
        />
      ),
    },
    // US Stock Config - only for us_stock (Binance bStock spot)
    {
      key: 'stockConfig' as const,
      icon: BarChart3,
      title: tr('stockConfig'),
      forStrategyType: 'us_stock' as const,
      content: editingConfig?.stock_config && (
        <StockConfigEditor
          config={editingConfig.stock_config}
          onChange={(stockConfig) => updateConfig('stock_config', stockConfig)}
          disabled={selectedStrategy?.is_default}
          language={language}
        />
      ),
    },
    // AI Trading sections
    {
      key: 'coinSource' as const,
      icon: Target,
      title: tr('coinSource'),
      forStrategyType: 'ai_trading' as const,
      content: editingConfig && (
        <CoinSourceEditor
          config={editingConfig.coin_source}
          onChange={(coinSource) => updateConfig('coin_source', coinSource)}
          disabled={selectedStrategy?.is_default}
          language={language}
        />
      ),
    },
    {
      key: 'indicators' as const,
      icon: BarChart3,
      title: tr('indicators'),
      forStrategyType: 'ai_trading' as const,
      content: editingConfig && (
        <IndicatorEditor
          config={editingConfig.indicators}
          onChange={(indicators) => updateConfig('indicators', indicators)}
          disabled={selectedStrategy?.is_default}
          language={language}
        />
      ),
    },
    {
      key: 'riskControl' as const,
      icon: Shield,
      title: tr('riskControl'),
      forStrategyType: 'ai_trading' as const,
      content: editingConfig && (
        <>
          <RiskControlEditor
            config={editingConfig.risk_control}
            onChange={(riskControl) =>
              updateConfig('risk_control', riskControl)
            }
            disabled={selectedStrategy?.is_default}
            language={language}
          />
          <div className="p-3 rounded-lg mt-6 bg-surface-2 border border-line">
            <label className="block text-[13px] mb-1 text-fg-2">
              {ts(riskControl.statsWindow, language)}
            </label>
            <p className="text-xs mb-2 text-fg-3">
              {ts(riskControl.statsWindowDesc, language)}
            </p>
            <Input
              type="number"
              value={editingConfig.stats_window_days ?? 0}
              onChange={(e) =>
                updateConfig(
                  'stats_window_days',
                  e.target.value === '' ? 0 : parseInt(e.target.value)
                )
              }
              disabled={selectedStrategy?.is_default}
              min={-1}
              max={365}
              className="w-32 px-3 bg-surface-2 border border-line text-fg num text-right"
            />
            <p className="text-xs mt-2 font-medium text-up">
              {(editingConfig.stats_window_days ?? 0) === 0
                ? language === 'zh'
                  ? '当前生效: 近 30 天(默认)'
                  : 'Effective: last 30 days (default)'
                : (editingConfig.stats_window_days ?? 0) < 0
                  ? language === 'zh'
                    ? '当前生效: 全量历史'
                    : 'Effective: full history'
                  : language === 'zh'
                    ? `当前生效: 近 ${editingConfig.stats_window_days} 天`
                    : `Effective: last ${editingConfig.stats_window_days} days`}
            </p>
          </div>
        </>
      ),
    },
    {
      key: 'promptSections' as const,
      icon: FileText,
      title: tr('promptSections'),
      forStrategyType: 'ai_trading' as const,
      content: editingConfig && (
        <PromptSectionsEditor
          config={editingConfig.prompt_sections}
          onChange={(promptSections) =>
            updateConfig('prompt_sections', promptSections)
          }
          disabled={selectedStrategy?.is_default}
          language={language}
        />
      ),
    },
    {
      key: 'customPrompt' as const,
      icon: Settings,
      title: tr('customPrompt'),
      forStrategyType: 'ai_trading' as const,
      content: editingConfig && (
        <div>
          <p className="text-xs mb-2 text-fg-3">{tr('customPromptDesc')}</p>
          <textarea
            value={editingConfig.custom_prompt || ''}
            onChange={(e) => updateConfig('custom_prompt', e.target.value)}
            disabled={selectedStrategy?.is_default}
            placeholder={tr('customPromptPlaceholder')}
            className="w-full h-32 px-3 py-2 rounded-md resize-none font-mono text-xs bg-surface-2 border border-line text-fg focus:border-brand focus:outline-none focus:ring-1 focus:ring-brand/40 disabled:opacity-50"
          />
        </div>
      ),
    },
    {
      key: 'publishSettings' as const,
      icon: Globe,
      title: tr('publishSettings'),
      forStrategyType: 'both' as const,
      content: selectedStrategy && (
        <PublishSettingsEditor
          isPublic={selectedStrategy.is_public ?? false}
          configVisible={selectedStrategy.config_visible ?? true}
          onIsPublicChange={(value) => {
            setSelectedStrategy({ ...selectedStrategy, is_public: value })
            setHasChanges(true)
          }}
          onConfigVisibleChange={(value) => {
            setSelectedStrategy({ ...selectedStrategy, config_visible: value })
            setHasChanges(true)
          }}
          disabled={selectedStrategy?.is_default}
          language={language}
        />
      ),
    },
  ].filter(
    (section) =>
      section.forStrategyType === 'both' ||
      section.forStrategyType === currentStrategyType
  )

  return (
    <main className="min-h-screen bg-bg text-fg">
      {/* Header */}
      <div className="px-3 py-3 border-b border-line bg-surface sm:px-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex items-center gap-3">
            <div className="p-2 rounded-md bg-brand-soft">
              <Sparkles className="w-5 h-5 text-brand" />
            </div>
            <div>
              <h1 className="text-xl font-semibold text-fg">{tr('title')}</h1>
              <p className="text-xs text-fg-3">{tr('subtitle')}</p>
            </div>
          </div>
          {error && (
            <div className="flex min-w-0 items-center gap-2 break-words rounded-md bg-down-soft px-3 py-1.5 text-xs text-down">
              {error}
              <Button
                variant="ghost"
                onClick={() => setError(null)}
                className=""
              >
                ×
              </Button>
            </div>
          )}
        </div>
      </div>

      {/* Strategy list, configuration and preview */}
      <div className="mx-auto grid max-w-[1920px] grid-cols-1 gap-3 p-3 xl:grid-cols-[minmax(0,1fr)_380px] 2xl:grid-cols-[minmax(0,1fr)_440px] sm:p-4">
        {/* Strategy List */}
        <div className="min-w-0 rounded-lg border border-line bg-surface xl:col-span-2">
          <div className="p-2">
            <div className="flex items-center justify-between mb-2 px-2">
              <span className="text-xs font-medium text-fg-3">
                {tr('strategies')}
              </span>
              <div className="flex items-center gap-1">
                {/* Import button with hidden file input */}
                <label
                  className="p-1 rounded hover:bg-surface-hover transition-colors cursor-pointer hover:text-fg text-fg-2"
                  title={tr('importStrategy')}
                >
                  <Upload className="w-4 h-4" />
                  <input
                    type="file"
                    accept=".json"
                    onChange={handleImportStrategy}
                    className="hidden accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                  />
                </label>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={handleCreateStrategy}
                  className="text-brand"
                  title={tr('newStrategyTooltip')}
                >
                  <Plus className="w-4 h-4" />
                </Button>
              </div>
            </div>
            <div className="grid max-h-48 grid-cols-1 gap-2 overflow-y-auto p-1 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4">
              {strategies.map((strategy) => (
                <div
                  key={strategy.id}
                  role="button"
                  tabIndex={0}
                  aria-pressed={selectedStrategy?.id === strategy.id}
                  onKeyDown={(e) => {
                    if (
                      e.target === e.currentTarget &&
                      (e.key === 'Enter' || e.key === ' ')
                    ) {
                      e.preventDefault()
                      e.currentTarget.click()
                    }
                  }}
                  onClick={() => {
                    setSelectedStrategy(strategy)
                    setEditingConfig(strategy.config)
                    setHasChanges(false)
                    setPromptPreview(null)
                    setAiTestResult(null)
                  }}
                  className={`group min-w-0 px-3 py-2 rounded-md cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 ${
                    selectedStrategy?.id === strategy.id
                      ? 'ring-1 ring-brand bg-brand-soft'
                      : 'hover:bg-surface-hover ring-1 ring-line bg-surface-2'
                  }`}
                >
                  <div className="flex items-start justify-between">
                    <span
                      className={`min-w-0 break-words line-clamp-2 text-fg ${language === 'zh' ? 'text-sm' : 'text-xs'}`}
                    >
                      {strategy.name}
                    </span>
                    <div className="flex shrink-0 items-center gap-0.5">
                      <Button
                        variant="ghost"
                        size="sm"
                        aria-label={tr('export')}
                        onClick={(e) => {
                          e.stopPropagation()
                          handleExportStrategy(strategy)
                        }}
                        className="w-7 px-0 text-fg-3"
                        title={tr('export')}
                      >
                        <Download className="w-3 h-3" />
                      </Button>
                      {!strategy.is_default && (
                        <>
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={(e) => {
                              e.stopPropagation()
                              handleDuplicateStrategy(strategy.id)
                            }}
                            className="w-7 px-0 text-fg-3"
                            title={tr('duplicate')}
                          >
                            <Copy className="w-3 h-3" />
                          </Button>
                          <Button
                            variant="danger"
                            size="sm"
                            onClick={(e) => {
                              e.stopPropagation()
                              handleDeleteStrategy(strategy.id)
                            }}
                            className="w-7 px-0 text-down"
                            title={tr('deleteTooltip')}
                          >
                            <Trash2 className="w-3 h-3" />
                          </Button>
                        </>
                      )}
                    </div>
                  </div>
                  <div className="flex items-center gap-1 mt-1 flex-wrap">
                    {strategy.is_active && (
                      <Badge variant="up" size="xs">
                        {tr('active')}
                      </Badge>
                    )}
                    {strategy.is_default && (
                      <Badge variant="brand" size="xs">
                        {tr('default')}
                      </Badge>
                    )}
                    {strategy.is_public && (
                      <Badge variant="info" size="xs">
                        <Globe className="w-2.5 h-2.5" />
                        {tr('public')}
                      </Badge>
                    )}
                  </div>
                </div>
              ))}
            </div>
          </div>
        </div>

        {/* Config Editor */}
        <div className="min-w-0">
          {selectedStrategy && editingConfig ? (
            <div className="space-y-3">
              {/* Strategy Name & Actions */}
              <div className="sticky top-16 z-20 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-line bg-surface p-3">
                <div className="min-w-0 basis-full sm:flex-1 sm:basis-0">
                  <Input
                    type="text"
                    value={selectedStrategy.name}
                    onChange={(e) => {
                      setSelectedStrategy({
                        ...selectedStrategy,
                        name: e.target.value,
                      })
                      setHasChanges(true)
                    }}
                    disabled={selectedStrategy.is_default}
                    aria-label={tr('strategies')}
                    className="w-full font-semibold"
                  />
                  <Input
                    type="text"
                    value={selectedStrategy.description || ''}
                    onChange={(e) => {
                      setSelectedStrategy({
                        ...selectedStrategy,
                        description: e.target.value,
                      })
                      setHasChanges(true)
                    }}
                    disabled={selectedStrategy.is_default}
                    placeholder={tr('addDescription')}
                    aria-label={tr('addDescription')}
                    className="w-full mt-2"
                  />
                  {hasChanges && (
                    <span className="text-xs text-brand">
                      ● {tr('unsaved')}
                    </span>
                  )}
                </div>
                <div className="flex items-center gap-2 flex-shrink-0">
                  {!selectedStrategy.is_active && (
                    <Button
                      variant="secondary"
                      size="sm"
                      onClick={() =>
                        handleActivateStrategy(selectedStrategy.id)
                      }
                      className="flex items-center gap-1 text-up"
                    >
                      <Check className="w-3 h-3" />
                      {tr('activate')}
                    </Button>
                  )}
                  {!selectedStrategy.is_default && (
                    <Button
                      variant="primary"
                      onClick={handleSaveStrategy}
                      disabled={isSaving || !hasChanges}
                    >
                      <Save className="w-3.5 h-3.5" />
                      {isSaving ? tr('saving') : tr('save')}
                    </Button>
                  )}
                </div>
              </div>

              {/* Token Estimate Bar */}
              {currentStrategyType === 'ai_trading' && (
                <div className="mb-4">
                  <TokenEstimateBar
                    config={editingConfig}
                    language={language}
                    onTokenCountChange={setEstimatedTokens}
                  />
                </div>
              )}

              {/* Strategy Type Selector */}
              {editingConfig && (
                <div className="mb-3 p-3 rounded-lg bg-surface border border-line">
                  <div className="flex items-center gap-2 mb-3">
                    <Zap className="w-4 h-4 text-brand" />
                    <span className="text-sm font-medium text-fg">
                      {tr('strategyType')}
                    </span>
                  </div>
                  <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
                    <button
                      onClick={() => {
                        if (!selectedStrategy?.is_default) {
                          updateConfig('strategy_type', 'ai_trading')
                          // Clear grid config when switching to AI trading
                          updateConfig('grid_config', undefined)
                          updateConfig('stock_config', undefined)
                        }
                      }}
                      disabled={selectedStrategy?.is_default}
                      className={cn(
                        `p-3 rounded-md border transition-colors text-left ${
                          !editingConfig.strategy_type ||
                          editingConfig.strategy_type === 'ai_trading'
                            ? 'border-brand bg-brand-soft'
                            : 'border-line hover:border-brand/50'
                        }`,
                        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50'
                      )}
                    >
                      <div className="flex items-center gap-2 mb-1">
                        <Bot className="w-4 h-4 text-brand" />
                        <span className="text-sm font-medium text-fg">
                          {tr('aiTrading')}
                        </span>
                      </div>
                      <p className="text-xs text-fg-3 text-left">
                        {tr('aiTradingDesc')}
                      </p>
                    </button>
                    <button
                      onClick={() => {
                        if (!selectedStrategy?.is_default) {
                          updateConfig('strategy_type', 'grid_trading')
                          // Initialize grid config if not exists
                          if (!editingConfig.grid_config) {
                            updateConfig('grid_config', defaultGridConfig)
                          }
                        }
                      }}
                      disabled={selectedStrategy?.is_default}
                      className={cn(
                        `p-3 rounded-md border transition-colors text-left ${
                          editingConfig.strategy_type === 'grid_trading'
                            ? 'border-brand bg-brand-soft'
                            : 'border-line hover:border-brand/50'
                        }`,
                        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50'
                      )}
                    >
                      <div className="flex items-center gap-2 mb-1">
                        <Activity className="w-4 h-4 text-up" />
                        <span className="text-sm font-medium text-fg">
                          {tr('gridTrading')}
                        </span>
                      </div>
                      <p className="text-xs text-fg-3 text-left">
                        {tr('gridTradingDesc')}
                      </p>
                    </button>
                    <button
                      data-testid="strategy-type-us-stock"
                      onClick={() => {
                        if (!selectedStrategy?.is_default) {
                          updateConfig('strategy_type', 'us_stock')
                          // Initialize stock config if not exists (paper ON,
                          // regular session only, no symbols yet)
                          if (!editingConfig.stock_config) {
                            updateConfig('stock_config', defaultStockConfig)
                          }
                        }
                      }}
                      disabled={selectedStrategy?.is_default}
                      className={cn(
                        `p-3 rounded-md border transition-colors text-left ${
                          editingConfig.strategy_type === 'us_stock'
                            ? 'border-brand bg-brand-soft'
                            : 'border-line hover:border-brand/50'
                        }`,
                        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50'
                      )}
                    >
                      <div className="flex items-center gap-2 mb-1">
                        <BarChart3 className="w-4 h-4 text-brand" />
                        <span className="text-sm font-medium text-fg">
                          {tr('usStock')}
                        </span>
                      </div>
                      <p className="text-xs text-fg-3 text-left">
                        {tr('usStockDesc')}
                      </p>
                    </button>
                  </div>
                </div>
              )}

              {/* Config Sections */}
              <div className="space-y-3">
                {configSections.map(({ key, icon: Icon, title, content }) => (
                  <Card key={key} dense className="min-w-0 overflow-hidden">
                    <CardHeader className="p-0 [&>div]:w-full">
                      <button
                        type="button"
                        onClick={() => toggleSection(key)}
                        aria-expanded={expandedSections[key]}
                        aria-controls={`strategy-section-${key}`}
                        className="flex min-h-9 w-full items-center justify-between gap-2 px-3 py-2 text-left hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50"
                      >
                        <span className="flex items-center gap-2 text-sm font-semibold">
                          <Icon className="h-4 w-4 text-brand" />
                          {title}
                        </span>
                        {expandedSections[key] ? (
                          <ChevronDown className="h-4 w-4 text-fg-3" />
                        ) : (
                          <ChevronRight className="h-4 w-4 text-fg-3" />
                        )}
                      </button>
                    </CardHeader>
                    {expandedSections[key] && (
                      <CardBody id={`strategy-section-${key}`}>
                        {content}
                      </CardBody>
                    )}
                  </Card>
                ))}
              </div>
            </div>
          ) : (
            <Card dense>
              <EmptyState
                icon={<Activity className="h-8 w-8" />}
                title={tr('selectOrCreate')}
              />
            </Card>
          )}
        </div>

        {/* Sticky Prompt Preview & AI Test */}
        <div className="min-w-0 self-start rounded-lg border border-line bg-surface xl:sticky xl:top-20 xl:max-h-[calc(100vh-96px)] xl:flex xl:flex-col">
          <Tabs
            value={effectiveRightTab}
            onChange={setActiveRightTab}
            className="shrink-0 gap-3 overflow-x-auto px-3"
            items={[
              ...(!isStockStrategy
                ? [
                    {
                      key: 'prompt' as const,
                      label: tr('promptPreview'),
                      icon: <Eye className="h-3.5 w-3.5" />,
                    },
                    {
                      key: 'test' as const,
                      label: tr('aiTestRun'),
                      icon: <Play className="h-3.5 w-3.5" />,
                    },
                  ]
                : []),
              {
                key: 'versions' as const,
                label: t('strategyVersions.tab', language),
                icon: <History className="h-3.5 w-3.5" />,
              },
            ]}
          />

          {isStockStrategy && (
            <p className="flex-shrink-0 px-3 py-2 text-xs text-fg-3 border-b border-line">
              {tr('stockPromptRuntimeNote')}
            </p>
          )}

          {/* Tab Content */}
          <div className="min-h-0 min-w-0 flex-1 overflow-y-auto">
            {effectiveRightTab === 'versions' ? (
              selectedStrategy ? (
                <VersionsPanel
                  strategyId={selectedStrategy.id}
                  token={token}
                  language={language}
                  refreshKey={selectedStrategy.updated_at}
                />
              ) : (
                <div className="flex flex-col items-center justify-center py-12 text-fg-3">
                  <History className="w-10 h-10 mb-2 opacity-30" />
                  <p className="text-sm">{tr('selectOrCreate')}</p>
                </div>
              )
            ) : effectiveRightTab === 'prompt' ? (
              /* Prompt Preview Tab */
              <div className="p-3 space-y-3">
                {/* Controls */}
                <div className="flex items-center gap-2 flex-wrap">
                  <select
                    value={selectedVariant}
                    onChange={(e) => setSelectedVariant(e.target.value)}
                    className="px-2 border border-line text-fg outline-none h-8 rounded-md bg-surface-2 text-[13px] focus:border-brand focus:outline-none focus:ring-1 focus:ring-brand/40 disabled:opacity-50"
                  >
                    <option value="balanced">{tr('balanced')}</option>
                    <option value="aggressive">{tr('aggressive')}</option>
                    <option value="conservative">{tr('conservative')}</option>
                  </select>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={fetchPromptPreview}
                    disabled={isLoadingPrompt || !editingConfig}
                    className="flex items-center gap-1.5 text-fg"
                  >
                    {isLoadingPrompt ? (
                      <Loader2 className="w-3 h-3 animate-spin" />
                    ) : (
                      <RefreshCw className="w-3 h-3" />
                    )}
                    {promptPreview ? tr('refreshPrompt') : tr('loadPrompt')}
                  </Button>
                </div>

                {promptPreview ? (
                  <>
                    {/* Config Summary */}
                    <div className="p-2 rounded-lg bg-bg border border-line">
                      <div className="flex items-center gap-1.5 mb-2">
                        <Code className="w-3 h-3 text-ai" />
                        <span className="text-xs font-medium text-ai">
                          Config
                        </span>
                      </div>
                      <div className="grid grid-cols-3 gap-2 text-xs">
                        {Object.entries(promptPreview.config_summary || {}).map(
                          ([key, value]) => (
                            <div key={key}>
                              <div className="text-fg-3">
                                {key.replace(/_/g, ' ')}
                              </div>
                              <div className="num break-words text-fg">
                                {String(value)}
                              </div>
                            </div>
                          )
                        )}
                      </div>
                    </div>

                    {/* System Prompt */}
                    <div>
                      <div className="flex items-center justify-between mb-1.5">
                        <div className="flex items-center gap-1.5">
                          <FileText className="w-3 h-3 text-ai" />
                          <span className="text-xs font-medium text-fg">
                            {tr('systemPrompt')}
                          </span>
                        </div>
                        <span className="num text-xs px-1.5 py-0.5 rounded bg-surface-2 text-fg-3">
                          {promptPreview.system_prompt.length.toLocaleString()}{' '}
                          chars
                        </span>
                      </div>
                      <pre className="max-h-[400px] p-2 rounded-lg text-[11px] font-mono overflow-auto bg-bg border border-line text-fg">
                        {promptPreview.system_prompt}
                      </pre>
                    </div>
                  </>
                ) : (
                  <div className="flex flex-col items-center justify-center py-12 text-fg-3">
                    <Eye className="w-10 h-10 mb-2 opacity-30" />
                    <p className="text-sm">{tr('generatePromptPreview')}</p>
                  </div>
                )}
              </div>
            ) : (
              /* AI Test Tab */
              <div className="p-3 space-y-3">
                {/* Controls */}
                <div className="space-y-2">
                  <div className="flex items-center gap-2">
                    <Bot className="w-4 h-4 text-up" />
                    <span className="text-xs font-medium text-fg">
                      {tr('selectModel')}
                    </span>
                  </div>
                  {aiModels.length > 0 ? (
                    <select
                      value={selectedModelId}
                      onChange={(e) => setSelectedModelId(e.target.value)}
                      className="w-full px-3 border border-line text-fg h-8 rounded-md bg-surface-2 text-[13px] focus:border-brand focus:outline-none focus:ring-1 focus:ring-brand/40 disabled:opacity-50"
                    >
                      {aiModels.map((model) => (
                        <option key={model.id} value={model.id}>
                          {model.name} ({model.provider})
                        </option>
                      ))}
                    </select>
                  ) : (
                    <div className="px-3 py-2 rounded-lg text-sm bg-down-soft text-down">
                      {tr('noModel')}
                    </div>
                  )}

                  <div className="flex items-center gap-2">
                    <select
                      value={selectedVariant}
                      onChange={(e) => setSelectedVariant(e.target.value)}
                      className="px-2 border border-line text-fg h-8 rounded-md bg-surface-2 text-[13px] focus:border-brand focus:outline-none focus:ring-1 focus:ring-brand/40 disabled:opacity-50"
                    >
                      <option value="balanced">{tr('balanced')}</option>
                      <option value="aggressive">{tr('aggressive')}</option>
                      <option value="conservative">{tr('conservative')}</option>
                    </select>
                    <Button
                      variant="secondary"
                      onClick={runAiTest}
                      disabled={
                        isRunningAiTest || !editingConfig || !selectedModelId
                      }
                      className="flex-1 flex items-center justify-center gap-2 text-fg"
                    >
                      {isRunningAiTest ? (
                        <>
                          <Loader2 className="w-4 h-4 animate-spin" />
                          {tr('running')}
                        </>
                      ) : (
                        <>
                          <Send className="w-4 h-4" />
                          {tr('runTest')}
                        </>
                      )}
                    </Button>
                  </div>
                  <p className="text-xs text-fg-3">{tr('testNote')}</p>
                </div>

                {/* Test Results */}
                {aiTestResult ? (
                  <div className="space-y-3">
                    {aiTestResult.error ? (
                      <div className="p-3 rounded-lg bg-down-soft border border-down/30">
                        <p className="text-sm text-down">
                          {aiTestResult.error}
                        </p>
                      </div>
                    ) : (
                      <>
                        {aiTestResult.duration_ms && (
                          <div className="flex items-center gap-2">
                            <Clock className="w-3 h-3 text-fg-3" />
                            <span className="num text-xs text-fg-3">
                              {tr('duration')}:{' '}
                              {(aiTestResult.duration_ms / 1000).toFixed(2)}s
                            </span>
                          </div>
                        )}

                        {/* User Prompt Input */}
                        {aiTestResult.user_prompt && (
                          <div>
                            <div className="flex items-center gap-1.5 mb-1.5">
                              <Terminal className="w-3 h-3 text-info" />
                              <span className="text-xs font-medium text-fg">
                                {tr('userPrompt')} (Input)
                              </span>
                            </div>
                            <pre className="max-h-[200px] p-2 rounded-lg text-xs font-mono overflow-auto bg-bg border border-line text-fg">
                              {aiTestResult.user_prompt}
                            </pre>
                          </div>
                        )}

                        {/* AI Reasoning */}
                        {aiTestResult.reasoning && (
                          <div>
                            <div className="flex items-center gap-1.5 mb-1.5">
                              <Sparkles className="w-3 h-3 text-brand" />
                              <span className="text-xs font-medium text-fg">
                                {tr('reasoning')}
                              </span>
                            </div>
                            <pre className="max-h-[200px] p-2 rounded-lg text-xs font-mono overflow-auto whitespace-pre-wrap bg-bg border border-brand/30 text-fg">
                              {aiTestResult.reasoning}
                            </pre>
                          </div>
                        )}

                        {/* AI Decisions */}
                        {aiTestResult.decisions &&
                          aiTestResult.decisions.length > 0 && (
                            <div>
                              <div className="flex items-center gap-1.5 mb-1.5">
                                <Activity className="w-3 h-3 text-up" />
                                <span className="text-xs font-medium text-fg">
                                  {tr('decisions')}
                                </span>
                              </div>
                              <pre className="max-h-[200px] p-2 rounded-lg text-xs font-mono overflow-auto bg-bg border border-up/30 text-fg">
                                {JSON.stringify(
                                  aiTestResult.decisions,
                                  null,
                                  2
                                )}
                              </pre>
                            </div>
                          )}

                        {/* Raw AI Response */}
                        {aiTestResult.ai_response && (
                          <div>
                            <div className="flex items-center gap-1.5 mb-1.5">
                              <FileText className="w-3 h-3 text-fg-3" />
                              <span className="text-xs font-medium text-fg">
                                {tr('aiOutput')} (Raw)
                              </span>
                            </div>
                            <pre className="max-h-[300px] p-2 rounded-lg text-xs font-mono overflow-auto whitespace-pre-wrap bg-bg border border-line text-fg">
                              {aiTestResult.ai_response}
                            </pre>
                          </div>
                        )}
                      </>
                    )}
                  </div>
                ) : (
                  <div className="flex flex-col items-center justify-center py-12 text-fg-3">
                    <Play className="w-10 h-10 mb-2 opacity-30" />
                    <p className="text-sm">{tr('runAiTestHint')}</p>
                  </div>
                )}
              </div>
            )}
          </div>
        </div>
      </div>
    </main>
  )
}

export default StrategyStudioPage
