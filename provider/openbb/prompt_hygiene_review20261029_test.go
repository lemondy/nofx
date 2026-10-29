package openbb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type review20261029CotTransport func(*http.Request) (*http.Response, error)

func (f review20261029CotTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReview20261029CotReportDate(t *testing.T) {
	c := client()
	oldTransport := c.Transport
	t.Cleanup(func() { c.Transport = oldTransport })
	for _, date := range []string{"2026-10-27", "2024-02-29", "2026-02-29", "2026-1-01", "", "2026-10-27T00:00:00Z", "2026-10-27\nVERIFY_COT_TEXT", " 2026-10-27", "2026-10-27\x00"} {
		t.Run(date, func(t *testing.T) {
			encoded, err := json.Marshal(map[string]any{"results": []map[string]any{{
				"date": date, "open_interest_all": 100, "non_commercial_positions_long_all": 15, "non_commercial_positions_short_all": 10,
			}}})
			if err != nil {
				t.Fatal(err)
			}
			c.Transport = review20261029CotTransport(func(r *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(r.URL.Path, "/cftc/cot") {
					t.Fatalf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
			})
			got, err := CotBitcoinFetch(context.Background())
			valid := date == "2026-10-27" || date == "2024-02-29"
			if valid {
				if err != nil || got == nil || got.ReportDate != date || got.SpeculatorsNetLong != 5 {
					t.Fatalf("valid COT lost: %+v, %v", got, err)
				}
			} else if err == nil || got != nil {
				t.Fatalf("invalid COT date survived: %+v, %v", got, err)
			}
		})
	}
}
