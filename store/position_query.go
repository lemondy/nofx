package store

import (
	"fmt"
	"math"
	"nofx/logger"
	"strings"
	"time"

	"nofx/fundsledger"

	"gorm.io/gorm"
)

// TraderStats trading statistics metrics
type TraderStats struct {
	TotalTrades  int     `json:"total_trades"`
	WinTrades    int     `json:"win_trades"`
	LossTrades   int     `json:"loss_trades"`
	WinRate      float64 `json:"win_rate"`
	ProfitFactor float64 `json:"profit_factor"`
	SharpeRatio  float64 `json:"sharpe_ratio"`
	TotalPnL     float64 `json:"total_pnl"`
	TotalFee     float64 `json:"total_fee"`
	TotalFunding float64 `json:"total_funding"` // Allocated funding only, signed.
	TotalNetPnL  float64 `json:"total_net_pnl"` // TotalPnL + TotalFunding; TotalPnL retains its legacy fee-only meaning.
	// Funding rows inside the selected lifecycles that could not be attributed
	// to a single position (overlapping positions on the same exchange+symbol).
	UnallocatedFunding      float64 `json:"unallocated_funding"`
	UnallocatedFundingCount int     `json:"unallocated_funding_count"`
	TotalWin                float64 `json:"total_win"`  // Sum of positive net_pnl, including funding.
	TotalLoss               float64 `json:"total_loss"` // Absolute sum of negative net_pnl, including funding.
	AvgWin                  float64 `json:"avg_win"`
	AvgLoss                 float64 `json:"avg_loss"`
	MaxDrawdownPct          float64 `json:"max_drawdown_pct"`
	// equity_twr: equity curve with external transfers removed;
	// closed_trades: closed-trade PnL curve on initialEquity; unavailable: 0.
	MaxDrawdownBasis string `json:"max_drawdown_basis"`
	// WindowDays: stats only cover trades closed within the last N days.
	// 0 (omitted in JSON) = full history.
	WindowDays int `json:"window_days,omitempty"`
}

// GetPositionStats gets position statistics. total_pnl keeps its legacy
// fee-only meaning; win counting and total_net_pnl include allocated funding.
func (s *PositionStore) GetPositionStats(traderID string) (map[string]interface{}, error) {
	rows, funding, err := s.closedPositionsWithFunding(traderID, 0)
	if err != nil {
		return nil, err
	}
	var wins int
	var pnl, fee, allocated float64
	for _, p := range rows {
		pnl += p.RealizedPnL - p.Fee
		fee += p.Fee
		allocated += p.FundingFee
		if p.NetPnL > 0 {
			wins++
		}
	}
	var rate float64
	if len(rows) > 0 {
		rate = float64(wins) / float64(len(rows)) * 100
	}
	return map[string]interface{}{
		"total_trades":              len(rows),
		"win_trades":                wins,
		"win_rate":                  rate,
		"total_pnl":                 pnl,
		"total_fee":                 fee,
		"total_funding":             allocated,
		"total_net_pnl":             pnl + allocated,
		"unallocated_funding":       funding.Amount,
		"unallocated_funding_count": funding.Count,
	}, nil
}

// GetFullStats gets complete trading statistics
// GetFullStats computes closed-trade statistics. initialEquity is the
// trader's real starting capital, used only for the closed-trade fallback.
// Equity MDD includes open PnL and adjusts confirmed external cash flows.
// Pass 0 when unknown; an unknown capital base is never invented.
func (s *PositionStore) GetFullStats(traderID string, initialEquity float64) (*TraderStats, error) {
	return s.getStats(traderID, initialEquity, 0)
}

// GetRollingStats computes closed-trade statistics over a rolling window:
// only trades closed within the last windowDays days count. windowDays <= 0
// means full history (same basis as GetFullStats). The window is a
// query-time filter — no rows are deleted, moved, or archived, so the full
// history always stays intact in trader_positions.
func (s *PositionStore) GetRollingStats(traderID string, initialEquity float64, windowDays int) (*TraderStats, error) {
	return s.getStats(traderID, initialEquity, windowDays)
}

