package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"nofx/store"
	"strings"
	"testing"
)

func TestReviewFactorQuantSwitchToCrypto(t *testing.T) {
	srv, r, token, user, _ := idorFixture(t)
	initial := `{"strategy_type":"factor_quant","factor_quant_config":{"market":"us","us_short_mode":"paper","us_short_fee_bps":4}}`
	if err := srv.store.Strategy().Create(&store.Strategy{ID: "fq-switch", UserID: user, Name: "FQ", Config: initial}); err != nil {
		t.Fatal(err)
	}
	r.PUT("/strategies/:id", srv.authMiddleware(), srv.handleUpdateStrategy)
	body := `{"config":{"factor_quant_config":{"market":"crypto","horizon":3,"us_short_mode":"","us_short_fee_bps":null}}}`
	req := httptest.NewRequest(http.MethodPut, "/strategies/fq-switch", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	saved, err := srv.store.Strategy().Get(user, "fq-switch")
	if err != nil {
		t.Fatal(err)
	}
	var cfg store.StrategyConfig
	if err := json.Unmarshal([]byte(saved.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.FactorQuantConfig.USShortMode != "" || cfg.FactorQuantConfig.USShortFeeBps != nil {
		t.Fatalf("hidden US state retained: %+v", cfg.FactorQuantConfig)
	}
}
