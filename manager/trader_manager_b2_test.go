package manager

import (
	"testing"

	"nofx/trader"
)

// review 2026-10-07 B2-2 (P1-2): the competition fan-out used to call
// GetAccountInfo in a bare goroutine — a single panic anywhere in an exchange
// client killed the whole live process. A panicking trader must be converted
// into its own error result while every other trader's data still comes back.
func TestConcurrentTraderDataRecoversPanic(t *testing.T) {
	tm := NewTraderManager()
	bad := &trader.AutoTrader{}
	good := &trader.AutoTrader{}
	setMapInstance(t, tm, "bad", bad)
	setMapInstance(t, tm, "good", good)

	tm.accountInfoFn = func(at *trader.AutoTrader) (map[string]interface{}, error) {
		if at == bad {
			panic("exchange client exploded")
		}
		return map[string]interface{}{"total_equity": 42.0}, nil
	}

	// With the old bare goroutine this call crashed the test process.
	// Results are placed by input index: [0] = bad (panics), [1] = good.
	// Zero-value AutoTraders carry no id/name, so identify by fields.
	results := tm.getConcurrentTraderData([]*trader.AutoTrader{bad, good})

	if len(results) != 2 {
		t.Fatalf("results has %d entries, want 2", len(results))
	}
	if errMsg, ok := results[0]["error"].(string); !ok || errMsg == "" {
		t.Fatalf("panicking trader result = %v, want a populated error field", results[0])
	}
	if _, hasErr := results[1]["error"]; hasErr {
		t.Fatalf("healthy trader result = %v, want no error field", results[1])
	}
	if eq, ok := results[1]["total_equity"].(float64); !ok || eq != 42.0 {
		t.Fatalf("healthy trader total_equity = %v, want 42.0", results[1]["total_equity"])
	}
}
