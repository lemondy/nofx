package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Hard limits to prevent token explosion in AI requests
const (
	MaxCandidateCoins = 10
	// MaxPositions is a sanity BOUND on risk_control.max_positions (fat-finger
	// protection), not the default — the configured value is honored up to
	// this cap. The default TEMPLATE for new strategies stays 3.
	MaxPositions  = 10
	MaxTimeframes = 4
	MinKlineCount = 10
	// 120: the structured signal derives indicators from the closed-bar
	// window — 30 bars left EMA50/MACD unstable (data_quality.sufficient
	// flagged EVERY candidate as insufficient). 120 keeps ≥60 closed bars
	// even after the forming-bar drop on the longest fetched TF.
	MaxKlineCount = 120
)

// ClampLimits enforces product-level limits on strategy config to prevent token overflow.
func (c *StrategyConfig) ClampLimits() {
	// Clamp coin source limits
	if c.CoinSource.AI500Limit > MaxCandidateCoins {
		c.CoinSource.AI500Limit = MaxCandidateCoins
	}
	if c.CoinSource.OITopLimit > MaxCandidateCoins {
		c.CoinSource.OITopLimit = MaxCandidateCoins
	}
	if c.CoinSource.OILowLimit > MaxCandidateCoins {
		c.CoinSource.OILowLimit = MaxCandidateCoins
	}

	// Clamp static coins
	if c.CoinSource.PiggyDashLimit > MaxCandidateCoins {
		c.CoinSource.PiggyDashLimit = MaxCandidateCoins
	}
	if c.CoinSource.ShortScanLimit > MaxCandidateCoins {
		c.CoinSource.ShortScanLimit = MaxCandidateCoins
	}
	if len(c.CoinSource.StaticCoins) > MaxCandidateCoins {
		c.CoinSource.StaticCoins = c.CoinSource.StaticCoins[:MaxCandidateCoins]
	}

	// Clamp kline count
	if c.Indicators.Klines.PrimaryCount < MinKlineCount {
		c.Indicators.Klines.PrimaryCount = MinKlineCount
	}
	if c.Indicators.Klines.PrimaryCount > MaxKlineCount {
		c.Indicators.Klines.PrimaryCount = MaxKlineCount
	}
	if c.Indicators.Klines.LongerCount > MaxKlineCount {
		c.Indicators.Klines.LongerCount = MaxKlineCount
	}

	// Clamp timeframes
	if len(c.Indicators.Klines.SelectedTimeframes) > MaxTimeframes {
		c.Indicators.Klines.SelectedTimeframes = c.Indicators.Klines.SelectedTimeframes[:MaxTimeframes]
	}

	// Clamp max positions
	if c.RiskControl.MaxPositions > MaxPositions {
		c.RiskControl.MaxPositions = MaxPositions
	}

}

// StrategyStore strategy storage
type StrategyStore struct {
	db *gorm.DB
}

