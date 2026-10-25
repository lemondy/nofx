import { cn } from '../../lib/cn'
import { Input } from '../ui'
import {
  Clock,
  Activity,
  TrendingUp,
  BarChart2,
  Info,
  Lock,
  Zap,
} from 'lucide-react'
import type { IndicatorConfig } from '../../types'
import { indicator, ts } from '../../i18n/strategy-translations'
import { NofxSelect } from '../ui/select'

// Default NofxOS API Key

interface IndicatorEditorProps {
  config: IndicatorConfig
  onChange: (config: IndicatorConfig) => void
  disabled?: boolean
  language: string
}

// All available timeframes
const allTimeframes = [
  { value: '1m', label: '1m', category: 'scalp' },
  { value: '3m', label: '3m', category: 'scalp' },
  { value: '5m', label: '5m', category: 'scalp' },
  { value: '15m', label: '15m', category: 'intraday' },
  { value: '30m', label: '30m', category: 'intraday' },
  { value: '1h', label: '1h', category: 'intraday' },
  { value: '2h', label: '2h', category: 'swing' },
  { value: '4h', label: '4h', category: 'swing' },
  { value: '6h', label: '6h', category: 'swing' },
  { value: '8h', label: '8h', category: 'swing' },
  { value: '12h', label: '12h', category: 'swing' },
  { value: '1d', label: '1D', category: 'position' },
  { value: '3d', label: '3D', category: 'position' },
  { value: '1w', label: '1W', category: 'position' },
]

