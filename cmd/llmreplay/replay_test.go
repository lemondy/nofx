package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nofx/mcp"
)

func fixtureRecords(n int) []record {
	var out []record
	for i := 1; i <= n; i++ {
		original := `[]`
		if i <= n/4 {
			original = `[{"action":"open_long"}]`
		}
		out = append(out, record{ID: int64(i), Timestamp: time.Date(2026, 10, 1, i%24, 0, 0, 0, time.UTC), System: "unique old", Input: "Account: Equity 1000\nCurrent Positions: None\n\ncandidates", Original: original})
	}
	return out
}
func TestReplaySamplingDeterministicAndWeighted(t *testing.T) {
	a, pop, err := stratified(fixtureRecords(80), 20, 7)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := stratified(fixtureRecords(80), 20, 7)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("not deterministic")
	}
	counts := map[string]int{}
	totals := map[string]float64{}
	for _, r := range a {
		counts[r.Stratum]++
		totals[r.Stratum] += r.Weight
	}
	if counts["open"] != 10 || counts["other"] != 10 || totals["open"] != 20 || totals["other"] != 60 || pop["open"] != 20 {
		t.Fatalf("%v %v %v", counts, totals, pop)
	}
	c, _, _ := stratified(fixtureRecords(80), 20, 8)
	if reflect.DeepEqual(a, c) {
		t.Fatal("seed ignored")
	}
	a, _, _ = stratified(fixtureRecords(8), 7, 7)
	if len(a) != 7 {
		t.Fatal("did not backfill sparse stratum")
	}
}
func TestReplayPatches(t *testing.T) {
	r := fixtureRecords(1)[0]
	v := variant{Append: "append", Replace: []replacement{{"old", "new"}}, Rules: "## Review Rules\n- wait"}
	sys, input, err := applyPatch(r, v)
	if err != nil || sys != "unique new\n\nappend" || !strings.Contains(input, "- wait\n\nCurrent Positions: None") {
		t.Fatalf("%q %q %v", sys, input, err)
	}
	for _, system := range []string{"none", "old old"} {
		r.System = system
		if _, _, err = applyPatch(r, v); err == nil {
			t.Fatal("must mark patch_miss")
		}
	}
	r.System = "unique old"
	r.Input = "no header"
	if _, _, err = applyPatch(r, v); err == nil {
		t.Fatal("missing header")
	}
	r.Input = "## Current Positions\n1. ETHUSDT"
	if _, _, err = applyPatch(r, v); err != nil {
		t.Fatal(err)
	}
	r.Input = "prefix ## Current Positions quoted"
	if _, _, err = applyPatch(r, v); err == nil {
		t.Fatal("must anchor header")
	}
}

type fakeLLM struct {
	calls    *atomic.Int32
	response string
	errs     []error
}

