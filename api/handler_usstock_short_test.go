package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nofx/store"
)

func TestUSStockPaperFallbackShortEquityAndSide(t *testing.T) {
	srv, r, token, _, id := idorFixture(t)
	protected := r.Group("/p", srv.authMiddleware())
	protected.GET("/paper/:trader_id", srv.handleUSStockPaper)
	sp := srv.store.StockPaper()
	if _, err := sp.EnsureAccount(id, 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sp.OpenOrAddSideWithFee(id, "AAPLBUSDT", "SHORT", 2, 100, 105, 90, 4, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sp.OpenOrAddSideWithFee(id, "MSFTBUSDT", "LONG", 1, 200, 190, 220, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/p/paper/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Equity float64                    `json:"equity"`
		Open   []store.StockPaperPosition `json:"open_positions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if math.Abs(body.Equity-999.92) > 1e-8 || len(body.Open) != 2 || body.Open[0].Side != "SHORT" || body.Open[1].Side != "LONG" {
		t.Fatalf("%s", rec.Body.String())
	}
}
