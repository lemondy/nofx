package kernel

import (
	"os"
	"strings"
	"testing"
)

// 2026-09-27 data-quality fix: the regime-skip SYNTHESIZED wait must carry
// the dedicated ALL_CANDIDATES_HARD_BLOCKED tag (not CONFLICT_UNRESOLVED,
// which misdescribed cycles whose real blockers are RR_MAX/STOP_PLAN/VENDOR_
// DIVERGENCE/MICRO_TREND/…). The tag must also survive validation whitelisting.
func TestRegimeSkipSyntheticBlockingFactor(t *testing.T) {
	src, err := os.ReadFile("engine_analysis.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `BlockingFactors: []string{"ALL_CANDIDATES_HARD_BLOCKED"}`) {
		t.Fatal("regime-skip synthesis must use ALL_CANDIDATES_HARD_BLOCKED")
	}
	if strings.Contains(string(src), `BlockingFactors: []string{"CONFLICT_UNRESOLVED"}`) {
		t.Fatal("regime-skip synthesis must not hardcode CONFLICT_UNRESOLVED")
	}
	got := NormalizeBlockingFactors([]string{"ALL_CANDIDATES_HARD_BLOCKED"})
	if len(got) != 1 || got[0] != "ALL_CANDIDATES_HARD_BLOCKED" {
		t.Fatalf("dedicated tag must survive validation, got %v", got)
	}
}
