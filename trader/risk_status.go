package trader

import (
	"math"
	"sort"
	"strings"
	"time"

	"nofx/kernel"
	"nofx/market"
)

// RiskStatus is the dashboard's read model of the program-enforced risk state
// (2026-10-07 review F1: the backend computed all of this but the UI showed
// none of it). Read-only: nothing here anchors baselines, places orders or
// mutates trader state.
type RiskStatus struct {
	GeneratedAt time.Time `json:"generated_at"`
	Equity      float64   `json:"equity"`

	// Daily-loss halt (UTC day, baseline = first equity seen that day).
	DayStartEquity  float64 `json:"day_start_equity"`
	DailyLossPct    float64 `json:"daily_loss_pct"`
	DailyLossCapPct float64 `json:"daily_loss_cap_pct"` // 0 = disabled
	DailyHalted     bool    `json:"daily_halted"`

	// Account drawdown breaker (vs initial balance).
	InitialBalance       float64 `json:"initial_balance"`
	AccountDrawdownPct   float64 `json:"account_drawdown_pct"`
	AccountDrawdownCap   float64 `json:"account_drawdown_cap_pct"` // 0 = disabled
	AccountBreakerActive bool    `json:"account_breaker_active"`

	ReduceOnly      bool   `json:"reduce_only"`
	ProtectionFault string `json:"protection_fault,omitempty"`

	LossStreakEnabled bool              `json:"loss_streak_enabled"`
	LossStreakMax     int               `json:"loss_streak_max"`
	LossStreakBans    []LossStreakBan   `json:"loss_streak_bans"`
	Pending           []PendingStatus   `json:"pending_entries"`
	Positions         []PositionRStatus `json:"positions"`
}

type LossStreakBan struct {
	Symbol string    `json:"symbol"`
	Until  time.Time `json:"until"`
}

type PendingStatus struct {
	Symbol     string    `json:"symbol"`
	Side       string    `json:"side"`
	Price      float64   `json:"price"`
	Quantity   float64   `json:"quantity"`
	StopLoss   float64   `json:"stop_loss"`
	TakeProfit float64   `json:"take_profit"`
	PlacedAt   time.Time `json:"placed_at"`
	FilledQty  float64   `json:"filled_qty"`
}

// PositionRStatus is one open AI position in R units against its opening stop.
type PositionRStatus struct {
	Symbol         string  `json:"symbol"`
	Side           string  `json:"side"`
	EntryPrice     float64 `json:"entry_price"`
	MarkPrice      float64 `json:"mark_price"`
	InitialStop    float64 `json:"initial_stop"`
	CurrentStop    float64 `json:"current_stop"`
	CurrentR       float64 `json:"current_r"`
	StopLockedR    float64 `json:"stop_locked_r"` // R guaranteed by the current stop (≥0 = breakeven armed)
	ExitMode       string  `json:"exit_mode"`
	AIManaged      bool    `json:"ai_managed"`
	HasInitialStop bool    `json:"has_initial_stop"`
}

