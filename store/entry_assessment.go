package store

import (
	"encoding/json"
	"strings"
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
	ID          int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID    string    `gorm:"column:trader_id;index:idx_ea_trader_ts" json:"trader_id"`
	Cycle       int       `gorm:"column:cycle" json:"cycle"`
	Ts          time.Time `gorm:"column:ts;index:idx_ea_trader_ts" json:"ts"`
	Symbol      string    `gorm:"column:symbol;index" json:"symbol"`
	Direction   string    `gorm:"column:direction;size:8" json:"direction"` // long | short | none
	Action      string    `gorm:"column:action;size:24" json:"action"`
	Stage       string    `gorm:"column:stage;size:16" json:"stage"` // decision_stage
	WaitBias    string    `gorm:"column:wait_bias;size:8" json:"wait_bias"`
	WaitState   string    `gorm:"column:wait_state;size:16" json:"wait_state"`      // BLOCKED | WATCH_* | READY_* (empty = not declared)
	NextTrigger string    `gorm:"column:next_trigger;size:192" json:"next_trigger"` // required-event sentence for directional states
	// Gate RR ceiling (review 09-16 point 2): the rr_scan.best_rr the model
	// was SHOWN this cycle for the row's direction, plus its usable flag.
	// Comparing a later open's realized RR on the same symbol against the
	// WATCH_* rows' gate_rr quantifies the RR decay paid for "wait for the
	// micro-trend turn". 0 / false when no scan was rendered (no noise floor).
	GateRR          float64 `gorm:"column:gate_rr;default:0" json:"gate_rr"`
	GateUsable      bool    `gorm:"column:gate_usable" json:"gate_usable"`
	EntryQuality    int     `gorm:"column:entry_quality;default:-1" json:"entry_quality"`      // -1 = not provided
	BlockingFactors string  `gorm:"column:blocking_factors;type:text" json:"blocking_factors"` // JSON array
	MgmtQuality     int     `gorm:"column:mgmt_quality;default:-1" json:"mgmt_quality"`        // -1 = not provided (holds)
	MgmtFlags       string  `gorm:"column:mgmt_flags;type:text" json:"mgmt_flags"`             // JSON array
	EntryPath       string  `gorm:"column:entry_path;size:24" json:"entry_path"`               // e.g. "15m:down" / "15m:rally" / "bb_ride"
	Price           float64 `gorm:"column:price;default:0" json:"price"`
	// OrderID / OrderTracked (review 2026-10-08 H): exchange id of the entry
	// order an open action placed. OrderTracked=false marks legacy rows written
	// before the link existed (bounded heuristic join); tracked + empty id =
	// no order was placed (failed/rejected open) and yields no outcome.
	OrderID      string `gorm:"column:order_id;size:64;default:''" json:"order_id"`
	OrderTracked bool   `gorm:"column:order_tracked;default:false" json:"order_tracked"`
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
	Bucket      string `json:"bucket"`
	Assessments int    `json:"assessments"`
	Traded      int    `json:"traded"` // = MatchedExact + MatchedLegacy
	// Which caliber produced the numbers (review 2026-10-08 H): exact =
	// order-id join to the position; legacy = bounded symbol+side+time+AI
	// heuristic for rows written before order tracking.
	MatchedExact  int     `json:"matched_exact"`
	MatchedLegacy int     `json:"matched_legacy"`
	Wins          int     `json:"wins"`
	WinRate       float64 `json:"win_rate"`    // % of traded
	AvgPnLPct     float64 `json:"avg_pnl_pct"` // mean journal PnL% (margin-based) of traded
	TotalPnL      float64 `json:"total_pnl"`   // USDT
}

// legacyMatchWindow bounds the legacy (untracked) heuristic join: covers the
// default 30-min limit-order lifetime with margin (review 2026-10-08 H).
const legacyMatchWindow = 2 * time.Hour

