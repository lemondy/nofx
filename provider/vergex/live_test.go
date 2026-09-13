//go:build live

package vergex

import (
	"fmt"
	"testing"
)

// Live integration check against the real upstream — run with:
//
//	go test -tags live ./provider/vergex/
func TestLiveTrendingSources(t *testing.T) {
	c := NewClient()

	ai, err := c.GetAI500Symbols(10)
	if err != nil {
		t.Fatalf("AI500: %v", err)
	}
	fmt.Printf("AI500: %v\n", ai)

	top, err := c.GetOITopSymbols(10)
	if err != nil {
		t.Fatalf("OI top: %v", err)
	}
	fmt.Printf("OI top: %v\n", top)

	low, err := c.GetOILowSymbols(10)
	if err != nil {
		t.Fatalf("OI low: %v", err)
	}
	fmt.Printf("OI low: %v\n", low)
}

func TestLiveRankings(t *testing.T) {
	c := NewClient()

	oi, err := c.GetOIRanking("1h", 5)
	if err != nil {
		t.Fatalf("OIRanking: %v", err)
	}
	fmt.Printf("OIRanking top=%d low=%d first=%+v\n", len(oi.TopPositions), len(oi.LowPositions), oi.TopPositions[0])

	pr, err := c.GetPriceRanking("1h", 5)
	if err != nil {
		t.Fatalf("PriceRanking: %v", err)
	}
	for dur, d := range pr.Durations {
		fmt.Printf("PriceRanking[%s] top=%d low=%d first_pair=%s delta=%.4f\n",
			dur, len(d.Top), len(d.Low), d.Top[0].Pair, d.Top[0].PriceDelta)
	}

	nf, err := c.GetNetFlowRanking("1h", 5)
	if err != nil {
		t.Fatalf("NetFlowRanking: %v", err)
	}
	fmt.Printf("NetFlowRanking inst_in=%d inst_out=%d first=%+v\n",
		len(nf.InstitutionFutureTop), len(nf.InstitutionFutureLow), nf.InstitutionFutureTop[0])

	snap, err := c.GetCoinSnapshot("BTCUSDT")
	if err != nil {
		t.Fatalf("CoinSnapshot: %v", err)
	}
	fmt.Printf("CoinSnapshot BTCUSDT: price=%v priceChange=%v oi=%v\n",
		snap.Price, snap.PriceChange, snap.OI != nil)
}
