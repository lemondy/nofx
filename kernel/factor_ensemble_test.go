package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

var factorEnsembleTestNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func factorEnsembleFixture(asOf string) string {
	return fmt.Sprintf(`{"version":1,"market":"crypto","generated_at":"2026-10-20T00:00:00+00:00","as_of":%q,"universe_size":2,"horizons":{
		"1":{"factor_count":4,"forward":{"inconclusive":4},"scores":{"BTCUSDT":{"z":0.83,"pct":0.91}}},
		"3":{"factor_count":2,"forward":{"negative":1,"inconclusive":1},"scores":{"BTCUSDT":{"z":-0.2,"pct":0.25}}},
		"7":{"factor_count":1,"forward":{"inconclusive":1},"scores":{"ETHUSDT":{"z":0,"pct":0}}}}}`, asOf)
}

// Handwritten schema-contract fixture, independent of the nightly Python writer.
func factorEnsembleExtendedFixture(asOf string) string {
	return fmt.Sprintf(`{"version":1,"market":"crypto","as_of":%q,"universe_size":60,
		"universe":{"core":60,"extended":87},
		"factors":{
			"f_a":{"family":"price_volume","horizon":1,"direction":-1,"expr":"example","val_t":5.06,"val_ic":0.0449,"forward":"inconclusive"},
			"f_b":{"family":"momentum","horizon":1,"direction":1,"val_t":0,"forward":"pending"}},
		"horizons":{
			"1":{"factor_count":8,"forward_failed_excluded":0,"forward":{"inconclusive":8},
				"factor_ids":["f_a","f_b","f_c","f_d","f_e","f_f","f_g","f_h"],
				"scores":{"BTCUSDT":{"z":0.16,"pct":0.60}},
				"ext_scores":{"LDOUSDT":{"z":-0.4,"pct":0.31},"BTCUSDT":{"z":99,"pct":1}},
				"detail":{"BTCUSDT":{"f_h":0,"f_c":2,"f_b":2,"f_a":-2,"f_d":0.5,"f_e":-0.5,"f_f":0.49,"f_g":-0.49},
					"LDOUSDT":{"f_a":0.5,"f_b":-0.5,"f_c":0}}},
			"3":{"factor_count":1,"forward":{"pending":1},"scores":{"BTCUSDT":{"z":-0.2,"pct":0.25}},
				"ext_scores":{"LDOUSDT":{"z":0,"pct":0}}},
			"7":{"factor_count":1,"forward":{"pending":1},"ext_scores":{"BTCUSDT":{"z":99,"pct":1}}}},
		"skipped":{"CXMTUSDT":"history<60d","FOOUSDT":"no_data"}}`, asOf)
}

func factorEnsembleTestFile(t *testing.T, raw string) string {
	t.Helper()
	t.Setenv("FACTOR_MINING_DIR", t.TempDir())
	path := factorEnsembleScoresPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFactorEnsemblePath(t *testing.T) {
	t.Setenv("FACTOR_MINING_DIR", "")
	if got := factorEnsembleScoresPath(); got != filepath.Join("data", "factor_mining", "crypto", "live_scores.json") {
		t.Fatalf("default path = %q", got)
	}
	dir := t.TempDir()
	t.Setenv("FACTOR_MINING_DIR", dir)
	if got := factorEnsembleScoresPath(); got != filepath.Join(dir, "crypto", "live_scores.json") {
		t.Fatalf("injected path = %q", got)
	}
}

func TestFactorEnsembleInvalidFiles(t *testing.T) {
	valid := factorEnsembleFixture("2026-10-19")
	for _, tc := range []struct{ name, raw string }{
		{"missing", valid},
		{"directory", valid},
		{"bad_json", "{"},
		{"trailing_json", valid + "{}"},
		{"null", "null"},
		{"wrong_version", strings.Replace(valid, `"version":1`, `"version":2`, 1)},
		{"wrong_market", strings.Replace(valid, `"market":"crypto"`, `"market":"stocks"`, 1)},
		{"bad_as_of", factorEnsembleFixture("not-a-date")},
		{"stale_as_of", factorEnsembleFixture("2026-10-16")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := factorEnsembleTestFile(t, tc.raw)
			if tc.name == "missing" || tc.name == "directory" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if tc.name == "directory" {
					if err := os.Mkdir(path, 0755); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow); got != nil {
				t.Fatalf("invalid file returned evidence: %+v", got)
			}
		})
	}
}

