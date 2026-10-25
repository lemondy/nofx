package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"nofx/store"

	"github.com/gin-gonic/gin"
)

// attribTally is one tally object of the gate-shadow-stats response.
type attribTally struct {
	Total    int     `json:"total"`
	TpFirst  int     `json:"tp_first"`
	SlFirst  int     `json:"sl_first"`
	Timeout  int     `json:"timeout"`
	SumR     float64 `json:"sum_r"`
	RSamples int     `json:"r_samples"`
}

// attribStats is the part of the gate-shadow-stats response these tests read.
type attribStats struct {
	RowsEvaluated  int                    `json:"rows_evaluated"`
	RowsDropped48h int                    `json:"rows_dropped_overlap_48h"`
	Overall        attribTally            `json:"overall"`
	ByCode         map[string]attribTally `json:"by_code"`
	BySole         map[string]attribTally `json:"by_code_sole"`
	Overall48h     attribTally            `json:"overall_48h"`
	ByCode48h      map[string]attribTally `json:"by_code_48h"`
	BySole48h      map[string]attribTally `json:"by_code_sole_48h"`
}

// attribRow is one seeded counterfactual. o8/x8 close the 8h pass; an empty o48
// leaves the 48h pass pending. Price levels come from attribLevels.
type attribRow struct {
	symbol, direction, codes string
	at                       time.Time
	o8, o48                  string
	x8, x48                  float64
}

// attribLevels gives entry 100 with a 5-point stop and a 10-point target on
// either side, so tp_first is +2R and sl_first is -1R.
func attribLevels(direction string) (entry, stop, tp float64) {
	if direction == "short" {
		return 100, 105, 90
	}
	return 100, 95, 110
}

