package kernel

import "testing"

func TestBatchSameSideMarketLimit(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		got := resolveBatchConflicts([]Decision{{Symbol: "BTCUSDT", Action: "open_" + side}, {Symbol: "BTCUSDT", Action: "open_" + side + "_limit"}})
		if len(got) != 1 || got[0].Action != "open_"+side {
			t.Fatalf("duplicate side passed: %+v", got)
		}
	}
}
