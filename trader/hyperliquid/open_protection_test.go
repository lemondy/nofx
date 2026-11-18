package hyperliquid

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/crypto"
	hl "github.com/sonirico/go-hyperliquid"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenPreservesExistingProtection(t *testing.T) {
	for _, fail := range []string{"", "updateLeverage", "order"} {
		for _, short := range []bool{false, true} {
			protected := true
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				body := string(b)
				if strings.Contains(body, "cancel") || strings.Contains(body, "openOrders") {
					protected = false
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(body, "allMids") {
					fmt.Fprint(w, `{"BTC":"100"}`)
					return
				}
				if fail != "" && strings.Contains(body, `"type":"`+fail+`"`) {
					w.WriteHeader(400)
					fmt.Fprint(w, `"rejected"`)
					return
				}
				if strings.Contains(body, `"type":"order"`) {
					fmt.Fprint(w, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"totalSz":"2","avgPx":"100","oid":123}}]}}}`)
					return
				}
				fmt.Fprint(w, `{"status":"ok","response":{"type":"default"}}`)
			}))
			key, _ := crypto.GenerateKey()
			meta := &hl.Meta{Universe: []hl.AssetInfo{{Name: "BTC", SzDecimals: 2}}}
			ex := hl.NewExchange(context.Background(), key, srv.URL, meta, "", "", &hl.SpotMeta{})
			tr := &HyperliquidTrader{ctx: context.Background(), exchange: ex, meta: meta}
			var err error
			if short {
				_, err = tr.OpenShort("BTCUSDT", 2, 3)
			} else {
				_, err = tr.OpenLong("BTCUSDT", 2, 3)
			}
			srv.Close()
			if fail == "" && err != nil {
				t.Fatal(err)
			}
			if !protected {
				t.Fatalf("fail=%s short=%v existing opposite stop canceled", fail, short)
			}
		}
	}
}
