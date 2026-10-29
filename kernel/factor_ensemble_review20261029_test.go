package kernel

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"nofx/logger"
	"nofx/market"
)

func TestFactorEnsembleReview20261029FutureAsOf(t *testing.T) {
	var warnings bytes.Buffer
	previousOutput := logger.Log.Out
	logger.Log.SetOutput(&warnings)
	t.Cleanup(func() { logger.Log.SetOutput(previousOutput) })
	for _, tc := range []struct {
		name, asOf, latestClosed string
		now                      time.Time
	}{
		{"future_year", "2099-01-01", "2026-10-19", factorEnsembleTestNow},
		{"current_utc_day", "2026-10-20", "2026-10-19", factorEnsembleTestNow},
		{"utc_before_local_day", "2026-10-19", "2026-10-18", time.Date(2026, 10, 20, 1, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings.Reset()
			path := factorEnsembleTestFile(t, factorEnsembleFixture(tc.asOf))
			for i := 0; i < 5; i++ {
				if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, tc.now); got != nil {
					t.Fatalf("future snapshot returned evidence: %+v", got)
				}
			}
			if log := warnings.String(); strings.Count(log, "[factor-ensemble]") != 1 || !strings.Contains(log, "WARN") || !strings.Contains(log, path) || !strings.Contains(log, "latest closed UTC day "+tc.latestClosed) {
				t.Fatalf("expected one WARN for this snapshot using the caller's UTC day: %s", log)
			}
			// A snapshot rejected in a replay can become usable as the caller's
			// clock advances; the rejection must not poison the file cache.
			asOf, err := time.Parse(time.DateOnly, tc.asOf)
			if err != nil {
				t.Fatal(err)
			}
			if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, asOf.AddDate(0, 0, 1)); got == nil || got.AgeDays != 0 {
				t.Fatalf("latest closed day must be accepted on a cache hit: %+v", got)
			}
			// Replaying again, even much later in wall-clock/caller time, still
			// emits no second warning for the same cached file.
			computeFactorEnsemble(&market.Data{Symbol: "ETHUSDT"}, tc.now.AddDate(0, 0, -10))
			if strings.Count(warnings.String(), "[factor-ensemble]") != 1 {
				t.Fatalf("replay repeated a file's warning: %s", warnings.String())
			}
			if err := os.WriteFile(path, []byte(factorEnsembleFixture("2099-02-01")), 0644); err != nil {
				t.Fatal(err)
			}
			mtime := tc.now.Add(time.Second)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, tc.now); got != nil || strings.Count(warnings.String(), "[factor-ensemble]") != 2 {
				t.Fatalf("replacement snapshot must be independently rejected and warned: ref=%+v logs=%s", got, warnings.String())
			}
		})
	}
}

func TestFactorEnsembleReview20261029HorizonAges(t *testing.T) {
	for _, tc := range []struct {
		age  int
		want []string
	}{
		{0, []string{"1", "2", "3", "4", "7"}},
		{1, []string{"1", "2", "3", "4", "7"}},
		{2, []string{"2", "3", "4", "7"}},
		{3, []string{"4", "7"}},
		{4, nil},
	} {
		t.Run(fmt.Sprintf("age_%d", tc.age), func(t *testing.T) {
			var entries []string
			for _, h := range []string{"1", "2", "3", "4", "7", "0", "-1", "bad"} {
				entries = append(entries, fmt.Sprintf(`%q:{"factor_count":1,"scores":{"BTCUSDT":{"z":0.8,"pct":0.9}}}`, h))
			}
			asOf := factorEnsembleTestNow.AddDate(0, 0, -1-tc.age).Format(time.DateOnly)
			factorEnsembleTestFile(t, fmt.Sprintf(`{"version":1,"market":"crypto","as_of":%q,"horizons":{%s}}`, asOf, strings.Join(entries, ",")))
			got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("all expired horizons must omit reference: %+v", got)
				}
				return
			}
			if got == nil || got.AgeDays != tc.age {
				t.Fatalf("expected age %d: %+v", tc.age, got)
			}
			var horizons []string
			for h := range got.Horizons {
				horizons = append(horizons, h)
			}
			sort.Strings(horizons)
			if !reflect.DeepEqual(horizons, tc.want) {
				t.Fatalf("age %d: horizons=%v, want %v", tc.age, horizons, tc.want)
			}
		})
	}
}

func TestFactorEnsembleReview20261029SnapshotIDInPrompt(t *testing.T) {
	for _, tc := range []struct{ name, runID, want string }{
		{"run_id", `"crypto-20261020T010000Z-abc123"`, "crypto-20261020T010000Z-abc123"},
		{"absent", "", ""},
		{"empty", `""`, ""},
		{"null", `null`, ""},
		{"malformed_optional_metadata", `123`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := factorEnsembleFixture("2026-10-19")
			if tc.runID != "" {
				raw = strings.Replace(raw, `"version":1`, `"version":1,"run_id":`+tc.runID, 1)
			}
			factorEnsembleTestFile(t, raw)
			want := tc.want
			if want == "" {
				want = fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))[:12]
			}
			for i := 0; i < 2; i++ {
				got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
				if got == nil || got.SnapshotID != want || got.AgeDays != 0 {
					t.Fatalf("snapshot identity/age on read %d = %+v, want %s/0", i, got, want)
				}
				prompt := (&StrategyEngine{}).renderSignalBlock(&SymbolSignal{Symbol: "BTCUSDT", DataComplete: true, FactorEnsemble: got})
				if !strings.Contains(prompt, `"snapshot_id":"`+want+`"`) || !strings.Contains(prompt, `"age_days":0`) {
					t.Fatalf("actual prompt signal must retain snapshot identity and zero age: %s", prompt)
				}
			}
		})
	}
}

func TestFactorEnsembleReview20261029ExpiredCoreDoesNotHideFreshExtended(t *testing.T) {
	factorEnsembleTestFile(t, factorEnsembleExtendedFixture("2026-10-16"))
	got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
	if got == nil || got.Scope != "extended" || got.AgeDays != 3 || len(got.Horizons) != 1 || got.Horizons["7"].Pct != 1 {
		t.Fatalf("expired core horizons must not suppress a fresh extended horizon: %+v", got)
	}
}
