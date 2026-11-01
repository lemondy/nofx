package kernel

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func vfBatchDecision(symbol, action string) Decision {
	d := Decision{Symbol: symbol, Action: action, Reasoning: "original " + action,
		Leverage: 3, PositionSizeUSD: 100, StopLoss: 95, TakeProfit: 120, Price: 100, CloseFraction: .25}
	if strings.HasPrefix(action, "open_short") {
		d.StopLoss, d.TakeProfit = 105, 80
	}
	return d
}

func TestReview20261028VFBatchConflicts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		actions []string
		want    []string
		code    string
	}{
		{"duplicate open", []string{"open_long", "open_long"}, []string{"open_long"}, ""},
		{"duplicate close", []string{"close_short", "close_short"}, []string{"close_short"}, ""},
		{"close reopen long", []string{"open_long", "close_long"}, []string{"wait", "close_long"}, "BATCH_CONFLICT_CLOSE_REOPEN"},
		{"close reopen short", []string{"close_short", "open_short"}, []string{"close_short", "wait"}, "BATCH_CONFLICT_CLOSE_REOPEN"},
		{"reverse long", []string{"close_long", "open_short"}, []string{"close_long", "open_short"}, ""},
		{"reverse short", []string{"open_long", "close_short"}, []string{"open_long", "close_short"}, ""},
		{"opposing opens", []string{"open_long", "open_short"}, []string{"wait", "wait"}, "BATCH_CONFLICT_OPPOSING_OPENS"},
		{"opposing opens duplicates", []string{"open_short", "open_long", "open_short"}, []string{"wait", "wait"}, "BATCH_CONFLICT_OPPOSING_OPENS"},
		{"partial long", []string{"partial_close_long", "close_long"}, []string{"close_long"}, ""},
		{"partial short", []string{"close_short", "partial_close_short"}, []string{"close_short"}, ""},
		{"other side partial", []string{"partial_close_short", "close_long"}, []string{"partial_close_short", "close_long"}, ""},
		{"stop and close long", []string{"adjust_stop_loss", "close_long"}, []string{"close_long"}, ""},
		{"stop and close short", []string{"close_short", "adjust_stop_loss"}, []string{"close_short"}, ""},
		{"limit reopen", []string{"open_long_limit", "close_long"}, []string{"wait", "close_long"}, "BATCH_CONFLICT_CLOSE_REOPEN"},
		{"opposing limits", []string{"open_long_limit", "open_short_limit"}, []string{"wait", "wait"}, "BATCH_CONFLICT_OPPOSING_OPENS"},
		{"mixed opposing", []string{"open_short_limit", "open_long"}, []string{"wait", "wait"}, "BATCH_CONFLICT_OPPOSING_OPENS"},
		{"limit reversal", []string{"close_short", "open_long_limit"}, []string{"close_short", "open_long_limit"}, ""},
		{"all conflicts", []string{"open_long", "open_short", "partial_close_long", "adjust_stop_loss", "close_long"}, []string{"wait", "wait", "close_long"}, "BATCH_CONFLICT_OPPOSING_OPENS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := make([]Decision, len(tc.actions))
			for i, action := range tc.actions {
				// These all denote the same contract after normalization.
				symbol := []string{"btc-usdt-swap", "BTC_USDT", " btcusdt "}[i%3]
				input[i] = vfBatchDecision(symbol, action)
			}
			body, _ := json.Marshal(input)
			fd, err := parseFullDecisionResponse(string(body), 1000, 10, 5, .5, .5, 12,
				map[string]bool{"BTCUSDT": true}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range fd.Decisions {
				got = append(got, d.Action)
				if d.Action == "wait" {
					if !strings.Contains(strings.Join(d.NoTradeReasons, " "), tc.code) || tc.code == "" {
						t.Errorf("conflict reason missing from decision record: %+v", d)
					}
					if d.PositionSizeUSD != 0 || d.Leverage != 0 || d.Price != 0 || d.StopLoss != 0 || d.TakeProfit != 0 || d.CloseFraction != 0 || d.Stage == "TRIGGERED" {
						t.Errorf("wait retained executable parameters/entry stage: %+v", d)
					}
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("actions=%v want %v", got, tc.want)
			}
			if len(fd.Decisions) > 0 && len(tc.want) == 1 && strings.HasPrefix(tc.name, "duplicate") && fd.Decisions[0].Reasoning != input[0].Reasoning {
				t.Fatal("deduplication did not retain the first decision")
			}
		})
	}
}

func TestReview20261028VFBatchIndependentSymbolsAndRejectedOpen(t *testing.T) {
	input := []Decision{vfBatchDecision("BTCUSDT", "close_long"), vfBatchDecision("ETHUSDT", "open_long"),
		vfBatchDecision("ETHUSDT", "open_short"), vfBatchDecision("SOLUSDT", "open_short")}
	// A validation-rejected opposing open must not suppress a valid open.
	input[2].PositionSizeUSD = 1
	body, _ := json.Marshal(input)
	fd, err := parseFullDecisionResponse(string(body), 1000, 10, 5, .5, .5, 12,
		map[string]bool{"BTCUSDT": true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range fd.Decisions {
		got = append(got, d.Action)
	}
	if !reflect.DeepEqual(got, []string{"close_long", "open_long", "wait", "open_short"}) {
		t.Fatalf("independent symbols / validation placeholders changed: %v", got)
	}
}
