package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeFields(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "fields.jsonl")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fieldJSON(t *testing.T, id int64, symbol, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"record_id": id, "symbol": symbol, "text": text})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func loadedFields(t *testing.T, content string) variant {
	t.Helper()
	dir := t.TempDir()
	writeFields(t, dir, content)
	v := variant{ID: "fields-v1", InputInject: &inputInjection{File: "fields.jsonl", Label: "Factor reference", RequireAll: true}}
	if err := loadInputInject(&v, filepath.Join(dir, "variant.json"), nil); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestInputInjectAnchorsAndComposition(t *testing.T) {
	v := loadedFields(t, fieldJSON(t, 1, "ETHUSDT", "rank=2")+fieldJSON(t, 2, "OTHER", "excluded")+fieldJSON(t, 1, "BTCUSDT", "因子=1"))
	block := "## Factor reference\n- BTCUSDT: 因子=1\n- ETHUSDT: rank=2\n\n"
	rules := "## Review Rules\n- wait"
	for _, tc := range []struct {
		name, input, rules, want string
	}{
		{"candidate", "Current Positions: None\n\n## Candidate Coins (2 coins)\nBTC ETH", "", "Current Positions: None\n\n" + block + "## Candidate Coins (2 coins)\nBTC ETH"},
		{"candidate at start", "## Candidate Coins (2 coins)\nBTC ETH", "", block + "## Candidate Coins (2 coins)\nBTC ETH"},
		{"positions fallback", "Account\n\n## Current Positions\nBTC", "", "Account\n\n" + block + "## Current Positions\nBTC"},
		{"empty positions fallback", "Current Positions: None", "", block + "Current Positions: None"},
		{"CRLF", "Account\r\nCurrent Positions: None\r\n\r\n## Candidate Coins (2 coins)\r\nBTC", "", "Account\r\nCurrent Positions: None\r\n\r\n" + block + "## Candidate Coins (2 coins)\r\nBTC"},
		{"rules and candidate", "Current Positions: None\n\n## Candidate Coins (2 coins)\nBTC ETH", rules + "\n", rules + "\n\nCurrent Positions: None\n\n" + block + "## Candidate Coins (2 coins)\nBTC ETH"},
		{"rules and fallback", "## Current Positions\nBTC", rules, rules + "\n\n" + block + "## Current Positions\nBTC"},
		{"original anchors", "Current Positions: None", "## Candidate Coins (quoted)", "## Candidate Coins (quoted)\n\n" + block + "Current Positions: None"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patched := v
			patched.Rules = tc.rules
			patched.Replace = []replacement{{"old", "new"}}
			patched.Append = "append"
			system, input, err := applyPatch(record{ID: 1, System: "old", Input: tc.input}, patched)
			if err != nil || input != tc.want || system != "new\n\nappend" {
				t.Fatalf("system=%q input=%q want=%q err=%v", system, input, tc.want, err)
			}
		})
	}
}

func TestInputInjectPatchMissAndOptionalRows(t *testing.T) {
	v := loadedFields(t, fieldJSON(t, 1, "BTCUSDT", "rank=1"))
	for _, input := range []string{"no header", "quoted ## Candidate Coins (1 coin)", "## Candidate Coins\nBTC", "prefix ## Current Positions"} {
		if _, _, err := applyPatch(record{ID: 1, Input: input}, v); err == nil || !strings.HasPrefix(err.Error(), "patch_miss: input_inject") {
			t.Fatalf("input=%q err=%v", input, err)
		}
	}
	r := record{ID: 99, System: "system", Input: "no header"}
	if _, _, err := applyPatch(r, v); err == nil || err.Error() != "patch_miss: input_inject 无该记录字段" {
		t.Fatalf("missing required fields: %v", err)
	}
	v.InputInject.RequireAll = false
	system, input, err := applyPatch(r, v)
	if err != nil || system != r.System || input != r.Input {
		t.Fatalf("optional fields must be a no-op: %q %q %v", system, input, err)
	}
}