func (s *PositionStore) getStats(traderID string, initialEquity float64, windowDays int) (*TraderStats, error) {
	stats := &TraderStats{WindowDays: windowDays, MaxDrawdownBasis: "unavailable"}

	var exitCutoff int64
	if windowDays > 0 {
		exitCutoff = time.Now().Add(-time.Duration(windowDays) * 24 * time.Hour).UnixMilli()
	}
	positions, funding, err := s.closedPositionsWithFunding(traderID, exitCutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to query position statistics: %w", err)
	}
	stats.UnallocatedFunding, stats.UnallocatedFundingCount = funding.Amount, funding.Count

	var pnls []float64
	var totalWin, totalLoss float64

	for _, pos := range positions {
		stats.TotalTrades++
		// All outcome metrics (win rate, PF, Sharpe, averages) use net_pnl,
		// including uniquely attributable funding. Legacy total_pnl stays fee-only.
		net := pos.NetPnL
		stats.TotalPnL += pos.RealizedPnL - pos.Fee
		stats.TotalFee += pos.Fee
		stats.TotalFunding += pos.FundingFee
		stats.TotalNetPnL += net
		pnls = append(pnls, net)

		if net > 0 {
			stats.WinTrades++
			totalWin += net
		} else if net < 0 {
			stats.LossTrades++
			totalLoss += -net
		}
	}

	if stats.TotalTrades > 0 {
		stats.WinRate = float64(stats.WinTrades) / float64(stats.TotalTrades) * 100
	}
	stats.TotalWin = totalWin
	stats.TotalLoss = totalLoss
	if totalLoss > 0 {
		stats.ProfitFactor = totalWin / totalLoss
	}
	if stats.WinTrades > 0 {
		stats.AvgWin = totalWin / float64(stats.WinTrades)
	}
	if stats.LossTrades > 0 {
		stats.AvgLoss = totalLoss / float64(stats.LossTrades)
	}
	if len(pnls) > 1 {
		stats.SharpeRatio = calculateSharpeRatioFromPnls(pnls)
	}
	// Max drawdown: prefer the equity curve (includes unrealized swings) with
	// external transfers stripped out (time-weighted index). The closed-trade
	// curve is only a fallback and needs a real capital base: no invented one.
	dd, ok, err := s.equityDrawdown(traderID, exitCutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to compute equity drawdown: %w", err)
	}
	switch {
	case ok:
		stats.MaxDrawdownPct, stats.MaxDrawdownBasis = dd, "equity_twr"
	case len(pnls) > 0 && initialEquity > 0 && !math.IsNaN(initialEquity) && !math.IsInf(initialEquity, 0):
		stats.MaxDrawdownPct = calculateMaxDrawdownFromPnls(pnls, initialEquity)
		stats.MaxDrawdownBasis = "closed_trades"
	}

	return stats, nil
}

// RecentTrade recent trade record
type RecentTrade struct {
	Symbol      string  `json:"symbol"`
	Side        string  `json:"side"`
	EntryPrice  float64 `json:"entry_price"`
	ExitPrice   float64 `json:"exit_price"`
	RealizedPnL float64 `json:"realized_pnl"`
	PnLPct      float64 `json:"pnl_pct"` // PRICE return (no leverage — 09-19 audit)
	// Reconciliation fields: the AI's risk/margin math runs on the notional,
	// fees explain realized-PnL gaps beyond the price return.
	PositionValue      float64 `json:"position_value"`
	Fee                float64 `json:"fee"`
	FundingFee         float64 `json:"funding_fee"` // signed, positive is income
	NetPnL             float64 `json:"net_pnl"`     // realized_pnl - fee + funding_fee
	FundingUnallocated bool    `json:"funding_unallocated"`
	EntryTime          int64   `json:"entry_time"`
	ExitTime           int64   `json:"exit_time"`
	HoldDuration       string  `json:"hold_duration"`
	// CloseReason: the per-cycle exit attribution (stop_loss / trailing_stop
	// / ai_close / take_profit / trend_runner / external …) — the trades
	// endpoint previously couldn't surface it while /positions/history could.
	CloseReason string `json:"close_reason"`
}

