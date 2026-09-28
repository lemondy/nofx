package store

import (
	"path/filepath"
	"testing"
)

// 09-28 review P2: GetSymbolStats truncates by TotalPnL DESC, so a capped
// window silently dropped the WORST symbols first — exactly the population
// POOR_HISTORY / NEG_EDGE_LOSING_SYMBOL / recentHistoryNote exist to
// surface. limit<=0 must return the full set.
func TestGetSymbolStatsNoLimitKeepsWorstSymbols(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "stats.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	traderID := "t1"
	mk := func(symbol string, pnl float64) {
		if err := st.gdb.Create(&TraderPosition{
			TraderID: traderID, Symbol: symbol, Side: "LONG",
			Quantity: 1, EntryPrice: 100, ExitPrice: 100 + pnl,
			RealizedPnL: pnl, Status: "CLOSED",
			EntryTime: 1000, ExitTime: 2000,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk("WINNERUSDT", 10)
	mk("LOSERRUSDT", -50) // the symbol every history gate needs to see
	mk("MIDDLEUSDT", 5)

	// Capped window (the old loadSymbolStats call): DESC order drops the
	// worst symbol first — pins the documented truncation behavior.
	capped, err := st.Position().GetSymbolStats(traderID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 2 {
		t.Fatalf("capped stats = %d rows, want 2", len(capped))
	}
	for _, s := range capped {
		if s.Symbol == "LOSERRUSDT" {
			t.Fatal("capped window must drop the worst symbol first (pins the DESC-truncate semantics)")
		}
	}

	// Unlimited (the new loadSymbolStats call): every symbol present.
	full, err := st.Position().GetSymbolStats(traderID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 3 {
		t.Fatalf("unlimited stats = %d rows, want 3", len(full))
	}
	found := false
	for _, s := range full {
		if s.Symbol == "LOSERRUSDT" {
			found = true
		}
	}
	if !found {
		t.Fatal("unlimited stats must include the worst-losing symbol")
	}
}
