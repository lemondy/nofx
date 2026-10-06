package trader

import (
	"fmt"
	"math"
	"nofx/market/breakout"
	"strings"
	"time"

	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
)

// Called under the account execution lock. A breaker applies to resting
// entry risk as well as new decisions, across all active strategies.
func (at *AutoTrader) pendingAccountHaltReason() string {
	if reason := at.protectionFaultReason(); reason != "" {
		return reason
	}
	at.invalidateExecutionCache()
	balance, err := at.trader.GetBalance()
	if err != nil {
		return "pending account balance unavailable: " + err.Error()
	}
	equity := gridAccountEquity(balance)
	if equity <= 0 || math.IsNaN(equity) || math.IsInf(equity, 0) {
		return "pending account equity unavailable"
	}
	for _, peer := range at.accountPeers() {
		if peer.config.StrategyConfig == nil {
			continue
		}
		rc := peer.config.StrategyConfig.RiskControl
		if reason := peer.dailyLossHaltBlocks(rc, equity); reason != "" {
			return reason
		}
		if rc.AccountMaxDrawdownPct > 0 && peer.initialBalance > 0 && (peer.initialBalance-equity)/peer.initialBalance*100 >= rc.AccountMaxDrawdownPct {
			return "account drawdown breaker active"
		}
		peer.runtimeMu.RLock()
		reduceOnly := peer.safeMode || peer.authBlocked
		peer.runtimeMu.RUnlock()
		if reduceOnly {
			return "account strategy is in reduce-only mode"
		}
	}
	return ""
}

func (at *AutoTrader) cancelAccountPendingRisk(reason string) {
	at.clearPendingProtectionFaults()
	attempted := map[string]bool{}
	reservations := at.accountPendingEntries()
	for _, peer := range at.accountPeers() {
		for _, key := range peer.snapshotPendingKeys() {
			peer.pendingEntriesMu.RLock()
			pe := peer.pendingEntries[key]
			peer.pendingEntriesMu.RUnlock()
			if pe == nil {
				continue
			}
			attempted[pe.Symbol+"|"+pe.OrderID] = true
			if err := peer.cancelPending(pe); err != nil {
				logger.Warnf("account halt: cancel %s %s failed (%s): %v", pe.Symbol, pe.Side, reason, err)
				at.setProtectionFault("pending:"+pe.Symbol+"_"+pe.Side, "account entry cancellation unverified: "+err.Error())
			}
		}
	}
	// Durable rows from inactive owners and grid reservations still carry entry
	// risk. Cancel only owned, non-reducing IDs; keep owner rows for reconciliation.
	c, ok := at.trader.(interface{ CancelOrder(string, string) error })
	if !ok {
		at.setProtectionFault("pending:account", "account reservation cancellation unsupported")
		return
	}
	for _, pe := range reservations {
		if pe == nil {
			at.setProtectionFault("pending:account", "durable account reservations unavailable")
			continue
		}
		if attempted[pe.Symbol+"|"+pe.OrderID] {
			continue
		}
		orders, err := at.trader.GetOpenOrders(pe.Symbol)
		if err != nil {
			at.setProtectionFault("pending:account", "reserved order snapshot unavailable")
			continue
		}
		reducing := false
		for _, o := range orders {
			if o.OrderID == pe.OrderID && (o.ReduceOnly || o.ClosePosition || (orderClosesSide(o, pe.Side) && (strings.Contains(o.Type, "STOP") || strings.Contains(o.Type, "TAKE_PROFIT")))) {
				reducing = true
			}
		}
		if reducing {
			continue
		}
		if err := c.CancelOrder(pe.Symbol, pe.OrderID); err != nil {
			logger.Warnf("account reserved entry cancel %s: %v", pe.Symbol, err)
		}
		status, err := at.trader.GetOrderStatus(pe.Symbol, pe.OrderID)
		if err != nil {
			at.setProtectionFault("pending:account", "reserved entry final fill unavailable")
			continue
		}
		qty, avg, valid := fillReceipt(status)
		if !valid {
			at.setProtectionFault("pending:account", "reserved entry receipt invalid")
			continue
		}
		st, _ := status["status"].(string)
		if !isTerminalEntryStatus(st) {
			at.setProtectionFault("pending:"+pe.Symbol+"_"+pe.Side, "account entry cancellation not terminal")
		}
		if qty > 0 {
			if pe.StopLoss <= 0 || (pe.Side == "long" && avg <= pe.StopLoss) || (pe.Side == "short" && avg >= pe.StopLoss) {
				at.reservedProtectionFailure(pe.Symbol, pe.Side, "reserved filled entry has invalid stop")
				continue
			}
			if err := ensureProtectiveCoverage(at.trader, pe.Symbol, strings.ToUpper(pe.Side), "SL", pe.StopLoss, qty); err != nil {
				at.reservedProtectionFailure(pe.Symbol, pe.Side, err.Error())
				continue
			}
			// Only restore the full stop for an inactive owner. Its durable
			// row retains the TP plan; this strategy's close fraction cannot
			// substitute for the absent owner's exit policy.
			orders, err = at.trader.GetOpenOrders(pe.Symbol)
			if err != nil || !enoughProtection(orders, pe.Side, "SL", pe.StopLoss, qty) {
				at.reservedProtectionFailure(pe.Symbol, pe.Side, "reserved filled entry SL not verified")
			}
		}
	}

}

