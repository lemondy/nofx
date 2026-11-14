package trader

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nofx/store"
)

// factor_quant (design 2026-11-01): the quant_targets.json reader. Python
// writes the file atomically (temp file + rename); a half-written or corrupt
// file only skips the tick.

const (
	quantTargetsVersion = 1
	quantTargetsMaxSize = 4 << 20
	// Sanity bounds on weights: the design's gross exposure is 1.0, so anything
	// beyond these is a corrupt file, not a portfolio.
	quantTargetsMaxNameWeight = 1.0
	quantTargetsMaxGross      = 3.0

	quantStatusOK = "ok"
	factorDateFmt = "2006-01-02"
)

// Reader errors (matched with errors.Is).
var (
	errQuantTargetsMissing = errors.New("quant_targets.json 不存在")
	errQuantTargetsInvalid = errors.New("quant_targets.json 内容无效")
)

// quantRule is the portfolio rule description (informational for the trader).
type quantRule struct {
	Name string `json:"name"`
}

// quantTargets is quant_targets.json schema v1.
type quantTargets struct {
	Version      int                `json:"version"`
	Market       string             `json:"market"`
	Status       string             `json:"status"`
	Reason       *string            `json:"reason"`
	GeneratedAt  string             `json:"generated_at"`
	AsOf         string             `json:"as_of"`
	RunID        string             `json:"run_id"`
	Horizon      int                `json:"horizon"`
	Rule         quantRule          `json:"rule"`
	FactorIDs    []string           `json:"factor_ids"`
	UniverseSize int                `json:"universe_size"`
	Weights      map[string]float64 `json:"weights"`
	Excluded     map[string]string  `json:"excluded"`
	Pct          map[string]float64 `json:"pct"`
	K            *float64           `json:"k,omitempty"`

	asOfDate time.Time // parsed AsOf (UTC midnight); zero when status != ok and as_of is unusable
}

// factorMiningRoot resolves the factor-mining data directory exactly like
// kernel/factor_ensemble.go: $FACTOR_MINING_DIR, else data/factor_mining.
func factorMiningRoot() string {
	if dir := os.Getenv("FACTOR_MINING_DIR"); dir != "" {
		return dir
	}
	return filepath.Join("data", "factor_mining")
}

// factorQuantTargetsPath is the configured targets file of a market.
func factorQuantTargetsPath(fq *store.FactorQuantConfig) string {
	if fq != nil && strings.TrimSpace(fq.TargetsPath) != "" {
		return fq.TargetsPath
	}
	market := store.FactorQuantMarketCrypto
	if fq != nil && fq.Market != "" {
		market = fq.Market
	}
	return filepath.Join(factorMiningRoot(), market, "quant_targets.json")
}

// loadQuantTargets reads and validates a targets file for market. A missing
// file wraps errQuantTargetsMissing; everything else wraps errQuantTargetsInvalid.
func loadQuantTargets(path, market string) (*quantTargets, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", errQuantTargetsMissing, path)
		}
		return nil, fmt.Errorf("%w: 打开失败 %v", errQuantTargetsInvalid, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errQuantTargetsInvalid, err)
	}
	if !info.Mode().IsRegular() || info.Size() > quantTargetsMaxSize {
		return nil, fmt.Errorf("%w: 需要不超过 %d 字节的普通文件", errQuantTargetsInvalid, quantTargetsMaxSize)
	}
	raw, err := io.ReadAll(io.LimitReader(f, quantTargetsMaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: 读取失败 %v", errQuantTargetsInvalid, err)
	}
	return parseQuantTargets(raw, market)
}

// parseQuantTargets decodes and validates targets JSON.
func parseQuantTargets(raw []byte, market string) (*quantTargets, error) {
	var t quantTargets
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("%w: JSON 损坏或不完整 (%v)", errQuantTargetsInvalid, err)
	}
	if t.Version != quantTargetsVersion {
		return nil, fmt.Errorf("%w: schema version=%d，仅支持 %d", errQuantTargetsInvalid, t.Version, quantTargetsVersion)
	}
	if market != "" && t.Market != market {
		return nil, fmt.Errorf("%w: market=%q，期望 %q", errQuantTargetsInvalid, t.Market, market)
	}
	if t.Status == "" {
		return nil, fmt.Errorf("%w: 缺少 status", errQuantTargetsInvalid)
	}
	if t.Weights == nil {
		t.Weights = map[string]float64{}
	}
	if t.AsOf != "" {
		d, err := time.Parse(factorDateFmt, t.AsOf)
		if err != nil {
			return nil, fmt.Errorf("%w: as_of=%q 不是 YYYY-MM-DD", errQuantTargetsInvalid, t.AsOf)
		}
		t.asOfDate = d.UTC()
	}
	gross := 0.0
	for sym, w := range t.Weights {
		if strings.TrimSpace(sym) == "" || math.IsNaN(w) || math.IsInf(w, 0) || math.Abs(w) > quantTargetsMaxNameWeight {
			return nil, fmt.Errorf("%w: 非法权重 %q=%v", errQuantTargetsInvalid, sym, w)
		}
		gross += math.Abs(w)
	}
	if gross > quantTargetsMaxGross {
		return nil, fmt.Errorf("%w: 权重毛敞口 %.2f 超过上限 %.1f", errQuantTargetsInvalid, gross, quantTargetsMaxGross)
	}
	return &t, nil
}

// reasonText renders the file's optional reason.
func (t *quantTargets) reasonText() string {
	if t == nil || t.Reason == nil {
		return ""
	}
	return *t.Reason
}