// GetRecentTrades gets recent closed trades
func (s *PositionStore) GetRecentTrades(traderID string, limit int) ([]RecentTrade, error) {
	var positions []TraderPosition
	err := s.db.Where("trader_id = ? AND status = ?", traderID, "CLOSED").
		Order("exit_time DESC").
		Limit(limit).
		Find(&positions).Error
	if err != nil {
		return nil, fmt.Errorf("failed to query recent trades: %w", err)
	}
	ptrs := make([]*TraderPosition, len(positions))
	for i := range positions {
		ptrs[i] = &positions[i]
	}
	if _, err := s.allocateFunding(ptrs); err != nil {
		return nil, fmt.Errorf("failed to allocate funding: %w", err)
	}

	var trades []RecentTrade
	for _, pos := range positions {
		t := RecentTrade{
			Symbol:      pos.Symbol,
			CloseReason: pos.CloseReason,
			Side:        strings.ToLower(pos.Side),
			EntryPrice:  pos.EntryPrice,
			ExitPrice:   pos.ExitPrice,
			// Net of fee (user audit 2026-10-01): the stored realized_pnl is
			// the exchange's gross realized PnL — rendering it raw contradicted
			// the dictionary ("已扣手续费") and overstated every winner. The
			// aggregate stats have netted per-trade since the 09-19 audit.
			RealizedPnL:        pos.RealizedPnL - pos.Fee,
			PositionValue:      pos.EntryQuantity * pos.EntryPrice,
			Fee:                pos.Fee,
			FundingFee:         pos.FundingFee,
			NetPnL:             pos.NetPnL,
			FundingUnallocated: pos.FundingUnallocated,
			EntryTime:          pos.EntryTime / 1000, // Convert ms to seconds for API compatibility
		}

		if pos.ExitTime > 0 {
			t.ExitTime = pos.ExitTime / 1000 // Convert ms to seconds
			durationMs := pos.ExitTime - pos.EntryTime
			t.HoldDuration = formatDurationMs(durationMs)
		}

		// 09-19 audit: PRICE return only — the stored leverage has been
		// unreliable (recorded 1x vs actual) and the prompt dictionary now
		// documents PnL% as price return.
		if pos.EntryPrice > 0 {
			if t.Side == "long" {
				t.PnLPct = (pos.ExitPrice - pos.EntryPrice) / pos.EntryPrice * 100
			} else {
				t.PnLPct = (pos.EntryPrice - pos.ExitPrice) / pos.EntryPrice * 100
			}
		}

		trades = append(trades, t)
	}

	return trades, nil
}

// calculateSharpeRatioFromPnls calculates Sharpe ratio
func calculateSharpeRatioFromPnls(pnls []float64) float64 {
	if len(pnls) < 2 {
		return 0
	}

	var sum float64
	for _, pnl := range pnls {
		sum += pnl
	}
	mean := sum / float64(len(pnls))

	var variance float64
	for _, pnl := range pnls {
		variance += (pnl - mean) * (pnl - mean)
	}
	stdDev := math.Sqrt(variance / float64(len(pnls)-1))

	if stdDev == 0 {
		return 0
	}

	return mean / stdDev
}

// calculateMaxDrawdownFromPnls calculates the max drawdown of the cumulative
// realized-PnL curve against the trader's real capital base.
func calculateMaxDrawdownFromPnls(pnls []float64, initialEquity float64) float64 {
	if len(pnls) == 0 {
		return 0
	}
	// No invented base: an unknown/invalid capital base yields no drawdown.
	if initialEquity <= 0 || math.IsNaN(initialEquity) || math.IsInf(initialEquity, 0) {
		return 0
	}

	equity := initialEquity
	peak := initialEquity
	var maxDD float64

	for _, pnl := range pnls {
		equity += pnl
		if equity > peak {
			peak = equity
		}
		if peak > 0 {
			dd := (peak - equity) / peak * 100
			if dd > maxDD {
				maxDD = dd
			}
		}
	}

	return maxDD
}

