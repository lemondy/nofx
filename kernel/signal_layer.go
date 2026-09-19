// Signal layer: converts raw per-timeframe market data into compact,
// normalized feature blocks for the AI prompt. The program owns everything
// mechanical — candle settlement, time alignment (UTC), missing-value
// handling, indicator math, outlier filtering — so the model reasons over
// clean features instead of raw candle dumps.
package kernel

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"nofx/market"
)

// TFSignal is the normalized feature block for one timeframe. Pointer fields
// are omitted from the prompt JSON when the underlying indicator is disabled
// or data is insufficient.
type TFSignal struct {
	Timeframe string `json:"timeframe"` // e.g. "5m", "1h"
	Trend     string `json:"trend"`     // up / down / pullback / range
	// Trend-window return: the change over the last `lookback` closed bars —
	// a WINDOW, not the bar timeframe (1h bars × 20 = 20h window). The window
	// span itself is in return_window_hours.
	ReturnPct         float64  `json:"trend_window_return_pct"`
	ReturnWindowHrs   float64  `json:"return_window_hours"`                  // real-world span of the trend window (lookback × bar duration)
	PrevHourChangePct *float64 `json:"prev_hour_close_change_pct,omitempty"` // last FULLY CLOSED hour vs the one before (closed data only)
	ATRPct            float64  `json:"atr_pct"`                              // ATR14 / price × 100
	ATRPercentile     *float64 `json:"atr_percentile,omitempty"`             // current ATR% rank within this TF's own ATR history (0-100)
	VolumeRatio       *float64 `json:"volume_ratio"`                         // last closed volume / 20-bar avg
	Volume            float64  `json:"volume"`                               // last CLOSED bar volume, base units (kline source native)
	VolumeChangePct   float64  `json:"volume_change_pct"`                    // last closed volume vs previous closed bar, %
	// Support/Resistance are ALWAYS emitted (possibly []) — an empty array is
	// meaningful ("no swing level remains on this side of the LIVE price;
	// price is making new highs/lows inside the window") and must stay
	// legible, not vanish from the JSON (09-18 audit #6: BTC 1h/4h showed no
	// resistance key at all while price was above every 4h structure high).
	Support           []float64 `json:"support"`               // nearest swing lows, ascending; [] = none below live price
	Resistance        []float64 `json:"resistance"`            // nearest swing highs, descending; [] = none above live price
	SupportDistPct    []float64 `json:"support_dist_pct"`      // pre-computed (level-anchor)/anchor×100, index-aligned with Support
	ResistanceDistPct []float64 `json:"resistance_dist_pct"`   // pre-computed, index-aligned with Resistance
	MACDHist          *float64  `json:"macd_hist,omitempty"`   // normalized by price
	MACDTrend         string    `json:"macd_trend,omitempty"`  // rising / falling / flat
	RSI14             *float64  `json:"rsi,omitempty"`         // 0-100
	StochRSIK         *float64  `json:"stoch_rsi_k,omitempty"` // StochRSI %K (14,14,3,3), 0-100
	StochRSID         *float64  `json:"stoch_rsi_d,omitempty"` // StochRSI %D (3-SMA of K), 0-100
	EMAFast           *float64  `json:"ema_fast,omitempty"`    // EMA20
	EMASlow           *float64  `json:"ema_slow,omitempty"`    // EMA50
	LastClosedCandle  string    `json:"last_closed_candle"`    // bullish / bearish / doji
	LastClose         float64   `json:"last_close"`            // last CLOSED candle close
	StructureHigh     *float64  `json:"structure_high"`        // highest close in window
	StructureLow      *float64  `json:"structure_low"`         // lowest close in window
	// Structure distances use the SAME convention as support_dist_pct /
	// resistance_dist_pct: (level − LIVE price)/live×100. NEGATIVE
	// structure_high_dist_pct means the live price has already cleared the
	// window high (price is printing new highs — the closed-candle structure
	// lags and offers no overhead reference). 09-18 audit #6: BTC 4h showed
	// structure_high 1.6% BELOW the live price with no marker at all.
	StructureHighDistPct *float64 `json:"structure_high_dist_pct,omitempty"`
	StructureLowDistPct  *float64 `json:"structure_low_dist_pct,omitempty"`
	BarsUsed             int      `json:"closed_bars_used"`
	UnclosedDropped      bool     `json:"unclosed_dropped"` // forming candle was dropped
}

// DerivSignal carries derivatives context.
type DerivSignal struct {
	FundingRate           *float64 `json:"funding_rate,omitempty"`
	FundingAnnualizedPct  *float64 `json:"funding_annualized_pct,omitempty"`    // 预计算: rate × (24/settle_hours) × 365 × 100,按实测结算间隔年化,模型直接使用不再自行年化
	FundingSettleHours    *float64 `json:"funding_settle_hours,omitempty"`      // 实测结算间隔(小时);缺失时年化按 8h 口径
	OIChange1hPct         *float64 `json:"oi_change_1h_pct,omitempty"`          // true 1h change (quant layer)
	OIVsAvgPct            *float64 `json:"oi_vs_avg_pct,omitempty"`             // current OI vs its period average
	PriceChange1hLivePct  *float64 `json:"price_change_60m_live_pct,omitempty"` // LIVE price vs ~60 minutes ago (self-computed from the finest ≤1h klines). NOT a candle-close figure.
	PriceChange24hLivePct *float64 `json:"price_change_24h_live_pct,omitempty"` // LIVE price vs 24 hours ago (rolling, exchange ticker basis)
	OICurrentBase         *float64 `json:"oi_current_base,omitempty"`           // current open interest, base-asset units
	// Crowd-positioning metrics from Binance futures data endpoints:
	LongShortAccountRatio  *float64 `json:"long_short_account_ratio,omitempty"`  // global accounts long/short
	TopTraderPositionRatio *float64 `json:"top_trader_position_ratio,omitempty"` // top-20%-by-position traders long/short
	TakerBuySellRatio      *float64 `json:"taker_buy_sell_ratio,omitempty"`      // taker buy/sell volume, 1h
	// FundingRollover is the PROGRAM-verified short-side crowding condition ②
	// ("费率刚从高位回落"), computed from the settled funding history vs the
	// live forward rate (user review 2026-09-15 point 6: the rule demanded
	// this evidence but no structured-signal field carried it, so the model
	// could only trust — or distrust — the scanner's snapshot pattern text).
	// Absent = history unavailable → the condition is UNKNOWN, never "not
	// rolled over".
	FundingRollover *FundingRolloverState `json:"funding_rollover,omitempty"`
}

// FundingRolloverState: detected via market.FundingRolloverDetected — the
// settled rate 3 intervals ago sat above the symbol-scaled crowding threshold
// and the live forward rate is now below it. from/to are annualized on the
// symbol's real settlement interval.
type FundingRolloverState struct {
	Detected          bool     `json:"detected"`
	FromAnnualizedPct *float64 `json:"from_annualized_pct,omitempty"` // the "high" it rolled off, annualized
	ToAnnualizedPct   *float64 `json:"to_annualized_pct,omitempty"`   // the live forward rate, annualized
	SettlesBack       int      `json:"settles_back"`                  // reference point: N settlements ago
}

// LiquiditySignal carries order-book tradability metrics.
type LiquiditySignal struct {
	SpreadBps         *float64 `json:"spread_bps,omitempty"`
	Depth05PctUSD     *float64 `json:"depth_0_5pct_usd,omitempty"`
	QuoteVolume24hUsd *float64 `json:"quote_volume_24h_usd,omitempty"` // absolute 24h turnover, for size/slip judgment
}

// SymbolSignal is the complete per-symbol block injected into the prompt.
type SymbolSignal struct {
	Symbol         string  `json:"symbol"`
	TimestampUTC   string  `json:"timestamp_utc"`
	Price          float64 `json:"price"`
	LimitBuyPrice  float64 `json:"limit_buy_price"`  // live price ×(1-offset) — 预计算的做多限价挂单价
	LimitSellPrice float64 `json:"limit_sell_price"` // live price ×(1+offset) — 预计算的做空限价挂单价
	// LimitEntryOffsetPct is the offset actually applied to the anchors above
	// (ATR-scaled per AnchorOffsetConfig; the model copies the prices, this
	// field documents the distance it is being asked to wait for).
	LimitEntryOffsetPct float64 `json:"limit_entry_offset_pct"`
	PrimaryTF           string  `json:"primary_tf"`
	// Explicit timeframe roles (⑨): one field, three jobs — which TF drives
	// execution timing, which carries the tradeable trend, which sets regime.
	RoleTFs        RoleTimeframes       `json:"role_tfs"`
	Timeframes     map[string]*TFSignal `json:"timeframes"`
	Derivatives    *DerivSignal         `json:"derivatives,omitempty"`
	Liquidity      *LiquiditySignal     `json:"liquidity,omitempty"`
	EntryTriggered bool                 `json:"entry_rule_triggered"`
	ExitTriggered  bool                 `json:"exit_rule_triggered"`
	// ⑧ Precomputed execution filter — mirrors the trader's entry-timing
	// gate so the model never has to derive "can I even open long here".
	ExecutionFilter *ExecutionFilter `json:"execution_filter,omitempty"`
	// ⑫ Precomputed breakout state vs the 1h structure levels.
	Breakout *BreakoutState `json:"breakout,omitempty"`
	// BBRide is the pre-computed 15m upper-band ride verdict (bb_ride):
	// volume surge + ≥3 consecutive bullish closed 15m bars hugging the upper
	// Bollinger band — the momentum-ride market-entry evidence.
	BBRide *BBRide `json:"bb_ride,omitempty"`
	// ShortRide is the lower-band plunge-ride mirror (short_ride, 09-16):
	// volume surge + ≥3 consecutive bearish closed 15m bars hugging the lower
	// Bollinger band — the market-open_short evidence for plunges where the
	// short limit anchor sits suppressed against the swing-low.
	ShortRide *BBShortRide `json:"short_ride,omitempty"`
	// ⑩ Data completeness ≠ data sufficiency: per-TF closed-bar counts vs
	// indicator requirements.
	DataQuality *DataQuality `json:"data_quality,omitempty"`
	// DataComplete means the core OHLC/kline data was sufficient this cycle.
	// It does NOT cover derivatives coverage — missing funding/long-short
	// data (e.g. tokenized-stock perps) is flagged in warnings instead.
	DataComplete bool     `json:"data_complete"`
	Warnings     []string `json:"warnings,omitempty"`

	BTC           *BTCCorrelation    `json:"btc_correlation,omitempty"`
	DataFreshness *DataFreshness     `json:"data_freshness,omitempty"`
	TraderHistory *TraderHistoryStat `json:"trader_history,omitempty"`
	// LossStreak is the program's circuit-breaker verdict for this symbol.
	// Present ONLY when the symbol is currently banned; the model may cite
	// LOSS_STREAK_BAN for this symbol and no other.
	LossStreak *LossStreakState `json:"loss_streak,omitempty"`
	// MinSize is the precomputed minimum-notional feasibility for this
	// symbol at the current equity (see MinSizeCheck).
	MinSize *MinSizeCheck `json:"min_size,omitempty"`

	// HardGate is the program's per-direction open verdict: entry basis,
	// anchor/limit availability, the exhaustive rr_scan, and the machine
	// blocker list (see HardEntryGate / DirectionGate). The model must open
	// only in a direction this says allowed (or argue one of the two
	// documented market-order exceptions), and adopt rr_scan.first_rr_ge_target
	// as the take-profit instead of re-scanning structure.
	HardGate *HardEntryGate `json:"hard_entry_gate,omitempty"`
	// Bias names the SOURCE of each directional read (see BiasBlock).
	Bias *BiasBlock `json:"bias,omitempty"`

	SignalConflict *SignalConflict `json:"signal_conflict,omitempty"`
}

