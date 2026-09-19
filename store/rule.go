package store

import (
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// TradingRuleDB GORM model for trading_rules table.
// Hard rules: machine-evaluable conditions (field + operator + value) checked
// programmatically before order placement — violation blocks or warns.
// Soft rules: text lessons with tags, injected into the AI prompt as guidance.
type TradingRuleDB struct {
	ID          int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID    string `gorm:"column:trader_id;not null;index:idx_rules_trader" json:"trader_id"`
	RuleType    string `gorm:"column:rule_type;not null;default:'hard'" json:"rule_type"` // hard|soft
	Name        string `gorm:"column:name;not null" json:"name"`
	Description string `gorm:"column:description;default:''" json:"description"`

	// Hard rule condition (JSON): {"field":"leverage","op":"<=","value":10}
	ConditionJSON string `gorm:"column:condition_json;default:''" json:"condition_json"`
	// On violation: "block" (reject order) or "warn" (log + notify only)
	OnViolation string `gorm:"column:on_violation;default:'warn'" json:"on_violation"`

	// Soft rule lesson (plain text, tagged)
	LessonText string `gorm:"column:lesson_text;default:''" json:"lesson_text"`
	Tags       string `gorm:"column:tags;default:''" json:"tags"` // comma-separated, e.g. "high_leverage,chase_pumping"

	// Provenance
	Source      string `gorm:"column:source;default:'manual'" json:"source"`       // manual|ai_review
	SourceStats string `gorm:"column:source_stats;default:''" json:"source_stats"` // supporting stats, e.g. "win_rate=15%,sample=20"
	Enabled     bool   `gorm:"column:enabled;default:true" json:"enabled"`
	HitCount    int64  `gorm:"column:hit_count;default:0" json:"hit_count"` // times this rule fired
	BlockCount  int64  `gorm:"column:block_count;default:0" json:"block_count"`

	CreatedAt int64 `gorm:"column:created_at;default:0" json:"created_at"`
	UpdatedAt int64 `gorm:"column:updated_at;default:0" json:"updated_at"`
}

func (TradingRuleDB) TableName() string { return "trading_rules" }

// RuleCheckLogDB GORM model for rule_check_logs table — one row per pre-trade
// rule evaluation that fired (violation), for audit and review.
type RuleCheckLogDB struct {
	ID          int64   `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID    string  `gorm:"column:trader_id;not null;index:idx_rule_logs_trader" json:"trader_id"`
	RuleID      int64   `gorm:"column:rule_id;default:0" json:"rule_id"`
	RuleName    string  `gorm:"column:rule_name;default:''" json:"rule_name"`
	Action      string  `gorm:"column:action;not null" json:"action"` // open_long|open_short|...
	Symbol      string  `gorm:"column:symbol;not null" json:"symbol"`
	Violated    bool    `gorm:"column:violated;not null;default:false" json:"violated"`
	Blocked     bool    `gorm:"column:blocked;not null;default:false" json:"blocked"`
	Message     string  `gorm:"column:message;default:''" json:"message"`
	SymbolPrice float64 `gorm:"column:symbol_price;default:0" json:"symbol_price"`
	CreatedAt   int64   `gorm:"column:created_at;default:0" json:"created_at"`
}

func (RuleCheckLogDB) TableName() string { return "rule_check_logs" }

// RuleStore trading rules storage
type RuleStore struct {
	db *gorm.DB
}

// NewRuleStore creates trading rules storage
func NewRuleStore(db *gorm.DB) *RuleStore {
	return &RuleStore{db: db}
}

// initTables initializes rule tables
func (s *RuleStore) initTables() error {
	if s.db.Dialector.Name() == "postgres" {
		var tableExists int64
		s.db.Raw(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'trading_rules'`).Scan(&tableExists)
		if tableExists > 0 {
			return nil
		}
	}
	if err := s.db.AutoMigrate(&TradingRuleDB{}); err != nil {
		return fmt.Errorf("failed to migrate trading_rules table: %w", err)
	}
	if err := s.db.AutoMigrate(&RuleCheckLogDB{}); err != nil {
		return fmt.Errorf("failed to migrate rule_check_logs table: %w", err)
	}
	return nil
}

