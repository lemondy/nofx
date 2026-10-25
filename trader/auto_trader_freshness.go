package trader

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	notify "nofx/telegram/notify"
)

// Bar-time staleness on the executor side (review 2026-10-20 #8 follow-up).
//
// The signal layer already refuses new entries when a timeframe's newest bar is
// too old or the recent tail has a hole (kernel DATA_STALE_<tf>), but the
// executor re-derives ATR / stop bands / trailing levels from its OWN fetch of
// data.TimeframeData and never consulted market.Data.StaleTimeframes. The
// helpers below apply the same verdict where K-lines price NEW risk:
//
//   - new entries (market + limit): stop-band ATR must come from fresh series;
//   - trailing / vol-target rescale: skipped on a stale ATR yardstick;
//   - grid ATR bounds: fall back to the fixed-% bounds.
//
// Paths that PROTECT an existing position (watchdog naked-stop repair,
// emergency exit, ATR-free R rules) never refuse on stale data: they either
// avoid the stale series or fall back to a K-line-free price.

// dataStaleReasons returns tf → reason for the stale series of a snapshot. It
// trusts the fetch layer's precomputed result and, like the kernel, evaluates a
// hand-built snapshot that carries SampledAt but no precomputed map. Snapshots
// without a sampling clock are not judged (tests / legacy callers).
func dataStaleReasons(data *market.Data) map[string]string {
	if data == nil {
		return nil
	}
	if len(data.StaleTimeframes) > 0 {
		return data.StaleTimeframes
	}
	if data.StaleTimeframes == nil && !data.SampledAt.IsZero() {
		return market.StaleTimeframes(data.Symbol, data.TimeframeData, data.SampledAt)
	}
	return nil
}

// staleAmong returns the stale timeframes (sorted, deduplicated) among tfs
// with their reasons formatted "tf: reason".
func staleAmong(data *market.Data, tfs ...string) (names []string, reasons []string) {
	stale := dataStaleReasons(data)
	if len(stale) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, tf := range tfs {
		if tf == "" || seen[tf] {
			continue
		}
		seen[tf] = true
		if r, ok := stale[tf]; ok {
			names = append(names, tf)
			reasons = append(reasons, tf+": "+r)
		}
	}
	sort.Strings(names)
	sort.Strings(reasons)
	return names, reasons
}

// stopBandTimeframes lists the series the stop-band floor/cap ATR is ACTUALLY
// computed from for this symbol (the first usable timeframe of each fallback
// chain; equity tokens ride the daily scale).
func stopBandTimeframes(symbol string, data *market.Data) []string {
	if market.IsBStockSymbol(symbol) {
		return []string{"1d"}
	}
	return []string{firstATRTimeframe(data, floorATRChain...), firstATRTimeframe(data, capATRChain...)}
}

// entryDataStaleBlock refuses a NEW entry whose stop band / sizing would be
// priced off a stale kline series. Called right before stopBandATRs on every
// open path (market long/short and the limit entry). nil = fresh (or not
// judged).
func (at *AutoTrader) entryDataStaleBlock(decision *kernel.Decision, data *market.Data) error {
	names, reasons := staleAmong(data, stopBandTimeframes(decision.Symbol, data)...)
	if len(names) == 0 {
		return nil
	}
	code := "DATA_STALE_" + strings.Join(names, "_")
	logger.Warnf("🛡️ [%s] %s %s refused: %s — stop band/sizing would be priced off a dead feed (%s)",
		at.name, decision.Action, decision.Symbol, code, strings.Join(reasons, "; "))
	if _, push := at.gateNotifyRecord("datastale-entry:"+decision.Symbol, time.Now()); push {
		notify.Notify("ALERT", at.name, fmt.Sprintf(
			"<b>🛡️ 行情陈旧,拒绝开仓 %s</b>\n<code>%s</code> 被程序阻断(%s)\n<i>%s</i>",
			notify.Escape(decision.Symbol), decision.Action, code, notify.Escape(strings.Join(reasons, "; "))))
	}
	return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %s — kline feed is stale, stop band and sizing cannot be trusted (%s)",
		decision.Action, decision.Symbol, code, strings.Join(reasons, "; "))
}

// atrYardstickStale reports whether the 1h-yardstick series (the one
// oneHourATRPct / dailyATRPct actually reads) is stale for a trailing / vol
// target decision, with a log-ready reason.
func atrYardstickStale(symbol string, data *market.Data) (bool, string) {
	var tf string
	if market.IsBStockSymbol(symbol) {
		tf = "1d"
	} else {
		tf = firstATRTimeframe(data, floorATRChain...)
	}
	if tf == "" {
		return false, ""
	}
	if _, reasons := staleAmong(data, tf); len(reasons) > 0 {
		return true, reasons[0]
	}
	return false, ""
}
