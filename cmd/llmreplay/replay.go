package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"nofx/kernel"
	"nofx/market"
	"nofx/mcp"
)

type record struct {
	ID         int64     `json:"record_id"`
	Timestamp  time.Time `json:"timestamp"`
	System     string    `json:"-"`
	Input      string    `json:"-"`
	Original   string    `json:"-"`
	Stratum    string    `json:"stratum"`
	Weight     float64   `json:"weight"`
	PromptHash string    `json:"prompt_hash"`
}
type sampleManifest struct {
	Version    int            `json:"version"`
	DB         string         `json:"db"`
	TraderID   string         `json:"trader_id"`
	Since      string         `json:"since"`
	Until      string         `json:"until"`
	Seed       int64          `json:"seed"`
	Requested  int            `json:"requested"`
	Population map[string]int `json:"population"`
	Records    []record       `json:"records"`
}
type replacement struct {
	Old string `json:"old"`
	New string `json:"new"`
}
type variant struct {
	ID         string        `json:"id"`
	Hypothesis string        `json:"hypothesis"`
	Source     string        `json:"source"`
	Append     string        `json:"system_append"`
	Replace    []replacement `json:"system_replace"`
	Rules      string        `json:"rules_text"`
}

func digest(b []byte) string   { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func (v variant) hash() string { b, _ := json.Marshal(v); return digest(b) }
func isOpen(action string) bool {
	switch action {
	case "open_long", "open_short", "open_long_limit", "open_short_limit":
		return true
	}
	return false
}
func originallyOpen(s string) bool {
	var ds []struct {
		Action string `json:"action"`
	}
	if json.Unmarshal([]byte(s), &ds) != nil {
		return false
	}
	for _, d := range ds {
		if isOpen(d.Action) {
			return true
		}
	}
	return false
}
func stratified(records []record, n int, seed int64) ([]record, map[string]int, error) {
	if n <= 0 || len(records) == 0 {
		return nil, nil, fmt.Errorf("没有可抽样记录或样本数不合法")
	}
	groups := map[string][]record{"open": {}, "other": {}}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	for _, r := range records {
		r.Stratum = "other"
		if originallyOpen(r.Original) {
			r.Stratum = "open"
		}
		r.PromptHash = digest([]byte(r.System + "\x00" + r.Input))
		groups[r.Stratum] = append(groups[r.Stratum], r)
	}
	pop := map[string]int{"open": len(groups["open"]), "other": len(groups["other"])}
	n = min(n, len(records))
	if n == 1 && pop["open"] > 0 && pop["other"] > 0 {
		return nil, nil, fmt.Errorf("两层都有记录时 --sample 至少为 2")
	}
	a, b := min((n+1)/2, pop["open"]), min(n/2, pop["other"])
	a += min(n-a-b, pop["open"]-a)
	b += min(n-a-b, pop["other"]-b)
	rng := rand.New(rand.NewSource(seed))
	out := make([]record, 0, n)
	for i, name := range []string{"open", "other"} {
		count := []int{a, b}[i]
		g := groups[name]
		rng.Shuffle(len(g), func(i, j int) { g[i], g[j] = g[j], g[i] })
		for _, r := range g[:count] {
			r.Weight = float64(len(g)) / float64(count)
			out = append(out, r)
		}
	}
	// Mix strata so the first M noise records are not an all-open subset.
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, pop, nil
}

var positionsHeader = regexp.MustCompile(`(?m)^(## Current Positions|Current Positions: None)[\t ]*\r?$`)

func applyPatch(r record, v variant) (string, string, error) {
	// Validate against the original prompt; a previous replacement cannot
	// manufacture a hit for a later replacement.
	for i, p := range v.Replace {
		if p.Old == "" || strings.Count(r.System, p.Old) != 1 {
			return "", "", fmt.Errorf("patch_miss: system_replace[%d] 命中 %d 次", i, strings.Count(r.System, p.Old))
		}
	}
	system := r.System
	for i, p := range v.Replace {
		if strings.Count(system, p.Old) != 1 {
			return "", "", fmt.Errorf("patch_miss: system_replace[%d] 与前项重叠", i)
		}
		system = strings.Replace(system, p.Old, p.New, 1)
	}
	if v.Append != "" {
		system += "\n\n" + v.Append
	}
	input := r.Input
	if v.Rules != "" {
		hit := positionsHeader.FindStringIndex(input)
		if hit == nil {
			return "", "", fmt.Errorf("patch_miss: 缺少持仓标题")
		}
		input = input[:hit[0]] + strings.TrimRight(v.Rules, "\r\n") + "\n\n" + input[hit[0]:]
	}
	return system, input, nil
}

type cacheKey struct {
	RecordID    int64  `json:"record_id"`
	Arm         string `json:"arm"`
	VariantHash string `json:"variant_hash"`
	Model       string `json:"model"`
	Repeat      int    `json:"repeat"`
}
type openDecision struct {
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	OrderType  string  `json:"order_type"`
	EntryPrice float64 `json:"entry_price"`
	LimitPrice float64 `json:"limit_price,omitempty"`
	StopLoss   float64 `json:"stop_loss"`
	TakeProfit float64 `json:"take_profit"`
	Confidence int     `json:"confidence"`
}
type outputRow struct {
	cacheKey
	Timestamp     time.Time         `json:"timestamp"`
	VariantID     string            `json:"variant_id,omitempty"`
	Status        string            `json:"status"`
	Opens         []openDecision    `json:"opens"`
	Decisions     []kernel.Decision `json:"decisions"`
	RawHash       string            `json:"raw_response_hash"`
	RawResponse   string            `json:"raw_response,omitempty"`
	ElapsedMS     int64             `json:"elapsed_ms"`
	Error         string            `json:"error,omitempty"`
	ParseFailure  bool              `json:"parse_failure"`
	RejectedItems int               `json:"rejected_items"`
}
type replayCache struct {
	f    *os.File
	done map[cacheKey]bool
}

func openCache(path string) (*replayCache, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*replayCache, error) { f.Close(); return nil, e }
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("输出目录已有重放任务: %w", err))
	}
	c := &replayCache{f: f, done: map[cacheKey]bool{}}
	reader := bufio.NewReader(f)
	var damaged []byte
	for {
		line, e := reader.ReadBytes('\n')
		if len(line) > 0 {
			var marker struct {
				Recovery string `json:"cache_tail_recovery"`
			}
			var row outputRow
			valid := json.Unmarshal(line, &row) == nil
			if damaged != nil {
				if json.Unmarshal(line, &marker) != nil || marker.Recovery != digest(damaged) {
					return fail(fmt.Errorf("缓存行损坏，缺少尾行恢复标记"))
				}
				damaged = nil
			} else if valid {
				if row.Status == "ok" || row.Status == "parse_error" || row.Status == "patch_miss" {
					c.done[row.cacheKey] = true
				}
			} else {
				damaged = []byte(strings.TrimSuffix(string(line), "\n"))
			}
			if e == io.EOF {
				// An interrupted tail is sealed with its hash. On every future
				// resume the reader verifies the marker, without deleting bytes.
				suffix := "\n"
				if !valid {
					body, _ := json.Marshal(map[string]string{"cache_tail_recovery": digest(damaged)})
					suffix += string(body) + "\n"
					damaged = nil
				}
				if _, err = f.WriteString(suffix); err != nil {
					return fail(err)
				}
				if err = f.Sync(); err != nil {
					return fail(err)
				}
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return fail(e)
		}
	}
	if damaged != nil {
		return fail(fmt.Errorf("缓存包含未恢复的损坏行"))
	}

	return c, nil
}
func (c *replayCache) append(row outputRow) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err = c.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = c.f.Sync(); err != nil {
		return err
	}
	c.done[row.cacheKey] = true
	return nil
}

