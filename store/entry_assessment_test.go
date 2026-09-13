package store

import (
	"testing"
	"time"
)

// ── tests ──

// Roundtrip + bucket join: an open assessment at T with quality 85 joins the
// journal trade opened at T+1 (win), an assessment without outcome stays
// traded=0, out-of-range quality (-1) is skipped.
func TestEntryAssessmentBucketStats(t *testing.T) {
	st := newTestStore(t)
	if err := st.EntryAssessment().initTables(); err != nil {
		t.Fatalf("init: %v", err)
	}
	now := time.Now()
	rows := []*EntryAssessment{
		{TraderID: "T1", Cycle: 1, Ts: now.Add(-3 * time.Hour), Symbol: "NEARUSDT", Direction: "long", Action: "open_long_limit", Stage: "TRIGGERED", EntryQuality: 85, Price: 3.1},
		{TraderID: "T1", Cycle: 2, Ts: now.Add(-2 * time.Hour), Symbol: "WLDUSDT", Direction: "short", Action: "wait", Stage: "WATCH", WaitBias: "short", EntryQuality: 65, BlockingFactors: `["ANCHOR_SUPPRESSED"]`},
		{TraderID: "T1", Cycle: 3, Ts: now.Add(-1 * time.Hour), Symbol: "KORUUSDT", Direction: "none", Action: "wait", Stage: "NO_SETUP", EntryQuality: -1},
	}
	for _, r := range rows {
		if err := st.EntryAssessment().Insert(r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	journal := []TradeJournalDB{
		{TraderID: "T1", Symbol: "NEARUSDT", Side: "LONG", EntryTime: now.Add(-2*time.Hour + 30*time.Minute).UnixMilli(), RealizedPnL: 1.2, PnLPct: 6.0},
	}
	for _, j := range journal {
		if err := st.EntryAssessment().db.Create(&j).Error; err != nil {
			t.Fatalf("journal: %v", err)
		}
	}

	stats, err := st.EntryAssessment().BucketStats("T1")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	byBucket := map[string]QualityBucketStat{}
	for _, b := range stats {
		byBucket[b.Bucket] = b
	}
	if b := byBucket["80+"]; b.Assessments != 1 || b.Traded != 1 || b.Wins != 1 || b.WinRate != 100 {
		t.Fatalf("80+ bucket wrong: %+v", b)
	}
	if b := byBucket["60-70"]; b.Assessments != 1 || b.Traded != 0 {
		t.Fatalf("60-70 bucket wrong (wait, no outcome): %+v", b)
	}
	total := 0
	for _, b := range stats {
		total += b.Assessments
	}
	if total != 2 {
		t.Fatalf("quality -1 must be skipped: total=%d", total)
	}
}