func TestFactorEnsembleSymbolAndHorizons(t *testing.T) {
	raw := factorEnsembleFixture("2026-10-19")
	factorEnsembleTestFile(t, raw)
	snapshotID := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))[:12]
	want := &FactorEnsembleRef{AsOf: "2026-10-19", AgeDays: 0, SnapshotID: snapshotID, Horizons: map[string]FactorHorizonRef{
		"1": {Pct: 0.91, Z: 0.83, Factors: 4, Forward: "inconclusive×4"},
		"3": {Pct: 0.25, Z: -0.2, Factors: 2, Forward: "inconclusive×1,negative×1"},
	}}
	for i := 0; i < 10; i++ {
		if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow); !reflect.DeepEqual(got, want) {
			t.Fatalf("BTC reference = %+v, want %+v", got, want)
		}
	}
	eth := computeFactorEnsemble(&market.Data{Symbol: "ETHUSDT"}, factorEnsembleTestNow)
	if eth == nil || len(eth.Horizons) != 1 || !reflect.DeepEqual(eth.Horizons["7"], FactorHorizonRef{Factors: 1, Forward: "inconclusive×1"}) {
		t.Fatalf("only the symbol's horizons should appear; zero scores are valid: %+v", eth)
	}
	encoded, err := json.Marshal(computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow))
	if err != nil || string(encoded) != `{"as_of":"2026-10-19","age_days":0,"snapshot_id":"`+snapshotID+`","horizons":{"1":{"pct":0.91,"z":0.83,"factors":4,"forward":"inconclusive×4"},"3":{"pct":0.25,"z":-0.2,"factors":2,"forward":"inconclusive×1,negative×1"}}}` {
		t.Fatalf("reference JSON changed: %s, err=%v", encoded, err)
	}
	for _, data := range []*market.Data{nil, {}, {Symbol: "XRPUSDT"}, {Symbol: "btcusdt"}} {
		if got := computeFactorEnsemble(data, factorEnsembleTestNow); got != nil {
			t.Fatalf("unknown symbol returned evidence: %+v", got)
		}
	}
}

