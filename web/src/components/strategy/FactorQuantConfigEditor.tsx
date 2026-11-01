import { cn } from '../../lib/cn'
import { Input } from '../ui'
import { Sigma } from 'lucide-react'
import type { FactorQuantConfig } from '../../types'
import { ts } from '../../i18n/strategy-translations'

interface FactorQuantConfigEditorProps {
  config: FactorQuantConfig
  onChange: (config: FactorQuantConfig) => void
  disabled?: boolean
  language: string
}

// Page-level strings (type-picker button, config-section title and the
// right-panel runtime note) so StrategyStudioPage can render the new type
// without growing the shared translations table. Same {zh,en} shape consumed
// by ts() as the editor texts below.
export const factorQuantTexts = {
  typeLabel: { zh: '因子量化（程序化）', en: 'Factor Quant (programmatic)' },
  typeDesc: {
    zh: '读取因子挖掘的目标权重，程序化自动调仓，不调用大模型',
    en: 'Trades factor-mining target weights programmatically, no LLM',
  },
  sectionTitle: { zh: '因子量化配置', en: 'Factor quant configuration' },
  runtimeNote: {
    zh: '因子量化策略为程序化执行，不使用大模型提示词',
    en: 'Factor-quant strategies run programmatically without LLM prompts',
  },
}

// Editor texts, same {zh,en} shape consumed by ts() as the regime section of
// StockConfigEditor (which also keeps its strings local).
const fq = {
  title: { zh: '程序化量化交易员', en: 'Programmatic quant trader' },
  intro: {
    zh: '程序化量化交易员：读取因子挖掘每晚生成的目标权重（quant_targets.json），按调仓周期在模拟账户自动调仓，不调用大模型。只能绑定模拟账户。美股目前没有通过验证的因子，交易员会保持空仓直到因子出现。',
    en: 'Programmatic quant trader: reads the target weights (quant_targets.json) generated nightly by factor mining and rebalances a paper account on the rebalance horizon — no LLM involved. Paper accounts only. No validated factors exist for US stocks yet, so the trader stays flat until factors appear.',
  },
  marketSection: { zh: '市场', en: 'Market' },
  marketCrypto: { zh: '加密（模拟合约）', en: 'Crypto (paper futures)' },
  marketCryptoDesc: {
    zh: '多空中性：做多因子排名前 20%、做空后 20%，空头带灾难止损',
    en: 'Market-neutral: long top-20% / short bottom-20% factor ranks, shorts carry a disaster stop',
  },
  marketUs: {
    zh: '美股（bStock 模拟账本）',
    en: 'US stocks (bStock paper ledger)',
  },
  marketUsDesc: {
    zh: '只做多；目前没有通过验证的因子，保持空仓直到因子出现',
    en: 'Long only; no validated factors yet — stays flat until factors appear',
  },
  horizon: { zh: '调仓周期 horizon（天）', en: 'Rebalance horizon (days)' },
  horizonDesc: {
    zh: '必须等于 quant_targets.json 的 horizon；美股填 0 表示接受文件给出的值',
    en: 'Must equal the targets file horizon; US 0 = accept whatever the file says',
  },
  capitalFraction: { zh: '资金使用比例', en: 'Capital fraction' },
  capitalFractionDesc: {
    zh: '用多大比例的权益执行目标权重，(0–1]；留空 = 默认 1.0',
    en: 'Fraction of equity to execute with, (0–1]; empty = default 1.0',
  },
  leverage: { zh: '杠杆', en: 'Leverage' },
  leverageDesc: {
    zh: '仅加密：模拟合约杠杆（1–10），用来保证保证金充足；留空 = 默认 3',
    en: 'Crypto only: paper-futures leverage (1–10) for margin headroom; empty = default 3',
  },
  rebalanceBand: { zh: '调仓阈值 %', en: 'Rebalance band %' },
  rebalanceBandDesc: {
    zh: '目标名义与当前持仓之差小于目标的 N% 时不交易，减少手续费；留空 = 默认 0.1',
    en: 'Skip a leg when |target − current| < target × N; empty = default 0.1',
  },
  minOrder: { zh: '单笔最小名义 (USDT)', en: 'Min order notional (USDT)' },
  minOrderDesc: {
    zh: '低于该金额的订单不下；留空 = 默认 10',
    en: 'Orders below this notional are skipped; empty = default 10',
  },
  shortStop: { zh: '空头灾难止损 %', en: 'Short disaster stop %' },
  shortStopDesc: {
    zh: '仅加密：触发价 = 入场价 ×(1+N)；填 0 关闭',
    en: 'Crypto only: stop at entry × (1+N); 0 = disabled',
  },
  usFee: { zh: '美股手续费 (bp)', en: 'US fee (bp)' },
  usFeeDesc: {
    zh: '仅美股：每笔成交手续费，基点（10 = 0.1%），从模拟现金扣除；0 = 无手续费；留空 = 默认 10',
    en: 'US only: per-fill fee in basis points (10 = 0.1%) taken from paper cash; 0 = none; empty = default 10',
  },
  usSlippage: { zh: '美股滑点 (bp)', en: 'US slippage (bp)' },
  usSlippageDesc: {
    zh: '仅美股：买入价高于报价、卖出价低于报价的基点数；0 = 无滑点；留空 = 默认 3',
    en: 'US only: basis points a buy fills above / a sell fills below the quote; 0 = none; empty = default 3',
  },
  maxStale: { zh: '目标过期天数', en: 'Max stale days' },
  maxStaleDesc: {
    zh: 'as_of 落后今天（UTC）超过这么多天就不按它调仓并告警；留空 = 默认 horizon+2',
    en: 'Targets with as_of older than this (UTC) are not traded; empty = horizon+2',
  },
  advanced: { zh: '高级', en: 'Advanced' },
  targetsPath: {
    zh: 'quant_targets.json 路径覆盖',
    en: 'quant_targets.json path override',
  },
  targetsPathDesc: {
    zh: '留空使用默认路径 data/factor_mining/{market}/quant_targets.json；一般无需修改，仅供测试',
    en: 'Empty = default data/factor_mining/{market}/quant_targets.json; testing only',
  },
  defaultHint: { zh: '默认', en: 'Default' },
}