func (f *fakeLLM) Call(ctx context.Context, system, input string) (string, error) {
	n := int(f.calls.Add(1))
	if n <= len(f.errs) {
		return "", f.errs[n-1]
	}
	return f.response, nil
}
func replayOptions(dir string) runOptions {
	return runOptions{Arm: "baseline", Model: "model", Out: dir, Concurrency: 2, NoiseSubset: 200, Timeout: time.Second, Backoff: time.Nanosecond, Retries: 2}
}
func TestReplayCacheResumeAndDryRun(t *testing.T) {
	dir := t.TempDir()
	o := replayOptions(dir)
	recs := fixtureRecords(4)
	var calls atomic.Int32
	factory := func() (llm, error) { return &fakeLLM{calls: &calls, response: "[]"}, nil }
	o.DryRun = true
	if err := runReplay(context.Background(), recs, o, func() (llm, error) { t.Fatal("dry run constructed client"); return nil, nil }, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "results.jsonl")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote results")
	}
	o.DryRun = false
	if err := runReplay(context.Background(), recs, o, factory, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	if err := runReplay(context.Background(), recs, o, factory, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	if calls.Load() != 4 || !bytes.Equal(before, after) {
		t.Fatal("resume did not skip done keys")
	}
	// Baseline ignores the supplied variant content, allowing cross-variant reuse.
	o.Variant = variant{ID: "another", Append: "x"}
	if err := runReplay(context.Background(), recs, o, factory, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal("baseline was not reusable")
	}
	o.Repeat = 1
	if err := runReplay(context.Background(), recs, o, factory, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 8 {
		t.Fatal("repeat key ignored")
	}
}
func TestReplayPatchMissAndRetry(t *testing.T) {
	dir := t.TempDir()
	o := replayOptions(dir)
	o.Arm = "variant"
	o.Variant = variant{ID: "v", Replace: []replacement{{"old", "new"}}}
	recs := fixtureRecords(2)
	recs[0].System = "missing"
	var calls atomic.Int32
	factory := func() (llm, error) { return &fakeLLM{calls: &calls, response: "[]"}, nil }
	if err := runReplay(context.Background(), recs, o, factory, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	if calls.Load() != 1 || !bytes.Contains(b, []byte(`"status":"patch_miss"`)) {
		t.Fatalf("%s calls=%d", b, calls.Load())
	}
	recs[1].System = "missing"
	if err := runReplay(context.Background(), recs, o, factory, io.Discard); err == nil {
		t.Fatal("all miss must abort")
	}
	calls.Store(0)
	fake := &fakeLLM{calls: &calls, response: `[{"symbol":"BTCUSDT","action":"open_long_limit","price":100,"leverage":3,"position_size_usd":100,"stop_loss":95,"take_profit":120,"confidence":80}]`, errs: []error{&mcp.APIStatusError{StatusCode: 429}, fmt.Errorf("API status 503")}}
	job := replayJob{rec: fixtureRecords(1)[0]}
	row := executeJob(context.Background(), fake, job, o)
	if calls.Load() != 3 || row.Status != "ok" || len(row.Opens) != 1 || row.Opens[0].Side != "long" {
		t.Fatalf("%+v calls=%d", row, calls.Load())
	}
	calls.Store(0)
	fake.errs = []error{&mcp.APIStatusError{StatusCode: 400}}
	row = executeJob(context.Background(), fake, job, o)
	if calls.Load() != 1 || row.Status != "call_error" {
		t.Fatal("retried permanent error")
	}
}
func makeFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE traders (id TEXT, name TEXT, user_id TEXT, ai_model_id TEXT)`,
		`CREATE TABLE ai_models (id TEXT, user_id TEXT, provider TEXT, enabled BOOL, custom_api_url TEXT, custom_model_name TEXT, api_key TEXT)`,
		`CREATE TABLE decision_records (id INTEGER, trader_id TEXT, timestamp DATETIME, system_prompt TEXT, input_prompt TEXT, decision_json TEXT, success BOOL)`,
		`INSERT INTO traders VALUES ('t','测试','u','deepseek')`,
		`INSERT INTO ai_models VALUES ('m','u','deepseek',1,'','deepseek-chat','ENC:v1:never-decrypt-in-dry-run')`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range fixtureRecords(8) {
		_, err = db.Exec(`INSERT INTO decision_records VALUES (?, 't', ?, ?, ?, ?, 1)`, r.ID, canonicalTime(r.Timestamp), r.System, r.Input, r.Original)
		if err != nil {
			t.Fatal(err)
		}
	}
	return path
}
func TestReplayCLIDryRunReadOnlyAndFrozenSample(t *testing.T) {
	path := makeFixtureDB(t)
	dir := t.TempDir()
	var out bytes.Buffer
	args := []string{"--db", path, "--trader", "t", "--since", "2026-10-01", "--sample", "4", "--dry-run", "--out", dir}
	if err := cli(args, &out, func() (llm, error) { t.Fatal("LLM factory called"); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "LLM 调用 0 次") {
		t.Fatal(out.String())
	}
	var manifest sampleManifest
	b, _ := os.ReadFile(filepath.Join(dir, "sample.json"))
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Records) != 4 {
		t.Fatal("missing manifest")
	}
	if err := cli(args, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`DELETE FROM traders`); err == nil {
		t.Fatal("database is writable")
	}
	args = append(args, "--seed", "99")
	if err := cli(args, io.Discard, nil); err == nil {
		t.Fatal("must reject incompatible sample")
	}
}
func TestReplayInterruptedCacheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	if err := os.WriteFile(path, []byte("{\"interrupted\":"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := openCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.append(outputRow{cacheKey: cacheKey{RecordID: 2, Arm: "baseline"}, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	c.f.Close()
	c, err = openCache(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.f.Close()
	if !c.done[cacheKey{RecordID: 2, Arm: "baseline"}] {
		t.Fatal("sealed tail lost completed row")
	}
}

type blockingLLM struct{}

func (*blockingLLM) Call(ctx context.Context, _, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}
func TestReplayPerCallTimeout(t *testing.T) {
	o := replayOptions(t.TempDir())
	o.Timeout = 5 * time.Millisecond
	started := time.Now()
	row := executeJob(context.Background(), &blockingLLM{}, replayJob{rec: fixtureRecords(1)[0]}, o)
	if row.Status != "call_error" || !strings.Contains(row.Error, "deadline exceeded") || time.Since(started) > time.Second {
		t.Fatalf("timeout not enforced: %+v", row)
	}
}
func TestReplayNoiseSubsetAndVariantCacheKeys(t *testing.T) {
	dir := t.TempDir()
	o := replayOptions(dir)
	o.Arm = "noise"
	o.NoiseSubset = 2
	recs, _, _ := stratified(fixtureRecords(8), 8, 7)
	var calls atomic.Int32
	factory := func() (llm, error) { return &fakeLLM{calls: &calls, response: "[]"}, nil }
	if err := runReplay(context.Background(), recs, o, factory, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	seen := map[int64]bool{}
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var row outputRow
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		seen[row.RecordID] = true
	}
	if calls.Load() != 2 || !seen[recs[0].ID] || !seen[recs[1].ID] {
		t.Fatal("noise subset not shared prefix")
	}
	o.Arm = "variant"
	o.Variant = variant{ID: "v", Append: "one"}
	if err := runReplay(context.Background(), recs[:1], o, factory, io.Discard); err != nil {
		t.Fatal(err)
	}
	o.Variant.Append = "two"
	if err := runReplay(context.Background(), recs[:1], o, factory, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal("variant hash did not isolate cache")
	}
}
