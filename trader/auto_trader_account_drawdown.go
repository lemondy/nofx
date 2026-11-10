package trader

import (
	"fmt"
	"math"

	"nofx/store"
)

// accountPeakEquity uses dailyBaselineKey in an LLM-specific namespace and
// reuses RiskState's monotonic durable peak. observe=false keeps RiskStatus
// read-only. The registry retains observations during transient store failures
// and also supports storeless test instances. Production uses RiskState.
// Cash flows and manual trades are not adjusted. In particular, withdrawals
// can trip this optional breaker, so use peak only on paper/dedicated accounts.
func (at *AutoTrader) accountPeakEquity(equity float64, observe bool) (float64, error) {
	if equity <= 0 || math.IsNaN(equity) || math.IsInf(equity, 0) {
		return 0, fmt.Errorf("account equity unavailable")
	}
	peak := math.Max(at.initialBalance, equity)
	for _, peer := range at.accountPeers() {
		peak = math.Max(peak, peer.initialBalance)
	}
	key := "llm_peak:" + at.dailyBaselineKey()
	dailyBaselineRegMu.Lock()
	registryKey := "peak:" + key
	peak = math.Max(peak, dailyBaselineReg[registryKey].equity)
	if observe {
		dailyBaselineReg[registryKey] = dailyBaselineRecord{equity: peak}
	}
	dailyBaselineRegMu.Unlock()
	if at.store != nil {
		historical, err := at.store.Equity().MaxAccountEquity(at.id, at.userID, at.exchangeID)
		if err != nil {
			return 0, fmt.Errorf("read account equity history: %w", err)
		}
		peak = math.Max(peak, historical)
		if observe {
			return at.store.RiskState().UpdatePeak(key, peak)
		}
		persisted, _, err := at.store.RiskState().DayBaseline(key, "__peak__")
		return math.Max(peak, persisted), err
	}
	return peak, nil
}

// peakDrawdownHaltReason is called only for the opt-in peak basis. Failure to
// verify/persist the peak blocks new risk until the durable source recovers.
func (at *AutoTrader) peakDrawdownHaltReason(rc store.RiskControlConfig, equity float64) string {
	peak, err := at.accountPeakEquity(equity, true)
	if err != nil {
		return "account drawdown from peak unavailable — reduce-only mode: " + err.Error()
	}
	drawdownPct := (peak - equity) / peak * 100
	if rc.AccountMaxDrawdownPct > 0 && drawdownPct >= rc.AccountMaxDrawdownPct {
		return fmt.Sprintf("account drawdown %.2f%% from peak %.2f ≥ cap %.1f%% — reduce-only mode", drawdownPct, peak, rc.AccountMaxDrawdownPct)
	}
	return ""
}