// RiskStatus assembles the read model. Exchange reads go through the
// adapters' normal (cached) balance/position calls.
func (at *AutoTrader) RiskStatus() RiskStatus {
	out := RiskStatus{GeneratedAt: time.Now().UTC(), InitialBalance: at.initialBalance}
	if balance, err := at.trader.GetBalance(); err == nil {
		out.Equity = gridAccountEquity(balance)
	}
	if at.config.StrategyConfig != nil {
		rc := at.config.StrategyConfig.RiskControl
		out.DailyLossCapPct = rc.EffectiveDailyMaxLossPct()
		out.AccountDrawdownCap = math.Max(0, rc.AccountMaxDrawdownPct)
		out.LossStreakEnabled = rc.LossStreakBanEnabled
		out.LossStreakMax = rc.LossStreakMaxLosses
		if out.LossStreakMax <= 0 {
			out.LossStreakMax = lossStreakDefaultN
		}
	}
	today := time.Now().UTC().Format("2006-01-02")
	// review 2026-10-07 B2-E: day anchors and pending fields are written under executionMutex.
	// Snapshot under the same lock, then release it before exchange or durable reads.
	mu := at.executionMutex()
	mu.Lock()
	dayStartDay, dayStartEquity := at.dayStartDay, at.dayStartEquity
	out.Pending = []PendingStatus{}
	at.pendingEntriesMu.RLock()
	for _, pe := range at.pendingEntries {
		if pe == nil {
			continue
		}
		out.Pending = append(out.Pending, PendingStatus{
			Symbol: pe.Symbol, Side: pe.Side, Price: pe.Price, Quantity: pe.Quantity,
			StopLoss: pe.StopLoss, TakeProfit: pe.TakeProfit, PlacedAt: pe.PlacedAt.UTC(), FilledQty: pe.ExecutedQty,
		})
	}
	at.pendingEntriesMu.RUnlock()
	mu.Unlock()
	if dayStartDay == today && dayStartEquity > 0 {
		out.DayStartEquity = dayStartEquity
	} else if baseline, ok := at.inheritDailyBaseline(today); ok {
		out.DayStartEquity = baseline
	}
	if out.DayStartEquity > 0 && out.Equity > 0 {
		out.DailyLossPct = (out.DayStartEquity - out.Equity) / out.DayStartEquity * 100
		out.DailyHalted = out.DailyLossCapPct > 0 && out.DailyLossPct >= out.DailyLossCapPct
	}
	if out.InitialBalance > 0 && out.Equity > 0 {
		out.AccountDrawdownPct = (out.InitialBalance - out.Equity) / out.InitialBalance * 100
		out.AccountBreakerActive = out.AccountDrawdownCap > 0 && out.AccountDrawdownPct >= out.AccountDrawdownCap
	}
	at.runtimeMu.RLock()
	out.ReduceOnly = at.safeMode || at.authBlocked
	at.runtimeMu.RUnlock()
	out.ProtectionFault = at.protectionFaultReason()

	out.LossStreakBans = []LossStreakBan{}
	if out.LossStreakEnabled {
		for sym, until := range at.lossStreakBannedMap(out.LossStreakMax) {
			out.LossStreakBans = append(out.LossStreakBans, LossStreakBan{Symbol: sym, Until: until.UTC()})
		}
		sort.Slice(out.LossStreakBans, func(i, j int) bool { return out.LossStreakBans[i].Symbol < out.LossStreakBans[j].Symbol })
	}

	sort.Slice(out.Pending, func(i, j int) bool { return out.Pending[i].PlacedAt.Before(out.Pending[j].PlacedAt) })

	out.Positions = []PositionRStatus{}
	if positions, err := at.trader.GetPositions(); err == nil {
		for _, pos := range positions {
			symbol, _ := pos["symbol"].(string)
			side, _ := pos["side"].(string)
			mark, _ := pos["markPrice"].(float64)
			qty, _ := pos["positionAmt"].(float64)
			entry := posEntryPrice(pos)
			if symbol == "" || qty == 0 || entry <= 0 {
				continue
			}
			side = strings.ToLower(side)
			ps := PositionRStatus{
				Symbol: market.Normalize(symbol), Side: side, EntryPrice: entry, MarkPrice: mark,
				InitialStop: at.GetInitialStopLoss(symbol, side), CurrentStop: at.GetRecordedStopLoss(symbol, side),
				ExitMode: at.ExitModeFor(symbol, side), AIManaged: at.isAIManaged(symbol, side),
			}
			if ps.ExitMode == "" {
				ps.ExitMode = kernel.ExitModeTrend
			}
			if ps.InitialStop <= 0 && at.store != nil {
				if row, err := at.store.Position().GetOpenPositionBySymbol(at.id, symbol, strings.ToUpper(side)); err == nil && row != nil {
					ps.InitialStop = row.InitialStopLoss
				}
			}
			if risk := math.Abs(entry - ps.InitialStop); ps.InitialStop > 0 && risk > 0 {
				ps.HasInitialStop = true
				dir := 1.0
				if side == "short" {
					dir = -1
				}
				if mark > 0 {
					ps.CurrentR = round2((mark - entry) * dir / risk)
				}
				if ps.CurrentStop > 0 {
					ps.StopLockedR = round2((ps.CurrentStop - entry) * dir / risk)
				}
			}
			out.Positions = append(out.Positions, ps)
		}
		sort.Slice(out.Positions, func(i, j int) bool { return out.Positions[i].Symbol < out.Positions[j].Symbol })
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
