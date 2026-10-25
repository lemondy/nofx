package breakout

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReview20261009IndependentTrades(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, symbols := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d_symbols", symbols), func(t *testing.T) {
			var signals []BTSignal
			for i := 0; i < 96; i++ { // every 45m over three days
				for symbol := 0; symbol < symbols; symbol++ {
					signals = append(signals, BTSignal{Time: start.Add(time.Duration(i) * 45 * time.Minute), Symbol: fmt.Sprint(symbol), Score: 90})
				}
			}
			// Inputs need not arrive in time order; the helper must not mutate them.
			for i, j := 0, len(signals)-1; i < j; i, j = i+1, j-1 {
				signals[i], signals[j] = signals[j], signals[i]
			}
			before := append([]BTSignal(nil), signals...)
			trades := independentTrades(signals, 80)
			if len(trades) != 3*symbols || !reflect.DeepEqual(signals, before) {
				t.Fatalf("got %d trades, want %d; input mutated=%v", len(trades), 3*symbols, !reflect.DeepEqual(signals, before))
			}
			last := map[string]time.Time{}
			counts := map[string]int{}
			for _, trade := range trades {
				if prev, ok := last[trade.Symbol]; ok && trade.Time.Sub(prev) < 24*time.Hour {
					t.Fatalf("overlapping trades for %s: %v / %v", trade.Symbol, prev, trade.Time)
				}
				last[trade.Symbol] = trade.Time
				counts[trade.Symbol]++
			}
			for symbol, n := range counts {
				if n != 3 {
					t.Fatalf("symbol %s has %d trades, want 3", symbol, n)
				}
			}
		})
	}

	t.Run("filter_before_thinning", func(t *testing.T) {
		signals := []BTSignal{
			{Time: start, Symbol: "X", Score: 50, Ret24h: -2},
			{Time: start.Add(time.Hour), Symbol: "X", Score: 90, Ret24h: 2},
			{Time: start.Add(24 * time.Hour), Symbol: "X", Score: 50, Ret24h: -2},
			{Time: start.Add(25 * time.Hour), Symbol: "X", Score: 90, Ret24h: 2},
		}
		for _, tc := range []struct {
			cutoff, edge float64
			first        time.Time
		}{{45, -2, start}, {80, 2, start.Add(time.Hour)}} {
			trades := independentTrades(signals, tc.cutoff)
			edge, n := cutoffEdge(signals, tc.cutoff)
			if n != 2 || edge != tc.edge || !trades[0].Time.Equal(tc.first) {
				t.Fatalf("cutoff %.0f: trades=%v edge=%v n=%d", tc.cutoff, trades, edge, n)
			}
		}
		if got := bestCutoff(signals, 50, 90, 10, 2); got != 60 {
			t.Fatalf("cutoff must filter before thinning: got %.0f, want 60", got)
		}
	})
}

func review20261009Params(t *testing.T) {
	t.Helper()
	SetParamsPath(filepath.Join(t.TempDir(), "params.json"))
	t.Cleanup(func() { SetParamsPath("data/breakout_params.json") })
}

// Thirty symbols over fourteen days fit the actual replay scale. Returns
// are already net of cost. The higher-score bucket has a genuine edge.
func review20261009Signals(noisyTest bool) []BTSignal {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var signals []BTSignal
	for day := 0; day < 14; day++ {
		for symbol := 0; symbol < 30; symbol++ {
			score, ret := 60.0, -2.0
			if symbol >= 20 {
				score, ret = 90, 3+float64(symbol%2)*0.2
				if noisyTest && day >= 9 {
					ret = 0.1 + 10*float64(1-2*(symbol%2))
				}
			} else if symbol >= 10 {
				score, ret = 85, -1
			}
			signals = append(signals, BTSignal{Time: start.Add(time.Duration(day) * 24 * time.Hour), Symbol: fmt.Sprint(symbol), Score: score, Ret24h: ret})
		}
	}
	return signals
}

func TestReview20261009PseudoReplicationTrap(t *testing.T) {
	review20261009Params(t)
	signals := review20261009Signals(false)[:210] // seven full train days
	start := signals[0].Time.Add(10 * 24 * time.Hour)
	var burst []BTSignal
	for i := 0; i < 40; i++ {
		burst = append(burst, BTSignal{Time: start.Add(time.Duration(i) * 15 * time.Minute), Symbol: "WIN", Score: 90, Ret24h: 3})
	}
	if got := bestCutoff(burst, 72, 90, 5, btMinSample); got != 0 {
		t.Fatalf("40 overlapping labels supplied a selectable train cutoff: %.0f", got)
	}
	signals = append(signals, burst...)
	for i := 0; i < 50; i++ {
		signals = append(signals, BTSignal{Time: start.Add(time.Duration(i) * 15 * time.Minute), Symbol: fmt.Sprintf("NOISE%d", i), Score: 45, Ret24h: 0})
	}
	changes, verified, rejected, trainN, testN, candidate := proposeWalkForward(signals)
	if trainN != 210 || testN != 90 || candidate != nil || len(changes) != 0 || len(verified) != 0 {
		t.Fatalf("pseudo-replication verified: split=%d/%d changes=%v verified=%v candidate=%v", trainN, testN, changes, verified, candidate)
	}
	if !strings.Contains(strings.Join(rejected, "\n"), "test n=1 < 15 (t=0.00)") {
		t.Fatalf("expected independent test sample rejection, got %v", rejected)
	}
}

