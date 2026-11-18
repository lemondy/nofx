package trader

import (
	"errors"
	"nofx/store"
	"testing"
)

type riskReadFailure struct{ review03Mock }

func (*riskReadFailure) GetBalance() (map[string]interface{}, error) {
	return nil, errors.New("balance unavailable")
}
func TestRiskStatusReadFailuresReview(t *testing.T) {
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = &riskReadFailure{review03Mock: review03Mock{positionsErr: errors.New("positions unavailable")}}
	out := at.RiskStatus()
	if !out.EquityUnknown || !out.PositionsUnknown {
		t.Fatalf("unknown flags missing: %+v", out)
	}
}
