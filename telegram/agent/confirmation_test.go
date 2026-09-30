package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMutationsNeedExactOneTimeConfirmation(t *testing.T) {
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writes.Add(1); w.Write([]byte("ok")) }))
	defer srv.Close()
	tool := &apiCallTool{baseURL: srv.URL, token: "test", client: srv.Client()}
	req := &apiRequest{Method: "POST", Path: "/api/traders/t/start", Body: map[string]any{"name": "original"}}
	if result := tool.execute(req); !strings.HasPrefix(result, "Confirmation required") {
		t.Fatal(result)
	}
	code := tool.confirmation
	req.Path = "/api/traders/other/start"
	if writes.Load() != 0 {
		t.Fatal("unconfirmed request executed")
	}
	if result := tool.confirm("wrong"); !strings.Contains(result, "invalid") {
		t.Fatal(result)
	}
	if tool.pending.Path != "/api/traders/t/start" {
		t.Fatal("confirmation request mutated")
	}
	if result := tool.confirm(code); result != "ok" || writes.Load() != 1 {
		t.Fatalf("%s writes=%d", result, writes.Load())
	}
	tool.confirm(code)
	if writes.Load() != 1 {
		t.Fatal("replay executed")
	}
	tool.execute(&apiRequest{Method: "DELETE", Path: "/api/traders/t"})
	tool.expires = time.Now().Add(-time.Second)
	tool.confirm(tool.confirmation)
	for _, path := range []string{"/api/user/password", "/api/telegram", "https://example.com/api/traders", "/api/traders/../user"} {
		tool.execute(&apiRequest{Method: "POST", Path: path})
	}
	if writes.Load() != 1 {
		t.Fatal("expired or restricted request executed")
	}
}
