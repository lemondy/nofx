package api

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReviewPaginationValidation(t *testing.T) {
	for _, query := range []string{"?offset=-1", "?offset=wat", "?limit=no", "?limit=-1"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/"+query, nil)
		if _, _, ok := reviewPagination(c); ok {
			t.Fatalf("accepted %s", query)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?limit=9999", nil)
	limit, offset, ok := reviewPagination(c)
	if !ok || limit != 500 || offset != 0 {
		t.Fatalf("%d %d %v", limit, offset, ok)
	}
}
func TestPromotionReportCorruptionStatusReview(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMOTION_DIR", dir)
	writeFactorMiningTestFile(t, dir, "report.json", "broken")
	rec := httptest.NewRecorder()
	promotionTestRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/promotion/report", nil))
	var body map[string]json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body["errors"]) == 0 {
		t.Fatal("corruption hidden as empty report")
	}
}
func TestFactorMiningCorruptionStatusReview(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_MINING_DIR", dir)
	writeFactorMiningTestFile(t, dir, "status.json", "broken")
	body := factorMiningTestGet(t, factorMiningTestRouter(), "/api/factor-mining/overview")
	if len(body["errors"]) == 0 {
		t.Fatal("corruption hidden")
	}
}

func TestQualityOwnershipReview(t *testing.T) {
	srv, r, token, _, traderID := idorFixture(t)
	r.GET("/quality", srv.authMiddleware(), srv.handleSystemQuality)
	for _, tc := range []struct {
		id   string
		want int
	}{{traderID, 200}, {"trader-B", 403}, {"missing", 403}} {
		req := httptest.NewRequest("GET", "/quality?trader_id="+tc.id, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.id, rec.Code, rec.Body.String())
		}
	}
}
