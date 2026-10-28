package trader

import (
	"fmt"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"time"
)

// review 2026-10-28 #10: evaluate the latest N consecutive losses within
// 24h, and ban for 24h from the newest (triggering) loss. Keep enough history
// to retain that verdict throughout the ban even as older losses age out.
const (
	lossStreakWindow   = 24 * time.Hour
	lossStreakBanFor   = 24 * time.Hour
	lossStreakDefaultN = 3
)

// lossStreakVerdict evaluates the ban from the symbol's closed positions
// (newest first, including the full window plus ban duration). maxLosses <= 0
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
		if p.RealizedPnL-p.Fee < 0 {
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
	// review 2026-10-28 #10: positions are newest first; the latest loss
	// triggers the ban, while the Nth record bounds the formation window.
	if positions[0].ExitTime-positions[maxLosses-1].ExitTime > lossStreakWindow.Milliseconds() {
		return streak, time.Time{}
	}
	bannedUntil := time.UnixMilli(positions[0].ExitTime).Add(lossStreakBanFor)
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
	sinceMs := time.Now().Add(-(lossStreakWindow + lossStreakBanFor)).UnixMilli()
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
		// AI-book attribution (09-28 review P3): order sync ingests the
		// WHOLE exchange account — a manual win on symbol X would reset the
		// AI's forming 3-loss streak and a manual loss would extend it.
		// Only AI-attributed closes feed the breaker.
		if !p.AIManaged {
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
// evaluates the ban for one symbol. Unavailable history blocks new risk.
func (at *AutoTrader) lossStreakBlocks(symbol string, maxLosses int) (bool, string) {
	if maxLosses <= 0 {
		return false, ""
	}
	// review 2026-10-28 #12: an unreadable history cannot verify admission.
	if at.store == nil {
		return true, "LOSS_HISTORY_UNAVAILABLE: position store unavailable"
	}
	sinceMs := time.Now().Add(-(lossStreakWindow + lossStreakBanFor)).UnixMilli()
	positions, err := at.store.Position().GetClosedPositionsSince(at.id, sinceMs)
	if err != nil {
		return true, "LOSS_HISTORY_UNAVAILABLE: " + err.Error()
	}
	normalized := market.Normalize(symbol)
	filtered := make([]store.TraderPosition, 0, len(positions))
	for _, p := range positions {
		// AI-book attribution only (09-28 review P3) — manual closes on the
		// shared account must not reset or extend the AI's streak.
		if market.Normalize(p.Symbol) == normalized && p.ExitTime > 0 && p.AIManaged {
			filtered = append(filtered, p)
		}
	}
	return lossStreakVerdict(filtered, maxLosses, time.Now().UnixMilli())
}
