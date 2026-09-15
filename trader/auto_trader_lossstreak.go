package trader

import (
	"fmt"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"time"
)

// Loss-streak circuit breaker: a symbol that closed >= N consecutive losing
// trades within the last 24h is banned from new opens for 24h. Stateless by
// design — the ban is recomputed from the persistent closed-trade record on
// every open decision, so it survives restarts and never needs bookkeeping:
//   - counting starts at the newest closed trade and stops at the first
//     profitable (or break-even) trade — "连续亏损";
//   - only trades that closed within the window count — "24h内";
//   - the ban expires 24h after the THIRD loss of the streak (the moment the
//     rule fired) — "接下来24h不准再开仓". Losses #4, #5… during the ban do
//     not refresh it.
const (
	lossStreakWindow   = 24 * time.Hour
	lossStreakBanFor   = 24 * time.Hour
	lossStreakDefaultN = 3
)

// lossStreakVerdict evaluates the ban from the symbol's closed positions
// (newest first, already window-filtered by the caller). maxLosses <= 0
// disables. Pure so the streak/ban arithmetic is testable without a DB.
func lossStreakVerdict(positions []store.TraderPosition, maxLosses int, nowMs int64) (bool, string) {
	streak, bannedUntil := lossStreakState(positions, maxLosses, nowMs)
	if bannedUntil.IsZero() {
		return false, ""
	}
	remaining := time.Duration(bannedUntil.UnixMilli()-nowMs) * time.Millisecond
	if remaining < 0 {
		remaining = 0
	}
	newestLoss := time.UnixMilli(positions[0].ExitTime).Format("01-02 15:04")
	return true, fmt.Sprintf(
		"loss-streak ban: %d consecutive losses within 24h (newest %s), opens blocked until %s (%.1fh left)",
		streak,
		newestLoss,
		bannedUntil.Format("01-02 15:04"),
		remaining.Hours(),
	)
}

// lossStreakState is the pure core shared by the executor's verdict and the
// prompt-side ban map: walks the tail counting consecutive losses and returns
// the streak plus when the resulting ban expires (zero time = not banned).
func lossStreakState(positions []store.TraderPosition, maxLosses int, nowMs int64) (int, time.Time) {
	if maxLosses <= 0 || len(positions) < maxLosses {
		return 0, time.Time{}
	}
	// Walk the tail: consecutive losses only, newest first.
	streak := 0
	for _, p := range positions {
		if p.ExitTime <= 0 {
			break
		}
		if p.RealizedPnL < 0 {
			streak++
			if streak >= maxLosses {
				break
			}
		} else {
			break
		}
	}
	if streak < maxLosses {
		return streak, time.Time{}
	}
	// The streak fired when its Nth loss closed; the ban runs 24h from that
	// moment. positions[0] is the newest loss, positions[maxLosses-1] is the
	// Nth (triggering) one.
	bannedUntil := time.UnixMilli(positions[maxLosses-1].ExitTime).Add(lossStreakBanFor)
	if nowMs >= bannedUntil.UnixMilli() {
		return streak, time.Time{}
	}
	return streak, bannedUntil
}

// lossStreakBannedMap evaluates the ban for every symbol with closed trades
// in the window, in ONE store pass, for prompt-side injection (kernel.Context.
// LossStreakBanned): the model must read the program's verdict instead of
// self-judging which symbols are banned. Same stateless design as
// lossStreakBlocks — recomputed from the persistent record every cycle.
func (at *AutoTrader) lossStreakBannedMap(maxLosses int) map[string]time.Time {
	if at.store == nil || maxLosses <= 0 {
		return nil
	}
	sinceMs := time.Now().Add(-lossStreakWindow).UnixMilli()
	positions, err := at.store.Position().GetClosedPositionsSince(at.id, sinceMs)
	if err != nil {
		logger.Infof("⚠️ [%s] loss-streak ban map: query failed (fail-open): %v", at.name, err)
		return nil
	}
	nowMs := time.Now().UnixMilli()
	bySymbol := make(map[string][]store.TraderPosition)
	for _, p := range positions {
		if p.ExitTime <= 0 {
			continue
		}
		key := market.Normalize(p.Symbol)
		bySymbol[key] = append(bySymbol[key], p)
	}
	out := make(map[string]time.Time, len(bySymbol))
	for symbol, pos := range bySymbol {
		// Newest first (GetClosedPositionsSince orders by exit_time DESC).
		if _, bannedUntil := lossStreakState(pos, maxLosses, nowMs); !bannedUntil.IsZero() {
			out[symbol] = bannedUntil
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lossStreakBlocks queries this trader's closed positions in the window and
// evaluates the ban for one symbol. Fail-open on any store error.
func (at *AutoTrader) lossStreakBlocks(symbol string, maxLosses int) (bool, string) {
	if at.store == nil {
		return false, ""
	}
	sinceMs := time.Now().Add(-lossStreakWindow).UnixMilli()
	positions, err := at.store.Position().GetClosedPositionsSince(at.id, sinceMs)
	if err != nil {
		return false, ""
	}
	normalized := market.Normalize(symbol)
	filtered := make([]store.TraderPosition, 0, len(positions))
	for _, p := range positions {
		if market.Normalize(p.Symbol) == normalized && p.ExitTime > 0 {
			filtered = append(filtered, p)
		}
	}
	return lossStreakVerdict(filtered, maxLosses, time.Now().UnixMilli())
}