func TestFactorEnsembleExtendedScopesAndDetail(t *testing.T) {
	factorEnsembleTestFile(t, factorEnsembleExtendedFixture("2026-10-19"))
	var previous string
	for i := 0; i < 20; i++ {
		got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
		if got == nil || got.AsOf != "2026-10-19" || got.Scope != "core" || got.ScopeNote != "" || got.Reason != "" || len(got.Horizons) != 2 {
			t.Fatalf("core must win over extended scores in every horizon: %+v", got)
		}
		h := got.Horizons["1"]
		if h.Pct != 0.60 || h.Z != 0.16 || h.Factors != 8 || h.Forward != "inconclusive×8" || h.Bull == nil || *h.Bull != 3 || h.Bear == nil || *h.Bear != 2 || h.Neutral == nil || *h.Neutral != 3 {
			t.Fatalf("core score or direction-adjusted votes = %+v", h)
		}
		if len(h.Top) != 3 || h.Top[0].ID != "f_a" || h.Top[0].Z != -2 || h.Top[1].ID != "f_b" || h.Top[1].Z != 2 || h.Top[2].ID != "f_c" || h.Top[2].Z != 2 {
			t.Fatalf("top ties must sort by id, after absolute z: %+v", h.Top)
		}
		if h.Top[0].Family != "price_volume" || h.Top[0].ValT == nil || *h.Top[0].ValT != 5.06 || h.Top[0].Forward != "inconclusive" || h.Top[1].ValT == nil || *h.Top[1].ValT != 0 || h.Top[1].Forward != "pending" {
			t.Fatalf("top metadata = %+v", h.Top)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && string(encoded) != previous {
			t.Fatal("factor reference must serialize deterministically")
		}
		previous = string(encoded)
		if !strings.Contains(previous, `{"id":"f_c","z":2}`) || strings.Contains(previous, `"expr"`) || strings.Contains(previous, `"val_ic"`) {
			t.Fatalf("missing metadata must be omitted; output must remain compact: %s", previous)
		}
		// A caller must not mutate the cached snapshot's metadata or details.
		*h.Top[0].ValT = 100
		h.Top[0].ID = "changed"
		*h.Bull = 100
	}
	ext := computeFactorEnsemble(&market.Data{Symbol: "LDOUSDT"}, factorEnsembleTestNow)
	if ext == nil || ext.Scope != "extended" || ext.AsOf != "2026-10-19" || ext.Reason != "" || len(ext.Horizons) != 2 {
		t.Fatalf("extended scope = %+v", ext)
	}
	for _, want := range []string{"流动性前60", "未在此档位验证", "相对核心宇宙外推", "权重应低于 core"} {
		if !strings.Contains(ext.ScopeNote, want) {
			t.Fatalf("scope note missing %q: %s", want, ext.ScopeNote)
		}
	}
	h := ext.Horizons["1"]
	if h.Pct != 0.31 || h.Z != -0.4 || h.Factors != 8 || h.Forward != "inconclusive×8" || h.Bull == nil || *h.Bull != 1 || h.Bear == nil || *h.Bear != 1 || h.Neutral == nil || *h.Neutral != 1 || len(h.Top) != 3 {
		t.Fatalf("extended votes at inclusive +/-0.5 boundaries: %+v", h)
	}
	if !reflect.DeepEqual(ext.Horizons["3"], FactorHorizonRef{Factors: 1, Forward: "pending×1"}) {
		t.Fatalf("missing detail must omit votes/top; zero score remains valid: %+v", ext.Horizons["3"])
	}
	encoded, err := json.Marshal(ext.Horizons["3"])
	if err != nil || string(encoded) != `{"pct":0,"z":0,"factors":1,"forward":"pending×1"}` {
		t.Fatalf("missing-detail JSON = %s, err=%v", encoded, err)
	}
}

func TestFactorEnsembleTopAbsoluteOrderAndZeroVotes(t *testing.T) {
	raw := strings.Replace(factorEnsembleExtendedFixture("2026-10-19"), `"f_a":0.5,"f_b":-0.5,"f_c":0`, `"f_a":0.5,"f_b":-1.8,"f_c":1.2,"f_d":0.7`, 1)
	factorEnsembleTestFile(t, raw)
	got := computeFactorEnsemble(&market.Data{Symbol: "LDOUSDT"}, factorEnsembleTestNow)
	if got == nil {
		t.Fatal("missing extended reference")
	}
	h := got.Horizons["1"]
	if len(h.Top) != 3 || h.Top[0].ID != "f_b" || h.Top[1].ID != "f_c" || h.Top[2].ID != "f_d" || h.Neutral == nil || *h.Neutral != 0 {
		t.Fatalf("absolute-z order or zero neutral count = %+v", h)
	}
	encoded, err := json.Marshal(h)
	if err != nil || !strings.Contains(string(encoded), `"neutral":0`) {
		t.Fatalf("valid zero votes must appear: %s, err=%v", encoded, err)
	}
}

func TestFactorEnsembleUncoveredAndNonCrypto(t *testing.T) {
	market.SetEquityClassificationForTesting(map[string]bool{"DELLUSDT": true, "TSLAUSDT": true}, nil)
	t.Cleanup(func() { market.SetEquityClassificationForTesting(nil, nil) })
	raw := factorEnsembleExtendedFixture("2026-10-19")
	factorEnsembleTestFile(t, raw)
	snapshotID := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))[:12]
	for _, tc := range []struct{ symbol, reason string }{
		{"CXMTUSDT", "history<60d"}, {"FOOUSDT", "no_data"}, {"XRPUSDT", "not_in_universe"},
	} {
		got := computeFactorEnsemble(&market.Data{Symbol: tc.symbol}, factorEnsembleTestNow)
		want := &FactorEnsembleRef{AsOf: "2026-10-19", AgeDays: 0, SnapshotID: snapshotID, Scope: "none", Reason: tc.reason}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: reference = %+v, want %+v", tc.symbol, got, want)
		}
		encoded, err := json.Marshal(got)
		if err != nil || strings.Contains(string(encoded), "horizons") || strings.Contains(string(encoded), "scope_note") {
			t.Fatalf("none must omit horizons and scope note: %s, err=%v", encoded, err)
		}
	}
	for _, symbol := range []string{"", "USDT", "xyz:TSLA", "xyz:TSLAUSDT", "XYZ:BTCUSDT", "AAPL", "DELLUSDT", "TSLAUSDT", "GOLDUSDT", "SILVERUSDT", "JPYUSDT", "EURUSDT", "XYZ100USDT", "BTCUSD", "btcusdt"} {
		if got := computeFactorEnsemble(&market.Data{Symbol: symbol}, factorEnsembleTestNow); got != nil {
			t.Fatalf("non-crypto/unnormalized symbol %s returned none: %+v", symbol, got)
		}
	}
	// The unchanged, cached snapshot must expire for all three scopes.
	for _, symbol := range []string{"BTCUSDT", "LDOUSDT", "CXMTUSDT", "XRPUSDT"} {
		if got := computeFactorEnsemble(&market.Data{Symbol: symbol}, factorEnsembleTestNow.AddDate(0, 0, 4)); got != nil {
			t.Fatalf("expired reference for %s: %+v", symbol, got)
		}
	}
}

func TestFactorEnsembleMalformedAdditionsAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name, factors, detail, extScores, factorIDs string
		votes                                       bool
	}{
		{"missing", `null`, `null`, `null`, `null`, false},
		{"wrong_containers", `[]`, `42`, `[]`, `{}`, false},
		{"bad_symbol_detail", `{"f_a":false}`, `{"BTCUSDT":[]}`, `"NaN"`, `1e999`, false},
		{"bad_factor_values", `{"f_a":[]}`, `{"BTCUSDT":{"f_a":"NaN","f_b":1e999,"f_c":null,"f_d":{},"f_e":-1e999}}`, `null`, `null`, false},
		{"partial_detail", `{"f_a":false}`, `{"BTCUSDT":{"f_a":0.5,"bad":"Infinity"}}`, `null`, `null`, true},
		{"partial_metadata", `{"f_a":{"family":"price_volume","val_t":1e999,"forward":"pending","val_ic":1e999,"horizon":{},"direction":"NaN"}}`, `{"BTCUSDT":{"f_a":0.5}}`, `null`, `null`, true},
		{"bad_metadata_fields", `{"f_a":{"family":{},"val_t":"NaN","forward":"unknown"}}`, `{"BTCUSDT":{"f_a":0.5}}`, `null`, `null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"version":1,"market":"crypto","as_of":"2026-10-19","universe":[],"factors":%s,"skipped":[],"horizons":{
				"1":{"factor_count":1,"forward":{"pending":1},"scores":{"BTCUSDT":{"pct":0.6,"z":0.1}},"detail":%s,"ext_scores":%s,"factor_ids":%s},
				"3":{"factor_count":1,"scores":{"BTCUSDT":{"pct":0.25,"z":-0.2}}},
				"7":{"factor_count":"bad"}}}`, tc.factors, tc.detail, tc.extScores, tc.factorIDs)
			factorEnsembleTestFile(t, raw)
			got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
			if got == nil || got.Scope != "core" || len(got.Horizons) != 2 || got.Horizons["1"].Pct != 0.6 || got.Horizons["3"].Pct != 0.25 {
				t.Fatalf("malformed additions discarded valid scores/horizons: %+v", got)
			}
			h := got.Horizons["1"]
			if (h.Bull != nil) != tc.votes || (h.Bear != nil) != tc.votes || (h.Neutral != nil) != tc.votes || (len(h.Top) > 0) != tc.votes {
				t.Fatalf("votes/top must use only finite detail: %+v", h)
			}
			if tc.name == "partial_metadata" && (h.Top[0].Family != "price_volume" || h.Top[0].ValT != nil || h.Top[0].Forward != "pending") {
				t.Fatalf("bad val_t or unrelated metadata hid valid fields: %+v", h.Top)
			}
			if tc.name == "bad_metadata_fields" && (h.Top[0].Family != "" || h.Top[0].ValT != nil || h.Top[0].Forward != "") {
				t.Fatalf("invalid metadata must be omitted: %+v", h.Top)
			}
			if _, err := json.Marshal(got); err != nil {
				t.Fatalf("malformed additions made reference unserializable: %v", err)
			}
		})
	}
}

func TestFactorEnsembleIncrementalFieldsAndSkippedFallback(t *testing.T) {
	market.SetEquityClassificationForTesting(map[string]bool{}, nil)
	t.Cleanup(func() { market.SetEquityClassificationForTesting(nil, nil) })
	for _, tc := range []struct{ name, addition, reason string }{
		{"universe_only", `"universe":{"core":60,"extended":0}`, "not_in_universe"},
		{"factors_only", `"factors":{}`, "not_in_universe"},
		{"skipped_only", `"skipped":{"XRPUSDT":"no_data"}`, "no_data"},
		{"malformed_skipped", `"skipped":[]`, "not_in_universe"},
		{"malformed_reason", `"skipped":{"XRPUSDT":42}`, "not_in_universe"},
		{"null_reason", `"skipped":{"XRPUSDT":null}`, "not_in_universe"},
		{"empty_reason", `"skipped":{"XRPUSDT":""}`, "not_in_universe"},
		{"ext_scores_only", "", "not_in_universe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := factorEnsembleFixture("2026-10-19")
			if tc.addition != "" {
				raw = strings.Replace(raw, `"version":1`, `"version":1,`+tc.addition, 1)
			} else {
				raw = strings.Replace(raw, `"factor_count":4`, `"factor_count":4,"ext_scores":{"LDOUSDT":{"pct":0.3,"z":-0.4}}`, 1)
			}
			factorEnsembleTestFile(t, raw)
			got := computeFactorEnsemble(&market.Data{Symbol: "XRPUSDT"}, factorEnsembleTestNow)
			if got == nil || got.Scope != "none" || got.Reason != tc.reason {
				t.Fatalf("independent addition / skipped fallback = %+v", got)
			}
			core := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
			if core == nil || core.Scope != "core" || core.Horizons["1"].Pct != 0.91 || core.Horizons["3"].Z != -0.2 {
				t.Fatalf("independent addition changed core scores: %+v", core)
			}
			if tc.name == "ext_scores_only" {
				ext := computeFactorEnsemble(&market.Data{Symbol: "LDOUSDT"}, factorEnsembleTestNow)
				if ext == nil || ext.Scope != "extended" || ext.Horizons["1"].Pct != 0.3 {
					t.Fatalf("ext_scores must work without other additions: %+v", ext)
				}
			}
		})
	}
}

