import { useEffect, useMemo, useState } from 'react'
import { AlertTriangle, CandlestickChart, Clock, Search, X } from 'lucide-react'
import type { StockConfig, USStockSymbol } from '../../types'
import { stockConfig as tx, ts } from '../../i18n/strategy-translations'
import { api } from '../../lib/api'

interface StockConfigEditorProps {
  config: StockConfig
  onChange: (config: StockConfig) => void
  disabled?: boolean
  language: string
}

// Max symbols per strategy (mirrors store.MaxStockSymbols).
const MAX_SYMBOLS = 20

// Defaults shown as placeholders (mirror the Go resolvers in
// store/strategy_stock.go; the backend stays the source of truth).
const DEFAULT_DIVERGENCE_PCT = 1.0
const DEFAULT_MAX_POSITION_PCT = 20
const DEFAULT_MAX_TOTAL_EXPOSURE_PCT = 80
const DEFAULT_MAX_POSITIONS = 5
const DEFAULT_RISK_PER_TRADE_PCT = 1.0
const STOP_ATR_DEFAULTS = {
  swing: { min: 1.5, max: 3.0 },
  position: { min: 2.0, max: 4.0 },
}

// Default config for a newly created us_stock strategy: no symbols yet,
// paper trading ON, regular session only. Values are explicit (not omitted)
// because the backend merges PUT bodies onto the stored config, so an omitted
// field would not override a previously stored value.
export const defaultStockConfig: StockConfig = {
  symbols: [],
  preset: 'swing',
  sessions: { regular: true, pre_market: false, after_hours: false },
  data_fallback_yahoo: true,
  max_divergence_pct: 0,
  max_position_pct: 0,
  max_total_exposure_pct: 0,
  max_positions: 0,
  risk_per_trade_pct: 0,
  stop_atr_min: 0,
  stop_atr_max: 0,
  paper_trading: true,
}

// "aapl, NVDABUSDT nvda" -> ['AAPL', 'NVDABUSDT', 'NVDA'] (deduped, uppercased)
function parseSymbolText(text: string): string[] {
  const out: string[] = []
  for (const raw of text.split(/[\s,，;；]+/)) {
    const sym = raw.trim().toUpperCase()
    if (sym && !out.includes(sym)) out.push(sym)
  }
  return out
}

