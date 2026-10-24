// Package fundsledger reconciles account equity without performing IO.
package fundsledger

import "time"

const (
	LiveFutures = "live_futures"
	LiveSpot    = "live_spot"
	Paper       = "paper"
)

type Snapshot struct {
	Time               time.Time
	Equity, Unrealized float64
}

// Trade.PnL is gross realized PnL. Fees are positive expenses.
type Trade struct {
	Time      time.Time
	PnL, Fees float64
}

// Income retains the exchange's original type and signed amount.
type Income struct {
	TranID, IncomeType, Category, Symbol, Asset, Info string
	Amount                                            float64
	TimeMS                                            int64
}
type AccountInput struct {
	TraderID, Name, Kind, Exchange string
	InitialBalance                 float64
	Snapshots                      []Snapshot
	Trades                         []Trade
	Income                         []Income
	Sources, Warnings              []string
}
type Options struct {
	Now          time.Time
	Days         int
	Location     *time.Location
	IncludePaper bool
}

type Amounts struct {
	InitialBalance    float64 `json:"initial_balance"`
	CurrentEquity     float64 `json:"current_equity"`
	UnrealizedPnL     float64 `json:"unrealized_pnl"`
	NetTransfers      float64 `json:"net_transfers"`
	TradingPnL        float64 `json:"trading_pnl"`
	Fees              float64 `json:"fees"`
	Funding           float64 `json:"funding"`
	OtherIncome       float64 `json:"other_income"`
	Unexplained       float64 `json:"unexplained"`
	AdjustedPnL       float64 `json:"adjusted_pnl"`
	AdjustedReturnPct float64 `json:"adjusted_return_pct"`
	RawReturnPct      float64 `json:"raw_return_pct"`
}
type Account struct {
	TraderID string `json:"trader_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Exchange string `json:"exchange"`
	Amounts
	FirstDate string   `json:"first_date"`
	LastDate  string   `json:"last_date"`
	Sources   []string `json:"sources"`
	Warnings  []string `json:"warnings"`
}
type DayAmounts struct {
	OpenEquity       float64 `json:"open_equity"`
	CloseEquity      float64 `json:"close_equity"`
	DeltaEquity      float64 `json:"delta_equity"`
	TradingPnL       float64 `json:"trading_pnl"`
	Fees             float64 `json:"fees"`
	Funding          float64 `json:"funding"`
	OtherIncome      float64 `json:"other_income"`
	UnrealizedChange float64 `json:"unrealized_change"`
	NetTransfer      float64 `json:"net_transfer"`
	TransferSource   string  `json:"transfer_source"`
	Unexplained      float64 `json:"unexplained"`
}
type Daily struct {
	Date     string `json:"date"`
	TraderID string `json:"trader_id,omitempty"`
	Name     string `json:"name,omitempty"`
	DayAmounts
}
type Flow struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Date      string    `json:"date"`
	TraderID  string    `json:"trader_id"`
	Name      string    `json:"name"`
	Amount    float64   `json:"amount"`
	Type      string    `json:"type"`
	Source    string    `json:"source"`
	Confirmed bool      `json:"confirmed"`
	Note      string    `json:"note"`
}
type Overview struct {
	GeneratedAt time.Time `json:"generated_at"`
	TZ          string    `json:"tz"`
	Days        int       `json:"days"`
	Totals      Amounts   `json:"totals"`
	Accounts    []Account `json:"accounts"`
	Daily       []Daily   `json:"daily"`
	DailyTotal  []Daily   `json:"daily_total"`
	Flows       []Flow    `json:"flows"`
}