// RoleTimeframes names the job of each timeframe (⑨): execution TF times the
// entry, trend TF carries the directional call, regime TF sets the context.
type RoleTimeframes struct {
	ExecutionTF string `json:"execution_tf"` // entry timing (usually 15m)
	TrendTF     string `json:"trend_tf"`     // directional call (1h)
	RegimeTF    string `json:"regime_tf"`    // market regime context (4h)
}

// ExecutionFilter precomputes whether each direction may even be opened
// under the micro-trend alignment rule (⑧): longs need up/pullback, shorts
// need down, range blocks both. When the strategy enables the entry-timing
// gate these are HARD rules (the trader rejects blocked opens); otherwise
// they are advisory.
type ExecutionFilter struct {
	MicroTF      string `json:"micro_tf"`
	MicroTrend   string `json:"micro_trend"`
	LongAllowed  bool   `json:"long_allowed"`
	ShortAllowed bool   `json:"short_allowed"`
	Reason       string `json:"reason,omitempty"`
}

// BreakoutState is the precomputed breakout verdict vs the 1h structure
// level in the direction the 1h trend implies (⑫): the model should read one
// field instead of assembling price/levels/volume/OI itself.
type BreakoutState struct {
	Status        string  `json:"status"`              // below | approach | broken_unconfirmed | confirmed | fake_break | retest_hold
	Direction     string  `json:"direction"`           // breakout (long) | breakdown (short)
	Level         float64 `json:"level"`               // the structure level being tested
	DistancePct   float64 `json:"distance_pct"`        // price vs level, % (positive = beyond)
	VolumeConfirm bool    `json:"volume_confirmation"` // breakout bars carry ≥1.5× volume
	OIConfirm     bool    `json:"oi_confirmation"`     // OI expanded in the breakout direction (1h)
}

// DataQuality separates "the fetch worked" from "there is enough history for
// the indicators to mean anything" (⑩): a 3-day-old listing has complete but
// insufficient 4h bars.
type DataQuality struct {
	Complete      bool           `json:"complete"`
	Sufficient    bool           `json:"sufficient"`
	AvailableBars map[string]int `json:"available_bars"`
	MinimumBars   map[string]int `json:"minimum_bars"`
	Shortfall     []string       `json:"shortfall,omitempty"`
}

// SignalConflict tallies directional evidence (timeframe trends + live 60m
// change + scanner bias) and flags when the scanner's bias contradicts market
// structure — the model must resolve it explicitly before trading.
// DirectionalScore is the net consensus on a -100..+100 scale (⑥): far more
// readable for the model than raw counts. Types classifies WHAT conflicts
// (⑦): scanner-vs-structure is meaningful, a 15m/5m range split is noise.
type SignalConflict struct {
	DirectionalConflict bool     `json:"directional_conflict"`
	BullishEvidence     int      `json:"bullish_evidence"`
	BearishEvidence     int      `json:"bearish_evidence"`
	DirectionalScore    int      `json:"directional_score"` // -100..+100 net consensus
	Types               []string `json:"types,omitempty"`   // SCANNER_VS_STRUCTURE | TIMEFRAME_SPLIT
	Note                string   `json:"note,omitempty"`
}

// BTCCorrelation quantifies co-movement with BTC on 1h returns (concentration
// risk: a basket of high-beta/high-corr alts is one big BTC position).
type BTCCorrelation struct {
	Pearson float64 `json:"pearson"` // -1..1, hourly returns vs BTC
	Beta    float64 `json:"beta"`    // cov(sym,btc)/var(btc)
	Samples int     `json:"samples"` // return observations used
	Window  string  `json:"window"`  // e.g. "1h"
}

// DataFreshness lets the model judge how trustworthy the block is.
type DataFreshness struct {
	SettlementAgeSeconds float64  `json:"settlement_age_seconds"`                  // now - last closed candle close (primary TF)
	VendorDivergencePct  *float64 `json:"vendor_vs_live_divergence_pct,omitempty"` // vendor forming close vs live ticker before the live-price patch
}

// TraderHistoryStat is this trader's own closed-trade record on one symbol.
type TraderHistoryStat struct {
	ClosedTrades int     `json:"closed_trades"`
	Wins         int     `json:"wins"`
	WinRatePct   float64 `json:"win_rate_pct"`
	RealizedPnL  float64 `json:"realized_pnl"`
}

// LossStreakState is the PROGRAM-computed loss-streak circuit-breaker verdict
// for one symbol (auto_trader_lossstreak.go is the enforcement mirror). The
// model must read this field instead of self-judging the ban — an audit on
// 09-15 found the model tagging just-won symbols (e.g. CAPUSDT ×17) with
// LOSS_STREAK_BAN, poisoning the entry_quality dataset.
type LossStreakState struct {
	Banned    bool    `json:"banned"`
	UntilUTC  string  `json:"until_utc,omitempty"`  // ban expiry (program-computed)
	HoursLeft float64 `json:"hours_left,omitempty"` // remaining ban duration
}

// MinSizeCheck precomputes whether any ALLOWED stop on this symbol can meet
// the strategy's minimum position size at the current equity (risk_control.
// min_position_size from the web strategy page — the same value
// enforceMinPositionSize rejects opens under). Small accounts have a
// structural dead zone: notional = equity×risk% ÷ stop%, so a wide stop floor
// (high-ATR coins) yields a notional below the minimum — every order would be
// rejected. Surfacing this per-symbol stops the model from burning decision
// budget on settings that can never execute (09-15 CAPUSDT case: 10.4-11.4%
// stop floor → 7-7.6U notional < 10U minimum, re-derived every cycle).
type MinSizeCheck struct {
	Feasible           bool    `json:"feasible"`                  // false = even the tightest allowed stop breaks the minimum
	MinPositionSizeUsd float64 `json:"min_position_size_usd"`     // risk_control.min_position_size in effect
	MaxStopPct         float64 `json:"max_stop_pct_for_min_size"` // stop distance beyond this → notional < minimum
	StopFloorPct       float64 `json:"stop_floor_pct,omitempty"`  // SLMinATRMult×ATR(1h); 0 = no noise floor configured
	Reason             string  `json:"reason,omitempty"`          // set when !Feasible
}

// RRScan is the program's exhaustive structural take-profit scan for one
// direction (user review 2026-09-15 points 1/2/8): walk EVERY resistance
// (long) / support (short) element across ALL timeframe blocks, near→far,
// deduped, and compute the RR of each at the TIGHTEST ALLOWED stop (the noise
// floor). The floor stop is deliberately the best case — a wide structural
// stop only lowers RR — so best_rr < min_rr is a DEFINITIVE verdict that the
// RR gate cannot be passed in this direction, whatever structure the model
// picks. first_rr_ge_target is the level the TP rule demands: the nearest
// target whose RR clears min_rr — the model adopts it instead of re-deriving.
type RRScan struct {
	Direction       string  `json:"direction"`                    // long | short
	EntryPrice      float64 `json:"entry_price"`                  // the anchor the scan assumed
	EntryBasis      string  `json:"entry_basis"`                  // limit_anchor | live_price
	StopDistancePct float64 `json:"stop_distance_pct"`            // METHODOLOGY stop (stop_plan) distance in % — the same stop the gate verdict is computed at
	StopPrice       float64 `json:"stop_price"`                   // the methodology stop price — adopt as stop_loss verbatim
	MinRR           float64 `json:"min_rr"`                       // strategy min_risk_reward_ratio in effect
	TargetsScanned  int     `json:"targets_scanned"`              // distinct structural levels scanned
	BestTarget      float64 `json:"best_target,omitempty"`        // farthest scanned level (max RR)
	BestRR          float64 `json:"best_rr"`                      // RR upper bound AT THE METHODOLOGY STOP — < min_rr ⇒ RR fails for sure
	FirstRRGeTarget float64 `json:"first_rr_ge_target,omitempty"` // nearest level with RR ≥ min_rr — the TP to use
	Usable          bool    `json:"usable"`                       // a qualifying target exists
}

// DirectionGate is the program's per-direction open verdict for one symbol —
// the "three hard gates" (stop band / RR≥min / timing + anchor + breaker +
// data + size) evaluated ONCE by code so the model sorts, explains and
// chooses instead of re-assembling gates per cycle (review point 10).
type DirectionGate struct {
	Allowed      bool    `json:"allowed"`
	EntryPrice   float64 `json:"entry_price"`
	EntryBasis   string  `json:"entry_basis"`   // limit_anchor | live_price
	LimitAllowed bool    `json:"limit_allowed"` // false = anchor suppressed (limit path dead; the two documented market-order exceptions may still apply)
	StopFloorPct float64 `json:"stop_floor_pct,omitempty"`
	// StopPlan is the precomputed METHODOLOGY stop (nearest opposite-side
	// structure + direction-aware buffer, clamped into the band). 09-19 RR
	// audit: the gate used to score RR at the noise-floor stop while actual
	// orders stopped at structure+buffer (systematically wider) — trades
	// passed the gate at "1.5R" but executed at 1.3-1.4R. Now the plan, the
	// gate and the executor's checkRR all price the same stop.
	StopPlanPrice float64  `json:"stop_plan_price,omitempty"`
	StopPlanPct   float64  `json:"stop_plan_pct,omitempty"`
	RR            *RRScan  `json:"rr_scan,omitempty"` // nil when no noise floor is configured (best-case RR undefined)
	Failed        []string `json:"failed,omitempty"`  // machine codes: MICRO_TREND_NOT_LONG/SHORT, LIMIT_ANCHOR_SUPPRESSED, RR_MAX_x.xx, STOP_PLAN_NO_STRUCTURE, STOP_PLAN_OUT_OF_BAND, DATA_INSUFFICIENT, MIN_SIZE_DEAD_ZONE, LOSS_STREAK_BANNED, STOCK_WEEKEND, VENDOR_DIVERGENCE_x.xx
}

// HardEntryGate holds both direction verdicts.
type HardEntryGate struct {
	Long  *DirectionGate `json:"long"`
	Short *DirectionGate `json:"short"`
}

// BiasBlock separates the three bias SOURCES so "scanner says short" is never
// phrased as "short evidence is strong" (review point 3, AKE case): scanner =
// the snapshot engine's stance; structure = market evidence (directional
// score sign); execution = which direction the micro-trend gate currently
// allows. The final trade bias stays the model's job.
type BiasBlock struct {
	Scanner   string `json:"scanner"`   // long | short | none
	Structure string `json:"structure"` // long | short | mixed
	Execution string `json:"execution"` // long_only | short_only | both | none | unknown
}

