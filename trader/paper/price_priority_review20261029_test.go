package paper

import (
	"testing"
	"time"

	"nofx/market"
	"nofx/market/bnweight"
)

// Paper execution simulation must fetch 1m bars at Normal priority; Low (the
// history-paging default) is held by the IP weight guard past the 15s client
// timeout and fails every paper mutation.
func TestCandles1mUsesNormalPriority(t *testing.T) {
	prev := getKlinesRange
	t.Cleanup(func() { getKlinesRange = prev })
	var gotPrio bnweight.Priority = -1
	var gotTF string
	getKlinesRange = func(symbol, tf string, start, end time.Time, prio bnweight.Priority) ([]market.Kline, error) {
		gotPrio, gotTF = prio, tf
		return []market.Kline{{OpenTime: start.UnixMilli(), Open: 1, High: 2, Low: 0.5, Close: 1.5}}, nil
	}
	src := &marketPriceSource{}
	rows, err := src.Candles1m("BTCUSDT", time.Now().Add(-3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].High != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	if gotPrio != bnweight.Normal || gotTF != "1m" {
		t.Fatalf("prio=%v tf=%q, want Normal/1m", gotPrio, gotTF)
	}
}
