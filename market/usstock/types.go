// Package usstock is the data layer of the us_stock strategy: the Binance
// spot bStock symbol registry (AAPLBUSDT ↔ AAPL), klines with a per-timeframe
// Yahoo Finance fallback, the US trading calendar/sessions, and the
// bStock-vs-underlying divergence quote.
// Design: docs/architecture/US_STOCK_BSTOCK_DESIGN_2026-10-09.md §3.
//
// Exported API (the contract the strategy engine and run cycle build on):
//
//	ListSymbols(ctx) ([]SymbolInfo, error)               // all tradable bStock pairs, cached 24h
//	LookupSymbol(ctx, symbol string) (SymbolInfo, bool)   // by bStock pair, e.g. "AAPLBUSDT"
//	GetSeries(ctx, symbol, timeframe string, need int, allowYahoo bool) (*Series, error)
//	GetQuote(ctx, symbol string) (*Quote, error)          // bStock price vs underlying reference
//	SessionAt(t time.Time) Session                        // US session at instant t
//	IsTradingDay(t time.Time) bool                        // NYSE trading day (ET date of t)
//	SessionBounds(t time.Time) (open, close time.Time, ok bool) // regular session of t's ET date (handles half days)
package usstock

import "time"

// Supported timeframes for GetSeries.
const (
	TF15m = "15m"
	TF1h  = "1h"
	TF4h  = "4h"
	TF1d  = "1d"
	TF1w  = "1w"
)

// Data sources recorded on a Series.
const (
	SourceBStock = "bstock" // Binance spot klines of the bStock pair
	SourceYahoo  = "yahoo"  // Yahoo Finance chart of the underlying ticker
)

// SymbolInfo is one tradable bStock pair and its spot trading filters.
type SymbolInfo struct {
	Symbol      string  `json:"symbol"`     // "AAPLBUSDT"
	BaseAsset   string  `json:"base_asset"` // "AAPLB"
	Underlying  string  `json:"underlying"` // "AAPL" (Yahoo ticker)
	Status      string  `json:"status"`     // exchange status, "TRADING"
	TickSize    float64 `json:"tick_size"`
	StepSize    float64 `json:"step_size"`
	MinQty      float64 `json:"min_qty"`
	MinNotional float64 `json:"min_notional"`
}

// Bar is one CLOSED candle. OpenTime is UTC.
type Bar struct {
	OpenTime time.Time `json:"open_time"`
	Open     float64   `json:"open"`
	High     float64   `json:"high"`
	Low      float64   `json:"low"`
	Close    float64   `json:"close"`
	Volume   float64   `json:"volume"`
}

// Series is one timeframe of one symbol from a single source (never spliced
// across sources: bStock days are 24/7 UTC days, Yahoo days are sessions).
type Series struct {
	Symbol     string `json:"symbol"`     // bStock pair
	Underlying string `json:"underlying"` // Yahoo ticker
	Timeframe  string `json:"timeframe"`
	Source     string `json:"source"` // SourceBStock | SourceYahoo
	Bars       []Bar  `json:"bars"`   // closed bars only, ascending by OpenTime
}

// Session is the US equity session at an instant (America/New_York).
type Session string

const (
	SessionPre     Session = "pre"     // 04:00–09:30 ET on trading days
	SessionRegular Session = "regular" // 09:30–16:00 ET (13:00 on half days)
	SessionAfter   Session = "after"   // 16:00–20:00 ET (after the regular close)
	SessionClosed  Session = "closed"  // nights, weekends, NYSE holidays
)

// Quote compares the bStock price with the underlying's reference price.
type Quote struct {
	Symbol        string    `json:"symbol"`
	BStockPrice   float64   `json:"bstock_price"`
	RefPrice      float64   `json:"ref_price"`      // Yahoo last (regular, or pre/post when in those sessions)
	RefTime       time.Time `json:"ref_time"`       // timestamp of RefPrice
	RefFresh      bool      `json:"ref_fresh"`      // RefPrice updated within the current session (false when closed/stale)
	DivergencePct float64   `json:"divergence_pct"` // (BStockPrice-RefPrice)/RefPrice*100; 0 when RefPrice unknown
	Session       Session   `json:"session"`
}