// SignalOptions carries the evaluation moment, the strategy's primary
// timeframe, and the quant layer's true 1h OI change. All indicator features
// are self-computed from closed bars — no display toggles involved.
type SignalOptions struct {
	Now       time.Time // zero falls back to time.Now()
	PrimaryTF string
	OI1hPct   *float64
	// CurrentPrice is the live ticker price used as the support/resistance
	// anchor (which pivot levels count as above/below). Zero falls back to
	// data.CurrentPrice, then to each timeframe's last closed close.
	CurrentPrice float64
	// LimitEntryOffsetPct is the fixed limit-entry anchor offset in percent:
	// limit_buy = price×(1-x/100), limit_sell = price×(1+x/100).
	// Zero falls back to 0.5% (the default strategy preference). It is the
	// fixed-mode value AND the ATR-unavailable fallback — see the
	// LimitEntryOffset* fields below.
	LimitEntryOffsetPct float64
	// Volatility-scaled anchor parameters (risk_control.limit_entry_offset_*):
	// mode "atr" (default when empty) = ATRMult × ATR(execution TF) clamped
	// to [MinPct, MaxPct]; "fixed" = LimitEntryOffsetPct as-is. The same
	// scaling drives the anchor breathing-room threshold.
	LimitEntryOffsetMode    string
	LimitEntryOffsetATRMult float64
	LimitEntryOffsetMinPct  float64
	LimitEntryOffsetMaxPct  float64
	// SupplyZonePct mirrors risk_control.open_reject_supply_pct: when > 0 the
	// pre-computed anchors are CROSS-VALIDATED against the 15m/1h structure —
	// an anchor parked within this % of the opposite-side level is zeroed out
	// (the trader would reject the order anyway; the model must not see a
	// tradable anchor that cannot execute).
	SupplyZonePct float64

	QuoteVolume24hUsd      float64            // absolute 24h turnover (USDT)
	ScannerBias            string             // "short" when the symbol carries a short-scanner hint
	ScannerConflict        bool               // two scanners give OPPOSITE directional conclusions for this symbol
	Quant                  *QuantData         // per-symbol quant snapshot (24h rolling change, current OI)
	LongShortAccountRatio  *float64           // global accounts long/short
	TopTraderPositionRatio *float64           // top traders' position long/short
	TakerBuySellRatio      *float64           // taker buy/sell volume ratio
	BtcCloses              []float64          // closed 1h BTC closes for correlation/beta
	VendorStalenessPct     *float64           // vendor forming close vs live ticker
	TraderHistory          *TraderHistoryStat // this trader's closed-trade record on the symbol
	// LossStreakBannedUntil is non-zero when the trader's circuit breaker has
	// banned this symbol from new opens until that moment (program-computed
	// from the closed-trade record — never the model's judgment).
	LossStreakBannedUntil time.Time
	// Min-position-size feasibility inputs: with notional = equity×risk% ÷
	// stop%, a stop beyond equity×risk%/minPositionSize cannot meet the
	// strategy's minimum position size (risk_control.min_position_size, web
	// strategy page — the same gate enforceMinPositionSize enforces).
	// Zero equity/risk/minPositionSize disables the MinSize block.
	EquityUSDT          float64
	RiskPct             float64 // RiskPerTradePct (prompt default 1.5)
	MinPositionSizeUSDT float64 // risk_control.min_position_size; default 12 when unset
	SLMinATRMult        float64 // stop noise floor multiplier; <=0 = no floor
	// Hard-entry gate inputs (09-15 review): the program re-evaluates the
	// strategy's own gates per direction so the model never re-derives them.
	MinRR             float64 // risk_control.min_risk_reward_ratio; <=0 = RR verdict skipped
	LimitEntryEnabled bool    // anchors gate the limit path when true; false = market-order default
	EntryTimingGate   bool    // micro-trend alignment is a HARD block only when enabled (executor parity)
	StockWeekendBlock bool    // symbol is a bstock AND US market weekend AND policy enabled
	// ShortFundingCrowdPctP is the configured per-8h settlement crowding
	// threshold in PERCENT (coin_source.short_scan_funding_rate_pct, default
	// 0.03). Scaled to the symbol's real settle interval, it defines the
	// "high" the funding_rollover condition rolls off — same number that
	// annualizes into strategy prompt condition ①.
	ShortFundingCrowdPctP float64
	// MaxVendorDivergencePct hard-blocks both directions when the vendor
	// forming-close vs live ticker diverges beyond this percent
	// (risk_control.max_vendor_divergence_pct; resolved by
	// EffectiveMaxVendorDivergencePct: 0 = default 2, negative = disabled).
	// A 2.6% vendor/live gap shifts every anchor/SL/TP off the real market
	// (MYXUSDT 09-18) — entry, stop and target are all priced off the wrong
	// tick. <=0 (disabled) skips the check.
	MaxVendorDivergencePct float64
}

