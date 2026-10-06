package breakout

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestFix05HistoricalLabelDoesNotStretchAfterDowntime(t *testing.T) {
	entry := time.Date(2026, 9, 1, 12, 34, 20, 0, time.UTC)
	s := shortSample{TS: entry.UnixMilli(), Symbol: "XUSDT", Price: 100}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/fapi/v1/fundingRate" {
			json.NewEncoder(w).Encode([]map[string]interface{}{{"fundingRate": "0.001", "fundingTime": entry.Add(8 * time.Hour).UnixMilli()}})
			return
		}
		if r.URL.Path != "/fapi/v1/klines" {
			t.Errorf("unexpected current ticker request %s", r.URL.Path)
		}
		open, _ := strconv.ParseInt(r.URL.Query().Get("startTime"), 10, 64)
		want := entry.Add(24 * time.Hour).Truncate(time.Minute).Add(-time.Minute).UnixMilli()
		if open != want {
			t.Errorf("label uses %d, want fixed horizon %d", open, want)
		}
		json.NewEncoder(w).Encode([][]interface{}{{open, "100", "100", "90", "90", "1", open + 59999}})
	}))
	defer srv.Close()
	t.Setenv("BINANCE_FAPI_BASE", srv.URL)
	old := binanceHTTP
	binanceHTTP = srv.Client()
	t.Cleanup(func() { binanceHTTP = old })
	price, at, funding, err := historicalShortLabel(s)
	if err != nil || price != 90 || funding != 0.1 || at != entry.Add(24*time.Hour).Truncate(time.Minute).UnixMilli() {
		t.Fatalf("price=%v at=%v funding=%v err=%v", price, at, funding, err)
	}
}

func TestFix05ScannersIgnoreFormingCandles(t *testing.T) {
	now := time.Now().UTC().Truncate(15 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rows [][]interface{}
		for _, open := range []time.Time{now.Add(-15 * time.Minute), now} {
			rows = append(rows, []interface{}{open.UnixMilli(), "100", "101", "99", "100", "1", open.Add(15*time.Minute).UnixMilli() - 1, "100", 1, "1", "100"})
		}
		json.NewEncoder(w).Encode(rows)
	}))
	defer srv.Close()
	old := binanceHTTP
	binanceHTTP = srv.Client()
	t.Cleanup(func() { binanceHTTP = old })
	bars, err := fetchKlines(srv.URL, "XUSDT", "15m", 2)
	if err != nil || len(bars) != 1 || bars[0].OpenTime != now.Add(-15*time.Minute).UnixMilli() {
		t.Fatalf("forming candle leaked: %v %v", bars, err)
	}
}

func TestFix05CorrelatedHourlySamplesAreNotIndependent(t *testing.T) {
	var samples []shortSample
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 300; i++ {
		samples = append(samples, shortSample{TS: start.Add(time.Duration(i) * time.Hour).UnixMilli(), Components: map[string]float64{"structure": float64(i)}, Outcome: float64(i)})
	}
	if _, ok := updateShortWeights(samples, DefaultShortWeights(), shortTunerEta); ok {
		t.Fatal("300 overlapping hourly labels bypassed 30 independent-block minimum")
	}
}

func TestFix05BoundedProjectionPreservesBothBoundsAndSum(t *testing.T) {
	weights := DefaultShortWeights()
	weights["structure"] = 1000
	weights["rejection"] = 0.000001
	got := boundedShortWeights(weights)
	sum := 0.0
	for _, v := range got {
		if v < 0.03-1e-10 || v > 0.30+1e-10 {
			t.Fatalf("weight outside bounds: %v", got)
		}
		sum += v
	}
	if math.Abs(sum-1) > 1e-10 {
		t.Fatalf("weight sum=%v", sum)
	}
}

func TestFix05ResearchProposalCannotPublishLiveParameters(t *testing.T) {
	before := GetParams()
	var signals []BTSignal
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		score, ret := 85.0, 0.1
		if i%3 == 0 {
			score, ret = 60, -1
		} else if i%3 == 1 {
			score, ret = 75, 3
		}
		signals = append(signals, BTSignal{Time: start.Add(time.Duration(i) * 48 * time.Hour), Score: score, Ret24h: ret})
	}
	changes, _, _, _, _, candidate := proposeWalkForward(signals)
	if len(changes) == 0 || candidate == nil {
		t.Fatal("fixture failed to produce research candidate")
	}
	if GetParams().StrongThreshold != before.StrongThreshold || GetParams().MediumThreshold != before.MediumThreshold {
		t.Fatal("forward-return research changed live parameters")
	}
}

func TestFix05EnabledWeightResearchStillCannotPublish(t *testing.T) {
	dir := t.TempDir()
	paramsMu.Lock()
	oldPath, oldLoaded, oldParams := paramsPath, paramsLoaded, currentParams
	paramsMu.Unlock()
	oldJournal, oldProposal := shortTuningPath, shortWeightProposalPath
	t.Cleanup(func() {
		paramsMu.Lock()
		paramsPath, paramsLoaded, currentParams = oldPath, oldLoaded, oldParams
		paramsMu.Unlock()
		shortTuningPath = oldJournal
		shortWeightProposalPath = oldProposal
	})
	SetParamsPath(filepath.Join(dir, "params.json"))
	shortTuningPath = filepath.Join(dir, "signals.jsonl")
	shortWeightProposalPath = filepath.Join(dir, "proposal.json")
	on := true
	p := GetParams()
	p.ShortTunerEnabled = &on
	if err := ApplyParamsChecked(p); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paramsPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(48 * time.Hour)
	var samples []shortSample
	for i := 0; i < 40; i++ {
		ts := now.Add(time.Duration(i-41) * 48 * time.Hour).UnixMilli()
		samples = append(samples, shortSample{TS: ts, LabelAt: ts + 24*time.Hour.Milliseconds(), LabelVersion: 2, Symbol: "XUSDT", Price: 100, Evaluated: true, Outcome: float64(i), Components: map[string]float64{"structure": float64(i)}})
	}
	if err := writeSamples(samples); err != nil {
		t.Fatal(err)
	}
	RunShortTuner(now)
	after, err := os.ReadFile(paramsPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("research modified live parameters or consumption cursor")
	}
	data, err := os.ReadFile(shortWeightProposalPath)
	if err != nil {
		t.Fatal(err)
	}
	var proposal ShortWeightProposal
	if json.Unmarshal(data, &proposal) != nil || proposal.Applied || len(proposal.Candidate) != 9 {
		t.Fatalf("invalid research proposal: %s", data)
	}
}