func TestReview20261009GenuineEdgeResearchOnlyAndPurge(t *testing.T) {
	review20261009Params(t)
	before := GetParams()
	signals := review20261009Signals(false)
	changes, verified, rejected, trainN, testN, candidate := proposeWalkForward(signals)
	// Split at day 9: purge the 24 same-day train signals whose labels cross
	// into test; labels ending exactly at the boundary (day 8) remain legal.
	if trainN != 270 || testN != 126 {
		t.Fatalf("purged split=%d/%d, want 270/126", trainN, testN)
	}
	if candidate == nil || len(changes) == 0 || len(verified) == 0 || candidate.StrongThreshold != 87 {
		t.Fatalf("real edge failed: changes=%v verified=%v rejected=%v candidate=%v", changes, verified, rejected, candidate)
	}
	trades := independentTrades(signals[294:], candidate.StrongThreshold)
	tStat := tradeTStatistic(trades)
	if len(trades) != 46 || tStat < 2 {
		t.Fatalf("genuine edge n=%d t=%v", len(trades), tStat)
	}
	want := fmt.Sprintf("(n=%d, t=%.2f)", len(trades), tStat)
	if !strings.Contains(strings.Join(verified, "\n"), want) {
		t.Fatalf("verification missing independent n/t %s: %v", want, verified)
	}
	if !reflect.DeepEqual(GetParams(), before) || !strings.Contains(strings.Join(rejected, "\n"), "LIVE PUBLICATION BLOCKED") {
		t.Fatal("research proposal changed live params or omitted publication block")
	}
	t.Logf("genuine edge: %s", strings.Join(verified, "; "))
}

func TestReview20261009PositiveNoisyMeanRejected(t *testing.T) {
	review20261009Params(t)
	signals := review20261009Signals(true)
	test := signals[294:]
	trades := independentTrades(test, 87)
	edge := avgRet(trades)
	incumbent, incumbentN := cutoffEdge(test, GetParams().StrongThreshold)
	baseline, _ := cutoffEdge(test, btReplayScoreFloor)
	tStat := tradeTStatistic(trades)
	if len(trades) < btVerifySample || incumbentN < btVerifySample || edge <= 0 || edge <= incumbent || edge <= baseline || tStat >= 2 {
		t.Fatalf("fixture must fail only significance: n=%d edge=%v incumbent=%v baseline=%v t=%v", len(trades), edge, incumbent, baseline, tStat)
	}
	changes, verified, rejected, _, _, candidate := proposeWalkForward(signals)
	if candidate != nil || len(changes) != 0 || len(verified) != 0 {
		t.Fatalf("noisy mean verified: changes=%v verified=%v candidate=%v", changes, verified, candidate)
	}
	want := fmt.Sprintf("strong_threshold 87: test t=%.2f < 2 (n=%d", tStat, len(trades))
	if !strings.Contains(strings.Join(rejected, "\n"), want) {
		t.Fatalf("expected t rejection %s, got %v", want, rejected)
	}
	t.Logf("noise: %s", strings.Join(rejected, "; "))
}

func TestReview20261009SampleTStatistic(t *testing.T) {
	// n=3, mean=2, sample variance=1; use n-1, not population variance.
	trades := []BTSignal{{Ret24h: 1}, {Ret24h: 2}, {Ret24h: 3}}
	if got, want := tradeTStatistic(trades), 2*math.Sqrt(3); math.Abs(got-want) > 1e-12 {
		t.Fatalf("t=%v, want %v", got, want)
	}
	for _, tc := range []struct {
		trades []BTSignal
		want   float64
	}{{nil, 0}, {[]BTSignal{{Ret24h: 1}}, 0}, {[]BTSignal{{}, {}}, 0},
		{[]BTSignal{{Ret24h: 1}, {Ret24h: 1}}, math.Inf(1)},
		{[]BTSignal{{Ret24h: -1}, {Ret24h: -1}}, math.Inf(-1)}} {
		if got := tradeTStatistic(tc.trades); got != tc.want {
			t.Fatalf("degenerate t=%v, want %v", got, tc.want)
		}
	}
}