// attribFixture opens a fresh SQLite store holding one user and one trader.
func attribFixture(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st, err := store.NewWithConfig(store.DBConfig{
		Type: store.DBTypeSQLite, Path: filepath.Join(t.TempDir(), "gate_shadow_attribution.db"),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const userID, traderID = "user-attrib", "trader-attrib"
	if err := st.User().Create(&store.User{ID: userID, Email: "attrib@test.com"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.Trader().Create(&store.Trader{ID: traderID, UserID: userID, Name: "Attribution"}); err != nil {
		t.Fatalf("create trader: %v", err)
	}
	return st, userID, traderID
}

// seedAttribRows records each counterfactual for traderID (plan RR 2), closes
// its 8h verdict and, when o48 is set, its 48h verdict too. Each row is closed
// before the next is created, so rows sharing a key never collide on the open
// row index.
func seedAttribRows(t *testing.T, st *store.Store, traderID string, rows ...attribRow) {
	t.Helper()
	for _, r := range rows {
		entry, stop, tp := attribLevels(r.direction)
		rec := &store.GateShadowBlock{
			TraderID:     traderID,
			Symbol:       r.symbol,
			Direction:    r.direction,
			BlockedCodes: r.codes,
			EntryPrice:   entry,
			StopPrice:    stop,
			TakeProfit:   tp,
			PlanRR:       2,
			HorizonHours: 8,
			CreatedAt:    r.at,
		}
		created, err := st.GateShadow().CreateIfIdle(rec)
		if err != nil || !created {
			t.Fatalf("create %s %s: created=%v err=%v", r.symbol, r.direction, created, err)
		}
		if err := st.GateShadow().MarkEvaluated(rec.ID, r.o8, r.x8, time.Now().UTC()); err != nil {
			t.Fatalf("mark 8h %s %s: %v", r.symbol, r.direction, err)
		}
		if r.o48 != "" {
			if err := st.GateShadow().MarkEvaluated48(rec.ID, r.o48, r.x48, time.Now().UTC()); err != nil {
				t.Fatalf("mark 48h %s %s: %v", r.symbol, r.direction, err)
			}
		}
	}
}

// fetchAttribStats runs the handler for userID and decodes the response.
func fetchAttribStats(t *testing.T, st *store.Store, userID string) attribStats {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/gate-shadow-stats", nil)
	c.Set("user_id", userID)
	(&Server{store: st}).handleGateShadowStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got attribStats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return got
}

// review 2026-10-09 F: by_code counts a row under every family it touched
// (any-blocker view); by_code_sole counts it only when that family is its sole
// blocker.
func TestGateShadowStatsSoleBlockerView(t *testing.T) {
	st, userID, traderID := attribFixture(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedAttribRows(t, st, traderID,
		// Two families: under both in by_code, under neither in by_code_sole.
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07,CONSENSUS_OPPOSED_50", at: base, o8: "tp_first", x8: 110},
		// RR_MAX alone: the sole blocker, so it also lands in by_code_sole.
		attribRow{symbol: "ETHUSDT", direction: "long", codes: "RR_MAX_0.12", at: base, o8: "sl_first", x8: 95},
	)
	got := fetchAttribStats(t, st, userID)

	if got.ByCode["RR_MAX"].Total != 2 || got.ByCode["CONSENSUS_OPPOSED"].Total != 1 || len(got.ByCode) != 2 {
		t.Errorf("by_code = %+v, want RR_MAX=2 and CONSENSUS_OPPOSED=1", got.ByCode)
	}
	wantSole := attribTally{Total: 1, SlFirst: 1, SumR: -1, RSamples: 1}
	if len(got.BySole) != 1 || got.BySole["RR_MAX"] != wantSole {
		t.Errorf("by_code_sole = %+v, want only RR_MAX = %+v", got.BySole, wantSole)
	}
}

// review 2026-10-09 F: a family repeated within one row is still a single
// blocker, so the row counts once under it in by_code_sole.
func TestGateShadowStatsSoleBlockerDuplicateFamily(t *testing.T) {
	st, userID, traderID := attribFixture(t)
	seedAttribRows(t, st, traderID, attribRow{
		symbol: "SOLUSDT", direction: "short", codes: "RR_MAX_0.07,RR_MAX_0.10",
		at: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), o8: "tp_first", x8: 90,
	})
	got := fetchAttribStats(t, st, userID)

	want := attribTally{Total: 1, TpFirst: 1, SumR: 2, RSamples: 1}
	if len(got.BySole) != 1 || got.BySole["RR_MAX"] != want {
		t.Errorf("by_code_sole = %+v, want only RR_MAX = %+v", got.BySole, want)
	}
}

// review 2026-10-09 F: rows of one trader+symbol+direction created 8h apart
// share most of their 48h path. The row 8h after the first is thinned out of
// the 48h tallies (its sl_first never shows up there), while all three still
// count in the 8h tallies.
func TestGateShadowStats48hThinsOverlap(t *testing.T) {
	st, userID, traderID := attribFixture(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedAttribRows(t, st, traderID,
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07", at: base, o8: "tp_first", x8: 110, o48: "tp_first", x48: 110},
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07", at: base.Add(8 * time.Hour), o8: "sl_first", x8: 95, o48: "sl_first", x48: 95},
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07", at: base.Add(50 * time.Hour), o8: "timeout", x8: 102, o48: "tp_first", x48: 110},
	)
	got := fetchAttribStats(t, st, userID)

	if got.Overall.Total != 3 {
		t.Errorf("overall total = %d, want 3 (8h tallies keep every row)", got.Overall.Total)
	}
	if got.RowsDropped48h != 1 {
		t.Errorf("rows_dropped_overlap_48h = %d, want 1", got.RowsDropped48h)
	}
	want48 := attribTally{Total: 2, TpFirst: 2, SumR: 4, RSamples: 2}
	if got.Overall48h != want48 {
		t.Errorf("overall_48h = %+v, want %+v", got.Overall48h, want48)
	}
	if got.ByCode48h["RR_MAX"] != want48 {
		t.Errorf("by_code_48h = %+v, want only RR_MAX = %+v", got.ByCode48h, want48)
	}
	if got.BySole48h["RR_MAX"] != want48 {
		t.Errorf("by_code_sole_48h = %+v, want only RR_MAX = %+v", got.BySole48h, want48)
	}
}

// review 2026-10-09 F: the thinning key is symbol+direction across traders (same
// price path). The same key 8h later is dropped — also from another trader —
// while another symbol or the opposite direction at that same time is kept.
func TestGateShadowStats48hThinKeys(t *testing.T) {
	st, userID, traderID := attribFixture(t)
	const otherTraderID = "trader-attrib-2"
	if err := st.Trader().Create(&store.Trader{ID: otherTraderID, UserID: userID, Name: "Attribution 2"}); err != nil {
		t.Fatalf("create trader: %v", err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at8h := base.Add(8 * time.Hour)
	seedAttribRows(t, st, traderID,
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07", at: base, o8: "tp_first", x8: 110, o48: "tp_first", x48: 110},
		// Same trader+symbol+direction 8h later: thinned out of the 48h tallies.
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07", at: at8h, o8: "tp_first", x8: 110, o48: "tp_first", x48: 110},
		// Different symbol at the same time: kept.
		attribRow{symbol: "ETHUSDT", direction: "long", codes: "RR_MAX_0.07", at: at8h, o8: "tp_first", x8: 110, o48: "tp_first", x48: 110},
		// Opposite direction at the same time: kept.
		attribRow{symbol: "BTCUSDT", direction: "short", codes: "RR_MAX_0.07", at: at8h, o8: "tp_first", x8: 90, o48: "tp_first", x48: 90},
	)
	// Another trader on the same symbol and direction 8h later: thinned too.
	seedAttribRows(t, st, otherTraderID,
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07", at: at8h, o8: "tp_first", x8: 110, o48: "tp_first", x48: 110},
	)
	got := fetchAttribStats(t, st, userID)

	if got.RowsEvaluated != 5 {
		t.Errorf("rows_evaluated = %d, want 5", got.RowsEvaluated)
	}
	if got.RowsDropped48h != 2 {
		t.Errorf("rows_dropped_overlap_48h = %d, want 2", got.RowsDropped48h)
	}
	if got.Overall48h.Total != 3 {
		t.Errorf("overall_48h total = %d, want 3", got.Overall48h.Total)
	}
}

// review 2026-10-09 F: a row whose 48h verdict is still pending takes no part in
// the thinning. It neither counts in the 48h tallies nor shadows a later row.
func TestGateShadowStats48hPendingRowsDoNotThin(t *testing.T) {
	st, userID, traderID := attribFixture(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedAttribRows(t, st, traderID,
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07", at: base, o8: "tp_first", x8: 110, o48: "tp_first", x48: 110},
		// 48h verdict still pending: 8h only.
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07", at: base.Add(8 * time.Hour), o8: "timeout", x8: 102},
		attribRow{symbol: "BTCUSDT", direction: "long", codes: "RR_MAX_0.07", at: base.Add(50 * time.Hour), o8: "tp_first", x8: 110, o48: "tp_first", x48: 110},
	)
	got := fetchAttribStats(t, st, userID)

	if got.RowsDropped48h != 0 {
		t.Errorf("rows_dropped_overlap_48h = %d, want 0", got.RowsDropped48h)
	}
	if got.Overall48h.Total != 2 {
		t.Errorf("overall_48h total = %d, want 2", got.Overall48h.Total)
	}
	if got.Overall.Total != 3 {
		t.Errorf("overall total = %d, want 3", got.Overall.Total)
	}
}