// SymbolStats per-symbol trading statistics
type SymbolStats struct {
	Symbol      string  `json:"symbol"`
	TotalTrades int     `json:"total_trades"`
	WinTrades   int     `json:"win_trades"`
	WinRate     float64 `json:"win_rate"`
	TotalPnL    float64 `json:"total_pnl"`
	AvgPnL      float64 `json:"avg_pnl"`
	AvgHoldMins float64 `json:"avg_hold_mins"`
}

// GetSymbolStats gets per-symbol trading statistics
func (s *PositionStore) GetSymbolStats(traderID string, limit int) ([]SymbolStats, error) {
	positions, _, err := s.closedPositionsWithFunding(traderID, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to query symbol stats: %w", err)
	}

	// Group by symbol
	symbolMap := make(map[string]*SymbolStats)
	symbolHoldMins := make(map[string][]float64)

	for _, pos := range positions {
		if _, ok := symbolMap[pos.Symbol]; !ok {
			symbolMap[pos.Symbol] = &SymbolStats{Symbol: pos.Symbol}
			symbolHoldMins[pos.Symbol] = []float64{}
		}
		s := symbolMap[pos.Symbol]
		s.TotalTrades++
		// NET caliber (same rationale as GetDirectionStats — gross flatters
		// nothing here, but per-symbol fee drag must be visible). Funding that
		// is uniquely attributable to the position is included (net_pnl).
		net := pos.NetPnL
		s.TotalPnL += net
		if net > 0 {
			s.WinTrades++
		}

		if pos.ExitTime > 0 {
			holdMins := float64(pos.ExitTime-pos.EntryTime) / 60000.0 // ms to minutes
			symbolHoldMins[pos.Symbol] = append(symbolHoldMins[pos.Symbol], holdMins)
		}
	}

	var stats []SymbolStats
	for symbol, s := range symbolMap {
		if s.TotalTrades > 0 {
			s.WinRate = float64(s.WinTrades) / float64(s.TotalTrades) * 100
			s.AvgPnL = s.TotalPnL / float64(s.TotalTrades)
		}
		if len(symbolHoldMins[symbol]) > 0 {
			var totalMins float64
			for _, m := range symbolHoldMins[symbol] {
				totalMins += m
			}
			s.AvgHoldMins = totalMins / float64(len(symbolHoldMins[symbol]))
		}
		stats = append(stats, *s)
	}

	// Sort by TotalPnL descending and limit
	for i := 0; i < len(stats)-1; i++ {
		for j := i + 1; j < len(stats); j++ {
			if stats[j].TotalPnL > stats[i].TotalPnL {
				stats[i], stats[j] = stats[j], stats[i]
			}
		}
	}

	if limit > 0 && len(stats) > limit {
		stats = stats[:limit]
	}

	return stats, nil
}

// HoldingTimeStats holding duration analysis
type HoldingTimeStats struct {
	Range      string  `json:"range"`
	TradeCount int     `json:"trade_count"`
	WinRate    float64 `json:"win_rate"`
	AvgPnL     float64 `json:"avg_pnl"`
}

// GetHoldingTimeStats analyzes performance by holding duration
func (s *PositionStore) GetHoldingTimeStats(traderID string) ([]HoldingTimeStats, error) {
	positions, _, err := s.closedPositionsWithFunding(traderID, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to query holding time stats: %w", err)
	}

	rangeStats := map[string]*struct {
		count    int
		wins     int
		totalPnL float64
	}{
		"<1h":   {},
		"1-4h":  {},
		"4-24h": {},
		">24h":  {},
	}

	for _, pos := range positions {
		if pos.ExitTime == 0 {
			continue
		}
		holdHours := float64(pos.ExitTime-pos.EntryTime) / 3600000.0 // ms to hours

		var rangeKey string
		switch {
		case holdHours < 1:
			rangeKey = "<1h"
		case holdHours < 4:
			rangeKey = "1-4h"
		case holdHours < 24:
			rangeKey = "4-24h"
		default:
			rangeKey = ">24h"
		}

		r := rangeStats[rangeKey]
		r.count++
		// NET caliber (2026-10-03 review): every other aggregate on this
		// store nets the fee — this one fed the history summary's
		// best-hold-range with gross numbers. Includes attributable funding.
		net := pos.NetPnL
		r.totalPnL += net
		if net > 0 {
			r.wins++
		}
	}

	var stats []HoldingTimeStats
	for _, rangeKey := range []string{"<1h", "1-4h", "4-24h", ">24h"} {
		r := rangeStats[rangeKey]
		if r.count > 0 {
			stats = append(stats, HoldingTimeStats{
				Range:      rangeKey,
				TradeCount: r.count,
				WinRate:    float64(r.wins) / float64(r.count) * 100,
				AvgPnL:     r.totalPnL / float64(r.count),
			})
		}
	}

	return stats, nil
}