// BucketStats joins assessments with realized outcomes. Open assessments with
// order tracking join EXACTLY: assessment.order_id → closed position(s) with
// that entry_order_id → their trade_journal row(s) (partial-fill splits sum
// net PnL into one outcome); an unfilled / still-open / failed open has no
// outcome. Legacy rows (written before tracking) keep a bounded heuristic:
// same symbol+side, journal entry within [ts, ts+legacyMatchWindow], and
// ai_managed=true (manual trades are never claimed). Each journal row is
// consumed once, and exact matches are resolved first so a legacy row cannot
// steal them (review 2026-10-08 H). Waits count per bucket without outcomes.
func (s *EntryAssessmentStore) BucketStats(traderID string) ([]QualityBucketStat, error) {
	var rows []EntryAssessment
	if err := s.db.Where("trader_id = ?", traderID).Order("ts ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	var journal []TradeJournalDB
	if err := s.db.Where("trader_id = ?", traderID).Order("entry_time ASC").Find(&journal).Error; err != nil {
		return nil, err
	}
	var positions []TraderPosition
	if err := s.db.Where("trader_id = ? AND status = ? AND entry_order_id <> ''", traderID, "CLOSED").
		Find(&positions).Error; err != nil {
		return nil, err
	}

	type jkey struct {
		symbol, side string
	}
	byKey := map[jkey][]TradeJournalDB{}
	byPos := map[int64]TradeJournalDB{}
	for _, j := range journal {
		k := jkey{j.Symbol, j.Side}
		byKey[k] = append(byKey[k], j)
		byPos[j.PositionID] = j
	}
	// entry order id → journal rows of the closed positions it filled.
	byOrder := map[string][]TradeJournalDB{}
	for _, p := range positions {
		if j, ok := byPos[p.ID]; ok {
			byOrder[p.EntryOrderID] = append(byOrder[p.EntryOrderID], j)
		}
	}

	buckets := []struct {
		name   string
		lo, hi int
	}{{"<60", 0, 59}, {"60-70", 60, 69}, {"70-80", 70, 79}, {"80+", 80, 1000}}
	stats := make([]QualityBucketStat, len(buckets))
	for i, b := range buckets {
		stats[i] = QualityBucketStat{Bucket: b.name}
	}
	consumed := map[int64]bool{}

	record := func(bi int, exact bool, js []TradeJournalDB) {
		stats[bi].Traded++
		if exact {
			stats[bi].MatchedExact++
		} else {
			stats[bi].MatchedLegacy++
		}
		// NET caliber (2026-10-03 review P1): every other aggregate nets
		// the fee — this calibration dataset (quality bucket → real win
		// rate) still classified and summed on gross, systematically
		// flattering the high-quality buckets.
		var net, pct float64
		for _, j := range js {
			net += j.RealizedPnL - j.Fee
			pct += j.PnLPct
		}
		if len(js) > 1 {
			pct /= float64(len(js)) // split fills of one order: mean margin PnL%
		}
		if net > 0 {
			stats[bi].Wins++
		}
		stats[bi].AvgPnLPct += pct
		stats[bi].TotalPnL += net
	}

	type legacyOpen struct {
		bi int
		a  EntryAssessment
	}
	var legacy []legacyOpen

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
		// P1 fix (2026-09-26 review): ONLY open actions may consume a trade
		// outcome. The direction check alone let a "wait long" assessment
		// claim the NEXT same-symbol long trade as its own outcome — with
		// wait rows outnumbering opens 10,238:315 the wait buckets were
		// describing trades the wait explicitly did NOT take. Wait rows
		// count toward Assessments but stay outcome-free.
		if !strings.HasPrefix(strings.ToLower(a.Action), "open") {
			continue
		}
		if !a.OrderTracked {
			legacy = append(legacy, legacyOpen{bi, a})
			continue
		}
		if a.OrderID == "" {
			continue // no order was placed
		}
		js := byOrder[a.OrderID]
		if len(js) == 0 {
			continue // unfilled or still open: no outcome yet
		}
		fresh := make([]TradeJournalDB, 0, len(js))
		for _, j := range js {
			if !consumed[j.ID] {
				consumed[j.ID] = true
				fresh = append(fresh, j)
			}
		}
		if len(fresh) > 0 {
			record(bi, true, fresh)
		}
	}

	// Legacy pass runs after every exact match is consumed.
	for _, l := range legacy {
		a := l.a
		side := "LONG"
		if a.Direction == "short" {
			side = "SHORT"
		}
		tsMs := a.Ts.UnixMilli()
		endMs := tsMs + legacyMatchWindow.Milliseconds()
		for _, j := range byKey[jkey{a.Symbol, side}] {
			if consumed[j.ID] || j.EntryTime < tsMs || j.EntryTime > endMs {
				continue
			}
			if j.AIManaged == nil || !*j.AIManaged {
				continue // manual or unattributed trades are never claimed
			}
			consumed[j.ID] = true
			record(l.bi, false, []TradeJournalDB{j})
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
