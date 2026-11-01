package api

import (
	"errors"
	"testing"

	"gorm.io/gorm"
	"nofx/store"
)

// Review 2026-10-28 03 06-B6: statistics query failures used to be dropped
// with `_`; the history response must surface them in stats_error.
func TestReview20261028VFPositionHistoryStatsErrors(t *testing.T) {
	srv, _, traderID := syncBalanceFixture(t, true)
	db := srv.store.GormDB()
	if err := db.Create(&store.TraderPosition{TraderID: traderID, Symbol: "XUSDT", Status: "CLOSED", RealizedPnL: 1, EntryTime: 100, ExitTime: 200}).Error; err != nil {
		t.Fatal(err)
	}

	stats, symbols, dirs, errs := positionHistoryStats(srv.store.Position(), traderID, 100)
	if len(errs) != 0 || stats == nil || len(symbols) != 1 || len(dirs) != 1 {
		t.Fatalf("healthy queries: stats=%v symbols=%v dirs=%v errs=%v", stats, symbols, dirs, errs)
	}

	const cb = "review20261028_vf_stats_failure"
	if err := db.Callback().Query().Before("gorm:query").Register(cb, func(q *gorm.DB) {
		if q.Statement.Table == "trader_positions" {
			q.AddError(errors.New("injected stats database failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(cb) })

	stats, symbols, dirs, errs = positionHistoryStats(srv.store.Position(), traderID, 100)
	if stats != nil || symbols != nil || dirs != nil {
		t.Fatalf("failed queries must return nil results: %v %v %v", stats, symbols, dirs)
	}
	for _, key := range []string{"stats", "symbol_stats", "direction_stats"} {
		if errs[key] == "" {
			t.Fatalf("missing %s error: %v", key, errs)
		}
	}
}