// ComputeSymbolSignals builds the normalized signal block for one symbol from
// its multi-timeframe market data. Returns an error when the data is too
// incomplete to trade on — the caller must then EXCLUDE the symbol (missing
// data = no trading).
func ComputeSymbolSignals(symbol string, data *market.Data, opt SignalOptions) (*SymbolSignal, error) {
	if data == nil {
		return nil, fmt.Errorf("no market data")
	}
	if data.CurrentPrice <= 0 {
		return nil, fmt.Errorf("invalid price %.8f", data.CurrentPrice)
	}
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	// Limit-entry anchor offset (percent). Strategy-configurable via
	// risk_control.limit_entry_offset_*; ATR-scaled with a fixed fallback —
	// the anchors themselves are computed AFTER the TF loop below (they read
	// the primary TF's ATR).
	offsetCfg := AnchorOffsetConfig{
		Mode:     opt.LimitEntryOffsetMode,
		FixedPct: opt.LimitEntryOffsetPct,
		ATRMult:  opt.LimitEntryOffsetATRMult,
		MinPct:   opt.LimitEntryOffsetMinPct,
		MaxPct:   opt.LimitEntryOffsetMaxPct,
	}

	sig := &SymbolSignal{
		Symbol:       strings.ToUpper(symbol),
		TimestampUTC: now.UTC().Format(time.RFC3339),
		Price:        data.CurrentPrice,
		PrimaryTF:    opt.PrimaryTF,
		Timeframes:   map[string]*TFSignal{},
	}

	// Deterministic timeframe ordering.
	tfs := make([]string, 0, len(data.TimeframeData))
	for tf := range data.TimeframeData {
		tfs = append(tfs, tf)
	}
	sort.Slice(tfs, func(i, j int) bool { return tfDuration(tfs[i]) < tfDuration(tfs[j]) })

	complete := len(tfs) > 0
	// Support/resistance anchor: the live ticker price when available, else
	// the fetch's current price. Filtering pivot levels against a stale
	// per-timeframe close misclassifies levels the live price already crossed
	// (e.g. a daily "resistance" below the live price after an intraday pump).
	anchorPrice := opt.CurrentPrice
	if anchorPrice <= 0 {
		anchorPrice = data.CurrentPrice
	}
	// Closed 1h closes (for BTC correlation) and the primary TF's settlement
	// age (for the freshness marker).
	var sym1hCloses []float64
	var primarySettlementAge float64
	for _, tf := range tfs {
		tfData := data.TimeframeData[tf]
		if tfData == nil || len(tfData.Klines) < 10 {
			complete = false
			sig.Warnings = append(sig.Warnings, fmt.Sprintf("%s: insufficient bars (%d)", tf, len(tfData.Klines)))
			continue
		}
		tfsig, warns := computeTFSignal(tf, tfData, now, anchorPrice)
		sig.Timeframes[tf] = tfsig
		if len(warns) > 0 {
			complete = false
			sig.Warnings = append(sig.Warnings, warns...)
		}
		if tf == "1h" {
			sym1hCloses = closedCloses(tfData, now, tfDuration(tf))
		}
		if tf == opt.PrimaryTF || (opt.PrimaryTF == "" && primarySettlementAge == 0) {
			closes := closedCloses(tfData, now, tfDuration(tf))
			if len(closes) >= 2 {
				kl := ClosedKlines(tfData, now, tfDuration(tf))
				lastClose := kl[len(kl)-1].Time + tfDuration(tf).Milliseconds()
				primarySettlementAge = now.Sub(time.UnixMilli(lastClose)).Seconds()
			}
		}
	}

	// Derivatives: funding + OI. The 1h OI change comes from the quant layer
	// when available; latest-vs-average is reported separately (it is NOT a
	// 1h change).
	d := &DerivSignal{}
	if data.FundingRateOK {
		// A real zero (bstock tokens pay no funding) is DATA, not absence —
		// keep it visible as 0; only a failed fetch is reported as missing.
		f := data.FundingRate
		d.FundingRate = &f
	}
	if opt.OI1hPct != nil {
		d.OIChange1hPct = opt.OI1hPct
	}
	if opt.Quant != nil {
		if pc, ok := opt.Quant.PriceChange["24h"]; ok {
			d.PriceChange24hLivePct = &pc
		}
		if qoi := opt.Quant.OI["binance"]; qoi != nil && qoi.CurrentOI > 0 {
			v := qoi.CurrentOI
			d.OICurrentBase = &v
		}
	}
	if data.OpenInterest != nil && data.OpenInterest.Average > 0 {
		vsAvg := (data.OpenInterest.Latest - data.OpenInterest.Average) / data.OpenInterest.Average * 100
		d.OIVsAvgPct = &vsAvg
	}
	// Rolling live change (Binance 15m×4) when provided — the raw
	// data.PriceChange1h anchors on the primary TF's candle boundary and, with
	// a stale vendor forming close, can degenerate into the vendor divergence
	// figure (identical numbers, different concept).
	// Self-computed rolling 1h change from the symbol's own klines — shares
	// no anchor with the vendor's frozen forming candle, so it can no longer
	// degenerate into a copy of the vendor divergence figure. Falls back to
	// the fetch-layer value only when no ≤1h timeframe has enough bars.
	p := rolling1hChangePct(data, now)
	d.PriceChange1hLivePct = &p
	if opt.LongShortAccountRatio != nil {
		d.LongShortAccountRatio = opt.LongShortAccountRatio
	}
	if opt.TopTraderPositionRatio != nil {
		d.TopTraderPositionRatio = opt.TopTraderPositionRatio
	}
	if opt.TakerBuySellRatio != nil {
		d.TakerBuySellRatio = opt.TakerBuySellRatio
	}
	sig.Derivatives = d
	// ⑤ Precomputed funding annualization on the symbol's REAL settlement
	// interval (measured from settlement timestamps, see market.Data).
	// Falls back to the 8h×3 convention when the interval is unknown — a
	// fixed ×3 understates 4h/1h listings by 2-8× and made the structured
	// signal disagree with the scanner's properly annualized figure.
	if d.FundingRate != nil && *d.FundingRate != 0 { // rate 0 → ann 0, no annualization needed
		ann := annualizeFunding(*d.FundingRate, data.FundingSettleHours)
		d.FundingAnnualizedPct = &ann
		if data.FundingSettleHours > 0 {
			h := data.FundingSettleHours
			d.FundingSettleHours = &h
		}
	}
	// Program-verified crowding condition ② (review 09-15 point 6): the
	// settled rate 3 intervals ago sat above the symbol-scaled crowding
	// threshold and the live forward rate is now below it. Same definition the
	// short scanner uses (market.FundingRolloverDetected) — the scanner keeps
	// its looser snapshot threshold, but "回落" can no longer mean two things
	// in one prompt. History missing → field absent = UNKNOWN.
	if len(data.FundingHistory) >= 4 && d.FundingRate != nil {
		settle := 8.0
		if data.FundingSettleHours > 0 {
			settle = data.FundingSettleHours
		}
		// Condition ①'s threshold (per 8h, percent) scaled to this symbol's
		// real interval: annualized X over frPct×3×365 ⇒ raw > frPct/100 ×
		// settle/8 per settlement.
		threshPct := opt.ShortFundingCrowdPctP
		if threshPct <= 0 {
			threshPct = 0.03
		}
		threshRaw := threshPct / 100 * settle / 8
		prev := data.FundingHistory[len(data.FundingHistory)-4]
		cur := *d.FundingRate
		from := annualizeFunding(prev, settle)
		to := annualizeFunding(cur, settle)
		d.FundingRollover = &FundingRolloverState{
			Detected:          market.FundingRolloverDetected(prev, cur, threshRaw),
			FromAnnualizedPct: &from,
			ToAnnualizedPct:   &to,
			SettlesBack:       3,
		}
	}

	if opt.QuoteVolume24hUsd > 0 {
		v := opt.QuoteVolume24hUsd
		sig.Liquidity = &LiquiditySignal{QuoteVolume24hUsd: &v}
	}
	if opt.TraderHistory != nil {
		sig.TraderHistory = opt.TraderHistory
	}
	if len(opt.BtcCloses) > 0 && len(sym1hCloses) > 0 {
		sig.BTC = btcCorrelation(sym1hCloses, opt.BtcCloses)
	}
	fresh := &DataFreshness{SettlementAgeSeconds: primarySettlementAge}
	if opt.VendorStalenessPct != nil {
		fresh.VendorDivergencePct = opt.VendorStalenessPct
	}
	sig.DataFreshness = fresh

	// Directional evidence tally + typed conflict flag (⑥⑦).
	// Range/unknown timeframes contribute NOTHING — a 15m/5m range split is
	// noise, not a directional conflict. A genuine TIMEFRAME_SPLIT requires
	// the 1h and 4h trends to actually oppose each other.
	bull, bear := 0, 0
	for _, tf := range sig.Timeframes {
		if tf == nil {
			continue
		}
		switch tf.Trend {
		case "up":
			bull++
		case "down":
			bear++
		}
	}
	if sig.Derivatives != nil && sig.Derivatives.PriceChange1hLivePct != nil {
		switch {
		case *sig.Derivatives.PriceChange1hLivePct > 0:
			bull++
		case *sig.Derivatives.PriceChange1hLivePct < 0:
			bear++
		}
	}
	if opt.ScannerBias == "short" {
		bear++ // the scanner's own bias is one unit of bearish evidence
	} else if opt.ScannerBias == "long" {
		bull++
	}
	conflict := &SignalConflict{BullishEvidence: bull, BearishEvidence: bear}
	if total := bull + bear; total > 0 {
		conflict.DirectionalScore = int(math.Round(100 * float64(bull-bear) / float64(total)))
	}
	var tfUp, tfDown bool
	t1h := sig.Timeframes["1h"]
	t4h := sig.Timeframes["4h"]
	if t1h != nil {
		tfUp = t1h.Trend == "up"
		tfDown = t1h.Trend == "down"
	}
	if t4h != nil {
		tfUp = tfUp && t4h.Trend == "up"
		tfDown = tfDown && t4h.Trend == "down"
		if t1h != nil &&
			((t1h.Trend == "up" && t4h.Trend == "down") || (t1h.Trend == "down" && t4h.Trend == "up")) {
			conflict.DirectionalConflict = true
			conflict.Types = append(conflict.Types, "TIMEFRAME_SPLIT")
		}
	}
	switch {
	case opt.ScannerBias == "short" && tfUp:
		conflict.DirectionalConflict = true
		conflict.Types = append(conflict.Types, "SCANNER_VS_STRUCTURE")
		conflict.Note = fmt.Sprintf("scanner bias SHORT vs bullish 1h+4h structure — resolve before trading")
	case opt.ScannerBias == "long" && tfDown:
		conflict.DirectionalConflict = true
		conflict.Types = append(conflict.Types, "SCANNER_VS_STRUCTURE")
		conflict.Note = "scanner bias LONG vs bearish 1h+4h structure — resolve before trading"
	}
	// Cross-scanner conflict (audit 2026-09-12 #11): short_scan says SHORT
	// while piggy_dash selected UP for the same symbol — the model must
	// resolve which scanner to trust, never average them away.
	if opt.ScannerConflict {
		conflict.DirectionalConflict = true
		conflict.Types = append(conflict.Types, "SCANNER_VS_SCANNER")
		conflict.Note = "short_scan (SHORT) and piggy_dash (UP) disagree on this symbol — resolve the conflict before trading"
	}
	if conflict.DirectionalConflict && conflict.Note == "" {
		conflict.Note = "1h and 4h trends oppose — resolve the regime before trading"
	}
	sig.SignalConflict = conflict

	// ⑧ Execution filter — mirror of the trader's micro-trend gate.
	var microTrend string
	var microTF string
	if t15 := sig.Timeframes["15m"]; t15 != nil && t15.Trend != "" && t15.Trend != "unknown" {
		microTF, microTrend = "15m", t15.Trend
	} else if t30 := sig.Timeframes["30m"]; t30 != nil {
		microTF, microTrend = "30m", t30.Trend
	}
	if microTF != "" {
		ef := &ExecutionFilter{MicroTF: microTF, MicroTrend: microTrend}
		ef.LongAllowed = microTrend == "up" || microTrend == "pullback"
		ef.ShortAllowed = microTrend == "down" || microTrend == "rally"
		switch {
		case ef.LongAllowed && !ef.ShortAllowed:
			ef.Reason = "micro trend aligned for longs only"
		case ef.ShortAllowed && !ef.LongAllowed:
			ef.Reason = "micro trend aligned for shorts only"
		default:
			ef.Reason = "micro trend is " + microTrend + " — no directional alignment"
		}
		sig.ExecutionFilter = ef
	}

	// ⑨ Role timeframes.
	sig.RoleTFs = RoleTimeframes{ExecutionTF: opt.PrimaryTF, TrendTF: "1h", RegimeTF: "4h"}

	// ⑩ Data quality — complete (fetched) vs sufficient (enough history for
	// the indicator stack: RSI14/MACD(26)/EMA50 need ≥60 closed bars).
	// Only timeframes the analysis stack actually reads are required. The
	// old static "1d" entry was never fetched, so its 0 bars pinned
	// sufficient=false forever (review 2026-09-07: dead flag).
	minBars := map[string]int{"15m": 60, "1h": 60, "4h": 60}
	if opt.PrimaryTF != "" {
		if _, ok := minBars[opt.PrimaryTF]; !ok {
			minBars[opt.PrimaryTF] = 60
		}
	}
	avail := map[string]int{}
	sufficient := true
	var shortfall []string
	for tfName, min := range minBars {
		n := 0
		if tfData, ok := data.TimeframeData[tfName]; ok && tfData != nil {
			n = len(tfData.Klines)
		}
		avail[tfName] = n
		if n > 0 && n < min {
			sufficient = false
			shortfall = append(shortfall, fmt.Sprintf("%s: %d bars < %d minimum — long-window indicators (EMA50/MACD) are unstable", tfName, n, min))
		}
		if n == 0 {
			sufficient = false
		}
	}
	sig.DataQuality = &DataQuality{
		Complete:      complete && sig.Price > 0,
		Sufficient:    sufficient,
		AvailableBars: avail,
		MinimumBars:   minBars,
		Shortfall:     shortfall,
	}

	// ⑪ Pre-computed limit-entry anchors — volatility-scaled: offset =
	// ATRMult × ATR(1h) clamped to [MinPct, MaxPct], falling back to the
	// fixed percent when ATR is unavailable (review 2026-09-07: a fixed 0.3%
	// demanded most of a quiet book's ATR and rode the live price for violent
	// movers; user directive 2026-09-10: the yardstick is ATR(1h), not the
	// execution TF, whose ATR tracked 15m micro-noise).
	var anchorATRPct float64
	if t1h, ok := sig.Timeframes["1h"]; ok && t1h != nil {
		anchorATRPct = t1h.ATRPct
	} else if primary := primaryTFSignal(sig, opt.PrimaryTF); primary != nil {
		anchorATRPct = primary.ATRPct
	}
	sig.LimitEntryOffsetPct = AnchorOffsetPct(anchorATRPct, offsetCfg)
	sig.LimitBuyPrice = data.CurrentPrice * (1 - sig.LimitEntryOffsetPct/100)
	sig.LimitSellPrice = data.CurrentPrice * (1 + sig.LimitEntryOffsetPct/100)

	// ① Anchor cross-validation (user review 2026-09-07): the pre-computed
	// limit anchors are checked against the same supply-zone rule the trader
	// enforces at execution — an anchor with no breathing room is ZEROED so
	// the model never sees a tradable price that would be rejected. The
	// breathing threshold scales with the EXECUTION TF's ATR (spec 2026-09-09
	// step 4: 0.5×ATR(执行周期)) — the check means "the fill must not land
	// right under a ceiling", so the yardstick is fill-site noise, not the
	// deep 1h corridor; the trader's gate applies the same formula to fresh
	// data.
	execATRPct := 0.0
	if primary := primaryTFSignal(sig, opt.PrimaryTF); primary != nil {
		execATRPct = primary.ATRPct
	}
	if opt.SupplyZonePct > 0 {
		breathing := AnchorBreathingPct(execATRPct, offsetCfg, opt.SupplyZonePct)
		suppressAnchorsAgainstStructure(sig, data, breathing)
	}

	// Entry / exit heuristic rules — deterministic, computed so the model sees
	// the program's verdict and can agree or disagree with reasoning.
	primary := primaryTFSignal(sig, opt.PrimaryTF)
	if primary != nil {
		sig.EntryTriggered = primary.Trend == "up" &&
			primary.RSI14 != nil && *primary.RSI14 > 40 && *primary.RSI14 < 72 &&
			primary.VolumeRatio != nil && *primary.VolumeRatio >= 1.0 &&
			primary.LastClosedCandle == "bullish"
		structLow := 0.0
		if primary.StructureLow != nil {
			structLow = *primary.StructureLow
		}
		sig.ExitTriggered = (structLow > 0 && sig.Price < structLow) ||
			(primary.RSI14 != nil && *primary.RSI14 > 80) ||
			(primary.Trend == "down" && primary.LastClosedCandle == "bearish")
	}

	// ⑫ Breakout state vs the 1h structure level, in the direction the 1h
	// trend implies — the model reads one verdict instead of assembling
	// price/levels/volume/OI itself.
	sig.Breakout = computeBreakoutState(data, sig)
	sig.BBRide = computeBBRide(data)
	sig.ShortRide = computeBBShortRide(data)

	// Loss-streak circuit breaker: the trader computes the ban from the
	// closed-trade record; the signal only mirrors the verdict so the model
	// never has to (and must not) self-judge it.
	if !opt.LossStreakBannedUntil.IsZero() {
		hoursLeft := opt.LossStreakBannedUntil.Sub(now).Hours()
		if hoursLeft < 0 {
			hoursLeft = 0
		}
		sig.LossStreak = &LossStreakState{
			Banned:    true,
			UntilUTC:  opt.LossStreakBannedUntil.UTC().Format(time.RFC3339),
			HoursLeft: math.Round(hoursLeft*10) / 10,
		}
	}

	// Min-position-size feasibility: notional = equity×risk% ÷ stop%, so the
	// widest stop the rules allow must still satisfy equity×risk%/stop ≥
	// min_position_size. The binding constraint is the NOISE FLOOR
	// (SLMinATRMult×ATR(1h)) — if even that floor overshoots, no permitted
	// stop can produce a tradable notional (structural dead zone). Mirrors
	// the sizing formula and the executor's enforceMinPositionSize rejection.
	if opt.EquityUSDT > 0 && opt.RiskPct > 0 && opt.MinPositionSizeUSDT > 0 {
		riskUSD := opt.EquityUSDT * opt.RiskPct / 100
		// Stop distance d (in %) at which notional exactly equals the minimum.
		maxStopPct := riskUSD / opt.MinPositionSizeUSDT * 100
		floorPct := stopFloorPct(sig, opt.SLMinATRMult)
		ms := &MinSizeCheck{
			Feasible:           floorPct <= maxStopPct,
			MinPositionSizeUsd: opt.MinPositionSizeUSDT,
			MaxStopPct:         math.Round(maxStopPct*100) / 100,
			StopFloorPct:       math.Round(floorPct*100) / 100,
		}
		if !ms.Feasible {
			ms.Reason = fmt.Sprintf(
				"stop floor %.2f%% (SLMinATR %.1f×ATR(1h)) exceeds %.2f%% max for the %.0fU minimum position size at equity %.1fU — no allowed stop can meet the strategy minimum, wait+MIN_SIZE",
				floorPct, opt.SLMinATRMult, maxStopPct, opt.MinPositionSizeUSDT, opt.EquityUSDT)
		}
		sig.MinSize = ms
	}

	// Program verdict of the "three hard gates" per direction + bias source
	// layering (review 2026-09-15 points 1-3/5/7/8/10): the model reads the
	// verdict and explains it instead of re-scanning structure and
	// re-assembling blockers every cycle.
	sig.HardGate = computeHardEntryGate(sig, opt)
	sig.Bias = computeBiasBlock(sig, opt)

	sig.DataComplete = complete && sig.Price > 0
	if !complete {
		sig.Warnings = append(sig.Warnings, "DATA INCOMPLETE — trading this symbol is prohibited this cycle")
	}
	// Derivatives coverage gaps are informational (tokenized-stock perps and
	// some listings have no funding/long-short data) — flag them so the model
	// does not treat the absence as "no crowding".
	if !data.FundingRateOK {
		sig.Warnings = append(sig.Warnings, "derivatives: funding_rate fetch FAILED this cycle — treat crowding as UNKNOWN, not absent; retries next cycle")
	}
	if opt.LongShortAccountRatio == nil {
		sig.Warnings = append(sig.Warnings, "derivatives: long/short account and top-trader ratios unavailable — crowd-positioning UNKNOWN")
	}
	return sig, nil
}

