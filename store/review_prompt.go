package store

import (
	"time"

	"gorm.io/gorm"
)

// ReviewPromptStore persists per-user AI review prompt templates. Empty
// fields mean "use the built-in default" so the feature degrades gracefully.
type ReviewPromptStore struct {
	db *gorm.DB
}

// ReviewPromptConfig is one user's custom review prompt templates.
type ReviewPromptConfig struct {
	ID                uint   `gorm:"primaryKey" json:"id"`
	UserID            string `gorm:"column:user_id;not null;uniqueIndex" json:"user_id"`
	AutoReviewEnabled bool   `gorm:"column:auto_review_enabled;not null;default:false" json:"auto_review_enabled"`
	// Unix-ms time of the last automatic review run for this user. Persisted
	// so backend restarts do not retrigger a run that already happened.
	AutoReviewLastRunAt int64 `gorm:"column:auto_review_last_run_at;not null;default:0" json:"auto_review_last_run_at"`
	// Custom system prompt for the comprehensive AI review (复盘). Empty =
	// built-in default.
	ReviewSystemPrompt string `gorm:"column:review_system_prompt;type:text;default:''" json:"review_system_prompt"`
	// Custom system prompt for AI rule extraction from the journal. Empty =
	// built-in default.
	RuleExtractSystemPrompt string    `gorm:"column:rule_extract_system_prompt;type:text;default:''" json:"rule_extract_system_prompt"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

func (ReviewPromptConfig) TableName() string { return "review_prompt_configs" }

// NewReviewPromptStore creates a new ReviewPromptStore
func NewReviewPromptStore(db *gorm.DB) *ReviewPromptStore {
	return &ReviewPromptStore{db: db}
}

func (s *ReviewPromptStore) initTables() error {
	// For PostgreSQL with existing table, skip AutoMigrate
	if s.db.Dialector.Name() == "postgres" {
		var tableExists int64
		if err := s.db.Raw(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'review_prompt_configs'`).Scan(&tableExists).Error; err != nil {
			return err
		}
		if tableExists > 0 {
			return ensureColumns(s.db, &ReviewPromptConfig{})
		}
	}
	return s.db.AutoMigrate(&ReviewPromptConfig{})
}

// Get returns the user's prompt config, or nil when no customization exists.
func (s *ReviewPromptStore) Get(userID string) (*ReviewPromptConfig, error) {
	if userID == "" {
		userID = "default"
	}
	var cfg ReviewPromptConfig
	err := s.db.Where("user_id = ?", userID).First(&cfg).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &cfg, nil
}

// Upsert saves the user's prompt templates. Empty string values are stored
// as-is and mean "use built-in default" at read time.
func (s *ReviewPromptStore) Upsert(userID string, reviewPrompt, ruleExtractPrompt string, autoReviewEnabled ...bool) error {
	if userID == "" {
		userID = "default"
	}
	var cfg ReviewPromptConfig
	err := s.db.Where("user_id = ?", userID).First(&cfg).Error
	if err != nil {
		if err != gorm.ErrRecordNotFound {
			return err
		}
		cfg = ReviewPromptConfig{UserID: userID}
	}
	cfg.ReviewSystemPrompt = reviewPrompt
	cfg.RuleExtractSystemPrompt = ruleExtractPrompt
	if len(autoReviewEnabled) > 0 {
		cfg.AutoReviewEnabled = autoReviewEnabled[0]
	}
	return s.db.Save(&cfg).Error
}

// MarkAutoReviewRun persists when the automatic review last ran for a user.
// It updates only that column so updated_at and the other fields stay intact.
func (s *ReviewPromptStore) MarkAutoReviewRun(userID string, at time.Time) error {
	if userID == "" {
		userID = "default"
	}
	return s.db.Model(&ReviewPromptConfig{}).Where("user_id = ?", userID).
		UpdateColumn("auto_review_last_run_at", at.UnixMilli()).Error
}

// ListAutoReviewUsers returns only users who explicitly enabled automatic review.
func (s *ReviewPromptStore) ListAutoReviewUsers() ([]ReviewPromptConfig, error) {
	var configs []ReviewPromptConfig
	err := s.db.Where("auto_review_enabled = ?", true).Order("user_id ASC").Find(&configs).Error
	return configs, err
}
