package trader

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Unified Position Rebuild Algorithm
// All exchanges use this same algorithm to reconstruct position history from trades
// =============================================================================

// openTradeEntry represents an opening trade for position tracking
type openTradeEntry struct {
	Price    float64
	Quantity float64
	Fee      float64
	Time     time.Time
	TradeID  string
}

// positionState tracks open trades for a symbol+side combination
type positionState struct {
	OpenTrades []openTradeEntry
	TotalQty   float64
}

// RebuildPositionsFromTrades reconstructs complete position records from trade history
// This is the unified algorithm used by all exchanges
//
// Algorithm:
// 1. Sort trades by time
// 2. Prefer OrderAction, then hedge PositionSide+Side, then one-way inventory
// 3. Add opening trades to the FIFO inventory
// 4. Match closing trades against that inventory, including breakeven exits
//
// The algorithm handles:
// - Partial opens (multiple trades to build a position)
// - Partial closes (multiple trades to close a position)
// - Both hedge mode (LONG/SHORT) and one-way mode (BOTH)
func RebuildPositionsFromTrades(trades []TradeRecord) []ClosedPnLRecord {
	if len(trades) == 0 {
		return nil
	}

	// Sort trades by time
	sort.SliceStable(trades, func(i, j int) bool {
		return trades[i].Time.Before(trades[j].Time)
	})

	// Track positions by symbol_side
	positions := make(map[string]*positionState)
	var records []ClosedPnLRecord

	for _, trade := range trades {
		if trade.Quantity <= 0 {
			continue
		}
		side, opening := classifyRebuildTrade(trade, positions)
		if side == "" {
			continue // Skip invalid trades
		}

		key := fmt.Sprintf("%s_%s", trade.Symbol, side)
		if positions[key] == nil {
			positions[key] = &positionState{}
		}
		state := positions[key]

		if opening {
			// Opening trade: add to open trades list
			state.OpenTrades = append(state.OpenTrades, openTradeEntry{
				Price:    trade.Price,
				Quantity: trade.Quantity,
				Fee:      trade.Fee,
				Time:     trade.Time,
				TradeID:  trade.TradeID,
			})
			state.TotalQty += trade.Quantity
		} else {
			// Closing trade: generate position record
			record := buildClosedPosition(trade, side, state)
			if record != nil {
				records = append(records, *record)
			}
		}
	}

	return records
}

// Explicit exchange intent takes precedence over inferred direction. In hedge
// mode a SELL/LONG or BUY/SHORT is a close even when realized PnL is zero.
func classifyRebuildTrade(trade TradeRecord, positions map[string]*positionState) (side string, opening bool) {
	switch strings.ToLower(trade.OrderAction) {
	case "open_long", "open_long_limit":
		return "long", true
	case "open_short", "open_short_limit":
		return "short", true
	case "close_long", "partial_close_long":
		return "long", false
	case "close_short", "partial_close_short":
		return "short", false
	}
	direction := strings.ToUpper(trade.Side)
	if direction != "BUY" && direction != "SELL" {
		return "", false
	}
	switch strings.ToUpper(trade.PositionSide) {
	case "LONG":
		return "long", direction == "BUY"
	case "SHORT":
		return "short", direction == "SELL"
	}
	// One-way trades without intent close the existing opposite inventory.
	// With incomplete history and no inventory, direction is the only evidence.
	side, opposite := "long", "short"
	if direction == "SELL" {
		side, opposite = "short", "long"
	}
	if p := positions[trade.Symbol+"_"+opposite]; p != nil && p.TotalQty > 0 {
		return opposite, false
	}
	// In incomplete one-way history, a nonzero realized PnL is positive
	// evidence of a close. Zero PnL alone never proves an opening trade.
	if trade.RealizedPnL != 0 {
		return opposite, false
	}
	return side, true
}

// buildClosedPosition builds a closed position record from a closing trade
func buildClosedPosition(trade TradeRecord, side string, state *positionState) *ClosedPnLRecord {
	var entryPrice float64
	var entryTime time.Time
	var totalEntryFee float64

	if len(state.OpenTrades) > 0 {
		// Use FIFO to match open trades
		remainingQty := trade.Quantity
		var weightedSum float64
		var matchedQty float64

		for i := 0; i < len(state.OpenTrades) && remainingQty > 0.00000001; i++ {
			ot := &state.OpenTrades[i]
			matchQty := ot.Quantity
			if matchQty > remainingQty {
				matchQty = remainingQty
			}

			weightedSum += ot.Price * matchQty
			matchedQty += matchQty
			matchedFee := ot.Fee * (matchQty / ot.Quantity)
			totalEntryFee += matchedFee
			ot.Fee -= matchedFee

			if entryTime.IsZero() {
				entryTime = ot.Time
			}

			remainingQty -= matchQty
			ot.Quantity -= matchQty

			// Remove fully consumed open trade
			if ot.Quantity <= 0.00000001 {
				state.OpenTrades = append(state.OpenTrades[:i], state.OpenTrades[i+1:]...)
				i--
			}
		}

		if matchedQty > 0.00000001 {
			entryPrice = weightedSum / matchedQty
		}
		state.TotalQty -= trade.Quantity
	}

	// If no open trades found (history incomplete), calculate entry price from PnL
	if entryPrice == 0 && trade.Quantity > 0 {
		// PnL = (exitPrice - entryPrice) * qty for LONG
		// PnL = (entryPrice - exitPrice) * qty for SHORT
		if side == "long" {
			entryPrice = trade.Price - trade.RealizedPnL/trade.Quantity
		} else {
			entryPrice = trade.Price + trade.RealizedPnL/trade.Quantity
		}
		entryTime = trade.Time // Use exit time as fallback
	}

	// Validate data
	if entryPrice <= 0 || trade.Price <= 0 || trade.Quantity <= 0 {
		return nil
	}

	return &ClosedPnLRecord{
		Symbol:      trade.Symbol,
		Side:        side,
		EntryPrice:  entryPrice,
		ExitPrice:   trade.Price,
		Quantity:    trade.Quantity,
		RealizedPnL: trade.RealizedPnL,
		Fee:         trade.Fee + totalEntryFee,
		EntryTime:   entryTime,
		ExitTime:    trade.Time,
		OrderID:     trade.TradeID,
		ExchangeID:  trade.TradeID,
		CloseType:   "unknown",
	}
}