// annualizeFunding converts a raw per-settlement funding rate to an
// annualized percent on the symbol's real settlement interval (8h×3/day
// fallback when unknown, per-day clamped at ≥1).
func annualizeFunding(rate, settleHours float64) float64 {
	perDay := 3.0
	if settleHours > 0 {
		perDay = 24.0 / settleHours
		if perDay < 1 {
			perDay = 1
		}
	}
	return rate * perDay * 365 * 100
}

// stopFloorPct mirrors the executor's noise floor (SLMinATRMult × ATR(1h),
// closed bars) — 0 when no floor is configured. Single source for the
// min_size block and the hard-entry gate.
func stopFloorPct(sig *SymbolSignal, mult float64) float64 {
	if mult <= 0 {
		return 0
	}
	if t1h, ok := sig.Timeframes["1h"]; ok && t1h != nil {
		return mult * t1h.ATRPct
	}
	return 0
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// Direction-aware structure-stop buffer, in ×ATR(1h). These are the WIDE end
// of the methodology's 0.3-0.5 band (longs take the lower half 0.3-0.4 → 0.4;
// shorts — especially bounce/chase shorts with longer wicks — take the upper
// half 0.4-0.5 → 0.5). Pricing the stop plan at the wide end makes the gate's
// RR a true LOWER bound: the model can only tighten into the band, never
// manufacture a passing RR.
const (
	methodStopBufferLong  = 0.4
	methodStopBufferShort = 0.5
)

// methodStopPlan precomputes the METHODOLOGY stop for one direction: the
// nearest opposite-side structure level (≥15m granularity — 5m pivots are
// noise at a 1h-ATR buffer scale) plus the direction-aware buffer, clamped
// into the band [noiseFloor, max(2×ATR(4h), 8%)].
//
// 09-19 RR audit (user): the gate used to score RR at the noise-floor stop
// while actual orders stopped at structure+buffer — systematically wider —
// so "1.5R" trades executed at 1.3-1.4R (30d 盈亏比 1.08, PF 0.81 matches).
// Codes: "" = ok; STOP_PLAN_NO_STRUCTURE = no level beyond entry (new
// highs/lows — no structural stop exists); STOP_PLAN_OUT_OF_BAND = the
// structure stop overshoots the band cap (放弃该设置 per methodology).
func methodStopPlan(sig *SymbolSignal, entry, floorPct float64, isLong bool) (price, distPct float64, code string) {
	t1h := sig.Timeframes["1h"]
	if t1h == nil || t1h.ATRPct <= 0 {
		return 0, 0, "STOP_PLAN_NO_STRUCTURE"
	}
	bufMult := methodStopBufferLong
	if !isLong {
		bufMult = methodStopBufferShort
	}
	bufPrice := bufMult * t1h.ATRPct / 100 * entry

	// Nearest structure level beyond the entry on the STOP side, at 15m or
	// coarser granularity: longs stop below (support), shorts above
	// (resistance) — the mirror of the scanRR target walk.
	var level float64
	found := false
	for name, tf := range sig.Timeframes {
		if tf == nil || tfDuration(name) < 15*time.Minute {
			continue
		}
		src := tf.Support
		if !isLong {
			src = tf.Resistance
		}
		for _, l := range src {
			if l <= 0 {
				continue
			}
			beyond := (isLong && l < entry) || (!isLong && l > entry)
			if !beyond {
				continue
			}
			if !found || (isLong && l > level) || (!isLong && l < level) {
				level, found = l, true
			}
		}
	}
	if !found {
		return 0, 0, "STOP_PLAN_NO_STRUCTURE"
	}

	if isLong {
		price = level - bufPrice
	} else {
		price = level + bufPrice
	}
	distPct = math.Abs(price-entry) / entry * 100
	// Never tighter than the noise floor: a structure sitting inside noise
	// is governed by the floor, not the pivot.
	if distPct < floorPct {
		distPct = floorPct
		if isLong {
			price = entry * (1 - floorPct/100)
		} else {
			price = entry * (1 + floorPct/100)
		}
	}
	// Band cap: max(2×ATR(4h), 8%) — same wide-of-the-two as the executor.
	capPct := 8.0
	if t4h := sig.Timeframes["4h"]; t4h != nil && t4h.ATRPct > 0 {
		if c := 2 * t4h.ATRPct; c > capPct {
			capPct = c
		}
	}
	if distPct > capPct {
		return 0, 0, "STOP_PLAN_OUT_OF_BAND"
	}
	return price, distPct, ""
}

// computeHardEntryGate evaluates, per direction, every program-decidable
// open blocker — micro-trend (when the timing gate is enabled, mirroring the
// executor's config switch), limit-anchor suppression, structural RR at the
// METHODOLOGY stop (stop_plan), data sufficiency, min-size dead zone,
// loss-streak ban, stock weekend. allowed = nothing failed.
func computeHardEntryGate(sig *SymbolSignal, opt SignalOptions) *HardEntryGate {
	floorPct := stopFloorPct(sig, opt.SLMinATRMult)
	evaluate := func(isLong bool) *DirectionGate {
		g := &DirectionGate{}
		anchor := sig.LimitBuyPrice
		if !isLong {
			anchor = sig.LimitSellPrice
		}
		g.LimitAllowed = anchor > 0
		if opt.LimitEntryEnabled && anchor > 0 {
			g.EntryPrice, g.EntryBasis = anchor, "limit_anchor"
		} else {
			g.EntryPrice, g.EntryBasis = sig.Price, "live_price"
		}
		g.StopFloorPct = round2(floorPct)
		add := func(code string) { g.Failed = append(g.Failed, code) }
		if floorPct > 0 {
			if price, dist, code := methodStopPlan(sig, g.EntryPrice, floorPct, isLong); code != "" {
				add(code)
			} else {
				g.StopPlanPrice = price
				g.StopPlanPct = round2(dist)
				g.RR = scanRR(g.EntryPrice, g.EntryBasis, dist, price, sig.Timeframes, isLong, opt.MinRR)
			}
		}
		if opt.EntryTimingGate && sig.ExecutionFilter != nil {
			if isLong && !sig.ExecutionFilter.LongAllowed {
				add("MICRO_TREND_NOT_LONG")
			}
			if !isLong && !sig.ExecutionFilter.ShortAllowed {
				add("MICRO_TREND_NOT_SHORT")
			}
		}
		if opt.LimitEntryEnabled && !g.LimitAllowed {
			add("LIMIT_ANCHOR_SUPPRESSED")
		}
		if g.RR != nil && opt.MinRR > 0 && !g.RR.Usable {
			add(fmt.Sprintf("RR_MAX_%.2f", g.RR.BestRR))
		}
		if sig.DataQuality != nil && !sig.DataQuality.Sufficient {
			add("DATA_INSUFFICIENT")
		}
		if sig.MinSize != nil && !sig.MinSize.Feasible {
			add("MIN_SIZE_DEAD_ZONE")
		}
		if sig.LossStreak != nil {
			add("LOSS_STREAK_BANNED")
		}
		if opt.StockWeekendBlock {
			add("STOCK_WEEKEND")
		}
		if opt.MaxVendorDivergencePct > 0 &&
			sig.DataFreshness != nil && sig.DataFreshness.VendorDivergencePct != nil &&
			math.Abs(*sig.DataFreshness.VendorDivergencePct) > opt.MaxVendorDivergencePct {
			add(fmt.Sprintf("VENDOR_DIVERGENCE_%.2f", *sig.DataFreshness.VendorDivergencePct))
		}
		g.Allowed = len(g.Failed) == 0
		return g
	}
	return &HardEntryGate{Long: evaluate(true), Short: evaluate(false)}
}

// scanRR walks EVERY structural target on the take-profit side across ALL
// timeframe blocks, near→far, deduped within 0.05% (same tolerance as the
// S/R builder), computing each one's RR at the METHODOLOGY stop (structure +
// buffer, band-clamped — the same stop the executor's checkRR validates).
// first_rr_ge_target is the
// rule-mandated pick (nearest qualifying level); best_rr is the definitive
// upper bound used to declare the RR gate structurally failed.
func scanRR(entry float64, basis string, stopPct float64, stopPrice float64, tfs map[string]*TFSignal, isLong bool, minRR float64) *RRScan {
	var targets []float64
	for _, tf := range tfs {
		if tf == nil {
			continue
		}
		src := tf.Resistance
		if !isLong {
			src = tf.Support
		}
		for _, l := range src {
			if l <= 0 {
				continue
			}
			if isLong && l > entry {
				targets = append(targets, l)
			}
			if !isLong && l < entry {
				targets = append(targets, l)
			}
		}
	}
	if isLong {
		sort.Float64s(targets) // ascending = near→far above
	} else {
		sort.Sort(sort.Reverse(sort.Float64Slice(targets))) // descending = near→far below
	}
	var uniq []float64
	for _, t := range targets {
		if !inList(uniq, t, entry) {
			uniq = append(uniq, t)
		}
	}
	direction := "long"
	if !isLong {
		direction = "short"
	}
	// 09-19 RR audit: stopPct/stopPrice are the METHODOLOGY stop (structure +
	// buffer, band-clamped — see methodStopPlan), not the noise floor. The
	// gate verdict, the adopted stop_loss and the executor's checkRR all
	// price this same stop, so a passing scan IS a ≥min-RR trade.
	if stopPct <= 0 {
		stopPct = math.Abs(stopPrice-entry) / entry * 100
	}
	out := &RRScan{
		Direction:       direction,
		EntryPrice:      entry,
		EntryBasis:      basis,
		StopDistancePct: round2(stopPct),
		StopPrice:       stopPrice,
		MinRR:           round2(minRR),
		TargetsScanned:  len(uniq),
	}
	best, bestT := 0.0, 0.0
	for _, t := range uniq {
		dist := (t - entry) / entry * 100
		if !isLong {
			dist = (entry - t) / entry * 100
		}
		v := dist / stopPct
		if v > best {
			best, bestT = v, t
		}
		if minRR > 0 && !out.Usable && v >= minRR-1e-9 {
			out.FirstRRGeTarget = t
			out.Usable = true
		}
	}
	if len(uniq) > 0 {
		out.BestTarget = bestT
		out.BestRR = round2(best)
	}
	return out
}

// computeBiasBlock names each directional read by its SOURCE (review point
// 3): the scanner's snapshot stance, the market-structure consensus, and the
// execution window — the model must phrase them as different things, never
// as one "strong short evidence".
func computeBiasBlock(sig *SymbolSignal, opt SignalOptions) *BiasBlock {
	b := &BiasBlock{Scanner: "none"}
	if opt.ScannerBias == "short" || opt.ScannerBias == "long" {
		b.Scanner = opt.ScannerBias
	}
	score := 0
	if sig.SignalConflict != nil {
		score = sig.SignalConflict.DirectionalScore
	}
	switch {
	case score >= 20:
		b.Structure = "long"
	case score <= -20:
		b.Structure = "short"
	default:
		b.Structure = "mixed"
	}
	switch {
	case sig.ExecutionFilter == nil:
		b.Execution = "unknown"
	case sig.ExecutionFilter.LongAllowed && sig.ExecutionFilter.ShortAllowed:
		b.Execution = "both"
	case sig.ExecutionFilter.LongAllowed:
		b.Execution = "long_only"
	case sig.ExecutionFilter.ShortAllowed:
		b.Execution = "short_only"
	default:
		b.Execution = "none"
	}
	return b
}

// computeTFSignal scores one timeframe: closed-candle settlement, trend
// classification, swing S/R, and the indicator features.
func computeTFSignal(tf string, tfData *market.TimeframeSeriesData, now time.Time, anchorPrice float64) (*TFSignal, []string) {
	var warns []string
	dur := tfDuration(tf)

	// ── Closed-candle settlement ──
	klines := tfData.Klines
	dropped := 0
	if len(klines) > 0 {
		last := klines[len(klines)-1]
		if time.UnixMilli(last.Time).Add(dur).After(now) {
			// The most recent candle is still forming — indicators and
			// structure are computed on CLOSED candles only.
			klines = klines[:len(klines)-1]
			dropped = 1
		}
	}
	n := len(klines)
	if n < 10 {
		return &TFSignal{
			Timeframe: tf,
			Trend:     "unknown", LastClosedCandle: "unknown",
			Support: []float64{}, Resistance: []float64{},
			SupportDistPct: []float64{}, ResistanceDistPct: []float64{},
			UnclosedDropped: dropped > 0, BarsUsed: n,
		}, []string{fmt.Sprintf("%s: only %d closed bars after settlement", tf, n)}
	}

	c := make([]float64, n)
	for i, k := range klines {
		c[i] = k.Close
	}
	last := klines[n-1]

	sig := &TFSignal{
		Timeframe:        tf,
		LastClose:        last.Close,
		BarsUsed:         n,
		UnclosedDropped:  dropped > 0,
		LastClosedCandle: candleColor(last),
	}

	// ── Return over this timeframe's lookback (min(20, n-1) bars) ──
	lookback := 20
	if n < lookback+1 {
		lookback = n - 1
	}
	if c[n-1-lookback] > 0 {
		sig.ReturnPct = (c[n-1]/c[n-1-lookback] - 1) * 100
	}
	// Make the return window explicit: 20 bars means a different real-world
	// span per timeframe (5h on 15m vs 20 days on 1d).
	sig.ReturnWindowHrs = float64(lookback) * dur.Hours()

	// ── Timeframes with bar duration ≤ 1h report the previous FULL hour's
	// close-over-close change. Purely closed data — deliberately different
	// from the symbol-level rolling price_change_1h_pct, which anchors on the
	// live price. The Summary labels both. ──
	if tfDuration(tf) <= time.Hour {
		bars1h := int(time.Hour / dur)
		if n > bars1h && c[n-1-bars1h] > 0 {
			r := (c[n-1]/c[n-1-bars1h] - 1) * 100
			sig.PrevHourChangePct = &r
		}
	}

	// ── Indicators: self-computed from the CLOSED bars only — independent of
	// the strategy's prompt-display switches. ──
	kb := barsToKlines(klines)
	atr := wilderATR(kb, 14)
	if atr > 0 && last.Close > 0 {
		sig.ATRPct = atr / last.Close * 100
		// Percentile of the current ATR% within this TF's own ATR history —
		// separates "this coin always moves like this" from "unusual vol".
		if series := wilderATRSeries(kb, 14); len(series) > 5 {
			atrPcts := make([]float64, 0, len(series))
			for i, a := range series {
				barIdx := i + 14 // series[i] is the ATR settled at bar i+period
				if barIdx < len(klines) && klines[barIdx].Close > 0 {
					atrPcts = append(atrPcts, a/klines[barIdx].Close*100)
				}
			}
			if len(atrPcts) > 5 {
				rank := 0
				for _, v := range atrPcts {
					if v <= sig.ATRPct {
						rank++
					}
				}
				pct := float64(rank) / float64(len(atrPcts)) * 100
				sig.ATRPercentile = &pct
			}
		}
	}

	if n >= 6 {
		avgLen := 20
		if n-1 < avgLen {
			avgLen = n - 1
		}
		var sum float64
		for _, k := range klines[n-1-avgLen : n-1] {
			sum += k.Volume
		}
		avg := sum / float64(avgLen)
		if avg > 0 {
			v := last.Volume / avg
			sig.VolumeRatio = &v
		}
	}
	// Absolute volume of the last closed bar and its bar-over-bar change.
	sig.Volume = last.Volume
	if n >= 2 && klines[n-2].Volume > 0 {
		sig.VolumeChangePct = (last.Volume/klines[n-2].Volume - 1) * 100
	}

	// Adaptive EMA periods: a 20/50 pair needs 50+ bars — with thinner
	// history fall back to shorter pairs so the trend is never "unknown".
	fastP, slowP := 20, 50
	if n < 50 {
		fastP, slowP = 10, 20
	}
	if n < 25 {
		fastP, slowP = 6, 12
	}
	fast := market.ExportCalculateEMA(kb, fastP)
	slow := market.ExportCalculateEMA(kb, slowP)
	if fast > 0 && slow > 0 {
		sig.EMAFast = &fast
		sig.EMASlow = &slow
	}
	sig.Trend = classifyTrend(c, fast, slow, fast > 0 && slow > 0)

	if macdLine := market.ExportCalculateMACD(kb); macdLine != 0 {
		prevLine := market.ExportCalculateMACD(kb[:len(kb)-1])
		h := macdLine / last.Close * 100 // price-normalized
		sig.MACDHist = &h
		switch {
		case macdLine > prevLine:
			sig.MACDTrend = "rising"
		case macdLine < prevLine:
			sig.MACDTrend = "falling"
		default:
			sig.MACDTrend = "flat"
		}
	}

	if rsi := market.ExportCalculateRSI(kb, 14); rsi > 0 {
		sig.RSI14 = &rsi
	}
	// StochRSI (14,14,3,3) on the closed bars: stochastic of the Wilder RSI
	// series, %K smoothed 3, %D = 3-SMA of %K. >80 overbought / <20 oversold.
	if k, d, ok := stochRSI(c, 14, 14, 3, 3); ok {
		sig.StochRSIK = &k
		sig.StochRSID = &d
	}

	// ── Swing support / resistance from the last ~30 closed bars ──
	structureWindow := 30
	if n < structureWindow {
		structureWindow = n
	}
	window := klines[n-structureWindow:]

	hi, lo := last.Close, last.Close
	var structureHigh, structureLow float64
	for _, k := range window {
		if k.High > hi {
			hi = k.High
		}
		if k.Low < lo {
			lo = k.Low
		}
	}
	structureHigh = hi
	structureLow = lo
	sig.StructureHigh = &structureHigh
	sig.StructureLow = &structureLow

	// Support/resistance are classified against the live price (anchor), not
	// this timeframe's last closed close — a pivot "high" the live price has
	// already cleared is not resistance.
	anchor := anchorPrice
	if anchor <= 0 {
		anchor = last.Close
	}
	// Structure-vs-live distances: negative high-dist = live price ABOVE the
	// window high (new highs printing, no overhead reference in this window);
	// negative low-dist is the normal state (window low below price). Same
	// (level − anchor)/anchor convention as the S/R dist arrays.
	if anchor > 0 {
		shDist := (structureHigh - anchor) / anchor * 100
		slDist := (structureLow - anchor) / anchor * 100
		sig.StructureHighDistPct = &shDist
		sig.StructureLowDistPct = &slDist
	}

	pivHighs, pivLows := swingPivots(window, 2)
	// Resistance: swing highs above price, nearest first. Starts non-nil so
	// an empty side still marshals as [] — "no level on this side of the
	// live price" is evidence, not an absence of data.
	resist := []float64{}
	for _, p := range pivHighs {
		if p > anchor {
			resist = append(resist, p)
		}
	}
	sort.Float64s(resist)
	if len(resist) > 2 {
		resist = resist[:2]
	}
	// Supplement with Bollinger upper when thin.
	if true {
		if len(tfData.BOLLUpper) > 0 {
			bu := lastNonZero(tfData.BOLLUpper)
			if bu > anchor && !inList(resist, bu, anchor) {
				resist = append(resist, bu)
				sort.Float64s(resist)
				if len(resist) > 2 {
					resist = resist[:2]
				}
			}
		}
	}
	sig.Resistance = resist

	// Support: swing lows below price, nearest first.
	supp := []float64{}
	for _, p := range pivLows {
		if p < anchor {
			supp = append(supp, p)
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(supp)))
	if len(supp) > 2 {
		supp = supp[:2]
	}
	if len(tfData.BOLLLower) > 0 {
		bl := lastNonZero(tfData.BOLLLower)
		if bl < anchor && !inList(supp, bl, anchor) {
			supp = append(supp, bl)
			sort.Sort(sort.Reverse(sort.Float64Slice(supp)))
			if len(supp) > 2 {
				supp = supp[:2]
			}
		}
	}
	sig.Support = supp

	// Pre-computed percentage distances from the live anchor — the model
	// should not do its own arithmetic on levels.
	sig.SupportDistPct = []float64{}
	sig.ResistanceDistPct = []float64{}
	if anchor > 0 {
		for _, lvl := range sig.Support {
			sig.SupportDistPct = append(sig.SupportDistPct, (lvl-anchor)/anchor*100)
		}
		for _, lvl := range sig.Resistance {
			sig.ResistanceDistPct = append(sig.ResistanceDistPct, (lvl-anchor)/anchor*100)
		}
	}

	return sig, warns
}

// classifyTrend: deterministic EMA/close based trend.
func classifyTrend(c []float64, fast, slow float64, hasEMA bool) string {
	last := c[len(c)-1]
	if !hasEMA || fast <= 0 || slow <= 0 {
		return "unknown"
	}
	// Symmetric 4-quadrant classification (audit 09-13): the EMA pair gives
	// the trend context (fast>slow = up-regime, fast<slow = down-regime) and
	// the price-vs-fast-EMA position gives the state — above = trend
	// continuation (up/down), the other side = dip/bounce entry window
	// (pullback/rally). fast≈slow = range.
	// History: the original down condition tested last<slow and the pullback
	// branch tested the inverted EMA-gap sign (unreachable) — uptrend dips
	// fed "range" (fixed 09-11) and downtrend bounces fed "down", leaving
	// shorts without the mirrored entry window (fixed 09-13).
	switch {
	case fast > slow && last > fast:
		return "up"
	case fast > slow:
		return "pullback"
	case fast < slow && last < fast:
		return "down"
	case fast < slow:
		return "rally"
	default:
		return "range"
	}
}

func candleColor(k market.KlineBar) string {
	switch {
	case k.Close > k.Open*1.0005:
		return "bullish"
	case k.Close < k.Open*0.9995:
		return "bearish"
	default:
		return "doji"
	}
}

// swingPivots returns local highs/lows with a 2-bar shoulder on each side.
func swingPivots(k []market.KlineBar, shoulder int) (highs, lows []float64) {
	for i := shoulder; i < len(k)-shoulder; i++ {
		isHigh, isLow := true, true
		for j := i - shoulder; j <= i+shoulder; j++ {
			if j == i {
				continue
			}
			if k[j].High >= k[i].High {
				isHigh = false
			}
			if k[j].Low <= k[i].Low {
				isLow = false
			}
		}
		if isHigh {
			highs = append(highs, k[i].High)
		}
		if isLow {
			lows = append(lows, k[i].Low)
		}
	}
	return highs, lows
}

func inList(list []float64, v, ref float64) bool {
	for _, x := range list {
		if math.Abs(x-v) <= ref*0.0005 {
			return true
		}
	}
	return false
}

func lastNonZero(xs []float64) float64 {
	for i := len(xs) - 1; i >= 0; i-- {
		if xs[i] != 0 {
			return xs[i]
		}
	}
	return 0
}

// barsToKlines converts KlineBar slices to the full Kline shape the market
// indicator functions expect.
func barsToKlines(k []market.KlineBar) []market.Kline {
	out := make([]market.Kline, len(k))
	for i, b := range k {
		out[i] = market.Kline{
			OpenTime: b.Time, Open: b.Open, High: b.High, Low: b.Low,
			Close: b.Close, Volume: b.Volume,
		}
	}
	return out
}

// wilderATR computes Wilder-smoothed ATR on closed bars.
func wilderATR(bars []market.Kline, period int) float64 {
	n := len(bars)
	if n == 0 {
		return 0
	}
	tr := make([]float64, n)
	tr[0] = bars[0].High - bars[0].Low
	for i := 1; i < n; i++ {
		pc := bars[i-1].Close
		tr[i] = math.Max(bars[i].High-bars[i].Low,
			math.Max(math.Abs(bars[i].High-pc), math.Abs(bars[i].Low-pc)))
	}
	a := tr[0]
	for i := 1; i < n; i++ {
		if i < period {
			a = a + (tr[i]-a)/float64(i+1)
		} else {
			a = (a*float64(period-1) + tr[i]) / float64(period)
		}
	}
	return a
}

// wilderATRSeries returns the Wilder-smoothed ATR series; entry i corresponds
// to bar i+period (the first complete window lands at bar `period`).
func wilderATRSeries(bars []market.Kline, period int) []float64 {
	n := len(bars)
	if n < period+1 {
		return nil
	}
	tr := make([]float64, n)
	tr[0] = bars[0].High - bars[0].Low
	for i := 1; i < n; i++ {
		pc := bars[i-1].Close
		tr[i] = math.Max(bars[i].High-bars[i].Low,
			math.Max(math.Abs(bars[i].High-pc), math.Abs(bars[i].Low-pc)))
	}
	a := 0.0
	for i := 1; i <= period; i++ {
		a += tr[i]
	}
	a /= float64(period)
	series := []float64{a}
	for i := period + 1; i < n; i++ {
		a = (a*float64(period-1) + tr[i]) / float64(period)
		series = append(series, a)
	}
	return series
}

// wilderRSISeries returns the Wilder RSI series over closes (index-aligned;
// the entry at `period` is the first complete value).
func wilderRSISeries(c []float64, period int) []float64 {
	n := len(c)
	if n < period+1 {
		return nil
	}
	rsi := make([]float64, n)
	var gains, losses float64
	for i := 1; i <= period; i++ {
		d := c[i] - c[i-1]
		if d > 0 {
			gains += d
		} else {
			losses -= d
		}
	}
	ag, al := gains/float64(period), losses/float64(period)
	setRSI := func(i int, ag, al float64) {
		switch {
		case ag == 0 && al == 0:
			rsi[i] = 50 // perfectly flat series — no direction, not overbought
		case al == 0:
			rsi[i] = 100
		default:
			rsi[i] = 100 - 100/(1+ag/al)
		}
	}
	setRSI(period, ag, al)
	for i := period + 1; i < n; i++ {
		d := c[i] - c[i-1]
		g, l := 0.0, 0.0
		if d > 0 {
			g = d
		} else {
			l = -d
		}
		ag = (ag*float64(period-1) + g) / float64(period)
		al = (al*float64(period-1) + l) / float64(period)
		setRSI(i, ag, al)
	}
	return rsi
}

// stochRSI computes the StochRSI %K/%D over closes: raw %K places the latest
// RSI within its `stochPeriod` range; %K is a smoothK-SMA of the raw values
// and %D a smoothD-SMA of %K. ok=false when history is insufficient.
func stochRSI(c []float64, rsiPeriod, stochPeriod, smoothK, smoothD int) (k, d float64, ok bool) {
	rsi := wilderRSISeries(c, rsiPeriod)
	// rsi[0..rsiPeriod-1] are unset zeros — a stochastic window may only
	// start where real RSI values exist, otherwise the zero head forces
	// lo=0 and poisons %K/%D on thin history.
	if len(rsi)-rsiPeriod < stochPeriod {
		return 0, 0, false
	}
	rawK := make([]float64, 0, len(rsi))
	for i := rsiPeriod + stochPeriod - 1; i < len(rsi); i++ {
		hi, lo := rsi[i], rsi[i]
		for j := i - stochPeriod + 1; j <= i; j++ {
			if rsi[j] > hi {
				hi = rsi[j]
			}
			if rsi[j] < lo {
				lo = rsi[j]
			}
		}
		if hi == lo {
			rawK = append(rawK, 50)
		} else {
			rawK = append(rawK, (rsi[i]-lo)/(hi-lo)*100)
		}
	}
	kSeries := smaSeries(rawK, smoothK)
	if len(kSeries) == 0 {
		return 0, 0, false
	}
	dSeries := smaSeries(kSeries, smoothD)
	if len(dSeries) == 0 {
		return 0, 0, false
	}
	return kSeries[len(kSeries)-1], dSeries[len(dSeries)-1], true
}

// smaSeries returns the `period`-simple moving average series (the first
// complete window lands at index period-1).
func smaSeries(xs []float64, period int) []float64 {
	if len(xs) < period {
		return nil
	}
	out := make([]float64, 0, len(xs)-period+1)
	for i := period - 1; i < len(xs); i++ {
		s := 0.0
		for j := i - period + 1; j <= i; j++ {
			s += xs[j]
		}
		out = append(out, s/float64(period))
	}
	return out
}

// primaryTFSignal returns the timeframe signal driving the deterministic
// entry/exit rules: the strategy's configured primary timeframe when it is
// present in the computed block, falling back to the longest timeframe
// (direction context) when the primary data is missing.
func primaryTFSignal(sig *SymbolSignal, primaryTF string) *TFSignal {
	if primaryTF != "" {
		if tf, ok := sig.Timeframes[primaryTF]; ok && tf != nil {
			return tf
		}
	}
	keys := make([]string, 0, len(sig.Timeframes))
	for k := range sig.Timeframes {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool { return tfDuration(keys[i]) > tfDuration(keys[j]) })
	return sig.Timeframes[keys[0]]
}

// TimeframeTrend returns the deterministic trend classification ("up", "down",
// "pullback", "rally", "range", "unknown") for one timeframe of a symbol, or "" when
// the data is unavailable. Used by the trader's hard risk gates (e.g. blocking
// counter-trend shorts on the 1d timeframe).
func TimeframeTrend(data *market.Data, tf string) string {
	if data == nil || data.CurrentPrice <= 0 {
		return ""
	}
	sig, err := ComputeSymbolSignals(data.Symbol, data, SignalOptions{Now: time.Now()})
	if err != nil {
		return ""
	}
	if s, ok := sig.Timeframes[tf]; ok && s != nil {
		return s.Trend
	}
	return ""
}

func tfDuration(tf string) time.Duration {
	return marketTFDuration(tf)
}

// marketTFDuration reads the market package's supported timeframe table,
// defaulting unknown labels to 1h.
func marketTFDuration(tf string) time.Duration {
	if d := market.TimeframeDuration(tf); d > 0 {
		return d
	}
	return time.Hour
}

// rolling1hChangePct measures the live price against the close ~60 minutes
// ago, using the finest ≤1h timeframe available in the block (15m×4 for most
// strategies, 30m×2 or 1h×1 as fallbacks). This works for every listing —
// including tokenized stock/commodity perps that Binance doesn't carry.
func rolling1hChangePct(data *market.Data, now time.Time) float64 {
	if data == nil || data.CurrentPrice <= 0 {
		if data != nil {
			return data.PriceChange1h
		}
		return 0
	}
	bestTf := ""
	bestDur := time.Duration(0)
	for tf := range data.TimeframeData {
		dur := market.TimeframeDuration(tf)
		if dur <= 0 || dur > time.Hour {
			continue
		}
		if bestDur == 0 || dur < bestDur {
			bestTf, bestDur = tf, dur
		}
	}
	if bestTf == "" {
		return data.PriceChange1h
	}
	barsBack := int(time.Hour / bestDur)
	if barsBack < 1 {
		barsBack = 1
	}
	closes := closedCloses(data.TimeframeData[bestTf], now, bestDur)
	idx := len(closes) - 1 - barsBack
	if idx < 0 || closes[idx] <= 0 {
		return data.PriceChange1h
	}
	return (data.CurrentPrice/closes[idx] - 1) * 100
}

// closedCloses returns the closes of bars fully closed at `now`.
func closedCloses(tfData *market.TimeframeSeriesData, now time.Time, dur time.Duration) []float64 {
	kl := ClosedKlines(tfData, now, dur)
	out := make([]float64, 0, len(kl))
	for _, k := range kl {
		out = append(out, k.Close)
	}
	return out
}

// closedKlines drops the still-forming tail candle, mirroring computeTFSignal.
func ClosedKlines(tfData *market.TimeframeSeriesData, now time.Time, dur time.Duration) []market.KlineBar {
	kl := tfData.Klines
	if len(kl) > 0 && time.UnixMilli(kl[len(kl)-1].Time).Add(dur).After(now) {
		kl = kl[:len(kl)-1]
	}
	return kl
}

// btcCorrelation computes Pearson correlation and beta of hourly returns
// (symbol vs BTC), aligning both series from the most recent closed bar.
func btcCorrelation(symCloses, btcCloses []float64) *BTCCorrelation {
	n := len(symCloses)
	if len(btcCloses) < n {
		n = len(btcCloses)
	}
	const maxSamples = 72
	const minReturns = 20
	if n > maxSamples {
		n = maxSamples
	}
	if n < minReturns+1 {
		return nil
	}
	sym := symCloses[len(symCloses)-n:]
	btc := btcCloses[len(btcCloses)-n:]
	var rs, rb []float64
	for i := 1; i < n; i++ {
		if sym[i-1] > 0 && btc[i-1] > 0 {
			rs = append(rs, sym[i]/sym[i-1]-1)
			rb = append(rb, btc[i]/btc[i-1]-1)
		}
	}
	if len(rs) < minReturns {
		return nil
	}
	var ms, mb float64
	for i := range rs {
		ms += rs[i]
		mb += rb[i]
	}
	ms /= float64(len(rs))
	mb /= float64(len(rb))
	var cov, vs, vb float64
	for i := range rs {
		cov += (rs[i] - ms) * (rb[i] - mb)
		vs += (rs[i] - ms) * (rs[i] - ms)
		vb += (rb[i] - mb) * (rb[i] - mb)
	}
	pearson := 0.0
	if vs > 0 && vb > 0 {
		pearson = cov / (math.Sqrt(vs) * math.Sqrt(vb))
	}
	beta := 0.0
	if vb > 0 {
		beta = cov / vb
	}
	return &BTCCorrelation{Pearson: pearson, Beta: beta, Samples: len(rs), Window: "1h"}
}

// RenderSignalJSON renders one symbol's signal block as compact JSON.
// RenderSignalJSON renders the structured signal as compact JSON with
// decision-grade numeric precision (09-18 token audit ④): raw float64
// marshaling printed 15-18% junk digits (1.5343639999999998,
// -19.394719896973594). Prices keep 6 significant digits; any *_pct /
// *percent* field keeps 2 decimals — both far below the granularity any
// entry/SL/TP decision operates at. Counts/strings/timestamps untouched.
func RenderSignalJSON(sig *SymbolSignal) string {
	data, err := json.Marshal(sig)
	if err != nil {
		return fmt.Sprintf("signal serialization failed: %v", err)
	}
	var generic interface{}
	if json.Unmarshal(data, &generic) != nil {
		return string(data)
	}
	rounded, err := json.Marshal(roundJSONNumbers(generic, ""))
	if err != nil {
		return string(data)
	}
	return string(rounded)
}

// roundJSONNumbers walks decoded JSON and rounds floats by field convention:
// keys ending _pct or containing "percent" → 2 decimals, everything else → 6
// significant digits (funding_rate 0.00033705 survives intact; oi/volume
// bases lose only sub-1-unit noise). Array children inherit their key.
func roundJSONNumbers(v interface{}, key string) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			t[k] = roundJSONNumbers(child, k)
		}
		return t
	case []interface{}:
		for i, child := range t {
			t[i] = roundJSONNumbers(child, key)
		}
		return t
	case float64:
		if strings.HasSuffix(key, "_pct") || strings.Contains(key, "percent") {
			return roundToDecimalPlaces(t, 2)
		}
		return roundSignificant(t, 6)
	default:
		return v
	}
}

