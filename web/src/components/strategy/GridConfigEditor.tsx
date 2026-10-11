import { Grid, DollarSign, TrendingUp, Shield, Compass } from 'lucide-react'
import type { GridStrategyConfig } from '../../types'
import { gridConfig, ts } from '../../i18n/strategy-translations'
import { NofxSelect } from '../ui/select'

interface GridConfigEditorProps {
  config: GridStrategyConfig
  onChange: (config: GridStrategyConfig) => void
  disabled?: boolean
  language: string
}

// Default grid configuration
export const defaultGridConfig: GridStrategyConfig = {
  symbol: 'BTCUSDT',
  grid_count: 10,
  total_investment: 1000,
  leverage: 5,
  upper_price: 0,
  lower_price: 0,
  use_atr_bounds: true,
  atr_multiplier: 2.0,
  distribution: 'gaussian',
  max_drawdown_pct: 15,
  stop_loss_pct: 5,
  daily_loss_limit_pct: 10,
  use_maker_only: true,
  enable_direction_adjust: false,
  direction_bias_ratio: 0.7,
}

export function GridConfigEditor({
  config,
  onChange,
  disabled,
  language,
}: GridConfigEditorProps) {
  const updateField = <K extends keyof GridStrategyConfig>(
    key: K,
    value: GridStrategyConfig[K]
  ) => {
    if (!disabled) {
      onChange({ ...config, [key]: value })
    }
  }

  const inputStyle = {
    background: 'var(--surface-hover)',
    border: '1px solid var(--line)',
    color: 'var(--fg)',
  }

  const sectionStyle = {
    background: 'var(--surface-2)',
    border: '1px solid var(--line)',
  }

  return (
    <div className="space-y-6">
      {/* Trading Setup */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <DollarSign className="w-5 h-5" style={{ color: 'var(--brand)' }} />
          <h3 className="font-medium" style={{ color: 'var(--fg)' }}>
            {ts(gridConfig.tradingPair, language)}
          </h3>
        </div>

        <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
          {/* Symbol */}
          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(gridConfig.symbol, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(gridConfig.symbolDesc, language)}
            </p>
            <NofxSelect
              value={config.symbol}
              onChange={(val) => updateField('symbol', val)}
              disabled={disabled}
              className="w-full px-3 py-2 rounded"
              style={inputStyle}
              options={[
                { value: 'BTCUSDT', label: 'BTC/USDT' },
                { value: 'ETHUSDT', label: 'ETH/USDT' },
                { value: 'SOLUSDT', label: 'SOL/USDT' },
                { value: 'BNBUSDT', label: 'BNB/USDT' },
                { value: 'XRPUSDT', label: 'XRP/USDT' },
                { value: 'DOGEUSDT', label: 'DOGE/USDT' },
              ]}
            />
          </div>

          {/* Investment */}
          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(gridConfig.totalInvestment, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(gridConfig.totalInvestmentDesc, language)}
            </p>
            <input
              type="number"
              value={config.total_investment}
              onChange={(e) =>
                updateField(
                  'total_investment',
                  parseFloat(e.target.value) || 1000
                )
              }
              disabled={disabled}
              min={100}
              step={100}
              className="w-full px-3 py-2 rounded"
              style={inputStyle}
            />
          </div>

          {/* Leverage */}
          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(gridConfig.leverage, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(gridConfig.leverageDesc, language)}
            </p>
            <input
              type="number"
              value={config.leverage}
              onChange={(e) =>
                updateField('leverage', parseInt(e.target.value) || 5)
              }
              disabled={disabled}
              min={1}
              max={5}
              className="w-full px-3 py-2 rounded"
              style={inputStyle}
            />
          </div>
        </div>
      </div>

      {/* Grid Parameters */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <Grid className="w-5 h-5" style={{ color: 'var(--brand)' }} />
          <h3 className="font-medium" style={{ color: 'var(--fg)' }}>
            {ts(gridConfig.gridParameters, language)}
          </h3>
        </div>

        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {/* Grid Count */}
          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(gridConfig.gridCount, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(gridConfig.gridCountDesc, language)}
            </p>
            <input
              type="number"
              value={config.grid_count}
              onChange={(e) =>
                updateField('grid_count', parseInt(e.target.value) || 10)
              }
              disabled={disabled}
              min={5}
              max={50}
              className="w-full px-3 py-2 rounded"
              style={inputStyle}
            />
          </div>

          {/* Distribution */}
          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(gridConfig.distribution, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(gridConfig.distributionDesc, language)}
            </p>
            <NofxSelect
              value={config.distribution}
              onChange={(val) =>
                updateField(
                  'distribution',
                  val as 'uniform' | 'gaussian' | 'pyramid'
                )
              }
              disabled={disabled}
              className="w-full px-3 py-2 rounded"
              style={inputStyle}
              options={[
                { value: 'uniform', label: ts(gridConfig.uniform, language) },
                { value: 'gaussian', label: ts(gridConfig.gaussian, language) },
                { value: 'pyramid', label: ts(gridConfig.pyramid, language) },
              ]}
            />
          </div>
        </div>
      </div>

      {/* Price Bounds */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <TrendingUp className="w-5 h-5" style={{ color: 'var(--brand)' }} />
          <h3 className="font-medium" style={{ color: 'var(--fg)' }}>
            {ts(gridConfig.priceBounds, language)}
          </h3>
        </div>

        {/* ATR Toggle */}
        <div className="p-4 rounded-lg mb-4" style={sectionStyle}>
          <div className="flex items-center justify-between">
            <div>
              <label className="block text-sm" style={{ color: 'var(--fg)' }}>
                {ts(gridConfig.useAtrBounds, language)}
              </label>
              <p className="text-xs" style={{ color: 'var(--fg-3)' }}>
                {ts(gridConfig.useAtrBoundsDesc, language)}
              </p>
            </div>
            <label className="relative inline-flex items-center cursor-pointer">
              <input
                type="checkbox"
                checked={config.use_atr_bounds}
                onChange={(e) =>
                  updateField('use_atr_bounds', e.target.checked)
                }
                disabled={disabled}
                className="sr-only peer"
              />
              <div className="w-11 h-6 bg-line-strong peer-focus:outline-none rounded-full peer peer-checked:after:translate-x-full rtl:peer-checked:after:-translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:start-[2px] after:bg-white after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-brand"></div>
            </label>
          </div>
        </div>

        {config.use_atr_bounds ? (
          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(gridConfig.atrMultiplier, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(gridConfig.atrMultiplierDesc, language)}
            </p>
            <input
              type="number"
              value={config.atr_multiplier}
              onChange={(e) =>
                updateField('atr_multiplier', parseFloat(e.target.value) || 2.0)
              }
              disabled={disabled}
              min={1}
              max={5}
              step={0.5}
              className="w-32 px-3 py-2 rounded"
              style={inputStyle}
            />
          </div>
        ) : (
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            <div className="p-4 rounded-lg" style={sectionStyle}>
              <label
                className="block text-sm mb-1"
                style={{ color: 'var(--fg)' }}
              >
                {ts(gridConfig.upperPrice, language)}
              </label>
              <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
                {ts(gridConfig.upperPriceDesc, language)}
              </p>
              <input
                type="number"
                value={config.upper_price}
                onChange={(e) =>
                  updateField('upper_price', parseFloat(e.target.value) || 0)
                }
                disabled={disabled}
                min={0}
                step={0.01}
                className="w-full px-3 py-2 rounded"
                style={inputStyle}
              />
            </div>
            <div className="p-4 rounded-lg" style={sectionStyle}>
              <label
                className="block text-sm mb-1"
                style={{ color: 'var(--fg)' }}
              >
                {ts(gridConfig.lowerPrice, language)}
              </label>
              <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
                {ts(gridConfig.lowerPriceDesc, language)}
              </p>
              <input
                type="number"
                value={config.lower_price}
                onChange={(e) =>
                  updateField('lower_price', parseFloat(e.target.value) || 0)
                }
                disabled={disabled}
                min={0}
                step={0.01}
                className="w-full px-3 py-2 rounded"
                style={inputStyle}
              />
            </div>
          </div>
        )}
      </div>

      {/* Risk Control */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <Shield className="w-5 h-5" style={{ color: 'var(--brand)' }} />
          <h3 className="font-medium" style={{ color: 'var(--fg)' }}>
            {ts(gridConfig.riskControl, language)}
          </h3>
        </div>

        <div className="grid grid-cols-1 md:grid-cols-3 gap-4 mb-4">
          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(gridConfig.maxDrawdown, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(gridConfig.maxDrawdownDesc, language)}
            </p>
            <input
              type="number"
              value={config.max_drawdown_pct}
              onChange={(e) =>
                updateField(
                  'max_drawdown_pct',
                  parseFloat(e.target.value) || 15
                )
              }
              disabled={disabled}
              min={5}
              max={50}
              className="w-full px-3 py-2 rounded"
              style={inputStyle}
            />
          </div>

          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(gridConfig.stopLoss, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(gridConfig.stopLossDesc, language)}
            </p>
            <input
              type="number"
              value={config.stop_loss_pct}
              onChange={(e) =>
                updateField('stop_loss_pct', parseFloat(e.target.value) || 5)
              }
              disabled={disabled}
              min={1}
              max={20}
              className="w-full px-3 py-2 rounded"
              style={inputStyle}
            />
          </div>

          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(gridConfig.dailyLossLimit, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(gridConfig.dailyLossLimitDesc, language)}
            </p>
            <input
              type="number"
              value={config.daily_loss_limit_pct}
              onChange={(e) =>
                updateField(
                  'daily_loss_limit_pct',
                  parseFloat(e.target.value) || 10
                )
              }
              disabled={disabled}
              min={1}
              max={30}
              className="w-full px-3 py-2 rounded"
              style={inputStyle}
            />
          </div>
        </div>

        {/* Maker Only Toggle */}
        <div className="p-4 rounded-lg" style={sectionStyle}>
          <div className="flex items-center justify-between">
            <div>
              <label className="block text-sm" style={{ color: 'var(--fg)' }}>
                {ts(gridConfig.useMakerOnly, language)}
              </label>
              <p className="text-xs" style={{ color: 'var(--fg-3)' }}>
                {ts(gridConfig.useMakerOnlyDesc, language)}
              </p>
            </div>
            <label className="relative inline-flex items-center cursor-pointer">
              <input
                type="checkbox"
                checked={config.use_maker_only}
                onChange={(e) =>
                  updateField('use_maker_only', e.target.checked)
                }
                disabled={disabled}
                className="sr-only peer"
              />
              <div className="w-11 h-6 bg-line-strong peer-focus:outline-none rounded-full peer peer-checked:after:translate-x-full rtl:peer-checked:after:-translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:start-[2px] after:bg-white after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-brand"></div>
            </label>
          </div>
        </div>
      </div>

      {/* Direction Auto-Adjust */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <Compass className="w-5 h-5" style={{ color: 'var(--brand)' }} />
          <h3 className="font-medium" style={{ color: 'var(--fg)' }}>
            {ts(gridConfig.directionAdjust, language)}
          </h3>
        </div>

        {/* Enable Toggle */}
        <div className="p-4 rounded-lg mb-4" style={sectionStyle}>
          <div className="flex items-center justify-between">
            <div>
              <label className="block text-sm" style={{ color: 'var(--fg)' }}>
                {ts(gridConfig.enableDirectionAdjust, language)}
              </label>
              <p className="text-xs" style={{ color: 'var(--fg-3)' }}>
                {ts(gridConfig.enableDirectionAdjustDesc, language)}
              </p>
            </div>
            <label className="relative inline-flex items-center cursor-pointer">
              <input
                type="checkbox"
                checked={config.enable_direction_adjust ?? false}
                onChange={(e) =>
                  updateField('enable_direction_adjust', e.target.checked)
                }
                disabled={disabled}
                className="sr-only peer"
              />
              <div className="w-11 h-6 bg-line-strong peer-focus:outline-none rounded-full peer peer-checked:after:translate-x-full rtl:peer-checked:after:-translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:start-[2px] after:bg-white after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-brand"></div>
            </label>
          </div>
        </div>

        {config.enable_direction_adjust && (
          <>
            {/* Direction Modes Explanation */}
            <div
              className="p-4 rounded-lg mb-4"
              style={{
                background: 'var(--surface-hover)',
                border:
                  '1px solid color-mix(in srgb, var(--brand) 20%, transparent)',
              }}
            >
              <p
                className="text-xs font-medium mb-2"
                style={{ color: 'var(--brand)' }}
              >
                📊 {ts(gridConfig.directionModes, language)}
              </p>
              <div
                className="grid grid-cols-1 md:grid-cols-2 gap-2 text-xs"
                style={{ color: 'var(--fg-3)' }}
              >
                <div>• {ts(gridConfig.modeNeutral, language)}</div>
                <div>
                  •{' '}
                  <span style={{ color: 'var(--up)' }}>
                    {ts(gridConfig.modeLongBias, language)}
                  </span>
                </div>
                <div>
                  •{' '}
                  <span style={{ color: 'var(--up)' }}>
                    {ts(gridConfig.modeLong, language)}
                  </span>
                </div>
                <div>
                  •{' '}
                  <span style={{ color: 'var(--down)' }}>
                    {ts(gridConfig.modeShortBias, language)}
                  </span>
                </div>
                <div>
                  •{' '}
                  <span style={{ color: 'var(--down)' }}>
                    {ts(gridConfig.modeShort, language)}
                  </span>
                </div>
              </div>
              <p
                className="text-xs mt-3 pt-2 border-t border-line-strong"
                style={{ color: 'var(--fg-3)' }}
              >
                💡 {ts(gridConfig.directionExplain, language)}
              </p>
            </div>

            {/* Bias Strength */}
            <div className="p-4 rounded-lg" style={sectionStyle}>
              <label
                className="block text-sm mb-1"
                style={{ color: 'var(--fg)' }}
              >
                {ts(gridConfig.directionBiasRatio, language)} (X)
              </label>
              <p className="text-xs mb-1" style={{ color: 'var(--fg-3)' }}>
                {ts(gridConfig.directionBiasRatioDesc, language)}
              </p>
              <p className="text-xs mb-3" style={{ color: 'var(--brand)' }}>
                {ts(gridConfig.directionBiasExplain, language)}
              </p>
              <div className="flex items-center gap-3">
                <input
                  type="range"
                  value={(config.direction_bias_ratio ?? 0.7) * 100}
                  onChange={(e) =>
                    updateField(
                      'direction_bias_ratio',
                      parseInt(e.target.value) / 100
                    )
                  }
                  disabled={disabled}
                  min={55}
                  max={90}
                  step={5}
                  className="flex-1 h-2 rounded-lg appearance-none cursor-pointer"
                  style={{ background: 'var(--line)' }}
                />
                <span
                  className="text-sm font-mono w-20 text-right"
                  style={{ color: 'var(--brand)' }}
                >
                  X = {Math.round((config.direction_bias_ratio ?? 0.7) * 100)}%
                </span>
              </div>
              <div className="mt-2 grid grid-cols-2 gap-2 text-xs">
                <div
                  className="p-2 rounded"
                  style={{
                    background: 'var(--up-soft)',
                    border:
                      '1px solid color-mix(in srgb, var(--up) 19%, transparent)',
                  }}
                >
                  <span style={{ color: 'var(--up)' }}>Long Bias: </span>
                  <span style={{ color: 'var(--fg)' }}>
                    {Math.round((config.direction_bias_ratio ?? 0.7) * 100)}%{' '}
                    {ts(gridConfig.buy, language)} +{' '}
                    {Math.round(
                      (1 - (config.direction_bias_ratio ?? 0.7)) * 100
                    )}
                    % {ts(gridConfig.sell, language)}
                  </span>
                </div>
                <div
                  className="p-2 rounded"
                  style={{
                    background: 'var(--down-soft)',
                    border:
                      '1px solid color-mix(in srgb, var(--down) 19%, transparent)',
                  }}
                >
                  <span style={{ color: 'var(--down)' }}>Short Bias: </span>
                  <span style={{ color: 'var(--fg)' }}>
                    {Math.round(
                      (1 - (config.direction_bias_ratio ?? 0.7)) * 100
                    )}
                    % {ts(gridConfig.buy, language)} +{' '}
                    {Math.round((config.direction_bias_ratio ?? 0.7) * 100)}%{' '}
                    {ts(gridConfig.sell, language)}
                  </span>
                </div>
              </div>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
