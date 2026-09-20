// Strategy Studio Types
export interface Strategy {
  id: string;
  name: string;
  description: string;
  is_active: boolean;
  is_default: boolean;
  is_public: boolean;           // 是否在策略市场公开
  config_visible: boolean;      // 配置参数是否公开可见
  config: StrategyConfig;
  created_at: string;
  updated_at: string;
}

// 策略使用统计
export interface StrategyStats {
  clone_count: number;          // 被克隆次数
  active_users: number;         // 当前使用人数
  top_performers?: StrategyPerformer[];  // 收益排行
}

// 策略使用者收益排行
export interface StrategyPerformer {
  user_id: string;
  user_name: string;            // 脱敏后的用户名
  total_pnl_pct: number;        // 总收益率
  total_pnl: number;            // 总收益金额
  win_rate: number;             // 胜率
  trade_count: number;          // 交易次数
  using_since: string;          // 使用开始时间
  rank: number;                 // 排名
}

export interface PromptSectionsConfig {
  role_definition?: string;
  trading_frequency?: string;
  entry_standards?: string;
  decision_process?: string;
}

export interface StrategyConfig {
  // Strategy type: "ai_trading" (default) or "grid_trading"
  strategy_type?: 'ai_trading' | 'grid_trading';
  // Language setting: "zh" for Chinese, "en" for English
  // Determines the language used for data formatting and prompt generation
  language?: 'zh' | 'en';
  coin_source: CoinSourceConfig;
  indicators: IndicatorConfig;
  custom_prompt?: string;
  // Rolling window (days) for the trading stats the AI sees (PF/win-rate).
  // 0/absent = 30 days default, negative = full history.
  stats_window_days?: number;
  risk_control: RiskControlConfig;
  prompt_sections?: PromptSectionsConfig;
  // Grid trading configuration (only used when strategy_type is 'grid_trading')
  grid_config?: GridStrategyConfig;
}

// Grid trading specific configuration
export interface GridStrategyConfig {
  // Trading pair (e.g., "BTCUSDT")
  symbol: string;
  // Number of grid levels (5-50)
  grid_count: number;
  // Total investment in USDT
  total_investment: number;
  // Leverage (1-20)
  leverage: number;
  // Upper price boundary (0 = auto-calculate from ATR)
  upper_price: number;
  // Lower price boundary (0 = auto-calculate from ATR)
  lower_price: number;
  // Use ATR to auto-calculate bounds
  use_atr_bounds: boolean;
  // ATR multiplier for bound calculation (default 2.0)
  atr_multiplier: number;
  // Position distribution: "uniform" | "gaussian" | "pyramid"
  distribution: 'uniform' | 'gaussian' | 'pyramid';
  // Maximum drawdown percentage before emergency exit
  max_drawdown_pct: number;
  // Stop loss percentage per position
  stop_loss_pct: number;
  // Daily loss limit percentage
  daily_loss_limit_pct: number;
  // Use maker-only orders for lower fees
  use_maker_only: boolean;
  // Enable automatic grid direction adjustment based on box breakouts
  enable_direction_adjust?: boolean;
  // Direction bias ratio for long_bias/short_bias modes (default 0.7 = 70%/30%)
  direction_bias_ratio?: number;
}

export interface CoinSourceConfig {
  source_type: 'static' | 'ai500' | 'oi_top' | 'oi_low' | 'piggy_dash' | 'short_scan' | 'mixed';
  static_coins?: string[];
  excluded_coins?: string[];   // 排除的币种列表
  // mixed mode: include the static coin list as one of the sources
  // (legacy configs without this flag always included it)
  use_static?: boolean;
  use_ai500: boolean;
  ai500_limit?: number;
  use_oi_top: boolean;
  oi_top_limit?: number;
  use_oi_low: boolean;
  oi_low_limit?: number;
  use_piggy_dash: boolean;
  piggy_dash_limit?: number;
  piggy_dash_direction?: string; // '', 'breakout', 'breakdown'
  // 做空扫描: top 24h gainers ranked by short-suitability score (Binance-derived)
  use_short_scan: boolean;
  short_scan_limit?: number;
  // 做空扫描资金费率拥挤阈值（%，8h 费率）；0/缺省 = 默认 0.03
  short_scan_funding_rate_pct?: number;
  // 历史涨幅池窗口（天）：近 N 天每日涨幅 Top20 合并进做空扫描宇宙；0/缺省 = 默认 7，负数 = 关闭
  short_scan_history_days?: number;
  // 历史涨幅池每轮额外分析上限（个）；0/缺省 = 默认 30
  short_scan_history_max?: number;
  // 候选币最低 OI 持仓价值（百万 USD），低于则跳过；0/缺省 = 默认 15M
  min_oi_value_millions?: number;
  // Note: API URLs are now built automatically using nofxos_api_key from IndicatorConfig
}

export interface IndicatorConfig {
  klines: KlineConfig;
  // Raw OHLCV kline data - required for AI analysis
  enable_raw_klines: boolean;
  // Technical indicators (optional)
  enable_ema: boolean;
  enable_macd: boolean;
  enable_rsi: boolean;
  enable_atr: boolean;
  enable_boll: boolean;
  enable_volume: boolean;
  enable_oi: boolean;
  enable_funding_rate: boolean;
  ema_periods?: number[];
  rsi_periods?: number[];
  atr_periods?: number[];
  boll_periods?: number[];
  external_data_sources?: ExternalDataSource[];

