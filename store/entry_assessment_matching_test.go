package store

import (
	"path/filepath"
	"testing"
	"time"
)

// P1 (2026-09-26 re-review): a WAIT assessment must never consume a later
// same-symbol trade as its own outcome — only open actions match.
func TestBucketStatsWaitDoesNotConsumeTrade(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "eq.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()

	// One WAIT assessment with direction + score.
	if err := st.EntryAssessment().Insert(&EntryAssessment{
		TraderID: "t1", Symbol: "AAAUSDT", Action: "wait", Stage: "NO_SETUP",
		Direction: "long", EntryQuality: 85, Ts: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// One OPEN assessment, same symbol/side, later — the one that owns the trade.
	if err := st.EntryAssessment().Insert(&EntryAssessment{
		TraderID: "t1", Symbol: "AAAUSDT", Action: "open_long_limit", Stage: "TRIGGERED",
		Direction: "long", EntryQuality: 65, Ts: now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// The trade itself (BucketStats joins trade_journal, not positions).
	if err := st.gdb.Create(&TradeJournalDB{
		TraderID: "t1", Symbol: "AAAUSDT", Side: "LONG",
		Quantity: 1, EntryPrice: 100, ExitPrice: 110, RealizedPnL: 10, PnLPct: 10,
		AIManaged: func() *bool { b := true; return &b }(), // legacy join requires AI ownership (review 2026-10-08 H)
		EntryTime: now.Add(-30 * time.Minute).UnixMilli(), ExitTime: now.UnixMilli(),
	}).Error; err != nil {
		t.Fatal(err)
	}

	stats, err := st.EntryAssessment().BucketStats("t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range stats {
		if b.Bucket == "80+" && b.Traded != 0 {
			t.Fatalf("wait(85) must NOT consume the trade, got traded=%d", b.Traded)
		}
		if b.Bucket == "60-70" && b.Traded != 1 {
			t.Fatalf("open(40) must own the trade, got traded=%d", b.Traded)
		}
	}
}
