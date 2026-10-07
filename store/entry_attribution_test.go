package store

import (
	"testing"
	"time"
)

// Entry-attribution rows: Record persists, ListByTrader scopes by trader and
// honors the since cutoff. Per-source win-rate stats read this table.
func TestEntryAttributionRecordAndList(t *testing.T) {
	st := newTestStore(t)

	base := time.Now().UTC().Add(-time.Hour)
	rows := []*EntryAttribution{
		{TraderID: "t1", Symbol: "AAAUSDT", Side: "long", OrderID: "o1",
			Sources: "ai500,oi_top", ScannerDirection: "up",
			ShortScore: 71.5, ShortGrade: "strong", ShortUniverse: "top200", ShortConfirmed: true,
			CreatedAt: base},
		{TraderID: "t1", Symbol: "BBBUSDT", Side: "short", OrderID: "o2",
			Sources: "unknown", CreatedAt: base.Add(time.Minute)},
		{TraderID: "t2", Symbol: "AAAUSDT", Side: "long", OrderID: "o3",
			Sources: "short_scan", CreatedAt: base.Add(2 * time.Minute)},
	}
	for _, r := range rows {
		if err := st.EntryAttribution().Record(r); err != nil {
			t.Fatalf("record %+v: %v", r, err)
		}
	}

	got, err := st.EntryAttribution().ListByTrader("t1", base.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows for t1, got %d", len(got))
	}
	if got[0].Sources != "ai500,oi_top" || got[0].Side != "long" || got[0].OrderID != "o1" {
		t.Fatalf("row 0 wrong: %+v", got[0])
	}
	if !got[0].ShortConfirmed || got[0].ShortGrade != "strong" || got[0].ShortScore != 71.5 || got[0].ShortUniverse != "top200" || got[0].ScannerDirection != "up" {
		t.Fatalf("short metadata lost: %+v", got[0])
	}
	if got[1].Sources != "unknown" || got[1].OrderID != "o2" {
		t.Fatalf("row 1 wrong: %+v", got[1])
	}

	// Another trader's rows must not leak in.
	got, err = st.EntryAttribution().ListByTrader("t2", base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Sources != "short_scan" {
		t.Fatalf("t2 rows wrong: %+v", got)
	}

	// The since cutoff filters older rows.
	got, err = st.EntryAttribution().ListByTrader("t1", base.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].OrderID != "o2" {
		t.Fatalf("since cutoff wrong: %+v", got)
	}
}

// review 2026-10-07: the same entry is marked from several paths (market
// open, pending fill, offline reconcile) — one row per (trader, order).
func TestEntryAttributionDedupesByOrder(t *testing.T) {
	s := newTestStore(t)
	es := s.EntryAttribution()
	first := &EntryAttribution{TraderID: "t1", Symbol: "BTCUSDT", Side: "long", OrderID: "o1", Sources: "piggy_dash"}
	if err := es.Record(first); err != nil {
		t.Fatalf("first record: %v", err)
	}
	again := &EntryAttribution{TraderID: "t1", Symbol: "BTCUSDT", Side: "long", OrderID: "o1", Sources: "unknown"}
	if err := es.Record(again); err != nil {
		t.Fatalf("duplicate record must be a no-op, got %v", err)
	}
	rows, err := es.ListByTrader("t1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Sources != "piggy_dash" {
		t.Fatalf("want 1 row keeping first attribution, got %+v", rows)
	}
}