func TestInputInjectLoadValidation(t *testing.T) {
	for _, tc := range []struct {
		name, content, wantError string
	}{
		{"duplicate pair", fieldJSON(t, 1, "BTCUSDT", "one") + fieldJSON(t, 1, "BTCUSDT", "two"), "记录 1 (BTCUSDT) 重复"},
		{"overlong unicode", fieldJSON(t, 1, "BTCUSDT", strings.Repeat("因", 301)), "超过 300"},
		{"overlong ASCII", fieldJSON(t, 1, "BTCUSDT", strings.Repeat("x", 70000)), "超过 300"},
		{"malformed JSON", "{", "第 1 行无效"},
		{"missing record", `{"symbol":"BTCUSDT","text":"x"}`, "需要 record_id"},
		{"missing text", `{"record_id":1,"symbol":"BTCUSDT"}`, "需要 record_id"},
		{"empty symbol", `{"record_id":1,"symbol":"","text":"x"}`, "需要 record_id"},
		{"non-integer record", `{"record_id":1.5,"symbol":"BTCUSDT","text":"x"}`, "第 1 行无效"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFields(t, dir, tc.content)
			v := variant{InputInject: &inputInjection{File: "fields.jsonl", Label: "Factor"}}
			if err := loadInputInject(&v, filepath.Join(dir, "variant.json"), nil); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("err=%v want=%q", err, tc.wantError)
			}
		})
	}
	// The length limit counts runes, and different record/symbol pairs are valid.
	v := loadedFields(t, "\n"+fieldJSON(t, 1, "BTCUSDT", strings.Repeat("因", 300))+fieldJSON(t, 2, "BTCUSDT", "")+fieldJSON(t, 1, "ETHUSDT", "x"))
	if len(v.InputInject.rows) != 2 || len(v.InputInject.rows[1]) != 2 {
		t.Fatal("valid fields were lost")
	}
}

func TestInputInjectAsOf(t *testing.T) {
	db, err := openReadOnly(makeFixtureDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		name, asOf, wantError string
	}{
		{"equal with offset", "2026-10-01T09:00:00+08:00", ""},
		{"earlier", "2026-10-01T00:59:59Z", ""},
		{"look-ahead", "2026-10-01T01:00:00.000000001Z", "记录 1 的 as_of"},
		{"invalid timestamp", "2026-10-01", "记录 1 的 as_of 不是有效 RFC3339"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFields(t, dir, `{"record_id":1,"symbol":"BTCUSDT","text":"x","as_of":"`+tc.asOf+`"}`)
			v := variant{InputInject: &inputInjection{File: "fields.jsonl", Label: "Factor"}}
			err := loadInputInject(&v, filepath.Join(dir, "variant.json"), db)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("err=%v want=%q", err, tc.wantError)
			}
		})
	}
	dir := t.TempDir()
	writeFields(t, dir, `{"record_id":999,"symbol":"BTCUSDT","text":"x","as_of":"2026-10-01T00:00:00Z"}`)
	v := variant{InputInject: &inputInjection{File: "fields.jsonl", Label: "Factor"}}
	if err := loadInputInject(&v, filepath.Join(dir, "variant.json"), db); err == nil || !strings.Contains(err.Error(), "记录 999 无法读取决策时间") {
		t.Fatalf("cannot validate an unknown record: %v", err)
	}
}