func TestFactorEnsembleInvalidExtendedScoresOnlyDropTheirHorizon(t *testing.T) {
	for _, score := range []string{
		`{"pct":1e999,"z":0}`, `{"pct":0.5,"z":-1e999}`,
		`{"pct":"NaN","z":0}`, `{"pct":0.5,"z":"Infinity"}`,
		`{"pct":null,"z":0}`, `{"pct":0.5}`, `{"z":0}`, `null`,
		`{"pct":-0.1,"z":0}`, `{"pct":1.1,"z":0}`,
	} {
		t.Run(score, func(t *testing.T) {
			raw := strings.Replace(factorEnsembleExtendedFixture("2026-10-19"), `{"z":-0.4,"pct":0.31}`, score, 1)
			factorEnsembleTestFile(t, raw)
			got := computeFactorEnsemble(&market.Data{Symbol: "LDOUSDT"}, factorEnsembleTestNow)
			if got == nil || got.Scope != "extended" || len(got.Horizons) != 1 || got.Horizons["3"].Pct != 0 {
				t.Fatalf("invalid extended score discarded a valid horizon: %+v", got)
			}
			core := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
			if core == nil || core.Horizons["1"].Pct != 0.6 {
				t.Fatalf("invalid extended score discarded core score: %+v", core)
			}
		})
	}
	raw := strings.Replace(factorEnsembleExtendedFixture("2026-10-19"), `{"z":-0.4,"pct":0.31}`, `{"pct":"NaN","z":0}`, 1)
	raw = strings.Replace(raw, `"LDOUSDT":{"z":0,"pct":0}`, `"LDOUSDT":{"pct":2,"z":0}`, 1)
	factorEnsembleTestFile(t, raw)
	if got := computeFactorEnsemble(&market.Data{Symbol: "LDOUSDT"}, factorEnsembleTestNow); got != nil {
		t.Fatalf("all invalid extended horizons must omit reference, not report none: %+v", got)
	}
}

func TestFactorEnsembleUTCDateExpiryOnCacheHit(t *testing.T) {
	factorEnsembleTestFile(t, factorEnsembleFixture("2026-10-17"))
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2026, 10, 20, 23, 59, 59, 0, time.UTC), true},
		{time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), false},
		{time.Date(2026, 10, 21, 7, 59, 59, 0, time.FixedZone("UTC+8", 8*3600)), true},
		{time.Date(2026, 10, 21, 8, 0, 0, 0, time.FixedZone("UTC+8", 8*3600)), false},
	} {
		if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, tc.at); (got != nil) != tc.want {
			t.Fatalf("at %s: reference = %+v, want valid=%v", tc.at, got, tc.want)
		}
	}
}

func TestFactorEnsembleInvalidScoresOnlyDropTheirHorizon(t *testing.T) {
	// JSON has no NaN/Infinity literals. Exponent overflow represents a
	// non-finite float; quoted variants must also never reach the prompt.
	for _, score := range []string{
		`{"pct":1e999,"z":0}`, `{"pct":-1e999,"z":0}`,
		`{"pct":0.5,"z":1e999}`, `{"pct":0.5,"z":-1e999}`,
		`{"pct":"NaN","z":0}`, `{"pct":0.5,"z":"Infinity"}`,
		`{"pct":null,"z":0}`, `{"pct":0.5}`, `{"pct":-0.1,"z":0}`, `{"pct":1.1,"z":0}`,
	} {
		t.Run(score, func(t *testing.T) {
			raw := strings.Replace(factorEnsembleFixture("2026-10-19"), `{"z":0.83,"pct":0.91}`, score, 1)
			factorEnsembleTestFile(t, raw)
			got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
			if got == nil || len(got.Horizons) != 1 || got.Horizons["3"].Pct != 0.25 {
				t.Fatalf("invalid score discarded a valid horizon: %+v", got)
			}
			if _, err := json.Marshal(got); err != nil {
				t.Fatalf("reference must serialize: %v", err)
			}
		})
	}
	raw := strings.ReplaceAll(factorEnsembleFixture("2026-10-19"), `"BTCUSDT"`, `"OTHERUSDT"`)
	raw = strings.Replace(raw, `"ETHUSDT":{"z":0,"pct":0}`, `"BTCUSDT":{"z":1e999,"pct":0}`, 1)
	factorEnsembleTestFile(t, raw)
	if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow); got != nil {
		t.Fatalf("all invalid horizons must omit the reference: %+v", got)
	}
}

func TestFactorEnsembleCacheRefreshAndInvalidation(t *testing.T) {
	valid := factorEnsembleFixture("2026-10-19")
	path := factorEnsembleTestFile(t, valid)
	write := func(raw string, seconds int) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
			t.Fatal(err)
		}
		mtime := factorEnsembleTestNow.Add(time.Duration(seconds) * time.Second)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want float64) {
		t.Helper()
		got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
		if got == nil || got.Horizons["1"].Pct != want {
			t.Fatalf("cached reference = %+v, want pct=%v", got, want)
		}
	}
	write(valid, 0)
	check(0.91)
	changed := strings.Replace(valid, `"pct":0.91`, `"pct":0.75`, 1)
	write(changed, 0)
	check(0.91) // unchanged mtime reuses the snapshot
	write(changed, 1)
	check(0.75)
	write("{", 2)
	if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow); got != nil {
		t.Fatal("invalid replacement served the previous valid snapshot")
	}
	write(valid, 2)
	if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow); got != nil {
		t.Fatal("invalid snapshot was not cached by mtime")
	}
	write(valid, 3)
	check(0.91)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow); got != nil {
		t.Fatal("missing replacement served the previous valid snapshot")
	}
	write(changed, 3)
	check(0.75)
	// Equal mtimes on different injected paths must not share a snapshot.
	path = factorEnsembleTestFile(t, valid)
	write(valid, 3)
	check(0.91)
}