// DirectionStats long/short performance comparison
type DirectionStats struct {
	Side       string  `json:"side"`
	TradeCount int     `json:"trade_count"`
	WinRate    float64 `json:"win_rate"`
	TotalPnL   float64 `json:"total_pnl"`
	AvgPnL     float64 `json:"avg_pnl"`
}

// GetDirectionStats analyzes long vs short performance.
// NET caliber (user audit 2026-10-01): realized_pnl is the exchange GROSS
// figure — the 177-long book showed −8.39 gross while fees alone were
// 10.14 (net −18.53). Fees decide winners too: a gross winner can be a net
// loser.
func (s *PositionStore) GetDirectionStats(traderID string) ([]DirectionStats, error) {
	positions, _, err := s.closedPositionsWithFunding(traderID, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to query direction stats: %w", err)
	}

	sideStats := make(map[string]*DirectionStats)
	for _, pos := range positions {
		if _, ok := sideStats[pos.Side]; !ok {
			sideStats[pos.Side] = &DirectionStats{Side: pos.Side}
		}
		s := sideStats[pos.Side]
		s.TradeCount++
		net := pos.NetPnL // fee-netted, plus attributable funding
		s.TotalPnL += net
		if net > 0 {
			s.WinRate++
		}
	}

	var stats []DirectionStats
	for _, s := range sideStats {
		if s.TradeCount > 0 {
			s.AvgPnL = s.TotalPnL / float64(s.TradeCount)
			s.WinRate = s.WinRate / float64(s.TradeCount) * 100
		}
		stats = append(stats, *s)
	}

	return stats, nil
}

// GetClosedPositionsSince returns the trader's closed positions whose exit
// time falls within [sinceMs, now], newest first — the raw material for
// streak-based risk gates (loss-streak circuit breaker).
func (s *PositionStore) GetClosedPositionsSince(traderID string, sinceMs int64) ([]TraderPosition, error) {
	var positions []TraderPosition
	err := s.db.Where("trader_id = ? AND status = ? AND exit_time >= ?", traderID, "CLOSED", sinceMs).
		Order("exit_time DESC").
		Find(&positions).Error
	if err != nil {
		return nil, fmt.Errorf("failed to query closed positions since %d: %w", sinceMs, err)
	}
	return positions, nil
}

// fundingSummary totals funding rows that could not be attributed to a single position.
type fundingSummary struct {
	Amount float64
	Count  int
}

func positionFundingKind(p *TraderPosition) string {
	if p.ExchangeType == "paper" {
		return fundsledger.Paper
	}
	return fundsledger.LiveFutures
}

// closedPositionsWithFunding loads the trader's closed positions (exit_time
// ASC, optionally exit_time >= exitCutoff) with funding allocated.
func (s *PositionStore) closedPositionsWithFunding(traderID string, exitCutoff int64) ([]*TraderPosition, fundingSummary, error) {
	q := s.db.Where("trader_id = ? AND status = ?", traderID, "CLOSED")
	if exitCutoff > 0 {
		q = q.Where("exit_time >= ?", exitCutoff)
	}
	var positions []*TraderPosition
	if err := q.Order("exit_time ASC").Find(&positions).Error; err != nil {
		return nil, fundingSummary{}, err
	}
	sum, err := s.allocateFunding(positions)
	return positions, sum, err
}

