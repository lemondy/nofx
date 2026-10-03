package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// TradeJournalDB GORM model for trade_journal table.
// One row per closed position. Trade facts are auto-synced from trader_positions,
// decision basis (planned SL/TP, reasoning) is enriched from decision_records,
// review fields are filled by the user or AI during retrospective review.
type TradeJournalDB struct {
	ID         int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID   string `gorm:"column:trader_id;not null;index:idx_journal_trader_pos,unique;index:idx_journal_trader" json:"trader_id"`
	PositionID int64  `gorm:"column:position_id;not null;index:idx_journal_trader_pos,unique" json:"position_id"`

	// Trade facts
	Symbol      string  `gorm:"column:symbol;not null" json:"symbol"`
	Side        string  `gorm:"column:side;not null" json:"side"` // LONG|SHORT
	EntryPrice  float64 `gorm:"column:entry_price;default:0" json:"entry_price"`
	ExitPrice   float64 `gorm:"column:exit_price;default:0" json:"exit_price"`
	Quantity    float64 `gorm:"column:quantity;default:0" json:"quantity"`
	Leverage    int     `gorm:"column:leverage;default:1" json:"leverage"`
	EntryTime   int64   `gorm:"column:entry_time;default:0" json:"entry_time"` // Unix ms UTC
	ExitTime    int64   `gorm:"column:exit_time;default:0" json:"exit_time"`   // Unix ms UTC
	RealizedPnL float64 `gorm:"column:realized_pnl;default:0" json:"realized_pnl"`
	Fee         float64 `gorm:"column:fee;default:0" json:"fee"`
	PnLPct      float64 `gorm:"column:pnl_pct;default:0" json:"pnl_pct"` // PnL% relative to margin (leverage-adjusted)
	CloseReason string  `gorm:"column:close_reason;default:''" json:"close_reason"`

	// Decision basis (captured at entry from AI decision records)
	PlannedStopLoss   float64 `gorm:"column:planned_stop_loss;default:0" json:"planned_stop_loss"`
	PlannedTakeProfit float64 `gorm:"column:planned_take_profit;default:0" json:"planned_take_profit"`
	EntryReasoning    string  `gorm:"column:entry_reasoning;default:''" json:"entry_reasoning"`
	Confidence        int     `gorm:"column:confidence;default:0" json:"confidence"`

	// Review fields (filled during retrospective)
	ExecutedAsPlan  string `gorm:"column:executed_as_plan;default:''" json:"executed_as_plan"` // yes|partial|no|unknown
	DeviationNote   string `gorm:"column:deviation_note;default:''" json:"deviation_note"`
	Emotions        string `gorm:"column:emotions;default:''" json:"emotions"`                 // comma-separated: calm,fomo,fear_of_missing,revenge,overconfident
	MistakeCategory string `gorm:"column:mistake_category;default:''" json:"mistake_category"` // strategy|execution|risk_control|market|none
	StrategyTag     string `gorm:"column:strategy_tag;default:''" json:"strategy_tag"`         // breakout|mean_reversion|trend_following|news|scalp|other
	Lesson          string `gorm:"column:lesson;default:''" json:"lesson"`
	ReviewStatus    string `gorm:"column:review_status;default:'pending'" json:"review_status"` // pending|reviewed
	ReviewedAt      int64  `gorm:"column:reviewed_at;default:0" json:"reviewed_at"`

	// AIManaged: the trade was opened by the AI (user request 09-27 — the
	// journal must distinguish AI trades from manual ones). Determined by
	// matchOpenDecision (a SUCCESSFUL open/open_*_limit decision for this
	// symbol+side inside the ownership window); NULL = not yet classified
	// (legacy rows awaiting the backfill, renders as 未标注).
	AIManaged *bool `gorm:"column:ai_managed" json:"ai_managed"`

	CreatedAt int64 `gorm:"column:created_at;default:0" json:"created_at"`
	UpdatedAt int64 `gorm:"column:updated_at;default:0" json:"updated_at"`
}

func (TradeJournalDB) TableName() string { return "trade_journal" }

