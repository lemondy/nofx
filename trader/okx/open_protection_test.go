package okx

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type protectionTransport func(*http.Request) (*http.Response, error)

func (f protectionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestOpenPreservesProtectionAndConvertsBaseOnce(t *testing.T) {
	for _, mult := range []float64{.01, 10} {
		for _, fail := range []bool{false, true} {
			for _, short := range []bool{false, true} {
				protected := true
				tr := &OKXTrader{instrumentsCacheTime: time.Now(), instrumentsCache: map[string]*OKXInstrument{"X-USDT-SWAP": {CtVal: mult, LotSz: 1, MinSz: 1}}}
				tr.httpClient = &http.Client{Transport: protectionTransport(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "cancel") {
						protected = false
					}
					body := `{"code":"0","data":[{"ordId":"123","sCode":"0"}]}`
					if strings.HasSuffix(r.URL.Path, "/order") {
						b, _ := io.ReadAll(r.Body)
						if !strings.Contains(string(b), `"sz":"2"`) {
							t.Errorf("incorrect contracts: %s", b)
						}
						if fail {
							body = `{"code":"1","msg":"rejected"}`
						}
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
				})}
				q, _ := tr.FormatQuantity("XUSDT", 2.75*mult)
				qty, _ := strconv.ParseFloat(q, 64)
				if short {
					tr.OpenShort("XUSDT", qty, 3)
				} else {
					tr.OpenLong("XUSDT", qty, 3)
				}
				if !protected {
					t.Fatalf("existing opposite stop canceled")
				}
			}
		}
	}
}
