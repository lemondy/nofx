package gate

import (
	"context"
	"fmt"
	"github.com/gateio/gateapi-go/v6"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestOpenPreservesProtectionAndConvertsBaseOnce(t *testing.T) {
	for _, mult := range []float64{.01, 10} {
		for _, fail := range []string{"", "leverage", "orders"} {
			for _, short := range []bool{false, true} {
				protected := true
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == "DELETE" {
						protected = false
					}
					if strings.HasSuffix(r.URL.Path, "/orders") && r.Method == "POST" {
						b, _ := io.ReadAll(r.Body)
						size := "2"
						if short {
							size = "-2"
						}
						if !strings.Contains(string(b), `"size":`+size) {
							t.Errorf("incorrect contracts: %s", b)
						}
					}
					if fail != "" && strings.Contains(r.URL.Path, fail) {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"label":"REJECTED","message":"rejected"}`)
						return
					}
					fmt.Fprint(w, `{"id":123,"fill_price":"100"}`)
				}))
				cfg := gateapi.NewConfiguration()
				cfg.BasePath = srv.URL
				tr := &GateTrader{ctx: context.Background(), client: gateapi.NewAPIClient(cfg), contractsCache: map[string]*gateapi.Contract{"X_USDT": {QuantoMultiplier: strconv.FormatFloat(mult, 'f', -1, 64)}}}
				q, _ := tr.FormatQuantity("XUSDT", 2.75*mult)
				qty, _ := strconv.ParseFloat(q, 64)
				if short {
					tr.OpenShort("XUSDT", qty, 3)
				} else {
					tr.OpenLong("XUSDT", qty, 3)
				}
				srv.Close()
				if !protected {
					t.Fatalf("existing opposite stop canceled")
				}
			}
		}
	}
}