type llm interface {
	Call(context.Context, string, string) (string, error)
}
type clientFactory func() (llm, error)
type runOptions struct {
	Arm, Model, Out                           string
	Repeat, Concurrency, NoiseSubset, Retries int
	Timeout, Backoff                          time.Duration
	DryRun                                    bool
	Variant                                   variant
}
type replayJob struct {
	rec           record
	system, input string
	patchErr      error
	key           cacheKey
}

func runReplay(ctx context.Context, records []record, o runOptions, factory clientFactory, w io.Writer) error {
	hash := ""
	if o.Arm == "variant" {
		hash = o.Variant.hash()
	}
	var jobs []replayJob
	hits, misses, tokens := 0, 0, 0
	for i, r := range records {
		if o.Arm == "noise" && i >= o.NoiseSubset {
			break
		}
		job := replayJob{rec: r, system: r.System, input: r.Input, key: cacheKey{r.ID, o.Arm, hash, o.Model, o.Repeat}}
		if o.Arm == "variant" {
			job.system, job.input, job.patchErr = applyPatch(r, o.Variant)
		}
		if job.patchErr != nil {
			misses++
		} else {
			hits++
			tokens += (utf8.RuneCountInString(job.system) + utf8.RuneCountInString(job.input) + 2) / 3
		}
		jobs = append(jobs, job)
	}
	fmt.Fprintf(w, "样本 %d；补丁命中 %d，patch_miss %d；预计输入 tokens ≈ %d（字符/3 粗估，不含回复）\n", len(jobs), hits, misses, tokens)
	if hits == 0 {
		return fmt.Errorf("全部样本 patch_miss 或没有待运行样本，拒绝运行")
	}
	if o.DryRun {
		fmt.Fprintln(w, "只读预演完成：未创建 LLM 客户端，LLM 调用 0 次")
		return nil
	}
	c, err := openCache(filepath.Join(o.Out, "results.jsonl"))
	if err != nil {
		return err
	}
	defer c.f.Close()
	pending := jobs[:0]
	for _, job := range jobs {
		if !c.done[job.key] {
			pending = append(pending, job)
		}
	}
	fmt.Fprintf(w, "缓存已完成 %d；待处理 %d\n", len(jobs)-len(pending), len(pending))
	if len(pending) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan replayJob)
	results := make(chan outputRow, o.Concurrency)
	var wg sync.WaitGroup
	// Construct clients before starting workers so a factory failure cannot
	// leave workers blocked on an unclosed queue.
	clients := make([]llm, min(o.Concurrency, len(pending)))
	needsClient := false
	for _, job := range pending {
		needsClient = needsClient || job.patchErr == nil
	}
	for i := range clients {
		if !needsClient {
			continue
		}
		clients[i], err = factory()
		if err != nil {
			return err
		}
	}
	for _, client := range clients {
		wg.Add(1)
		go func(client llm) {
			defer wg.Done()
			for job := range queue {
				row := executeJob(ctx, client, job, o)
				select {
				case results <- row:
				case <-ctx.Done():
					return
				}
			}
		}(client)
	}

	go func() {
		defer close(queue)
		for _, job := range pending {
			select {
			case queue <- job:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	logger := log.New(w, "重放 ", log.Ltime)
	completed := 0
	for row := range results {
		if err := c.append(row); err != nil {
			cancel()
			return err
		}
		completed++
		logger.Printf("%d/%d record=%d arm=%s 状态=%s 耗时=%dms", completed, len(pending), row.RecordID, row.Arm, row.Status, row.ElapsedMS)
	}
	return ctx.Err()
}
func executeJob(ctx context.Context, client llm, job replayJob, o runOptions) outputRow {
	row := outputRow{cacheKey: job.key, Timestamp: job.rec.Timestamp, Opens: []openDecision{}}
	if o.Arm == "variant" {
		row.VariantID = o.Variant.ID
	}
	if job.patchErr != nil {
		row.Status = "patch_miss"
		row.Error = job.patchErr.Error()
		return row
	}
	started := time.Now()
	var resp string
	var err error
	for attempt := 0; attempt <= o.Retries; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, o.Timeout)
		resp, err = client.Call(callCtx, job.system, job.input)
		cancel()
		if err == nil || !retryable(err) || attempt == o.Retries {
			break
		}
		delay := min(o.Backoff*time.Duration(1<<min(attempt, 10)), 30*time.Second)
		var status *mcp.APIStatusError
		if errors.As(err, &status) {
			delay = max(delay, min(status.RetryAfter, 60*time.Second))
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			err = ctx.Err()
		}
		if ctx.Err() != nil {
			break
		}
	}
	row.ElapsedMS = time.Since(started).Milliseconds()
	row.RawHash = digest([]byte(resp))
	row.RawResponse = resp
	if err != nil {
		row.Status = "call_error"
		row.Error = err.Error()
		return row
	}
	equity := 0.0
	if match := equityPattern.FindStringSubmatch(job.rec.Input); match != nil {
		equity, _ = strconv.ParseFloat(match[1], 64)
	}
	ds, err := kernel.ParseReplayDecisions(resp, equity)
	if err != nil {
		row.Status = "parse_error"
		row.ParseFailure = true
		row.Error = err.Error()
		return row
	}
	row.Status = "ok"
	row.Decisions = ds
	for _, d := range ds {
		for _, reason := range d.NoTradeReasons {
			if strings.HasPrefix(reason, "VALIDATION_REJECTED:") {
				row.ParseFailure = true
				row.RejectedItems++
				break
			}
		}
		if d.Symbol == "ALL" && strings.HasPrefix(d.Reasoning, "Model didn't output structured JSON decision") {
			row.ParseFailure = true
		}
		if !isOpen(d.Action) {
			continue
		}
		side := "short"
		if strings.Contains(d.Action, "long") {
			side = "long"
		}
		kind := "market"
		limit := 0.0
		if strings.HasSuffix(d.Action, "_limit") {
			kind = "limit"
			limit = d.Price
		}
		row.Opens = append(row.Opens, openDecision{market.Normalize(d.Symbol), side, kind, d.Price, limit, d.StopLoss, d.TakeProfit, d.Confidence})
	}
	if row.ParseFailure {
		row.Error = "回复含校验拒绝项或无结构化决策的安全回退"
	}
	return row
}

var equityPattern = regexp.MustCompile(`Account: Equity ([0-9.]+)`)
var statusPattern = regexp.MustCompile(`status[ =:]+([0-9]{3})`)

func retryable(err error) bool {
	var status *mcp.APIStatusError
	code := 0
	if errors.As(err, &status) {
		code = status.StatusCode
	} else if match := statusPattern.FindStringSubmatch(err.Error()); match != nil {
		code, _ = strconv.Atoi(match[1])
	}
	return code == 429 || code >= 500 && code < 600
}
