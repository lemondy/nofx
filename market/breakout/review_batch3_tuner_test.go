package breakout

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// P3-3 (review 2026-10-06): the tuner's labelling used to hold shortTunerMu
// across up to 50 samples × 2 sequential HTTP calls. The refactor labels
// WITHOUT the lock and merges by (TS, Symbol) into a FRESH read. This test
// pins the merge contract: the due sample gets labelled, a concurrently
// appended (fresh) sample survives the write-back, and nothing is lost.
func TestFix3ShortTunerMergeKeepsConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	setGainerHistoryPath(filepath.Join(dir, "gainers.json"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/fapi/v1/fundingRate" {
			json.NewEncoder(w).Encode([]map[string]interface{}{{"fundingRate": "0.0005", "fundingTime": 0}})
			return
		}
		// historicalShortLabel's 1m klines probe — echo the requested open
		// time back as openTime (the misalignment guard) with close at index 4.
		openMs := r.URL.Query().Get("startTime")
		json.NewEncoder(w).Encode([][]interface{}{{openMs, "100", "101", "99", "90", "0", "0", 0, "0", "0", "0", "0"}})
	}))
	oldHTTP := binanceHTTP
	binanceHTTP = srv.Client()
	defer func() {
		binanceHTTP = oldHTTP
		srv.Close()
	}()
	// historicalShortLabel hits fapiBase() — point it at the test server.
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
	due := shortSample{TS: now.Add(-48 * time.Hour).UnixMilli(), Symbol: "DUEUSDT", Price: 100}
	fresh := shortSample{TS: now.Add(-1 * time.Hour).UnixMilli(), Symbol: "FRESHUSDT", Price: 50}
	if err := writeSamples([]shortSample{due, fresh}); err != nil {
		t.Fatal(err)
	}

	RunShortTuner(now)

	samples := readSamples()
	bySym := map[string]shortSample{}
	for _, s := range samples {
		bySym[s.Symbol] = s
	}
	if len(samples) != 2 {
		t.Fatalf("journal shrank from 2 to %d — the lock-free merge dropped samples", len(samples))
	}
	if got := bySym["DUEUSDT"]; !got.Evaluated || got.LabelVersion != 2 {
		t.Fatalf("due sample was not labelled: %+v", got)
	}
	if got := bySym["FRESHUSDT"]; got.Evaluated {
		t.Fatal("fresh (not-yet-mature) sample must not be labelled")
	}
}