export function StockConfigEditor({
  config,
  onChange,
  disabled,
  language,
}: StockConfigEditorProps) {
  const updateField = <K extends keyof StockConfig>(
    key: K,
    value: StockConfig[K]
  ) => {
    if (!disabled) {
      onChange({ ...config, [key]: value })
    }
  }

  const inputStyle = {
    background: '#E9E4D6',
    border: '1px solid #C0B9A2',
    color: '#1E1E1A',
  }
  const sectionStyle = {
    background: '#F2EFE6',
    border: '1px solid #C0B9A2',
  }
  const headingStyle = { color: '#1E1E1A' }
  const mutedStyle = { color: '#6E6E60' }

  // ---- symbol universe (GET /api/usstock/symbols) -------------------------
  const [universe, setUniverse] = useState<USStockSymbol[] | null>(null)
  const [universeState, setUniverseState] = useState<
    'loading' | 'ready' | 'failed'
  >('loading')
  const [query, setQuery] = useState('')
  const [manualText, setManualText] = useState(() =>
    (config.symbols || []).join(', ')
  )

  useEffect(() => {
    let cancelled = false
    api
      .getUSStockSymbols()
      .then((list) => {
        if (cancelled) return
        if (list.length > 0) {
          setUniverse(list)
          setUniverseState('ready')
        } else {
          setUniverseState('failed')
        }
      })
      .catch(() => {
        if (!cancelled) setUniverseState('failed')
      })
    return () => {
      cancelled = true
    }
  }, [])

  const symbols = config.symbols || []
  const selected = useMemo(() => new Set(symbols), [symbols])

  const filtered = useMemo(() => {
    if (!universe) return []
    const q = query.trim().toUpperCase()
    if (!q) return universe
    return universe.filter(
      (s) =>
        s.symbol.toUpperCase().includes(q) ||
        s.underlying.toUpperCase().includes(q)
    )
  }, [universe, query])

  const toggleSymbol = (symbol: string) => {
    if (disabled) return
    if (selected.has(symbol)) {
      updateField(
        'symbols',
        symbols.filter((s) => s !== symbol)
      )
    } else if (symbols.length < MAX_SYMBOLS) {
      updateField('symbols', [...symbols, symbol])
    }
  }

  const underlyingOf = (symbol: string) =>
    universe?.find((s) => s.symbol === symbol)?.underlying ||
    symbol.replace(/BUSDT$/, '')

  // ---- helpers -------------------------------------------------------------
  const sessions = config.sessions || {}
  const regularOn = sessions.regular !== false
  const updateSession = (patch: Partial<StockConfig['sessions']>) =>
    updateField('sessions', {
      regular: regularOn,
      pre_market: !!sessions.pre_market,
      after_hours: !!sessions.after_hours,
      ...patch,
    })
  const noSession = !regularOn && !sessions.pre_market && !sessions.after_hours

  const preset = config.preset === 'position' ? 'position' : 'swing'
  const atrDefaults = STOP_ATR_DEFAULTS[preset]
  const isPaper = config.paper_trading !== false

  // Numeric input: empty -> 0 ("use default"), otherwise the parsed number.
  const numField = (
    key:
      | 'max_divergence_pct'
      | 'max_position_pct'
      | 'max_total_exposure_pct'
      | 'max_positions'
      | 'risk_per_trade_pct'
      | 'stop_atr_min'
      | 'stop_atr_max',
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
      <div className="p-4 rounded-lg" style={sectionStyle}>
        <label className="block text-sm mb-1" style={headingStyle}>
          {label}
        </label>
        {opts.desc && (
          <p className="text-xs mb-2" style={mutedStyle}>
            {opts.desc}
          </p>
        )}
        <input
          type="number"
          data-testid={`stock-${key}`}
          value={value ? value : ''}
          placeholder={`${ts(tx.defaultHint, language)} ${defaultValue}`}
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
          className="w-full px-3 py-2 rounded"
          style={inputStyle}
        />
      </div>
    )
  }

  return (
    <div className="space-y-6" data-testid="stock-config-editor">
      {/* Symbols */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <CandlestickChart className="w-5 h-5" style={{ color: '#B8912A' }} />
          <h3 className="font-medium" style={headingStyle}>
            {ts(tx.symbolsSection, language)}
          </h3>
        </div>
        <div className="p-4 rounded-lg" style={sectionStyle}>
          <label className="block text-sm mb-1" style={headingStyle}>
            {ts(tx.symbols, language)}
          </label>
          <p className="text-xs mb-3" style={mutedStyle}>
            {ts(tx.symbolsDesc, language)}
          </p>

          {/* Selected chips */}
          {symbols.length > 0 && (
            <div className="flex flex-wrap gap-2 mb-3">
              {symbols.map((sym) => (
                <span
                  key={sym}
                  data-testid="stock-selected-symbol"
                  className="inline-flex items-center gap-1 px-2 py-1 rounded text-xs"
                  style={{ background: '#E9E4D6', border: '1px solid #B8912A' }}
                >
                  {underlyingOf(sym)} · {sym}
                  {!disabled && (
                    <button
                      type="button"
                      onClick={() => toggleSymbol(sym)}
                      aria-label={`remove ${sym}`}
                    >
                      <X className="w-3 h-3" />
                    </button>
                  )}
                </span>
              ))}
            </div>
          )}

          {universeState === 'loading' && (
            <p className="text-xs" style={mutedStyle}>
              {ts(tx.symbolsLoading, language)}
            </p>
          )}

          {universeState === 'ready' && (
            <div>
              <div className="relative mb-2">
                <Search
                  className="w-4 h-4 absolute left-2 top-1/2 -translate-y-1/2"
                  style={mutedStyle}
                />
                <input
                  type="text"
                  data-testid="stock-symbol-search"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder={ts(tx.symbolSearch, language)}
                  disabled={disabled}
                  className="w-full pl-8 pr-3 py-2 rounded"
                  style={inputStyle}
                />
              </div>
              <div
                className="max-h-56 overflow-y-auto rounded"
                style={{ border: '1px solid #C0B9A2' }}
              >
                {filtered.length === 0 && (
                  <p className="p-3 text-xs" style={mutedStyle}>
                    {ts(tx.symbolsNoMatch, language)}
                  </p>
                )}
                {filtered.map((s) => {
                  const checked = selected.has(s.symbol)
                  const full = !checked && symbols.length >= MAX_SYMBOLS
                  return (
                    <label
                      key={s.symbol}
                      className="flex items-center gap-2 px-3 py-1.5 text-sm cursor-pointer hover:bg-black/5"
                      style={{ opacity: full ? 0.5 : 1, color: '#1E1E1A' }}
                    >
                      <input
                        type="checkbox"
                        checked={checked}
                        disabled={disabled || full}
                        onChange={() => toggleSymbol(s.symbol)}
                      />
                      <span>
                        {s.underlying} · {s.symbol}
                      </span>
                    </label>
                  )
                })}
              </div>
              <p className="text-xs mt-2" style={mutedStyle}>
                {ts(tx.symbolsSelected, language)} {symbols.length}/
                {MAX_SYMBOLS}
              </p>
            </div>
          )}

          {universeState === 'failed' && (
            <div>
              <p className="text-xs mb-2" style={mutedStyle}>
                {ts(tx.symbolsFallback, language)}
              </p>
              <input
                type="text"
                data-testid="stock-symbol-manual"
                value={manualText}
                onChange={(e) => {
                  setManualText(e.target.value)
                  updateField('symbols', parseSymbolText(e.target.value))
                }}
                placeholder={ts(tx.symbolsFallbackPlaceholder, language)}
                disabled={disabled}
                className="w-full px-3 py-2 rounded font-mono text-sm"
                style={inputStyle}
              />
            </div>
          )}

          {symbols.length === 0 && (
            <p
              className="text-xs mt-2 flex items-center gap-1"
              style={{ color: '#C0392B' }}
            >
              <AlertTriangle className="w-3 h-3" />
              {ts(tx.symbolsEmptyWarn, language)}
            </p>
          )}
          {symbols.length > MAX_SYMBOLS && (
            <p className="text-xs mt-2" style={{ color: '#C0392B' }}>
              {ts(tx.symbolsMax, language)}
            </p>
          )}
        </div>
      </div>

      {/* Holding style preset */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <Clock className="w-5 h-5" style={{ color: '#2E7D4F' }} />
          <h3 className="font-medium" style={headingStyle}>
            {ts(tx.presetSection, language)}
          </h3>
        </div>
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {(
            [
              ['swing', tx.presetSwing, tx.presetSwingDesc],
              ['position', tx.presetPosition, tx.presetPositionDesc],
            ] as const
          ).map(([value, label, desc]) => (
            <label
              key={value}
              className="p-4 rounded-lg cursor-pointer"
              style={{
                ...sectionStyle,
                border:
                  preset === value ? '1px solid #B8912A' : sectionStyle.border,
              }}
            >
              <div className="flex items-center gap-2 mb-1">
                <input
                  type="radio"
                  name="stock-preset"
                  data-testid={`stock-preset-${value}`}
                  checked={preset === value}
                  disabled={disabled}
                  onChange={() => updateField('preset', value)}
                />
                <span className="text-sm font-medium" style={headingStyle}>
                  {ts(label, language)}
                </span>
              </div>
              <p className="text-xs" style={mutedStyle}>
                {ts(desc, language)}
              </p>
            </label>
          ))}
        </div>
      </div>

      {/* Sessions */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <Clock className="w-5 h-5" style={{ color: '#B8912A' }} />
          <h3 className="font-medium" style={headingStyle}>
            {ts(tx.sessionsSection, language)}
          </h3>
        </div>
        <div className="p-4 rounded-lg space-y-2" style={sectionStyle}>
          <label
            className="flex items-center gap-2 text-sm"
            style={headingStyle}
          >
            <input
              type="checkbox"
              data-testid="stock-session-regular"
              checked={regularOn}
              disabled={disabled}
              onChange={(e) => updateSession({ regular: e.target.checked })}
            />
            {ts(tx.sessionRegular, language)}
          </label>
          <label
            className="flex items-center gap-2 text-sm"
            style={headingStyle}
          >
            <input
              type="checkbox"
              data-testid="stock-session-pre"
              checked={!!sessions.pre_market}
              disabled={disabled}
              onChange={(e) => updateSession({ pre_market: e.target.checked })}
            />
            {ts(tx.sessionPreMarket, language)}
          </label>
          <label
            className="flex items-center gap-2 text-sm"
            style={headingStyle}
          >
            <input
              type="checkbox"
              data-testid="stock-session-after"
              checked={!!sessions.after_hours}
              disabled={disabled}
              onChange={(e) => updateSession({ after_hours: e.target.checked })}
            />
            {ts(tx.sessionAfterHours, language)}
          </label>
          <p className="text-xs pt-1" style={mutedStyle}>
            {ts(tx.sessionsNote, language)}
          </p>
          {noSession && (
            <p className="text-xs" style={{ color: '#C0392B' }}>
              {ts(tx.sessionsNoneWarn, language)}
            </p>
          )}
        </div>
      </div>

      {/* Data */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <h3 className="font-medium" style={headingStyle}>
            {ts(tx.dataSection, language)}
          </h3>
        </div>
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          <div className="p-4 rounded-lg" style={sectionStyle}>
            <label
              className="flex items-center gap-2 text-sm mb-1"
              style={headingStyle}
            >
              <input
                type="checkbox"
                data-testid="stock-yahoo-fallback"
                checked={config.data_fallback_yahoo !== false}
                disabled={disabled}
                onChange={(e) =>
                  updateField('data_fallback_yahoo', e.target.checked)
                }
              />
              {ts(tx.yahooFallback, language)}
            </label>
            <p className="text-xs" style={mutedStyle}>
              {ts(tx.yahooFallbackDesc, language)}
            </p>
          </div>
          {numField(
            'max_divergence_pct',
            ts(tx.maxDivergence, language),
            DEFAULT_DIVERGENCE_PCT,
            { step: 0.1, max: 10, desc: ts(tx.maxDivergenceDesc, language) }
          )}
        </div>
      </div>

      {/* Risk */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <h3 className="font-medium" style={headingStyle}>
            {ts(tx.riskSection, language)}
          </h3>
        </div>
        <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
          {numField(
            'max_position_pct',
            ts(tx.maxPositionPct, language),
            DEFAULT_MAX_POSITION_PCT,
            { step: 1, min: 0, max: 100 }
          )}
          {numField(
            'max_total_exposure_pct',
            ts(tx.maxTotalExposurePct, language),
            DEFAULT_MAX_TOTAL_EXPOSURE_PCT,
            { step: 1, min: 0, max: 100 }
          )}
          {numField(
            'max_positions',
            ts(tx.maxPositions, language),
            DEFAULT_MAX_POSITIONS,
            { step: 1, min: 0, max: 20, integer: true }
          )}
          {numField(
            'risk_per_trade_pct',
            ts(tx.riskPerTradePct, language),
            DEFAULT_RISK_PER_TRADE_PCT,
            { step: 0.1, min: 0, max: 5 }
          )}
          {numField(
            'stop_atr_min',
            ts(tx.stopAtrMin, language),
            atrDefaults.min,
            { step: 0.1, min: 0, max: 10 }
          )}
          {numField(
            'stop_atr_max',
            ts(tx.stopAtrMax, language),
            atrDefaults.max,
            { step: 0.1, min: 0, max: 10 }
          )}
        </div>
      </div>

      {/* Paper trading */}
      <div>
        <div className="flex items-center gap-2 mb-4">
          <h3 className="font-medium" style={headingStyle}>
            {ts(tx.paperSection, language)}
          </h3>
        </div>
        <div
          className="p-4 rounded-lg"
          data-testid="stock-paper-box"
          style={
            isPaper
              ? sectionStyle
              : { background: '#FBEAE7', border: '1px solid #C0392B' }
          }
        >
          <label
            className="flex items-center gap-2 text-sm font-medium"
            style={headingStyle}
          >
            <input
              type="checkbox"
              data-testid="stock-paper-trading"
              checked={isPaper}
              disabled={disabled}
              onChange={(e) => updateField('paper_trading', e.target.checked)}
            />
            {ts(tx.paperTrading, language)}
          </label>
          <p className="text-xs mt-1" style={mutedStyle}>
            {ts(tx.paperTradingDesc, language)}
          </p>
          {!isPaper && (
            <p
              className="text-xs mt-2 flex items-center gap-1 font-medium"
              data-testid="stock-live-warning"
              style={{ color: '#C0392B' }}
            >
              <AlertTriangle className="w-3 h-3" />
              {ts(tx.liveWarning, language)}
            </p>
          )}
        </div>
      </div>
    </div>
  )
}
