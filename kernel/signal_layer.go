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
	ReturnPct         float64   `json:"trend_window_return_pct"`
	ReturnWindowHrs   float64   `json:"return_window_hours"`                  // real-world span of the trend window (lookback × bar duration)
	PrevHourChangePct *float64  `json:"prev_hour_close_change_pct,omitempty"` // last FULLY CLOSED hour vs the one before (closed data only)
	ATRPct            float64   `json:"atr_pct"`                              // ATR14 / price × 100
	ATRPercentile     *float64  `json:"atr_percentile,omitempty"`             // current ATR% rank within this TF's own ATR history (0-100)
	VolumeRatio       *float64  `json:"volume_ratio"`                         // last closed volume / 20-bar avg
	Volume            float64   `json:"volume"`                               // last CLOSED bar volume, base units (kline source native)
	VolumeChangePct   float64   `json:"volume_change_pct"`                    // last closed volume vs previous closed bar, %
	Support           []float64 `json:"support,omitempty"`                    // nearest swing lows, ascending
	Resistance        []float64 `json:"resistance,omitempty"`                 // nearest swing highs, descending
	SupportDistPct    []float64 `json:"support_dist_pct,omitempty"`           // pre-computed (level-anchor)/anchor×100, index-aligned with Support
	ResistanceDistPct []float64 `json:"resistance_dist_pct,omitempty"`        // pre-computed, index-aligned with Resistance
	MACDHist          *float64  `json:"macd_hist,omitempty"`                  // normalized by price
	MACDTrend         string    `json:"macd_trend,omitempty"`                 // rising / falling / flat
	RSI14             *float64  `json:"rsi,omitempty"`                        // 0-100
	StochRSIK         *float64  `json:"stoch_rsi_k,omitempty"`                // StochRSI %K (14,14,3,3), 0-100
	StochRSID         *float64  `json:"stoch_rsi_d,omitempty"`                // StochRSI %D (3-SMA of K), 0-100
	EMAFast           *float64  `json:"ema_fast,omitempty"`                   // EMA20
	EMASlow           *float64  `json:"ema_slow,omitempty"`                   // EMA50
	LastClosedCandle  string    `json:"last_closed_candle"`                   // bullish / bearish / doji
	LastClose         float64   `json:"last_close"`                           // last CLOSED candle close
	StructureHigh     *float64  `json:"structure_high"`                       // highest close in window
	StructureLow      *float64  `json:"structure_low"`                        // lowest close in window
	BarsUsed          int       `json:"closed_bars_used"`
	UnclosedDropped   bool      `json:"unclosed_dropped"` // forming candle was dropped
}

// DerivSignal carries derivatives context.
type DerivSignal struct {
	FundingRate           *float64 `json:"funding_rate,omitempty"`
	FundingAnnualizedPct  *float64 `json:"funding_annualized_pct,omitempty"` // 预计算: rate × (24/settle_hours) × 365 × 100,按实测结算间隔年化,模型直接使用不再自行年化
	FundingSettleHours    *float64 `json:"funding_settle_hours,omitempty"`   // 实测结算间隔(小时);缺失时年化按 8h 口径
	OIChange1hPct         *float64 `json:"oi_change_1h_pct,omitempty"`          // true 1h change (quant layer)
	OIVsAvgPct            *float64 `json:"oi_vs_avg_pct,omitempty"`             // current OI vs its period average
	PriceChange1hLivePct  *float64 `json:"price_change_60m_live_pct,omitempty"` // LIVE price vs ~60 minutes ago (self-computed from the finest ≤1h klines). NOT a candle-close figure.
	PriceChange24hLivePct *float64 `json:"price_change_24h_live_pct,omitempty"` // LIVE price vs 24 hours ago (rolling, exchange ticker basis)
	OICurrentBase         *float64 `json:"oi_current_base,omitempty"`           // current open interest, base-asset units
	// Crowd-positioning metrics from Binance futures data endpoints:
	LongShortAccountRatio  *float64 `json:"long_short_account_ratio,omitempty"`  // global accounts long/short
	TopTraderPositionRatio *float64 `json:"top_trader_position_ratio,omitempty"` // top-20%-by-position traders long/short
	TakerBuySellRatio      *float64 `json:"taker_buy_sell_ratio,omitempty"`      // taker buy/sell volume, 1h
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
		Mode:    opt.LimitEntryOffsetMode,
		FixedPct: opt.LimitEntryOffsetPct,
		ATRMult: opt.LimitEntryOffsetATRMult,
		MinPct:  opt.LimitEntryOffsetMinPct,
		MaxPct:  opt.LimitEntryOffsetMaxPct,
	}

	sig := &SymbolSignal{
		Symbol:         strings.ToUpper(symbol),
		TimestampUTC:   now.UTC().Format(time.RFC3339),
		Price:          data.CurrentPrice,
		PrimaryTF:      opt.PrimaryTF,
		Timeframes:     map[string]*TFSignal{},
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
		perDay := 3.0
		if data.FundingSettleHours > 0 {
			perDay = 24.0 / data.FundingSettleHours
			if perDay < 1 {
				perDay = 1
			}
		}
		ann := *d.FundingRate * perDay * 365 * 100
		d.FundingAnnualizedPct = &ann
		if data.FundingSettleHours > 0 {
			h := data.FundingSettleHours
			d.FundingSettleHours = &h
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
			Trend: "unknown", LastClosedCandle: "unknown",
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

	pivHighs, pivLows := swingPivots(window, 2)
	// Resistance: swing highs above price, nearest first.
	var resist []float64
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
	var supp []float64
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
func RenderSignalJSON(sig *SymbolSignal) string {
	data, err := json.Marshal(sig)
	if err != nil {
		return fmt.Sprintf("signal serialization failed: %v", err)
	}
	return string(data)
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
