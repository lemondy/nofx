package breakout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withTempGainerHistory(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gainer_history.json")
	old := gainerHistPath
	gainerHistPath = p
	t.Cleanup(func() { gainerHistPath = old })
}

func readGainerHistoryFile(t *testing.T) *gainerHistoryFile {
	t.Helper()
	b, err := os.ReadFile(gainerHistPath)
	if err != nil {
		t.Fatalf("history file missing: %v", err)
	}
	var hist gainerHistoryFile
	if err := json.Unmarshal(b, &hist); err != nil {
		t.Fatalf("bad history json: %v", err)
	}
	return &hist
}

// Each (day, symbol) entry must keep the PEAK change seen that day, only the
// day's nominal Top-20 rows feed the pool, and the file must survive a
// reload (persistence is the whole point of the pool).
func TestRecordGainerHistoryMaxMergeAndBoardKeep(t *testing.T) {
	withTempGainerHistory(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	// 25 quotes: only the top 20 (board keep) may enter the pool. FADESUSDT
	// (peak 60) sits at rank 20; five fillers rank below the cut.
	var board []GainerQuote
	for i := 0; i < 19; i++ {
		board = append(board, GainerQuote{Symbol: string(rune('A'+i)) + "USDT", ChgPct: float64(100 - i), Price: 1})
	}
	board = append(board, GainerQuote{Symbol: "FADESUSDT", ChgPct: 60, Price: 2})
	for i := 0; i < 5; i++ {
		board = append(board, GainerQuote{Symbol: string(rune('V'+i)) + "USDT", ChgPct: float64(35 - i), Price: 1})
	}
	recordGainerHistory(board, now)

	// Second snapshot the same day: FADESUSDT faded 60 → 12 — the peak (60)
	// must survive; NEWUSDT is a fresh top-20 entrant.
	board2 := []GainerQuote{
		{Symbol: "FADESUSDT", ChgPct: 12, Price: 2},
		{Symbol: "NEWUSDT", ChgPct: 55, Price: 3},
	}
	recordGainerHistory(board2, now)

	hist := readGainerHistoryFile(t)
	day := hist.Days[now.UTC().Format("2006-01-02")]
	if len(day) != 21 { // 20 from board1 + NEWUSDT (FADESUSDT already in)
		t.Fatalf("day entries = %d, want 21", len(day))
	}
	bySym := map[string]gainerHistEntry{}
	for _, e := range day {
		bySym[e.Symbol] = e
	}
	if e := bySym["AUSDT"]; e.Chg != 100 {
		t.Fatalf("AUSDT chg = %v, want 100 (board keep = top 20)", e.Chg)
	}
	if _, ok := bySym["ZUSDT"]; ok { // rank-25 filler must be cut
		t.Fatalf("rank-25 symbol must not be recorded")
	}
	if e := bySym["FADESUSDT"]; e.Chg != 60 {
		t.Fatalf("FADESUSDT kept chg = %v, want 60 (max-merge)", e.Chg)
	}
	if _, ok := bySym["NEWUSDT"]; !ok {
		t.Fatal("NEWUSDT missing from the day's record")
	}

	// Reload via the loader: persistence round-trip.
	hist2 := loadGainerHistory()
	if len(hist2.Days[now.UTC().Format("2006-01-02")]) != 21 {
		t.Fatal("reload lost entries")
	}
}

// Days older than window+buffer are pruned on write; the buffer keeps
// shrink-then-re-enable window changes from destroying data.
func TestRecordGainerHistoryPrune(t *testing.T) {
	withTempGainerHistory(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	hist := &gainerHistoryFile{Days: map[string][]gainerHistEntry{
		now.UTC().AddDate(0, 0, -8).Format("2006-01-02"): {{Symbol: "KEEPLUSDT", Chg: 80}}, // buffer edge (keep 7 + buffer 2 − 1) — kept
		now.UTC().AddDate(0, 0, -9).Format("2006-01-02"): {{Symbol: "OLDUSDT", Chg: 90}},   // beyond — pruned
	}}
	if err := saveGainerHistoryLocked(hist); err != nil {
		t.Fatal(err)
	}
	recordGainerHistory([]GainerQuote{{Symbol: "NEWUSDT", ChgPct: 40, Price: 1}}, now)
	hist2 := readGainerHistoryFile(t)
	if _, ok := hist2.Days[now.UTC().AddDate(0, 0, -8).Format("2006-01-02")]; !ok {
		t.Fatal("buffer-edge day must survive")
	}
	if _, ok := hist2.Days[now.UTC().AddDate(0, 0, -9).Format("2006-01-02")]; ok {
		t.Fatal("day beyond window+buffer must be pruned")
	}
}

// The merged universe: strongest recorded pump first, capped, skipping
// symbols already on the live board and symbols without a live quote
// (delisted / volume faded below the liquidity floor), and honoring the
// window cutoff.
func TestHistoryUniverseCandidates(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	today := now.UTC().Format("2006-01-02")
	d3 := now.UTC().AddDate(0, 0, -3).Format("2006-01-02")
	d9 := now.UTC().AddDate(0, 0, -9).Format("2006-01-02")
	hist := &gainerHistoryFile{Days: map[string][]gainerHistEntry{
		today: {{Symbol: "COVEREDUSDT", Chg: 95}, {Symbol: "FADEUSDT", Chg: 60}},
		d3:    {{Symbol: "OLDPUMPUSDT", Chg: 120}, {Symbol: "DEADUSDT", Chg: 70}},
		d9:    {{Symbol: "EXPIREDUSDT", Chg: 200}}, // outside the 7d window
	}}
	index := map[string]GainerQuote{
		"FADEUSDT":    {Symbol: "FADEUSDT", ChgPct: -5, Price: 2}, // still quoted, off today's board
		"OLDPUMPUSDT": {Symbol: "OLDPUMPUSDT", ChgPct: -12, Price: 3},
		// DEADUSDT: no live quote (delisted/illiquid) — must be skipped
	}
	covered := map[string]bool{"COVEREDUSDT": true}

	got := historyUniverseCandidates(hist, now, 7, index, covered, 30)
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2: %+v", len(got), got)
	}
	// Merit order: OLDPUMPUSDT (peak 120) before FADEUSDT (peak 60).
	if got[0].Symbol != "OLDPUMPUSDT" || got[1].Symbol != "FADEUSDT" {
		t.Fatalf("order = [%s %s], want [OLDPUMPUSDT FADEUSDT]", got[0].Symbol, got[1].Symbol)
	}
	// Analysis input must carry the LIVE quote, not the recorded peak.
	if got[0].ChgPct != -12 {
		t.Fatalf("live chg = %v, want -12", got[0].ChgPct)
	}

	// Cap + disabled-window guards.
	if got := historyUniverseCandidates(hist, now, 7, index, covered, 1); len(got) != 1 {
		t.Fatalf("cap=1 produced %d candidates", len(got))
	}
	if got := historyUniverseCandidates(hist, now, 0, index, covered, 30); len(got) != 0 {
		t.Fatal("disabled window (0) must yield no candidates")
	}
}

func TestResolveShortScanHistoryConfig(t *testing.T) {
	cases := []struct {
		raw, want int
	}{
		{0, DefaultShortScanHistoryDays},
		{-1, 0}, // negative = pool disabled
		{3, 3},
		{14, 14},
	}
	for _, c := range cases {
		if got := ResolveShortScanHistoryDays(c.raw); got != c.want {
			t.Fatalf("ResolveShortScanHistoryDays(%d) = %d, want %d", c.raw, got, c.want)
		}
	}
	maxCases := []struct {
		raw, want int
	}{
		{0, DefaultShortScanHistoryMax},
		{-5, DefaultShortScanHistoryMax},
		{45, 45},
		{200, 100}, // hard ceiling
	}
	for _, c := range maxCases {
		if got := ResolveShortScanHistoryMax(c.raw); got != c.want {
			t.Fatalf("ResolveShortScanHistoryMax(%d) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// A4 (QUANT_REVIEW 09-22): the process-global setter is gone — the resolve
// helpers are now the single semantic source that ScanShorts applies to the
// raw per-call config. Pin their mapping so defaults/disabled/cap behavior
// cannot drift between the kernel (strategy values) and the scheduler (0/0).
func TestResolveShortScanHistorySemantics(t *testing.T) {
	if d, m := ResolveShortScanHistoryDays(0), ResolveShortScanHistoryMax(0); d != 7 || m != 30 {
		t.Fatalf("defaults: days=%d max=%d, want 7/30", d, m)
	}
	if d, m := ResolveShortScanHistoryDays(-1), ResolveShortScanHistoryMax(200); d != 0 || m != 100 {
		t.Fatalf("raw(-1,200): days=%d max=%d, want 0(disabled)/100(cap)", d, m)
	}
}

// A history-pool coin that ALSO passes the grinding-top screen keeps its
// hist_gainer label and gains NearHighAlso — the OI-floor exemption follows
// the near_high screen, not the gainer label.
func TestMergeShortScansHistGainerCollision(t *testing.T) {
	gainers := []ShortSignal{
		{Symbol: "FADEUSDT", Universe: "hist_gainer", Score: 62},
	}
	slowTops := []ShortSignal{
		{Symbol: "FADEUSDT", Universe: "near_high", Score: 62},
	}
	out := mergeShortScans(gainers, slowTops)
	if len(out) != 1 {
		t.Fatalf("merged len = %d, want 1", len(out))
	}
	if out[0].Universe != "hist_gainer" || !out[0].NearHighAlso {
		t.Fatalf("universe=%q nearHighAlso=%v — want hist_gainer kept + flag set", out[0].Universe, out[0].NearHighAlso)
	}
}