// ruleInput user-facing rule payload (create/update)
type RuleInput struct {
	RuleType    string `json:"rule_type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Condition   string `json:"condition"`    // JSON string for hard rules
	OnViolation string `json:"on_violation"` // block|warn
	LessonText  string `json:"lesson_text"`  // for soft rules
	Tags        string `json:"tags"`
	Source      string `json:"source"`       // manual|ai_review (defaults to manual)
	SourceStats string `json:"source_stats"` // supporting stats from AI extraction
	Enabled     *bool  `json:"enabled"`
}

// ListRules returns all rules for a trader
func (s *RuleStore) ListRules(traderID string) ([]*TradingRuleDB, error) {
	var rules []*TradingRuleDB
	if err := s.db.Where("trader_id = ?", traderID).Order("rule_type ASC, id DESC").Find(&rules).Error; err != nil {
		return nil, err
	}
	return rules, nil
}

// GetRule returns a single rule
func (s *RuleStore) GetRule(traderID string, id int64) (*TradingRuleDB, error) {
	var rule TradingRuleDB
	if err := s.db.Where("trader_id = ? AND id = ?", traderID, id).First(&rule).Error; err != nil {
		return nil, err
	}
	return &rule, nil
}

// CreateRule creates a new rule
func (s *RuleStore) CreateRule(traderID string, input *RuleInput) (*TradingRuleDB, error) {
	now := time.Now().UTC().UnixMilli()
	rule := &TradingRuleDB{
		TraderID:      traderID,
		RuleType:      input.RuleType,
		Name:          input.Name,
		Description:   input.Description,
		ConditionJSON: input.Condition,
		OnViolation:   input.OnViolation,
		LessonText:    input.LessonText,
		Tags:          input.Tags,
		Source:        "manual",
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if input.OnViolation == "" {
		rule.OnViolation = "warn"
	}
	if input.Enabled != nil {
		rule.Enabled = *input.Enabled
	}
	if input.Source != "" {
		rule.Source = input.Source
	}
	if input.SourceStats != "" {
		rule.SourceStats = input.SourceStats
	}
	if err := s.db.Create(rule).Error; err != nil {
		return nil, err
	}
	return rule, nil
}

// UpdateRule updates an existing rule
func (s *RuleStore) UpdateRule(traderID string, id int64, input *RuleInput) (*TradingRuleDB, error) {
	rule, err := s.GetRule(traderID, id)
	if err != nil {
		return nil, err
	}
	if input.RuleType != "" {
		rule.RuleType = input.RuleType
	}
	if input.Name != "" {
		rule.Name = input.Name
	}
	if input.Description != "" {
		rule.Description = input.Description
	}
	if input.Condition != "" {
		rule.ConditionJSON = input.Condition
	}
	if input.OnViolation != "" {
		rule.OnViolation = input.OnViolation
	}
	if input.LessonText != "" {
		rule.LessonText = input.LessonText
	}
	if input.Tags != "" {
		rule.Tags = input.Tags
	}
	if input.Enabled != nil {
		rule.Enabled = *input.Enabled
	}
	rule.UpdatedAt = time.Now().UTC().UnixMilli()
	if err := s.db.Save(rule).Error; err != nil {
		return nil, err
	}
	return rule, nil
}

// DeleteRule removes a rule
func (s *RuleStore) DeleteRule(traderID string, id int64) error {
	return s.db.Where("trader_id = ? AND id = ?", traderID, id).Delete(&TradingRuleDB{}).Error
}

// GetEnabledRules returns all enabled rules for a trader
func (s *RuleStore) GetEnabledRules(traderID string) ([]*TradingRuleDB, error) {
	var rules []*TradingRuleDB
	if err := s.db.Where("trader_id = ? AND enabled = ?", traderID, true).Order("id ASC").Find(&rules).Error; err != nil {
		return nil, err
	}
	return rules, nil
}

// IncrementHitCount records that a rule fired (violated)
func (s *RuleStore) IncrementHitCount(id int64, blocked bool) error {
	updates := map[string]interface{}{
		"hit_count":  gorm.Expr("hit_count + 1"),
		"updated_at": time.Now().UTC().UnixMilli(),
	}
	if blocked {
		updates["block_count"] = gorm.Expr("block_count + 1")
	}
	return s.db.Model(&TradingRuleDB{}).Where("id = ?", id).Updates(updates).Error
}

// LogCheck writes a rule check log entry
func (s *RuleStore) LogCheck(log *RuleCheckLogDB) error {
	if log.CreatedAt == 0 {
		log.CreatedAt = time.Now().UTC().UnixMilli()
	}
	return s.db.Create(log).Error
}

// ListCheckLogs returns recent rule check logs
func (s *RuleStore) ListCheckLogs(traderID string, limit int) ([]*RuleCheckLogDB, error) {
	if limit <= 0 {
		limit = 50
	}
	var logs []*RuleCheckLogDB
	if err := s.db.Where("trader_id = ?", traderID).
		Order("created_at DESC").Limit(limit).Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

// ClearCheckLogs removes all check logs for a trader
func (s *RuleStore) ClearCheckLogs(traderID string) error {
	return s.db.Where("trader_id = ?", traderID).Delete(&RuleCheckLogDB{}).Error
}

// ExportRules returns all rules as a JSON string (JSON/YAML config export)
func (s *RuleStore) ExportRules(traderID string) (string, error) {
	rules, err := s.ListRules(traderID)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
