package trader

import (
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

// 09-28 review P3: order sync ingests the WHOLE exchange account — without
// the AI-book attribution a manual WIN on symbol X reset the AI's forming
// 3-loss streak and a manual loss extended it. Only AI-attributed closes
// feed the breaker. Rows are built through the REAL position-builder path
// (registry mark → open fill → close fill), exactly like order sync does in
// production.
func TestLossStreakIgnoresManualTrades(t *testing.T) {
	st, err := store.NewWithConfig(store.DBConfig{Type: store.DBTypeSQLite, Path: filepath.Join(t.TempDir(), "streak.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	pb := store.NewPositionBuilder(st.Position())
	now := time.Now()
	mk := func(traderID, symbol string, ai bool, pnl float64, ageHours int) {
		exitTime := now.Add(-time.Duration(ageHours) * time.Hour)
		entryTime := exitTime.Add(-time.Hour)
		if ai {
			if err := st.AIManaged().Mark(traderID, symbol, "short"); err != nil {
				t.Fatal(err)
			}
		}
		if err := pb.ProcessTrade(traderID, "ex", "binance", symbol, "SHORT", "open_short",
			1, 100, 0, 0, entryTime.UnixMilli(), "o1"); err != nil {
			t.Fatal(err)
		}
		if err := pb.ProcessTrade(traderID, "ex", "binance", symbol, "SHORT", "close_short",
			1, 100+pnl, 0, pnl, exitTime.UnixMilli(), "o2"); err != nil {
			t.Fatal(err)
		}
	}

	// AI: two consecutive losses on XUSDT, then a MANUAL win (no registry
	// mark) lands between them and the third AI loss. Old behavior: the
	// manual win reset the streak and no ban fired; correct behavior: it is
	// invisible to the breaker.
	mk("t1", "XUSDT", true, -1, 5)
	mk("t1", "XUSDT", true, -1, 4)
	// The AI lifecycle unmarks the registry when its position closes — a
	// manual position opened afterwards is born while NO mark exists (and
	// in production a same-direction manual open would just average into
	// the AI row anyway). Reproduce that ordering.
	if err := st.AIManaged().Unmark("t1", "XUSDT", "short"); err != nil {
		t.Fatal(err)
	}
	mk("t1", "XUSDT", false, +2, 3)
	mk("t1", "XUSDT", true, -1, 2)

	at := &AutoTrader{id: "t1", store: st}
	banned, msg := at.lossStreakBlocks("XUSDT", 3)
	if !banned {
		t.Fatalf("3 AI losses with a manual win interleaved must still trip the ban (manual trades are not the AI's book): %q", msg)
	}

	// Mirror: a manual LOSS must not COUNT toward the streak either —
	// 2 AI losses + 1 manual loss = no ban.
	mk("t2", "YUSDT", true, -1, 5)
	mk("t2", "YUSDT", true, -1, 4)
	if err := st.AIManaged().Unmark("t2", "YUSDT", "short"); err != nil {
		t.Fatal(err)
	}
	mk("t2", "YUSDT", false, -1, 3)
	at2 := &AutoTrader{id: "t2", store: st}
	if banned2, _ := at2.lossStreakBlocks("YUSDT", 3); banned2 {
		t.Fatal("2 AI losses + 1 manual loss must NOT trip the 3-loss ban")
	}
}