// Defaults applied when the market switches (crypto: 3-day horizon, 3x paper
// leverage, short stop on; us: horizon 0 = accept the file's horizon).
const MARKET_DEFAULTS = {
  crypto: { horizon: 3, leverage: 3, short_stop_pct: 0.5 },
  us: { horizon: 0 },
} as const

// Default config for a newly created factor_quant strategy (crypto, 3-day
// rebalance). Values are explicit (not omitted) because the backend merges
// PUT bodies onto the stored config, so an omitted field would not override
// a previously stored value.
export const defaultFactorQuantConfig: FactorQuantConfig = {
  market: 'crypto',
  horizon: 3,
  leverage: 3,
  short_stop_pct: 0.5,
}

export function FactorQuantConfigEditor({
  config,
  onChange,
  disabled,
  language,
}: FactorQuantConfigEditorProps) {
  const updateField = <K extends keyof FactorQuantConfig>(
    key: K,
    value: FactorQuantConfig[K]
  ) => {
    if (!disabled) {
      onChange({ ...config, [key]: value })
    }
  }

  const market = config.market === 'us' ? 'us' : 'crypto'

  // Market switch applies that market's defaults (see MARKET_DEFAULTS).
  const switchMarket = (next: 'crypto' | 'us') => {
    if (disabled || next === market) return
    onChange({ ...config, market: next, ...MARKET_DEFAULTS[next] })
  }

  // Numeric input: empty -> 0 ("use default"), otherwise the parsed number.
  const numField = (
    key:
      | 'horizon'
      | 'capital_fraction'
      | 'leverage'
      | 'rebalance_band_pct'
      | 'min_order_usdt'
      | 'short_stop_pct'
      | 'max_stale_days',
    label: string,
    defaultValue: number,
    opts: {
      step?: number
      min?: number
      max?: number
      integer?: boolean
      desc?: string
    }
  ) => {
    const value = config[key]
    return (
      <div className="p-3 rounded-lg bg-surface-2 border border-line">
        <label className="block text-[13px] mb-1 text-fg-2">{label}</label>
        {opts.desc && <p className="text-xs mb-2 text-fg-3">{opts.desc}</p>}
        <Input
          type="number"
          data-testid={`factor-${key}`}
          value={value ? value : ''}
          placeholder={`${ts(fq.defaultHint, language)} ${defaultValue}`}
          onChange={(e) => {
            const raw = e.target.value
            if (raw === '' || raw === '-') {
              updateField(key, 0)
              return
            }
            const n = opts.integer ? parseInt(raw, 10) : parseFloat(raw)
            updateField(key, Number.isFinite(n) ? n : 0)
          }}
          disabled={disabled}
          min={opts.min}
          max={opts.max}
          step={opts.step ?? 1}
          className="w-full px-3 bg-surface-2 border border-line text-fg num text-right"
        />
      </div>
    )
  }

  // Basis-point input where 0 is a real value (no cost) and empty means the
  // backend default: empty clears the field to null so a merged PUT drops any
  // stored value.
  const bpsField = (
    key: 'us_fee_bps' | 'us_slippage_bps',
    label: string,
    defaultValue: number,
    desc: string
  ) => {
    const value = config[key]
    return (
      <div className="p-3 rounded-lg bg-surface-2 border border-line">
        <label className="block text-[13px] mb-1 text-fg-2">{label}</label>
        <p className="text-xs mb-2 text-fg-3">{desc}</p>
        <Input
          type="number"
          data-testid={`factor-${key}`}
          value={value ?? ''}
          placeholder={`${ts(fq.defaultHint, language)} ${defaultValue}`}
          onChange={(e) => {
            const raw = e.target.value
            const n = parseFloat(raw)
            updateField(key, raw === '' || !Number.isFinite(n) ? null : n)
          }}
          disabled={disabled}
          min={0}
          max={100}
          step={0.5}
          className="w-full px-3 bg-surface-2 border border-line text-fg num text-right"
        />
      </div>
    )
  }

  // Default max_stale_days = horizon + 2 (the backend resolver).
  const effectiveHorizon = config.horizon ?? MARKET_DEFAULTS[market].horizon
  const staleDefault = effectiveHorizon + 2

  return (
    <div className="space-y-4" data-testid="factor-config-editor">
      {/* What this trader does */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <Sigma className="w-5 h-5 text-brand" />
          <h3 className="font-medium text-fg">{ts(fq.title, language)}</h3>
        </div>
        <p className="text-xs mb-4 p-3 rounded-lg bg-surface-2 border border-line text-fg-3">
          {ts(fq.intro, language)}
        </p>

        <p className="text-[13px] mb-2 text-fg-2">
          {ts(fq.marketSection, language)}
        </p>
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {(
            [
              ['crypto', fq.marketCrypto, fq.marketCryptoDesc],
              ['us', fq.marketUs, fq.marketUsDesc],
            ] as const
          ).map(([value, label, desc]) => (
            <label
              key={value}
              data-testid={`factor-market-${value}`}
              className={cn(
                'p-3 rounded-lg cursor-pointer',
                'bg-surface-2',
                market === value ? 'border border-brand' : 'border border-line',
                'text-fg-2'
              )}
            >
              <div className="flex items-center gap-2 mb-1">
                <input
                  type="radio"
                  name="factor-market"
                  checked={market === value}
                  disabled={disabled}
                  onChange={() => switchMarket(value)}
                  className="accent-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:opacity-50"
                />
                <span className="text-sm font-medium text-fg">
                  {ts(label, language)}
                </span>
              </div>
              <p className="text-xs text-fg-3">{ts(desc, language)}</p>
            </label>
          ))}
        </div>
      </div>

      {/* Execution parameters */}
      <div>
        <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
          {numField(
            'horizon',
            ts(fq.horizon, language),
            MARKET_DEFAULTS[market].horizon,
            {
              step: 1,
              min: 0,
              max: 30,
              integer: true,
              desc: ts(fq.horizonDesc, language),
            }
          )}
          {numField('capital_fraction', ts(fq.capitalFraction, language), 1, {
            step: 0.05,
            min: 0.01,
            max: 1,
            desc: ts(fq.capitalFractionDesc, language),
          })}
          {market === 'crypto' &&
            numField(
              'leverage',
              ts(fq.leverage, language),
              MARKET_DEFAULTS.crypto.leverage,
              {
                step: 1,
                min: 1,
                max: 10,
                integer: true,
                desc: ts(fq.leverageDesc, language),
              }
            )}
          {numField('rebalance_band_pct', ts(fq.rebalanceBand, language), 0.1, {
            step: 0.01,
            min: 0,
            max: 1,
            desc: ts(fq.rebalanceBandDesc, language),
          })}
          {numField('min_order_usdt', ts(fq.minOrder, language), 10, {
            step: 1,
            min: 0,
            desc: ts(fq.minOrderDesc, language),
          })}
          {market === 'crypto' &&
            numField(
              'short_stop_pct',
              ts(fq.shortStop, language),
              MARKET_DEFAULTS.crypto.short_stop_pct,
              {
                step: 0.1,
                min: 0,
                desc: ts(fq.shortStopDesc, language),
              }
            )}
          {market === 'us' &&
            bpsField(
              'us_fee_bps',
              ts(fq.usFee, language),
              10,
              ts(fq.usFeeDesc, language)
            )}
          {market === 'us' &&
            bpsField(
              'us_slippage_bps',
              ts(fq.usSlippage, language),
              3,
              ts(fq.usSlippageDesc, language)
            )}
          {numField('max_stale_days', ts(fq.maxStale, language), staleDefault, {
            step: 1,
            min: 0,
            integer: true,
            desc: ts(fq.maxStaleDesc, language),
          })}
        </div>
      </div>

      {/* Advanced */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <h3 className="font-medium text-fg">{ts(fq.advanced, language)}</h3>
        </div>
        <div className="p-3 rounded-lg bg-surface-2 border border-line">
          <label className="block text-[13px] mb-1 text-fg-2">
            {ts(fq.targetsPath, language)}
          </label>
          <p className="text-xs mb-2 text-fg-3">
            {ts(fq.targetsPathDesc, language)}
          </p>
          <Input
            type="text"
            data-testid="factor-targets_path"
            value={config.targets_path || ''}
            onChange={(e) => {
              const v = e.target.value
              updateField('targets_path', v.length > 0 ? v : undefined)
            }}
            disabled={disabled}
            placeholder="data/factor_mining/crypto/quant_targets.json"
            className="w-full px-3 num bg-surface-2 border border-line text-fg"
          />
        </div>
      </div>
    </div>
  )
}
