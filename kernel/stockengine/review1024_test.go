package stockengine

import (
	"context"
	"testing"
	"time"

	"nofx/market/usstock"
)

func TestReview1024ScanRejectsStaleDaily(t *testing.T) {
	report, err := Scan(context.Background(), []usstock.SymbolInfo{{Symbol: "AAPLBUSDT"}}, nil,
		func(context.Context, string) ([]usstock.Bar, string, error) {
			return cycleTestPop(), usstock.SourceYahoo, nil
		},
		time.Date(2026, 10, 22, 19, 30, 0, 0, time.UTC))
	if err != nil || len(report.Results) != 0 || report.Skipped["AAPLBUSDT"] == "" {
		t.Fatalf("stale scan accepted: report=%+v err=%v", report, err)
	}
}

func TestReview1024BStockCalendarDaysNotMomentum(t *testing.T) {
	now := time.Date(2026, 10, 22, 19, 30, 0, 0, time.UTC)
	bars := cycleTestBars(cycleTestPath(260, 100, 1), nil)
	for i := range bars {
		bars[i].OpenTime = time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i-len(bars)+1)
	}
	report, err := Scan(context.Background(), []usstock.SymbolInfo{{Symbol: "AAPLBUSDT"}}, nil,
		func(context.Context, string) ([]usstock.Bar, string, error) { return bars, usstock.SourceBStock, nil }, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.TopMomentum(1, nil)) != 0 {
		t.Fatal("24/7 calendar-day series entered momentum ranking")
	}
}

func TestReview1024RemovedHoldingExitAuthorization(t *testing.T) {
	for _, action := range []string{ActionHold, ActionReduceLong, ActionCloseLong, ActionAdjustStop, ActionAddLong, ActionOpenLong} {
		t.Run(action, func(t *testing.T) {
			c := testContext()
			holding(c)
			c.Config.Symbols = []string{"MSFTBUSDT"}
			d := buy("AAPLBUSDT")
			d.Action = action
			d.StopLoss = 96
			d.ReduceFraction = .5
			v := Validate(c, []Decision{d})[0]
			if action == ActionOpenLong || action == ActionAddLong {
				assertCode(t, v, "EXIT_ONLY_SYMBOL")
			} else {
				assertCode(t, v, "")
			}
		})
	}
}

func TestReview1024SnapshotStaleBarsBlockOnlyEntries(t *testing.T) {
	c := testContext()
	holding(c)
	daily := risingSeries(usstock.TF1d, usstock.SourceYahoo, 260)
	weekly := risingSeries(usstock.TF1w, usstock.SourceYahoo, 30)
	s := BuildSnapshot("AAPLBUSDT", "AAPL", map[string]*usstock.Series{usstock.TF1d: daily, usstock.TF1w: weekly}, &usstock.Quote{BStockPrice: 100, RefFresh: true}, c.Now)
	if !contains(s.MissingTF, usstock.TF1d) || !contains(s.MissingTF, usstock.TF1w) || s.DataThrough[usstock.TF1d] == "" {
		t.Fatalf("stale periods not marked: %+v", s)
	}
	c.Snapshots = []*SymbolSnapshot{s}
	for _, action := range []string{ActionHold, ActionReduceLong, ActionCloseLong, ActionAddLong} {
		d := buy("AAPLBUSDT")
		d.Action = action
		d.ReduceFraction = .5
		code := ""
		if action == ActionAddLong {
			code = "DATA_INSUFFICIENT"
		}
		assertCode(t, Validate(c, []Decision{d})[0], code)
	}
}

func TestReview1024ExplicitEntryAuthorization(t *testing.T) {
	c := testContext()
	holding(c)
	c.EntrySymbols = map[string]bool{"MSFTBUSDT": true}
	d := buy("AAPLBUSDT")
	d.Action = ActionAddLong
	assertCode(t, Validate(c, []Decision{d})[0], "EXIT_ONLY_SYMBOL")
	d.Action = ActionCloseLong
	assertCode(t, Validate(c, []Decision{d})[0], "")
}

func TestReview1024ScanCacheExpiresAfterSettlementGrace(t *testing.T) {
	env := setupScanTest(t)
	before := time.Date(2026, 10, 14, 19, 59, 0, 0, time.UTC) // 15:59 ET
	env.now.Store(before.UnixNano())
	first, err := GetScanReport(context.Background(), 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	env.now.Store(time.Date(2026, 10, 14, 21, 29, 0, 0, time.UTC).UnixNano())
	same, err := GetScanReport(context.Background(), 48*time.Hour)
	if err != nil || same != first {
		t.Fatalf("grace not honored: err=%v", err)
	}
	env.now.Store(time.Date(2026, 10, 14, 21, 30, 0, 0, time.UTC).UnixNano())
	next, err := GetScanReport(context.Background(), 48*time.Hour)
	if err != nil || next == first || env.fetches.Load() != 2 || next.Results[0].DataThrough != "2026-10-14" {
		t.Fatalf("settlement date did not expire cache: report=%+v err=%v fetches=%d", next, err, env.fetches.Load())
	}
}

func TestReview1024ReportChecksAgeAndIndividualDates(t *testing.T) {
	now := time.Date(2026, 10, 22, 19, 30, 0, 0, time.UTC)
	r := &ScanReport{GeneratedAt: now, Results: []ScanResult{{Symbol: "FRESH", DataThrough: "2026-10-21"}, {Symbol: "OLD", DataThrough: "2026-10-20"}, {Symbol: "UNKNOWN"}}}
	if rows := r.FreshResults(now, time.Hour); len(rows) != 1 || rows[0].Symbol != "FRESH" {
		t.Fatalf("stale rows accepted: %+v", rows)
	}
	r.GeneratedAt = now.Add(-time.Hour)
	if len(r.FreshResults(now, time.Hour)) != 0 {
		t.Fatal("report deadline must be exclusive")
	}
	r.GeneratedAt = now.Add(time.Second)
	if len(r.FreshResults(now, time.Hour)) != 0 {
		t.Fatal("future report accepted")
	}
}
