package store

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// ============================================================================
// Entry assessments — the quality→outcome backtest dataset (user 2026-09-11)
//
// Every AI decision (open or wait) is persisted as one row carrying the
// model's self-assessed entry quality and machine-aggregatable blockers.
// Joined against the trade journal on (trader, symbol, direction,
// entry_time ≥ assessment time), quality buckets map to real win rates and
// expectancy — the test of whether the model's judgment has predictive value.
// ============================================================================

type EntryAssessment struct {
	ID        int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID  string    `gorm:"column:trader_id;index:idx_ea_trader_ts" json:"trader_id"`
	Cycle     int       `gorm:"column:cycle" json:"cycle"`
	Ts        time.Time `gorm:"column:ts;index:idx_ea_trader_ts" json:"ts"`
	Symbol    string    `gorm:"column:symbol;index" json:"symbol"`
	Direction string    `gorm:"column:direction;size:8" json:"direction"` // long | short | none
	Action    string    `gorm:"column:action;size:24" json:"action"`
	Stage     string    `gorm:"column:stage;size:16" json:"stage"`     // decision_stage
	WaitBias  string    `gorm:"column:wait_bias;size:8" json:"wait_bias"`
	WaitState   string `gorm:"column:wait_state;size:16" json:"wait_state"`       // BLOCKED | WATCH_* | READY_* (empty = not declared)
	NextTrigger string `gorm:"column:next_trigger;size:192" json:"next_trigger"` // required-event sentence for directional states
	// Gate RR ceiling (review 09-16 point 2): the rr_scan.best_rr the model
	// was SHOWN this cycle for the row's direction, plus its usable flag.
	// Comparing a later open's realized RR on the same symbol against the
	// WATCH_* rows' gate_rr quantifies the RR decay paid for "wait for the
	// micro-trend turn". 0 / false when no scan was rendered (no noise floor).
	GateRR     float64 `gorm:"column:gate_rr;default:0" json:"gate_rr"`
	GateUsable bool    `gorm:"column:gate_usable" json:"gate_usable"`
	EntryQuality int    `gorm:"column:entry_quality;default:-1" json:"entry_quality"` // -1 = not provided
	BlockingFactors string `gorm:"column:blocking_factors;type:text" json:"blocking_factors"` // JSON array
	MgmtQuality  int    `gorm:"column:mgmt_quality;default:-1" json:"mgmt_quality"`   // -1 = not provided (holds)
	MgmtFlags    string `gorm:"column:mgmt_flags;type:text" json:"mgmt_flags"`        // JSON array
	EntryPath    string `gorm:"column:entry_path;size:24" json:"entry_path"`          // e.g. "15m:down" / "15m:rally" / "bb_ride"
	Price     float64   `gorm:"column:price;default:0" json:"price"`
}

func (EntryAssessment) TableName() string { return "entry_assessments" }

type EntryAssessmentStore struct {
	db *gorm.DB
}

func NewEntryAssessmentStore(db *gorm.DB) *EntryAssessmentStore {
	return &EntryAssessmentStore{db: db}
}

func (s *EntryAssessmentStore) initTables() error {
	return s.db.AutoMigrate(&EntryAssessment{})
}

func (s *EntryAssessmentStore) Insert(rec *EntryAssessment) error {
	return s.db.Create(rec).Error
}

// QualityBucketStat is one entry-quality bucket's outcome summary.
type QualityBucketStat struct {
	Bucket      string  `json:"bucket"`
	Assessments int     `json:"assessments"`
	Traded      int     `json:"traded"`
	Wins        int     `json:"wins"`
	WinRate     float64 `json:"win_rate"`     // % of traded
	AvgPnLPct   float64 `json:"avg_pnl_pct"`  // mean journal PnL% (margin-based) of traded
	TotalPnL    float64 `json:"total_pnl"`    // USDT
}

// BucketStats joins assessments with the trade journal: an assessment with an
// open action "became" the earliest journal trade of the same trader+symbol+
// direction whose entry_time is at/after the assessment timestamp (each
// journal row consumed once). Waits count per bucket without outcomes — the
// shadow-outcome engine is future work.
func (s *EntryAssessmentStore) BucketStats(traderID string) ([]QualityBucketStat, error) {
	var rows []EntryAssessment
	if err := s.db.Where("trader_id = ?", traderID).Order("ts ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	var journal []TradeJournalDB
	if err := s.db.Where("trader_id = ?", traderID).Order("entry_time ASC").Find(&journal).Error; err != nil {
		return nil, err
	}

	type jkey struct {
		symbol, side string
	}
	byKey := map[jkey][]TradeJournalDB{}
	for _, j := range journal {
		k := jkey{j.Symbol, j.Side}
		byKey[k] = append(byKey[k], j)
	}

	buckets := []struct {
		name       string
		lo, hi     int
	}{ {"<60", 0, 59}, {"60-70", 60, 69}, {"70-80", 70, 79}, {"80+", 80, 1000} }
	stats := make([]QualityBucketStat, len(buckets))
	for i, b := range buckets {
		stats[i] = QualityBucketStat{Bucket: b.name}
	}
	consumed := map[int64]bool{}

	for _, a := range rows {
		bi := -1
		for i, b := range buckets {
			if a.EntryQuality >= b.lo && a.EntryQuality <= b.hi {
				bi = i
				break
			}
		}
		if bi < 0 {
			continue // -1 (not provided) or out of range
		}
		stats[bi].Assessments++

		if a.Direction != "long" && a.Direction != "short" {
			continue
		}
		side := "LONG"
		if a.Direction == "short" {
			side = "SHORT"
		}
		candidates := byKey[jkey{a.Symbol, side}]
		tsMs := a.Ts.UnixMilli()
		for ji, j := range candidates {
			if consumed[j.ID] || j.EntryTime < tsMs {
				continue
			}
			consumed[j.ID] = true
			_ = ji
			stats[bi].Traded++
			if j.RealizedPnL > 0 {
				stats[bi].Wins++
			}
			stats[bi].AvgPnLPct += j.PnLPct
			stats[bi].TotalPnL += j.RealizedPnL
			break
		}
	}
	for i := range stats {
		if stats[i].Traded > 0 {
			stats[i].WinRate = float64(stats[i].Wins) / float64(stats[i].Traded) * 100
			stats[i].AvgPnLPct /= float64(stats[i].Traded)
		}
	}
	return stats, nil
}

// MarshalBlockingFactors renders the tag slice for storage.
func MarshalBlockingFactors(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	b, _ := json.Marshal(tags)
	return string(b)
}