// TradeJournalStore trade journal storage
type TradeJournalStore struct {
	db *gorm.DB
}

// NewTradeJournalStore creates trade journal storage
func NewTradeJournalStore(db *gorm.DB) *TradeJournalStore {
	return &TradeJournalStore{db: db}
}

// initTables initializes trade journal table
func (s *TradeJournalStore) initTables() error {
	if s.db.Dialector.Name() == "postgres" {
		var tableExists int64
		s.db.Raw(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'trade_journal'`).Scan(&tableExists)
		if tableExists > 0 {
			return ensureColumns(s.db, &TradeJournalDB{})
		}
	}
	if err := s.db.AutoMigrate(&TradeJournalDB{}); err != nil {
		return fmt.Errorf("failed to migrate trade_journal table: %w", err)
	}
	return nil
}

// JournalReviewPayload user-facing review update payload (API layer)
type JournalReviewPayload struct {
	ExecutedAsPlan  *string `json:"executed_as_plan"`
	DeviationNote   *string `json:"deviation_note"`
	Emotions        *string `json:"emotions"`
	MistakeCategory *string `json:"mistake_category"`
	StrategyTag     *string `json:"strategy_tag"`
	Lesson          *string `json:"lesson"`
}

// UpdateReviewPayload updates review fields from an API payload
func (s *TradeJournalStore) UpdateReviewPayload(traderID string, id int64, payload *JournalReviewPayload) (*TradeJournalDB, error) {
	return s.UpdateReview(traderID, id, &journalReviewUpdate{
		ExecutedAsPlan:  payload.ExecutedAsPlan,
		DeviationNote:   payload.DeviationNote,
		Emotions:        payload.Emotions,
		MistakeCategory: payload.MistakeCategory,
		StrategyTag:     payload.StrategyTag,
		Lesson:          payload.Lesson,
	})
}

// journalReviewUpdate fields that can be updated via review
type journalReviewUpdate struct {
	ExecutedAsPlan  *string `json:"executed_as_plan"`
	DeviationNote   *string `json:"deviation_note"`
	Emotions        *string `json:"emotions"`
	MistakeCategory *string `json:"mistake_category"`
	StrategyTag     *string `json:"strategy_tag"`
	Lesson          *string `json:"lesson"`
}

// SyncFromPositions creates journal entries for closed positions that don't have
// one yet and enriches them with the original AI decision basis. Returns the
// number of newly created entries.
func (s *TradeJournalStore) SyncFromPositions(traderID string) (int, error) {
	var positions []TraderPosition
	if err := s.db.Where("trader_id = ? AND status = ?", traderID, "CLOSED").
		Order("exit_time DESC").Limit(500).Find(&positions).Error; err != nil {
		return 0, fmt.Errorf("failed to query closed positions: %w", err)
	}

	created := 0
	now := time.Now().UTC().UnixMilli()
	for _, pos := range positions {
		var count int64
		if err := s.db.Model(&TradeJournalDB{}).
			Where("trader_id = ? AND position_id = ?", traderID, pos.ID).
			Count(&count).Error; err != nil {
			return created, err
		}
		if count > 0 {
			continue
		}

		entry := &TradeJournalDB{
			TraderID:     traderID,
			PositionID:   pos.ID,
			Symbol:       pos.Symbol,
			Side:         pos.Side,
			EntryPrice:   pos.EntryPrice,
			ExitPrice:    pos.ExitPrice,
			Quantity:     pos.Quantity,
			Leverage:     pos.Leverage,
			EntryTime:    pos.EntryTime,
			ExitTime:     pos.ExitTime,
			RealizedPnL:  pos.RealizedPnL,
			Fee:          pos.Fee,
			CloseReason:  pos.CloseReason,
			ReviewStatus: "pending",
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		entry.PnLPct = calculateJournalPnLPct(pos)

		// Enrich with the original AI decision basis (planned SL/TP, reasoning)
		s.enrichFromDecisions(traderID, entry)

		// Ownership stamp (user request 09-27): AI trade vs manual — a
		// SUCCESSFUL open/open_*_limit decision for this symbol+side inside
		// the ownership window.
		ai := s.matchOpenDecision(traderID, pos.Symbol, pos.Side, pos.EntryTime)
		entry.AIManaged = &ai

		if err := s.db.Create(entry).Error; err != nil {
			// Unique index race (concurrent sync): skip silently
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				continue
			}
			return created, fmt.Errorf("failed to create journal entry: %w", err)
		}
		created++
	}
	return created, nil
}

// ownershipWindowMinutes: AI decides, then the order fills — the decision
// precedes the entry by up to the limit-order lifetime (30 min) plus
// placement latency, so the ownership/enrichment window starts 40 minutes
// before the position entry (30 would miss slow limit fills).
const ownershipWindowMinutes = 40

// matchOpenDecision reports whether a SUCCESSFUL AI open decision exists for
// (symbol, side) within the ownership window before entryTimeMs. Matches both
// market (open_long/open_short) and limit (open_long_limit/open_short_limit)
// actions — since the limit-entry default every AI open ships as *_limit, and
// the plain-name match alone classified nearly everything as manual (09-27
// validation: 63/218 matched before the fix, 174 after). Requires
// success=true: a REJECTED open proposal (e.g. HANDS-OFF on a manual
// position) must not attribute the trade to the AI.
func (s *TradeJournalStore) matchOpenDecision(traderID, symbol, side string, entryTimeMs int64) bool {
	if entryTimeMs <= 0 {
		return false
	}
	base := "open_long"
	if side == "SHORT" {
		base = "open_short"
	}
	windowStart := time.UnixMilli(entryTimeMs).UTC().Add(-ownershipWindowMinutes * time.Minute)
	windowEnd := time.UnixMilli(entryTimeMs).UTC().Add(2 * time.Minute)

	var records []DecisionRecordDB
	if err := s.db.Where("trader_id = ? AND timestamp BETWEEN ? AND ?",
		traderID, windowStart, windowEnd).
		Order("timestamp ASC").Limit(20).Find(&records).Error; err != nil {
		return false
	}
	for _, rec := range records {
		var actions []DecisionAction
		if json.Unmarshal([]byte(rec.Decisions), &actions) != nil {
			continue
		}
		for _, act := range actions {
			if act.Symbol == symbol && (act.Action == base || act.Action == base+"_limit") && act.Success {
				return true
			}
		}
	}
	return false
}

// BackfillOwnership classifies legacy journal rows that predate the
// ai_managed column (user request 09-27): NULL rows get the matchOpenDecision
// verdict, and rows synced before the limit-action fix get their planned
// SL/TP/reasoning re-enriched (additive only — never touches filled fields or
// review payloads). Idempotent; cheap when there is nothing to do.
func (s *TradeJournalStore) BackfillOwnership(traderID string) (int64, error) {
	var pending []TradeJournalDB
	if err := s.db.Where("trader_id = ? AND ai_managed IS NULL", traderID).
		Order("id DESC").Limit(1000).Find(&pending).Error; err != nil {
		return 0, fmt.Errorf("failed to query unclassified journal rows: %w", err)
	}
	if len(pending) == 0 {
		return 0, nil
	}
	stamped := int64(0)
	for _, e := range pending {
		ai := s.matchOpenDecision(traderID, e.Symbol, e.Side, e.EntryTime)
		updates := map[string]interface{}{"ai_managed": ai}
		// Rows synced before the *_limit fix carry empty decision basis even
		// though a matching AI decision exists — re-run the enrichment
		// (enrichFromDecisions writes only zero-valued fields).
		if e.PlannedStopLoss == 0 && e.PlannedTakeProfit == 0 && e.Confidence == 0 {
			entry := e // copy — enrich writes pointer fields
			s.enrichFromDecisions(traderID, &entry)
			if entry.PlannedStopLoss != 0 {
				updates["planned_stop_loss"] = entry.PlannedStopLoss
			}
			if entry.PlannedTakeProfit != 0 {
				updates["planned_take_profit"] = entry.PlannedTakeProfit
			}
			if entry.EntryReasoning != "" {
				updates["entry_reasoning"] = entry.EntryReasoning
			}
			if entry.Confidence != 0 {
				updates["confidence"] = entry.Confidence
			}
		}
		if err := s.db.Model(&TradeJournalDB{}).Where("id = ?", e.ID).Updates(updates).Error; err != nil {
			return stamped, fmt.Errorf("failed to stamp journal row %d: %w", e.ID, err)
		}
		stamped++
	}
	return stamped, nil
}

// calculateJournalPnLPct computes leverage-adjusted PnL percentage relative to margin
func calculateJournalPnLPct(pos TraderPosition) float64 {
	if pos.EntryPrice <= 0 || pos.Quantity <= 0 {
		return 0
	}
	notional := pos.EntryPrice * pos.Quantity
	if notional <= 0 {
		return 0
	}
	// Margin = notional / leverage; PnL% relative to margin.
	// 2026-10-03 review: NET the fee so the percentage matches the net-USDT
	// figure rendered beside it (gross/net mixed rows confused the review).
	margin := notional
	if pos.Leverage > 1 {
		margin = notional / float64(pos.Leverage)
	}
	return (pos.RealizedPnL - pos.Fee) / margin * 100
}

// enrichFromDecisions finds the AI open decision matching this position and
// fills planned SL/TP, reasoning and confidence.
func (s *TradeJournalStore) enrichFromDecisions(traderID string, entry *TradeJournalDB) {
	if entry.EntryTime <= 0 {
		return
	}
	openAction := "open_long"
	if entry.Side == "SHORT" {
		openAction = "open_short"
	}

	// Decision records within a window before position entry (AI decides,
	// then the order fills — limit fills can lag the decision by the full
	// order lifetime, hence the 40-min window shared with matchOpenDecision).
	// Both market and *_limit actions match: since limit-entry became the
	// default every AI open ships as *_limit and the plain-name match alone
	// left the whole 计划止损/止盈 column empty (09-27).
	windowStart := time.UnixMilli(entry.EntryTime).UTC().Add(-ownershipWindowMinutes * time.Minute)
	windowEnd := time.UnixMilli(entry.EntryTime).UTC().Add(2 * time.Minute)

	var records []DecisionRecordDB
	if err := s.db.Where("trader_id = ? AND timestamp BETWEEN ? AND ?",
		traderID, windowStart, windowEnd).
		Order("timestamp ASC").Limit(20).Find(&records).Error; err != nil {
		return
	}

	for _, rec := range records {
		var actions []DecisionAction
		if json.Unmarshal([]byte(rec.Decisions), &actions) != nil {
			continue
		}
		for _, act := range actions {
			if act.Symbol == entry.Symbol && (act.Action == openAction || act.Action == openAction+"_limit") {
				// Prefer a successful action record
				if entry.PlannedStopLoss == 0 && act.StopLoss != 0 {
					entry.PlannedStopLoss = act.StopLoss
				}
				if entry.PlannedTakeProfit == 0 && act.TakeProfit != 0 {
					entry.PlannedTakeProfit = act.TakeProfit
				}
				if entry.EntryReasoning == "" && act.Reasoning != "" {
					entry.EntryReasoning = act.Reasoning
				}
				if entry.Confidence == 0 && act.Confidence != 0 {
					entry.Confidence = act.Confidence
				}
				if act.Success && entry.EntryReasoning != "" {
					return // good enough match with success confirmed
				}
			}
		}
	}
}

// List returns journal entries with pagination and optional filters
func (s *TradeJournalStore) List(traderID string, limit, offset int, symbol, reviewStatus string) ([]*TradeJournalDB, int64, error) {
	query := s.db.Model(&TradeJournalDB{}).Where("trader_id = ?", traderID)
	if symbol != "" {
		query = query.Where("symbol = ?", symbol)
	}
	if reviewStatus != "" {
		query = query.Where("review_status = ?", reviewStatus)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	if limit <= 0 {
		limit = 50
	}
	var entries []*TradeJournalDB
	if err := query.Order("exit_time DESC").Limit(limit).Offset(offset).Find(&entries).Error; err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

// Get returns a single journal entry
func (s *TradeJournalStore) Get(traderID string, id int64) (*TradeJournalDB, error) {
	var entry TradeJournalDB
	if err := s.db.Where("trader_id = ? AND id = ?", traderID, id).First(&entry).Error; err != nil {
		return nil, err
	}
	return &entry, nil
}

// UpdateReview updates the review fields of a journal entry and marks it reviewed
func (s *TradeJournalStore) UpdateReview(traderID string, id int64, update *journalReviewUpdate) (*TradeJournalDB, error) {
	entry, err := s.Get(traderID, id)
	if err != nil {
		return nil, err
	}
	if update.ExecutedAsPlan != nil {
		entry.ExecutedAsPlan = *update.ExecutedAsPlan
	}
	if update.DeviationNote != nil {
		entry.DeviationNote = *update.DeviationNote
	}
	if update.Emotions != nil {
		entry.Emotions = *update.Emotions
	}
	if update.MistakeCategory != nil {
		entry.MistakeCategory = *update.MistakeCategory
	}
	if update.StrategyTag != nil {
		entry.StrategyTag = *update.StrategyTag
	}
	if update.Lesson != nil {
		entry.Lesson = *update.Lesson
	}
	entry.ReviewStatus = "reviewed"
	entry.ReviewedAt = time.Now().UTC().UnixMilli()
	entry.UpdatedAt = entry.ReviewedAt
	// 2026-10-03 review: Save wrote the WHOLE row — a concurrent sync could
	// land trade facts between the Get and the Save and get overwritten with
	// stale copies. Persist only the review-owned columns.
	if err := s.db.Model(&TradeJournalDB{}).Where("id = ? AND trader_id = ?", id, traderID).Updates(map[string]interface{}{
		"executed_as_plan": entry.ExecutedAsPlan,
		"deviation_note":   entry.DeviationNote,
		"emotions":         entry.Emotions,
		"mistake_category": entry.MistakeCategory,
		"strategy_tag":     entry.StrategyTag,
		"lesson":           entry.Lesson,
		"review_status":    entry.ReviewStatus,
		"reviewed_at":      entry.ReviewedAt,
		"updated_at":       entry.UpdatedAt,
	}).Error; err != nil {
		return nil, err
	}
	return entry, nil
}

// JournalGroupStats aggregated statistics for one group (emotion/category/tag/adherence)
type JournalGroupStats struct {
	Key      string  `json:"key"`
	Count    int     `json:"count"`
	Wins     int     `json:"wins"`
	WinRate  float64 `json:"win_rate"`
	TotalPnL float64 `json:"total_pnl"`
	AvgPnL   float64 `json:"avg_pnl"`
}

// JournalStats aggregated review statistics across all journal entries
type JournalStats struct {
	TotalEntries  int     `json:"total_entries"`
	ReviewedCount int     `json:"reviewed_count"`
	PendingCount  int     `json:"pending_count"`
	WinTrades     int     `json:"win_trades"`
	LossTrades    int     `json:"loss_trades"`
	WinRate       float64 `json:"win_rate"`
	TotalPnL      float64 `json:"total_pnl"`
	AvgWin        float64 `json:"avg_win"`
	AvgLoss       float64 `json:"avg_loss"`
	Expectancy    float64 `json:"expectancy"` // win_rate*avg_win - (1-win_rate)*avg_loss
	ProfitFactor  float64 `json:"profit_factor"`

	// Execution layer: plan adherence
	AdherencePlan []JournalGroupStats `json:"adherence_plan"` // executed_as_plan
	EmotionStats  []JournalGroupStats `json:"emotion_stats"`  // emotions
	MistakeStats  []JournalGroupStats `json:"mistake_stats"`  // mistake_category
	StrategyStats []JournalGroupStats `json:"strategy_stats"` // strategy_tag

	// Deviation from plan: how often planned SL/TP existed vs missing
	WithPlanCount   int     `json:"with_plan_count"`
	WithPlanWinRate float64 `json:"with_plan_win_rate"`
	NoPlanCount     int     `json:"no_plan_count"`
	NoPlanWinRate   float64 `json:"no_plan_win_rate"`
}

// GetStats computes aggregated review statistics from journal entries
func (s *TradeJournalStore) GetStats(traderID string) (*JournalStats, error) {
	var entries []*TradeJournalDB
	if err := s.db.Where("trader_id = ?", traderID).Order("exit_time ASC").Find(&entries).Error; err != nil {
		return nil, err
	}

	stats := &JournalStats{
		AdherencePlan: []JournalGroupStats{},
		EmotionStats:  []JournalGroupStats{},
		MistakeStats:  []JournalGroupStats{},
		StrategyStats: []JournalGroupStats{},
	}

	adherence := map[string]*JournalGroupStats{}
	emotions := map[string]*JournalGroupStats{}
	mistakes := map[string]*JournalGroupStats{}
	strategies := map[string]*JournalGroupStats{}

	var totalWin, totalLoss float64

	for _, e := range entries {
		stats.TotalEntries++
		if e.ReviewStatus == "reviewed" {
			stats.ReviewedCount++
		} else {
			stats.PendingCount++
		}
		// NET caliber (user audit 2026-10-01): RealizedPnL is the exchange
		// GROSS figure; Fee rides on the entry since sync. PF/expectancy/win
		// classification must reflect what the account keeps — the 177-long
		// book was −8.39 gross / −18.53 net with fees alone at 10.14.
		net := e.RealizedPnL - e.Fee
		stats.TotalPnL += net

		win := net > 0
		if win {
			stats.WinTrades++
			totalWin += net
		} else if net < 0 {
			stats.LossTrades++
			totalLoss += -net
		}

		// group helpers
		upsert := func(m map[string]*JournalGroupStats, key string) *JournalGroupStats {
			if key == "" {
				key = "untagged"
			}
			g, ok := m[key]
			if !ok {
				g = &JournalGroupStats{Key: key}
				m[key] = g
			}
			g.Count++
			if win {
				g.Wins++
			}
			g.TotalPnL += net
			return g
		}

		if e.ExecutedAsPlan != "" {
			upsert(adherence, e.ExecutedAsPlan)
		}
		// one entry can carry multiple emotion tags
		for _, emo := range splitCommaList(e.Emotions) {
			upsert(emotions, emo)
		}
		if e.MistakeCategory != "" {
			upsert(mistakes, e.MistakeCategory)
		}
		if e.StrategyTag != "" {
			upsert(strategies, e.StrategyTag)
		}

		// plan coverage analysis
		hasPlan := e.PlannedStopLoss > 0 || e.PlannedTakeProfit > 0
		if hasPlan {
			stats.WithPlanCount++
		} else {
			stats.NoPlanCount++
		}
	}

	finalize := func(m map[string]*JournalGroupStats) []JournalGroupStats {
		out := make([]JournalGroupStats, 0, len(m))
		for _, g := range m {
			if g.Count > 0 {
				g.WinRate = float64(g.Wins) / float64(g.Count) * 100
				g.AvgPnL = g.TotalPnL / float64(g.Count)
			}
			out = append(out, *g)
		}
		// sort by count desc for stable display
		for i := 0; i < len(out)-1; i++ {
			for j := i + 1; j < len(out); j++ {
				if out[j].Count > out[i].Count {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		return out
	}

	stats.AdherencePlan = finalize(adherence)
	stats.EmotionStats = finalize(emotions)
	stats.MistakeStats = finalize(mistakes)
	stats.StrategyStats = finalize(strategies)

	if stats.TotalEntries > 0 {
		stats.WinRate = float64(stats.WinTrades) / float64(stats.TotalEntries) * 100
	}
	if stats.WinTrades > 0 {
		stats.AvgWin = totalWin / float64(stats.WinTrades)
	}
	if stats.LossTrades > 0 {
		stats.AvgLoss = totalLoss / float64(stats.LossTrades)
	}
	if totalLoss > 0 {
		stats.ProfitFactor = totalWin / totalLoss
	}
	// Expectancy = win_rate*avg_win - (1-win_rate)*avg_loss
	wr := stats.WinRate / 100
	stats.Expectancy = wr*stats.AvgWin - (1-wr)*stats.AvgLoss

	// plan coverage win rates (NET caliber, same as above)
	withPlanWin, noPlanWin := 0, 0
	for _, e := range entries {
		hasPlan := e.PlannedStopLoss > 0 || e.PlannedTakeProfit > 0
		if hasPlan {
			if e.RealizedPnL-e.Fee > 0 {
				withPlanWin++
			}
		} else if e.RealizedPnL-e.Fee > 0 {
			noPlanWin++
		}
	}
	if stats.WithPlanCount > 0 {
		stats.WithPlanWinRate = float64(withPlanWin) / float64(stats.WithPlanCount) * 100
	}
	if stats.NoPlanCount > 0 {
		stats.NoPlanWinRate = float64(noPlanWin) / float64(stats.NoPlanCount) * 100
	}

	return stats, nil
}

// GetRecentReviewed returns recent reviewed entries with lessons (for AI context)
func (s *TradeJournalStore) GetRecentReviewed(traderID string, n int) ([]*TradeJournalDB, error) {
	var entries []*TradeJournalDB
	if err := s.db.Where("trader_id = ? AND review_status = ?", traderID, "reviewed").
		Order("exit_time DESC").Limit(n).Find(&entries).Error; err != nil {
		return nil, err
	}
	return entries, nil
}

// splitCommaList splits a comma-separated tag list
func splitCommaList(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// RMeasure is the MEASURED R-distribution over closed journal trades whose
// planned stop is known — the fix for the expectancy_r prompt line, which
// assumed every loser = −1R while the real 86-trade baseline measured
// avg loss −0.70R (systematic pessimism fed to the model every cycle,
// QUANT_REVIEW_2026-09-22 E1).
type RMeasure struct {
	AvgWinR     float64 // mean R over winners (R > 0)
	AvgLossR    float64 // mean |R| over losers (R < 0)
	ExpectancyR float64 // winrate×avgWinR − (1−winrate)×avgLossR
	Samples     int     // trades with a computable R
}

// MeasureRExpectancy computes the measured R distribution for one trader.
// R per trade = (RealizedPnL − Fee) / risk, risk = |entry − planned_stop| ×
// qty — the same net-PnL caliber as GetRollingStats. Trades without a
// planned stop (manual closes, legacy rows) are excluded, so small sample
// counts are honest, not padded. sinceMs = 0 → full history.
func (s *TradeJournalStore) MeasureRExpectancy(traderID string, sinceMs int64) (RMeasure, error) {
	var out RMeasure
	q := s.db.Where("trader_id = ? AND planned_stop_loss > 0 AND entry_price > 0 AND quantity > 0", traderID)
	if sinceMs > 0 {
		q = q.Where("exit_time >= ?", sinceMs)
	}
	var rows []TradeJournalDB
	if err := q.Find(&rows).Error; err != nil {
		return out, err
	}
	var winRSum, lossRSum float64
	wins, losses := 0, 0
	for _, r := range rows {
		risk := (r.EntryPrice - r.PlannedStopLoss) * r.Quantity
		if risk < 0 {
			risk = -risk
		}
		if risk <= 0 {
			continue
		}
		rMult := (r.RealizedPnL - r.Fee) / risk
		if rMult > 0 {
			winRSum += rMult
			wins++
		} else if rMult < 0 {
			lossRSum += -rMult
			losses++
		}
	}
	out.Samples = wins + losses
	if out.Samples == 0 {
		return out, nil
	}
	if wins > 0 {
		out.AvgWinR = winRSum / float64(wins)
	}
	if losses > 0 {
		out.AvgLossR = lossRSum / float64(losses)
	}
	wr := float64(wins) / float64(out.Samples)
	out.ExpectancyR = wr*out.AvgWinR - (1-wr)*out.AvgLossR
	return out, nil
}
