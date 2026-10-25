package store

import (
	"path/filepath"
	"testing"
	"time"
)

// Review 2026-10-08 H: quality→outcome buckets must join an open assessment to
// ITS order's trade (exact), and the legacy heuristic must be time-bounded and
// AI-owned only.

func bucketTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "bk.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func bptr(b bool) *bool { return &b }

func addAssessment(t *testing.T, st *Store, a EntryAssessment) {
	t.Helper()
	a.TraderID = "t1"
	if a.Symbol == "" {
		a.Symbol = "AAAUSDT"
	}
	if a.Direction == "" {
		a.Direction = "long"
	}
	if a.Action == "" {
		a.Action = "open_long_limit"
	}
	if err := st.EntryAssessment().Insert(&a); err != nil {
		t.Fatal(err)
	}
}

// addTrade inserts a closed position (entry order id) and its journal row.
func addTrade(t *testing.T, st *Store, orderID string, entry time.Time, pnl float64, ai *bool) {
	t.Helper()
	pos := TraderPosition{
		TraderID: "t1", Symbol: "AAAUSDT", Side: "LONG", Quantity: 1, EntryPrice: 100,
		EntryOrderID: orderID, EntryTime: entry.UnixMilli(), ExitTime: entry.Add(time.Hour).UnixMilli(),
		RealizedPnL: pnl, Status: "CLOSED",
	}
	if err := st.gdb.Create(&pos).Error; err != nil {
		t.Fatal(err)
	}
	j := TradeJournalDB{
		TraderID: "t1", PositionID: pos.ID, Symbol: "AAAUSDT", Side: "LONG", Quantity: 1,
		EntryPrice: 100, RealizedPnL: pnl, PnLPct: pnl, AIManaged: ai,
		EntryTime: entry.UnixMilli(), ExitTime: entry.Add(time.Hour).UnixMilli(),
	}
	if err := st.gdb.Create(&j).Error; err != nil {
		t.Fatal(err)
	}
}

func bucketOf(t *testing.T, st *Store, name string) QualityBucketStat {
	t.Helper()
	stats, err := st.EntryAssessment().BucketStats("t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range stats {
		if b.Bucket == name {
			return b
		}
	}
	t.Fatalf("bucket %s missing", name)
	return QualityBucketStat{}
}

func TestBucketExactJoinIgnoresEarlierUnrelatedTrade(t *testing.T) {
	st := bucketTestStore(t)
	now := time.Now().UTC()
	addAssessment(t, st, EntryAssessment{EntryQuality: 65, OrderTracked: true, OrderID: "ORD1", Ts: now.Add(-3 * time.Hour)})
	addTrade(t, st, "OTHER", now.Add(-170*time.Minute), -5, bptr(true)) // earlier unrelated AI trade
	addTrade(t, st, "ORD1", now.Add(-150*time.Minute), 10, bptr(true))
	b := bucketOf(t, st, "60-70")
	if b.Traded != 1 || b.MatchedExact != 1 || b.MatchedLegacy != 0 || b.Wins != 1 || b.TotalPnL != 10 {
		t.Fatalf("exact join wrong: %+v", b)
	}
}

func TestBucketTrackedUnfilledClaimsNothing(t *testing.T) {
	st := bucketTestStore(t)
	now := time.Now().UTC()
	addAssessment(t, st, EntryAssessment{EntryQuality: 65, OrderTracked: true, OrderID: "EXPIRED", Ts: now.Add(-3 * time.Hour)})
	addTrade(t, st, "LATER", now.Add(-2*time.Hour), 7, bptr(true))
	if b := bucketOf(t, st, "60-70"); b.Traded != 0 || b.Assessments != 1 {
		t.Fatalf("unfilled order must have no outcome: %+v", b)
	}
}

func TestBucketTrackedFailedOpenNoOutcome(t *testing.T) {
	st := bucketTestStore(t)
	now := time.Now().UTC()
	addAssessment(t, st, EntryAssessment{EntryQuality: 65, OrderTracked: true, OrderID: "", Ts: now.Add(-3 * time.Hour)})
	addTrade(t, st, "X", now.Add(-170*time.Minute), 7, bptr(true))
	if b := bucketOf(t, st, "60-70"); b.Traded != 0 {
		t.Fatalf("failed open must have no outcome: %+v", b)
	}
}

func TestBucketPartialFillSplitAggregates(t *testing.T) {
	st := bucketTestStore(t)
	now := time.Now().UTC()
	addAssessment(t, st, EntryAssessment{EntryQuality: 75, OrderTracked: true, OrderID: "SPLIT", Ts: now.Add(-3 * time.Hour)})
	addTrade(t, st, "SPLIT", now.Add(-170*time.Minute), 10, bptr(true))
	addTrade(t, st, "SPLIT", now.Add(-169*time.Minute), -4, bptr(true))
	b := bucketOf(t, st, "70-80")
	if b.Traded != 1 || b.MatchedExact != 1 || b.TotalPnL != 6 || b.Wins != 1 {
		t.Fatalf("split fills must aggregate into one trade: %+v", b)
	}
}

func TestBucketLegacyBoundedAndAIOnly(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name  string
		delay time.Duration
		ai    *bool
		want  int
	}{
		{"ai within 2h", 90 * time.Minute, bptr(true), 1},
		{"ai 3h later", 3 * time.Hour, bptr(true), 0},
		{"manual", 10 * time.Minute, bptr(false), 0},
		{"ai_managed NULL", 10 * time.Minute, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := bucketTestStore(t)
			ts := now.Add(-5 * time.Hour)
			addAssessment(t, st, EntryAssessment{EntryQuality: 85, Ts: ts}) // legacy: OrderTracked=false
			addTrade(t, st, "", ts.Add(c.delay), 3, c.ai)
			b := bucketOf(t, st, "80+")
			if b.Traded != c.want || b.MatchedLegacy != c.want || b.MatchedExact != 0 {
				t.Fatalf("want %d legacy match, got %+v", c.want, b)
			}
		})
	}
}

func TestBucketExactMatchNotReclaimedByLegacy(t *testing.T) {
	st := bucketTestStore(t)
	now := time.Now().UTC()
	// The legacy row is OLDER, so it would claim the journal row first if exact weren't resolved first.
	addAssessment(t, st, EntryAssessment{EntryQuality: 85, Ts: now.Add(-3 * time.Hour)})
	addAssessment(t, st, EntryAssessment{EntryQuality: 65, OrderTracked: true, OrderID: "ORD1", Ts: now.Add(-100 * time.Minute)})
	addTrade(t, st, "ORD1", now.Add(-90*time.Minute), 10, bptr(true))
	if b := bucketOf(t, st, "80+"); b.Traded != 0 {
		t.Fatalf("legacy must not re-claim an exact-matched row: %+v", b)
	}
	if b := bucketOf(t, st, "60-70"); b.MatchedExact != 1 {
		t.Fatalf("exact row lost its trade: %+v", b)
	}
}
