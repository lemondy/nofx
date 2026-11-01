package store

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// StrategyTypeFactorQuant selects the programmatic factor strategy: no LLM, no
// AI decision cycle. A nightly job writes target weights to quant_targets.json
// and the trader rebalances a PAPER account towards them every `horizon` days
// (design: docs/architecture/FACTOR_QUANT_TRADER_DESIGN_2026-11-01.md).
const StrategyTypeFactorQuant = "factor_quant"

// Factor-quant markets.
const (
	FactorQuantMarketCrypto = "crypto"
	FactorQuantMarketUS     = "us"
)

// FactorQuantConfig is the factor_quant strategy configuration. Zero values
// mean "use the documented default" (see the Effective* resolvers), except
// ShortStopPct where an explicit 0 disables the stop.
type FactorQuantConfig struct {
	// Market: "crypto" (paper futures account, long/short) or "us" (bStock paper
	// ledger, long-only). Required.
	Market string `json:"market"`
	// Horizon in days; must equal the targets file's horizon. crypto default 3;
	// us default 0 = accept the horizon given by the file.
	Horizon int `json:"horizon,omitempty"`
	// CapitalFraction of equity the weights are applied to, (0,1]; default 1.
	CapitalFraction float64 `json:"capital_fraction,omitempty"`
	// Leverage: crypto only, paper futures leverage that keeps margin
	// sufficient; 1..10, default 3.
	Leverage int `json:"leverage,omitempty"`
	// RebalanceBandPct: a symbol whose target notional differs from the held
	// notional by less than this fraction of the target is left alone; [0,1),
	// default 0.10.
	RebalanceBandPct float64 `json:"rebalance_band_pct,omitempty"`
	// MinOrderUSDT: smallest order notional; default 10.
	MinOrderUSDT float64 `json:"min_order_usdt,omitempty"`
	// ShortStopPct: crypto only, catastrophic stop on shorts at
	// entry*(1+ShortStopPct). nil = default 0.5; explicit 0 disables.
	ShortStopPct *float64 `json:"short_stop_pct,omitempty"`
	// MaxStaleDays: targets whose as_of is older than this many days (UTC)
	// are not traded; default horizon+2.
	MaxStaleDays int `json:"max_stale_days,omitempty"`
	// USFeeBps / USSlippageBps: US only, trading costs of the bStock paper
	// ledger, in basis points per fill. Buys fill at quote*(1+slippage), sells
	// at quote*(1-slippage); the fee is a fraction of the notional deducted from
	// paper cash. nil = defaults (fee 10 = Binance spot 0.1%, slippage 3);
	// explicit 0 disables. Valid 0..100.
	USFeeBps      *float64 `json:"us_fee_bps,omitempty"`
	USSlippageBps *float64 `json:"us_slippage_bps,omitempty"`
	// TargetsPath overrides $FACTOR_MINING_DIR/{market}/quant_targets.json.
	TargetsPath string `json:"targets_path,omitempty"`
}

const (
	defaultFactorQuantCryptoHorizon = 3
	defaultFactorQuantLeverage      = 3
	defaultFactorQuantBand          = 0.10
	defaultFactorQuantMinOrder      = 10.0
	defaultFactorQuantShortStop     = 0.5
	defaultFactorQuantUSFeeBps      = 10.0
	defaultFactorQuantUSSlippageBps = 3.0
	maxFactorQuantUSCostBps         = 100.0
)

func (f *FactorQuantConfig) IsCrypto() bool { return f != nil && f.Market == FactorQuantMarketCrypto }
func (f *FactorQuantConfig) IsUS() bool     { return f != nil && f.Market == FactorQuantMarketUS }

// EffectiveHorizon: configured horizon, crypto 3 when unset, us 0 (= follow the file).
func (f *FactorQuantConfig) EffectiveHorizon() int {
	if f == nil {
		return 0
	}
	if f.Horizon > 0 {
		return f.Horizon
	}
	if f.IsCrypto() {
		return defaultFactorQuantCryptoHorizon
	}
	return 0
}

// EffectiveCapitalFraction: (0,1], default 1.
func (f *FactorQuantConfig) EffectiveCapitalFraction() float64 {
	if f == nil || f.CapitalFraction <= 0 {
		return 1
	}
	return f.CapitalFraction
}

// EffectiveLeverage: 1..10, default 3.
func (f *FactorQuantConfig) EffectiveLeverage() int {
	if f == nil || f.Leverage <= 0 {
		return defaultFactorQuantLeverage
	}
	return f.Leverage
}

// EffectiveBand: rebalance band as a fraction of the target notional.
func (f *FactorQuantConfig) EffectiveBand() float64 {
	if f == nil || f.RebalanceBandPct <= 0 {
		return defaultFactorQuantBand
	}
	return f.RebalanceBandPct
}

// EffectiveMinOrderUSDT: minimum order notional.
func (f *FactorQuantConfig) EffectiveMinOrderUSDT() float64 {
	if f == nil || f.MinOrderUSDT <= 0 {
		return defaultFactorQuantMinOrder
	}
	return f.MinOrderUSDT
}

// EffectiveShortStopPct: 0 = no stop.
func (f *FactorQuantConfig) EffectiveShortStopPct() float64 {
	if f == nil || f.ShortStopPct == nil {
		return defaultFactorQuantShortStop
	}
	return *f.ShortStopPct
}

// EffectiveUSFeeBps: per-fill fee of the US paper ledger, basis points.
func (f *FactorQuantConfig) EffectiveUSFeeBps() float64 {
	if f == nil || f.USFeeBps == nil {
		return defaultFactorQuantUSFeeBps
	}
	return *f.USFeeBps
}