func TestFactorEnsembleFileSizeLimit(t *testing.T) {
	valid := factorEnsembleFixture("2026-10-19")
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("extra=%d", extra), func(t *testing.T) {
			factorEnsembleTestFile(t, valid+strings.Repeat(" ", factorEnsembleMaxFileSize-len(valid)+extra))
			got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
			if (got != nil) != (extra == 0) {
				t.Fatalf("8 MiB + %d bytes: reference = %+v", extra, got)
			}
		})
	}
}

func TestFactorEnsembleConcurrentReaders(t *testing.T) {
	factorEnsembleTestFile(t, factorEnsembleFixture("2026-10-19"))
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				got := computeFactorEnsemble(&market.Data{Symbol: "BTCUSDT"}, factorEnsembleTestNow)
				if got == nil || got.Horizons["1"].Pct != 0.91 {
					t.Errorf("concurrent reference = %+v", got)
					return
				}
				delete(got.Horizons, "1") // caller-owned maps cannot mutate the cache
			}
		}()
	}
	wg.Wait()
}

func TestFactorEnsembleDisabledCompatibilityAndGateIsolation(t *testing.T) {
	factorEnsembleTestFile(t, factorEnsembleFixture("2026-10-19"))
	data := sweepCompatibilityData(factorEnsembleTestNow)
	data.Symbol = "BTCUSDT"
	opt := SignalOptions{Now: factorEnsembleTestNow, PrimaryTF: "15m"}
	disabled, err := ComputeSymbolSignals(data.Symbol, data, opt)
	if err != nil {
		t.Fatal(err)
	}
	before := RenderSignalJSON(disabled)
	if disabled.FactorEnsemble != nil || strings.Contains(before, "factor_ensemble") {
		t.Fatal("disabled signal contains factor_ensemble")
	}
	opt.EnableFactorEnsemble = true
	enabled, err := ComputeSymbolSignals(data.Symbol, data, opt)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.FactorEnsemble == nil || !strings.Contains(RenderSignalJSON(enabled), `"factor_ensemble":{`) || !strings.Contains(RenderSignalJSON(enabled), `"as_of":"2026-10-19"`) {
		t.Fatal("enabled signal missing factor evidence or snake_case as_of")
	}
	enabled.FactorEnsemble = nil
	if !reflect.DeepEqual(enabled, disabled) || RenderSignalJSON(enabled) != before {
		t.Fatal("factor reference changed another signal field, score, gate, or entry/exit verdict")
	}
	// The file key comes from data.Symbol, even when the display symbol differs.
	alias, err := ComputeSymbolSignals("DISPLAYUSDT", data, opt)
	if err != nil || alias.FactorEnsemble == nil {
		t.Fatalf("lookup must use data.Symbol: signal=%+v, err=%v", alias, err)
	}
	data.Symbol = "XRPUSDT"
	unknown, err := ComputeSymbolSignals(data.Symbol, data, opt)
	if err != nil || unknown.FactorEnsemble != nil || strings.Contains(RenderSignalJSON(unknown), "factor_ensemble") {
		t.Fatalf("unknown symbol must omit factor_ensemble: signal=%+v, err=%v", unknown, err)
	}
}

func TestFactorEnsembleExtendedSignalIsolation(t *testing.T) {
	market.SetEquityClassificationForTesting(map[string]bool{"DELLUSDT": true}, nil)
	t.Cleanup(func() { market.SetEquityClassificationForTesting(nil, nil) })
	factorEnsembleTestFile(t, factorEnsembleExtendedFixture("2026-10-19"))
	for _, tc := range []struct{ symbol, scope string }{
		{"BTCUSDT", "core"}, {"LDOUSDT", "extended"}, {"CXMTUSDT", "none"}, {"DELLUSDT", ""},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			data := sweepCompatibilityData(factorEnsembleTestNow)
			data.Symbol = tc.symbol
			opt := SignalOptions{Now: factorEnsembleTestNow, PrimaryTF: "15m"}
			disabled, err := ComputeSymbolSignals(data.Symbol, data, opt)
			if err != nil {
				t.Fatal(err)
			}
			before := RenderSignalJSON(disabled)
			if disabled.FactorEnsemble != nil || strings.Contains(before, "factor_ensemble") {
				t.Fatal("disabled signal contains factor_ensemble for new-format file")
			}
			opt.EnableFactorEnsemble = true
			enabled, err := ComputeSymbolSignals(data.Symbol, data, opt)
			if err != nil {
				t.Fatal(err)
			}
			if tc.scope == "" {
				if enabled.FactorEnsemble != nil || strings.Contains(RenderSignalJSON(enabled), "factor_ensemble") {
					t.Fatal("non-crypto signal contains uncovered factor reference")
				}
			} else if enabled.FactorEnsemble == nil || enabled.FactorEnsemble.Scope != tc.scope || !strings.Contains(RenderSignalJSON(enabled), `"scope":"`+tc.scope+`"`) {
				t.Fatalf("enabled reference has wrong scope: %+v", enabled.FactorEnsemble)
			}
			enabled.FactorEnsemble = nil
			if !reflect.DeepEqual(enabled, disabled) || RenderSignalJSON(enabled) != before {
				t.Fatal("factor evidence changed another signal field, score, gate, or entry/exit verdict")
			}
		})
	}
}