// allocateFunding fills FundingFee / NetPnL / FundingUnallocated (query-only
// fields, never persisted) on closed positions. A FUNDING_FEE row belongs to a
// position when it has the same exchange_id, symbol and account kind and its
// time_ms lies in [entry_time, exit_time]. If any OTHER position (another
// trader sharing the exchange, the opposite hedge leg, an open position, or an
// adjacent cycle touching the same instant) also covers that instant the owner
// is ambiguous: the row is not allocated, the positions are flagged
// funding_unallocated, and the row is counted once in the returned summary.
// Two bulk queries (funding rows + peer lifecycles) serve all positions.
func (s *PositionStore) allocateFunding(positions []*TraderPosition) (fundingSummary, error) {
	var sum fundingSummary
	var eligible []*TraderPosition
	exchanges, symbols, kinds := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var minStart, maxEnd int64
	for _, p := range positions {
		p.FundingFee, p.FundingUnallocated = 0, false
		p.NetPnL = p.RealizedPnL - p.Fee
		if p.ExchangeID == "" || p.Symbol == "" || p.EntryTime <= 0 || p.ExitTime < p.EntryTime {
			continue
		}
		eligible = append(eligible, p)
		exchanges[p.ExchangeID], symbols[p.Symbol], kinds[positionFundingKind(p)] = true, true, true
		if minStart == 0 || p.EntryTime < minStart {
			minStart = p.EntryTime
		}
		if p.ExitTime > maxEnd {
			maxEnd = p.ExitTime
		}
	}
	if len(eligible) == 0 {
		return sum, nil
	}
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	rows, err := NewAccountIncomeStore(s.db).fundingBetween(keys(exchanges), keys(symbols), keys(kinds), minStart, maxEnd)
	if err != nil {
		// Funding is an enrichment: without it NetPnL stays realized − fee
		// (initialised above). Never fail the position history over it.
		logger.Warnf("funding allocation skipped (income unavailable): %v", err)
		return sum, nil
	}
	if len(rows) == 0 {
		return sum, nil
	}
	// Peer lifecycles on the same exchange+symbol, any trader/side/status.
	// exit_time = 0 (open) is treated as open-ended.
	var peers []TraderPosition
	if err := s.db.Select("id", "exchange_id", "exchange_type", "symbol", "entry_time", "exit_time").
		Where("exchange_id IN ? AND symbol IN ? AND entry_time <= ? AND (exit_time = 0 OR exit_time >= ?)",
			keys(exchanges), keys(symbols), maxEnd, minStart).Find(&peers).Error; err != nil {
		logger.Warnf("funding allocation skipped (peer positions unavailable): %v", err)
		return sum, nil
	}
	mine := map[int64]*TraderPosition{}
	for _, p := range eligible {
		mine[p.ID] = p
	}
	for _, r := range rows {
		var owners []int64
		var ownedByMe []*TraderPosition
		for i := range peers {
			c := &peers[i]
			if c.ExchangeID != r.ExchangeID || c.Symbol != r.Symbol || positionFundingKind(c) != r.AccountKind {
				continue
			}
			if c.EntryTime > r.TimeMS || (c.ExitTime != 0 && c.ExitTime < r.TimeMS) {
				continue
			}
			owners = append(owners, c.ID)
			if p := mine[c.ID]; p != nil {
				ownedByMe = append(ownedByMe, p)
			}
		}
		if len(ownedByMe) == 0 {
			continue
		}
		if len(owners) == 1 {
			ownedByMe[0].FundingFee += r.Amount
			continue
		}
		sum.Amount += r.Amount
		sum.Count++
		for _, p := range ownedByMe {
			p.FundingUnallocated = true
		}
	}
	for _, p := range eligible {
		p.NetPnL = p.RealizedPnL - p.Fee + p.FundingFee
	}
	return sum, nil
}

// equityPoint / cashFlow feed the time-weighted equity index.
type equityPoint struct {
	At     time.Time
	Equity float64
}
type cashFlow struct {
	At     time.Time
	Amount float64 // signed: deposit > 0, withdrawal < 0
}