func (at *AutoTrader) pendingDirectionBlocked(pe *pendingEntry) string {
	if gs := at.cycleGateStates[market.Normalize(pe.Symbol)]; gs != nil {
		allowed := gs.LongAllowed
		if pe.Side == "short" {
			allowed = gs.ShortAllowed
		}
		if !allowed {
			return "pending direction gate disallowed"
		}
	}
	if at.config.StrategyConfig != nil {
		// Incident 2026-10-07 (29/29 limit entries cancelled within seconds):
		// this recheck used the generic getMarketData path — 100×3m bars
		// aggregated into ~20 15m bars — while ComputeSymbolSignals validates
		// DataQuality at the strategy's 60-bars-per-timeframe minimum. The
		// gap was structural: DATA_INSUFFICIENT fired on EVERY recheck and
		// the monitor cancelled every resting entry moments after placement.
		// The recheck now fetches the SAME strategy-scoped series the AI
		// analysis ran on (selected/primary timeframes × primary count), so
		// a healthy placement passes the same gates it was approved by.
		md, err := at.recheckMarketData(pe.Symbol)
		if err != nil {
			return "pending direction data unavailable: " + err.Error()
		}
		rc := at.config.StrategyConfig.RiskControl
		primaryTF := at.config.StrategyConfig.Indicators.Klines.PrimaryTimeframe
		if primaryTF == "" {
			primaryTF = "15m"
		}
		if pe.Side == "short" && rc.BlockShort1dUptrend && kernel.TimeframeTrend(md, "1d") == "up" {
			return "pending short: 1d trend is up"
		}
		btc := []float64(nil)
		if rc.EffectiveBTCFilterLong() || rc.EffectiveBTCFilterShort() {
			btc = kernel.BTC4hTrendCloses(300)
			if len(btc) < 60 {
				return "pending BTC regime data unavailable"
			}
		}
		sig, err := kernel.ComputeSymbolSignals(pe.Symbol, md, kernel.SignalOptions{
			Now: time.Now(), PrimaryTF: primaryTF, CurrentPrice: md.CurrentPrice,
			EntryTimingGate: rc.EntryTimingGate, PumpGuard4hPct: kernel.PumpGuard4h(&rc),
			BTCFilterLong: rc.EffectiveBTCFilterLong(), BTCFilterShort: rc.EffectiveBTCFilterShort(), BtcTrendCloses: btc,
			LongMaxEMA20DistPct: rc.EffectiveLongMaxEMA20DistPct(), LongPullbackEntry: rc.EffectiveLongPullbackEntry(),
			ConfiguredTimeframes: at.recheckConfiguredTimeframes(pe.Symbol),
		})
		if err != nil || sig == nil || sig.HardGate == nil {
			return "pending fresh gate unavailable"
		}
		gate := sig.HardGate.Long
		if pe.Side == "short" {
			gate = sig.HardGate.Short
		}
		if gate == nil {
			return "pending fresh direction gate unavailable"
		}
		for _, code := range gate.Failed {
			if absoluteBanCode(code) || strings.HasPrefix(code, "MICRO_TREND_") || code == "EXTENDED_PUMP_UNCONFIRMED" {
				return "pending fresh gate: " + code
			}
		}
		if pe.Side == "short" && rc.EffectiveShortTopConfirmGate() {
			shorts, _ := breakout.DefaultScheduler().ShortSnapshot()
			for _, s := range shorts {
				if market.Normalize(s.Symbol) == market.Normalize(pe.Symbol) && !s.Confirmed {
					return "pending short topping confirmation lost"
				}
			}
		}
	}
	return ""
}

func (at *AutoTrader) entryExecutionBlocked(symbol, side string) error {
	if reason := at.protectionFaultReason(); reason != "" {
		return fmt.Errorf("entry blocked: %s", reason)
	}
	if gs := at.cycleGateStates[market.Normalize(symbol)]; gs != nil {
		failed := gs.LongFailed
		if strings.EqualFold(side, "short") {
			failed = gs.ShortFailed
		}
		for _, code := range failed {
			if absoluteBanCode(code) {
				return fmt.Errorf("entry blocked: %s", code)
			}
		}
	}
	return nil
}

func (at *AutoTrader) reservedProtectionFailure(symbol, side, reason string) {
	at.setProtectionFault("pending:"+symbol+"_"+side, reason)
	at.protectionFailure(symbol, side, reason)
}

// recheckMarketData fetches the strategy-configured timeframes for a pending
// recheck — the SAME series shape the AI analysis validated the decision on
// (selected timeframes × primary count), NOT the generic 3m-aggregated quote.
// Field seam so tests can serve synthetic series without a Binance round-trip.
func (at *AutoTrader) recheckMarketData(symbol string) (*market.Data, error) {
	if at.recheckDataFn != nil {
		return at.recheckDataFn(symbol)
	}
	kcfg := at.config.StrategyConfig.Indicators.Klines
	primary := kcfg.PrimaryTimeframe
	if primary == "" {
		primary = "15m"
	}
	count := kcfg.PrimaryCount
	if count <= 0 {
		count = 60
	}
	tfs := at.recheckConfiguredTimeframes(symbol)
	if len(tfs) == 0 {
		tfs = []string{primary}
	}
	return at.getMarketTimeframes(symbol, tfs, primary, count)
}

// recheckConfiguredTimeframes expands the strategy's timeframe list the same
// way the AI cycle does (selected + primary/longer + symbol-required) so the
// recheck's DataQuality bar map matches the decision's exactly.
func (at *AutoTrader) recheckConfiguredTimeframes(symbol string) []string {
	kcfg := at.config.StrategyConfig.Indicators.Klines
	tfs := kcfg.SelectedTimeframes
	if len(tfs) == 0 {
		if p := kcfg.PrimaryTimeframe; p != "" {
			tfs = append(tfs, p)
		}
		if l := kcfg.LongerTimeframe; l != "" {
			tfs = append(tfs, l)
		}
	}
	return kernel.WithRequiredSymbolTimeframes(tfs, symbol)
}
