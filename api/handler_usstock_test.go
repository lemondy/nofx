package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nofx/market/usstock"
)

func TestUSStockSymbolsShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	old := usstockListSymbols
	t.Cleanup(func() { usstockListSymbols = old })
	usstockListSymbols = func(context.Context) ([]usstock.SymbolInfo, error) {
		return []usstock.SymbolInfo{{Symbol: "AAPLBUSDT", BaseAsset: "AAPLB", Underlying: "AAPL", TickSize: 0.01}}, nil
	}
	r := gin.New()
	s := &Server{}
	r.GET("/api/usstock/symbols", s.handleUSStockSymbols)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/usstock/symbols", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string][]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got := body["symbols"]
	if len(got) != 1 || got[0]["symbol"] != "AAPLBUSDT" || got[0]["underlying"] != "AAPL" || got[0]["base_asset"] != "AAPLB" || len(got[0]) != 3 {
		t.Fatalf("unexpected payload %s", rec.Body.String())
	}
}

func TestUSStockPaperEndpointIsOwnerScoped(t *testing.T) {
	srv, r, tokenA, _, traderA := idorFixture(t)
	protected := r.Group("/p", srv.authMiddleware())
	protected.GET("/paper/:trader_id", srv.handleUSStockPaper)
	if _, err := srv.store.StockPaper().EnsureAccount(traderA, 5000); err != nil {
		t.Fatal(err)
	}
	get := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/p/paper/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+tokenA)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(traderA); rec.Code != http.StatusOK {
		t.Fatalf("own trader: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("trader-B"); rec.Code == http.StatusOK {
		t.Fatal("another user's paper ledger must not be readable")
	}
}
