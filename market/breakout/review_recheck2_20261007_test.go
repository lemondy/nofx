package breakout

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// ── recheck 2026-10-07 (CODE_REVIEW_2026-10-07_RECHECK.md) R3/R4 ──

// R3: retention is a FIXED anti-bloat cap (90d) — the file is shared across
// strategies, so a default-7d scan must never destroy a 30d consumer's
// history, and the per-strategy window applies at consumption time.
func TestFix8GainerRetentionIsSharedAndBounded(t *testing.T) {
	setGainerHistoryPath(filepath.Join(t.TempDir(), "gainers.json"))
	now := time.Now()

	// Seed the shared file DIRECTLY with days at three ages (recordGainerHistory
	// always writes "today" — backdating its clock would just shift the prune
	// horizon with it). Then one live record triggers the prune.
	gainerHistMu.Lock()
	hist := &gainerHistoryFile{Days: map[string][]gainerHistEntry{
		now.AddDate(0, 0, -20).UTC().Format("2006-01-02"): {{Symbol: "OLD20USDT", Chg: 10}},
		now.AddDate(0, 0, -95).UTC().Format("2006-01-02"): {{Symbol: "OLD95USDT", Chg: 10}},
	}}
	saveGainerHistoryLocked(hist)
	gainerHistMu.Unlock()

	recordGainerHistory([]GainerQuote{{Symbol: "AAAUSDT", ChgPct: 10, Price: 1}}, now)

	hist = loadGainerHistory()
	key20 := now.AddDate(0, 0, -20).UTC().Format("2006-01-02")
	key95 := now.AddDate(0, 0, -95).UTC().Format("2006-01-02")
	if _, ok := hist.Days[key20]; !ok {
		t.Fatalf("20-day-old day pruned — the fixed cap must cover any supported window (a 7d caller ran last in the old scheme)")
	}
	if _, ok := hist.Days[key95]; ok {
		t.Fatalf("95-day-old day survived — the anti-bloat cap is dead")
	}
}

func newFix8LabelServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/fapi/v1/fundingRate" {
			json.NewEncoder(w).Encode([]map[string]interface{}{{"fundingRate": "0.0005", "fundingTime": 0}})
			return
		}
		// historicalShortLabel's 1m klines probe — echo the requested open
		// time (misalignment guard) with close=90 at index 4.
		openMs := r.URL.Query().Get("startTime")
		json.NewEncoder(w).Encode([][]interface{}{{openMs, "100", "101", "99", "90", "0", "0", 0, "0", "0", "0", "0"}})
	}))
}

// R4: a matured v1 label must MIGRATE on the merge pass — the fresh read
// re-applies the legacy invalidation, so the re-labelled result is applied
// instead of being skipped (the 6cc2d726 lock-free refactor regression).
func TestFix8ShortTunerLegacyLabelMigrates(t *testing.T) {
	dir := t.TempDir()
	setGainerHistoryPath(filepath.Join(dir, "gainers.json"))

	srv := newFix8LabelServer()
	oldHTTP := binanceHTTP
	binanceHTTP = srv.Client()
	defer func() {
		binanceHTTP = oldHTTP
		srv.Close()
	}()
	t.Setenv("BINANCE_FAPI_BASE", srv.URL)

	oldJournal := shortTuningPath
	oldProposal := shortWeightProposalPath
	shortTuningPath = filepath.Join(dir, "signals.jsonl")
	shortWeightProposalPath = filepath.Join(dir, "proposal.json")
	t.Cleanup(func() {
		shortTuningPath = oldJournal
		shortWeightProposalPath = oldProposal
	})

	now := time.Now().UTC()
	legacy := shortSample{
		TS: now.Add(-48 * time.Hour).UnixMilli(), Symbol: "LEGACYUSDT", Price: 100,
		Evaluated: true, Outcome: 99, LabelVersion: 1, // v1 label from the old ticker scheme
	}
	if err := writeSamples([]shortSample{legacy}); err != nil {
		t.Fatal(err)
	}

	RunShortTuner(now)

	samples := readSamples()
	if len(samples) != 1 {
		t.Fatalf("journal size %d, want 1", len(samples))
	}
	got := samples[0]
	if !got.Evaluated || got.LabelVersion != 2 {
		t.Fatalf("v1 label not migrated: evaluated=%v version=%d — the merge pass skipped it", got.Evaluated, got.LabelVersion)
	}
	if got.Outcome == 99 {
		t.Fatal("outcome still the legacy value — the fresh label was discarded")
	}
	if got.Unpriceable {
		t.Fatalf("label marked unpriceable: %s", got.MissingReason)
	}
}
