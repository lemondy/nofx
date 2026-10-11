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
import { withAlpha } from '../../lib/colorAlpha'

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
          'fixed top-4 left-1/2 -translate-x-1/2 px-4 py-2 rounded-lg text-sm z-50 shadow-lg'
        toast.style.cssText = 'background:var(--down);color:var(--brand-fg);'
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
    scalp: 'var(--down)',
    intraday: 'var(--brand)',
    swing: 'var(--up)',
    position: 'var(--info)',
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
    <div className="space-y-5">
      {/* ============================================ */}
      {/* NofxOS Data Provider - Top Configuration */}
      {/* ============================================ */}
      <div
        className="rounded-lg overflow-hidden relative"
        style={{
          background:
            'linear-gradient(135deg, var(--ai-soft) 0%, var(--ai-soft) 50%, rgba(236, 72, 153, 0.08) 100%)',
          border: '1px solid color-mix(in srgb, var(--ai) 30%, transparent)',
        }}
      >
        {/* Decorative gradient line at top */}
        <div
          className="absolute top-0 left-0 right-0 h-[2px]"
          style={{
            background: 'linear-gradient(90deg, var(--ai), var(--ai), #ec4899)',
          }}
        />

        <div className="p-4">
          {/* Header Row */}
          <div className="flex items-center justify-between mb-3">
            <div className="flex items-center gap-2">
              <div
                className="w-8 h-8 rounded-lg flex items-center justify-center"
                style={{
                  background: 'linear-gradient(135deg, var(--ai), var(--ai))',
                }}
              >
                <Zap className="w-4 h-4 text-fg" />
              </div>
              <div>
                <h3
                  className="text-sm font-semibold"
                  style={{ color: 'var(--fg)' }}
                >
                  {ts(indicator.nofxosTitle, language)}
                </h3>
                <span className="text-[10px]" style={{ color: 'var(--fg-3)' }}>
                  {ts(indicator.nofxosFeatures, language)}
                </span>
              </div>
            </div>
          </div>

          {/* NofxOS Data Sources Grid */}
          <div className="mt-4">
            <div
              className="text-[10px] font-medium mb-2"
              style={{ color: 'var(--fg-3)' }}
            >
              {ts(indicator.nofxosDataSources, language)}
            </div>
            <div className="grid grid-cols-2 gap-2">
              {/* Quant Data */}
              <div
                className="p-2.5 rounded-lg transition-all cursor-pointer"
                style={{
                  background: config.enable_quant_data
                    ? 'var(--info-soft)'
                    : 'color-mix(in srgb, var(--surface) 50%, transparent)',
                  border: config.enable_quant_data
                    ? '1px solid color-mix(in srgb, var(--info) 30%, transparent)'
                    : '1px solid var(--line)',
                  opacity: disabled ? 0.5 : 1,
                }}
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
                    <div
                      className="w-2 h-2 rounded-full"
                      style={{ background: 'var(--info)' }}
                    />
                    <span
                      className="text-xs font-medium"
                      style={{ color: 'var(--fg)' }}
                    >
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
                    className="w-3.5 h-3.5 rounded accent-blue-500"
                  />
                </div>
                <p
                  className="text-[10px] mt-1"
                  style={{ color: 'var(--fg-3)' }}
                >
                  {ts(indicator.quantDataDesc, language)}
                </p>
                {config.enable_quant_data && (
                  <div className="flex gap-3 mt-2">
                    <label className="flex items-center gap-1.5 cursor-pointer">
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
                        className="w-3 h-3 rounded accent-blue-500"
                      />
                      <span
                        className="text-[10px]"
                        style={{ color: 'var(--fg)' }}
                      >
                        OI
                      </span>
                    </label>
                    <label className="flex items-center gap-1.5 cursor-pointer">
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
                        className="w-3 h-3 rounded accent-blue-500"
                      />
                      <span
                        className="text-[10px]"
                        style={{ color: 'var(--fg)' }}
                      >
                        Netflow
                      </span>
                    </label>
                  </div>
                )}
              </div>

              {/* OI Ranking */}
              <div
                className="p-2.5 rounded-lg transition-all cursor-pointer"
                style={{
                  background: config.enable_oi_ranking
                    ? 'var(--up-soft)'
                    : 'color-mix(in srgb, var(--surface) 50%, transparent)',
                  border: config.enable_oi_ranking
                    ? '1px solid color-mix(in srgb, var(--up) 30%, transparent)'
                    : '1px solid var(--line)',
                  opacity: disabled ? 0.5 : 1,
                }}
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
                    <div
                      className="w-2 h-2 rounded-full"
                      style={{ background: 'var(--up)' }}
                    />
                    <span
                      className="text-xs font-medium"
                      style={{ color: 'var(--fg)' }}
                    >
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
                    className="w-3.5 h-3.5 rounded accent-green-500"
                  />
                </div>
                <p
                  className="text-[10px] mt-1"
                  style={{ color: 'var(--fg-3)' }}
                >
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
                      className="flex-1 px-2 py-1 rounded text-[10px]"
                      style={{
                        background: 'var(--surface-hover)',
                        border: '1px solid var(--line)',
                        color: 'var(--fg)',
                      }}
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
                      className="w-14 px-2 py-1 rounded text-[10px]"
                      style={{
                        background: 'var(--surface-hover)',
                        border: '1px solid var(--line)',
                        color: 'var(--fg)',
                      }}
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
                className="p-2.5 rounded-lg transition-all cursor-pointer"
                style={{
                  background: config.enable_netflow_ranking
                    ? 'var(--warn-soft)'
                    : 'color-mix(in srgb, var(--surface) 50%, transparent)',
                  border: config.enable_netflow_ranking
                    ? '1px solid color-mix(in srgb, var(--warn) 30%, transparent)'
                    : '1px solid var(--line)',
                  opacity: disabled ? 0.5 : 1,
                }}
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
                    <div
                      className="w-2 h-2 rounded-full"
                      style={{ background: 'var(--warn)' }}
                    />
                    <span
                      className="text-xs font-medium"
                      style={{ color: 'var(--fg)' }}
                    >
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
                    className="w-3.5 h-3.5 rounded accent-amber-500"
                  />
                </div>
                <p
                  className="text-[10px] mt-1"
                  style={{ color: 'var(--fg-3)' }}
                >
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
                      className="flex-1 px-2 py-1 rounded text-[10px]"
                      style={{
                        background: 'var(--surface-hover)',
                        border: '1px solid var(--line)',
                        color: 'var(--fg)',
                      }}
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
                      className="w-14 px-2 py-1 rounded text-[10px]"
                      style={{
                        background: 'var(--surface-hover)',
                        border: '1px solid var(--line)',
                        color: 'var(--fg)',
                      }}
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
                className="p-2.5 rounded-lg transition-all cursor-pointer"
                style={{
                  background: config.enable_price_ranking
                    ? 'rgba(236, 72, 153, 0.1)'
                    : 'color-mix(in srgb, var(--surface) 50%, transparent)',
                  border: config.enable_price_ranking
                    ? '1px solid rgba(236, 72, 153, 0.3)'
                    : '1px solid var(--line)',
                  opacity: disabled ? 0.5 : 1,
                }}
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
                    <div
                      className="w-2 h-2 rounded-full"
                      style={{ background: '#ec4899' }}
                    />
                    <span
                      className="text-xs font-medium"
                      style={{ color: 'var(--fg)' }}
                    >
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
                    className="w-3.5 h-3.5 rounded accent-pink-500"
                  />
                </div>
                <p
                  className="text-[10px] mt-1"
                  style={{ color: 'var(--fg-3)' }}
                >
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
                      className="flex-1 px-2 py-1 rounded text-[10px]"
                      style={{
                        background: 'var(--surface-hover)',
                        border: '1px solid var(--line)',
                        color: 'var(--fg)',
                      }}
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
                      className="w-14 px-2 py-1 rounded text-[10px]"
                      style={{
                        background: 'var(--surface-hover)',
                        border: '1px solid var(--line)',
                        color: 'var(--fg)',
                      }}
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
      <div
        className="rounded-lg overflow-hidden"
        style={{
          background: 'var(--surface-2)',
          border: '1px solid var(--line)',
        }}
      >
        <div
          className="px-3 py-2 flex items-center gap-2"
          style={{
            background: 'var(--surface-hover)',
            borderBottom: '1px solid var(--line)',
          }}
        >
          <BarChart2 className="w-4 h-4" style={{ color: 'var(--brand)' }} />
          <span className="text-sm font-medium" style={{ color: 'var(--fg)' }}>
            {ts(indicator.marketData, language)}
          </span>
          <span className="text-xs" style={{ color: 'var(--fg-3)' }}>
            - {ts(indicator.marketDataDesc, language)}
          </span>
        </div>

        <div className="p-3 space-y-4">
          {/* Raw Klines - Required, Always On */}
          <div
            className="flex items-center justify-between p-3 rounded-lg"
            style={{
              background: 'var(--brand-soft)',
              border:
                '1px solid color-mix(in srgb, var(--brand) 20%, transparent)',
            }}
          >
            <div className="flex items-center gap-3">
              <div
                className="w-8 h-8 rounded-lg flex items-center justify-center"
                style={{ background: 'var(--brand-soft)' }}
              >
                <TrendingUp
                  className="w-4 h-4"
                  style={{ color: 'var(--brand)' }}
                />
              </div>
              <div>
                <div className="flex items-center gap-2">
                  <span
                    className="text-sm font-medium"
                    style={{ color: 'var(--fg)' }}
                  >
                    {ts(indicator.rawKlines, language)}
                  </span>
                  <span
                    className="px-1.5 py-0.5 rounded text-[10px] font-medium flex items-center gap-1"
                    style={{
                      background: 'var(--brand-soft)',
                      color: 'var(--brand)',
                    }}
                  >
                    <Lock className="w-2.5 h-2.5" />
                    {ts(indicator.required, language)}
                  </span>
                </div>
                <p className="text-xs mt-0.5" style={{ color: 'var(--fg-3)' }}>
                  {ts(indicator.rawKlinesDesc, language)}
                </p>
              </div>
            </div>
            <input
              type="checkbox"
              checked={true}
              disabled={true}
              className="w-5 h-5 rounded accent-yellow-500 cursor-not-allowed"
            />
          </div>

          {/* Timeframe Selection */}
          <div>
            <div className="flex items-center justify-between mb-2">
              <div className="flex items-center gap-2">
                <Clock
                  className="w-3.5 h-3.5"
                  style={{ color: 'var(--fg-3)' }}
                />
                <span
                  className="text-xs font-medium"
                  style={{ color: 'var(--fg)' }}
                >
                  {ts(indicator.timeframes, language)}
                </span>
              </div>
              <div className="flex items-center gap-2">
                <span className="text-[10px]" style={{ color: 'var(--fg-3)' }}>
                  {ts(indicator.klineCount, language)}:
                </span>
                <input
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
                  className="w-16 px-2 py-1 rounded text-xs text-center"
                  style={{
                    background: 'var(--surface-hover)',
                    border: '1px solid var(--line)',
                    color: 'var(--fg)',
                  }}
                />
                <span className="text-[10px]" style={{ color: 'var(--fg-3)' }}>
                  Prompt根数:
                </span>
                <input
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
                  className="w-16 px-2 py-1 rounded text-xs text-center"
                  style={{
                    background: 'var(--surface-hover)',
                    border: '1px solid var(--line)',
                    color: 'var(--fg)',
                  }}
                />
              </div>
            </div>
            <p className="text-[10px] mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(indicator.timeframesDesc, language)}
            </p>
            <p className="text-[10px] mb-2" style={{ color: 'var(--fg-3)' }}>
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
                        className="text-[10px] w-10 flex-shrink-0"
                        style={{ color: categoryColors[category] }}
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
                              className={`px-2 py-1 rounded text-xs font-medium transition-all ${
                                isSelected ? '' : 'opacity-40 hover:opacity-70'
                              }`}
                              style={{
                                background: isSelected
                                  ? `${withAlpha(categoryColors[category], 8)}`
                                  : 'transparent',
                                border: `1px solid ${isSelected ? categoryColors[category] : 'var(--line)'}`,
                                color: isSelected
                                  ? categoryColors[category]
                                  : 'var(--fg-3)',
                                boxShadow: isPrimary
                                  ? `0 0 0 2px ${categoryColors[category]}`
                                  : undefined,
                              }}
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
      <div
        className="rounded-lg overflow-hidden"
        style={{
          background: 'var(--surface-2)',
          border: '1px solid var(--line)',
        }}
      >
        <div
          className="px-3 py-2 flex items-center gap-2"
          style={{
            background: 'var(--surface-hover)',
            borderBottom: '1px solid var(--line)',
          }}
        >
          <Activity className="w-4 h-4" style={{ color: 'var(--up)' }} />
          <span className="text-sm font-medium" style={{ color: 'var(--fg)' }}>
            {ts(indicator.technicalIndicators, language)}
          </span>
          <span className="text-xs" style={{ color: 'var(--fg-3)' }}>
            - {ts(indicator.technicalIndicatorsDesc, language)}
          </span>
        </div>

        <div className="p-3">
          {/* Tip */}
          <div
            className="flex items-start gap-2 mb-3 p-2 rounded"
            style={{
              background: 'color-mix(in srgb, var(--up) 5%, transparent)',
            }}
          >
            <Info
              className="w-3.5 h-3.5 mt-0.5 flex-shrink-0"
              style={{ color: 'var(--up)' }}
            />
            <p className="text-[10px]" style={{ color: 'var(--fg-3)' }}>
              {ts(indicator.aiCanCalculate, language)}
            </p>
          </div>

          {/* Indicator Grid */}
          <div className="grid grid-cols-2 gap-2">
            {[
              {
                key: 'enable_ema',
                label: 'ema',
                desc: 'emaDesc',
                color: 'var(--brand)',
                periodKey: 'ema_periods',
                defaultPeriods: '20,50',
              },
              {
                key: 'enable_macd',
                label: 'macd',
                desc: 'macdDesc',
                color: 'var(--ai)',
              },
              {
                key: 'enable_rsi',
                label: 'rsi',
                desc: 'rsiDesc',
                color: 'var(--down)',
                periodKey: 'rsi_periods',
                defaultPeriods: '7,14',
              },
              {
                key: 'enable_atr',
                label: 'atr',
                desc: 'atrDesc',
                color: 'var(--info)',
                periodKey: 'atr_periods',
                defaultPeriods: '14',
              },
              {
                key: 'enable_boll',
                label: 'boll',
                desc: 'bollDesc',
                color: '#ec4899',
                periodKey: 'boll_periods',
                defaultPeriods: '20',
              },
            ].map(({ key, label, desc, color, periodKey, defaultPeriods }) => (
              <div
                key={key}
                className="p-2.5 rounded-lg transition-all"
                style={{
                  background: config[key as keyof IndicatorConfig]
                    ? `${withAlpha(color, 3)}`
                    : 'transparent',
                  border: `1px solid ${config[key as keyof IndicatorConfig] ? `${withAlpha(color, 19)}` : 'var(--line)'}`,
                }}
              >
                <div className="flex items-center justify-between mb-1">
                  <div className="flex items-center gap-2">
                    <div
                      className="w-2 h-2 rounded-full"
                      style={{ background: color }}
                    />
                    <span
                      className="text-xs font-medium"
                      style={{ color: 'var(--fg)' }}
                    >
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
                    className="w-4 h-4 rounded accent-yellow-500"
                  />
                </div>
                <p
                  className="text-[10px] mb-1.5"
                  style={{ color: 'var(--fg-3)' }}
                >
                  {ts(indicator[desc as keyof typeof indicator], language)}
                </p>
                {periodKey && config[key as keyof IndicatorConfig] && (
                  <input
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
                    className="w-full px-2 py-1 rounded text-[10px] text-center"
                    style={{
                      background: 'var(--surface-hover)',
                      border: '1px solid var(--line)',
                      color: 'var(--fg)',
                    }}
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
      <div
        className="rounded-lg overflow-hidden"
        style={{
          background: 'var(--surface-2)',
          border: '1px solid var(--line)',
        }}
      >
        <div
          className="px-3 py-2 flex items-center gap-2"
          style={{
            background: 'var(--surface-hover)',
            borderBottom: '1px solid var(--line)',
          }}
        >
          <TrendingUp className="w-4 h-4" style={{ color: 'var(--up)' }} />
          <span className="text-sm font-medium" style={{ color: 'var(--fg)' }}>
            {ts(indicator.marketSentiment, language)}
          </span>
          <span className="text-xs" style={{ color: 'var(--fg-3)' }}>
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
                color: 'var(--ai)',
              },
              {
                key: 'enable_oi',
                label: 'oi',
                desc: 'oiDesc',
                color: 'var(--up)',
              },
              {
                key: 'enable_funding_rate',
                label: 'fundingRate',
                desc: 'fundingRateDesc',
                color: 'var(--warn)',
              },
            ].map(({ key, label, desc, color }) => (
              <div
                key={key}
                className="p-2.5 rounded-lg transition-all"
                style={{
                  background: config[key as keyof IndicatorConfig]
                    ? `${withAlpha(color, 3)}`
                    : 'transparent',
                  border: `1px solid ${config[key as keyof IndicatorConfig] ? `${withAlpha(color, 19)}` : 'var(--line)'}`,
                }}
              >
                <div className="flex items-center justify-between mb-1">
                  <div className="flex items-center gap-2">
                    <div
                      className="w-2 h-2 rounded-full"
                      style={{ background: color }}
                    />
                    <span
                      className="text-xs font-medium"
                      style={{ color: 'var(--fg)' }}
                    >
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
                    className="w-4 h-4 rounded accent-yellow-500"
                  />
                </div>
                <p className="text-[10px]" style={{ color: 'var(--fg-3)' }}>
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