func TestInputInjectFileHashAndReplayCache(t *testing.T) {
	dir := t.TempDir()
	o := replayOptions(t.TempDir())
	o.Arm = "variant"
	var calls atomic.Int32
	factory := func() (llm, error) { return &fakeLLM{calls: &calls, response: "[]"}, nil }
	r := record{ID: 1, Input: "## Candidate Coins (1 coin)\nBTC"}
	previous := ""
	for _, content := range []string{fieldJSON(t, 1, "BTCUSDT", "one"), fieldJSON(t, 1, "BTCUSDT", "two"), fieldJSON(t, 1, "BTCUSDT", "two") + "\n"} {
		writeFields(t, dir, content)
		o.Variant = variant{ID: "fields-v1", InputInject: &inputInjection{File: "fields.jsonl", Label: "Factor", SHA256: "untrusted"}}
		if err := loadInputInject(&o.Variant, filepath.Join(dir, "variant.json"), nil); err != nil {
			t.Fatal(err)
		}
		if o.Variant.InputInject.SHA256 != digest([]byte(content)) || o.Variant.hash() == previous {
			t.Fatal("file bytes did not change the variant hash")
		}
		previous = o.Variant.hash()
		for i := 0; i < 2; i++ {
			if err := runReplay(context.Background(), []record{r}, o, factory, io.Discard); err != nil {
				t.Fatal(err)
			}
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("cache did not isolate file changes or resume identical content: calls=%d", calls.Load())
	}
}

func TestInputInjectDryRunAndPatchMissRows(t *testing.T) {
	o := replayOptions(t.TempDir())
	o.Arm, o.DryRun = "variant", true
	o.Variant = loadedFields(t, fieldJSON(t, 1, "BTCUSDT", "one")+fieldJSON(t, 2, "BTCUSDT", "two"))
	recs := []record{{ID: 1, Input: "## Candidate Coins (1 coin)"}, {ID: 2, Input: "no header"}, {ID: 3, Input: "no fields"}}
	var out bytes.Buffer
	if err := runReplay(context.Background(), recs, o, func() (llm, error) { t.Fatal("dry run created client"); return nil, nil }, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "补丁命中 1，patch_miss 2") || !strings.Contains(out.String(), "input_inject 命中 1，缺失 2") {
		t.Fatal(out.String())
	}
	o.Variant.InputInject.RequireAll = false
	out.Reset()
	if err := runReplay(context.Background(), recs, o, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "补丁命中 2，patch_miss 1") || !strings.Contains(out.String(), "input_inject 命中 2，缺失 1") {
		t.Fatal(out.String())
	}
	o.Variant.InputInject.RequireAll = true
	o.DryRun = false
	var calls atomic.Int32
	if err := runReplay(context.Background(), recs, o, func() (llm, error) { return &fakeLLM{calls: &calls, response: "[]"}, nil }, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(o.Out, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || bytes.Count(b, []byte(`"status":"patch_miss"`)) != 2 || !bytes.Contains(b, []byte("input_inject 无该记录字段")) {
		t.Fatalf("calls=%d results=%s", calls.Load(), b)
	}
}

func TestInputInjectCLIRelativeFileAndUnselectedLeakage(t *testing.T) {
	dbPath := makeFixtureDB(t)
	dir := t.TempDir()
	writeFields(t, dir, fieldJSON(t, 1, "BTCUSDT", "factor=1"))
	variantPath := filepath.Join(dir, "variant.json")
	if err := os.WriteFile(variantPath, []byte(`{"id":"fields-v1","input_inject":{"file":"fields.jsonl","label":"Factor reference","require_all":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--db", dbPath, "--trader", "t", "--until", "2026-10-01T02:00:00Z", "--sample", "1", "--arm", "variant", "--variant", variantPath, "--dry-run", "--out", t.TempDir()}
	var out bytes.Buffer
	if err := cli(args, &out, func() (llm, error) { t.Fatal("dry run created client"); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "input_inject 命中 1，缺失 0") || !strings.Contains(out.String(), "LLM 调用 0 次") {
		t.Fatal(out.String())
	}
	// Record 8 is outside the selected time window, but must still be checked.
	writeFields(t, dir, `{"record_id":8,"symbol":"BTCUSDT","text":"future","as_of":"`+canonicalTime(fixtureRecords(8)[7].Timestamp.Add(time.Second))+`"}`)
	if err := cli(args, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "记录 8 的 as_of") {
		t.Fatalf("unselected look-ahead row was not rejected: %v", err)
	}
}
