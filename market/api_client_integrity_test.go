package market

import (
	"testing"
)

func TestParseKlineRejectsInvalid(t *testing.T) {
	good := KlineResponse{float64(1), "10", "12", "9", "11", "1", float64(2), "1", float64(1), "1", "1"}
	for _, tc := range []struct {
		i int
		v interface{}
	}{{0, nil}, {1, "NaN"}, {2, "8"}, {3, "Inf"}, {5, "-1"}, {8, []int{1}}, {10, "bad"}} {
		row := append(KlineResponse(nil), good...)
		row[tc.i] = tc.v
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			if _, err := parseKline(row); err == nil {
				t.Errorf("accepted field %d=%v", tc.i, tc.v)
			}
		}()
	}
}