// equityDrawdown computes max drawdown (%) from equity snapshots with external
// transfers neutralised. ok=false means no usable equity curve (fewer than two
// snapshots / no positive equity), so the caller falls back.
func (s *PositionStore) equityDrawdown(traderID string, exitCutoff int64) (float64, bool, error) {
	start := time.Unix(0, 0)
	if exitCutoff > 0 {
		start = time.UnixMilli(exitCutoff)
	}
	var snaps []EquitySnapshot
	if err := s.db.Select("timestamp", "total_equity").Where("trader_id = ? AND timestamp >= ?", traderID, start).
		Order("timestamp ASC").Find(&snaps).Error; err != nil {
		return 0, false, err
	}
	if len(snaps) < 2 {
		return 0, false, nil
	}
	points := make([]equityPoint, len(snaps))
	for i, sn := range snaps {
		points[i] = equityPoint{At: sn.Timestamp, Equity: sn.TotalEquity}
	}
	flows, err := s.externalCashFlows(traderID, points[0].At, points[len(points)-1].At)
	if err != nil {
		return 0, false, err
	}
	dd, ok := twrMaxDrawdown(points, flows)
	return dd, ok, nil
}

// externalCashFlows returns confirmed deposits/withdrawals on the trader's
// exchange account in (from, to]. Classification is shared with the funds page
// (fundsledger.ClassifyIncome / StableAsset); the account kind follows the
// funds page too (paper exchange -> paper ledger, otherwise live_futures).
// Non-stable assets are skipped. Transfers inferred from reconciliation
// residuals are NOT used: a residual can also be a real loss.
func (s *PositionStore) externalCashFlows(traderID string, from, to time.Time) ([]cashFlow, error) {
	var tr Trader
	if err := s.db.Select("id", "exchange_id").Where("id = ?", traderID).First(&tr).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	if tr.ExchangeID == "" {
		return nil, nil
	}
	kind := fundsledger.LiveFutures
	var ex Exchange
	if err := s.db.Select("id", "exchange_type").Where("id = ?", tr.ExchangeID).First(&ex).Error; err == nil && ex.ExchangeType == "paper" {
		kind = fundsledger.Paper
	}
	incomes, err := NewAccountIncomeStore(s.db).TransfersForKind(tr.ExchangeID, kind, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	var flows []cashFlow
	for _, in := range incomes {
		if fundsledger.ClassifyIncome(in.IncomeType) != "transfer" || !fundsledger.StableAsset(in.Asset) || in.Amount == 0 {
			continue
		}
		flows = append(flows, cashFlow{At: time.UnixMilli(in.TimeMS), Amount: in.Amount})
	}
	return flows, nil
}

// twrMaxDrawdown builds a time-weighted return index from equity snapshots
// (sorted ascending) and cash flows, and returns the max drawdown of that index
// in percent. For each snapshot interval (t0, t1] the return is
// (E1 - E0 - F) / E0 where F is the net external flow in that interval
// (Modified-Dietz with flows at the interval end), so a deposit is not profit
// and a withdrawal is not loss. Intervals with non-positive opening equity
// carry no return information and are skipped. ok=false when no interval
// produced a return.
func twrMaxDrawdown(points []equityPoint, flows []cashFlow) (float64, bool) {
	index, peak, maxDD := 1.0, 1.0, 0.0
	used := false
	fi := 0
	for i := 1; i < len(points); i++ {
		var f float64
		for fi < len(flows) && !flows[fi].At.After(points[i].At) {
			if flows[fi].At.After(points[i-1].At) {
				f += flows[fi].Amount
			}
			fi++
		}
		e0, e1 := points[i-1].Equity, points[i].Equity
		if e0 <= 0 || math.IsNaN(e0) || math.IsNaN(e1) || math.IsInf(e0, 0) || math.IsInf(e1, 0) {
			continue
		}
		r := (e1 - e0 - f) / e0
		if r < -1 {
			r = -1
		}
		index *= 1 + r
		used = true
		if index > peak {
			peak = index
		}
		if dd := (peak - index) / peak * 100; dd > maxDD {
			maxDD = dd
		}
	}
	return maxDD, used
}
