package kernel

import (
	"testing"
	"time"

	"nofx/market/breakout"
)

// S3: scanner-unconfirmed candidate is accepted when the LIVE funding
// rollover (strategy threshold) is detected.
func TestShortTopConfirmGateLiveRollover(t *testing.T) {
	no := false
	mk := func(rollover *FundingRolloverState) *DirectionGate {
		sig := lpSig(100, 99, 105)
		if rollover != nil {
			sig.Derivatives = &DerivSignal{FundingRollover: rollover}
		}
		opt := lpOpt()
		opt.ShortTopConfirmGate = true
		opt.ShortScanConfirmed = &no
		return computeHardEntryGate(sig, opt).Short
	}
	if hasCode(mk(&FundingRolloverState{Detected: true}), "SHORT_TOP_CONFIRM_MISSING") {
		t.Fatal("live funding_rollover.detected must satisfy the confirmation gate")
	}
	if !hasCode(mk(&FundingRolloverState{Detected: false}), "SHORT_TOP_CONFIRM_MISSING") {
		t.Fatal("no live rollover + scanner unconfirmed must emit the code")
	}
	if !hasCode(mk(nil), "SHORT_TOP_CONFIRM_MISSING") {
		t.Fatal("no derivatives + scanner unconfirmed must emit the code")
	}
}

func TestDedupeShortCandidatesKeepsHigherScore(t *testing.T) {
	in := []CandidateCoin{
		{Symbol: "A", ShortScore: 60}, {Symbol: "B", ShortScore: 70}, {Symbol: "A", ShortScore: 80},
	}
	out := dedupeShortCandidates(in)
	if len(out) != 2 || out[0].Symbol != "B" || out[1].Symbol != "A" || out[1].ShortScore != 80 {
		t.Fatalf("got %+v", out)
	}
}

func TestShortEvidenceAtMs(t *testing.T) {
	scan := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	old := scan.Add(-45 * time.Minute)
	if got := shortEvidenceAtMs(breakout.ShortSignal{GeneratedAt: old}, scan); got != old.UnixMilli() {
		t.Fatalf("older own timestamp must win, got %d", got)
	}
	if got := shortEvidenceAtMs(breakout.ShortSignal{}, scan); got != scan.UnixMilli() {
		t.Fatalf("zero GeneratedAt must fall back to scan time, got %d", got)
	}
	if got := shortEvidenceAtMs(breakout.ShortSignal{GeneratedAt: scan.Add(time.Minute)}, scan); got != scan.UnixMilli() {
		t.Fatalf("newer GeneratedAt must not override, got %d", got)
	}
}