export function IndicatorEditor({
  config,
  onChange,
  disabled,
  language,
}: IndicatorEditorProps) {
  // Get currently selected timeframes
  const selectedTimeframes = config.klines.selected_timeframes || [
    config.klines.primary_timeframe,
  ]

  // Toggle timeframe selection
  const toggleTimeframe = (tf: string) => {
    if (disabled) return
    const current = [...selectedTimeframes]
    const index = current.indexOf(tf)

    if (index >= 0) {
      if (current.length > 1) {
        current.splice(index, 1)
        const newPrimary =
          tf === config.klines.primary_timeframe
            ? current[0]
            : config.klines.primary_timeframe
        onChange({
          ...config,
          klines: {
            ...config.klines,
            selected_timeframes: current,
            primary_timeframe: newPrimary,
            enable_multi_timeframe: current.length > 1,
          },
        })
      }
    } else {
      if (current.length >= 4) {
        // Show toast notification
        const toast = document.createElement('div')
        toast.textContent =
          language === 'zh'
            ? '最多选择 4 个时间维度'
            : 'Maximum 4 timeframes allowed'
        toast.className =
          'fixed top-4 left-1/2 -translate-x-1/2 px-4 py-2 rounded-lg text-sm z-50 shadow-pop border border-down/30 bg-down-soft text-down'
        document.body.appendChild(toast)
        setTimeout(() => toast.remove(), 2000)
        return
      }
      current.push(tf)
      onChange({
        ...config,
        klines: {
          ...config.klines,
          selected_timeframes: current,
          enable_multi_timeframe: current.length > 1,
        },
      })
    }
  }

  // Set primary timeframe
  const setPrimaryTimeframe = (tf: string) => {
    if (disabled) return
    onChange({
      ...config,
      klines: {
        ...config.klines,
        primary_timeframe: tf,
      },
    })
  }

  const categoryColors: Record<string, string> = {
    scalp: 'text-down',
    intraday: 'text-brand',
    swing: 'text-up',
    position: 'text-info',
  }

  // Ensure enable_raw_klines is always true
  const ensureRawKlines = () => {
    if (!config.enable_raw_klines) {
      onChange({ ...config, enable_raw_klines: true })
    }
  }

  // Call on mount if needed
  if (
    config.enable_raw_klines === undefined ||
    config.enable_raw_klines === false
  ) {
    ensureRawKlines()
  }

  return (
    <div className="space-y-4">
      {/* ============================================ */}
      {/* NofxOS Data Provider - Top Configuration */}
      {/* ============================================ */}
      <div className="rounded-lg overflow-hidden relative bg-surface border border-line">
        <div className="p-3">
          {/* Header Row */}
          <div className="flex items-center justify-between mb-3">
            <div className="flex items-center gap-2">
              <div className="w-8 h-8 rounded-lg flex items-center justify-center bg-ai-soft text-ai">
                <Zap className="w-4 h-4 text-ai" />
              </div>
              <div>
                <h3 className="text-sm font-semibold text-fg">
                  {ts(indicator.nofxosTitle, language)}
                </h3>
                <span className="text-xs text-fg-3">
                  {ts(indicator.nofxosFeatures, language)}
                </span>
              </div>
            </div>
          </div>

          {/* NofxOS Data Sources Grid */}
          <div className="mt-4">
            <div className="text-xs font-medium mb-2 text-fg-3">
              {ts(indicator.nofxosDataSources, language)}
            </div>
            <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
              {/* Quant Data */}
              <div
                className={cn(
                  'p-2.5 rounded-lg transition-colors cursor-pointer',
                  config.enable_quant_data ? 'bg-brand-soft' : 'bg-surface-2',
                  config.enable_quant_data
                    ? 'border border-brand'
                    : 'border border-line'
                )}
                style={{ opacity: disabled ? 0.5 : 1 }}
                onClick={() =>
                  !disabled &&
                  onChange({
                    ...config,
                    enable_quant_data: !config.enable_quant_data,
                  })
                }
              >
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <div className="w-2 h-2 rounded-full bg-info" />
                    <span className="text-xs font-medium text-fg">
                      {ts(indicator.quantData, language)}
                    </span>
                  </div>
                  <input
                    type="checkbox"
                    checked={config.enable_quant_data || false}
                    onChange={(e) => {
                      e.stopPropagation()
                      if (!disabled)
                        onChange({
                          ...config,
                          enable_quant_data: e.target.checked,
                        })
                    }}
                    disabled={disabled}
                    className="w-3.5 h-3.5 rounded accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                  />
                </div>
                <p className="text-xs mt-1 text-fg-3">
                  {ts(indicator.quantDataDesc, language)}
                </p>
                {config.enable_quant_data && (
                  <div className="flex gap-3 mt-2">
                    <label className="flex items-center gap-1.5 cursor-pointer text-fg-2">
                      <input
                        type="checkbox"
                        checked={config.enable_quant_oi !== false}
                        onChange={(e) => {
                          e.stopPropagation()
                          if (!disabled)
                            onChange({
                              ...config,
                              enable_quant_oi: e.target.checked,
                            })
                        }}
                        disabled={disabled}
                        className="w-3 h-3 rounded accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                      />
                      <span className="text-xs text-fg">OI</span>
                    </label>
                    <label className="flex items-center gap-1.5 cursor-pointer text-fg-2">
                      <input
                        type="checkbox"
                        checked={config.enable_quant_netflow !== false}
                        onChange={(e) => {
                          e.stopPropagation()
                          if (!disabled)
                            onChange({
                              ...config,
                              enable_quant_netflow: e.target.checked,
                            })
                        }}
                        disabled={disabled}
                        className="w-3 h-3 rounded accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                      />
                      <span className="text-xs text-fg">Netflow</span>
                    </label>
                  </div>
                )}
              </div>

              {/* OI Ranking */}
              <div
                className={cn(
                  'p-2.5 rounded-lg transition-colors cursor-pointer',
                  config.enable_oi_ranking ? 'bg-brand-soft' : 'bg-surface-2',
                  config.enable_oi_ranking
                    ? 'border border-brand'
                    : 'border border-line'
                )}
                style={{ opacity: disabled ? 0.5 : 1 }}
                onClick={() =>
                  !disabled &&
                  onChange({
                    ...config,
                    enable_oi_ranking: !config.enable_oi_ranking,
                    ...(!config.enable_oi_ranking && !config.oi_ranking_duration
                      ? { oi_ranking_duration: '1h' }
                      : {}),
                    ...(!config.enable_oi_ranking && !config.oi_ranking_limit
                      ? { oi_ranking_limit: 10 }
                      : {}),
                  })
                }
              >
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <div className="w-2 h-2 rounded-full bg-up" />
                    <span className="text-xs font-medium text-fg">
                      {ts(indicator.oiRanking, language)}
                    </span>
                  </div>
                  <input
                    type="checkbox"
                    checked={config.enable_oi_ranking || false}
                    onChange={(e) => {
                      e.stopPropagation()
                      if (!disabled)
                        onChange({
                          ...config,
                          enable_oi_ranking: e.target.checked,
                          ...(e.target.checked && !config.oi_ranking_duration
                            ? { oi_ranking_duration: '1h' }
                            : {}),
                          ...(e.target.checked && !config.oi_ranking_limit
                            ? { oi_ranking_limit: 10 }
                            : {}),
                        })
                    }}
                    disabled={disabled}
                    className="w-3.5 h-3.5 rounded accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                  />
                </div>
                <p className="text-xs mt-1 text-fg-3">
                  {ts(indicator.oiRankingDesc, language)}
                </p>
                {config.enable_oi_ranking && (
                  <div
                    className="flex gap-2 mt-2"
                    onClick={(e) => e.stopPropagation()}
                  >
                    <NofxSelect
                      value={config.oi_ranking_duration || '1h'}
                      onChange={(val) =>
                        !disabled &&
                        onChange({ ...config, oi_ranking_duration: val })
                      }
                      disabled={disabled}
                      className="flex-1 px-2 text-xs bg-surface-2 border border-line text-fg h-8 rounded-md bg-surface-2 text-[13px] hover:border-line-strong"
                      options={[
                        { value: '1h', label: '1h' },
                        { value: '4h', label: '4h' },
                        { value: '24h', label: '24h' },
                      ]}
                    />
                    <NofxSelect
                      value={config.oi_ranking_limit || 10}
                      onChange={(val) =>
                        !disabled &&
                        onChange({ ...config, oi_ranking_limit: parseInt(val) })
                      }
                      disabled={disabled}
                      className="w-14 px-2 text-xs bg-surface-2 border border-line text-fg h-8 rounded-md bg-surface-2 text-[13px] hover:border-line-strong"
                      options={[5, 10, 15, 20].map((n) => ({
                        value: n,
                        label: String(n),
                      }))}
                    />
                  </div>
                )}
              </div>

              {/* NetFlow Ranking */}
              <div
                className={cn(
                  'p-2.5 rounded-lg transition-colors cursor-pointer',
                  config.enable_netflow_ranking
                    ? 'bg-brand-soft'
                    : 'bg-surface-2',
                  config.enable_netflow_ranking
                    ? 'border border-brand'
                    : 'border border-line'
                )}
                style={{ opacity: disabled ? 0.5 : 1 }}
                onClick={() =>
                  !disabled &&
                  onChange({
                    ...config,
                    enable_netflow_ranking: !config.enable_netflow_ranking,
                    ...(!config.enable_netflow_ranking &&
                    !config.netflow_ranking_duration
                      ? { netflow_ranking_duration: '1h' }
                      : {}),
                    ...(!config.enable_netflow_ranking &&
                    !config.netflow_ranking_limit
                      ? { netflow_ranking_limit: 10 }
                      : {}),
                  })
                }
              >
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <div className="w-2 h-2 rounded-full bg-warn" />
                    <span className="text-xs font-medium text-fg">
                      {ts(indicator.netflowRanking, language)}
                    </span>
                  </div>
                  <input
                    type="checkbox"
                    checked={config.enable_netflow_ranking || false}
                    onChange={(e) => {
                      e.stopPropagation()
                      if (!disabled)
                        onChange({
                          ...config,
                          enable_netflow_ranking: e.target.checked,
                          ...(e.target.checked &&
                          !config.netflow_ranking_duration
                            ? { netflow_ranking_duration: '1h' }
                            : {}),
                          ...(e.target.checked && !config.netflow_ranking_limit
                            ? { netflow_ranking_limit: 10 }
                            : {}),
                        })
                    }}
                    disabled={disabled}
                    className="w-3.5 h-3.5 rounded accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                  />
                </div>
                <p className="text-xs mt-1 text-fg-3">
                  {ts(indicator.netflowRankingDesc, language)}
                </p>
                {config.enable_netflow_ranking && (
                  <div
                    className="flex gap-2 mt-2"
                    onClick={(e) => e.stopPropagation()}
                  >
                    <NofxSelect
                      value={config.netflow_ranking_duration || '1h'}
                      onChange={(val) =>
                        !disabled &&
                        onChange({ ...config, netflow_ranking_duration: val })
                      }
                      disabled={disabled}
                      className="flex-1 px-2 text-xs bg-surface-2 border border-line text-fg h-8 rounded-md bg-surface-2 text-[13px] hover:border-line-strong"
                      options={[
                        { value: '1h', label: '1h' },
                        { value: '4h', label: '4h' },
                        { value: '24h', label: '24h' },
                      ]}
                    />
                    <NofxSelect
                      value={config.netflow_ranking_limit || 10}
                      onChange={(val) =>
                        !disabled &&
                        onChange({
                          ...config,
                          netflow_ranking_limit: parseInt(val),
                        })
                      }
                      disabled={disabled}
                      className="w-14 px-2 text-xs bg-surface-2 border border-line text-fg h-8 rounded-md bg-surface-2 text-[13px] hover:border-line-strong"
                      options={[5, 10, 15, 20].map((n) => ({
                        value: n,
                        label: String(n),
                      }))}
                    />
                  </div>
                )}
              </div>

              {/* Price Ranking */}
              <div
                className={cn(
                  'p-3 rounded-md border transition-colors cursor-pointer',
                  config.enable_price_ranking
                    ? 'bg-brand-soft border-brand'
                    : 'bg-surface-2 border-line',
                  disabled && 'opacity-50'
                )}
                onClick={() =>
                  !disabled &&
                  onChange({
                    ...config,
                    enable_price_ranking: !config.enable_price_ranking,
                    ...(!config.enable_price_ranking &&
                    !config.price_ranking_duration
                      ? { price_ranking_duration: '1h,4h,24h' }
                      : {}),
                    ...(!config.enable_price_ranking &&
                    !config.price_ranking_limit
                      ? { price_ranking_limit: 10 }
                      : {}),
                  })
                }
              >
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <div className="w-2 h-2 rounded-full bg-ai" />
                    <span className="text-xs font-medium text-fg">
                      {ts(indicator.priceRanking, language)}
                    </span>
                  </div>
                  <input
                    type="checkbox"
                    checked={config.enable_price_ranking || false}
                    onChange={(e) => {
                      e.stopPropagation()
                      if (!disabled)
                        onChange({
                          ...config,
                          enable_price_ranking: e.target.checked,
                          ...(e.target.checked && !config.price_ranking_duration
                            ? { price_ranking_duration: '1h,4h,24h' }
                            : {}),
                          ...(e.target.checked && !config.price_ranking_limit
                            ? { price_ranking_limit: 10 }
                            : {}),
                        })
                    }}
                    disabled={disabled}
                    className="w-3.5 h-3.5 rounded accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                  />
                </div>
                <p className="text-xs mt-1 text-fg-3">
                  {ts(indicator.priceRankingDesc, language)}
                </p>
                {config.enable_price_ranking && (
                  <div
                    className="flex gap-2 mt-2"
                    onClick={(e) => e.stopPropagation()}
                  >
                    <NofxSelect
                      value={config.price_ranking_duration || '1h,4h,24h'}
                      onChange={(val) =>
                        !disabled &&
                        onChange({ ...config, price_ranking_duration: val })
                      }
                      disabled={disabled}
                      className="flex-1 px-2 text-xs bg-surface-2 border border-line text-fg h-8 rounded-md bg-surface-2 text-[13px] hover:border-line-strong"
                      options={[
                        { value: '1h', label: '1h' },
                        { value: '4h', label: '4h' },
                        { value: '24h', label: '24h' },
                        {
                          value: '1h,4h,24h',
                          label: ts(indicator.priceRankingMulti, language),
                        },
                      ]}
                    />
                    <NofxSelect
                      value={config.price_ranking_limit || 10}
                      onChange={(val) =>
                        !disabled &&
                        onChange({
                          ...config,
                          price_ranking_limit: parseInt(val),
                        })
                      }
                      disabled={disabled}
                      className="w-14 px-2 text-xs bg-surface-2 border border-line text-fg h-8 rounded-md bg-surface-2 text-[13px] hover:border-line-strong"
                      options={[5, 10, 15, 20].map((n) => ({
                        value: n,
                        label: String(n),
                      }))}
                    />
                  </div>
                )}
              </div>
            </div>
          </div>
        </div>
      </div>

      {/* ============================================ */}
      {/* Section 1: Market Data (Required) */}
      {/* ============================================ */}
      <div className="rounded-lg overflow-hidden bg-surface-2 border border-line">
        <div className="px-3 py-2 flex items-center gap-2 bg-surface-2 border-b border-line">
          <BarChart2 className="w-4 h-4 text-brand" />
          <span className="text-sm font-medium text-fg">
            {ts(indicator.marketData, language)}
          </span>
          <span className="text-xs text-fg-3">
            - {ts(indicator.marketDataDesc, language)}
          </span>
        </div>

        <div className="p-3 space-y-4">
          {/* Raw Klines - Required, Always On */}
          <div className="flex items-center justify-between p-3 rounded-lg bg-brand-soft border border-line">
            <div className="flex items-center gap-3">
              <div className="w-8 h-8 rounded-lg flex items-center justify-center bg-brand-soft">
                <TrendingUp className="w-4 h-4 text-brand" />
              </div>
              <div>
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium text-fg">
                    {ts(indicator.rawKlines, language)}
                  </span>
                  <span className="px-1.5 py-0.5 rounded text-xs font-medium flex items-center gap-1 bg-brand-soft text-brand">
                    <Lock className="w-2.5 h-2.5" />
                    {ts(indicator.required, language)}
                  </span>
                </div>
                <p className="text-xs mt-0.5 text-fg-3">
                  {ts(indicator.rawKlinesDesc, language)}
                </p>
              </div>
            </div>
            <input
              type="checkbox"
              checked={true}
              disabled={true}
              className="w-5 h-5 rounded cursor-not-allowed accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
            />
          </div>

          {/* Timeframe Selection */}
          <div>
            <div className="flex flex-wrap items-center justify-between gap-2 mb-2">
              <div className="flex items-center gap-2">
                <Clock className="w-3.5 h-3.5 text-fg-3" />
                <span className="text-xs font-medium text-fg">
                  {ts(indicator.timeframes, language)}
                </span>
              </div>
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-xs text-fg-3">
                  {ts(indicator.klineCount, language)}:
                </span>
                <Input
                  type="number"
                  value={config.klines.primary_count}
                  onChange={(e) =>
                    !disabled &&
                    onChange({
                      ...config,
                      klines: {
                        ...config.klines,
                        primary_count: parseInt(e.target.value) || 30,
                      },
                    })
                  }
                  disabled={disabled}
                  min={10}
                  max={30}
                  className="w-16 px-2 bg-surface-2 border border-line text-fg num text-right"
                />
                <span className="text-xs text-fg-3">Prompt根数:</span>
                <Input
                  type="number"
                  value={config.klines.prompt_kline_bars ?? 0}
                  onChange={(e) =>
                    !disabled &&
                    onChange({
                      ...config,
                      klines: {
                        ...config.klines,
                        prompt_kline_bars: parseInt(e.target.value) || 0,
                      },
                    })
                  }
                  disabled={disabled}
                  min={-1}
                  max={60}
                  title="喂给 AI 的每周期最近K线根数:0=默认20,负数=完整历史"
                  className="w-16 px-2 bg-surface-2 border border-line text-fg num text-right"
                />
              </div>
            </div>
            <p className="text-xs mb-2 text-fg-3">
              {ts(indicator.timeframesDesc, language)}
            </p>
            <p className="text-xs mb-2 text-fg-3">
              Prompt根数 = 每周期只把最近 N 根已闭合 K 线喂给 AI(0=默认
              20,负数=完整历史);指标仍按完整历史计算。
            </p>

            {/* Timeframe Grid */}
            <div className="space-y-1.5">
              {(['scalp', 'intraday', 'swing', 'position'] as const).map(
                (category) => {
                  const categoryTfs = allTimeframes.filter(
                    (tf) => tf.category === category
                  )
                  return (
                    <div key={category} className="flex items-center gap-2">
                      <span
                        className={cn(
                          'text-xs w-10 flex-shrink-0',
                          categoryColors[category]
                        )}
                      >
                        {ts(indicator[category], language)}
                      </span>
                      <div className="flex flex-wrap gap-1">
                        {categoryTfs.map((tf) => {
                          const isSelected = selectedTimeframes.includes(
                            tf.value
                          )
                          const isPrimary =
                            config.klines.primary_timeframe === tf.value
                          return (
                            <button
                              key={tf.value}
                              onClick={() => toggleTimeframe(tf.value)}
                              onDoubleClick={() =>
                                setPrimaryTimeframe(tf.value)
                              }
                              disabled={disabled}
                              aria-pressed={isSelected}
                              className={`num inline-flex h-7 items-center rounded-md border px-2 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50 ${isSelected ? 'border-brand bg-brand-soft text-brand' : 'border-line bg-surface-2 text-fg-3 hover:bg-surface-hover hover:text-fg'} ${isPrimary ? 'font-semibold ring-1 ring-brand' : ''}`}
                              title={
                                isPrimary ? `${tf.label} (Primary)` : tf.label
                              }
                            >
                              {tf.label}
                              {isPrimary && (
                                <span className="ml-0.5 text-[8px]">★</span>
                              )}
                            </button>
                          )
                        })}
                      </div>
                    </div>
                  )
                }
              )}
            </div>
          </div>
        </div>
      </div>

      {/* ============================================ */}
      {/* Section 2: Technical Indicators (Optional) */}
      {/* ============================================ */}
      <div className="rounded-lg overflow-hidden bg-surface-2 border border-line">
        <div className="px-3 py-2 flex items-center gap-2 bg-surface-2 border-b border-line">
          <Activity className="w-4 h-4 text-up" />
          <span className="text-sm font-medium text-fg">
            {ts(indicator.technicalIndicators, language)}
          </span>
          <span className="text-xs text-fg-3">
            - {ts(indicator.technicalIndicatorsDesc, language)}
          </span>
        </div>

        <div className="p-3">
          {/* Tip */}
          <div className="flex items-start gap-2 mb-3 p-2 rounded bg-up-soft">
            <Info className="w-3.5 h-3.5 mt-0.5 flex-shrink-0 text-up" />
            <p className="text-xs text-fg-3">
              {ts(indicator.aiCanCalculate, language)}
            </p>
          </div>

          {/* Indicator Grid */}
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
            {[
              {
                key: 'enable_ema',
                label: 'ema',
                desc: 'emaDesc',
                color: 'bg-brand',
                periodKey: 'ema_periods',
                defaultPeriods: '20,50',
              },
              {
                key: 'enable_macd',
                label: 'macd',
                desc: 'macdDesc',
                color: 'bg-ai',
              },
              {
                key: 'enable_rsi',
                label: 'rsi',
                desc: 'rsiDesc',
                color: 'bg-down',
                periodKey: 'rsi_periods',
                defaultPeriods: '7,14',
              },
              {
                key: 'enable_atr',
                label: 'atr',
                desc: 'atrDesc',
                color: 'bg-info',
                periodKey: 'atr_periods',
                defaultPeriods: '14',
              },
              {
                key: 'enable_boll',
                label: 'boll',
                desc: 'bollDesc',
                color: 'bg-ai',
                periodKey: 'boll_periods',
                defaultPeriods: '20',
              },
            ].map(({ key, label, desc, color, periodKey, defaultPeriods }) => (
              <div
                key={key}
                className={cn(
                  'p-3 rounded-md border transition-colors',
                  config[key as keyof IndicatorConfig]
                    ? 'bg-brand-soft border-brand'
                    : 'bg-surface-2 border-line'
                )}
              >
                <div className="flex items-center justify-between mb-1">
                  <div className="flex items-center gap-2">
                    <div className={cn('w-2 h-2 rounded-full', color)} />
                    <span className="text-xs font-medium text-fg">
                      {ts(indicator[label as keyof typeof indicator], language)}
                    </span>
                  </div>
                  <input
                    type="checkbox"
                    checked={
                      (config[key as keyof IndicatorConfig] as boolean) || false
                    }
                    onChange={(e) =>
                      !disabled &&
                      onChange({ ...config, [key]: e.target.checked })
                    }
                    disabled={disabled}
                    className="w-4 h-4 rounded accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                  />
                </div>
                <p className="text-xs mb-1.5 text-fg-3">
                  {ts(indicator[desc as keyof typeof indicator], language)}
                </p>
                {periodKey && config[key as keyof IndicatorConfig] && (
                  <Input
                    type="text"
                    value={
                      (
                        config[periodKey as keyof IndicatorConfig] as number[]
                      )?.join(',') || defaultPeriods
                    }
                    onChange={(e) => {
                      if (disabled) return
                      const periods = e.target.value
                        .split(',')
                        .map((s) => parseInt(s.trim()))
                        .filter((n) => !isNaN(n) && n > 0)
                      onChange({ ...config, [periodKey]: periods })
                    }}
                    disabled={disabled}
                    placeholder={defaultPeriods}
                    className="num w-full px-2 text-right bg-surface-2 border border-line text-fg"
                  />
                )}
              </div>
            ))}
          </div>
        </div>
      </div>

      {/* ============================================ */}
      {/* Section 3: Market Sentiment */}
      {/* ============================================ */}
      <div className="rounded-lg overflow-hidden bg-surface-2 border border-line">
        <div className="px-3 py-2 flex items-center gap-2 bg-surface-2 border-b border-line">
          <TrendingUp className="w-4 h-4 text-up" />
          <span className="text-sm font-medium text-fg">
            {ts(indicator.marketSentiment, language)}
          </span>
          <span className="text-xs text-fg-3">
            - {ts(indicator.marketSentimentDesc, language)}
          </span>
        </div>

        <div className="p-3">
          <div className="grid grid-cols-3 gap-2">
            {[
              {
                key: 'enable_volume',
                label: 'volume',
                desc: 'volumeDesc',
                color: 'bg-ai',
              },
              {
                key: 'enable_oi',
                label: 'oi',
                desc: 'oiDesc',
                color: 'bg-up',
              },
              {
                key: 'enable_funding_rate',
                label: 'fundingRate',
                desc: 'fundingRateDesc',
                color: 'bg-warn',
              },
            ].map(({ key, label, desc, color }) => (
              <div
                key={key}
                className={cn(
                  'p-3 rounded-md border transition-colors',
                  config[key as keyof IndicatorConfig]
                    ? 'bg-brand-soft border-brand'
                    : 'bg-surface-2 border-line'
                )}
              >
                <div className="flex items-center justify-between mb-1">
                  <div className="flex items-center gap-2">
                    <div className={cn('w-2 h-2 rounded-full', color)} />
                    <span className="text-xs font-medium text-fg">
                      {ts(indicator[label as keyof typeof indicator], language)}
                    </span>
                  </div>
                  <input
                    type="checkbox"
                    checked={
                      (config[key as keyof IndicatorConfig] as boolean) || false
                    }
                    onChange={(e) =>
                      !disabled &&
                      onChange({ ...config, [key]: e.target.checked })
                    }
                    disabled={disabled}
                    className="w-4 h-4 rounded accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                  />
                </div>
                <p className="text-xs text-fg-3">
                  {ts(indicator[desc as keyof typeof indicator], language)}
                </p>
              </div>
            ))}
          </div>
        </div>
      </div>
    </div>
  )
}