func TestFactorEnsembleConfigDefaultsAndRoundTrip(t *testing.T) {
	for _, lang := range []string{"zh", "en"} {
		if store.GetDefaultStrategyConfig(lang).Indicators.EnableFactorEnsemble {
			t.Fatalf("%s default enabled factor ensemble", lang)
		}
	}
	for _, raw := range []string{`{}`, `{"indicators":{"enable_ema":true}}`, `{"indicators":{"enable_factor_ensemble":false}}`, `{"indicators":{"enable_factor_ensemble":true}}`} {
		var cfg store.StrategyConfig
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatal(err)
		}
		want := strings.Contains(raw, `"enable_factor_ensemble":true`)
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Indicators.EnableFactorEnsemble != want || strings.Contains(string(encoded), "enable_factor_ensemble") != want {
			t.Fatalf("incorrect default or serialization: %s", encoded)
		}
		var restored store.StrategyConfig
		if err := json.Unmarshal(encoded, &restored); err != nil || restored.Indicators.EnableFactorEnsemble != want {
			t.Fatalf("opt-in lost in round trip: %+v, %v", restored.Indicators, err)
		}
	}
}

func TestFactorEnsemblePromptOptIn(t *testing.T) {
	for _, limit := range []bool{false, true} {
		for _, sweep := range []bool{false, true} {
			cfg := &store.StrategyConfig{Language: "zh"}
			cfg.RiskControl.LimitEntryEnabled = limit
			cfg.Indicators.EnableLiquiditySweep = sweep
			disabled := NewStrategyEngine(cfg).BuildSystemPrompt(100, "")
			if strings.Contains(disabled, "factor_ensemble") {
				t.Fatal("disabled prompt contains factor_ensemble")
			}
			cfg.Indicators.EnableFactorEnsemble = true
			enabled := NewStrategyEngine(cfg).BuildSystemPrompt(100, "")
			var addition string
			for _, line := range strings.SplitAfter(enabled, "\n") {
				if strings.HasPrefix(line, "- factor_ensemble ") {
					addition += line
				}
			}
			for _, want := range []string{"程序每日预计算的多因子截面参考", "pct 越大越被因子看多、越小越看空", "z 为方向调整后的截面标准分", "按 horizon 天数", "scope=core", "scope=extended", "流动性档位验证", "相对核心宇宙外推", "参考权重应低于 core", "bull/bear/neutral", "方向投票", "top", "family、val_t、前向状态", "scope=none", "没经过因子校验", "reason 说明原因", "不能据此推断看多/看空", "inconclusive/pending", "尚未被样本外证明", "仅是参考证据", "不是独立入场理由", "不能绕过 hard_entry_gate / rr_scan"} {
				if !strings.Contains(addition, want) {
					t.Fatalf("legend missing %q", want)
				}
			}
			if strings.Count(addition, "\n") != 1 || strings.Replace(enabled, addition, "", 1) != disabled {
				t.Fatal("opt-in must add exactly one line and preserve existing prompt bytes")
			}
		}
	}
}

func TestFactorEnsembleEngineWiring(t *testing.T) {
	now := time.Now()
	factorEnsembleTestFile(t, factorEnsembleFixture(now.UTC().AddDate(0, 0, -1).Format(time.DateOnly)))
	old := binanceGetFn
	t.Cleanup(func() { binanceGetFn = old })
	binanceGetFn = func(context.Context, string, interface{}) error { return fmt.Errorf("offline factor test") }
	cfg := store.GetDefaultStrategyConfig("zh")
	cfg.Indicators.Klines.PrimaryTimeframe = "15m"
	data := sweepCompatibilityData(now)
	data.Symbol = "BTCUSDT"
	for _, enabled := range []bool{false, true} {
		cfg.Indicators.EnableFactorEnsemble = enabled
		sig := NewStrategyEngine(&cfg).computeCoinSignal(data, nil, &Context{}, nil)
		if sig == nil || (sig.FactorEnsemble != nil) != enabled {
			t.Fatalf("engine opt-in=%v: signal=%+v", enabled, sig)
		}
	}
}

type factorEnsembleStaleCall struct {
	asOf    string
	age     int
	expired bool
}