// Strategy strategy configuration
type Strategy struct {
	ID            string    `gorm:"primaryKey" json:"id"`
	UserID        string    `gorm:"column:user_id;not null;default:'';index" json:"user_id"`
	Name          string    `gorm:"not null" json:"name"`
	Description   string    `gorm:"default:''" json:"description"`
	IsActive      bool      `gorm:"column:is_active;default:false;index" json:"is_active"`
	IsDefault     bool      `gorm:"column:is_default;default:false" json:"is_default"`
	IsPublic      bool      `gorm:"column:is_public;default:false;index" json:"is_public"`    // whether visible in strategy market
	ConfigVisible bool      `gorm:"column:config_visible;default:true" json:"config_visible"` // whether config details are visible
	Config        string    `gorm:"not null;default:'{}'" json:"config"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (Strategy) TableName() string { return "strategies" }

// StrategyConfig strategy configuration details (JSON structure)
type StrategyConfig struct {
	// Strategy type: "ai_trading" (default) or "grid_trading"
	StrategyType string `json:"strategy_type,omitempty"`

	// language setting: "zh" for Chinese, "en" for English
	// This determines the language used for data formatting and prompt generation
	Language string `json:"language,omitempty"`
	// coin source configuration
	CoinSource CoinSourceConfig `json:"coin_source"`
	// quantitative data configuration
	Indicators IndicatorConfig `json:"indicators"`
	// custom prompt (appended at the end)
	CustomPrompt string `json:"custom_prompt,omitempty"`
	// StatsWindowDays is the rolling window (in days) for the trading stats
	// the AI sees (PF/win-rate/expectancy in strategy_health). Only trades
	// closed within the window feed those numbers. 0 or absent = 30 days
	// (default), a negative value = full history (window disabled). The
	// window changes what is REPORTED to the AI only — no trades are ever
	// deleted or moved.
	StatsWindowDays int `json:"stats_window_days,omitempty"`

	// USStockSessionBoostPct: during US regular trading hours (Eastern
	// Mon–Fri 09:30–16:00), short-scan candidates that are US equity tokens
	// get their score multiplied by (1 + pct/100) BEFORE the top-N cut —
	// equity tokens tracked the underlying session show lower volatility and
	// (so far) a better realized win rate than the crypto pool (user
	// directive 2026-09-23; journal baseline: stocks 11 trades 54.5% win vs
	// crypto 184 trades 40.8%). 0 = default 20, negative = disabled.
	// Weekends stay blocked by stock_weekend_no_open regardless.
	USStockSessionBoostPct float64 `json:"us_stock_session_boost_pct,omitempty"`
	// risk control configuration
	RiskControl RiskControlConfig `json:"risk_control"`
	// editable sections of System Prompt
	PromptSections PromptSectionsConfig `json:"prompt_sections,omitempty"`

	// Grid trading configuration (only used when StrategyType == "grid_trading")
	GridConfig *GridStrategyConfig `json:"grid_config,omitempty"`
}

// GridStrategyConfig grid trading specific configuration
type GridStrategyConfig struct {
	// Trading pair (e.g., "BTCUSDT")
	Symbol string `json:"symbol"`
	// Number of grid levels (5-50)
	GridCount int `json:"grid_count"`
	// Total investment in USDT
	TotalInvestment float64 `json:"total_investment"`
	// Leverage (1-20)
	Leverage int `json:"leverage"`
	// Upper price boundary (0 = auto-calculate from ATR)
	UpperPrice float64 `json:"upper_price"`
	// Lower price boundary (0 = auto-calculate from ATR)
	LowerPrice float64 `json:"lower_price"`
	// Use ATR to auto-calculate bounds
	UseATRBounds bool `json:"use_atr_bounds"`
	// ATR multiplier for bound calculation (default 2.0)
	ATRMultiplier float64 `json:"atr_multiplier"`
	// Position distribution: "uniform" | "gaussian" | "pyramid"
	Distribution string `json:"distribution"`
	// Maximum drawdown percentage before emergency exit
	MaxDrawdownPct float64 `json:"max_drawdown_pct"`
	// Stop loss percentage per position
	StopLossPct float64 `json:"stop_loss_pct"`
	// Daily loss limit percentage
	DailyLossLimitPct float64 `json:"daily_loss_limit_pct"`
	// Use maker-only orders for lower fees
	UseMakerOnly bool `json:"use_maker_only"`
	// Enable automatic grid direction adjustment based on box breakouts
	EnableDirectionAdjust bool `json:"enable_direction_adjust"`
	// Direction bias ratio for long_bias/short_bias modes (default 0.7 = 70%/30%)
	DirectionBiasRatio float64 `json:"direction_bias_ratio"`
}

// PromptSectionsConfig editable sections of System Prompt
type PromptSectionsConfig struct {
	// role definition (title + description)
	RoleDefinition string `json:"role_definition,omitempty"`
	// trading frequency awareness
	TradingFrequency string `json:"trading_frequency,omitempty"`
	// entry standards
	EntryStandards string `json:"entry_standards,omitempty"`
	// decision process
	DecisionProcess string `json:"decision_process,omitempty"`
}

// CoinSourceConfig coin source configuration
type CoinSourceConfig struct {
	// source type: "static" | "ai500" | "oi_top" | "oi_low" | "piggy_dash" | "mixed"
	// "mixed" combines any selection of the individual sources (multi-select)
	SourceType string `json:"source_type"`
	// static coin list (used when source_type = "static", or in mixed mode
	// when UseStatic is set)
	StaticCoins []string `json:"static_coins,omitempty"`
	// excluded coins list (filtered out from all sources)
	ExcludedCoins []string `json:"excluded_coins,omitempty"`
	// mixed mode: whether to include the static coin list as a source.
	// Pointer so absent (legacy configs, which always included static coins)
	// can default to true while an explicit false excludes them.
	UseStatic *bool `json:"use_static,omitempty"`
	// whether to use AI500 coin pool
	UseAI500 bool `json:"use_ai500"`
	// AI500 coin pool maximum count
	AI500Limit int `json:"ai500_limit,omitempty"`
	// whether to use OI Top (OI increase ranking, suitable for long positions)
	UseOITop bool `json:"use_oi_top"`
	// OI Top maximum count
	OITopLimit int `json:"oi_top_limit,omitempty"`
	// whether to use OI Low (OI decrease ranking, suitable for short positions)
	UseOILow bool `json:"use_oi_low"`
	// OI Low maximum count
	OILowLimit int `json:"oi_low_limit,omitempty"`
	// whether to use the breakout engine "piggy dash" source (猪猪冲刺:
	// strongest breakout/breakdown signals from the 5-min scheduler)
	UsePiggyDash       bool   `json:"use_piggy_dash"`
	PiggyDashLimit     int    `json:"piggy_dash_limit,omitempty"`
	PiggyDashDirection string `json:"piggy_dash_direction,omitempty"` // "", "breakout", "breakdown"
	// whether to use the "short scan" source (做空扫描: top 24h gainers ranked
	// by short-suitability score computed from Binance indicators)
	UseShortScan bool `json:"use_short_scan"`
	// ShortScan maximum count
	ShortScanLimit int `json:"short_scan_limit,omitempty"`
	// ShortScan funding-rate crowding threshold (percent per 8h, e.g. 0.03 =
	// 0.03%). Injected into the AI prompt as a short-entry condition; injected
	// value falls back to the built-in default (0.03) when unset.
	ShortScanFundingRatePct float64 `json:"short_scan_funding_rate_pct,omitempty"`
	// Minimum open-interest value (millions USD) for a candidate coin to be
	// analyzed — lower-liquidity coins are skipped. 0 = built-in default (15M).
	MinOIValueMillions float64 `json:"min_oi_value_millions,omitempty"`
	// Gainer history pool (历史涨幅池): the past N days of daily top-gainer
	// snapshots are merged (deduped) into the short-scan universe, so coins
	// that pumped hard days ago and have since rolled over — already off
	// today's 24h board — still get scored as short candidates.
	// 0 = built-in default (7 days), negative = pool disabled.
	ShortScanHistoryDays int `json:"short_scan_history_days,omitempty"`
	// Cap on EXTRA history-pool symbols analyzed per scan (each costs ~6
	// fapi calls every 5 min). 0 = built-in default (30).
	ShortScanHistoryMax int `json:"short_scan_history_max,omitempty"`
	// whether to use Hyperliquid All coins (all available perp pairs)
	UseHyperAll bool `json:"use_hyper_all"`
	// whether to use Hyperliquid Main coins (top N by 24h volume)
	UseHyperMain bool `json:"use_hyper_main"`
	// Hyperliquid Main maximum count (default 20)
	HyperMainLimit int `json:"hyper_main_limit,omitempty"`
	// Note: API URLs are now built automatically using NofxOSAPIKey from IndicatorConfig
}

// DefaultMinOIValueMillions is the built-in OI-value floor applied when the
// per-strategy min_oi_value_millions is unset (0).
const DefaultMinOIValueMillions = 15.0

// EffectiveMinOIMillions returns the configured OI-value floor, falling back
// to the built-in default when unset. All OI-filter call sites go through
// this method — the default lives here and nowhere else.
func (c *CoinSourceConfig) EffectiveMinOIMillions() float64 {
	if c.MinOIValueMillions <= 0 {
		return DefaultMinOIValueMillions
	}
	return c.MinOIValueMillions
}

// IndicatorConfig indicator configuration
type IndicatorConfig struct {
	// K-line configuration
	Klines KlineConfig `json:"klines"`
	// raw kline data (OHLCV) - always enabled, required for AI analysis
	EnableRawKlines bool `json:"enable_raw_klines"`
	// technical indicator switches
	EnableEMA         bool `json:"enable_ema"`
	EnableMACD        bool `json:"enable_macd"`
	EnableRSI         bool `json:"enable_rsi"`
	EnableATR         bool `json:"enable_atr"`
	EnableBOLL        bool `json:"enable_boll"` // Bollinger Bands
	EnableVolume      bool `json:"enable_volume"`
	EnableOI          bool `json:"enable_oi"`           // open interest
	EnableFundingRate bool `json:"enable_funding_rate"` // funding rate
	// EMA period configuration
	EMAPeriods []int `json:"ema_periods,omitempty"` // default [20, 50]
	// RSI period configuration
	RSIPeriods []int `json:"rsi_periods,omitempty"` // default [7, 14]
	// ATR period configuration
	ATRPeriods []int `json:"atr_periods,omitempty"` // default [14]
	// BOLL period configuration (period, standard deviation multiplier is fixed at 2)
	BOLLPeriods []int `json:"boll_periods,omitempty"` // default [20] - can select multiple timeframes
	// external data sources
	ExternalDataSources []ExternalDataSource `json:"external_data_sources,omitempty"`

	// ========== NofxOS Unified API Configuration ==========
	// Unified API Key for all NofxOS data sources
	NofxOSAPIKey string `json:"nofxos_api_key,omitempty"`

	// quantitative data sources (capital flow, position changes, price changes)
	EnableQuantData    bool `json:"enable_quant_data"`    // whether to enable quantitative data
	EnableQuantOI      bool `json:"enable_quant_oi"`      // whether to show OI data
	EnableQuantNetflow bool `json:"enable_quant_netflow"` // whether to show Netflow data

	// OI ranking data (market-wide open interest increase/decrease rankings)
	EnableOIRanking   bool   `json:"enable_oi_ranking"`             // whether to enable OI ranking data
	OIRankingDuration string `json:"oi_ranking_duration,omitempty"` // duration: 1h, 4h, 24h
	OIRankingLimit    int    `json:"oi_ranking_limit,omitempty"`    // number of entries (default 10)

	// NetFlow ranking data (market-wide fund flow rankings - institution/personal)
	EnableNetFlowRanking   bool   `json:"enable_netflow_ranking"`             // whether to enable NetFlow ranking data
	NetFlowRankingDuration string `json:"netflow_ranking_duration,omitempty"` // duration: 1h, 4h, 24h
	NetFlowRankingLimit    int    `json:"netflow_ranking_limit,omitempty"`    // number of entries (default 10)

	// Price ranking data (market-wide gainers/losers)
	EnablePriceRanking   bool   `json:"enable_price_ranking"`             // whether to enable price ranking data
	PriceRankingDuration string `json:"price_ranking_duration,omitempty"` // durations: "1h" or "1h,4h,24h"
	PriceRankingLimit    int    `json:"price_ranking_limit,omitempty"`    // number of entries per ranking (default 10)
}

// KlineConfig K-line configuration
type KlineConfig struct {
	// primary timeframe: "1m", "3m", "5m", "15m", "1h", "4h"
	PrimaryTimeframe string `json:"primary_timeframe"`
	// primary timeframe K-line count
	PrimaryCount int `json:"primary_count"`
	// longer timeframe
	LongerTimeframe string `json:"longer_timeframe,omitempty"`
	// longer timeframe K-line count
	LongerCount int `json:"longer_count,omitempty"`
	// whether to enable multi-timeframe analysis
	EnableMultiTimeframe bool `json:"enable_multi_timeframe"`
	// selected timeframe list (new: supports multi-timeframe selection)
	SelectedTimeframes []string `json:"selected_timeframes,omitempty"`
}

// ExternalDataSource external data source configuration
type ExternalDataSource struct {
	Name        string            `json:"name"`   // data source name
	Type        string            `json:"type"`   // type: "api" | "webhook"
	URL         string            `json:"url"`    // API URL
	Method      string            `json:"method"` // HTTP method
	Headers     map[string]string `json:"headers,omitempty"`
	DataPath    string            `json:"data_path,omitempty"`    // JSON data path
	RefreshSecs int               `json:"refresh_secs,omitempty"` // refresh interval (seconds)
}

// RiskControlConfig risk control configuration
type RiskControlConfig struct {
	// Max number of coins held simultaneously (CODE ENFORCED)
	MaxPositions int `json:"max_positions"`

	// BTC/ETH exchange leverage for opening positions (AI guided)
	BTCETHMaxLeverage int `json:"btc_eth_max_leverage"`
	// Altcoin exchange leverage for opening positions (AI guided)
	AltcoinMaxLeverage int `json:"altcoin_max_leverage"`

	// BTC/ETH single position max value = equity × this ratio (CODE ENFORCED, default: 5)
	BTCETHMaxPositionValueRatio float64 `json:"btc_eth_max_position_value_ratio"`
	// Altcoin single position max value = equity × this ratio (CODE ENFORCED, default: 1)
	AltcoinMaxPositionValueRatio float64 `json:"altcoin_max_position_value_ratio"`

	// Max margin utilization (e.g. 0.9 = 90%) (CODE ENFORCED)
	MaxMarginUsage float64 `json:"max_margin_usage"`
	// Min position size in USDT (CODE ENFORCED)
	MinPositionSize float64 `json:"min_position_size"`

	// Min take_profit / stop_loss ratio (CODE ENFORCED at open: entries with a
	// lower computed ratio are rejected)
	MinRiskRewardRatio float64 `json:"min_risk_reward_ratio"`
	// Min AI confidence to open position (AI guided)
	MinConfidence int `json:"min_confidence"`

	// Min holding period in minutes before AI-initiated closes are allowed.
	// Exchange stop-loss/take-profit triggers bypass this lock. 0 = disabled. (CODE ENFORCED)
	MinHoldMinutes int `json:"min_hold_minutes"`
	// MaxSpreadPct: reject opens when the order-book spread exceeds this % of
	// mid (a wide spread eats the limit-order edge and taxes market fills).
	// 0 = default 0.5%; negative = disabled. Fail-open when no book. (CODE ENFORCED)
	MaxSpreadPct float64 `json:"max_spread_pct"`
	// MaxVendorDivergencePct: hard-block both directions when the vendor
	// forming-close vs live ticker diverges beyond this % — entry/SL/TP are
	// all priced off the live tick, so a large vendor gap invalidates the
	// whole setup (MYXUSDT 09-18: −2.57%). 0 = default 1%; negative =
	// disabled. Surfaced via hard_entry_gate.failed VENDOR_DIVERGENCE_x.xx.
	MaxVendorDivergencePct float64 `json:"max_vendor_divergence_pct"`
	// StockWeekendNoOpen: block new opens on Binance tokenized stocks
	// (underlyingSubType "Stocks") during the US-market weekend (Sat/Sun ET)
	// — weekend volatility and edge are poor until Binance supports 24h stock
	// trading. nil/true = block (default ON); false = allow. Closes, SL/TP
	// fills and drawdown-protect are unaffected. (CODE ENFORCED)
	StockWeekendNoOpen *bool `json:"stock_weekend_no_open,omitempty"`
	// ProfitLockAtR: the R-multiple (PnL ÷ initial stop distance) that arms
	// the profit lock — at this level the program market-trims 50% of the
	// position (once) AND moves the stop-loss to entry (breakeven). The
	// remaining half rides to the structural TP. 0 = default 1R; negative =
	// disabled. Supersedes the ROE trim tier of the TP ladder while active;
	// the 25% full-close backstop and drawdown-protect stay. (CODE ENFORCED)
	ProfitLockAtR float64 `json:"profit_lock_at_r"`
	// ProfitLockBEOffsetR: how far PAST entry (long; mirror for short) the
	// 1R lock parks the stop, in units of the opening risk R. 0.2 = entry
	// +0.2R — locks a sliver of profit so a post-lock pullback can't scratch
	// the runner back to flat. 0 = default 0.2 (09-21 user experiment);
	// negative = pure breakeven at entry (legacy).
	ProfitLockBEOffsetR float64 `json:"profit_lock_be_offset_r"`
	// TPCloseFraction: the fraction of the position the take-profit algo
	// closes at the planned structure level. The remainder stays on as a
	// trend-runner under the trailing stop (2×ATR ratchet) — a resting
	// full-size TP structurally sold every spike top (BTCUSDT 2026-09-21:
	// TP filled 83000, price printed 84275 in the same minute). 0 = default
	// 0.5 (09-21 user experiment); negative = 1.0 (legacy full close).
	// Collapses to 1.0 when trailing_stop_enabled is off — a runner without
	// a ratchet just gives the move back. (CODE ENFORCED)
	TPCloseFraction float64 `json:"tp_close_fraction"`
	// TP ladder on leveraged PnL% (CODE ENFORCED): at >= TpTrimProfitPct the
	// program market-trims 1/3 of the position (once per position); at >=
	// TpFullProfitPct it closes the rest. 0 = defaults 10/25; negative = that
	// tier off. Exchange SL/TP and drawdown-protect unaffected.
	TpTrimProfitPct float64 `json:"tp_trim_profit_pct"`
	TpFullProfitPct float64 `json:"tp_full_profit_pct"`
	// TpFullYieldsToLock: when the 1R profit lock is active, the ROE full
	// tier (default 25% leveraged) ALSO yields to it, not just the trim tier.
	// Without this, a 5%-price spike at 5x closes the whole position at the
	// ladder step while the trend-runner design wants the rest riding to the
	// structural target (QUANT_REVIEW_2026-09-22 C3). Default OFF = current
	// behavior — flipping it mid-experiment would corrupt the 2-week R
	// distribution retest, so the user opts in. (CODE ENFORCED when true)
	TpFullYieldsToLock bool `json:"tp_full_yields_to_lock"`
	// EarlyCloseMinHours: AI-initiated closes before this many hours of hold
	// time are blocked unless the 1h timeframe shows ≥2 closed candles against
	// the position direction (trend-change evidence). Exchange SL/TP triggers
	// and the drawdown-protect close bypass it by construction (neither is an
	// AI close decision). 0 = default 4h; negative = disabled. (CODE ENFORCED)
	EarlyCloseMinHours int `json:"early_close_min_hours"`
	// Block open_short when the 1d trend is up (counter-trend protection). (CODE ENFORCED)
	BlockShort1dUptrend bool `json:"block_short_1d_uptrend"`
	// Entry timing gate: the finest sub-hour timeframe (15m/30m) trend must
	// align with the entry direction — longs need up/pullback, shorts need
	// down/rally (selling the bounce in a downtrend); "range" blocks both.
	// Symmetric four-quadrant policy (audit 09-13). (CODE ENFORCED)
	EntryTimingGate bool `json:"entry_timing_gate"`
	// Risk-based position sizing: position value is capped at
	// equity × RiskPerTradePct% ÷ stop-distance% (defaults to 1.5% when
	// unset). Sizing derives FROM the stop, not the other way round. (CODE ENFORCED)
	RiskPerTradePct float64 `json:"risk_per_trade_pct"`
	// Stop-distance floor: reject entries whose stop is closer than
	// SLMinATRMult × ATR(1h) — inside normal noise. 0 disables. (CODE ENFORCED)
	SLMinATRMult float64 `json:"sl_min_atr_mult"`
	// Limit entry state machine: the AI may emit open_long_limit /
	// open_short_limit with price = trigger level; the system places a LIMIT
	// order cancels it after max(30min, LimitEntryMaxCycles × scan interval)
	// unfilled — time-based, since cycle length varies with the scan
	// interval. (CODE ENFORCED)
	LimitEntryEnabled   bool `json:"limit_entry_enabled"`
	LimitEntryMaxCycles int  `json:"limit_entry_max_cycles"`
	// LimitEntryOffsetPct is the pre-computed limit-entry anchor offset from
	// the snapshot live price, in percent: buy limit below / sell limit above.
	// Default 0.5 when unset; used as the fixed-mode value AND as the
	// fallback when ATR is unavailable. In the default "atr" mode the offset
	// is clamped to [LimitEntryOffsetMinPct, LimitEntryOffsetMaxPct]
	// (defaults 0.15%-1.2%, kernel/anchor_offset.go). (CODE ENFORCED)
	LimitEntryOffsetPct float64 `json:"limit_entry_offset_pct"`
	// LimitEntryOffsetMode selects how the anchor offset scales:
	// "atr" (default when empty) = OffsetATRMult × ATR(execution TF) clamped
	// to [OffsetMinPct, OffsetMaxPct] — a fixed percent is either most of a
	// quiet symbol's ATR or a hair off the live price for violent movers;
	// "fixed" = LimitEntryOffsetPct as-is. The same scaling drives the
	// anchor-vs-structure breathing-room threshold (suppression pre-filter
	// and the execution supply-zone gate share it). (CODE ENFORCED)
	LimitEntryOffsetMode    string  `json:"limit_entry_offset_mode,omitempty"`
	LimitEntryOffsetATRMult float64 `json:"limit_entry_offset_atr_mult"`
	LimitEntryOffsetMinPct  float64 `json:"limit_entry_offset_min_pct"`
	LimitEntryOffsetMaxPct  float64 `json:"limit_entry_offset_max_pct"`
	// LimitEntryMarketFallback: the anchor is computed at prompt-build time,
	// but the AI latency window (minutes) lets the live price cross it. A long
	// anchor at/above the live price (short anchor at/below) means the planned
	// pullback/rally already arrived — the resting order would fill
	// immediately at a price at least as good as the anchor — so the entry is
	// converted to MARKET with every risk gate re-validated at the live price
	// (and rejected outright when the price is already at/beyond the SL).
	// Default on (nil = enabled); set false to keep rejecting crossed
	// anchors. (CODE ENFORCED)
	LimitEntryMarketFallback *bool `json:"limit_entry_market_fallback,omitempty"`
	// Volatility-targeted position sizing, recomputed every cycle with an
	// 80/120 hysteresis band (reduce-only automation; adds stay AI-driven).
	VolTargetEnabled bool `json:"vol_target_enabled"`
	// Rule-based trailing stop: arms at 1.5× initial stop distance, trails at
	// 2×ATR(1h), monotonic tighten-only. (CODE ENFORCED)
	TrailingStopEnabled bool `json:"trailing_stop_enabled"`
	// Resistance-breakout hold: block an AI-initiated close of a LOSING
	// position when the nearest opposite-side structure level (overhead
	// resistance for longs / support below for shorts) is within
	// CloseRejectBreakoutPct% and the near-TF (15m) structure is NOT yet
	// broken — give the breakout/breakdown room instead of exiting on
	// "resistance rejection". 0 disables. (CODE ENFORCED)
	CloseRejectBreakoutPct float64 `json:"close_reject_breakout_pct"`
	// Supply-zone entry block: reject an open_long_limit / open_short_limit
	// whose anchor sits within OpenRejectSupplyPct% of the opposite-side
	// structure (below resistance for longs / above support for shorts) —
	// the fill would land inside the supply/demand zone with no room.
	// 0 disables. (CODE ENFORCED)
	OpenRejectSupplyPct float64 `json:"open_reject_supply_pct"`
	// Extended-pump long guard (暴涨延伸做多确认门): when a coin's 4h
	// trend-window return is at/above this percent, the EMA "up" label lags
	// the round-trip by days — longs then need a CONFIRMED pullback (15m
	// close back above its EMA20 AND a higher 15m swing low), otherwise the
	// long direction is blocked (EXTENDED_PUMP_UNCONFIRMED). Vertical pumps
	// bought as "trend pullbacks" round-trip hard (FILUSDT/SAGAUSDT
	// 2026-09-19: 4h +29%, −5.2%/−3.6% stop-outs within hours).
	// 0 = built-in default (20), negative = guard disabled. (CODE ENFORCED)
	PumpGuard4hPct float64 `json:"pump_guard_4h_pct"`
	// Loss-streak circuit breaker: when a symbol closes LossStreakMaxLosses
	// consecutive losing trades within the last 24h, new opens on that symbol
	// are blocked for 24h from the third loss (recomputed statelessly from
	// the closed-trade record, restart-safe). (CODE ENFORCED)
	LossStreakBanEnabled bool `json:"loss_streak_ban_enabled"`
	LossStreakMaxLosses  int  `json:"loss_streak_max_losses"`
	// Drawdown protection close: when a position's peak profit (margin
	// basis) reaches PeakDrawdownMinProfitPct% and then gives back
	// PeakDrawdownMaxDrawdownPct% of that peak, the program closes it to
	// lock the remainder. Defaults 5 / 55 when unset. (CODE ENFORCED)
	PeakDrawdownMinProfitPct float64 `json:"peak_drawdown_min_profit_pct"`
	PeakDrawdownMaxDDPct     float64 `json:"peak_drawdown_max_dd_pct"`
	// Account-level circuit breaker: when account equity sits
	// AccountMaxDrawdownPct% below the initial balance, ALL new opens are
	// blocked (closes/reduces still allowed) — the per-symbol loss-streak
	// breaker cannot catch a negative-edge strategy bleeding across many
	// symbols. 0 disables. (CODE ENFORCED)
	AccountMaxDrawdownPct float64 `json:"account_max_drawdown_pct"`
	// MaxAccountRiskPct caps TOTAL open stop-risk: Σ over positions of
	// qty×|entry−SL| (unprotected positions worst-cased at the 8% stop-band
	// cap) plus the new trade's risk ≤ pct×equity. Position COUNT caps say
	// nothing about correlation — five same-direction altcoin stops are one
	// big position; this is the cap that bounds a full-load stop-out.
	// 0 = default 10, negative = disabled. (CODE ENFORCED, QUANT_REVIEW
	// 2026-09-22 D2)
	MaxAccountRiskPct float64 `json:"max_account_risk_pct"`
	// DailyMaxLossPct halts new opens for the rest of the UTC day once equity
	// is down pct% from that day's first-seen equity. Closes/SL/TP unaffected.
	// 0 = default 10, negative = disabled. (CODE ENFORCED, QUANT_REVIEW
	// 2026-09-22 D2 — restores the lost daily-halt design; dailyPnL existed
	// but nothing consumed it)
	DailyMaxLossPct float64 `json:"daily_max_loss_pct"`
}

// EffectiveMaxAccountRiskPct resolves the account risk-exposure cap:
// 0/unset → DefaultMaxAccountRiskPct, negative → disabled (0).
func (r RiskControlConfig) EffectiveMaxAccountRiskPct() float64 {
	if r.MaxAccountRiskPct < 0 {
		return 0
	}
	if r.MaxAccountRiskPct == 0 {
		return DefaultMaxAccountRiskPct
	}
	return r.MaxAccountRiskPct
}

// EffectiveDailyMaxLossPct resolves the daily-loss halt threshold:
// 0/unset → DefaultDailyMaxLossPct, negative → disabled (0).
func (r RiskControlConfig) EffectiveDailyMaxLossPct() float64 {
	if r.DailyMaxLossPct < 0 {
		return 0
	}
	if r.DailyMaxLossPct == 0 {
		return DefaultDailyMaxLossPct
	}
	return r.DailyMaxLossPct
}

// NewStrategyStore creates a new StrategyStore
func NewStrategyStore(db *gorm.DB) *StrategyStore {
	return &StrategyStore{db: db}
}

// DefaultStatsWindowDays is the rolling stats window when the strategy
// config leaves stats_window_days unset.
const DefaultStatsWindowDays = 30

// DefaultUSStockSessionBoostPct backs EffectiveUSStockSessionBoostPct.
const DefaultUSStockSessionBoostPct = 20.0

// EffectiveUSStockSessionBoostPct resolves the US-session equity weighting:
// 0/unset → DefaultUSStockSessionBoostPct, negative → disabled (0).
func (c *StrategyConfig) EffectiveUSStockSessionBoostPct() float64 {
	if c.USStockSessionBoostPct < 0 {
		return 0
	}
	if c.USStockSessionBoostPct == 0 {
		return DefaultUSStockSessionBoostPct
	}
	return c.USStockSessionBoostPct
}

// DefaultMaxAccountRiskPct / DefaultDailyMaxLossPct back the effective
// helpers above — rendered into the prompt from these constants so the text
// can never drift from the enforced value.
const (
	DefaultMaxAccountRiskPct = 10.0
	DefaultDailyMaxLossPct   = 10.0
)

// EffectiveStatsWindowDays resolves the stats window in days: 0 means
// "full history" (explicitly negative config), otherwise the window length
// (DefaultStatsWindowDays when unset/0).
func (c *StrategyConfig) EffectiveStatsWindowDays() int {
	switch {
	case c.StatsWindowDays < 0:
		return 0
	case c.StatsWindowDays == 0:
		return DefaultStatsWindowDays
	default:
		return c.StatsWindowDays
	}
}

func (s *StrategyStore) initTables() error {
	// AutoMigrate will add missing columns without dropping existing data
	return s.db.AutoMigrate(&Strategy{})
}

func (s *StrategyStore) initDefaultData() error {
	// No longer pre-populate strategies - create on demand when user configures
	return nil
}

// GetDefaultStrategyConfig returns the default strategy configuration for the given language
func GetDefaultStrategyConfig(lang string) StrategyConfig {
	// Normalize language to "zh" or "en"
	normalizedLang := "en"
	if lang == "zh" {
		normalizedLang = "zh"
	}

	config := StrategyConfig{
		Language: normalizedLang,
		CoinSource: CoinSourceConfig{
			SourceType: "ai500",
			UseAI500:   true,
			AI500Limit: 3,
			UseOITop:   false,
			OITopLimit: 3,
			UseOILow:   false,
			OILowLimit: 3,
		},
		Indicators: IndicatorConfig{
			Klines: KlineConfig{
				PrimaryTimeframe:     "5m",
				PrimaryCount:         20,
				LongerTimeframe:      "4h",
				LongerCount:          10,
				EnableMultiTimeframe: true,
				SelectedTimeframes:   []string{"5m", "15m", "1h"},
			},
			EnableRawKlines:   true, // Required - raw OHLCV data for AI analysis
			EnableEMA:         false,
			EnableMACD:        false,
			EnableRSI:         false,
			EnableATR:         false,
			EnableBOLL:        false,
			EnableVolume:      true,
			EnableOI:          true,
			EnableFundingRate: true,
			EMAPeriods:        []int{20, 50},
			RSIPeriods:        []int{7, 14},
			ATRPeriods:        []int{14},
			BOLLPeriods:       []int{20},
			// NofxOS unified API key
			NofxOSAPIKey: "cm_568c67eae410d912c54c",
			// Quant data
			EnableQuantData:    true,
			EnableQuantOI:      true,
			EnableQuantNetflow: true,
			// OI ranking data
			EnableOIRanking:   true,
			OIRankingDuration: "1h",
			OIRankingLimit:    10,
			// NetFlow ranking data
			EnableNetFlowRanking:   true,
			NetFlowRankingDuration: "1h",
			NetFlowRankingLimit:    10,
			// Price ranking data
			EnablePriceRanking:   true,
			PriceRankingDuration: "1h,4h,24h",
			PriceRankingLimit:    10,
		},
		RiskControl: RiskControlConfig{
			MaxPositions:                 3,   // Max 3 coins simultaneously (CODE ENFORCED)
			BTCETHMaxLeverage:            5,   // BTC/ETH exchange leverage (AI guided)
			AltcoinMaxLeverage:           5,   // Altcoin exchange leverage (AI guided)
			BTCETHMaxPositionValueRatio:  5.0, // BTC/ETH: max position = 5x equity (CODE ENFORCED)
			AltcoinMaxPositionValueRatio: 1.0, // Altcoin: max position = 1x equity (CODE ENFORCED)
			MaxMarginUsage:               0.9, // Max 90% margin usage (CODE ENFORCED)
			MinPositionSize:              12,  // Min 12 USDT per position (CODE ENFORCED)
			MinRiskRewardRatio:           3.0, // Min 3:1 profit/loss ratio (CODE ENFORCED at open — struct field comment is the source of truth)
			MinConfidence:                75,  // Min 75% confidence (AI guided)

			MaxAccountRiskPct: 10.0, // Σ open stop-risk + new risk ≤ 10% equity (CODE ENFORCED)
			DailyMaxLossPct:   10.0, // Daily-loss halt: opens blocked at −10% from day-start equity (CODE ENFORCED)
		},
	}

	if lang == "zh" {
		config.PromptSections = PromptSectionsConfig{
			RoleDefinition: `# 你是一个专业的加密货币交易AI

你的任务是根据提供的市场数据做出交易决策。你是一个经验丰富的量化交易员，擅长技术分析和风险管理。`,
			TradingFrequency: `# ⏱️ 交易频率意识

- 优秀交易员：每天2-4笔 ≈ 每小时0.1-0.2笔
- 每小时超过2笔 = 过度交易
- 单笔持仓时间 ≥ 30-60分钟
如果你发现自己每个周期都在交易 → 标准太低；如果持仓不到30分钟就平仓 → 太冲动。`,
			EntryStandards: `# 🎯 入场标准（严格）

只在多个信号共振时入场。自由使用任何有效的分析方法，避免单一指标、信号矛盾、横盘震荡、或平仓后立即重新开仓等低质量行为。`,
			DecisionProcess: `# 📋 决策流程

1. 检查持仓 → 是否止盈/止损
2. 扫描候选币种 + 多时间框架 → 是否存在强信号
3. 先写思维链，再输出结构化JSON`,
		}
	} else {
		config.PromptSections = PromptSectionsConfig{
			RoleDefinition: `# You are a professional cryptocurrency trading AI

Your task is to make trading decisions based on the provided market data. You are an experienced quantitative trader skilled in technical analysis and risk management.`,
			TradingFrequency: `# ⏱️ Trading Frequency Awareness

- Excellent trader: 2-4 trades per day ≈ 0.1-0.2 trades per hour
- >2 trades per hour = overtrading
- Single position holding time ≥ 30-60 minutes
If you find yourself trading every cycle → standards are too low; if closing positions in <30 minutes → too impulsive.`,
			EntryStandards: `# 🎯 Entry Standards (Strict)

Only enter positions when multiple signals resonate. Freely use any effective analysis methods, avoid low-quality behaviors such as single indicators, contradictory signals, sideways oscillation, or immediately restarting after closing positions.`,
			DecisionProcess: `# 📋 Decision Process

1. Check positions → whether to take profit/stop loss
2. Scan candidate coins + multi-timeframe → whether strong signals exist
3. Write chain of thought first, then output structured JSON`,
		}
	}

	return config
}

// Create create a strategy
func (s *StrategyStore) Create(strategy *Strategy) error {
	return s.db.Create(strategy).Error
}

// Update update a strategy
func (s *StrategyStore) Update(strategy *Strategy) error {
	return s.db.Model(&Strategy{}).
		Where("id = ? AND user_id = ?", strategy.ID, strategy.UserID).
		Updates(map[string]interface{}{
			"name":           strategy.Name,
			"description":    strategy.Description,
			"config":         strategy.Config,
			"is_public":      strategy.IsPublic,
			"config_visible": strategy.ConfigVisible,
			"updated_at":     time.Now().UTC(),
		}).Error
}

// Delete delete a strategy
func (s *StrategyStore) Delete(userID, id string) error {
	// do not allow deleting system default strategy
	var st Strategy
	if err := s.db.Where("id = ?", id).First(&st).Error; err == nil {
		if st.IsDefault {
			return fmt.Errorf("cannot delete system default strategy")
		}
	}

	// Check if any trader references this strategy
	var count int64
	if err := s.db.Model(&Trader{}).
		Where("user_id = ? AND strategy_id = ?", userID, id).
		Count(&count).Error; err == nil && count > 0 {
		return fmt.Errorf("cannot delete strategy in use by %d trader(s) - reassign those traders first", count)
	}

	return s.db.Where("id = ? AND user_id = ?", id, userID).Delete(&Strategy{}).Error
}

// List get user's strategy list
func (s *StrategyStore) List(userID string) ([]*Strategy, error) {
	var strategies []*Strategy
	err := s.db.Where("user_id = ? OR is_default = ?", userID, true).
		Order("is_default DESC, created_at DESC").
		Find(&strategies).Error
	if err != nil {
		return nil, err
	}
	return strategies, nil
}

// ListPublic get all public strategies for the strategy market
func (s *StrategyStore) ListPublic() ([]*Strategy, error) {
	var strategies []*Strategy
	err := s.db.Where("is_public = ?", true).
		Order("created_at DESC").
		Find(&strategies).Error
	if err != nil {
		return nil, err
	}
	return strategies, nil
}

// Get get a single strategy
func (s *StrategyStore) Get(userID, id string) (*Strategy, error) {
	var st Strategy
	err := s.db.Where("id = ? AND (user_id = ? OR is_default = ?)", id, userID, true).
		First(&st).Error
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// GetActive get user's currently active strategy
func (s *StrategyStore) GetActive(userID string) (*Strategy, error) {
	var st Strategy
	err := s.db.Where("user_id = ? AND is_active = ?", userID, true).First(&st).Error
	if err == gorm.ErrRecordNotFound {
		// no active strategy, return system default strategy
		return s.GetDefault()
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// GetDefault get system default strategy
func (s *StrategyStore) GetDefault() (*Strategy, error) {
	var st Strategy
	err := s.db.Where("is_default = ?", true).First(&st).Error
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// SetActive set active strategy (will first deactivate other strategies)
func (s *StrategyStore) SetActive(userID, strategyID string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		// first deactivate all strategies for the user
		if err := tx.Model(&Strategy{}).Where("user_id = ?", userID).
			Update("is_active", false).Error; err != nil {
			return err
		}

		// activate specified strategy
		return tx.Model(&Strategy{}).
			Where("id = ? AND (user_id = ? OR is_default = ?)", strategyID, userID, true).
			Update("is_active", true).Error
	})
}

// Duplicate duplicate a strategy (used to create custom strategy based on default strategy)
func (s *StrategyStore) Duplicate(userID, sourceID, newID, newName string) error {
	// get source strategy
	source, err := s.Get(userID, sourceID)
	if err != nil {
		return fmt.Errorf("failed to get source strategy: %w", err)
	}

	// create new strategy
	newStrategy := &Strategy{
		ID:          newID,
		UserID:      userID,
		Name:        newName,
		Description: "Created based on [" + source.Name + "]",
		IsActive:    false,
		IsDefault:   false,
		Config:      source.Config,
	}

	return s.Create(newStrategy)
}

// ParseConfig parse strategy configuration JSON
func (s *Strategy) ParseConfig() (*StrategyConfig, error) {
	var config StrategyConfig
	if err := json.Unmarshal([]byte(s.Config), &config); err != nil {
		return nil, fmt.Errorf("failed to parse strategy configuration: %w", err)
	}
	return &config, nil
}

// SetConfig set strategy configuration
func (s *Strategy) SetConfig(config *StrategyConfig) error {
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to serialize strategy configuration: %w", err)
	}
	s.Config = string(data)
	return nil
}

// ============================================================================
// Token Estimation
// ============================================================================

// TokenEstimate holds the result of token estimation
type TokenEstimate struct {
	Total       int            `json:"total"`
	Breakdown   TokenBreakdown `json:"breakdown"`
	ModelLimits []ModelLimit   `json:"model_limits"`
	Suggestions []string       `json:"suggestions"`
}

// TokenBreakdown shows estimated tokens per component
type TokenBreakdown struct {
	SystemPrompt  int `json:"system_prompt"`
	MarketData    int `json:"market_data"`
	RankingData   int `json:"ranking_data"`
	QuantData     int `json:"quant_data"`
	FixedOverhead int `json:"fixed_overhead"`
}

// ModelLimit shows token usage against a specific model's context limit
type ModelLimit struct {
	Name         string `json:"name"`
	ContextLimit int    `json:"context_limit"`
	UsagePct     int    `json:"usage_pct"`
	Level        string `json:"level"` // "ok" | "warning" | "danger"
}

// Context window sizes (tokens) for each model family
const (
	contextLimitDeepSeek = 131_072   // 128K
	contextLimitOpenAI   = 128_000   // 128K
	contextLimitClaude   = 200_000   // 200K
	contextLimitQwen     = 131_072   // 128K
	contextLimitGemini   = 1_000_000 // 1M
	contextLimitGrok     = 131_072   // 128K
	contextLimitKimi     = 131_072   // 128K
	contextLimitMinimax  = 1_000_000 // 1M
	contextLimitGLM      = 131_072   // 128K
)

// ModelContextLimits maps provider names to their context window sizes (in tokens)
var ModelContextLimits = map[string]int{
	"deepseek": contextLimitDeepSeek,
	"openai":   contextLimitOpenAI,
	"claude":   contextLimitClaude,
	"qwen":     contextLimitQwen,
	"gemini":   contextLimitGemini,
	"grok":     contextLimitGrok,
	"kimi":     contextLimitKimi,
	"minimax":  contextLimitMinimax,
	"glm":      contextLimitGLM,
}

// GetContextLimit returns the context limit for a given provider
func GetContextLimit(provider string) int {
	if limit, ok := ModelContextLimits[provider]; ok {
		return limit
	}
	return contextLimitDeepSeek // safe default
}

// GetContextLimitForClient returns context limit for a provider+model pair.
func GetContextLimitForClient(provider, model string) int {
	return GetContextLimit(provider)
}

// EstimateTokens estimates the total token count for a strategy configuration.
// This is a pure computation based on config fields — no network calls.
func (c *StrategyConfig) EstimateTokens() TokenEstimate {
	breakdown := TokenBreakdown{}

	// --- System Prompt ---
	// Base system prompt: schema + role + rules + output format
	baseChars := 4000 // English default
	if c.Language == "zh" {
		baseChars = 3000
	}
	// Add prompt sections
	baseChars += len(c.PromptSections.RoleDefinition)
	baseChars += len(c.PromptSections.TradingFrequency)
	baseChars += len(c.PromptSections.EntryStandards)
	baseChars += len(c.PromptSections.DecisionProcess)
	baseChars += len(c.CustomPrompt)

	if c.Language == "zh" {
		breakdown.SystemPrompt = baseChars / 2 // CJK: ~2 chars per token
	} else {
		breakdown.SystemPrompt = baseChars / 4 // English: ~4 chars per token
	}

	// --- Fixed Overhead ---
	// Time, BTC price, account info, section headers
	breakdown.FixedOverhead = 800 / 4 // ~200 tokens

	// --- Market Data ---
	numCoins := c.getEffectiveCoinCount()
	numTimeframes := c.getEffectiveTimeframeCount()
	klineCount := c.Indicators.Klines.PrimaryCount
	if klineCount <= 0 {
		klineCount = 20
	}

	// Per coin per timeframe: the structured signal renders DERIVED fields
	// (trend/indicators/levels/quality), not raw klines — cost is constant
	// per TF regardless of kline count.
	charsPerCoinTF := 450

	// Add enabled indicator overhead per timeframe
	indicatorCharsPerLine := 0
	if c.Indicators.EnableEMA {
		indicatorCharsPerLine += 20 // EMA values appended
	}
	if c.Indicators.EnableMACD {
		indicatorCharsPerLine += 30
	}
	if c.Indicators.EnableRSI {
		indicatorCharsPerLine += 15
	}
	if c.Indicators.EnableATR {
		indicatorCharsPerLine += 15
	}
	if c.Indicators.EnableBOLL {
		indicatorCharsPerLine += 25
	}
	if c.Indicators.EnableVolume {
		indicatorCharsPerLine += 10
	}
	_ = klineCount // indicator detail is folded into the 450-char structured block

	totalMarketChars := numCoins * numTimeframes * charsPerCoinTF

	// OI + Funding per coin
	if c.Indicators.EnableOI || c.Indicators.EnableFundingRate {
		totalMarketChars += numCoins * 100
	}

	breakdown.MarketData = totalMarketChars / 4 // numeric data: ~4 chars per token

	// --- Quant Data ---
	if c.Indicators.EnableQuantData {
		quantCharsPerCoin := 0
		if c.Indicators.EnableQuantOI {
			quantCharsPerCoin += 300
		}
		if c.Indicators.EnableQuantNetflow {
			quantCharsPerCoin += 300
		}
		breakdown.QuantData = (numCoins * quantCharsPerCoin) / 4
	}

	// --- Ranking Data ---
	rankingChars := 0
	if c.Indicators.EnableOIRanking {
		limit := c.Indicators.OIRankingLimit
		if limit <= 0 {
			limit = 10
		}
		rankingChars += limit * 60
	}
	if c.Indicators.EnableNetFlowRanking {
		limit := c.Indicators.NetFlowRankingLimit
		if limit <= 0 {
			limit = 10
		}
		rankingChars += limit * 80
	}
	if c.Indicators.EnablePriceRanking {
		limit := c.Indicators.PriceRankingLimit
		if limit <= 0 {
			limit = 10
		}
		// Count durations (comma-separated)
		numDurations := 1
		if c.Indicators.PriceRankingDuration != "" {
			numDurations = len(strings.Split(c.Indicators.PriceRankingDuration, ","))
		}
		rankingChars += limit * numDurations * 40
	}
	breakdown.RankingData = rankingChars / 4

	// --- Total with 15% safety margin ---
	subtotal := breakdown.SystemPrompt + breakdown.MarketData + breakdown.RankingData + breakdown.QuantData + breakdown.FixedOverhead
	total := subtotal * 115 / 100

	// --- Model limits ---
	modelLimits := make([]ModelLimit, 0, len(ModelContextLimits))
	for name, limit := range ModelContextLimits {
		pct := total * 100 / limit
		level := "ok"
		if pct >= 100 {
			level = "danger"
		} else if pct >= 80 {
			level = "warning"
		}
		modelLimits = append(modelLimits, ModelLimit{
			Name:         name,
			ContextLimit: limit,
			UsagePct:     pct,
			Level:        level,
		})
	}

	// Sort by usage_pct desc, then name asc for deterministic order
	sort.Slice(modelLimits, func(i, j int) bool {
		if modelLimits[i].UsagePct != modelLimits[j].UsagePct {
			return modelLimits[i].UsagePct > modelLimits[j].UsagePct
		}
		return modelLimits[i].Name < modelLimits[j].Name
	})

	// --- Suggestions ---
	var suggestions []string
	// Find the strictest model (smallest context)
	minLimit := 0
	for _, limit := range ModelContextLimits {
		if minLimit == 0 || limit < minLimit {
			minLimit = limit
		}
	}
	if minLimit > 0 && total > minLimit {
		if numTimeframes > 1 {
			savedPerTF := (numCoins * klineCount * (80 + indicatorCharsPerLine)) / 4 * 115 / 100
			suggestions = append(suggestions, fmt.Sprintf("Reduce 1 timeframe to save ~%d tokens", savedPerTF))
		}
		if numCoins > 1 {
			savedPerCoin := (numTimeframes * klineCount * (80 + indicatorCharsPerLine)) / 4 * 115 / 100
			suggestions = append(suggestions, fmt.Sprintf("Reduce 1 coin to save ~%d tokens", savedPerCoin))
		}
		if klineCount > 15 {
			suggestions = append(suggestions, "Reduce K-line count to 15 to save tokens")
		}
	}

	return TokenEstimate{
		Total:       total,
		Breakdown:   breakdown,
		ModelLimits: modelLimits,
		Suggestions: suggestions,
	}
}

// getEffectiveCoinCount returns the estimated number of coins that will be analyzed
func (c *StrategyConfig) getEffectiveCoinCount() int {
	count := 0
	switch c.CoinSource.SourceType {
	case "static":
		count = len(c.CoinSource.StaticCoins)
	case "ai500":
		count = c.CoinSource.AI500Limit
	case "oi_top":
		count = c.CoinSource.OITopLimit
	case "oi_low":
		count = c.CoinSource.OILowLimit
	case "piggy_dash":
		count = c.CoinSource.PiggyDashLimit
	case "short_scan":
		count = c.CoinSource.ShortScanLimit
	case "mixed":
		if c.CoinSource.UseAI500 {
			count += c.CoinSource.AI500Limit
		}
		if c.CoinSource.UseOITop {
			count += c.CoinSource.OITopLimit
		}
		if c.CoinSource.UseOILow {
			count += c.CoinSource.OILowLimit
		}
		if c.CoinSource.UsePiggyDash {
			count += c.CoinSource.PiggyDashLimit
		}
		if c.CoinSource.UseShortScan {
			count += c.CoinSource.ShortScanLimit
		}
		if c.CoinSource.UseStatic == nil || *c.CoinSource.UseStatic {
			count += len(c.CoinSource.StaticCoins)
		}
	default:
		count = c.CoinSource.AI500Limit
	}
	if count <= 0 {
		count = 3
	}
	return count
}

// getEffectiveTimeframeCount returns the number of timeframes that will be used
func (c *StrategyConfig) getEffectiveTimeframeCount() int {
	if len(c.Indicators.Klines.SelectedTimeframes) > 0 {
		return len(c.Indicators.Klines.SelectedTimeframes)
	}
	count := 1
	if c.Indicators.Klines.LongerTimeframe != "" {
		count++
	}
	return count
}