// EffectiveUSSlippageBps: per-fill adverse slippage of the US paper ledger, basis points.
func (f *FactorQuantConfig) EffectiveUSSlippageBps() float64 {
	if f == nil || f.USSlippageBps == nil {
		return defaultFactorQuantUSSlippageBps
	}
	return *f.USSlippageBps
}

// EffectiveMaxStaleDays: configured value, else horizon+2. fileHorizon is the
// horizon given by the targets file, used when the config follows the file.
func (f *FactorQuantConfig) EffectiveMaxStaleDays(fileHorizon int) int {
	if f != nil && f.MaxStaleDays > 0 {
		return f.MaxStaleDays
	}
	h := f.EffectiveHorizon()
	if h <= 0 {
		h = fileHorizon
	}
	return h + 2
}

// Validate rejects unusable input; zero retains the documented defaults.
func (f *FactorQuantConfig) Validate() error {
	if f == nil {
		return fmt.Errorf("factor_quant configuration is required")
	}
	if f.Market != FactorQuantMarketCrypto && f.Market != FactorQuantMarketUS {
		return fmt.Errorf("factor_quant market must be %q or %q", FactorQuantMarketCrypto, FactorQuantMarketUS)
	}
	if f.Horizon < 0 {
		return fmt.Errorf("factor_quant horizon must be >= 0")
	}
	if fqBadFloat(f.CapitalFraction) || f.CapitalFraction < 0 || f.CapitalFraction > 1 {
		return fmt.Errorf("factor_quant capital_fraction must be in (0,1]")
	}
	if f.Leverage < 0 || f.Leverage > 10 {
		return fmt.Errorf("factor_quant leverage must be 1..10")
	}
	if fqBadFloat(f.RebalanceBandPct) || f.RebalanceBandPct < 0 || f.RebalanceBandPct >= 1 {
		return fmt.Errorf("factor_quant rebalance_band_pct must be in [0,1)")
	}
	if fqBadFloat(f.MinOrderUSDT) || f.MinOrderUSDT < 0 {
		return fmt.Errorf("factor_quant min_order_usdt must be >= 0")
	}
	if f.ShortStopPct != nil && (fqBadFloat(*f.ShortStopPct) || *f.ShortStopPct < 0) {
		return fmt.Errorf("factor_quant short_stop_pct must be >= 0")
	}
	for name, v := range map[string]*float64{"us_fee_bps": f.USFeeBps, "us_slippage_bps": f.USSlippageBps} {
		if v != nil && (fqBadFloat(*v) || *v < 0 || *v > maxFactorQuantUSCostBps) {
			return fmt.Errorf("factor_quant %s must be in [0,%g]", name, maxFactorQuantUSCostBps)
		}
	}
	if f.MaxStaleDays < 0 {
		return fmt.Errorf("factor_quant max_stale_days must be >= 0")
	}
	if strings.ContainsRune(f.TargetsPath, 0) {
		return fmt.Errorf("factor_quant targets_path is invalid")
	}
	return nil
}

func fqBadFloat(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) }

// ============================================================================
// Durable rebalance state: the as_of of the last rebalance attempt, so a
// restart or config reload can never rebalance the same targets twice.
// ============================================================================

// FactorQuantState is one row per trader.
type FactorQuantState struct {
	TraderID    string `gorm:"column:trader_id;primaryKey" json:"trader_id"`
	LastAsOf    string `gorm:"column:last_as_of;not null;default:''" json:"last_as_of"` // YYYY-MM-DD of the targets last rebalanced to
	LastRunID   string `gorm:"column:last_run_id;default:''" json:"last_run_id"`
	LastRule    string `gorm:"column:last_rule;default:''" json:"last_rule"`
	RebalanceAt int64  `gorm:"column:rebalance_at;default:0" json:"rebalance_at"`    // Unix ms UTC
	LastSkipKey string `gorm:"column:last_skip_key;default:''" json:"last_skip_key"` // last skip reason already recorded (dedup across restarts)
	UpdatedAt   int64  `gorm:"column:updated_at;default:0" json:"updated_at"`        // Unix ms UTC
}

func (FactorQuantState) TableName() string { return "factor_quant_state" }

// FactorQuantStore persists FactorQuantState.
type FactorQuantStore struct {
	db *gorm.DB
}

func NewFactorQuantStore(db *gorm.DB) *FactorQuantStore { return &FactorQuantStore{db: db} }

func (s *FactorQuantStore) initTables() error { return s.db.AutoMigrate(&FactorQuantState{}) }

// Get returns the trader's state, or (nil, nil) when it never rebalanced.
func (s *FactorQuantStore) Get(traderID string) (*FactorQuantState, error) {
	var row FactorQuantState
	err := s.db.Where("trader_id = ?", traderID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// SetRebalanced records the as_of of a completed rebalance attempt.
func (s *FactorQuantStore) SetRebalanced(traderID, asOf, runID, rule string, at time.Time) error {
	now := time.Now().UTC().UnixMilli()
	row := &FactorQuantState{TraderID: traderID, LastAsOf: asOf, LastRunID: runID, LastRule: rule, RebalanceAt: at.UTC().UnixMilli(), UpdatedAt: now}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "trader_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_as_of", "last_run_id", "last_rule", "rebalance_at", "updated_at"}),
	}).Create(row).Error
}

// SetSkipKey records the last skip reason already written to a decision record.
func (s *FactorQuantStore) SetSkipKey(traderID, key string) error {
	now := time.Now().UTC().UnixMilli()
	row := &FactorQuantState{TraderID: traderID, LastSkipKey: key, UpdatedAt: now}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "trader_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_skip_key", "updated_at"}),
	}).Create(row).Error
}
