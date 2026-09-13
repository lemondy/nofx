//go:build live

package breakout

import (
	"fmt"
	"testing"
)

// Live check against real Binance public data:
//
//	go test -tags live ./market/breakout/ -run TestLiveAnalyze -v
func TestLiveAnalyze(t *testing.T) {
	for _, sym := range []string{"BTCUSDT", "ETHUSDT", "BTRUSDT"} {
		rep, err := Analyze(sym, NewBinanceDS(sym))
		if err != nil {
			t.Errorf("%s: %v", sym, err)
			continue
		}
		tf := rep.Timeframes["1h"][rep.Selected]
		fmt.Printf("%s price=%.4f dir=%s score=%.2f(%s) 15m=%.2f/1h=%.2f level=%.4f(%s) α=%.2f β=%.2f volMult=%.2f taker=%.2f\n",
			sym, rep.Price, rep.Selected, rep.SelectedScore, rep.Grade,
			rep.Breakout.Score15m, rep.Breakout.Score1h,
			tf.Level, tf.LevelSource, tf.Alpha, tf.Beta,
			rep.Context.VolMultiple15m, rep.Context.TakerRatio1h)
	}
}

func TestLiveScan(t *testing.T) {
	syms, err := TopVolumeSymbols(5)
	if err != nil {
		t.Fatalf("top symbols: %v", err)
	}
	fmt.Println("top symbols:", syms)
	for _, r := range AnalyzeMany(syms, 3) {
		fmt.Printf("  %s %.4f %s score=%.2f(%s) level=%.4f(%s)\n",
			r.Symbol, r.Price, r.Direction, r.Score, r.Grade, r.Level, r.LevelSource)
	}
}
