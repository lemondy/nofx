package market

import (
	"testing"
	"time"
)

func TestParseFundingSettlements(t *testing.T) {
	body := []byte(`[{"symbol":"BTCUSDT","fundingTime":1700000000005,"fundingRate":"0.00010000","markPrice":"37000.5"},
{"symbol":"BTCUSDT","fundingTime":1700028800000,"fundingRate":"-0.00020000","markPrice":""}]`)
	got, err := parseFundingSettlements(body)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if got[0].Rate != 0.0001 || got[0].MarkPrice != 37000.5 || !got[0].Time.Equal(time.UnixMilli(1700000000005)) {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].Rate != -0.0002 || got[1].MarkPrice != 0 {
		t.Fatalf("second = %+v", got[1])
	}
	if _, err := parseFundingSettlements([]byte(`[{"fundingTime":1,"fundingRate":"x"}]`)); err == nil {
		t.Fatal("bad rate accepted")
	}
	if _, err := parseFundingSettlements([]byte(`{"code":-1121}`)); err == nil {
		t.Fatal("error payload accepted")
	}
	if got, err := parseFundingSettlements([]byte(`[]`)); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

func TestGetFundingSettlementsEmptyRange(t *testing.T) {
	now := time.Now()
	if got, err := GetFundingSettlementsPriority("BTCUSDT", now, now, 0); err != nil || got != nil {
		t.Fatalf("empty range must not call the network: %v %v", got, err)
	}
}
