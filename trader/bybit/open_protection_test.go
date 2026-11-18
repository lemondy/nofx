package bybit

import (
	"fmt"
	bybit "github.com/bybit-exchange/bybit.go.api"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenPreservesExistingProtection(t *testing.T) {
	for _, fail := range []string{"", "leverage", "create"} {
		for _, short := range []bool{false, true} {
			protected := true
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "cancel") {
					protected = false
				}
				if fail != "" && strings.Contains(r.URL.Path, fail) {
					fmt.Fprint(w, `{"retCode":10001,"retMsg":"rejected","result":{}}`)
					return
				}
				fmt.Fprint(w, `{"retCode":0,"retMsg":"OK","result":{"orderId":"123"}}`)
			}))
			tr := &BybitTrader{client: bybit.NewBybitHttpClient("key", "secret", bybit.WithBaseURL(srv.URL)), qtyStepCache: map[string]float64{"XUSDT": 1}}
			if short {
				tr.OpenShort("XUSDT", 2, 3)
			} else {
				tr.OpenLong("XUSDT", 2, 3)
			}
			srv.Close()
			if !protected {
				t.Fatalf("fail=%s short=%v existing opposite stop canceled", fail, short)
			}
		}
	}
}