// roundToDecimalPlaces rounds to n digits after the decimal point via
// FormatFloat/ParseFloat — math.Round(v*10^n)/10^n re-multiplies the scaling
// error back in (100198×0.001 → 100.19800000000001).
func roundToDecimalPlaces(v float64, n int) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return v
	}
	out, err := strconv.ParseFloat(strconv.FormatFloat(v, 'f', n, 64), 64)
	if err != nil {
		return v
	}
	return out
}

// roundSignificant rounds to n significant digits. Same FormatFloat trick as
// roundToDecimalPlaces: the naive Round(v/pow)*pow reintroduces float noise.
func roundSignificant(v float64, n int) float64 {
	if v == 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return v
	}
	exp := int(math.Floor(math.Log10(math.Abs(v))))
	digits := n - 1 - exp
	if digits > 20 {
		digits = 20
	}
	if digits < -30 {
		digits = -30
	}
	out, err := strconv.ParseFloat(strconv.FormatFloat(v, 'f', digits, 64), 64)
	if err != nil {
		return v
	}
	return out
}

// computeBreakoutState derives the breakout verdict (⑫) from the 1h closed
// bars and the 1h structure level, in the direction the 1h trend implies:
//
//	up   → breakout  vs structure_high (approach/broken/confirmed/fake/retest)
//	down → breakdown vs structure_low  (mirror)
//
// Status semantics:
//
//	below               price has not reached the level
//	approach            within 1×ATR(1h)% of the level
//	broken_unconfirmed  beyond the level but without volume/OI confirmation
//	confirmed           beyond the level WITH volume + OI confirmation
//	fake_break          crossed within the last 8 bars but closed back inside
//	retest_hold         crossed >8 bars ago, pulled back to the level and held
//
// Best-effort: any missing input (no 1h bars, no structure level, no ATR)
// returns nil — the model falls back to reading the raw levels.
func computeBreakoutState(data *market.Data, sig *SymbolSignal) *BreakoutState {
	if data == nil || sig == nil || sig.Price <= 0 {
		return nil
	}
	t1h := sig.Timeframes["1h"]
	if t1h == nil || t1h.StructureHigh == nil || t1h.StructureLow == nil {
		return nil
	}
	isLong := t1h.Trend == "up" || t1h.Trend == "pullback"
	isShort := t1h.Trend == "down" || t1h.Trend == "rally"
	if !isLong && !isShort {
		return nil // no directional call to anchor the level choice
	}
	level := *t1h.StructureHigh
	direction := "breakout"
	if isShort {
		level = *t1h.StructureLow
		direction = "breakdown"
	}
	if level <= 0 {
		return nil
	}

	dist := 0.0
	if isLong {
		dist = (sig.Price - level) / level * 100
	} else {
		dist = (level - sig.Price) / level * 100
	}
	beyond := dist > 0

	// ATR(1h)% for the "approach" band.
	atrPct := t1h.ATRPct // TFSignal.ATRPct is a plain float64 (percent)

	st := &BreakoutState{
		Direction:   direction,
		Level:       level,
		DistancePct: math.Round(dist*100) / 100,
	}
	switch {
	case !beyond:
		st.Status = "below"
		if atrPct > 0 && -dist <= atrPct {
			st.Status = "approach"
		}
		return st
	}

	// Beyond the level: volume + OI confirmation.
	if t1h.VolumeRatio != nil {
		st.VolumeConfirm = *t1h.VolumeRatio >= 1.5
	}
	if sig.Derivatives != nil && sig.Derivatives.OIChange1hPct != nil {
		// OI expanding IN the break direction = new positions driving the
		// move (longs on a breakout, shorts on a breakdown). The old `< 0`
		// for shorts confirmed liquidation-driven declines — inverted
		// (audit 2026-09-11).
		st.OIConfirm = *sig.Derivatives.OIChange1hPct > 0
	}

	// Cross recency + retest/fake detection from the closed 1h bars.
	tfData, ok := data.TimeframeData["1h"]
	if ok && tfData != nil && len(tfData.Klines) >= 16 {
		bars := tfData.Klines
		bars = bars[:len(bars)-1] // drop the forming candle
		n := len(bars)
		crossedIdx := -1
		for i := n - 1; i >= n-8 && i >= 0; i-- {
			crossed := (isLong && bars[i].High > level) || (!isLong && bars[i].Low < level)
			if crossed {
				crossedIdx = i
			}
		}
		olderCross := false
		for i := 0; i < n-8; i++ {
			crossed := (isLong && bars[i].High > level) || (!isLong && bars[i].Low < level)
			if crossed {
				olderCross = true
				break
			}
		}
		backInside := (isLong && bars[n-1].Close < level) || (!isLong && bars[n-1].Close > level)
		switch {
		case crossedIdx >= 0 && backInside:
			st.Status = "fake_break"
		case olderCross && !backInside:
			// crossed a while ago and still beyond — check the retest.
			touched := false
			for i := n - 8; i < n; i++ {
				if isLong && bars[i].Low <= level*(1+0.0025) {
					touched = true
				}
				if !isLong && bars[i].High >= level*(1-0.0025) {
					touched = true
				}
			}
			if touched {
				st.Status = "retest_hold"
			} else {
				st.Status = "extended"
			}
		case st.VolumeConfirm && st.OIConfirm:
			st.Status = "confirmed"
		default:
			st.Status = "broken_unconfirmed"
		}
		return st
	}
	if st.VolumeConfirm && st.OIConfirm {
		st.Status = "confirmed"
	} else {
		st.Status = "broken_unconfirmed"
	}
	return st
}

