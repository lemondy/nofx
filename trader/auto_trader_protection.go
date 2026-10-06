package trader

import (
	"fmt"
	"math"
	"strings"

	"nofx/trader/types"
)

// Hedge-mode legs close only their position side. One-way legs must carry
// reduce-only/close-position semantics so a stale trigger cannot add risk.
func orderClosesSide(o types.OpenOrder, side string) bool {
	want := strings.ToUpper(side)
	ps := strings.ToUpper(o.PositionSide)
	if ps != "" && ps != "BOTH" && ps != want {
		return false
	}
	exitSide := "SELL"
	if want == "SHORT" {
		exitSide = "BUY"
	}
	if o.Side != "" && !strings.EqualFold(o.Side, exitSide) {
		return false
	}
	if ps == "" || ps == "BOTH" {
		return o.ReduceOnly || o.ClosePosition
	}
	return true
}

func protectionPrice(orders []types.OpenOrder, side, kind string) float64 {
	price := 0.0
	for _, o := range orders {
		if !protectiveOrderMatches(o, side, kind, o.StopPrice) {
			continue
		}
		// Prefer the tightest stop / nearest target. Only that price's
		// actual coverage is credited by the reconciler.
		higher := strings.EqualFold(side, "LONG") == (kind == "SL")
		if price == 0 || (higher && o.StopPrice > price) || (!higher && o.StopPrice < price) {
			price = o.StopPrice
		}
	}
	return price
}

func enoughProtection(orders []types.OpenOrder, side, kind string, price, quantity float64) bool {
	return quantity > 0 && price > 0 && protectiveCoverage(orders, side, kind, price)+math.Max(1e-9, quantity*1e-6) >= quantity
}

func (at *AutoTrader) setProtectionFault(key, reason string) {
	at.runtimeMu.Lock()
	defer at.runtimeMu.Unlock()
	if at.protectionFaults == nil {
		at.protectionFaults = map[string]string{}
	}
	at.protectionFaults[key] = reason
}

func (at *AutoTrader) clearProtectionFaults(live map[string]bool) {
	at.runtimeMu.Lock()
	defer at.runtimeMu.Unlock()
	for key := range at.protectionFaults {
		if strings.HasPrefix(key, "pending:") {
			continue
		}
		if strings.HasPrefix(key, "recovery:") && live[strings.TrimPrefix(key, "recovery:")] {
			continue
		}
		delete(at.protectionFaults, key)
	}
	for key := range at.protectionFailures {
		if !live[key] {
			delete(at.protectionFailures, key)
		}
	}
}
func (at *AutoTrader) clearPendingProtectionFaults() {
	at.runtimeMu.Lock()
	defer at.runtimeMu.Unlock()
	for key := range at.protectionFaults {
		if strings.HasPrefix(key, "pending:") {
			delete(at.protectionFaults, key)
		}
	}
}

func (at *AutoTrader) protectionFaultReason() string {
	for _, peer := range at.accountPeers() {
		peer.runtimeMu.RLock()
		for key, reason := range peer.protectionFaults {
			peer.runtimeMu.RUnlock()
			return fmt.Sprintf("unverified account protection %s: %s", key, reason)
		}
		peer.runtimeMu.RUnlock()
	}
	return ""
}

// Failed stop repair has a bounded retry budget. Keep the fault until a fresh
// snapshot proves coverage or disappearance; close acknowledgements are not proof.
func (at *AutoTrader) protectionFailure(symbol, side, reason string) {
	key := symbol + "_" + side
	at.setProtectionFault(key, reason)
	at.runtimeMu.Lock()
	if at.protectionFailures == nil {
		at.protectionFailures = map[string]int{}
	}
	at.protectionFailures[key]++
	attempts := at.protectionFailures[key]
	at.runtimeMu.Unlock()
	if attempts >= 3 {
		at.markCloseIntent(symbol, side, "stop_loss_recovery")
		if err := at.emergencyClosePosition(symbol, side); err != nil {
			at.alertUnprotectedPosition(symbol, side, "stop repair exhausted; exit failed: "+err.Error())
		}
	}
}

func (at *AutoTrader) protectionVerified(symbol, side string) {
	at.runtimeMu.Lock()
	delete(at.protectionFailures, symbol+"_"+side)
	at.runtimeMu.Unlock()
}
