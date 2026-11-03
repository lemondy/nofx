package kernel

import (
	"encoding/json"
	"testing"
)

func TestReplayParseRealShapedResponses(t *testing.T) {
	// Shapes from decision_parse_test.go: reasoning tags, numeric placeholders,
	// empty fenced arrays and completed objects before a truncated final object.
	for _, tc := range []struct {
		response string
		n        int
		action   string
	}{
		{"analysis preamble\n```json [] ```", 0, ""},
		{"<reasoning>逐币检查</reasoning><decision>[{\"symbol\":\"INJUSDT\",\"action\":\"open_long\",\"leverage\":5,\"position_size_usd\":255,\"stop_loss\":5.20,\"take_profit\":5.62,\"confidence\":80,\"risk_usd\":?}]</decision>", 1, "open_long"},
		{`<reasoning>检查持仓</reasoning><decision>[{"symbol":"CLUSDT","action":"close_long"},{"symbol":"BULLAUSDT","action":"wait"`, 1, "close_long"},
		{`[{"symbol":"ETHUSDT","action":"hold"}]`, 1, "hold"},
	} {
		ds, err := ParseReplayDecisions(tc.response, 0)
		if err != nil || len(ds) != tc.n {
			t.Fatalf("parse %q: %+v %v", tc.response, ds, err)
		}
		if tc.n > 0 && ds[0].Action != tc.action {
			t.Fatalf("got %+v", ds)
		}
	}
}

func TestReplayNeutralCapsAndBatchConflicts(t *testing.T) {
	// Reuse the existing production batch fixture, with size below the live
	// default minimum and leverage above the live default cap.
	d := vfBatchDecision("BTCUSDT", "open_long_limit")
	d.PositionSizeUSD, d.Leverage = 1, 100
	b, _ := json.Marshal([]Decision{d, d})
	ds, err := ParseReplayDecisions(string(b), 1)
	if err != nil || len(ds) != 1 || ds[0].Leverage != 100 || ds[0].Price != 100 {
		t.Fatalf("neutral replay: %+v %v", ds, err)
	}
	b, _ = json.Marshal([]Decision{d, vfBatchDecision("BTCUSDT", "open_short")})
	ds, err = ParseReplayDecisions(string(b), 1)
	if err != nil || len(ds) != 2 || ds[0].Action != "wait" || ds[1].Action != "wait" {
		t.Fatalf("conflicts: %+v %v", ds, err)
	}
	d.StopLoss = 0
	b, _ = json.Marshal([]Decision{d})
	if _, err := ParseReplayDecisions(string(b), 1); err == nil {
		t.Fatal("missing SL must not be repaired from an absent gate")
	}
}
