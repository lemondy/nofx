package store

import (
	"testing"
)

// The partial unique index must (1) dedupe historical OPEN duplicates at
// migration, (2) allow a NEW open row after the previous one was evaluated,
// and (3) still reject a concurrent second open row for the same key.
// 2026-10-04: the first tag-based version indexed ALL rows and FATAL'd
// startup on legitimate historical data.
func TestGateShadowPartialUniqueMigration(t *testing.T) {
	st := newTestStore(t)
	// newTestStore already ran the migration (index live) — drop it to
	// reconstruct the PRE-migration historical state being tested.
	if err := st.gdb.Exec("DROP INDEX IF EXISTS uniq_gshadow_open").Error; err != nil {
		t.Fatal(err)
	}

	seed := func(horizon int, outcome string) {
		if err := st.gdb.Exec(`INSERT INTO gate_shadow_blocks
			(trader_id, symbol, direction, horizon_hours, outcome, created_at)
			VALUES ('t1','AGTUSDT','long',?,?,'2026-10-01')`, horizon, outcome).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Historical mess: two OPEN duplicates for the same key+horizon, plus an
	// already-evaluated row, plus the 48h twin.
	seed(8, "")
	seed(8, "")
	seed(8, "tp_first")
	seed(48, "")

	if err := st.GateShadow().initTables(); err != nil {
		t.Fatalf("migration with duplicates must dedupe and succeed: %v", err)
	}

	var open int
	if err := st.gdb.Raw(`SELECT COUNT(*) FROM gate_shadow_blocks
		WHERE trader_id='t1' AND symbol='AGTUSDT' AND direction='long' AND horizon_hours=8 AND outcome=''`).Scan(&open).Error; err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Fatalf("duplicate open rows must dedupe to 1, got %d", open)
	}

	// After ALL open rows for the key are evaluated, a NEW block row must be
	// allowed (legitimate re-block) — and must not collide with the evaluated
	// history. Note CreateIfIdle's idle check is key-wide (no horizon filter,
	// pre-existing semantics): an open 48h row blocks an 8h re-block too.
	if err := st.gdb.Exec(`UPDATE gate_shadow_blocks SET outcome='tp_first'
		WHERE trader_id='t1' AND symbol='AGTUSDT' AND outcome=''`).Error; err != nil {
		t.Fatal(err)
	}
	if created, err := st.GateShadow().CreateIfIdle(&GateShadowBlock{
		TraderID: "t1", Symbol: "AGTUSDT", Direction: "long", HorizonHours: 8,
	}); err != nil || !created {
		t.Fatalf("re-block after full evaluation must be allowed: created=%v err=%v", created, err)
	}

	// A second OPEN row for the same key+horizon must now be rejected by the
	// partial index (CreateIfIdle → benign not-idle).
	created, err := st.GateShadow().CreateIfIdle(&GateShadowBlock{
		TraderID: "t1", Symbol: "AGTUSDT", Direction: "long", HorizonHours: 8,
	})
	if err != nil {
		t.Fatalf("CreateIfIdle must tolerate the unique race: %v", err)
	}
	if created {
		t.Fatal("second open row for the same key must not be created")
	}
}
