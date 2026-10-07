package trader

import (
	"path/filepath"
	"testing"

	"nofx/store"
)

// review 2026-10-07 B1-8: the cycle passes a lowercase side while the table
// stores LONG/SHORT; the lookup must still hit the OPEN row.
func TestLookupOpenPositionEntryMatchesLowercaseSide(t *testing.T) {
	st, err := store.NewWithConfig(store.DBConfig{Type: store.DBTypeSQLite, Path: filepath.Join(t.TempDir(), "age.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const entryTime = int64(1_700_000_000_000)
	if err := st.Position().CreateOpenPosition(&store.TraderPosition{
		TraderID: "t1", Symbol: "AAAUSDT", Side: "LONG",
		Quantity: 1, EntryQuantity: 2, EntryPrice: 100, EntryTime: entryTime, Status: "OPEN",
	}); err != nil {
		t.Fatal(err)
	}

	at := &AutoTrader{id: "t1", store: st}
	for _, side := range []string{"long", "LONG"} {
		et, qty, ok := at.lookupOpenPositionEntry("AAAUSDT", side)
		if !ok || et != entryTime || qty != 2 {
			t.Fatalf("side %q: got (%d, %v, %v), want (%d, 2, true)", side, et, qty, ok, entryTime)
		}
	}
	if _, _, ok := at.lookupOpenPositionEntry("AAAUSDT", "short"); ok {
		t.Fatal("short must not match a LONG row")
	}
	if _, _, ok := (&AutoTrader{id: "t1"}).lookupOpenPositionEntry("AAAUSDT", "long"); ok {
		t.Fatal("nil store must report not found")
	}
}
