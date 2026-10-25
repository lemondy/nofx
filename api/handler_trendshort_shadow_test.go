package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nofx/market/breakout"
	"nofx/store"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func trendAPILabel(net float64) breakout.TrendShortLabel {
	return breakout.TrendShortLabel{Done: true, NetPct: net}
}
func trendAPIRows(base time.Time) []breakout.TrendShortShadowRow {
	var rows []breakout.TrendShortShadowRow
	for i := 0; i < 10; i++ {
		l := trendAPILabel(5)
		l.Path = "tp_first"
		l.R = 2
		if i == 8 {
			l.Path = "sl_first"
			l.R = -1
		}
		if i == 9 {
			l.Path = "timeout"
			l.R = .5
		}
		rows = append(rows, breakout.TrendShortShadowRow{Kind: "pick", TS: base.UnixMilli(), Rank: i + 1, TrendShortPick: breakout.TrendShortPick{Symbol: fmt.Sprintf("S%02dUSDT", i), Price: 100}, Labels: breakout.TrendShortLabels{H4: trendAPILabel(2), H24: l}})
	}
	l := trendAPILabel(8)
	l.Path = "tp_first"
	l.R = 2
	rows = append(rows, breakout.TrendShortShadowRow{Kind: "pick", TS: base.Add(time.Hour).UnixMilli(), Rank: 1, TrendShortPick: breakout.TrendShortPick{Symbol: "MONUSDT", Price: 100}, Labels: breakout.TrendShortLabels{H4: trendAPILabel(4), H24: l}})
	for i := 0; i < 2; i++ {
		rows = append(rows, breakout.TrendShortShadowRow{Kind: "control", TS: base.Add(time.Duration(i) * time.Hour).UnixMilli(), Prices: map[string]float64{"AUSDT": 100, "BUSDT": 100}, ControlLabels: map[string]breakout.TrendShortLabels{
			"AUSDT": {H4: trendAPILabel(0), H24: trendAPILabel(float64(3 + i))},
			"BUSDT": {H4: trendAPILabel(2), H24: trendAPILabel(float64(5 + i))},
		}})
	}
	return rows
}
func TestTrendShortShadowAPIAggregationAndOverlap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "decisions.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&store.DecisionRecordDB{}); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewFromGorm(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	rows := trendAPIRows(base)
	path := filepath.Join(t.TempDir(), "shadow.jsonl")
	breakout.SetTrendShortShadowPath(path)
	t.Cleanup(func() { breakout.SetTrendShortShadowPath("data/trendshort_shadow.jsonl") })
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	for _, d := range []store.DecisionRecord{
		{TraderID: "one", Timestamp: base.Add(10 * time.Minute), CandidateCoins: []string{"S00USDT"}},
		{TraderID: "two", Timestamp: base.Add(-10 * time.Minute), CandidateCoins: []string{"S01USDT"}},
		{TraderID: "one", Timestamp: base.Add(10*time.Minute + time.Millisecond), CandidateCoins: []string{"S02USDT"}},
		{TraderID: "two", Timestamp: base.Add(time.Hour - 10*time.Minute), CandidateCoins: []string{"MONUSDT"}},
		{TraderID: "two", Timestamp: base.Add(-11 * time.Minute), CandidateCoins: []string{"S03USDT"}},
	} {
		if err := st.Decision().LogDecision(&d); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&store.DecisionRecordDB{TraderID: "bad", Timestamp: base, CandidateCoins: "malformed"}).Error; err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	s := &Server{store: st}
	s.route(router.Group("/api"), "GET", "/breakout/trend-short-shadow", "shadow", s.handleTrendShortShadow)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/breakout/trend-short-shadow", nil))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var got trendShadowStats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.ShadowOnly || got.Counts["picks"] != 11 || got.Counts["sampled_hours"] != 2 || got.PerSymbol["MONUSDT"] != 1 {
		t.Fatalf("counts %+v", got)
	}
	for _, h := range []string{"h4", "h24"} {
		e := got.Edge[h]
		if e.N != 2 || e.Mean == nil || *e.Mean != 2 || e.T == nil || *e.T != 2 {
			t.Fatalf("hour edge %s=%+v (ten correlated picks must count once)", h, e)
		}
	}
	if got.Picks.H24.N != 11 || math.Abs(*got.Picks.H24.Mean-58.0/11) > 1e-9 || *got.Picks.H24.Median != 5 || *got.Picks.H24.WinRate != 1 {
		t.Fatalf("picks %+v", got.Picks)
	}
	if got.Controls.H24.N != 2 || *got.Controls.H24.Mean != 4.5 || *got.Controls.H24.Median != 4.5 || *got.Controls.H24.WinRate != 1 {
		t.Fatalf("controls %+v", got.Controls)
	}
	p := got.Picks.Path
	if p.N != 11 || p.TPFirst != 9 || p.SLFirst != 1 || p.Timeout != 1 || math.Abs(*p.MeanR-17.5/11) > 1e-9 {
		t.Fatalf("path %+v", p)
	}
	if got.OverlapN != 11 || got.OverlapMatched != 3 || math.Abs(*got.OverlapLivePool-3.0/11) > 1e-9 {
		t.Fatalf("overlap %+v", got)
	}
}
func TestTrendShortShadowAPIEmptyAndIncompleteControls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.jsonl")
	breakout.SetTrendShortShadowPath(path)
	t.Cleanup(func() { breakout.SetTrendShortShadowPath("data/trendshort_shadow.jsonl") })
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s := &Server{}
	router.GET("/api/breakout/trend-short-shadow", s.handleTrendShortShadow)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/breakout/trend-short-shadow", nil))
	if w.Code != 200 {
		t.Fatalf("empty journal %s", w.Body.String())
	}
	var got trendShadowStats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Picks.H24.N != 0 || got.Edge["h24"].T != nil || got.OverlapLivePool != nil {
		t.Fatalf("empty stats %+v", got)
	}
	rows := trendAPIRows(time.Now().Truncate(time.Hour))
	rows[len(rows)-1].ControlLabels["BUSDT"] = breakout.TrendShortLabels{} // Partial control hour must not enter edge.
	got = aggregateTrendShadow(rows)
	if got.Controls.H24.N != 1 || got.Edge["h24"].N != 1 || got.Edge["h24"].T != nil {
		t.Fatalf("partial controls %+v", got)
	}
	for _, xs := range [][]float64{{1, 1}, {0, 0}, {-1, 1}} {
		e := trendEdge(xs)
		if xs[0] == 1 && (!e.TInfinite || e.T != nil) {
			t.Fatalf("constant nonzero %+v", e)
		}
		if xs[0] != 1 && (e.T == nil || *e.T != 0) {
			t.Fatalf("zero mean %+v", e)
		}
	}
	if err := os.WriteFile(path, []byte("bad journal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/breakout/trend-short-shadow", nil))
	if w.Code != 500 {
		t.Fatalf("corrupt journal status %d", w.Code)
	}
}