// suppressAnchorsAgainstStructure zeroes limit_buy_price / limit_sell_price
// when the anchor sits within thresholdPct of the opposite-side structure
// (long: resistance + structure_high above the buy anchor; short: supports
// below the sell anchor). Zero = "no valid anchor this direction" — the
// trader's supply-zone gate would reject the order.
//
// Resistance/support ARRAYS are classified against the LIVE price, so a
// level between the anchor and live itself is invisible to them — yet filling
// the limit right under that level is exactly the supply-zone trap. Raw swing
// pivots close that blind zone, but only on the 1h series: the pivot scan
// used to cover 15m too, and once the threshold moved to the 1h yardstick
// (0.3-1.2%) every 15m minor pivot inside the pullback corridor zeroed the
// anchor — 63% suppression and a whole day without a single open (2026-09-10).
// 15m minors are noise at this scale; the curated 15m/1h arrays stay in.
func suppressAnchorsAgainstStructure(sig *SymbolSignal, data *market.Data, thresholdPct float64) {
	nearestAbove, nearestBelow := 0.0, 0.0
	for _, tfName := range []string{"1h"} {
		tfData, ok := data.TimeframeData[tfName]
		if !ok || tfData == nil || len(tfData.Klines) < 8 {
			continue
		}
		bars := tfData.Klines
		bars = bars[:len(bars)-1] // closed bars only
		highs, lows := swingPivots(bars, 2)
		for _, h := range highs {
			if h > sig.LimitBuyPrice && (nearestAbove == 0 || h < nearestAbove) {
				nearestAbove = h
			}
		}
		for _, l := range lows {
			if l > 0 && l < sig.LimitSellPrice && (nearestBelow == 0 || l > nearestBelow) {
				nearestBelow = l
			}
		}
	}
	// Keep the array levels too (they are pivot-derived anyway, and the
	// execution-side gate checks exactly these).
	for _, tfName := range []string{"15m", "1h"} {
		if t := sig.Timeframes[tfName]; t != nil {
			for _, r := range t.Resistance {
				if r > sig.LimitBuyPrice && (nearestAbove == 0 || r < nearestAbove) {
					nearestAbove = r
				}
			}
			for _, sup := range t.Support {
				if sup > 0 && sup < sig.LimitSellPrice && (nearestBelow == 0 || sup > nearestBelow) {
					nearestBelow = sup
				}
			}
		}
	}
	if sig.LimitBuyPrice > 0 && nearestAbove > 0 {
		if (nearestAbove-sig.LimitBuyPrice)/sig.LimitBuyPrice*100 < thresholdPct {
			sig.LimitBuyPrice = 0
			sig.Warnings = append(sig.Warnings, fmt.Sprintf("limit_buy_price suppressed: anchor within %.1f%% of a swing-high pivot — no breathing room, long limit would be rejected", thresholdPct))
		}
	}
	if sig.LimitSellPrice > 0 && nearestBelow > 0 {
		if (sig.LimitSellPrice-nearestBelow)/sig.LimitSellPrice*100 < thresholdPct {
			sig.LimitSellPrice = 0
			sig.Warnings = append(sig.Warnings, fmt.Sprintf("limit_sell_price suppressed: anchor within %.1f%% of a swing-low pivot — no breathing room, short limit would be rejected", thresholdPct))
		}
	}
}
