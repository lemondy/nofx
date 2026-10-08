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

// review 2026-10-08 C: no_data rows (exit 0) polluted sum_r/avg_r.
func TestShadowRRejectsRowsWithoutUsableR(t *testing.T) {
	cases := []struct {
		name      string
		direction string
		outcome   string
		entry     float64
		stop      float64
		exit      float64
		wantR     float64
		wantOK    bool
	}{
		{"long no_data exit 0", "long", "no_data", 100, 95, 0, 0, false},
		{"short no_data exit 0", "short", "no_data", 100, 105, 0, 0, false},
		{"long tp_first", "long", "tp_first", 100, 95, 110, 2, true},
		{"short sl_first", "short", "sl_first", 100, 105, 105, -1, true},
		{"timeout exit 0", "long", "timeout", 100, 95, 0, 0, false},
		{"short timeout at entry", "short", "timeout", 100, 105, 100, 0, true},
		{"long risk <= 0", "long", "tp_first", 100, 105, 110, 0, false},
		{"short risk <= 0", "short", "sl_first", 100, 95, 95, 0, false},
		{"empty outcome", "long", "", 100, 95, 110, 0, false},
	}
	for _, tc := range cases {
		gotR, gotOK := shadowR(tc.direction, tc.outcome, tc.entry, tc.stop, tc.exit)
		if gotR != tc.wantR || gotOK != tc.wantOK {
			t.Errorf("%s: shadowR = (%v, %v), want (%v, %v)", tc.name, gotR, gotOK, tc.wantR, tc.wantOK)
		}
	}
}

// review 2026-10-08 C: no_data rows (exit 0) polluted sum_r/avg_r. Seeds four
// resolved counterfactuals for one trader and checks the stats handler: a
// no_data horizon counts in total and no_data but adds no R, avg_r divides by
// r_samples, and the 48h R marks to the 48h exit (never the 8h one).
func TestGateShadowStatsSkipsNoDataR(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st, err := store.NewWithConfig(store.DBConfig{
		Type: store.DBTypeSQLite,
		Path: filepath.Join(t.TempDir(), "gate_shadow_stats.db"),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const userID, traderID = "user-gshadow", "trader-gshadow"
	if err := st.User().Create(&store.User{ID: userID, Email: "gshadow@test.com"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.Trader().Create(&store.Trader{ID: traderID, UserID: userID, Name: "GateShadow"}); err != nil {
		t.Fatalf("create trader: %v", err)
	}

	// block records one blocked counterfactual (plan RR 2, code RR_MAX) and
	// marks its 8h verdict; an empty o48 leaves the 48h pass pending.
	block := func(sym, dir string, entry, stop, tp float64, o8 string, x8 float64, o48 string, x48 float64) {
		t.Helper()
		rec := &store.GateShadowBlock{
			TraderID:     traderID,
			Symbol:       sym,
			Direction:    dir,
			BlockedCodes: "RR_MAX_0.07",
			EntryPrice:   entry,
			StopPrice:    stop,
			TakeProfit:   tp,
			PlanRR:       2,
			HorizonHours: 8,
			CreatedAt:    time.Now().UTC(),
		}
		created, err := st.GateShadow().CreateIfIdle(rec)
		if err != nil || !created {
			t.Fatalf("create %s %s: created=%v err=%v", sym, dir, created, err)
		}
		if err := st.GateShadow().MarkEvaluated(rec.ID, o8, x8, time.Now().UTC()); err != nil {
			t.Fatalf("mark 8h %s: %v", sym, err)
		}
		if o48 != "" {
			if err := st.GateShadow().MarkEvaluated48(rec.ID, o48, x48, time.Now().UTC()); err != nil {
				t.Fatalf("mark 48h %s: %v", sym, err)
			}
		}
	}
	// long: +2R at 8h; 48h still pending, so it is absent from overall_48h.
	block("BTCUSDT", "long", 100, 95, 110, "tp_first", 110, "", 0)
	// long: no_data on both horizons, contributes no R anywhere.
	block("ETHUSDT", "long", 100, 95, 110, "no_data", 0, "no_data", 0)
	// short: -1R at 8h; 48h timeout at 95 is +1R, from the 48h exit.
	block("SOLUSDT", "short", 100, 105, 90, "sl_first", 105, "timeout", 95)
	// short: no_data at 8h; 48h tp_first at 90 is +2R.
	block("XRPUSDT", "short", 100, 105, 90, "no_data", 0, "tp_first", 90)

	srv := &Server{store: st}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/gate-shadow-stats", nil)
	c.Set("user_id", userID)
	srv.handleGateShadowStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	type tallyJSON struct {
		Total     int     `json:"total"`
		TpFirst   int     `json:"tp_first"`
		SlFirst   int     `json:"sl_first"`
		Timeout   int     `json:"timeout"`
		NoData    int     `json:"no_data"`
		WinRate   float64 `json:"win_rate_pct"`
		SumR      float64 `json:"sum_r"`
		AvgR      float64 `json:"avg_r"`
		RSamples  int     `json:"r_samples"`
		PlannedRR float64 `json:"avg_planned_rr"`
	}
	var got struct {
		RowsEvaluated int                  `json:"rows_evaluated"`
		Overall       tallyJSON            `json:"overall"`
		ByCode        map[string]tallyJSON `json:"by_code"`
		Overall48     tallyJSON            `json:"overall_48h"`
		ByCode48      map[string]tallyJSON `json:"by_code_48h"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}

	// 8h: +2R (BTC tp_first) and -1R (SOL sl_first); the two no_data rows add
	// no R, so avg_r = 1 / 2 usable samples.
	wantOverall := tallyJSON{Total: 4, TpFirst: 1, SlFirst: 1, NoData: 2, WinRate: 50, SumR: 1, AvgR: 0.5, RSamples: 2, PlannedRR: 2}
	// 48h: +1R (SOL timeout) and +2R (XRP tp_first); BTC is pending and ETH is
	// no_data, so avg_r = 3 / 2 usable samples.
	wantOverall48 := tallyJSON{Total: 3, TpFirst: 1, Timeout: 1, NoData: 1, WinRate: 100, SumR: 3, AvgR: 1.5, RSamples: 2, PlannedRR: 2}

	if got.RowsEvaluated != 4 {
		t.Errorf("rows_evaluated = %d, want 4", got.RowsEvaluated)
	}
	if got.Overall != wantOverall {
		t.Errorf("overall = %+v, want %+v", got.Overall, wantOverall)
	}
	if got.Overall48 != wantOverall48 {
		t.Errorf("overall_48h = %+v, want %+v", got.Overall48, wantOverall48)
	}
	if len(got.ByCode) != 1 || got.ByCode["RR_MAX"] != wantOverall {
		t.Errorf("by_code = %+v, want only RR_MAX = %+v", got.ByCode, wantOverall)
	}
	if len(got.ByCode48) != 1 || got.ByCode48["RR_MAX"] != wantOverall48 {
		t.Errorf("by_code_48h = %+v, want only RR_MAX = %+v", got.ByCode48, wantOverall48)
	}
}
