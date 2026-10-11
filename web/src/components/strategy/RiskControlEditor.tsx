import { Shield, AlertTriangle } from 'lucide-react'
import type { RiskControlConfig } from '../../types'
import { riskControl, ts } from '../../i18n/strategy-translations'

interface RiskControlEditorProps {
  config: RiskControlConfig
  onChange: (config: RiskControlConfig) => void
  disabled?: boolean
  language: string
}

export function RiskControlEditor({
  config,
  onChange,
  disabled,
  language,
}: RiskControlEditorProps) {
  const updateField = <K extends keyof RiskControlConfig>(
    key: K,
    value: RiskControlConfig[K]
  ) => {
    if (!disabled) {
      onChange({ ...config, [key]: value })
    }
  }

  return (
    <div className="space-y-6">
      {/* Position Limits */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <Shield className="w-5 h-5" style={{ color: 'var(--brand)' }} />
          <h3 className="font-medium" style={{ color: 'var(--fg)' }}>
            {ts(riskControl.positionLimits, language)}
          </h3>
        </div>

        <div className="grid grid-cols-1 gap-4 mb-4">
          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.maxPositions, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.maxPositionsDesc, language)}
            </p>
            <input
              type="number"
              value={config.max_positions ?? 3}
              onChange={(e) =>
                updateField('max_positions', parseInt(e.target.value) || 3)
              }
              disabled={disabled}
              min={1}
              max={10}
              className="w-32 px-3 py-2 rounded"
              style={{
                background: 'var(--surface-hover)',
                border: '1px solid var(--line)',
                color: 'var(--fg)',
              }}
            />
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.earlyCloseMinHours, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.earlyCloseMinHoursDesc, language)}
            </p>
            <input
              type="number"
              value={config.early_close_min_hours ?? 0}
              onChange={(e) =>
                updateField(
                  'early_close_min_hours',
                  e.target.value === '' ? 0 : parseInt(e.target.value)
                )
              }
              disabled={disabled}
              min={-1}
              max={72}
              className="w-32 px-3 py-2 rounded"
              style={{
                background: 'var(--surface-hover)',
                border: '1px solid var(--line)',
                color: 'var(--fg)',
              }}
            />
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {`当前生效: ${(config.early_close_min_hours ?? 0) === 0 ? 4 : (config.early_close_min_hours ?? 0) <= -1 ? '禁用' : config.early_close_min_hours}h`}
            </p>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.pumpGuard4hPct, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.pumpGuard4hPctDesc, language)}
            </p>
            <input
              type="number"
              value={config.pump_guard_4h_pct ?? 0}
              onChange={(e) =>
                updateField(
                  'pump_guard_4h_pct',
                  e.target.value === '' ? 0 : parseFloat(e.target.value)
                )
              }
              disabled={disabled}
              min={-1}
              max={200}
              className="w-32 px-3 py-2 rounded"
              style={{
                background: 'var(--surface-hover)',
                border: '1px solid var(--line)',
                color: 'var(--fg)',
              }}
            />
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {`当前生效: ${(config.pump_guard_4h_pct ?? 0) === 0 ? 20 : (config.pump_guard_4h_pct ?? 0) < 0 ? '禁用' : config.pump_guard_4h_pct}%`}
            </p>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.tpTrimProfitPct, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.tpTrimProfitPctDesc, language)}
            </p>
            <input
              type="number"
              value={config.tp_trim_profit_pct ?? 0}
              onChange={(e) =>
                updateField(
                  'tp_trim_profit_pct',
                  e.target.value === '' ? 0 : parseFloat(e.target.value)
                )
              }
              disabled={disabled}
              className="w-32 px-3 py-2 rounded"
              style={{
                background: 'var(--surface-hover)',
                border: '1px solid var(--line)',
                color: 'var(--fg)',
              }}
            />
            {(() => {
              const lockR = config.profit_lock_at_r ?? 0
              if (lockR >= 0) {
                return (
                  <p
                    className="text-xs mt-2 font-medium"
                    style={{ color: 'var(--brand)' }}
                  >
                    {ts(riskControl.profitLockTrimHint, language)}
                  </p>
                )
              }
              return null
            })()}
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.tpFullProfitPct, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.tpFullProfitPctDesc, language)}
            </p>
            <input
              type="number"
              value={config.tp_full_profit_pct ?? 0}
              onChange={(e) =>
                updateField(
                  'tp_full_profit_pct',
                  e.target.value === '' ? 0 : parseFloat(e.target.value)
                )
              }
              disabled={disabled}
              className="w-32 px-3 py-2 rounded"
              style={{
                background: 'var(--surface-hover)',
                border: '1px solid var(--line)',
                color: 'var(--fg)',
              }}
            />
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {'止盈阶梯 R 档(R 单位,优先于上面的 ROE 档)'}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {
                'ROE 档随杠杆漂移(15% ROE ≈ 0.5R@10x / 1.67R@3x),R 档与杠杆无关。>0 = 该 R 值生效并覆盖 ROE 档;负数 = 该档关闭;0 = 用上面的 ROE 兼容档'
              }
            </p>
            <div className="flex gap-4">
              <div>
                <label
                  className="block text-xs mb-1"
                  style={{ color: 'var(--fg-3)' }}
                >
                  {'减仓 1/3 触发 (R)'}
                </label>
                <input
                  type="number"
                  step={0.1}
                  value={config.tp_trim_at_r ?? 0}
                  onChange={(e) =>
                    updateField(
                      'tp_trim_at_r',
                      e.target.value === '' ? 0 : parseFloat(e.target.value)
                    )
                  }
                  disabled={disabled}
                  className="w-32 px-3 py-2 rounded"
                  style={{
                    background: 'var(--surface-hover)',
                    border: '1px solid var(--line)',
                    color: 'var(--fg)',
                  }}
                />
              </div>
              <div>
                <label
                  className="block text-xs mb-1"
                  style={{ color: 'var(--fg-3)' }}
                >
                  {'全部平仓触发 (R;负数=关闭)'}
                </label>
                <input
                  type="number"
                  step={0.1}
                  value={config.tp_full_at_r ?? 0}
                  onChange={(e) =>
                    updateField(
                      'tp_full_at_r',
                      e.target.value === '' ? 0 : parseFloat(e.target.value)
                    )
                  }
                  disabled={disabled}
                  className="w-32 px-3 py-2 rounded"
                  style={{
                    background: 'var(--surface-hover)',
                    border: '1px solid var(--line)',
                    color: 'var(--fg)',
                  }}
                />
              </div>
            </div>
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {(() => {
                const trimR = config.tp_trim_at_r ?? 0
                const fullR = config.tp_full_at_r ?? 0
                const trimTxt =
                  trimR > 0
                    ? `${trimR}R 减仓 1/3`
                    : trimR < 0
                      ? '减仓档关'
                      : '减仓走 ROE 档'
                const fullTxt =
                  fullR > 0
                    ? `${fullR}R 全平`
                    : fullR < 0
                      ? '全平档关(归结构位TP+移动止损+回撤保护)'
                      : '全平走 ROE 档'
                return `当前生效: ${trimTxt};${fullTxt}`
              })()}
            </p>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.profitLockAtR, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.profitLockAtRDesc, language)}
            </p>
            <input
              type="number"
              step={0.1}
              value={config.profit_lock_at_r ?? 0}
              onChange={(e) =>
                updateField(
                  'profit_lock_at_r',
                  e.target.value === '' ? 0 : parseFloat(e.target.value)
                )
              }
              disabled={disabled}
              className="w-32 px-3 py-2 rounded"
              style={{
                background: 'var(--surface-hover)',
                border: '1px solid var(--line)',
                color: 'var(--fg)',
              }}
            />
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {`当前生效: ${(config.profit_lock_at_r ?? 0) < 0 ? '禁用(ROE 减仓档接管)' : `${(config.profit_lock_at_r ?? 0) === 0 ? 1 : config.profit_lock_at_r}R 时保本+减半 50%`}`}
            </p>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.profitLockBEOffsetR, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.profitLockBEOffsetRDesc, language)}
            </p>
            <input
              type="number"
              step={0.05}
              value={config.profit_lock_be_offset_r ?? 0}
              onChange={(e) =>
                updateField(
                  'profit_lock_be_offset_r',
                  e.target.value === '' ? 0 : parseFloat(e.target.value)
                )
              }
              disabled={disabled}
              className="w-32 px-3 py-2 rounded"
              style={{
                background: 'var(--surface-hover)',
                border: '1px solid var(--line)',
                color: 'var(--fg)',
              }}
            />
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {`当前生效: ${(config.profit_lock_be_offset_r ?? 0) < 0 ? '纯保本(开仓价)' : `开仓价+${(config.profit_lock_be_offset_r ?? 0) === 0 ? 0.2 : config.profit_lock_be_offset_r}R`}
 `}
            </p>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.tpCloseFraction, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.tpCloseFractionDesc, language)}
            </p>
            <input
              type="number"
              step={0.05}
              min={-1}
              max={1}
              value={config.tp_close_fraction ?? 0}
              onChange={(e) =>
                updateField(
                  'tp_close_fraction',
                  e.target.value === '' ? 0 : parseFloat(e.target.value)
                )
              }
              disabled={disabled}
              className="w-32 px-3 py-2 rounded"
              style={{
                background: 'var(--surface-hover)',
                border: '1px solid var(--line)',
                color: 'var(--fg)',
              }}
            />
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {(() => {
                const raw = config.tp_close_fraction ?? 0
                // Backend bool: absent/false = trailing OFF → full close (no runner).
                const trailingOn = config.trailing_stop_enabled === true
                if (!trailingOn)
                  return '当前生效: 全平(移动止损关闭,趋势跑单不可用)'
                const eff = raw < 0 ? 1 : raw === 0 ? 0.5 : raw
                return `当前生效: 止盈触发平 ${(eff * 100).toFixed(0)}%,剩余 ${100 - eff * 100}% 由移动止损接管`
              })()}
            </p>
          </div>
        </div>

        {/* Trading Leverage (Exchange) */}
        <div className="mb-2">
          <p
            className="text-xs font-medium mb-2"
            style={{ color: 'var(--brand)' }}
          >
            {ts(riskControl.tradingLeverage, language)}
          </p>
        </div>
        <div className="grid grid-cols-2 gap-4 mb-4">
          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.btcEthLeverage, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.btcEthLeverageDesc, language)}
            </p>
            <div className="flex items-center gap-2">
              <input
                type="range"
                value={config.btc_eth_max_leverage ?? 5}
                onChange={(e) =>
                  updateField('btc_eth_max_leverage', parseInt(e.target.value))
                }
                disabled={disabled}
                min={1}
                max={20}
                className="flex-1 accent-yellow-500"
              />
              <span
                className="w-12 text-center font-mono"
                style={{ color: 'var(--brand)' }}
              >
                {config.btc_eth_max_leverage ?? 5}x
              </span>
            </div>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.altcoinLeverage, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.altcoinLeverageDesc, language)}
            </p>
            <div className="flex items-center gap-2">
              <input
                type="range"
                value={config.altcoin_max_leverage ?? 5}
                onChange={(e) =>
                  updateField('altcoin_max_leverage', parseInt(e.target.value))
                }
                disabled={disabled}
                min={1}
                max={20}
                className="flex-1 accent-yellow-500"
              />
              <span
                className="w-12 text-center font-mono"
                style={{ color: 'var(--brand)' }}
              >
                {config.altcoin_max_leverage ?? 5}x
              </span>
            </div>
          </div>
        </div>

        {/* Position Value Ratio (Risk Control - CODE ENFORCED) */}
        <div className="mb-2">
          <p className="text-xs font-medium" style={{ color: 'var(--up)' }}>
            {ts(riskControl.positionValueRatio, language)}
          </p>
          <p className="text-xs mt-1" style={{ color: 'var(--fg-3)' }}>
            {ts(riskControl.positionValueRatioDesc, language)}
          </p>
        </div>
        <div className="grid grid-cols-2 gap-4">
          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--up)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.btcEthPositionValueRatio, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.btcEthPositionValueRatioDesc, language)}
            </p>
            <div className="flex items-center gap-2">
              <input
                type="range"
                value={config.btc_eth_max_position_value_ratio ?? 5}
                onChange={(e) =>
                  updateField(
                    'btc_eth_max_position_value_ratio',
                    parseFloat(e.target.value)
                  )
                }
                disabled={disabled}
                min={0.5}
                max={10}
                step={0.5}
                className="flex-1 accent-green-500"
              />
              <span
                className="w-12 text-center font-mono"
                style={{ color: 'var(--up)' }}
              >
                {config.btc_eth_max_position_value_ratio ?? 5}x
              </span>
            </div>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--up)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.altcoinPositionValueRatio, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.altcoinPositionValueRatioDesc, language)}
            </p>
            <div className="flex items-center gap-2">
              <input
                type="range"
                value={config.altcoin_max_position_value_ratio ?? 1}
                onChange={(e) =>
                  updateField(
                    'altcoin_max_position_value_ratio',
                    parseFloat(e.target.value)
                  )
                }
                disabled={disabled}
                min={0.5}
                max={10}
                step={0.5}
                className="flex-1 accent-green-500"
              />
              <span
                className="w-12 text-center font-mono"
                style={{ color: 'var(--up)' }}
              >
                {config.altcoin_max_position_value_ratio ?? 1}x
              </span>
            </div>
          </div>
        </div>
      </div>

      {/* Risk Parameters */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <AlertTriangle className="w-5 h-5" style={{ color: 'var(--down)' }} />
          <h3 className="font-medium" style={{ color: 'var(--fg)' }}>
            {ts(riskControl.riskParameters, language)}
          </h3>
        </div>

        <div className="grid grid-cols-2 gap-4">
          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.minRiskReward, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.minRiskRewardDesc, language)}
            </p>
            <div className="flex items-center">
              <span style={{ color: 'var(--fg-3)' }}>1:</span>
              <input
                type="number"
                value={config.min_risk_reward_ratio ?? 3}
                onChange={(e) => {
                  // 0 is a legal value (disables the RR gate) — a bare
                  // `|| 3` made it unreachable from the UI.
                  const v = parseFloat(e.target.value)
                  updateField(
                    'min_risk_reward_ratio',
                    Number.isFinite(v) ? v : 3
                  )
                }}
                disabled={disabled}
                min={0}
                max={10}
                step={0.5}
                className="w-20 px-3 py-2 rounded ml-2"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
            </div>
            {(config.min_risk_reward_ratio ?? 3) === 0 && (
              <p
                className="text-xs mt-2 font-medium"
                style={{ color: 'var(--brand)' }}
              >
                {ts(riskControl.minRRZeroHint, language)}
              </p>
            )}
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--up)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.maxMarginUsage, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.maxMarginUsageDesc, language)}
            </p>
            <div className="flex items-center gap-2">
              <input
                type="range"
                value={(config.max_margin_usage ?? 0.9) * 100}
                onChange={(e) =>
                  updateField(
                    'max_margin_usage',
                    parseInt(e.target.value) / 100
                  )
                }
                disabled={disabled}
                min={10}
                max={100}
                className="flex-1 accent-green-500"
              />
              <span
                className="w-12 text-center font-mono"
                style={{ color: 'var(--up)' }}
              >
                {Math.round((config.max_margin_usage ?? 0.9) * 100)}%
              </span>
            </div>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--up)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.riskPerTradePct, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.riskPerTradePctDesc, language)}
            </p>
            <div className="flex items-center">
              <input
                type="number"
                value={config.risk_per_trade_pct ?? 0}
                onChange={(e) => {
                  const v = parseFloat(e.target.value)
                  updateField(
                    'risk_per_trade_pct',
                    e.target.value === '' || isNaN(v) ? 0 : v
                  )
                }}
                disabled={disabled}
                min={0}
                max={10}
                step={0.1}
                className="w-24 px-3 py-2 rounded"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
              <span className="ml-2" style={{ color: 'var(--fg-3)' }}>
                %
              </span>
            </div>
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {`当前生效: ${(config.risk_per_trade_pct ?? 0) <= 0 ? '默认 1.5' : config.risk_per_trade_pct}%`}
            </p>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--up)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.maxAccountRiskPct, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.maxAccountRiskPctDesc, language)}
            </p>
            <div className="flex items-center">
              <input
                type="number"
                value={config.max_account_risk_pct ?? 0}
                onChange={(e) => {
                  const v = parseFloat(e.target.value)
                  updateField(
                    'max_account_risk_pct',
                    e.target.value === '' || isNaN(v) ? 0 : v
                  )
                }}
                disabled={disabled}
                min={-1}
                max={100}
                step={0.5}
                className="w-24 px-3 py-2 rounded"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
              <span className="ml-2" style={{ color: 'var(--fg-3)' }}>
                %
              </span>
            </div>
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {`当前生效: ${(config.max_account_risk_pct ?? 0) < 0 ? '禁用' : (config.max_account_risk_pct ?? 0) === 0 ? '默认 10' : config.max_account_risk_pct}%`}
            </p>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--up)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.maxNetDirectionalRiskPct, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.maxNetDirectionalRiskPctDesc, language)}
            </p>
            <div className="flex items-center">
              <input
                type="number"
                value={config.max_net_directional_risk_pct ?? 0}
                onChange={(e) => {
                  const v = parseFloat(e.target.value)
                  updateField(
                    'max_net_directional_risk_pct',
                    e.target.value === '' || isNaN(v) ? 0 : v
                  )
                }}
                disabled={disabled}
                min={-1}
                max={100}
                step={0.5}
                className="w-24 px-3 py-2 rounded"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
              <span className="ml-2" style={{ color: 'var(--fg-3)' }}>
                %
              </span>
            </div>
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {`当前生效: ${(config.max_net_directional_risk_pct ?? 0) < 0 ? '禁用' : (config.max_net_directional_risk_pct ?? 0) === 0 ? '默认 6' : config.max_net_directional_risk_pct}%`}
            </p>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--up)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.vendorDivergence, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.vendorDivergenceDesc, language)}
            </p>
            <div className="flex items-center">
              <input
                type="number"
                value={config.max_vendor_divergence_pct ?? 0}
                onChange={(e) => {
                  const v = parseFloat(e.target.value)
                  updateField(
                    'max_vendor_divergence_pct',
                    e.target.value === '' || isNaN(v) ? 0 : v
                  )
                }}
                disabled={disabled}
                min={-5}
                max={10}
                step={0.5}
                className="w-24 px-3 py-2 rounded"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
              <span className="ml-2" style={{ color: 'var(--fg-3)' }}>
                %
              </span>
            </div>
            <p
              className="text-xs mt-2 font-medium"
              style={{ color: 'var(--up)' }}
            >
              {`当前生效: ${(config.max_vendor_divergence_pct ?? 0) < 0 ? '禁用' : (config.max_vendor_divergence_pct ?? 0) === 0 ? '默认 1' : config.max_vendor_divergence_pct}%`}
            </p>
          </div>
        </div>
      </div>

      {/* Entry Requirements */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <Shield className="w-5 h-5" style={{ color: 'var(--up)' }} />
          <h3 className="font-medium" style={{ color: 'var(--fg)' }}>
            {ts(riskControl.entryRequirements, language)}
          </h3>
        </div>

        <div className="grid grid-cols-2 gap-4">
          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.minPositionSize, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.minPositionSizeDesc, language)}
            </p>
            <div className="flex items-center">
              <input
                type="number"
                value={config.min_position_size ?? 12}
                onChange={(e) =>
                  updateField(
                    'min_position_size',
                    parseFloat(e.target.value) || 12
                  )
                }
                disabled={disabled}
                min={10}
                max={1000}
                className="w-24 px-3 py-2 rounded"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
              <span className="ml-2" style={{ color: 'var(--fg-3)' }}>
                USDT
              </span>
            </div>
          </div>

          <div
            className="p-4 rounded-lg"
            style={{
              background: 'var(--surface-2)',
              border: '1px solid var(--line)',
            }}
          >
            <label
              className="block text-sm mb-1"
              style={{ color: 'var(--fg)' }}
            >
              {ts(riskControl.minConfidence, language)}
            </label>
            <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
              {ts(riskControl.minConfidenceDesc, language)}
            </p>
            <div className="flex items-center gap-2">
              <input
                type="range"
                value={config.min_confidence ?? 75}
                onChange={(e) =>
                  updateField('min_confidence', parseInt(e.target.value))
                }
                disabled={disabled}
                min={50}
                max={100}
                className="flex-1 accent-green-500"
              />
              <span
                className="w-12 text-center font-mono"
                style={{ color: 'var(--up)' }}
              >
                {config.min_confidence ?? 75}
              </span>
            </div>
          </div>
        </div>

        <div
          className="p-4 rounded-lg mt-4"
          style={{
            background: 'var(--surface-2)',
            border: '1px solid var(--line)',
          }}
        >
          <label className="block text-sm mb-1" style={{ color: 'var(--fg)' }}>
            限价挂单偏移 limit_entry_offset_pct
          </label>
          <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
            默认限价开仓:做多挂现价下方、做空挂现价上方该百分比(0.1–5%,默认0.5)。偏移越大成交越慢、价位越优
          </p>
          <div className="flex items-center gap-2">
            <input
              type="range"
              value={config.limit_entry_offset_pct ?? 0.5}
              onChange={(e) =>
                updateField(
                  'limit_entry_offset_pct',
                  parseFloat(e.target.value)
                )
              }
              disabled={disabled}
              min={0.1}
              max={5}
              step={0.1}
              className="flex-1 accent-green-500"
            />
            <span
              className="w-14 text-center font-mono"
              style={{ color: 'var(--up)' }}
            >
              {(config.limit_entry_offset_pct ?? 0.5).toFixed(1)}%
            </span>
          </div>
        </div>

        <div
          className="p-4 rounded-lg mt-4"
          style={{
            background: 'var(--surface-2)',
            border: '1px solid var(--line)',
          }}
        >
          <label
            className="flex items-center justify-between block text-sm mb-1 cursor-pointer"
            style={{ color: 'var(--fg)' }}
          >
            <span>锚点穿越转市价 limit_entry_market_fallback</span>
            <input
              type="checkbox"
              checked={config.limit_entry_market_fallback !== false}
              onChange={(e) =>
                updateField('limit_entry_market_fallback', e.target.checked)
              }
              disabled={disabled}
              className="w-3.5 h-3.5 rounded accent-green-500"
            />
          </label>
          <p className="text-xs" style={{ color: 'var(--fg-3)' }}>
            AI
            推理期间行情穿过限价锚点(多单锚点高于现价/空单低于现价)时,视为回踩/反弹已到位,自动转为市价单入场(全部风险闸门按现价复检);价格已越过止损则拒单。关闭后维持旧行为:穿越即拒单
          </p>
        </div>

        <div
          className="p-4 rounded-lg mt-4"
          style={{
            background: 'var(--surface-2)',
            border: '1px solid var(--line)',
          }}
        >
          <label className="block text-sm mb-1" style={{ color: 'var(--fg)' }}>
            回撤保护平仓 peak_drawdown
          </label>
          <p className="text-xs mb-2" style={{ color: 'var(--fg-3)' }}>
            浮盈峰值达到「起征浮盈」后,从峰值回撤超过「最大回撤」即程序自动平仓锁定利润
          </p>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label
                className="block text-xs mb-1"
                style={{ color: 'var(--fg-3)' }}
              >
                起征浮盈 %
              </label>
              <input
                type="number"
                value={config.peak_drawdown_min_profit_pct ?? 5}
                onChange={(e) =>
                  updateField(
                    'peak_drawdown_min_profit_pct',
                    parseFloat(e.target.value) || 5
                  )
                }
                disabled={disabled}
                min={1}
                max={50}
                step={0.5}
                className="w-24 px-3 py-2 rounded"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
            </div>
            <div>
              <label
                className="block text-xs mb-1"
                style={{ color: 'var(--fg-3)' }}
              >
                最大回撤 %
              </label>
              <input
                type="number"
                value={config.peak_drawdown_max_dd_pct ?? 55}
                onChange={(e) =>
                  updateField(
                    'peak_drawdown_max_dd_pct',
                    parseFloat(e.target.value) || 55
                  )
                }
                disabled={disabled}
                min={10}
                max={95}
                step={1}
                className="w-24 px-3 py-2 rounded"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
            </div>
          </div>
          <div className="grid grid-cols-2 gap-3 mt-3">
            <div>
              <label
                className="block text-xs mb-1"
                style={{ color: 'var(--fg-3)' }}
              >
                R 档起征 (×初始止损距离;&gt;0 启用,替代上面的 ROE 档)
              </label>
              <input
                type="number"
                step={0.1}
                value={config.peak_drawdown_arm_r ?? 0}
                onChange={(e) =>
                  updateField(
                    'peak_drawdown_arm_r',
                    e.target.value === '' ? 0 : parseFloat(e.target.value)
                  )
                }
                disabled={disabled}
                className="w-24 px-3 py-2 rounded"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
            </div>
            <div>
              <label
                className="block text-xs mb-1"
                style={{ color: 'var(--fg-3)' }}
              >
                R 档回吐比例 (0-1,0=默认 0.5)
              </label>
              <input
                type="number"
                step={0.05}
                value={config.peak_drawdown_giveback_r ?? 0}
                onChange={(e) =>
                  updateField(
                    'peak_drawdown_giveback_r',
                    e.target.value === '' ? 0 : parseFloat(e.target.value)
                  )
                }
                disabled={disabled}
                className="w-24 px-3 py-2 rounded"
                style={{
                  background: 'var(--surface-hover)',
                  border: '1px solid var(--line)',
                  color: 'var(--fg)',
                }}
              />
            </div>
          </div>
          <p
            className="text-xs mt-2 font-medium"
            style={{ color: 'var(--up)' }}
          >
            {(() => {
              const armR = config.peak_drawdown_arm_r ?? 0
              if (armR > 0) {
                const gb =
                  (config.peak_drawdown_giveback_r ?? 0) > 0
                    ? (config.peak_drawdown_giveback_r ?? 0)
                    : 0.5
                return `当前生效: R 档 — 峰值 ≥ ${armR}R 后回吐 ≥ ${gb * 100}% 平仓(与杠杆无关)`
              }
              return '当前生效: ROE 档(随杠杆漂移)'
            })()}
          </p>
        </div>
      </div>
    </div>
  )
}
