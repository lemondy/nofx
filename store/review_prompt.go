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
	ID     uint   `gorm:"primaryKey" json:"id"`
	UserID string `gorm:"column:user_id;not null;uniqueIndex" json:"user_id"`
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
		s.db.Raw(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'review_prompt_configs'`).Scan(&tableExists)
		if tableExists > 0 {
			return nil
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
func (s *ReviewPromptStore) Upsert(userID string, reviewPrompt, ruleExtractPrompt string) error {
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
	return s.db.Save(&cfg).Error
}
