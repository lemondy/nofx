package store

import (
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 2026-10-10 per-candidate block reasons: round-trip through the DB.
func TestDecisionCandidateVerdictsRoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "d.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	s := NewDecisionStore(db)
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
	rec := &DecisionRecord{
		TraderID: "t1", CycleNumber: 1, CandidateCoins: []string{"AUSDT", "OIUSDT"},
		CandidateVerdicts: []CandidateVerdict{
			{Symbol: "AUSDT", Status: "blocked", LongFailed: []string{"BTC_4H_DOWNTREND"}, ShortFailed: []string{"RR_MAX_0.24"}},
			{Symbol: "OIUSDT", Status: "filtered", Reason: "OI 3.60M < 5.0M"},
		},
	}
	if err := s.LogDecision(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.LogDecision(&DecisionRecord{TraderID: "t1", CycleNumber: 2}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLatestRecords("t1", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d err %v", len(got), err)
	}
	var withV, without *DecisionRecord
	for _, r := range got {
		if r.CycleNumber == 1 {
			withV = r
		} else {
			without = r
		}
	}
	if len(withV.CandidateVerdicts) != 2 || withV.CandidateVerdicts[1].Reason != "OI 3.60M < 5.0M" || withV.CandidateVerdicts[0].ShortFailed[0] != "RR_MAX_0.24" {
		t.Errorf("verdicts lost: %+v", withV.CandidateVerdicts)
	}
	if len(without.CandidateVerdicts) != 0 {
		t.Errorf("old-style record should have no verdicts: %+v", without.CandidateVerdicts)
	}
}
