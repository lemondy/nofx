package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStopPersistsIntentWhileReloadInstanceAbsent(t *testing.T) {
	srv, r, token, user, id := idorFixture(t)
	if err := srv.store.Trader().UpdateStatus(user, id, true); err != nil {
		t.Fatal(err)
	}
	r.POST("/stop/:id", srv.authMiddleware(), srv.handleStopTrader)
	req := httptest.NewRequest(http.MethodPost, "/stop/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stop missing instance: %d %s", w.Code, w.Body.String())
	}
	row, err := srv.store.Trader().GetForUser(user, id)
	if err != nil || row.IsRunning {
		t.Fatalf("stop not durable: %+v %v", row, err)
	}
}
