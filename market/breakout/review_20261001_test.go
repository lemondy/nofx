package breakout

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReview20261001NegativeHoldoutMustPreventLiveParameterChange(t *testing.T) {
	SetParamsPath(filepath.Join(t.TempDir(), "params.json"))
	defer SetParamsPath("data/breakout_params.json")
	prev := GetParams()
	var signals []BTSignal
	for i := 0; i < 100; i++ {
		r := 10.0
		if i >= 70 {
			r = -10
		}
		signals = append(signals, BTSignal{Time: time.Unix(int64(i)*24*3600, 0), Score: 90, Ret24h: r, ATRStrength: 1.6, VolMultiple: 3})
	}
	changes, verified, rejected, _, _ := tuneWalkForward(signals)
	if len(verified) != 0 || len(changes) != 0 || len(rejected) == 0 {
		t.Fatalf("negative holdout must reject a selectable candidate: changes=%v verified=%v rejected=%v", changes, verified, rejected)
	}
	next := GetParams()
	if next.PriceATRCenter != prev.PriceATRCenter || next.VolCenter != prev.VolCenter {
		t.Fatalf("all holdout returns are -10%%, verified=%v rejected=%v, yet live centers changed: %v", verified, rejected, changes)
	}
}
