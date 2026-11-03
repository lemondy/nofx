package store

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"gorm.io/gorm"
)

// NormalizeFailureMode maps unrecognized classifications to the fixed fallback.
func NormalizeFailureMode(mode string) string {
	switch mode {
	case "chased_entry", "counter_trend", "weak_setup", "stop_too_tight", "stop_too_wide",
		"early_exit", "late_exit", "size_too_large", "market_shock", "no_mistake", "other":
		return mode
	default:
		return "other"
	}
}

// JournalTradeR uses the same planned-stop risk as MeasureRExpectancy:
// (realized PnL - fee) / (abs(entry - planned stop) * quantity).
// Missing/zero risk is unavailable, rather than an artificial zero R.
func JournalTradeR(e *TradeJournalDB) (float64, bool) {
	if e.EntryPrice <= 0 || e.PlannedStopLoss <= 0 || e.Quantity <= 0 {
		return 0, false
	}
	risk := math.Abs(e.EntryPrice-e.PlannedStopLoss) * e.Quantity
	if risk <= 0 {
		return 0, false
	}
	r := (e.RealizedPnL - e.Fee) / risk
	return r, !math.IsNaN(r) && !math.IsInf(r, 0)
}

type FailureModeStats struct {
	Key      string   `json:"key"`
	Count    int      `json:"count"`
	TotalPnL float64  `json:"total_pnl"`
	AvgR     *float64 `json:"avg_r"`
	RSamples int      `json:"r_samples"`
}

func groupFailureModes(entries []*TradeJournalDB) []FailureModeStats {
	groups := map[string]*FailureModeStats{}
	sums := map[string]float64{}
	for _, e := range entries {
		if e.FailureMode == "" {
			continue
		}
		key := NormalizeFailureMode(e.FailureMode)
		g := groups[key]
		if g == nil {
			g = &FailureModeStats{Key: key}
			groups[key] = g
		}
		g.Count++
		g.TotalPnL += e.RealizedPnL - e.Fee
		if r, ok := JournalTradeR(e); ok {
			g.RSamples++
			sums[key] += r
		}
	}
	out := make([]FailureModeStats, 0, len(groups))
	for key, g := range groups {
		if g.RSamples > 0 {
			avg := sums[key] / float64(g.RSamples)
			g.AvgR = &avg
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Key < out[j].Key
		}
		return out[i].Count > out[j].Count
	})
	return out
}

// ListAutoReviewPending caps work across all traders owned by this user
// (at most 200 rows per run, oldest first).
func (s *TradeJournalStore) ListAutoReviewPending(userID string, now time.Time) ([]*TradeJournalDB, error) {
	var rows []*TradeJournalDB
	traders := s.db.Model(&Trader{}).Select("id").Where("user_id = ?", userID)
	err := s.db.Where("trader_id IN (?) AND ai_managed = ? AND review_status = ? AND exit_time >= ? AND exit_time <= ?",
		traders, true, "pending", now.Add(-90*24*time.Hour).UnixMilli(), now.Add(-10*time.Minute).UnixMilli()).
		Order("exit_time ASC, id ASC").Limit(200).Find(&rows).Error
	return rows, err
}

// AIJournalReview contains only review-owned fields; trade facts cannot be changed.
type AIJournalReview struct {
	ID              int64  `json:"id"`
	FailureMode     string `json:"failure_mode"`
	MistakeCategory string `json:"mistake_category"`
	StrategyTag     string `json:"strategy_tag"`
	ExecutedAsPlan  string `json:"executed_as_plan"`
	Lesson          string `json:"lesson"`
	DeviationNote   string `json:"deviation_note"`
}

// UpdateAIReview is a compare-and-set: human reviews always win, even when
// saved while an LLM call is in flight. Repeated AI results are also no-ops.
func (s *TradeJournalStore) UpdateAIReview(traderID string, review AIJournalReview, now time.Time) error {
	return s.db.Model(&TradeJournalDB{}).
		Where("id = ? AND trader_id = ? AND review_status IN ?", review.ID, traderID, []string{"pending"}).
		Updates(map[string]interface{}{
			"failure_mode": NormalizeFailureMode(review.FailureMode), "mistake_category": review.MistakeCategory,
			"strategy_tag": review.StrategyTag, "executed_as_plan": review.ExecutedAsPlan,
			"lesson": review.Lesson, "deviation_note": review.DeviationNote,
			"review_status": "ai_reviewed", "reviewed_at": now.UnixMilli(), "updated_at": now.UnixMilli(),
		}).Error
}

// FailAIReviews increments failed attempts atomically and stops retrying at three.
// The pending guard protects both human reviews and completed AI reviews.
func (s *TradeJournalStore) FailAIReviews(ids []int64, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.Model(&TradeJournalDB{}).Where("id IN ? AND review_status IN ?", ids, []string{"pending"}).
		Updates(map[string]interface{}{
			"ai_review_attempts": gorm.Expr("ai_review_attempts + 1"),
			"review_status":      gorm.Expr("CASE WHEN ai_review_attempts + 1 >= 3 THEN 'ai_failed' ELSE 'pending' END"),
			"updated_at":         now.UnixMilli(),
		}).Error
}

func isJournalOpenAction(act DecisionAction, symbol, side string) bool {
	base := "open_long"
	if side == "SHORT" {
		base = "open_short"
	}
	return act.Symbol == symbol && (act.Action == base || act.Action == base+"_limit")
}

// OpeningReviewDecision shares ownership's symbol/side/action/success semantics,
// but uses the review spec's strict pre-entry 30-minute window and latest match.
// Read only reasoning context, never full prompts or unrelated account facts.
func (s *TradeJournalStore) OpeningReviewDecision(e *TradeJournalDB) (*DecisionRecordDB, *DecisionAction, error) {
	if e.EntryTime <= 0 {
		return nil, nil, nil
	}
	var records []DecisionRecordDB
	end := time.UnixMilli(e.EntryTime).UTC()
	if err := s.db.Select("id", "timestamp", "decisions", "cot_trace").
		Where("trader_id = ? AND timestamp BETWEEN ? AND ?", e.TraderID, end.Add(-30*time.Minute), end).
		Order("timestamp DESC, id DESC").Find(&records).Error; err != nil {
		return nil, nil, err
	}
	for _, rec := range records {
		var actions []DecisionAction
		if json.Unmarshal([]byte(rec.Decisions), &actions) != nil {
			continue
		}
		for _, act := range actions {
			if act.Success && isJournalOpenAction(act, e.Symbol, e.Side) {
				return &rec, &act, nil
			}
		}
	}
	return nil, nil, nil
}