  // ========== NofxOS 数据源统一配置 ==========
  // Unified NofxOS API Key - used for all NofxOS data sources
  nofxos_api_key?: string;

  // 量化数据源（资金流向、持仓变化、价格变化）
  enable_quant_data?: boolean;
  enable_quant_oi?: boolean;
  enable_quant_netflow?: boolean;

  // OI 排行数据（市场持仓量增减排行）
  enable_oi_ranking?: boolean;
  oi_ranking_duration?: string;  // "1h", "4h", "24h"
  oi_ranking_limit?: number;

  // NetFlow 排行数据（机构/散户资金流向排行）
  enable_netflow_ranking?: boolean;
  netflow_ranking_duration?: string;  // "1h", "4h", "24h"
  netflow_ranking_limit?: number;

  // Price 排行数据（涨跌幅排行）
  enable_price_ranking?: boolean;
  price_ranking_duration?: string;  // "1h", "4h", "24h" or "1h,4h,24h"
  price_ranking_limit?: number;
}

export interface KlineConfig {
  primary_timeframe: string;
  primary_count: number;
  longer_timeframe?: string;
  longer_count?: number;
  enable_multi_timeframe: boolean;
  // 新增：支持选择多个时间周期
  selected_timeframes?: string[];
}

export interface ExternalDataSource {
  name: string;
  type: 'api' | 'webhook';
  url: string;
  method: string;
  headers?: Record<string, string>;
  data_path?: string;
  refresh_secs?: number;
}

export interface RiskControlConfig {
  // Max number of coins held simultaneously (CODE ENFORCED)
  max_positions: number;

  // Trading Leverage - exchange leverage for opening positions (AI guided)
  btc_eth_max_leverage: number;    // BTC/ETH max exchange leverage
  altcoin_max_leverage: number;    // Altcoin max exchange leverage

  // Position Value Ratio - single position notional value / account equity (CODE ENFORCED)
  // Max position value = equity × this ratio
  btc_eth_max_position_value_ratio?: number;     // default: 5 (BTC/ETH max position = 5x equity)
  altcoin_max_position_value_ratio?: number;     // default: 1 (Altcoin max position = 1x equity)

  // Risk Parameters
  max_margin_usage: number;        // Max margin utilization, e.g. 0.9 = 90% (CODE ENFORCED)
  min_position_size: number;       // Min position size in USDT (CODE ENFORCED)
  min_risk_reward_ratio: number;   // Min take_profit / stop_loss ratio (CODE ENFORCED at open)
  min_confidence: number;          // Min AI confidence to open position (AI guided)

  // Min holding period in minutes before AI-initiated closes are allowed (CODE ENFORCED)
  min_hold_minutes?: number;
  // AI closes before this many hours need ≥2 against-direction closed 1h candles (0 = default 4h, negative = disabled) (CODE ENFORCED)
  early_close_min_hours?: number;
  // 暴涨延伸做多确认门：4h 趋势窗口涨幅 ≥ N% 时做多需回踩确认；0 = 默认 20，负数 = 禁用
  pump_guard_4h_pct?: number;
  // TP ladder on leveraged PnL%: at ≥ this the program trims 1/3 (0 = default 10, negative = off) (CODE ENFORCED)
  tp_trim_profit_pct?: number;
  // At ≥ this PnL% the program closes the rest (0 = default 25, negative = off) (CODE ENFORCED)
  tp_full_profit_pct?: number;
  // Block open_short when the 1d trend is up (CODE ENFORCED)
  block_short_1d_uptrend?: boolean;
  // Entry timing gate: finest sub-hour TF trend must align with direction (CODE ENFORCED)
  entry_timing_gate?: boolean;
  // Position value capped at equity × this % ÷ stop-distance% (default 1.5)
  risk_per_trade_pct?: number;
  // Stop-distance noise floor: reject stops closer than N × ATR(1h) (0 = off)
  sl_min_atr_mult?: number;
  // Limit-entry state machine: AI can emit open_*_limit trigger orders (CODE ENFORCED)
  limit_entry_enabled?: boolean;
  // Cancel a limit entry after this many cycles unfilled (default 3)
  limit_entry_max_cycles?: number;
  // Limit-entry anchor offset from the snapshot price, % (buy below / sell above; default 0.5)
  limit_entry_offset_pct?: number;
  // Convert a limit entry to market when the live price already crossed the
  // anchor (pullback arrived); rejected if price is beyond the SL. Default on.
  limit_entry_market_fallback?: boolean;
  // Volatility-targeted sizing with 80/120 hysteresis band (CODE ENFORCED)
  vol_target_enabled?: boolean;
  // Rule-based trailing stop: arm at 1.5× stop distance, trail 2×ATR (CODE ENFORCED)
  trailing_stop_enabled?: boolean;
  // Drawdown-protect close: peak profit must reach this % before the
  // giveback guard arms (default 5)
  peak_drawdown_min_profit_pct?: number;
  // Giveback (of peak) that triggers the protective close (default 55)
  peak_drawdown_max_dd_pct?: number;
  // 1R profit lock: at N× the opening risk the SL moves to entry and 50%
  // is trimmed (0 = default 1R, negative = disabled). While ON it
  // supersedes the ROE trim tier (tp_trim_profit_pct).
  profit_lock_at_r?: number;
  // Hard-block opens when the vendor-vs-live price diverges beyond this %
  // (0 = default 1, negative = disabled) (CODE ENFORCED)
  max_vendor_divergence_pct?: number;
}
