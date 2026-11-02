// llmreplay re-asks historical prompts without importing the trader loop or
// opening a writable store. Exchange clients and migrations are never started.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"nofx/crypto"
	"nofx/mcp"
	_ "nofx/mcp/provider"
	"nofx/store"

	"github.com/joho/godotenv"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type options struct {
	DB, Trader, Since, Until, VariantFile string
	Sample                                int
	Seed                                  int64
	runOptions
}

func main() {
	if err := cli(os.Args[1:], os.Stdout, nil); err != nil {
		fmt.Fprintln(os.Stderr, "离线重放:", err)
		os.Exit(1)
	}
}
func cli(args []string, w io.Writer, injected clientFactory) error {
	fs := flag.NewFlagSet("llmreplay", flag.ContinueOnError)
	fs.SetOutput(w)
	var o options
	fs.StringVar(&o.DB, "db", "data/data.db", "SQLite 数据库路径（只读）")
	fs.StringVar(&o.Trader, "trader", "", "交易员 id 或名称")
	fs.StringVar(&o.Since, "since", "", "起始 UTC 日期/时间（含）")
	fs.StringVar(&o.Until, "until", "", "截止 UTC 日期/时间（不含）")
	fs.IntVar(&o.Sample, "sample", 600, "分层抽样数")
	fs.Int64Var(&o.Seed, "seed", 7, "固定随机种子")
	fs.StringVar(&o.VariantFile, "variant", "", "变体 JSON 文件")
	fs.StringVar(&o.Arm, "arm", "baseline", "baseline|variant|noise")
	fs.IntVar(&o.NoiseSubset, "noise-subset", 200, "噪声组使用前 M 个样本")
	fs.IntVar(&o.Concurrency, "concurrency", 3, "并发数")
	fs.StringVar(&o.Model, "model", "", "ai_models id，默认沿用交易员配置")
	fs.StringVar(&o.Out, "out", "data/evolution/replay/", "共享样本与缓存目录")
	fs.BoolVar(&o.DryRun, "dry-run", false, "仅统计样本、补丁命中和预估 tokens；不调用 LLM")
	fs.IntVar(&o.Repeat, "repeat", 0, "重复实验编号（独立缓存键）")
	fs.DurationVar(&o.Timeout, "timeout", 3*time.Minute, "每次调用超时")
	fs.IntVar(&o.Retries, "retries", 3, "429/5xx 最大重试次数")
	fs.DurationVar(&o.Backoff, "backoff", time.Second, "指数退避初始间隔")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || o.Trader == "" || o.Out == "" || o.Sample <= 0 || o.Concurrency <= 0 || o.NoiseSubset <= 0 || o.Repeat < 0 || o.Retries < 0 || o.Retries > 10 || o.Timeout <= 0 || o.Backoff <= 0 {
		return fmt.Errorf("需要 --trader，且样本数、并发数、超时等参数必须合法")
	}
	if o.Arm != "baseline" && o.Arm != "variant" && o.Arm != "noise" {
		return fmt.Errorf("--arm 必须为 baseline、variant 或 noise")
	}
	since, err := utcTime(o.Since)
	if err != nil {
		return err
	}
	until, err := utcTime(o.Until)
	if err != nil {
		return err
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return fmt.Errorf("--until 必须晚于 --since")
	}
	if o.Arm == "variant" && o.VariantFile == "" {
		return fmt.Errorf("变体组需要 --variant")
	}
	if o.VariantFile != "" {
		b, err := os.ReadFile(o.VariantFile)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&o.Variant); err != nil {
			return fmt.Errorf("变体文件无效: %w", err)
		}
		if o.Variant.ID == "" {
			return fmt.Errorf("变体 id 不能为空")
		}
	}
	db, err := openReadOnly(o.DB)
	if err != nil {
		return err
	}
	defer db.Close()
	// Do not use store.New: it migrates tables and can change journal mode.
	gd, err := gorm.Open(sqlite.New(sqlite.Config{DriverName: "sqlite", Conn: db}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return err
	}
	var traders []store.Trader
	if err = gd.Select("id, name, user_id, ai_model_id").Where("id = ? OR name = ?", o.Trader, o.Trader).Find(&traders).Error; err != nil {
		return err
	}
	if len(traders) != 1 {
		return fmt.Errorf("交易员匹配 %d 条，请使用唯一 id", len(traders))
	}
	tr := traders[0]
	model, err := resolveModel(gd, tr, o.Model)
	if err != nil {
		return err
	}
	// Include the effective endpoint/model configuration, never credentials.
	// A model renamed in-place must not silently reuse an older replay.
	modelConfig, _ := json.Marshal([]string{model.Provider, model.CustomAPIURL, model.CustomModelName})
	o.Model = model.ID + "@" + digest(modelConfig)
	fmt.Fprintf(w, "数据库只读；交易员 %s (%s)；模型 %s / %s / %s\n", tr.Name, tr.ID, model.ID, model.Provider, model.CustomModelName)
	var records []record
	query := `SELECT id, timestamp, system_prompt, input_prompt, COALESCE(decision_json,'') FROM decision_records WHERE trader_id=? AND success=1 AND trim(COALESCE(system_prompt,''))!='' AND trim(COALESCE(input_prompt,''))!=''`
	queryArgs := []any{tr.ID}
	if !since.IsZero() {
		query += " AND julianday(timestamp) >= julianday(?)"
		queryArgs = append(queryArgs, canonicalTime(since))
	}
	if !until.IsZero() {
		query += " AND julianday(timestamp) < julianday(?)"
		queryArgs = append(queryArgs, canonicalTime(until))
	}
	rows, err := db.Query(query+" ORDER BY id", queryArgs...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r record
		var ts any
		if err = rows.Scan(&r.ID, &ts, &r.System, &r.Input, &r.Original); err != nil {
			rows.Close()
			return err
		}
		r.Timestamp, err = databaseTime(ts)
		if err != nil {
			rows.Close()
			return fmt.Errorf("记录 %d 时间无效: %w", r.ID, err)
		}
		if !since.IsZero() && r.Timestamp.Before(since) || !until.IsZero() && !r.Timestamp.Before(until) {
			continue
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	sampled, pop, err := stratified(records, o.Sample, o.Seed)
	if err != nil {
		return err
	}
	dbPath, err := filepath.Abs(o.DB)
	if err != nil {
		return err
	}
	manifest := sampleManifest{1, dbPath, tr.ID, canonicalTime(since), canonicalTime(until), o.Seed, o.Sample, pop, sampled}
	if err = os.MkdirAll(o.Out, 0755); err != nil {
		return err
	}
	manifest, sampled, err = saveOrResumeSample(filepath.Join(o.Out, "sample.json"), manifest, records)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "样本总体：开仓层 %d，其余层 %d；抽样 %d，seed=%d\n", manifest.Population["open"], manifest.Population["other"], len(sampled), o.Seed)
	for _, name := range []string{"open", "other"} {
		n, weight := 0, 0.0
		for _, r := range sampled {
			if r.Stratum == name {
				n++
				weight = r.Weight
			}
		}
		fmt.Fprintf(w, "分层 %s：抽取 %d，单点权重 %.4f\n", name, n, weight)
	}
	if o.Arm == "variant" {
		path := filepath.Join(o.Out, "variant-"+o.Variant.hash()+".json")
		b, _ := json.MarshalIndent(o.Variant, "", "  ")
		if err = writeExclusive(path, append(b, '\n')); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	factory := injected
	if factory == nil {
		factory = func() (llm, error) { return configuredClient(gd, model.ID, o.Timeout) }
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return runReplay(ctx, sampled, o.runOptions, factory, w)
}
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("只读打开数据库失败: %w", err)
	}
	return db, nil
}
func resolveModel(db *gorm.DB, tr store.Trader, override string) (store.AIModel, error) {
	// Same resolution as manager/trader_manager.go: owner list, exact id first,
	// then legacy provider matching. Explicit --model accepts only an id.
	var models []store.AIModel
	err := db.Select("id, user_id, provider, enabled, custom_api_url, custom_model_name").Where("user_id = ?", tr.UserID).Order("id").Find(&models).Error
	if err != nil {
		return store.AIModel{}, err
	}
	target := tr.AIModelID
	if override != "" {
		target = override
	}
	for _, m := range models {
		if m.ID == target {
			return m, nil
		}
	}
	if override == "" {
		for _, m := range models {
			if m.Provider == target {
				return m, nil
			}
		}
	}
	return store.AIModel{}, fmt.Errorf("找不到交易员所属用户的 AI 模型 %s", target)
}

type mcpLLM struct {
	client     mcp.AIClient
	contextual interface{ SetRequestContext(context.Context) }
}

func (c *mcpLLM) Call(ctx context.Context, system, input string) (string, error) {
	c.contextual.SetRequestContext(ctx)
	return c.client.CallWithMessages(system, input)
}
func configuredClient(db *gorm.DB, id string, timeout time.Duration) (llm, error) {
	// Reuse storage decryption and provider registry used by the live trader.
	_ = godotenv.Load()
	cs, err := crypto.NewCryptoService()
	if err != nil {
		return nil, fmt.Errorf("初始化凭据解密失败: %w", err)
	}
	crypto.SetGlobalCryptoService(cs)
	model, err := store.NewAIModelStore(db).GetByID(id)
	if err != nil {
		return nil, err
	}
	if !model.Enabled || model.APIKey == "" {
		return nil, fmt.Errorf("模型未启用或缺少 API key")
	}
	opts := []mcp.ClientOption{mcp.WithTimeout(timeout), mcp.WithMaxRetries(0), mcp.WithStreamDecisions(false)}
	client := mcp.NewAIClientByProvider(model.Provider, opts...)
	if client == nil {
		client = mcp.NewClient(opts...)
	}
	client.SetAPIKey(string(model.APIKey), model.CustomAPIURL, model.CustomModelName)
	client.SetTimeout(timeout)
	contextual, ok := client.(interface{ SetRequestContext(context.Context) })
	if !ok {
		return nil, fmt.Errorf("模型客户端不支持调用超时上下文")
	}
	return &mcpLLM{client, contextual}, nil
}
func utcTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("无效 UTC 日期/时间 %q", s)
}
func databaseTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), nil
	case string:
		return utcTime(t)
	case []byte:
		return utcTime(string(t))
	default:
		return time.Time{}, fmt.Errorf("未知时间类型 %T", v)
	}
}
func canonicalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func writeExclusive(path string, b []byte) error {
	// Publish a complete manifest atomically without replacing an existing
	// experiment. A crash cannot leave half of sample.json visible.
	f, err := os.CreateTemp(filepath.Dir(path), ".llmreplay-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Link(f.Name(), path)
}

func saveOrResumeSample(path string, m sampleManifest, records []record) (sampleManifest, []record, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		body, _ := json.MarshalIndent(m, "", "  ")
		err = writeExclusive(path, append(body, '\n'))
		if errors.Is(err, os.ErrExist) {
			return saveOrResumeSample(path, m, records)
		}
		return m, m.Records, err
	}
	if err != nil {
		return m, nil, err
	}
	var old sampleManifest
	if err = json.Unmarshal(b, &old); err != nil {
		return m, nil, err
	}
	if old.Version != m.Version || old.DB != m.DB || old.TraderID != m.TraderID || old.Since != m.Since || old.Until != m.Until || old.Seed != m.Seed || old.Requested != m.Requested {
		return m, nil, fmt.Errorf("sample.json 参数不匹配，请为另一个实验使用新 --out")
	}
	// Resume from the frozen manifest even if new history has since arrived.
	byID := map[int64]record{}
	for _, r := range records {
		byID[r.ID] = r
	}
	var hydrated []record
	seen := map[int64]bool{}
	for _, s := range old.Records {
		r, ok := byID[s.ID]
		if !ok || seen[s.ID] || s.Weight <= 0 || !r.Timestamp.Equal(s.Timestamp) || digest([]byte(r.System+"\x00"+r.Input)) != s.PromptHash {
			return m, nil, fmt.Errorf("样本记录 %d 缺失、重复或提示已变更", s.ID)
		}
		seen[s.ID] = true
		r.Weight = s.Weight
		r.Stratum = s.Stratum
		r.PromptHash = s.PromptHash
		hydrated = append(hydrated, r)
	}
	if len(hydrated) == 0 {
		return m, nil, fmt.Errorf("样本清单为空")
	}
	return old, hydrated, nil
}
