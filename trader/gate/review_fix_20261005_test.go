package gate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gateapi "github.com/gateio/gateapi-go/v6"
)

func TestFix05GateProtectiveTriggerRulesAndSizedOrders(t *testing.T) {
	var submitted gateapi.FuturesPriceTriggeredOrder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 1})
	}))
	defer srv.Close()
	tr := NewGateTrader("fake", "fake")
	tr.client.GetConfig().BasePath = srv.URL
	tr.contractsCache["BTC_USDT"] = &gateapi.Contract{QuantoMultiplier: "0.01"}
	for _, c := range []struct {
		side, kind string
		rule       int32
		size       int64
	}{{"LONG", "SL", 2, -10}, {"SHORT", "SL", 1, 10}, {"LONG", "TP", 1, -10}, {"SHORT", "TP", 2, 10}} {
		var err error
		if c.kind == "SL" {
			err = tr.SetStopLoss("BTCUSDT", c.side, 0.1, 100)
		} else {
			err = tr.SetTakeProfit("BTCUSDT", c.side, 0.1, 100)
		}
		if err != nil {
			t.Fatal(err)
		}
		if submitted.Trigger.Rule != c.rule || submitted.Initial.Size != c.size || !submitted.Initial.ReduceOnly || submitted.Initial.Close {
			t.Fatalf("%s %s incorrect payload: %+v", c.side, c.kind, submitted)
		}
		side, kind, err := gateTriggerIdentity(submitted)
		want := "STOP_MARKET"
		if c.kind == "TP" {
			want = "TAKE_PROFIT_MARKET"
		}
		if err != nil || side != c.side || kind != want {
			t.Fatalf("readback identity %s/%s/%v", side, kind, err)
		}
	}
	if err := tr.SetStopLoss("BTCUSDT", "LONG", 0, 100); err == nil {
		t.Fatal("zero protective quantity silently became one contract")
	}
}
