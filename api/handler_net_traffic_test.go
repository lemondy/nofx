package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nofx/netmeter"
)

type netTrafficTestTransport struct{}

func (netTrafficTestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("payload")), ContentLength: 7}, nil
}

func TestNetTrafficPathsAndExistingFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{}
	router := gin.New()
	router.GET("/api/net-traffic", func(c *gin.Context) {
		c.Set("user_id", "traffic-user")
		s.handleNetTraffic(c)
	})
	// Exercise the real counting layer without any network or database access.
	// More than 30 hosts ensures the host list is retained in full.
	rt := netmeter.Wrap(netTrafficTestTransport{})
	for i := 0; i < 35; i++ {
		req, err := http.NewRequest(http.MethodGet, "https://traffic-"+strconv.Itoa(i)+".test/fapi/v1/klines?symbol=BTCUSDT", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	const credential = "123456:ABC-DEF_secret_TOKEN123456"
	req, err := http.NewRequest(http.MethodGet, "https://api.telegram.test/bot"+credential+"/sendMessage", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	totals, hosts := netmeter.Snapshot()
	wantPaths := netmeter.SnapshotPaths(30)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/net-traffic", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"since": totals.Since.UTC().Format("2006-01-02T15:04:05Z"), "since_unix": totals.Since.Unix(),
		"requests": totals.Requests, "http_up": totals.HTTPUp, "http_down": totals.HTTPDown,
		"ws_up": totals.WsUp, "ws_down": totals.WsDown, "hosts": hosts, "paths": wantPaths,
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected response fields: %s", w.Body.String())
	}
	for field, value := range want {
		if field == "hosts" { // Snapshot does not specify an order for host byte ties.
			continue
		}
		expected, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if string(got[field]) != string(expected) {
			t.Errorf("field %s = %s, want %s", field, got[field], expected)
		}
	}
	var gotHosts []netmeter.HostUsage
	if err := json.Unmarshal(got["hosts"], &gotHosts); err != nil {
		t.Fatal(err)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Host < hosts[j].Host })
	sort.Slice(gotHosts, func(i, j int) bool { return gotHosts[i].Host < gotHosts[j].Host })
	if !reflect.DeepEqual(gotHosts, hosts) {
		t.Fatalf("hosts changed or truncated: got %+v, want %+v", gotHosts, hosts)
	}
	var paths []netmeter.PathUsage
	if err := json.Unmarshal(got["paths"], &paths); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 30 || len(hosts) < 36 || !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("unexpected Top 30 or truncated hosts: paths=%+v hosts=%d", paths, len(hosts))
	}
	foundTelegram := false
	for _, path := range paths {
		if path.Host == "api.telegram.test" {
			foundTelegram = path.Path == "/bot:token/sendMessage"
		}
	}
	if !foundTelegram || strings.Contains(w.Body.String(), "ABC-DEF") || strings.Contains(w.Body.String(), "123456") {
		t.Fatalf("Telegram path missing or credential leaked: %s", w.Body.String())
	}
}

func TestNetTrafficRequiresAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s := &Server{}
	router.GET("/api/net-traffic", s.handleNetTraffic)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/net-traffic", nil))
	if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "paths") {
		t.Fatalf("unauthenticated response %d: %s", w.Code, w.Body.String())
	}
}