// captureFactorEnsembleStale swaps the alert hook for a recorder and resets the
// rate-limit state so tests are independent.
func captureFactorEnsembleStale(t *testing.T) *[]factorEnsembleStaleCall {
	t.Helper()
	var calls []factorEnsembleStaleCall
	orig := factorEnsembleStaleNotify
	factorEnsembleStaleNotify = func(asOf string, age int, expired bool) {
		calls = append(calls, factorEnsembleStaleCall{asOf, age, expired})
	}
	factorEnsembleStaleState.mu.Lock()
	factorEnsembleStaleState.lastLog, factorEnsembleStaleState.lastNotify = [2]time.Time{}, [2]time.Time{}
	factorEnsembleStaleState.mu.Unlock()
	t.Cleanup(func() { factorEnsembleStaleNotify = orig })
	return &calls
}

func TestFactorEnsembleStaleAlerts(t *testing.T) {
	btc := &market.Data{Symbol: "BTCUSDT"}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) // latest closed UTC day = 10-19

	t.Run("fresh_no_alert", func(t *testing.T) {
		calls := captureFactorEnsembleStale(t)
		factorEnsembleTestFile(t, factorEnsembleFixture("2026-10-18")) // age 1: normal before the nightly run
		if computeFactorEnsemble(btc, now) == nil {
			t.Fatal("fresh snapshot must return evidence")
		}
		if len(*calls) != 0 {
			t.Fatalf("unexpected alerts: %+v", *calls)
		}
	})

	t.Run("expiring_age2_once_per_12h", func(t *testing.T) {
		calls := captureFactorEnsembleStale(t)
		factorEnsembleTestFile(t, factorEnsembleFixture("2026-10-17")) // age 2: one run missed
		for i := 0; i < 5; i++ {                                       // many symbols/cycles within the window
			if computeFactorEnsemble(btc, now.Add(time.Duration(i)*time.Minute)) == nil {
				t.Fatal("expiring snapshot must still return evidence")
			}
		}
		if want := []factorEnsembleStaleCall{{"2026-10-17", 2, false}}; !reflect.DeepEqual(*calls, want) {
			t.Fatalf("calls = %+v, want %+v", *calls, want)
		}
		computeFactorEnsemble(btc, now.Add(11*time.Hour+59*time.Minute))
		if len(*calls) != 1 {
			t.Fatalf("repeated inside 12h: %+v", *calls)
		}
		computeFactorEnsemble(btc, now.Add(12*time.Hour)) // still age 2/3 -> re-alert
		if len(*calls) != 2 {
			t.Fatalf("expected a second alert after 12h: %+v", *calls)
		}
	})

	t.Run("expired_age_over_3", func(t *testing.T) {
		calls := captureFactorEnsembleStale(t)
		factorEnsembleTestFile(t, factorEnsembleFixture("2026-10-15")) // age 4: every horizon dropped
		for i := 0; i < 3; i++ {
			if got := computeFactorEnsemble(btc, now); got != nil {
				t.Fatalf("expired snapshot returned evidence: %+v", got)
			}
		}
		if want := []factorEnsembleStaleCall{{"2026-10-15", 4, true}}; !reflect.DeepEqual(*calls, want) {
			t.Fatalf("calls = %+v, want %+v", *calls, want)
		}
	})

	t.Run("age3_7d_horizon_still_served", func(t *testing.T) {
		calls := captureFactorEnsembleStale(t)
		factorEnsembleTestFile(t, factorEnsembleFixture("2026-10-16"))          // age 3: 7d horizon still served
		if computeFactorEnsemble(&market.Data{Symbol: "ETHUSDT"}, now) == nil { // only ETH has 7d scores
			t.Fatal("age 3 must still return evidence")
		}
		if want := []factorEnsembleStaleCall{{"2026-10-16", 3, false}}; !reflect.DeepEqual(*calls, want) {
			t.Fatalf("calls = %+v, want %+v", *calls, want)
		}
	})

	t.Run("expiring_and_expired_budgets_are_separate", func(t *testing.T) {
		calls := captureFactorEnsembleStale(t)
		factorEnsembleTestFile(t, factorEnsembleFixture("2026-10-17"))
		computeFactorEnsemble(btc, now)                  // age 2 -> expiring
		computeFactorEnsemble(btc, now.AddDate(0, 0, 2)) // age 4 -> expired, same cache
		if len(*calls) != 2 || (*calls)[0].expired || !(*calls)[1].expired {
			t.Fatalf("calls = %+v", *calls)
		}
	})

	t.Run("missing_or_bad_file_does_not_alert", func(t *testing.T) {
		for _, raw := range []string{"{", "null", factorEnsembleFixture("not-a-date"), "missing"} {
			calls := captureFactorEnsembleStale(t)
			path := factorEnsembleTestFile(t, raw)
			if raw == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if got := computeFactorEnsemble(btc, now); got != nil {
				t.Fatalf("%q returned evidence: %+v", raw, got)
			}
			if len(*calls) != 0 {
				t.Fatalf("%q triggered stale alert: %+v", raw, *calls)
			}
		}
	})
}
